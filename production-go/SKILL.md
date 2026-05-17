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
license: Apache-2.0
compatibility: Requires Go 1.26+, golangci-lint
metadata:
  author: cbro
  version: "0.3"
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

Labels:
- **Safety invariant** — violating this can cause incidents.
- **Project default** — use this for new code unless there is a concrete reason not to.
- **Style preference** — consistency matters more than the specific choice.

| I need to... | Do this | Label |
|---|---|---|
| Run N things concurrently (all-or-nothing) | `errgroup.WithContext` + `SetLimit` | Safety invariant |
| Run N things concurrently (best-effort collect) | `safe.Collect` — bounded, panic-safe, per-item errors; validate config-derived limits before calling | Project default |
| Run multiple subsystems in one process | `errgroup` if they share a cancel signal; `oklog/run.Group` if they need independent interrupt/cleanup | Project default |
| Run a background worker | errgroup goroutine with `select` on `ctx.Done()` | Safety invariant |
| Signal completion | `context.CancelFunc` or `chan struct{}` | Project default |
| Adapt a one-shot callback/channel API | Prefer synchronous code. If async is unavoidable, use `chan T` size 1 only with documented owner, cancellation, wait path, and context-aware producer; otherwise use `errgroup`/`safe.Go` | Safety invariant |
| Protect shared state | non-pointer `sync.Mutex` field (read-heavy: `sync.RWMutex`); for values needing atomic read-modify-write: `safe.Locked[T]` | Safety invariant |
| Lazy-initialize | `sync.OnceValue`; `sync.Once` for side-effect-only init | Project default |
| Store a counter/flag | `go.uber.org/atomic` | Style preference |
| Pass request metadata | `context.WithValue` | Project default |
| Pass a dependency | Constructor parameter | Safety invariant |
| Define a dependency boundary | Interface at the consumer site | Project default |
| Configure optional settings | Config struct with zero-value defaults | Project default |
| Configure loaded/validated settings | Config struct + `Validate() error` | Safety invariant |
| Choose at most one of N; require one | Interface field on config struct for "at most one"; `Validate()` enforces that one is required | Project default |
| Enforce construction order | Builder with type-state | Project default |
| Handle an error | Add operation context and return. Use `%w` only when exposing the cause is part of the stable error contract; otherwise convert to a domain error or use `%v` at public boundaries | Safety invariant |
| Report errors to callers | Map domain errors → HTTP/gRPC status at boundary via error map | Safety invariant |
| Process async work durably | External broker (SQS, Pub/Sub, NATS) with ack/nack; in-process queue only for single-binary deploys | Safety invariant |
| Run DB operations atomically | `WithTx(ctx, db, fn)` — fn receives `*sql.Tx`, pass it explicitly to store methods via `Querier` interface | Safety invariant |
| Serve HTTP | `http.Server{}` with explicit `ReadHeaderTimeout`, `ReadTimeout`, `WriteTimeout`, `IdleTimeout` | Safety invariant |
| Decode/encode JSON | Use stdlib `encoding/json` with explicit size limits and error handling; optionally ban direct `encoding/json` when the project has a serialization wrapper | Safety invariant |
| Make outgoing HTTP requests | Custom `http.Client{}` with `Timeout` set. Never `http.DefaultClient` or `http.Get()` in production | Safety invariant |
| Connect to a database | Explicit pool config: `MaxConns`, `MaxConnLifetime`, `MaxConnIdleTime` | Safety invariant |
| Represent a domain identifier | `type FooID string` / `type FooID int64` — strong type, not raw primitive | Safety invariant |
| Log | `*slog.Logger` via constructor | Project default |
| Instrument | Endpoint metrics by default; OpenTelemetry when multi-backend export, cross-service tracing, or org policy justifies it | Project default |
| Build a CLI | `github.com/alecthomas/kong` for flags; config package for env/files/secrets | Project default |
| Discover all service config | Single config struct or `loadConfig()` — a reviewer answers "what does this connect to?" from one location | Safety invariant |
| Ban dangerous imports | `depguard` in golangci-lint for project-specific dependency boundaries; optionally ban direct `encoding/json` when the project has a serialization wrapper | Project default |

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
- Generated files are exempt; do not modify them. Hand-written adapters and wrappers around generated code must follow Tier 1 safety rules.

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

## Project Defaults For New Code

For new production services, use generated or contract-first HTTP APIs when an
OpenAPI spec exists or is expected. Keep transport handlers thin: decode,
validate, call one application service method, map domain errors, encode. Use
Connect for RPC when possible, Kong for CLIs, and `slog` for logging. Instrument
endpoints with metrics by default. Use OpenTelemetry when multi-backend export,
cross-service tracing, or org policy justifies it; prefer metrics-only until
tracing has a concrete operational need. Use `golangci-lint` for static checks,
`errgroup` for bounded all-or-nothing task groups, `safe.Collect` for bounded
best-effort fan-out/collect, `oklog/run.Group` for multi-subsystem lifecycle
management, and `goleak` for packages that start goroutines. These are project
defaults, not universal Go law. Preserve existing equivalent choices unless a
migration has a concrete operational reason.

## Rules — Tier 1: Safety Invariants

These prevent production incidents. Apply them unconditionally to all
hand-written code: new code, existing code, adapters and wrappers around
generated code, and reviews. Generated files are exempt; do not modify them.

1. **No mutable globals, avoid `init()`**. Package-level `var` only for sentinel errors, compile-time interface checks, and values that are immutable by construction (embed.FS, compiled regexps, `strings.NewReplacer(...)`, `cases.Fold()`, sync.Pool with deterministic New, `sync.OnceValue` lazy singletons). Package-level slices, maps, pointers, and structs with mutable fields are mutable even when treated as read-only; build them in constructors or return defensive copies. Avoid `init()`; if unavoidable, it must be deterministic, avoid I/O/env/global mutation/goroutines, and not depend on init ordering. Acceptable `init()` uses: registering a value into an in-process registry (codec registration, `database/sql` drivers via blank imports) — the registration itself must not perform I/O or start goroutines. In `*_test.go`, package-level mutable variables and `init()` are allowed only for standard test flag registration or test fixtures that cannot be expressed locally. Production code still avoids them. Everything else flows through constructors. See [references/design.md](references/design.md).

2. **Errors: propagate with context, handle once at the boundary.** Interior code adds operation context and returns. Use `%w` only when exposing the cause is an intentional, stable contract; otherwise use `%v` or map to a domain error at the package boundary. Boundaries log, count, retry, or map errors to user-facing responses. Never log and return the same error. Never swallow errors silently — if intentionally discarding, annotate with `//nolint:errcheck`. See [references/errors.md](references/errors.md).

3. **No naked goroutines.** A goroutine's maximum lifetime must be bounded by the scope that owns and waits for it. Start goroutines via `errgroup`, `run.Group`, `safe.Go`, `safe.Collect`, or an explicit owner that can cancel and wait. Looping or blocking goroutines select on `ctx.Done()`. Raw `go` requires documented owner, stop path, wait path, and reason. See [references/concurrency.md](references/concurrency.md).

4. **Bounded concurrency.** `errgroup.SetLimit(n)` or `semaphore.Weighted`. Never spawn unbounded goroutines in a loop.

5. **Graceful shutdown is mandatory and phased.** SIGTERM/SIGINT triggers a multi-phase sequence: (1) **Drain** — stop accepting new work, wait for in-flight requests; (2) **Hammer** — force-cancel anything still running after a configurable deadline (e.g., 30s); (3) **Terminate** — close dependencies, flush telemetry, exit. Use `oklog/run.Group` for multi-subsystem coordination when subsystems need independent interrupt/cleanup. Use `errgroup` when subsystems share a cancel signal. The hammer phase prevents the real production bug: a graceful period that hangs forever because one connection never drains. See [references/server.md](references/server.md).

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

12. **Context is not a service locator.** `context.Context` is the first parameter and is never stored in a struct. Use it for cancellation, deadlines, and request-scoped values that genuinely flow with the request (trace ID, auth principal, request ID). Context-carried log fields are allowed only for request-scoped diagnostic metadata. Dependencies go through constructors; never use context to pass services, clients, loggers, configs, repositories, or helpers like `ctxutil.DB(ctx)`.

13. **No panic, no recover in application code.** Return errors. Library code may panic only for true invariant violations where continuing is unsafe. Dev/test-only invariant checks may panic if gated out of production and used to catch programmer misuse. Production runtime failures still return errors. `recover` is allowed in exactly two places: goroutine supervisors (`safe.Go`, `safe.Collect`) and package-internal entry points (recursive parsers/walkers where panic is a structured longjmp that never escapes the package). In both cases, the panic is converted to a returned error — never swallowed. Do not add HTTP/gRPC panic recovery middleware. See [references/errors.md](references/errors.md).

## Rules — Tier 2: Project Defaults And Style Preferences

These improve maintainability and readability. Apply project defaults to new
code unless there is a concrete reason not to. Style preferences should follow
the local codebase. In existing codebases, apply these only when already
modifying the relevant code or during planned rewrites. Do not churn stable code
purely for style compliance.

14. **Project default — constructor injection.** The wiring in `main()` IS the dependency graph. Manual wiring scales: organize large graphs into small `newX(...)` helpers, not service locators or DI containers. Constructor injection is a means to testability; testability is a means to correctness. Prioritize the end (correctness, observable behavior) over the means (DI purity). This is intentionally stricter than many Go guides because hidden dependencies are especially costly in generated service code. See [references/design.md](references/design.md).

15. **Project default — interfaces at the consumer, not the producer.** 1-2 methods. Accept interfaces, return structs. Define an interface only when the consumer needs a seam: a test double, a second implementation, or a plugin boundary. Do not define provider-side interfaces preemptively. Concrete types are fine when you control both sides. `var _ I = (*T)(nil)` for compile-time checks. See [references/design.md](references/design.md).

16. **Safety invariant — channels are size 0 or 1 by default.** Unbuffered for synchronous handoff. Buffered-1 channels are acceptable for one-shot handoff only when the goroutine has a documented owner, stop path, wait path, and context-aware producer; they are not goroutine lifecycle managers. Any buffer greater than 1 requires a comment explaining the bound, why a channel is the right primitive, what prevents unbounded growth, and what happens if producers outpace consumers. `make(chan Result, len(items))` is allowed only for finite fan-in where `len(items)` is explicitly bounded, each producer sends at most once, and concurrency is limited separately. Prefer `errgroup.SetLimit`, `safe.Collect`, a preallocated result slice, or a mutex-protected collection for ordinary fan-out/fan-in.

17. **Project default — slog only, no global logger in services.** No `fmt.Println`, no `log.Printf`, no package-level `slog.Info/Error`, and no `slog.Default()` outside `main()` or test bootstrap. Constructors take `*slog.Logger` and bind component attributes once with `logger.With(...)`. Request logs use `logger.LogAttrs(ctx, ...)` with typed `slog.Attr` values. Startup/shutdown logs use `logger.LogAttrs(context.Background(), ...)` when no request context exists. CLI tools write user-facing output to an injected `io.Writer`; use `slog.Default()` only in `main()` or test bootstrap. See [references/observability.md](references/observability.md).

18. **Project default — zero value should be useful.** Prefer useful zero values for value types and optional configuration objects. Service types with required dependencies may require constructors. `sync.OnceValue` for lazy-initialized package-level values; `sync.Once` for side-effect-only initialization. Defaults applied in constructors/methods, not required from callers.

19. **Style preference — generics for type safety, not cleverness.** Good: typed containers, `Set[T]`, `SyncMap[K,V]`, eliminating `any` casts. Bad: one concrete type, premature abstraction. Wait for 3+ implementations.

20. **Project default — prefer config structs, not functional options.** Few required params: plain constructor. Optional settings: config struct with zero-value defaults and validation. At-most-one choices: interface field on the config struct; `Validate()` enforces required-one. Stateful construction order: builder. All configuration for a service should be discoverable from a single type or location — a reviewer should be able to answer "what does this service connect to?" without grepping. This intentionally diverges from Uber's public-API guidance in favor of inspectable service configuration. For production services, prefer explicit config structs because they are inspectable, serializable, and validate cleanly. For reusable public Go libraries, Uber-style value options are acceptable, especially when the API has many optional parameters and does not model loaded runtime config; closure-style `WithX` options still require explicit justification. See [references/design.md](references/design.md).

21. **Project default — CLIs use Kong** (for new code). Use `github.com/alecthomas/kong` for command-line parsing. Decide source ownership early: Kong handles flags and commands only; the config package handles env, files, and secrets. Do not use Kong `env:"..."` tags for application config values like `DATABASE_URL`. In existing codebases, preserve the current CLI framework.

22. **Style preference — naming.** Name length ~ distance from declaration to use. All-caps acronyms (`HTTPServer`). No `Get` prefix on getters. Package name is part of the identifier. No `util`/`common`/`helpers`.

23. **Style preference — Uber style details matter.** Non-pointer mutex fields; never embed mutexes. Use comma-ok type assertions. Use `time.Time`/`time.Duration`. Prefer nil slices for empty results unless wire semantics require `[]`. Avoid built-in names and naked bools. Defer cleanup by default. Start enums at one unless zero is meaningful.

24. **Project default — enforce with linters.** `golangci-lint` in CI and pre-commit. Config: [assets/golangci.yml](assets/golangci.yml). Rationale: [references/linting.md](references/linting.md).

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
| [references/database.md](references/database.md) | Explicit transactions, Querier interface, cursor iteration, async work (brokers + retry), invariant checks | Writing database access code, managing transactions, processing async work, adding dev-time safety checks |
| [references/observability.md](references/observability.md) | slog, OpenTelemetry, metrics, tracing, pprof, canonical log lines | Adding logging, metrics, or tracing; diagnosing performance; setting up observability |
| [references/testing.md](references/testing.md) | goleak, property testing, integration tests, benchmarks, fakes, coverage | Writing tests for concurrent code, setting up integration infra, or benchmarking |
| [references/linting.md](references/linting.md) | golangci-lint config, linter rationale, CI setup | Configuring linters or adding a new project's CI pipeline |

## Packages

| Package | Use |
|---|---|
| [packages/safe](packages/safe) | Concurrency helpers: `safe.Go` (single goroutine in errgroup with panic recovery), `safe.Collect` (bounded best-effort fan-out/collect with per-item errors and panic recovery), and `safe.Locked[T]` (mutex-protected value with closure-based compound mutations — prevents the Get/Set logical race). Copy into a project or import when this skill is vendored as a module. |
