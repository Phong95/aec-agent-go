package agent_test

import (
	"os"
	"testing"

	"go.uber.org/goleak"

	"github.com/Phong95/aec-agent-go/internal/mcptest"
)

// TestMain fails the package if any test leaves a goroutine running. goleak is
// never called inside a testing/synctest bubble: there it would report the
// bubble's own goroutines.
func TestMain(m *testing.M) {
	// A-208: with AEC_FAKE_MCP=stdio the test binary is the fake MCP server of the MCP
	// manager tests (a helper process), not a test run.
	if os.Getenv(mcptest.HelperEnv) == "stdio" {
		os.Exit(mcptest.ServeStdio(os.Stdin, os.Stdout))
	}
	goleak.VerifyTestMain(m)
}
