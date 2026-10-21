// Package subagent holds the contracts of subagents (A-105): a parent agent hands a
// bounded task to another agent profile through the spawn_subagent tool, waits for the
// child's final report and writes the reply itself. The parent stays the only one
// talking to the user: a child runs in a fresh session of its own, sees only the brief
// (never the parent's conversation), cannot ask the user anything, and cannot delegate
// further (depth 1).
//
// The agent's subagent manager (package agent) implements Subagents: as a parent it
// validates the brief, enforces the per-turn budget and records each run
// (persistence.SubagentRunStore); as a child it scopes the child session's tools
// (read-only by default) and routes its approvals to the parent's channel to the user.
// The host decides HOW a child turn runs through a Runner (AgentCore.UseSubagentRunner),
// so a child turn takes the host's per-session lock, stop registry, traces and usage.
//
// Design revision: two kinds of target. A SUBAGENT TYPE (Claude Code style) is a
// definition from the subagenthub library, enabled per agent (Config.Types); it runs as a
// short-lived loop inside the parent - the parent's model, a filtered set of its tools,
// its sandbox - in a side-branch session of the parent. An existing AGENT (Config.Allow)
// runs a turn of that full agent in a session of its own.
//
// The JSON shapes are the wire shapes of core/ts (SubagentConfig, SubagentRun), so the
// Go and TypeScript servers share the same Postgres rows and HTTP bodies.
//
// This package is a type-only leaf (it imports only kernel and the JS text helpers), so
// profile, persistence, toolhub and agent can all import it without a cycle.
package subagent

import (
	"context"
	"slices"

	"github.com/Phong95/aec-agent-go/internal/jsstr"
	"github.com/Phong95/aec-agent-go/kernel"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// ---------------------------------------------------------------------------
// Config - AgentProfile.Subagents
// ---------------------------------------------------------------------------

// Access is what a child may do. AccessReadOnly (the default) offers it only its
// read-only tools. AccessWrite offers all its tools, and needs the child to have its own
// sandbox, where only one writer runs at a time - the parent applies or checks the
// result.
type Access string

// The access levels. The string values are stable wire values.
const (
	AccessReadOnly Access = "read-only"
	AccessWrite    Access = "write"
)

// TargetConfig is an agent this agent may delegate to.
type TargetConfig struct {
	AgentID string `json:"agentId"`
	// Access defaults to AccessReadOnly.
	Access Access `json:"access,omitempty"`
}

// Config is AgentProfile.Subagents: who the agent may delegate to, and the budget all
// children of one parent turn share. A limit of 0 (omitted) uses the SDK default
// (DefaultLimits); values above the hard caps (HardCapLimits) are clamped.
type Config struct {
	// Allow are the agents this one may delegate to. Empty = none.
	Allow []TargetConfig `json:"allow"`
	// Types are the subagent types from the library (subagenthub.SubagentHub) this agent
	// may use. Omitted = none.
	Types []string `json:"types,omitempty"`
	// MaxConcurrent is how many children of one parent turn run at once (default 3, at
	// most 8).
	MaxConcurrent int `json:"maxConcurrent,omitempty"`
	// MaxRunsPerTurn is how many children one parent turn may start (default 6, at most
	// 20).
	MaxRunsPerTurn int `json:"maxRunsPerTurn,omitempty"`
	// MaxTokensPerTurn is the tokens (input + output) all children of one parent turn may
	// use together (default 200 000).
	MaxTokensPerTurn int `json:"maxTokensPerTurn,omitempty"`
	// MaxToolCallsPerTurn is the tool calls all children of one parent turn may make
	// together (default 50).
	MaxToolCallsPerTurn int `json:"maxToolCallsPerTurn,omitempty"`
	// MaxElapsedMsPerTurn is the wall-clock time from a parent turn's first child until
	// all its children must be done, in ms (default 10 min).
	MaxElapsedMsPerTurn int `json:"maxElapsedMsPerTurn,omitempty"`
	// ChildTimeoutMs is one child's deadline, in ms (default 5 min, at most 30 min).
	ChildTimeoutMs int `json:"childTimeoutMs,omitempty"`
}

// Limits is the budget part of a resolved Config.
type Limits struct {
	MaxConcurrent       int
	MaxRunsPerTurn      int
	MaxTokensPerTurn    int
	MaxToolCallsPerTurn int
	MaxElapsedMsPerTurn int
	ChildTimeoutMs      int
}

// DefaultLimits returns the SDK defaults of the limits a Config leaves out (the
// SUBAGENT_DEFAULTS of core/ts).
func DefaultLimits() Limits {
	return Limits{
		MaxConcurrent:       3,
		MaxRunsPerTurn:      6,
		MaxTokensPerTurn:    200_000,
		MaxToolCallsPerTurn: 50,
		MaxElapsedMsPerTurn: 10 * 60_000,
		ChildTimeoutMs:      5 * 60_000,
	}
}

// HardCapLimits returns the most each limit may be (the SUBAGENT_HARD_CAPS of core/ts).
func HardCapLimits() Limits {
	return Limits{
		MaxConcurrent:       8,
		MaxRunsPerTurn:      20,
		MaxTokensPerTurn:    2_000_000,
		MaxToolCallsPerTurn: 500,
		MaxElapsedMsPerTurn: 60 * 60_000,
		ChildTimeoutMs:      30 * 60_000,
	}
}

// The limits of the brief a parent sends (the SUBAGENT_BRIEF_LIMITS of core/ts), in
// UTF-16 code units like a JS string length.
const (
	// MaxTaskChars is the most characters of the task text.
	MaxTaskChars = 8_000
	// MaxContextChars is the most characters of the context, as JSON.
	MaxContextChars = 16_000
	// MaxOutputShapeChars is the most characters of the requested output shape.
	MaxOutputShapeChars = 2_000
	// MaxOutputChars is the most characters of a child's report handed back to the
	// parent.
	MaxOutputChars = 20_000
)

// ResolvedConfig is a Config with the defaults and the hard caps applied
// (ResolveConfig). Every target has its access set.
type ResolvedConfig struct {
	Allow []TargetConfig
	// Types are the enabled subagent type names, trimmed and without duplicates (never
	// nil).
	Types []string
	Limits
}

// ResolveConfig returns the profile's subagent config with defaults and hard caps
// applied. A limit below 1 falls back to the default; a target with an empty id or one
// already listed is skipped; an access other than "write" is read-only. A nil config
// has no targets and the default limits.
// (Update (A-105 revision): a type name is trimmed, and an empty or repeated one is
// skipped.)
func ResolveConfig(c *Config) ResolvedConfig {
	def, caps := DefaultLimits(), HardCapLimits()
	field := func(v, d, hard int) int {
		if v < 1 {
			return d
		}
		return min(v, hard)
	}
	out := ResolvedConfig{Allow: []TargetConfig{}, Types: []string{}, Limits: def}
	if c == nil {
		return out
	}
	seen := map[string]bool{}
	for _, t := range c.Allow {
		if t.AgentID == "" || seen[t.AgentID] {
			continue
		}
		seen[t.AgentID] = true
		access := AccessReadOnly
		if t.Access == AccessWrite {
			access = AccessWrite
		}
		out.Allow = append(out.Allow, TargetConfig{AgentID: t.AgentID, Access: access})
	}
	for _, name := range c.Types {
		name = jsstr.Trim(name)
		if name == "" || slices.Contains(out.Types, name) {
			continue
		}
		out.Types = append(out.Types, name)
	}
	out.MaxConcurrent = field(c.MaxConcurrent, def.MaxConcurrent, caps.MaxConcurrent)
	out.MaxRunsPerTurn = field(c.MaxRunsPerTurn, def.MaxRunsPerTurn, caps.MaxRunsPerTurn)
	out.MaxTokensPerTurn = field(c.MaxTokensPerTurn, def.MaxTokensPerTurn, caps.MaxTokensPerTurn)
	out.MaxToolCallsPerTurn = field(c.MaxToolCallsPerTurn, def.MaxToolCallsPerTurn, caps.MaxToolCallsPerTurn)
	out.MaxElapsedMsPerTurn = field(c.MaxElapsedMsPerTurn, def.MaxElapsedMsPerTurn, caps.MaxElapsedMsPerTurn)
	out.ChildTimeoutMs = field(c.ChildTimeoutMs, def.ChildTimeoutMs, caps.ChildTimeoutMs)
	return out
}

// ---------------------------------------------------------------------------
// Requests, runs and results
// ---------------------------------------------------------------------------

// Request is the bounded brief a parent sends - the only thing the child learns about
// the parent's work.
type Request struct {
	// AgentID is the target agent's id (or its name).
	// Update (A-105 revision): the target - an enabled subagent type's name, or an allowed
	// agent's id (or name). Types win on a name clash.
	AgentID string `json:"agentId"`
	// Task is what the child should do, self-contained.
	Task string `json:"task"`
	// Context is facts the child needs (file paths, ids, constraints), plain JSON data.
	// A zero object means none.
	Context ordered.Object `json:"context,omitzero"`
	// OutputShape is the shape the report should have ("a bullet list of risks with page
	// numbers").
	OutputShape string `json:"outputShape,omitempty"`
}

// Status is how a child run ended, or StatusRunning while it runs. The string values
// are stable wire values.
type Status string

// The statuses.
const (
	// StatusRunning: the child is running (a stored record while it runs).
	StatusRunning Status = "running"
	// StatusCompleted: it finished its turn.
	StatusCompleted Status = "completed"
	// StatusPartial: it stopped at its own turn limit (maxIterations) or output token
	// limit; the report is what it had.
	StatusPartial Status = "partial"
	// StatusFailed: it errored, or the provider stopped it (refusal, filter).
	StatusFailed Status = "failed"
	// StatusCancelled: the parent turn was stopped, or someone stopped the child.
	StatusCancelled Status = "cancelled"
	// StatusTimedOut: it hit its deadline; the report is what it had.
	StatusTimedOut Status = "timed_out"
	// StatusBudgetExhausted: the parent turn's shared child budget ran out (before it
	// started, or while it ran); the report is what it had.
	StatusBudgetExhausted Status = "budget_exhausted"
)

// Kind is what a run delegated to: a subagent type (a short-lived loop in the parent) or
// an existing agent. The string values are stable wire values.
type Kind string

// The kinds.
const (
	// KindType: a subagent type from the library, run inside the parent agent.
	KindType Kind = "type"
	// KindAgent: an existing agent, run in a session of its own.
	KindAgent Kind = "agent"
)

// Run is a child run record - persisted in PersistenceHub.SubagentRuns, so it outlives
// a restart. Spawn returns the finished record (its Status is never StatusRunning).
type Run struct {
	ID              string `json:"id"`
	Kind            Kind   `json:"kind"`
	ParentAgentID   string `json:"parentAgentId"`
	ParentSessionID string `json:"parentSessionId"`
	// ParentRunID is the parent turn that started it (children of one turn share a
	// budget).
	ParentRunID string `json:"parentRunId"`
	// ParentToolCallID is the parent's spawn_subagent call, when known - a client
	// matches the run to that call.
	ParentToolCallID string `json:"parentToolCallId,omitempty"`
	// ChildAgentID is the agent that owns the child's session: the target agent, or - for
	// a subagent type - the parent itself (the session is a side branch of the parent).
	ChildAgentID string `json:"childAgentId"`
	// ChildAgentName is the target agent's name, or the subagent type's name.
	ChildAgentName string `json:"childAgentName"`
	// ChildSessionID is the child's own session ("" when the run never started).
	ChildSessionID string `json:"childSessionId,omitempty"`
	Access         Access `json:"access"`
	// Task is the task text (cut to MaxTaskChars).
	Task   string `json:"task"`
	Status Status `json:"status"`
	// Output is the child's report - sanitized, treat it as untrusted data.
	Output string `json:"output,omitempty"`
	// Usage is the tokens the child used.
	Usage kernel.TokenUsage `json:"usage"`
	// ToolCalls is the tool calls the child made.
	ToolCalls int `json:"toolCalls"`
	// Error is a safe one-line reason for a run that did not complete.
	Error      string `json:"error,omitempty"`
	StartedAt  string `json:"startedAt"`
	FinishedAt string `json:"finishedAt,omitempty"`
	// DurationMs is set once the run finished.
	DurationMs *int64 `json:"durationMs,omitempty"`
}

// Clone returns a deep copy: the usage's cache counters and DurationMs are copied. Add a
// line here when a pointer, slice or map field is added to Run.
func (r Run) Clone() Run {
	r.Usage = cloneUsage(r.Usage)
	if r.DurationMs != nil {
		v := *r.DurationMs
		r.DurationMs = &v
	}
	return r
}

func cloneUsage(u kernel.TokenUsage) kernel.TokenUsage {
	if u.CacheReadTokens != nil {
		v := *u.CacheReadTokens
		u.CacheReadTokens = &v
	}
	if u.CacheWriteTokens != nil {
		v := *u.CacheWriteTokens
		u.CacheWriteTokens = &v
	}
	return u
}

// SpawnContext is what the spawn_subagent tool knows about the parent turn calling it.
// The parent turn's cancellation arrives through the ctx of Spawn: cancelling it
// cancels the child.
//
// Unlike core/ts, it carries no interaction handler: the child's approval requests go
// to the parent session's channel to the user, which the parent agent resolves itself
// (its interaction router), so a host call and a tool call route them the same way.
type SpawnContext struct {
	// SessionID is the parent's session.
	SessionID string
	// RunID is the parent turn (toolhub.Context.RunID); "" = the session stands in for it.
	RunID string
	// ToolCallID is the parent's spawn_subagent call.
	ToolCallID string
}

// TargetInfo is an agent the parent may delegate to, for the model's index and error
// messages. (Update (A-105 revision): or a subagent type - Kind says which.)
type TargetInfo struct {
	Kind Kind `json:"kind"`
	// AgentID is the agent's id, or the type's name.
	AgentID string `json:"agentId"`
	Name    string `json:"name"`
	Access  Access `json:"access"`
	// Description is the first line of the agent's system prompt.
	// (Update (A-105 revision): or the type's description.)
	Description string `json:"description"`
}

// Subagents is an agent's subagents - toolhub.Context.Subagents, and Agent.Subagents
// for hosts (the AgentSubagents of core/ts).
type Subagents interface {
	// Spawn runs a child for the parent turn in sc and waits for its report.
	// Refusals decided before the child runs (not allowed, unknown agent, nesting, bad
	// brief, no writable sandbox) return an *Error; everything after returns the run
	// with a status and a nil error.
	Spawn(ctx context.Context, req Request, sc SpawnContext) (Run, error)
	// Targets returns the agents this one may delegate to (configured and running).
	// Update (A-105 revision): what this agent may delegate to - the enabled subagent
	// types, then the allowed running agents. A type the library cannot read is left out.
	Targets(ctx context.Context) []TargetInfo
	// List returns the child runs started from one of this agent's sessions, oldest
	// first.
	List(ctx context.Context, parentSessionID string) ([]Run, error)
	// Get returns one child run started by this agent, or nil and a nil error.
	Get(ctx context.Context, runID string) (*Run, error)
	// Stop stops a running child of this agent. False when it is not running here.
	Stop(runID string) bool
	// Running returns how many child runs of this agent run now.
	Running() int
}

// ---------------------------------------------------------------------------
// How a child turn runs
// ---------------------------------------------------------------------------

// ParentRef is what a child knows about the turn that delegated to it.
type ParentRef struct {
	RunID     string `json:"runId"`
	AgentID   string `json:"agentId"`
	SessionID string `json:"sessionId"`
	// SubagentRunID is the child run's id.
	SubagentRunID string `json:"subagentRunId"`
}

// TurnContext is one child turn, handed to the Runner. Its cancellation (parent stop,
// deadline, budget or Stop(runID)) arrives through the runner's ctx.
type TurnContext struct {
	// Kind is what the run delegated to.
	Kind Kind
	// AgentID is the child agent.
	// Update (A-105 revision): the agent that owns the child's session (the target agent,
	// or the parent for a type).
	AgentID string
	// ChildAgentName (A-106) is the target agent's name, or the subagent type's name.
	ChildAgentName string
	// SessionID is the child's session, created for this run.
	// (A-106: or the one the spawn scope named - SpawnScope.ChildSession.)
	SessionID string
	// Prompt is the brief, as the child's user message.
	Prompt string
	// Task (A-106) is the task text the parent sent (a group shows it as the hand-off).
	Task string
	// Parent is the parent turn - put on the child's run span so traces correlate.
	Parent ParentRef
	// TraceAttributes are attributes for the child's "agent-run" span
	// (agent.WithChatTraceAttributes).
	TraceAttributes map[string]any
	// Execute runs the child turn (the target agent's Chat, or the type's short-lived
	// loop). A host runner wraps its bookkeeping around it and passes the ctx it controls
	// (derived from the runner's ctx).
	Execute func(ctx context.Context) (TurnResult, error)
}

// TurnResult is what a child turn produced.
type TurnResult struct {
	Output string
	Usage  kernel.TokenUsage
	// Aborted: the turn was stopped through its ctx.
	Aborted bool
	// StopReason is the stop reason of the turn's last response (max_tokens and error
	// matter; "" otherwise).
	StopReason kernel.StopReason
}

// Runner runs one child turn. The host sets one (AgentCore.UseSubagentRunner) so a
// child turn takes its bookkeeping - per-session lock, stop registry, traces, usage -
// like its chat routes. Absent = the child agent's own Chat.
// (Update (A-105 revision): absent = tc.Execute(ctx), the child agent's Chat or the
// type's short-lived loop.)
type Runner func(ctx context.Context, tc TurnContext) (TurnResult, error)

// ---------------------------------------------------------------------------
// Spawn scopes (A-106) - a session whose spawns follow other rules than the profile's
// ---------------------------------------------------------------------------

// ScopeTarget is the child a SpawnScope's Brief is written for.
type ScopeTarget struct {
	// AgentID is the agent that owns the child's session.
	AgentID string
	// Name is the target agent's name.
	Name string
	// RunID is the child run's id.
	RunID string
}

// SpawnScope is how spawns from one session work instead of the profile's subagent
// config (the SubagentSpawnScope of core/ts). A group chat's lead, while it leads a group
// turn, delegates to the group's members: they see the group thread (not a fresh brief),
// work in their own member sessions, stream into the thread, and the hand-offs need no
// extra approval (the user set the group up for it).
type SpawnScope struct {
	// Allow is who may be delegated to from the session - it replaces the profile's
	// Subagents.Allow; subagent types are off.
	Allow []TargetConfig
	// Limits replace the profile's limits field by field (0 keeps the profile's; the hard
	// caps still apply).
	Limits Limits
	// PreApproved: spawn_subagent runs in the session without asking the user (the
	// children's own approval-gated calls still ask).
	PreApproved bool
	// GrantSpawnTool: the session is granted spawn_subagent even when the agent's
	// allowlist lacks it.
	GrantSpawnTool bool
	// IndexIntro replaces the first sentences of the <available_subagents> index (the
	// default "Each starts fresh..."). "" keeps the default.
	IndexIntro string
	// ChildSession returns the session a child of agentID runs in, instead of a fresh
	// child session (agent targets only). Nil = a fresh session.
	ChildSession func(ctx context.Context, agentID string) (string, error)
	// Brief returns the child's user message, instead of the fresh brief. Nil = the brief.
	Brief func(ctx context.Context, req Request, target ScopeTarget) (string, error)
	// Runner runs the child turn instead of the host's subagent runner. Nil = the host's.
	Runner Runner
}

// Scopes is the spawn scopes of the host's sessions (AgentCore: the GroupHub's, for group
// leads). Get returns nil for a session without one.
type Scopes interface {
	Get(sessionID string) *SpawnScope
}

// SessionSource is the "source" metadata value of a session created for a child run.
const SessionSource = "subagent"

// IsSession reports whether a session was created for a child run (its metadata
// "source" is "subagent"). A nil session is not.
func IsSession(s *kernel.Session) bool {
	if s == nil {
		return false
	}
	v, ok := s.Metadata.Get("source")
	return ok && v == SessionSource
}
