package secret

import "github.com/Phong95/aec-agent-go/kernel"

// A-101: why a secret could not be stored, found, used or opened. None of these change
// when the same call is retried.

// ErrorCode is the failure category of an Error.
type ErrorCode string

// The Error codes. The string values are stable wire values, the codes of core/ts
// SecretError.
const (
	// CodeInvalidArgument: a bad name, description, host, tool list or value.
	CodeInvalidArgument ErrorCode = "invalid_argument"
	// CodeNotFound: no secret of that name for this owner (or the agent may not use that
	// global secret).
	CodeNotFound ErrorCode = "not_found"
	// CodeAlreadyExists: a secret of that name already exists for this owner.
	CodeAlreadyExists ErrorCode = "already_exists"
	// CodeNotAllowed: the secret may not be used by this tool or sent to this host.
	CodeNotAllowed ErrorCode = "not_allowed"
	// CodeDecryptFailed: the value could not be decrypted - the master key that sealed it
	// is missing or wrong.
	CodeDecryptFailed ErrorCode = "decrypt_failed"
	// CodeUnavailable: the SecretHub provider failed (KMS unreachable, access denied), or no user
	// can enter a value.
	CodeUnavailable ErrorCode = "unavailable"
)

// Error is a secret failure (the SecretError of core/ts). It is never retryable.
type Error struct {
	kernel.SDKError
	Code ErrorCode
}

// NewError creates an Error.
func NewError(message string, code ErrorCode) *Error {
	return &Error{SDKError: kernel.SDKError{Message: message, Retryable: false}, Code: code}
}

// Name returns the class name ("SecretError"), the name core/ts uses.
func (e *Error) Name() string { return "SecretError" }

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
