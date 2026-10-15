package docker

import (
	"context"
	"strings"
	"testing"

	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/components/toolhub/builtin/workspace"
	"github.com/Phong95/aec-agent-go/kernel"
)

// The three workspace-tool cases ("workspace tools", A-61), run on a real container. They are guarded like every real
// daemon test (guard_test.go): they run only with AEC_DOCKER_TESTS=1 and a Docker
// daemon, and every name they create is aec-test-<hex>. The tools are the Go workspace
// tools run directly with a toolhub.Context whose GetSandbox returns the agent's view of the
// shared container; importing builtin/workspace here is a test import and creates no
// cycle, because that package imports sandbox and not sandbox/docker.

// workspaceContext returns a tool context whose GetSandbox returns the
// view of the agent on the shared sandbox.
func workspaceContext(t *testing.T, e *dockerEnv, agentID string) toolhub.Context {
	t.Helper()
	view := e.resolve(t, agentID)
	return toolhub.Context{
		SessionID:  "s1",
		AgentID:    agentID,
		Emit:       func(string, any) {},
		GetSandbox: func(context.Context) (sandboxhub.View, error) { return view, nil },
	}
}

// workspaceTool returns the workspace tool of that name with the default options.
func workspaceTool(t *testing.T, name string) toolhub.Handler {
	t.Helper()
	for _, h := range workspace.NewTools(workspace.Options{}) {
		if h.Definition().Name == name {
			return h
		}
	}
	t.Fatalf("no workspace tool %q", name)
	return nil
}

// runTool executes a tool with JSON arguments and fails the test on a returned error.
func runTool(t *testing.T, h toolhub.Handler, tc toolhub.Context, args string) toolhub.Output {
	t.Helper()
	a, err := kernel.NewArguments([]byte(args))
	if err != nil {
		t.Fatalf("arguments %s: %v", args, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), dockerCallWait)
	defer cancel()
	out, err := h.Execute(ctx, a, tc)
	if err != nil {
		t.Fatalf("%s returned an error: %v", h.Definition().Name, err)
	}
	return out
}

func toolFailed(o toolhub.Output) bool { return o.IsError != nil && *o.IsError }

func TestDockerWorkspaceTools(t *testing.T) {
	t.Run("write \u2192 edit \u2192 read \u2192 search with read-back verification", func(t *testing.T) {
		e := newDockerEnv(t)
		tc := workspaceContext(t, e, "tools-files")

		written := runTool(t, workspaceTool(t, "write_file"), tc, `{"path":"docs/spec.md","content":"Slab: 200mm\nConcrete: C30\n"}`)
		if !strings.Contains(written.Content, "verified by read-back") {
			t.Errorf("write_file = %q, want a verified write", written.Content)
		}
		if edited := runTool(t, workspaceTool(t, "edit_file"), tc, `{"path":"docs/spec.md","old_text":"C30","new_text":"C35"}`); toolFailed(edited) {
			t.Errorf("edit_file failed: %s", edited.Content)
		}
		if read := runTool(t, workspaceTool(t, "read_file"), tc, `{"path":"docs/spec.md"}`); !strings.Contains(read.Content, "2\tConcrete: C35") {
			t.Errorf("read_file = %q", read.Content)
		}
		if found := runTool(t, workspaceTool(t, "search_files"), tc, `{"pattern":"C35"}`); !strings.Contains(found.Content, "docs/spec.md:2:") {
			t.Errorf("search_files = %q", found.Content)
		}
	})

	t.Run("terminal uses the container's shell (pipes, chaining) with bounded output", func(t *testing.T) {
		e := newDockerEnv(t)
		files := workspaceContext(t, e, "tools-files")
		runTool(t, workspaceTool(t, "write_file"), files, `{"path":"docs/spec.md","content":"Slab: 200mm\nConcrete: C30\n"}`)
		tc := workspaceContext(t, e, "tools-terminal")

		piped := runTool(t, workspaceTool(t, "terminal"), tc, `{"command":"mkdir -p out && seq 1 5 | tail -n 2 > out/n.txt && cat out/n.txt"}`)
		if !strings.Contains(piped.Content, "exit code 0") || !strings.Contains(piped.Content, "4\n5") {
			t.Errorf("piped command = %q, want exit code 0 and the last two numbers", piped.Content)
		}

		flood := runTool(t, workspace.NewTerminalTool(workspace.Options{MaxDisplayOutputBytes: 2000}), tc, `{"command":"seq 1 200000"}`)
		if v, _ := flood.Metadata.Get("displayTruncated"); v != true {
			t.Errorf("displayTruncated = %v, want true", v)
		}
		if !strings.Contains(flood.Content, "omitted") {
			t.Errorf("flood = %.200q..., want an omitted marker", flood.Content)
		}
		if !strings.Contains(flood.Content, "200000") { // the tail is kept
			t.Errorf("flood = %.200q..., want the tail of the output", flood.Content)
		}

		escape := runTool(t, workspaceTool(t, "terminal"), tc, `{"command":"cat /workspace/agents/tools-files/docs/spec.md"}`)
		// exec can see other agents' folders in a SHARED container (documented trade-off); file tools cannot:
		if read := runTool(t, workspaceTool(t, "read_file"), tc, `{"path":"/workspace/agents/tools-files/docs/spec.md"}`); !strings.Contains(read.Content, "path_escape") {
			t.Errorf("read_file of another agent's folder = %q, want path_escape", read.Content)
		}
		if !strings.Contains(escape.Content, "exit code") {
			t.Errorf("terminal = %q, want an exit code line", escape.Content)
		}
	})

	t.Run("run_code runs javascript from stdin in the working directory", func(t *testing.T) {
		e := newDockerEnv(t)
		tc := workspaceContext(t, e, "tools-code")
		runTool(t, workspaceTool(t, "write_file"), tc, `{"path":"data.json","content":"{\"beams\":[3,4,5]}"}`)

		out := runTool(t, workspaceTool(t, "run_code"), tc,
			`{"language":"javascript","code":"const d = require('./data.json'); console.log('total', d.beams.reduce((a, b) => a + b, 0))"}`)
		if !strings.Contains(out.Content, "total 12") {
			t.Errorf("run_code javascript = %q, want total 12", out.Content)
		}
		shell := runTool(t, workspaceTool(t, "run_code"), tc, `{"language":"shell","code":"echo $((6 * 7))"}`)
		if !strings.Contains(shell.Content, "42") {
			t.Errorf("run_code shell = %q, want 42", shell.Content)
		}
	})
}
