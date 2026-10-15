package memorysystem

import (
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Phong95/aec-agent-go/components/agent/memorysystem/memory"
	"github.com/Phong95/aec-agent-go/components/persistence"
	"github.com/Phong95/aec-agent-go/kernel"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// ---------------------------------------------------------------------------
// The agent-ownership, checkpoint and extraction-failure cases of consolidation, at
// the System level: a real dispatcher, real in-memory stores (with counting wrappers
// where a call must be observed) and a scripted model, on the virtual clock. 10-02
// proved the same rules at the Dreaming level (TestConsolidation_StampsAgentID and
// friends); these prove them through the scheduler and the System. The names carry
// OnTick where the 10-02 test of the same title exists.
// ---------------------------------------------------------------------------

// activeRecords are the agent's active records.
func activeRecords(t *testing.T, w *world, agentID string) []memory.Record {
	t.Helper()
	recs, err := w.hub.Memory.Query(t.Context(), persistence.MemoryFilter{AgentID: agentID, Statuses: []memory.Status{memory.StatusActive}})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	return recs
}

func values(recs []memory.Record) []string {
	out := make([]string, 0, len(recs))
	for _, r := range recs {
		out = append(out, r.Value)
	}
	return out
}

// Case "stamps MemoryRecord.agentId on records created during consolidation" (the
// test wraps the memory store to see every save).
func TestConsolidation_StampsAgentIDOnTick(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		sys := w.system("agent-1", memCfg(1), newFakeModel(extractionOf(t, "English")))
		sys.Start(t.Context())

		s1 := w.session()
		w.chat("agent-1", s1, "hello")
		advance(minute)

		found := false
		for _, r := range w.mem.saves() {
			if r.Key == sharedKey && r.AgentID == "agent-1" {
				found = true
			}
		}
		if !found {
			t.Fatalf("no save with key %s and agentId agent-1: %+v", sharedKey, w.mem.saves())
		}
		dispose(t, sys)
	})
}

// Case "does not supersede another agent's memory with the same scope+key". Two agents
// share the hub and the bus; each extracts a different value for the same key.
func TestConsolidation_AgentIsolationOnTick(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		a := w.system("agent-a", memCfg(1), newFakeModel(extractionOf(t, "English")))
		a.Start(t.Context())
		sA := w.session()
		w.chat("agent-a", sA, "hi")
		advance(minute)

		// Agent B shares persistence and extracts a different value for the same key.
		b := w.system("agent-b", memCfg(1), newFakeModel(extractionOf(t, "Vietnamese")))
		b.Start(t.Context())
		sB := w.session()
		w.chat("agent-b", sB, "xin chao")
		advance(minute)

		if got := values(activeRecords(t, w, "agent-a")); !slices.Equal(got, []string{"English"}) {
			t.Fatalf("agent a records = %v, want [English]", got)
		}
		if got := values(activeRecords(t, w, "agent-b")); !slices.Equal(got, []string{"Vietnamese"}) {
			t.Fatalf("agent b records = %v, want [Vietnamese]", got)
		}
		rec, err := w.hub.Memory.FindByKey(t.Context(), "agent-a", memory.ScopeUser, sharedKey)
		if err != nil || rec == nil || rec.Value != "English" {
			t.Fatalf("FindByKey(agent-a) = %+v, %v, want the English record", rec, err)
		}
		dispose(t, b)
		dispose(t, a)
	})
}

// Case "expiry pass only expires the consolidating agent's stale memories".
func TestConsolidation_ExpiryOwnAgentOnly(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		sys := w.system("agent-1", memCfg(1), newFakeModel(extractionOf(t, "English")))
		sys.Start(t.Context())

		stale, ok := isoFromMs(float64(time.Now().UnixMilli()) - 365*24*60*60*1000)
		if !ok {
			t.Fatal("stale date out of range")
		}
		base := memory.Record{
			Kind: memory.KindFact, Scope: memory.ScopeUser, Subject: "user", Value: "old",
			Status: memory.StatusActive, Confidence: memory.ConfidenceExplicit, Source: "conversation",
			SourceTrust: memory.TrustUserApproved, ObservedAt: stale, UpdatedAt: stale,
		}
		mine, theirs := base, base
		mine.ID, mine.AgentID, mine.Key = "mine", "agent-1", "k1"
		theirs.ID, theirs.AgentID, theirs.Key = "theirs", "other-agent", "k2"
		for _, r := range []memory.Record{mine, theirs} {
			if err := w.hub.Memory.Save(t.Context(), r); err != nil {
				t.Fatal(err)
			}
		}

		s1 := w.session()
		w.chat("agent-1", s1, "hi")
		advance(minute)

		for id, want := range map[string]memory.Status{"mine": memory.StatusExpired, "theirs": memory.StatusActive} {
			got, err := w.hub.Memory.Get(t.Context(), id)
			if err != nil || got == nil || got.Status != want {
				t.Errorf("record %s = %+v, %v, want status %s", id, got, err, want)
			}
		}
		dispose(t, sys)
	})
}

// Case "recall only returns the querying agent's memories".
func TestRecall_OwnAgentOnly(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		sys := w.system("agent-1", memCfg(1), newFakeModel(extractionOf(t, "English")))
		sys.Start(t.Context())

		foreign := memory.Record{
			ID: "foreign", AgentID: "other-agent", Kind: memory.KindUserPreference, Scope: memory.ScopeUser, Subject: "user",
			Key: sharedKey, Value: "Vietnamese response language", Status: memory.StatusActive, Confidence: memory.ConfidenceExplicit,
			Source: "conversation", SourceTrust: memory.TrustUserApproved, ObservedAt: isoNow(time.Now),
		}
		if err := w.hub.Memory.Save(t.Context(), foreign); err != nil {
			t.Fatal(err)
		}

		s1 := w.session()
		w.chat("agent-1", s1, "hi")
		advance(minute)

		results, err := sys.Recall(t.Context(), memory.RetrievalQuery{
			AgentID: "agent-1", SessionID: s1, CurrentContext: "response language", Scope: memory.ScopeUser,
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(results) == 0 {
			t.Fatal("recall returned nothing, want the agent's own memory")
		}
		for _, r := range results {
			if r.Record.AgentID != "agent-1" {
				t.Errorf("recall returned a record of agent %q", r.Record.AgentID)
			}
		}
		dispose(t, sys)
	})
}

// Case "persists the checkpoint on the Session without bumping updatedAt".
func TestConsolidation_CheckpointKeepsUpdatedAtOnTick(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		sys := w.system("agent-1", memCfg(1), newFakeModel())
		sys.Start(t.Context())

		s1 := w.session()
		w.chat("agent-1", s1, "hello") // user + assistant = 2 messages
		before, err := w.hub.Sessions.Get(t.Context(), s1)
		if err != nil || before == nil {
			t.Fatalf("session: %v %v", before, err)
		}
		advance(minute)

		after, err := w.hub.Sessions.Get(t.Context(), s1)
		if err != nil || after == nil {
			t.Fatalf("session: %v %v", after, err)
		}
		if after.ConsolidatedMessageCount == nil || *after.ConsolidatedMessageCount != 2 {
			t.Fatalf("checkpoint = %v, want 2", after.ConsolidatedMessageCount)
		}
		if after.UpdatedAt != before.UpdatedAt {
			t.Fatalf("updatedAt moved from %d to %d: background consolidation is not session activity", before.UpdatedAt, after.UpdatedAt)
		}
		dispose(t, sys)
	})
}

// Case "resumes from the checkpoint after the agent is re-spawned". Nothing in RAM
// survives: a fresh System over the same persistence reads only the two new messages
// (the range read is (session.id, 4, 2)).
func TestConsolidation_ResumesFromCheckpoint(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		first := w.system("agent-1", memCfg(1), newFakeModel())
		first.Start(t.Context())
		s1 := w.session()
		w.chat("agent-1", s1, "first")
		advance(minute)
		if got := w.checkpoint(s1); got != 2 {
			t.Fatalf("checkpoint = %d after the first pass, want 2", got)
		}
		dispose(t, first) // the agent is removed

		respawned := w.system("agent-1", memCfg(1), newFakeModel())
		respawned.Start(t.Context())
		w.chat("agent-1", s1, "second") // messages 3 and 4

		w.msgs.take() // forget the reads of the first pass
		advance(minute)

		calls := w.msgs.take()
		if len(calls) != 1 || calls[0] != [3]any{s1, 4, 2} {
			t.Fatalf("GetRange calls = %v, want exactly [%s 4 2]: only the two new messages are read", calls, s1)
		}
		if got := w.checkpoint(s1); got != 4 {
			t.Fatalf("checkpoint = %d, want 4", got)
		}
		dispose(t, respawned)
	})
}

// Case "never lets a stale Session write-back lower the checkpoint": the Phase 6 store
// is monotonic, re-asserted at this level with the recorded sequence.
func TestConsolidation_StaleSessionWriteBack(t *testing.T) {
	store := persistence.NewMemorySessionStore()
	session, err := store.Create(t.Context(), ordered.Object{})
	if err != nil {
		t.Fatal(err)
	}
	staleCopy := session

	if err := store.SetConsolidatedMessageCount(t.Context(), session.ID, 6); err != nil {
		t.Fatal(err)
	}
	if err := store.Update(t.Context(), staleCopy); err != nil { // e.g. ConversationManager's cached copy
		t.Fatal(err)
	}
	if err := store.SetConsolidatedMessageCount(t.Context(), session.ID, 3); err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(t.Context(), session.ID)
	if err != nil || got == nil || got.ConsolidatedMessageCount == nil || *got.ConsolidatedMessageCount != 6 {
		t.Fatalf("checkpoint = %+v, %v, want 6", got, err)
	}
}

// Case "does not advance the checkpoint when the model output is truncated, and
// retries next tick". The reasoning model burned the whole budget thinking: stop
// reason max_tokens with empty content.
func TestConsolidation_TruncatedOutputRetried(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		logs := &logCapture{}
		truncated := reply{Content: "", StopReason: kernel.StopMaxTokens, Usage: &kernel.TokenUsage{InputTokens: 100, OutputTokens: 8192}}
		sys := w.system("agent-1", memCfg(1), newFakeModel(truncated, extractionOf(t, "English")), WithLogger(logs.logger()))
		sys.Start(t.Context())

		s1 := w.session()
		w.chat("agent-1", s1, "hello")
		advance(minute)

		if got := w.checkpoint(s1); got != 0 {
			t.Fatalf("checkpoint = %d after a truncated reply, want 0", got)
		}
		if n := len(w.records("agent-1")); n != 0 {
			t.Fatalf("%d records after a truncated reply, want 0", n)
		}
		if !strings.Contains(logs.String(), "consolidation failed") {
			t.Fatalf("the failed pass is not logged: %s", logs.String())
		}

		// Next tick: the model answers properly, the same messages are processed.
		advance(minute)
		if got := w.checkpoint(s1); got != 2 {
			t.Fatalf("checkpoint = %d after the retry, want 2", got)
		}
		var keys []string
		for _, r := range w.records("agent-1") {
			keys = append(keys, r.Key)
		}
		if !slices.Equal(keys, []string{sharedKey}) {
			t.Fatalf("keys = %v, want [%s]", keys, sharedKey)
		}
		dispose(t, sys)
	})
}

// Case "accepts a JSON array wrapped in markdown fences". (10-02 has the Memory-level
// TestMemory_AcceptsFencedJSON.)
func TestConsolidation_AcceptsFencedJSONOnTick(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		fenced := extractionOf(t, "English")
		fenced.Content = "```json\n" + fenced.Content + "\n```"
		sys := w.system("agent-1", memCfg(1), newFakeModel(fenced))
		sys.Start(t.Context())

		s1 := w.session()
		w.chat("agent-1", s1, "hello")
		advance(minute)

		if got := values(w.records("agent-1")); !slices.Equal(got, []string{"English"}) {
			t.Fatalf("values = %v, want [English]", got)
		}
		dispose(t, sys)
	})
}

// System.Consolidate returns an empty result and touches nothing when memory is
// disabled, and RecallForTurn reports why it did not run.
func TestSystem_DisabledMemory(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		model := newFakeModel(extractionOf(t, "English"))
		sys := w.system("agent-1", memCfgOff(1), model)

		s1 := w.session()
		w.say(s1, "hello")
		got, err := sys.Consolidate(t.Context(), s1)
		if err != nil || got == nil || len(got) != 0 {
			t.Fatalf("Consolidate = %v, %v, want an empty non-nil slice", got, err)
		}
		if n := len(model.extractionCalls()); n != 0 {
			t.Fatalf("%d model calls while disabled, want 0", n)
		}
		rec, err := sys.Recall(t.Context(), memory.RetrievalQuery{AgentID: "agent-1", CurrentContext: "hello"})
		if err != nil || rec == nil || len(rec) != 0 {
			t.Fatalf("Recall = %v, %v, want an empty non-nil slice", rec, err)
		}
		out, err := sys.RecallForTurn(t.Context(), "hello", s1)
		if err != nil || out.Skipped != memory.SkippedMemoryDisabled || out.Content != nil || out.IndexRebuilt {
			t.Fatalf("RecallForTurn = %+v, %v, want skipped memory-disabled", out, err)
		}
		dispose(t, sys)
	})
}
