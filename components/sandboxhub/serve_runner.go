package sandboxhub

import (
	"context"
	"encoding/json"
	"errors"
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
// ---------------------------------------------------------------------------

// RunnerHelloFor is the first message a runner sends.
func RunnerHelloFor(view View, folderName string, allowExec bool) RunnerHello {
	limits := view.Limits()
	return RunnerHello{
		Type:       RunnerMessageHello,
		Protocol:   RunnerProtocolVersion,
		OS:         view.Traits().OS,
		FolderName: folderName,
		AllowExec:  allowExec,
		Limits:     &limits,
	}
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
}

// ServeRunner answers the server's requests with view, sending results through send.
// Each request runs in its own goroutine.
func ServeRunner(view View, send func(RunnerMessage) error, opts ServeRunnerOptions) *RunnerHandle {
	return &RunnerHandle{view: view, send: send, denyExec: opts.DenyExec, running: map[string]context.CancelFunc{}}
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
		h.mu.Unlock()
		if cancel != nil {
			cancel()
		}
	case RunnerMessageRequest:
		h.start(m)
	}
}

// Close stops every running operation (the connection closed).
func (h *RunnerHandle) Close() {
	h.mu.Lock()
	h.closed = true
	running := h.running
	h.running = map[string]context.CancelFunc{}
	h.mu.Unlock()
	for _, cancel := range running {
		cancel()
	}
}

func (h *RunnerHandle) start(m RunnerMessage) {
	switch m.Op {
	case RunnerReadFile, RunnerWriteFile, RunnerStat, RunnerList, RunnerSearch, RunnerExec:
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
	h.mu.Unlock()
	go func() {
		defer func() {
			h.mu.Lock()
			delete(h.running, m.ID)
			h.mu.Unlock()
			cancel()
		}()
		result, err := h.run(ctx, m)
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
