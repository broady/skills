# Observability Reference

Investment order (Peter Bourgon): metrics first, structured logging second,
distributed tracing third. Tracing has the highest operational cost and only
pays off with many services. Metrics and logs cover 90% of incident response.

## Contents

1. [slog — The Only Logging Library](#1-slog----the-only-logging-library) — setup, injection, scoped loggers, levels, LogAttrs, canonical log lines
2. [OpenTelemetry for Metrics and Tracing](#2-opentelemetry-for-metrics-and-tracing) — provider setup, middleware spans, manual spans, RED/USE metrics
3. [Runtime Diagnostics](#3-runtime-diagnostics) — pprof, runtime/metrics, expvar

---

## 1. slog -- Project Default Logging

Project default: stdlib `log/slog`. Preserve an existing consistent logger in
stable code unless there is a planned migration. One handler per environment.

### Setup in main()

```go
func newLogger(env string) *slog.Logger {
    var h slog.Handler
    switch env {
    case "production":
        h = slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})
    default:
        h = slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug, AddSource: true})
    }
    return slog.New(h)
}
```

### Dependency injection -- no global logger

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

### Scoped loggers

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

### Log levels

| Level | Use for |
|---|---|
| Debug | Development-only detail, disabled in production |
| Info | Normal operations: startup, shutdown, request completed |
| Warn | Recoverable issues: retry succeeded, fallback used, deprecated path |
| Error | Failures requiring attention: dependency unreachable, invariant violated |

### Structured logging -- always key-value pairs

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

### Value redaction

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

### Prefer LogAttrs everywhere

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

### Typed attr constructors

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
[linting.md](linting.md#sloglint).

### Never log and return

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

### Context-aware logging

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

### Log cancellation causes at boundaries

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

See [concurrency.md](concurrency.md#cancellation-causes) for how to attach
causes with `context.WithCancelCause`.

### One log line per request -- canonical log lines

The "never log and return" rule eliminates stacked log lines (one timeout → N
error logs across layers → N× noise during incidents). But lower layers often
have legitimate diagnostic data (query timing, cache hit/miss, retry count) that
shouldn't be its own log line. Two complementary patterns solve this:

**Pattern 1: Context-collected fields.** Lower layers contribute attributes to a
shared collector on context. Middleware emits one structured line at the end.

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

---

## 2. OpenTelemetry for Metrics and Tracing

> **Note:** OTel is used here for demonstration — it shows the patterns
> (provider setup, middleware spans, manual instrumentation, RED/USE metrics)
> in a vendor-neutral way. It is not a blanket recommendation. OTel adds a
> large dependency tree, non-trivial per-request overhead, and startup cost.
> Evaluate whether you need it:
>
> - **Single service, Prometheus backend** — `prometheus/client_golang`
>   directly is simpler and lighter.
> - **Single service, no metrics backend yet** — canonical log lines (§1) +
>   pprof (§3) cover most debugging needs. Add metrics when you have
>   somewhere to send them.
> - **Multiple services, need cross-service tracing** — OTel earns its weight
>   here via vendor-neutral OTLP export and contrib auto-instrumentation.
>
> If you do adopt OTel, prefer metrics-only (`sdkmetric`) until tracing is
> justified. Tracing doubles the dependency and runtime cost.

When using OTel: export to Prometheus, OTLP, or other backends via exporters
rather than coupling to a vendor client directly.

### Provider setup in main()

```go
func initTelemetry(ctx context.Context, svcName, svcVersion string) (func(context.Context) error, error) {
    res, err := resource.New(ctx, resource.WithAttributes(
        semconv.ServiceName(svcName), semconv.ServiceVersion(svcVersion),
    ))
    if err != nil {
        return nil, fmt.Errorf("create resource: %w", err)
    }

    traceExp, err := otlptracegrpc.New(ctx)
    if err != nil {
        return nil, fmt.Errorf("create trace exporter: %w", err)
    }
    tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(traceExp), sdktrace.WithResource(res))
    otel.SetTracerProvider(tp)

    metricExp, err := otlpmetricgrpc.New(ctx)
    if err != nil {
        return nil, fmt.Errorf("create metric exporter: %w", err)
    }
    mp := sdkmetric.NewMeterProvider(
        sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExp)),
        sdkmetric.WithResource(res),
    )
    otel.SetMeterProvider(mp)

    return func(ctx context.Context) error {
        return errors.Join(tp.Shutdown(ctx), mp.Shutdown(ctx))
    }, nil
}
```

### HTTP/gRPC middleware -- automatic spans

```go
// HTTP: go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp
handler := otelhttp.NewHandler(mux, "http-server")

// gRPC: go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc
srv := grpc.NewServer(grpc.StatsHandler(otelgrpc.NewServerHandler()))
```

### Manual spans for important operations

```go
func (s *OrderService) Create(ctx context.Context, req CreateOrderReq) (*Order, error) {
    ctx, span := otel.Tracer("order-service").Start(ctx, "OrderService.Create")
    defer span.End()
    span.SetAttributes(attribute.String("order_id", req.ID))

    order, err := s.store.Insert(ctx, req)
    if err != nil {
        span.RecordError(err)
        span.SetStatus(codes.Error, "insert failed")
        return nil, fmt.Errorf("insert order %s: %w", req.ID, err)
    }
    return order, nil
}
```

### Instrumenting a database call

```go
func (s *Store) GetUser(ctx context.Context, id string) (*User, error) {
    ctx, span := otel.Tracer("user-store").Start(ctx, "Store.GetUser")
    defer span.End()

    var u User
    err := s.db.QueryRowContext(ctx, "SELECT id, name, email FROM users WHERE id = $1", id).
        Scan(&u.ID, &u.Name, &u.Email)
    if err != nil {
        span.RecordError(err)
        span.SetStatus(codes.Error, "query failed")
        return nil, fmt.Errorf("get user %s: %w", id, err)
    }
    return &u, nil
}
```

### Metrics -- RED and USE

**RED for endpoints**: Rate, Errors, Duration.
**USE for resources**: Utilization, Saturation, Errors.

Create counters/histograms via `meter.Int64Counter(...)` and
`meter.Float64Histogram(...)`. Record in middleware:

```go
func metricsMiddleware(dur metric.Float64Histogram, total metric.Int64Counter, next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        start := time.Now()
        rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
        next.ServeHTTP(rec, r)

        attrs := metric.WithAttributes(
            attribute.String("http.method", r.Method),
            attribute.String("http.route", r.Pattern),
            attribute.Int("http.status_code", rec.status),
        )
        dur.Record(r.Context(), time.Since(start).Seconds(), attrs)
        total.Add(r.Context(), 1, attrs)
    })
}
```

---

## 3. Runtime Diagnostics

### pprof -- always on, separate internal port

```go
type DebugServerConfig struct {
    Addr              string
    ShutdownTimeout   time.Duration
    ReadHeaderTimeout time.Duration
    ReadTimeout       time.Duration
    WriteTimeout      time.Duration
    IdleTimeout       time.Duration
}

func addDebugServer(g *run.Group, logger *slog.Logger, cfg DebugServerConfig) {
    mux := http.NewServeMux()
    mux.HandleFunc("GET /debug/pprof/", pprof.Index)
    mux.HandleFunc("GET /debug/pprof/profile", pprof.Profile)
    mux.HandleFunc("GET /debug/pprof/symbol", pprof.Symbol)
    mux.HandleFunc("GET /debug/pprof/trace", pprof.Trace)
    mux.Handle("GET /debug/pprof/heap", pprof.Handler("heap"))
    mux.Handle("GET /debug/pprof/goroutine", pprof.Handler("goroutine"))
    mux.Handle("GET /debug/vars", expvar.Handler())

    srv := &http.Server{
        Addr:              cfg.Addr,
        Handler:           mux,
        ReadHeaderTimeout: cfg.ReadHeaderTimeout,
        ReadTimeout:       cfg.ReadTimeout,
        WriteTimeout:      cfg.WriteTimeout,
        IdleTimeout:       cfg.IdleTimeout,
    }
    g.Add(
        func() error {
            err := srv.ListenAndServe()
            if errors.Is(err, http.ErrServerClosed) {
                return nil
            }
            return fmt.Errorf("serve debug: %w", err)
        },
        func(error) {
            ctx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
            defer cancel()
            if err := srv.Shutdown(ctx); err != nil {
                logger.LogAttrs(context.Background(), slog.LevelWarn,
                    "graceful shutdown timed out, forcing close",
                    slog.Any("err", err),
                )
                _ = srv.Close()
            }
        },
    )
}
```

### pprof goroutine labels

Label long-lived goroutines with `runtime/pprof.Labels` so goroutine dumps are
actionable in production. When a goroutine dump shows 500 goroutines, labels tell
you which component owns each one without reading stack frames.

```go
import "runtime/pprof"

func (w *Worker) run(ctx context.Context) {
    ctx = pprof.WithLabels(ctx, pprof.Labels(
        "component", "webhook_sender",
        "queue", w.queueName,
    ))
    pprof.SetGoroutineLabels(ctx)
    // ... worker loop
}
```

Use labels for:
- Background workers and queue processors
- Per-subsystem goroutines in multi-subsystem servers
- Goroutines that survive longer than a single request

Labels appear in `go tool pprof` goroutine profiles and can be filtered with
`-tagfocus`. They also integrate with continuous profiling tools (Pyroscope, Parca)
for per-component flame graphs.

### runtime/metrics for GC and goroutines

```go
func collectRuntimeMetrics() {
    samples := []metrics.Sample{
        {Name: "/gc/cycles/total:gc-cycles"},
        {Name: "/sched/goroutines:goroutines"},
        {Name: "/memory/classes/heap/objects:bytes"},
    }
    metrics.Read(samples) // export via OTEL gauges or expvar
}
```

### expvar for custom debug state

```go
var activeConns = expvar.NewInt("active_connections")

activeConns.Add(1)
defer activeConns.Add(-1)
```

---

## Decision Matrix

| Question | Answer |
|---|---|
| Which logging library? | `log/slog` -- no alternatives |
| Log format in production? | `slog.NewJSONHandler` to stdout |
| Log format in development? | `slog.NewTextHandler` to stderr, with source |
| Where to log errors? | At the boundary only (handler, interceptor) |
| Hot-path logging? | `logger.LogAttrs(ctx, ...)` to avoid allocations |
| Metrics library? | Project default: OpenTelemetry SDK when multi-backend export or org policy justifies it; Prometheus client is fine for single-backend services |
| Tracing library? | OpenTelemetry SDK with OTLP exporter when tracing has a concrete operational need |
| What to instrument first? | RED metrics on every endpoint |
| When to add tracing? | Multiple services needing cross-service debug |
| pprof in production? | Yes, always -- on a separate internal port |
| Custom debug vars? | `expvar` on the debug server |
