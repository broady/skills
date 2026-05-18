# gRPC with Connect

Connect handlers, interceptors, and traditional google.golang.org/grpc fallback.

## Contents

- [gRPC with Connect (project default)](#grpc-with-connect-project-default)
- [Traditional google.golang.org/grpc](#traditional-googlegolangorggrpc)

## gRPC with Connect (project default)

connectrpc.com/connect serves Connect, gRPC, and gRPC-Web on a single HTTP
port. No separate listener. Works with stdlib `net/http`.

```go
import (
	"connectrpc.com/connect"
	"connectrpc.com/grpchealth"
	"connectrpc.com/grpcreflect"
	orderv1connect "example.com/gen/order/v1/orderv1connect"
)

func buildConnectServer(cfg *Config, logger *slog.Logger, orderSvc *OrderService) *http.Server {
	mux := http.NewServeMux()

	orderPath, orderHandler := orderv1connect.NewOrderServiceHandler(
		&orderServiceServer{svc: orderSvc},
		connect.WithInterceptors(newLoggingInterceptor(logger)),
	)
	mux.Handle(orderPath, orderHandler)

	// Health (grpc.health.v1.Health). Reflection is a discovery surface; enable
	// it only for local/dev/admin environments via build tag or config flag.
	checker := grpchealth.NewStaticChecker(orderv1connect.OrderServiceName)
	mux.Handle(grpchealth.NewHandler(checker))
	if cfg.EnableReflection {
		mux.Handle(grpcreflect.NewHandlerV1(
			grpcreflect.NewStaticReflector(orderv1connect.OrderServiceName),
		))
	}

	// Engineering decisions, not config — these are correct for our protocol
	// and workload. Graduate to config only when they need to vary per
	// deployment (see references/config.md).
	return &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
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

## Traditional google.golang.org/grpc

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
