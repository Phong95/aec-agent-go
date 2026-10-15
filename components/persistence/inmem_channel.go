package persistence

import (
	"cmp"
	"context"
	"slices"
	"sync"

	"github.com/Phong95/aec-agent-go/components/channelplatformhub/channel"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// MemoryChannelStore is the default in-memory ChannelStore, the zero-config default for
// A-100. Channels live in process memory and are lost on restart; a host persists them
// with a durable store (the servers use Postgres). It deep-copies records on the way in
// and out and is safe for concurrent use.
type MemoryChannelStore struct {
	mu       sync.RWMutex
	channels ordered.Map[channel.Channel]
}

// NewMemoryChannelStore returns an empty store.
func NewMemoryChannelStore() *MemoryChannelStore { return &MemoryChannelStore{} }

var _ ChannelStore = (*MemoryChannelStore)(nil)

// Save upserts a copy of the channel.
func (s *MemoryChannelStore) Save(_ context.Context, c channel.Channel) error {
	cp := c.Clone()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.channels.Set(cp.ID, cp)
	return nil
}

// Get returns a copy of the channel, or nil and a nil error for an unknown id.
func (s *MemoryChannelStore) Get(_ context.Context, id string) (*channel.Channel, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c, ok := s.channels.Get(id)
	if !ok {
		return nil, nil //nolint:nilnil // nil, nil means an unknown channel
	}
	cp := c.Clone()
	return &cp, nil
}

// ListByAgent returns copies of the agent's channels, oldest created first (ties by id).
func (s *MemoryChannelStore) ListByAgent(_ context.Context, agentID string) ([]channel.Channel, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []channel.Channel{}
	for _, c := range s.channels.All() {
		if c.AgentID == agentID {
			out = append(out, c.Clone())
		}
	}
	slices.SortStableFunc(out, func(a, b channel.Channel) int {
		return cmp.Or(cmp.Compare(a.CreatedAt, b.CreatedAt), cmp.Compare(a.ID, b.ID))
	})
	return out, nil
}

// Delete removes the channel; an unknown id is a no-op.
func (s *MemoryChannelStore) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.channels.Delete(id)
	return nil
}

// DeleteByAgent removes every channel of the agent.
func (s *MemoryChannelStore) DeleteByAgent(_ context.Context, agentID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range s.channels.Keys() { // a snapshot: the map is modified below
		if c, ok := s.channels.Get(id); ok && c.AgentID == agentID {
			s.channels.Delete(id)
		}
	}
	return nil
}
