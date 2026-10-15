// Package agent is the Agent and its managers. An Agent is the harness around one LLM:
// it owns the agent's profile, the managers that adapt the SDK hubs to the agent loop's
// ports (ModelManager, ToolManager, ContextManager, ConversationManager) and the loop
// itself, and it exposes the session API (StartSession, LoadSession, ListSessions,
// Chat, ChatStream, Complete) and the runtime Update* setters.
//
// Concurrency: an Agent is race-free. Chat, ChatStream, Complete, the session calls,
// Profile and the Update* setters may be called from several goroutines at once. The
// chats of one agent are serialized by the agent itself, so two overlapping chats never
// interleave in the one conversation: a Chat or ChatStream that starts while another
// chat of the agent runs waits for its turn, honouring its ctx. A caller that keeps its
// own per-agent mutex (server/go does) still works; it is no longer needed for
// correctness.
//
// -> Superseded (A-97): turns are serialized per session, not per agent. Chat and
// ChatStream take the session with WithChatSession (default: the session StartSession
// or LoadSession set last). Turns in different sessions of one agent run in parallel;
// turns in the same session run one at a time, in arrival order, and a waiting turn
// honours its ctx. loopConfig.maxConcurrentTurns optionally caps how many turns the
// agent runs at once (0 = no cap); it is read on every turn. A nested chat on the same
// session (from a tool of a running turn of that session) returns ErrReentrantChat; on
// another session it runs. Each turn loads its session's conversation from the stores
// when it starts and drops it when it ends, so turns share no in-memory state and see
// what the host wrote between turns. Agent.RunningTurns reports the turns running now.
// The ConversationManager starts every turn (beginTurn: resolve the session, wait at the
// turn gate, load the Conversation) and owns the gate and the bookkeeping Dispose uses to
// close the turns; the Agent only runs the loop inside the turn.
// server/go keeps a per-session lock for its own session writes.
//
// Session binding: a run is for the session that is active when it starts, and the
// loop refuses a session id that is not the active one (agentloop.ErrSessionNotActive),
// so a chat that was started for one session cannot write into another one when the
// active session is switched underneath it. The binding holds for the whole turn: Chat
// and ChatStream put the session id on the run's context and the ConversationManager
// refuses to write (an error matching agentloop.ErrSessionNotActive) when the active
// session is no longer that one, so a StartSession or LoadSession from another goroutine
// makes the running turn fail at its next write and never moves its messages or token
// usage into the new session.
//
// -> Superseded (A-97): there is no binding to enforce any more. A turn works on the
// Conversation of its own session, so a StartSession or LoadSession from another
// goroutine only changes the default session of later turns; the running turn finishes
// in its session as if nothing happened.
//
// Ownership: an Agent deep-copies the profile it is spawned with and never keeps
// a reference to the caller's struct. Profile returns a deep copy, and the
// Update* setters change only the agent's own copy and its managers; none of them
// persists the profile.
//
// Lifecycle: an agent owns background work, so it must be disposed when it is no
// longer used. New builds the agent's memory system and starts it: while the
// profile's consolidation interval is above zero a goroutine consolidates the
// finished turns (the interval can be changed at runtime with UpdateMemoryConfig).
// The work runs under a lifetime context the agent owns, derived from
// context.Background(): neither the cancellation nor the values of the context given
// to New reach it. An agent whose caller cancelled the spawning request therefore keeps
// running until Dispose (a behaviour change for direct agent.New users). Dispose refuses new
// chats (ErrAgentDisposed), cancels the running chat and waits for it (A-97: every running
// chat, in every session, and the waiting ones give up), stops the scheduled jobs,
// stops the memory system (waiting for the pass in flight), waits for the job runs
// in flight, then releases the agent's own sandbox (its files are kept, and deleted only
// by Agent.Delete), then cancels the lifetime context; no goroutine of the
// agent outlives it. Dispose is safe to call more than once and concurrently: a call that
// finds another one running waits for it inside its own ctx, and a later
// Agent.Delete still removes the own-sandbox data when no earlier call has.
// After Dispose the agent's sandbox manager is closed: no sandbox is created again. A job
// must pass the ctx of its Execute to Dispose if it disposes its own agent (see
// Agent.Dispose).
//
// Scheduled jobs: UseScheduledJob registers a ScheduledTaskHandler (a cron expression or an
// interval, see ScheduledTaskDefinition) and StartScheduledJob runs it on its schedule, in its
// own goroutine, under the lifetime context. The outcome of every run is a JobCompleted
// or JobFailed event on the event bus, on the session "scheduled-job:<agent>:<name>".
// Cron expressions are standard 5-field cron, read in the job's Timezone or the manager's
// location.
// Update (A-126): scheduled tasks (plugin code, UseScheduledTask) were removed; every job
// is a stored job of the job manager (see package job).
//
// Sandbox and artifacts: with Deps.Sandboxes a tool reaches the agent's sandbox
// lazily through toolhub.Context.GetSandbox (the shared sandbox in the agent's own
// folder by default, the agent's own sandbox when the profile asks for one, a
// fail-closed error for "none" or an unconfigured shared sandbox). With
// Deps.Artifacts a tool reaches the agent's artifact view.
//
// The package imports neither the root aec package nor aecmock nor conformance;
// aec.AgentCore builds agents through orchestration.Orchestration.
package agent
