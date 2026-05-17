# Server Reference

Load the sub-file relevant to the current task. Skip the rest.

| File | Covers | Load when... |
|---|---|---|
| [server/scaffold.md](server/scaffold.md) | Kong CLI, loadConfig, complete main.go, run group, shutdown flow | Starting a new service or wiring the process entry point |
| [server/handlers.md](server/handlers.md) | Service layer, generic handler adapter, decoders, error mapping, HTTP server assembly | Adding endpoints, changing request/response handling |
| [server/middleware.md](server/middleware.md) | Request ID, logging, auth, no panic recovery | Adding or modifying HTTP middleware |
| [server/connect-grpc.md](server/connect-grpc.md) | Connect handlers, interceptors, traditional gRPC fallback | Adding gRPC/Connect services |
| [server/health.md](server/health.md) | Liveness vs readiness, ReadinessChecker interface | Adding or debugging health checks |
