package agentloop_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Phong95/aec-agent-go/aecmock"
	"github.com/Phong95/aec-agent-go/components/agent/agentloop"
	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/kernel"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

const doNotRetry = ". Do not retry this action unless the user asks for it."

// consequentialTool is a tool that needs the user's approval.
func consequentialTool(name string) *fakeTool {
	t := constTool(name, toolhub.Output{Content: "sent"})
	t.def.Risk = toolhub.RiskConsequential
	return t
}

// sandboxTool is a tool that writes in the agent's sandbox and declares the draft
// risk, as the built-in workspace writers do, so only its sandbox can gate it. (It
// used to declare no risk, which Go now gates like a consequential tool; a test
// about the unset risk clears it.)
func sandboxTool(name string, op sandboxhub.Operation, run func(context.Context, kernel.Arguments, toolhub.Context) (toolhub.Output, error)) *fakeTool {
	if run == nil {
		run = func(context.Context, kernel.Arguments, toolhub.Context) (toolhub.Output, error) {
			return toolhub.Output{Content: "saved"}, nil
		}
	}
	t := newFakeTool(name, run)
	t.def.SandboxOperation = op
	t.def.Risk = toolhub.RiskDraft
	return t
}

// fakeSandboxes is the agent's sandbox access: one view, the real default sandbox
// approval policy, and optionally a resolution failure.
type fakeSandboxes struct {
	view sandboxhub.View
	err  error
	hub  *sandboxhub.SandboxHub
}

func newFakeSandboxes(t *testing.T, kind sandboxhub.Kind, id string) *fakeSandboxes {
	t.Helper()
	prov := aecmock.NewInMemoryProvider(aecmock.WithKind(kind))
	env, err := prov.Create(t.Context(), sandboxhub.Spec{Kind: kind}, sandboxhub.CreateContext{ID: id, Tenancy: sandboxhub.TenancyOwn})
	if err != nil {
		t.Fatal(err)
	}
	view, err := env.ForAgent(t.Context(), "test-agent")
	if err != nil {
		t.Fatal(err)
	}
	return &fakeSandboxes{view: view, hub: sandboxhub.NewSandboxHub()}
}

func (f *fakeSandboxes) GetSandbox(context.Context) (sandboxhub.View, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.view, nil
}

func (f *fakeSandboxes) RequiresApproval(traits sandboxhub.Traits, op sandboxhub.Operation) bool {
	return f.hub.RequiresApproval(traits, op)
}

// recordingHandler is an interaction handler that records the requests it gets and
// answers with a function. The slice is read after the run, which happens after the
// handler returned; the appends are locked because calls of one batch run in
// parallel.
func recordingHandler(answer func(context.Context, toolhub.ApprovalRequest) (toolhub.ApprovalDecision, error)) (approvalFunc, *[]toolhub.ApprovalRequest) {
	var (
		mu   sync.Mutex
		reqs []toolhub.ApprovalRequest
	)
	h := approvalFunc(func(ctx context.Context, r toolhub.ApprovalRequest) (toolhub.ApprovalDecision, error) {
		mu.Lock()
		reqs = append(reqs, r)
		mu.Unlock()
		return answer(ctx, r)
	})
	return h, &reqs
}

func approve() func(context.Context, toolhub.ApprovalRequest) (toolhub.ApprovalDecision, error) {
	return func(context.Context, toolhub.ApprovalRequest) (toolhub.ApprovalDecision, error) {
		return toolhub.ApprovalDecision{Approved: true}, nil
	}
}

// TestApproval_ApprovedRunsTheToolAndRecordsTheWait: a consequential tool the user
// approves. The handler gets the expected request fields, ToolApprovalRequested carries no
// reason and no sandbox, ToolApprovalResolved reports the wait in exact fake time,
// and the dispatch span records the risk and the approval.
func TestApproval_ApprovedRunsTheToolAndRecordsTheWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		send := consequentialTool("send_rfi")
		handler, reqs := recordingHandler(func(context.Context, toolhub.ApprovalRequest) (toolhub.ApprovalDecision, error) {
			time.Sleep(250 * time.Millisecond) // the user thinks
			return toolhub.ApprovalDecision{Approved: true}, nil
		})
		rg := newRig(t, rigOptions{handlers: []toolhub.Handler{send}, interaction: handler},
			toolTurn(callOf("tc-1", "send_rfi", `{"to":"arch@example.com"}`)), aecmock.ScriptedTurn{Content: "Sent."})
		if _, err := rg.run(t, "Send the RFI"); err != nil {
			t.Fatal(err)
		}
		if send.executions() != 1 {
			t.Fatalf("%d executions, want 1", send.executions())
		}

		session, _ := rg.conv.Session()
		want := toolhub.ApprovalRequest{
			SessionID: session.ID, AgentID: "test-agent", ToolCallID: "tc-1", ToolName: "send_rfi",
			ToolDescription: "fake tool send_rfi", Risk: toolhub.RiskConsequential, Arguments: kernel.Arguments(`{"to":"arch@example.com"}`),
		}
		if len(*reqs) != 1 || !requestsEqual((*reqs)[0], want) {
			t.Fatalf("handler got %+v, want %+v", *reqs, want)
		}

		if got := rg.log.payload(t, events.ToolApprovalRequested.Name); got != `{"agentId":"test-agent","name":"send_rfi","toolCallId":"tc-1","risk":"consequential","arguments":{"to":"arch@example.com"}}` {
			t.Fatalf("ToolApprovalRequested = %s (no reason, no sandbox)", got)
		}
		if got := rg.log.payload(t, events.ToolApprovalResolved.Name); got != `{"agentId":"test-agent","name":"send_rfi","toolCallId":"tc-1","approved":true,"waitedMs":250}` {
			t.Fatalf("ToolApprovalResolved = %s", got)
		}
		wantTypes := []string{events.ToolDispatched.Name, events.ToolApprovalRequested.Name, events.ToolApprovalResolved.Name, events.ToolCompleted.Name}
		var got []string
		for _, ty := range rg.log.types() {
			if slices.Contains(wantTypes, ty) {
				got = append(got, ty)
			}
		}
		if !slices.Equal(got, wantTypes) {
			t.Fatalf("event order = %v, want %v", got, wantTypes)
		}

		attrs, _ := rg.sink.endAttrs("send_rfi")
		if risk, _ := attrs.Get("risk"); risk != "consequential" {
			t.Fatalf("span risk = %v", risk)
		}
		approval, _ := attrs.Get("approval")
		o, ok := approval.(ordered.Object)
		if !ok {
			t.Fatalf("span approval = %#v", approval)
		}
		if a, _ := o.Get("approved"); a != true {
			t.Fatalf("approval = %v", o.Keys())
		}
		if w, _ := o.Get("waitedMs"); w != int64(250) {
			t.Fatalf("approval waitedMs = %#v, want 250", w)
		}
		if o.Has("reason") {
			t.Fatal("an approval without a reason has no reason key")
		}
	})
}

func requestsEqual(a, b toolhub.ApprovalRequest) bool {
	return a.SessionID == b.SessionID && a.AgentID == b.AgentID && a.ToolCallID == b.ToolCallID && a.ToolName == b.ToolName &&
		a.ToolDescription == b.ToolDescription && a.Risk == b.Risk && string(a.Arguments) == string(b.Arguments) &&
		a.Reason == b.Reason && (a.Sandbox == nil) == (b.Sandbox == nil) && (a.Sandbox == nil || *a.Sandbox == *b.Sandbox)
}

// TestApproval_DeclineDeniesWithTheReason: the user declines. Nothing runs, the model
// is told not to retry, ToolDenied carries the reason and the span ends with the
// approval-denied error.
func TestApproval_DeclineDeniesWithTheReason(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reason *string
		text   string
	}{
		{"with a reason", strRef("not now"), "user declined (not now)"},
		{"without a reason", nil, "user declined"},
		{"with an empty reason", strRef(""), "user declined"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				send := consequentialTool("send_rfi")
				handler, _ := recordingHandler(func(context.Context, toolhub.ApprovalRequest) (toolhub.ApprovalDecision, error) {
					return toolhub.ApprovalDecision{Approved: false, Reason: tc.reason}, nil
				})
				rg := newRig(t, rigOptions{handlers: []toolhub.Handler{send}, interaction: handler},
					toolTurn(callOf("tc-1", "send_rfi", "{}")), aecmock.ScriptedTurn{Content: "Understood."})
				if _, err := rg.run(t, "go"); err != nil {
					t.Fatal(err)
				}
				if send.executions() != 0 {
					t.Fatal("a declined tool ran")
				}
				results := resultsOf(rg)
				if len(results) != 1 || results[0].Content != "Approval denied: "+tc.text+doNotRetry || results[0].IsError == nil || !*results[0].IsError {
					t.Fatalf("results = %+v", results)
				}
				if got := rg.log.payload(t, events.ToolDenied.Name); got != `{"agentId":"test-agent","name":"send_rfi","reason":"`+tc.text+`"}` {
					t.Fatalf("ToolDenied = %s", got)
				}
				attrs, _ := rg.sink.endAttrs("send_rfi")
				if e, _ := attrs.Get("error"); e != "approval-denied" {
					t.Fatalf("span end = %v, want error approval-denied", attrs.Keys())
				}
				if countOf(rg, events.ToolCompleted.Name) != 0 {
					t.Fatal("a declined call has no ToolCompleted")
				}
			})
		})
	}
}

// TestApproval_NoHandlerFailsClosed: a consequential tool with no interaction handler
// is denied, with the no-handler text, and no approval request is emitted.
func TestApproval_NoHandlerFailsClosed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		send := consequentialTool("send_rfi")
		rg := newRig(t, rigOptions{handlers: []toolhub.Handler{send}},
			toolTurn(callOf("tc-1", "send_rfi", "{}")), aecmock.ScriptedTurn{Content: "ok"})
		if _, err := rg.run(t, "go"); err != nil {
			t.Fatal(err)
		}
		if send.executions() != 0 {
			t.Fatal("a consequential tool ran without anyone to approve it")
		}
		want := "Approval denied: no interactive user is available to approve this consequential action" + doNotRetry
		if results := resultsOf(rg); len(results) != 1 || results[0].Content != want {
			t.Fatalf("results = %+v, want %q", results, want)
		}
		if countOf(rg, events.ToolApprovalRequested.Name) != 0 {
			t.Fatal("no handler: nothing to request")
		}
		attrs, _ := rg.sink.endAttrs("send_rfi")
		approval, _ := attrs.Get("approval")
		o, _ := approval.(ordered.Object)
		if a, _ := o.Get("approved"); a != false {
			t.Fatalf("approval = %v", o.Keys())
		}
		if r, _ := o.Get("reason"); r != "no-interaction-handler" {
			t.Fatalf("approval reason = %v", r)
		}
	})
}

// TestApproval_HandlerErrorFailsClosed: a handler that fails denies the call.
func TestApproval_HandlerErrorFailsClosed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		send := consequentialTool("send_rfi")
		handler, _ := recordingHandler(func(context.Context, toolhub.ApprovalRequest) (toolhub.ApprovalDecision, error) {
			return toolhub.ApprovalDecision{Approved: true}, errors.New("channel closed") // an error beats the decision
		})
		rg := newRig(t, rigOptions{handlers: []toolhub.Handler{send}, interaction: handler},
			toolTurn(callOf("tc-1", "send_rfi", "{}")), aecmock.ScriptedTurn{Content: "ok"})
		if _, err := rg.run(t, "go"); err != nil {
			t.Fatal(err)
		}
		if send.executions() != 0 {
			t.Fatal("the tool ran although the approval request failed")
		}
		want := "Approval denied: user declined (approval request failed: channel closed)" + doNotRetry
		if results := resultsOf(rg); len(results) != 1 || results[0].Content != want {
			t.Fatalf("results = %+v, want %q", results, want)
		}
		if got := rg.log.payload(t, events.ToolApprovalResolved.Name); got != `{"agentId":"test-agent","name":"send_rfi","toolCallId":"tc-1","approved":false,"reason":"approval request failed: channel closed","waitedMs":0}` {
			t.Fatalf("ToolApprovalResolved = %s", got)
		}
	})
}

// blockingApproval is an interaction handler whose decision the test delivers: the
// handler blocks on a channel (a user who has not answered yet) and ignores ctx.
type blockingApproval struct {
	asked  chan toolhub.ApprovalRequest
	decide chan toolhub.ApprovalDecision
}

func newBlockingApproval() *blockingApproval {
	return &blockingApproval{asked: make(chan toolhub.ApprovalRequest, 4), decide: make(chan toolhub.ApprovalDecision)}
}

func (*blockingApproval) AskUser(context.Context, toolhub.ClarifyRequest) (toolhub.ClarifyResponse, error) {
	return toolhub.ClarifyResponse{}, nil
}

func (b *blockingApproval) RequestApproval(_ context.Context, r toolhub.ApprovalRequest) (toolhub.ApprovalDecision, error) {
	b.asked <- r
	return <-b.decide, nil
}

// TestApproval_ToolDoesNotRunBeforeTheDecision (T-09-06): the tool does not execute
// until the handler answers; the test observes zero executions while the handler
// blocks, then releases it.
func TestApproval_ToolDoesNotRunBeforeTheDecision(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		send := consequentialTool("send_rfi")
		handler := newBlockingApproval()
		rg := newRig(t, rigOptions{handlers: []toolhub.Handler{send}, interaction: handler},
			toolTurn(callOf("tc-1", "send_rfi", "{}")), aecmock.ScriptedTurn{Content: "Sent."})

		done := make(chan struct{})
		go func() {
			defer close(done)
			if _, err := rg.run(t, "go"); err != nil {
				t.Error(err)
			}
		}()
		synctest.Wait() // everything is blocked: the run waits for the user
		select {
		case <-handler.asked:
		default:
			t.Fatal("the handler was never asked")
		}
		if send.executions() != 0 {
			t.Fatal("the tool ran before the decision arrived")
		}
		if countOf(rg, events.ToolApprovalRequested.Name) != 1 || countOf(rg, events.ToolApprovalResolved.Name) != 0 {
			t.Fatalf("events = %v, want the request and no resolution yet", rg.log.types())
		}

		time.Sleep(3 * time.Second) // the user takes their time
		handler.decide <- toolhub.ApprovalDecision{Approved: true}
		<-done
		if send.executions() != 1 {
			t.Fatalf("%d executions after approval, want 1", send.executions())
		}
		if got := rg.log.payload(t, events.ToolApprovalResolved.Name); got != `{"agentId":"test-agent","name":"send_rfi","toolCallId":"tc-1","approved":true,"waitedMs":3000}` {
			t.Fatalf("ToolApprovalResolved = %s", got)
		}
	})
}

// TestApproval_StoppedRunIsNotApproved (fail closed): stopping the run while the
// user decides denies the call; the tool never runs, even if the user would have
// approved a moment later.
func TestApproval_StoppedRunIsNotApproved(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		send := consequentialTool("send_rfi")
		handler := newBlockingApproval()
		rg := newRig(t, rigOptions{handlers: []toolhub.Handler{send}, interaction: handler},
			toolTurn(callOf("tc-1", "send_rfi", "{}")), aecmock.ScriptedTurn{Content: "unreachable"})
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		done := make(chan struct{})
		go func() {
			defer close(done)
			_, _ = rg.loop.Run(ctx, "go", "s-1", llmhub.ModelOptions{}, agentloop.RunOptions{})
		}()
		synctest.Wait()
		cancel()
		synctest.Wait()
		handler.decide <- toolhub.ApprovalDecision{Approved: true} // too late: the abandoned handler call ends
		<-done

		if send.executions() != 0 {
			t.Fatal("the tool ran after the run was stopped")
		}
		want := "Approval denied: user declined (the run was stopped by the user)" + doNotRetry
		if results := resultsOf(rg); len(results) != 1 || results[0].Content != want {
			t.Fatalf("results = %+v, want %q", results, want)
		}
	})
}

// TestApproval_SandboxGatesAWriteOnAHostFolder: a tool with no risk that writes in a
// sandbox that is not isolated from the host is gated by the sandbox approval policy.
// The request carries the reason and the sandbox, and reports the effective risk of
// a gated tool without a risk: consequential.
func TestApproval_SandboxGatesAWriteOnAHostFolder(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		write := sandboxTool("write_note", sandboxhub.OperationFSWrite, nil)
		write.def.Risk = "" // no risk: the sandbox reason wins, the risk reads consequential
		handler, reqs := recordingHandler(approve())
		rg := newRig(t, rigOptions{handlers: []toolhub.Handler{write}, interaction: handler, sandboxes: newFakeSandboxes(t, sandboxhub.KindHost, "host-1")},
			toolTurn(callOf("tc-1", "write_note", `{"text":"hello"}`)), aecmock.ScriptedTurn{Content: "Noted."})
		if _, err := rg.run(t, "go"); err != nil {
			t.Fatal(err)
		}
		if write.executions() != 1 {
			t.Fatalf("%d executions, want 1", write.executions())
		}
		wantReason := `fs.write in "host-1", which is not isolated from the host`
		if len(*reqs) != 1 {
			t.Fatalf("%d approval requests, want 1", len(*reqs))
		}
		got := (*reqs)[0]
		if got.Risk != toolhub.RiskConsequential || got.Reason != wantReason || got.Sandbox == nil || *got.Sandbox != (toolhub.ApprovalSandbox{ID: "host-1", Kind: sandboxhub.KindHost, Isolated: false}) {
			t.Fatalf("request = %+v", got)
		}
		wantEvent := `{"agentId":"test-agent","name":"write_note","toolCallId":"tc-1","risk":"consequential","arguments":{"text":"hello"},"reason":"fs.write in \"host-1\", which is not isolated from the host","sandbox":{"id":"host-1","kind":"host","isolated":false}}`
		if p := rg.log.payload(t, events.ToolApprovalRequested.Name); p != wantEvent {
			t.Fatalf("ToolApprovalRequested = %s", p)
		}
		attrs, _ := rg.sink.endAttrs("write_note")
		if id, _ := attrs.Get("approvalSandbox"); id != "host-1" {
			t.Fatalf("span attributes = %v, want approvalSandbox host-1", attrs.Keys())
		}
	})
}

// TestApproval_IsolatedSandboxNeedsNoApproval: the same tool in an isolated sandbox
// (docker) runs without asking anyone.
func TestApproval_IsolatedSandboxNeedsNoApproval(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		write := sandboxTool("write_note", sandboxhub.OperationFSWrite, nil)
		handler, reqs := recordingHandler(approve())
		rg := newRig(t, rigOptions{handlers: []toolhub.Handler{write}, interaction: handler, sandboxes: newFakeSandboxes(t, sandboxhub.KindDocker, "box-1")},
			toolTurn(callOf("tc-1", "write_note", "{}")), aecmock.ScriptedTurn{Content: "ok"})
		if _, err := rg.run(t, "go"); err != nil {
			t.Fatal(err)
		}
		if write.executions() != 1 || len(*reqs) != 0 {
			t.Fatalf("%d executions, %d approval requests: an isolated sandbox needs no approval", write.executions(), len(*reqs))
		}
	})
}

// TestApproval_UnresolvedSandboxFailsClosed (WR-02): when the sandbox cannot be
// resolved the gate does not know whether the operation touches the host, so a
// sandbox tool is gated whatever its declared risk (fail closed). The request
// carries a reason and no sandbox; a consequential tool is gated the same way.
func TestApproval_UnresolvedSandboxFailsClosed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sb := newFakeSandboxes(t, sandboxhub.KindHost, "host-1")
		sb.err = errors.New("no sandbox")

		plain := sandboxTool("write_note", sandboxhub.OperationFSWrite, nil)
		risky := sandboxTool("wipe", sandboxhub.OperationExec, nil)
		risky.def.Risk = toolhub.RiskConsequential
		handler, reqs := recordingHandler(approve())
		rg := newRig(t, rigOptions{handlers: []toolhub.Handler{plain, risky}, interaction: handler, sandboxes: sb},
			toolTurn(callOf("tc-1", "write_note", "{}"), callOf("tc-2", "wipe", "{}")), aecmock.ScriptedTurn{Content: "ok"})
		if _, err := rg.run(t, "go"); err != nil {
			t.Fatal(err)
		}
		if plain.executions() != 1 || risky.executions() != 1 {
			t.Fatalf("executions %d and %d, want both (approved)", plain.executions(), risky.executions())
		}
		if len(*reqs) != 2 {
			t.Fatalf("%d approval requests, want 2: an unresolved sandbox is gated for every sandbox tool", len(*reqs))
		}
		for _, req := range *reqs {
			if !strings.Contains(req.Reason, "could not be resolved") || req.Sandbox != nil {
				t.Fatalf("request = %+v, want a reason naming the unresolved sandbox and no sandbox", req)
			}
		}
	})
}

// TestApproval_UnresolvedSandboxDeclinedNeverRuns (WR-02): the user declines (or
// nobody is there to ask), so the sandbox tool never executes, and its own sandbox
// call would have failed the same way (one resolution per dispatch).
func TestApproval_UnresolvedSandboxDeclinedNeverRuns(t *testing.T) {
	for _, tc := range []struct {
		name string
		ask  bool
	}{{"declined", true}, {"no handler", false}} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				sb := newFakeSandboxes(t, sandboxhub.KindHost, "host-1")
				sb.err = errors.New("no sandbox")
				plain := sandboxTool("write_note", sandboxhub.OperationFSWrite, nil)
				opts := rigOptions{handlers: []toolhub.Handler{plain}, sandboxes: sb}
				if tc.ask {
					handler, _ := recordingHandler(func(context.Context, toolhub.ApprovalRequest) (toolhub.ApprovalDecision, error) {
						return toolhub.ApprovalDecision{Approved: false}, nil
					})
					opts.interaction = handler
				}
				rg := newRig(t, opts, toolTurn(callOf("tc-1", "write_note", "{}")), aecmock.ScriptedTurn{Content: "ok"})
				if _, err := rg.run(t, "go"); err != nil {
					t.Fatal(err)
				}
				if plain.executions() != 0 {
					t.Fatal("a sandbox tool ran although its sandbox was never resolved and nobody approved it")
				}
			})
		})
	}
}

// flipSandboxes is a sandbox access whose binding changes between calls: the first
// resolution returns the first view, every later one the second view.
type flipSandboxes struct {
	mu    sync.Mutex
	views []sandboxhub.View
	hub   *sandboxhub.SandboxHub
	calls int
}

func (f *flipSandboxes) GetSandbox(context.Context) (sandboxhub.View, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := min(f.calls, len(f.views)-1)
	f.calls++
	return f.views[i], nil
}

func (f *flipSandboxes) RequiresApproval(traits sandboxhub.Traits, op sandboxhub.Operation) bool {
	return f.hub.RequiresApproval(traits, op)
}

func (f *flipSandboxes) resolutions() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// TestApproval_GateAndToolShareOneSandboxResolution (WR-02): the gate sees an
// isolated sandbox (no approval needed) and the binding then flips to a host folder.
// The tool must get the sandbox the gate judged, not the new binding, and the
// resolver runs once for the whole dispatch.
func TestApproval_GateAndToolShareOneSandboxResolution(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		docker := newFakeSandboxes(t, sandboxhub.KindDocker, "box-1")
		host := newFakeSandboxes(t, sandboxhub.KindHost, "host-1")
		flip := &flipSandboxes{views: []sandboxhub.View{docker.view, host.view}, hub: docker.hub}

		var seen []string
		write := sandboxTool("write_note", sandboxhub.OperationFSWrite, func(ctx context.Context, _ kernel.Arguments, tc toolhub.Context) (toolhub.Output, error) {
			for range 2 { // a retry or a second use inside the tool gets the same view
				view, err := tc.GetSandbox(ctx)
				if err != nil {
					return toolhub.Output{}, err
				}
				seen = append(seen, view.ID())
			}
			return toolhub.Output{Content: "saved"}, nil
		})
		handler, reqs := recordingHandler(approve())
		rg := newRig(t, rigOptions{handlers: []toolhub.Handler{write}, interaction: handler, sandboxes: flip},
			toolTurn(callOf("tc-1", "write_note", "{}")), aecmock.ScriptedTurn{Content: "ok"})
		if _, err := rg.run(t, "go"); err != nil {
			t.Fatal(err)
		}
		if len(*reqs) != 0 {
			t.Fatalf("%d approval requests, want none: the gate judged an isolated sandbox", len(*reqs))
		}
		if len(seen) != 2 || seen[0] != "box-1" || seen[1] != "box-1" {
			t.Fatalf("the tool saw sandboxes %v, want the gate's box-1 both times", seen)
		}
		if got := flip.resolutions(); got != 1 {
			t.Fatalf("the sandbox was resolved %d times, want once per dispatch", got)
		}
	})
}

// TestApproval_SandboxGateWithoutAHandlerNamesTheReason: a sandbox-gated tool whose
// declared risk is not consequential gets the second no-handler text, with the
// sandbox reason in it.
func TestApproval_SandboxGateWithoutAHandlerNamesTheReason(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		write := sandboxTool("write_note", sandboxhub.OperationFSWrite, nil)
		write.def.Risk = toolhub.RiskDraft
		rg := newRig(t, rigOptions{handlers: []toolhub.Handler{write}, sandboxes: newFakeSandboxes(t, sandboxhub.KindHost, "host-1")},
			toolTurn(callOf("tc-1", "write_note", "{}")), aecmock.ScriptedTurn{Content: "ok"})
		if _, err := rg.run(t, "go"); err != nil {
			t.Fatal(err)
		}
		want := `Approval denied: no interactive user is available to approve this action (fs.write in "host-1", which is not isolated from the host)` + doNotRetry
		if results := resultsOf(rg); len(results) != 1 || results[0].Content != want || write.executions() != 0 {
			t.Fatalf("results = %+v, executions %d", results, write.executions())
		}
	})
}

// TestApproval_SkillsSkipTheGate: skill_view is not gated even when it is declared
// consequential, and no handler is needed.
func TestApproval_SkillsSkipTheGate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		view := skillViewTool(map[string]string{"plan": "Planning instructions"})
		view.def.Risk = toolhub.RiskConsequential
		rg := newRig(t, rigOptions{skills: aecmock.NewSkillTools(view)},
			toolTurn(callOf("tc-1", "skill_view", `{"name":"plan"}`)), aecmock.ScriptedTurn{Content: "ok"})
		if _, err := rg.run(t, "go"); err != nil {
			t.Fatal(err)
		}
		if view.executions() != 1 || countOf(rg, events.ToolDenied.Name) != 0 {
			t.Fatalf("executions %d, events %v", view.executions(), rg.log.types())
		}
	})
}

// TestApproval_PolicyDenialStopsBeforeApprovalAndExecution: a policy that denies
// gives ToolDenied{reason} and "Policy denied: <reason>" before any approval is asked.
func TestApproval_PolicyDenialStopsBeforeApprovalAndExecution(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		send := consequentialTool("send_rfi")
		approvals := &approvalCounter{}
		rg := newRig(t, rigOptions{handlers: []toolhub.Handler{send}, policy: denyAll{}, interaction: approvals},
			toolTurn(callOf("tc-1", "send_rfi", "{}")), aecmock.ScriptedTurn{Content: "ok"})
		if _, err := rg.run(t, "go"); err != nil {
			t.Fatal(err)
		}
		if send.executions() != 0 || approvals.requests.Load() != 0 {
			t.Fatalf("executions %d, approval requests %d: a denial comes first", send.executions(), approvals.requests.Load())
		}
		if results := resultsOf(rg); len(results) != 1 || results[0].Content != "Policy denied: blocked" || results[0].IsError == nil || !*results[0].IsError {
			t.Fatalf("results = %+v", results)
		}
		if got := rg.log.payload(t, events.ToolDenied.Name); got != `{"agentId":"test-agent","name":"send_rfi","reason":"blocked"}` {
			t.Fatalf("ToolDenied = %s", got)
		}
		attrs, _ := rg.sink.endAttrs("send_rfi")
		if e, _ := attrs.Get("error"); e != "policy-denied" {
			t.Fatalf("span end = %v", attrs.Keys())
		}
	})
}

// reasonlessPolicy denies with the given decision.
type reasonlessPolicy struct{ decision toolhub.Decision }

func (reasonlessPolicy) Name() string    { return "policy:reasonless" }
func (reasonlessPolicy) Version() string { return "1.0.0" }
func (p reasonlessPolicy) Check(context.Context, string, kernel.Arguments, toolhub.Context, toolhub.Definition, []string) toolhub.Decision {
	return p.decision
}

// TestApproval_PolicyDenialWithoutAReason: a denial with no reason, or an empty one,
// reads just "Policy denied" (never "Policy denied: undefined" or "Policy denied: ").
// The event keeps the decision's reason as it is: absent, or empty.
func TestApproval_PolicyDenialWithoutAReason(t *testing.T) {
	for _, tc := range []struct {
		name     string
		decision toolhub.Decision
		text     string
		payload  string
	}{
		{"no reason", toolhub.Decision{}, "Policy denied", `{"agentId":"test-agent","name":"echo"}`},
		{"empty reason", toolhub.Decision{Reason: strRef("")}, "Policy denied", `{"agentId":"test-agent","name":"echo","reason":""}`},
		{"a reason", toolhub.Decision{Reason: strRef("not on weekends")}, "Policy denied: not on weekends", `{"agentId":"test-agent","name":"echo","reason":"not on weekends"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				rg := newRig(t, rigOptions{handlers: []toolhub.Handler{constTool("echo", toolhub.Output{Content: "ok"})}, policy: reasonlessPolicy{tc.decision}},
					toolTurn(callOf("tc-1", "echo", "{}")), aecmock.ScriptedTurn{Content: "ok"})
				if _, err := rg.run(t, "go"); err != nil {
					t.Fatal(err)
				}
				if results := resultsOf(rg); len(results) != 1 || results[0].Content != tc.text {
					t.Fatalf("results = %+v, want %q", results, tc.text)
				}
				if got := rg.log.payload(t, events.ToolDenied.Name); got != tc.payload {
					t.Fatalf("ToolDenied = %s, want %s", got, tc.payload)
				}
			})
		})
	}
}

// TestTracedSandbox_OperationsAreSpansUnderTheDispatchSpan: a tool that writes through
// its context's sandbox leaves a sandbox-op span under its own dispatch span, with
// the attribute keys sorted (the Phase 7 contract is map-based).
func TestTracedSandbox_OperationsAreSpansUnderTheDispatchSpan(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		write := sandboxTool("write_note", sandboxhub.OperationFSWrite, func(ctx context.Context, _ kernel.Arguments, tc toolhub.Context) (toolhub.Output, error) {
			view, err := tc.GetSandbox(ctx)
			if err != nil {
				return toolhub.Output{}, err
			}
			if _, err := view.WriteFile(ctx, "note.txt", "hello", sandboxhub.WriteOptions{}); err != nil {
				return toolhub.Output{}, err
			}
			return toolhub.Output{Content: "saved"}, nil
		})
		handler, _ := recordingHandler(approve())
		rg := newRig(t, rigOptions{handlers: []toolhub.Handler{write}, interaction: handler, sandboxes: newFakeSandboxes(t, sandboxhub.KindDocker, "box-1")},
			toolTurn(callOf("tc-1", "write_note", "{}")), aecmock.ScriptedTurn{Content: "ok"})
		if _, err := rg.run(t, "go"); err != nil {
			t.Fatal(err)
		}
		if got := rg.sink.parentName("fs.write"); got != "write_note" {
			t.Fatalf("the sandbox-op span hangs under %q, want the dispatch span write_note", got)
		}
		var start ordered.Object
		for _, r := range rg.sink.all() {
			if r.phase == "start" && r.span.Name == "fs.write" {
				if r.span.Kind != "sandbox-op" {
					t.Fatalf("kind = %q", r.span.Kind)
				}
				start = r.span.Attributes
			}
		}
		if keys := start.Keys(); !slices.IsSorted(keys) || !slices.Contains(keys, "sandboxId") || !slices.Contains(keys, "agentId") {
			t.Fatalf("start attributes %v: want sorted keys incl. sandboxId and agentId", keys)
		}
		// The ended span holds the start attributes and then the keys the end added
		// (Object.assign order): the new ones come sorted too.
		end, ok := rg.sink.endAttrs("fs.write")
		added := end.Keys()[min(len(start.Keys()), len(end.Keys())):]
		if !ok || len(added) == 0 || !slices.IsSorted(added) {
			t.Fatalf("end attributes %v: the keys added at the end must be sorted", end.Keys())
		}
		if started, ended := rg.sink.startedAndEnded(); started != ended {
			t.Fatalf("%d spans started, %d ended", started, ended)
		}
	})
}

// The approval gate for an unset Risk.

// TestApproval_UnsetRiskFailsClosed: a tool that declares no Risk is gated like a
// consequential one (fail closed). With nobody to ask it is denied
// and never runs; with an approving user it runs once, and the request reports the
// effective risk, consequential.
func TestApproval_UnsetRiskFailsClosed(t *testing.T) {
	t.Run("no handler", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ft := constTool("mystery", toolhub.Output{Content: "done"})
			ft.def.Risk = ""
			rg := newRig(t, rigOptions{handlers: []toolhub.Handler{ft}},
				toolTurn(callOf("tc-1", "mystery", "{}")), aecmock.ScriptedTurn{Content: "ok"})
			if _, err := rg.run(t, "go"); err != nil {
				t.Fatal(err)
			}
			if ft.executions() != 0 {
				t.Fatal("a tool without a Risk ran with nobody to approve it")
			}
			want := "Approval denied: no interactive user is available to approve this consequential action" + doNotRetry
			if results := resultsOf(rg); len(results) != 1 || results[0].Content != want {
				t.Fatalf("results = %+v, want %q", results, want)
			}
		})
	})
	t.Run("approved", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ft := constTool("mystery", toolhub.Output{Content: "done"})
			ft.def.Risk = ""
			handler, reqs := recordingHandler(approve())
			rg := newRig(t, rigOptions{handlers: []toolhub.Handler{ft}, interaction: handler},
				toolTurn(callOf("tc-1", "mystery", "{}")), aecmock.ScriptedTurn{Content: "ok"})
			if _, err := rg.run(t, "go"); err != nil {
				t.Fatal(err)
			}
			if ft.executions() != 1 || len(*reqs) != 1 || (*reqs)[0].Risk != toolhub.RiskConsequential {
				t.Fatalf("%d executions, requests %+v: want one approved run reported as consequential", ft.executions(), *reqs)
			}
		})
	})
	t.Run("declared read-only", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ft := constTool("lookup", toolhub.Output{Content: "found"})
			rg := newRig(t, rigOptions{handlers: []toolhub.Handler{ft}},
				toolTurn(callOf("tc-1", "lookup", "{}")), aecmock.ScriptedTurn{Content: "ok"})
			if _, err := rg.run(t, "go"); err != nil {
				t.Fatal(err)
			}
			if ft.executions() != 1 || countOf(rg, events.ToolApprovalRequested.Name) != 0 {
				t.Fatalf("%d executions, events %v: a read-only tool runs without approval", ft.executions(), rg.log.types())
			}
		})
	})
}
