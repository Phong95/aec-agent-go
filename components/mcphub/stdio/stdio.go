// Package stdio is the MCP stdio transport (A-208): the server is a child process of the
// host; messages are newline-delimited JSON on its stdin / stdout, and the tail of its
// stderr is kept for error messages. It is the StdioTransport of core/ts.
//
// It starts a host process, so - like the sandboxhub/host and sandboxhub/docker
// backends - it is an opt-in package outside the hubs: a host that allows stdio MCP
// servers registers it on the McpHub,
//
//	core.McpHub().RegisterTransport(mcpserver.TransportStdio, stdio.Factory)
//
// and without it a stdio server fails to connect ("no transport for stdio"). The child
// gets only a small safe set of the host's environment variables (Environment) plus the
// definition's env, never the host's own keys.
package stdio

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/Phong95/aec-agent-go/components/mcphub/mcpclient"
	"github.com/Phong95/aec-agent-go/components/mcphub/mcpserver"
	"github.com/Phong95/aec-agent-go/internal/jsonx"
)

// stderrTail is how many of the last characters of the child's stderr are kept.
const stderrTail = 2_000

// maxLine bounds one line from the child (16 MiB): a broken server must not fill memory.
const maxLine = 16 << 20

// The time Close gives the child to exit after its stdin closed, then after the kill.
const (
	exitGrace = 2 * time.Second
	killGrace = 5 * time.Second
)

// inheritedEnv are the host's variables a stdio server inherits - enough to find
// programs and a home folder, never the host's own keys.
func inheritedEnv() []string {
	if runtime.GOOS == "windows" {
		return []string{"APPDATA", "HOMEDRIVE", "HOMEPATH", "LOCALAPPDATA", "PATH", "PATHEXT", "PROCESSOR_ARCHITECTURE", "SYSTEMDRIVE", "SYSTEMROOT", "TEMP", "TMP", "USERNAME", "USERPROFILE", "PROGRAMFILES", "WINDIR"}
	}
	return []string{"HOME", "LOGNAME", "PATH", "SHELL", "TERM", "USER", "LANG", "TMPDIR"}
}

// Environment returns the environment a stdio server starts with: the safe inherited
// set of the host's variables plus env, as KEY=VALUE pairs.
func Environment(env map[string]string) []string {
	keep := inheritedEnv()
	out := []string{}
	for _, kv := range os.Environ() {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || strings.HasPrefix(v, "()") {
			continue
		}
		name := k
		if runtime.GOOS == "windows" {
			name = strings.ToUpper(k)
		}
		if _, override := env[k]; override {
			continue
		}
		for _, want := range keep {
			if name == want {
				out = append(out, kv)
				break
			}
		}
	}
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}

// Factory is the mcpclient.Factory of the stdio transport type.
func Factory(cfg mcpserver.Transport, _ mcpclient.TransportContext) (mcpclient.Transport, error) {
	if cfg.Command == "" {
		return nil, errors.New("a stdio MCP server needs a command")
	}
	return New(cfg.Command, cfg.Args, cfg.Env, cfg.Cwd), nil
}

// Transport is one stdio connection.
type Transport struct {
	command string
	args    []string
	env     map[string]string
	cwd     string

	writeMu sync.Mutex
	stdin   io.WriteCloser

	mu      sync.Mutex
	cmd     *exec.Cmd
	h       mcpclient.Handlers
	stderr  string
	closing bool
	ended   bool
	// exited is closed once the child exited and its pipes were read to the end.
	exited chan struct{}
}

var _ mcpclient.Transport = (*Transport)(nil)

// New returns a stdio transport for command; nothing runs until Start.
func New(command string, args []string, env map[string]string, cwd string) *Transport {
	return &Transport{command: command, args: args, env: env, cwd: cwd, exited: make(chan struct{})}
}

// Stderr returns the last lines the server wrote to stderr.
func (t *Transport) Stderr() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.stderr
}

// Start starts the child process. The process outlives ctx: it runs until Close or until
// it exits.
func (t *Transport) Start(_ context.Context, h mcpclient.Handlers) error {
	//nolint:gosec,noctx // G204: the command is the host's MCP server definition by design; the process must outlive the Start ctx, so it is ended by Close, not by a ctx
	cmd := exec.Command(t.command, t.args...)
	cmd.Env = Environment(t.env)
	cmd.Dir = t.cwd
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	t.mu.Lock()
	t.h = h
	t.mu.Unlock()
	if err := cmd.Start(); err != nil {
		return err
	}
	t.writeMu.Lock()
	t.stdin = stdin
	t.writeMu.Unlock()
	t.mu.Lock()
	t.cmd = cmd
	t.mu.Unlock()
	var readers sync.WaitGroup
	readers.Go(func() { t.readStdout(stdout) })
	readers.Go(func() { t.readStderr(stderr) })
	go func() {
		readers.Wait()
		err := cmd.Wait()
		why := "code 0"
		if err != nil {
			why = err.Error()
		}
		tail := strings.TrimSpace(t.Stderr())
		if len(tail) > 500 {
			tail = tail[len(tail)-500:]
		}
		msg := "the MCP server process exited (" + why + ")"
		if tail != "" {
			msg += ": " + tail
		}
		t.end(errors.New(msg))
		close(t.exited)
	}()
	return nil
}

// SetProtocolVersion does nothing: stdio carries no version header.
func (t *Transport) SetProtocolVersion(string) {}

// Send writes one message as a line to the child's stdin.
func (t *Transport) Send(_ context.Context, m mcpclient.Message) error {
	b, err := jsonx.Marshal(m)
	if err != nil {
		return err
	}
	t.mu.Lock()
	running := t.cmd != nil && !t.ended
	t.mu.Unlock()
	if !running {
		return errors.New("the MCP server process is not running")
	}
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	if t.stdin == nil {
		return errors.New("the MCP server process is not running")
	}
	_, err = t.stdin.Write(append(b, '\n'))
	return err
}

// Close ends stdin, then kills the child if it does not exit, and waits for it.
// Idempotent; OnClose is not called.
func (t *Transport) Close() error {
	t.mu.Lock()
	t.closing = true
	cmd := t.cmd
	t.mu.Unlock()
	if cmd == nil {
		return nil
	}
	t.writeMu.Lock()
	if t.stdin != nil {
		_ = t.stdin.Close()
	}
	t.writeMu.Unlock()
	select {
	case <-t.exited:
		return nil
	case <-time.After(exitGrace):
	}
	_ = cmd.Process.Kill()
	select {
	case <-t.exited:
	case <-time.After(killGrace):
	}
	return nil
}

func (t *Transport) readStdout(r io.Reader) {
	br := bufio.NewReaderSize(r, 64*1024)
	var line []byte
	for {
		chunk, isPrefix, err := br.ReadLine()
		if len(chunk) > 0 && len(line)+len(chunk) <= maxLine {
			line = append(line, chunk...)
		}
		if err != nil {
			return
		}
		if isPrefix {
			continue
		}
		text := strings.TrimSpace(string(line))
		line = line[:0]
		if text == "" {
			continue
		}
		var m mcpclient.Message
		if json.Unmarshal([]byte(text), &m) != nil {
			// Not a JSON-RPC message (a server logging to stdout): ignored.
			continue
		}
		t.mu.Lock()
		h := t.h.OnMessage
		t.mu.Unlock()
		if h != nil {
			h(m)
		}
	}
}

func (t *Transport) readStderr(r io.Reader) {
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			t.mu.Lock()
			t.stderr += string(buf[:n])
			if len(t.stderr) > stderrTail {
				t.stderr = t.stderr[len(t.stderr)-stderrTail:]
			}
			t.mu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

func (t *Transport) end(err error) {
	t.mu.Lock()
	if t.ended {
		t.mu.Unlock()
		return
	}
	t.ended = true
	closing := t.closing
	h := t.h.OnClose
	t.mu.Unlock()
	if !closing && h != nil {
		h(err)
	}
}
