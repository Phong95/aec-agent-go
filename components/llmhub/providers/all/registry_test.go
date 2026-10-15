package all_test

// The "Provider registry" cases, run on the registry that llm/all links. The agent
// construction cases belong to Phase 11. No request is sent: the providers are only
// built.

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"

	"github.com/Phong95/aec-agent-go/components/llmhub"
	_ "github.com/Phong95/aec-agent-go/components/llmhub/providers/all"
	"github.com/Phong95/aec-agent-go/components/llmhub/providers/anthropic"
	"github.com/Phong95/aec-agent-go/components/llmhub/providers/openaicompat"
	"github.com/Phong95/aec-agent-go/internal/wiretest"
)

// hubErrors holds the two fields of llm-hub-errors.json used here: the built-in
// order and the unknown-provider text, both recorded from the reference registry.
type hubErrors struct {
	UnknownProvider string   `json:"unknownProvider"`
	BuiltinOrder    []string `json:"builtinOrder"`
}

func loadHubErrors(t *testing.T) hubErrors {
	t.Helper()
	var e hubErrors
	if err := json.Unmarshal(wiretest.Load(t, "llm-hub-errors.json"), &e); err != nil {
		t.Fatalf("decode llm-hub-errors.json: %v", err)
	}
	return e
}

func TestProviderRegistryAll(t *testing.T) {
	fx := loadHubErrors(t)

	t.Run("lists built-in providers", func(t *testing.T) {
		got := llmhub.ListProviders()
		if len(got) < len(fx.BuiltinOrder) || !slices.Equal(got[:len(fx.BuiltinOrder)], fx.BuiltinOrder) {
			t.Fatalf("ListProviders = %v, want it to start with %v", got, fx.BuiltinOrder)
		}
	})

	t.Run("resolves anthropic to AnthropicLLMModelProvider", func(t *testing.T) {
		p, err := llmhub.ResolveProvider("anthropic", llmhub.ProviderConfig{APIKey: "test-key", Model: "claude-sonnet-4-20250514"})
		if err != nil {
			t.Fatalf("ResolveProvider: %v", err)
		}
		if _, ok := p.(*anthropic.Provider); !ok {
			t.Fatalf("provider is %T, want *anthropic.Provider", p)
		}
		if p.Name() != "anthropic" {
			t.Fatalf("Name = %q, want anthropic", p.Name())
		}
	})

	t.Run("resolves novita to OpenAICompatibleLLMModelProvider", func(t *testing.T) {
		p, err := llmhub.ResolveProvider("novita", llmhub.ProviderConfig{APIKey: "test-key", Model: "deepseek/deepseek-v3.2"})
		if err != nil {
			t.Fatalf("ResolveProvider: %v", err)
		}
		if _, ok := p.(*openaicompat.Provider); !ok {
			t.Fatalf("provider is %T, want *openaicompat.Provider", p)
		}
		if p.Name() != "novita" {
			t.Fatalf("Name = %q, want novita", p.Name())
		}
	})

	t.Run("throws on unknown provider", func(t *testing.T) {
		_, err := llmhub.ResolveProvider("nope", llmhub.ProviderConfig{APIKey: "x", Model: "y"})
		if err == nil {
			t.Fatal("ResolveProvider of an unknown name returned no error")
		}
		if !errors.Is(err, llmhub.ErrUnknownProvider) {
			t.Fatalf("error %v does not match ErrUnknownProvider", err)
		}
		if err.Error() != fx.UnknownProvider {
			t.Fatalf("error text = %q, want %q", err.Error(), fx.UnknownProvider)
		}
	})
}
