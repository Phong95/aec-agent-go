package aec

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/Phong95/aec-agent-go/components/agent"
	"github.com/Phong95/aec-agent-go/components/agent/command"
	"github.com/Phong95/aec-agent-go/components/agent/job"
	"github.com/Phong95/aec-agent-go/components/agent/profile"
	"github.com/Phong95/aec-agent-go/components/agent/subagent"
	"github.com/Phong95/aec-agent-go/components/artifacthub"
	"github.com/Phong95/aec-agent-go/components/channelplatformhub"
	"github.com/Phong95/aec-agent-go/components/channelplatformhub/channel"
	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/components/grouphub"
	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/components/orchestration"
	"github.com/Phong95/aec-agent-go/components/persistence"
	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/components/secrethub"
	"github.com/Phong95/aec-agent-go/components/skillhub"
	"github.com/Phong95/aec-agent-go/components/skillhub/skill"
	"github.com/Phong95/aec-agent-go/components/subagenthub"
	"github.com/Phong95/aec-agent-go/components/subagenthub/subagenttype"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/components/trace"
	"github.com/Phong95/aec-agent-go/internal/nilcheck"
	"github.com/Phong95/aec-agent-go/internal/redact"
	"github.com/Phong95/aec-agent-go/kernel"
)

// AgentCore is the centralized AI agent service: the one place that
// sees every hub. A host adds tools, skills and LLM providers, picks the stores (a
// persistence hub backed by Postgres, Redis, and so on), and spawns the agents of
// one project. Project A may use Bedrock and Postgres while project B uses another
// provider and Redis: each builds its own AgentCore.
//
// Build one with NewAgentCore. The hubs are reachable through accessor methods
// (LLM, ToolHub, SkillHub, Sandboxes, Artifacts, Persistence, Events and Trace),
// ToolHub and SkillHub carry a Hub suffix because Tools and Skills are the AgentCore
// methods that list the registered tools and skills (formerly the Kernel interface
// methods). The orchestration is not handed out: Agent and ListAgents are a read-only view of the live agents, and
// an agent is spawned with SpawnAgent and removed with RemoveAgent, so no caller can
// spawn around the profile save.
//
// Persistence capture: an agent keeps the persistence hub that was current when it
// was spawned for its conversation (the hub is passed to its ConversationManager
// when it is built), while the skill hub and the artifact hub read
// the current hub through a function on every call. UsePersistence therefore
// reaches the two hubs at once and the agents spawned afterwards, but not the
// agents that are already live. A host that plugs a hub must not mutate its
// exported store fields afterwards. A slot the host left nil gets the in-memory
// default when the hub is handed over (persistence.PersistenceHub.FillDefaults).
//
// An AgentCore is safe for concurrent use; the persistence hub pointer is guarded
// by a mutex.
type AgentCore struct {
	events events.Bus
	llm    *llmhub.LLMHub
	tools  *toolhub.ToolHub
	skills *skillhub.SkillHub
	// subagentTypes is the global library of subagent types (A-105 revision): built-in
	// types, app types (UseSubagentType) and custom ones users create
	// (PersistenceHub.SubagentTypes). Agents enable types by name.
	subagentTypes *subagenthub.SubagentHub
	// secrets is the global hub for secrets (A-101) - API keys and tokens agents use
	// without the model seeing them. It seals values with envelope encryption under the SecretHub
	// provider (UseSecretHubProvider) into PersistenceHub.Secrets; hosts also seal their own
	// credential columns with it.
	secrets   *secrethub.SecretHub
	agents    *orchestration.Orchestration
	trace     *trace.Service
	sandboxes *sandboxhub.SandboxHub
	artifacts *artifacthub.ArtifactHub
	logger    *slog.Logger

	mu      sync.RWMutex
	persist *persistence.PersistenceHub
	// jobRunner runs the prompt-job turns of every agent (A-69); nil means each agent's
	// default runner. jobsStarted is set by StartScheduledJobs: every agent spawned
	// afterwards starts its prompt jobs too. Both are guarded by mu.
	// Update (A-126): jobRunner runs every job run (schedule, event, webhook) - the trigger
	// runner merged in - and jobsStarted is set by StartJobs.
	jobRunner   job.Runner
	jobsStarted bool
	// channels is the channel platform hub: the chat platforms agents' channels connect through
	// (A-100); it has its own lock. channelRunner runs the channel turns of every agent (nil means
	// each agent's Chat) and channelsStarted is set by StartChannels; both are guarded by mu.
	channels        *channelplatformhub.ChannelPlatformHub
	channelRunner   channel.TurnRunner
	channelsStarted bool
	// subagentRunner runs the child turns of every agent (A-105; nil means the child
	// agent's own Chat); guarded by mu.
	subagentRunner subagent.Runner
	// groups is group chats (A-106) - several agents in one conversation: the groups, their
	// sessions and threads, and the group turns (a lead routes the work, everyone answers,
	// or only the members the user addresses). Stored in PersistenceHub.Groups and
	// GroupMessages; its spawn scopes are every agent's Deps.SubagentScopes.
	groups *grouphub.Hub
	// groupRunner runs the member turns of group chats (A-106; nil means the member's own
	// ChatStream); guarded by mu.
	groupRunner grouphub.MemberRunner
	// hostCommands is the host's slash commands (A-136, UseCommand), offered by every
	// agent; guarded by mu.
	hostCommands []command.HostCommand
	// triggerRunner runs the trigger turns of every agent (A-70; nil means each agent's
	// Chat); guarded by mu. unsubscribeTriggerStart is the agent:spawned subscription
	// StartTriggers makes, so an agent spawned later starts its trigger worker too; Dispose
	// drops it. Guarded by mu.
	// Update (A-126): both removed - jobRunner runs trigger turns too, and an agent spawned
	// after StartJobs starts its jobs (clock and worker) in SpawnAgent (jobsStarted).
}

// NewAgentCore builds an AgentCore with every hub, in this order: the event bus,
// the LLM provider hub, the tool hub, the skill hub (over the persistence skill
// store), the orchestration, the trace service, the sandbox hub, the persistence
// hub and the artifact hub (over the persistence artifact store). It registers the
// three essential skills (skill-creator, plan, web-research) as built-in skills and
// returns the registration error when one fails, instead of panicking. It also subscribes the listener that disables a deleted skill
// for every agent that had it (see disableSkillForAllAgents).
// Update (A-70): there are now four essential skills; watch-and-react is the fourth.
// Update (A-126): watch-and-notify replaces watch-and-react.
func NewAgentCore(opts ...AgentCoreOption) (*AgentCore, error) {
	var o coreOptions
	for _, opt := range opts {
		if opt != nil {
			opt(&o)
		}
	}
	c := &AgentCore{logger: o.logger}
	if c.logger == nil {
		c.logger = slog.Default()
	}

	c.events = o.bus
	if c.events == nil {
		var busOpts []events.Option
		if o.logger != nil {
			busOpts = append(busOpts, events.WithLogger(o.logger))
		}
		c.events = events.New(busOpts...)
	}
	c.llm = llmhub.NewLLMHub()
	c.tools = toolhub.NewToolHub()
	// Resolved on every call, so UsePersistence after startup reaches the hub.
	c.skills = skillhub.NewSkillHub(func() persistence.SkillStore { return c.PersistenceHub().Skills }, skillhub.WithLogger(c.logger), skillhub.WithEvents(c.events))
	// A-105 revision: resolved on every call too; Delete publishes subagent-type:deleted.
	c.subagentTypes = subagenthub.NewSubagentHub(func() persistence.SubagentTypeStore { return c.PersistenceHub().SubagentTypes },
		subagenthub.WithLogger(c.logger), subagenthub.WithEvents(c.events))
	// A-101: no SecretHub provider until the host sets one - values are then stored unencrypted
	// (warned once). Resolved on every call too; Delete publishes secret:deleted.
	c.secrets = secrethub.NewSecretHub(func() persistence.SecretStore { return c.PersistenceHub().Secrets },
		secrethub.WithLogger(c.logger), secrethub.WithEvents(c.events))
	c.channels = channelplatformhub.New()
	c.agents = orchestration.New(c.spawnFactory)
	c.trace = o.trace
	if c.trace == nil {
		c.trace = trace.NewService()
	}
	// The sandbox hub and the artifact hub release a removed agent's data themselves,
	// as subscribers of agent:removed (see RemoveAgent); they log through the core's logger.
	c.sandboxes = sandboxhub.NewSandboxHub(sandboxhub.WithEvents(c.events))
	c.sandboxes.SetLogger(c.logger)
	// A partially filled hub gets the in-memory default in its empty slots.
	c.persist = o.persistence.FillDefaults()
	if c.persist == nil {
		c.persist = persistence.NewPersistenceHub()
	}
	// Resolved on every call, so UsePersistence after startup reaches views already
	// handed out.
	c.artifacts = artifacthub.NewArtifactHub(func() persistence.ArtifactStore { return c.PersistenceHub().Artifacts },
		artifacthub.WithEvents(c.events), artifacthub.WithLogger(c.logger))
	// A-106: group chats over the agents above; stores resolved on every call
	// (UsePersistence). It leaves a deleted agent's groups as a subscriber of agent:removed.
	c.groups = grouphub.New(grouphub.Deps{
		Events:   c.events,
		Store:    func() persistence.GroupStore { return c.PersistenceHub().Groups },
		Messages: func() persistence.GroupMessageStore { return c.PersistenceHub().GroupMessages },
		Sessions: func() persistence.SessionStore { return c.PersistenceHub().Sessions },
		MemberMessages: func() []persistence.MessageStore {
			h := c.PersistenceHub()
			return []persistence.MessageStore{h.Messages, h.CompactedMessages}
		},
		Agents: func() grouphub.AgentHost { return c.agents },
		Runner: c.groupTurnRunner,
		Logger: c.logger,
	})

	// Essential skills (skill-creator, plan, web-research) ship with the SDK; an agent
	// uses one when it is in its profile.skills and it has the tools the skill requires.
	// Update (A-70): plus watch-and-react (needs schedule_job and manage_trigger).
	// Update (A-126): watch-and-notify instead (needs manage_job).
	for _, pkg := range skillhub.EssentialSkills() {
		if _, err := c.skills.Register(pkg, skill.SourceBuiltIn); err != nil {
			return nil, err
		}
	}
	// A-105: built-in subagent types (general-purpose, researcher); custom ones are read
	// from the store on use.
	for _, def := range subagenthub.BuiltInTypes() {
		if _, err := c.subagentTypes.Register(def, subagenttype.SourceBuiltIn); err != nil {
			return nil, err
		}
	}
	// A deleted skill is disabled for every agent that had it - whoever deleted it (an agent
	// through skill_manage, or the host) - so no profile keeps pointing at a missing skill.
	c.skills.OnDeleted(c.disableSkillForAllAgents)
	// A-105 revision: a deleted subagent type is disabled for every agent that had it - a
	// subscriber of subagent-type:deleted, which the hub publishes before Delete returns.
	events.SubscribeCtx(c.events, events.SubagentTypeDeleted, func(ctx context.Context, _ kernel.Event, p events.SubagentTypeDeletedPayload) {
		c.removeSubagentType(ctx, p.Name)
	})
	// A-101: a deleted GLOBAL secret is dropped from every agent that used it - a subscriber
	// of secret:deleted, which the hub publishes before Delete returns.
	events.SubscribeCtx(c.events, events.SecretDeleted, func(ctx context.Context, _ kernel.Event, p events.SecretChangedPayload) {
		if p.OwnerAgentID == "" {
			c.removeGlobalSecret(ctx, p.Name)
		}
	})
	// A-105: a deleted agent is no longer anyone's subagent - a subscriber of agent:removed,
	// after the sandbox hub and the artifact hub.
	events.SubscribeCtx(c.events, events.AgentRemoved, func(ctx context.Context, _ kernel.Event, p events.AgentRemovedPayload) {
		c.removeSubagentTarget(ctx, p.AgentID)
	})
	return c, nil
}

// removeSubagentTarget is the agent:removed subscriber (A-105): every live agent that
// could delegate to the removed agent drops it from its allowlist, and its profile is
// saved through the CURRENT persistence hub. A failed save is logged and does not fail
// the removal; the ctx is the one RemoveAgent published with, without its cancellation
// (the removal is already done), bounded per agent by skillDeleteSaveTimeout. A panic is
// recovered and logged: the subscriber runs inside RemoveAgent.
func (c *AgentCore) removeSubagentTarget(ctx context.Context, agentID string) {
	defer func() {
		if r := recover(); r != nil {
			c.logger.Error("subagents: panic while dropping a deleted agent from the allowlists", "agent", agentID, "panic", redact.Panic(r))
		}
	}()
	for _, a := range c.agents.List() {
		p := a.Profile()
		if p.Subagents == nil || !slices.ContainsFunc(p.Subagents.Allow, func(t subagent.TargetConfig) bool { return t.AgentID == agentID }) {
			continue
		}
		cfg := p.Subagents.Clone()
		cfg.Allow = slices.DeleteFunc(cfg.Allow, func(t subagent.TargetConfig) bool { return t.AgentID == agentID })
		a.UpdateSubagents(cfg)
		saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), skillDeleteSaveTimeout)
		if err := c.PersistenceHub().Agents.Save(saveCtx, a.Profile()); err != nil {
			c.logger.Warn("subagents: could not save an agent after deleting one of its subagents", "agent", a.ID(), "removed", agentID, "error", err)
		}
		cancel()
	}
}

// removeSubagentType is the subagent-type:deleted subscriber (A-105 revision): every live
// agent that had the type enabled loses it, and its profile is saved through the CURRENT
// persistence hub. A failed save is logged and does not fail the delete; the ctx is the
// one Delete published with, without its cancellation (the delete is already done),
// bounded per agent by skillDeleteSaveTimeout. A panic is recovered and logged: the
// subscriber runs inside SubagentHub.Delete.
func (c *AgentCore) removeSubagentType(ctx context.Context, name string) {
	defer func() {
		if r := recover(); r != nil {
			c.logger.Error("subagents: panic while disabling a deleted subagent type for the agents", "type", name, "panic", redact.Panic(r))
		}
	}()
	for _, a := range c.agents.List() {
		p := a.Profile()
		if p.Subagents == nil || !slices.Contains(p.Subagents.Types, name) {
			continue
		}
		cfg := p.Subagents.Clone()
		cfg.Types = slices.DeleteFunc(cfg.Types, func(t string) bool { return t == name })
		a.UpdateSubagents(cfg)
		saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), skillDeleteSaveTimeout)
		if err := c.PersistenceHub().Agents.Save(saveCtx, a.Profile()); err != nil {
			c.logger.Warn("subagents: could not save an agent after deleting a subagent type", "agent", a.ID(), "type", name, "error", err)
		}
		cancel()
	}
}

// removeGlobalSecret is the secret:deleted subscriber for a global secret (A-101): every
// live agent that had it in AgentProfile.Secrets.Use loses it - and its secrets config is
// cleared (nil) when that was the last one - and its profile is saved through the CURRENT
// persistence hub. A failed save is logged and does not fail the delete; the ctx is the
// one Delete published with, without its cancellation (the delete is already done),
// bounded per agent by skillDeleteSaveTimeout. A panic is recovered and logged: the
// subscriber runs inside SecretHub.Delete.
func (c *AgentCore) removeGlobalSecret(ctx context.Context, name string) {
	defer func() {
		if r := recover(); r != nil {
			c.logger.Error("secrets: panic while dropping a deleted secret from the agents", "secret", name, "panic", redact.Panic(r))
		}
	}()
	for _, a := range c.agents.List() {
		p := a.Profile()
		if p.Secrets == nil || !slices.Contains(p.Secrets.Use, name) {
			continue
		}
		cfg := p.Secrets.Clone()
		cfg.Use = slices.DeleteFunc(cfg.Use, func(n string) bool { return n == name })
		if len(cfg.Use) == 0 {
			cfg = nil
		}
		a.UpdateSecrets(cfg)
		saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), skillDeleteSaveTimeout)
		if err := c.PersistenceHub().Agents.Save(saveCtx, a.Profile()); err != nil {
			c.logger.Warn("secrets: could not save an agent after deleting a secret it used", "agent", a.ID(), "secret", name, "error", err)
		}
		cancel()
	}
}

// skillDeleteSaveTimeout bounds the profile save of the skill-delete fan-out. The save
// runs inline, inside the skill hub's Delete, so a store that hangs must not hang the
// delete for ever. Each agent gets its own budget.
const skillDeleteSaveTimeout = 10 * time.Second

// fanOutParallelism is how many agents the skill-delete fan-out and Dispose handle at
// once.
const fanOutParallelism = 8

// disableSkillForAllAgents is the OnDeleted listener of the skill hub: every agent that had the skill enabled loses it, and its
// profile is saved. The skill list changes synchronously, before the listener returns
// (SkillManager.Delete relies on that: it finds the skill already disabled), and the
// save goes through the CURRENT persistence hub, like the allowlist save of the
// SkillManager.
//
// Each agent is updated with Agent.DisableSkill: the removal and the save run under
// that agent's allowlist change mutex, so a skill the agent creates at the same time is
// neither lost by the removal nor overwritten by the save. The agents are handled in parallel, at most fanOutParallelism
// at once, each with its own save timeout (skillDeleteSaveTimeout) under the values of
// no request (context.Background: the listener has no ctx).
//
// The saves run inline and the listener returns when all of them are over; a failure
// is logged with the agent id and the skill name and does not fail the delete. A
// background goroutine would outlive the call,
// and with it goleak and the Dispose promise that nothing runs after it. A panic is
// recovered and logged: the listener runs inside Delete, on the caller's goroutine.
func (c *AgentCore) disableSkillForAllAgents(name string) {
	defer func() {
		if r := recover(); r != nil {
			c.logger.Error("skills: panic while disabling a deleted skill for the agents", "skill", name, "panic", redact.Panic(r))
		}
	}()
	forEachBounded(c.agents.List(), func(a *agent.Agent) {
		defer func() {
			if r := recover(); r != nil {
				c.logger.Error("skills: panic while disabling a deleted skill for an agent", "agent", a.ID(), "skill", name, "panic", redact.Panic(r))
			}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), skillDeleteSaveTimeout)
		defer cancel()
		if _, err := a.DisableSkill(ctx, name); err != nil {
			c.logger.Warn("skills: could not save agent after deleting a skill", "agent", a.ID(), "skill", name, "error", err)
		}
	})
}

// forEachBounded runs f for every item, at most fanOutParallelism at once, and returns
// when every call returned.
func forEachBounded[T any](items []T, f func(T)) {
	sem := make(chan struct{}, fanOutParallelism)
	var wg sync.WaitGroup
	for _, it := range items {
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			f(it)
		})
	}
	wg.Wait()
}

// spawnFactory builds the agent of a profile over the hubs of this core. It is the
// orchestration factory. The persistence hub is the one current at this moment: the
// agent keeps that snapshot (see the AgentCore doc).
//
// The sandbox hub and the artifact hub are passed whole: the agent resolves its
// sandbox and its artifact view from them lazily, on every use.
//
// The skill allowlist save is the exception to the snapshot: a skill the agent
// creates or deletes saves its profile through the core's CURRENT persistence hub,
// read at call time under the read lock, so a
// save after UsePersistence reaches the new hub.
func (c *AgentCore) spawnFactory(ctx context.Context, p profile.AgentProfile) (*agent.Agent, error) {
	return agent.New(ctx, agent.Deps{
		LLMHub:         c.llm,
		ToolHub:        c.tools,
		PersistenceHub: c.PersistenceHub(),
		Events:         c.events,
		Trace:          c.trace,
		SkillHub:       c.SkillHub(),
		SandboxHub:     c.SandboxHub(),
		ArtifactHub:    c.ArtifactHub(),
		SaveProfile: func(ctx context.Context, p profile.AgentProfile) error {
			if err := c.PersistenceHub().Agents.Save(ctx, p); err != nil {
				return err
			}
			// A notification for hosts once the change is stored; the save error above
			// still reaches the SkillManager.
			if raw, err := kernel.EncodeJSON(p); err == nil {
				events.Emit(c.events, events.AgentProfileChanged, "", events.AgentProfileChangedPayload{AgentID: p.ID, Profile: raw})
			} else {
				c.logger.Warn("events: could not encode the changed profile; agent:profile-changed not emitted", "agent", p.ID, "error", err)
			}
			return nil
		},
		Logger: c.logger,
		// Read on every run, so UseScheduledJobRunner reaches agents spawned before it.
		// Update (A-126): UseJobRunner, for every job run.
		JobRunner: c.jobTurnRunner,
		// A-100: the channel platform hub is shared; the runner is read on every turn.
		ChannelPlatformHub: c.channels,
		ChannelRunner:      c.channelTurnRunner,
		// A-105: the agents a subagent can be, and the runner of child turns (read on every
		// child run).
		Agents:         c.agents,
		SubagentRunner: c.subagentTurnRunner,
		// A-105 revision: the library the agent's enabled subagent types resolve from.
		SubagentHub: c.subagentTypes,
		// A-106: spawn scopes of group leads while they lead a group turn.
		SubagentScopes: c.groups.Scopes(),
		// A-101: the secrets the agent's tools use (its own and the global ones it enables).
		SecretHub: c.secrets,
		// A-136: the host's slash commands, read on every call.
		HostCommands: c.hostCommandList,
	}, p)
}

// ---------------------------------------------------------------------------
// Accessors
// ---------------------------------------------------------------------------

// LLMHub returns the hub of LLM providers. During a session a user
// can switch between models, for example when the session's limit is hit.
func (c *AgentCore) LLMHub() *llmhub.LLMHub { return c.llm }

// ToolHub returns the hub of tools. The hub may hold 50 tools while a
// DevOps agent enables only the 10 it needs through its profile.
//
// It is also where the host sets the interaction handler (Hub.SetInteractionHandler),
// the one channel to the human user that receives both the approval requests of
// consequential tools and the prompts of the clarify tool. An agent reads the handler at
// every tool batch, so one set after the agent was spawned is used. The handler may be
// called concurrently (the loop runs up to agentloop.WithMaxParallelTools tool calls at
// once, so two clarify or approval calls can overlap), must honour ctx (cancelled when the
// run stops), and the SDK sets no timeout on it: the host owns it.
func (c *AgentCore) ToolHub() *toolhub.ToolHub { return c.tools }

// SkillHub returns the hub of skills.
func (c *AgentCore) SkillHub() *skillhub.SkillHub { return c.skills }

// SubagentHub returns the library of subagent types (A-105 revision): built-in, app
// (UseSubagentType) and custom ones (persisted in PersistenceHub.SubagentTypes). Agents
// enable types by name in AgentProfile.Subagents.Types.
func (c *AgentCore) SubagentHub() *subagenthub.SubagentHub { return c.subagentTypes }

// Groups returns group chats (A-106): several agents in one conversation - the groups,
// their sessions and threads, and the group turns. Stored in PersistenceHub.Groups and
// GroupMessages.
func (c *AgentCore) Groups() *grouphub.Hub { return c.groups }

// SecretHub returns the hub of secrets (A-101): API keys and tokens agents use without the
// model seeing them, sealed with envelope encryption under the SecretHub provider
// (UseSecretHubProvider) into PersistenceHub.Secrets. Hosts manage global secrets through it, and
// seal and rotate their own credential columns with it.
func (c *AgentCore) SecretHub() *secrethub.SecretHub { return c.secrets }

// SandboxHub returns the hub of sandboxes: the shared sandbox, the
// provider registry and the policies. An agent's own sandbox is managed by the
// agent.
func (c *AgentCore) SandboxHub() *sandboxhub.SandboxHub { return c.sandboxes }

// ArtifactHub returns the hub of artifacts: the lasting, typed,
// versioned outputs agents make. It hands out a per-agent view over the persistence
// artifact store.
func (c *AgentCore) ArtifactHub() *artifacthub.ArtifactHub { return c.artifacts }

// PersistenceHub returns the persistence hub that is current now.
func (c *AgentCore) PersistenceHub() *persistence.PersistenceHub {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.persist
}

// Events returns the event bus, the pub-sub that decouples the
// services.
func (c *AgentCore) Events() events.Bus { return c.events }

// Trace returns the trace service: register sinks on it to tap the
// agent lifecycle spans.
func (c *AgentCore) Trace() *trace.Service { return c.trace }

// Agent returns the live agent registered under id; the bool is false when no agent
// of that id is live in this core. It is part of the read-only view of the
// orchestration: spawn agents through SpawnAgent, which also
// saves the profile, and remove them through RemoveAgent.
func (c *AgentCore) Agent(id string) (*agent.Agent, bool) { return c.agents.Get(id) }

// ListAgents returns the live agents in spawn order, as a fresh slice the caller may
// change.
func (c *AgentCore) ListAgents() []*agent.Agent { return c.agents.List() }

// ---------------------------------------------------------------------------
// Registration
// ---------------------------------------------------------------------------

// UsePersistence plugs a persistence hub into the core: the new
// hub replaces the current one and the skill hub reloads, because its custom skills
// live in the new hub's skill store. A nil hub, or a typed nil, is an error.
//
// Agents that are already live keep the hub they were spawned with for their
// conversations (see the AgentCore doc); only agents spawned afterwards use the new
// hub. A host that plugs a hub must not mutate its exported fields afterwards; a
// slot it left nil gets the in-memory default here (persistence.PersistenceHub.FillDefaults),
// before the hub is published.
func (c *AgentCore) UsePersistence(h *persistence.PersistenceHub) error {
	if nilcheck.IsNil(h) {
		return errors.New("aec: UsePersistence needs a persistence hub")
	}
	h.FillDefaults()
	c.mu.Lock()
	c.persist = h
	c.mu.Unlock()
	c.skills.Reload()
	// A-105: and custom subagent types.
	c.subagentTypes.Reload()
	return nil
}

// UseLLMProvider registers an LLM provider; the first one registered is the primary.
func (c *AgentCore) UseLLMProvider(p llmhub.Provider) error { return c.llm.Register(p) }

// UseTool registers a tool.
func (c *AgentCore) UseTool(t toolhub.Handler) error { return c.tools.Register(t) }

// UseSkill registers an app skill package. It is read-only at runtime
// and an agent enables it through its profile skills. An invalid package or a taken
// name is an error.
func (c *AgentCore) UseSkill(pkg skill.Package) error {
	_, err := c.skills.Register(pkg, skill.SourceApp)
	return err
}

// UseSandboxProvider registers a sandbox provider on the sandbox hub: one provider per
// kind.
func (c *AgentCore) UseSandboxProvider(p sandboxhub.Provider) error { return c.sandboxes.Register(p) }

// UseScheduledJobRunner sets how the prompt-job turns of every agent run (A-69), for
// example behind the same per-agent lock as the host's chat routes. (A-97: server/go uses a
// per-session lock; the default runner chats in the run's session and the agent's
// per-session turn gate orders it.) It applies to the
// agents that are already live too. A nil runner restores each agent's default runner.
// Update (A-126): UseJobRunner - every job run (schedule, event, webhook); it was
// UseScheduledJobRunner + UseTriggerRunner.
func (c *AgentCore) UseJobRunner(r job.Runner) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.jobRunner = r
}

// jobTurnRunner is the runner set with UseJobRunner (nil when none).
func (c *AgentCore) jobTurnRunner() job.Runner {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.jobRunner
}

// UseSubagentType registers an app subagent type (A-105 revision). It is read-only at
// runtime and an agent enables it through AgentProfile.Subagents.Types. An invalid
// definition or a taken name is a *subagent.Error.
func (c *AgentCore) UseSubagentType(def subagenttype.Definition) error {
	_, err := c.subagentTypes.Register(def, subagenttype.SourceApp)
	return err
}

// UseCommand adds a slash command every agent offers (A-136), in the web app and chat apps
// alike; it applies to the agents that are already live too. An error is a
// *command.Error: CodeConflict when its name or an alias is taken by a built-in or another
// host command (opts.Replace replaces a built-in marked replaceable: /usage), CodeInvalid
// for a bad name or a spec without exactly one of Run and Prompt.
func (c *AgentCore) UseCommand(spec command.Spec, opts command.RegisterOptions) error {
	if err := command.Validate(spec); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entries := agent.BuiltInCommandSpecs()
	for _, h := range c.hostCommands {
		entries = append(entries, command.Entry{Spec: h.Spec, Source: command.SourceHost})
	}
	if err := command.CheckClash(spec, opts, entries, command.SourceHost); err != nil {
		return err
	}
	c.hostCommands = append(c.hostCommands, command.HostCommand{Spec: spec, Options: opts})
	return nil
}

// hostCommandList is the commands added with UseCommand (A-136), a fresh slice.
func (c *AgentCore) hostCommandList() []command.HostCommand {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return slices.Clone(c.hostCommands)
}

// UseSecretHubProvider sets who holds the master keys secrets are sealed with (A-101):
// secrethub.LocalSecretHubProvider (a key from the host's environment), or the host's KMS adapter
// (AWS KMS, Vault...). Without one, values are stored unencrypted (warned once).
func (c *AgentCore) UseSecretHubProvider(p secrethub.SecretHubProvider) {
	c.secrets.UseSecretHubProvider(p)
}

// UseSubagentRunner sets how the child turns of every agent run (A-105), for example
// behind the same per-session lock, stop registry, traces and usage as the host's chat
// routes. It applies to the agents that are already live too. A nil runner restores the
// child agent's own Chat.
// (A-105 revision: a nil runner restores the turn itself - subagent.TurnContext.Execute,
// the child agent's Chat or the subagent type's short-lived loop; a host runner calls
// Execute inside its bookkeeping.)
func (c *AgentCore) UseSubagentRunner(r subagent.Runner) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.subagentRunner = r
}

// UseGroupRunner sets how the member turns of group chats run (A-106), for example
// behind the same per-session lock, stop registry, traces and usage as the host's chat
// routes. It applies to the turns that start afterwards. A nil runner restores the
// member's own ChatStream (grouphub.MemberTurnContext.Execute).
func (c *AgentCore) UseGroupRunner(r grouphub.MemberRunner) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.groupRunner = r
}

// groupTurnRunner is the runner set with UseGroupRunner (nil when none).
func (c *AgentCore) groupTurnRunner() grouphub.MemberRunner {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.groupRunner
}

// subagentTurnRunner is the runner set with UseSubagentRunner (nil when none).
func (c *AgentCore) subagentTurnRunner() subagent.Runner {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.subagentRunner
}

// UseChannelPlatform adds a chat platform agents' channels can use (A-100), for example
// telegram.Platform(). It replaces a platform of the same name. A nil platform or an
// empty name is an error.
func (c *AgentCore) UseChannelPlatform(p channel.Platform) error {
	return c.channels.Register(p)
}

// ChannelPlatformHub returns the hub of channels: the chat platforms agents' channels use and
// which bot account each channel holds. An agent's channels are managed by the agent.
func (c *AgentCore) ChannelPlatformHub() *channelplatformhub.ChannelPlatformHub { return c.channels }

// UseChannelTurnRunner sets how the channel turns of every agent run (A-100), for example
// behind the same per-session lock as the host's chat routes. It applies to the agents
// that are already live too. A nil runner restores each agent's own Chat.
func (c *AgentCore) UseChannelTurnRunner(r channel.TurnRunner) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.channelRunner = r
}

// channelTurnRunner is the runner set with UseChannelTurnRunner (nil when none).
func (c *AgentCore) channelTurnRunner() channel.TurnRunner {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.channelRunner
}

// HandleChannelWebhook routes an inbound webhook request to the channel it is for (A-100
// phase 3), e.g. from the host's POST /webhooks/channels/{id}. Used by platforms that push
// messages to the host (Microsoft Teams). 404 when no running agent owns the channel.
func (c *AgentCore) HandleChannelWebhook(ctx context.Context, channelID string, req channel.WebhookRequest) channel.WebhookResponse {
	notFound := channel.WebhookResponse{Status: 404, Body: map[string]string{"error": "Channel not found"}}
	ch, err := c.PersistenceHub().Channels.Get(ctx, channelID)
	if err != nil || ch == nil {
		return notFound
	}
	a, ok := c.agents.Get(ch.AgentID)
	if !ok {
		return notFound
	}
	return a.Channels().HandleWebhook(ctx, channelID, req)
}

// StartChannels connects every live agent's stored, enabled channels (A-100); from then
// on every agent spawned with SpawnAgent connects its channels too. Call it once the host
// is ready to run turns: tools, platforms and the runner set. A channel that cannot
// connect shows "failed" in its State; an agent whose channels could not be listed is
// logged and the others still start; the errors are joined.
func (c *AgentCore) StartChannels(ctx context.Context) error {
	c.mu.Lock()
	c.channelsStarted = true
	c.mu.Unlock()
	var errs []error
	for _, a := range c.agents.List() {
		if err := a.StartChannels(ctx); err != nil {
			c.logger.Error("channels: could not start an agent's channels", "agent", a.ID(), "error", err)
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// HandleTriggerWebhook routes a webhook delivery to the trigger it is for (A-70), e.g.
// from the host's POST /webhooks/triggers/{id}. The trigger checks the signature on the
// raw body and the call returns only after the event is stored, so the host answers 2xx
// only for a delivery it will not lose; a store failure is an error (answer 5xx). 404
// when no running agent owns the trigger.
// Update (A-126): HandleJobWebhook - to the webhook job it is for (POST
// /webhooks/jobs/{id}).
func (c *AgentCore) HandleJobWebhook(ctx context.Context, jobID string, req job.WebhookRequest) (job.WebhookResponse, error) {
	j, err := c.PersistenceHub().Jobs.Get(ctx, jobID)
	if err != nil {
		return job.WebhookResponse{}, err
	}
	var a *agent.Agent
	if j != nil {
		a, _ = c.agents.Get(j.AgentID)
	}
	if a == nil {
		return job.WebhookResponse{Status: 404, Body: map[string]string{"error": "Job not found"}}, nil
	}
	return a.Jobs().HandleWebhook(ctx, jobID, req)
}

// StartScheduledJobs loads every live agent's stored prompt jobs and arms their timers
// (A-69); from then on every agent spawned with SpawnAgent starts its jobs too, so an
// agent that a host rebuilds (DisposeAgent then SpawnAgent) keeps its schedule. Call it
// once the host is ready to run turns: tools registered and the runner set. A job missed
// while the host was down runs once. An agent whose jobs could not start is logged and
// the others still start; the errors are joined.
// Update (A-126): StartJobs - it also starts every agent's job worker (queued runs,
// retries, runs a previous process left); it was StartScheduledJobs + StartTriggers.
func (c *AgentCore) StartJobs(ctx context.Context) error {
	c.mu.Lock()
	c.jobsStarted = true
	c.mu.Unlock()
	var errs []error
	for _, a := range c.agents.List() {
		if err := a.StartJobs(ctx); err != nil {
			c.logger.Error("jobs: could not start an agent's jobs", "agent", a.ID(), "error", err)
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// ---------------------------------------------------------------------------
// Agent management
// ---------------------------------------------------------------------------

// SpawnAgent spawns the agent of a profile and saves the profile to the agent store.
// events.AgentSpawned (SessionID "") is emitted once the agent is spawned, before the
// save; Initialize does not emit it for the stored agents it brings back.
// A duplicate id is an error whose text is `Agent "<id>" already exists`.
//
// The save is synchronous and the agent is typed. The agent is spawned first, so
// when the save fails the agent stays spawned and registered, and SpawnAgent returns
// it together with the save error.
func (c *AgentCore) SpawnAgent(ctx context.Context, p profile.AgentProfile) (*agent.Agent, error) {
	a, err := c.agents.Spawn(ctx, p)
	if err != nil {
		return nil, err
	}
	events.Emit(c.events, events.AgentSpawned, "", events.AgentSpawnedPayload{AgentID: p.ID})
	if err := c.PersistenceHub().Agents.Save(ctx, p); err != nil {
		return a, err
	}
	// After StartScheduledJobs, a new (or rebuilt) agent starts its prompt jobs at once.
	c.mu.RLock()
	started := c.jobsStarted
	channelsStarted := c.channelsStarted
	c.mu.RUnlock()
	if started {
		if err := a.StartJobs(ctx); err != nil {
			c.logger.Error("jobs: could not start an agent's jobs", "agent", p.ID, "error", err)
		}
	}
	// After StartChannels, a new (or rebuilt) agent connects its channels at once (A-100).
	if channelsStarted {
		if err := a.StartChannels(ctx); err != nil {
			c.logger.Error("channels: could not start an agent's channels", "agent", p.ID, "error", err)
		}
	}
	return a, nil
}

// RemoveAgent deletes an agent for good: it is stopped, its profile is deleted from
// the store, and its sandbox files and artifacts go with it. The order is:
//
//  1. the orchestration deletes the agent (Agent.Delete), which stops it and deletes
//     its own sandbox's files (running or cold). Best effort: a Docker that is down must
//     not block deleting the agent, so a failure is logged and the next steps run;
//  2. the profile is deleted from the agent store. A failure is returned at once and
//     the two releases below are skipped;
//  3. the sandbox hub removes the agent's folder in the shared sandbox. Best effort,
//     logged;
//  4. the artifact hub removes the agent's artifacts. Best effort, logged;
//  5. events.AgentRemoved is emitted (SessionID "") for hosts that clean up their own
//     data. It is not emitted when step 2 fails.
//
// Steps 3 to 5 are one events.Publish of AgentRemoved with ctx: the sandbox hub and
// the artifact hub are its first subscribers (sandboxhub.WithEvents, artifacthub.WithEvents,
// subscribed in that order by NewAgentCore), and a host's subscribers run after them.
// (A-105: the core's own subscriber follows the two hubs: every live agent that could
// delegate to the removed one drops it from its subagent allowlist. The subagent runs the
// removed agent started are deleted before the publish, with its jobs and channels.)
//
// The log lines carry the agent id and the error, never the profile or file contents.
// An id with no live agent is not an error: the data of an agent that was never
// spawned in this process, for example one stored by an earlier run, is still deleted.
// Step 1 then has no agent to dispose, so the own-sandbox data (cold: nothing runs) is
// removed through a throwaway sandbox manager that goes through the same providers.
// For a live agent that was disposed before (by the host, with Dispose or Delete),
// or whose earlier Dispose failed, Agent.Delete still removes the data
// instead of returning at once, so RemoveAgent can finish what that call left.
func (c *AgentCore) RemoveAgent(ctx context.Context, id string) error {
	_, live := c.agents.Get(id)
	// Retention: an agent's sandbox files live until the agent is deleted.
	if err := c.agents.Delete(ctx, id); err != nil {
		c.logger.Warn("agent did not stop cleanly; its own sandbox files may be left behind", "agent", id, "error", err)
	}
	if !live {
		if err := agent.NewSandboxManager(c.sandboxes, id, nil).Release(ctx, true); err != nil {
			c.logger.Warn("sandbox: could not delete the own sandbox files of an agent that is not live", "agent", id, "error", err)
		}
	}
	if err := c.PersistenceHub().Agents.Delete(ctx, id); err != nil {
		return err
	}
	// Its prompt jobs and their run history go with it (A-69); Agent.Delete above stopped
	// its timers. Best effort, logged.
	// Its triggers, their inbox and run history go with it (A-70); Agent.Delete above
	// stopped its runs. Best effort, logged.
	// Update (A-126): its jobs of every kind, with their inbox and run history.
	if err := c.PersistenceHub().Jobs.DeleteByAgent(ctx, id); err != nil {
		c.logger.Warn("jobs: could not delete the jobs of a removed agent", "agent", id, "error", err)
	}
	// Its channels go with it (A-100); Agent.Delete above disconnected them. Their chats'
	// sessions stay. Best effort, logged.
	if err := c.PersistenceHub().Channels.DeleteByAgent(ctx, id); err != nil {
		c.logger.Warn("channels: could not delete the channels of a removed agent", "agent", id, "error", err)
	}
	// The child runs it started go with it (A-105); the children's own sessions stay with
	// the child agents. Other agents that could delegate to it drop it from their
	// allowlists (removeSubagentTarget, on agent:removed). Best effort, logged.
	if err := c.PersistenceHub().SubagentRuns.DeleteByAgent(ctx, id); err != nil {
		c.logger.Warn("subagents: could not delete the subagent runs of a removed agent", "agent", id, "error", err)
	}
	// Its own secrets go with it (A-101); global secrets stay. Best effort, logged.
	if err := c.secrets.DeleteByOwner(ctx, id); err != nil {
		c.logger.Warn("secrets: could not delete the secrets of a removed agent", "agent", id, "error", err)
	}
	// The shared-sandbox folder and the artifacts are released by the agent:removed
	// subscribers of the sandbox hub and the artifact hub, with this ctx.
	events.Publish(ctx, c.events, events.AgentRemoved, "", events.AgentRemovedPayload{AgentID: id})
	return nil
}

// DisposeAgent stops the live agent registered under id and unloads it from the core,
// keeping its stored profile, sessions, sandbox files and artifacts, so a later
// SpawnAgent or Initialize can bring it back. RemoveAgent is the delete. An unknown id
// is a no-op that returns nil.
func (c *AgentCore) DisposeAgent(ctx context.Context, id string) error {
	return c.agents.Dispose(ctx, id)
}

// ---------------------------------------------------------------------------
// Lifecycle
// ---------------------------------------------------------------------------

// Initialize runs the persistence hub's initialize hook and spawns every stored
// agent profile that is not already live. The profiles are not saved again.
func (c *AgentCore) Initialize(ctx context.Context) error {
	if err := kernel.Initialize(ctx, c.PersistenceHub()); err != nil {
		return err
	}
	profiles, err := c.PersistenceHub().Agents.List(ctx)
	if err != nil {
		return err
	}
	for _, p := range profiles {
		if _, live := c.agents.Get(p.ID); live {
			continue
		}
		if _, err := c.agents.Spawn(ctx, p); err != nil {
			return err
		}
	}
	return nil
}

// disposeStepTimeout bounds each cleanup step of Dispose that runs after the agents
// (the sandbox hub, the persistence hub): those steps run under
// context.WithoutCancel(ctx), so a ctx that expired while the agents were stopping does
// not make them fail at once and leak (a shared container left running), and under
// this timeout, so one that hangs cannot hang Dispose for ever.
const disposeStepTimeout = 30 * time.Second

// Dispose stops the core: every agent, then the sandbox hub (which stops the shared
// sandbox and keeps its files for the next start), then the persistence hub. A step
// that fails does not stop the ones after it: the errors are joined with errors.Join.
//
// The agents are disposed in parallel, at most 8 at once, each with ctx, so one slow agent does not use up the time of the others;
// each agent bounds its own waits by ctx and runs its own cleanup under its own budget
// (see agent.Agent.Dispose). The two hub steps then run one after the other under
// context.WithoutCancel(ctx) with a timeout of their own each, so they still run when
// ctx has already ended.
func (c *AgentCore) Dispose(ctx context.Context) error {
	// A-106: stops running group turns (their members' turns end with the agents below).
	c.groups.Dispose()
	agents := c.agents.List()
	var mu sync.Mutex
	var errs []error
	forEachBounded(agents, func(a *agent.Agent) {
		err := func() (err error) {
			defer func() {
				if r := recover(); r != nil {
					err = errors.New("aec: panic while disposing agent " + a.ID() + ": " + redact.Panic(r))
				}
			}()
			return c.agents.Dispose(ctx, a.ID())
		}()
		if err != nil {
			mu.Lock()
			errs = append(errs, err)
			mu.Unlock()
		}
	})
	if err := runAll(ctx, []func(context.Context) error{
		detachedStep(c.sandboxes.Dispose),
		detachedStep(func(ctx context.Context) error { return kernel.Dispose(ctx, c.PersistenceHub()) }),
	}); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// detachedStep runs step under context.WithoutCancel of the ctx it is given, bounded by
// disposeStepTimeout (see Dispose).
func detachedStep(step func(context.Context) error) func(context.Context) error {
	return func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), disposeStepTimeout)
		defer cancel()
		return step(ctx)
	}
}

// runAll runs every step in order, whatever the earlier ones returned, and joins the
// errors of the failing ones (nil when none failed).
func runAll(ctx context.Context, steps []func(context.Context) error) error {
	var errs []error
	for _, step := range steps {
		if err := step(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
