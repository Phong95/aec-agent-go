package agentloop

import (
	"context"
	"iter"

	"github.com/Phong95/aec-agent-go/components/agent/conversation"
	"github.com/Phong95/aec-agent-go/components/agent/job"
	"github.com/Phong95/aec-agent-go/components/agent/subagent"
	"github.com/Phong95/aec-agent-go/components/channelplatformhub/channel"
	"github.com/Phong95/aec-agent-go/components/secrethub/secret"
	"github.com/Phong95/aec-agent-go/components/variablehub/variable"

	"github.com/Phong95/aec-agent-go/components/agent/memorysystem/memory"
	"github.com/Phong95/aec-agent-go/components/agent/profile"
	"github.com/Phong95/aec-agent-go/components/artifacthub/artifact"
	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/components/trace"
	"github.com/Phong95/aec-agent-go/kernel"
)

// The ports below are small consumer-side interfaces: the loop declares exactly
// what it needs from the rest of the SDK, and the Phase 10 managers (and the
// aecmock fakes) satisfy them. They exist so that agentloop imports neither the
// persistence package nor a concrete tool hub.

// Models is the model access of the loop (chat and stream).
//
// Stream keeps the llmhub.Provider contract: a provider failure is ONE terminal
// chunk of type error with a non-empty Error, yielded with a nil error, after
// which the sequence ends; the error slot carries only the ctx error, yielded
// once on cancellation. Chat returns failures as *kernel.ModelError and the bare
// ctx error on cancellation.
//
// An implementation may also implement OptionResolver and ProviderNamer, which
// the loop finds by type assertion (the llmhub.ModelLister precedent).
type Models interface {
	Chat(ctx context.Context, msgs []kernel.Message, opts llmhub.ModelOptions) (kernel.ModelResponse, error)
	Stream(ctx context.Context, msgs []kernel.Message, opts llmhub.ModelOptions) iter.Seq2[kernel.StreamChunk, error]
}

// OptionResolver is the optional tracing-only side of Models: it returns the options
// after the manager's own defaults merge, which is what the model-call span records as
// the effective options. Without it the options are recorded as they are.
// Since A-88 it is not tracing-only: the loop also reads the resolved MaxTokens from it
// for the output-token-limit tool error and the consecutive-truncation message.
type OptionResolver interface {
	ResolveOptions(opts llmhub.ModelOptions) llmhub.ModelOptions
}

// VisionChecker (A-180) is the optional side of Models that says whether the model a call
// with opts goes to can see images: false only when its stored record says it can't, true
// when it can or reports nothing. Without it every model counts as able. The loop sends
// tool-result images, and offers tools with toolhub.Definition.NeedsVision, only when true.
type VisionChecker interface {
	AcceptsImages(ctx context.Context, opts llmhub.ModelOptions) bool
}

// ProviderNamer is the optional tracing-only side of Models: it names the provider
// a call would use. An error (for example when no provider is registered) means no
// provider attribute.
type ProviderNamer interface {
	ResolveProviderName(provider string) (string, error)
}

// Tools is the agent's tool access.
//
// List is pre-filtered by the agent's tool list: it returns the definitions of the
// registry's tools among profile.Tools. Authorize is the tool lookup plus the hub's
// policy: a name outside the agent's tool list returns an error that matches
// toolhub.ErrNotFound and carries the text `Tool "x" not available for this agent`; a
// name the agent may use is authorized through the hub with
// the agent's tool list. A denial is the returned Decision with Allowed false and
// a nil error, exactly like toolhub.ToolHub.Authorize.
type Tools interface {
	List() []toolhub.Definition
	Authorize(ctx context.Context, name string, args kernel.Arguments, tc toolhub.Context) (toolhub.Authorized, toolhub.Decision, error)
}

// SessionTools is the optional session side of Tools (A-106), found by type assertion:
// ListFor is List plus the tools the session is granted beyond the agent's tool list (a
// group lead's session is granted spawn_subagent while it leads). Authorize already
// sees the session (toolhub.Context.SessionID). Without it, List is used for every
// session.
type SessionTools interface {
	ListFor(sessionID string) []toolhub.Definition
}

// ToolScope (A-209, ToolScope of core/ts) is a session's tool scope. With a nil call it
// says whether the session sees the tool at all (the tool list); with a call - the call's
// own risk and arguments - whether this call may run (a read-only child may save an
// artifact but not update one).
type ToolScope func(def toolhub.Definition, call *ScopeCall) bool

// ScopeCall (A-209) is the call a ToolScope judges at dispatch: its own risk (the
// handler's RiskFor when it is a toolhub.CallRisker, else the definition's Risk) and its
// arguments.
type ScopeCall struct {
	Risk toolhub.Risk
	Args kernel.Arguments
}

// CallRisks is the optional part of Tools that gives the risk of one call before it is
// authorized (A-209), found by type assertion: the tool scope of a subagent judges a call
// before the policy runs. Without it, the scope sees the definition's Risk.
type CallRisks interface {
	CallRisk(name string, args kernel.Arguments) (toolhub.Risk, bool)
}

// ToolLookup is the optional part of Tools that finds one of the session's tools by name,
// also one the tool list leaves out (A-213: an MCP tool tool search defers), found by
// type assertion. The output limit reads the tool's MaxResultChars from it.
type ToolLookup interface {
	Lookup(sessionID, name string) (toolhub.Definition, bool)
}

// listTools is the session's tools: SessionTools.ListFor when t has it, List otherwise.
func listTools(t Tools, sessionID string) []toolhub.Definition {
	if st, ok := t.(SessionTools); ok {
		return st.ListFor(sessionID)
	}
	return t.List()
}

// Skills is the agent's skill access. It is optional in Deps:
// an agent without skills passes nil.
//
// Tool returns the skill tool (skill_view) registered under name. ToolDefinitions
// returns the definitions of those skill tools, appended to the agent's own tool
// definitions for the model.
type Skills interface {
	Tool(name string) (toolhub.Handler, bool)
	ToolDefinitions(ctx context.Context) ([]toolhub.Definition, error)
}

// Context is the context manager: it runs the context middlewares, in priority
// order, around the model call.
//
// PrepareMessages builds the model input from the conversation messages (the
// system prompt and the always-loaded context first, then the messages, then
// every BeforeModel hook). ProcessResponse runs every AfterModel hook on a
// response. Run and Stream both call both; Stream calls ProcessResponse on the
// collected response.
type Context interface {
	PrepareMessages(ctx context.Context, msgs []kernel.Message, mc MiddlewareContext) ([]kernel.Message, error)
	ProcessResponse(ctx context.Context, resp kernel.ModelResponse, mc MiddlewareContext) (kernel.ModelResponse, error)
}

// Conversation is the active conversation. The loop
// reads the session and the model's working set from it and writes every message
// back through it.
//
// A-97: a run takes its Conversation from RunOptions.Conversation when the caller
// passes one (the agent loads one per turn and session, so turns of different
// sessions run in parallel), and from Deps.Conversation otherwise.
//
// Every write runs under context.WithoutCancel of the caller's ctx plus the
// persist timeout, so an implementation sees a ctx that is never cancelled by the
// caller but may carry a deadline.
//
// UpdateSession merges CurrentTokenUsage and CounterCalibration of s into the
// active session and persists it (the loop runs on a session snapshot, so it hands
// the snapshot back through this method). The loop calls it right after the
// AddMessage that follows each usage record, so an implementation that bumps and
// saves its own session copy inside AddMessage is not overwritten by a stale one.
// Phase 10's ConversationManager must honour this contract.
type Conversation interface {
	// Session returns the active session, or false when there is none.
	Session() (kernel.Session, bool)
	// LLMMessages returns the model's working set: the compacted messages when a
	// compaction happened, the full history otherwise.
	LLMMessages() []kernel.Message
	// AddMessage appends a message to the history (and to the compacted working
	// set when there is one), stamping the timestamp when it is empty.
	AddMessage(ctx context.Context, m kernel.Message) error
	// SetCompactedMessages replaces the model's working set after a compaction.
	SetCompactedMessages(ctx context.Context, msgs []kernel.Message) error
	// UpdateSession merges CurrentTokenUsage and CounterCalibration into the
	// active session and persists it.
	UpdateSession(ctx context.Context, s kernel.Session) error
}

// Memory is the agent's long-term memory as far as the loop uses it (the per-turn
// recall). It is optional in Deps: an agent without a memory system, or whose
// memory system cannot recall per turn, passes nil, and the run then starts no
// memory-recall span.
//
// RecallForTurn is called once per run with the user's message and the session id,
// under the caller's ctx. A failure never fails the turn: the loop records it and
// carries on without memories. The recall types live in package memory.
type Memory interface {
	RecallForTurn(ctx context.Context, userMessage, sessionID string) (memory.RecalledMemories, error)
}

// MemoryTools is the optional part of a Memory that offers tools to the model
// (memory_search: recall on demand). Like the skill tools, they are added to the
// model's tools every call and run directly, without the policy or the approval gate:
// they only read the agent's own memory.
type MemoryTools interface {
	Tool(name string) (toolhub.Handler, bool)
	ToolDefinitions(ctx context.Context) ([]toolhub.Definition, error)
}

// Sandboxes is the agent's sandbox access (the two methods of the sandbox manager
// the loop uses). It is optional in Deps: an agent without sandbox wiring passes
// nil, and tools then see no GetSandbox.
//
// GetSandbox resolves the agent's sandbox lazily and returns an error when the
// agent has none (fail closed). RequiresApproval says whether an operation needs
// the user's approval in a sandbox with these traits; sandboxhub.SandboxHub.RequiresApproval
// has exactly this shape.
type Sandboxes interface {
	GetSandbox(ctx context.Context) (sandboxhub.View, error)
	RequiresApproval(traits sandboxhub.Traits, op sandboxhub.Operation) bool
}

// SessionSandboxes is the optional session side of Sandboxes (A-153), found by type
// assertion: GetSessionSandbox is the session's workspace - the folder on the user's
// machine when the user attached a runner to the session, else GetSandbox. Workspace
// tools (toolhub.Definition.SessionWorkspace) use it; without it they use GetSandbox.
type SessionSandboxes interface {
	GetSessionSandbox(ctx context.Context, sessionID string) (sandboxhub.View, error)
}

// Deps is everything a Loop needs.
type Deps struct {
	// Models, Tools, Context and Conversation are required.
	// -> Amended (A-97): Conversation is optional. Nil means every run names its
	// conversation in RunOptions.Conversation; a run with neither fails with
	// ErrNoActiveSession.
	Models Models
	Tools  Tools
	Skills Skills // optional: nil means the agent has no skills
	// Memory recalls long-term memories once per run. Optional: nil means the agent
	// has no memory system, or one that cannot recall per turn.
	Memory       Memory
	Context      Context
	Conversation Conversation
	// Events receives the loop's events. Required.
	Events events.Bus
	// Profile returns the agent's profile. The loop calls it at the start of
	// every run and reads the agent id and the loop limits from the result, so a
	// profile updated between two runs applies to the next one. Required.
	Profile func() profile.AgentProfile
	// Trace records spans. Nil means no tracing.
	Trace *trace.Service
	// Interaction returns the host's channel to the human user (clarify and
	// approval), resolved on every tool batch so a handler registered after the
	// agent was built is still picked up. Nil, or a nil
	// result, means there is no interactive user: consequential tools are denied
	// and clarify reports an error.
	// A-100 phase 2: called with the run's session id, so the agent can route a channel
	// turn's approvals and questions to its chat and every other session to the host's
	// handler.
	Interaction func(sessionID string) toolhub.InteractionHandler
	// ApprovalMode (A-135) is the session's approval mode, read at every call that needs
	// approval (so a switch during a turn applies to its next call).
	// kernel.ApprovalModeYolo runs the call without asking the interaction handler. Nil,
	// or an error, means kernel.ApprovalModeAsk.
	ApprovalMode func(ctx context.Context, sessionID string) (kernel.ApprovalMode, error)
	// Sandbox resolves the agent's sandbox for file and terminal tools and answers
	// the sandbox-aware approval question. Nil means the agent has no sandbox.
	Sandbox Sandboxes
	// Artifacts returns the artifact view of a session for the artifact tools. Nil
	// means the host wired no artifact store.
	Artifacts func(sessionID string) artifact.View
	// ScheduledJobs is the agent's prompt jobs (A-69) for the scheduled-job tools. Nil
	// means none.
	// Triggers is the agent's triggers (A-70), handed to tools as toolhub.Context.Triggers.
	// Nil means none.
	// Update (A-126): Jobs is the agent's jobs of every kind, handed to tools as
	// toolhub.Context.Jobs for manage_job / list_jobs. Nil means none.
	Jobs job.Jobs
	// JobDelivery (A-100 phase 3) returns the delivery of a job scheduled from the
	// session's turn: its chat while a channel turn runs in it, nil otherwise. Resolved
	// on every tool batch. Nil means jobs are never delivered by default.
	// Update (A-127): JobDeliveryOptions - that chat, and the home chats of the agent's
	// channels (where a job made outside a chat app reports).
	JobDeliveryOptions func(sessionID string) job.DeliveryOptions
	// Channels (A-219) is the channel tool's port, handed to tools as
	// toolhub.Context.Channels. Nil means none.
	Channels channel.ToolPort
	// Conversations (A-220) is the conversations tool's port, handed to tools as
	// toolhub.Context.Conversations. Nil means none.
	Conversations conversation.Conversations
	// Subagents (A-105) is the agent's subagents, handed to tools as
	// toolhub.Context.Subagents for spawn_subagent. Nil means none.
	Subagents subagent.Subagents
	// ToolScope (A-105) says which tools a session may use, beyond the agent's allowlist -
	// a child run's session (a subagent) gets only its read-only tools, and never
	// spawn_subagent. Resolved per model call and per dispatch; nil, or a nil result,
	// means every tool of the agent.
	// Update (A-209): at dispatch the scope also gets the call's risk and arguments (a
	// read-only child may save an artifact but not update one); see ToolScope.
	ToolScope func(sessionID string) ToolScope
	// DelegatedTools (A-137) is the tools the agent hands to its subagents in a session
	// (the subagent manager's delegatedTools), left out of the tool list the model is
	// offered - hidden, not refused, so a call already in the history still runs.
	// Resolved per model call; nil means none.
	DelegatedTools func(ctx context.Context, session kernel.Session) []string
	// PreApproved (A-106) says whether a call runs without asking even though it needs
	// approval - the user already agreed to it for this session (a group lead handing work
	// to the group's members through spawn_subagent). The callee's own approval-gated
	// calls still ask. Nil means none.
	PreApproved func(sessionID, toolName string) bool
	// Secrets (A-101) is the agent's secrets, handed to tools as toolhub.Context.Secrets;
	// every tool result and tool error is passed through its Redact before anyone sees it
	// (the model, the conversation, traces, events, the UI). Nil means none.
	Secrets secret.Secrets
	// Variables (A-215) is the agent's variables, handed to tools as
	// toolhub.Context.Variables. Nil means none.
	Variables variable.Variables
}
