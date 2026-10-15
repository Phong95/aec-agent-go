package llmhub_test

import (
	"testing"

	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/internal/clonecheck"
)

// ModelRecord.Clone must share nothing with the original: a pointer or slice
// field added to a model record (or its capabilities and pricing) without a Clone
// line fails here.
func TestModelRecordCloneIsDeep(t *testing.T) {
	yes := true
	window, maxOut := 200000, 8192
	in, out := 3.0, 15.0
	sample := llmhub.ModelRecord{
		ID: "claude-sonnet", Provider: "anthropic", DisplayName: "Claude Sonnet", Description: "d",
		OwnedBy: "anthropic", CreatedAt: "2025-05-01T00:00:00Z",
		ContextWindowTokens: &window, MaxContextWindowTokens: &window, MaxOutputTokens: &maxOut,
		InputModalities: []string{"text", "image"}, OutputModalities: []string{"text"},
		ReasoningEfforts: []string{"low", "high"}, DefaultReasoningEffort: "low",
		Capabilities: &llmhub.ModelCapabilities{
			Vision: &yes, PDFInput: &yes, ToolCalling: &yes, Reasoning: &yes, StructuredOutputs: &yes, Streaming: &yes,
		},
		Pricing: &llmhub.ModelPricing{InputUSDPerMillionTokens: &in, OutputUSDPerMillionTokens: &out},
	}
	clonecheck.AssertDeep(t, sample, llmhub.ModelRecord.Clone)
}

// A nil pointer or slice stays nil (omitempty drops it).
func TestModelRecordCloneKeepsNil(t *testing.T) {
	got := llmhub.ModelRecord{ID: "m", Provider: "p"}.Clone()
	if got.Capabilities != nil || got.Pricing != nil || got.InputModalities != nil || got.ContextWindowTokens != nil {
		t.Fatalf("nil fields must stay nil: %#v", got)
	}
}
