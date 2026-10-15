package events

import (
	"encoding/json"

	"github.com/Phong95/aec-agent-go/kernel"
)

// Per-event payload shapes - each SDK event carries a typed payload.
//
// Field order is the wire order: agentId first, then the rest of the payload.
// Every struct encodes byte-identical to the recorded wire form (see the
// events-all-22 fixture; Update (A-126): 20 events since the two code-job events were
// removed with scheduled tasks).

// UserMessagePayload is the payload of the UserMessage kind.
type UserMessagePayload struct {
	AgentID string `json:"agentId"`
	Role    string `json:"role"`
	Content string `json:"content"`
}

// IterationPayload is the payload of the Iteration kind.
type IterationPayload struct {
	AgentID       string `json:"agentId"`
	Iteration     int    `json:"iteration"`
	MaxIterations int    `json:"maxIterations"`
}

// ModelRequestedPayload is the payload of the ModelRequested kind.
type ModelRequestedPayload struct {
	AgentID   string `json:"agentId"`
	Iteration int    `json:"iteration"`
	ToolCount int    `json:"toolCount"`
}

// ModelRespondedPayload is the payload of the ModelResponded kind. Usage is
// absent on the streaming path.
type ModelRespondedPayload struct {
	AgentID    string             `json:"agentId"`
	StopReason string             `json:"stopReason"`
	Usage      *kernel.TokenUsage `json:"usage,omitempty"`
	// HasToolCalls has no omit: it is always written.
	HasToolCalls bool `json:"hasToolCalls"`
}

// ContextUsagePayload (A-104) is the payload of the ContextUsage kind: the agent's id,
// then the wire shape of agentloop.ContextUsage, which this package cannot import.
type ContextUsagePayload struct {
	AgentID             string              `json:"agentId"`
	SessionID           string              `json:"sessionId"`
	Model               string              `json:"model,omitempty"`
	ContextWindowTokens int                 `json:"contextWindowTokens"`
	CompactAtTokens     int                 `json:"compactAtTokens"`
	UsedTokens          int                 `json:"usedTokens"`
	UsedSource          string              `json:"usedSource"`
	FreeTokens          int                 `json:"freeTokens"`
	Categories          []ContextCategory   `json:"categories"`
	Tools               []ContextToolTokens `json:"tools"`
	Iteration           int                 `json:"iteration,omitempty"`
}

// ContextCategory is one category of a ContextUsagePayload (agentloop.ContextCategory).
type ContextCategory struct {
	Key    string `json:"key"`
	Tokens int    `json:"tokens"`
}

// ContextToolTokens is one tool of a ContextUsagePayload (agentloop.ContextToolTokens).
type ContextToolTokens struct {
	Name   string `json:"name"`
	Tokens int    `json:"tokens"`
}

// DispatchStartPayload is the payload of ToolDispatched and SkillDispatched.
type DispatchStartPayload struct {
	AgentID   string           `json:"agentId"`
	Name      string           `json:"name"`
	Arguments kernel.Arguments `json:"arguments"`
}

// DispatchEndPayload is the payload of ToolCompleted and SkillCompleted.
type DispatchEndPayload struct {
	AgentID string `json:"agentId"`
	Name    string `json:"name"`
	IsError bool   `json:"isError"`
	Attempt int    `json:"attempt"`
}

// DispatchRetryPayload is the payload of ToolRetry and SkillRetry.
type DispatchRetryPayload struct {
	AgentID string `json:"agentId"`
	Name    string `json:"name"`
	Attempt int    `json:"attempt"`
	Reason  string `json:"reason"`
}

// DispatchFailedPayload is the payload of ToolFailed and SkillFailed.
type DispatchFailedPayload struct {
	AgentID string `json:"agentId"`
	Name    string `json:"name"`
	Error   string `json:"error"`
}

// ToolDeniedPayload is the payload of the ToolDenied kind.
type ToolDeniedPayload struct {
	AgentID string `json:"agentId"`
	Name    string `json:"name"`
	// Reason is a pointer so an explicit empty string survives a round trip
	// (an approval handler may return reason ""); absent stays absent.
	Reason *string `json:"reason,omitempty"`
}

// ToolApprovalRequestedPayload is the payload of the ToolApprovalRequested kind.
type ToolApprovalRequestedPayload struct {
	AgentID    string `json:"agentId"`
	Name       string `json:"name"`
	ToolCallID string `json:"toolCallId"`
	// Risk is a plain string here: the ToolRisk type is owned by the tool
	// package, which arrives in Phase 5, and the events package sits below it.
	Risk      string           `json:"risk"`
	Arguments kernel.Arguments `json:"arguments"`
	// Reason is why approval is needed when it is not simply a consequential tool (for
	// example a write on a host folder). It is set only when the gate has a non-empty
	// reason, so omitempty is exact.
	Reason string `json:"reason,omitempty"`
	// Sandbox says where a sandbox tool would act. It is set only when the gate
	// resolved a sandbox.
	Sandbox *ApprovalSandboxPayload `json:"sandbox,omitempty"`
}

// ApprovalSandboxPayload is the sandbox of a sandbox-aware approval request. Kind
// is a plain string for the same reason as Risk: events sits below the sandbox
// package in the layer table.
type ApprovalSandboxPayload struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	Isolated bool   `json:"isolated"`
}

// ToolApprovalResolvedPayload is the payload of the ToolApprovalResolved kind.
type ToolApprovalResolvedPayload struct {
	AgentID    string `json:"agentId"`
	Name       string `json:"name"`
	ToolCallID string `json:"toolCallId"`
	Approved   bool   `json:"approved"`
	// Reason is a pointer so an explicit empty string from an approval handler
	// survives a round trip; absent stays absent.
	Reason   *string `json:"reason,omitempty"`
	WaitedMs int64   `json:"waitedMs"`
}

// RunCompletedPayload is the payload of the RunCompleted kind.
type RunCompletedPayload struct {
	AgentID    string            `json:"agentId"`
	Iterations int               `json:"iterations"`
	TotalUsage kernel.TokenUsage `json:"totalUsage"`
	// Aborted is true when the run was stopped through RunOptions.signal instead of
	// ending naturally (A-65). It is only ever set to true, so omitempty is exact.
	Aborted bool `json:"aborted,omitempty"`
}

// MaxIterationsPayload is the payload of the MaxIterationsReached kind.
type MaxIterationsPayload struct {
	AgentID    string            `json:"agentId"`
	Iterations int               `json:"iterations"`
	TotalUsage kernel.TokenUsage `json:"totalUsage"`
}

// Prompt jobs (A-69) carry JobID, RunID and Trigger and are emitted with the run's
// real session id; code jobs (agent.ScheduledTaskHandler) leave those fields empty and use the
// session id "scheduled-job:<agent>:<name>".
// Update (A-126): every job emits the job:* kinds (below). Code jobs (scheduled tasks)
// and their payloads were removed.

// JobDelivery is the chat a prompt job's result is sent to (A-100 phase 3): the wire
// shape of promptjob.Delivery, which this package cannot import. (Update (A-126):
// job.Delivery.)
type JobDelivery struct {
	ChannelID string `json:"channelId"`
	// Target is a chat id, or "home" for the channel's home chat.
	Target   string `json:"target"`
	ThreadID string `json:"threadId,omitempty"`
	When     string `json:"when"`
}

// ArtifactWrittenPayload says an artifact version was written (A-62). It is
// emitted for both the first version and later ones.
type ArtifactWrittenPayload struct {
	AgentID    string `json:"agentId"`
	ArtifactID string `json:"artifactId"`
	Name       string `json:"name"`
	Type       string `json:"type"`
	Version    int    `json:"version"`
	Bytes      int64  `json:"bytes"`
	// SandboxPath is the sandbox path of the file this version tracks.
	SandboxPath string `json:"sandboxPath"`
	// SandboxID is the id of the sandbox the tracked file lives in.
	SandboxID string `json:"sandboxId"`
}

// ArtifactDeletedPayload is the payload of the ArtifactDeleted kind.
type ArtifactDeletedPayload struct {
	AgentID    string `json:"agentId"`
	ArtifactID string `json:"artifactId"`
	Name       string `json:"name"`
}

// AgentSpawnedPayload is the payload of the AgentSpawned kind. No session: it is
// emitted with SessionID "".
type AgentSpawnedPayload struct {
	AgentID string `json:"agentId"`
}

// AgentRemovedPayload is the payload of the AgentRemoved kind (the agent is stopped,
// its profile deleted and its data released). No session: emitted with SessionID "".
type AgentRemovedPayload struct {
	AgentID string `json:"agentId"`
}

// AgentProfileChangedPayload is the payload of the AgentProfileChanged kind: the
// agent's profile as it is after the change. No session: emitted with SessionID "".
//
// Profile is the encoded profile.AgentProfile (the same bytes core/ts sends), kept raw
// because events is a leaf package and must not import components/agent/profile (see
// tests/arch). A subscriber decodes it with json.Unmarshal into a profile.AgentProfile.
type AgentProfileChangedPayload struct {
	AgentID string          `json:"agentId"`
	Profile json.RawMessage `json:"profile"`
}

// SkillDeletedPayload is the payload of the SkillDeleted kind: a custom skill was
// deleted from the skill hub. No session: emitted with SessionID "".
type SkillDeletedPayload struct {
	Name string `json:"name"`
}

// Channels (A-100) are emitted by an agent's channel manager: the message kinds with the
// chat's session id, the others with SessionID "".

// ChannelStatusChangedPayload is the payload of the ChannelStatusChanged kind: a
// channel's live connection state changed.
type ChannelStatusChangedPayload struct {
	AgentID   string `json:"agentId"`
	ChannelID string `json:"channelId"`
	Platform  string `json:"platform"`
	// Status is "stopped", "connecting", "running", "retrying" or "failed".
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

// ChannelMessageReceivedPayload is the payload of the ChannelMessageReceived kind: a
// message from an allowed sender was accepted for a turn.
type ChannelMessageReceivedPayload struct {
	AgentID   string `json:"agentId"`
	ChannelID string `json:"channelId"`
	Platform  string `json:"platform"`
	ChatID    string `json:"chatId"`
	SenderID  string `json:"senderId"`
	MessageID string `json:"messageId"`
}

// ChannelMessageSentPayload is the payload of the ChannelMessageSent kind: the agent's
// reply was sent to the chat, in Parts messages when it was split.
type ChannelMessageSentPayload struct {
	AgentID   string `json:"agentId"`
	ChannelID string `json:"channelId"`
	Platform  string `json:"platform"`
	ChatID    string `json:"chatId"`
	Parts     int    `json:"parts"`
	// JobID is set when the message was a scheduled job's result (A-100 phase 3).
	// Update (A-126): any job's result.
	JobID string `json:"jobId,omitempty"`
}

// ChannelPairingRequestedPayload is the payload of the ChannelPairingRequested kind: an
// unknown sender asked to talk to the agent and got a pairing code.
type ChannelPairingRequestedPayload struct {
	AgentID    string `json:"agentId"`
	ChannelID  string `json:"channelId"`
	Platform   string `json:"platform"`
	Code       string `json:"code"`
	SenderID   string `json:"senderId"`
	SenderName string `json:"senderName,omitempty"`
	ExpiresAt  string `json:"expiresAt"`
}

// ChannelFailedPayload is the payload of the ChannelFailed kind: a channel turn or
// send failed.
type ChannelFailedPayload struct {
	AgentID   string `json:"agentId"`
	ChannelID string `json:"channelId"`
	Platform  string `json:"platform"`
	ChatID    string `json:"chatId,omitempty"`
	Error     string `json:"error"`
	// JobID is set when a scheduled job's result could not be delivered (A-100 phase 3).
	// Update (A-126): any job's result.
	JobID string `json:"jobId,omitempty"`
}

// Triggers (A-70) are emitted by an agent's trigger manager: the run kinds with the run's
// session id, TriggerEventReceived with SessionID "".
// Update (A-126): jobs of every kind are emitted by an agent's job manager.

// JobEventReceivedPayload is the payload of the JobEventReceived kind: an event reached a
// job's inbox (or was a duplicate of one that had). A clock tick is one too.
type JobEventReceivedPayload struct {
	AgentID string `json:"agentId"`
	JobID   string `json:"jobId"`
	// EventID is the inbox entry.
	EventID string `json:"eventId"`
	Type    string `json:"type"`
	// Status is "accepted", "filtered", "suppressed" or "duplicate".
	Status string `json:"status"`
	RunID  string `json:"runId,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// JobRunStartedPayload is the payload of the JobRunStarted kind: an attempt of a job run
// started its agent turn.
type JobRunStartedPayload struct {
	AgentID string `json:"agentId"`
	JobID   string `json:"jobId"`
	JobName string `json:"jobName"`
	RunID   string `json:"runId"`
	// Cause is what started the run: "schedule", "catch-up", "manual", "event",
	// "webhook", "test" or "replay".
	Cause   string `json:"cause"`
	Attempt int    `json:"attempt"`
}

// JobRunRetryingPayload is the payload of the JobRunRetrying kind: an attempt failed and
// the run will be tried again at RetryAt.
type JobRunRetryingPayload struct {
	AgentID string `json:"agentId"`
	JobID   string `json:"jobId"`
	JobName string `json:"jobName"`
	RunID   string `json:"runId"`
	Attempt int    `json:"attempt"`
	Error   string `json:"error"`
	RetryAt string `json:"retryAt"`
}

// JobRunFinishedPayload is the payload of the JobRunFinished kind: a job run ended.
type JobRunFinishedPayload struct {
	AgentID string `json:"agentId"`
	JobID   string `json:"jobId"`
	RunID   string `json:"runId"`
	Cause   string `json:"cause"`
	// Status is "succeeded", "failed", "blocked", "suppressed" or "stopped".
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
	Reason string `json:"reason,omitempty"`
	// JobName is the job's name, unless it was deleted meanwhile.
	JobName string `json:"jobName,omitempty"`
	// Output is the agent's reply, when the turn finished.
	Output string `json:"output,omitempty"`
	// Silent: the reply was [[SILENT]] - nothing to report, nothing is delivered.
	Silent bool `json:"silent,omitempty"`
	// Action (A-103) is "tool" for a tool job's run (its output is the tool's output, or
	// the follow-up reply).
	Action string `json:"action,omitempty"`
	// Paused (A-103): this run's failure paused the job (Limits.PauseAfterFailures failed
	// runs in a row).
	Paused bool `json:"paused,omitempty"`
	// Delivery is the chat the result is sent to (the job's delivery as it is now): sent
	// by the agent's channel manager.
	// Update (A-127): Deliveries - every chat it is sent to.
	Deliveries []JobDelivery `json:"deliveries,omitempty"`
}

// ---------------------------------------------------------------------------
// Subagent runs (A-105) - emitted by the parent agent's subagent manager, with the
// parent's session id.
// ---------------------------------------------------------------------------

// SubagentSpawnedPayload is the payload of the SubagentSpawned kind: a parent agent
// started a child run.
type SubagentSpawnedPayload struct {
	// AgentID is the parent agent.
	AgentID          string `json:"agentId"`
	RunID            string `json:"runId"`
	ParentRunID      string `json:"parentRunId"`
	ParentToolCallID string `json:"parentToolCallId,omitempty"`
	ChildAgentID     string `json:"childAgentId"`
	ChildSessionID   string `json:"childSessionId"`
	// Access is "read-only" or "write".
	Access string `json:"access"`
}

// SubagentTypeDeletedPayload is the payload of the SubagentTypeDeleted kind: a custom
// subagent type was deleted from the library (A-105 revision). No session: emitted with
// SessionID "".
type SubagentTypeDeletedPayload struct {
	Name string `json:"name"`
}

// SubagentFinishedPayload is the payload of the SubagentCompleted, SubagentFailed and
// SubagentCancelled kinds: a child run ended.
type SubagentFinishedPayload struct {
	// AgentID is the parent agent.
	AgentID          string `json:"agentId"`
	RunID            string `json:"runId"`
	ParentRunID      string `json:"parentRunId"`
	ParentToolCallID string `json:"parentToolCallId,omitempty"`
	ChildAgentID     string `json:"childAgentId"`
	ChildSessionID   string `json:"childSessionId,omitempty"`
	// Status is "completed", "partial", "failed", "cancelled", "timed_out" or
	// "budget_exhausted".
	Status     string            `json:"status"`
	Usage      kernel.TokenUsage `json:"usage"`
	ToolCalls  int               `json:"toolCalls"`
	DurationMs int64             `json:"durationMs"`
	Error      string            `json:"error,omitempty"`
}

// ---------------------------------------------------------------------------
// Secrets (A-101) - emitted by the SecretHub; names only, never values.
// ---------------------------------------------------------------------------

// SecretChangedPayload is the payload of the SecretCreated, SecretUpdated and
// SecretDeleted kinds: a secret was created, changed or deleted. No session: emitted with
// SessionID "".
type SecretChangedPayload struct {
	Name string `json:"name"`
	// OwnerAgentID is the owning agent; "" = a global secret.
	OwnerAgentID string `json:"ownerAgentId,omitempty"`
	// ValueChanged is set on secret:updated only: true when the value changed (not just the
	// description, hosts or tools).
	ValueChanged *bool `json:"valueChanged,omitempty"`
}

// SecretUsedPayload is the payload of the SecretUsed kind: a tool was handed a secret's
// value.
type SecretUsedPayload struct {
	Name         string `json:"name"`
	OwnerAgentID string `json:"ownerAgentId,omitempty"`
	// AgentID is the agent whose tool used it.
	AgentID string `json:"agentId,omitempty"`
	Tool    string `json:"tool"`
	Host    string `json:"host,omitempty"`
}
