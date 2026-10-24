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
	// dropped makes the server's messages never reach the runner.
	dropped bool
}

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

func connectRunner(t *testing.T, folder string, allowExec, drop bool) *runnerPair {
	t.Helper()
	local, err := OpenRunnerFolder(t.Context(), folder, nil)
	if err != nil {
		t.Fatal(err)
	}
	helloJSON, err := json.Marshal(sandboxhub.RunnerHelloFor(local, filepath.Base(folder), allowExec))
	if err != nil {
		t.Fatal(err)
	}
	hello, err := sandboxhub.ParseRunnerHello(helloJSON)
	if err != nil {
		t.Fatal(err)
	}
	p := &runnerPair{dropped: drop}
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
	p := connectRunner(t, runnerFolder(t), true, false)
	ctx := t.Context()
	if tr := p.view.Traits(); tr.Kind != sandboxhub.KindRemote || tr.Isolated || tr.Tenancy != sandboxhub.TenancyOwn {
		t.Fatalf("traits = %+v", tr)
	}
	if p.view.WorkingDir() != sandboxhub.WorkspaceRoot {
		t.Fatalf("working dir = %q", p.view.WorkingDir())
	}

	read, err := p.view.ReadFile(ctx, "note.txt", sandboxhub.ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if read.Content != "hello from the user's machine\n" || read.Path != "/workspace/note.txt" {
		t.Fatalf("read = %+v", read)
	}

	written, err := p.view.WriteFile(ctx, "out/result.md", "# done\n", sandboxhub.WriteOptions{})
	if err != nil || !written.Created {
		t.Fatalf("write = %+v, %v", written, err)
	}
	back, err := p.view.ReadFile(ctx, "/workspace/out/result.md", sandboxhub.ReadOptions{})
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
	if !paths["/workspace/note.txt"] || !paths["/workspace/src/app.ts"] {
		t.Fatalf("listed %v", paths)
	}

	found, err := p.view.Search(ctx, sandboxhub.SearchQuery{Pattern: "answer", Path: "src"})
	if err != nil || len(found.Matches) != 1 || found.Matches[0].Path != "/workspace/src/app.ts" || found.Matches[0].Line != 1 {
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
}

// contextWithCancel is the test's context with a cancel function, cancelled at cleanup too.
func contextWithCancel(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	return ctx, cancel
}
