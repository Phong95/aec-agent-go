package aecmock

import (
	"context"
	"slices"

	"github.com/Phong95/aec-agent-go/components/agent/agentloop"
	"github.com/Phong95/aec-agent-go/components/toolhub"
)

// SkillTools is the agent's skill access as the agentloop.Skills port the loop uses
// in tests (a skill manager reduced to the two calls the loop makes): it holds the
// skill tools (skill_view and the like), finds one by its definition name and lists
// their definitions in registration order.
//
// Unlike the tools, a skill tool is handed to the loop directly, and the loop runs
// it without policy or approval, as skill_view only reads. A test
// that needs the policy path registers its handler with a toolhub.ToolHub instead.
type SkillTools struct {
	handlers []toolhub.Handler
}

var _ agentloop.Skills = (*SkillTools)(nil)

// NewSkillTools returns SkillTools over the given handlers, in the given order. A
// later handler with the same definition name is shadowed by the earlier one.
func NewSkillTools(handlers ...toolhub.Handler) *SkillTools {
	return &SkillTools{handlers: slices.Clone(handlers)}
}

// Tool returns the skill tool registered under name.
func (s *SkillTools) Tool(name string) (toolhub.Handler, bool) {
	for _, h := range s.handlers {
		if h.Definition().Name == name {
			return h, true
		}
	}
	return nil, false
}

// ToolDefinitions returns the definitions of the skill tools, in registration
// order, as copies.
func (s *SkillTools) ToolDefinitions(context.Context) ([]toolhub.Definition, error) {
	out := make([]toolhub.Definition, len(s.handlers))
	for i, h := range s.handlers {
		out[i] = h.Definition().Clone()
	}
	return out, nil
}
