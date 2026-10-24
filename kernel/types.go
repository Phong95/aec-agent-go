// Package kernel holds only the cross-cutting contracts that have no single
// owner (shared data types, errors, id/time helpers, Plugin). Feature-specific
// types live in the package that owns the feature, per the CLAUDE.md layout
// rules.
//
// Every type here encodes to wire-exact JSON when encoded with EncodeJSON.
// Struct field order is the wire order of the recorded fixtures. Absent
// optional values are omitted, never null; required collections encode as []
// or {}.
package kernel

import (
	"slices"
	"time"

	"github.com/Phong95/aec-agent-go/internal/jsonx"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// Event is a pub-sub event that decouples services (like MediatR in .Net).
//
// Field order is the wire order: the fields of the partial event come first,
// then id and timestamp, which the bus assigns when it emits. Payload is any:
// decode into json.RawMessage when the exact payload bytes matter.
type Event struct {
	Type      string `json:"type"`
	SessionID string `json:"sessionId"`
	Payload   any    `json:"payload"`
	ID        string `json:"id"`
	Timestamp int64  `json:"timestamp"` // epoch ms
}

// Time returns Timestamp (epoch ms) as a time.Time in the local zone; a zero
// Timestamp is the Unix epoch.
func (e Event) Time() time.Time { return time.UnixMilli(e.Timestamp) }

// Role is the author of a Message.
type Role string

// The roles a Message can carry.
const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleSystem    Role = "system"
	RoleTool      Role = "tool"
)

// Message is one entry of a conversation.
type Message struct {
	Role Role `json:"role"`
	// Content is always present on the wire, even when empty.
	Content string `json:"content"`
	// Timestamp is an ISO string (see NowISO); empty means absent.
	Timestamp string `json:"timestamp,omitempty"`
	// ToolCalls uses omitzero: nil is omitted, an empty non-nil slice encodes [].
	ToolCalls   []ToolCall   `json:"toolCalls,omitzero"`
	ToolResults []ToolResult `json:"toolResults,omitzero"`
	// Usage is the token usage of the model response that produced this assistant
	// message (InputTokens = the context sent, OutputTokens = what the model
	// generated). Nil when the provider reported none and on every other message.
	Usage *TokenUsage `json:"usage,omitempty"`
	// Summary (A-104) is set on the message a compaction wrote in place of the middle
	// of the conversation (its summary), so a context breakdown can tell it apart.
	Summary bool `json:"summary,omitempty"`
}

// Time parses Timestamp (an ISO string, see NowISO) as a time.Time in UTC. The
// bool is false when Timestamp is empty (absent) or is not an RFC 3339 time.
func (m Message) Time() (time.Time, bool) {
	if m.Timestamp == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, m.Timestamp)
	if err != nil {
		return time.Time{}, false
	}
	return t.UTC(), true
}

// Clone returns a deep copy: the ToolCalls and ToolResults slices, every
// ToolCall.Arguments byte slice and every ToolResult.IsError pointer are copied
// (nil stays nil and an empty slice stays empty), so a caller and a store never
// share a mutable part of a message. Add a line here when a pointer, slice or map
// field is added to Message, ToolCall or ToolResult; the clonecheck test fails
// until you do.
func (m Message) Clone() Message {
	if m.ToolCalls != nil {
		calls := slices.Clone(m.ToolCalls)
		for i := range calls {
			calls[i].Arguments = slices.Clone(calls[i].Arguments)
		}
		m.ToolCalls = calls
	}
	if m.ToolResults != nil {
		results := slices.Clone(m.ToolResults)
		for i := range results {
			if results[i].IsError != nil {
				v := *results[i].IsError
				results[i].IsError = &v
			}
			if results[i].Link != nil {
				l := *results[i].Link
				results[i].Link = &l
			}
			results[i].CompactArguments = slices.Clone(results[i].CompactArguments)
		}
		m.ToolResults = results
	}
	if m.Usage != nil {
		u := m.Usage.Clone()
		m.Usage = &u
	}
	return m
}

// ToolCall is a tool invocation requested by the model.
type ToolCall struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Arguments holds the exact JSON bytes the model sent; empty encodes {}.
	Arguments Arguments `json:"arguments"`
	// ArgumentsError is set when the model's arguments could not be used: the
	// text was not valid JSON, or was not one JSON object. A model can produce
	// such text (an unescaped quote or newline inside a long string), and that
	// must not fail the whole response, so the call carries empty Arguments and
	// this report instead. The harness answers such a call with a tool error and
	// never runs it, so the model can try again (Phase 9). llmhub.ParseToolArguments
	// builds both fields; empty means the arguments are usable and the field is
	// omitted on the wire.
	ArgumentsError string `json:"argumentsError,omitempty"`
}

// ToolResult is the outcome of a ToolCall.
type ToolResult struct {
	ToolCallID string `json:"toolCallId"`
	Content    string `json:"content"`
	// IsError is a pointer because an explicit false is meaningful.
	IsError *bool `json:"isError,omitempty"`
	// Link (A-132) is something this call produced that the user can open - e.g. the
	// artifact save_artifact / update_artifact wrote - so a UI can link to it and open
	// it. Kept with the conversation; providers never send it to the model (the model
	// reads Content).
	Link *ToolResultLink `json:"link,omitempty"`
	// CompactArguments (A-129) are the arguments of this result's call as the model
	// reads them from now on (e.g. a written file's content replaced by a reference to
	// it). Kept with the conversation; the agent loop swaps them in for the call's
	// arguments, and providers never send this field. Empty means none.
	CompactArguments Arguments `json:"compactArguments,omitempty"`
}

// ToolResultLink (A-132) is a link to something a tool call produced that the user
// can open. One shape for every kind: the URI scheme says what it points at, and the
// UI routes it to the view that opens it ("artifact://<id>" today; A-124 adds
// "preview://..."). The URI is a stable reference, never a final address - the host
// resolves it when the user opens it (a preview URL needs a short-lived token), so a
// saved conversation keeps working.
type ToolResultLink struct {
	// URI is e.g. "artifact://<artifact id>" - the same URI the model writes in its reply.
	URI string `json:"uri"`
	// Name is what the UI shows, e.g. the artifact's name "landing.html".
	Name string `json:"name"`
	// Type and Version are optional details for the UI: a MIME type, an artifact version.
	Type    string `json:"type,omitempty"`
	Version int    `json:"version,omitempty"`
}

// ChunkType discriminates the kinds of StreamChunk.
type ChunkType string

// The kinds of StreamChunk.
const (
	ChunkText          ChunkType = "text"
	ChunkToolCallStart ChunkType = "tool_call_start"
	ChunkToolCallDelta ChunkType = "tool_call_delta"
	ChunkToolCall      ChunkType = "tool_call"
	ChunkToolResult    ChunkType = "tool_result"
	ChunkDone          ChunkType = "done"
	ChunkError         ChunkType = "error"
	// ChunkUsage carries the usage of one model response (StreamChunk.Usage). The agent
	// loop yields one after every response the provider reported a usage for; the
	// usage is also saved on that response's assistant message (Message.Usage).
	ChunkUsage ChunkType = "usage"
)

// StreamChunk is one piece of a streamed model response. It is a flat struct:
// which fields are set depends on Type.
type StreamChunk struct {
	Type ChunkType `json:"type"`
	// Content is a pointer so an explicit empty string survives a round trip:
	// the Anthropic provider yields event.delta.text unguarded, so an empty
	// text_delta becomes a chunk with content "" (the other providers skip empty
	// deltas), and that is written as "content":"" while an absent field must
	// stay absent.
	Content  *string   `json:"content,omitempty"`
	ToolCall *ToolCall `json:"toolCall,omitempty"`
	// ToolCallProgress is present on "tool_call_start" / "tool_call_delta"
	// chunks: a tool call the model is still writing.
	ToolCallProgress *ToolCallProgress `json:"toolCallProgress,omitempty"`
	ToolResult       *ToolResult       `json:"toolResult,omitempty"`
	Error            string            `json:"error,omitempty"`
	Usage            *TokenUsage       `json:"usage,omitempty"`
	// Aborted is set on the final "done" chunk when the run was stopped by
	// cancelling it (A-65). It is only ever set to true.
	Aborted bool `json:"aborted,omitempty"`
	// StopReason is set on the final "done" chunk (A-88). A provider sets it from
	// the stop reason the model reported, and leaves it empty when none was
	// reported. The agent loop's own final done chunk carries it only when the turn
	// ended because the output token limit was hit (max_tokens) or the response
	// stopped with an error; a normal end leaves it empty.
	StopReason StopReason `json:"stopReason,omitempty"`
}

// ToolCallProgress is a tool call the model is still generating, streamed so
// consumers can show progress while long arguments (e.g. a file body) are
// being written.
//
// "tool_call_start" carries the id and name as soon as the provider knows
// them; each "tool_call_delta" carries the next slice of the raw JSON
// arguments text. Slices are not valid JSON on their own. The complete,
// parsed arguments always follow in a "tool_call" chunk with the same id -
// that chunk is the only one the agent loop executes.
type ToolCallProgress struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// ArgumentsDelta is the next slice of the raw JSON arguments text
	// ("tool_call_delta" only).
	ArgumentsDelta string `json:"argumentsDelta,omitempty"`
}

// StopReason says why a model response ended.
type StopReason string

// The reasons a model response can end.
const (
	StopEndTurn   StopReason = "end_turn"
	StopToolUse   StopReason = "tool_use"
	StopMaxTokens StopReason = "max_tokens"
	StopError     StopReason = "error"
)

// ModelResponse is a complete (non-streamed) model answer.
type ModelResponse struct {
	Message    Message    `json:"message"`
	Usage      TokenUsage `json:"usage"`
	StopReason StopReason `json:"stopReason"`
}

// TokenUsage counts the tokens of one model call or of a whole run.
//
// CacheReadTokens and CacheWriteTokens are pointers because the SDK writes
// explicit zeros (run totals always carry them) and an explicit zero must
// survive a round trip while an absent field must stay absent.
type TokenUsage struct {
	InputTokens      int  `json:"inputTokens"`
	OutputTokens     int  `json:"outputTokens"`
	CacheReadTokens  *int `json:"cacheReadTokens,omitempty"`
	CacheWriteTokens *int `json:"cacheWriteTokens,omitempty"`
}

// Clone returns a deep copy: the cache token pointers are copied.
func (u TokenUsage) Clone() TokenUsage {
	if u.CacheReadTokens != nil {
		v := *u.CacheReadTokens
		u.CacheReadTokens = &v
	}
	if u.CacheWriteTokens != nil {
		v := *u.CacheWriteTokens
		u.CacheWriteTokens = &v
	}
	return u
}

// CounterCalibration is the learned chars-per-token ratio of the Counter.
type CounterCalibration struct {
	CharsPerToken float64 `json:"charsPerToken"`
	Samples       int     `json:"samples"`
}

// Session is a conversation container persisted by the session store.
type Session struct {
	ID string `json:"id"`
	// Metadata is required on the wire: a zero Object encodes as {}. It keeps the
	// authored key order at every nesting level (ordered.Object).
	Metadata  ordered.Object `json:"metadata"`
	CreatedAt int64          `json:"createdAt"` // epoch ms
	UpdatedAt int64          `json:"updatedAt"` // epoch ms
	// LLMModel: every session has its own model; if the Agent switches back to
	// another provider it falls back to the default llmModel in AgentProfile.
	LLMModel string `json:"llmModel,omitempty"`
	// ContextWindowTokens is the context window (tokens) this session plans around,
	// overriding the agent's CompactorConfig.ContextWindowTokens - e.g. raised to the
	// model's ModelInfo.MaxContextWindowTokens. Zero = use the agent's setting.
	// Only moves the compaction threshold; the host checks it against the model's max.
	ContextWindowTokens int `json:"contextWindowTokens,omitempty"`
	// CurrentTokenUsage is the token usage from the most recent model response
	// in this session. InputTokens = full conversation sent to the model (=
	// current context size). OutputTokens = what the model generated (added to
	// context for the next turn). Together they represent the current context
	// window usage.
	CurrentTokenUsage *TokenUsage `json:"currentTokenUsage,omitempty"`
	// CounterCalibration is the learned chars-per-token ratio from the
	// Counter's calibration. Persisted so the next run starts with an accurate
	// estimator instead of the default 4:1 guess.
	CounterCalibration *CounterCalibration `json:"counterCalibration,omitempty"`
	// ConsolidatedMessageCount is the memory consolidation checkpoint: how many
	// of this session's messages (in MessageStore order) the memory system has
	// already processed. Persisted so consolidation resumes from here after a
	// restart instead of reprocessing the whole session. Messages are
	// append-only, so this value only ever grows: SessionStore.Update() must
	// never decrease it (other components write back cached Session copies
	// that may hold an older value). A pointer so an explicit 0 survives.
	ConsolidatedMessageCount *int `json:"consolidatedMessageCount,omitempty"`
	// JobID is the scheduled job (promptjob.Job.ID) whose run created this
	// session, so a client can group a job's runs together. Empty for sessions a user
	// or host started. Once set it sticks: SessionStore.Update() with it empty keeps
	// the stored value (cached Session copies written back by other components may
	// predate it).
	// Update (A-126): the job (job.Job.ID, of any kind) - was ScheduledJobID.
	JobID string `json:"jobId,omitempty"`
	// ApprovalMode (A-135) is how the session's tool calls that need approval are
	// handled. Empty = ApprovalModeAsk. Set with Agent.SetApprovalMode - never written
	// back from a cached copy, since the ConversationManager updates the running turn's
	// copy too.
	ApprovalMode ApprovalMode `json:"approvalMode,omitempty"`
}

// ApprovalMode (A-135): ApprovalModeAsk (the default) pauses every call that needs
// approval for the user; ApprovalModeYolo runs them without asking. Only the approval
// prompt is skipped - tool scope, ToolPolicy and secret allowlists still apply,
// questions (clarify) still go to the user, and job runs never use it.
// A-136: ApprovalModePlan is read-only planning - only read-only tool calls run (like a
// read-only subagent); the rest are refused, so the agent can look around and plan but
// change nothing. Unlike YOLO it is allowed in a job's session (it only takes
// permissions away).
type ApprovalMode string

// The approval modes. The string values are stable wire values.
const (
	ApprovalModeAsk  ApprovalMode = "ask"
	ApprovalModeYolo ApprovalMode = "yolo"
	// ApprovalModePlan (A-136): read-only planning.
	ApprovalModePlan ApprovalMode = "plan"
)

// Created returns CreatedAt (epoch ms) as a time.Time in the local zone; a zero
// CreatedAt is the Unix epoch.
func (s Session) Created() time.Time { return time.UnixMilli(s.CreatedAt) }

// Updated returns UpdatedAt (epoch ms) as a time.Time in the local zone; a zero
// UpdatedAt is the Unix epoch.
func (s Session) Updated() time.Time { return time.UnixMilli(s.UpdatedAt) }

// MarshalJSON encodes the session with a zero Metadata normalised to {} (the
// field is required, never null). It uses jsonx.Marshal on an alias type to
// avoid recursion and to keep the wire-exact encoding.
func (s Session) MarshalJSON() ([]byte, error) {
	type alias Session // drops methods, avoids recursion
	if s.Metadata.IsZero() {
		s.Metadata = ordered.NewObject()
	}
	return jsonx.Marshal(alias(s))
}

// Clone returns a deep copy: the Metadata object and the CurrentTokenUsage,
// CounterCalibration and ConsolidatedMessageCount pointers are copied, so a
// caller and a store never share a mutable part of a session. Add a line here
// when a pointer, slice or map field is added to Session; the clonecheck test
// fails until you do.
func (s Session) Clone() Session {
	s.Metadata = s.Metadata.Clone()
	if s.CurrentTokenUsage != nil {
		u := s.CurrentTokenUsage.Clone()
		s.CurrentTokenUsage = &u
	}
	if s.CounterCalibration != nil {
		c := *s.CounterCalibration
		s.CounterCalibration = &c
	}
	if s.ConsolidatedMessageCount != nil {
		n := *s.ConsolidatedMessageCount
		s.ConsolidatedMessageCount = &n
	}
	return s
}

// SessionFilter selects sessions by metadata and by the scheduled job that created
// them.
type SessionFilter struct {
	Metadata ordered.Object `json:"metadata,omitzero"`
	// JobID keeps only the sessions created by this scheduled job's runs
	// (empty = no constraint). Update (A-126): this job's runs.
	JobID string `json:"jobId,omitempty"`
}
