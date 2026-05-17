# Server Scaffold

CLI setup, configuration loading, and complete main.go with shutdown flow.

## Contents

- [CLI and Configuration](#cli-and-configuration)
- [Complete main.go](#complete-maingo)
- [Shutdown flow: multi-phase](#shutdown-flow-multi-phase)

## CLI and Configuration

Project default: use `github.com/alecthomas/kong` for new CLIs. Kong owns
command and flag parsing only.

Clean split:
- Kong parses flags such as `--config`, `--log-level`, and subcommands.
- `LoadConfig` reads env, files, and secrets once.
- Commands receive the parsed `*Config` from construction or `BeforeApply`.

For the full config loading philosophy (what belongs in config, what doesn't,
file + env overlay, secrets, validation), see [config.md](../config.md).

**Key principle:** config is for values that actually differ between deployments.
HTTP timeouts, pool lifetimes, max header sizes, and similar operational knobs
are engineering decisions -- they belong in code as constants. They graduate to
config only when a concrete operational reason demands per-deployment tuning.

```go
type CLI struct {
    Config   string `type:"path" help:"Path to config file."`
    LogLevel string `default:"info" enum:"debug,info,warn,error" help:"Log level."`

    Serve ServeCmd `cmd:"" default:"withargs" help:"Run the server."`
}

type ServeCmd struct{}

// Config holds values that actually differ between deployments.
// Engineering decisions (timeouts, pool lifetimes) stay in code.
type Config struct {
    Addr        string // listen address
    DatabaseURL Secret // connection string (secret)
    PaymentsURL string // downstream service

    // Graduated: pool size depends on deployment tier.
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

type App struct {
    Config *Config
    Logger *slog.Logger
}

func (c *ServeCmd) Run(app *App) error {
    return runServer(app.Config, app.Logger)
}
```

## Complete main.go

`main()` owns exactly one process exit. Kong parses commands and flags, then the
selected command calls into the server runner with a parsed `*Config`.
`oklog/run.Group` coordinates subsystem lifecycles. Each `Add` takes an execute
func and an interrupt func. When ANY execute returns, all other actors'
interrupt funcs are called. `Run` then waits for all to finish.

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

// --- Config: what actually varies between deployments ---

func LoadConfig(configPath string) (*Config, error) {
    cfg := &Config{
        Addr:       ":8080",
        DBMaxConns: 16,
    }

    if configPath != "" {
        if err := loadConfigFile(configPath, cfg); err != nil {
            return nil, fmt.Errorf("load config file: %w", err)
        }
    }

    // Env overlay: only values that differ per deployment.
    envStr("ADDR", &cfg.Addr)
    envStr("PAYMENTS_URL", &cfg.PaymentsURL)
    envInt("DB_MAX_CONNS", &cfg.DBMaxConns)

    if err := envSecret("DATABASE_URL", &cfg.DatabaseURL); err != nil {
        return nil, err
    }

    if err := cfg.Validate(); err != nil {
        return nil, err
    }

    return cfg, nil
}

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

// --- Server ---

func runServer(cfg *Config, logger *slog.Logger) error {
    // --- Dependency wiring ---
    db, err := connectDB(context.Background(), cfg)
    if err != nil {
        return fmt.Errorf("connect database: %w", err)
    }
    defer db.Close()

    userStore := NewUserStore(db)
    orderSvc := NewOrderService(logger, userStore)
    httpSrv := buildHTTPServer(logger, orderSvc)

    // --- Run group ---
    var g run.Group

    // Actor: HTTP server
    ln, err := net.Listen("tcp", cfg.Addr)
    if err != nil {
        return fmt.Errorf("listen %s: %w", cfg.Addr, err)
    }
    g.Add(func() error {
        logger.LogAttrs(context.Background(), slog.LevelInfo, "serving",
            slog.String("addr", ln.Addr().String()),
        )
        if err := httpSrv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
            return fmt.Errorf("serve http: %w", err)
        }
        return nil
    }, func(error) {
        // Drain then hammer. See "Shutdown flow" below.
        ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
        defer cancel()
        if err := httpSrv.Shutdown(ctx); err != nil {
            logger.LogAttrs(context.Background(), slog.LevelWarn,
                "graceful shutdown timed out, forcing close",
                slog.Any("err", err),
            )
            _ = httpSrv.Close()
        }
    })

    // Actor: Signal handler
    g.Add(run.SignalHandler(context.Background(), syscall.SIGTERM, syscall.SIGINT))

    // Actor: Background worker (uncomment as needed)
    // ctx, cancel := context.WithCancel(context.Background())
    // g.Add(func() error { return worker.Run(ctx) }, func(error) { cancel() })

    if err := g.Run(); err != nil {
        var se run.SignalError
        if errors.As(err, &se) {
            logger.LogAttrs(context.Background(), slog.LevelInfo, "shutdown complete")
            return nil
        }
        return fmt.Errorf("run: %w", err)
    }
    return nil
}

func connectDB(ctx context.Context, cfg *Config) (*sql.DB, error) {
    db, err := sql.Open("pgx", cfg.DatabaseURL.Value())
    if err != nil {
        return nil, fmt.Errorf("open db: %w", err)
    }

    // Engineering decisions: these are correct for our workload.
    // They do not vary between deployments.
    db.SetMaxOpenConns(cfg.DBMaxConns) // graduated: varies by deployment tier
    db.SetConnMaxLifetime(30 * time.Minute)
    db.SetConnMaxIdleTime(5 * time.Minute)

    if err := db.PingContext(ctx); err != nil {
        _ = db.Close()
        return nil, fmt.Errorf("ping db: %w", err)
    }
    return db, nil
}

func buildHTTPServer(logger *slog.Logger, orderSvc *OrderService) *http.Server {
    mux := http.NewServeMux()
    mux.Handle("GET /api/orders/{id}", handle(logger, decodePathID("id"), orderSvc.Get))
    mux.Handle("POST /api/orders", handle(logger, decodeJSON[CreateOrderRequest](1<<20), orderSvc.Create))
    mux.HandleFunc("GET /healthz", handleHealthz())
    mux.HandleFunc("GET /readyz", handleReadyz(orderSvc))

    handler := withRequestID(withLogging(logger, mux))

    // Engineering decisions: chosen for correctness, not tuning.
    return &http.Server{
        Handler:           handler,
        ReadHeaderTimeout: 5 * time.Second,
        ReadTimeout:       15 * time.Second,
        WriteTimeout:      30 * time.Second,
        IdleTimeout:       2 * time.Minute,
        MaxHeaderBytes:    1 << 20,
    }
}
```

## Shutdown flow: multi-phase

Signal -> **Drain** (HTTP `Shutdown` drains in-flight, workers finish current
item) -> **Hammer** (force-close anything still running after deadline) ->
**Terminate** (close DB, flush telemetry) -> exit 0.

The hammer phase prevents the production bug where one hung connection blocks
shutdown forever:

```go
func(error) {
    // Phase 1: Drain -- give in-flight requests time to complete.
    ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
    defer cancel()
    if err := httpSrv.Shutdown(ctx); err != nil {
        // Phase 2: Hammer -- drain timed out, force close.
        logger.LogAttrs(context.Background(), slog.LevelWarn,
            "graceful shutdown timed out, forcing close",
            slog.Any("err", err),
        )
        _ = httpSrv.Close()
    }
}
```

For servers with long-lived connections (WebSocket, SSE), the hammer phase is
essential. Without it, a single idle connection holds the process open
indefinitely.

The shutdown timeout (15s above) is an engineering decision for most services.
Graduate it to config only if your orchestrator's drain window varies between
deployments (e.g., ECS vs bare-metal have different SIGTERM grace periods).
