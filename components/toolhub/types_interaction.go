package toolhub

import (
	"context"

	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/kernel"
)

// ---------------------------------------------------------------------------
// Interaction control - the host app's channel to the human user
// ---------------------------------------------------------------------------

// ClarifyRequest is a question the agent asks the user when a request is
// genuinely ambiguous.
type ClarifyRequest struct {
	SessionID string `json:"sessionId"`
	AgentID   string `json:"agentId,omitempty"`
	Question  string `json:"question"`
	// Choices are optional multiple-choice answers; the user may still answer
	// free-form.
	Choices []string `json:"choices,omitzero"`
}

// ClarifyResponse is the user's answer to a ClarifyRequest.
type ClarifyResponse struct {
	Answer string `json:"answer"`
}

// ApprovalSandbox says where a sandbox tool would act (sandbox-aware approval).
type ApprovalSandbox struct {
	ID       string          `json:"id"`
	Kind     sandboxhub.Kind `json:"kind"`
	Isolated bool            `json:"isolated"`
}

// ApprovalRequest is shown to the user before a "consequential" tool runs. It
// carries everything needed to approve safely: what will run, with which
// arguments, and why it is gated.
//
// Arguments are the raw bytes the policy checked, not a decoded map: the user
// approves exactly what will run, and a re-encode could reorder keys or change
// number spellings.
type ApprovalRequest struct {
	SessionID       string           `json:"sessionId"`
	AgentID         string           `json:"agentId,omitempty"`
	ToolCallID      string           `json:"toolCallId"`
	ToolName        string           `json:"toolName"`
	ToolDescription string           `json:"toolDescription"`
	Risk            Risk             `json:"risk"`
	Arguments       kernel.Arguments `json:"arguments"`
	// Reason is why approval is needed, when it is not simply Risk
	// "consequential" (for example a write on a host folder).
	Reason string `json:"reason,omitempty"`
	// Sandbox is where a sandbox tool would act (sandbox-aware approval).
	Sandbox *ApprovalSandbox `json:"sandbox,omitempty"`
	// Subagent (A-105) is set when a subagent asks - the child whose tool call this is.
	// The request is routed to the PARENT's session (SessionID / AgentID above are the
	// parent's), so it shows in the chat the user is watching; the decision applies to
	// this one call of the child.
	Subagent *ApprovalSubagent `json:"subagent,omitempty"`
}

// ApprovalSubagent is the child agent behind an ApprovalRequest a subagent made (A-105).
type ApprovalSubagent struct {
	AgentID   string `json:"agentId"`
	AgentName string `json:"agentName"`
	// SessionID is the child's own session.
	SessionID string `json:"sessionId"`
	// RunID is the child run.
	RunID string `json:"runId"`
}

// ApprovalDecision is the user's answer to an ApprovalRequest.
type ApprovalDecision struct {
	Approved bool `json:"approved"`
	// Reason is an optional note from the user, returned to the model on denial.
	// It is a pointer so an explicit empty string stays distinct from an absent
	// one.
	Reason *string `json:"reason,omitempty"`
}

// SecretRequest asks the user to type a secret's value into a masked field (A-101,
// manage_secret). The value goes from the user straight to the SecretHub - it is never
// shown to the model, stored in the conversation or traced.
type SecretRequest struct {
	SessionID  string `json:"sessionId"`
	AgentID    string `json:"agentId,omitempty"`
	ToolCallID string `json:"toolCallId,omitempty"`
	// Name is the secret's name ({{secret:NAME}}).
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// AllowedHosts and AllowedTools say where the value may be used (shown so the user
	// knows what they are handing over).
	AllowedHosts []string `json:"allowedHosts"`
	AllowedTools []string `json:"allowedTools"`
	// Replacing is true when the secret exists and gets a new value.
	Replacing bool `json:"replacing,omitempty"`
}

// SecretResponse is the user's answer to a SecretRequest.
type SecretResponse struct {
	// Value is the value the user entered; "" when they cancelled.
	Value string `json:"value,omitempty"`
}

// SecretRequester is the optional part of an InteractionHandler that collects a secret's
// value in a masked field (A-101, the optional requestSecret of core/ts). A handler without
// it (a chat app, an unattended run) can't take secret values, so manage_secret says so.
// Like the other calls it blocks until the user answers, must honour ctx and may be called
// concurrently.
type SecretRequester interface {
	RequestSecret(ctx context.Context, req SecretRequest) (SecretResponse, error)
}

// InteractionHandler is implemented by the host app (server, CLI) to reach the
// human user. Both calls block the agent loop until the user responds - the host
// owns timeouts and cancellation (return Approved false, or an error, when the
// user does not answer in time; ctx is cancelled when the run stops).
//
// The clarify tool is builtin/clarify (it calls AskUser); the approval gate of the
// agent loop calls RequestApproval. The handler implementation is the host app; this
// package defines only the contract, and aecmock.ScriptedInteraction is a
// test double of it.
//
// Both methods may be called concurrently: the loop runs up to
// agentloop.WithMaxParallelTools tool calls at once, so two clarify prompts, two
// approval requests or one of each can overlap, and an implementation must be safe
// for concurrent use. Both must honour ctx, which is cancelled when the run stops, so
// a prompt that waits for a user does not outlive the run. The SDK sets no timeout
// of its own on either call: the host owns it.
type InteractionHandler interface {
	AskUser(ctx context.Context, req ClarifyRequest) (ClarifyResponse, error)
	RequestApproval(ctx context.Context, req ApprovalRequest) (ApprovalDecision, error)
}
