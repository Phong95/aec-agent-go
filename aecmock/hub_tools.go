package aecmock

import (
	"context"
	"slices"

	"github.com/Phong95/aec-agent-go/components/agent/agentloop"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/kernel"
)

// HubTools is the agent's tool access over a toolhub.ToolHub, as the agentloop.Tools
// port the loop uses in tests. It exposes only the tools in the agent's tool
// list: List is hub.GetByNames(allowed), and Authorize refuses a name outside the
// list with a "not available for this agent" error before the hub is asked.
//
// An empty list therefore means no tools at all: HubTools built from an empty
// profile tool list lists nothing and finds nothing. The
// toolhub.ToolHub treats a nil or empty allowlist as allow-all, but that is only ever
// reached for a name that is in a non-empty list.
type HubTools struct {
	hub     *toolhub.ToolHub
	allowed []string
}

var _ agentloop.Tools = (*HubTools)(nil)

// NewHubTools returns HubTools over hub with the agent's tool list allowed (the
// list is copied).
func NewHubTools(hub *toolhub.ToolHub, allowed []string) *HubTools {
	return &HubTools{hub: hub, allowed: slices.Clone(allowed)}
}

// List returns the definitions of the registered tools among the agent's tool
// list, in the list's order.
func (h *HubTools) List() []toolhub.Definition {
	return h.hub.GetByNames(h.allowed)
}

// Authorize looks the tool up in the agent's list and authorizes the call through
// the hub's policy. A name outside the list returns an error matching
// toolhub.ErrNotFound with the text `Tool "x" not available for this agent`.
func (h *HubTools) Authorize(ctx context.Context, name string, args kernel.Arguments, tc toolhub.Context) (toolhub.Authorized, toolhub.Decision, error) {
	if !slices.Contains(h.allowed, name) {
		return toolhub.Authorized{}, toolhub.Decision{}, &notAvailableError{name: name}
	}
	return h.hub.Authorize(ctx, name, args, tc, h.allowed)
}

// notAvailableError carries the text of an unavailable tool and unwraps to
// toolhub.ErrNotFound, so errors.Is(err, toolhub.ErrNotFound) holds as it does for the
// hub's own error.
type notAvailableError struct{ name string }

func (e *notAvailableError) Error() string {
	return `Tool "` + e.name + `" not available for this agent`
}

func (*notAvailableError) Unwrap() error { return toolhub.ErrNotFound }
