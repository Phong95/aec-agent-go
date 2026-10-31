package gemini

import (
	"strings"
	"testing"

	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/internal/llmtest"
	"github.com/Phong95/aec-agent-go/kernel"
)

// A-178: a thinking model is asked for its thought summaries, which come out as
// reasoning chunks; an older model gets no thinkingConfig.
func TestThoughtSummariesAreReasoning(t *testing.T) {
	for model, think := range map[string]bool{"gemini-2.5-flash": true, "models/gemini-3-pro-preview": true, "gemini-2.0-flash": false} {
		if thinkingModel.MatchString(model) != think {
			t.Errorf("%s: thinking model = %v", model, !think)
		}
	}

	up := llmtest.NewUpstream(t, func(llmtest.Request) llmtest.Reply {
		return sseReply(
			`{"candidates":[{"content":{"parts":[{"text":"Pondering.","thought":true}]}}]}`,
			`{"candidates":[{"content":{"parts":[{"text":"Done."}]},"finishReason":"STOP"}]}`,
		)
	})
	p := newTestProvider(t, up, "gemini-2.5-flash")
	chunks := drain(t, p, []kernel.Message{{Role: kernel.RoleUser, Content: "hi"}}, llmhub.ModelOptions{})
	var reasoning, text strings.Builder
	for _, c := range chunks {
		switch c.Type {
		case kernel.ChunkReasoning:
			reasoning.WriteString(*c.Content)
		case kernel.ChunkText:
			text.WriteString(*c.Content)
		default:
		}
	}
	if reasoning.String() != "Pondering." || text.String() != "Done." {
		t.Fatalf("reasoning %q, text %q", reasoning.String(), text.String())
	}
	if body := string(up.Requests()[0].Body); !strings.Contains(body, `"generationConfig":{"thinkingConfig":{"includeThoughts":true}}`) {
		t.Fatalf("body = %s", body)
	}
}
