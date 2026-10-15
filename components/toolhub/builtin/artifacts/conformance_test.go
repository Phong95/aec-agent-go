package artifacts

import (
	"context"
	"slices"
	"testing"

	"github.com/Phong95/aec-agent-go/components/artifacthub"
	"github.com/Phong95/aec-agent-go/components/artifacthub/artifact"
	"github.com/Phong95/aec-agent-go/components/persistence"
	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/conformance"
	"github.com/Phong95/aec-agent-go/kernel"
)

// withStore wraps a handler so that every Execute gets a tool context with an artifact
// store and a sandbox. The conformance suite builds its own bare toolhub.Context, so a
// factory cannot inject them; this decorator is how the suite also reaches the path of a
// tool that has an artifact store.
type withStore struct {
	toolhub.Handler
	view artifact.View
	env  sandboxhub.View
}

func (w withStore) Execute(ctx context.Context, args kernel.Arguments, tc toolhub.Context) (toolhub.Output, error) {
	tc.GetArtifacts = func() artifact.View { return w.view }
	tc.GetSandbox = func(context.Context) (sandboxhub.View, error) { return w.env, nil }
	return w.Handler.Execute(ctx, args, tc)
}

// TestArtifactToolsConformance runs the tool-handler conformance suite on every artifact
// tool, once with no artifact store (the tool reports that the agent has none) and once
// with an artifact store over an in-memory sandbox.
func TestArtifactToolsConformance(t *testing.T) {
	cases := []struct {
		name    string
		factory func(Options) toolhub.Handler
		args    string
	}{
		{"save_artifact", func(o Options) toolhub.Handler { return NewSaveTool(o) }, `{"name":"a.md","type":"text/markdown","content":"hello"}`},
		{"update_artifact", func(o Options) toolhub.Handler { return NewUpdateTool(o) }, `{"name":"a.md","content":"bye"}`},
		{"read_artifact", func(o Options) toolhub.Handler { return NewReadTool(o) }, `{"name":"a.md"}`},
		{"list_artifacts", func(o Options) toolhub.Handler { return NewListTool(o) }, `{}`},
	}
	if len(cases) != len(ToolNames()) {
		t.Fatalf("the table has %d tools, ToolNames has %d: keep them in step", len(cases), len(ToolNames()))
	}
	for _, c := range cases {
		args, err := kernel.NewArguments([]byte(c.args))
		if err != nil {
			t.Fatal(err)
		}
		t.Run(c.name+" with no artifact store", func(t *testing.T) {
			conformance.ToolHandler(t, func(*testing.T) toolhub.Handler { return c.factory(Options{}) }, args)
		})
		t.Run(c.name+" with an artifact store", func(t *testing.T) {
			conformance.ToolHandler(t, func(t *testing.T) toolhub.Handler {
				ph := persistence.NewPersistenceHub()
				hub := artifacthub.NewArtifactHub(func() persistence.ArtifactStore { return ph.Artifacts })
				view, err := hub.ForAgent("a1", "s1")
				if err != nil {
					t.Fatal(err)
				}
				env := sandboxOf(t, "a1", sandboxhub.KindDocker)
				if _, err := view.Save(t.Context(), artifact.SaveInput{Name: "a.md", Type: "text/markdown", Content: ptr("hello")}, env); err != nil {
					t.Fatal(err)
				}
				return withStore{Handler: c.factory(Options{}), view: view, env: env}
			}, args)
		})
	}
}

func ptr[T any](v T) *T { return &v }

// TestArtifactCorpusCoversEveryTool keeps the recorder honest: every tool of
// NewTools has a definition entry and cases in the recorded corpus, so a tool
// added to the package cannot go without a recording.
func TestArtifactCorpusCoversEveryTool(t *testing.T) {
	fx := loadWireFixture(t)
	defs := map[string]int{}
	for _, d := range fx.Definitions {
		defs[d.Tool]++
	}
	cases := map[string]int{}
	for _, c := range fx.Cases {
		cases[c.Tool]++
	}
	for _, name := range ToolNames() {
		if defs[name] < 3 {
			t.Errorf("%s has %d definition variants in the corpus, want at least 3 (default and two option variants)", name, defs[name])
		}
		if cases[name] < 20 {
			t.Errorf("%s has %d cases in the corpus, want at least 20", name, cases[name])
		}
	}
	for name := range cases {
		if !slices.Contains(ToolNames(), name) {
			t.Errorf("the corpus holds cases for %q, which is not an artifact tool", name)
		}
	}
}

// TestToolNames: the tool names in order, a fresh copy on every call, and the
// names of the handlers NewTools builds.
func TestToolNames(t *testing.T) {
	want := []string{"save_artifact", "update_artifact", "read_artifact", "list_artifacts"}
	if got := ToolNames(); !slices.Equal(got, want) {
		t.Fatalf("ToolNames() = %v, want %v", got, want)
	}
	first := ToolNames()
	first[0] = "changed"
	if second := ToolNames(); second[0] != "save_artifact" {
		t.Fatalf("ToolNames returned a shared slice: a change leaked into %v", second)
	}
	tools := NewTools(Options{})
	if len(tools) != len(want) {
		t.Fatalf("NewTools returned %d tools, want %d", len(tools), len(want))
	}
	for i, h := range tools {
		if h.Definition().Name != want[i] {
			t.Errorf("tool %d is %q, want %q", i, h.Definition().Name, want[i])
		}
	}
}

// TestOptionsDefaults: a zero (or negative) option field means the default, and a
// positive one is used. The two ceilings are visible in what the tools show the model.
func TestOptionsDefaults(t *testing.T) {
	for _, o := range []Options{{}, {MaxReadBytes: -1, MaxListEntries: -5}} {
		if got := resolve(o); got.maxReadBytes != 100_000 || got.maxListEntries != 50 {
			t.Errorf("resolve(%+v) = %+v, want the TS defaults 100000 and 50", o, got)
		}
	}
	if got := resolve(Options{MaxReadBytes: 7, MaxListEntries: 3}); got.maxReadBytes != 7 || got.maxListEntries != 3 {
		t.Errorf("resolve = %+v, want 7 and 3", got)
	}
}
