package llmhub

import "encoding/json"

// ---------------------------------------------------------------------------
// LLM model metadata - provider-agnostic model catalog contract (A-54)
// ---------------------------------------------------------------------------
// Every field except ID and Provider is optional. A nil pointer, nil slice or
// empty string means the provider did not report the value - no value is ever
// guessed. Numbers and flags where zero or false is a real answer are pointers,
// so an explicit zero stays on the wire.

// ModelCapabilities is what a model can do.
type ModelCapabilities struct {
	// Vision: accepts image input.
	Vision *bool `json:"vision,omitempty"`
	// PDFInput: accepts PDF input.
	PDFInput *bool `json:"pdfInput,omitempty"`
	// ToolCalling: supports function/tool calling.
	ToolCalling *bool `json:"toolCalling,omitempty"`
	// Reasoning: supports extended thinking or reasoning effort.
	Reasoning *bool `json:"reasoning,omitempty"`
	// StructuredOutputs: supports structured/JSON-schema-constrained outputs.
	StructuredOutputs *bool `json:"structuredOutputs,omitempty"`
	// Streaming: supports streaming responses.
	Streaming *bool `json:"streaming,omitempty"`
}

// ModelPricing is the price of a model.
type ModelPricing struct {
	// InputUSDPerMillionTokens is USD per 1,000,000 input tokens.
	InputUSDPerMillionTokens *float64 `json:"inputUsdPerMillionTokens,omitempty"`
	// OutputUSDPerMillionTokens is USD per 1,000,000 output tokens.
	OutputUSDPerMillionTokens *float64 `json:"outputUsdPerMillionTokens,omitempty"`
}

// ModelRecord is the persistable form of ModelInfo: everything except the Raw
// provider payload, which can be large and is not a stable contract. This is what
// a ProviderStore keeps for each of a provider's models.
//
// ModelRecord must not get a MarshalJSON method: ModelInfo embeds it, so the
// method would be promoted and ModelInfo would lose its raw field.
type ModelRecord struct {
	// ID is the value passed as ModelOptions.Model.
	ID string `json:"id"`
	// Provider is the listing provider's Name (for example "anthropic", "novita").
	Provider string `json:"provider"`
	// DisplayName is the human-readable model name.
	DisplayName string `json:"displayName,omitempty"`
	// Description is the vendor-provided free-text description.
	Description string `json:"description,omitempty"`
	// OwnedBy is the vendor or organisation that owns the model.
	OwnedBy string `json:"ownedBy,omitempty"`
	// CreatedAt is the ISO 8601 creation/release date, when reported.
	CreatedAt string `json:"createdAt,omitempty"`
	// ContextWindowTokens is the max input/context tokens - the same meaning as
	// CompactorConfig.contextWindowTokens.
	ContextWindowTokens *int `json:"contextWindowTokens,omitempty"`
	// MaxContextWindowTokens is the largest context window the model accepts when a
	// client opts into more than the default (ContextWindowTokens). Nil when the
	// provider reports only one limit.
	MaxContextWindowTokens *int `json:"maxContextWindowTokens,omitempty"`
	// MaxOutputTokens is the max output tokens per response.
	MaxOutputTokens *int `json:"maxOutputTokens,omitempty"`
	// InputModalities is lowercase and an open set (for example "text", "image",
	// "pdf", "audio").
	InputModalities []string `json:"inputModalities,omitzero"`
	// OutputModalities is lowercase and an open set (for example "text").
	OutputModalities []string `json:"outputModalities,omitzero"`
	// ReasoningEfforts are the reasoning efforts the model accepts, lowercase, in the
	// provider's order (for example "low", "high").
	ReasoningEfforts []string `json:"reasoningEfforts,omitzero"`
	// DefaultReasoningEffort is the reasoning effort the provider uses when none is set.
	DefaultReasoningEffort string             `json:"defaultReasoningEffort,omitempty"`
	Capabilities           *ModelCapabilities `json:"capabilities,omitempty"`
	Pricing                *ModelPricing      `json:"pricing,omitempty"`
}

// ModelInfo is one model a provider's credentials can use, with normalized
// metadata. It is a ModelRecord plus the untouched provider payload.
type ModelInfo struct {
	ModelRecord
	// Raw is the untouched provider payload, for fields that are not normalized.
	// It is a json.RawMessage, so its exact bytes (key order, spelling) are kept.
	Raw json.RawMessage `json:"raw,omitempty"`
}

// ToModelRecord strips the non-persistable Raw payload from a listed model.
func ToModelRecord(m ModelInfo) ModelRecord {
	return m.ModelRecord
}
