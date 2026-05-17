# Database & Async Patterns

## Contents

- [Transaction Management](#transaction-management) — explicit passing, scoping, nested calls
- [Cursor-Based Iteration](#cursor-based-iteration) — batched processing of large result sets
- [Async Work](#async-work) — broker consumers, retry with backoff, at-least-once delivery
- [Dev-Only Invariant Checks](#dev-only-invariant-checks) — runtime safety checks gated by environment

---

## Transaction Management

### Pass transactions explicitly

Transactions are function parameters, not context values. This makes the scope
visible in the type signature and avoids violating rule 12 ("context is not a
service locator").

```go
// The boundary owns the transaction lifecycle.
func (s *OrderService) PlaceOrder(ctx context.Context, req PlaceOrderReq) (*Order, error) {
    tx, err := s.db.BeginTx(ctx, nil)
    if err != nil {
        return nil, fmt.Errorf("begin tx: %w", err)
    }
    defer tx.Rollback() //nolint:errcheck // no-op after commit

    order, err := s.store.InsertOrder(ctx, tx, req)
    if err != nil {
        return nil, fmt.Errorf("insert order: %w", err)
    }
    if err := s.store.DeductInventory(ctx, tx, order.Items); err != nil {
        return nil, fmt.Errorf("deduct inventory: %w", err)
    }
    if err := tx.Commit(); err != nil {
        return nil, fmt.Errorf("commit: %w", err)
    }
    return order, nil
}

// Store methods accept a transaction (or Querier interface).
func (s *Store) InsertOrder(ctx context.Context, q Querier, req PlaceOrderReq) (*Order, error) {
    // ...
}
```

### Querier interface

Define a minimal interface that both `*sql.DB` and `*sql.Tx` satisfy. Store
methods accept this interface — callers decide whether to pass a pool or a
transaction:

```go
type Querier interface {
    ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
    QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
    QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

var (
    _ Querier = (*sql.DB)(nil)
    _ Querier = (*sql.Tx)(nil)
)
```

This is the same approach sqlc generates. Store methods don't know or care
whether they're in a transaction — that's the caller's decision.

### WithTx helper for scoping

A convenience wrapper that handles begin/commit/rollback in one place:

```go
func WithTx(ctx context.Context, db *sql.DB, fn func(tx *sql.Tx) error) error {
    tx, err := db.BeginTx(ctx, nil)
    if err != nil {
        return fmt.Errorf("begin tx: %w", err)
    }
    defer tx.Rollback() //nolint:errcheck // no-op after commit

    if err := fn(tx); err != nil {
        return err
    }
    return tx.Commit()
}

// Generic variant for returning a value.
func WithTxResult[T any](ctx context.Context, db *sql.DB, fn func(tx *sql.Tx) (T, error)) (T, error) {
    var result T
    err := WithTx(ctx, db, func(tx *sql.Tx) error {
        var innerErr error
        result, innerErr = fn(tx)
        return innerErr
    })
    return result, err
}
```

### Nested service calls

When a service method needs to call another method within the same transaction,
pass the `Querier` (or `*sql.Tx`) down. Don't hide transaction state in context:

```go
func (s *OrderService) PlaceOrder(ctx context.Context, req PlaceOrderReq) (*Order, error) {
    return WithTxResult(ctx, s.db, func(tx *sql.Tx) (*Order, error) {
        order, err := s.orderStore.Insert(ctx, tx, req)
        if err != nil {
            return nil, fmt.Errorf("insert order: %w", err)
        }
        // Another store, same transaction — explicit.
        if err := s.inventoryStore.Deduct(ctx, tx, order.Items); err != nil {
            return nil, fmt.Errorf("deduct inventory: %w", err)
        }
        return order, nil
    })
}
```

### Rules

- **Explicit over implicit** — pass `Querier`/`*sql.Tx` as a parameter, not
  through context. Transaction scope should be visible in type signatures.
- **Service layer owns transaction boundaries** — handlers don't begin
  transactions; store methods don't commit them.
- **No nested BEGIN** — if an inner function receives a `*sql.Tx`, it operates
  within the existing transaction. It does not call `BeginTx` again.
- **Don't store `*sql.Tx` in structs** — transactions are request-scoped.
- **Rollback is deferred unconditionally** — `Rollback()` is a no-op after
  `Commit()`. Defer it immediately after `BeginTx`.

---

## Cursor-Based Iteration

For processing large result sets without loading everything into memory.

**Always prefer cursor-based (keyset) pagination** — offset-based pagination is
unstable if rows are inserted or deleted during iteration (you'll skip or
double-process items). Advance by the last-seen key:

```go
// Keyed constrains types that expose a cursor key for pagination.
type Keyed interface {
    GetID() int64
}

func IterateAll[T Keyed](ctx context.Context, q Querier, batchSize int, fn func(item T) error) error {
    var cursor int64
    for {
        select {
        case <-ctx.Done():
            return ctx.Err()
        default:
        }

        batch, err := fetchBatch[T](ctx, q, cursor, batchSize)
        if err != nil {
            return fmt.Errorf("fetch batch after id=%d: %w", cursor, err)
        }
        if len(batch) == 0 {
            return nil
        }
        for _, item := range batch {
            if err := fn(item); err != nil {
                return err
            }
        }
        cursor = batch[len(batch)-1].GetID()
    }
}

func fetchBatch[T Keyed](ctx context.Context, q Querier, afterID int64, limit int) ([]T, error) {
    rows, err := q.QueryContext(ctx,
        `SELECT * FROM table WHERE id > $1 ORDER BY id ASC LIMIT $2`,
        afterID, limit,
    )
    if err != nil {
        return nil, err
    }
    defer rows.Close()
    // scan rows into []T ...
    return nil, rows.Err()
}
```

If your table lacks a stable, ordered key (rare), fall back to offset-based
pagination — but document the instability risk and ensure the callback is
idempotent.

### Properties

- **Cursor-based by default** — stable under concurrent inserts/deletes.
- Respects context cancellation between batches.
- Batch size from configuration, not hardcoded.
- Processes items one at a time (back-pressure friendly).

---

## Async Work

### External message brokers (preferred)

For durable async processing in production, use an external message broker
(SQS, Cloud Pub/Sub, NATS JetStream, Kafka). The broker handles persistence,
retry, dead-letter, and horizontal scaling. Your service is a **consumer**:

```go
type Consumer[T any] struct {
    subscriber Subscriber
    handler    func(ctx context.Context, msg T) error
    logger     *slog.Logger
}

func (c *Consumer[T]) Run(ctx context.Context) error {
    for {
        msg, err := c.subscriber.Receive(ctx)
        if err != nil {
            if ctx.Err() != nil {
                return nil // clean shutdown
            }
            return fmt.Errorf("receive: %w", err)
        }
        if err := c.process(ctx, msg); err != nil {
            c.logger.ErrorContext(ctx, "processing failed",
                "msg_id", msg.ID(),
                "err", err,
            )
            msg.Nack() // broker will redeliver after visibility timeout
            continue
        }
        msg.Ack()
    }
}
```

### Retry with backoff

For transient failures (downstream timeout, temporary unavailability), retry
with exponential backoff and jitter. Bound both attempts and total duration:

```go
func retry(ctx context.Context, maxAttempts int, base time.Duration, fn func(ctx context.Context) error) error {
    var err error
    d := base
    for attempt := range maxAttempts {
        err = fn(ctx)
        if err == nil {
            return nil
        }
        if !isRetryable(err) {
            return err
        }
        if attempt == maxAttempts-1 {
            break
        }
        // Exponential backoff with jitter.
        jitter := time.Duration(rand.Int64N(int64(d) / 4))
        timer := time.NewTimer(d + jitter)
        select {
        case <-ctx.Done():
            timer.Stop()
            return ctx.Err()
        case <-timer.C:
            d = min(d*2, 30*time.Second) // cap
        }
    }
    return fmt.Errorf("after %d attempts: %w", maxAttempts, err)
}
```

### At-least-once delivery

Most brokers provide at-least-once semantics — your handler may be called more
than once for the same message. Make handlers **idempotent**:

- Use a unique message/event ID as a deduplication key.
- Check "already processed" before doing work (idempotency table, conditional write).
- Design state transitions to be safe to repeat (inserting with ON CONFLICT DO NOTHING,
  updating with WHERE version = ?).

### In-process queues (specific use cases)

In-process persistent queues (backed by LevelDB, SQLite, or Redis) are useful
when:
- You're a single-binary deployment without access to a managed broker.
- Work items are cheap to lose on crash but should survive restarts.
- You need dynamic worker scaling within one process.

The key design: handlers return unprocessed items explicitly:

```go
type HandlerFunc[T any] func(items ...T) (unhandled []T)
```

Unhandled items are requeued with backoff. This makes partial failure visible in
the type system — you can't accidentally drop work by returning `nil, nil`.

### Decision table

| Situation | Use |
|---|---|
| N tasks, all must succeed, bounded time | `errgroup.WithContext` + `SetLimit` |
| N tasks, best-effort, collect results | `safe.Collect` |
| Durable delivery, horizontal scale, dead-letter | External broker (SQS, Pub/Sub, NATS) |
| Single-binary, must survive restart, no broker available | In-process persistent queue |
| Transient failure in synchronous call | `retry` with bounded attempts + backoff |

---

## Dev-Only Invariant Checks

For subtle correctness bugs that are hard to catch at compile time, add runtime
invariant checks that only run in development or testing mode. These should panic
with a clear message — the goal is to fail loudly during development, not silently
in production.

Gate these checks behind a build tag (`//go:build !prod`), an environment
variable (`os.Getenv("ENV") != "production"`), or a config flag set once at
startup. The mechanism doesn't matter — what matters is zero cost in production.

### Pattern

```go
// devmode.go
var devMode = sync.OnceValue(func() bool {
    return os.Getenv("APP_ENV") != "production"
})

func checkInvariant(condition bool, msg string) {
    if !devMode() {
        return
    }
    if !condition {
        panic(fmt.Sprintf("invariant violation: %s", msg))
    }
}
```

### Examples

**Detect use-after-close:**

```go
func (p *Pool) Acquire(ctx context.Context) (*Conn, error) {
    if devMode() && p.closed.Load() {
        panic("invariant violation: Acquire called after Close")
    }
    // ...
}
```

**Detect out-of-order initialization:**

```go
func (s *Server) Serve() error {
    if devMode() && s.handler == nil {
        panic("invariant violation: Serve called before RegisterHandler")
    }
    // ...
}
```

**Detect wrong-ID-type at boundary** (catch before it becomes data corruption):

```go
func (s *Store) GetMembership(ctx context.Context, q Querier, id MembershipID) (*Membership, error) {
    if devMode() && id == "" {
        panic("invariant violation: empty MembershipID passed to GetMembership")
    }
    // ...
}
```

### When to use

- Detecting misuse of shared resources (pool after close, conn after release)
- Verifying ordering constraints (Init before Use, Register before Serve)
- Catching empty/zero domain IDs at store boundaries
- Checking data invariants that are expensive to verify on every call

### Rules

- Gate behind a build tag, env var, or startup config flag — never pay the cost in production.
- Panic, don't log — these represent programmer errors, not runtime conditions.
- Include enough context in the panic message to identify the call site.
- Remove or convert to proper validation once the invariant can be enforced at
  compile time or through the type system.
