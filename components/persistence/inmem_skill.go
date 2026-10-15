package persistence

import (
	"context"
)

// MemorySkillStore is the default in-memory SkillStore. It copies records on
// save, load and list; it lists them in insertion order and is safe for
// concurrent use.
type MemorySkillStore struct {
	store keyedStore[SkillRecord]
}

// NewMemorySkillStore returns an empty store.
func NewMemorySkillStore() *MemorySkillStore {
	return &MemorySkillStore{}
}

var _ SkillStore = (*MemorySkillStore)(nil)

// Save inserts or replaces a copy of the record by name (an existing name keeps
// its list position).
func (s *MemorySkillStore) Save(_ context.Context, r SkillRecord) error {
	s.store.save(r.Name, r)
	return nil
}

// Load returns a copy of the record, or nil and a nil error for an unknown name.
func (s *MemorySkillStore) Load(_ context.Context, name string) (*SkillRecord, error) {
	return s.store.load(name), nil // nil for an unknown skill
}

// List returns copies of every record in insertion order.
func (s *MemorySkillStore) List(_ context.Context) ([]SkillRecord, error) {
	return s.store.list(), nil
}

// Delete removes the record.
func (s *MemorySkillStore) Delete(_ context.Context, name string) error {
	s.store.delete(name)
	return nil
}
