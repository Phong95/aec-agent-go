package mcphub_test

import (
	"errors"
	"slices"
	"sync"
	"testing"

	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/components/mcphub"
	"github.com/Phong95/aec-agent-go/components/mcphub/mcpclient"
	"github.com/Phong95/aec-agent-go/components/mcphub/mcpserver"
	"github.com/Phong95/aec-agent-go/components/persistence"
	"github.com/Phong95/aec-agent-go/kernel"
)

func stdioTransport(cmd string) mcpserver.Transport {
	return mcpserver.Transport{Type: mcpserver.TransportStdio, Command: cmd, Args: []string{"x.js"}}
}

func TestHubRegistersCreatesAndAnnounces(t *testing.T) {
	bus := events.New()
	store := persistence.NewMemoryMcpServerStore()
	hub := mcphub.NewMcpHub(func() persistence.McpServerStore { return store }, mcphub.WithEvents(bus))
	app, err := hub.Register(mcpserver.Definition{ID: "files", Transport: stdioTransport("node")})
	if err != nil || app.Name != "files" || !app.Enabled || app.Source != mcpserver.SourceApp {
		t.Fatalf("register = %+v %v", app, err)
	}
	if _, err := hub.Register(mcpserver.Definition{ID: "files", Transport: stdioTransport("node")}); !errors.Is(err, &mcpserver.Error{Code: mcpserver.CodeAlreadyExists}) {
		t.Fatalf("duplicate register err = %v", err)
	}
	var mu sync.Mutex
	var changes []string
	unsub := events.Subscribe(bus, events.McpServerChanged, func(_ kernel.Event, p events.McpServerChangedPayload) {
		mu.Lock()
		changes = append(changes, p.ServerID+":"+p.Change)
		mu.Unlock()
	})
	defer unsub()
	requestMs := 5000
	created, err := hub.Create(t.Context(), mcpserver.Definition{
		ID: "remote", Name: "Remote",
		Transport: mcpserver.Transport{Type: mcpserver.TransportHTTP, URL: "https://mcp.example.com/mcp", Headers: map[string]string{"Authorization": "Bearer {{secret:TOKEN}}"}},
		Timeouts:  &mcpserver.Timeouts{RequestMs: &requestMs},
	})
	if err != nil || created.Source != mcpserver.SourceCustom || created.RequestTimeoutMs() != 5000 || created.CreatedAt == "" {
		t.Fatalf("create = %+v %v", created, err)
	}
	stored, _ := store.List(t.Context())
	if len(stored) != 1 || stored[0].ID != "remote" {
		t.Fatalf("store = %+v", stored)
	}
	off := false
	if _, err := hub.Update(t.Context(), "remote", mcpserver.Definition{Transport: created.Transport, Enabled: &off}); err != nil {
		t.Fatal(err)
	}
	if s, ok, _ := hub.Get(t.Context(), "remote"); !ok || s.Enabled {
		t.Fatalf("updated = %+v %v", s, ok)
	}
	if _, err := hub.Update(t.Context(), "files", mcpserver.Definition{Transport: app.Transport}); !errors.Is(err, &mcpserver.Error{Code: mcpserver.CodeAlreadyExists}) {
		t.Fatalf("update app err = %v", err)
	}
	list, _ := hub.List(t.Context())
	var ids []string
	for _, s := range list {
		ids = append(ids, s.ID)
	}
	if !slices.Equal(ids, []string{"files", "remote"}) {
		t.Fatalf("list = %v", ids)
	}
	if err := hub.Delete(t.Context(), "remote"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := hub.Get(t.Context(), "remote"); ok {
		t.Fatal("deleted server still there")
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(changes, []string{"remote:created", "remote:updated", "remote:deleted"}) {
		t.Fatalf("changes = %v", changes)
	}
}

func TestHubRejectsInvalidDefinitions(t *testing.T) {
	hub := mcphub.NewMcpHub(func() persistence.McpServerStore { return persistence.NewMemoryMcpServerStore() })
	neg := -1
	bad := []mcpserver.Definition{
		{ID: "Bad Id", Transport: stdioTransport("x")},
		{ID: "x", Transport: stdioTransport(" ")},
		{ID: "x", Transport: mcpserver.Transport{Type: mcpserver.TransportHTTP, URL: "ftp://host"}},
		{ID: "x", Transport: mcpserver.Transport{Type: "websocket", URL: "ws://host"}},
		{ID: "x", Transport: mcpserver.Transport{Type: mcpserver.TransportStdio, Command: "x", Env: map[string]string{"BAD-NAME": "1"}}},
		{ID: "x", Transport: stdioTransport("x"), Timeouts: &mcpserver.Timeouts{RequestMs: &neg}},
	}
	for _, d := range bad {
		if _, err := hub.Register(d); !errors.Is(err, &mcpserver.Error{Code: mcpserver.CodeInvalidArgument}) {
			t.Errorf("%+v: err = %v", d, err)
		}
	}
}

func TestHubTransports(t *testing.T) {
	hub := mcphub.NewMcpHub(func() persistence.McpServerStore { return persistence.NewMemoryMcpServerStore() })
	if _, err := hub.NewTransport(stdioTransport("node"), mcpclient.TransportContext{}); !errors.Is(err, &mcpserver.Error{Code: mcpserver.CodeInvalidArgument}) {
		t.Fatalf("stdio without a registered transport err = %v", err)
	}
	if tr, err := hub.NewTransport(mcpserver.Transport{Type: mcpserver.TransportHTTP, URL: "http://127.0.0.1:1/mcp"}, mcpclient.TransportContext{}); err != nil || tr == nil {
		t.Fatalf("http transport = %v %v", tr, err)
	}
}
