package persistence

import (
	"cmp"
	"context"
	"slices"
	"strconv"
	"sync"

	"github.com/Phong95/aec-agent-go/components/artifacthub/artifact"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// MemoryArtifactStore is the default in-memory ArtifactStore: the zero-config
// default for A-62. Metadata (the head
// record and the content-less version log) lives in process memory and is lost on
// restart; the tracked file's actual content is never held here, it lives in the
// sandbox. It deep-copies records on the way in and out and is safe for
// concurrent use; one lock guards both maps.
type MemoryArtifactStore struct {
	mu        sync.RWMutex
	artifacts ordered.Map[artifact.Artifact]
	// versions maps an artifact id to its version log, keyed by the decimal
	// version number.
	versions map[string]*ordered.Map[artifact.Version]
}

// NewMemoryArtifactStore returns an empty store.
func NewMemoryArtifactStore() *MemoryArtifactStore {
	return &MemoryArtifactStore{versions: make(map[string]*ordered.Map[artifact.Version])}
}

var _ ArtifactStore = (*MemoryArtifactStore)(nil)

func versionKey(n int) string { return strconv.Itoa(n) }

// Create stores the head record and REPLACES the version log of that id with one
// holding only the given version.
func (s *MemoryArtifactStore) Create(_ context.Context, a artifact.Artifact, v artifact.Version) error {
	ac, vc := a.Clone(), v.Clone()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.artifacts.Set(ac.ID, ac)
	log := ordered.New[artifact.Version]()
	log.Set(versionKey(vc.Version), vc)
	s.versions[ac.ID] = log
	return nil
}

// Get returns a copy of the head record, or nil and a nil error for an unknown id.
func (s *MemoryArtifactStore) Get(_ context.Context, id string) (*artifact.Artifact, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.artifacts.Get(id)
	if !ok {
		return nil, nil //nolint:nilnil // nil, nil means an unknown artifact
	}
	c := v.Clone()
	return &c, nil
}

// FindByName returns a copy of the first head record, in insertion order, of the
// agent with that name, or nil and a nil error.
func (s *MemoryArtifactStore) FindByName(_ context.Context, agentID, name string) (*artifact.Artifact, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, a := range s.artifacts.All() {
		if a.AgentID == agentID && a.Name == name {
			c := a.Clone()
			return &c, nil
		}
	}
	return nil, nil //nolint:nilnil // nil, nil means the agent has no artifact of that name
}

// AddVersion replaces the head record with a copy of a and adds the version to its
// log; the same version number overwrites the earlier entry.
func (s *MemoryArtifactStore) AddVersion(_ context.Context, a artifact.Artifact, v artifact.Version) error {
	ac, vc := a.Clone(), v.Clone()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.artifacts.Set(ac.ID, ac)
	log := s.versions[ac.ID]
	if log == nil {
		log = ordered.New[artifact.Version]()
		s.versions[ac.ID] = log
	}
	log.Set(versionKey(vc.Version), vc)
	return nil
}

// ListVersions returns copies of the versions of the artifact, newest first (an
// empty non-nil slice for an unknown id). The sort is stable.
func (s *MemoryArtifactStore) ListVersions(_ context.Context, artifactID string) ([]artifact.Version, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []artifact.Version{}
	if log := s.versions[artifactID]; log != nil {
		for _, v := range log.All() {
			out = append(out, v.Clone())
		}
	}
	slices.SortStableFunc(out, func(a, b artifact.Version) int { return cmp.Compare(b.Version, a.Version) })
	return out, nil
}

// Query returns copies of the matching head records, newest updated first. The
// filters apply in this order (agentId, sessionId, types, name, tags any-of,
// updatedAt >= since, updatedAt < before), and a zero-value field is no filter;
// the sort is by updatedAt descending and STABLE (ties keep insertion order;
// sort.Slice would not); the limit is applied by jsSliceLimit.
func (s *MemoryArtifactStore) Query(_ context.Context, f ArtifactFilter) ([]artifact.Artifact, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []artifact.Artifact{}
	for _, a := range s.artifacts.All() {
		if matchesArtifactFilter(a, f) {
			out = append(out, a)
		}
	}
	slices.SortStableFunc(out, func(a, b artifact.Artifact) int { return cmp.Compare(b.UpdatedAt, a.UpdatedAt) })
	out = jsSliceLimit(out, f.Limit)
	for i := range out {
		out[i] = out[i].Clone()
	}
	return out, nil
}

// matchesArtifactFilter applies the filters in the order Query documents.
func matchesArtifactFilter(a artifact.Artifact, f ArtifactFilter) bool {
	if f.AgentID != "" && a.AgentID != f.AgentID {
		return false
	}
	if f.SessionID != "" && a.SessionID != f.SessionID {
		return false
	}
	if len(f.Types) > 0 && !slices.Contains(f.Types, a.Type) {
		return false
	}
	if f.Name != "" && a.Name != f.Name {
		return false
	}
	if len(f.Tags) > 0 && !slices.ContainsFunc(a.Tags, func(t string) bool { return slices.Contains(f.Tags, t) }) {
		return false
	}
	if f.Since != "" && a.UpdatedAt < f.Since {
		return false
	}
	if f.Before != "" && a.UpdatedAt >= f.Before {
		return false
	}
	return true
}

// Delete removes the head record and its version log.
func (s *MemoryArtifactStore) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.artifacts.Delete(id)
	delete(s.versions, id)
	return nil
}

// DeleteByAgent removes every head record of the agent and their version logs.
func (s *MemoryArtifactStore) DeleteByAgent(_ context.Context, agentID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range s.artifacts.Keys() { // a snapshot: the map is modified below
		if a, ok := s.artifacts.Get(id); ok && a.AgentID == agentID {
			s.artifacts.Delete(id)
			delete(s.versions, id)
		}
	}
	return nil
}
