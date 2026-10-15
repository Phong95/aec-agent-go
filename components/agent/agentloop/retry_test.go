package agentloop_test

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"
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

// loopProfile is the test profile with the given loop limits (nil leaves the
// default).
func loopProfile(retries, backoffMs *int) *profile.AgentProfile {
	p := testProfile("test-agent")
	p.LoopConfig = &profile.LoopConfig{MaxToolRetries: retries, RetryBackoffMs: backoffMs}
	return &p
}

// failingTool is a tool that fails with isError for the first
// failCount calls and then succeeds.
func failingTool(name string, failCount int) *fakeTool {
	var n atomic.Int32
	return newFakeTool(name, func(context.Context, kernel.Arguments, toolhub.Context) (toolhub.Output, error) {
		c := int(n.Add(1))
		if c <= failCount {
			return toolhub.Output{Content: "Temporary error on attempt " + strconv.Itoa(c), IsError: boolRef(true)}, nil
		}
		return toolhub.Output{Content: "Success after retries"}, nil
	})
}

// TestRetry_BacksOffFiveHundredThenAThousand is conformance case C7 with the exact
// timing: a tool that fails twice with isError and then succeeds produces
// ToolRetry 1 and 2 and ToolCompleted with the zero-based attempt 2,
// and 500 + 1000 ms of backoff pass in fake time.
func TestRetry_BacksOffFiveHundredThenAThousand(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		flaky := failingTool("flaky", 2)
		rg := newRig(t, rigOptions{handlers: []toolhub.Handler{flaky}},
			toolTurn(callOf("tc-1", "flaky", "{}")),
			aecmock.ScriptedTurn{Content: "Got the result."},
		)
		start := time.Now()
		if _, err := rg.run(t, "Use flaky tool"); err != nil {
			t.Fatal(err)
		}
		if elapsed := time.Since(start); elapsed != 1500*time.Millisecond {
			t.Fatalf("elapsed = %v, want exactly 500 + 1000 ms", elapsed)
		}
		if flaky.executions() != 3 {
			t.Fatalf("%d executions, want 3", flaky.executions())
		}
		var attempts []int
		for _, e := range rg.log.all() {
			if e.Type == events.ToolRetry.Name {
				p, err := events.PayloadAs[events.DispatchRetryPayload](e)
				if err != nil {
					t.Fatal(err)
				}
				attempts = append(attempts, p.Attempt)
				if p.Reason == "" {
					t.Fatal("ToolRetry without a reason")
				}
			}
		}
		if len(attempts) != 2 || attempts[0] != 1 || attempts[1] != 2 {
			t.Fatalf("ToolRetry attempts = %v, want the one-based 1, 2", attempts)
		}
		if got := rg.log.payload(t, events.ToolCompleted.Name); got != `{"agentId":"test-agent","name":"flaky","isError":false,"attempt":2}` {
			t.Fatalf("ToolCompleted = %s", got)
		}
		if countOf(rg, events.ToolFailed.Name) != 0 {
			t.Fatal("a tool that succeeds after retries emits no ToolFailed")
		}
		if results := resultsOf(rg); len(results) != 1 || results[0].Content != "Success after retries" || results[0].IsError != nil {
			t.Fatalf("results = %+v", results)
		}
	})
}

// TestRetry_BackoffTiming is cases L13 and L14 in exact fake time: a backoff of 0 takes
// no time at all, and a backoff of 60 ms with one retry takes exactly 60 ms.
func TestRetry_BackoffTiming(t *testing.T) {
	for _, tc := range []struct {
		name            string
		retries, backMs int
		want            time.Duration
	}{
		{"backoff 0", 2, 0, 0},
		{"backoff 60 with 1 retry", 1, 60, 60 * time.Millisecond},
		{"backoff 60 with 3 retries doubles", 3, 60, (60 + 120 + 240) * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				flaky := failingTool("flaky", 100)
				rg := newRig(t, rigOptions{prof: loopProfile(&tc.retries, &tc.backMs), handlers: []toolhub.Handler{flaky}},
					toolTurn(callOf("tc-1", "flaky", "{}")), aecmock.ScriptedTurn{Content: "ok"})
				start := time.Now()
				if _, err := rg.run(t, "go"); err != nil {
					t.Fatal(err)
				}
				if elapsed := time.Since(start); elapsed != tc.want {
					t.Fatalf("elapsed = %v, want %v", elapsed, tc.want)
				}
			})
		})
	}
}

// TestRetry_Counts is cases L11 and L12: maxToolRetries 0 gives one call, no retry and
// one failure; 3 gives four calls, three retries and one failure.
func TestRetry_Counts(t *testing.T) {
	for _, retries := range []int{0, 1, 3} {
		t.Run("maxToolRetries "+strconv.Itoa(retries), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				zero := 0
				crasher := newFakeTool("crasher", func(context.Context, kernel.Arguments, toolhub.Context) (toolhub.Output, error) {
					return toolhub.Output{}, errors.New("crasher crashed")
				})
				rg := newRig(t, rigOptions{prof: loopProfile(&retries, &zero), handlers: []toolhub.Handler{crasher}},
					toolTurn(callOf("tc-1", "crasher", "{}")), aecmock.ScriptedTurn{Content: "ok"})
				if _, err := rg.run(t, "go"); err != nil {
					t.Fatal(err)
				}
				if crasher.executions() != retries+1 {
					t.Fatalf("%d executions, want %d", crasher.executions(), retries+1)
				}
				if n := countOf(rg, events.ToolRetry.Name); n != retries {
					t.Fatalf("%d ToolRetry events, want %d", n, retries)
				}
				if n := countOf(rg, events.ToolFailed.Name); n != 1 {
					t.Fatalf("%d ToolFailed events, want 1", n)
				}
				// One attempt reads "1 attempt", not "1 attempts".
				attempts := strconv.Itoa(retries+1) + " attempts"
				if retries == 0 {
					attempts = "1 attempt"
				}
				want := "Error after " + attempts + ": crasher crashed"
				if results := resultsOf(rg); len(results) != 1 || results[0].Content != want || results[0].IsError == nil || !*results[0].IsError {
					t.Fatalf("results = %+v, want %q", results, want)
				}
			})
		})
	}
}

// TestRetry_ReadsLoopConfigPerDispatch: the limits are read from the live profile
// on every dispatch, so a profile updated between two runs applies to the next one.
func TestRetry_ReadsLoopConfigPerDispatch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		zero, two := 0, 2
		var cur atomic.Pointer[profile.AgentProfile]
		cur.Store(loopProfile(&zero, &zero))
		crasher := newFakeTool("crasher", func(context.Context, kernel.Arguments, toolhub.Context) (toolhub.Output, error) {
			return toolhub.Output{}, errors.New("boom")
		})
		rg := newRig(t, rigOptions{profFn: func() profile.AgentProfile { return *cur.Load() }, handlers: []toolhub.Handler{crasher}},
			toolTurn(callOf("tc-1", "crasher", "{}")), aecmock.ScriptedTurn{Content: "one"},
			toolTurn(callOf("tc-2", "crasher", "{}")), aecmock.ScriptedTurn{Content: "two"},
		)
		if _, err := rg.run(t, "first"); err != nil {
			t.Fatal(err)
		}
		if crasher.executions() != 1 {
			t.Fatalf("run 1: %d executions, want 1 (maxToolRetries 0)", crasher.executions())
		}
		cur.Store(loopProfile(&two, &zero))
		if _, err := rg.run(t, "second"); err != nil {
			t.Fatal(err)
		}
		if crasher.executions() != 1+3 {
			t.Fatalf("after the update: %d executions in total, want 1 + 3 (maxToolRetries 2)", crasher.executions())
		}
	})
}

// TestRetry_NonRetryableErrorIsNotRetried: an isError result with retryable false is
// returned after one call, ToolCompleted{isError true, attempt 0}.
func TestRetry_NonRetryableErrorIsNotRetried(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		blocked := constTool("fetch", toolhub.Output{Content: "blocked url", IsError: boolRef(true), Retryable: boolRef(false)})
		rg := newRig(t, rigOptions{handlers: []toolhub.Handler{blocked}},
			toolTurn(callOf("tc-1", "fetch", "{}")), aecmock.ScriptedTurn{Content: "ok"})
		start := time.Now()
		if _, err := rg.run(t, "go"); err != nil {
			t.Fatal(err)
		}
		if blocked.executions() != 1 || countOf(rg, events.ToolRetry.Name) != 0 || time.Since(start) != 0 {
			t.Fatalf("executions %d, retries %d, elapsed %v: want one call, no retry, no wait", blocked.executions(), countOf(rg, events.ToolRetry.Name), time.Since(start))
		}
		if got := rg.log.payload(t, events.ToolCompleted.Name); got != `{"agentId":"test-agent","name":"fetch","isError":true,"attempt":0}` {
			t.Fatalf("ToolCompleted = %s", got)
		}
		results := resultsOf(rg)
		if len(results) != 1 || results[0].Content != "blocked url" {
			t.Fatalf("results = %+v", results)
		}
	})
}

// TestRetry_ThrowingToolSurfacesTheError is conformance case C8: a handler error is
// retried, then the model gets `Error after 3 attempts: crasher crashed` and one
// ToolFailed event carries the message.
func TestRetry_ThrowingToolSurfacesTheError(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		crasher := newFakeTool("crasher", func(context.Context, kernel.Arguments, toolhub.Context) (toolhub.Output, error) {
			return toolhub.Output{}, errors.New("crasher crashed")
		})
		rg := newRig(t, rigOptions{handlers: []toolhub.Handler{crasher}},
			toolTurn(callOf("tc-1", "crasher", "{}")), aecmock.ScriptedTurn{Content: "I see the error."})
		if _, err := rg.run(t, "Use crasher"); err != nil {
			t.Fatal(err)
		}
		if got := rg.log.payload(t, events.ToolFailed.Name); got != `{"agentId":"test-agent","name":"crasher","error":"crasher crashed"}` {
			t.Fatalf("ToolFailed = %s", got)
		}
		results := resultsOf(rg)
		if len(results) != 1 || results[0].Content != "Error after 3 attempts: crasher crashed" {
			t.Fatalf("results = %+v", results)
		}
		if countOf(rg, events.ToolCompleted.Name) != 0 {
			t.Fatal("an exhausted throw emits ToolFailed, not ToolCompleted")
		}
	})
}

// TestRetry_PanicIsFinal: a panicking tool is recovered, so the turn goes on
// (CONTEXT "Panicking tool"), but unlike a returned error it is never retried: a panic is
// a bug, not a transient failure. The model and the ToolFailed event get the generic
// `tool bomb failed: internal error`, never the panic value, and the panic is logged
// with its stack and the value redacted to its type.
func TestRetry_PanicIsFinal(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		bomb := newFakeTool("bomb", func(context.Context, kernel.Arguments, toolhub.Context) (toolhub.Output, error) {
			panic("kaboom secret")
		})
		var logs strings.Builder
		lg := slog.New(slog.NewTextHandler(&logs, nil))
		rg := newRig(t, rigOptions{handlers: []toolhub.Handler{bomb}, loopOpts: []agentloop.Option{agentloop.WithLogger(lg)}},
			toolTurn(callOf("tc-1", "bomb", "{}")), aecmock.ScriptedTurn{Content: "Carrying on."})
		start := time.Now()
		res, err := rg.run(t, "Use bomb")
		if err != nil {
			t.Fatal(err)
		}
		if res.Message.Content != "Carrying on." {
			t.Fatalf("the run did not continue after the panic: %+v", res)
		}
		if bomb.executions() != 1 || time.Since(start) != 0 {
			t.Fatalf("executions %d, elapsed %v: a panic is final, with no backoff", bomb.executions(), time.Since(start))
		}
		if n := countOf(rg, events.ToolRetry.Name); n != 0 {
			t.Fatalf("%d ToolRetry events, want none", n)
		}
		results := resultsOf(rg)
		if len(results) != 1 || results[0].Content != "tool bomb failed: internal error" || results[0].IsError == nil || !*results[0].IsError {
			t.Fatalf("results = %+v", results)
		}
		if got := rg.log.payload(t, events.ToolFailed.Name); got != `{"agentId":"test-agent","name":"bomb","error":"tool bomb failed: internal error"}` {
			t.Fatalf("ToolFailed = %s", got)
		}
		out := logs.String()
		if !strings.Contains(out, "tool call panicked") || !strings.Contains(out, "panic=string") || !strings.Contains(out, "stack=") {
			t.Fatalf("log = %q, want the panic logged with its type and stack", out)
		}
		if strings.Contains(out, "kaboom") || strings.Contains(results[0].Content, "kaboom") {
			t.Fatalf("the panic value leaked: log %q", out)
		}
	})
}

// TestRetry_AuthorizationSchedule
// check the authorization schedule: attempt 0 reuses the authorization of the policy
// step, so a call that succeeds first time asks the policy exactly once, and every
// later attempt asks again.
func TestRetry_AuthorizationSchedule(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		policy := newCountingPolicy()
		ok := constTool("ok", toolhub.Output{Content: "fine"})
		flaky := failingTool("flaky", 2)
		rg := newRig(t, rigOptions{policy: policy, handlers: []toolhub.Handler{ok, flaky}},
			toolTurn(callOf("tc-1", "ok", "{}")), aecmock.ScriptedTurn{Content: "one"})
		if _, err := rg.run(t, "go"); err != nil {
			t.Fatal(err)
		}
		if got := policy.checks.Load(); got != 1 {
			t.Fatalf("the policy ran %d times for a call that succeeded first time, want exactly once", got)
		}

		rg2 := newRig(t, rigOptions{policy: policy, handlers: []toolhub.Handler{flaky}},
			toolTurn(callOf("tc-2", "flaky", "{}")), aecmock.ScriptedTurn{Content: "two"})
		policy.checks.Store(0)
		if _, err := rg2.run(t, "go"); err != nil {
			t.Fatal(err)
		}
		if got := policy.checks.Load(); got != 3 {
			t.Fatalf("the policy ran %d times for 3 attempts, want 3 (attempt 0 reuses the first authorization)", got)
		}
	})
}

// TestRetry_PolicyChangedBetweenAttemptsDeniesTheNextOne: after SetPolicy the next
// attempt authorizes under the new policy; a denying one ends the retries with the
// policy denial text and a ToolDenied event.
func TestRetry_PolicyChangedBetweenAttemptsDeniesTheNextOne(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var hub *toolhub.ToolHub
		flaky := newFakeTool("flaky", func(context.Context, kernel.Arguments, toolhub.Context) (toolhub.Output, error) {
			hub.SetPolicy(denyAll{}) // the policy changes while attempt 0 runs
			return toolhub.Output{Content: "Temporary error", IsError: boolRef(true)}, nil
		})
		rg := newRig(t, rigOptions{handlers: []toolhub.Handler{flaky}},
			toolTurn(callOf("tc-1", "flaky", "{}")), aecmock.ScriptedTurn{Content: "ok"})
		hub = rg.tools
		if _, err := rg.run(t, "go"); err != nil {
			t.Fatal(err)
		}
		if flaky.executions() != 1 {
			t.Fatalf("%d executions, want 1: the denied re-authorization ends the retries", flaky.executions())
		}
		results := resultsOf(rg)
		if len(results) != 1 || results[0].Content != "Policy denied: blocked" || results[0].IsError == nil || !*results[0].IsError {
			t.Fatalf("results = %+v, want Policy denied: blocked", results)
		}
		if got := rg.log.payload(t, events.ToolDenied.Name); got != `{"agentId":"test-agent","name":"flaky","reason":"blocked"}` {
			t.Fatalf("ToolDenied = %s", got)
		}
		if countOf(rg, events.ToolFailed.Name) != 0 || countOf(rg, events.ToolCompleted.Name) != 0 {
			t.Fatalf("events = %v: a denial on re-authorization is final", rg.log.types())
		}
	})
}

// TestRetry_PolicyChangedBeforeExecuteReauthorizesOnce: an Authorized minted under a
// policy that was replaced since (here while the user decided about an approval)
// refuses to run with ErrPolicyChanged; the executor authorizes once more under the
// new policy and runs the tool.
func TestRetry_PolicyChangedBeforeExecuteReauthorizesOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		send := constTool("send", toolhub.Output{Content: "sent"})
		send.def.Risk = toolhub.RiskConsequential
		newPolicy := newCountingPolicy()

		var hub *toolhub.ToolHub
		handler := approvalFunc(func(context.Context, toolhub.ApprovalRequest) (toolhub.ApprovalDecision, error) {
			hub.SetPolicy(newPolicy) // replaced while the user decides
			return toolhub.ApprovalDecision{Approved: true}, nil
		})
		oldPolicy := newCountingPolicy()
		rg := newRig(t, rigOptions{policy: oldPolicy, handlers: []toolhub.Handler{send}, interaction: handler},
			toolTurn(callOf("tc-1", "send", "{}")), aecmock.ScriptedTurn{Content: "ok"})
		hub = rg.tools
		if _, err := rg.run(t, "go"); err != nil {
			t.Fatal(err)
		}
		if send.executions() != 1 {
			t.Fatalf("%d executions, want 1", send.executions())
		}
		if oldPolicy.checks.Load() != 1 || newPolicy.checks.Load() != 1 {
			t.Fatalf("old policy ran %d times, new policy %d times, want once each", oldPolicy.checks.Load(), newPolicy.checks.Load())
		}
		if results := resultsOf(rg); len(results) != 1 || results[0].Content != "sent" {
			t.Fatalf("results = %+v", results)
		}
		if countOf(rg, events.ToolRetry.Name) != 0 {
			t.Fatal("the policy-changed re-authorization is not a retry")
		}
	})
}

// approvalFunc adapts a function to the interaction handler.
type approvalFunc func(context.Context, toolhub.ApprovalRequest) (toolhub.ApprovalDecision, error)

func (approvalFunc) AskUser(context.Context, toolhub.ClarifyRequest) (toolhub.ClarifyResponse, error) {
	return toolhub.ClarifyResponse{}, nil
}

func (f approvalFunc) RequestApproval(ctx context.Context, r toolhub.ApprovalRequest) (toolhub.ApprovalDecision, error) {
	return f(ctx, r)
}

// TestRetry_AbortDuringBackoffStopsRetrying: stopping the run during a backoff ends
// the dispatch at that instant, makes no further attempt and answers the call with
// the cancelled result.
func TestRetry_AbortDuringBackoffStopsRetrying(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		flaky := failingTool("flaky", 100)
		rg := newRig(t, rigOptions{handlers: []toolhub.Handler{flaky}},
			toolTurn(callOf("tc-1", "flaky", "{}")), aecmock.ScriptedTurn{Content: "unreachable"})
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		start := time.Now()
		time.AfterFunc(700*time.Millisecond, cancel) // inside the second backoff (500 to 1500 ms)

		res, err := rg.loop.Run(ctx, "go", "s-1", llmhub.ModelOptions{}, agentloop.RunOptions{})
		// A stop is a result, never an error (09-03 replaced the 09-02 stand-in that
		// accepted either shape).
		if err != nil || !res.Aborted || res.Message.Content != "" {
			t.Fatalf("the run was stopped but returned %+v, %v, want an aborted result and no error", res, err)
		}
		if elapsed := time.Since(start); elapsed != 700*time.Millisecond {
			t.Fatalf("returned after %v, want the instant of the abort (700 ms)", elapsed)
		}
		if flaky.executions() != 2 {
			t.Fatalf("%d executions, want 2: no attempt after the abort", flaky.executions())
		}
		results := resultsOf(rg)
		if len(results) != 1 || results[0].Content != "Cancelled: the user stopped the run before this tool call finished." {
			t.Fatalf("results = %+v, want the cancelled result", results)
		}
		if got := rg.log.payload(t, events.ToolFailed.Name); got != `{"agentId":"test-agent","name":"flaky","error":"cancelled"}` {
			t.Fatalf("ToolFailed = %s", got)
		}
	})
}

// TestRaceAbort covers the abort helper: it returns the work's result when it
// finishes first, aborts at the instant of the cancel when the work ignores ctx, and
// recovers a panic of the work.
func TestRaceAbort(t *testing.T) {
	t.Run("result when the work finishes first", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			v, aborted, err := agentloop.RaceAbort(t.Context(), func() (int, error) {
				time.Sleep(10 * time.Millisecond)
				return 42, nil
			})
			if v != 42 || aborted || err != nil {
				t.Fatalf("got %d, %v, %v", v, aborted, err)
			}
		})
	})
	t.Run("the work's error is returned", func(t *testing.T) {
		boom := errors.New("boom")
		_, aborted, err := agentloop.RaceAbort(t.Context(), func() (int, error) { return 0, boom })
		if aborted || !errors.Is(err, boom) {
			t.Fatalf("aborted %v, err %v", aborted, err)
		}
	})
	t.Run("a work that ignores ctx does not delay the abort", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			release := make(chan struct{})
			time.AfterFunc(20*time.Millisecond, cancel)
			start := time.Now()
			_, aborted, err := agentloop.RaceAbort(ctx, func() (int, error) {
				<-release // never looks at ctx
				return 1, nil
			})
			if !aborted || err != nil || time.Since(start) != 20*time.Millisecond {
				t.Fatalf("aborted %v, err %v after %v, want an abort at exactly 20 ms", aborted, err, time.Since(start))
			}
			close(release) // the abandoned goroutine must be able to finish: its send never blocks
			synctest.Wait()
		})
	})
	t.Run("an already stopped ctx does not start the work", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		var ran atomic.Bool
		_, aborted, _ := agentloop.RaceAbort(ctx, func() (int, error) { ran.Store(true); return 1, nil })
		if !aborted || ran.Load() {
			t.Fatalf("aborted %v, ran %v", aborted, ran.Load())
		}
	})
	t.Run("a panic is recovered as an error", func(t *testing.T) {
		_, aborted, err := agentloop.RaceAbort(t.Context(), func() (int, error) { panic("kaboom") })
		if aborted || err == nil || err.Error() != "panic: kaboom" {
			t.Fatalf("aborted %v, err %v", aborted, err)
		}
	})
}

// TestAbortableDelay: the delay takes exactly d, is cut short by a cancel and takes
// no time for a zero or negative duration.
func TestAbortableDelay(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		agentloop.AbortableDelay(t.Context(), 300*time.Millisecond)
		if got := time.Since(start); got != 300*time.Millisecond {
			t.Fatalf("delay took %v, want 300 ms", got)
		}
		start = time.Now()
		agentloop.AbortableDelay(t.Context(), 0)
		agentloop.AbortableDelay(t.Context(), -time.Second)
		if got := time.Since(start); got != 0 {
			t.Fatalf("a non-positive delay took %v", got)
		}

		ctx, cancel := context.WithCancel(t.Context())
		time.AfterFunc(50*time.Millisecond, cancel)
		start = time.Now()
		agentloop.AbortableDelay(ctx, time.Hour)
		if got := time.Since(start); got != 50*time.Millisecond {
			t.Fatalf("a cancelled delay took %v, want 50 ms", got)
		}
	})
}

// Retry safety.

// TestRetry_ErrorRetryability: a returned error is retried only when it may succeed
// again. An SDK error says so itself (kernel.IsRetryable); any other error is
// retried; a consequential tool, or one that declares no Risk, is never attempted
// again once it ran, whatever it returned.
func TestRetry_ErrorRetryability(t *testing.T) {
	cases := []struct {
		name string
		risk toolhub.Risk
		// out is what every attempt returns.
		out  toolhub.Output
		err  error
		runs int
		// retries is the number of ToolRetry events.
		retries int
		result  string
	}{
		{"plain error, read-only", toolhub.RiskReadOnly, toolhub.Output{}, errors.New("flaky"), 3, 2, "Error after 3 attempts: flaky"},
		{"retryable SDK error", toolhub.RiskReadOnly, toolhub.Output{}, kernel.NewToolError("timed out", kernel.ToolCodeTimeout, "t", true), 3, 2, "Error after 3 attempts: timed out"},
		{"non-retryable SDK error", toolhub.RiskReadOnly, toolhub.Output{}, kernel.NewToolError("bad args", kernel.ToolCodeInvalidArgs, "t", false), 1, 0, "Error after 1 attempt: bad args"},
		{"policy error", toolhub.RiskDraft, toolhub.Output{}, kernel.NewPolicyError("not allowed", kernel.PolicyCodeAuthorizationDenied, "t"), 1, 0, "Error after 1 attempt: not allowed"},
		{"sandbox path escape", toolhub.RiskDraft, toolhub.Output{}, kernel.NewSandboxError("outside the roots", kernel.SandboxCodePathEscape, "box"), 1, 0, "Error after 1 attempt: outside the roots"},
		{"wrapped non-retryable SDK error", toolhub.RiskReadOnly, toolhub.Output{}, errors.Join(errors.New("ctx"), kernel.NewToolError("bad", kernel.ToolCodeInvalidArgs, "t", false)), 1, 0, "Error after 1 attempt: ctx\nbad"},
		{"plain error, consequential", toolhub.RiskConsequential, toolhub.Output{}, errors.New("flaky"), 1, 0, "Error after 1 attempt: flaky"},
		{"retryable SDK error, consequential", toolhub.RiskConsequential, toolhub.Output{}, kernel.NewToolError("timed out", kernel.ToolCodeTimeout, "t", true), 1, 0, "Error after 1 attempt: timed out"},
		{"plain error, no risk", "", toolhub.Output{}, errors.New("flaky"), 1, 0, "Error after 1 attempt: flaky"},
		{"isError result, read-only", toolhub.RiskReadOnly, toolhub.Output{Content: "busy", IsError: boolRef(true)}, nil, 3, 2, "busy"},
		{"isError result, consequential", toolhub.RiskConsequential, toolhub.Output{Content: "busy", IsError: boolRef(true)}, nil, 1, 0, "busy"},
		{"isError result marked retryable, consequential", toolhub.RiskConsequential, toolhub.Output{Content: "busy", IsError: boolRef(true), Retryable: boolRef(true)}, nil, 1, 0, "busy"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ft := newFakeTool("act", func(context.Context, kernel.Arguments, toolhub.Context) (toolhub.Output, error) {
					return c.out, c.err
				})
				ft.def.Risk = c.risk
				handler, _ := recordingHandler(approve())
				rg := newRig(t, rigOptions{handlers: []toolhub.Handler{ft}, interaction: handler},
					toolTurn(callOf("tc-1", "act", "{}")), aecmock.ScriptedTurn{Content: "ok"})
				if _, err := rg.run(t, "go"); err != nil {
					t.Fatal(err)
				}
				if ft.executions() != c.runs {
					t.Fatalf("%d executions, want %d", ft.executions(), c.runs)
				}
				if n := countOf(rg, events.ToolRetry.Name); n != c.retries {
					t.Fatalf("%d ToolRetry events, want %d", n, c.retries)
				}
				if results := resultsOf(rg); len(results) != 1 || results[0].Content != c.result {
					t.Fatalf("results = %+v, want %q", results, c.result)
				}
			})
		})
	}
}

// TestRun_ConsequentialToolIsNeverRetried: a consequential tool that failed after it
// ran is not attempted again, so its effect cannot happen twice; the user is asked
// once and the model hears the failure at once, without a backoff.
func TestRetry_ConsequentialToolIsNeverRetried(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		send := newFakeTool("send_rfi", func(context.Context, kernel.Arguments, toolhub.Context) (toolhub.Output, error) {
			return toolhub.Output{}, errors.New("connection reset after the request was sent")
		})
		send.def.Risk = toolhub.RiskConsequential
		handler, reqs := recordingHandler(approve())
		rg := newRig(t, rigOptions{handlers: []toolhub.Handler{send}, interaction: handler},
			toolTurn(callOf("tc-1", "send_rfi", "{}")), aecmock.ScriptedTurn{Content: "ok"})
		start := time.Now()
		if _, err := rg.run(t, "go"); err != nil {
			t.Fatal(err)
		}
		if send.executions() != 1 || len(*reqs) != 1 || time.Since(start) != 0 {
			t.Fatalf("%d executions, %d approvals, %v elapsed: want one of each and no backoff", send.executions(), len(*reqs), time.Since(start))
		}
		if got := rg.log.payload(t, events.ToolFailed.Name); got != `{"agentId":"test-agent","name":"send_rfi","error":"connection reset after the request was sent"}` {
			t.Fatalf("ToolFailed = %s", got)
		}
	})
}
