package persistence

import (
	"context"
	"slices"
	"sync"

	"github.com/Phong95/aec-agent-go/components/agent/memorysystem/memory"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// MemoryMemoryStore is the default in-memory MemoryStore.
// It deep-copies records on the way in and out, keeps insertion order and is
// safe for concurrent use.
type MemoryMemoryStore struct {
	mu   sync.RWMutex
	data ordered.Map[memory.Record]
}

// NewMemoryMemoryStore returns an empty store.
func NewMemoryMemoryStore() *MemoryMemoryStore {
	return &MemoryMemoryStore{}
}

var _ MemoryStore = (*MemoryMemoryStore)(nil)

// Save upserts a copy of the record (an existing id keeps its position).
func (s *MemoryMemoryStore) Save(_ context.Context, r memory.Record) error {
	c := r.Clone()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.Set(c.ID, c)
	return nil
}

// Get returns a copy of the record, or nil and a nil error for an unknown id.
func (s *MemoryMemoryStore) Get(_ context.Context, id string) (*memory.Record, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.data.Get(id)
	if !ok {
		return nil, nil //nolint:nilnil // nil, nil means an unknown record
	}
	c := v.Clone()
	return &c, nil
}

// Update upserts a copy of the record, exactly like Save.
func (s *MemoryMemoryStore) Update(ctx context.Context, r memory.Record) error {
	return s.Save(ctx, r)
}

// Delete removes the record.
func (s *MemoryMemoryStore) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.Delete(id)
	return nil
}

// Query returns copies of the matching records in insertion order, with no sort.
// The filters apply in a fixed order and a zero-value field is no filter (an
// empty string or list is no filter); ISO timestamps compare as strings; the
// limit is applied by jsSliceLimit.
func (s *MemoryMemoryStore) Query(_ context.Context, f MemoryFilter) ([]memory.Record, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []memory.Record{}
	for _, r := range s.data.All() {
		if !matchesMemoryFilter(r, f) {
			continue
		}
		out = append(out, r)
	}
	out = jsSliceLimit(out, f.Limit)
	for i := range out {
		out[i] = out[i].Clone()
	}
	return out, nil
}

// matchesMemoryFilter applies the filters in the order Query documents.
func matchesMemoryFilter(r memory.Record, f MemoryFilter) bool {
	if f.AgentID != "" && r.AgentID != f.AgentID {
		return false
	}
	if len(f.Kinds) > 0 && !slices.Contains(f.Kinds, r.Kind) {
		return false
	}
	if len(f.Scopes) > 0 && !slices.Contains(f.Scopes, r.Scope) {
		return false
	}
	if len(f.Statuses) > 0 && !slices.Contains(f.Statuses, r.Status) {
		return false
	}
	if f.Subject != "" && r.Subject != f.Subject {
		return false
	}
	if f.Key != "" && r.Key != f.Key {
		return false
	}
	if len(f.Tags) > 0 && !slices.ContainsFunc(r.Tags, func(t string) bool { return slices.Contains(f.Tags, t) }) {
		return false
	}
	if f.Since != "" && r.ObservedAt < f.Since {
		return false
	}
	if f.Before != "" && r.ObservedAt >= f.Before {
		return false
	}
	return true
}

// FindByKey returns a copy of the first record, in insertion order, with the
// same agent, scope and key whose status is active. Superseded, disputed and
// expired records and other agents' records are ignored.
func (s *MemoryMemoryStore) FindByKey(_ context.Context, agentID string, scope memory.Scope, key string) (*memory.Record, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, r := range s.data.All() {
		if r.AgentID == agentID && r.Scope == scope && r.Key == key && r.Status == memory.StatusActive {
			c := r.Clone()
			return &c, nil
		}
	}
	return nil, nil //nolint:nilnil // nil, nil means no active record matches
}

// jsSliceLimit is list.slice(0, limit) where 0 means "no limit": a limit of 0
// keeps everything, a positive limit keeps the first n, and a negative limit
// drops that many from the end. It never allocates by the limit.
func jsSliceLimit[T any](list []T, limit int) []T {
	switch {
	case limit == 0:
		return list
	case limit > 0:
		return list[:min(limit, len(list))]
	default:
		return list[:max(len(list)+limit, 0)]
	}
}
