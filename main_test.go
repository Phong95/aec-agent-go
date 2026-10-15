package aec_test

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain fails the package if any test leaves a goroutine running. goleak is
// never called inside a testing/synctest bubble: there it would report the
// bubble's own goroutines.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
