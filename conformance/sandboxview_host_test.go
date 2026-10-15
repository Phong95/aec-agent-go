package conformance_test

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/components/sandboxhub/host"
	"github.com/Phong95/aec-agent-go/conformance"
)

// TestSandboxViewHost runs the SandboxView suite on a real host sandbox, as the
// roadmap asks, in addition to the host sandbox's own tests (which use an
// OWN-tenancy sandbox). The suite
// asserts that the view belongs to a SHARED sandbox, so every subtest builds one under
// its own temporary root: <root>/.shared, with the agent folder under agents/.
//
// Every subtest runs, the exec ones too: the commands of the suite (echo, pwd, false,
// cat, printenv, sleep, yes) are mapped to a re-exec of this test binary (MapExec, the
// helper process of helper_process_test.go), so the suite needs no coreutils and runs
// on a Windows host. Nothing is skipped on a machine that can create a symlink or a
// junction: Windows without a privilege runs the symlink subtests with junctions (the
// test logs "link mode: junction").
func TestSandboxViewHost(t *testing.T) {
	conformance.SandboxView(t, hostFixture)
}

// hostFixture builds the fixture of one subtest: a host provider over a fresh
// temporary root, a shared sandbox, the view of the agent under test and the hooks
// that put links and files on the real disk.
func hostFixture(t *testing.T) conformance.SandboxViewFixture {
	t.Helper()
	root := t.TempDir()
	// Link targets outside /workspace (the suite uses /etc) map to a second temporary
	// folder, so nothing outside the temporary roots is ever touched.
	outside := t.TempDir()

	p, err := host.New(host.Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	sb, err := p.Create(t.Context(), sandboxhub.Spec{Kind: sandboxhub.KindHost},
		sandboxhub.CreateContext{ID: "host:shared", Tenancy: sandboxhub.TenancyShared})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := sb.Dispose(t.Context(), false); err != nil {
			t.Errorf("dispose the host sandbox: %v", err)
		}
	})
	view, err := sb.ForAgent(t.Context(), "agent-under-test")
	if err != nil {
		t.Fatal(err)
	}
	base := sb.(*host.Sandbox).Base()

	// inWorkspace maps a logical path below /workspace to the host folder.
	inWorkspace := func(logical string) (string, bool) {
		if logical == sandboxhub.WorkspaceRoot {
			return base, true
		}
		rest, ok := strings.CutPrefix(logical, sandboxhub.WorkspaceRoot+"/")
		if !ok {
			return "", false
		}
		return filepath.Join(base, filepath.FromSlash(rest)), true
	}

	return conformance.SandboxViewFixture{
		View: view,
		// The link lives in the workspace; its target is a logical path that may
		// point anywhere: inside the workspace it maps through the base, outside it
		// maps to a folder of the separate temporary root named after the path.
		Symlink: func(link, target string) error {
			hostLink, ok := inWorkspace(link)
			if !ok {
				return fmt.Errorf("the link %q is outside /workspace", link)
			}
			hostTarget, ok := inWorkspace(target)
			if !ok {
				hostTarget = filepath.Join(outside, strings.ReplaceAll(strings.TrimPrefix(target, "/"), "/", "_"))
			}
			if err := os.MkdirAll(filepath.Dir(hostLink), 0o750); err != nil {
				return err
			}
			// A junction needs an existing folder, and the target of the suite's links
			// is always a folder.
			if err := os.MkdirAll(hostTarget, 0o750); err != nil {
				return err
			}
			makeLink(t, hostLink, hostTarget)
			return nil
		},
		PlantFile: func(logical, content string) error {
			hostPath, ok := inWorkspace(logical)
			if !ok {
				return fmt.Errorf("the planted file %q is outside /workspace", logical)
			}
			if err := os.MkdirAll(filepath.Dir(hostPath), 0o750); err != nil {
				return err
			}
			return os.WriteFile(hostPath, []byte(content), 0o600)
		},
		MapExec: mapToHelper,
		// The commands print native host paths and the suite expects the logical
		// working directory, so pwd is compared with the host folder of that directory.
		PwdMatches: func(stdout, workingDir string) bool {
			hostDir, ok := inWorkspace(workingDir)
			return ok && samePath(strings.TrimSpace(stdout), hostDir)
		},
	}
}

// helperCommands are the programs of the suite that run as the test binary.
var helperCommands = []string{"echo", "pwd", "false", "cat", "printenv", "sleep", "yes"}

// mapToHelper rewrites a request for one of the suite commands to the same arguments
// on the test binary, with AEC_CONFORMANCE_HELPER naming the command in a COPY of the
// request env (the request itself is never changed). Any other request, the empty and
// the unknown command of the validation subtests included, is returned unchanged. The
// variable travels through the request env because nothing else of the server
// environment reaches a host command.
func mapToHelper(req sandboxhub.ExecRequest) sandboxhub.ExecRequest {
	if len(req.Command) == 0 || !slices.Contains(helperCommands, req.Command[0]) {
		return req
	}
	exe, err := os.Executable()
	if err != nil {
		return req
	}
	mapped := req
	mapped.Command = append([]string{exe}, req.Command[1:]...)
	mapped.Env = maps.Clone(req.Env)
	if mapped.Env == nil {
		mapped.Env = map[string]string{}
	}
	mapped.Env["AEC_CONFORMANCE_HELPER"] = req.Command[0]
	return mapped
}

// samePath compares two host paths after the symlink evaluator on both sides (Windows
// can return 8.3 short names for a temporary folder) and, on Windows, ignoring case.
func samePath(a, b string) bool {
	if ra, err := filepath.EvalSymlinks(a); err == nil {
		a = ra
	}
	if rb, err := filepath.EvalSymlinks(b); err == nil {
		b = rb
	}
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}
