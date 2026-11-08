package agent_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/kernel"
)

func sandboxErrCode(err error) kernel.SandboxErrorCode {
	var se *kernel.SandboxError
	if errors.As(err, &se) {
		return se.Code
	}
	return ""
}

// A-216: a session scoped to folders of the agent's sandbox.
func TestSandboxManager_SessionFolders(t *testing.T) {
	ctx := t.Context()
	hub, _ := hubWith(t)
	m, _ := managerFor(hub, sandboxhub.Selection{})
	full := mustSandbox(t, m) // /workspace/agents/agent-a
	for _, f := range []string{"api/main.go", "web/index.html", "other/secret.txt"} {
		if _, err := full.WriteFile(ctx, f, "x", sandboxhub.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	const api, web = "/workspace/agents/agent-a/api", "/workspace/agents/agent-a/web"

	// Validation: inside the roots, existing, folders.
	if _, err := m.SetSessionFolders(ctx, "s1", []string{"/workspace/agents/agent-b"}); sandboxErrCode(err) != kernel.SandboxCodePathEscape {
		t.Fatalf("outside the roots: %v", err)
	}
	if _, err := m.SetSessionFolders(ctx, "s1", []string{"missing"}); sandboxErrCode(err) != kernel.SandboxCodeNotFound {
		t.Fatalf("missing: %v", err)
	}
	if _, err := m.SetSessionFolders(ctx, "s1", []string{"api/main.go"}); sandboxErrCode(err) != kernel.SandboxCodeInvalidPath {
		t.Fatalf("a file: %v", err)
	}
	if _, ok := m.SessionFolders("s1"); ok {
		t.Fatal("a refused set must not store folders")
	}

	folders, err := m.SetSessionFolders(ctx, "s1", []string{"api", web})
	if err != nil || !slices.Equal(folders, []string{api, web}) {
		t.Fatalf("set = %v, %v", folders, err)
	}

	v, err := m.GetSessionSandbox(ctx, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if v.WorkingDir() != api || !slices.Equal(v.Roots(), []string{api, web}) {
		t.Fatalf("layout = %q %v", v.WorkingDir(), v.Roots())
	}
	if _, err := v.ReadFile(ctx, "/workspace/agents/agent-a/other/secret.txt", sandboxhub.ReadOptions{}); sandboxErrCode(err) != kernel.SandboxCodePathEscape {
		t.Fatalf("read outside: %v", err)
	}
	r, err := v.Exec(ctx, sandboxhub.ExecRequest{Command: []string{"pwd"}, TimeoutMs: 5000, MaxOutputBytes: 1000}, sandboxhub.ExecOptions{})
	if err != nil || strings.TrimSpace(r.Stdout) != api {
		t.Fatalf("exec = %+v, %v", r, err)
	}

	// Another session keeps the whole sandbox.
	if other, err := m.GetSessionSandbox(ctx, "s2"); err != nil || other.WorkingDir() != full.WorkingDir() {
		t.Fatalf("s2 = %v, %v", other, err)
	}

	// A subagent's run inherits the scope while it follows the parent.
	stop := m.FollowSession("child", "s1")
	child, err := m.GetSessionSandbox(ctx, "child")
	if err != nil || child.WorkingDir() != api {
		t.Fatalf("child = %v, %v", child, err)
	}
	stop()
	if child, _ := m.GetSessionSandbox(ctx, "child"); child.WorkingDir() != full.WorkingDir() {
		t.Fatal("the child keeps the scope after the run")
	}

	// Status and prompt.
	if got := m.DescribeSessionFolders("s1"); got != "Working in "+api+", "+web {
		t.Fatalf("describe = %q", got)
	}
	index, ok := m.SessionFoldersIndex("s1")
	if !ok || !strings.Contains(index, "- "+api+" (the working directory") || !strings.Contains(index, "- "+web) {
		t.Fatalf("index = %q", index)
	}

	// Clearing gives the whole sandbox back.
	if !m.ClearSessionFolders("s1") {
		t.Fatal("clear")
	}
	if v, _ := m.GetSessionSandbox(ctx, "s1"); v.WorkingDir() != full.WorkingDir() {
		t.Fatal("still scoped after clear")
	}
	if _, ok := m.SessionFoldersIndex("s1"); ok || m.DescribeSessionFolders("s1") != "" {
		t.Fatal("index/describe after clear")
	}
}
