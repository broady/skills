# Async Work

## External message brokers (preferred)

For durable async processing in production, use an external message broker
(SQS, Cloud Pub/Sub, NATS JetStream, Kafka). The broker handles persistence,
retry, dead-letter, and horizontal scaling. Your service is a **consumer**:

```go
type Consumer[T any] struct {
    subscriber Subscriber
    handler    func(ctx context.Context, msg T) error
    logger     *slog.Logger
}

func (c *Consumer[T]) Run(ctx context.Context) error {
    for {
        msg, err := c.subscriber.Receive(ctx)
        if err != nil {
            if ctx.Err() != nil {
                return nil // clean shutdown
            }
            return fmt.Errorf("receive: %w", err)
        }
        if err := c.process(ctx, msg); err != nil {
            c.logger.LogAttrs(ctx, slog.LevelError, "processing failed",
                slog.String("msg_id", msg.ID()),
                slog.Any("err", err),
            )
            msg.Nack() // broker will redeliver after visibility timeout
            continue
        }
        msg.Ack()
    }
}
```

## Retry with backoff

For transient failures (downstream timeout, temporary unavailability), retry
with exponential backoff and jitter. Bound attempts, and require callers to pass
a deadline-bearing context when total duration matters:

```go
func retry(ctx context.Context, maxAttempts int, base time.Duration, fn func(ctx context.Context) error) error {
    var err error
    d := base
    for attempt := range maxAttempts {
        err = fn(ctx)
        if err == nil {
            return nil
        }
        if !isRetryable(err) {
            return err
        }
        if attempt == maxAttempts-1 {
            break
        }
        // Exponential backoff with jitter.
        jitter := time.Duration(rand.Int64N(int64(d) / 4))
        timer := time.NewTimer(d + jitter)
        select {
        case <-ctx.Done():
            timer.Stop()
            return ctx.Err()
        case <-timer.C:
            d = min(d*2, 30*time.Second) // cap
        }
    }
    return fmt.Errorf("after %d attempts: %w", maxAttempts, err)
}
```

## At-least-once delivery

Most brokers provide at-least-once semantics — your handler may be called more
than once for the same message. Make handlers **idempotent**:

- Use a unique message/event ID as a deduplication key.
- Check "already processed" before doing work (idempotency table, conditional write).
- Design state transitions to be safe to repeat (inserting with ON CONFLICT DO NOTHING,
  updating with WHERE version = ?).

## In-process queues (specific use cases)

In-process persistent queues (backed by LevelDB, SQLite, or Redis) are useful
when:
- You're a single-binary deployment without access to a managed broker.
- Work items are cheap to lose on crash but should survive restarts.
- You need dynamic worker scaling within one process.

The key design: handlers return unprocessed items explicitly:

```go
type HandlerFunc[T any] func(items ...T) (unhandled []T)
```

Unhandled items are requeued with backoff. This makes partial failure visible in
the type system — you can't accidentally drop work by returning `nil, nil`.

## Decision table

| Situation | Use |
|---|---|
| N tasks, all must succeed, bounded time | `errgroup.WithContext` + `SetLimit` |
| N tasks, best-effort, collect results | `safe.Collect` |
| Durable delivery, horizontal scale, dead-letter | External broker (SQS, Pub/Sub, NATS) |
| Single-binary, must survive restart, no broker available | In-process persistent queue |
| Transient failure in synchronous call | `retry` with bounded attempts + backoff |
