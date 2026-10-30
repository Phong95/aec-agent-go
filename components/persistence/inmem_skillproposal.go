package persistence

import (
	"cmp"
	"context"
	"slices"
	"sync"

	"github.com/Phong95/aec-agent-go/components/agent/skilllearning/learning"
)

// MemorySkillProposalStore is the default in-memory SkillProposalStore, the zero-config
// default for A-166. Proposals and learned-skill records live in process memory and are
// lost on restart; the servers keep them in Postgres. It deep-copies records on the way
// in and out and is safe for concurrent use.
type MemorySkillProposalStore struct {
	mu        sync.RWMutex
	proposals map[string]learning.Proposal
	learned   map[[2]string]learning.LearnedSkill
}

// NewMemorySkillProposalStore returns an empty store.
func NewMemorySkillProposalStore() *MemorySkillProposalStore {
	return &MemorySkillProposalStore{
		proposals: map[string]learning.Proposal{},
		learned:   map[[2]string]learning.LearnedSkill{},
	}
}

var _ SkillProposalStore = (*MemorySkillProposalStore)(nil)

// Save upserts a copy of the proposal.
func (s *MemorySkillProposalStore) Save(_ context.Context, p learning.Proposal) error {
	cp := p.Clone()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.proposals[cp.ID] = cp
	return nil
}

// Get returns a copy of the proposal, or nil and a nil error for an unknown id.
func (s *MemorySkillProposalStore) Get(_ context.Context, id string) (*learning.Proposal, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.proposals[id]
	if !ok {
		return nil, nil //nolint:nilnil // nil, nil means an unknown proposal
	}
	cp := p.Clone()
	return &cp, nil
}

// List returns copies of the agent's proposals, newest first.
func (s *MemorySkillProposalStore) List(_ context.Context, agentID string, filter learning.ProposalFilter) ([]learning.Proposal, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []learning.Proposal{}
	for _, p := range s.proposals {
		if p.AgentID == agentID && (filter.Status == "" || p.Status == filter.Status) {
			out = append(out, p.Clone())
		}
	}
	slices.SortFunc(out, func(a, b learning.Proposal) int {
		return cmp.Or(cmp.Compare(b.CreatedAt, a.CreatedAt), cmp.Compare(b.ID, a.ID))
	})
	return out, nil
}

// Delete removes the proposal.
func (s *MemorySkillProposalStore) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.proposals, id)
	return nil
}

// SaveLearnedSkill upserts a copy of the record.
func (s *MemorySkillProposalStore) SaveLearnedSkill(_ context.Context, r learning.LearnedSkill) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.learned[[2]string{r.AgentID, r.SkillName}] = r
	return nil
}

// GetLearnedSkill returns a copy of the record, or nil and a nil error.
func (s *MemorySkillProposalStore) GetLearnedSkill(_ context.Context, agentID, skillName string) (*learning.LearnedSkill, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.learned[[2]string{agentID, skillName}]
	if !ok {
		return nil, nil //nolint:nilnil // nil, nil means no record
	}
	return &r, nil
}

// ListLearnedSkills returns copies of the agent's records, by skill name.
func (s *MemorySkillProposalStore) ListLearnedSkills(_ context.Context, agentID string) ([]learning.LearnedSkill, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []learning.LearnedSkill{}
	for _, r := range s.learned {
		if r.AgentID == agentID {
			out = append(out, r)
		}
	}
	slices.SortFunc(out, func(a, b learning.LearnedSkill) int { return cmp.Compare(a.SkillName, b.SkillName) })
	return out, nil
}

// DeleteLearnedSkill removes the record.
func (s *MemorySkillProposalStore) DeleteLearnedSkill(_ context.Context, agentID, skillName string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.learned, [2]string{agentID, skillName})
	return nil
}

// DeleteByAgent removes every proposal and record of the agent.
func (s *MemorySkillProposalStore) DeleteByAgent(_ context.Context, agentID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, p := range s.proposals {
		if p.AgentID == agentID {
			delete(s.proposals, id)
		}
	}
	for k := range s.learned {
		if k[0] == agentID {
			delete(s.learned, k)
		}
	}
	return nil
}
