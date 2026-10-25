package sandboxhub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
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

// runnerNoTimeout (A-160) is a request timeout of none: an upload takes as long as its
// bytes; ctx (and the connection) end it.
const runnerNoTimeout time.Duration = -1

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

// parseRunnerFeatures (A-160) keeps the known features of a hello, each once (unknown
// ones are dropped).
func parseRunnerFeatures(features []RunnerFeature) []RunnerFeature {
	var out []RunnerFeature
	for _, f := range features {
		if (f == RunnerFeatureStream || f == RunnerFeatureServices) && !slices.Contains(out, f) {
			out = append(out, f)
		}
	}
	return out
}

// runnerError is an "error" message from the runner as a *kernel.SandboxError.
func runnerError(m RunnerMessage, sandboxID string) error {
	code := m.Code
	if !runnerErrorCodes[code] {
		code = kernel.SandboxCodeEnvironmentUnavailable
	}
	msg := m.Message
	if msg == "" {
		msg = "The runner could not run the operation"
	}
	return kernel.NewSandboxError(msg, code, sandboxID)
}

// ParseRunnerHello checks a runner's first message. It returns a *kernel.SandboxError
// (invalid_argument, or policy_denied for another protocol version) - the host then
// closes the connection.
func ParseRunnerHello(data []byte) (RunnerHello, error) {
	var m RunnerHello
	if err := json.Unmarshal(data, &m); err != nil || m.Type != RunnerMessageHello {
		return RunnerHello{}, kernel.NewSandboxError("The runner must start with a hello message", kernel.SandboxCodeInvalidArgument, "")
	}
	// A-159: protocol 1 runners (one folder at /workspace) are still accepted.
	if !slices.Contains(RunnerProtocolVersions, m.Protocol) {
		versions := make([]string, len(RunnerProtocolVersions))
		for i, v := range RunnerProtocolVersions {
			versions[i] = strconv.Itoa(v)
		}
		return RunnerHello{}, kernel.NewSandboxError(
			fmt.Sprintf("The runner speaks protocol %d; this server speaks %s", m.Protocol, strings.Join(versions, " and ")),
			kernel.SandboxCodePolicyDenied, "")
	}
	switch m.OS {
	case OSLinux, OSWindows, OSDarwin:
	default:
		return RunnerHello{}, kernel.NewSandboxError(fmt.Sprintf("Unknown runner OS %q", m.OS), kernel.SandboxCodeInvalidArgument, "")
	}
	if m.Limits != nil {
		l := *m.Limits
		m.Limits = &l
	}
	m.Features = parseRunnerFeatures(m.Features)
	if m.Protocol >= 2 {
		folders, err := parseRunnerFolders(m.Folders, m.Folders != nil)
		if err != nil {
			return RunnerHello{}, err
		}
		m.Folders = folders
		m.FolderName = folders[0].Name
		m.AllowExec = false
		for _, f := range folders {
			m.AllowExec = m.AllowExec || f.AllowExec
		}
		return m, nil
	}
	m.Folders = nil
	name := strings.TrimSpace(m.FolderName)
	if name == "" {
		return RunnerHello{}, kernel.NewSandboxError("The runner must name its folder", kernel.SandboxCodeInvalidArgument, "")
	}
	m.FolderName = truncateRunes(name, maxRunnerFolderName)
	return m, nil
}

// ParseRunnerFolders (A-159) checks a runner's folder list (hello or "folders" message):
// 1 to RunnerMaxFolders folders whose names are path segments as RunnerFolderName makes
// them, unique without case. It returns a *kernel.SandboxError (invalid_argument).
func ParseRunnerFolders(folders []RunnerFolder) ([]RunnerFolder, error) {
	return parseRunnerFolders(folders, true)
}

// parseRunnerFolders checks folders; present false = the field was missing.
func parseRunnerFolders(folders []RunnerFolder, present bool) ([]RunnerFolder, error) {
	if !present || len(folders) == 0 {
		return nil, kernel.NewSandboxError("The runner must offer at least one folder", kernel.SandboxCodeInvalidArgument, "")
	}
	if len(folders) > RunnerMaxFolders {
		return nil, kernel.NewSandboxError(
			fmt.Sprintf("The runner offers %d folders; at most %d are allowed", len(folders), RunnerMaxFolders),
			kernel.SandboxCodeInvalidArgument, "")
	}
	out := make([]RunnerFolder, 0, len(folders))
	names := make([]string, 0, len(folders))
	for _, f := range folders {
		// The runner routes by these names, so a name is refused rather than changed.
		if f.Name == "" || RunnerFolderName(f.Name, names) != f.Name {
			return nil, kernel.NewSandboxError(
				fmt.Sprintf("The runner's folder name %q is not a plain, unique folder name", f.Name),
				kernel.SandboxCodeInvalidArgument, "")
		}
		out = append(out, f)
		names = append(names, f.Name)
	}
	return out, nil
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

// channelInbound (A-160) takes a channel's incoming frames.
type channelInbound interface {
	Push(data string)
	End()
	Fail(err error)
}

// runnerChannel (A-160) is the bytes of a request after (readStream, connect) or along
// with (writeStream) its response. inbound gets the runner's "data" / "end" frames,
// writer its acks; onSent runs once the request went out (writeStream starts its bytes).
type runnerChannel struct {
	inbound channelInbound
	writer  *ChannelWriter
	onSent  func()
}

func (c *runnerChannel) fail(err error) {
	if c.inbound != nil {
		c.inbound.Fail(err)
	}
	if c.writer != nil {
		c.writer.Fail(err)
	}
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
	send    func(RunnerMessage) error
	timeout time.Duration

	mu             sync.Mutex
	pending        map[string]*runnerPending
	nextID         int64
	closedReason   string
	closed         bool
	closeListeners map[int]func()
	nextListener   int
	// folders (A-159) are the runner's folders now (protocol 1: its one folder, flat at
	// /workspace). Guarded by mu.
	folders         []RunnerFolder
	folderListeners map[int]func([]RunnerFolder)
	// channels (A-160) are the open byte channels by request id. Guarded by mu.
	channels map[string]*runnerChannel
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
	folders := append([]RunnerFolder(nil), o.Hello.Folders...)
	if o.Hello.Folders == nil {
		folders = []RunnerFolder{{Name: o.Hello.FolderName, AllowExec: o.Hello.AllowExec}}
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
		limits:          limits,
		send:            o.Send,
		timeout:         timeout,
		pending:         map[string]*runnerPending{},
		closeListeners:  map[int]func(){},
		folders:         folders,
		folderListeners: map[int]func([]RunnerFolder){},
		channels:        map[string]*runnerChannel{},
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

// Hello returns the runner's hello. A-159: its folders may have changed since - see
// Folders.
func (s *RunnerSandbox) Hello() RunnerHello { return s.hello }

// Folders (A-159) are the runner's folders, the working one first.
func (s *RunnerSandbox) Folders() []RunnerFolder {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]RunnerFolder(nil), s.folders...)
}

// The layouts of a runner's folders (A-159).
const (
	// RunnerLayoutFolders (protocol 2): each folder at /workspace/<name>.
	RunnerLayoutFolders = "folders"
	// RunnerLayoutFlat (protocol 1): the one folder is /workspace itself.
	RunnerLayoutFlat = "flat"
)

// Layout (A-159) is where the folders are: RunnerLayoutFolders or RunnerLayoutFlat.
func (s *RunnerSandbox) Layout() string {
	if s.hello.Folders != nil {
		return RunnerLayoutFolders
	}
	return RunnerLayoutFlat
}

// capabilities offer commands when any folder allows them (A-159: may change with the
// folders).
// A-160: byte streams and services when the runner offers them; services run commands,
// so they need a folder with commands on too.
func (s *RunnerSandbox) capabilities() []Capability {
	caps := []Capability{CapabilityFSRead, CapabilityFSWrite, CapabilityFSSearch}
	exec := false
	for _, f := range s.Folders() {
		exec = exec || f.AllowExec
	}
	if exec {
		caps = append(caps, CapabilityExec, CapabilityExecShell)
	}
	if s.hasFeature(RunnerFeatureStream) {
		caps = append(caps, CapabilityFSStream)
	}
	if exec && s.hasFeature(RunnerFeatureServices) {
		caps = append(caps, CapabilityServices)
	}
	return caps
}

// Features (A-160) are the optional operations the runner serves.
func (s *RunnerSandbox) Features() []RunnerFeature {
	return append([]RunnerFeature(nil), s.hello.Features...)
}

func (s *RunnerSandbox) hasFeature(f RunnerFeature) bool {
	return slices.Contains(s.hello.Features, f)
}

// OnFoldersChange (A-159) calls fn with the new list when the runner's folders change.
// It returns the function that removes fn.
func (s *RunnerSandbox) OnFoldersChange(fn func([]RunnerFolder)) (unsubscribe func()) {
	s.mu.Lock()
	id := s.nextListener
	s.nextListener++
	s.folderListeners[id] = fn
	s.mu.Unlock()
	return func() {
		s.mu.Lock()
		delete(s.folderListeners, id)
		s.mu.Unlock()
	}
}

// receiveFolders takes a "folders" message (protocol 2 only; a bad list is ignored).
func (s *RunnerSandbox) receiveFolders(m RunnerMessage) {
	if s.Layout() != RunnerLayoutFolders {
		return
	}
	folders, err := parseRunnerFolders(m.Folders, m.Folders != nil)
	if err != nil {
		return
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.folders = folders
	listeners := make([]func([]RunnerFolder), 0, len(s.folderListeners))
	for _, l := range s.folderListeners {
		listeners = append(listeners, l)
	}
	s.mu.Unlock()
	for _, l := range listeners {
		l(append([]RunnerFolder(nil), folders...))
	}
}

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
	if err := json.Unmarshal(data, &m); err != nil {
		return
	}
	// A-159: the user added or removed a folder.
	if m.Type == RunnerMessageFolders {
		s.receiveFolders(m)
		return
	}
	if m.ID == "" {
		return
	}
	s.mu.Lock()
	p := s.pending[m.ID]
	// A-160: frames of a byte channel.
	if ch := s.channels[m.ID]; ch != nil {
		switch m.Type {
		case RunnerMessageData:
			s.mu.Unlock()
			if ch.inbound != nil {
				ch.inbound.Push(m.Data)
			}
			return
		case RunnerMessageAck:
			s.mu.Unlock()
			if ch.writer != nil {
				ch.writer.OnAck()
			}
			return
		case RunnerMessageEnd:
			if p == nil {
				delete(s.channels, m.ID)
			}
			s.mu.Unlock()
			if ch.inbound != nil {
				ch.inbound.End()
			}
			return
		case RunnerMessageError:
			delete(s.channels, m.ID)
			// Before the response the call fails (below) and the bytes never started;
			// after it, the bytes fail.
			if p == nil {
				s.mu.Unlock()
				ch.fail(runnerError(m, s.id))
				return
			}
		}
	}
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
		p.reply <- runnerReply{err: runnerError(m, s.id)}
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
	channels := s.channels
	s.channels = map[string]*runnerChannel{}
	s.mu.Unlock()
	for _, p := range pending {
		p.reply <- runnerReply{err: kernel.NewSandboxError(reason, kernel.SandboxCodeEnvironmentUnavailable, s.id)}
	}
	for _, ch := range channels {
		ch.fail(kernel.NewSandboxError(reason, kernel.SandboxCodeEnvironmentUnavailable, s.id))
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
// A-159: with several folders (protocol 2), each is /workspace/<name> and the first is
// the working directory. A view is made per tool call, so it sees the folders of then.
// Update (A-160): the view is a StreamView and / or a ServicesView exactly when it has
// those capabilities (the runner offers them), so a type assertion never finds methods
// the runner cannot serve.
func (s *RunnerSandbox) ForAgent(context.Context, string) (View, error) {
	v := &runnerView{sb: s, caps: s.capabilities()}
	if s.Layout() == RunnerLayoutFlat {
		v.workingDir, v.roots = WorkspaceRoot, []string{WorkspaceRoot}
	} else {
		for _, f := range s.Folders() {
			v.roots = append(v.roots, WorkspaceRoot+"/"+f.Name)
		}
		v.workingDir = v.roots[0]
	}
	stream, services := slices.Contains(v.caps, CapabilityFSStream), slices.Contains(v.caps, CapabilityServices)
	switch {
	case stream && services:
		return &runnerStreamServicesView{v}, nil
	case stream:
		return &runnerStreamView{v}, nil
	case services:
		return &runnerServicesView{v}, nil
	}
	return v, nil
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
	return s.requestAs(ctx, s.newID(), op, args, onOutput, timeout, nil)
}

// newID is the id of the next request.
func (s *RunnerSandbox) newID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	return strconv.FormatInt(s.nextID, 10)
}

// channelSend (A-160) sends the frames of request id's channel.
func (s *RunnerSandbox) channelSend(id string) ChannelSend {
	return func(m RunnerMessage) {
		m.ID = id
		_ = s.send(m)
	}
}

// closeChannel (A-160) forgets request id's channel; cancel also tells the runner to stop.
func (s *RunnerSandbox) closeChannel(id string, cancel bool) {
	s.mu.Lock()
	_, open := s.channels[id]
	delete(s.channels, id)
	s.mu.Unlock()
	if open && cancel {
		s.cancel(id)
	}
}

// failChannel (A-160) fails and forgets request id's channel (the call failed or was
// cancelled).
func (s *RunnerSandbox) failChannel(id string, err error) {
	s.mu.Lock()
	ch := s.channels[id]
	delete(s.channels, id)
	s.mu.Unlock()
	if ch != nil {
		ch.fail(err)
	}
}

// requestAs is request with the id chosen first and, for A-160's byte operations, a
// channel registered with it (frames may follow the response at once). A timeout of
// runnerNoTimeout waits for the answer until ctx ends.
func (s *RunnerSandbox) requestAs(ctx context.Context, id string, op RunnerOperation, args []any, onOutput func(OutputStream, string), timeout time.Duration, ch *runnerChannel) (json.RawMessage, error) {
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
	p := &runnerPending{op: op, reply: make(chan runnerReply, 1), onOutput: onOutput}
	s.pending[id] = p
	if ch != nil {
		s.channels[id] = ch
	}
	s.mu.Unlock()

	if err := s.send(RunnerMessage{Type: RunnerMessageRequest, ID: id, Op: op, Args: raw}); err != nil {
		s.forget(id)
		s.closeChannel(id, false)
		return nil, kernel.NewSandboxError("Could not reach the runner: "+err.Error(), kernel.SandboxCodeEnvironmentUnavailable, s.id)
	}
	if ch != nil && ch.onSent != nil {
		ch.onSent()
	}
	if timeout == 0 {
		timeout = s.timeout
	}
	var expired <-chan time.Time
	if timeout > 0 {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		expired = timer.C
	}
	done := ctx.Done()
	for {
		select {
		case r := <-p.reply:
			if r.err != nil {
				// The request failed before any bytes: its channel goes with it.
				s.failChannel(id, r.err)
			}
			return r.result, r.err
		case <-done:
			s.cancel(id)
			if op == RunnerExec {
				// Wait for the runner's answer (cancelled: true) until the timer.
				done = nil
				continue
			}
			s.forget(id)
			err := kernel.NewSandboxError("The operation was cancelled", kernel.SandboxCodeCancelled, s.id)
			s.failChannel(id, err)
			return nil, err
		case <-expired:
			s.cancel(id)
			s.forget(id)
			err := kernel.NewSandboxError(
				fmt.Sprintf("The runner did not answer within %d s", int(timeout.Round(time.Second)/time.Second)),
				kernel.SandboxCodeTimeout, s.id)
			s.failChannel(id, err)
			return nil, err
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
// A-159: its capabilities, working directory and roots are the folders' when it was made.
type runnerView struct {
	sb         *RunnerSandbox
	caps       []Capability
	workingDir string
	roots      []string
}

var _ View = (*runnerView)(nil)

func (v *runnerView) ID() string                 { return v.sb.id }
func (v *runnerView) Traits() Traits             { return v.sb.traits }
func (v *runnerView) Capabilities() []Capability { return append([]Capability(nil), v.caps...) }
func (v *runnerView) Limits() Limits             { return v.sb.limits }
func (v *runnerView) WorkingDir() string         { return v.workingDir }
func (v *runnerView) Roots() []string            { return append([]string(nil), v.roots...) }

func (v *runnerView) hasExec() bool {
	for _, c := range v.caps {
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
	return v.decode(raw, out)
}

// decode decodes a result into out. A-160: millisecond times may come with a fraction
// (a core/ts runner sends Node's mtimeMs as it is); they are cut to whole milliseconds.
func (v *runnerView) decode(raw json.RawMessage, out any) error {
	if len(raw) == 0 {
		raw = json.RawMessage("null")
	}
	if err := json.Unmarshal(raw, out); err != nil {
		fixed, ok := wholeMillis(raw)
		if !ok {
			return v.badResult(err)
		}
		if err := json.Unmarshal(fixed, out); err != nil {
			return v.badResult(err)
		}
	}
	return nil
}

// wholeMillis (A-160) rewrites fractional "modifiedAt" / "startedAt" numbers to whole
// ones; ok false when there was nothing to rewrite.
func wholeMillis(raw json.RawMessage) (json.RawMessage, bool) {
	var value any
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	if err := dec.Decode(&value); err != nil {
		return nil, false
	}
	changed := false
	var walk func(any)
	walk = func(x any) {
		switch t := x.(type) {
		case map[string]any:
			for k, val := range t {
				if n, isNum := val.(json.Number); isNum && (k == "modifiedAt" || k == "startedAt") {
					if f, err := n.Float64(); err == nil && f != math.Trunc(f) {
						t[k] = json.Number(strconv.FormatInt(int64(math.Floor(f)), 10))
						changed = true
					}
					continue
				}
				walk(val)
			}
		case []any:
			for _, val := range t {
				walk(val)
			}
		}
	}
	walk(value)
	if !changed {
		return nil, false
	}
	b, err := jsonx.Marshal(value)
	if err != nil {
		return nil, false
	}
	return b, true
}

func (v *runnerView) badResult(err error) error {
	return kernel.NewSandboxError("The runner sent a malformed result: "+err.Error(), kernel.SandboxCodeEnvironmentUnavailable, v.sb.id)
}

// --- A-160: byte streams and services ---------------------------------------------

// runnerStreamView is a runner view with byte streams.
type runnerStreamView struct{ *runnerView }

// runnerServicesView is a runner view with services.
type runnerServicesView struct{ *runnerView }

// runnerStreamServicesView is a runner view with both.
type runnerStreamServicesView struct{ *runnerView }

var (
	_ StreamView   = (*runnerStreamView)(nil)
	_ ServicesView = (*runnerServicesView)(nil)
	_ StreamView   = (*runnerStreamServicesView)(nil)
	_ ServicesView = (*runnerStreamServicesView)(nil)
)

func (v *runnerStreamView) ReadStream(ctx context.Context, path string, opts ReadStreamOptions) (*ByteStream, error) {
	return v.readStream(ctx, path, opts)
}

func (v *runnerStreamView) WriteStream(ctx context.Context, path string, body io.Reader, opts WriteStreamOptions) (WriteResult, error) {
	return v.writeStream(ctx, path, body, opts)
}

func (v *runnerStreamServicesView) ReadStream(ctx context.Context, path string, opts ReadStreamOptions) (*ByteStream, error) {
	return v.readStream(ctx, path, opts)
}

func (v *runnerStreamServicesView) WriteStream(ctx context.Context, path string, body io.Reader, opts WriteStreamOptions) (WriteResult, error) {
	return v.writeStream(ctx, path, body, opts)
}

func (v *runnerServicesView) StartService(ctx context.Context, spec ServiceSpec) (ServiceInfo, error) {
	return v.startService(ctx, spec)
}

func (v *runnerServicesView) StopService(ctx context.Context, name string) (*ServiceInfo, error) {
	return v.stopService(ctx, name)
}

func (v *runnerServicesView) ListServices(ctx context.Context) ([]ServiceInfo, error) {
	return v.listServices(ctx)
}

func (v *runnerServicesView) ServiceLogs(ctx context.Context, name string, maxBytes int) (ServiceLogs, error) {
	return v.serviceLogs(ctx, name, maxBytes)
}

func (v *runnerServicesView) Connect(ctx context.Context, port int) (io.ReadWriteCloser, error) {
	return v.connect(ctx, port)
}

func (v *runnerStreamServicesView) StartService(ctx context.Context, spec ServiceSpec) (ServiceInfo, error) {
	return v.startService(ctx, spec)
}

func (v *runnerStreamServicesView) StopService(ctx context.Context, name string) (*ServiceInfo, error) {
	return v.stopService(ctx, name)
}

func (v *runnerStreamServicesView) ListServices(ctx context.Context) ([]ServiceInfo, error) {
	return v.listServices(ctx)
}

func (v *runnerStreamServicesView) ServiceLogs(ctx context.Context, name string, maxBytes int) (ServiceLogs, error) {
	return v.serviceLogs(ctx, name, maxBytes)
}

func (v *runnerStreamServicesView) Connect(ctx context.Context, port int) (io.ReadWriteCloser, error) {
	return v.connect(ctx, port)
}

// runnerStreamBody is a ByteStream's Body: it stops watching ctx when closed.
type runnerStreamBody struct {
	*ChannelReader
	stop func() bool
}

func (b *runnerStreamBody) Close() error {
	b.stop()
	return b.ChannelReader.Close()
}

// readStream: the runner answers with the file's facts, then sends its bytes on the
// channel. The body is read under ctx: when it ends the runner is told to stop.
func (v *runnerView) readStream(ctx context.Context, path string, opts ReadStreamOptions) (*ByteStream, error) {
	sb := v.sb
	id := sb.newID()
	send := sb.channelSend(id)
	reader := NewChannelReader(
		func() { send(RunnerMessage{Type: RunnerMessageAck}) },
		func() { sb.closeChannel(id, true) },
	)
	raw, err := sb.requestAs(ctx, id, RunnerReadStream, []any{path, opts}, nil, 0, &runnerChannel{inbound: reader})
	if err != nil {
		return nil, err
	}
	var head ByteStream
	if err := v.decode(raw, &head); err != nil {
		sb.closeChannel(id, true)
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() {
		sb.closeChannel(id, true)
		reader.Fail(kernel.NewSandboxError("The operation was cancelled", kernel.SandboxCodeCancelled, sb.id))
	})
	head.Body = &runnerStreamBody{ChannelReader: reader, stop: stop}
	return &head, nil
}

// writeStream: the bytes go out on the channel while the runner writes them; it answers
// once the file is in place.
func (v *runnerView) writeStream(ctx context.Context, path string, body io.Reader, opts WriteStreamOptions) (WriteResult, error) {
	sb := v.sb
	id := sb.newID()
	writer := NewChannelWriter(sb.channelSend(id))
	pumped := make(chan error, 1)
	ch := &runnerChannel{writer: writer, onSent: func() {
		go func() {
			buf := make([]byte, RunnerStreamChunkBytes)
			for {
				n, rerr := body.Read(buf)
				if n > 0 {
					if _, werr := writer.Write(buf[:n]); werr != nil {
						pumped <- werr
						return
					}
				}
				if rerr == io.EOF {
					writer.End()
					pumped <- nil
					return
				}
				if rerr != nil {
					// The body failed (the upload was cut off): the runner drops the file.
					sb.closeChannel(id, true)
					pumped <- rerr
					return
				}
			}
		}()
	}}
	// No timeout: an upload takes as long as its bytes; ctx (and the connection) end it.
	raw, err := sb.requestAs(ctx, id, RunnerWriteStream, []any{path, opts}, nil, runnerNoTimeout, ch)
	sb.closeChannel(id, false)
	if err != nil {
		select {
		case perr := <-pumped:
			var se *kernel.SandboxError
			if perr != nil && !errors.As(perr, &se) {
				return WriteResult{}, perr
			}
		default:
		}
		return WriteResult{}, err
	}
	var out WriteResult
	if err := v.decode(raw, &out); err != nil {
		return WriteResult{}, err
	}
	return out, nil
}

func (v *runnerView) startService(ctx context.Context, spec ServiceSpec) (ServiceInfo, error) {
	var out ServiceInfo
	err := v.call(ctx, RunnerStartService, []any{spec}, &out)
	return out, err
}

func (v *runnerView) stopService(ctx context.Context, name string) (*ServiceInfo, error) {
	var out *ServiceInfo
	if err := v.call(ctx, RunnerStopService, []any{name}, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (v *runnerView) listServices(ctx context.Context) ([]ServiceInfo, error) {
	var out []ServiceInfo
	if err := v.call(ctx, RunnerListServices, []any{}, &out); err != nil {
		return nil, err
	}
	if out == nil {
		out = []ServiceInfo{}
	}
	return out, nil
}

func (v *runnerView) serviceLogs(ctx context.Context, name string, maxBytes int) (ServiceLogs, error) {
	opts := map[string]any{}
	if maxBytes > 0 {
		opts["maxBytes"] = maxBytes
	}
	var out ServiceLogs
	err := v.call(ctx, RunnerServiceLogs, []any{name, opts}, &out)
	return out, err
}

// connect: a TCP connection on the user's machine, carried on the channel both ways.
// ctx bounds opening it only.
func (v *runnerView) connect(ctx context.Context, port int) (io.ReadWriteCloser, error) {
	sb := v.sb
	id := sb.newID()
	conn := newChannelConn(sb.channelSend(id), func() { sb.closeChannel(id, true) })
	if _, err := sb.requestAs(ctx, id, RunnerConnect, []any{port}, nil, 0, &runnerChannel{inbound: conn, writer: conn.writer}); err != nil {
		return nil, err
	}
	return conn, nil
}
