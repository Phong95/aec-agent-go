package persistence

import (
	"cmp"
	"context"
	"slices"
	"sync"

	"github.com/Phong95/aec-agent-go/components/mcphub/mcpserver"
)

// MemoryMcpServerStore is the default in-memory McpServerStore, the zero-config default
// for A-208. Custom servers live in process memory and are lost on restart. It deep-copies
// records on the way in and out and is safe for concurrent use.
type MemoryMcpServerStore struct {
	mu      sync.RWMutex
	servers map[string]mcpserver.Server
}

// NewMemoryMcpServerStore returns an empty store.
func NewMemoryMcpServerStore() *MemoryMcpServerStore {
	return &MemoryMcpServerStore{servers: map[string]mcpserver.Server{}}
}

var _ McpServerStore = (*MemoryMcpServerStore)(nil)

// List returns copies of every server, sorted by id.
func (s *MemoryMcpServerStore) List(context.Context) ([]mcpserver.Server, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]mcpserver.Server, 0, len(s.servers))
	for _, v := range s.servers {
		out = append(out, v.Clone())
	}
	slices.SortFunc(out, func(a, b mcpserver.Server) int { return cmp.Compare(a.ID, b.ID) })
	return out, nil
}

// Save upserts a copy of the server.
func (s *MemoryMcpServerStore) Save(_ context.Context, v mcpserver.Server) error {
	cp := v.Clone()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.servers == nil {
		s.servers = map[string]mcpserver.Server{}
	}
	s.servers[cp.ID] = cp
	return nil
}

// Delete removes the server.
func (s *MemoryMcpServerStore) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.servers, id)
	return nil
}

// MemoryMcpAuthStore is the default in-memory McpAuthStore, the zero-config default for
// A-214. Sign-ins live in process memory and are lost on restart. It is safe for
// concurrent use.
type MemoryMcpAuthStore struct {
	mu      sync.RWMutex
	records map[string]McpAuthRecord
}

// NewMemoryMcpAuthStore returns an empty store.
func NewMemoryMcpAuthStore() *MemoryMcpAuthStore {
	return &MemoryMcpAuthStore{records: map[string]McpAuthRecord{}}
}

var _ McpAuthStore = (*MemoryMcpAuthStore)(nil)

// Get returns the server's record.
func (s *MemoryMcpAuthStore) Get(_ context.Context, serverID string) (McpAuthRecord, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.records[serverID]
	return r, ok, nil
}

// Save upserts the record.
func (s *MemoryMcpAuthStore) Save(_ context.Context, r McpAuthRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.records == nil {
		s.records = map[string]McpAuthRecord{}
	}
	s.records[r.ServerID] = r
	return nil
}

// Delete removes the record.
func (s *MemoryMcpAuthStore) Delete(_ context.Context, serverID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.records, serverID)
	return nil
}
