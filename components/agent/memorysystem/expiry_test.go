package memorysystem

import (
	"testing"
	"time"

	"github.com/Phong95/aec-agent-go/components/agent/memorysystem/memory"
	"github.com/Phong95/aec-agent-go/components/persistence"
)

// ---------------------------------------------------------------------------
// Recall keeps memories alive: being recalled counts as a touch for expiry. These are
// its four cases. The test builds the unexported dreaming component with only an
// agent id and a memory store and calls expireStale directly.
// ---------------------------------------------------------------------------

const (
	expiryAgent = "agent-1"
	expiryNowMs = 1700000000000
	dayDuration = 24 * time.Hour
)

func daysAgo(days float64) string {
	s, _ := isoFromMs(float64(expiryNowMs) - days*float64(dayDuration.Milliseconds()))
	return s
}

// expiryRecord builds a test record: stale by default (observed 40 days ago).
func expiryRecord(id string, mutate func(*memory.Record)) memory.Record {
	r := memory.Record{
		ID: id, AgentID: expiryAgent, Kind: memory.KindUserPreference, Scope: memory.ScopeUser, Subject: "user", Key: "units",
		Value: "always use metric units", Status: memory.StatusActive, Confidence: memory.ConfidenceExplicit, Source: "conversation",
		SourceTrust: memory.TrustUserApproved, ObservedAt: daysAgo(40),
	}
	if mutate != nil {
		mutate(&r)
	}
	return r
}

func TestRecallExpiry(t *testing.T) {
	setup := func(t *testing.T, recs ...memory.Record) (s *persistence.MemoryMemoryStore, ix *indexing, rt *retrieval, expire func() []memory.ConsolidationResult) {
		t.Helper()
		s = seedStore(t, recs...)
		d := newTestDeps(expiryAgent, WithClock(fixedClock(expiryNowMs)), WithLocation(time.UTC))
		ix = newIndexing(expiryAgent, s)
		rt = newRetrieval(d, s, ix)
		dr := &dreaming{deps: d, store: s}
		expire = func() []memory.ConsolidationResult {
			res, err := dr.expireStale(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			return res
		}
		return s, ix, rt, expire
	}

	t.Run("recall stamps lastRecalledAt on returned records", func(t *testing.T) {
		rec := expiryRecord("r1", nil)
		st, ix, rt, _ := setup(t, rec)
		if _, err := ix.rebuild(t.Context()); err != nil {
			t.Fatal(err)
		}

		results, err := rt.recall(t.Context(), memory.RetrievalQuery{AgentID: expiryAgent, CurrentContext: "metric units"})
		if err != nil {
			t.Fatal(err)
		}

		found := false
		for _, r := range results {
			found = found || r.Record.ID == rec.ID
		}
		if !found {
			t.Fatalf("the record was not recalled: %+v", results)
		}
		stored := getRecord(t, st, rec.ID)
		if stored.LastRecalledAt == "" {
			t.Fatal("lastRecalledAt is not set")
		}
		stamped, err := time.Parse(time.RFC3339Nano, stored.LastRecalledAt)
		if err != nil {
			t.Fatal(err)
		}
		if age := time.UnixMilli(expiryNowMs).Sub(stamped); age < 0 || age >= 5*time.Second {
			t.Fatalf("lastRecalledAt is %v old, want under 5s", age)
		}
	})

	t.Run("a stale memory that was recalled recently does not expire", func(t *testing.T) {
		rec := expiryRecord("r1", func(r *memory.Record) { r.LastRecalledAt = daysAgo(2) })
		st, _, _, expire := setup(t, rec)

		results := expire()

		if len(results) != 0 {
			t.Fatalf("results = %+v, want none", results)
		}
		if got := getRecord(t, st, rec.ID); got.Status != memory.StatusActive {
			t.Fatalf("status = %q, want active", got.Status)
		}
	})

	t.Run("a stale memory that was never recalled still expires", func(t *testing.T) {
		rec := expiryRecord("r1", nil)
		st, _, _, expire := setup(t, rec)

		results := expire()

		if len(results) != 1 || results[0].RecordID != rec.ID || results[0].Action != memory.ActionExpire {
			t.Fatalf("results = %+v, want one expire of %s", results, rec.ID)
		}
		if got := getRecord(t, st, rec.ID); got.Status != memory.StatusExpired {
			t.Fatalf("status = %q, want expired", got.Status)
		}
	})

	t.Run("a memory last recalled beyond the expiry window expires", func(t *testing.T) {
		rec := expiryRecord("r1", func(r *memory.Record) { r.LastRecalledAt = daysAgo(35) })
		st, _, _, expire := setup(t, rec)

		expire()

		if got := getRecord(t, st, rec.ID); got.Status != memory.StatusExpired {
			t.Fatalf("status = %q, want expired", got.Status)
		}
	})
}

// getRecord reads a record that must exist.
func getRecord(t *testing.T, s persistence.MemoryStore, id string) memory.Record {
	t.Helper()
	r, err := s.Get(t.Context(), id)
	if err != nil || r == nil {
		t.Fatalf("record %s: %v %v", id, r, err)
	}
	return *r
}
