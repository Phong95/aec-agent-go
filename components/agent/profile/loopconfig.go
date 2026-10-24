package profile

import "time"

// The agent-loop defaults. They are what an unset or invalid
// LoopConfig field falls back to; an agent overrides them through
// AgentProfile.LoopConfig.
const (
	// DefaultMaxIterations is the model/tool round-trips per turn.
	DefaultMaxIterations = 100
	// DefaultMaxToolRetries is the retries per failing tool or skill call, so
	// the total attempts are retries + 1.
	DefaultMaxToolRetries = 2
	// DefaultRetryBackoffMs is the base delay of the exponential backoff in
	// milliseconds, delay = base x 2^attempt.
	DefaultRetryBackoffMs = 500
	// DefaultMaxConcurrentTurns (A-97) is the most turns one agent runs at once across
	// its sessions; 0 = no cap, every session of an agent may run a turn at the same time.
	DefaultMaxConcurrentTurns = 0
	// DefaultMaxToolResultChars (A-129 item 4) is the most characters (UTF-16 code units)
	// of one tool result the model is given (~12k tokens); the rest is cut from the middle
	// and, in an isolated sandbox, saved to a file. 0 = no limit.
	DefaultMaxToolResultChars = 50_000
)

// The smallest value each LoopConfig field accepts; a value below it is invalid
// and falls back to the default.
const (
	minMaxIterations  = 1
	minMaxToolRetries = 0
	minRetryBackoffMs = 0
	// A-97: 0 is valid (no cap).
	minMaxConcurrentTurns = 0
	// A-129: 0 is valid (no limit).
	minMaxToolResultChars = 0
)

// RetryBackoff returns RetryBackoffMs as a time.Duration.
func (c ResolvedLoopConfig) RetryBackoff() time.Duration {
	return time.Duration(c.RetryBackoffMs) * time.Millisecond
}

// RetryBackoff returns the base backoff delay this config resolves to, as a
// time.Duration: RetryBackoffMs when it is set and valid, else
// DefaultRetryBackoffMs (see ResolveLoopConfig). A nil config resolves to the
// default.
func (c *LoopConfig) RetryBackoff() time.Duration {
	return ResolveLoopConfig(c).RetryBackoff()
}

// ResolveLoopConfig resolves an agent's optional loop config into concrete limits. It
// falls back to the SDK defaults per field when a value is missing (nil, or a nil
// config) or below the field's minimum: 1 for MaxIterations, 0 for MaxToolRetries and
// RetryBackoffMs (and, since A-97, 0 for MaxConcurrentTurns; since A-129, 0 for
// MaxToolResultChars). One invalid field does not reset the others.
//
// The fields are *int, so a value is always a finite integer: there is no NaN,
// Infinity or fraction to fall back from or to floor.
func ResolveLoopConfig(c *LoopConfig) ResolvedLoopConfig {
	var maxIterations, maxToolRetries, retryBackoffMs, maxConcurrentTurns, maxToolResultChars *int
	if c != nil {
		maxIterations, maxToolRetries, retryBackoffMs = c.MaxIterations, c.MaxToolRetries, c.RetryBackoffMs
		maxConcurrentTurns = c.MaxConcurrentTurns
		maxToolResultChars = c.MaxToolResultChars
	}
	return ResolvedLoopConfig{
		MaxIterations:  resolveField(maxIterations, minMaxIterations, DefaultMaxIterations),
		MaxToolRetries: resolveField(maxToolRetries, minMaxToolRetries, DefaultMaxToolRetries),
		RetryBackoffMs: resolveField(retryBackoffMs, minRetryBackoffMs, DefaultRetryBackoffMs),
		// A-97: read on every turn by the agent's turn gate.
		MaxConcurrentTurns: resolveField(maxConcurrentTurns, minMaxConcurrentTurns, DefaultMaxConcurrentTurns),
		// A-129: read on every model turn by the agent loop.
		MaxToolResultChars: resolveField(maxToolResultChars, minMaxToolResultChars, DefaultMaxToolResultChars),
	}
}

// resolveField returns *v, or fallback when v is nil or *v is below min.
func resolveField(v *int, minimum, fallback int) int {
	if v == nil || *v < minimum {
		return fallback
	}
	return *v
}
