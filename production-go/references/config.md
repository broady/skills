# Configuration

What belongs in config, how to load it, how to validate it.

## Contents

- [Philosophy](#philosophy)
- [What belongs in config](#what-belongs-in-config)
- [Default approach](#default-approach)
- [The Secret type](#the-secret-type)
- [Validation](#validation)
- [Complete example](#complete-example)
- [When to deviate](#when-to-deviate)
- [Config file vs env vars](#config-file-vs-env-vars)
- [Per-environment variation](#per-environment-variation)

---

## Philosophy

Configuration is for things that **actually differ between deployments**. Not
for things that *could theoretically* be tuned. The test:

> If this value is the same in dev, staging, and prod -- it is not config.
> It is an engineering decision. Put it in code.

HTTP server timeouts, DB pool lifetimes, retry backoff durations, idle timeouts,
max header sizes -- these are correctness defaults chosen once based on protocol
semantics and expected workload. They belong in code as constants or struct
literals. They graduate to config only when:

1. You have a concrete operational reason to vary them between deployments.
2. An operator (not a developer) needs to tune them without recompilation.
3. They differ between deployment targets (e.g., pool size for different machine sizes).

Exposing every knob as an env var creates a false sense of flexibility while
making the service harder to reason about. A Kubernetes manifest with 15 env
vars for timeouts nobody changes is not "configurable" -- it is noisy.

**Operational tuning knobs start in code and graduate to config when proven
necessary.** This is the opposite of "expose everything by default."

---

## What belongs in config

**Always config** (differs per deployment):

| Value | Why |
|---|---|
| Listen address | Different per environment, port conflicts |
| Database URL | Always different; is a secret |
| Downstream service URLs | Different per environment |
| Feature flags | Rolled out incrementally |
| Log level | Debug in dev, info in prod |

**Sometimes config** (when you have a concrete reason):

| Value | Graduate when... |
|---|---|
| DB pool max connections | Different machine sizes or scaling tiers |
| Worker concurrency | CPU-bound work on varied hardware |
| Shutdown timeout | Different deployment orchestrators have different drain windows |

**Almost never config** (engineering decisions):

| Value | Why it stays in code |
|---|---|
| HTTP `ReadHeaderTimeout` | Correctness: 5s is right for your protocol |
| HTTP `WriteTimeout` | Correctness: based on your slowest endpoint |
| HTTP `IdleTimeout` | Correctness: matches your load balancer |
| Retry backoff intervals | Chosen to match downstream SLOs |
| TLS handshake timeout | Physical constraint, rarely varies |
| Max header bytes | Security boundary, not a knob |
| `MaxConnLifetime` | Correctness: prevents stale connections |

If you find yourself wanting to change `ReadHeaderTimeout` between staging and
prod, you probably have a bug. Fix the bug, don't add a config value.

---

## Default approach

One `Config` struct. One `LoadConfig()`. Explicit `Validate()`.

- **Kong** owns the CLI envelope: `--config`, `--log-level`, subcommands.
- **`LoadConfig()`** owns service configuration: defaults, optional file, env/secret overlay, validation.
- **Constructors** receive the values they need, not the whole config struct.

Source precedence:

```text
1. Code defaults (in struct literal or DefaultConfig())
2. Optional config file (--config flag)
3. Environment variables / secret files
4. Validation
5. Construct dependencies with resolved values
```

---

## The Secret type

Secrets are not strings. They should not appear in logs, error messages, or
debug endpoints.

```go
// Secret holds a sensitive value. Its String() method returns "<redacted>"
// to prevent accidental logging. Access the value explicitly via Value().
type Secret struct {
    value string
}

func (s Secret) Value() string { return s.value }

func (s Secret) String() string {
    if s.value == "" {
        return ""
    }
    return "<redacted>"
}

func (s Secret) MarshalText() ([]byte, error) {
    return []byte("<redacted>"), nil
}

func (s *Secret) UnmarshalText(text []byte) error {
    s.value = string(text)
    return nil
}
```

Loading convention:

```text
APP_DB_URL=postgres://...            # direct value (acceptable for dev)
APP_DB_URL_FILE=/run/secrets/db_url  # path to secret file (preferred for prod)
```

If both are set, fail. Ambiguous secret precedence is a bug.

---

## Validation

Validation is for **semantic correctness**, not for checking that `ReadTimeout >
0` (that is a programming error if you set it to zero in your own code).

Validate things the **deployment operator** controls:

```go
func (c Config) Validate() error {
    var errs []error
    if c.DatabaseURL.Value() == "" {
        errs = append(errs, errors.New("database_url is required"))
    }
    if c.PaymentsURL == "" {
        errs = append(errs, errors.New("payments_url is required"))
    }
    if c.MaxWorkers < 1 {
        errs = append(errs, errors.New("max_workers must be >= 1"))
    }
    return errors.Join(errs...)
}
```

Do not validate hardcoded engineering defaults. If `WriteTimeout` is always
`30s` in your code, there is nothing to validate.

---

## Complete example

A realistic service that talks to a database and one downstream API:

```go
package main

import (
    "context"
    "database/sql"
    "errors"
    "fmt"
    "log/slog"
    "net"
    "net/http"
    "os"
    "strings"
    "syscall"
    "time"

    "github.com/alecthomas/kong"
    _ "github.com/jackc/pgx/v5/stdlib"
    "github.com/oklog/run"
)

// --- CLI (Kong owns flags and commands only) ---

type CLI struct {
    Config   string `type:"path" help:"Path to YAML config file."`
    LogLevel string `default:"info" enum:"debug,info,warn,error" help:"Log level."`

    Serve ServeCmd `cmd:"" default:"withargs" help:"Run the server."`
}

type ServeCmd struct{}

func (c *ServeCmd) Run(app *App) error {
    return runServer(app.Config, app.Logger)
}

type App struct {
    Config *Config
    Logger *slog.Logger
}

func main() {
    var cli CLI
    ctx := kong.Parse(&cli)

    cfg, err := LoadConfig(cli.Config)
    if err != nil {
        fmt.Fprintf(os.Stderr, "config: %v\n", err)
        os.Exit(1)
    }

    logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
        Level: parseLogLevel(cli.LogLevel),
    }))

    if err := ctx.Run(&App{Config: cfg, Logger: logger}); err != nil {
        logger.LogAttrs(context.Background(), slog.LevelError, "exit", slog.Any("err", err))
        os.Exit(1)
    }
}

func parseLogLevel(s string) slog.Level {
    var l slog.Level
    _ = l.UnmarshalText([]byte(s))
    return l
}

// --- Config (what actually varies between deployments) ---

type Config struct {
    Addr        string // listen address
    DatabaseURL Secret // connection string
    PaymentsURL string // downstream service base URL

    // Graduated to config because pool size depends on deployment tier.
    DBMaxConns int
}

func (c Config) Validate() error {
    var errs []error
    if c.Addr == "" {
        errs = append(errs, errors.New("addr is required"))
    }
    if c.DatabaseURL.Value() == "" {
        errs = append(errs, errors.New("database_url is required"))
    }
    if c.PaymentsURL == "" {
        errs = append(errs, errors.New("payments_url is required"))
    }
    if c.DBMaxConns < 1 {
        errs = append(errs, errors.New("db_max_conns must be >= 1"))
    }
    return errors.Join(errs...)
}

func LoadConfig(configPath string) (*Config, error) {
    // Start with code defaults.
    cfg := &Config{
        Addr:       ":8080",
        DBMaxConns: 16,
    }

    // File overlay (if provided).
    if configPath != "" {
        if err := loadConfigFile(configPath, cfg); err != nil {
            return nil, fmt.Errorf("load config file: %w", err)
        }
    }

    // Env overlay (deployment-specific values).
    envStr("ADDR", &cfg.Addr)
    envStr("PAYMENTS_URL", &cfg.PaymentsURL)
    envInt("DB_MAX_CONNS", &cfg.DBMaxConns)

    // Secrets: support both direct value and _FILE convention.
    if err := envSecret("DATABASE_URL", &cfg.DatabaseURL); err != nil {
        return nil, err
    }

    if err := cfg.Validate(); err != nil {
        return nil, err
    }

    return cfg, nil
}

// --- Env helpers (tiny, no framework needed) ---

func envStr(name string, dst *string) {
    if v, ok := os.LookupEnv(name); ok {
        *dst = v
    }
}

func envInt(name string, dst *int) {
    if v, ok := os.LookupEnv(name); ok {
        fmt.Sscanf(v, "%d", dst)
    }
}

func envSecret(name string, dst *Secret) error {
    val, hasVal := os.LookupEnv(name)
    path, hasFile := os.LookupEnv(name + "_FILE")

    if hasVal && hasFile {
        return fmt.Errorf("%s and %s_FILE are mutually exclusive", name, name)
    }
    if hasFile {
        b, err := os.ReadFile(path)
        if err != nil {
            return fmt.Errorf("read %s_FILE: %w", name, err)
        }
        dst.value = strings.TrimRight(string(b), "\r\n")
        return nil
    }
    if hasVal {
        dst.value = val
    }
    return nil
}

// --- Server (engineering decisions live here, not in config) ---

func runServer(cfg *Config, logger *slog.Logger) error {
    db, err := sql.Open("pgx", cfg.DatabaseURL.Value())
    if err != nil {
        return fmt.Errorf("open db: %w", err)
    }
    defer db.Close()

    // Engineering decisions: these are correct for our protocol and workload.
    // They do not vary between deployments.
    db.SetMaxOpenConns(cfg.DBMaxConns)
    db.SetConnMaxLifetime(30 * time.Minute)
    db.SetConnMaxIdleTime(5 * time.Minute)

    if err := db.PingContext(context.Background()); err != nil {
        return fmt.Errorf("ping db: %w", err)
    }

    paymentsClient := &http.Client{
        Timeout: 5 * time.Second,
        Transport: &http.Transport{
            MaxConnsPerHost:     20,
            MaxIdleConnsPerHost: 20,
            IdleConnTimeout:     90 * time.Second,
        },
    }

    mux := http.NewServeMux()
    // Wire handlers with db, paymentsClient, cfg.PaymentsURL, logger...
    _ = paymentsClient
    mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
        fmt.Fprintln(w, "ok")
    })

    srv := &http.Server{
        Handler: mux,
        // Engineering decisions, not config:
        ReadHeaderTimeout: 5 * time.Second,
        ReadTimeout:       15 * time.Second,
        WriteTimeout:      30 * time.Second,
        IdleTimeout:       2 * time.Minute,
        MaxHeaderBytes:    1 << 20,
    }

    var g run.Group

    ln, err := net.Listen("tcp", cfg.Addr)
    if err != nil {
        return fmt.Errorf("listen %s: %w", cfg.Addr, err)
    }
    g.Add(func() error {
        logger.LogAttrs(context.Background(), slog.LevelInfo, "serving",
            slog.String("addr", ln.Addr().String()),
        )
        if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
            return err
        }
        return nil
    }, func(error) {
        ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
        defer cancel()
        if err := srv.Shutdown(ctx); err != nil {
            _ = srv.Close()
        }
    })

    g.Add(run.SignalHandler(context.Background(), syscall.SIGTERM, syscall.SIGINT))

    if err := g.Run(); err != nil {
        var se run.SignalError
        if errors.As(err, &se) {
            return nil
        }
        return fmt.Errorf("run: %w", err)
    }
    return nil
}
```

Notice what is NOT in `Config`:
- `ReadHeaderTimeout`, `WriteTimeout`, `IdleTimeout` -- engineering decisions
- `ConnMaxLifetime`, `ConnMaxIdleTime` -- engineering decisions
- `MaxHeaderBytes` -- security boundary
- `ShutdownTimeout` -- could graduate, but 15s is correct for most orchestrators
- HTTP client timeout, transport settings -- protocol-specific engineering

The config struct has **4 fields**. A reviewer looks at it and immediately knows:
"this service listens on an address, connects to a database, and calls a
payments API."

---

## When to deviate

### Graduating a value to config

When you have a concrete operational reason:

```go
// Before: engineering decision
db.SetMaxOpenConns(16)

// After: graduated because we autoscale from 1 to 32 cores
// and pool size should track deployment tier.
db.SetMaxOpenConns(cfg.DBMaxConns)
```

Add it to the struct, add a default, add validation, add a single env var.
Do not pre-emptively graduate values "in case someone needs to tune it."

### Large services with many downstreams

When you have 5+ downstreams with different URLs per environment, a config file
becomes justified. The file provides structure; env vars override individual
values:

```yaml
# config.yaml -- reviewed and deployed per environment
addr: ":8080"
payments_url: "https://payments.internal"
inventory_url: "https://inventory.internal"
shipping_url: "https://shipping.internal"
```

```sh
# Deployment override
DATABASE_URL_FILE=/run/secrets/db_url
```

### Env-only (small services, tools)

For tiny services where a config file is overkill, use `caarlos0/env` or
`sethvargo/go-envconfig` with struct tags. Still call `Validate()` afterward:

```go
type Config struct {
    Addr        string `env:"ADDR" envDefault:":8080"`
    DatabaseURL string `env:"DATABASE_URL,required"`
}
```

### CLI-first programs

When flags ARE the product interface (not service config), let Kong own more:

```go
type CLI struct {
    Output string `short:"o" default:"-" help:"Output file."`
    Format string `enum:"json,csv" default:"json" help:"Output format."`
    Run    RunCmd `cmd:"" help:"Execute the export."`
}
```

---

## Config file vs env vars

| Use env vars when... | Use a config file when... |
|---|---|
| Small, flat config (3-5 values) | Structured, nested, 10+ values |
| Pure Twelve-Factor deployment | Team-reviewed deployment manifests |
| Single deployment target | Multiple named environments with shared structure |
| Quick iteration in dev | Operational documentation that lives in version control |

The hybrid: **file for structure, env for deployment-specific overrides and
secrets.**

---

## Per-environment variation

Same binary. Same `Config` type. Different deployment inputs.

```text
dev:     ADDR=:8080  DATABASE_URL=postgres://localhost/app  PAYMENTS_URL=http://localhost:9090
staging: ADDR=:8080  DATABASE_URL_FILE=/run/secrets/db      PAYMENTS_URL=https://payments.staging.internal
prod:    ADDR=:8080  DATABASE_URL_FILE=/run/secrets/db      PAYMENTS_URL=https://payments.internal
```

Never:

```go
if os.Getenv("ENV") == "prod" {
    // ...
}
```

"prod", "staging", "dev" are deployment concerns. The application does not know
or care which environment it is in. It receives addresses, credentials, and
feature flags -- that is all.
