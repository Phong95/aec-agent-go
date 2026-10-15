package persistence

import (
	"context"

	"github.com/Phong95/aec-agent-go/components/agent/profile"
)

// MemoryAgentStore is the default in-memory AgentStore. It
// deep-copies profiles on the way in and out, lists them in insertion order and
// is safe for concurrent use.
type MemoryAgentStore struct {
	store keyedStore[profile.AgentProfile]
}

// NewMemoryAgentStore returns an empty store.
func NewMemoryAgentStore() *MemoryAgentStore {
	return &MemoryAgentStore{}
}

var _ AgentStore = (*MemoryAgentStore)(nil)

// Save upserts a copy of the profile. An existing id keeps its list position; a
// profile saved again after a Delete is appended.
func (s *MemoryAgentStore) Save(_ context.Context, p profile.AgentProfile) error {
	s.store.save(p.ID, p)
	return nil
}

// Load returns a copy of the profile, or nil and a nil error for an unknown id.
func (s *MemoryAgentStore) Load(_ context.Context, id string) (*profile.AgentProfile, error) {
	return s.store.load(id), nil // nil for an unknown profile
}

// List returns copies of every profile in insertion order.
func (s *MemoryAgentStore) List(_ context.Context) ([]profile.AgentProfile, error) {
	return s.store.list(), nil
}

// Delete removes the profile.
func (s *MemoryAgentStore) Delete(_ context.Context, id string) error {
	s.store.delete(id)
	return nil
}
