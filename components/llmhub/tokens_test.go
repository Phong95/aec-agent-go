package llmhub_test

import (
	"testing"

	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/internal/jsonx"
	"github.com/Phong95/aec-agent-go/internal/wiretest"
	"github.com/Phong95/aec-agent-go/kernel"
)

// TestEstimateTokensMatchesGolden replays llm-count-tokens.json, the recorded
// token counts over messages with tool calls (one with an argumentsError), tool
// results, astral characters and empty arrays.
func TestEstimateTokensMatchesGolden(t *testing.T) {
	var f struct {
		Entries []struct {
			Name     string           `json:"name"`
			Messages []kernel.Message `json:"messages"`
			Tokens   int              `json:"tokens"`
		} `json:"entries"`
	}
	if err := jsonx.UnmarshalStrict(wiretest.Load(t, "llm-count-tokens.json"), &f); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	if len(f.Entries) < 8 {
		t.Fatalf("the corpus has %d entries, want at least 8", len(f.Entries))
	}
	for _, e := range f.Entries {
		t.Run(e.Name, func(t *testing.T) {
			if got := llmhub.EstimateTokens(e.Messages); got != e.Tokens {
				t.Fatalf("EstimateTokens = %d, TS countTokens = %d", got, e.Tokens)
			}
		})
	}
}

func TestEstimateTokensEmpty(t *testing.T) {
	if got := llmhub.EstimateTokens(nil); got != 0 {
		t.Fatalf("EstimateTokens(nil) = %d, want 0", got)
	}
}
