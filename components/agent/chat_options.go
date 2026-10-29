package agent

import (
	"maps"
	"slices"

	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/kernel"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// callOptions are the settings a ChatOption or a CompleteOption changes.
type callOptions struct {
	model llmhub.ModelOptions
	// session is the session whose model override Complete applies; nil means none.
	session *kernel.Session
	// sessionID is the session a Chat or ChatStream runs in (A-97, WithChatSession);
	// empty means the agent's default session.
	sessionID string
	// traceAttributes are the extra attributes of the turn's "agent-run" span (A-105,
	// WithChatTraceAttributes); nil means none.
	traceAttributes map[string]any
	// readOnly runs the turn in plan mode (A-136, WithChatReadOnly).
	readOnly bool
	// attachments are the images attached to the turn's message (A-165, WithChatAttachments).
	attachments []kernel.MessageAttachment
}

// ChatOption is a per-call option of Agent.Chat and Agent.ChatStream. Every
// ModelOption is one. Options are applied in order, so a later one wins.
// (A-97: WithChatSession is one too.)
type ChatOption interface {
	applyChat(*callOptions)
}

// CompleteOption is a per-call option of Agent.Complete: every ModelOption, and
// WithCompleteSession. Options are applied in order, so a later one wins.
type CompleteOption interface {
	applyComplete(*callOptions)
}

// ModelOption is a per-call model override. It is both a ChatOption and a
// CompleteOption. The profile's inference config is the base layer and the model
// options of the call go on top; the session's model applies when the merged
// options name no model.
type ModelOption func(*llmhub.ModelOptions)

func (f ModelOption) applyChat(o *callOptions) {
	if f != nil {
		f(&o.model)
	}
}

func (f ModelOption) applyComplete(o *callOptions) {
	if f != nil {
		f(&o.model)
	}
}

// WithModel sets the model id of the call; "" keeps the agent's default.
func WithModel(model string) ModelOption {
	return func(o *llmhub.ModelOptions) { o.Model = model }
}

// WithProvider sets the registered provider the call goes to; "" keeps the agent's
// default provider.
func WithProvider(name string) ModelOption {
	return func(o *llmhub.ModelOptions) { o.Provider = name }
}

// WithTemperature sets the sampling temperature of the call. An explicit 0 is kept.
func WithTemperature(t float64) ModelOption {
	return func(o *llmhub.ModelOptions) { o.Temperature = llmhub.Float(t) }
}

// WithMaxTokens sets the output token limit of the call.
func WithMaxTokens(n int) ModelOption {
	return func(o *llmhub.ModelOptions) { o.MaxTokens = llmhub.Int(n) }
}

// WithTopP sets the nucleus sampling value of the call. An explicit 0 is kept.
func WithTopP(p float64) ModelOption {
	return func(o *llmhub.ModelOptions) { o.TopP = llmhub.Float(p) }
}

// WithReasoningEffort (A-164) sets the reasoning effort level of the call, over the
// session's.
func WithReasoningEffort(effort string) ModelOption {
	return func(o *llmhub.ModelOptions) { o.ReasoningEffort = effort }
}

// WithStopSequences sets the stop sequences of the call; the slice is copied. An
// empty list keeps the profile's.
func WithStopSequences(seqs ...string) ModelOption {
	seqs = slices.Clone(seqs)
	return func(o *llmhub.ModelOptions) { o.StopSequences = slices.Clone(seqs) }
}

// WithModelOptions is the advanced escape hatch: it replaces the model options set
// so far by the earlier options of the call with a copy of opts (the options after
// it apply on top). Every field is passed to the loop as it is, so it can do what
// the narrower options cannot: a non-nil Tools replaces the tool definitions the
// agent would send (even an empty one), and a SystemPrompt reaches the provider of
// Complete. Prefer WithModel, WithTemperature and the like.
func WithModelOptions(opts llmhub.ModelOptions) ModelOption {
	opts = cloneModelOptions(opts)
	return func(o *llmhub.ModelOptions) { *o = cloneModelOptions(opts) }
}

// completeSessionOption is the CompleteOption of WithCompleteSession.
type completeSessionOption struct{ session kernel.Session }

func (s completeSessionOption) applyComplete(o *callOptions) {
	session := s.session
	o.session = &session
}

// WithCompleteSession makes Complete apply the model override of session, as Chat
// does for the active session. The session does not have to be the active one.
func WithCompleteSession(session kernel.Session) CompleteOption {
	return completeSessionOption{session: session}
}

// chatSessionOption is the ChatOption of WithChatSession (A-97).
type chatSessionOption struct{ id string }

func (s chatSessionOption) applyChat(o *callOptions) { o.sessionID = s.id }

// WithChatSession makes Chat or ChatStream run in the session sessionID (A-97) instead
// of the agent's default session (the one StartSession or LoadSession set last).
// Turns in different sessions of one agent run in parallel; turns in the same session
// run one at a time, in arrival order. The session does not have to be loaded first:
// the turn reads it from the stores when it starts, and an unknown id fails the turn
// with an error that matches ErrSessionNotFound before anything is written. "" keeps
// the default session.
func WithChatSession(sessionID string) ChatOption {
	return chatSessionOption{id: sessionID}
}

// chatTraceOption is the ChatOption of WithChatTraceAttributes (A-105).
type chatTraceOption struct{ attrs map[string]any }

func (t chatTraceOption) applyChat(o *callOptions) { o.traceAttributes = maps.Clone(t.attrs) }

// WithChatTraceAttributes puts extra attributes on the turn's root "agent-run" span
// (A-105): a child run (a subagent's turn) puts its parent turn there (parentSessionId,
// parentRunId...), so the parent's trace view can show the child's trace too. agentId
// and sessionId always name the turn's own agent and session. attrs is copied; nil
// means none.
func WithChatTraceAttributes(attrs map[string]any) ChatOption {
	return chatTraceOption{attrs: maps.Clone(attrs)}
}

// chatReadOnlyOption is the ChatOption of WithChatReadOnly (A-136).
type chatReadOnlyOption struct{}

func (chatReadOnlyOption) applyChat(o *callOptions) { o.readOnly = true }

// WithChatReadOnly runs the turn in plan mode (A-136): only read-only tool calls run,
// whatever the session's approval mode - /review reviews without changing anything. Only
// this turn; the session's mode stays as it was. (core/ts: RunOptions.readOnly.)
func WithChatReadOnly() ChatOption { return chatReadOnlyOption{} }

// chatAttachmentsOption is the ChatOption of WithChatAttachments (A-165).
type chatAttachmentsOption struct{ attachments []kernel.MessageAttachment }

func (a chatAttachmentsOption) applyChat(o *callOptions) {
	o.attachments = slices.Clone(a.attachments)
}

// WithChatAttachments (A-165) attaches images to the turn's user message; the slice is
// copied. They are checked with kernel.CheckAttachments when the turn starts. (core/ts:
// RunOptions.attachments.)
func WithChatAttachments(attachments ...kernel.MessageAttachment) ChatOption {
	return chatAttachmentsOption{attachments: slices.Clone(attachments)}
}

// chatOptions applies opts in order. A nil option is skipped.
func chatOptions(opts []ChatOption) callOptions {
	var o callOptions
	for _, opt := range opts {
		if opt != nil {
			opt.applyChat(&o)
		}
	}
	return o
}

// completeOptions applies opts in order. A nil option is skipped.
func completeOptions(opts []CompleteOption) callOptions {
	var o callOptions
	for _, opt := range opts {
		if opt != nil {
			opt.applyComplete(&o)
		}
	}
	return o
}

// sessionOptions are the settings a SessionOption changes.
type sessionOptions struct {
	metadata ordered.Object
	model    string
}

// SessionOption is an option of Agent.StartSession. Options are applied in order.
type SessionOption func(*sessionOptions)

// WithMetadata sets the metadata stored with the session; md is stored as given.
func WithMetadata(md ordered.Object) SessionOption {
	return func(o *sessionOptions) { o.metadata = md }
}

// WithSessionModel sets the session's model override, which Chat and Complete apply
// when the call's options name no model. "" means none.
func WithSessionModel(model string) SessionOption {
	return func(o *sessionOptions) { o.model = model }
}
