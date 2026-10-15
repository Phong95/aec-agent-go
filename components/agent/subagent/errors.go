package subagent

import "github.com/Phong95/aec-agent-go/kernel"

// A-105: why a parent agent could not hand a task to a subagent. Each is decided before
// the child runs, so retrying the same call cannot succeed.

// ErrorCode is the failure category of an Error.
type ErrorCode string

// The Error codes. The string values are stable wire values, the codes of core/ts
// SubagentError.
const (
	// CodeNotAllowed: the agent has no subagents configured, or the target is not on its
	// allowlist.
	CodeNotAllowed ErrorCode = "not_allowed"
	// CodeNotFound: the target agent does not exist (or is not running in this process).
	CodeNotFound ErrorCode = "not_found"
	// CodeDepthExceeded: a subagent tried to delegate (V1 nesting depth is 1), or an
	// agent named itself.
	CodeDepthExceeded ErrorCode = "depth_exceeded"
	// CodeInvalidArgument: a missing or oversized task, context or output shape.
	CodeInvalidArgument ErrorCode = "invalid_argument"
	// CodeWriteUnavailable: a write-capable child needs its own sandbox, and only one
	// writer may run in it at a time.
	CodeWriteUnavailable ErrorCode = "write_unavailable"
	// CodeAlreadyExists: a subagent type with that name already exists (or a built-in or
	// app type can't be changed).
	CodeAlreadyExists ErrorCode = "already_exists"
)

// Error is a refusal of Subagents.Spawn (the SubagentError of core/ts). It is never
// retryable.
type Error struct {
	kernel.SDKError
	Code ErrorCode
}

// NewError creates an Error.
func NewError(message string, code ErrorCode) *Error {
	return &Error{SDKError: kernel.SDKError{Message: message, Retryable: false}, Code: code}
}

// Name returns the class name ("SubagentError"), the name core/ts uses.
func (e *Error) Name() string { return "SubagentError" }

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
