# Handlers

Service layer, handler adapter pattern, and HTTP server assembly.

## Contents

- [Service Layer](#service-layer)
- [Handler Adapter](#handler-adapter)
- [Decoders](#decoders)
- [Error mapping](#error-mapping)
- [HTTP Server (stdlib net/http)](#http-server-stdlib-nethttp)

## Service Layer

Service methods have one shape: `func(context.Context, In) (Out, error)`. No
transport types, no wire formats. Dependencies live on the struct; the handler
adapter sees a plain function via method values (`orderSvc.Get` binds the
receiver, so the adapter sees `func(context.Context, string) (*Order, error)`).

```go
// Domain errors. Services return these; the HTTP layer maps them to status codes.
// Prefix with domain noun for grep-ability (see references/errors.md).
var (
	ErrNotFound   = errors.New("order: not found")
	ErrValidation = errors.New("order: validation")
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
		if errors.Is(err, ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("find order: %v", err)
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
		return nil, fmt.Errorf("insert order: %v", err)
	}
	return order, nil
}
```

## Handler Adapter

A generic function handles decode -> validate -> call -> encode for every endpoint.
Plumbing bugs are fixed once; adding an endpoint costs one decoder and one router
line (Redowan Delowar).

```go
// handle adapts a service function to an HTTP handler.
// If In implements Validator, validation runs after decode, before the call.
func handle[In, Out any](
	logger *slog.Logger,
	decode func(http.ResponseWriter, *http.Request) (In, error),
	fn     func(context.Context, In) (Out, error),
) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		in, err := decode(w, r)
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
			logger.LogAttrs(r.Context(), slog.LevelError, "encode response", slog.Any("err", err))
		}
	})
}
```

For custom response encoding (SSE, non-JSON), add an `encode` parameter to
`handle` for the full three-arg shape. The default adapter assumes JSON
responses, which covers most endpoints.

**Pointer-receiver caveat:** The `any(in).(Validator)` assertion checks the
decoded value. If `Validate()` has a pointer receiver (e.g., `func (r
*CreateOrderRequest) Validate() error`), the assertion only matches when `In`
is already a pointer type. Use value receivers for request validators, or decode
into a pointer type (`decodeJSON[*CreateOrderRequest](...)`), to ensure
validation always fires.

### Decoders

Decoders are small functions that extract input from the request. Reuse the
common ones; write per-endpoint decoders for custom wire shapes.

```go
// decodeJSON reads a JSON body into In. The caller supplies the route-level
// request body budget explicitly.
func decodeJSON[In any](maxBodyBytes int64) func(http.ResponseWriter, *http.Request) (In, error) {
	return func(w http.ResponseWriter, r *http.Request) (In, error) {
		var in In
		r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		dec := json.NewDecoder(r.Body)
		if err := dec.Decode(&in); err != nil {
			var maxErr *http.MaxBytesError
			if errors.As(err, &maxErr) {
				return in, fmt.Errorf("%w: request body too large", ErrValidation)
			}
			return in, fmt.Errorf("%w: invalid request body", ErrValidation)
		}
		var extra any
		if err := dec.Decode(&extra); err != io.EOF {
			var maxErr *http.MaxBytesError
			if errors.As(err, &maxErr) {
				return in, fmt.Errorf("%w: request body too large", ErrValidation)
			}
			if err != nil {
				return in, fmt.Errorf("%w: invalid request body", ErrValidation)
			}
			return in, fmt.Errorf("%w: multiple JSON values", ErrValidation)
		}
		return in, nil
	}
}

// decodePathID reads a single path parameter.
func decodePathID(param string) func(http.ResponseWriter, *http.Request) (string, error) {
	return func(_ http.ResponseWriter, r *http.Request) (string, error) {
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
		writeJSONError(w, http.StatusNotFound, "not found")
	case errors.Is(err, ErrValidation):
		writeJSONError(w, http.StatusBadRequest, err.Error())
	default:
		logger.LogAttrs(r.Context(), slog.LevelError, "internal error",
			slog.Any("err", err),
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
		)
		writeJSONError(w, http.StatusInternalServerError, "internal server error")
	}
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(struct {
		Error string `json:"error"`
	}{Error: msg}); err != nil {
		// Nothing useful to do after headers are written.
		return
	}
}
```

## HTTP Server (stdlib net/http)

```go
func buildHTTPServer(logger *slog.Logger, orderSvc *OrderService) *http.Server {
	mux := http.NewServeMux()

	// Each line: decode → validate → call → JSON encode.
	mux.Handle("GET /api/orders/{id}", handle(logger, decodePathID("id"), orderSvc.Get))
	mux.Handle("POST /api/orders", handle(
		logger,
		decodeJSON[CreateOrderRequest](1<<20), // 1 MB body limit
		orderSvc.Create,
	))
	mux.HandleFunc("GET /healthz", handleHealthz())
	mux.HandleFunc("GET /readyz", handleReadyz(2*time.Second, orderSvc))

	// Middleware stack: outermost wraps first, runs first.
	// Do not add panic recovery middleware; handlers return errors.
	handler := withRequestID(withLogging(logger, mux))

	// Engineering decisions, not config — these are correct for our protocol
	// and workload. Graduate to config only when they need to vary per
	// deployment (see references/config.md).
	return &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    1 << 20, // 1 MB
	}
}
```
