package aec_test

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"

	aec "github.com/Phong95/aec-agent-go"
	"github.com/Phong95/aec-agent-go/aecmock"
	"github.com/Phong95/aec-agent-go/components/agent"
	"github.com/Phong95/aec-agent-go/components/agent/profile"
	"github.com/Phong95/aec-agent-go/components/agent/subagent"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/components/toolhub/builtin/subagents"
	"github.com/Phong95/aec-agent-go/kernel"
)

// A-105 - the AgentCore side of subagents: the orchestration is every agent's host,
// UseSubagentRunner runs the child turns, and a deleted agent is dropped from the other
// agents' allowlists (the "drops a deleted agent from other agents' allowlists" case of
// tests/unit/subagents.test.ts of core/ts).

// approveAll approves every tool.
type approveAll struct{}

func (approveAll) AskUser(context.Context, toolhub.ClarifyRequest) (toolhub.ClarifyResponse, error) {
	return toolhub.ClarifyResponse{}, nil
}

func (approveAll) RequestApproval(context.Context, toolhub.ApprovalRequest) (toolhub.ApprovalDecision, error) {
	return toolhub.ApprovalDecision{Approved: true}, nil
}

func subagentCore(t *testing.T) *aec.AgentCore {
	t.Helper()
	core, err := aec.NewAgentCore()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = core.Dispose(context.WithoutCancel(t.Context())) })
	return core
}

func subagentCoreProfile(id string, tools []string, allow ...string) profile.AgentProfile {
	p := profile.AgentProfile{ID: id, Name: id, SystemPrompt: "You are " + id + ".", Tools: tools}
	if len(allow) > 0 {
		p.Subagents = &subagent.Config{}
		for _, a := range allow {
			p.Subagents.Allow = append(p.Subagents.Allow, subagent.TargetConfig{AgentID: a})
		}
	}
	return p
}

func TestAgentCore_RemoveAgentDropsItFromOtherAgentsAllowlists(t *testing.T) {
	core := subagentCore(t)
	if err := core.UseLLMProvider(aecmock.NewMockProvider("primary", func([]kernel.Message) string { return "y" })); err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	for _, id := range []string{"a", "b"} {
		if _, err := core.SpawnAgent(ctx, subagentCoreProfile(id, nil)); err != nil {
			t.Fatal(err)
		}
	}
	parent, err := core.SpawnAgent(ctx, subagentCoreProfile("parent", []string{"spawn_subagent"}, "a", "b"))
	if err != nil {
		t.Fatal(err)
	}
	if err := core.RemoveAgent(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	ids := func(ts []subagent.TargetConfig) []string {
		out := []string{}
		for _, x := range ts {
			out = append(out, x.AgentID)
		}
		return out
	}
	if got := ids(parent.Profile().Subagents.Allow); !slices.Equal(got, []string{"b"}) {
		t.Fatalf("allow = %v, want [b]", got)
	}
	var targets []string
	for _, x := range parent.Subagents().Targets(t.Context()) {
		targets = append(targets, x.AgentID)
	}
	if !slices.Equal(targets, []string{"b"}) {
		t.Fatalf("targets = %v, want [b]", targets)
	}
	stored, err := core.PersistenceHub().Agents.Load(ctx, "parent")
	if err != nil || stored == nil || !slices.Equal(ids(stored.Subagents.Allow), []string{"b"}) {
		t.Fatalf("stored profile = %+v (%v), want the allowlist saved", stored, err)
	}
}

func TestAgentCore_UseSubagentRunnerRunsTheChildTurnsAndRemoveAgentDeletesTheRuns(t *testing.T) {
	core := subagentCore(t)
	// The parent asks for one spawn_subagent call, then answers with the tool result.
	parentLLM := aecmock.NewScriptedProvider("parent-llm", []aecmock.ScriptedTurn{
		{ToolCalls: []kernel.ToolCall{{ID: "tc-1", Name: "spawn_subagent", Arguments: kernel.Arguments(`{"agent":"child","task":"look it up"}`)}}},
		{Content: "done"},
	})
	if err := core.UseLLMProvider(parentLLM); err != nil {
		t.Fatal(err)
	}
	if err := core.UseTool(subagents.NewSpawnSubagentTool()); err != nil {
		t.Fatal(err)
	}
	core.ToolHub().SetInteractionHandler(approveAll{})
	var mu sync.Mutex
	var turns []subagent.TurnContext
	core.UseSubagentRunner(func(_ context.Context, tc subagent.TurnContext) (subagent.TurnResult, error) {
		mu.Lock()
		turns = append(turns, tc)
		mu.Unlock()
		return subagent.TurnResult{Output: "from the host runner", Usage: kernel.TokenUsage{InputTokens: 3, OutputTokens: 4}}, nil
	})
	ctx := t.Context()
	if _, err := core.SpawnAgent(ctx, subagentCoreProfile("child", nil)); err != nil {
		t.Fatal(err)
	}
	p := subagentCoreProfile("parent", []string{"spawn_subagent"}, "child")
	p.Provider = "parent-llm"
	parent, err := core.SpawnAgent(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	session, err := parent.StartSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parent.Chat(ctx, "go", agent.WithChatSession(session.ID)); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	got := slices.Clone(turns)
	mu.Unlock()
	if len(got) != 1 || got[0].AgentID != "child" || got[0].Parent.AgentID != "parent" || !strings.Contains(got[0].Prompt, "look it up") ||
		got[0].TraceAttributes["parentSessionId"] != session.ID {
		t.Fatalf("runner turns = %+v", got)
	}
	runs, err := parent.Subagents().List(ctx, session.ID)
	if err != nil || len(runs) != 1 || runs[0].Output != "from the host runner" || runs[0].Usage.InputTokens != 3 {
		t.Fatalf("runs = %+v (%v)", runs, err)
	}
	if err := core.RemoveAgent(ctx, "parent"); err != nil {
		t.Fatal(err)
	}
	left, err := core.PersistenceHub().SubagentRuns.ListByParentSession(ctx, session.ID)
	if err != nil || len(left) != 0 {
		t.Fatalf("runs after RemoveAgent = %+v (%v)", left, err)
	}
}
