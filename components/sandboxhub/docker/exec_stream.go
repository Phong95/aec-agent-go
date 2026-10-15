package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/internal/jsonx"
	"github.com/Phong95/aec-agent-go/internal/jsstr"
	"github.com/Phong95/aec-agent-go/internal/viewkit"
	"github.com/Phong95/aec-agent-go/kernel"
)

// ---------------------------------------------------------------------------
// The exec stream: one `docker exec` of the helper in exec mode.
//
// The host writes one JSON header line to the helper's stdin and keeps stdin open.
// The helper answers on stdout with one line, "AEC-OK" (the command is running; the
// rest of stdout is the command's stdout, stderr is the command's stderr) or
// "AEC-ERR <json>" (the command could not start). Stopping the command (timeout,
// cancel, output cap) CLOSES the helper's stdin, which kills the command's process
// group inside the container; killing the docker client alone would leave it running,
// so the client is only killed after stopGrace as a backstop.
// ---------------------------------------------------------------------------

const (
	// stopGrace is how long `docker exec` may keep running after we asked the helper
	// to stop the command.
	stopGrace = 5 * time.Second
	// maxHeadBytes is the longest the first stdout line may grow without a newline
	// before the helper's answer is called unexpected.
	maxHeadBytes = 65_536
	// readChunk is the size of one read of a stream.
	readChunk = 32 * 1024
)

// errContainerDown marks a container that stopped or vanished: it is recoverable by
// restarting it, so Exec recovers once and retries.
var errContainerDown = errors.New("the sandbox container is down")

// containerDownError carries the first lines of docker's own message and matches
// errContainerDown with errors.Is.
type containerDownError struct{ message string }

func (e *containerDownError) Error() string { return e.message }

// Is reports that every containerDownError is errContainerDown.
func (*containerDownError) Is(target error) bool { return target == errContainerDown }

// execHeader is the first stdin line of the helper's exec mode, in a fixed key order
// (the helper ignores the order; the field order keeps the header bytes stable).
type execHeader struct {
	Command        []string          `json:"command"`
	Shell          bool              `json:"shell"`
	Cwd            string            `json:"cwd"`
	Roots          []string          `json:"roots"`
	Env            map[string]string `json:"env"`
	Stdin          string            `json:"stdin"`
	TimeoutMs      int               `json:"timeoutMs"`
	MaxOutputBytes int               `json:"maxOutputBytes"`
}

// execState is everything one stream shares between its two readers, the timers and
// the cancellation callback. All of it sits under mu.
type execState struct {
	id        string
	proc      Process
	onOutput  func(stream sandboxhub.OutputStream, data string)
	maxOutput int

	mu sync.Mutex
	// kept are the raw bytes of each stream that count towards the output (stdout,
	// stderr); the text is decoded from them once, at the end.
	kept     [2][]byte
	decoders [2]viewkit.Decoder
	// early is stderr received before AEC-OK (docker's own message, or command output
	// that overtook stdout); it is replayed once the command is known to run.
	early     [][]byte
	head      []byte
	running   bool
	startErr  error
	written   int
	stopping  bool
	truncated bool
	timedOut  bool
	cancelled bool
	// headerDone is true once the header was written. A stop that arrives earlier waits
	// for it before closing stdin, so the helper always reads the header first.
	headerDone bool
	forceKill  *time.Timer
	killDone   chan struct{}
	// pending are the pieces taken but not yet handed to onOutput, in the order they
	// were taken. onOutput is never called while mu is held: a callback that blocks (a
	// slow client) must not keep the timeout, the cancellation and the output cap from
	// stopping the command. emitMu serialises the hand-over so the pieces still
	// arrive in the order they were taken.
	pending []pendingOutput
	emitMu  sync.Mutex
}

// pendingOutput is one piece of decoded command output waiting for onOutput.
type pendingOutput struct {
	stream int
	text   string
}

const (
	streamStdout = 0
	streamStderr = 1
)

func streamName(i int) sandboxhub.OutputStream {
	if i == streamStdout {
		return sandboxhub.StreamStdout
	}
	return sandboxhub.StreamStderr
}

// stopLocked stops the command: it closes the helper's stdin (the helper then kills
// the command's process group inside the container) and arms the backstop that kills
// the docker client after stopGrace. It does nothing the second time.
func (s *execState) stopLocked() {
	if s.stopping {
		return
	}
	s.stopping = true
	if s.headerDone {
		_ = s.proc.Stdin().Close()
	}
	done := make(chan struct{})
	s.killDone = done
	s.forceKill = time.AfterFunc(stopGrace, func() {
		defer close(done)
		_ = s.proc.Kill()
	})
}

func (s *execState) stop(mark func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if mark != nil {
		mark()
	}
	s.stopLocked()
}

// takeLocked adds a piece of command output, cut at the room left under the cap by
// bytes; a cut stops the command. Output after a stop is dropped. The
// decoded text is queued in pending; flushOutput hands it to onOutput.
func (s *execState) takeLocked(stream int, data []byte) {
	if s.stopping || len(data) == 0 {
		return
	}
	room := max(s.maxOutput-s.written, 0)
	piece := data
	if len(data) > room {
		piece = data[:room]
	}
	s.written += len(piece)
	if len(piece) > 0 {
		s.kept[stream] = append(s.kept[stream], piece...)
		if text := s.decoders[stream].Write(piece); text != "" && s.onOutput != nil {
			s.pending = append(s.pending, pendingOutput{stream, text})
		}
	}
	if len(piece) < len(data) {
		s.truncated = true
		s.stopLocked()
	}
}

// flushOutput hands the pending pieces to onOutput, outside mu and in the order they
// were taken (as the host collector calls OnOutput after releasing its mutex). Only
// one goroutine delivers at a time (emitMu), and it keeps going until nothing is
// pending, so a piece queued by the other reader meanwhile is delivered too.
func (s *execState) flushOutput() {
	if s.onOutput == nil {
		return
	}
	s.emitMu.Lock()
	defer s.emitMu.Unlock()
	for {
		s.mu.Lock()
		batch := s.pending
		s.pending = nil
		s.mu.Unlock()
		if len(batch) == 0 {
			return
		}
		for _, p := range batch {
			s.onOutput(streamName(p.stream), p.text)
		}
	}
}

// onStdout handles one read of the helper's stdout.
func (s *execState) onStdout(chunk []byte) {
	s.mu.Lock()
	s.handleStdoutLocked(chunk)
	s.mu.Unlock()
	s.flushOutput()
}

func (s *execState) handleStdoutLocked(chunk []byte) {
	if s.running {
		s.takeLocked(streamStdout, chunk)
		return
	}
	if s.startErr != nil {
		return
	}
	s.head = append(s.head, chunk...)
	nl := bytes.IndexByte(s.head, '\n')
	if nl == -1 {
		if len(s.head) > maxHeadBytes {
			s.startErr = kernel.NewSandboxError("Unexpected output from the sandbox helper", kernel.SandboxCodeEnvironmentUnavailable, s.id)
			s.stopLocked()
		}
		return
	}
	line := viewkit.LossyUTF8(s.head[:nl])
	rest := s.head[nl+1:]
	s.head = nil
	switch {
	case line == "AEC-OK":
		s.running = true
		for _, early := range s.early {
			s.takeLocked(streamStderr, early)
		}
		s.early = nil
		s.takeLocked(streamStdout, rest)
	case strings.HasPrefix(line, "AEC-ERR "):
		s.startErr = helperError(line[len("AEC-ERR "):], s.id)
		s.stopLocked()
	default:
		s.startErr = kernel.NewSandboxError(
			"Unexpected output from the sandbox helper: "+jsstr.TruncateUTF16(line, 200), kernel.SandboxCodeEnvironmentUnavailable, s.id)
		s.stopLocked()
	}
}

// onStderr handles one read of the helper's stderr.
func (s *execState) onStderr(chunk []byte) {
	s.mu.Lock()
	s.handleStderrLocked(chunk)
	s.mu.Unlock()
	s.flushOutput()
}

func (s *execState) handleStderrLocked(chunk []byte) {
	// Before "AEC-OK" stderr is docker's own (e.g. "No such container") - or command
	// output that overtook stdout; it is replayed once the command is known to run.
	if s.running {
		s.takeLocked(streamStderr, chunk)
		return
	}
	s.early = append(s.early, bytes.Clone(chunk))
}

// pump reads a stream to its end, one read at a time.
func pump(r io.Reader, handle func([]byte)) {
	buf := make([]byte, readChunk)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			handle(buf[:n])
		}
		if err != nil {
			return
		}
	}
}

// helperError reads the JSON of an AEC-ERR line: the helper's own code and message.
func helperError(text, sandboxID string) error {
	// The text is parsed as JSON and code and message are read from it; text that does
	// not parse, or parses to null, is unreadable.
	var parsed any
	if err := json.Unmarshal([]byte(text), &parsed); err != nil || parsed == nil {
		return kernel.NewSandboxError("The sandbox helper reported an unreadable error", kernel.SandboxCodeEnvironmentUnavailable, sandboxID)
	}
	object, _ := parsed.(map[string]any)
	code, _ := object["code"].(string)
	message, _ := object["message"].(string)
	return kernel.NewSandboxError(message, kernel.SandboxErrorCode(code), sandboxID)
}

// spawnExec starts the helper in exec mode. A start failure carries this sandbox's id.
func (s *Sandbox) spawnExec(ctx context.Context) (Process, error) {
	args := []string{"exec", "--interactive", "--user", s.p.settings.User, s.containerName, "node", helperPath, "exec"}
	proc, err := s.p.runner.Start(ctx, args)
	if err != nil {
		var se *kernel.SandboxError
		if errors.As(err, &se) && se.SandboxID == "" {
			return nil, kernel.NewSandboxError(se.Message, se.Code, s.id)
		}
		return nil, err
	}
	return proc, nil
}

// maxHelperTimeoutMs is the largest timeout the helper can honour: it arms
// setTimeout(kill, timeoutMs + 1000) (embed/helper.js), and a JS timer above 2^31-1 ms
// fires after 1 ms. About 24.8 days.
const maxHelperTimeoutMs = 2147483647 - 1000

// helperTimeoutMs caps an exec timeout at maxHelperTimeoutMs, so a request or ceiling
// above it runs for that long instead of being killed by the helper at once.
func helperTimeoutMs(ms int) int {
	return min(ms, maxHelperTimeoutMs)
}

// runExec is one `docker exec` of the helper. Timeout, cancel and the output cap close
// the helper's stdin, which kills the command's process group INSIDE the container
// (killing the docker client alone would leave it running). A container that is down
// before the helper answered is errContainerDown (carrying docker's first lines), so
// the caller can recover and retry.
func (v *view) runExec(ctx context.Context, h execHeader, opts sandboxhub.ExecOptions) (sandboxhub.ExecResult, error) {
	started := time.Now()
	var result sandboxhub.ExecResult
	if ctx.Err() != nil {
		result.Cancelled = true
		return result, nil //nolint:nilerr // a cancelled context is a Cancelled result, not an error (exec reports it in the result)
	}
	proc, err := v.sb.spawnExec(ctx)
	if err != nil {
		return sandboxhub.ExecResult{}, err
	}
	headerText, err := jsonx.Marshal(h)
	if err != nil {
		_ = proc.Stdin().Close()
		_ = proc.Kill()
		_, _ = proc.Wait()
		return sandboxhub.ExecResult{}, v.fail("Cannot encode the command: "+err.Error(), kernel.SandboxCodeInvalidArgument)
	}

	s := &execState{id: v.sb.id, proc: proc, onOutput: opts.OnOutput, maxOutput: h.MaxOutputBytes}

	// The timeout and the cancellation call stop; each callback is waited for below,
	// so none outlives this call.
	var callbacks sync.WaitGroup
	callbacks.Add(2)
	// The timer takes the real duration, even outside 1 to 2^31-1 ms (helperTimeoutMs
	// keeps the helper's own timer valid).
	timer := time.AfterFunc(viewkit.ExecTimeout(h.TimeoutMs), func() {
		defer callbacks.Done()
		s.stop(func() { s.timedOut = true })
	})
	unwatch := context.AfterFunc(ctx, func() {
		defer callbacks.Done()
		s.stop(func() { s.cancelled = true })
	})

	var readers, writer sync.WaitGroup
	readers.Add(2)
	go func() {
		defer readers.Done()
		pump(proc.Stdout(), s.onStdout)
	}()
	go func() {
		defer readers.Done()
		pump(proc.Stderr(), s.onStderr)
	}()
	writer.Add(1)
	go func() {
		defer writer.Done()
		// A write error is ignored: the client has gone, and the close handler reports
		// why.
		_, _ = proc.Stdin().Write(append(headerText, '\n'))
		s.mu.Lock()
		s.headerDone = true
		if s.stopping {
			_ = proc.Stdin().Close()
		}
		s.mu.Unlock()
	}()

	readers.Wait()
	code, waitErr := proc.Wait()
	// The client is gone: stop the timers and wait for a callback that already started.
	if timer.Stop() {
		callbacks.Done()
	}
	if unwatch() {
		callbacks.Done()
	}
	callbacks.Wait()
	s.mu.Lock()
	if s.forceKill != nil && !s.forceKill.Stop() {
		killDone := s.killDone
		s.mu.Unlock()
		<-killDone
		s.mu.Lock()
	}
	s.mu.Unlock()
	// A client that has exited has closed its end of stdin (cmd.Wait closes the pipe), so
	// a header write still in flight has failed by now; stdin is not closed here, because
	// only a stop closes it.
	writer.Wait()

	if waitErr != nil {
		return sandboxhub.ExecResult{}, kernel.NewSandboxError("Could not run the Docker CLI: "+waitErr.Error(), kernel.SandboxCodeEnvironmentUnavailable, v.sb.id)
	}

	res, err := v.settleExec(s, code, started, result)
	if err == nil {
		// The tails of the decoders were queued under mu; they are delivered after it.
		s.flushOutput()
	}
	return res, err
}

// settleExec turns the finished stream into the result, or the error that explains why
// the command never started. The decoder tails (a character cut off by the end of the
// stream) are queued for onOutput, which the caller delivers after mu is released.
func (v *view) settleExec(s *execState, code *int, started time.Time, result sandboxhub.ExecResult) (sandboxhub.ExecResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.startErr != nil {
		return sandboxhub.ExecResult{}, s.startErr
	}
	if !s.running && !s.cancelled && !s.timedOut {
		stderr := viewkit.LossyUTF8(bytes.Join(s.early, nil))
		if containerDown.MatchString(stderr) {
			return sandboxhub.ExecResult{}, &containerDownError{message: firstLines(stderr)}
		}
		detail := firstLines(stderr)
		if detail == "" {
			detail = "docker exited with " + codeText(code)
		}
		return sandboxhub.ExecResult{}, kernel.NewSandboxError("Could not start the command: "+detail, kernel.SandboxCodeEnvironmentUnavailable, v.sb.id)
	}
	for i := range s.decoders {
		if tail := s.decoders[i].End(); tail != "" && s.onOutput != nil {
			s.pending = append(s.pending, pendingOutput{i, tail})
		}
	}
	result.Stdout = viewkit.LossyUTF8(s.kept[streamStdout])
	result.Stderr = viewkit.LossyUTF8(s.kept[streamStderr])
	result.Truncated = s.truncated
	result.TimedOut = s.timedOut
	result.Cancelled = s.cancelled
	if !s.stopping {
		result.ExitCode = code
	}
	result.DurationMs = time.Since(started).Milliseconds()
	return result, nil
}

// validEnvName reports whether name matches /^[A-Za-z_][A-Za-z0-9_]*$/: a variable
// name the helper may pass on.
func validEnvName(name string) bool {
	if name == "" {
		return false
	}
	for i := range len(name) {
		c := name[i]
		letter := c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c == '_'
		digit := c >= '0' && c <= '9'
		if !letter && (!digit || i == 0) {
			return false
		}
	}
	return true
}

// containerNotRunning is the error of a container that is down twice in a row.
func (v *view) containerNotRunning(err error) error {
	return v.fail("The sandbox container is not running: "+err.Error(), kernel.SandboxCodeEnvironmentUnavailable)
}
