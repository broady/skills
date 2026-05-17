# Boundary Resilience and Flow Control

Resilience patterns at system boundaries: outbound calls to other services,
databases, caches, queues, and inbound HTTP/gRPC request handling under load.

**Core principle: never generate an unbounded remote call.** Every boundary call
needs a propagated `context.Context`, a deadline, bounded concurrency, metrics,
and explicit retry/cancellation behavior.

## Contents

1. [Default Go Stack](#default-go-stack)
2. [Default Outbound Composition](#default-outbound-composition)
3. [Default Inbound Composition](#default-inbound-composition)
4. [Circuit Breaker](#1-circuit-breaker)
5. [Retry with Budget](#2-retry-with-budget)
6. [Load Shedding](#3-load-shedding)
7. [Hedged Requests](#4-hedged-requests)
8. [Bulkheading](#5-bulkheading)
9. [Backpressure Propagation](#6-backpressure-propagation)
10. [Timeouts as a System](#7-timeouts-as-a-system)
11. [Cross-Pattern Rules](#cross-pattern-rules)
12. [Decision Matrix](#decision-matrix)

---

## Default Go Stack

| Concern | Default |
|---|---|
| Policy composition | `github.com/failsafe-go/failsafe-go` |
| Retry, budget, circuit breaker, bulkhead, adaptive limiter, timeout, hedge | Failsafe-go |
| HTTP integration | `failsafehttp` or explicit `http.RoundTripper` / `http.Handler` wrapper |
| gRPC integration | `failsafegrpc` interceptors / handlers |
| Simple contractual rate limits | `golang.org/x/time/rate` (not a substitute for load shedding) |

Failsafe-go is pre-v1. Centralize all policy wrappers in one internal package
(e.g., `internal/downstream`) and pin a tested version in `go.mod`.

---

## Default Outbound Composition

One **dependency client wrapper** per downstream. Compose policies there, not at
each call site.

Layer order (outermost to innermost):

```
RetryWithBudget(CircuitBreaker(Bulkhead(PerAttemptTimeout(call(ctx)))))
```

In Failsafe-go, policies compose in argument order:

```go
res, err := failsafe.With[*http.Response](
    retryPolicy,        // outer: decides whether another attempt is allowed
    circuitBreaker,     // fail fast when dependency is unhealthy
    dependencyBulkhead, // cap concurrent attempts to this dependency
    perAttemptTimeout,  // inner: one timeout per actual attempt
).WithContext(ctx).GetWithExecution(func(exec failsafe.Execution[*http.Response]) (*http.Response, error) {
    req, err := http.NewRequestWithContext(exec.Context(), http.MethodGet, url, nil)
    if err != nil {
        return nil, err
    }
    return httpClient.Do(req)
})
```

**Critical:** do not let a circuit breaker treat `bulkhead.ErrFull`, rate-limiter
rejections, caller cancellations, or retry-budget exhaustion as downstream
failures.

---

## Default Inbound Composition

1. Server transport timeouts.
2. Establish or honor a request deadline.
3. Classify priority/criticality cheaply.
4. Adaptive concurrency/load shedding before expensive work.
5. Propagate `context.Context` to all work.
6. Return explicit overload signals: HTTP `429`/`503` with `Retry-After`; gRPC `RESOURCE_EXHAUSTED`.

---

## 1. Circuit Breaker

Prevents cascading failure from slow or partially-available dependencies.

Three states: **Closed** (normal, measuring failures) → **Open** (fail fast) →
**Half-open** (limited probes after cooldown).

### Defaults

- Minimum volume: **20 requests** before failure-rate threshold applies.
- Failure-rate trip: **>=50%** over **30-60 second** rolling window.
- Low-QPS alternative: **5 consecutive failures**.
- Open delay: **30 seconds**, optionally with jitter.
- Half-open probes: **1-3 concurrent**.
- Success threshold: **2-5 successes** before closing.

Never trip on one error. Failsafe-go's default builder can open after one
failure if unconfigured -- always set thresholds explicitly.

### Implementation

```go
package downstream

import (
    "context"
    "errors"
    "net/http"
    "time"

    "github.com/failsafe-go/failsafe-go/circuitbreaker"
)

func isDownstreamFailure(resp *http.Response, err error) bool {
    if errors.Is(err, context.Canceled) {
        return false
    }
    if err != nil {
        return true
    }
    if resp == nil {
        return false
    }
    return resp.StatusCode >= 500
}

type DependencyClient struct {
    httpClient *http.Client
    breaker    circuitbreaker.CircuitBreaker[*http.Response]
}

func NewDependencyClient(httpClient *http.Client) *DependencyClient {
    return &DependencyClient{
        httpClient: httpClient,
        breaker: circuitbreaker.NewBuilder[*http.Response]().
            HandleIf(isDownstreamFailure).
            WithFailureRateThreshold(0.50, 20, 30*time.Second).
            WithDelay(30 * time.Second).
            WithSuccessThreshold(3).
            Build(),
    }
}
```

**Idempotency note:** idempotency is NOT required for a breaker to reject calls.
It matters for retries and hedges, not breakers. For half-open probing of
non-idempotent writes, prefer a health/read probe.

---

## 2. Retry with Budget

Prevents retry storms while still handling transient failures.

**Per-call:** at most 2 retries, exponential backoff with jitter.
**System-level:** shared retry budget per dependency: **20% additional traffic**.

### Per-call defaults

```
max attempts:     3 total (1 original + 2 retries)
initial backoff:  25-100 ms
max backoff:      500 ms - 2s
jitter:           full jitter or >= 50%
total deadline:   inherited from caller; never extended by retry
retryable:        GET/HEAD and explicitly idempotent POST/PUT with idempotency key
```

### Implementation

```go
package downstream

import (
    "context"
    "errors"
    "net/http"
    "time"

    "github.com/failsafe-go/failsafe-go"
    "github.com/failsafe-go/failsafe-go/budget"
    "github.com/failsafe-go/failsafe-go/retrypolicy"
)

func retryableHTTP(resp *http.Response, err error) bool {
    if errors.Is(err, context.Canceled) {
        return false
    }
    if err != nil {
        return true
    }
    if resp == nil {
        return false
    }
    switch resp.StatusCode {
    case http.StatusTooManyRequests,
        http.StatusBadGateway,
        http.StatusServiceUnavailable,
        http.StatusGatewayTimeout:
        return true
    default:
        return false
    }
}

type RetryingClient struct {
    httpClient  *http.Client
    retryBudget budget.Budget
    retryPolicy retrypolicy.RetryPolicy[*http.Response]
}

func NewRetryingClient(httpClient *http.Client) *RetryingClient {
    b := budget.NewBuilder().
        WithMaxRate(0.20).
        WithMinConcurrency(3).
        Build()

    return &RetryingClient{
        httpClient:  httpClient,
        retryBudget: b,
        retryPolicy: retrypolicy.NewBuilder[*http.Response]().
            HandleIf(retryableHTTP).
            AbortOnErrors(context.Canceled).
            WithBackoff(50*time.Millisecond, 1*time.Second).
            WithJitterFactor(0.5).
            WithMaxRetries(2).
            WithBudget(b).
            Build(),
    }
}

func (c *RetryingClient) Get(ctx context.Context, url string) (*http.Response, error) {
    return failsafe.With[*http.Response](c.retryPolicy).
        WithContext(ctx).
        GetWithExecution(func(exec failsafe.Execution[*http.Response]) (*http.Response, error) {
            req, err := http.NewRequestWithContext(exec.Context(), http.MethodGet, url, nil)
            if err != nil {
                return nil, err
            }
            return c.httpClient.Do(req)
        })
}
```

---

## 3. Load Shedding

Rejects excess work early to preserve goodput for completable requests.

Load shedding differs from rate limiting:
- **Rate limiting** enforces a contract ("tenant A gets 100 RPS").
- **Load shedding** protects the service when overloaded.

### Implementation: adaptive limiter

```go
package inbound

import (
    "net/http"
    "time"

    "github.com/failsafe-go/failsafe-go/adaptivelimiter"
    "github.com/failsafe-go/failsafe-go/failsafehttp"
)

func NewHandler(app http.Handler) http.Handler {
    limiter := adaptivelimiter.NewBuilder[any]().
        WithLimits(10, 500, 100).
        WithRecentWindow(1*time.Second, 30*time.Second, 100).
        WithBaselineWindow(10).
        WithQueueing(1, 2).
        Build()

    return failsafehttp.NewHandler(app, limiter)
}
```

Use adaptive limiting (capacity changes with autoscaling, noisy neighbors, mixed
request costs). Use a static semaphore only for hard local resource limits.

### Priority shedding

Classify priority early and cheaply (route, auth principal, tenant tier). Default
order: health checks > user-visible paid ops > internal control-plane > background
batch > crawlers/prefetch.

---

## 4. Hedged Requests

Speculative duplicate to cut tail latency. Use only for **idempotent reads**
where the downstream has spare capacity and multiple replicas.

### Defaults

```
hedge delay:  rolling p95 latency per dependency + route
max hedges:   1
budget:       shared with retries, 5-20% extra work
```

### Implementation

```go
package downstream

import (
    "net/http"
    "sync/atomic"
    "time"

    "github.com/failsafe-go/failsafe-go"
    "github.com/failsafe-go/failsafe-go/budget"
    "github.com/failsafe-go/failsafe-go/hedgepolicy"
)

type HedgingClient struct {
    p95Millis       atomic.Int64
    extraWorkBudget budget.Budget
    hedge           hedgepolicy.HedgePolicy[*http.Response]
}

func NewHedgingClient() *HedgingClient {
    c := &HedgingClient{}

    c.extraWorkBudget = budget.NewBuilder().
        WithMaxRate(0.10).
        WithMinConcurrency(1).
        Build()

    c.hedge = hedgepolicy.NewBuilderWithDelayFunc[*http.Response](
        func(exec failsafe.ExecutionAttempt[*http.Response]) time.Duration {
            p := c.p95Millis.Load()
            if p <= 0 {
                return 50 * time.Millisecond
            }
            return time.Duration(p) * time.Millisecond
        },
    ).
        WithMaxHedges(1).
        WithBudget(c.extraWorkBudget).
        Build()

    return c
}
```

---

## 5. Bulkheading

Isolates failure domains so one slow dependency cannot starve others.

### Sizing

Start with Little's Law: `concurrency ~ healthy_peak_rps x healthy_p99_latency`.
Cap by downstream contract, local memory/goroutine budget, and retry/hedge
amplification.

### Implementation

```go
type PaymentsClient struct {
    httpClient *http.Client
    bulkhead   bulkhead.Bulkhead[any]
}

func NewPaymentsClient(httpClient *http.Client) *PaymentsClient {
    return &PaymentsClient{
        httpClient: httpClient,
        bulkhead: bulkhead.NewBuilder[any](40).
            WithMaxWaitTime(20 * time.Millisecond).
            Build(),
    }
}
```

Also use dedicated HTTP transports per downstream with `MaxConnsPerHost` matching
the bulkhead size. See [Timeouts as a System](#7-timeouts-as-a-system) for the
full HTTP client template.

---

## 6. Backpressure Propagation

Explicit protocol-level signals that tell upstream callers to slow down.

| Situation | HTTP | gRPC |
|---|---|---|
| Tenant exceeded quota | `429 Too Many Requests` | `RESOURCE_EXHAUSTED` |
| Global overload | `503 Service Unavailable` + `Retry-After` | `UNAVAILABLE` |
| Caller deadline expired | `504` at gateway | `DEADLINE_EXCEEDED` |

```go
func WriteOverload(w http.ResponseWriter, perClient bool, retryAfter time.Duration) {
    if retryAfter > 0 {
        w.Header().Set("Retry-After", strconv.Itoa(int(retryAfter.Round(time.Second).Seconds())))
    }
    w.Header().Set("Content-Type", "application/json")
    if perClient {
        w.WriteHeader(http.StatusTooManyRequests)
        json.NewEncoder(w).Encode(struct{ Error string `json:"error"` }{Error: "too many requests"})
        return
    }
    w.WriteHeader(http.StatusServiceUnavailable)
    json.NewEncoder(w).Encode(struct{ Error string `json:"error"` }{Error: "service overloaded"})
}
```

Clients must: respect `Retry-After`, retry only if safe and budget permits,
use jittered backoff when `Retry-After` absent, stop immediately on context
cancellation.

---

## 7. Timeouts as a System

Every inbound request and outbound call uses a `context.Context` deadline.

### Budget concept

```
incoming request deadline: 300 ms
local work:                 50 ms
inventory call:             80 ms
payments call:             120 ms
retry allowance:            30 ms
response reserve:           20 ms
```

Never `context.WithTimeout(context.Background(), ...)` inside a handler. Child
contexts derive from parent request context.

### Child deadline helper

```go
func WithChildTimeout(parent context.Context, max time.Duration) (context.Context, context.CancelFunc) {
    if deadline, ok := parent.Deadline(); ok {
        remaining := time.Until(deadline)
        if remaining <= 0 {
            ctx, cancel := context.WithCancel(parent)
            cancel()
            return ctx, cancel
        }
        if remaining < max {
            max = remaining
        }
    }
    return context.WithTimeout(parent, max)
}
```

### Per-attempt vs overall timeout

In Failsafe-go: timeout outside retry = cancels whole sequence; timeout inside
retry = per-attempt.

```go
failsafe.With[*http.Response](
    retryPolicy,
    circuitBreaker,
    bulkhead,
    perAttemptTimeout, // inner: per-attempt
).WithContext(ctx).GetWithExecution(call) // ctx: overall deadline
```

### HTTP client template

```go
func NewHTTPClient(maxConnsPerHost int) *http.Client {
    dialer := &net.Dialer{
        Timeout:   100 * time.Millisecond,
        KeepAlive: 30 * time.Second,
    }
    return &http.Client{Transport: &http.Transport{
        Proxy:                  http.ProxyFromEnvironment,
        DialContext:            dialer.DialContext,
        TLSHandshakeTimeout:   200 * time.Millisecond,
        ResponseHeaderTimeout:  500 * time.Millisecond,
        ExpectContinueTimeout:  1 * time.Second,
        MaxConnsPerHost:        maxConnsPerHost,
        MaxIdleConns:           maxConnsPerHost * 2,
        MaxIdleConnsPerHost:    maxConnsPerHost,
        IdleConnTimeout:        90 * time.Second,
    }}
}
```

### HTTP server timeouts

```go
srv := &http.Server{
    Handler:           handler,
    ReadHeaderTimeout: 5 * time.Second,
    ReadTimeout:       15 * time.Second,
    WriteTimeout:      30 * time.Second,
    IdleTimeout:       2 * time.Minute,
    MaxHeaderBytes:    1 << 20,
}
```

---

## Cross-Pattern Rules

### Generate one wrapper per downstream

```go
type InventoryClient struct {
    http     *http.Client
    retry    failsafe.Policy[*http.Response]
    breaker  failsafe.Policy[*http.Response]
    bulkhead failsafe.Policy[*http.Response]
    timeout  failsafe.Policy[*http.Response]
}
```

The wrapper owns timeout configuration, retry classification, breaker thresholds,
bulkhead size, metrics labels, and overload handling.

### Classify errors explicitly

Distinguish these categories -- they drive metrics and prevent one policy from
corrupting another's signal:

- Caller cancellation / deadline exceeded
- Per-attempt downstream timeout
- Open circuit
- Bulkhead full / retry budget exhausted
- Downstream `5xx`
- Permanent client/application errors

### Policy interaction rules

- **Retry + breaker:** do not retry open-circuit errors. Abort immediately.
- **Retry + timeout:** never sleep past remaining deadline. Per-attempt timeout inside retry.
- **Retry + bulkhead:** do not hold permit while sleeping backoff.
- **Breaker signal:** do not count bulkhead rejections, rate-limit rejections, caller cancellations, or budget exhaustion as downstream failures.
- **Hedges + retries:** share one extra-work budget. Do not run independently.
- **Hedges + breaker:** do not hedge when circuit open. Canceled losers are not failures.
- **Load shedding + breaker:** inbound shed responses are not downstream failures.
- **Load shedding + hedging:** reduce/disable hedging when shedding active.
- **Timeout + bulkhead:** timeouts release bulkhead slots. Without them, permits held indefinitely.

### Common mistakes

- Counting caller cancellations as dependency failures (corrupts breaker signal).
- No per-attempt timeouts (slow dep holds resources indefinitely).
- Retrying at every layer of a call stack (3^N amplification).
- No retry budget, only per-call `maxAttempts`.
- Retrying non-idempotent writes without idempotency keys.
- Missing jitter on backoff.
- Sleeping backoff inside a bulkhead slot.
- One global breaker or connection pool for multiple downstreams.
- Breaker thresholds too low (flapping on transient errors).
- Hedging writes or hedging to the same backend instance.
- Not canceling losing hedged requests.
- Returning `500` for overload (use `429`/`503`).
- Omitting `Retry-After` on overload responses.
- Shedding health checks before ordinary traffic.
- `context.Background()` inside request handling.
- Continuing work after `ctx.Done()`.
- Not closing response bodies.

### Minimum viable metrics

```
outbound_attempts_total{dependency, method, outcome}
outbound_latency_seconds{dependency, method, outcome}
circuit_breaker_state{dependency}
bulkhead_in_use{dependency}
load_shed_total{endpoint}
```

Do not mix fast shed/open-circuit failures into successful downstream latency
histograms.

### Testing requirements

Generate tests or test hooks for: downstream hangs > per-attempt timeout,
connection refused, `503`/`429` with `Retry-After`, caller cancellation, retry
budget exhaustion, circuit open and half-open recovery, bulkhead full, hedged
request cancellation, inbound overload.

---

## Decision Matrix

| Pattern | Default | Use when | Do not use when |
|---|---|---|---|
| Circuit breaker | Failsafe-go per dependency | Correlated downstream slowness/failure; have a fallback or degraded path | No fallback, low traffic (will flap), local cheap call |
| Retry with budget | Failsafe-go + budget, max 2 retries | Safe/idempotent, transient error, deadline has budget, budget has capacity | Non-idempotent writes without key, expired deadline, retrying at multiple layers |
| Load shedding | Failsafe-go adaptive limiter | Overload reduces goodput; mixed-priority traffic | Substitute for tenant quota enforcement |
| Hedged requests | Failsafe-go hedge + budget, p95 delay, max 1 | Idempotent replicated reads with tail latency; spare downstream capacity | Writes, overloaded downstream, no cancellation, same instance |
| Bulkheading | Failsafe-go + per-downstream pools | Slow dep must not starve others; multiple downstreams or mixed workloads | Single trivial dep already bounded |
| Backpressure | HTTP `429`/`503` + `Retry-After`; gRPC `RESOURCE_EXHAUSTED` | Rejecting due to quota or overload | Hiding overload as success (`200`/`500`) |
| Timeouts | Parent deadline + child budgets + transport timeouts | Every boundary call | Never optional |
