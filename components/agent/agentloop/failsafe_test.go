package agentloop_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"math"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Phong95/aec-agent-go/aecmock"
	"github.com/Phong95/aec-agent-go/components/agent/agentloop"
	"github.com/Phong95/aec-agent-go/components/agent/memorysystem/memory"
	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/kernel"
)

// This file holds the regression tests of the 09 code review findings WR-01 to WR-05
// (approval on re-authorization, one sandbox resolution, ports that ignore ctx,
// panic cleanup in Run, backoff overflow).

// failFirstTools is a tools port whose first Authorize fails with err; every later
// call goes to the wrapped port. It models an adapter with a transient error, or a
// tool that is registered during the backoff.
type failFirstTools struct {
	agentloop.Tools
	err   error
	calls atomic.Int32
}

func (f *failFirstTools) Authorize(ctx context.Context, name string, args kernel.Arguments, tc toolhub.Context) (toolhub.Authorized, toolhub.Decision, error) {
	if f.calls.Add(1) == 1 {
		return toolhub.Authorized{}, toolhub.Decision{}, f.err
	}
	return f.Tools.Authorize(ctx, name, args, tc)
}

func failFirst(err error) func(agentloop.Tools) agentloop.Tools {
	return func(inner agentloop.Tools) agentloop.Tools { return &failFirstTools{Tools: inner, err: err} }
}

// TestApproval_ReauthorizationAfterAFailedFirstAuthorizeStillGates (WR-01): the first
// Authorize of a consequential call fails (a transient adapter error), so the approval
// gate had nothing to judge. The retry authorizes successfully, and the gate must run
// before the tool executes, whatever the answer. A tool that is not known at first
// is not retried: TestApproval_NotFoundAtFirstIsFinal.
func TestApproval_ReauthorizationAfterAFailedFirstAuthorizeStillGates(t *testing.T) {
	errs := map[string]error{
		"transient adapter error": errors.New("adapter unavailable"),
	}
	for name, firstErr := range errs {
		t.Run(name+"/approved", func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				send := consequentialTool("send_rfi")
				handler, reqs := recordingHandler(approve())
				rg := newRig(t, rigOptions{handlers: []toolhub.Handler{send}, interaction: handler, wrapTools: failFirst(firstErr)},
					toolTurn(callOf("tc-1", "send_rfi", `{"to":"arch@example.com"}`)), aecmock.ScriptedTurn{Content: "Sent."})
				if _, err := rg.run(t, "go"); err != nil {
					t.Fatal(err)
				}
				if len(*reqs) != 1 || (*reqs)[0].ToolCallID != "tc-1" {
					t.Fatalf("approval requests = %+v, want exactly one, asked before the retry executed", *reqs)
				}
				if send.executions() != 1 {
					t.Fatalf("%d executions, want 1 (after the approval)", send.executions())
				}
				if got := countOf(rg, events.ToolApprovalRequested.Name); got != 1 {
					t.Fatalf("%d ToolApprovalRequested events, want 1", got)
				}
				if results := resultsOf(rg); len(results) != 1 || results[0].Content != "sent" {
					t.Fatalf("results = %+v", results)
				}
			})
		})

		t.Run(name+"/declined", func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				send := consequentialTool("send_rfi")
				handler, _ := recordingHandler(func(context.Context, toolhub.ApprovalRequest) (toolhub.ApprovalDecision, error) {
					return toolhub.ApprovalDecision{Approved: false, Reason: strRef("no")}, nil
				})
				rg := newRig(t, rigOptions{handlers: []toolhub.Handler{send}, interaction: handler, wrapTools: failFirst(firstErr)},
					toolTurn(callOf("tc-1", "send_rfi", "{}")), aecmock.ScriptedTurn{Content: "ok"})
				if _, err := rg.run(t, "go"); err != nil {
					t.Fatal(err)
				}
				if send.executions() != 0 {
					t.Fatal("a consequential tool ran on the retry although the user declined")
				}
				want := "Approval denied: user declined (no)" + doNotRetry
				if results := resultsOf(rg); len(results) != 1 || results[0].Content != want || results[0].IsError == nil || !*results[0].IsError {
					t.Fatalf("results = %+v, want %q", results, want)
				}
				if countOf(rg, events.ToolCompleted.Name) != 0 || countOf(rg, events.ToolRetry.Name) != 1 {
					t.Fatalf("events = %v: one retry (the failed first authorization), then a final denial", rg.log.types())
				}
			})
		})

		t.Run(name+"/no handler", func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				send := consequentialTool("send_rfi")
				rg := newRig(t, rigOptions{handlers: []toolhub.Handler{send}, wrapTools: failFirst(firstErr)},
					toolTurn(callOf("tc-1", "send_rfi", "{}")), aecmock.ScriptedTurn{Content: "ok"})
				if _, err := rg.run(t, "go"); err != nil {
					t.Fatal(err)
				}
				if send.executions() != 0 {
					t.Fatal("a consequential tool ran on the retry with nobody to approve it")
				}
				want := "Approval denied: no interactive user is available to approve this consequential action" + doNotRetry
				if results := resultsOf(rg); len(results) != 1 || results[0].Content != want {
					t.Fatalf("results = %+v, want %q", results, want)
				}
			})
		})
	}
}

// TestApproval_ApprovalIsNotAskedAgainOnRetry: once the user approved a call, the
// retries of that call (re-authorizations) keep the approval. The tool is a draft
// that its host sandbox gates: a consequential tool is never retried once it ran
// (TestRetry_ConsequentialToolIsNeverRetried).
func TestApproval_ApprovalIsNotAskedAgainOnRetry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		flaky := failingTool("flaky", 2)
		flaky.def.Risk = toolhub.RiskDraft
		flaky.def.SandboxOperation = sandboxhub.OperationFSWrite
		handler, reqs := recordingHandler(approve())
		rg := newRig(t, rigOptions{handlers: []toolhub.Handler{flaky}, interaction: handler, sandboxes: newFakeSandboxes(t, sandboxhub.KindHost, "host-1")},
			toolTurn(callOf("tc-1", "flaky", "{}")), aecmock.ScriptedTurn{Content: "ok"})
		if _, err := rg.run(t, "go"); err != nil {
			t.Fatal(err)
		}
		if flaky.executions() != 3 || len(*reqs) != 1 {
			t.Fatalf("%d executions, %d approval requests, want 3 and 1", flaky.executions(), len(*reqs))
		}
	})
}

// TestApproval_NotFoundAtFirstIsFinal: a first Authorize that
// does not find the tool is answered at once with the not-available result. Nothing
// is retried, so the tool registered later neither runs nor asks for approval.
func TestApproval_NotFoundAtFirstIsFinal(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		send := consequentialTool("send_rfi")
		handler, reqs := recordingHandler(approve())
		rg := newRig(t, rigOptions{handlers: []toolhub.Handler{send}, interaction: handler, wrapTools: failFirst(fmt.Errorf("registered later: %w", toolhub.ErrNotFound))},
			toolTurn(callOf("tc-1", "send_rfi", "{}")), aecmock.ScriptedTurn{Content: "ok"})
		if _, err := rg.run(t, "go"); err != nil {
			t.Fatal(err)
		}
		if send.executions() != 0 || len(*reqs) != 0 {
			t.Fatalf("%d executions, %d approval requests, want none", send.executions(), len(*reqs))
		}
		want := `Tool "send_rfi" not available for this agent`
		if results := resultsOf(rg); len(results) != 1 || results[0].Content != want {
			t.Fatalf("results = %+v, want %q", results, want)
		}
		if countOf(rg, events.ToolRetry.Name) != 0 || countOf(rg, events.ToolCompleted.Name) != 1 {
			t.Fatalf("events = %v, want no retry and one ToolCompleted", rg.log.types())
		}
	})
}

// panicSandboxes is a sandbox access whose resolver panics.
type panicSandboxes struct{ *fakeSandboxes }

func (panicSandboxes) GetSandbox(context.Context) (sandboxhub.View, error) { panic("resolver blew up") }

// TestExecuteOne_PanicOutsideTheToolClosesTheDispatchSpan (IN-04): a port that panics
// after the dispatch span opened (here the sandbox resolver, called by the approval
// gate) answers the call with an error result, emits the failed event and ends the
// span, so no span stays open on a successful turn.
func TestExecuteOne_PanicOutsideTheToolClosesTheDispatchSpan(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		write := sandboxTool("write_note", sandboxhub.OperationFSWrite, nil)
		quiet := slog.New(slog.NewTextHandler(io.Discard, nil)) // the panic is logged with its stack
		rg := newRig(t, rigOptions{handlers: []toolhub.Handler{write}, sandboxes: panicSandboxes{newFakeSandboxes(t, sandboxhub.KindHost, "host-1")}, loopOpts: []agentloop.Option{agentloop.WithLogger(quiet)}},
			toolTurn(callOf("tc-1", "write_note", "{}")), aecmock.ScriptedTurn{Content: "ok"})
		if _, err := rg.run(t, "go"); err != nil {
			t.Fatal(err)
		}
		// The panic value never reaches the model, the event or the span: the generic
		// text does.
		results := resultsOf(rg)
		if len(results) != 1 || results[0].Content != "tool write_note failed: internal error" || results[0].IsError == nil || !*results[0].IsError {
			t.Fatalf("results = %+v", results)
		}
		if write.executions() != 0 {
			t.Fatal("the tool ran although the gate panicked")
		}
		if got := rg.log.payload(t, events.ToolFailed.Name); got != `{"agentId":"test-agent","name":"write_note","error":"tool write_note failed: internal error"}` {
			t.Fatalf("ToolFailed = %s", got)
		}
		attrs, ok := rg.sink.endAttrs("write_note")
		if !ok {
			t.Fatal("the dispatch span never ended")
		}
		if v, _ := attrs.Get("error"); v != "tool write_note failed: internal error" {
			t.Fatalf("dispatch span end = %v", attrs.Keys())
		}
		if started, ended := rg.sink.startedAndEnded(); started != ended {
			t.Fatalf("%d spans started, %d ended", started, ended)
		}
	})
}

// blockingMemory is a memory system that ignores ctx until released.
type blockingMemory struct{ release chan struct{} }

func (m blockingMemory) RecallForTurn(context.Context, string, string) (memory.RecalledMemories, error) {
	<-m.release
	return memory.RecalledMemories{}, nil
}

// slowContext is a context pipeline whose steps ignore ctx until released.
type slowContext struct {
	aecmock.PassthroughContext
	prepare, process chan struct{}
}

func (c slowContext) PrepareMessages(ctx context.Context, msgs []kernel.Message, mc agentloop.MiddlewareContext) ([]kernel.Message, error) {
	if c.prepare != nil {
		<-c.prepare
	}
	return c.PassthroughContext.PrepareMessages(ctx, msgs, mc)
}

func (c slowContext) ProcessResponse(ctx context.Context, resp kernel.ModelResponse, mc agentloop.MiddlewareContext) (kernel.ModelResponse, error) {
	if c.process != nil {
		<-c.process
	}
	return c.PassthroughContext.ProcessResponse(ctx, resp, mc)
}

// TestStop_PortsThatIgnoreCtxDoNotDelayIt (WR-03): the memory recall, the context
// middlewares before and after the model call and the compaction summary are raced
// against ctx, so a stop at 100 ms returns the aborted result at 100 ms, with every
// span closed, although the port keeps blocking.
func TestStop_PortsThatIgnoreCtxDoNotDelayIt(t *testing.T) {
	cases := []struct {
		name  string
		opts  func(release chan struct{}) rigOptions
		turns []aecmock.ScriptedTurn
		modes []string
	}{
		{"memory recall", func(release chan struct{}) rigOptions {
			return rigOptions{memory: blockingMemory{release: release}}
		}, []aecmock.ScriptedTurn{{Content: "unreachable"}}, []string{"run", "stream"}},
		{"PrepareMessages", func(release chan struct{}) rigOptions {
			return rigOptions{context: slowContext{prepare: release}}
		}, []aecmock.ScriptedTurn{{Content: "unreachable"}}, []string{"run", "stream"}},
		// Stream never post-processes the response.
		{"ProcessResponse", func(release chan struct{}) rigOptions {
			return rigOptions{context: slowContext{process: release}}
		}, []aecmock.ScriptedTurn{{Content: "unreachable"}}, []string{"run"}},
	}
	for _, tc := range cases {
		for _, mode := range tc.modes {
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					release := make(chan struct{})
					rg := newRig(t, tc.opts(release), tc.turns...)
					ctx, cancel := context.WithCancel(t.Context())
					defer cancel()
					time.AfterFunc(100*time.Millisecond, cancel)
					start := time.Now()

					var aborted bool
					if mode == "run" {
						res, err := rg.loop.Run(ctx, "go", "s-1", llmhub.ModelOptions{}, agentloop.RunOptions{})
						if err != nil {
							t.Fatalf("Run: %v", err)
						}
						aborted = res.Aborted
					} else {
						chunks, err := drain(rg.seq(ctx, "go"))
						if err != nil {
							t.Fatalf("Stream: %v", err)
						}
						aborted = len(chunks) > 0 && chunks[len(chunks)-1].Type == kernel.ChunkDone && chunks[len(chunks)-1].Aborted
					}
					if !aborted {
						t.Fatal("the stopped run did not report an abort")
					}
					if elapsed := time.Since(start); elapsed != 100*time.Millisecond {
						t.Fatalf("returned after %v, want the instant of the stop (100 ms)", elapsed)
					}
					if started, ended := rg.sink.startedAndEnded(); started != ended {
						t.Fatalf("%d spans started, %d ended", started, ended)
					}
					if got := countOf(rg, events.RunCompleted.Name); got != 1 {
						t.Fatalf("%d RunCompleted events, want 1 (aborted)", got)
					}
					close(release) // the abandoned call can finish: nothing leaks
					synctest.Wait()
				})
			})
		}
	}
}

// hangingChatModels is a models port whose Chat ignores ctx until released.
type hangingChatModels struct{ release chan struct{} }

func (m hangingChatModels) Chat(context.Context, []kernel.Message, llmhub.ModelOptions) (kernel.ModelResponse, error) {
	<-m.release
	return kernel.ModelResponse{}, nil
}

func (hangingChatModels) Stream(context.Context, []kernel.Message, llmhub.ModelOptions) iter.Seq2[kernel.StreamChunk, error] {
	return func(func(kernel.StreamChunk, error) bool) {}
}

// TestSummarizer_StopDoesNotWaitForTheModel (WR-03, IN-07): the summary call is raced
// against ctx, so a stop returns at once with ctx.Err() although the model ignores
// ctx, and the compactor treats it as a stop, not as a failure to warn about.
func TestSummarizer_StopDoesNotWaitForTheModel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		time.AfterFunc(80*time.Millisecond, cancel)
		start := time.Now()
		_, err := agentloop.Summarize(ctx, hangingChatModels{release: release}, []kernel.Message{{Role: kernel.RoleUser, Content: "hi"}}, 100)
		if !errors.Is(err, context.Canceled) || time.Since(start) != 80*time.Millisecond {
			t.Fatalf("err %v after %v, want context.Canceled at 80 ms", err, time.Since(start))
		}
		close(release)
		synctest.Wait()
	})
}

// TestCompactor_StoppedSummaryIsNotWarnedAbout (IN-07): a summary cut short by a stop
// skips the compaction without a warning; a real failure still warns.
func TestCompactor_StoppedSummaryIsNotWarnedAbout(t *testing.T) {
	var logged strings.Builder
	lg := slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelWarn}))
	cfg := defaultCompactor()
	cfg.ContextWindowTokens, cfg.Threshold, cfg.ProtectFirstN, cfg.ProtectLastN, cfg.TailTargetRatio = 100, 0.5, 1, 1, 0.01
	c := agentloop.NewCompactor(cfg)
	agentloop.SetCompactorLogger(c, lg)
	msgs := []kernel.Message{
		{Role: kernel.RoleSystem, Content: "head"},
		{Role: kernel.RoleUser, Content: "middle one"},
		{Role: kernel.RoleAssistant, Content: "middle two"},
		{Role: kernel.RoleUser, Content: "tail"},
	}
	usage := kernel.TokenUsage{InputTokens: 100, OutputTokens: 50}
	fail := func(ctx context.Context, _ []kernel.Message, _ int, _, _ string) (string, error) {
		return "", ctx.Err()
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if res := c.CompactIfNeeded(ctx, msgs, usage, nil, fail); res.Compacted {
		t.Fatal("a stopped summary compacted")
	}
	if logged.Len() != 0 {
		t.Fatalf("a stop was logged as a failure:\n%s", logged.String())
	}

	boom := func(context.Context, []kernel.Message, int, string, string) (string, error) {
		return "", errors.New("model down")
	}
	if res := c.CompactIfNeeded(t.Context(), msgs, usage, nil, boom); res.Compacted {
		t.Fatal("a failed summary compacted")
	}
	if !strings.Contains(logged.String(), "model down") {
		t.Fatalf("a real failure was not logged:\n%s", logged.String())
	}
}

// panicTools is a tools port whose List panics: a port the loop calls directly, not
// through raceAbort.
type panicTools struct {
	agentloop.Tools
	value any
}

func (p panicTools) List() []toolhub.Definition { panic(p.value) }

// TestRun_PanicInAPortCleansUpAndRepanics (WR-04): a panic of a port that is not
// raced ends every open span with the panic text, removes the trace context, emits no
// RunCompleted, and then panics again with the same value (it is never swallowed).
func TestRun_PanicInAPortCleansUpAndRepanics(t *testing.T) {
	rg := newRig(t, rigOptions{wrapTools: func(in agentloop.Tools) agentloop.Tools { return panicTools{Tools: in, value: "tools list blew up"} }},
		aecmock.ScriptedTurn{Content: "unreachable"})
	var recovered any
	func() {
		defer func() { recovered = recover() }()
		_, _ = rg.run(t, "go")
	}()
	if recovered != "tools list blew up" {
		t.Fatalf("recovered %v, want the port's own panic value", recovered)
	}
	started, ended := rg.sink.startedAndEnded()
	if started == 0 || started != ended {
		t.Fatalf("%d spans started, %d ended: every span must close on a panic", started, ended)
	}
	attrs, ok := rg.sink.endAttrs("agent-run")
	if !ok {
		t.Fatal("the run span never ended")
	}
	if v, _ := attrs.Get("error"); v != "panic: tools list blew up" {
		t.Fatalf("run span end attributes = %v, want the panic text as the error", attrs.Keys())
	}
	if countOf(rg, events.RunCompleted.Name) != 0 {
		t.Fatal("a panicked run emitted RunCompleted")
	}
	traceID := rg.sink.all()[0].span.TraceID
	if _, still := rg.deps.Trace.Context(traceID); still {
		t.Fatal("the trace context of a panicked run was never removed")
	}
}

// panicMemory is a memory system that panics.
type panicMemory struct{}

func (panicMemory) RecallForTurn(context.Context, string, string) (memory.RecalledMemories, error) {
	panic("index exploded")
}

// TestMemoryRecallPanicDoesNotFailTheTurn (WR-04): recallMemories is documented as
// never failing the turn, so a panicking memory system is recorded on the span like
// any failed recall and the run carries on.
func TestMemoryRecallPanicDoesNotFailTheTurn(t *testing.T) {
	lg := slog.New(slog.NewTextHandler(io.Discard, nil))
	r := promptRig(t, rigOptions{memory: panicMemory{}, loopOpts: []agentloop.Option{agentloop.WithLogger(lg)}}, aecmock.ScriptedTurn{Content: "carried on"})
	res, err := r.run(t, "question")
	if err != nil || res.Message.Content != "carried on" {
		t.Fatalf("Run = %+v, %v", res, err)
	}
	attrs, ok := r.sink.endAttrs("memory-recall")
	if !ok {
		t.Fatal("the memory-recall span never ended")
	}
	if v, _ := attrs.Get("error"); v != "panic: index exploded" {
		t.Fatalf("memory-recall error = %v", v)
	}
}

// TestBackoff (WR-05): the delay is base x 2^attempt, saturated at the cap, and never
// wraps: a base above 9.2e12 ms used to overflow the Duration conversion.
func TestBackoff(t *testing.T) {
	maxBase := math.MaxInt
	for _, tc := range []struct {
		name            string
		baseMs, attempt int
		want            time.Duration
	}{
		{"defaults, attempt 0", 500, 0, 500 * time.Millisecond},
		{"defaults, attempt 1", 500, 1, time.Second},
		{"defaults, attempt 2", 500, 2, 2 * time.Second},
		{"zero base", 0, 5, 0},
		{"negative base", -5, 3, 0},
		{"doubling reaches the cap", 500, 29, agentloop.MaxBackoff},
		{"a huge attempt count terminates at the cap", 500, 1_000_000, agentloop.MaxBackoff},
		{"a base that overflows a Duration", 1 << 62, 0, agentloop.MaxBackoff},
		{"a base that overflows, doubled", 1 << 62, 3, agentloop.MaxBackoff},
		{"the largest int base", maxBase, 1, agentloop.MaxBackoff},
		{"just under the cap", 299_999, 0, 299_999 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := agentloop.Backoff(tc.baseMs, tc.attempt); got != tc.want {
				t.Fatalf("backoff(%d, %d) = %v, want %v", tc.baseMs, tc.attempt, got, tc.want)
			}
		})
	}
}

// TestRetry_HugeBackoffConfigWaitsAtTheCap (WR-05): a loop config whose base would
// wrap negative still waits, at the cap, before the retry.
func TestRetry_HugeBackoffConfigWaitsAtTheCap(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		one, huge := 1, 1<<62
		p := loopProfile(&one, &huge)
		flaky := failingTool("flaky", 1)
		rg := newRig(t, rigOptions{prof: p, handlers: []toolhub.Handler{flaky}},
			toolTurn(callOf("tc-1", "flaky", "{}")), aecmock.ScriptedTurn{Content: "ok"})
		start := time.Now()
		if _, err := rg.run(t, "go"); err != nil {
			t.Fatal(err)
		}
		if elapsed := time.Since(start); elapsed != agentloop.MaxBackoff {
			t.Fatalf("waited %v, want the cap %v", elapsed, agentloop.MaxBackoff)
		}
		if flaky.executions() != 2 {
			t.Fatalf("%d executions, want 2", flaky.executions())
		}
	})
}
