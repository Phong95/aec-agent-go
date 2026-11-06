package agent_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"iter"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/Phong95/aec-agent-go/components/agent"
	"github.com/Phong95/aec-agent-go/components/agent/memorysystem/memory"
	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/components/persistence"
	"github.com/Phong95/aec-agent-go/components/trace"
	"github.com/Phong95/aec-agent-go/kernel"
)

// Memory is recalled on demand, through the memory system's memory_search tool. It is
// no longer injected into every turn: a block that changed each turn broke the provider's
// prompt cache. These cases mirror core/ts tests/unit/agent-memory-recall.test.ts.

// recallPrompt is the system prompt of the memory recall cases, the system prompt
// of every agent built by testProfile.
const recallPrompt = "You are a helpful assistant."

// recallMemory is the memory config of the cases: memory enabled with the interval 0, so
// consolidation never runs and the memories are seeded directly.
func recallMemory(mutate ...func(*memory.Config)) memory.Config {
	zero := 0.0
	cfg := memory.Config{Enabled: true, UpdateUser: true, Consolidation: &memory.ConsolidationTrigger{IntervalMinutes: &zero}}
	for _, m := range mutate {
		m(&cfg)
	}
	return cfg
}

// maxResultsOf sets the recall config to { maxResults: n }.
func maxResultsOf(n int) func(*memory.Config) {
	return func(c *memory.Config) { c.Recall = &memory.RecallConfig{MaxResults: &n} }
}

// recallProvider answers "recall: <query>" with one memory_search call for that query,
// and everything else (the turn's next call, after the tool result, too) with "ok". It
// records every call's messages and the names of the tools offered.
type recallProvider struct {
	mu    sync.Mutex
	calls []recallCall
}

type recallCall struct {
	messages []kernel.Message
	tools    []string
}

var _ llmhub.Provider = (*recallProvider)(nil)

func (*recallProvider) Name() string                     { return "recall" }
func (*recallProvider) Version() string                  { return "1.0.0" }
func (*recallProvider) Initialize(context.Context) error { return nil }
func (*recallProvider) Dispose(context.Context) error    { return nil }
func (*recallProvider) CountTokens(context.Context, []kernel.Message) (int, error) {
	return 1, nil
}

func (p *recallProvider) Chat(_ context.Context, msgs []kernel.Message, opts llmhub.ModelOptions) (kernel.ModelResponse, error) {
	names := make([]string, len(opts.Tools))
	for i, d := range opts.Tools {
		names[i] = d.Name
	}
	p.mu.Lock()
	p.calls = append(p.calls, recallCall{messages: slices.Clone(msgs), tools: names})
	p.mu.Unlock()
	usage := kernel.TokenUsage{InputTokens: 10, OutputTokens: 5}
	if n := len(msgs); n > 0 && msgs[n-1].Role == kernel.RoleUser {
		if q, ok := strings.CutPrefix(msgs[n-1].Content, "recall: "); ok {
			args, _ := json.Marshal(map[string]string{"query": q})
			return kernel.ModelResponse{
				Message:    kernel.Message{Role: kernel.RoleAssistant, ToolCalls: []kernel.ToolCall{{ID: "tc-1", Name: "memory_search", Arguments: kernel.Arguments(args)}}},
				Usage:      usage,
				StopReason: kernel.StopToolUse,
			}, nil
		}
	}
	return kernel.ModelResponse{Message: kernel.Message{Role: kernel.RoleAssistant, Content: "ok"}, Usage: usage, StopReason: kernel.StopEndTurn}, nil
}

func (p *recallProvider) Stream(ctx context.Context, msgs []kernel.Message, opts llmhub.ModelOptions) iter.Seq2[kernel.StreamChunk, error] {
	return func(yield func(kernel.StreamChunk, error) bool) {
		r, _ := p.Chat(ctx, msgs, opts)
		for i := range r.Message.ToolCalls {
			tc := r.Message.ToolCalls[i]
			if !yield(kernel.StreamChunk{Type: kernel.ChunkToolCall, ToolCall: &tc}, nil) {
				return
			}
		}
		if r.Message.Content != "" {
			c := r.Message.Content
			if !yield(kernel.StreamChunk{Type: kernel.ChunkText, Content: &c}, nil) {
				return
			}
		}
		u := r.Usage
		yield(kernel.StreamChunk{Type: kernel.ChunkDone, Usage: &u}, nil)
	}
}

func (p *recallProvider) call(t *testing.T, n int) recallCall {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if n < 0 {
		n += len(p.calls)
	}
	if n < 0 || n >= len(p.calls) {
		t.Fatalf("the model got %d calls, want call %d", len(p.calls), n)
	}
	return p.calls[n]
}

// memRig is one agent over the recall provider and an env with a trace service and a
// captured log.
type memRig struct {
	*env
	agent *agent.Agent
	model *recallProvider
	logs  *bytes.Buffer
}

func newMemRig(t *testing.T, cfg memory.Config, opts ...agent.Option) *memRig {
	t.Helper()
	e := newEnv(t)
	model := &recallProvider{}
	if err := e.llm.Register(model); err != nil {
		t.Fatal(err)
	}
	p := testProfile("test-agent")
	p.Provider = "recall"
	p.Memory = cfg
	logs := &bytes.Buffer{}
	deps := e.deps()
	deps.Logger = slog.New(slog.NewTextHandler(logs, nil))
	a, err := agent.New(t.Context(), deps, p, opts...)
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	t.Cleanup(func() { _ = a.Dispose(context.WithoutCancel(t.Context())) })
	return &memRig{env: e, agent: a, model: model, logs: logs}
}

// seed saves one active user-preference memory of the agent agentID.
func (r *memRig) seed(t *testing.T, agentID, id, key, value string) {
	t.Helper()
	err := r.hub.Memory.Save(t.Context(), memory.Record{
		ID: id, AgentID: agentID, Kind: memory.KindUserPreference, Scope: memory.ScopeUser, Subject: "user",
		Key: key, Value: value, Status: memory.StatusActive, Confidence: memory.ConfidenceExplicit,
		Source: "conversation", SourceTrust: memory.TrustUserApproved, ObservedAt: kernel.MillisToISO(kernel.NowMillis()),
	})
	if err != nil {
		t.Fatalf("seed %s: %v", id, err)
	}
}

// systemOf returns the system messages joined by a newline.
func systemOf(messages []kernel.Message) string {
	var parts []string
	for _, m := range messages {
		if m.Role == kernel.RoleSystem {
			parts = append(parts, m.Content)
		}
	}
	return strings.Join(parts, "\n")
}

// toolResultOf is the first tool result in a model input.
func toolResultOf(messages []kernel.Message) *kernel.ToolResult {
	for _, m := range messages {
		if len(m.ToolResults) > 0 {
			return &m.ToolResults[0]
		}
	}
	return nil
}

func isErrorResult(r *kernel.ToolResult) bool { return r != nil && r.IsError != nil && *r.IsError }

// TestAgentMemoryRecall covers recall on demand through a real Agent and a real memory
// system: memory_search is offered while memory is on, nothing is injected into the
// system prompt, and a call returns this agent's matching memories.
func TestAgentMemoryRecall(t *testing.T) {
	t.Run("offers memory_search and injects nothing into the system prompt", func(t *testing.T) {
		r := newMemRig(t, recallMemory())
		r.seed(t, "test-agent", "m1", "response_language", "User prefers Vietnamese responses")
		begin(t, r.agent)
		chat(t, r.agent, "which language should responses use?")

		c := r.model.call(t, 0)
		if got := systemOf(c.messages); got != recallPrompt {
			t.Errorf("system message = %q, want only the prompt", got)
		}
		if !slices.Contains(c.tools, "memory_search") {
			t.Errorf("tools offered = %v, want memory_search", c.tools)
		}
	})

	t.Run("does not run a per-turn recall", func(t *testing.T) {
		r := newMemRig(t, recallMemory())
		r.seed(t, "test-agent", "m1", "response_language", "User prefers Vietnamese responses")
		spans := r.sinkOf()
		begin(t, r.agent)
		chat(t, r.agent, "first")
		chat(t, r.agent, "second")
		if got := spans.ofKind(trace.KindMemoryRecall); len(got) != 0 {
			t.Errorf("memory-recall spans = %d, want 0", len(got))
		}
	})

	t.Run("is not offered when memory is disabled or recall.maxResults is 0", func(t *testing.T) {
		for _, cfg := range []memory.Config{recallMemory(func(c *memory.Config) { c.Enabled = false }), recallMemory(maxResultsOf(0))} {
			r := newMemRig(t, cfg)
			begin(t, r.agent)
			chat(t, r.agent, "which language should responses use?")
			c := r.model.call(t, 0)
			if got := systemOf(c.messages); got != recallPrompt {
				t.Errorf("system message = %q, want only the prompt", got)
			}
			if slices.Contains(c.tools, "memory_search") {
				t.Errorf("tools offered = %v, want no memory_search", c.tools)
			}
		}
	})

	t.Run("a memory_search call returns this agent's matching memories, without approval", func(t *testing.T) {
		r := newMemRig(t, recallMemory())
		r.seed(t, "test-agent", "m1", "response_language", "User prefers Vietnamese responses")
		r.seed(t, "test-agent", "m2", "favorite_color", "Blue")
		r.seed(t, "other-agent", "foreign", "response_language", "User prefers French responses")
		begin(t, r.agent)
		res := chat(t, r.agent, "recall: response language")
		if res.Message.Content != "ok" {
			t.Fatalf("reply = %q, want ok", res.Message.Content)
		}
		out := toolResultOf(r.model.call(t, 1).messages)
		if out == nil || isErrorResult(out) {
			t.Fatalf("memory_search result = %+v, want a result", out)
		}
		for _, want := range []string{"## Recalled memories", "User prefers Vietnamese responses"} {
			if !strings.Contains(out.Content, want) {
				t.Errorf("result %q lacks %q", out.Content, want)
			}
		}
		for _, not := range []string{"Blue", "French"} {
			if strings.Contains(out.Content, not) {
				t.Errorf("result %q has %q", out.Content, not)
			}
		}
	})

	t.Run("caps the returned memories at recall.maxResults", func(t *testing.T) {
		r := newMemRig(t, recallMemory(maxResultsOf(2)))
		for i := range 5 {
			n := string(rune('0' + i))
			r.seed(t, "test-agent", "m"+n, "language_"+n, "language note "+n)
		}
		begin(t, r.agent)
		chat(t, r.agent, "recall: language")
		out := toolResultOf(r.model.call(t, 1).messages)
		lines := 0
		for _, l := range strings.Split(out.Content, "\n") {
			if strings.HasPrefix(l, "- [") {
				lines++
			}
		}
		if lines != 2 {
			t.Errorf("memory lines = %d, want 2", lines)
		}
	})

	t.Run("says so when nothing matches", func(t *testing.T) {
		r := newMemRig(t, recallMemory())
		begin(t, r.agent)
		chat(t, r.agent, "recall: deadline")
		if out := toolResultOf(r.model.call(t, 1).messages); out == nil || out.Content != `No memories match "deadline".` {
			t.Errorf("result = %+v, want the no-match text", out)
		}
	})

	t.Run("is traced as a tool dispatch", func(t *testing.T) {
		r := newMemRig(t, recallMemory())
		spans := r.sinkOf()
		begin(t, r.agent)
		chat(t, r.agent, "recall: language")
		found := false
		for _, sp := range spans.ofKind(trace.KindToolDispatch) {
			found = found || sp.Name == "memory_search"
		}
		if !found {
			t.Error("no tool-dispatch span named memory_search")
		}
	})
}

// failingQuery is a memory store whose Query fails, which is what the recall of a real
// memory system reads through.
type failingQuery struct {
	persistence.MemoryStore
}

func (failingQuery) Query(context.Context, persistence.MemoryFilter) ([]memory.Record, error) {
	return nil, errors.New("memory store offline")
}

// A memory_search whose recall fails inside the real memory system (its store is down)
// is an error result for the model, and the turn still completes.
func TestAgentMemoryRecall_StoreFailureIsLogged(t *testing.T) {
	e := newEnv(t)
	e.hub = persistence.NewPersistenceHub(persistence.WithMemory(failingQuery{persistence.NewMemoryMemoryStore()}))
	model := &recallProvider{}
	if err := e.llm.Register(model); err != nil {
		t.Fatal(err)
	}
	p := testProfile("test-agent")
	p.Provider = "recall"
	p.Memory = recallMemory()
	a, err := agent.New(t.Context(), e.deps(), p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Dispose(context.WithoutCancel(t.Context())) })

	begin(t, a)
	res := chat(t, a, "recall: language")
	if res.Message.Content != "ok" {
		t.Fatalf("reply = %q, want ok", res.Message.Content)
	}
	out := toolResultOf(model.call(t, -1).messages)
	if !isErrorResult(out) {
		t.Fatalf("memory_search result = %+v, want an error result", out)
	}
}
