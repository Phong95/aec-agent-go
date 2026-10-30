package agent_test

import (
	"context"
	"strings"
	"testing"

	"github.com/Phong95/aec-agent-go/aecmock"
	"github.com/Phong95/aec-agent-go/components/agent"
	"github.com/Phong95/aec-agent-go/components/agent/command"
	"github.com/Phong95/aec-agent-go/components/agent/skilllearning"
	"github.com/Phong95/aec-agent-go/components/agent/skilllearning/learning"
	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/kernel"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// A-166 skill learning through the Agent: the /learn command, the runtime config, and
// (A-174) the real verification replay, which fails without an isolated sandbox.

const learnedDraft = `{"reusable": true, "action": "create", "name": "door-export",
	"description": "Export a door schedule. Use when the user asks for a door schedule.",
	"steps": ["Read the model", "Write the workbook"], "pitfalls": [], "parameters": [], "requiresTools": []}`

// learningAgent is an agent with skill learning set to cfg; its model writes learnedDraft
// for a distill call and accepts every verdict.
func learningAgent(t *testing.T, cfg *learning.Config) (*agent.Agent, *env) {
	t.Helper()
	e := newEnv(t)
	m := aecmock.NewMockProvider("learner", func(msgs []kernel.Message) string {
		switch {
		case strings.Contains(msgs[0].Content, "reusable skill"):
			return learnedDraft
		case strings.Contains(msgs[0].Content, "accepted the result"):
			return `{"verdict":"accept"}`
		default:
			return "done"
		}
	})
	if err := e.llm.Register(m); err != nil {
		t.Fatal(err)
	}
	p := testProfile("learner-" + kernel.NewID()[:8])
	p.Provider = "learner"
	p.SkillLearning = cfg
	a := e.spawn(t, p)
	return a, e
}

// playHardTurn writes and announces one finished turn with tool calls, as the loop would.
func playHardTurn(t *testing.T, e *env, a *agent.Agent, sessionID string, calls int) {
	t.Helper()
	ctx := t.Context()
	id := a.Profile().ID
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(e.hub.Messages.Append(ctx, sessionID, kernel.Message{Role: kernel.RoleUser, Content: "Export the doors"}))
	events.Emit(e.bus, events.UserMessage, sessionID, events.UserMessagePayload{AgentID: id, Role: "user", Content: "Export the doors"})
	for i := range calls {
		cid := kernel.NewID()
		must(e.hub.Messages.Append(ctx, sessionID, kernel.Message{Role: kernel.RoleAssistant, ToolCalls: []kernel.ToolCall{{ID: cid, Name: "terminal"}}}))
		events.Emit(e.bus, events.ToolDispatched, sessionID, events.DispatchStartPayload{AgentID: id, Name: "terminal"})
		events.Emit(e.bus, events.ToolCompleted, sessionID, events.DispatchEndPayload{AgentID: id, Name: "terminal", IsError: i%2 == 1})
		must(e.hub.Messages.Append(ctx, sessionID, kernel.Message{Role: kernel.RoleTool, ToolResults: []kernel.ToolResult{{ToolCallID: cid, Content: "ok"}}}))
	}
	must(e.hub.Messages.Append(ctx, sessionID, kernel.Message{Role: kernel.RoleAssistant, Content: "Done."}))
	events.Emit(e.bus, events.RunCompleted, sessionID, events.RunCompletedPayload{AgentID: id, Iterations: 2})
}

func TestLearnCommandCapturesTheLastTurn(t *testing.T) {
	a, e := learningAgent(t, &learning.Config{Enabled: true})
	s, err := a.StartSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	run := func() command.Result {
		return a.Commands().Run(t.Context(), "/learn", command.Invocation{SessionID: s.ID, Client: "web"})
	}
	if res := run(); res.Error != command.ResultNotFound {
		t.Fatalf("before a turn = %+v", res)
	}
	playHardTurn(t, e, a, s.ID, 1)
	if res := run(); res.Error != "" || !strings.Contains(res.Text, "draft skill") {
		t.Fatalf("learn = %+v", res)
	}
	waitLearner(t, a)
	ps, err := a.SkillLearning().ListProposals(t.Context(), learning.ProposalFilter{})
	if err != nil || len(ps) != 1 || ps[0].SkillName != "door-export" {
		t.Fatalf("proposals = %+v %v", ps, err)
	}
	if _, err := a.SkillLearning().Accept(t.Context(), ps[0].ID, nil); err != nil {
		t.Fatal(err)
	}
	if !contains(a.Profile().Skills, "door-export") {
		t.Fatalf("skills = %v", a.Profile().Skills)
	}
}

func TestLearnCommandWhenOffAndRuntimeToggle(t *testing.T) {
	a, _ := learningAgent(t, nil)
	s, _ := a.StartSession(t.Context())
	res := a.Commands().Run(t.Context(), "/learn", command.Invocation{SessionID: s.ID, Client: "web"})
	if res.Error != command.ResultUnavailable || !strings.Contains(res.Text, "off") {
		t.Fatalf("off = %+v", res)
	}
	a.UpdateSkillLearningConfig(&learning.Config{Enabled: true})
	if !a.SkillLearning().Enabled() || a.Profile().SkillLearning == nil || !a.Profile().SkillLearning.Enabled {
		t.Fatal("not switched on")
	}
	a.UpdateSkillLearningConfig(nil)
	if a.SkillLearning().Enabled() || a.Profile().SkillLearning != nil {
		t.Fatal("not switched off")
	}
}

// A-174: the real replay needs an isolated sandbox; without one it fails, the draft waits
// for review with the reason, and no throwaway session is left behind.
func TestRealReplayWithoutAnIsolatedSandboxLeavesTheDraftPending(t *testing.T) {
	a, e := learningAgent(t, &learning.Config{Enabled: true, Mode: learning.ModeAuto})
	s, _ := a.StartSession(t.Context())
	playHardTurn(t, e, a, s.ID, 1)
	if res := a.Commands().Run(t.Context(), "/learn", command.Invocation{SessionID: s.ID, Client: "web"}); res.Error != "" {
		t.Fatalf("learn = %+v", res)
	}
	waitLearner(t, a)
	ps, _ := a.SkillLearning().ListProposals(t.Context(), learning.ProposalFilter{})
	if len(ps) != 1 || ps[0].Status != learning.StatusPending || ps[0].Verification == nil || ps[0].Verification.Passed || ps[0].Verification.Reason == "" {
		t.Fatalf("proposals = %+v", ps)
	}
	var meta ordered.Object
	meta.Set("source", skilllearning.VerificationSource)
	left, err := e.hub.Sessions.List(t.Context(), kernel.SessionFilter{Metadata: meta})
	if err != nil || len(left) != 0 {
		t.Fatalf("replay sessions left = %d %v", len(left), err)
	}
}

func TestInspectReplay(t *testing.T) {
	isErr := true
	msgs := []kernel.Message{
		{Role: kernel.RoleUser, Content: "do it"},
		{Role: kernel.RoleAssistant, ToolCalls: []kernel.ToolCall{{ID: "s", Name: "skill_view", Arguments: kernel.Arguments(`{"name":"door-export"}`)}}},
		{Role: kernel.RoleTool, ToolResults: []kernel.ToolResult{{ToolCallID: "s", Content: "<skill_content>"}}},
		{Role: kernel.RoleAssistant, ToolCalls: []kernel.ToolCall{{ID: "r", Name: "read_file"}, {ID: "w", Name: "write_file"}, {ID: "x", Name: "terminal"}}},
		{Role: kernel.RoleTool, ToolResults: []kernel.ToolResult{{ToolCallID: "r", Content: "a"}, {ToolCallID: "w", Content: "ok"}, {ToolCallID: "x", Content: "boom", IsError: &isErr}}},
	}
	risk := func(n string) string {
		if n == "read_file" {
			return "read-only"
		}
		return "draft"
	}
	got := agent.InspectReplay(msgs, "door-export", risk)
	if !got.SkillUsed || !got.ChangedEnvironment || got.ToolCalls != 3 || got.FailedCalls != 1 {
		t.Fatalf("got = %+v", got)
	}
	got = agent.InspectReplay(msgs, "other", func(n string) string {
		if n == "write_file" {
			return "read-only"
		}
		return risk(n)
	})
	if got.SkillUsed || got.ChangedEnvironment || got.ToolCalls != 3 || got.FailedCalls != 1 {
		t.Fatalf("got = %+v", got)
	}
}

func waitLearner(t *testing.T, a *agent.Agent) {
	t.Helper()
	w, ok := a.SkillLearning().(interface{ WaitForIdle(context.Context) error })
	if !ok {
		t.Fatal("the learner has no WaitForIdle")
	}
	if err := w.WaitForIdle(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
