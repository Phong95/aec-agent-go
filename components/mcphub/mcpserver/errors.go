package mcpserver

import "github.com/Phong95/aec-agent-go/kernel"

// A-208: why an MCP server could not be registered, reached or used.

// ErrorCode is the failure category of an Error.
type ErrorCode string

// The Error codes. The string values are stable wire values, the codes of core/ts
// McpError.
const (
	// CodeInvalidArgument: a server definition (id, transport, timeouts...) is invalid.
	CodeInvalidArgument ErrorCode = "invalid_argument"
	// CodeNotFound: no MCP server with that id (in the McpHub, or among the agent's servers).
	CodeNotFound ErrorCode = "not_found"
	// CodeAlreadyExists: a server with that id already exists (or a registered app server
	// can't be changed).
	CodeAlreadyExists ErrorCode = "already_exists"
	// CodeNotConnected: the server is not connected (disabled, still connecting, or gave
	// up reconnecting).
	CodeNotConnected ErrorCode = "not_connected"
	// CodeConnectFailed: starting the transport or the initialize handshake failed.
	CodeConnectFailed ErrorCode = "connect_failed"
	// CodeTimeout: a request got no answer in time.
	CodeTimeout ErrorCode = "timeout"
	// CodeProtocol: the server sent something that is not valid MCP / JSON-RPC, or an
	// unsupported protocol version.
	CodeProtocol ErrorCode = "protocol"
	// CodeServerError: the server answered a request with a JSON-RPC error.
	CodeServerError ErrorCode = "server_error"
	// CodeClosed: the connection closed while the request was waiting.
	CodeClosed ErrorCode = "closed"
	// CodeUnauthorized: the server refused the request (HTTP 401 / 403) - it needs an
	// OAuth sign-in (or other credentials). A-214.
	CodeUnauthorized ErrorCode = "unauthorized"
	// CodeAuthFailed: an OAuth sign-in step failed (discovery, client registration, the
	// token exchange, the user said no). A-214.
	CodeAuthFailed ErrorCode = "auth_failed"
)

// Error is an MCP failure (the McpError of core/ts). Timeout, closed and not_connected
// are retryable; the others are not.
type Error struct {
	kernel.SDKError
	Code ErrorCode
	// RPCCode is the JSON-RPC error code the server sent (CodeServerError only).
	RPCCode int
}

// NewError creates an Error.
func NewError(message string, code ErrorCode) *Error {
	retryable := code == CodeTimeout || code == CodeClosed || code == CodeNotConnected
	return &Error{SDKError: kernel.SDKError{Message: message, Retryable: retryable}, Code: code}
}

// Name returns the class name ("McpError"), the name core/ts uses.
func (e *Error) Name() string { return "McpError" }

// Unwrap returns the embedded SDKError so errors.As to *kernel.SDKError works.
func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return &e.SDKError
}

// Is matches an *Error target with an empty Code (class level) or an equal Code.
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return e != nil && ok && t != nil && (t.Code == "" || t.Code == e.Code)
}
