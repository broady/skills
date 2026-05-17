# OpenTelemetry for Metrics and Tracing

## Contents

- [Provider setup in main()](#provider-setup-in-main)
- [HTTP/gRPC middleware -- automatic spans](#httpgrpc-middleware----automatic-spans)
- [Manual spans for important operations](#manual-spans-for-important-operations)
- [Instrumenting a database call](#instrumenting-a-database-call)
- [Metrics -- RED and USE](#metrics----red-and-use)

> **Note:** OTel is used here for demonstration — it shows the patterns
> (provider setup, middleware spans, manual instrumentation, RED/USE metrics)
> in a vendor-neutral way. It is not a blanket recommendation. OTel adds a
> large dependency tree, non-trivial per-request overhead, and startup cost.
> Evaluate whether you need it:
>
> - **Single service, Prometheus backend** — `prometheus/client_golang`
>   directly is simpler and lighter.
> - **Single service, no metrics backend yet** — canonical log lines (§1) +
>   pprof (§3) cover most debugging needs. Add metrics when you have
>   somewhere to send them.
> - **Multiple services, need cross-service tracing** — OTel earns its weight
>   here via vendor-neutral OTLP export and contrib auto-instrumentation.
>
> If you do adopt OTel, prefer metrics-only (`sdkmetric`) until tracing is
> justified. Tracing doubles the dependency and runtime cost.

When using OTel: export to Prometheus, OTLP, or other backends via exporters
rather than coupling to a vendor client directly.

## Provider setup in main()

```go
func initTelemetry(ctx context.Context, svcName, svcVersion string) (func(context.Context) error, error) {
    res, err := resource.New(ctx, resource.WithAttributes(
        semconv.ServiceName(svcName), semconv.ServiceVersion(svcVersion),
    ))
    if err != nil {
        return nil, fmt.Errorf("create resource: %w", err)
    }

    traceExp, err := otlptracegrpc.New(ctx)
    if err != nil {
        return nil, fmt.Errorf("create trace exporter: %w", err)
    }
    tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(traceExp), sdktrace.WithResource(res))
    otel.SetTracerProvider(tp)

    metricExp, err := otlpmetricgrpc.New(ctx)
    if err != nil {
        return nil, fmt.Errorf("create metric exporter: %w", err)
    }
    mp := sdkmetric.NewMeterProvider(
        sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExp)),
        sdkmetric.WithResource(res),
    )
    otel.SetMeterProvider(mp)

    return func(ctx context.Context) error {
        return errors.Join(tp.Shutdown(ctx), mp.Shutdown(ctx))
    }, nil
}
```

## HTTP/gRPC middleware -- automatic spans

```go
// HTTP: go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp
handler := otelhttp.NewHandler(mux, "http-server")

// gRPC: go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc
srv := grpc.NewServer(grpc.StatsHandler(otelgrpc.NewServerHandler()))
```

## Manual spans for important operations

```go
func (s *OrderService) Create(ctx context.Context, req CreateOrderReq) (*Order, error) {
    ctx, span := otel.Tracer("order-service").Start(ctx, "OrderService.Create")
    defer span.End()
    span.SetAttributes(attribute.String("order_id", req.ID))

    order, err := s.store.Insert(ctx, req)
    if err != nil {
        span.RecordError(err)
        span.SetStatus(codes.Error, "insert failed")
        return nil, fmt.Errorf("insert order %s: %w", req.ID, err)
    }
    return order, nil
}
```

## Instrumenting a database call

```go
func (s *Store) GetUser(ctx context.Context, id string) (*User, error) {
    ctx, span := otel.Tracer("user-store").Start(ctx, "Store.GetUser")
    defer span.End()

    var u User
    err := s.db.QueryRowContext(ctx, "SELECT id, name, email FROM users WHERE id = $1", id).
        Scan(&u.ID, &u.Name, &u.Email)
    if err != nil {
        span.RecordError(err)
        span.SetStatus(codes.Error, "query failed")
        return nil, fmt.Errorf("get user %s: %w", id, err)
    }
    return &u, nil
}
```

## Metrics -- RED and USE

**RED for endpoints**: Rate, Errors, Duration.
**USE for resources**: Utilization, Saturation, Errors.

Create counters/histograms via `meter.Int64Counter(...)` and
`meter.Float64Histogram(...)`. Record in middleware:

```go
func metricsMiddleware(dur metric.Float64Histogram, total metric.Int64Counter, next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        start := time.Now()
        rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
        next.ServeHTTP(rec, r)

        attrs := metric.WithAttributes(
            attribute.String("http.method", r.Method),
            attribute.String("http.route", r.Pattern),
            attribute.Int("http.status_code", rec.status),
        )
        dur.Record(r.Context(), time.Since(start).Seconds(), attrs)
        total.Add(r.Context(), 1, attrs)
    })
}
```
