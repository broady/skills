# Go Design Idioms Reference

Struct design, receiver consistency, style guardrails, function organization, generics, copy semantics, and API evolution.
See [design.md](design.md) for the core model (packages, DI, interfaces, API design).

## Contents

1. [Struct Design](#1-struct-design) — zero value, field names, embedding, mutex fields, noCopy
2. [Receiver Consistency](#2-receiver-consistency) — pointer or value receivers per type, never mixed
3. [Uber Style Guardrails](#3-uber-style-guardrails) — type assertions, enums, time, nil slices, names, defer
4. [Code Organization Within a Function](#4-code-organization-within-a-function) — guard clauses, line of sight, variable placement
5. [Generics Guidelines](#5-generics-guidelines) — when to use, when not to, good patterns
6. [API Evolution Safety](#6-api-evolution-safety) — _ struct{}, sealed interfaces, enforced construction
7. [Copy Slices and Maps at Boundaries](#7-copy-slices-and-maps-at-boundaries) — inbound copies, outbound copies, when to skip

## 1. Struct Design

Prefer useful zero values for value types and optional configuration objects.
Service types with required dependencies may require constructors; a meaningless
zero-value service is worse than an explicit `NewService(...)`.

Make value-type zero values useful:

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

## 2. Receiver Consistency

Choose pointer receivers or value receivers for a type and use that choice for
every method on the type. Do not mix receiver kinds. Mixed receivers make method
sets harder to reason about and can create subtle interface-satisfaction bugs.

Use pointer receivers when any method mutates the receiver, the type contains a
mutex, atomic value, noCopy marker, slice/map that represents owned mutable
state, or the struct is large enough that copying is meaningful. Once one method
needs a pointer receiver, all methods on that type use pointer receivers.

Use value receivers only for small immutable value types where copying is part
of the intended semantics.

```go
// GOOD: all methods use pointer receivers.
func (s *Store) Get(ctx context.Context, id UserID) (User, error) { /* ... */ }
func (s *Store) Put(ctx context.Context, user User) error         { /* ... */ }

// BAD: mixed receiver kinds on one type.
func (s Store) Get(ctx context.Context, id UserID) (User, error) { /* ... */ }
func (s *Store) Put(ctx context.Context, user User) error        { /* ... */ }
```

## 3. Uber Style Guardrails

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

## 4. Code Organization Within a Function

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

## 5. Generics Guidelines

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

## 6. API Evolution Safety

### Prevent Unkeyed Struct Literals
Add `_ struct{}` as the last field in public structs:
```go
type Settings struct {
	ID        component.ID
	BuildInfo BuildInfo
	_         struct{} // prevent unkeyed literal initialization
}
```
This forces callers to use field names, making it safe to add new fields without breaking existing code.

### Sealed Interfaces
Add an unexported method to prevent external implementations:
```go
type Factory interface {
	CreateDefaultConfig() Config
	unexportedFactory() // prevents external implementations
}
```
All instances must come through official constructors. Enables safe API evolution without breaking external code. Use for: factory interfaces in libraries where you need to add methods over time.

### Config Must Come From ParseConfig
For library types with complex defaults, enforce construction through the parser:
```go
type Config struct {
	// ... fields ...
	createdByParseConfig bool
}

func ParseConfig(connString string) (*Config, error) {
	cfg := &Config{createdByParseConfig: true}
	// ... parse and set defaults ...
	return cfg, nil
}

func Connect(ctx context.Context, cfg *Config) (*Conn, error) {
	if !cfg.createdByParseConfig {
		panic("config must be created by ParseConfig")
	}
	// ...
}
```
Prevents misconfiguration from zero-valued structs. Provide `Copy()` for safe modification after parsing.

## 7. Copy Slices and Maps at Boundaries

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
