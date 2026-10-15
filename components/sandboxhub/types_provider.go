package sandboxhub

import (
	"context"

	"github.com/Phong95/aec-agent-go/kernel"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// ---------------------------------------------------------------------------
// Provider creates sandboxes of one kind (docker, host, remote). The sandbox
// hub (Phase 7) decides WHICH sandbox an agent gets; providers only know HOW to
// build and run one.
//
// This file holds Spec, CreateContext, Provider and ProviderDataRemover
// (ResourceLimits and LimitsOverride are in types.go).
// ---------------------------------------------------------------------------

// Spec is everything a provider needs to create one sandbox.
type Spec struct {
	Kind Kind `json:"kind"`
	// Image is the container image (docker).
	Image   string      `json:"image,omitempty"`
	Network NetworkMode `json:"network,omitempty"`
	// Limits is a partial ResourceLimits (only the set fields apply).
	Limits *LimitsOverride `json:"limits,omitempty"`
	// Options are provider-specific settings (e.g. a host folder, a runner
	// address). Never taken from agent profiles. It keeps the key order of the
	// JSON object at every nesting level (ordered.Object).
	Options ordered.Object `json:"options,omitzero"`
}

// CreateContext is the context of one Provider.Create call.
type CreateContext struct {
	// ID is the stable id the hub assigned, e.g. "docker:shared", "docker:own:<agentId>".
	ID      string  `json:"id"`
	Tenancy Tenancy `json:"tenancy"`
	// AgentID is the owning agent for "own" tenancy.
	AgentID string `json:"agentId,omitempty"`
}

// Provider creates sandboxes of one kind.
type Provider interface {
	kernel.Plugin
	Kind() Kind
	// Create creates or reconnects to a sandbox. It is called lazily on first
	// use, never at agent spawn.
	Create(ctx context.Context, spec Spec, cc CreateContext) (Sandbox, error)
}

// ProviderDataRemover is the optional part of Provider that deletes the stored
// data of a sandbox that is NOT running in this process - the whole sandbox
// (own tenancy), or with a non-empty agentID one agent's folder in a shared one.
// It is used when an agent is deleted; running sandboxes are cleaned through
// Sandbox instead. A caller checks for it with a type assertion.
type ProviderDataRemover interface {
	RemoveData(ctx context.Context, spec Spec, cc CreateContext, agentID string) error
}
