package clonecheck_test

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain fails the package if any test leaves a goroutine running (Phase 12, REL-04).
func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }
