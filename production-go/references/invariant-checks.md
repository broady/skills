# Dev-Only Invariant Checks

For subtle correctness bugs that are hard to catch at compile time, add runtime
invariant checks that only run in development or testing mode. These should panic
with a clear message — the goal is to fail loudly during development, not silently
in production.

Gate these checks behind a build tag (`//go:build !prod`), an environment
variable (`os.Getenv("ENV") != "production"`), or a config flag set once at
startup. The mechanism doesn't matter — what matters is zero cost in production.

## Pattern

```go
// devmode.go
var devMode = sync.OnceValue(func() bool {
    return os.Getenv("APP_ENV") != "production"
})

func checkInvariant(condition bool, msg string) {
    if !devMode() {
        return
    }
    if !condition {
        panic(fmt.Sprintf("invariant violation: %s", msg))
    }
}
```

## Examples

**Detect use-after-close:**

```go
func (p *Pool) Acquire(ctx context.Context) (*Conn, error) {
    if devMode() && p.closed.Load() {
        panic("invariant violation: Acquire called after Close")
    }
    // ...
}
```

**Detect out-of-order initialization:**

```go
func (s *Server) Serve() error {
    if devMode() && s.handler == nil {
        panic("invariant violation: Serve called before RegisterHandler")
    }
    // ...
}
```

**Detect wrong-ID-type at boundary** (catch before it becomes data corruption):

```go
func (s *Store) GetMembership(ctx context.Context, q Querier, id MembershipID) (*Membership, error) {
    if devMode() && id == "" {
        panic("invariant violation: empty MembershipID passed to GetMembership")
    }
    // ...
}
```

## When to use

- Detecting misuse of shared resources (pool after close, conn after release)
- Verifying ordering constraints (Init before Use, Register before Serve)
- Catching empty/zero domain IDs at store boundaries
- Checking data invariants that are expensive to verify on every call

## Rules

- Gate behind a build tag, env var, or startup config flag — never pay the cost in production.
- Panic, don't log — these represent programmer errors, not runtime conditions.
- Include enough context in the panic message to identify the call site.
- Remove or convert to proper validation once the invariant can be enforced at
  compile time or through the type system.
