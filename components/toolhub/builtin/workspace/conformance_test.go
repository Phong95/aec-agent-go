package workspace

import (
	"context"
	"slices"
	"testing"

	"github.com/Phong95/aec-agent-go/aecmock"
	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/conformance"
	"github.com/Phong95/aec-agent-go/kernel"
)

// withView wraps a handler so that every Execute gets a tool context whose GetSandbox
// returns an in-memory view. The conformance suite builds its own bare toolhub.Context, so
// a factory cannot inject a sandbox; this decorator is how the suite also reaches the
// path of a tool that has one.
type withView struct {
	toolhub.Handler
	view sandboxhub.View
}

func (w withView) Execute(ctx context.Context, args kernel.Arguments, tc toolhub.Context) (toolhub.Output, error) {
	tc.GetSandbox = func(context.Context) (sandboxhub.View, error) { return w.view, nil }
	return w.Handler.Execute(ctx, args, tc)
}

// TestWorkspaceToolsConformance runs the tool-handler conformance suite on every
// workspace tool, once with no sandbox accessor (the tool reports that the agent has no
// workspace) and once with an in-memory sandbox accessor.
func TestWorkspaceToolsConformance(t *testing.T) {
	cases := []struct {
		name    string
		factory func(Options) toolhub.Handler
		args    string
	}{
		{"read_file", func(o Options) toolhub.Handler { return NewReadFileTool(o) }, `{"path":"a.txt"}`},
		{"write_file", func(o Options) toolhub.Handler { return NewWriteFileTool(o) }, `{"path":"a.txt","content":"hello\n"}`},
		{"edit_file", func(o Options) toolhub.Handler { return NewEditFileTool(o) }, `{"path":"a.txt","old_text":"hello","new_text":"bye"}`},
		{"list_files", func(o Options) toolhub.Handler { return NewListFilesTool(o) }, `{}`},
		{"search_files", func(o Options) toolhub.Handler { return NewSearchFilesTool(o) }, `{"pattern":"hello"}`},
		{"terminal", func(o Options) toolhub.Handler { return NewTerminalTool(o) }, `{"command":"echo hello"}`},
		{"run_code", func(o Options) toolhub.Handler { return NewRunCodeTool(o) }, `{"language":"python","code":"print(1)"}`},
	}
	if len(cases) != len(ToolNames()) {
		t.Fatalf("the table has %d tools, ToolNames has %d: keep them in step", len(cases), len(ToolNames()))
	}
	for _, c := range cases {
		args, err := kernel.NewArguments([]byte(c.args))
		if err != nil {
			t.Fatal(err)
		}
		t.Run(c.name+" with no sandbox", func(t *testing.T) {
			conformance.ToolHandler(t, func(*testing.T) toolhub.Handler { return c.factory(Options{}) }, args)
		})
		t.Run(c.name+" with an in-memory sandbox", func(t *testing.T) {
			conformance.ToolHandler(t, func(t *testing.T) toolhub.Handler {
				view, err := aecmock.NewInMemoryView("docker:shared", "a1")
				if err != nil {
					t.Fatal(err)
				}
				if _, err := view.WriteFile(t.Context(), "a.txt", "hello\n", sandboxhub.WriteOptions{}); err != nil {
					t.Fatal(err)
				}
				return withView{Handler: c.factory(Options{}), view: view}
			}, args)
		})
	}
}

// TestWorkspaceCorpusCoversEveryTool keeps the recorder honest: every tool of
// NewTools has a definition entry and cases in the recorded corpus, so a tool
// added to the package cannot go without a recording.
func TestWorkspaceCorpusCoversEveryTool(t *testing.T) {
	fx := loadWireFixture(t)
	defs := map[string]int{}
	for _, d := range fx.Definitions {
		defs[d.Tool]++
	}
	cases := map[string]int{}
	for _, c := range fx.Cases {
		cases[c.Tool]++
	}
	for _, name := range ToolNames() {
		if defs[name] < 3 {
			t.Errorf("%s has %d definition variants in the corpus, want at least 3 (default and two option variants)", name, defs[name])
		}
		if cases[name] < 20 {
			t.Errorf("%s has %d cases in the corpus, want at least 20", name, cases[name])
		}
	}
	for name := range cases {
		if !slices.Contains(ToolNames(), name) {
			t.Errorf("the corpus holds cases for %q, which is not a workspace tool", name)
		}
	}
}

// TestToolNames: the tool names in order, a fresh copy on every call, and
// the names of the handlers NewTools builds.
func TestToolNames(t *testing.T) {
	want := []string{"read_file", "write_file", "edit_file", "list_files", "search_files", "terminal", "run_code"}
	if got := ToolNames(); !slices.Equal(got, want) {
		t.Fatalf("ToolNames() = %v, want %v", got, want)
	}
	first := ToolNames()
	first[0] = "changed"
	if second := ToolNames(); second[0] != "read_file" {
		t.Fatalf("ToolNames returned a shared slice: a change leaked into %v", second)
	}
	tools := NewTools(Options{})
	if len(tools) != len(want) {
		t.Fatalf("NewTools returned %d tools, want %d", len(tools), len(want))
	}
	for i, h := range tools {
		if h.Definition().Name != want[i] {
			t.Errorf("tool %d is %q, want %q", i, h.Definition().Name, want[i])
		}
	}
}
