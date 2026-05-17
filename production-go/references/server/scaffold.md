# Server Scaffold

CLI setup, configuration loading, and complete main.go with shutdown flow.

## Contents

- [CLI and Configuration](#cli-and-configuration)
- [Complete main.go](#complete-maingo)
- [Shutdown flow: multi-phase](#shutdown-flow-multi-phase)

## CLI and Configuration

Project default: use `github.com/alecthomas/kong` for new CLIs. Kong owns
command and flag parsing.
The config package owns env vars, config files, and secrets. Do not use Kong
`env:"..."` tags for application config values if the config package also reads
env; two sources claiming `DATABASE_URL` will eventually disagree.

Clean split:
- Kong parses flags such as `--config`, `--log-level`, and subcommands.
- `loadConfig` reads env, files, and secrets once.
- Commands receive the parsed `*Config` from construction or `BeforeApply`.

```go
type CLI struct {
	ConfigPath string `help:"Path to config file." type:"path"`
	LogLevel   string `help:"Log level override." enum:"debug,info,warn,error"`

	Serve ServeCmd `cmd:"" help:"Run the server."`
}

type ServeCmd struct{}

type Config struct {
	HTTPAddr              string
	GRPCAddr              string
	DatabaseURL           string
	DBMaxConns            int32
	DBMaxConnLifetime     time.Duration
	DBMaxConnIdleTime     time.Duration
	LogLevel              slog.Level
	ShutdownTimeout       time.Duration
	HTTPReadHeaderTimeout time.Duration
	HTTPReadTimeout       time.Duration
	HTTPWriteTimeout      time.Duration
	HTTPIdleTimeout       time.Duration
	HTTPMaxBodyBytes      int64
}

func (c Config) Validate() error {
	if c.DatabaseURL == "" {
		return fmt.Errorf("database url required")
	}
	if c.DBMaxConns <= 0 {
		return fmt.Errorf("db max conns must be positive")
	}
	if c.DBMaxConnLifetime <= 0 {
		return fmt.Errorf("db max conn lifetime must be positive")
	}
	if c.DBMaxConnIdleTime <= 0 {
		return fmt.Errorf("db max conn idle time must be positive")
	}
	return nil
}

type App struct {
	Config *Config
	Logger *slog.Logger
}

func (c *ServeCmd) Run(app *App) error {
	return runServer(app.Config, app.Logger)
}
```

The full `main()`, `runCLI()`, and `loadConfig()` are shown in the complete
scaffold below.

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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/alecthomas/kong"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/oklog/run"
)

func main() {
	if err := runCLI(); err != nil {
		logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
		logger.LogAttrs(context.Background(), slog.LevelError, "exit", slog.Any("err", err))
		os.Exit(1)
	}
}

func runCLI() error {
	var cli CLI
	ctx := kong.Parse(&cli)

	cfg, err := loadConfig(cli.ConfigPath, cli.LogLevel)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("validate config: %w", err)
	}
	logger := newLogger(cfg)

	if err := ctx.Run(&App{
		Config: cfg,
		Logger: logger,
	}); err != nil {
		return fmt.Errorf("run command: %w", err)
	}
	return nil
}

func loadConfig(configPath, logLevelOverride string) (*Config, error) {
	if configPath != "" {
		return nil, fmt.Errorf("config file loading not wired: %s", configPath)
	}
	logLevel, err := parseLogLevel(envString("LOG_LEVEL", "info"))
	if err != nil {
		return nil, fmt.Errorf("parse log level: %w", err)
	}
	if logLevelOverride != "" {
		logLevel, err = parseLogLevel(logLevelOverride)
		if err != nil {
			return nil, fmt.Errorf("parse log level override: %w", err)
		}
	}
	dbMaxConns, err := envInt32("DB_MAX_CONNS", 20)
	if err != nil {
		return nil, err
	}
	dbMaxConnLifetime, err := envDuration("DB_MAX_CONN_LIFETIME", 30*time.Minute)
	if err != nil {
		return nil, err
	}
	dbMaxConnIdleTime, err := envDuration("DB_MAX_CONN_IDLE_TIME", 5*time.Minute)
	if err != nil {
		return nil, err
	}
	shutdownTimeout, err := envDuration("SHUTDOWN_TIMEOUT", 30*time.Second)
	if err != nil {
		return nil, err
	}
	httpReadHeaderTimeout, err := envDuration("HTTP_READ_HEADER_TIMEOUT", 5*time.Second)
	if err != nil {
		return nil, err
	}
	httpReadTimeout, err := envDuration("HTTP_READ_TIMEOUT", 15*time.Second)
	if err != nil {
		return nil, err
	}
	httpWriteTimeout, err := envDuration("HTTP_WRITE_TIMEOUT", 30*time.Second)
	if err != nil {
		return nil, err
	}
	httpIdleTimeout, err := envDuration("HTTP_IDLE_TIMEOUT", 2*time.Minute)
	if err != nil {
		return nil, err
	}
	httpMaxBodyBytes, err := envInt64("HTTP_MAX_BODY_BYTES", 1<<20)
	if err != nil {
		return nil, err
	}
	cfg := &Config{
		HTTPAddr:              envString("HTTP_ADDR", ":8080"),
		GRPCAddr:              envString("GRPC_ADDR", ":8081"),
		DatabaseURL:           envString("DATABASE_URL", ""),
		DBMaxConns:            dbMaxConns,
		DBMaxConnLifetime:     dbMaxConnLifetime,
		DBMaxConnIdleTime:     dbMaxConnIdleTime,
		LogLevel:              logLevel,
		ShutdownTimeout:       shutdownTimeout,
		HTTPReadHeaderTimeout: httpReadHeaderTimeout,
		HTTPReadTimeout:       httpReadTimeout,
		HTTPWriteTimeout:      httpWriteTimeout,
		HTTPIdleTimeout:       httpIdleTimeout,
		HTTPMaxBodyBytes:      httpMaxBodyBytes,
	}
	return cfg, nil
}

func newLogger(cfg *Config) *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: cfg.LogLevel,
	}))
}

func parseLogLevel(value string) (slog.Level, error) {
	var level slog.Level
	if err := level.UnmarshalText([]byte(value)); err != nil {
		return 0, fmt.Errorf("invalid slog level %q: %w", value, err)
	}
	return level, nil
}

func envString(name, fallback string) string {
	value, ok := os.LookupEnv(name)
	if !ok {
		return fallback
	}
	return value
}

func envInt64(name string, fallback int64) (int64, error) {
	value, ok := os.LookupEnv(name)
	if !ok {
		return fallback, nil
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse %s: %w", name, err)
	}
	return parsed, nil
}

func envInt32(name string, fallback int32) (int32, error) {
	value, ok := os.LookupEnv(name)
	if !ok {
		return fallback, nil
	}
	parsed, err := strconv.ParseInt(value, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("parse %s: %w", name, err)
	}
	return int32(parsed), nil
}

func envDuration(name string, fallback time.Duration) (time.Duration, error) {
	value, ok := os.LookupEnv(name)
	if !ok {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("parse %s: %w", name, err)
	}
	return parsed, nil
}

func runServer(cfg *Config, logger *slog.Logger) error {
	logger = logger.With("component", "server")

	// --- Dependency wiring (this IS the dependency graph) ---
	db, err := connectDB(context.Background(), cfg)
	if err != nil {
		return fmt.Errorf("connect database: %w", err)
	}
	defer db.Close()

	userStore := NewUserStore(db)
	orderSvc := NewOrderService(logger, userStore)
	httpSrv := buildHTTPServer(cfg, logger, orderSvc)

	// --- Run group ---
	var g run.Group

	// Actor: HTTP server
	{
		ln, err := net.Listen("tcp", cfg.HTTPAddr)
		if err != nil {
			return fmt.Errorf("listen http %s: %w", cfg.HTTPAddr, err)
		}
		g.Add(func() error {
			logger.LogAttrs(context.Background(), slog.LevelInfo, "http server starting",
				slog.String("addr", ln.Addr().String()),
			)
			if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				return fmt.Errorf("serve http: %w", err)
			}
			return nil
		}, func(error) {
			ctx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
			defer cancel()
			if err := httpSrv.Shutdown(ctx); err != nil {
				logger.LogAttrs(context.Background(), slog.LevelWarn,
					"graceful shutdown timed out, forcing close",
					slog.Any("err", err),
				)
				_ = httpSrv.Close() // best effort after graceful shutdown timeout
			}
		})
	}

	// Actor: Signal handler
	{
		ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
		g.Add(func() error {
			<-ctx.Done()
			logger.LogAttrs(context.Background(), slog.LevelInfo, "received shutdown signal")
			return nil
		}, func(error) {
			cancel()
		})
	}

	// Actor: Background worker (uncomment as needed)
	// ctx, cancel := context.WithCancel(context.Background())
	// g.Add(func() error { return runFlusher(ctx, logger) }, func(error) { cancel() })

	logger.LogAttrs(context.Background(), slog.LevelInfo, "starting",
		slog.String("http", cfg.HTTPAddr),
	)
	if err := g.Run(); err != nil {
		return fmt.Errorf("run server: %w", err)
	}
	logger.LogAttrs(context.Background(), slog.LevelInfo, "shutdown complete")
	return nil
}

func connectDB(ctx context.Context, cfg *Config) (*sql.DB, error) {
	db, err := sql.Open("pgx", cfg.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("open db: %v", err)
	}
	db.SetMaxOpenConns(int(cfg.DBMaxConns))
	db.SetConnMaxLifetime(cfg.DBMaxConnLifetime)
	db.SetConnMaxIdleTime(cfg.DBMaxConnIdleTime)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close() // best effort after failed startup ping
		return nil, fmt.Errorf("ping db: %v", err)
	}
	return db, nil
}
```

### Shutdown flow: multi-phase

Signal -> **Drain** (HTTP `Shutdown` drains in-flight, workers finish current item) -> **Hammer** (force-cancel anything still running after `ShutdownTimeout`) -> **Terminate** (close DB, flush telemetry) -> exit 0.

The hammer phase prevents the production bug where one hung connection blocks
shutdown forever. Implement it as a context with a hard deadline:

```go
// In the interrupt func of the HTTP actor:
func(error) {
    // Phase 1: Drain — give in-flight requests time to complete.
    ctx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
    defer cancel()
    if err := httpSrv.Shutdown(ctx); err != nil {
        // Phase 2: Hammer — drain timed out, force close.
        logger.LogAttrs(context.Background(), slog.LevelWarn,
            "graceful shutdown timed out, forcing close",
            slog.Any("err", err),
        )
        _ = httpSrv.Close() // best effort after graceful shutdown timeout
    }
}
```

For servers with long-lived connections (WebSocket, SSE, git smart HTTP), the
hammer phase is essential. Without it, a single idle connection holds the process
open indefinitely. Configure `ShutdownTimeout` via operational config (default
30s is reasonable for most services).
