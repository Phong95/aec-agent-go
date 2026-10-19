package agent_test

import (
	"strings"
	"testing"

	"github.com/Phong95/aec-agent-go/components/agent/profile"
)

// TestAgent_UpdateInitialContextReachesNextModelCall: UpdateInitialContext (which a
// consolidation pass uses for a compacted USER.md / MEMORY.md) rebuilds the system
// message, so the next model call sees the new files and not the old ones.
func TestAgent_UpdateInitialContextReachesNextModelCall(t *testing.T) {
	p := testProfile("a1")
	p.InitialContext = &profile.InitialContext{UserMd: "old user", MemoryMd: "old memory", SoulMd: "soul"}
	a, e := newTestAgent(t, p)
	if _, err := a.StartSession(t.Context()); err != nil {
		t.Fatal(err)
	}

	a.UpdateInitialContext(&profile.InitialContext{UserMd: "new user", MemoryMd: "new memory", SoulMd: "soul"})
	chat(t, a, "hello")

	calls := e.mocks["primary"].Calls()
	if len(calls) == 0 || len(calls[0].Messages) == 0 {
		t.Fatal("no model call")
	}
	system := calls[len(calls)-1].Messages[0].Content
	if !strings.Contains(system, "new user") || !strings.Contains(system, "new memory") || strings.Contains(system, "old memory") {
		t.Errorf("system message = %q, want the new files only", system)
	}
	if got := a.Profile().InitialContext; got == nil || got.MemoryMd != "new memory" {
		t.Errorf("Profile().InitialContext = %+v, want the new files", got)
	}
}
