package agent

import (
	"context"
	"errors"
	"iter"
	"slices"
	"sync"
	"time"

	"github.com/Phong95/aec-agent-go/components/agent/agentloop"
	"github.com/Phong95/aec-agent-go/components/agent/command"
	"github.com/Phong95/aec-agent-go/components/agent/job"
	"github.com/Phong95/aec-agent-go/components/agent/memorysystem"
	"github.com/Phong95/aec-agent-go/components/agent/memorysystem/memory"
	"github.com/Phong95/aec-agent-go/components/agent/profile"
	"github.com/Phong95/aec-agent-go/components/agent/subagent"
	"github.com/Phong95/aec-agent-go/components/artifacthub/artifact"
	"github.com/Phong95/aec-agent-go/components/channelplatformhub/channel"
	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/components/secrethub/secret"
	"github.com/Phong95/aec-agent-go/components/subagenthub"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/internal/nilcheck"
	"github.com/Phong95/aec-agent-go/kernel"
)

// Agent is the minimum unit of the SDK and its most important concept: the harness
// layer around an LLM. It is built by aec.AgentCore.SpawnAgent, which
// goes through orchestration.Orchestration, or directly with New.
//
// It owns the managers that adapt the SDK hubs to the loop's ports, the memory
// system, the loop and a private copy of its profile. All methods are safe for
// concurrent use; see the package comment for what that does and does not promise.
// (A-97: turns of different sessions run in parallel, turns of one session in order.)
//
// An Agent runs background work (the memory consolidation passes and the scheduled
// jobs) under a lifetime context it owns: independent of the context given to New (it
// carries neither its cancellation nor its values), and cancelled by Dispose only after
// Dispose has waited for the work. An Agent must therefore be disposed when it is no
// longer used.
type Agent struct {
	id string

	// mu guards profile. Lock order: mu first, then the lock of a manager, and mu is
	// never held across a call into a manager that can call back (the setters copy
	// under mu, release it, and only then tell the manager).
	mu      sync.RWMutex
	profile profile.AgentProfile

	saveProfile func(ctx context.Context, p profile.AgentProfile) error
	// toolHub is the hub whose interaction handler the router falls back to (A-105: the
	// router is Agent.interactionFor).
	toolHub *toolhub.ToolHub

	models *ModelManager
	tools  *ToolManager
	// skills is nil when the agent was built without a skill hub (Deps.SkillHub nil).
	skills *SkillManager
	ctxMgr *ContextManager
	conv   *ConversationManager
	loop   *agentloop.Loop
	// memory is the agent's long-term memory: recall for each turn and the
	// consolidation scheduler. It is started by New and stopped by Dispose.
	memory *memorysystem.System
	// sandboxes and artifacts are nil when the agent was built without the hub
	// (Deps.SandboxHub or Deps.ArtifactHub nil).
	sandboxes *SandboxManager
	artifacts *ArtifactManager
	// jobs is the agent's scheduled jobs. They run under the
	// lifetime context and are stopped and waited for by Dispose.
	// Update (A-126): the scheduled tasks (plugin code) were removed; every job is a
	// stored job of the job manager below.
	// prompts is the agent's prompt jobs (A-69): stored prompt + schedule, each run a
	// full turn. Their runs derive from the lifetime context; Dispose cancels them and
	// waits for them.
	// Update (A-126): merged into jobs below.
	// channels is the agent's chat-app channels (A-100). Their turns derive from the
	// lifetime context; Dispose disconnects them first and waits for their turns.
	channels *channelManager
	// triggers is the agent's event triggers (A-70). Their runs derive from the lifetime
	// context; Dispose stops intake after the channels and gives the runs in progress back
	// to the queue.
	// Update (A-126): jobs - the agent's jobs of every kind (prompt jobs and triggers
	// merged): the clock, intake and worker of the job manager.
	jobs *jobManager
	// subagents (A-105) is this agent as a parent (spawn_subagent) and as somebody's
	// child: the child runs it started, their budgets, and the bindings of its sessions
	// that are child runs. Dispose stops its running children.
	subagents *subagentManager
	// secrets (A-101) is the agent's secrets - its own and the global ones it enabled;
	// values reach tools only.
	secrets *secretManager
	// commands (A-136) is the agent's slash commands: the built-ins (working through the
	// managers above), the host's and the agent's own.
	commands *commandManager
	// jobTurn serializes the turns of the default prompt-job runner (runJobTurn): a load
	// of the run's session and the chat on it are one step. Capacity one.
	// -> Superseded (A-97): removed. The default runner chats in the run's session
	// (WithChatSession) and the turn gate orders it with every other turn of that
	// session, while runs in other sessions go in parallel.

	// cancelLifetime cancels the lifetime context of the background work. Dispose
	// calls it last, after the work was waited for.
	cancelLifetime context.CancelFunc

	// runLock, turns, runsMu, closed, closing, runs and runsCtx (the turn gate and the
	// dispose bookkeeping of the chat runs) moved to the ConversationManager (conv) with
	// their comments: it starts every turn (beginTurn) and Dispose closes them through it.

	// disposeMu guards disposeDone and dataRemoved. It is never held across a wait.
	disposeMu sync.Mutex
	// disposeDone is nil until the first Dispose starts and is closed when it ends; a
	// later Dispose waits on it (honouring its own ctx).
	disposeDone chan struct{}
	// dataRemoved is true once a Release that removes the data (Delete) succeeded, so
	// a later Delete has nothing left to remove.
	dataRemoved bool
}

// An Agent is a kernel.Disposer, so kernel.Dispose and any host that disposes
// plugins can stop it.
var _ kernel.Disposer = (*Agent)(nil)

// New builds an Agent from the profile p: the managers, then the loop. The profile
// is deep-copied, so the agent never aliases the caller's struct. It returns an
// error naming the first missing required dependency (Deps.LLMHub, ToolHub, PersistenceHub
// or Events, a typed nil counts as missing), and it fails with the ctx error when
// ctx is already done.
//
// New also builds the agent's memory system over the persistence hub of Deps (the
// snapshot taken at spawn) and the agent's ModelManager, and starts it: a goroutine
// runs only while the profile's consolidation interval is above zero. The background
// work runs under a lifetime context that is independent of ctx (it derives from
// context.Background()): neither the cancellation of the ctx of a request that spawned
// the agent nor the values it carries (auth, trace ids of that request) reach the
// agent's background work, so cancelling that request does not stop its memory;
// Dispose ends it. A profile with an empty id is an error, because the memory records
// carry the id.
func New(ctx context.Context, deps Deps, p profile.AgentProfile, opts ...Option) (*Agent, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	required := []struct {
		name string
		nil  bool
	}{
		{"LLMHub", nilcheck.IsNil(deps.LLMHub)},
		{"ToolHub", nilcheck.IsNil(deps.ToolHub)},
		{"PersistenceHub", nilcheck.IsNil(deps.PersistenceHub)},
		{"Events", nilcheck.IsNil(deps.Events)},
	}
	for _, d := range required {
		if d.nil {
			return nil, errors.New("agent: Deps." + d.name + " is required")
		}
	}
	var o options
	for _, opt := range opts {
		if opt != nil {
			opt(&o)
		}
	}

	// The lifetime context of the background work. It must outlive Dispose's wait, so
	// it is cancelled by Dispose, or here when New fails (nothing is running then). It
	// derives from context.Background(), not from ctx: a derived ctx would keep the
	// values of the spawning request (auth, trace ids) alive for as long as the agent
	// lives (10-SECURITY UF-3).
	lifetime, cancel := context.WithCancel(context.Background())
	built := false
	defer func() {
		if !built {
			cancel()
		}
	}()

	own := p.Clone()
	a := &Agent{
		id: own.ID, profile: own, saveProfile: deps.SaveProfile, cancelLifetime: cancel,
		toolHub: deps.ToolHub,
	}
	// The scheduled jobs: nothing runs until a job is started, and Dispose stops them.
	// Update (A-126): scheduled tasks (plugin code) removed.
	// The prompt jobs (A-69): nothing is armed until StartJobs (or Create), and Dispose
	// stops them. The runner is resolved on every run.
	// Update (A-126): see the job manager below.
	// Channels (A-100): chat apps the agent talks through, stored in the persistence hub.
	// Nothing connects until StartChannels (or Create); turns go through the host's runner
	// or this agent's Chat.
	a.channels = newChannelManager(channelManagerDeps{
		agentID:     own.ID,
		bus:         deps.Events,
		store:       deps.PersistenceHub.Channels,
		sessions:    deps.PersistenceHub.Sessions,
		platformHub: deps.ChannelPlatformHub,
		runner: func() channel.TurnRunner {
			if deps.ChannelRunner != nil {
				if r := deps.ChannelRunner(); r != nil {
					return r
				}
			}
			return a.runChannelTurn
		},
		agentName: func() string { return a.Profile().Name },
		listModels: func(ctx context.Context) ([]string, bool) {
			return a.models.ListModels(ctx)
		},
		defaultModel: func() string { return a.Profile().LLMModel },
		// Phase 3: a job result posted in a chat is added to the chat's conversation.
		appendMessage: func(ctx context.Context, sessionID string, m kernel.Message) error {
			return a.conv.AppendMessage(ctx, sessionID, m)
		},
		// A-135: /yolo and /status.
		approvalMode: func(ctx context.Context, sessionID string) (kernel.ApprovalMode, error) {
			return a.conv.ApprovalMode(ctx, sessionID)
		},
		setApprovalMode: func(ctx context.Context, sessionID string, mode kernel.ApprovalMode) error {
			_, _, err := a.conv.SetApprovalMode(ctx, sessionID, mode)
			return err
		},
		// A-136: every other command runs through the agent's command manager (built below).
		commands: func() command.Commands { return a.commands },
		lifetime: lifetime, //nolint:contextcheck // the lifetime derives from Background on purpose (10-SECURITY UF-3)
		logger:   deps.Logger,
	})
	// Event triggers (A-70): configured events wake the agent; the inbox and run queue live
	// in the persistence hub, turns go through the host's runner or this agent's Chat. It
	// hears scheduled-job and channel events on the bus only. Nothing is claimed until
	// StartTriggers.
	// Update (A-126): the job manager - jobs on a schedule (A-69 prompt jobs), on events and
	// on webhooks; turns go through the host's runner (Deps.JobRunner) or this agent's
	// Chat. It hears job and channel events on the bus only. Nothing is armed or claimed
	// until StartJobs.
	a.jobs = newJobManager(jobManagerDeps{
		agentID:  own.ID,
		bus:      deps.Events,
		store:    deps.PersistenceHub.Jobs,
		sessions: deps.PersistenceHub.Sessions,
		runner: func() job.Runner {
			if deps.JobRunner != nil {
				if r := deps.JobRunner(); r != nil {
					return r
				}
			}
			return a.runJobTurn
		},
		loc: o.jobLocation,
		now: o.promptJobClock,
		// Runs leave a turn free for people's chats under loopConfig.maxConcurrentTurns.
		capacity: func() (int, int) { return a.conv.RunningTurns(), a.maxConcurrentTurns() },
		lifetime: lifetime, //nolint:contextcheck // the lifetime derives from Background on purpose (10-SECURITY UF-3)
		logger:   deps.Logger,
		// A-103: tool jobs - the stored call runs through the agent loop (built below, read
		// at run time), and the call and its result are added to the run's session.
		runTool: func(ctx context.Context, call kernel.ToolCall, sessionID string) (kernel.ToolResult, bool) {
			return a.loop.ExecuteToolCall(ctx, call, sessionID)
		},
		checkTool: func(name string) string { return a.tools.ScheduleProblem(name) },
		appendMessage: func(ctx context.Context, sessionID string, m kernel.Message) error {
			return a.conv.AppendMessage(ctx, sessionID, m)
		},
	})
	a.models = NewModelManager(deps.LLMHub, own)
	// A-106: a group lead's session is granted spawn_subagent while it leads (its spawn
	// scope).
	a.tools = NewToolManager(deps.ToolHub, own, WithSessionGrants(func(sessionID string) []string {
		return a.subagents.grantedTools(sessionID)
	}))
	// A-105 revision: the sandbox and artifact managers are built before the subagent
	// manager, whose subagent types work in this agent's sandbox and artifacts.
	var sandboxes agentloop.Sandboxes
	if deps.SandboxHub != nil {
		a.sandboxes = NewSandboxManager(deps.SandboxHub, own.ID, a.sandboxSelection)
		sandboxes = a.sandboxes
	}
	var artifacts func(sessionID string) artifact.View
	if deps.ArtifactHub != nil {
		a.artifacts = NewArtifactManager(deps.ArtifactHub, own.ID, deps.Logger)
		artifacts = a.artifacts.View
	}

	// A-101: the agent's secrets - its own and the global ones it enabled; values reach tools
	// only. Built before the subagent manager, whose subagent types use them too.
	a.secrets = newSecretManager(secretManagerDeps{
		agentID:   own.ID,
		profile:   a.Profile,
		hub:       deps.SecretHub,
		canManage: func() bool { return a.tools.Has("manage_secret") },
		logger:    deps.Logger,
	})

	// A-105: subagents - this agent as a parent (spawn_subagent) and as somebody's child.
	// Children are other agents of the host (Deps.Agents); child turns go through the
	// host's runner (Deps.SubagentRunner) or the child's own Chat. It hears the children's
	// model, tool and max-iterations events, and the parent's run completions, on the bus.
	a.subagents = newSubagentManager(subagentManagerDeps{
		agentID:      own.ID,
		profile:      a.Profile,
		bus:          deps.Events,
		store:        deps.PersistenceHub.SubagentRuns,
		sessions:     deps.PersistenceHub.Sessions,
		messages:     deps.PersistenceHub.Messages,
		host:         deps.Agents,
		runner:       deps.SubagentRunner,
		hasSpawnTool: func() bool { return a.tools.Has("spawn_subagent") },
		interaction:  a.interactionFor,
		// A-105 revision: subagent types from the library (Deps.SubagentHub) run as a
		// short-lived loop inside this agent, with its model, a filtered set of its tools,
		// its sandbox and artifacts, in a side-branch session of this agent.
		hub: func() *subagenthub.SubagentHub { return deps.SubagentHub },
		childLoop: childLoopDeps{
			models:    a.models,
			toolHub:   deps.ToolHub,
			bus:       deps.Events,
			trace:     deps.Trace,
			sandboxes: sandboxes,
			artifacts: artifacts,
			open: func(ctx context.Context, sessionID string) (*Conversation, bool, error) {
				return a.conv.Open(ctx, sessionID)
			},
			logger: deps.Logger,
			// A-101: a type child uses its parent's secrets.
			secrets: a.secrets,
			// A-135: a type child's approvals use its parent session's approval mode.
			approvalMode: a.approvalModeFor,
		},
		// A-106: a session's spawn scope (a group lead's, while it leads a group turn).
		scope: func(sessionID string) *subagent.SpawnScope {
			if deps.SubagentScopes == nil {
				return nil
			}
			return deps.SubagentScopes.Get(sessionID)
		},
		// A-137: which of this agent's tools are preferred for subagents.
		toolDefinitions: a.tools.ListFor,
		logger:          deps.Logger,
	})
	defer func() {
		if !built {
			a.subagents.dispose() // its bus subscriptions must not outlive a failed New
		}
	}()
	var ctxOpts []ContextOption
	var skills agentloop.Skills
	if deps.SkillHub != nil {
		skillOpts := []SkillManagerOption{
			// A skill whose required tools this agent lacks stays out of its index.
			WithHasTool(a.tools.Has),
			// A skill the agent created (or deleted) through skill_manage changes its allowlist.
			WithOnAllowlistChange(a.onAllowlistChange),
		}
		if deps.Logger != nil {
			skillOpts = append(skillOpts, WithSkillLogger(deps.Logger))
		}
		a.skills = NewSkillManager(deps.SkillHub, own, skillOpts...)
		skills = a.skills
		ctxOpts = append(ctxOpts, WithSkillIndex(a.skills.BuildIndex))
	}
	// A-105: the agents this one may delegate to, for the turn's session.
	ctxOpts = append(ctxOpts, WithSubagentIndex(a.subagents.buildIndex))
	// A-101: the agent's secrets (names only), after the subagent index.
	ctxOpts = append(ctxOpts, WithSecretIndex(func(ctx context.Context) (string, bool) {
		return a.secrets.buildIndex(ctx, a.secrets.d.canManage())
	}))
	// A-159: the folders on the user's computer the session works in, when it has a runner.
	if a.sandboxes != nil {
		ctxOpts = append(ctxOpts, WithWorkspaceIndex(func(_ context.Context, session kernel.Session) (string, bool) {
			return a.sandboxes.RunnerFoldersIndex(session.ID)
		}))
	}
	a.ctxMgr = NewContextManager(own, ctxOpts...)
	// A-97: the cap is read on every turn, so UpdateLoopConfig applies to the next one.
	// The runs' context ends with the lifetime one, so the cancel of a failed New (above)
	// releases it too.
	// A-135: it also holds each session's approval mode and announces changes on the bus.
	a.conv = NewConversationManager(deps.PersistenceHub, withTurns(lifetime, a.maxConcurrentTurns), withApprovalEvents(deps.Events, own.ID)) //nolint:contextcheck // derived from the lifetime context on purpose (10-SECURITY UF-3)
	// A-136: slash commands - the built-ins work through the managers (the loop is built
	// below and read when a command runs); the host's (Deps.HostCommands) are read on
	// every call.
	a.commands = newCommandManager(commandManagerDeps{
		agentID:      own.ID,
		bus:          deps.Events,
		hostCommands: deps.HostCommands,
		isBusy:       a.conv.IsBusy,
		builtIns:     builtInCommands(a.builtInCommandDeps()),
	})

	memOpts := make([]memorysystem.Option, 0, len(o.memoryOpts)+1)
	if deps.Logger != nil {
		memOpts = append(memOpts, memorysystem.WithLogger(deps.Logger))
	}
	memOpts = append(memOpts, o.memoryOpts...)
	sys, err := memorysystem.New(memorysystem.Deps{
		AgentID:     own.ID,
		Persistence: deps.PersistenceHub,
		Events:      deps.Events,
		Model:       a.models,
		MemoryFiles: a.memoryFiles,
		// A consolidation pass rebuilt USER.md / MEMORY.md: apply and save them.
		OnMemoryFilesCompacted: a.onMemoryFilesCompacted,
	}, own.Memory, memOpts...)
	if err != nil {
		return nil, err
	}
	a.memory = sys
	var mem agentloop.Memory = sys
	if o.memoryWrap != nil {
		mem = o.memoryWrap(mem)
	}

	loopOpts := make([]agentloop.Option, 0, len(o.loopOpts)+1)
	if deps.Logger != nil {
		loopOpts = append(loopOpts, agentloop.WithLogger(deps.Logger))
	}
	loopOpts = append(loopOpts, o.loopOpts...)
	loop, err := agentloop.New(agentloop.Deps{
		Models:  a.models,
		Tools:   a.tools,
		Skills:  skills,
		Memory:  mem,
		Context: a.ctxMgr,
		// A-97: no Deps.Conversation. Every run gets the Conversation of its session,
		// loaded for that turn, through agentloop.RunOptions.Conversation.
		Events:    deps.Events,
		Profile:   a.Profile,
		Trace:     deps.Trace,
		Sandbox:   sandboxes,
		Artifacts: artifacts,
		// The agent's prompt jobs, for the schedule_job and list_scheduled_jobs tools.
		// A-70: the agent's triggers, for the manage_trigger and list_triggers tools.
		// Update (A-126): the agent's jobs, for the manage_job and list_jobs tools.
		Jobs: a.jobs,
		// A-100 phase 3: a job scheduled from a chat ("remind me every morning", sent in
		// Telegram) sends its results back to that chat.
		// Update (A-127): and the home chats of the agent's channels.
		JobDeliveryOptions: a.channels.deliveryOptions,
		// Resolved on every tool batch, so a handler set on the hub after the agent was
		// spawned is picked up.
		// A-100 phase 2 - the interaction router: a channel turn's approvals and questions
		// go to its chat; every other session uses the host's handler (the web UI's broker).
		// A-70: a trigger run has nobody to ask - its approvals are denied and the run ends
		// "blocked". The order is channel, then trigger, then the host's handler.
		// A-105: a child run's session asks the parent's channel (approvals only), first.
		// (The router moved to Agent.interactionFor, which the subagent manager uses too.)
		Interaction: a.interactionFor,
		// A-135: YOLO mode skips the approval prompt for the session (a child run: its
		// parent's).
		ApprovalMode: a.approvalModeFor,
		// A-105: for spawn_subagent; a child run's session is limited to its scope
		// (read-only by default).
		Subagents: a.subagents,
		ToolScope: a.subagents.toolScope,
		// A-137: the tools handed to subagents are left out of the agent's own tool list.
		DelegatedTools: a.subagents.delegatedToolNames,
		// A-106: a group lead's hand-offs to the group's members need no extra approval.
		PreApproved: a.subagents.preApproved,
		// A-101: for the tools that use secrets; tool results and errors are redacted
		// through it.
		Secrets: a.secrets,
	}, loopOpts...)
	if err != nil {
		return nil, err
	}
	a.loop = loop
	// Started last, so a failure above leaves nothing running: the memory system is
	// started only after the loop is built.
	sys.Start(lifetime) //nolint:contextcheck // the lifetime derives from Background on purpose: it must not carry the request values of New (10-SECURITY UF-3)
	built = true
	return a, nil
}

// builtInCommandDeps (A-136) is what the built-in commands use: the agent's managers.
func (a *Agent) builtInCommandDeps() builtInCommandDeps {
	return builtInCommandDeps{
		agentName:    func() string { return a.Profile().Name },
		defaultModel: func() string { return a.Profile().LLMModel },
		listModels:   func(ctx context.Context) ([]string, bool) { return a.models.ListModels(ctx) },
		getSession:   a.conv.GetSession,
		listSessions: a.conv.ListSessions,
		messages:     a.conv.Messages,
		updateSession: func(ctx context.Context, sessionID string, fields []string, change func(*kernel.Session)) (kernel.Session, bool, error) {
			return a.conv.UpdateSession(ctx, sessionID, fields, change)
		},
		startSessionFrom: a.conv.StartSessionFrom,
		isBusy:           a.conv.IsBusy,
		approvalMode:     a.conv.ApprovalMode,
		setApprovalMode:  a.conv.SetApprovalMode,
		contextUsage:     a.ContextUsage,
		compact:          a.Compact,
		describeWorkspace: func(sessionID string) string {
			if a.sandboxes == nil {
				return "none"
			}
			if runner := a.sandboxes.DescribeRunner(sessionID); runner != "" {
				return runner
			}
			switch a.sandboxSelection().Mode {
			case sandboxhub.SelectionNone:
				return "none"
			case sandboxhub.SelectionOwn:
				return "its own sandbox"
			default:
				return "the shared sandbox"
			}
		},
		sandbox: func(ctx context.Context, sessionID string) (commandWorkspace, error) {
			v, err := a.SessionSandbox(ctx, sessionID)
			if err != nil {
				return nil, err
			}
			return v, nil
		},
		listCommands: func(ac command.AvailabilityContext) []command.Info { return a.commands.List(ac) },
	}
}

// interactionFor is the agent's interaction router (A-100 phase 2): a child run's
// session asks the parent's channel (A-105), a channel turn its chat, a job run nobody
// (A-70), and every other session the host's handler, resolved on every call so a
// handler set on the hub after the agent was spawned is picked up.
func (a *Agent) interactionFor(sessionID string) toolhub.InteractionHandler {
	if h := a.subagents.interactionFor(sessionID); h != nil {
		return h
	}
	if h := a.channels.interactionFor(sessionID); h != nil {
		return h
	}
	if h := a.jobs.interactionFor(sessionID); h != nil {
		return h
	}
	return a.toolHub.InteractionHandler()
}

// sandboxSelection is the live sandbox selection of the profile, which the
// SandboxManager reads on every GetSandbox (the profile's sandbox, "shared" when
// unset).
func (a *Agent) sandboxSelection() sandboxhub.Selection {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.profile.Sandbox.Clone()
}

// onAllowlistChange is the SkillManager hook: it applies the
// new allowlist to the agent and then saves the profile through Deps.SaveProfile,
// which the AgentCore points at its current persistence hub. Without a SaveProfile
// the change stays in memory.
func (a *Agent) onAllowlistChange(ctx context.Context, names []string) error {
	a.UpdateSkills(names)
	if a.saveProfile == nil {
		return nil
	}
	return a.saveProfile(ctx, a.Profile())
}

// memoryFiles is the memory system's view of the profile's USER.md, MEMORY.md and SOUL.md.
func (a *Agent) memoryFiles() memorysystem.CompactedMemoryFiles {
	a.mu.RLock()
	defer a.mu.RUnlock()
	var files memorysystem.CompactedMemoryFiles
	if ic := a.profile.InitialContext; ic != nil {
		userMd, memoryMd, soulMd := ic.UserMd, ic.MemoryMd, ic.SoulMd
		files.UserMd, files.MemoryMd, files.SoulMd = &userMd, &memoryMd, &soulMd
	}
	return files
}

// onMemoryFilesCompacted is the memory system hook: it applies the rebuilt USER.md,
// MEMORY.md and/or SOUL.md to the agent and then saves the profile through Deps.SaveProfile.
// Without a SaveProfile the change stays in memory.
func (a *Agent) onMemoryFilesCompacted(ctx context.Context, files memorysystem.CompactedMemoryFiles) error {
	ic := profile.InitialContext{}
	if cur := a.Profile().InitialContext; cur != nil {
		ic = *cur
	}
	if files.UserMd != nil {
		ic.UserMd = *files.UserMd
	}
	if files.MemoryMd != nil {
		ic.MemoryMd = *files.MemoryMd
	}
	if files.SoulMd != nil {
		ic.SoulMd = *files.SoulMd
	}
	a.UpdateInitialContext(&ic)
	if a.saveProfile == nil {
		return nil
	}
	return a.saveProfile(ctx, a.Profile())
}

// ID returns the agent's id, the id of its profile.
func (a *Agent) ID() string { return a.id }

// Profile returns a deep copy of the agent's profile as it is now, with the runtime
// updates applied. Changing the copy changes nothing in the agent.
func (a *Agent) Profile() profile.AgentProfile {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.profile.Clone()
}

// StartSession creates a session and makes it the agent's active one.
// (A-97: the active session is the default session of a Chat or ChatStream without
// WithChatSession; a turn that names its session never needs it.)
// WithMetadata sets the metadata stored with the session, and
// WithSessionModel the session's model override, which Chat and Complete apply when
// the options name no model.
func (a *Agent) StartSession(ctx context.Context, opts ...SessionOption) (kernel.Session, error) {
	var o sessionOptions
	for _, opt := range opts {
		if opt != nil {
			opt(&o)
		}
	}
	return a.conv.StartSession(ctx, o.metadata, o.model)
}

// LoadSession makes a stored session the active one. The bool is
// false when the store has no such session; the active session then stays as it was
// (it is not deactivated, so the next Chat does not fail with ErrNoActiveSession).
// (A-97: it only sets the default session and loads no messages; each turn reads its
// session from the stores when it starts.)
func (a *Agent) LoadSession(ctx context.Context, sessionID string) (kernel.Session, bool, error) {
	return a.conv.LoadSession(ctx, sessionID)
}

// ApprovalMode (A-135) is a session's approval mode: kernel.ApprovalModeAsk unless it
// was set to kernel.ApprovalModeYolo (always ask in a job's session and for an unknown
// one).
func (a *Agent) ApprovalMode(ctx context.Context, sessionID string) (kernel.ApprovalMode, error) {
	return a.conv.ApprovalMode(ctx, sessionID)
}

// SetApprovalMode (A-135) sets a session's approval mode - kernel.ApprovalModeYolo runs
// its calls that need approval without asking. It applies at once, also to a turn
// running in the session. The bool is false, with a nil error, when there is no such
// session; YOLO in a job's session is a kernel.SessionError with SessionCodeNotAllowed.
func (a *Agent) SetApprovalMode(ctx context.Context, sessionID string, mode kernel.ApprovalMode) (kernel.Session, bool, error) {
	return a.conv.SetApprovalMode(ctx, sessionID, mode)
}

// SetReasoningEffort (A-164) sets a session's reasoning effort ("" clears it), used from
// its next turn. The bool is false, with a nil error, when there is no such session; a
// malformed level is a kernel.SessionError with SessionCodeInvalid.
func (a *Agent) SetReasoningEffort(ctx context.Context, sessionID, effort string) (kernel.Session, bool, error) {
	return a.conv.SetReasoningEffort(ctx, sessionID, effort)
}

// approvalModeFor (A-135) is the approval mode a session's calls use - a child run's
// session uses its parent's.
func (a *Agent) approvalModeFor(ctx context.Context, sessionID string) (kernel.ApprovalMode, error) {
	if parent, ok := a.subagents.parentSessionOf(sessionID); ok {
		sessionID = parent
	}
	return a.conv.ApprovalMode(ctx, sessionID)
}

// Commands (A-136) returns this agent's slash commands: list them (help, autocomplete),
// run one in a session, and register the agent's own.
func (a *Agent) Commands() command.Commands { return a.commands }

// Compact (A-136) compacts a session's working set now (/compact), with an optional focus
// for the summary ("" for none). It waits for the session like a turn does. The bool is
// false, with a nil error, when there is no such session. A compaction emits
// SessionUpdated (["compactedMessages"]).
func (a *Agent) Compact(ctx context.Context, sessionID, focus string) (agentloop.CompactNowResult, bool, error) {
	if _, ok, err := a.conv.GetSession(ctx, sessionID); err != nil || !ok {
		return agentloop.CompactNowResult{}, false, err
	}
	runCtx, turn, err := a.conv.beginTurn(ctx, sessionID, turnOptions{})
	if err != nil {
		if errors.Is(err, ErrSessionNotFound) {
			return agentloop.CompactNowResult{}, false, nil
		}
		return agentloop.CompactNowResult{}, false, err
	}
	defer turn.release()
	result, err := a.loop.CompactNow(runCtx, turn.conversation, focus)
	if err != nil {
		return agentloop.CompactNowResult{}, true, err
	}
	if result.Compacted {
		a.conv.EmitUpdated(sessionID, []string{"compactedMessages"})
	}
	return result, true, nil
}

// ListSessions lists the sessions of the session store that match the filter.
// There is no agent filter: a session does not record its agent and
// kernel.SessionFilter selects by metadata only, so every session of the store is a
// candidate. An application that wants per-agent sessions puts the agent id in the
// session metadata (WithMetadata) and filters on it.
func (a *Agent) ListSessions(ctx context.Context, f kernel.SessionFilter) ([]kernel.Session, error) {
	return a.conv.ListSessions(ctx, f)
}

// ContextUsage (A-104) returns what fills a session's context window now, by category
// (sessionID "" = the default session). It reads the session without waiting for or
// blocking a running turn, and makes no model call. The bool is false, with a nil
// error, when there is no such session.
func (a *Agent) ContextUsage(ctx context.Context, sessionID string) (agentloop.ContextUsage, bool, error) {
	if sessionID == "" {
		id, ok := a.conv.DefaultSessionID()
		if !ok {
			return agentloop.ContextUsage{}, false, nil
		}
		sessionID = id
	}
	conv, ok, err := a.conv.Open(ctx, sessionID)
	if err != nil || !ok {
		return agentloop.ContextUsage{}, false, err
	}
	usage, err := a.loop.MeasureContext(ctx, conv)
	if err != nil {
		return agentloop.ContextUsage{}, false, err
	}
	return usage, true, nil
}

// ListProviders returns the names of the registered LLM providers.
func (a *Agent) ListProviders() []string { return a.models.ListProviders() }

// Chat runs one agent turn on the active session and returns the result.
// With no active session it fails with ErrNoActiveSession.
//
// opts are per-call model overrides (WithModel, WithTemperature, ...): the profile's
// inference config is the base layer and opts go on top, and the session's model
// applies when the merged options name none. Cancelling ctx stops the run at the next
// safe point (A-65): the result is then Aborted, with a nil error.
//
// Chat is safe to call concurrently with the other methods of the agent, and it
// serializes the chats of the agent (the caller does not have to): a Chat or ChatStream
// that starts while another chat of the agent runs waits for it, so two chats never
// interleave their messages in the one conversation. The wait honours ctx: when ctx
// ends before the turn could start, Chat returns the ctx error (context.Canceled or
// context.DeadlineExceeded) and an empty result, because nothing was run or written; a
// ctx that is already done when the agent is idle still runs the turn, which then ends
// Aborted as described above. Chat called with the ctx of a running chat of the same
// agent (from one of its tools, say) would wait for itself, so it returns
// ErrReentrantChat at once.
//
// After Dispose has started, Chat returns ErrAgentDisposed; a chat that is running
// when Dispose starts is cancelled (its result is Aborted), and Dispose waits for it.
//
// The turn is bound to the session that was active when it started: a StartSession
// or LoadSession from another goroutine while the turn runs does not move its
// remaining messages or token usage into the other session. The turn fails at its
// next write instead, with an error that matches agentloop.ErrSessionNotActive (the
// messages written before the switch stay in the session they belong to).
//
// -> Superseded (A-97): turns are serialized per session, not per agent. WithChatSession
// picks the session (default: the active session of StartSession and LoadSession).
// Turns in different sessions run in parallel; a turn in a busy session waits for it,
// in arrival order, with the same ctx and Dispose rules as above, and
// loopConfig.maxConcurrentTurns optionally caps how many turns the agent runs at once.
// ErrReentrantChat is only for a nested chat on the SAME session (it would wait for
// itself); a nested chat on another session runs. Each turn loads its session's
// conversation from the stores when it starts (an unknown session fails with an error
// that matches ErrSessionNotFound, before anything is written), so a StartSession or
// LoadSession while the turn runs changes nothing for it, and the ErrSessionNotActive
// failure described in the last paragraph no longer happens.
func (a *Agent) Chat(ctx context.Context, message string, opts ...ChatOption) (agentloop.RunResult, error) {
	o := chatOptions(opts)
	// A-136: readOnly - the turn runs in plan mode (/review).
	runCtx, turn, err := a.conv.beginTurn(ctx, o.sessionID, turnOptions{readOnly: o.readOnly})
	if err != nil {
		return agentloop.RunResult{}, err
	}
	defer turn.release()
	session, _ := turn.conversation.Session()
	// The run is bound to the session it starts on (see ConversationManager).
	// (A-97: it works in the Conversation of its session, loaded for this turn.)
	return a.loop.Run(runCtx, message, session.ID, a.mergeSessionModel(session, o.model), agentloop.RunOptions{Conversation: turn.conversation, TraceAttributes: o.traceAttributes, Attachments: o.attachments})
}

// ChatStream is Chat that yields the model's output as it arrives.
// With no active session the sequence is exactly one pair, a zero chunk and
// ErrNoActiveSession. The session check happens when the sequence is first
// iterated, not when ChatStream is called.
//
// Errors: every pair with a non-nil error is the last one of the sequence. A failure of
// the model (the provider's terminal error chunk) or the iteration cap (the loop's
// "Reached maximum reasoning steps." chunk) is yielded as the error chunk itself
// together with a *kernel.SDKError that carries the chunk's text, so a plain "if err !=
// nil" sees it and a UI can still render the chunk. The sequence ends after that pair:
// the turn is finished in the background of the call (the reply so far is saved and the
// run completes with the stop reason error, as Chat does), and nothing more is yielded,
// not even the done chunk. A precondition or port failure (no active session, a
// conversation write that failed) is a zero chunk with that error, as before.
//
// ChatStream takes part in the serialization of Chat: the sequence waits for its turn
// when its iteration starts and holds it until the iteration ends (the last pair, or
// the consumer breaking out of the loop). A sequence that is never iterated never
// holds or waits for the turn. When the wait fails (ctx ended, Dispose started, or a
// nested call) the sequence is one pair, a zero chunk and that error.
//
// A-97: ChatStream takes part in the per-session ordering of Chat (see Chat):
// WithChatSession picks the session, and the sequence waits for that session's turn
// when its iteration starts.
func (a *Agent) ChatStream(ctx context.Context, message string, opts ...ChatOption) iter.Seq2[kernel.StreamChunk, error] {
	o := chatOptions(opts)
	return func(yield func(kernel.StreamChunk, error) bool) {
		runCtx, turn, err := a.conv.beginTurn(ctx, o.sessionID, turnOptions{readOnly: o.readOnly})
		if err != nil {
			yield(kernel.StreamChunk{}, err)
			return
		}
		defer turn.release()
		session, _ := turn.conversation.Session()
		a.loop.Stream(runCtx, message, session.ID, a.mergeSessionModel(session, o.model), agentloop.RunOptions{Conversation: turn.conversation, TraceAttributes: o.traceAttributes, Attachments: o.attachments})(errorChunkYield(yield))
	}
}

// RunningTurns is how many turns of the agent run now, across all its sessions (A-97).
// A turn that waits for its session or for a slot under loopConfig.maxConcurrentTurns
// is not counted.
func (a *Agent) RunningTurns() int { return a.conv.RunningTurns() }

// maxConcurrentTurns is the live cap of the turn gate: loopConfig.maxConcurrentTurns
// of the profile as it is now (0 = no cap).
func (a *Agent) maxConcurrentTurns() int {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return profile.ResolveLoopConfig(a.profile.LoopConfig).MaxConcurrentTurns
}

// errorChunkYield wraps the consumer's yield for ChatStream: an error chunk is passed
// on with a *kernel.SDKError carrying its text, and from then on nothing reaches the
// consumer. The loop is told to go on (true) after the error chunk whatever the
// consumer answered, so it finishes the turn as it does without a consumer stop (it
// saves the reply and completes the run with the error stop reason) instead of
// cleaning it up as aborted; a model error ends the turn, so that is bounded work.
func errorChunkYield(yield func(kernel.StreamChunk, error) bool) func(kernel.StreamChunk, error) bool {
	ended := false
	return func(chunk kernel.StreamChunk, err error) bool {
		if ended {
			return true
		}
		if err == nil && chunk.Type == kernel.ChunkError {
			ended = true
			yield(chunk, kernel.NewSDKError(chunk.Error, false))
			return true
		}
		return yield(chunk, err)
	}
}

// Complete makes one model call with this agent's provider, model and inference
// defaults (the same resolution Chat uses) but outside the agent loop: it sends
// exactly msgs, with no system prompt, tools, skills, memory, conversation history
// or trace spans, and nothing is added to any session. It is for side
// tasks such as writing a session title or a summary.
//
// opts are the per-call model overrides of Chat (WithModel, WithTemperature, ...).
// WithCompleteSession applies that session's model override, as Chat does for the
// active session; it does not have to be the active one.
func (a *Agent) Complete(ctx context.Context, msgs []kernel.Message, opts ...CompleteOption) (kernel.ModelResponse, error) {
	o := completeOptions(opts)
	model := o.model
	if o.session != nil {
		model = a.mergeSessionModel(*o.session, model)
	}
	return a.models.Chat(ctx, msgs, model)
}

// mergeSessionModel layers the model options: the profile's inference config is the
// base layer, the call's options go on top, and the session's model applies only
// when the merged options carry none.
func (a *Agent) mergeSessionModel(session kernel.Session, opts llmhub.ModelOptions) llmhub.ModelOptions {
	a.mu.RLock()
	inference := a.profile.InferenceConfig
	merged := mergeInference(inference, opts)
	a.mu.RUnlock()
	if merged.Model == "" && session.LLMModel != "" {
		merged.Model = session.LLMModel
	}
	// A-164: the session's reasoning effort, unless the call set one.
	if merged.ReasoningEffort == "" {
		merged.ReasoningEffort = session.ReasoningEffort
	}
	return merged
}

// ---------------------------------------------------------------------------
// Dynamic configuration - update the agent at runtime
// ---------------------------------------------------------------------------

// UpdateTools replaces the agent's tool allowlist and propagates it to the
// ToolManager. names is copied. It does not persist the profile.
func (a *Agent) UpdateTools(names []string) {
	names = slices.Clone(names)
	a.mu.Lock()
	a.profile.Tools = slices.Clone(names)
	a.mu.Unlock()
	a.tools.SetAllowedTools(names)
}

// UpdateSkills replaces the agent's skill allowlist and propagates it to the
// SkillManager. names is copied. It does not persist the profile;
// only an allowlist change that comes from a skill the agent itself created or
// deleted is saved (through Deps.SaveProfile).
//
// The profile lock is released before the SkillManager is told, so UpdateSkills can
// be called from the skill hub's OnDeleted listener while the same agent is inside a
// SkillManager call.
func (a *Agent) UpdateSkills(names []string) {
	names = slices.Clone(names)
	a.mu.Lock()
	a.profile.Skills = slices.Clone(names)
	a.mu.Unlock()
	if a.skills != nil {
		a.skills.SetAllowedSkills(names)
	}
}

// DisableSkill takes name out of the agent's skill allowlist and saves the profile
// through Deps.SaveProfile (without one the change stays in memory). The bool is
// false when the skill was not enabled, and nothing is saved then. The
// read-modify-write and the save run under the agent's allowlist change mutex (the
// one SkillManager.Create and Delete use), so a concurrent skill change of the same
// agent can neither be lost by the removal nor overwritten by its save. The AgentCore
// calls it for every agent when a skill is deleted. The error is the save's; the
// allowlist was changed in memory all the same.
//
// It must not be called from inside the onAllowlistChange hook of the same agent (the
// mutex is held there); the skill hub's OnDeleted listener is not called there.
func (a *Agent) DisableSkill(ctx context.Context, name string) (bool, error) {
	if a.skills != nil {
		return a.skills.disable(ctx, name)
	}
	current := a.Profile().Skills
	if !slices.Contains(current, name) {
		return false, nil
	}
	return true, a.onAllowlistChange(ctx, slices.DeleteFunc(current, func(s string) bool { return s == name }))
}

// UpdateProvider changes the agent's default provider; "" means the hub's primary.
// It does not persist the profile.
func (a *Agent) UpdateProvider(name string) {
	a.mu.Lock()
	a.profile.Provider = name
	a.mu.Unlock()
	a.models.SetDefaultProvider(name)
}

// UpdateModel changes the agent's default LLM model; "" means the provider's own
// default. It does not persist the profile.
func (a *Agent) UpdateModel(model string) {
	a.mu.Lock()
	a.profile.LLMModel = model
	a.mu.Unlock()
	a.models.SetDefaultModel(model)
}

// UpdateInferenceConfig changes the agent's default inference config
// (maxTokens, temperature, topP, stopSequences); c is copied and nil clears it. It
// does not persist the profile.
func (a *Agent) UpdateInferenceConfig(c *profile.InferenceConfig) {
	c = cloneInferenceConfig(c)
	a.mu.Lock()
	a.profile.InferenceConfig = cloneInferenceConfig(c)
	a.mu.Unlock()
	a.models.SetDefaultInferenceConfig(c)
}

// UpdateLoopConfig changes the agent's loop limits (maxIterations, maxToolRetries,
// retryBackoffMs); c is copied and nil restores the defaults. The loop reads the
// profile at the start of every run, so it applies to the next run. It does not
// persist the profile.
func (a *Agent) UpdateLoopConfig(c *profile.LoopConfig) {
	c = cloneLoopConfig(c)
	a.mu.Lock()
	a.profile.LoopConfig = c
	a.mu.Unlock()
}

// UpdateMemoryConfig replaces the agent's memory config at runtime
// and re-arms the consolidation scheduler: an interval that
// became positive starts the scheduler's goroutine, a changed one restarts its
// period and a disabled one stops it. c is copied. It does not persist the profile.
func (a *Agent) UpdateMemoryConfig(c memory.Config) {
	c = c.Clone()
	a.mu.Lock()
	a.profile.Memory = c.Clone()
	a.mu.Unlock()
	// Outside the profile lock, so the scheduler never waits for it.
	a.memory.UpdateConfig(c)
}

// UpdateInitialContext replaces the agent's initial context files (USER.md, MEMORY.md,
// SOUL.md); it applies to the next model call. ic is copied and nil clears them. It
// does not persist the profile.
func (a *Agent) UpdateInitialContext(ic *profile.InitialContext) {
	if ic != nil {
		c := *ic
		ic = &c
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.profile.InitialContext = ic
	// Under the profile lock, so two updates reach the context manager in order.
	a.ctxMgr.RefreshProfile(a.profile)
}

// UpdateSubagents replaces who the agent may delegate to and its subagent budget
// (A-105); it applies to the next spawn. c is copied and nil means no subagents. It
// does not persist the profile.
func (a *Agent) UpdateSubagents(c *subagent.Config) {
	c = c.Clone()
	a.mu.Lock()
	a.profile.Subagents = c
	a.mu.Unlock()
}

// UpdateSecrets replaces which global secrets the agent may use (A-101); it applies to
// the next tool call. c is copied and nil means only its own. It does not persist the
// profile.
func (a *Agent) UpdateSecrets(c *secret.Config) {
	c = c.Clone()
	a.mu.Lock()
	a.profile.Secrets = c
	a.mu.Unlock()
}

// ---------------------------------------------------------------------------
// Sandbox - the agent's place for file and terminal work
// ---------------------------------------------------------------------------

// errNoSandboxHub is returned by Sandbox and UpdateSandbox when the agent was built
// without a sandbox hub (Deps.SandboxHub nil).
func errNoSandboxHub() error {
	return kernel.NewSandboxError("This agent was built without a sandbox hub", kernel.SandboxCodeNotConfigured, "")
}

// Sandbox returns the agent's sandbox view, creating the sandbox on first use.
// The profile's selection decides: the shared sandbox in the agent's own
// folder by default, the agent's own sandbox when the profile asks for one, and a
// fail-closed *kernel.SandboxError for "none" or an unconfigured shared sandbox. A
// tool gets the same view through toolhub.Context.GetSandbox.
func (a *Agent) Sandbox(ctx context.Context) (sandboxhub.View, error) {
	if a.sandboxes == nil {
		return nil, errNoSandboxHub()
	}
	return a.sandboxes.GetSandbox(ctx)
}

// SessionSandbox (A-153) is Sandbox for one session: the folder on the user's machine
// when the user attached a runner to the session (sandboxhub.SandboxHub.AttachRunner),
// else the agent's sandbox. The session's workspace tools get the same view.
func (a *Agent) SessionSandbox(ctx context.Context, sessionID string) (sandboxhub.View, error) {
	if a.sandboxes == nil {
		return nil, errNoSandboxHub()
	}
	return a.sandboxes.GetSessionSandbox(ctx, sessionID)
}

// UpdateSandbox changes where the agent's file and terminal tools run.
// The selection is validated against the hub policy first: a
// refused selection returns the *kernel.SandboxError and leaves the profile as it
// was. It applies to the next tool call; an own sandbox is created lazily. sel is
// copied, and the zero Selection means the shared sandbox. It does not persist the
// profile.
func (a *Agent) UpdateSandbox(sel sandboxhub.Selection) error {
	if a.sandboxes == nil {
		return errNoSandboxHub()
	}
	if err := a.sandboxes.Validate(sel); err != nil {
		return err
	}
	sel = sel.Clone()
	a.mu.Lock()
	a.profile.Sandbox = sel
	a.mu.Unlock()
	return nil
}

// ---------------------------------------------------------------------------
// Artifacts - lasting, versioned outputs this agent made (A-62)
// ---------------------------------------------------------------------------

// Artifacts returns this agent's artifact view. sessionID is stamped
// on the versions written through it; reads never depend on it, because artifacts
// outlive the session that made them. It returns the error of the artifact hub's
// ForAgent directly; the loop's own artifact port instead returns a view that fails
// at use.
func (a *Agent) Artifacts(sessionID string) (artifact.View, error) {
	if a.artifacts == nil {
		return nil, kernel.NewArtifactError("This agent was built without an artifact hub", kernel.ArtifactCodeNotConfigured, "")
	}
	return a.artifacts.Artifacts(sessionID)
}

// ---------------------------------------------------------------------------
// Prompt jobs (A-69) - a stored prompt + schedule; each run is a full agent turn in
// a session the user can open later
// Update (A-126): jobs - on a schedule, an event or a webhook (A-70 triggers merged in)
// ---------------------------------------------------------------------------

// Jobs returns the agent's jobs: configure, run now, test events, webhook intake, event
// and run history. They are kept in the persistence hub's JobStore.
func (a *Agent) Jobs() job.Jobs { return a.jobs }

// StartJobs loads the agent's stored prompt jobs and arms their timers. Call it once the
// host is ready to run turns (tools registered, runner set); AgentCore.StartJobs
// does it for every agent. A job whose time passed while the host was down runs once.
// Update (A-126): it also starts working the job queue - due runs, retries, and runs a
// previous process left behind (their leases expire). Idempotent; after Dispose it does
// nothing.
func (a *Agent) StartJobs(ctx context.Context) error {
	return a.jobs.start(ctx) //nolint:contextcheck // the worker runs under the agent's lifetime, not the caller's ctx (10-SECURITY UF-3)
}

// runJobTurn is the default prompt-job runner, used when the host set none: one turn in
// the run's session, one prompt job at a time. It does not coordinate with Chat calls
// the host makes itself (LoadSession switches the active session), so a host that chats
// concurrently supplies a runner that shares its own run lock
// (AgentCore.UseScheduledJobRunner). Update (A-126): the default job runner, for every
// job run (AgentCore.UseJobRunner).
//
// -> Superseded (A-97): runs are no longer serialized here (jobTurn) and the run no
// longer has to LoadSession first. The turn names its session (WithChatSession) and
// the agent's turn gate orders it with every other turn of that session, Chat and
// ChatStream included, while runs in other sessions go in parallel. A host still
// supplies its own runner when a run must also take its own bookkeeping (a stop
// registry, traces, usage) or its own per-session lock.
func (a *Agent) runJobTurn(ctx context.Context, rc job.RunContext) (job.RunResult, error) {
	result, err := a.Chat(ctx, rc.Prompt, WithChatSession(rc.SessionID))
	if err != nil {
		// A run stopped while it waited for its session is a stopped run, not a failure.
		if ctx.Err() != nil && errors.Is(err, ctx.Err()) {
			return job.RunResult{Aborted: true}, nil
		}
		return job.RunResult{}, err
	}
	out := job.RunResult{
		Output:  result.Message.Content,
		Usage:   &job.Usage{InputTokens: result.Usage.InputTokens, OutputTokens: result.Usage.OutputTokens},
		Aborted: result.Aborted,
	}
	if result.StopReason == kernel.StopMaxTokens || result.StopReason == kernel.StopError {
		out.StopReason = string(result.StopReason)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Subagents (A-105) - bounded tasks this agent hands to other agents
// ---------------------------------------------------------------------------

// Subagents returns this agent's child runs: targets, run records, stop. The records are
// kept in the persistence hub's SubagentRunStore.
func (a *Agent) Subagents() subagent.Subagents { return a.subagents }

// ---------------------------------------------------------------------------
// Secrets (A-101) - keys the agent's tools use without the model seeing them
// ---------------------------------------------------------------------------

// Secrets returns this agent's secrets: list (metadata only), its own secrets' CRUD,
// redaction. The values live sealed in the host's SecretHub.
func (a *Agent) Secrets() secret.Secrets { return a.secrets }

// ---------------------------------------------------------------------------
// Channels (A-100) - chat apps (Telegram, your own app...) users talk to this agent through
// ---------------------------------------------------------------------------

// Channels returns the agent's channels: connect, edit, approve senders, send. They are
// kept in the persistence hub's ChannelStore.
func (a *Agent) Channels() channel.Channels { return a.channels }

// StartChannels connects the agent's stored, enabled channels. Call it once the host is
// ready to run turns (tools registered, platforms and runner set);
// AgentCore.StartChannels does it for every agent. A channel that cannot connect shows
// "failed" in its State. Idempotent; after Dispose it does nothing.
func (a *Agent) StartChannels(ctx context.Context) error { return a.channels.start(ctx) }

// runChannelTurn is the default channel runner, used when the host set none: one turn in
// the chat's session.
// -> Phase 2: streamed (ChatStream + channel.TurnFromStream), so the chat shows the reply
// while it is written.
func (a *Agent) runChannelTurn(ctx context.Context, tc channel.TurnContext) (channel.TurnResult, error) {
	opts := []ChatOption{WithChatSession(tc.SessionID)}
	// A-136: readOnly - a /review sent from the chat.
	if tc.ReadOnly {
		opts = append(opts, WithChatReadOnly())
	}
	out, err := channel.TurnFromStream(a.ChatStream(ctx, tc.Text, opts...), tc.OnChunk)
	if err != nil {
		// A turn stopped while it waited for its session is a stopped turn, not a failure.
		if ctx.Err() != nil && errors.Is(err, ctx.Err()) {
			return channel.TurnResult{Aborted: true}, nil
		}
		return channel.TurnResult{}, err
	}
	if out.StopReason != string(kernel.StopMaxTokens) && out.StopReason != string(kernel.StopError) {
		out.StopReason = ""
	}
	return out, nil
}

// Dispose stops the agent and keeps its data; Delete is Dispose for an agent that is
// being deleted. The order is: refuse new chats (Chat and ChatStream return
// ErrAgentDisposed, also those that were waiting for their turn), cancel the chat that is
// running and wait for it to end; stop every scheduled job (a run that is executing is
// not aborted); stop the memory system, which waits for the consolidation pass in flight
// and never starts a final one; wait for the scheduled job runs in flight; release the
// agent's own sandbox (its files are kept by Dispose and deleted by Delete); then cancel
// the lifetime context. A step that fails does not stop the ones after it, and the errors
// are joined with errors.Join.
//
// The waits honour ctx, but the sandbox release is a cleanup that must not be skipped
// because ctx already ended (a container left running would leak): it runs under
// context.WithoutCancel(ctx) bounded by its own timeout (DisposeCleanupTimeout), so it
// keeps the values of ctx and not its deadline.
//
// Dispose is mandatory: without it the goroutines of the memory system and of the
// started jobs, and the bus subscription, live as long as the process. ctx bounds the
// waits: when it ends while a pass or a job run is still going Dispose returns the ctx
// error, and the lifetime context is cancelled all the same, so the pass and the run
// see the cancellation. A pass or a job run that ignores its context therefore delays
// the end of that work, not Dispose. Once Dispose has started, StartScheduledJob
// returns an error.
//
// Dispose is safe to call more than once, also concurrently, and a later call is not
// a blind no-op. A call that finds another one running waits for it, or returns the
// ctx error when its own ctx ends first. A call after the first one finished does
// nothing more, except that a Delete still removes the agent's own sandbox
// data when no earlier call has done so: the first Dispose may have kept the data, or
// failed to remove it (Docker was down), and RemoveAgent must be able to finish the
// job.
//
// A job must not wait for its own agent: Dispose called with the ctx a job run
// received (or one derived from it) does not wait for the job runs, which would be a
// wait for itself. It stops the jobs, cancels the lifetime context, which ends the
// run's ctx, and returns; the run ends when its Execute returns. Dispose called from
// inside a job with a ctx that is not derived from the run's would wait for that run
// until its ctx ends, so pass the ctx of Execute. The same holds for a chat: Dispose
// called with the ctx of a running chat of this agent (from one of its tools) neither
// cancels that chat early nor waits for it; the chat's ctx ends with the lifetime
// context at the end of Dispose.
func (a *Agent) Dispose(ctx context.Context) error {
	return a.dispose(ctx, false)
}

// Delete is Dispose for an agent that is being deleted (AgentCore.RemoveAgent uses
// it): it does everything Dispose does, and the release of the agent's own sandbox
// deletes its files too, including the cold data of every own-sandbox kind the agent
// may have used. It changes nothing for an agent whose sandbox is shared or none. A
// Delete after a Dispose, or after a Delete that failed to remove the data, still
// removes it; see Dispose.
func (a *Agent) Delete(ctx context.Context) error {
	return a.dispose(ctx, true)
}

// dispose is Dispose and Delete; removeData says the agent is being deleted.
func (a *Agent) dispose(ctx context.Context, removeData bool) error {
	a.disposeMu.Lock()
	if a.disposeDone != nil {
		done := a.disposeDone
		a.disposeMu.Unlock()
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
		return a.removeDataAgain(ctx, removeData)
	}
	done := make(chan struct{})
	a.disposeDone = done
	a.disposeMu.Unlock()
	defer close(done)

	var errs []error
	// A-100: channels before chats, so no chat-app message starts a turn from here on;
	// their turns are cancelled and waited for. Not from inside a channel turn of this
	// agent: that would wait for itself.
	if err := a.channels.close(ctx, ctx.Value(channelTurnKey{}) != a.channels); err != nil {
		errs = append(errs, err)
	}
	// A-70: trigger intake stops and the runs in progress are cancelled and given back to
	// the queue (taken up after a restart), and waited for. Not from inside a trigger run
	// of this agent: that would wait for itself.
	// Update (A-126): the job manager - its clock and intake stop, its runs (scheduled
	// ones too) are given back and waited for.
	if err := a.jobs.close(ctx, ctx.Value(jobRunKey{}) != a.jobs); err != nil {
		errs = append(errs, err)
	}
	// A-105: stops its running children (they record themselves as cancelled) and its bus
	// subscriptions.
	a.subagents.dispose()
	// Chats first: no chat starts from here on, the waiting ones give up, and
	// the running one is cancelled and waited for (A-97: every running one, in any
	// session). Not from inside that chat: it would
	// wait for itself, and cancelling its ctx now would fail every wait below at once.
	// (The steps moved to ConversationManager.closeTurns with the turn bookkeeping.)
	if err := a.conv.closeTurns(ctx); err != nil {
		errs = append(errs, err)
	}
	// Jobs next: no new run starts from here on. A run that is already
	// executing is not aborted; it is waited for below.
	// Update (A-126): scheduled tasks removed; the job manager is closed above.
	// Prompt jobs (A-69): their timers stop, their runs are cancelled (a run may wait for
	// a host lock the caller of Dispose holds) and waited for. Not from inside a run of
	// this agent: that would wait for itself.
	// Update (A-126): closed with the job manager above.
	if err := a.memory.Dispose(ctx); err != nil {
		errs = append(errs, err)
	}
	// A running job is waited for. A run still executing would
	// outlive the agent (and goleak), so it is waited for until ctx ends, and it runs
	// under the lifetime context, which is cancelled last. Not from inside a run of
	// this agent: that would wait for itself (WR-A07).
	// Update (A-126): scheduled tasks removed; job runs are waited for by jobs.close.
	if a.sandboxes != nil {
		cleanup, cancel := cleanupContext(ctx)
		err := a.sandboxes.Release(cleanup, removeData)
		cancel()
		if err != nil {
			errs = append(errs, err)
		} else if removeData {
			a.markDataRemoved()
		}
	}
	a.conv.cancelTurns()
	a.cancelLifetime()
	return errors.Join(errs...)
}

// DisposeCleanupTimeout bounds each cleanup step of Agent.Dispose (the release of the
// own sandbox): the step runs under context.WithoutCancel of the Dispose ctx, so an
// expired ctx does not make it fail at once and leak the container, and under this
// timeout, so a provider that hangs cannot hang Dispose for ever.
const DisposeCleanupTimeout = 30 * time.Second

// cleanupContext is the ctx of one cleanup step: the values of ctx, not its
// cancellation, bounded by DisposeCleanupTimeout.
func cleanupContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), DisposeCleanupTimeout)
}

func (a *Agent) markDataRemoved() {
	a.disposeMu.Lock()
	a.dataRemoved = true
	a.disposeMu.Unlock()
}

// removeDataAgain is the part of a repeated Dispose that is not a no-op: removing the
// own sandbox data when this call asks for it and none succeeded yet. The sandbox
// manager is closed and has nothing running, so Release only removes the cold data.
func (a *Agent) removeDataAgain(ctx context.Context, removeData bool) error {
	if !removeData || a.sandboxes == nil {
		return nil
	}
	a.disposeMu.Lock()
	removed := a.dataRemoved
	a.disposeMu.Unlock()
	if removed {
		return nil
	}
	cleanup, cancel := cleanupContext(ctx)
	defer cancel()
	if err := a.sandboxes.Release(cleanup, true); err != nil {
		return err
	}
	a.markDataRemoved()
	return nil
}
