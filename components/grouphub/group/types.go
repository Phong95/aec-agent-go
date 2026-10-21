// Package group holds the contracts of group chats (A-106) that the group hub shares
// with its stores and the event payloads: a group, its limits, the messages of a group
// session's thread and the plan of a group turn.
//
// A group is one conversation with several agents. It has members, a lead and a reply
// mode: the lead routes the work (hands parts to members through spawn_subagent, A-105,
// then sums up), everyone answers side by side (then the lead sums up), or only the
// members the user addresses answer. The composer's "To" list and @mentions override the
// reply mode for one message.
//
// The group thread is its own session (metadata source "group") with its own messages
// (Message: user / agent / handoff / event), kept in PersistenceHub.GroupMessages. Each
// member works in a private member session per group session (metadata source
// "group-member"), so its own tools, memory, compaction and context usage stay its own;
// each of its turns is told what was said in the thread since its last turn.
//
// The JSON shapes are the wire shapes of core/ts (Group, GroupMessage, GroupPlan), so the
// Go and TypeScript servers share the same Postgres rows and HTTP bodies.
//
// This package is a type-only leaf (it imports only kernel), so persistence, events and
// grouphub can all import it without a cycle.
package group

import (
	"slices"

	"github.com/Phong95/aec-agent-go/kernel"
)

// ReplyMode is who answers a message nobody was addressed in. The string values are
// stable wire values.
type ReplyMode string

// The reply modes.
const (
	// ReplyLead: the lead reads the message, may hand parts to members, then sums up.
	ReplyLead ReplyMode = "lead"
	// ReplyAll: every active member answers side by side; then the lead sums up.
	ReplyAll ReplyMode = "all"
	// ReplyMention: nobody answers until the user addresses someone.
	ReplyMention ReplyMode = "mention"
)

// MemberView is what a member is told about the thread (A-111). The string values are
// stable wire values.
type MemberView string

// The member views.
const (
	// ViewFull: every message since its last turn.
	ViewFull MemberView = "full"
	// ViewAddressed: only the user's messages for it (or for everyone), hand-offs to it,
	// summaries and notes - not the other members' answers.
	ViewAddressed MemberView = "addressed"
)

// Limits is the guard rails of one group turn (A-112). A field of 0 (omitted) uses the
// default (DefaultLimits); values above the caps (CapLimits) are clamped.
type Limits struct {
	// MaxMembersPerRound is the members answering in one round, hand-offs included
	// (default 4, at most 8).
	MaxMembersPerRound int `json:"maxMembersPerRound,omitempty"`
	// MaxTokensPerTurn is the tokens (input + output) every member of one group turn may
	// use together (default 400 000, at most 4 000 000).
	MaxTokensPerTurn int `json:"maxTokensPerTurn,omitempty"`
	// MaxElapsedMsPerTurn is the wall-clock time of one group turn, in ms (default 15 min,
	// at most 60 min).
	MaxElapsedMsPerTurn int `json:"maxElapsedMsPerTurn,omitempty"`
	// MemberTimeoutMs is one member's deadline in a turn, in ms (default 5 min, at most
	// 30 min).
	MemberTimeoutMs int `json:"memberTimeoutMs,omitempty"`
}

// DefaultLimits returns the defaults of the limits a group leaves out (the
// GROUP_LIMIT_DEFAULTS of core/ts).
func DefaultLimits() Limits {
	return Limits{
		MaxMembersPerRound:  4,
		MaxTokensPerTurn:    400_000,
		MaxElapsedMsPerTurn: 15 * 60_000,
		MemberTimeoutMs:     5 * 60_000,
	}
}

// CapLimits returns the most each limit may be (the GROUP_LIMIT_CAPS of core/ts).
func CapLimits() Limits {
	return Limits{
		MaxMembersPerRound:  8,
		MaxTokensPerTurn:    4_000_000,
		MaxElapsedMsPerTurn: 60 * 60_000,
		MemberTimeoutMs:     30 * 60_000,
	}
}

// ResolveLimits returns l with the defaults and caps applied: a field below 1 falls back
// to the default, one above its cap is clamped.
func ResolveLimits(l Limits) Limits {
	def, caps := DefaultLimits(), CapLimits()
	field := func(v, d, c int) int {
		if v < 1 {
			return d
		}
		return min(v, c)
	}
	return Limits{
		MaxMembersPerRound:  field(l.MaxMembersPerRound, def.MaxMembersPerRound, caps.MaxMembersPerRound),
		MaxTokensPerTurn:    field(l.MaxTokensPerTurn, def.MaxTokensPerTurn, caps.MaxTokensPerTurn),
		MaxElapsedMsPerTurn: field(l.MaxElapsedMsPerTurn, def.MaxElapsedMsPerTurn, caps.MaxElapsedMsPerTurn),
		MemberTimeoutMs:     field(l.MemberTimeoutMs, def.MemberTimeoutMs, caps.MemberTimeoutMs),
	}
}

// The sizes a group and its messages are checked against (the GROUP_TEXT_LIMITS of
// core/ts), in UTF-16 code units like a JS string length.
const (
	// MaxNameChars is the most characters of a group's name.
	MaxNameChars = 120
	// MaxTopicChars is the most characters of a group's topic.
	MaxTopicChars = 2_000
	// MaxMembers is the most members of one group.
	MaxMembers = 12
	// MaxMessageChars is the most characters of a user message.
	MaxMessageChars = 32_000
	// MaxPromptMessageChars is the most characters of one thread message as a member is
	// told it (longer ones are cut).
	MaxPromptMessageChars = 8_000
	// MaxPromptMessages is the most thread messages a member is told in one turn (older
	// ones are left out, with a note).
	MaxPromptMessages = 60
)

// Definition is what a group is created from.
type Definition struct {
	Name string `json:"name"`
	// Topic is what the group is for - shared context every member is told.
	Topic string `json:"topic,omitempty"`
	// Members are agent ids, at least 2.
	Members []string `json:"members"`
	// LeadID is a member; it can't be muted.
	LeadID string `json:"leadId"`
	// ReplyMode defaults to ReplyLead.
	ReplyMode ReplyMode `json:"replyMode,omitempty"`
	// Muted are members that are in the group but don't answer unless addressed.
	Muted []string `json:"muted,omitempty"`
	// MemberView defaults to ViewFull.
	MemberView MemberView `json:"memberView,omitempty"`
	Limits     Limits     `json:"limits,omitzero"`
}

// Patch is a change to a group; a nil field stays as it is.
type Patch struct {
	Name       *string     `json:"name,omitempty"`
	Topic      *string     `json:"topic,omitempty"`
	Members    []string    `json:"members,omitempty"`
	LeadID     *string     `json:"leadId,omitempty"`
	ReplyMode  *ReplyMode  `json:"replyMode,omitempty"`
	Muted      []string    `json:"muted,omitempty"`
	MemberView *MemberView `json:"memberView,omitempty"`
	Limits     *Limits     `json:"limits,omitempty"`
}

// Group is a stored group.
type Group struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Topic      string     `json:"topic"`
	Members    []string   `json:"members"`
	LeadID     string     `json:"leadId"`
	ReplyMode  ReplyMode  `json:"replyMode"`
	Muted      []string   `json:"muted"`
	MemberView MemberView `json:"memberView"`
	// Limits are as set (may be empty); ResolveLimits applies the defaults.
	Limits    Limits `json:"limits"`
	CreatedAt string `json:"createdAt"`
	UpdatedAt string `json:"updatedAt"`
}

// Clone returns a deep copy (the member lists are copied; a nil list stays nil).
func (g Group) Clone() Group {
	g.Members = slices.Clone(g.Members)
	g.Muted = slices.Clone(g.Muted)
	return g
}

// ---------------------------------------------------------------------------
// Thread messages
// ---------------------------------------------------------------------------

// MessageKind is what a thread message is. The string values are stable wire values.
type MessageKind string

// The message kinds.
const (
	// KindUser: the user's message; To is who it was addressed to (the To list and
	// @mentions).
	KindUser MessageKind = "user"
	// KindAgent: a member's answer (AgentID); RoundID when it answered side by side with
	// others; Summary for the lead's summary.
	KindAgent MessageKind = "agent"
	// KindHandoff: the lead handing part of the work to a member (AgentID -> ToAgentID,
	// Text = the ask).
	KindHandoff MessageKind = "handoff"
	// KindEvent: a note - membership and settings changes, nobody answering, limits hit.
	KindEvent MessageKind = "event"
)

// MessageStatus is how a member's answer (or a hand-off) is doing. The finished values
// follow subagent.Status. The string values are stable wire values.
type MessageStatus string

// The message statuses.
const (
	StatusRunning         MessageStatus = "running"
	StatusCompleted       MessageStatus = "completed"
	StatusPartial         MessageStatus = "partial"
	StatusFailed          MessageStatus = "failed"
	StatusCancelled       MessageStatus = "cancelled"
	StatusTimedOut        MessageStatus = "timed_out"
	StatusBudgetExhausted MessageStatus = "budget_exhausted"
)

// ToolStepStatus is how one tool call of an answer is doing.
type ToolStepStatus string

// The tool step statuses.
const (
	ToolRunning ToolStepStatus = "running"
	ToolDone    ToolStepStatus = "done"
	ToolError   ToolStepStatus = "error"
)

// ToolStep is one tool call of a member's answer, as the thread shows it.
type ToolStep struct {
	ID        string           `json:"id"`
	Name      string           `json:"name"`
	Status    ToolStepStatus   `json:"status"`
	Arguments kernel.Arguments `json:"arguments,omitempty"`
	// ResultPreview is the start of the result (at most 300 characters).
	ResultPreview string `json:"resultPreview,omitempty"`
	DurationMs    *int64 `json:"durationMs,omitempty"`
}

// Message is one message of a group session's thread.
type Message struct {
	ID string `json:"id"`
	// SessionID is the group session.
	SessionID string `json:"sessionId"`
	GroupID   string `json:"groupId"`
	// Seq is the position in the thread, from 1 - assigned by the store on append.
	Seq  int         `json:"seq"`
	Kind MessageKind `json:"kind"`
	Text string      `json:"text"`
	// To (user) is who it was addressed to (agent ids); empty = nobody in particular. A
	// user message always carries it (nil only on the other kinds).
	To []string `json:"to,omitzero"`
	// AgentID is who answered (agent), or who handed off (handoff).
	AgentID string `json:"agentId,omitempty"`
	// ToAgentID (handoff) is who it was handed to.
	ToAgentID string `json:"toAgentId,omitempty"`
	// RoundID is shared by answers given side by side.
	RoundID string `json:"roundId,omitempty"`
	// Summary marks the lead's summary of a round.
	Summary bool `json:"summary,omitempty"`
	// Status is set on agent and handoff messages only.
	Status MessageStatus `json:"status,omitempty"`
	// SubagentRunID is a hand-off's child run (and the answer it produced).
	SubagentRunID string             `json:"subagentRunId,omitempty"`
	Tools         []ToolStep         `json:"tools,omitzero"`
	Usage         *kernel.TokenUsage `json:"usage,omitempty"`
	// Error is a safe one-line reason for an answer that did not complete.
	Error     string `json:"error,omitempty"`
	CreatedAt string `json:"createdAt"`
	UpdatedAt string `json:"updatedAt"`
}

// Clone returns a deep copy: To, Tools (with their arguments and durations) and Usage
// are copied. Add a line here when a pointer, slice or map field is added.
func (m Message) Clone() Message {
	m.To = slices.Clone(m.To)
	if m.Tools != nil {
		tools := make([]ToolStep, len(m.Tools))
		for i, t := range m.Tools {
			t.Arguments = slices.Clone(t.Arguments)
			if t.DurationMs != nil {
				v := *t.DurationMs
				t.DurationMs = &v
			}
			tools[i] = t
		}
		m.Tools = tools
	}
	if m.Usage != nil {
		u := m.Usage.Clone()
		m.Usage = &u
	}
	return m
}

// ---------------------------------------------------------------------------
// The turn plan and its end
// ---------------------------------------------------------------------------

// TurnStatus is how a group turn ended. The string values are stable wire values.
type TurnStatus string

// The turn statuses.
const (
	TurnCompleted TurnStatus = "completed"
	// TurnStopped: stopped by the user (Stop, or the send ctx).
	TurnStopped TurnStatus = "stopped"
	// TurnLimited: a guard rail ended it (token budget or elapsed time).
	TurnLimited TurnStatus = "limited"
	// TurnNoReply: nobody answered (only when mentioned, nobody addressed).
	TurnNoReply TurnStatus = "no_reply"
)

// PresenceState is what a member of a running group turn is doing. The string values are
// stable wire values.
type PresenceState string

// The presence states.
const (
	PresenceThinking PresenceState = "thinking"
	// PresenceTool: it runs a tool (the presence's Tool names it).
	PresenceTool    PresenceState = "tool"
	PresenceWriting PresenceState = "writing"
	PresenceIdle    PresenceState = "idle"
)

// Change is what happened to a group (group:changed). The string values are stable
// wire values.
type Change string

// The changes.
const (
	ChangeCreated Change = "created"
	ChangeUpdated Change = "updated"
	ChangeDeleted Change = "deleted"
)
