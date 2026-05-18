# Go Design Reference

## Contents

1. [Package Design](#1-package-design) — naming, internal/, file organization, declaration ordering
2. [Dependency Injection](#2-dependency-injection) — constructor wiring, context is not DI, context keys
3. [Interface Design](#3-interface-design) — consumer-side, small interfaces, compile-time checks
4. [API Design — Hard to Misuse](#4-api-design--hard-to-misuse) — strong types, config structs, builders, functional options (discouraged)

**See also:** [design-idioms.md](design-idioms.md) — struct design, Uber guardrails, function organization, generics, copy semantics, API evolution

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

Within a file, order declarations for navigation:

1. Package-level types, constants, and variables that define the public surface.
2. Constructors and setup functions (`NewX`, `OpenX`, `ParseX`).
3. Methods grouped by receiver, exported before unexported.
4. Package-level helper functions, exported before unexported.

Keep a receiver's methods together unless separating them by build tag or
platform-specific implementation. Do not scatter methods for the same type
throughout a file by chronological edit history.

## 2. Dependency Injection

Constructor injection only. No mutable globals. Avoid `init()`. No DI containers.
Manual wiring scales further than people expect because the dependency graph is
plain code: searchable, debuggable, breakpointable, and visible in review. A
service with 30-40 dependencies is still tractable when wiring is grouped into a
few small `newX(...)` helpers.

```go
// GOOD: explicit wiring in main(). Config is loaded once, then values are
// passed to constructors. See references/config.md for the full pattern.
func run(ctx context.Context, cfg *Config) error {
	db := postgres.Open(cfg.DatabaseURL)
	cache := redis.New(cfg.RedisURL)
	users := user.NewService(db, cache)
	orders := order.NewService(db, users)
	srv := api.NewServer(users, orders)
	if err := srv.ListenAndServe(cfg.Addr); err != nil && !errors.Is(err, http.ErrServerClosed) {
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
[server/scaffold.md](server/scaffold.md) for the complete pattern.

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

Do not export context keys. Export accessor functions instead. Package-level
pointer keys create an avoidable exception to the global-pointer rule and make
it easier for callers to bypass type-safe accessors.

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

Do not define interfaces preemptively. Define an interface only when the
consumer needs a seam: a test double, a second implementation, or a plugin
boundary. Do not define provider-side interfaces before a consumer needs them.
(Google Go Style Guide: "Do not define interfaces before they are used.")

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
| At most one of N strategies; require one | Interface field on config struct + `Validate() error` |
| Cross-field validation rules | Config struct + `Validate() error` |
| Valid-next depends on what-came-before | Builder, optionally with type-state |

### Config structs for optional settings

Prefer config structs over functional options for optional service settings.
Structs are serializable, inspectable in debuggers, easy to validate, and
visible in tests. They make defaults and cross-field constraints explicit.
Closure-style `WithX` options hide configuration and are especially discouraged.

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
at-most-one choices. Add `Validate()` when one choice is required. Required
dependencies may remain explicit constructor parameters when that makes
ownership clearer.

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

Functional options do not express "at most one of" as directly as a single
interface field. An interface field on the config struct enforces mutual
exclusion at compile time. A nil interface field still compiles, so `Validate()`
enforces that one strategy is required.

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
	Transport Transport // at most one concrete strategy; Validate requires one
	RetryMax  int
}

func (c ClientConfig) Validate() error {
	if c.Transport == nil {
		return errors.New("transport is required")
	}
	return nil
}

// Usage: the caller chooses one, and can't choose two.
client := NewClient(ClientConfig{
	Transport: GRPCTransport{Addr: "localhost:9090"},
	RetryMax:  3,
})
```

### Config validation

When fields have cross-cutting constraints, add a `Validate` method. Call
it at the top of the constructor. For application-level config, `LoadConfig()`
also calls `Validate()` before construction (see [config.md](config.md)) —
both layers can coexist since validation is idempotent.

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

### Functional options are discouraged for service config

For production services, prefer explicit config structs because they are
inspectable, serializable, and validate cleanly. For reusable public Go
libraries, Uber-style functional options are acceptable, especially when the API
has many optional parameters and does not model loaded runtime config.
Closure-style `WithX` options are especially discouraged. This intentionally
diverges from Uber's public-API guidance: for production services, inspectable
config usually matters more than call-site fluency.

Risks:

- **Mutual exclusion is runtime-only**: callers can pass `WithHTTP(...)` and
  `WithGRPC(...)` unless the API adds validation. A single interface field makes
  "at most one" visible in the type.
- **Ordering is runtime-only**: options can't express "set source before sink";
  use a builder when valid next steps matter.
- **Loaded config is awkward**: options don't map cleanly to env/files/flags or
  round-trip tests.
- **Inspectability varies**: Uber-style value options are more debuggable than
  closures, but a config struct is still easier to print, diff, validate, and
  pass around as data.

Use config structs for optional and serializable settings. Use interface fields
for at-most-one choices and `Validate()` when one is required. Use builders when
valid next steps depend on construction order.

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
