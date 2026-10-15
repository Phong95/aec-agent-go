package aecmock

import (
	"context"
	"iter"
	"math"
	"slices"
	"strings"
	"sync"

	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/kernel"
)

// Compile-time proof that MockProvider is an llmhub.Provider.
var _ llmhub.Provider = (*MockProvider)(nil)

// MockCall is one recorded call to Chat or Stream. It is test bookkeeping, not a
// wire type.
type MockCall struct {
	Messages []kernel.Message
	Options  llmhub.ModelOptions
}

// MockOption changes how a MockProvider answers. Options are applied in order.
type MockOption func(*MockProvider)

// WithStream replaces the default word-by-word stream with the chunks the
// script returns. They are yielded verbatim, so the script is responsible for
// ending with a done chunk when the test needs one.
func WithStream(script func([]kernel.Message) []kernel.StreamChunk) MockOption {
	return func(m *MockProvider) { m.streamScript = script }
}

// WithResponse replaces the default Chat response with the one the script
// returns.
func WithResponse(script func([]kernel.Message) kernel.ModelResponse) MockOption {
	return func(m *MockProvider) { m.responseScript = script }
}

// WithFailure makes the provider fail the way a real one does: Chat returns err,
// and Stream yields one terminal chunk of type error carrying err.Message, with a
// nil error, and then ends (the in-band failure contract of llmhub.Provider). The
// failure wins over WithStream and WithResponse.
func WithFailure(err *kernel.ModelError) MockOption {
	return func(m *MockProvider) { m.failure = err }
}

// WithHangAfter makes Stream yield n chunks and then block until its context is
// done, yielding ctx.Err() once and ending. It simulates an upstream that stalls,
// for cancellation tests. A negative n counts as 0; an n beyond the number of
// chunks hangs after the last one.
func WithHangAfter(n int) MockOption {
	return func(m *MockProvider) {
		m.hang = true
		m.hangAfter = max(n, 0)
	}
}

// MockProvider is a deterministic llmhub.Provider for tests. With no options it
// answers with respond(messages): Chat
// returns it as an assistant message with fixed usage, and Stream yields it one
// space-separated word at a time, each word followed by a space, then a done
// chunk. The options add scripted chunks and responses, in-band failures and
// hangs.
//
// Stream is a pure iterator that starts no goroutine, so abandoning it cannot
// leak one. The zero value is not usable; create one with NewMockProvider.
type MockProvider struct {
	name    string
	respond func([]kernel.Message) string

	streamScript   func([]kernel.Message) []kernel.StreamChunk
	responseScript func([]kernel.Message) kernel.ModelResponse
	failure        *kernel.ModelError
	hang           bool
	hangAfter      int

	mu    sync.Mutex
	calls []MockCall
}

// NewMockProvider returns a MockProvider named name. A nil respond answers with
// the empty string.
func NewMockProvider(name string, respond func([]kernel.Message) string, opts ...MockOption) *MockProvider {
	m := &MockProvider{name: name, respond: respond}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// Name returns the provider name.
func (m *MockProvider) Name() string { return m.name }

// Version returns the plugin version.
func (*MockProvider) Version() string { return mockVersion }

// Initialize does nothing.
func (*MockProvider) Initialize(context.Context) error { return nil }

// Dispose does nothing.
func (*MockProvider) Dispose(context.Context) error { return nil }

// content returns the response text for msgs.
func (m *MockProvider) content(msgs []kernel.Message) string {
	if m.respond == nil {
		return ""
	}
	return m.respond(msgs)
}

// record stores a deep-enough copy of one call.
func (m *MockProvider) record(msgs []kernel.Message, opts llmhub.ModelOptions) {
	call := MockCall{Messages: cloneMessages(msgs), Options: cloneOptions(opts)}
	m.mu.Lock()
	m.calls = append(m.calls, call)
	m.mu.Unlock()
}

// Calls returns the recorded calls in order. The result and everything it holds
// are copies, so the caller may change them freely. It is safe for concurrent
// use.
func (m *MockProvider) Calls() []MockCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]MockCall, len(m.calls))
	for i, c := range m.calls {
		out[i] = MockCall{Messages: cloneMessages(c.Messages), Options: cloneOptions(c.Options)}
	}
	return out
}

// Chat returns the scripted or default response. A cancelled context returns the
// bare ctx.Err(), as llmhub.Provider requires.
func (m *MockProvider) Chat(ctx context.Context, msgs []kernel.Message, opts llmhub.ModelOptions) (kernel.ModelResponse, error) {
	m.record(msgs, opts)
	if err := ctx.Err(); err != nil {
		return kernel.ModelResponse{}, err
	}
	if m.failure != nil {
		return kernel.ModelResponse{}, m.failure
	}
	if m.responseScript != nil {
		return m.responseScript(msgs), nil
	}
	return kernel.ModelResponse{
		Message:    kernel.Message{Role: kernel.RoleAssistant, Content: m.content(msgs)},
		Usage:      kernel.TokenUsage{InputTokens: 100, OutputTokens: 50},
		StopReason: kernel.StopEndTurn,
	}, nil
}

// chunks builds the chunk list of one stream.
func (m *MockProvider) chunks(msgs []kernel.Message) []kernel.StreamChunk {
	switch {
	case m.failure != nil:
		return []kernel.StreamChunk{{Type: kernel.ChunkError, Error: m.failure.Message}}
	case m.streamScript != nil:
		return m.streamScript(msgs)
	}
	words := strings.Split(m.content(msgs), " ")
	out := make([]kernel.StreamChunk, 0, len(words)+1)
	for _, word := range words {
		text := word + " "
		out = append(out, kernel.StreamChunk{Type: kernel.ChunkText, Content: &text})
	}
	return append(out, kernel.StreamChunk{Type: kernel.ChunkDone})
}

// Stream returns the chunk sequence. ctx.Err() is checked before every yield and
// is yielded once, in the error slot, when set; the sequence then ends. The body
// returns as soon as yield returns false and never calls yield again.
func (m *MockProvider) Stream(ctx context.Context, msgs []kernel.Message, opts llmhub.ModelOptions) iter.Seq2[kernel.StreamChunk, error] {
	m.record(msgs, opts)
	return func(yield func(kernel.StreamChunk, error) bool) {
		chunks := m.chunks(msgs)
		if m.hang && m.hangAfter < len(chunks) {
			chunks = chunks[:m.hangAfter]
		}
		for _, c := range chunks {
			if err := ctx.Err(); err != nil {
				yield(kernel.StreamChunk{}, err)
				return
			}
			if !yield(c, nil) {
				return
			}
		}
		if m.hang {
			<-ctx.Done()
			yield(kernel.StreamChunk{}, ctx.Err())
		}
	}
}

// CountTokens estimates a quarter token per UTF-16 code unit: it sums the UTF-16
// code units of the messages and rounds the quarter up, so the result is an int. A cancelled context returns
// the bare ctx.Err().
func (m *MockProvider) CountTokens(ctx context.Context, msgs []kernel.Message) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	total := 0
	for _, msg := range msgs {
		total += kernel.UTF16Len(msg.Content)
	}
	return int(math.Ceil(float64(total) / 4)), nil
}

// cloneMessages copies the slice and the slices inside each message.
func cloneMessages(msgs []kernel.Message) []kernel.Message {
	if msgs == nil {
		return nil
	}
	out := slices.Clone(msgs)
	for i := range out {
		out[i].ToolCalls = slices.Clone(out[i].ToolCalls)
		out[i].ToolResults = slices.Clone(out[i].ToolResults)
	}
	return out
}

// cloneOptions copies the slices of the options.
func cloneOptions(o llmhub.ModelOptions) llmhub.ModelOptions {
	o.StopSequences = slices.Clone(o.StopSequences)
	o.Tools = slices.Clone(o.Tools)
	return o
}
