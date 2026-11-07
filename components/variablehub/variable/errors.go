package variable

import "github.com/Phong95/aec-agent-go/kernel"

// A-215: why a variable could not be stored, found or used. None of these change when the
// same call is retried.

// ErrorCode is the failure category of an Error.
type ErrorCode string

// The Error codes. The string values are stable wire values, the codes of core/ts
// VariableError.
const (
	// CodeInvalidArgument: a bad name, description or value (or a value that equals a
	// secret's).
	CodeInvalidArgument ErrorCode = "invalid_argument"
	// CodeNotFound: no variable of that name for this owner (or the agent may not use that
	// global variable).
	CodeNotFound ErrorCode = "not_found"
	// CodeAlreadyExists: a variable of that name already exists for this owner.
	CodeAlreadyExists ErrorCode = "already_exists"
	// CodeUnavailable: the host has no variable store.
	CodeUnavailable ErrorCode = "unavailable"
)

// Error is a variable failure (the VariableError of core/ts). It is never retryable.
type Error struct {
	kernel.SDKError
	Code ErrorCode
}

// NewError creates an Error.
func NewError(message string, code ErrorCode) *Error {
	return &Error{SDKError: kernel.SDKError{Message: message, Retryable: false}, Code: code}
}

// Name returns the class name ("VariableError"), the name core/ts uses.
func (e *Error) Name() string { return "VariableError" }

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
