package command

import "github.com/Phong95/aec-agent-go/kernel"

// A-136: registering a slash command failed. Running one never returns an Error: user
// errors come back in its Result.

// ErrorCode is the failure category of an Error.
type ErrorCode string

// The Error codes. The string values are stable wire values, the codes of core/ts
// CommandError.
const (
	// CodeConflict: the command's name or an alias is already taken (a built-in, a host
	// or an agent command).
	CodeConflict ErrorCode = "conflict"
	// CodeInvalid: a bad command spec (name, aliases, or not exactly one of Run and Prompt).
	CodeInvalid ErrorCode = "invalid"
)

// Error is a refusal to register a command (the CommandError of core/ts). It is never
// retryable.
type Error struct {
	kernel.SDKError
	Code ErrorCode
}

// NewError creates an Error.
func NewError(message string, code ErrorCode) *Error {
	return &Error{SDKError: kernel.SDKError{Message: message, Retryable: false}, Code: code}
}

// Name returns the class name ("CommandError"), the name core/ts uses.
func (e *Error) Name() string { return "CommandError" }

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
