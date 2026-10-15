package toolkit

import (
	"context"
	"errors"

	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/components/toolhub"
)

// ErrNoSandbox says the tool context offers no sandbox: the host gave no accessor, or
// the accessor returned no view. It is the "no sandbox accessor" case; a tool package maps
// it to its own text for the model.
var ErrNoSandbox = errors.New("toolkit: the agent has no sandbox")

// SandboxFor returns the sandbox view of the agent a tool call runs for. It returns
// ErrNoSandbox when tc.GetSandbox is nil or returns a nil view. An error of the
// accessor itself (a *kernel.SandboxError such as disabled or not_configured, or any
// other failure) is returned unchanged, so the caller can tell it from a missing
// sandbox with errors.As and map the code to its hint.
//
// Every tool that works on files, commands or artifacts resolves its sandbox here and
// then only ever uses the returned sandboxhub.View, never the host (T-05-08).
func SandboxFor(ctx context.Context, tc toolhub.Context) (sandboxhub.View, error) {
	if tc.GetSandbox == nil {
		return nil, ErrNoSandbox
	}
	view, err := tc.GetSandbox(ctx)
	if err != nil {
		return nil, err
	}
	if view == nil {
		return nil, ErrNoSandbox
	}
	return view, nil
}
