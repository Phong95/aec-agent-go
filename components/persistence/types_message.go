package persistence

import (
	"context"

	"github.com/Phong95/aec-agent-go/internal/jsonx"
	"github.com/Phong95/aec-agent-go/kernel"
)

// ---------------------------------------------------------------------------
// MessageStore - pluggable persistence for conversation messages
//
// The same interface is used for both full message history and the compacted
// LLM working set. Each can be wired to a different backend (Postgres, Redis,
// file-system, etc.) through the Persistence hub.
// ---------------------------------------------------------------------------

// MessageSlice is a window of a session's messages plus the session's total
// message count.
type MessageSlice struct {
	Messages []kernel.Message `json:"messages"`
	Total    int              `json:"total"`
}

// MarshalJSON writes a nil Messages as [] (the field is required). It takes a
// value receiver so a MessageSlice held by value encodes correctly, and it never
// writes the normalised slice back into the caller's value.
func (s MessageSlice) MarshalJSON() ([]byte, error) {
	type plain MessageSlice // drops methods, avoids recursion
	if s.Messages == nil {
		s.Messages = []kernel.Message{}
	}
	return jsonx.Marshal(plain(s))
}

// MessageStore persists the messages of conversation sessions.
type MessageStore interface {
	// Get retrieves all messages for a session. It returns an empty, non-nil
	// slice when the session has no messages.
	Get(ctx context.Context, sessionID string) ([]kernel.Message, error)

	// GetRange retrieves a slice of messages ending at before (exclusive),
	// returning up to limit messages in chronological order. It is used for
	// reverse-paginated loading (load from the end, scroll up to fetch more).
	//
	// before is the index to fetch before (exclusive); pass -1 for "from the
	// end". limit is the maximum number of messages to return.
	GetRange(ctx context.Context, sessionID string, before, limit int) (MessageSlice, error)

	// Set replaces the entire message list for a session.
	Set(ctx context.Context, sessionID string, msgs []kernel.Message) error

	// Append adds a single message to the end of the session's message list.
	Append(ctx context.Context, sessionID string, m kernel.Message) error

	// Clear removes all messages for a session.
	Clear(ctx context.Context, sessionID string) error

	// Count returns the number of messages without loading them.
	Count(ctx context.Context, sessionID string) (int, error)
}
