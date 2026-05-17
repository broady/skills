---
name: production-go
description: >
  Strict production Go standards for generated or reviewed Go code. Trigger for
  almost all non-trivial Go work: services, libraries, CLIs, concurrency, error
  handling, HTTP/gRPC, DB access, config, linting, and observability. For toy
  examples, apply only the safety rules relevant to the task.
license: Apache-2.0
compatibility: Requires Go 1.26+, golangci-lint
metadata:
  author: cbro
  version: "0.4"
---

# Production Go

Readability over writability. Explicitness over magic. Compile-time safety over
runtime checks. Bounded everything. Correctness at boundaries.

## How to use this skill

1. **Classify the task** — review, generate, scaffold, design, add concurrency,
   add DB/async, add observability, configure linting/tests.
2. **Always enforce safety invariants** (below) — these apply to every task.
3. **Preserve existing framework choices** unless the task is a new scaffold or
   a planned migration. Do not introduce a second logger, router, CLI framework,
   RPC stack, or DI style in one-off changes.
4. **Load only the reference file needed** for the current task (see router below).

## The Five Questions

Before approving any code — generated or human-written — answer these:

1. **Who owns this data?** If a function stores a reference, it must own a copy. If it returns internal state, it returns a copy. If data crosses a system boundary, validate it.
2. **Who handles this error?** The boundary handles it (logs, maps to status). Interior code wraps and returns. Never both. Never swallowed.
3. **Who owns this goroutine?** Every goroutine must be traceable to a manager that can stop it and wait for it. If you can't point to the owner, it's a leak.
4. **What bounds this resource?** Every retry loop, queue, request body, connection pool, HTTP client, worker count, and shutdown path needs an explicit budget.
5. **Is this the right data?** At system boundaries: correct ID type? Field actually populated? Invariants documented and validated?

## Safety Invariants

These prevent production incidents. Apply unconditionally to all hand-written
code. Generated files are exempt; do not modify them.

1. **No mutable globals, avoid `init()`.** Package-level `var` only for sentinels, compile-time checks, and immutable-by-construction values. Everything else flows through constructors. See [references/design.md](references/design.md).
2. **Errors: propagate with context, handle once at the boundary.** Use `%w` only when exposing the cause is stable contract; otherwise `%v` or map to domain error. Never log and return. See [references/errors.md](references/errors.md).
3. **No naked goroutines.** A goroutine's maximum lifetime must be bounded by the scope that owns and waits for it. Start goroutines via `errgroup`, `run.Group`, `safe.Go`, `safe.Collect`, or an explicit owner that can cancel and wait. Looping or blocking goroutines select on `ctx.Done()`. Raw `go` requires documented owner, stop path, wait path, and reason. See [references/concurrency.md](references/concurrency.md).
4. **Bounded concurrency.** `errgroup.SetLimit(n)` or `semaphore.Weighted`. Never unbounded goroutines in a loop.
5. **Graceful shutdown is mandatory and phased.** Drain → Hammer → Terminate. See [references/server/scaffold.md](references/server/scaffold.md).
6. **Bound every resource explicitly.** HTTP servers/clients: explicit timeouts. DB pools: `MaxConns`, lifetime, idle time. Retries: max attempts + backoff. Queues: explicit capacity. Shutdown: deadline on drain.
7. **Strong types for domain values.** `type AccountID string`, `type Cents int64`. Prevents wrong-ID-type bugs at compile time.
8. **System boundary contracts.** Cross-service data validated at boundaries: correct ID types, populated fields, documented invariants. Treat external data with suspicion.
9. **No `log.Fatal`, `os.Exit` outside `main()`.** Library/service code returns errors.
10. **Operational parameters from configuration.** Addresses, credentials, feature flags loaded from config, never compiled into the binary.
11. **Copy mutable data at ownership boundaries.** Store a caller-provided slice/map → copy it. Return internal state → return a snapshot. See [references/design.md](references/design.md).
12. **Context is not a service locator.** First parameter, never stored in a struct. Used for cancellation, deadlines, request-scoped values. Dependencies go through constructors.
13. **No panic, no recover in application code.** Return errors. `recover` only in goroutine supervisors (`safe.Go`, `safe.Collect`) and package-internal entry points where panic is structured longjmp. See [references/errors.md](references/errors.md).

## Review Checklist

Before finalizing, verify no violations of:

- Unmanaged goroutines or unbounded concurrency
- Ignored/swallowed errors or log-and-return
- Mutable package globals or unsafe `init()`
- `http.DefaultClient`, `http.Get()`, or servers without explicit timeouts
- Hardcoded operational parameters
- Public API with ambiguous same-type arguments or naked booleans
- Cross-system data used without boundary validation
- Server shutdown that doesn't drain every listener, worker, and dependency
- Missing tests for changed error handling, cancellation, concurrency, or ownership paths

## Core Decision Table

| I need to... | Do this |
|---|---|
| Run N things concurrently (all must succeed) | `errgroup.WithContext` + `SetLimit` |
| Run N things concurrently (best-effort collect) | `safe.Collect` — bounded, panic-safe, per-item errors |
| Run multiple subsystems in one process | `errgroup` (shared cancel) or `oklog/run.Group` (independent interrupt/cleanup) |
| Pass a dependency | Constructor parameter |
| Configure optional settings | Config struct with zero-value defaults + `Validate() error` |
| Handle an error | Add operation context and return; `%w` only for stable contract |
| Map errors at boundary | Domain errors → HTTP/gRPC status via error map |
| Run DB operations atomically | `WithTx(ctx, db, fn)` — fn receives `*sql.Tx`, pass via `Querier` interface |
| Serve HTTP | `http.Server{}` with explicit `ReadHeaderTimeout`, `ReadTimeout`, `WriteTimeout`, `IdleTimeout` |
| Make outgoing HTTP requests | Custom `http.Client{Timeout: ...}`. Never `http.DefaultClient` |
| Represent a domain identifier | `type FooID string` / `type FooID int64` — not raw primitive |
| Log | `*slog.Logger` via constructor |
| Protect shared state | `sync.Mutex` (read-heavy: `sync.RWMutex`); compound mutations: `safe.Locked[T]` |

## Existing Codebases

Apply safety invariants immediately. Preserve existing framework choices unless
migrating the whole subsystem. Do not churn stable code for style compliance —
fix safety issues; leave aesthetics for new code or planned rewrites.

## Tier 2: Project Defaults

For new code only. These improve maintainability but should follow local style
in existing codebases. Constructor injection, consumer-side interfaces (1-2
methods), slog only (no global logger), useful zero values, generics for type
safety not cleverness, config structs over functional options, Kong for CLIs,
`golangci-lint` in CI. Details in the relevant reference files.

## References

Load a reference file only when the task involves its domain. Skip unrelated ones.

| File | Covers | Load when... |
|---|---|---|
| [references/concurrency.md](references/concurrency.md) | Structured concurrency model, goroutine lifecycle, workers, sync vs channels, closure pitfalls | Spawning goroutines, channels, workers, shared state |
| [references/errors.md](references/errors.md) | Error types, wrapping, sentinels, boundary mapping, panic/recover | Error contracts, error handling, boundary mapping |
| [references/design.md](references/design.md) | Packages, DI, interfaces, API design, config structs, builders, generics, defensive copies | Package structure, constructors, public APIs, config patterns |
| [references/server/](references/server.md) | HTTP+gRPC scaffold, shutdown, handlers, middleware, Connect, health | Building/modifying servers, endpoints, shutdown, middleware |
| [references/database/](references/database.md) | Transactions, cursor iteration, async brokers, retry, invariant checks | DB access, transactions, async work, pagination |
| [references/observability/](references/observability.md) | slog, redaction, canonical log lines, OTel, metrics, tracing, pprof | Logging, metrics, tracing, performance diagnostics |
| [references/testing.md](references/testing.md) | goleak, property testing, integration tests, benchmarks, fakes | Writing tests for concurrent code, integration infra, benchmarks |
| [references/linting.md](references/linting.md) | golangci-lint config, linter rationale, CI setup | Configuring linters, CI pipeline |
| [references/performance.md](references/performance.md) | Allocation reduction, profiling, benchmarking | Hot-path optimization, profiling |
| [references/project-layout.md](references/project-layout.md) | Directory structure, dependency direction | Greenfield service scaffolding |

## Packages

| Package | Use |
|---|---|
| [packages/safe](packages/safe) | `safe.Go` (supervised goroutine), `safe.Collect` (bounded best-effort fan-out), `safe.Locked[T]` (mutex-protected value with closure-based compound mutations). Copy into a project or import when vendored. |
