// Package llmhub holds the LLM provider contracts of the SDK. It ports
// core/ts/src/components/llmhub-provider-hub.
package llmhub

import (
	"context"
	"iter"
	"slices"

	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/kernel"
)

// Provider is a swappable AI model provider.
//
// Streaming contract of Stream:
//   - On success the sequence yields chunks, then exactly one chunk of type
//     done, and nothing after it. The done chunk carries the stop reason the
//     model reported (kernel.StreamChunk.StopReason, A-88), mapped like Chat maps
//     it, and leaves it empty when the model reported none.
//   - A provider failure is one terminal chunk of type error with a non-empty
//     Error, yielded with a nil error, after which the sequence ends. Every
//     built-in provider does the same.
//   - The error slot carries only ctx.Err(): it is yielded once on
//     cancellation, then the sequence ends.
//   - When yield returns false the body returns at once. Every implementation
//     defers the cancel of its derived context and the close of its upstream, so
//     breaking out of the loop releases the connection.
//
// Chat returns failures as *kernel.ModelError and returns the bare ctx.Err() on
// cancellation, so errors.Is(err, context.Canceled) works; an aborted call is not
// a model failure.
//
// CountTokens counts UTF-16 code units.
//
// ListModels is optional: a provider that has a model
// catalog also implements ModelLister, and a caller finds out with a type
// assertion.
type Provider interface {
	kernel.Plugin
	Chat(ctx context.Context, msgs []kernel.Message, opts ModelOptions) (kernel.ModelResponse, error)
	Stream(ctx context.Context, msgs []kernel.Message, opts ModelOptions) iter.Seq2[kernel.StreamChunk, error]
	CountTokens(ctx context.Context, msgs []kernel.Message) (int, error)
}

// ModelLister is the optional catalog side of a Provider. It lists the models
// the provider's credentials can use, with normalized metadata. It is optional
// because not every backend exposes a catalog.
type ModelLister interface {
	ListModels(ctx context.Context) ([]ModelInfo, error)
}

// TokenUpdater is the optional token-swap side of a Provider that signs in with
// an OAuth token: the ChatGPT subscription provider (Phase 8) implements it. A
// caller, such as a server routine that refreshes the token before it expires,
// finds out with a type assertion on an llmhub.Provider.
//
// UpdateTokens swaps the access token and the account id as one pair: a request
// that starts after the call uses both new values, and a request that already
// started keeps the old pair, so a request never carries a token with another
// token's account id. It is safe to call while requests are in flight. The OAuth
// refresh itself stays outside the SDK (a server route); the SDK only holds the
// pair it is given.
type TokenUpdater interface {
	UpdateTokens(accessToken, accountID string)
}

// DefaultMaxOutputTokens is the output token limit the Anthropic and Bedrock
// providers send when ModelOptions.MaxTokens is nil (A-88). The old default of
// 4096 cut large tool calls (a file write) short.
const DefaultMaxOutputTokens = 16384

// ModelOptions are the per-call options of Chat and Stream. Cancellation is the
// ctx parameter, not a field: it is a per-call control, not an inference
// parameter, and is never traced or persisted.
//
// Numbers where zero is a valid value are pointers, so an explicit zero is
// distinct from unset.
//
// MaxTokens is the output token limit of one response. When it is nil, the
// Anthropic and Bedrock providers send DefaultMaxOutputTokens and the
// OpenAI-compatible and Gemini providers leave it out, so the server's default
// applies (A-88; Gemini counts thinking tokens against the limit, A-162). A model whose output cap is lower than that default (the Claude 3 and
// 3.5 families, for example) needs it set, or the provider rejects the request.
type ModelOptions struct {
	Provider      string               `json:"provider,omitempty"`
	Model         string               `json:"model,omitempty"`
	MaxTokens     *int                 `json:"maxTokens,omitempty"`
	Temperature   *float64             `json:"temperature,omitempty"`
	TopP          *float64             `json:"topP,omitempty"`
	StopSequences []string             `json:"stopSequences,omitzero"`
	Tools         []toolhub.Definition `json:"tools,omitzero"`
	SystemPrompt  string               `json:"systemPrompt,omitempty"`
	// ReasoningEffort (A-164) is how much the model reasons, as an effort level name,
	// lowercase ("low", "high"; ModelInfo.ReasoningEfforts lists what a model accepts).
	// Empty = the provider's default. Anthropic sends it as output_config.effort,
	// OpenAI-compatible as reasoning_effort, the ChatGPT subscription (Responses API) as
	// reasoning.effort, and Gemini as thinkingConfig.thinkingLevel. Bedrock sends it to
	// Claude models as additionalModelRequestFields.output_config.effort (A-176) and
	// ignores it for other models. The provider rejects a level the model does not accept.
	ReasoningEffort string `json:"reasoningEffort,omitempty"`
}

// ModelAcceptsImages (A-165) says whether a listed model takes images: accepts is true when
// it reports vision support or image input; known is false when it reports neither
// capabilities.vision nor input modalities (a host should allow images then, with a
// warning). The same rules as core/ts modelAcceptsImages.
func ModelAcceptsImages(m ModelRecord) (accepts, known bool) {
	if m.Capabilities != nil && m.Capabilities.Vision != nil && *m.Capabilities.Vision {
		return true, true
	}
	if slices.Contains(m.InputModalities, "image") {
		return true, true
	}
	if (m.Capabilities != nil && m.Capabilities.Vision != nil) || m.InputModalities != nil {
		return false, true
	}
	return false, false
}

// Float returns a pointer to v, for the optional number fields of ModelOptions:
// llmhub.ModelOptions{Temperature: llmhub.Float(0.2)}.
func Float(v float64) *float64 { return &v }

// Int returns a pointer to v, for the optional integer fields of ModelOptions:
// llmhub.ModelOptions{MaxTokens: llmhub.Int(1024)}.
func Int(v int) *int { return &v }
