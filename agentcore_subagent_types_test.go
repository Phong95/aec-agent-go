package aec_test

import (
	"context"
	"encoding/json"
	"iter"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	aec "github.com/Phong95/aec-agent-go"
	"github.com/Phong95/aec-agent-go/components/agent"
	"github.com/Phong95/aec-agent-go/components/agent/profile"
	"github.com/Phong95/aec-agent-go/components/agent/subagent"
	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/components/subagenthub/subagenttype"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/components/toolhub/builtin/subagents"
	"github.com/Phong95/aec-agent-go/kernel"
)

// A-105 design revision: subagent TYPES - definitions from the SubagentHub library that
// run as a short-lived loop inside the parent agent (vs. delegating to an existing
// agent, covered by components/agent/subagents_test.go). The "subagent types (A-105
// revision)" part of tests/unit/subagent-types.test.ts of core/ts; the SubagentHub part
// is in components/subagenthub.

// typeScript answers a model call of a type's loop.
type typeScript func(ctx context.Context, msgs []kernel.Message, opts llmhub.ModelOptions) (kernel.ModelResponse, error)

// typesLLM plays the parent ("You are the lead") and every type's loop (any other
// system prompt): the parent delegates on its first call, then reports the first line of
// every result that came back.
type typesLLM struct {
	delegate []map[string]any
	child    typeScript

	mu        sync.Mutex
	delegated bool
	calls     []typesCall
}

type typesCall struct {
	msgs []kernel.Message
	opts llmhub.ModelOptions
}

func (*typesLLM) Name() string    { return "main-llm" }
func (*typesLLM) Version() string { return "1.0.0" }

func (p *typesLLM) Chat(ctx context.Context, msgs []kernel.Message, opts llmhub.ModelOptions) (kernel.ModelResponse, error) {
	p.mu.Lock()
	p.calls = append(p.calls, typesCall{msgs: slices.Clone(msgs), opts: opts})
	lead := strings.HasPrefix(systemOf(msgs), "You are the lead")
	first := lead && !p.delegated
	if first {
		p.delegated = true
	}
	p.mu.Unlock()
	if lead {
		if first {
			calls := make([]kernel.ToolCall, len(p.delegate))
			for i, args := range p.delegate {
				raw, _ := json.Marshal(args)
				calls[i] = kernel.ToolCall{ID: "tc-" + strconv.Itoa(i+1), Name: "spawn_subagent", Arguments: kernel.Arguments(raw)}
			}
			return typesToolUse(calls), nil
		}
		var lines []string
		for _, r := range resultsOf(msgs) {
			lines = append(lines, strings.SplitN(r.Content, "\n", 2)[0])
		}
		return typesReply(strings.Join(lines, " | ")), nil
	}
	return p.child(ctx, msgs, opts)
}

func (*typesLLM) Stream(context.Context, []kernel.Message, llmhub.ModelOptions) iter.Seq2[kernel.StreamChunk, error] {
	return func(yield func(kernel.StreamChunk, error) bool) {
		yield(kernel.StreamChunk{Type: kernel.ChunkDone}, nil)
	}
}

func (*typesLLM) CountTokens(context.Context, []kernel.Message) (int, error) { return 10, nil }

func (p *typesLLM) recorded() []typesCall {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.calls)
}

func typesReply(content string) kernel.ModelResponse {
	return kernel.ModelResponse{Message: kernel.Message{Role: kernel.RoleAssistant, Content: content}, Usage: kernel.TokenUsage{InputTokens: 10, OutputTokens: 5}, StopReason: kernel.StopEndTurn}
}

func typesToolUse(calls []kernel.ToolCall) kernel.ModelResponse {
	return kernel.ModelResponse{Message: kernel.Message{Role: kernel.RoleAssistant, ToolCalls: calls}, Usage: kernel.TokenUsage{InputTokens: 10, OutputTokens: 5}, StopReason: kernel.StopToolUse}
}

// typesCallTool is one tool call of a type's loop.
func typesCallTool(name string) kernel.ModelResponse {
	return typesToolUse([]kernel.ToolCall{{ID: "tc-" + name + "-" + strconv.FormatInt(time.Now().UnixNano(), 36), Name: name, Arguments: kernel.Arguments("{}")}})
}

func systemOf(msgs []kernel.Message) string {
	for _, m := range msgs {
		if m.Role == kernel.RoleSystem {
			return m.Content
		}
	}
	return ""
}

func resultsOf(msgs []kernel.Message) []kernel.ToolResult {
	for i := len(msgs) - 1; i >= 0; i-- {
		if len(msgs[i].ToolResults) > 0 {
			return msgs[i].ToolResults
		}
	}
	return nil
}

func toolNames(defs []toolhub.Definition) []string {
	out := []string{}
	for _, d := range defs {
		out = append(out, d.Name)
	}
	return out
}

// typeTool is a tool with a risk; execute defaults to "<name> ok".
type typeTool struct {
	name    string
	risk    toolhub.Risk
	execute func() toolhub.Output
}

func (t typeTool) Name() string    { return "tool:" + t.name }
func (t typeTool) Version() string { return "1.0.0" }
func (t typeTool) Definition() toolhub.Definition {
	return toolhub.Definition{Name: t.name, Description: t.name, Parameters: toolhub.JSONSchema{Type: "object"}, Risk: t.risk}
}

func (t typeTool) Execute(context.Context, kernel.Arguments, toolhub.Context) (toolhub.Output, error) {
	if t.execute != nil {
		return t.execute(), nil
	}
	return toolhub.Output{Content: t.name + " ok"}, nil
}

// approvalLog approves every tool and records the requests.
type approvalLog struct {
	mu   sync.Mutex
	reqs []toolhub.ApprovalRequest
}

func (*approvalLog) AskUser(context.Context, toolhub.ClarifyRequest) (toolhub.ClarifyResponse, error) {
	return toolhub.ClarifyResponse{Answer: "?"}, nil
}

func (l *approvalLog) RequestApproval(_ context.Context, r toolhub.ApprovalRequest) (toolhub.ApprovalDecision, error) {
	l.mu.Lock()
	l.reqs = append(l.reqs, r)
	l.mu.Unlock()
	return toolhub.ApprovalDecision{Approved: true}, nil
}

func (l *approvalLog) recorded() []toolhub.ApprovalRequest {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.reqs)
}

type typesSetup struct {
	core      *aec.AgentCore
	parent    *agent.Agent
	llm       *typesLLM
	sessionID string
	approvals *approvalLog
	mu        sync.Mutex
	events    []kernel.Event
}

type typesOptions struct {
	delegate    []map[string]any
	child       typeScript
	types       []string // nil = general-purpose
	tools       []typeTool
	parentTools []string // nil = every tool of tools
}

func setupTypes(t *testing.T, o typesOptions) *typesSetup {
	t.Helper()
	s := &typesSetup{core: subagentCore(t), approvals: &approvalLog{}}
	if err := s.core.UseTool(subagents.NewSpawnSubagentTool()); err != nil {
		t.Fatal(err)
	}
	for _, tool := range o.tools {
		if err := s.core.UseTool(tool); err != nil {
			t.Fatal(err)
		}
	}
	s.core.ToolHub().SetInteractionHandler(s.approvals)
	s.core.Events().OnAny(func(e kernel.Event) {
		s.mu.Lock()
		s.events = append(s.events, e)
		s.mu.Unlock()
	})
	s.llm = &typesLLM{delegate: o.delegate, child: o.child}
	if err := s.core.UseLLMProvider(s.llm); err != nil {
		t.Fatal(err)
	}
	parentTools := o.parentTools
	if parentTools == nil {
		for _, tool := range o.tools {
			parentTools = append(parentTools, tool.name)
		}
	}
	types := o.types
	if types == nil {
		types = []string{"general-purpose"}
	}
	zero := 0
	parent, err := s.core.SpawnAgent(t.Context(), profile.AgentProfile{
		ID:           "lead",
		Name:         "Lead",
		SystemPrompt: "You are the lead.",
		Provider:     "main-llm",
		Tools:        append([]string{"spawn_subagent"}, parentTools...),
		LoopConfig:   &profile.LoopConfig{MaxToolRetries: &zero, RetryBackoffMs: &zero},
		Subagents:    &subagent.Config{Allow: []subagent.TargetConfig{}, Types: types},
	})
	if err != nil {
		t.Fatal(err)
	}
	s.parent = parent
	session, err := parent.StartSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	s.sessionID = session.ID
	return s
}

func (s *typesSetup) chat(t *testing.T) string {
	t.Helper()
	res, err := s.parent.Chat(t.Context(), "go", agent.WithChatSession(s.sessionID))
	if err != nil {
		t.Fatal(err)
	}
	return res.Message.Content
}

func (s *typesSetup) runs(t *testing.T) []subagent.Run {
	t.Helper()
	runs, err := s.parent.Subagents().List(t.Context(), s.sessionID)
	if err != nil {
		t.Fatal(err)
	}
	return runs
}

func TestSubagentTypes_RunsABuiltInTypeInsideTheParentReadOnlyInASideBranchSession(t *testing.T) {
	var mu sync.Mutex
	var childTools []string
	var childSystem string
	s := setupTypes(t, typesOptions{
		tools:    []typeTool{{name: "look", risk: toolhub.RiskReadOnly}, {name: "write_thing", risk: toolhub.RiskDraft}, {name: "send_rfi", risk: toolhub.RiskConsequential}},
		delegate: []map[string]any{{"subagent": "general-purpose", "task": "look around"}},
		child: func(_ context.Context, msgs []kernel.Message, opts llmhub.ModelOptions) (kernel.ModelResponse, error) {
			mu.Lock()
			childTools, childSystem = toolNames(opts.Tools), systemOf(msgs)
			mu.Unlock()
			if r := resultsOf(msgs); len(r) > 0 {
				return typesReply("report: " + r[0].Content), nil
			}
			return typesCallTool("look"), nil
		},
	})
	if got := s.chat(t); !regexp.MustCompile(`Subagent "general-purpose" .* status: completed`).MatchString(got) {
		t.Fatalf("reply = %q", got)
	}
	mu.Lock()
	if !slices.Equal(childTools, []string{"look"}) {
		t.Errorf("child tools = %v, want [look]", childTools)
	}
	if !strings.Contains(childSystem, "You are a focused assistant") || strings.Contains(childSystem, "You are the lead") || strings.Contains(childSystem, "<available_subagents>") {
		t.Errorf("child system prompt = %q", childSystem)
	}
	mu.Unlock()

	runs := s.runs(t)
	if len(runs) != 1 {
		t.Fatalf("%d runs, want 1", len(runs))
	}
	run := runs[0]
	if run.Kind != subagent.KindType || run.ChildAgentID != "lead" || run.ChildAgentName != "general-purpose" || run.Access != subagent.AccessReadOnly ||
		run.Status != subagent.StatusCompleted || run.Output != "report: look ok" || run.ToolCalls != 1 {
		t.Fatalf("run = %+v", run)
	}
	childSession, err := s.core.PersistenceHub().Sessions.Get(t.Context(), run.ChildSessionID)
	if err != nil || childSession == nil {
		t.Fatalf("child session: %v %v", childSession, err)
	}
	for k, want := range map[string]any{"agentId": "lead", "source": "subagent", "kind": "type", "parentSessionId": s.sessionID} {
		if got, _ := childSession.Metadata.Get(k); got != want {
			t.Errorf("child session metadata %s = %v, want %v", k, got, want)
		}
	}
	// The parent's own conversation holds none of the child's steps.
	parentMessages, err := s.core.PersistenceHub().Messages.Get(t.Context(), s.sessionID)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range parentMessages {
		for _, c := range m.ToolCalls {
			if c.Name == "look" {
				t.Fatal("the parent's conversation holds the child's tool call")
			}
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !slices.ContainsFunc(s.events, func(e kernel.Event) bool {
		return e.Type == events.SubagentCompleted.Name && e.SessionID == s.sessionID
	}) {
		t.Fatal("no subagent:completed event in the parent's session")
	}
}

func TestSubagentTypes_ListsEnabledTypesInTheIndexAndRefusesATypeNotEnabled(t *testing.T) {
	s := setupTypes(t, typesOptions{
		delegate: []map[string]any{{"subagent": "researcher", "task": "x"}},
		child: func(context.Context, []kernel.Message, llmhub.ModelOptions) (kernel.ModelResponse, error) {
			return typesReply("never"), nil
		},
	})
	s.chat(t)
	calls := s.llm.recorded()
	first := systemOf(calls[0].msgs)
	if !strings.Contains(first, "- general-purpose (subagent type, read-only): A general assistant") || strings.Contains(first, "researcher (subagent type") {
		t.Fatalf("index = %q", first)
	}
	want := `"researcher" is not one of your subagents ` + "—" + ` you may delegate to: "general-purpose"`
	if r := resultsOf(calls[1].msgs); len(r) == 0 || !strings.Contains(r[0].Content, want) {
		t.Fatalf("spawn result = %+v, want %q", r, want)
	}
}

func TestSubagentTypes_ACustomWriteTypeGetsItsToolsItsModelAndTheWriterLock(t *testing.T) {
	gate := make(chan struct{})
	var mu sync.Mutex
	var writes []string
	var problems []string
	s := setupTypes(t, typesOptions{
		tools: []typeTool{
			{name: "look", risk: toolhub.RiskReadOnly},
			{name: "write_thing", risk: toolhub.RiskDraft, execute: func() toolhub.Output {
				mu.Lock()
				writes = append(writes, "w")
				mu.Unlock()
				return toolhub.Output{Content: "written"}
			}},
			{name: "other", risk: toolhub.RiskDraft},
		},
		types:    []string{"drafter"},
		delegate: []map[string]any{{"subagent": "drafter", "task": "draft 1"}, {"subagent": "drafter", "task": "draft 2"}},
		child: func(ctx context.Context, msgs []kernel.Message, opts llmhub.ModelOptions) (kernel.ModelResponse, error) {
			mu.Lock()
			if opts.Model != "cheap-model" {
				problems = append(problems, "model "+opts.Model)
			}
			if got := toolNames(opts.Tools); !slices.Equal(got, []string{"write_thing"}) {
				problems = append(problems, "tools "+strings.Join(got, ","))
			}
			mu.Unlock()
			if len(resultsOf(msgs)) > 0 {
				return typesReply("drafted"), nil
			}
			select {
			case <-gate:
			case <-ctx.Done():
				return kernel.ModelResponse{}, ctx.Err()
			}
			return typesCallTool("write_thing"), nil
		},
	})
	if _, err := s.core.SubagentHub().Create(t.Context(), subagenttype.Definition{
		Name: "drafter", Description: "Drafts documents", SystemPrompt: "You draft.",
		Tools: []string{"write_thing", "not_a_parent_tool"}, Access: subagent.AccessWrite, Model: "cheap-model",
	}); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(50 * time.Millisecond)
		close(gate)
	}()
	got := s.chat(t)
	if !strings.Contains(got, "status: completed") || !strings.Contains(got, "already working in your sandbox") {
		t.Fatalf("reply = %q", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(writes, []string{"w"}) || len(problems) > 0 {
		t.Fatalf("writes = %v, problems = %v", writes, problems)
	}
}

func TestSubagentTypes_ATypesApprovalGatedCallAsksTheUserInTheParentsChat(t *testing.T) {
	s := setupTypes(t, typesOptions{
		tools:    []typeTool{{name: "send_rfi", risk: toolhub.RiskConsequential}},
		types:    []string{"mailer"},
		delegate: []map[string]any{{"subagent": "mailer", "task": "send"}},
		child: func(_ context.Context, msgs []kernel.Message, _ llmhub.ModelOptions) (kernel.ModelResponse, error) {
			if r := resultsOf(msgs); len(r) > 0 {
				return typesReply(r[0].Content), nil
			}
			return typesCallTool("send_rfi"), nil
		},
	})
	if _, err := s.core.SubagentHub().Create(t.Context(), subagenttype.Definition{Name: "mailer", Description: "Sends mail", SystemPrompt: "You mail.", Access: subagent.AccessWrite}); err != nil {
		t.Fatal(err)
	}
	s.chat(t)
	runs := s.runs(t)
	if len(runs) != 1 {
		t.Fatalf("%d runs, want 1", len(runs))
	}
	run := runs[0]
	approvals := s.approvals.recorded()
	var names []string
	for _, a := range approvals {
		names = append(names, a.ToolName)
	}
	if !slices.Equal(names, []string{"spawn_subagent", "send_rfi"}) {
		t.Fatalf("approvals = %v", names)
	}
	a := approvals[1]
	if a.SessionID != s.sessionID || a.AgentID != "lead" || a.Subagent == nil ||
		*a.Subagent != (toolhub.ApprovalSubagent{AgentID: "lead", AgentName: "mailer", SessionID: run.ChildSessionID, RunID: run.ID}) {
		t.Fatalf("approval = %+v (subagent %+v)", a, a.Subagent)
	}
	if run.Output != "send_rfi ok" {
		t.Fatalf("output = %q", run.Output)
	}
}

// A-135: a type's approval-gated call uses the parent session's YOLO mode.
func TestSubagentTypes_ATypesApprovalGatedCallUsesTheParentSessionsYoloMode(t *testing.T) {
	s := setupTypes(t, typesOptions{
		tools:    []typeTool{{name: "send_rfi", risk: toolhub.RiskConsequential}},
		types:    []string{"mailer"},
		delegate: []map[string]any{{"subagent": "mailer", "task": "send"}},
		child: func(_ context.Context, msgs []kernel.Message, _ llmhub.ModelOptions) (kernel.ModelResponse, error) {
			if r := resultsOf(msgs); len(r) > 0 {
				return typesReply(r[0].Content), nil
			}
			return typesCallTool("send_rfi"), nil
		},
	})
	if _, err := s.core.SubagentHub().Create(t.Context(), subagenttype.Definition{Name: "mailer", Description: "Sends mail", SystemPrompt: "You mail.", Access: subagent.AccessWrite}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.parent.SetApprovalMode(t.Context(), s.sessionID, kernel.ApprovalModeYolo); err != nil || !ok {
		t.Fatalf("SetApprovalMode: ok=%v err=%v", ok, err)
	}
	s.chat(t)
	runs := s.runs(t)
	if len(runs) != 1 {
		t.Fatalf("%d runs, want 1", len(runs))
	}
	if approvals := s.approvals.recorded(); len(approvals) != 0 {
		t.Fatalf("approvals = %+v, want none", approvals)
	}
	if runs[0].Output != "send_rfi ok" {
		t.Fatalf("output = %q", runs[0].Output)
	}
	s.mu.Lock()
	var auto []string
	for _, e := range s.events {
		if e.Type == events.ToolAutoApproved.Name {
			auto = append(auto, e.Payload.(events.ToolAutoApprovedPayload).Name)
		}
	}
	s.mu.Unlock()
	if !slices.Equal(auto, []string{"spawn_subagent", "send_rfi"}) {
		t.Fatalf("auto-approved = %v", auto)
	}
}

func TestSubagentTypes_ATypeNameWinsOverAnAgentWithTheSameName(t *testing.T) {
	s := setupTypes(t, typesOptions{
		delegate: []map[string]any{{"subagent": "general-purpose", "task": "x"}},
		child: func(context.Context, []kernel.Message, llmhub.ModelOptions) (kernel.ModelResponse, error) {
			return typesReply("from type"), nil
		},
	})
	if _, err := s.core.SpawnAgent(t.Context(), profile.AgentProfile{ID: "gp-agent", Name: "general-purpose", SystemPrompt: "agent", Provider: "main-llm"}); err != nil {
		t.Fatal(err)
	}
	s.parent.UpdateSubagents(&subagent.Config{Allow: []subagent.TargetConfig{{AgentID: "gp-agent"}}, Types: []string{"general-purpose"}})
	s.chat(t)
	runs := s.runs(t)
	if len(runs) != 1 || runs[0].Kind != subagent.KindType || runs[0].Output != "from type" {
		t.Fatalf("runs = %+v", runs)
	}
	// Agents stay reachable by id.
	var kinds []string
	for _, x := range s.parent.Subagents().Targets(t.Context()) {
		kinds = append(kinds, string(x.Kind)+":"+x.AgentID)
	}
	if !slices.Equal(kinds, []string{"type:general-purpose", "agent:gp-agent"}) {
		t.Fatalf("targets = %v", kinds)
	}
}

func TestSubagentTypes_DeletingACustomTypeDisablesItForEveryAgent(t *testing.T) {
	s := setupTypes(t, typesOptions{
		types: []string{"general-purpose", "temp"},
		child: func(context.Context, []kernel.Message, llmhub.ModelOptions) (kernel.ModelResponse, error) {
			return typesReply("x"), nil
		},
	})
	hub := s.core.SubagentHub()
	if _, err := hub.Create(t.Context(), subagenttype.Definition{Name: "temp", Description: "d", SystemPrompt: "p"}); err != nil {
		t.Fatal(err)
	}
	if err := hub.Delete(t.Context(), "temp"); err != nil {
		t.Fatal(err)
	}
	if got := s.parent.Profile().Subagents.Types; !slices.Equal(got, []string{"general-purpose"}) {
		t.Fatalf("types = %v, want [general-purpose]", got)
	}
	stored, err := s.core.PersistenceHub().Agents.Load(t.Context(), "lead")
	if err != nil || stored == nil || !slices.Equal(stored.Subagents.Types, []string{"general-purpose"}) {
		t.Fatalf("stored profile = %+v (%v), want the types saved", stored, err)
	}
}

func TestSubagentTypes_UseSubagentTypeRegistersAnAppType(t *testing.T) {
	core := subagentCore(t)
	if err := core.UseSubagentType(subagenttype.Definition{Name: "checker", Description: "Checks", SystemPrompt: "You check."}); err != nil {
		t.Fatal(err)
	}
	if err := core.UseSubagentType(subagenttype.Definition{Name: "checker", Description: "Checks", SystemPrompt: "You check."}); err == nil {
		t.Fatal("a taken name was registered twice")
	}
	types, err := core.SubagentHub().List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, x := range types {
		got = append(got, string(x.Source)+":"+x.Name)
	}
	if !slices.Equal(got, []string{"app:checker", "built-in:general-purpose", "built-in:researcher"}) {
		t.Fatalf("types = %v", got)
	}
}
