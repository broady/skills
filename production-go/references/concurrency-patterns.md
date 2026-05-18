# Concurrency Patterns Reference

Applied patterns, pitfalls, testing, and anti-patterns for concurrent Go code.
See [concurrency.md](concurrency.md) for the core model (lifecycle, bounded concurrency, sync primitives).

## Contents

1. [Common Patterns Done Right](#1-common-patterns-done-right) — fan-out/fan-in, background workers, rate limiting, timeouts, cancellation causes, singleflight
2. [Closure Capture Pitfalls](#2-closure-capture-pitfalls) — pointer capture, method values, concurrent handlers, named returns, slice headers
3. [Anti-Patterns to Never Generate](#3-anti-patterns-to-never-generate) — fire-and-forget, naked go, busy spin
4. [Leak Detection with goleak](#4-leak-detection-with-goleak) — TestMain, per-test, production detection
5. [Deterministic Time Testing with synctest](#5-deterministic-time-testing-with-synctest) — fake time, ticker/timer tests without real sleeps

---

## 1. Common Patterns Done Right

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
                f.logger.LogAttrs(ctx, slog.LevelError, "final flush failed", slog.Any("err", err))
            }
            cancel()
            return ctx.Err()
        case <-ticker.C:
            if err := f.buffer.Flush(ctx); err != nil {
                f.logger.LogAttrs(ctx, slog.LevelError, "periodic flush failed", slog.Any("err", err))
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
    logger.LogAttrs(ctx, slog.LevelError, "request failed",
        slog.Any("err", ctx.Err()),            // "context canceled" or "context deadline exceeded"
        slog.Any("cause", context.Cause(ctx)), // "order ord-123: 5s processing timeout"
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

### Request deduplication with singleflight

Use `golang.org/x/sync/singleflight` to collapse duplicate concurrent requests
for the same key into a single execution. Prevents thundering herd on cache miss.

```go
type UserCache struct {
    sf    singleflight.Group
    store Store
}

func (c *UserCache) GetUser(ctx context.Context, id string) (*User, error) {
    v, err, _ := c.sf.Do(id, func() (any, error) {
        return c.store.LoadUser(ctx, id)
    })
    if err != nil {
        return nil, err
    }
    return v.(*User), nil
}
```

**Context footgun:** all coalesced callers share the first caller's context.
If the first caller cancels, every waiting caller gets the cancellation error
— even callers whose contexts are still valid. For short, idempotent reads
this is acceptable. When it is not, detach the inner context:

```go
v, err, _ := c.sf.Do(id, func() (any, error) {
    // WithoutCancel strips cancellation AND deadlines from the parent.
    // An explicit timeout is mandatory — without it, the call has no bound.
    ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
    defer cancel()
    return c.store.LoadUser(ctx, id)
})
```

### context.AfterFunc — deferred cleanup on cancellation

`context.AfterFunc` (Go 1.21+) runs a function in its own goroutine when a
context is cancelled. This is a stdlib-blessed "naked goroutine" — the
goroutine has no explicit owner in your code. Manage its lifecycle via the
returned `stop` func:

```go
conn := acquireConnection()
stop := context.AfterFunc(ctx, func() {
    conn.Close() // called in a new goroutine when ctx is cancelled
})
defer stop() // prevent the goroutine from firing if we return first
```

**Rules for AfterFunc:**
- Always call the returned `stop` func (in a defer or cleanup path).
- Keep `f` short-lived and non-blocking — it runs in an unmanaged goroutine.
- Do not use AfterFunc as a general-purpose goroutine launcher; it is for
  context-triggered cleanup callbacks.

### context.WithoutCancel — strips deadlines too

`context.WithoutCancel` returns a context that is never cancelled and has
**no deadline**. It preserves only the parent's values. This means code
running under a `WithoutCancel` context has no bound unless you add one:

```go
// WRONG: no timeout, no deadline — the call can hang forever.
detached := context.WithoutCancel(ctx)
result, err := s.dependency.Call(detached, req)

// RIGHT: explicit timeout replaces the stripped deadline.
detached, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
defer cancel()
result, err := s.dependency.Call(detached, req)
```

Use `WithoutCancel` only when you explicitly need to outlive the parent (e.g.,
async post-processing after a handler returns, singleflight deduplication).
Always pair it with an explicit timeout.

---

## 2. Closure Capture Pitfalls

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

## 3. Anti-Patterns to Never Generate

```go
go sendEmail(user, body)                           // NEVER: fire-and-forget
go func() { cache.Set(key, compute()) }()          // NEVER: naked go, no owner
for !ready.Load() {}                               // NEVER: busy spin
mu := make(chan struct{}, 1); mu <- struct{}{}; <-mu // NEVER: channel as mutex
ch := make(chan int); ch <- 42                      // DEADLOCK: unbuffered, no receiver
func init() { go backgroundRefresh() }             // NEVER: goroutine in init
conn.onError = s.handleError                       // NEVER: method value on mutable receiver without documenting shared fields
```

**Unbounded goroutines in a loop without a gate** — if shutdown sets a flag
but new goroutines keep starting, they leak. Use the goroutine gate pattern
(see [concurrency.md §1](concurrency.md#1-goroutine-lifecycle-management)).

**select without done case** — every select in a goroutine loop must
include `case <-ctx.Done()`. No exceptions.

---

## 4. Leak Detection with goleak

Use `go.uber.org/goleak` to catch goroutine leaks in tests. Put this in
every package that starts goroutines.

Leak detection works because structured concurrency makes the expected set of
goroutines knowable. If every goroutine is owned, cancelable, and waited, then
an orphaned goroutine is a bug unless it is a documented library runtime
goroutine and explicitly filtered.

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
    // Naturally bounded: exactly one supervised goroutine.
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

### Production leak detection (Go 1.26+, experimental)

`/debug/pprof/goroutineleak` uses GC reachability to detect leaked goroutines
in running services. `goleak` remains the primary tool for tests; the pprof
profile is complementary for production observability.

---

## 5. Deterministic Time Testing with synctest

Use `testing/synctest` (Go 1.24+) to test ticker/timer code without real
sleeps. Fake time advances only when all goroutines in the bubble are blocked.

The API is `synctest.Test(t, func(t *testing.T))` — it integrates directly
with `*testing.T`. Use `t.Context()` for cancellation instead of manual
`context.WithCancel(context.Background())`.

```go
func TestFlusherTick(t *testing.T) {
    synctest.Test(t, func(t *testing.T) {
        var count atomic.Int64
        ctx := t.Context()
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
        // t.Context() is cancelled when the test ends; goroutine exits.
        if got := count.Load(); got != 2 {
            t.Fatalf("expected 2 ticks, got %d", got)
        }
    })
}
```

**Note:** Mutex operations are not "durably blocking" in synctest — the
bubble does not advance fake time while waiting on a mutex. If your test
needs to observe state protected by a mutex, use a channel or WaitGroup
signal to coordinate rather than relying on time advancement.

Prefer `synctest` over mocking time interfaces — it works with the real
`time` package and catches races that interface mocks hide.

---

## Decision Matrix

| Question | Answer |
|---|---|
| Need fire-and-wait (no error returns)? | `sync.WaitGroup.Go` (Go 1.25+) |
| Need concurrent work with error returns? | `errgroup.WithContext` |
| Need to limit concurrency? | `errgroup.SetLimit` or `semaphore.Weighted` |
| Need to protect shared state? | `sync.Mutex` |
| Need read-heavy locking? | `sync.RWMutex` (only after profiling) |
| Need a counter/flag? | `sync/atomic` typed wrappers (`atomic.Bool`, `atomic.Int64`, etc.) |
| Need to signal done? | `context.CancelFunc` or `close(chan struct{})` |
| Need one async result? | Prefer synchronous code; if adapting an async API, use `chan T` buffered 1 only with owner/stop/wait and context-aware producer documented, otherwise `errgroup` |
| Need a pipeline? | Bounded channel + worker goroutines via errgroup |
| Need multiple subsystems? | `errgroup` (shared cancel) or `oklog/run.Group` (independent interrupt/cleanup) |
| Need periodic background work? | `time.Ticker` + `select` on `ctx.Done()` |
| Need cleanup on context cancellation? | `context.AfterFunc` (call returned `stop` in a defer) |
| Need contractual rate limiting? | `golang.org/x/time/rate.Limiter` (enforces "N per second" contracts; not a substitute for adaptive load shedding — see [resilience.md](resilience.md)) |
| Need to deduplicate concurrent requests? | `golang.org/x/sync/singleflight` |
| Need to detect goroutine leaks? | `go.uber.org/goleak` |
