package aecmock

import (
	"context"
	"iter"
	"sync"

	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/kernel"
)

// Compile-time proof that ScriptedProvider is an llmhub.Provider.
var _ llmhub.Provider = (*ScriptedProvider)(nil)

// ScriptedProviderOption changes how a ScriptedProvider behaves. Options are
// applied in order by NewScriptedProvider.
type ScriptedProviderOption func(*ScriptedProvider)

// WithCallHook runs hook before each Chat or Stream call is served, with the
// call's ctx and the zero-based index of the call. Every call counts, the
// compaction summarizer's Chat calls included, and the index is taken when the
// call starts: for Chat when it is made, for Stream when its sequence is first
// iterated.
//
// The hook may block, for example until ctx is done (a provider that stalls), or
// cancel a ctx it closed over (a stop in the middle of a turn), or only count
// calls. Once it returns the call is served as usual, so a call whose ctx is done
// by then fails with the ctx error and is not recorded: Calls lists the calls that
// were served.
func WithCallHook(hook func(ctx context.Context, call int)) ScriptedProviderOption {
	return func(p *ScriptedProvider) { p.hook = hook }
}

// WithHangAfterChunks makes every Stream call yield the first n chunks of its
// turn and then block until its ctx is done, yielding ctx.Err() once and ending:
// a provider that streams a little and stalls (MockProvider's WithHangAfter, for
// scripted turns). A negative n counts as 0, and an n beyond the number of chunks
// hangs after the last one. Chat is not affected.
func WithHangAfterChunks(n int) ScriptedProviderOption {
	return func(p *ScriptedProvider) {
		p.hang = true
		p.hangAfter = max(n, 0)
	}
}

// WithScriptedOptions passes options to the ScriptedModels the provider serves its
// turns with. The one that matters for a provider is WithSummaryReply (or
// WithSummaryError): a Chat call whose options carry a system prompt, which is the
// call the compaction summarizer makes, is then answered with that text and does not
// consume a scripted turn. The option resolver and provider name options only matter
// when the models are used as the model port of an agent loop.
func WithScriptedOptions(opts ...ScriptedOption) ScriptedProviderOption {
	return func(p *ScriptedProvider) { p.models.With(opts...) }
}

// ScriptedProvider is a public llmhub.Provider that answers its n-th call with the
// n-th scripted turn, so a test can drive a full Agent turn through an AgentCore
// without any network access: register it with AgentCore.UseLLMProvider and the
// agent's chat, stream and summarizer calls are served from the script. It is
// exported for code outside this module as well, such as server/go, which needs
// the same deterministic turns (REL-02 groundwork).
//
// The turn logic is that of ScriptedModels (it is used by composition): Chat
// returns the turn as a model response, Stream yields the turn's chunks, a failed
// turn fails Chat and ends Stream with one terminal error chunk, and a call past
// the last turn fails the same way. Both honour ctx as llmhub.Provider requires.
// Stream is a pure iterator that starts no goroutine, so abandoning it cannot
// leak one. CountTokens counts like MockProvider. The provider is safe for
// concurrent use. The zero value is not usable; create one with
// NewScriptedProvider.
type ScriptedProvider struct {
	name   string
	models *ScriptedModels
	// counter supplies CountTokens; it keeps no state.
	counter MockProvider

	// Set once, by NewScriptedProvider, before the provider is used.
	hook      func(ctx context.Context, call int)
	hang      bool
	hangAfter int

	mu   sync.Mutex
	next int
}

// NewScriptedProvider returns a ScriptedProvider named name that answers with
// turns in order.
func NewScriptedProvider(name string, turns []ScriptedTurn, opts ...ScriptedProviderOption) *ScriptedProvider {
	p := &ScriptedProvider{name: name, models: NewScriptedModels(turns...)}
	for _, opt := range opts {
		if opt != nil {
			opt(p)
		}
	}
	return p
}

// Name returns the provider name.
func (p *ScriptedProvider) Name() string { return p.name }

// Version returns the plugin version.
func (*ScriptedProvider) Version() string { return mockVersion }

// Initialize does nothing.
func (*ScriptedProvider) Initialize(context.Context) error { return nil }

// Dispose does nothing.
func (*ScriptedProvider) Dispose(context.Context) error { return nil }

// Calls returns the calls that were served, in order. The result and everything it
// holds are copies, so the caller may change them freely. It is safe for
// concurrent use.
func (p *ScriptedProvider) Calls() []MockCall { return p.models.Calls() }

// begin takes the index of a call and runs the hook.
func (p *ScriptedProvider) begin(ctx context.Context) {
	p.mu.Lock()
	call := p.next
	p.next++
	p.mu.Unlock()
	if p.hook != nil {
		p.hook(ctx, call)
	}
}

// Chat runs the hook and returns the next scripted turn as a model response. A
// ctx that is done returns the bare ctx.Err() before the turn is used.
func (p *ScriptedProvider) Chat(ctx context.Context, msgs []kernel.Message, opts llmhub.ModelOptions) (kernel.ModelResponse, error) {
	p.begin(ctx)
	return p.models.Chat(ctx, msgs, opts)
}

// Stream returns the chunks of the next scripted turn. The hook runs when the
// sequence is first iterated. ctx.Err() is yielded once, in the error slot, when it
// is set before a chunk; the sequence then ends. The body returns as soon as yield
// returns false and never calls yield again.
func (p *ScriptedProvider) Stream(ctx context.Context, msgs []kernel.Message, opts llmhub.ModelOptions) iter.Seq2[kernel.StreamChunk, error] {
	return func(yield func(kernel.StreamChunk, error) bool) {
		p.begin(ctx)
		yielded := 0
		for chunk, err := range p.models.Stream(ctx, msgs, opts) {
			if p.hang && yielded >= p.hangAfter {
				break
			}
			if !yield(chunk, err) {
				return
			}
			if err != nil {
				return
			}
			yielded++
		}
		if p.hang {
			<-ctx.Done()
			yield(kernel.StreamChunk{}, ctx.Err())
		}
	}
}

// CountTokens counts like MockProvider.CountTokens: the UTF-16 code units of the
// contents divided by 4, rounded up. A ctx that is done returns the bare ctx.Err().
func (p *ScriptedProvider) CountTokens(ctx context.Context, msgs []kernel.Message) (int, error) {
	return p.counter.CountTokens(ctx, msgs)
}
