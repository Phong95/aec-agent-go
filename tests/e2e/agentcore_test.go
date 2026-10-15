package e2e_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	aec "github.com/Phong95/aec-agent-go"
	"github.com/Phong95/aec-agent-go/aecmock"
	"github.com/Phong95/aec-agent-go/components/agent/agentloop"
	"github.com/Phong95/aec-agent-go/components/agent/profile"
	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/components/persistence"
	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/components/skillhub"
	"github.com/Phong95/aec-agent-go/components/skillhub/skill"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/kernel"
)

// testCompactor is the default compactor of the tests: a 200000 token window with
// a 0.8 threshold, which these short runs never reach.
func testCompactor() profile.CompactorConfig {
	return profile.CompactorConfig{
		ContextWindowTokens:      200_000,
		Threshold:                0.8,
		MinTailUserMessages:      4,
		ProtectFirstN:            2,
		ProactivePruneTokens:     50_000,
		OldToolResultMinTokens:   2_000,
		TailTargetRatio:          0.2,
		ProtectLastN:             10,
		SummarizeMaxOutputTokens: 4_096,
	}
}

// testProfile is a minimal profile for the AgentCore tests.
func testProfile(id string) profile.AgentProfile {
	return profile.AgentProfile{
		ID:           id,
		Name:         id,
		SystemPrompt: "You are terse.",
		Compactor:    testCompactor(),
	}
}

// newCore builds an AgentCore with one mock provider that always answers reply.
func newCore(t *testing.T, reply string, opts ...aec.AgentCoreOption) (*aec.AgentCore, *aecmock.MockProvider) {
	t.Helper()
	core, err := aec.NewAgentCore(opts...)
	if err != nil {
		t.Fatalf("NewAgentCore: %v", err)
	}
	mock := aecmock.NewMockProvider("mock", func([]kernel.Message) string { return reply })
	if err := core.UseLLMProvider(mock); err != nil {
		t.Fatalf("UseLLMProvider: %v", err)
	}
	return core, mock
}

// The tracer: AgentCore -> SpawnAgent -> StartSession -> Chat over the real managers
// and the Phase 9 loop, with the reply and the user message persisted in the core's
// persistence hub.
func TestAgentCore_SpawnStartSessionChat(t *testing.T) {
	ctx := t.Context()
	core, mock := newCore(t, "Hello back!")

	a, err := core.SpawnAgent(ctx, testProfile("a1"))
	if err != nil {
		t.Fatalf("SpawnAgent: %v", err)
	}
	session, err := a.StartSession(ctx)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	res, err := a.Chat(ctx, "Hello")
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if res.Message.Content != "Hello back!" || res.Message.Role != kernel.RoleAssistant {
		t.Fatalf("reply = %+v, want the assistant message Hello back!", res.Message)
	}
	if res.Iterations != 1 || res.Aborted {
		t.Fatalf("Iterations = %d, Aborted = %v, want 1 and false", res.Iterations, res.Aborted)
	}

	// The provider saw the system prompt, then the user message.
	calls := mock.Calls()
	if len(calls) != 1 {
		t.Fatalf("provider calls = %d, want 1", len(calls))
	}
	sent := calls[0].Messages
	if len(sent) != 2 ||
		sent[0].Role != kernel.RoleSystem || sent[0].Content != "You are terse." ||
		sent[1].Role != kernel.RoleUser || sent[1].Content != "Hello" {
		t.Fatalf("provider input = %+v, want [system You are terse., user Hello]", sent)
	}

	// The user message and the reply are in the core's persistence hub, in order.
	stored, err := core.PersistenceHub().Messages.Get(ctx, session.ID)
	if err != nil {
		t.Fatalf("Messages.Get: %v", err)
	}
	if len(stored) != 2 ||
		stored[0].Role != kernel.RoleUser || stored[0].Content != "Hello" ||
		stored[1].Role != kernel.RoleAssistant || stored[1].Content != "Hello back!" {
		t.Fatalf("stored messages = %+v, want [user Hello, assistant Hello back!]", stored)
	}

	// SpawnAgent saved the profile, and the agent is registered.
	saved, err := core.PersistenceHub().Agents.Load(ctx, "a1")
	if err != nil || saved == nil || saved.ID != "a1" || saved.SystemPrompt != "You are terse." {
		t.Fatalf("Agents.Load(a1) = %+v, %v, want the spawned profile", saved, err)
	}
	listed := core.ListAgents()
	if len(listed) != 1 || listed[0].ID() != "a1" || listed[0] != a {
		t.Fatalf("ListAgents() = %v, want exactly the spawned agent a1", listed)
	}

	if err := core.Dispose(ctx); err != nil {
		t.Fatalf("Dispose: %v", err)
	}
}

// Chat before a session exists fails with the expected text, and the error still matches
// the loop's sentinel.
func TestAgentCore_ChatWithoutSession(t *testing.T) {
	ctx := t.Context()
	core, mock := newCore(t, "unused")
	a, err := core.SpawnAgent(ctx, testProfile("a1"))
	if err != nil {
		t.Fatalf("SpawnAgent: %v", err)
	}
	_, err = a.Chat(ctx, "Hello")
	if err == nil {
		t.Fatal("Chat without a session succeeded")
	}
	if want := "No active session \u2014 call startSession or loadSession first"; err.Error() != want {
		t.Fatalf("error text = %q, want %q", err.Error(), want)
	}
	if !errors.Is(err, agentloop.ErrNoActiveSession) {
		t.Fatalf("errors.Is(err, agentloop.ErrNoActiveSession) = false for %v", err)
	}
	if n := len(mock.Calls()); n != 0 {
		t.Fatalf("the provider was called %d times without a session", n)
	}
	if err := core.Dispose(ctx); err != nil {
		t.Fatalf("Dispose: %v", err)
	}
}

// skillPackage is a minimal valid SKILL.md package.
func skillPackage(name string) skill.Package {
	return skill.Package{Content: fmt.Sprintf("---\nname: %s\ndescription: Does %s.\n---\n\nStep 1.\n", name, name)}
}

// The core implements every Kernel method; each one is exercised here.
func TestAgentCore_KernelSurface(t *testing.T) {
	ctx := t.Context()

	t.Run("LLMHub", func(t *testing.T) {
		core, err := aec.NewAgentCore()
		if err != nil {
			t.Fatal(err)
		}
		// An empty hub has no primary.
		if _, err := core.LLMHub().Primary(); !errors.Is(err, llmhub.ErrNoProvider) {
			t.Fatalf("LLM().Primary() on an empty hub = %v, want llmhub.ErrNoProvider", err)
		}
		// An unknown name is reported as missing, never answered with the primary.
		if _, ok := core.LLMHub().Get("unknown"); ok {
			t.Fatal("LLM().Get(unknown) on an empty hub found a provider")
		}
		first := aecmock.NewMockProvider("first", nil)
		second := aecmock.NewMockProvider("second", nil)
		for _, p := range []llmhub.Provider{first, second} {
			if err := core.UseLLMProvider(p); err != nil {
				t.Fatalf("UseLLMProvider: %v", err)
			}
		}
		if got, err := core.LLMHub().Primary(); err != nil || got.Name() != "first" {
			t.Errorf("LLM().Primary() = %v, %v, want first", got, err)
		}
		for _, name := range []string{"first", "second"} {
			if got, ok := core.LLMHub().Get(name); !ok || got.Name() != name {
				t.Errorf("LLM().Get(%q) = %v, %v, want %s", name, got, ok, name)
			}
		}
		if got, ok := core.LLMHub().Get("unknown"); ok || got != nil {
			t.Errorf("LLM().Get(unknown) = %v, %v, want nil and false", got, ok)
		}
		var names []string
		for _, p := range core.LLMHub().List() {
			names = append(names, p.Name())
		}
		if !slices.Equal(names, []string{"first", "second"}) {
			t.Errorf("LLM().List() = %v, want [first second]", names)
		}
		if err := core.UseLLMProvider(nil); !errors.Is(err, llmhub.ErrNilProvider) {
			t.Errorf("UseLLMProvider(nil) = %v, want llmhub.ErrNilProvider", err)
		}
	})

	t.Run("Tool and Tools", func(t *testing.T) {
		core, err := aec.NewAgentCore()
		if err != nil {
			t.Fatal(err)
		}
		if err := core.UseTool(&aecmock.ScheduleLookupTool{}); err != nil {
			t.Fatalf("UseTool: %v", err)
		}
		if err := core.UseTool(&aecmock.ScheduleLookupTool{}); !errors.Is(err, toolhub.ErrAlreadyRegistered) {
			t.Fatalf("UseTool twice = %v, want toolhub.ErrAlreadyRegistered", err)
		}
		def, ok := core.ToolHub().Get("schedule_lookup")
		if !ok || def.Name != "schedule_lookup" {
			t.Fatalf("Tool(schedule_lookup) = %+v, %v", def, ok)
		}
		if _, ok := core.ToolHub().Get("missing"); ok {
			t.Fatal("Tool(missing) found a tool")
		}
		if tools := core.ToolHub().List(); len(tools) != 1 || tools[0].Name != "schedule_lookup" {
			t.Fatalf("Tools() = %+v, want the one registered definition", tools)
		}
	})

	t.Run("skills", func(t *testing.T) {
		core, err := aec.NewAgentCore()
		if err != nil {
			t.Fatal(err)
		}
		// The three essential skills are registered built-in.
		// Update (A-70): four now, with watch-and-react.
		for _, name := range skillhub.EssentialSkillNames() {
			sk, ok, err := core.SkillHub().Get(ctx, name)
			if err != nil || !ok || sk.Source != skill.SourceBuiltIn {
				t.Fatalf("Skill(%s) = %v, %v, %v, want a built-in skill", name, sk.Source, ok, err)
			}
		}
		if err := core.UseSkill(skillPackage("site-diary")); err != nil {
			t.Fatalf("UseSkill: %v", err)
		}
		if err := core.UseSkill(skillPackage("site-diary")); err == nil {
			t.Fatal("UseSkill with a taken name succeeded")
		}
		if err := core.UseSkill(skill.Package{Content: "no front matter"}); err == nil {
			t.Fatal("UseSkill with an invalid package succeeded")
		}
		sk, ok, err := core.SkillHub().Get(ctx, "site-diary")
		if err != nil || !ok || sk.Source != skill.SourceApp {
			t.Fatalf("Skill(site-diary) = %v, %v, %v, want an app skill", sk.Source, ok, err)
		}
		if _, ok, err := core.SkillHub().Get(ctx, "nope"); err != nil || ok {
			t.Fatalf("Skill(nope) = %v, %v, want false and no error", ok, err)
		}
		all, err := core.SkillHub().List(ctx)
		if err != nil || len(all) != len(skillhub.EssentialSkillNames())+1 {
			t.Fatalf("Skills() = %d skills, %v, want %d essential plus the app skill", len(all), err, len(skillhub.EssentialSkillNames()))
		}
	})

	t.Run("UseSandboxProvider", func(t *testing.T) {
		core, err := aec.NewAgentCore()
		if err != nil {
			t.Fatal(err)
		}
		if err := core.UseSandboxProvider(aecmock.NewInMemoryProvider()); err != nil {
			t.Fatalf("UseSandboxProvider: %v", err)
		}
		if _, ok := core.SandboxHub().Provider(sandboxhub.KindDocker); !ok {
			t.Fatal("the provider is not registered on the sandbox hub")
		}
		if err := core.UseSandboxProvider(aecmock.NewInMemoryProvider()); !errors.Is(err, sandboxhub.ErrProviderAlreadyRegistered) {
			t.Fatalf("a second provider of a kind = %v, want sandboxhub.ErrProviderAlreadyRegistered", err)
		}
	})

	t.Run("UsePersistence", func(t *testing.T) {
		core, err := aec.NewAgentCore()
		if err != nil {
			t.Fatal(err)
		}
		if err := core.UsePersistence(nil); err == nil {
			t.Fatal("UsePersistence(nil) succeeded")
		}
		var typedNil *persistence.PersistenceHub
		if err := core.UsePersistence(typedNil); err == nil {
			t.Fatal("UsePersistence(typed nil) succeeded")
		}

		// A custom skill lives in the persistence skill store, so it disappears when
		// the store is swapped (the skill hub reloads).
		if _, err := core.SkillHub().Create(ctx, skillPackage("custom-one"), skillhub.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		if _, ok, _ := core.SkillHub().Get(ctx, "custom-one"); !ok {
			t.Fatal("the custom skill is not visible")
		}
		fresh := persistence.NewPersistenceHub()
		if err := core.UsePersistence(fresh); err != nil {
			t.Fatalf("UsePersistence: %v", err)
		}
		if core.PersistenceHub() != fresh {
			t.Fatal("Persistence() is not the plugged hub")
		}
		if _, ok, _ := core.SkillHub().Get(ctx, "custom-one"); ok {
			t.Fatal("the custom skill survived the swap of the skill store")
		}

		// An agent spawned after the swap saves its profile into the new hub.
		core2, _ := newCore(t, "x")
		if err := core2.UsePersistence(fresh); err != nil {
			t.Fatal(err)
		}
		if _, err := core2.SpawnAgent(ctx, testProfile("after-swap")); err != nil {
			t.Fatal(err)
		}
		if p, err := fresh.Agents.Load(ctx, "after-swap"); err != nil || p == nil {
			t.Fatalf("the profile was not saved into the new hub: %v, %v", p, err)
		}
	})

	t.Run("accessors", func(t *testing.T) {
		core, err := aec.NewAgentCore()
		if err != nil {
			t.Fatal(err)
		}
		if core.LLMHub() == nil || core.ToolHub() == nil || core.SkillHub() == nil || core.SandboxHub() == nil ||
			core.ArtifactHub() == nil || core.PersistenceHub() == nil || core.Events() == nil || core.Trace() == nil {
			t.Fatal("an accessor returned nil")
		}
	})
}

// A nil option and a nil value keep the defaults; a value given is kept.
func TestAgentCore_Options(t *testing.T) {
	hub := persistence.NewPersistenceHub()
	core, err := aec.NewAgentCore(aec.WithPersistence(hub), aec.WithPersistence(nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	if core.PersistenceHub() != hub {
		t.Fatal("WithPersistence was not applied (or a later nil replaced it)")
	}
}

// failingAgents is an agent store whose Save fails.
type failingAgents struct {
	persistence.AgentStore
	err error
}

func (f failingAgents) Save(context.Context, profile.AgentProfile) error { return f.err }

// When the profile cannot be saved the agent stays spawned and is returned with the
// error (CONTEXT "SpawnAgent").
func TestAgentCore_SpawnSaveFailureKeepsAgent(t *testing.T) {
	ctx := t.Context()
	boom := errors.New("store is down")
	hub := persistence.NewPersistenceHub(persistence.WithAgents(failingAgents{AgentStore: persistence.NewMemoryAgentStore(), err: boom}))
	core, _ := newCore(t, "x", aec.WithPersistence(hub))

	a, err := core.SpawnAgent(ctx, testProfile("a1"))
	if !errors.Is(err, boom) {
		t.Fatalf("SpawnAgent error = %v, want the save error", err)
	}
	if a == nil {
		t.Fatal("SpawnAgent returned no agent with the save error")
	}
	if got, ok := core.Agent("a1"); !ok || got != a {
		t.Fatalf("Agent(a1) = %v, %v, want the spawned agent to stay registered", got, ok)
	}
	// The agent works.
	if _, err := a.StartSession(ctx); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
}

// A duplicate id fails with the expected text and leaves the first agent and its stored
// profile.
func TestAgentCore_SpawnDuplicate(t *testing.T) {
	ctx := t.Context()
	core, _ := newCore(t, "x")
	first, err := core.SpawnAgent(ctx, testProfile("a1"))
	if err != nil {
		t.Fatal(err)
	}
	other := testProfile("a1")
	other.SystemPrompt = "second"
	a, err := core.SpawnAgent(ctx, other)
	if a != nil || err == nil || err.Error() != `Agent "a1" already exists` {
		t.Fatalf("second SpawnAgent = %v, %v, want no agent and the TS text", a, err)
	}
	if got, _ := core.Agent("a1"); got != first {
		t.Fatal("the first agent was replaced")
	}
	if p, _ := core.PersistenceHub().Agents.Load(ctx, "a1"); p == nil || p.SystemPrompt != "You are terse." {
		t.Fatalf("the stored profile = %+v, want the first one untouched", p)
	}
}

// Initialize spawns the stored profiles that are not live and saves nothing.
func TestAgentCore_InitializeSpawnsStored(t *testing.T) {
	ctx := t.Context()
	core, _ := newCore(t, "x")
	live, err := core.SpawnAgent(ctx, testProfile("live"))
	if err != nil {
		t.Fatal(err)
	}
	if err := core.PersistenceHub().Agents.Save(ctx, testProfile("stored")); err != nil {
		t.Fatal(err)
	}

	if err := core.Initialize(ctx); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	var ids []string
	for _, a := range core.ListAgents() {
		ids = append(ids, a.ID())
	}
	if !slices.Equal(ids, []string{"live", "stored"}) {
		t.Fatalf("agents after Initialize = %v, want [live stored]", ids)
	}
	if got, _ := core.Agent("live"); got != live {
		t.Fatal("Initialize replaced the live agent")
	}

	// A second Initialize finds everything live and changes nothing.
	if err := core.Initialize(ctx); err != nil {
		t.Fatalf("second Initialize: %v", err)
	}
	if n := len(core.ListAgents()); n != 2 {
		t.Fatalf("agents after a second Initialize = %d, want 2", n)
	}
}

// Dispose stops every agent, then the sandbox hub, then the persistence hub, and the
// agents leave the registry. The part that continues past a failing step and joins
// the errors is TestRunAll_ContinuesPastFailuresAndJoins (no real step can fail yet).
func TestAgentCore_DisposeOrderAndJoin(t *testing.T) {
	ctx := t.Context()
	core, _ := newCore(t, "x")
	provider := aecmock.NewInMemoryProvider()
	if err := core.UseSandboxProvider(provider); err != nil {
		t.Fatal(err)
	}
	if err := core.SandboxHub().ConfigureShared(sandboxhub.Spec{Kind: sandboxhub.KindDocker}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a1", "a2"} {
		if _, err := core.SpawnAgent(ctx, testProfile(id)); err != nil {
			t.Fatal(err)
		}
	}
	// Start the shared sandbox, so Dispose has something to stop.
	if _, err := core.SandboxHub().Resolve(ctx, "a1", sandboxhub.Selection{}); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	sb, ok := provider.Sandbox("docker:shared")
	if !ok || sb.Disposed() {
		t.Fatalf("the shared sandbox is missing or already disposed: %v, %v", sb, ok)
	}

	if err := core.Dispose(ctx); err != nil {
		t.Fatalf("Dispose: %v", err)
	}
	if n := len(core.ListAgents()); n != 0 {
		t.Fatalf("%d agents left after Dispose", n)
	}
	if !sb.Disposed() {
		t.Fatal("the shared sandbox was not stopped")
	}
	// Disposing again is harmless.
	if err := core.Dispose(ctx); err != nil {
		t.Fatalf("second Dispose: %v", err)
	}
}

// An agent keeps the persistence hub it was spawned with: UsePersistence reaches
// the agents spawned afterwards, not the live ones (the hub is passed to the
// ConversationManager when it is built).
func TestAgentCore_UsePersistenceKeepsLiveAgentSnapshot(t *testing.T) {
	ctx := t.Context()
	core, _ := newCore(t, "x")
	oldHub := core.PersistenceHub()
	live, err := core.SpawnAgent(ctx, testProfile("live"))
	if err != nil {
		t.Fatal(err)
	}
	newHub := persistence.NewPersistenceHub()
	if err := core.UsePersistence(newHub); err != nil {
		t.Fatal(err)
	}
	later, err := core.SpawnAgent(ctx, testProfile("later"))
	if err != nil {
		t.Fatal(err)
	}

	liveSession, err := live.StartSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	laterSession, err := later.StartSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if s, _ := oldHub.Sessions.Get(ctx, liveSession.ID); s == nil {
		t.Fatal("the live agent no longer writes to the hub it was spawned with")
	}
	if s, _ := newHub.Sessions.Get(ctx, liveSession.ID); s != nil {
		t.Fatal("the live agent writes to the new hub")
	}
	if s, _ := newHub.Sessions.Get(ctx, laterSession.ID); s == nil {
		t.Fatal("the agent spawned after the swap does not use the new hub")
	}
}

// The AgentCore.Dispose half of the agent-sandbox case "agent.dispose stops its own
// sandbox, and AgentCore.dispose stops the shared one": disposing an agent stops its own
// sandbox and not the shared one, and Dispose of the core stops the shared sandbox.
func TestAgentCore_DisposeStopsSharedSandbox(t *testing.T) {
	ctx := t.Context()
	core, err := aec.NewAgentCore()
	if err != nil {
		t.Fatal(err)
	}
	provider := aecmock.NewInMemoryProvider()
	if err := core.UseSandboxProvider(provider); err != nil {
		t.Fatal(err)
	}
	if err := core.SandboxHub().ConfigureShared(sandboxhub.Spec{Kind: sandboxhub.KindDocker}); err != nil {
		t.Fatal(err)
	}

	own := testProfile("first")
	own.Sandbox = sandboxhub.Selection{Mode: sandboxhub.SelectionOwn, Own: &sandboxhub.OwnConfig{}}
	first, err := core.SpawnAgent(ctx, own)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Sandbox(ctx); err != nil {
		t.Fatalf("first.Sandbox: %v", err)
	}
	if err := first.Dispose(ctx); err != nil {
		t.Fatalf("dispose first: %v", err)
	}
	ownSandbox, ok := provider.Sandbox("docker:own:first")
	if !ok || !ownSandbox.Disposed() {
		t.Fatal("disposing the agent did not stop its own sandbox")
	}

	second, err := core.SpawnAgent(ctx, testProfile("second"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := second.Sandbox(ctx); err != nil {
		t.Fatalf("second.Sandbox: %v", err)
	}
	shared, ok := provider.Sandbox("docker:shared")
	if !ok {
		t.Fatal("the shared sandbox was not created")
	}
	if shared.Disposed() {
		t.Fatal("disposing an agent stopped the shared sandbox")
	}

	if err := core.Dispose(context.WithoutCancel(ctx)); err != nil {
		t.Fatalf("core Dispose: %v", err)
	}
	if !shared.Disposed() {
		t.Fatal("AgentCore.Dispose did not stop the shared sandbox")
	}
	if n := len(core.ListAgents()); n != 0 {
		t.Fatalf("agents after Dispose = %d, want 0", n)
	}
}
