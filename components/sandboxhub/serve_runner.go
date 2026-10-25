package sandboxhub

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"

	"github.com/Phong95/aec-agent-go/internal/jsonx"
	"github.com/Phong95/aec-agent-go/kernel"
)

// ---------------------------------------------------------------------------
// The client side of a runner (A-153): a client app opens the folder the user chose
// with host.OpenRunnerFolder, connects to the server, sends RunnerHelloFor and hands
// every message from the server to ServeRunner's handle. Each operation runs in a host
// sandbox over that folder, so paths outside it (.., absolute paths, symlinks that
// lead out) fail with path_escape. Commands start in the folder but are not confined
// by the OS: the server asks the user before each one, and AllowExec false turns them off.
// Update (A-159): the view is a *RunnerFolders (host.OpenRunnerFolders), so one runner
// offers several folders at /workspace/<name>; when the user adds or removes one, the
// app sends RunnerFoldersUpdateFor on the same connection.
// Update (A-160): byte streams and services too, when the view has them (hello.features):
// a stream's bytes and a connect tunnel travel on a channel with the request's id
// (runner_channel.go). Services run commands, so DenyExec refuses them too.
// ---------------------------------------------------------------------------

// RunnerFeaturesFor (A-160) are the optional operations a view can serve.
func RunnerFeaturesFor(view View) []RunnerFeature {
	var features []RunnerFeature
	if _, ok := AsStreamView(view); ok {
		features = append(features, RunnerFeatureStream)
	}
	if _, ok := AsServicesView(view); ok {
		features = append(features, RunnerFeatureServices)
	}
	return features
}

// runnerAnswered (A-160) is what run returns when it answered on its own (a channel
// operation): no "response" is sent for it.
type runnerAnswered struct{}

// RunnerHelloFor is the first message a runner sends (protocol 2: the folders, the
// working one first).
func RunnerHelloFor(view *RunnerFolders) RunnerHello {
	limits := view.Limits()
	folders := view.Folders()
	hello := RunnerHello{
		Type:     RunnerMessageHello,
		Protocol: RunnerProtocolVersion,
		OS:       view.Traits().OS,
		Folders:  folders,
		Limits:   &limits,
		Features: RunnerFeaturesFor(view),
	}
	if len(folders) > 0 {
		hello.FolderName = folders[0].Name
	}
	for _, f := range folders {
		hello.AllowExec = hello.AllowExec || f.AllowExec
	}
	return hello
}

// RunnerFoldersUpdateFor (A-159) tells the server the runner's folders changed (send it
// on the open connection).
func RunnerFoldersUpdateFor(view *RunnerFolders) RunnerMessage {
	return RunnerMessage{Type: RunnerMessageFolders, Folders: view.Folders()}
}

// ServeRunnerOptions configures ServeRunner.
type ServeRunnerOptions struct {
	// DenyExec refuses commands (policy_denied). The zero value allows them.
	DenyExec bool
}

// RunnerHandle answers one server connection. It is safe for concurrent use.
type RunnerHandle struct {
	view     View
	send     func(RunnerMessage) error
	denyExec bool

	mu      sync.Mutex
	running map[string]context.CancelFunc
	closed  bool
	// channels (A-160) are the open channels by request id - incoming bytes, outgoing
	// acks, a tunnel's connection. Guarded by mu.
	channels map[string]*serveChannel
}

// serveChannel (A-160) is one open channel of a request on the runner side.
type serveChannel struct {
	reader *ChannelReader
	writer *ChannelWriter
	conn   io.ReadWriteCloser
}

func (c *serveChannel) stop(err error) {
	if c.reader != nil {
		c.reader.Fail(err)
	}
	if c.writer != nil {
		c.writer.Fail(err)
	}
	if c.conn != nil {
		_ = c.conn.Close()
	}
}

// ServeRunner answers the server's requests with view, sending results through send.
// Each request runs in its own goroutine.
func ServeRunner(view View, send func(RunnerMessage) error, opts ServeRunnerOptions) *RunnerHandle {
	return &RunnerHandle{view: view, send: send, denyExec: opts.DenyExec, running: map[string]context.CancelFunc{}, channels: map[string]*serveChannel{}}
}

func (h *RunnerHandle) channelSend(id string) ChannelSend {
	return func(m RunnerMessage) {
		m.ID = id
		h.reply(m)
	}
}

func (h *RunnerHandle) setChannel(id string, ch *serveChannel) {
	h.mu.Lock()
	h.channels[id] = ch
	h.mu.Unlock()
}

func (h *RunnerHandle) channel(id string) *serveChannel {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.channels[id]
}

func (h *RunnerHandle) dropChannel(id string) {
	h.mu.Lock()
	delete(h.channels, id)
	h.mu.Unlock()
}

// Receive takes every message from the server. Malformed messages are ignored.
func (h *RunnerHandle) Receive(data []byte) {
	var m RunnerMessage
	if err := json.Unmarshal(data, &m); err != nil || m.ID == "" {
		return
	}
	switch m.Type {
	case RunnerMessageCancel:
		h.mu.Lock()
		cancel := h.running[m.ID]
		// A-160: and its channel.
		ch := h.channels[m.ID]
		delete(h.channels, m.ID)
		h.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		if ch != nil {
			ch.stop(kernel.NewSandboxError("The operation was cancelled", kernel.SandboxCodeCancelled, ""))
		}
	case RunnerMessageRequest:
		h.start(m)
	// A-160: frames of a channel.
	case RunnerMessageAck:
		if ch := h.channel(m.ID); ch != nil && ch.writer != nil {
			ch.writer.OnAck()
		}
	case RunnerMessageData:
		if ch := h.channel(m.ID); ch != nil && ch.reader != nil {
			ch.reader.Push(m.Data)
		}
	case RunnerMessageEnd:
		if ch := h.channel(m.ID); ch != nil && ch.reader != nil {
			ch.reader.End()
		}
	}
}

// Close stops every running operation (the connection closed).
// A-160: open streams stop and tunnels close with the connection.
func (h *RunnerHandle) Close() {
	h.mu.Lock()
	h.closed = true
	running := h.running
	h.running = map[string]context.CancelFunc{}
	channels := h.channels
	h.channels = map[string]*serveChannel{}
	h.mu.Unlock()
	for _, cancel := range running {
		cancel()
	}
	for _, ch := range channels {
		ch.stop(kernel.NewSandboxError("The connection closed", kernel.SandboxCodeCancelled, ""))
	}
}

func (h *RunnerHandle) start(m RunnerMessage) {
	switch m.Op {
	case RunnerReadFile, RunnerWriteFile, RunnerStat, RunnerList, RunnerSearch, RunnerExec,
		RunnerReadStream, RunnerWriteStream, RunnerStartService, RunnerStopService, RunnerListServices, RunnerServiceLogs, RunnerConnect:
	default:
		h.reply(RunnerMessage{Type: RunnerMessageError, ID: m.ID, Message: `Unknown runner operation "` + string(m.Op) + `"`, Code: kernel.SandboxCodeInvalidArgument})
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	h.mu.Lock()
	if h.closed || h.running[m.ID] != nil {
		h.mu.Unlock()
		cancel()
		return
	}
	h.running[m.ID] = cancel
	// A-160: set up before the next message: the server's bytes follow the request at once.
	if m.Op == RunnerWriteStream {
		id := m.ID
		h.channels[id] = &serveChannel{reader: NewChannelReader(func() { h.reply(RunnerMessage{Type: RunnerMessageAck, ID: id}) }, nil)}
	}
	h.mu.Unlock()
	go func() {
		defer func() {
			h.mu.Lock()
			delete(h.running, m.ID)
			h.mu.Unlock()
			cancel()
		}()
		result, err := h.run(ctx, m)
		if _, ok := result.(runnerAnswered); ok && err == nil {
			return
		}
		if err != nil {
			code := kernel.SandboxCodeEnvironmentUnavailable
			var se *kernel.SandboxError
			if errors.As(err, &se) {
				code = se.Code
			}
			h.reply(RunnerMessage{Type: RunnerMessageError, ID: m.ID, Message: errorMessage(err), Code: code})
			return
		}
		raw, err := jsonx.Marshal(result)
		if err != nil {
			h.reply(RunnerMessage{Type: RunnerMessageError, ID: m.ID, Message: err.Error(), Code: kernel.SandboxCodeEnvironmentUnavailable})
			return
		}
		h.reply(RunnerMessage{Type: RunnerMessageResponse, ID: m.ID, Result: raw})
	}()
}

func errorMessage(err error) string {
	var se *kernel.SandboxError
	if errors.As(err, &se) {
		return se.Message
	}
	return err.Error()
}

func (h *RunnerHandle) run(ctx context.Context, m RunnerMessage) (any, error) {
	switch m.Op {
	case RunnerReadFile:
		var opts ReadOptions
		path, err := runnerArgs(m.Args, &opts)
		if err != nil {
			return nil, err
		}
		return h.view.ReadFile(ctx, path, opts)
	case RunnerWriteFile:
		var path, content string
		var opts WriteOptions
		if err := decodeArgs(m.Args, &path, &content, &opts); err != nil {
			return nil, err
		}
		return h.view.WriteFile(ctx, path, content, opts)
	case RunnerStat:
		path, err := runnerArgs(m.Args, nil)
		if err != nil {
			return nil, err
		}
		return h.view.Stat(ctx, path)
	case RunnerList:
		var opts ListOptions
		path, err := runnerArgs(m.Args, &opts)
		if err != nil {
			return nil, err
		}
		return h.view.List(ctx, path, opts)
	case RunnerSearch:
		var q SearchQuery
		if err := decodeArgs(m.Args, &q); err != nil {
			return nil, err
		}
		return h.view.Search(ctx, q)
	case RunnerReadStream, RunnerWriteStream, RunnerStartService, RunnerStopService, RunnerListServices, RunnerServiceLogs, RunnerConnect:
		return h.runA160(ctx, m)
	default: // RunnerExec
		if h.denyExec {
			return nil, kernel.NewSandboxError("Commands are turned off for this folder in the app", kernel.SandboxCodePolicyDenied, "")
		}
		var req ExecRequest
		if err := decodeArgs(m.Args, &req); err != nil {
			return nil, err
		}
		id := m.ID
		return h.view.Exec(ctx, req, ExecOptions{OnOutput: func(stream OutputStream, data string) {
			h.reply(RunnerMessage{Type: RunnerMessageOutput, ID: id, Stream: stream, Data: data})
		}})
	}
}

// runnerArgs decodes [path, options?]: the path is required, the options optional.
func runnerArgs(args []json.RawMessage, opts any) (string, error) {
	var path string
	targets := []any{&path}
	if opts != nil && len(args) > 1 {
		targets = append(targets, opts)
	}
	return path, decodeArgs(args, targets...)
}

// decodeArgs decodes args[i] into targets[i]; a missing or null argument after the
// first leaves its target zero. The first argument is required.
func decodeArgs(args []json.RawMessage, targets ...any) error {
	if len(args) == 0 {
		return kernel.NewSandboxError("The request has no arguments", kernel.SandboxCodeInvalidArgument, "")
	}
	for i, t := range targets {
		if i >= len(args) {
			break
		}
		if string(args[i]) == "null" && i > 0 {
			continue
		}
		if err := json.Unmarshal(args[i], t); err != nil {
			return kernel.NewSandboxError("Malformed argument: "+err.Error(), kernel.SandboxCodeInvalidArgument, "")
		}
	}
	return nil
}

// reply sends a message; a send failure is ignored (the server fails the call itself).
func (h *RunnerHandle) reply(m RunnerMessage) {
	_ = h.send(m)
}

func unsupportedOp(op RunnerOperation) error {
	return kernel.NewSandboxError(`This runner does not serve "`+string(op)+`"`, kernel.SandboxCodeCapabilityUnsupported, "")
}

// runA160 runs the byte stream and service operations (A-160).
func (h *RunnerHandle) runA160(ctx context.Context, m RunnerMessage) (any, error) {
	id := m.ID
	switch m.Op {
	case RunnerReadStream:
		sv, ok := AsStreamView(h.view)
		if !ok {
			return nil, unsupportedOp(m.Op)
		}
		var opts ReadStreamOptions
		path, err := runnerArgs(m.Args, &opts)
		if err != nil {
			return nil, err
		}
		stream, err := sv.ReadStream(ctx, path, opts)
		if err != nil {
			return nil, err
		}
		defer func() { _ = stream.Body.Close() }()
		writer := NewChannelWriter(h.channelSend(id))
		h.setChannel(id, &serveChannel{writer: writer})
		defer h.dropChannel(id)
		head, err := jsonx.Marshal(stream)
		if err != nil {
			return nil, err
		}
		h.reply(RunnerMessage{Type: RunnerMessageResponse, ID: id, Result: head})
		buf := make([]byte, RunnerStreamChunkBytes)
		for {
			n, rerr := stream.Body.Read(buf)
			if n > 0 {
				if _, werr := writer.Write(buf[:n]); werr != nil {
					return runnerAnswered{}, nil
				}
			}
			if rerr == io.EOF {
				writer.End()
				return runnerAnswered{}, nil
			}
			if rerr != nil {
				if ctx.Err() == nil {
					h.replyError(id, rerr)
				}
				return runnerAnswered{}, nil
			}
		}
	case RunnerWriteStream:
		defer h.dropChannel(id)
		ch := h.channel(id)
		sv, ok := AsStreamView(h.view)
		if !ok || ch == nil {
			return nil, unsupportedOp(m.Op)
		}
		var path string
		var opts WriteStreamOptions
		if err := decodeArgs(m.Args, &path, &opts); err != nil {
			return nil, err
		}
		return sv.WriteStream(ctx, path, ch.reader, opts)
	case RunnerStartService:
		svc, ok := AsServicesView(h.view)
		if !ok {
			return nil, unsupportedOp(m.Op)
		}
		if h.denyExec {
			return nil, kernel.NewSandboxError("Commands are turned off for this folder in the app", kernel.SandboxCodePolicyDenied, "")
		}
		var spec ServiceSpec
		if err := decodeArgs(m.Args, &spec); err != nil {
			return nil, err
		}
		return svc.StartService(ctx, spec)
	case RunnerStopService:
		svc, ok := AsServicesView(h.view)
		if !ok {
			return nil, unsupportedOp(m.Op)
		}
		var name string
		if err := decodeArgs(m.Args, &name); err != nil {
			return nil, err
		}
		return svc.StopService(ctx, name)
	case RunnerListServices:
		svc, ok := AsServicesView(h.view)
		if !ok {
			return nil, unsupportedOp(m.Op)
		}
		return svc.ListServices(ctx)
	case RunnerServiceLogs:
		svc, ok := AsServicesView(h.view)
		if !ok {
			return nil, unsupportedOp(m.Op)
		}
		var name string
		var opts struct {
			MaxBytes int `json:"maxBytes"`
		}
		if err := decodeArgs(m.Args, &name, &opts); err != nil {
			return nil, err
		}
		return svc.ServiceLogs(ctx, name, opts.MaxBytes)
	default: // RunnerConnect
		svc, ok := AsServicesView(h.view)
		if !ok {
			return nil, unsupportedOp(m.Op)
		}
		var port int
		if err := decodeArgs(m.Args, &port); err != nil {
			return nil, err
		}
		conn, err := svc.Connect(ctx, port)
		if err != nil {
			return nil, err
		}
		h.serveTunnel(id, conn)
		return runnerAnswered{}, nil
	}
}

// serveTunnel (A-160) carries a connect tunnel: the connection's bytes go out on the
// channel, the server's frames are written to it (each answered once taken).
func (h *RunnerHandle) serveTunnel(id string, conn io.ReadWriteCloser) {
	writer := NewChannelWriter(h.channelSend(id))
	reader := NewChannelReader(func() { h.reply(RunnerMessage{Type: RunnerMessageAck, ID: id}) }, nil)
	h.setChannel(id, &serveChannel{reader: reader, writer: writer, conn: conn})
	h.reply(RunnerMessage{Type: RunnerMessageResponse, ID: id, Result: json.RawMessage("null")})
	// Server -> connection; "end" from the server closes the connection's write side.
	go func() {
		_, _ = io.Copy(conn, reader)
		if cw, ok := conn.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		} else {
			_ = conn.Close()
		}
	}()
	// Connection -> server; when it closes the server hears "end".
	go func() {
		buf := make([]byte, RunnerStreamChunkBytes)
		for {
			n, err := conn.Read(buf)
			if n > 0 {
				if _, werr := writer.Write(buf[:n]); werr != nil {
					break
				}
			}
			if err != nil {
				break
			}
		}
		h.dropChannel(id)
		writer.End()
		writer.Fail(kernel.NewSandboxError("The connection closed", kernel.SandboxCodeCancelled, ""))
		reader.Fail(kernel.NewSandboxError("The connection closed", kernel.SandboxCodeCancelled, ""))
		_ = conn.Close()
	}()
}

// replyError sends an "error" for request id.
func (h *RunnerHandle) replyError(id string, err error) {
	code := kernel.SandboxCodeEnvironmentUnavailable
	var se *kernel.SandboxError
	if errors.As(err, &se) {
		code = se.Code
	}
	h.reply(RunnerMessage{Type: RunnerMessageError, ID: id, Message: errorMessage(err), Code: code})
}
