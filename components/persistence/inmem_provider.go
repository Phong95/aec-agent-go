package persistence

import (
	"context"
)

// MemoryProviderStore is the default in-memory ProviderStore. It deep-copies records on the way in and out, lists them
// in insertion order and is safe for concurrent use.
type MemoryProviderStore struct {
	store keyedStore[ProviderRecord]
}

// NewMemoryProviderStore returns an empty store.
func NewMemoryProviderStore() *MemoryProviderStore {
	return &MemoryProviderStore{}
}

var _ ProviderStore = (*MemoryProviderStore)(nil)

// Save upserts a copy of the record, keyed by name (an existing name keeps its
// list position).
func (s *MemoryProviderStore) Save(_ context.Context, p ProviderRecord) error {
	s.store.save(p.Name, p)
	return nil
}

// Load returns a copy of the record, or nil and a nil error for an unknown name.
func (s *MemoryProviderStore) Load(_ context.Context, name string) (*ProviderRecord, error) {
	return s.store.load(name), nil // nil for an unknown provider
}

// List returns copies of every record in insertion order.
func (s *MemoryProviderStore) List(_ context.Context) ([]ProviderRecord, error) {
	return s.store.list(), nil
}

// Delete removes the record.
func (s *MemoryProviderStore) Delete(_ context.Context, name string) error {
	s.store.delete(name)
	return nil
}
