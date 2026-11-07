package agent_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Phong95/aec-agent-go/components/agent"
	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/components/mcphub"
	"github.com/Phong95/aec-agent-go/components/mcphub/mcpserver"
	"github.com/Phong95/aec-agent-go/components/mcphub/stdio"
	"github.com/Phong95/aec-agent-go/components/persistence"
	"github.com/Phong95/aec-agent-go/components/secrethub"
	"github.com/Phong95/aec-agent-go/components/secrethub/secret"
	"github.com/Phong95/aec-agent-go/internal/mcptest"
	"github.com/Phong95/aec-agent-go/kernel"
)

// ---------------------------------------------------------------------------
// A-208: each agent's MCP manager connecting to a fake MCP server over stdio (this test
// binary as a helper process, see TestMain) and streamable HTTP: connect, discover,
// list_changed, reconnect, health checks, secrets in env / headers, and dispose. The Go
// form of core/ts tests/unit/mcp.test.ts.
// ---------------------------------------------------------------------------

func stdioDef(id string, env map[string]string) mcpserver.Definition {
	all := map[string]string{mcptest.HelperEnv: "stdio"}
	for k, v := range env {
		all[k] = v
	}
	return mcpserver.Definition{ID: id, Transport: mcpserver.Transport{Type: mcpserver.TransportStdio, Command: os.Args[0], Env: all}}
}

type mcpRig struct {
	env     *env
	hub     *mcphub.McpHub
	secrets *secrethub.SecretHub
}

func newMcpRig(t *testing.T, defs ...mcpserver.Definition) *mcpRig {
	t.Helper()
	e := newEnv(t)
	r := &mcpRig{env: e}
	r.hub = mcphub.NewMcpHub(func() persistence.McpServerStore { return e.hub.McpServers }, mcphub.WithEvents(e.bus))
	r.hub.RegisterTransport(mcpserver.TransportStdio, stdio.Factory)
	r.secrets = secrethub.NewSecretHub(func() persistence.SecretStore { return e.hub.Secrets })
	for _, d := range defs {
		if _, err := r.hub.Register(d); err != nil {
			t.Fatalf("register %s: %v", d.ID, err)
		}
	}
	return r
}

func (r *mcpRig) spawn(t *testing.T, id string, servers []string, opts ...agent.Option) *agent.Agent {
	t.Helper()
	p := testProfile(id)
	p.Mcp = &mcpserver.Config{Servers: servers}
	deps := r.env.deps()
	deps.McpHub = r.hub
	deps.SecretHub = r.secrets
	opts = append([]agent.Option{
		agent.WithMcpReconnectPolicy(agent.McpReconnectPolicy{BaseDelay: 20 * time.Millisecond, MaxDelay: 100 * time.Millisecond, MaxAttempts: 3}),
		agent.WithMcpHealthCheckInterval(0),
	}, opts...)
	a, err := agent.New(t.Context(), deps, p, opts...)
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	t.Cleanup(func() { _ = a.Dispose(context.WithoutCancel(t.Context())) })
	return a
}

// waitKind returns a channel that receives the first payload of kind k that match accepts.
func waitKind[P any](t *testing.T, bus events.Bus, k events.Kind[P], match func(P) bool) <-chan P {
	t.Helper()
	ch := make(chan P, 1)
	var once sync.Once
	unsub := events.Subscribe(bus, k, func(_ kernel.Event, p P) {
		if match != nil && !match(p) {
			return
		}
		once.Do(func() { ch <- p })
	})
	t.Cleanup(func() { unsub() })
	return ch
}

func recv[P any](t *testing.T, ch <-chan P, what string) P {
	t.Helper()
	select {
	case p := <-ch:
		return p
	case <-time.After(15 * time.Second):
		t.Fatalf("no %s within 15 s", what)
		var zero P
		return zero
	}
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	end := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(end) {
			t.Fatalf("%s: not met in time", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func textOf(t *testing.T, res mcpserver.CallToolResult) string {
	t.Helper()
	if len(res.Content) == 0 {
		t.Fatalf("no content: %+v", res)
	}
	s, _ := res.Content[0]["text"].(string)
	return s
}

func TestMcpStdioConnectDiscoverAndCall(t *testing.T) {
	r := newMcpRig(t, stdioDef("fake", nil))
	a := r.spawn(t, "a1", []string{"fake"})
	connected := waitKind(t, r.env.bus, events.McpConnected, nil)
	if err := a.StartMcp(t.Context()); err != nil {
		t.Fatal(err)
	}
	ev := recv(t, connected, "mcp:connected")
	if ev.AgentID != "a1" || ev.ServerID != "fake" || ev.ProtocolVersion != "2025-06-18" || ev.Reconnected {
		t.Fatalf("connected event = %+v", ev)
	}
	st, ok := a.Mcp().Get("fake")
	if !ok || st.Status != mcpserver.StatusConnected || st.ServerInfo.Name != "fake-mcp" || st.Instructions != "Use echo to test." {
		t.Fatalf("state = %+v", st)
	}
	var names []string
	for _, tl := range st.Tools {
		names = append(names, tl.Name)
	}
	// tools/list is paged one tool per page: every page was followed.
	if !slices.Equal(names, []string{"echo", "add", "add_tool", "crash", "env", "slow"}) {
		t.Fatalf("tools = %v", names)
	}
	if st.Tools[0].Annotations == nil || st.Tools[0].Annotations.ReadOnlyHint == nil || !*st.Tools[0].Annotations.ReadOnlyHint {
		t.Fatalf("annotations lost: %+v", st.Tools[0])
	}
	if len(st.Resources) != 1 || st.Resources[0].URI != "file:///readme.md" || len(st.Prompts) != 1 || st.Prompts[0].Name != "greet" {
		t.Fatalf("resources / prompts = %+v %+v", st.Resources, st.Prompts)
	}
	ctx := t.Context()
	res, err := a.Mcp().CallTool(ctx, "fake", "echo", map[string]any{"text": "hi"}, mcpserver.CallOptions{})
	if err != nil || textOf(t, res) != "hi" {
		t.Fatalf("echo = %+v %v", res, err)
	}
	res, err = a.Mcp().CallTool(ctx, "fake", "add", map[string]any{"a": 2, "b": 3}, mcpserver.CallOptions{})
	if err != nil || res.StructuredContent["sum"] != float64(5) {
		t.Fatalf("add = %+v %v", res, err)
	}
	rr, err := a.Mcp().ReadResource(ctx, "fake", "file:///readme.md", mcpserver.CallOptions{})
	if err != nil || len(rr.Contents) != 1 || rr.Contents[0].Text != "# Readme" {
		t.Fatalf("read = %+v %v", rr, err)
	}
	gp, err := a.Mcp().GetPrompt(ctx, "fake", "greet", map[string]string{"who": "Ann"}, mcpserver.CallOptions{})
	if err != nil || len(gp.Messages) != 1 || gp.Messages[0].Content["text"] != "Hello Ann" {
		t.Fatalf("prompt = %+v %v", gp, err)
	}
	if err := a.Mcp().Ping(ctx, "fake", mcpserver.CallOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Mcp().CallTool(ctx, "nope", "echo", nil, mcpserver.CallOptions{}); !errors.Is(err, &mcpserver.Error{Code: mcpserver.CodeNotFound}) {
		t.Fatalf("unknown server err = %v", err)
	}
}

func TestMcpRequestTimeout(t *testing.T) {
	r := newMcpRig(t, stdioDef("fake", nil))
	a := r.spawn(t, "a1", []string{"fake"})
	if err := a.StartMcp(t.Context()); err != nil {
		t.Fatal(err)
	}
	_, err := a.Mcp().CallTool(t.Context(), "fake", "slow", map[string]any{"ms": 2000}, mcpserver.CallOptions{Timeout: 100 * time.Millisecond})
	if !errors.Is(err, &mcpserver.Error{Code: mcpserver.CodeTimeout}) {
		t.Fatalf("slow err = %v", err)
	}
	// The connection is still usable.
	res, err := a.Mcp().CallTool(t.Context(), "fake", "echo", map[string]any{"text": "ok"}, mcpserver.CallOptions{})
	if err != nil || textOf(t, res) != "ok" {
		t.Fatalf("echo after timeout = %+v %v", res, err)
	}
}

func TestMcpListChanged(t *testing.T) {
	r := newMcpRig(t, stdioDef("fake", nil))
	a := r.spawn(t, "a1", []string{"fake"})
	if err := a.StartMcp(t.Context()); err != nil {
		t.Fatal(err)
	}
	changed := waitKind(t, r.env.bus, events.McpToolsChanged, nil)
	if _, err := a.Mcp().CallTool(t.Context(), "fake", "add_tool", map[string]any{"name": "brand_new"}, mcpserver.CallOptions{}); err != nil {
		t.Fatal(err)
	}
	ev := recv(t, changed, "mcp:tools-changed")
	if !slices.ContainsFunc(ev.Tools, func(tl mcpserver.Tool) bool { return tl.Name == "brand_new" }) {
		t.Fatalf("tools-changed = %+v", ev.Tools)
	}
	st, _ := a.Mcp().Get("fake")
	if !slices.ContainsFunc(st.Tools, func(tl mcpserver.Tool) bool { return tl.Name == "brand_new" }) {
		t.Fatalf("state tools = %+v", st.Tools)
	}
}

func TestMcpReconnectAfterTheProcessDies(t *testing.T) {
	r := newMcpRig(t, stdioDef("fake", nil))
	a := r.spawn(t, "a1", []string{"fake"})
	if err := a.StartMcp(t.Context()); err != nil {
		t.Fatal(err)
	}
	lost := waitKind(t, r.env.bus, events.McpDisconnected, nil)
	back := waitKind(t, r.env.bus, events.McpConnected, func(p events.McpConnectedPayload) bool { return p.Reconnected })
	if _, err := a.Mcp().CallTool(t.Context(), "fake", "crash", nil, mcpserver.CallOptions{}); err != nil {
		t.Fatal(err)
	}
	if ev := recv(t, lost, "mcp:disconnected"); ev.ServerID != "fake" || !ev.WillReconnect {
		t.Fatalf("disconnected = %+v", ev)
	}
	recv(t, back, "reconnected mcp:connected")
	res, err := a.Mcp().CallTool(t.Context(), "fake", "echo", map[string]any{"text": "again"}, mcpserver.CallOptions{})
	if err != nil || textOf(t, res) != "again" {
		t.Fatalf("echo after reconnect = %+v %v", res, err)
	}
}

func TestMcpSecretsInEnvAndNoOtherHostVariables(t *testing.T) {
	t.Setenv("AEC_TEST_HOST_ONLY", "leak")
	def := stdioDef("fake", map[string]string{"TOKEN": "tok-{{secret:MY_TOKEN}}"})
	r := newMcpRig(t, def)
	if _, err := r.secrets.Create(t.Context(), secrethub.CreateInput{
		Definition: secret.Definition{Name: "MY_TOKEN", AllowedTools: []string{agent.McpSecretTool}}, Value: "s3cret", OwnerAgentID: "a1",
	}); err != nil {
		t.Fatal(err)
	}
	a := r.spawn(t, "a1", []string{"fake"})
	if err := a.StartMcp(t.Context()); err != nil {
		t.Fatal(err)
	}
	res, err := a.Mcp().CallTool(t.Context(), "fake", "env", map[string]any{"name": "TOKEN"}, mcpserver.CallOptions{})
	if err != nil || textOf(t, res) != "tok-s3cret" {
		t.Fatalf("TOKEN = %+v %v", res, err)
	}
	res, err = a.Mcp().CallTool(t.Context(), "fake", "env", map[string]any{"name": "AEC_TEST_HOST_ONLY"}, mcpserver.CallOptions{})
	if err != nil || textOf(t, res) != "" {
		t.Fatalf("host variable leaked: %+v %v", res, err)
	}
}

func TestMcpUnknownDisabledAndIncompatibleServers(t *testing.T) {
	off := stdioDef("off", nil)
	off.Enabled = boolPtr(false)
	r := newMcpRig(t, off, stdioDef("old", map[string]string{mcptest.FakeVersionEnv: "1999-01-01"}))
	var mu sync.Mutex
	var failures []events.McpFailedPayload
	unsub := events.Subscribe(r.env.bus, events.McpFailed, func(_ kernel.Event, p events.McpFailedPayload) {
		mu.Lock()
		failures = append(failures, p)
		mu.Unlock()
	})
	defer unsub()
	a := r.spawn(t, "a1", []string{"missing", "off", "old"})
	if err := a.StartMcp(t.Context()); err != nil {
		t.Fatal(err)
	}
	var got [][2]string
	for _, s := range a.Mcp().List() {
		got = append(got, [2]string{s.ServerID, string(s.Status)})
	}
	want := [][2]string{{"missing", "failed"}, {"off", "disabled"}, {"old", "failed"}}
	if !slices.Equal(got, want) {
		t.Fatalf("statuses = %v, want %v", got, want)
	}
	if st, _ := a.Mcp().Get("old"); !strings.Contains(st.Error, "protocol version 1999-01-01") {
		t.Fatalf("old error = %q", st.Error)
	}
	mu.Lock()
	for _, f := range failures {
		if f.WillRetry {
			t.Errorf("retry for %+v", f)
		}
	}
	mu.Unlock()
	if _, err := a.Mcp().CallTool(t.Context(), "off", "echo", nil, mcpserver.CallOptions{}); !errors.Is(err, &mcpserver.Error{Code: mcpserver.CodeNotConnected}) {
		t.Fatalf("disabled call err = %v", err)
	}
}

func TestMcpGivesUpThenReconnects(t *testing.T) {
	r := newMcpRig(t, mcpserver.Definition{ID: "broken", Transport: mcpserver.Transport{Type: mcpserver.TransportStdio, Command: "aec-no-such-command-208"}})
	a := r.spawn(t, "a1", []string{"broken"})
	if err := a.StartMcp(t.Context()); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "failed", func() bool { st, _ := a.Mcp().Get("broken"); return st.Status == mcpserver.StatusFailed })
	if st, _ := a.Mcp().Get("broken"); st.Attempts != 3 {
		t.Fatalf("attempts = %d, want 3", st.Attempts)
	}
	st, err := a.Mcp().Reconnect(t.Context(), "broken")
	if err != nil || st.Attempts != 1 {
		t.Fatalf("reconnect = %+v %v", st, err)
	}
}

func TestMcpFollowsProfileChangesAndClosesOnDispose(t *testing.T) {
	r := newMcpRig(t, stdioDef("a", nil), stdioDef("b", nil))
	ag := r.spawn(t, "a1", []string{"a"})
	if err := ag.StartMcp(t.Context()); err != nil {
		t.Fatal(err)
	}
	closedA := waitKind(t, r.env.bus, events.McpDisconnected, func(p events.McpDisconnectedPayload) bool { return p.ServerID == "a" })
	if err := ag.UpdateMcp(t.Context(), &mcpserver.Config{Servers: []string{"b"}}); err != nil {
		t.Fatal(err)
	}
	if ev := recv(t, closedA, "a closed"); ev.WillReconnect {
		t.Fatalf("closed a = %+v", ev)
	}
	list := ag.Mcp().List()
	if len(list) != 1 || list[0].ServerID != "b" || list[0].Status != mcpserver.StatusConnected {
		t.Fatalf("list = %+v", list)
	}
	closedB := waitKind(t, r.env.bus, events.McpDisconnected, func(p events.McpDisconnectedPayload) bool { return p.ServerID == "b" })
	if err := ag.Dispose(t.Context()); err != nil {
		t.Fatal(err)
	}
	if ev := recv(t, closedB, "b closed"); ev.WillReconnect {
		t.Fatalf("closed b = %+v", ev)
	}
}

func TestMcpHTTPSecretsListChangedSessionLossAndDelete(t *testing.T) {
	fake := mcptest.NewHTTP()
	srv := httptest.NewServer(fake)
	t.Cleanup(func() {
		fake.Close()
		srv.CloseClientConnections()
		srv.Close()
	})
	r := newMcpRig(t, mcpserver.Definition{ID: "web", Transport: mcpserver.Transport{Type: mcpserver.TransportHTTP, URL: srv.URL + "/mcp", Headers: map[string]string{"Authorization": "Bearer {{secret:TOKEN}}"}}})
	if _, err := r.secrets.Create(t.Context(), secrethub.CreateInput{
		Definition: secret.Definition{Name: "TOKEN", AllowedTools: []string{agent.McpSecretTool}, AllowedHosts: []string{"127.0.0.1"}}, Value: "abc", OwnerAgentID: "a1",
	}); err != nil {
		t.Fatal(err)
	}
	a := r.spawn(t, "a1", []string{"web"})
	if err := a.StartMcp(t.Context()); err != nil {
		t.Fatal(err)
	}
	if st, _ := a.Mcp().Get("web"); st.Status != mcpserver.StatusConnected {
		t.Fatalf("state = %+v", st)
	}
	if got := fake.Auth(); !slices.Equal(got, []string{"Bearer abc"}) {
		t.Fatalf("auth = %v", got)
	}
	res, err := a.Mcp().CallTool(t.Context(), "web", "echo", map[string]any{"text": "over http"}, mcpserver.CallOptions{})
	if err != nil || textOf(t, res) != "over http" {
		t.Fatalf("echo = %+v %v", res, err)
	}
	// An SSE answer to a POST works too.
	fake.SetSSE(true)
	res, err = a.Mcp().CallTool(t.Context(), "web", "add", map[string]any{"a": 1, "b": 1}, mcpserver.CallOptions{})
	if err != nil || textOf(t, res) != "2" {
		t.Fatalf("add over sse = %+v %v", res, err)
	}
	waitUntil(t, "GET stream open", func() bool { return fake.Streams() > 0 })
	changed := waitKind(t, r.env.bus, events.McpToolsChanged, nil)
	if _, err := a.Mcp().CallTool(t.Context(), "web", "add_tool", map[string]any{"name": "from_http"}, mcpserver.CallOptions{}); err != nil {
		t.Fatal(err)
	}
	ev := recv(t, changed, "tools-changed over http")
	if !slices.ContainsFunc(ev.Tools, func(tl mcpserver.Tool) bool { return tl.Name == "from_http" }) {
		t.Fatalf("tools = %+v", ev.Tools)
	}
	// The server forgets the session: the manager notices (the next request or the GET
	// stream gets 404) and reconnects with a new one.
	back := waitKind(t, r.env.bus, events.McpConnected, func(p events.McpConnectedPayload) bool { return p.Reconnected })
	fake.ForgetSessions()
	_ = a.Mcp().Ping(t.Context(), "web", mcpserver.CallOptions{})
	recv(t, back, "reconnected over http")
	if n := fake.Sessions(); n != 1 {
		t.Fatalf("sessions = %d, want 1", n)
	}
	if err := a.Dispose(t.Context()); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "session deleted", func() bool { return len(fake.Deleted()) == 1 })
}

func TestMcpHealthPingFailureIsALostConnection(t *testing.T) {
	fake := mcptest.NewHTTP()
	srv := httptest.NewServer(fake)
	t.Cleanup(func() {
		fake.Close()
		srv.CloseClientConnections()
		srv.Close()
	})
	r := newMcpRig(t, mcpserver.Definition{ID: "web", Transport: mcpserver.Transport{Type: mcpserver.TransportHTTP, URL: srv.URL + "/mcp"}})
	a := r.spawn(t, "a1", []string{"web"}, agent.WithMcpHealthCheckInterval(50*time.Millisecond),
		// Never give up: the server stays down for a while.
		agent.WithMcpReconnectPolicy(agent.McpReconnectPolicy{BaseDelay: 20 * time.Millisecond, MaxDelay: 100 * time.Millisecond}))
	if err := a.StartMcp(t.Context()); err != nil {
		t.Fatal(err)
	}
	health := waitKind(t, r.env.bus, events.McpFailed, func(p events.McpFailedPayload) bool { return p.Phase == "health" })
	lost := waitKind(t, r.env.bus, events.McpDisconnected, nil)
	fake.SetDown(true)
	recv(t, health, "health failure")
	if ev := recv(t, lost, "disconnected"); !ev.WillReconnect {
		t.Fatalf("lost = %+v", ev)
	}
	back := waitKind(t, r.env.bus, events.McpConnected, func(p events.McpConnectedPayload) bool { return p.Reconnected })
	fake.SetDown(false)
	recv(t, back, "reconnected after health failure")
}
