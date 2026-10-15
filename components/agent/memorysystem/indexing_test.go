package memorysystem

import (
	"encoding/json"
	"fmt"
	"slices"
	"sync"
	"testing"

	"github.com/Phong95/aec-agent-go/components/agent/memorysystem/memory"
	"github.com/Phong95/aec-agent-go/components/persistence"
)

// TestTokenizeMatchesGolden replays memorysystem-tokenize.json: every text went through
// the reference tokenizer (lowercase, split on /[\s\p{P}]+/u, keep tokens longer than one
// UTF-16 unit) in Node 22.19, including the code points that are new in Unicode 16.
func TestTokenizeMatchesGolden(t *testing.T) {
	var rows [][2]json.RawMessage
	loadFixture(t, "memorysystem-tokenize", &rows)
	if len(rows) < 35 {
		t.Fatalf("the corpus holds %d texts, want at least 35", len(rows))
	}
	for i, r := range rows {
		var text string
		if err := json.Unmarshal(r[0], &text); err != nil {
			t.Fatal(err)
		}
		if got := encode(t, tokenize(text)); got != string(r[1]) {
			t.Errorf("row %d: tokenize(%+q)\n got: %s\nwant: %s", i, text, got, r[1])
		}
	}
}

func rec(id, key, value string) memory.Record {
	return memory.Record{ID: id, AgentID: "agent-1", Key: key, Value: value, Subject: "s", Status: memory.StatusActive}
}

func ids(ms []scoredMatch) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.recordID
	}
	return out
}

func TestKeywordIndex_ScoreAndStableOrder(t *testing.T) {
	k := newKeywordIndex()
	// Insertion order is r2, r1, r5, r4, r3 and differs from the id order on purpose.
	k.rebuild([]memory.Record{
		rec("r2", "alpha", "metric only"),
		rec("r1", "beta", "metric and units"),
		rec("r5", "gamma", "units with metric"),
		rec("r4", "delta", "metric units system all three"),
		rec("r3", "epsilon", "units only"),
		rec("r0", "zeta", "nothing relevant"),
	})

	got := k.search("metric units system", 10)
	want := []string{"r4", "r1", "r5", "r2", "r3"}
	if !slices.Equal(ids(got), want) {
		t.Fatalf("order = %v, want %v (score descending, ties in insertion order)", ids(got), want)
	}
	for i, wantScore := range []float64{1, 2.0 / 3, 2.0 / 3, 1.0 / 3, 1.0 / 3} {
		if got[i].score != wantScore {
			t.Errorf("score[%d] = %v, want %v", i, got[i].score, wantScore)
		}
	}

	// A query term that repeats counts every time: the score divides by the term count.
	if got := k.search("metric metric", 10); len(got) == 0 || got[0].score != 1 {
		t.Errorf("repeated term: %+v", got)
	}
	if got := k.search("!!! ??", 10); len(got) != 0 {
		t.Errorf("a query without terms must find nothing, got %+v", got)
	}

	// limit is JS slice(0, limit).
	if n := len(k.search("metric units system", 2)); n != 2 {
		t.Errorf("limit 2 returned %d", n)
	}
	if n := len(k.search("metric units system", 0)); n != 0 {
		t.Errorf("limit 0 returned %d", n)
	}
	if n := len(k.search("metric units system", -1)); n != 4 {
		t.Errorf("limit -1 returned %d, want all but the last (4)", n)
	}
}

func TestKeywordIndex_AddAndRemoveKeepInsertionOrder(t *testing.T) {
	k := newKeywordIndex()
	k.rebuild([]memory.Record{rec("a", "k", "metric"), rec("b", "k", "metric")})

	// Updating a record keeps its place (JS Map.set on an existing key).
	k.add(rec("a", "k", "metric again"))
	if got := ids(k.search("metric", 10)); !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("after update: %v", got)
	}
	// Removing and adding it again moves it to the end.
	k.remove("a")
	k.add(rec("a", "k", "metric"))
	if got := ids(k.search("metric", 10)); !slices.Equal(got, []string{"b", "a"}) {
		t.Fatalf("after remove and add: %v", got)
	}
	k.remove("a")
	k.remove("missing")
	if k.size() != 1 {
		t.Fatalf("size = %d, want 1", k.size())
	}

	// Tags are part of the indexed text.
	tagged := rec("t", "k", "plain")
	tagged.Tags = []string{"Estimating"}
	k.add(tagged)
	if got := ids(k.search("estimating", 10)); !slices.Equal(got, []string{"t"}) {
		t.Fatalf("tag search: %v", got)
	}
}

func TestIndexing_RebuildCountsOnlyTheAgentsActiveRecords(t *testing.T) {
	other := rec("o", "k", "metric")
	other.AgentID = "other"
	gone := rec("g", "k", "metric")
	gone.Status = memory.StatusExpired
	store := seedStore(t, rec("a", "k", "metric"), other, gone)
	ix := newIndexing("agent-1", store)

	n, err := ix.rebuild(t.Context())
	if err != nil || n != 1 {
		t.Fatalf("rebuild = %d, %v; want 1", n, err)
	}
	if got := ids(ix.search("metric", 10)); !slices.Equal(got, []string{"a"}) {
		t.Fatalf("search = %v", got)
	}
	recs, err := ix.queryByMetadata(t.Context(), persistence.MemoryFilter{})
	if err != nil || len(recs) != 2 {
		t.Fatalf("queryByMetadata = %d records, %v; want the agent's 2 (any status)", len(recs), err)
	}
}

// TestKeywordIndex_ConcurrentSearchAndRebuild runs searches against rebuilds under
// the race detector: the loop searches while the consolidation actor rebuilds.
func TestKeywordIndex_ConcurrentSearchAndRebuild(t *testing.T) {
	k := newKeywordIndex()
	var recs []memory.Record
	for i := range 50 {
		recs = append(recs, rec(fmt.Sprintf("r%02d", i), "k", fmt.Sprintf("metric units %d", i)))
	}
	k.rebuild(recs)

	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for range 200 {
				// Every rebuild holds all 50 records, so a search that sees fewer saw a
				// half built index.
				if n := len(k.search("metric units", 1000)); n < 50 {
					t.Errorf("a half built index was visible: %d matches", n)
					return
				}
			}
		})
	}
	wg.Go(func() {
		for range 100 {
			k.rebuild(recs)
			k.add(rec("extra", "k", "metric units"))
			k.remove("extra")
		}
	})
	wg.Wait()
}
