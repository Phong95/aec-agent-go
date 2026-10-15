package conformance

import (
	"fmt"
	"sync"
	"testing"

	"github.com/Phong95/aec-agent-go/components/persistence"
	"github.com/Phong95/aec-agent-go/conformance/internal/check"
	"github.com/Phong95/aec-agent-go/kernel"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// SessionStoreFactory returns a persistence.SessionStore. See the package comment
// for the freshness rules of SessionStore and SessionStoreDurability.
type SessionStoreFactory func(t *testing.T) persistence.SessionStore

// runPrefix returns a per-run prefix for the data of a store suite: the letter t
// and the first eight hex characters of kernel.NewID. It uses lowercase letters
// and digits only, so a prefixed name stays valid for stores that validate names
// (skill names, for example). Every store suite derives all its ids and tag
// values from it, so one shared database can serve any number of runs.
func runPrefix() string {
	return "t" + kernel.NewID()[:8]
}

// metaOf builds a metadata object from alternating keys and string values.
func metaOf(kv ...string) ordered.Object {
	o := ordered.NewObject()
	for i := 0; i+1 < len(kv); i += 2 {
		o.Set(kv[i], kv[i+1])
	}
	return o
}

// metaString returns the string stored under key, or "" when it is absent or
// not a string.
func metaString(o ordered.Object, key string) string {
	v, _ := o.Get(key)
	s, _ := v.(string)
	return s
}

// createSession creates a session and fails the test on an error.
func createSession(t *testing.T, s persistence.SessionStore, md ordered.Object) kernel.Session {
	t.Helper()
	sess, err := s.Create(t.Context(), md)
	check.NoErr(t, err, "create")
	return sess
}

// getSession returns the stored session and fails the test when it is absent.
func getSession(t *testing.T, s persistence.SessionStore, id string) kernel.Session {
	t.Helper()
	got, err := s.Get(t.Context(), id)
	check.NoErr(t, err, "get %s", id)
	if got == nil {
		t.Fatalf("session %s is absent, want it stored", id)
	}
	return *got
}

// listSessions lists the sessions matching the metadata filter.
func listSessions(t *testing.T, s persistence.SessionStore, filter ordered.Object) []kernel.Session {
	t.Helper()
	out, err := s.List(t.Context(), kernel.SessionFilter{Metadata: filter})
	check.NoErr(t, err, "list")
	return out
}

// hasSessionID reports whether a session with the id is in the list.
func hasSessionID(list []kernel.Session, id string) bool {
	for _, s := range list {
		if s.ID == id {
			return true
		}
	}
	return false
}

// checkpointOf returns the stored consolidated message count, 0 when absent.
func checkpointOf(s kernel.Session) int {
	if s.ConsolidatedMessageCount == nil {
		return 0
	}
	return *s.ConsolidatedMessageCount
}

// SessionStore runs the conformance suite for persistence.SessionStore
// implementations. It verifies the CRUD invariants and session isolation, with
// the data of the list tests isolated by a per-run prefix (see the package
// comment), plus the guarantees that a
// returned session is a copy and that concurrent writers keep the highest
// consolidation checkpoint. Message storage is covered by MessageStore.
func SessionStore(t *testing.T, newStore SessionStoreFactory) {
	t.Helper()

	// -- Lifecycle --

	t.Run("initialize() resolves without error", func(t *testing.T) {
		store := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store))
	})

	t.Run("dispose() resolves without error", func(t *testing.T) {
		store := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store))
		check.NoErr(t, kernel.Dispose(t.Context(), store))
	})

	// -- create() --

	t.Run("create() returns a session with a unique id", func(t *testing.T) {
		store := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store))

		s1 := createSession(t, store, ordered.Object{})
		s2 := createSession(t, store, ordered.Object{})

		check.True(t, s1.ID != "", "first id is empty")
		check.True(t, s2.ID != "", "second id is empty")
		check.NotEqual(t, s1.ID, s2.ID)
	})

	t.Run("create() sets timestamps", func(t *testing.T) {
		store := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store))

		before := kernel.NowMillis()
		session := createSession(t, store, ordered.Object{})
		after := kernel.NowMillis()

		check.True(t, session.CreatedAt >= before && session.CreatedAt <= after,
			"createdAt %d is outside [%d, %d]", session.CreatedAt, before, after)
		check.True(t, session.UpdatedAt >= before && session.UpdatedAt <= after,
			"updatedAt %d is outside [%d, %d]", session.UpdatedAt, before, after)
	})

	t.Run("create() stores initial metadata", func(t *testing.T) {
		store := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store))

		session := createSession(t, store, metaOf("userId", "u1", "projectId", "p1"))

		check.Equal(t, metaString(session.Metadata, "userId"), "u1")
		check.Equal(t, metaString(session.Metadata, "projectId"), "p1")
	})

	t.Run("create() defaults to empty metadata", func(t *testing.T) {
		store := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store))

		session := createSession(t, store, ordered.Object{})

		check.Equal(t, session.Metadata.Len(), 0)
	})

	// -- get() --

	t.Run("get() returns the created session", func(t *testing.T) {
		store := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store))
		tag := runPrefix() + "-test"

		created := createSession(t, store, metaOf("tag", tag))
		fetched := getSession(t, store, created.ID)

		check.Equal(t, fetched.ID, created.ID)
		check.Equal(t, metaString(fetched.Metadata, "tag"), tag)
	})

	t.Run("get() returns null for unknown session id", func(t *testing.T) {
		store := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store))

		got, err := store.Get(t.Context(), "nonexistent-"+kernel.NewID())
		check.NoErr(t, err)
		check.True(t, got == nil, "an unknown id must give a nil session")
	})

	// -- update() --

	t.Run("update() persists metadata changes", func(t *testing.T) {
		store := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store))

		session := createSession(t, store, ordered.Object{})
		session.Metadata.Set("key", "value")
		check.NoErr(t, store.Update(t.Context(), session))

		fetched := getSession(t, store, session.ID)
		check.Equal(t, metaString(fetched.Metadata, "key"), "value")
	})

	t.Run("update() never lowers consolidatedMessageCount", func(t *testing.T) {
		store := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store))

		stale := createSession(t, store, ordered.Object{})
		check.NoErr(t, store.SetConsolidatedMessageCount(t.Context(), stale.ID, 10))
		zero := 0
		stale.ConsolidatedMessageCount = &zero
		check.NoErr(t, store.Update(t.Context(), stale))

		fetched := getSession(t, store, stale.ID)
		check.Equal(t, checkpointOf(fetched), 10)
	})

	t.Run("update() persists jobId", func(t *testing.T) {
		store := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store))

		session := createSession(t, store, ordered.Object{})
		session.JobID = "job-1"
		check.NoErr(t, store.Update(t.Context(), session))

		fetched := getSession(t, store, session.ID)
		check.Equal(t, fetched.JobID, "job-1")
	})

	t.Run("update() without jobId keeps the stored one", func(t *testing.T) {
		store := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store))

		stale := createSession(t, store, ordered.Object{})
		tagged := stale.Clone()
		tagged.JobID = "job-1"
		check.NoErr(t, store.Update(t.Context(), tagged))
		stale.Metadata = metaOf("key", "value")
		check.NoErr(t, store.Update(t.Context(), stale))

		fetched := getSession(t, store, stale.ID)
		check.Equal(t, fetched.JobID, "job-1")
		check.Equal(t, metaString(fetched.Metadata, "key"), "value")
	})

	t.Run("create() leaves jobId unset", func(t *testing.T) {
		store := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store))

		session := createSession(t, store, ordered.Object{})
		check.Equal(t, session.JobID, "")
		check.Equal(t, getSession(t, store, session.ID).JobID, "")
	})

	// -- setConsolidatedMessageCount() --

	t.Run("setConsolidatedMessageCount() persists the checkpoint", func(t *testing.T) {
		store := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store))

		session := createSession(t, store, ordered.Object{})
		check.NoErr(t, store.SetConsolidatedMessageCount(t.Context(), session.ID, 5))

		fetched := getSession(t, store, session.ID)
		check.Equal(t, checkpointOf(fetched), 5)
	})

	t.Run("setConsolidatedMessageCount() never decreases the value", func(t *testing.T) {
		store := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store))

		session := createSession(t, store, ordered.Object{})
		check.NoErr(t, store.SetConsolidatedMessageCount(t.Context(), session.ID, 8))
		check.NoErr(t, store.SetConsolidatedMessageCount(t.Context(), session.ID, 3))

		fetched := getSession(t, store, session.ID)
		check.Equal(t, checkpointOf(fetched), 8)
	})

	t.Run("setConsolidatedMessageCount() does not touch updatedAt or metadata", func(t *testing.T) {
		store := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store))
		value := runPrefix() + "-value"

		session := createSession(t, store, metaOf("key", value))
		before := getSession(t, store, session.ID)
		check.NoErr(t, store.SetConsolidatedMessageCount(t.Context(), session.ID, 4))

		fetched := getSession(t, store, session.ID)
		check.Equal(t, fetched.UpdatedAt, before.UpdatedAt)
		check.Equal(t, fetched.Metadata.Len(), 1)
		check.Equal(t, metaString(fetched.Metadata, "key"), value)
	})

	t.Run("setConsolidatedMessageCount() is a no-op for unknown sessions", func(t *testing.T) {
		store := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store))

		check.NoErr(t, store.SetConsolidatedMessageCount(t.Context(), "nonexistent-"+kernel.NewID(), 3))
	})

	// -- delete() --

	t.Run("delete() removes the session", func(t *testing.T) {
		store := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store))

		session := createSession(t, store, ordered.Object{})
		check.NoErr(t, store.Delete(t.Context(), session.ID))

		fetched, err := store.Get(t.Context(), session.ID)
		check.NoErr(t, err)
		check.True(t, fetched == nil, "a deleted session must be absent")
	})

	t.Run("delete() does not affect other sessions", func(t *testing.T) {
		store := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store))

		s1 := createSession(t, store, ordered.Object{})
		s2 := createSession(t, store, ordered.Object{})
		check.NoErr(t, store.Delete(t.Context(), s1.ID))

		fetched := getSession(t, store, s2.ID)
		check.Equal(t, fetched.ID, s2.ID)
	})

	// -- list() --

	t.Run("list() returns all sessions when no filter is given", func(t *testing.T) {
		store := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store))
		group := runPrefix() + "-list-all"

		s1 := createSession(t, store, metaOf("group", group))
		s2 := createSession(t, store, metaOf("group", group))

		all := listSessions(t, store, ordered.Object{})
		check.True(t, len(all) >= 2, "got %d sessions, want at least 2", len(all))
		check.True(t, hasSessionID(all, s1.ID), "the first session is missing from the unfiltered list")
		check.True(t, hasSessionID(all, s2.ID), "the second session is missing from the unfiltered list")
	})

	t.Run("list() filters by metadata", func(t *testing.T) {
		store := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store))
		p := runPrefix()
		alice, bob := p+"-alice", p+"-bob"
		prod, dev := p+"-prod", p+"-dev"

		createSession(t, store, metaOf("owner", alice, "env", prod))
		createSession(t, store, metaOf("owner", alice, "env", dev))
		createSession(t, store, metaOf("owner", bob, "env", prod))

		aliceSessions := listSessions(t, store, metaOf("owner", alice))
		prodSessions := listSessions(t, store, metaOf("env", prod))

		check.Len(t, aliceSessions, 2)
		for _, s := range aliceSessions {
			check.Equal(t, metaString(s.Metadata, "owner"), alice)
		}
		check.Len(t, prodSessions, 2)
		for _, s := range prodSessions {
			check.Equal(t, metaString(s.Metadata, "env"), prod)
		}
	})

	t.Run("list() filters by jobId", func(t *testing.T) {
		store := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store))
		p := runPrefix()
		jobA, jobB := p+"-job-a", p+"-job-b"
		carol, dave := p+"-carol", p+"-dave"

		tag := func(s kernel.Session, job string) kernel.Session {
			s.JobID = job
			check.NoErr(t, store.Update(t.Context(), s))
			return s
		}
		a1 := tag(createSession(t, store, metaOf("owner", carol)), jobA)
		a2 := tag(createSession(t, store, metaOf("owner", dave)), jobA)
		tag(createSession(t, store, metaOf("owner", carol)), jobB)
		createSession(t, store, metaOf("owner", carol))

		runsA, err := store.List(t.Context(), kernel.SessionFilter{JobID: jobA})
		check.NoErr(t, err)
		check.Len(t, runsA, 2)
		check.True(t, hasSessionID(runsA, a1.ID) && hasSessionID(runsA, a2.ID))

		// Combined with a metadata filter: both must match.
		carolA, err := store.List(t.Context(), kernel.SessionFilter{Metadata: metaOf("owner", carol), JobID: jobA})
		check.NoErr(t, err)
		check.Len(t, carolA, 1)
		check.Equal(t, carolA[0].ID, a1.ID)
	})

	t.Run("list() with non-matching filter returns empty", func(t *testing.T) {
		store := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store))
		p := runPrefix()

		createSession(t, store, metaOf("owner", p+"-alice"))

		result := listSessions(t, store, metaOf("owner", p+"-nonexistent"))
		check.Len(t, result, 0)
	})

	// -- Isolation --

	t.Run("metadata-filtered list does not leak across groups", func(t *testing.T) {
		store := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store))
		p := runPrefix()
		groupA, groupB := p+"-A", p+"-B"

		createSession(t, store, metaOf("group", groupA))
		createSession(t, store, metaOf("group", groupB))

		got := listSessions(t, store, metaOf("group", groupB))
		check.Len(t, got, 1)
		if len(got) == 1 {
			check.Equal(t, metaString(got[0].Metadata, "group"), groupB)
		}
	})

	// -- Go-only guarantees --

	t.Run("mutating a returned session does not change the store (Go-only)", func(t *testing.T) {
		store := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store))
		p := runPrefix()
		group := p + "-copy"

		nested := metaOf("inner", "original")
		md := metaOf("group", group, "name", "original")
		md.Set("nested", nested)
		md.Set("list", []any{"original"})

		cache, samples, count := 7, 3, 2
		created := createSession(t, store, md)
		created.LLMModel = "model-original"
		created.CurrentTokenUsage = &kernel.TokenUsage{InputTokens: 10, OutputTokens: 5, CacheReadTokens: &cache}
		created.CounterCalibration = &kernel.CounterCalibration{CharsPerToken: 3.5, Samples: samples}
		created.ConsolidatedMessageCount = &count
		check.NoErr(t, store.Update(t.Context(), created))

		// The session handed to Update is the caller's again: changing it must not
		// reach the store.
		created.Metadata.Set("name", "changed-after-update")
		*created.ConsolidatedMessageCount = 99
		created.CurrentTokenUsage.InputTokens = 999

		// verify reads the stored session and requires every original value.
		verify := func(label string) {
			t.Helper()
			got := getSession(t, store, created.ID)
			check.Equal(t, metaString(got.Metadata, "name"), "original", "%s: metadata entry", label)
			inner, _ := got.Metadata.Get("nested")
			innerObj, ok := inner.(ordered.Object)
			check.True(t, ok, "%s: nested metadata is %T, want ordered.Object", label, inner)
			check.Equal(t, metaString(innerObj, "inner"), "original", "%s: nested object", label)
			items, _ := got.Metadata.Get("list")
			list, ok := items.([]any)
			check.True(t, ok && len(list) == 1, "%s: nested list is %v", label, items)
			if ok && len(list) == 1 {
				check.Equal(t, fmt.Sprint(list[0]), "original", "%s: nested list element", label)
			}
			check.Equal(t, checkpointOf(got), 2, "%s: consolidatedMessageCount", label)
			if got.CurrentTokenUsage == nil || got.CounterCalibration == nil {
				t.Fatalf("%s: stored pointer fields are missing", label)
			}
			check.Equal(t, got.CurrentTokenUsage.InputTokens, 10, "%s: currentTokenUsage", label)
			if got.CurrentTokenUsage.CacheReadTokens == nil {
				t.Fatalf("%s: stored cacheReadTokens is missing", label)
			}
			check.Equal(t, *got.CurrentTokenUsage.CacheReadTokens, 7, "%s: cacheReadTokens", label)
			check.Equal(t, got.CounterCalibration.Samples, 3, "%s: counterCalibration", label)
		}
		verify("after mutating the value given to Update")

		// Change every mutable part of a session returned by Get.
		got := getSession(t, store, created.ID)
		got.Metadata.Set("name", "changed-after-get")
		innerAny, _ := got.Metadata.Get("nested")
		if innerObj, ok := innerAny.(ordered.Object); ok {
			innerObj.Set("inner", "changed-after-get")
		}
		if itemsAny, _ := got.Metadata.Get("list"); itemsAny != nil {
			if list, ok := itemsAny.([]any); ok && len(list) == 1 {
				list[0] = "changed-after-get"
			}
		}
		if got.ConsolidatedMessageCount != nil {
			*got.ConsolidatedMessageCount = 99
		}
		if got.CurrentTokenUsage != nil {
			got.CurrentTokenUsage.InputTokens = 999
			if got.CurrentTokenUsage.CacheReadTokens != nil {
				*got.CurrentTokenUsage.CacheReadTokens = 999
			}
		}
		if got.CounterCalibration != nil {
			got.CounterCalibration.Samples = 999
		}
		verify("after mutating a session returned by Get")

		// Change every mutable part of a session returned in a List.
		listed := listSessions(t, store, metaOf("group", group))
		check.Len(t, listed, 1)
		if len(listed) == 1 {
			el := listed[0]
			el.Metadata.Set("name", "changed-after-list")
			if el.ConsolidatedMessageCount != nil {
				*el.ConsolidatedMessageCount = 99
			}
			if el.CurrentTokenUsage != nil {
				el.CurrentTokenUsage.OutputTokens = 999
				if el.CurrentTokenUsage.CacheReadTokens != nil {
					*el.CurrentTokenUsage.CacheReadTokens = 999
				}
			}
			if el.CounterCalibration != nil {
				el.CounterCalibration.CharsPerToken = 999
			}
		}
		verify("after mutating a session returned by List")
	})

	t.Run("concurrent Update and SetConsolidatedMessageCount keep the highest checkpoint (Go-only)", func(t *testing.T) {
		const writers = 16
		store := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store))
		base := createSession(t, store, ordered.Object{})

		var wg sync.WaitGroup
		for n := 1; n <= writers; n++ {
			wg.Go(func() {
				ctx := t.Context()
				c := base.Clone()
				count := n
				c.ConsolidatedMessageCount = &count
				if err := store.Update(ctx, c); err != nil {
					t.Errorf("update with count %d: %v", n, err)
				}
				if err := store.SetConsolidatedMessageCount(ctx, base.ID, n); err != nil {
					t.Errorf("set count %d: %v", n, err)
				}
			})
		}
		wg.Wait()

		fetched := getSession(t, store, base.ID)
		check.Equal(t, checkpointOf(fetched), writers, "the stored checkpoint must be the highest written")
	})
}

// SessionStoreDurability runs the durability suite for persistence.SessionStore
// implementations.
//
// newStore must return a NEW instance backed by the SAME persistent storage each
// time it is called. This proves data survives instance destruction.
//
// In-memory stores participate by sharing a persistence.SessionBacking across
// instances. Database-backed stores share the same connection or database. Each
// test uses its own unique tags, so one shared backing can serve the whole run.
func SessionStoreDurability(t *testing.T, newStore SessionStoreFactory) {
	t.Helper()

	t.Run("session survives dispose and recreate", func(t *testing.T) {
		scope := "dur-" + kernel.NewID()
		store1 := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store1))

		session := createSession(t, store1, metaOf("scope", scope))
		session.Metadata.Set("key", "value")
		check.NoErr(t, store1.Update(t.Context(), session))

		check.NoErr(t, kernel.Dispose(t.Context(), store1))

		store2 := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store2))

		recovered := getSession(t, store2, session.ID)
		check.Equal(t, recovered.ID, session.ID)
		check.Equal(t, metaString(recovered.Metadata, "key"), "value")
	})

	t.Run("list with filter survives dispose and recreate", func(t *testing.T) {
		tag := "dur-list-" + kernel.NewID()
		store1 := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store1))

		createSession(t, store1, metaOf("tag", tag))
		createSession(t, store1, metaOf("tag", tag))
		check.NoErr(t, kernel.Dispose(t.Context(), store1))

		store2 := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store2))

		sessions := listSessions(t, store2, metaOf("tag", tag))
		check.Len(t, sessions, 2)
	})

	t.Run("delete persists across dispose and recreate", func(t *testing.T) {
		store1 := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store1))

		session := createSession(t, store1, ordered.Object{})
		check.NoErr(t, store1.Delete(t.Context(), session.ID))
		check.NoErr(t, kernel.Dispose(t.Context(), store1))

		store2 := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store2))

		result, err := store2.Get(t.Context(), session.ID)
		check.NoErr(t, err)
		check.True(t, result == nil, "a deleted session must stay deleted")
	})

	t.Run("metadata isolation holds across dispose and recreate", func(t *testing.T) {
		tagX := "dur-X-" + kernel.NewID()
		tagY := "dur-Y-" + kernel.NewID()
		store1 := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store1))

		createSession(t, store1, metaOf("group", tagX))
		createSession(t, store1, metaOf("group", tagY))
		check.NoErr(t, kernel.Dispose(t.Context(), store1))

		store2 := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store2))

		groupX := listSessions(t, store2, metaOf("group", tagX))
		groupY := listSessions(t, store2, metaOf("group", tagY))
		check.Len(t, groupX, 1)
		check.Len(t, groupY, 1)
		if len(groupX) == 1 {
			check.Equal(t, metaString(groupX[0].Metadata, "group"), tagX)
		}
		if len(groupY) == 1 {
			check.Equal(t, metaString(groupY[0].Metadata, "group"), tagY)
		}
	})
}
