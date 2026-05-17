# Server Scaffold

Complete patterns for HTTP+gRPC servers. Copy, adapt, ship.

## Contents

- [CLI and Configuration](#cli-and-configuration) — Kong, loadConfig, source ownership
- [Complete main.go](#complete-maingo) — run group, signal handling, shutdown flow
- [Service Layer](#service-layer) — method shape, domain errors, validation
- [Handler Adapter](#handler-adapter) — generic decode/validate/call/encode, decoders, error mapping
- [HTTP Server (stdlib net/http)](#http-server-stdlib-nethttp) — mux, middleware stack, timeouts
- [Middleware](#middleware) — request ID, logging, auth, no panic recovery
- [gRPC with Connect (preferred)](#grpc-with-connect-preferred) — Connect handlers, interceptors, traditional gRPC
- [Health Checks](#health-checks) — liveness vs readiness, ReadinessChecker interface

## CLI and Configuration

Use `github.com/alecthomas/kong` for CLIs. Kong owns command and flag parsing.
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
	HTTPAddr        string
	GRPCAddr        string
	DatabaseURL     string
	LogLevel        slog.Level
	ShutdownTimeout time.Duration
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
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/alecthomas/kong"
	"github.com/oklog/run"
)

func main() {
	if err := runCLI(); err != nil {
		slog.New(slog.NewJSONHandler(os.Stderr, nil)).Error("exit", "err", err)
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
	logger := newLogger(cfg)

	if err := ctx.Run(&App{
		Config: cfg,
		Logger: logger,
	}); err != nil {
		return fmt.Errorf("run command: %w", err)
	}
	return nil
}

func runServer(cfg *Config, logger *slog.Logger) error {
	logger = logger.With("component", "server")

	// --- Dependency wiring (this IS the dependency graph) ---
	db, err := connectDB(cfg.DatabaseURL)
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
	{
		ln, err := net.Listen("tcp", cfg.HTTPAddr)
		if err != nil {
			return fmt.Errorf("listen http %s: %w", cfg.HTTPAddr, err)
		}
		g.Add(func() error {
			logger.Info("http server starting", "addr", ln.Addr().String())
			if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				return fmt.Errorf("serve http: %w", err)
			}
			return nil
		}, func(error) {
			ctx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
			defer cancel()
			if err := httpSrv.Shutdown(ctx); err != nil {
				logger.Error("shutdown http server", "err", err)
			}
		})
	}

	// Actor: Signal handler
	{
		ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
		g.Add(func() error {
			<-ctx.Done()
			logger.Info("received shutdown signal")
			return nil
		}, func(error) {
			cancel()
		})
	}

	// Actor: Background worker (uncomment as needed)
	// ctx, cancel := context.WithCancel(context.Background())
	// g.Add(func() error { return runFlusher(ctx, logger) }, func(error) { cancel() })

	logger.Info("starting", "http", cfg.HTTPAddr)
	if err := g.Run(); err != nil {
		return fmt.Errorf("run server: %w", err)
	}
	logger.Info("shutdown complete")
	return nil
}
```

Shutdown flow: signal -> HTTP `Shutdown` (drains in-flight) -> worker canceled -> exit 0.

## Service Layer

Service methods have one shape: `func(context.Context, In) (Out, error)`. No
transport types, no wire formats. Dependencies live on the struct; the handler
adapter sees a plain function via method values (`orderSvc.Get` binds the
receiver, so the adapter sees `func(context.Context, string) (*Order, error)`).

```go
// Domain errors. Services return these; the HTTP layer maps them to status codes.
var (
	ErrNotFound   = errors.New("not found")
	ErrValidation = errors.New("validation")
)

// Validator is optionally implemented by request types. The handler adapter
// calls Validate automatically after decode, before the service function.
type Validator interface {
	Validate() error
}

type CreateOrderRequest struct {
	UserID string   `json:"user_id"`
	Items  []string `json:"items"`
}

func (r CreateOrderRequest) Validate() error {
	if r.UserID == "" {
		return fmt.Errorf("%w: user_id required", ErrValidation)
	}
	if len(r.Items) == 0 {
		return fmt.Errorf("%w: at least one item required", ErrValidation)
	}
	return nil
}

type OrderService struct {
	logger *slog.Logger
	store  *UserStore
}

func (s *OrderService) Get(ctx context.Context, id string) (*Order, error) {
	order, err := s.store.FindOrder(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("find order: %w", err)
	}
	if order == nil {
		return nil, ErrNotFound
	}
	return order, nil
}

// Create validates via CreateOrderRequest.Validate() automatically.
func (s *OrderService) Create(ctx context.Context, in CreateOrderRequest) (*Order, error) {
	order, err := s.store.InsertOrder(ctx, in.UserID, in.Items)
	if err != nil {
		return nil, fmt.Errorf("insert order: %w", err)
	}
	return order, nil
}
```

## Handler Adapter

A generic function handles decode → validate → call → encode for every endpoint.
Plumbing bugs are fixed once; adding an endpoint costs one decoder and one router
line (Redowan Delowar).

```go
// handle adapts a service function to an HTTP handler.
// If In implements Validator, validation runs after decode, before the call.
func handle[In, Out any](
	logger *slog.Logger,
	decode func(*http.Request) (In, error),
	fn     func(context.Context, In) (Out, error),
) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		in, err := decode(r)
		if err != nil {
			writeError(logger, w, r, err)
			return
		}
		if v, ok := any(in).(Validator); ok {
			if err := v.Validate(); err != nil {
				writeError(logger, w, r, err)
				return
			}
		}
		out, err := fn(r.Context(), in)
		if err != nil {
			writeError(logger, w, r, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(out); err != nil {
			logger.ErrorContext(r.Context(), "encode response", "err", err)
		}
	})
}
```

For custom response encoding (SSE, non-JSON), add an `encode` parameter to
`handle` for the full three-arg shape. The default adapter assumes JSON
responses, which covers most endpoints.

### Decoders

Decoders are small functions that extract input from the request. Reuse the
common ones; write per-endpoint decoders for custom wire shapes.

```go
// decodeJSON reads a JSON body into In.
func decodeJSON[In any](r *http.Request) (In, error) {
	var in In
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		return in, fmt.Errorf("%w: invalid request body", ErrValidation)
	}
	return in, nil
}

// decodePathID reads a single path parameter.
func decodePathID(param string) func(*http.Request) (string, error) {
	return func(r *http.Request) (string, error) {
		id := r.PathValue(param)
		if id == "" {
			return "", fmt.Errorf("%w: missing %s", ErrValidation, param)
		}
		return id, nil
	}
}
```

### Error mapping

Domain errors map to HTTP status at the boundary. Transport-layer errors (bad
JSON, missing path param) wrap `ErrValidation` so the same switch handles them.

```go
func writeError(logger *slog.Logger, w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
	case errors.Is(err, ErrValidation):
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusBadRequest)
	default:
		logger.ErrorContext(r.Context(), "internal error",
			"err", err, "method", r.Method, "path", r.URL.Path)
		http.Error(w, `{"error":"internal server error"}`, http.StatusInternalServerError)
	}
}
```

## HTTP Server (stdlib net/http)

```go
func buildHTTPServer(logger *slog.Logger, orderSvc *OrderService) *http.Server {
	mux := http.NewServeMux()

	// Each line: decode → validate → call → JSON encode.
	mux.Handle("GET /api/orders/{id}", handle(logger, decodePathID("id"), orderSvc.Get))
	mux.Handle("POST /api/orders", handle(logger, decodeJSON[CreateOrderRequest], orderSvc.Create))
	mux.HandleFunc("GET /healthz", handleHealthz())
	mux.HandleFunc("GET /readyz", handleReadyz(orderSvc))

	// Middleware stack: outermost wraps first, runs first.
	// Do not add panic recovery middleware; handlers return errors.
	handler := withRequestID(withLogging(logger, mux))

	return &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20, // 1 MB
	}
}
```

## Middleware

### Request ID

```go
type requestIDKey struct{}

func withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if id == "" {
			id = uuid.NewString()
		}
		ctx := context.WithValue(r.Context(), requestIDKey{}, id)
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}
```

### Logging

```go
func withLogging(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		logger.LogAttrs(r.Context(), slog.LevelInfo, "request",
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("status", sw.status),
			slog.Duration("duration", time.Since(start)),
			slog.String("request_id", RequestID(r.Context())),
		)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wroteHeader {
		w.status = code
		w.wroteHeader = true
	}
	w.ResponseWriter.WriteHeader(code)
}
```

### No panic recovery middleware

Handlers return errors and map them at the boundary. Do not add HTTP/gRPC
middleware that recovers panics. A panic is a bug, not a request-level error
path. The standard `net/http` server recovers panics from handlers; do not copy
that behavior into application middleware.

Use the goroutine supervisor pattern in
[concurrency.md](concurrency.md#panic-supervision-with-safego) for owned
background goroutines that must report panics to their owner.

### Auth

```go
func withAuth(verifier TokenVerifier, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if token == "" {
			http.Error(w, `{"error":"missing authorization"}`, http.StatusUnauthorized)
			return
		}
		claims, err := verifier.Verify(r.Context(), token)
		if err != nil {
			http.Error(w, `{"error":"invalid token"}`, http.StatusUnauthorized)
			return
		}
		ctx := context.WithValue(r.Context(), claimsKey{}, claims)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
```

## gRPC with Connect (preferred)

connectrpc.com/connect serves Connect, gRPC, and gRPC-Web on a single HTTP
port. No separate listener. Works with stdlib `net/http`.

```go
import (
	"connectrpc.com/connect"
	"connectrpc.com/grpchealth"
	"connectrpc.com/grpcreflect"
	orderv1connect "example.com/gen/order/v1/orderv1connect"
)

func buildConnectServer(logger *slog.Logger, orderSvc *OrderService) *http.Server {
	mux := http.NewServeMux()

	orderPath, orderHandler := orderv1connect.NewOrderServiceHandler(
		&orderServiceServer{svc: orderSvc},
		connect.WithInterceptors(newLoggingInterceptor(logger)),
	)
	mux.Handle(orderPath, orderHandler)

	// Health (grpc.health.v1.Health) and reflection (for grpcurl)
	checker := grpchealth.NewStaticChecker(orderv1connect.OrderServiceName)
	mux.Handle(grpchealth.NewHandler(checker))
	mux.Handle(grpcreflect.NewHandlerV1(
		grpcreflect.NewStaticReflector(orderv1connect.OrderServiceName),
	))

	return &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
}

// Connect interceptor
func newLoggingInterceptor(logger *slog.Logger) connect.UnaryInterceptorFunc {
	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			start := time.Now()
			resp, err := next(ctx, req)
			logger.LogAttrs(ctx, slog.LevelInfo, "rpc",
				slog.String("procedure", req.Spec().Procedure),
				slog.Duration("duration", time.Since(start)),
				slog.Bool("error", err != nil),
			)
			return resp, err
		}
	}
}
```

### Traditional google.golang.org/grpc

Use when Connect is not an option (bidirectional streaming, legacy clients).
Separate port, own run group actor. Interrupt via `GracefulStop()`.

```go
grpcSrv := grpc.NewServer(
	grpc.ChainUnaryInterceptor(loggingUnaryInterceptor(logger), authUnaryInterceptor(verifier)),
)
orderv1.RegisterOrderServiceServer(grpcSrv, &orderGRPCServer{svc: orderSvc})

ln, err := net.Listen("tcp", cfg.GRPCAddr)
if err != nil {
	return fmt.Errorf("listen grpc %s: %w", cfg.GRPCAddr, err)
}
g.Add(
	func() error {
		if err := grpcSrv.Serve(ln); err != nil {
			return fmt.Errorf("serve grpc: %w", err)
		}
		return nil
	},
	func(error) {
		done := make(chan struct{})
		// Raw go is acceptable here: GracefulStop has no context, this helper is
		// waited below, and Stop bounds shutdown if GracefulStop hangs.
		go func() {
			defer close(done)
			grpcSrv.GracefulStop()
		}()
		timer := time.NewTimer(cfg.ShutdownTimeout)
		defer timer.Stop()
		select {
		case <-done:
		case <-timer.C:
			grpcSrv.Stop()
		}
	},
)
```

## Health Checks

Two endpoints, two purposes. Never combine them.

```go
// Liveness: is the process alive? Always 200. No dependency checks.
func handleHealthz() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) { fmt.Fprintln(w, "ok") }
}

// Readiness: can this instance serve traffic? Failure removes from pool without restart.
func handleReadyz(deps ...ReadinessChecker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		for _, dep := range deps {
			if err := dep.Ready(ctx); err != nil {
				http.Error(w, fmt.Sprintf("not ready: %v", err), http.StatusServiceUnavailable)
				return
			}
		}
		fmt.Fprintln(w, "ok")
	}
}

// ReadinessChecker is implemented by any dependency that can report health.
type ReadinessChecker interface {
	Ready(ctx context.Context) error
}

// Example: func (s *UserStore) Ready(ctx context.Context) error { return s.db.PingContext(ctx) }
```
