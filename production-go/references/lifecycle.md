# Process Lifecycle, Shutdown, and Reload

Patterns for starting, stopping, and reconfiguring production Go services.
Derived from Prometheus, Caddy, OTel Collector, NATS, Thanos, Syncthing,
Traefik, Temporal, Kubernetes, containerd, Loki, and OpenTofu.

## Contents

1. [Process Lifecycle Orchestration](#1-process-lifecycle-orchestration)
2. [Graceful Shutdown Patterns](#2-graceful-shutdown-patterns)
3. [Config Hot-Reload](#3-config-hot-reload)
4. [Component Lifecycle Interfaces](#4-component-lifecycle-interfaces)
5. [Decision Table](#5-decision-table)
6. [Anti-Patterns](#6-anti-patterns)

---

## 1. Process Lifecycle Orchestration

### run.Group pattern

Use `oklog/run.Group` to orchestrate independent long-running subsystems.
Each component is an `(execute, interrupt)` actor pair. When any actor
returns, all others are interrupted.

```go
var g run.Group

// Actor 1: signal handler.
g.Add(run.SignalHandler(context.Background(), syscall.SIGTERM, syscall.SIGINT))

// Actor 2: HTTP server.
httpSrv := &http.Server{Addr: cfg.Addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
g.Add(
    func() error {
        if err := httpSrv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
            return err
        }
        return nil
    },
    func(error) {
        ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
        defer cancel()
        if err := httpSrv.Shutdown(ctx); err != nil {
            _ = httpSrv.Close()
        }
    },
)

// Actor 3: background worker.
workerCtx, cancelWorker := context.WithCancel(context.Background())
g.Add(
    func() error { return worker.Run(workerCtx) },
    func(error) { cancelWorker() },
)

if err := g.Run(); err != nil {
    logger.Error("process exited with error", "err", err)
    os.Exit(1)
}
```

### run.Group vs errgroup

| Characteristic | `run.Group` | `errgroup` |
|---|---|---|
| Purpose | Independent long-running subsystems | Concurrent tasks sharing a lifecycle |
| Cancellation | Each actor has its own interrupt func | Shared context; first error cancels all |
| Shutdown logic | Per-actor (HTTP shutdown vs context cancel) | Uniform (context cancellation) |
| Typical scope | Process-level `main()` orchestration | Request-scoped or batch-scoped work |

Use `run.Group` when subsystems need different shutdown mechanics. Use
`errgroup` when tasks share a cancel context and uniform error handling.

### Topological ordering

Start downstream components before upstream ones. Shut down in reverse.
This prevents producers from sending to unready consumers.

```
Start order:  storage -> processors -> receivers
Stop order:   receivers -> processors -> storage
```

```go
func (p *Pipeline) Start(ctx context.Context) error {
    if err := p.exporter.Start(ctx); err != nil {
        return fmt.Errorf("start exporter: %w", err)
    }
    if err := p.processor.Start(ctx); err != nil {
        _ = p.exporter.Shutdown(ctx)
        return fmt.Errorf("start processor: %w", err)
    }
    if err := p.receiver.Start(ctx); err != nil {
        _ = p.processor.Shutdown(ctx)
        _ = p.exporter.Shutdown(ctx)
        return fmt.Errorf("start receiver: %w", err)
    }
    return nil
}

func (p *Pipeline) Shutdown(ctx context.Context) error {
    return errors.Join(
        p.receiver.Shutdown(ctx),
        p.processor.Shutdown(ctx),
        p.exporter.Shutdown(ctx),
    )
}
```

---

## 2. Graceful Shutdown Patterns

### Phased shutdown

Every shutdown follows the same structure: **stop ingress, drain in-flight
work, release resources.** The specific actions vary by domain:

| Domain | Stop ingress | Drain | Release |
|---|---|---|---|
| Network server (NATS, Caddy) | Close listener | Drain in-flight requests with timeout | Force-close remaining connections |
| Pipeline (OTel Collector) | Stop receivers | Drain queues/processors | Flush and stop exporters |
| Data system (Prometheus TSDB) | Reject new writes | Compact/flush pending data | Close storage handles |
| Stateful service (Temporal, Loki) | Mark unhealthy, deregister from ring | Wait drain period | Stop processing |

Network server example:

```go
func shutdownHTTP(srv *http.Server, timeout time.Duration) error {
    ctx, cancel := context.WithTimeout(context.Background(), timeout)
    defer cancel()
    if err := srv.Shutdown(ctx); err != nil {
        _ = srv.Close()
        return fmt.Errorf("graceful shutdown timed out: %w", err)
    }
    return nil
}
```

Stateful service example:

```go
func (s *Service) Shutdown(ctx context.Context) error {
    s.health.SetReady(false) // stop routing

    if err := s.ring.Unregister(ctx); err != nil {
        return fmt.Errorf("ring unregister: %w", err)
    }

    select { // drain period; OK: called once at shutdown, no leak risk.
    case <-time.After(s.drainTimeout):
    case <-ctx.Done():
        return ctx.Err()
    }

    return s.worker.Stop(ctx)
}
```

### Shutdown timeouts and error accumulation

Every shutdown must be bounded. Use `context.Background()` as the parent --
the triggering context may already be cancelled. Use `errors.Join` so every
component gets a chance to clean up.

```go
func (a *App) Shutdown() error {
    ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
    defer cancel()
    return errors.Join(
        a.server.Shutdown(ctx),
        a.worker.Shutdown(ctx),
        a.db.Close(),
    )
}
```

### Named lifecycle signals

For complex shutdown ordering, model phases as a DAG of once-closed channels.
Components select on upstream signals to coordinate their own shutdown.

```go
type Lifecycle struct {
    ListenerClosed chan struct{}
    DrainComplete  chan struct{}
}

func (lc *Lifecycle) Shutdown(ctx context.Context, srv *http.Server, pool *WorkerPool, db *sql.DB) error {
    var errs []error

    if err := srv.Shutdown(ctx); err != nil {
        errs = append(errs, fmt.Errorf("http shutdown: %w", err))
    }
    close(lc.ListenerClosed)

    if err := pool.Drain(ctx); err != nil {
        errs = append(errs, fmt.Errorf("worker drain: %w", err))
    }
    close(lc.DrainComplete)

    if err := db.Close(); err != nil {
        errs = append(errs, fmt.Errorf("db close: %w", err))
    }
    return errors.Join(errs...)
}

// Workers select on lifecycle signals.
func (w *Worker) Run(ctx context.Context, lc *Lifecycle) error {
    for {
        select {
        case <-ctx.Done():
            return ctx.Err()
        case <-lc.ListenerClosed:
            return w.drainQueue(ctx)
        case job := <-w.jobs:
            if err := w.process(ctx, job); err != nil {
                return err
            }
        }
    }
}
```

---

## 3. Config Hot-Reload

### Reloader chain (Prometheus)

Named reloaders run in sequence. Partial failure does not abort the chain.
Checksum-based change detection avoids unnecessary reloads.

```go
type Reloader struct {
    Name   string
    Reload func(cfg *Config) error
}

func ApplyReloaders(cfg *Config, reloaders []Reloader, logger *slog.Logger) error {
    var failed []string
    for _, r := range reloaders {
        if err := r.Reload(cfg); err != nil {
            logger.Error("reloader failed", "name", r.Name, "err", err)
            failed = append(failed, r.Name)
            continue
        }
    }
    if len(failed) > 0 {
        return fmt.Errorf("failed reloaders: %s", strings.Join(failed, ", "))
    }
    return nil
}
```

### Start-then-stop (Caddy)

New config is fully started before old is stopped. If new fails, old stays.

```go
func (s *Server) Reload(ctx context.Context, newCfg *Config) error {
    newHandler, err := buildHandler(newCfg)
    if err != nil {
        return fmt.Errorf("build new handler: %w", err) // old stays
    }
    if err := newHandler.Start(ctx); err != nil {
        return fmt.Errorf("start new handler: %w", err) // old stays
    }

    old := s.swapHandler(newHandler)
    shutdownCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
    defer cancel()
    if err := old.Shutdown(shutdownCtx); err != nil {
        slog.Warn("old handler shutdown error", "err", err)
    }
    return nil
}
```

### Atomic handler swap (Traefik)

`sync.RWMutex`-guarded swap. In-flight requests finish on the old handler.

```go
type SwappableHandler struct {
    mu      sync.RWMutex
    handler http.Handler
}

func (s *SwappableHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
    s.mu.RLock()
    h := s.handler
    s.mu.RUnlock()
    h.ServeHTTP(w, r)
}

func (s *SwappableHandler) Swap(h http.Handler) {
    s.mu.Lock()
    s.handler = h
    s.mu.Unlock()
}
```

### Verify-then-commit (Syncthing)

All subscribers verify first (any can veto), then all commit. Deep-copy
config at boundaries to prevent shared mutation.

```go
type ConfigSubscriber interface {
    VerifyConfig(cfg Config) error
    CommitConfig(cfg Config)
}

func (m *ConfigManager) Apply(newCfg Config) error {
    for _, sub := range m.subscribers {
        if err := sub.VerifyConfig(newCfg); err != nil {
            return fmt.Errorf("rejected by %T: %w", sub, err)
        }
    }
    for _, sub := range m.subscribers {
        sub.CommitConfig(deepCopyConfig(newCfg))
    }
    m.current = newCfg
    return nil
}
```

### Generational context cancellation (Traefik)

Create a context for the new generation, apply the new config, then cancel the
previous generation only after the replacement is live. If apply fails, cancel
the new generation and keep the old one running.

```go
type ConfigController struct {
    mu         sync.Mutex
    cancelPrev context.CancelFunc
}

func (c *ConfigController) Reload(parentCtx context.Context, cfg *Config) error {
    c.mu.Lock()
    defer c.mu.Unlock()

    genCtx, cancel := context.WithCancel(parentCtx)
    if err := c.apply(genCtx, cfg); err != nil {
        cancel()
        return err
    }
    if c.cancelPrev != nil {
        c.cancelPrev()
    }
    c.cancelPrev = cancel
    return nil
}
```

### Serialized reloads

Concurrent reloads race. Serialize with a mutex.

```go
func (s *Server) handleSIGHUP(ctx context.Context) {
    sigCh := make(chan os.Signal, 1)
    signal.Notify(sigCh, syscall.SIGHUP)
    for {
        select {
        case <-ctx.Done():
            return
        case <-sigCh:
            s.reloadMu.Lock()
            if err := s.reload(ctx); err != nil {
                slog.Error("reload failed", "err", err)
            }
            s.reloadMu.Unlock()
        }
    }
}
```

---

## 4. Component Lifecycle Interfaces

### Minimal interface (OTel Collector)

```go
type Component interface {
    Start(ctx context.Context, host Host) error
    Shutdown(ctx context.Context) error
}

// Nil-safe adapter for components that only need shutdown.
type ShutdownFunc func(ctx context.Context) error

func (f ShutdownFunc) Start(context.Context, Host) error { return nil }
func (f ShutdownFunc) Shutdown(ctx context.Context) error {
    if f == nil {
        return nil
    }
    return f(ctx)
}
```

### Module lifecycle (Caddy)

Cleanup runs even on partial Provision failure.

```go
type Module interface {
    Provision(ctx Context) error
    Validate() error
    Start() error
    Stop() error
    Cleanup() error // always runs, even on Provision failure
}

func startModule(ctx Context, m Module) error {
    if err := m.Provision(ctx); err != nil {
        _ = m.Cleanup()
        return fmt.Errorf("provision: %w", err)
    }
    if err := m.Validate(); err != nil {
        _ = m.Cleanup()
        return fmt.Errorf("validate: %w", err)
    }
    if err := m.Start(); err != nil {
        _ = m.Cleanup()
        return fmt.Errorf("start: %w", err)
    }
    return nil
}
```

### Automatic rollback (fx pattern)

If Start fails partway, stop already-started components in reverse.

```go
func StartAll(ctx context.Context, components []Component) (func(context.Context) error, error) {
    var started []Component
    for _, c := range components {
        if err := c.Start(ctx); err != nil {
            shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
            defer cancel()
            for i := len(started) - 1; i >= 0; i-- {
                if stopErr := started[i].Shutdown(shutdownCtx); stopErr != nil {
                    slog.Error("rollback failed", "component", fmt.Sprintf("%T", started[i]), "err", stopErr)
                }
            }
            return nil, fmt.Errorf("start %T: %w", c, err)
        }
        started = append(started, c)
    }
    shutdown := func(ctx context.Context) error {
        var errs []error
        for i := len(started) - 1; i >= 0; i-- {
            if err := started[i].Shutdown(ctx); err != nil {
                errs = append(errs, fmt.Errorf("shutdown %T: %w", started[i], err))
            }
        }
        return errors.Join(errs...)
    }
    return shutdown, nil
}
```

### Three-phase service (Loki/dskit)

Model lifecycle as starting -> running -> stopping with explicit state.

```go
type Service struct {
    state   atomic.Int32 // 0=new, 1=starting, 2=running, 3=stopping, 4=stopped
    startFn func(ctx context.Context) error
    runFn   func(ctx context.Context) error
    stopFn  func(failureReason error) error
}

func (s *Service) Run(ctx context.Context) error {
    s.state.Store(1)
    if err := s.startFn(ctx); err != nil {
        s.state.Store(4)
        return fmt.Errorf("starting: %w", err)
    }
    s.state.Store(2)
    runErr := s.runFn(ctx)
    s.state.Store(3)
    stopErr := s.stopFn(runErr) // always runs; receives run error as context
    s.state.Store(4)
    return errors.Join(runErr, stopErr)
}
```

---

## 5. Decision Table

| I need to... | Use this |
|---|---|
| Run multiple independent subsystems | `run.Group` -- any failure triggers all shutdowns |
| Run pipeline stages in order | Topological start (downstream first), reverse shutdown |
| Reload config without downtime | Start-then-stop with rollback, or atomic handler swap |
| Shut down a network server | Stop accepting, drain with timeout, force close |
| Shut down a stateful service | Mark unhealthy, deregister, drain, stop |
| Coordinate complex shutdown order | Named lifecycle signals (DAG of channels) |
| Let subscribers veto config changes | Verify-then-commit with deep copies |
| Clean up background goroutines on reload | Generational context cancellation |
| Roll back on partial start failure | Start in order, stop already-started in reverse |
| Handle shutdown errors from many components | `errors.Join` -- never short-circuit |

---

## 6. Anti-Patterns

**Shutdown without timeout** -- can hang the process forever. Always wrap
shutdown contexts with `context.WithTimeout(context.Background(), ...)`.

**Short-circuiting on first shutdown error** -- leaves other components
running. Use `errors.Join` so every component attempts cleanup.

**Using cancelled context for shutdown** -- the context that triggered
shutdown is already done. Shutdown needs a fresh `context.Background()`
with a deadline.

**Not accounting for idle consumers** -- downstream components started before
upstream may time out or log spurious errors if they have aggressive health or
idle timeouts. Ensure consumer readiness probes tolerate an initial empty
period.

**Reloading config without serialization** -- concurrent SIGHUP handlers
or API reloads can race. Serialize all reloads with a mutex.

**Tearing down before building replacement** -- if new config build fails
after old is torn down, the service has no working config. Always
start-then-stop.
