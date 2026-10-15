package agent

import (
	"cmp"
	"context"
	"slices"
	"strings"
	"sync"

	"github.com/Phong95/aec-agent-go/components/agent/agentloop"
	"github.com/Phong95/aec-agent-go/components/agent/profile"
	"github.com/Phong95/aec-agent-go/internal/nilcheck"
	"github.com/Phong95/aec-agent-go/kernel"
)

// ContextManager is the agent's context manager, as the agentloop.Context port. It builds
// one always-loaded system message from the profile at construction (the system
// prompt, then the initial context parts userMd, memoryMd and soulMd that are not
// empty, joined by a blank line) and puts it in front of the conversation messages
// on every PrepareMessages call, then runs the context middlewares in priority
// order through their optional agentloop.BeforeModelMiddleware and
// agentloop.AfterModelMiddleware hooks.
//
// The system message is built once, so a later profile change does not reach it. It
// is a fresh value on every call, which matters for the duplicated system message
// after a second compaction (a quirk the loop keeps).
//
// The agent's skill index (the <available_skills> block) is appended to the end of
// the system message on every call, so skills enabled or created mid-conversation
// show up on the next model call; the index source is set with WithSkillIndex (the
// Agent passes its SkillManager's BuildIndex when it has a skill hub), and without
// one there is no index.
//
// It is safe for concurrent use.
type ContextManager struct {
	system kernel.Message
	// profileSections (A-104) are the profile's parts of system, labelled: the system
	// prompt and the initial context files.
	profileSections []agentloop.ContextSection

	// skillIndex resolves the skill index on every PrepareMessages; the bool is false
	// (or the text empty) when the agent has none. Nil means no skills.
	skillIndex func(ctx context.Context) (string, bool)
	// subagentIndex (A-105) resolves the agents this one may delegate to
	// (<available_subagents>) on every PrepareMessages, for the turn's session - a child
	// run's session gets none. Nil means none.
	subagentIndex func(ctx context.Context, session kernel.Session) (string, bool)
	// secretIndex (A-101) resolves the agent's secrets (<available_secrets>: names, what
	// they are for, where they may be used - never values) on every PrepareMessages. Nil
	// means none.
	secretIndex func(ctx context.Context) (string, bool)

	mu          sync.RWMutex
	middlewares []agentloop.Middleware
}

var (
	_ agentloop.Context          = (*ContextManager)(nil)
	_ agentloop.SectionedContext = (*ContextManager)(nil)
)

// ContextOption configures a ContextManager.
type ContextOption func(*ContextManager)

// WithSkillIndex sets the source of the skill index. It is called on every
// PrepareMessages with the ctx of the model call. A nil function is ignored.
func WithSkillIndex(f func(ctx context.Context) (string, bool)) ContextOption {
	return func(c *ContextManager) {
		if f != nil {
			c.skillIndex = f
		}
	}
}

// WithSubagentIndex sets the source of the subagent index (A-105): the agents this one
// may delegate to, appended after the skill index. It is called on every PrepareMessages
// with the ctx of the model call and the turn's session; the bool is false (or the text
// empty) when there is none. A nil function is ignored.
func WithSubagentIndex(f func(ctx context.Context, session kernel.Session) (string, bool)) ContextOption {
	return func(c *ContextManager) {
		if f != nil {
			c.subagentIndex = f
		}
	}
}

// WithSecretIndex sets the source of the secret index (A-101): the agent's secrets,
// appended after the subagent index. It is called on every PrepareMessages with the ctx of
// the model call; the bool is false (or the text empty) when there is none. A nil
// function is ignored.
func WithSecretIndex(f func(ctx context.Context) (string, bool)) ContextOption {
	return func(c *ContextManager) {
		if f != nil {
			c.secretIndex = f
		}
	}
}

// NewContextManager returns a ContextManager for p.
func NewContextManager(p profile.AgentProfile, opts ...ContextOption) *ContextManager {
	parts := []string{p.SystemPrompt}
	if ic := p.InitialContext; ic != nil {
		for _, extra := range []string{ic.UserMd, ic.MemoryMd, ic.SoulMd} {
			if extra != "" {
				parts = append(parts, extra)
			}
		}
	}
	c := &ContextManager{
		system:          kernel.Message{Role: kernel.RoleSystem, Content: strings.Join(parts, "\n\n")},
		profileSections: profileSections(p),
	}
	for _, opt := range opts {
		if opt != nil {
			opt(c)
		}
	}
	return c
}

// AddMiddleware adds a context middleware and keeps the list in ascending priority
// order. Middlewares with the same priority keep the order they were added in (the
// sort is stable). The priorities are compared, not subtracted, so a huge
// priority cannot overflow; a nil middleware is ignored.
func (c *ContextManager) AddMiddleware(mw agentloop.Middleware) {
	if nilcheck.IsNil(mw) {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.middlewares = append(c.middlewares, mw)
	slices.SortStableFunc(c.middlewares, func(a, b agentloop.Middleware) int {
		return cmp.Compare(a.Priority(), b.Priority())
	})
}

// snapshot returns the middlewares in priority order. The hooks run outside the
// lock, so a hook may add a middleware without deadlocking.
func (c *ContextManager) snapshot() []agentloop.Middleware {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return slices.Clone(c.middlewares)
}

// PrepareMessages returns the system message (with the skill index) followed by
// msgs, rewritten by every BeforeModel hook in priority order.
func (c *ContextManager) PrepareMessages(ctx context.Context, msgs []kernel.Message, mc agentloop.MiddlewareContext) ([]kernel.Message, error) {
	prepared, _, err := c.Prepare(ctx, msgs, mc)
	return prepared, err
}

// Prepare (A-104) is PrepareMessages plus the labelled sections the system message was
// built from (agentloop.SectionedContext): the profile's sections, then the skill,
// subagent and secret indexes that are not empty. The messages are the same as
// PrepareMessages returns.
func (c *ContextManager) Prepare(ctx context.Context, msgs []kernel.Message, mc agentloop.MiddlewareContext) ([]kernel.Message, []agentloop.ContextSection, error) {
	prepared := make([]kernel.Message, 0, len(msgs)+1)
	sections := slices.Clone(c.profileSections)
	system, skills := c.withSkillIndex(ctx)
	if skills != "" {
		sections = append(sections, agentloop.ContextSection{Key: agentloop.ContextSkills, Text: skills})
	}
	if c.subagentIndex != nil {
		if index, ok := c.subagentIndex(ctx, mc.Session); ok && index != "" {
			system = withIndex(system, index)
			sections = append(sections, agentloop.ContextSection{Key: agentloop.ContextAgents, Text: index})
		}
	}
	if c.secretIndex != nil {
		if index, ok := c.secretIndex(ctx); ok && index != "" {
			system = withIndex(system, index)
			sections = append(sections, agentloop.ContextSection{Key: agentloop.ContextSecrets, Text: index})
		}
	}
	prepared = append(prepared, system)
	prepared = append(prepared, msgs...)
	for _, mw := range c.snapshot() {
		if hook, ok := mw.(agentloop.BeforeModelMiddleware); ok {
			var err error
			if prepared, err = hook.BeforeModel(ctx, prepared, mc); err != nil {
				return nil, nil, err
			}
		}
	}
	return prepared, sections, nil
}

// ProcessResponse runs every AfterModel hook on resp in priority order.
func (c *ContextManager) ProcessResponse(ctx context.Context, resp kernel.ModelResponse, mc agentloop.MiddlewareContext) (kernel.ModelResponse, error) {
	for _, mw := range c.snapshot() {
		if hook, ok := mw.(agentloop.AfterModelMiddleware); ok {
			var err error
			if resp, err = hook.AfterModel(ctx, resp, mc); err != nil {
				return kernel.ModelResponse{}, err
			}
		}
	}
	return resp, nil
}

// withSkillIndex returns a fresh copy of the system message with the skill index at
// its end: after a blank line when the message has content,
// as the whole content when it has none.
// Update (A-104): it also returns the index it added ("" for none), so Prepare can
// report it as a section.
func (c *ContextManager) withSkillIndex(ctx context.Context) (kernel.Message, string) {
	system := c.system.Clone()
	if c.skillIndex == nil {
		return system, ""
	}
	index, ok := c.skillIndex(ctx)
	if !ok || index == "" {
		return system, ""
	}
	return withIndex(system, index), index
}

// profileSections returns the profile's labelled parts of the always-loaded system
// message (A-104): the system prompt, and the initial context files that are not empty
// joined by a blank line.
func profileSections(p profile.AgentProfile) []agentloop.ContextSection {
	sections := []agentloop.ContextSection{{Key: agentloop.ContextSystemPrompt, Text: p.SystemPrompt}}
	if ic := p.InitialContext; ic != nil {
		var files []string
		for _, f := range []string{ic.UserMd, ic.MemoryMd, ic.SoulMd} {
			if f != "" {
				files = append(files, f)
			}
		}
		if len(files) > 0 {
			sections = append(sections, agentloop.ContextSection{Key: agentloop.ContextMemoryFiles, Text: strings.Join(files, "\n\n")})
		}
	}
	return sections
}

// withIndex returns system with an index (skills, A-105 subagents, A-101 secrets) at its end: after a
// blank line when the message has content, as the whole content when it has none. An
// empty index changes nothing.
func withIndex(system kernel.Message, index string) kernel.Message {
	if index == "" {
		return system
	}
	if system.Content != "" {
		system.Content += "\n\n" + index
	} else {
		system.Content = index
	}
	return system
}
