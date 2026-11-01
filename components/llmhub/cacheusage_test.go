package llmhub_test

import (
	"testing"

	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/kernel"
)

func TestNewTokenUsage(t *testing.T) {
	three, two, zero, neg := 3, 2, 0, -4
	cases := []struct {
		name string
		in   llmhub.ProviderUsage
		want kernel.TokenUsage
	}{
		{"no cache fields", llmhub.ProviderUsage{Input: 10, Output: 5}, kernel.TokenUsage{InputTokens: 10, OutputTokens: 5}},
		{"input includes the cache", llmhub.ProviderUsage{Input: 10, Output: 5, CacheRead: &three}, kernel.TokenUsage{InputTokens: 10, OutputTokens: 5, CacheReadTokens: &three}},
		{
			"input excludes the cache",
			llmhub.ProviderUsage{Input: 10, Output: 5, CacheRead: &three, CacheWrite: &two, InputExcludesCache: true},
			kernel.TokenUsage{InputTokens: 15, OutputTokens: 5, CacheReadTokens: &three, CacheWriteTokens: &two},
		},
		{"a present zero stays", llmhub.ProviderUsage{Input: 1, CacheRead: &zero, InputExcludesCache: true}, kernel.TokenUsage{InputTokens: 1, CacheReadTokens: &zero}},
		{"negative counts are 0", llmhub.ProviderUsage{Input: neg, Output: neg, CacheRead: &neg}, kernel.TokenUsage{CacheReadTokens: &zero}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := llmhub.NewTokenUsage(c.in)
			if got.InputTokens != c.want.InputTokens || got.OutputTokens != c.want.OutputTokens ||
				!samePtr(got.CacheReadTokens, c.want.CacheReadTokens) || !samePtr(got.CacheWriteTokens, c.want.CacheWriteTokens) {
				t.Errorf("got %+v, want %+v", got, c.want)
			}
			if c.in.CacheRead != nil && got.CacheReadTokens == c.in.CacheRead {
				t.Error("the cache read count aliases the input pointer")
			}
		})
	}
}

func samePtr(a, b *int) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}
