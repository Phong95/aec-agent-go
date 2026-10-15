package agent_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/Phong95/aec-agent-go/aecmock"
	"github.com/Phong95/aec-agent-go/components/agent"
	"github.com/Phong95/aec-agent-go/components/agent/agentloop"
	"github.com/Phong95/aec-agent-go/components/agent/profile"
	"github.com/Phong95/aec-agent-go/components/artifacthub"
	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/components/persistence"
	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/components/skillhub"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/components/trace"
	"github.com/Phong95/aec-agent-go/kernel"
)

// testCompactor is the conformance suite's default compactor: a 200000 token window with
// a 0.8 threshold, which these short runs never reach.
func testCompactor() profile.CompactorConfig {
	return profile.CompactorConfig{
		ContextWindowTokens:      200_000,
		Threshold:                0.8,
		MinTailUserMessages:      4,
		ProtectFirstN:            2,
		ProactivePruneTokens:     50_000,
		OldToolResultMinTokens:   2_000,
		TailTargetRatio:          0.2,
		ProtectLastN:             10,
		SummarizeMaxOutputTokens: 4_096,
	}
}

// testProfile is the profile of the agent tests: a system prompt and the default
// compactor, no tools.
func testProfile(id string) profile.AgentProfile {
	return profile.AgentProfile{
		ID:           id,
		Name:         id,
		SystemPrompt: "You are a helpful assistant.",
		Compactor:    testCompactor(),
	}
}

// env is the hubs an agent is built over. Several agents of one env share the
// persistence hub, the skill hub, the sandbox hub and the artifact hub, like the
// agents of one AgentCore.
type env struct {
	llm       *llmhub.LLMHub
	tools     *toolhub.ToolHub
	hub       *persistence.PersistenceHub
	skills    *skillhub.SkillHub
	sandboxes *sandboxhub.SandboxHub
	artifacts *artifacthub.ArtifactHub
	trace     *trace.Service
	bus       *events.Dispatcher
	mocks     map[string]*aecmock.MockProvider
}

// newEnv builds the hubs and registers one mock provider per name; the first
// one is the primary. Each provider answers "reply from <name>".
func newEnv(t *testing.T, providers ...string) *env {
	t.Helper()
	e := &env{
		llm:       llmhub.NewLLMHub(),
		tools:     toolhub.NewToolHub(),
		hub:       persistence.NewPersistenceHub(),
		sandboxes: sandboxhub.NewSandboxHub(),
		trace:     trace.NewService(),
		bus:       events.New(),
		mocks:     map[string]*aecmock.MockProvider{},
	}
	// The artifact views read the env's persistence hub, as in an AgentCore.
	e.artifacts = artifacthub.NewArtifactHub(func() persistence.ArtifactStore { return e.hub.Artifacts })
	// The custom skills live in the env's persistence hub, as in an AgentCore.
	e.skills = skillhub.NewSkillHub(func() persistence.SkillStore { return e.hub.Skills })
	if len(providers) == 0 {
		providers = []string{"primary"}
	}
	for _, name := range providers {
		m := aecmock.NewMockProvider(name, func([]kernel.Message) string { return "reply from " + name })
		if err := e.llm.Register(m); err != nil {
			t.Fatalf("register provider %s: %v", name, err)
		}
		e.mocks[name] = m
	}
	return e
}

func (e *env) deps() agent.Deps {
	return agent.Deps{
		LLMHub: e.llm, ToolHub: e.tools, PersistenceHub: e.hub, Events: e.bus, SkillHub: e.skills,
		SandboxHub: e.sandboxes, ArtifactHub: e.artifacts, Trace: e.trace,
	}
}

// spawn builds an agent over the env and disposes it with the test.
func (e *env) spawn(t *testing.T, p profile.AgentProfile, opts ...agent.Option) *agent.Agent {
	t.Helper()
	a, err := agent.New(t.Context(), e.deps(), p, opts...)
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	t.Cleanup(func() { _ = a.Dispose(context.WithoutCancel(t.Context())) })
	return a
}

// newTestAgent is an env with one provider and one agent over it.
func newTestAgent(t *testing.T, p profile.AgentProfile, opts ...agent.Option) (*agent.Agent, *env) {
	t.Helper()
	e := newEnv(t)
	a := e.spawn(t, p, opts...)
	return a, e
}

// chat runs one turn with no options.
func chat(t *testing.T, a *agent.Agent, message string) agentloop.RunResult {
	t.Helper()
	res, err := a.Chat(t.Context(), message)
	if err != nil {
		t.Fatalf("Chat(%q): %v", message, err)
	}
	return res
}

// boolPtr is a pointer to b, for the optional boolean fields of toolhub.Output.
func boolPtr(b bool) *bool { return &b }

// intPtr is a pointer to n.
func intPtr(n int) *int { return &n }

// probeTool is the workspace tool of the agent sandbox cases: it writes a file and
// runs echo through toolhub.Context.GetSandbox, and reports the sandbox id, the working
// directory and the echo output. When the sandbox fails it reports the error code of
// the SandboxError, as a failed, non-retryable result.
type probeTool struct{}

func (probeTool) Name() string    { return "tool:workspace-probe" }
func (probeTool) Version() string { return "1.0.0" }
func (probeTool) Definition() toolhub.Definition {
	return toolhub.Definition{Name: "workspace_probe", Description: "probe", Parameters: toolhub.JSONSchema{Type: "object"}, Risk: toolhub.RiskReadOnly}
}

func (probeTool) Execute(ctx context.Context, _ kernel.Arguments, tc toolhub.Context) (toolhub.Output, error) {
	if tc.GetSandbox == nil {
		return toolhub.Output{Content: "no sandbox accessor", IsError: boolPtr(true)}, nil
	}
	fail := func(err error) (toolhub.Output, error) {
		code := err.Error()
		var se *kernel.SandboxError
		if errors.As(err, &se) {
			code = string(se.Code)
		}
		return toolhub.Output{Content: code, IsError: boolPtr(true), Retryable: boolPtr(false)}, nil
	}
	view, err := tc.GetSandbox(ctx)
	if err != nil {
		return fail(err)
	}
	if _, err := view.WriteFile(ctx, "probe.txt", "SECRET-FILE-CONTENT", sandboxhub.WriteOptions{}); err != nil {
		return fail(err)
	}
	res, err := view.Exec(ctx, sandboxhub.ExecRequest{Command: []string{"echo", "SECRET-ARG"}, TimeoutMs: 1000, MaxOutputBytes: 1000}, sandboxhub.ExecOptions{})
	if err != nil {
		return fail(err)
	}
	return toolhub.Output{Content: view.ID() + " " + view.WorkingDir() + " " + strings.TrimSpace(res.Stdout)}, nil
}

// oneToolCallProvider is a mock model that calls the named tool on its first turn
// (no tool result in its input yet) and ends the turn on its second. lastToolResult
// reads the last tool result it saw.
func oneToolCallProvider(t *testing.T, name, toolName string) *aecmock.MockProvider {
	t.Helper()
	args, err := kernel.NewArguments([]byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	return aecmock.NewMockProvider(name, nil, aecmock.WithResponse(func(msgs []kernel.Message) kernel.ModelResponse {
		usage := kernel.TokenUsage{InputTokens: 1, OutputTokens: 1}
		if slices.ContainsFunc(msgs, func(m kernel.Message) bool { return len(m.ToolResults) > 0 }) {
			return kernel.ModelResponse{Message: kernel.Message{Role: kernel.RoleAssistant, Content: "done"}, Usage: usage, StopReason: kernel.StopEndTurn}
		}
		return kernel.ModelResponse{
			Message:    kernel.Message{Role: kernel.RoleAssistant, ToolCalls: []kernel.ToolCall{{ID: "tc-1", Name: toolName, Arguments: args}}},
			Usage:      usage,
			StopReason: kernel.StopToolUse,
		}
	}))
}

// lastToolResult is the content of the last tool result the mock saw in any call, or
// "" when it saw none.
func lastToolResult(m *aecmock.MockProvider) string {
	result := ""
	for _, c := range m.Calls() {
		for _, msg := range c.Messages {
			for _, r := range msg.ToolResults {
				result = r.Content
			}
		}
	}
	return result
}

// sandboxRig is the setup of the agent sandbox cases: an env whose model calls the
// probe tool once, a docker-kind in-memory sandbox provider, the shared sandbox
// configured (unless asked not to) and one agent with the given sandbox selection.
type sandboxRig struct {
	*env
	agent    *agent.Agent
	model    *aecmock.MockProvider
	provider *aecmock.InMemoryProvider
}

func newSandboxRig(t *testing.T, sel sandboxhub.Selection, configureShared bool, opts ...agent.Option) *sandboxRig {
	t.Helper()
	e := newEnv(t)
	model := oneToolCallProvider(t, "one-tool-call", "workspace_probe")
	if err := e.llm.Register(model); err != nil {
		t.Fatal(err)
	}
	if err := e.tools.Register(probeTool{}); err != nil {
		t.Fatal(err)
	}
	provider := aecmock.NewInMemoryProvider()
	if err := e.sandboxes.Register(provider); err != nil {
		t.Fatal(err)
	}
	if configureShared {
		if err := e.sandboxes.ConfigureShared(sandboxhub.Spec{Kind: sandboxhub.KindDocker}); err != nil {
			t.Fatal(err)
		}
	}
	p := testProfile("probe-agent")
	p.Provider = "one-tool-call"
	p.Tools = []string{"workspace_probe"}
	p.LoopConfig = &profile.LoopConfig{MaxToolRetries: intPtr(0), RetryBackoffMs: intPtr(0)}
	p.Sandbox = sel
	return &sandboxRig{env: e, agent: e.spawn(t, p, opts...), model: model, provider: provider}
}

// spanSink is a trace sink that keeps every span that ended, in the order they
// ended. It is a pointer type, so it is comparable and the service can remove it.
type spanSink struct {
	mu    sync.Mutex
	spans []trace.Span
}

func (*spanSink) OnSpanStart(trace.Span) {}
func (s *spanSink) OnSpanEnd(sp trace.Span) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.spans = append(s.spans, sp)
}
func (*spanSink) Flush(context.Context) error { return nil }

// ofKind returns the ended spans of one kind, in the order they ended.
func (s *spanSink) ofKind(kind trace.SpanKind) []trace.Span {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []trace.Span
	for _, sp := range s.spans {
		if sp.Kind == kind {
			out = append(out, sp)
		}
	}
	return out
}

// sinkOf registers a new span sink on the env's trace service.
func (e *env) sinkOf() *spanSink {
	s := &spanSink{}
	e.trace.AddSink(s)
	return s
}
