package safe

import (
	"context"
	"sync"
)

// Result holds the outcome of a single concurrent operation within Collect.
type Result[R any] struct {
	Val R
	Err error // nil on success.
}

// Collect runs fn for each item with at most limit concurrent goroutines.
// Results are returned in input order.
//
// Panics in fn crash the process. This is intentional: panics indicate
// programmer errors where state may be corrupted. Do not use Collect to
// run untrusted or panic-prone code. If fn may panic on malformed input
// from an external source, validate inputs before calling Collect.
//
// If limit <= 0, Collect panics. Callers must provide an explicit bound.
// Validate config-derived limits before calling Collect; this panic is for
// programmer misuse, not ordinary production runtime failure.
//
// When ctx is cancelled, in-flight goroutines run to completion but new items
// are not started; their Result.Err is set to ctx.Err().
//
// Use Collect for best-effort fan-out/collect where individual failures are
// tolerable (prefetch, cache warming, batch lookups with partial failure
// tolerance). For all-or-nothing concurrency (first error aborts remaining
// work), use errgroup.WithContext instead.
func Collect[T, R any](ctx context.Context, limit int, items []T, fn func(context.Context, T) (R, error)) []Result[R] {
	if len(items) == 0 {
		return nil
	}
	if limit <= 0 {
		panic("safe.Collect: limit must be > 0")
	}

	results := make([]Result[R], len(items))
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup

	for i, item := range items {
		// Fast path: context already cancelled.
		if ctx.Err() != nil {
			results[i] = Result[R]{Err: ctx.Err()}
			continue
		}

		// Acquire semaphore slot, respecting context cancellation.
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			results[i] = Result[R]{Err: ctx.Err()}
			continue
		}

		wg.Add(1)
		go func(idx int, it T) {
			defer wg.Done()
			defer func() { <-sem }()
			val, err := fn(ctx, it)
			results[idx] = Result[R]{Val: val, Err: err}
		}(i, item)
	}

	wg.Wait()
	return results
}
