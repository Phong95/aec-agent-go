package host_test

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"

	"go.uber.org/goleak"
)

// TestMain fails the package if any test leaves a goroutine running. The host
// view starts none of its own that outlive a call; a leak would mean a file handle,
// a root or a command was kept alive by a goroutine.
//
// It also turns the test binary into the commands the exec tests run: when
// AEC_HOST_HELPER names a mode the binary acts as that program and exits before any
// test (and before goleak) runs, the standard helper-process pattern. The variable
// reaches the command through ExecRequest.Env, so no server variable is needed.
// The helper mode is refused during a real go test run: go test always passes -test.*
// flags, while the helpers are started with their own command arguments, so a stray
// AEC_HOST_HELPER in the environment makes the binary exit 2 with a message instead of
// reporting ok with no test run (Phase 12, T-12-09).
func TestMain(m *testing.M) {
	if mode := os.Getenv("AEC_HOST_HELPER"); mode != "" {
		if hasTestFlag(os.Args[1:]) {
			_, _ = os.Stderr.WriteString("AEC_HOST_HELPER is set during a go test run; unset it (it turns this test binary into a helper process)\n")
			os.Exit(2)
		}
		os.Exit(runHelper(mode))
	}
	goleak.VerifyTestMain(m)
}

// hasTestFlag reports whether any argument is a -test.* flag. go test always passes
// some (at least -test.paniconexit0 and -test.timeout), while the helper processes
// are started with their own command arguments and never with one.
func hasTestFlag(args []string) bool {
	for _, a := range args {
		if strings.HasPrefix(a, "-test.") {
			return true
		}
	}
	return false
}

// TestHelperVariableDuringTestRunFails runs this package's own test binary the way
// go test does (with a -test.* flag) while AEC_HOST_HELPER is set. The binary must
// refuse with exit code 2 and a stderr line naming the variable, not report ok with
// no test run (Phase 12, T-12-09).
func TestHelperVariableDuringTestRunFails(t *testing.T) {
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), "AEC_HOST_HELPER=echo")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 2 {
		t.Fatalf("exit = %v, want exit code 2 (stderr %q)", err, stderr.String())
	}
	if !strings.Contains(stderr.String(), "AEC_HOST_HELPER is set during a go test run") {
		t.Fatalf("stderr %q does not name the variable", stderr.String())
	}
}
