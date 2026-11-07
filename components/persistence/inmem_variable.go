package persistence

import (
	"cmp"
	"context"
	"slices"
	"sync"

	"github.com/Phong95/aec-agent-go/components/variablehub/variable"
)

// variableKey is the (owner, name) key of a stored variable.
type variableKey struct{ owner, name string }

// MemoryVariableStore is the default in-memory VariableStore, the zero-config default for
// A-215. Variables live in process memory and are lost on restart. It is safe for
// concurrent use (Info has no reference fields, so copies are plain values).
type MemoryVariableStore struct {
	mu        sync.RWMutex
	variables map[variableKey]variable.Info
}

// NewMemoryVariableStore returns an empty store.
func NewMemoryVariableStore() *MemoryVariableStore {
	return &MemoryVariableStore{variables: map[variableKey]variable.Info{}}
}

var _ VariableStore = (*MemoryVariableStore)(nil)

// Get returns a copy of the variable, or nil and a nil error when there is none.
func (s *MemoryVariableStore) Get(_ context.Context, owner, name string) (*variable.Info, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.variables[variableKey{owner, name}]
	if !ok {
		return nil, nil //nolint:nilnil // nil, nil means no such variable
	}
	return &v, nil
}

// List returns the owner's variables, sorted by name.
func (s *MemoryVariableStore) List(_ context.Context, owner string) ([]variable.Info, error) {
	return s.list(func(v variable.Info) bool { return v.OwnerAgentID == owner }), nil
}

// ListAll returns every variable, globals first, then by owner and name.
func (s *MemoryVariableStore) ListAll(context.Context) ([]variable.Info, error) {
	return s.list(func(variable.Info) bool { return true }), nil
}

func (s *MemoryVariableStore) list(keep func(variable.Info) bool) []variable.Info {
	s.mu.RLock()
	out := []variable.Info{}
	for _, v := range s.variables {
		if keep(v) {
			out = append(out, v)
		}
	}
	s.mu.RUnlock()
	slices.SortFunc(out, func(a, b variable.Info) int {
		return cmp.Or(cmp.Compare(a.OwnerAgentID, b.OwnerAgentID), cmp.Compare(a.Name, b.Name))
	})
	return out
}

// Save upserts the variable by (owner, name).
func (s *MemoryVariableStore) Save(_ context.Context, v variable.Info) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.variables == nil {
		s.variables = map[variableKey]variable.Info{}
	}
	s.variables[variableKey{v.OwnerAgentID, v.Name}] = v
	return nil
}

// Delete removes one variable.
func (s *MemoryVariableStore) Delete(_ context.Context, owner, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.variables, variableKey{owner, name})
	return nil
}

// DeleteByOwner removes every variable the agent owns; an empty id removes nothing.
func (s *MemoryVariableStore) DeleteByOwner(_ context.Context, agentID string) error {
	if agentID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for k := range s.variables {
		if k.owner == agentID {
			delete(s.variables, k)
		}
	}
	return nil
}
