package conformance

import (
	"fmt"
	"slices"
	"sync"
	"testing"

	"github.com/Phong95/aec-agent-go/components/persistence"
	"github.com/Phong95/aec-agent-go/components/skillhub/skill"
	"github.com/Phong95/aec-agent-go/conformance/internal/check"
	"github.com/Phong95/aec-agent-go/kernel"
)

// SkillStoreFactory returns a persistence.SkillStore. See the package comment
// for the freshness rules of the store suites.
type SkillStoreFactory func(t *testing.T) persistence.SkillStore

// skillRecordFor builds a complete record whose content is
// derived from the (run-prefixed) name.
func skillRecordFor(name string) persistence.SkillRecord {
	return persistence.SkillRecord{
		Name:      name,
		Content:   "---\nname: " + name + "\ndescription: Test skill " + name + ".\n---\n\nDo it.\n",
		Files:     []skill.File{{Path: "references/notes.md", Content: "notes"}},
		CreatedBy: "agent-1",
		Version:   1,
		CreatedAt: "2026-09-28T00:00:00.000Z",
		UpdatedAt: "2026-09-28T00:00:00.000Z",
	}
}

// sameSkillRecord compares two records field by field. A nil and an empty Files
// list are the same value (the wire form of both is []).
func sameSkillRecord(a, b persistence.SkillRecord) bool {
	return a.Name == b.Name && a.Content == b.Content && a.CreatedBy == b.CreatedBy &&
		a.Version == b.Version && a.CreatedAt == b.CreatedAt && a.UpdatedAt == b.UpdatedAt &&
		slices.Equal(a.Files, b.Files)
}

// loadSkill loads a record and fails the test when it is absent.
func loadSkill(t *testing.T, s persistence.SkillStore, name string) persistence.SkillRecord {
	t.Helper()
	got, err := s.Load(t.Context(), name)
	check.NoErr(t, err, "load %s", name)
	if got == nil {
		t.Fatalf("skill %s is absent, want it stored", name)
	}
	return *got
}

func saveSkill(t *testing.T, s persistence.SkillStore, r persistence.SkillRecord) {
	t.Helper()
	check.NoErr(t, s.Save(t.Context(), r), "save %s", r.Name)
}

// listSkills lists every stored record.
func listSkills(t *testing.T, s persistence.SkillStore) []persistence.SkillRecord {
	t.Helper()
	out, err := s.List(t.Context())
	check.NoErr(t, err, "list")
	return out
}

// SkillStore runs the conformance suite for persistence.SkillStore
// implementations (custom skills, A-66). It verifies upsert-by-name,
// round-tripping of content and files, and deletion. Every skill name carries a per-run prefix (see the package comment)
// and the content is derived from the prefixed name. The Go-only subtests prove
// that records are copied in and out and that concurrent Save calls keep one
// whole record per name.
func SkillStore(t *testing.T, newStore SkillStoreFactory) {
	t.Helper()

	t.Run("save() then load() round-trips every field", func(t *testing.T) {
		store := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store))
		saved := skillRecordFor(runPrefix() + "-rfi-summary")

		saveSkill(t, store, saved)

		got := loadSkill(t, store, saved.Name)
		check.True(t, sameSkillRecord(got, saved), "loaded %+v, want %+v", got, saved)
	})

	t.Run("load() returns null for an unknown name", func(t *testing.T) {
		store := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store))

		got, err := store.Load(t.Context(), runPrefix()+"-does-not-exist")
		check.NoErr(t, err)
		check.True(t, got == nil, "an unknown name must give a nil record")
	})

	t.Run("save() replaces an existing record with the same name", func(t *testing.T) {
		store := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store))
		name := runPrefix() + "-plan-b"

		saveSkill(t, store, skillRecordFor(name))
		updated := skillRecordFor(name)
		updated.Content = "---\nname: " + name + "\ndescription: Updated.\n---\n\nNew body.\n"
		updated.Files = []skill.File{}
		updated.Version = 2
		updated.UpdatedAt = "2026-09-29T00:00:00.000Z"
		saveSkill(t, store, updated)

		got := loadSkill(t, store, name)
		check.True(t, sameSkillRecord(got, updated), "loaded %+v, want %+v", got, updated)
		same := 0
		for _, r := range listSkills(t, store) {
			if r.Name == name {
				same++
			}
		}
		check.Equal(t, same, 1, "records named %s in the list", name)
	})

	t.Run("list() returns every saved record", func(t *testing.T) {
		store := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store))
		p := runPrefix()
		a := skillRecordFor(p + "-list-a")
		b := skillRecordFor(p + "-list-b")
		b.CreatedBy = ""

		saveSkill(t, store, a)
		saveSkill(t, store, b)

		var names []string
		for _, r := range listSkills(t, store) {
			names = append(names, r.Name)
		}
		check.True(t, slices.Contains(names, a.Name), "%s is missing from the list", a.Name)
		check.True(t, slices.Contains(names, b.Name), "%s is missing from the list", b.Name)
	})

	t.Run("delete() removes the record and ignores unknown names", func(t *testing.T) {
		store := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store))
		p := runPrefix()
		name := p + "-to-delete"

		saveSkill(t, store, skillRecordFor(name))
		check.NoErr(t, store.Delete(t.Context(), name))

		got, err := store.Load(t.Context(), name)
		check.NoErr(t, err)
		check.True(t, got == nil, "a deleted record must be absent")
		check.NoErr(t, store.Delete(t.Context(), p+"-never-existed"))
	})

	t.Run("returned records are copies - mutating them does not change the store", func(t *testing.T) {
		store := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store))
		name := runPrefix() + "-immutable"
		want := skillRecordFor(name)

		saveSkill(t, store, skillRecordFor(name))
		loaded := loadSkill(t, store, name)
		loaded.Files = append(loaded.Files, skill.File{Path: "x.md", Content: "x"})
		loaded.Content = "changed"

		got := loadSkill(t, store, name)
		check.True(t, sameSkillRecord(got, want), "loaded %+v after a mutation of an earlier copy, want %+v", got, want)
	})

	// -- Go-only guarantees --

	t.Run("mutating a returned record does not change the store (Go-only)", func(t *testing.T) {
		store := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store))
		name := runPrefix() + "-copies"
		want := skillRecordFor(name)

		// The record handed to Save is the caller's again: changing its Files
		// elements and its slice must not reach the store.
		given := skillRecordFor(name)
		saveSkill(t, store, given)
		given.Files[0].Content = "changed after save"
		given.Files = append(given.Files, skill.File{Path: "y.md", Content: "y"})
		given.Files[0].Path = "changed.md"

		verify := func(label string) {
			t.Helper()
			got := loadSkill(t, store, name)
			check.True(t, sameSkillRecord(got, want), "%s: loaded %+v, want %+v", label, got, want)
		}
		verify("after mutating the record given to Save")

		// Change the element of a loaded record in place (the slice is shared if
		// the store hands out its own).
		loaded := loadSkill(t, store, name)
		if len(loaded.Files) > 0 {
			loaded.Files[0].Content = "changed after load"
			loaded.Files[0].Path = "changed.md"
		}
		verify("after mutating the Files of a loaded record")

		// Change the element of a listed record in place.
		for _, r := range listSkills(t, store) {
			if r.Name == name && len(r.Files) > 0 {
				r.Files[0].Content = "changed after list"
				r.Files[0].Path = "changed.md"
			}
		}
		verify("after mutating the Files of a listed record")
	})

	t.Run("concurrent Save of distinct and same names keeps one record per name (Go-only)", func(t *testing.T) {
		const writers = 16
		store := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store))
		p := runPrefix()
		sameName := p + "-conc-same"

		// Every writer saves its own distinct name AND the one shared name. The
		// shared-name records differ in content and version, so a torn or
		// duplicated record can be told from a whole one.
		names := make([]string, writers)
		variants := make([]persistence.SkillRecord, writers)
		for w := range writers {
			names[w] = fmt.Sprintf("%s-conc%d", p, w)
			variants[w] = skillRecordFor(sameName)
			variants[w].Content = fmt.Sprintf("---\nname: %s\ndescription: Writer %d.\n---\n\nBody %d.\n", sameName, w, w)
			variants[w].Version = w + 1
		}
		var wg sync.WaitGroup
		for w := range writers {
			wg.Go(func() {
				if err := store.Save(t.Context(), skillRecordFor(names[w])); err != nil {
					t.Errorf("save distinct from writer %d: %v", w, err)
				}
				if err := store.Save(t.Context(), variants[w]); err != nil {
					t.Errorf("save same name from writer %d: %v", w, err)
				}
			})
		}
		wg.Wait()

		listed := listSkills(t, store)
		for _, name := range names {
			want := skillRecordFor(name)
			got := loadSkill(t, store, name)
			check.True(t, sameSkillRecord(got, want), "loaded %+v, want %+v", got, want)
			same := 0
			for _, r := range listed {
				if r.Name == name {
					same++
				}
			}
			check.Equal(t, same, 1, "records named %s in the list", name)
		}

		// The shared name holds exactly one record, and it is one writer's record
		// as a whole (upsert-by-name: the last write replaces, none is merged).
		same := 0
		for _, r := range listed {
			if r.Name == sameName {
				same++
			}
		}
		check.Equal(t, same, 1, "records named %s in the list", sameName)
		got := loadSkill(t, store, sameName)
		check.True(t, slices.ContainsFunc(variants, func(v persistence.SkillRecord) bool { return sameSkillRecord(got, v) }),
			"loaded %+v, want the record of exactly one writer", got)
	})
}
