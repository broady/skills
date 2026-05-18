---
name: production-go
description: >
  Strict production Go standards for agent-produced or reviewed Go code. Trigger for
  almost all non-trivial production or deployed Go work: services, libraries, CLIs,
  concurrency, error handling, HTTP/gRPC, DB access, config, linting, and
  observability. Also trigger
  when the user mentions goroutines, context, slog, errgroup, circuit breaker, retry,
  health checks, graceful shutdown, golangci-lint, transaction management, or asks to
  review Go code for production safety. For toy examples, apply only the safety rules
  relevant to the task.
license: Apache-2.0
compatibility: Requires Go 1.26+, golangci-lint
metadata:
  author: cbro
  version: "0.6"
---

# Production Go

Readability over writability. Explicitness over magic. Compile-time safety over
runtime checks. Bounded everything. Correctness at boundaries.

## How to use this skill

1. **Classify the task** — review, produce, scaffold, design, add concurrency,
   add DB/async, add observability, configure linting/tests.
2. **Always enforce safety invariants** (below) — these apply to every task.
3. **Preserve existing framework choices** unless the task is a new scaffold or
   a planned migration. Do not introduce a second logger, router, CLI framework,
   RPC stack, or DI style in one-off changes.
4. **Load only the reference file needed** for the current task (see router below).

## Scope Calibration

| Task | Apply |
|---|---|
| Script / prototype | Safety invariants 1-5 only (globals, init, errors, goroutines, bounds). Skip lifecycle, config, observability |
| Internal library | All safety invariants. Tier 2 defaults. Skip server/scaffold |
| Service (internal) | Full skill. Load relevant references |
| Service (public API) | Full skill + boundary contracts, API evolution (design-idioms.md) |

## The Five Questions

Before approving any code — agent-produced or human-written — answer these:

1. **Who owns this data?** If a function stores a reference, it must own a copy. If it returns internal state, it returns a copy. If data crosses a system boundary, validate it.
2. **Who handles this error?** The boundary handles it (logs, maps to status). Interior code wraps and returns. Never both. Never swallowed.
3. **Who owns this goroutine?** Every goroutine must be traceable to a manager that can stop it and wait for it. If you can't point to the owner, it's a leak.
4. **What bounds this resource?** Every retry loop, queue, request body, connection pool, HTTP client, worker count, and shutdown path needs an explicit budget.
5. **Is this the right data?** At system boundaries: correct ID type? Field actually populated? Invariants documented and validated?

## Safety Invariants

These prevent production incidents. Apply unconditionally to all hand-written
and agent-produced code. Tool-generated files (protobuf stubs, sqlc output,
`go generate` artifacts) are exempt; do not modify them.

1. **No mutable globals.** Package-level `var` only for sentinels, compile-time checks, and immutable-by-construction values. Everything else flows through constructors. See [references/design.md](references/design.md).
2. **Avoid `init()`.** Prefer explicit registry assembly. `init()` is acceptable only for deterministic metadata/factory registration with no I/O, goroutines, live dependencies, or config reads. See [references/design.md](references/design.md) and [references/plugin-systems.md](references/plugin-systems.md).
3. **Errors: propagate with context, handle once at the boundary.** Use `%w` only when exposing the cause is stable contract; otherwise `%v` or map to domain error. Never log and return. See [references/errors.md](references/errors.md).
4. **No naked goroutines.** A goroutine's maximum lifetime must be bounded by the scope that owns and waits for it. Start goroutines via `sync.WaitGroup.Go`, `errgroup`, `run.Group`, `safe.Collect`, or an explicit owner that can cancel and wait. Looping or blocking goroutines select on `ctx.Done()`. Raw `go` requires documented owner, stop path, wait path, and reason. For servers with goroutine-per-connection patterns bounded by MaxConn, a goroutine gate function (check state + WaitGroup.Add + go) is acceptable; see [references/concurrency.md](references/concurrency.md).
5. **Bounded concurrency.** `errgroup.SetLimit(n)` or `semaphore.Weighted`. Never unbounded goroutines in a loop.
6. **Graceful shutdown is mandatory and phased.** Drain → Hammer → Terminate. See [references/server/scaffold.md](references/server/scaffold.md).
7. **Bound every resource explicitly.** HTTP servers/clients: explicit timeouts. DB pools: `MaxConns`, lifetime, idle time. Retries: max attempts + backoff. Queues: explicit capacity. Shutdown: deadline on drain.
8. **Strong types for domain values.** `type AccountID string`, `type Cents int64`. Prevents wrong-ID-type bugs at compile time.
9. **System boundary contracts.** Cross-service data validated at boundaries: correct ID types, populated fields, documented invariants. Treat external data with suspicion.
10. **No `log.Fatal`, `os.Exit` outside `main()`.** Library/service code returns errors.
11. **Deployment-varying operational parameters from configuration.** Addresses, credentials, feature flags, pool sizes, and values that differ between environments are loaded from config. Protocol-level correctness values (timeouts derived from downstream SLOs, security boundaries) live in code with documented rationale.
12. **Copy mutable data at ownership boundaries.** Store a caller-provided slice/map → copy it. Return internal state → return a snapshot. See [references/design-idioms.md](references/design-idioms.md).
13. **Context is not a service locator.** First parameter, never stored in a struct. Used for cancellation, deadlines, request-scoped values. Dependencies go through constructors. Exception: storing a context that represents component/config lifecycle (not request lifetime) is acceptable when the context is created at provision and cancelled at unload.
14. **No panic, no recover in application code.** Return errors. Panics crash the process; the orchestrator restarts it. `recover` appears only in package-internal entry points (structured longjmp for recursive code) and infrastructure boundaries (add observability then re-panic — the process still crashes). Goroutine managers (`errgroup`, `safe.Collect`, goroutine gates) do **not** recover panics. `panic()` is limited to programmer-error invariants in Must* constructors, exhaustive switches, and `_ struct{}` API evolution safety. See [references/errors.md](references/errors.md).

## Output Contracts

### When reviewing code

Structure findings as:

- **Must fix** — safety invariant violations, production incident risks
- **Should fix** — Tier 2 defaults violated in new code, unclear ownership
- **Nice to have** — style, naming, documentation
- **Verify** — tests/commands the author should run

### When producing code

State clearly:

- Files created or modified
- Which safety invariants are satisfied and how
- Tests added (or why not)
- Commands to validate (`go vet`, `golangci-lint run`, `go test ./...`)

### When scaffolding a new service

Include:

- File tree with one-line purpose per file
- Config knobs with defaults and env var names
- Shutdown behavior: what drains, what gets force-killed, what timeout
- Lint + test + run commands (prefer Taskfile.yml)

### When modifying existing code

Separate changes into:

- **Safety fixes** — applied immediately regardless of style
- **Style migrations** — only if the task is a planned migration of the subsystem

## Review Checklist

Before finalizing, verify no violations. The first group applies always (safety
invariants). The second group applies to services and systems — skip for scripts
and libraries.

**Always check:**

- Unmanaged goroutines or unbounded concurrency
- Buffered channels with capacity > 1 and no backpressure-contract comment
- Ignored/swallowed errors or log-and-return
- Mutable package globals or unsafe `init()`
- `http.DefaultClient`, `http.Get()`, or servers without explicit timeouts
- Hardcoded operational parameters
- Public API with ambiguous same-type arguments or naked booleans
- Cross-system data used without boundary validation
- Missing tests for changed error handling, cancellation, concurrency, or ownership paths

**Service/system scope:**

- Server shutdown that doesn't drain every listener, worker, and dependency
- Shutdown not ordered: components stopped in wrong order (producers before consumers, or missing drain phase)
- Error taxonomy missing: errors not classified as permanent/retryable at creation
- Config reload unsafe: no rollback on failure, no serialization of concurrent reloads
- Backpressure missing: unbounded queue or channel between pipeline stages
- Data writes not atomic: no temp-file-then-rename for durable writes
- Plugin cleanup missing: no Cleanup on partial Provision/Start failure

## Decision Table

### Core (all scopes)

| I need to... | Do this |
|---|---|
| Run N things concurrently (fire-and-wait, no errors) | `sync.WaitGroup.Go` (Go 1.24+) |
| Run N things concurrently (all must succeed, error returns) | `errgroup.WithContext` + `SetLimit` |
| Run N things concurrently (best-effort collect) | `safe.Collect` — bounded, per-item errors, panics crash |
| Pass a dependency | Constructor parameter |
| Configure optional settings | Config struct with zero-value defaults + `Validate() error` |
| Handle an error | Add operation context and return; `%w` only for stable contract |
| Represent a domain identifier | `type FooID string` / `type FooID int64` — not raw primitive |
| Log | `*slog.Logger` via constructor |
| Protect shared state | `sync.Mutex` (read-heavy: `sync.RWMutex`); compound mutations: `safe.Locked[T]` |
| Make outgoing HTTP requests | Custom `http.Client{Timeout: ...}`. Never `http.DefaultClient` |
| Handle partial failure in fan-out | Collect results + errors separately. Return partial results with warnings — see [references/resilience.md](references/resilience.md) |

### Service scope

| I need to... | Do this |
|---|---|
| Run multiple subsystems in one process | `errgroup` (shared cancel) or `oklog/run.Group` (independent interrupt/cleanup) |
| Map errors at boundary | Domain errors → HTTP/gRPC status via error map |
| Run DB operations atomically | `WithTx(ctx, db, fn)` — fn receives `*sql.Tx`, pass via `Querier` interface |
| Serve HTTP | `http.Server{}` with explicit `ReadHeaderTimeout`, `ReadTimeout`, `WriteTimeout`, `IdleTimeout` |
| Protect outbound calls (retry, breaker, timeout) | `failsafe-go` composition per dependency — see [resilience.md](references/resilience.md) |
| Classify errors for retry decisions | Tag at creation: permanent (never retry), retryable (with backoff). Use `SafeToRetry` for DB/network — see [references/errors.md](references/errors.md) |
| Manage process lifecycle (multi-subsystem) | `run.Group` for independent subsystems, topological ordering for pipelines — see [references/lifecycle.md](references/lifecycle.md) |
| Reload config without downtime | Start-then-stop or atomic handler swap. Serialize reloads. Support rollback — see [references/lifecycle.md](references/lifecycle.md) |
| Write data durably | temp file → write → fsync → rename → fsync dir — see [references/data-integrity.md](references/data-integrity.md) |
| Apply backpressure in a pipeline | Multi-layered: memory limits → queue capacity → rate limiting. Never single mechanism — see [references/backpressure.md](references/backpressure.md) |
| Prevent API breaking changes in Go | `_ struct{}` as last field in public structs. Sealed factory interfaces via unexported methods — see [references/design-idioms.md](references/design-idioms.md) |
| Run a controller/reconciliation loop | Informer → rate-limited work queue → bounded workers. DeepCopy before mutation — see [references/controller-loops.md](references/controller-loops.md) |
| Manage plugin/extension lifecycle | Provision → Validate → Start → Stop → Cleanup. Cleanup on partial failure — see [references/plugin-systems.md](references/plugin-systems.md) |
| Implement multi-tenant isolation | Tenant ID in context (never struct). Per-tenant limits via runtime-reloadable config — see [references/config.md](references/config.md) |

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
| [references/backpressure.md](references/backpressure.md) | Tiered flow control, memory limiters, queue bounds, per-tenant rate limiting, slow consumer handling | Connecting pipeline stages with channels/queues, adding rate limiting, handling slow consumers |
| [references/concurrency.md](references/concurrency.md) | Structured concurrency model, goroutine lifecycle, bounded concurrency, goroutine gate, sync vs channels, Locked[T] | Adding goroutines, protecting shared state with mutexes, choosing between channels and sync primitives |
| [references/concurrency-patterns.md](references/concurrency-patterns.md) | Fan-out/fan-in, background workers, closure pitfalls, cancellation causes, anti-patterns, goleak, synctest | Writing loops that spawn goroutines, reviewing concurrent code, testing time-dependent code, rate limiting, singleflight |
| [references/errors.md](references/errors.md) | Error types, wrapping, sentinels, boundary mapping, panic/recover | Adding/modifying functions that return errors, wrapping errors from dependencies, mapping errors to HTTP/gRPC status codes |
| [references/config.md](references/config.md) | What belongs in config vs code, Secret type, validation, LoadConfig pattern, graduation criteria, env-only/file/CLI deviations, config hot-reload basics | Config loading, adding config values, deciding what should be configurable, basic hot-reload patterns |
| [references/controller-loops.md](references/controller-loops.md) | Informer-queue-worker pattern, reconciliation, work queue semantics, bounded retry, cache sync | Writing Kubernetes-style controllers, reconciliation loops, event-driven processing |
| [references/data-integrity.md](references/data-integrity.md) | Atomic writes, verify-after-write, conflict detection, filesystem safety, crash recovery | Writing data to disk safely, handling concurrent file access, backup/sync operations |
| [references/design.md](references/design.md) | Packages, DI, interfaces, API design, config structs, builders | Adding a new package, writing a constructor, designing a public API, choosing between config structs and options |
| [references/design-idioms.md](references/design-idioms.md) | Struct design, Uber guardrails, function organization, generics, API evolution (_ struct{}), copy semantics | Adding exported structs, using generics, deciding receiver types, evolving a public API without breaking callers |
| [references/testing.md](references/testing.md) | goleak, property testing, integration tests, benchmarks, fakes | Writing tests (especially for concurrent or integration code), adding benchmarks, choosing test doubles |
| [references/lifecycle.md](references/lifecycle.md) | Process lifecycle orchestration, run.Group, supervision trees, config reload, shutdown phasing | Wiring multiple long-running components together, adding config hot-reload, changing shutdown order |
| [references/linting.md](references/linting.md) | golangci-lint config, linter rationale, CI setup | Setting up or modifying golangci-lint config, adding linters to CI |
| [references/performance.md](references/performance.md) | Profiling workflow, escape analysis, allocation reduction, GC tuning (GOGC/GOMEMLIMIT), benchmarking | Optimizing a function identified as slow by profiling, reducing allocations, tuning GC for containers, writing benchmarks |
| [references/plugin-systems.md](references/plugin-systems.md) | Module lifecycle, explicit registries, sealed interfaces, two-phase commit, config-driven provisioning | Adding a plugin/extension point, writing a registry, managing component provision and teardown |
| [references/project-layout.md](references/project-layout.md) | Directory structure, dependency direction | Scaffolding a new service, adding a new package to an existing service, deciding where code lives |
| [references/invariant-checks.md](references/invariant-checks.md) | Runtime safety checks gated by environment, dev-only panics | Adding debug assertions, catching programmer errors during development, validating internal assumptions at runtime |

### Server

| File | Covers | Load when... |
|---|---|---|
| [references/server/scaffold.md](references/server/scaffold.md) | Kong CLI, loadConfig, complete main.go, run group, shutdown flow | Starting a new service or wiring the process entry point |
| [references/server/handlers.md](references/server/handlers.md) | Service layer, generic handler adapter, decoders, error mapping, HTTP server assembly | Adding endpoints, changing request/response handling |
| [references/server/middleware.md](references/server/middleware.md) | Request ID, logging, auth, no panic recovery | Adding or modifying HTTP middleware |
| [references/server/connect-grpc.md](references/server/connect-grpc.md) | Connect handlers, interceptors, traditional gRPC fallback | Adding gRPC/Connect services |
| [references/server/health.md](references/server/health.md) | Liveness vs readiness, ReadinessChecker interface | Adding or debugging health checks |

### Database & Async

| File | Covers | Load when... |
|---|---|---|
| [references/database/transactions.md](references/database/transactions.md) | Explicit tx passing, Querier interface, WithTx helper, nested service calls, connection safety (pgx) | Writing or reviewing code that uses SQL transactions, pgx connection pool safety |
| [references/database/cursor-iteration.md](references/database/cursor-iteration.md) | Keyset pagination, batched processing of large result sets | Iterating over large tables or implementing paginated queries |
| [references/database/async-brokers.md](references/database/async-brokers.md) | External broker consumers, retry with backoff, at-least-once delivery, in-process queues | Implementing async processing, background jobs, or message handling |

### Resilience & Flow Control

| File | Covers | Load when... |
|---|---|---|
| [references/resilience.md](references/resilience.md) | Circuit breaker, retry with budget, load shedding, hedged requests, bulkheading, backpressure, timeouts as a system, failsafe-go composition | Adding an HTTP/gRPC client to a new dependency, adding retry or timeout logic, reviewing outbound call patterns |

### Observability

| File | Covers | Load when... |
|---|---|---|
| [references/observability/logging.md](references/observability/logging.md) | slog setup, injection, scoped loggers, levels, LogAttrs, redaction, canonical log lines | Adding/changing logging, reviewing log hygiene |
| [references/observability/metrics-tracing.md](references/observability/metrics-tracing.md) | OTel provider setup, HTTP/gRPC middleware spans, manual spans, DB instrumentation, RED/USE metrics | Adding metrics or tracing, instrumenting endpoints |
| [references/observability/runtime.md](references/observability/runtime.md) | pprof, goroutine labels, runtime/metrics, expvar | Adding a pprof endpoint, exposing runtime metrics, labeling goroutines for debug |

## Packages

| Package | Use |
|---|---|
| [packages/safe](packages/safe) | `safe.Collect` (bounded best-effort fan-out with per-item errors), `safe.Locked[T]` (mutex-protected value with closure-based compound mutations). Panics are not recovered — they crash the process. Copy into a project or import when vendored. |
