package docker

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/kernel"
)

// docker-view-transcripts.json is recorded by wiregen from a reference sandbox over a
// recording cli: agent folders (created once per agent and container run,
// removed on request), and the five file operations with their defaults, clamps and
// host-side refusals. The view must send helper requests that decode to the same JSON
// (the helper ignores key order) and return the same results and errors; a request that
// is refused on the host (path escape, NUL, too large, unsafe agent id) must make no
// docker call at all. The steps and the replayer are shared with TestTranscriptsMatchGolden.
func TestViewTranscriptsMatchGolden(t *testing.T) {
	for _, sc := range loadScenarios(t, "docker-view-transcripts.json") {
		t.Run(sc.Name, func(t *testing.T) {
			newReplayer(t, sc).run()
		})
	}
}

// startedSandbox is a sandbox over a fake runner that has been created and has a
// shared agent view. Every later call needs a scripted reply.
func startedSandbox(t *testing.T, tenancy sandboxhub.Tenancy) (*fakeRunner, *Sandbox, sandboxhub.View) {
	t.Helper()
	runner := newFakeRunner(t)
	provider, err := New(Options{Runner: runner})
	if err != nil {
		t.Fatal(err)
	}
	cc := sandboxhub.CreateContext{ID: "docker:shared", Tenancy: sandboxhub.TenancyShared}
	if tenancy == sandboxhub.TenancyOwn {
		cc = sandboxhub.CreateContext{ID: "docker:own:a1", Tenancy: sandboxhub.TenancyOwn, AgentID: "a1"}
	}
	zero, one := 0, 1
	ok := reply{Code: &zero}
	helperOK := reply{Code: &zero, Stdout: `{"ok":true,"result":{}}` + "\n"}
	runner.script([]reply{
		{Code: &zero, Stdout: "27.0.1\n"}, ok, {Code: &one, Stderr: "No such container"}, ok, ok, helperOK,
	})
	sb, err := provider.Create(context.Background(), sandboxhub.Spec{Kind: sandboxhub.KindDocker}, cc)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	runner.script([]reply{helperOK})
	v, err := sb.ForAgent(context.Background(), "a1")
	if err != nil {
		t.Fatalf("ForAgent: %v", err)
	}
	runner.script(nil)
	return runner, sb.(*Sandbox), v
}

// TestViewRejectsBeforeCallingDocker: what the host can refuse never reaches docker,
// and the runner fails the test on any call.
func TestViewRejectsBeforeCallingDocker(t *testing.T) {
	runner, sb, v := startedSandbox(t, sandboxhub.TenancyShared)
	ctx := context.Background()
	big := strings.Repeat("x", 5_000_001)

	cases := []struct {
		name string
		code kernel.SandboxErrorCode
		call func() error
	}{
		{"read escape", kernel.SandboxCodePathEscape, func() error { _, err := v.ReadFile(ctx, "../a2/x", sandboxhub.ReadOptions{}); return err }},
		{"read absolute", kernel.SandboxCodePathEscape, func() error { _, err := v.ReadFile(ctx, "/etc/passwd", sandboxhub.ReadOptions{}); return err }},
		{"read host path", kernel.SandboxCodePathEscape, func() error { _, err := v.ReadFile(ctx, `C:\Windows\win.ini`, sandboxhub.ReadOptions{}); return err }},
		{"read NUL", kernel.SandboxCodeInvalidPath, func() error { _, err := v.ReadFile(ctx, "a\x00b", sandboxhub.ReadOptions{}); return err }},
		{"write too large", kernel.SandboxCodeTooLarge, func() error { _, err := v.WriteFile(ctx, "a.txt", big, sandboxhub.WriteOptions{}); return err }},
		{"write escape", kernel.SandboxCodePathEscape, func() error { _, err := v.WriteFile(ctx, "../x", "x", sandboxhub.WriteOptions{}); return err }},
		{"stat escape", kernel.SandboxCodePathEscape, func() error { _, err := v.Stat(ctx, "/workspace"); return err }},
		{"list escape", kernel.SandboxCodePathEscape, func() error { _, err := v.List(ctx, "/workspace", sandboxhub.ListOptions{}); return err }},
		{"search empty pattern", kernel.SandboxCodeInvalidArgument, func() error { _, err := v.Search(ctx, sandboxhub.SearchQuery{}); return err }},
		{"search escape", kernel.SandboxCodePathEscape, func() error {
			_, err := v.Search(ctx, sandboxhub.SearchQuery{Pattern: "x", Path: "../a2"})
			return err
		}},
		{"unsafe agent id", kernel.SandboxCodePolicyDenied, func() error { _, err := sb.ForAgent(ctx, "../x"); return err }},
		{"unsafe agent id on remove", kernel.SandboxCodePolicyDenied, func() error { return sb.RemoveAgentData(ctx, "a/b") }},
		// Exec validates on the host before any client is started (the exec stream itself
		// is covered by exec_stream_test.go).
		{"exec empty command", kernel.SandboxCodeInvalidArgument, func() error {
			_, err := v.Exec(ctx, sandboxhub.ExecRequest{TimeoutMs: 1000, MaxOutputBytes: 10}, sandboxhub.ExecOptions{})
			return err
		}},
		{"exec escape cwd", kernel.SandboxCodePathEscape, func() error {
			_, err := v.Exec(ctx, sandboxhub.ExecRequest{Command: []string{"echo"}, Cwd: "../a2", TimeoutMs: 1000, MaxOutputBytes: 10}, sandboxhub.ExecOptions{})
			return err
		}},
	}
	for _, c := range cases {
		err := c.call()
		var se *kernel.SandboxError
		if !errors.As(err, &se) || se.Code != c.code {
			t.Errorf("%s: error = %v, want code %s", c.name, err, c.code)
		}
	}
	if n := len(runner.recorded()); n != 0 {
		t.Errorf("%d docker calls were made, want none: %v", n, callArgs(runner.recorded()))
	}

	// A cancelled context is refused before the call, with the cancelled code.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := v.ReadFile(cancelled, "a.txt", sandboxhub.ReadOptions{}); !hasCode(err, kernel.SandboxCodeCancelled) {
		t.Errorf("cancelled read error = %v", err)
	}
	if n := len(runner.recorded()); n != 0 {
		t.Errorf("a cancelled read made %d docker calls", n)
	}
}

func hasCode(err error, code kernel.SandboxErrorCode) bool {
	var se *kernel.SandboxError
	return errors.As(err, &se) && se.Code == code
}

// TestHelperAnswersDecodeLeniently: the helper is a separate program that may gain
// fields, so an answer with unknown fields still decodes (the Go types hold what they
// know); an answer of the wrong shape is an error, not a panic.
func TestHelperAnswersDecodeLeniently(t *testing.T) {
	runner, _, v := startedSandbox(t, sandboxhub.TenancyShared)
	zero := 0
	answer := func(result string) {
		runner.script([]reply{{Code: &zero, Stdout: `{"ok":true,"result":` + result + `,"future":1}` + "\n"}})
	}
	ctx := context.Background()

	answer(`{"path":"/workspace/agents/a1/a","content":"hi","size":2,"truncated":false,"sha256":"abc","encoding":"utf8"}`)
	got, err := v.ReadFile(ctx, "a", sandboxhub.ReadOptions{})
	if err != nil || got.Content != "hi" || got.Size != 2 || got.SHA256 != "abc" {
		t.Errorf("ReadFile = %+v, %v", got, err)
	}

	answer(`{"path":"/workspace/agents/a1/a","type":"file","size":2,"modifiedAt":1700000000000,"mode":"0644"}`)
	st, err := v.Stat(ctx, "a")
	if err != nil || st == nil || st.Type != sandboxhub.EntryFile || st.ModifiedAt != 1700000000000 {
		t.Errorf("Stat = %+v, %v", st, err)
	}

	answer(`null`)
	if st, err := v.Stat(ctx, "a"); err != nil || st != nil {
		t.Errorf("Stat of a missing path = %+v, %v", st, err)
	}

	answer(`{"entries":[{"path":"/p","name":"p","type":"file","size":1,"inode":7}],"truncated":false}`)
	list, err := v.List(ctx, ".", sandboxhub.ListOptions{})
	if err != nil || len(list.Entries) != 1 || list.Entries[0].Name != "p" {
		t.Errorf("List = %+v, %v", list, err)
	}

	answer(`{"matches":[{"path":"/p","line":3,"text":"t","column":2}],"truncated":true}`)
	found, err := v.Search(ctx, sandboxhub.SearchQuery{Pattern: "t"})
	if err != nil || len(found.Matches) != 1 || found.Matches[0].Line != 3 || !found.Truncated {
		t.Errorf("Search = %+v, %v", found, err)
	}

	answer(`{"path":"/p","bytes":1,"sha256":"s","created":true,"mtime":5}`)
	wrote, err := v.WriteFile(ctx, "p", "x", sandboxhub.WriteOptions{})
	if err != nil || wrote.Bytes != 1 || !wrote.Created {
		t.Errorf("WriteFile = %+v, %v", wrote, err)
	}

	// The wrong shape: size is a string.
	answer(`{"path":"/p","content":"","size":"big"}`)
	if _, err := v.ReadFile(ctx, "a", sandboxhub.ReadOptions{}); !hasCode(err, kernel.SandboxCodeEnvironmentUnavailable) {
		t.Errorf("a misshapen answer gave %v, want environment_unavailable", err)
	}
}

// TestViewShape: capabilities, limits and layout of a view.
func TestViewShape(t *testing.T) {
	_, sb, v := startedSandbox(t, sandboxhub.TenancyShared)
	if !slices.Equal(v.Capabilities(), []sandboxhub.Capability{"fs.read", "fs.write", "fs.search", "exec", "exec.shell", "fs.stream", "services"}) {
		t.Errorf("capabilities = %v", v.Capabilities())
	}
	if v.Limits() != DefaultLimits() {
		t.Errorf("limits = %+v", v.Limits())
	}
	if v.ID() != "docker:shared" || v.WorkingDir() != "/workspace/agents/a1" || !slices.Equal(v.Roots(), []string{"/workspace/agents/a1"}) {
		t.Errorf("view = %s %s %v", v.ID(), v.WorkingDir(), v.Roots())
	}
	roots := v.Roots()
	roots[0] = "/changed"
	if v.Roots()[0] != "/workspace/agents/a1" {
		t.Errorf("Roots returned the internal slice")
	}
	tr := sb.Traits()
	if tr.Kind != sandboxhub.KindDocker || !tr.Isolated || !tr.Persistent || tr.Network != sandboxhub.NetworkNone || tr.OS != sandboxhub.OSLinux || tr.Tenancy != sandboxhub.TenancyShared {
		t.Errorf("traits = %+v", tr)
	}
}
