package job

import "github.com/Phong95/aec-agent-go/kernel"

// Update (A-126): one error for jobs - the promptjob.Error (ScheduledJobError, A-69) and
// trigger.Error (TriggerError, A-70) merged when scheduled jobs and triggers became one
// Job concept.

// ErrorCode is the failure category of an Error.
type ErrorCode string

// The Error codes. The string values are stable wire values, the codes of core/ts
// JobError.
const (
	// CodeNotFound: no job (or run) with that id for this agent.
	CodeNotFound ErrorCode = "not_found"
	// CodeNameConflict: the agent already has a job with that name.
	CodeNameConflict ErrorCode = "name_conflict"
	// CodeInvalidArgument: a malformed name, prompt, schedule (a bad cron expression, an
	// unknown timezone, an interval that is too short, a time in the past), source,
	// filter, coalescing or limits.
	CodeInvalidArgument ErrorCode = "invalid_argument"
	// CodeLimitReached: the agent already has MaxJobsPerAgent jobs.
	CodeLimitReached ErrorCode = "limit_reached"
)

// Error is a failure of a job configuration call (the JobError of core/ts). Every code
// is deterministic - retrying the same call cannot succeed - so an Error is never
// retryable.
type Error struct {
	kernel.SDKError
	Code ErrorCode
	// JobID is the job the failure is about, when one was resolved ("" otherwise).
	JobID string
}

// NewError creates an Error.
func NewError(message string, code ErrorCode, jobID string) *Error {
	return &Error{SDKError: kernel.SDKError{Message: message, Retryable: false}, Code: code, JobID: jobID}
}

// Name returns the class name ("JobError"), the name core/ts uses.
func (e *Error) Name() string { return "JobError" }

// Unwrap returns the embedded SDKError so errors.As to *kernel.SDKError works.
func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return &e.SDKError
}

// Is matches an *Error target with an empty Code (class level) or an equal Code.
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return e != nil && ok && t != nil && (t.Code == "" || t.Code == e.Code)
}
