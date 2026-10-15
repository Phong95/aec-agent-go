package aecmock_test

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/Phong95/aec-agent-go/aecmock"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/internal/jsonx"
	"github.com/Phong95/aec-agent-go/internal/wiretest"
	"github.com/Phong95/aec-agent-go/kernel"
)

// mockRun is one recorded execute call: the tool, its arguments and its output.
type mockRun struct {
	Tool   string           `json:"tool"`
	Args   kernel.Arguments `json:"args"`
	Output toolhub.Output   `json:"output"`
}

// mockFixture mirrors mock-tools.json. The definitions stay raw: DocumentSearch
// is equal to the fixture as JSON but not byte for byte, so the fixture as a whole is
// deliberately not round-tripped through typed structs.
type mockFixture struct {
	Definitions map[string]json.RawMessage `json:"definitions"`
	Outputs     []mockRun                  `json:"outputs"`
}

func loadMockFixture(t *testing.T) mockFixture {
	t.Helper()
	var f mockFixture
	if err := jsonx.UnmarshalStrict(wiretest.Load(t, "mock-tools.json"), &f); err != nil {
		t.Fatalf("decode mock-tools.json: %v", err)
	}
	return f
}

// mockTools returns the four tools; send_rfi draws 450, the draw the recorded
// fixture was made with (floor(0.5 * 900) + 100 = 550).
func mockTools() map[string]toolhub.Handler {
	return map[string]toolhub.Handler{
		"schedule_lookup": &aecmock.ScheduleLookupTool{},
		"budget_query":    &aecmock.BudgetQueryTool{},
		"document_search": &aecmock.DocumentSearchTool{},
		"send_rfi":        &aecmock.SendRfiTool{RandIntn: func(int) int { return 450 }},
	}
}

func TestMockToolOutputsMatchGolden(t *testing.T) {
	f := loadMockFixture(t)
	tools := mockTools()
	if len(f.Outputs) != 8 {
		t.Fatalf("fixture shape changed: %d outputs", len(f.Outputs))
	}
	seen := map[string]int{}
	for i, run := range f.Outputs {
		h, ok := tools[run.Tool]
		if !ok {
			t.Fatalf("output %d names unknown tool %q", i, run.Tool)
		}
		seen[run.Tool]++
		before := string(run.Args)
		got, err := h.Execute(t.Context(), run.Args, toolhub.Context{SessionID: "session-1"})
		if err != nil {
			t.Fatalf("output %d (%s %s): %v", i, run.Tool, run.Args, err)
		}
		if got.Content != run.Output.Content {
			t.Errorf("output %d (%s %s):\n got %s\nwant %s", i, run.Tool, run.Args, got.Content, run.Output.Content)
		}
		if got.IsError != nil || got.Retryable != nil || !got.Metadata.IsZero() {
			t.Errorf("output %d: a mock tool sets only the content: %+v", i, got)
		}
		if string(run.Args) != before {
			t.Errorf("output %d: Execute changed the argument bytes", i)
		}
	}
	for name := range tools {
		if seen[name] == 0 {
			t.Errorf("no fixture output for tool %s", name)
		}
	}
	// The send_rfi id is the patched-random id and the status keeps its em dash.
	var rfi struct {
		Status string `json:"status"`
		RfiID  string `json:"rfiId"`
	}
	for _, run := range f.Outputs {
		if run.Tool != "send_rfi" {
			continue
		}
		if err := json.Unmarshal([]byte(run.Output.Content), &rfi); err != nil {
			t.Fatal(err)
		}
		if rfi.RfiID != "RFI-550" || !strings.Contains(rfi.Status, string([]byte{0xe2, 0x80, 0x94})) {
			t.Errorf("send_rfi fixture: %+v", rfi)
		}
	}
}

// TestMockToolOutputsArgumentEdgeCases covers inputs the fixture does not hold:
// arguments of other JSON types are echoed the way JSON.stringify would, and a
// non-object argument is an error, never a panic.
func TestMockToolOutputsArgumentEdgeCases(t *testing.T) {
	sched := &aecmock.ScheduleLookupTool{}
	args, err := kernel.NewArguments([]byte(`{"projectId":{"b":2,"a":[1,1.50,"<"]},"scope":7}`))
	if err != nil {
		t.Fatal(err)
	}
	out, err := sched.Execute(t.Context(), args, toolhub.Context{})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"project":{"a":[1,1.5,"<"],"b":2},"scope":7,"tasks":`; !strings.HasPrefix(out.Content, want) {
		t.Errorf("content = %s, want prefix %s", out.Content, want)
	}

	// No arguments at all: project is omitted, scope defaults.
	out, err = sched.Execute(t.Context(), nil, toolhub.Context{})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"scope":"all","tasks":`; !strings.HasPrefix(out.Content, want) {
		t.Errorf("content = %s, want prefix %s", out.Content, want)
	}

	for name, h := range mockTools() {
		if _, err := h.Execute(t.Context(), kernel.Arguments("[1]"), toolhub.Context{}); err == nil {
			t.Errorf("%s: a non-object argument must be an error", name)
		}
	}
}

func TestMockToolDefinitionsMatchGolden(t *testing.T) {
	f := loadMockFixture(t)
	tools := mockTools()
	if len(f.Definitions) != len(tools) {
		t.Fatalf("fixture holds %d definitions, want %d", len(f.Definitions), len(tools))
	}
	for name, h := range tools {
		want, ok := f.Definitions[name]
		if !ok {
			t.Errorf("no fixture definition for %s", name)
			continue
		}
		got, err := kernel.EncodeJSON(h.Definition())
		if err != nil {
			t.Fatal(err)
		}
		// RISK-UNSET: the approval gate treats a tool without a Risk as consequential,
		// so the read-only mock tools declare "read-only" explicitly; the fixture leaves
		// it unset. The expected bytes are the fixture definition plus that one field.
		if h.Definition().Risk == toolhub.RiskReadOnly && !bytes.Contains(want, []byte(`"risk"`)) {
			want = append(bytes.TrimSuffix(bytes.Clone(want), []byte("}")), []byte(`,"risk":"read-only"}`)...)
		}
		if name == "document_search" {
			// The fixture has docType as type, description, enum; Go encodes type, enum,
			// description (the JSONSchema field order). The two are equal as JSON, not
			// byte for byte.
			var g, w any
			if err := json.Unmarshal(got, &g); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(want, &w); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(g, w) {
				t.Errorf("%s differs as JSON:\n got %s\nwant %s", name, got, want)
			}
			if string(got) == string(want) {
				t.Errorf("%s is byte-identical now: drop the key-order special case", name)
			}
			continue
		}
		if string(got) != string(want) {
			t.Errorf("%s:\n got %s\nwant %s", name, got, want)
		}
	}
}

func TestMockToolIdentity(t *testing.T) {
	want := map[string]string{
		"schedule_lookup": "tool:schedule-lookup",
		"budget_query":    "tool:budget-query",
		"document_search": "tool:document-search",
		"send_rfi":        "tool:send-rfi",
	}
	for name, h := range mockTools() {
		if h.Name() != want[name] || h.Version() != "1.0.0" {
			t.Errorf("%s: Name %q Version %q", name, h.Name(), h.Version())
		}
		if h.Definition().Name != name {
			t.Errorf("%s: definition name %q", name, h.Definition().Name)
		}
		if err := kernel.Initialize(t.Context(), h); err != nil {
			t.Errorf("%s: Initialize: %v", name, err)
		}
		if err := kernel.Dispose(t.Context(), h); err != nil {
			t.Errorf("%s: Dispose: %v", name, err)
		}
	}
	if def := (&aecmock.SendRfiTool{}).Definition(); def.Risk != toolhub.RiskConsequential {
		t.Errorf("send_rfi risk = %q", def.Risk)
	}
}

// TestSendRfiDefaultRandomness proves the zero value (crypto/rand) yields ids in
// the range RFI-100 to RFI-999.
func TestSendRfiDefaultRandomness(t *testing.T) {
	h := &aecmock.SendRfiTool{}
	args, err := kernel.NewArguments([]byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for range 50 {
		out, err := h.Execute(t.Context(), args, toolhub.Context{})
		if err != nil {
			t.Fatal(err)
		}
		var r struct {
			RfiID string `json:"rfiId"`
		}
		if err := json.Unmarshal([]byte(out.Content), &r); err != nil {
			t.Fatal(err)
		}
		if len(r.RfiID) != 7 || !strings.HasPrefix(r.RfiID, "RFI-") || r.RfiID[4] < '1' {
			t.Fatalf("id %q is outside RFI-100..RFI-999", r.RfiID)
		}
		ids[r.RfiID] = true
	}
	if len(ids) < 2 {
		t.Errorf("50 draws gave %d distinct ids", len(ids))
	}
}

var mockFixtures = []string{"mock-tools.json"}

// TestMockFixturesCovered fails when a mock-tool fixture has no entry above and
// when an entry has no file.
func TestMockFixturesCovered(t *testing.T) {
	entries, err := os.ReadDir(wiretest.Dir(t))
	if err != nil {
		t.Fatalf("read fixture dir: %v", err)
	}
	present := map[string]bool{}
	for _, e := range entries {
		present[e.Name()] = true
		if strings.HasPrefix(e.Name(), "mock-tool") && !slices.Contains(mockFixtures, e.Name()) {
			t.Errorf("fixture %s has no entry in mockFixtures", e.Name())
		}
	}
	for _, f := range mockFixtures {
		if !present[f] {
			t.Errorf("mockFixtures entry %s has no fixture file", f)
		}
	}
}
