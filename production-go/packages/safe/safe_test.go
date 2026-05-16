package safe

import (
	"errors"
	"testing"
)

type testGroup struct {
	err error
}

func (g *testGroup) Go(fn func() error) {
	g.err = fn()
}

func TestGoReturnsFunctionError(t *testing.T) {
	want := errors.New("database unavailable")
	var g testGroup

	Go(&g, "sync users", func() error {
		return want
	})

	if !errors.Is(g.err, want) {
		t.Fatalf("got %v, want wrapped %v", g.err, want)
	}
}

func TestGoReportsPanic(t *testing.T) {
	var g testGroup

	Go(&g, "sync users", func() error {
		panic("bad invariant")
	})

	var panicErr *PanicError
	if !errors.As(g.err, &panicErr) {
		t.Fatalf("got %T %v, want PanicError", g.err, g.err)
	}
	if panicErr.Name != "sync users" {
		t.Fatalf("got name %q, want %q", panicErr.Name, "sync users")
	}
	if panicErr.Value != "bad invariant" {
		t.Fatalf("got value %v, want %v", panicErr.Value, "bad invariant")
	}
	if len(panicErr.Stack) == 0 {
		t.Fatal("stack is empty")
	}
}

func TestGoAllowsUnnamedFunctions(t *testing.T) {
	want := errors.New("stop")
	var g testGroup

	Go(&g, "", func() error {
		return want
	})

	if !errors.Is(g.err, want) {
		t.Fatalf("got %v, want %v", g.err, want)
	}
}
