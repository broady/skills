# Controller Loops

The informer-queue-worker pattern used by Kubernetes, Cilium, and similar
control-plane systems for reconciling desired state against actual state.

## Contents

1. [Informer-Queue-Worker Architecture](#1-informer-queue-worker-architecture)
2. [Controller Skeleton](#2-controller-skeleton)
3. [Error Handling and Requeue](#3-error-handling-and-requeue)
4. [Cache Sync Gate](#4-cache-sync-gate)
5. [DeepCopy at Cache Boundaries](#5-deepcopy-at-cache-boundaries)
6. [Injectable Sync Handler](#6-injectable-sync-handler)
7. [SlowStartBatch for Cascading Mutations](#7-slowstartbatch-for-cascading-mutations)
8. [Reconciliation vs Event-Driven](#8-reconciliation-vs-event-driven)
9. [Decision Table](#decision-table)
10. [Anti-Patterns](#anti-patterns)

---

## 1. Informer-Queue-Worker Architecture

Four components compose the canonical controller loop:

**Informers** watch API resources and maintain a local read-only cache. A shared
informer factory creates one watch per resource type, shared across all
controllers in the process.

**Event handlers** registered on informers enqueue keys (namespace/name strings),
never full objects. The key is a stable identifier that the worker resolves
against the cache at processing time.

**Rate-limited work queue** with three-set deduplication. Internally the queue
tracks three sets: `queue` (ordered items to process), `dirty` (items needing
work), and `processing` (items currently being handled). An item being processed
can be re-marked dirty without duplication -- the queue will re-add it after the
current processing completes.

**Bounded worker pool** dequeues keys and calls the sync handler. Workers run in
a fixed-size pool controlled by the `workers` parameter to `Run()`.

---

## 2. Controller Skeleton

```go
type Controller struct {
    logger      *slog.Logger
    queue       workqueue.TypedRateLimitingInterface[string]
    informer    cache.SharedIndexInformer
    lister      appslisters.DeploymentLister
    syncHandler func(ctx context.Context, key string) error
}

func NewController(
    ctx context.Context,
    logger *slog.Logger,
    informer appsinformers.DeploymentInformer,
    client clientset.Interface,
) *Controller {
    c := &Controller{
        logger: logger,
        queue: workqueue.NewTypedRateLimitingQueueWithConfig(
            workqueue.DefaultTypedControllerRateLimiter[string](),
            workqueue.TypedRateLimitingQueueConfig[string]{Name: "mycontroller"},
        ),
        lister: informer.Lister(),
    }
    c.syncHandler = c.syncDeployment // injectable for testing

    informer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
        AddFunc:    func(obj interface{}) { c.enqueue(obj) },
        UpdateFunc: func(_, obj interface{}) { c.enqueue(obj) },
        DeleteFunc: func(obj interface{}) { c.enqueue(obj) },
    })

    return c
}

func (c *Controller) enqueue(obj interface{}) {
    key, err := cache.MetaNamespaceKeyFunc(obj)
    if err != nil {
        return
    }
    c.queue.Add(key) // enqueue key, NOT the object
}
```

### Run

```go
func (c *Controller) Run(ctx context.Context, workers int) {
    // Gate: never process items until cache is populated.
    if !cache.WaitForCacheSync(ctx.Done(), c.informer.HasSynced) {
        return
    }

    var wg sync.WaitGroup
    for range workers {
        wg.Go(func() {
            for c.processNextWorkItem(ctx) {
            }
        })
    }

    <-ctx.Done()
    c.queue.ShutDown() // unblocks workers waiting in queue.Get()
    wg.Wait()
}
```

### Process loop

```go
func (c *Controller) processNextWorkItem(ctx context.Context) bool {
    key, quit := c.queue.Get()
    if quit {
        return false
    }
    defer c.queue.Done(key)

    err := c.syncHandler(ctx, key)
    c.handleErr(ctx, err, key)
    return true
}
```

---

## 3. Error Handling and Requeue

Three outcomes after `syncHandler` returns:

| Outcome | Action | Effect |
|---|---|---|
| Success (`err == nil`) | `queue.Forget(key)` | Clears retry tracking for this key |
| Transient error, below max retries | `queue.AddRateLimited(key)` | Requeues with exponential backoff |
| Exceeded max retries | `queue.Forget(key)` | Drops the item, logs the failure |

The default rate limiter composes per-item exponential backoff (5ms base, 1000s
max) with a global token bucket (10 QPS, burst 100).

```go
const maxRetries = 15

func (c *Controller) handleErr(ctx context.Context, err error, key string) {
    if err == nil {
        c.queue.Forget(key)
        return
    }

    if c.queue.NumRequeues(key) < maxRetries {
        c.logger.LogAttrs(ctx, slog.LevelWarn, "sync failed, retrying",
            slog.String("key", key),
            slog.Any("err", err),
        )
        c.queue.AddRateLimited(key)
        return
    }

    c.logger.LogAttrs(ctx, slog.LevelError, "dropping item after max retries",
        slog.String("key", key),
        slog.Any("err", err),
    )
    c.queue.Forget(key)
}
```

---

## 4. Cache Sync Gate

Never process items until all informer caches have synced their initial list.
Without this gate, the controller acts on incomplete state -- it may see zero
replicas and attempt to create all of them, or miss existing resources and
create duplicates.

```go
// Block until the initial list is complete for ALL informers.
if !cache.WaitForCacheSync(ctx.Done(),
    c.deploymentSynced,
    c.replicaSetSynced,
    c.podSynced,
) {
    // Context was canceled before sync completed. Do not proceed.
    return
}
```

Place this call in `Run()` before launching any workers. Every informer the
controller reads from must be included.

---

## 5. DeepCopy at Cache Boundaries

Objects retrieved from the informer cache are shared read-only references. Every
controller sharing the informer sees the same pointer. Mutating a cached object
corrupts the local cache for all consumers.

```go
func (c *Controller) syncDeployment(ctx context.Context, key string) error {
    namespace, name, err := cache.SplitMetaNamespaceKey(key)
    if err != nil {
        return fmt.Errorf("split key: %w", err)
    }

    deployment, err := c.lister.Deployments(namespace).Get(name)
    if errors.IsNotFound(err) {
        return nil // deleted between enqueue and processing
    }
    if err != nil {
        return fmt.Errorf("get deployment: %w", err)
    }

    // DeepCopy before ANY mutation. The lister returns a shared cache reference.
    d := deployment.DeepCopy()
    d.Status.ObservedGeneration = d.Generation

    // ... reconcile ...
    return nil
}
```

---

## 6. Injectable Sync Handler

Store the sync handler as a function field, not a direct method call. This lets
tests substitute a test handler without mocking the queue, informer, or API
client.

```go
type Controller struct {
    // syncHandler is the function called for each work item.
    // Set to syncDeployment in production. Tests replace it.
    syncHandler func(ctx context.Context, key string) error
    // ...
}

// Production wiring (in constructor):
c.syncHandler = c.syncDeployment

// Test wiring:
c.syncHandler = func(ctx context.Context, key string) error {
    synced <- key
    return nil
}
```

This pattern is used by every Kubernetes built-in controller. The test exercises
the queue-worker machinery with a controlled sync function, verifying retry
behavior, error handling, and deduplication without real API calls.

---

## 7. SlowStartBatch for Cascading Mutations

When creating or deleting many dependent resources (e.g., pods for a
ReplicaSet), use slow-start batching: begin with batch size 1, double on
success, stop completely on the first failure in any batch.

This prevents thundering herd on the API server when quota is already exceeded
or the downstream is unhealthy. With batch size 1, only one doomed call is made
before the controller backs off.

```go
func slowStartBatch(count int, initialBatchSize int, fn func() error) (int, error) {
    remaining := count
    successes := 0
    for batchSize := min(remaining, initialBatchSize); batchSize > 0; batchSize = min(2*batchSize, remaining) {
        errCh := make(chan error, batchSize)
        var wg sync.WaitGroup
        for range batchSize {
            wg.Go(func() {
                if err := fn(); err != nil {
                    errCh <- err
                }
            })
        }
        wg.Wait()
        curSuccesses := batchSize - len(errCh)
        successes += curSuccesses
        if len(errCh) > 0 {
            return successes, <-errCh
        }
        remaining -= batchSize
    }
    return successes, nil
}
```

Kubernetes uses `SlowStartInitialBatchSize = 1`. The progression for creating
100 pods: 1, 2, 4, 8, 16, 32, 37 (remainder). If the first batch of 1 fails,
only 1 API call was wasted.

---

## 8. Reconciliation vs Event-Driven

Controllers are **level-triggered** (reconcile desired vs actual state), not
**edge-triggered** (react to individual events). The sync handler reads the
current state from the cache and computes the full diff, regardless of which
event caused the enqueue.

The queue provides natural level-triggering: multiple events for the same key
coalesce into one reconciliation. If a Deployment receives 10 rapid updates,
the queue deduplicates them into a single sync call that sees the final state.

**Consequences:**

- The sync handler must be idempotent. Running it twice with the same state
  produces the same outcome.
- Event handlers should be thin. Extract the key and enqueue. Do not compute
  diffs or make decisions in the event handler.
- Status updates from the controller's own writes trigger re-enqueue. The sync
  handler must handle re-entry without infinite loops (compare generation vs
  observed generation, or compare desired vs actual counts).

---

## Decision Table

| Question | Answer |
|---|---|
| How do I watch resources? | SharedInformer via a factory. One watch per resource type, shared across controllers. |
| What do I enqueue? | String keys (`namespace/name`), never full objects. |
| How do I bound concurrency? | Fixed worker pool size passed to `Run()`. |
| When do I start processing? | After `WaitForCacheSync` returns true for all informers. |
| How do I handle transient errors? | `AddRateLimited(key)` with exponential backoff up to `maxRetries`. |
| How do I handle permanent errors? | `Forget(key)` and log. Do not requeue indefinitely. |
| How do I test the sync logic? | Inject a test `syncHandler` function. |
| How do I batch mutations? | `slowStartBatch` with initial batch size 1, doubling on success. |
| How do I avoid stale data? | `DeepCopy()` before mutation. Read from lister, not from event object. |

---

## Anti-Patterns

- **Enqueuing full objects instead of keys.** The object may be stale by the
  time the worker processes it. Always re-fetch from the lister cache.
- **Processing before cache sync.** Acting on incomplete state causes spurious
  creates or deletes.
- **Mutating cached objects.** Corrupts the shared informer cache for all
  controllers in the process.
- **Unbounded retries.** Without `maxRetries`, a permanently failing key blocks
  the rate limiter's per-item backoff at 1000s intervals forever.
- **Business logic in event handlers.** Event handlers should extract a key and
  call `queue.Add`. Decision-making belongs in the sync handler where the full
  current state is available.
- **Edge-triggered sync.** Computing diffs from the old/new objects in the
  update handler instead of reconciling desired vs actual in the sync handler.
  Missed events or resyncs will produce incorrect state.
- **Creating all dependents in one batch.** Without slow-start, quota exhaustion
  causes N doomed API calls instead of 1.
- **Skipping `queue.Done(key)`.** The queue will never re-process the key and
  the `processing` set leaks.
- **Using a raw `go` statement for workers.** Workers must be waited on before
  `Run()` returns. Use `sync.WaitGroup` or `errgroup`.
