package kernel

import "errors"

// This file holds the error classes whose retryability is derived from the code
// (the sandbox and artifact errors): the constructor computes
// Retryable as "code is not in the non-retryable set", it is never set by the
// caller.

// SandboxErrorCode is the failure category of a SandboxError.
type SandboxErrorCode string

// The SandboxError codes. The string values are stable wire values.
const (
	// SandboxCodeNotConfigured: the agent uses the shared sandbox but none is configured on the SandboxHub.
	SandboxCodeNotConfigured SandboxErrorCode = "not_configured"
	// SandboxCodeDisabled: the agent's profile selects sandbox "none".
	SandboxCodeDisabled SandboxErrorCode = "disabled"
	// SandboxCodeProviderUnavailable: no SandboxProvider is registered for the sandbox kind.
	SandboxCodeProviderUnavailable SandboxErrorCode = "provider_unavailable"
	// SandboxCodeCapabilityUnsupported: the sandbox does not offer the requested operation (e.g. exec, shell).
	SandboxCodeCapabilityUnsupported SandboxErrorCode = "capability_unsupported"
	// SandboxCodePathEscape: a path resolves outside the roots the agent may use (traversal, absolute, symlink).
	SandboxCodePathEscape SandboxErrorCode = "path_escape"
	SandboxCodeNotFound   SandboxErrorCode = "not_found"
	// SandboxCodeTooLarge: input or content above the sandbox's limits.
	SandboxCodeTooLarge SandboxErrorCode = "too_large"
	// SandboxCodeConflict: writeFile's expectedSha256 did not match the current file.
	SandboxCodeConflict  SandboxErrorCode = "conflict"
	SandboxCodeTimeout   SandboxErrorCode = "timeout"
	SandboxCodeCancelled SandboxErrorCode = "cancelled"
	// SandboxCodePolicyDenied: selection or configuration rejected by the hub policy (image, network, named env).
	SandboxCodePolicyDenied SandboxErrorCode = "policy_denied"
	// SandboxCodeEnvironmentUnavailable: the sandbox exists but cannot serve requests (container down, runner offline).
	SandboxCodeEnvironmentUnavailable SandboxErrorCode = "environment_unavailable"
	// SandboxCodeInvalidPath: the path is malformed or the wrong type (e.g. reading a directory).
	SandboxCodeInvalidPath SandboxErrorCode = "invalid_path"
	// SandboxCodeInvalidArgument: any other malformed request (bad regex, non-positive timeout, empty command).
	SandboxCodeInvalidArgument SandboxErrorCode = "invalid_argument"
)

// nonRetryableSandboxCodes are the codes where retrying the same call cannot
// succeed; tools map these to retryable false.
var nonRetryableSandboxCodes = map[SandboxErrorCode]struct{}{
	SandboxCodeNotConfigured:         {},
	SandboxCodeDisabled:              {},
	SandboxCodeProviderUnavailable:   {},
	SandboxCodeCapabilityUnsupported: {},
	SandboxCodePathEscape:            {},
	SandboxCodeNotFound:              {},
	SandboxCodeTooLarge:              {},
	SandboxCodeConflict:              {},
	SandboxCodePolicyDenied:          {},
	SandboxCodeInvalidPath:           {},
	SandboxCodeInvalidArgument:       {},
}

// SandboxError is a failure reported by a sandbox.
type SandboxError struct {
	SDKError
	Code SandboxErrorCode
	// SandboxID is the sandbox the failure came from, when one was resolved ("" otherwise).
	SandboxID string
}

// NewSandboxError creates a SandboxError; Retryable is derived from the code.
func NewSandboxError(message string, code SandboxErrorCode, sandboxID string) *SandboxError {
	_, nonRetryable := nonRetryableSandboxCodes[code]
	return &SandboxError{SDKError: SDKError{Message: message, Retryable: !nonRetryable}, Code: code, SandboxID: sandboxID}
}

// Name returns the class name ("SandboxError"), the stable name used by wire fixtures and
// logs. Go code should match with errors.As, errors.Is or HasCode instead.
func (e *SandboxError) Name() string { return "SandboxError" }

// sdkError returns the embedded SDKError (nil for a nil e), for WithCause.
func (e *SandboxError) sdkError() *SDKError {
	if e == nil {
		return nil
	}
	return &e.SDKError
}

// Unwrap returns the embedded SDKError so errors.As to *SDKError works; the
// SDKError unwraps in turn to the cause (WithCause).
func (e *SandboxError) Unwrap() error {
	if e == nil {
		return nil
	}
	return &e.SDKError
}

// Is matches a *SandboxError target with an empty Code (class level) or equal Code.
func (e *SandboxError) Is(target error) bool {
	t, ok := target.(*SandboxError)
	return e != nil && ok && t != nil && (t.Code == "" || t.Code == e.Code)
}

// AsSandboxError wraps an unknown failure from a sandbox as a retryable
// "environment_unavailable" SandboxError. An error that already is (or wraps) a
// *SandboxError is returned unchanged.
//
// The message is built by plain concatenation, Sandbox "<id>" is unavailable:
// <cause>, with literal double quotes.
//
// A nil err is a programmer error; the text "<nil>" is used for it.
//
// The new error carries err as its cause (WithCause), so errors.Is and errors.As
// still reach the original failure; the message is unchanged by it.
func AsSandboxError(err error, sandboxID string) *SandboxError {
	var existing *SandboxError
	if errors.As(err, &existing) && existing != nil {
		return existing
	}
	cause := "<nil>"
	if err != nil {
		cause = err.Error()
	}
	return WithCause(NewSandboxError(`Sandbox "`+sandboxID+`" is unavailable: `+cause, SandboxCodeEnvironmentUnavailable, sandboxID), err)
}

// ArtifactErrorCode is the failure category of an ArtifactError.
type ArtifactErrorCode string

// The ArtifactError codes. The string values are stable wire values.
const (
	// ArtifactCodeNotFound: no artifact with that id/name for this agent.
	ArtifactCodeNotFound ArtifactErrorCode = "not_found"
	// ArtifactCodeNameConflict: save_artifact used a name this agent already owns; update_artifact is the way to change it.
	ArtifactCodeNameConflict ArtifactErrorCode = "name_conflict"
	// ArtifactCodeInvalidArgument: malformed name, type or reference.
	ArtifactCodeInvalidArgument ArtifactErrorCode = "invalid_argument"
	// ArtifactCodeConflict: expectedVersion did not match the current head; someone else wrote in between.
	ArtifactCodeConflict ArtifactErrorCode = "conflict"
	// ArtifactCodeFileMissing: the tracked (or to-be-tracked) file is not in the agent's sandbox.
	ArtifactCodeFileMissing ArtifactErrorCode = "file_missing"
	// ArtifactCodeNotConfigured: no ArtifactStore is wired into the PersistenceHub.
	ArtifactCodeNotConfigured ArtifactErrorCode = "not_configured"
)

// nonRetryableArtifactCodes are the codes where retrying the same call cannot
// succeed. Every artifact failure is deterministic today; the set is explicit
// so a future transient code (e.g. a store timeout) is retryable by default.
var nonRetryableArtifactCodes = map[ArtifactErrorCode]struct{}{
	ArtifactCodeNotFound:        {},
	ArtifactCodeNameConflict:    {},
	ArtifactCodeInvalidArgument: {},
	ArtifactCodeConflict:        {},
	ArtifactCodeFileMissing:     {},
	ArtifactCodeNotConfigured:   {},
}

// ArtifactError is a failure of an artifact operation.
type ArtifactError struct {
	SDKError
	Code ArtifactErrorCode
	// ArtifactID is the artifact the failure is about, when one was resolved ("" otherwise).
	ArtifactID string
}

// NewArtifactError creates an ArtifactError; Retryable is derived from the code.
func NewArtifactError(message string, code ArtifactErrorCode, artifactID string) *ArtifactError {
	_, nonRetryable := nonRetryableArtifactCodes[code]
	return &ArtifactError{SDKError: SDKError{Message: message, Retryable: !nonRetryable}, Code: code, ArtifactID: artifactID}
}

// Name returns the class name ("ArtifactError"), the stable name used by wire fixtures and
// logs. Go code should match with errors.As, errors.Is or HasCode instead.
func (e *ArtifactError) Name() string { return "ArtifactError" }

// sdkError returns the embedded SDKError (nil for a nil e), for WithCause.
func (e *ArtifactError) sdkError() *SDKError {
	if e == nil {
		return nil
	}
	return &e.SDKError
}

// Unwrap returns the embedded SDKError so errors.As to *SDKError works; the
// SDKError unwraps in turn to the cause (WithCause).
func (e *ArtifactError) Unwrap() error {
	if e == nil {
		return nil
	}
	return &e.SDKError
}

// Is matches a *ArtifactError target with an empty Code (class level) or equal Code.
func (e *ArtifactError) Is(target error) bool {
	t, ok := target.(*ArtifactError)
	return e != nil && ok && t != nil && (t.Code == "" || t.Code == e.Code)
}
