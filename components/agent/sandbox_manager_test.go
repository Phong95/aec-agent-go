package agent_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Phong95/aec-agent-go/aecmock"
	"github.com/Phong95/aec-agent-go/components/agent"
	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/kernel"
)

// selector is the mutable profile sandbox of the SandboxManager cases: the manager
// reads the selection on every call, and a case changes it between calls.
type selector struct {
	mu  sync.Mutex
	sel sandboxhub.Selection
}

func (s *selector) get() sandboxhub.Selection {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sel
}

func (s *selector) set(sel sandboxhub.Selection) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sel = sel
}

// specProvider is an in-memory provider that also records the spec of each sandbox it
// creates, which InMemoryProvider does not keep.
type specProvider struct {
	*aecmock.InMemoryProvider
	mu    sync.Mutex
	specs map[string]sandboxhub.Spec
}

func newSpecProvider(opts ...aecmock.InMemoryOption) *specProvider {
	return &specProvider{InMemoryProvider: aecmock.NewInMemoryProvider(opts...), specs: map[string]sandboxhub.Spec{}}
}

func (p *specProvider) Create(ctx context.Context, spec sandboxhub.Spec, cc sandboxhub.CreateContext) (sandboxhub.Sandbox, error) {
	p.mu.Lock()
	p.specs[cc.ID] = spec.Clone()
	p.mu.Unlock()
	return p.InMemoryProvider.Create(ctx, spec, cc)
}

func (p *specProvider) spec(id string) (sandboxhub.Spec, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s, ok := p.specs[id]
	return s, ok
}

// hubWith returns a hub with an in-memory docker provider and, unless
// shared is false, the shared sandbox configured with the image aec-sandbox:latest.
func hubWith(t *testing.T, opts ...aecmock.InMemoryOption) (*sandboxhub.SandboxHub, *specProvider) {
	t.Helper()
	hub := sandboxhub.NewSandboxHub()
	provider := newSpecProvider(opts...)
	if err := hub.Register(provider); err != nil {
		t.Fatal(err)
	}
	if err := hub.ConfigureShared(sandboxhub.Spec{Kind: sandboxhub.KindDocker, Image: "aec-sandbox:latest"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hub.Dispose(context.WithoutCancel(t.Context())) })
	return hub, provider
}

// managerFor returns the manager of agent-a with the selection.
func managerFor(hub *sandboxhub.SandboxHub, sel sandboxhub.Selection) (*agent.SandboxManager, *selector) {
	s := &selector{sel: sel}
	return agent.NewSandboxManager(hub, "agent-a", s.get), s
}

// mustSandbox gets the manager's sandbox view or fails the test.
func mustSandbox(t *testing.T, m *agent.SandboxManager) sandboxhub.View {
	t.Helper()
	v, err := m.GetSandbox(t.Context())
	if err != nil {
		t.Fatalf("GetSandbox: %v", err)
	}
	return v
}

// released releases the manager, with the ctx of the test that is not cancelled.
func released(t *testing.T, m *agent.SandboxManager, removeData bool) {
	t.Helper()
	if err := m.Release(context.WithoutCancel(t.Context()), removeData); err != nil {
		t.Fatalf("Release(removeData=%v): %v", removeData, err)
	}
}

// sandboxOf is the in-memory sandbox a provider created under id.
func sandboxOf(t *testing.T, p *specProvider, id string) *aecmock.InMemorySandbox {
	t.Helper()
	sb, ok := p.Sandbox(id)
	if !ok {
		t.Fatalf("provider has no sandbox %q", id)
	}
	return sb
}

// TestSandboxManager is "SandboxManager own sandbox lifecycle" and "Own sandbox kind
// (docker or host)" of the sandbox hub cases: the 6 cases that need the agent's manager.
func TestSandboxManager(t *testing.T) {
	t.Run("gives an agent a dedicated sandbox owning the whole /workspace", func(t *testing.T) {
		hub, provider := hubWith(t)
		m, _ := managerFor(hub, ownSelection(sandboxhub.OwnConfig{}))
		own := mustSandbox(t, m)
		if own.ID() != "docker:own:agent-a" {
			t.Fatalf("id = %q, want docker:own:agent-a", own.ID())
		}
		if own.Traits().Tenancy != sandboxhub.TenancyOwn {
			t.Fatalf("tenancy = %q, want own", own.Traits().Tenancy)
		}
		if own.WorkingDir() != "/workspace" {
			t.Fatalf("working dir = %q, want /workspace", own.WorkingDir())
		}
		if roots := own.Roots(); !slices.Equal(roots, []string{"/workspace"}) {
			t.Fatalf("roots = %v, want [/workspace]", roots)
		}
		spec, ok := provider.spec("docker:own:agent-a")
		if !ok {
			t.Fatal("the provider never created docker:own:agent-a")
		}
		l := spec.Limits
		if spec.Kind != sandboxhub.KindDocker || spec.Image != "aec-sandbox:latest" || spec.Network != sandboxhub.NetworkNone ||
			l == nil || l.Cpus == nil || *l.Cpus != 2 || l.MemoryMb == nil || *l.MemoryMb != 2048 || l.Pids == nil || *l.Pids != 256 {
			t.Fatalf("spec = %+v (limits %+v), want docker, aec-sandbox:latest, none, 2 cpus, 2048 MB, 256 pids", spec, spec.Limits)
		}
		released(t, m, false)
	})

	t.Run("recreates an own sandbox when its config changes, and reuses it otherwise", func(t *testing.T) {
		hub, provider := hubWith(t)
		m, sel := managerFor(hub, ownSelection(sandboxhub.OwnConfig{}))

		mustSandbox(t, m)
		mustSandbox(t, m)
		if n := provider.CreateCount(); n != 1 {
			t.Fatalf("CreateCount after two calls = %d, want 1", n)
		}

		first := sandboxOf(t, provider, "docker:own:agent-a")
		pids := 50
		sel.set(ownSelection(sandboxhub.OwnConfig{Limits: &sandboxhub.LimitsOverride{Pids: &pids}}))
		mustSandbox(t, m)
		if n := provider.CreateCount(); n != 2 {
			t.Fatalf("CreateCount after the config changed = %d, want 2", n)
		}
		if !first.Disposed() {
			t.Fatal("the stale own sandbox was not disposed")
		}
		released(t, m, false)
	})

	t.Run("release stops the own sandbox; releaseAgentData clears the agent's shared folder", func(t *testing.T) {
		hub, provider := hubWith(t)
		m, _ := managerFor(hub, ownSelection(sandboxhub.OwnConfig{}))
		ctx := t.Context()

		mustSandbox(t, m)
		shared, err := hub.Resolve(ctx, "agent-a", sandboxhub.Selection{Mode: sandboxhub.SelectionShared})
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if _, err := shared.WriteFile(ctx, "work.txt", "x", sandboxhub.WriteOptions{}); err != nil {
			t.Fatal(err)
		}

		// release without removeData: stops the sandbox, keeps files
		released(t, m, false)
		if !sandboxOf(t, provider, "docker:own:agent-a").Disposed() {
			t.Fatal("Release did not stop the own sandbox")
		}
		if st, err := shared.Stat(ctx, "work.txt"); err != nil || st == nil {
			t.Fatalf("Stat(work.txt) after Release = %v, %v, want the file kept", st, err)
		}

		// releaseAgentData: cleans the agent's shared folder
		if err := hub.ReleaseAgentData(ctx, "agent-a"); err != nil {
			t.Fatalf("ReleaseAgentData: %v", err)
		}
		if sandboxOf(t, provider, "docker:shared").Has("/workspace/agents/agent-a/work.txt") {
			t.Fatal("ReleaseAgentData left work.txt in the shared sandbox")
		}
	})

	t.Run("release({ removeData }) deletes a running own sandbox's files, or cold own data through the provider", func(t *testing.T) {
		hub, provider := hubWith(t)
		m, _ := managerFor(hub, ownSelection(sandboxhub.OwnConfig{}))

		// Running: disposed with its data.
		env := mustSandbox(t, m)
		if _, err := env.WriteFile(t.Context(), "mine.txt", "x", sandboxhub.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
		released(t, m, true)
		own := sandboxOf(t, provider, "docker:own:agent-a")
		if !own.Disposed() {
			t.Fatal("Release(removeData) did not stop the running own sandbox")
		}
		if own.Has("/workspace/mine.txt") {
			t.Fatal("Release(removeData) left mine.txt in the own sandbox")
		}
		if got := provider.RemovedData(); len(got) != 0 {
			t.Fatalf("provider.RemovedData = %v, want none: the running sandbox's kind is not removed twice", got)
		}

		// Not running (another process, or the agent switched to shared): through the provider.
		cold, _ := managerFor(hub, sandboxhub.Selection{Mode: sandboxhub.SelectionShared})
		released(t, cold, true)
		want := []aecmock.RemovedData{{ID: "docker:own:agent-a"}}
		if got := provider.RemovedData(); !slices.Equal(got, want) {
			t.Fatalf("provider.RemovedData = %v, want %v", got, want)
		}

		// A plain Release of a cold sandbox deletes nothing.
		released(t, cold, false)
		if n := len(provider.RemovedData()); n != 1 {
			t.Fatalf("RemovedData count after a plain Release = %d, want 1", n)
		}
	})

	t.Run("dispose stops shared sandbox; own sandbox is stopped by SandboxManager.release()", func(t *testing.T) {
		hub, provider := hubWith(t)
		m, _ := managerFor(hub, ownSelection(sandboxhub.OwnConfig{}))

		if _, err := hub.Resolve(t.Context(), "agent-b", sandboxhub.Selection{}); err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		mustSandbox(t, m)

		// SandboxManager stops its own sandbox.
		released(t, m, false)
		if !sandboxOf(t, provider, "docker:own:agent-a").Disposed() {
			t.Fatal("Release did not stop the own sandbox")
		}
		// The hub stops the shared sandbox.
		if err := hub.Dispose(t.Context()); err != nil {
			t.Fatalf("hub Dispose: %v", err)
		}
		if !sandboxOf(t, provider, "docker:shared").Disposed() {
			t.Fatal("the hub did not stop the shared sandbox")
		}
	})

	t.Run("switching kind starts the other provider's sandbox; each keeps its own files", func(t *testing.T) {
		hub, docker := hubWith(t)
		host := newSpecProvider(aecmock.WithKind(sandboxhub.KindHost))
		if err := hub.Register(host); err != nil {
			t.Fatal(err)
		}
		kinds := []sandboxhub.Kind{sandboxhub.KindDocker, sandboxhub.KindHost}
		hub.SetPolicy(sandboxhub.PolicyUpdate{OwnKinds: &kinds})
		m, sel := managerFor(hub, ownSelection(sandboxhub.OwnConfig{}))
		ctx := t.Context()

		inDocker := mustSandbox(t, m)
		if inDocker.ID() != "docker:own:agent-a" {
			t.Fatalf("id = %q, want docker:own:agent-a", inDocker.ID())
		}
		if _, err := inDocker.WriteFile(ctx, "d.txt", "docker", sandboxhub.WriteOptions{}); err != nil {
			t.Fatal(err)
		}

		sel.set(ownSelection(sandboxhub.OwnConfig{Kind: sandboxhub.KindHost}))
		inHost := mustSandbox(t, m)
		if inHost.ID() != "host:own:agent-a" {
			t.Fatalf("id = %q, want host:own:agent-a", inHost.ID())
		}
		if inHost.Traits().Isolated {
			t.Fatal("a host sandbox must not be isolated")
		}
		if !m.RequiresApproval(inHost.Traits(), sandboxhub.OperationFSWrite) {
			t.Fatal("a write in the host sandbox must need approval")
		}
		if !sandboxOf(t, docker, "docker:own:agent-a").Disposed() {
			t.Fatal("switching kind did not stop the docker own sandbox")
		}
		if st, err := inHost.Stat(ctx, "d.txt"); err != nil || st != nil {
			t.Fatalf("Stat(d.txt) in the host sandbox = %v, %v, want nil: each kind keeps its own files", st, err)
		}

		// Deleting the agent clears the running host sandbox AND the docker one it left behind.
		released(t, m, true)
		if !sandboxOf(t, host, "host:own:agent-a").Disposed() {
			t.Fatal("Release did not stop the host own sandbox")
		}
		if got, want := docker.RemovedData(), []aecmock.RemovedData{{ID: "docker:own:agent-a"}}; !slices.Equal(got, want) {
			t.Fatalf("docker RemovedData = %v, want %v", got, want)
		}
		if got := host.RemovedData(); len(got) != 0 {
			t.Fatalf("host RemovedData = %v, want none", got)
		}
	})
}

// The refusals of GetSandbox keep their error codes and texts.
func TestSandboxManager_Refusals(t *testing.T) {
	t.Run("no provider for the own kind", func(t *testing.T) {
		hub := sandboxhub.NewSandboxHub()
		m, _ := managerFor(hub, ownSelection(sandboxhub.OwnConfig{}))
		_, err := m.GetSandbox(t.Context())
		var se *kernel.SandboxError
		if !errors.As(err, &se) || se.Code != kernel.SandboxCodeProviderUnavailable || se.SandboxID != "docker:own:agent-a" {
			t.Fatalf("GetSandbox = %v, want provider_unavailable for docker:own:agent-a", err)
		}
		if want := `No sandbox provider is registered for "docker"`; se.Message != want {
			t.Fatalf("message = %q, want %q", se.Message, want)
		}
	})
	t.Run("policy refuses the own config", func(t *testing.T) {
		hub, _ := hubWith(t)
		m, _ := managerFor(hub, ownSelection(sandboxhub.OwnConfig{Network: sandboxhub.NetworkFull}))
		_, err := m.GetSandbox(t.Context())
		var se *kernel.SandboxError
		if !errors.As(err, &se) || se.Code != kernel.SandboxCodePolicyDenied {
			t.Fatalf("GetSandbox = %v, want policy_denied", err)
		}
	})
	t.Run("an unrecognised selection", func(t *testing.T) {
		hub, _ := hubWith(t)
		m, _ := managerFor(hub, sandboxhub.Selection{Mode: "bogus"})
		_, err := m.GetSandbox(t.Context())
		var se *kernel.SandboxError
		if !errors.As(err, &se) || se.Code != kernel.SandboxCodePolicyDenied || se.Message != "Unrecognised sandbox selection" {
			t.Fatalf("GetSandbox = %v, want policy_denied Unrecognised sandbox selection", err)
		}
	})
	t.Run("none is disabled", func(t *testing.T) {
		hub, _ := hubWith(t)
		m, _ := managerFor(hub, sandboxhub.Selection{Mode: sandboxhub.SelectionNone})
		_, err := m.GetSandbox(t.Context())
		var se *kernel.SandboxError
		if !errors.As(err, &se) || se.Code != kernel.SandboxCodeDisabled || se.Message != `This agent has no sandbox (sandbox: "none")` {
			t.Fatalf("GetSandbox = %v, want disabled", err)
		}
	})
}

// A failed creation is not cached: the next call retries (Docker Desktop was still
// starting).
func TestSandboxManager_FailedCreateNotCached(t *testing.T) {
	hub, provider := hubWith(t, aecmock.WithFailCreates(1))
	m, _ := managerFor(hub, ownSelection(sandboxhub.OwnConfig{}))

	_, err := m.GetSandbox(t.Context())
	var se *kernel.SandboxError
	if !errors.As(err, &se) || se.Code != kernel.SandboxCodeEnvironmentUnavailable || se.SandboxID != "docker:own:agent-a" {
		t.Fatalf("first GetSandbox = %v, want environment_unavailable for docker:own:agent-a", err)
	}
	mustSandbox(t, m)
	if n := provider.CreateCount(); n != 2 {
		t.Fatalf("CreateCount = %d, want 2 (the failure was retried)", n)
	}
	released(t, m, false)
}

// gatedProvider holds every Create until the gate is opened, ignoring its ctx, like a
// provider that is slow to start a container. entered counts the Create calls.
type gatedProvider struct {
	*aecmock.InMemoryProvider
	gate    chan struct{}
	mu      sync.Mutex
	entered int
}

func newGatedProvider() *gatedProvider {
	return &gatedProvider{InMemoryProvider: aecmock.NewInMemoryProvider(), gate: make(chan struct{})}
}

func (p *gatedProvider) Create(ctx context.Context, spec sandboxhub.Spec, cc sandboxhub.CreateContext) (sandboxhub.Sandbox, error) {
	p.mu.Lock()
	p.entered++
	p.mu.Unlock()
	<-p.gate
	return p.InMemoryProvider.Create(ctx, spec, cc)
}

func (p *gatedProvider) open() { close(p.gate) }

func gatedHub(t *testing.T) (*sandboxhub.SandboxHub, *gatedProvider) {
	t.Helper()
	hub := sandboxhub.NewSandboxHub()
	p := newGatedProvider()
	if err := hub.Register(p); err != nil {
		t.Fatal(err)
	}
	return hub, p
}

// Two concurrent GetSandbox calls on an own selection create the sandbox once and
// both get a view (the shared future of the sandbox hub, for the own sandbox).
func TestSandboxManager_ConcurrentOwnCreateOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		hub, provider := gatedHub(t)
		m, _ := managerFor(hub, ownSelection(sandboxhub.OwnConfig{}))

		const callers = 8
		views := make([]sandboxhub.View, callers)
		errs := make([]error, callers)
		var wg sync.WaitGroup
		for i := range callers {
			wg.Go(func() { views[i], errs[i] = m.GetSandbox(t.Context()) })
		}
		synctest.Wait() // every caller is blocked on the one creation
		provider.open()
		wg.Wait()

		for i := range callers {
			if errs[i] != nil || views[i] == nil || views[i].ID() != "docker:own:agent-a" {
				t.Fatalf("caller %d: view %v, err %v", i, views[i], errs[i])
			}
		}
		if n := provider.CreateCount(); n != 1 {
			t.Fatalf("CreateCount = %d, want 1: concurrent callers share one creation", n)
		}
		if err := m.Release(t.Context(), false); err != nil {
			t.Fatalf("Release: %v", err)
		}
	})
}

// A caller whose ctx ends while the creation runs gets its ctx error, the creation
// carries on for the other caller, and no sandbox is created twice or leaked.
func TestSandboxManager_CallerCancelDoesNotFailOthers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		hub, provider := gatedHub(t)
		m, _ := managerFor(hub, ownSelection(sandboxhub.OwnConfig{}))

		ctxA, cancelA := context.WithCancel(t.Context())
		defer cancelA()
		var wg sync.WaitGroup
		var errA, errB error
		var viewB sandboxhub.View
		wg.Go(func() { _, errA = m.GetSandbox(ctxA) })
		synctest.Wait() // A started the creation and waits for it
		wg.Go(func() { viewB, errB = m.GetSandbox(t.Context()) })
		synctest.Wait() // B waits on the same creation

		cancelA()
		synctest.Wait()
		if !errors.Is(errA, context.Canceled) {
			t.Fatalf("caller A error = %v, want context.Canceled", errA)
		}

		provider.open()
		wg.Wait()
		if errB != nil || viewB == nil {
			t.Fatalf("caller B: view %v, err %v, want a view: A's cancellation must not fail it", viewB, errB)
		}
		if n := provider.CreateCount(); n != 1 {
			t.Fatalf("CreateCount = %d, want 1", n)
		}

		if err := m.Release(t.Context(), false); err != nil {
			t.Fatalf("Release: %v", err)
		}
		own, ok := provider.Sandbox("docker:own:agent-a")
		if !ok || !own.Disposed() {
			t.Fatal("the own sandbox is not stopped by Release")
		}
	})
}

// A Release whose ctx ends while the creation is still running returns that error and
// leaves the finishing creation to dispose the sandbox, so none is leaked.
func TestSandboxManager_ReleaseOrphansPendingCreation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		hub, provider := gatedHub(t)
		m, _ := managerFor(hub, ownSelection(sandboxhub.OwnConfig{}))

		var wg sync.WaitGroup
		wg.Go(func() { _, _ = m.GetSandbox(t.Context()) })
		synctest.Wait() // the creation is running and ignores its ctx

		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		err := m.Release(ctx, false)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Release = %v, want the ctx error after 5 s of fake time", err)
		}

		provider.open()
		wg.Wait()
		synctest.Wait()
		own, ok := provider.Sandbox("docker:own:agent-a")
		if !ok || !own.Disposed() {
			t.Fatal("the orphaned creation left its sandbox running")
		}
	})
}

// Regression tests of the phase 10 code review warnings WR-A02, WR-A03 and WR-A06.

// --- WR-A02: a released SandboxManager creates nothing ---------------------------------

// After Release the manager is closed: a tool call that arrives later (a chat that was
// still running while the agent was disposed) must not start a sandbox that nothing
// would release.
func TestSandboxManager_ClosedAfterRelease(t *testing.T) {
	hub, provider := hubWith(t)
	m, _ := managerFor(hub, ownSelection(sandboxhub.OwnConfig{}))
	mustSandbox(t, m)
	released(t, m, false)

	_, err := m.GetSandbox(t.Context())
	var se *kernel.SandboxError
	if !errors.As(err, &se) || se.Code != kernel.SandboxCodeDisabled {
		t.Fatalf("GetSandbox after Release = %v, want a disabled *kernel.SandboxError", err)
	}
	if n := provider.CreateCount(); n != 1 {
		t.Fatalf("CreateCount = %d, want 1: nothing may be created after Release", n)
	}

	// The shared selection is closed too: the agent is gone.
	shared, _ := managerFor(hub, sandboxhub.Selection{Mode: sandboxhub.SelectionShared})
	released(t, shared, false)
	if _, err := shared.GetSandbox(t.Context()); !errors.As(err, &se) {
		t.Fatalf("shared GetSandbox after Release = %v, want a *kernel.SandboxError", err)
	}
}

// A caller that arrives after a Release whose ctx ended during a creation does not
// start a second creation, and the orphaned creation is still disposed.
func TestSandboxManager_NoCreateAfterOrphaningRelease(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		hub, provider := gatedHub(t)
		m, _ := managerFor(hub, ownSelection(sandboxhub.OwnConfig{}))

		var wg sync.WaitGroup
		wg.Go(func() { _, _ = m.GetSandbox(t.Context()) })
		synctest.Wait() // the creation is running

		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		if err := m.Release(ctx, false); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Release = %v, want the ctx error", err)
		}
		if _, err := m.GetSandbox(t.Context()); err == nil {
			t.Fatal("GetSandbox after Release = nil, want the closed error")
		}

		provider.open()
		wg.Wait()
		synctest.Wait()
		provider.mu.Lock()
		entered := provider.entered
		provider.mu.Unlock()
		if entered != 1 || provider.CreateCount() != 1 {
			t.Fatalf("Create entered %d times (%d finished), want 1", entered, provider.CreateCount())
		}
		if own, ok := provider.Sandbox("docker:own:agent-a"); !ok || !own.Disposed() {
			t.Fatal("the orphaned creation left its sandbox running")
		}
	})
}

// Agent.Sandbox after Dispose fails and creates nothing.
func TestAgent_SandboxAfterDispose(t *testing.T) {
	r := newSandboxRig(t, ownSelection(sandboxhub.OwnConfig{}), true)
	if _, err := r.agent.Sandbox(t.Context()); err != nil {
		t.Fatal(err)
	}
	ctx := context.WithoutCancel(t.Context())
	if err := r.agent.Dispose(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := r.agent.Sandbox(ctx); err == nil {
		t.Fatal("Sandbox after Dispose = nil, want an error")
	}
	if n := r.provider.CreateCount(); n != 1 {
		t.Fatalf("CreateCount = %d, want 1: Dispose must not be followed by a new sandbox", n)
	}
}

// --- WR-A03: the own path checks the agent id -------------------------------------------

func TestSandboxManager_OwnRejectsUnsafeAgentID(t *testing.T) {
	hub, provider := hubWith(t)
	for _, id := range []string{"../x", "a/b", "", "a..b"} {
		m := agent.NewSandboxManager(hub, id, func() sandboxhub.Selection { return ownSelection(sandboxhub.OwnConfig{}) })
		_, err := m.GetSandbox(t.Context())
		var se *kernel.SandboxError
		if !errors.As(err, &se) || se.Code != kernel.SandboxCodePolicyDenied {
			t.Errorf("GetSandbox(agent %q) = %v, want policy_denied", id, err)
		}
	}
	if n := provider.CreateCount(); n != 0 {
		t.Fatalf("CreateCount = %d, want 0: an unsafe id must never reach a provider", n)
	}
}

// --- WR-A06: a recreation is serialised --------------------------------------------------

// slowProvider is an in-memory provider whose sandboxes block in Dispose until the gate
// is opened, and that logs the order of Create and Dispose.
type slowProvider struct {
	*aecmock.InMemoryProvider
	gate chan struct{}

	mu  sync.Mutex
	log []string
}

func newSlowProvider() *slowProvider {
	return &slowProvider{InMemoryProvider: aecmock.NewInMemoryProvider(), gate: make(chan struct{})}
}

func (p *slowProvider) add(s string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.log = append(p.log, s)
}

func (p *slowProvider) events() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.log)
}

func (p *slowProvider) Create(ctx context.Context, spec sandboxhub.Spec, cc sandboxhub.CreateContext) (sandboxhub.Sandbox, error) {
	p.add("create")
	sb, err := p.InMemoryProvider.Create(ctx, spec, cc)
	if err != nil {
		return nil, err
	}
	return &slowSandbox{Sandbox: sb, p: p}, nil
}

type slowSandbox struct {
	sandboxhub.Sandbox
	p *slowProvider
}

func (s *slowSandbox) Dispose(ctx context.Context, removeData bool) error {
	s.p.add("dispose-start")
	<-s.p.gate
	err := s.Sandbox.Dispose(ctx, removeData)
	s.p.add("dispose-end")
	return err
}

// While the stale own sandbox is being disposed, a second caller with the new config
// waits: it must not create a sandbox of the same id before the old one is gone.
func TestSandboxManager_RecreateWaitsForStaleDispose(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		hub := sandboxhub.NewSandboxHub()
		provider := newSlowProvider()
		if err := hub.Register(provider); err != nil {
			t.Fatal(err)
		}
		if err := hub.ConfigureShared(sandboxhub.Spec{Kind: sandboxhub.KindDocker, Image: "aec-sandbox:latest"}); err != nil {
			t.Fatal(err)
		}
		m, sel := managerFor(hub, ownSelection(sandboxhub.OwnConfig{}))
		mustSandbox(t, m)

		pids := 50
		sel.set(ownSelection(sandboxhub.OwnConfig{Limits: &sandboxhub.LimitsOverride{Pids: &pids}}))
		views := make([]sandboxhub.View, 2)
		errs := make([]error, 2)
		var wg sync.WaitGroup
		wg.Go(func() { views[0], errs[0] = m.GetSandbox(t.Context()) })
		synctest.Wait() // the first caller is disposing the stale sandbox
		wg.Go(func() { views[1], errs[1] = m.GetSandbox(t.Context()) })
		synctest.Wait()

		if got, want := provider.events(), []string{"create", "dispose-start"}; !slices.Equal(got, want) {
			t.Fatalf("events while the stale sandbox is disposed = %v, want %v: the second caller must wait", got, want)
		}

		close(provider.gate)
		wg.Wait()
		for i := range errs {
			if errs[i] != nil || views[i] == nil {
				t.Fatalf("caller %d: view %v, err %v", i, views[i], errs[i])
			}
		}
		if got, want := provider.events(), []string{"create", "dispose-start", "dispose-end", "create"}; !slices.Equal(got, want) {
			t.Fatalf("events = %v, want %v: the new sandbox is created after the old one is gone", got, want)
		}
		if err := m.Release(t.Context(), false); err != nil {
			t.Fatalf("Release: %v", err)
		}
	})
}
