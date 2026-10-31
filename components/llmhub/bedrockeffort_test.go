package llmhub

import (
	"encoding/json"
	"slices"
	"testing"
)

// A-176: which Bedrock ids are Claude models, and the effort levels of the known ones.
func TestBedrockEffortSupport(t *testing.T) {
	for id, want := range map[string]bool{
		"anthropic.claude-opus-5-5":        true,
		"apac.anthropic.claude-sonnet-4-6": true,
		"arn:aws:bedrock:us-east-1:123:inference-profile/us.anthropic.claude-opus-5-5": true,
		"arn:aws:bedrock:us-east-1:123:application-inference-profile/abc123":           false,
		"meta.llama3-70b-instruct-v1:0":                                                false,
	} {
		if got := IsBedrockAnthropicModel(id); got != want {
			t.Errorf("IsBedrockAnthropicModel(%q) = %v", id, got)
		}
	}

	all := []string{"low", "medium", "high", "xhigh", "max"}
	for id, want := range map[string][]string{
		"us.anthropic.claude-opus-5-5":              all,
		"anthropic.claude-opus-5":                   all,
		"anthropic.claude-opus-4-6-v1":              {"low", "medium", "high", "max"},
		"anthropic.claude-opus-4-5-20251101-v1:0":   {"low", "medium", "high"},
		"anthropic.claude-opus-5-7":                 nil,
		"anthropic.claude-sonnet-4-5-20250929-v1:0": nil,
		"anthropic.claude-haiku-4-5-20251001-v1:0":  nil,
		"amazon.titan-text-express-v1":              nil,
	} {
		got, ok := LookupBedrockEffort(id)
		if ok != (want != nil) || !slices.Equal(got.Efforts, want) {
			t.Errorf("LookupBedrockEffort(%q) = %+v, %v; want %v", id, got, ok, want)
		}
	}
	if got, _ := LookupBedrockEffort("anthropic.claude-opus-4-5-20251101-v1:0"); got.Beta != "effort-2025-11-24" {
		t.Errorf("Opus 4.5 beta = %q", got.Beta)
	}
}

// A-176: the Bedrock model listing reports the levels of known Claude models only.
func TestMapBedrockFoundationModelReasoningEfforts(t *testing.T) {
	efforts := func(id string) []string {
		raw, _ := json.Marshal(map[string]string{"modelId": id})
		info, ok := MapBedrockFoundationModel(raw, "bedrock")
		if !ok {
			t.Fatalf("%s dropped", id)
		}
		return info.ReasoningEfforts
	}
	if got := efforts("anthropic.claude-opus-5-5"); !slices.Equal(got, []string{"low", "medium", "high", "xhigh", "max"}) {
		t.Errorf("Opus 5.5 efforts = %v", got)
	}
	if got := efforts("anthropic.claude-sonnet-4-5-20250929-v1:0"); got != nil {
		t.Errorf("Sonnet 4.5 efforts = %v", got)
	}
	if got := efforts("amazon.titan-text-express-v1"); got != nil {
		t.Errorf("Titan efforts = %v", got)
	}
}
