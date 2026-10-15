package agent_test

import (
	"context"
	"errors"
	"iter"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/Phong95/aec-agent-go/aecmock"
	"github.com/Phong95/aec-agent-go/components/agent"
	"github.com/Phong95/aec-agent-go/components/agent/agentloop"
	"github.com/Phong95/aec-agent-go/components/agent/profile"
	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/kernel"
)

// ---------------------------------------------------------------------------
// A-97: one agent runs turns in several sessions at once; turns of the same
// session stay one at a time, in arrival order.
// ---------------------------------------------------------------------------

// lastUserText is the content of the last user message of msgs.
func lastUserText(msgs []kernel.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == kernel.RoleUser {
			return msgs[i].Content
		}
	}
	return ""
}

// gatedModel is a provider whose every model call waits until the test releases it
// (by the user message it answers), then replies "reply to <message>". inFlight lists
// the calls waiting now, so a test can see which turns run at the same time.
type gatedModel struct {
	*aecmock.MockProvider
	mu      sync.Mutex
	pending map[string]chan struct{}
	seen    map[string][]kernel.Message
	calls   atomic.Int32
}

func newGatedModel() *gatedModel {
	return &gatedModel{
		MockProvider: aecmock.NewMockProvider("gated", func(msgs []kernel.Message) string { return "reply to " + lastUserText(msgs) }),
		pending:      map[string]chan struct{}{},
		seen:         map[string][]kernel.Message{},
	}
}

func (g *gatedModel) wait(ctx context.Context, msgs []kernel.Message) error {
	g.calls.Add(1)
	text := lastUserText(msgs)
	ch := make(chan struct{})
	g.mu.Lock()
	g.pending[text] = ch
	g.seen[text] = slices.Clone(msgs)
	g.mu.Unlock()
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		g.mu.Lock()
		delete(g.pending, text)
		g.mu.Unlock()
		return ctx.Err()
	}
}

func (g *gatedModel) release(t *testing.T, text string) {
	t.Helper()
	g.mu.Lock()
	ch, ok := g.pending[text]
	delete(g.pending, text)
	g.mu.Unlock()
	if !ok {
		t.Fatalf("no pending model call for %q", text)
	}
	close(ch)
}

func (g *gatedModel) inFlight() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]string, 0, len(g.pending))
	for text := range g.pending {
		out = append(out, text)
	}
	sort.Strings(out)
	return out
}

func (g *gatedModel) Chat(ctx context.Context, msgs []kernel.Message, opts llmhub.ModelOptions) (kernel.ModelResponse, error) {
	if err := g.wait(ctx, msgs); err != nil {
		return kernel.ModelResponse{}, err
	}
	return g.MockProvider.Chat(ctx, msgs, opts)
}

func (g *gatedModel) Stream(ctx context.Context, msgs []kernel.Message, opts llmhub.ModelOptions) iter.Seq2[kernel.StreamChunk, error] {
	return func(yield func(kernel.StreamChunk, error) bool) {
		if err := g.wait(ctx, msgs); err != nil {
			yield(kernel.StreamChunk{}, err)
			return
		}
		g.MockProvider.Stream(ctx, msgs, opts)(yield)
	}
}

// gatedAgent is an agent over a gatedModel; mutate changes its profile.
func gatedAgent(t *testing.T, mutate func(*profile.AgentProfile)) (*agent.Agent, *gatedModel, *env) {
	t.Helper()
	e := newEnv(t)
	e.llm = llmhub.NewLLMHub()
	model := newGatedModel()
	if err := e.llm.Register(model); err != nil {
		t.Fatal(err)
	}
	p := testProfile("a1")
	if mutate != nil {
		mutate(&p)
	}
	a := e.spawn(t, p)
	return a, model, e
}

func startSession(t *testing.T, a *agent.Agent) string {
	t.Helper()
	s, err := a.StartSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return s.ID
}

func goChatIn(ctx context.Context, a *agent.Agent, msg, sessionID string) chan chatOutcome {
	out := make(chan chatOutcome, 1)
	go func() {
		res, err := a.Chat(ctx, msg, agent.WithChatSession(sessionID))
		out <- chatOutcome{res, err}
	}()
	return out
}

// history renders a session's stored messages as "role: content".
func history(t *testing.T, e *env, sessionID string) []string {
	t.Helper()
	msgs, err := e.hub.Messages.Get(context.WithoutCancel(t.Context()), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, len(msgs))
	for i, m := range msgs {
		out[i] = string(m.Role) + ": " + m.Content
	}
	return out
}

func finished(t *testing.T, ch chan chatOutcome) agentloop.RunResult {
	t.Helper()
	o := <-ch
	if o.err != nil || o.res.Aborted {
		t.Fatalf("chat = %+v, %v, want a finished turn", o.res, o.err)
	}
	return o.res
}

func TestConcurrentSessions_DifferentSessionsRunAtOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a, model, e := gatedAgent(t, nil)
		sa, sb := startSession(t, a), startSession(t, a)

		ra := goChatIn(t.Context(), a, "from A", sa)
		rb := goChatIn(t.Context(), a, "from B", sb)
		synctest.Wait()
		if got := model.inFlight(); !slices.Equal(got, []string{"from A", "from B"}) {
			t.Fatalf("in flight = %v, want both turns", got)
		}
		if n := a.RunningTurns(); n != 2 {
			t.Fatalf("RunningTurns = %d, want 2", n)
		}

		model.release(t, "from B")
		if got := finished(t, rb).Message.Content; got != "reply to from B" {
			t.Fatalf("B = %q", got)
		}
		model.release(t, "from A")
		if got := finished(t, ra).Message.Content; got != "reply to from A" {
			t.Fatalf("A = %q", got)
		}
		if got := history(t, e, sa); !slices.Equal(got, []string{"user: from A", "assistant: reply to from A"}) {
			t.Fatalf("history A = %v", got)
		}
		if got := history(t, e, sb); !slices.Equal(got, []string{"user: from B", "assistant: reply to from B"}) {
			t.Fatalf("history B = %v", got)
		}
		synctest.Wait()
		if n := a.RunningTurns(); n != 0 {
			t.Fatalf("RunningTurns after both = %d, want 0", n)
		}
	})
}

func TestConcurrentSessions_SameSessionInArrivalOrder(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a, model, e := gatedAgent(t, nil)
		s := startSession(t, a)

		r1 := goChatIn(t.Context(), a, "first", s)
		synctest.Wait()
		r2 := goChatIn(t.Context(), a, "second", s)
		synctest.Wait()
		r3 := goChatIn(t.Context(), a, "third", s)
		synctest.Wait()
		if got := model.inFlight(); !slices.Equal(got, []string{"first"}) {
			t.Fatalf("in flight = %v, want only the first turn", got)
		}

		model.release(t, "first")
		finished(t, r1)
		synctest.Wait()
		if got := model.inFlight(); !slices.Equal(got, []string{"second"}) {
			t.Fatalf("in flight = %v, want the second turn next", got)
		}
		// The second turn sees the first one's whole exchange.
		model.mu.Lock()
		seen := model.seen["second"]
		model.mu.Unlock()
		var texts []string
		for _, m := range seen {
			if m.Role != kernel.RoleSystem {
				texts = append(texts, m.Content)
			}
		}
		if !slices.Equal(texts, []string{"first", "reply to first", "second"}) {
			t.Fatalf("the second turn saw %v", texts)
		}
		model.release(t, "second")
		finished(t, r2)
		synctest.Wait()
		model.release(t, "third")
		finished(t, r3)

		want := []string{
			"user: first", "assistant: reply to first",
			"user: second", "assistant: reply to second",
			"user: third", "assistant: reply to third",
		}
		if got := history(t, e, s); !slices.Equal(got, want) {
			t.Fatalf("history = %v, want %v", got, want)
		}
	})
}

func TestConcurrentSessions_StreamsRunAtOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a, model, _ := gatedAgent(t, nil)
		sa, sb := startSession(t, a), startSession(t, a)

		collect := func(text, sessionID string) chan string {
			out := make(chan string, 1)
			go func() {
				var b strings.Builder
				for chunk, err := range a.ChatStream(t.Context(), text, agent.WithChatSession(sessionID)) {
					if err != nil {
						t.Errorf("stream %s: %v", text, err)
						break
					}
					if chunk.Type == kernel.ChunkText && chunk.Content != nil {
						b.WriteString(*chunk.Content)
					}
				}
				out <- b.String()
			}()
			return out
		}
		ca, cb := collect("A", sa), collect("B", sb)
		synctest.Wait()
		if got := model.inFlight(); !slices.Equal(got, []string{"A", "B"}) {
			t.Fatalf("in flight = %v, want both streams", got)
		}
		model.release(t, "A")
		model.release(t, "B")
		if got := strings.TrimSpace(<-ca); got != "reply to A" {
			t.Fatalf("stream A = %q", got)
		}
		if got := strings.TrimSpace(<-cb); got != "reply to B" {
			t.Fatalf("stream B = %q", got)
		}
	})
}

// loopConfig.maxConcurrentTurns caps the turns across sessions; it is read on every
// turn, so raising it at runtime lets the waiting turns in at the next release.
func TestConcurrentSessions_MaxConcurrentTurns(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a, model, _ := gatedAgent(t, func(p *profile.AgentProfile) {
			p.LoopConfig = &profile.LoopConfig{MaxConcurrentTurns: intPtr(1)}
		})
		sa, sb, sc := startSession(t, a), startSession(t, a), startSession(t, a)

		ra := goChatIn(t.Context(), a, "A", sa)
		synctest.Wait()
		rb := goChatIn(t.Context(), a, "B", sb)
		rc := goChatIn(t.Context(), a, "C", sc)
		synctest.Wait()
		if got := model.inFlight(); !slices.Equal(got, []string{"A"}) {
			t.Fatalf("in flight = %v, want only A under a cap of 1", got)
		}
		if n := a.RunningTurns(); n != 1 {
			t.Fatalf("RunningTurns = %d, want 1 (waiting turns are not counted)", n)
		}

		// No cap from now on: both waiting turns start when A ends.
		a.UpdateLoopConfig(nil)
		model.release(t, "A")
		finished(t, ra)
		synctest.Wait()
		if got := model.inFlight(); !slices.Equal(got, []string{"B", "C"}) {
			t.Fatalf("in flight = %v, want B and C once the cap is gone", got)
		}
		model.release(t, "B")
		model.release(t, "C")
		finished(t, rb)
		finished(t, rc)
	})
}

// A turn whose ctx ends while it waits for its session returns the ctx error and runs
// and saves nothing (the Go form of the A-97 "aborted before start" result); the stream
// form yields that error as its one pair.
func TestConcurrentSessions_CancelWhileWaiting(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a, model, e := gatedAgent(t, nil)
		s := startSession(t, a)

		r1 := goChatIn(t.Context(), a, "first", s)
		synctest.Wait()

		ctx, cancel := context.WithCancel(t.Context())
		r2 := goChatIn(ctx, a, "second", s)
		synctest.Wait()
		cancel()
		if o := <-r2; !errors.Is(o.err, context.Canceled) || o.res.Iterations != 0 {
			t.Fatalf("waiting chat = %+v, %v, want context.Canceled and nothing run", o.res, o.err)
		}
		var pairs int
		for chunk, err := range a.ChatStream(ctx, "third", agent.WithChatSession(s)) {
			pairs++
			if !errors.Is(err, context.Canceled) || chunk.Type != "" {
				t.Errorf("waiting stream = %+v, %v, want one zero chunk with context.Canceled", chunk, err)
			}
		}
		if pairs != 1 {
			t.Errorf("waiting stream yielded %d pairs, want 1", pairs)
		}

		model.release(t, "first")
		finished(t, r1)
		if n := model.calls.Load(); n != 1 {
			t.Fatalf("model calls = %d, want 1", n)
		}
		if got := history(t, e, s); !slices.Equal(got, []string{"user: first", "assistant: reply to first"}) {
			t.Fatalf("history = %v, want only the first turn", got)
		}
	})
}

// Dispose cancels the running turns of every session (Aborted), releases the waiting
// ones with ErrAgentDisposed, waits for the running ones, and refuses new turns.
func TestConcurrentSessions_DisposeStopsEverySession(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a, _, _ := gatedAgent(t, nil)
		sa, sb := startSession(t, a), startSession(t, a)

		ra := goChatIn(t.Context(), a, "A", sa)
		rb := goChatIn(t.Context(), a, "B", sb)
		synctest.Wait()
		waiting := goChatIn(t.Context(), a, "A again", sa)
		synctest.Wait()

		if err := a.Dispose(t.Context()); err != nil {
			t.Fatalf("Dispose = %v", err)
		}
		synctest.Wait()
		for name, ch := range map[string]chan chatOutcome{"A": ra, "B": rb} {
			select {
			case o := <-ch:
				if o.err != nil || !o.res.Aborted {
					t.Errorf("running turn %s = %+v, %v, want Aborted", name, o.res, o.err)
				}
			default:
				t.Fatalf("Dispose returned before turn %s ended", name)
			}
		}
		if o := <-waiting; !errors.Is(o.err, agent.ErrAgentDisposed) {
			t.Errorf("waiting turn = %v, want ErrAgentDisposed", o.err)
		}
		if n := a.RunningTurns(); n != 0 {
			t.Errorf("RunningTurns after Dispose = %d, want 0", n)
		}
		if _, err := a.Chat(t.Context(), "late", agent.WithChatSession(sa)); !errors.Is(err, agent.ErrAgentDisposed) {
			t.Errorf("Chat after Dispose = %v, want ErrAgentDisposed", err)
		}
	})
}

// A Chat without WithChatSession still runs in the StartSession/LoadSession default.
func TestConcurrentSessions_DefaultSession(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a, model, e := gatedAgent(t, nil)
		sa := startSession(t, a)
		sb := startSession(t, a) // sb is now the default
		if _, ok, err := a.LoadSession(t.Context(), sa); !ok || err != nil {
			t.Fatalf("LoadSession = %v, %v", ok, err)
		}
		r := goChat(t.Context(), a, "hello")
		synctest.Wait()
		model.release(t, "hello")
		finished(t, r)
		if got := history(t, e, sa); len(got) != 2 {
			t.Fatalf("default session history = %v, want the turn", got)
		}
		if got := history(t, e, sb); len(got) != 0 {
			t.Fatalf("other session history = %v, want nothing", got)
		}
	})
}

// A turn in a session that does not exist fails before anything runs or is written.
func TestConcurrentSessions_MissingSession(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a, model, e := gatedAgent(t, nil)
		_, err := a.Chat(t.Context(), "hi", agent.WithChatSession("missing"))
		if !errors.Is(err, agent.ErrSessionNotFound) || !strings.Contains(err.Error(), `"missing"`) {
			t.Fatalf("Chat in a missing session = %v, want ErrSessionNotFound naming it", err)
		}
		for _, err := range a.ChatStream(t.Context(), "hi", agent.WithChatSession("missing")) {
			if !errors.Is(err, agent.ErrSessionNotFound) {
				t.Fatalf("ChatStream in a missing session = %v, want ErrSessionNotFound", err)
			}
		}
		if n := model.calls.Load(); n != 0 {
			t.Fatalf("model calls = %d, want 0", n)
		}
		if n := a.RunningTurns(); n != 0 {
			t.Fatalf("RunningTurns = %d, want 0", n)
		}
		if got := history(t, e, "missing"); len(got) != 0 {
			t.Fatalf("history of the missing session = %v, want nothing written", got)
		}
	})
}

// ---------------------------------------------------------------------------
// Nested chats from inside a turn (agent-to-agent groundwork, A-17)
// ---------------------------------------------------------------------------

// nestTool chats with the agent from inside a turn (run) and returns the reply, or the
// error as a failed, non-retryable result.
type nestTool struct {
	run func(ctx context.Context) (string, error)
}

func (nestTool) Name() string    { return "tool:nest" }
func (nestTool) Version() string { return "1.0.0" }
func (nestTool) Definition() toolhub.Definition {
	return toolhub.Definition{Name: "nest", Description: "Chats with the agent from inside a turn.", Parameters: toolhub.JSONSchema{Type: "object"}, Risk: toolhub.RiskReadOnly}
}

func (n nestTool) Execute(ctx context.Context, _ kernel.Arguments, _ toolhub.Context) (toolhub.Output, error) {
	out, err := n.run(ctx)
	if err != nil {
		// A failed nested chat is the tool's failed result, not a tool error.
		return toolhub.Output{Content: err.Error(), IsError: boolPtr(true), Retryable: boolPtr(false)}, nil //nolint:nilerr // reported to the model as a failed result
	}
	return toolhub.Output{Content: out}, nil
}

// nestingAgent is an agent whose model calls nest for "outer" (until a tool result came
// back) and ends every other turn with "done <message>". maxConcurrentTurns is the cap.
func nestingAgent(t *testing.T, maxConcurrentTurns int, run func(a *agent.Agent) func(ctx context.Context) (string, error)) (*agent.Agent, *env) {
	t.Helper()
	e := newEnv(t)
	e.llm = llmhub.NewLLMHub()
	args, err := kernel.NewArguments([]byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	model := aecmock.NewMockProvider("nesting", nil, aecmock.WithResponse(func(msgs []kernel.Message) kernel.ModelResponse {
		usage := kernel.TokenUsage{InputTokens: 10, OutputTokens: 5}
		text := lastUserText(msgs)
		if text == "outer" && len(msgs[len(msgs)-1].ToolResults) == 0 {
			return kernel.ModelResponse{
				Message:    kernel.Message{Role: kernel.RoleAssistant, ToolCalls: []kernel.ToolCall{{ID: "call-1", Name: "nest", Arguments: args}}},
				Usage:      usage,
				StopReason: kernel.StopToolUse,
			}
		}
		return kernel.ModelResponse{Message: kernel.Message{Role: kernel.RoleAssistant, Content: "done " + text}, Usage: usage, StopReason: kernel.StopEndTurn}
	}))
	if err := e.llm.Register(model); err != nil {
		t.Fatal(err)
	}
	var a *agent.Agent
	if err := e.tools.Register(nestTool{run: func(ctx context.Context) (string, error) { return run(a)(ctx) }}); err != nil {
		t.Fatal(err)
	}
	p := testProfile("a1")
	p.Tools = []string{"nest"}
	p.LoopConfig = &profile.LoopConfig{MaxToolRetries: intPtr(0), MaxConcurrentTurns: intPtr(maxConcurrentTurns)}
	a = e.spawn(t, p)
	return a, e
}

// A nested chat in another session runs while the outer turn waits for it, also under
// a cap of 1 (the nested turn does not wait for a slot the outer turn holds).
func TestConcurrentSessions_NestedChatInAnotherSession(t *testing.T) {
	for _, maxTurns := range []int{0, 1} {
		var other string
		a, e := nestingAgent(t, maxTurns, func(a *agent.Agent) func(ctx context.Context) (string, error) {
			return func(ctx context.Context) (string, error) {
				res, err := a.Chat(ctx, "inner", agent.WithChatSession(other))
				return res.Message.Content, err
			}
		})
		outer := startSession(t, a)
		other = startSession(t, a)
		res, err := a.Chat(t.Context(), "outer", agent.WithChatSession(outer))
		if err != nil || res.Message.Content != "done outer" {
			t.Fatalf("cap %d: outer chat = %+v, %v", maxTurns, res, err)
		}
		if got := history(t, e, other); !slices.Equal(got, []string{"user: inner", "assistant: done inner"}) {
			t.Fatalf("cap %d: other session = %v, want the nested turn", maxTurns, got)
		}
		msgs, _ := e.hub.Messages.Get(t.Context(), outer)
		if i := slices.IndexFunc(msgs, func(m kernel.Message) bool { return len(m.ToolResults) > 0 }); i < 0 || msgs[i].ToolResults[0].Content != "done inner" {
			t.Fatalf("cap %d: outer session = %+v, want the nested reply as the tool result", maxTurns, msgs)
		}
	}
}

// A nested chat in the same session is refused with ErrReentrantChat instead of waiting
// for itself; the outer turn finishes.
func TestConcurrentSessions_NestedChatInSameSession(t *testing.T) {
	var session string
	var nested error
	a, e := nestingAgent(t, 0, func(a *agent.Agent) func(ctx context.Context) (string, error) {
		return func(ctx context.Context) (string, error) {
			res, err := a.Chat(ctx, "inner", agent.WithChatSession(session))
			nested = err
			return res.Message.Content, err
		}
	})
	session = startSession(t, a)
	res, err := a.Chat(t.Context(), "outer", agent.WithChatSession(session))
	if err != nil || res.Message.Content != "done outer" {
		t.Fatalf("outer chat = %+v, %v", res, err)
	}
	if !errors.Is(nested, agent.ErrReentrantChat) {
		t.Fatalf("nested chat in the same session = %v, want ErrReentrantChat", nested)
	}
	msgs, _ := e.hub.Messages.Get(t.Context(), session)
	i := slices.IndexFunc(msgs, func(m kernel.Message) bool { return len(m.ToolResults) > 0 })
	if i < 0 || msgs[i].ToolResults[0].IsError == nil || !*msgs[i].ToolResults[0].IsError {
		t.Fatalf("session = %+v, want the refusal as a failed tool result", msgs)
	}
}
