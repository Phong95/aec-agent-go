package persistence

import (
	"context"

	"github.com/Phong95/aec-agent-go/components/artifacthub/artifact"
)

// ---------------------------------------------------------------------------
// Durable artifact persistence (A-62) - same pluggable-store pattern as
// SessionStore / MemoryStore. The head record (Artifact) and its content-less
// history log (Version) are stored separately; the content itself lives only in
// the agent's sandbox, never here.
//
// Artifacts are agent-owned: Artifact.AgentID is stamped at creation and every
// read path here is either keyed by id (the caller checks ownership) or filtered
// by agent. The ArtifactHub's per-agent view is what enforces that check, so
// stores stay simple.
// ---------------------------------------------------------------------------

// ArtifactFilter selects artifact head records. Every field is optional: an empty
// string, an empty list and a Limit of 0 all mean "no filter".
type ArtifactFilter struct {
	// AgentID restricts to one agent's artifacts. The hub's view always sets this.
	AgentID string `json:"agentId,omitempty"`
	// SessionID selects artifacts first created in this session.
	SessionID string   `json:"sessionId,omitempty"`
	Types     []string `json:"types,omitzero"`
	Name      string   `json:"name,omitempty"`
	// Tags matches an artifact carrying any of these tags.
	Tags []string `json:"tags,omitzero"`
	// Since and Before are ISO timestamps on UpdatedAt: Since is inclusive,
	// Before is exclusive.
	Since  string `json:"since,omitempty"`
	Before string `json:"before,omitempty"`
	// Limit keeps the first Limit matches (newest updated first); 0 means no limit.
	Limit int `json:"limit,omitempty"`
}

// ArtifactStore persists artifact head records and their version log.
type ArtifactStore interface {
	// Create makes the head record and its version 1. It replaces any previous
	// version log of that id.
	Create(ctx context.Context, a artifact.Artifact, v artifact.Version) error
	// Get returns the head record, or a nil pointer and a nil error when the id is
	// unknown.
	Get(ctx context.Context, id string) (*artifact.Artifact, error)
	// FindByName returns the first artifact of the agent with that name, or nil
	// and a nil error. Names are unique per agent - this is how the model refers
	// to an artifact by name.
	FindByName(ctx context.Context, agentID, name string) (*artifact.Artifact, error)
	// AddVersion appends a version and moves the head record to it, atomically
	// where the backend allows.
	AddVersion(ctx context.Context, a artifact.Artifact, v artifact.Version) error
	// ListVersions returns the versions, newest first.
	ListVersions(ctx context.Context, artifactID string) ([]artifact.Version, error)
	// Query returns the matching head records, newest updated first.
	Query(ctx context.Context, f ArtifactFilter) ([]artifact.Artifact, error)
	// Delete removes the artifact and every version of it.
	Delete(ctx context.Context, id string) error
	// DeleteByAgent removes every artifact of one agent (the agent was deleted).
	DeleteByAgent(ctx context.Context, agentID string) error
}
