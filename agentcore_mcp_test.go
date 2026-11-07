package aec_test

import (
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/Phong95/aec-agent-go/aecmock"
	"github.com/Phong95/aec-agent-go/components/agent/profile"
	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/components/mcphub/mcpserver"
	"github.com/Phong95/aec-agent-go/internal/mcptest"
	"github.com/Phong95/aec-agent-go/kernel"
)

// A-208: AgentCore wiring of MCP servers - UseMcp and custom servers in the McpHub,
// StartMcp (also for agents spawned later), and a deleted server dropped from every
// agent's profile. The connection behaviour is tested in components/agent.

func mcpProfile(id string, servers ...string) profile.AgentProfile {
	return profile.AgentProfile{ID: id, Name: id, SystemPrompt: "You are " + id + ".", Mcp: &mcpserver.Config{Servers: servers}}
}

func TestAgentCore_McpWiring(t *testing.T) {
	fake := mcptest.NewHTTP()
	srv := httptest.NewServer(fake)
	t.Cleanup(func() {
		fake.Close()
		srv.CloseClientConnections()
		srv.Close()
	})
	core := subagentCore(t)
	if err := core.UseLLMProvider(aecmock.NewMockProvider("primary", func([]kernel.Message) string { return "y" })); err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	httpDef := func(id string) mcpserver.Definition {
		return mcpserver.Definition{ID: id, Transport: mcpserver.Transport{Type: mcpserver.TransportHTTP, URL: srv.URL + "/mcp"}}
	}
	if _, err := core.UseMcp(httpDef("app-fake")); err != nil {
		t.Fatal(err)
	}
	if _, err := core.McpHub().Create(ctx, httpDef("custom-fake")); err != nil {
		t.Fatal(err)
	}
	a1, err := core.SpawnAgent(ctx, mcpProfile("a1", "app-fake", "custom-fake"))
	if err != nil {
		t.Fatal(err)
	}
	if err := core.StartMcp(ctx); err != nil {
		t.Fatal(err)
	}
	var got [][2]string
	for _, s := range a1.Mcp().List() {
		got = append(got, [2]string{s.ServerID, string(s.Status)})
	}
	if want := [][2]string{{"app-fake", "connected"}, {"custom-fake", "connected"}}; !slices.Equal(got, want) {
		t.Fatalf("a1 servers = %v, want %v", got, want)
	}

	// Spawned after StartMcp: connects too (SpawnAgent returns once it did).
	a2, err := core.SpawnAgent(ctx, mcpProfile("a2", "app-fake"))
	if err != nil {
		t.Fatal(err)
	}
	if st, ok := a2.Mcp().Get("app-fake"); !ok || st.Status != mcpserver.StatusConnected {
		t.Fatalf("a2 state = %+v %v", st, ok)
	}

	closed := make(chan events.McpDisconnectedPayload, 4)
	unsub := events.Subscribe(core.Events(), events.McpDisconnected, func(_ kernel.Event, p events.McpDisconnectedPayload) {
		if p.ServerID == "custom-fake" {
			closed <- p
		}
	})
	defer unsub()
	if err := core.McpHub().Delete(ctx, "custom-fake"); err != nil {
		t.Fatal(err)
	}
	select {
	case p := <-closed:
		if p.AgentID != "a1" || p.WillReconnect {
			t.Fatalf("disconnected = %+v", p)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("custom-fake was not closed")
	}
	if p := a1.Profile(); p.Mcp == nil || !slices.Equal(p.Mcp.Servers, []string{"app-fake"}) {
		t.Fatalf("a1 profile mcp = %+v", p.Mcp)
	}
	stored, err := core.PersistenceHub().Agents.Load(ctx, "a1")
	if err != nil || stored == nil || stored.Mcp == nil || !slices.Equal(stored.Mcp.Servers, []string{"app-fake"}) {
		t.Fatalf("stored a1 = %+v %v", stored, err)
	}
}
