package host

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Phong95/aec-agent-go/components/sandboxhub"
)

// These tests are ROADMAP success criterion 4 (SBX-03): cancelling or timing out a
// host command that started other processes leaves none of them running, also a
// grandchild whose parent already exited. The commands are the test binary itself
// (helper_process_test.go): "spawn-grandchild" starts a grandchild and writes both
// process ids to a file, so the tests know exactly which processes they started and
// only ever signal those (never a process found by name). A process still alive when
// a test ends is killed by id in a cleanup.

const (
	pollEvery   = 25 * time.Millisecond
	pollTimeout = 10 * time.Second
	// execDeadline fails a test whose Exec does not return, instead of hanging it.
	execDeadline = 45 * time.Second
)

// helperExe is the path of the test binary, which every helper command runs.
func helperExe(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return exe
}

// helperRequest is a request that runs the test binary as the given helper mode.
func helperRequest(t *testing.T, mode string, env map[string]string, timeoutMs int) sandboxhub.ExecRequest {
	t.Helper()
	all := map[string]string{"AEC_HOST_HELPER": mode}
	// Under go test -cover the helper is a coverage-instrumented binary, and the
	// host sandbox passes only its own variable list, so the helper would print
	// "warning: GOCOVERDIR not set, no coverage data emitted" to stderr on exit.
	// That line counts against MaxOutputBytes and breaks the exact output-cap
	// tests, so the helper gets a coverage directory of its own (its counters
	// are discarded with the directory; the parent's coverage is unaffected).
	if testing.CoverMode() != "" {
		all["GOCOVERDIR"] = t.TempDir()
	}
	for k, v := range env {
		all[k] = v
	}
	return sandboxhub.ExecRequest{Command: []string{helperExe(t)}, Env: all, TimeoutMs: timeoutMs, MaxOutputBytes: 10_000}
}

// execOutcome is what one Exec returned.
type execOutcome struct {
	res sandboxhub.ExecResult
	err error
	at  time.Duration
}

// startExec runs Exec on its own goroutine and returns the channel of its outcome.
func startExec(ctx context.Context, v sandboxhub.View, req sandboxhub.ExecRequest) <-chan execOutcome {
	done := make(chan execOutcome, 1)
	go func() {
		started := time.Now()
		res, err := v.Exec(ctx, req, sandboxhub.ExecOptions{})
		done <- execOutcome{res, err, time.Since(started)}
	}()
	return done
}

// awaitExec waits for the outcome of startExec and fails the test when it takes
// longer than execDeadline.
func awaitExec(t *testing.T, done <-chan execOutcome) execOutcome {
	t.Helper()
	select {
	case out := <-done:
		return out
	case <-time.After(execDeadline):
		t.Fatalf("Exec did not return within %v", execDeadline)
		return execOutcome{}
	}
}

// pidsOf reads the "<child> <grandchild>" pair a helper wrote.
func pidsOf(pidfile string) (child, grandchild int, ok bool) {
	data, err := os.ReadFile(pidfile)
	if err != nil {
		return 0, 0, false
	}
	fields := strings.Fields(string(data))
	if len(fields) != 2 {
		return 0, 0, false
	}
	child, err1 := strconv.Atoi(fields[0])
	grandchild, err2 := strconv.Atoi(fields[1])
	if err1 != nil || err2 != nil || child <= 0 || grandchild <= 0 {
		return 0, 0, false
	}
	return child, grandchild, true
}

// treeFixture prepares one process-tree test: the view, the pid file and a cleanup that
// kills, by process id, whichever of the two recorded processes is still alive.
func treeFixture(t *testing.T) (sandboxhub.View, string) {
	t.Helper()
	_, _, _, view := newOwnView(t)
	pidfile := filepath.Join(t.TempDir(), "pids")
	t.Cleanup(func() {
		child, grandchild, ok := pidsOf(pidfile)
		if !ok {
			return
		}
		for _, pid := range []int{child, grandchild} {
			if processAlive(pid) {
				if p, err := os.FindProcess(pid); err == nil {
					_ = p.Kill()
				}
			}
		}
	})
	return view, pidfile
}

// awaitPids waits for the helper to record its pids, then proves that the grandchild
// is alive, and the command too when childAlive is set (an orphan test lets the command
// exit at once): without it a kill could "pass" because the processes never started.
func awaitPids(t *testing.T, pidfile string, childAlive bool) (child, grandchild int) {
	t.Helper()
	deadline := time.Now().Add(pollTimeout)
	for time.Now().Before(deadline) {
		if c, g, ok := pidsOf(pidfile); ok {
			if !processAlive(g) || (childAlive && !processAlive(c)) {
				t.Fatalf("processes %d and %d were not alive when they started", c, g)
			}
			return c, g
		}
		time.Sleep(pollEvery)
	}
	t.Fatalf("the helper never wrote its process ids to %s", pidfile)
	return 0, 0
}

// requireDead polls until the process is gone and fails when it is still running
// after pollTimeout.
func requireDead(t *testing.T, pid int, what string) {
	t.Helper()
	deadline := time.Now().Add(pollTimeout)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return
		}
		time.Sleep(pollEvery)
	}
	t.Errorf("the %s (pid %d) is still running", what, pid)
}

// TestProcessTreeKilledOnTimeout: a command that started a grandchild is killed as a
// whole tree at its timeout.
func TestProcessTreeKilledOnTimeout(t *testing.T) {
	view, pidfile := treeFixture(t)
	req := helperRequest(t, "spawn-grandchild", map[string]string{"AEC_HOST_PIDFILE": pidfile}, 4000)
	done := startExec(t.Context(), view, req)
	child, grandchild := awaitPids(t, pidfile, true)

	out := awaitExec(t, done)
	if out.err != nil {
		t.Fatalf("Exec: %v", out.err)
	}
	if !out.res.TimedOut || out.res.Cancelled || out.res.Truncated {
		t.Errorf("flags: timedOut=%v cancelled=%v truncated=%v, want only timedOut", out.res.TimedOut, out.res.Cancelled, out.res.Truncated)
	}
	if out.res.ExitCode != nil {
		t.Errorf("a killed command has no exit code, got %d", *out.res.ExitCode)
	}
	requireDead(t, child, "command")
	requireDead(t, grandchild, "grandchild")
}

// TestProcessTreeKilledOnCancel: cancelling the context kills the whole tree.
func TestProcessTreeKilledOnCancel(t *testing.T) {
	view, pidfile := treeFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	req := helperRequest(t, "spawn-grandchild", map[string]string{"AEC_HOST_PIDFILE": pidfile}, 30_000)
	done := startExec(ctx, view, req)
	child, grandchild := awaitPids(t, pidfile, true)

	cancel()
	out := awaitExec(t, done)
	if out.err != nil {
		t.Fatalf("Exec: %v", out.err)
	}
	if !out.res.Cancelled || out.res.TimedOut || out.res.Truncated {
		t.Errorf("flags: timedOut=%v cancelled=%v truncated=%v, want only cancelled", out.res.TimedOut, out.res.Cancelled, out.res.Truncated)
	}
	if out.res.ExitCode != nil {
		t.Errorf("a killed command has no exit code, got %d", *out.res.ExitCode)
	}
	requireDead(t, child, "command")
	requireDead(t, grandchild, "grandchild")
}

// TestProcessTreeOrphanKilled: the command exits at once while its grandchild lives on
// and holds the output pipe, the case "taskkill /T" misses (the grandchild is no longer
// linked to a parent). Exec still returns - within the wait delay, long before the
// 30 s timeout - and the grandchild is dead afterwards.
func TestProcessTreeOrphanKilled(t *testing.T) {
	view, pidfile := treeFixture(t)
	req := helperRequest(t, "orphan", map[string]string{"AEC_HOST_PIDFILE": pidfile}, 30_000)
	done := startExec(t.Context(), view, req)
	_, grandchild := awaitPids(t, pidfile, false)

	out := awaitExec(t, done)
	if out.err != nil {
		t.Fatalf("Exec: %v", out.err)
	}
	if limit := execWaitDelay + 8*time.Second; out.at > limit {
		t.Errorf("Exec took %v, want it bounded by the wait delay (under %v)", out.at, limit)
	}
	if out.res.TimedOut || out.res.Cancelled || out.res.Truncated {
		t.Errorf("flags: timedOut=%v cancelled=%v truncated=%v, want none (the command exited by itself)", out.res.TimedOut, out.res.Cancelled, out.res.Truncated)
	}
	if out.res.ExitCode == nil || *out.res.ExitCode != 0 {
		t.Errorf("the command exited with 0 by itself, got %v", out.res.ExitCode)
	}
	requireDead(t, grandchild, "orphaned grandchild")
}

// TestOrphanKilledAtTimeout: the same orphan, but the timeout (1.5 s) comes before the
// wait delay: the timer kills the tree directly, although the command's own process
// is long gone and os/exec would never call cmd.Cancel for it.
func TestOrphanKilledAtTimeout(t *testing.T) {
	view, pidfile := treeFixture(t)
	req := helperRequest(t, "orphan", map[string]string{"AEC_HOST_PIDFILE": pidfile}, 1500)
	done := startExec(t.Context(), view, req)
	_, grandchild := awaitPids(t, pidfile, false)

	out := awaitExec(t, done)
	if out.err != nil {
		t.Fatalf("Exec: %v", out.err)
	}
	if !out.res.TimedOut || out.res.ExitCode != nil {
		t.Errorf("timedOut=%v exitCode=%v, want timedOut and no exit code", out.res.TimedOut, out.res.ExitCode)
	}
	requireDead(t, grandchild, "orphaned grandchild")
}
