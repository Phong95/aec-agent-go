package agentloop_test

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Phong95/aec-agent-go/aecmock"
	"github.com/Phong95/aec-agent-go/components/agent/agentloop"
	"github.com/Phong95/aec-agent-go/components/agent/profile"
	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/components/trace"
	"github.com/Phong95/aec-agent-go/kernel"
)

func zeroInt() *int { return intPtr(0) }

// TestRun_EndTurn is the tracer: one end_turn turn over the fakes, with the expected
// events, span order and attribute order, the persisted messages and the
// usage written back to the session.
func TestRun_EndTurn(t *testing.T) {
	r := newRig(t, rigOptions{}, aecmock.ScriptedTurn{Content: "Hello back!", StopReason: kernel.StopEndTurn})
	res, err := r.run(t, "Hello")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	wantUsage := kernel.TokenUsage{InputTokens: 10, OutputTokens: 5, CacheReadTokens: zeroInt(), CacheWriteTokens: zeroInt()}
	if res.Message.Role != kernel.RoleAssistant || res.Message.Content != "Hello back!" || res.Iterations != 1 || res.Aborted {
		t.Fatalf("result = %+v", res)
	}
	if !reflect.DeepEqual(res.Usage, wantUsage) {
		t.Fatalf("usage = %+v, want %+v", res.Usage, wantUsage)
	}

	wantTypes := []string{"agent-loop:message", "agent-loop:iteration", "agent-loop:model-request", "agent-loop:model-response", "agent-loop:context-usage", "agent-loop:complete"}
	if got := r.log.types(); !slices.Equal(got, wantTypes) {
		t.Fatalf("event types = %v, want %v", got, wantTypes)
	}
	if got, want := r.log.payload(t, events.UserMessage.Name), `{"agentId":"test-agent","role":"user","content":"Hello"}`; got != want {
		t.Fatalf("UserMessage payload = %s, want %s", got, want)
	}
	if got, want := r.log.payload(t, events.ModelResponded.Name), `{"agentId":"test-agent","stopReason":"end_turn","usage":{"inputTokens":10,"outputTokens":5},"hasToolCalls":false}`; got != want {
		t.Fatalf("ModelResponded payload = %s, want %s", got, want)
	}
	if got, want := r.log.payload(t, events.RunCompleted.Name), `{"agentId":"test-agent","iterations":1,"totalUsage":{"inputTokens":10,"outputTokens":5,"cacheReadTokens":0,"cacheWriteTokens":0}}`; got != want {
		t.Fatalf("RunCompleted payload = %s, want %s", got, want)
	}

	wantEnds := []string{"compaction", "context-assembly", "model-call", "iteration-1", "agent-run"}
	if got := r.sink.endNames(); !slices.Equal(got, wantEnds) {
		t.Fatalf("span end order = %v, want %v", got, wantEnds)
	}
	if got := r.sink.parentName("compaction"); got != "context-assembly" {
		t.Fatalf("compaction parent = %q, want context-assembly", got)
	}
	if got := r.sink.parentName("agent-run"); got != "" {
		t.Fatalf("agent-run parent = %q, want none", got)
	}
	if started, ended := r.sink.startedAndEnded(); started != ended || started != 5 {
		t.Fatalf("spans started %d, ended %d, want 5 and 5", started, ended)
	}

	// Attribute order is the expected order, key by key.
	assertAttrKeys(t, r.sink, "start", "agent-run", []string{"agentId", "sessionId"})
	assertAttrKeys(t, r.sink, "end", "agent-run", []string{"agentId", "sessionId", "iterations", "totalUsage"})
	assertAttrKeys(t, r.sink, "start", "iteration-1", []string{"iteration", "maxIterations"})
	assertAttrKeys(t, r.sink, "start", "compaction", []string{"messageCountBefore", "inputTokens", "outputTokens"})
	assertAttrKeys(t, r.sink, "end", "compaction", []string{"messageCountBefore", "inputTokens", "outputTokens", "triggered", "messageCountAfter", "summarySourceLength", "tailTokens"})
	assertAttrKeys(t, r.sink, "start", "model-call", []string{"iteration", "toolCount", "toolNames", "inputMessageCount", "options"})
	assertAttrKeys(t, r.sink, "end", "model-call", []string{"iteration", "toolCount", "toolNames", "inputMessageCount", "options", "stopReason", "inputTokens", "outputTokens", "hasToolCalls", "responsePreview"})

	msgs := r.conv.Messages()
	if len(msgs) != 2 || msgs[0].Role != kernel.RoleUser || msgs[0].Content != "Hello" || msgs[1].Role != kernel.RoleAssistant || msgs[1].Content != "Hello back!" {
		t.Fatalf("persisted messages = %+v", msgs)
	}
	stored, err := r.hub.Sessions.Get(t.Context(), mustSession(t, r.conv).ID)
	if err != nil || stored == nil {
		t.Fatalf("stored session: %v %v", stored, err)
	}
	if u := stored.CurrentTokenUsage; u == nil || u.InputTokens != 10 || u.OutputTokens != 5 {
		t.Fatalf("stored current usage = %+v, want 10 and 5", u)
	}
}

func mustSession(t *testing.T, c agentloop.Conversation) kernel.Session {
	t.Helper()
	s, ok := c.Session()
	if !ok {
		t.Fatal("no active session")
	}
	return s
}

// assertAttrKeys requires the first span of the name, on the given phase, to hold
// exactly keys in that order.
func assertAttrKeys(t *testing.T, s *recSink, phase, name string, keys []string) {
	t.Helper()
	for _, r := range s.all() {
		if r.phase == phase && r.span.Name == name {
			if got := r.span.Attributes.Keys(); !slices.Equal(got, keys) {
				t.Fatalf("%s %s attribute keys = %v, want %v", phase, name, got, keys)
			}
			return
		}
	}
	t.Fatalf("no %s notification for span %q", phase, name)
}

// writeCtxConversation wraps a Conversation and records ctx.Err() at every write.
type writeCtxConversation struct {
	agentloop.Conversation
	mu   sync.Mutex
	errs []error
	n    int
}

func (c *writeCtxConversation) record(ctx context.Context) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.errs = append(c.errs, ctx.Err())
	c.n++
}

func (c *writeCtxConversation) AddMessage(ctx context.Context, m kernel.Message) error {
	c.record(ctx)
	return c.Conversation.AddMessage(ctx, m)
}

func (c *writeCtxConversation) SetCompactedMessages(ctx context.Context, msgs []kernel.Message) error {
	c.record(ctx)
	return c.Conversation.SetCompactedMessages(ctx, msgs)
}

func (c *writeCtxConversation) UpdateSession(ctx context.Context, s kernel.Session) error {
	c.record(ctx)
	return c.Conversation.UpdateSession(ctx, s)
}

// cancelAfterChat cancels a context right after the wrapped model returned.
type cancelAfterChat struct {
	agentloop.Models
	cancel context.CancelFunc
}

func (m *cancelAfterChat) Chat(ctx context.Context, msgs []kernel.Message, opts llmhub.ModelOptions) (kernel.ModelResponse, error) {
	resp, err := m.Models.Chat(ctx, msgs, opts)
	m.cancel()
	// Give the run a moment to see the stop before the late reply is handed over, so
	// the outcome does not depend on which of the two it notices first.
	time.Sleep(20 * time.Millisecond)
	return resp, err
}

// TestRun_PersistsUnderCancelledCtx proves that no conversation write sees the
// caller's cancellation: the caller cancels right after the model answers (from the
// ModelResponded subscriber, which runs before the reply is stored; a cancel inside
// the model call is a stop, see TestRun_CancelInsideTheModelCallIsAStop), and
// the reply and the session usage are still written, each under a live ctx. It is
// a negative control for persist: using the caller's ctx makes it fail.
func TestRun_PersistsUnderCancelledCtx(t *testing.T) {
	r := newRig(t, rigOptions{}, aecmock.ScriptedTurn{Content: "Hello back!"})
	wrapped := &writeCtxConversation{Conversation: r.conv}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	deps := r.deps
	deps.Conversation = wrapped
	r.bus.On(events.ModelResponded.Name, func(kernel.Event) { cancel() })
	loop, err := agentloop.New(deps)
	if err != nil {
		t.Fatal(err)
	}
	res, err := loop.Run(ctx, "Hello", "s-1", llmhub.ModelOptions{}, agentloop.RunOptions{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Message.Content != "Hello back!" {
		t.Fatalf("result = %+v", res)
	}
	if ctx.Err() == nil {
		t.Fatal("the caller ctx was not cancelled: the test proves nothing")
	}
	if wrapped.n != 3 {
		t.Fatalf("%d conversation writes, want 3 (user message, reply, session usage)", wrapped.n)
	}
	for i, e := range wrapped.errs {
		if e != nil {
			t.Fatalf("write %d ran under a done ctx: %v", i, e)
		}
	}
	msgs := r.conv.Messages()
	if len(msgs) != 2 || msgs[1].Content != "Hello back!" {
		t.Fatalf("messages after cancel = %+v", msgs)
	}
}

// TestRun_CancelInsideTheModelCallIsAStop: a cancel that arrives while the model call
// is still running stops the run, even when the model then answers: the reply is
// dropped, nothing but the user message is stored and the result says Aborted.
func TestRun_CancelInsideTheModelCallIsAStop(t *testing.T) {
	r := newRig(t, rigOptions{}, aecmock.ScriptedTurn{Content: "late answer"})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	deps := r.deps
	deps.Models = &cancelAfterChat{Models: r.models, cancel: cancel}
	loop, err := agentloop.New(deps)
	if err != nil {
		t.Fatal(err)
	}
	res, err := loop.Run(ctx, "Hello", "s-1", llmhub.ModelOptions{}, agentloop.RunOptions{})
	if err != nil || !res.Aborted || res.Message.Content != "" {
		t.Fatalf("Run = %+v, %v, want an aborted result and no error", res, err)
	}
	if msgs := r.conv.Messages(); len(msgs) != 1 || msgs[0].Role != kernel.RoleUser {
		t.Fatalf("messages = %+v, want only the user message", msgs)
	}
}

func TestNew_RejectsMissingDeps(t *testing.T) {
	good := newRig(t, rigOptions{}).deps
	var typedNil *aecmock.ScriptedModels
	cases := []struct {
		name   string
		mutate func(*agentloop.Deps)
		want   string
	}{
		{"models", func(d *agentloop.Deps) { d.Models = nil }, "agentloop: Deps.Models is required"},
		{"typed nil models", func(d *agentloop.Deps) { d.Models = typedNil }, "agentloop: Deps.Models is required"},
		{"tools", func(d *agentloop.Deps) { d.Tools = nil }, "agentloop: Deps.Tools is required"},
		{"context", func(d *agentloop.Deps) { d.Context = nil }, "agentloop: Deps.Context is required"},
		// A-97: Deps.Conversation is optional (a run can name its own in
		// RunOptions.Conversation), see TestRun_PerRunConversation.
		{"events", func(d *agentloop.Deps) { d.Events = nil }, "agentloop: Deps.Events is required"},
		{"profile", func(d *agentloop.Deps) { d.Profile = nil }, "agentloop: Deps.Profile is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := good
			tc.mutate(&d)
			l, err := agentloop.New(d)
			if err == nil || err.Error() != tc.want || l != nil {
				t.Fatalf("New = %v, %v, want error %q", l, err, tc.want)
			}
		})
	}
	// Skills and Trace are optional; a typed nil Skills is treated as absent.
	d := good
	d.Skills = (*nilSkills)(nil)
	d.Trace = nil
	if _, err := agentloop.New(d); err != nil {
		t.Fatalf("New with optional deps absent: %v", err)
	}
}

// A-97: Deps.Conversation is optional. A run names its conversation in
// RunOptions.Conversation; a run with neither fails with ErrNoActiveSession before
// anything is written.
func TestRun_PerRunConversation(t *testing.T) {
	r := newRig(t, rigOptions{}, aecmock.ScriptedTurn{Content: "hi back", StopReason: kernel.StopEndTurn})
	d := r.deps
	d.Conversation = nil
	l, err := agentloop.New(d)
	if err != nil {
		t.Fatalf("New without Deps.Conversation: %v", err)
	}
	if _, err := l.Run(t.Context(), "hi", "s-1", llmhub.ModelOptions{}, agentloop.RunOptions{}); !errors.Is(err, agentloop.ErrNoActiveSession) {
		t.Fatalf("Run without any conversation: err = %v, want ErrNoActiveSession", err)
	}
	res, err := l.Run(t.Context(), "hi", "s-1", llmhub.ModelOptions{}, agentloop.RunOptions{Conversation: r.conv})
	if err != nil {
		t.Fatalf("Run with RunOptions.Conversation: %v", err)
	}
	if res.Message.Content != "hi back" {
		t.Fatalf("content = %q, want %q", res.Message.Content, "hi back")
	}
	if msgs := r.conv.Messages(); len(msgs) != 2 {
		t.Fatalf("messages = %+v, want the user message and the reply", msgs)
	}
}

// nilSkills is a Skills implementation used only as a typed nil.
type nilSkills struct{}

func (*nilSkills) Tool(string) (toolhub.Handler, bool) { return nil, false }
func (*nilSkills) ToolDefinitions(context.Context) ([]toolhub.Definition, error) {
	return nil, nil
}

// loopingTurns scripts n answers that each call a tool (one that does not exist, so
// it is answered at once): none of them ends the turn, so the run keeps iterating
// until the cap. They do not stop with max_tokens, because max_tokens ends the turn
// (TestRunVsStreamStopReasons).
func loopingTurns(n int) []aecmock.ScriptedTurn {
	turns := make([]aecmock.ScriptedTurn, n)
	for i := range turns {
		turns[i] = toolTurn(callOf("tc-"+strconv.Itoa(i+1), "ghost", "{}"))
	}
	return turns
}

func countType(types []string, want string) int {
	n := 0
	for _, t := range types {
		if t == want {
			n++
		}
	}
	return n
}

// The loop-config cap cases (L7 and L8) on the Run path.
func TestRun_CapsAtMaxIterations(t *testing.T) {
	prof := testProfile("test-agent")
	prof.LoopConfig = &profile.LoopConfig{MaxIterations: intPtr(2)}
	r := newRig(t, rigOptions{prof: &prof}, loopingTurns(5)...)

	res, err := r.run(t, "go on")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	types := r.log.types()
	if res.Iterations != 2 || res.Aborted || res.Message.Role != kernel.RoleAssistant || res.Message.Content != "Reached maximum reasoning steps. Please refine your request." {
		t.Fatalf("result = %+v", res)
	}
	if got := countType(types, events.Iteration.Name); got != 2 {
		t.Fatalf("%d Iteration events, want 2", got)
	}
	if countType(types, events.RunCompleted.Name) != 0 || countType(types, events.MaxIterationsReached.Name) != 1 {
		t.Fatalf("event types = %v, want MaxIterationsReached and no RunCompleted", types)
	}
	if got, want := r.log.payload(t, events.MaxIterationsReached.Name), `{"agentId":"test-agent","iterations":2,"totalUsage":{"inputTokens":20,"outputTokens":10,"cacheReadTokens":0,"cacheWriteTokens":0}}`; got != want {
		t.Fatalf("MaxIterationsReached payload = %s, want %s", got, want)
	}
	if got := len(r.models.Calls()); got != 2 {
		t.Fatalf("%d model calls, want 2", got)
	}
	attrs, ok := r.sink.endAttrs("agent-run")
	if !ok || !slices.Equal(attrs.Keys(), []string{"agentId", "sessionId", "iterations", "totalUsage", "maxIterationsReached"}) {
		t.Fatalf("agent-run end attributes = %v", attrs.Keys())
	}
	if v, _ := attrs.Get("maxIterationsReached"); v != true {
		t.Fatalf("maxIterationsReached = %v", v)
	}
	if started, ended := r.sink.startedAndEnded(); started != ended {
		t.Fatalf("spans started %d, ended %d", started, ended)
	}
}

func TestRun_DefaultCapIsHundred(t *testing.T) {
	r := newRig(t, rigOptions{}, loopingTurns(102)...)
	res, err := r.run(t, "go on")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Iterations != 100 {
		t.Fatalf("Iterations = %d, want the default cap 100", res.Iterations)
	}
	if got := countType(r.log.types(), events.Iteration.Name); got != 100 {
		t.Fatalf("%d Iteration events, want 100", got)
	}
	if got := r.log.payload(t, events.Iteration.Name); got != `{"agentId":"test-agent","iteration":1,"maxIterations":100}` {
		t.Fatalf("Iteration payload = %s", got)
	}
}

// TestLoop_ReadsLoopConfigLive is case L10 on the Run path: the limits come from the
// profile snapshot Deps.Profile returns at the start of each run, so changing the
// snapshot between two runs changes the cap of the next one, and a nil config
// restores the default.
func TestLoop_ReadsLoopConfigLive(t *testing.T) {
	base := newRig(t, rigOptions{}, loopingTurns(2+3+100)...)
	current := testProfile("test-agent")
	deps := base.deps
	deps.Profile = func() profile.AgentProfile { return current }
	loop, err := agentloop.New(deps)
	if err != nil {
		t.Fatal(err)
	}

	for _, step := range []struct {
		name string
		cfg  *profile.LoopConfig
		want int
	}{
		{"cap 2", &profile.LoopConfig{MaxIterations: intPtr(2)}, 2},
		{"cap 3", &profile.LoopConfig{MaxIterations: intPtr(3)}, 3},
		{"restored", nil, 100},
	} {
		current.LoopConfig = step.cfg
		res, err := loop.Run(t.Context(), "go on", "s-1", llmhub.ModelOptions{}, agentloop.RunOptions{})
		if err != nil || res.Iterations != step.want {
			t.Fatalf("%s: Iterations = %d, %v, want %d", step.name, res.Iterations, err, step.want)
		}
	}
}

func TestRun_ModelFailureClosesSpans(t *testing.T) {
	boom := kernel.NewModelError("provider down", kernel.ModelCodeProvider, true)
	r := newRig(t, rigOptions{}, aecmock.ScriptedTurn{Err: boom})

	res, err := r.run(t, "Hello")
	var me *kernel.ModelError
	if !errors.As(err, &me) || me != boom {
		t.Fatalf("Run error = %v, want the *kernel.ModelError itself", err)
	}
	if res.Message.Content != "" || res.Iterations != 0 {
		t.Fatalf("a failed run returned %+v, want the zero result", res)
	}

	if started, ended := r.sink.startedAndEnded(); started != ended || started == 0 {
		t.Fatalf("spans started %d, ended %d: every started span must end", started, ended)
	}
	for _, name := range []string{"model-call", "iteration-1", "agent-run"} {
		attrs, ok := r.sink.endAttrs(name)
		if v, _ := attrs.Get("error"); !ok || v != "provider down" {
			t.Fatalf("%s end error attribute = %v, want the model error text", name, v)
		}
	}
	// Spans that had already ended with their own attributes keep them.
	if attrs, _ := r.sink.endAttrs("compaction"); attrs.Has("error") {
		t.Fatal("a span that ended normally was ended again with the error")
	}
	for _, rec := range r.sink.all() {
		if rec.phase == "end" && rec.span.Name == "agent-run" && rec.span.Status != trace.StatusError {
			t.Fatalf("agent-run status = %q, want error", rec.span.Status)
		}
	}

	traceID := r.sink.all()[0].span.TraceID
	if _, ok := r.deps.Trace.Context(traceID); ok {
		t.Fatal("the trace context was not removed")
	}
	types := r.log.types()
	if countType(types, events.RunCompleted.Name) != 0 || countType(types, events.MaxIterationsReached.Name) != 0 {
		t.Fatalf("event types = %v: a failed run emits neither RunCompleted nor MaxIterationsReached", types)
	}
	// The user message was persisted before the model was asked.
	if msgs := r.conv.Messages(); len(msgs) != 1 || msgs[0].Content != "Hello" {
		t.Fatalf("persisted messages = %+v", msgs)
	}
}

func TestRun_NoActiveSession(t *testing.T) {
	r := newRig(t, rigOptions{noStart: true}, aecmock.ScriptedTurn{Content: "unused"})
	res, err := r.run(t, "Hello")
	if !errors.Is(err, agentloop.ErrNoActiveSession) || err.Error() != "No active session" {
		t.Fatalf("Run error = %v, want ErrNoActiveSession (No active session)", err)
	}
	if res.Iterations != 0 || res.Message.Content != "" {
		t.Fatalf("result = %+v", res)
	}
	if got := r.log.types(); len(got) != 0 {
		t.Fatalf("events = %v, want none", got)
	}
	if recs := r.sink.all(); len(recs) != 0 {
		t.Fatalf("%d span notifications, want none (no trace is opened without a session)", len(recs))
	}
	if len(r.conv.Messages()) != 0 || len(r.models.Calls()) != 0 {
		t.Fatal("a run without a session persisted a message or called the model")
	}
}

// hangingConversation never completes a message write by itself: AddMessage
// returns only when its ctx is done.
type hangingConversation struct{ agentloop.Conversation }

func (hangingConversation) AddMessage(ctx context.Context, _ kernel.Message) error {
	<-ctx.Done()
	return ctx.Err()
}

// TestRun_PersistTimeoutBoundsAHangingStore proves a store that never answers
// cannot hang a run: the write is cut off by the persist timeout (WithPersistTimeout,
// default 10 s) even though its ctx ignores the caller's cancellation, and the run
// then fails and closes its spans. The bubble makes the 5 s cost no wall time.
func TestRun_PersistTimeoutBoundsAHangingStore(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newRig(t, rigOptions{}, aecmock.ScriptedTurn{Content: "never asked"})
		deps := r.deps
		deps.Conversation = hangingConversation{Conversation: r.conv}
		loop, err := agentloop.New(deps, agentloop.WithPersistTimeout(5*time.Second), agentloop.WithPersistTimeout(0))
		if err != nil {
			t.Fatal(err)
		}

		start := time.Now()
		_, err = loop.Run(t.Context(), "Hello", "s-1", llmhub.ModelOptions{}, agentloop.RunOptions{})
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Run error = %v, want the persist deadline", err)
		}
		if got := time.Since(start); got != 5*time.Second {
			t.Fatalf("the write was cut off after %v, want exactly 5s (a zero option keeps the previous value)", got)
		}
		if len(r.models.Calls()) != 0 {
			t.Fatal("the model was asked although the user message was never stored")
		}
		if started, ended := r.sink.startedAndEnded(); started != ended || started != 1 {
			t.Fatalf("spans started %d, ended %d, want the run span started and ended", started, ended)
		}
	})
}

// TestRun_RejectsInactiveSessionID (IN-05): a sessionID that is not the active
// session's id fails with ErrSessionNotActive before anything is persisted,
// emitted or traced, and the model is never asked.
func TestRun_RejectsInactiveSessionID(t *testing.T) {
	r := newRig(t, rigOptions{}, aecmock.ScriptedTurn{Content: "never"})
	_, err := r.loop.Run(t.Context(), "hi", "some-other-session", llmhub.ModelOptions{}, agentloop.RunOptions{})
	if !errors.Is(err, agentloop.ErrSessionNotActive) {
		t.Fatalf("Run error = %v, want a match on ErrSessionNotActive", err)
	}
	if text := err.Error(); !strings.Contains(text, "some-other-session") || !strings.Contains(text, "s-1") {
		t.Fatalf("error text = %q, want both session ids", text)
	}
	if errors.Is(err, agentloop.ErrNoActiveSession) {
		t.Fatal("a session mismatch must not read as no active session")
	}
	if n := len(r.conv.Messages()); n != 0 {
		t.Fatalf("%d messages were persisted for a rejected run", n)
	}
	if types := r.log.types(); len(types) != 0 {
		t.Fatalf("events %v were emitted for a rejected run", types)
	}
	if started, _ := r.sink.startedAndEnded(); started != 0 {
		t.Fatalf("%d spans started for a rejected run", started)
	}
	if n := len(r.models.Calls()); n != 0 {
		t.Fatalf("the model was called %d times for a rejected run", n)
	}

	// The same loop still runs the active session.
	if _, err := r.run(t, "hi"); err != nil {
		t.Fatalf("Run on the active session: %v", err)
	}
}

// The run's own usage and the stored working set without a system message.

// TestRun_UsageCountsOnlyTheRunsOwnCalls: the second run's usage, done chunk and
// RunCompleted count its own model call only (the session's current usage, 10/5
// here, is not added to any total).
func TestRun_UsageCountsOnlyTheRunsOwnCalls(t *testing.T) {
	usage := &kernel.TokenUsage{InputTokens: 10, OutputTokens: 5}
	rg := newRig(t, rigOptions{},
		aecmock.ScriptedTurn{Content: "one", Usage: usage},
		aecmock.ScriptedTurn{Content: "two", Usage: usage},
		aecmock.ScriptedTurn{Content: "three", Usage: usage},
	)
	want := `{"inputTokens":10,"outputTokens":5,"cacheReadTokens":0,"cacheWriteTokens":0}`
	for _, q := range []string{"first", "second"} {
		res, err := rg.run(t, q)
		if err != nil {
			t.Fatal(err)
		}
		if got := mustEncode(t, res.Usage); got != want {
			t.Fatalf("run %q usage = %s, want %s", q, got, want)
		}
	}
	chunks, err := rg.stream(t, "third")
	if err != nil {
		t.Fatal(err)
	}
	if got := mustEncode(t, chunks[len(chunks)-1].Usage); got != want {
		t.Fatalf("stream done usage = %s, want %s", got, want)
	}
	for _, e := range rg.log.all() {
		if e.Type != events.RunCompleted.Name {
			continue
		}
		if got := mustEncode(t, e.Payload); !strings.Contains(got, `"totalUsage":`+want) {
			t.Fatalf("RunCompleted = %s, want the run's own usage", got)
		}
	}
}

// TestRun_StoredSystemMessageIsNotDuplicated: a working set stored
// with a system message in front (by an older version) is read without it, so the
// context port's fresh system message is the only one in the model input.
func TestRun_StoredSystemMessageIsNotDuplicated(t *testing.T) {
	prof := testProfile("test-agent")
	rg := newRig(t, rigOptions{prof: &prof, context: aecmock.NewPromptContext(prof)}, aecmock.ScriptedTurn{Content: "ok"})
	stored := []kernel.Message{
		{Role: kernel.RoleSystem, Content: prof.SystemPrompt},
		{Role: kernel.RoleUser, Content: "earlier"},
		{Role: kernel.RoleAssistant, Content: "SUMMARY"},
	}
	if err := rg.conv.SetCompactedMessages(t.Context(), stored); err != nil {
		t.Fatal(err)
	}
	if _, err := rg.run(t, "now"); err != nil {
		t.Fatal(err)
	}
	in := rg.models.Calls()[0].Messages
	if got, want := contents(in), []string{prof.SystemPrompt, "earlier", "SUMMARY", "now"}; strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("model input = %v, want %v", got, want)
	}
}
