# Concurrency Reference

Intellectual foundation: Bryan Mills, "Rethinking Classical Concurrency Patterns" (GopherCon 2018).
Core thesis: most channel-based patterns are worse than the mutex or errgroup equivalent.

**Philosophy**: bounded everything, every goroutine has an owner, channels are almost always wrong.

## Contents

1. [Goroutine Lifecycle Management](#1-goroutine-lifecycle-management) — WaitGroup.Go, errgroup, run.Group, safe.Collect, goroutine gate, ctx.Done
2. [Bounded Concurrency](#2-bounded-concurrency) — SetLimit, semaphore, worker pools
3. [Channels vs Sync Primitives](#3-channels-vs-sync-primitives) — mutex, atomic, OnceValue, Locked[T], when channels are correct

**See also:** [concurrency-patterns.md](concurrency-patterns.md) — applied patterns, singleflight, closure pitfalls, anti-patterns, goleak, synctest

---

## Structured concurrency model

Treat every goroutine as a child task with an owner. A child task must not outlive
the scope that owns and waits for it. This preserves local reasoning: when a
function returns, its work is done, including any concurrent work it started.

In Go, `sync.WaitGroup.Go`, `errgroup`, `run.Group`, and `safe.Collect` are the
approved nursery-like mechanisms. They provide the structural guarantee: a parent
can start child work, cancel it, observe its errors, and wait for it.

A raw `go` statement is an escape hatch, not a default. It is allowed only when
the surrounding code documents:
- owner
- stop path
- wait path
- reason this cannot use an approved lifecycle primitive

If a helper function starts work into a caller-owned lifecycle, make that visible
at the call site by accepting the lifecycle explicitly, for example an
`*errgroup.Group`, a supervisor interface, or by exposing `Run(ctx) error`.
Hidden background work breaks the function abstraction.

---

## 1. Goroutine Lifecycle Management

Every goroutine must be traceable to a lifecycle manager that (a) waits for it
to finish and (b) can tell it to stop. No exceptions.

Prefer `sync.WaitGroup.Go`, `errgroup`, or `run.Group`. A raw `go` statement is
allowed only when the surrounding code shows the owner, stop path, wait path,
and reason. Do not use raw `go` in examples unless ownership is the point of
the example.

**Callers own concurrency.** Library functions return results; they do not
spawn internal goroutines. The caller decides whether work is concurrent and
provides the lifecycle (context, errgroup). If a library must run background
work, it exposes a `Run(ctx) error` method so the caller controls start/stop.

### errgroup.WithContext — the default tool

Use `golang.org/x/sync/errgroup` for any set of concurrent operations
that share a lifecycle. The first error cancels the group's context.

```go
func (s *Server) processAll(ctx context.Context, items []Item) error {
	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(s.MaxConcurrent)
	for _, item := range items {
		g.Go(func() error {
			return s.process(ctx, item)
		})
	}
	return g.Wait()
}
```

### oklog/run.Group — multi-subsystem servers

Use `oklog/run` when orchestrating independent long-running subsystems.
Each actor gets an `execute` and an `interrupt` func. When any returns,
all others are interrupted.

```go
var g run.Group

httpSrv := &http.Server{
    Addr:              cfg.HTTPAddr,
    Handler:           mux,
    ReadHeaderTimeout: cfg.HTTPReadHeaderTimeout,
    ReadTimeout:       cfg.HTTPReadTimeout,
    WriteTimeout:      cfg.HTTPWriteTimeout,
    IdleTimeout:       cfg.HTTPIdleTimeout,
}
g.Add(
    func() error {
        err := httpSrv.ListenAndServe()
        if errors.Is(err, http.ErrServerClosed) {
            return nil
        }
        return err
    },
    func(error) {
        ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
        defer cancel()
        if err := httpSrv.Shutdown(ctx); err != nil {
            logger.LogAttrs(context.Background(), slog.LevelWarn,
                "graceful shutdown timed out, forcing close",
                slog.Any("err", err),
            )
            _ = httpSrv.Close() // best effort after graceful shutdown timeout
        }
    },
)

workerCtx, cancelWorker := context.WithCancel(context.Background())
g.Add(
    func() error { return worker.Run(workerCtx) },
    func(error) { cancelWorker() },
)

g.Add(run.SignalHandler(context.Background(), syscall.SIGTERM, syscall.SIGINT))

if err := g.Run(); err != nil {
    return fmt.Errorf("run server: %w", err)
}
return nil
```

### Always select on ctx.Done()

Any goroutine that loops or blocks must check for cancellation.

```go
// WRONG — ignores cancellation
func (w *Worker) Run() {
    for msg := range w.queue { w.handle(msg) }
}

// RIGHT — respects context
func (w *Worker) Run(ctx context.Context) error {
    for {
        select {
        case <-ctx.Done():
            return ctx.Err()
        case msg, ok := <-w.queue:
            if !ok {
                return nil
            }
            if err := w.handle(ctx, msg); err != nil {
                return fmt.Errorf("handle message: %w", err)
            }
        }
    }
}
```

### WaitGroup.Go — stdlib fire-and-wait (Go 1.25+)

For simple "start N goroutines and wait" patterns where you don't need error
returns or cancellation-on-first-error, use `sync.WaitGroup.Go` directly:

```go
var wg sync.WaitGroup
for _, shard := range shards {
    wg.Go(func() {
        shard.Flush(ctx)
    })
}
wg.Wait()
```

`WaitGroup.Go` does not recover panics — a panic in any goroutine crashes
the process (same as our policy). It also does not return errors; if you need
error propagation or cancel-on-first-error, use `errgroup`.

### errgroup with named errors

Wrap errors with context at the call site. No helper needed:

```go
g, ctx := errgroup.WithContext(ctx)
g.Go(func() error {
    if err := flusher.Run(ctx); err != nil {
        return fmt.Errorf("flush metrics: %w", err)
    }
    return nil
})
if err := g.Wait(); err != nil {
    return fmt.Errorf("run workers: %w", err)
}
```

Panics in errgroup goroutines crash the process — errgroup does not recover.

Always pair `errgroup` with `SetLimit` — unbounded concurrency requires a
justifying comment explaining why the number of goroutines is naturally bounded
(e.g., fixed number of shards).

### Best-effort fan-out/collect with safe.Collect

When individual failures are tolerable (prefetch, cache warming, batch lookups),
use `safe.Collect`. It dispatches work to bounded goroutines and returns
per-item results with per-item errors.

Panics in item functions crash the process — they are not recovered. Validate
untrusted inputs before passing them to Collect.

`safe.Collect` panics when `limit <= 0`; that is programmer misuse, not a
production runtime failure path. Validate config-derived limits before calling
it.

```go
type fetchResult struct {
    URL string
    Img image.Image
}

results := safe.Collect(ctx, 10, urls, func(ctx context.Context, url string) (fetchResult, error) {
    img, err := fetchImage(ctx, url)
    return fetchResult{URL: url, Img: img}, err
})

images := make(map[string]image.Image, len(results))
for _, r := range results {
    if r.Err != nil {
        logger.LogAttrs(ctx, slog.LevelWarn, "prefetch failed",
            slog.String("url", r.Val.URL),
            slog.Any("err", r.Err),
        )
        continue
    }
    images[r.Val.URL] = r.Val.Img
}
```

**When to use `safe.Collect` vs `errgroup`:**

| Scenario | Use |
|----------|-----|
| All items must succeed or the operation fails | `errgroup.WithContext` |
| Partial results are useful; failures are logged but non-fatal | `safe.Collect` |
| Order of results matters (must match input order) | `safe.Collect` (preserves order) |
| Need to cancel remaining work on first error | `errgroup.WithContext` |

`safe.Collect` does **not** recover panics. A panic in any item function
crashes the process — this is intentional. Panics indicate programmer errors
where state may be corrupted; recovering and returning partial results would
mask the corruption. Validate untrusted inputs before passing them to Collect.

### ExecQueue — serialized async execution

When state changes must be processed in order but not block the caller, use
a serial execution queue. `Add(f)` appends to a slice; if no goroutine is
running, one is started to drain the queue. At most one goroutine runs at a
time. Use for observer notifications, state machine transitions, ordered
event delivery. Not suitable when cancellation or error propagation is needed
— use errgroup or a context-aware worker channel instead.

### Goroutine gate — long-lived independent goroutines

When goroutines are long-lived and independently managed (goroutine-per-connection
servers), errgroup's "first error cancels all" semantic is wrong — one connection
failing should not tear down all others. Use a gate: a mutex-guarded boolean that
refuses new goroutines during shutdown, paired with a WaitGroup for the join point.

A raw `go` is justified here because the gate itself provides the lifecycle:
owner (Server), stop path (context cancellation), wait path (WaitGroup), and
admission control (boolean gate).

Normal errors (connection closed, client timeout) are logged and the server
continues — one bad connection is expected. Panics are **not recovered**. A
panic is a programmer error; state may be corrupted. The process crashes and
the orchestrator restarts it. This follows Google's Go style guidance:
recovering panics to avoid crashes is a historical mistake that masks corruption.

```go
type Server struct {
    mu      sync.Mutex
    running bool
    wg      sync.WaitGroup
    ctx     context.Context
    cancel  context.CancelFunc
    logger  *slog.Logger
}

func (s *Server) startGoRoutine(name string, f func(context.Context) error) bool {
    s.mu.Lock()
    defer s.mu.Unlock()
    if !s.running {
        return false
    }
    // wg.Go calls Add(1) synchronously, then launches the goroutine.
    // The Add happens under our mutex, preserving the gate invariant.
    s.wg.Go(func() {
        if err := f(s.ctx); err != nil && !errors.Is(err, context.Canceled) {
            s.logger.LogAttrs(s.ctx, slog.LevelError, "goroutine failed",
                slog.String("name", name),
                slog.Any("err", err),
            )
        }
    })
    return true
}

// Shutdown closes the gate, signals all goroutines, and waits for drain.
func (s *Server) Shutdown() {
    s.mu.Lock()
    s.running = false
    s.mu.Unlock()

    s.cancel()  // signal all goroutines to stop
    s.wg.Wait() // wait for all to drain
}
```

The gate and shutdown must use the same mutex. Without it, a goroutine can
start between setting `running = false` and calling `cancel()`, then miss
the cancellation signal and leak.

---

## 2. Bounded Concurrency

Unbounded goroutine spawning is a resource leak. At 100 req/s, a
fire-and-forget `go` in a handler spawns 6,000 goroutines per minute.

### errgroup.SetLimit — fan-out with a ceiling

```go
g, ctx := errgroup.WithContext(ctx)
g.SetLimit(10) // at most 10 in-flight
for _, url := range urls {
    g.Go(func() error { return fetch(ctx, url) })
}
return g.Wait()
```

### semaphore.Weighted — resource-limited access

Use when limiting access to a shared resource across multiple call sites
(DB connections, file descriptors).

```go
type ImageProcessor struct{ sem *semaphore.Weighted }

func NewImageProcessor(max int64) *ImageProcessor {
    return &ImageProcessor{sem: semaphore.NewWeighted(max)}
}

func (p *ImageProcessor) Resize(ctx context.Context, img Image) (Image, error) {
    // Blocking Acquire: waits for capacity (suitable for worker queuing).
    // For load-shedding / bulkhead scenarios where you want to reject
    // immediately when full, use sem.TryAcquire(1) instead — see
    // references/resilience.md.
    if err := p.sem.Acquire(ctx, 1); err != nil {
        return Image{}, fmt.Errorf("acquire semaphore: %w", err)
    }
    defer p.sem.Release(1)
    return doResize(ctx, img)
}
```

### Worker pool with errgroup

```go
func processStream(ctx context.Context, items <-chan Item, numWorkers int) error {
    g, ctx := errgroup.WithContext(ctx)
    // Naturally bounded: exactly numWorkers supervised goroutines.
    for range numWorkers {
        g.Go(func() error {
            for {
                select {
                case <-ctx.Done():
                    return ctx.Err()
                case item, ok := <-items:
                    if !ok {
                        return nil
                    }
                    if err := process(ctx, item); err != nil {
                        return fmt.Errorf("process item %s: %w", item.ID, err)
                    }
                }
            }
        })
    }
    return g.Wait()
}
```

---

## 3. Channels vs Sync Primitives

**Default to sync primitives.** Channels are coordination tools, not data
structures. Most "shared state" problems are solved better with a mutex.

### sync.Mutex — the default for shared state

```go
type Cache struct {
    mu    sync.Mutex
    items map[string]*Entry
}

func (c *Cache) Get(key string) (*Entry, bool) {
    c.mu.Lock()
    defer c.mu.Unlock()
    e, ok := c.items[key]
    return e, ok
}
```

**sync.RWMutex**: same pattern with `RLock`/`RUnlock` for reads. Use only
when profiling confirms read contention — do not prematurely optimize.

**atomic**: use stdlib `sync/atomic` typed wrappers (`atomic.Bool`,
`atomic.Int64`, etc.) for simple counters and flags. The underlying value is
unexported, so non-atomic access is impossible at compile time.

```go
type HealthCheck struct{ ready atomic.Bool }
func (h *HealthCheck) SetReady()     { h.ready.Store(true) }
func (h *HealthCheck) IsReady() bool { return h.ready.Load() }
```

### sync.OnceValue — lazy initialization

```go
var defaultTemplates = sync.OnceValue(func() *template.Template {
    return template.Must(template.ParseFS(embeddedTemplates, "templates/*.tmpl"))
})
```

Use for expensive, deterministic one-time initialization with no I/O, no request
context, and no cleanup requirement. Prefer constructor injection for resources
that need lifecycle management (database pools, clients, workers).

**Panic replay:** If the init function panics, `OnceValue` records the panic and
replays it on every subsequent call. The panic is never swallowed — consistent
with our "panics crash the process" policy. This means a `Must*` inside a
`OnceValue` is safe: if the template fails to parse, every caller panics with
the same value.

### Compound mutations — the Get/Set gap

A mutex wrapper with separate `Get()` and `Set()` methods creates a logical race
that **`go test -race` will not detect**. Each call is individually synchronized,
but the compound read-modify-write is not atomic:

```go
// BUG: another goroutine can modify between Get and Set.
// Race detector is silent — no data race, but updates are silently lost.
v := counter.Get()
v++
counter.Set(v)
```

The same applies to struct state:

```go
s := state.Get()
s.Count++
state.Set(s) // lost if another goroutine mutated between Get and Set
```

**Fix: hold the lock for the entire mutation via a closure.**

Use `safe.Locked[T]` (see [packages/safe/locked.go](../packages/safe/locked.go)):
- `Do(func(*T))` — write lock held for the entire closure (atomic read-modify-write)
- `Get() T` — read lock, returns a shallow copy
- `Store(v T)` — write lock, wholesale replacement

`Locked[T]` is safe for scalar, deep-value, or immutable `T`. It does not
deep-copy values. For maps, slices, pointers, or structs containing them,
callers must provide their own copy discipline before storing or after loading.

Usage:

```go
var counter Locked[int]

// CORRECT: lock held for the entire increment
counter.Do(func(v *int) { *v++ })

// CORRECT: struct mutation is atomic
var state Locked[AppState]
state.Do(func(s *AppState) {
    if s.Count < s.Max {
        s.Count++
        s.Name = fmt.Sprintf("item-%d", s.Count)
    }
})
```

**When plain `Store` is fine:** replacing a value wholesale without reading it
first (config swap, boolean flag toggled from one place). As soon as the new
value depends on the current value, use `Do`.

This pattern has stdlib precedent: `database/sql` uses an internal
`withLock(lk sync.Locker, fn func())` helper (~18 call sites). Tailscale ships
a public `syncs.MutexValue[T]` with the same API shape.

**Decision guide:**

| Situation | Use |
|---|---|
| Simple counter/flag, single writer | `sync/atomic` typed wrappers |
| Read current value (no write-back) | `Get()` for scalar/deep-value/immutable values |
| Replace value (independent of old) | `Store(v)` |
| Read-modify-write, conditional update | `Do(func(*T))` |
| Multiple fields with invariants | `Do(func(*T))` |

### WaitGroup with done channel

`sync.WaitGroup` cannot be used in a `select`. When shutdown needs to wait
on a WaitGroup alongside `ctx.Done()` or a timeout, wrap it with a helper
that calls `wg.Wait()` in a `sync.Once`-guarded internal goroutine and
closes a `chan struct{}` on completion. The internal goroutine's lifetime
is bounded by the WaitGroup itself reaching zero.

### When channels ARE correct

| Use case | Pattern |
|---|---|
| Signaling (done, shutdown) | `chan struct{}` — unbuffered |
| One-shot callback/channel adapter | Prefer synchronous code; if async is unavoidable, use `chan T` buffered 1 only with documented owner, stop path, wait path, and context-aware producer |
| Pipeline stage | Bounded channel, requires justifying comment |

```go
// Signaling
done := make(chan struct{})
g, ctx := errgroup.WithContext(ctx)
// Naturally bounded: exactly one supervised goroutine.
g.Go(func() error {
	defer close(done)
	return runTask(ctx)
})
select {
case <-done:
	// task completed
case <-ctx.Done():
	return ctx.Err()
}
if err := g.Wait(); err != nil {
	return fmt.Errorf("run task: %w", err)
}

// One-shot adapter — prefer synchronous code. Use this only when adapting an
// API that is inherently async/callback/channel based. compute must honor ctx;
// otherwise cancellation cannot stop the goroutine predictably.
type computeResult struct {
	Value Result
	Err   error
}
ch := make(chan computeResult, 1) // one-shot handoff; lifecycle is owned by errgroup
g, ctx := errgroup.WithContext(ctx)
// Naturally bounded: exactly one supervised goroutine.
g.Go(func() error {
	result, err := compute(ctx)
	ch <- computeResult{Value: result, Err: err}
	return nil
})

var result computeResult
select {
case result = <-ch:
case <-ctx.Done():
	if err := g.Wait(); err != nil {
		return Result{}, fmt.Errorf("wait compute: %w", err)
	}
	return Result{}, ctx.Err()
}
if err := g.Wait(); err != nil {
	return Result{}, fmt.Errorf("wait compute: %w", err)
}
if result.Err != nil {
	return Result{}, fmt.Errorf("compute: %w", result.Err)
}
```

### Channel size rule

**0 or 1 by default. Every buffered channel with capacity > 1 needs a comment
explaining the backpressure contract.**

```go
make(chan Event)          // fine: synchronous handoff
make(chan Result, 1)      // one-shot handoff only with owner/stop/wait documented
make(chan LogEntry, 4096) // bounded log buffer: drops oldest at capacity; producers never block request path
```

`chan Result` with capacity `len(items)` is allowed only for finite fan-in when
`items` is already explicitly bounded, each producer sends at most once, there
is a separate concurrency limit, and the memory cost is acceptable. It always
needs a short justification comment:

```go
results := make(chan Result, len(items)) // bounded fan-in: len(items) <= maxBatchSize; one send per item; not a work queue
```

If you need a buffered channel > 1, you probably want `errgroup.SetLimit`,
`safe.Collect`, a preallocated result slice, a mutex-protected collection, a
semaphore, or a ring buffer instead.

The comment must answer: why this capacity, what happens when full, and how
producers are blocked, shed, or throttled. A bare number like `1000` is not a
bound; it is an unreviewed outage threshold.
