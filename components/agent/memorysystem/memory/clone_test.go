package memory_test

import (
	"testing"

	"github.com/Phong95/aec-agent-go/components/agent/memorysystem/memory"
	"github.com/Phong95/aec-agent-go/internal/clonecheck"
)

// Record.Clone must share nothing with the original: a slice field added to
// Record without a Clone line fails here.
func TestRecordCloneIsDeep(t *testing.T) {
	sample := memory.Record{
		ID: "m1", AgentID: "a1", Kind: memory.KindFact, Scope: memory.ScopeProject,
		Subject: "s", Key: "k", Value: "v", Status: memory.StatusActive,
		Confidence: memory.ConfidenceExplicit, Source: "chat", SourceTrust: memory.TrustUserApproved,
		ObservedAt: "2026-10-01T08:00:00.000Z", UpdatedAt: "2026-10-01T09:00:00.000Z",
		LastRecalledAt: "2026-10-01T10:00:00.000Z", SupersededBy: "m2", ExpiresAt: "2027-10-01T00:00:00.000Z",
		Tags: []string{"a", "b"},
	}
	clonecheck.AssertDeep(t, sample, memory.Record.Clone)
}

// Config.Clone must share nothing with the original: every pointer sub-config is
// copied.
func TestConfigCloneIsDeep(t *testing.T) {
	interval, maxResults := 60.0, 8
	sample := memory.Config{
		Enabled: true, UpdateSoul: true, UpdateUser: true,
		Consolidation: &memory.ConsolidationTrigger{IntervalMinutes: &interval},
		Dreaming:      &memory.DreamingConfig{MemoryExpiryDays: 30},
		Recall:        &memory.RecallConfig{MaxResults: &maxResults},
	}
	clonecheck.AssertDeep(t, sample, memory.Config.Clone)
}

// A nil Tags stays nil (omitzero drops it) and an empty one stays empty.
func TestRecordCloneKeepsNilVersusEmpty(t *testing.T) {
	if got := (memory.Record{}).Clone(); got.Tags != nil {
		t.Fatalf("nil Tags must stay nil: %#v", got.Tags)
	}
	if got := (memory.Record{Tags: []string{}}).Clone(); got.Tags == nil {
		t.Fatal("empty Tags must stay non-nil")
	}
}

// RecalledMemories.Clone must share nothing with the original: the content pointer,
// both slices and the two optional numbers are copied.
func TestRecalledMemoriesCloneIsDeep(t *testing.T) {
	content, ms, n := "## Recalled memories", 12.5, 40
	sample := memory.RecalledMemories{
		Content:   &content,
		RecordIDs: []string{"m1", "m2"},
		Memories: []memory.RecalledMemoryTrace{
			{ID: "m1", Kind: "fact", Key: "k1", CombinedScore: 0.9, RelevanceScore: 0.8, RecencyScore: 0.7, ImportanceScore: 0.6},
		},
		MaxResults: 8, IndexRebuilt: true, Skipped: memory.SkippedMemoryDisabled,
		IndexRebuildMs: &ms, IndexedCount: &n,
	}
	clonecheck.AssertDeep(t, sample, memory.RecalledMemories.Clone)
}

// A nil Content, nil slices and nil numbers stay nil.
func TestRecalledMemoriesCloneKeepsNil(t *testing.T) {
	got := (memory.RecalledMemories{}).Clone()
	if got.Content != nil || got.RecordIDs != nil || got.Memories != nil || got.IndexRebuildMs != nil || got.IndexedCount != nil {
		t.Fatalf("a zero value gained a part: %#v", got)
	}
}
