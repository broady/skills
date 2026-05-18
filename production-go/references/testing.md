# Testing — Production Concerns

This file covers assertions, leak detection, property testing, integration
infrastructure, benchmarking, race detection, fakes, and coverage.

## Contents

1. [Assertions](#1-assertions) — assert/v2, go-cmp, error contracts
2. [Goroutine Leak Detection (goleak)](#2-goroutine-leak-detection-goleak) — TestMain, per-test, filtering
3. [Property-Based Testing](#3-property-based-testing) — round-trip, idempotency, rapid
4. [Test Placement](#4-test-placement) — white-box vs black-box, integration test location
5. [Integration Test Infrastructure](#5-integration-test-infrastructure) — build tags, custom flags, testcontainers, fixtures
6. [Benchmarking Discipline](#6-benchmarking-discipline) — b.Loop, sink, benchstat
7. [Race Detection](#7-race-detection) — CI rule, flaky tests under -race
8. [Test Helpers and Fakes](#8-test-helpers-and-fakes) — fakes over mocks, t.Helper, t.Cleanup, t.Context
9. [Coverage Pragmatics](#9-coverage-pragmatics) — floor/ceiling, branch vs line, error paths
10. [Dev-Only Runtime Invariant Checks](#10-dev-only-runtime-invariant-checks) — runtime safety checks, environment-gated panics

---

## 1. Assertions

Use `github.com/alecthomas/assert/v2` for routine assertions in new tests. It
has a small API surface, fails immediately, uses generics for type safety, and
prints useful diffs for equality checks.

Use stdlib `testing` for test structure, helpers, subtests, cleanup, contexts,
and custom control flow. Use `github.com/google/go-cmp/cmp` directly when a
comparison needs custom options.

```go
func TestOrderServiceCreate(t *testing.T) {
	ctx := t.Context()
	got, err := svc.Create(ctx, req)
	assert.NoError(t, err)
	assert.NotZero(t, got.ID)
	assert.Equal(t, StatusActive, got.Status)
}
```

For comparisons that need options, call `cmp.Diff` directly:

```go
if diff := cmp.Diff(want, got, cmpopts.IgnoreFields(Order{}, "CreatedAt")); diff != "" {
	t.Fatalf("Create() mismatch (-want +got):\n%s", diff)
}
```

For error contracts:

```go
err := svc.Delete(ctx, missingID)
assert.IsError(t, err, domain.ErrNotFound)
```

Use `errors.As` manually when you need to inspect fields on a custom error type.

---

## 2. Goroutine Leak Detection (goleak)

Project default: packages that spawn goroutines use `go.uber.org/goleak` or an
existing equivalent leak check. Leaking goroutines cause resource exhaustion,
flaky tests, and production OOMs.

### Package-wide: TestMain

Preferred. Catches leaks from any test in the package.

```go
func TestMain(m *testing.M) {
    goleak.VerifyTestMain(m)
}
```

### Per-test

Use when you need focused verification or the package already has a TestMain.

```go
func TestWorkerPool(t *testing.T) {
    defer goleak.VerifyNone(t)

    pool := NewPool(4)
    pool.Submit(func() { /* work */ })
    pool.Shutdown()
}
```

### Filtering background goroutines

Some libraries spawn long-lived goroutines. Filter explicitly, never suppress goleak.

```go
func TestMain(m *testing.M) {
    goleak.VerifyTestMain(m,
        goleak.IgnoreTopFunction("go.opencensus.io/stats/view.(*worker).start"),
        goleak.IgnoreTopFunction("database/sql.(*DB).connectionOpener"),
    )
}
```

For deterministic time testing (`synctest`), per-test leak verification with
errgroup, and the relationship between goleak and structured concurrency, see
[concurrency-patterns.md](concurrency-patterns.md#5-leak-detection-with-goleak).

---

## 3. Property-Based Testing

Use for parsers, serializers, validators, and any function that must hold invariants
across all inputs. Table-driven tests prove specific cases; property tests prove
universal properties.

### Properties worth testing

- **Round-trip**: `Unmarshal(Marshal(x)) == x`
- **Idempotency**: `f(f(x)) == f(x)`
- **Invariant preservation**: `Validate(Transform(validInput))` never errors
- **Commutativity**: `Merge(a, b) == Merge(b, a)`

### rapid — expressive property tests

`pgregory.net/rapid` provides typed generators and shrinking. Use rapid for
invariant assertions; use `testing.F` for "does this crash" questions.

```go
import "pgregory.net/rapid"

func TestJSONRoundTrip(t *testing.T) {
    rapid.Check(t, func(t *rapid.T) {
        original := User{
            Name:  rapid.String().Draw(t, "name"),
            Email: rapid.StringMatching(`[a-z]+@[a-z]+\.[a-z]{2,4}`).Draw(t, "email"),
            Age:   rapid.IntRange(0, 150).Draw(t, "age"),
        }
        data, err := json.Marshal(original)
        if err != nil {
            t.Fatal(err)
        }
        var decoded User
        if err := json.Unmarshal(data, &decoded); err != nil {
            t.Fatal(err)
        }
        if original != decoded {
            t.Fatalf("round-trip failed: %+v != %+v", original, decoded)
        }
    })
}
```

---

## 4. Test Placement

### White-box vs black-box

| Package declaration | Access | Use for |
|---|---|---|
| `package mypkg` | Unexported + exported | Unit tests of internal logic (the default) |
| `package mypkg_test` | Exported only | Validating the public API contract |

Use `_test` suffix when you want to prove the API is usable without internal
knowledge — catches coupling to unexported details during refactors. The stdlib
uses this for `net/http`, `context`, `errors`.

Both live in the same directory. A directory may contain exactly two test
packages: `mypkg` and `mypkg_test`.

### Integration test location

Single-package integration tests use a build tag in the same directory:

```go
//go:build integration

package store_test
```

Cross-package integration or end-to-end tests live in a top-level directory
(`integration/`, `e2e/`) with their own package name, importing the packages
under test explicitly. Use a top-level directory only when testing multiple
packages together.

---

## 5. Integration Test Infrastructure

### Build tags and env gates

```go
//go:build integration

package store_test

func TestPostgresStore(t *testing.T) {
    dsn := os.Getenv("TEST_DB_URL")
    if dsn == "" {
        t.Skip("TEST_DB_URL not set")
    }
    // ...
}
```

Run with `go test -tags=integration -count=1 ./...`. Always `-count=1` -- caching
hides flakes in integration tests.

### Custom flags for test categories

Build tags work for the canonical `integration` gate, but additional categories
(snapshot, slow, external) are better served by custom flags. Flags are typed,
self-documenting via `-h`, and discoverable without grepping source.

In `*_test.go`, custom test flags are a narrow exception to the production
"no mutable globals, avoid `init()`" rule. Register flags in `init()` to keep
declaration next to the tests that use it:

```go
package render_test

import (
    "flag"
    "testing"
)

var snapshot bool // test flag state; allowed only in _test.go

func init() {
    flag.BoolVar(&snapshot, "custom.snapshot", false, "run snapshot tests")
}

func TestRenderOutput(t *testing.T) {
    if !snapshot {
        t.Skip("pass -custom.snapshot to run this test")
    }
    // compare against golden snapshot ...
}
```

Run with `go test -v -custom.snapshot ./...`.

Use a `custom.` (or project-specific) prefix for grepability and to distinguish
from built-in test flags. Discover all registered flags with:

```bash
go test -v -args -h   # lists custom flags alongside built-ins
```

Prefer `init()` over parsing in `TestMain` for flag registration only —
packages that already use `TestMain` for goleak or container setup don't need
to coordinate `flag.Parse()` calls (`go test` parses flags before `TestMain`
runs). Do not use this exception for production initialization, hidden
dependencies, or test state that can be local to a test.

### testcontainers-go

Spin up real dependencies. No mocks for infrastructure you own.

```go
//go:build integration

func TestOrderRepository(t *testing.T) {
    ctx := t.Context()
    pg, err := postgres.Run(ctx, "postgres:16-alpine",
        postgres.WithDatabase("testdb"),
        postgres.WithUsername("test"),
        postgres.WithPassword("test"),
        testcontainers.WithWaitStrategy(
            wait.ForLog("database system is ready").
                WithOccurrence(2).WithStartupTimeout(10*time.Second)),
    )
    if err != nil {
        t.Fatal(err)
    }
    t.Cleanup(func() {
        ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
        defer cancel()
        assert.NoError(t, pg.Terminate(ctx))
    })

    connStr, err := pg.ConnectionString(ctx, "sslmode=disable")
    assert.NoError(t, err)

    repo := NewPostgresOrderRepo(connStr)
    // test against real Postgres
}
```

### Test fixtures

Use `t.TempDir()` for writable temp dirs (auto-cleaned) and `testdata/` for static
fixtures (committed, ignored by `go build`).

---

## 6. Benchmarking Discipline

### Basics

```go
var sink any // package-level sink prevents dead-code elimination

func BenchmarkMarshalUser(b *testing.B) {
    u := User{Name: "Alice", Email: "alice@example.com", Age: 30}
    b.ResetTimer()
    b.ReportAllocs()
    for b.Loop() {
        data, err := json.Marshal(u)
        if err != nil {
            b.Fatal(err)
        }
        sink = data
    }
}
```

Without `sink`, the compiler may optimize away the loop body. `b.Loop()` is the
Go 1.24+ form -- always use it over `for i := 0; i < b.N; i++`.

### Compare with benchstat

Never eyeball benchmark output. Use `benchstat` for statistical comparison.

```bash
go test -bench=. -count=10 ./... > old.txt
# make changes
go test -bench=. -count=10 ./... > new.txt
benchstat old.txt new.txt
```

Only claim improvements when benchstat shows p < 0.05. Profile with
`-cpuprofile=cpu.prof -memprofile=mem.prof`, analyze via `go tool pprof`.

---

## 7. Race Detection

### Non-negotiable CI rule

```bash
go test -race ./...
```

Every CI pipeline runs `-race`. No exceptions.

- If a test is flaky only under `-race`, **that is a real bug**. Fix the race.
- Overhead is 5-10x CPU and memory. Acceptable in CI, never in production.
- Tune `-parallel` if `-race` causes CI timeouts.

---

## 8. Test Helpers and Fakes

### Fakes over mocks

Fakes implement the interface with real in-memory behavior. Mocks record calls and
assert on them. Fakes test behavior; mocks test implementation details.

```go
// The interface (defined by the consumer).
type OrderStore interface {
    Create(ctx context.Context, order *Order) error
    Get(ctx context.Context, id string) (*Order, error)
}

// Fake — real behavior, in-memory storage. Thread-safe.
type FakeOrderStore struct {
    mu     sync.Mutex
    orders map[string]Order
}

func NewFakeOrderStore() *FakeOrderStore {
    return &FakeOrderStore{orders: make(map[string]Order)}
}

func (f *FakeOrderStore) Create(_ context.Context, order *Order) error {
    f.mu.Lock()
    defer f.mu.Unlock()
    if _, exists := f.orders[order.ID]; exists {
        return fmt.Errorf("order %s already exists", order.ID)
    }
    f.orders[order.ID] = *order
    return nil
}

func (f *FakeOrderStore) Get(_ context.Context, id string) (*Order, error) {
    f.mu.Lock()
    defer f.mu.Unlock()
    o, ok := f.orders[id]
    if !ok {
        return nil, ErrNotFound
    }
    return &o, nil
}
```

### Test helpers

Helpers take `*testing.T`, call `t.Helper()`, and never return errors. They fail
the test directly, producing failure output that points at the caller.

```go
func setupTestServer(t *testing.T, store OrderStore) *httptest.Server {
    t.Helper()
    srv := httptest.NewServer(NewHandler(store))
    t.Cleanup(srv.Close)
    return srv
}
```

### t.Cleanup over defer

Prefer `t.Cleanup` for teardown that belongs to the test or subtest lifecycle,
especially from helpers, because it composes cleanly and runs at the right
scope. Defers still run after `t.Fatal` in the same goroutine, but they are tied
to the lexical function, not the test resource lifecycle.

### t.Context() (Go 1.24+)

Returns a context canceled when the test ends. Use it instead of manual
`context.WithCancel` boilerplate: `ctx := t.Context()`.

### Over-parameterize for testability

If a struct is hard to test, it needs more parameters, not fewer. Inject time,
ID generation, and external calls as `func` fields with production defaults:

```go
type Server struct {
    store  OrderStore
    clock  func() time.Time  // default: time.Now
    idFunc func() string     // default: uuid.NewString
}
```

In tests: `clock: func() time.Time { return fixedTime }` for determinism.

---

## 9. Coverage Pragmatics

- **Floor**: ~50% package coverage. Below that, you are missing important paths.
- **Ceiling**: 100% is counterproductive -- incentivizes testing trivial getters
  and writing brittle tests coupled to implementation.
- **Branch > line**: a function with three error returns and one happy path is 25%
  branch-covered by a single happy-path test.
- **Error paths are where bugs hide**. Prioritize error returns, edge cases, and
  recovery logic over sunny-day paths.
- `go test -coverprofile=c.out ./... && go tool cover -func=c.out` to find gaps.

---

## 10. Dev-Only Runtime Invariant Checks

For subtle correctness bugs that static analysis can't catch (session reuse in
iterators, ordering violations, data corruption from wrong ID types), add runtime
invariant checks that run only in dev/test mode.

See [database/invariant-checks.md](database/invariant-checks.md) for the full
pattern and examples. The key properties:

- Gated behind `!setting.IsProd` or a build tag — zero cost in production.
- Panic with a descriptive message — these are programmer errors.
- Help catch bugs that would otherwise surface as silent data corruption.
- Remove once the invariant can be enforced at compile time.
