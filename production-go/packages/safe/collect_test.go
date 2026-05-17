package safe

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestCollectReturnsResultsInOrder(t *testing.T) {
	items := []int{10, 20, 30, 40, 50}

	results := Collect(context.Background(), 2, items, func(_ context.Context, n int) (int, error) {
		return n * 2, nil
	})

	if len(results) != len(items) {
		t.Fatalf("got %d results, want %d", len(results), len(items))
	}
	for i, r := range results {
		if r.Err != nil {
			t.Errorf("results[%d]: unexpected error: %v", i, r.Err)
		}
		want := items[i] * 2
		if r.Val != want {
			t.Errorf("results[%d]: got %d, want %d", i, r.Val, want)
		}
	}
}

func TestCollectBoundsConcurrency(t *testing.T) {
	const limit = 3
	const total = 20

	var running atomic.Int32
	var maxSeen atomic.Int32

	items := make([]int, total)
	for i := range items {
		items[i] = i
	}

	Collect(context.Background(), limit, items, func(_ context.Context, _ int) (struct{}, error) {
		cur := running.Add(1)
		for {
			old := maxSeen.Load()
			if cur <= old || maxSeen.CompareAndSwap(old, cur) {
				break
			}
		}
		time.Sleep(time.Millisecond)
		running.Add(-1)
		return struct{}{}, nil
	})

	if peak := maxSeen.Load(); peak > limit {
		t.Fatalf("peak concurrency %d exceeded limit %d", peak, limit)
	}
}

func TestCollectRecoversPanic(t *testing.T) {
	items := []string{"ok", "panic", "ok"}

	results := Collect(context.Background(), 3, items, func(_ context.Context, s string) (string, error) {
		if s == "panic" {
			panic("boom")
		}
		return s + "!", nil
	})

	// Item 0: success.
	if results[0].Err != nil || results[0].Val != "ok!" {
		t.Errorf("results[0] = %+v, want Val=ok! Err=nil", results[0])
	}

	// Item 1: panic recovered.
	var pe *PanicError
	if !errors.As(results[1].Err, &pe) {
		t.Fatalf("results[1].Err = %T %v, want *PanicError", results[1].Err, results[1].Err)
	}
	if pe.Value != "boom" {
		t.Errorf("PanicError.Value = %v, want boom", pe.Value)
	}
	if len(pe.Stack) == 0 {
		t.Error("PanicError.Stack is empty")
	}

	// Item 2: success (not affected by sibling panic).
	if results[2].Err != nil || results[2].Val != "ok!" {
		t.Errorf("results[2] = %+v, want Val=ok! Err=nil", results[2])
	}
}

func TestCollectReportsIndividualErrors(t *testing.T) {
	errBad := errors.New("bad item")
	items := []int{1, 2, 3}

	results := Collect(context.Background(), 3, items, func(_ context.Context, n int) (int, error) {
		if n == 2 {
			return 0, errBad
		}
		return n * 10, nil
	})

	if results[0].Val != 10 || results[0].Err != nil {
		t.Errorf("results[0] = %+v", results[0])
	}
	if !errors.Is(results[1].Err, errBad) {
		t.Errorf("results[1].Err = %v, want %v", results[1].Err, errBad)
	}
	if results[2].Val != 30 || results[2].Err != nil {
		t.Errorf("results[2] = %+v", results[2])
	}
}

func TestCollectRespectsContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	items := make([]int, 100)
	var started atomic.Int32

	results := Collect(ctx, 1, items, func(ctx context.Context, _ int) (int, error) {
		n := started.Add(1)
		if n == 3 {
			cancel() // cancel after 3 items have started
		}
		// Simulate work.
		time.Sleep(time.Millisecond)
		return 1, nil
	})

	if len(results) != 100 {
		t.Fatalf("got %d results, want 100", len(results))
	}

	// Some results should have ctx.Err() because they were never started.
	var cancelled int
	for _, r := range results {
		if r.Err == context.Canceled {
			cancelled++
		}
	}
	if cancelled == 0 {
		t.Fatal("expected some results to be cancelled")
	}
	// With limit=1 and cancel after 3 starts, most items should be skipped.
	if cancelled < 90 {
		t.Errorf("only %d cancelled, expected most of 97 remaining", cancelled)
	}
}

func TestCollectNilSlice(t *testing.T) {
	results := Collect[int, int](context.Background(), 5, nil, func(_ context.Context, n int) (int, error) {
		return n, nil
	})
	if results != nil {
		t.Fatalf("got %v, want nil", results)
	}
}

func TestCollectEmptySlice(t *testing.T) {
	results := Collect(context.Background(), 5, []int{}, func(_ context.Context, n int) (int, error) {
		return n, nil
	})
	if results != nil {
		t.Fatalf("got %v, want nil", results)
	}
}

func TestCollectPanicsForNonPositiveLimit(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic for non-positive limit")
		}
	}()

	Collect(context.Background(), 0, []int{1}, func(_ context.Context, n int) (int, error) {
		return n, nil
	})
}
