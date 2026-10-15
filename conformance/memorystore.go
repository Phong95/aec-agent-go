package conformance

import (
	"fmt"
	"slices"
	"sync"
	"testing"

	"github.com/Phong95/aec-agent-go/components/agent/memorysystem/memory"
	"github.com/Phong95/aec-agent-go/components/persistence"
	"github.com/Phong95/aec-agent-go/conformance/internal/check"
	"github.com/Phong95/aec-agent-go/kernel"
)

// MemoryStoreFactory returns a persistence.MemoryStore. See the package comment
// for the freshness rules of the store suites.
type MemoryStoreFactory func(t *testing.T) persistence.MemoryStore

// The observedAt values the query tests cut at.
const (
	observedJan = "2026-01-01T00:00:00.000Z"
	observedFeb = "2026-02-01T00:00:00.000Z"
	observedMar = "2026-03-01T00:00:00.000Z"
)

// memoryRecordFor builds an active record of the agent. Its id, subject and key
// are derived from id, so every record of a run is distinguishable.
func memoryRecordFor(id, agentID string, kind memory.Kind, scope memory.Scope, observedAt string) memory.Record {
	return memory.Record{
		ID: id, AgentID: agentID, Kind: kind, Scope: scope,
		Subject: "subject-" + id, Key: "key-" + id, Value: "value-" + id,
		Status: memory.StatusActive, Confidence: memory.ConfidenceVerified,
		Source: "chat", SourceTrust: memory.TrustVerifiedInternal, ObservedAt: observedAt,
	}
}

// sameMemoryRecord compares two records field by field. A nil and an empty Tags
// list are the same value.
func sameMemoryRecord(a, b memory.Record) bool {
	return a.ID == b.ID && a.AgentID == b.AgentID && a.Kind == b.Kind && a.Scope == b.Scope &&
		a.Subject == b.Subject && a.Key == b.Key && a.Value == b.Value && a.Status == b.Status &&
		a.Confidence == b.Confidence && a.Source == b.Source && a.SourceTrust == b.SourceTrust &&
		a.ObservedAt == b.ObservedAt && a.UpdatedAt == b.UpdatedAt && a.LastRecalledAt == b.LastRecalledAt &&
		a.SupersededBy == b.SupersededBy && a.ExpiresAt == b.ExpiresAt && slices.Equal(a.Tags, b.Tags)
}

func saveMemory(t *testing.T, s persistence.MemoryStore, r memory.Record) {
	t.Helper()
	check.NoErr(t, s.Save(t.Context(), r), "save %s", r.ID)
}

// getMemory returns the record and fails the test when it is absent.
func getMemory(t *testing.T, s persistence.MemoryStore, id string) memory.Record {
	t.Helper()
	got, err := s.Get(t.Context(), id)
	check.NoErr(t, err, "get %s", id)
	if got == nil {
		t.Fatalf("memory record %s is absent, want it stored", id)
	}
	return *got
}

// queryMemoryIDs runs the query and returns the ids of the result.
func queryMemoryIDs(t *testing.T, s persistence.MemoryStore, f persistence.MemoryFilter) []string {
	t.Helper()
	out, err := s.Query(t.Context(), f)
	check.NoErr(t, err, "query")
	ids := make([]string, 0, len(out))
	for _, r := range out {
		ids = append(ids, r.ID)
	}
	return ids
}

// memorySeed is the records of one query test: a unique agent and ids derived
// from the run prefix. Fewer than 100 records, so a store with a default query
// cap still returns them all.
type memorySeed struct {
	agent string
	id    func(name string) string
}

func newMemorySeed() memorySeed {
	p := runPrefix()
	return memorySeed{agent: "agent-" + p, id: func(name string) string { return p + "-" + name }}
}

// MemoryStore runs the conformance suite for persistence.MemoryStore
// implementations.
//
// It is written for server/go's Postgres stores. It encodes the filters every
// memory store applies (agent, kinds, scopes, statuses, subject, key, any-of tags, since
// inclusive, before exclusive, limit) and the active-only, agent-scoped FindByKey.
// Every agent and record id carries a per-run prefix and each query test seeds its
// own agent with fewer than 100 records, so results are compared as sets and no
// subtest assumes an empty store, a list order, or what happens when a record is
// updated before it is saved. The Go-only subtests prove that records are copied
// in and out and that concurrent saves keep every record.
func MemoryStore(t *testing.T, newStore MemoryStoreFactory) {
	t.Helper()

	// -- save() / get() / update() / delete() --

	t.Run("save() then get() round-trips every field", func(t *testing.T) {
		store := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store))
		seed := newMemorySeed()
		r := memoryRecordFor(seed.id("full"), seed.agent, memory.KindDecision, memory.ScopeProject, observedFeb)
		r.UpdatedAt = "2026-02-02T00:00:00.000Z"
		r.LastRecalledAt = "2026-02-03T00:00:00.000Z"
		r.SupersededBy = seed.id("newer")
		r.ExpiresAt = "2027-01-01T00:00:00.000Z"
		r.Tags = []string{"alpha", "beta"}

		saveMemory(t, store, r)

		got := getMemory(t, store, r.ID)
		check.True(t, sameMemoryRecord(got, r), "loaded %+v, want %+v", got, r)
	})

	t.Run("save() then get() round-trips a record without optional fields", func(t *testing.T) {
		store := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store))
		seed := newMemorySeed()
		r := memoryRecordFor(seed.id("minimal"), seed.agent, memory.KindFact, memory.ScopeUser, observedJan)

		saveMemory(t, store, r)

		got := getMemory(t, store, r.ID)
		check.True(t, sameMemoryRecord(got, r), "loaded %+v, want %+v", got, r)
	})

	t.Run("get() returns null for an unknown id", func(t *testing.T) {
		store := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store))

		got, err := store.Get(t.Context(), "missing-"+kernel.NewID())
		check.NoErr(t, err)
		check.True(t, got == nil, "an unknown id must give a nil record")
	})

	t.Run("update() replaces a saved record", func(t *testing.T) {
		store := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store))
		seed := newMemorySeed()
		r := memoryRecordFor(seed.id("upd"), seed.agent, memory.KindFact, memory.ScopeProject, observedJan)
		saveMemory(t, store, r)

		changed := r
		changed.Value = "changed value"
		changed.Status = memory.StatusSuperseded
		changed.SupersededBy = seed.id("successor")
		changed.UpdatedAt = "2026-01-05T00:00:00.000Z"
		changed.Tags = []string{"updated"}
		check.NoErr(t, store.Update(t.Context(), changed))

		got := getMemory(t, store, r.ID)
		check.True(t, sameMemoryRecord(got, changed), "loaded %+v, want %+v", got, changed)
	})

	t.Run("delete() removes the record and ignores unknown ids", func(t *testing.T) {
		store := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store))
		seed := newMemorySeed()
		r := memoryRecordFor(seed.id("del"), seed.agent, memory.KindFact, memory.ScopeProject, observedJan)
		other := memoryRecordFor(seed.id("keep"), seed.agent, memory.KindFact, memory.ScopeProject, observedJan)
		saveMemory(t, store, r)
		saveMemory(t, store, other)

		check.NoErr(t, store.Delete(t.Context(), r.ID))

		gone, err := store.Get(t.Context(), r.ID)
		check.NoErr(t, err)
		check.True(t, gone == nil, "a deleted record must be absent")
		getMemory(t, store, other.ID)
		check.NoErr(t, store.Delete(t.Context(), "missing-"+kernel.NewID()))
	})

	// -- query() --

	t.Run("query() filters by agentId and never returns another agent's records", func(t *testing.T) {
		store := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store))
		seed := newMemorySeed()
		theirs := "other-" + seed.agent
		saveMemory(t, store, memoryRecordFor(seed.id("a1"), seed.agent, memory.KindFact, memory.ScopeProject, observedJan))
		saveMemory(t, store, memoryRecordFor(seed.id("a2"), seed.agent, memory.KindFact, memory.ScopeProject, observedFeb))
		saveMemory(t, store, memoryRecordFor(seed.id("b1"), theirs, memory.KindFact, memory.ScopeProject, observedJan))

		mine := queryMemoryIDs(t, store, persistence.MemoryFilter{AgentID: seed.agent})
		others := queryMemoryIDs(t, store, persistence.MemoryFilter{AgentID: theirs})
		none := queryMemoryIDs(t, store, persistence.MemoryFilter{AgentID: "unknown-" + seed.agent})

		check.True(t, sameSet(mine, seed.id("a1"), seed.id("a2")), "agent query returned %v", mine)
		check.True(t, sameSet(others, seed.id("b1")), "other agent query returned %v", others)
		check.Len(t, none, 0)
	})

	t.Run("query() filters by kinds, scopes and statuses", func(t *testing.T) {
		store := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store))
		seed := newMemorySeed()
		fact := memoryRecordFor(seed.id("fact"), seed.agent, memory.KindFact, memory.ScopeProject, observedJan)
		decision := memoryRecordFor(seed.id("decision"), seed.agent, memory.KindDecision, memory.ScopeUser, observedJan)
		procedure := memoryRecordFor(seed.id("procedure"), seed.agent, memory.KindProcedure, memory.ScopeTask, observedJan)
		procedure.Status = memory.StatusExpired
		for _, r := range []memory.Record{fact, decision, procedure} {
			saveMemory(t, store, r)
		}

		byKinds := queryMemoryIDs(t, store, persistence.MemoryFilter{AgentID: seed.agent, Kinds: []memory.Kind{memory.KindDecision, memory.KindProcedure}})
		byScopes := queryMemoryIDs(t, store, persistence.MemoryFilter{AgentID: seed.agent, Scopes: []memory.Scope{memory.ScopeProject, memory.ScopeTask}})
		byStatuses := queryMemoryIDs(t, store, persistence.MemoryFilter{AgentID: seed.agent, Statuses: []memory.Status{memory.StatusExpired}})

		check.True(t, sameSet(byKinds, decision.ID, procedure.ID), "kinds filter returned %v", byKinds)
		check.True(t, sameSet(byScopes, fact.ID, procedure.ID), "scopes filter returned %v", byScopes)
		check.True(t, sameSet(byStatuses, procedure.ID), "statuses filter returned %v", byStatuses)
	})

	t.Run("query() filters by subject and key", func(t *testing.T) {
		store := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store))
		seed := newMemorySeed()
		one := memoryRecordFor(seed.id("one"), seed.agent, memory.KindFact, memory.ScopeProject, observedJan)
		two := memoryRecordFor(seed.id("two"), seed.agent, memory.KindFact, memory.ScopeProject, observedJan)
		saveMemory(t, store, one)
		saveMemory(t, store, two)

		bySubject := queryMemoryIDs(t, store, persistence.MemoryFilter{AgentID: seed.agent, Subject: one.Subject})
		byKey := queryMemoryIDs(t, store, persistence.MemoryFilter{AgentID: seed.agent, Key: two.Key})

		check.True(t, sameSet(bySubject, one.ID), "subject filter returned %v", bySubject)
		check.True(t, sameSet(byKey, two.ID), "key filter returned %v", byKey)
	})

	t.Run("query() filters by tags (any of)", func(t *testing.T) {
		store := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store))
		seed := newMemorySeed()
		x := memoryRecordFor(seed.id("x"), seed.agent, memory.KindFact, memory.ScopeProject, observedJan)
		x.Tags = []string{"red", "blue"}
		y := memoryRecordFor(seed.id("y"), seed.agent, memory.KindFact, memory.ScopeProject, observedJan)
		y.Tags = []string{"green"}
		untagged := memoryRecordFor(seed.id("untagged"), seed.agent, memory.KindFact, memory.ScopeProject, observedJan)
		for _, r := range []memory.Record{x, y, untagged} {
			saveMemory(t, store, r)
		}

		blue := queryMemoryIDs(t, store, persistence.MemoryFilter{AgentID: seed.agent, Tags: []string{"blue"}})
		either := queryMemoryIDs(t, store, persistence.MemoryFilter{AgentID: seed.agent, Tags: []string{"blue", "green"}})
		missing := queryMemoryIDs(t, store, persistence.MemoryFilter{AgentID: seed.agent, Tags: []string{"purple"}})

		check.True(t, sameSet(blue, x.ID), "tag filter returned %v", blue)
		check.True(t, sameSet(either, x.ID, y.ID), "any-of tag filter returned %v", either)
		check.Len(t, missing, 0)
	})

	t.Run("query() since is inclusive and before is exclusive", func(t *testing.T) {
		store := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store))
		seed := newMemorySeed()
		jan := memoryRecordFor(seed.id("jan"), seed.agent, memory.KindFact, memory.ScopeProject, observedJan)
		feb := memoryRecordFor(seed.id("feb"), seed.agent, memory.KindFact, memory.ScopeProject, observedFeb)
		mar := memoryRecordFor(seed.id("mar"), seed.agent, memory.KindFact, memory.ScopeProject, observedMar)
		for _, r := range []memory.Record{jan, feb, mar} {
			saveMemory(t, store, r)
		}

		since := queryMemoryIDs(t, store, persistence.MemoryFilter{AgentID: seed.agent, Since: observedFeb})
		before := queryMemoryIDs(t, store, persistence.MemoryFilter{AgentID: seed.agent, Before: observedMar})
		window := queryMemoryIDs(t, store, persistence.MemoryFilter{AgentID: seed.agent, Since: observedFeb, Before: observedMar})

		check.True(t, sameSet(since, feb.ID, mar.ID), "since filter returned %v", since)
		check.True(t, sameSet(before, jan.ID, feb.ID), "before filter returned %v", before)
		check.True(t, sameSet(window, feb.ID), "since and before returned %v", window)
	})

	t.Run("query() combines filters", func(t *testing.T) {
		store := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store))
		seed := newMemorySeed()
		hit := memoryRecordFor(seed.id("hit"), seed.agent, memory.KindDecision, memory.ScopeProject, observedFeb)
		wrongKind := memoryRecordFor(seed.id("wrong-kind"), seed.agent, memory.KindFact, memory.ScopeProject, observedFeb)
		wrongScope := memoryRecordFor(seed.id("wrong-scope"), seed.agent, memory.KindDecision, memory.ScopeUser, observedFeb)
		tooOld := memoryRecordFor(seed.id("too-old"), seed.agent, memory.KindDecision, memory.ScopeProject, observedJan)
		for _, r := range []memory.Record{hit, wrongKind, wrongScope, tooOld} {
			saveMemory(t, store, r)
		}

		got := queryMemoryIDs(t, store, persistence.MemoryFilter{
			AgentID: seed.agent, Kinds: []memory.Kind{memory.KindDecision},
			Scopes: []memory.Scope{memory.ScopeProject}, Since: observedFeb,
		})

		check.True(t, sameSet(got, hit.ID), "combined filter returned %v", got)
	})

	t.Run("query() honours limit", func(t *testing.T) {
		store := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store))
		seed := newMemorySeed()
		all := []string{}
		for i := range 5 {
			r := memoryRecordFor(seed.id(fmt.Sprintf("lim%d", i)), seed.agent, memory.KindFact, memory.ScopeProject, observedJan)
			saveMemory(t, store, r)
			all = append(all, r.ID)
		}

		got := queryMemoryIDs(t, store, persistence.MemoryFilter{AgentID: seed.agent, Limit: 2})

		// Which two records come back depends on the store order: only the count
		// and the membership are part of the contract.
		check.Len(t, got, 2)
		for _, id := range got {
			check.True(t, slices.Contains(all, id), "limit returned %s, which is not one of the agent's records", id)
		}
	})

	// -- findByKey() --

	t.Run("findByKey() returns the active record of the agent, scope and key", func(t *testing.T) {
		store := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store))
		seed := newMemorySeed()
		other := "other-" + seed.agent
		mine := memoryRecordFor(seed.id("mine"), seed.agent, memory.KindFact, memory.ScopeProject, observedJan)
		mine.Key = seed.id("shared-key")
		theirs := memoryRecordFor(seed.id("theirs"), other, memory.KindFact, memory.ScopeProject, observedJan)
		theirs.Key = mine.Key
		otherScope := memoryRecordFor(seed.id("scope"), seed.agent, memory.KindFact, memory.ScopeUser, observedJan)
		otherScope.Key = mine.Key
		for _, r := range []memory.Record{mine, theirs, otherScope} {
			saveMemory(t, store, r)
		}

		got, err := store.FindByKey(t.Context(), seed.agent, memory.ScopeProject, mine.Key)
		check.NoErr(t, err)
		if got == nil {
			t.Fatalf("findByKey missed the active record of the agent")
		}
		check.Equal(t, got.ID, mine.ID)
		gotOther, err := store.FindByKey(t.Context(), other, memory.ScopeProject, mine.Key)
		check.NoErr(t, err)
		if gotOther == nil {
			t.Fatalf("findByKey missed the active record of the other agent")
		}
		check.Equal(t, gotOther.ID, theirs.ID)
		gotScope, err := store.FindByKey(t.Context(), seed.agent, memory.ScopeUser, mine.Key)
		check.NoErr(t, err)
		if gotScope == nil {
			t.Fatalf("findByKey missed the active record of the other scope")
		}
		check.Equal(t, gotScope.ID, otherScope.ID)
	})

	t.Run("findByKey() ignores records that are not active", func(t *testing.T) {
		store := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store))
		seed := newMemorySeed()
		old := memoryRecordFor(seed.id("old"), seed.agent, memory.KindFact, memory.ScopeProject, observedJan)
		old.Key = seed.id("key")
		old.Status = memory.StatusSuperseded
		saveMemory(t, store, old)

		got, err := store.FindByKey(t.Context(), seed.agent, memory.ScopeProject, old.Key)
		check.NoErr(t, err)
		check.True(t, got == nil, "a superseded record must not be found, got %+v", got)

		current := memoryRecordFor(seed.id("current"), seed.agent, memory.KindFact, memory.ScopeProject, observedFeb)
		current.Key = old.Key
		saveMemory(t, store, current)

		got, err = store.FindByKey(t.Context(), seed.agent, memory.ScopeProject, old.Key)
		check.NoErr(t, err)
		if got == nil {
			t.Fatalf("findByKey missed the active record")
		}
		check.Equal(t, got.ID, current.ID)
	})

	t.Run("findByKey() returns null on a miss", func(t *testing.T) {
		store := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store))
		seed := newMemorySeed()

		got, err := store.FindByKey(t.Context(), seed.agent, memory.ScopeProject, seed.id("no-such-key"))
		check.NoErr(t, err)
		check.True(t, got == nil, "a miss must give a nil record, got %+v", got)
	})

	// -- Go-only guarantees --

	t.Run("mutating a returned or given record does not change the store (Go-only)", func(t *testing.T) {
		store := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store))
		seed := newMemorySeed()
		want := memoryRecordFor(seed.id("copy"), seed.agent, memory.KindFact, memory.ScopeProject, observedJan)
		want.Key = seed.id("copy-key")
		want.Tags = []string{"one", "two"}

		given := want
		given.Tags = []string{"one", "two"}
		saveMemory(t, store, given)
		given.Tags[0] = "changed after save"
		given.Value = "changed after save"

		verify := func(label string) {
			t.Helper()
			got := getMemory(t, store, want.ID)
			check.True(t, sameMemoryRecord(got, want), "%s: loaded %+v, want %+v", label, got, want)
		}
		verify("after mutating the record given to Save")

		got := getMemory(t, store, want.ID)
		got.Tags[1] = "changed after get"
		got.Value = "changed after get"
		verify("after mutating the record returned by Get")

		queried, err := store.Query(t.Context(), persistence.MemoryFilter{AgentID: seed.agent})
		check.NoErr(t, err)
		for _, r := range queried {
			if len(r.Tags) > 0 {
				r.Tags[0] = "changed after query"
			}
		}
		verify("after mutating the records returned by Query")

		found, err := store.FindByKey(t.Context(), seed.agent, memory.ScopeProject, want.Key)
		check.NoErr(t, err)
		if found != nil && len(found.Tags) > 0 {
			found.Tags[0] = "changed after findByKey"
		}
		verify("after mutating the record returned by FindByKey")

		// The record handed to Update is the caller's again too.
		changed := want
		changed.Value = "updated"
		changed.Tags = []string{"three"}
		check.NoErr(t, store.Update(t.Context(), changed))
		changed.Tags[0] = "changed after update"
		again := getMemory(t, store, want.ID)
		check.True(t, slices.Equal(again.Tags, []string{"three"}), "tags after mutating the record given to Update: %v", again.Tags)
	})

	t.Run("concurrent Save of distinct records keeps all of them (Go-only)", func(t *testing.T) {
		const writers = 16
		store := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store))
		seed := newMemorySeed()

		ids := make([]string, writers)
		var wg sync.WaitGroup
		for w := range writers {
			ids[w] = seed.id(fmt.Sprintf("conc%d", w))
			wg.Go(func() {
				r := memoryRecordFor(ids[w], seed.agent, memory.KindFact, memory.ScopeProject, observedJan)
				if err := store.Save(t.Context(), r); err != nil {
					t.Errorf("save from writer %d: %v", w, err)
				}
			})
		}
		wg.Wait()

		got := queryMemoryIDs(t, store, persistence.MemoryFilter{AgentID: seed.agent})
		check.True(t, sameSet(got, ids...), "query returned %d records, want the %d saved ones", len(got), writers)
		for _, id := range ids {
			getMemory(t, store, id)
		}
	})
}
