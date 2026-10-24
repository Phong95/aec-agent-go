package sandboxhub

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Phong95/aec-agent-go/internal/jsonx"
	"github.com/Phong95/aec-agent-go/kernel"
)

// defaultRunnerRequestTimeout is how long an operation without its own timeout may
// wait for the runner.
const defaultRunnerRequestTimeout = 120 * time.Second

// runnerExecGrace is the extra time an exec gets past its own timeout: the runner
// kills it and still answers.
const runnerExecGrace = 15 * time.Second

const maxRunnerFolderName = 255

// runnerDefaultLimits are the host sandbox's defaults (host.DefaultLimits), the
// ceiling of a runner: the server keeps the lower of these and the runner's own.
func runnerDefaultLimits() Limits {
	return Limits{
		MaxReadBytes:       2_000_000,
		MaxWriteBytes:      5_000_000,
		MaxListEntries:     5_000,
		MaxSearchMatches:   500,
		MaxExecTimeoutMs:   10 * 60_000,
		MaxExecOutputBytes: 2_000_000,
	}
}

var runnerErrorCodes = map[kernel.SandboxErrorCode]bool{
	kernel.SandboxCodeNotConfigured: true, kernel.SandboxCodeDisabled: true, kernel.SandboxCodeProviderUnavailable: true,
	kernel.SandboxCodeCapabilityUnsupported: true, kernel.SandboxCodePathEscape: true, kernel.SandboxCodeNotFound: true,
	kernel.SandboxCodeTooLarge: true, kernel.SandboxCodeConflict: true, kernel.SandboxCodeTimeout: true,
	kernel.SandboxCodeCancelled: true, kernel.SandboxCodePolicyDenied: true, kernel.SandboxCodeEnvironmentUnavailable: true,
	kernel.SandboxCodeInvalidPath: true, kernel.SandboxCodeInvalidArgument: true,
}

// ParseRunnerHello checks a runner's first message. It returns a *kernel.SandboxError
// (invalid_argument, or policy_denied for another protocol version) - the host then
// closes the connection.
func ParseRunnerHello(data []byte) (RunnerHello, error) {
	var m RunnerHello
	if err := json.Unmarshal(data, &m); err != nil || m.Type != RunnerMessageHello {
		return RunnerHello{}, kernel.NewSandboxError("The runner must start with a hello message", kernel.SandboxCodeInvalidArgument, "")
	}
	if m.Protocol != RunnerProtocolVersion {
		return RunnerHello{}, kernel.NewSandboxError(
			fmt.Sprintf("The runner speaks protocol %d; this server speaks %d", m.Protocol, RunnerProtocolVersion),
			kernel.SandboxCodePolicyDenied, "")
	}
	switch m.OS {
	case OSLinux, OSWindows, OSDarwin:
	default:
		return RunnerHello{}, kernel.NewSandboxError(fmt.Sprintf("Unknown runner OS %q", m.OS), kernel.SandboxCodeInvalidArgument, "")
	}
	name := strings.TrimSpace(m.FolderName)
	if name == "" {
		return RunnerHello{}, kernel.NewSandboxError("The runner must name its folder", kernel.SandboxCodeInvalidArgument, "")
	}
	m.FolderName = truncateRunes(name, maxRunnerFolderName)
	if m.Limits != nil {
		l := *m.Limits
		m.Limits = &l
	}
	return m, nil
}

func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// RunnerSandboxOptions configures NewRunnerSandbox.
type RunnerSandboxOptions struct {
	// ID is the stable id, e.g. "remote:runner:<sessionId>".
	ID string
	// Hello is the runner's hello, checked with ParseRunnerHello.
	Hello RunnerHello
	// Send sends a message to the runner over the host's connection.
	Send func(RunnerMessage) error
	// RequestTimeout is the ceiling for an operation without its own timeout. Zero
	// means 120 s.
	RequestTimeout time.Duration
}

// runnerReply is the answer to one request.
type runnerReply struct {
	result json.RawMessage
	err    error
}

type runnerPending struct {
	op       RunnerOperation
	reply    chan runnerReply
	onOutput func(OutputStream, string)
}

// RunnerSandbox is a folder on the user's own machine (A-153), reached through the
// runner of a client app (desktop, TUI). Every View operation becomes a request to
// the runner, which runs it confined to the chosen folder (ServeRunner on the client)
// and answers.
//
// Not isolated: the default approval policy asks before every write and command. The
// SandboxHub keeps one instance per (agent, session) the runner was attached to; when
// the connection drops it is closed and every call fails with environment_unavailable
// until the app reconnects - it never falls back to another sandbox. It is safe for
// concurrent use. Create it with NewRunnerSandbox.
type RunnerSandbox struct {
	id      string
	hello   RunnerHello
	traits  Traits
	limits  Limits
	caps    []Capability
	send    func(RunnerMessage) error
	timeout time.Duration

	mu             sync.Mutex
	pending        map[string]*runnerPending
	nextID         int64
	closedReason   string
	closed         bool
	closeListeners map[int]func()
	nextListener   int
}

var _ Sandbox = (*RunnerSandbox)(nil)

// NewRunnerSandbox returns the server side of one runner connection.
func NewRunnerSandbox(o RunnerSandboxOptions) *RunnerSandbox {
	timeout := o.RequestTimeout
	if timeout <= 0 {
		timeout = defaultRunnerRequestTimeout
	}
	// The lower of the runner's ceilings and the host defaults.
	limits := runnerDefaultLimits()
	if own := o.Hello.Limits; own != nil {
		limits.MaxReadBytes = lowerLimit(limits.MaxReadBytes, own.MaxReadBytes)
		limits.MaxWriteBytes = lowerLimit(limits.MaxWriteBytes, own.MaxWriteBytes)
		limits.MaxListEntries = lowerLimit(limits.MaxListEntries, own.MaxListEntries)
		limits.MaxSearchMatches = lowerLimit(limits.MaxSearchMatches, own.MaxSearchMatches)
		limits.MaxExecTimeoutMs = lowerLimit(limits.MaxExecTimeoutMs, own.MaxExecTimeoutMs)
		limits.MaxExecOutputBytes = lowerLimit(limits.MaxExecOutputBytes, own.MaxExecOutputBytes)
	}
	caps := []Capability{CapabilityFSRead, CapabilityFSWrite, CapabilityFSSearch}
	if o.Hello.AllowExec {
		caps = append(caps, CapabilityExec, CapabilityExecShell)
	}
	return &RunnerSandbox{
		id:    o.ID,
		hello: o.Hello,
		traits: Traits{
			Kind:     KindRemote,
			Isolated: false,
			// Commands run on the user's machine with its network.
			Network:    NetworkFull,
			Persistent: true,
			Tenancy:    TenancyOwn,
			OS:         o.Hello.OS,
		},
		limits:         limits,
		caps:           caps,
		send:           o.Send,
		timeout:        timeout,
		pending:        map[string]*runnerPending{},
		closeListeners: map[int]func(){},
	}
}

func lowerLimit(base, own int) int {
	if own > 0 && own < base {
		return own
	}
	return base
}

// ID returns the sandbox id.
func (s *RunnerSandbox) ID() string { return s.id }

// Traits returns the sandbox traits (remote, not isolated, own tenancy).
func (s *RunnerSandbox) Traits() Traits { return s.traits }

// Hello returns the runner's hello.
func (s *RunnerSandbox) Hello() RunnerHello { return s.hello }

// Connected is false once the connection dropped or the sandbox was disposed.
func (s *RunnerSandbox) Connected() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.closed
}

// Receive takes every message the runner sends after its hello. Malformed messages
// and messages for unknown requests are ignored.
func (s *RunnerSandbox) Receive(data []byte) {
	var m RunnerMessage
	if err := json.Unmarshal(data, &m); err != nil || m.ID == "" {
		return
	}
	s.mu.Lock()
	p := s.pending[m.ID]
	if p == nil {
		s.mu.Unlock()
		return
	}
	switch m.Type {
	case RunnerMessageOutput:
		s.mu.Unlock()
		if p.onOutput != nil && (m.Stream == StreamStdout || m.Stream == StreamStderr) {
			p.onOutput(m.Stream, m.Data)
		}
		return
	case RunnerMessageResponse:
		delete(s.pending, m.ID)
		s.mu.Unlock()
		p.reply <- runnerReply{result: m.Result}
	case RunnerMessageError:
		delete(s.pending, m.ID)
		s.mu.Unlock()
		code := m.Code
		if !runnerErrorCodes[code] {
			code = kernel.SandboxCodeEnvironmentUnavailable
		}
		msg := m.Message
		if msg == "" {
			msg = "The runner could not run the operation"
		}
		p.reply <- runnerReply{err: kernel.NewSandboxError(msg, code, s.id)}
	default:
		s.mu.Unlock()
	}
}

// Close marks the connection as gone (or the host closed it): every waiting call
// fails with environment_unavailable, and so does every later one. An empty reason
// means the app disconnected. Closing twice is a no-op.
func (s *RunnerSandbox) Close(reason string) {
	if reason == "" {
		reason = "The folder on the user's machine is offline (the app disconnected)"
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.closedReason = reason
	pending := s.pending
	s.pending = map[string]*runnerPending{}
	listeners := make([]func(), 0, len(s.closeListeners))
	for _, l := range s.closeListeners {
		listeners = append(listeners, l)
	}
	s.closeListeners = map[int]func(){}
	s.mu.Unlock()
	for _, p := range pending {
		p.reply <- runnerReply{err: kernel.NewSandboxError(reason, kernel.SandboxCodeEnvironmentUnavailable, s.id)}
	}
	for _, l := range listeners {
		l()
	}
}

// OnClose calls fn once when the sandbox closes (at once when it already is). It
// returns the function that removes fn.
func (s *RunnerSandbox) OnClose(fn func()) (unsubscribe func()) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		fn()
		return func() {}
	}
	id := s.nextListener
	s.nextListener++
	s.closeListeners[id] = fn
	s.mu.Unlock()
	return func() {
		s.mu.Lock()
		delete(s.closeListeners, id)
		s.mu.Unlock()
	}
}

// ForAgent returns the one view of the folder: the whole folder is /workspace.
func (s *RunnerSandbox) ForAgent(context.Context, string) (View, error) {
	return &runnerView{sb: s}, nil
}

// Health is ready while the runner is connected.
func (s *RunnerSandbox) Health(context.Context) Health {
	if s.Connected() {
		return HealthReady
	}
	return HealthUnavailable
}

// Dispose closes the sandbox (the runner's folder is never removed).
func (s *RunnerSandbox) Dispose(context.Context, bool) error {
	s.Close("The folder on the user's machine was detached from this session")
	return nil
}

// request sends one operation to the runner and waits for its answer. On ctx end it
// tells the runner to stop: a cancelled exec still answers (cancelled, with its
// output so far); every other operation ends here with "cancelled".
func (s *RunnerSandbox) request(ctx context.Context, op RunnerOperation, args []any, onOutput func(OutputStream, string), timeout time.Duration) (json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, kernel.NewSandboxError("The operation was cancelled", kernel.SandboxCodeCancelled, s.id)
	}
	raw := make([]json.RawMessage, len(args))
	for i, a := range args {
		b, err := jsonx.Marshal(a)
		if err != nil {
			return nil, kernel.NewSandboxError("Could not encode the request: "+err.Error(), kernel.SandboxCodeInvalidArgument, s.id)
		}
		raw[i] = b
	}
	s.mu.Lock()
	if s.closed {
		reason := s.closedReason
		s.mu.Unlock()
		return nil, kernel.NewSandboxError(reason, kernel.SandboxCodeEnvironmentUnavailable, s.id)
	}
	s.nextID++
	id := strconv.FormatInt(s.nextID, 10)
	p := &runnerPending{op: op, reply: make(chan runnerReply, 1), onOutput: onOutput}
	s.pending[id] = p
	s.mu.Unlock()

	if err := s.send(RunnerMessage{Type: RunnerMessageRequest, ID: id, Op: op, Args: raw}); err != nil {
		s.forget(id)
		return nil, kernel.NewSandboxError("Could not reach the runner: "+err.Error(), kernel.SandboxCodeEnvironmentUnavailable, s.id)
	}
	if timeout <= 0 {
		timeout = s.timeout
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	done := ctx.Done()
	for {
		select {
		case r := <-p.reply:
			return r.result, r.err
		case <-done:
			s.cancel(id)
			if op == RunnerExec {
				// Wait for the runner's answer (cancelled: true) until the timer.
				done = nil
				continue
			}
			s.forget(id)
			return nil, kernel.NewSandboxError("The operation was cancelled", kernel.SandboxCodeCancelled, s.id)
		case <-timer.C:
			s.cancel(id)
			s.forget(id)
			return nil, kernel.NewSandboxError(
				fmt.Sprintf("The runner did not answer within %d s", int(timeout.Round(time.Second)/time.Second)),
				kernel.SandboxCodeTimeout, s.id)
		}
	}
}

func (s *RunnerSandbox) forget(id string) {
	s.mu.Lock()
	delete(s.pending, id)
	s.mu.Unlock()
}

// cancel tells the runner to stop the request; a send failure is ignored (Close
// fails the waiting calls).
func (s *RunnerSandbox) cancel(id string) {
	_ = s.send(RunnerMessage{Type: RunnerMessageCancel, ID: id})
}

// runnerView is the one view of a runner's folder.
type runnerView struct{ sb *RunnerSandbox }

var _ View = (*runnerView)(nil)

func (v *runnerView) ID() string                 { return v.sb.id }
func (v *runnerView) Traits() Traits             { return v.sb.traits }
func (v *runnerView) Capabilities() []Capability { return append([]Capability(nil), v.sb.caps...) }
func (v *runnerView) Limits() Limits             { return v.sb.limits }
func (v *runnerView) WorkingDir() string         { return WorkspaceRoot }
func (v *runnerView) Roots() []string            { return []string{WorkspaceRoot} }

func (v *runnerView) hasExec() bool {
	for _, c := range v.sb.caps {
		if c == CapabilityExec {
			return true
		}
	}
	return false
}

func (v *runnerView) ReadFile(ctx context.Context, path string, opts ReadOptions) (FileContent, error) {
	var out FileContent
	err := v.call(ctx, RunnerReadFile, []any{path, opts}, &out)
	return out, err
}

func (v *runnerView) WriteFile(ctx context.Context, path, content string, opts WriteOptions) (WriteResult, error) {
	var out WriteResult
	err := v.call(ctx, RunnerWriteFile, []any{path, content, opts}, &out)
	return out, err
}

func (v *runnerView) Stat(ctx context.Context, path string) (*FileStat, error) {
	var out *FileStat
	if err := v.call(ctx, RunnerStat, []any{path}, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (v *runnerView) List(ctx context.Context, path string, opts ListOptions) (ListResult, error) {
	var out ListResult
	err := v.call(ctx, RunnerList, []any{path, opts}, &out)
	return out, err
}

func (v *runnerView) Search(ctx context.Context, q SearchQuery) (SearchResult, error) {
	var out SearchResult
	err := v.call(ctx, RunnerSearch, []any{q}, &out)
	return out, err
}

func (v *runnerView) Exec(ctx context.Context, req ExecRequest, opts ExecOptions) (ExecResult, error) {
	if !v.hasExec() {
		return ExecResult{}, kernel.NewSandboxError("Commands are turned off for this folder in the user's app", kernel.SandboxCodeCapabilityUnsupported, v.sb.id)
	}
	timeout := time.Duration(min(req.TimeoutMs, v.sb.limits.MaxExecTimeoutMs))*time.Millisecond + runnerExecGrace
	raw, err := v.sb.request(ctx, RunnerExec, []any{req}, opts.OnOutput, timeout)
	if err != nil {
		return ExecResult{}, err
	}
	var out ExecResult
	if err := json.Unmarshal(raw, &out); err != nil {
		return ExecResult{}, v.badResult(err)
	}
	return out, nil
}

// call runs one operation and decodes its result into out.
func (v *runnerView) call(ctx context.Context, op RunnerOperation, args []any, out any) error {
	raw, err := v.sb.request(ctx, op, args, nil, 0)
	if err != nil {
		return err
	}
	if len(raw) == 0 {
		raw = json.RawMessage("null")
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return v.badResult(err)
	}
	return nil
}

func (v *runnerView) badResult(err error) error {
	return kernel.NewSandboxError("The runner sent a malformed result: "+err.Error(), kernel.SandboxCodeEnvironmentUnavailable, v.sb.id)
}
