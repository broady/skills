# Errors

## Contents

- [The One Rule: Handle Once](#the-one-rule-handle-once) — add context and return interior, log at boundary
- [Return Signature Design](#return-signature-design-one-source-of-truth) — error as single source of truth, no redundant bools
- [Wrapping Strategy](#wrapping-strategy) — %w vs %v, terse prefixes, double-wrap avoidance
- [Structured Domain Errors](#structured-domain-errors) — Kind, Op, Resource pattern
- [Error Types Decision Matrix](#error-types-decision-matrix) — when to use sentinels vs custom types
- [Sentinel Errors](#sentinel-errors) — naming, matching with errors.Is
- [Custom Error Types](#custom-error-types) — exported fields, Unwrap, errors.As
- [Boundary Error Mapping](#boundary-error-mapping) — HTTP handler and gRPC interceptor patterns
- [Error Classification for Retry and Routing](#error-classification-for-retry-and-routing) — permanent/retryable, SafeToRetry, OperationPossiblySucceeded
- [Multi-Error Patterns](#multi-error-patterns) — errors.Join, validation, cleanup
- [Testing Errors](#testing-errors) — assert specific errors, table-driven patterns
- [Panic and Recover](#panic-and-recover) — when acceptable, approved recover sites

## The One Rule: Handle Once

An error is handled exactly once. Handling means making an operational decision:
logging it, returning a user-facing response, incrementing an error metric, or
triggering a retry. Interior code does not handle errors; it adds context and
propagates them.

Long-running loops may log and continue after a recoverable per-iteration
failure. That is handling the error, so do not also return the same error to
the caller.

```go
// BAD -- handled twice (logged AND returned)
func (s *OrderService) Create(ctx context.Context, o *Order) error {
    if err := s.store.Insert(ctx, o); err != nil {
        s.logger.LogAttrs(ctx, slog.LevelError, "insert order", slog.Any("err", err)) // handle #1
        return fmt.Errorf("insert order: %w", err)       // handle #2
    }
    return nil
}

// GOOD -- add operation context and return; let the boundary handle it
func (s *OrderService) Create(ctx context.Context, o *Order) error {
    if err := s.store.Insert(ctx, o); err != nil {
        return fmt.Errorf("insert order: %v", err) // hide store details at service boundary
    }
    return nil
}
```

The boundary (HTTP handler, gRPC interceptor, CLI main) is where errors get
logged and mapped to user-facing responses. Everywhere else, add operation
context and return; use `%w` only when exposing the cause is an intentional
contract.

Never ignore errors silently. If you intentionally discard one, annotate it:

```go
_ = conn.Close() //nolint:errcheck // best-effort cleanup on shutdown path
```

## Return Signature Design: One Source of Truth

`error` is the single source of truth for success/failure. Do not encode
failure state redundantly in other return values.

A `(bool, error)` return creates four states — only two are meaningful:

| bool | error | Meaning |
|---|---|---|
| true | nil | Success — unambiguous |
| false | non-nil | Failure — unambiguous |
| false | nil | **Bug**: swallowed failure, caller thinks "no error" |
| true | non-nil | **Bug**: contradicts itself |

The caller cannot know which combinations a function actually produces
without reading its implementation. Eliminate the ambiguity at the source.

```go
// BAD — bool duplicates what error already communicates
func validate(input string) (bool, error) {
    if input == "" {
        return false, nil // swallowed failure: caller sees "ok, no error"
    }
    if err := checkUpstream(); err != nil {
        return false, err
    }
    return true, nil
}

// GOOD — error alone encodes all failure modes
func validate(input string) error {
    if input == "" {
        return fmt.Errorf("validate: %w", ErrEmpty)
    }
    return checkUpstream()
}
```

**The rule:** first return value is a data payload, not a status flag. Use
sentinel errors or custom types to distinguish failure *kinds* within the
error itself. If success produces no data, return only `error`.

This applies equally to `(int, error)` where the int is a status code, or
`(T, bool, error)` patterns. If the bool/int duplicates information that
the error already carries, remove it.

## Wrapping Strategy

Always wrap with the operation as prefix. Terse. No "failed to", no "error
while", no "unable to". The error chain already implies failure.

```go
// BAD
return fmt.Errorf("failed to fetch user from database: %w", err)

// GOOD
return fmt.Errorf("fetch user: %w", err)

// GOOD -- include dynamic context when useful for debugging
return fmt.Errorf("fetch user %q: %w", id, err)
```

### `%w` vs `%v` is an API design choice

This is not a stylistic preference — it's a contract decision. **No linter
can tell you which to use** because it depends on what you're promising
callers about your error surface.

**`%w` makes the wrapped error part of your public API.** Callers can (and
will) write `errors.Is(err, ErrConnectionRefused)` against your internal
Redis error. Now you can't swap Redis for Memcached without breaking them.

**`%v` hides the cause.** Callers can't match or inspect it. Your
implementation details stay private. But callers also can't distinguish
recoverable from fatal, so you need to provide your own sentinel errors or
types for anything they need to branch on.

The rule:

At package and service boundaries, convert implementation errors into stable
domain errors before exposing them. Preserve enough cause for diagnostics, but
do not expose infrastructure details as the caller's matching contract.

| Layer | Verb | Reason |
|---|---|---|
| Within a package (internal) | `%w` | Implementation details aren't leaking — same package |
| Between packages you own | `%w` | If the upstream error is part of YOUR domain model |
| Between packages you own | `%v` | If the upstream error is an implementation detail (SQL, HTTP client, etc.) |
| At a public API boundary | `%v` + your own sentinel/type | Hide internals, expose a stable error contract |

```go
// Internal service layer -- preserve the chain (same trust boundary)
return fmt.Errorf("charge payment: %w", err)

// Public API adapter -- break the chain, expose your own errors
if errors.Is(err, sql.ErrNoRows) {
    return fmt.Errorf("get user: %w", ErrNotFound)  // YOUR sentinel, not sql's
}
return fmt.Errorf("get user: %v", err)  // hide internals from callers
```

**Ask yourself**: "If a caller writes `errors.Is(err, X)` against this, am
I comfortable maintaining that contract forever?" If not, use `%v` and
provide your own error types.

Never double-wrap. If the callee already provides sufficient context, do not
add more:

```go
// BAD -- "fetch user: fetch user: sql: no rows"
user, err := s.store.GetUser(ctx, id) // already wraps as "fetch user: ..."
if err != nil {
    return fmt.Errorf("fetch user: %w", err)
}

// GOOD -- add the NEXT layer of context
if err != nil {
    return fmt.Errorf("create order: %w", err)
}
```

## Structured Domain Errors

For systems with stable domain failure modes, prefer structured error contracts
over string conventions. Inspired by Upspin's error model: errors should carry
the operation, a stable kind, relevant domain identifiers, and the underlying
cause. The resulting chain is an operational trace through the system, not a
stack trace.

Use this idea selectively for domain boundaries. Do not replace stdlib
`errors`, do not introduce variadic runtime-typed constructors, and do not make
formatting strings the API.

```go
type Kind string

const (
	KindInvalid    Kind = "invalid"
	KindNotFound   Kind = "not_found"
	KindConflict   Kind = "conflict"
	KindPermission Kind = "permission"
	KindTransient  Kind = "transient"
	KindInternal   Kind = "internal"
)

type DomainError struct {
	Op       string
	Kind     Kind
	Resource string
	ID       string
	Err      error
}

func (e *DomainError) Error() string { /* format concise operational trace */ }
func (e *DomainError) Unwrap() error { return e.Err }
```

## Error Types Decision Matrix

| Need to match? | Dynamic data? | Approach | Example |
|---|---|---|---|
| No | No | `errors.New` | `errors.New("connection reset")` |
| No | Yes | `fmt.Errorf` | `fmt.Errorf("column %q missing", name)` |
| Yes | No | Sentinel | `var ErrNotFound = errors.New("not found")` |
| Yes | Yes | Custom type | `&ValidationError{Field: "email", ...}` |

## Sentinel Errors

Package-level immutable values. Use when callers need to branch on a specific
condition but no dynamic data is needed.

```go
// Exported -- part of the package's API contract.
var (
    ErrNotFound   = errors.New("user: not found")
    ErrConflict   = errors.New("user: conflict")
)

// Unexported -- internal branching only.
var errPoolExhausted = errors.New("pool exhausted")
```

Rules:
- Naming: exported `ErrFoo`, unexported `errFoo`.
- Prefix the message with the package/domain noun for grep-ability.
- Document what each sentinel means and when it is returned.
- Check with `errors.Is`, never `==`:

```go
// BAD
if err == ErrNotFound {

// GOOD
if errors.Is(err, ErrNotFound) {
```

Never use sentinels when the caller needs structured data about the failure.
That is what custom types are for.

## Custom Error Types

Use when callers need to match AND extract structured information.

```go
// ValidationError reports one or more invalid fields.
type ValidationError struct {
    Field   string
    Message string
    Value   any
}

func (e *ValidationError) Error() string {
    return fmt.Sprintf("validation: %s: %s", e.Field, e.Message)
}

// ConflictError indicates a write conflict with an existing resource.
type ConflictError struct {
    Resource   string
    Identifier string
    Cause      error
}

func (e *ConflictError) Error() string {
    return fmt.Sprintf("conflict: %s %s already exists", e.Resource, e.Identifier)
}

func (e *ConflictError) Unwrap() error { return e.Cause }
```

Rules:
- Suffix the type name with `Error`.
- Implement `Unwrap() error` when wrapping an underlying cause.
- Fields are exported for programmatic inspection.
- Check with `errors.As`:

```go
var ve *ValidationError
if errors.As(err, &ve) {
    logger.LogAttrs(ctx, slog.LevelWarn, "validation failed",
        slog.String("field", ve.Field),
        slog.String("message", ve.Message),
    )
}
```

## Boundary Error Mapping

Domain code never imports `net/http` or `google.golang.org/grpc/status`.
Map domain errors to transport codes at the boundary.

### Transport-agnostic error map

Define a reusable error map that translates domain sentinels to transport status
codes. This decouples the mapping logic from any specific handler and ensures
consistency across all endpoints:

```go
// errkind maps domain error sentinels to transport-level status info.
type errMapping struct {
    sentinel error
    httpCode int
    grpcCode codes.Code
    message  string // user-facing; never expose internal details
}

var errorMap = []errMapping{
    {domain.ErrNotFound, http.StatusNotFound, codes.NotFound, "not found"},
    {domain.ErrValidation, http.StatusBadRequest, codes.InvalidArgument, ""},    // message from error
    {domain.ErrConflict, http.StatusConflict, codes.AlreadyExists, "resource already exists"},
    {domain.ErrPermission, http.StatusForbidden, codes.PermissionDenied, "permission denied"},
}

func mapErrorHTTP(err error) (int, string) {
    for _, m := range errorMap {
        if errors.Is(err, m.sentinel) {
            msg := m.message
            if msg == "" {
                msg = err.Error()
            }
            return m.httpCode, msg
        }
    }
    return http.StatusInternalServerError, "internal error"
}

func mapErrorGRPC(err error) (codes.Code, string) {
    for _, m := range errorMap {
        if errors.Is(err, m.sentinel) {
            msg := m.message
            if msg == "" {
                msg = err.Error()
            }
            return m.grpcCode, msg
        }
    }
    return codes.Internal, "internal error"
}
```

This pattern scales: adding a new domain error is one line in the table. Both
HTTP and gRPC boundaries stay consistent automatically. Unknown errors always
map to 500/Internal and get logged — the caller never sees implementation details.

### HTTP handler pattern

```go
func (h *Handler) CreateOrder(w http.ResponseWriter, r *http.Request) {
    order, err := decode[CreateOrderRequest](r)
    if err != nil {
        respondError(w, http.StatusBadRequest, "invalid request body")
        return
    }

    result, err := h.orders.Create(r.Context(), order)
    if err != nil {
        h.handleError(w, r, err)
        return
    }
    respond(w, http.StatusCreated, result)
}

func (h *Handler) handleError(w http.ResponseWriter, r *http.Request, err error) {
    var ve *domain.ValidationError
    switch {
    case errors.As(err, &ve):
        respondError(w, http.StatusUnprocessableEntity, ve.Error())
    case errors.Is(err, domain.ErrNotFound):
        respondError(w, http.StatusNotFound, "not found")
    case errors.Is(err, domain.ErrConflict):
        respondError(w, http.StatusConflict, "resource already exists")
    default:
        // Unknown errors are internal. Log and return 500.
        h.logger.LogAttrs(r.Context(), slog.LevelError, "unhandled error",
            slog.String("method", r.Method),
            slog.String("path", r.URL.Path),
            slog.Any("err", err),
        )
        respondError(w, http.StatusInternalServerError, "internal error")
    }
}
```

### gRPC interceptor pattern

```go
func ErrorUnaryInterceptor(logger *slog.Logger) grpc.UnaryServerInterceptor {
    return func(
        ctx context.Context,
        req any,
        info *grpc.UnaryServerInfo,
        handler grpc.UnaryHandler,
    ) (any, error) {
        resp, err := handler(ctx, req)
        if err == nil {
            return resp, nil
        }
        return nil, domainToGRPC(ctx, logger, info.FullMethod, err)
    }
}

func domainToGRPC(ctx context.Context, logger *slog.Logger, method string, err error) error {
    var ve *domain.ValidationError
    switch {
    case errors.As(err, &ve):
        return status.Errorf(codes.InvalidArgument, "%s: %s", ve.Field, ve.Message)
    case errors.Is(err, domain.ErrNotFound):
        return status.Error(codes.NotFound, "not found")
    case errors.Is(err, domain.ErrConflict):
        return status.Error(codes.AlreadyExists, "resource already exists")
    default:
        logger.LogAttrs(ctx, slog.LevelError, "unhandled error",
            slog.String("method", method),
            slog.Any("err", err),
        )
        return status.Error(codes.Internal, "internal error")
    }
}
```

## Error Classification for Retry and Routing

Errors in production systems need more than a message — they need classification
so callers can make routing decisions (retry, propagate, alert) without parsing
strings or knowing implementation details. Classify at creation, not at handling.

### Classify at Creation

Mark errors as permanent or retryable at the point where you know. Do not push
this decision to the caller.

```go
// Package-level error wrapping for classification.
type PermanentError struct{ Err error }
func (e *PermanentError) Error() string { return e.Err.Error() }
func (e *PermanentError) Unwrap() error { return e.Err }

func IsPermanent(err error) bool {
	var p *PermanentError
	return errors.As(err, &p)
}
```

OTel Collector, Thanos, and Temporal all use this pattern. Thanos adds a
`HaltError` that blocks the process to prevent further damage from data
corruption.

### SafeToRetry (pgx pattern)

For database and network operations, the critical question is: "did we send data
to the server?" Only errors that occurred before any data was sent are safe to
retry unconditionally. Unknown errors default to not safe — false negatives are
better than duplicate writes.

```go
// SafeToRetry checks whether the failed operation is known
// to have occurred before any data was sent to the server.
func SafeToRetry(err error) bool {
	for {
		s, ok := err.(interface{ SafeToRetry() bool })
		if ok {
			return s.SafeToRetry()
		}
		err = errors.Unwrap(err)
		if err == nil {
			return false // unknown errors are not safe to retry
		}
	}
}
```

### OperationPossiblySucceeded (Temporal pattern)

For operations with side effects (DB writes, RPCs), classify whether the
operation might have committed despite the error. If possibly succeeded, trigger
side-effect notifications (task scheduling, event propagation). If definitely not
committed, skip them.

```go
func OperationPossiblySucceeded(err error) bool {
	if err == nil {
		return true
	}
	// These errors mean definitely not committed:
	if IsConditionFailed(err) || IsResourceExhausted(err) {
		return false
	}
	// Network timeout, unknown error = might have committed
	return true
}
```

### Typed Close/Failure Reasons (NATS pattern)

For connection-oriented systems, type every close reason. This enables precise
monitoring, differential cleanup, and targeted alerting.

```go
type ClosedState int
const (
	ClientClosed ClosedState = iota + 1
	AuthenticationTimeout
	SlowConsumerPendingBytes
	SlowConsumerWriteDeadline
	WriteError
	ReadError
	ParseError
	MaxPayloadExceeded
	// ... every reason is enumerated
)
```

### Three-Way Error Action (TiDB pattern)

When multiple components must decide how to handle an error, use a three-way
classification that separates error classification from error handling:

- `ActionError` — this is a real error, propagate it
- `ActionRetry` — this is retryable, retry the operation
- `ActionNoIdea` — I don't know, let someone else decide

This allows each layer to contribute classification without overriding the
decisions of other layers.

### Decision Table

| I need to... | Pattern |
|---|---|
| Decide whether to retry | Classify as permanent/retryable at creation |
| Retry a DB/network operation | Check SafeToRetry — was data sent? |
| Handle possible partial commit | Use OperationPossiblySucceeded |
| Monitor connection failures | Type every close reason with an enum |
| Let multiple layers classify | Three-way action (Error/Retry/NoIdea) |

## Multi-Error Patterns

Use `errors.Join` (Go 1.20+) to collect independent errors. Each joined
error is individually reachable via `errors.Is` and `errors.As`.

```go
func (c *Config) Validate() error {
    var errs []error
    if c.Port == 0 {
        errs = append(errs, errors.New("port is required"))
    }
    if c.Host == "" {
        errs = append(errs, errors.New("host is required"))
    }
    if c.Timeout <= 0 {
        errs = append(errs, errors.New("timeout must be positive"))
    }
    return errors.Join(errs...) // nil if errs is empty
}
```

When to use multi-errors vs fail-fast:
- **Multi-error**: validation, cleanup (close N resources), health checks --
  collect everything so the caller sees all problems at once.
- **Fail-fast**: sequential operations where later steps depend on earlier
  ones, or when continuing would cause damage.

## Testing Errors

Always assert the specific error, not just its existence.

```go
// BAD -- only checks that an error occurred
if err == nil {
    t.Fatal("expected error")
}

// BAD -- fragile string match
if err.Error() != "not found" {
    t.Fatalf("wrong error: %v", err)
}

// GOOD -- sentinel check
if !errors.Is(err, domain.ErrNotFound) {
    t.Fatalf("expected ErrNotFound, got: %v", err)
}

// GOOD -- type check with inspection
var ve *domain.ValidationError
if !errors.As(err, &ve) {
    t.Fatalf("expected ValidationError, got: %T: %v", err, err)
}
if ve.Field != "email" {
    t.Errorf("expected field %q, got %q", "email", ve.Field)
}
```

For table-driven tests, include the expected error in the test case. In new
tests, use `github.com/alecthomas/assert/v2` for routine assertions:

```go
tests := []struct {
    name    string
    input   string
    wantErr error // nil, sentinel, or checked with errors.As in the assertion
}{
    {"valid", "alice@example.com", nil},
    {"empty", "", domain.ErrRequired},
    {"duplicate", "taken@example.com", domain.ErrConflict},
}
for _, tt := range tests {
    t.Run(tt.name, func(t *testing.T) {
        err := validate(tt.input)
        if tt.wantErr == nil {
            assert.NoError(t, err)
            return
        }
        assert.IsError(t, err, tt.wantErr)
    })
}
```

## Panic and Recover

Panic is not an error handling strategy. It is a signal that a programmer
invariant has been violated and the program is in an inconsistent state.

### When panic is acceptable

1. **True invariant violations** — continuing would cause data corruption or
   undefined behavior. This is rare in application code.
2. **API misuse that is always a programmer bug** — e.g., a nil argument that
   the contract forbids, called before `Init()`. The stdlib panics in these
   cases (slice out-of-bounds, nil pointer dereference, closed channel send).
3. **Dev/test-only invariant assertions** — checks that panic to catch
   programmer misuse are acceptable only when gated out of production by build
   tags, environment, or validated config. Production runtime failures still
   return errors.
4. **Package-internal control flow** — see below.

### When panic is NOT acceptable

- Transient failures (network, disk, auth). Return errors.
- Malformed user input. Return errors.
- As a substitute for `return err` because unwinding is convenient.

### Approved recover sites

`recover` is never used in application code. It appears only in code whose
purpose is to own goroutines or contain panics at a documented boundary. There
are exactly three approved patterns:

**1. Goroutine supervisors** (`safe.Go`, `safe.Collect`):

A supervisor owns goroutines, waits for them, and converts panics into errors
visible to the owner. The owner does not continue as if nothing happened — it
receives a fatal error and acts on it (cancel siblings, shut down). This is NOT
the "broad recovery middleware" anti-pattern (net/http's handler recovery, which
swallows panics and continues serving with potentially corrupted state).

See [concurrency.md](concurrency.md) for usage.

**2. Package-internal panic+recover for deeply recursive code:**

Adapted from the Google Go Style Guide. In recursive descent parsers, tree
walkers, or other deeply nested traversals, threading `error` through every
frame adds noise. An internal panic with a deferred recover at the public entry
point acts as a structured longjmp.

```go
// syntaxError is unexported — the panic never escapes this package.
type syntaxError struct {
    pos int
    msg string
}

func Parse(in string) (_ *Node, err error) {
    defer func() {
        if p := recover(); p != nil {
            e, ok := p.(*syntaxError)
            if !ok {
                panic(p) // re-panic: not ours
            }
            err = fmt.Errorf("parse %q at %d: %s", in, e.pos, e.msg)
        }
    }()
    return parse(in, 0)
}

// Deep in recursion:
func (p *parser) expect(tok tokenType) {
    if p.current.Type != tok {
        panic(&syntaxError{pos: p.current.Pos, msg: fmt.Sprintf("expected %s", tok)})
    }
}
```

**Invariants for this pattern:**

- The panic value is an **unexported type** — it cannot escape the package.
- The recover is in the **same function** as the public entry point.
- Unknown panics are **re-panicked** (`if !ok { panic(p) }`).
- The recovered panic is **converted to a returned error** — it is not
  swallowed or logged-and-continued.
- This is a deliberate design choice, not a default. Prefer normal error
  returns unless the recursion depth genuinely makes them unwieldy.

**3. Infrastructure boundary recovery that aborts the operation:**

Transport infrastructure and narrow system boundaries around third-party code
that may panic on malformed input may recover only to attach structured
observability and then abort the operation. They must not translate the panic
into ordinary application control flow or continue as if the operation
succeeded. For `net/http`, log panic metadata and re-panic with
`http.ErrAbortHandler` after ensuring request-scoped identifiers are recorded.
For gRPC/Connect, prefer process-level supervision; only use equivalent
infrastructure recovery when it aborts the request and preserves panic
visibility.

### What is NOT approved

- HTTP/gRPC panic recovery middleware that converts panics into normal 500
  responses and continues. Google's style guide calls net/http's handler
  recovery "a historical mistake." A broad recover that catches panics from
  arbitrary code cannot know whether state is corrupted. Do not add
  `recovery.UnaryServerInterceptor()` or equivalent unless it follows approved
  pattern 3.
- Recovering panics to "avoid crashes." If the code panicked, something is
  wrong. Make it visible, don't hide it.
- Recovering in application code and continuing the current operation. Only
  supervisors, package entry points, and aborting infrastructure/system
  boundaries may recover.

**Acceptable panic() sites beyond Must* constructors:**

- Exhaustive switch defaults for enum types (crash > silent wrong behavior)
- Data structure invariant violations where continuing risks data corruption
- `_ struct{}` as last field in public structs (prevents unkeyed literal
  initialization)
