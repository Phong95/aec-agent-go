package host

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/kernel"
)

// A-153: a folder on the user's machine, reached through a client app's runner. The
// server side (sandboxhub.RunnerSandbox) and the client side (sandboxhub.ServeRunner over
// OpenRunnerFolder) are wired back to back here through JSON; the host's real transport
// (a WebSocket) only carries the bytes.

type runnerPair struct {
	sb     *sandboxhub.RunnerSandbox
	handle *sandboxhub.RunnerHandle
	view   sandboxhub.View
	// local (A-159) is the runner's folders, on the client side.
	local *sandboxhub.RunnerFolders
	// dropped makes the server's messages never reach the runner.
	dropped bool
}

// sendToServer (A-159) carries one runner message to the server side through JSON.
func (p *runnerPair) sendToServer(t *testing.T, m sandboxhub.RunnerMessage) {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	p.sb.Receive(b)
}

// rootOf (A-159) is a folder's place in the runner's workspace, /workspace/<its name>.
func rootOf(folder string) string { return sandboxhub.WorkspaceRoot + "/" + filepath.Base(folder) }

func runnerFolder(t *testing.T) string {
	t.Helper()
	folder := t.TempDir()
	if err := os.WriteFile(filepath.Join(folder, "note.txt"), []byte("hello from the user's machine\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(folder, "src"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(folder, "src", "app.ts"), []byte("export const answer = 42;\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return folder
}

func connectRunner(t *testing.T, folder string, allowExec, drop bool, extra ...sandboxhub.RunnerFolderSpec) *runnerPair {
	t.Helper()
	specs := append([]sandboxhub.RunnerFolderSpec{{Path: folder, DenyExec: !allowExec}}, extra...)
	local, err := OpenRunnerFolders(t.Context(), specs, nil)
	if err != nil {
		t.Fatal(err)
	}
	helloJSON, err := json.Marshal(sandboxhub.RunnerHelloFor(local))
	if err != nil {
		t.Fatal(err)
	}
	hello, err := sandboxhub.ParseRunnerHello(helloJSON)
	if err != nil {
		t.Fatal(err)
	}
	p := &runnerPair{dropped: drop, local: local}
	p.sb = sandboxhub.NewRunnerSandbox(sandboxhub.RunnerSandboxOptions{
		ID:    "remote:runner:s1",
		Hello: hello,
		Send: func(m sandboxhub.RunnerMessage) error {
			if p.dropped {
				return nil
			}
			b, err := json.Marshal(m)
			if err != nil {
				return err
			}
			p.handle.Receive(b)
			return nil
		},
	})
	p.handle = sandboxhub.ServeRunner(local, func(m sandboxhub.RunnerMessage) error {
		b, err := json.Marshal(m)
		if err != nil {
			return err
		}
		p.sb.Receive(b)
		return nil
	}, sandboxhub.ServeRunnerOptions{DenyExec: !allowExec})
	t.Cleanup(p.handle.Close)
	p.view, err = p.sb.ForAgent(t.Context(), "agent-1")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func wantSandboxCode(t *testing.T, err error, code kernel.SandboxErrorCode) {
	t.Helper()
	var se *kernel.SandboxError
	if !errors.As(err, &se) || se.Code != code {
		t.Fatalf("error = %v, want a SandboxError %q", err, code)
	}
}

func TestRunnerFilesInTheChosenFolder(t *testing.T) {
	folder := runnerFolder(t)
	w := rootOf(folder)
	p := connectRunner(t, folder, true, false)
	ctx := t.Context()
	if tr := p.view.Traits(); tr.Kind != sandboxhub.KindRemote || tr.Isolated || tr.Tenancy != sandboxhub.TenancyOwn {
		t.Fatalf("traits = %+v", tr)
	}
	if p.view.WorkingDir() != w {
		t.Fatalf("working dir = %q", p.view.WorkingDir())
	}

	read, err := p.view.ReadFile(ctx, "note.txt", sandboxhub.ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if read.Content != "hello from the user's machine\n" || read.Path != w+"/note.txt" {
		t.Fatalf("read = %+v", read)
	}

	written, err := p.view.WriteFile(ctx, "out/result.md", "# done\n", sandboxhub.WriteOptions{})
	if err != nil || !written.Created {
		t.Fatalf("write = %+v, %v", written, err)
	}
	back, err := p.view.ReadFile(ctx, w+"/out/result.md", sandboxhub.ReadOptions{})
	if err != nil || back.Content != "# done\n" {
		t.Fatalf("read back = %+v, %v", back, err)
	}

	recursive := true
	listed, err := p.view.List(ctx, ".", sandboxhub.ListOptions{Recursive: &recursive})
	if err != nil {
		t.Fatal(err)
	}
	paths := map[string]bool{}
	for _, e := range listed.Entries {
		paths[e.Path] = true
	}
	if !paths[w+"/note.txt"] || !paths[w+"/src/app.ts"] {
		t.Fatalf("listed %v", paths)
	}

	found, err := p.view.Search(ctx, sandboxhub.SearchQuery{Pattern: "answer", Path: "src"})
	if err != nil || len(found.Matches) != 1 || found.Matches[0].Path != w+"/src/app.ts" || found.Matches[0].Line != 1 {
		t.Fatalf("search = %+v, %v", found, err)
	}

	if st, err := p.view.Stat(ctx, "missing.txt"); err != nil || st != nil {
		t.Fatalf("stat missing = %+v, %v", st, err)
	}
	if st, err := p.view.Stat(ctx, "note.txt"); err != nil || st == nil || st.Type != sandboxhub.EntryFile {
		t.Fatalf("stat note = %+v, %v", st, err)
	}
}

func TestRunnerKeepsFilesInsideTheFolder(t *testing.T) {
	p := connectRunner(t, runnerFolder(t), true, false)
	_, err := p.view.ReadFile(t.Context(), "../outside.txt", sandboxhub.ReadOptions{})
	wantSandboxCode(t, err, kernel.SandboxCodePathEscape)
	_, err = p.view.WriteFile(t.Context(), "/etc/passwd", "x", sandboxhub.WriteOptions{})
	wantSandboxCode(t, err, kernel.SandboxCodePathEscape)
	_, err = p.view.ReadFile(t.Context(), "/workspace/note.txt", sandboxhub.ReadOptions{})
	wantSandboxCode(t, err, kernel.SandboxCodePathEscape)
}

func TestRunnerRunsCommandsInTheFolderAndStreamsOutput(t *testing.T) {
	folder := runnerFolder(t)
	p := connectRunner(t, folder, true, false)
	var mu sync.Mutex
	var streamed strings.Builder
	res, err := p.view.Exec(t.Context(), helperRequest(t, "cwd", nil, 20_000), sandboxhub.ExecOptions{OnOutput: func(_ sandboxhub.OutputStream, data string) {
		mu.Lock()
		streamed.WriteString(data)
		mu.Unlock()
	}})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode == nil || *res.ExitCode != 0 {
		t.Fatalf("exit = %+v", res)
	}
	want, _ := filepath.EvalSymlinks(folder)
	got, _ := filepath.EvalSymlinks(res.Stdout)
	if !strings.EqualFold(got, want) {
		t.Fatalf("cwd = %q, want %q", res.Stdout, folder)
	}
	mu.Lock()
	defer mu.Unlock()
	if streamed.String() != res.Stdout {
		t.Fatalf("streamed %q, result %q", streamed.String(), res.Stdout)
	}
}

func TestRunnerStopsACancelledCommand(t *testing.T) {
	p := connectRunner(t, runnerFolder(t), true, false)
	ctx, cancel := contextWithCancel(t)
	time.AfterFunc(300*time.Millisecond, cancel)
	res, err := p.view.Exec(ctx, helperRequest(t, "sleep-forever", nil, 60_000), sandboxhub.ExecOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Cancelled || res.ExitCode != nil {
		t.Fatalf("result = %+v, want cancelled", res)
	}
}

func TestRunnerWithoutCommands(t *testing.T) {
	p := connectRunner(t, runnerFolder(t), false, false)
	for _, c := range p.view.Capabilities() {
		if c == sandboxhub.CapabilityExec {
			t.Fatal("exec offered although commands are off")
		}
	}
	_, err := p.view.Exec(t.Context(), helperRequest(t, "exit3", nil, 5_000), sandboxhub.ExecOptions{})
	wantSandboxCode(t, err, kernel.SandboxCodeCapabilityUnsupported)

	// The runner refuses them too, whatever the server asks.
	local, err := OpenRunnerFolder(t.Context(), runnerFolder(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	replies := make(chan sandboxhub.RunnerMessage, 1)
	h := sandboxhub.ServeRunner(local, func(m sandboxhub.RunnerMessage) error { replies <- m; return nil }, sandboxhub.ServeRunnerOptions{DenyExec: true})
	defer h.Close()
	req, _ := json.Marshal(helperRequest(t, "exit3", nil, 5_000))
	msg, _ := json.Marshal(sandboxhub.RunnerMessage{Type: "request", ID: "1", Op: sandboxhub.RunnerExec, Args: []json.RawMessage{req}})
	h.Receive(msg)
	select {
	case m := <-replies:
		if m.Type != "error" || m.Code != kernel.SandboxCodePolicyDenied {
			t.Fatalf("reply = %+v", m)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no reply")
	}
}

func TestRunnerFailsCallsOnceTheConnectionDrops(t *testing.T) {
	p := connectRunner(t, runnerFolder(t), true, true)
	errs := make(chan error, 1)
	go func() {
		_, err := p.view.ReadFile(t.Context(), "note.txt", sandboxhub.ReadOptions{})
		errs <- err
	}()
	time.Sleep(50 * time.Millisecond)
	p.sb.Close("")
	wantSandboxCode(t, <-errs, kernel.SandboxCodeEnvironmentUnavailable)
	_, err := p.view.ReadFile(t.Context(), "note.txt", sandboxhub.ReadOptions{})
	wantSandboxCode(t, err, kernel.SandboxCodeEnvironmentUnavailable)
	if p.sb.Health(t.Context()) != sandboxhub.HealthUnavailable {
		t.Fatal("a closed runner is not unavailable")
	}
}

func TestRunnerEndsANonExecCallWhenCancelled(t *testing.T) {
	p := connectRunner(t, runnerFolder(t), true, true)
	ctx, cancel := contextWithCancel(t)
	time.AfterFunc(50*time.Millisecond, cancel)
	_, err := p.view.ReadFile(ctx, "note.txt", sandboxhub.ReadOptions{})
	wantSandboxCode(t, err, kernel.SandboxCodeCancelled)
}

func TestOpenRunnerFolderNeedsAnExistingAbsoluteFolder(t *testing.T) {
	_, err := OpenRunnerFolder(t.Context(), "relative/dir", nil)
	wantSandboxCode(t, err, kernel.SandboxCodeInvalidPath)
	_, err = OpenRunnerFolder(t.Context(), filepath.Join(runnerFolder(t), "note.txt"), nil)
	wantSandboxCode(t, err, kernel.SandboxCodeInvalidPath)
	_, err = OpenRunnerFolders(t.Context(), nil, nil)
	wantSandboxCode(t, err, kernel.SandboxCodeInvalidArgument)
}

// --- A-159: a runner with several folders -----------------------------------------

func secondFolder(t *testing.T) string {
	t.Helper()
	folder := t.TempDir()
	if err := os.WriteFile(filepath.Join(folder, "guide.md"), []byte("# the second folder\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return folder
}

func TestRunnerStillServesProtocol1(t *testing.T) {
	local, err := OpenRunnerFolder(t.Context(), runnerFolder(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	hello, err := sandboxhub.ParseRunnerHello([]byte(`{"type":"hello","protocol":1,"os":"` + string(local.Traits().OS) + `","folderName":"my-app","allowExec":true}`))
	if err != nil {
		t.Fatal(err)
	}
	var handle *sandboxhub.RunnerHandle
	sb := sandboxhub.NewRunnerSandbox(sandboxhub.RunnerSandboxOptions{ID: "remote:runner:v1", Hello: hello, Send: func(m sandboxhub.RunnerMessage) error {
		b, _ := json.Marshal(m)
		handle.Receive(b)
		return nil
	}})
	handle = sandboxhub.ServeRunner(local, func(m sandboxhub.RunnerMessage) error {
		b, _ := json.Marshal(m)
		sb.Receive(b)
		return nil
	}, sandboxhub.ServeRunnerOptions{})
	t.Cleanup(handle.Close)
	view, err := sb.ForAgent(t.Context(), "agent-1")
	if err != nil {
		t.Fatal(err)
	}
	if sb.Layout() != sandboxhub.RunnerLayoutFlat || view.WorkingDir() != sandboxhub.WorkspaceRoot {
		t.Fatalf("layout %q, working dir %q", sb.Layout(), view.WorkingDir())
	}
	read, err := view.ReadFile(t.Context(), "note.txt", sandboxhub.ReadOptions{})
	if err != nil || read.Path != "/workspace/note.txt" {
		t.Fatalf("read = %+v, %v", read, err)
	}
	if f := sb.Folders(); len(f) != 1 || f[0] != (sandboxhub.RunnerFolder{Name: "my-app", AllowExec: true}) {
		t.Fatalf("folders = %+v", f)
	}
}

func TestRunnerPutsEachFolderAtItsName(t *testing.T) {
	folder, second := runnerFolder(t), secondFolder(t)
	w, d := rootOf(folder), rootOf(second)
	p := connectRunner(t, folder, true, false, sandboxhub.RunnerFolderSpec{Path: second})
	ctx := t.Context()
	if p.sb.Layout() != sandboxhub.RunnerLayoutFolders || p.view.WorkingDir() != w || strings.Join(p.view.Roots(), ",") != w+","+d {
		t.Fatalf("layout %q, working dir %q, roots %v", p.sb.Layout(), p.view.WorkingDir(), p.view.Roots())
	}
	if r, err := p.view.ReadFile(ctx, "note.txt", sandboxhub.ReadOptions{}); err != nil || r.Path != w+"/note.txt" {
		t.Fatalf("read = %+v, %v", r, err)
	}
	guide, err := p.view.ReadFile(ctx, d+"/guide.md", sandboxhub.ReadOptions{})
	if err != nil || guide.Path != d+"/guide.md" || guide.Content != "# the second folder\n" {
		t.Fatalf("guide = %+v, %v", guide, err)
	}
	// A relative path may step into a sibling folder of the workspace.
	if r, err := p.view.ReadFile(ctx, "../"+filepath.Base(second)+"/guide.md", sandboxhub.ReadOptions{}); err != nil || r.Path != d+"/guide.md" {
		t.Fatalf("relative read = %+v, %v", r, err)
	}
	if _, err := p.view.WriteFile(ctx, d+"/new/plan.md", "plan", sandboxhub.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if st, err := p.view.Stat(ctx, d+"/new/plan.md"); err != nil || st == nil || st.Path != d+"/new/plan.md" {
		t.Fatalf("stat = %+v, %v", st, err)
	}
	listed, err := p.view.List(ctx, d, sandboxhub.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	paths := map[string]bool{}
	for _, e := range listed.Entries {
		paths[e.Path] = true
	}
	if !paths[d+"/guide.md"] || !paths[d+"/new"] {
		t.Fatalf("listed %v", paths)
	}
	found, err := p.view.Search(ctx, sandboxhub.SearchQuery{Pattern: "second", Path: d})
	if err != nil || len(found.Matches) != 1 || found.Matches[0].Path != d+"/guide.md" {
		t.Fatalf("search = %+v, %v", found, err)
	}
}

func TestRunnerFoldersKeepPathsInsideAndNameThemInErrors(t *testing.T) {
	second := secondFolder(t)
	d := rootOf(second)
	p := connectRunner(t, runnerFolder(t), true, false, sandboxhub.RunnerFolderSpec{Path: second})
	_, err := p.view.ReadFile(t.Context(), d+"/../../etc/passwd", sandboxhub.ReadOptions{})
	wantSandboxCode(t, err, kernel.SandboxCodePathEscape)
	_, err = p.view.ReadFile(t.Context(), "/workspace/elsewhere/x.txt", sandboxhub.ReadOptions{})
	wantSandboxCode(t, err, kernel.SandboxCodePathEscape)
	_, err = p.view.ReadFile(t.Context(), d+"/missing.md", sandboxhub.ReadOptions{})
	wantSandboxCode(t, err, kernel.SandboxCodeNotFound)
	if strings.Contains(err.Error(), `"/workspace/missing.md"`) {
		t.Fatalf("error names the folder's own path: %v", err)
	}
}

func TestRunnerFoldersRunCommandsWhereTheyAreOn(t *testing.T) {
	folder, second := runnerFolder(t), secondFolder(t)
	p := connectRunner(t, folder, true, false, sandboxhub.RunnerFolderSpec{Path: second, DenyExec: true})
	res, err := p.view.Exec(t.Context(), helperRequest(t, "cwd", nil, 20_000), sandboxhub.ExecOptions{})
	if err != nil {
		t.Fatal(err)
	}
	want, _ := filepath.EvalSymlinks(folder)
	got, _ := filepath.EvalSymlinks(res.Stdout)
	if !strings.EqualFold(got, want) {
		t.Fatalf("cwd = %q, want %q", res.Stdout, folder)
	}
	req := helperRequest(t, "cwd", nil, 20_000)
	req.Cwd = rootOf(second)
	_, err = p.view.Exec(t.Context(), req, sandboxhub.ExecOptions{})
	wantSandboxCode(t, err, kernel.SandboxCodePolicyDenied)

	// Commands are offered while any folder allows them.
	off := connectRunner(t, runnerFolder(t), false, false, sandboxhub.RunnerFolderSpec{Path: secondFolder(t), DenyExec: true})
	for _, c := range off.view.Capabilities() {
		if c == sandboxhub.CapabilityExec {
			t.Fatal("exec offered although commands are off everywhere")
		}
	}
	on := connectRunner(t, runnerFolder(t), true, false, sandboxhub.RunnerFolderSpec{Path: second})
	req.Cwd = rootOf(second)
	res, err = on.view.Exec(t.Context(), req, sandboxhub.ExecOptions{})
	if err != nil {
		t.Fatal(err)
	}
	want, _ = filepath.EvalSymlinks(second)
	got, _ = filepath.EvalSymlinks(res.Stdout)
	if !strings.EqualFold(got, want) {
		t.Fatalf("cwd = %q, want %q", res.Stdout, second)
	}
}

func TestRunnerFoldersNameApartAndRefuseDuplicates(t *testing.T) {
	folder, second := runnerFolder(t), secondFolder(t)
	nested := filepath.Join(second, filepath.Base(folder))
	if err := os.Mkdir(nested, 0o750); err != nil {
		t.Fatal(err)
	}
	local, err := OpenRunnerFolders(t.Context(), []sandboxhub.RunnerFolderSpec{{Path: folder}, {Path: nested}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Base(folder)
	if f := local.Folders(); len(f) != 2 || f[0].Name != base || f[1].Name != base+"-2" {
		t.Fatalf("folders = %+v", f)
	}
	_, err = local.Add(t.Context(), sandboxhub.RunnerFolderSpec{Path: folder})
	wantSandboxCode(t, err, kernel.SandboxCodeConflict)
	if ok, err := local.Remove("nope"); ok || err != nil {
		t.Fatalf("remove nope = %v, %v", ok, err)
	}
	if ok, err := local.Remove(base + "-2"); !ok || err != nil {
		t.Fatalf("remove = %v, %v", ok, err)
	}
	_, err = local.Remove(base)
	wantSandboxCode(t, err, kernel.SandboxCodeInvalidArgument)
}

func TestRunnerFollowsFoldersAddedAndRemoved(t *testing.T) {
	folder, second := runnerFolder(t), secondFolder(t)
	w, d := rootOf(folder), rootOf(second)
	p := connectRunner(t, folder, true, false)
	hub := sandboxhub.NewSandboxHub()
	if _, err := hub.AttachRunner("agent-1", "s1", p.sb); err != nil {
		t.Fatal(err)
	}
	var changes []int
	stop := p.sb.OnFoldersChange(func(f []sandboxhub.RunnerFolder) { changes = append(changes, len(f)) })
	defer stop()

	if _, err := p.local.Add(t.Context(), sandboxhub.RunnerFolderSpec{Path: second, DenyExec: true}); err != nil {
		t.Fatal(err)
	}
	p.sendToServer(t, sandboxhub.RunnerFoldersUpdateFor(p.local))
	if len(changes) != 1 || changes[0] != 2 {
		t.Fatalf("changes = %v", changes)
	}
	info, _ := hub.RunnerInfo("agent-1", "s1")
	wantFolders := []sandboxhub.RunnerFolder{{Name: filepath.Base(folder), AllowExec: true}, {Name: filepath.Base(second), AllowExec: false}}
	if info.FolderName != filepath.Base(folder) || !info.AllowExec || len(info.Folders) != 2 || info.Folders[0] != wantFolders[0] || info.Folders[1] != wantFolders[1] {
		t.Fatalf("info = %+v", info)
	}
	view, _ := p.sb.ForAgent(t.Context(), "agent-1")
	if strings.Join(view.Roots(), ",") != w+","+d {
		t.Fatalf("roots = %v", view.Roots())
	}
	if r, err := view.ReadFile(t.Context(), d+"/guide.md", sandboxhub.ReadOptions{}); err != nil || !strings.Contains(r.Content, "second folder") {
		t.Fatalf("guide = %+v, %v", r, err)
	}

	if _, err := p.local.Remove(filepath.Base(folder)); err != nil {
		t.Fatal(err)
	}
	p.sendToServer(t, sandboxhub.RunnerFoldersUpdateFor(p.local))
	after, _ := p.sb.ForAgent(t.Context(), "agent-1")
	if after.WorkingDir() != d {
		t.Fatalf("working dir = %q", after.WorkingDir())
	}
	for _, c := range after.Capabilities() {
		if c == sandboxhub.CapabilityExec {
			t.Fatal("exec offered although the only folder has commands off")
		}
	}
	_, err := after.ReadFile(t.Context(), w+"/note.txt", sandboxhub.ReadOptions{})
	wantSandboxCode(t, err, kernel.SandboxCodePathEscape)

	// A bad list is ignored.
	p.sb.Receive([]byte(`{"type":"folders","folders":[{"name":"../up","allowExec":true}]}`))
	if f := p.sb.Folders(); len(f) != 1 || f[0].Name != filepath.Base(second) {
		t.Fatalf("folders = %+v", f)
	}
}

// contextWithCancel is the test's context with a cancel function, cancelled at cleanup too.
func contextWithCancel(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	return ctx, cancel
}
