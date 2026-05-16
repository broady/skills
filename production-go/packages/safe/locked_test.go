package safe

import (
	"sync"
	"testing"
)

func TestLocked_Do_AtomicIncrement(t *testing.T) {
	var counter Locked[int]

	const goroutines = 10
	const increments = 1000

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			for range increments {
				counter.Do(func(v *int) { *v++ })
			}
		}()
	}
	wg.Wait()

	got := counter.Get()
	want := goroutines * increments
	if got != want {
		t.Errorf("counter = %d, want %d", got, want)
	}
}

func TestLocked_Store_WholesaleReplace(t *testing.T) {
	var l Locked[string]
	l.Store("hello")
	if got := l.Get(); got != "hello" {
		t.Errorf("Get() = %q, want %q", got, "hello")
	}
	l.Store("world")
	if got := l.Get(); got != "world" {
		t.Errorf("Get() = %q, want %q", got, "world")
	}
}

func TestLocked_Do_StructMutation(t *testing.T) {
	type state struct {
		Count int
		Max   int
	}

	var s Locked[state]
	s.Store(state{Max: 5})

	const goroutines = 10
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			s.Do(func(st *state) {
				if st.Count < st.Max {
					st.Count++
				}
			})
		}()
	}
	wg.Wait()

	got := s.Get()
	if got.Count != got.Max {
		t.Errorf("Count = %d, want %d (Max)", got.Count, got.Max)
	}
}

func TestLocked_ZeroValue(t *testing.T) {
	// Zero value should be usable without initialization.
	var l Locked[int]
	if got := l.Get(); got != 0 {
		t.Errorf("zero value Get() = %d, want 0", got)
	}
	l.Do(func(v *int) { *v = 42 })
	if got := l.Get(); got != 42 {
		t.Errorf("Get() = %d, want 42", got)
	}
}
