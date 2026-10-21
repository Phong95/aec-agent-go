// Package grouphub is group chats (A-106): several agents in one conversation. A lead
// routes the work, everyone answers side by side, or only the members the user
// addresses answer. User doc and API: docs/group-chat.md of the repository.
//
//	Hub.Send -> turns -> ResolveRecipients -> plan
//	                |
//	                |- single / round -> member runner (host) -> agent.ChatStream(member session)
//	                |                    `- answerWriter -> thread -> group:* events
//	                `- lead -> spawn scope of the lead's session (agent.Deps.SubagentScopes)
//	                            `- spawn_subagent -> the subagent manager (A-105) -> scope Brief / ChildSession / Runner
//
// | File            | Owns                                                                        |
// |-----------------|-----------------------------------------------------------------------------|
// | types.go        | the hub's runtime contracts: member runner, send request, turn result      |
// | recipients.go   | ResolveRecipients (A-108): who answers. Pure.                               |
// | member_prompt.go| BuildMemberPrompt (A-111): what a member is told each turn                  |
// | thread.go       | the thread: ordered appends and updates per session, group:* events, cursors|
// | turns.go        | group turns (A-109, A-112, A-115): rounds, lead routing, guard rails, stop  |
// | answer.go       | answerWriter: one member answer streamed into the thread                    |
// | hub.go          | Hub: groups (validation, A-107), sessions, notes, agent removal             |
//
// Nothing group-specific lives on agent.Agent. The only hooks are generic:
// agent.Deps.SubagentScopes (read by the subagent manager), the ToolManager's session
// grants, and agentloop.Deps.PreApproved. The contracts shared with the stores and the
// events live in the type-only subpackage group. It mirrors core/ts
// src/components/group-hub.
package grouphub

import (
	"context"

	"github.com/Phong95/aec-agent-go/components/agent"
	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/components/grouphub/group"
	"github.com/Phong95/aec-agent-go/kernel"
)

// AgentHost is the agents members can be: AgentCore's orchestration satisfies it.
type AgentHost interface {
	// Get returns the live agent registered under id.
	Get(id string) (*agent.Agent, bool)
}

// ---------------------------------------------------------------------------
// Sessions
// ---------------------------------------------------------------------------

// The "source" metadata values of group sessions.
const (
	// SessionSource marks a group's thread session.
	SessionSource = "group"
	// MemberSessionSource marks a member's private session in a group session.
	MemberSessionSource = "group-member"
)

// IsGroupSession reports whether a session is a group's thread (metadata source
// "group"). A nil session is not.
func IsGroupSession(s *kernel.Session) bool { return sourceOf(s) == SessionSource }

// IsGroupMemberSession reports whether a session is a member's private session in a
// group session (metadata source "group-member"). A nil session is not.
func IsGroupMemberSession(s *kernel.Session) bool { return sourceOf(s) == MemberSessionSource }

func sourceOf(s *kernel.Session) string {
	if s == nil {
		return ""
	}
	v, _ := s.Metadata.Get("source")
	str, _ := v.(string)
	return str
}

// MemberSessionRef is where a member session belongs.
type MemberSessionRef struct {
	GroupID        string `json:"groupId"`
	GroupSessionID string `json:"groupSessionId"`
	AgentID        string `json:"agentId"`
}

// MemberSession is a current member's private session in a group session.
type MemberSession struct {
	AgentID   string `json:"agentId"`
	SessionID string `json:"sessionId"`
}

// SessionFields is a change to a group session (Hub.UpdateSession); a nil field stays.
type SessionFields struct {
	// Title renames the session; "" (after trimming) removes the title.
	Title *string `json:"title,omitempty"`
	// Pinned pins or unpins it.
	Pinned *bool `json:"pinned,omitempty"`
}

// UpdateOptions are the options of Hub.Update.
type UpdateOptions struct {
	// NoteIn is a session of the group (usually the one the user has open): the changes
	// are written there as notes ("RFI Desk now leads."). "" writes none.
	NoteIn string
}

// ---------------------------------------------------------------------------
// Recipients (A-108)
// ---------------------------------------------------------------------------

// MemberRef is a member as recipient resolution sees it.
type MemberRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// RecipientInput is what ResolveRecipients decides from.
type RecipientInput struct {
	Text string
	// To is the composer's To list (agent ids); non-members are ignored.
	To []string
	// Members are in group order.
	Members   []MemberRef
	LeadID    string
	ReplyMode group.ReplyMode
	Muted     []string
}

// Recipients is who was addressed and what the message sets off.
type Recipients struct {
	// Addressed is who the message was addressed to: the To list (as given), then
	// @mentions (in group order), without duplicates (never nil).
	Addressed []string   `json:"addressed"`
	Plan      group.Plan `json:"plan"`
}

// ---------------------------------------------------------------------------
// Running member turns
// ---------------------------------------------------------------------------

// MemberRole is why a member runs in a group turn. The string values are stable wire
// values.
type MemberRole string

// The member roles.
const (
	// RoleMember: it answers the user (alone or in a round).
	RoleMember MemberRole = "member"
	// RoleLead: it leads - it may hand parts to members, then sums up.
	RoleLead MemberRole = "lead"
	// RoleSummary: the lead sums up a round.
	RoleSummary MemberRole = "summary"
	// RoleHandoff: the lead handed it a task.
	RoleHandoff MemberRole = "handoff"
	// RoleRetry: it answers again (retry).
	RoleRetry MemberRole = "retry"
)

// MemberTurnResult is what a member turn produced.
type MemberTurnResult struct {
	Output string
	Usage  kernel.TokenUsage
	// Aborted: the turn was stopped through its ctx.
	Aborted bool
	// StopReason is kernel.StopMaxTokens or kernel.StopError when the turn ended so, ""
	// otherwise.
	StopReason kernel.StopReason
}

// MemberTurnContext is one member turn, handed to the MemberRunner. Its cancellation
// (stop, the member's deadline, the turn's limits) arrives through the runner's ctx.
type MemberTurnContext struct {
	GroupID        string
	GroupSessionID string
	// AgentID is the member.
	AgentID string
	// SessionID is the member's private session.
	SessionID string
	Role      MemberRole
	RoundID   string
	// Prompt is what the member is told, as its user message.
	Prompt string
	// TraceAttributes are attributes for the member's "agent-run" span
	// (agent.WithChatTraceAttributes).
	TraceAttributes map[string]any
	// Execute runs the member's streamed turn and feeds the thread. A host runner wraps
	// its bookkeeping around it (per-session lock, stop registry, traces, usage) and
	// passes the ctx it controls (derived from the runner's ctx).
	Execute func(ctx context.Context) (MemberTurnResult, error)
}

// MemberRunner runs one member turn. The host sets one (AgentCore.UseGroupRunner) so a
// member turn takes its bookkeeping like its chat routes. Nil = mc.Execute(ctx).
type MemberRunner func(ctx context.Context, mc MemberTurnContext) (MemberTurnResult, error)

// SendRequest is one message sent to a group session.
type SendRequest struct {
	Text string `json:"text"`
	// To is the composer's To list (agent ids).
	To []string `json:"to,omitempty"`
}

// TurnResult is how a group turn ended.
type TurnResult struct {
	TurnID string           `json:"turnId"`
	Status group.TurnStatus `json:"status"`
	Plan   group.Plan       `json:"plan"`
	// Messages are the messages this turn added, in the order they were added (with their
	// final state).
	Messages []group.Message `json:"messages"`
	// Usage is the tokens every member used in the turn.
	Usage kernel.TokenUsage `json:"usage"`
}

// RunningTurn is a group turn running now, for "busy" marks.
type RunningTurn struct {
	GroupID   string `json:"groupId"`
	SessionID string `json:"sessionId"`
	TurnID    string `json:"turnId"`
	// Presence is who is doing what.
	Presence []events.GroupPresencePayload `json:"presence"`
}
