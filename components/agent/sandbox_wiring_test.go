package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Phong95/aec-agent-go/components/agent"
	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/components/trace"
	"github.com/Phong95/aec-agent-go/kernel"
)

// begin starts a session of the agent.
func begin(t *testing.T, a *agent.Agent) {
	t.Helper()
	if _, err := a.StartSession(t.Context()); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
}

// ownSelection is the own-sandbox selection with cfg.
func ownSelection(cfg sandboxhub.OwnConfig) sandboxhub.Selection {
	return sandboxhub.Selection{Mode: sandboxhub.SelectionOwn, Own: &cfg}
}

// TestAgentSandbox covers the agent's sandbox wiring, ToolContext
// GetSandbox, the lazy start and the sandbox-op spans. The AgentCore.dispose half of
// the last case is TestAgentCore_DisposeStopsSharedSandbox in the root package; here
// the shared sandbox is stopped by the hub, which is what AgentCore.Dispose calls.
func TestAgentSandbox(t *testing.T) {
	t.Run("does not start a sandbox at spawn \u2014 only when a tool asks", func(t *testing.T) {
		r := newSandboxRig(t, sandboxhub.Selection{}, true)
		begin(t, r.agent)
		if n := r.provider.CreateCount(); n != 0 {
			t.Fatalf("CreateCount after StartSession = %d, want 0", n)
		}
		chat(t, r.agent, "probe")
		if n := r.provider.CreateCount(); n != 1 {
			t.Fatalf("CreateCount after chat = %d, want 1", n)
		}
	})

	t.Run("gives tools the shared sandbox by default, in the agent's own folder", func(t *testing.T) {
		r := newSandboxRig(t, sandboxhub.Selection{}, true)
		begin(t, r.agent)
		chat(t, r.agent, "probe")
		if got, want := lastToolResult(r.model), "docker:shared /workspace/agents/probe-agent SECRET-ARG"; got != want {
			t.Fatalf("tool result = %q, want %q", got, want)
		}
		shared, ok := r.provider.Sandbox("docker:shared")
		if !ok {
			t.Fatal("the shared sandbox was not created")
		}
		if !shared.Has("/workspace/agents/probe-agent/probe.txt") {
			t.Fatal("probe.txt is not in the agent's folder of the shared sandbox")
		}
	})

	t.Run("uses the agent's own sandbox when the profile asks for one", func(t *testing.T) {
		r := newSandboxRig(t, ownSelection(sandboxhub.OwnConfig{}), true)
		begin(t, r.agent)
		chat(t, r.agent, "probe")
		if got, want := lastToolResult(r.model), "docker:own:probe-agent /workspace SECRET-ARG"; got != want {
			t.Fatalf("tool result = %q, want %q", got, want)
		}
	})

	t.Run(`fails closed for sandbox "none" and for an unconfigured shared sandbox`, func(t *testing.T) {
		none := newSandboxRig(t, sandboxhub.Selection{Mode: sandboxhub.SelectionNone}, true)
		begin(t, none.agent)
		chat(t, none.agent, "probe")
		if got := lastToolResult(none.model); got != "disabled" {
			t.Fatalf(`sandbox "none": tool result = %q, want disabled`, got)
		}
		if n := none.provider.CreateCount(); n != 0 {
			t.Fatalf(`sandbox "none" created %d sandboxes, want 0`, n)
		}

		unconfigured := newSandboxRig(t, sandboxhub.Selection{}, false)
		begin(t, unconfigured.agent)
		chat(t, unconfigured.agent, "probe")
		if got := lastToolResult(unconfigured.model); got != "not_configured" {
			t.Fatalf("unconfigured shared: tool result = %q, want not_configured", got)
		}
		if n := unconfigured.provider.CreateCount(); n != 0 {
			t.Fatalf("unconfigured shared created %d sandboxes, want 0", n)
		}
	})

	t.Run("traces every sandbox operation as a sandbox-op span under the tool span, without contents", func(t *testing.T) {
		r := newSandboxRig(t, sandboxhub.Selection{}, true)
		sink := r.sinkOf()
		begin(t, r.agent)
		chat(t, r.agent, "probe")

		var dispatch trace.Span
		for _, sp := range sink.ofKind(trace.KindToolDispatch) {
			if sp.Name == "workspace_probe" {
				dispatch = sp
			}
		}
		if dispatch.SpanID == "" {
			t.Fatal("no tool-dispatch span for workspace_probe")
		}
		ops := sink.ofKind(trace.KindSandboxOp)
		var names []string
		for _, op := range ops {
			names = append(names, op.Name)
		}
		if got := strings.Join(names, ","); got != "fs.write,exec" {
			t.Fatalf("sandbox-op spans = %q, want fs.write,exec", got)
		}
		for _, op := range ops {
			if op.ParentSpanID != dispatch.SpanID {
				t.Errorf("%s parent = %q, want the tool-dispatch span %q", op.Name, op.ParentSpanID, dispatch.SpanID)
			}
			for key, want := range map[string]any{"sandboxId": "docker:shared", "agentId": "probe-agent", "tenancy": "shared"} {
				if got, _ := op.Attributes.Get(key); got != want {
					t.Errorf("%s attribute %s = %v, want %v", op.Name, key, got, want)
				}
			}
		}
		exec := ops[1].Attributes
		if got, _ := exec.Get("command"); got != "echo" {
			t.Errorf("exec command = %v, want echo", got)
		}
		if got, _ := exec.Get("argCount"); got != 1 {
			t.Errorf("exec argCount = %v, want 1", got)
		}
		if got, _ := exec.Get("exitCode"); got != 0 {
			t.Errorf("exec exitCode = %v, want 0", got)
		}
		serialized, err := json.Marshal(ops)
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{"SECRET-FILE-CONTENT", "SECRET-ARG"} {
			if strings.Contains(string(serialized), secret) {
				t.Errorf("the sandbox-op spans carry %q: %s", secret, serialized)
			}
		}
	})

	t.Run("updateSandbox validates against the hub policy before applying", func(t *testing.T) {
		r := newSandboxRig(t, sandboxhub.Selection{}, true)
		err := r.agent.UpdateSandbox(ownSelection(sandboxhub.OwnConfig{Image: "not-allowed"}))
		var se *kernel.SandboxError
		if !errors.As(err, &se) || se.Code != kernel.SandboxCodePolicyDenied {
			t.Fatalf("UpdateSandbox(image not-allowed) = %v, want a policy_denied SandboxError", err)
		}
		if got := r.agent.Profile().Sandbox; got.Mode != "" || got.Own != nil {
			t.Fatalf("profile sandbox after a refused update = %+v, want unchanged (zero)", got)
		}

		if err := r.agent.UpdateSandbox(ownSelection(sandboxhub.OwnConfig{})); err != nil {
			t.Fatalf("UpdateSandbox(own {}): %v", err)
		}
		if got := r.agent.Profile().Sandbox; got.Mode != sandboxhub.SelectionOwn {
			t.Fatalf("profile sandbox after a valid update = %+v, want own", got)
		}
		begin(t, r.agent)
		chat(t, r.agent, "probe")
		if got := lastToolResult(r.model); !strings.Contains(got, "docker:own:probe-agent") {
			t.Fatalf("tool result = %q, want the own sandbox", got)
		}
	})

	t.Run("agent.dispose stops its own sandbox, and AgentCore.dispose stops the shared one", func(t *testing.T) {
		r := newSandboxRig(t, ownSelection(sandboxhub.OwnConfig{}), true)
		if _, err := r.agent.Sandbox(t.Context()); err != nil {
			t.Fatalf("Sandbox: %v", err)
		}
		if err := r.agent.Dispose(context.WithoutCancel(t.Context())); err != nil {
			t.Fatalf("Dispose: %v", err)
		}
		own, ok := r.provider.Sandbox("docker:own:probe-agent")
		if !ok || !own.Disposed() {
			t.Fatal("the own sandbox is not stopped by Agent.Dispose")
		}

		second := testProfile("second")
		other := r.spawn(t, second)
		if _, err := other.Sandbox(t.Context()); err != nil {
			t.Fatalf("second.Sandbox: %v", err)
		}
		shared, ok := r.provider.Sandbox("docker:shared")
		if !ok {
			t.Fatal("the shared sandbox was not created")
		}
		// Disposing an agent never stops the shared sandbox: it belongs to the hub.
		if err := other.Dispose(context.WithoutCancel(t.Context())); err != nil {
			t.Fatalf("second Dispose: %v", err)
		}
		if shared.Disposed() {
			t.Fatal("Agent.Dispose stopped the shared sandbox")
		}
		// What AgentCore.Dispose calls.
		if err := r.sandboxes.Dispose(context.WithoutCancel(t.Context())); err != nil {
			t.Fatalf("hub Dispose: %v", err)
		}
		if !shared.Disposed() {
			t.Fatal("the shared sandbox is not stopped by the hub")
		}
	})
}
