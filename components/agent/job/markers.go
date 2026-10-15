package job

import (
	"strings"

	"github.com/Phong95/aec-agent-go/internal/jsstr"
)

// ---------------------------------------------------------------------------
// Reply markers (A-126)
// ---------------------------------------------------------------------------

// SilentMarker is a run's whole reply when there is nothing to report: the run succeeds
// and nothing is delivered.
const SilentMarker = "[[SILENT]]"

// FailedMarker starts a reply that says the run failed, for the reason after it (not
// retried).
const FailedMarker = "[[FAILED]]"

// OutcomeKind is what a run's reply says about its own outcome.
type OutcomeKind string

// The outcome kinds.
const (
	OutcomeReply  OutcomeKind = "reply"
	OutcomeSilent OutcomeKind = "silent"
	OutcomeFailed OutcomeKind = "failed"
)

// ReplyOutcome is what a run's reply says about its own outcome; Reason is set for
// OutcomeFailed.
type ReplyOutcome struct {
	Kind   OutcomeKind `json:"kind"`
	Reason string      `json:"reason,omitempty"`
}

// ReadReply reads the reply markers of a run's reply (the replyOutcome of core/ts).
func ReadReply(output string) ReplyOutcome {
	text := jsstr.Trim(output)
	if text == SilentMarker {
		return ReplyOutcome{Kind: OutcomeSilent}
	}
	if rest, ok := strings.CutPrefix(text, FailedMarker); ok {
		reason := jsstr.Trim(rest)
		if reason == "" {
			reason = "The run reported a failure"
		}
		return ReplyOutcome{Kind: OutcomeFailed, Reason: reason}
	}
	return ReplyOutcome{Kind: OutcomeReply}
}
