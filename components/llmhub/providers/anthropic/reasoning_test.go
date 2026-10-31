package anthropic

import (
	"strings"
	"testing"

	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/kernel"
)

// A-178: a model that thinks by default is asked for its summarized thinking text,
// after output_config; no other model gets the field, and a message's reasoning is
// never sent back.
func TestRequestAsksForThinkingText(t *testing.T) {
	p, err := New(llmhub.ProviderConfig{APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	msgs := []kernel.Message{
		{Role: kernel.RoleUser, Content: "hi"},
		{Role: kernel.RoleAssistant, Content: "hello", Reasoning: "SECRET-THOUGHT"},
		{Role: kernel.RoleUser, Content: "again"},
	}
	body, _, err := p.buildRequest(msgs, llmhub.ModelOptions{Model: "claude-opus-5-5", ReasoningEffort: "low"})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(body); !strings.HasSuffix(got, `"output_config":{"effort":"low"},"thinking":{"type":"adaptive","display":"summarized"}}`) || strings.Contains(got, "SECRET-THOUGHT") {
		t.Fatalf("body = %s", got)
	}
	body, _, err = p.buildRequest(msgs, llmhub.ModelOptions{Model: "claude-sonnet-4-6"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "thinking") {
		t.Fatalf("body for an older model = %s", body)
	}
}
