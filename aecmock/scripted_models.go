package aecmock

import (
	"context"
	"errors"
	"iter"
	"slices"
	"strings"
	"sync"

	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/kernel"
)

// ScriptedTurn is one scripted model answer (the conformance suite's scripted
// turn, with the extras the loop tests need).
type ScriptedTurn struct {
	// Content is the answer text.
	Content string
	// ToolCalls are the tool calls the answer asks for.
	ToolCalls []kernel.ToolCall
	// StopReason is why the answer ended. Empty means tool_use when the turn has
	// tool calls and end_turn otherwise. Stream puts it on the done chunk of the
	// default chunks only when it is set (A-88), as a provider reports the stop
	// reason the model gave.
	StopReason kernel.StopReason
	// Usage is the usage of the answer. Nil means the conformance stub numbers,
	// 10 input and 5 output tokens, on Chat; Stream sends usage only when set.
	Usage *kernel.TokenUsage
	// Chunks, when set, are streamed verbatim instead of the default chunks built
	// from Content and ToolCalls. The script is then responsible for ending with
	// a done chunk when the test needs one.
	Chunks []kernel.StreamChunk
	// Err, when set, makes the turn fail: Chat returns it, and Stream yields one
	// terminal error chunk carrying its text (the in-band failure contract of
	// llmhub.Provider).
	Err error
}

// ScriptedModels is a deterministic model for the agent loop tests: it answers
// the n-th call with the n-th scripted turn (the scripted model of the agent-loop
// conformance suite). It satisfies agentloop.Models, and also the two
// optional tracing interfaces agentloop.OptionResolver and agentloop.ProviderNamer:
// ResolveOptions returns its argument unchanged and ResolveProviderName always
// fails, like the harness stubs, so no provider attribute is recorded.
//
// Stream is a pure iterator that starts no goroutine, so abandoning it cannot
// leak one. It is safe for concurrent use. The zero value is not usable; create
// one with NewScriptedModels.
//
// Chat and Stream both honour ctx, as the llmhub.Provider contract requires: a ctx
// that is done makes Chat return its error before the turn is used, and makes
// Stream yield it once, in the error slot, instead of the next chunk. A test that
// needs a provider that ignores ctx (one that never answers, or answers late)
// wraps the ScriptedModels in a local decorator that overrides Chat or Stream;
// embedding *ScriptedModels keeps the two optional tracing interfaces visible to
// the loop.
type ScriptedModels struct {
	mu    sync.Mutex
	turns []ScriptedTurn
	next  int
	calls []MockCall

	// Set once, by With, before the models are used.
	summary      *string
	summaryErr   error
	resolve      func(llmhub.ModelOptions) llmhub.ModelOptions
	providerName *string
}

// NewScriptedModels returns a ScriptedModels that answers with turns in order.
func NewScriptedModels(turns ...ScriptedTurn) *ScriptedModels {
	return &ScriptedModels{turns: slices.Clone(turns)}
}

// ScriptedOption changes how a ScriptedModels behaves; apply it with With.
type ScriptedOption func(*ScriptedModels)

// With applies the options and returns m, so a test can write
// NewScriptedModels(turns...).With(WithSummaryReply("...")). Call it before the
// models are used: the options are not guarded by the mutex.
func (m *ScriptedModels) With(opts ...ScriptedOption) *ScriptedModels {
	for _, opt := range opts {
		if opt != nil {
			opt(m)
		}
	}
	return m
}

// WithSummaryReply makes a Chat call whose options carry a SystemPrompt answer
// text instead of the next turn: that is the call the compaction summarizer makes
// (the context-trace tests have it answer SUMMARY-TEXT when the options carry a
// SystemPrompt). The reply ends with end_turn and carries the default usage, the call is
// still recorded, and no scripted turn is consumed, so the main conversation keeps
// its script. An empty text is a valid reply (the summarizer then fails on it).
func WithSummaryReply(text string) ScriptedOption {
	return func(m *ScriptedModels) { m.summary = &text }
}

// WithSummaryError makes the summary call fail with err instead of answering, so a
// test can show that a failed summary skips the compaction. It takes precedence
// over WithSummaryReply.
func WithSummaryError(err error) ScriptedOption {
	return func(m *ScriptedModels) {
		empty := ""
		m.summary, m.summaryErr = &empty, err
	}
}

// WithOptionResolver sets what ResolveOptions returns: f is applied to the options
// the loop is about to send, like a model manager merging its defaults, so a
// test can show what the model-call span records as the effective options.
func WithOptionResolver(f func(llmhub.ModelOptions) llmhub.ModelOptions) ScriptedOption {
	return func(m *ScriptedModels) { m.resolve = f }
}

// WithProviderName makes ResolveProviderName answer name instead of failing, so a
// traced call records a provider.
func WithProviderName(name string) ScriptedOption {
	return func(m *ScriptedModels) { m.providerName = &name }
}

// Calls returns the recorded calls in order. The result and everything it holds
// are copies.
func (m *ScriptedModels) Calls() []MockCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]MockCall, len(m.calls))
	for i, c := range m.calls {
		out[i] = MockCall{Messages: cloneMessageSlice(c.Messages), Options: cloneOptions(c.Options)}
	}
	return out
}

// ResolveOptions returns opts unchanged (as the harness stub does), or what
// the WithOptionResolver function makes of them.
func (m *ScriptedModels) ResolveOptions(opts llmhub.ModelOptions) llmhub.ModelOptions {
	if m.resolve != nil {
		return m.resolve(opts)
	}
	return opts
}

// ResolveProviderName fails by default: the scripted model has no provider
// registry, so a traced call records no provider. With WithProviderName it
// answers that name.
func (m *ScriptedModels) ResolveProviderName(string) (string, error) {
	if m.providerName != nil {
		return *m.providerName, nil
	}
	return "", errors.New("aecmock: ScriptedModels has no provider registry")
}

// errScriptExhausted is the failure of a call made after the last turn.
var errScriptExhausted = errors.New("aecmock: ScriptedModels has no more scripted turns")

// record keeps a copy of a call without consuming a turn.
func (m *ScriptedModels) record(msgs []kernel.Message, opts llmhub.ModelOptions) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, MockCall{Messages: cloneMessageSlice(msgs), Options: cloneOptions(opts)})
}

// take records the call and returns its turn.
func (m *ScriptedModels) take(msgs []kernel.Message, opts llmhub.ModelOptions) (ScriptedTurn, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, MockCall{Messages: cloneMessageSlice(msgs), Options: cloneOptions(opts)})
	if m.next >= len(m.turns) {
		return ScriptedTurn{}, errScriptExhausted
	}
	turn := m.turns[m.next]
	m.next++
	return turn, nil
}

// Chat returns the next scripted turn as a model response. A cancelled context
// returns the bare ctx.Err() before the turn is used, as llmhub.Provider requires.
func (m *ScriptedModels) Chat(ctx context.Context, msgs []kernel.Message, opts llmhub.ModelOptions) (kernel.ModelResponse, error) {
	if err := ctx.Err(); err != nil {
		return kernel.ModelResponse{}, err
	}
	if m.summary != nil && opts.SystemPrompt != "" {
		m.record(msgs, opts)
		if m.summaryErr != nil {
			return kernel.ModelResponse{}, m.summaryErr
		}
		return kernel.ModelResponse{
			Message:    kernel.Message{Role: kernel.RoleAssistant, Content: *m.summary},
			Usage:      kernel.TokenUsage{InputTokens: 10, OutputTokens: 5},
			StopReason: kernel.StopEndTurn,
		}, nil
	}
	turn, err := m.take(msgs, opts)
	if err != nil {
		return kernel.ModelResponse{}, err
	}
	if turn.Err != nil {
		return kernel.ModelResponse{}, turn.Err
	}
	usage := kernel.TokenUsage{InputTokens: 10, OutputTokens: 5}
	if turn.Usage != nil {
		usage = cloneTokenUsage(*turn.Usage)
	}
	return kernel.ModelResponse{
		Message: kernel.Message{
			Role:      kernel.RoleAssistant,
			Content:   turn.Content,
			ToolCalls: cloneToolCalls(turn.ToolCalls),
		},
		Usage:      usage,
		StopReason: turn.stopReason(),
	}, nil
}

// stopReason is the turn's stop reason with the documented default.
func (t ScriptedTurn) stopReason() kernel.StopReason {
	switch {
	case t.StopReason != "":
		return t.StopReason
	case len(t.ToolCalls) > 0:
		return kernel.StopToolUse
	default:
		return kernel.StopEndTurn
	}
}

// chunks builds the default chunk list of a turn: the stub shape, each word
// of Content followed by a space as a text chunk, one tool_call chunk per call,
// then a done chunk that carries the usage only when the turn sets one (and the
// StopReason only when the turn sets one, A-88).
func (t ScriptedTurn) chunks() []kernel.StreamChunk {
	if t.Chunks != nil {
		return slices.Clone(t.Chunks)
	}
	var out []kernel.StreamChunk
	if t.Content != "" {
		for word := range strings.SplitSeq(t.Content, " ") {
			text := word + " "
			out = append(out, kernel.StreamChunk{Type: kernel.ChunkText, Content: &text})
		}
	}
	for _, call := range t.ToolCalls {
		c := cloneToolCalls([]kernel.ToolCall{call})[0]
		out = append(out, kernel.StreamChunk{Type: kernel.ChunkToolCall, ToolCall: &c})
	}
	done := kernel.StreamChunk{Type: kernel.ChunkDone, StopReason: t.StopReason}
	if t.Usage != nil {
		u := cloneTokenUsage(*t.Usage)
		done.Usage = &u
	}
	return append(out, done)
}

// Stream returns the chunks of the next scripted turn. ctx.Err() is checked before
// every yield and is yielded once, in the error slot, when set; the sequence then
// ends. The body returns as soon as yield returns false and never calls yield
// again. A failed turn and a call past the last turn each yield one terminal
// error chunk.
func (m *ScriptedModels) Stream(ctx context.Context, msgs []kernel.Message, opts llmhub.ModelOptions) iter.Seq2[kernel.StreamChunk, error] {
	return func(yield func(kernel.StreamChunk, error) bool) {
		if err := ctx.Err(); err != nil {
			yield(kernel.StreamChunk{}, err)
			return
		}
		turn, err := m.take(msgs, opts)
		if err == nil {
			err = turn.Err
		}
		if err != nil {
			yield(kernel.StreamChunk{Type: kernel.ChunkError, Error: err.Error()}, nil)
			return
		}
		for _, c := range turn.chunks() {
			if err := ctx.Err(); err != nil {
				yield(kernel.StreamChunk{}, err)
				return
			}
			if !yield(c, nil) {
				return
			}
		}
	}
}

// cloneMessageSlice deep-copies msgs; nil stays nil.
func cloneMessageSlice(msgs []kernel.Message) []kernel.Message {
	if msgs == nil {
		return nil
	}
	out := make([]kernel.Message, len(msgs))
	for i, m := range msgs {
		out[i] = m.Clone()
	}
	return out
}

// cloneToolCalls deep-copies calls; nil stays nil.
func cloneToolCalls(calls []kernel.ToolCall) []kernel.ToolCall {
	if calls == nil {
		return nil
	}
	return kernel.Message{ToolCalls: calls}.Clone().ToolCalls
}

// cloneTokenUsage copies u, including its optional counters.
func cloneTokenUsage(u kernel.TokenUsage) kernel.TokenUsage {
	if u.CacheReadTokens != nil {
		v := *u.CacheReadTokens
		u.CacheReadTokens = &v
	}
	if u.CacheWriteTokens != nil {
		v := *u.CacheWriteTokens
		u.CacheWriteTokens = &v
	}
	return u
}
