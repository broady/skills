package safe

import "sync"

// Locked[T] protects a value with an RWMutex. It prevents the logical race
// caused by separate Get/Set calls where the race detector is silent (each
// call is individually synchronized, but the compound read-modify-write is not
// atomic).
//
// Use Do for compound mutations where the new value depends on the current
// value. Use Get for read-only snapshots. Use Store for wholesale replacement
// that doesn't depend on the current value.
type Locked[T any] struct {
	mu sync.RWMutex
	v  T
}

// Do holds the write lock for the entire function, enabling atomic
// read-modify-write operations. Use this whenever the new value depends on
// the current value.
//
//	counter.Do(func(v *int) { *v++ })
//
//	state.Do(func(s *AppState) {
//	    if s.Count < s.Max {
//	        s.Count++
//	    }
//	})
func (l *Locked[T]) Do(f func(*T)) {
	l.mu.Lock()
	defer l.mu.Unlock()
	f(&l.v)
}

// Get returns a snapshot of the value under a read lock. Safe for reads that
// don't feed back into writes. If the value contains pointers, maps, or
// slices, the caller receives shared references — copy them if mutation is
// intended.
func (l *Locked[T]) Get() T {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.v
}

// Store replaces the value wholesale under a write lock. Safe when the new
// value doesn't depend on the current value (config swap, flag toggle from a
// single source). If the new value depends on the old value, use Do instead.
func (l *Locked[T]) Store(v T) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.v = v
}
