# slog -- Project Default Logging

Project default: stdlib `log/slog`. Preserve an existing consistent logger in
stable code unless there is a planned migration. One handler per environment.

## Contents

- [Setup in main()](#setup-in-main)
- [Dependency injection -- no global logger](#dependency-injection----no-global-logger)
- [Scoped loggers](#scoped-loggers)
- [Log levels](#log-levels)
- [Structured logging -- always key-value pairs](#structured-logging----always-key-value-pairs)
- [Value redaction](#value-redaction)
- [Prefer LogAttrs everywhere](#prefer-logattrs-everywhere)
- [Typed attr constructors](#typed-attr-constructors)
- [Never log and return](#never-log-and-return)
- [Context-aware logging](#context-aware-logging)
- [Log cancellation causes at boundaries](#log-cancellation-causes-at-boundaries)
- [One log line per request -- canonical log lines](#one-log-line-per-request----canonical-log-lines)

## Setup in main()

The application does not know or care which environment it is in (see
[config.md](../config.md)). Format and level come from configuration, not an
environment name.

```go
func newLogger(format string, level slog.Level) *slog.Logger {
    opts := &slog.HandlerOptions{Level: level}
    var h slog.Handler
    switch format {
    case "text":
        opts.AddSource = true
        h = slog.NewTextHandler(os.Stderr, opts)
    default:
        h = slog.NewJSONHandler(os.Stdout, opts)
    }
    return slog.New(h)
}
```

Use `"json"` (the default) for production log aggregation, `"text"` for local
development readability. Both are config values, not environment detection.

## Dependency injection -- no global logger

Pass `*slog.Logger` through constructors. No package-level logger variables.
No `slog.Default()` outside `main()` or test bootstrap. Library code calling
`slog.Info(...)`, `slog.Error(...)`, or other package-level logging functions
uses the default logger as a hidden global. CLI tools write user-facing output
to an injected `io.Writer`; operational logs still use an injected
`*slog.Logger`.

```go
type OrderService struct {
    logger *slog.Logger
    store  OrderStore
}

func NewOrderService(logger *slog.Logger, store OrderStore) *OrderService {
    return &OrderService{
        logger: logger.With("component", "order-service"),
        store:  store,
    }
}
```

## Scoped loggers

Component-scoped attributes are bound once in the constructor with
`logger.With(...)`, not rebuilt on every call. Request-scoped attributes stay
on `context.Context` or are added at the request boundary.

```go
func (s *OrderService) Create(ctx context.Context, req CreateOrderReq) (*Order, error) {
    order, err := s.store.Insert(ctx, req)
    if err != nil {
        return nil, fmt.Errorf("insert order %s: %w", req.ID, err)
    }
    s.logger.LogAttrs(ctx, slog.LevelInfo, "order created",
        slog.String("order_id", req.ID),
        slog.String("user_id", req.UserID),
        slog.Int64("total", order.Total),
    )
    return order, nil
}
```

## Log levels

| Level | Use for |
|---|---|
| Debug | Development-only detail, disabled in production |
| Info | Normal operations: startup, shutdown, request completed |
| Warn | Recoverable issues: retry succeeded, fallback used, deprecated path |
| Error | Failures requiring attention: dependency unreachable, invariant violated |

## Structured logging -- always key-value pairs

```go
// GOOD
logger.LogAttrs(ctx, slog.LevelInfo, "order created",
    slog.String("order_id", id),
    slog.Int64("total", total),
)

// BAD -- string interpolation destroys structure
logger.LogAttrs(ctx, slog.LevelInfo, fmt.Sprintf("created order %s with total %d", id, total))
```

Use snake_case keys consistently. Never log passwords, tokens, or PII.

## Value redaction

Don't rely on per-log-site discipline to mask sensitive fields. Instead, have
domain types produce loggable versions of themselves — a single function that
zeros sensitive fields, called once per type rather than at every log call site.

```go
// LoggableRequest returns a copy of r with sensitive fields zeroed for logging.
func LoggableRequest(r *CreateUserRequest) *CreateUserRequest {
    safe := *r
    safe.Password = ""
    safe.SSN = ""
    return &safe
}

// At the boundary:
logger.LogAttrs(ctx, slog.LevelInfo, "request received",
    slog.Any("req", LoggableRequest(req)),
    slog.String("method", r.Method),
)
```

This scales: when a new sensitive field is added, update the redaction function
once — not every log site. For proto types, generate `NewLoggableXxxRequest`
methods alongside the proto output (etcd does this for Put and Txn requests).

## Prefer LogAttrs everywhere

The `...any` key-value API (`logger.InfoContext(ctx, "msg", "key", val)`) is
silently unsafe. A missing value produces `!BADKEY`, swapped key/value pairs
produce valid-but-wrong JSON, and a wrong value type produces the wrong JSON
type — all three compile and run without error. Use `logger.LogAttrs` with
typed `slog.Attr` constructors as the default form, not just in hot paths:

```go
logger.LogAttrs(ctx, slog.LevelInfo, "request handled",
    slog.String("method", r.Method),
    slog.String("path", r.URL.Path),
    slog.Int("status", status),
    slog.Duration("latency", elapsed),
)
```

This also avoids per-arg boxing allocations that `InfoContext` incurs.

## Typed attr constructors

Inline `slog.String("order_id", id)` scatters key names across every call site.
Centralize attrs in a single file so renames, type changes, and redaction are
one-line edits:

```go
// internal/log/attrs.go
package log

import "log/slog"

func OrderID(s string) slog.Attr    { return slog.String("order_id", s) }
func UserID(s string) slog.Attr     { return slog.String("user_id", s) }
func AmountCents(c int64) slog.Attr { return slog.Int64("amount_cents", c) }
func Err(e error) slog.Attr         { return slog.String("err", e.Error()) }
```

Call sites import as `applog` (to avoid collision with stdlib `log`):

```go
logger.LogAttrs(ctx, slog.LevelInfo, "order placed",
    applog.OrderID(o.ID),
    applog.UserID(o.UserID),
    applog.AmountCents(o.AmountCents),
)
```

Enforce with `sloglint` (`attr-only: true`) — see
[linting.md](../linting.md#sloglint).

## Never log and return

Pick one. At boundaries (handlers, interceptors), log. Everywhere else, wrap and return.

```go
// WRONG
if err != nil {
    s.logger.LogAttrs(ctx, slog.LevelError, "query failed", slog.Any("err", err))
    return fmt.Errorf("query users: %w", err)
}

// RIGHT
if err != nil {
    return fmt.Errorf("query users: %w", err)
}
```

## Context-aware logging

Use `LogAttrs` with the request context for code that runs inside a request.
This lets handlers and slog handlers attach trace IDs, request IDs, auth
principals, and other request-scoped values without threading them through
every call site as explicit log fields.

```go
s.logger.LogAttrs(ctx, slog.LevelInfo, "payment processed", slog.Int64("amount", amount))
```

Startup and shutdown logs that do not have a request context still use
`LogAttrs`; pass `context.Background()` explicitly so linting and review can
distinguish lifecycle logs from accidental context-free request logs.

## Log cancellation causes at boundaries

When a request fails due to context cancellation, log both `ctx.Err()` (the
category) and `context.Cause(ctx)` (the specific reason). Without the cause,
"context canceled" is unactionable in production.

```go
if ctx.Err() != nil {
    logger.LogAttrs(ctx, slog.LevelError, "request failed",
        slog.Any("err", ctx.Err()),            // "context canceled" or "context deadline exceeded"
        slog.Any("cause", context.Cause(ctx)), // "order ord-123: 5s processing timeout"
    )
}
```

See [concurrency.md](../concurrency.md#cancellation-causes) for how to attach
causes with `context.WithCancelCause`.

## One log line per request -- canonical log lines

The "never log and return" rule eliminates stacked log lines (one timeout → N
error logs across layers → N× noise during incidents). But lower layers often
have legitimate diagnostic data (query timing, cache hit/miss, retry count) that
shouldn't be its own log line. Two complementary patterns solve this:

**Pattern 1: Context-collected fields.** Lower layers contribute attributes to a
shared collector on context. Middleware emits one structured line at the end.
This is not dependency injection through context. Context-carried log fields are
allowed only for request-scoped diagnostic metadata. Do not use context to pass
services, clients, loggers, configs, or repositories.

```go
// Package reqlog provides per-request log field collection.
package reqlog

type fields struct {
    mu    sync.Mutex
    attrs []slog.Attr
}

type ctxKey struct{}

// Add attaches a log field to the request's collector.
func Add(ctx context.Context, key string, value any) {
    if f, ok := ctx.Value(ctxKey{}).(*fields); ok {
        f.mu.Lock()
        f.attrs = append(f.attrs, slog.Any(key, value))
        f.mu.Unlock()
    }
}

// Middleware creates the collector, runs the handler, emits one line.
func Middleware(logger *slog.Logger, next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        f := &fields{}
        ctx := context.WithValue(r.Context(), ctxKey{}, f)
        rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
        start := time.Now()

        next.ServeHTTP(rec, r.WithContext(ctx))

        f.mu.Lock()
        attrs := append([]slog.Attr{
            slog.String("method", r.Method),
            slog.String("path", r.URL.Path),
            slog.Int("status", rec.status),
            slog.Duration("duration", time.Since(start)),
        }, f.attrs...)
        f.mu.Unlock()

        level := slog.LevelInfo
        if rec.status >= 500 {
            level = slog.LevelError
        }
        logger.LogAttrs(ctx, level, "request", attrs...)
    })
}
```

Lower layers contribute fields without logging:

```go
func (r *UserRepo) GetByID(ctx context.Context, id string) (User, error) {
    start := time.Now()
    user, err := r.db.QueryRowContext(ctx, "SELECT ...")
    reqlog.Add(ctx, "db_duration", time.Since(start))
    if err != nil {
        return User{}, fmt.Errorf("userRepo.GetByID: %w", err)
    }
    return user, nil
}
```

Result — one structured line per request with contributions from every layer:

```json
{"level":"INFO","msg":"request","method":"GET","path":"/users/abc123","status":200,"duration":"4.1ms","db_duration":"3.5ms"}
```

**Pattern 2: Canonical log lines (Stripe-style).** Extend pattern 1 to include
every queryable dimension: auth type, user ID, feature flags hit, DB query
count, cache hit ratio, rate limit remaining. This is not a replacement for
distributed tracing — it complements tracing for ad-hoc request-level queries
in log aggregation tools.

Keep fields narrow: counts, durations, IDs, booleans. Don't embed full
request/response bodies.

**When lower layers should still log directly:**
- Debug-level logs are acceptable — they're off in production by default.
- Genuine decisions that add new information (e.g., "falling back to secondary
  node") warrant their own log line at Warn level.
