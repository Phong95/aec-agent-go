package agent_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Phong95/aec-agent-go/aecmock"
	"github.com/Phong95/aec-agent-go/components/agent"
	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/kernel"
)

// A-153: a session the user attached a runner to (a folder on their machine) gets that
// folder for its workspace tools; every other session, and every other tool, keeps the
// agent's sandbox.

// idProbeTool reports the id of the sandbox it got; sessionWorkspace marks it as a
// workspace tool.
type idProbeTool struct {
	name             string
	sessionWorkspace bool
}

func (p idProbeTool) Name() string  { return "tool:" + p.name }
func (idProbeTool) Version() string { return "1.0.0" }
func (p idProbeTool) Definition() toolhub.Definition {
	return toolhub.Definition{
		Name: p.name, Description: "probe", Parameters: toolhub.JSONSchema{Type: "object"}, Risk: toolhub.RiskReadOnly,
		SandboxOperation: sandboxhub.OperationFSRead, SessionWorkspace: p.sessionWorkspace,
	}
}

func (idProbeTool) Execute(ctx context.Context, _ kernel.Arguments, tc toolhub.Context) (toolhub.Output, error) {
	// A sandbox failure is the tool's result, not an error of the call.
	fail := func(err error) (toolhub.Output, error) {
		return toolhub.Output{Content: err.Error(), IsError: boolPtr(true), Retryable: boolPtr(false)}, nil
	}
	view, err := tc.GetSandbox(ctx)
	if err != nil {
		return fail(err)
	}
	return toolhub.Output{Content: view.ID()}, nil
}

// silentRunner is a runner sandbox whose connection goes nowhere: enough for a tool that
// only reads the view's id.
func silentRunner(id string) *sandboxhub.RunnerSandbox {
	return sandboxhub.NewRunnerSandbox(sandboxhub.RunnerSandboxOptions{
		ID:    id,
		Hello: sandboxhub.RunnerHello{Type: "hello", Protocol: sandboxhub.RunnerProtocolVersion, OS: sandboxhub.OSWindows, FolderName: "my-app", AllowExec: true},
		Send:  func(sandboxhub.RunnerMessage) error { return nil },
	})
}

type runnerRig struct {
	env   *env
	agent *agent.Agent
	model *aecmock.MockProvider
}

func newRunnerRig(t *testing.T, tool idProbeTool) *runnerRig {
	t.Helper()
	e := newEnv(t)
	model := oneToolCallProvider(t, "one-tool-call", tool.name)
	if err := e.llm.Register(model); err != nil {
		t.Fatal(err)
	}
	if err := e.tools.Register(tool); err != nil {
		t.Fatal(err)
	}
	if err := e.sandboxes.Register(aecmock.NewInMemoryProvider()); err != nil {
		t.Fatal(err)
	}
	if err := e.sandboxes.ConfigureShared(sandboxhub.Spec{Kind: sandboxhub.KindDocker}); err != nil {
		t.Fatal(err)
	}
	p := testProfile("runner-agent")
	p.Provider = "one-tool-call"
	p.Tools = []string{tool.name}
	return &runnerRig{env: e, agent: e.spawn(t, p), model: model}
}

func TestSessionRunnerWorkspace(t *testing.T) {
	t.Run("a workspace tool works in the session's runner folder", func(t *testing.T) {
		r := newRunnerRig(t, idProbeTool{name: "session_probe", sessionWorkspace: true})
		s, err := r.agent.StartSession(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := r.env.sandboxes.AttachRunner("runner-agent", s.ID, silentRunner("remote:runner:"+s.ID)); err != nil {
			t.Fatal(err)
		}
		chat(t, r.agent, "probe")
		if got := lastToolResult(r.model); got != "remote:runner:"+s.ID {
			t.Fatalf("tool result = %q, want the runner", got)
		}
		view, err := r.agent.SessionSandbox(t.Context(), s.ID)
		if err != nil || view.Traits().Kind != sandboxhub.KindRemote {
			t.Fatalf("SessionSandbox = %v, %v", view, err)
		}
		if view, err := r.agent.Sandbox(t.Context()); err != nil || view.Traits().Kind == sandboxhub.KindRemote {
			t.Fatalf("Sandbox = %v, %v; want the agent's own", view, err)
		}
	})

	t.Run("a session without a runner keeps the agent's sandbox", func(t *testing.T) {
		r := newRunnerRig(t, idProbeTool{name: "session_probe", sessionWorkspace: true})
		begin(t, r.agent)
		chat(t, r.agent, "probe")
		if got := lastToolResult(r.model); got != "docker:shared" {
			t.Fatalf("tool result = %q, want docker:shared", got)
		}
	})

	t.Run("other tools keep the agent's sandbox in a runner session", func(t *testing.T) {
		r := newRunnerRig(t, idProbeTool{name: "artifact_probe"})
		s, err := r.agent.StartSession(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := r.env.sandboxes.AttachRunner("runner-agent", s.ID, silentRunner("remote:runner:"+s.ID)); err != nil {
			t.Fatal(err)
		}
		chat(t, r.agent, "probe")
		if got := lastToolResult(r.model); got != "docker:shared" {
			t.Fatalf("tool result = %q, want docker:shared", got)
		}
	})
}

func TestSandboxManagerSessionRunners(t *testing.T) {
	hub := sandboxhub.NewSandboxHub()
	none := func() sandboxhub.Selection { return sandboxhub.Selection{Mode: sandboxhub.SelectionNone} }
	m := agent.NewSandboxManager(hub, "agent-1", none)
	ctx := t.Context()
	wantDisabled := func(t *testing.T, err error) {
		t.Helper()
		var se *kernel.SandboxError
		if !errors.As(err, &se) || se.Code != kernel.SandboxCodeDisabled {
			t.Fatalf("error = %v, want disabled", err)
		}
	}

	runner := silentRunner("remote:runner:s1")
	if _, err := hub.AttachRunner("agent-1", "s1", runner); err != nil {
		t.Fatal(err)
	}
	if v, err := m.GetSessionSandbox(ctx, "s1"); err != nil || v.ID() != "remote:runner:s1" {
		t.Fatalf("s1 = %v, %v", v, err)
	}
	_, err := m.GetSessionSandbox(ctx, "s2")
	wantDisabled(t, err)
	// Another agent's session of the same id does not get it.
	_, err = agent.NewSandboxManager(hub, "agent-2", none).GetSessionSandbox(ctx, "s1")
	wantDisabled(t, err)
	if got := m.DescribeRunner("s1"); got != `the folder "my-app" on the user's computer` {
		t.Fatalf("DescribeRunner = %q", got)
	}

	// A subagent's run follows its parent session while it runs.
	stop := m.FollowSession("child", "s1")
	if v, err := m.GetSessionSandbox(ctx, "child"); err != nil || v.ID() != "remote:runner:s1" {
		t.Fatalf("child = %v, %v", v, err)
	}
	stop()
	_, err = m.GetSessionSandbox(ctx, "child")
	wantDisabled(t, err)

	// Offline: the session keeps its runner (calls fail), until it is detached.
	runner.Close("")
	if !strings.HasSuffix(m.DescribeRunner("s1"), "(offline)") {
		t.Fatalf("DescribeRunner = %q", m.DescribeRunner("s1"))
	}
	v, err := m.GetSessionSandbox(ctx, "s1")
	if err != nil {
		t.Fatal(err)
	}
	_, err = v.ReadFile(ctx, "x.txt", sandboxhub.ReadOptions{})
	var se *kernel.SandboxError
	if !errors.As(err, &se) || se.Code != kernel.SandboxCodeEnvironmentUnavailable {
		t.Fatalf("offline read = %v", err)
	}
	if !hub.DetachRunner("agent-1", "s1", nil) {
		t.Fatal("DetachRunner = false")
	}
	_, err = m.GetSessionSandbox(ctx, "s1")
	wantDisabled(t, err)
}

func TestSandboxHubRunnerEvents(t *testing.T) {
	bus := events.New()
	hub := sandboxhub.NewSandboxHub(sandboxhub.WithEvents(bus))
	t.Cleanup(func() { _ = hub.Dispose(context.Background()) })
	var seen []string
	events.Subscribe(bus, events.SandboxRunnerChanged, func(_ kernel.Event, p events.SandboxRunnerChangedPayload) {
		switch {
		case p.Runner == nil:
			seen = append(seen, "none")
		case p.Runner.Connected:
			seen = append(seen, "connected")
		default:
			seen = append(seen, "offline")
		}
	})
	first := silentRunner("remote:runner:s1")
	second := silentRunner("remote:runner:s1")
	if _, err := hub.AttachRunner("agent-1", "s1", first); err != nil {
		t.Fatal(err)
	}
	// A reconnect replaces (and closes) the old runner without an offline event for it.
	if _, err := hub.AttachRunner("agent-1", "s1", second); err != nil {
		t.Fatal(err)
	}
	if first.Connected() {
		t.Fatal("the replaced runner is still connected")
	}
	// A stale detach (the old connection closing) leaves the new runner alone.
	if hub.DetachRunner("agent-1", "s1", first) {
		t.Fatal("a stale detach detached the new runner")
	}
	second.Close("")
	hub.DetachRunner("agent-1", "s1", nil)
	want := []string{"connected", "connected", "offline", "none"}
	if strings.Join(seen, ",") != strings.Join(want, ",") {
		t.Fatalf("events = %v, want %v", seen, want)
	}
}
