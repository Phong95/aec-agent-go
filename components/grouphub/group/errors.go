package group

import "github.com/Phong95/aec-agent-go/kernel"

// A-106: why a group chat request could not be done. Each is decided before any member
// runs, so retrying the same call cannot succeed (except CodeBusy, once the turn ends).

// ErrorCode is the failure category of an Error.
type ErrorCode string

// The Error codes. The string values are stable wire values, the codes of core/ts
// GroupError.
const (
	// CodeInvalidArgument: a bad group definition or message - too few members, a lead who
	// is not a member, a muted lead, an empty message.
	CodeInvalidArgument ErrorCode = "invalid_argument"
	// CodeNotFound: no such group, group session or message, or the session is not one of
	// this group's.
	CodeNotFound ErrorCode = "not_found"
	// CodeUnknownAgent: a member is not a running agent of this host.
	CodeUnknownAgent ErrorCode = "unknown_agent"
	// CodeBusy: a group turn is already running in this session; wait for it or stop it.
	CodeBusy ErrorCode = "busy"
)

// Error is a refusal of a group hub call (the GroupError of core/ts). It is never
// retryable.
type Error struct {
	kernel.SDKError
	Code ErrorCode
}

// NewError creates an Error.
func NewError(message string, code ErrorCode) *Error {
	return &Error{SDKError: kernel.SDKError{Message: message, Retryable: false}, Code: code}
}

// Name returns the class name ("GroupError"), the name core/ts uses.
func (e *Error) Name() string { return "GroupError" }

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
