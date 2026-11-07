package persistence

import (
	"context"

	"github.com/Phong95/aec-agent-go/components/mcphub/mcpserver"
)

// ---------------------------------------------------------------------------
// MCP server persistence (A-208) - the custom MCP servers created at runtime. App servers
// (AgentCore.UseMcp) live only in the mcphub.McpHub. Same pluggable-store pattern as
// SubagentTypeStore; write through the McpHub so its cache stays current. Header and env
// values are kept as written: put keys in them as {{secret:NAME}}, never in clear.
// ---------------------------------------------------------------------------

// McpServerStore persists custom MCP server definitions.
type McpServerStore interface {
	// List returns every stored custom server, sorted by id.
	List(ctx context.Context) ([]mcpserver.Server, error)
	// Save inserts or replaces the server by id.
	Save(ctx context.Context, s mcpserver.Server) error
	// Delete removes the server; an unknown id is not an error.
	Delete(ctx context.Context, id string) error
}

// ---------------------------------------------------------------------------
// MCP sign-in persistence (A-214) - one record per MCP server that has an OAuth sign-in
// (its client registration and tokens). Data is sealed by the McpHub's OAuth with the
// SecretHub (aad "mcp-auth:<serverId>") before it reaches the store, so a store keeps it
// as an opaque string. Write through McpHub.OAuth so its cache stays current.
// ---------------------------------------------------------------------------

// McpAuthRecord is one server's stored sign-in.
type McpAuthRecord struct {
	ServerID string `json:"serverId"`
	// Data is the sealed JSON of the sign-in (mcpoauth.Data).
	Data      string `json:"data"`
	UpdatedAt string `json:"updatedAt"`
}

// McpAuthStore persists MCP sign-ins.
type McpAuthStore interface {
	// Get returns the server's record; the bool is false when there is none.
	Get(ctx context.Context, serverID string) (McpAuthRecord, bool, error)
	// Save inserts or replaces the record by ServerID.
	Save(ctx context.Context, r McpAuthRecord) error
	// Delete removes the record; an unknown id is not an error.
	Delete(ctx context.Context, serverID string) error
}
