package persistence

import (
	"context"

	"github.com/Phong95/aec-agent-go/components/agent/memorysystem/memory"
)

// ---------------------------------------------------------------------------
// Pluggable stores - same adapter pattern as SessionStore in kernel
// ---------------------------------------------------------------------------

// MemoryFilter selects memory records. Every field is optional: an empty string,
// an empty list and a Limit of 0 all mean "no filter".
type MemoryFilter struct {
	// AgentID restricts to one agent's memories. Core memory components always
	// set this.
	AgentID  string          `json:"agentId,omitempty"`
	Kinds    []memory.Kind   `json:"kinds,omitzero"`
	Scopes   []memory.Scope  `json:"scopes,omitzero"`
	Statuses []memory.Status `json:"statuses,omitzero"`
	Subject  string          `json:"subject,omitempty"`
	Key      string          `json:"key,omitempty"`
	// Tags keeps records that carry at least one of the tags.
	Tags []string `json:"tags,omitzero"`
	// Since keeps records observed at or after it (ISO string, inclusive).
	Since string `json:"since,omitempty"`
	// Before keeps records observed before it (ISO string, exclusive).
	Before string `json:"before,omitempty"`
	// Limit keeps the first Limit matches in store order; 0 means no limit.
	Limit int `json:"limit,omitempty"`
}

// Memories are agent-owned: Record.AgentID is stamped at creation, and every
// read path (Query, FindByKey) is filtered by agent so one agent's
// dedup/expiry/retrieval never touches another agent's records.

// MemoryStore is the durable memory record persistence.
type MemoryStore interface {
	Save(ctx context.Context, r memory.Record) error
	// Get returns the record, or a nil pointer and a nil error when the id is
	// unknown.
	Get(ctx context.Context, id string) (*memory.Record, error)
	Update(ctx context.Context, r memory.Record) error
	Delete(ctx context.Context, id string) error
	Query(ctx context.Context, f MemoryFilter) ([]memory.Record, error)
	// FindByKey returns the active record of the agent with the scope and key, or
	// a nil pointer and a nil error when there is none.
	FindByKey(ctx context.Context, agentID string, scope memory.Scope, key string) (*memory.Record, error)
}
