package persistence

import (
	"context"

	"github.com/Phong95/aec-agent-go/components/agent/subagent"
	"github.com/Phong95/aec-agent-go/components/subagenthub/subagenttype"
)

// ---------------------------------------------------------------------------
// Subagent run persistence (A-105) - one record per child run a parent agent
// started, so the parent's chat can show what ran, how long it took and what the
// child returned, after a restart too. Records are written by the parent's
// subagent manager: once when the child starts ("running") and once when it ends.
//
// A durable store should mark records still "running" when the process starts as
// "cancelled" (their turn died with the previous process).
// ---------------------------------------------------------------------------

// SubagentRunStore persists subagent runs.
type SubagentRunStore interface {
	// Save inserts or replaces the run by id.
	Save(ctx context.Context, run subagent.Run) error
	// Get returns the run, or a nil pointer and a nil error when the id is unknown.
	Get(ctx context.Context, id string) (*subagent.Run, error)
	// ListByParentSession returns the runs started from one parent session, oldest
	// started first (ties by id).
	ListByParentSession(ctx context.Context, parentSessionID string) ([]subagent.Run, error)
	// DeleteByAgent deletes every run an agent started (the agent was deleted).
	DeleteByAgent(ctx context.Context, agentID string) error
}

// ---------------------------------------------------------------------------
// Subagent type persistence (A-105, design revision) - the custom subagent types users
// create. Built-in and app types live only in the subagenthub.SubagentHub. Same
// pluggable-store pattern as SkillStore; write through the SubagentHub so its cache
// stays current.
// ---------------------------------------------------------------------------

// SubagentTypeStore persists custom subagent types.
type SubagentTypeStore interface {
	// List returns every stored custom type.
	List(ctx context.Context) ([]subagenttype.Type, error)
	// Save inserts or replaces the type by name.
	Save(ctx context.Context, t subagenttype.Type) error
	// Delete removes the type; an unknown name is not an error.
	Delete(ctx context.Context, name string) error
}
