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
func handleReadyz(deps ...ReadinessChecker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		for _, dep := range deps {
			if err := dep.Ready(ctx); err != nil {
				writeJSONError(w, http.StatusServiceUnavailable, fmt.Sprintf("not ready: %v", err))
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
