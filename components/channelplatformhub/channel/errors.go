package channel

import "github.com/Phong95/aec-agent-go/kernel"

// ErrorCode is the failure category of an Error.
type ErrorCode string

// The Error codes. The string values are stable wire values, the codes of core/ts
// ChannelError.
const (
	// CodeNotFound: no channel with that id for this agent, or no pairing with that code.
	CodeNotFound ErrorCode = "not_found"
	// CodeInvalidArgument: a malformed name, credentials or access settings.
	CodeInvalidArgument ErrorCode = "invalid_argument"
	// CodeUnknownPlatform: no Platform with that name is registered.
	CodeUnknownPlatform ErrorCode = "unknown_platform"
	// CodeConnectFailed: the platform rejected the credentials or could not be reached.
	CodeConnectFailed ErrorCode = "connect_failed"
	// CodeNotRunning: the channel is not connected, so nothing can be sent through it.
	CodeNotRunning ErrorCode = "not_running"
	// CodeLimitReached: the agent already has MaxChannelsPerAgent channels.
	CodeLimitReached ErrorCode = "limit_reached"
)

// Error is a failure of a channel operation (the ChannelError of core/ts). Only
// CodeConnectFailed and CodeNotRunning may succeed on a later retry.
type Error struct {
	kernel.SDKError
	Code ErrorCode
	// ChannelID is the channel the failure is about, when one was resolved ("" otherwise).
	ChannelID string
}

// NewError creates an Error.
func NewError(message string, code ErrorCode, channelID string) *Error {
	retryable := code == CodeConnectFailed || code == CodeNotRunning
	return &Error{SDKError: kernel.SDKError{Message: message, Retryable: retryable}, Code: code, ChannelID: channelID}
}

// Name returns the class name ("ChannelError"), the name core/ts uses.
func (e *Error) Name() string { return "ChannelError" }

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
