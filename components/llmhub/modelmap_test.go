package llmhub_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/internal/jsonx"
	"github.com/Phong95/aec-agent-go/internal/wiretest"
)

// modelCase is one entry of llm-model-info.json: a raw catalog object and what
// the reference mapper returned for it.
type modelCase struct {
	Mapper      string          `json:"mapper"`
	Provider    string          `json:"provider"`
	Name        string          `json:"name"`
	Raw         json.RawMessage `json:"raw"`
	GoException string          `json:"goException"`
	Out         json.RawMessage `json:"out"`
}

// loadModelCases decodes the mapper corpus.
func loadModelCases(t *testing.T) []modelCase {
	t.Helper()
	var f struct {
		Entries []modelCase `json:"entries"`
	}
	if err := jsonx.UnmarshalStrict(wiretest.Load(t, "llm-model-info.json"), &f); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	return f.Entries
}

// runMapper runs the named Go mapper over raw. The bool is false only for a
// Bedrock summary that has no modelId.
func runMapper(t *testing.T, mapper string, raw json.RawMessage, provider string) (llmhub.ModelInfo, bool) {
	t.Helper()
	switch mapper {
	case "anthropic":
		return llmhub.MapAnthropicModel(raw, provider), true
	case "openaiCompatible":
		return llmhub.MapOpenAICompatibleModel(raw, provider), true
	case "bedrock":
		return llmhub.MapBedrockFoundationModel(raw, provider)
	case "codex":
		return llmhub.MapCodexModel(raw, provider), true
	}
	t.Fatalf("unknown mapper %q in the fixture", mapper)
	return llmhub.ModelInfo{}, false
}

// TestModelMapMatchesGolden replays llm-model-info.json: every catalog object runs
// through the Go mapper of the same name and must encode to the exact bytes the
// recorded mapper result has. An entry that names a goException is the
// documented exception: it is asserted by that exception's check instead.
func TestModelMapMatchesGolden(t *testing.T) {
	entries := loadModelCases(t)
	if len(entries) == 0 {
		t.Fatal("the corpus is empty")
	}
	for _, e := range entries {
		t.Run(e.Mapper+"/"+e.Name, func(t *testing.T) {
			info, ok := runMapper(t, e.Mapper, e.Raw, e.Provider)
			if e.GoException != "" {
				ex, known := documentedExceptions[e.GoException]
				if !known {
					t.Fatalf("goException %q has no entry in documentedExceptions", e.GoException)
				}
				if !ok {
					t.Fatal("the mapper dropped an entry that has an id")
				}
				if string(info.Raw) != string(e.Raw) {
					t.Fatalf("Raw = %s, want the input bytes %s", info.Raw, e.Raw)
				}
				ex.check(t, info, e)
				return
			}
			if string(e.Out) == "null" {
				if ok {
					t.Fatalf("the mapper returned %+v, TS returned undefined", info)
				}
				return
			}
			if !ok {
				t.Fatalf("the mapper dropped the entry, TS returned %s", e.Out)
			}
			if string(info.Raw) != string(e.Raw) {
				t.Fatalf("Raw = %s, want the input bytes %s", info.Raw, e.Raw)
			}
			got, err := jsonx.Marshal(info)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			if string(got) != string(e.Out) {
				t.Fatalf("Go encodes\n%s\nTS encodes\n%s", got, e.Out)
			}
		})
	}
}

// documentedException is a difference between the Go mapper and the recorded
// fixture output that cannot be removed, with the assertion of what Go does
// instead. A fixture entry names its exception with goException, and the
// recorded output of such an entry is NOT compared byte for byte.
type documentedException struct {
	reason string
	check  func(t *testing.T, got llmhub.ModelInfo, e modelCase)
}

var documentedExceptions = map[string]documentedException{
	// A count above 2^53 is a double in the fixture and prints as its shortest
	// digits (4611686018427388000); Go keeps an int and prints the exact value.
	"large-integer-digits": {
		reason: "an int field prints the exact integer, JS prints the shortest digits of the double",
		check: func(t *testing.T, got llmhub.ModelInfo, _ modelCase) {
			if got.ContextWindowTokens == nil || *got.ContextWindowTokens != 1<<62 {
				t.Fatalf("contextWindowTokens = %v, want 2^62", got.ContextWindowTokens)
			}
		},
	},
	// A count that does not fit an int is kept by the fixture as a double (1e+21).
	"integer-above-maxint": {
		reason: "a value above math.MaxInt does not fit an int field and is absent",
		check: func(t *testing.T, got llmhub.ModelInfo, _ modelCase) {
			if got.ContextWindowTokens != nil || got.MaxOutputTokens != nil {
				t.Fatalf("a count above MaxInt must be absent: %+v", got.ModelRecord)
			}
		},
	},
	// The fixture keeps a reported empty string; a ModelRecord string field has no way
	// to tell an empty value from an absent one (models.go).
	"empty-strings-omitted": {
		reason: "an empty string is the same as absent in a ModelRecord string field",
		check: func(t *testing.T, got llmhub.ModelInfo, e modelCase) {
			gotJSON, err := jsonx.Marshal(got)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			var want, have map[string]any
			if err := json.Unmarshal(e.Out, &want); err != nil {
				t.Fatalf("decode TS output: %v", err)
			}
			if err := json.Unmarshal(gotJSON, &have); err != nil {
				t.Fatalf("decode Go output: %v", err)
			}
			for k, v := range want {
				if v == "" {
					delete(want, k)
				}
			}
			if !reflect.DeepEqual(have, want) {
				t.Fatalf("Go encodes\n%s\nwant the TS output without its empty strings\n%v", gotJSON, want)
			}
		},
	},
	// The recorded mapper throws a RangeError for a time outside the JS Date
	// range; Go must not fail, so the date is absent and the rest is mapped.
	"created-beyond-js-date-range": {
		reason: "TS throws RangeError (Invalid time value); Go leaves createdAt absent",
		check: func(t *testing.T, got llmhub.ModelInfo, e modelCase) {
			if !strings.Contains(string(e.Out), "Invalid time value") {
				t.Fatalf("the fixture no longer records the TS throw: %s", e.Out)
			}
			if got.CreatedAt != "" || got.ID != "m" || got.Provider != "openai" {
				t.Fatalf("want createdAt absent and the other fields mapped: %+v", got.ModelRecord)
			}
		},
	},
}

// TestModelMapCorpusShape fails when a documented exception has no corpus entry
// (so a stale row cannot stay) or the corpus is smaller than the plan promises.
func TestModelMapCorpusShape(t *testing.T) {
	entries := loadModelCases(t)
	used := map[string]bool{}
	mappers := map[string]bool{}
	for _, e := range entries {
		used[e.GoException] = true
		mappers[e.Mapper] = true
	}
	for name, ex := range documentedExceptions {
		if !used[name] {
			t.Errorf("documented exception %q (%s) has no corpus entry", name, ex.reason)
		}
	}
	if len(entries) < 30 || len(mappers) != 4 {
		t.Fatalf("the corpus has %d entries over %d mappers, want at least 30 over 4", len(entries), len(mappers))
	}
}

// everyKey is each key any mapper reads.
var everyKey = []string{
	"id", "display_name", "name", "title", "description", "owned_by", "created", "created_at",
	"context_window", "context_length", "context_size", "max_context_length", "max_model_len", "max_input_tokens",
	"max_output_tokens", "max_completion_tokens", "max_tokens", "max_context_window",
	"input_modalities", "output_modalities", "supported_parameters", "features", "pricing",
	"capabilities", "top_provider", "architecture",
	"modelId", "modelName", "providerName", "inputModalities", "outputModalities", "responseStreamingSupported",
	"slug", "supported_reasoning_levels",
}

// TestModelMapNeverPanics feeds every mapper input that is not an object, and
// objects whose every field has the wrong type: it must neither panic nor return
// an info that cannot be encoded (threat T-08-07).
func TestModelMapNeverPanics(t *testing.T) {
	deep := strings.Repeat("[", 200000) + strings.Repeat("]", 200000)
	nonObjects := []string{
		"", " ", "null", "true", "7", "-0", "1e999", `"text"`, "[]", "[1,2]", "{", "}", "{]", `{"id":`,
		`{"id":"a"} trailing`, `{"id":"a"}{"id":"b"}`, "\xff\xfe", "{\"id\":\"\xff\"}", deep,
	}
	wrongValues := []string{
		"null", "true", "false", "0", "-1", "7", "1.5", "1e999", "-1e999", "1e-999", "1e21", `""`, `"s"`, `"1e3"`,
		`"Infinity"`, "[]", "[1]", `["a",1,null]`, `[["x"]]`, "{}", `{"a":1}`,
		`{"supported":true}`, `{"prompt":"x","completion":[]}`,
	}

	check := func(t *testing.T, raw json.RawMessage) {
		t.Helper()
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("a mapper panicked on %.80q: %v", raw, r)
			}
		}()
		infos := []llmhub.ModelInfo{
			llmhub.MapAnthropicModel(raw, "p"),
			llmhub.MapOpenAICompatibleModel(raw, "p"),
			llmhub.MapCodexModel(raw, "p"),
		}
		if b, ok := llmhub.MapBedrockFoundationModel(raw, "p"); ok {
			infos = append(infos, b)
		}
		for _, info := range infos {
			if info.Provider != "p" {
				t.Fatalf("provider not carried over: %+v", info.ModelRecord)
			}
			if _, err := jsonx.Marshal(info); err != nil {
				t.Fatalf("the info of %.80q cannot be encoded: %v", raw, err)
			}
		}
	}

	t.Run("not an object", func(t *testing.T) {
		for _, in := range nonObjects {
			check(t, json.RawMessage(in))
		}
		check(t, nil)
	})

	t.Run("every field the wrong type", func(t *testing.T) {
		for _, v := range wrongValues {
			parts := make([]string, 0, len(everyKey))
			for _, k := range everyKey {
				parts = append(parts, `"`+k+`":`+v)
			}
			check(t, json.RawMessage("{"+strings.Join(parts, ",")+"}"))
		}
	})

	t.Run("nested paths the wrong type", func(t *testing.T) {
		for _, v := range wrongValues {
			raw := `{"id":"m","top_provider":{"context_length":` + v + `,"max_completion_tokens":` + v +
				`},"architecture":{"input_modalities":` + v + `,"output_modalities":` + v +
				`},"pricing":{"prompt":` + v + `,"completion":` + v + `},"capabilities":{"image_input":` + v +
				`,"pdf_input":` + v + `,"thinking":` + v + `,"structured_outputs":` + v + `}}`
			check(t, json.RawMessage(raw))
		}
	})

	t.Run("Bedrock drops what has no modelId", func(t *testing.T) {
		for _, in := range nonObjects {
			if _, ok := llmhub.MapBedrockFoundationModel(json.RawMessage(in), "p"); ok {
				t.Errorf("%.40q gave an info with no modelId", in)
			}
		}
	})

	t.Run("Raw is a copy of the input", func(t *testing.T) {
		in := []byte(`{"id":"a"}`)
		info := llmhub.MapOpenAICompatibleModel(in, "p")
		in[2] = 'X'
		if string(info.Raw) != `{"id":"a"}` {
			t.Fatalf("Raw follows a change of the input: %s", info.Raw)
		}
	})
}

// TestMapCodexModelReasoningAndTools covers the Codex fields beyond the goldens: the
// reasoning efforts (lowercased, in order, without repeats or empty entries), the
// default effort, and tool calling read from supports_parallel_tool_calls.
func TestMapCodexModelReasoningAndTools(t *testing.T) {
	raw := json.RawMessage(`{"slug":"gpt-6.1-sol","display_name":"GPT-6.1-Sol","context_window":500000,` +
		`"max_context_window":872000,"input_modalities":["text","image"],"supports_parallel_tool_calls":false,` +
		`"default_reasoning_level":"Low","supported_reasoning_levels":[{"effort":"low"},{"effort":"HIGH"},` +
		`{"effort":"high"},{"effort":""},{"description":"no effort"},"xhigh",{"effort":"ultra"}]}`)
	got := llmhub.MapCodexModel(raw, "openai-subscription")

	if want := []string{"low", "high", "ultra"}; !reflect.DeepEqual(got.ReasoningEfforts, want) {
		t.Errorf("ReasoningEfforts = %v, want %v", got.ReasoningEfforts, want)
	}
	if got.DefaultReasoningEffort != "low" {
		t.Errorf("DefaultReasoningEffort = %q, want low", got.DefaultReasoningEffort)
	}
	if got.ContextWindowTokens == nil || *got.ContextWindowTokens != 500000 {
		t.Errorf("ContextWindowTokens = %v, want 500000", got.ContextWindowTokens)
	}
	if c := got.Capabilities; c == nil || c.ToolCalling == nil || !*c.ToolCalling {
		t.Errorf("Capabilities.ToolCalling = %+v, want true", c)
	}
	if got.MaxOutputTokens != nil {
		t.Errorf("MaxOutputTokens = %d, want absent (the Codex list does not report it)", *got.MaxOutputTokens)
	}

	bare := llmhub.MapCodexModel(json.RawMessage(`{"slug":"m","supports_parallel_tool_calls":"yes"}`), "p")
	if bare.ReasoningEfforts != nil || bare.DefaultReasoningEffort != "" || bare.Capabilities != nil {
		t.Errorf("a model without the fields got %+v", bare.ModelRecord)
	}
}
