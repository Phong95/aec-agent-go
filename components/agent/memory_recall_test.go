package agent_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/Phong95/aec-agent-go/aecmock"
	"github.com/Phong95/aec-agent-go/components/agent"
	"github.com/Phong95/aec-agent-go/components/agent/agentloop"
	"github.com/Phong95/aec-agent-go/components/agent/memorysystem/memory"
	"github.com/Phong95/aec-agent-go/components/persistence"
	"github.com/Phong95/aec-agent-go/components/trace"
	"github.com/Phong95/aec-agent-go/kernel"
)

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

// memRig is one agent whose mock model records every input it
// gets, over an env with a trace service and a captured log.
type memRig struct {
	*env
	agent *agent.Agent
	model *aecmock.MockProvider
	logs  *bytes.Buffer
}

func newMemRig(t *testing.T, cfg memory.Config, opts ...agent.Option) *memRig {
	t.Helper()
	e := newEnv(t)
	model := aecmock.NewMockProvider("mock", func([]kernel.Message) string { return "ok" })
	if err := e.llm.Register(model); err != nil {
		t.Fatal(err)
	}
	p := testProfile("test-agent")
	p.Provider = "mock"
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

// modelInput is the messages of the n-th model call.
func (r *memRig) modelInput(t *testing.T, n int) []kernel.Message {
	t.Helper()
	calls := r.model.Calls()
	if len(calls) <= n {
		t.Fatalf("the model got %d calls, want at least %d", len(calls), n+1)
	}
	return calls[n].Messages
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

func countSystem(messages []kernel.Message) int {
	n := 0
	for _, m := range messages {
		if m.Role == kernel.RoleSystem {
			n++
		}
	}
	return n
}

// recallFunc is a memory port made of a function, for the test hook that replaces the
// memory system of the loop.
type recallFunc func(ctx context.Context, userMessage, sessionID string) (memory.RecalledMemories, error)

func (f recallFunc) RecallForTurn(ctx context.Context, userMessage, sessionID string) (memory.RecalledMemories, error) {
	return f(ctx, userMessage, sessionID)
}

// spanAttrs is the end attributes of a span as decoded JSON, so numbers compare as
// float64 whatever Go type the loop stored them in.
func spanAttrs(t *testing.T, sp trace.Span) map[string]any {
	t.Helper()
	raw, err := json.Marshal(sp.Attributes)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestAgentMemoryRecall covers the agent memory recall cases: the memories recalled for a
// turn reach the model through a real Agent and a real memory system, in Chat and in
// ChatStream, are never persisted in the history, and are traced on the memory-recall
// span. The 8 cases of "memory recall in the agent loop" come first, then the 3 of
// "memory-recall trace span".
func TestAgentMemoryRecall(t *testing.T) {
	const language = "User prefers Vietnamese responses"
	const question = "which language should responses use?"

	t.Run("injects relevant memories into the system prompt (index built lazily, no consolidation needed)", func(t *testing.T) {
		r := newMemRig(t, recallMemory())
		r.seed(t, "test-agent", "m1", "response_language", language)
		r.seed(t, "test-agent", "m2", "favorite_color", "Blue")
		begin(t, r.agent)
		chat(t, r.agent, question)

		input := r.modelInput(t, 0)
		system := systemOf(input)
		if !strings.HasPrefix(system, recallPrompt) {
			t.Errorf("system message does not start with the prompt:\n%s", system)
		}
		if !strings.Contains(system, "## Recalled memories") || !strings.Contains(system, language) {
			t.Errorf("system message lacks the recalled memory:\n%s", system)
		}
		if strings.Contains(system, "Blue") {
			t.Errorf("system message holds a memory that does not match the message:\n%s", system)
		}
		// Still a single system message: Anthropic keeps only the last one.
		if n := countSystem(input); n != 1 {
			t.Errorf("system messages = %d, want 1", n)
		}
	})

	t.Run("also injects on the streaming path", func(t *testing.T) {
		r := newMemRig(t, recallMemory())
		r.seed(t, "test-agent", "m1", "response_language", language)
		begin(t, r.agent)
		if _, err := drain(t, r.agent.ChatStream(t.Context(), "which language for responses?")); err != nil {
			t.Fatalf("ChatStream: %v", err)
		}
		if system := systemOf(r.modelInput(t, 0)); !strings.Contains(system, language) {
			t.Errorf("the streamed turn's system message lacks the recalled memory:\n%s", system)
		}
	})

	t.Run("never persists recalled memories into the conversation history", func(t *testing.T) {
		r := newMemRig(t, recallMemory())
		r.seed(t, "test-agent", "m1", "response_language", language)
		session, err := r.agent.StartSession(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		chat(t, r.agent, question)
		if !strings.Contains(systemOf(r.modelInput(t, 0)), "Recalled memories") {
			t.Fatal("the model input carries no recalled memories, so the check below proves nothing")
		}
		stored, err := r.hub.Messages.Get(t.Context(), session.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range stored {
			if strings.Contains(m.Content, "Recalled memories") {
				t.Errorf("a recalled memory was persisted: %+v", m)
			}
		}
	})

	t.Run("recalls once per turn, keyed on the user's message", func(t *testing.T) {
		type recallCall struct{ message, session string }
		var mu sync.Mutex
		var calls []recallCall
		spy := agent.WithMemoryWrap(func(inner agentloop.Memory) agentloop.Memory {
			return recallFunc(func(ctx context.Context, userMessage, sessionID string) (memory.RecalledMemories, error) {
				mu.Lock()
				calls = append(calls, recallCall{userMessage, sessionID})
				mu.Unlock()
				return inner.RecallForTurn(ctx, userMessage, sessionID)
			})
		})
		r := newMemRig(t, recallMemory(), spy)
		session, err := r.agent.StartSession(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		chat(t, r.agent, "first")
		chat(t, r.agent, "second")

		mu.Lock()
		defer mu.Unlock()
		want := []recallCall{{"first", session.ID}, {"second", session.ID}}
		if len(calls) != 2 || calls[0] != want[0] || calls[1] != want[1] {
			t.Fatalf("recalls = %+v, want %+v", calls, want)
		}
	})

	t.Run("does not inject another agent's memories", func(t *testing.T) {
		r := newMemRig(t, recallMemory())
		r.seed(t, "other-agent", "foreign", "response_language", language)
		begin(t, r.agent)
		chat(t, r.agent, question)
		if system := systemOf(r.modelInput(t, 0)); strings.Contains(system, "Recalled memories") {
			t.Errorf("another agent's memory was injected:\n%s", system)
		}
	})

	t.Run("injects nothing when memory is disabled or recall.maxResults is 0", func(t *testing.T) {
		disabled := func(c *memory.Config) { c.Enabled = false }
		for name, cfg := range map[string]memory.Config{
			"disabled":        recallMemory(disabled),
			"maxResults is 0": recallMemory(maxResultsOf(0)),
		} {
			r := newMemRig(t, cfg)
			r.seed(t, "test-agent", "m1", "response_language", language)
			begin(t, r.agent)
			chat(t, r.agent, question)
			if system := systemOf(r.modelInput(t, 0)); system != recallPrompt {
				t.Errorf("%s: system message = %q, want only the prompt", name, system)
			}
		}
	})

	t.Run("caps injected memories at recall.maxResults", func(t *testing.T) {
		r := newMemRig(t, recallMemory(maxResultsOf(2)))
		for i := range 5 {
			r.seed(t, "test-agent", "m"+string(rune('0'+i)), "language_"+string(rune('0'+i)), "language note "+string(rune('0'+i)))
		}
		begin(t, r.agent)
		chat(t, r.agent, "language")

		var lines int
		for line := range strings.SplitSeq(systemOf(r.modelInput(t, 0)), "\n") {
			if strings.HasPrefix(line, "- [") {
				lines++
			}
		}
		if lines != 2 {
			t.Fatalf("injected memory lines = %d, want 2", lines)
		}
	})

	t.Run("a failing recall is logged and the turn still completes without memories", func(t *testing.T) {
		boom := agent.WithMemoryWrap(func(agentloop.Memory) agentloop.Memory {
			return recallFunc(func(context.Context, string, string) (memory.RecalledMemories, error) {
				return memory.RecalledMemories{}, errors.New("index exploded")
			})
		})
		r := newMemRig(t, recallMemory(), boom)
		begin(t, r.agent)
		res := chat(t, r.agent, "hello")

		if !strings.Contains(res.Message.Content, "ok") {
			t.Errorf("reply = %q, want the model's ok", res.Message.Content)
		}
		if system := systemOf(r.modelInput(t, 0)); system != recallPrompt {
			t.Errorf("system message = %q, want only the prompt", system)
		}
		logged := r.logs.String()
		if !strings.Contains(logged, "memory recall failed") || !strings.Contains(logged, "index exploded") {
			t.Errorf("the failure was not logged:\n%s", logged)
		}
	})

	t.Run("records hits, scores, injected size and the cold index rebuild", func(t *testing.T) {
		r := newMemRig(t, recallMemory())
		sink := r.sinkOf()
		r.seed(t, "test-agent", "m1", "response_language", language)
		r.seed(t, "test-agent", "m2", "favorite_color", "Blue")
		begin(t, r.agent)
		chat(t, r.agent, question)
		chat(t, r.agent, "language again")

		spans := sink.ofKind(trace.KindMemoryRecall)
		if len(spans) != 2 {
			t.Fatalf("memory-recall spans = %d, want 2", len(spans))
		}
		first, second := spans[0], spans[1]
		if first.Status != trace.StatusOK {
			t.Errorf("status = %q, want ok", first.Status)
		}
		if first.ParentSpanID == "" {
			t.Error("the memory-recall span has no parent")
		}
		attrs := spanAttrs(t, first)
		for key, want := range map[string]any{
			"hit": true, "recalledCount": 1.0, "maxResults": 8.0, "indexRebuilt": true, "indexedCount": 2.0, "queryPreview": question,
		} {
			if attrs[key] != want {
				t.Errorf("attribute %s = %v, want %v", key, attrs[key], want)
			}
		}
		for _, key := range []string{"injectedChars", "injectedTokens", "topScore"} {
			if v, _ := attrs[key].(float64); v <= 0 {
				t.Errorf("attribute %s = %v, want > 0", key, attrs[key])
			}
		}
		memories, _ := attrs["memories"].([]any)
		if len(memories) != 1 {
			t.Fatalf("memories = %v, want one hit", attrs["memories"])
		}
		hit, _ := memories[0].(map[string]any)
		if hit["id"] != "m1" || hit["kind"] != "user_preference" || hit["key"] != "response_language" {
			t.Errorf("memory trace = %v, want m1 / user_preference / response_language", hit)
		}
		// The trace carries ids and scores only, never the memory value.
		raw, _ := json.Marshal(attrs["memories"])
		if strings.Contains(string(raw), "Vietnamese") {
			t.Errorf("the memory trace carries the memory value: %s", raw)
		}
		// The index is warm on the next turn.
		if got := spanAttrs(t, second)["indexRebuilt"]; got != false {
			t.Errorf("second indexRebuilt = %v, want false", got)
		}
	})

	t.Run("records a miss and a skipped recall", func(t *testing.T) {
		miss := newMemRig(t, recallMemory())
		missSink := miss.sinkOf()
		begin(t, miss.agent)
		chat(t, miss.agent, "hello")
		missSpans := missSink.ofKind(trace.KindMemoryRecall)
		if len(missSpans) == 0 {
			t.Fatal("no memory-recall span for the miss")
		}
		attrs := spanAttrs(t, missSpans[0])
		if attrs["hit"] != false || attrs["recalledCount"] != 0.0 || attrs["injectedChars"] != 0.0 {
			t.Errorf("miss attributes = %v, want hit false, recalledCount 0, injectedChars 0", attrs)
		}

		off := newMemRig(t, recallMemory(maxResultsOf(0)))
		offSink := off.sinkOf()
		begin(t, off.agent)
		chat(t, off.agent, "hello")
		offSpans := offSink.ofKind(trace.KindMemoryRecall)
		if len(offSpans) == 0 {
			t.Fatal("no memory-recall span for the skipped recall")
		}
		attrs = spanAttrs(t, offSpans[0])
		if attrs["skipped"] != "recall-disabled" || attrs["recalledCount"] != 0.0 {
			t.Errorf("skipped attributes = %v, want skipped recall-disabled and recalledCount 0", attrs)
		}
	})

	t.Run("marks the span as error when recall fails", func(t *testing.T) {
		boom := agent.WithMemoryWrap(func(agentloop.Memory) agentloop.Memory {
			return recallFunc(func(context.Context, string, string) (memory.RecalledMemories, error) {
				return memory.RecalledMemories{}, errors.New("index exploded")
			})
		})
		r := newMemRig(t, recallMemory(), boom)
		sink := r.sinkOf()
		begin(t, r.agent)
		chat(t, r.agent, "hello")

		spans := sink.ofKind(trace.KindMemoryRecall)
		if len(spans) == 0 {
			t.Fatal("no memory-recall span")
		}
		if spans[0].Status != trace.StatusError {
			t.Errorf("status = %q, want error", spans[0].Status)
		}
		if got := spanAttrs(t, spans[0])["error"]; got != "index exploded" {
			t.Errorf("error attribute = %v, want index exploded", got)
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

// A recall that fails inside the real memory system (its store is down) is logged, the
// span ends with the error and the turn completes without memories, exactly as when
// the port itself fails.
func TestAgentMemoryRecall_StoreFailureIsLogged(t *testing.T) {
	e := newEnv(t)
	e.hub = persistence.NewPersistenceHub(persistence.WithMemory(failingQuery{persistence.NewMemoryMemoryStore()}))
	model := aecmock.NewMockProvider("mock", func([]kernel.Message) string { return "ok" })
	if err := e.llm.Register(model); err != nil {
		t.Fatal(err)
	}
	sink := e.sinkOf()
	p := testProfile("test-agent")
	p.Provider = "mock"
	p.Memory = recallMemory()
	logs := &bytes.Buffer{}
	deps := e.deps()
	deps.Logger = slog.New(slog.NewTextHandler(logs, nil))
	a, err := agent.New(t.Context(), deps, p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Dispose(context.WithoutCancel(t.Context())) })

	begin(t, a)
	res := chat(t, a, "hello")
	if res.Message.Content != "ok" {
		t.Fatalf("reply = %q, want ok", res.Message.Content)
	}
	if system := systemOf(model.Calls()[0].Messages); system != recallPrompt {
		t.Errorf("system message = %q, want only the prompt", system)
	}
	if !strings.Contains(logs.String(), "memory store offline") {
		t.Errorf("the store failure was not logged:\n%s", logs.String())
	}
	spans := sink.ofKind(trace.KindMemoryRecall)
	if len(spans) != 1 || spans[0].Status != trace.StatusError {
		t.Fatalf("memory-recall spans = %+v, want one with the error status", spans)
	}
}
