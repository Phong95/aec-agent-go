package llmhub

import (
	"regexp"
	"strings"
)

// A-207: prompt caching. A provider's prompt cache only reuses an exact prefix of the
// request (tools, then system, then messages), so the providers whose API needs explicit
// opt-in mark the stable parts:
//   - Anthropic: cache_control on the last system block (it covers the tools and the system
//     prompt) and on the last block of the last message (the growing conversation; the
//     previous call's marker is always within the API's 20-block lookback, so every call
//     reads the history the previous one wrote);
//   - Bedrock Converse: the same two places as cachePoint blocks, only for model families
//     that support them (BedrockCachePoints);
//   - OpenRouter: cache_control parts for the upstreams that need them
//     (OpenRouterCacheControl);
//   - OpenAI-compatible: prompt_cache_key (the session id) for the registry entries that
//     take it.
//
// Gemini caches implicitly and the ChatGPT subscription is routed by its cache key
// (A-188/A-202). The same rules as core/ts (llm-provider-hub/prompt-cache.ts).

// PromptCachingOn reports whether prompt caching is on for a call: on unless
// ModelOptions.PromptCaching is set to false.
func PromptCachingOn(o ModelOptions) bool {
	return o.PromptCaching == nil || *o.PromptCaching
}

var (
	bedrockClaude = regexp.MustCompile(`(^|\.)anthropic\.claude`)
	bedrockNova   = regexp.MustCompile(`(^|\.)amazon\.nova`)
)

// BedrockCachePoints reports whether a Bedrock model takes Converse cachePoint blocks:
// Anthropic Claude and Amazon Nova. The id may carry a cross-region inference prefix
// (us., eu., apac., global.) or be an ARN. Other model families get no markers.
func BedrockCachePoints(modelID string) bool {
	id := strings.ToLower(modelID[strings.LastIndex(modelID, "/")+1:])
	return bedrockClaude.MatchString(id) || bedrockNova.MatchString(id)
}

// BedrockClaude reports whether a Bedrock model id is an Anthropic Claude model (the
// only family that takes a cachePoint after the tools).
func BedrockClaude(modelID string) bool {
	id := strings.ToLower(modelID[strings.LastIndex(modelID, "/")+1:])
	return bedrockClaude.MatchString(id)
}

// OpenRouterCacheControl reports whether an OpenRouter model needs cache_control parts to
// cache: Anthropic Claude and Google Gemini slugs.
func OpenRouterCacheControl(modelID string) bool {
	id := strings.ToLower(modelID)
	return strings.HasPrefix(id, "anthropic/") || strings.HasPrefix(id, "google/gemini")
}
