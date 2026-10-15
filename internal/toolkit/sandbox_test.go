package toolkit_test

import (
	"context"
	"errors"
	"testing"

	"github.com/Phong95/aec-agent-go/aecmock"
	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/internal/toolkit"
	"github.com/Phong95/aec-agent-go/kernel"
)

// TestSandboxFor covers the four outcomes: no accessor, an accessor with no view, an
// accessor error that must come back unchanged, and a view.
func TestSandboxFor(t *testing.T) {
	view, err := aecmock.NewInMemoryView("docker:shared", "a1")
	if err != nil {
		t.Fatal(err)
	}
	disabled := kernel.NewSandboxError("no sandbox", kernel.SandboxCodeDisabled, "")

	t.Run("no accessor", func(t *testing.T) {
		if _, err := toolkit.SandboxFor(t.Context(), toolhub.Context{}); !errors.Is(err, toolkit.ErrNoSandbox) {
			t.Fatalf("err = %v, want ErrNoSandbox", err)
		}
	})
	t.Run("accessor returns no view", func(t *testing.T) {
		tc := toolhub.Context{GetSandbox: func(context.Context) (sandboxhub.View, error) { return nil, nil }}
		if _, err := toolkit.SandboxFor(t.Context(), tc); !errors.Is(err, toolkit.ErrNoSandbox) {
			t.Fatalf("err = %v, want ErrNoSandbox", err)
		}
	})
	t.Run("accessor error is returned unchanged", func(t *testing.T) {
		tc := toolhub.Context{GetSandbox: func(context.Context) (sandboxhub.View, error) { return nil, disabled }}
		_, err := toolkit.SandboxFor(t.Context(), tc)
		if !errors.Is(err, error(disabled)) {
			t.Fatalf("err = %v, want the accessor's own error", err)
		}
		var se *kernel.SandboxError
		if !errors.As(err, &se) || se.Code != kernel.SandboxCodeDisabled {
			t.Fatalf("errors.As lost the sandbox code: %v", err)
		}
		if errors.Is(err, toolkit.ErrNoSandbox) {
			t.Fatal("an accessor error must not read as ErrNoSandbox")
		}
	})
	t.Run("a view", func(t *testing.T) {
		tc := toolhub.Context{GetSandbox: func(context.Context) (sandboxhub.View, error) { return view, nil }}
		got, err := toolkit.SandboxFor(t.Context(), tc)
		if err != nil || got != view {
			t.Fatalf("SandboxFor = %v, %v; want the view", got, err)
		}
	})
}
