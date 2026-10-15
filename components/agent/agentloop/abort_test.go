package agentloop_test

import (
	"context"
	"errors"
	"iter"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Phong95/aec-agent-go/aecmock"
	"github.com/Phong95/aec-agent-go/components/agent/agentloop"
	"github.com/Phong95/aec-agent-go/components/agent/profile"
	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/kernel"
)

// This file holds the abort tests (R1 to R9) and the tests of the abort contract:
// a stop is a result and never an error, deadlines
// count as stops, every write survives the stop, and nothing that ignores ctx can
// delay it. The tests that wait on a timer or on a blocked fake run inside a
// synctest bubble (t.Run and goleak stay outside it), and every blocked fake is
// released before the bubble body returns.

// withDeps builds a loop over the rig's dependencies with some of them replaced.
func (r *rig) withDeps(t *testing.T, mutate func(*agentloop.Deps), opts ...agentloop.Option) *agentloop.Loop {
	t.Helper()
	deps := r.deps
	mutate(&deps)
	loop, err := agentloop.New(deps, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return loop
}

// hangingModels is a model whose Chat ignores ctx and does not answer until it is
// released (a provider that never settles). It embeds the concrete ScriptedModels so
// the tracing-only interfaces stay visible to the loop.
type hangingModels struct {
	*aecmock.ScriptedModels
	release chan struct{}
}

func newHangingModels(m *aecmock.ScriptedModels) *hangingModels {
	return &hangingModels{ScriptedModels: m, release: make(chan struct{})}
}

func (m *hangingModels) Chat(ctx context.Context, _ []kernel.Message, _ llmhub.ModelOptions) (kernel.ModelResponse, error) {
	<-m.release
	return kernel.ModelResponse{}, ctx.Err()
}

// ctxModels records the ctx its Stream was called with.
type ctxModels struct {
	*aecmock.ScriptedModels
	mu  sync.Mutex
	ctx context.Context
}

func (m *ctxModels) Stream(ctx context.Context, msgs []kernel.Message, opts llmhub.ModelOptions) iter.Seq2[kernel.StreamChunk, error] {
	m.mu.Lock()
	m.ctx = ctx
	m.mu.Unlock()
	return m.ScriptedModels.Stream(ctx, msgs, opts)
}

func (m *ctxModels) seen() context.Context {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ctx
}

// hangAfterConversation stores the first `after` messages and then blocks every
// AddMessage until its ctx is done: a store that stops answering.
type hangAfterConversation struct {
	agentloop.Conversation
	after int
	mu    sync.Mutex
	n     int
}

func (c *hangAfterConversation) AddMessage(ctx context.Context, m kernel.Message) error {
	c.mu.Lock()
	c.n++
	hang := c.n > c.after
	c.mu.Unlock()
	if hang {
		<-ctx.Done()
		return ctx.Err()
	}
	return c.Conversation.AddMessage(ctx, m)
}

// textChunk is a text chunk.
func textChunk(s string) kernel.StreamChunk {
	return kernel.StreamChunk{Type: kernel.ChunkText, Content: &s}
}

// drainEach collects a sequence like drain and calls each after every chunk the
// consumer received, which is where a test cancels the run from the consumer side.
func drainEach(seq iter.Seq2[kernel.StreamChunk, error], each func(kernel.StreamChunk)) ([]kernel.StreamChunk, error) {
	var chunks []kernel.StreamChunk
	for c, err := range seq {
		if err != nil {
			return chunks, err
		}
		chunks = append(chunks, c)
		each(c)
	}
	return chunks, nil
}

// requireAbortedDone requires the stream to end with exactly one done chunk marked
// aborted.
func requireAbortedDone(t *testing.T, chunks []kernel.StreamChunk) kernel.StreamChunk {
	t.Helper()
	if len(chunks) == 0 {
		t.Fatal("the stream yielded nothing, want a done chunk marked aborted")
	}
	last := chunks[len(chunks)-1]
	if last.Type != kernel.ChunkDone || !last.Aborted || last.Usage == nil {
		t.Fatalf("last chunk = %+v, want a done chunk marked aborted that carries usage", last)
	}
	dones := 0
	for _, c := range chunks {
		if c.Type == kernel.ChunkDone {
			dones++
		}
	}
	if dones != 1 {
		t.Fatalf("%d done chunks, want exactly one", dones)
	}
	return last
}

// TestStream_AbortMidStream (R1): the consumer cancels after the second text
// chunk. The stream ends with done and aborted, the third chunk is never forwarded, the
// history holds the user message and the partial reply "Hello" without tool calls, and
// the provider was called once.
func TestStream_AbortMidStream(t *testing.T) {
	r := newRig(t, rigOptions{}, aecmock.ScriptedTurn{Chunks: []kernel.StreamChunk{
		textChunk("Hel"), textChunk("lo"), textChunk(" world"), {Type: kernel.ChunkDone},
	}})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	texts := 0
	chunks, err := drainEach(r.seq(ctx, "hi"), func(c kernel.StreamChunk) {
		if c.Type == kernel.ChunkText {
			texts++
			if texts == 2 {
				cancel()
			}
		}
	})
	if err != nil {
		t.Fatalf("a stop is a done chunk, never an error: %v", err)
	}
	requireAbortedDone(t, chunks)
	if texts != 2 {
		t.Fatalf("%d text chunks reached the consumer, want 2", texts)
	}
	if got := len(r.models.Calls()); got != 1 {
		t.Fatalf("the provider was called %d times, want 1", got)
	}
	msgs := r.conv.Messages()
	if len(msgs) != 2 || msgs[0].Content != "hi" || msgs[1].Role != kernel.RoleAssistant || msgs[1].Content != "Hello" || msgs[1].ToolCalls != nil {
		t.Fatalf("history = %+v, want the user message and the partial reply Hello", msgs)
	}
	if got, want := r.log.payload(t, events.RunCompleted.Name), `{"agentId":"test-agent","iterations":1,"totalUsage":{"inputTokens":0,"outputTokens":0},"aborted":true}`; got != want {
		t.Fatalf("RunCompleted = %s, want %s", got, want)
	}
}

// TestStream_AlreadyCancelled (R2): a ctx that is done before the call. The user
// message is persisted, the model is never asked, exactly one done chunk marked
// aborted is yielded, and RunCompleted is emitted with iterations 0.
func TestStream_AlreadyCancelled(t *testing.T) {
	r := newRig(t, rigOptions{}, aecmock.ScriptedTurn{Content: "never"})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	chunks, err := drain(r.seq(ctx, "hi"))
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 1 {
		t.Fatalf("%d chunks, want exactly one", len(chunks))
	}
	requireAbortedDone(t, chunks)
	if len(r.models.Calls()) != 0 {
		t.Fatal("the model was called although the ctx was already done")
	}
	msgs := r.conv.Messages()
	if len(msgs) != 1 || msgs[0].Role != kernel.RoleUser || msgs[0].Content != "hi" {
		t.Fatalf("history = %+v, want only the user message", msgs)
	}
	if got, want := r.log.payload(t, events.RunCompleted.Name), `{"agentId":"test-agent","iterations":0,"totalUsage":{"inputTokens":0,"outputTokens":0},"aborted":true}`; got != want {
		t.Fatalf("RunCompleted = %s, want %s", got, want)
	}
	if countOf(r, events.Iteration.Name) != 0 {
		t.Fatal("an iteration started although the ctx was already done")
	}
	if started, ended := r.sink.startedAndEnded(); started != ended || started != 1 {
		t.Fatalf("spans started %d, ended %d, want only the run span, closed", started, ended)
	}
}

// TestRun_AlreadyCancelled is R2 on Run: the user message stays, the model is not
// called and the result says Aborted, with iterations 0 and no error.
func TestRun_AlreadyCancelled(t *testing.T) {
	r := newRig(t, rigOptions{}, aecmock.ScriptedTurn{Content: "never"})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	res, err := r.loop.Run(ctx, "hi", "s-1", llmhub.ModelOptions{}, agentloop.RunOptions{})
	if err != nil || !res.Aborted || res.Iterations != 0 || res.Message.Role != kernel.RoleAssistant || res.Message.Content != "" {
		t.Fatalf("Run = %+v, %v, want an aborted result with 0 iterations and no error", res, err)
	}
	if len(r.models.Calls()) != 0 || len(r.conv.Messages()) != 1 {
		t.Fatal("the model was called or something besides the user message was stored")
	}
	if countOf(r, events.RunCompleted.Name) != 1 {
		t.Fatalf("event types = %v, want one RunCompleted", r.log.types())
	}
}

// TestStream_NoCancelNoAbortedFlag (R3): without a stop the done chunk has no
// aborted flag.
func TestStream_NoCancelNoAbortedFlag(t *testing.T) {
	r := newRig(t, rigOptions{}, aecmock.ScriptedTurn{Content: "hi there", Usage: &kernel.TokenUsage{InputTokens: 5, OutputTokens: 5}})
	chunks, err := r.stream(t, "hi")
	if err != nil {
		t.Fatal(err)
	}
	last := chunks[len(chunks)-1]
	if last.Type != kernel.ChunkDone || last.Aborted {
		t.Fatalf("last chunk = %+v, want a done chunk without the aborted flag", last)
	}
}

// TestStream_AbortDropsCollectedToolCall (R4): a tool call that was being streamed
// when the run was stopped is dropped: only the user message is persisted, so no
// tool call dangles without a result.
func TestStream_AbortDropsCollectedToolCall(t *testing.T) {
	call := callOf("tc-1", "ghost_tool", "{}")
	r := newRig(t, rigOptions{}, aecmock.ScriptedTurn{Chunks: []kernel.StreamChunk{
		{Type: kernel.ChunkToolCall, ToolCall: &call},
		// the provider never sends done: it hangs, which the scripted model models by
		// ending after the stop
	}})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	chunks, err := drainEach(r.seq(ctx, "hi"), func(c kernel.StreamChunk) {
		if c.Type == kernel.ChunkToolCall {
			cancel()
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	requireAbortedDone(t, chunks)
	msgs := r.conv.Messages()
	if len(msgs) != 1 || msgs[0].Role != kernel.RoleUser {
		t.Fatalf("history = %+v, want only the user message: the collected call is never persisted", msgs)
	}
}

// TestStream_ForwardsCtxToProvider (R5): the provider receives the run's ctx, so a
// stop reaches it, and the tool definitions are still in the options.
func TestStream_ForwardsCtxToProvider(t *testing.T) {
	echo := constTool("echo", toolhub.Output{Content: "ok"})
	r := newRig(t, rigOptions{handlers: []toolhub.Handler{echo}}, aecmock.ScriptedTurn{Chunks: []kernel.StreamChunk{
		textChunk("Hel"), textChunk("lo"), textChunk(" world"), {Type: kernel.ChunkDone},
	}})
	spy := &ctxModels{ScriptedModels: r.models}
	loop := r.withDeps(t, func(d *agentloop.Deps) { d.Models = spy })

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	texts := 0
	_, err := drainEach(loop.Stream(ctx, "hi", "s-1", llmhub.ModelOptions{}, agentloop.RunOptions{}), func(c kernel.StreamChunk) {
		if c.Type == kernel.ChunkText {
			texts++
			if texts == 2 {
				cancel()
			}
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	got := spy.seen()
	if got == nil {
		t.Fatal("the provider was never called")
	}
	select {
	case <-got.Done():
	default:
		t.Fatal("the provider's ctx is not done after the run was cancelled: it is not the run's ctx")
	}
	calls := r.models.Calls()
	if len(calls) != 1 || len(calls[0].Options.Tools) != 1 || calls[0].Options.Tools[0].Name != "echo" {
		t.Fatalf("options reaching the provider = %+v, want the tool definitions", calls)
	}
}

// TestStream_AbortWhileToolRuns (R6): the run is stopped while a tool that ignores
// ctx runs. The call is answered with the cancelled result (persisted and yielded as a
// tool_result chunk), the history has 3 messages, and the stream ends with done and
// aborted. The tool is released at the end of the test.
func TestStream_AbortWhileToolRuns(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		slow := newFakeTool("slow_tool", func(context.Context, kernel.Arguments, toolhub.Context) (toolhub.Output, error) {
			cancel() // the user stops the run while the tool works
			<-release
			return toolhub.Output{Content: "never used"}, nil
		})
		r := newRig(t, rigOptions{handlers: []toolhub.Handler{slow}},
			aecmock.ScriptedTurn{ToolCalls: []kernel.ToolCall{callOf("tc-1", "slow_tool", "{}")}, Usage: &kernel.TokenUsage{InputTokens: 10, OutputTokens: 5}})

		chunks, err := drain(r.seq(ctx, "hi"))
		if err != nil {
			t.Fatal(err)
		}
		requireAbortedDone(t, chunks)
		var results []kernel.ToolResult
		for _, c := range chunks {
			if c.Type == kernel.ChunkToolResult {
				results = append(results, *c.ToolResult)
			}
		}
		if len(results) != 1 || results[0].IsError == nil || !*results[0].IsError || results[0].Content != "Cancelled: the user stopped the run before this tool call finished." {
			t.Fatalf("tool_result chunks = %+v, want the cancelled result", results)
		}
		msgs := r.conv.Messages()
		if len(msgs) != 3 || len(msgs[1].ToolCalls) != 1 || len(msgs[2].ToolResults) != 1 || msgs[2].ToolResults[0].IsError == nil || !*msgs[2].ToolResults[0].IsError {
			t.Fatalf("history = %+v, want the user message, the call and the cancelled result", msgs)
		}
		if slow.executions() != 1 || len(r.models.Calls()) != 1 {
			t.Fatalf("the tool ran %d times and the model was called %d times, want 1 and 1", slow.executions(), len(r.models.Calls()))
		}
		if got, want := r.log.payload(t, events.RunCompleted.Name), `{"agentId":"test-agent","iterations":1,"totalUsage":{"inputTokens":10,"outputTokens":5,"cacheReadTokens":0,"cacheWriteTokens":0},"aborted":true}`; got != want {
			t.Fatalf("RunCompleted = %s, want %s", got, want)
		}
		close(release)
		synctest.Wait()
	})
}

// TestStream_AbortWhileWaitingForApproval (R7): the run is stopped from a
// ToolApprovalRequested subscriber while the user has not answered. It fails closed:
// the tool never runs, the persisted result is the denial text, RunCompleted says
// aborted and the stream ends with done and aborted.
func TestStream_AbortWhileWaitingForApproval(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		send := consequentialTool("send_rfi")
		handler, _ := recordingHandler(func(context.Context, toolhub.ApprovalRequest) (toolhub.ApprovalDecision, error) {
			<-release // the user never answers
			return toolhub.ApprovalDecision{Approved: true}, nil
		})
		r := newRig(t, rigOptions{handlers: []toolhub.Handler{send}, interaction: handler},
			aecmock.ScriptedTurn{ToolCalls: []kernel.ToolCall{callOf("tc-1", "send_rfi", "{}")}})
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		r.bus.On(events.ToolApprovalRequested.Name, func(kernel.Event) { cancel() })

		chunks, err := drain(r.seq(ctx, "hi"))
		if err != nil {
			t.Fatal(err)
		}
		requireAbortedDone(t, chunks)
		if send.executions() != 0 {
			t.Fatal("the tool ran although nobody approved it")
		}
		results := resultsOf(r)
		want := "Approval denied: user declined (the run was stopped by the user)" + doNotRetry
		if len(results) != 1 || results[0].IsError == nil || !*results[0].IsError || results[0].Content != want {
			t.Fatalf("persisted results = %+v, want the denial %q", results, want)
		}
		if got, want := r.log.payload(t, events.RunCompleted.Name), `{"agentId":"test-agent","iterations":1,"totalUsage":{"inputTokens":0,"outputTokens":0},"aborted":true}`; got != want {
			t.Fatalf("RunCompleted = %s, want %s", got, want)
		}
		close(release)
		synctest.Wait()
	})
}

// TestRun_AbortWhileModelHangs (R8): Run with a model call that ignores ctx and
// never answers returns at the instant of the stop: Aborted true, an empty message, no
// error, only the user message persisted.
func TestRun_AbortWhileModelHangs(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newRig(t, rigOptions{})
		hang := newHangingModels(r.models)
		loop := r.withDeps(t, func(d *agentloop.Deps) { d.Models = hang })
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		time.AfterFunc(5*time.Millisecond, cancel)

		start := time.Now()
		res, err := loop.Run(ctx, "x", "s-1", llmhub.ModelOptions{}, agentloop.RunOptions{})
		if err != nil || !res.Aborted || res.Message.Content != "" || res.Message.Role != kernel.RoleAssistant || res.Iterations != 1 {
			t.Fatalf("Run = %+v, %v, want an aborted result after 1 iteration and no error", res, err)
		}
		if elapsed := time.Since(start); elapsed != 5*time.Millisecond {
			t.Fatalf("Run returned after %v, want the instant of the stop (5ms)", elapsed)
		}
		msgs := r.conv.Messages()
		if len(msgs) != 1 || msgs[0].Content != "x" {
			t.Fatalf("history = %+v, want only the user message", msgs)
		}
		attrs, ok := r.sink.endAttrs("model-call")
		if !ok {
			t.Fatal("the model-call span never ended")
		}
		if v, _ := attrs.Get("aborted"); v != true {
			t.Fatalf("model-call end attributes = %v, want aborted", attrs.Keys())
		}
		close(hang.release)
		synctest.Wait()
	})
}

// TestStream_NoRetryAfterAbort (R9): a failing tool is not retried once the run is
// stopped, and a backoff wait ends at the instant of the stop. Two cases: the stop
// arrives during the first attempt (no ToolRetry at all), and during the backoff
// (the ToolRetry was emitted before the wait started, but there is no second attempt
// and the wait does not run to its end).
func TestStream_NoRetryAfterAbort(t *testing.T) {
	t.Run("stop during the first attempt", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			flaky := newFakeTool("flaky_tool", func(context.Context, kernel.Arguments, toolhub.Context) (toolhub.Output, error) {
				time.Sleep(20 * time.Millisecond) // ignores ctx
				return toolhub.Output{Content: "transient failure", IsError: boolRef(true)}, nil
			})
			r := newRig(t, rigOptions{handlers: []toolhub.Handler{flaky}}, toolTurn(callOf("tc-1", "flaky_tool", "{}")))
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			time.AfterFunc(10*time.Millisecond, cancel)

			start := time.Now()
			chunks, err := drain(r.seq(ctx, "hi"))
			if err != nil {
				t.Fatal(err)
			}
			requireAbortedDone(t, chunks)
			if elapsed := time.Since(start); elapsed != 10*time.Millisecond {
				t.Fatalf("the stream ended after %v, want the instant of the stop (10ms)", elapsed)
			}
			time.Sleep(time.Second) // let the abandoned attempt finish: it must stay silent
			if flaky.executions() != 1 || countOf(r, events.ToolRetry.Name) != 0 {
				t.Fatalf("%d executions and %d ToolRetry events, want 1 and 0", flaky.executions(), countOf(r, events.ToolRetry.Name))
			}
		})
	})
	t.Run("stop during the backoff", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			flaky := failingTool("flaky_tool", 100)
			r := newRig(t, rigOptions{handlers: []toolhub.Handler{flaky}}, toolTurn(callOf("tc-1", "flaky_tool", "{}")))
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			time.AfterFunc(100*time.Millisecond, cancel) // inside the first 500 ms backoff

			start := time.Now()
			chunks, err := drain(r.seq(ctx, "hi"))
			if err != nil {
				t.Fatal(err)
			}
			requireAbortedDone(t, chunks)
			if elapsed := time.Since(start); elapsed != 100*time.Millisecond {
				t.Fatalf("the stream ended after %v, want the instant of the stop (100ms): the backoff must be cut short", elapsed)
			}
			if flaky.executions() != 1 {
				t.Fatalf("%d executions, want 1: no attempt after the stop", flaky.executions())
			}
			if got := countOf(r, events.ToolRetry.Name); got != 1 {
				t.Fatalf("%d ToolRetry events, want the one emitted before the wait began", got)
			}
		})
	})
}

// TestRun_DeadlineIsAbort: a ctx whose deadline passes stops the run exactly like a
// cancel (CONTEXT "What is an abort"): an aborted result and no error, at the instant
// of the deadline. context.Cause still tells the two apart.
func TestRun_DeadlineIsAbort(t *testing.T) {
	cases := []struct {
		name    string
		failure func(ctx context.Context) error // what the provider returns once ctx is done
	}{
		{"a provider that returns the ctx error", func(ctx context.Context) error { return ctx.Err() }},
		{"a provider that wraps the stop in its own error", func(context.Context) error { return errors.New("connection reset by peer") }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				r := newRig(t, rigOptions{})
				loop := r.withDeps(t, func(d *agentloop.Deps) { d.Models = ctxWaitingModels{ScriptedModels: r.models, failure: c.failure} })
				ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
				defer cancel()

				start := time.Now()
				res, err := loop.Run(ctx, "hi", "s-1", llmhub.ModelOptions{}, agentloop.RunOptions{})
				if err != nil || !res.Aborted {
					t.Fatalf("Run = %+v, %v, want an aborted result and no error", res, err)
				}
				if elapsed := time.Since(start); elapsed != 50*time.Millisecond {
					t.Fatalf("Run returned after %v, want the instant of the deadline (50ms)", elapsed)
				}
				if !errors.Is(context.Cause(ctx), context.DeadlineExceeded) {
					t.Fatalf("context.Cause = %v, want the deadline", context.Cause(ctx))
				}
			})
		})
	}
}

// ctxWaitingModels is a provider whose Chat waits for the ctx to be done and then
// fails with whatever failure says.
type ctxWaitingModels struct {
	*aecmock.ScriptedModels
	failure func(ctx context.Context) error
}

func (m ctxWaitingModels) Chat(ctx context.Context, _ []kernel.Message, _ llmhub.ModelOptions) (kernel.ModelResponse, error) {
	<-ctx.Done()
	return kernel.ModelResponse{}, m.failure(ctx)
}

// slowFailingModels fails after a delay with a provider timeout, on its own clock
// and not because of the run's ctx.
type slowFailingModels struct {
	*aecmock.ScriptedModels
	after time.Duration
	err   error
}

func (m slowFailingModels) Chat(context.Context, []kernel.Message, llmhub.ModelOptions) (kernel.ModelResponse, error) {
	time.Sleep(m.after)
	return kernel.ModelResponse{}, m.err
}

// TestRun_ProviderTimeoutIsModelFailure: a timeout inside the provider while the
// run's ctx is alive is a model failure, returned unchanged with the spans closed; it
// is not mistaken for a stop.
func TestRun_ProviderTimeoutIsModelFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newRig(t, rigOptions{})
		want := kernel.NewModelError("the provider timed out", kernel.ModelCodeTimeout, true)
		loop := r.withDeps(t, func(d *agentloop.Deps) {
			d.Models = slowFailingModels{ScriptedModels: r.models, after: 30 * time.Second, err: want}
		})
		res, err := loop.Run(t.Context(), "hi", "s-1", llmhub.ModelOptions{}, agentloop.RunOptions{})
		var me *kernel.ModelError
		if !errors.As(err, &me) || me != want || res.Aborted {
			t.Fatalf("Run = %+v, %v, want the provider's *kernel.ModelError and no aborted result", res, err)
		}
		if started, ended := r.sink.startedAndEnded(); started != ended {
			t.Fatalf("spans started %d, ended %d", started, ended)
		}
		attrs, _ := r.sink.endAttrs("agent-run")
		if _, ok := attrs.Get("error"); !ok {
			t.Fatalf("agent-run end attributes = %v, want the error", attrs.Keys())
		}
	})
}

// TestPersistTimeoutBoundsBlockedWrite: a conversation whose write blocks until its
// ctx ends cannot hang a run. The write is cut off after exactly the persist timeout
// (10 s by default, WithPersistTimeout otherwise) in fake time, and the run reports
// context.DeadlineExceeded: as the error of Run, and once in the error slot of Stream.
func TestPersistTimeoutBoundsBlockedWrite(t *testing.T) {
	t.Run("Run with the default timeout", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			r := newRig(t, rigOptions{}, aecmock.ScriptedTurn{Content: "never asked"})
			loop := r.withDeps(t, func(d *agentloop.Deps) { d.Conversation = hangingConversation{Conversation: r.conv} })
			start := time.Now()
			_, err := loop.Run(t.Context(), "hi", "s-1", llmhub.ModelOptions{}, agentloop.RunOptions{})
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("Run error = %v, want the persist deadline", err)
			}
			if got := time.Since(start); got != 10*time.Second {
				t.Fatalf("the write was cut off after %v, want exactly 10s", got)
			}
		})
	})
	t.Run("Run with WithPersistTimeout", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			r := newRig(t, rigOptions{}, aecmock.ScriptedTurn{Content: "never asked"})
			loop := r.withDeps(t, func(d *agentloop.Deps) { d.Conversation = hangingConversation{Conversation: r.conv} }, agentloop.WithPersistTimeout(2*time.Second))
			start := time.Now()
			_, err := loop.Run(t.Context(), "hi", "s-1", llmhub.ModelOptions{}, agentloop.RunOptions{})
			if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) != 2*time.Second {
				t.Fatalf("Run error = %v after %v, want the deadline after exactly 2s", err, time.Since(start))
			}
		})
	})
	t.Run("Stream reports it once in the error slot", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			r := newRig(t, rigOptions{}, aecmock.ScriptedTurn{Content: "never asked"})
			loop := r.withDeps(t, func(d *agentloop.Deps) { d.Conversation = hangingConversation{Conversation: r.conv} })
			start := time.Now()
			yields := 0
			var got error
			for _, err := range loop.Stream(t.Context(), "hi", "s-1", llmhub.ModelOptions{}, agentloop.RunOptions{}) {
				yields++
				got = err
			}
			if yields != 1 || !errors.Is(got, context.DeadlineExceeded) || time.Since(start) != 10*time.Second {
				t.Fatalf("%d yields, error %v after %v, want one deadline error after exactly 10s", yields, got, time.Since(start))
			}
		})
	})
	t.Run("a write on the abort path still lands or times out", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			r := newRig(t, rigOptions{}, aecmock.ScriptedTurn{Chunks: []kernel.StreamChunk{textChunk("Hel"), textChunk("lo"), textChunk("!"), {Type: kernel.ChunkDone}}})
			// The user message is stored; the write of the partial reply blocks.
			loop := r.withDeps(t, func(d *agentloop.Deps) { d.Conversation = &hangAfterConversation{Conversation: r.conv, after: 1} })
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var stopAt time.Time
			texts := 0
			var got error
			var chunks []kernel.StreamChunk
			for c, err := range loop.Stream(ctx, "hi", "s-1", llmhub.ModelOptions{}, agentloop.RunOptions{}) {
				if err != nil {
					got = err
					break
				}
				chunks = append(chunks, c)
				if c.Type == kernel.ChunkText {
					texts++
					if texts == 2 {
						stopAt = time.Now()
						cancel()
					}
				}
			}
			if !errors.Is(got, context.DeadlineExceeded) || time.Since(stopAt) != 10*time.Second {
				t.Fatalf("error %v after %v, want the persist deadline after exactly 10s", got, time.Since(stopAt))
			}
			for _, c := range chunks {
				if c.Type == kernel.ChunkDone {
					t.Fatalf("a done chunk was yielded although the cleanup failed: %+v", chunks)
				}
			}
			// The cleanup finished anyway: the spans are closed and RunCompleted was emitted.
			if started, ended := r.sink.startedAndEnded(); started != ended {
				t.Fatalf("spans started %d, ended %d", started, ended)
			}
			if countOf(r, events.RunCompleted.Name) != 1 {
				t.Fatalf("event types = %v, want one RunCompleted", r.log.types())
			}
		})
	})
}

// TestAToolThatIgnoresCtxDoesNotDelayAbort: a tool that blocks on a channel and
// never looks at ctx does not delay the stop: Run returns at the instant of the
// cancel, and the abandoned goroutine can finish once the tool is released (the
// result channel is buffered, so it never blocks on the send).
func TestAToolThatIgnoresCtxDoesNotDelayAbort(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		stuck := newFakeTool("stuck_tool", func(context.Context, kernel.Arguments, toolhub.Context) (toolhub.Output, error) {
			<-release
			return toolhub.Output{Content: "late"}, nil
		})
		r := newRig(t, rigOptions{handlers: []toolhub.Handler{stuck}}, toolTurn(callOf("tc-1", "stuck_tool", "{}")))
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		time.AfterFunc(30*time.Millisecond, cancel)

		start := time.Now()
		res, err := r.loop.Run(ctx, "hi", "s-1", llmhub.ModelOptions{}, agentloop.RunOptions{})
		if err != nil || !res.Aborted {
			t.Fatalf("Run = %+v, %v, want an aborted result and no error", res, err)
		}
		if elapsed := time.Since(start); elapsed != 30*time.Millisecond {
			t.Fatalf("Run returned after %v, want the instant of the cancel (30ms)", elapsed)
		}
		results := resultsOf(r)
		if len(results) != 1 || results[0].Content != "Cancelled: the user stopped the run before this tool call finished." {
			t.Fatalf("results = %+v, want the cancelled result", results)
		}
		close(release)
		synctest.Wait() // every goroutine must be able to end, or the bubble deadlocks
	})
}

// TestRun_AbortDuringToolsReturnsAnAbortedResult: Run stopped while a tool runs
// persists the cancelled results message and then returns the aborted result at the
// next safe point, with the iteration it was in.
func TestRun_AbortDuringToolsReturnsAnAbortedResult(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		slow := newFakeTool("slow_tool", func(context.Context, kernel.Arguments, toolhub.Context) (toolhub.Output, error) {
			cancel()
			<-release
			return toolhub.Output{}, nil
		})
		r := newRig(t, rigOptions{handlers: []toolhub.Handler{slow}}, toolTurn(callOf("tc-1", "slow_tool", "{}")), aecmock.ScriptedTurn{Content: "never"})
		res, err := r.loop.Run(ctx, "hi", "s-1", llmhub.ModelOptions{}, agentloop.RunOptions{})
		if err != nil || !res.Aborted || res.Iterations != 1 || res.Message.Content != "" {
			t.Fatalf("Run = %+v, %v, want an aborted result in iteration 1", res, err)
		}
		if got := len(r.conv.Messages()); got != 3 {
			t.Fatalf("%d messages, want 3 (user, call, cancelled result)", got)
		}
		if got := len(r.models.Calls()); got != 1 {
			t.Fatalf("the model was called %d times, want 1: the next iteration never started", got)
		}
		if !slices.Contains(r.log.types(), events.RunCompleted.Name) {
			t.Fatalf("event types = %v, want RunCompleted", r.log.types())
		}
		close(release)
		synctest.Wait()
	})
}

// lastIterationStopRig builds a rig whose cap is one iteration and whose only tool
// cancels the returned cancel func while it runs and then blocks until release is
// called: a stop that arrives while the last allowed iteration's tools run.
// release is idempotent and is also registered with t.Cleanup, so the tool goroutine is
// released on every exit path (a failing assertion reports itself instead of leaving
// the synctest bubble deadlocked on the blocked tool).
func lastIterationStopRig(t *testing.T) (r *rig, ctx context.Context, release func()) {
	t.Helper()
	releaseCh := make(chan struct{})
	var once sync.Once
	release = func() { once.Do(func() { close(releaseCh) }) }
	t.Cleanup(release)
	var cancel context.CancelFunc
	ctx, cancel = context.WithCancel(t.Context())
	t.Cleanup(cancel)
	stopper := newFakeTool("stopper_tool", func(context.Context, kernel.Arguments, toolhub.Context) (toolhub.Output, error) {
		cancel()
		<-releaseCh
		return toolhub.Output{}, nil
	})
	p := testProfile("test-agent")
	p.LoopConfig = &profile.LoopConfig{MaxIterations: intPtr(1)}
	r = newRig(t, rigOptions{prof: &p, handlers: []toolhub.Handler{stopper}}, toolTurn(callOf("tc-1", "stopper_tool", "{}")), aecmock.ScriptedTurn{Content: "never"})
	return r, ctx, release
}

// TestRun_StopDuringLastIterationToolsReturnsAborted: a stop that arrives while the
// last allowed iteration's tools run is a stop, not the cap: Run checks ctx before
// the max-iterations close and returns the aborted result (Aborted true, no error)
// with RunCompleted{aborted}, not the max-iterations fallback with
// MaxIterationsReached.
func TestRun_StopDuringLastIterationToolsReturnsAborted(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r, ctx, release := lastIterationStopRig(t)
		res, err := r.loop.Run(ctx, "hi", "s-1", llmhub.ModelOptions{}, agentloop.RunOptions{})
		if err != nil {
			t.Fatalf("Run error = %v, want nil", err)
		}
		if !res.Aborted || res.Iterations != 1 || res.Message.Content != "" {
			t.Fatalf("Run = %+v, want the aborted result of iteration 1", res)
		}
		if res.StopReason != kernel.StopToolUse {
			t.Fatalf("StopReason = %q, want tool_use (the last response)", res.StopReason)
		}
		if ctx.Err() == nil {
			t.Fatal("the ctx was not cancelled: the test proves nothing")
		}
		if countOf(r, events.MaxIterationsReached.Name) != 0 || countOf(r, events.RunCompleted.Name) != 1 {
			t.Fatalf("event types = %v, want RunCompleted and no MaxIterationsReached", r.log.types())
		}
		if got := r.log.payload(t, events.RunCompleted.Name); !strings.Contains(got, `"aborted":true`) {
			t.Fatalf("RunCompleted = %s, want aborted", got)
		}
		if got := len(r.models.Calls()); got != 1 {
			t.Fatalf("the model was called %d times, want 1", got)
		}
		if started, ended := r.sink.startedAndEnded(); started != ended {
			t.Fatalf("spans started %d, ended %d", started, ended)
		}
		release()
		synctest.Wait()
	})
}

// TestStream_StopDuringLastIterationToolsEndsAborted is the same case for Stream. The
// same stop ends the stream with the done chunk marked aborted, and never with the
// "Reached maximum reasoning steps." error chunk.
func TestStream_StopDuringLastIterationToolsEndsAborted(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r, ctx, release := lastIterationStopRig(t)
		chunks, err := drain(r.loop.Stream(ctx, "hi", "s-1", llmhub.ModelOptions{}, agentloop.RunOptions{}))
		if err != nil {
			t.Fatal(err)
		}
		if len(chunks) == 0 {
			t.Fatal("no chunks")
		}
		last := chunks[len(chunks)-1]
		if last.Type != kernel.ChunkDone || !last.Aborted {
			t.Fatalf("last chunk = %+v, want the aborted done chunk (chunk types %v)", last, chunkTypes(chunks))
		}
		for _, c := range chunks {
			if c.Type == kernel.ChunkError {
				t.Fatalf("chunk types = %v, want no max-steps error chunk", chunkTypes(chunks))
			}
		}
		if ctx.Err() == nil {
			t.Fatal("the ctx was not cancelled: the test proves nothing")
		}
		if countOf(r, events.MaxIterationsReached.Name) != 0 || countOf(r, events.RunCompleted.Name) != 1 {
			t.Fatalf("event types = %v, want RunCompleted and no MaxIterationsReached", r.log.types())
		}
		if started, ended := r.sink.startedAndEnded(); started != ended {
			t.Fatalf("spans started %d, ended %d", started, ended)
		}
		release()
		synctest.Wait()
	})
}

// TestStream_AbortWritesRunUnderALiveCtx: the writes made on an abort path (the user
// message, the partial reply) never see the caller's cancellation: each runs under a
// ctx that is not done, so a stop can never lose them. It is the Stream counterpart of
// TestRun_PersistsUnderCancelledCtx.
func TestStream_AbortWritesRunUnderALiveCtx(t *testing.T) {
	r := newRig(t, rigOptions{}, aecmock.ScriptedTurn{Chunks: []kernel.StreamChunk{
		textChunk("Hel"), textChunk("lo"), textChunk("!"), {Type: kernel.ChunkDone},
	}})
	wrapped := &writeCtxConversation{Conversation: r.conv}
	loop := r.withDeps(t, func(d *agentloop.Deps) { d.Conversation = wrapped })
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	texts := 0
	chunks, err := drainEach(loop.Stream(ctx, "hi", "s-1", llmhub.ModelOptions{}, agentloop.RunOptions{}), func(c kernel.StreamChunk) {
		if c.Type == kernel.ChunkText {
			texts++
			if texts == 2 {
				cancel()
			}
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	requireAbortedDone(t, chunks)
	if ctx.Err() == nil {
		t.Fatal("the caller ctx was not cancelled: the test proves nothing")
	}
	if wrapped.n != 2 {
		t.Fatalf("%d conversation writes, want 2 (user message, partial reply)", wrapped.n)
	}
	for i, e := range wrapped.errs {
		if e != nil {
			t.Fatalf("write %d ran under a done ctx: %v", i, e)
		}
	}
	if msgs := r.conv.Messages(); len(msgs) != 2 || msgs[1].Content != "Hello" {
		t.Fatalf("history = %+v, want the partial reply stored", msgs)
	}
}

// cancelAfterStream is a model that cancels the run's ctx right after its stream
// has delivered every chunk, including the done chunk that carries the usage. The
// loop has then recorded the usage and is about to write the message, so the stop
// lands between the usage record and the message write. It embeds the concrete
// ScriptedModels so the tracing-only interfaces stay visible to the loop.
type cancelAfterStream struct {
	*aecmock.ScriptedModels
	cancel context.CancelFunc
}

func (c cancelAfterStream) Stream(ctx context.Context, msgs []kernel.Message, opts llmhub.ModelOptions) iter.Seq2[kernel.StreamChunk, error] {
	inner := c.ScriptedModels.Stream(ctx, msgs, opts)
	return func(yield func(kernel.StreamChunk, error) bool) {
		for chunk, err := range inner {
			if !yield(chunk, err) {
				return
			}
		}
		c.cancel()
	}
}

// TestAbort_PersistsUsageWithoutPartialText (IN-03): a stream turn whose done chunk
// carries usage and that is stopped before the assistant message is written, with no
// streamed text, still leaves the usage on the stored session. Before the fix the
// session write was tied to the partial reply, so the recorded usage was lost.
func TestAbort_PersistsUsageWithoutPartialText(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		call := callOf("tc-1", "ghost_tool", "{}")
		usage := kernel.TokenUsage{InputTokens: 7, OutputTokens: 3}
		r := newRig(t, rigOptions{}, aecmock.ScriptedTurn{Chunks: []kernel.StreamChunk{
			{Type: kernel.ChunkToolCall, ToolCall: &call},
			{Type: kernel.ChunkDone, Usage: &usage},
		}})
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		loop := r.withDeps(t, func(d *agentloop.Deps) {
			d.Models = cancelAfterStream{ScriptedModels: r.models, cancel: cancel}
		})

		chunks, err := drainEach(loop.Stream(ctx, "hi", "s-1", llmhub.ModelOptions{}, agentloop.RunOptions{}), func(kernel.StreamChunk) {})
		if err != nil {
			t.Fatal(err)
		}
		requireAbortedDone(t, chunks)

		stored, err := r.hub.Sessions.Get(t.Context(), "s-1")
		if err != nil || stored == nil {
			t.Fatalf("Sessions.Get = %v, %v", stored, err)
		}
		if got := stored.CurrentTokenUsage; got == nil || got.InputTokens != 7 || got.OutputTokens != 3 {
			t.Fatalf("stored CurrentTokenUsage = %+v, want 7 input and 3 output", got)
		}
		if stored.CounterCalibration == nil {
			t.Fatal("the counter calibration was not stored with the usage")
		}
		// Nothing but the user message was persisted: no partial text, and the collected
		// tool call is never saved.
		if msgs := r.conv.Messages(); len(msgs) != 1 || msgs[0].Role != kernel.RoleUser {
			t.Fatalf("history = %+v, want only the user message", msgs)
		}
	})
}

// failAssistantWrites is a conversation whose AddMessage fails for an assistant
// message, and that counts the UpdateSession calls it forwards.
type failAssistantWrites struct {
	agentloop.Conversation
	updates atomic.Int32
}

func (f *failAssistantWrites) AddMessage(ctx context.Context, m kernel.Message) error {
	if m.Role == kernel.RoleAssistant {
		return errors.New("store is down")
	}
	return f.Conversation.AddMessage(ctx, m)
}

func (f *failAssistantWrites) UpdateSession(ctx context.Context, s kernel.Session) error {
	f.updates.Add(1)
	return f.Conversation.UpdateSession(ctx, s)
}

// TestAbort_PersistsUsageWhenThePartialReplyWriteFails (IN-B01): the partial reply and
// the session usage are independent writes; a failing reply write must not skip the
// usage write that IN-03 added.
func TestAbort_PersistsUsageWhenThePartialReplyWriteFails(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		partial := "partial answer"
		call := callOf("tc-1", "ghost_tool", "{}")
		usage := kernel.TokenUsage{InputTokens: 7, OutputTokens: 3}
		r := newRig(t, rigOptions{}, aecmock.ScriptedTurn{Chunks: []kernel.StreamChunk{
			{Type: kernel.ChunkText, Content: &partial},
			{Type: kernel.ChunkToolCall, ToolCall: &call},
			{Type: kernel.ChunkDone, Usage: &usage},
		}})
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		conv := &failAssistantWrites{Conversation: r.conv}
		loop := r.withDeps(t, func(d *agentloop.Deps) {
			d.Models = cancelAfterStream{ScriptedModels: r.models, cancel: cancel}
			d.Conversation = conv
		})

		_, _ = drainEach(loop.Stream(ctx, "hi", "s-1", llmhub.ModelOptions{}, agentloop.RunOptions{}), func(kernel.StreamChunk) {})

		if conv.updates.Load() == 0 {
			t.Fatal("the usage write was skipped after the partial reply write failed")
		}
		stored, err := r.hub.Sessions.Get(t.Context(), "s-1")
		if err != nil || stored == nil {
			t.Fatalf("Sessions.Get = %v, %v", stored, err)
		}
		if got := stored.CurrentTokenUsage; got == nil || got.InputTokens != 7 || got.OutputTokens != 3 {
			t.Fatalf("stored CurrentTokenUsage = %+v, want 7 input and 3 output", got)
		}
	})
}

// The kept result of a tool that finished before a stop.

// lateDoneCtx is a ctx whose stop is seen by Err before its Done channel closes, so
// a test can stop the run right after a tool returned, before the dispatch race looks
// at Done: the window in which a finished call could wrongly be answered as
// cancelled. Done closes when the test releases it.
type lateDoneCtx struct {
	context.Context
	stopped atomic.Bool
	done    chan struct{}
	once    sync.Once
}

func newLateDoneCtx(parent context.Context) *lateDoneCtx {
	return &lateDoneCtx{Context: parent, done: make(chan struct{})}
}

func (c *lateDoneCtx) Done() <-chan struct{} { return c.done }

func (c *lateDoneCtx) Err() error {
	if c.stopped.Load() {
		return context.Canceled
	}
	return nil
}

func (c *lateDoneCtx) release() { c.once.Do(func() { close(c.done) }) }

// TestRun_ToolThatFinishedBeforeTheStopKeepsItsResult: a tool that returned its result
// before the stop was seen is reported as it ran (its real result in the history,
// ToolCompleted), and the run then ends as aborted at the next safe point. Replacing
// the result with the cancelled text would let the model repeat an action that already
// happened.
func TestRun_ToolThatFinishedBeforeTheStopKeepsItsResult(t *testing.T) {
	for _, mode := range []string{"run", "stream"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx := newLateDoneCtx(t.Context())
				defer ctx.release()
				send := newFakeTool("send_rfi", func(context.Context, kernel.Arguments, toolhub.Context) (toolhub.Output, error) {
					ctx.stopped.Store(true) // the stop arrives as the tool returns
					return toolhub.Output{Content: "RFI-42 sent"}, nil
				})
				rg := newRig(t, rigOptions{handlers: []toolhub.Handler{send}},
					toolTurn(callOf("tc-1", "send_rfi", "{}")), aecmock.ScriptedTurn{Content: "never asked"})
				var aborted bool
				switch mode {
				case "run":
					res, err := rg.loop.Run(ctx, "send it", "s-1", llmhub.ModelOptions{}, agentloop.RunOptions{})
					if err != nil {
						t.Fatal(err)
					}
					aborted = res.Aborted
				case "stream":
					chunks, err := drain(rg.loop.Stream(ctx, "send it", "s-1", llmhub.ModelOptions{}, agentloop.RunOptions{}))
					if err != nil {
						t.Fatal(err)
					}
					last := chunks[len(chunks)-1]
					aborted = last.Type == kernel.ChunkDone && last.Aborted
					var streamed []string
					for _, c := range chunks {
						if c.Type == kernel.ChunkToolResult {
							streamed = append(streamed, c.ToolResult.Content)
						}
					}
					if len(streamed) != 1 || streamed[0] != "RFI-42 sent" {
						t.Fatalf("tool_result chunks = %v, want the real result", streamed)
					}
				}
				if !aborted {
					t.Fatal("the run was not reported as aborted")
				}
				if send.executions() != 1 {
					t.Fatalf("%d executions, want 1", send.executions())
				}
				if results := resultsOf(rg); len(results) != 1 || results[0].Content != "RFI-42 sent" {
					t.Fatalf("persisted results = %+v, want the real result, not the cancelled text", results)
				}
				if countOf(rg, events.ToolCompleted.Name) != 1 || countOf(rg, events.ToolFailed.Name) != 0 {
					t.Fatalf("events = %v, want ToolCompleted and no ToolFailed", rg.log.types())
				}
				if got := len(rg.models.Calls()); got != 1 {
					t.Fatalf("the model was asked %d times after the stop, want 1 call in all", got)
				}
			})
		})
	}
}
