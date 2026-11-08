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

See [config.md](../config.md) for the full philosophy on what belongs in config
vs code, the `Secret` type, validation guidance, and deviation patterns.

```go
type CLI struct {
	ConfigPath string `help:"Path to config file." type:"path"`
	LogLevel   string `help:"Log level override." enum:"debug,info,warn,error"`

	Serve ServeCmd `cmd:"" help:"Run the server."`
}

type ServeCmd struct{}

// Config holds values that actually vary between deployments. HTTP timeouts
// and DB pool lifetimes are engineering decisions -- they live in code as
// constants. The shutdown deadline is config because drain windows differ per
// orchestrator. See references/config.md for graduation criteria.
type Config struct {
	HTTPAddr          string
	GRPCAddr          string
	DatabaseURL       Secret
	DBMaxConns        int32
	LogLevel          slog.Level
	EnableReflection  bool          // gRPC reflection; see connect-grpc.md
	ShutdownTimeout   time.Duration // hard deadline for graceful shutdown
}

func (c Config) Validate() error {
	if c.DatabaseURL.Value() == "" {
		return fmt.Errorf("database url required")
	}
	if c.DBMaxConns <= 0 {
		return fmt.Errorf("db max conns must be positive")
	}
	if c.ShutdownTimeout <= 0 {
		return fmt.Errorf("shutdown timeout must be positive")
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

`main()` owns process exit. The only other exit is the panic fatal hook
`panicExit`, which must not wait for graceful shutdown. Kong parses commands and flags, then the
selected command calls into the server runner with a parsed `*Config`.
`oklog/run.Group` coordinates subsystem lifecycles. Each `Add` takes an execute
func and an interrupt func. When ANY execute returns, all other actors'
interrupt funcs are called. `Run` then waits for all to finish. Requires
`github.com/oklog/run` v1.2.0+ for `run.ErrSignal`.

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
	"strconv"
	"strings"
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
	dbMaxConns, err := envInt32("DB_MAX_CONNS", 16)
	if err != nil {
		return nil, err
	}
	var dbURL Secret
	if err := envSecret("DATABASE_URL", &dbURL); err != nil {
		return nil, err
	}
	enableReflection, err := envBool("ENABLE_REFLECTION", false)
	if err != nil {
		return nil, err
	}
	shutdownTimeout, err := envDuration("SHUTDOWN_TIMEOUT", 15*time.Second)
	if err != nil {
		return nil, err
	}
	cfg := &Config{
		HTTPAddr:         envString("HTTP_ADDR", ":8080"),
		GRPCAddr:         envString("GRPC_ADDR", ":8081"),
		DatabaseURL:      dbURL,
		DBMaxConns:       dbMaxConns,
		LogLevel:         logLevel,
		EnableReflection: enableReflection,
		ShutdownTimeout:  shutdownTimeout,
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

// envBool rejects malformed values instead of reading them as false.
func envBool(name string, fallback bool) (bool, error) {
	value, ok := os.LookupEnv(name)
	if !ok {
		return fallback, nil
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf("parse %s: %w", name, err)
	}
	return parsed, nil
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

// Secret prevents credentials from appearing in logs or debug output.
// See references/config.md for the full pattern including MarshalText.
type Secret struct{ value string }

func (s Secret) Value() string  { return s.value }
func (s Secret) String() string { if s.value == "" { return "" }; return "<redacted>" }

// envSecret loads a secret from NAME (direct value) or NAME_FILE (path to
// file). If both are set, it returns an error — ambiguous precedence is a bug.
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

func runServer(cfg *Config, logger *slog.Logger) error {
	logger = logger.With("component", "server")

	// --- Dependency wiring (this IS the dependency graph) ---
	db, err := connectDB(context.Background(), cfg)
	if err != nil {
		return fmt.Errorf("connect database: %w", err)
	}

	userStore := NewUserStore(db)
	orderSvc := NewOrderService(logger, userStore)
	httpSrv := buildHTTPServer(logger, orderSvc, panicExit(logger))

	// --- Run group ---
	var g run.Group

	// drained is false if the HTTP drain hit its deadline. run.Group calls
	// interrupts on this goroutine, so the write is visible after g.Run.
	drained := true

	// Actor: HTTP server
	{
		ln, err := net.Listen("tcp", cfg.HTTPAddr)
		if err != nil {
			// No handlers have run yet, so db.Close returns promptly.
			return errors.Join(fmt.Errorf("listen http %s: %w", cfg.HTTPAddr, err), db.Close())
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
				drained = false
				logger.LogAttrs(context.Background(), slog.LevelWarn,
					"graceful shutdown timed out, forcing close",
					slog.Any("err", err),
				)
				_ = httpSrv.Close() // best effort after graceful shutdown timeout
			}
		})
	}

	// Actor: Signal handler — run.SignalHandler returns a ready-made
	// (execute, interrupt) pair; no manual signal.NotifyContext needed.
	g.Add(run.SignalHandler(context.Background(), syscall.SIGTERM, syscall.SIGINT))

	// Actor: Background worker (uncomment as needed). It must return once ctx
	// is cancelled: run.Group cannot force a stuck actor (see lifecycle.md).
	// ctx, cancel := context.WithCancel(context.Background())
	// g.Add(func() error { return runFlusher(ctx, logger) }, func(error) { cancel() })

	logger.LogAttrs(context.Background(), slog.LevelInfo, "starting",
		slog.String("http", cfg.HTTPAddr),
	)
	// The signal actor returns run.ErrSignal on SIGTERM/SIGINT. That is a
	// normal stop (exit 0); any other error is a subsystem failure (exit 1).
	runErr := g.Run()
	if errors.Is(runErr, run.ErrSignal) {
		runErr = nil
	}

	// db.Close takes no context and waits for in-flight queries. After a drain
	// timeout, handlers may still hold some (Server.Close does not wait for
	// handlers), so bound the wait; process exit is the hammer for the rest.
	closeErr := closeWithin(dbCloseTimeout, db.Close)
	if runErr != nil {
		return fmt.Errorf("run server: %w", errors.Join(runErr, closeErr))
	}

	// Policy: a forced shutdown after a signal still exits 0, because the stop
	// was requested; it is logged as a warning. Return an error instead if
	// your deployment should alert on incomplete shutdowns.
	level, msg := slog.LevelInfo, "shutdown complete"
	if !drained || closeErr != nil {
		level, msg = slog.LevelWarn, "shutdown forced"
	}
	logger.LogAttrs(context.Background(), level, msg,
		slog.Bool("drained", drained),
		slog.Any("close_db_err", closeErr),
	)
	return nil
}

// dbCloseTimeout bounds db.Close after the HTTP drain. Total shutdown is at
// most SHUTDOWN_TIMEOUT + dbCloseTimeout; keep the orchestrator's grace period
// (Kubernetes terminationGracePeriodSeconds) above that.
const dbCloseTimeout = 5 * time.Second

// closeWithin bounds a close function that takes no context. On timeout the
// close goroutine is abandoned; process exit follows and ends it.
func closeWithin(d time.Duration, closeFn func() error) error {
	done := make(chan error, 1)
	go func() { done <- closeFn() }()
	select {
	case err := <-done:
		return err
	case <-time.After(d):
		return fmt.Errorf("close did not finish within %s", d)
	}
}

// panicExit is the fatal hook for Terminate (see errors.md): an unexpected
// panic exits immediately instead of draining on possibly corrupted state.
// The JSON handler writes synchronously, so the record lands before os.Exit.
func panicExit(logger *slog.Logger) func(string, any, []byte) {
	return func(requestID string, p any, stack []byte) {
		logger.LogAttrs(context.Background(), slog.LevelError, "panic",
			slog.String("request_id", requestID),
			slog.Any("value", p),
			slog.String("stack", string(stack)),
		)
		os.Exit(2)
	}
}

func connectDB(ctx context.Context, cfg *Config) (*sql.DB, error) {
	db, err := sql.Open("pgx", cfg.DatabaseURL.Value())
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	db.SetMaxOpenConns(int(cfg.DBMaxConns))
	// Engineering decisions: correct for our protocol and workload.
	// Graduate to config when pool lifetimes need to differ per deployment.
	db.SetConnMaxLifetime(30 * time.Minute)
	db.SetConnMaxIdleTime(5 * time.Minute)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close() // best effort after failed startup ping
		return nil, fmt.Errorf("ping db: %w", err)
	}
	return db, nil
}
```

### Shutdown flow: multi-phase

Signal -> **Drain** (HTTP `Shutdown` drains in-flight, workers finish current item) -> **Hammer** (force-close remaining HTTP connections after the shutdown deadline) -> **Terminate** (close DB within `dbCloseTimeout`, flush telemetry) -> exit 0. The final log line runs after cleanup and says whether shutdown was clean (`shutdown complete`) or forced (`shutdown forced`). A signal is a normal stop, so `runServer` returns nil for `run.ErrSignal`; subsystem failures still exit 1.

The hammer phase prevents the production bug where one hung connection blocks
shutdown forever. The interrupt func above implements this: `Shutdown(ctx)`
attempts drain within the deadline; if it times out, `Close()` closes the
remaining HTTP connections. `Close` does not wait for their handlers, which is
why the DB close above is bounded separately.

Long-lived HTTP responses (SSE, streaming, git smart HTTP) stay active, so
`Shutdown` waits on them until the hammer. Give their handlers a shutdown
signal of their own so they finish inside the drain window instead.

Hijacked connections such as WebSockets are different: `Shutdown` and `Close`
neither wait for nor close them. They don't block shutdown, but without extra
work clients get no close frame and their handlers keep running until the
process exits. Track them, notify them from `RegisterOnShutdown` (send a close
frame, but don't wait inside the hook), then wait for them within the same
shutdown deadline. The 15s default (`SHUTDOWN_TIMEOUT`) matches most Kubernetes/orchestrator
drain windows; set it to fit yours (see [config.md](../config.md)).
