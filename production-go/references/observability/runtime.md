# Runtime Diagnostics

## Contents

- [Runtime diagnostics -- available to operators, gated from users](#runtime-diagnostics----available-to-operators-gated-from-users)
- [pprof goroutine labels](#pprof-goroutine-labels)
- [runtime/metrics for GC and goroutines](#runtimemetrics-for-gc-and-goroutines)
- [expvar for custom debug state](#expvar-for-custom-debug-state)

## Runtime diagnostics -- available to operators, gated from users

Runtime diagnostics must be available to operators without redeploying, but
must never listen on the public service interface. Run them on a separate admin
listener. In non-local environments, require authentication, mTLS, or equivalent
admin-plane access control in addition to network isolation.

`pprof`, `trace`, `expvar`, goroutine dumps, heap profiles, and symbol endpoints
are sensitive. Goroutine dumps can reveal business logic and dependency names.
Heap profiles and `/debug/vars` can expose customer data, secrets, topology, or
high-cardinality operational state. Trace and profile collection can also create
meaningful CPU, memory, and disk pressure.

```go
type DebugServerConfig struct {
    Addr              string
    ShutdownTimeout   time.Duration
    ReadHeaderTimeout time.Duration
    ReadTimeout       time.Duration
    WriteTimeout      time.Duration
    IdleTimeout       time.Duration
}

// requireAdminAccess enforces the deployment's admin-plane access policy:
// localhost-only in local development, and authentication or mTLS plus network
// isolation in non-local environments.
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
        Handler:           requireAdminAccess(mux),
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
                _ = srv.Close() // best effort after graceful shutdown timeout
            }
        },
    )
}
```

## pprof goroutine labels

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

## runtime/metrics for GC and goroutines

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

## expvar for custom debug state

```go
var activeConns = expvar.NewInt("active_connections")

activeConns.Add(1)
defer activeConns.Add(-1)
```
