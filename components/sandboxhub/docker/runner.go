package docker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/Phong95/aec-agent-go/kernel"
)

// maxCaptureBytes: output above this is dropped - docker's own output, never a
// command's (that is streamed).
const maxCaptureBytes = 64 * 1024 * 1024

// cliWaitDelay bounds how long Run waits for the output pipes after the docker
// client was killed, so a client that left a descendant holding a pipe cannot keep
// a call waiting forever.
const cliWaitDelay = 5 * time.Second

// cliRunner is the default Runner: the docker CLI binary over os/exec.
type cliRunner struct {
	binary string
}

// capture collects the output of both streams of one call, up to maxCaptureBytes
// together (a chunk that starts below the cap is kept whole).
type capture struct {
	mu       sync.Mutex
	captured int
	out, err []byte
}

type captureWriter struct {
	c    *capture
	into func(c *capture) *[]byte
}

func (w captureWriter) Write(p []byte) (int, error) {
	w.c.mu.Lock()
	defer w.c.mu.Unlock()
	if w.c.captured < maxCaptureBytes {
		w.c.captured += len(p)
		buf := w.into(w.c)
		*buf = append(*buf, p...)
	}
	return len(p), nil
}

// Run implements Runner.
func (r cliRunner) Run(ctx context.Context, args []string, opts RunOptions) (RunResult, error) {
	if ctx.Err() != nil {
		return RunResult{}, kernel.NewSandboxError("Operation cancelled", kernel.SandboxCodeCancelled, opts.SandboxID)
	}
	runCtx := ctx
	if opts.Timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(runCtx, r.binary, args...) //nolint:gosec // the binary is host configuration and the arguments are built by the provider, never profile data
	hideWindow(cmd)
	cmd.WaitDelay = cliWaitDelay
	cmd.Stdin = strings.NewReader(opts.Input)
	var c capture
	cmd.Stdout = captureWriter{c: &c, into: func(c *capture) *[]byte { return &c.out }}
	cmd.Stderr = captureWriter{c: &c, into: func(c *capture) *[]byte { return &c.err }}

	err := cmd.Run()
	// The caller's cancellation wins over the timeout; both end the call without a
	// result.
	if err != nil && ctx.Err() != nil {
		return RunResult{}, kernel.NewSandboxError("Operation cancelled", kernel.SandboxCodeCancelled, opts.SandboxID)
	}
	if err != nil && errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		return RunResult{}, kernel.NewSandboxError(
			fmt.Sprintf("docker %s timed out after %d ms", firstArg(args), opts.Timeout.Milliseconds()),
			kernel.SandboxCodeTimeout, opts.SandboxID)
	}
	var exit *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exit):
	default:
		return RunResult{}, spawnError(err, r.binary, opts.SandboxID)
	}
	return RunResult{Code: exitCode(cmd), Stdout: string(c.out), Stderr: string(c.err)}, nil
}

// Start implements Runner. The client lives until it exits or Kill is called: the
// ctx is used for the start only, because stopping a command is a sequence (close the
// helper's stdin, then kill the client after a grace period) driven by the caller.
func (r cliRunner) Start(ctx context.Context, args []string) (Process, error) {
	cmd := exec.CommandContext(context.WithoutCancel(ctx), r.binary, args...) //nolint:gosec // the binary is host configuration and the arguments are built by the provider, never profile data
	hideWindow(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, spawnError(err, r.binary, "")
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, spawnError(err, r.binary, "")
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, spawnError(err, r.binary, "")
	}
	if err := cmd.Start(); err != nil {
		return nil, spawnError(err, r.binary, "")
	}
	return &cliProcess{cmd: cmd, stdin: stdin, stdout: stdout, stderr: stderr}, nil
}

type cliProcess struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.Reader
	stderr io.Reader
}

func (p *cliProcess) Stdin() io.WriteCloser { return p.stdin }
func (p *cliProcess) Stdout() io.Reader     { return p.stdout }
func (p *cliProcess) Stderr() io.Reader     { return p.stderr }

func (p *cliProcess) Wait() (*int, error) {
	err := p.cmd.Wait()
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		return nil, err
	}
	return exitCode(p.cmd), nil
}

func (p *cliProcess) Kill() error {
	if p.cmd.Process == nil {
		return nil
	}
	return p.cmd.Process.Kill()
}

// exitCode is the exit code of a finished command, nil when a signal ended it (Go
// reports -1 then).
func exitCode(cmd *exec.Cmd) *int {
	if cmd.ProcessState == nil {
		return nil
	}
	code := cmd.ProcessState.ExitCode()
	if code < 0 {
		return nil
	}
	return &code
}

// spawnError maps a failure to start the docker client to a SandboxError: a
// binary that cannot be found is environment_unavailable with the install hint, any
// other start failure is environment_unavailable too.
func spawnError(err error, binary, sandboxID string) *kernel.SandboxError {
	message := "Could not run the Docker CLI: " + err.Error()
	if errors.Is(err, exec.ErrNotFound) || errors.Is(err, exec.ErrDot) || errors.Is(err, fs.ErrNotExist) {
		message = fmt.Sprintf("Docker CLI \"%s\" was not found \U00002014 install Docker to use sandboxes", binary)
	}
	return kernel.NewSandboxError(message, kernel.SandboxCodeEnvironmentUnavailable, sandboxID)
}

func firstArg(args []string) string {
	if len(args) == 0 {
		return "undefined"
	}
	return args[0]
}
