// Package mcptest is a small fake MCP server for the SDK's MCP tests (A-208), the Go
// form of core/ts tests/fixtures/fake-mcp-server.mjs. It is used two ways: ServeStdio
// makes a test binary act as an MCP server over stdio (the helper-process pattern), and
// HTTP serves the same logic over MCP streamable HTTP for httptest.
//
// Tools: echo, add (tools/list is paged: one tool per page), plus control tools the tests
// call: add_tool (adds a tool, then sends tools/list_changed), crash (the stdio process
// exits), env (returns an environment variable), slow (answers after `ms`).
package mcptest

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Phong95/aec-agent-go/components/mcphub/mcpclient"
	"github.com/Phong95/aec-agent-go/components/mcphub/mcpserver"
	"github.com/Phong95/aec-agent-go/internal/jsonx"
)

// HelperEnv is the environment variable that turns a test binary into the stdio fake
// server (its value is "stdio"); FakeVersionEnv makes it answer initialize with another
// protocol version.
const (
	HelperEnv      = "AEC_FAKE_MCP"
	FakeVersionEnv = "FAKE_MCP_VERSION"
)

// Server is the fake server logic. It is safe for concurrent use.
type Server struct {
	mu    sync.Mutex
	tools []mcpserver.Tool
	push  func(mcpclient.Message)
	// Version is the protocol version initialize answers with ("" = 2025-06-18).
	Version string
	// OnCrash runs for the crash tool (stdio: the process exits).
	OnCrash func()
}

// New returns a server whose notifications go to push.
func New(push func(mcpclient.Message)) *Server {
	obj := map[string]any{"type": "object"}
	ro := true
	return &Server{push: push, tools: []mcpserver.Tool{
		{Name: "echo", Description: "Echo the text back", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"text": map[string]any{"type": "string"}}, "required": []any{"text"}}, Annotations: &mcpserver.ToolAnnotations{ReadOnlyHint: &ro}},
		{Name: "add", Description: "Add two numbers", InputSchema: obj},
		{Name: "add_tool", Description: "Test control", InputSchema: obj},
		{Name: "crash", Description: "Test control", InputSchema: obj},
		{Name: "env", Description: "Test control", InputSchema: obj},
		{Name: "slow", Description: "Test control", InputSchema: obj},
	}}
}

func result(id json.RawMessage, v any) mcpclient.Message {
	b, _ := jsonx.Marshal(v)
	return mcpclient.Message{JSONRPC: "2.0", ID: id, Result: b}
}

func text(id json.RawMessage, s string, extra map[string]any) mcpclient.Message {
	out := map[string]any{"content": []any{map[string]any{"type": "text", "text": s}}}
	for k, v := range extra {
		out[k] = v
	}
	return result(id, out)
}

// Handle answers one message; the bool is false for a notification.
func (s *Server) Handle(msg mcpclient.Message) (mcpclient.Message, bool) {
	if !msg.IsRequest() {
		return mcpclient.Message{}, false
	}
	var p map[string]any
	_ = json.Unmarshal(msg.Params, &p)
	switch msg.Method {
	case "initialize":
		v := s.Version
		if v == "" {
			v = "2025-06-18"
		}
		return result(msg.ID, map[string]any{
			"protocolVersion": v,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": true}, "resources": map[string]any{"listChanged": true}, "prompts": map[string]any{"listChanged": true}},
			"serverInfo":      map[string]any{"name": "fake-mcp", "version": "0.0.1"},
			"instructions":    "Use echo to test.",
		}), true
	case "ping":
		return result(msg.ID, map[string]any{}), true
	case "tools/list":
		start := 0
		if c, ok := p["cursor"].(string); ok {
			start, _ = strconv.Atoi(c)
		}
		s.mu.Lock()
		tools := append([]mcpserver.Tool(nil), s.tools...)
		s.mu.Unlock()
		out := map[string]any{"tools": []mcpserver.Tool{}}
		if start < len(tools) {
			out["tools"] = tools[start : start+1]
		}
		if start+1 < len(tools) {
			out["nextCursor"] = strconv.Itoa(start + 1)
		}
		return result(msg.ID, out), true
	case "tools/call":
		args, _ := p["arguments"].(map[string]any)
		switch p["name"] {
		case "echo":
			return text(msg.ID, toString(args["text"]), nil), true
		case "add":
			a, _ := args["a"].(float64)
			b, _ := args["b"].(float64)
			return text(msg.ID, strconv.FormatFloat(a+b, 'f', -1, 64), map[string]any{"structuredContent": map[string]any{"sum": a + b}}), true
		case "add_tool":
			s.mu.Lock()
			s.tools = append(s.tools, mcpserver.Tool{Name: toString(args["name"]), InputSchema: map[string]any{"type": "object"}})
			s.mu.Unlock()
			go func() {
				time.Sleep(10 * time.Millisecond)
				s.push(mcpclient.Message{JSONRPC: "2.0", Method: "notifications/tools/list_changed"})
			}()
			return text(msg.ID, "added", nil), true
		case "crash":
			if s.OnCrash != nil {
				go func() {
					time.Sleep(10 * time.Millisecond)
					s.OnCrash()
				}()
			}
			return text(msg.ID, "bye", nil), true
		case "env":
			return text(msg.ID, os.Getenv(toString(args["name"])), nil), true
		case "slow":
			ms, _ := args["ms"].(float64)
			time.Sleep(time.Duration(ms) * time.Millisecond)
			return text(msg.ID, "late", nil), true
		}
		return text(msg.ID, "unknown tool", map[string]any{"isError": true}), true
	case "resources/list":
		return result(msg.ID, map[string]any{"resources": []any{map[string]any{"uri": "file:///readme.md", "name": "readme", "mimeType": "text/markdown"}}}), true
	case "resources/read":
		return result(msg.ID, map[string]any{"contents": []any{map[string]any{"uri": p["uri"], "mimeType": "text/markdown", "text": "# Readme"}}}), true
	case "prompts/list":
		return result(msg.ID, map[string]any{"prompts": []any{map[string]any{"name": "greet", "arguments": []any{map[string]any{"name": "who", "required": true}}}}}), true
	case "prompts/get":
		a, _ := p["arguments"].(map[string]any)
		return result(msg.ID, map[string]any{"messages": []any{map[string]any{"role": "user", "content": map[string]any{"type": "text", "text": "Hello " + toString(a["who"])}}}}), true
	}
	return mcpclient.Message{JSONRPC: "2.0", ID: msg.ID, Error: &mcpclient.RPCError{Code: -32601, Message: "no method " + msg.Method}}, true
}

func toString(v any) string {
	s, _ := v.(string)
	return s
}

// ServeStdio runs the server over stdio until in ends; it returns the process exit code.
// The crash tool exits the process with code 3.
func ServeStdio(in io.Reader, out io.Writer) int {
	var wmu sync.Mutex
	write := func(m mcpclient.Message) {
		b, _ := jsonx.Marshal(m)
		wmu.Lock()
		_, _ = out.Write(append(b, '\n'))
		wmu.Unlock()
	}
	s := New(write)
	s.Version = os.Getenv(FakeVersionEnv)
	s.OnCrash = func() { os.Exit(3) }
	_, _ = os.Stderr.WriteString("fake-mcp: started\n")
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 64*1024), 16<<20)
	var wg sync.WaitGroup
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var m mcpclient.Message
		if json.Unmarshal([]byte(line), &m) != nil {
			continue
		}
		wg.Go(func() {
			if r, ok := s.Handle(m); ok {
				write(r)
			}
		})
	}
	wg.Wait()
	return 0
}

// HTTP is the fake server behind MCP streamable HTTP. Its exported fields may be set by
// the test at any time (they are read under its lock through the setters).
type HTTP struct {
	srv *Server

	mu       sync.Mutex
	sse      bool
	down     bool
	n        int
	sessions map[string]bool
	streams  map[chan mcpclient.Message]bool
	deleted  []string
	auth     []string
}

// NewHTTP returns the HTTP fake; serve it with httptest.NewServer.
func NewHTTP() *HTTP {
	h := &HTTP{sessions: map[string]bool{}, streams: map[chan mcpclient.Message]bool{}}
	h.srv = New(h.pushAll)
	return h
}

// SetSSE makes POST answers SSE streams.
func (h *HTTP) SetSSE(v bool) { h.mu.Lock(); h.sse = v; h.mu.Unlock() }

// SetDown makes every request answer 503.
func (h *HTTP) SetDown(v bool) { h.mu.Lock(); h.down = v; h.mu.Unlock() }

// ForgetSessions drops every session (the next request with one gets 404) and ends the
// GET streams.
func (h *HTTP) ForgetSessions() {
	h.mu.Lock()
	h.sessions = map[string]bool{}
	for ch := range h.streams {
		close(ch)
		delete(h.streams, ch)
	}
	h.mu.Unlock()
}

// Sessions is the number of live sessions.
func (h *HTTP) Sessions() int { h.mu.Lock(); defer h.mu.Unlock(); return len(h.sessions) }

// Streams is the number of open GET streams.
func (h *HTTP) Streams() int { h.mu.Lock(); defer h.mu.Unlock(); return len(h.streams) }

// Deleted is the sessions a DELETE ended.
func (h *HTTP) Deleted() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.deleted...)
}

// Auth is the Authorization headers of the initialize requests.
func (h *HTTP) Auth() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.auth...)
}

// Close ends the GET streams (call it before closing the httptest server).
func (h *HTTP) Close() { h.ForgetSessions() }

func (h *HTTP) pushAll(m mcpclient.Message) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.streams {
		select {
		case ch <- m:
		default:
		}
	}
}

// ServeHTTP implements MCP streamable HTTP for the fake server.
func (h *HTTP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	down, sse := h.down, h.sse
	session := r.Header.Get("Mcp-Session-Id")
	known := h.sessions[session]
	h.mu.Unlock()
	if down {
		http.Error(w, "down", http.StatusServiceUnavailable)
		return
	}
	switch r.Method {
	case http.MethodDelete:
		h.mu.Lock()
		if session != "" {
			h.deleted = append(h.deleted, session)
		}
		h.mu.Unlock()
		w.WriteHeader(http.StatusOK)
		return
	case http.MethodGet:
		if !known {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		ch := make(chan mcpclient.Message, 16)
		h.mu.Lock()
		h.streams[ch] = true
		h.mu.Unlock()
		defer func() {
			h.mu.Lock()
			if h.streams[ch] {
				delete(h.streams, ch)
			}
			h.mu.Unlock()
		}()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, ": open\n\n")
		flush(w)
		for {
			select {
			case <-r.Context().Done():
				return
			case m, ok := <-ch:
				if !ok {
					return
				}
				b, _ := jsonx.Marshal(m)
				_, _ = io.WriteString(w, "event: message\ndata: "+string(b)+"\n\n")
				flush(w)
			}
		}
	}
	body, _ := io.ReadAll(r.Body)
	var msg mcpclient.Message
	if json.Unmarshal(body, &msg) != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if msg.Method == "initialize" {
		h.mu.Lock()
		if a := r.Header.Get("Authorization"); a != "" {
			h.auth = append(h.auth, a)
		}
		h.n++
		id := "s" + strconv.Itoa(h.n)
		h.sessions[id] = true
		h.mu.Unlock()
		w.Header().Set("Mcp-Session-Id", id)
	} else if !known {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	reply, ok := h.srv.Handle(msg)
	if !ok {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	b, _ := jsonx.Marshal(reply)
	if sse {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message\ndata: "+string(b)+"\n\n")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(b)
}

func flush(w http.ResponseWriter) {
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}
