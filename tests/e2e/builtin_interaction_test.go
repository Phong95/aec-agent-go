package e2e_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/Phong95/aec-agent-go/aecmock"
	"github.com/Phong95/aec-agent-go/components/agent/profile"
	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/components/toolhub/builtin/clarify"
	"github.com/Phong95/aec-agent-go/kernel"
)

// This file holds the ten loop-level interaction-control cases at the root level (the
// four ClarifyTool unit cases are TestClarifyTool in builtin/clarify). Each case runs
// over a real AgentCore with an aecmock.ScriptedProvider: the model calls one tool in its
// first turn and ends the turn in its second, and the test reads the tool result the
// model saw in the second call (see lastToolResult). The interaction handler is an
// aecmock.ScriptedInteraction set on the tool hub, so one handler receives both the
// approval requests and the clarify prompts (TLS-02).
//
// The subtests are flat and carry descriptive titles, so a failure points at its case.

// interactionLoopConfig is the LoopConfig of the interaction-control setup: two retries
// and no backoff, so a failing tool runs three times and the test does not wait.
func interactionLoopConfig(p *profile.AgentProfile) {
	retries, backoff := 2, 0
	p.Name = "interaction-test"
	p.SystemPrompt = "You are a test agent."
	p.LoopConfig = &profile.LoopConfig{MaxToolRetries: &retries, RetryBackoffMs: &backoff}
}

// sendRfiTool is a tool whose risk is consequential.
func sendRfiTool(run func(context.Context, kernel.Arguments, toolhub.Context) (toolhub.Output, error)) *loopTool {
	t := newLoopTool("send_rfi", run)
	t.def.Description = "Send an RFI email to an external consultant"
	t.def.Parameters = toolhub.JSONSchema{
		Type:       "object",
		Properties: toolhub.NewProperties(toolhub.Property{Name: "to", Schema: toolhub.JSONSchema{Type: "string"}}),
		Required:   []string{"to"},
	}
	t.def.Risk = toolhub.RiskConsequential
	return t
}

// sentOK is a tool body that succeeds with "sent".
func sentOK(context.Context, kernel.Arguments, toolhub.Context) (toolhub.Output, error) {
	return toolhub.Output{Content: "sent"}, nil
}

// lastToolResult is the first tool result of the last
// message that has any, over the messages of the last model call that has one.
func lastToolResult(t *testing.T, p *aecmock.ScriptedProvider) kernel.ToolResult {
	t.Helper()
	calls := p.Calls()
	for i := len(calls) - 1; i >= 0; i-- {
		msgs := calls[i].Messages
		for j := len(msgs) - 1; j >= 0; j-- {
			if len(msgs[j].ToolResults) > 0 {
				return msgs[j].ToolResults[0]
			}
		}
	}
	t.Fatal("the model never saw a tool result")
	return kernel.ToolResult{}
}

// toolTurns are the two scripted turns of a one-tool-call model: one call of the tool
// named name with the given arguments, then the end of the turn.
func toolTurns(name, args string) []aecmock.ScriptedTurn {
	return []aecmock.ScriptedTurn{
		toolTurn(toolCallOf("tc-1", name, args)),
		{Content: "done"},
	}
}

func isErrorResult(r kernel.ToolResult) bool { return r.IsError != nil && *r.IsError }

// failingAnswer is the handler stub whose askUser throws.
func failingAnswer(msg string) aecmock.InteractionOption {
	return aecmock.WithAnswerFunc(func(context.Context, toolhub.ClarifyRequest) (toolhub.ClarifyResponse, error) {
		return toolhub.ClarifyResponse{}, errors.New(msg)
	})
}

// resolvedPayloads decodes the payloads of the approval-resolved events, whatever Go type
// the loop emitted them as.
func resolvedPayloads(t *testing.T, l *loopCore) []events.ToolApprovalResolvedPayload {
	t.Helper()
	var out []events.ToolApprovalResolvedPayload
	for _, e := range l.log.all() {
		if e.Type != events.ToolApprovalResolved.Name {
			continue
		}
		raw, err := kernel.EncodeJSON(e.Payload)
		if err != nil {
			t.Fatal(err)
		}
		var p events.ToolApprovalResolvedPayload
		if err := json.Unmarshal(raw, &p); err != nil {
			t.Fatalf("decode %s: %v", raw, err)
		}
		out = append(out, p)
	}
	return out
}

func TestInteractionControl(t *testing.T) {
	const sendArgs = `{"to":"consultant@example.com"}`

	// ---- clarify in the agent loop ----

	t.Run("passes the session/agent to askUser and feeds the answer back to the model", func(t *testing.T) {
		handler := aecmock.NewScriptedInteraction()
		l := newLoopCore(t, loopOptions{profile: interactionLoopConfig, tools: []toolhub.Handler{clarify.NewTool()}, interaction: handler},
			toolTurns("clarify", `{"question":"Which tower?"}`))
		defer l.dispose(t)

		res, err := l.chat(t.Context(), "Draft the RFI")
		if err != nil {
			t.Fatalf("Chat: %v", err)
		}
		asked := handler.Asked()
		if len(asked) != 1 {
			t.Fatalf("askUser was called %d times, want once", len(asked))
		}
		if asked[0].AgentID != l.agent.Profile().ID || asked[0].SessionID != l.session.ID || asked[0].Question != "Which tower?" {
			t.Errorf("request = %+v, want agent %q session %q question %q", asked[0], l.agent.Profile().ID, l.session.ID, "Which tower?")
		}
		if got := lastToolResult(t, l.provider).Content; got != "User's answer: Tower A" {
			t.Errorf("the model saw %q, want %q", got, "User's answer: Tower A")
		}
		if res.Message.Content != "done" {
			t.Errorf("reply = %q, want done", res.Message.Content)
		}
	})

	t.Run("does not re-ask the user when askUser fails (no retry)", func(t *testing.T) {
		handler := aecmock.NewScriptedInteraction(failingAnswer("disconnected"))
		l := newLoopCore(t, loopOptions{profile: interactionLoopConfig, tools: []toolhub.Handler{clarify.NewTool()}, interaction: handler},
			toolTurns("clarify", `{"question":"Which tower?"}`))
		defer l.dispose(t)

		if _, err := l.chat(t.Context(), "Draft the RFI"); err != nil {
			t.Fatalf("Chat: %v", err)
		}
		if n := len(handler.Asked()); n != 1 {
			t.Errorf("askUser was called %d times, want once: a clarify failure is not retried", n)
		}
		got := lastToolResult(t, l.provider)
		if !isErrorResult(got) || !strings.Contains(got.Content, "clarify failed: disconnected.") {
			t.Errorf("tool result = %+v, want the clarify failed text", got)
		}
	})

	// ---- approval gate for consequential tools ----

	t.Run("denies a consequential tool when no interaction handler is configured (fail closed)", func(t *testing.T) {
		send := sendRfiTool(sentOK)
		l := newLoopCore(t, loopOptions{profile: interactionLoopConfig, tools: []toolhub.Handler{send}}, toolTurns("send_rfi", sendArgs))
		defer l.dispose(t)

		if _, err := l.chat(t.Context(), "Send it"); err != nil {
			t.Fatalf("Chat: %v", err)
		}
		if n := send.executions(); n != 0 {
			t.Errorf("the tool ran %d times, want never", n)
		}
		got := lastToolResult(t, l.provider)
		if !isErrorResult(got) || !strings.Contains(got.Content, "Approval denied: no interactive user") {
			t.Errorf("tool result = %+v, want an Approval denied: no interactive user error", got)
		}
		if l.log.count(events.ToolDenied.Name) == 0 {
			t.Error("no tool-denied event was emitted")
		}
	})

	t.Run("runs the tool once after the user approves, with the full request shown", func(t *testing.T) {
		send := sendRfiTool(sentOK)
		handler := aecmock.NewScriptedInteraction()
		l := newLoopCore(t, loopOptions{profile: interactionLoopConfig, tools: []toolhub.Handler{send}, interaction: handler}, toolTurns("send_rfi", sendArgs))
		defer l.dispose(t)

		if _, err := l.chat(t.Context(), "Send it"); err != nil {
			t.Fatalf("Chat: %v", err)
		}
		approvals := handler.Approvals()
		if len(approvals) != 1 {
			t.Fatalf("requestApproval was called %d times, want once", len(approvals))
		}
		req := approvals[0]
		if req.AgentID != l.agent.Profile().ID || req.SessionID != l.session.ID || req.ToolCallID != "tc-1" || req.ToolName != "send_rfi" ||
			req.ToolDescription != "Send an RFI email to an external consultant" || req.Risk != toolhub.RiskConsequential {
			t.Errorf("request = %+v", req)
		}
		if string(req.Arguments) != sendArgs {
			t.Errorf("request arguments = %s, want the raw bytes %s", req.Arguments, sendArgs)
		}
		if n := send.executions(); n != 1 {
			t.Errorf("the tool ran %d times, want once", n)
		}
		if got := lastToolResult(t, l.provider).Content; got != "sent" {
			t.Errorf("the model saw %q, want sent", got)
		}

		types := l.log.types()
		if !slices.Contains(types, events.ToolApprovalRequested.Name) {
			t.Error("no tool-approval-request event")
		}
		resolved := resolvedPayloads(t, l)
		if len(resolved) != 1 || !resolved[0].Approved || resolved[0].Name != "send_rfi" {
			t.Errorf("approval-resolved events = %+v, want one approved send_rfi", resolved)
		}
		if slices.Index(types, events.ToolApprovalResolved.Name) >= slices.Index(types, events.ToolCompleted.Name) {
			t.Errorf("the approval must be resolved before the tool completes: %v", types)
		}
	})

	t.Run("does not run the tool when the user declines, and tells the model why", func(t *testing.T) {
		send := sendRfiTool(sentOK)
		reason := "wrong recipient"
		handler := aecmock.NewScriptedInteraction(aecmock.WithApprovalFunc(func(context.Context, toolhub.ApprovalRequest) (toolhub.ApprovalDecision, error) {
			return toolhub.ApprovalDecision{Approved: false, Reason: &reason}, nil
		}))
		l := newLoopCore(t, loopOptions{profile: interactionLoopConfig, tools: []toolhub.Handler{send}, interaction: handler}, toolTurns("send_rfi", sendArgs))
		defer l.dispose(t)

		if _, err := l.chat(t.Context(), "Send it"); err != nil {
			t.Fatalf("Chat: %v", err)
		}
		if n := send.executions(); n != 0 {
			t.Errorf("the tool ran %d times, want never", n)
		}
		if n := len(handler.Approvals()); n != 1 {
			t.Errorf("requestApproval was called %d times, want once", n)
		}
		got := lastToolResult(t, l.provider)
		if !isErrorResult(got) || !strings.Contains(got.Content, "wrong recipient") {
			t.Errorf("tool result = %+v, want an error that carries the reason", got)
		}
	})

	t.Run("fails closed when the approval request throws", func(t *testing.T) {
		send := sendRfiTool(sentOK)
		handler := aecmock.NewScriptedInteraction(aecmock.WithApprovalFunc(func(context.Context, toolhub.ApprovalRequest) (toolhub.ApprovalDecision, error) {
			return toolhub.ApprovalDecision{}, errors.New("timeout")
		}))
		l := newLoopCore(t, loopOptions{profile: interactionLoopConfig, tools: []toolhub.Handler{send}, interaction: handler}, toolTurns("send_rfi", sendArgs))
		defer l.dispose(t)

		if _, err := l.chat(t.Context(), "Send it"); err != nil {
			t.Fatalf("Chat: %v", err)
		}
		if n := send.executions(); n != 0 {
			t.Errorf("the tool ran %d times, want never", n)
		}
		if got := lastToolResult(t, l.provider).Content; !strings.Contains(got, "approval request failed: timeout") {
			t.Errorf("the model saw %q, want the approval request failed text", got)
		}
	})

	t.Run("asks for approval once and never re-runs an approved consequential tool that fails", func(t *testing.T) {
		send := sendRfiTool(func(context.Context, kernel.Arguments, toolhub.Context) (toolhub.Output, error) {
			failed := true
			return toolhub.Output{Content: "smtp down", IsError: &failed}, nil
		})
		handler := aecmock.NewScriptedInteraction()
		l := newLoopCore(t, loopOptions{profile: interactionLoopConfig, tools: []toolhub.Handler{send}, interaction: handler}, toolTurns("send_rfi", sendArgs))
		defer l.dispose(t)

		if _, err := l.chat(t.Context(), "Send it"); err != nil {
			t.Fatalf("Chat: %v", err)
		}
		if n := len(handler.Approvals()); n != 1 {
			t.Errorf("requestApproval was called %d times, want once", n)
		}
		// A consequential tool that already ran is never retried: a retry would repeat its
		// side effect (another RFI sent) on the one approval.
		if n := send.executions(); n != 1 {
			t.Errorf("the tool ran %d times, want once (no retry of a consequential tool)", n)
		}
	})

	t.Run("does not ask approval for read-only tools", func(t *testing.T) {
		for _, risk := range []toolhub.Risk{toolhub.RiskReadOnly} {
			send := sendRfiTool(func(context.Context, kernel.Arguments, toolhub.Context) (toolhub.Output, error) {
				return toolhub.Output{Content: "ok"}, nil
			})
			send.def.Risk = risk
			handler := aecmock.NewScriptedInteraction()
			l := newLoopCore(t, loopOptions{profile: interactionLoopConfig, tools: []toolhub.Handler{send}, interaction: handler}, toolTurns("send_rfi", sendArgs))

			if _, err := l.chat(t.Context(), "Look it up"); err != nil {
				t.Fatalf("Chat (risk %q): %v", risk, err)
			}
			if n := len(handler.Approvals()); n != 0 {
				t.Errorf("risk %q: requestApproval was called %d times, want never", risk, n)
			}
			if n := send.executions(); n != 1 {
				t.Errorf("risk %q: the tool ran %d times, want once", risk, n)
			}
			l.dispose(t)
		}
	})

	t.Run("picks up an interaction handler registered after the agent was spawned", func(t *testing.T) {
		send := sendRfiTool(sentOK)
		l := newLoopCore(t, loopOptions{profile: interactionLoopConfig, tools: []toolhub.Handler{send}}, toolTurns("send_rfi", sendArgs))
		defer l.dispose(t)

		handler := aecmock.NewScriptedInteraction()
		l.core.ToolHub().SetInteractionHandler(handler)
		if _, err := l.chat(t.Context(), "Send it"); err != nil {
			t.Fatalf("Chat: %v", err)
		}
		if n := len(handler.Approvals()); n != 1 {
			t.Errorf("requestApproval was called %d times, want once", n)
		}
		if n := send.executions(); n != 1 {
			t.Errorf("the tool ran %d times, want once", n)
		}
	})

	// ---- non-retryable tool errors ----

	t.Run("are returned to the model without retries", func(t *testing.T) {
		strict := newLoopTool("strict", func(context.Context, kernel.Arguments, toolhub.Context) (toolhub.Output, error) {
			failed, retry := true, false
			return toolhub.Output{Content: "bad input", IsError: &failed, Retryable: &retry}, nil
		})
		l := newLoopCore(t, loopOptions{profile: interactionLoopConfig, tools: []toolhub.Handler{strict}}, toolTurns("strict", `{}`))
		defer l.dispose(t)

		if _, err := l.chat(t.Context(), "go"); err != nil {
			t.Fatalf("Chat: %v", err)
		}
		if n := strict.executions(); n != 1 {
			t.Errorf("the tool ran %d times, want once", n)
		}
		got := lastToolResult(t, l.provider)
		if got.Content != "bad input" || !isErrorResult(got) {
			t.Errorf("tool result = %+v, want the error bad input", got)
		}
	})
}
