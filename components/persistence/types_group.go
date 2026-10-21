package persistence

import (
	"context"

	"github.com/Phong95/aec-agent-go/components/grouphub/group"
)

// ---------------------------------------------------------------------------
// Group chat persistence (A-106) - the groups themselves, and the messages of each
// group session's thread. Group sessions and the members' private sessions live in
// the SessionStore (metadata source "group" / "group-member"). Write through the
// grouphub.Hub, which validates groups and emits group:* events.
// ---------------------------------------------------------------------------

// GroupStore persists groups.
type GroupStore interface {
	// Save inserts or replaces the group by id.
	Save(ctx context.Context, g group.Group) error
	// Get returns the group, or a nil pointer and a nil error when the id is unknown.
	Get(ctx context.Context, id string) (*group.Group, error)
	// List returns every group, oldest created first (ties by id).
	List(ctx context.Context) ([]group.Group, error)
	// Delete removes the group; an unknown id is not an error.
	Delete(ctx context.Context, id string) error
}

// GroupMessageStore persists the thread of each group session. Messages are appended
// (the store assigns Seq, from 1 per session) and updated in place while a member's
// answer streams in.
type GroupMessageStore interface {
	// Append adds m to its session's thread (m.Seq is ignored) and returns it with its
	// Seq.
	Append(ctx context.Context, m group.Message) (group.Message, error)
	// Update replaces a stored message by id (its Seq and SessionID are kept as stored).
	// An unknown id is a no-op.
	Update(ctx context.Context, m group.Message) error
	// Get returns the message, or a nil pointer and a nil error when the id is unknown.
	Get(ctx context.Context, id string) (*group.Message, error)
	// List returns the session's messages in Seq order; afterSeq > 0 keeps only later
	// ones.
	List(ctx context.Context, sessionID string, afterSeq int) ([]group.Message, error)
	// DeleteBySession deletes a session's whole thread.
	DeleteBySession(ctx context.Context, sessionID string) error
}
