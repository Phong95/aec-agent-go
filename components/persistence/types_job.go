package persistence

import (
	"context"

	"github.com/Phong95/aec-agent-go/components/agent/job"
)

// ---------------------------------------------------------------------------
// Trigger persistence (A-70) - the triggers, their durable inbox and their run queue /
// history. Same pluggable-store pattern as ScheduledJobStore, plus a few operations that
// MUST be atomic in a durable implementation (one transaction each), because the
// agent's trigger manager's guarantees rest on them:
//
//   - Accept:    dedupe + insert the event + join or open its run, together. A host
//     acknowledges a webhook only after this returns.
//   - Claim:     take one due run (or a run whose lease expired) and lease it, so two
//     workers never run the same run at once (Postgres: SELECT ... FOR UPDATE SKIP
//     LOCKED).
//   - UpdateRun: a write guarded by the lease owner, so a worker that lost its lease
//     cannot overwrite the new holder's work.
//
// Triggers are agent-owned: Trigger.AgentID is stamped at creation and every list path
// is filtered by agent. Reads by id return any agent's records - the manager checks
// ownership, so stores stay simple.
//
// Trigger.Secret is a webhook signing secret: a durable store should encrypt it.
//
// Update (A-126): job persistence - scheduled jobs (A-69) and triggers (A-70) are one
// job.Job, so ScheduledJobStore and TriggerStore merged into this JobStore. Every run, a
// scheduled one too, goes through the inbox and queue above; a clock tick is an event
// deduped on its scheduled time. UpdateJobState writes only a job's run state (next run,
// last status, reuse session), so a worker never overwrites a person's edit made while a
// run went on.
// ---------------------------------------------------------------------------

// JobRunPlan is the run an accepted event is queued in (AcceptJobEventInput.Run).
//   - The open run ("pending", not leased) of the same trigger + BucketKey takes it: the
//     event id is appended, revision and depth become the max, NotBefore =
//     min(JoinNotBefore, run.Deadline).
//   - Otherwise NewRun (EventIDs = [event.ID]) is inserted.
//   - A run that now holds MaxBatch events closes: its status becomes "waiting"
//     (claimable at its NotBefore, takes no more events); a new run with MaxBatch <= 1 is
//     "waiting" at once.
//
// At most one open run exists per trigger + BucketKey (Postgres: a partial unique index
// on (trigger_id, bucket_key) WHERE status = 'pending'; a racing insert that hits it
// retries the accept).
type JobRunPlan struct {
	NewRun        job.Run `json:"newRun"`
	JoinNotBefore string  `json:"joinNotBefore"`
	MaxBatch      int     `json:"maxBatch"`
}

// AcceptJobEventInput is what JobStore.Accept stores.
type AcceptJobEventInput struct {
	// Event is the event, already filtered (status accepted / filtered / suppressed).
	Event job.Event `json:"event"`
	// Run is, for an accepted event, the run to queue it in; nil for the others.
	Run *JobRunPlan `json:"run,omitempty"`
}

// ClaimJobRunInput is what JobStore.Claim leases.
type ClaimJobRunInput struct {
	AgentID string `json:"agentId"`
	// Now is the ISO now.
	Now        string `json:"now"`
	Owner      string `json:"owner"`
	LeaseUntil string `json:"leaseUntil"`
	// ExcludeJobIDs are triggers already at their concurrency limit.
	ExcludeJobIDs []string `json:"excludeJobIds,omitempty"`
}

// JobRunField names a run field a JobRunPatch can clear.
type JobRunField string

// The clearable run fields.
const (
	JobRunError  JobRunField = "error"
	JobRunReason JobRunField = "reason"
)

// JobRunPatch is the fields a run update may set. A nil field is left alone; Clear
// lists fields to remove; ReleaseLease clears LeaseOwner / LeaseUntil.
type JobRunPatch struct {
	Status    *job.RunStatus `json:"status,omitempty"`
	NotBefore *string        `json:"notBefore,omitempty"`
	Attempts  *int           `json:"attempts,omitempty"`
	SessionID *string        `json:"sessionId,omitempty"`
	StartedAt *string        `json:"startedAt,omitempty"`
	ResultAt  *string        `json:"resultAt,omitempty"`
	Output    *string        `json:"output,omitempty"`
	Silent    *bool          `json:"silent,omitempty"`
	// Action (A-103) records what the run did ("tool").
	Action      *job.ActionKind `json:"action,omitempty"`
	Usage       *job.Usage      `json:"usage,omitempty"`
	StopReason  *string         `json:"stopReason,omitempty"`
	Error       *string         `json:"error,omitempty"`
	Reason      *string         `json:"reason,omitempty"`
	DeliveredAt *string         `json:"deliveredAt,omitempty"`
	FinishedAt  *string         `json:"finishedAt,omitempty"`

	ReleaseLease bool          `json:"releaseLease,omitempty"`
	Clear        []JobRunField `json:"clear,omitempty"`
}

// JobUsage is what JobStore.UsageSince counts.
type JobUsage struct {
	Runs   int `json:"runs"`
	Tokens int `json:"tokens"`
}

// JobStatePatch is a job's run state, written by the clock and the worker (A-126). A nil
// field is left alone; ClearLastError removes the last error. UpdatedAt is not touched:
// these are not edits.
type JobStatePatch struct {
	// NextRunAt sets the next run time; ClearNextRunAt sets it to null.
	NextRunAt      *string `json:"nextRunAt,omitempty"`
	ClearNextRunAt bool    `json:"-"`
	// Enabled: the clock turns a one-off ("at") job off once it has fired.
	Enabled        *bool          `json:"enabled,omitempty"`
	LastRunAt      *string        `json:"lastRunAt,omitempty"`
	LastStatus     *job.RunStatus `json:"lastStatus,omitempty"`
	LastError      *string        `json:"lastError,omitempty"`
	ClearLastError bool           `json:"-"`
	// SessionID is the session a "reuse" job's runs append to.
	SessionID *string `json:"sessionId,omitempty"`
}

// JobStore persists jobs, their inbox and their runs.
type JobStore interface {
	// -- Jobs --

	// Save inserts or replaces the trigger by id.
	Save(ctx context.Context, t job.Job) error
	// Get returns the trigger, or a nil pointer and a nil error when the id is unknown.
	Get(ctx context.Context, id string) (*job.Job, error)
	// FindByName returns the agent's trigger with that name, or nil and a nil error.
	// Names are unique per agent.
	FindByName(ctx context.Context, agentID, name string) (*job.Job, error)
	// ListByAgent returns one agent's triggers, oldest created first.
	ListByAgent(ctx context.Context, agentID string) ([]job.Job, error)
	// Delete removes the trigger with its events and runs.
	Delete(ctx context.Context, id string) error
	// UpdateJobState applies patch to a job's run state (A-126). It returns the job, or
	// nil and a nil error when it no longer exists.
	UpdateJobState(ctx context.Context, id string, patch JobStatePatch) (*job.Job, error)
	// DeleteByAgent removes every trigger (with events and runs) of one agent (the agent
	// was deleted).
	DeleteByAgent(ctx context.Context, agentID string) error

	// -- Inbox and queue --

	// Accept, atomically: if an event with the same TriggerID + Source + SourceEventID is
	// stored, it returns it with Duplicate = true and changes nothing. Otherwise it
	// inserts the event, queues an accepted one (see JobRunPlan), sets Event.RunID,
	// and returns both.
	Accept(ctx context.Context, in AcceptJobEventInput) (job.AcceptResult, error)
	// Claim atomically leases one run of the agent: "pending" / "waiting" (not leased)
	// with NotBefore <= Now, or "running" with LeaseUntil < Now (its worker died).
	// Soonest NotBefore first. It sets status "running", LeaseOwner, LeaseUntil,
	// Attempts + 1 and UpdatedAt (StartedAt is set by the manager once the turn really
	// starts). Nil when none is due.
	Claim(ctx context.Context, in ClaimJobRunInput) (*job.Run, error)
	// RenewLease extends a lease the owner still holds (the run is "running" and leased
	// by owner). False when it lost it.
	RenewLease(ctx context.Context, runID, owner, leaseUntil string) (bool, error)
	// UpdateRun applies patch (and UpdatedAt = now) when the guard holds: with an owner,
	// the run is leased by it; with owner "", the run is "pending" or "waiting" and not
	// leased. It returns the updated run, or nil when the run is unknown or the guard
	// failed.
	UpdateRun(ctx context.Context, runID, owner string, patch JobRunPatch, now string) (*job.Run, error)
	// InsertRun inserts a run (a replay).
	InsertRun(ctx context.Context, r job.Run) error

	// -- History --

	// GetRun returns the run, or nil and a nil error.
	GetRun(ctx context.Context, id string) (*job.Run, error)
	// ListRuns returns a trigger's runs, newest created first; limit 0 means all.
	ListRuns(ctx context.Context, jobID string, limit int) ([]job.Run, error)
	// HasActiveRun says whether the job has a run queued or running ("pending",
	// "waiting" or "running") (A-126).
	HasActiveRun(ctx context.Context, jobID string) (bool, error)
	// ListEvents returns a trigger's events, newest received first; limit 0 means all.
	ListEvents(ctx context.Context, jobID string, limit int) ([]job.Event, error)
	// GetEvents returns events by id, in the order given (unknown ids are skipped).
	GetEvents(ctx context.Context, ids []string) ([]job.Event, error)
	// UsageSince counts the runs whose turn started at or after since (StartedAt) and
	// the tokens they used - for the rate and token limits.
	UsageSince(ctx context.Context, jobID, since string) (JobUsage, error)
	// LastSucceeded returns the newest succeeded run of a trigger + bucketKey (by
	// FinishedAt) - for stale revisions and cooldown - or nil.
	LastSucceeded(ctx context.Context, jobID, bucketKey string) (*job.Run, error)
	// Prune keeps the newest keepEvents events and keepRuns finished runs of a job.
	// Queued and running runs, and the events they hold, are kept.
	Prune(ctx context.Context, jobID string, keepEvents, keepRuns int) error
}
