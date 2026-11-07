package agent

import (
	"context"
	"errors"
	"log/slog"
	"net/url"
	"slices"
	"sync"
	"time"

	"github.com/Phong95/aec-agent-go/components/agent/profile"
	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/components/mcphub"
	"github.com/Phong95/aec-agent-go/components/mcphub/mcpclient"
	"github.com/Phong95/aec-agent-go/components/mcphub/mcpserver"
	"github.com/Phong95/aec-agent-go/components/secrethub/secret"
	"github.com/Phong95/aec-agent-go/components/variablehub/variable"
	"github.com/Phong95/aec-agent-go/kernel"
)

// McpSecretTool is the tool name a secret must allow to be put in an MCP server's
// headers or env (A-208).
const McpSecretTool = "mcp"

// McpReconnectPolicy is how the MCP manager tries again after a lost connection or a
// failed try (A-208).
type McpReconnectPolicy struct {
	// BaseDelay is the delay before the first try; it doubles each try. Default 1 s.
	BaseDelay time.Duration
	// MaxDelay is the longest delay between tries. Default 30 s.
	MaxDelay time.Duration
	// MaxAttempts is the tries before the server is marked failed (0 = never give up).
	// Default 5.
	MaxAttempts int
}

// DefaultMcpReconnect is the default reconnect policy.
var DefaultMcpReconnect = McpReconnectPolicy{BaseDelay: time.Second, MaxDelay: 30 * time.Second, MaxAttempts: 5}

// DefaultMcpHealthInterval is how often a connected server is pinged (a failed ping
// counts as a lost connection).
const DefaultMcpHealthInterval = 60 * time.Second

// mcpManagerDeps is what the MCP manager needs from its agent.
type mcpManagerDeps struct {
	agentID string
	// profile is read on every sync, so UpdateMcp applies.
	profile func() profile.AgentProfile
	// hub is the host's McpHub; nil = the agent has no MCP servers.
	hub *mcphub.McpHub
	bus events.Bus
	// secrets fills {{secret:NAME}} in headers / env; nil = such a server can't connect.
	secrets secret.Secrets
	// variables (A-215) fills {{var:NAME}} in headers / env before the secrets; nil = such a
	// server can't connect.
	variables variable.Variables
	// lifetime is the agent's lifetime context; background tries and health checks derive
	// from it.
	lifetime context.Context
	logger   *slog.Logger
	policy   *McpReconnectPolicy
	// health is the ping interval; nil = DefaultMcpHealthInterval, 0 = none.
	health *time.Duration
}

type mcpRefresh struct{ dirty bool }

// mcpConn is one of the agent's servers. Every field is guarded by mcpManager.mu.
type mcpConn struct {
	id     string
	server *mcpserver.Server
	state  mcpserver.State
	client *mcpclient.Client
	// gen is bumped by every new try and every close: callbacks of an older client are
	// ignored.
	gen        uint64
	retry      *time.Timer
	healthStop chan struct{}
	refreshing map[string]*mcpRefresh
	// everConnected is set once the server connected (the next connected event says
	// reconnected).
	everConnected bool
}

// mcpManager is the agent's MCP connections (A-208; the McpManager of core/ts). Which
// servers it uses comes from the profile (Mcp.Servers, ids of McpHub servers). On start
// it connects each one: the transport (built by the McpHub, with {{secret:NAME}} filled
// in from the agent's secrets), the initialize handshake, then discovery of the tools,
// resources and prompts the server says it has. It keeps them current on list_changed,
// pings connected servers, reconnects with backoff when a connection is lost, and closes
// everything on dispose. It announces each change on the bus (mcp:*) and owns nothing
// else. It is safe for concurrent use.
//
// A-214: an HTTP server without its own Authorization header uses the McpHub's OAuth
// sign-in for it; a server that refuses the connection is needs_auth (not retried) until
// mcp-server:auth-changed says it was signed in.
type mcpManager struct {
	d      mcpManagerDeps
	policy McpReconnectPolicy
	health time.Duration
	// bg is the context of the background work (retries, health checks, refreshes); it
	// derives from the agent's lifetime and dispose cancels it first.
	bg       context.Context
	bgCancel context.CancelFunc

	mu       sync.Mutex
	conns    map[string]*mcpConn
	started  bool
	disposed bool
	wg       sync.WaitGroup

	syncMu sync.Mutex
	unsub  []events.Unsubscribe
}

var _ mcpserver.Connections = (*mcpManager)(nil)

func newMcpManager(d mcpManagerDeps) *mcpManager {
	if d.logger == nil {
		d.logger = slog.Default()
	}
	m := &mcpManager{d: d, policy: DefaultMcpReconnect, health: DefaultMcpHealthInterval, conns: map[string]*mcpConn{}}
	if d.policy != nil {
		m.policy = *d.policy
	}
	if d.health != nil {
		m.health = *d.health
	}
	m.bg, m.bgCancel = context.WithCancel(d.lifetime) //nolint:contextcheck // the background work derives from the agent's lifetime on purpose (10-SECURITY UF-3)
	return m
}

// start connects every server in the profile; it returns once each has connected or
// failed its first try.
func (m *mcpManager) start(ctx context.Context) error {
	m.mu.Lock()
	if m.disposed {
		m.mu.Unlock()
		return nil
	}
	if !m.started {
		m.started = true
		// Subscribed on start, so an agent whose New failed leaves no subscriber behind.
		m.unsub = []events.Unsubscribe{
			events.SubscribeCtx(m.d.bus, events.McpServerChanged, func(ctx context.Context, _ kernel.Event, p events.McpServerChangedPayload) {
				m.onServerChanged(ctx, p.ServerID, p.Change)
			}),
			// A-214: signed in (or the sign-in was dropped): connect again with the new
			// tokens. Emitted, not awaited - the reconnect runs in the background.
			events.Subscribe(m.d.bus, events.McpAuthChanged, func(_ kernel.Event, p events.McpAuthChangedPayload) {
				m.goTracked(func(ctx context.Context) { m.onServerChanged(ctx, p.ServerID, "updated") })
			}),
		}
	}
	m.mu.Unlock()
	return m.sync(ctx)
}

// sync applies the profile's server list: the servers added connect, the ones removed
// close. Before start it does nothing.
func (m *mcpManager) sync(ctx context.Context) error {
	m.syncMu.Lock()
	defer m.syncMu.Unlock()
	m.mu.Lock()
	if !m.started || m.disposed {
		m.mu.Unlock()
		return nil
	}
	wanted := m.wanted()
	var gone, added []string
	for id := range m.conns {
		if !slices.Contains(wanted, id) {
			gone = append(gone, id)
		}
	}
	for _, id := range wanted {
		if _, ok := m.conns[id]; !ok {
			added = append(added, id)
		}
	}
	m.mu.Unlock()
	for _, id := range gone {
		m.close(id, "closed")
	}
	var wg sync.WaitGroup
	for _, id := range added {
		wg.Go(func() { m.open(ctx, id, nil) })
	}
	wg.Wait()
	return nil
}

func (m *mcpManager) wanted() []string {
	p := m.d.profile()
	if p.Mcp == nil {
		return nil
	}
	out := []string{}
	for _, id := range p.Mcp.Servers {
		if !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	return out
}

// List returns every server in the profile, in profile order.
func (m *mcpManager) List() []mcpserver.State {
	wanted := m.wanted()
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []mcpserver.State{}
	for _, id := range wanted {
		if c, ok := m.conns[id]; ok {
			out = append(out, c.state.Clone())
		}
	}
	return out
}

// Get returns one server's state.
func (m *mcpManager) Get(serverID string) (mcpserver.State, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.conns[serverID]
	if !ok {
		return mcpserver.State{}, false
	}
	return c.state.Clone(), true
}

// Reconnect closes the connection (if any) and connects again now.
func (m *mcpManager) Reconnect(ctx context.Context, serverID string) (mcpserver.State, error) {
	m.mu.Lock()
	c, ok := m.conns[serverID]
	m.mu.Unlock()
	if !ok {
		return mcpserver.State{}, notMine(serverID)
	}
	m.drop(c, "closed")
	m.mu.Lock()
	c.state.Attempts = 0
	m.mu.Unlock()
	m.open(ctx, serverID, c)
	st, _ := m.Get(serverID)
	return st, nil
}

// Ping does a ping round trip.
func (m *mcpManager) Ping(ctx context.Context, serverID string, opts mcpserver.CallOptions) error {
	client, server, err := m.require(serverID)
	if err != nil {
		return err
	}
	return client.Ping(ctx, timeoutOf(server, opts))
}

// CallTool calls one of the server's tools.
func (m *mcpManager) CallTool(ctx context.Context, serverID, name string, args map[string]any, opts mcpserver.CallOptions) (mcpserver.CallToolResult, error) {
	client, server, err := m.require(serverID)
	if err != nil {
		return mcpserver.CallToolResult{}, err
	}
	return client.CallTool(ctx, name, args, timeoutOf(server, opts))
}

// ReadResource reads one of the server's resources.
func (m *mcpManager) ReadResource(ctx context.Context, serverID, uri string, opts mcpserver.CallOptions) (mcpserver.ReadResourceResult, error) {
	client, server, err := m.require(serverID)
	if err != nil {
		return mcpserver.ReadResourceResult{}, err
	}
	return client.ReadResource(ctx, uri, timeoutOf(server, opts))
}

// GetPrompt gets one of the server's prompts.
func (m *mcpManager) GetPrompt(ctx context.Context, serverID, name string, args map[string]string, opts mcpserver.CallOptions) (mcpserver.GetPromptResult, error) {
	client, server, err := m.require(serverID)
	if err != nil {
		return mcpserver.GetPromptResult{}, err
	}
	return client.GetPrompt(ctx, name, args, timeoutOf(server, opts))
}

// dispose closes every connection, stops listening and waits for the background work.
func (m *mcpManager) dispose(ctx context.Context) error {
	m.mu.Lock()
	if m.disposed {
		m.mu.Unlock()
		return nil
	}
	m.disposed = true
	ids := make([]string, 0, len(m.conns))
	for id := range m.conns {
		ids = append(ids, id)
	}
	unsub := m.unsub
	m.mu.Unlock()
	for _, off := range unsub {
		off()
	}
	m.bgCancel()
	// A sync in progress finishes first; the ones after see disposed and do nothing.
	m.syncMu.Lock()
	defer m.syncMu.Unlock()
	for _, id := range ids {
		m.close(id, "closed")
	}
	done := make(chan struct{})
	go func() {
		m.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// goTracked runs f in a goroutine dispose waits for; after dispose it does nothing.
func (m *mcpManager) goTracked(f func(ctx context.Context)) {
	m.mu.Lock()
	if m.disposed {
		m.mu.Unlock()
		return
	}
	m.wg.Add(1)
	m.mu.Unlock()
	go func() {
		defer m.wg.Done()
		f(m.bg)
	}()
}

// open looks the server up and connects (first try). existing keeps the state of a
// reconnect.
func (m *mcpManager) open(ctx context.Context, serverID string, existing *mcpConn) {
	m.mu.Lock()
	if m.disposed {
		m.mu.Unlock()
		return
	}
	c := existing
	if c == nil {
		c = &mcpConn{id: serverID, state: mcpserver.State{ServerID: serverID, Name: serverID, Status: mcpserver.StatusConnecting}, refreshing: map[string]*mcpRefresh{}}
	}
	m.conns[serverID] = c
	gen := c.gen
	m.mu.Unlock()

	var server mcpserver.Server
	found := false
	if m.d.hub != nil {
		var err error
		server, found, err = m.d.hub.Get(ctx, serverID)
		if err != nil {
			m.fail(c, gen, "could not read the MCP server definition: "+err.Error(), true)
			return
		}
	}
	m.mu.Lock()
	if m.conns[serverID] != c || m.disposed || c.gen != gen {
		m.mu.Unlock()
		return
	}
	if !found {
		c.server = nil
		m.mu.Unlock()
		m.fail(c, gen, `no MCP server with id "`+serverID+`"`, false)
		return
	}
	c.server = &server
	c.state.Name = server.Name
	if !server.Enabled {
		c.state.Status = mcpserver.StatusDisabled
		c.state.Error = ""
		m.mu.Unlock()
		return
	}
	m.mu.Unlock()
	m.connect(ctx, c)
}

// connect is one try: transport, handshake, discovery. On failure a retry is scheduled.
func (m *mcpManager) connect(ctx context.Context, c *mcpConn) {
	m.mu.Lock()
	c.gen++
	gen := c.gen
	server := *c.server
	if c.everConnected || c.state.Attempts > 0 {
		c.state.Status = mcpserver.StatusReconnecting
	} else {
		c.state.Status = mcpserver.StatusConnecting
	}
	m.mu.Unlock()

	transport, err := m.buildTransport(ctx, server)
	if err != nil {
		m.failTry(c, gen, err)
		return
	}
	client := mcpclient.New(transport, mcpclient.Options{
		RequestTimeout: time.Duration(server.RequestTimeoutMs()) * time.Millisecond,
		OnNotification: func(n mcpclient.Message) {
			m.goTracked(func(ctx context.Context) { m.onNotification(ctx, c, gen, n) })
		},
		OnClose: func(err error) {
			m.goTracked(func(context.Context) { m.onLost(c, gen, err) })
		},
	})
	init, err := client.Connect(ctx, time.Duration(server.ConnectTimeoutMs())*time.Millisecond)
	if err != nil {
		_ = client.Close()
		m.failTry(c, gen, err)
		return
	}
	m.mu.Lock()
	if gen != c.gen || m.disposed {
		m.mu.Unlock()
		_ = client.Close()
		return
	}
	c.client = client
	m.mu.Unlock()

	caps := init.Capabilities
	tools, resources, prompts := []mcpserver.Tool{}, []mcpserver.Resource{}, []mcpserver.Prompt{}
	reqTimeout := time.Duration(server.RequestTimeoutMs()) * time.Millisecond
	var discoverErrs []string
	if caps.Tools != nil {
		if l, err := client.ListTools(ctx, reqTimeout); err == nil {
			tools = l
		} else {
			discoverErrs = append(discoverErrs, "tools/list failed: "+err.Error())
		}
	}
	if caps.Resources != nil {
		if l, err := client.ListResources(ctx, reqTimeout); err == nil {
			resources = l
		} else {
			discoverErrs = append(discoverErrs, "resources/list failed: "+err.Error())
		}
	}
	if caps.Prompts != nil {
		if l, err := client.ListPrompts(ctx, reqTimeout); err == nil {
			prompts = l
		} else {
			discoverErrs = append(discoverErrs, "prompts/list failed: "+err.Error())
		}
	}

	m.mu.Lock()
	if gen != c.gen || m.disposed {
		m.mu.Unlock()
		return
	}
	info := init.ServerInfo
	capsCopy := init.Capabilities
	c.state.ProtocolVersion = init.ProtocolVersion
	c.state.ServerInfo = &info
	c.state.Capabilities = &capsCopy
	c.state.Instructions = init.Instructions
	c.state.Tools, c.state.Resources, c.state.Prompts = tools, resources, prompts
	c.state.Status = mcpserver.StatusConnected
	c.state.ConnectedAt = kernel.MillisToISO(kernel.NowMillis())
	c.state.Attempts = 0
	c.state.Error = ""
	if len(discoverErrs) > 0 {
		c.state.Error = discoverErrs[len(discoverErrs)-1]
	}
	reconnected := c.everConnected
	c.everConnected = true
	stop := make(chan struct{})
	c.healthStop = stop
	m.mu.Unlock()

	for _, e := range discoverErrs {
		m.emitError(c.id, "discover", e, false)
	}
	m.startHealth(c, gen, client, stop, server)
	events.Emit(m.d.bus, events.McpConnected, "", events.McpConnectedPayload{
		AgentID: m.d.agentID, ServerID: c.id, ServerName: server.Name, ProtocolVersion: init.ProtocolVersion,
		ServerInfo: init.ServerInfo, Capabilities: init.Capabilities,
		Tools: slices.Clone(tools), Resources: slices.Clone(resources), Prompts: slices.Clone(prompts),
		Reconnected: reconnected,
	})
}

// failTry is fail for the error of a connect try. A-214: refused (401 / 403) waits for a
// sign-in instead of retrying.
func (m *mcpManager) failTry(c *mcpConn, gen uint64, err error) {
	var me *mcpserver.Error
	if errors.As(err, &me) && me.Code == mcpserver.CodeUnauthorized {
		m.failAuth(c, gen, err.Error())
		return
	}
	m.fail(c, gen, err.Error(), !permanentMcpError(err))
}

// failAuth marks the server needs_auth: it is not retried; mcp-server:auth-changed
// reconnects it.
func (m *mcpManager) failAuth(c *mcpConn, gen uint64, msg string) {
	m.mu.Lock()
	if gen != c.gen || m.disposed || m.conns[c.id] != c {
		m.mu.Unlock()
		return
	}
	c.client = nil
	c.state.Error = msg
	c.state.Attempts++
	c.state.Status = mcpserver.StatusNeedsAuth
	m.mu.Unlock()
	events.Emit(m.d.bus, events.McpFailed, "", events.McpFailedPayload{AgentID: m.d.agentID, ServerID: c.id, Phase: "connect", Error: msg, WillRetry: false, NeedsAuth: true})
}

// fail marks a connect try failed and schedules the next one while tries are left.
func (m *mcpManager) fail(c *mcpConn, gen uint64, msg string, retry bool) {
	m.mu.Lock()
	if gen != c.gen || m.disposed || m.conns[c.id] != c {
		// A newer try or a close took over.
		m.mu.Unlock()
		return
	}
	c.client = nil
	c.state.Error = msg
	c.state.Attempts++
	maxN := m.policy.MaxAttempts
	willRetry := retry && m.started && !m.disposed && (maxN == 0 || c.state.Attempts < maxN)
	if willRetry {
		c.state.Status = mcpserver.StatusReconnecting
		m.scheduleRetryLocked(c)
	} else {
		c.state.Status = mcpserver.StatusFailed
	}
	m.mu.Unlock()
	m.emitError(c.id, "connect", msg, willRetry)
}

// scheduleRetryLocked arms the next try; the caller holds mu.
func (m *mcpManager) scheduleRetryLocked(c *mcpConn) {
	if c.retry != nil {
		c.retry.Stop()
	}
	delay := m.policy.BaseDelay
	for i := 1; i < c.state.Attempts && delay < m.policy.MaxDelay; i++ {
		delay *= 2
	}
	delay = min(delay, m.policy.MaxDelay)
	gen := c.gen
	c.retry = time.AfterFunc(delay, func() {
		m.goTracked(func(ctx context.Context) {
			m.mu.Lock()
			ok := c.gen == gen && !m.disposed && m.conns[c.id] == c
			m.mu.Unlock()
			if ok {
				// Read the definition again: the hub may have been unreachable, or the
				// server changed.
				m.open(ctx, c.id, c)
			}
		})
	})
}

// onLost is the connection ending without close (the process exited, the session
// expired, a ping failed).
func (m *mcpManager) onLost(c *mcpConn, gen uint64, cause error) {
	m.mu.Lock()
	if gen != c.gen || m.disposed || c.client == nil {
		// Still in the handshake: the connect try fails and schedules the retry itself.
		m.mu.Unlock()
		return
	}
	c.gen++
	m.stopTimersLocked(c)
	client := c.client
	c.client = nil
	reason := "the connection closed"
	if cause != nil {
		reason = cause.Error()
	}
	c.state.Error = reason
	c.state.Status = mcpserver.StatusReconnecting
	c.state.ConnectedAt = ""
	c.state.Attempts = 0
	m.scheduleRetryLocked(c)
	m.mu.Unlock()
	_ = client.Close()
	events.Emit(m.d.bus, events.McpDisconnected, "", events.McpDisconnectedPayload{AgentID: m.d.agentID, ServerID: c.id, Reason: reason, WillReconnect: true})
}

func (m *mcpManager) startHealth(c *mcpConn, gen uint64, client *mcpclient.Client, stop chan struct{}, server mcpserver.Server) {
	if m.health <= 0 {
		return
	}
	pingTimeout := min(time.Duration(server.RequestTimeoutMs())*time.Millisecond, 10*time.Second)
	m.goTracked(func(ctx context.Context) {
		ticker := time.NewTicker(m.health)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			if err := client.Ping(ctx, pingTimeout); err != nil {
				if ctx.Err() != nil {
					return
				}
				m.mu.Lock()
				current := c.gen == gen && !m.disposed
				m.mu.Unlock()
				if !current {
					return
				}
				m.emitError(c.id, "health", "ping failed: "+err.Error(), true)
				m.onLost(c, gen, errors.New("ping failed: "+err.Error()))
				return
			}
		}
	})
}

func (m *mcpManager) onNotification(ctx context.Context, c *mcpConn, gen uint64, n mcpclient.Message) {
	var kind string
	switch n.Method {
	case "notifications/tools/list_changed":
		kind = "tools"
	case "notifications/resources/list_changed":
		kind = "resources"
	case "notifications/prompts/list_changed":
		kind = "prompts"
	default:
		return
	}
	m.refresh(ctx, c, gen, kind)
}

// refresh re-reads one list after list_changed; a change arriving meanwhile reads it once
// more.
func (m *mcpManager) refresh(ctx context.Context, c *mcpConn, gen uint64, kind string) {
	m.mu.Lock()
	if r := c.refreshing[kind]; r != nil {
		r.dirty = true
		m.mu.Unlock()
		return
	}
	mark := &mcpRefresh{dirty: true}
	c.refreshing[kind] = mark
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		if c.refreshing[kind] == mark {
			delete(c.refreshing, kind)
		}
		m.mu.Unlock()
	}()
	for {
		m.mu.Lock()
		if !mark.dirty || c.gen != gen || c.client == nil {
			m.mu.Unlock()
			return
		}
		mark.dirty = false
		client := c.client
		timeout := time.Duration(c.server.RequestTimeoutMs()) * time.Millisecond
		m.mu.Unlock()
		var err error
		var tools []mcpserver.Tool
		var resources []mcpserver.Resource
		var prompts []mcpserver.Prompt
		switch kind {
		case "tools":
			tools, err = client.ListTools(ctx, timeout)
		case "resources":
			resources, err = client.ListResources(ctx, timeout)
		default:
			prompts, err = client.ListPrompts(ctx, timeout)
		}
		m.mu.Lock()
		current := c.gen == gen
		if err == nil && current {
			switch kind {
			case "tools":
				c.state.Tools = tools
			case "resources":
				c.state.Resources = resources
			default:
				c.state.Prompts = prompts
			}
		}
		m.mu.Unlock()
		if !current {
			return
		}
		if err != nil {
			m.emitError(c.id, "refresh", kind+"/list failed: "+err.Error(), false)
			return
		}
		switch kind {
		case "tools":
			events.Emit(m.d.bus, events.McpToolsChanged, "", events.McpToolsChangedPayload{AgentID: m.d.agentID, ServerID: c.id, Tools: slices.Clone(tools)})
		case "resources":
			events.Emit(m.d.bus, events.McpResourcesChanged, "", events.McpResourcesChangedPayload{AgentID: m.d.agentID, ServerID: c.id, Resources: slices.Clone(resources)})
		default:
			events.Emit(m.d.bus, events.McpPromptsChanged, "", events.McpPromptsChangedPayload{AgentID: m.d.agentID, ServerID: c.id, Prompts: slices.Clone(prompts)})
		}
	}
}

// onServerChanged is the mcp-server:changed subscriber: reconnect with the new
// definition, or close the server.
func (m *mcpManager) onServerChanged(ctx context.Context, serverID, change string) {
	m.mu.Lock()
	active := m.started && !m.disposed
	c := m.conns[serverID]
	m.mu.Unlock()
	if !active || !slices.Contains(m.wanted(), serverID) {
		return
	}
	if change == "deleted" {
		m.close(serverID, "closed")
		return
	}
	if c != nil {
		m.drop(c, "closed")
		m.mu.Lock()
		c.state.Attempts = 0
		m.mu.Unlock()
	}
	m.open(ctx, serverID, c)
}

// close closes a connection on purpose and forgets it.
func (m *mcpManager) close(serverID, reason string) {
	m.mu.Lock()
	c, ok := m.conns[serverID]
	if ok {
		delete(m.conns, serverID)
	}
	m.mu.Unlock()
	if ok {
		m.drop(c, reason)
	}
}

// drop stops a connection (timers, client); it announces mcp:disconnected when it was up.
func (m *mcpManager) drop(c *mcpConn, reason string) {
	m.mu.Lock()
	c.gen++
	m.stopTimersLocked(c)
	wasUp := c.state.Status == mcpserver.StatusConnected || c.state.Status == mcpserver.StatusReconnecting
	client := c.client
	c.client = nil
	c.state.ConnectedAt = ""
	m.mu.Unlock()
	if client != nil {
		_ = client.Close()
	}
	if wasUp {
		events.Emit(m.d.bus, events.McpDisconnected, "", events.McpDisconnectedPayload{AgentID: m.d.agentID, ServerID: c.id, Reason: reason, WillReconnect: false})
	}
}

func (m *mcpManager) stopTimersLocked(c *mcpConn) {
	if c.retry != nil {
		c.retry.Stop()
		c.retry = nil
	}
	if c.healthStop != nil {
		close(c.healthStop)
		c.healthStop = nil
	}
}

func (m *mcpManager) emitError(serverID, phase, msg string, willRetry bool) {
	events.Emit(m.d.bus, events.McpFailed, "", events.McpFailedPayload{AgentID: m.d.agentID, ServerID: serverID, Phase: phase, Error: msg, WillRetry: willRetry})
}

func (m *mcpManager) require(serverID string) (*mcpclient.Client, mcpserver.Server, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.conns[serverID]
	if !ok {
		return nil, mcpserver.Server{}, notMine(serverID)
	}
	if c.client == nil || c.state.Status != mcpserver.StatusConnected || c.server == nil {
		msg := `MCP server "` + serverID + `" is not connected (` + string(c.state.Status)
		if c.state.Error != "" {
			msg += ": " + c.state.Error
		}
		return nil, mcpserver.Server{}, mcpserver.NewError(msg+")", mcpserver.CodeNotConnected)
	}
	return c.client, *c.server, nil
}

// buildTransport builds the server's transport with {{secret:NAME}} in its headers / env
// filled in.
func (m *mcpManager) buildTransport(ctx context.Context, server mcpserver.Server) (mcpclient.Transport, error) {
	if m.d.hub == nil {
		return nil, mcpserver.NewError("this agent has no McpHub", mcpserver.CodeInvalidArgument)
	}
	t := server.Transport.Clone()
	var err error
	switch t.Type {
	case mcpserver.TransportHTTP:
		host := ""
		if u, perr := url.Parse(t.URL); perr == nil {
			host = u.Hostname()
		}
		t.Headers, err = m.fill(ctx, t.Headers, host)
	default:
		t.Env, err = m.fill(ctx, t.Env, "")
	}
	if err != nil {
		return nil, err
	}
	// A-214: the server's OAuth sign-in, unless it sends its own Authorization header.
	tc := mcpclient.TransportContext{}
	if t.Type == mcpserver.TransportHTTP && !server.Transport.HasOwnAuthorization() {
		tc.Auth = m.d.hub.OAuth().AuthFor(server)
	}
	return m.d.hub.NewTransport(t, tc)
}

func (m *mcpManager) fill(ctx context.Context, raw map[string]string, host string) (map[string]string, error) {
	// A-215: variables first, so a variable may hold a {{secret:NAME}} (the secret's own
	// rules still apply).
	values, err := m.fillVariables(ctx, raw)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, v := range values {
		for _, n := range secret.PlaceholderNames(v) {
			if !slices.Contains(names, n) {
				names = append(names, n)
			}
		}
	}
	if len(names) == 0 {
		return values, nil
	}
	if m.d.secrets == nil {
		return nil, mcpserver.NewError("this MCP server uses {{secret:...}} but the agent has no secrets", mcpserver.CodeInvalidArgument)
	}
	slices.Sort(names)
	resolved, err := m.d.secrets.Resolve(ctx, names, secret.Use{Tool: McpSecretTool, Host: host}, secret.ResolveContext{})
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(values))
	for k, v := range values {
		out[k] = secret.FillPlaceholders(v, resolved)
	}
	return out, nil
}

// fillVariables fills {{var:NAME}} in the values (A-215).
func (m *mcpManager) fillVariables(ctx context.Context, values map[string]string) (map[string]string, error) {
	var names []string
	for _, v := range values {
		for _, n := range variable.PlaceholderNames(v) {
			if !slices.Contains(names, n) {
				names = append(names, n)
			}
		}
	}
	if len(names) == 0 {
		return values, nil
	}
	if m.d.variables == nil {
		return nil, mcpserver.NewError("this MCP server uses {{var:...}} but the agent has no variables", mcpserver.CodeInvalidArgument)
	}
	slices.Sort(names)
	resolved, err := m.d.variables.Resolve(ctx, names)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(values))
	for k, v := range values {
		out[k] = variable.FillPlaceholders(v, resolved)
	}
	return out, nil
}

// permanentMcpError reports whether trying again cannot help: a bad definition, an
// unsupported protocol version or a missing secret (A-215: or variable).
func permanentMcpError(err error) bool {
	var me *mcpserver.Error
	if errors.As(err, &me) && (me.Code == mcpserver.CodeInvalidArgument || me.Code == mcpserver.CodeProtocol) {
		return true
	}
	var se *secret.Error
	if errors.As(err, &se) {
		return true
	}
	var ve *variable.Error
	return errors.As(err, &ve)
}

func notMine(serverID string) error {
	return mcpserver.NewError(`MCP server "`+serverID+`" is not one of this agent's servers`, mcpserver.CodeNotFound)
}

func timeoutOf(server mcpserver.Server, opts mcpserver.CallOptions) time.Duration {
	if opts.Timeout > 0 {
		return opts.Timeout
	}
	return time.Duration(server.RequestTimeoutMs()) * time.Millisecond
}
