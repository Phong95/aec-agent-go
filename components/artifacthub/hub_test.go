package artifacthub_test

import (
	"fmt"
	"sync"
	"testing"

	"github.com/Phong95/aec-agent-go/components/artifacthub"
	"github.com/Phong95/aec-agent-go/components/artifacthub/artifact"
	"github.com/Phong95/aec-agent-go/components/persistence"
	"github.com/Phong95/aec-agent-go/kernel"
)

// TestArtifactHubFailsClosedWhenNoStoreIsWired checks that the hub fails closed when
// no store is wired: every call of a view fails with not_configured, whether the
// store function is nil, returns nil, or returns a typed nil.
func TestArtifactHubFailsClosedWhenNoStoreIsWired(t *testing.T) {
	ctx := t.Context()
	var typedNil *persistence.MemoryArtifactStore
	getters := map[string]func() persistence.ArtifactStore{
		"nil getStore":  nil,
		"returns nil":   func() persistence.ArtifactStore { return nil },
		"returns typed": func() persistence.ArtifactStore { return typedNil },
	}
	for name, get := range getters {
		t.Run(name, func(t *testing.T) {
			hub := artifacthub.NewArtifactHub(get)
			v, err := hub.ForAgent("a1", "")
			if err != nil {
				t.Fatalf("ForAgent must not touch the store: %v", err)
			}
			fv := newView(t, "docker:shared", "a1") //nolint:contextcheck // newView builds its own in-memory sandbox
			body := "x"
			_, err = v.List(ctx, artifact.Query{})
			requireCode(t, err, kernel.ArtifactCodeNotConfigured)
			_, err = v.Save(ctx, artifact.SaveInput{Name: "a.md", Type: "text/markdown", Content: &body}, fv)
			requireCode(t, err, kernel.ArtifactCodeNotConfigured)
			_, err = v.Update(ctx, artifact.UpdateInput{Ref: artifact.Ref{Name: "a.md"}}, fv)
			requireCode(t, err, kernel.ArtifactCodeNotConfigured)
			_, err = v.Get(ctx, artifact.Ref{Name: "a.md"})
			requireCode(t, err, kernel.ArtifactCodeNotConfigured)
			_, err = v.Read(ctx, artifact.Ref{Name: "a.md"}, fv, artifact.ReadOptions{})
			requireCode(t, err, kernel.ArtifactCodeNotConfigured)
			_, err = v.Versions(ctx, artifact.Ref{Name: "a.md"})
			requireCode(t, err, kernel.ArtifactCodeNotConfigured)
			requireCode(t, v.Delete(ctx, artifact.Ref{Name: "a.md"}), kernel.ArtifactCodeNotConfigured)
			ae := expectCode(t, hub.ReleaseAgentData(ctx, "a1"), kernel.ArtifactCodeNotConfigured)
			if ae.Message != "No artifact store is configured on this server" {
				t.Fatalf("message = %q", ae.Message)
			}
			if st, err := fv.Stat(ctx, "artifacts/a.md"); st != nil || err != nil {
				t.Fatalf("a failed-closed save wrote a file: %v, %v", st, err)
			}
		})
	}
}

// TestAgentOwnershipHidesAnotherAgentsArtifactEvenWhenItsIdIsKnown checks that the hub
// hides another agent's artifact, even when its id is known: one agent can never
// read, update, list, version or delete another agent's artifact, it gets the same
// not_found as for a missing id, and the two agents may use the same name.
func TestAgentOwnershipHidesAnotherAgentsArtifactEvenWhenItsIdIsKnown(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	mine := e.view(t, "a1", "s1")
	theirs := e.view(t, "a2", "s1")
	w := save(t, mine, e.fv, "secret.md", "text/markdown", "mine")
	ref := artifact.Ref{ID: w.Artifact.ID}

	_, err := theirs.Read(ctx, ref, e.fv, artifact.ReadOptions{})
	requireCode(t, err, kernel.ArtifactCodeNotFound)
	hijack := "hijacked"
	_, err = theirs.Update(ctx, artifact.UpdateInput{Ref: ref, Content: &hijack}, e.fv)
	requireCode(t, err, kernel.ArtifactCodeNotFound)
	requireCode(t, theirs.Delete(ctx, ref), kernel.ArtifactCodeNotFound)
	_, err = theirs.Get(ctx, ref)
	requireCode(t, err, kernel.ArtifactCodeNotFound)
	_, err = theirs.Versions(ctx, ref)
	requireCode(t, err, kernel.ArtifactCodeNotFound)
	if list, err := theirs.List(ctx, artifact.Query{}); err != nil || len(list) != 0 {
		t.Fatalf("theirs.List = %v, %v; want empty", list, err)
	}
	// The failed calls changed nothing: the artifact is intact for its owner.
	if got, err := mine.Get(ctx, ref); err != nil || got.Version != 1 || got.AgentID != "a1" {
		t.Fatalf("mine.Get = %+v, %v", got, err)
	}

	// Looking at another agent's name is a miss, not a hit.
	_, err = theirs.Get(ctx, artifact.Ref{Name: "secret.md"})
	requireCode(t, err, kernel.ArtifactCodeNotFound)

	// A foreign id is indistinguishable from a missing one.
	_, missingErr := theirs.Get(ctx, artifact.Ref{ID: "missing-id"})
	_, foreignErr := theirs.Get(ctx, ref)
	missing, foreign := artifactError(t, missingErr), artifactError(t, foreignErr)
	if missing.Code != foreign.Code || (missing.Message == "") != (foreign.Message == "") {
		t.Fatalf("missing %+v and foreign %+v must look alike", missing, foreign)
	}

	// Two agents may use the same name without colliding.
	other := "theirs"
	got, err := theirs.Save(ctx, artifact.SaveInput{Name: "secret.md", Type: "text/markdown", Content: &other}, e.fv)
	if err != nil || got.Artifact.AgentID != "a2" || got.Artifact.Version != 1 {
		t.Fatalf("theirs.Save = %+v, %v", got, err)
	}
	if list, err := mine.List(ctx, artifact.Query{}); err != nil || len(list) != 1 || list[0].AgentID != "a1" {
		t.Fatalf("mine.List = %v, %v", list, err)
	}
}

// TestAgentOwnershipDropsAnAgentsArtifactsWhenTheAgentIsDeleted checks that the hub
// drops an agent's artifacts when the agent is deleted, and leaves others alone: a1's
// versions go too, a2 keeps its artifact and its history.
func TestAgentOwnershipDropsAnAgentsArtifactsWhenTheAgentIsDeleted(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	a1 := e.view(t, "a1", "")
	a2 := e.view(t, "a2", "")
	w1 := save(t, a1, e.fv, "a.md", "text/markdown", "a")
	save(t, a2, e.fv, "b.md", "text/markdown", "b")

	if err := e.hub.ReleaseAgentData(ctx, "a1"); err != nil {
		t.Fatal(err)
	}
	if list, err := a1.List(ctx, artifact.Query{}); err != nil || len(list) != 0 {
		t.Fatalf("a1.List = %v, %v; want empty", list, err)
	}
	if versions, err := e.store.ListVersions(ctx, w1.Artifact.ID); err != nil || len(versions) != 0 {
		t.Fatalf("a1 versions = %v, %v; want none", versions, err)
	}
	list, err := a2.List(ctx, artifact.Query{})
	if err != nil || len(names(list)) != 1 || names(list)[0] != "b.md" {
		t.Fatalf("a2.List = %v, %v; want [b.md]", names(list), err)
	}
	// Releasing an agent without data is not an error.
	if err := e.hub.ReleaseAgentData(ctx, "nobody"); err != nil {
		t.Fatalf("ReleaseAgentData(nobody): %v", err)
	}
}

// TestHubIdentityAndBlankAgent: the plugin identity, and a blank agent id is the
// invalid_argument error of ForAgent.
func TestHubIdentityAndBlankAgent(t *testing.T) {
	hub := artifacthub.NewArtifactHub(nil)
	if hub.Name() != "artifact-hub" || hub.Version() != "1.0.0" {
		t.Fatalf("identity = %q %q", hub.Name(), hub.Version())
	}
	var _ kernel.Plugin = hub
	for _, blank := range []string{"", "   ", "\t\n", string([]rune{0xA0, 0xFEFF})} {
		v, err := hub.ForAgent(blank, "s")
		if v != nil {
			t.Errorf("ForAgent(%q) returned a view", blank)
		}
		ae := expectCode(t, err, kernel.ArtifactCodeInvalidArgument)
		if ae.Message != "An agent id is required to reach artifacts" || ae.ArtifactID != "" {
			t.Errorf("ForAgent(%q) error = %+v", blank, ae)
		}
	}
	if v, err := hub.ForAgent(" a1 ", ""); err != nil || v.AgentID() != " a1 " {
		t.Fatalf("ForAgent(\" a1 \") = %v, %v; the id is kept as given", v, err)
	}
}

// TestStoreSwapReachesEarlierViews: the store is resolved on every call, so a later
// store swap (Kernel.UsePersistence) reaches a view handed out earlier.
func TestStoreSwapReachesEarlierViews(t *testing.T) {
	ctx := t.Context()
	first := persistence.NewMemoryArtifactStore()
	second := persistence.NewMemoryArtifactStore()
	current := first
	hub := artifacthub.NewArtifactHub(func() persistence.ArtifactStore { return current })
	v, err := hub.ForAgent("a1", "s1")
	if err != nil {
		t.Fatal(err)
	}
	fv := newView(t, "docker:shared", "a1")
	save(t, v, fv, "a.md", "text/markdown", "one")
	if list, _ := first.Query(ctx, persistence.ArtifactFilter{}); len(list) != 1 {
		t.Fatalf("first store holds %d artifacts, want 1", len(list))
	}

	current = second
	if list, err := v.List(ctx, artifact.Query{}); err != nil || len(list) != 0 {
		t.Fatalf("after the swap List = %v, %v; want empty", list, err)
	}
	save(t, v, fv, "a.md", "text/markdown", "two") // the same name is free in the new store
	if list, _ := second.Query(ctx, persistence.ArtifactFilter{}); len(list) != 1 {
		t.Fatalf("second store holds %d artifacts, want 1", len(list))
	}
}

// TestHubOptionsIgnoreNil: a nil clock or id function keeps the default.
func TestHubOptionsIgnoreNil(t *testing.T) {
	store := persistence.NewMemoryArtifactStore()
	hub := artifacthub.NewArtifactHub(func() persistence.ArtifactStore { return store }, artifacthub.WithClock(nil), artifacthub.WithIDs(nil), nil)
	v, err := hub.ForAgent("a1", "")
	if err != nil {
		t.Fatal(err)
	}
	w := save(t, v, newView(t, "docker:shared", "a1"), "a.md", "text/markdown", "x")
	if len(w.Artifact.ID) != 36 || w.Artifact.CreatedAt == "" || w.Artifact.CreatedAt != w.Artifact.UpdatedAt {
		t.Fatalf("artifact = %+v; want a UUID and one timestamp", w.Artifact)
	}
}

// TestConcurrentSaveSameName: 16 goroutines save one name through one hub; the
// per-name lock makes exactly one succeed and the other 15 get name_conflict (run
// under -race).
func TestConcurrentSaveSameName(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	const workers = 16
	results := make([]error, workers)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, err := e.hub.ForAgent("a1", "s1")
			if err != nil {
				results[i] = err
				return
			}
			body := fmt.Sprintf("content %d", i)
			<-start
			_, results[i] = v.Save(ctx, artifact.SaveInput{Name: "same.md", Type: "text/markdown", Content: &body}, e.fv)
		}()
	}
	close(start)
	wg.Wait()

	ok, conflicts := 0, 0
	for i, err := range results {
		switch {
		case err == nil:
			ok++
		case artifactError(t, err).Code == kernel.ArtifactCodeNameConflict:
			conflicts++
		default:
			t.Errorf("worker %d: unexpected error %v", i, err)
		}
	}
	if ok != 1 || conflicts != workers-1 {
		t.Fatalf("successes = %d, name_conflicts = %d; want 1 and %d", ok, conflicts, workers-1)
	}
	list, err := e.view(t, "a1", "").List(ctx, artifact.Query{})
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %v, %v; want exactly one artifact", names(list), err)
	}
	// The winner's content is what the single artifact tracks.
	r := mustRead(t, e.view(t, "a1", ""), artifact.Ref{Name: "same.md"}, e.fv)
	if r.File.Status != artifact.FileCurrent {
		t.Fatalf("file status = %s; the tracked hash must match the file", r.File.Status)
	}
}

// TestConcurrentUpdatesAreSerialised: concurrent updates through one hub never
// produce a repeated or skipped version number.
func TestConcurrentUpdatesAreSerialised(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	v := e.view(t, "a1", "s1")
	save(t, v, e.fv, "a.md", "text/markdown", "v1")
	const workers = 12
	var wg sync.WaitGroup
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			body := fmt.Sprintf("body %d", i)
			if _, err := v.Update(ctx, artifact.UpdateInput{Ref: artifact.Ref{Name: "a.md"}, Content: &body}, e.fv); err != nil {
				t.Errorf("Update %d: %v", i, err)
			}
		}()
	}
	wg.Wait()
	versions, err := v.Versions(ctx, artifact.Ref{Name: "a.md"})
	if err != nil || len(versions) != workers+1 {
		t.Fatalf("versions = %d, %v; want %d", len(versions), err, workers+1)
	}
	for i, ver := range versions {
		if want := workers + 1 - i; ver.Version != want {
			t.Fatalf("versions[%d].Version = %d, want %d", i, ver.Version, want)
		}
	}
}
