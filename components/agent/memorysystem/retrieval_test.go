package memorysystem

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Phong95/aec-agent-go/components/agent/memorysystem/memory"
	"github.com/Phong95/aec-agent-go/components/persistence"
)

// ---------------------------------------------------------------------------
// Recall against the recorded fixture.
//
// memorysystem-recall.json is recorded by tools/wiregen/memory/pipeline.cases.mjs: each
// scenario seeded the SDK's in-memory memory store, rebuilt the index and ran a recall
// under a pinned clock. The Go test replays the steps and requires the same ranked
// results (all four scores, which are floats printed with the JS number format) and the
// same store after the lastRecalledAt stamps.
// ---------------------------------------------------------------------------

type recallStep struct {
	Rebuild   bool  `json:"rebuild"`
	AdvanceMs int64 `json:"advanceMs"`
	Mutate    []struct {
		ID  string            `json:"id"`
		Set map[string]string `json:"set"`
	} `json:"mutate"`
	Remove []string              `json:"remove"`
	Query  memory.RetrievalQuery `json:"query"`
}

type recallScenario struct {
	Name    string          `json:"name"`
	AgentID string          `json:"agentId"`
	ClockMs int64           `json:"clockMs"`
	Seed    []memory.Record `json:"seed"`
	Steps   []recallStep    `json:"steps"`
	Result  struct {
		Steps []struct {
			Rebuilt *int            `json:"rebuilt"`
			Results json.RawMessage `json:"results"`
		} `json:"steps"`
		Store json.RawMessage `json:"store"`
	} `json:"result"`
}

func TestRecallMatchesGolden(t *testing.T) {
	var scenarios []recallScenario
	loadFixture(t, "memorysystem-recall", &scenarios)
	if len(scenarios) < 10 {
		t.Fatalf("the fixture holds %d scenarios, want at least 10", len(scenarios))
	}
	for _, sc := range scenarios {
		t.Run(sc.Name, func(t *testing.T) {
			ms := sc.ClockMs
			clock := func() time.Time { return time.UnixMilli(ms) }
			store := seedStore(t, sc.Seed...)
			d := newTestDeps(sc.AgentID, WithClock(clock), WithLocation(time.UTC))
			ix := newIndexing(sc.AgentID, store)
			rt := newRetrieval(d, store, ix)

			if len(sc.Steps) != len(sc.Result.Steps) {
				t.Fatalf("%d steps, the fixture has %d results", len(sc.Steps), len(sc.Result.Steps))
			}
			for i, step := range sc.Steps {
				ms += step.AdvanceMs
				for _, m := range step.Mutate {
					rec, err := store.Get(t.Context(), m.ID)
					if err != nil || rec == nil {
						t.Fatalf("mutate: record %s: %v", m.ID, err)
					}
					for k, v := range m.Set {
						if k != "status" {
							t.Fatalf("mutate: unsupported field %q", k)
						}
						rec.Status = memory.Status(v)
					}
					if err := store.Update(t.Context(), *rec); err != nil {
						t.Fatal(err)
					}
				}
				for _, id := range step.Remove {
					if err := store.Delete(t.Context(), id); err != nil {
						t.Fatal(err)
					}
				}

				want := sc.Result.Steps[i]
				if step.Rebuild {
					n, err := ix.rebuild(t.Context())
					if err != nil {
						t.Fatal(err)
					}
					if want.Rebuilt == nil || *want.Rebuilt != n {
						t.Errorf("step %d: rebuilt %d records, TS %v", i, n, want.Rebuilt)
					}
				}
				got, err := rt.recall(t.Context(), step.Query)
				if err != nil {
					t.Fatalf("step %d: %v", i, err)
				}
				if gotJSON := encode(t, got); gotJSON != string(want.Results) {
					t.Errorf("step %d: results differ\n got: %s\nwant: %s", i, gotJSON, want.Results)
				}
			}
			if gotStore := encode(t, storeRecords(t, store)); gotStore != string(sc.Result.Store) {
				t.Errorf("store differs\n got: %s\nwant: %s", gotStore, sc.Result.Store)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// More retrieval tests
// ---------------------------------------------------------------------------

// failingMemoryStore fails the writes of Update; every other call goes to the
// wrapped store.
type failingMemoryStore struct {
	persistence.MemoryStore
	updateErr error
}

func (f *failingMemoryStore) Update(_ context.Context, _ memory.Record) error { return f.updateErr }

func TestRecall_FailedStampIsLoggedAndRecallStillReturns(t *testing.T) {
	var logged lockedBuffer
	logger := slog.New(slog.NewTextHandler(&logged, nil))
	r1 := rec("r1", "units", "always use metric units")
	r1.ObservedAt = "2023-11-14T00:00:00.000Z"
	inner := seedStore(t, r1)
	store := &failingMemoryStore{MemoryStore: inner, updateErr: errors.New("disk full")}
	d := newTestDeps("agent-1", WithClock(fixedClock(1700000000000)), WithLogger(logger), WithLocation(time.UTC))
	ix := newIndexing("agent-1", store)
	if _, err := ix.rebuild(t.Context()); err != nil {
		t.Fatal(err)
	}

	got, err := newRetrieval(d, store, ix).recall(t.Context(), memory.RetrievalQuery{AgentID: "agent-1", CurrentContext: "metric units"})
	if err != nil || len(got) != 1 {
		t.Fatalf("recall = %d results, %v; want 1 and no error", len(got), err)
	}
	if got[0].Record.LastRecalledAt == "" {
		t.Error("the returned record must carry the stamp even when the store write failed")
	}
	text := logged.String()
	if want := "record=r1"; !strings.Contains(text, want) {
		t.Errorf("the log lacks %q: %s", want, text)
	}
	if strings.Contains(text, "metric") {
		t.Errorf("a memory value leaked into the log: %s", text)
	}
}

// TestRecall_StampKeepsAConcurrentStatusChange pins the divergence of markRecalled:
// the stamp is written onto a fresh read of the record, so a record that another
// writer expired between the search and the stamp is not turned active again.
func TestRecall_StampKeepsAConcurrentStatusChange(t *testing.T) {
	r1 := rec("r1", "units", "always use metric units")
	r1.ObservedAt = "2023-11-14T00:00:00.000Z"
	inner := seedStore(t, r1)
	// The store expires the record the first time recall reads it for scoring, which
	// is before the stamp; the stamp must keep that status.
	store := &expireOnGet{MemoryStore: inner}
	d := newTestDeps("agent-1", WithClock(fixedClock(1700000000000)), WithLocation(time.UTC))
	ix := newIndexing("agent-1", inner)
	if _, err := ix.rebuild(t.Context()); err != nil {
		t.Fatal(err)
	}

	if _, err := newRetrieval(d, store, ix).recall(t.Context(), memory.RetrievalQuery{AgentID: "agent-1", CurrentContext: "metric units"}); err != nil {
		t.Fatal(err)
	}
	got, err := inner.Get(t.Context(), "r1")
	if err != nil || got == nil {
		t.Fatalf("get: %v %v", got, err)
	}
	if got.Status != memory.StatusExpired {
		t.Errorf("status = %q, the stamp resurrected an expired record", got.Status)
	}
	if got.LastRecalledAt == "" {
		t.Error("the stamp was not written")
	}
}

// expireOnGet returns the record on its first Get and expires it in the store right
// after, as a concurrent consolidation would between the read and the stamp.
type expireOnGet struct {
	persistence.MemoryStore
	done bool
}

func (e *expireOnGet) Get(ctx context.Context, id string) (*memory.Record, error) {
	rec, err := e.MemoryStore.Get(ctx, id)
	if err == nil && rec != nil && !e.done {
		e.done = true
		expired := *rec
		expired.Status = memory.StatusExpired
		if uerr := e.Update(ctx, expired); uerr != nil {
			return nil, uerr
		}
	}
	return rec, err
}

func TestRecall_DeletedRecordStaysDeleted(t *testing.T) {
	r1 := rec("r1", "units", "always use metric units")
	r1.ObservedAt = "2023-11-14T00:00:00.000Z"
	inner := seedStore(t, r1)
	store := &deleteOnGet{MemoryStore: inner}
	d := newTestDeps("agent-1", WithClock(fixedClock(1700000000000)), WithLocation(time.UTC))
	ix := newIndexing("agent-1", inner)
	if _, err := ix.rebuild(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := newRetrieval(d, store, ix).recall(t.Context(), memory.RetrievalQuery{AgentID: "agent-1", CurrentContext: "metric units"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := inner.Get(t.Context(), "r1"); got != nil {
		t.Errorf("the stamp re-created a deleted record: %+v", got)
	}
}

type deleteOnGet struct {
	persistence.MemoryStore
	done bool
}

func (e *deleteOnGet) Get(ctx context.Context, id string) (*memory.Record, error) {
	rec, err := e.MemoryStore.Get(ctx, id)
	if err == nil && rec != nil && !e.done {
		e.done = true
		if derr := e.Delete(ctx, id); derr != nil {
			return nil, derr
		}
	}
	return rec, err
}

func TestImportanceScoreWeights(t *testing.T) {
	r := memory.Record{Confidence: memory.ConfidenceVerified, SourceTrust: memory.TrustTeamAuthored}
	if got, want := importanceScore(r), (0.8+0.6)/2; got != want {
		t.Errorf("importance = %v, want %v", got, want)
	}
	if got := importanceScore(memory.Record{Confidence: "bogus", SourceTrust: "bogus"}); got != 0 {
		t.Errorf("unknown values weigh 0, got %v", got)
	}
}

// lockedBuffer is an io.Writer a logger can write to from any goroutine.
type lockedBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	return len(p), nil
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}

var _ io.Writer = (*lockedBuffer)(nil)
