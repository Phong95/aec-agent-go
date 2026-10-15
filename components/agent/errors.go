package agent

import (
	"errors"
	"strconv"

	"github.com/Phong95/aec-agent-go/components/agent/agentloop"
	"github.com/Phong95/aec-agent-go/components/toolhub"
)

// ErrNoActiveSession is returned by Chat and ChatStream when the agent has no
// active session. Its text is "No active session", an em dash, then "call
// startSession or loadSession first".
// (A-97: when the call names no session with WithChatSession and no default session
// was set by StartSession or LoadSession.)
//
// It is a different value from agentloop.ErrNoActiveSession, whose text is the
// shorter "No active session" of the loop and of the conversation writes, but
// errors.Is(err, agentloop.ErrNoActiveSession) is true for it, so a caller that
// matches the loop's sentinel sees both.
var ErrNoActiveSession error = noActiveSessionError{}

// noActiveSessionError is the type of ErrNoActiveSession. The text is capitalised
// as a sentence, which a plain errors.New literal would not be allowed to be
// (staticcheck ST1005); agentloop.noActiveSessionError is the precedent for a typed
// error that carries a capitalised text. The source stays ASCII, so the em dash is
// written as an escape.
type noActiveSessionError struct{}

func (noActiveSessionError) Error() string {
	return "No active session \u2014 call startSession or loadSession first"
}

// Unwrap makes errors.Is(err, agentloop.ErrNoActiveSession) true.
func (noActiveSessionError) Unwrap() error { return agentloop.ErrNoActiveSession }

// notAvailableError carries the text of a tool the agent may not use and
// unwraps to toolhub.ErrNotFound, so errors.Is(err, toolhub.ErrNotFound) holds as it does
// for the hub's own error.
type notAvailableError struct{ name string }

func (e *notAvailableError) Error() string {
	return `Tool "` + e.name + `" not available for this agent`
}

func (*notAvailableError) Unwrap() error { return toolhub.ErrNotFound }

// ErrAgentDisposed is returned by Chat and ChatStream once Dispose has started. A
// chat that was waiting for its turn when Dispose started returns it too; a chat that
// was running is cancelled by Dispose and ends with an Aborted result, as any
// cancelled run does. (A-97: in every session of the agent.)
var ErrAgentDisposed = errors.New("agent: the agent was disposed")

// ErrReentrantChat is returned by Chat and ChatStream when they are called with the
// ctx of a chat of the same agent that is still running (for example from a tool of
// that chat). Chats of one agent are serialized, so the nested chat would wait for
// the one that called it for ever.
//
// -> Superseded (A-97): chats are serialized per session, so it is returned only for a
// nested chat on the SAME session as a running chat of its ctx chain (that one would
// wait for itself). A nested chat on another session of the agent runs.
var ErrReentrantChat = errors.New("agent: chat called from inside a chat of the same agent")

// ErrSessionNotFound is matched, with errors.Is, by the error Chat and ChatStream return
// when the session the turn runs in (WithChatSession, or the default session) is not in
// the session store (A-97). Nothing is written then. The text of the returned error names
// the session id.
var ErrSessionNotFound = errors.New("agent: session not found")

// sessionNotFoundError carries the id of the missing session and unwraps to
// ErrSessionNotFound.
type sessionNotFoundError struct{ id string }

func (e *sessionNotFoundError) Error() string {
	return "agent: session " + strconv.Quote(e.id) + " not found"
}

func (*sessionNotFoundError) Unwrap() error { return ErrSessionNotFound }
