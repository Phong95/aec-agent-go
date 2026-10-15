package host

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/kernel"
)

// The exec tests run the test binary itself as the command (helper_process_test.go)
// so that they need neither node nor coreutils and behave the same on Windows and
// Linux.

// runExec runs one Exec and fails the test when it does not return in time.
func runExec(ctx context.Context, t *testing.T, v sandboxhub.View, req sandboxhub.ExecRequest, opts sandboxhub.ExecOptions) (sandboxhub.ExecResult, error) {
	t.Helper()
	done := make(chan execOutcome, 1)
	go func() {
		res, err := v.Exec(ctx, req, opts)
		done <- execOutcome{res: res, err: err}
	}()
	out := awaitExec(t, done)
	return out.res, out.err
}

func wantExit(t *testing.T, res sandboxhub.ExecResult, code int) {
	t.Helper()
	if res.ExitCode == nil || *res.ExitCode != code {
		t.Fatalf("exit code = %v, want %d (stdout %q, stderr %q)", res.ExitCode, code, res.Stdout, res.Stderr)
	}
}

func TestPassthroughEnvIsTheGoldenList(t *testing.T) {
	wantWindows := []string{
		"SystemRoot", "windir", "ComSpec", "PATHEXT", "PATH", "Path", "TEMP", "TMP", "USERPROFILE", "APPDATA",
		"LOCALAPPDATA", "ProgramFiles", "ProgramData", "NUMBER_OF_PROCESSORS", "PROCESSOR_ARCHITECTURE",
	}
	if got := passthroughEnvFor("windows"); !slices.Equal(got, wantWindows) {
		t.Errorf("windows list = %v, want %v", got, wantWindows)
	}
	wantUnix := []string{"PATH", "HOME", "LANG", "TMPDIR", "USER", "SHELL"}
	for _, goos := range []string{"linux", "darwin", "freebsd"} {
		if got := passthroughEnvFor(goos); !slices.Equal(got, wantUnix) {
			t.Errorf("%s list = %v, want %v", goos, got, wantUnix)
		}
	}
	if len(wantWindows) != 15 || len(wantUnix) != 6 {
		t.Fatal("the lists changed size")
	}
}

// TestHostExecBuildEnvPassesOnlyTheListAndTheRequest (threat T-07-22).
func TestHostExecBuildEnvPassesOnlyTheListAndTheRequest(t *testing.T) {
	t.Setenv("AEC_HOST_SECRET", "leaked")
	t.Setenv("AEC_HOST_OTHER", "also leaked")
	env := buildEnv(map[string]string{"ZED": "z", "GREETING": "hi", "AEC_HOST_HELPER": "env"})
	for _, entry := range env {
		if strings.HasPrefix(entry, "AEC_HOST_SECRET=") || strings.HasPrefix(entry, "AEC_HOST_OTHER=") {
			t.Errorf("a server variable crossed: %q", entry)
		}
	}
	// Every entry is a passthrough variable or a request variable.
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		if !slices.Contains(passthroughEnv, key) && !slices.Contains([]string{"ZED", "GREETING", "AEC_HOST_HELPER"}, key) {
			t.Errorf("unexpected variable %q", entry)
		}
	}
	// The request keys come last, sorted.
	tail := env[len(env)-3:]
	want := []string{"AEC_HOST_HELPER=env", "GREETING=hi", "ZED=z"}
	if !slices.Equal(tail, want) {
		t.Errorf("request entries = %v, want %v", tail, want)
	}
}

// TestHostExecRequestEnvOverridesPassthrough: the request env is laid over the server
// values, so a request can set PATH for the command.
func TestHostExecRequestEnvOverridesPassthrough(t *testing.T) {
	t.Setenv("LANG", "server-lang")
	t.Setenv("PATH", "/server/path")
	env := buildEnv(map[string]string{"LANG": "request-lang"})
	last := ""
	for _, entry := range env {
		if strings.HasPrefix(entry, "LANG=") {
			last = entry
		}
	}
	if last != "LANG=request-lang" {
		t.Errorf("the last LANG entry = %q, want the request value", last)
	}
}

func TestHostExecRunsShellCommandsAndReportsExitCodes(t *testing.T) {
	_, _, _, view := newOwnView(t)
	ctx := t.Context()

	res, err := runExec(ctx, t, view, sandboxhub.ExecRequest{
		Command: []string{"echo", "hello"}, Shell: boolPtr(true), TimeoutMs: 10_000, MaxOutputBytes: 10_000,
	}, sandboxhub.ExecOptions{})
	if err != nil {
		t.Fatal(err)
	}
	wantExit(t, res, 0)
	if strings.TrimSpace(res.Stdout) != "hello" {
		t.Errorf("shell stdout = %q, want hello", res.Stdout)
	}
	if res.TimedOut || res.Cancelled || res.Truncated {
		t.Errorf("flags set on a plain run: %+v", res)
	}

	// The shell joins the argv with spaces: a word that needs the shell works.
	res, err = runExec(ctx, t, view, sandboxhub.ExecRequest{
		Command: []string{"exit", "3"}, Shell: boolPtr(true), TimeoutMs: 10_000, MaxOutputBytes: 10_000,
	}, sandboxhub.ExecOptions{})
	if err != nil {
		t.Fatal(err)
	}
	wantExit(t, res, 3)
}

func TestHostExecExitCodeOfAnArgvCommand(t *testing.T) {
	_, _, _, view := newOwnView(t)
	res, err := runExec(t.Context(), t, view, helperRequest(t, "exit3", nil, 10_000), sandboxhub.ExecOptions{})
	if err != nil {
		t.Fatal(err)
	}
	wantExit(t, res, 3)
}

func TestHostExecUnknownProgramIsExit127(t *testing.T) {
	_, _, _, view := newOwnView(t)
	res, err := runExec(t.Context(), t, view, sandboxhub.ExecRequest{
		Command: []string{"aec-no-such-program", "arg"}, TimeoutMs: 10_000, MaxOutputBytes: 10_000,
	}, sandboxhub.ExecOptions{})
	if err != nil {
		t.Fatalf("an unknown program is a result, not an error: %v", err)
	}
	wantExit(t, res, 127)
	if res.Stderr != "aec-no-such-program: command not found\n" {
		t.Errorf("stderr = %q", res.Stderr)
	}
	if res.Stdout != "" || res.TimedOut || res.Cancelled || res.Truncated {
		t.Errorf("unexpected result %+v", res)
	}

	// A program given by a path that does not exist is not found either.
	missing := filepath.Join(t.TempDir(), "nothing-here")
	res, err = runExec(t.Context(), t, view, sandboxhub.ExecRequest{
		Command: []string{missing}, TimeoutMs: 10_000, MaxOutputBytes: 10_000,
	}, sandboxhub.ExecOptions{})
	if err != nil {
		t.Fatalf("a missing path is a result, not an error: %v", err)
	}
	wantExit(t, res, 127)
	if res.Stderr != missing+": command not found\n" {
		t.Errorf("stderr = %q", res.Stderr)
	}
}

// TestHostExecStartFailureIsEnvironmentUnavailable: a program that exists but cannot
// be started (a file that is not an executable: not marked executable on Unix, not a
// valid program on Windows) is an error with the expected message form, not exit 127.
func TestHostExecStartFailureIsEnvironmentUnavailable(t *testing.T) {
	_, _, _, view := newOwnView(t)
	broken := filepath.Join(t.TempDir(), "broken.exe")
	if err := os.WriteFile(broken, []byte("this is not a program"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := runExec(t.Context(), t, view, sandboxhub.ExecRequest{Command: []string{broken}, TimeoutMs: 10_000, MaxOutputBytes: 10_000}, sandboxhub.ExecOptions{})
	requireCode(t, err, kernel.SandboxCodeEnvironmentUnavailable)
	if want := `Could not start "` + broken + `": `; !strings.HasPrefix(err.Error(), want) {
		t.Errorf("message = %q, want it to start with %q", err.Error(), want)
	}
}

func TestHostExecPassesStdin(t *testing.T) {
	_, _, _, view := newOwnView(t)
	req := helperRequest(t, "stdin-echo", nil, 10_000)
	req.Stdin = "line one\nline two \U000020ac\n"
	res, err := runExec(t.Context(), t, view, req, sandboxhub.ExecOptions{})
	if err != nil {
		t.Fatal(err)
	}
	wantExit(t, res, 0)
	if res.Stdout != req.Stdin {
		t.Errorf("stdout = %q, want %q", res.Stdout, req.Stdin)
	}
}

// TestHostExecCapCutsAtACharacter: a cap inside a character cuts before it, so the
// result never ends in a replacement character.
func TestHostExecCapCutsAtACharacter(t *testing.T) {
	_, _, _, view := newOwnView(t)
	req := helperRequest(t, "utf8-big", nil, 10_000)
	req.MaxOutputBytes = 10 // three euro signs are 9 bytes, the tenth byte starts a fourth
	var out outputRecorder
	res, err := runExec(t.Context(), t, view, req, sandboxhub.ExecOptions{OnOutput: out.record})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Truncated || res.ExitCode != nil {
		t.Errorf("truncated=%v exitCode=%v, want truncated and no exit code", res.Truncated, res.ExitCode)
	}
	if want := strings.Repeat("\U000020ac", 3); res.Stdout != want {
		t.Errorf("stdout = %q, want %q", res.Stdout, want)
	}
	if got := out.joined(sandboxhub.StreamStdout); got != res.Stdout {
		t.Errorf("OnOutput saw %q, the result is %q", got, res.Stdout)
	}
}

// TestHostExecCapIsInclusive: output exactly as long as the cap is not truncated; one
// more byte is (the next byte overflows).
func TestHostExecCapIsInclusive(t *testing.T) {
	_, _, _, view := newOwnView(t)
	req := helperRequest(t, "write-n", map[string]string{"AEC_HOST_N": "100"}, 10_000)
	req.MaxOutputBytes = 100
	res, err := runExec(t.Context(), t, view, req, sandboxhub.ExecOptions{})
	if err != nil {
		t.Fatal(err)
	}
	wantExit(t, res, 0)
	if res.Truncated || len(res.Stdout) != 100 {
		t.Errorf("an exact fit: truncated=%v len=%d", res.Truncated, len(res.Stdout))
	}

	req.MaxOutputBytes = 99
	res, err = runExec(t.Context(), t, view, req, sandboxhub.ExecOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Truncated || res.ExitCode != nil || len(res.Stdout) != 99 {
		t.Errorf("one byte over: truncated=%v exitCode=%v len=%d", res.Truncated, res.ExitCode, len(res.Stdout))
	}
}

// TestHostExecSplitCharactersReachOnOutputWhole: a character split across two pipe
// reads is delivered whole, not as U+FFFD (H8).
func TestHostExecSplitCharactersReachOnOutputWhole(t *testing.T) {
	_, _, _, view := newOwnView(t)
	var out outputRecorder
	res, err := runExec(t.Context(), t, view, helperRequest(t, "utf8-split", nil, 10_000), sandboxhub.ExecOptions{OnOutput: out.record})
	if err != nil {
		t.Fatal(err)
	}
	wantExit(t, res, 0)
	want := "a\U000020ac b\U0001f600 c\U000000e9\n"
	if res.Stdout != want {
		t.Errorf("stdout = %q, want %q", res.Stdout, want)
	}
	if got := out.joined(sandboxhub.StreamStdout); got != want {
		t.Errorf("OnOutput saw %q, want %q", got, want)
	}
	for _, piece := range out.pieces(sandboxhub.StreamStdout) {
		if strings.ContainsRune(piece, '\U0000fffd') {
			t.Errorf("a piece has a replacement character: %q", piece)
		}
	}
}

// outputRecorder is an OnOutput that is safe to call from both copy goroutines.
type outputRecorder struct {
	mu     sync.Mutex
	chunks []recordedChunk
}

type recordedChunk struct {
	stream sandboxhub.OutputStream
	data   string
}

func (o *outputRecorder) record(stream sandboxhub.OutputStream, data string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.chunks = append(o.chunks, recordedChunk{stream, data})
}

func (o *outputRecorder) pieces(stream sandboxhub.OutputStream) []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	var out []string
	for _, c := range o.chunks {
		if c.stream == stream {
			out = append(out, c.data)
		}
	}
	return out
}

func (o *outputRecorder) joined(stream sandboxhub.OutputStream) string {
	return strings.Join(o.pieces(stream), "")
}

// TestHostExecCancelledContextDoesNotSpawn: a context that is cancelled before the
// call gives Cancelled, no exit code and no error, and starts nothing.
func TestHostExecCancelledContextDoesNotSpawn(t *testing.T) {
	_, _, _, view := newOwnView(t)
	pidfile := filepath.Join(t.TempDir(), "started")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	res, err := runExec(ctx, t, view, helperRequest(t, "pidfile", map[string]string{"AEC_HOST_PIDFILE": pidfile}, 10_000), sandboxhub.ExecOptions{})
	if err != nil {
		t.Fatalf("a cancelled context is a result, not an error: %v", err)
	}
	if !res.Cancelled || res.ExitCode != nil || res.TimedOut || res.Truncated {
		t.Errorf("result = %+v, want only cancelled and no exit code", res)
	}
	// Give a process that was wrongly started the time to write its file.
	time.Sleep(500 * time.Millisecond)
	if _, err := os.Stat(pidfile); err == nil {
		t.Error("a process was started although the context was already cancelled")
	}
}

// TestHostExecCancellationDuringStartIsACancelledResult is the WR-06 regression: the
// context ends in the window between Exec's own check and cmd.Start (which checks it
// again and returns the context error). That is a Cancelled result, not an
// environment_unavailable error.
func TestHostExecCancellationDuringStartIsACancelledResult(t *testing.T) {
	_, _, _, view := newOwnView(t)
	pidfile := filepath.Join(t.TempDir(), "started")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	old := startCommand
	startCommand = func(cmd *exec.Cmd) (*tree, error) {
		cancel() // the caller gives up after the check, before the process exists
		return old(cmd)
	}
	t.Cleanup(func() { startCommand = old })

	res, err := runExec(ctx, t, view, helperRequest(t, "pidfile", map[string]string{"AEC_HOST_PIDFILE": pidfile}, 10_000), sandboxhub.ExecOptions{})
	if err != nil {
		t.Fatalf("a cancellation during start is a result, not an error: %v", err)
	}
	if !res.Cancelled || res.ExitCode != nil || res.TimedOut || res.Truncated {
		t.Errorf("result = %+v, want only cancelled and no exit code", res)
	}
	time.Sleep(300 * time.Millisecond)
	if _, err := os.Stat(pidfile); err == nil {
		t.Error("a process was started although the context ended before the start")
	}
}

func TestHostExecValidationAndOrder(t *testing.T) {
	_, _, _, view := newOwnView(t)
	ctx := t.Context()
	valid := sandboxhub.ExecRequest{Command: []string{"x"}, TimeoutMs: 1000, MaxOutputBytes: 1000}

	_, err := view.Exec(ctx, sandboxhub.ExecRequest{TimeoutMs: 1000, MaxOutputBytes: 1000}, sandboxhub.ExecOptions{})
	requireCode(t, err, kernel.SandboxCodeInvalidArgument)
	if err.Error() != "command must be a non-empty argv array" {
		t.Errorf("message = %q", err.Error())
	}
	for _, bad := range []sandboxhub.ExecRequest{
		{Command: []string{"x"}, TimeoutMs: 0, MaxOutputBytes: 1000},
		{Command: []string{"x"}, TimeoutMs: 1000, MaxOutputBytes: 0},
		{Command: []string{"x"}, TimeoutMs: -1, MaxOutputBytes: 1000},
	} {
		_, err = view.Exec(ctx, bad, sandboxhub.ExecOptions{})
		requireCode(t, err, kernel.SandboxCodeInvalidArgument)
		if err.Error() != "timeoutMs and maxOutputBytes must be positive" {
			t.Errorf("message = %q", err.Error())
		}
	}

	// The cwd checks come after the argument checks, with the fixed messages.
	req := valid
	req.Cwd = "no-such-folder"
	_, err = view.Exec(ctx, req, sandboxhub.ExecOptions{})
	requireCode(t, err, kernel.SandboxCodeNotFound)
	if want := `Working directory "no-such-folder" does not exist`; err.Error() != want {
		t.Errorf("message = %q, want %q", err.Error(), want)
	}
	if _, werr := view.WriteFile(ctx, "plain.txt", "x", sandboxhub.WriteOptions{}); werr != nil {
		t.Fatal(werr)
	}
	req.Cwd = "plain.txt"
	_, err = view.Exec(ctx, req, sandboxhub.ExecOptions{})
	requireCode(t, err, kernel.SandboxCodeInvalidPath)
	if want := `"plain.txt" is not a directory`; err.Error() != want {
		t.Errorf("message = %q, want %q", err.Error(), want)
	}
	req.Cwd = "../../outside"
	_, err = view.Exec(ctx, req, sandboxhub.ExecOptions{})
	requireCode(t, err, kernel.SandboxCodePathEscape)
}

// TestHostExecWorkingDirectoryMissingPrintsUndefined: with no cwd in the request and
// the working directory gone, the message prints "undefined".
func TestHostExecWorkingDirectoryMissingPrintsUndefined(t *testing.T) {
	root, _, _, view := newOwnView(t)
	if err := os.RemoveAll(filepath.Join(root, ownAgent)); err != nil {
		t.Fatal(err)
	}
	_, err := view.Exec(t.Context(), sandboxhub.ExecRequest{Command: []string{"x"}, TimeoutMs: 1000, MaxOutputBytes: 1000}, sandboxhub.ExecOptions{})
	requireCode(t, err, kernel.SandboxCodeNotFound)
	if want := `Working directory "undefined" does not exist`; err.Error() != want {
		t.Errorf("message = %q, want %q", err.Error(), want)
	}
}

func TestHostExecNeedsTheCapabilities(t *testing.T) {
	_, _, sb, view := newOwnView(t)
	ctx := t.Context()
	req := sandboxhub.ExecRequest{Command: []string{"x"}, Shell: boolPtr(true), TimeoutMs: 1000, MaxOutputBytes: 1000}

	sb.caps = []sandboxhub.Capability{sandboxhub.CapabilityFSRead, sandboxhub.CapabilityExec}
	_, err := view.Exec(ctx, req, sandboxhub.ExecOptions{})
	requireCode(t, err, kernel.SandboxCodeCapabilityUnsupported)
	if want := "This sandbox does not support shell commands"; err.Error() != want {
		t.Errorf("message = %q, want %q", err.Error(), want)
	}

	sb.caps = []sandboxhub.Capability{sandboxhub.CapabilityFSRead}
	_, err = view.Exec(ctx, req, sandboxhub.ExecOptions{})
	requireCode(t, err, kernel.SandboxCodeCapabilityUnsupported)
	if want := "This sandbox does not support exec"; err.Error() != want {
		t.Errorf("message = %q, want %q", err.Error(), want)
	}
}

// TestHostExecClampsToTheLimits: the request is capped by the sandbox limits, both
// the timeout and the output.
func TestHostExecClampsToTheLimits(t *testing.T) {
	_, _, _, view := newOwnView(t, func(o *Options) {
		timeout, output := 300, 50
		o.Limits = sandboxhub.LimitsPatch{MaxExecTimeoutMs: &timeout, MaxExecOutputBytes: &output}
	})
	started := time.Now()
	res, err := runExec(t.Context(), t, view, helperRequest(t, "sleep-forever", nil, 600_000), sandboxhub.ExecOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !res.TimedOut {
		t.Errorf("the 300 ms limit did not apply: %+v (after %v)", res, time.Since(started))
	}

	// The output cap on a sandbox of its own: under the 300 ms ceiling above, a slow
	// runner (Windows CI) times the helper out before it writes anything.
	_, _, _, view = newOwnView(t, func(o *Options) {
		output := 50
		o.Limits = sandboxhub.LimitsPatch{MaxExecOutputBytes: &output}
	})
	req := helperRequest(t, "write-n", map[string]string{"AEC_HOST_N": "200"}, 10_000)
	req.MaxOutputBytes = 1_000_000
	res, err = runExec(t.Context(), t, view, req, sandboxhub.ExecOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Truncated || len(res.Stdout) != 50 {
		t.Errorf("the 50 byte limit did not apply: truncated=%v len=%d", res.Truncated, len(res.Stdout))
	}
}

// TestHostExecCeilingZeroOrHugeIsNoOneMsTimeout pins that a ceiling of 0 or above
// 2^31-1 ms does not arm a 1 ms timer that times every command out at once: a
// ceiling of 0 is no cap and a huge ceiling is a real (long) timer.
func TestHostExecCeilingZeroOrHugeIsNoOneMsTimeout(t *testing.T) {
	for _, ceiling := range []int{0, -1, 3_000_000_000} {
		_, _, _, view := newOwnView(t, func(o *Options) {
			c := ceiling
			o.Limits = sandboxhub.LimitsPatch{MaxExecTimeoutMs: &c}
		})
		res, err := runExec(t.Context(), t, view, helperRequest(t, "write-n", map[string]string{"AEC_HOST_N": "10"}, 3_000_000_000), sandboxhub.ExecOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if res.TimedOut || res.ExitCode == nil || *res.ExitCode != 0 {
			t.Errorf("ceiling %d: result = %+v, want a normal exit", ceiling, res)
		}
	}
}

// TestHostExecOnlyOneCauseIsReported: a command that overflows its cap is truncated,
// not also timed out, even when the timeout fires while it is being killed.
func TestHostExecOnlyOneCauseIsReported(t *testing.T) {
	_, _, _, view := newOwnView(t)
	req := helperRequest(t, "flood", nil, 10_000)
	req.MaxOutputBytes = 1000
	res, err := runExec(t.Context(), t, view, req, sandboxhub.ExecOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Truncated || res.TimedOut || res.Cancelled || res.ExitCode != nil || len(res.Stdout) > 1000 {
		t.Errorf("result = truncated %v timedOut %v cancelled %v exit %v len %d", res.Truncated, res.TimedOut, res.Cancelled, res.ExitCode, len(res.Stdout))
	}
}
