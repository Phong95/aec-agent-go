package e2e_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	aec "github.com/Phong95/aec-agent-go"
	"github.com/Phong95/aec-agent-go/aecmock"
	"github.com/Phong95/aec-agent-go/components/agent/memorysystem/memory"
	"github.com/Phong95/aec-agent-go/components/agent/profile"
	"github.com/Phong95/aec-agent-go/components/persistence"
	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/components/skillhub"
	"github.com/Phong95/aec-agent-go/components/skillhub/skill"
)

// stepLog records the order in which the steps of RemoveAgent reach their stores.
type stepLog struct {
	mu    sync.Mutex
	steps []string
	// ctxTags is the rigCtxKey value of the ctx each release saw, in call order.
	ctxTags []any
}

// rigCtxKey tags a ctx so a test can check it reached a release.
type rigCtxKey struct{}

func (l *stepLog) addCtx(ctx context.Context) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ctxTags = append(l.ctxTags, ctx.Value(rigCtxKey{}))
}

func (l *stepLog) ctxList() []any {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.ctxTags)
}

func (l *stepLog) add(s string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.steps = append(l.steps, s)
}

func (l *stepLog) reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.steps = nil
}

func (l *stepLog) list() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.steps)
}

// removeProvider is an in-memory sandbox provider that records the two kinds of cold
// data removal RemoveAgent makes: the agent's own sandbox data (step 1, through the
// agent's Dispose with RemoveData) and the agent's folder in the shared sandbox (step
// 3, through the sandbox hub). Either can be made to fail.
type removeProvider struct {
	*aecmock.InMemoryProvider
	log       *stepLog
	failOwn   error
	failShare error
}

func (p *removeProvider) RemoveData(ctx context.Context, spec sandboxhub.Spec, cc sandboxhub.CreateContext, agentID string) error {
	if cc.Tenancy == sandboxhub.TenancyShared {
		p.log.add("sandbox.shared")
		p.log.addCtx(ctx)
		if p.failShare != nil {
			return p.failShare
		}
	} else {
		p.log.add("agent.own-data")
		if p.failOwn != nil {
			return p.failOwn
		}
	}
	return p.InMemoryProvider.RemoveData(ctx, spec, cc, agentID)
}

// removeAgents records Delete and can make it fail.
type removeAgents struct {
	persistence.AgentStore
	log *stepLog
	err error
}

func (s *removeAgents) Delete(ctx context.Context, id string) error {
	s.log.add("agents.delete")
	if s.err != nil {
		return s.err
	}
	return s.AgentStore.Delete(ctx, id)
}

// removeArtifacts records DeleteByAgent and can make it fail.
type removeArtifacts struct {
	persistence.ArtifactStore
	log *stepLog
	err error
}

func (s *removeArtifacts) DeleteByAgent(ctx context.Context, id string) error {
	s.log.add("artifacts.release")
	s.log.addCtx(ctx)
	if s.err != nil {
		return s.err
	}
	return s.ArtifactStore.DeleteByAgent(ctx, id)
}

// removeRig is an AgentCore with an agent a1 whose own-sandbox data, shared-sandbox
// folder and artifacts all have a recording store behind them.
type removeRig struct {
	core      *aec.AgentCore
	log       *stepLog
	provider  *removeProvider
	agents    *removeAgents
	artifacts *removeArtifacts
	logs      *bytes.Buffer
}

func newRemoveRig(t *testing.T) *removeRig {
	t.Helper()
	log := &stepLog{}
	agents := &removeAgents{AgentStore: persistence.NewMemoryAgentStore(), log: log}
	artifacts := &removeArtifacts{ArtifactStore: persistence.NewMemoryArtifactStore(), log: log}
	var logs bytes.Buffer
	core, err := aec.NewAgentCore(
		aec.WithPersistence(persistence.NewPersistenceHub(persistence.WithAgents(agents), persistence.WithArtifacts(artifacts))),
		aec.WithLogger(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))),
	)
	if err != nil {
		t.Fatal(err)
	}
	provider := &removeProvider{InMemoryProvider: aecmock.NewInMemoryProvider(), log: log}
	if err := core.UseSandboxProvider(provider); err != nil {
		t.Fatal(err)
	}
	if err := core.SandboxHub().ConfigureShared(sandboxhub.Spec{Kind: sandboxhub.KindDocker}); err != nil {
		t.Fatal(err)
	}
	p := testProfile("a1")
	p.SystemPrompt = "TOP-SECRET-PROMPT"
	if _, err := core.SpawnAgent(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	return &removeRig{core: core, log: log, provider: provider, agents: agents, artifacts: artifacts, logs: &logs}
}

// TestAgentCore_RemoveAgentOrder checks the RemoveAgent order: the orchestration disposes
// the agent with its data (logged on failure), then the profile is deleted (a failure is
// returned at once), then the sandbox hub and the artifact hub release the agent's data
// (logged on failure).
func TestAgentCore_RemoveAgentOrder(t *testing.T) {
	t.Run("runs the four steps in the TS order", func(t *testing.T) {
		r := newRemoveRig(t)
		if err := r.core.RemoveAgent(t.Context(), "a1"); err != nil {
			t.Fatalf("RemoveAgent: %v", err)
		}
		want := []string{"agent.own-data", "agents.delete", "sandbox.shared", "artifacts.release"}
		if got := r.log.list(); !slices.Equal(got, want) {
			t.Fatalf("steps = %v, want %v", got, want)
		}
		if _, ok := r.core.Agent("a1"); ok {
			t.Error("the agent is still registered")
		}
		if stored, err := r.core.PersistenceHub().Agents.Load(t.Context(), "a1"); err != nil || stored != nil {
			t.Errorf("Agents.Load(a1) = %v, %v, want the profile deleted", stored, err)
		}
		// the id can be used again
		if _, err := r.core.SpawnAgent(t.Context(), testProfile("a1")); err != nil {
			t.Errorf("SpawnAgent after RemoveAgent: %v", err)
		}
	})

	t.Run("a failing dispose is logged and the rest still runs", func(t *testing.T) {
		r := newRemoveRig(t)
		r.provider.failOwn = errors.New("docker is down")
		if err := r.core.RemoveAgent(t.Context(), "a1"); err != nil {
			t.Fatalf("RemoveAgent = %v, want nil: a failing dispose is best effort", err)
		}
		want := []string{"agent.own-data", "agents.delete", "sandbox.shared", "artifacts.release"}
		if got := r.log.list(); !slices.Equal(got, want) {
			t.Fatalf("steps = %v, want %v", got, want)
		}
		if _, ok := r.core.Agent("a1"); ok {
			t.Error("the agent stays registered after a failed dispose (TS removes it in finally)")
		}
		out := r.logs.String()
		if !strings.Contains(out, "a1") || !strings.Contains(out, "docker is down") {
			t.Errorf("log = %q, want the agent id and the error", out)
		}
	})

	t.Run("a failing Delete is returned at once and skips both releases", func(t *testing.T) {
		r := newRemoveRig(t)
		boom := errors.New("store is down")
		r.agents.err = boom
		if err := r.core.RemoveAgent(t.Context(), "a1"); !errors.Is(err, boom) {
			t.Fatalf("RemoveAgent = %v, want the Delete error", err)
		}
		want := []string{"agent.own-data", "agents.delete"}
		if got := r.log.list(); !slices.Equal(got, want) {
			t.Fatalf("steps = %v, want %v (no release after a failed Delete)", got, want)
		}
	})

	t.Run("failing releases are logged and RemoveAgent returns nil", func(t *testing.T) {
		r := newRemoveRig(t)
		r.provider.failShare = errors.New("shared volume is busy")
		r.artifacts.err = errors.New("artifact store is down")
		if err := r.core.RemoveAgent(t.Context(), "a1"); err != nil {
			t.Fatalf("RemoveAgent = %v, want nil: the releases are best effort", err)
		}
		want := []string{"agent.own-data", "agents.delete", "sandbox.shared", "artifacts.release"}
		if got := r.log.list(); !slices.Equal(got, want) {
			t.Fatalf("steps = %v, want %v (the artifact release runs after a failed sandbox release)", got, want)
		}
		out := r.logs.String()
		for _, frag := range []string{"shared volume is busy", "artifact store is down", "a1"} {
			if !strings.Contains(out, frag) {
				t.Errorf("log = %q, want it to contain %q", out, frag)
			}
		}
		if strings.Contains(out, "TOP-SECRET-PROMPT") {
			t.Errorf("the log carries profile content: %q", out)
		}
	})

	// WR-A01: an id with no live agent has no Agent.Dispose to remove its own-sandbox
	// data (cold: a volume or folder left by an earlier run), so RemoveAgent removes it
	// through a throwaway sandbox manager before it deletes the profile.
	t.Run("an id with no live agent still deletes its stored data and its cold own-sandbox data", func(t *testing.T) {
		r := newRemoveRig(t)
		if err := r.core.RemoveAgent(t.Context(), "never-spawned"); err != nil {
			t.Fatalf("RemoveAgent = %v, want nil", err)
		}
		want := []string{"agent.own-data", "agents.delete", "sandbox.shared", "artifacts.release"}
		if got := r.log.list(); !slices.Equal(got, want) {
			t.Fatalf("steps = %v, want %v", got, want)
		}
	})

	// WR-A01: a host that disposed the agent itself (without RemoveData) leaves it in the
	// registry; RemoveAgent must still remove the own-sandbox data, not find a disposed
	// agent whose Dispose is a no-op.
	t.Run("an agent the host already disposed still has its own-sandbox data removed", func(t *testing.T) {
		r := newRemoveRig(t)
		a, ok := r.core.Agent("a1")
		if !ok {
			t.Fatal("a1 is not live")
		}
		if err := a.Dispose(t.Context()); err != nil {
			t.Fatalf("host Dispose: %v", err)
		}
		if got := r.log.list(); len(got) != 0 {
			t.Fatalf("the host Dispose without RemoveData removed data: %v", got)
		}
		if err := r.core.RemoveAgent(t.Context(), "a1"); err != nil {
			t.Fatalf("RemoveAgent = %v, want nil", err)
		}
		want := []string{"agent.own-data", "agents.delete", "sandbox.shared", "artifacts.release"}
		if got := r.log.list(); !slices.Equal(got, want) {
			t.Fatalf("steps = %v, want %v", got, want)
		}
	})

	// WR-A01: a Dispose whose own-data removal failed is retried by RemoveAgent.
	t.Run("a Dispose that failed to remove the data is retried by RemoveAgent", func(t *testing.T) {
		r := newRemoveRig(t)
		a, _ := r.core.Agent("a1")
		r.provider.failOwn = errors.New("docker is down")
		if err := a.Delete(t.Context()); err == nil {
			t.Fatal("host Delete = nil while the provider was failing")
		}
		r.provider.failOwn = nil
		r.log.reset()
		if err := r.core.RemoveAgent(t.Context(), "a1"); err != nil {
			t.Fatalf("RemoveAgent = %v, want nil", err)
		}
		want := []string{"agent.own-data", "agents.delete", "sandbox.shared", "artifacts.release"}
		if got := r.log.list(); !slices.Equal(got, want) {
			t.Fatalf("steps = %v, want the own-data removal retried: %v", got, want)
		}
	})
}

// failAfterSpawn is an agent store whose Save can be made to fail or to panic, or to
// block until its ctx ends, after the agents were spawned.
type failAfterSpawn struct {
	persistence.AgentStore
	mu      sync.Mutex
	saved   []string
	err     error
	panicOn string
	block   bool
}

func (s *failAfterSpawn) Save(ctx context.Context, p profile.AgentProfile) error {
	s.mu.Lock()
	err, panicOn, block := s.err, s.panicOn, s.block
	s.mu.Unlock()
	if panicOn == p.ID {
		panic("store exploded")
	}
	if block {
		<-ctx.Done()
		return ctx.Err()
	}
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.saved = append(s.saved, p.ID)
	s.mu.Unlock()
	return s.AgentStore.Save(ctx, p)
}

func (s *failAfterSpawn) set(f func(*failAfterSpawn)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f(s)
}

// newSkillFanOutCore is an AgentCore over a store that can fail, with the agents a1 (site-diary
// and other), a2 (site-diary) and a3 (other), and the custom skill site-diary.
func newSkillFanOutCore(t *testing.T, buf *bytes.Buffer) (*aec.AgentCore, *failAfterSpawn) {
	t.Helper()
	store := &failAfterSpawn{AgentStore: persistence.NewMemoryAgentStore()}
	core, err := aec.NewAgentCore(
		aec.WithPersistence(persistence.NewPersistenceHub(persistence.WithAgents(store))),
		aec.WithLogger(slog.New(slog.NewTextHandler(buf, nil))),
	)
	if err != nil {
		t.Fatal(err)
	}
	for id, skills := range map[string][]string{"a1": {"site-diary", "other"}, "a2": {"site-diary"}, "a3": {"other"}} {
		p := testProfile(id)
		p.Skills = skills
		if _, err := core.SpawnAgent(t.Context(), p); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := core.SkillHub().Create(t.Context(), skill.Package{Content: skillMD("site-diary")}, skillhub.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	return core, store
}

// TestAgentCore_SkillDeleteDisablesForAllAgents: deleting a skill through the skill hub
// (whoever deletes it) disables it for every agent that had it, synchronously, and
// saves each changed profile through the live persistence hub.
func TestAgentCore_SkillDeleteDisablesForAllAgents(t *testing.T) {
	t.Run("disables it for every agent that had it and saves each profile", func(t *testing.T) {
		var logs bytes.Buffer
		core, store := newSkillFanOutCore(t, &logs)
		store.set(func(s *failAfterSpawn) { s.saved = nil })

		if err := core.SkillHub().Delete(t.Context(), "site-diary"); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		// Synchronous: no wait, no sleep.
		for id, want := range map[string][]string{"a1": {"other"}, "a2": {}, "a3": {"other"}} {
			a, _ := core.Agent(id)
			if got := a.Profile().Skills; !slices.Equal(got, want) {
				t.Errorf("%s skills = %v, want %v", id, got, want)
			}
		}
		store.mu.Lock()
		saved := slices.Clone(store.saved)
		store.mu.Unlock()
		slices.Sort(saved)
		if !slices.Equal(saved, []string{"a1", "a2"}) {
			t.Errorf("saved profiles = %v, want a1 and a2 only (a3 never had the skill)", saved)
		}
		stored, err := core.PersistenceHub().Agents.Load(t.Context(), "a1")
		if err != nil || stored == nil || !slices.Equal(stored.Skills, []string{"other"}) {
			t.Errorf("stored a1 = %+v, %v, want skills [other]", stored, err)
		}
	})

	t.Run("a failing save is logged and the delete still succeeds", func(t *testing.T) {
		var logs bytes.Buffer
		core, store := newSkillFanOutCore(t, &logs)
		store.set(func(s *failAfterSpawn) { s.err = errors.New("db is down") })

		if err := core.SkillHub().Delete(t.Context(), "site-diary"); err != nil {
			t.Fatalf("Delete = %v, want nil: a failed save must not fail the delete", err)
		}
		// the in-memory update happened for both agents despite the failed saves
		a1, _ := core.Agent("a1")
		a2, _ := core.Agent("a2")
		if !slices.Equal(a1.Profile().Skills, []string{"other"}) || len(a2.Profile().Skills) != 0 {
			t.Errorf("skills after the failed saves: a1 %v, a2 %v", a1.Profile().Skills, a2.Profile().Skills)
		}
		out := logs.String()
		for _, frag := range []string{"a1", "a2", "site-diary", "db is down"} {
			if !strings.Contains(out, frag) {
				t.Errorf("log = %q, want it to contain %q", out, frag)
			}
		}
	})

	t.Run("a panic in the save is recovered and logged", func(t *testing.T) {
		var logs bytes.Buffer
		core, store := newSkillFanOutCore(t, &logs)
		store.set(func(s *failAfterSpawn) { s.panicOn = "a1" })

		if err := core.SkillHub().Delete(t.Context(), "site-diary"); err != nil {
			t.Fatalf("Delete = %v, want nil", err)
		}
		// The log has the type of the recovered panic, never its value (10-SECURITY UF-1,
		// Phase 12 12-02).
		if !strings.Contains(logs.String(), "panic=string") {
			t.Errorf("log = %q, want the type of the recovered panic", logs.String())
		}
		if strings.Contains(logs.String(), "store exploded") {
			t.Errorf("log = %q shows the panic value", logs.String())
		}
	})

	t.Run("the saves are bounded to 10 seconds each, run in parallel and inline", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			var logs bytes.Buffer
			core, store := newSkillFanOutCore(t, &logs)
			store.set(func(s *failAfterSpawn) { s.block = true })

			start := time.Now()
			if err := core.SkillHub().Delete(t.Context(), "site-diary"); err != nil {
				t.Fatalf("Delete = %v, want nil", err)
			}
			// a1 and a2 both had the skill. Each save waits for its own 10 s bound, and the
			// agents are handled in parallel, so Delete
			// returns after one bound, not two.
			if got := time.Since(start); got != 10*time.Second {
				t.Errorf("Delete took %v of fake time, want two parallel bounded saves of 10 s", got)
			}
			if !strings.Contains(logs.String(), "deadline exceeded") {
				t.Errorf("log = %q, want the timeout", logs.String())
			}
		})
	})
}

// TestAgentCore_DisposeStopsJobsAndMemory: after the core's Dispose nothing is left
// running: the started interval job and the armed consolidation scheduler of an agent are
// stopped, and no goroutine remains (a goroutine left in the bubble would make
// synctest.Test panic).
// Update (A-126): scheduled tasks (the interval job) were removed; the test checks
// the consolidation scheduler.
func TestAgentCore_DisposeStopsJobsAndMemory(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := t.Context()
		core, mock := newCore(t, "ok")
		interval := 1.0
		p := testProfile("a1")
		p.Memory = memory.Config{Enabled: true, UpdateUser: true, Consolidation: &memory.ConsolidationTrigger{IntervalMinutes: &interval}}
		a, err := core.SpawnAgent(ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		// a finished turn marks the session dirty, so the armed scheduler has work
		if _, err := a.StartSession(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := a.Chat(ctx, "hello"); err != nil {
			t.Fatalf("Chat: %v", err)
		}

		time.Sleep(time.Minute)
		synctest.Wait()
		calls := len(mock.Calls())

		if err := core.Dispose(ctx); err != nil {
			t.Fatalf("Dispose: %v", err)
		}
		time.Sleep(10 * time.Minute)
		synctest.Wait()
		if n := len(mock.Calls()); n != calls {
			t.Errorf("%d model calls after Dispose, want the scheduler stopped at %d", n, calls)
		}
	})
}

// TestAgentCore_DisposeAgentUnloadsAndKeepsData stops and unloads a live agent without
// deleting it: the stored profile survives, so the agent can be spawned again.
func TestAgentCore_DisposeAgentUnloadsAndKeepsData(t *testing.T) {
	ctx := t.Context()
	core, _ := newCore(t, "ok")
	defer func() {
		if err := core.Dispose(ctx); err != nil {
			t.Errorf("Dispose: %v", err)
		}
	}()
	if _, err := core.SpawnAgent(ctx, testProfile("keep")); err != nil {
		t.Fatalf("SpawnAgent: %v", err)
	}
	if err := core.DisposeAgent(ctx, "keep"); err != nil {
		t.Fatalf("DisposeAgent: %v", err)
	}
	if _, live := core.Agent("keep"); live {
		t.Fatal("the agent is still registered after DisposeAgent")
	}
	if p, err := core.PersistenceHub().Agents.Load(ctx, "keep"); err != nil || p == nil {
		t.Fatalf("stored profile after DisposeAgent: %v %v, want it kept", p, err)
	}
	if _, err := core.SpawnAgent(ctx, testProfile("keep")); err != nil {
		t.Fatalf("SpawnAgent after DisposeAgent: %v", err)
	}
	if err := core.DisposeAgent(ctx, "unknown"); err != nil {
		t.Fatalf("DisposeAgent of an unknown id = %v, want nil", err)
	}
}
