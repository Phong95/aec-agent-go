package host

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/kernel"
)

// Host sandboxes run on the real machine, so these tests use real files in
// t.TempDir(). They call provider.Create with own tenancy instead of going through
// SandboxManager (CONTEXT D4). The command tests run the test binary itself
// (helper_process_test.go), and the three command cases are at the end of this
// file (plan 07-05).

const ownAgent = "host-agent"

// newOwnView creates the own host sandbox of agent "host-agent" under a fresh
// temporary root, and returns the root, the provider,
// the sandbox and the agent's view.
func newOwnView(t *testing.T, opts ...func(*Options)) (string, *Provider, *Sandbox, sandboxhub.View) {
	t.Helper()
	root := t.TempDir()
	o := Options{Root: root}
	for _, f := range opts {
		f(&o)
	}
	p, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	sb, err := p.Create(t.Context(), sandboxhub.Spec{Kind: sandboxhub.KindHost},
		sandboxhub.CreateContext{ID: "host:own:" + ownAgent, Tenancy: sandboxhub.TenancyOwn, AgentID: ownAgent})
	if err != nil {
		t.Fatal(err)
	}
	view, err := sb.ForAgent(t.Context(), ownAgent)
	if err != nil {
		t.Fatal(err)
	}
	return root, p, sb.(*Sandbox), view
}

// requireCode requires err to be a *kernel.SandboxError with the given code.
func requireCode(t *testing.T, err error, code kernel.SandboxErrorCode) {
	t.Helper()
	var se *kernel.SandboxError
	if !errors.As(err, &se) {
		t.Fatalf("want SandboxError %q, got %v", code, err)
	}
	if se.Code != code {
		t.Fatalf("code = %q, want %q (%v)", se.Code, code, err)
	}
}

func TestHostSandboxProviderRejectsARelativeRoot(t *testing.T) {
	_, err := New(Options{Root: "relative/dir"})
	if err == nil {
		t.Fatal("a relative root must be rejected")
	}
	if want := `HostSandboxProvider needs an absolute root folder (got "relative/dir")`; err.Error() != want {
		t.Errorf("error = %q, want %q", err.Error(), want)
	}
	if _, err := New(Options{}); err == nil {
		t.Error("an empty root must be rejected")
	}
}

func TestHostSandboxProviderMapsAnOwnSandboxToRootAgentIDAndReportsItAsNotIsolated(t *testing.T) {
	root, p, _, view := newOwnView(t)
	if view.ID() != "host:own:host-agent" {
		t.Errorf("id = %q", view.ID())
	}
	traits := view.Traits()
	if traits.Kind != sandboxhub.KindHost || traits.Isolated || traits.Tenancy != sandboxhub.TenancyOwn || !traits.Persistent {
		t.Errorf("traits = %+v", traits)
	}
	if traits.Network != sandboxhub.NetworkFull {
		t.Errorf("network = %q, want full", traits.Network)
	}
	if view.WorkingDir() != "/workspace" {
		t.Errorf("working directory = %q", view.WorkingDir())
	}
	if st, err := os.Stat(filepath.Join(root, "host-agent")); err != nil || !st.IsDir() {
		t.Errorf("the own folder %s must exist: %v", filepath.Join(root, "host-agent"), err)
	}
	if p.Name() != "sandbox:host" || p.Version() != "1.0.0" || p.Kind() != sandboxhub.KindHost {
		t.Errorf("identity = %s %s %s", p.Name(), p.Version(), p.Kind())
	}
	if p.Root() != filepath.Clean(root) {
		t.Errorf("root = %q, want %q", p.Root(), filepath.Clean(root))
	}
}

func TestHostSandboxProviderWritesRealFilesAndReadsThemBackWithMatchingHashes(t *testing.T) {
	root, _, _, view := newOwnView(t)
	ctx := t.Context()
	written, err := view.WriteFile(ctx, "notes/plan.md", "# Plan\n", sandboxhub.WriteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if written.Path != "/workspace/notes/plan.md" || !written.Created {
		t.Errorf("write result = %+v", written)
	}
	onDisk, err := os.ReadFile(filepath.Join(root, "host-agent", "notes", "plan.md"))
	if err != nil || string(onDisk) != "# Plan\n" {
		t.Errorf("the real file holds %q (%v)", onDisk, err)
	}
	read, err := view.ReadFile(ctx, "/workspace/notes/plan.md", sandboxhub.ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if read.Content != "# Plan\n" || read.SHA256 != written.SHA256 || read.Truncated {
		t.Errorf("read result = %+v, want content, hash %s and no truncation", read, written.SHA256)
	}
	_, err = view.WriteFile(ctx, "notes/plan.md", "x", sandboxhub.WriteOptions{ExpectedSHA256: "stale"})
	requireCode(t, err, kernel.SandboxCodeConflict)
}

// TestHostSandboxProviderCreateErrors covers the provider errors of Create:
// another kind, an own sandbox without an agent id, an unsafe agent id.
func TestHostSandboxProviderCreateErrors(t *testing.T) {
	p, err := New(Options{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	_, err = p.Create(ctx, sandboxhub.Spec{Kind: sandboxhub.KindDocker}, sandboxhub.CreateContext{ID: "docker:shared", Tenancy: sandboxhub.TenancyShared})
	requireCode(t, err, kernel.SandboxCodeProviderUnavailable)
	_, err = p.Create(ctx, sandboxhub.Spec{Kind: sandboxhub.KindHost}, sandboxhub.CreateContext{ID: "host:own:x", Tenancy: sandboxhub.TenancyOwn})
	requireCode(t, err, kernel.SandboxCodeInvalidArgument)
	_, err = p.Create(ctx, sandboxhub.Spec{Kind: sandboxhub.KindHost}, sandboxhub.CreateContext{ID: "host:own:x", Tenancy: sandboxhub.TenancyOwn, AgentID: "../x"})
	requireCode(t, err, kernel.SandboxCodePolicyDenied)
}

// TestHostSandboxProviderDefaultsAndLimits pins DefaultLimits and the
// way Options.Limits overlays them.
func TestHostSandboxProviderDefaultsAndLimits(t *testing.T) {
	want := sandboxhub.Limits{
		MaxReadBytes: 2_000_000, MaxWriteBytes: 5_000_000, MaxListEntries: 5_000, MaxSearchMatches: 500,
		MaxExecTimeoutMs: 600_000, MaxExecOutputBytes: 2_000_000,
	}
	if got := DefaultLimits(); got != want {
		t.Errorf("DefaultLimits() = %+v, want %+v", got, want)
	}
	_, _, _, view := newOwnView(t)
	if view.Limits() != want {
		t.Errorf("view limits = %+v, want the defaults", view.Limits())
	}
	maxWrite := 10
	_, _, _, small := newOwnView(t, func(o *Options) { o.Limits = sandboxhub.LimitsPatch{MaxWriteBytes: &maxWrite} })
	if small.Limits().MaxWriteBytes != 10 || small.Limits().MaxReadBytes != want.MaxReadBytes {
		t.Errorf("patched limits = %+v", small.Limits())
	}
	_, err := small.WriteFile(t.Context(), "big.txt", "12345678901", sandboxhub.WriteOptions{})
	requireCode(t, err, kernel.SandboxCodeTooLarge)
}

// TestHostSandboxCapabilities documents what the host view provides: the five
// capabilities in the fixed order (plan 07-05 added exec and
// exec.shell to the file side of plan 07-04), and a fresh capability slice.
func TestHostSandboxCapabilities(t *testing.T) {
	_, _, _, view := newOwnView(t)
	want := []sandboxhub.Capability{
		sandboxhub.CapabilityFSRead, sandboxhub.CapabilityFSWrite, sandboxhub.CapabilityFSSearch,
		sandboxhub.CapabilityExec, sandboxhub.CapabilityExecShell, sandboxhub.CapabilityFSStream,
		sandboxhub.CapabilityServices,
	}
	if got := view.Capabilities(); !slices.Equal(got, want) {
		t.Errorf("capabilities = %v, want %v", got, want)
	}
	caps := view.Capabilities()
	caps[0] = "tampered"
	if !slices.Equal(view.Capabilities(), want) {
		t.Error("Capabilities() must return a fresh slice")
	}
}

func TestHostSandboxProviderListsStatsAndSearches(t *testing.T) {
	_, _, _, view := newOwnView(t)
	ctx := t.Context()
	for p, content := range map[string]string{"src/a.ts": "const Rebar = 1;\nrebar spacing\n", "src/.hidden": "rebar"} {
		if _, err := view.WriteFile(ctx, p, content, sandboxhub.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	listed, err := view.List(ctx, "src", sandboxhub.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range listed.Entries {
		names = append(names, e.Name)
	}
	if !slices.Equal(names, []string{"a.ts"}) {
		t.Errorf("listing = %v, want only a.ts (the hidden file is skipped)", names)
	}
	st, err := view.Stat(ctx, "src/a.ts")
	if err != nil || st == nil || st.Type != sandboxhub.EntryFile {
		t.Errorf("stat = %+v (%v)", st, err)
	}
	if st != nil && (st.Size != len("const Rebar = 1;\nrebar spacing\n") || st.ModifiedAt <= 0 || st.Path != "/workspace/src/a.ts") {
		t.Errorf("stat details = %+v", st)
	}
	if missing, err := view.Stat(ctx, "nope"); err != nil || missing != nil {
		t.Errorf("stat of a missing path = %+v (%v), want nil and no error", missing, err)
	}
	found, err := view.Search(ctx, sandboxhub.SearchQuery{Pattern: "rebar", Path: "src", Glob: "*.ts"})
	if err != nil {
		t.Fatal(err)
	}
	want := []sandboxhub.SearchMatch{{Path: "/workspace/src/a.ts", Line: 2, Text: "rebar spacing"}}
	if !slices.Equal(found.Matches, want) {
		t.Errorf("matches = %+v, want %+v", found.Matches, want)
	}
}

func TestHostSandboxProviderBlocksASymlinkOrJunctionThatPointsOutsideTheAgentsFolder(t *testing.T) {
	root, _, _, view := newOwnView(t)
	outside := filepath.Join(root, "victim")
	if err := os.Mkdir(outside, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("top secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	makeLink(t, filepath.Join(root, "host-agent", "escape"), outside)
	_, err := view.ReadFile(t.Context(), "escape/secret.txt", sandboxhub.ReadOptions{})
	requireCode(t, err, kernel.SandboxCodePathEscape)
	_, err = view.WriteFile(t.Context(), "escape/new.txt", "x", sandboxhub.WriteOptions{})
	requireCode(t, err, kernel.SandboxCodePathEscape)
	if _, statErr := os.Stat(filepath.Join(outside, "new.txt")); statErr == nil {
		t.Error("the refused write created a file in the victim folder")
	}
}

func TestHostSandboxProviderNeedsApprovalForWritesAndCommandsNotForReads(t *testing.T) {
	_, _, _, view := newOwnView(t)
	hub := sandboxhub.NewSandboxHub()
	for op, want := range map[sandboxhub.Operation]bool{
		sandboxhub.OperationFSWrite:  true,
		sandboxhub.OperationExec:     true,
		sandboxhub.OperationFSRead:   false,
		sandboxhub.OperationFSSearch: false,
	} {
		if got := hub.RequiresApproval(view.Traits(), op); got != want {
			t.Errorf("RequiresApproval(host, %s) = %v, want %v (default policy)", op, got, want)
		}
	}
}

func TestHostSandboxProviderDeletesTheFolderOnReleaseAlsoWhenNotRunning(t *testing.T) {
	root, p, sb, view := newOwnView(t)
	ctx := t.Context()
	if _, err := view.WriteFile(ctx, "x.txt", "x", sandboxhub.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := sb.Dispose(ctx, true); err != nil {
		t.Fatal(err)
	}
	folder := filepath.Join(root, "host-agent")
	if _, err := os.Stat(folder); err == nil {
		t.Fatal("Dispose with removeData must delete the folder")
	}

	if err := os.Mkdir(folder, 0o750); err != nil {
		t.Fatal(err)
	}
	// Cold: the sandbox is not running, so the data goes through provider.RemoveData.
	cc := sandboxhub.CreateContext{ID: "host:own:host-agent", Tenancy: sandboxhub.TenancyOwn, AgentID: ownAgent}
	if err := p.RemoveData(ctx, sandboxhub.Spec{Kind: sandboxhub.KindHost}, cc, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(folder); err == nil {
		t.Fatal("RemoveData must delete the folder of a sandbox that is not running")
	}
	// Removing what is not there is not an error (fs.rm force).
	if err := p.RemoveData(ctx, sandboxhub.Spec{Kind: sandboxhub.KindHost}, cc, ""); err != nil {
		t.Errorf("RemoveData of a missing folder: %v", err)
	}
}

// TestHostSandboxSharedLayout checks the shared sandbox layout and the rules the
// provider has for it: <root>/.shared/agents/<id>, one folder per agent, and
// RemoveData of one agent's folder.
func TestHostSandboxSharedLayout(t *testing.T) {
	root, sb, a1, a2 := newSharedViews(t)
	ctx := t.Context()
	if sb.Base() != filepath.Join(root, ".shared") {
		t.Errorf("base = %q, want <root>/.shared", sb.Base())
	}
	if a1.WorkingDir() != "/workspace/agents/a1" || !slices.Equal(a1.Roots(), []string{"/workspace/agents/a1"}) {
		t.Errorf("layout = %s %v", a1.WorkingDir(), a1.Roots())
	}
	if a1.Traits().Tenancy != sandboxhub.TenancyShared {
		t.Errorf("tenancy = %q", a1.Traits().Tenancy)
	}
	for _, id := range []string{"a1", "a2"} {
		if st, err := os.Stat(filepath.Join(sb.Base(), "agents", id)); err != nil || !st.IsDir() {
			t.Errorf("the folder of agent %s must exist: %v", id, err)
		}
	}
	if _, err := a1.WriteFile(ctx, "f.txt", "one", sandboxhub.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := a2.ReadFile(ctx, "/workspace/agents/a1/f.txt", sandboxhub.ReadOptions{}); err == nil {
		t.Error("an agent read another agent's folder")
	} else {
		requireCode(t, err, kernel.SandboxCodePathEscape)
	}

	p, err := New(Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	cc := sandboxhub.CreateContext{ID: "host:shared", Tenancy: sandboxhub.TenancyShared}
	if err := p.RemoveData(ctx, sandboxhub.Spec{Kind: sandboxhub.KindHost}, cc, "a1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(sb.Base(), "agents", "a1")); err == nil {
		t.Error("RemoveData with an agent id must delete that agent's folder")
	}
	if _, err := os.Stat(filepath.Join(sb.Base(), "agents", "a2")); err != nil {
		t.Errorf("the other agent's folder must stay: %v", err)
	}
	if err := p.RemoveData(ctx, sandboxhub.Spec{Kind: sandboxhub.KindHost}, cc, "../x"); err == nil {
		t.Error("RemoveData must validate the agent id")
	} else {
		requireCode(t, err, kernel.SandboxCodePolicyDenied)
	}
}

// TestHostSandboxRemoveAgentDataValidatesTheID pins the id hardening (T-07-17):
// an id like ".." would resolve to the parent folder.
func TestHostSandboxRemoveAgentDataValidatesTheID(t *testing.T) {
	root, sb, _, a2 := newSharedViews(t)
	ctx := t.Context()
	if err := os.WriteFile(filepath.Join(root, "keep.txt"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"..", "../x", "", ".hidden", "a/b", "a..b"} {
		err := sb.RemoveAgentData(ctx, id)
		requireCode(t, err, kernel.SandboxCodePolicyDenied)
	}
	if _, err := os.Stat(filepath.Join(root, "keep.txt")); err != nil {
		t.Errorf("a rejected id removed something: %v", err)
	}
	if _, err := a2.WriteFile(ctx, "mine.txt", "x", sandboxhub.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := sb.RemoveAgentData(ctx, "a2"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(sb.Base(), "agents", "a2")); err == nil {
		t.Error("RemoveAgentData must delete the agent's folder")
	}
	if err := sb.RemoveAgentData(ctx, "never-existed"); err != nil {
		t.Errorf("removing a folder that is not there: %v", err)
	}
}

// TestHostSandboxRemovalDoesNotFollowLinks proves that deleting an agent folder (or a
// whole sandbox) that contains a link to a folder outside deletes the link and
// nothing behind it, on Windows with a junction as well (os.RemoveAll over a junction
// has recursed into the target on some Go versions).
func TestHostSandboxRemovalDoesNotFollowLinks(t *testing.T) {
	for _, mode := range []string{"RemoveAgentData", "Dispose", "RemoveData"} {
		t.Run(mode, func(t *testing.T) {
			root, sb, _, _ := newSharedViews(t)
			outside := t.TempDir()
			keep := filepath.Join(outside, "keep.txt")
			if err := os.WriteFile(keep, []byte("keep"), 0o600); err != nil {
				t.Fatal(err)
			}
			link := filepath.Join(sb.Base(), "agents", "a1", "escape")
			makeLink(t, link, outside)
			ctx := t.Context()
			var err error
			switch mode {
			case "RemoveAgentData":
				err = sb.RemoveAgentData(ctx, "a1")
			case "Dispose":
				err = sb.Dispose(ctx, true)
			default:
				var prov *Provider
				if prov, err = New(Options{Root: root}); err == nil {
					cc := sandboxhub.CreateContext{ID: "host:shared", Tenancy: sandboxhub.TenancyShared}
					err = prov.RemoveData(ctx, sandboxhub.Spec{Kind: sandboxhub.KindHost}, cc, "a1")
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if data, readErr := os.ReadFile(keep); readErr != nil || string(data) != "keep" {
				t.Errorf("the file behind the link was removed: %q (%v)", data, readErr)
			}
			if _, statErr := os.Lstat(link); statErr == nil {
				t.Error("the link itself must be removed with the folder")
			}
		})
	}
}

func TestHostSandboxProviderRunsCommandsInTheAgentsFolderAndPassesOnlyExplicitVariables(t *testing.T) {
	root, _, _, view := newOwnView(t)
	ctx := t.Context()

	cwd, err := runExec(ctx, t, view, helperRequest(t, "cwd", nil, 10_000), sandboxhub.ExecOptions{})
	if err != nil {
		t.Fatal(err)
	}
	wantExit(t, cwd, 0)
	// Both sides go through the symlink evaluator (Windows can return 8.3 short names
	// for a temporary folder) and compare case-insensitively on Windows.
	got, err := filepath.EvalSymlinks(cwd.Stdout)
	if err != nil {
		t.Fatalf("the command printed %q: %v", cwd.Stdout, err)
	}
	want, err := filepath.EvalSymlinks(filepath.Join(root, ownAgent))
	if err != nil {
		t.Fatal(err)
	}
	if got != want && (!onWindows || !strings.EqualFold(got, want)) {
		t.Errorf("working directory = %q, want %q", got, want)
	}

	t.Setenv("AEC_HOST_SECRET", "leaked")
	res, err := runExec(ctx, t, view, helperRequest(t, "env", map[string]string{"GREETING": "hi"}, 10_000), sandboxhub.ExecOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Stdout != "undefined hi" {
		t.Errorf("stdout = %q, want %q", res.Stdout, "undefined hi")
	}
}

func TestHostSandboxProviderRunsShellCommandsReportsExitCodesAndUnknownPrograms(t *testing.T) {
	_, _, _, view := newOwnView(t)
	ctx := t.Context()
	shell, err := runExec(ctx, t, view, sandboxhub.ExecRequest{
		Command: []string{"echo", "hello"}, Shell: boolPtr(true), TimeoutMs: 10_000, MaxOutputBytes: 10_000,
	}, sandboxhub.ExecOptions{})
	if err != nil {
		t.Fatal(err)
	}
	wantExit(t, shell, 0)
	if strings.TrimSpace(shell.Stdout) != "hello" {
		t.Errorf("shell stdout = %q, want hello", shell.Stdout)
	}
	fail, err := runExec(ctx, t, view, helperRequest(t, "exit3", nil, 10_000), sandboxhub.ExecOptions{})
	if err != nil {
		t.Fatal(err)
	}
	wantExit(t, fail, 3)
	missing, err := runExec(ctx, t, view, sandboxhub.ExecRequest{
		Command: []string{"aec-no-such-program"}, TimeoutMs: 10_000, MaxOutputBytes: 10_000,
	}, sandboxhub.ExecOptions{})
	if err != nil {
		t.Fatal(err)
	}
	wantExit(t, missing, 127)
	if missing.Stderr != "aec-no-such-program: command not found\n" {
		t.Errorf("stderr = %q", missing.Stderr)
	}
}

func TestHostSandboxProviderKillsCommandsOnTimeoutOutputCapAndCancel(t *testing.T) {
	_, _, _, view := newOwnView(t)
	ctx := t.Context()

	timedOut, err := runExec(ctx, t, view, helperRequest(t, "sleep-forever", nil, 300), sandboxhub.ExecOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !timedOut.TimedOut || timedOut.ExitCode != nil {
		t.Errorf("timeout: timedOut=%v exitCode=%v", timedOut.TimedOut, timedOut.ExitCode)
	}

	noisyReq := helperRequest(t, "flood", nil, 10_000)
	noisyReq.MaxOutputBytes = 1_000
	noisy, err := runExec(ctx, t, view, noisyReq, sandboxhub.ExecOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !noisy.Truncated || noisy.ExitCode != nil || len(noisy.Stdout) > 1_000 {
		t.Errorf("output cap: truncated=%v exitCode=%v bytes=%d", noisy.Truncated, noisy.ExitCode, len(noisy.Stdout))
	}

	cancelCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	timer := time.AfterFunc(200*time.Millisecond, cancel)
	defer timer.Stop()
	cancelled, err := runExec(cancelCtx, t, view, helperRequest(t, "sleep-forever", nil, 10_000), sandboxhub.ExecOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !cancelled.Cancelled || cancelled.ExitCode != nil {
		t.Errorf("cancel: cancelled=%v exitCode=%v", cancelled.Cancelled, cancelled.ExitCode)
	}
}
