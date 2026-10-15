package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"

	"github.com/Phong95/aec-agent-go/components/agent/agentloop"
	"github.com/Phong95/aec-agent-go/components/agent/profile"
	"github.com/Phong95/aec-agent-go/components/agent/subagent"
	"github.com/Phong95/aec-agent-go/components/artifacthub/artifact"
	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/components/secrethub/secret"
	"github.com/Phong95/aec-agent-go/components/subagenthub/subagenttype"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/components/trace"
)

// childLoopDeps is what a subagent type's short-lived loop borrows from its parent agent
// (A-105 revision, the ChildLoopDeps of core/ts). The parent's model, sandbox and
// artifacts are shared; tools come from the global ToolHub, filtered.
type childLoopDeps struct {
	models    agentloop.Models
	toolHub   *toolhub.ToolHub
	bus       events.Bus
	trace     *trace.Service
	sandboxes agentloop.Sandboxes
	artifacts func(sessionID string) artifact.View
	// open loads a session's conversation for one turn (the parent's
	// ConversationManager.Open).
	open   func(ctx context.Context, sessionID string) (*Conversation, bool, error)
	logger *slog.Logger
	// secrets (A-101) is the parent's secrets - a type child uses the secrets its parent may
	// use (never manage_secret). Nil means none.
	secrets *secretManager
}

// typeTurn is one turn of a subagent type, as runTypeTurn gets it.
type typeTurn struct {
	sessionID       string
	prompt          string
	traceAttributes map[string]any
	interaction     toolhub.InteractionHandler
}

// typeTools is the tools a type may use in a parent with parentTools: its list (or all
// of the parent's) intersected with the parent's, minus the excluded ones.
func typeTools(t subagenttype.Type, parentTools []string) []string {
	wanted := t.Tools
	if wanted == nil {
		wanted = parentTools
	}
	out := []string{}
	for _, name := range wanted {
		if slices.Contains(parentTools, name) && !subagent.IsChildExcludedTool(name) {
			out = append(out, name)
		}
	}
	return out
}

// typeProfile is the synthetic profile of a type's loop: the parent's id (so its
// sandbox, artifacts and bus accounting apply), the type's system prompt, its tools,
// no skills, no memory, no subagents, and the type's turn limit.
func typeProfile(parent profile.AgentProfile, t subagenttype.Type) profile.AgentProfile {
	p := parent.Clone()
	p.Name = parent.Name + " › " + t.Name
	p.SystemPrompt = t.SystemPrompt
	p.Tools = typeTools(t, parent.Tools)
	p.Skills = []string{}
	p.InitialContext = nil
	p.Subagents = nil
	p.Memory.Enabled = false
	if t.MaxIterations != nil {
		lc := profile.LoopConfig{}
		if p.LoopConfig != nil {
			lc = *p.LoopConfig
		}
		v := *t.MaxIterations
		lc.MaxIterations = &v
		p.LoopConfig = &lc
	}
	return p
}

// runTypeTurn is one turn of a subagent type: an agentloop.Loop built for this run only -
// the type's system prompt, its tools (scoped by its access), no skills, memory, jobs or
// subagents - in the child's session (a side branch of the parent, created for this run
// and used by nothing else, so no turn gate is needed). Events and tool contexts carry
// the parent's agent id, so the parent's sandbox, artifacts and bus accounting apply.
func runTypeTurn(ctx context.Context, d childLoopDeps, parent profile.AgentProfile, t subagenttype.Type, turn typeTurn) (subagent.TurnResult, error) {
	p := typeProfile(parent, t)
	scope := childToolScope(t.Access)
	var loopOpts []agentloop.Option
	if d.logger != nil {
		loopOpts = append(loopOpts, agentloop.WithLogger(d.logger))
	}
	var ctxOpts []ContextOption
	var secrets secret.Secrets
	if d.secrets != nil {
		ctxOpts = append(ctxOpts, WithSecretIndex(func(ctx context.Context) (string, bool) { return d.secrets.buildIndex(ctx, false) }))
		secrets = d.secrets
	}
	loop, err := agentloop.New(agentloop.Deps{
		Models:      d.models,
		Tools:       NewToolManager(d.toolHub, p),
		Context:     NewContextManager(p, ctxOpts...),
		Events:      d.bus,
		Profile:     p.Clone,
		Trace:       d.trace,
		Sandbox:     d.sandboxes,
		Artifacts:   d.artifacts,
		Interaction: func(string) toolhub.InteractionHandler { return turn.interaction },
		ToolScope:   func(string) func(toolhub.Definition) bool { return scope },
		Secrets:     secrets,
	}, loopOpts...)
	if err != nil {
		return subagent.TurnResult{}, err
	}
	conv, ok, err := d.open(ctx, turn.sessionID)
	if err != nil {
		if ctx.Err() != nil && errors.Is(err, ctx.Err()) {
			return subagent.TurnResult{Aborted: true}, nil
		}
		return subagent.TurnResult{}, err
	}
	if !ok {
		return subagent.TurnResult{}, fmt.Errorf("session %s not found", turn.sessionID)
	}
	res, err := loop.Run(ctx, turn.prompt, turn.sessionID, llmhub.ModelOptions{Provider: t.Provider, Model: t.Model},
		agentloop.RunOptions{Conversation: conv, TraceAttributes: turn.traceAttributes})
	if err != nil {
		if ctx.Err() != nil && errors.Is(err, ctx.Err()) {
			return subagent.TurnResult{Aborted: true}, nil
		}
		return subagent.TurnResult{}, err
	}
	return subagent.TurnResult{Output: res.Message.Content, Usage: res.Usage, Aborted: res.Aborted, StopReason: res.StopReason}, nil
}
