package safe

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestBoundedGroupLimitsConcurrency(t *testing.T) {
	const limit = 3
	g, ctx := NewBoundedGroup(context.Background(), limit)

	var peak atomic.Int64
	var current atomic.Int64

	for range 20 {
		g.Go(func() error {
			n := current.Add(1)
			// Track peak concurrency.
			for {
				old := peak.Load()
				if n <= old || peak.CompareAndSwap(old, n) {
					break
				}
			}
			// Simulate work.
			select {
			case <-time.After(10 * time.Millisecond):
			case <-ctx.Done():
			}
			current.Add(-1)
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p := peak.Load(); p > limit {
		t.Fatalf("peak concurrency %d exceeded limit %d", p, limit)
	}
}

func TestBoundedGroupFirstErrorCancels(t *testing.T) {
	g, ctx := NewBoundedGroup(context.Background(), 5)
	want := errors.New("first failure")

	g.Go(func() error { return want })

	// Wait a moment so the error propagates.
	g.Go(func() error {
		<-ctx.Done()
		return nil
	})

	err := g.Wait()
	if !errors.Is(err, want) {
		t.Fatalf("got %v, want %v", err, want)
	}
}

func TestBoundedGroupContextCancelledSkipsFn(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled

	g, _ := NewBoundedGroup(ctx, 2)

	var called atomic.Bool
	g.Go(func() error {
		called.Store(true)
		return nil
	})

	if err := g.Wait(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if called.Load() {
		t.Fatal("fn should not have been called with cancelled context")
	}
}

func TestBoundedGroupSatisfiesGroupInterface(t *testing.T) {
	g, _ := NewBoundedGroup(context.Background(), 2)

	// Compile-time check: BoundedGroup satisfies Group.
	var _ Group = g

	want := errors.New("oops")
	Go(g, "test-task", func() error { return want })

	err := g.Wait()
	if !errors.Is(err, want) {
		t.Fatalf("got %v, want wrapped %v", err, want)
	}
}

func TestBoundedGroupPanicsOnZeroLimit(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for limit <= 0")
		}
	}()
	NewBoundedGroup(context.Background(), 0)
}
