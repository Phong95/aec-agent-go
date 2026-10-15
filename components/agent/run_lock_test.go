package agent_test

import (
	"context"
	"errors"
	"iter"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Phong95/aec-agent-go/aecmock"
	"github.com/Phong95/aec-agent-go/components/agent"
	"github.com/Phong95/aec-agent-go/components/agent/agentloop"
	"github.com/Phong95/aec-agent-go/components/agent/profile"
	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/kernel"
)

// heldModel is a provider whose model calls block until the gate opens or their ctx
// ends. It counts the calls in flight and their peak, so a test can see whether two
// chats overlapped.
type heldModel struct {
	*aecmock.MockProvider
	gate    chan struct{}
	entered chan struct{} // receives one value per call that starts
	live    atomic.Int32
	peak    atomic.Int32
}

func newHeldModel() *heldModel {
	return &heldModel{
		MockProvider: aecmock.NewMockProvider("held", func([]kernel.Message) string { return "done" }),
		gate:         make(chan struct{}),
		entered:      make(chan struct{}, 16),
	}
}

func (h *heldModel) hold(ctx context.Context) error {
	n := h.live.Add(1)
	defer h.live.Add(-1)
	for {
		p := h.peak.Load()
		if n <= p || h.peak.CompareAndSwap(p, n) {
			break
		}
	}
	h.entered <- struct{}{}
	select {
	case <-h.gate:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (h *heldModel) Chat(ctx context.Context, msgs []kernel.Message, opts llmhub.ModelOptions) (kernel.ModelResponse, error) {
	if err := h.hold(ctx); err != nil {
		return kernel.ModelResponse{}, err
	}
	return h.MockProvider.Chat(ctx, msgs, opts)
}

func (h *heldModel) Stream(ctx context.Context, msgs []kernel.Message, opts llmhub.ModelOptions) iter.Seq2[kernel.StreamChunk, error] {
	return func(yield func(kernel.StreamChunk, error) bool) {
		if err := h.hold(ctx); err != nil {
			yield(kernel.StreamChunk{}, err)
			return
		}
		h.MockProvider.Stream(ctx, msgs, opts)(yield)
	}
}

// heldAgent is an agent over a heldModel with an active session.
// (A-97: every chat of these tests runs in that one default session, so the
// one-turn-at-a-time rule they check is the per-session rule; turns of different
// sessions run in parallel, see concurrent_sessions_test.go.)
func heldAgent(t *testing.T) (*agent.Agent, *heldModel) {
	t.Helper()
	e := newEnv(t)
	e.llm = llmhub.NewLLMHub()
	model := newHeldModel()
	if err := e.llm.Register(model); err != nil {
		t.Fatal(err)
	}
	a := e.spawn(t, testProfile("a1"))
	if _, err := a.StartSession(t.Context()); err != nil {
		t.Fatal(err)
	}
	return a, model
}

type chatOutcome struct {
	res agentloop.RunResult
	err error
}

func goChat(ctx context.Context, a *agent.Agent, msg string) chan chatOutcome {
	out := make(chan chatOutcome, 1)
	go func() {
		res, err := a.Chat(ctx, msg)
		out <- chatOutcome{res, err}
	}()
	return out
}

// Two overlapping chats of one agent are serialized by the agent (the caller does not
// have to): the second waits until the first is over, the model never
// sees two calls at once, and both turns land whole in the conversation.
// (A-97: two chats of one session.)
func TestAgent_ChatsAreSerialized(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a, model := heldAgent(t)
		first := goChat(t.Context(), a, "one")
		<-model.entered
		second := goChat(t.Context(), a, "two")
		synctest.Wait()
		if n := len(model.entered); n != 0 {
			t.Fatalf("%d model calls started while the first chat runs, want the second chat to wait", n)
		}
		close(model.gate)
		for _, ch := range []chan chatOutcome{first, second} {
			if o := <-ch; o.err != nil || o.res.Aborted {
				t.Fatalf("chat = %+v, %v, want a finished turn", o.res, o.err)
			}
		}
		if p := model.peak.Load(); p != 1 {
			t.Errorf("peak concurrent model calls = %d, want 1", p)
		}
	})
}

// A chat that waits for its turn gives up when its ctx ends, with the ctx error and
// nothing written; the running chat is not disturbed.
func TestAgent_WaitingChatHonoursCtx(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a, model := heldAgent(t)
		first := goChat(t.Context(), a, "one")
		<-model.entered

		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		start := time.Now()
		_, err := a.Chat(ctx, "two")
		if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) != 5*time.Second {
			t.Fatalf("waiting Chat = %v after %v, want the deadline after 5s", err, time.Since(start))
		}
		// The stream form gives the same error as its one pair.
		var pairs int
		for _, err := range a.ChatStream(ctx, "three") {
			pairs++
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("ChatStream with an expired ctx while busy = %v", err)
			}
		}
		if pairs != 1 {
			t.Errorf("ChatStream yielded %d pairs, want 1", pairs)
		}
		close(model.gate)
		if o := <-first; o.err != nil || o.res.Aborted {
			t.Fatalf("first chat = %+v, %v", o.res, o.err)
		}
	})
}

// A ChatStream holds the turn while it is iterated, and a sequence that is never
// iterated never takes it.
func TestAgent_ChatStreamHoldsTurnWhileIterated(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a, model := heldAgent(t)
		_ = a.ChatStream(t.Context(), "never iterated")

		var mu sync.Mutex
		var streamDone bool
		go func() {
			for _, err := range a.ChatStream(t.Context(), "streamed") {
				if err != nil {
					t.Errorf("stream: %v", err)
				}
			}
			mu.Lock()
			streamDone = true
			mu.Unlock()
		}()
		<-model.entered
		second := goChat(t.Context(), a, "after the stream")
		synctest.Wait()
		if n := len(model.entered); n != 0 {
			t.Fatal("a chat started while a stream was being iterated")
		}
		close(model.gate)
		if o := <-second; o.err != nil {
			t.Fatal(o.err)
		}
		// The stream releases the turn inside its iterator, a moment before the range
		// loop above returns, so the waiting chat may finish before streamDone is set:
		// overlap is judged by the model calls in flight, and the stream goroutine is
		// only required to end.
		if p := model.peak.Load(); p != 1 {
			t.Errorf("peak concurrent model calls = %d, want 1: the chat ran while the stream was being iterated", p)
		}
		synctest.Wait()
		mu.Lock()
		defer mu.Unlock()
		if !streamDone {
			t.Error("the stream did not finish")
		}
	})
}

// Dispose cancels the running chat (its result is Aborted), releases the chat that
// waits for its turn with ErrAgentDisposed, waits for the running one, and any later
// Chat or ChatStream returns ErrAgentDisposed.
func TestAgent_DisposeStopsChats(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a, model := heldAgent(t)
		running := goChat(t.Context(), a, "one")
		<-model.entered
		waiting := goChat(t.Context(), a, "two")
		synctest.Wait()

		if err := a.Dispose(t.Context()); err != nil {
			t.Fatalf("Dispose = %v", err)
		}
		// Dispose waited for the running chat, so both outcomes are ready now. The run
		// ends inside Chat, and goChat sends its outcome right after Chat returns, so
		// let that goroutine reach its send (nothing in the bubble is durably blocked
		// on the run any more) before looking without blocking.
		synctest.Wait()
		select {
		case o := <-running:
			if o.err != nil || !o.res.Aborted {
				t.Errorf("running chat = %+v, %v, want an Aborted result", o.res, o.err)
			}
		default:
			t.Fatal("Dispose returned before the running chat ended")
		}
		if o := <-waiting; !errors.Is(o.err, agent.ErrAgentDisposed) {
			t.Errorf("waiting chat = %v, want ErrAgentDisposed", o.err)
		}
		if _, err := a.Chat(t.Context(), "late"); !errors.Is(err, agent.ErrAgentDisposed) {
			t.Errorf("Chat after Dispose = %v, want ErrAgentDisposed", err)
		}
		for _, err := range a.ChatStream(t.Context(), "late") {
			if !errors.Is(err, agent.ErrAgentDisposed) {
				t.Errorf("ChatStream after Dispose = %v, want ErrAgentDisposed", err)
			}
		}
		// Idempotent.
		if err := a.Dispose(t.Context()); err != nil {
			t.Errorf("second Dispose = %v", err)
		}
	})
}

// Dispose cancels a running chat even when its model ignores cancellation: the loop
// gives up on the model call at once (its abort race), the chat ends Aborted, and
// Dispose returns without spending its ctx. The abandoned model call ends when the
// model returns.
func TestAgent_DisposeStubbornModel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		e.llm = llmhub.NewLLMHub()
		gate := make(chan struct{})
		entered := make(chan struct{}, 1)
		stubborn := &funcModel{
			MockProvider: aecmock.NewMockProvider("stubborn", func([]kernel.Message) string { return "late" }),
			before: func(context.Context) error {
				entered <- struct{}{}
				<-gate
				return nil
			},
		}
		if err := e.llm.Register(stubborn); err != nil {
			t.Fatal(err)
		}
		a := e.spawn(t, testProfile("a1"))
		if _, err := a.StartSession(t.Context()); err != nil {
			t.Fatal(err)
		}
		running := goChat(t.Context(), a, "one")
		<-entered

		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		start := time.Now()
		if err := a.Dispose(ctx); err != nil {
			t.Fatalf("Dispose = %v, want nil", err)
		}
		if got := time.Since(start); got != 0 {
			t.Errorf("Dispose took %v of fake time, want none", got)
		}
		if o := <-running; o.err != nil || !o.res.Aborted {
			t.Errorf("chat = %+v, %v, want Aborted", o.res, o.err)
		}
		close(gate)
		synctest.Wait()
	})
}

// funcModel is a mock provider that runs before ahead of each model call.
type funcModel struct {
	*aecmock.MockProvider
	before func(ctx context.Context) error
}

func (f *funcModel) Chat(ctx context.Context, msgs []kernel.Message, opts llmhub.ModelOptions) (kernel.ModelResponse, error) {
	if err := f.before(ctx); err != nil {
		return kernel.ModelResponse{}, err
	}
	return f.MockProvider.Chat(ctx, msgs, opts)
}

func (f *funcModel) Stream(ctx context.Context, msgs []kernel.Message, opts llmhub.ModelOptions) iter.Seq2[kernel.StreamChunk, error] {
	return func(yield func(kernel.StreamChunk, error) bool) {
		if err := f.before(ctx); err != nil {
			yield(kernel.StreamChunk{}, err)
			return
		}
		f.MockProvider.Stream(ctx, msgs, opts)(yield)
	}
}

// A chat started with the ctx of a running chat of the same agent (here from inside
// the model call, as a tool would) returns ErrReentrantChat instead of waiting for
// itself; a Dispose from there neither waits for the chat nor cancels it early.
// (A-97: the nested chat names no session, so it is for the same, default session; a
// nested chat on another session runs, see TestConcurrentSessions_NestedChatInAnotherSession.)
func TestAgent_ReentrantChat(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		e.llm = llmhub.NewLLMHub()
		var a *agent.Agent
		var nested, disposed error
		var calls atomic.Int32
		// The inner Dispose cancels the outer chat, so the loop abandons this model call
		// and the outer Chat may return while the hook still runs: the test reads nested
		// and disposed only after probed is closed.
		probed := make(chan struct{})
		probe := &funcModel{
			MockProvider: aecmock.NewMockProvider("probe", func([]kernel.Message) string { return "ok" }),
			before: func(ctx context.Context) error {
				if calls.Add(1) == 1 {
					defer close(probed)
					_, nested = a.Chat(ctx, "nested")
					disposed = a.Dispose(ctx)
				}
				return nil
			},
		}
		if err := e.llm.Register(probe); err != nil {
			t.Fatal(err)
		}
		a = e.spawn(t, testProfile("a1"))
		if _, err := a.StartSession(t.Context()); err != nil {
			t.Fatal(err)
		}
		if _, err := a.Chat(t.Context(), "hi"); err != nil {
			t.Fatalf("outer Chat = %v", err)
		}
		<-probed
		if !errors.Is(nested, agent.ErrReentrantChat) {
			t.Errorf("nested Chat = %v, want ErrReentrantChat", nested)
		}
		if disposed != nil {
			t.Errorf("Dispose from inside the chat = %v, want nil (no wait for itself)", disposed)
		}
		if _, err := a.Chat(t.Context(), "after"); !errors.Is(err, agent.ErrAgentDisposed) {
			t.Errorf("Chat after the inner Dispose = %v, want ErrAgentDisposed", err)
		}
	})
}

// DisableSkill removes the skill from the allowlist and saves the profile, as one
// allowlist change: eight removals at once lose none, and the last save holds none of
// them (the skill-delete fan-out reads, changes and saves under the agent's allowlist
// mutex). A skill that is not enabled reports false and saves nothing.
func TestAgent_DisableSkillIsAtomic(t *testing.T) {
	e := newEnv(t)
	var mu sync.Mutex
	var saves [][]string
	deps := e.deps()
	deps.SaveProfile = func(_ context.Context, p profile.AgentProfile) error {
		mu.Lock()
		defer mu.Unlock()
		saves = append(saves, slices.Clone(p.Skills))
		return nil
	}
	p := testProfile("a1")
	names := []string{"s0", "s1", "s2", "s3", "s4", "s5", "s6", "s7"}
	p.Skills = append(slices.Clone(names), "keep")
	a, err := agent.New(t.Context(), deps, p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Dispose(context.WithoutCancel(t.Context())) })

	var wg sync.WaitGroup
	for _, n := range names {
		wg.Go(func() {
			if changed, err := a.DisableSkill(t.Context(), n); !changed || err != nil {
				t.Errorf("DisableSkill(%s) = %v, %v", n, changed, err)
			}
		})
	}
	wg.Wait()
	if got := a.Profile().Skills; !slices.Equal(got, []string{"keep"}) {
		t.Fatalf("Skills = %v, want [keep]", got)
	}
	mu.Lock()
	last := saves[len(saves)-1]
	n := len(saves)
	mu.Unlock()
	if n != len(names) || !slices.Equal(last, []string{"keep"}) {
		t.Fatalf("%d saves, last %v; want one per removal and the last one holding only keep", n, last)
	}
	if changed, err := a.DisableSkill(t.Context(), "absent"); changed || err != nil {
		t.Fatalf("DisableSkill(absent) = %v, %v, want false and nothing saved", changed, err)
	}
}
