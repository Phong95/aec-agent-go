package persistence

import (
	"context"

	"github.com/Phong95/aec-agent-go/components/agent/profile"
)

// ---------------------------------------------------------------------------
// Agent store - persistence for agent profiles
// ---------------------------------------------------------------------------

// AgentStore persists agent profiles.
type AgentStore interface {
	Save(ctx context.Context, p profile.AgentProfile) error
	// Load returns the profile, or a nil pointer and a nil error when the id is
	// unknown.
	Load(ctx context.Context, id string) (*profile.AgentProfile, error)
	List(ctx context.Context) ([]profile.AgentProfile, error)
	Delete(ctx context.Context, id string) error
}
