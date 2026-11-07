package mcpserver

import (
	"context"
	"slices"
	"time"
)

// ---------------------------------------------------------------------------
// MCP servers of one agent (A-208) - what the agent's MCP manager offers its hosts and
// A-142's tool adapter (the AgentMcp and McpServerState of core/ts).
// ---------------------------------------------------------------------------

// ConnectionStatus is where a connection is. The string values are stable wire values.
type ConnectionStatus string

// The connection statuses.
const (
	// StatusConnecting: the first handshake is running.
	StatusConnecting ConnectionStatus = "connecting"
	// StatusConnected: handshake and discovery done, requests go through.
	StatusConnected ConnectionStatus = "connected"
	// StatusReconnecting: the connection was lost or a try failed, another try is scheduled.
	StatusReconnecting ConnectionStatus = "reconnecting"
	// StatusFailed: gave up (bad definition, unknown server, or out of reconnect tries) -
	// Reconnect tries again.
	StatusFailed ConnectionStatus = "failed"
	// StatusDisabled: the server is disabled in the McpHub.
	StatusDisabled ConnectionStatus = "disabled"
	// StatusNeedsAuth: the server refused the connection (401 / 403) - sign in
	// (McpHub.OAuth), and the manager reconnects by itself once the sign-in is done (A-214).
	StatusNeedsAuth ConnectionStatus = "needs_auth"
)

// State is one of the agent's servers as it is now - what it offers once connected.
type State struct {
	ServerID string           `json:"serverId"`
	Name     string           `json:"name"`
	Status   ConnectionStatus `json:"status"`
	// Error is the last error (connect, discovery, health check).
	Error           string          `json:"error,omitempty"`
	ProtocolVersion string          `json:"protocolVersion,omitempty"`
	ServerInfo      *Implementation `json:"serverInfo,omitempty"`
	// Instructions is the server's own advice for using it (initialize result).
	Instructions string        `json:"instructions,omitempty"`
	Capabilities *Capabilities `json:"capabilities,omitempty"`
	Tools        []Tool        `json:"tools"`
	Resources    []Resource    `json:"resources"`
	Prompts      []Prompt      `json:"prompts"`
	// ConnectedAt is when the current connection was made (ISO 8601), "" when not connected.
	ConnectedAt string `json:"connectedAt,omitempty"`
	// Attempts is the failed tries since the last good connection.
	Attempts int `json:"attempts"`
}

// Clone returns a copy whose lists can be changed freely (the list entries are shared:
// they are never changed in place).
func (s State) Clone() State {
	s.Tools = slices.Clone(s.Tools)
	s.Resources = slices.Clone(s.Resources)
	s.Prompts = slices.Clone(s.Prompts)
	if s.Tools == nil {
		s.Tools = []Tool{}
	}
	if s.Resources == nil {
		s.Resources = []Resource{}
	}
	if s.Prompts == nil {
		s.Prompts = []Prompt{}
	}
	if s.ServerInfo != nil {
		v := *s.ServerInfo
		s.ServerInfo = &v
	}
	if s.Capabilities != nil {
		v := *s.Capabilities
		s.Capabilities = &v
	}
	return s
}

// CallOptions tune one call.
type CallOptions struct {
	// Timeout overrides the server's timeouts.requestMs for this call (0 = the server's).
	Timeout time.Duration
}

// Connections is the agent's MCP servers as hosts and A-142's tool adapter reach them
// (Agent.Mcp) - the AgentMcp of core/ts. Errors are *Error ("not_found", "not_connected",
// "timeout", "server_error"...). It is safe for concurrent use.
type Connections interface {
	// List returns every server in the profile, in profile order.
	List() []State
	// Get returns one server's state; false when it is not one of the agent's servers.
	Get(serverID string) (State, bool)
	// Reconnect closes the connection (if any) and connects again now.
	Reconnect(ctx context.Context, serverID string) (State, error)
	// Ping does a ping round trip.
	Ping(ctx context.Context, serverID string, opts CallOptions) error
	CallTool(ctx context.Context, serverID, name string, args map[string]any, opts CallOptions) (CallToolResult, error)
	ReadResource(ctx context.Context, serverID, uri string, opts CallOptions) (ReadResourceResult, error)
	GetPrompt(ctx context.Context, serverID, name string, args map[string]string, opts CallOptions) (GetPromptResult, error)
}

// AgentTool (A-213, McpAgentTool of core/ts) is one of the agent's MCP tools as hosts show
// it (Agent.McpToolList).
type AgentTool struct {
	// Name is the agent tool name: mcp__<server>__<tool>.
	Name     string `json:"name"`
	ServerID string `json:"serverId"`
	// Tool is the tool's name on the server.
	Tool  string `json:"tool"`
	Title string `json:"title,omitempty"`
	// ReadOnly is the tool's readOnlyHint: plan mode offers it.
	ReadOnly bool `json:"readOnly"`
	// Approval is ApprovalNever (runs without asking), ApprovalAsk (asks; YOLO skips it),
	// ApprovalAlways (asks every time) or ApprovalDenied (not offered).
	Approval string `json:"approval"`
	// Deferred is true when tool search defers the agent's MCP tools.
	Deferred bool `json:"deferred"`
}

// The values of AgentTool.Approval (A-213).
const (
	ApprovalNever  = "never"
	ApprovalAsk    = "ask"
	ApprovalAlways = "always"
	ApprovalDenied = "denied"
)
