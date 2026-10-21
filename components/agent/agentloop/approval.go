package agentloop

import (
	"context"
	"time"

	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/components/trace"
	"github.com/Phong95/aec-agent-go/internal/nilcheck"
	"github.com/Phong95/aec-agent-go/kernel"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// gate says why a call needs the user's approval: reason and
// sandbox are both optional, a consequential tool alone gives an empty gate.
type gate struct {
	reason  string
	sandbox *toolhub.ApprovalSandbox
}

// approvalResult is the outcome of requestApproval: a denied call carries its
// result, and trace is what the dispatch span records under "approval".
type approvalResult struct {
	denied *kernel.ToolResult
	trace  ordered.Object
}

// effectiveRisk is the risk reported for a gated call: a tool without a risk that is
// gated by its sandbox counts as consequential. The approval gate and the retry rule
// use it too: a tool that declares no Risk is treated as consequential (fail closed).
func effectiveRisk(def toolhub.Definition) toolhub.Risk {
	if def.Risk == "" {
		return toolhub.RiskConsequential
	}
	return def.Risk
}

// callDefinition is the definition the approval gate and the retry rule judge for this
// call (A-101): the tool's definition with the call's own risk (toolhub.Authorized.Risk,
// e.g. http_request GET is read-only, POST consequential).
func callDefinition(auth toolhub.Authorized) toolhub.Definition {
	def := auth.Definition()
	def.Risk = auth.Risk()
	return def
}

// gateCall runs the approval gate for one call: it decides whether the call needs
// the user's approval, asks when it does, and records the outcome on the dispatch
// span (risk, approval, approvalSandbox). It returns the denial result, or nil when
// the call may run (no gate applies, or the user approved).
func (e *executor) gateCall(ctx context.Context, run *toolRun, call kernel.ToolCall, tc toolhub.Context, def toolhub.Definition, span trace.SpanHandle) *kernel.ToolResult {
	g := e.approvalGate(ctx, def, tc)
	if g == nil {
		return nil
	}
	// A-135: a session in YOLO mode runs the call without asking (read now, so a switch
	// during the turn applies to its next call).
	var approval approvalResult
	if e.approvalMode(ctx, tc.SessionID) == kernel.ApprovalModeYolo {
		approval = e.autoApprove(run, def, call, g)
	} else {
		approval = e.requestApproval(ctx, run, def, call, tc, g)
	}
	if span != nil {
		attrs := obj("risk", string(effectiveRisk(def)), "approval", approval.trace)
		if g.sandbox != nil {
			attrs.Set("approvalSandbox", g.sandbox.ID)
		}
		span.SetAttributes(attrs)
	}
	return approval.denied
}

// approvalGate asks whether this call needs the user's approval, and why. A sandbox
// tool resolves the agent's sandbox here, which is the
// sandbox selection before dispatch. The executor hands the gate and the tool one
// shared resolution (see resolveOnce), so the sandbox the gate judged is the sandbox
// the tool acts in.
//
// A sandbox tool whose sandbox cannot be resolved is gated (fail closed): without
// the sandbox's traits nobody knows whether the operation touches the host, so the
// user decides.
//
// A tool that declares no Risk is gated like a consequential one (fail closed):
// nobody said it is safe to run unattended. Every built-in tool declares its Risk.
func (e *executor) approvalGate(ctx context.Context, def toolhub.Definition, tc toolhub.Context) *gate {
	consequential := effectiveRisk(def) == toolhub.RiskConsequential
	op := def.SandboxOperation
	if op == "" || tc.GetSandbox == nil || nilcheck.IsNil(e.deps.Sandbox) {
		return consequentialGate(consequential, nil)
	}

	view, err := tc.GetSandbox(ctx)
	if err != nil || nilcheck.IsNil(view) {
		return &gate{reason: string(op) + " in the agent's sandbox, which could not be resolved, so its isolation from the host is unknown"}
	}
	traits := view.Traits()
	sb := &toolhub.ApprovalSandbox{ID: view.ID(), Kind: traits.Kind, Isolated: traits.Isolated}
	if e.deps.Sandbox.RequiresApproval(traits, op) {
		not := "not "
		if traits.Isolated {
			not = ""
		}
		return &gate{
			sandbox: sb,
			reason:  string(op) + ` in "` + view.ID() + `", which is ` + not + "isolated from the host",
		}
	}
	return consequentialGate(consequential, sb)
}

// consequentialGate is the gate of a consequential tool, or none.
func consequentialGate(consequential bool, sb *toolhub.ApprovalSandbox) *gate {
	if !consequential {
		return nil
	}
	return &gate{sandbox: sb}
}

// approvalMode is the session's approval mode (A-135), from Deps.ApprovalMode. No
// ApprovalMode dep, or an error reading it, is ApprovalModeAsk (fail closed).
func (e *executor) approvalMode(ctx context.Context, sessionID string) kernel.ApprovalMode {
	if e.deps.ApprovalMode == nil {
		return kernel.ApprovalModeAsk
	}
	mode, err := e.deps.ApprovalMode(ctx, sessionID)
	if err != nil {
		return kernel.ApprovalModeAsk
	}
	return mode
}

// autoApprove (A-135): the session is in YOLO mode - the call runs without asking; the
// event and the span say so.
func (e *executor) autoApprove(run *toolRun, def toolhub.Definition, call kernel.ToolCall, g *gate) approvalResult {
	r := run.r
	auto := events.ToolAutoApprovedPayload{
		AgentID:    r.agentID,
		Name:       call.Name,
		ToolCallID: call.ID,
		Risk:       string(effectiveRisk(def)),
		Mode:       kernel.ApprovalModeYolo,
		Reason:     g.reason,
	}
	if g.sandbox != nil {
		auto.Sandbox = &events.ApprovalSandboxPayload{ID: g.sandbox.ID, Kind: string(g.sandbox.Kind), Isolated: g.sandbox.Isolated}
	}
	emit(r, events.ToolAutoApproved, auto)
	return approvalResult{trace: obj("approved", true, "auto", string(kernel.ApprovalModeYolo))}
}

// requestApproval pauses the call for the user. It fails
// closed: no interaction channel, a denial, a handler error and a stopped run all
// block the call, and the tool does not execute before the decision arrives.
func (e *executor) requestApproval(ctx context.Context, run *toolRun, def toolhub.Definition, call kernel.ToolCall, tc toolhub.Context, g *gate) approvalResult {
	r := run.r
	risk := effectiveRisk(def)

	deny := func(reason string, trace ordered.Object) approvalResult {
		emit(r, events.ToolDenied, events.ToolDeniedPayload{AgentID: r.agentID, Name: call.Name, Reason: &reason})
		res := errorResult(call, "Approval denied: "+reason+". Do not retry this action unless the user asks for it.")
		return approvalResult{denied: &res, trace: trace}
	}

	handler := tc.Interaction
	if nilcheck.IsNil(handler) {
		text := "no interactive user is available to approve this consequential action"
		if risk != toolhub.RiskConsequential {
			why := g.reason
			if why == "" {
				why = "approval required"
			}
			text = "no interactive user is available to approve this action (" + why + ")"
		}
		return deny(text, obj("approved", false, "reason", "no-interaction-handler"))
	}

	requested := events.ToolApprovalRequestedPayload{
		AgentID:    r.agentID,
		Name:       call.Name,
		ToolCallID: call.ID,
		Risk:       string(risk),
		Arguments:  call.Arguments,
		Reason:     g.reason,
	}
	req := toolhub.ApprovalRequest{
		SessionID:       tc.SessionID,
		AgentID:         tc.AgentID,
		ToolCallID:      call.ID,
		ToolName:        call.Name,
		ToolDescription: def.Description,
		Risk:            risk,
		Arguments:       call.Arguments,
		Reason:          g.reason,
		Sandbox:         g.sandbox,
	}
	if g.sandbox != nil {
		requested.Sandbox = &events.ApprovalSandboxPayload{ID: g.sandbox.ID, Kind: string(g.sandbox.Kind), Isolated: g.sandbox.Isolated}
	}
	emit(r, events.ToolApprovalRequested, requested)

	startedAt := time.Now()
	approved := false
	var reason *string
	// The wait is raced against the run: an aborted wait is NOT approved (fail
	// closed) and follows the same deny path as an explicit decline.
	decision, aborted, err := raceAbort(ctx, func() (toolhub.ApprovalDecision, error) {
		return handler.RequestApproval(ctx, req)
	})
	switch {
	case aborted:
		reason = strPtr("the run was stopped by the user")
	case err != nil:
		reason = strPtr("approval request failed: " + err.Error())
	default:
		approved = decision.Approved
		reason = decision.Reason
	}
	waitedMs := time.Since(startedAt).Milliseconds()

	emit(r, events.ToolApprovalResolved, events.ToolApprovalResolvedPayload{
		AgentID:    r.agentID,
		Name:       call.Name,
		ToolCallID: call.ID,
		Approved:   approved,
		Reason:     reason,
		WaitedMs:   waitedMs,
	})

	tr := obj("approved", approved)
	if reason != nil {
		tr.Set("reason", *reason)
	}
	tr.Set("waitedMs", waitedMs)
	if !approved {
		if reason != nil && *reason != "" {
			return deny("user declined ("+*reason+")", tr)
		}
		return deny("user declined", tr)
	}
	return approvalResult{trace: tr}
}

func strPtr(s string) *string { return &s }
