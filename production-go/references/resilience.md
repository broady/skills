# Boundary Resilience and Flow Control

Resilience patterns at system boundaries: outbound calls to other services,
databases, caches, queues, and inbound HTTP/gRPC request handling under load.

This document extends the safety invariants in [../SKILL.md](../SKILL.md):
"Retry loops: max attempts or deadline. Exponential backoff with jitter" and
"Bound every resource explicitly." It provides the concrete patterns, library
recommendations, and composition rules for implementing those invariants at
service boundaries.

**Core principle: never generate an unbounded remote call.** Every boundary call
needs a propagated `context.Context`, a deadline, bounded concurrency, metrics,
and explicit retry/cancellation behavior. Cascading failures are positive-feedback
loops: overload causes latency, latency holds resources, held resources reduce
capacity, clients retry, and the retry traffic amplifies the original failure.

## Contents

1. [Default Go Stack](#default-go-stack) -- library recommendations
2. [Default Outbound Composition](#default-outbound-composition) -- how policies layer
3. [Default Inbound Composition](#default-inbound-composition) -- admission control
4. [Circuit Breaker](#1-circuit-breaker) -- fail fast on unhealthy dependencies
5. [Retry with Budget](#2-retry-with-budget) -- bounded retries, system-level caps
6. [Load Shedding](#3-load-shedding) -- reject work to preserve goodput
7. [Hedged Requests](#4-hedged-requests) -- speculative retries for tail latency
8. [Bulkheading](#5-bulkheading) -- isolate failure domains
9. [Backpressure Propagation](#6-backpressure-propagation) -- signal "slow down" upstream
10. [Timeouts as a System](#7-timeouts-as-a-system) -- deadline budgets across call chains
11. [Cross-Pattern Rules](#cross-pattern-implementation-rules) -- wrappers, classification, metrics, testing

---

## Default Go Stack

Use this unless the service already has an internal platform library.

| Concern | Default |
|---|---|
| Policy composition | `github.com/failsafe-go/failsafe-go` |
| Retry, budget, circuit breaker, bulkhead, adaptive limiter, throttler, timeout, hedge | Failsafe-go |
| Standalone circuit breaker only | `github.com/sony/gobreaker/v2` |
| HTTP integration | `failsafehttp` or explicit `http.RoundTripper` / `http.Handler` wrapper |
| gRPC integration | `failsafegrpc` interceptors / handlers |
| Simple contractual rate limits | `golang.org/x/time/rate` (not a substitute for load shedding) |

Failsafe-go is pre-v1. Generated code should centralize all policy wrappers in
one internal package (e.g., `internal/downstream`) and pin a tested version in
`go.mod`. This isolates the rest of the codebase from API churn.

---

## Default Outbound Composition

For a single outbound dependency call, generate one **dependency client wrapper**
and compose policies there, not at each call site.

Default layer order (outermost to innermost):

```
RetryWithBudget(
  CircuitBreaker(
    Bulkhead(
      PerAttemptTimeout(
        call(ctx)
      )
    )
  )
)
```

In Failsafe-go, policies compose in argument order:
`failsafe.With(retry, breaker, bulkhead, timeout)` =>
`Retry(Breaker(Bulkhead(Timeout(fn))))`.

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
failures. That confuses the breaker's signal.

---

## Default Inbound Composition

For inbound HTTP/gRPC handlers:

1. Use server transport timeouts.
2. Establish or honor a request deadline.
3. Classify priority/criticality cheaply.
4. Apply adaptive concurrency/load shedding before expensive parsing or downstream calls.
5. Propagate `context.Context` to all work.
6. Return explicit overload signals: HTTP `429` or `503` with `Retry-After`; gRPC `RESOURCE_EXHAUSTED`.

---

## 1. Circuit Breaker

### Default recommendation

Use **Failsafe-go circuitbreaker** when composing with retries, timeouts,
bulkheads, and budgets. Use **sony/gobreaker v2** only for standalone use.

Three states:

- **Closed**: normal. Failures measured.
- **Open**: calls fail fast without hitting the downstream.
- **Half-open**: after cooldown, limited probes. Enough successes close; failures reopen.

### Failure mode prevented

A slow dependency is worse than a dead one. If `payments-api` accepts TCP
connections but takes 30s to respond, every handler goroutine blocks. The service
keeps receiving traffic, all workers become occupied, unrelated endpoints fail.

Concrete scenario: a cache cluster routing bug causes Redis misses to go slow.
Every handler waits on Redis; the service exhausts goroutines and connection
pools. A circuit breaker opens after a measured failure window, returns fast
errors, and preserves capacity.

### Decision criteria

**Use when:**
- Dependency can become slow or partially unavailable.
- Failure is correlated, not purely per-request.
- Service has a fallback, degraded path, or explicit error response.
- Downstream has limited capacity and retries would worsen recovery.

**Do not use when:**
- Operation is local, cheap, and already bounded.
- Traffic too low for statistical breaker (will flap).
- Cannot tolerate fail-fast and have no fallback.
- Substitute for timeouts (breaker measures outcomes; timeout bounds a single call).

**Idempotency note:** idempotency is NOT required for a breaker to reject calls.
It matters for retries and hedges, not breakers. The subtle issue is half-open
probing: prefer a health/read probe for non-idempotent writes rather than using
a business write as a probe.

### Trip thresholds (defaults)

- Minimum volume: **20 requests** before using failure-rate threshold.
- Failure-rate trip: **>=50%** over **30-60 second** rolling window.
- Low-QPS alternative: **5 consecutive failures**.
- Open delay: **30 seconds** initially, optionally with jitter.
- Half-open probes: **1-3 concurrent**.
- Success threshold: **2-5 successes** before closing.

Never trip on one error in production. Failsafe-go's default builder can open
after one failure if unconfigured -- generated code must explicitly set thresholds.

### Go implementation

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
    switch resp.StatusCode {
    case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
        return true
    default:
        return resp.StatusCode >= 500
    }
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

Standalone gobreaker version:

```go
package downstream

import (
    "context"
    "errors"
    "time"

    gobreaker "github.com/sony/gobreaker/v2"
)

type Result struct {
    Value string
}

type InventoryClient struct {
    cb *gobreaker.CircuitBreaker[Result]
}

func NewInventoryClient() *InventoryClient {
    return &InventoryClient{
        cb: gobreaker.NewCircuitBreaker[Result](gobreaker.Settings{
            Name:        "inventory-api",
            MaxRequests: 2,                // half-open probes
            Interval:    60 * time.Second, // rolling window reset
            Timeout:     30 * time.Second, // open-state cooldown
            ReadyToTrip: func(c gobreaker.Counts) bool {
                if c.Requests < 20 {
                    return false
                }
                return float64(c.TotalFailures)/float64(c.Requests) >= 0.50
            },
            IsExcluded: func(err error) bool {
                return errors.Is(err, context.Canceled)
            },
        }),
    }
}

func (c *InventoryClient) Call(ctx context.Context) (Result, error) {
    return c.cb.Execute(func() (Result, error) {
        return doRemoteCall(ctx)
    })
}
```

### Interactions

- **+ retry:** retry only when breaker closed/half-open and budget allows. Open-circuit errors abort retry immediately.
- **+ timeout:** every attempt still needs a timeout. Breaker opens after observing failures.
- **+ bulkhead:** bulkhead prevents slow dependency consuming all concurrency before breaker has signal.
- **+ load shedding:** inbound rejections must not count as downstream failures.

### Common mistakes

- No per-attempt timeouts.
- Counting caller cancellations as dependency failures.
- Counting bulkhead/rate-limit/budget rejections as dependency failures.
- Global breaker for multiple downstreams.
- Thresholds too low -- breaker flapping.
- Returning generic `500` instead of degraded/overload response.
- No breaker-state metrics and alerts.

---

## 2. Retry with Budget

### Default recommendation

**At most two retries** for safe outbound reads. Exponential backoff with jitter.
A **shared system-level retry budget** per dependency: **20% additional retry
traffic** (matching Finagle/Linkerd production defaults). Use `failsafe-go/budget`.

### Failure mode prevented

Per-call retries prevent transient failures (connection reset, brief blip, single
bad replica, `503` during deploy).

Retry budgets prevent retry storms. Scenario: `catalog-api` returns `503` after
a bad deploy. Every frontend and worker retries 3x. The dependency receives more
load exactly when it has less capacity, and retry traffic prevents recovery.

In a 5-deep call stack with 3 retries per layer: 3^5 = 243x load amplifier.

### Decision criteria

**Retry when all true:**
- Idempotent or protected by idempotency key.
- Original deadline has remaining budget.
- Error is transient: connection reset, `502`/`503`/`504`, gRPC `UNAVAILABLE`/`DEADLINE_EXCEEDED`.
- Retry budget has capacity.
- Not retrying at multiple layers for same logical operation.

**Do not retry when:**
- Caller context canceled or deadline expired.
- Non-idempotent write without idempotency key.
- Permanent client error (4xx, validation, auth).
- Dependency open-circuited.
- Service overloaded without signaling retry is useful.

### Per-call defaults

```
max attempts:     3 total (1 original + 2 retries)
initial backoff:  25-100 ms
max backoff:      500 ms - 2s (depends on endpoint SLO)
jitter:           full jitter or >= 50%
total deadline:   inherited from caller; never extended by retry
retryable:        GET/HEAD and explicitly idempotent POST/PUT with idempotency key
```

### Go implementation

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
        WithMaxRate(0.20).     // retries <= 20% of original traffic
        WithMinConcurrency(3). // small services still get a floor
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

### Interactions

- **+ timeout:** total deadline outside retry loop. Per-attempt timeout inside. Never sleep past remaining deadline.
- **+ circuit breaker:** do not retry open-circuit errors.
- **+ load shedding/backpressure:** respect `429`, `503`, `Retry-After`. Retrying overload without budget is a storm.
- **+ hedging:** retries and hedges share one extra-work budget.
- **+ bulkhead:** each retry consumes downstream concurrency. Do not hold a permit while sleeping backoff.

### Common mistakes

- Retrying at every layer of a call stack.
- No retry budget, only per-call `maxAttempts`.
- Retrying after `context.Canceled` or expired deadlines.
- Retrying non-idempotent writes without idempotency keys.
- Missing jitter.
- Sleeping inside a bulkhead slot.
- Ignoring `Retry-After`.
- Not emitting attempt count, budget exhaustion, and retry latency metrics.

---

## 3. Load Shedding

### Default recommendation

Use **server-side adaptive concurrency limiting** as the default inbound
load-shedding mechanism. In Go, use **Failsafe-go `adaptivelimiter`** for
latency/inflight-based adaptive limiting. Use a static semaphore for simple,
well-known limits.

Load shedding differs from rate limiting:
- **Rate limiting** enforces a contract ("tenant A gets 100 RPS").
- **Load shedding** protects the service when overloaded, regardless of contract compliance.

### Failure mode prevented

Load shedding prevents brownout collapse. Scenario: marketing launch doubles
traffic. Without shedding, every request enters the handler, allocates memory,
parses JSON, waits in queues, times out. Goodput falls below nominal capacity.
With shedding, excess/low-priority requests are rejected early, preserving
capacity for completable work.

### Decision criteria

**Use when:**
- Service can become CPU, memory, goroutine, connection, or queue constrained.
- Offered work exceeds completable work within deadlines.
- Upstream clients can retry later, degrade, or route elsewhere.
- Mixed-priority traffic.
- Autoscaling is slower than traffic spikes.

**Adaptive vs static:**
- Adaptive: capacity changes due to autoscaling, noisy neighbors, GC, dependency latency, mixed request costs.
- Static: hard local resource limit ("only 200 concurrent expensive reports").

### Go implementation: adaptive limiter

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

### Go implementation: static semaphore shedder

```go
package inbound

import (
    "encoding/json"
    "net/http"
    "strconv"
    "time"

    "golang.org/x/sync/semaphore"
)

func StaticConcurrencyShedder(maxConcurrent int64, retryAfter time.Duration) func(http.Handler) http.Handler {
    sem := semaphore.NewWeighted(maxConcurrent)

    return func(next http.Handler) http.Handler {
        return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
            if !sem.TryAcquire(1) {
                if retryAfter > 0 {
                    w.Header().Set("Retry-After", strconv.Itoa(int(retryAfter.Seconds())))
                }
                w.Header().Set("Content-Type", "application/json")
                w.WriteHeader(http.StatusServiceUnavailable)
                json.NewEncoder(w).Encode(struct {
                    Error string `json:"error"`
                }{Error: "service overloaded"})
                return
            }
            defer sem.Release(1)

            next.ServeHTTP(w, r)
        })
    }
}
```

### Priority shedding

Classify request priority early and cheaply: route, auth principal, tenant tier,
API key, or explicit priority header. Do not perform expensive work to decide
whether to shed.

Default priority order:
1. Health checks (load balancer probes).
2. User-visible, paid, or critical operations.
3. Internal control-plane work.
4. Background/batch work.
5. Crawlers, speculative prefetch, analytics.

### Interactions

- **+ retry budget:** overload responses must not trigger unlimited retries.
- **+ circuit breaker:** inbound shed != downstream failure.
- **+ backpressure:** shedding is local; backpressure is the signal sent upstream.
- **+ bulkhead:** inbound shedding is service-level bulkhead; downstream bulkheads are separate.

### Common mistakes

- Implementing only RPS rate limits and calling it load shedding.
- Queueing thousands of requests, shedding only after latency is bad.
- Doing expensive auth/parsing/tracing before the shed decision.
- Returning `500` (makes clients treat overload as application failure).
- Shedding health checks before ordinary traffic.
- Polluting success latency histograms with fast shed responses.
- No overload tests.

---

## 4. Hedged Requests

### Default recommendation

Use hedging only for **idempotent reads** where tail latency matters and the
downstream has spare capacity. Use **Failsafe-go `hedgepolicy`** with a shared
budget. Alternative: `cristalhq/hedgedhttp` for pure HTTP transport-level hedging.

Dean and Barroso's "The Tail at Scale": send a duplicate request to another
replica after a delay, use the first response, cancel the rest. Delay the hedge
until the request exceeds a high percentile (p95) to limit extra load.

### Failure mode prevented

One straggling replica dominates tail latency. Scenario: search fans out to 100
shards. 99 reply in 10ms; one is stuck behind GC and replies in 800ms. Without
hedging, the user waits. With hedging after p95, a duplicate goes to a different
replica and the faster result wins.

### Decision criteria

**Use when:**
- Idempotent read.
- Multiple replicas can serve the same request.
- Downstream has spare capacity.
- p99/p99.9 matters and is caused by stragglers (not uniform overload).
- Losing request can be canceled.

**Do not use when:**
- Mutates state.
- Read has side effects ("mark as read," "charge," "reserve").
- Downstream overloaded.
- Hedge goes to the same instance.
- Cannot cancel losing attempts.
- Retries already consuming extra-work budget.

### Defaults

```
hedge delay:  rolling p95 latency per dependency + route
max hedges:   1
budget:       shared with retries, 5-20% extra work
```

Track per dependency and operation. Exclude shed responses and open-circuit
failures from the latency estimator.

### Go implementation

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

### Interactions

- **+ budget:** hedges consume the same extra-work budget as retries.
- **+ bulkhead:** every hedge consumes downstream concurrency. Size for total attempts.
- **+ circuit breaker:** do not hedge when circuit open. Canceled losers are not failures.
- **+ load shedding:** disable/reduce hedging when shedding is active.
- **+ priority:** mark hedges lower priority than primary requests when downstream supports it.

### Common mistakes

- Hedging writes.
- Not canceling losers.
- Hedging at p50 (doubles load).
- Hedging to same backend instance.
- Counting canceled losers as failures.
- No budget for extra work.
- Running hedging and retries independently (one request explodes into many attempts).

---

## 5. Bulkheading

### Default recommendation

Create **one bulkhead per downstream dependency and expensive local resource**.
Use Failsafe-go `bulkhead` for policy composition. Use separate HTTP
clients/transports or connection pools per dependency. Use `x/sync/semaphore` for
simple local concurrency caps.

### Failure mode prevented

Resource starvation across failure domains. Scenario: `recommendations-api`
becomes slow. If all outbound calls share one pool, checkout/login/recommendations
all compete for blocked resources. With a separate recommendations bulkhead, only
recommendations degrades; checkout still reaches payments.

### Decision criteria

**Use when:**
- Downstream can be slow or unavailable.
- Multiple downstreams or mixed workloads.
- Resource has natural limit (DB connections, HTTP connections, goroutines).
- Failure of one dependency should not starve others.

**Overkill when:**
- Single simple dependency with strict inbound limit.
- Local cheap operation.
- Limit so high it never binds.

### Sizing rule

Start with Little's Law:

```
concurrency ~ healthy_peak_rps x healthy_p99_latency
```

Then cap by downstream contract, local memory/goroutine budget, connection-pool
size, retry/hedge amplification, and priority reservations.

Example: `payments-api` handles 250 RPS, healthy p99 is 80ms.
Baseline: 250 x 0.08 = 20. Start around 25-40.

### Go implementation: Failsafe-go

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

### Go implementation: dedicated HTTP transport

Use the full HTTP client template from [Timeouts as a System](#7-timeouts-as-a-system),
setting `maxConnsPerHost` to match the bulkhead size. Generate one client/transport
per downstream class when limits differ.

### Go implementation: semaphore bulkhead

```go
type Bulkhead struct {
    sem *semaphore.Weighted
}

func NewBulkhead(max int64) *Bulkhead {
    return &Bulkhead{sem: semaphore.NewWeighted(max)}
}

func (b *Bulkhead) Do(ctx context.Context, fn func(context.Context) error) error {
    if !b.sem.TryAcquire(1) {
        return ErrBulkheadFull
    }
    defer b.sem.Release(1)
    return fn(ctx)
}
```

Use `TryAcquire` or very short acquire timeout for request-path work.

### Interactions

- **+ timeout:** timeouts release bulkhead slots. Without them, permits held indefinitely.
- **+ retry:** each attempt acquires a permit. Do not hold permit while sleeping backoff.
- **+ circuit breaker:** breaker fail-fast reduces pressure on bulkhead.
- **+ hedging:** hedged request = another concurrent attempt. Count it.
- **+ load shedding:** inbound limit is service-level bulkhead; downstream bulkheads isolate deps from each other.

### Common mistakes

- One global outbound semaphore for all dependencies.
- Unlimited goroutine fanout inside a request.
- Sharing single connection pool with no per-downstream limits.
- Waiting too long for a permit.
- Holding permit while sleeping backoff.
- Forgetting `defer Release()`.
- Sizing from average latency rather than p99.
- Not exporting `in_use`, `wait_time`, and `rejected` metrics.

---

## 6. Backpressure Propagation

### Default recommendation

Backpressure is how a service says "slow down" to upstream callers. Generate
explicit protocol-level signals and make clients respect them.

| Situation | HTTP | gRPC |
|---|---|---|
| Tenant/client exceeded quota | `429 Too Many Requests` | `RESOURCE_EXHAUSTED` |
| Global overload, may recover | `503 Service Unavailable` + `Retry-After` | `RESOURCE_EXHAUSTED` or `UNAVAILABLE` |
| Caller deadline expired | `504` at gateway; cancel in service | `DEADLINE_EXCEEDED` |
| Dependency unavailable | `502`/`503`/`504` | `UNAVAILABLE` |
| Request invalid | `400` | `INVALID_ARGUMENT` |

### Failure mode prevented

Hidden queues and futile work. Scenario: batch client sends 20k RPS to an API
handling 5k RPS. If the API accepts everything and queues internally, all clients
see timeouts. If it quickly returns `429/503` with `Retry-After`, well-behaved
clients slow down, budgets limit extra work, and latency is preserved for accepted
traffic.

### Go implementation: HTTP overload response

```go
func WriteOverload(w http.ResponseWriter, perClient bool, retryAfter time.Duration) {
    if retryAfter > 0 {
        seconds := int(retryAfter.Round(time.Second).Seconds())
        if seconds < 1 {
            seconds = 1
        }
        w.Header().Set("Retry-After", strconv.Itoa(seconds))
    }
    w.Header().Set("Content-Type", "application/json")
    if perClient {
        w.WriteHeader(http.StatusTooManyRequests)
        json.NewEncoder(w).Encode(struct {
            Error string `json:"error"`
        }{Error: "too many requests"})
        return
    }
    w.WriteHeader(http.StatusServiceUnavailable)
    json.NewEncoder(w).Encode(struct {
        Error string `json:"error"`
    }{Error: "service overloaded"})
}
```

### Go implementation: gRPC overload response

```go
import (
    "google.golang.org/grpc/codes"
    "google.golang.org/grpc/status"
)

func rejectConcurrencyLimit() error {
    return status.Error(codes.ResourceExhausted, "concurrency limit exceeded")
}
```

### Client behavior

Generated clients must:
- Treat `429`, `503`, gRPC `RESOURCE_EXHAUSTED` as overload signals.
- Respect `Retry-After`.
- Retry only if safe and budget permits.
- Use jittered backoff when `Retry-After` absent.
- Stop immediately when caller context canceled.
- Export overload response metrics separately from application failures.

### Adaptive concurrency limits

Use Failsafe-go `adaptivelimiter` for Go services. It uses TCP Vegas-style
adaptive algorithms, tracking inflight requests and latency to adjust limits
dynamically.

### Interactions

- **+ retry budget:** backpressure is useful only if callers don't retry without bound.
- **+ load shedding:** shedding is local admission control; backpressure is the protocol response.
- **+ circuit breaker:** persistent overload from a downstream may warrant opening.
- **+ priority:** low-priority traffic receives backpressure first.

### Common mistakes

- Returning `500` for overload.
- Omitting `Retry-After`.
- Treating `RESOURCE_EXHAUSTED` as generic application error.
- Letting clients queue infinitely.
- Not canceling server work after client goes away.
- Static RPS limits for variable-cost workloads.
- One adaptive limiter across unrelated endpoints with different latency profiles.
- Overload logging so expensive that rejection itself becomes the bottleneck.

---

## 7. Timeouts as a System

### Default recommendation

Every inbound request and outbound call uses a `context.Context` deadline. One
**overall request deadline** and smaller **child budgets** for downstream calls.
Configure both connection-establishment and request/response timeouts for HTTP
clients.

### Failure mode prevented

Resource leaks and call-chain budget blowups. Scenario: frontend has 300ms SLO,
calls A with 1s timeout, A calls B with 1s, B calls C with 1s. User gives up at
300ms, lower layers keep working for seconds, wasting capacity.

### Timeout budget concept

```
incoming request deadline: 300 ms
local validation/render:    50 ms
inventory call budget:      80 ms
payments call budget:      120 ms
retry/backoff allowance:    30 ms
final response reserve:     20 ms
```

Never generate `context.WithTimeout(context.Background(), ...)` inside a handler.
Child contexts derive from parent request context.

### Go helper: child deadline

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

Usage:

```go
ctx, cancel := WithChildTimeout(parentCtx, 80*time.Millisecond)
defer cancel()
resp, err := inventoryClient.Get(ctx, sku)
```

### Per-attempt vs overall timeout

Use both:
- **Overall deadline**: inherited from inbound request or job.
- **Per-attempt timeout**: one timeout per actual outbound attempt.

In Failsafe-go: timeout outside retry = cancels whole sequence; timeout inside
retry = per-attempt.

```go
// Correct: overall from ctx, per-attempt from composed timeout policy.
failsafe.With[*http.Response](
    retryPolicy,
    circuitBreaker,
    bulkhead,
    perAttemptTimeout,
).WithContext(ctx).GetWithExecution(call)
```

### HTTP client timeout configuration

| Knob | Bounds |
|---|---|
| `net.Dialer.Timeout` | TCP connect |
| `TLSHandshakeTimeout` | TLS handshake |
| `ResponseHeaderTimeout` | Waiting for response headers after request write |
| Request `Context` | Logical request + cancellation propagation |
| `http.Client.Timeout` | Hard cap for whole request including body read (dangerous for streaming) |

```go
func NewHTTPClient(maxConnsPerHost int) *http.Client {
    dialer := &net.Dialer{
        Timeout:   100 * time.Millisecond,
        KeepAlive: 30 * time.Second,
    }
    transport := &http.Transport{
        Proxy:                 http.ProxyFromEnvironment,
        DialContext:           dialer.DialContext,
        TLSHandshakeTimeout:  200 * time.Millisecond,
        ResponseHeaderTimeout: 500 * time.Millisecond,
        ExpectContinueTimeout: 1 * time.Second,
        MaxConnsPerHost:       maxConnsPerHost,
        MaxIdleConns:          maxConnsPerHost * 2,
        MaxIdleConnsPerHost:   maxConnsPerHost,
        IdleConnTimeout:       90 * time.Second,
    }
    return &http.Client{Transport: transport}
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

### Interactions

- **+ retry:** never retry if remaining deadline < attempt + backoff.
- **+ circuit breaker:** timeout indicating downstream slowness is a breaker signal; caller cancellation is not.
- **+ bulkhead:** timeout releases scarce concurrency.
- **+ hedging:** losing hedges canceled through context.
- **+ backpressure:** if inbound context canceled, stop work immediately.

### Common mistakes

- `context.Background()` inside request handling.
- Each downstream gets a fixed timeout longer than parent deadline.
- Only request timeout, no connect/TLS/header timeout.
- Relying on `http.Client.Timeout` for streaming.
- Missing `defer cancel()`.
- Not closing response bodies.
- Continuing work after `ctx.Done()`.
- Treating caller deadline expiration as dependency failure.
- Sleeping backoff when remaining deadline can't fit another attempt.

---

## Cross-Pattern Implementation Rules

### Generate one wrapper per downstream

Do not scatter policy creation at call sites. Generate a dependency client:

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
bulkhead size, metrics labels, idempotency rules, and overload handling.

### Classify errors explicitly

Generated code must distinguish:
- Caller cancellation
- Caller deadline exceeded
- Per-attempt downstream timeout
- Open circuit
- Bulkhead full
- Retry budget exhausted
- Rate limit / load shed rejection
- Downstream `5xx`
- Permanent client/application errors

This drives metrics and prevents one policy from corrupting another's signal.

### Minimum viable metrics

Every generated boundary wrapper should emit at minimum:

```
outbound_attempts_total{dependency, method, outcome}
outbound_latency_seconds{dependency, method, outcome}
circuit_breaker_state{dependency}
bulkhead_in_use{dependency}
load_shed_total{endpoint}
```

Full observability adds:

```
outbound_attempts_total{dependency, method, outcome, attempt, is_retry, is_hedge}
retry_budget_exhausted_total{dependency}
circuit_breaker_transitions_total{dependency, from, to}
bulkhead_rejected_total{dependency}
bulkhead_wait_seconds{dependency}
adaptive_limit{endpoint}
load_shed_total{endpoint, priority, reason}
backpressure_responses_total{status_or_code, reason}
deadline_remaining_ms{endpoint}
```

Do not mix fast shed/open-circuit failures into successful downstream latency
histograms.

Label names follow OTel semantic conventions where applicable (e.g., `http.method`,
`http.status_code`). The labels shown here (`dependency`, `method`, `outcome`) are
application-domain attributes without an OTel convention.

### Testing requirements

Generate tests or test hooks for:
- Downstream hangs longer than per-attempt timeout
- Connection refused
- Slow TLS/connect
- `503` with `Retry-After`
- `429` with `Retry-After`
- Caller cancellation
- Retry budget exhaustion
- Circuit open and half-open recovery
- Bulkhead full
- Hedged request cancellation
- Inbound overload

Use Toxiproxy or equivalent for integration tests.

---

## Decision Matrix

| Pattern | Default | Use when | Do not use when |
|---|---|---|---|
| Circuit breaker | Failsafe-go per dependency | Correlated downstream slowness/failure | No fallback, low traffic, local cheap call |
| Retry with budget | Failsafe-go + budget, max 2 retries | Safe/idempotent transient failures | Non-idempotent writes, expired deadline, no budget |
| Load shedding | Failsafe-go adaptive limiter on inbound | Overload reduces goodput | Substitute for tenant quota |
| Hedged requests | Failsafe-go hedge + budget, p95 delay, max 1 | Idempotent replicated reads with tail latency | Writes, overloaded downstream, no cancellation |
| Bulkheading | Failsafe-go + per-downstream pools | Slow dep must not starve others | Single trivial dep already bounded |
| Backpressure | HTTP `429/503` + `Retry-After`; gRPC `RESOURCE_EXHAUSTED` | Rejected due to quota/overload | Hiding overload as success |
| Timeouts | Parent deadline + child budgets + transport timeouts | Every boundary call | Never optional |
