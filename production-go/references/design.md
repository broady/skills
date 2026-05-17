# Go Design Reference

## Contents

1. [Package Design](#1-package-design) — naming, internal/, file organization
2. [Dependency Injection](#2-dependency-injection) — constructor wiring, context is not DI, context keys
3. [Interface Design](#3-interface-design) — consumer-side, small interfaces, compile-time checks
4. [API Design — Hard to Misuse](#4-api-design--hard-to-misuse) — strong types, config structs, builders, functional options (discouraged)
5. [Struct Design](#5-struct-design) — zero value, field names, embedding, mutex fields, noCopy
6. [Uber Style Guardrails](#6-uber-style-guardrails) — type assertions, enums, time, nil slices, names, defer
7. [Code Organization Within a Function](#7-code-organization-within-a-function) — guard clauses, line of sight, variable placement
8. [Generics Guidelines](#8-generics-guidelines) — when to use, when not to, good patterns
9. [Copy Slices and Maps at Boundaries](#9-copy-slices-and-maps-at-boundaries) — inbound copies, outbound copies, when to skip

## 1. Package Design

Name packages for what they **provide**, not what they contain.

```
GOOD: user/  order/  payment/  auth/  notify/
BAD:  utils/ common/ helpers/ base/ misc/ models/ types/
```

Prefer fewer, larger packages over deep hierarchies. A flat `internal/` tree beats `internal/core/base/abstract/`.

Use `internal/` to prevent external imports of implementation details:

```
myapp/
  cmd/server/main.go
  order/         # public API
  payment/       # public API
  internal/
    postgres/    # implementation detail — unexportable
    stripe/      # implementation detail — unexportable
```

Organize files within a package by **import similarity** — files that import the same dependencies belong together. Do not split one-struct-per-file.

## 2. Dependency Injection

Constructor injection only. No mutable globals. Avoid `init()`. No DI containers.
Manual wiring scales further than people expect because the dependency graph is
plain code: searchable, debuggable, breakpointable, and visible in review. A
service with 30-40 dependencies is still tractable when wiring is grouped into a
few small `newX(...)` helpers.

```go
// GOOD: explicit wiring in main()
func run(ctx context.Context) error {
	db := postgres.Open(os.Getenv("DATABASE_URL"))
	cache := redis.New(os.Getenv("REDIS_URL"))
	users := user.NewService(db, cache)
	orders := order.NewService(db, users)
	srv := api.NewServer(users, orders)
	if err := srv.ListenAndServe(":8080"); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve http: %w", err)
	}
	return nil
}
```

```go
// BAD: hidden global state, I/O in init, and process exit outside main
var db *sql.DB

func init() {
	var err error
	db, err = sql.Open("postgres", os.Getenv("DATABASE_URL"))
	if err != nil {
		log.Fatal(err)
	}
}
```

When `init()` is unavoidable, follow Uber's constraints: it must be completely
deterministic, avoid I/O, avoid reading environment or process state, avoid
goroutines, avoid mutable global state, and not depend on the order of other
`init()` functions. Acceptable cases are rare: registration hooks required by
an imported package, complex immutable precomputation, or similar deterministic
setup that cannot be expressed as a plain declaration.

The wiring in `main()` is the documentation. If one function becomes too large,
split it by subsystem while keeping construction explicit:

```go
func run(ctx context.Context) error {
	cfg := loadConfig()
	logger := newLogger(cfg)
	db := newDB(cfg)

	stores := newStores(db)
	services := newServices(logger, stores)
	srv := newHTTPServer(logger, services)

	if err := srv.ListenAndServe(cfg.HTTPAddr); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve http: %w", err)
	}
	return nil
}
```

Server lifecycle and signal handling belong in the runner; see
[server.md](server.md) for the complete pattern.

The helpers should only wire dependencies. They should not hide global state,
start background work, read ambient configuration, or choose implementations
behind an opaque service locator.

### Context is not dependency injection

Dependencies come from constructors. Request-scoped values come from
`context.Context`. Do not blur the line.

```go
// BAD: context as service locator
func CreateOrder(ctx context.Context, req CreateOrderRequest) error {
	db := ctxutil.DB(ctx)
	logger := ctxutil.Logger(ctx)
	return insertOrder(ctx, db, logger, req)
}

// GOOD: dependencies are explicit, request metadata stays in context
type OrderService struct {
	db     *sql.DB
	logger *slog.Logger
}

func (s *OrderService) Create(ctx context.Context, req CreateOrderRequest) error {
	return insertOrder(ctx, s.db, s.logger, req)
}
```

Allowed context values are values that genuinely travel with one request:
trace/span IDs, request IDs, auth principals, locale, and similar metadata.
Database handles, clients, stores, services, loggers, clocks, ID generators,
feature flag clients, and configuration are dependencies. Pass them explicitly.

### Context key design

Never use `string` or other built-in types as context keys — they collide
across packages. Use an unexported type so only your package can read/write it.

```go
// Internal key — zero-allocation, collision-proof by type identity.
type requestIDKey struct{}

func WithRequestID(ctx context.Context, id string) context.Context {
    return context.WithValue(ctx, requestIDKey{}, id)
}

func RequestIDFromContext(ctx context.Context) (string, bool) {
    id, ok := ctx.Value(requestIDKey{}).(string)
    return id, ok
}
```

Expose `WithX` / `XFromContext` accessor functions, not the key itself.
This centralizes the type assertion, prevents misuse, and follows the
stdlib pattern (`net/http/httptrace`, `runtime/pprof`, OpenTelemetry).

For exported keys (rare — prefer accessors), use a pointer to a named struct:

```go
type contextKey struct{ name string }
var UserIDKey = &contextKey{"user-id"} // pointer identity, no allocation
```

Provide no-op defaults for optional dependencies. Loggers are not optional
inside production components: accept `*slog.Logger` in the constructor and bind
component attributes there.

```go
type Service struct {
	db     *sql.DB
	logger *slog.Logger
	output io.Writer
}

func NewService(db *sql.DB, logger *slog.Logger, cfg ServiceConfig) *Service {
	if cfg.Output == nil {
		cfg.Output = io.Discard
	}
	return &Service{
		db:     db,
		logger: logger.With("component", "service"),
		output: cfg.Output,
	}
}
```

## 3. Interface Design

Define interfaces at the **consumer**, not the producer.

```go
// GOOD: consumer defines what it needs
package order

type UserLookup interface {
	User(ctx context.Context, id user.ID) (*user.User, error)
}

type Service struct {
	users UserLookup
}
```

```go
// BAD: producer defines a large interface that all consumers must accept
package user

type Service interface {
	Create(ctx context.Context, u *User) error
	User(ctx context.Context, id ID) (*User, error)
	Update(ctx context.Context, u *User) error
	Delete(ctx context.Context, id ID) error
	List(ctx context.Context, filter Filter) ([]*User, error)
	// ... 15 more methods
}
```

Keep interfaces small: 1-2 methods. `io.Reader` not `io.ReadWriteCloser`.

Accept interfaces, return structs:

```go
// GOOD
func NewService(store UserLookup) *Service { ... }

// BAD
func NewService(store UserLookup) ServiceInterface { ... }
```

Compile-time interface verification:

```go
var _ http.Handler = (*Server)(nil)
var _ io.ReadCloser = (*FileBuffer)(nil)
```

Do not define interfaces preemptively. Wait until you have a second consumer that needs a different implementation. (Google Go Style Guide: "Do not define interfaces before they are used.")

Never use a pointer to an interface:

```go
// BAD — always wrong
func Process(r *io.Reader) { ... }

// GOOD
func Process(r io.Reader) { ... }
```

## 4. API Design — Hard to Misuse

Use distinct types to prevent argument swapping:

```go
// BAD: which ID is which?
func Transfer(from, to string, amount int) error

// GOOD: compiler catches swaps
type AccountID string
type UserID string
type Cents int

func Transfer(from, to AccountID, amount Cents) error
```

### Constructor style decision matrix

| Situation | Pattern |
|---|---|
| Few params, all required | Plain constructor: `NewStore(db, logger)` |
| Optional settings, good defaults | Config struct with zero-value defaults |
| Serializable/loaded settings | Config struct with explicit tags |
| Exactly one of N strategies | Interface field on config struct |
| Cross-field validation rules | Config struct + `Validate() error` |
| Valid-next depends on what-came-before | Builder, optionally with type-state |

### Config structs for optional settings

Prefer config structs over functional options for optional settings. Structs
are serializable, inspectable in debuggers, easy to validate, and visible in
tests. They make defaults and cross-field constraints explicit instead of
hiding configuration in a list of closures.

```go
type ClientConfig struct {
	Timeout    time.Duration
	MaxRetries int
	Transport  Transport
}

func NewClient(logger *slog.Logger, cfg ClientConfig) (*Client, error) {
	if cfg.Timeout == 0 {
		cfg.Timeout = 30 * time.Second
	}
	if cfg.MaxRetries == 0 {
		cfg.MaxRetries = 3
	}
	if cfg.Transport == nil {
		return nil, errors.New("transport is required")
	}
	return &Client{
		logger:    logger.With("component", "client"),
		timeout:   cfg.Timeout,
		retryMax:  cfg.MaxRetries,
		transport: cfg.Transport,
	}, nil
}
```

Use interfaces inside config structs for substitutable dependencies or
exactly-one-of choices. Required dependencies may remain explicit constructor
parameters when that makes ownership clearer.

### Config structs for loaded settings

Use config structs when settings are loaded from files/env/flags, need
cross-field validation, or must be serialized/inspected in tests.

```go
type ServerConfig struct {
	Addr       string        `json:"addr" yaml:"addr"`
	Timeout    time.Duration `json:"timeout" yaml:"timeout"`
	MaxRetries int           `json:"max_retries" yaml:"max_retries"`
}

func NewServer(logger *slog.Logger, cfg ServerConfig) *Server {
	if cfg.Addr == "" {
		cfg.Addr = ":8080"
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 30 * time.Second
	}
	logger = logger.With("component", "server")
	// ...
}
```

Zero-value fields get sensible defaults in the constructor. Callers set only
what they care about. The struct literal is self-documenting.

### Interface fields for mutually exclusive choices

Functional options can't express "exactly one of" — they're a flat list. An
interface field on the config struct enforces mutual exclusion at compile time.

```go
// Sealed interface — only this package defines implementations.
type Transport interface{ transport() }

type HTTPTransport struct {
	URL     string
	Timeout time.Duration
}
func (HTTPTransport) transport() {}

type GRPCTransport struct {
	Addr     string
	Insecure bool
}
func (GRPCTransport) transport() {}

type ClientConfig struct {
	Transport Transport // exactly one — compiler enforces it
	RetryMax  int
}

// Usage: the caller MUST choose, and CAN'T choose two.
client := NewClient(ClientConfig{
	Transport: GRPCTransport{Addr: "localhost:9090"},
	RetryMax:  3,
})
```

### Config validation

When fields have cross-cutting constraints, add a `Validate` method. Call
it at the top of the constructor.

```go
func (c ServerConfig) Validate() error {
	if c.Timeout > 0 && c.Timeout < time.Second {
		return errors.New("timeout must be >= 1s if set")
	}
	if c.MaxRetries < 0 {
		return errors.New("max_retries must be non-negative")
	}
	return nil
}

func NewServer(logger *slog.Logger, cfg ServerConfig) (*Server, error) {
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("server config: %w", err)
	}
	logger = logger.With("component", "server")
	// ...
}
```

### Builders for stateful construction

When what's valid next depends on what came before, a builder with type-state
makes invalid sequences unrepresentable. Each step returns a different type,
narrowing the available methods.

```go
type PipelineSpec struct{}
type PipelineWithSource struct{ source string }
type PipelineReady struct{ source, sink string; filter func([]byte) bool }

func NewPipeline() PipelineSpec { return PipelineSpec{} }

func (PipelineSpec) From(src string) PipelineWithSource {
	return PipelineWithSource{source: src}
}

func (p PipelineWithSource) To(sink string) PipelineReady {
	return PipelineReady{source: p.source, sink: sink}
}

func (p PipelineReady) WithFilter(f func([]byte) bool) PipelineReady {
	p.filter = f
	return p
}

func (p PipelineReady) Build() (*Pipeline, error) {
	// source and sink are guaranteed non-zero — compiler enforced.
	return &Pipeline{source: p.source, sink: p.sink, filter: p.filter}, nil
}

// Usage: only compiles if From → To → Build order is followed.
pipe, err := NewPipeline().From("input.csv").To("output.parquet").Build()
```

### Functional options are discouraged

Functional options (`WithFoo(...)`) are a flat list of closures. Avoid adding
them in new code unless you are extending an existing option-based API or
wrapping an ecosystem API that already uses them. Problems:

- **No mutual exclusion**: callers can pass `WithHTTP(...)` AND `WithGRPC(...)`
  — the last one silently wins, or you add runtime validation that the type
  system should have caught.
- **No ordering**: can't express "set source before sink."
- **Not serializable**: can't load from config files, can't round-trip in tests.
- **Opaque**: a `[]Option` is invisible to debuggers and loggers.
- **Hard to document as data**: callers can't easily print, diff, validate, or
  pass the complete configuration around.

Use config structs for optional and serializable settings. Use interface fields
for exactly-one-of choices. Use builders when valid next steps depend on
construction order.

Require at least one item when an empty collection is nonsensical:

```go
// GOOD: compile-time guarantee of >= 1 item
func Merge(first *Config, rest ...*Config) *Config

// BAD: panics or returns zero value on empty input
func Merge(configs []*Config) *Config
```

Accept the narrowest interface:

```go
// GOOD: works with any reader
func Parse(r io.Reader) (*Doc, error)

// BAD: demands a file when you only need to read
func Parse(f *os.File) (*Doc, error)
```

Avoid nil parameters. If a parameter can be nil, callers must think about nil.
Prefer config structs with zero-value defaults instead.

## 5. Struct Design

Make the zero value useful:

```go
// GOOD: zero value is a usable, unbuffered writer
type Writer struct {
	buf    []byte
	output io.Writer
}

func (w *Writer) Write(p []byte) (int, error) {
	if w.output == nil {
		w.output = os.Stdout // useful default
	}
	return w.output.Write(p)
}
```

```go
// BAD: zero value panics
type Writer struct {
	output io.Writer // nil → panic on use
}
```

Always use field names in struct literals:

```go
// GOOD
srv := &Server{
	Addr:    ":8080",
	Handler: mux,
}

// BAD: breaks when fields are reordered
srv := &Server{":8080", mux}
```

Always add struct tags for serialized types — the serialized form is a contract:

```go
type Order struct {
	ID        string    `json:"id"`
	UserID    string    `json:"user_id"`
	Total     Cents     `json:"total"`
	CreatedAt time.Time `json:"created_at"`
}
```

Embed consciously and intentionally. Do not embed mutexes, and do not embed
public implementation details that leak into the outer type's API. Use a named
field unless every promoted method is intentionally part of the API.

```go
// BAD: embedding exposes Lock/Unlock as part of SafeMap's API
type SafeMap struct {
	sync.Mutex
	m map[string]int
}

// GOOD: mutex is an implementation detail
type SafeMap struct {
	mu sync.Mutex
	m  map[string]int
}

// BAD: embedding for field reuse; just use a named field
type OrderResponse struct {
	BaseResponse // "inheriting" fields — confusing
	Items []Item
}
```

The zero value of `sync.Mutex` and `sync.RWMutex` is valid. Use non-pointer
mutex fields:

```go
type Cache struct {
	mu    sync.Mutex
	items map[string]Item
}
```

Field ordering: group by purpose, exported before unexported:

```go
type Server struct {
	// Configuration (exported)
	Addr    string
	Handler http.Handler

	// Internal state (unexported)
	logger   *slog.Logger
	mu       sync.Mutex
	conns    map[string]*conn
}
```

### Preventing struct copies with noCopy

For types where copying breaks invariants (connection pools, builders, types
holding a mutex), use a `noCopy` sentinel so `go vet` flags copies statically:

```go
type noCopy struct{}

func (*noCopy) Lock()   {}
func (*noCopy) Unlock() {}

type ConnPool struct {
    _  noCopy
    mu sync.Mutex
    // ...
}
```

The `Lock`/`Unlock` no-ops satisfy `sync.Locker`, which is what `go vet`'s
copylock checker looks for. Use a blank (`_`) or named field — never embed,
or the methods promote onto your type's API. The struct is zero-size, so there
is no runtime cost. This is the same pattern the stdlib uses in `sync.WaitGroup`,
`sync.Once`, and `strings.Builder`.

## 6. Uber Style Guardrails

### Type assertions

Use the comma-ok form. A single-value type assertion panics when the dynamic
type is not what you expected.

```go
// BAD
user := v.(*User)

// GOOD
user, ok := v.(*User)
if !ok {
	return errors.New("value is not a user")
}
```

### Enums

Start enum-like constants at one when zero is not a valid state. Use zero only
when it has a clear meaning such as unknown or default.

```go
type Status int

const (
	StatusUnknown Status = iota
	StatusPending
	StatusActive
)
```

### Time

Use `time.Time` for instants and `time.Duration` for durations. If an external
format forces a number, include the unit in the field name.

```go
type RetryConfig struct {
	Timeout       time.Duration `json:"-"`
	TimeoutMillis int           `json:"timeout_millis"`
}
```

### Nil slices

Nil slices are valid. Prefer `var items []T` or returning `nil` for empty
slices unless JSON/API semantics require an allocated empty slice.

```go
var filtered []Item
for _, item := range items {
	if item.Valid() {
		filtered = append(filtered, item)
	}
}
```

### Names and arguments

Avoid predeclared names such as `error`, `string`, `len`, and `cap`. Avoid
naked booleans and ambiguous same-type arguments; prefer domain types or named
constants.

```go
type Locality int

const (
	UnknownLocality Locality = iota
	Local
	Remote
)
```

### Cleanup

Use `defer` for cleanup of locks, files, spans, timers, and other resources.
Avoid `defer` only in measured nanosecond hot paths where the overhead matters.

### Raw strings and printf formats

Use raw string literals when they avoid escaping noise. If a printf-style
format string is declared outside the call, make it a `const` so vet can check
it.

## 7. Code Organization Within a Function

Guard clauses first. Happy path flows straight down ("line of sight"):

```go
// GOOD: errors handled immediately, happy path is left-aligned
func (s *Service) Process(ctx context.Context, id string) (*Result, error) {
	if id == "" {
		return nil, errors.New("empty id")
	}

	item, err := s.store.Get(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("get item: %w", err)
	}

	if item.Expired() {
		return nil, ErrExpired
	}

	result, err := s.transform(ctx, item)
	if err != nil {
		return nil, fmt.Errorf("transform: %w", err)
	}

	return result, nil
}
```

```go
// BAD: deep nesting, hard to follow
func (s *Service) Process(ctx context.Context, id string) (*Result, error) {
	if id != "" {
		item, err := s.store.Get(ctx, id)
		if err == nil {
			if !item.Expired() {
				result, err := s.transform(ctx, item)
				if err == nil {
					return result, nil
				} else {
					return nil, fmt.Errorf("transform: %w", err)
				}
			} else {
				return nil, ErrExpired
			}
		} else {
			return nil, fmt.Errorf("get item: %w", err)
		}
	}
	return nil, errors.New("empty id")
}
```

No unnecessary `else` after `return`:

```go
// GOOD
if err != nil {
	return err
}
proceed()

// BAD
if err != nil {
	return err
} else {
	proceed()
}
```

Declare variables closest to their use:

```go
// GOOD
func process(items []Item) error {
	for _, item := range items {
		key := item.Key() // declared where used
		if err := store.Put(key, item); err != nil {
			return err
		}
	}
	return nil
}

// BAD
func process(items []Item) error {
	var key string // declared far from use
	var err error
	for _, item := range items {
		key = item.Key()
		err = store.Put(key, item)
		if err != nil {
			return err
		}
	}
	return nil
}
```

## 8. Generics Guidelines

Use generics for:
- Type-safe containers and data structures
- Algorithms over slices, maps, channels
- Eliminating `interface{}`/`any` casts

Do not use generics for:
- Abstractions with only one concrete type today
- Showing off type-level programming
- Replacing interfaces that work fine

Wait for 3+ concrete implementations before reaching for generics. Start concrete.

Good generic patterns:

```go
// Result type — eliminates (T, error) tuple unpacking in some contexts
type Result[T any] struct {
	Value T
	Err   error
}

// Set — no built-in set type in Go
type Set[T comparable] map[T]struct{}

func NewSet[T comparable](items ...T) Set[T] {
	s := make(Set[T], len(items))
	for _, item := range items {
		s[item] = struct{}{}
	}
	return s
}

func (s Set[T]) Contains(v T) bool {
	_, ok := s[v]
	return ok
}

// SyncMap — typed wrapper around sync.Map
type SyncMap[K comparable, V any] struct {
	m sync.Map
}

func (m *SyncMap[K, V]) Load(key K) (V, bool) {
	val, ok := m.m.Load(key)
	if !ok {
		var zero V
		return zero, false
	}
	return val.(V), true
}

func (m *SyncMap[K, V]) Store(key K, val V) {
	m.m.Store(key, val)
}
```

Bad generic usage:

```go
// BAD: generic for no reason — only one type ever used
func GetUserName[T UserService](svc T, id string) string {
	return svc.GetName(id)
}

// GOOD: just use the concrete type
func GetUserName(svc *UserService, id string) string {
	return svc.GetName(id)
}
```

## 9. Copy Slices and Maps at Boundaries

Slices and maps are reference types. When a struct stores a slice received
from a caller, both sides share the backing array. The caller mutates it
later, your internal state silently corrupts. These bugs manifest under
load, weeks after the code ships, and are extremely hard to reproduce.

**No linter can catch this.** It requires understanding ownership intent —
sometimes sharing is deliberate. Apply this rule at every boundary where
data crosses an ownership line.

### Inbound: copy what you store

```go
// BAD: stores the caller's slice — caller can mutate s.hosts later
func NewCluster(hosts []string) *Cluster {
	return &Cluster{hosts: hosts}
}

// GOOD: defensive copy — the struct owns its data
func NewCluster(hosts []string) *Cluster {
	return &Cluster{hosts: slices.Clone(hosts)}
}
```

Same for maps:

```go
// BAD
func NewConfig(labels map[string]string) *Config {
	return &Config{labels: labels}
}

// GOOD
func NewConfig(labels map[string]string) *Config {
	return &Config{labels: maps.Clone(labels)}
}
```

### Outbound: copy what you return

```go
// BAD: returns internal state — caller can corrupt it
func (c *Cluster) Hosts() []string {
	return c.hosts
}

// GOOD: returns a copy — internal state is safe
func (c *Cluster) Hosts() []string {
	return slices.Clone(c.hosts)
}
```

### When NOT to copy

- **Hot paths** where the copy is measured as a bottleneck (profile first).
- **Deliberate shared ownership** — document it: `// NOTE: caller retains ownership; mutations are visible to this struct.`
- **Immutable-by-convention** types that are never mutated after construction.

If you skip the copy, the decision must be explicit and commented. The
default is to copy.

### Maps in particular

Maps are worse than slices because concurrent read/write on a map is a
**fatal runtime crash** (not a data race — a hard crash). If a map might be
accessed from multiple goroutines, either copy it or protect it with a mutex.

```go
// BAD: returns internal map — concurrent access = crash
func (s *Store) Labels() map[string]string {
	return s.labels
}

// GOOD: snapshot
func (s *Store) Labels() map[string]string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return maps.Clone(s.labels)
}
```
