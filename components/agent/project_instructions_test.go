package agent_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/Phong95/aec-agent-go/aecmock"
	"github.com/Phong95/aec-agent-go/components/agent/command"
	"github.com/Phong95/aec-agent-go/components/agent/profile"
	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/components/toolhub/builtin/workspace"
	"github.com/Phong95/aec-agent-go/kernel"
)

// A-227 / A-229: a session scoped to a project folder gets the folder's AGENTS.md in its
// system message from the first model call, and the AGENTS.md of a sub-folder with the
// result of the first file tool call that reaches it.
func TestAgent_ProjectInstructions(t *testing.T) {
	e := newEnv(t)
	args, err := kernel.NewArguments([]byte(`{"path":"packages/api/src/x.ts"}`))
	if err != nil {
		t.Fatal(err)
	}
	model := aecmock.NewMockProvider("instr-model", nil, aecmock.WithResponse(func(msgs []kernel.Message) kernel.ModelResponse {
		usage := kernel.TokenUsage{InputTokens: 1, OutputTokens: 1}
		if slices.ContainsFunc(msgs, func(m kernel.Message) bool { return len(m.ToolResults) > 0 }) {
			return kernel.ModelResponse{Message: kernel.Message{Role: kernel.RoleAssistant, Content: "done"}, Usage: usage, StopReason: kernel.StopEndTurn}
		}
		return kernel.ModelResponse{
			Message:    kernel.Message{Role: kernel.RoleAssistant, ToolCalls: []kernel.ToolCall{{ID: "tc-1", Name: "read_file", Arguments: args}}},
			Usage:      usage,
			StopReason: kernel.StopToolUse,
		}
	}))
	if err := e.llm.Register(model); err != nil {
		t.Fatal(err)
	}
	if err := e.tools.Register(workspace.NewReadFileTool(workspace.Options{})); err != nil {
		t.Fatal(err)
	}
	if err := e.sandboxes.Register(aecmock.NewInMemoryProvider()); err != nil {
		t.Fatal(err)
	}
	if err := e.sandboxes.ConfigureShared(sandboxhub.Spec{Kind: sandboxhub.KindDocker}); err != nil {
		t.Fatal(err)
	}
	p := testProfile("instr-agent")
	p.Provider = "instr-model"
	p.Tools = []string{"read_file"}
	p.LoopConfig = &profile.LoopConfig{MaxToolRetries: intPtr(0), RetryBackoffMs: intPtr(0)}
	a := e.spawn(t, p)

	ctx := t.Context()
	view, err := a.Sandbox(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for file, content := range map[string]string{
		"proj/AGENTS.md":                  "Root rule: answer in haiku.",
		"proj/packages/api/AGENTS.md":     "API rule: never touch the schema.",
		"proj/packages/web/AGENTS.md":     "Web rule: use tabs.",
		"proj/packages/api/src/x.ts":      "export const x = 1\n",
		"elsewhere/AGENTS.md":             "Not this project.",
		"proj/packages/api/src/CLAUDE.md": "Not read.",
	} {
		if _, err := view.WriteFile(ctx, file, content, sandboxhub.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	session, err := a.StartSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.SetSessionFolders(ctx, session.ID, []string{"proj"}); err != nil {
		t.Fatal(err)
	}
	chat(t, a, "what are this project's rules?")

	calls := model.Calls()
	if len(calls) < 2 {
		t.Fatalf("%d model calls, want 2", len(calls))
	}
	system := calls[0].Messages[0].Content
	if !strings.Contains(system, "<project_instructions>") || !strings.Contains(system, "Root rule: answer in haiku.") {
		t.Fatalf("the first call's system message misses the root AGENTS.md:\n%s", system)
	}
	for _, not := range []string{"API rule", "Web rule", "Not this project", "Not read"} {
		if strings.Contains(system, not) {
			t.Fatalf("the system message must not carry %q:\n%s", not, system)
		}
	}
	result := lastToolResult(model)
	if !strings.Contains(result, "export const x = 1") || !strings.Contains(result, "API rule: never touch the schema.") || strings.Contains(result, "Web rule") {
		t.Fatalf("the read's result must carry the api AGENTS.md only:\n%s", result)
	}

	state := a.ProjectInstructions(ctx, session.ID)
	var paths []string
	for _, f := range state.Files {
		paths = append(paths, f.Path)
	}
	if !state.Enabled || len(paths) != 2 || !strings.HasSuffix(paths[0], "/proj/AGENTS.md") || !strings.HasSuffix(paths[1], "/proj/packages/api/AGENTS.md") {
		t.Fatalf("state = %+v", state)
	}

	// A-230: /instructions lists both.
	res := a.Commands().Run(ctx, "/instructions", command.Invocation{SessionID: session.ID, Client: "web"})
	if text := res.Text; !strings.Contains(text, "/proj/AGENTS.md (") || !strings.Contains(text, "Loaded from sub-folders in this conversation:") || !strings.Contains(text, "/proj/packages/api/AGENTS.md (") {
		t.Fatalf("/instructions = %q", text)
	}

	// Another session without folders gets nothing.
	other, err := a.StartSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := a.ProjectInstructions(ctx, other.ID); len(got.Files) != 0 || len(got.Folders) != 0 {
		t.Fatalf("a session without folders: %+v", got)
	}
	if res := a.Commands().Run(ctx, "/instructions", command.Invocation{SessionID: other.ID, Client: "web"}); !strings.Contains(res.Text, "no project folder") {
		t.Fatalf("/instructions without folders = %+v", res)
	}
}
