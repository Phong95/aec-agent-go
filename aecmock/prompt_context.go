package aecmock

import (
	"context"
	"slices"
	"strings"

	"github.com/Phong95/aec-agent-go/components/agent/agentloop"
	"github.com/Phong95/aec-agent-go/components/agent/profile"
	"github.com/Phong95/aec-agent-go/kernel"
)

// PromptContext is a context manager as the agentloop.Context port for tests.
// It builds one system message from the profile (the system prompt followed by
// the initial context parts, joined by a blank line) and puts it in front of the
// conversation messages on every PrepareMessages call, then runs the middlewares
// in priority order through their optional agentloop.BeforeModelMiddleware and
// agentloop.AfterModelMiddleware hooks.
//
// It has no skill index option. The system message is a fresh value on every
// call, which matters for the duplicated system message after a second
// compaction (a behaviour the loop keeps).
type PromptContext struct {
	system      kernel.Message
	middlewares []agentloop.Middleware
}

var _ agentloop.Context = (*PromptContext)(nil)

// NewPromptContext returns a PromptContext for p with the given middlewares.
// Middlewares with the same priority keep the order they were given in (the sort
// is stable).
func NewPromptContext(p profile.AgentProfile, mws ...agentloop.Middleware) *PromptContext {
	parts := []string{p.SystemPrompt}
	if ic := p.InitialContext; ic != nil {
		for _, extra := range []string{ic.UserMd, ic.MemoryMd, ic.SoulMd} {
			if extra != "" {
				parts = append(parts, extra)
			}
		}
	}
	sorted := slices.Clone(mws)
	slices.SortStableFunc(sorted, func(a, b agentloop.Middleware) int { return a.Priority() - b.Priority() })
	return &PromptContext{
		system:      kernel.Message{Role: kernel.RoleSystem, Content: strings.Join(parts, "\n\n")},
		middlewares: sorted,
	}
}

// PrepareMessages returns the system message followed by msgs, rewritten by every
// BeforeModel hook in priority order.
func (c *PromptContext) PrepareMessages(ctx context.Context, msgs []kernel.Message, mc agentloop.MiddlewareContext) ([]kernel.Message, error) {
	prepared := make([]kernel.Message, 0, len(msgs)+1)
	prepared = append(prepared, c.system.Clone())
	prepared = append(prepared, msgs...)
	for _, mw := range c.middlewares {
		if hook, ok := mw.(agentloop.BeforeModelMiddleware); ok {
			var err error
			if prepared, err = hook.BeforeModel(ctx, prepared, mc); err != nil {
				return nil, err
			}
		}
	}
	return prepared, nil
}

// ProcessResponse runs every AfterModel hook on resp in priority order.
func (c *PromptContext) ProcessResponse(ctx context.Context, resp kernel.ModelResponse, mc agentloop.MiddlewareContext) (kernel.ModelResponse, error) {
	for _, mw := range c.middlewares {
		if hook, ok := mw.(agentloop.AfterModelMiddleware); ok {
			var err error
			if resp, err = hook.AfterModel(ctx, resp, mc); err != nil {
				return kernel.ModelResponse{}, err
			}
		}
	}
	return resp, nil
}

// PassthroughContext is the conformance stub context manager: PrepareMessages
// returns the messages unchanged (no system message is added) and ProcessResponse
// returns the response unchanged.
type PassthroughContext struct{}

var _ agentloop.Context = PassthroughContext{}

// PrepareMessages returns msgs unchanged.
func (PassthroughContext) PrepareMessages(_ context.Context, msgs []kernel.Message, _ agentloop.MiddlewareContext) ([]kernel.Message, error) {
	return msgs, nil
}

// ProcessResponse returns resp unchanged.
func (PassthroughContext) ProcessResponse(_ context.Context, resp kernel.ModelResponse, _ agentloop.MiddlewareContext) (kernel.ModelResponse, error) {
	return resp, nil
}
