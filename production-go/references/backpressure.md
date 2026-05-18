# Backpressure

Multi-layered flow control patterns extracted from NATS Server, OTel Collector,
Loki, Traefik, kafka-go, and Caddy. Never rely on a single mechanism.

## Contents

- [1. Backpressure Layers](#1-backpressure-layers)
- [2. Bounded Queue Patterns](#2-bounded-queue-patterns)
- [3. Slow Consumer Handling (NATS Pattern)](#3-slow-consumer-handling-nats-pattern)
- [4. Per-Tenant Rate Limiting](#4-per-tenant-rate-limiting)
- [5. Memory-Based Admission Control (OTel)](#5-memory-based-admission-control-otel)
- [6. Context Ownership Transfer](#6-context-ownership-transfer)
- [Decision Table](#decision-table)
- [Anti-Patterns](#anti-patterns)

## 1. Backpressure Layers

| Layer | Mechanism | Example | When it fires |
|-------|-----------|---------|---------------|
| 1. Admission control | Reject at the gate | OTel memory limiter, HTTP `MaxHeaderBytes` | Before any processing starts |
| 2. Queue capacity bounds | Bounded channels/queues | OTel bounded queue exporter | Pipeline stages cannot keep up |
| 3. Rate limiting | Per-tenant, per-connection | Traefik `golang.org/x/time/rate` | Contractual throughput limits |
| 4. Slow consumer detection | Pending bytes threshold | NATS: close when pending > max | Single consumer drags down system |
| 5. Write deadline | Per-connection type policy | NATS: 10s clients, retry routes | Writes stall on full network buffer |

Every pipeline needs at least layers 1, 2, and 5. Layers 3 and 4 apply to
multi-tenant and pub/sub systems respectively.

---

## 2. Bounded Queue Patterns

### Block on overflow (OTel)

Callers block on a bounded channel until space is available or the context is
cancelled. Use when data loss is unacceptable and upstream tolerates latency
spikes.

```go
func (q *BlockingQueue[T]) Put(ctx context.Context, item T) error {
	select {
	case q.ch <- item:
		return nil
	case <-q.closed:
		return errors.New("queue closed")
	case <-ctx.Done():
		return ctx.Err()
	}
}
```

### Reject on overflow (OTel)

```go
var ErrQueueFull = errors.New("queue is full")

func (q *BoundedQueue[T]) Offer(item T) error {
	select {
	case q.ch <- item:
		return nil
	default:
		return ErrQueueFull
	}
}
```

### Drop oldest on overflow (Prometheus notifier)

Evict the oldest item. Use for alerting queues where newest data is most
valuable.

```go
func (q *DropOldestQueue[T]) Push(item T) (dropped bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.count == len(q.buf) {
		q.head = (q.head + 1) % len(q.buf)
		q.count--
		dropped = true
	}
	idx := (q.head + q.count) % len(q.buf)
	q.buf[idx] = item
	q.count++
	return dropped
}
```

---

## 3. Slow Consumer Handling (NATS Pattern)

Three-layer defense for systems where producers must not stall on behalf of
one consumer.

**Layer 1 -- Soft stall:** When outbound buffer > 75% capacity, create a stall
channel. Producers block 2-5ms cooperatively before each write.

```go
func (c *Connection) waitIfStalled(ctx context.Context) error {
	c.mu.Lock()
	stall := c.stallCh
	c.mu.Unlock()
	if stall == nil {
		return nil
	}
	select {
	case <-stall:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	default:
		timer := time.NewTimer(5 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-stall:
			return nil
		case <-timer.C:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
```

**Layer 2 -- Hard pending limit:** When pending bytes > max, close the
connection immediately. The consumer cannot recover.

```go
func (c *Connection) queueWrite(data []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.outBuf.Len()+len(data) > c.outBufMax {
		c.closeSlowConsumer()
		return errors.New("slow consumer: pending bytes exceeded limit")
	}
	_, err := c.outBuf.Write(data)
	return err
}
```

**Layer 3 -- Write deadline:** Per-connection type policy. Close clients on
timeout (10s default), retry infrastructure connections (30s).

```go
var clientPolicy = WritePolicy{Deadline: 10 * time.Second, OnExpiry: (*Connection).Close}
var routePolicy  = WritePolicy{Deadline: 30 * time.Second, OnExpiry: (*Connection).scheduleReconnect}

func (c *Connection) writeWithDeadline(data []byte, p WritePolicy) error {
	_ = c.netConn.SetWriteDeadline(time.Now().Add(p.Deadline))
	if _, err := c.netConn.Write(data); err != nil {
		p.OnExpiry(c)
		return fmt.Errorf("write to %s: %w", c.name, err)
	}
	return nil
}
```

---

## 4. Per-Tenant Rate Limiting

### Token bucket per source (Traefik)

```go
type PerSourceLimiter struct {
	mu       sync.Mutex
	limiters map[string]*rate.Limiter
	rate     rate.Limit
	burst    int
	maxSrcs  int // bounded tracking: max 65536 entries
}

func (l *PerSourceLimiter) Allow(source string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	lim, ok := l.limiters[source]
	if !ok {
		if len(l.limiters) >= l.maxSrcs {
			return false // refuse unknown sources when table is full
		}
		lim = rate.NewLimiter(l.rate, l.burst)
		l.limiters[source] = lim
	}
	return lim.Allow()
}
```

**Limitation:** This implementation has no eviction — once the map reaches
`maxSrcs`, all new unknown sources are refused permanently. For long-running
services with rotating source IPs, add an LRU or probabilistic LRU to evict
stale entries. Cache eviction is a deep topic; use a proven library rather
than hand-rolling one.

Always return `Retry-After` header on 429 responses.

### Runtime-reloadable limits (Loki)

Decouple limit values from limiter lifecycle. Swap overrides atomically on
config reload without restarting. See [config.md](config.md) for the broader
hot-reload pattern and per-tenant fallback-to-defaults approach.

```go
type TenantLimits struct {
	defaultRate rate.Limit
	overrides   atomic.Pointer[map[string]rate.Limit]
}

func (t *TenantLimits) RateFor(tenant string) rate.Limit {
	if o := t.overrides.Load(); o != nil {
		if r, ok := (*o)[tenant]; ok {
			return r
		}
	}
	return t.defaultRate
}

func (t *TenantLimits) Reload(o map[string]rate.Limit) { t.overrides.Store(&o) }
```

---

## 5. Memory-Based Admission Control (OTel)

Periodic memory check sets an atomic flag. Incoming data is rejected at the
outermost receiver before allocating buffers.

```go
type MemoryLimiter struct {
	mustRefuse atomic.Bool
	limit      uint64
	interval   time.Duration
}

func (m *MemoryLimiter) Run(ctx context.Context) error {
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			var stats runtime.MemStats
			runtime.ReadMemStats(&stats)
			m.mustRefuse.Store(stats.Alloc > m.limit)
		}
	}
}

func (m *MemoryLimiter) Admit() error {
	if m.mustRefuse.Load() {
		return errors.New("memory limit exceeded: rejecting data")
	}
	return nil
}
```

---

## 6. Context Ownership Transfer

When work moves from request scope to a background queue, strip cancellation
but preserve values using `context.WithoutCancel` (Go 1.21+).

```go
func (q *WorkQueue) Enqueue(ctx context.Context, work Work) error {
	bgCtx := context.WithoutCancel(ctx) // keeps trace/tenant values

	select {
	case q.ch <- queuedWork{ctx: bgCtx, work: work}:
		return nil
	default:
		return ErrQueueFull
	}
}
```

**Use for:** background pipelines, async notifications, audit log writes.
**Do not use for:** synchronous handling where the caller waits for the result.

---

## Decision Table

| Situation | Strategy | Overflow behavior |
|-----------|----------|-------------------|
| Data loss unacceptable, can wait | Block on overflow | Caller blocks until space available |
| Data loss unacceptable, latency-sensitive | Reject + caller retry | `ErrQueueFull`, retry with backoff |
| Notification/alerting queue | Drop oldest | Evict stale, newest always delivered |
| Pub/sub, mixed consumer speeds | Slow consumer detection | Stall, pending limit, write deadline |
| Multi-tenant ingestion | Per-tenant rate + memory admission | 429 + Retry-After; 503 on memory pressure |
| Request to background handoff | `context.WithoutCancel` + bounded queue | Reject at queue; work survives request |

---

## Anti-Patterns

- **Unbounded queues.** Every queue needs capacity derived from memory budget
  and processing rate.
- **Single-layer backpressure.** One mechanism is always insufficient under
  real load. Combine admission control, queue bounds, and deadlines.
- **Ignoring slow consumers.** One stalled consumer blocks all publishers.
  Detect and disconnect or isolate.
- **`time.Sleep` as backpressure.** Fixed sleeps do not adapt. Use `sync.Cond`,
  channel select, or `rate.Limiter.Wait`.
- **Dropping without metrics.** Every drop must increment a counter
  (`backpressure_dropped_total{queue, reason}`).
- **Passing request context into background queues.** Work dies when the HTTP
  request returns. Use `context.WithoutCancel`.
- **Memory check only at startup.** Use a periodic ticker (100ms-1s) and an
  atomic flag.
- **Hardcoded per-tenant limits.** Require a deploy to change. Store overrides
  in dynamic config and swap atomically.
- **Blocking in the reject path.** The "queue full" code path must be
  non-blocking. No slow locks or allocations.
