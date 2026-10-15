package agent_test

import (
	"testing"

	"github.com/Phong95/aec-agent-go/components/sandboxhub"
)

// TestAgent_SharedSandboxIsLazy is the hand-built-Deps half of the tracer of plan
// 10-05: an agent over a sandbox hub creates no sandbox at spawn or when a session
// starts, only when a tool asks, and the tool gets the shared sandbox in the agent's
// own folder, which the agent's own Sandbox call sees too.
func TestAgent_SharedSandboxIsLazy(t *testing.T) {
	r := newSandboxRig(t, sandboxhub.Selection{}, true)
	if _, err := r.agent.StartSession(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n := r.provider.CreateCount(); n != 0 {
		t.Fatalf("CreateCount after spawn and StartSession = %d, want 0", n)
	}

	chat(t, r.agent, "probe")
	if n := r.provider.CreateCount(); n != 1 {
		t.Fatalf("CreateCount after the tool ran = %d, want 1", n)
	}
	if got, want := lastToolResult(r.model), "docker:shared /workspace/agents/probe-agent SECRET-ARG"; got != want {
		t.Fatalf("tool result = %q, want %q", got, want)
	}

	// The agent's own handle reaches the same folder: the file the tool wrote is there.
	view, err := r.agent.Sandbox(t.Context())
	if err != nil {
		t.Fatalf("Sandbox: %v", err)
	}
	got, err := view.ReadFile(t.Context(), "probe.txt", sandboxhub.ReadOptions{})
	if err != nil || got.Content != "SECRET-FILE-CONTENT" {
		t.Fatalf("ReadFile(probe.txt) = %+v, %v, want the tool's file", got, err)
	}
	if n := r.provider.CreateCount(); n != 1 {
		t.Fatalf("CreateCount after Sandbox = %d, want the sandbox reused (1)", n)
	}
}
