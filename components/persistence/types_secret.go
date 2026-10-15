package persistence

import (
	"context"

	"github.com/Phong95/aec-agent-go/components/secrethub/secret"
)

// ---------------------------------------------------------------------------
// Secret persistence (A-101) - sealed secret values and their metadata. A store never
// sees a plain value (unless the host runs without a SecretHub provider); write through the
// SecretHub, which seals values and keeps the owner rules.
// The owner is an agent id, or "" for a global secret.
// ---------------------------------------------------------------------------

// SecretStore persists secrets.
type SecretStore interface {
	// Get returns one secret of an owner ("" = global), or a nil pointer and a nil error
	// when there is none.
	Get(ctx context.Context, owner, name string) (*secret.Stored, error)
	// List returns the secrets of one owner ("" = global), sorted by name.
	List(ctx context.Context, owner string) ([]secret.Stored, error)
	// ListAll returns every secret, globals first, then by owner and name (the list()
	// without an owner of core/ts).
	ListAll(ctx context.Context) ([]secret.Stored, error)
	// Save inserts or replaces the secret by (owner, name).
	Save(ctx context.Context, s secret.Stored) error
	// MarkUsed records a use (LastUsedAt = at); it does nothing when the secret is gone.
	MarkUsed(ctx context.Context, owner, name, at string) error
	// Delete removes one secret; an unknown one is not an error.
	Delete(ctx context.Context, owner, name string) error
	// DeleteByOwner deletes every secret an agent owns (the agent was deleted). An empty
	// agent id deletes nothing.
	DeleteByOwner(ctx context.Context, agentID string) error
}
