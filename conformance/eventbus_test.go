package conformance_test

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"testing"

	"go.uber.org/goleak"

	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/conformance"
)

// TestMain fails the package if any suite leaves a goroutine running. When
// AEC_CONFORMANCE_HELPER names a command, the test binary acts as that command and
// exits before any test (and before goleak) runs: the host sandbox suite maps its
// echo, pwd, false, cat, printenv, sleep and yes to this binary (helper process).
// The helper mode is refused during a real go test run: go test always passes -test.*
// flags, while the helpers are started with their own command arguments, so a stray
// AEC_CONFORMANCE_HELPER in the environment makes the binary exit 2 with a message instead of
// reporting ok with no test run (Phase 12, T-12-09).
func TestMain(m *testing.M) {
	if mode := os.Getenv("AEC_CONFORMANCE_HELPER"); mode != "" {
		if hasTestFlag(os.Args[1:]) {
			_, _ = os.Stderr.WriteString("AEC_CONFORMANCE_HELPER is set during a go test run; unset it (it turns this test binary into a helper process)\n")
			os.Exit(2)
		}
		os.Exit(runConformanceHelper(mode, os.Args[1:]))
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
// go test does (with a -test.* flag) while AEC_CONFORMANCE_HELPER is set. The binary
// must refuse with exit code 2 and a stderr line naming the variable, not report ok
// with no test run (Phase 12, T-12-09).
func TestHelperVariableDuringTestRunFails(t *testing.T) {
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), "AEC_CONFORMANCE_HELPER=echo")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 2 {
		t.Fatalf("exit = %v, want exit code 2 (stderr %q)", err, stderr.String())
	}
	if !strings.Contains(stderr.String(), "AEC_CONFORMANCE_HELPER is set during a go test run") {
		t.Fatalf("stderr %q does not name the variable", stderr.String())
	}
}

// quiet keeps the recovered panic of the isolation subtest out of the test output.
func quiet() events.Option {
	return events.WithLogger(slog.New(slog.DiscardHandler))
}

func TestEventBusDispatcher(t *testing.T) {
	conformance.EventBus(t, func(_ *testing.T) events.Bus { return events.New(quiet()) })
}

func TestEventBusDurability(t *testing.T) {
	// One shared log for the whole suite run, so each new bus sees the same events.
	log := events.NewLog()
	conformance.EventBusDurability(t, func(_ *testing.T) events.Bus {
		return events.New(events.WithBacking(log), quiet())
	})
}
