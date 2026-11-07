package mcpclient

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/Phong95/aec-agent-go/components/mcphub/mcpserver"
	"github.com/Phong95/aec-agent-go/internal/jsonx"
)

// InitializeResult is what the server answered to initialize.
type InitializeResult struct {
	ProtocolVersion string                   `json:"protocolVersion"`
	Capabilities    mcpserver.Capabilities   `json:"capabilities"`
	ServerInfo      mcpserver.Implementation `json:"serverInfo"`
	Instructions    string                   `json:"instructions,omitempty"`
}

// Options configure a Client.
type Options struct {
	// RequestTimeout is the default time limit of one request (default 60 s).
	RequestTimeout time.Duration
	// OnNotification gets each notification from the server (notifications/tools/
	// list_changed, ...). It runs on a transport goroutine and must not block.
	OnNotification func(Message)
	// OnClose is called once when the connection ends without Close (the McpManager
	// reconnects). It runs on a transport goroutine and must not block.
	OnClose func(err error)
	// ClientInfo names the client in initialize (default ClientInfo).
	ClientInfo *mcpserver.Implementation
}

// ClientInfo is the clientInfo the client sends by default.
var ClientInfo = mcpserver.Implementation{Name: "aec-agent", Version: "1.0.0"}

// maxListPages is the most pages of one list call before the client stops following
// nextCursor.
const maxListPages = 50

// cancelTimeout bounds the notifications/cancelled sent after a request gave up.
const cancelTimeout = 2 * time.Second

type reply struct {
	result json.RawMessage
	err    error
}

// Client is an MCP client over one transport (the McpClient of core/ts): the initialize
// handshake, requests with time limits and cancellation, notifications, and the server's
// own requests (ping is answered, anything else gets "method not found": the client
// offers no roots, sampling or elicitation). One client = one connection; the McpManager
// makes a new one to reconnect. It is safe for concurrent use.
type Client struct {
	t    Transport
	opts Options

	mu      sync.Mutex
	nextID  int64
	pending map[string]chan reply
	closed  bool
	server  *InitializeResult
}

// New returns a client over t; nothing is sent until Connect.
func New(t Transport, opts Options) *Client {
	if opts.RequestTimeout <= 0 {
		opts.RequestTimeout = time.Duration(mcpserver.DefaultRequestMs) * time.Millisecond
	}
	return &Client{t: t, opts: opts, pending: map[string]chan reply{}}
}

// Server returns the initialize result once Connect finished.
func (c *Client) Server() (InitializeResult, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.server == nil {
		return InitializeResult{}, false
	}
	return *c.server, true
}

// Connect starts the transport and runs the initialize handshake within timeout. It
// returns a *mcpserver.Error ("connect_failed", "protocol", "timeout", ...).
func (c *Client) Connect(ctx context.Context, timeout time.Duration) (InitializeResult, error) {
	if err := c.t.Start(ctx, Handlers{OnMessage: c.handleMessage, OnClose: c.handleClose}); err != nil {
		c.mu.Lock()
		c.closed = true
		c.mu.Unlock()
		return InitializeResult{}, mcpserver.NewError("could not start the MCP transport: "+err.Error(), mcpserver.CodeConnectFailed)
	}
	info := ClientInfo
	if c.opts.ClientInfo != nil {
		info = *c.opts.ClientInfo
	}
	var raw json.RawMessage
	params := map[string]any{"protocolVersion": LatestProtocolVersion, "capabilities": map[string]any{}, "clientInfo": info}
	if err := c.Request(ctx, "initialize", params, timeout, &raw); err != nil {
		return InitializeResult{}, err
	}
	result, err := parseInitialize(raw)
	if err != nil {
		return InitializeResult{}, err
	}
	c.t.SetProtocolVersion(result.ProtocolVersion)
	if err := c.Notify(ctx, "notifications/initialized", nil); err != nil {
		return InitializeResult{}, closedError("could not send notifications/initialized", err)
	}
	c.mu.Lock()
	c.server = &result
	c.mu.Unlock()
	return result, nil
}

// Request sends a request and decodes its result into out (nil = ignore it). timeout <=
// 0 means the client's RequestTimeout. When ctx ends or the time is up the request is
// given up and notifications/cancelled is sent.
func (c *Client) Request(ctx context.Context, method string, params any, timeout time.Duration, out any) error {
	if timeout <= 0 {
		timeout = c.opts.RequestTimeout
	}
	msg := Message{JSONRPC: "2.0", Method: method}
	if params != nil {
		b, err := jsonx.Marshal(params)
		if err != nil {
			return mcpserver.NewError("could not encode the "+method+" params: "+err.Error(), mcpserver.CodeInvalidArgument)
		}
		msg.Params = b
	}
	ch := make(chan reply, 1)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return mcpserver.NewError("the MCP connection is closed", mcpserver.CodeClosed)
	}
	c.nextID++
	id := strconv.FormatInt(c.nextID, 10)
	c.pending[id] = ch
	c.mu.Unlock()
	msg.ID = json.RawMessage(id)
	defer c.forget(id)

	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := c.t.Send(reqCtx, msg); err != nil {
		// A-214: refused (401 / 403) - the reason wins over the "closed" the transport's
		// lost connection may have put in ch meanwhile.
		var refused *mcpserver.Error
		if errors.As(err, &refused) && refused.Code == mcpserver.CodeUnauthorized {
			return refused
		}
		// The answer may have been read before the send failed (it never is, but be safe).
		select {
		case r := <-ch:
			return decodeReply(r, out)
		default:
		}
		if reqCtx.Err() != nil {
			return c.gaveUp(ctx, id, method, timeout)
		}
		var me *mcpserver.Error
		if errors.As(err, &me) {
			return me
		}
		return closedError("could not send "+method, err)
	}
	select {
	case r := <-ch:
		return decodeReply(r, out)
	case <-reqCtx.Done():
		return c.gaveUp(ctx, id, method, timeout)
	}
}

// gaveUp sends notifications/cancelled for a request that ran out of time or whose ctx
// ended, and returns its error.
func (c *Client) gaveUp(ctx context.Context, id, method string, timeout time.Duration) error {
	reason := "timeout"
	if ctx.Err() != nil {
		reason = "aborted"
	}
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cancelTimeout)
	defer cancel()
	_ = c.Notify(cctx, "notifications/cancelled", map[string]any{"requestId": json.RawMessage(id), "reason": reason})
	if err := ctx.Err(); err != nil {
		return err
	}
	return mcpserver.NewError("MCP request "+method+" timed out after "+strconv.FormatInt(timeout.Milliseconds(), 10)+" ms", mcpserver.CodeTimeout)
}

// Notify sends a notification. On a closed client it does nothing.
func (c *Client) Notify(ctx context.Context, method string, params any) error {
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return nil
	}
	msg := Message{JSONRPC: "2.0", Method: method}
	if params != nil {
		b, err := jsonx.Marshal(params)
		if err != nil {
			return err
		}
		msg.Params = b
	}
	return c.t.Send(ctx, msg)
}

// Ping sends a ping and waits for its answer.
func (c *Client) Ping(ctx context.Context, timeout time.Duration) error {
	return c.Request(ctx, "ping", nil, timeout, nil)
}

// ListTools reads every page of tools/list.
func (c *Client) ListTools(ctx context.Context, timeout time.Duration) ([]mcpserver.Tool, error) {
	return listAll[mcpserver.Tool](ctx, c, "tools/list", "tools", timeout)
}

// ListResources reads every page of resources/list.
func (c *Client) ListResources(ctx context.Context, timeout time.Duration) ([]mcpserver.Resource, error) {
	return listAll[mcpserver.Resource](ctx, c, "resources/list", "resources", timeout)
}

// ListPrompts reads every page of prompts/list.
func (c *Client) ListPrompts(ctx context.Context, timeout time.Duration) ([]mcpserver.Prompt, error) {
	return listAll[mcpserver.Prompt](ctx, c, "prompts/list", "prompts", timeout)
}

// CallTool calls a tool (tools/call). A nil args is sent as {}.
func (c *Client) CallTool(ctx context.Context, name string, args map[string]any, timeout time.Duration) (mcpserver.CallToolResult, error) {
	if args == nil {
		args = map[string]any{}
	}
	var out mcpserver.CallToolResult
	if err := c.Request(ctx, "tools/call", map[string]any{"name": name, "arguments": args}, timeout, &out); err != nil {
		return mcpserver.CallToolResult{}, err
	}
	if out.Content == nil {
		out.Content = []mcpserver.Content{}
	}
	return out, nil
}

// ReadResource reads a resource (resources/read).
func (c *Client) ReadResource(ctx context.Context, uri string, timeout time.Duration) (mcpserver.ReadResourceResult, error) {
	var out mcpserver.ReadResourceResult
	err := c.Request(ctx, "resources/read", map[string]any{"uri": uri}, timeout, &out)
	return out, err
}

// GetPrompt gets a prompt (prompts/get). A nil args is sent as {}.
func (c *Client) GetPrompt(ctx context.Context, name string, args map[string]string, timeout time.Duration) (mcpserver.GetPromptResult, error) {
	if args == nil {
		args = map[string]string{}
	}
	var out mcpserver.GetPromptResult
	err := c.Request(ctx, "prompts/get", map[string]any{"name": name, "arguments": args}, timeout, &out)
	return out, err
}

// Close closes the connection; waiting requests fail with "closed". OnClose is not
// called. It is idempotent.
func (c *Client) Close() error {
	c.mu.Lock()
	c.closed = true
	c.failPendingLocked(mcpserver.NewError("the MCP connection was closed", mcpserver.CodeClosed))
	c.mu.Unlock()
	return c.t.Close()
}

func (c *Client) forget(id string) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

func (c *Client) handleMessage(m Message) {
	switch {
	case m.IsRequest():
		c.answerServerRequest(m)
	case m.IsNotification():
		if c.opts.OnNotification != nil {
			c.opts.OnNotification(m)
		}
	case m.IsResponse():
		c.mu.Lock()
		ch, ok := c.pending[string(m.ID)]
		if ok {
			delete(c.pending, string(m.ID))
		}
		c.mu.Unlock()
		if !ok {
			return
		}
		if m.Error != nil {
			e := mcpserver.NewError("MCP server error "+strconv.Itoa(m.Error.Code)+": "+m.Error.Message, mcpserver.CodeServerError)
			e.RPCCode = m.Error.Code
			ch <- reply{err: e}
			return
		}
		ch <- reply{result: m.Result}
	}
}

func (c *Client) answerServerRequest(m Message) {
	out := Message{JSONRPC: "2.0", ID: m.ID}
	if m.Method == "ping" {
		out.Result = json.RawMessage("{}")
	} else {
		out.Error = &RPCError{Code: MethodNotFound, Message: "method not supported: " + m.Method}
	}
	ctx, cancel := context.WithTimeout(context.Background(), cancelTimeout)
	defer cancel()
	_ = c.t.Send(ctx, out)
}

func (c *Client) handleClose(err error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	text := "the MCP connection closed"
	if err != nil {
		text += ": " + err.Error()
	}
	c.failPendingLocked(mcpserver.NewError(text, mcpserver.CodeClosed))
	c.mu.Unlock()
	if c.opts.OnClose != nil {
		c.opts.OnClose(err)
	}
}

func (c *Client) failPendingLocked(err error) {
	for id, ch := range c.pending {
		delete(c.pending, id)
		ch <- reply{err: err}
	}
}

func decodeReply(r reply, out any) error {
	if r.err != nil {
		return r.err
	}
	if out == nil || len(r.result) == 0 {
		return nil
	}
	if raw, ok := out.(*json.RawMessage); ok {
		*raw = slices.Clone(r.result)
		return nil
	}
	if err := json.Unmarshal(r.result, out); err != nil {
		return mcpserver.NewError("the MCP server sent a result that does not decode: "+err.Error(), mcpserver.CodeProtocol)
	}
	return nil
}

func listAll[T any](ctx context.Context, c *Client, method, key string, timeout time.Duration) ([]T, error) {
	items := []T{}
	cursor := ""
	for range maxListPages {
		var params any
		if cursor != "" {
			params = map[string]any{"cursor": cursor}
		}
		var page map[string]json.RawMessage
		if err := c.Request(ctx, method, params, timeout, &page); err != nil {
			return nil, err
		}
		raw, ok := page[key]
		if !ok {
			return nil, mcpserver.NewError(method+" returned no "+key+" list", mcpserver.CodeProtocol)
		}
		var list []T
		if err := json.Unmarshal(raw, &list); err != nil {
			return nil, mcpserver.NewError(method+" returned a "+key+" list that does not decode: "+err.Error(), mcpserver.CodeProtocol)
		}
		items = append(items, list...)
		cursor = ""
		if next, ok := page["nextCursor"]; ok {
			_ = json.Unmarshal(next, &cursor)
		}
		if cursor == "" {
			break
		}
	}
	return items, nil
}

func parseInitialize(raw json.RawMessage) (InitializeResult, error) {
	var r InitializeResult
	if len(raw) == 0 || json.Unmarshal(raw, &r) != nil {
		return InitializeResult{}, mcpserver.NewError("initialize returned no valid result", mcpserver.CodeProtocol)
	}
	if !slices.Contains(ProtocolVersions, r.ProtocolVersion) {
		got := r.ProtocolVersion
		if got == "" {
			got = "undefined"
		}
		return InitializeResult{}, mcpserver.NewError("the MCP server wants protocol version "+got+"; this client speaks "+joinVersions(), mcpserver.CodeProtocol)
	}
	return r, nil
}

func joinVersions() string {
	out := ""
	for i, v := range ProtocolVersions {
		if i > 0 {
			out += ", "
		}
		out += v
	}
	return out
}

func closedError(what string, err error) error {
	return mcpserver.NewError(what+": "+err.Error(), mcpserver.CodeClosed)
}
