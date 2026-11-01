package openaicompat

import (
	"encoding/json"

	sdk "github.com/openai/openai-go/v3"

	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/kernel"
)

// usageOf (A-188) maps a chat completion usage. prompt_tokens already counts the
// cached tokens on every OpenAI-compatible API, so they are not added to it. The
// cache read count is the first one the server sent of:
//   - prompt_tokens_details.cached_tokens (OpenAI, OpenRouter and most gateways);
//   - prompt_cache_hit_tokens (DeepSeek's own API);
//   - a top-level cached_tokens (Moonshot/Kimi).
//
// The cache write count is prompt_tokens_details.cache_write_tokens (OpenRouter, for
// the models that report it). A count the server did not send stays nil, and a
// JSON null counts as not sent.
func usageOf(u sdk.CompletionUsage) kernel.TokenUsage {
	pu := llmhub.ProviderUsage{Input: int(u.PromptTokens), Output: int(u.CompletionTokens)}
	d := u.PromptTokensDetails
	switch {
	case u.JSON.PromptTokensDetails.Valid() && d.JSON.CachedTokens.Valid():
		pu.CacheRead = llmhub.IntPtr(int(d.CachedTokens))
	default:
		for _, key := range []string{"prompt_cache_hit_tokens", "cached_tokens"} {
			if n, ok := extraInt(u.JSON.ExtraFields, key); ok {
				pu.CacheRead = &n
				break
			}
		}
	}
	if u.JSON.PromptTokensDetails.Valid() && d.JSON.CacheWriteTokens.Valid() {
		pu.CacheWrite = llmhub.IntPtr(int(d.CacheWriteTokens))
	}
	return llmhub.NewTokenUsage(pu)
}

// extraInt reads an integer field the SDK does not know; false when it is absent,
// null or not a number.
func extraInt[F interface{ Raw() string }](extra map[string]F, key string) (int, bool) {
	f, ok := extra[key]
	if !ok {
		return 0, false
	}
	var n *float64
	if json.Unmarshal([]byte(f.Raw()), &n) != nil || n == nil {
		return 0, false
	}
	return int(*n), true
}
