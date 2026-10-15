package sandboxhub_test

import (
	"errors"
	"slices"
	"testing"

	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/internal/jsonx"
	"github.com/Phong95/aec-agent-go/internal/wiretest"
	"github.com/Phong95/aec-agent-go/kernel"
)

// wireErr is the error half of an outcome the fixture recorded. Code and
// SandboxID are absent when the recorded error carried none (a plain error has no
// code).
type wireErr struct {
	Code      string  `json:"code"`
	Message   string  `json:"message"`
	SandboxID *string `json:"sandboxId"`
}

// outcome is what one recorded call did: it returned OK, or it failed with Err. A
// call that returned nothing records neither key.
type outcome[T any] struct {
	OK  *T       `json:"ok"`
	Err *wireErr `json:"err"`
}

type logicalPathFixture struct {
	Constants struct {
		WorkspaceRoot string `json:"workspaceRoot"`
		AgentsDir     string `json:"agentsDir"`
	} `json:"constants"`
	Resolve []struct {
		Layout  string          `json:"layout"`
		Input   string          `json:"input"`
		Outcome outcome[string] `json:"outcome"`
	} `json:"resolve"`
	AgentIDs []struct {
		ID      string       `json:"id"`
		Outcome outcome[any] `json:"outcome"`
	} `json:"agentIds"`
	Layouts []struct {
		Tenancy string                     `json:"tenancy"`
		AgentID string                     `json:"agentId"`
		Outcome outcome[sandboxhub.Layout] `json:"outcome"`
	} `json:"layouts"`
	WithinRoots []struct {
		Path   string   `json:"path"`
		Roots  []string `json:"roots"`
		Result bool     `json:"result"`
	} `json:"withinRoots"`
}

func loadLogicalPathFixture(t *testing.T) logicalPathFixture {
	t.Helper()
	var f logicalPathFixture
	if err := jsonx.UnmarshalStrict(wiretest.Load(t, "sandbox-logical-path.json"), &f); err != nil {
		t.Fatalf("decode sandbox-logical-path.json: %v", err)
	}
	return f
}

// assertErrorMatches requires err to be the *kernel.SandboxError the fixture
// recorded: the same code, message and sandbox id. A nil want means the call must
// succeed.
func assertErrorMatches(t *testing.T, name string, err error, want *wireErr) {
	t.Helper()
	if want == nil {
		if err != nil {
			t.Errorf("%s: unexpected error %v", name, err)
		}
		return
	}
	var se *kernel.SandboxError
	if want.Code == "" {
		// A plain recorded error: no code, so the error is not a SandboxError either.
		if err == nil || errors.As(err, &se) || err.Error() != want.Message {
			t.Errorf("%s: error = %v (%T), want a plain error with message %q", name, err, err, want.Message)
		}
		return
	}
	if !errors.As(err, &se) {
		t.Errorf("%s: error = %v (%T), want a *kernel.SandboxError with code %q", name, err, err, want.Code)
		return
	}
	wantID := ""
	if want.SandboxID != nil {
		wantID = *want.SandboxID
	}
	if string(se.Code) != want.Code || se.Message != want.Message || se.SandboxID != wantID {
		t.Errorf("%s:\n got code=%q message=%q sandboxId=%q\nwant code=%q message=%q sandboxId=%q",
			name, se.Code, se.Message, se.SandboxID, want.Code, want.Message, wantID)
	}
}

// layoutNamed returns the working directory and roots of one of the three named
// layouts of the fixture. The rootless layout is the input of the root === "/"
// branch of isWithinRoots.
func layoutNamed(t *testing.T, name string) (string, []string) {
	t.Helper()
	switch name {
	case "shared", "own":
		l, err := sandboxhub.AgentLayout(sandboxhub.Tenancy(name), "a1")
		if err != nil {
			t.Fatalf("AgentLayout(%s): %v", name, err)
		}
		return l.WorkingDir, l.Roots
	case "rootless":
		return "/", []string{"/"}
	}
	t.Fatalf("unknown fixture layout %q", name)
	return "", nil
}

func jsonQuote(t *testing.T, s string) string {
	t.Helper()
	b, err := jsonx.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestResolveLogicalPathMatchesGolden(t *testing.T) {
	f := loadLogicalPathFixture(t)
	if f.Constants.WorkspaceRoot != sandboxhub.WorkspaceRoot || f.Constants.AgentsDir != sandboxhub.AgentsDir {
		t.Errorf("constants = %q, %q, want %q, %q", sandboxhub.WorkspaceRoot, sandboxhub.AgentsDir, f.Constants.WorkspaceRoot, f.Constants.AgentsDir)
	}
	if len(f.Resolve) < 150 {
		t.Fatalf("the corpus has %d rows, want at least 150", len(f.Resolve))
	}
	for _, row := range f.Resolve {
		workingDir, roots := layoutNamed(t, row.Layout)
		name := row.Layout + " " + jsonQuote(t, row.Input)
		got, err := sandboxhub.ResolveLogicalPath(row.Input, workingDir, roots, "sb-test")
		assertErrorMatches(t, name, err, row.Outcome.Err)
		if row.Outcome.Err != nil {
			continue
		}
		if row.Outcome.OK == nil {
			t.Fatalf("%s: the fixture row has neither ok nor err", name)
		}
		if got != *row.Outcome.OK {
			t.Errorf("%s: got %q, want %q", name, got, *row.Outcome.OK)
		}
	}
}

func TestAssertSafeAgentIDMatchesGolden(t *testing.T) {
	f := loadLogicalPathFixture(t)
	for _, row := range f.AgentIDs {
		err := sandboxhub.AssertSafeAgentID(row.ID)
		assertErrorMatches(t, "agent id "+jsonQuote(t, row.ID), err, row.Outcome.Err)
	}
}

func TestAgentLayoutMatchesGolden(t *testing.T) {
	f := loadLogicalPathFixture(t)
	for _, row := range f.Layouts {
		name := row.Tenancy + " " + jsonQuote(t, row.AgentID)
		got, err := sandboxhub.AgentLayout(sandboxhub.Tenancy(row.Tenancy), row.AgentID)
		assertErrorMatches(t, name, err, row.Outcome.Err)
		if row.Outcome.Err != nil {
			continue
		}
		want := *row.Outcome.OK
		if got.WorkingDir != want.WorkingDir || !slices.Equal(got.Roots, want.Roots) {
			t.Errorf("%s: got %+v, want %+v", name, got, want)
			continue
		}
		// Every call returns a fresh roots slice a caller may modify.
		got.Roots[0] = "mutated"
		again, err := sandboxhub.AgentLayout(sandboxhub.Tenancy(row.Tenancy), row.AgentID)
		if err != nil || !slices.Equal(again.Roots, want.Roots) {
			t.Errorf("%s: a second call shares the roots slice: %v %v", name, again, err)
		}
	}
}

func TestIsWithinRootsMatchesGolden(t *testing.T) {
	f := loadLogicalPathFixture(t)
	for _, row := range f.WithinRoots {
		if got := sandboxhub.IsWithinRoots(row.Path, row.Roots); got != row.Result {
			t.Errorf("IsWithinRoots(%q, %q) = %v, want %v", row.Path, row.Roots, got, row.Result)
		}
	}
}

// The three "logical paths" cases of the hub.

func TestLogicalPathsResolvesRelativeAbsoluteAndBackslashPathsInsideTheRoots(t *testing.T) {
	layout, err := sandboxhub.AgentLayout(sandboxhub.TenancyShared, "a1")
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"src/app.ts":             "/workspace/agents/a1/src/app.ts",
		`src\app.ts`:             "/workspace/agents/a1/src/app.ts",
		"":                       layout.WorkingDir,
		"./a/../b/":              "/workspace/agents/a1/b",
		"/workspace/agents/a1/x": "/workspace/agents/a1/x",
	}
	for input, want := range cases {
		got, err := sandboxhub.ResolveLogicalPath(input, layout.WorkingDir, layout.Roots, "")
		if err != nil || got != want {
			t.Errorf("ResolveLogicalPath(%q) = %q, %v, want %q", input, got, err, want)
		}
	}
}

func TestLogicalPathsRejectsEscapesAndHostPaths(t *testing.T) {
	layout, err := sandboxhub.AgentLayout(sandboxhub.TenancyShared, "a1")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"..", "../a2/x", "/workspace", "/workspace/agents", "/workspace/shared/x", "/etc/passwd", `D:\secrets`, "//server/share"} {
		_, err := sandboxhub.ResolveLogicalPath(p, layout.WorkingDir, layout.Roots, "")
		var se *kernel.SandboxError
		if !errors.As(err, &se) {
			t.Errorf("ResolveLogicalPath(%q) error = %v, want a *kernel.SandboxError", p, err)
		}
	}
}

func TestLogicalPathsOwnTenancyOwnsWorkspace(t *testing.T) {
	got, err := sandboxhub.AgentLayout(sandboxhub.TenancyOwn, "a1")
	if err != nil {
		t.Fatal(err)
	}
	if got.WorkingDir != "/workspace" || !slices.Equal(got.Roots, []string{"/workspace"}) {
		t.Errorf("AgentLayout(own) = %+v", got)
	}
}
