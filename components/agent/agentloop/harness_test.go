package agentloop_test

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Phong95/aec-agent-go/aecmock"
	"github.com/Phong95/aec-agent-go/components/agent/agentloop"
	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/kernel"
)

// resultsOf returns the tool results persisted in the conversation, in order.
func resultsOf(rg *rig) []kernel.ToolResult {
	var out []kernel.ToolResult
	for _, m := range rg.conv.Messages() {
		out = append(out, m.ToolResults...)
	}
	return out
}

// countOf is how many events of the type were emitted.
func countOf(rg *rig, kind string) int {
	n := 0
	for _, ty := range rg.log.types() {
		if ty == kind {
			n++
		}
	}
	return n
}

// TestRun_OneToolCall is the tracer of the executor (conformance case C15): a
// tool_use turn runs one tool through Tools.Authorize and Authorized.Execute,
// persists the result as one message and loops to the final answer.
func TestRun_OneToolCall(t *testing.T) {
	echo := constTool("echo", toolhub.Output{Content: "ok"})
	rg := newRig(t, rigOptions{handlers: []toolhub.Handler{echo}},
		toolTurn(callOf("tc-1", "echo", "{}")),
		aecmock.ScriptedTurn{Content: "Done."},
	)
	res, err := rg.run(t, "Use the tool")
	if err != nil {
		t.Fatal(err)
	}
	if res.Message.Content != "Done." || res.Iterations != 2 {
		t.Fatalf("result = %+v, want the final answer after 2 iterations", res)
	}
	if res.Usage.InputTokens != 20 || res.Usage.OutputTokens != 10 {
		t.Fatalf("usage = %+v, want 20 in and 10 out (10 and 5 per scripted turn)", res.Usage)
	}
	if echo.executions() != 1 {
		t.Fatalf("the tool ran %d times, want once", echo.executions())
	}

	want := []string{
		events.UserMessage.Name, events.Iteration.Name, events.ModelRequested.Name, events.ModelResponded.Name, events.ContextUsage.Name,
		events.ToolDispatched.Name, events.ToolCompleted.Name,
		events.Iteration.Name, events.ModelRequested.Name, events.ModelResponded.Name, events.ContextUsage.Name, events.RunCompleted.Name,
	}
	if got := rg.log.types(); !slices.Equal(got, want) {
		t.Fatalf("events = %v\nwant     %v", got, want)
	}
	if got := rg.log.payload(t, events.ToolCompleted.Name); got != `{"agentId":"test-agent","name":"echo","isError":false,"attempt":0}` {
		t.Fatalf("ToolCompleted payload = %s", got)
	}

	msgs := rg.conv.Messages()
	if len(msgs) != 4 {
		t.Fatalf("%d persisted messages, want user, assistant with the call, assistant with the result, assistant answer", len(msgs))
	}
	if len(msgs[1].ToolCalls) != 1 || msgs[1].ToolCalls[0].Name != "echo" {
		t.Fatalf("message 1 = %+v, want the assistant message with the call", msgs[1])
	}
	if msgs[2].Role != kernel.RoleAssistant || msgs[2].Content != "" || len(msgs[2].ToolResults) != 1 || msgs[2].ToolResults[0].Content != "ok" || msgs[2].ToolResults[0].ToolCallID != "tc-1" {
		t.Fatalf("message 2 = %+v, want ONE assistant message holding the result", msgs[2])
	}
	if got := rg.sink.parentName("echo"); got != "iteration-1" {
		t.Fatalf("the dispatch span hangs under %q, want iteration-1", got)
	}
	if started, ended := rg.sink.startedAndEnded(); started != ended {
		t.Fatalf("%d spans started and %d ended", started, ended)
	}
}

// TestRun_ToolResultsAreOneMessageInCallOrder checks that several calls of one
// response persist ONE results message in the order of the calls, whatever order
// they finish in.
func TestRun_ToolResultsAreOneMessageInCallOrder(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		slow := newFakeTool("slow", func(context.Context, kernel.Arguments, toolhub.Context) (toolhub.Output, error) {
			time.Sleep(200 * time.Millisecond)
			return toolhub.Output{Content: "slow done"}, nil
		})
		fast := constTool("fast", toolhub.Output{Content: "fast done"})
		rg := newRig(t, rigOptions{handlers: []toolhub.Handler{slow, fast}},
			toolTurn(callOf("tc-1", "slow", "{}"), callOf("tc-2", "fast", "{}")),
			aecmock.ScriptedTurn{Content: "ok"},
		)
		if _, err := rg.run(t, "go"); err != nil {
			t.Fatal(err)
		}
		results := resultsOf(rg)
		if len(results) != 2 || results[0].ToolCallID != "tc-1" || results[0].Content != "slow done" || results[1].ToolCallID != "tc-2" || results[1].Content != "fast done" {
			t.Fatalf("results = %+v, want the slow call first (call order)", results)
		}
		if n := countOf(rg, events.ToolDispatched.Name); n != 2 {
			t.Fatalf("%d ToolDispatched events, want 2", n)
		}
	})
}

// TestRun_ParallelCallsStartTogether is conformance case C10: both calls are in
// flight before either finishes. A barrier proves it without a wall sleep: each
// tool waits for the other to have started, so a sequential executor would block.
func TestRun_ParallelCallsStartTogether(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var started sync.WaitGroup
		started.Add(2)
		meet := func(name string) *fakeTool {
			return newFakeTool(name, func(context.Context, kernel.Arguments, toolhub.Context) (toolhub.Output, error) {
				started.Done()
				started.Wait() // returns only when both tools are running
				return toolhub.Output{Content: name + " done"}, nil
			})
		}
		rg := newRig(t, rigOptions{handlers: []toolhub.Handler{meet("alpha"), meet("beta")}},
			toolTurn(callOf("tc-a", "alpha", "{}"), callOf("tc-b", "beta", "{}")),
			aecmock.ScriptedTurn{Content: "Both done."},
		)
		if _, err := rg.run(t, "Run both"); err != nil {
			t.Fatal(err)
		}
		if got := resultsOf(rg); len(got) != 2 {
			t.Fatalf("results = %+v, want both", got)
		}
	})
}

// TestRun_ParallelCallsNeverExceedTheBound runs 20 calls with the default bound of
// 8: at most 8 handlers are in flight, all 20 results come back in call order and
// every call has a closed span. In fake time the three waves of 50 ms take exactly
// 150 ms.
func TestRun_ParallelCallsNeverExceedTheBound(t *testing.T) {
	for _, tc := range []struct {
		name  string
		opts  []agentloop.Option
		bound int
	}{
		{"default bound is 8", nil, 8},
		{"WithMaxParallelTools(3)", []agentloop.Option{agentloop.WithMaxParallelTools(3)}, 3},
		{"WithMaxParallelTools(0) keeps the default", []agentloop.Option{agentloop.WithMaxParallelTools(0)}, 8},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				const calls = 20
				var inFlight, peak atomic.Int32
				work := newFakeTool("work", func(_ context.Context, args kernel.Arguments, _ toolhub.Context) (toolhub.Output, error) {
					n := inFlight.Add(1)
					for {
						p := peak.Load()
						if n <= p || peak.CompareAndSwap(p, n) {
							break
						}
					}
					time.Sleep(50 * time.Millisecond)
					inFlight.Add(-1)
					return toolhub.Output{Content: string(args)}, nil
				})
				var cs []kernel.ToolCall
				for i := range calls {
					cs = append(cs, callOf("tc-"+strconv.Itoa(i), "work", fmt.Sprintf(`{"i":%d}`, i)))
				}
				rg := newRig(t, rigOptions{handlers: []toolhub.Handler{work}, loopOpts: tc.opts},
					toolTurn(cs...), aecmock.ScriptedTurn{Content: "ok"})

				start := time.Now()
				if _, err := rg.run(t, "go"); err != nil {
					t.Fatal(err)
				}
				elapsed := time.Since(start)

				if int(peak.Load()) != tc.bound {
					t.Fatalf("peak in flight = %d, want exactly the bound %d", peak.Load(), tc.bound)
				}
				waves := (calls + tc.bound - 1) / tc.bound
				if want := time.Duration(waves) * 50 * time.Millisecond; elapsed != want {
					t.Fatalf("elapsed = %v, want %v (%d waves)", elapsed, want, waves)
				}
				results := resultsOf(rg)
				if len(results) != calls {
					t.Fatalf("%d results, want %d", len(results), calls)
				}
				for i, r := range results {
					if r.ToolCallID != "tc-"+strconv.Itoa(i) || r.Content != fmt.Sprintf(`{"i":%d}`, i) {
						t.Fatalf("result %d = %+v, want call order", i, r)
					}
				}
				if started, ended := rg.sink.startedAndEnded(); started != ended {
					t.Fatalf("%d spans started, %d ended: every call needs a closed span", started, ended)
				}
			})
		})
	}
}

// TestRun_UnknownToolIsAnsweredAtOnce is conformance case C3: an unregistered tool
// is an isError result marked non-retryable, so the model is told at once, without
// a 500 then 1000 ms backoff, with ToolCompleted{attempt 0} and no ToolRetry.
func TestRun_UnknownToolIsAnsweredAtOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rg := newRig(t, rigOptions{},
			toolTurn(callOf("tc-1", "ghost", "{}")),
			aecmock.ScriptedTurn{Content: "I cannot use that."},
		)
		start := time.Now()
		res, err := rg.run(t, "Use ghost")
		if err != nil {
			t.Fatal(err)
		}
		if elapsed := time.Since(start); elapsed != 0 {
			t.Fatalf("elapsed = %v, want no backoff", elapsed)
		}
		if res.Iterations != 2 {
			t.Fatalf("iterations = %d, want 2", res.Iterations)
		}
		results := resultsOf(rg)
		if len(results) != 1 || results[0].Content != `Tool "ghost" not available for this agent` || results[0].IsError == nil || !*results[0].IsError {
			t.Fatalf("results = %+v, want the TS not-available error", results)
		}
		if n := countOf(rg, events.ToolRetry.Name); n != 0 {
			t.Fatalf("%d ToolRetry events, want none", n)
		}
		if got := rg.log.payload(t, events.ToolCompleted.Name); got != `{"agentId":"test-agent","name":"ghost","isError":true,"attempt":0}` {
			t.Fatalf("ToolCompleted = %s, want isError true with the zero-based attempt 0 (Q18)", got)
		}
		if countOf(rg, events.ToolFailed.Name) != 0 {
			t.Fatal("an unknown tool ends with ToolCompleted, never ToolFailed")
		}
	})
}

// TestRun_ToolOutsideTheAgentsListIsUnknown: a registered tool the agent may not
// use is reported exactly like an unregistered one (the tool lookup does not find
// it).
func TestRun_ToolOutsideTheAgentsListIsUnknown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rg := newRig(t, rigOptions{}, toolTurn(callOf("tc-1", "secret", "{}")), aecmock.ScriptedTurn{Content: "ok"})
		secret := constTool("secret", toolhub.Output{Content: "classified"})
		if err := rg.tools.Register(secret); err != nil {
			t.Fatal(err)
		}
		// The rig's tool list is empty: secret is registered with the hub but not
		// in the list, so the agent may not use it.
		if _, err := rg.run(t, "go"); err != nil {
			t.Fatal(err)
		}
		if secret.executions() != 0 {
			t.Fatal("a tool outside the agent's list ran")
		}
		if results := resultsOf(rg); len(results) != 1 || results[0].Content != `Tool "secret" not available for this agent` {
			t.Fatalf("results = %+v", results)
		}
	})
}

// countingPolicy counts how often the hub asked it, and delegates to the default.
type countingPolicy struct {
	toolhub.Policy
	checks atomic.Int32
}

func newCountingPolicy() *countingPolicy {
	return &countingPolicy{Policy: toolhub.NewDefaultPolicy(toolhub.DefaultPolicyOptions{})}
}

func (p *countingPolicy) Check(ctx context.Context, name string, args kernel.Arguments, tc toolhub.Context, def toolhub.Definition, allowed []string) toolhub.Decision {
	p.checks.Add(1)
	return p.Policy.Check(ctx, name, args, tc, def, allowed)
}

// approvalCounter is an interaction handler that approves everything and counts the
// requests.
type approvalCounter struct{ requests atomic.Int32 }

func (*approvalCounter) AskUser(context.Context, toolhub.ClarifyRequest) (toolhub.ClarifyResponse, error) {
	return toolhub.ClarifyResponse{}, nil
}

func (a *approvalCounter) RequestApproval(context.Context, toolhub.ApprovalRequest) (toolhub.ApprovalDecision, error) {
	a.requests.Add(1)
	return toolhub.ApprovalDecision{Approved: true}, nil
}

// TestRun_InvalidArgumentsNeverReachPolicyApprovalOrHandler is the harness half of
// T-08-01 (case I1, run path): a call that carries ArgumentsError is answered with the
// invalid-arguments tool error and reaches nothing: not the authorization, not the
// approval gate, not the handler. The corrected call that follows runs once, through
// all three.
func TestRun_InvalidArgumentsNeverReachPolicyApprovalOrHandler(t *testing.T) {
	write := newFakeTool("write_file", func(context.Context, kernel.Arguments, toolhub.Context) (toolhub.Output, error) {
		return toolhub.Output{Content: "written"}, nil
	})
	write.def.Risk = toolhub.RiskConsequential // would need approval if it got that far
	policy := newCountingPolicy()
	approvals := &approvalCounter{}
	broken := callOf("tc-1", "write_file", "")
	broken.ArgumentsError = "the arguments are not valid JSON (Unterminated string in JSON at position 52)"

	rg := newRig(t, rigOptions{handlers: []toolhub.Handler{write}, policy: policy, interaction: approvals},
		toolTurn(broken),
		toolTurn(callOf("tc-2", "write_file", `{"path":"notes/report.md"}`)),
		aecmock.ScriptedTurn{Content: "Saved."},
	)

	if _, err := rg.run(t, "write the report"); err != nil {
		t.Fatal(err)
	}
	results := resultsOf(rg)
	if len(results) != 2 {
		t.Fatalf("results = %+v, want one per call", results)
	}
	wantPrefix := `Invalid arguments for "write_file": the arguments are not valid JSON`
	if !strings.HasPrefix(results[0].Content, wantPrefix) || !strings.Contains(results[0].Content, "valid JSON object") {
		t.Fatalf("broken call content = %q", results[0].Content)
	}
	if results[0].IsError == nil || !*results[0].IsError {
		t.Fatal("the broken call must be an error result")
	}
	if results[1].Content != "written" {
		t.Fatalf("corrected call = %+v", results[1])
	}

	// Only the corrected call went through the policy, the approval gate and the handler.
	if got := policy.checks.Load(); got != 1 {
		t.Fatalf("the policy ran %d times, want once (the corrected call only)", got)
	}
	if got := approvals.requests.Load(); got != 1 {
		t.Fatalf("%d approval requests, want 1 (the corrected call only)", got)
	}
	if write.executions() != 1 {
		t.Fatalf("the tool ran %d times, want once", write.executions())
	}
	if countOf(rg, events.ToolDenied.Name) != 0 {
		t.Fatal("an invalid-arguments call must not be a denial")
	}
	failed := rg.log.payload(t, events.ToolFailed.Name)
	if failed != `{"agentId":"test-agent","name":"write_file","error":"invalid-arguments"}` {
		t.Fatalf("ToolFailed = %s", failed)
	}
	attrs, ok := rg.sink.endAttrs("write_file")
	if !ok {
		t.Fatal("no dispatch span ended")
	}
	if got, _ := attrs.Get("error"); got != "invalid-arguments" {
		t.Fatalf("span end attributes = %v, want error invalid-arguments", attrs.Keys())
	}
	if got, _ := attrs.Get("argumentsError"); got != broken.ArgumentsError {
		t.Fatalf("span argumentsError = %v", got)
	}
}

// skillViewTool is a stand-in for skill_view: it returns the named skill's
// instructions.
func skillViewTool(instructions map[string]string) *fakeTool {
	t := newFakeTool("skill_view", func(_ context.Context, args kernel.Arguments, _ toolhub.Context) (toolhub.Output, error) {
		var a struct {
			Name string `json:"name"`
		}
		_ = args.Decode(&a)
		if text, ok := instructions[a.Name]; ok {
			return toolhub.Output{Content: text}, nil
		}
		return toolhub.Output{Content: "no such skill", IsError: boolRef(true), Retryable: boolRef(false)}, nil
	})
	t.def.Risk = toolhub.RiskReadOnly
	return t
}

// denyAll is a policy that denies every tool call.
type denyAll struct{}

func (denyAll) Name() string    { return "policy:deny-all" }
func (denyAll) Version() string { return "1.0.0" }
func (denyAll) Check(context.Context, string, kernel.Arguments, toolhub.Context, toolhub.Definition, []string) toolhub.Decision {
	return toolhub.Deny("blocked")
}

// TestRun_SkillViewIsASkillDispatch is conformance case C11: skill_view routes to the
// skills port as a skill dispatch (skill events, a skill-dispatch span named after
// the skill) and skips the policy: a policy that denies every tool does not affect it.
func TestRun_SkillViewIsASkillDispatch(t *testing.T) {
	view := skillViewTool(map[string]string{"plan": "Planning instructions"})
	rg := newRig(t, rigOptions{policy: denyAll{}, skills: aecmock.NewSkillTools(view)},
		toolTurn(callOf("tc-1", "skill_view", `{"name":"plan"}`)),
		aecmock.ScriptedTurn{Content: "Done."},
	)
	if _, err := rg.run(t, "Plan it"); err != nil {
		t.Fatal(err)
	}
	if countOf(rg, events.SkillDispatched.Name) != 1 || countOf(rg, events.SkillCompleted.Name) != 1 {
		t.Fatalf("events = %v, want one SkillDispatched and one SkillCompleted", rg.log.types())
	}
	if countOf(rg, events.ToolDispatched.Name) != 0 || countOf(rg, events.ToolDenied.Name) != 0 {
		t.Fatalf("events = %v, want no tool dispatch and no denial", rg.log.types())
	}
	if results := resultsOf(rg); len(results) != 1 || results[0].Content != "Planning instructions" {
		t.Fatalf("results = %+v", results)
	}
	attrs, ok := rg.sink.endAttrs("plan")
	if !ok {
		t.Fatal("no span named after the skill ended")
	}
	if got, _ := attrs.Get("skillName"); got != "plan" {
		t.Fatalf("skillName = %v, want plan", got)
	}
	for _, r := range rg.sink.all() {
		if r.phase == "start" && r.span.Name == "plan" && r.span.Kind != "skill-dispatch" {
			t.Fatalf("span kind = %q, want skill-dispatch", r.span.Kind)
		}
	}
}
