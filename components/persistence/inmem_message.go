package persistence

import (
	"context"
	"sync"

	"github.com/Phong95/aec-agent-go/kernel"
)

// MemoryMessageStore is the default in-memory MessageStore. It deep-copies messages on the way in and out and is safe
// for concurrent use. The map is only ever indexed by session id, never ranged,
// so key order cannot leak.
type MemoryMessageStore struct {
	mu   sync.RWMutex
	data map[string][]kernel.Message
}

// NewMemoryMessageStore returns an empty store.
func NewMemoryMessageStore() *MemoryMessageStore {
	return &MemoryMessageStore{data: make(map[string][]kernel.Message)}
}

var _ MessageStore = (*MemoryMessageStore)(nil)

// cloneMessages deep-copies msgs into a non-nil slice.
func cloneMessages(msgs []kernel.Message) []kernel.Message {
	out := make([]kernel.Message, len(msgs))
	for i, m := range msgs {
		out[i] = m.Clone()
	}
	return out
}

// Get returns copies of all messages of the session, an empty non-nil slice for
// an unknown session. The result is a copy because callers cache and push into
// it, which would otherwise double-append alongside Append.
func (s *MemoryMessageStore) Get(_ context.Context, sessionID string) ([]kernel.Message, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneMessages(s.data[sessionID]), nil
}

// GetRange returns copies of up to limit messages ending at before (exclusive)
// and the session's total. The arithmetic: end is the total when
// before is negative or above it, start is max(0, end-limit), and a start at or
// past end (a limit of 0 or less) gives an empty, non-nil slice.
func (s *MemoryMessageStore) GetRange(_ context.Context, sessionID string, before, limit int) (MessageSlice, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	all := s.data[sessionID]
	total := len(all)
	end := before
	if before < 0 || before > total {
		end = total
	}
	start := max(0, end-limit)
	if start >= end {
		return MessageSlice{Messages: []kernel.Message{}, Total: total}, nil
	}
	return MessageSlice{Messages: cloneMessages(all[start:end]), Total: total}, nil
}

// Set replaces the session's messages with copies of msgs.
func (s *MemoryMessageStore) Set(_ context.Context, sessionID string, msgs []kernel.Message) error {
	c := cloneMessages(msgs)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[sessionID] = c
	return nil
}

// Append adds a copy of m to the end of the session's messages.
func (s *MemoryMessageStore) Append(_ context.Context, sessionID string, m kernel.Message) error {
	c := m.Clone()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[sessionID] = append(s.data[sessionID], c)
	return nil
}

// Clear removes all messages of the session.
func (s *MemoryMessageStore) Clear(_ context.Context, sessionID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data, sessionID)
	return nil
}

// Count returns the number of messages of the session, 0 for an unknown one.
func (s *MemoryMessageStore) Count(_ context.Context, sessionID string) (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.data[sessionID]), nil
}
