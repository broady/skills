# Plugin Systems

Patterns for extensible Go architectures extracted from Caddy, OpenTelemetry
Collector, containerd, and OpenTofu.

## Contents

1. [Module Lifecycle](#1-module-lifecycle)
2. [Plugin Registration](#2-plugin-registration)
3. [Sealed Factory Interfaces](#3-sealed-factory-interfaces)
4. [API Evolution Safety](#4-api-evolution-safety)
5. [Two-Phase Commit for System Resources](#5-two-phase-commit-for-system-resources)
6. [Typed Error Classification](#6-typed-error-classification)
7. [Decision Table](#decision-table)
8. [Anti-Patterns](#anti-patterns)

---

## 1. Module Lifecycle

The standard lifecycle from Caddy and OTel Collector follows a strict
sequence. Each phase has a single responsibility:

```
New() -> Unmarshal/Configure -> Provision(ctx) -> Validate() -> [Start()] -> [Stop()] -> Cleanup()
```

Key principles:

- **Cleanup runs even on partial Provision failure.** If `Provision` allocates
  a file handle then fails on a network connection, `Cleanup` must still close
  the file handle. Without this guarantee, half-initialized modules leak
  resources.
- **Start is non-blocking.** Long-running work spawns into a goroutine whose
  context is created from `context.Background()` and canceled in `Stop`.
- **Stop/Shutdown is idempotent.** Safe to call without `Start` having been
  called. Safe to call twice. This simplifies teardown paths.

### Component interface (OTel Collector)

```go
type Component interface {
    Start(ctx context.Context, host Host) error
    Shutdown(ctx context.Context) error
}
```

### Optional lifecycle interfaces

Some hosts split lifecycle hooks into optional interfaces:

```go
type Provisioner interface { Provision(Context) error }
type Validator interface    { Validate() error }
type CleanerUpper interface { Cleanup() error }  // must work after partial Provision
```

The host calls these in order and always calls `Cleanup` if `Provision` was
called, regardless of whether it succeeded.

---

## 2. Plugin Registration

Prefer explicit registry assembly. It keeps ownership visible, avoids hidden
import-time mutation, and lets tests build isolated registries.

### Explicit registry assembly

```go
type Registry struct {
    modules map[string]ModuleInfo
}

func NewRegistry() *Registry {
    return &Registry{modules: make(map[string]ModuleInfo)}
}

func (r *Registry) RegisterModule(instance Module) {
    info := instance.ModuleInfo()
    if info.ID == "" {
        panic("module ID missing")
    }
    if _, ok := r.modules[string(info.ID)]; ok {
        panic(fmt.Sprintf("module already registered: %s", info.ID))
    }
    r.modules[string(info.ID)] = info
}

func RegisterStandardModules(r *Registry) {
    auth.Register(r)
    cache.Register(r)
    logging.Register(r)
}
```

Each plugin package exports an explicit registration function:

```go
func Register(r *Registry) {
    r.RegisterModule(MyPlugin{})
}

type MyPlugin struct{}

func (MyPlugin) ModuleInfo() ModuleInfo {
    return ModuleInfo{
        ID:  "http.handlers.myplugin",
        New: func() Module { return new(MyPlugin) },
    }
}
```

### Rare init() self-registration exception

Avoid `init()` for plugin registration in application and internal platform
code. It is hidden inversion of control: importing a package mutates global
process state.

Allow `init()` self-registration only when supporting an established
third-party plugin ecosystem whose documented composition model is blank
imports. Even then, `init()` may only register immutable metadata or factory
descriptors. It must not read config, perform I/O, start goroutines, create
clients, open files, log, or construct live plugin instances.

```go
func init() {
    RegisterModuleInfo(ModuleInfo{
        ID:  "http.handlers.myplugin",
        New: func() Module { return new(MyPlugin) },
    })
}
```

### Blank-import composition

Use only for ecosystem compatibility. A single `imports.go` controls which
plugins are compiled in:

```go
package standard

import (
    _ "example.com/modules/auth"
    _ "example.com/modules/cache"
    _ "example.com/modules/logging"
)
```

The main binary imports this package. Adding or removing a plugin is then a
build-composition change, not runtime configuration.

---

## 3. Sealed Factory Interfaces

Add an unexported method to prevent external implementations of a factory
interface. All instances must come through the package's constructor.

```go
type Factory interface {
    Type() Type
    CreateDefaultConfig() Config
    unexportedFactoryFunc() // prevents external implementations
}

type factory struct {
    cfgType          Type
    createDefaultCfg func() Config
}

func (f *factory) Type() Type                  { return f.cfgType }
func (f *factory) CreateDefaultConfig() Config { return f.createDefaultCfg() }
func (f *factory) unexportedFactoryFunc()      {}

func NewFactory(typ Type, defaultCfg func() Config, opts ...FactoryOption) Factory {
    f := &factory{cfgType: typ, createDefaultCfg: defaultCfg}
    for _, opt := range opts {
        opt.apply(f)
    }
    return f
}
```

New methods can be added to the unexported `factory` struct without breaking
external code. The compiler enforces that all factories go through `NewFactory`.

---

## 4. API Evolution Safety

### Prevent positional initialization

Add `_ struct{}` as the last field in public config structs:

```go
type Config struct {
    Host    string
    Port    int
    Timeout time.Duration
    _       struct{} // forces named fields; safe to add new fields
}
```

### Enforce construction through ParseConfig

The pgx pattern: an unexported field proves the config was built correctly.

```go
type ConnConfig struct {
    Host                 string
    Port                 uint16
    createdByParseConfig bool // unexported; set only by ParseConfig
}

func ParseConfig(connString string) (*ConnConfig, error) {
    cfg := &ConnConfig{} // ... parse and validate ...
    cfg.createdByParseConfig = true
    return cfg, nil
}

func ConnectConfig(ctx context.Context, cfg *ConnConfig) (*Conn, error) {
    if !cfg.createdByParseConfig {
        panic("config must be created by ParseConfig") // programmer bug, not runtime
    }
    // ...
}
```

---

## 5. Two-Phase Commit for System Resources

Separate preparation (reversible) from activation (point of no return). The
caller inspects prepared state before committing.

```go
func LoadResources(cfg Config) (commit func(), cleanup func(), err error) {
    var resources []*Resource

    // Phase 1: prepare (reversible)
    for _, spec := range cfg.Specs {
        r, err := prepare(spec)
        if err != nil {
            for _, prev := range resources {
                prev.Close()
            }
            return nil, nil, fmt.Errorf("prepare %s: %w", spec.Name, err)
        }
        resources = append(resources, r)
    }

    cleanup = func() {
        for _, r := range resources {
            r.Close()
        }
    }
    commit = func() {
        // Phase 2: activate (point of no return)
        for _, r := range resources {
            r.Activate()
        }
    }
    return commit, cleanup, nil
}
```

This avoids "half-activated" state where some resources are live and others
failed. Cilium uses this for BPF map loading; OpenTofu uses it for provider
plugin initialization.

---

## 6. Typed Error Classification

The containerd errdefs pattern: a closed set of sentinel error types with
marker interface methods, mapped 1:1 to gRPC/HTTP status codes.

### Define the error set

```go
var (
    ErrNotFound        = errNotFound{}
    ErrAlreadyExists   = errAlreadyExists{}
    ErrInvalidArgument = errInvalidArgument{}
    ErrInternal        = errInternal{}
)

type errNotFound struct{}
func (errNotFound) Error() string { return "not found" }
func (errNotFound) NotFound()     {} // marker method

// Interface for cross-package matching without importing the sentinel.
type notFound interface{ NotFound() }

func IsNotFound(err error) bool {
    return errors.Is(err, ErrNotFound) || isInterface[notFound](err)
}
```

The marker interface lets plugins define their own error types that satisfy
`NotFound()` without importing the sentinel package.

### Bidirectional mapping at boundaries

```go
func toGRPCStatus(err error) *status.Status {
    switch {
    case IsNotFound(err):        return status.New(codes.NotFound, err.Error())
    case IsInvalidArgument(err): return status.New(codes.InvalidArgument, err.Error())
    case IsAlreadyExists(err):   return status.New(codes.AlreadyExists, err.Error())
    default:                     return status.New(codes.Internal, err.Error())
    }
}

func fromGRPCStatus(err error) error {
    st, ok := status.FromError(err)
    if !ok { return err }
    switch st.Code() {
    case codes.NotFound:        return fmt.Errorf("%s: %w", st.Message(), ErrNotFound)
    case codes.InvalidArgument: return fmt.Errorf("%s: %w", st.Message(), ErrInvalidArgument)
    default:                    return fmt.Errorf("%s: %w", st.Message(), ErrInternal)
    }
}
```

Without `fromGRPCStatus`, clients receive raw gRPC errors that domain code
cannot match with `errors.Is`.

---

## Decision Table

| Question | Answer |
|---|---|
| How do I register plugins? | Prefer explicit `Register(r *Registry)` calls during assembly. |
| When is `init()` registration acceptable? | Only for established blank-import plugin ecosystems, and only for immutable metadata/factories. |
| How do I compose a default build? | Explicit registration by default; blank imports only for ecosystem compatibility. |
| How do I prevent external Factory implementations? | Unexported method on the interface. |
| How do I make config structs safe to extend? | `_ struct{}` as last field prevents positional initialization. |
| How do I enforce config construction? | Unexported `createdByParseConfig` field, panic if false. |
| How do I avoid half-activated resources? | Two-phase commit: prepare, then commit or cleanup. |
| How do I classify plugin errors? | Closed set of sentinel types with marker interfaces. Map to gRPC/HTTP at boundaries. |
| When does Cleanup run? | Always, even if Provision partially failed. |
| Is Shutdown idempotent? | Yes. Safe to call without Start, safe to call twice. |

---

## Anti-Patterns

- **`init()` self-registration in ordinary application code.** It hides
  ownership and mutates global state during import. Prefer explicit registry
  assembly.
- **I/O in init().** The rare ecosystem self-registration exception must be
  deterministic and side-effect-free. Connections, file reads, config reads,
  logging, and goroutines belong in `Provision`/`Start`.
- **Silently ignoring duplicate registration.** Panic on duplicates. A silent
  overwrite means the wrong plugin runs and the developer gets no signal.
- **Exported Factory interface without sealed method.** External implementations
  break when methods are added. Seal with an unexported method.
- **Positional struct initialization in public APIs.** Adding a field breaks all
  callers. Use `_ struct{}` to force named fields.
- **Cleanup that skips partial state.** `Cleanup` must handle nil fields and
  zero values. A half-provisioned module still needs resource release.
- **Blocking Start.** `Start` must return promptly. Long-running work goes into
  a goroutine with a cancellable context.
- **Non-idempotent Shutdown.** The framework may call `Shutdown` without
  `Start`, or call it twice during error recovery. Guard with `sync.Once` or
  nil checks.
- **String-based error classification.** Parsing error messages to determine
  status codes is fragile. Use typed sentinels with marker interfaces.
- **Leaking gRPC status types into domain code.** Domain packages must not
  import `google.golang.org/grpc/status`. Map at the boundary.
- **One-way error mapping.** Without `fromGRPCStatus`, the client receives raw
  gRPC errors that domain code cannot match with `errors.Is`.
- **Global mutable state beyond an ecosystem registry.** Plugin instances must
  receive their dependencies through `Provision`/`Start`, not package-level
  variables.
