package agent_test

import (
	"context"
	"errors"
	"testing"

	"github.com/Phong95/aec-agent-go/aecmock"
	"github.com/Phong95/aec-agent-go/components/agent"
	"github.com/Phong95/aec-agent-go/components/agent/profile"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/kernel"
)

// ToolManager reads its allowlist from the agent profile's Tools and resolves the
// tools lazily from the hub. The five subtests of TestToolManager carry the titles
// of the shared tool manager conformance cases.

func profileWithTools(tools ...string) profile.AgentProfile {
	return profile.AgentProfile{ID: "agent-a", Name: "a", SystemPrompt: "t", Tools: tools, Skills: []string{}}
}

func mustRegister(t *testing.T, hub *toolhub.ToolHub, handlers ...toolhub.Handler) {
	t.Helper()
	for _, h := range handlers {
		if err := hub.Register(h); err != nil {
			t.Fatalf("register %s: %v", h.Name(), err)
		}
	}
}

func TestToolManager(t *testing.T) {
	t.Run("should resolve tools registered AFTER ToolManager construction", func(t *testing.T) {
		hub := toolhub.NewToolHub()
		// Create the manager before any tool is registered (simulates setup ordering).
		manager := agent.NewToolManager(hub, profileWithTools("schedule_lookup", "budget_query"))

		// Tools not yet registered: the list is empty.
		if n := len(manager.List()); n != 0 {
			t.Fatalf("List() = %d tools before registration, want 0", n)
		}
		if manager.Has("schedule_lookup") {
			t.Fatal("Has(schedule_lookup) before registration")
		}

		// Register tools after the manager was built.
		mustRegister(t, hub, &aecmock.ScheduleLookupTool{}, &aecmock.BudgetQueryTool{})

		if n := len(manager.List()); n != 2 {
			t.Fatalf("List() = %d tools after registration, want 2", n)
		}
		if !manager.Has("schedule_lookup") || !manager.Has("budget_query") {
			t.Fatal("the newly registered tools are not visible")
		}
	})

	t.Run("should only return tools in the allowlist", func(t *testing.T) {
		hub := toolhub.NewToolHub()
		mustRegister(t, hub, &aecmock.ScheduleLookupTool{}, &aecmock.BudgetQueryTool{}, &aecmock.DocumentSearchTool{})

		manager := agent.NewToolManager(hub, profileWithTools("schedule_lookup"))
		if n := len(manager.List()); n != 1 {
			t.Fatalf("List() = %d tools, want 1", n)
		}
		if !manager.Has("schedule_lookup") || manager.Has("budget_query") || manager.Has("document_search") {
			t.Fatal("only schedule_lookup is allowed")
		}

		// A name outside the allowlist cannot be authorized, whatever the hub holds.
		_, _, err := manager.Authorize(context.Background(), "budget_query", kernel.Arguments(`{}`), toolhub.Context{})
		if err == nil || !errors.Is(err, toolhub.ErrNotFound) {
			t.Fatalf("Authorize(budget_query) error = %v, want a match on toolhub.ErrNotFound", err)
		}
		if want := `Tool "budget_query" not available for this agent`; err.Error() != want {
			t.Fatalf("error text = %q, want %q", err.Error(), want)
		}
	})

	t.Run("should reflect allowlist changes via setAllowedTools()", func(t *testing.T) {
		hub := toolhub.NewToolHub()
		mustRegister(t, hub, &aecmock.ScheduleLookupTool{}, &aecmock.BudgetQueryTool{}, &aecmock.DocumentSearchTool{})

		manager := agent.NewToolManager(hub, profileWithTools("schedule_lookup"))
		if n := len(manager.List()); n != 1 {
			t.Fatalf("List() = %d tools, want 1", n)
		}

		// Simulates PATCH /agents/:id { tools: [...] }.
		manager.SetAllowedTools([]string{"schedule_lookup", "budget_query", "document_search"})
		if n := len(manager.List()); n != 3 {
			t.Fatalf("List() = %d tools after SetAllowedTools, want 3", n)
		}
		if !manager.Has("document_search") {
			t.Fatal("document_search is not visible after SetAllowedTools")
		}
	})

	t.Run("should return empty list when allowlist is empty", func(t *testing.T) {
		hub := toolhub.NewToolHub()
		mustRegister(t, hub, &aecmock.ScheduleLookupTool{})

		manager := agent.NewToolManager(hub, profileWithTools())
		if n := len(manager.List()); n != 0 {
			t.Fatalf("List() = %d tools for an empty allowlist, want 0", n)
		}
		if manager.Has("schedule_lookup") {
			t.Fatal("an empty allowlist must not find a registered tool")
		}
		// The hub itself reads an empty allowlist as allow-all; the manager must not
		// let a call through.
		if _, _, err := manager.Authorize(context.Background(), "schedule_lookup", kernel.Arguments(`{"projectId":"p"}`), toolhub.Context{}); !errors.Is(err, toolhub.ErrNotFound) {
			t.Fatalf("Authorize with an empty allowlist = %v, want toolhub.ErrNotFound", err)
		}
	})

	t.Run("should ignore allowlist names that don't match any registered tool", func(t *testing.T) {
		hub := toolhub.NewToolHub()
		mustRegister(t, hub, &aecmock.ScheduleLookupTool{})

		manager := agent.NewToolManager(hub, profileWithTools("schedule_lookup", "nonexistent_tool"))
		if n := len(manager.List()); n != 1 {
			t.Fatalf("List() = %d tools, want 1", n)
		}
		if manager.Has("nonexistent_tool") {
			t.Fatal("an unregistered name must not be found")
		}
	})
}

// The allowlist is copied: changing the slice a caller handed in changes nothing.
func TestToolManager_AllowlistIsCopied(t *testing.T) {
	hub := toolhub.NewToolHub()
	mustRegister(t, hub, &aecmock.ScheduleLookupTool{}, &aecmock.BudgetQueryTool{})
	names := []string{"schedule_lookup"}
	manager := agent.NewToolManager(hub, profile.AgentProfile{Tools: names})
	names[0] = "budget_query"
	if !manager.Has("schedule_lookup") || manager.Has("budget_query") {
		t.Fatal("the manager aliases the caller's slice")
	}
	manager.SetAllowedTools(names)
	names[0] = "schedule_lookup"
	if !manager.Has("budget_query") || manager.Has("schedule_lookup") {
		t.Fatal("SetAllowedTools aliases the caller's slice")
	}
}
