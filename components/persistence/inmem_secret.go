package persistence

import (
	"cmp"
	"context"
	"slices"
	"sync"

	"github.com/Phong95/aec-agent-go/components/secrethub/secret"
)

// secretKey is the (owner, name) key of a stored secret.
type secretKey struct{ owner, name string }

// MemorySecretStore is the default in-memory SecretStore, the zero-config default for
// A-101. Sealed values live in process memory and are lost on restart; the servers'
// Postgres stores are the durable ones. It deep-copies records on the way in and out and
// is safe for concurrent use.
type MemorySecretStore struct {
	mu      sync.RWMutex
	secrets map[secretKey]secret.Stored
}

// NewMemorySecretStore returns an empty store.
func NewMemorySecretStore() *MemorySecretStore {
	return &MemorySecretStore{secrets: map[secretKey]secret.Stored{}}
}

var _ SecretStore = (*MemorySecretStore)(nil)

// Get returns a copy of the secret, or nil and a nil error when there is none.
func (s *MemorySecretStore) Get(_ context.Context, owner, name string) (*secret.Stored, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.secrets[secretKey{owner, name}]
	if !ok {
		return nil, nil //nolint:nilnil // nil, nil means no such secret
	}
	cp := v.Clone()
	return &cp, nil
}

// List returns copies of the owner's secrets, sorted by name.
func (s *MemorySecretStore) List(_ context.Context, owner string) ([]secret.Stored, error) {
	return s.list(func(v secret.Stored) bool { return v.OwnerAgentID == owner }), nil
}

// ListAll returns copies of every secret, globals first, then by owner and name.
func (s *MemorySecretStore) ListAll(context.Context) ([]secret.Stored, error) {
	return s.list(func(secret.Stored) bool { return true }), nil
}

func (s *MemorySecretStore) list(keep func(secret.Stored) bool) []secret.Stored {
	s.mu.RLock()
	out := []secret.Stored{}
	for _, v := range s.secrets {
		if keep(v) {
			out = append(out, v.Clone())
		}
	}
	s.mu.RUnlock()
	slices.SortFunc(out, func(a, b secret.Stored) int {
		return cmp.Or(cmp.Compare(a.OwnerAgentID, b.OwnerAgentID), cmp.Compare(a.Name, b.Name))
	})
	return out
}

// Save upserts a copy of the secret by (owner, name).
func (s *MemorySecretStore) Save(_ context.Context, v secret.Stored) error {
	cp := v.Clone()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.secrets == nil {
		s.secrets = map[secretKey]secret.Stored{}
	}
	s.secrets[secretKey{cp.OwnerAgentID, cp.Name}] = cp
	return nil
}

// MarkUsed sets LastUsedAt; a missing secret is ignored.
func (s *MemorySecretStore) MarkUsed(_ context.Context, owner, name, at string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := secretKey{owner, name}
	if v, ok := s.secrets[k]; ok {
		v.LastUsedAt = at
		s.secrets[k] = v
	}
	return nil
}

// Delete removes one secret.
func (s *MemorySecretStore) Delete(_ context.Context, owner, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.secrets, secretKey{owner, name})
	return nil
}

// DeleteByOwner removes every secret the agent owns; an empty id removes nothing.
func (s *MemorySecretStore) DeleteByOwner(_ context.Context, agentID string) error {
	if agentID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for k := range s.secrets {
		if k.owner == agentID {
			delete(s.secrets, k)
		}
	}
	return nil
}
