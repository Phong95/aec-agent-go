package memory_test

import (
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/Phong95/aec-agent-go/components/agent/memorysystem/memory"
	"github.com/Phong95/aec-agent-go/internal/jsonx"
	"github.com/Phong95/aec-agent-go/internal/wiretest"
)

// pipelineWire mirrors the object memory-pipeline.json holds, fields in fixture
// order: a minimal record, the three consolidation shapes, both retrieval query
// forms, a retrieval result with a full record and both feedback signal forms.
type pipelineWire struct {
	RecordMinimal          memory.Record              `json:"recordMinimal"`
	ConsolidationKeep      memory.ConsolidationResult `json:"consolidationKeep"`
	ConsolidationMerge     memory.ConsolidationResult `json:"consolidationMerge"`
	ConsolidationSupersede memory.ConsolidationResult `json:"consolidationSupersede"`
	RetrievalQuery         memory.RetrievalQuery      `json:"retrievalQuery"`
	RetrievalQueryMinimal  memory.RetrievalQuery      `json:"retrievalQueryMinimal"`
	RetrievalResult        memory.RetrievalResult     `json:"retrievalResult"`
	FeedbackSignal         memory.FeedbackSignal      `json:"feedbackSignal"`
	FeedbackSignalMinimal  memory.FeedbackSignal      `json:"feedbackSignalMinimal"`
}

// The pipeline types decode strictly and re-encode byte-identical, an explicit
// zero maxResults survives and a minimal record omits every optional key.
func TestMemoryPipelineFixture(t *testing.T) {
	var w pipelineWire
	wiretest.AssertRoundTrip(t, "memory-pipeline.json", &w)

	if w.RetrievalQuery.MaxResults == nil || *w.RetrievalQuery.MaxResults != 0 {
		t.Errorf("explicit maxResults 0 lost: %v", w.RetrievalQuery.MaxResults)
	}
	if w.RetrievalQueryMinimal.MaxResults != nil || w.RetrievalQueryMinimal.Kinds != nil || w.RetrievalQueryMinimal.Scope != "" {
		t.Errorf("minimal query gained fields: %+v", w.RetrievalQueryMinimal)
	}
	full := w.RetrievalResult.Record
	if full.SourceTrust != memory.TrustUserApproved || full.UpdatedAt == "" || full.LastRecalledAt == "" || full.SupersededBy == "" || full.ExpiresAt == "" || len(full.Tags) != 2 {
		t.Errorf("full record lost a field: %+v", full)
	}
	b, err := jsonx.Marshal(w.RecordMinimal)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"updatedAt", "lastRecalledAt", "supersededBy", "expiresAt", "tags"} {
		if strings.Contains(string(b), `"`+key+`"`) {
			t.Errorf("a minimal record wrote the optional key %s: %s", key, b)
		}
	}
	if w.ConsolidationMerge.Action != memory.ActionMerge || w.FeedbackSignal.SignalType != memory.SignalTaskSucceeded {
		t.Errorf("enum values lost: %+v %+v", w.ConsolidationMerge, w.FeedbackSignal)
	}
}

// memoryFixtures is every golden fixture owned by package memory.
var memoryFixtures = []string{
	"memory-pipeline.json",
}

// TestMemoryFixturesCovered fails when a fixture with the memory- prefix has no
// entry above, and when an entry has no file, so a fixture added to wiregen
// cannot go unchecked.
func TestMemoryFixturesCovered(t *testing.T) {
	entries, err := os.ReadDir(wiretest.Dir(t))
	if err != nil {
		t.Fatalf("read fixture dir: %v", err)
	}
	present := map[string]bool{}
	for _, e := range entries {
		present[e.Name()] = true
		if strings.HasPrefix(e.Name(), "memory-") && !slices.Contains(memoryFixtures, e.Name()) {
			t.Errorf("fixture %s has no entry in memoryFixtures", e.Name())
		}
	}
	for _, f := range memoryFixtures {
		if !present[f] {
			t.Errorf("memoryFixtures entry %s has no fixture file", f)
		}
	}
}

// recalledWire is the recorded recallForTurn output, in its runtime key order: a hit
// with a rebuilt index, a hit without a rebuild, nothing found, and both skip reasons
// (which put skipped after indexRebuilt). Each value is written without whitespace so
// that it is also the expected encoding.
const recalledWire = `{
"hit":{"content":"## Recalled memories\nLong-term memories.\n- [fact] Project - deadline: Friday","recordIds":["m1","m2"],"memories":[{"id":"m1","kind":"fact","key":"deadline","combinedScore":0.91,"relevanceScore":0.8,"recencyScore":0.95,"importanceScore":0.7},{"id":"m2","kind":"preference","key":"tone","combinedScore":0.5,"relevanceScore":0.4,"recencyScore":0.6,"importanceScore":0.5}],"maxResults":8,"indexRebuilt":true,"indexRebuildMs":12.5,"indexedCount":40},
"warm":{"content":"## Recalled memories","recordIds":["m3"],"memories":[{"id":"m3","kind":"fact","key":"k","combinedScore":1,"relevanceScore":1,"recencyScore":1,"importanceScore":1}],"maxResults":3,"indexRebuilt":false},
"nothing":{"content":null,"recordIds":[],"memories":[],"maxResults":8,"indexRebuilt":false},
"memoryDisabled":{"content":null,"recordIds":[],"memories":[],"maxResults":8,"indexRebuilt":false,"skipped":"memory-disabled"},
"recallDisabled":{"content":null,"recordIds":[],"memories":[],"maxResults":0,"indexRebuilt":false,"skipped":"recall-disabled"}
}`

// TestRecalledMemoriesWire: a recall result decodes strictly and encodes back to the
// same bytes, content stays null (never omitted), the id and memory lists stay
// arrays, and the optional keys appear only when set. The bytes are the runtime
// shape of the recorded recallForTurn output.
func TestRecalledMemoriesWire(t *testing.T) {
	var all map[string]json.RawMessage
	if err := json.Unmarshal([]byte(recalledWire), &all); err != nil {
		t.Fatal(err)
	}
	decoded := map[string]memory.RecalledMemories{}
	for _, name := range []string{"hit", "warm", "nothing", "memoryDisabled", "recallDisabled"} {
		var r memory.RecalledMemories
		if err := jsonx.UnmarshalStrict(all[name], &r); err != nil {
			t.Fatalf("%s: decode: %v", name, err)
		}
		got, err := jsonx.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(all[name]) {
			t.Errorf("%s: re-encode differs\n got: %s\nwant: %s", name, got, all[name])
		}
		decoded[name] = r
	}

	hit, nothing, off := decoded["hit"], decoded["nothing"], decoded["recallDisabled"]
	if hit.Content == nil || len(hit.Memories) != 2 || hit.IndexedCount == nil || *hit.IndexedCount != 40 || hit.IndexRebuildMs == nil || *hit.IndexRebuildMs != 12.5 {
		t.Errorf("hit lost a field: %+v", hit)
	}
	if nothing.Content != nil || nothing.RecordIDs == nil || nothing.Memories == nil {
		t.Errorf("nothing: content must be nil and the lists empty non-nil: %+v", nothing)
	}
	if off.Skipped != memory.SkippedRecallDisabled || off.MaxResults != 0 {
		t.Errorf("recall-disabled lost its reason or its explicit zero: %+v", off)
	}
}
