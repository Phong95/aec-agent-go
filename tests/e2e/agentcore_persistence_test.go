package e2e_test

import (
	"context"
	"sync"
	"testing"

	aec "github.com/Phong95/aec-agent-go"
	"github.com/Phong95/aec-agent-go/aecmock"
	"github.com/Phong95/aec-agent-go/components/agent/memorysystem/memory"
	"github.com/Phong95/aec-agent-go/components/agent/profile"
	"github.com/Phong95/aec-agent-go/components/artifacthub/artifact"
	"github.com/Phong95/aec-agent-go/components/persistence"
	"github.com/Phong95/aec-agent-go/components/skillhub"
	"github.com/Phong95/aec-agent-go/components/skillhub/skill"
	"github.com/Phong95/aec-agent-go/kernel"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// tally counts the calls a counting store wrapper saw, by method name.
type tally struct {
	mu    sync.Mutex
	calls map[string]int
}

func newTally() *tally { return &tally{calls: map[string]int{}} }

func (t *tally) hit(method string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.calls[method]++
}

func (t *tally) count(method string) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.calls[method]
}

// The counting wrappers are spies on the stores: each one embeds
// a real in-memory store, counts every method in its tally and delegates, so a test can
// prove that the component that owns a store really calls the plugged one.

type countAgents struct {
	persistence.AgentStore
	t *tally
}

func (c *countAgents) Save(ctx context.Context, p profile.AgentProfile) error {
	c.t.hit("Save")
	return c.AgentStore.Save(ctx, p)
}

func (c *countAgents) Load(ctx context.Context, id string) (*profile.AgentProfile, error) {
	c.t.hit("Load")
	return c.AgentStore.Load(ctx, id)
}

func (c *countAgents) List(ctx context.Context) ([]profile.AgentProfile, error) {
	c.t.hit("List")
	return c.AgentStore.List(ctx)
}

func (c *countAgents) Delete(ctx context.Context, id string) error {
	c.t.hit("Delete")
	return c.AgentStore.Delete(ctx, id)
}

type countProviders struct {
	persistence.ProviderStore
	t *tally
}

func (c *countProviders) Save(ctx context.Context, p persistence.ProviderRecord) error {
	c.t.hit("Save")
	return c.ProviderStore.Save(ctx, p)
}

func (c *countProviders) Load(ctx context.Context, name string) (*persistence.ProviderRecord, error) {
	c.t.hit("Load")
	return c.ProviderStore.Load(ctx, name)
}

func (c *countProviders) List(ctx context.Context) ([]persistence.ProviderRecord, error) {
	c.t.hit("List")
	return c.ProviderStore.List(ctx)
}

func (c *countProviders) Delete(ctx context.Context, name string) error {
	c.t.hit("Delete")
	return c.ProviderStore.Delete(ctx, name)
}

type countSessions struct {
	persistence.SessionStore
	t *tally
}

func (c *countSessions) Create(ctx context.Context, md ordered.Object) (kernel.Session, error) {
	c.t.hit("Create")
	return c.SessionStore.Create(ctx, md)
}

func (c *countSessions) Get(ctx context.Context, id string) (*kernel.Session, error) {
	c.t.hit("Get")
	return c.SessionStore.Get(ctx, id)
}

func (c *countSessions) Update(ctx context.Context, s kernel.Session) error {
	c.t.hit("Update")
	return c.SessionStore.Update(ctx, s)
}

func (c *countSessions) SetConsolidatedMessageCount(ctx context.Context, id string, n int) error {
	c.t.hit("SetConsolidatedMessageCount")
	return c.SessionStore.SetConsolidatedMessageCount(ctx, id, n)
}

func (c *countSessions) Delete(ctx context.Context, id string) error {
	c.t.hit("Delete")
	return c.SessionStore.Delete(ctx, id)
}

func (c *countSessions) List(ctx context.Context, f kernel.SessionFilter) ([]kernel.Session, error) {
	c.t.hit("List")
	return c.SessionStore.List(ctx, f)
}

type countMessages struct {
	persistence.MessageStore
	t *tally
}

func (c *countMessages) Get(ctx context.Context, id string) ([]kernel.Message, error) {
	c.t.hit("Get")
	return c.MessageStore.Get(ctx, id)
}

func (c *countMessages) GetRange(ctx context.Context, id string, before, limit int) (persistence.MessageSlice, error) {
	c.t.hit("GetRange")
	return c.MessageStore.GetRange(ctx, id, before, limit)
}

func (c *countMessages) Set(ctx context.Context, id string, msgs []kernel.Message) error {
	c.t.hit("Set")
	return c.MessageStore.Set(ctx, id, msgs)
}

func (c *countMessages) Append(ctx context.Context, id string, m kernel.Message) error {
	c.t.hit("Append")
	return c.MessageStore.Append(ctx, id, m)
}

func (c *countMessages) Clear(ctx context.Context, id string) error {
	c.t.hit("Clear")
	return c.MessageStore.Clear(ctx, id)
}

func (c *countMessages) Count(ctx context.Context, id string) (int, error) {
	c.t.hit("Count")
	return c.MessageStore.Count(ctx, id)
}

type countMemory struct {
	persistence.MemoryStore
	t *tally
}

func (c *countMemory) Save(ctx context.Context, r memory.Record) error {
	c.t.hit("Save")
	return c.MemoryStore.Save(ctx, r)
}

func (c *countMemory) Get(ctx context.Context, id string) (*memory.Record, error) {
	c.t.hit("Get")
	return c.MemoryStore.Get(ctx, id)
}

func (c *countMemory) Update(ctx context.Context, r memory.Record) error {
	c.t.hit("Update")
	return c.MemoryStore.Update(ctx, r)
}

func (c *countMemory) Delete(ctx context.Context, id string) error {
	c.t.hit("Delete")
	return c.MemoryStore.Delete(ctx, id)
}

func (c *countMemory) Query(ctx context.Context, f persistence.MemoryFilter) ([]memory.Record, error) {
	c.t.hit("Query")
	return c.MemoryStore.Query(ctx, f)
}

func (c *countMemory) FindByKey(ctx context.Context, agentID string, scope memory.Scope, key string) (*memory.Record, error) {
	c.t.hit("FindByKey")
	return c.MemoryStore.FindByKey(ctx, agentID, scope, key)
}

type countArtifacts struct {
	persistence.ArtifactStore
	t *tally
}

func (c *countArtifacts) Create(ctx context.Context, a artifact.Artifact, v artifact.Version) error {
	c.t.hit("Create")
	return c.ArtifactStore.Create(ctx, a, v)
}

func (c *countArtifacts) Get(ctx context.Context, id string) (*artifact.Artifact, error) {
	c.t.hit("Get")
	return c.ArtifactStore.Get(ctx, id)
}

func (c *countArtifacts) FindByName(ctx context.Context, agentID, name string) (*artifact.Artifact, error) {
	c.t.hit("FindByName")
	return c.ArtifactStore.FindByName(ctx, agentID, name)
}

func (c *countArtifacts) AddVersion(ctx context.Context, a artifact.Artifact, v artifact.Version) error {
	c.t.hit("AddVersion")
	return c.ArtifactStore.AddVersion(ctx, a, v)
}

func (c *countArtifacts) ListVersions(ctx context.Context, id string) ([]artifact.Version, error) {
	c.t.hit("ListVersions")
	return c.ArtifactStore.ListVersions(ctx, id)
}

func (c *countArtifacts) Query(ctx context.Context, f persistence.ArtifactFilter) ([]artifact.Artifact, error) {
	c.t.hit("Query")
	return c.ArtifactStore.Query(ctx, f)
}

func (c *countArtifacts) Delete(ctx context.Context, id string) error {
	c.t.hit("Delete")
	return c.ArtifactStore.Delete(ctx, id)
}

func (c *countArtifacts) DeleteByAgent(ctx context.Context, agentID string) error {
	c.t.hit("DeleteByAgent")
	return c.ArtifactStore.DeleteByAgent(ctx, agentID)
}

type countSkills struct {
	persistence.SkillStore
	t *tally
}

func (c *countSkills) Save(ctx context.Context, r persistence.SkillRecord) error {
	c.t.hit("Save")
	return c.SkillStore.Save(ctx, r)
}

func (c *countSkills) Load(ctx context.Context, name string) (*persistence.SkillRecord, error) {
	c.t.hit("Load")
	return c.SkillStore.Load(ctx, name)
}

func (c *countSkills) List(ctx context.Context) ([]persistence.SkillRecord, error) {
	c.t.hit("List")
	return c.SkillStore.List(ctx)
}

func (c *countSkills) Delete(ctx context.Context, name string) error {
	c.t.hit("Delete")
	return c.SkillStore.Delete(ctx, name)
}

// countedHub is a persistence hub of counting wrappers over in-memory stores: the 8
// slots of PER-01, each with its own tally.
type countedHub struct {
	hub                                              *persistence.PersistenceHub
	agents, providers, sessions, messages, compacted *tally
	memory, artifacts, skills                        *tally
}

func newCountedHub() *countedHub {
	c := &countedHub{
		agents: newTally(), providers: newTally(), sessions: newTally(), messages: newTally(),
		compacted: newTally(), memory: newTally(), artifacts: newTally(), skills: newTally(),
	}
	agents := &countAgents{persistence.NewMemoryAgentStore(), c.agents}
	providers := &countProviders{persistence.NewMemoryProviderStore(), c.providers}
	sessions := &countSessions{persistence.NewMemorySessionStore(), c.sessions}
	messages := &countMessages{persistence.NewMemoryMessageStore(), c.messages}
	compacted := &countMessages{persistence.NewMemoryMessageStore(), c.compacted}
	mem := &countMemory{persistence.NewMemoryMemoryStore(), c.memory}
	arts := &countArtifacts{persistence.NewMemoryArtifactStore(), c.artifacts}
	skills := &countSkills{persistence.NewMemorySkillStore(), c.skills}
	c.hub = persistence.NewPersistenceHub(
		persistence.WithAgents(agents),
		persistence.WithProviders(providers),
		persistence.WithSessions(sessions),
		persistence.WithMessages(messages),
		persistence.WithCompactedMessages(compacted),
		persistence.WithMemory(mem),
		persistence.WithArtifacts(arts),
		persistence.WithSkills(skills),
	)
	return c
}

// requireHit fails the test when the tally saw no call of the method.
func requireHit(t *testing.T, store string, tl *tally, methods ...string) {
	t.Helper()
	for _, m := range methods {
		if tl.count(m) == 0 {
			t.Errorf("%s store: %s was never called, the component that owns the store does not use the plugged one", store, m)
		}
	}
}

// memoryProfile is a profile with the memory system on, so a turn recalls from the
// memory store.
func memoryProfile(id string) profile.AgentProfile {
	p := testProfile(id)
	p.Memory = memory.Config{Enabled: true}
	return p
}

// PER-01 (ROADMAP success criterion 1): a persistence hub that covers all 8 store
// interfaces, every one custom, plugs into the AgentCore, and every store is used by the
// component that owns it: agents by SpawnAgent and Initialize, sessions and the message
// stores by the conversation (the compacted one when a session is loaded), memory by
// the turn's recall, artifacts by the artifact hub, skills by the skill hub. Providers
// is the one slot no SDK component writes (the server does): it must be the plugged
// wrapper and reachable.
func TestAgentCore_PersistenceHubAllEightCustom(t *testing.T) {
	ctx := t.Context()
	c := newCountedHub()
	core, err := aec.NewAgentCore(aec.WithPersistence(c.hub))
	if err != nil {
		t.Fatalf("NewAgentCore: %v", err)
	}
	if err := core.UseLLMProvider(aecmock.NewMockProvider("mock", func([]kernel.Message) string { return "hello" })); err != nil {
		t.Fatal(err)
	}

	// Initialize reads the stored agents.
	if err := core.Initialize(ctx); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	// SpawnAgent saves the profile.
	a, err := core.SpawnAgent(ctx, memoryProfile("a1"))
	if err != nil {
		t.Fatalf("SpawnAgent: %v", err)
	}
	// A turn: session create and update, user and assistant messages, the recall.
	session, err := a.StartSession(ctx)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if _, err := a.Chat(ctx, "Hello"); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	// Loading the session reads the history and the compacted working set.
	if _, ok, err := a.LoadSession(ctx, session.ID); err != nil || !ok {
		t.Fatalf("LoadSession = %v, %v", ok, err)
	}
	// A skill create goes to the skill store.
	if _, err := core.SkillHub().Create(ctx, skill.Package{Content: skillMD("site-diary")}, skillhub.CreateOptions{CreatedBy: "a1"}); err != nil {
		t.Fatalf("skill create: %v", err)
	}
	// An artifact write goes to the artifact store.
	view, err := core.ArtifactHub().ForAgent("a1", session.ID)
	if err != nil {
		t.Fatalf("ForAgent: %v", err)
	}
	sb, err := aecmock.NewInMemoryView("docker:shared", "a1")
	if err != nil {
		t.Fatal(err)
	}
	content := "# notes"
	if _, err := view.Save(ctx, artifact.SaveInput{Name: "notes.md", Type: "text/markdown", Content: &content}, sb); err != nil {
		t.Fatalf("artifact save: %v", err)
	}
	// Providers is reachable through the core, and is the plugged wrapper.
	if got := core.PersistenceHub(); got != c.hub {
		t.Fatalf("core.PersistenceHub() is not the plugged hub")
	}
	if _, ok := core.PersistenceHub().Providers.(*countProviders); !ok {
		t.Fatalf("core.PersistenceHub().Providers is a %T, want the plugged counting wrapper", core.PersistenceHub().Providers)
	}
	if _, err := core.PersistenceHub().Providers.List(ctx); err != nil {
		t.Fatalf("Providers.List: %v", err)
	}

	requireHit(t, "Agents", c.agents, "Save", "List")
	requireHit(t, "Sessions", c.sessions, "Create", "Update", "Get")
	requireHit(t, "Messages", c.messages, "Append", "Get")
	requireHit(t, "CompactedMessages", c.compacted, "Get")
	requireHit(t, "Memory", c.memory, "Query")
	requireHit(t, "Artifacts", c.artifacts, "Create")
	requireHit(t, "Skills", c.skills, "Save")
	requireHit(t, "Providers", c.providers, "List")

	// Every hub accessor of the core is non-nil.
	hubs := map[string]bool{
		"Events": core.Events() != nil, "LLM": core.LLMHub() != nil, "ToolHub": core.ToolHub() != nil,
		"SkillHub": core.SkillHub() != nil, "Sandboxes": core.SandboxHub() != nil,
		"Artifacts": core.ArtifactHub() != nil, "Trace": core.Trace() != nil,
		"Persistence": core.PersistenceHub() != nil,
	}
	for name, ok := range hubs {
		if !ok {
			t.Errorf("accessor %s returned nil", name)
		}
	}

	// The data really landed in the plugged stores (not just was called).
	if stored, err := c.hub.Agents.Load(ctx, "a1"); err != nil || stored == nil {
		t.Errorf("Agents.Load(a1) = %v, %v, want the spawned profile", stored, err)
	}
	if msgs, err := c.hub.Messages.Get(ctx, session.ID); err != nil || len(msgs) != 2 {
		t.Errorf("Messages.Get = %d messages, %v, want the user message and the reply", len(msgs), err)
	}
	if rec, err := c.hub.Skills.Load(ctx, "site-diary"); err != nil || rec == nil {
		t.Errorf("Skills.Load(site-diary) = %v, %v, want the created skill", rec, err)
	}
	if arts, err := c.hub.Artifacts.Query(ctx, persistence.ArtifactFilter{AgentID: "a1"}); err != nil || len(arts) != 1 {
		t.Errorf("Artifacts.Query = %d artifacts, %v, want 1", len(arts), err)
	}
}

// PER-01, mixed: a hub with custom Sessions and Messages and nothing else gets the
// in-memory default in the other six slots, and a chat round-trips through the custom
// ones.
func TestAgentCore_PersistenceHubMixedWithDefaults(t *testing.T) {
	ctx := t.Context()
	sessions, messages := newTally(), newTally()
	hub := persistence.NewPersistenceHub(
		persistence.WithSessions(&countSessions{persistence.NewMemorySessionStore(), sessions}),
		persistence.WithMessages(&countMessages{persistence.NewMemoryMessageStore(), messages}),
	)
	// Every slot is filled: the defaults arrived for the six that were nil.
	for name, ok := range map[string]bool{
		"Agents": hub.Agents != nil, "Providers": hub.Providers != nil, "CompactedMessages": hub.CompactedMessages != nil,
		"Memory": hub.Memory != nil, "Artifacts": hub.Artifacts != nil, "Skills": hub.Skills != nil,
	} {
		if !ok {
			t.Errorf("the %s slot of a mixed hub is nil, want the in-memory default", name)
		}
	}

	core, err := aec.NewAgentCore(aec.WithPersistence(hub))
	if err != nil {
		t.Fatal(err)
	}
	if err := core.UseLLMProvider(aecmock.NewMockProvider("mock", func([]kernel.Message) string { return "pong" })); err != nil {
		t.Fatal(err)
	}
	a, err := core.SpawnAgent(ctx, testProfile("a1"))
	if err != nil {
		t.Fatalf("SpawnAgent (the default agent store): %v", err)
	}
	session, err := a.StartSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	res, err := a.Chat(ctx, "ping")
	if err != nil || res.Message.Content != "pong" {
		t.Fatalf("Chat = %q, %v, want pong", res.Message.Content, err)
	}
	requireHit(t, "Sessions", sessions, "Create", "Update")
	requireHit(t, "Messages", messages, "Append")
	msgs, err := hub.Messages.Get(ctx, session.ID)
	if err != nil || len(msgs) != 2 || msgs[0].Content != "ping" || msgs[1].Content != "pong" {
		t.Fatalf("custom Messages hold %v, %v, want the ping and pong round trip", msgs, err)
	}
	// The defaults work: the profile went to the default agent store.
	if stored, err := hub.Agents.Load(ctx, "a1"); err != nil || stored == nil {
		t.Fatalf("default Agents.Load(a1) = %v, %v", stored, err)
	}
	// And the same chat can be read back through the session API.
	if _, ok, err := a.LoadSession(ctx, session.ID); err != nil || !ok {
		t.Fatalf("LoadSession = %v, %v", ok, err)
	}
	if got := a.Profile().ID; got != "a1" {
		t.Fatalf("Profile().ID = %q", got)
	}
}

// UsePersistence after a spawn (the Agent and its MemorySystem capture the persistence
// hub at construction; the skill and artifact hubs read it through thunks, and the allowlist
// save reads it at call time): a live agent keeps writing its conversation and its
// memory into the hub captured at spawn, while the skill hub, the artifact hub, the
// allowlist save and every agent spawned afterwards use the new hub.
func TestAgentCore_UsePersistenceDoesNotReachLiveAgents(t *testing.T) {
	ctx := t.Context()
	oldHub, newHub := newCountedHub(), newCountedHub()
	core, err := aec.NewAgentCore(aec.WithPersistence(oldHub.hub))
	if err != nil {
		t.Fatal(err)
	}
	if err := core.UseLLMProvider(aecmock.NewMockProvider("mock", func([]kernel.Message) string { return "ok" })); err != nil {
		t.Fatal(err)
	}
	live, err := core.SpawnAgent(ctx, memoryProfile("live"))
	if err != nil {
		t.Fatal(err)
	}
	if err := core.UsePersistence(newHub.hub); err != nil {
		t.Fatalf("UsePersistence: %v", err)
	}
	later, err := core.SpawnAgent(ctx, memoryProfile("later"))
	if err != nil {
		t.Fatal(err)
	}

	// The live agent: conversation and memory recall stay on the old hub.
	liveSession, err := live.StartSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := live.Chat(ctx, "hi"); err != nil {
		t.Fatal(err)
	}
	requireHit(t, "old Sessions", oldHub.sessions, "Create", "Update")
	requireHit(t, "old Messages", oldHub.messages, "Append")
	requireHit(t, "old Memory", oldHub.memory, "Query")
	if n := newHub.sessions.count("Create") + newHub.messages.count("Append") + newHub.memory.count("Query"); n != 0 {
		t.Errorf("the live agent reached the new hub: %d conversation or memory calls", n)
	}
	if msgs, _ := oldHub.hub.Messages.Get(ctx, liveSession.ID); len(msgs) != 2 {
		t.Errorf("old Messages hold %d messages of the live session, want 2", len(msgs))
	}

	// The agent spawned afterwards uses the new hub.
	laterSession, err := later.StartSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := later.Chat(ctx, "hi"); err != nil {
		t.Fatal(err)
	}
	if msgs, _ := newHub.hub.Messages.Get(ctx, laterSession.ID); len(msgs) != 2 {
		t.Errorf("new Messages hold %d messages of the later session, want 2", len(msgs))
	}
	if saved, _ := newHub.hub.Agents.Load(ctx, "later"); saved == nil {
		t.Error("the profile of the agent spawned after the swap was not saved to the new hub")
	}

	// The skill hub reads the new hub's skill store (it reloaded on UsePersistence).
	if _, err := core.SkillHub().Create(ctx, skill.Package{Content: skillMD("site-diary")}, skillhub.CreateOptions{CreatedBy: "live"}); err != nil {
		t.Fatalf("skill create: %v", err)
	}
	requireHit(t, "new Skills", newHub.skills, "Save")
	if n := oldHub.skills.count("Save"); n != 0 {
		t.Errorf("the skill hub still writes to the old hub: %d saves", n)
	}

	// The artifact hub reads the new hub's artifact store, also for a view handed out
	// before the swap would (the store is resolved on every call).
	view, err := core.ArtifactHub().ForAgent("live", "")
	if err != nil {
		t.Fatal(err)
	}
	sb, err := aecmock.NewInMemoryView("docker:shared", "live")
	if err != nil {
		t.Fatal(err)
	}
	content := "x"
	if _, err := view.Save(ctx, artifact.SaveInput{Name: "n.md", Type: "text/markdown", Content: &content}, sb); err != nil {
		t.Fatalf("artifact save: %v", err)
	}
	requireHit(t, "new Artifacts", newHub.artifacts, "Create")
	if n := oldHub.artifacts.count("Create"); n != 0 {
		t.Errorf("the artifact hub still writes to the old hub: %d creates", n)
	}
}

// A literal hub with empty slots is not a nil-store trap: NewAgentCore (through
// WithPersistence) and UsePersistence fill each empty slot with the in-memory default,
// in place, and keep the slots that are set.
func TestAgentCore_PartialHubGetsDefaults(t *testing.T) {
	ctx := t.Context()
	core, err := aec.NewAgentCore(aec.WithPersistence(&persistence.PersistenceHub{}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := core.SpawnAgent(ctx, testProfile("a1")); err != nil {
		t.Fatalf("SpawnAgent over a zero hub: %v", err)
	}
	if stored, err := core.PersistenceHub().Agents.Load(ctx, "a1"); err != nil || stored == nil {
		t.Fatalf("default Agents.Load(a1) = %v, %v", stored, err)
	}

	agents := persistence.NewMemoryAgentStore()
	partial := &persistence.PersistenceHub{Agents: agents}
	if err := core.UsePersistence(partial); err != nil {
		t.Fatal(err)
	}
	if core.PersistenceHub() != partial || partial.Agents != persistence.AgentStore(agents) {
		t.Fatal("UsePersistence must keep the hub and the slots that were set")
	}
	if partial.Skills == nil || partial.Sessions == nil || partial.Artifacts == nil {
		t.Fatal("UsePersistence left an empty slot of the plugged hub")
	}
	if _, err := core.SkillHub().List(ctx); err != nil {
		t.Fatalf("Skills over the default skill store: %v", err)
	}
}
