package persistence

import (
	"context"

	"github.com/Phong95/aec-agent-go/kernel"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// ---------------------------------------------------------------------------
// Session store - pluggable persistence
// ---------------------------------------------------------------------------

// SessionStore persists conversation sessions.
type SessionStore interface {
	// Create makes a new session with a fresh id and the given metadata. A zero
	// metadata object becomes an explicit empty one.
	Create(ctx context.Context, metadata ordered.Object) (kernel.Session, error)
	// Get returns the session, or a nil pointer and a nil error when the id is
	// unknown.
	Get(ctx context.Context, id string) (*kernel.Session, error)
	// Update stores the session (an upsert). It takes a value and does NOT write
	// the merged consolidatedMessageCount checkpoint back into the caller's
	// session: call Get to observe the stored checkpoint. JobID is
	// sticky: an empty incoming value keeps the stored one.
	Update(ctx context.Context, s kernel.Session) error
	// SetConsolidatedMessageCount is a narrow write of
	// Session.ConsolidatedMessageCount only - it must not touch UpdatedAt or any
	// other field (background consolidation is not session activity). It never
	// decreases the stored value. It is a no-op for unknown sessions.
	SetConsolidatedMessageCount(ctx context.Context, id string, count int) error
	Delete(ctx context.Context, id string) error
	// List returns the sessions whose metadata strictly equals every entry of
	// the filter's metadata and, when the filter's JobID is set, whose
	// JobID equals it (a zero or empty filter keeps all).
	List(ctx context.Context, f kernel.SessionFilter) ([]kernel.Session, error)
}
