//go:build windows

package host

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/Phong95/aec-agent-go/components/sandboxhub"
)

// TestJobObjectStructsHaveTheWindowsSize: the hand-written
// JOBOBJECT_EXTENDED_LIMIT_INFORMATION must be exactly as big as the Windows one, or
// SetInformationJobObject refuses it. It is 144 bytes on 64-bit Windows (RESEARCH
// F-W6); the basic part is 64 and the I/O counters 48.
func TestJobObjectStructsHaveTheWindowsSize(t *testing.T) {
	if runtime.GOARCH != "amd64" {
		t.Skipf("the 144 byte layout is checked on amd64, this is %s", runtime.GOARCH)
	}
	if got := unsafe.Sizeof(basicLimitInformation{}); got != 64 {
		t.Errorf("sizeof(basicLimitInformation) = %d, want 64", got)
	}
	if got := unsafe.Sizeof(ioCounters{}); got != 48 {
		t.Errorf("sizeof(ioCounters) = %d, want 48", got)
	}
	if got := unsafe.Sizeof(extendedLimitInformation{}); got != 144 {
		t.Errorf("sizeof(extendedLimitInformation) = %d, want 144", got)
	}
	if got := unsafe.Offsetof(basicLimitInformation{}.LimitFlags); got != 16 {
		t.Errorf("offsetof(LimitFlags) = %d, want 16", got)
	}
}

// TestJobObjectKillOnJobClose: closing the last handle to the job ends what is in it
// (what tree.release does when Exec returns), the command and its grandchild, without
// any kill call.
func TestJobObjectKillOnJobClose(t *testing.T) {
	_, pidfile := treeFixture(t)
	cmd := exec.CommandContext(t.Context(), helperExe(t)) //nolint:gosec // the test binary itself
	cmd.Env = append(os.Environ(), "AEC_HOST_HELPER=spawn-grandchild", "AEC_HOST_PIDFILE="+pidfile)
	tr, err := startInTree(cmd)
	if err != nil {
		t.Fatal(err)
	}
	waited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(waited)
	}()
	child, grandchild := awaitPids(t, pidfile, true)

	tr.release()
	requireDead(t, child, "command")
	requireDead(t, grandchild, "grandchild")
	select {
	case <-waited:
	case <-time.After(pollTimeout):
		t.Error("the command was not reaped after its job was closed")
	}
	// A kill after release is a no-op, not a use of a closed handle.
	if err := tr.kill(); err != nil {
		t.Errorf("kill after release: %v", err)
	}
}

// TestJobAssignmentFailureResumesTheChildAndFallsBackToTaskkill (threat T-07-21): when
// the child cannot be put in a job it must not stay suspended, and killing it still
// takes down its grandchild through taskkill /T.
func TestJobAssignmentFailureResumesTheChildAndFallsBackToTaskkill(t *testing.T) {
	old := assignToJob
	assignToJob = func(syscall.Handle, syscall.Handle) bool { return false }
	t.Cleanup(func() { assignToJob = old })

	view, pidfile := treeFixture(t)
	req := helperRequest(t, "spawn-grandchild", map[string]string{"AEC_HOST_PIDFILE": pidfile}, 3000)
	done := startExec(t.Context(), view, req)
	// awaitPids proves the child ran at all: a process left suspended could never
	// have written its pid file.
	child, grandchild := awaitPids(t, pidfile, true)
	out := awaitExec(t, done)
	if out.err != nil {
		t.Fatalf("Exec: %v", out.err)
	}
	if !out.res.TimedOut || out.res.ExitCode != nil {
		t.Errorf("timedOut=%v exitCode=%v, want timedOut and no exit code", out.res.TimedOut, out.res.ExitCode)
	}
	requireDead(t, child, "command")
	requireDead(t, grandchild, "grandchild")
}

// TestHostExecWindowsShellLineIsVerbatim (Pitfall 4): the line reaches cmd.exe exactly
// as Node sends it, so quotes and "&" behave as typed.
func TestHostExecWindowsShellLineIsVerbatim(t *testing.T) {
	_, _, _, view := newOwnView(t)
	shell := true
	res, err := view.Exec(t.Context(), sandboxhub.ExecRequest{
		Command: []string{`echo "a  b" & echo c`}, Shell: &shell, TimeoutMs: 10_000, MaxOutputBytes: 10_000,
	}, sandboxhub.ExecOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if want := "\"a  b\" \r\nc\r\n"; res.Stdout != want {
		t.Errorf("stdout = %q, want %q", res.Stdout, want)
	}
	if res.ExitCode == nil || *res.ExitCode != 0 {
		t.Errorf("exit code = %v, want 0", res.ExitCode)
	}
}

// TestHostExecWindowsComSpecFromTheServerEnvironment: ComSpec comes from the server
// environment (Node: process.env.comspec), not from the request env.
func TestHostExecWindowsComSpecFromTheServerEnvironment(t *testing.T) {
	_, _, _, view := newOwnView(t)
	dir := t.TempDir()
	// A ComSpec that does not exist must make the shell command fail to start, which
	// proves the server variable is the one used.
	t.Setenv("ComSpec", filepath.Join(dir, "no-such-shell.exe"))
	shell := true
	res, err := view.Exec(t.Context(), sandboxhub.ExecRequest{
		Command: []string{"echo", "hi"}, Shell: &shell, TimeoutMs: 10_000, MaxOutputBytes: 10_000,
	}, sandboxhub.ExecOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode == nil || *res.ExitCode != 127 {
		t.Errorf("exit code = %v, want 127 for a missing shell", res.ExitCode)
	}
	if _, err := os.Stat(filepath.Join(dir, "no-such-shell.exe")); err == nil {
		t.Error("the fake shell exists")
	}
}

// TestNtStatusErrorIsNotAWin32Error (IN-03): an NTSTATUS is reported as such, not as a
// syscall.Errno with the text of an unrelated Win32 error.
func TestNtStatusErrorIsNotAWin32Error(t *testing.T) {
	err := ntStatusError(0xC000010A)
	if want := "NtResumeProcess failed: NTSTATUS 0xc000010a"; err.Error() != want {
		t.Errorf("error = %q, want %q", err.Error(), want)
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		t.Errorf("an NTSTATUS must not be a syscall.Errno: %v", err)
	}
}

// TestTaskkillIsResolvedThroughSystemRoot (IN-03): the fallback runs the taskkill of
// the system folder, not whichever one the server's PATH finds first.
func TestTaskkillIsResolvedThroughSystemRoot(t *testing.T) {
	t.Setenv("SystemRoot", `C:\Windows`)
	if got, want := taskkillProgram(), `C:\Windows\System32\taskkill.exe`; got != want {
		t.Errorf("taskkill program = %q, want %q", got, want)
	}
	t.Setenv("SystemRoot", "")
	if got := taskkillProgram(); got != "taskkill" {
		t.Errorf("without SystemRoot the program = %q, want the bare name", got)
	}
}
