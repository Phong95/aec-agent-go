package anthropic

import (
	"strings"
	"testing"

	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/kernel"
)

// A-164: ModelOptions.ReasoningEffort is sent as the provider's effort setting, and left
// out when it is not set.
func TestRequestCarriesReasoningEffort(t *testing.T) {
	msgs := []kernel.Message{{Role: kernel.RoleUser, Content: "hi"}}
	p, err := New(llmhub.ProviderConfig{APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	build := func(opts llmhub.ModelOptions) []byte {
		b, _, err := p.buildRequest(msgs, opts)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	if got := string(build(llmhub.ModelOptions{ReasoningEffort: "xhigh"})); !strings.Contains(got, `"output_config":{"effort":"xhigh"}`) {
		t.Fatalf("body = %s", got)
	}
	if got := string(build(llmhub.ModelOptions{})); strings.Contains(got, `output_config`) {
		t.Fatalf("body without an effort = %s", got)
	}
}
