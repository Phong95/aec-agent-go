package persistence

import (
	"context"

	"github.com/Phong95/aec-agent-go/internal/nilcheck"
	"github.com/Phong95/aec-agent-go/kernel"
)

// ---------------------------------------------------------------------------
// Default persistence hub - in-memory, zero-config
// ---------------------------------------------------------------------------

// PersistenceHub is the persistence hub: the ten store slots of an
// agent runtime. NewPersistenceHub fills every slot a host did not provide with the
// in-memory default. It is a kernel.Plugin whose lifecycle hooks do nothing.
// (Update (A-70): eleven slots, with Triggers.)
// (Update (A-126): ten again - Jobs replaces ScheduledJobs and Triggers.)
// (Update (A-105): eleven, with SubagentRuns; twelve with SubagentTypes.)
// (Update (A-101): thirteen, with Secrets.)
//
// The two message slots are distinct instances: Messages is the full history and
// CompactedMessages is the compacted LLM working set.
//
// The store fields are exported so a host can build a hub as a literal, but they
// are set before the hub is handed over (to NewAgentCore through WithPersistence, or
// to AgentCore.UsePersistence) and are not changed afterwards. A slot left nil (or
// a typed nil) is not an error: FillDefaults, which NewPersistenceHub, NewAgentCore and
// UsePersistence all run, puts the in-memory default there, so a partially filled
// hub such as &PersistenceHub{Agents: pg} works, and a zero PersistenceHub handed over is all in memory.
type PersistenceHub struct {
	Agents            AgentStore
	Providers         ProviderStore
	Sessions          SessionStore
	Messages          MessageStore
	CompactedMessages MessageStore
	Memory            MemoryStore
	Artifacts         ArtifactStore
	// Skills holds custom skills created at runtime (built-in and app skills live
	// only in the SkillHub).
	Skills SkillStore
	// Channels holds connected chat-app channels (A-100); the live connections live in
	// each agent's channel manager.
	Channels ChannelStore
	// Triggers holds event triggers, their durable inbox and run queue (A-70); the worker
	// lives in each agent's trigger manager. The in-memory default keeps nothing across a
	// restart.
	// Update (A-126): Jobs holds jobs of every kind - schedule, event and webhook
	// (ScheduledJobs (A-69) and Triggers (A-70) merged) - with their inbox and run
	// history; the clock and worker live in each agent's job manager.
	Jobs JobStore
	// SubagentRuns holds one record per child run a parent agent started through
	// spawn_subagent (A-105) - what ran, how it ended and what the child returned. The
	// in-memory default keeps nothing across a restart.
	SubagentRuns SubagentRunStore
	// SubagentTypes holds the custom subagent types users created (A-105 revision;
	// built-in and app types live only in the subagenthub.SubagentHub).
	SubagentTypes SubagentTypeStore
	// Secrets holds secrets (A-101) - sealed values (envelope-encrypted by the
	// secrethub.SecretHub) and their metadata, global or owned by an agent. Write through
	// the SecretHub. The in-memory default keeps nothing across a restart.
	Secrets SecretStore
}

// Option sets one store slot of a PersistenceHub.
type Option func(*PersistenceHub)

// WithAgents sets the agent store.
func WithAgents(s AgentStore) Option { return func(h *PersistenceHub) { h.Agents = s } }

// WithProviders sets the provider store.
func WithProviders(s ProviderStore) Option { return func(h *PersistenceHub) { h.Providers = s } }

// WithSessions sets the session store.
func WithSessions(s SessionStore) Option { return func(h *PersistenceHub) { h.Sessions = s } }

// WithMessages sets the full message history store.
func WithMessages(s MessageStore) Option { return func(h *PersistenceHub) { h.Messages = s } }

// WithCompactedMessages sets the compacted message (LLM working set) store.
func WithCompactedMessages(s MessageStore) Option {
	return func(h *PersistenceHub) { h.CompactedMessages = s }
}

// WithMemory sets the memory record store.
func WithMemory(s MemoryStore) Option { return func(h *PersistenceHub) { h.Memory = s } }

// WithArtifacts sets the artifact store.
func WithArtifacts(s ArtifactStore) Option { return func(h *PersistenceHub) { h.Artifacts = s } }

// WithSkills sets the custom skill store.
func WithSkills(s SkillStore) Option { return func(h *PersistenceHub) { h.Skills = s } }

// WithChannels sets the chat-app channel store.
func WithChannels(s ChannelStore) Option { return func(h *PersistenceHub) { h.Channels = s } }

// WithJobs sets the job store (A-126; was WithScheduledJobs and WithTriggers).
func WithJobs(s JobStore) Option { return func(h *PersistenceHub) { h.Jobs = s } }

// WithSubagentRuns sets the subagent run store (A-105).
func WithSubagentRuns(s SubagentRunStore) Option {
	return func(h *PersistenceHub) { h.SubagentRuns = s }
}

// WithSubagentTypes sets the custom subagent type store (A-105 revision).
func WithSubagentTypes(s SubagentTypeStore) Option {
	return func(h *PersistenceHub) { h.SubagentTypes = s }
}

// WithSecrets sets the secret store (A-101).
func WithSecrets(s SecretStore) Option {
	return func(h *PersistenceHub) { h.Secrets = s }
}

// NewPersistenceHub returns a hub whose slots are the given stores. Every slot that is not
// given, or is given as nil or as a typed nil (an interface holding a nil
// pointer), gets the in-memory default.
func NewPersistenceHub(opts ...Option) *PersistenceHub {
	h := &PersistenceHub{}
	for _, o := range opts {
		if o != nil {
			o(h)
		}
	}
	return h.FillDefaults()
}

// FillDefaults puts the in-memory default store into every slot of h that is nil or
// a typed nil (an interface holding a nil pointer), and returns h. The slots that
// are set are kept. The two message slots get distinct instances. It changes h in
// place, so it runs before the hub is shared; a nil h returns nil.
func (h *PersistenceHub) FillDefaults() *PersistenceHub {
	if h == nil {
		return nil
	}
	if nilcheck.IsNil(h.Agents) {
		h.Agents = NewMemoryAgentStore()
	}
	if nilcheck.IsNil(h.Providers) {
		h.Providers = NewMemoryProviderStore()
	}
	if nilcheck.IsNil(h.Sessions) {
		h.Sessions = NewMemorySessionStore()
	}
	if nilcheck.IsNil(h.Messages) {
		h.Messages = NewMemoryMessageStore()
	}
	if nilcheck.IsNil(h.CompactedMessages) {
		h.CompactedMessages = NewMemoryMessageStore()
	}
	if nilcheck.IsNil(h.Memory) {
		h.Memory = NewMemoryMemoryStore()
	}
	if nilcheck.IsNil(h.Artifacts) {
		h.Artifacts = NewMemoryArtifactStore()
	}
	if nilcheck.IsNil(h.Skills) {
		h.Skills = NewMemorySkillStore()
	}
	if nilcheck.IsNil(h.Channels) {
		h.Channels = NewMemoryChannelStore()
	}
	if nilcheck.IsNil(h.Jobs) {
		h.Jobs = NewMemoryJobStore()
	}
	if nilcheck.IsNil(h.SubagentRuns) {
		h.SubagentRuns = NewMemorySubagentRunStore()
	}
	if nilcheck.IsNil(h.SubagentTypes) {
		h.SubagentTypes = NewMemorySubagentTypeStore()
	}
	if nilcheck.IsNil(h.Secrets) {
		h.Secrets = NewMemorySecretStore()
	}
	return h
}

var (
	_ kernel.Plugin      = (*PersistenceHub)(nil)
	_ kernel.Initializer = (*PersistenceHub)(nil)
	_ kernel.Disposer    = (*PersistenceHub)(nil)
)

// Name returns "persistence-hub".
func (*PersistenceHub) Name() string { return "persistence-hub" }

// Version returns "1.0.0".
func (*PersistenceHub) Version() string { return "1.0.0" }

// Initialize does nothing; the default stores need no setup.
func (*PersistenceHub) Initialize(context.Context) error { return nil }

// Dispose does nothing; the default stores hold no resources.
func (*PersistenceHub) Dispose(context.Context) error { return nil }
