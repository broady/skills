# Linting

## Project Default: golangci-lint

Use `golangci-lint` as the project default linting tool. Avoid adding standalone
`go vet` invocations, separate `staticcheck` binaries, or ad-hoc shell scripts
unless an existing codebase already has a consistent equivalent. One tool, one
config, one CI step.

```sh
golangci-lint run ./...
```

Configure via `.golangci.yml` at the project root. Pin the version in CI and in
pre-commit hooks so all developers and agents produce identical results.

A ready-to-use config is at [../assets/golangci.yml](../assets/golangci.yml).

## Recommended Linters

### Correctness

These catch real bugs. All are non-negotiable.

| Linter | Why |
|---|---|
| `govet` | Catches printf format mismatches, struct tag errors, unreachable code, and more. The standard set of Go analyzers. |
| `staticcheck` | The most comprehensive single Go analyzer. Finds unused code, deprecated API usage, incorrect sync usage, impossible conditions. |
| `errcheck` | Ensures every returned error is checked. Unhandled errors are the #1 source of silent data loss. |
| `bodyclose` | Detects unclosed `http.Response.Body`. An unclosed body leaks a TCP connection and eventually exhausts the connection pool. |
| `noctx` | Flags HTTP requests made without a `context.Context`. Without context, you lose cancellation, timeouts, and tracing propagation. |
| `contextcheck` | Catches functions that discard an incoming context and call downstream work with `context.Background()` or another detached context. This breaks cancellation, deadlines, tracing, and shutdown. |
| `fatcontext` | Catches contexts created inside loops and function literals where cancellation stacks up or is deferred too late. This prevents timer leaks and accidentally long-lived child contexts. |
| `sqlclosecheck` | Catches unclosed `sql.Rows` and `sql.Stmt`. Leaked rows hold a database connection hostage. |
| `rowserrcheck` | Ensures `sql.Rows.Err()` is checked after iteration. Without this, partial result sets go undetected. |
| `nilerr` | Detects `return nil` inside an `if err != nil` block. Almost always a copy-paste bug. |
| `exhaustive` | Requires switch statements on enum-like types to cover every case. Prevents silent logic gaps when new values are added. |
| `forcetypeassert` | Flags single-value type assertions that can panic. Use comma-ok assertions. |
| `nilaway` | Inference-based nil-panic detection. Tracks nil flows across packages and through interfaces — catches conditional assignments dereferenced outside their guard and nil returns immediately dereferenced at call sites. Requires custom golangci-lint build (see [NilAway setup](#nilaway)). |

### Style

These enforce the house rules so code reviews can focus on logic, not formatting.

| Linter | Why |
|---|---|
| `gofumpt` | Stricter superset of `gofmt`. Enforces consistent grouping of declarations, removes unnecessary blank lines, and simplifies composite literals. |
| `revive` | Configurable replacement for the deprecated `golint`. Use it to enforce specific rules: no dot imports, no empty blocks, no bare returns in non-trivial functions. |
| `errname` | Enforces `ErrFoo` for sentinel errors and `FooError` for error types. Consistent naming makes errors greppable. |
| `gci` | Enforces Uber-style import ordering: standard library, then everything else. Deterministic imports eliminate diff noise. |
| `misspell` | Catches typos in comments and string literals. Typos in log messages and error strings make grep-based debugging harder. |
| `predeclared` | Flags names that shadow built-ins such as `error`, `string`, `len`, and `cap`. |

### Performance

These surface easy wins that agents often miss.

| Linter | Why |
|---|---|
| `prealloc` | Suggests `make([]T, 0, n)` when the loop bound is known. Avoids repeated slice growth allocations. |
| `goconst` | Flags string literals repeated 3+ times. Repeated strings waste memory and invite typos. Extract to a constant. |

### Security

| Linter | Why |
|---|---|
| `gosec` | Detects hardcoded credentials, weak crypto, SQL injection via string concatenation, unsafe use of `unsafe`, uncontrolled file paths, and other common vulnerabilities. |

### Architectural

| Linter | Why |
|---|---|
| `depguard` | Enforces import deny-lists and layer boundaries at lint time. Prevents architectural violations before code review. See [Architectural Boundaries](#architectural-boundaries-depguard). |

### Optional Style and Architecture Enforcement

These can be useful on greenfield code or strict subsystems, but they primarily
enforce style or architecture preferences. Keep them tiered so they do not block
adoption of the bug-catching defaults.

| Linter | Tier | Why |
|---|---|---|
| `copyloopvar` | Correctness on older Go / style on Go 1.22+ | Flags loop-variable captures. Still useful for code that must support older semantics or for making captures explicit. |
| `unconvert` | Style | Removes redundant conversions. Good cleanup signal, rarely a production bug. |
| `unparam` | Style/design | Finds unused parameters. Useful during refactors, but can fight interface-conformance and future-proofing. |
| `containedctx` | Architecture | Prevents storing `context.Context` in structs. Enable when a codebase has drifted toward context-as-state. |
| `ireturn` | Architecture | Restricts returning interfaces. Useful in strict package-boundary designs, noisy for factories and plugin systems. |
| `interfacebloat` | Architecture | Caps interface size. Useful as a review aid, but thresholds are inherently local. |
| `usestdlibvars` | Style | Replaces magic strings/status codes with stdlib constants. Helpful readability rule, not usually a bug. |

### Architectural Boundaries (depguard)

Use `depguard` to enforce import restrictions at the linter level. This prevents
architectural violations from even compiling in CI — no code review needed for
the obvious cases.

| Use case | Configuration |
|---|---|
| Force wrapper usage | Deny `encoding/json`, require internal `mypkg/json` |
| Ban deprecated packages | Deny `io/ioutil`, `golang.org/x/exp` |
| Layer separation | Deny `models` in `migrations/` — migrations must not import live model code |
| Ban unstable deps | Deny `github.com/pkg/errors` — use stdlib `errors` + `fmt.Errorf` |

```yaml
linters-settings:
  depguard:
    rules:
      main:
        deny:
          - pkg: encoding/json
            desc: use internal/json wrapper for consistent serialization
          - pkg: io/ioutil
            desc: use os or io instead (deprecated since Go 1.16)
          - pkg: github.com/pkg/errors
            desc: use stdlib errors + fmt.Errorf
      migrations:
        files:
          - '**/migrations/**/*.go'
        deny:
          - pkg: myapp/models$
            desc: migrations must not depend on live models (they change over time)
```

`depguard` catches violations at lint time, before code review. It is the
cheapest way to enforce "always use our wrapper" and "never import across this
boundary." Add it to every project alongside `forbidigo`.

### Banned Patterns

Use `forbidigo` or `revive` rules to enforce these bans in non-test code:

| Banned | Replacement | Why |
|---|---|---|
| `fmt.Print*` | `logger.LogAttrs(...)` for operational logs; injected `io.Writer` for CLI user output | Unstructured process output. Impossible to filter, route, or query in production. |
| `log.Print*` | `logger.LogAttrs(...)` | The `log` package has no structured fields and no levels. Use `slog` for production logging. |
| `log.Fatal*` outside `main` | Return an error | `log.Fatal` calls `os.Exit`. Only the entrypoint decides to terminate the process. |
| `os.Exit` outside `main` | Return an error | `os.Exit` bypasses `defer`, skips graceful shutdown, and makes non-entrypoint code untestable. Use the `run() error` pattern. |
| Dependencies from `context.Context` | Constructor injection | Context carries cancellation, deadlines, and request-scoped metadata. It is not a service locator. Ban project helpers such as `ctxutil.DB(ctx)`, `ctxutil.Store(ctx)`, and `ctxutil.Client(ctx)`. |
| `slog.Default()` outside `main` or test bootstrap | Constructor-injected `*slog.Logger` | The default logger is global state. Components receive loggers explicitly. |
| Package-level `slog.Info`, `slog.Error`, etc. | Injected `*slog.Logger` | Package-level slog calls use the global default logger and lose component/request policy. |

No general-purpose linter can reliably know whether a `ctx.Value` result is a
dependency or request metadata. Add `forbidigo` patterns for project-specific
context helper packages and review direct `ctx.Value` usage carefully.

Likewise, no stock linter can perfectly identify "inside a request" for logging.
Ban package-level slog calls with `forbidigo`; review service/handler code for
`logger.LogAttrs(ctx, ...)` with typed `slog.Attr` values rather than
context-free `Info`, `Error`, key/value `InfoContext`, or per-call
`logger.With(...)`.

### sloglint

Enforces type-safe, context-aware, structured logging with `slog`. Complements
`forbidigo` — `forbidigo` bans the wrong functions, `sloglint` enforces the
right calling convention on the functions you keep.

```yaml
linters-settings:
  sloglint:
    attr-only: true          # reject kv pairs; require typed slog.Attr
    no-global: "all"         # block slog.Info, slog.Default(), etc.
    context: "all"           # require a context on every log call
    static-msg: true         # message must be a string literal
    key-naming-case: snake   # enforce snake_case keys
```

See [observability.md](observability.md#prefer-logattrs-everywhere) for the
rationale behind `attr-only` and the typed attr constructor pattern.

### NilAway

Uber's inference-based nil-panic detector. Unlike `govet`'s `nilness` analyzer,
NilAway tracks nil flows across package boundaries, through interfaces, and into
conditional branches. It requires no annotations — it reads standard Go code.

**What it catches that `nilness` misses:**

```go
// nilness: no error. NilAway: "result may be nil"
func getUser(id string) *User {
    if id == "" {
        return nil
    }
    return &User{ID: id}
}

func handler(id string) {
    u := getUser(id)
    fmt.Println(u.Name) // potential nil panic — NilAway flags this
}
```

**Setup** — NilAway is not bundled in golangci-lint; it requires the module
plugin system (golangci-lint v2+).

1. Create `.custom-gcl.yml` at the project root:

```yaml
version: v2.1.6
plugins:
  - module: go.uber.org/nilaway/cmd/gclplugin
    import: go.uber.org/nilaway/cmd/gclplugin
    version: # pin to latest release tag
```

2. Enable in `.golangci.yml`:

```yaml
linters:
  enable:
    - nilaway

  settings:
    custom:
      nilaway:
        type: module
        settings:
          include-pkgs: "github.com/yourorg/yourrepo"  # first-party only
```

3. Build and run the custom binary:

```sh
golangci-lint custom         # builds ./custom-gcl
./custom-gcl run ./...       # run as usual
```

**`include-pkgs` is strongly recommended** — without it NilAway analyzes
dependencies, which is slow and noisy. Scope it to your module path.

**Status**: actively developed at Uber, production-tested internally. False
positives are possible; suppress with `//nolint:nilaway` and a justifying
comment. Do not disable the linter wholesale because of occasional noise.

---

### Optional dependency bans

Use `depguard` for project-specific dependency boundaries, not as a universal
ban list. For JSON, direct `encoding/json` is acceptable when handlers apply
explicit request size limits and handle decode/encode errors. If the project has
a serialization wrapper that centralizes those limits, unknown-field policy,
redaction, or alternate encoders, then ban direct `encoding/json` imports and
force callers through the wrapper.

## Linters to Disable

These are explicitly disabled because they create noise without catching real
issues:

| Linter | Why disabled |
|---|---|
| `wsl` | Nitpicks whitespace placement. Constant false positives on idiomatic Go. |
| `funlen` | Arbitrary function length limits. A 60-line table-driven test is perfectly readable. |
| `gocognit`, `cyclop` | Cognitive/cyclomatic complexity thresholds trigger on straightforward switch statements and table-driven tests. |
| `lll` | Line length limits conflict with Go's preference for descriptive names, struct tags, and long function signatures. |
| `godox` | Banning `TODO`/`FIXME` comments hides technical debt rather than surfacing it. |

## golangci-lint in CI

Run as a dedicated CI step with a pinned version:

```yaml
# GitHub Actions example
- uses: golangci/golangci-lint-action@v7
  with:
    version: v2.1.6
```

For incremental linting on pull requests, use `--new-from-rev` to lint only
changed code against the merge base:

```sh
golangci-lint run --new-from-rev=origin/main ./...
```

This keeps existing code from blocking PRs while ensuring all new code meets
the standard. Remove `--new-from-rev` once the full codebase is clean.

Set `--timeout 5m` in CI. Large module graphs and `staticcheck` can exceed the
default timeout on cold caches.
