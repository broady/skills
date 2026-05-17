package safe

import (
	"context"
	"sync"
)

// BoundedGroup runs functions concurrently with a maximum of limit goroutines
// in flight. The first non-nil error cancels the group's context; Wait returns
// that error.
//
// BoundedGroup satisfies the Group interface, so safe.Go works with it:
//
//	g, ctx := safe.NewBoundedGroup(ctx, 10)
//	safe.Go(g, "process", func() error { return process(ctx, item) })
//	return g.Wait()
//
// Unlike errgroup, boundedness is declared at construction — there is no
// SetLimit. This makes the concurrency bound visible and grep-able at the
// declaration site.
type BoundedGroup struct {
	ctx    context.Context
	cancel context.CancelCauseFunc
	wg     sync.WaitGroup
	sem    chan struct{}

	mu  sync.Mutex
	err error
}

// NewBoundedGroup returns a BoundedGroup that allows at most limit concurrent
// goroutines and a derived context that is cancelled when Wait returns or on
// the first error.
//
// If limit <= 0, it panics — unbounded concurrency must use errgroup directly
// and justify the choice.
func NewBoundedGroup(ctx context.Context, limit int) (*BoundedGroup, context.Context) {
	if limit <= 0 {
		panic("safe.NewBoundedGroup: limit must be > 0")
	}
	ctx, cancel := context.WithCancelCause(ctx)
	return &BoundedGroup{
		ctx:    ctx,
		cancel: cancel,
		sem:    make(chan struct{}, limit),
	}, ctx
}

// Go starts fn in a new goroutine, blocking until a semaphore slot is
// available or the group's context is cancelled.
//
// If the context is already cancelled when Go is called, fn is not started.
func (g *BoundedGroup) Go(fn func() error) {
	// Fast path: context already cancelled.
	if g.ctx.Err() != nil {
		return
	}

	// Acquire semaphore slot, respecting context cancellation.
	select {
	case g.sem <- struct{}{}:
	case <-g.ctx.Done():
		return
	}

	g.wg.Add(1)
	go func() {
		defer g.wg.Done()
		defer func() { <-g.sem }()

		if err := fn(); err != nil {
			g.mu.Lock()
			if g.err == nil {
				g.err = err
				g.cancel(err)
			}
			g.mu.Unlock()
		}
	}()
}

// Wait blocks until all goroutines started by Go have returned, then cancels
// the group's context and returns the first non-nil error (if any).
func (g *BoundedGroup) Wait() error {
	g.wg.Wait()
	g.cancel(nil)
	return g.err
}
