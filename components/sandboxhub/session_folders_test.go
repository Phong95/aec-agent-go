package sandboxhub_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/Phong95/aec-agent-go/aecmock"
	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/kernel"
)

// A-216: a view scoped to session folders.

// ownView is an own-tenancy in-memory sandbox (roots /workspace) with a few folders.
func ownView(t *testing.T) (sandboxhub.View, *aecmock.InMemorySandbox) {
	t.Helper()
	p := aecmock.NewInMemoryProvider()
	sb, err := p.Create(t.Context(), sandboxhub.Spec{Kind: sandboxhub.KindDocker}, sandboxhub.CreateContext{ID: "docker:own:a1", Tenancy: sandboxhub.TenancyOwn, AgentID: "a1"})
	if err != nil {
		t.Fatal(err)
	}
	mem := sb.(*aecmock.InMemorySandbox)
	for _, f := range []string{"/workspace/api/main.go", "/workspace/api/src/app.go", "/workspace/web/index.html", "/workspace/other/secret.txt"} {
		if err := mem.PlantFile(f, "x"); err != nil {
			t.Fatal(err)
		}
	}
	v, err := sb.ForAgent(t.Context(), "a1")
	if err != nil {
		t.Fatal(err)
	}
	return v, mem
}

func sandboxCode(err error) kernel.SandboxErrorCode {
	var se *kernel.SandboxError
	if errors.As(err, &se) {
		return se.Code
	}
	return ""
}

func TestScopeLimitsPathsToTheFolders(t *testing.T) {
	ctx := t.Context()
	inner, mem := ownView(t)
	// A link inside a folder that leads out of it.
	if err := mem.Symlink("/workspace/api/escape", "/workspace/other"); err != nil {
		t.Fatal(err)
	}
	v, err := sandboxhub.Scope(inner, []string{"/workspace/api", "/workspace/web"})
	if err != nil {
		t.Fatal(err)
	}
	if v.WorkingDir() != "/workspace/api" || !slices.Equal(v.Roots(), []string{"/workspace/api", "/workspace/web"}) {
		t.Fatalf("layout = %q %v", v.WorkingDir(), v.Roots())
	}
	if v.ID() != inner.ID() || v.Traits() != inner.Traits() {
		t.Fatal("id and traits must stay the inner view's")
	}

	cases := []struct {
		name string
		path string
		want kernel.SandboxErrorCode // "" = allowed
	}{
		{"relative inside the first folder", "main.go", ""},
		{"absolute inside", "/workspace/api/main.go", ""},
		{"nested", "src/app.go", ""},
		{"second folder", "/workspace/web/index.html", ""},
		{"root itself", "/workspace/api", ""},
		{"outside", "/workspace/other/secret.txt", kernel.SandboxCodePathEscape},
		{"dot-dot", "../other/secret.txt", kernel.SandboxCodePathEscape},
		{"the workspace root", "/workspace", kernel.SandboxCodePathEscape},
		{"prefix only", "/workspace/api2/x", kernel.SandboxCodePathEscape},
		{"symlink escape", "escape/secret.txt", kernel.SandboxCodePathEscape},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := v.Stat(ctx, c.path)
			if got := sandboxCode(err); got != c.want {
				t.Fatalf("Stat(%q) code = %q, want %q (err %v)", c.path, got, c.want, err)
			}
		})
	}

	// Every file operation checks the path.
	if _, err := v.ReadFile(ctx, "/workspace/other/secret.txt", sandboxhub.ReadOptions{}); sandboxCode(err) != kernel.SandboxCodePathEscape {
		t.Fatalf("ReadFile outside: %v", err)
	}
	if _, err := v.ReadFile(ctx, "escape/secret.txt", sandboxhub.ReadOptions{}); sandboxCode(err) != kernel.SandboxCodePathEscape {
		t.Fatalf("ReadFile through the link: %v", err)
	}
	if _, err := v.WriteFile(ctx, "/workspace/other/new.txt", "x", sandboxhub.WriteOptions{}); sandboxCode(err) != kernel.SandboxCodePathEscape {
		t.Fatalf("WriteFile outside: %v", err)
	}
	if _, err := v.WriteFile(ctx, "escape/new.txt", "x", sandboxhub.WriteOptions{}); sandboxCode(err) != kernel.SandboxCodePathEscape {
		t.Fatalf("WriteFile through the link: %v", err)
	}
	if mem.Has("/workspace/other/new.txt") {
		t.Fatal("a write through the link landed outside the folders")
	}
	if _, err := v.List(ctx, "/workspace/other", sandboxhub.ListOptions{}); sandboxCode(err) != kernel.SandboxCodePathEscape {
		t.Fatalf("List outside: %v", err)
	}
	if _, err := v.Search(ctx, sandboxhub.SearchQuery{Pattern: "x", Path: "/workspace/other"}); sandboxCode(err) != kernel.SandboxCodePathEscape {
		t.Fatalf("Search outside: %v", err)
	}
	if !strings.Contains(errMessage(v.Stat(ctx, "/workspace/other"))+"", "/workspace/api, /workspace/web") {
		t.Fatal("the path_escape message lists the folders")
	}

	// Inside works, and a search without a path searches the first folder.
	if w, err := v.WriteFile(ctx, "new.txt", "hello", sandboxhub.WriteOptions{}); err != nil || w.Path != "/workspace/api/new.txt" {
		t.Fatalf("WriteFile inside = %+v, %v", w, err)
	}
	res, err := v.Search(ctx, sandboxhub.SearchQuery{Pattern: "hello"})
	if err != nil || len(res.Matches) != 1 || res.Matches[0].Path != "/workspace/api/new.txt" {
		t.Fatalf("Search = %+v, %v", res, err)
	}
	list, err := v.List(ctx, "/workspace/web", sandboxhub.ListOptions{})
	if err != nil || len(list.Entries) != 1 {
		t.Fatalf("List = %+v, %v", list, err)
	}
}

func errMessage(_ *sandboxhub.FileStat, err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func TestScopeExecStartsInTheFirstFolder(t *testing.T) {
	ctx := t.Context()
	inner, _ := ownView(t)
	v, err := sandboxhub.Scope(inner, []string{"/workspace/api", "/workspace/web"})
	if err != nil {
		t.Fatal(err)
	}
	run := func(cwd string) (sandboxhub.ExecResult, error) {
		return v.Exec(ctx, sandboxhub.ExecRequest{Command: []string{"pwd"}, Cwd: cwd, TimeoutMs: 5000, MaxOutputBytes: 1000}, sandboxhub.ExecOptions{})
	}
	if r, err := run(""); err != nil || strings.TrimSpace(r.Stdout) != "/workspace/api" {
		t.Fatalf("pwd = %+v, %v", r, err)
	}
	if r, err := run("/workspace/web"); err != nil || strings.TrimSpace(r.Stdout) != "/workspace/web" {
		t.Fatalf("pwd in web = %+v, %v", r, err)
	}
	if _, err := run("/workspace/other"); sandboxCode(err) != kernel.SandboxCodePathEscape {
		t.Fatalf("cwd outside: %v", err)
	}
}

func TestScopeRefusesFoldersOutsideTheView(t *testing.T) {
	v, err := aecmock.NewInMemoryView("docker:shared", "a1") // roots /workspace/agents/a1
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sandboxhub.Scope(v, []string{"/workspace/agents/a2"}); sandboxCode(err) != kernel.SandboxCodePathEscape {
		t.Fatalf("Scope outside the roots: %v", err)
	}
	// No folders = the view itself.
	if same, err := sandboxhub.Scope(v, nil); err != nil || same != v {
		t.Fatal("Scope with no folders must return the view")
	}
}

func TestNormalizeSessionFolders(t *testing.T) {
	got, err := sandboxhub.NormalizeSessionFolders([]string{"api", "/workspace/web/", "/workspace/api"}, "/workspace", []string{"/workspace"})
	if err != nil || !slices.Equal(got, []string{"/workspace/api", "/workspace/web"}) {
		t.Fatalf("got %v, %v", got, err)
	}
	if _, err := sandboxhub.NormalizeSessionFolders(nil, "", nil); sandboxCode(err) != kernel.SandboxCodePolicyDenied {
		t.Fatalf("no folders: %v", err)
	}
	if _, err := sandboxhub.NormalizeSessionFolders([]string{"../x"}, "/workspace", nil); sandboxCode(err) != kernel.SandboxCodePathEscape {
		t.Fatalf("outside: %v", err)
	}
	many := make([]string, sandboxhub.MaxSessionFolders+1)
	for i := range many {
		many[i] = "/workspace/f" + string(rune('a'+i))
	}
	if _, err := sandboxhub.NormalizeSessionFolders(many, "", nil); sandboxCode(err) != kernel.SandboxCodePolicyDenied {
		t.Fatalf("too many: %v", err)
	}
}

func TestHubSessionFolders(t *testing.T) {
	bus := events.New()
	var got []events.SandboxSessionFoldersChangedPayload
	events.Subscribe(bus, events.SandboxSessionFoldersChanged, func(_ kernel.Event, p events.SandboxSessionFoldersChangedPayload) {
		got = append(got, p)
	})
	hub := sandboxhub.NewSandboxHub(sandboxhub.WithEvents(bus))
	t.Cleanup(func() { _ = hub.Dispose(context.Background()) })

	if _, ok := hub.SessionFolders("a1", "s1"); ok {
		t.Fatal("no folders yet")
	}
	folders, err := hub.SetSessionFolders("a1", "s1", []string{"/workspace/api", "/workspace/api"})
	if err != nil || !slices.Equal(folders, []string{"/workspace/api"}) {
		t.Fatalf("set = %v, %v", folders, err)
	}
	if _, err := hub.SetSessionFolders("a1", "s1", []string{"relative"}); err != nil {
		// relative resolves against /workspace
		t.Fatal(err)
	}
	if f, _ := hub.SessionFolders("a1", "s1"); !slices.Equal(f, []string{"/workspace/relative"}) {
		t.Fatalf("folders = %v", f)
	}
	if _, ok := hub.SessionFolders("a1", "s2"); ok {
		t.Fatal("folders are per session")
	}
	if !hub.ClearSessionFolders("a1", "s1") || hub.ClearSessionFolders("a1", "s1") {
		t.Fatal("clear reports whether there were folders")
	}
	if len(got) != 3 || len(got[2].Folders) != 0 || got[0].Folders[0] != "/workspace/api" {
		t.Fatalf("events = %+v", got)
	}

	// A removed agent's sessions lose their folders.
	if _, err := hub.SetSessionFolders("a1", "s3", []string{"/workspace/api"}); err != nil {
		t.Fatal(err)
	}
	events.Publish(t.Context(), bus, events.AgentRemoved, "", events.AgentRemovedPayload{AgentID: "a1"})
	if _, ok := hub.SessionFolders("a1", "s3"); ok {
		t.Fatal("folders kept after the agent was removed")
	}
}
