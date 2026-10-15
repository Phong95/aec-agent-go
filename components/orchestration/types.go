package orchestration

import (
	"context"

	"github.com/Phong95/aec-agent-go/components/agent"
	"github.com/Phong95/aec-agent-go/components/agent/profile"
)

// Factory builds the agent of a profile. Spawn runs it without holding the registry
// lock, so the factory may call Get and List of the Orchestration that runs it; a
// Spawn of the same id from inside it fails with ErrAgentExists (the id is reserved).
type Factory func(ctx context.Context, p profile.AgentProfile) (*agent.Agent, error)
