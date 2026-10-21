package kernel

import (
	"errors"
	"time"
)

// This file holds the SDK error hierarchy (the base SDKError and the model,
// tool, policy, session and skill errors; the sandbox and artifact errors with
// code-derived retryability are in errors_derived.go).
//
// Go shape of the hierarchy:
//
//   - Every class is a struct that embeds SDKError and adds a typed Code plus
//     the class's extra field (ToolName, SkillName, ...). Use pointers (*ModelError).
//   - errors.As(err, &sdk) with sdk of type *SDKError matches every class,
//     because each class's Unwrap returns its embedded SDKError.
//   - errors.As(err, &modelErr) matches the concrete class.
//   - errors.Is(err, &ModelError{}) matches at class level (an empty Code in the
//     target is a wildcard); errors.Is(err, &ModelError{Code: ModelCodeAuth})
//     matches the exact code.
//   - errors.Is(err, &SDKError{}) (an SDKError target with an empty Message) is
//     the class-level wildcard of the base: it matches every SDK error. Any other *SDKError target matches only itself.
//   - Constructors always return a concrete, non-nil pointer. Beware of the
//     typed-nil trap: a function returning error must return a literal nil, never
//     a nil *ModelError, because an interface holding a nil pointer is non-nil.
//     Calling Error() on a nil class pointer panics.
//
// Matching a code: HasCode is the one helper for every class.
// kernel.HasCode(err, kernel.ModelCodeAuth) is exactly
// errors.Is(err, &kernel.ModelError{Code: kernel.ModelCodeAuth}): the code type
// picks the class, so a ToolCodeTimeout never matches a ModelError. For the model
// codes, which callers test most, the sentinels ErrModelAuth, ErrModelRateLimit,
// ErrModelTimeout, ErrModelInvalidRequest and ErrModelProvider are the same test in
// the errors.Is form: errors.Is(err, kernel.ErrModelRateLimit). The sentinels are
// match targets only; never return or modify one.
//
// Cause chain: every class can carry the lower-level error that made it (a
// transport *url.Error, a context.DeadlineExceeded, the vendor SDK's error), set
// with WithCause on a freshly built error. The cause is never part of Error(), so
// the message stays exactly what the constructor was given; it is reached through
// Unwrap, so errors.Is(err, context.DeadlineExceeded) and errors.As(err, &urlErr)
// see through an SDK error. The chain is class -> embedded SDKError -> cause. A
// cause's own Error() text is not credential-safe (a vendor error can embed the
// request URL with its query): log the SDK error's Error(), not its cause's.
//
// Name methods return the class names. They exist for wire fixtures and logs
// that carry the error name; Go code should match with errors.As, errors.Is or
// HasCode instead.

// SDKError is the base error for all SDK errors. Every SDK error embeds it so
// consumers can match broadly (errors.As to *SDKError) or narrowly.
type SDKError struct {
	Message string
	// Retryable says whether retrying the same call may succeed.
	Retryable bool
	// cause is the lower-level error behind this one (WithCause), nil when there
	// is none. It is unexported so it never appears in any encoded output, and it
	// is never part of Error().
	cause error
}

// NewSDKError creates a base SDK error with the given retryability.
func NewSDKError(message string, retryable bool) *SDKError {
	return &SDKError{Message: message, Retryable: retryable}
}

// Error returns the message.
func (e *SDKError) Error() string { return e.Message }

// Name returns the class name ("SDKError"), the stable name used by wire fixtures and
// logs. Go code should match with errors.As or errors.Is instead.
func (e *SDKError) Name() string { return "SDKError" }

// Unwrap returns the cause set with WithCause (nil when there is none). Every
// class unwraps to its embedded SDKError, so the cause is reached from each one.
func (e *SDKError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

// sdkError returns the receiver; with the per-class methods it lets WithCause
// reach the embedded SDKError of any class.
func (e *SDKError) sdkError() *SDKError { return e }

// ErrorClass is the constraint of WithCause: it is satisfied by *SDKError and by
// the pointer of every error class, and by nothing outside this package (its
// method is unexported).
type ErrorClass interface {
	error
	sdkError() *SDKError
}

// WithCause sets cause as the lower-level error behind e and returns e, so it
// wraps a constructor call:
//
//	return kernel.WithCause(kernel.NewModelError("Connection error.", kernel.ModelCodeProvider, true), err)
//
// It changes e in place: use it on an error just built, before it is returned or
// shared. Error() is unchanged (the cause is never part of the text); errors.Is
// and errors.As reach the cause through Unwrap. A nil e is returned unchanged, and
// a nil cause clears it.
func WithCause[E ErrorClass](e E, cause error) E {
	if base := e.sdkError(); base != nil {
		base.cause = cause
	}
	return e
}

// Is matches a target *SDKError with an empty Message at class level, so
// errors.Is(err, &kernel.SDKError{}) is true for every SDK error (each class
// unwraps to its embedded SDKError); any other target matches only by identity.
func (e *SDKError) Is(target error) bool {
	t, ok := target.(*SDKError)
	return e != nil && ok && t != nil && (t == e || t.Message == "")
}

// IsRetryable reports whether err is (or wraps) an SDKError with Retryable set.
// It is the one retry test for every class.
func IsRetryable(err error) bool {
	var sdk *SDKError
	return errors.As(err, &sdk) && sdk != nil && sdk.Retryable
}

// ModelErrorCode is the failure category of a ModelError.
type ModelErrorCode string

// The ModelError codes. The string values are stable wire values.
const (
	ModelCodeAuth           ModelErrorCode = "auth"
	ModelCodeRateLimit      ModelErrorCode = "rate_limit"
	ModelCodeTimeout        ModelErrorCode = "timeout"
	ModelCodeInvalidRequest ModelErrorCode = "invalid_request"
	ModelCodeProvider       ModelErrorCode = "provider"
)

// ModelError is a failure of an LLM provider call. Retryability is decided by
// the caller, which always passes it.
//
// StatusCode and RetryAfter are filled by the providers from the
// HTTP response; they are never encoded (json:"-") and never part of Error().
type ModelError struct {
	SDKError
	Code ModelErrorCode
	// StatusCode is the HTTP status of the provider response that failed (401,
	// 429, 503, ...), 0 when the failure had no error status (a connection
	// failure, an error event inside a stream, a request that was never sent).
	StatusCode int `json:"-"`
	// RetryAfter is the wait the provider asked for in its Retry-After (or
	// retry-after-ms) response header, 0 when it sent none. It is advice for a
	// caller that retries; the SDK itself does not sleep on it.
	RetryAfter time.Duration `json:"-"`
}

// NewModelError creates a ModelError.
func NewModelError(message string, code ModelErrorCode, retryable bool) *ModelError {
	return &ModelError{SDKError: SDKError{Message: message, Retryable: retryable}, Code: code}
}

// Name returns the class name ("ModelError"), the stable name used by wire fixtures and
// logs. Go code should match with errors.As, errors.Is or HasCode instead.
func (e *ModelError) Name() string { return "ModelError" }

// sdkError returns the embedded SDKError (nil for a nil e), for WithCause.
func (e *ModelError) sdkError() *SDKError {
	if e == nil {
		return nil
	}
	return &e.SDKError
}

// Unwrap returns the embedded SDKError so errors.As to *SDKError works; the
// SDKError unwraps in turn to the cause (WithCause).
func (e *ModelError) Unwrap() error {
	if e == nil {
		return nil
	}
	return &e.SDKError
}

// Is matches a *ModelError target with an empty Code (class level) or equal Code.
func (e *ModelError) Is(target error) bool {
	t, ok := target.(*ModelError)
	return e != nil && ok && t != nil && (t.Code == "" || t.Code == e.Code)
}

// The model-code sentinels: errors.Is(err, ErrModelRateLimit) is true when err
// is (or wraps) a *ModelError with that code, the same test as
// HasCode(err, ModelCodeRateLimit). They are match targets only: never return
// one as a failure (it has no real message) and never modify one. They are typed
// error so WithCause cannot be applied to them.
var (
	ErrModelAuth           error = modelSentinel(ModelCodeAuth)
	ErrModelRateLimit      error = modelSentinel(ModelCodeRateLimit)
	ErrModelTimeout        error = modelSentinel(ModelCodeTimeout)
	ErrModelInvalidRequest error = modelSentinel(ModelCodeInvalidRequest)
	ErrModelProvider       error = modelSentinel(ModelCodeProvider)
)

// modelSentinel builds the match target of one model code.
func modelSentinel(code ModelErrorCode) *ModelError {
	return &ModelError{SDKError: SDKError{Message: "model error: " + string(code)}, Code: code}
}

// ToolErrorCode is the failure category of a ToolError.
type ToolErrorCode string

// The ToolError codes. The string values are stable wire values.
const (
	ToolCodeNotFound        ToolErrorCode = "not_found"
	ToolCodeNotAllowed      ToolErrorCode = "not_allowed"
	ToolCodeInvalidArgs     ToolErrorCode = "invalid_args"
	ToolCodeExecutionFailed ToolErrorCode = "execution_failed"
	ToolCodeTimeout         ToolErrorCode = "timeout"
)

// ToolError is a failure of a tool call.
type ToolError struct {
	SDKError
	Code     ToolErrorCode
	ToolName string
}

// NewToolError creates a ToolError with the given retryability.
func NewToolError(message string, code ToolErrorCode, toolName string, retryable bool) *ToolError {
	return &ToolError{SDKError: SDKError{Message: message, Retryable: retryable}, Code: code, ToolName: toolName}
}

// Name returns the class name ("ToolError"), the stable name used by wire fixtures and
// logs. Go code should match with errors.As, errors.Is or HasCode instead.
func (e *ToolError) Name() string { return "ToolError" }

// sdkError returns the embedded SDKError (nil for a nil e), for WithCause.
func (e *ToolError) sdkError() *SDKError {
	if e == nil {
		return nil
	}
	return &e.SDKError
}

// Unwrap returns the embedded SDKError so errors.As to *SDKError works; the
// SDKError unwraps in turn to the cause (WithCause).
func (e *ToolError) Unwrap() error {
	if e == nil {
		return nil
	}
	return &e.SDKError
}

// Is matches a *ToolError target with an empty Code (class level) or equal Code.
func (e *ToolError) Is(target error) bool {
	t, ok := target.(*ToolError)
	return e != nil && ok && t != nil && (t.Code == "" || t.Code == e.Code)
}

// PolicyErrorCode is the failure category of a PolicyError.
type PolicyErrorCode string

// The PolicyError codes. The string values are stable wire values.
const (
	PolicyCodeNotInAllowlist      PolicyErrorCode = "not_in_allowlist"
	PolicyCodeInvalidArgs         PolicyErrorCode = "invalid_args"
	PolicyCodeAuthorizationDenied PolicyErrorCode = "authorization_denied"
)

// PolicyError is a tool call rejected by policy. It is never retryable.
type PolicyError struct {
	SDKError
	Code     PolicyErrorCode
	ToolName string
}

// NewPolicyError creates a PolicyError (always non-retryable).
func NewPolicyError(message string, code PolicyErrorCode, toolName string) *PolicyError {
	return &PolicyError{SDKError: SDKError{Message: message, Retryable: false}, Code: code, ToolName: toolName}
}

// Name returns the class name ("PolicyError"), the stable name used by wire fixtures and
// logs. Go code should match with errors.As, errors.Is or HasCode instead.
func (e *PolicyError) Name() string { return "PolicyError" }

// sdkError returns the embedded SDKError (nil for a nil e), for WithCause.
func (e *PolicyError) sdkError() *SDKError {
	if e == nil {
		return nil
	}
	return &e.SDKError
}

// Unwrap returns the embedded SDKError so errors.As to *SDKError works; the
// SDKError unwraps in turn to the cause (WithCause).
func (e *PolicyError) Unwrap() error {
	if e == nil {
		return nil
	}
	return &e.SDKError
}

// Is matches a *PolicyError target with an empty Code (class level) or equal Code.
func (e *PolicyError) Is(target error) bool {
	t, ok := target.(*PolicyError)
	return e != nil && ok && t != nil && (t.Code == "" || t.Code == e.Code)
}

// SessionErrorCode is the failure category of a SessionError.
type SessionErrorCode string

// The SessionError codes. The string values are stable wire values.
const (
	SessionCodeNotFound         SessionErrorCode = "not_found"
	SessionCodeAlreadyExists    SessionErrorCode = "already_exists"
	SessionCodeStoreUnavailable SessionErrorCode = "store_unavailable"
	SessionCodeCorrupted        SessionErrorCode = "corrupted"
	// SessionCodeNotAllowed (A-135): the session can't take that setting (YOLO mode in
	// a job's session).
	SessionCodeNotAllowed SessionErrorCode = "not_allowed"
	// SessionCodeBusy (A-136): the session is busy with a running turn and the action
	// can't wait for it (/compact).
	SessionCodeBusy SessionErrorCode = "busy"
)

// SessionError is a failure of a session or its store.
type SessionError struct {
	SDKError
	Code SessionErrorCode
}

// NewSessionError creates a SessionError with the given retryability.
func NewSessionError(message string, code SessionErrorCode, retryable bool) *SessionError {
	return &SessionError{SDKError: SDKError{Message: message, Retryable: retryable}, Code: code}
}

// Name returns the class name ("SessionError"), the stable name used by wire fixtures and
// logs. Go code should match with errors.As, errors.Is or HasCode instead.
func (e *SessionError) Name() string { return "SessionError" }

// sdkError returns the embedded SDKError (nil for a nil e), for WithCause.
func (e *SessionError) sdkError() *SDKError {
	if e == nil {
		return nil
	}
	return &e.SDKError
}

// Unwrap returns the embedded SDKError so errors.As to *SDKError works; the
// SDKError unwraps in turn to the cause (WithCause).
func (e *SessionError) Unwrap() error {
	if e == nil {
		return nil
	}
	return &e.SDKError
}

// Is matches a *SessionError target with an empty Code (class level) or equal Code.
func (e *SessionError) Is(target error) bool {
	t, ok := target.(*SessionError)
	return e != nil && ok && t != nil && (t.Code == "" || t.Code == e.Code)
}

// SkillErrorCode is the failure category of a SkillError.
type SkillErrorCode string

// The SkillError codes. The string values are stable wire values.
const (
	SkillCodeNotFound   SkillErrorCode = "not_found"
	SkillCodeNotAllowed SkillErrorCode = "not_allowed"
	// SkillCodeInvalid: SKILL.md or a file breaks the format rules; the message lists every problem.
	SkillCodeInvalid SkillErrorCode = "invalid"
	// SkillCodeAlreadyExists: a skill with this name is already in the library.
	SkillCodeAlreadyExists SkillErrorCode = "already_exists"
	// SkillCodeReadOnly: built-in and app skills cannot be changed at runtime.
	SkillCodeReadOnly SkillErrorCode = "read_only"
)

// SkillError is a failure of a skill operation.
type SkillError struct {
	SDKError
	Code      SkillErrorCode
	SkillName string
}

// NewSkillError creates a SkillError with the given retryability.
func NewSkillError(message string, code SkillErrorCode, skillName string, retryable bool) *SkillError {
	return &SkillError{SDKError: SDKError{Message: message, Retryable: retryable}, Code: code, SkillName: skillName}
}

// Name returns the class name ("SkillError"), the stable name used by wire fixtures and
// logs. Go code should match with errors.As, errors.Is or HasCode instead.
func (e *SkillError) Name() string { return "SkillError" }

// sdkError returns the embedded SDKError (nil for a nil e), for WithCause.
func (e *SkillError) sdkError() *SDKError {
	if e == nil {
		return nil
	}
	return &e.SDKError
}

// Unwrap returns the embedded SDKError so errors.As to *SDKError works; the
// SDKError unwraps in turn to the cause (WithCause).
func (e *SkillError) Unwrap() error {
	if e == nil {
		return nil
	}
	return &e.SDKError
}

// Is matches a *SkillError target with an empty Code (class level) or equal Code.
func (e *SkillError) Is(target error) bool {
	t, ok := target.(*SkillError)
	return e != nil && ok && t != nil && (t.Code == "" || t.Code == e.Code)
}

// ErrorCode is the set of the code types of the error classes; it is the
// constraint of HasCode. The exact types are listed (no ~), so the code type
// alone decides the class, and an untyped string constant does not compile.
type ErrorCode interface {
	ModelErrorCode | ToolErrorCode | PolicyErrorCode | SessionErrorCode | SkillErrorCode | SandboxErrorCode | ArtifactErrorCode
}

// HasCode reports whether err is (or wraps, through %w or a cause) an SDK error of
// the class that owns the type of code, with that code:
//
//	kernel.HasCode(err, kernel.ModelCodeAuth)   // a *ModelError with code auth
//	kernel.HasCode(err, kernel.ToolCodeTimeout) // a *ToolError with code timeout
//
// It is exactly errors.Is(err, &Class{Code: code}), so an empty code matches any
// error of the class, as the errors.Is class-level target does.
func HasCode[C ErrorCode](err error, code C) bool {
	var target error
	switch c := any(code).(type) {
	case ModelErrorCode:
		target = &ModelError{Code: c}
	case ToolErrorCode:
		target = &ToolError{Code: c}
	case PolicyErrorCode:
		target = &PolicyError{Code: c}
	case SessionErrorCode:
		target = &SessionError{Code: c}
	case SkillErrorCode:
		target = &SkillError{Code: c}
	case SandboxErrorCode:
		target = &SandboxError{Code: c}
	case ArtifactErrorCode:
		target = &ArtifactError{Code: c}
	default:
		return false
	}
	return errors.Is(err, target)
}
