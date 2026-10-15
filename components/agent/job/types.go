// Package job holds the contracts of jobs (A-126): one concept for "when X happens, run
// this prompt, maybe tell me".
//
// A Job is a stored prompt plus WHEN it runs:
//   - on a clock   ("at" / "every" / "cron")  - was a scheduled prompt job (A-69)
//   - on an event  ("event")                   - the agent's own events, was an event trigger (A-70)
//   - on a webhook ("webhook")                 - a signed POST from another system, was a webhook trigger (A-70)
//
// Every run, whatever started it, goes through one durable path:
//
//	clock tick / bus event / webhook / run now / test event
//	     -> filter -> durable inbox (dedupe + coalesce) -> claim/lease -> agent turn -> result
//
// The agent's job manager decides WHEN and WHAT; the host decides HOW through a Runner,
// so a run takes the host's per-session lock, stop registry, traces and usage.
//
// (from A-70) Durability comes from the store: JobStore.Accept records the event and
// queues its run in one atomic step, so a host that answers a webhook only after Accept
// returns never acknowledges an event it could lose. The in-memory store keeps nothing
// across a restart.
//
// (from A-69) A prompt job is data, not code: it is created by a user (host API/UI) or
// by the agent itself (the manage_job tool), kept in persistence.JobStore and survives a
// restart. Each run is a real agent turn - model + tools + skills + memory - in a session
// the user can open later. Code registered by a plugin is a scheduled task
// (agent.ScheduledTaskHandler), not a job. (Update (A-126): scheduled tasks were
// removed; a job is the only scheduled work.)
//
// Update (A-70 agent tools, kept): the agent may create and change jobs through the
// manage_job tool (toolhub/builtin/jobs), which asks the user for approval on every call
// (so an unattended run never can). Such a job is marked CreatedBy "agent", and when it
// was asked for in a channel chat it delivers its results back there (Delivery).
//
// The JSON shapes are the wire shapes of core/ts (Job, JobEvent, JobRun), so the Go and
// TypeScript servers share the same Postgres rows and HTTP bodies.
//
// Update (A-126): this package replaces the promptjob (A-69) and trigger (A-70)
// packages, which held the same contracts for scheduled prompt jobs and triggers.
package job

import (
	"bytes"
	"context"
	"encoding/json"
)

// ---------------------------------------------------------------------------
// When a job runs
// ---------------------------------------------------------------------------

// Kind says when a job runs. The string values are stable wire values.
type Kind string

// The kinds.
const (
	// KindCron is a 5-field cron expression ("min hour dom month dow") in a timezone.
	KindCron Kind = "cron"
	// KindEvery is a fixed interval, counted from the previous scheduled time.
	KindEvery Kind = "every"
	// KindAt is one run at a fixed time; the job disables itself after it has run.
	KindAt Kind = "at"
	// KindEvent is the agent's own SDK events on the event bus (InternalEventTypes).
	// In-process: an event emitted while the host is down is not seen.
	KindEvent Kind = "event"
	// KindWebhook is POSTs to the host's webhook route, signed with the job's secret.
	KindWebhook Kind = "webhook"
)

// SignatureScheme is how a webhook delivery is signed.
type SignatureScheme string

// The signature schemes.
const (
	// SignatureStandard is Standard Webhooks: webhook-id / webhook-timestamp /
	// webhook-signature.
	SignatureStandard SignatureScheme = "standard"
	// SignatureGithub is X-Hub-Signature-256 + X-GitHub-Delivery + X-GitHub-Event.
	SignatureGithub SignatureScheme = "github"
)

// When is when a job runs: a schedule (KindCron, KindEvery, KindAt), or events from a
// source (KindEvent, KindWebhook). Kind says which of the other fields apply: Expr and
// TZ for cron, Ms for every, At for at; Signature, SubjectPath and RevisionPath for a
// webhook (SubjectPath / RevisionPath pick the subject and revision from the event,
// "data.pull_request.number"; a CloudEvents body sets its own subject).
type When struct {
	Kind Kind `json:"kind"`
	// Expr is the cron expression: ranges, steps, lists and names; the day of month and
	// the day of week combine with OR, like classic cron.
	Expr string `json:"expr,omitempty"`
	// TZ is the IANA timezone of a cron expression, e.g. "Asia/Ho_Chi_Minh". Empty means
	// the job manager's location (the server's timezone by default).
	TZ string `json:"tz,omitempty"`
	// Ms is the interval of an every schedule, in milliseconds.
	Ms int64 `json:"ms,omitempty"`
	// At is the ISO 8601 time of an at schedule.
	At        string          `json:"at,omitempty"`
	Signature SignatureScheme `json:"signature,omitempty"`
	// SubjectPath is "subject" or "data.<dot.path>".
	SubjectPath string `json:"subjectPath,omitempty"`
	// RevisionPath is "subject" or "data.<dot.path>".
	RevisionPath string `json:"revisionPath,omitempty"`
}

// IsSchedule says whether the job runs on a clock ("at" / "every" / "cron").
func (w When) IsSchedule() bool {
	return w.Kind == KindCron || w.Kind == KindEvery || w.Kind == KindAt
}

// ConditionOp is the operator of a Condition.
type ConditionOp string

// The condition operators.
const (
	OpEq       ConditionOp = "eq"
	OpNeq      ConditionOp = "neq"
	OpIn       ConditionOp = "in"
	OpExists   ConditionOp = "exists"
	OpContains ConditionOp = "contains"
)

// Condition is one declarative condition - no scripts. Path is "type", "subject",
// "source" or "data.<dot.path>".
type Condition struct {
	Path string      `json:"path"`
	Op   ConditionOp `json:"op"`
	// Value is, for eq/neq, the value; for in, a list ([]any); for exists, true
	// (default) or false; for contains, a string or a list item. Nil means absent
	// (unlike core/ts, a JSON null cannot be compared against).
	Value any `json:"value,omitempty"`
}

// Filter is which events an event or webhook job accepts. Every part that is set must
// match. A schedule job has none.
type Filter struct {
	// Types are event types; "prefix.*" matches a prefix. Required for an event job.
	// Empty means any type.
	Types []string `json:"types,omitempty"`
	// Subjects keeps only these subjects (e.g. one job's id).
	Subjects []string    `json:"subjects,omitempty"`
	Where    []Condition `json:"where,omitempty"`
}

// Coalesce is burst coalescing (event and webhook jobs): events for the same job +
// subject that arrive before their run starts are handled by one run. The run starts
// once no new event came for QuietMs, at most MaxWaitMs after the first one, with at
// most MaxBatch events (more open the next run). A schedule job's ticks never coalesce.
type Coalesce struct {
	QuietMs   int `json:"quietMs"`
	MaxWaitMs int `json:"maxWaitMs"`
	MaxBatch  int `json:"maxBatch"`
}

// Limits bound unattended work.
type Limits struct {
	// MaxRunsPerHour is runs started per rolling hour; more wait for the window. Nil
	// means no limit (the default for a schedule job: its clock already bounds it).
	MaxRunsPerHour *int `json:"maxRunsPerHour,omitempty"`
	// MaxConcurrentRuns is runs of this job at once (this process).
	MaxConcurrentRuns int `json:"maxConcurrentRuns"`
	// MaxAttempts is attempts per run, the first included; transient failures retry
	// with backoff. A schedule job defaults to 1.
	MaxAttempts int `json:"maxAttempts"`
	// RunDeadlineMs: a run is not retried later than this after it was queued.
	RunDeadlineMs int `json:"runDeadlineMs"`
	// MaxTokensPerDay is tokens per rolling day; once used up, runs end "blocked". Nil
	// means no budget.
	MaxTokensPerDay *int `json:"maxTokensPerDay,omitempty"`
	// CooldownMs: after a run for a subject, the next run for it waits at least this long.
	CooldownMs int `json:"cooldownMs"`
	// MaxDepth is how deep a chain of event jobs may go. An event caused by a run of an
	// event or webhook job has depth = that run's depth + 1; 0 (default) ignores every
	// event such a run caused. Events from a schedule job's runs have depth 0.
	MaxDepth int `json:"maxDepth"`
	// MaxRunMs (A-103) is the longest one attempt may run, in ms (10 s - 24 h). When it
	// passes the run is aborted and ends "failed" ("Timed out after ..."), not retried. Nil
	// (a job stored before A-103) means DefaultMaxRunMs.
	MaxRunMs *int `json:"maxRunMs,omitempty"`
	// PauseAfterFailures (A-103): after this many finished runs in a row ended "failed" or
	// "blocked", the job disables itself (0 = never). Succeeded and silent runs reset the
	// streak; suppressed and stopped runs don't count. Runs from before the job's last
	// edit don't count either, so re-enabling it starts fresh. Nil (a job stored before
	// A-103) means DefaultPauseAfterFailures.
	PauseAfterFailures *int `json:"pauseAfterFailures,omitempty"`
}

// SessionMode says which session a run writes into.
type SessionMode string

// The session modes.
const (
	// SessionNew gives every run a fresh session (the default).
	SessionNew SessionMode = "new"
	// SessionReuse appends every run to one session, created on the first run (a
	// watcher that remembers what it already reported).
	SessionReuse SessionMode = "reuse"
)

// DeliveryWhen says which finished runs a job delivers to its chat (A-100 phase 3).
type DeliveryWhen string

// The delivery conditions. A stopped run is never delivered. Update (A-126): a silent
// run (reply [[SILENT]]) is never delivered either.
const (
	// DeliverAlways delivers every finished run: the reply, a "no reply" notice, or the
	// error.
	DeliverAlways DeliveryWhen = "always"
	// DeliverOnOutput delivers only a run that succeeded with a reply.
	DeliverOnOutput DeliveryWhen = "on-output"
	// DeliverOnFailure delivers only a failed run.
	DeliverOnFailure DeliveryWhen = "on-failure"
)

// DeliveryHome is the Delivery.Target of the channel's home chat (/sethome).
const DeliveryHome = "home"

// Delivery is where a job's result goes (A-100 phase 3): a chat of one of the agent's
// channels. The agent's channel manager sends it when the run ends (it listens for the
// job events), so the job manager itself never talks to a channel.
type Delivery struct {
	// ChannelID is one of the agent's channels.
	ChannelID string `json:"channelId"`
	// Target is a chat id on that channel, or DeliveryHome for its home chat.
	Target string `json:"target"`
	// ThreadID is the thread inside a named chat (unused with DeliveryHome).
	ThreadID string       `json:"threadId,omitempty"`
	When     DeliveryWhen `json:"when"`
}

// Deliveries (A-127) are all the chats a job's result goes to: any of the agent's
// channels' home chats, or named chats. It reads the stored shape of before A-127 too (one
// Delivery object), so a row saved then loads as a one-item list.
type Deliveries []Delivery

// UnmarshalJSON reads a list, one Delivery (the shape before A-127) or null.
func (d *Deliveries) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	switch {
	case len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")):
		*d = nil
		return nil
	case trimmed[0] == '{':
		var one Delivery
		if err := json.Unmarshal(trimmed, &one); err != nil {
			return err
		}
		*d = Deliveries{one}
		return nil
	}
	var list []Delivery
	if err := json.Unmarshal(trimmed, &list); err != nil {
		return err
	}
	*d = list
	return nil
}

// DeliveryOptions (A-127) are where a job asked for in a turn can send its results: the
// chat app conversation the turn runs in (nil outside a chat app) and the agent's
// running channels that have a home chat.
type DeliveryOptions struct {
	// Here is the chat this turn came from, as "home" when it is its channel's home chat.
	Here *Delivery
	// Homes are the channels with a home chat, by name.
	Homes []HomeChat
}

// HomeChat is one channel whose home chat can get job results.
type HomeChat struct {
	ChannelID string
	// Name is the channel's name, Platform its platform ("telegram", "slack"...).
	Name     string
	Platform string
}

// Default is where a job's results go when the turn names nothing: back to this chat in
// a chat app, else to every home chat. Empty when there is neither.
func (o DeliveryOptions) Default() Deliveries {
	if o.Here != nil {
		return Deliveries{*o.Here}
	}
	return o.HomeDeliveries(o.Homes)
}

// HomeDeliveries is a delivery to the home chat of each of homes.
func (o DeliveryOptions) HomeDeliveries(homes []HomeChat) Deliveries {
	var out Deliveries
	for _, h := range homes {
		out = append(out, Delivery{ChannelID: h.ChannelID, Target: DeliveryHome, When: DeliverAlways})
	}
	return out
}

// ---------------------------------------------------------------------------
// What a run does (A-103)
// ---------------------------------------------------------------------------

// FollowUp says when a tool job also starts a model turn after its tool (A-103).
type FollowUp string

// The follow-ups.
const (
	// FollowUpNever: the tool's output is the result (the default).
	FollowUpNever FollowUp = "never"
	// FollowUpOnFailure: a turn only when the tool failed (e.g. "explain the error").
	FollowUpOnFailure FollowUp = "on-failure"
	// FollowUpAlways: a turn after every tool run; it sees the tool's output in the session.
	FollowUpAlways FollowUp = "always"
)

// ActionKind is what a run does.
type ActionKind string

// The action kinds.
const (
	// ActionPrompt is a full agent turn with the job's prompt (as before A-103).
	ActionPrompt ActionKind = "prompt"
	// ActionTool is one stored tool call, run without the model.
	ActionTool ActionKind = "tool"
)

// Action is what a run does (A-103). A nil Job.Action is a prompt run (every job stored
// before A-103). A tool action runs the exact tool and arguments approved when the job
// was created; schedule jobs only. The tool must be in the agent's tool list and marked
// schedulable (toolhub.Definition.Schedulable).
type Action struct {
	Kind ActionKind `json:"kind"`
	// Tool is a schedulable tool in the agent's tool list (Kind tool).
	Tool string `json:"tool,omitempty"`
	// Args are the exact arguments, approved when the job was created; never templated.
	Args map[string]any `json:"args,omitempty"`
	// FollowUp defaults to never (empty). Any other value needs the job's prompt (the
	// follow-up turn's message).
	FollowUp FollowUp `json:"followUp,omitempty"`
}

// CreatedBy says who made a job.
type CreatedBy string

// The creators.
const (
	// CreatedByUser is a job made through the host API or UI.
	CreatedByUser CreatedBy = "user"
	// CreatedByAgent is a job the agent made itself with the manage_job tool.
	CreatedByAgent CreatedBy = "agent"
)

// Job is a stored job.
type Job struct {
	ID string `json:"id"`
	// AgentID is the owning agent. Every read path is filtered by it.
	AgentID string `json:"agentId"`
	// Name is short and unique per agent.
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Enabled     bool   `json:"enabled"`
	// Version is bumped on every configuration change; a run records the version it was
	// queued under.
	Version int  `json:"version"`
	When    When `json:"when"`
	// Filter is which events start a run (event and webhook jobs). Empty for a schedule job.
	Filter Filter `json:"filter"`
	// Prompt is the message the agent receives on each run. It must stand on its own -
	// runs see no chat history. Event and webhook jobs may use placeholders
	// {{job.name}}, {{event.type}}, {{event.subject}}, {{event.source}},
	// {{event.time}}, {{events.count}}; event data is never inlined into it, it follows
	// in a block marked as untrusted.
	// Update (A-103): every job may also use {{now}}, {{job.lastRunAt}},
	// {{job.lastSuccessAt}} and {{job.lastResult}} (the previous successful result,
	// attached in its own block). A tool job's prompt is only the follow-up turn's
	// message, so it may be empty when the follow-up is never.
	Prompt string `json:"prompt"`
	// Action (A-103) is what a run does; nil is a prompt run (a full agent turn).
	Action      *Action     `json:"action,omitempty"`
	Coalesce    Coalesce    `json:"coalesce"`
	Limits      Limits      `json:"limits"`
	SessionMode SessionMode `json:"sessionMode"`
	// SessionID is the session of a reuse job (set on its first run).
	SessionID string `json:"sessionId,omitempty"`
	// NextRunAt is, for a schedule job, the ISO time of the next run; nil (JSON null)
	// when disabled, not on a clock, or a one-off job has run.
	NextRunAt  *string   `json:"nextRunAt"`
	LastRunAt  string    `json:"lastRunAt,omitempty"`
	LastStatus RunStatus `json:"lastStatus,omitempty"`
	LastError  string    `json:"lastError,omitempty"`
	// Secret is the webhook signing secret ("whsec_" + base64). Generated by the SDK;
	// hosts must encrypt it at rest and show it only on create / rotate.
	Secret string `json:"secret,omitempty"`
	// CreatedBy says who made it: a person (host API/UI) or the agent itself (the
	// manage_job tool).
	CreatedBy CreatedBy `json:"createdBy"`
	// CreatedInSessionID is the session the agent was asked in, when CreatedBy is
	// CreatedByAgent.
	CreatedInSessionID string `json:"createdInSessionId,omitempty"`
	// Delivery sends each run's result to a chat (A-100 phase 3); nil keeps results in
	// the run's session only.
	// Update (A-127): Deliveries - to every chat in the list (several channels' home
	// chats, say); empty keeps results in the run's session only.
	Deliveries Deliveries `json:"deliveries,omitempty"`
	// CreatedAt and UpdatedAt are ISO timestamps (the shape of JS toISOString).
	CreatedAt string `json:"createdAt"`
	UpdatedAt string `json:"updatedAt"`
}

// ---------------------------------------------------------------------------
// Inbox and runs
// ---------------------------------------------------------------------------

// EventStatus is what happened to an incoming event.
type EventStatus string

// The event statuses.
const (
	// EventAccepted: matched and queued in a run.
	EventAccepted EventStatus = "accepted"
	// EventFiltered: did not match the job's filter (kept so the history shows why).
	EventFiltered EventStatus = "filtered"
	// EventSuppressed: matched but dropped - a chain too deep, the previous run still
	// going... (Reason).
	EventSuppressed EventStatus = "suppressed"
)

// Event is one event in a job's durable inbox - a CloudEvents-style envelope plus where
// it came from. Unique per job + source + SourceEventID: the same source event delivered
// twice is one inbox entry, while different jobs each get their own. A clock tick is an
// event too (source "schedule", id = the scheduled time), so a scheduled time runs at
// most once.
type Event struct {
	ID      string `json:"id"`
	JobID   string `json:"jobId"`
	AgentID string `json:"agentId"`
	// Source is "schedule", "aec:jobs", "aec:channels", "webhook", "manual", or a
	// CloudEvents source.
	Source string `json:"source"`
	// SourceEventID is the source's own id for the event (a scheduled time, webhook-id,
	// X-GitHub-Delivery, a run id...).
	SourceEventID string `json:"sourceEventId"`
	Type          string `json:"type"`
	// Subject is the entity it is about; events are coalesced per subject. Empty means
	// none.
	Subject string `json:"subject,omitempty"`
	// Revision is the monotonic revision of the subject; a run for an older revision
	// than one already handled is suppressed.
	Revision *float64 `json:"revision,omitempty"`
	// Time is when it happened at the source (ISO).
	Time string `json:"time"`
	// ReceivedAt is when the inbox received it (ISO).
	ReceivedAt string `json:"receivedAt"`
	// Data is bounded JSON data - untrusted. Nil encodes as null.
	Data json.RawMessage `json:"data"`
	// CorrelationID ties together everything this event led to.
	CorrelationID string `json:"correlationId"`
	// CausationID is the event that caused this one, when known.
	CausationID string `json:"causationId,omitempty"`
	// OriginRunID is the job run whose output caused this event, when it came from one.
	OriginRunID string `json:"originRunId,omitempty"`
	// Depth is 0 for an outside event, origin run depth + 1 for one caused by an event or
	// webhook job's run.
	Depth  int         `json:"depth"`
	Status EventStatus `json:"status"`
	// Reason is why it was filtered or suppressed.
	Reason string `json:"reason,omitempty"`
	// RunID is the run it joined.
	RunID string `json:"runId,omitempty"`
}

// RunStatus is the state of a run.
type RunStatus string

// The run statuses.
const (
	// RunPending is open: collecting events until NotBefore, then claimable.
	RunPending RunStatus = "pending"
	// RunWaiting is closed and claimable from NotBefore: a retry after a transient
	// failure, or waiting for the rate window.
	RunWaiting   RunStatus = "waiting"
	RunRunning   RunStatus = "running"
	RunSucceeded RunStatus = "succeeded"
	RunFailed    RunStatus = "failed"
	// RunBlocked ended without doing its work: an approval was needed, or the token
	// budget is used up (Reason).
	RunBlocked RunStatus = "blocked"
	// RunSuppressed was dropped before running: the job was disabled or changed, the
	// revision was stale... (Reason).
	RunSuppressed RunStatus = "suppressed"
	// RunStopped was stopped by a person.
	RunStopped RunStatus = "stopped"
)

// RunCause is what started a run.
type RunCause string

// The causes. "schedule" = the job's clock; "catch-up" = its time passed while the host
// was down; "manual" = run now; "event" / "webhook" = an event from that source; "test"
// = a test event; "replay" = a person replayed another run's events.
const (
	CauseSchedule RunCause = "schedule"
	CauseCatchUp  RunCause = "catch-up"
	CauseManual   RunCause = "manual"
	CauseEvent    RunCause = "event"
	CauseWebhook  RunCause = "webhook"
	CauseTest     RunCause = "test"
	CauseReplay   RunCause = "replay"
)

// Usage is the token usage of a run.
type Usage struct {
	InputTokens  int `json:"inputTokens"`
	OutputTokens int `json:"outputTokens"`
}

// Run is one logical run - the unit of work, retries and history. Several coalesced
// events share one run. Retries re-run the same logical run (same id and session); a
// result checkpoint (ResultAt) means the reasoning is done and a reclaim only finishes
// recording it.
type Run struct {
	ID      string `json:"id"`
	JobID   string `json:"jobId"`
	AgentID string `json:"agentId"`
	// JobVersion is the job version when the run was queued.
	JobVersion int      `json:"jobVersion"`
	Cause      RunCause `json:"cause"`
	// BucketKey is the coalescing bucket: the subject, or "" when events have none.
	BucketKey string    `json:"bucketKey"`
	Status    RunStatus `json:"status"`
	// EventIDs are the inbox events this run handles, oldest first.
	EventIDs []string `json:"eventIds"`
	// Revision is the newest revision among the events.
	Revision      *float64 `json:"revision,omitempty"`
	CorrelationID string   `json:"correlationId"`
	// Depth is the depth of the run's events (the deepest).
	Depth int `json:"depth"`
	// NotBefore is when the run is claimable from (ISO).
	NotBefore string `json:"notBefore"`
	// Deadline is when collecting ends (ISO): the quiet window never pushes NotBefore
	// past it.
	Deadline string `json:"deadline"`
	// ExpiresAt: no retry is scheduled after this (ISO).
	ExpiresAt   string `json:"expiresAt"`
	Attempts    int    `json:"attempts"`
	MaxAttempts int    `json:"maxAttempts"`
	// LeaseOwner and LeaseUntil are the worker holding the run, while it runs.
	LeaseOwner string `json:"leaseOwner,omitempty"`
	LeaseUntil string `json:"leaseUntil,omitempty"`
	// SessionID is the session the run writes into (created on the first attempt, kept
	// for retries).
	SessionID string `json:"sessionId,omitempty"`
	// ResultAt is the result checkpoint: when the agent's turn finished (ISO).
	ResultAt string `json:"resultAt,omitempty"`
	// Output is the agent's reply.
	Output string `json:"output,omitempty"`
	// Silent: the reply was [[SILENT]] - nothing to report, so nothing is delivered.
	Silent bool `json:"silent,omitempty"`
	// Action (A-103) is what the run did ("tool": a tool job's stored call); empty is a
	// prompt run.
	Action ActionKind `json:"action,omitempty"`
	Usage  *Usage     `json:"usage,omitempty"`
	// StopReason is the A-88 stop reason of the turn ("max_tokens" or "error") when it
	// was not a normal finish.
	StopReason string `json:"stopReason,omitempty"`
	Error      string `json:"error,omitempty"`
	// Reason is why it was blocked or suppressed.
	Reason string `json:"reason,omitempty"`
	// DeliveredAt is the in-app delivery: when the result was recorded where people read
	// it (ISO). Separate from the run status.
	DeliveredAt string `json:"deliveredAt,omitempty"`
	// ReplayOf is set on a run made by replaying another run's events.
	ReplayOf string `json:"replayOf,omitempty"`
	// CreatedAt, StartedAt, FinishedAt and UpdatedAt are ISO timestamps.
	CreatedAt  string `json:"createdAt"`
	StartedAt  string `json:"startedAt,omitempty"`
	FinishedAt string `json:"finishedAt,omitempty"`
	UpdatedAt  string `json:"updatedAt"`
}

// ---------------------------------------------------------------------------
// Inputs
// ---------------------------------------------------------------------------

// CoalesceInput is a partial Coalesce: a nil field keeps the base value (the default on
// create, the job's value on update).
type CoalesceInput struct {
	QuietMs   *int `json:"quietMs,omitempty"`
	MaxWaitMs *int `json:"maxWaitMs,omitempty"`
	MaxBatch  *int `json:"maxBatch,omitempty"`
}

// LimitsInput is a partial Limits: a nil field keeps the base value.
type LimitsInput struct {
	MaxRunsPerHour *int `json:"maxRunsPerHour,omitempty"`
	// ClearMaxRunsPerHour removes the hourly limit (the wire's "maxRunsPerHour": null);
	// it wins over MaxRunsPerHour.
	ClearMaxRunsPerHour bool `json:"-"`
	MaxConcurrentRuns   *int `json:"maxConcurrentRuns,omitempty"`
	MaxAttempts         *int `json:"maxAttempts,omitempty"`
	RunDeadlineMs       *int `json:"runDeadlineMs,omitempty"`
	MaxTokensPerDay     *int `json:"maxTokensPerDay,omitempty"`
	// ClearMaxTokensPerDay removes the token budget (the wire's "maxTokensPerDay": null);
	// it wins over MaxTokensPerDay.
	ClearMaxTokensPerDay bool `json:"-"`
	CooldownMs           *int `json:"cooldownMs,omitempty"`
	MaxDepth             *int `json:"maxDepth,omitempty"`
	// MaxRunMs and PauseAfterFailures (A-103).
	MaxRunMs           *int `json:"maxRunMs,omitempty"`
	PauseAfterFailures *int `json:"pauseAfterFailures,omitempty"`
}

// CreateInput is what a caller may set when creating a job. Omitted coalescing / limits
// use the defaults for its kind.
type CreateInput struct {
	Name        string  `json:"name"`
	Description string  `json:"description,omitempty"`
	When        When    `json:"when"`
	Filter      *Filter `json:"filter,omitempty"`
	Prompt      string  `json:"prompt"`
	// Enabled defaults to true when nil.
	Enabled *bool `json:"enabled,omitempty"`
	// SessionMode defaults to SessionNew when empty.
	SessionMode SessionMode    `json:"sessionMode,omitempty"`
	Coalesce    *CoalesceInput `json:"coalesce,omitempty"`
	Limits      *LimitsInput   `json:"limits,omitempty"`
	// CreatedBy defaults to CreatedByUser when empty. The manage_job tool sets
	// CreatedByAgent.
	CreatedBy          CreatedBy `json:"createdBy,omitempty"`
	CreatedInSessionID string    `json:"createdInSessionId,omitempty"`
	// Delivery defaults to none. When is DeliverAlways when empty.
	// Update (A-127): Deliveries, a list.
	Deliveries Deliveries `json:"deliveries,omitempty"`
	// Action (A-103) defaults to a prompt run.
	Action *Action `json:"action,omitempty"`
}

// UpdateInput is a partial update: a nil field is left as it is. Any change bumps the
// version. A changed schedule or a re-enable recomputes NextRunAt (counted from now - no
// catch-up for the time it was off).
type UpdateInput struct {
	Name *string `json:"name,omitempty"`
	// Description replaces the description; an empty one removes it.
	Description *string        `json:"description,omitempty"`
	Enabled     *bool          `json:"enabled,omitempty"`
	When        *When          `json:"when,omitempty"`
	Filter      *Filter        `json:"filter,omitempty"`
	Prompt      *string        `json:"prompt,omitempty"`
	SessionMode *SessionMode   `json:"sessionMode,omitempty"`
	Coalesce    *CoalesceInput `json:"coalesce,omitempty"`
	Limits      *LimitsInput   `json:"limits,omitempty"`
	// Delivery replaces the job's delivery.
	// Update (A-127): Deliveries replaces the job's deliveries; nil leaves them, an
	// empty list removes them.
	Deliveries Deliveries `json:"deliveries,omitempty"`
	// ClearDelivery removes the delivery (the wire's "delivery": null); it wins over
	// Delivery.
	// Update (A-127): ClearDeliveries removes them all (the wire's "deliveries": null or []).
	ClearDeliveries bool `json:"-"`
	// Action (A-103) replaces the job's action; a prompt action turns a tool job back
	// into a prompt job.
	Action *Action `json:"action,omitempty"`
	// ClearAction turns a tool job back into a prompt job (the wire's "action": null); it
	// wins over Action.
	ClearAction bool `json:"-"`
}

// TestEventInput is a test event sent by hand (the Jobs page's "Send test event").
type TestEventInput struct {
	Type string `json:"type"`
	// Subject is optional; when set it must not be blank.
	Subject  string   `json:"subject,omitempty"`
	Revision *float64 `json:"revision,omitempty"`
	// Data is any JSON-encodable value.
	Data any `json:"data,omitempty"`
}

// AcceptResult is what JobStore.Accept did with an incoming event.
type AcceptResult struct {
	// Duplicate: the source event was already in the inbox; nothing changed.
	Duplicate bool  `json:"duplicate"`
	Event     Event `json:"event"`
	// Run is the run the event joined, when it was accepted.
	Run *Run `json:"run,omitempty"`
}

// WebhookRequest is an inbound webhook request, passed through by the host. Body is the
// raw bytes the signature covers.
type WebhookRequest struct {
	Method string `json:"method"`
	// Headers has lower-case names.
	Headers map[string]string `json:"headers"`
	Body    []byte            `json:"body"`
}

// WebhookResponse is what the host answers a webhook request with.
type WebhookResponse struct {
	Status int `json:"status"`
	// Body is JSON-encoded by the host; nil = no body.
	Body any `json:"body,omitempty"`
}

// ---------------------------------------------------------------------------
// Runner
// ---------------------------------------------------------------------------

// RunContext is what the Runner receives for one attempt of a run. The attempt's
// context.Context is cancelled when the run is stopped, the agent is disposed, or the
// lease is lost.
type RunContext struct {
	AgentID string   `json:"agentId"`
	JobID   string   `json:"jobId"`
	JobName string   `json:"jobName"`
	RunID   string   `json:"runId"`
	Cause   RunCause `json:"cause"`
	Attempt int      `json:"attempt"`
	// SessionID is the session to run in - already created and owned by the agent.
	SessionID string `json:"sessionId"`
	// Prompt is the message the agent receives: the job's prompt, plus (event and
	// webhook jobs) the untrusted event block.
	Prompt string `json:"prompt"`
}

// RunResult is what a Runner returns.
type RunResult struct {
	// Output is the agent's reply.
	Output string `json:"output,omitempty"`
	Usage  *Usage `json:"usage,omitempty"`
	// Aborted is true when the run was stopped before it finished.
	Aborted    bool   `json:"aborted,omitempty"`
	StopReason string `json:"stopReason,omitempty"`
}

// Runner runs one agent turn for a job run: rc.Prompt as the user message in
// rc.SessionID. Supplied by the host (aec.AgentCore.UseJobRunner) so the turn takes the
// host's per-session lock, stop registry, traces and usage. An error fails the attempt;
// a *kernel.SDKError (or a class embedding it) with Retryable false is not retried. The
// runner should pass rc.SessionID as the run's session (A-97).
type Runner func(ctx context.Context, rc RunContext) (RunResult, error)

// ---------------------------------------------------------------------------
// API
// ---------------------------------------------------------------------------

// Jobs is an agent's jobs - the API the host routes and the job tools use. Every call is
// scoped to the owning agent; an id from another agent is not found (an *Error with
// CodeNotFound).
type Jobs interface {
	// List returns the agent's jobs, oldest created first.
	List(ctx context.Context) ([]Job, error)
	// Get returns the job, or nil and a nil error when the agent has no such job.
	Get(ctx context.Context, id string) (*Job, error)
	// Create validates the configuration (an *Error otherwise). A webhook job gets a
	// fresh secret.
	Create(ctx context.Context, in CreateInput) (Job, error)
	// Update applies a partial update.
	Update(ctx context.Context, id string, in UpdateInput) (Job, error)
	// Delete removes the job, its inbox and run history; it stops its runs in progress.
	Delete(ctx context.Context, id string) error
	// RotateSecret sets a new webhook secret; the old one stops working at once.
	RotateSecret(ctx context.Context, id string) (Job, error)
	// RunNow queues a run now, outside the schedule (schedule jobs; event and webhook
	// jobs take a test event).
	RunNow(ctx context.Context, id string) (Run, error)
	// SendTestEvent accepts a hand-made event (source "manual") through the same filter
	// and inbox (event and webhook jobs).
	SendTestEvent(ctx context.Context, id string, in TestEventInput) (AcceptResult, error)
	// HandleWebhook verifies, parses and accepts a webhook delivery. It returns only
	// after the event is stored; a store failure is an error (answer 5xx, so the sender
	// keeps the delivery).
	HandleWebhook(ctx context.Context, id string, req WebhookRequest) (WebhookResponse, error)
	// ListEvents returns the job's events, newest received first; limit is clamped to
	// [1, 200] and 0 means 50.
	ListEvents(ctx context.Context, id string, limit int) ([]Event, error)
	// ListRuns returns the job's runs, newest queued first; limit as ListEvents.
	ListRuns(ctx context.Context, id string, limit int) ([]Run, error)
	// GetRun returns the run, or nil and a nil error.
	GetRun(ctx context.Context, runID string) (*Run, error)
	// GetRunEvents returns the inbox events a run handles.
	GetRunEvents(ctx context.Context, runID string) ([]Event, error)
	// StopRun stops a running or queued run. False when it had already ended.
	StopRun(ctx context.Context, runID string) (bool, error)
	// ReplayRun queues a new run over the same events (same event ids, ReplayOf = runID).
	ReplayRun(ctx context.Context, runID string) (Run, error)
}
