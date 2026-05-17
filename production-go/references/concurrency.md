# Concurrency Reference

Intellectual foundation: Bryan Mills, "Rethinking Classical Concurrency Patterns" (GopherCon 2018).
Core thesis: most channel-based patterns are worse than the mutex or errgroup equivalent.

**Philosophy**: bounded everything, every goroutine has an owner, channels are almost always wrong.

## Contents

1. [Goroutine Lifecycle Management](#1-goroutine-lifecycle-management) — errgroup, run.Group, safe.Go, safe.Collect, ctx.Done
2. [Bounded Concurrency](#2-bounded-concurrency) — SetLimit, semaphore, worker pools
3. [Channels vs Sync Primitives](#3-channels-vs-sync-primitives) — mutex, atomic, Locked[T], when channels are correct
4. [Common Patterns Done Right](#4-common-patterns-done-right) — fan-out/fan-in, background workers, rate limiting, timeouts, cancellation causes
5. [Closure Capture Pitfalls](#5-closure-capture-pitfalls) — pointer capture, method values, concurrent handlers, named returns, slice headers
6. [Anti-Patterns to Never Generate](#6-anti-patterns-to-never-generate) — fire-and-forget, naked go, busy spin
7. [Leak Detection with goleak](#7-leak-detection-with-goleak) — TestMain, per-test, synctest, production detection

---

## 1. Goroutine Lifecycle Management

Every goroutine must be traceable to a lifecycle manager that (a) waits for it
to finish and (b) can tell it to stop. No exceptions.

Prefer `errgroup`, `run.Group`, or `safe.Go`. A raw `go` statement is allowed
only when the surrounding code shows the owner, stop path, wait path, and reason.
Do not use raw `go` in examples unless ownership is the point of the example.

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

httpSrv := &http.Server{Addr: ":8080", Handler: mux}
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
            logger.Error("shutdown http server", "err", err)
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

### Panic supervision with safe.Go

Production goroutines must return errors for ordinary failures. A panic means
a programmer bug or true invariant violation. A goroutine supervisor is one of
two approved `recover` sites (the other is package-internal control flow — see
[errors.md](errors.md#approved-recover-sites)). The supervisor converts the
panic into an owner-visible error and lets the owning group cancel the rest of
the work. It must not swallow the panic and continue silently.

Copy or import the implementation from
[../packages/safe](../packages/safe). It has no third-party dependencies; its
small `Group` interface is satisfied by `*errgroup.Group`.

```go
g, ctx := errgroup.WithContext(ctx)
g.SetLimit(10)
safe.Go(g, "flush metrics", func() error {
	return flusher.Run(ctx)
})
if err := g.Wait(); err != nil {
	return fmt.Errorf("run workers: %w", err)
}
```

Always pair `errgroup` with `SetLimit` — unbounded concurrency requires a
justifying comment explaining why the number of goroutines is naturally bounded
(e.g., fixed number of shards).

Do not use this wrapper to make panics acceptable. Use it to ensure panics are
observable, cancel sibling work, and fail the owner.

### Best-effort fan-out/collect with safe.Collect

When individual failures are tolerable (prefetch, cache warming, batch lookups),
use `safe.Collect`. It dispatches work to bounded goroutines and returns
per-item results. Panics are recovered and reported as `*PanicError` — the
caller sees them, not the process.

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
        logger.WarnContext(ctx, "prefetch failed", "url", r.Val.URL, "err", r.Err)
        continue
    }
    images[r.Val.URL] = r.Val.Img
}
```

**When to use `safe.Collect` vs `errgroup`:**

| Scenario | Use |
|----------|-----|
| All items must succeed or the operation fails | `errgroup` + `safe.Go` |
| Partial results are useful; failures are logged but non-fatal | `safe.Collect` |
| Order of results matters (must match input order) | `safe.Collect` (preserves order) |
| Need to cancel remaining work on first error | `errgroup.WithContext` |

`safe.Collect` is an approved goroutine supervisor: same as `safe.Go`, it is
the one place where `recover` is justified. Application code never calls
`recover` — the supervisor converts panics to per-item errors visible to the
caller.

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

**atomic**: follow Uber style and use `go.uber.org/atomic` for simple counters
and flags. It wraps atomic operations in typed APIs so reads and writes are
harder to misuse.

```go
type HealthCheck struct{ ready atomic.Bool }
func (h *HealthCheck) SetReady()     { h.ready.Store(true) }
func (h *HealthCheck) IsReady() bool { return h.ready.Load() }
```

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
- `Get() T` — read lock, returns a snapshot
- `Store(v T)` — write lock, wholesale replacement

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
| Simple counter/flag, single writer | `go.uber.org/atomic` |
| Read current value (no write-back) | `Get()` / snapshot |
| Replace value (independent of old) | `Store(v)` |
| Read-modify-write, conditional update | `Do(func(*T))` |
| Multiple fields with invariants | `Do(func(*T))` |

### When channels ARE correct

| Use case | Pattern |
|---|---|
| Signaling (done, shutdown) | `chan struct{}` — unbuffered |
| One-shot async result | `chan T` — buffered 1 |
| Pipeline stage | Bounded channel, requires justifying comment |

```go
// Signaling
done := make(chan struct{})
g, ctx := errgroup.WithContext(ctx)
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

// One-shot result — buffered 1 so sender never blocks if receiver is gone
type computeResult struct {
	Value Result
	Err   error
}
ch := make(chan computeResult, 1)

// Raw go is acceptable here because ownership is explicit:
// the goroutine has one non-blocking send, the receiver waits below, and ctx
// bounds how long the caller waits.
go func() {
	result, err := compute(ctx)
	ch <- computeResult{Value: result, Err: err}
}()

var result computeResult
select {
case result = <-ch:
case <-ctx.Done():
	return Result{}, ctx.Err()
}
if result.Err != nil {
	return Result{}, fmt.Errorf("compute: %w", result.Err)
}
```

### Channel size rule

**0 or 1. Any other size needs a comment.**

```go
make(chan Event)          // fine: synchronous handoff
make(chan Result, 1)      // fine: one-shot future
make(chan LogEntry, 4096) // REQUIRES justifying comment
```

If you need a buffered channel > 1, you probably want errgroup.SetLimit,
a semaphore, or a ring buffer instead.

---

## 4. Common Patterns Done Right

### Fan-out/fan-in with errgroup

Collect results from parallel work using a mutex-protected slice:

```go
g, ctx := errgroup.WithContext(ctx)
g.SetLimit(len(shards))
var mu sync.Mutex
var results []Result
for _, shard := range shards {
    g.Go(func() error {
        r, err := shard.Query(ctx, q)
        if err != nil {
            return fmt.Errorf("query shard %s: %w", shard.Name, err)
        }
        mu.Lock()
        results = append(results, r...)
        mu.Unlock()
        return nil
    })
}
if err := g.Wait(); err != nil {
    return nil, err
}
```

### Background worker with graceful shutdown

```go
func (f *Flusher) Run(ctx context.Context) error {
    ticker := time.NewTicker(5 * time.Second)
    defer ticker.Stop()
    for {
        select {
        case <-ctx.Done():
            // Final flush before exit
            flushCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
            if err := f.buffer.Flush(flushCtx); err != nil {
                f.logger.ErrorContext(ctx, "final flush failed", "err", err)
            }
            cancel()
            return ctx.Err()
        case <-ticker.C:
            if err := f.buffer.Flush(ctx); err != nil {
                f.logger.ErrorContext(ctx, "periodic flush failed", "err", err)
            }
        }
    }
}
```

This pattern also covers periodic tasks: replace the flush call with
any periodic operation (metrics export, cache refresh, etc.).

### Rate limiting with golang.org/x/time/rate

```go
type RateLimitedClient struct {
    client  *http.Client
    limiter *rate.Limiter
}

func (c *RateLimitedClient) Do(ctx context.Context, req *http.Request) (*http.Response, error) {
    if err := c.limiter.Wait(ctx); err != nil {
        return nil, fmt.Errorf("rate limiter: %w", err)
    }
    return c.client.Do(req)
}

// Construction: rate.NewLimiter(rate.Limit(rps), burst)
```

### Timeout enforcement

Derive a child context with a deadline. Never use `time.After` in a select — it leaks timers.

```go
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
defer cancel()
return s.client.Get(ctx, "/data/"+id)
```

### Cancellation causes

A bare `context canceled` in production logs tells you *that* cancellation
occurred, not *why*. Use `context.WithCancelCause` to attach a typed reason,
and `context.Cause(ctx)` to read it at boundaries.

```go
func processOrder(ctx context.Context, orderID string) error {
    ctx, cancel := context.WithCancelCause(ctx)
    defer cancel(nil) // nil cause → defaults to context.Canceled

    // Manual timer gives full control over the timeout cause.
    timer := time.AfterFunc(5*time.Second, func() {
        cancel(fmt.Errorf("order %s: 5s processing timeout", orderID))
    })
    defer timer.Stop()

    if err := checkInventory(ctx, orderID); err != nil {
        cancel(fmt.Errorf("order %s: inventory check failed: %w", orderID, err))
        return err
    }
    return chargePayment(ctx, orderID)
}
```

At the boundary, log both the category and the cause:

```go
if ctx.Err() != nil {
    logger.ErrorContext(ctx, "request failed",
        "err", ctx.Err(),            // "context canceled" or "context deadline exceeded"
        "cause", context.Cause(ctx), // "order ord-123: 5s processing timeout"
    )
}
```

**`WithTimeoutCause` gotcha:** The cause only fires when the timer expires.
On normal return, `defer cancel()` passes `nil`, so `context.Cause` returns
plain `context.Canceled`. If you need distinct causes on all paths (timeout,
error, success), use the manual timer pattern above with `WithCancelCause`.

**When `WithTimeoutCause` is fine:** Simple deadline enforcement where you only
care about distinguishing "timed out" from "cancelled by parent" — no need for
per-path causes:

```go
ctx, cancel := context.WithTimeoutCause(ctx, 30*time.Second,
    fmt.Errorf("enrichment for user %s: 30s timeout", userID),
)
defer cancel()
return s.enricher.Enrich(ctx, userID)
```

**First-cancel-wins:** `WithCancelCause` records only the first non-nil cause.
Subsequent calls are no-ops. This means the most specific reason (the first
failure) is preserved automatically.

---

## 5. Closure Capture Pitfalls

Go closures capture variables by reference, not by value. A closure over a
pointer (or a method value on a pointer receiver) sees every future mutation
of the underlying data. This is a data race when the closure runs in another
goroutine, and a logic bug even in single-threaded code when state changes
between creation and invocation.

### The rule

When a closure or method value lands on a long-lived struct or is passed to
another goroutine, **capture immutable values (strings, ints, copies) rather
than pointers to mutable structs**. If the closure genuinely needs live state,
guard reads with the same synchronization the writers use.

### Pointer capture sees future writes

```go
// WRONG — closure reads through cfg on every call, sees mutations
cfg := &Config{Host: "localhost", Port: 8080}
c.addr = func() string {
    return fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
}
cfg.Host = "example.com" // c.addr() now returns "example.com:8080"

// RIGHT — snapshot what the closure needs at creation time
host, port := cfg.Host, cfg.Port
c.addr = func() string {
    return fmt.Sprintf("%s:%d", host, port)
}
```

### Method values capture their receiver

`conn.crashMsg = s.serverCrashMessage` is syntactic sugar for a closure that
captures `s`. Every field the method reads is shared state. If the server
mutates `s.logFile` (rotation, shutdown), the connection's crash message
races or reports stale data.

```go
// WRONG — method value keeps s alive, reads s.logFile on each call
func (s *Server) newConn() *Conn {
    return &Conn{crashMsg: s.serverCrashMessage}
}

// RIGHT — copy the value the closure needs while it's known valid
func (s *Server) newConn() *Conn {
    name := s.logFile.Name()
    return &Conn{
        crashMsg: func() string { return "log: " + name },
    }
}
```

### Captured parameters shared across concurrent handlers

A closure parameter is still captured by reference. In HTTP handlers, `net/http`
dispatches each request in its own goroutine — mutations to a captured variable
race without any explicit `go` statement in your code.

```go
// WRONG — all concurrent requests mutate the same captured bool
func NewMiddleware(next http.Handler, rateLimit bool) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        if strings.HasPrefix(r.URL.Path, "/admin") {
            rateLimit = false // races with every other request
        }
        // ...
    })
}

// RIGHT — shadow with a per-request copy
func NewMiddleware(next http.Handler, rateLimit bool) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        rateLimit := rateLimit // per-request copy via :=
        if strings.HasPrefix(r.URL.Path, "/admin") {
            rateLimit = false
        }
        // ...
    })
}
```

### Named return variables — compiler-generated writes

Named return variables are scoped to the entire function body. A `return 20`
statement *writes* to the named variable — the compiler generates an assignment
invisible in source. A goroutine that captures the named return races with that
write even though no explicit assignment appears at the `return` site.

```go
// WRONG — compiler writes to 'result' on return; goroutine reads it
func compute(ctx context.Context) (result int, err error) {
    go func() {
        log.Println(result) // races with the implicit write below
    }()
    return 20, nil // compiler generates: result = 20; err = nil; return
}

// RIGHT — use a local variable; return it explicitly
func compute(ctx context.Context) (int, error) {
    r := 20
    go func() {
        log.Println(r) // safe: r is never reassigned after goroutine launch
    }()
    return r, nil
}
```

**Rule**: never launch a goroutine that reads a named return variable.
If you need named returns (for deferred error annotation), do not share
those names with concurrent closures.

### Slice internals are value types

A slice header is three words (pointer, length, capacity) copied by value at
every assignment, argument pass, and closure capture. Locking an `append` does
not protect a concurrent reader that copied the header *outside* the lock.

```go
var mu sync.Mutex
var results []Result

// Writer (locked)
mu.Lock()
results = append(results, r) // may reallocate — updates ptr, len, cap
mu.Unlock()

// WRONG — copies slice header without lock, races on len/cap fields
go func() {
    process(results) // header copied here, outside any lock
}()

// RIGHT — copy under lock, or read after all writes complete
mu.Lock()
snapshot := make([]Result, len(results))
copy(snapshot, results)
mu.Unlock()
go func() {
    process(snapshot)
}()
```

The race detector *will* fire on the meta-field read, but the bug is
non-obvious because the programmer thinks "I only locked the append."
The same applies to passing a shared slice to `errgroup.Go` — always
snapshot or wait for `g.Wait()` before reading.

### Detection and limits

- `go test -race` catches concurrent races but not single-threaded
  "wrong value at the wrong time" bugs (e.g., log rotation example).
- `go vet`'s `loopclosure` catches loop-variable capture in pre-1.22
  modules but cannot catch the broader struct-capture case.
- Go 1.22 fixed loop-variable lifetime, but pointer captures and method
  values on long-lived receivers are unchanged.

---

## 6. Anti-Patterns to Never Generate

```go
go sendEmail(user, body)                           // NEVER: fire-and-forget
go func() { cache.Set(key, compute()) }()          // NEVER: naked go, no owner
for !ready.Load() {}                               // NEVER: busy spin
mu := make(chan struct{}, 1); mu <- struct{}{}; <-mu // NEVER: channel as mutex
ch := make(chan int); ch <- 42                      // DEADLOCK: unbuffered, no receiver
func init() { go backgroundRefresh() }             // NEVER: goroutine in init
conn.onError = s.handleError                       // NEVER: method value on mutable receiver without documenting shared fields
```

**select without done case** — every select in a goroutine loop must
include `case <-ctx.Done()`. No exceptions.

---

## 7. Leak Detection with goleak

Use `go.uber.org/goleak` to catch goroutine leaks in tests. Put this in
every package that starts goroutines.

### TestMain — package-wide leak detection

```go
func TestMain(m *testing.M) {
    goleak.VerifyTestMain(m)
}
```

### Per-test verification

```go
func TestWorkerShutdown(t *testing.T) {
    defer goleak.VerifyNone(t)

    ctx, cancel := context.WithCancel(t.Context())
    g, ctx := errgroup.WithContext(ctx)
    w := NewWorker()
    g.Go(func() error {
        return w.Run(ctx)
    })

    // ... exercise the worker ...

    cancel()
    if err := g.Wait(); err != nil && !errors.Is(err, context.Canceled) {
        t.Fatalf("wait for worker shutdown: %v", err)
    }
}
```

### Filtering known library goroutines

```go
func TestMain(m *testing.M) {
    goleak.VerifyTestMain(m,
        goleak.IgnoreTopFunction("database/sql.(*DB).connectionOpener"),
    )
}
```

### Deterministic time testing with testing/synctest (Go 1.24+)

Use `synctest.Run` to test ticker/timer code without real sleeps. Fake time
advances only when all goroutines in the bubble are blocked.

```go
func TestFlusherTick(t *testing.T) {
    synctest.Run(func() {
        var count atomic.Int64
        ctx, cancel := context.WithCancel(context.Background())
        go func() {
            ticker := time.NewTicker(5 * time.Second)
            defer ticker.Stop()
            for {
                select {
                case <-ctx.Done():
                    return
                case <-ticker.C:
                    count.Add(1)
                }
            }
        }()

        time.Sleep(12 * time.Second) // advances fake clock, not wall time
        cancel()
        synctest.Wait() // wait for goroutine to exit
        if got := count.Load(); got != 2 {
            t.Fatalf("expected 2 ticks, got %d", got)
        }
    })
}
```

Prefer `synctest` over mocking time interfaces — it works with the real
`time` package and catches races that interface mocks hide.

### Production leak detection (Go 1.26+, experimental)

`/debug/pprof/goroutineleak` uses GC reachability to detect leaked goroutines
in running services. `goleak` remains the primary tool for tests; the pprof
profile is complementary for production observability.

---

## Decision Matrix

| Question | Answer |
|---|---|
| Need concurrent work? | `errgroup.WithContext` |
| Need to limit concurrency? | `errgroup.SetLimit` or `semaphore.Weighted` |
| Need to protect shared state? | `sync.Mutex` |
| Need read-heavy locking? | `sync.RWMutex` (only after profiling) |
| Need a counter/flag? | `go.uber.org/atomic` |
| Need to signal done? | `context.CancelFunc` or `close(chan struct{})` |
| Need one async result? | `chan T` (buffered 1) |
| Need a pipeline? | Bounded channel + worker goroutines via errgroup |
| Need multiple subsystems? | `oklog/run.Group` |
| Need periodic background work? | `time.Ticker` + `select` on `ctx.Done()` |
| Need rate limiting? | `golang.org/x/time/rate.Limiter` |
| Need to detect goroutine leaks? | `go.uber.org/goleak` |
