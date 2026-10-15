package sandboxhub

import (
	"testing"
	"time"
)

// The millisecond fields read back as time.Duration values.
func TestDurationAccessors(t *testing.T) {
	if got := (Limits{MaxExecTimeoutMs: 120_000}).MaxExecTimeout(); got != 2*time.Minute {
		t.Errorf("Limits.MaxExecTimeout() = %v, want 2m", got)
	}
	if got := (ExecRequest{TimeoutMs: 1500}).Timeout(); got != 1500*time.Millisecond {
		t.Errorf("ExecRequest.Timeout() = %v, want 1.5s", got)
	}
	if got := (ExecRequest{}).Timeout(); got != 0 {
		t.Errorf("zero ExecRequest.Timeout() = %v, want 0", got)
	}
}
