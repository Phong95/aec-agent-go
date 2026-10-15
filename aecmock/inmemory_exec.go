package aecmock

import (
	"context"
	"errors"
	"fmt"
	"path"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/internal/viewkit"
	"github.com/Phong95/aec-agent-go/kernel"
)

// -- Commands --

// yesChunkUnits is the size, in UTF-16 units, of the chunk the yes command writes
// at a time.
const yesChunkUnits = 8192

// execRun is the state of one Exec call: the result being built, the kill channel
// and the flags. The command runs in the calling goroutine; only the timeout timer
// and the context callback touch the run from other goroutines, and they do it
// through kill, under mu.
type execRun struct {
	mu        sync.Mutex
	result    sandboxhub.ExecResult
	killed    bool
	written   int
	maxOutput int
	onOutput  func(sandboxhub.OutputStream, string)
	// killCh is closed exactly once, when the process is killed.
	killCh chan struct{}
}

// kill marks the process killed and records why. The first reason wins: a timer, the
// context and the output cap each set exactly one flag, and once the process is
// killed the others change nothing.
func (r *execRun) kill(setReason func(*sandboxhub.ExecResult)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.killed {
		return
	}
	r.killed = true
	setReason(&r.result)
	close(r.killCh)
}

// out is the output sink of the process. After a kill it drops everything. A piece
// that does not fit in the remaining room (output cap, stdout and stderr together)
// is cut by bytes, which sets truncated and kills the process.
func (r *execRun) out(stream sandboxhub.OutputStream, data string) {
	r.mu.Lock()
	if r.killed {
		r.mu.Unlock()
		return
	}
	room := r.maxOutput - r.written
	piece := data
	cut := len(data) > room
	if cut {
		piece = viewkit.SliceBytes(data, room)
	}
	r.written += len(piece)
	if stream == sandboxhub.StreamStdout {
		r.result.Stdout += piece
	} else {
		r.result.Stderr += piece
	}
	r.mu.Unlock()

	if piece != "" && r.onOutput != nil {
		r.onOutput(stream, piece)
	}
	if cut {
		r.kill(func(res *sandboxhub.ExecResult) { res.Truncated = true })
	}
}

// Exec runs one command. Timeouts, output overflow and cancellation are reported in
// the result (the process is killed and ExitCode is nil) rather than returned as an
// error; an error means the command could not be started (disposed sandbox,
// capability, bad request, bad cwd).
//
// A cancelled context is not an error either: an already-cancelled context starts
// nothing and returns a result with Cancelled set. Only variables passed in the request exist for the command.
//
// The clock is read twice, at the start and at the end, so DurationMs is the
// difference of two injected ticks.
func (v *inMemoryView) Exec(ctx context.Context, req sandboxhub.ExecRequest, opts sandboxhub.ExecOptions) (sandboxhub.ExecResult, error) { //nolint:contextcheck // a nil ctx is replaced by Background, as the other view methods tolerate it
	if ctx == nil {
		ctx = context.Background()
	}
	cwd, err := v.validateExec(req)
	if err != nil {
		return sandboxhub.ExecResult{}, err
	}

	timeoutMs := min(req.TimeoutMs, v.sb.limits.MaxExecTimeoutMs)
	maxOutput := min(req.MaxOutputBytes, v.sb.limits.MaxExecOutputBytes)
	started := v.sb.tick()
	run := &execRun{maxOutput: maxOutput, onOutput: opts.OnOutput, killCh: make(chan struct{})}

	timerDone := make(chan struct{})
	timer := time.AfterFunc(viewkit.TimeoutDelay(float64(timeoutMs)), func() {
		defer close(timerDone)
		run.kill(func(res *sandboxhub.ExecResult) { res.TimedOut = true })
	})
	ctxDone := make(chan struct{})
	var stopCtx func() bool
	if ctx.Err() != nil {
		run.kill(func(res *sandboxhub.ExecResult) { res.Cancelled = true })
	} else {
		stopCtx = context.AfterFunc(ctx, func() {
			defer close(ctxDone)
			run.kill(func(res *sandboxhub.ExecResult) { res.Cancelled = true })
		})
	}
	// cleanup stops the timer and the context registration and, when one of them
	// already fired, waits for its callback to finish, so no goroutine, timer or
	// registration outlives the call.
	var cleanupOnce sync.Once
	cleanup := func() {
		cleanupOnce.Do(func() {
			if !timer.Stop() {
				<-timerDone
			}
			if stopCtx != nil && !stopCtx() {
				<-ctxDone
			}
		})
	}
	defer cleanup()

	if !run.isKilled() {
		code := v.runCommand(ctx, req, cwd, run)
		run.mu.Lock()
		if !run.killed {
			run.result.ExitCode = &code
		}
		run.mu.Unlock()
	}
	cleanup()

	run.mu.Lock()
	result := run.result
	run.mu.Unlock()
	result.DurationMs = v.sb.tick() - started
	return result, nil
}

func (r *execRun) isKilled() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.killed
}

// validateExec checks the request in this order (disposed, exec capability,
// exec.shell when Shell, a non-empty argv, positive timeout and output cap, the
// working directory) and returns the real working directory.
func (v *inMemoryView) validateExec(req sandboxhub.ExecRequest) (string, error) {
	v.sb.mu.Lock()
	defer v.sb.mu.Unlock()
	if v.sb.disposed {
		return "", v.fail(fmt.Sprintf("Sandbox \"%s\" was disposed", v.sb.id), kernel.SandboxCodeEnvironmentUnavailable)
	}
	if !v.sb.hasCapability(sandboxhub.CapabilityExec) {
		return "", v.fail("This sandbox does not support exec", kernel.SandboxCodeCapabilityUnsupported)
	}
	if req.Shell != nil && *req.Shell && !v.sb.hasCapability(sandboxhub.CapabilityExecShell) {
		return "", v.fail("This sandbox does not support shell commands", kernel.SandboxCodeCapabilityUnsupported)
	}
	if len(req.Command) == 0 {
		return "", v.fail("command must be a non-empty argv array", kernel.SandboxCodeInvalidArgument)
	}
	if req.TimeoutMs <= 0 || req.MaxOutputBytes <= 0 {
		return "", v.fail("timeoutMs and maxOutputBytes must be positive", kernel.SandboxCodeInvalidArgument)
	}
	requested := req.Cwd
	if requested == "" {
		requested = v.workingDir
	}
	cwd, err := v.resolve(requested)
	if err != nil {
		return "", err
	}
	// The messages print the requested cwd, shown as undefined when it was left out.
	shown := req.Cwd
	if shown == "" {
		shown = "undefined"
	}
	node, ok := v.sb.store.get(cwd)
	if !ok {
		return "", v.fail(fmt.Sprintf("Working directory \"%s\" does not exist", shown), kernel.SandboxCodeNotFound)
	}
	if node.kind != sandboxhub.EntryDirectory {
		return "", v.fail(fmt.Sprintf("\"%s\" is not a directory", shown), kernel.SandboxCodeInvalidPath)
	}
	return cwd, nil
}

// errorCode is the code of a SandboxError, or "error" for anything else, as the cat
// and rm error lines print it.
func errorCode(err error) string {
	var se *kernel.SandboxError
	if errors.As(err, &se) && se != nil {
		return string(se.Code)
	}
	return "error"
}

// runCommand is the POSIX mini-exec: echo, pwd, true, false, printenv, cat, rm,
// sleep and yes; anything else is "command not found" with exit code 127. It runs
// in the calling goroutine and holds no lock, so cat and rm can use the view.
func (v *inMemoryView) runCommand(ctx context.Context, req sandboxhub.ExecRequest, cwd string, run *execRun) int {
	program, args := req.Command[0], req.Command[1:]
	switch program {
	case "echo":
		run.out(sandboxhub.StreamStdout, strings.Join(args, " ")+"\n")
		return 0
	case "pwd":
		run.out(sandboxhub.StreamStdout, cwd+"\n")
		return 0
	case "true":
		return 0
	case "false":
		return 1
	case "printenv":
		// Only variables passed in the request exist - nothing leaks from the server process.
		if len(args) == 0 {
			return 1
		}
		value, ok := req.Env[args[0]]
		if !ok {
			return 1
		}
		run.out(sandboxhub.StreamStdout, value+"\n")
		return 0
	case "cat":
		return v.cat(ctx, args, cwd, run)
	case "rm":
		return v.rm(args, cwd, run)
	case "sleep":
		operand := "0"
		if len(args) > 0 {
			operand = args[0]
		}
		// The operand goes through JSNumber and TimeoutDelay, so "abc", "-1"
		// and "1e10" wait about 1 ms and exit 0.
		timer := time.NewTimer(viewkit.TimeoutDelay(viewkit.JSNumber(operand) * 1000))
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-run.killCh:
		}
		return 0
	case "yes":
		line := strings.Join(args, " ")
		if line == "" {
			line = "y"
		}
		line += "\n"
		chunk := strings.Repeat(line, max(1, yesChunkUnits/kernel.UTF16Len(line)))
		for !run.isKilled() {
			run.out(sandboxhub.StreamStdout, chunk)
			runtime.Gosched()
		}
		return 0
	default:
		run.out(sandboxhub.StreamStderr, program+": command not found\n")
		return 127
	}
}

// cat prints each file through the view's ReadFile, so every view rule applies
// (capability, roots, the read limit). A file that cannot be read is one line on
// stderr, "cat: <file>: <code>", and exit code 1.
func (v *inMemoryView) cat(ctx context.Context, files []string, cwd string, run *execRun) int {
	code := 0
	for _, file := range files {
		target := file
		if !path.IsAbs(file) {
			target = path.Join(cwd, file)
		}
		// The read passes no cancellation: reading a file is not cancellable.
		fc, err := v.ReadFile(context.WithoutCancel(ctx), target, sandboxhub.ReadOptions{})
		if err != nil {
			run.out(sandboxhub.StreamStderr, fmt.Sprintf("cat: %s: %s\n", file, errorCode(err)))
			code = 1
			continue
		}
		run.out(sandboxhub.StreamStdout, fc.Content)
	}
	return code
}

// rm is a minimal "rm [-r] [-f] [--] path...", enough for tools that clear a folder
// they own.
func (v *inMemoryView) rm(args []string, cwd string, run *execRun) int {
	flags := map[rune]bool{}
	var paths []string
	endOfFlags := false
	for _, arg := range args {
		switch {
		case !endOfFlags && arg == "--":
			endOfFlags = true
		case !endOfFlags && strings.HasPrefix(arg, "-"):
			for _, ch := range arg[1:] {
				flags[ch] = true
			}
		default:
			paths = append(paths, arg)
		}
	}
	recursive := flags['r'] || flags['R']
	force := flags['f']
	code := 0
	for _, p := range paths {
		target := p
		if !path.IsAbs(p) {
			target = path.Join(cwd, p)
		}
		if msg := v.rmOne(p, target, recursive, force); msg != "" {
			run.out(sandboxhub.StreamStderr, msg)
			code = 1
		}
	}
	return code
}

// rmOne removes one path and returns the stderr line when it failed ("" when it
// did not, including a missing path with -f).
func (v *inMemoryView) rmOne(shown, target string, recursive, force bool) string {
	v.sb.mu.Lock()
	defer v.sb.mu.Unlock()
	resolved, err := v.resolve(target)
	if err != nil {
		return fmt.Sprintf("rm: %s: %s\n", shown, errorCode(err))
	}
	node, ok := v.sb.store.get(resolved)
	if !ok {
		if !force {
			return fmt.Sprintf("rm: %s: No such file or directory\n", shown)
		}
		return ""
	}
	if node.kind == sandboxhub.EntryDirectory && !recursive {
		return fmt.Sprintf("rm: %s: is a directory\n", shown)
	}
	v.sb.store.deleteWhere(func(key string) bool { return key == resolved || strings.HasPrefix(key, resolved+"/") })
	return ""
}
