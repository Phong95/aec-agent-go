package e2e_test

import (
	"strings"
	"testing"

	aec "github.com/Phong95/aec-agent-go"
	"github.com/Phong95/aec-agent-go/aecmock"
	"github.com/Phong95/aec-agent-go/components/artifacthub/artifact"
	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/components/toolhub/builtin/artifacts"
)

// TestArtifacts_SaveEndToEnd is the tracer of plan 11-03: an agent built with CreateAgent
// and the artifact tools saves an artifact through save_artifact. The content lands in the
// agent's folder of the in-memory sandbox of a real AgentCore, the artifact is tracked in
// the core's artifact hub for that agent and session, and artifact:saved is emitted with
// the expected payload (ROADMAP success criterion 3).
func TestArtifacts_SaveEndToEnd(t *testing.T) {
	ctx := t.Context()
	wc := newWorkspaceCore(t, workspaceCoreOptions{})
	core, provider := wc.core, wc.docker

	const content = "# Plan\n"
	model := aecmock.NewScriptedProvider("scripted", []aecmock.ScriptedTurn{
		toolTurn(toolCallOf("tc-1", "save_artifact", `{"name":"site-plan","type":"report","content":"# Plan\n"}`)),
		{Content: "Saved"},
	})

	a, err := aec.CreateAgent(ctx, aec.AgentConfig{
		Provider: "scripted",
		APIKey:   "k",
		Model:    "m",
		Tools:    []toolhub.Handler{artifacts.NewSaveTool(artifacts.Options{})},
	}, aec.WithCore(core), aec.WithProviderRegistry(registryFor(model)))
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	disposed := false
	dispose := func() {
		t.Helper()
		if disposed {
			return
		}
		disposed = true
		if err := a.Dispose(ctx); err != nil {
			t.Errorf("Dispose: %v", err)
		}
	}
	defer dispose()

	sess, err := a.StartSession(ctx)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	res, err := a.Chat(ctx, "Save the plan")
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if res.Message.Content != "Saved" {
		t.Errorf("reply = %q, want Saved", res.Message.Content)
	}

	// The model received the expected text as the tool result of the same turn.
	var toolContent string
	var isError bool
	for _, call := range model.Calls() {
		for _, m := range call.Messages {
			for _, r := range m.ToolResults {
				toolContent = r.Content
				isError = r.IsError != nil && *r.IsError
			}
		}
	}
	if !strings.HasPrefix(toolContent, `Saved artifact "site-plan" (report) as v1`) {
		t.Errorf("tool message = %q, want it to start with Saved artifact \"site-plan\" (report) as v1", toolContent)
	}
	if isError {
		t.Errorf("the tool result is an error: %q", toolContent)
	}

	// The agent's artifact view lists exactly that artifact, first created in this session.
	view, err := a.Artifacts(sess.ID)
	if err != nil {
		t.Fatalf("Artifacts: %v", err)
	}
	listed, err := view.List(ctx, artifact.Query{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(listed) != 1 {
		t.Fatalf("the agent has %d artifacts, want exactly one: %+v", len(listed), listed)
	}
	got := listed[0]
	if got.Name != "site-plan" || got.Type != "report" || got.Version != 1 || got.AgentID != a.ID() || got.SessionID != sess.ID {
		t.Errorf("artifact = %+v, want site-plan (report) v1 of agent %s created in session %s", got, a.ID(), sess.ID)
	}
	if !strings.Contains(toolContent, "id "+got.ID+".") {
		t.Errorf("tool message = %q does not name the artifact id %s", toolContent, got.ID)
	}

	// The tracked file is in the agent's folder of the in-memory sandbox, with that content.
	if n := provider.CreateCount(); n != 1 {
		t.Errorf("CreateCount = %d, want one shared sandbox", n)
	}
	sb, ok := provider.Sandbox("docker:shared")
	if !ok {
		t.Fatal("the provider holds no shared sandbox")
	}
	if !sb.Has(got.SandboxPath) {
		t.Fatalf("the sandbox has no file %s", got.SandboxPath)
	}
	if want := "/workspace/agents/" + a.ID() + "/artifacts/site-plan"; got.SandboxPath != want {
		t.Errorf("SandboxPath = %s, want %s", got.SandboxPath, want)
	}
	env, err := a.Sandbox(ctx)
	if err != nil {
		t.Fatalf("Sandbox: %v", err)
	}
	file, err := env.ReadFile(ctx, "artifacts/site-plan", sandboxhub.ReadOptions{})
	if err != nil || file.Content != content {
		t.Fatalf("artifacts/site-plan = %+v, %v; want content %q", file, err, content)
	}
	read, err := view.Read(ctx, artifact.Ref{Name: "site-plan"}, env, artifact.ReadOptions{})
	if err != nil || read.File.Status != artifact.FileCurrent || read.File.Content != content {
		t.Fatalf("Read = %+v, %v; want the current file with content %q", read.File, err, content)
	}

	// One artifact:saved event carries the agent id, the artifact id, version 1 and the
	// byte count, under the session of the run.
	if n := wc.log.count(events.ArtifactSaved.Name); n != 1 {
		t.Fatalf("%d artifact:saved events, want exactly one (all: %v)", n, wc.log.types())
	}
	for _, e := range wc.log.all() {
		if e.Type != events.ArtifactSaved.Name {
			continue
		}
		payload, ok := e.Payload.(events.ArtifactWrittenPayload)
		if !ok {
			t.Fatalf("payload is %T, want events.ArtifactWrittenPayload", e.Payload)
		}
		want := events.ArtifactWrittenPayload{
			AgentID: a.ID(), ArtifactID: got.ID, Name: "site-plan", Type: "report", Version: 1,
			Bytes: int64(len(content)), SandboxPath: got.SandboxPath, SandboxID: "docker:shared",
		}
		if payload != want {
			t.Errorf("artifact:saved payload = %+v\n want %+v", payload, want)
		}
		if e.SessionID != sess.ID {
			t.Errorf("artifact:saved SessionID = %q, want the session %q", e.SessionID, sess.ID)
		}
	}
	dispose()
}
