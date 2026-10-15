package persistence

import (
	"cmp"
	"context"
	"slices"
	"sync"

	"github.com/Phong95/aec-agent-go/components/agent/subagent"
	"github.com/Phong95/aec-agent-go/components/subagenthub/subagenttype"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// MemorySubagentRunStore is the default in-memory SubagentRunStore, the zero-config
// default for A-105. Runs live in process memory and are lost on restart; a host keeps
// them with a durable store (the servers use Postgres). It deep-copies records on the
// way in and out and is safe for concurrent use.
type MemorySubagentRunStore struct {
	mu   sync.RWMutex
	runs ordered.Map[subagent.Run]
}

// NewMemorySubagentRunStore returns an empty store.
func NewMemorySubagentRunStore() *MemorySubagentRunStore { return &MemorySubagentRunStore{} }

var _ SubagentRunStore = (*MemorySubagentRunStore)(nil)

// Save upserts a copy of the run.
func (s *MemorySubagentRunStore) Save(_ context.Context, run subagent.Run) error {
	cp := run.Clone()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runs.Set(cp.ID, cp)
	return nil
}

// Get returns a copy of the run, or nil and a nil error for an unknown id.
func (s *MemorySubagentRunStore) Get(_ context.Context, id string) (*subagent.Run, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.runs.Get(id)
	if !ok {
		return nil, nil //nolint:nilnil // nil, nil means an unknown run
	}
	cp := r.Clone()
	return &cp, nil
}

// ListByParentSession returns copies of the runs started from the session, oldest
// started first (ties by id).
func (s *MemorySubagentRunStore) ListByParentSession(_ context.Context, parentSessionID string) ([]subagent.Run, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []subagent.Run{}
	for _, r := range s.runs.All() {
		if r.ParentSessionID == parentSessionID {
			out = append(out, r.Clone())
		}
	}
	slices.SortStableFunc(out, func(a, b subagent.Run) int {
		return cmp.Or(cmp.Compare(a.StartedAt, b.StartedAt), cmp.Compare(a.ID, b.ID))
	})
	return out, nil
}

// DeleteByAgent removes every run the agent started (as the parent).
func (s *MemorySubagentRunStore) DeleteByAgent(_ context.Context, agentID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range s.runs.Keys() { // a snapshot: the map is modified below
		if r, ok := s.runs.Get(id); ok && r.ParentAgentID == agentID {
			s.runs.Delete(id)
		}
	}
	return nil
}

// MemorySubagentTypeStore is the default in-memory SubagentTypeStore, the zero-config
// default for A-105. Custom types live in process memory and are lost on restart; the
// servers keep them in Postgres. It deep-copies records on the way in and out and is
// safe for concurrent use.
type MemorySubagentTypeStore struct {
	mu    sync.RWMutex
	types ordered.Map[subagenttype.Type]
}

// NewMemorySubagentTypeStore returns an empty store.
func NewMemorySubagentTypeStore() *MemorySubagentTypeStore { return &MemorySubagentTypeStore{} }

var _ SubagentTypeStore = (*MemorySubagentTypeStore)(nil)

// List returns copies of every type, sorted by name.
func (s *MemorySubagentTypeStore) List(context.Context) ([]subagenttype.Type, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]subagenttype.Type, 0, s.types.Len())
	for _, t := range s.types.All() {
		out = append(out, t.Clone())
	}
	slices.SortFunc(out, func(a, b subagenttype.Type) int { return cmp.Compare(a.Name, b.Name) })
	return out, nil
}

// Save upserts a copy of the type.
func (s *MemorySubagentTypeStore) Save(_ context.Context, t subagenttype.Type) error {
	cp := t.Clone()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.types.Set(cp.Name, cp)
	return nil
}

// Delete removes the type.
func (s *MemorySubagentTypeStore) Delete(_ context.Context, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.types.Delete(name)
	return nil
}
