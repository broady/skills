# Observability Reference

Investment order (Peter Bourgon): metrics first, structured logging second, distributed tracing third.

Load the sub-file relevant to the current task. Skip the rest.

| File | Covers | Load when... |
|---|---|---|
| [observability/logging.md](observability/logging.md) | slog setup, injection, scoped loggers, levels, LogAttrs, redaction, canonical log lines | Adding/changing logging, reviewing log hygiene |
| [observability/metrics-tracing.md](observability/metrics-tracing.md) | OTel provider setup, HTTP/gRPC middleware spans, manual spans, DB instrumentation, RED/USE metrics | Adding metrics or tracing, instrumenting endpoints |
| [observability/runtime.md](observability/runtime.md) | pprof, goroutine labels, runtime/metrics, expvar | Debugging performance, adding profiling, exposing debug state |

## Decision Matrix

| Question | Answer |
|---|---|
| Which logging library? | `log/slog` -- no alternatives |
| Log format in production? | `slog.NewJSONHandler` to stdout |
| Log format in development? | `slog.NewTextHandler` to stderr, with source |
| Where to log errors? | At the boundary only (handler, interceptor) |
| Hot-path logging? | `logger.LogAttrs(ctx, ...)` to avoid allocations |
| Metrics library? | Project default: OpenTelemetry SDK when multi-backend export or org policy justifies it; Prometheus client is fine for single-backend services |
| Tracing library? | OpenTelemetry SDK with OTLP exporter when tracing has a concrete operational need |
| What to instrument first? | RED metrics on every endpoint |
| When to add tracing? | Multiple services needing cross-service debug |
| pprof in production? | Yes, always -- on a separate internal port |
| Custom debug vars? | `expvar` on the debug server |
