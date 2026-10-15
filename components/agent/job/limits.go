package job

import "time"

// Limits of the triggers of one agent and of their events. They are the constants of
// core/ts (matching.ts, webhook.ts, trigger-manager.ts).
// Update (A-126): of the jobs of one agent (core/ts rules.ts, webhook.ts, job-manager.ts).
const (
	// MaxJobsPerAgent is the most jobs one agent may have.
	MaxJobsPerAgent = 50
	// MaxNameLength, MaxPromptLength and MaxDescriptionLength bound a job's text, in
	// characters (UTF-16 units, as core/ts counts).
	MaxNameLength        = 80
	MaxPromptLength      = 8000
	MaxDescriptionLength = 500
	// MinIntervalMs is the shortest every interval, so an agent cannot schedule a turn
	// every second.
	MinIntervalMs = 60_000
	// MaxEventDataBytes is the largest event data kept in the inbox, as JSON.
	MaxEventDataBytes = 64 * 1024
	// MaxWebhookBodyBytes is the largest webhook body read.
	MaxWebhookBodyBytes = 256 * 1024
	// WebhookTolerance: a Standard Webhooks signature older or newer than this is refused.
	WebhookTolerance = 5 * time.Minute

	// LeaseDuration is how long a claimed run is leased; renewed every third of it while
	// the turn runs.
	LeaseDuration = 60 * time.Second
	// PollInterval is how often the worker looks for due runs it was not told about
	// (other processes, expired leases).
	PollInterval = 10 * time.Second
	// RetryBase is the first retry delay; it doubles per attempt, with +-25% jitter, at
	// most MaxRetryDelay.
	RetryBase = 30 * time.Second
	// MaxRetryDelay caps the retry delay.
	MaxRetryDelay = 15 * time.Minute
	// MaxParallelRuns is the job runs of one agent at once in this process.
	// Update (A-126): 4 (was 2 for triggers alone), now that scheduled runs share it.
	MaxParallelRuns = 4
	// KeepEvents and KeepRuns are the history kept per job.
	KeepEvents = 200
	KeepRuns   = 100
)

// InternalEventTypes returns the SDK event types an "event" job may listen for, as a
// fresh slice. Deliberately narrow: no every-tool-error or every-turn-complete
// subscriptions, and never trigger:* events, so a trigger's own runs cannot set off
// another trigger.
// Update (A-126): job.* are the agent's own job runs ending - the loop guard
// (Limits.MaxDepth) keeps an event job's runs from setting off another event job.
func InternalEventTypes() []string {
	return []string{"job.completed", "job.failed", "job.stopped", "channel.error"}
}

// A-103 limits.
const (
	// MaxToolArgsBytes is the largest tool-job arguments, as JSON.
	MaxToolArgsBytes = 16 * 1024
	// DefaultMaxRunMs is a run's default time limit (Limits.MaxRunMs).
	DefaultMaxRunMs = 30 * 60_000
	// DefaultPauseAfterFailures is the failed runs in a row that pause a job by default
	// (Limits.PauseAfterFailures).
	DefaultPauseAfterFailures = 5
)

// DefaultCoalesce returns the coalescing a job gets when it sets none.
func DefaultCoalesce() Coalesce {
	return Coalesce{QuietMs: 5_000, MaxWaitMs: 60_000, MaxBatch: 20}
}

// DefaultEventLimits returns the limits of an event or webhook job that sets none (A-70's
// trigger defaults; no token budget).
func DefaultEventLimits() Limits {
	hourly, runMs, pause := 10, DefaultMaxRunMs, DefaultPauseAfterFailures
	return Limits{MaxRunsPerHour: &hourly, MaxConcurrentRuns: 1, MaxAttempts: 3, RunDeadlineMs: 60 * 60_000, CooldownMs: 0, MaxDepth: 0, MaxRunMs: &runMs, PauseAfterFailures: &pause}
}

// DefaultScheduleLimits returns the limits of a schedule job that sets none: its clock
// bounds it (no hourly limit), and a failed run is not retried - the next tick is the
// retry (A-126 decision D2).
func DefaultScheduleLimits() Limits {
	runMs, pause := DefaultMaxRunMs, DefaultPauseAfterFailures
	return Limits{MaxConcurrentRuns: 1, MaxAttempts: 1, RunDeadlineMs: 60 * 60_000, CooldownMs: 0, MaxDepth: 0, MaxRunMs: &runMs, PauseAfterFailures: &pause}
}

// DefaultLimits returns the default limits for a job that runs w.
func DefaultLimits(w When) Limits {
	if w.IsSchedule() {
		return DefaultScheduleLimits()
	}
	return DefaultEventLimits()
}
