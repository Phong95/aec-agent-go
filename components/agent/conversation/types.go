// Package conversation holds the contracts of the conversations tool (A-220): the port a
// tool reads an agent's other conversations through, and who may see what. The agent's
// ConversationManager implements them; toolhub and the root only name them.
package conversation

import (
	"context"

	"github.com/Phong95/aec-agent-go/kernel"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// Reach is how far a session's conversations tool reaches.
type Reach string

// The reaches.
const (
	// ReachAgent is every conversation of the agent.
	ReachAgent Reach = "agent"
	// ReachChat is only that chat's own earlier conversations.
	ReachChat Reach = "chat"
)

// Scope lets a host with several users on one agent narrow what a session sees: extra
// session metadata a visible conversation must also carry, e.g. userId from the session's
// own metadata. The SDK does not know about users, so without a Scope every conversation
// of the agent is visible to its web sessions. A zero Object means no extra filter.
type Scope func(session kernel.Session) ordered.Object

// Visibility is what the ConversationManager needs to answer "what may this session see".
type Visibility struct {
	AgentID string
	// Reach is asked on every call (the channel manager's answer, from the running turn).
	Reach func(session kernel.Session) Reach
	// Scope is read on every call; nil, or a nil result, means no host scope.
	Scope func() Scope
}

// Summary is one conversation in a list.
type Summary struct {
	ID    string
	Title string
	// Source is where it happened: the chat app's platform, else "web".
	Source string
	// UpdatedAt is the last activity, epoch ms.
	UpdatedAt    int64
	MessageCount int
}

// Snippet is a matching piece of a message.
type Snippet struct {
	// Index is the message's position in its conversation, from 0.
	Index int
	Role  kernel.Role
	Text  string
}

// SearchHit is one conversation a search found.
type SearchHit struct {
	ID        string
	Title     string
	Source    string
	UpdatedAt int64
	Snippets  []Snippet
}

// Message is one message of a conversation being read.
type Message struct {
	Index     int
	Role      kernel.Role
	Text      string
	Timestamp string
}

// Page is a part of one conversation, oldest message first.
type Page struct {
	Conversation Summary
	Messages     []Message
	// Before is what to pass for the messages older than these; 0 when none remain.
	Before int
}

// Conversations is toolhub.Context.Conversations. Each call names the session asking, and
// the visible set is worked out again from that session: never from the model's arguments.
// A conversation outside it reads as not found.
type Conversations interface {
	// ListVisible is the conversations the session may see, newest first (limit 0 = the default).
	ListVisible(ctx context.Context, sessionID string, limit int) ([]Summary, error)
	// SearchVisible is the conversations with messages matching the query's words.
	SearchVisible(ctx context.Context, sessionID, query string, limit int) ([]SearchHit, error)
	// ReadVisible is one conversation's user and assistant text, by id or unique id prefix:
	// the newest limit messages before index before (0 = from the end, and the default limit).
	ReadVisible(ctx context.Context, sessionID, ref string, before, limit int) (Page, error)
}
