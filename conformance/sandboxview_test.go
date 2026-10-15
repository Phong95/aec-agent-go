package conformance_test

import (
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/Phong95/aec-agent-go/aecmock"
	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/conformance"
	"github.com/Phong95/aec-agent-go/conformance/internal/check"
)

// TestSandboxViewInMemory runs the SandboxView suite on the in-memory sandbox,
// resolved through sandboxhub.SandboxHub: an in-memory provider registered as docker, the shared sandbox configured, and
// the agent under test resolved from the hub.
func TestSandboxViewInMemory(t *testing.T) {
	conformance.SandboxView(t, inMemoryFixture)
}

// inMemoryFixture builds the fixture of TestSandboxViewInMemory, so the hook
// tests below start from the same hub-resolved sandbox.
func inMemoryFixture(t *testing.T) conformance.SandboxViewFixture {
	t.Helper()
	hub := sandboxhub.NewSandboxHub()
	provider := aecmock.NewInMemoryProvider()
	check.NoErr(t, hub.Register(provider))
	check.NoErr(t, hub.ConfigureShared(sandboxhub.Spec{Kind: sandboxhub.KindDocker}))
	t.Cleanup(func() { check.NoErr(t, hub.Dispose(t.Context()), "dispose the hub") })
	view, err := hub.Resolve(t.Context(), "agent-under-test", sandboxhub.Selection{})
	check.NoErr(t, err)
	sb, ok := provider.Sandbox("docker:shared")
	if !ok {
		t.Fatal("the provider holds no docker:shared sandbox")
	}
	return conformance.SandboxViewFixture{View: view, Symlink: sb.Symlink, PlantFile: sb.PlantFile}
}

// skipRecorder notes which subtests of a suite run ended skipped. The factory
// registers a cleanup per subtest, and a cleanup runs after the subtest body, when
// Skipped is final.
type skipRecorder struct {
	mu      sync.Mutex
	skipped []string
	ran     int
}

func (r *skipRecorder) watch(t *testing.T) {
	t.Cleanup(func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.ran++
		if t.Skipped() {
			r.skipped = append(r.skipped, strings.TrimPrefix(t.Name(), "TestSandboxViewHooks"))
		}
	})
}

// TestSandboxViewHooksSkipExec proves SkipExec skips exactly the ten exec
// subtests (the 8 core exec subtests and the 2 exec Go-only subtests) and nothing
// else.
func TestSandboxViewHooksSkipExec(t *testing.T) {
	var rec skipRecorder
	conformance.SandboxView(t, func(t *testing.T) conformance.SandboxViewFixture {
		fx := inMemoryFixture(t)
		fx.SkipExec = "the backend cannot run commands"
		rec.watch(t)
		return fx
	})
	check.Equal(t, rec.ran, 33, "subtests that ran")
	check.Equal(t, len(rec.skipped), 10, "skipped subtests %v", rec.skipped)
	for _, name := range rec.skipped {
		if strings.Contains(name, "path escape") || strings.Contains(name, "symlink") {
			t.Errorf("subtest %q was skipped but does not run commands", name)
		}
	}
}

// TestSandboxViewHooksNilSymlink proves a nil Symlink skips exactly the two
// symlink subtests.
func TestSandboxViewHooksNilSymlink(t *testing.T) {
	var rec skipRecorder
	conformance.SandboxView(t, func(t *testing.T) conformance.SandboxViewFixture {
		fx := inMemoryFixture(t)
		fx.Symlink = nil
		rec.watch(t)
		return fx
	})
	check.Equal(t, len(rec.skipped), 2, "skipped subtests %v", rec.skipped)
	for _, name := range rec.skipped {
		if !strings.Contains(name, "symlinks") {
			t.Errorf("subtest %q was skipped but does not use symlinks", name)
		}
	}
}

// TestSandboxViewHooksSkipFollowInside proves SkipFollowInside skips only the
// follow-inside subtest.
func TestSandboxViewHooksSkipFollowInside(t *testing.T) {
	var rec skipRecorder
	conformance.SandboxView(t, func(t *testing.T) conformance.SandboxViewFixture {
		fx := inMemoryFixture(t)
		fx.SkipFollowInside = "the backend refuses to follow symlinks"
		rec.watch(t)
		return fx
	})
	check.Equal(t, len(rec.skipped), 1, "skipped subtests %v", rec.skipped)
	check.True(t, strings.Contains(rec.skipped[0], "follows_symlinks_that_stay_inside_the_roots"), "skipped %v", rec.skipped)
}

// TestSandboxViewHooksMapExecAndPwdMatches proves MapExec sees every exec request
// of the suite (the empty and unknown commands included) and PwdMatches decides the
// pwd assertion.
func TestSandboxViewHooksMapExecAndPwdMatches(t *testing.T) {
	var mu sync.Mutex
	var seen [][]string
	pwdCalls := 0
	conformance.SandboxView(t, func(t *testing.T) conformance.SandboxViewFixture {
		fx := inMemoryFixture(t)
		fx.MapExec = func(req sandboxhub.ExecRequest) sandboxhub.ExecRequest {
			mu.Lock()
			defer mu.Unlock()
			seen = append(seen, slices.Clone(req.Command))
			return req
		}
		fx.PwdMatches = func(stdout, workingDir string) bool {
			mu.Lock()
			defer mu.Unlock()
			pwdCalls++
			return strings.TrimSpace(stdout) == workingDir
		}
		return fx
	})
	mu.Lock()
	defer mu.Unlock()
	check.Equal(t, pwdCalls, 1, "PwdMatches calls")
	check.True(t, slices.ContainsFunc(seen, func(c []string) bool { return len(c) == 0 }), "MapExec never saw the empty command: %v", seen)
	check.True(t, slices.ContainsFunc(seen, func(c []string) bool { return len(c) == 1 && c[0] == "pwd" }), "MapExec never saw pwd: %v", seen)
	check.True(t, len(seen) >= 10, "MapExec saw only %d requests: %v", len(seen), seen)
}
