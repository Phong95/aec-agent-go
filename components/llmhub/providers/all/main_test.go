package all_test

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain fails the package if any test leaves a goroutine running. The registry
// tests build providers without sending a request, and the live tests skip unless
// they are opted in, so nothing here may keep a goroutine or a connection open.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
