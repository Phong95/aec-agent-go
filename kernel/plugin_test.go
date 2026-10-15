package kernel

import (
	"context"
	"errors"
	"testing"
)

type ctxKey struct{}

// bare implements only Plugin (Name/Version), with no hooks.
type bare struct{}

func (bare) Name() string    { return "bare" }
func (bare) Version() string { return "1.0.0" }

// full implements Plugin and both optional hooks.
type full struct {
	bare
	initCalls, disposeCalls int
	initCtx, disposeCtx     context.Context
	initErr, disposeErr     error
}

func (p *full) Initialize(ctx context.Context) error {
	p.initCalls++
	p.initCtx = ctx
	return p.initErr
}

func (p *full) Dispose(ctx context.Context) error {
	p.disposeCalls++
	p.disposeCtx = ctx
	return p.disposeErr
}

// initOnly is not a Plugin at all; it only has the initialize hook.
type initOnly struct{ calls int }

func (p *initOnly) Initialize(context.Context) error {
	p.calls++
	return nil
}

// disposeOnly has only the dispose hook.
type disposeOnly struct{ calls int }

func (p *disposeOnly) Dispose(context.Context) error {
	p.calls++
	return nil
}

// Compile-time contract checks.
var (
	_ Plugin      = bare{}
	_ Plugin      = (*full)(nil)
	_ Initializer = (*full)(nil)
	_ Disposer    = (*full)(nil)
)

func TestPluginWithoutHooks(t *testing.T) {
	ctx := context.Background()
	if err := Initialize(ctx, bare{}); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := Dispose(ctx, bare{}); err != nil {
		t.Fatalf("Dispose: %v", err)
	}
}

func TestPluginWithBothHooks(t *testing.T) {
	ctx := context.WithValue(context.Background(), ctxKey{}, "caller")
	p := &full{}
	if err := Initialize(ctx, p); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := Dispose(ctx, p); err != nil {
		t.Fatalf("Dispose: %v", err)
	}
	if p.initCalls != 1 || p.disposeCalls != 1 {
		t.Fatalf("hooks called init=%d dispose=%d, want 1 each", p.initCalls, p.disposeCalls)
	}
	if p.initCtx != ctx || p.disposeCtx != ctx {
		t.Fatal("hooks must receive the caller's context")
	}
}

func TestPluginHookError(t *testing.T) {
	sentinel := errors.New("boom")
	p := &full{initErr: sentinel, disposeErr: sentinel}
	if err := Initialize(context.Background(), p); !errors.Is(err, sentinel) {
		t.Fatalf("Initialize error = %v, want %v", err, sentinel)
	}
	if err := Dispose(context.Background(), p); !errors.Is(err, sentinel) {
		t.Fatalf("Dispose error = %v, want %v", err, sentinel)
	}
	if p.initCalls != 1 || p.disposeCalls != 1 {
		t.Fatalf("hooks called init=%d dispose=%d, want 1 each", p.initCalls, p.disposeCalls)
	}
}

// A value with only one hook still works: the helpers take any, so a value that
// has just that method is called.
func TestHookOnlyValue(t *testing.T) {
	ctx := context.Background()
	i := &initOnly{}
	if err := Initialize(ctx, i); err != nil || i.calls != 1 {
		t.Fatalf("initOnly: calls=%d err=%v", i.calls, err)
	}
	if err := Dispose(ctx, i); err != nil || i.calls != 1 {
		t.Fatalf("Dispose of an initOnly value must be a no-op: calls=%d err=%v", i.calls, err)
	}
	d := &disposeOnly{}
	if err := Dispose(ctx, d); err != nil || d.calls != 1 {
		t.Fatalf("disposeOnly: calls=%d err=%v", d.calls, err)
	}
	if err := Initialize(ctx, d); err != nil || d.calls != 1 {
		t.Fatalf("Initialize of a disposeOnly value must be a no-op: calls=%d err=%v", d.calls, err)
	}
}

func TestNilPlugin(t *testing.T) {
	ctx := context.Background()
	if err := Initialize(ctx, nil); err != nil {
		t.Fatalf("Initialize(nil): %v", err)
	}
	if err := Dispose(ctx, nil); err != nil {
		t.Fatalf("Dispose(nil): %v", err)
	}
	// A typed nil pointer implementing the hooks must not be called (it would panic on field access).
	var p *full
	if err := Initialize(ctx, p); err != nil {
		t.Fatalf("Initialize(typed nil): %v", err)
	}
	if err := Dispose(ctx, p); err != nil {
		t.Fatalf("Dispose(typed nil): %v", err)
	}
}
