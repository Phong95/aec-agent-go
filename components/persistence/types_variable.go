package persistence

import (
	"context"

	"github.com/Phong95/aec-agent-go/components/variablehub/variable"
)

// ---------------------------------------------------------------------------
// Variable persistence (A-215) - names and values of variables, stored as they are (they
// are not secret). Write through the variablehub.VariableHub, which checks names, limits
// and owners. The owner is an agent id, or "" for a global variable.
// ---------------------------------------------------------------------------

// VariableStore persists variables.
type VariableStore interface {
	// Get returns one variable of an owner ("" = global), or a nil pointer and a nil error
	// when there is none.
	Get(ctx context.Context, owner, name string) (*variable.Info, error)
	// List returns the variables of one owner ("" = global), sorted by name.
	List(ctx context.Context, owner string) ([]variable.Info, error)
	// ListAll returns every variable, globals first, then by owner and name (the list()
	// without an owner of core/ts).
	ListAll(ctx context.Context) ([]variable.Info, error)
	// Save inserts or replaces the variable by (owner, name).
	Save(ctx context.Context, v variable.Info) error
	// Delete removes one variable; an unknown one is not an error.
	Delete(ctx context.Context, owner, name string) error
	// DeleteByOwner deletes every variable an agent owns (the agent was deleted). An empty
	// agent id deletes nothing.
	DeleteByOwner(ctx context.Context, agentID string) error
}
