package artifacthub_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Phong95/aec-agent-go/components/artifacthub"
	"github.com/Phong95/aec-agent-go/components/artifacthub/artifact"
	"github.com/Phong95/aec-agent-go/components/persistence"
	"github.com/Phong95/aec-agent-go/components/sandboxhub"
)

// gatedView is a sandbox view whose WriteFile stops until the test releases it,
// standing in for a slow docker exec or a remote runner.
type gatedView struct {
	sandboxhub.View
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func newGatedView(t *testing.T, inner sandboxhub.View) *gatedView {
	t.Helper()
	g := &gatedView{View: inner, entered: make(chan struct{}, 16), release: make(chan struct{})}
	t.Cleanup(g.open) // never leave a goroutine parked when a test fails early
	return g
}

// open lets every parked and future write through.
func (g *gatedView) open() { g.once.Do(func() { close(g.release) }) }

func (g *gatedView) WriteFile(ctx context.Context, p, content string, opts sandboxhub.WriteOptions) (sandboxhub.WriteResult, error) {
	g.entered <- struct{}{}
	<-g.release
	return g.View.WriteFile(ctx, p, content, opts)
}

// nilView implements sandboxhub.View through an embedded (nil) interface, so a nil
// *nilView is a typed nil that any method call would panic on.
type nilView struct{ sandboxhub.View }

// settle is how long a test waits for an operation that the fix is meant to hold
// back. Before the fix the operation finishes at once; after it, it is still
// waiting when settle ends.
const settle = 150 * time.Millisecond

// within fails the test unless ch delivers inside d.
func within[T any](t *testing.T, ch <-chan T, d time.Duration, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(d):
		t.Fatalf("%s did not finish within %s", what, d)
		panic("unreachable")
	}
}

// TestDeleteDoesNotResurrectAnInFlightUpdate: a Delete that arrives while an
// Update is writing the sandbox file must not be undone by the Update's
// AddVersion (the memory store upserts the head record).
func TestDeleteDoesNotResurrectAnInFlightUpdate(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	v := e.view(t, "a1", "s1")
	save(t, v, e.fv, "a.md", "text/markdown", "v1")
	g := newGatedView(t, e.fv)

	body := "v2"
	updateDone := make(chan error, 1)
	go func() {
		_, err := v.Update(ctx, artifact.UpdateInput{Ref: artifact.Ref{Name: "a.md"}, Content: &body}, g)
		updateDone <- err
	}()
	<-g.entered

	deleteDone := make(chan error, 1)
	go func() { deleteDone <- v.Delete(ctx, artifact.Ref{Name: "a.md"}) }()
	var deleteErr error
	deleted := false
	select {
	case deleteErr, deleted = <-deleteDone: // the Delete overtook the Update
	case <-time.After(settle):
	}

	g.open()
	if err := within(t, updateDone, 5*time.Second, "Update"); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if !deleted {
		deleteErr = within(t, deleteDone, 5*time.Second, "Delete")
	}
	if deleteErr != nil {
		t.Fatalf("Delete: %v", deleteErr)
	}

	if got, err := v.Get(ctx, artifact.Ref{Name: "a.md"}); err == nil {
		t.Fatalf("the deleted artifact was resurrected by the update: %+v", got)
	}
	if versions, err := e.store.ListVersions(ctx, "00000000-0000-4000-8000-000000000001"); err != nil || len(versions) != 0 {
		t.Fatalf("versions of the deleted artifact = %d, %v; want none", len(versions), err)
	}
}

// TestReleaseAgentDataWaitsForAnInFlightSave: removing an agent's artifacts must
// not run between a Save's name check and its store.Create, or the artifact of a
// deleted agent is written after the cleanup.
func TestReleaseAgentDataWaitsForAnInFlightSave(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	v := e.view(t, "a1", "s1")
	g := newGatedView(t, e.fv)

	body := "x"
	saveDone := make(chan error, 1)
	go func() {
		_, err := v.Save(ctx, artifact.SaveInput{Name: "a.md", Type: "text/markdown", Content: &body}, g)
		saveDone <- err
	}()
	<-g.entered

	releaseDone := make(chan error, 1)
	go func() { releaseDone <- e.hub.ReleaseAgentData(ctx, "a1") }()
	var releaseErr error
	released := false
	select {
	case releaseErr, released = <-releaseDone: // the cleanup overtook the Save
	case <-time.After(settle):
	}

	g.open()
	if err := within(t, saveDone, 5*time.Second, "Save"); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if !released {
		releaseErr = within(t, releaseDone, 5*time.Second, "ReleaseAgentData")
	}
	if releaseErr != nil {
		t.Fatalf("ReleaseAgentData: %v", releaseErr)
	}

	if list, err := v.List(ctx, artifact.Query{}); err != nil || len(list) != 0 {
		t.Fatalf("after ReleaseAgentData the agent still has %v (%v)", names(list), err)
	}
}

// TestSlowSandboxWriteBlocksOnlyItsOwnName: one agent's stuck sandbox write must
// not hold up another agent, nor another artifact of the same agent.
func TestSlowSandboxWriteBlocksOnlyItsOwnName(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	slow := e.view(t, "a1", "s1")
	other := e.view(t, "a2", "s1")
	g := newGatedView(t, e.fv)

	body := "slow"
	slowDone := make(chan error, 1)
	go func() {
		_, err := slow.Save(ctx, artifact.SaveInput{Name: "slow.md", Type: "text/markdown", Content: &body}, g)
		slowDone <- err
	}()
	<-g.entered

	run := func(what string, v artifact.View, name string) {
		done := make(chan error, 1)
		go func() {
			text := "fast"
			_, err := v.Save(ctx, artifact.SaveInput{Name: name, Type: "text/markdown", Content: &text}, e.fv)
			done <- err
		}()
		if err := within(t, done, 3*time.Second, what); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	run("another agent's Save", other, "fast.md")
	run("another name of the same agent", slow, "also-fast.md")

	g.open()
	if err := within(t, slowDone, 5*time.Second, "slow Save"); err != nil {
		t.Fatalf("slow Save: %v", err)
	}
}

// TestWaitForABusyNameHonoursTheContext: a Save queued behind a stuck write of
// the same name gives up when its context ends, instead of waiting for a mutex.
func TestWaitForABusyNameHonoursTheContext(t *testing.T) {
	e := newEnv(t)
	v := e.view(t, "a1", "s1")
	g := newGatedView(t, e.fv)

	body := "first"
	firstDone := make(chan error, 1)
	go func() {
		_, err := v.Save(t.Context(), artifact.SaveInput{Name: "same.md", Type: "text/markdown", Content: &body}, g)
		firstDone <- err
	}()
	<-g.entered

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	second := "second"
	secondDone := make(chan error, 1)
	go func() {
		_, err := v.Save(ctx, artifact.SaveInput{Name: "same.md", Type: "text/markdown", Content: &second}, e.fv)
		secondDone <- err
	}()
	if err := within(t, secondDone, 3*time.Second, "the queued Save"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("queued Save error = %v, want the context deadline", err)
	}

	g.open()
	if err := within(t, firstDone, 5*time.Second, "first Save"); err != nil {
		t.Fatalf("first Save: %v", err)
	}
}

// TestParallelSavesOfDifferentAgents runs saves of many agents at once under
// -race. The injected clock and id source of newEnv are not goroutine safe, so
// the hub itself must call them one at a time.
func TestParallelSavesOfDifferentAgents(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	const agents, perAgent = 8, 5
	var wg sync.WaitGroup
	for a := range agents {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, err := e.hub.ForAgent(fmt.Sprintf("agent-%d", a), "")
			if err != nil {
				t.Error(err)
				return
			}
			for n := range perAgent {
				body := fmt.Sprintf("%d/%d", a, n)
				if _, err := v.Save(ctx, artifact.SaveInput{Name: fmt.Sprintf("f%d.md", n), Type: "text/markdown", Content: &body}, e.fv); err != nil {
					t.Errorf("agent %d save %d: %v", a, n, err)
				}
			}
		}()
	}
	wg.Wait()
	all, err := e.store.Query(ctx, persistence.ArtifactFilter{})
	if err != nil || len(all) != agents*perAgent {
		t.Fatalf("stored %d artifacts, %v; want %d", len(all), err, agents*perAgent)
	}
}

// TestTypedNilSandboxViewIsRejected: a nil *nilView (a test-local pointer type
// that implements sandboxhub.View) in a sandboxhub.View is as
// absent as a nil interface (the store check treats it that way), so every call
// that needs a sandbox fails with an error instead of panicking.
func TestTypedNilSandboxViewIsRejected(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	v := e.view(t, "a1", "s1")
	w := save(t, v, e.fv, "a.md", "text/markdown", "x")
	var typedNil *nilView
	body := "y"

	if _, err := v.Save(ctx, artifact.SaveInput{Name: "b.md", Type: "text/markdown", Content: &body}, typedNil); err == nil {
		t.Error("Save with a typed-nil sandbox succeeded")
	}
	if _, err := v.Update(ctx, artifact.UpdateInput{Ref: artifact.Ref{ID: w.Artifact.ID}, Content: &body}, typedNil); err == nil {
		t.Error("Update with a typed-nil sandbox succeeded")
	}
	if _, err := v.Read(ctx, artifact.Ref{ID: w.Artifact.ID}, typedNil, artifact.ReadOptions{}); err == nil {
		t.Error("Read with a typed-nil sandbox succeeded")
	}
	if list, _ := v.List(ctx, artifact.Query{}); len(list) != 1 {
		t.Errorf("a rejected Save changed the store: %v", names(list))
	}
}

// TestPendingReleaseDoesNotParkAnotherNameUncancellably: an Update of A is stuck
// in the sandbox, ReleaseAgentData of the agent is pending behind it, and a Save of
// another name B arrives. B queues behind the pending release (it must not starve
// it), but its wait honours its context: it fails with the deadline instead of
// hanging with the write, and once the release gives up, B goes through.
func TestPendingReleaseDoesNotParkAnotherNameUncancellably(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	v := e.view(t, "a1", "s1")
	save(t, v, e.fv, "a.md", "text/markdown", "v1")
	g := newGatedView(t, e.fv)

	body := "v2"
	updateDone := make(chan error, 1)
	go func() {
		_, err := v.Update(ctx, artifact.UpdateInput{Ref: artifact.Ref{Name: "a.md"}, Content: &body}, g)
		updateDone <- err
	}()
	<-g.entered

	releaseCtx, cancelRelease := context.WithCancel(ctx)
	defer cancelRelease()
	releaseDone := make(chan error, 1)
	go func() { releaseDone <- e.hub.ReleaseAgentData(releaseCtx, "a1") }()
	time.Sleep(settle) // let the release reach the gate and wait behind the Update

	saveB := func(ctx context.Context) error {
		text := "b"
		_, err := v.Save(ctx, artifact.SaveInput{Name: "b.md", Type: "text/markdown", Content: &text}, e.fv)
		return err
	}
	shortCtx, cancelShort := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancelShort()
	queued := make(chan error, 1)
	go func() { queued <- saveB(shortCtx) }()
	if err := within(t, queued, 3*time.Second, "the Save of B behind the pending release"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Save(B) error = %v, want the context deadline", err)
	}

	cancelRelease()
	if err := within(t, releaseDone, 3*time.Second, "the cancelled ReleaseAgentData"); !errors.Is(err, context.Canceled) {
		t.Fatalf("ReleaseAgentData error = %v, want context.Canceled", err)
	}
	after := make(chan error, 1)
	go func() { after <- saveB(ctx) }()
	if err := within(t, after, 3*time.Second, "the Save of B once the release gave up"); err != nil {
		t.Fatalf("Save(B) after the release gave up: %v", err)
	}

	g.open()
	if err := within(t, updateDone, 5*time.Second, "Update"); err != nil {
		t.Fatalf("Update: %v", err)
	}
}

// TestReleaseAgentDataWaitHonoursTheContext: a ReleaseAgentData that waits for a
// stuck write gives up with the context error and removes nothing.
func TestReleaseAgentDataWaitHonoursTheContext(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	v := e.view(t, "a1", "s1")
	save(t, v, e.fv, "keep.md", "text/markdown", "keep")
	g := newGatedView(t, e.fv)

	body := "x"
	saveDone := make(chan error, 1)
	go func() {
		_, err := v.Save(ctx, artifact.SaveInput{Name: "slow.md", Type: "text/markdown", Content: &body}, g)
		saveDone <- err
	}()
	<-g.entered

	shortCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	releaseDone := make(chan error, 1)
	go func() { releaseDone <- e.hub.ReleaseAgentData(shortCtx, "a1") }()
	if err := within(t, releaseDone, 3*time.Second, "ReleaseAgentData behind a stuck write"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ReleaseAgentData error = %v, want the context deadline", err)
	}

	g.open()
	if err := within(t, saveDone, 5*time.Second, "Save"); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if list, err := v.List(ctx, artifact.Query{}); err != nil || len(list) != 2 {
		t.Fatalf("a cancelled release removed data: %v (%v)", names(list), err)
	}
}

// swapEnv is a hub whose store can be replaced while operations are queued.
type swapEnv struct {
	cur  atomic.Pointer[persistence.MemoryArtifactStore]
	hub  *artifacthub.ArtifactHub
	fv   sandboxhub.View
	view artifact.View
}

func newSwapEnv(t *testing.T) (*swapEnv, *persistence.MemoryArtifactStore) {
	t.Helper()
	first := persistence.NewMemoryArtifactStore()
	s := &swapEnv{fv: newView(t, "docker:shared", "a1")}
	s.cur.Store(first)
	s.hub = artifacthub.NewArtifactHub(func() persistence.ArtifactStore { return s.cur.Load() })
	v, err := s.hub.ForAgent("a1", "s1")
	if err != nil {
		t.Fatal(err)
	}
	s.view = v
	return s, first
}

// TestSaveQueuedAcrossAStoreSwapUsesTheCurrentStore: a Save that waits for the
// lock of its name while the store is swapped runs against the new store as a
// whole, so it does not see the name of the old store as taken.
func TestSaveQueuedAcrossAStoreSwapUsesTheCurrentStore(t *testing.T) {
	ctx := t.Context()
	s, first := newSwapEnv(t)
	second := persistence.NewMemoryArtifactStore()
	g := newGatedView(t, s.fv)

	body := "first"
	firstDone := make(chan error, 1)
	go func() {
		_, err := s.view.Save(ctx, artifact.SaveInput{Name: "same.md", Type: "text/markdown", Content: &body}, g)
		firstDone <- err
	}()
	<-g.entered

	text := "second"
	queuedDone := make(chan error, 1)
	go func() {
		_, err := s.view.Save(ctx, artifact.SaveInput{Name: "same.md", Type: "text/markdown", Content: &text}, s.fv)
		queuedDone <- err
	}()
	time.Sleep(settle) // let the second Save queue for the lock of the name
	s.cur.Store(second)

	g.open()
	if err := within(t, firstDone, 5*time.Second, "first Save"); err != nil {
		t.Fatalf("first Save: %v", err)
	}
	if err := within(t, queuedDone, 5*time.Second, "queued Save"); err != nil {
		t.Fatalf("queued Save: %v (it must run against the store that is current once it has the lock)", err)
	}
	if got, _ := first.Query(ctx, persistence.ArtifactFilter{}); len(got) != 1 {
		t.Fatalf("old store holds %d artifacts, want only the first Save", len(got))
	}
	if got, _ := second.Query(ctx, persistence.ArtifactFilter{}); len(got) != 1 {
		t.Fatalf("new store holds %d artifacts, want the queued Save", len(got))
	}
}

// TestReleaseAgentDataQueuedAcrossAStoreSwapCleansTheCurrentStore: the cleanup that
// waited for a write cleans the store that is current when it runs.
func TestReleaseAgentDataQueuedAcrossAStoreSwapCleansTheCurrentStore(t *testing.T) {
	ctx := t.Context()
	s, first := newSwapEnv(t)
	second := persistence.NewMemoryArtifactStore()
	seed, err := artifacthub.NewArtifactHub(func() persistence.ArtifactStore { return second }).ForAgent("a1", "s1")
	if err != nil {
		t.Fatal(err)
	}
	save(t, seed, s.fv, "seed.md", "text/markdown", "seed")
	g := newGatedView(t, s.fv)

	body := "x"
	saveDone := make(chan error, 1)
	go func() {
		_, err := s.view.Save(ctx, artifact.SaveInput{Name: "old.md", Type: "text/markdown", Content: &body}, g)
		saveDone <- err
	}()
	<-g.entered

	releaseDone := make(chan error, 1)
	go func() { releaseDone <- s.hub.ReleaseAgentData(ctx, "a1") }()
	time.Sleep(settle) // let the release queue behind the Save
	s.cur.Store(second)

	g.open()
	if err := within(t, saveDone, 5*time.Second, "Save"); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := within(t, releaseDone, 5*time.Second, "ReleaseAgentData"); err != nil {
		t.Fatalf("ReleaseAgentData: %v", err)
	}
	if got, _ := second.Query(ctx, persistence.ArtifactFilter{AgentID: "a1"}); len(got) != 0 {
		t.Fatalf("the current store still holds %d artifacts of the released agent", len(got))
	}
	if got, _ := first.Query(ctx, persistence.ArtifactFilter{AgentID: "a1"}); len(got) != 1 {
		t.Fatalf("the cleanup reached the pre-swap store: it holds %d artifacts, want the 1 written before the swap", len(got))
	}
}
