package llmhub_test

import (
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/internal/jsonx"
	"github.com/Phong95/aec-agent-go/internal/wiretest"
	"github.com/Phong95/aec-agent-go/kernel"
)

// llmWire mirrors the object llm-wire.json holds, fields in fixture order.
type llmWire struct {
	ModelInfo        llmhub.ModelInfo      `json:"modelInfo"`
	ModelInfoMinimal llmhub.ModelInfo      `json:"modelInfoMinimal"`
	ModelRecord      llmhub.ModelRecord    `json:"modelRecord"`
	ProviderConfig   llmhub.ProviderConfig `json:"providerConfig"`
	ModelOptions     llmhub.ModelOptions   `json:"modelOptions"`
}

func TestLLMWireFixture(t *testing.T) {
	var w llmWire
	wiretest.AssertRoundTrip(t, "llm-wire.json", &w)

	// raw keeps the exact bytes: keys not sorted, a nested array, an inner null.
	const wantRaw = `{"zeta":1,"alpha":[1,{"b":2,"a":1},"x"],"mid":{"z":true,"a":null}}`
	if got := string(w.ModelInfo.Raw); got != wantRaw {
		t.Errorf("raw = %s, want %s", got, wantRaw)
	}
	// An explicit false is a reported value and must survive.
	caps := w.ModelInfo.Capabilities
	if caps == nil || caps.Reasoning == nil || *caps.Reasoning || caps.Vision == nil || !*caps.Vision {
		t.Errorf("capabilities: %+v", caps)
	}
	if w.ModelInfo.Pricing == nil || w.ModelInfo.Pricing.OutputUSDPerMillionTokens == nil || *w.ModelInfo.Pricing.OutputUSDPerMillionTokens != 15.5 {
		t.Errorf("pricing: %+v", w.ModelInfo.Pricing)
	}
	if w.ModelInfo.ContextWindowTokens == nil || *w.ModelInfo.ContextWindowTokens != 200000 {
		t.Errorf("contextWindowTokens: %v", w.ModelInfo.ContextWindowTokens)
	}

	// The minimal info carries nothing but id and provider.
	if m := w.ModelInfoMinimal; m.ID != "mystery-model" || m.Provider != "novita" || m.Raw != nil ||
		m.Capabilities != nil || m.Pricing != nil || m.InputModalities != nil || m.ContextWindowTokens != nil {
		t.Errorf("minimal info is not minimal: %+v", m)
	}

	// The provider config holds every field and the oauth auth type.
	c := w.ProviderConfig
	if c.AuthType != llmhub.AuthOAuth || c.TokenExpiresAt == nil || *c.TokenExpiresAt != 1700000000000 ||
		c.ChatgptAccountID != "acct-1" || c.Region != "us-east-1" {
		t.Errorf("provider config: %+v", c)
	}
}

func TestToModelRecordDropsRaw(t *testing.T) {
	var fixture struct {
		ModelRecord json.RawMessage `json:"modelRecord"`
	}
	// Lenient on purpose: only the one key is wanted, as the recorded model-record
	// bytes.
	if err := json.Unmarshal(wiretest.Load(t, "llm-wire.json"), &fixture); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	var w llmWire
	if err := jsonx.UnmarshalStrict(wiretest.Load(t, "llm-wire.json"), &w); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	if w.ModelInfo.Raw == nil {
		t.Fatal("the full model info must carry raw")
	}

	record := llmhub.ToModelRecord(w.ModelInfo)
	got, err := kernel.EncodeJSON(record)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(fixture.ModelRecord) {
		t.Fatalf("ToModelRecord encodes to\n%s\nwant the real toLLMModelRecord output\n%s", got, fixture.ModelRecord)
	}
	if strings.Contains(string(got), `"raw"`) {
		t.Fatalf("a record must not carry raw: %s", got)
	}
	// The info it was made from is untouched.
	if w.ModelInfo.Raw == nil {
		t.Fatal("ToModelRecord changed its argument")
	}
}

func TestModelOptionsZeroValues(t *testing.T) {
	var w llmWire
	wiretest.AssertRoundTrip(t, "llm-wire.json", &w)
	o := w.ModelOptions
	if o.MaxTokens == nil || *o.MaxTokens != 0 || o.Temperature == nil || *o.Temperature != 0 || o.TopP == nil || *o.TopP != 1 {
		t.Fatalf("explicit zero values were lost: %+v", o)
	}
	if o.StopSequences == nil || len(o.StopSequences) != 0 {
		t.Fatalf("stopSequences: [] must decode as an empty non-nil slice, got %#v", o.StopSequences)
	}
	if len(o.Tools) != 1 || o.Tools[0].Name != "lookup" {
		t.Fatalf("tools: %+v", o.Tools)
	}

	got, err := kernel.EncodeJSON(o)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"maxTokens":0`, `"temperature":0`, `"topP":1`, `"stopSequences":[]`} {
		if !strings.Contains(string(got), want) {
			t.Errorf("encoded options lack %s: %s", want, got)
		}
	}

	// Unset stays unset: nothing is invented for the zero value.
	empty, err := kernel.EncodeJSON(llmhub.ModelOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if string(empty) != `{}` {
		t.Fatalf("zero ModelOptions encodes to %s, want {}", empty)
	}
}

// llmFixtures is every golden fixture owned by package llm: llm-hub-errors.json is
// checked by the hub and registry tests, llm-wire.json by the tests above, and
// llm-tool-arguments.json by TestParseToolArgumentsMatchesGolden and
// llm-count-tokens.json by TestEstimateTokensMatchesGolden and llm-model-info.json by
// TestModelMapMatchesGolden.
var llmFixtures = []string{
	"llm-count-tokens.json",
	"llm-hub-errors.json",
	"llm-model-info.json",
	"llm-tool-arguments.json",
	"llm-wire.json",
}

// providerFixturePrefixes are the llm- fixture name prefixes owned by the
// provider leaf packages (llm/anthropic, llm/openaicompat, llm/bedrock,
// llm/openaisub), each of which has its own covered test. TestLLMFixturesCovered
// skips them so a provider plan adds its fixtures without editing this file.
var providerFixturePrefixes = []string{"llm-anthropic-", "llm-openai-", "llm-bedrock-", "llm-subscription-"}

// TestLLMFixturesCovered fails when a fixture with the llm- prefix has no entry
// above, and when an entry has no file, so a fixture added to wiregen cannot go
// unchecked.
func TestLLMFixturesCovered(t *testing.T) {
	entries, err := os.ReadDir(wiretest.Dir(t))
	if err != nil {
		t.Fatalf("read fixture dir: %v", err)
	}
	present := map[string]bool{}
	for _, e := range entries {
		present[e.Name()] = true
		if hasAnyPrefix(e.Name(), providerFixturePrefixes) {
			continue
		}
		if strings.HasPrefix(e.Name(), "llm-") && !slices.Contains(llmFixtures, e.Name()) {
			t.Errorf("fixture %s has no entry in llmFixtures", e.Name())
		}
	}
	for _, f := range llmFixtures {
		if !present[f] {
			t.Errorf("llmFixtures entry %s has no fixture file", f)
		}
	}
}

// hasAnyPrefix reports whether name starts with one of prefixes.
func hasAnyPrefix(name string, prefixes []string) bool {
	return slices.ContainsFunc(prefixes, func(p string) bool { return strings.HasPrefix(name, p) })
}
