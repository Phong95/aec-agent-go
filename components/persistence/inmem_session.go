package persistence

import (
	"context"
	"sync"

	"github.com/Phong95/aec-agent-go/kernel"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// SessionBacking is the shared storage of one or more MemorySessionStores: a
// lock plus the sessions in insertion order. Create it with NewSessionBacking
// and pass it to WithSessionBacking so several stores see each other's writes
// (the durability suite relies on this).
type SessionBacking struct {
	mu   sync.RWMutex
	data ordered.Map[kernel.Session]
}

// NewSessionBacking returns an empty backing.
func NewSessionBacking() *SessionBacking {
	return &SessionBacking{}
}

// MemorySessionStore is the default in-memory SessionStore. It deep-copies sessions on the way in and out and is safe
// for concurrent use.
type MemorySessionStore struct {
	b     *SessionBacking
	now   func() int64
	newID func() string
}

// SessionOption configures a MemorySessionStore.
type SessionOption func(*MemorySessionStore)

// WithSessionBacking makes the store use a shared backing (nil is ignored).
func WithSessionBacking(b *SessionBacking) SessionOption {
	return func(s *MemorySessionStore) {
		if b != nil {
			s.b = b
		}
	}
}

// WithSessionClock sets the timestamp source in epoch ms (default
// kernel.NowMillis; nil is ignored).
func WithSessionClock(now func() int64) SessionOption {
	return func(s *MemorySessionStore) {
		if now != nil {
			s.now = now
		}
	}
}

// WithSessionIDs sets the session id source (default kernel.NewID; nil is
// ignored).
func WithSessionIDs(f func() string) SessionOption {
	return func(s *MemorySessionStore) {
		if f != nil {
			s.newID = f
		}
	}
}

// NewMemorySessionStore returns an empty store.
func NewMemorySessionStore(opts ...SessionOption) *MemorySessionStore {
	s := &MemorySessionStore{
		b:     NewSessionBacking(),
		now:   kernel.NowMillis,
		newID: kernel.NewID,
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

var _ SessionStore = (*MemorySessionStore)(nil)

// Create stores a new session. The clock is read twice, createdAt first and
// updatedAt second.
func (s *MemorySessionStore) Create(_ context.Context, metadata ordered.Object) (kernel.Session, error) {
	md := metadata.Clone()
	if md.IsZero() {
		md = ordered.NewObject()
	}
	sess := kernel.Session{ID: s.newID(), Metadata: md}
	sess.CreatedAt = s.now()
	sess.UpdatedAt = s.now()
	s.b.mu.Lock()
	defer s.b.mu.Unlock()
	s.b.data.Set(sess.ID, sess.Clone())
	return sess, nil
}

// Get returns a copy of the session, or nil and a nil error for an unknown id.
func (s *MemorySessionStore) Get(_ context.Context, id string) (*kernel.Session, error) {
	s.b.mu.RLock()
	defer s.b.mu.RUnlock()
	v, ok := s.b.data.Get(id)
	if !ok {
		return nil, nil //nolint:nilnil // nil, nil means an unknown session
	}
	c := v.Clone()
	return &c, nil
}

// Update upserts a copy of the session (an existing id keeps its position). The
// consolidated message count is monotonic: the stored copy gets the larger of the
// stored and the incoming count, so a stale copy never lowers it. JobID
// is sticky: an empty incoming value keeps the stored one. UpdatedAt is
// not touched, and the caller's value is never modified.
func (s *MemorySessionStore) Update(_ context.Context, sess kernel.Session) error {
	c := sess.Clone()
	s.b.mu.Lock()
	defer s.b.mu.Unlock()
	var checkpoint int
	existing, found := s.b.data.Get(c.ID)
	if found && existing.ConsolidatedMessageCount != nil {
		checkpoint = *existing.ConsolidatedMessageCount
	}
	if found && c.JobID == "" {
		c.JobID = existing.JobID
	}
	if c.ConsolidatedMessageCount != nil {
		checkpoint = max(checkpoint, *c.ConsolidatedMessageCount)
	}
	if checkpoint > 0 {
		c.ConsolidatedMessageCount = &checkpoint
	}
	s.b.data.Set(c.ID, c)
	return nil
}

// SetConsolidatedMessageCount raises the stored checkpoint to count when count is
// larger. An explicit 0 is stored (the field is always set). It is a no-op
// for an unknown id.
func (s *MemorySessionStore) SetConsolidatedMessageCount(_ context.Context, id string, count int) error {
	s.b.mu.Lock()
	defer s.b.mu.Unlock()
	v, ok := s.b.data.Get(id)
	if !ok {
		return nil
	}
	var cur int
	if v.ConsolidatedMessageCount != nil {
		cur = *v.ConsolidatedMessageCount
	}
	next := max(cur, count)
	v.ConsolidatedMessageCount = &next
	s.b.data.Set(id, v)
	return nil
}

// Delete removes the session. A session created afterwards is listed after the
// existing ones.
func (s *MemorySessionStore) Delete(_ context.Context, id string) error {
	s.b.mu.Lock()
	defer s.b.mu.Unlock()
	s.b.data.Delete(id)
	return nil
}

// List returns copies of the sessions in insertion order. A session is kept when
// every filter entry strictly equals the session's metadata value for
// that key, and its JobID equals the filter's when that is set.
func (s *MemorySessionStore) List(_ context.Context, f kernel.SessionFilter) ([]kernel.Session, error) {
	s.b.mu.RLock()
	defer s.b.mu.RUnlock()
	out := []kernel.Session{}
	for _, sess := range s.b.data.All() {
		if matchesMetadata(sess, f.Metadata) && (f.JobID == "" || sess.JobID == f.JobID) {
			out = append(out, sess.Clone())
		}
	}
	return out, nil
}

// matchesMetadata reports whether every filter entry strictly equals the session's
// metadata value for that key.
// A key the session does not have never matches.
func matchesMetadata(sess kernel.Session, filter ordered.Object) bool {
	for k, want := range filter.All() {
		got, ok := sess.Metadata.Get(k)
		if !ok || !ordered.StrictEqual(got, want) {
			return false
		}
	}
	return true
}
