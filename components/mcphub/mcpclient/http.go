package mcpclient

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Phong95/aec-agent-go/components/mcphub/mcpserver"
	"github.com/Phong95/aec-agent-go/internal/jsonx"
)

// maxStreamRetries is how often the transport (re)opens the server's GET event stream
// before it stops listening on it.
const maxStreamRetries = 3

// maxBody bounds one JSON answer or SSE event (16 MiB): a broken server must not fill
// memory.
const maxBody = 16 << 20

// HTTPOptions configure an HTTPTransport.
type HTTPOptions struct {
	URL string
	// Headers are extra request headers, already resolved (no {{secret:...}} left).
	Headers map[string]string
	// Auth is the server's OAuth sign-in (A-214): a bearer token on every request, one
	// refresh after a 401. nil = none.
	Auth HTTPAuth
	// Client is the HTTP client (default http.DefaultClient).
	Client *http.Client
}

// HTTPTransport is MCP streamable HTTP (the HttpTransport of core/ts): every message is
// a POST to the endpoint; the answer is JSON or an SSE stream that carries the response
// (and any notifications sent with it). The server's session id (Mcp-Session-Id) and the
// agreed protocol version go on every later request. After the handshake a GET stream is
// opened for messages the server sends on its own (list_changed); a server without one
// answers 405 and that is fine. A 404 for the session means the server dropped it: the
// connection is reported closed (the McpManager reconnects with a new session). Close
// deletes the session.
//
// A-214: with an OAuth sign-in (HTTPOptions.Auth) every request carries its bearer token;
// a 401 refreshes it once and sends the request again. A 401 / 403 that stays is a
// *mcpserver.Error "unauthorized" (the MCP manager marks the server needs_auth); after the
// handshake it also ends the connection.
type HTTPTransport struct {
	opts   HTTPOptions
	client *http.Client

	mu              sync.Mutex
	h               Handlers
	sessionID       string
	protocolVersion string
	closed          bool
	// done is closed by Close or a lost session; in-flight requests and the GET stream
	// end with it.
	done chan struct{}
	wg   sync.WaitGroup
}

// NewHTTP returns an HTTP transport; nothing is sent until Start / Send.
func NewHTTP(opts HTTPOptions) *HTTPTransport {
	c := opts.Client
	if c == nil {
		c = http.DefaultClient
	}
	return &HTTPTransport{opts: opts, client: c, done: make(chan struct{})}
}

// HTTPFactory is the Factory of the http transport type.
func HTTPFactory(cfg mcpserver.Transport, tc TransportContext) (Transport, error) {
	return NewHTTP(HTTPOptions{URL: cfg.URL, Headers: cfg.Headers, Auth: tc.Auth}), nil
}

var _ Transport = (*HTTPTransport)(nil)

// Start checks the url and keeps the handlers.
func (t *HTTPTransport) Start(_ context.Context, h Handlers) error {
	u, err := url.Parse(t.opts.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errors.New("invalid MCP url " + t.opts.URL)
	}
	t.mu.Lock()
	t.h = h
	t.mu.Unlock()
	return nil
}

// SetProtocolVersion keeps the agreed version for the later requests.
func (t *HTTPTransport) SetProtocolVersion(v string) {
	t.mu.Lock()
	t.protocolVersion = v
	t.mu.Unlock()
}

// Send POSTs one message and reads its answer.
func (t *HTTPTransport) Send(ctx context.Context, m Message) error {
	body, err := jsonx.Marshal(m)
	if err != nil {
		return err
	}
	ctx, cancel := t.bound(ctx)
	defer cancel()
	token := t.token(ctx)
	resp, err := t.post(ctx, body, token)
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusUnauthorized && t.opts.Auth != nil {
		// The token ran out or was revoked: refresh once and send again.
		_ = resp.Body.Close()
		if t.opts.Auth.Refresh(ctx, token) {
			token = t.token(ctx)
			if resp, err = t.post(ctx, body, token); err != nil {
				return err
			}
		}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return t.refused(resp, token)
	}
	if s := resp.Header.Get("Mcp-Session-Id"); s != "" {
		t.mu.Lock()
		t.sessionID = s
		t.mu.Unlock()
	}
	if resp.StatusCode == http.StatusNotFound && t.session() != "" {
		t.lost(errors.New("the MCP server ended the session (404)"))
		return mcpserver.NewError("the MCP server ended the session", mcpserver.CodeClosed)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		text, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		msg := "MCP server answered HTTP " + strconv.Itoa(resp.StatusCode)
		if len(text) > 0 {
			msg += ": " + string(text)
		}
		return mcpserver.NewError(msg, mcpserver.CodeConnectFailed)
	}
	if resp.StatusCode == http.StatusAccepted || !m.IsRequest() {
		// A notification or a response to the server: nothing comes back.
		if m.Method == "notifications/initialized" {
			t.openStream(0) //nolint:contextcheck // the GET stream outlives this request; Close ends it
		}
		return nil
	}
	if strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		return t.readEvents(resp.Body)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return err
	}
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return nil
	}
	if data[0] == '[' {
		var batch []Message
		if err := json.Unmarshal(data, &batch); err != nil {
			return mcpserver.NewError("the MCP server answered with something that is not JSON-RPC", mcpserver.CodeProtocol)
		}
		for _, msg := range batch {
			t.deliver(msg)
		}
		return nil
	}
	var msg Message
	if err := json.Unmarshal(data, &msg); err != nil {
		return mcpserver.NewError("the MCP server answered with something that is not JSON-RPC", mcpserver.CodeProtocol)
	}
	t.deliver(msg)
	return nil
}

func (t *HTTPTransport) post(ctx context.Context, body []byte, token string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.opts.URL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	t.setHeaders(req, token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	return t.client.Do(req)
}

// token is the current bearer token ("" without a sign-in or when it can't be read).
func (t *HTTPTransport) token(ctx context.Context) string {
	if t.opts.Auth == nil {
		return ""
	}
	tok, err := t.opts.Auth.Token(ctx)
	if err != nil {
		return ""
	}
	return tok
}

// refused is the error of a 401 / 403 that stayed; mid-session (the sign-in was revoked)
// the connection is over too.
func (t *HTTPTransport) refused(resp *http.Response, token string) error {
	challenge := ParseWWWAuthenticate(resp.Header.Get("WWW-Authenticate"))
	own := token != ""
	for k := range t.opts.Headers {
		if strings.EqualFold(k, "authorization") {
			own = true
		}
	}
	msg := "the MCP server needs a sign-in"
	if own {
		msg = "the MCP server refused the credentials"
	}
	msg += " (HTTP " + strconv.Itoa(resp.StatusCode)
	detail := challenge["error_description"]
	if detail == "" {
		detail = challenge["error"]
	}
	if detail != "" {
		msg += ": " + detail
	}
	err := mcpserver.NewError(msg+")", mcpserver.CodeUnauthorized)
	t.mu.Lock()
	handshakeDone := t.protocolVersion != ""
	t.mu.Unlock()
	if handshakeDone {
		// The Client returns this error to the waiting request (not "closed").
		t.lost(err)
	}
	return err
}

// Close ends the GET stream and the in-flight requests and deletes the session (best
// effort: the server may not support DELETE or be gone already). Idempotent.
func (t *HTTPTransport) Close() error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil
	}
	t.closed = true
	close(t.done)
	session := t.sessionID
	t.mu.Unlock()
	t.wg.Wait()
	if session == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, t.opts.URL, nil)
	if err != nil {
		return nil
	}
	t.setHeaders(req, t.token(ctx))
	if resp, err := t.client.Do(req); err == nil {
		_ = resp.Body.Close()
	}
	return nil
}

// bound returns ctx that also ends when the transport closes.
func (t *HTTPTransport) bound(ctx context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(ctx)
	go func() {
		select {
		case <-t.done:
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, cancel
}

func (t *HTTPTransport) session() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.sessionID
}

func (t *HTTPTransport) setHeaders(req *http.Request, token string) {
	for k, v := range t.opts.Headers {
		req.Header.Set(k, v)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	t.mu.Lock()
	session, version := t.sessionID, t.protocolVersion
	t.mu.Unlock()
	if session != "" {
		req.Header.Set("Mcp-Session-Id", session)
	}
	if version != "" {
		req.Header.Set("Mcp-Protocol-Version", version)
	}
}

func (t *HTTPTransport) deliver(m Message) {
	t.mu.Lock()
	h := t.h.OnMessage
	t.mu.Unlock()
	if h != nil {
		h(m)
	}
}

// openStream opens the GET stream for messages the server sends on its own, in a
// goroutine Close waits for.
func (t *HTTPTransport) openStream(attempt int) {
	t.mu.Lock()
	if t.closed || attempt > maxStreamRetries {
		t.mu.Unlock()
		return
	}
	t.wg.Add(1)
	t.mu.Unlock()
	go func() {
		defer t.wg.Done()
		for try := attempt; try <= maxStreamRetries; try++ {
			if try > attempt {
				select {
				case <-t.done:
					return
				case <-time.After(time.Duration(try) * time.Second):
				}
			}
			again := t.stream()
			if !again {
				return
			}
		}
	}()
}

// stream runs one GET stream to its end; it reports whether to open it again.
func (t *HTTPTransport) stream() bool {
	ctx, cancel := t.bound(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.opts.URL, nil)
	if err != nil {
		return false
	}
	t.setHeaders(req, t.token(ctx))
	req.Header.Set("Accept", "text/event-stream")
	resp, err := t.client.Do(req)
	if err != nil {
		return ctx.Err() == nil
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound && t.session() != "" {
		t.lost(errors.New("the MCP server ended the session (404)"))
		return false
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 || !strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		// 405: the server has no stream of its own; it can still answer every request.
		return false
	}
	_ = t.readEvents(resp.Body)
	return ctx.Err() == nil
}

// readEvents reads an SSE body to its end; each data event is one JSON-RPC message.
func (t *HTTPTransport) readEvents(body io.Reader) error {
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 64*1024), maxBody)
	var data []string
	event := ""
	dispatch := func() {
		if len(data) > 0 && (event == "" || event == "message") {
			var msg Message
			if json.Unmarshal([]byte(strings.Join(data, "\n")), &msg) == nil {
				t.deliver(msg)
			}
		}
		data = data[:0]
		event = ""
	}
	for sc.Scan() {
		line := strings.TrimSuffix(sc.Text(), "\r")
		switch {
		case line == "":
			dispatch()
		case strings.HasPrefix(line, ":"):
		default:
			field, val, _ := strings.Cut(line, ":")
			val = strings.TrimPrefix(val, " ")
			switch field {
			case "data":
				data = append(data, val)
			case "event":
				event = val
			}
		}
	}
	dispatch()
	return sc.Err()
}

// lost reports the connection closed (the server dropped the session).
func (t *HTTPTransport) lost(err error) {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return
	}
	t.closed = true
	close(t.done)
	h := t.h.OnClose
	t.mu.Unlock()
	if h != nil {
		h(err)
	}
}
