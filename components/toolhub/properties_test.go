package toolhub_test

import (
	"encoding/json"
	"testing"

	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/internal/clonecheck"
	"github.com/Phong95/aec-agent-go/kernel"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// An empty property set is a valid schema ({"type":"object","properties":{}}
// is what a no-argument tool declares). It must decode, clone and encode without
// a panic, and write "properties":{} byte for byte as JSON.stringify does.
func TestEmptyPropertiesDecodeCloneEncode(t *testing.T) {
	const want = `{"type":"object","properties":{}}`

	var decoded toolhub.JSONSchema
	if err := json.Unmarshal([]byte(want), &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}

	cases := map[string]toolhub.JSONSchema{
		"decoded":               decoded,
		"decoded then cloned":   decoded.Clone(),
		"zero Properties":       {Type: "object", Properties: &toolhub.Properties{}},
		"zero Properties clone": (toolhub.JSONSchema{Type: "object", Properties: &toolhub.Properties{}}).Clone(),
		"NewProperties()":       {Type: "object", Properties: toolhub.NewProperties()},
		"NewProperties clone":   (toolhub.JSONSchema{Type: "object", Properties: toolhub.NewProperties()}).Clone(),
	}
	for name, schema := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := kernel.EncodeJSON(schema)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			if string(got) != want {
				t.Fatalf("encoded = %s, want %s", got, want)
			}
		})
	}
}

// A Definition whose parameters hold an empty set survives the same round trip.
func TestEmptyPropertiesDefinitionClone(t *testing.T) {
	def := toolhub.Definition{
		Name:       "ping",
		Parameters: toolhub.JSONSchema{Type: "object", Properties: &toolhub.Properties{}},
	}
	got, err := kernel.EncodeJSON(def.Clone())
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	const want = `{"name":"ping","description":"","parameters":{"type":"object","properties":{}}}`
	if string(got) != want {
		t.Fatalf("encoded = %s, want %s", got, want)
	}
}

// A nil *Properties on its own marshals as an empty object, whatever its inner
// state.
func TestPropertiesMarshalNilAndZero(t *testing.T) {
	var nilProps *toolhub.Properties
	for name, p := range map[string]*toolhub.Properties{"nil": nilProps, "zero": {}} {
		got, err := p.MarshalJSON()
		if err != nil || string(got) != "{}" {
			t.Fatalf("%s: MarshalJSON = %q, %v", name, got, err)
		}
	}
}

// JSON null leaves a Properties unchanged, like ordered.Map and ordered.Object.
func TestPropertiesUnmarshalNull(t *testing.T) {
	p := toolhub.NewProperties(toolhub.Property{Name: "a", Schema: toolhub.JSONSchema{Type: "string"}})
	if err := p.UnmarshalJSON([]byte("null")); err != nil {
		t.Fatalf("UnmarshalJSON(null) = %v", err)
	}
	if !p.Has("a") {
		t.Fatal("null changed the properties")
	}
}

// Output.Clone must share nothing with the original: the two bool pointers, the
// Link (A-132) and the Metadata object (nested values included).
func TestOutputCloneIsDeep(t *testing.T) {
	isErr, retry := true, false
	meta := ordered.NewObject()
	meta.Set("count", json.Number("2"))
	meta.Set("nested", []any{"a", ordered.NewObject()})
	link := &kernel.ToolResultLink{URI: "artifact://art-1", Name: "a.md", Type: "text/markdown", Version: 1}
	sample := toolhub.Output{Content: "done", IsError: &isErr, Retryable: &retry, Empty: true, Link: link, CompactArguments: kernel.Arguments(`{"content":"[ref]"}`), Metadata: meta}
	clonecheck.AssertDeep(t, sample, toolhub.Output.Clone)
}

// An absent Metadata stays absent, so the clone still omits it on the wire.
func TestOutputCloneKeepsAbsentMetadata(t *testing.T) {
	got := toolhub.Output{Content: "x"}.Clone()
	if !got.Metadata.IsZero() || got.IsError != nil || got.Retryable != nil {
		t.Fatalf("Clone invented parts: %#v", got)
	}
}
