package artifact

import (
	"context"

	"github.com/Phong95/aec-agent-go/components/sandboxhub"
)

// View is one agent's view of the artifact store - the only thing tools, routes
// and the SDK ever touch (given to tools as toolhub.Context.GetArtifacts). Every
// call is scoped to AgentID, so one agent can never read or overwrite another
// agent's artifacts. Every method fails closed with a *kernel.ArtifactError.
type View interface {
	AgentID() string
	// SessionID is the session stamped on versions written through this view
	// (provenance); "" when the view is unscoped.
	SessionID() string

	// Save creates a new artifact at version 1. Error code "name_conflict" if the
	// name is taken.
	Save(ctx context.Context, in SaveInput, sb sandboxhub.View) (Write, error)
	// Update appends a version to an existing artifact, re-tracking (or rewriting)
	// its file.
	Update(ctx context.Context, in UpdateInput, sb sandboxhub.View) (Write, error)
	// Get returns the metadata only - no sandbox needed.
	Get(ctx context.Context, ref Ref) (Artifact, error)
	// Read reads the tracked file live through sb. Content is bounded only by
	// opts.MaxBytes.
	Read(ctx context.Context, ref Ref, sb sandboxhub.View, opts ReadOptions) (Read, error)
	List(ctx context.Context, q Query) ([]Artifact, error)
	// Versions returns every version of an artifact, newest first, content-less.
	Versions(ctx context.Context, ref Ref) ([]Version, error)
	// Delete deletes the artifact record and its history. The sandbox file is left
	// untouched.
	Delete(ctx context.Context, ref Ref) error
}
