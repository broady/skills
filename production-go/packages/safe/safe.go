// Package safe contains small concurrency helpers for production Go services.
//
// The package has no third-party dependencies. Its Group interface is satisfied
// by *errgroup.Group from golang.org/x/sync/errgroup.
package safe

import (
	"fmt"
	"runtime/debug"
)

// Group is the subset of errgroup.Group used by Go.
type Group interface {
	Go(func() error)
}

// PanicError reports a panic recovered by Go.
//
// A PanicError is returned through the owning group. It is not swallowed; the
// owner observes it from Wait and should fail or shut down the supervised work.
type PanicError struct {
	Name  string
	Value any
	Stack []byte
}

func (e *PanicError) Error() string {
	if e.Name == "" {
		return fmt.Sprintf("panic: %v", e.Value)
	}
	return fmt.Sprintf("%s panic: %v", e.Name, e.Value)
}

// Go starts fn in g and reports returned errors or panics through g.Wait.
//
// Use this as the only approved recover boundary for owned goroutines. It is
// intended to make panics visible to the owner, cancel sibling work when used
// with errgroup.WithContext, and prevent silent background goroutine failure.
func Go(g Group, name string, fn func() error) {
	g.Go(func() (err error) {
		defer func() {
			if v := recover(); v != nil {
				err = &PanicError{
					Name:  name,
					Value: v,
					Stack: debug.Stack(),
				}
			}
		}()
		if err := fn(); err != nil {
			if name == "" {
				return err
			}
			return fmt.Errorf("%s: %w", name, err)
		}
		return nil
	})
}
