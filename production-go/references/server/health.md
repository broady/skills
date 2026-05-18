# Health Checks

Liveness vs readiness endpoints and the ReadinessChecker interface.

## Health Checks

Two endpoints, two purposes. Never combine them.

```go
// Liveness: is the process alive? Always 200. No dependency checks.
func handleHealthz() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) { fmt.Fprintln(w, "ok") }
}

// Readiness: can this instance serve traffic? Failure removes from pool without restart.
// readinessTimeout should come from configuration (default: 2s is reasonable for
// most services; tune based on the slowest dependency check).
func handleReadyz(logger *slog.Logger, readinessTimeout time.Duration, deps ...ReadinessChecker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), readinessTimeout)
		defer cancel()
		for _, dep := range deps {
			if err := dep.Ready(ctx); err != nil {
				logger.WarnContext(ctx, "readiness check failed",
					slog.String("dependency", dep.Name()),
					slog.Any("err", err),
				)
				writeJSONError(w, http.StatusServiceUnavailable, "not ready: "+dep.Name())
				return
			}
		}
		fmt.Fprintln(w, "ok")
	}
}

// ReadinessChecker is implemented by any dependency that can report health.
// Name returns a broad category such as "database" or "cache". Log raw
// dependency errors internally; never expose them to probe clients.
type ReadinessChecker interface {
	Name() string
	Ready(ctx context.Context) error
}

// Example — implement on any dependency (e.g., OrderService, UserStore):
//
// func (s *OrderService) Name() string   { return "orders" }
// func (s *OrderService) Ready(ctx context.Context) error { return s.store.PingContext(ctx) }
//
// func (s *UserStore) Name() string { return "database" }
// func (s *UserStore) Ready(ctx context.Context) error { return s.db.PingContext(ctx) }
```
