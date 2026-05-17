---
name: production-go
description: >
  Opinionated production Go for agent-generated code. Enforces safe concurrency
  (bounded goroutines, no fire-and-forget), strict error handling (wrap once,
  handle once), constructor injection (no mutable globals, avoid init), strong
  domain types, system boundary contracts, and bounded resources.
  Targets high-performance HTTP+gRPC servers on Go 1.26+. Use this skill
  whenever the user is writing, reviewing, or scaffolding Go code intended for
  production -- even if they don't explicitly say "production." Also trigger for
  Go project setup, Go API design, linter configuration, error handling patterns,
  graceful shutdown, dependency injection in Go, Go concurrency patterns, gRPC or
  Connect service design, Go code review, or any task where Go correctness and
  safety matter. If the user mentions Go and the code will run in a server,
  service, CLI tool, or library used by others, use this skill.
  Complements the go-testing skill.
license: Apache-2.0
compatibility: Requires Go 1.26+, golangci-lint
metadata:
  author: cbro
  version: "0.2"
  sources: >
    Dave Cheney (Practical Go), Peter Bourgon (Go for Industrial Programming),
    Bryan Mills (Rethinking Classical Concurrency Patterns), Uber Go Style Guide,
    Google Go Style Guide, Mat Ryer, CockroachDB style, Kubernetes conventions
---

# Production Go

Readability over writability. Explicitness over magic. Compile-time safety over
runtime checks. Bounded everything. Correctness at boundaries.

This is a strict production standard for fast, robust Go. Prefer patterns that
make incorrect code hard to write, easy to review, and observable in production.

## The Five Questions

Before approving any code — generated or human-written — answer these. They
represent the judgment calls no linter can make, and the bug classes that cause
production incidents:

1. **Who owns this data?** If a function stores a reference, it must own a copy. If it returns internal state, it returns a copy. If data crosses a system boundary, validate it — don't trust the writer got it right.
2. **Who handles this error?** The boundary handles it (logs, maps to status). Interior code wraps and returns. Never both. Never swallowed.
3. **Who owns this goroutine?** Every goroutine must be traceable to a manager that can stop it and wait for it. If you can't point to the owner, it's a leak.
4. **What bounds this resource?** Every retry loop, queue, request body, connection pool, HTTP client, worker count, and shutdown path needs an explicit budget.
5. **Is this the right data?** At system boundaries: is this the right ID type? Is this field actually populated? Are the invariants documented and validated? Semantic correctness matters more than structural correctness.

## Contract

- Every dependency is explicit.
- Every goroutine is owned, cancelable, bounded, and waited.
- Every error has a stable contract and is handled once.
- Every mutable or shared value has clear ownership.
- Every server starts, observes, drains, and exits predictably.
- Every public API is hard to misuse.
- Every cross-system data flow has a documented contract.
- Every operational parameter comes from configuration.
- Every performance claim is measured.
- Every generated example compiles or is clearly marked as pseudocode.

## Decision Table

| I need to... | Do this |
|---|---|
| Run N things concurrently (all-or-nothing) | `errgroup.WithContext` + `SetLimit` |
| Run N things concurrently (best-effort collect) | `safe.Collect` — bounded, panic-safe, per-item errors |
| Run multiple subsystems in one process | `errgroup` if they share a cancel signal; `oklog/run.Group` if they need independent interrupt/cleanup |
| Run a background worker | errgroup goroutine with `select` on `ctx.Done()` |
| Signal completion | `context.CancelFunc` or `chan struct{}` |
| Get one async result | `chan T` (size 1) |
| Protect shared state | non-pointer `sync.Mutex` field (read-heavy: `sync.RWMutex`); for values needing atomic read-modify-write: `safe.Locked[T]` |
| Lazy-initialize | `sync.Once` |
| Store a counter/flag | `go.uber.org/atomic` |
| Pass request metadata | `context.WithValue` |
| Pass a dependency | Constructor parameter |
| Define a dependency boundary | Interface at the consumer site |
| Configure optional settings | Config struct with zero-value defaults |
| Configure loaded/validated settings | Config struct + `Validate() error` |
| Choose exactly one of N | Interface field on config struct |
| Enforce construction order | Builder with type-state |
| Handle an error | `fmt.Errorf("op: %w", err)` and return |
| Report errors to callers | Map domain errors → HTTP/gRPC status at boundary |
| Serve HTTP | `http.Server{}` with explicit `ReadHeaderTimeout`, `ReadTimeout`, `WriteTimeout`, `IdleTimeout` |
| Make outgoing HTTP requests | Custom `http.Client{}` with `Timeout` set. Never `http.DefaultClient` or `http.Get()` in production |
| Connect to a database | Explicit pool config: `MaxConns`, `MaxConnLifetime`, `MaxConnIdleTime` |
| Represent a domain identifier | `type FooID string` / `type FooID int64` — strong type, not raw primitive |
| Log | `*slog.Logger` via constructor |
| Instrument | OpenTelemetry SDK |
| Build a CLI | `github.com/alecthomas/kong` for flags; config package for env/files/secrets |
| Discover all service config | Single config struct or `loadConfig()` — a reviewer answers "what does this connect to?" from one location |

## Strict Review Checklist

Before finalizing Go code, verify:

- No unmanaged goroutines.
- No ignored or swallowed errors.
- No mutable package globals or unsafe `init()`.
- No log-and-return error handling.
- No `log.Fatal`, `os.Exit`, or `log.Fatalf` outside `main()`.
- No context-carried dependencies.
- No uncopied slices, maps, or pointer-owned data crossing ownership boundaries.
- No closures or method values capturing mutable receivers/pointers without documenting shared fields or snapshotting values.
- No unbounded concurrency, queues, retries, or timeouts.
- No `http.DefaultClient`, `http.Get()`, or `http.Server{}` without explicit timeouts.
- No hardcoded operational parameters (addresses, credentials, season IDs, feature flags).
- No public API with ambiguous same-type arguments or naked booleans.
- Cross-system data validated at boundaries: correct ID types, populated fields, documented invariants.
- Server shutdown drains every listener, worker, telemetry provider, and dependency.
- Tests cover changed behavior; if the change touches error handling, cancellation, concurrency, or ownership, tests exercise those paths.
- Generated code (protobuf, OpenAPI, sqlc, etc.) is exempt from style rules; do not modify generated files.

## Existing Codebases

Apply safety rules immediately: goroutine ownership, bounded concurrency, error
contracts, system boundary validation, context discipline, defensive copies, and
graceful shutdown. Preserve existing framework choices unless migrating the whole
subsystem. Do not introduce a second logger, CLI framework, router, RPC stack,
metrics library, or DI style in one-off changes.

**What NOT to fix in stable code:** Don't refactor working flag-based config into
Kong just because the rule says so. Don't add errgroup to a signal handler
goroutine that already works. Don't add interfaces just to have them. Don't
migrate to slog if the existing logger is consistent and adequate. The cost of
churn for pure-style compliance in stable code exceeds the benefit. Fix safety
issues; leave aesthetic preferences for new code or planned rewrites.

## New Code Defaults

For new production services, use generated or contract-first HTTP APIs when an
OpenAPI spec exists or is expected. Keep transport handlers thin: decode,
validate, call one application service method, map domain errors, encode. Use
Connect for RPC when possible, Kong for CLIs, `slog` for logging, OpenTelemetry
for metrics and tracing, `golangci-lint` for static checks, `errgroup` for
bounded all-or-nothing task groups, `safe.Collect` for bounded best-effort
fan-out/collect, `oklog/run.Group` for multi-subsystem lifecycle management,
and `goleak` for packages that start goroutines. Deviations require a concrete
operational reason.

## Rules — Tier 1: Safety

These prevent production incidents. Apply them unconditionally to all code —
new, existing, generated wrappers, and reviews.

1. **No mutable globals, avoid `init()`**. Package-level `var` only for sentinel errors, compile-time interface checks, and values that are immutable by construction (embed.FS, compiled regexps, `strings.NewReplacer(...)`, `cases.Fold()`, sync.Pool with deterministic New). Package-level slices, maps, pointers, and structs with mutable fields are mutable even when treated as read-only; build them in constructors or return defensive copies. Avoid `init()`; if unavoidable, it must be deterministic, avoid I/O/env/global mutation/goroutines, and not depend on init ordering. Everything else flows through constructors. See [references/design.md](references/design.md).

2. **Errors: propagate with context, handle once at the boundary.** Interior code wraps and returns: `return fmt.Errorf("operation: %w", err)`. Boundaries log, count, retry, or map errors to user-facing responses. Never log and return the same error. Never swallow errors silently — if intentionally discarding, annotate with `//nolint:errcheck`. `%w` exposes an error contract; `%v` hides implementation details. See [references/errors.md](references/errors.md).

3. **No naked goroutines.** Every goroutine is started by `errgroup`, `run.Group`, `safe.Go`, `safe.Collect`, or an explicit owner that can cancel it and wait for it. Looping or blocking goroutines select on `ctx.Done()`. Raw `go` statements require a documented owner, stop path, wait path, and reason. See [references/concurrency.md](references/concurrency.md).

4. **Bounded concurrency.** `errgroup.SetLimit(n)` or `semaphore.Weighted`. Never spawn unbounded goroutines in a loop.

5. **Graceful shutdown is mandatory.** SIGTERM/SIGINT → drain in-flight → close dependencies → exit. Use `oklog/run.Group` for multi-subsystem coordination when subsystems need independent interrupt/cleanup. Use `errgroup` when subsystems share a cancel signal. See [references/server.md](references/server.md).

6. **Bound every resource explicitly.** Unbounded defaults are production bugs:
   - HTTP servers: `ReadHeaderTimeout`, `ReadTimeout`, `WriteTimeout`, `IdleTimeout`.
   - HTTP clients: custom `http.Client{Timeout: ...}`. Never use `http.DefaultClient` or `http.Get()` in production code.
   - Database pools: `MaxConns`, `MaxConnLifetime`, `MaxConnIdleTime`.
   - Retry loops: max attempts or deadline. Exponential backoff with jitter.
   - Queues and buffers: explicit capacity tied to expected load.
   - Shutdown: deadline on drain (e.g., 30s). Don't wait forever.

7. **Strong types for domain values.** `type AccountID string`, `type MembershipID int64`, `type Cents int64`. Prevents argument swapping, wrong-ID-type bugs, and primitive obsession at compile time. This is the purest expression of "compile-time safety over runtime checks." Use strong types especially at system boundaries where ID confusion causes data corruption.

8. **System boundary contracts.** When two services communicate through a shared database, message queue, or API: document the data contract (which fields, which ID types, which invariants). Treat cross-service data reads with the same suspicion as external API responses — validate types and ranges, don't assume the writer got it right. The bug is not "we didn't inject the dependency cleanly" — it's "we used the wrong membership ID column and corrupted 10k records."

9. **No `log.Fatal`, `os.Exit` outside `main()`.** They bypass deferred cleanup, don't flush telemetry, and are unrecoverable panics in disguise. Library and service code must return errors. Only `main()` calls `os.Exit`.

10. **Operational parameters from configuration.** Addresses, credentials, season identifiers, feature flags, and external service URLs are loaded from config (env vars, files, or secrets), never compiled into the binary. If changing a value requires recompilation and redeployment, it's a hardcoded operational parameter. See [references/server.md](references/server.md).

11. **Copy mutable data at ownership boundaries.** When a struct stores a caller-provided slice, map, or pointer to mutable data, it must take ownership by copying or document shared ownership explicitly. When returning internal mutable data, return a snapshot. See [references/design.md](references/design.md).

12. **Context is not a service locator.** `context.Context` is the first parameter and is never stored in a struct. Use it for cancellation, deadlines, and request-scoped values that genuinely flow with the request (trace ID, auth principal, request ID). Dependencies go through constructors, never through `ctx.Value` helpers like `ctxutil.DB(ctx)`.

13. **No panic, no recover in application code.** Return errors. Library code may panic only for true invariant violations where continuing is unsafe. `recover` is allowed in exactly two places: goroutine supervisors (`safe.Go`, `safe.Collect`) and package-internal entry points (recursive parsers/walkers where panic is a structured longjmp that never escapes the package). In both cases, the panic is converted to a returned error — never swallowed. Do not add HTTP/gRPC panic recovery middleware. See [references/errors.md](references/errors.md).

## Rules — Tier 2: Quality

These improve maintainability and readability. Apply to new code. In existing
codebases, apply only when already modifying the relevant code or during planned
rewrites. Do not churn stable code purely for style compliance.

14. **Constructor injection.** The wiring in `main()` IS the dependency graph. Manual wiring scales: organize large graphs into small `newX(...)` helpers, not service locators or DI containers. Constructor injection is a means to testability; testability is a means to correctness. Prioritize the end (correctness, observable behavior) over the means (DI purity). See [references/design.md](references/design.md).

15. **Interfaces at the consumer, not the producer.** 1-2 methods. Accept interfaces, return structs. No interface is better than a premature interface — don't define one until you need a test double or a second implementation. Concrete types are fine when you control both sides. `var _ I = (*T)(nil)` for compile-time checks. See [references/design.md](references/design.md).

16. **Channels: size 0 or 1.** Unbuffered for sync, buffered-1 for futures. Fan-out/collect with buffer == producer count is self-documenting and needs no justifying comment. Other sizes require a justifying comment. Prefer `sync.Mutex` for shared state, `errgroup` for coordination. Channels are the exception, not the default.

17. **slog only, no global logger in long-running services.** No `fmt.Println`, no `log.Printf`, no package-level `slog.Info/Error`. Constructors take `*slog.Logger` and bind component attributes once with `logger.With(...)`. Request code uses `InfoContext`, `ErrorContext`, or `logger.LogAttrs(ctx, ...)`. Exception: CLI tools and one-shot commands where injecting a logger through many layers is pure ceremony — `slog.Default()` is acceptable there. See [references/observability.md](references/observability.md).

18. **Zero value must be useful.** Design structs so `var x T` works. `sync.Once` for lazy init. Defaults applied in constructors/methods, not required from callers.

19. **Generics for type safety, not cleverness.** Good: typed containers, `Set[T]`, `SyncMap[K,V]`, eliminating `any` casts. Bad: one concrete type, premature abstraction. Wait for 3+ implementations.

20. **Prefer config structs, not functional options.** Few required params: plain constructor. Optional settings: config struct with zero-value defaults and validation. Exactly-one-of choices: interface field on the config struct. Stateful construction order: builder. All configuration for a service should be discoverable from a single type or location — a reviewer should be able to answer "what does this service connect to?" without grepping. See [references/design.md](references/design.md).

21. **CLIs use Kong** (for new code). Use `github.com/alecthomas/kong` for command-line parsing. Decide source ownership early: Kong handles flags and commands only; the config package handles env, files, and secrets. Do not use Kong `env:"..."` tags for application config values like `DATABASE_URL`. In existing codebases, preserve the current CLI framework.

22. **Naming.** Name length ~ distance from declaration to use. All-caps acronyms (`HTTPServer`). No `Get` prefix on getters. Package name is part of the identifier. No `util`/`common`/`helpers`.

23. **Uber style details matter.** Non-pointer mutex fields; never embed mutexes. Use comma-ok type assertions. Use `time.Time`/`time.Duration`. Prefer nil slices for empty results unless wire semantics require `[]`. Avoid built-in names and naked bools. Defer cleanup by default. Start enums at one unless zero is meaningful.

24. **Enforce with linters.** `golangci-lint` in CI and pre-commit. Config: [assets/golangci.yml](assets/golangci.yml). Rationale: [references/linting.md](references/linting.md).

## Performance

Performance guidance applies to hot paths. Pre-allocate slices/maps when size
known. `strconv` over `fmt`. Avoid repeated string-to-`[]byte` conversions:
convert once and reuse. `strings.Builder` for concat. `sync.Pool` only for
measured hot-path allocations. Pointer receivers for large structs, value for
small. Profile before optimizing (`go tool pprof`). Benchmark before claiming
faster (`testing.B`).

## Project Layout

```
cmd/server/main.go        # flags, wiring, signal handling
internal/
  domain/                  # core types, zero external deps
  store/                   # data access
  service/                 # business logic, orchestration
  transport/http/          # HTTP handlers (thin: decode → call → encode)
  transport/grpc/          # gRPC/Connect handlers
  middleware/              # auth, logging, metrics
proto/                     # protobuf definitions
migrations/                # DB migrations
Taskfile.yml               # build, test, lint, run
.golangci.yml              # linter config
```

## References

Read a reference file when the task involves its domain. Skip it for unrelated work.

| File | Covers | Read when... |
|---|---|---|
| [references/concurrency.md](references/concurrency.md) | Goroutine lifecycle, worker pools, sync vs channels, closure pitfalls, anti-patterns | Writing code that spawns goroutines, uses channels, coordinates workers, or protects shared state |
| [references/errors.md](references/errors.md) | Error types, wrapping, sentinels, custom types, boundary mapping, panic/recover | Designing error contracts, adding error handling, mapping errors at HTTP/gRPC boundaries |
| [references/design.md](references/design.md) | Packages, DI, interfaces, API design, config structs, builders, generics, defensive copies | Structuring packages, designing constructors or public APIs, choosing between config patterns |
| [references/server.md](references/server.md) | HTTP+gRPC scaffold, graceful shutdown, middleware, health checks, Connect | Building a new server, adding endpoints, wiring shutdown, or setting up middleware |
| [references/observability.md](references/observability.md) | slog, OpenTelemetry, metrics, tracing, pprof, canonical log lines | Adding logging, metrics, or tracing; diagnosing performance; setting up observability |
| [references/testing.md](references/testing.md) | goleak, property testing, integration tests, benchmarks, fakes, coverage | Writing tests for concurrent code, setting up integration infra, or benchmarking |
| [references/linting.md](references/linting.md) | golangci-lint config, linter rationale, CI setup | Configuring linters or adding a new project's CI pipeline |

## Packages

| Package | Use |
|---|---|
| [packages/safe](packages/safe) | Concurrency helpers: `safe.Go` (single goroutine in errgroup with panic recovery), `safe.Collect` (bounded best-effort fan-out/collect with per-item errors and panic recovery), and `safe.Locked[T]` (mutex-protected value with closure-based compound mutations — prevents the Get/Set logical race). Copy into a project or import when this skill is vendored as a module. |
