package agentloop

import (
	"context"
	"errors"

	"github.com/Phong95/aec-agent-go/kernel"
)

// TokenBudget is the token budget handed to the context middlewares on every
// call. It is advice for a middleware, not a
// limit the loop enforces.
const TokenBudget = 100_000

// RunOptions is the per-run control of Loop.Run and Loop.Stream. Cancellation goes
// through the ctx parameter: cancelling the ctx stops the run at the next safe
// point. The struct stays for future fields.
type RunOptions struct {
	// Conversation (A-97) is the conversation this run works in: one session, loaded
	// for this turn and never shared with a run of another session. Nil uses
	// Deps.Conversation. The caller makes sure no other run of the same session
	// writes to the session at the same time (the agent's per-session turn gate).
	Conversation Conversation
	// TraceAttributes (A-105) are extra attributes for this run's root "agent-run" span - a
	// child run puts its parent turn there (parentSessionId, parentRunId...), so the
	// parent's trace view can show the child's trace too. They come first, in sorted key
	// order; agentId and sessionId always name this run's own agent and session. Nil means
	// none.
	TraceAttributes map[string]any
	// Attachments (A-165) are images the user attached to this turn's message
	// (kernel.Message.Attachments). Checked with kernel.CheckAttachments before the run
	// writes anything; invalid ones fail the run with a ModelError "invalid_request". The
	// loop does not check that the model takes images: the provider rejects them then (a
	// host checks llmhub.ModelAcceptsImages first).
	Attachments []kernel.MessageAttachment
}

// RunResult is the outcome of Loop.Run.
type RunResult struct {
	// Message is the final assistant message. For an aborted run it is an empty
	// assistant message, and at the iteration cap it is the fixed fallback text.
	Message kernel.Message
	// Usage is the total token usage of the run: the sum of this run's model calls
	// only; it is not seeded from the session's current usage.
	Usage kernel.TokenUsage
	// Iterations is the number of iterations the run started (the cap when the
	// run ran out of iterations).
	Iterations int
	// Aborted is true when the run was stopped through ctx instead of ending
	// naturally (A-65).
	Aborted bool
	// StopReason is the stop reason of the run's last model response: end_turn,
	// max_tokens or error for a turn the model ended, tool_use at the iteration cap,
	// and empty when no response arrived (a run stopped before the first answer).
	// It is also max_tokens when the consecutive-truncation guard stopped the run
	// (A-88): responses were cut off at the output token limit while writing tool
	// calls too many times in a row, and Message is the guard's fallback text.
	StopReason kernel.StopReason
}

// MiddlewareContext is what the loop hands a context middleware on every call.
type MiddlewareContext struct {
	// Session is a snapshot of the active session with the usage recorded so far
	// in this run. A middleware may keep or change it freely.
	Session kernel.Session
	// TokenBudget is TokenBudget, the budget a middleware may plan against.
	TokenBudget int
	// Usage is the run's total token usage so far.
	Usage kernel.TokenUsage
}

// Middleware is a context middleware. It is a plugin with a
// priority; the Context port runs them in ascending priority order. A middleware
// takes part by also implementing BeforeModelMiddleware, AfterModelMiddleware or
// both, the two optional hooks beforeModel and afterModel.
type Middleware interface {
	kernel.Plugin
	// Priority orders the middlewares: the lowest runs first.
	Priority() int
}

// BeforeModelMiddleware is the optional beforeModel hook: it rewrites the messages
// about to be sent to the model.
type BeforeModelMiddleware interface {
	BeforeModel(ctx context.Context, msgs []kernel.Message, mc MiddlewareContext) ([]kernel.Message, error)
}

// AfterModelMiddleware is the optional afterModel hook: it rewrites the model's
// response. Run and Stream both apply it; Stream applies it to the collected response,
// after its chunks were yielded, so it changes what is saved and executed, not
// what the consumer already saw.
type AfterModelMiddleware interface {
	AfterModel(ctx context.Context, resp kernel.ModelResponse, mc MiddlewareContext) (kernel.ModelResponse, error)
}

// ErrNoActiveSession is returned by Run when the conversation has no active
// session. Its text is "No active session".
var ErrNoActiveSession error = noActiveSessionError{}

// noActiveSessionError is the type of ErrNoActiveSession. The text is capitalised,
// which a plain errors.New literal would not be allowed to be (staticcheck ST1005);
// tool.hubError is the precedent for a typed error that carries a capitalised
// text.
type noActiveSessionError struct{}

func (noActiveSessionError) Error() string { return "No active session" }

// ErrSessionNotActive is matched, with errors.Is, by the error Run and Stream
// return when the sessionID they were given is not the id of the conversation's
// active session (Phase 9 review IN-05). The check happens before anything is
// persisted or emitted, so the events and the tools can never disagree silently
// about which session they act on. The text of the returned error names both ids.
var ErrSessionNotActive = errors.New("agentloop: the session is not the active session")

// sessionNotActiveError is the error beginRun returns for a mismatch: it carries
// both ids and unwraps to ErrSessionNotActive.
type sessionNotActiveError struct{ given, active string }

func (e *sessionNotActiveError) Error() string {
	return "agentloop: session \"" + e.given + "\" is not the active session \"" + e.active + "\""
}

func (*sessionNotActiveError) Unwrap() error { return ErrSessionNotActive }

// CompactorResult is the outcome of Compactor.CompactIfNeeded.
type CompactorResult struct {
	// Compacted is true when the conversation was shortened.
	Compacted bool
	// Messages is the message list to use: the compacted one, or the input
	// unchanged when nothing was compacted.
	Messages []kernel.Message
	// SummarySource is the text of the messages that were summarised, joined by
	// newlines; empty when nothing was compacted.
	SummarySource string
	// TailTokens is the token count of the kept tail; zero when nothing was
	// compacted.
	TailTokens int
	// SummaryIndex is the index of the summary message in Messages. It is
	// meaningful only when Compacted is true.
	SummaryIndex int
}

// CountFunc counts the tokens of one message (the callback the Compactor takes).
type CountFunc func(kernel.Message) int

// SummarizeFunc turns the messages to compact into one summary text. model is the
// optional summarization model, empty for the agent's default.
// A-136: instructions is what the user asked the summary to focus on (/compact <focus>),
// empty for none.
type SummarizeFunc func(ctx context.Context, msgs []kernel.Message, maxOutputTokens int, model, instructions string) (string, error)
