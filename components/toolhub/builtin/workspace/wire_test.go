package workspace

import (
	"bytes"
	"encoding/json"
	"math"
	"reflect"
	"testing"

	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/internal/wiretest"
	"github.com/Phong95/aec-agent-go/kernel"
)

// builtin-workspace-tools.json is recorded by wiregen from reference workspace tools
// over a real in-memory sandbox wrapped in a logging proxy (see
// tools/wiregen/builtin/workspace.cases.mjs). Each case carries its inputs, the expected
// output, the view calls it made and the files the workspace held
// afterwards. This replay rebuilds the same environment from aecmock's in-memory
// sandbox, runs the Go tool and compares everything.

type wireCase struct {
	Name    string          `json:"name"`
	Tool    string          `json:"tool"`
	Options json.RawMessage `json:"options"`
	Sandbox sandboxSpec     `json:"sandbox"`
	Args    json.RawMessage `json:"args"`
	Output  json.RawMessage `json:"output"`
	Calls   json.RawMessage `json:"calls"`
	Files   *[][2]string    `json:"files"`
}

type wireDefinition struct {
	Tool       string          `json:"tool"`
	Options    json.RawMessage `json:"options"`
	Name       string          `json:"name"`
	Version    string          `json:"version"`
	Definition json.RawMessage `json:"definition"`
}

type wireFixture struct {
	Definitions []wireDefinition `json:"definitions"`
	Cases       []wireCase       `json:"cases"`
}

func loadWireFixture(t *testing.T) wireFixture {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(wiretest.Load(t, "builtin-workspace-tools.json")))
	dec.DisallowUnknownFields()
	var fx wireFixture
	if err := dec.Decode(&fx); err != nil {
		t.Fatalf("decode builtin-workspace-tools: %v", err)
	}
	return fx
}

// optionsFrom rebuilds the Options of a recorded options object. A number is used only
// when it is finite and greater than zero, floored; every other value (a string, null
// for NaN or Infinity, zero, a negative number) means the default. The Go fields are ints
// with the same positive-or-default rule, so a fractional recorded value is floored here
// before it is handed to the tool.
func optionsFrom(t *testing.T, raw json.RawMessage) Options {
	t.Helper()
	if len(raw) == 0 {
		return Options{}
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("options %s: %v", raw, err)
	}
	num := func(key string) int {
		f, ok := m[key].(float64)
		if !ok || math.IsNaN(f) || math.IsInf(f, 0) || f <= 0 {
			return 0
		}
		return int(math.Floor(f))
	}
	return Options{
		MaxReadBytes:          num("maxReadBytes"),
		MaxDisplayOutputBytes: num("maxDisplayOutputBytes"),
		DefaultTimeoutMs:      num("defaultTimeoutMs"),
		MaxTimeoutMs:          num("maxTimeoutMs"),
		MaxListEntries:        num("maxListEntries"),
		MaxSearchMatches:      num("maxSearchMatches"),
	}
}

// handlerFor builds the tool a recorded case ran. A tool name the Go package does not
// know fails the test, so a tool added to the recorder cannot be forgotten here.
func handlerFor(t *testing.T, name string, o Options) toolhub.Handler {
	t.Helper()
	for _, h := range NewTools(o) {
		if h.Definition().Name == name {
			return h
		}
	}
	t.Fatalf("the fixture holds an entry for the tool %q, which NewTools does not build", name)
	return nil
}

// execNotRetriedDivergences are the recorded cases where terminal or run_code fail with an
// error that is no SandboxError. The recorded output leaves retryable unset, which would
// let the harness re-run a command that may already have had its side effects; the tool
// marks the output not retryable ("exec failures are not retried", withExecSandbox). Each
// holds the expected output.
var execNotRetriedDivergences = map[string]string{
	"exec fails with a plain error": `{"content":"terminal failed: runner offline","isError":true,"retryable":false}`,
}

// TestWorkspaceToolsMatchGolden replays builtin-workspace-tools.json: the name, version and
// definition of every tool and option variant, and for every case the output bytes, the
// calls the tool made through the view and the files the workspace held afterwards.
func TestWorkspaceToolsMatchGolden(t *testing.T) {
	fx := loadWireFixture(t)
	if len(fx.Cases) < 30 {
		t.Fatalf("the corpus holds %d cases, want at least 30", len(fx.Cases))
	}

	for _, d := range fx.Definitions {
		h := handlerFor(t, d.Tool, optionsFrom(t, d.Options))
		if h.Name() != d.Name || h.Version() != d.Version {
			t.Errorf("%s: tool = %s %s, want the TS %s %s", d.Tool, h.Name(), h.Version(), d.Name, d.Version)
		}
		got, err := kernel.EncodeJSON(h.Definition())
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(d.Definition) {
			t.Errorf("%s with options %s: definition differs from TS\n got: %s\nwant: %s", d.Tool, d.Options, got, d.Definition)
		}
	}

	for _, c := range fx.Cases {
		t.Run(c.Name, func(t *testing.T) {
			h := handlerFor(t, c.Tool, optionsFrom(t, c.Options))
			env := newEnvironment(t, c.Sandbox)
			tc := toolhub.Context{SessionID: "s1", AgentID: "a1", Emit: func(string, any) {}, GetSandbox: env.getSandbox}
			args, err := kernel.NewArguments(c.Args)
			if err != nil {
				t.Fatalf("arguments: %v", err)
			}

			out, err := h.Execute(t.Context(), args, tc)
			if err != nil {
				t.Fatalf("Execute returned an error (every failure must be an output): %v", err)
			}
			got, err := kernel.EncodeJSON(out)
			if err != nil {
				t.Fatal(err)
			}
			want := string(c.Output)
			if goWant, ok := execNotRetriedDivergences[c.Name]; ok {
				if goWant == want {
					t.Fatalf("the TS output equals the Go one now; drop %q from execNotRetriedDivergences", c.Name)
				}
				want = goWant
			}
			if string(got) != want {
				t.Errorf("output differs from the expected one\n got: %s\nwant: %s\n  TS: %s", got, want, c.Output)
			}

			var wantCalls any
			if err := json.Unmarshal(c.Calls, &wantCalls); err != nil {
				t.Fatalf("calls: %v", err)
			}
			gotCalls := []map[string]any{}
			if env.logged != nil && env.logged.calls != nil {
				gotCalls = env.logged.calls
			}
			if g := normalized(t, gotCalls); !reflect.DeepEqual(g, wantCalls) {
				t.Errorf("view calls differ from TS\n got: %v\nwant: %v", g, wantCalls)
			}

			if env.plain != nil && !c.Sandbox.NoFiles {
				if c.Files == nil {
					t.Fatal("the fixture has no files for a case with a sandbox")
				}
				if g := filesOf(t, env.plain); !reflect.DeepEqual(g, *c.Files) {
					t.Errorf("workspace files differ from TS\n got: %q\nwant: %q", g, *c.Files)
				}
			}
		})
	}
}
