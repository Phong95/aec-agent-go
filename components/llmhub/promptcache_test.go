package llmhub_test

import (
	"testing"

	"github.com/Phong95/aec-agent-go/components/llmhub"
)

// A-207: the shared prompt-cache rules (core/ts prompt-cache.ts).
func TestPromptCacheRules(t *testing.T) {
	off, on := false, true
	if !llmhub.PromptCachingOn(llmhub.ModelOptions{}) || !llmhub.PromptCachingOn(llmhub.ModelOptions{PromptCaching: &on}) || llmhub.PromptCachingOn(llmhub.ModelOptions{PromptCaching: &off}) {
		t.Error("PromptCachingOn: nil and true are on, false is off")
	}
	for id, want := range map[string]bool{
		"us.anthropic.claude-opus-5-5": true,
		"arn:aws:bedrock:us-east-1:1:inference-profile/global.anthropic.claude-sonnet-5-5": true,
		"amazon.nova-pro-v1:0":                   true,
		"meta.llama4-maverick-17b-instruct-v1:0": false,
	} {
		if got := llmhub.BedrockCachePoints(id); got != want {
			t.Errorf("BedrockCachePoints(%q) = %v, want %v", id, got, want)
		}
	}
	if llmhub.BedrockClaude("amazon.nova-pro-v1:0") || !llmhub.BedrockClaude("eu.anthropic.claude-haiku-5-5") {
		t.Error("BedrockClaude")
	}
	for id, want := range map[string]bool{
		"anthropic/claude-haiku-5.5": true, "google/gemini-3.5-flash": true,
		"deepseek/deepseek-v4.1-flash": false, "google/gemma-4": false,
	} {
		if got := llmhub.OpenRouterCacheControl(id); got != want {
			t.Errorf("OpenRouterCacheControl(%q) = %v, want %v", id, got, want)
		}
	}
}
