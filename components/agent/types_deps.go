package agent

import (
	"context"
	"log/slog"

	"github.com/Phong95/aec-agent-go/components/agent/command"
	"github.com/Phong95/aec-agent-go/components/agent/job"
	"github.com/Phong95/aec-agent-go/components/agent/profile"
	"github.com/Phong95/aec-agent-go/components/agent/subagent"
	"github.com/Phong95/aec-agent-go/components/artifacthub"
	"github.com/Phong95/aec-agent-go/components/channelplatformhub"
	"github.com/Phong95/aec-agent-go/components/channelplatformhub/channel"
	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/components/persistence"
	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/components/secrethub"
	"github.com/Phong95/aec-agent-go/components/skillhub"
	"github.com/Phong95/aec-agent-go/components/subagenthub"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/components/trace"
)

// Deps is what an Agent needs from the AgentCore that spawns it. LLMHub, ToolHub,
// PersistenceHub and Events are required; Trace, SkillHub, SaveProfile and Logger are optional.
type Deps struct {
	// LLMHub is the hub of LLM providers the agent's ModelManager resolves from.
	LLMHub *llmhub.LLMHub
	// ToolHub is the hub of tools the agent's ToolManager resolves from, lazily on
	// every access, so a tool registered after the agent is spawned is visible.
	ToolHub *toolhub.ToolHub
	// PersistenceHub is the persistence hub that is current at spawn. The agent keeps
	// THIS hub for its conversation (the ConversationManager captures it when it is
	// built): a later AgentCore.UsePersistence does not reach a live agent. A caller
	// that plugs a PersistenceHub must not mutate its exported store fields afterwards, because
	// the agents spawned before the change would see the change only partly.
	PersistenceHub *persistence.PersistenceHub
	// Events receives the loop's events.
	Events events.Bus
	// Trace records spans. Nil means no tracing.
	Trace *trace.Service
	// SkillHub is the skill library the agent's SkillManager resolves from, lazily on
	// every access. Optional: nil means the agent has no skills (no index, no
	// skill_view tool, no toolhub.Context.Skills).
	SkillHub *skillhub.SkillHub
	// SaveProfile persists the agent's profile after a skill the agent created or
	// deleted changed its allowlist (the agent store is read at call time). The
	// AgentCore passes a closure over its CURRENT persistence hub, not the hub snapshot
	// in PersistenceHub, so a save after UsePersistence reaches the new hub. Optional: nil
	// means the change is applied in memory only. The ctx is the one of the call that
	// made the change.
	SaveProfile func(ctx context.Context, p profile.AgentProfile) error
	// SandboxHub is the sandbox hub the agent's SandboxManager resolves from: the
	// shared sandbox through the hub, an own sandbox through the manager. Optional:
	// nil means the agent has no sandbox wiring, so tools see no GetSandbox and
	// Agent.Sandbox and Agent.UpdateSandbox fail with a not_configured error.
	SandboxHub *sandboxhub.SandboxHub
	// ArtifactHub is the artifact hub the agent's ArtifactManager hands views from.
	// Optional: nil means tools see no GetArtifacts and Agent.Artifacts fails with a
	// not_configured error.
	ArtifactHub *artifacthub.ArtifactHub
	// Logger receives the agent's warnings. Nil means slog.Default().
	Logger *slog.Logger
	// JobRunner returns the runner of the agent's prompt-job turns (A-69). It is called
	// on every run, so a runner set on the AgentCore after the agent was spawned is used.
	// Optional: nil, or a nil runner, means the agent's default runner, which does not
	// share a lock with chats the host makes itself.
	// Update (A-126): the runner of every job run (schedule, event, webhook) - TriggerRunner
	// merged in.
	JobRunner func() job.Runner
	// ChannelPlatformHub is the chat platforms the agent's channels connect through (A-100),
	// shared by every agent of the AgentCore and read on every connect. Optional: nil
	// means none is registered, so the agent's channels cannot connect.
	ChannelPlatformHub *channelplatformhub.ChannelPlatformHub
	// ChannelRunner returns the runner of the agent's channel turns (A-100). It is called
	// on every turn. Optional: nil, or a nil runner, means the agent's own Chat, which
	// takes none of the host's bookkeeping (its lock, stop registry, usage).
	ChannelRunner func() channel.TurnRunner
	// Agents (A-105) is the agents a subagent can be - AgentCore passes its orchestration.
	// Optional: nil means the agent cannot delegate.
	Agents SubagentHost
	// SubagentRunner returns the runner of child turns (A-105). It is called on every child
	// run. Optional: nil, or a nil runner, means the child agent's own Chat, which takes
	// none of the host's bookkeeping (its lock, stop registry, usage).
	SubagentRunner func() subagent.Runner
	// SubagentHub (A-105 revision) is the library of subagent types the agent's enabled
	// types (AgentProfile.Subagents.Types) resolve from, on every spawn - AgentCore passes
	// its own. Optional: nil means the agent has no subagent types.
	SubagentHub *subagenthub.SubagentHub
	// SecretHub (A-101) is the host's secrets - sealed keys the agent's tools use without
	// the model seeing them (AgentCore passes its own). Optional: nil means no secrets.
	SecretHub *secrethub.SecretHub
	// HostCommands (A-136) returns the host's slash commands (AgentCore.UseCommand), read
	// on every call so a command added after the agent was spawned applies. Optional: nil
	// means none.
	HostCommands func() []command.HostCommand
}

// SubagentHost is the agents a parent can reach (A-105): orchestration.Orchestration,
// which AgentCore passes as Deps.Agents, satisfies it.
type SubagentHost interface {
	// Get returns the live agent registered under id.
	Get(id string) (*Agent, bool)
}

// Disposing an agent is two methods: Agent.Dispose keeps the agent's data and
// Agent.Delete removes it too. Delete says the agent is being deleted: its own
// sandbox's files are deleted too, including the cold data of every own-sandbox kind
// the agent may have used. It has no effect for an agent whose sandbox is shared or
// none.
