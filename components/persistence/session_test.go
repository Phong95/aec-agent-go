package persistence_test

import (
	"encoding/json"
	"slices"
	"sync"
	"testing"

	"github.com/Phong95/aec-agent-go/components/persistence"
	"github.com/Phong95/aec-agent-go/kernel"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

func mustGet(t *testing.T, s persistence.SessionStore, id string) kernel.Session {
	t.Helper()
	got, err := s.Get(t.Context(), id)
	if err != nil || got == nil {
		t.Fatalf("Get(%s) = %v, %v", id, got, err)
	}
	return *got
}

func encode(t *testing.T, v any) string {
	t.Helper()
	b, err := kernel.EncodeJSON(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func ids(list []kernel.Session) []string {
	out := make([]string, len(list))
	for i, s := range list {
		out[i] = s.ID
	}
	return out
}

// Mutating anything a caller received from, or handed to, the store never
// changes what the store holds (PER-02, success criterion 1).
func TestMemorySessionStoreCopies(t *testing.T) {
	ctx := t.Context()
	store := persistence.NewMemorySessionStore()

	nested := ordered.NewObject()
	nested.Set("k", json.Number("1"))
	meta := ordered.NewObject()
	meta.Set("nested", nested)
	meta.Set("list", []any{"x", "y"})
	created, err := store.Create(ctx, meta)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := func() string { return encode(t, mustGet(t, store, created.ID)) }
	want := snapshot()
	unchanged := func(step string) {
		t.Helper()
		if got := snapshot(); got != want {
			t.Fatalf("%s changed the stored session:\n got: %s\nwant: %s", step, got, want)
		}
	}

	// 1. The metadata passed to Create, mutated after the call (the nested
	// Object shares its map with the copy held by meta).
	meta.Set("added", true)
	nested.Set("k2", true)
	unchanged("mutating the metadata passed to Create")

	// 2. The session returned by Create: metadata, nested object and list.
	created.Metadata.Set("x", 1)
	if v, ok := created.Metadata.Get("nested"); ok {
		n := v.(ordered.Object)
		n.Set("k3", true)
	}
	if v, ok := created.Metadata.Get("list"); ok {
		v.([]any)[0] = "changed"
	}
	unchanged("mutating the session returned by Create")

	// 3. A session returned by Get, including every pointer field.
	three, ten := 3, 10
	full := mustGet(t, store, created.ID)
	full.LLMModel = "m"
	full.CurrentTokenUsage = &kernel.TokenUsage{InputTokens: 1, OutputTokens: 2, CacheReadTokens: &ten}
	full.CounterCalibration = &kernel.CounterCalibration{CharsPerToken: 4, Samples: 1}
	full.ConsolidatedMessageCount = &three
	if err := store.Update(ctx, full); err != nil {
		t.Fatal(err)
	}
	want = snapshot()

	got := mustGet(t, store, created.ID)
	got.CurrentTokenUsage.InputTokens = 999
	*got.CurrentTokenUsage.CacheReadTokens = 999
	got.CounterCalibration.Samples = 999
	*got.ConsolidatedMessageCount = 999
	got.Metadata.Set("viaGet", true)
	if v, ok := got.Metadata.Get("nested"); ok {
		n := v.(ordered.Object)
		n.Set("viaGet", true)
	}
	unchanged("mutating a session returned by Get")

	// 4. The value passed to Update, mutated after the call.
	full.CurrentTokenUsage.OutputTokens = 999
	*full.CurrentTokenUsage.CacheReadTokens = 999
	full.CounterCalibration.CharsPerToken = 999
	full.Metadata.Set("afterUpdate", true)
	if v, ok := full.Metadata.Get("nested"); ok {
		n := v.(ordered.Object)
		n.Set("afterUpdate", true)
	}
	unchanged("mutating the value passed to Update")

	// 5. An element returned by List.
	list, err := store.List(ctx, kernel.SessionFilter{})
	if err != nil || len(list) != 1 {
		t.Fatalf("List = %v, %v", list, err)
	}
	list[0].Metadata.Set("viaList", true)
	*list[0].ConsolidatedMessageCount = 999
	list[0].CurrentTokenUsage.InputTokens = 999
	unchanged("mutating a List element")
}

func TestMemorySessionStoreSemantics(t *testing.T) {
	ctx := t.Context()

	t.Run("unknown id is a nil pointer and a nil error", func(t *testing.T) {
		store := persistence.NewMemorySessionStore()
		got, err := store.Get(ctx, "nope")
		if got != nil || err != nil {
			t.Fatalf("Get = %v, %v", got, err)
		}
	})

	t.Run("Update keeps the larger count and does not touch the caller's value", func(t *testing.T) {
		store := persistence.NewMemorySessionStore()
		created, err := store.Create(ctx, ordered.Object{})
		if err != nil {
			t.Fatal(err)
		}
		five, two := 5, 2
		high := created.Clone()
		high.ConsolidatedMessageCount = &five
		if err := store.Update(ctx, high); err != nil {
			t.Fatal(err)
		}
		stale := created.Clone()
		stale.ConsolidatedMessageCount = &two
		if err := store.Update(ctx, stale); err != nil {
			t.Fatal(err)
		}
		if got := mustGet(t, store, created.ID); got.ConsolidatedMessageCount == nil || *got.ConsolidatedMessageCount != 5 {
			t.Fatalf("stored count = %v, want 5 (a stale copy must not lower it)", got.ConsolidatedMessageCount)
		}
		if *stale.ConsolidatedMessageCount != 2 {
			t.Fatalf("Update wrote the merged checkpoint back into the caller's value: %d", *stale.ConsolidatedMessageCount)
		}
		if got := mustGet(t, store, created.ID); got.UpdatedAt != created.UpdatedAt {
			t.Fatalf("Update touched updatedAt: %d != %d", got.UpdatedAt, created.UpdatedAt)
		}
	})

	t.Run("SetConsolidatedMessageCount", func(t *testing.T) {
		store := persistence.NewMemorySessionStore()
		if err := store.SetConsolidatedMessageCount(ctx, "unknown", 4); err != nil {
			t.Fatalf("unknown id must be a no-op, got %v", err)
		}
		if list, _ := store.List(ctx, kernel.SessionFilter{}); len(list) != 0 {
			t.Fatalf("a no-op created a session: %v", list)
		}
		created, _ := store.Create(ctx, ordered.Object{})
		if err := store.SetConsolidatedMessageCount(ctx, created.ID, 0); err != nil {
			t.Fatal(err)
		}
		got := mustGet(t, store, created.ID)
		if got.ConsolidatedMessageCount == nil || *got.ConsolidatedMessageCount != 0 {
			t.Fatalf("an explicit 0 must be stored: %v", got.ConsolidatedMessageCount)
		}
		_ = store.SetConsolidatedMessageCount(ctx, created.ID, 7)
		_ = store.SetConsolidatedMessageCount(ctx, created.ID, 3)
		if got := mustGet(t, store, created.ID); *got.ConsolidatedMessageCount != 7 || got.UpdatedAt != created.UpdatedAt {
			t.Fatalf("count = %d updatedAt = %d, want 7 and %d", *got.ConsolidatedMessageCount, got.UpdatedAt, created.UpdatedAt)
		}
	})

	t.Run("insertion order, delete and re-create", func(t *testing.T) {
		store := persistence.NewMemorySessionStore()
		var order []string
		var all []kernel.Session
		for range 3 {
			s, _ := store.Create(ctx, ordered.Object{})
			order = append(order, s.ID)
			all = append(all, s)
		}
		_ = store.Update(ctx, all[1]) // an existing id keeps its position
		list, _ := store.List(ctx, kernel.SessionFilter{})
		if !slices.Equal(ids(list), order) {
			t.Fatalf("order = %v, want %v", ids(list), order)
		}
		_ = store.Delete(ctx, order[0])
		_ = store.Update(ctx, all[0]) // an upsert of a deleted id appends
		list, _ = store.List(ctx, kernel.SessionFilter{})
		if want := []string{order[1], order[2], order[0]}; !slices.Equal(ids(list), want) {
			t.Fatalf("order after delete and re-add = %v, want %v", ids(list), want)
		}
	})

	t.Run("filter uses JS strict equality", func(t *testing.T) {
		store := persistence.NewMemorySessionStore()
		var m1, m2 ordered.Object
		m1.Set("n", json.Number("3"))
		m1.Set("s", "x")
		m2.Set("n", "3")
		m2.Set("o", nil)
		s1, _ := store.Create(ctx, m1)
		s2, _ := store.Create(ctx, m2)
		check := func(key string, val any, want ...string) {
			t.Helper()
			var f kernel.SessionFilter
			f.Metadata.Set(key, val)
			list, _ := store.List(ctx, f)
			if !slices.Equal(ids(list), want) {
				t.Errorf("filter %s=%v (%T) = %v, want %v", key, val, val, ids(list), want)
			}
		}
		check("n", 3, s1.ID)                  // a number compares numerically
		check("n", 3.0, s1.ID)                // across float64
		check("n", json.Number("3.0"), s1.ID) // and across json.Number spellings
		check("n", "3", s2.ID)                // a string never equals a number
		check("s", "x", s1.ID)
		check("o", nil, s2.ID)        // null === null
		check("o", "", []string{}...) // a missing key never matches, nor does a different type
		check("missing", nil, []string{}...)
		nestedFilter := ordered.NewObject()
		var f kernel.SessionFilter
		f.Metadata.Set("n", nestedFilter) // an object never equals anything (JS reference compare)
		if list, _ := store.List(ctx, f); len(list) != 0 {
			t.Errorf("an object filter value matched: %v", ids(list))
		}
	})

	t.Run("stores over one backing see each other", func(t *testing.T) {
		backing := persistence.NewSessionBacking()
		a := persistence.NewMemorySessionStore(persistence.WithSessionBacking(backing))
		b := persistence.NewMemorySessionStore(persistence.WithSessionBacking(backing))
		created, _ := a.Create(ctx, ordered.Object{})
		if got, err := b.Get(ctx, created.ID); err != nil || got == nil || got.ID != created.ID {
			t.Fatalf("second store Get = %v, %v", got, err)
		}
		_ = b.Delete(ctx, created.ID)
		if got, _ := a.Get(ctx, created.ID); got != nil {
			t.Fatal("a delete through one store is not visible through the other")
		}
		persistence.WithSessionBacking(nil)(a) // nil options are ignored
		persistence.WithSessionClock(nil)(a)
		persistence.WithSessionIDs(nil)(a)
		if _, err := a.Create(ctx, ordered.Object{}); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("concurrent writers keep the monotonic maximum", func(t *testing.T) {
		store := persistence.NewMemorySessionStore()
		created, _ := store.Create(ctx, ordered.Object{})
		var wg sync.WaitGroup
		for i := 1; i <= 50; i++ {
			wg.Add(2)
			go func() {
				defer wg.Done()
				_ = store.SetConsolidatedMessageCount(ctx, created.ID, i)
			}()
			go func() {
				defer wg.Done()
				s := created.Clone()
				s.ConsolidatedMessageCount = &i
				_ = store.Update(ctx, s)
				_, _ = store.Get(ctx, created.ID)
				_, _ = store.List(ctx, kernel.SessionFilter{})
			}()
		}
		wg.Wait()
		if got := mustGet(t, store, created.ID); got.ConsolidatedMessageCount == nil || *got.ConsolidatedMessageCount != 50 {
			t.Fatalf("count = %v, want 50", got.ConsolidatedMessageCount)
		}
	})
}
