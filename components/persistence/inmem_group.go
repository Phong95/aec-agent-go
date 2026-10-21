package persistence

import (
	"cmp"
	"context"
	"slices"
	"sync"

	"github.com/Phong95/aec-agent-go/components/grouphub/group"
)

// MemoryGroupStore is the default in-memory GroupStore, the zero-config default for
// A-106. Groups live in process memory and are lost on restart; the servers' Postgres
// stores are the durable ones. It deep-copies records on the way in and out and is safe
// for concurrent use.
type MemoryGroupStore struct {
	mu     sync.RWMutex
	groups map[string]group.Group
}

// NewMemoryGroupStore returns an empty store.
func NewMemoryGroupStore() *MemoryGroupStore {
	return &MemoryGroupStore{groups: map[string]group.Group{}}
}

var _ GroupStore = (*MemoryGroupStore)(nil)

// Save upserts a copy of the group.
func (s *MemoryGroupStore) Save(_ context.Context, g group.Group) error {
	cp := g.Clone()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.groups[cp.ID] = cp
	return nil
}

// Get returns a copy of the group, or nil and a nil error for an unknown id.
func (s *MemoryGroupStore) Get(_ context.Context, id string) (*group.Group, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	g, ok := s.groups[id]
	if !ok {
		return nil, nil //nolint:nilnil // nil, nil means an unknown group
	}
	cp := g.Clone()
	return &cp, nil
}

// List returns copies of every group, oldest created first (ties by id).
func (s *MemoryGroupStore) List(context.Context) ([]group.Group, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]group.Group, 0, len(s.groups))
	for _, g := range s.groups {
		out = append(out, g.Clone())
	}
	slices.SortFunc(out, func(a, b group.Group) int {
		return cmp.Or(cmp.Compare(a.CreatedAt, b.CreatedAt), cmp.Compare(a.ID, b.ID))
	})
	return out, nil
}

// Delete removes the group.
func (s *MemoryGroupStore) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.groups, id)
	return nil
}

// MemoryGroupMessageStore is the default in-memory GroupMessageStore (A-106). Threads
// live in process memory and are lost on restart. It deep-copies messages on the way in
// and out and is safe for concurrent use.
type MemoryGroupMessageStore struct {
	mu        sync.RWMutex
	bySession map[string][]group.Message
	// sessionOf is the session of each stored message id.
	sessionOf map[string]string
}

// NewMemoryGroupMessageStore returns an empty store.
func NewMemoryGroupMessageStore() *MemoryGroupMessageStore {
	return &MemoryGroupMessageStore{bySession: map[string][]group.Message{}, sessionOf: map[string]string{}}
}

var _ GroupMessageStore = (*MemoryGroupMessageStore)(nil)

// Append adds a copy of m to its session's thread with the next Seq.
func (s *MemoryGroupMessageStore) Append(_ context.Context, m group.Message) (group.Message, error) {
	stored := m.Clone()
	s.mu.Lock()
	defer s.mu.Unlock()
	thread := s.bySession[stored.SessionID]
	stored.Seq = 1
	if n := len(thread); n > 0 {
		stored.Seq = thread[n-1].Seq + 1
	}
	s.bySession[stored.SessionID] = append(thread, stored)
	s.sessionOf[stored.ID] = stored.SessionID
	return stored.Clone(), nil
}

// Update replaces the stored message of m's id, keeping its Seq and SessionID.
func (s *MemoryGroupMessageStore) Update(_ context.Context, m group.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sessionID, ok := s.sessionOf[m.ID]
	if !ok {
		return nil
	}
	thread := s.bySession[sessionID]
	for i := range thread {
		if thread[i].ID == m.ID {
			next := m.Clone()
			next.Seq = thread[i].Seq
			next.SessionID = sessionID
			thread[i] = next
			break
		}
	}
	return nil
}

// Get returns a copy of the message, or nil and a nil error for an unknown id.
func (s *MemoryGroupMessageStore) Get(_ context.Context, id string) (*group.Message, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sessionID, ok := s.sessionOf[id]
	if !ok {
		return nil, nil //nolint:nilnil // nil, nil means an unknown message
	}
	for _, m := range s.bySession[sessionID] {
		if m.ID == id {
			cp := m.Clone()
			return &cp, nil
		}
	}
	return nil, nil //nolint:nilnil // nil, nil means an unknown message
}

// List returns copies of the session's messages after afterSeq, in Seq order.
func (s *MemoryGroupMessageStore) List(_ context.Context, sessionID string, afterSeq int) ([]group.Message, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []group.Message{}
	for _, m := range s.bySession[sessionID] {
		if m.Seq > afterSeq {
			out = append(out, m.Clone())
		}
	}
	return out, nil
}

// DeleteBySession removes the session's whole thread.
func (s *MemoryGroupMessageStore) DeleteBySession(_ context.Context, sessionID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.bySession[sessionID] {
		delete(s.sessionOf, m.ID)
	}
	delete(s.bySession, sessionID)
	return nil
}
