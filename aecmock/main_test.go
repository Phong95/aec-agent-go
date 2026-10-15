package aecmock_test

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain fails the package if any test leaves a goroutine running. The
// in-memory sandbox starts a timer and a context callback per Exec call, and none
// of them may outlive the call.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
