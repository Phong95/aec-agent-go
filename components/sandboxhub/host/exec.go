package host

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/internal/viewkit"
	"github.com/Phong95/aec-agent-go/kernel"
)

// execWaitDelay bounds how long Exec waits for the output pipes after the command
// itself is over or was killed (os/exec Cmd.WaitDelay): a descendant that escaped
// the kill and still holds a pipe cannot keep Exec waiting forever.
const execWaitDelay = 2 * time.Second

// exitCommandNotFound is the exit code of an unknown program, as in a shell.
const exitCommandNotFound = 127

// killCause is why a command was killed.
type killCause int

const (
	causeNone killCause = iota
	causeTimedOut
	causeCancelled
	causeTruncated
)

// startCommand starts cmd in its own tree. It is a variable so that a test can end
// the caller's context exactly between Exec's cancellation check and the start.
var startCommand = startInTree

// runState is the kill bookkeeping of one Exec (the killed flag and stop()).
// The first of the timeout timer, the caller's context and the output cap sets the
// cause and kills the tree; later ones do nothing, so exactly one of timedOut,
// cancelled and truncated is ever reported.
type runState struct {
	col *collector

	mu    sync.Mutex
	cause killCause
	tree  *tree
	done  bool
}

// stop records cause (when it is the first) and kills the whole tree DIRECTLY, not
// through cmd.Cancel: os/exec only calls Cancel while the command's own process is
// still running, and a group or job whose direct child already exited can still
// hold descendants that must die.
func (r *runState) stop(cause killCause) {
	r.mu.Lock()
	if r.cause != causeNone || r.done {
		r.mu.Unlock()
		return
	}
	r.cause = cause
	t := r.tree
	r.mu.Unlock()
	r.col.halt()
	if t != nil {
		_ = t.kill()
	}
}

// setTree hands the tree to the run state once the command has started. A stop that
// raced the start (the caller's context ended while the process was being created)
// kills it now.
func (r *runState) setTree(t *tree) {
	r.mu.Lock()
	r.tree = t
	pending := r.cause != causeNone
	r.mu.Unlock()
	if pending {
		_ = t.kill()
	}
}

// finish closes the state to later stops and returns the cause.
func (r *runState) finish() killCause {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.done = true
	return r.cause
}

// Exec runs a command in the agent's folder.
//
// The checks run in this order: the sandbox is not disposed, it has the exec
// capability (and exec.shell for a shell command), the argv is not empty, the
// timeout and the output cap are positive, the working directory resolves inside
// the roots and is an existing directory. A command that was started always comes
// back as an ExecResult: a timeout, an output overflow and a cancellation are
// reported in it (the process tree is killed and ExitCode is nil), not as an error.
// An error means the command could not be started.
//
// A context that is already cancelled gives Cancelled with no exit code and no
// process: nothing is started only to be killed at once.
func (v *view) Exec(ctx context.Context, req sandboxhub.ExecRequest, opts sandboxhub.ExecOptions) (sandboxhub.ExecResult, error) { //nolint:contextcheck // a nil ctx is tolerated, like the file operations do
	if ctx == nil {
		ctx = context.Background()
	}
	if v.sb.isDisposed() {
		return sandboxhub.ExecResult{}, v.sb.disposedError()
	}
	if !slices.Contains(v.sb.caps, sandboxhub.CapabilityExec) {
		return sandboxhub.ExecResult{}, v.fail("This sandbox does not support exec", kernel.SandboxCodeCapabilityUnsupported)
	}
	shell := req.Shell != nil && *req.Shell
	if shell && !slices.Contains(v.sb.caps, sandboxhub.CapabilityExecShell) {
		return sandboxhub.ExecResult{}, v.fail("This sandbox does not support shell commands", kernel.SandboxCodeCapabilityUnsupported)
	}
	if len(req.Command) == 0 {
		return sandboxhub.ExecResult{}, v.fail("command must be a non-empty argv array", kernel.SandboxCodeInvalidArgument)
	}
	if req.TimeoutMs <= 0 || req.MaxOutputBytes <= 0 {
		return sandboxhub.ExecResult{}, v.fail("timeoutMs and maxOutputBytes must be positive", kernel.SandboxCodeInvalidArgument)
	}
	cwdInput := req.Cwd
	if cwdInput == "" {
		cwdInput = v.workingDir
	}
	res, err := v.resolve(cwdInput)
	if err != nil {
		return sandboxhub.ExecResult{}, err
	}
	if err := v.checkWorkingDirectory(res, req.Cwd); err != nil {
		return sandboxhub.ExecResult{}, err
	}
	if ctx.Err() != nil {
		return sandboxhub.ExecResult{Cancelled: true}, nil
	}

	// A ceiling of 0 or below is no cap (quirk Q7, fixed in Go).
	timeoutMs := viewkit.ExecTimeoutMs(req.TimeoutMs, v.sb.limits.MaxExecTimeoutMs)
	maxOutput := min(req.MaxOutputBytes, v.sb.limits.MaxExecOutputBytes)
	started := time.Now()
	program := req.Command[0]

	// The command runs on a context derived from the caller's, so a cancelled
	// caller also reaches cmd.Cancel; the kill itself is always stop().
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	var cmd *exec.Cmd
	if shell {
		cmd = shellCommand(runCtx, req.Command)
	} else {
		cmd = exec.CommandContext(runCtx, program, req.Command[1:]...) //nolint:gosec // G204: running the model's program is the point of exec; the approval policy gates it
	}
	cmd.Dir = res.realPath
	cmd.Env = buildEnv(req.Env)
	cmd.Stdin = strings.NewReader(req.Stdin)

	state := &runState{}
	state.col = newCollector(maxOutput, opts.OnOutput, func() { state.stop(causeTruncated) })
	cmd.Stdout, cmd.Stderr = state.col.stdout(), state.col.stderr()
	cmd.Cancel = func() error {
		state.stop(causeCancelled)
		return nil
	}
	cmd.WaitDelay = execWaitDelay

	tr, err := startCommand(cmd)
	if err != nil {
		if ctx.Err() != nil && errors.Is(err, ctx.Err()) {
			// The caller's context ended between the check above and the start (cmd.Start
			// checks it again): a cancellation is a Cancelled result, not an error.
			return sandboxhub.ExecResult{Cancelled: true, DurationMs: time.Since(started).Milliseconds()}, nil
		}
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist) {
			// Like a shell: an unknown program is exit 127, not an exception.
			code := exitCommandNotFound
			return sandboxhub.ExecResult{
				ExitCode: &code, Stderr: program + ": command not found\n", DurationMs: time.Since(started).Milliseconds(),
			}, nil
		}
		return sandboxhub.ExecResult{}, v.fail(fmt.Sprintf("Could not start \"%s\": %v", program, err), kernel.SandboxCodeEnvironmentUnavailable)
	}
	state.setTree(tr)
	// A timeout outside 1 to 2^31-1 ms (only possible through a ceiling set that way)
	// is not cut to 1 ms: the timer takes the real duration.
	timer := time.AfterFunc(viewkit.ExecTimeout(timeoutMs), func() { state.stop(causeTimedOut) })
	unwatch := context.AfterFunc(ctx, func() { state.stop(causeCancelled) })

	waitErr := cmd.Wait()
	timer.Stop()
	unwatch()
	cause := state.finish()
	if errors.Is(waitErr, exec.ErrWaitDelay) {
		// The command is over but a descendant still held the pipes: end the tree.
		_ = tr.kill()
	}
	tr.release()
	state.col.finish()

	stdout, stderr := state.col.result()
	result := sandboxhub.ExecResult{
		Stdout:     stdout,
		Stderr:     stderr,
		Truncated:  cause == causeTruncated,
		TimedOut:   cause == causeTimedOut,
		Cancelled:  cause == causeCancelled,
		DurationMs: time.Since(started).Milliseconds(),
	}
	// The exit code stays nil once the command was killed, and when the system
	// reports none (a process ended by a signal).
	if cause == causeNone && cmd.ProcessState != nil {
		if code := cmd.ProcessState.ExitCode(); code >= 0 {
			result.ExitCode = &code
		}
	}
	return result, nil
}

// checkWorkingDirectory fails when the resolved working directory does not exist or
// is not a directory, with the fixed messages. input is the cwd the caller supplied;
// an absent one prints as undefined.
func (v *view) checkWorkingDirectory(res resolvedPath, input string) error {
	label := input
	if label == "" {
		label = "undefined"
	}
	err := inRoot(res.realRoot, res.realPath, func(root *os.Root, rel string) error {
		st, err := root.Stat(rel)
		if err != nil {
			return v.fail(fmt.Sprintf("Working directory \"%s\" does not exist", label), kernel.SandboxCodeNotFound)
		}
		if !st.IsDir() {
			return v.fail(fmt.Sprintf("\"%s\" is not a directory", label), kernel.SandboxCodeInvalidPath)
		}
		return nil
	})
	if err == nil {
		return nil
	}
	var se *kernel.SandboxError
	if errors.As(err, &se) {
		return err
	}
	return v.fail(fmt.Sprintf("Working directory \"%s\" does not exist", label), kernel.SandboxCodeNotFound)
}
