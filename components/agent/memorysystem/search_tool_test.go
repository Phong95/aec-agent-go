package memorysystem

import (
	"strings"
	"testing"
	"time"

	"github.com/Phong95/aec-agent-go/components/agent/memorysystem/memory"
	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/components/persistence"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/kernel"
)

// searchSystem is a memory system of agent a1 holding one remembered preference.
func searchSystem(t *testing.T, cfg memory.Config) *System {
	t.Helper()
	hub := persistence.NewPersistenceHub()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	rec := memory.Record{
		ID: "m1", AgentID: "a1", Kind: memory.KindFact, Scope: memory.ScopeUser, Subject: "user",
		Key: "response_language", Value: "Reply in Vietnamese", Status: memory.StatusActive,
		Confidence: memory.ConfidenceExplicit, Source: "conversation", SourceTrust: memory.TrustUserApproved,
		ObservedAt: now, UpdatedAt: now,
	}
	if err := hub.Memory.Save(t.Context(), rec); err != nil {
		t.Fatal(err)
	}
	sys, err := New(Deps{AgentID: "a1", Persistence: hub, Events: events.New(), Model: newFakeModel()}, cfg, WithLocation(time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	return sys
}

// TestSearchTool_OfferedOnlyWhileMemoryIsOn: memory_search is in the model's tools while
// memory and recall are on, and gone when memory is off or recall.maxResults is 0.
func TestSearchTool_OfferedOnlyWhileMemoryIsOn(t *testing.T) {
	zero := 0
	for name, tc := range map[string]struct {
		cfg  memory.Config
		want int
	}{
		"on":          {memory.Config{Enabled: true}, 1},
		"memory off":  {memory.Config{Enabled: false}, 0},
		"recall at 0": {memory.Config{Enabled: true, Recall: &memory.RecallConfig{MaxResults: &zero}}, 0},
	} {
		t.Run(name, func(t *testing.T) {
			defs, err := searchSystem(t, tc.cfg).ToolDefinitions(t.Context())
			if err != nil || len(defs) != tc.want {
				t.Fatalf("ToolDefinitions = %v, %v, want %d", defs, err, tc.want)
			}
			if tc.want == 1 && (defs[0].Name != SearchToolName || defs[0].Risk != toolhub.RiskReadOnly) {
				t.Fatalf("definition = %+v, want read-only %s", defs[0], SearchToolName)
			}
		})
	}
}

// TestSearchTool_RecallsOnTheQuery: a call returns the recalled memories for its query,
// "no memories" for a miss, and an error result for an empty query.
func TestSearchTool_RecallsOnTheQuery(t *testing.T) {
	sys := searchSystem(t, memory.Config{Enabled: true})
	h, ok := sys.Tool(SearchToolName)
	if !ok {
		t.Fatal("Tool(memory_search) not found")
	}
	if _, ok := sys.Tool("skill_view"); ok {
		t.Fatal("Tool returned a handler for another name")
	}
	tc := toolhub.Context{SessionID: "s1", AgentID: "a1"}

	out, err := h.Execute(t.Context(), kernel.Arguments(`{"query":"response language"}`), tc)
	if err != nil || out.IsError != nil {
		t.Fatalf("Execute = %+v, %v", out, err)
	}
	if !strings.Contains(out.Content, "Reply in Vietnamese") || !strings.Contains(out.Content, recallHeading) {
		t.Fatalf("content = %q, want the recalled preference", out.Content)
	}

	out, err = h.Execute(t.Context(), kernel.Arguments(`{"query":"   "}`), tc)
	if err != nil || out.IsError == nil || !*out.IsError {
		t.Fatalf("empty query = %+v, %v, want an error result", out, err)
	}
}
