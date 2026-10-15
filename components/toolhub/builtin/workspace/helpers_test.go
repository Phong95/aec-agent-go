package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/Phong95/aec-agent-go/aecmock"
	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/internal/wiretest"
	"github.com/Phong95/aec-agent-go/kernel"
)

// This file holds the sandbox side of the tests of the package: the environment of a
// recorded case (a real in-memory sandbox behind a logging view with the wrappers of the
// recorder), and the small helpers the replay and the unit cases share.

// specFail is the recorder's fail wrapper: that view call throws.
type specFail struct {
	Op      string  `json:"op"`
	Code    string  `json:"code"`
	Message *string `json:"message"`
}

// specRace is the recorder's race wrapper: a readFile whose content equals When first
// overwrites the file with Writes through the real view.
type specRace struct {
	When   string `json:"when"`
	Writes string `json:"writes"`
}

// sandboxSpec is the sandbox of a recorded case (see workspace.cases.mjs).
type sandboxSpec struct {
	Mode         string                  `json:"mode"`
	Code         string                  `json:"code"`
	Message      *string                 `json:"message"`
	Limits       *sandboxhub.LimitsPatch `json:"limits"`
	Capabilities *[]string               `json:"capabilities"`
	Planted      []struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	} `json:"planted"`
	Symlinks []struct {
		Link   string `json:"link"`
		Target string `json:"target"`
	} `json:"symlinks"`
	Fail *specFail `json:"fail"`
	// LieSha replaces the sha256 of every verification read (MaxBytes 1).
	LieSha string    `json:"lieSha"`
	Race   *specRace `json:"race"`
	// ExecResult is returned by every Exec instead of running the command.
	ExecResult *sandboxhub.ExecResult `json:"execResult"`
	NoFiles    bool                   `json:"noFiles"`
}

// simulatedError is the error the recorder's failWith throws: a SandboxError of the code,
// or a plain Error when there is no code.
func simulatedError(code string, message *string) error {
	if code != "" {
		msg := "simulated " + code
		if message != nil {
			msg = *message
		}
		return kernel.NewSandboxError(msg, kernel.SandboxErrorCode(code), "docker:shared")
	}
	msg := "simulated failure"
	if message != nil {
		msg = *message
	}
	return errors.New(msg)
}

// loggedView is the Go counterpart of the recorder's logged() proxy: it records every
// call, delegates to the real in-memory view and applies the fail, lieSha and race
// wrappers of the case.
type loggedView struct {
	sandboxhub.View
	spec  sandboxSpec
	calls []map[string]any
}

func (v *loggedView) guard(op string) error {
	if v.spec.Fail != nil && v.spec.Fail.Op == op {
		return simulatedError(v.spec.Fail.Code, v.spec.Fail.Message)
	}
	return nil
}

func (v *loggedView) ReadFile(ctx context.Context, path string, opts sandboxhub.ReadOptions) (sandboxhub.FileContent, error) {
	v.calls = append(v.calls, map[string]any{"op": "readFile", "path": path, "opts": opts})
	if err := v.guard("readFile"); err != nil {
		return sandboxhub.FileContent{}, err
	}
	read, err := v.View.ReadFile(ctx, path, opts)
	if err != nil {
		return read, err
	}
	if v.spec.Race != nil && read.Content == v.spec.Race.When {
		if _, err := v.View.WriteFile(ctx, path, v.spec.Race.Writes, sandboxhub.WriteOptions{}); err != nil {
			return sandboxhub.FileContent{}, err
		}
	}
	if v.spec.LieSha != "" && opts.MaxBytes != nil && *opts.MaxBytes == 1 {
		read.SHA256 = v.spec.LieSha
	}
	return read, nil
}

func (v *loggedView) WriteFile(ctx context.Context, path, content string, opts sandboxhub.WriteOptions) (sandboxhub.WriteResult, error) {
	v.calls = append(v.calls, map[string]any{"op": "writeFile", "path": path, "content": content, "opts": opts})
	if err := v.guard("writeFile"); err != nil {
		return sandboxhub.WriteResult{}, err
	}
	return v.View.WriteFile(ctx, path, content, opts)
}

func (v *loggedView) Stat(ctx context.Context, path string) (*sandboxhub.FileStat, error) {
	v.calls = append(v.calls, map[string]any{"op": "stat", "path": path})
	if err := v.guard("stat"); err != nil {
		return nil, err
	}
	return v.View.Stat(ctx, path)
}

func (v *loggedView) List(ctx context.Context, path string, opts sandboxhub.ListOptions) (sandboxhub.ListResult, error) {
	v.calls = append(v.calls, map[string]any{"op": "list", "path": path, "opts": opts})
	if err := v.guard("list"); err != nil {
		return sandboxhub.ListResult{}, err
	}
	return v.View.List(ctx, path, opts)
}

func (v *loggedView) Search(ctx context.Context, q sandboxhub.SearchQuery) (sandboxhub.SearchResult, error) {
	v.calls = append(v.calls, map[string]any{"op": "search", "query": q})
	if err := v.guard("search"); err != nil {
		return sandboxhub.SearchResult{}, err
	}
	return v.View.Search(ctx, q)
}

func (v *loggedView) Exec(ctx context.Context, req sandboxhub.ExecRequest, opts sandboxhub.ExecOptions) (sandboxhub.ExecResult, error) {
	v.calls = append(v.calls, map[string]any{"op": "exec", "request": req})
	if err := v.guard("exec"); err != nil {
		return sandboxhub.ExecResult{}, err
	}
	if v.spec.ExecResult != nil {
		return *v.spec.ExecResult, nil
	}
	return v.View.Exec(ctx, req, opts)
}

// workDir is the working directory of the agent a1 in the shared in-memory sandbox.
const workDir = "/workspace/agents/a1"

// environment is the sandbox side of one case: the getSandbox of the tool context, the
// logging view (nil without a sandbox) and the real view the files are dumped through.
type environment struct {
	getSandbox func(context.Context) (sandboxhub.View, error)
	logged     *loggedView
	plain      sandboxhub.View
}

func newEnvironment(t *testing.T, spec sandboxSpec) environment {
	t.Helper()
	switch spec.Mode {
	case "none", "":
		return environment{}
	case "throws":
		err := simulatedError(spec.Code, spec.Message)
		return environment{getSandbox: func(context.Context) (sandboxhub.View, error) { return nil, err }}
	case "isolated", "host":
	default:
		t.Fatalf("unknown sandbox mode %q", spec.Mode)
	}
	opts := []aecmock.InMemoryOption{aecmock.WithClock(wiretest.Clock(1700000000000, 1))}
	if spec.Mode == "host" {
		opts = append(opts, aecmock.WithKind(sandboxhub.KindHost))
	}
	if spec.Limits != nil {
		opts = append(opts, aecmock.WithLimits(*spec.Limits))
	}
	if spec.Capabilities != nil {
		caps := make([]sandboxhub.Capability, len(*spec.Capabilities))
		for i, c := range *spec.Capabilities {
			caps[i] = sandboxhub.Capability(c)
		}
		opts = append(opts, aecmock.WithCapabilities(caps...))
	}
	provider := aecmock.NewInMemoryProvider(opts...)
	if _, err := provider.Create(t.Context(), sandboxhub.Spec{Kind: sandboxhub.KindDocker}, sandboxhub.CreateContext{ID: "docker:shared", Tenancy: sandboxhub.TenancyShared}); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	sb, ok := provider.Sandbox("docker:shared")
	if !ok {
		t.Fatal("the provider lost its sandbox")
	}
	view, err := sb.ForAgent(t.Context(), "a1")
	if err != nil {
		t.Fatalf("ForAgent: %v", err)
	}
	for _, p := range spec.Planted {
		if err := sb.PlantFile(workDir+"/"+p.Path, p.Content); err != nil {
			t.Fatalf("PlantFile(%s): %v", p.Path, err)
		}
	}
	for _, l := range spec.Symlinks {
		if err := sb.Symlink(workDir+"/"+l.Link, l.Target); err != nil {
			t.Fatalf("Symlink(%s): %v", l.Link, err)
		}
	}
	logged := &loggedView{View: view, spec: spec}
	return environment{
		getSandbox: func(context.Context) (sandboxhub.View, error) { return logged, nil },
		logged:     logged,
		plain:      view,
	}
}

// filesOf lists every file of the working directory with its content, sorted by path,
// like the recorder's filesOf.
func filesOf(t *testing.T, view sandboxhub.View) [][2]string {
	t.Helper()
	yes, limit := true, 1000
	listed, err := view.List(t.Context(), ".", sandboxhub.ListOptions{Recursive: &yes, IncludeHidden: &yes, MaxEntries: &limit})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	out := [][2]string{}
	for _, e := range listed.Entries {
		if e.Type != sandboxhub.EntryFile {
			continue
		}
		fc, err := view.ReadFile(t.Context(), e.Path, sandboxhub.ReadOptions{})
		if err != nil {
			t.Fatalf("ReadFile(%s): %v", e.Path, err)
		}
		out = append(out, [2]string{e.Path, fc.Content})
	}
	slices.SortFunc(out, func(a, b [2]string) int { return strings.Compare(a[0], b[0]) })
	return out
}

// normalized is v after a JSON round trip, so a call log built in Go and one decoded
// from the fixture compare equal whatever the key order or the number type was.
func normalized(t *testing.T, v any) any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("encode %T: %v", v, err)
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("decode %s: %v", b, err)
	}
	return out
}
