package llmhub_test

import (
	"testing"

	"github.com/Phong95/aec-agent-go/components/llmhub"
)

// A-178: only the Claude models that think by default are asked for their thinking
// text; the same table as core/ts reasoning-text.test.ts.
func TestClaudeThinksByDefault(t *testing.T) {
	for _, id := range []string{"claude-opus-5-5", "claude-sonnet-5", "claude-haiku-5-5", "claude-fable-5-1", "us.anthropic.claude-opus-5-5-v1:0", "anthropic.claude-sonnet-5-20260101-v1:0"} {
		if !llmhub.ClaudeThinksByDefault(id) || llmhub.ClaudeThinking(id) == nil {
			t.Errorf("%s: not a model that thinks by default", id)
		}
	}
	for _, id := range []string{"claude-sonnet-4-6", "claude-opus-4-8", "claude-haiku-4-5", "anthropic.claude-sonnet-4-20250514-v1:0", "claude-opus-50", "gpt-5"} {
		if llmhub.ClaudeThinksByDefault(id) || llmhub.ClaudeThinking(id) != nil {
			t.Errorf("%s: matched", id)
		}
	}
	if got := *llmhub.ClaudeThinking("claude-opus-5-5"); got != (llmhub.ClaudeThinkingDisplay{Type: "adaptive", Display: "summarized"}) {
		t.Errorf("ClaudeThinking = %+v", got)
	}
}
