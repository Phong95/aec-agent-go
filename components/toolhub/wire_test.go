package toolhub_test

import (
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/internal/jsonx"
	"github.com/Phong95/aec-agent-go/internal/wiretest"
	"github.com/Phong95/aec-agent-go/kernel"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// toolWire mirrors the object tool-wire.json holds, fields in fixture order.
type toolWire struct {
	Definitions []toolhub.Definition `json:"definitions"`
	Outputs     []toolhub.Output     `json:"outputs"`
	Decisions   []toolhub.Decision   `json:"decisions"`
}

func TestToolWireFixture(t *testing.T) {
	var w toolWire
	wiretest.AssertRoundTrip(t, "tool-wire.json", &w)
	if len(w.Definitions) != 4 || len(w.Outputs) != 4 || len(w.Decisions) != 3 {
		t.Fatalf("fixture shape changed: %d definitions, %d outputs, %d decisions", len(w.Definitions), len(w.Outputs), len(w.Decisions))
	}

	// A sandbox tool definition keeps its risk and sandbox operation.
	if d := w.Definitions[3]; d.Risk != toolhub.RiskReadOnly || d.SandboxOperation != sandboxhub.OperationFSRead {
		t.Errorf("definition 3 risk %q sandboxOperation %q, want read-only and fs.read", d.Risk, d.SandboxOperation)
	}

	// The explicit empty values must survive as empty, not become absent.
	sink := w.Definitions[2]
	nested, ok := sink.Parameters.Properties.Get("nested")
	if !ok || nested.Required == nil || len(nested.Required) != 0 {
		t.Errorf("nested required: [] must decode as an empty non-nil slice, got %#v", nested.Required)
	}
	mode, ok := sink.Parameters.Properties.Get("mode")
	if !ok || mode.Enum == nil || len(mode.Enum) != 0 {
		t.Errorf("enum: [] must decode as an empty non-nil slice, got %#v", mode.Enum)
	}
	if w.Outputs[2].IsError == nil || *w.Outputs[2].IsError {
		t.Error("an explicit isError:false must survive")
	}
	if w.Outputs[3].Metadata.IsZero() {
		t.Error("an explicit empty metadata object must survive")
	}

	// Metadata keeps the authored (non-sorted) key order at every nesting level.
	md := w.Outputs[1].Metadata
	if got := md.Keys(); !slices.Equal(got, []string{"bytes", "artifactId", "nested"}) {
		t.Errorf("metadata keys = %v, want [bytes artifactId nested]", got)
	}
	if v, ok := md.Get("nested"); !ok {
		t.Error("nested metadata missing")
	} else if n, ok := v.(ordered.Object); !ok || !slices.Equal(n.Keys(), []string{"z", "a"}) {
		t.Errorf("nested metadata = %#v, want an ordered.Object with keys [z a]", v)
	}
	if w.Decisions[2].Reason == nil || *w.Decisions[2].Reason != "" {
		t.Error("an explicit empty reason must survive")
	}
}

// toolInteractionWire mirrors the object tool-interaction-wire.json holds,
// fields in fixture order.
type toolInteractionWire struct {
	ClarifyRequest              toolhub.ClarifyRequest   `json:"clarifyRequest"`
	ClarifyRequestMinimal       toolhub.ClarifyRequest   `json:"clarifyRequestMinimal"`
	ClarifyResponse             toolhub.ClarifyResponse  `json:"clarifyResponse"`
	ApprovalRequest             toolhub.ApprovalRequest  `json:"approvalRequest"`
	ApprovalRequestMinimal      toolhub.ApprovalRequest  `json:"approvalRequestMinimal"`
	ApprovalDecision            toolhub.ApprovalDecision `json:"approvalDecision"`
	ApprovalDecisionEmptyReason toolhub.ApprovalDecision `json:"approvalDecisionEmptyReason"`
	ApprovalDecisionBare        toolhub.ApprovalDecision `json:"approvalDecisionBare"`
}

func TestToolInteractionWireFixture(t *testing.T) {
	var w toolInteractionWire
	wiretest.AssertRoundTrip(t, "tool-interaction-wire.json", &w)

	if len(w.ClarifyRequest.Choices) != 2 || w.ClarifyRequestMinimal.Choices != nil || w.ClarifyRequestMinimal.AgentID != "" {
		t.Errorf("clarify request choices/agent: %+v / %+v", w.ClarifyRequest, w.ClarifyRequestMinimal)
	}

	// The approval arguments are the raw bytes: non-sorted keys and a raw < survive.
	if got, want := string(w.ApprovalRequest.Arguments), `{"zeta":1,"alpha":"a<b"}`; got != want {
		t.Errorf("approval arguments = %s, want %s", got, want)
	}
	if w.ApprovalRequest.Sandbox == nil || w.ApprovalRequest.Sandbox.Kind != sandboxhub.KindDocker || !w.ApprovalRequest.Sandbox.Isolated {
		t.Errorf("approval sandbox: %+v", w.ApprovalRequest.Sandbox)
	}
	if w.ApprovalRequestMinimal.Sandbox != nil || w.ApprovalRequestMinimal.Reason != "" {
		t.Errorf("minimal approval request must carry no optional fields: %+v", w.ApprovalRequestMinimal)
	}
	if r := w.ApprovalDecision.Reason; r == nil || *r != "not now" || w.ApprovalDecision.Approved {
		t.Errorf("approval decision: %+v", w.ApprovalDecision)
	}
	if r := w.ApprovalDecisionEmptyReason.Reason; r == nil || *r != "" {
		t.Error("an explicit empty reason must survive")
	}
	if w.ApprovalDecisionBare.Reason != nil || !w.ApprovalDecisionBare.Approved {
		t.Errorf("a bare decision must carry no reason: %+v", w.ApprovalDecisionBare)
	}
}

// toolFixtures is every golden fixture owned by package tool.
var toolFixtures = []string{
	"tool-wire.json",
	"tool-hub-errors.json",
	"tool-policy-decisions.json",
	"tool-interaction-wire.json",
}

var toolFixturePrefixes = []string{"tool-wire", "tool-hub-", "tool-policy-", "tool-interaction-"}

// TestToolFixturesCovered fails when a fixture with a tool prefix has no entry
// above, and when an entry has no file, so a fixture added to wiregen cannot go
// unchecked.
func TestToolFixturesCovered(t *testing.T) {
	entries, err := os.ReadDir(wiretest.Dir(t))
	if err != nil {
		t.Fatalf("read fixture dir: %v", err)
	}
	present := map[string]bool{}
	for _, e := range entries {
		present[e.Name()] = true
		for _, p := range toolFixturePrefixes {
			if strings.HasPrefix(e.Name(), p) && !slices.Contains(toolFixtures, e.Name()) {
				t.Errorf("fixture %s has no entry in toolFixtures", e.Name())
			}
		}
	}
	for _, f := range toolFixtures {
		if !present[f] {
			t.Errorf("toolFixtures entry %s has no fixture file", f)
		}
	}
}

func encode(t *testing.T, v any) string {
	t.Helper()
	b, err := kernel.EncodeJSON(v)
	if err != nil {
		t.Fatalf("encode %T: %v", v, err)
	}
	return string(b)
}

func TestJSONSchemaEmptyVersusNil(t *testing.T) {
	if got := encode(t, toolhub.JSONSchema{Type: "string"}); got != `{"type":"string"}` {
		t.Errorf("nil required and enum must be omitted: %s", got)
	}
	empty := toolhub.JSONSchema{Type: "string", Required: []string{}, Enum: []string{}}
	if got := encode(t, empty); got != `{"type":"string","required":[],"enum":[]}` {
		t.Errorf("empty required and enum must be kept: %s", got)
	}
	full := toolhub.JSONSchema{Type: "string", Enum: []string{"a"}, Description: "d"}
	if got := encode(t, full); got != `{"type":"string","enum":["a"],"description":"d"}` {
		t.Errorf("field order must be type, properties, required, items, enum, description: %s", got)
	}

	// The same distinction must survive decoding.
	var s toolhub.JSONSchema
	if err := jsonx.UnmarshalStrict([]byte(`{"type":"string","enum":[]}`), &s); err != nil {
		t.Fatal(err)
	}
	if s.Enum == nil || s.Required != nil {
		t.Errorf("decoded enum nil=%v required nil=%v, want enum non-nil and required nil", s.Enum == nil, s.Required == nil)
	}
}

func TestPropertiesKeepDecodeOrder(t *testing.T) {
	in := `{"b":{"type":"string"},"a":{"type":"number"}}`
	var p toolhub.Properties
	if err := jsonx.UnmarshalStrict([]byte(in), &p); err != nil {
		t.Fatal(err)
	}
	if got := p.Names(); !slices.Equal(got, []string{"b", "a"}) {
		t.Fatalf("Names = %v, want [b a]", got)
	}
	if got := encode(t, &p); got != in {
		t.Fatalf("re-encode = %s, want %s", got, in)
	}
}

func TestPropertiesConstructionAndNilSafety(t *testing.T) {
	p := toolhub.NewProperties(
		toolhub.Property{Name: "x", Schema: toolhub.JSONSchema{Type: "string"}},
		toolhub.Property{Name: "y", Schema: toolhub.JSONSchema{Type: "number"}},
		toolhub.Property{Name: "x", Schema: toolhub.JSONSchema{Type: "boolean"}}, // replaces, keeps position
	)
	if got := p.Names(); !slices.Equal(got, []string{"x", "y"}) {
		t.Fatalf("Names = %v", got)
	}
	if s, ok := p.Get("x"); !ok || s.Type != "boolean" {
		t.Fatalf("x = %+v, %v", s, ok)
	}
	var names []string
	for name := range p.All() {
		names = append(names, name)
	}
	if !slices.Equal(names, []string{"x", "y"}) || p.Len() != 2 || !p.Has("y") {
		t.Fatalf("All = %v Len = %d", names, p.Len())
	}

	var nilProps *toolhub.Properties
	if nilProps.Len() != 0 || nilProps.Has("x") || nilProps.Names() != nil {
		t.Fatal("a nil *Properties must read as empty")
	}
	for range nilProps.All() {
		t.Fatal("a nil *Properties must yield nothing")
	}

	var zero toolhub.Properties
	zero.Set("k", toolhub.JSONSchema{Type: "string"})
	if zero.Len() != 1 {
		t.Fatalf("zero value Set: Len = %d", zero.Len())
	}
}
