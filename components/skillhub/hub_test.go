package skillhub_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Phong95/aec-agent-go/components/persistence"
	"github.com/Phong95/aec-agent-go/components/skillhub"
	"github.com/Phong95/aec-agent-go/components/skillhub/skill"
	"github.com/Phong95/aec-agent-go/internal/jsonx"
	"github.com/Phong95/aec-agent-go/internal/wiretest"
	"github.com/Phong95/aec-agent-go/kernel"
)

// --- helpers --------------------------------------------------------------------------

// md is the SKILL.md text of the hub tests.
func md(name string) string {
	return mdWith(name, fmt.Sprintf("Does %s.", name), "Step 1.")
}

func mdWith(name, description, body string) string {
	return fmt.Sprintf("---\nname: %s\ndescription: %s\n---\n\n%s\n", name, description, body)
}

// wiregenMD is the SKILL.md text the skillhub-errors case of wiregen builds.
func wiregenMD(name, body string) string {
	return fmt.Sprintf("---\nname: %s\ndescription: The %s skill.\n---\n%s\n", name, name, body)
}

func newHub(store persistence.SkillStore, opts ...skillhub.Option) *skillhub.SkillHub {
	return skillhub.NewSkillHub(func() persistence.SkillStore { return store }, opts...)
}

// skillErrorOf requires err to be a *kernel.SkillError with this code and
// returns it.
func skillErrorOf(t *testing.T, err error, code kernel.SkillErrorCode) *kernel.SkillError {
	t.Helper()
	var se *kernel.SkillError
	if !errors.As(err, &se) {
		t.Fatalf("error = %v (%T), want a *kernel.SkillError", err, err)
	}
	if se.Code != code {
		t.Fatalf("code = %q, want %q (message %q)", se.Code, code, se.Message)
	}
	return se
}

// expectSkillError is skillErrorOf for callers that only need the checks.
func expectSkillError(t *testing.T, err error, code kernel.SkillErrorCode) {
	t.Helper()
	_ = skillErrorOf(t, err, code)
}

// recorder is a slog.Handler that keeps the level and message of every record.
type recorder struct {
	mu     sync.Mutex
	levels []slog.Level
	msgs   []string
}

func (r *recorder) Enabled(context.Context, slog.Level) bool { return true }

func (r *recorder) Handle(_ context.Context, rec slog.Record) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.levels = append(r.levels, rec.Level)
	r.msgs = append(r.msgs, rec.Message)
	return nil
}

func (r *recorder) WithAttrs([]slog.Attr) slog.Handler { return r }
func (r *recorder) WithGroup(string) slog.Handler      { return r }

func (r *recorder) messages() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.msgs)
}

// flakyStore is a MemorySkillStore whose List can fail a number of times and that
// counts the List calls.
type flakyStore struct {
	*persistence.MemorySkillStore
	failures atomic.Int32
	lists    atomic.Int32
}

var errDBDown = errors.New("db down")

func (s *flakyStore) List(ctx context.Context) ([]persistence.SkillRecord, error) {
	s.lists.Add(1)
	if s.failures.Add(-1) >= 0 {
		return nil, errDBDown
	}
	return s.MemorySkillStore.List(ctx)
}

func newFlaky() *flakyStore {
	return &flakyStore{MemorySkillStore: persistence.NewMemorySkillStore()}
}

// goldenError is one entry of the errors object of skillhub-errors.json.
type goldenError struct {
	Message   string `json:"message"`
	Code      string `json:"code"`
	SkillName string `json:"skillName"`
}

type errorsFixture struct {
	Errors   map[string]goldenError `json:"errors"`
	Warnings struct {
		Seed  []persistence.SkillRecord `json:"seed"`
		Texts []string                  `json:"texts"`
		Names []string                  `json:"names"`
	} `json:"warnings"`
}

func loadErrorsFixture(t *testing.T) errorsFixture {
	t.Helper()
	var f errorsFixture
	if err := jsonx.UnmarshalStrict(wiretest.Load(t, "skillhub-errors.json"), &f); err != nil {
		t.Fatalf("decode skillhub-errors.json: %v", err)
	}
	return f
}

func mustRegister(t *testing.T, hub *skillhub.SkillHub, content string, source skill.Source) skill.Skill {
	t.Helper()
	sk, err := hub.Register(skill.Package{Content: content}, source)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	return sk
}

func mustCreate(t *testing.T, hub *skillhub.SkillHub, pkg skill.Package, opts skillhub.CreateOptions) skill.Skill {
	t.Helper()
	sk, err := hub.Create(t.Context(), pkg, opts)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return sk
}

// --- the five hub-level tests ---------------------------------------------------------
// The three AgentCore-level tests (essential registration through AgentCore, useSkill,
// usePersistence) run with AgentCore in Phase 10.

func TestSkillHubCreatesUpdatesAndDeletesCustomSkillsInTheStore(t *testing.T) {
	ctx := t.Context()
	store := persistence.NewMemorySkillStore()
	hub := newHub(store, skillhub.WithClock(wiretest.Clock(1700000000000, 1000)))

	created := mustCreate(t, hub, skill.Package{
		Content: md("site-diary"),
		Files:   []skill.File{{Path: "assets/template.md", Content: "T"}},
	}, skillhub.CreateOptions{CreatedBy: "agent-1"})
	if created.Name != "site-diary" || created.Source != skill.SourceCustom || created.Version != 1 || created.CreatedBy != "agent-1" {
		t.Errorf("created = %+v", created)
	}
	record, err := store.Load(ctx, "site-diary")
	if err != nil || record == nil || record.Name != "site-diary" || record.Version != 1 || record.CreatedBy != "agent-1" {
		t.Errorf("stored record = %+v, %v", record, err)
	}

	updated, err := hub.Update(ctx, "site-diary", skill.Package{
		Content: mdWith("site-diary", "Writes the daily site diary.", "Step 1."),
		Files:   []skill.File{},
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.Description != "Writes the daily site diary." || updated.Version != 2 || updated.CreatedBy != "agent-1" || len(updated.Files) != 0 {
		t.Errorf("updated = %+v", updated)
	}
	if updated.CreatedAt != created.CreatedAt {
		t.Errorf("createdAt changed from %q to %q", created.CreatedAt, updated.CreatedAt)
	}
	record, err = store.Load(ctx, "site-diary")
	if err != nil || record == nil || record.Version != 2 {
		t.Errorf("stored record after update = %+v, %v", record, err)
	}

	if err := hub.Delete(ctx, "site-diary"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, found, err := hub.Get(ctx, "site-diary"); err != nil || found {
		t.Errorf("Get after delete = found %v, %v", found, err)
	}
	if record, err := store.Load(ctx, "site-diary"); err != nil || record != nil {
		t.Errorf("record after delete = %+v, %v", record, err)
	}
}

func TestSkillHubRejectsNamesAlreadyInTheLibrary(t *testing.T) {
	ctx := t.Context()
	hub := newHub(persistence.NewMemorySkillStore())
	mustRegister(t, hub, md("plan"), skill.SourceBuiltIn)
	_, err := hub.Create(ctx, skill.Package{Content: md("plan")}, skillhub.CreateOptions{})
	expectSkillError(t, err, kernel.SkillCodeAlreadyExists)
	mustCreate(t, hub, skill.Package{Content: md("custom-one")}, skillhub.CreateOptions{})
	_, err = hub.Create(ctx, skill.Package{Content: md("custom-one")}, skillhub.CreateOptions{})
	expectSkillError(t, err, kernel.SkillCodeAlreadyExists)
}

func TestSkillHubBuiltInAndAppSkillsAreReadOnlyRenamesAreRejected(t *testing.T) {
	ctx := t.Context()
	hub := newHub(persistence.NewMemorySkillStore())
	mustRegister(t, hub, md("plan"), skill.SourceBuiltIn)
	_, err := hub.Update(ctx, "plan", skill.Package{Content: mdWith("plan", "changed", "Step 1.")})
	expectSkillError(t, err, kernel.SkillCodeReadOnly)
	expectSkillError(t, hub.Delete(ctx, "plan"), kernel.SkillCodeReadOnly)
	expectSkillError(t, hub.Delete(ctx, "missing"), kernel.SkillCodeNotFound)

	mustCreate(t, hub, skill.Package{Content: md("takeoff")}, skillhub.CreateOptions{})
	_, err = hub.Update(ctx, "takeoff", skill.Package{Content: md("takeoff-v2")})
	se := skillErrorOf(t, err, kernel.SkillCodeInvalid)
	if want := "cannot be renamed"; !strings.Contains(se.Message, want) {
		t.Errorf("message %q does not contain %q", se.Message, want)
	}
}

func TestSkillHubLoadsCustomSkillsFromTheStoreSkippingInvalidOrShadowedRecords(t *testing.T) {
	ctx := t.Context()
	store := persistence.NewMemorySkillStore()
	now := kernel.NowISO()
	for _, r := range []persistence.SkillRecord{
		{Name: "stored-ok", Content: md("stored-ok"), Files: []skill.File{}, Version: 3, CreatedAt: now, UpdatedAt: now},
		{Name: "broken", Content: "not a skill", Files: []skill.File{}, Version: 1, CreatedAt: now, UpdatedAt: now},
		{Name: "plan", Content: md("plan"), Files: []skill.File{}, Version: 1, CreatedAt: now, UpdatedAt: now},
	} {
		if err := store.Save(ctx, r); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	rec := &recorder{}
	hub := newHub(store, skillhub.WithLogger(slog.New(rec)))
	mustRegister(t, hub, mdWith("plan", "The built-in plan.", "Step 1."), skill.SourceBuiltIn)

	skills, err := hub.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var got []string
	for _, s := range skills {
		got = append(got, s.Name+":"+string(s.Source))
	}
	if want := []string{"plan:built-in", "stored-ok:custom"}; !slices.Equal(got, want) {
		t.Errorf("List() = %v, want %v", got, want)
	}
	stored, found, err := hub.Get(ctx, "stored-ok")
	if err != nil || !found || stored.Version != 3 {
		t.Errorf("Get(stored-ok) = %+v, %v, %v", stored, found, err)
	}
	if n := len(rec.messages()); n != 2 {
		t.Errorf("%d warnings logged, want 2: %q", n, rec.messages())
	}
}

func TestSkillHubRetriesAFailedStoreLoadOnTheNextRead(t *testing.T) {
	ctx := t.Context()
	store := newFlaky()
	store.failures.Store(1)
	hub := newHub(store)
	if _, err := hub.List(ctx); !errors.Is(err, errDBDown) {
		t.Fatalf("first List error = %v, want %v", err, errDBDown)
	}
	skills, err := hub.List(ctx)
	if err != nil || len(skills) != 0 {
		t.Errorf("second List = %v, %v", skills, err)
	}
}

// --- error goldens ----------------------------------------------------------------------

// TestErrorsMatchGolden drives the Go hub through the error paths of the wiregen case
// and compares message, code and skillName with the golden.
func TestErrorsMatchGolden(t *testing.T) {
	ctx := t.Context()
	golden := loadErrorsFixture(t).Errors

	hub := newHub(persistence.NewMemorySkillStore())
	mustRegister(t, hub, wiregenMD("app-one", "App body."), skill.SourceApp)
	mustRegister(t, hub, wiregenMD("built-one", "Built-in body."), skill.SourceBuiltIn)
	mustCreate(t, hub, skill.Package{Content: wiregenMD("custom-one", "Custom body.")}, skillhub.CreateOptions{CreatedBy: "agent-1"})

	noFrontmatter := skill.Package{Content: "no frontmatter at all"}
	paths := []struct {
		label string
		run   func() error
	}{
		{"registerTwice", func() error {
			_, err := hub.Register(skill.Package{Content: wiregenMD("app-one", "Again.")}, skill.SourceApp)
			return err
		}},
		{"registerInvalid", func() error { _, err := hub.Register(noFrontmatter, skill.SourceApp); return err }},
		{"createExistsRegistered", func() error {
			_, err := hub.Create(ctx, skill.Package{Content: wiregenMD("app-one", "Shadow attempt.")}, skillhub.CreateOptions{})
			return err
		}},
		{"createExistsCustom", func() error {
			_, err := hub.Create(ctx, skill.Package{Content: wiregenMD("custom-one", "Twice.")}, skillhub.CreateOptions{})
			return err
		}},
		{"createInvalid", func() error { _, err := hub.Create(ctx, noFrontmatter, skillhub.CreateOptions{}); return err }},
		{"updateReadOnlyApp", func() error {
			_, err := hub.Update(ctx, "app-one", skill.Package{Content: wiregenMD("app-one", "Edit.")})
			return err
		}},
		{"updateReadOnlyBuiltIn", func() error {
			_, err := hub.Update(ctx, "built-one", skill.Package{Content: wiregenMD("built-one", "Edit.")})
			return err
		}},
		{"deleteReadOnlyApp", func() error { return hub.Delete(ctx, "app-one") }},
		{"updateNotFound", func() error {
			_, err := hub.Update(ctx, "missing", skill.Package{Content: wiregenMD("missing", "Edit.")})
			return err
		}},
		{"deleteNotFound", func() error { return hub.Delete(ctx, "missing") }},
		{"updateRename", func() error {
			_, err := hub.Update(ctx, "custom-one", skill.Package{Content: wiregenMD("other-name", "Renamed.")})
			return err
		}},
		{"updateInvalid", func() error { _, err := hub.Update(ctx, "custom-one", noFrontmatter); return err }},
	}
	if len(paths) != len(golden) {
		t.Fatalf("the test drives %d error paths, the golden has %d", len(paths), len(golden))
	}
	for _, p := range paths {
		want, ok := golden[p.label]
		if !ok {
			t.Errorf("%s: no golden entry", p.label)
			continue
		}
		err := p.run()
		var se *kernel.SkillError
		if !errors.As(err, &se) {
			t.Errorf("%s: error = %v (%T), want a *kernel.SkillError", p.label, err, err)
			continue
		}
		if se.Message != want.Message || string(se.Code) != want.Code || se.SkillName != want.SkillName {
			t.Errorf("%s:\n got: %q %q %q\nwant: %q %q %q", p.label, se.Message, se.Code, se.SkillName, want.Message, want.Code, want.SkillName)
		}
	}
}

func TestWarningsMatchGolden(t *testing.T) {
	ctx := t.Context()
	f := loadErrorsFixture(t)
	store := persistence.NewMemorySkillStore()
	for _, r := range f.Warnings.Seed {
		if err := store.Save(ctx, r); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	rec := &recorder{}
	hub := newHub(store, skillhub.WithLogger(slog.New(rec)))
	mustRegister(t, hub, wiregenMD("app-one", "App body."), skill.SourceApp)

	skills, err := hub.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var names []string
	for _, s := range skills {
		names = append(names, s.Name)
	}
	if !slices.Equal(names, f.Warnings.Names) {
		t.Errorf("List() names = %v, TS %v", names, f.Warnings.Names)
	}
	if got := rec.messages(); !slices.Equal(got, f.Warnings.Texts) {
		t.Errorf("warnings differ from the TS texts\n got: %q\nwant: %q", got, f.Warnings.Texts)
	}
	for _, level := range rec.levels {
		if level != slog.LevelWarn {
			t.Errorf("a warning was logged at level %v", level)
		}
	}

	// The skipped records are warned about once: the load is cached.
	if _, err := hub.List(ctx); err != nil {
		t.Fatalf("second List: %v", err)
	}
	if n := len(rec.messages()); n != 2 {
		t.Errorf("%d warnings after a second List, want 2", n)
	}
}

// --- behaviours -------------------------------------------------------------------------

func TestUpdateKeepsProvenance(t *testing.T) {
	ctx := t.Context()
	store := persistence.NewMemorySkillStore()
	hub := newHub(store, skillhub.WithClock(wiretest.Clock(1700000000000, 1000)))
	created := mustCreate(t, hub, skill.Package{Content: md("site-diary")}, skillhub.CreateOptions{CreatedBy: "agent-1"})
	if created.CreatedAt != "2023-11-14T22:13:20.000Z" || created.UpdatedAt != created.CreatedAt {
		t.Fatalf("created timestamps = %q, %q", created.CreatedAt, created.UpdatedAt)
	}

	updated, err := hub.Update(ctx, "site-diary", skill.Package{Content: mdWith("site-diary", "Changed.", "Step 2.")})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.Version != 2 || updated.CreatedBy != "agent-1" || updated.CreatedAt != created.CreatedAt {
		t.Errorf("updated = %+v", updated)
	}
	if updated.UpdatedAt != "2023-11-14T22:13:21.000Z" {
		t.Errorf("updatedAt = %q, want the second clock tick", updated.UpdatedAt)
	}
	record, err := store.Load(ctx, "site-diary")
	if err != nil || record == nil {
		t.Fatalf("Load: %+v, %v", record, err)
	}
	if record.Version != 2 || record.CreatedBy != "agent-1" || record.CreatedAt != created.CreatedAt || record.UpdatedAt != updated.UpdatedAt {
		t.Errorf("stored record = %+v", record)
	}
	got, found, err := hub.Get(ctx, "site-diary")
	if err != nil || !found || got.Description != "Changed." {
		t.Errorf("Get after update = %+v, %v, %v", got, found, err)
	}

	// An agent that created nothing leaves createdBy absent.
	other := mustCreate(t, hub, skill.Package{Content: md("other")}, skillhub.CreateOptions{})
	if other.CreatedBy != "" {
		t.Errorf("createdBy = %q, want empty", other.CreatedBy)
	}
}

func TestDeleteNotifiesListeners(t *testing.T) {
	ctx := t.Context()
	store := persistence.NewMemorySkillStore()
	hub := newHub(store)
	mustCreate(t, hub, skill.Package{Content: md("one")}, skillhub.CreateOptions{})
	mustCreate(t, hub, skill.Package{Content: md("two")}, skillhub.CreateOptions{})

	var order []string
	stillStored := false
	unsubscribeFirst := hub.OnDeleted(func(name string) {
		order = append(order, "first:"+name)
		if r, err := store.Load(ctx, name); err == nil && r != nil {
			stillStored = true
		}
	})
	hub.OnDeleted(func(name string) { order = append(order, "second:"+name) })
	unsubscribed := hub.OnDeleted(func(name string) { order = append(order, "unsubscribed:"+name) })
	unsubscribed()
	unsubscribed() // idempotent

	if err := hub.Delete(ctx, "one"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if want := []string{"first:one", "second:one"}; !slices.Equal(order, want) {
		t.Errorf("listener calls = %v, want %v", order, want)
	}
	if stillStored {
		t.Error("a listener ran before the record left the store")
	}

	unsubscribeFirst()
	if err := hub.Delete(ctx, "two"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if want := []string{"first:one", "second:one", "second:two"}; !slices.Equal(order, want) {
		t.Errorf("listener calls after unsubscribe = %v, want %v", order, want)
	}

	// A failed delete notifies nobody.
	order = nil
	if err := hub.Delete(ctx, "two"); err == nil {
		t.Fatal("deleting an unknown skill must fail")
	}
	if len(order) != 0 {
		t.Errorf("listeners ran for a failed delete: %v", order)
	}
}

func TestListenerMayReenterHub(t *testing.T) {
	ctx := t.Context()
	hub := newHub(persistence.NewMemorySkillStore())
	mustCreate(t, hub, skill.Package{Content: md("one")}, skillhub.CreateOptions{})
	var listed []string
	var createErr error
	hub.OnDeleted(func(string) {
		skills, err := hub.List(ctx)
		if err != nil {
			createErr = err
			return
		}
		for _, s := range skills {
			listed = append(listed, s.Name)
		}
		// The listener may also write: no lock is held while it runs.
		_, createErr = hub.Create(ctx, skill.Package{Content: md("from-listener")}, skillhub.CreateOptions{})
	})

	done := make(chan error, 1)
	go func() { done <- hub.Delete(ctx, "one") }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Delete: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Delete deadlocked: a listener that calls back into the hub never returned")
	}
	if len(listed) != 0 {
		t.Errorf("the listener saw %v, want an empty library (the skill is already deleted)", listed)
	}
	if createErr != nil {
		t.Errorf("the listener's Create failed: %v", createErr)
	}
	if _, found, _ := hub.Get(ctx, "from-listener"); !found {
		t.Error("the skill created by the listener is missing")
	}
}

func TestFailedLoadIsRetried(t *testing.T) {
	ctx := t.Context()
	store := newFlaky()
	store.failures.Store(1)
	hub := newHub(store)

	if _, _, err := hub.Get(ctx, "x"); !errors.Is(err, errDBDown) {
		t.Fatalf("first Get error = %v, want %v", err, errDBDown)
	}
	if _, _, err := hub.Get(ctx, "x"); err != nil {
		t.Fatalf("second Get error = %v, want the load to be retried", err)
	}
	if _, err := hub.List(ctx); err != nil {
		t.Fatalf("List: %v", err)
	}
	if n := store.lists.Load(); n != 2 {
		t.Errorf("the store was listed %d times, want 2 (one failure, one cached success)", n)
	}

	// Create and Update surface a failed load too, and write nothing.
	failing := newFlaky()
	failing.failures.Store(1)
	failingHub := newHub(failing)
	if _, err := failingHub.Create(ctx, skill.Package{Content: md("a")}, skillhub.CreateOptions{}); !errors.Is(err, errDBDown) {
		t.Fatalf("Create error = %v, want %v", err, errDBDown)
	}
	if records, _ := failing.MemorySkillStore.List(ctx); len(records) != 0 {
		t.Errorf("a failed load still wrote %d records", len(records))
	}
}

func TestReloadForgetsCache(t *testing.T) {
	ctx := t.Context()
	store := newFlaky()
	hub := newHub(store)
	if _, err := hub.List(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := hub.List(ctx); err != nil {
		t.Fatal(err)
	}
	if n := store.lists.Load(); n != 1 {
		t.Fatalf("store listed %d times for two reads, want 1", n)
	}

	// A record written behind the hub's back is invisible until Reload.
	now := kernel.NowISO()
	if err := store.Save(ctx, persistence.SkillRecord{Name: "late", Content: md("late"), Version: 1, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := hub.Get(ctx, "late"); found {
		t.Fatal("the cache must hide a record written behind the hub")
	}
	hub.Reload()
	if _, found, err := hub.Get(ctx, "late"); err != nil || !found {
		t.Fatalf("Get(late) after Reload = %v, %v", found, err)
	}
	if n := store.lists.Load(); n != 2 {
		t.Errorf("store listed %d times, want 2 (Reload forgot the cache)", n)
	}
}

// blockingStore is a MemorySkillStore whose first List signals it has started, reads
// its snapshot, and waits to be released.
type blockingStore struct {
	*persistence.MemorySkillStore
	first   sync.Once
	entered chan struct{}
	release chan struct{}
}

func (s *blockingStore) List(ctx context.Context) ([]persistence.SkillRecord, error) {
	records, err := s.MemorySkillStore.List(ctx)
	s.first.Do(func() {
		close(s.entered)
		<-s.release
	})
	return records, err
}

// TestReloadDiscardsARunningLoad proves a load that was running when Reload was
// called does not cache its (stale) result.
func TestReloadDiscardsARunningLoad(t *testing.T) {
	ctx := t.Context()
	store := &blockingStore{
		MemorySkillStore: persistence.NewMemorySkillStore(),
		entered:          make(chan struct{}),
		release:          make(chan struct{}),
	}
	hub := newHub(store)

	type result struct {
		skills []skill.Skill
		err    error
	}
	done := make(chan result, 1)
	go func() {
		skills, err := hub.List(ctx)
		done <- result{skills, err}
	}()
	<-store.entered
	now := kernel.NowISO()
	if err := store.Save(ctx, persistence.SkillRecord{Name: "fresh", Content: md("fresh"), Version: 1, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	hub.Reload()
	close(store.release)

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("List: %v", r.err)
		}
		if len(r.skills) != 1 || r.skills[0].Name != "fresh" {
			t.Errorf("List() = %v, want the record written before Reload", r.skills)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("List never returned")
	}
}

func TestConcurrentCreateSameName(t *testing.T) {
	const n = 16
	ctx := t.Context()
	store := persistence.NewMemorySkillStore()
	hub := newHub(store)

	start := make(chan struct{})
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, errs[i] = hub.Create(ctx, skill.Package{Content: md("same-name")}, skillhub.CreateOptions{CreatedBy: fmt.Sprintf("agent-%d", i)})
		}()
	}
	close(start)
	wg.Wait()

	successes := 0
	for _, err := range errs {
		if err == nil {
			successes++
			continue
		}
		expectSkillError(t, err, kernel.SkillCodeAlreadyExists)
	}
	if successes != 1 {
		t.Errorf("%d Create calls succeeded, want exactly 1", successes)
	}
	records, err := store.List(ctx)
	if err != nil || len(records) != 1 {
		t.Errorf("store holds %d records, %v; want 1", len(records), err)
	}
}

func TestConcurrentReadsDuringMutation(t *testing.T) {
	ctx := t.Context()
	hub := newHub(persistence.NewMemorySkillStore())
	mustRegister(t, hub, md("registered"), skill.SourceBuiltIn)

	ignoreSkillErrors := func(err error) error {
		var se *kernel.SkillError
		if err == nil || errors.As(err, &se) {
			return nil
		}
		return err
	}

	const rounds = 40
	var wg sync.WaitGroup
	failures := make(chan error, 64)
	report := func(err error) {
		if err = ignoreSkillErrors(err); err != nil {
			select {
			case failures <- err:
			default:
			}
		}
	}
	run := func(f func(i int)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range rounds {
				f(i)
			}
		}()
	}

	for w := range 3 {
		run(func(i int) {
			name := fmt.Sprintf("skill-%d-%d", w, i%5)
			_, err := hub.Create(ctx, skill.Package{Content: md(name)}, skillhub.CreateOptions{})
			report(err)
			_, err = hub.Update(ctx, name, skill.Package{Content: mdWith(name, "Updated.", "Step 2.")})
			report(err)
			report(hub.Delete(ctx, name))
		})
	}
	run(func(int) { hub.Reload() })
	for range 3 {
		run(func(i int) {
			_, err := hub.List(ctx)
			report(err)
			_, _, err = hub.Get(ctx, fmt.Sprintf("skill-0-%d", i%5))
			report(err)
			_, err = hub.GetByNames(ctx, []string{"registered", "skill-1-0", "missing"})
			report(err)
		})
	}
	run(func(int) {
		unsubscribe := hub.OnDeleted(func(string) {})
		unsubscribe()
	})
	wg.Wait()
	close(failures)
	for err := range failures {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestReturnedSkillIsACopy(t *testing.T) {
	ctx := t.Context()
	hub := newHub(persistence.NewMemorySkillStore())
	content := "---\nname: copy-me\ndescription: Copy test.\nallowed-tools: read_file terminal\nmetadata:\n  requires-tools: read_file\n  author: aec\n---\n\nStep 1.\n"
	pkg := skill.Package{Content: content, Files: []skill.File{{Path: "references/a.md", Content: "a"}}}
	registered, err := hub.Register(pkg, skill.SourceApp)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	encode := func(sk skill.Skill) string {
		b, err := kernel.EncodeJSON(sk)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	want := encode(registered)

	scribble := func(sk *skill.Skill) {
		sk.Metadata.Set("scribble", "x")
		sk.Metadata.Set("author", "changed")
		if len(sk.Files) > 0 {
			sk.Files[0].Content = "changed"
		}
		sk.Files = append(sk.Files, skill.File{Path: "extra.md"})
		if len(sk.RequiresTools) > 0 {
			sk.RequiresTools[0] = "changed"
		}
		if len(sk.AllowedTools) > 0 {
			sk.AllowedTools[0] = "changed"
		}
		sk.Name = "changed"
	}
	scribble(&registered)

	got, found, err := hub.Get(ctx, "copy-me")
	if err != nil || !found {
		t.Fatalf("Get = %v, %v", found, err)
	}
	if encode(got) != want {
		t.Errorf("mutating the Register result changed the stored skill\n got: %s\nwant: %s", encode(got), want)
	}
	scribble(&got)
	list, _ := hub.List(ctx)
	byNames, _ := hub.GetByNames(ctx, []string{"copy-me"})
	if encode(list[0]) != want || encode(byNames[0]) != want {
		t.Error("mutating the Get result changed what List or GetByNames return")
	}
	scribble(&list[0])
	scribble(&byNames[0])
	again, _, _ := hub.Get(ctx, "copy-me")
	if encode(again) != want {
		t.Error("mutating a List or GetByNames result changed the stored skill")
	}

	created := mustCreate(t, hub, pkg2("copy-custom"), skillhub.CreateOptions{})
	wantCustom := encode(created)
	scribble(&created)
	gotCustom, _, _ := hub.Get(ctx, "copy-custom")
	if encode(gotCustom) != wantCustom {
		t.Error("mutating the Create result changed the stored skill")
	}
}

func pkg2(name string) skill.Package {
	return skill.Package{
		Content: "---\nname: " + name + "\ndescription: Custom copy test.\nmetadata:\n  requires-tools: read_file\n---\n\nStep 1.\n",
		Files:   []skill.File{{Path: "references/a.md", Content: "a"}},
	}
}

func TestGetByNamesKeepsTheGivenOrderAndDropsUnknownNames(t *testing.T) {
	ctx := t.Context()
	hub := newHub(persistence.NewMemorySkillStore())
	mustRegister(t, hub, md("zeta"), skill.SourceApp)
	mustCreate(t, hub, skill.Package{Content: md("alpha")}, skillhub.CreateOptions{})
	got, err := hub.GetByNames(ctx, []string{"alpha", "missing", "zeta"})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, s := range got {
		names = append(names, s.Name)
	}
	if want := []string{"alpha", "zeta"}; !slices.Equal(names, want) {
		t.Errorf("GetByNames = %v, want %v", names, want)
	}
}

func TestListIsRegisteredFirstThenCustomEachSortedByName(t *testing.T) {
	ctx := t.Context()
	hub := newHub(persistence.NewMemorySkillStore())
	mustRegister(t, hub, md("pear"), skill.SourceApp)
	mustRegister(t, hub, md("apple"), skill.SourceBuiltIn)
	mustCreate(t, hub, skill.Package{Content: md("zucchini")}, skillhub.CreateOptions{})
	mustCreate(t, hub, skill.Package{Content: md("a-1")}, skillhub.CreateOptions{})
	list, err := hub.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, s := range list {
		names = append(names, s.Name)
	}
	if want := []string{"apple", "pear", "a-1", "zucchini"}; !slices.Equal(names, want) {
		t.Errorf("List = %v, want %v", names, want)
	}
}

func TestRegisterSourceRules(t *testing.T) {
	hub := newHub(persistence.NewMemorySkillStore())
	app, err := hub.Register(skill.Package{Content: md("defaulted")}, "")
	if err != nil || app.Source != skill.SourceApp || app.Version != 1 {
		t.Errorf("empty source = %+v, %v; want an app skill", app, err)
	}
	if _, err := hub.Register(skill.Package{Content: md("customised")}, skill.SourceCustom); err == nil {
		t.Error("registering a custom skill must fail")
	}
	if _, err := hub.Register(skill.Package{Content: md("odd")}, skill.Source("mystery")); err == nil {
		t.Error("registering an unknown source must fail")
	}
}

func TestMissingStoreIsAnError(t *testing.T) {
	ctx := t.Context()
	var typedNil *persistence.MemorySkillStore
	for name, hub := range map[string]*skillhub.SkillHub{
		"nil getter":  skillhub.NewSkillHub(nil),
		"nil result":  skillhub.NewSkillHub(func() persistence.SkillStore { return nil }),
		"typed nil":   skillhub.NewSkillHub(func() persistence.SkillStore { return typedNil }),
		"nil options": skillhub.NewSkillHub(nil, nil, skillhub.WithClock(nil), skillhub.WithLogger(nil)),
	} {
		if _, err := hub.List(ctx); err == nil {
			t.Errorf("%s: List without a store must fail", name)
		}
		if _, err := hub.Create(ctx, skill.Package{Content: md("a")}, skillhub.CreateOptions{}); err == nil {
			t.Errorf("%s: Create without a store must fail", name)
		}
	}
	// A registered skill is found without touching the store.
	hub := skillhub.NewSkillHub(nil)
	mustRegister(t, hub, md("plan"), skill.SourceBuiltIn)
	if _, found, err := hub.Get(ctx, "plan"); err != nil || !found {
		t.Errorf("Get(plan) = %v, %v", found, err)
	}
}

// TestStoreIsResolvedOnEveryUse proves Kernel.UsePersistence can swap the store: the
// getter is called again on each load and write.
func TestStoreIsResolvedOnEveryUse(t *testing.T) {
	ctx := t.Context()
	first := persistence.NewMemorySkillStore()
	second := persistence.NewMemorySkillStore()
	current := persistence.SkillStore(first)
	hub := skillhub.NewSkillHub(func() persistence.SkillStore { return current })

	mustCreate(t, hub, skill.Package{Content: md("in-first")}, skillhub.CreateOptions{})
	current = second
	hub.Reload()
	if _, found, _ := hub.Get(ctx, "in-first"); found {
		t.Error("after the swap and Reload the first store's skill must be gone")
	}
	mustCreate(t, hub, skill.Package{Content: md("in-second")}, skillhub.CreateOptions{})
	if r, _ := second.Load(ctx, "in-second"); r == nil {
		t.Error("the write went to the old store")
	}
	if r, _ := first.Load(ctx, "in-second"); r != nil {
		t.Error("the write reached the old store")
	}
}
