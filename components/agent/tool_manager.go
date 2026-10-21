package agent

import (
	"context"
	"slices"
	"strings"
	"sync"

	"github.com/Phong95/aec-agent-go/components/agent/agentloop"
	"github.com/Phong95/aec-agent-go/components/agent/profile"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/kernel"
)

// ToolManager is the agent's tool access over the tool hub, as the
// agentloop.Tools port. It lists and authorizes only the tools in the agent's
// allowlist (profile.Tools), and it resolves them from the hub lazily on every
// access, so that
//   - a tool registered after the agent is spawned is visible (startup ordering),
//   - an allowlist change through SetAllowedTools takes effect immediately.
//
// An empty allowlist therefore means no tools at all: a manager built from an empty
// profile.Tools lists nothing and finds nothing. The toolhub.ToolHub treats a nil or empty
// allowlist as allow-all, but that is only ever reached for a name that is in a
// non-empty list.
//
// The allowlist sits behind a read-write mutex and is copied on the way in and on
// every read, so SetAllowedTools may run while a chat is in flight.
type ToolManager struct {
	hub *toolhub.ToolHub
	// sessionGrants (A-106) is the tools a session may use beyond the allowlist, read on
	// every call; nil means none.
	sessionGrants func(sessionID string) []string

	mu      sync.RWMutex
	allowed []string
}

var (
	_ agentloop.Tools        = (*ToolManager)(nil)
	_ agentloop.SessionTools = (*ToolManager)(nil)
)

// ToolManagerOption configures a ToolManager (A-106).
type ToolManagerOption func(*ToolManager)

// WithSessionGrants sets the tools a session may use beyond the allowlist (A-106), read
// on every call: a group lead's session is granted spawn_subagent to hand work to the
// group's members.
func WithSessionGrants(grants func(sessionID string) []string) ToolManagerOption {
	return func(m *ToolManager) { m.sessionGrants = grants }
}

// NewToolManager returns a ToolManager over hub whose allowlist is p.Tools (copied).
func NewToolManager(hub *toolhub.ToolHub, p profile.AgentProfile, opts ...ToolManagerOption) *ToolManager {
	m := &ToolManager{hub: hub, allowed: slices.Clone(p.Tools)}
	for _, o := range opts {
		if o != nil {
			o(m)
		}
	}
	return m
}

// GrantedTools (A-106) returns the tools sessionID is granted beyond the allowlist (none
// without a session).
func (m *ToolManager) GrantedTools(sessionID string) []string {
	if sessionID == "" || m.sessionGrants == nil {
		return nil
	}
	return slices.Clone(m.sessionGrants(sessionID))
}

// grantsBeyond is the session's granted tools that allowed lacks.
func (m *ToolManager) grantsBeyond(allowed []string, sessionID string) []string {
	var out []string
	for _, name := range m.GrantedTools(sessionID) {
		if !slices.Contains(allowed, name) && !slices.Contains(out, name) {
			out = append(out, name)
		}
	}
	return out
}

// ListFor (A-106) is List plus the tools sessionID is granted beyond the allowlist
// (agentloop.SessionTools).
func (m *ToolManager) ListFor(sessionID string) []toolhub.Definition {
	allowed := m.allowlist()
	if granted := m.grantsBeyond(allowed, sessionID); len(granted) > 0 {
		allowed = append(allowed, granted...)
	}
	return m.hub.GetByNames(allowed)
}

// allowlist returns a copy of the current allowlist.
func (m *ToolManager) allowlist() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return slices.Clone(m.allowed)
}

// List returns the definitions of the registered tools among the allowlist, in the
// allowlist's order. Names that match no registered tool are ignored.
func (m *ToolManager) List() []toolhub.Definition {
	return m.hub.GetByNames(m.allowlist())
}

// Has reports whether the agent may use the tool: the name is in the allowlist AND a
// tool of that name is registered. The skill manager uses it to hide a skill whose
// required tools the agent lacks.
func (m *ToolManager) Has(name string) bool {
	if !slices.Contains(m.allowlist(), name) {
		return false
	}
	_, ok := m.hub.Get(name)
	return ok
}

// ScheduleProblem says why name can't be a tool job's tool for this agent (A-103): it is
// not one of the agent's tools, or not marked schedulable. "" means it can.
func (m *ToolManager) ScheduleProblem(name string) string {
	var def *toolhub.Definition
	var schedulable []string
	for _, d := range m.List() {
		if d.Name == name {
			def = &d
		}
		if d.Schedulable {
			schedulable = append(schedulable, d.Name)
		}
	}
	if def == nil {
		return `"` + name + `" is not one of this agent's tools`
	}
	if !def.Schedulable {
		msg := `"` + name + `" can't run in a tool job`
		if len(schedulable) > 0 {
			msg += " " + emDash + " schedulable tools of this agent: " + strings.Join(schedulable, ", ")
		}
		return msg
	}
	return ""
}

// Authorize looks the tool up in the allowlist and authorizes the call through the
// hub's policy with that allowlist. A name outside the allowlist returns, before the
// hub is asked, an error matching toolhub.ErrNotFound with the text `Tool "x" not
// available for this agent`. A denial is the returned Decision with Allowed false and
// a nil error, exactly like toolhub.ToolHub.Authorize.
// A-106: the session's granted tools (tc.SessionID) are available and allowed too; an
// empty allowlist stays "no restriction" for the policy.
func (m *ToolManager) Authorize(ctx context.Context, name string, args kernel.Arguments, tc toolhub.Context) (toolhub.Authorized, toolhub.Decision, error) {
	allowed := m.allowlist()
	granted := m.grantsBeyond(allowed, tc.SessionID)
	if !slices.Contains(allowed, name) && !slices.Contains(granted, name) {
		return toolhub.Authorized{}, toolhub.Decision{}, &notAvailableError{name: name}
	}
	if len(allowed) > 0 && len(granted) > 0 {
		allowed = append(allowed, granted...)
	}
	return m.hub.Authorize(ctx, name, args, tc, allowed)
}

// SetAllowedTools replaces the allowlist (for example after a PATCH update); names
// is copied.
func (m *ToolManager) SetAllowedTools(names []string) {
	names = slices.Clone(names)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.allowed = names
}
