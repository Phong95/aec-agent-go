package llmhub

import "github.com/Phong95/aec-agent-go/kernel"

// ProviderUsage (A-188) is a model call's token counts as a provider reports
// them, before they are turned into a kernel.TokenUsage with NewTokenUsage.
type ProviderUsage struct {
	Input  int `json:"input"`
	Output int `json:"output"`
	// CacheRead and CacheWrite are nil when the provider sent no such field, and
	// point at 0 when it sent 0, so stats can tell "nothing cached" from "not
	// reported".
	CacheRead  *int `json:"cacheRead,omitempty"`
	CacheWrite *int `json:"cacheWrite,omitempty"`
	// InputExcludesCache is true for the APIs whose input count leaves the cached
	// tokens out (Anthropic input_tokens, Bedrock Converse inputTokens); the
	// OpenAI-style APIs and Gemini count them in.
	InputExcludesCache bool `json:"inputExcludesCache,omitempty"`
}

// NewTokenUsage (A-188) gives every provider the same meaning for TokenUsage:
// InputTokens is all the input of the call, cache reads and cache writes
// included, so totals and the context measure compare across providers;
// CacheReadTokens and CacheWriteTokens are the parts of it that were read from
// or written to the provider's prompt cache. When the provider counts the input
// without the cache (InputExcludesCache), the cache counts are added to it.
// Negative counts are taken as 0.
func NewTokenUsage(u ProviderUsage) kernel.TokenUsage {
	usage := kernel.TokenUsage{InputTokens: max(0, u.Input), OutputTokens: max(0, u.Output)}
	if u.CacheRead != nil {
		v := max(0, *u.CacheRead)
		usage.CacheReadTokens = &v
		if u.InputExcludesCache {
			usage.InputTokens += v
		}
	}
	if u.CacheWrite != nil {
		v := max(0, *u.CacheWrite)
		usage.CacheWriteTokens = &v
		if u.InputExcludesCache {
			usage.InputTokens += v
		}
	}
	return usage
}

// IntPtr (A-188) returns a pointer to v, for a cache count a provider did send.
func IntPtr(v int) *int { return &v }
