package artifacthub_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Phong95/aec-agent-go/aecmock"
	"github.com/Phong95/aec-agent-go/components/artifacthub"
	"github.com/Phong95/aec-agent-go/components/artifacthub/artifact"
	"github.com/Phong95/aec-agent-go/components/persistence"
	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/internal/wiretest"
	"github.com/Phong95/aec-agent-go/kernel"
)

// env is one hub over a fresh in-memory store and one shared sandbox file view,
// the shared test setup (a1 works in docker:shared).
type env struct {
	store *persistence.MemoryArtifactStore
	hub   *artifacthub.ArtifactHub
	fv    sandboxhub.View
}

// newEnv builds the hub with a deterministic clock (one second per read) and ids.
func newEnv(t *testing.T) env {
	t.Helper()
	store := persistence.NewMemoryArtifactStore()
	hub := artifacthub.NewArtifactHub(
		func() persistence.ArtifactStore { return store },
		artifacthub.WithClock(wiretest.Clock(1700000000000, 1000)),
		artifacthub.WithIDs(wiretest.IDs()),
	)
	return env{store: store, hub: hub, fv: newView(t, "docker:shared", "a1")}
}

// view returns the artifact view of an agent.
func (e env) view(t *testing.T, agent, session string) artifact.View {
	t.Helper()
	v, err := e.hub.ForAgent(agent, session)
	if err != nil {
		t.Fatalf("ForAgent(%q): %v", agent, err)
	}
	return v
}

// newView returns a view of a fresh shared in-memory sandbox with the given id,
// the real in-memory sandbox that replaced the Phase 6 FileView stop-gap.
func newView(t *testing.T, id, agentID string) sandboxhub.View {
	t.Helper()
	v, err := aecmock.NewInMemoryView(id, agentID)
	if err != nil {
		t.Fatalf("NewInMemoryView(%q, %q): %v", id, agentID, err)
	}
	return v
}

func str(s string) *string { return &s }

// save saves inline content and fails the test on any error.
func save(t *testing.T, v artifact.View, sb sandboxhub.View, name, typ, content string) artifact.Write {
	t.Helper()
	w, err := v.Save(t.Context(), artifact.SaveInput{Name: name, Type: typ, Content: &content}, sb)
	if err != nil {
		t.Fatalf("Save(%s): %v", name, err)
	}
	return w
}

// artifactError returns the *kernel.ArtifactError in err, failing when there is none.
func artifactError(t *testing.T, err error) *kernel.ArtifactError {
	t.Helper()
	var ae *kernel.ArtifactError
	if !errors.As(err, &ae) {
		t.Fatalf("error = %v, want a *kernel.ArtifactError", err)
	}
	return ae
}

// expectCode fails unless err is an ArtifactError with this code, and returns it.
func expectCode(t *testing.T, err error, code kernel.ArtifactErrorCode) *kernel.ArtifactError {
	t.Helper()
	ae := artifactError(t, err)
	if ae.Code != code {
		t.Fatalf("code = %s (%s), want %s", ae.Code, ae.Message, code)
	}
	return ae
}

// requireCode is expectCode for callers that do not need the error.
func requireCode(t *testing.T, err error, code kernel.ArtifactErrorCode) {
	t.Helper()
	if ae := artifactError(t, err); ae.Code != code {
		t.Fatalf("code = %s (%s), want %s", ae.Code, ae.Message, code)
	}
}

func mustRead(t *testing.T, v artifact.View, ref artifact.Ref, sb sandboxhub.View) artifact.Read {
	t.Helper()
	r, err := v.Read(t.Context(), ref, sb, artifact.ReadOptions{})
	if err != nil {
		t.Fatalf("Read(%+v): %v", ref, err)
	}
	return r
}

func names(list []artifact.Artifact) []string {
	out := []string{}
	for _, a := range list {
		out = append(out, a.Name)
	}
	return out
}

// --- the goldens --------------------------------------------------------------

// goldenError is one entry of artifacthub-errors.json.
type goldenError struct {
	Message    string `json:"message"`
	Code       string `json:"code"`
	ArtifactID string `json:"artifactId"`
}

type errorsFixture struct {
	Errors map[string]goldenError `json:"errors"`
}

// TestErrorsMatchGolden replays the sequence of the wiregen case artifacthub-errors
// and requires every error to have the code, message and artifactId of the recorded
// golden. The expectations are decoded from the fixture, never typed here.
func TestErrorsMatchGolden(t *testing.T) {
	ctx := t.Context()
	var fx errorsFixture
	unmarshalStrict(t, wiretest.Load(t, "artifacthub-errors.json"), &fx)

	e := newEnv(t)
	a1 := e.view(t, "a1", "")
	a2 := e.view(t, "a2", "")
	one := newView(t, "stub:one", "a1")
	two := newView(t, "stub:two", "a1")
	const md = "text/markdown"

	first := save(t, a1, one, "notes.md", md, "v1")
	id := first.Artifact.ID
	content := func(s string) *string { return &s }

	covered := map[string]bool{}
	expect := func(name string, err error) {
		t.Helper()
		covered[name] = true
		want, ok := fx.Errors[name]
		if !ok {
			t.Errorf("%s: no golden in artifacthub-errors.json", name)
			return
		}
		if err == nil {
			t.Errorf("%s: no error, want %s %q", name, want.Code, want.Message)
			return
		}
		ae := artifactError(t, err)
		if string(ae.Code) != want.Code || ae.Message != want.Message || ae.ArtifactID != want.ArtifactID {
			t.Errorf("%s:\n got {%s %q %q}\nwant {%s %q %q}", name, ae.Code, ae.Message, ae.ArtifactID, want.Code, want.Message, want.ArtifactID)
		}
		if ae.Retryable {
			t.Errorf("%s: an artifact error must not be retryable", name)
		}
	}
	saveErr := func(v artifact.View, sb sandboxhub.View, in artifact.SaveInput) error {
		_, err := v.Save(ctx, in, sb)
		return err
	}
	updateErr := func(v artifact.View, sb sandboxhub.View, in artifact.UpdateInput) error {
		_, err := v.Update(ctx, in, sb)
		return err
	}

	_, err := e.hub.ForAgent("   ", "")
	expect("blankAgentId", err)
	noStore := artifacthub.NewArtifactHub(func() persistence.ArtifactStore { return nil })
	nv, err := noStore.ForAgent("a1", "")
	if err != nil {
		t.Fatal(err)
	}
	_, err = nv.List(ctx, artifact.Query{})
	expect("noStoreConfigured", err)
	expect("neitherContentNorPath", saveErr(a1, one, artifact.SaveInput{Name: "spec.md", Type: md}))
	expect("nameBadPattern", saveErr(a1, one, artifact.SaveInput{Name: "bad*name", Type: md, Content: content("x")}))
	expect("nameBlank", saveErr(a1, one, artifact.SaveInput{Name: "   ", Type: md, Content: content("x")}))
	expect("nameTooLong", saveErr(a1, one, artifact.SaveInput{Name: strings.Repeat("x", 201), Type: md, Content: content("x")}))
	expect("nameDotDot", saveErr(a1, one, artifact.SaveInput{Name: "a..b", Type: md, Content: content("x")}))
	expect("nameDoubleSlash", saveErr(a1, one, artifact.SaveInput{Name: "a//b", Type: md, Content: content("x")}))
	expect("nameTrailingSlash", saveErr(a1, one, artifact.SaveInput{Name: "a/", Type: md, Content: content("x")}))
	expect("typeBad", saveErr(a1, one, artifact.SaveInput{Name: "ok.md", Type: "not a type!", Content: content("x")}))
	expect("nameConflict", saveErr(a1, one, artifact.SaveInput{Name: "notes.md", Type: md, Content: content("again")}))
	_, err = a1.Get(ctx, artifact.Ref{ID: "missing-id"})
	expect("getNotFoundById", err)
	_, err = a1.Get(ctx, artifact.Ref{Name: "ghost.md"})
	expect("getNotFoundByName", err)
	_, err = a2.Get(ctx, artifact.Ref{ID: id})
	expect("foreignGetById", err)
	_, err = a2.Read(ctx, artifact.Ref{ID: id}, one, artifact.ReadOptions{})
	expect("foreignReadById", err)
	expect("foreignUpdateById", updateErr(a2, one, artifact.UpdateInput{Ref: artifact.Ref{ID: id}, Content: content("hijacked")}))
	_, err = a2.Versions(ctx, artifact.Ref{ID: id})
	expect("foreignVersionsById", err)
	expect("foreignDeleteById", a2.Delete(ctx, artifact.Ref{ID: id}))
	_, err = a2.Get(ctx, artifact.Ref{Name: "notes.md"})
	expect("foreignGetByName", err)
	_, err = a1.Get(ctx, artifact.Ref{})
	expect("neitherIdNorName", err)
	nine := 9
	expect("staleExpectedVersion", updateErr(a1, one, artifact.UpdateInput{Ref: artifact.Ref{Name: "notes.md"}, Content: content("v2"), ExpectedVersion: &nine}))
	expect("saveFromMissingPath", saveErr(a1, one, artifact.SaveInput{Name: "tracked.md", Type: md, Path: "nope.md"}))
	expect("updateFromMissingPath", updateErr(a1, one, artifact.UpdateInput{Ref: artifact.Ref{Name: "notes.md"}, Path: "nope.md"}))
	expect("updateOtherSandboxNoContent", updateErr(a1, two, artifact.UpdateInput{Ref: artifact.Ref{Name: "notes.md"}}))

	for name := range fx.Errors {
		if !covered[name] {
			t.Errorf("golden %s is not exercised by the Go test", name)
		}
	}
}

// --- save_artifact, update_artifact, read_artifact and list_artifacts, at view level

// TestSaveWritesInlineContentAndTracks checks that Save writes inline content to the
// sandbox, tracks it and stamps the session: the file
// lands under artifacts/, the head and the version log carry the provenance, and
// the version log holds no content.
func TestSaveWritesInlineContentAndTracks(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	body := "# Clashes\n"
	w, err := e.view(t, "a1", "sess-7").Save(ctx, artifact.SaveInput{
		Name: "reports/clash.md", Type: "text/markdown", Content: &body,
		Description: "Level 3 clashes", Tags: []string{"clash", "clash"}, Note: "first pass", Tool: "save_artifact",
	}, e.fv)
	if err != nil {
		t.Fatal(err)
	}
	if w.Version.Version != 1 || w.Version.Bytes != 10 || w.Artifact.Bytes != 10 {
		t.Fatalf("write = %+v", w)
	}
	written, err := e.fv.ReadFile(ctx, "artifacts/reports/clash.md", sandboxhub.ReadOptions{})
	if err != nil || written.Content != body {
		t.Fatalf("file = %+v, %v", written, err)
	}

	r := mustRead(t, e.view(t, "a1", ""), artifact.Ref{Name: "reports/clash.md"}, e.fv)
	a := r.Artifact
	if a.Type != "text/markdown" || a.Description != "Level 3 clashes" || a.SessionID != "sess-7" || a.Version != 1 ||
		a.SandboxID != "docker:shared" || a.SandboxPath != "/workspace/agents/a1/artifacts/reports/clash.md" {
		t.Fatalf("artifact = %+v", a)
	}
	if len(a.Tags) != 1 || a.Tags[0] != "clash" { // duplicates collapsed
		t.Fatalf("tags = %v, want [clash]", a.Tags)
	}
	if r.File.Status != artifact.FileCurrent || r.File.Content != body {
		t.Fatalf("file = %+v", r.File)
	}

	versions, err := e.view(t, "a1", "").Versions(ctx, artifact.Ref{Name: "reports/clash.md"})
	if err != nil || len(versions) != 1 {
		t.Fatalf("versions = %+v, %v", versions, err)
	}
	wantOrigin := artifact.Origin{
		AgentID: "a1", SessionID: "sess-7", Tool: "save_artifact", SandboxID: "docker:shared",
		SandboxPath: "/workspace/agents/a1/artifacts/reports/clash.md",
	}
	if versions[0].Note != "first pass" || versions[0].Origin != wantOrigin {
		t.Fatalf("version = %+v", versions[0])
	}
}

// TestSaveTracksAnExistingFileFromPath: a path without content tracks the file
// where it is and creates nothing under artifacts/.
func TestSaveTracksAnExistingFileFromPath(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	if _, err := e.fv.WriteFile(ctx, "out/takeoff.csv", "item,qty\nbeam,12\n", sandboxhub.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	v := e.view(t, "a1", "s1")
	if _, err := v.Save(ctx, artifact.SaveInput{Name: "takeoff.csv", Type: "text/csv", Path: "out/takeoff.csv", Note: "tracked"}, e.fv); err != nil {
		t.Fatal(err)
	}
	r := mustRead(t, v, artifact.Ref{Name: "takeoff.csv"}, e.fv)
	if r.Artifact.SandboxPath != "/workspace/agents/a1/out/takeoff.csv" || r.File.Status != artifact.FileCurrent || r.File.Content != "item,qty\nbeam,12\n" {
		t.Fatalf("read = %+v", r)
	}
	if st, err := e.fv.Stat(ctx, "artifacts/takeoff.csv"); st != nil || err != nil {
		t.Fatalf("Stat artifacts/takeoff.csv = %v, %v; want nil, nil", st, err)
	}
}

// TestSaveRefusesNameAlreadyUsed: a taken name is a name_conflict naming the
// existing artifact, and the existing file stays untouched.
func TestSaveRefusesNameAlreadyUsed(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	v := e.view(t, "a1", "s1")
	first := save(t, v, e.fv, "a.md", "text/markdown", "one")
	two := "two"
	_, err := v.Save(ctx, artifact.SaveInput{Name: "a.md", Type: "text/markdown", Content: &two}, e.fv)
	ae := expectCode(t, err, kernel.ArtifactCodeNameConflict)
	if ae.ArtifactID != first.Artifact.ID || !strings.Contains(ae.Message, "update_artifact") {
		t.Fatalf("error = %+v", ae)
	}
	if f, err := e.fv.ReadFile(ctx, "artifacts/a.md", sandboxhub.ReadOptions{}); err != nil || f.Content != "one" {
		t.Fatalf("file = %+v, %v; want the first content", f, err)
	}
}

// TestSaveContentAndPathWritesAtThePath: with both given, the inline content is
// written at the path (the tool layer reports the dropped from_path in Phase 11).
func TestSaveContentAndPathWritesAtThePath(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	v := e.view(t, "a1", "s1")
	if _, err := e.fv.WriteFile(ctx, "out/draft.md", "from the file", sandboxhub.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	inline := "inline wins"
	if _, err := v.Save(ctx, artifact.SaveInput{Name: "a.md", Type: "text/markdown", Content: &inline, Path: "out/draft.md"}, e.fv); err != nil {
		t.Fatal(err)
	}
	r := mustRead(t, v, artifact.Ref{Name: "a.md"}, e.fv)
	if r.File.Content != "inline wins" || r.Artifact.SandboxPath != "/workspace/agents/a1/out/draft.md" {
		t.Fatalf("read = %+v", r)
	}

	// A blank path next to content falls back to artifacts/<name>.
	blank := "blank path"
	if _, err := v.Save(ctx, artifact.SaveInput{Name: "b.md", Type: "text/markdown", Content: &blank, Path: "   "}, e.fv); err != nil {
		t.Fatal(err)
	}
	if got := mustRead(t, v, artifact.Ref{Name: "b.md"}, e.fv).Artifact.SandboxPath; got != "/workspace/agents/a1/artifacts/b.md" {
		t.Fatalf("path = %q", got)
	}
}

// TestSaveEmptyArtifact: an intentionally empty inline content is a valid empty file.
func TestSaveEmptyArtifact(t *testing.T) {
	e := newEnv(t)
	v := e.view(t, "a1", "s1")
	save(t, v, e.fv, "empty.md", "text/markdown", "")
	r := mustRead(t, v, artifact.Ref{Name: "empty.md"}, e.fv)
	if r.File.Status != artifact.FileCurrent || r.File.Content != "" || r.File.Size != 0 {
		t.Fatalf("read = %+v", r.File)
	}
}

// TestUpdateRewritesTheTrackedFile checks that Update rewrites the tracked file, moves
// the head to v2 and keeps a content-less version log.
func TestUpdateRewritesTheTrackedFile(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	v := e.view(t, "a1", "s1")
	save(t, v, e.fv, "a.md", "text/markdown", "v1")
	v2 := "v2"
	w, err := v.Update(ctx, artifact.UpdateInput{Ref: artifact.Ref{Name: "a.md"}, Content: &v2, Note: "added totals"}, e.fv)
	if err != nil {
		t.Fatal(err)
	}
	if w.Artifact.Version != 2 || w.Version.Version != 2 || w.Version.Note != "added totals" {
		t.Fatalf("write = %+v", w)
	}
	if r := mustRead(t, v, artifact.Ref{Name: "a.md"}, e.fv); r.File.Content != "v2" || r.File.Status != artifact.FileCurrent {
		t.Fatalf("read = %+v", r)
	}
	versions, err := v.Versions(ctx, artifact.Ref{Name: "a.md"})
	if err != nil || len(versions) != 2 || versions[0].Version != 2 || versions[1].Version != 1 {
		t.Fatalf("versions = %+v, %v; want [2 1]", versions, err)
	}
}

// TestUpdateRetracksTheSamePathWithANewSha: only the name given, the file edited
// outside the tool: the head moves to v2 with the new hash and stays current.
func TestUpdateRetracksTheSamePathWithANewSha(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	v := e.view(t, "a1", "s1")
	first := save(t, v, e.fv, "a.md", "text/markdown", "v1")
	if _, err := e.fv.WriteFile(ctx, "artifacts/a.md", "edited outside the tool", sandboxhub.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	w, err := v.Update(ctx, artifact.UpdateInput{Ref: artifact.Ref{Name: "a.md"}}, e.fv)
	if err != nil {
		t.Fatal(err)
	}
	if w.Artifact.Version != 2 || w.Artifact.SHA256 == first.Artifact.SHA256 || w.Artifact.Bytes != len("edited outside the tool") {
		t.Fatalf("write = %+v", w)
	}
	r := mustRead(t, v, artifact.Ref{Name: "a.md"}, e.fv)
	if r.Artifact.SandboxPath != "/workspace/agents/a1/artifacts/a.md" || r.File.Status != artifact.FileCurrent || r.File.Content != "edited outside the tool" {
		t.Fatalf("read = %+v", r)
	}
}

// TestUpdateSwitchesTheTrackedPath: an explicit path re-tracks another file.
func TestUpdateSwitchesTheTrackedPath(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	v := e.view(t, "a1", "s1")
	save(t, v, e.fv, "a.md", "text/markdown", "v1")
	if _, err := e.fv.WriteFile(ctx, "out/new.md", "moved", sandboxhub.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := v.Update(ctx, artifact.UpdateInput{Ref: artifact.Ref{Name: "a.md"}, Path: "out/new.md"}, e.fv); err != nil {
		t.Fatal(err)
	}
	r := mustRead(t, v, artifact.Ref{Name: "a.md"}, e.fv)
	if r.Artifact.SandboxPath != "/workspace/agents/a1/out/new.md" || r.File.Content != "moved" {
		t.Fatalf("read = %+v", r)
	}
}

// TestUpdateHonoursExpectedVersion: a stale edit fails with conflict and does not
// rewrite the file.
func TestUpdateHonoursExpectedVersion(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	v := e.view(t, "a1", "s1")
	save(t, v, e.fv, "a.md", "text/markdown", "v1")
	v2, v3 := "v2", "v3"
	if _, err := v.Update(ctx, artifact.UpdateInput{Ref: artifact.Ref{Name: "a.md"}, Content: &v2}, e.fv); err != nil {
		t.Fatal(err)
	}
	stale := 1
	_, err := v.Update(ctx, artifact.UpdateInput{Ref: artifact.Ref{Name: "a.md"}, Content: &v3, ExpectedVersion: &stale}, e.fv)
	requireCode(t, err, kernel.ArtifactCodeConflict)
	if f, err := e.fv.ReadFile(ctx, "artifacts/a.md", sandboxhub.ReadOptions{}); err != nil || f.Content != "v2" {
		t.Fatalf("file = %+v, %v; want v2 untouched", f, err)
	}
	// The head version is accepted.
	current := 2
	if _, err := v.Update(ctx, artifact.UpdateInput{Ref: artifact.Ref{Name: "a.md"}, Content: &v3, ExpectedVersion: &current}, e.fv); err != nil {
		t.Fatalf("matching expectedVersion: %v", err)
	}
}

// TestUpdateReplacesDescriptionAndTagsOnlyWhenGiven: nil keeps, a value replaces,
// an explicit empty description clears it, and an empty tag list clears the tags.
func TestUpdateReplacesDescriptionAndTagsOnlyWhenGiven(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	v := e.view(t, "a1", "s1")
	body := "v1"
	if _, err := v.Save(ctx, artifact.SaveInput{Name: "a.md", Type: "text/markdown", Content: &body, Description: "first", Tags: []string{"keep"}}, e.fv); err != nil {
		t.Fatal(err)
	}
	ref := artifact.Ref{Name: "a.md"}
	update := func(in artifact.UpdateInput) artifact.Artifact {
		t.Helper()
		in.Ref = ref
		w, err := v.Update(ctx, in, e.fv)
		if err != nil {
			t.Fatal(err)
		}
		return w.Artifact
	}

	v2 := "v2"
	if a := update(artifact.UpdateInput{Content: &v2}); a.Description != "first" || len(a.Tags) != 1 || a.Tags[0] != "keep" {
		t.Fatalf("keep: %+v", a)
	}
	v3 := "v3"
	if a := update(artifact.UpdateInput{Content: &v3, Description: str("second"), Tags: []string{"new"}}); a.Description != "second" || len(a.Tags) != 1 || a.Tags[0] != "new" {
		t.Fatalf("replace: %+v", a)
	}
	if got := mustRead(t, v, ref, e.fv).Artifact; got.Description != "second" || got.Tags[0] != "new" {
		t.Fatalf("stored: %+v", got)
	}
	// An explicit empty description clears it; an empty (non-nil) tag list clears the tags.
	cleared := update(artifact.UpdateInput{Description: str(""), Tags: []string{}})
	if cleared.Description != "" || cleared.Tags != nil {
		t.Fatalf("clear: %+v", cleared)
	}
	if got := mustRead(t, v, ref, e.fv).Artifact; got.Description != "" || got.Tags != nil {
		t.Fatalf("stored after clear: %+v", got)
	}
	// A blank description (only spaces) clears as well, a blank tag list too.
	update(artifact.UpdateInput{Description: str("again"), Tags: []string{"x"}})
	blank := update(artifact.UpdateInput{Description: str("   "), Tags: []string{"  ", ""}})
	if blank.Description != "" || blank.Tags != nil {
		t.Fatalf("blank: %+v", blank)
	}
}

// TestUpdateInAnotherSandbox: with no content or path there is nothing to point at
// (file_missing, no write happened); with content a fresh artifacts/<name> is written
// in the caller's sandbox and the artifact moves there.
func TestUpdateInAnotherSandbox(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	v := e.view(t, "a1", "s1")
	save(t, v, e.fv, "a.md", "text/markdown", "v1")
	other := newView(t, "host:repo", "a1")

	_, err := v.Update(ctx, artifact.UpdateInput{Ref: artifact.Ref{Name: "a.md"}}, other)
	ae := expectCode(t, err, kernel.ArtifactCodeFileMissing)
	if !strings.Contains(ae.Message, "host:repo") || !strings.Contains(ae.Message, "docker:shared") {
		t.Fatalf("message = %q, want both sandbox ids", ae.Message)
	}
	if got, err := v.Get(ctx, artifact.Ref{Name: "a.md"}); err != nil || got.Version != 1 {
		t.Fatalf("Get = %+v, %v; want version 1 (no write happened)", got, err)
	}

	moved := "moved here"
	w, err := v.Update(ctx, artifact.UpdateInput{Ref: artifact.Ref{Name: "a.md"}, Content: &moved}, other)
	if err != nil {
		t.Fatal(err)
	}
	if w.Artifact.SandboxID != "host:repo" || w.Artifact.SandboxPath != "/workspace/agents/a1/artifacts/a.md" || w.Artifact.Version != 2 {
		t.Fatalf("write = %+v", w.Artifact)
	}
}

// TestUpdateNotFound: an artifact that does not exist is not_found.
func TestUpdateNotFound(t *testing.T) {
	e := newEnv(t)
	x := "x"
	_, err := e.view(t, "a1", "").Update(t.Context(), artifact.UpdateInput{Ref: artifact.Ref{Name: "ghost.md"}, Content: &x}, e.fv)
	requireCode(t, err, kernel.ArtifactCodeNotFound)
}

// TestReadStates: current, changed (not an error), and missing for a deleted file or a
// fresh sandbox with the same id.
func TestReadStates(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	v := e.view(t, "a1", "s1")
	save(t, v, e.fv, "a.md", "text/markdown", "v1")
	ref := artifact.Ref{Name: "a.md"}

	if r := mustRead(t, v, ref, e.fv); r.File.Status != artifact.FileCurrent || r.File.Content != "v1" {
		t.Fatalf("current: %+v", r.File)
	}
	if _, err := e.fv.WriteFile(ctx, "artifacts/a.md", "edited outside the tool", sandboxhub.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if r := mustRead(t, v, ref, e.fv); r.File.Status != artifact.FileChanged || r.File.Content != "edited outside the tool" {
		t.Fatalf("changed: %+v", r.File)
	}

	// The same sandbox id with an empty store: the file is no longer there.
	fresh := newView(t, "docker:shared", "a1")
	r := mustRead(t, v, ref, fresh)
	if r.File.Status != artifact.FileMissing || r.File.Reason != `"/workspace/agents/a1/artifacts/a.md" does not exist` || r.File.SandboxID != "docker:shared" {
		t.Fatalf("missing: %+v", r.File)
	}

	_, err := v.Read(ctx, artifact.Ref{Name: "ghost.md"}, e.fv, artifact.ReadOptions{})
	requireCode(t, err, kernel.ArtifactCodeNotFound)
}

// TestReadFromAnotherSandboxNamesBothIds: a different sandbox id gives a missing
// file whose reason names the tracked and the given sandbox.
func TestReadFromAnotherSandboxNamesBothIds(t *testing.T) {
	e := newEnv(t)
	v := e.view(t, "a1", "s1")
	save(t, v, e.fv, "a.md", "text/markdown", "v1")
	other := newView(t, "host:repo", "a1")
	r := mustRead(t, v, artifact.Ref{Name: "a.md"}, other)
	if r.File.Status != artifact.FileMissing || r.File.SandboxID != "docker:shared" ||
		!strings.Contains(r.File.Reason, "docker:shared") || !strings.Contains(r.File.Reason, "host:repo") {
		t.Fatalf("file = %+v", r.File)
	}
	if r.File.Path != r.Artifact.SandboxPath {
		t.Fatalf("path = %q, want the tracked path %q", r.File.Path, r.Artifact.SandboxPath)
	}
}

// TestReadBoundsLongContent: MaxBytes cuts the content returned, not the tracked file.
func TestReadBoundsLongContent(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	v := e.view(t, "a1", "s1")
	save(t, v, e.fv, "a.md", "text/markdown", strings.Repeat("z", 500))
	limit := 100
	r, err := v.Read(ctx, artifact.Ref{Name: "a.md"}, e.fv, artifact.ReadOptions{MaxBytes: &limit})
	if err != nil {
		t.Fatal(err)
	}
	if !r.File.Truncated || r.File.Size != 500 || len(r.File.Content) != 100 || r.File.Status != artifact.FileCurrent {
		t.Fatalf("file = %+v", r.File)
	}
	if whole := mustRead(t, v, artifact.Ref{Name: "a.md"}, e.fv); whole.File.Content != strings.Repeat("z", 500) || whole.File.Truncated {
		t.Fatalf("whole read = %+v", whole.File)
	}
}

// failingView wraps a sandbox view and fails the file operations it overrides.
type failingView struct {
	sandboxhub.View
	read  error
	write error
}

func (f failingView) ReadFile(ctx context.Context, p string, o sandboxhub.ReadOptions) (sandboxhub.FileContent, error) {
	if f.read != nil {
		return sandboxhub.FileContent{}, f.read
	}
	return f.View.ReadFile(ctx, p, o)
}

func (f failingView) WriteFile(ctx context.Context, p, c string, o sandboxhub.WriteOptions) (sandboxhub.WriteResult, error) {
	if f.write != nil {
		return sandboxhub.WriteResult{}, f.write
	}
	return f.View.WriteFile(ctx, p, c, o)
}

// TestSandboxErrorsOutsideTheMissingCodesPassThrough: a SandboxError whose code is
// not_found, invalid_path or path_escape becomes a missing file; any other error is
// returned unchanged, so errors.As finds the original value (T-06-27).
func TestSandboxErrorsOutsideTheMissingCodesPassThrough(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	v := e.view(t, "a1", "s1")
	save(t, v, e.fv, "a.md", "text/markdown", "v1")
	ref := artifact.Ref{Name: "a.md"}

	for _, code := range []kernel.SandboxErrorCode{kernel.SandboxCodeNotFound, kernel.SandboxCodeInvalidPath, kernel.SandboxCodePathEscape} {
		cause := kernel.NewSandboxError("gone: "+string(code), code, "docker:shared")
		r := mustRead(t, v, ref, failingView{View: e.fv, read: cause})
		if r.File.Status != artifact.FileMissing || r.File.Reason != cause.Message {
			t.Errorf("%s: file = %+v", code, r.File)
		}
	}

	original := kernel.NewSandboxError("container is down", kernel.SandboxCodeEnvironmentUnavailable, "docker:shared")
	_, err := v.Read(ctx, ref, failingView{View: e.fv, read: original}, artifact.ReadOptions{})
	var se *kernel.SandboxError
	if !errors.As(err, &se) || se != original {
		t.Fatalf("Read error = %v, want the original *SandboxError returned unchanged", err)
	}
	sentinel := errors.New("disk on fire")
	if _, err := v.Read(ctx, ref, failingView{View: e.fv, read: sentinel}, artifact.ReadOptions{}); !errors.Is(err, sentinel) {
		t.Fatalf("Read error = %v, want the sentinel", err)
	}

	// The same rule applies when tracking an existing file and when writing.
	_, err = v.Update(ctx, artifact.UpdateInput{Ref: ref}, failingView{View: e.fv, read: original})
	if !errors.As(err, &se) || se != original {
		t.Fatalf("Update (track) error = %v, want the original", err)
	}
	_, err = v.Save(ctx, artifact.SaveInput{Name: "t.md", Type: "text/markdown", Path: "x.md"}, failingView{View: e.fv, read: original})
	if !errors.As(err, &se) || se != original {
		t.Fatalf("Save (track) error = %v, want the original", err)
	}
	tooLarge := kernel.NewSandboxError("too big", kernel.SandboxCodeTooLarge, "docker:shared")
	body := "x"
	_, err = v.Save(ctx, artifact.SaveInput{Name: "w.md", Type: "text/markdown", Content: &body}, failingView{View: e.fv, write: tooLarge})
	if !errors.As(err, &se) || se != tooLarge {
		t.Fatalf("Save (write) error = %v, want the original", err)
	}
	if _, err := v.Get(ctx, artifact.Ref{Name: "w.md"}); err == nil {
		t.Fatal("a failed write must not leave an artifact behind")
	}
}

// TestListNewestFirstFiltersByTypeAndTag checks that List returns metadata newest
// first and filters by type and tag.
func TestListNewestFirstFiltersByTypeAndTag(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	v := e.view(t, "a1", "s1")
	a, b := "a", "b"
	if _, err := v.Save(ctx, artifact.SaveInput{Name: "a.md", Type: "text/markdown", Content: &a, Tags: []string{"report"}}, e.fv); err != nil {
		t.Fatal(err)
	}
	if _, err := v.Save(ctx, artifact.SaveInput{Name: "b.csv", Type: "text/csv", Content: &b, Tags: []string{"takeoff"}, Description: "Quantities"}, e.fv); err != nil {
		t.Fatal(err)
	}

	all, err := v.List(ctx, artifact.Query{})
	if err != nil || len(all) != 2 || all[0].Name != "b.csv" || all[1].Name != "a.md" {
		t.Fatalf("all = %v, %v; want [b.csv a.md]", names(all), err)
	}
	if all[0].Description != "Quantities" || all[0].SandboxPath != "/workspace/agents/a1/artifacts/b.csv" {
		t.Fatalf("b.csv = %+v", all[0])
	}
	byType, err := v.List(ctx, artifact.Query{Types: []string{"text/csv"}})
	if err != nil || len(byType) != 1 || byType[0].Name != "b.csv" {
		t.Fatalf("by type = %v, %v", names(byType), err)
	}
	byTag, err := v.List(ctx, artifact.Query{Tags: []string{"report"}})
	if err != nil || len(byTag) != 1 || byTag[0].Name != "a.md" {
		t.Fatalf("by tag = %v, %v", names(byTag), err)
	}
	byName, err := v.List(ctx, artifact.Query{Name: "a.md"})
	if err != nil || len(byName) != 1 {
		t.Fatalf("by name = %v, %v", names(byName), err)
	}
	two := 1
	limited, err := v.List(ctx, artifact.Query{Limit: &two})
	if err != nil || len(limited) != 1 || limited[0].Name != "b.csv" {
		t.Fatalf("limit 1 = %v, %v", names(limited), err)
	}
}

// TestListDropsFalsyFilterFields: empty strings, empty lists and a limit of 0 mean
// "no filter".
func TestListDropsFalsyFilterFields(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	v := e.view(t, "a1", "s1")
	save(t, v, e.fv, "a.md", "text/markdown", "a")
	save(t, v, e.fv, "b.md", "text/markdown", "b")
	zero := 0
	got, err := v.List(ctx, artifact.Query{SessionID: "", Types: []string{}, Name: "", Tags: []string{}, Since: "", Before: "", Limit: &zero})
	if err != nil || len(got) != 2 {
		t.Fatalf("list = %v, %v; want both artifacts", names(got), err)
	}
}

// TestListOnlyCountsThisSessionsArtifactsWhenAsked: SessionID filters by the session
// the artifact was first created in.
func TestListOnlyCountsThisSessionsArtifactsWhenAsked(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	save(t, e.view(t, "a1", "s1"), e.fv, "old.md", "text/markdown", "old")
	s2 := e.view(t, "a1", "s2")
	save(t, s2, e.fv, "new.md", "text/markdown", "new")

	scoped, err := s2.List(ctx, artifact.Query{SessionID: "s2"})
	if err != nil || len(scoped) != 1 || scoped[0].Name != "new.md" {
		t.Fatalf("session s2 = %v, %v", names(scoped), err)
	}
	all, err := s2.List(ctx, artifact.Query{})
	if err != nil || len(all) != 2 {
		t.Fatalf("no filter = %v, %v", names(all), err)
	}
}

// TestListNothingYet: an agent without artifacts gets an empty, non-nil list.
func TestListNothingYet(t *testing.T) {
	e := newEnv(t)
	got, err := e.view(t, "a1", "").List(t.Context(), artifact.Query{})
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("list = %#v, %v; want an empty non-nil slice", got, err)
	}
}

// TestDescriptionTagsAndNoteLimits: trimming, truncation (at a UTF-16 boundary),
// the 20 tag cap before deduplication, and the optional fields.
func TestDescriptionTagsAndNoteLimits(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	v := e.view(t, "a1", "s1")
	body := "x"

	// Description: trimmed and cut to 1000 UTF-16 units. A surrogate pair that
	// straddles the limit is dropped whole (JS would leave a lone surrogate).
	emoji := "\U0001F600"
	w, err := v.Save(ctx, artifact.SaveInput{
		Name: "d1.md", Type: "text/markdown", Content: &body,
		Description: "  " + strings.Repeat("d", 999) + emoji + "tail  ",
	}, e.fv)
	if err != nil {
		t.Fatal(err)
	}
	if w.Artifact.Description != strings.Repeat("d", 999) {
		t.Fatalf("description length %d, want 999 (the split pair dropped)", len(w.Artifact.Description))
	}
	w, err = v.Save(ctx, artifact.SaveInput{Name: "d2.md", Type: "text/markdown", Content: &body, Description: strings.Repeat("d", 1001)}, e.fv)
	if err != nil || len(w.Artifact.Description) != 1000 {
		t.Fatalf("description length %d, %v; want 1000", len(w.Artifact.Description), err)
	}
	w, err = v.Save(ctx, artifact.SaveInput{Name: "d3.md", Type: "text/markdown", Content: &body, Description: "   "}, e.fv)
	if err != nil || w.Artifact.Description != "" {
		t.Fatalf("blank description = %q, %v; want none", w.Artifact.Description, err)
	}

	// Tags: trimmed, cut to 50, empties dropped, first 20 kept, THEN deduplicated.
	var many []string
	for i := range 25 {
		many = append(many, "t"+strings.Repeat("0", i%2)+string(rune('a'+i)))
	}
	w, err = v.Save(ctx, artifact.SaveInput{Name: "t1.md", Type: "text/markdown", Content: &body, Tags: many}, e.fv)
	if err != nil || len(w.Artifact.Tags) != 20 || w.Artifact.Tags[0] != "ta" {
		t.Fatalf("tags = %v, %v; want the first 20", w.Artifact.Tags, err)
	}
	dups := append(strings.Split(strings.Repeat("a,", 20), ",")[:20], "b", "c")
	w, err = v.Save(ctx, artifact.SaveInput{Name: "t2.md", Type: "text/markdown", Content: &body, Tags: dups}, e.fv)
	if err != nil || len(w.Artifact.Tags) != 1 || w.Artifact.Tags[0] != "a" {
		t.Fatalf("tags = %v, %v; want [a] (cut to 20 first, then deduplicated)", w.Artifact.Tags, err)
	}
	w, err = v.Save(ctx, artifact.SaveInput{
		Name: "t3.md", Type: "text/markdown", Content: &body,
		Tags: []string{"  spaced  ", "", "   ", strings.Repeat("g", 60), "spaced"},
	}, e.fv)
	if err != nil || len(w.Artifact.Tags) != 2 || w.Artifact.Tags[0] != "spaced" || w.Artifact.Tags[1] != strings.Repeat("g", 50) {
		t.Fatalf("tags = %v, %v", w.Artifact.Tags, err)
	}
	w, err = v.Save(ctx, artifact.SaveInput{Name: "t4.md", Type: "text/markdown", Content: &body, Tags: []string{"", "  "}}, e.fv)
	if err != nil || w.Artifact.Tags != nil {
		t.Fatalf("tags = %v, %v; want none", w.Artifact.Tags, err)
	}

	// Note: trimmed, cut to 500, kept only when not empty. Tool: kept only when not empty.
	w, err = v.Save(ctx, artifact.SaveInput{Name: "n1.md", Type: "text/markdown", Content: &body, Note: "  " + strings.Repeat("n", 600) + "  ", Tool: "save_artifact"}, e.fv)
	if err != nil || len(w.Version.Note) != 500 || w.Version.Origin.Tool != "save_artifact" {
		t.Fatalf("version = %+v, %v", w.Version, err)
	}
	w, err = v.Save(ctx, artifact.SaveInput{Name: "n2.md", Type: "text/markdown", Content: &body, Note: "   "}, e.fv)
	if err != nil || w.Version.Note != "" || w.Version.Origin.Tool != "" {
		t.Fatalf("version = %+v, %v; want no note and no tool", w.Version, err)
	}
}

// TestNameAndTypeValidation: the patterns accept the valid names and types and the
// extra rules reject the invalid ones.
func TestNameAndTypeValidation(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	v := e.view(t, "a1", "")
	body := "x"
	saveOK := func(name, typ string) {
		t.Helper()
		if _, err := v.Save(ctx, artifact.SaveInput{Name: name, Type: typ, Content: &body}, e.fv); err != nil {
			t.Errorf("Save(%q, %q): %v", name, typ, err)
		}
	}
	saveBad := func(name, typ string) {
		t.Helper()
		_, err := v.Save(ctx, artifact.SaveInput{Name: name, Type: typ, Content: &body}, e.fv)
		requireCode(t, err, kernel.ArtifactCodeInvalidArgument)
	}
	saveOK("reports/q3.md", "text/markdown")
	saveOK("a b_c-d.e", "aec/takeoff")
	saveOK("  padded.md  ", "  text/plain  ")
	saveOK(strings.Repeat("x", 200), "application/vnd.api+json")
	saveOK("v9", "x")
	if got, err := v.Get(ctx, artifact.Ref{Name: "padded.md"}); err != nil || got.Type != "text/plain" {
		t.Errorf("trimmed name and type: %+v, %v", got, err)
	}
	saveBad(strings.Repeat("x", 201), "text/plain")
	saveBad(".hidden", "text/plain")
	saveBad("/abs", "text/plain")
	saveBad("a..b", "text/plain")
	saveBad("a//b", "text/plain")
	saveBad("dir/", "text/plain")
	saveBad("caf\u00e9", "text/plain")
	saveBad("", "text/plain")
	saveBad("ok2.md", "")
	saveBad("ok3.md", "-bad")
	saveBad("ok4.md", strings.Repeat("t", 101))
	saveBad("ok5.md", "has space")
	// A bad name or type fails before anything is written.
	if st, err := e.fv.Stat(ctx, "artifacts/ok2.md"); st != nil || err != nil {
		t.Fatalf("a rejected save wrote a file: %v, %v", st, err)
	}
}

// TestDeleteRemovesTheArtifactAndItsHistory: delete resolves first, drops the head
// and the versions, and leaves the sandbox file.
func TestDeleteRemovesTheArtifactAndItsHistory(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	v := e.view(t, "a1", "s1")
	w := save(t, v, e.fv, "a.md", "text/markdown", "v1")
	if err := v.Delete(ctx, artifact.Ref{Name: "a.md"}); err != nil {
		t.Fatal(err)
	}
	_, err := v.Get(ctx, artifact.Ref{ID: w.Artifact.ID})
	requireCode(t, err, kernel.ArtifactCodeNotFound)
	if versions, err := e.store.ListVersions(ctx, w.Artifact.ID); err != nil || len(versions) != 0 {
		t.Fatalf("versions = %v, %v; want none", versions, err)
	}
	if f, err := e.fv.ReadFile(ctx, "artifacts/a.md", sandboxhub.ReadOptions{}); err != nil || f.Content != "v1" {
		t.Fatalf("the sandbox file must stay: %+v, %v", f, err)
	}
	requireCode(t, v.Delete(ctx, artifact.Ref{Name: "a.md"}), kernel.ArtifactCodeNotFound)
}

// TestViewIdentityAndTrimmedReferences: AgentID and SessionID are reported, and an
// id or a name is trimmed before it is looked up.
func TestViewIdentityAndTrimmedReferences(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	v := e.view(t, "a1", "s9")
	if v.AgentID() != "a1" || v.SessionID() != "s9" {
		t.Fatalf("identity = %q %q", v.AgentID(), v.SessionID())
	}
	w := save(t, v, e.fv, "a.md", "text/markdown", "v1")
	if got, err := v.Get(ctx, artifact.Ref{Name: "  a.md  "}); err != nil || got.ID != w.Artifact.ID {
		t.Fatalf("Get by padded name = %+v, %v", got, err)
	}
	if got, err := v.Get(ctx, artifact.Ref{ID: "  " + w.Artifact.ID + "  "}); err != nil || got.ID != w.Artifact.ID {
		t.Fatalf("Get by padded id = %+v, %v", got, err)
	}
	// An id wins over a name.
	if _, err := v.Get(ctx, artifact.Ref{ID: "missing", Name: "a.md"}); err == nil {
		t.Fatal("an unknown id must not fall back to the name")
	}
	// A blank id and name is the invalid_argument case, not a lookup.
	_, err := v.Get(ctx, artifact.Ref{ID: "  ", Name: "\t"})
	requireCode(t, err, kernel.ArtifactCodeInvalidArgument)
}

// TestReturnedArtifactsAreCopies: mutating what the view returned never reaches the store.
func TestReturnedArtifactsAreCopies(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	v := e.view(t, "a1", "s1")
	body := "x"
	w, err := v.Save(ctx, artifact.SaveInput{Name: "a.md", Type: "text/markdown", Content: &body, Tags: []string{"one"}}, e.fv)
	if err != nil {
		t.Fatal(err)
	}
	w.Artifact.Tags[0] = "mutated"
	got, err := v.Get(ctx, artifact.Ref{Name: "a.md"})
	if err != nil || got.Tags[0] != "one" {
		t.Fatalf("stored tags = %v, %v; want [one]", got.Tags, err)
	}
	got.Tags[0] = "mutated"
	again, err := v.Get(ctx, artifact.Ref{Name: "a.md"})
	if err != nil || again.Tags[0] != "one" {
		t.Fatalf("stored tags = %v, %v; want [one]", again.Tags, err)
	}
}
