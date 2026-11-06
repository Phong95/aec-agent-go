package agentloop_test

import (
	"context"
	"strings"
	"testing"

	"github.com/Phong95/aec-agent-go/aecmock"
	"github.com/Phong95/aec-agent-go/components/agent/memorysystem/memory"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/kernel"
)

// fakeMemoryTools is a Memory that also offers a memory_search tool (MemoryTools). It
// counts per-turn recalls, which must not happen any more.
type fakeMemoryTools struct {
	turnRecalls int
	search      *fakeTool
}

func (f *fakeMemoryTools) RecallForTurn(context.Context, string, string) (memory.RecalledMemories, error) {
	f.turnRecalls++
	c := "## Recalled memories\n- [fact] tone: brief"
	return memory.RecalledMemories{Content: &c, RecordIDs: []string{"m1"}, Memories: []memory.RecalledMemoryTrace{}}, nil
}

func (f *fakeMemoryTools) Tool(name string) (toolhub.Handler, bool) {
	if name == f.search.def.Name {
		return f.search, true
	}
	return nil, false
}

func (f *fakeMemoryTools) ToolDefinitions(context.Context) ([]toolhub.Definition, error) {
	return []toolhub.Definition{f.search.def}, nil
}

// TestMemoryRecallIsATool: the loop recalls no memories into the turn, offers the memory
// system's memory_search to the model, and runs a call of it directly (it is not on the
// agent's tool list, so the policy would refuse it), with the session id.
func TestMemoryRecallIsATool(t *testing.T) {
	var gotSession string
	mem := &fakeMemoryTools{search: &fakeTool{
		def: toolhub.Definition{
			Name: "memory_search", Description: "search memory", Risk: toolhub.RiskReadOnly,
			Parameters: toolhub.JSONSchema{Type: "object", Properties: toolhub.NewProperties(), Required: []string{}},
		},
		run: func(_ context.Context, _ kernel.Arguments, tc toolhub.Context) (toolhub.Output, error) {
			gotSession = tc.SessionID
			return toolhub.Output{Content: "- [fact] tone: brief"}, nil
		},
	}}
	r := newRig(t, rigOptions{memory: mem},
		aecmock.ScriptedTurn{ToolCalls: []kernel.ToolCall{{ID: "c1", Name: "memory_search", Arguments: kernel.Arguments(`{"query":"tone"}`)}}},
		aecmock.ScriptedTurn{Content: "Brief it is.", StopReason: kernel.StopEndTurn},
	)
	if _, err := r.run(t, "How should you write?"); err != nil {
		t.Fatal(err)
	}

	if mem.turnRecalls != 0 {
		t.Errorf("per-turn recalls = %d, want 0 (recall is on demand)", mem.turnRecalls)
	}
	if n := mem.search.calls.Load(); n != 1 || gotSession != "s-1" {
		t.Fatalf("memory_search ran %d times in session %q, want once in s-1", n, gotSession)
	}
	calls := r.models.Calls()
	if len(calls) != 2 {
		t.Fatalf("model calls = %d, want 2", len(calls))
	}
	offered := false
	for _, d := range calls[0].Options.Tools {
		offered = offered || d.Name == "memory_search"
	}
	if !offered {
		t.Errorf("tools offered = %v, want memory_search among them", calls[0].Options.Tools)
	}
	for _, m := range calls[0].Messages {
		if m.Role == kernel.RoleSystem && strings.Contains(m.Content, "Recalled memories") {
			t.Errorf("the system message carries recalled memories: %q", m.Content)
		}
	}
	var result *kernel.ToolResult
	for _, m := range calls[1].Messages {
		for i := range m.ToolResults {
			if m.ToolResults[i].ToolCallID == "c1" {
				result = &m.ToolResults[i]
			}
		}
	}
	if result == nil || (result.IsError != nil && *result.IsError) || result.Content != "- [fact] tone: brief" {
		t.Fatalf("memory_search result = %+v, want the recalled memory", result)
	}
}
