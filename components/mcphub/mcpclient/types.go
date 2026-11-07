// Package mcpclient is the wire side of MCP (A-208): JSON-RPC 2.0 messages, the Transport
// contract a Client talks through, the Client itself (the initialize handshake, requests
// with time limits and cancellation, notifications, the server's own requests) and the
// streamable HTTP transport. The stdio transport starts a host process, so it lives in
// the opt-in package mcphub/stdio. It is the McpClient, HttpTransport and client/types of
// core/ts.
package mcpclient

import (
	"context"
	"encoding/json"

	"github.com/Phong95/aec-agent-go/components/mcphub/mcpserver"
)

// Message is one JSON-RPC 2.0 message: a request (ID and Method), a notification (Method
// only) or a response (ID and Result or Error). ID is kept raw, so a string id and a
// number id both round-trip.
type Message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// IsRequest reports whether m is a request (an id and a method).
func (m Message) IsRequest() bool { return m.Method != "" && hasID(m.ID) }

// IsNotification reports whether m is a notification (a method and no id).
func (m Message) IsNotification() bool { return m.Method != "" && !hasID(m.ID) }

// IsResponse reports whether m is a response (an id and no method).
func (m Message) IsResponse() bool { return m.Method == "" && hasID(m.ID) }

func hasID(id json.RawMessage) bool { return len(id) > 0 && string(id) != "null" }

// RPCError is the error object of a JSON-RPC response.
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// Handlers are what a Transport calls back. OnMessage gets every message from the
// server; OnClose is called at most once when the connection ends without Close. Both
// may run on the transport's own goroutines and must not block.
type Handlers struct {
	OnMessage func(Message)
	OnClose   func(err error)
}

// Transport is one connection to an MCP server. The Client owns it: Start once, Send
// many, Close once. Send returns when the message is handed over (HTTP: once the POST's
// answer was read); answers to requests arrive through Handlers.OnMessage.
type Transport interface {
	Start(ctx context.Context, h Handlers) error
	Send(ctx context.Context, m Message) error
	// SetProtocolVersion is the version the server agreed in initialize (HTTP sends it on
	// every later request).
	SetProtocolVersion(version string)
	// Close ends the connection; it is idempotent.
	Close() error
}

// Factory builds the transport of a server from its transport config (with
// {{secret:NAME}} already filled in) and what the MCP manager gives besides (A-214).
type Factory func(cfg mcpserver.Transport, tc TransportContext) (Transport, error)

// TransportContext is what the MCP manager gives a Factory besides the config (the
// McpTransportContext of core/ts, A-214).
type TransportContext struct {
	// Auth is the server's OAuth sign-in for http (bearer token, refresh after a 401).
	// nil = no OAuth (e.g. the server sends its own Authorization header).
	Auth HTTPAuth
}

// HTTPAuth is how an HTTP transport gets its bearer token (the McpHttpAuth of core/ts,
// A-214). Token is read before every request ("" = none); Refresh is called once after a
// 401 with the token that was refused and reports whether a new one is there to try again
// with. Both must be safe for concurrent use.
type HTTPAuth interface {
	Token(ctx context.Context) (string, error)
	Refresh(ctx context.Context, rejected string) bool
}

// ProtocolVersions are the protocol versions this client speaks, newest first; the first
// is the one it asks for.
var ProtocolVersions = []string{"2025-06-18", "2025-03-26", "2024-11-05"}

// LatestProtocolVersion is the version the client asks for.
const LatestProtocolVersion = "2025-06-18"

// MethodNotFound is the JSON-RPC error code the client answers unsupported server
// requests with.
const MethodNotFound = -32601
