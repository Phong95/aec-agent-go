package agent

import (
	"testing"

	"github.com/Phong95/aec-agent-go/components/agent/profile"
	"github.com/Phong95/aec-agent-go/components/llmhub"
)

// A-207: the agent's inferenceConfig.promptCaching is the default of every call, and a
// call that sets ModelOptions.PromptCaching wins.
func TestMergeInferencePromptCaching(t *testing.T) {
	off, on := false, true
	got := mergeInference(&profile.InferenceConfig{PromptCaching: &off}, llmhub.ModelOptions{})
	if got.PromptCaching == nil || *got.PromptCaching {
		t.Fatalf("PromptCaching = %v, want the agent default false", got.PromptCaching)
	}
	got = mergeInference(&profile.InferenceConfig{PromptCaching: &off}, llmhub.ModelOptions{PromptCaching: &on})
	if got.PromptCaching == nil || !*got.PromptCaching {
		t.Fatalf("PromptCaching = %v, want the call's true", got.PromptCaching)
	}
	if got := mergeInference(nil, llmhub.ModelOptions{}); got.PromptCaching != nil {
		t.Fatalf("PromptCaching = %v, want nil (on)", got.PromptCaching)
	}
}
