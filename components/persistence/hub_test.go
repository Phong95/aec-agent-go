package persistence_test

import (
	"testing"

	"github.com/Phong95/aec-agent-go/components/persistence"
	"github.com/Phong95/aec-agent-go/kernel"
)

// NewPersistenceHub with no options fills all ten slots with working in-memory stores, and
// (Update (A-105): eleven, with SubagentRuns)
// (Update (A-101): and Secrets)
// Messages and CompactedMessages are two distinct instances.
func TestNewHubDefaults(t *testing.T) {
	h := persistence.NewPersistenceHub()
	slots := map[string]any{
		"Agents": h.Agents, "Providers": h.Providers, "Sessions": h.Sessions, "Messages": h.Messages,
		"CompactedMessages": h.CompactedMessages, "Memory": h.Memory, "Artifacts": h.Artifacts, "Skills": h.Skills,
		"Channels": h.Channels, "Jobs": h.Jobs, "SubagentRuns": h.SubagentRuns, "Secrets": h.Secrets,
	}
	for name, s := range slots {
		if s == nil {
			t.Errorf("slot %s is nil", name)
		}
	}
	if h.Messages == h.CompactedMessages {
		t.Fatal("Messages and CompactedMessages must be distinct instances")
	}
	ctx := t.Context()
	if err := h.Messages.Append(ctx, "s", kernel.Message{Role: kernel.RoleUser, Content: "x"}); err != nil {
		t.Fatal(err)
	}
	if n, _ := h.CompactedMessages.Count(ctx, "s"); n != 0 {
		t.Fatalf("CompactedMessages shares state with Messages: count %d", n)
	}
	if n, _ := h.Messages.Count(ctx, "s"); n != 1 {
		t.Fatalf("Messages lost the message: count %d", n)
	}
}

// Every With option wires its own slot and leaves the others at their defaults.
func TestNewHubCustomStore(t *testing.T) {
	custom := persistence.NewMemorySessionStore()
	h := persistence.NewPersistenceHub(persistence.WithSessions(custom))
	if h.Sessions != persistence.SessionStore(custom) {
		t.Fatal("WithSessions did not install the custom store")
	}
	if h.Agents == nil || h.Providers == nil || h.Messages == nil || h.CompactedMessages == nil ||
		h.Memory == nil || h.Artifacts == nil || h.Skills == nil || h.Channels == nil || h.Jobs == nil || h.SubagentRuns == nil {
		t.Fatal("the other ten slots must default")
	}

	agents := persistence.NewMemoryAgentStore()
	providers := persistence.NewMemoryProviderStore()
	messages := persistence.NewMemoryMessageStore()
	compacted := persistence.NewMemoryMessageStore()
	mem := persistence.NewMemoryMemoryStore()
	artifacts := persistence.NewMemoryArtifactStore()
	skills := persistence.NewMemorySkillStore()
	jobs := persistence.NewMemoryJobStore()
	channels := persistence.NewMemoryChannelStore()
	runs := persistence.NewMemorySubagentRunStore()
	all := persistence.NewPersistenceHub(
		persistence.WithAgents(agents), persistence.WithProviders(providers), persistence.WithSessions(custom),
		persistence.WithMessages(messages), persistence.WithCompactedMessages(compacted), persistence.WithMemory(mem),
		persistence.WithArtifacts(artifacts), persistence.WithSkills(skills), persistence.WithJobs(jobs), persistence.WithChannels(channels), persistence.WithSubagentRuns(runs), nil, // a nil option is ignored
	)
	switch {
	case all.Agents != persistence.AgentStore(agents), all.Providers != persistence.ProviderStore(providers),
		all.Sessions != persistence.SessionStore(custom), all.Messages != persistence.MessageStore(messages),
		all.CompactedMessages != persistence.MessageStore(compacted), all.Memory != persistence.MemoryStore(mem),
		all.Artifacts != persistence.ArtifactStore(artifacts), all.Skills != persistence.SkillStore(skills),
		all.Jobs != persistence.JobStore(jobs), all.Channels != persistence.ChannelStore(channels),
		all.SubagentRuns != persistence.SubagentRunStore(runs):
		t.Fatal("a With option installed the wrong store")
	}
}

// A nil interface or a typed-nil pointer option counts as absent, so the slot
// gets the default instead of a store that would panic on first use.
func TestNewHubTypedNilFallsBack(t *testing.T) {
	h := persistence.NewPersistenceHub(
		persistence.WithAgents((*persistence.MemoryAgentStore)(nil)),
		persistence.WithProviders((*persistence.MemoryProviderStore)(nil)),
		persistence.WithSessions((*persistence.MemorySessionStore)(nil)),
		persistence.WithMessages((*persistence.MemoryMessageStore)(nil)),
		persistence.WithCompactedMessages(nil),
		persistence.WithMemory((*persistence.MemoryMemoryStore)(nil)),
		persistence.WithArtifacts((*persistence.MemoryArtifactStore)(nil)),
		persistence.WithSkills((*persistence.MemorySkillStore)(nil)),
		persistence.WithJobs((*persistence.MemoryJobStore)(nil)),
		persistence.WithChannels((*persistence.MemoryChannelStore)(nil)),
		persistence.WithSubagentRuns((*persistence.MemorySubagentRunStore)(nil)),
	)
	ctx := t.Context()
	// Each default must work, which a typed nil would not.
	if _, err := h.Agents.List(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Providers.List(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Sessions.List(ctx, kernel.SessionFilter{}); err != nil {
		t.Fatal(err)
	}
	if err := h.Messages.Append(ctx, "s", kernel.Message{Role: kernel.RoleUser}); err != nil {
		t.Fatal(err)
	}
	if err := h.CompactedMessages.Append(ctx, "s", kernel.Message{Role: kernel.RoleUser}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Memory.Query(ctx, persistence.MemoryFilter{}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Artifacts.Query(ctx, persistence.ArtifactFilter{}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Skills.List(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Jobs.ListByAgent(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Channels.ListByAgent(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.SubagentRuns.ListByParentSession(ctx, "s"); err != nil {
		t.Fatal(err)
	}
	if h.Messages == h.CompactedMessages {
		t.Fatal("the two message defaults must be distinct instances")
	}
}

// The hub is a Plugin named persistence-hub at 1.0.0 whose lifecycle hooks do
// nothing.
func TestHubIdentity(t *testing.T) {
	h := persistence.NewPersistenceHub()
	if h.Name() != "persistence-hub" || h.Version() != "1.0.0" {
		t.Fatalf("identity = %q %q", h.Name(), h.Version())
	}
	ctx := t.Context()
	if err := kernel.Initialize(ctx, h); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := kernel.Dispose(ctx, h); err != nil {
		t.Fatalf("Dispose: %v", err)
	}
	var p kernel.Plugin = h
	if p.Name() != "persistence-hub" {
		t.Fatal("the hub is not a kernel.Plugin")
	}
}

// FillDefaults fills only the empty slots of a literal hub, in place, and keeps the
// ones that are set; a zero PersistenceHub becomes an all-in-memory hub and a nil hub stays nil.
func TestHubFillDefaults(t *testing.T) {
	agents := persistence.NewMemoryAgentStore()
	h := &persistence.PersistenceHub{Agents: agents, Skills: (*persistence.MemorySkillStore)(nil)}
	if got := h.FillDefaults(); got != h {
		t.Fatal("FillDefaults must return the hub it filled")
	}
	if h.Agents != persistence.AgentStore(agents) {
		t.Fatal("FillDefaults replaced a slot that was set")
	}
	if h.Providers == nil || h.Sessions == nil || h.Messages == nil || h.CompactedMessages == nil ||
		h.Memory == nil || h.Artifacts == nil || h.Skills == nil || h.Jobs == nil || h.SubagentRuns == nil {
		t.Fatal("FillDefaults left an empty slot")
	}
	if h.Messages == h.CompactedMessages {
		t.Fatal("the two message defaults must be distinct instances")
	}
	if _, err := h.Skills.List(t.Context()); err != nil {
		t.Fatalf("the typed-nil Skills slot did not get a working default: %v", err)
	}
	var zero persistence.PersistenceHub
	zero.FillDefaults()
	if _, err := zero.Agents.List(t.Context()); err != nil {
		t.Fatal(err)
	}
	var none *persistence.PersistenceHub
	if none.FillDefaults() != nil {
		t.Fatal("FillDefaults on a nil hub must return nil")
	}
}
