# Middleware

HTTP middleware patterns: request ID, logging, auth, and why not to recover panics.

## Request ID

Validate and length-cap inbound `X-Request-ID`. Reflecting arbitrary header
content without normalization risks log injection and header smuggling.

```go
type requestIDKey struct{}

const maxRequestIDLen = 64

// validRequestID returns true if id contains only printable ASCII without
// control characters or spaces — safe for headers and structured logs.
func validRequestID(id string) bool {
	if len(id) == 0 || len(id) > maxRequestIDLen {
		return false
	}
	for _, c := range id {
		if c < '!' || c > '~' { // printable ASCII, no space
			return false
		}
	}
	return true
}

func withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if !validRequestID(id) {
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

## Logging

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
	if w.wroteHeader {
		return // net/http: superfluous WriteHeader call
	}
	w.status = code
	w.wroteHeader = true
	w.ResponseWriter.WriteHeader(code)
}
```

## No panic recovery middleware

Handlers return errors and map them at the boundary. Do not add HTTP/gRPC
middleware that recovers panics. A panic is a bug, not a request-level error
path. The standard `net/http` server recovers panics from handlers; do not copy
that behavior into application middleware. Make panics observable through normal
process supervision, server error logs, and crash reporting; do not translate
them into ordinary application control flow.

Use the goroutine supervisor pattern in
[concurrency.md](../concurrency.md#panic-supervision-with-safego) for owned
background goroutines that must report panics to their owner.

## Auth

Parse Authorization as an actual Bearer scheme. `strings.TrimPrefix` alone
accepts any non-empty string that doesn't start with "Bearer " as a valid
token, which is incorrect.

```go
func withAuth(verifier TokenVerifier, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := parseBearerToken(r.Header.Get("Authorization"))
		if !ok {
			writeJSONError(w, http.StatusUnauthorized, "missing or malformed authorization")
			return
		}
		claims, err := verifier.Verify(r.Context(), token)
		if err != nil {
			writeJSONError(w, http.StatusUnauthorized, "invalid token")
			return
		}
		ctx := context.WithValue(r.Context(), claimsKey{}, claims)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// parseBearerToken extracts the token from a "Bearer <token>" header value.
// Returns ("", false) if the header is missing, empty, or not Bearer scheme.
func parseBearerToken(header string) (string, bool) {
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return "", false
	}
	token := header[len(prefix):]
	if token == "" {
		return "", false
	}
	return token, true
}
```
