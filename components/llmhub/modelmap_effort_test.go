package llmhub_test

import (
	"slices"
	"testing"

	"github.com/Phong95/aec-agent-go/components/llmhub"
)

// A-164: an Anthropic model lists the effort levels its capabilities support, lowest first.
func TestMapAnthropicModelReasoningEfforts(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want []string
	}{
		{
			"supported levels in order",
			`{"id":"claude-opus-5-5","capabilities":{"effort":{"supported":true,"max":{"supported":true},"low":{"supported":true},"medium":{"supported":true},"high":{"supported":true},"xhigh":{"supported":false}}}}`,
			[]string{"low", "medium", "high", "max"},
		},
		{"no effort support", `{"id":"claude-haiku-4-5","capabilities":{"effort":{"supported":false,"low":{"supported":true}}}}`, nil},
		{"no capabilities", `{"id":"old"}`, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := llmhub.MapAnthropicModel([]byte(c.raw), "anthropic").ReasoningEfforts
			if !slices.Equal(got, c.want) {
				t.Fatalf("ReasoningEfforts = %v, want %v", got, c.want)
			}
		})
	}
}
