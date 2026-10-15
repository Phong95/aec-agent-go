package persistence

import (
	"sync"

	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// cloner is a record type with a deep-copy method.
type cloner[T any] interface{ Clone() T }

// keyedStore is the shared core of the in-memory stores that hold one record per
// string key and copy it on the way in and out (MemoryAgentStore,
// MemoryProviderStore, MemorySkillStore). It keeps the insertion order (an existing
// key keeps its position; a key saved again after a delete is appended) and is safe
// for concurrent use. Its zero value is an empty store.
type keyedStore[T cloner[T]] struct {
	mu   sync.RWMutex
	data ordered.Map[T]
}

// save upserts a copy of v under key.
func (s *keyedStore[T]) save(key string, v T) {
	c := v.Clone()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.Set(key, c)
}

// load returns a copy of the record under key, or nil when there is none.
func (s *keyedStore[T]) load(key string) *T {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.data.Get(key)
	if !ok {
		return nil
	}
	c := v.Clone()
	return &c
}

// list returns copies of every record in insertion order, as a fresh non-nil slice.
func (s *keyedStore[T]) list() []T {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]T, 0, s.data.Len())
	for _, v := range s.data.All() {
		out = append(out, v.Clone())
	}
	return out
}

// delete removes the record under key; an unknown key is a no-op.
func (s *keyedStore[T]) delete(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.Delete(key)
}
