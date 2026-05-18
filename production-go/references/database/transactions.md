# Transaction Management

## Pass transactions explicitly

Transactions are function parameters, not context values. This makes the scope
visible in the type signature and avoids violating rule 12 ("context is not a
service locator").

```go
// The boundary owns the transaction lifecycle.
func (s *OrderService) PlaceOrder(ctx context.Context, req PlaceOrderReq) (*Order, error) {
    tx, err := s.db.BeginTx(ctx, nil)
    if err != nil {
        return nil, fmt.Errorf("begin tx: %v", err)
    }
    defer tx.Rollback() //nolint:errcheck // no-op after commit

    order, err := s.store.InsertOrder(ctx, tx, req)
    if err != nil {
        return nil, fmt.Errorf("insert order: %v", err)
    }
    if err := s.store.DeductInventory(ctx, tx, order.Items); err != nil {
        return nil, fmt.Errorf("deduct inventory: %v", err)
    }
    if err := tx.Commit(); err != nil {
        return nil, fmt.Errorf("commit: %v", err)
    }
    return order, nil
}

// Store methods accept a transaction (or Querier interface).
func (s *Store) InsertOrder(ctx context.Context, q Querier, req PlaceOrderReq) (*Order, error) {
    // ...
}
```

## Querier interface

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

## WithTx helper for scoping

A convenience wrapper that handles begin/commit/rollback in one place:

```go
func WithTx(ctx context.Context, db *sql.DB, fn func(tx *sql.Tx) error) error {
    tx, err := db.BeginTx(ctx, nil)
    if err != nil {
        return fmt.Errorf("begin tx: %v", err)
    }
    defer tx.Rollback() //nolint:errcheck // no-op after commit

    if err := fn(tx); err != nil {
        return err
    }
    if err := tx.Commit(); err != nil {
        return fmt.Errorf("commit tx: %v", err)
    }
    return nil
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

## Nested service calls

When a service method needs to call another method within the same transaction,
pass the `Querier` (or `*sql.Tx`) down. Don't hide transaction state in context:

```go
func (s *OrderService) PlaceOrder(ctx context.Context, req PlaceOrderReq) (*Order, error) {
    return WithTxResult(ctx, s.db, func(tx *sql.Tx) (*Order, error) {
        order, err := s.orderStore.Insert(ctx, tx, req)
        if err != nil {
            return nil, fmt.Errorf("insert order: %v", err)
        }
        // Another store, same transaction — explicit.
        if err := s.inventoryStore.Deduct(ctx, tx, order.Items); err != nil {
            return nil, fmt.Errorf("deduct inventory: %v", err)
        }
        return order, nil
    })
}
```

## Rules

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

## Connection Safety

### Connection Poisoning
Kill connections on failed transaction control:
```go
func (tx *Tx) Commit(ctx context.Context) error {
	_, err := tx.conn.Exec(ctx, "COMMIT")
	if err != nil {
		tx.conn.Close(ctx) // state is indeterminate
		return err
	}
	if tx.conn.TxStatus() != 'I' {
		tx.conn.Close(ctx) // server rolled back our COMMIT
		return ErrTxCommitRollback
	}
	return nil
}
```
A failed COMMIT or ROLLBACK means the connection state is indeterminate. Destroy it.

### Pool Release Guard
Never return a mid-transaction connection to the pool:
```go
func (c *PoolConn) Release() {
	if c.conn.PgConn().TxStatus() != 'I' {
		c.destroy() // still in a transaction — connection is unsafe
		return
	}
	c.pool.putIdle(c)
}
```

### SafeToRetry for Database Operations
Only retry if no data was sent to the server:
```go
if pgconn.SafeToRetry(err) {
	// Error occurred before any data was sent — safe to retry
	return retry(ctx, fn)
}
// Data may have been sent — do NOT retry without idempotency key
return err
```

### Connection Pool Health Dimensions
Configure all five dimensions:
```go
pool, _ := pgxpool.NewWithConfig(ctx, &pgxpool.Config{
	MaxConns:              int32(max(4, runtime.NumCPU())),
	MaxConnLifetime:       time.Hour,
	MaxConnLifetimeJitter: 5 * time.Minute, // prevent thundering herd
	MaxConnIdleTime:       30 * time.Minute,
	MinConns:              2,
})
```
`MaxConnLifetimeJitter` prevents all connections from expiring simultaneously after a deploy.
