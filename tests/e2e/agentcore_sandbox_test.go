package e2e_test

import (
	"context"
	"errors"
	"testing"

	aec "github.com/Phong95/aec-agent-go"
	"github.com/Phong95/aec-agent-go/aecmock"
	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/kernel"
)

// writeNoteTool is the tool of the tracer of plan 10-05: it writes note.txt with the
// content "hi" through toolhub.Context.GetSandbox, the only way a tool reaches a sandbox.
type writeNoteTool struct{}

func (writeNoteTool) Name() string    { return "tool:write-note" }
func (writeNoteTool) Version() string { return "1.0.0" }
func (writeNoteTool) Definition() toolhub.Definition {
	return toolhub.Definition{Name: "write_note", Description: "Writes note.txt", Parameters: toolhub.JSONSchema{Type: "object"}, Risk: toolhub.RiskDraft}
}

func (writeNoteTool) Execute(ctx context.Context, _ kernel.Arguments, tc toolhub.Context) (toolhub.Output, error) {
	if tc.GetSandbox == nil {
		return toolhub.Output{}, errors.New("no sandbox in the tool context")
	}
	view, err := tc.GetSandbox(ctx)
	if err != nil {
		return toolhub.Output{}, err
	}
	if _, err := view.WriteFile(ctx, "note.txt", "hi", sandboxhub.WriteOptions{}); err != nil {
		return toolhub.Output{}, err
	}
	return toolhub.Output{Content: "written"}, nil
}

// The tracer of plan 10-05: an AgentCore with an in-memory sandbox provider and a
// configured shared sandbox; a tool that an agent runs writes through
// toolhub.Context.GetSandbox into the agent's own folder of the shared sandbox, and a
// second agent of the same core does not see the file (each agent has its own folder).
func TestAgentCore_ToolWritesIntoAgentFolder(t *testing.T) {
	ctx := t.Context()
	core, err := aec.NewAgentCore()
	if err != nil {
		t.Fatal(err)
	}
	provider := aecmock.NewInMemoryProvider()
	if err := core.UseSandboxProvider(provider); err != nil {
		t.Fatalf("UseSandboxProvider: %v", err)
	}
	if err := core.SandboxHub().ConfigureShared(sandboxhub.Spec{Kind: sandboxhub.KindDocker}); err != nil {
		t.Fatalf("ConfigureShared: %v", err)
	}
	model := callThenEnd(t, "write_note")
	if err := core.UseLLMProvider(model); err != nil {
		t.Fatal(err)
	}
	if err := core.UseTool(writeNoteTool{}); err != nil {
		t.Fatal(err)
	}

	p1 := testProfile("agent-one")
	p1.Tools = []string{"write_note"}
	p2 := testProfile("agent-two")
	p2.Tools = []string{"write_note"}
	one, err := core.SpawnAgent(ctx, p1)
	if err != nil {
		t.Fatalf("SpawnAgent one: %v", err)
	}
	two, err := core.SpawnAgent(ctx, p2)
	if err != nil {
		t.Fatalf("SpawnAgent two: %v", err)
	}
	if n := provider.CreateCount(); n != 0 {
		t.Fatalf("CreateCount after spawning = %d, want 0 (the sandbox is created lazily)", n)
	}

	session, err := one.StartSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := one.Chat(ctx, "write the note"); err != nil {
		t.Fatalf("Chat: %v", err)
	}

	// The tool result is not an error.
	stored, err := core.PersistenceHub().Messages.Get(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	var results int
	for _, m := range stored {
		for _, r := range m.ToolResults {
			results++
			if r.IsError != nil && *r.IsError {
				t.Fatalf("write_note result is an error: %q", r.Content)
			}
			if r.Content != "written" {
				t.Fatalf("write_note result = %q, want written", r.Content)
			}
		}
	}
	if results != 1 {
		t.Fatalf("stored tool results = %d, want 1", results)
	}

	// Agent one reads its file back; agent two has a different folder and no file.
	view1, err := one.Sandbox(ctx)
	if err != nil {
		t.Fatalf("one.Sandbox: %v", err)
	}
	got, err := view1.ReadFile(ctx, "note.txt", sandboxhub.ReadOptions{})
	if err != nil || got.Content != "hi" {
		t.Fatalf("one reads note.txt = %+v, %v, want hi", got, err)
	}
	if view1.WorkingDir() != "/workspace/agents/agent-one" {
		t.Fatalf("one working dir = %q", view1.WorkingDir())
	}
	view2, err := two.Sandbox(ctx)
	if err != nil {
		t.Fatalf("two.Sandbox: %v", err)
	}
	if view2.WorkingDir() != "/workspace/agents/agent-two" {
		t.Fatalf("two working dir = %q", view2.WorkingDir())
	}
	if got, err := view2.ReadFile(ctx, "note.txt", sandboxhub.ReadOptions{}); err == nil {
		t.Fatalf("two reads note.txt = %+v, want an error: it is agent one's file", got)
	}
	// Neither can reach the other's folder by an absolute path.
	if got, err := view2.ReadFile(ctx, "/workspace/agents/agent-one/note.txt", sandboxhub.ReadOptions{}); err == nil {
		t.Fatalf("two reads agent one's file by its path = %+v, want path_escape", got)
	}
	if n := provider.CreateCount(); n != 1 {
		t.Fatalf("CreateCount = %d, want one shared sandbox for both agents", n)
	}

	if err := core.Dispose(context.WithoutCancel(ctx)); err != nil {
		t.Fatalf("Dispose: %v", err)
	}
}
