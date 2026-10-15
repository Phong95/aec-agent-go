package artifacts

import (
	"context"
	"strings"
	"testing"

	"github.com/Phong95/aec-agent-go/components/artifacthub"
	"github.com/Phong95/aec-agent-go/components/artifacthub/artifact"
	"github.com/Phong95/aec-agent-go/components/persistence"
	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/components/toolhub"
)

// A fractional expected_version cannot be handed to the view (its field is an int): the
// tool refuses it before the view is called, with an argument error, instead of a
// conflict from the view.
func TestUpdateRefusesAFractionalExpectedVersion(t *testing.T) {
	h := setup(t, "s1")
	execute(t, NewSaveTool(Options{}), h.tc, `{"name":"a.md","type":"text/markdown","content":"v1"}`)

	for _, v := range []string{"1.5", "0.5", "-2.25", "1e20"} {
		out := execute(t, NewUpdateTool(Options{}), h.tc, `{"name":"a.md","content":"v2","expected_version":`+v+`}`)
		if !notRetryable(out) || !strings.Contains(out.Content, `"expected_version" must be a whole number`) {
			t.Errorf("expected_version %s: output = %+v, want the whole-number argument error", v, out)
		}
	}
	if got := mustRead(t, h.env, "artifacts/a.md"); got != "v1" {
		t.Errorf("the file = %q, want it untouched", got)
	}
}

// A number beyond the double range (1e999, which JS reads as Infinity) drops that one
// argument only; the other arguments of the call are kept (the toolkit's per-key decode).
func TestHugeNumberKeepsTheOtherArguments(t *testing.T) {
	h := setup(t, "s1")
	execute(t, NewSaveTool(Options{}), h.tc, `{"name":"a.md","type":"text/markdown","content":"v1"}`)

	out := execute(t, NewUpdateTool(Options{}), h.tc, `{"name":"a.md","content":"v2","expected_version":1e999}`)
	if isError(out) {
		t.Fatalf("error output: %s", out.Content)
	}
	hasMeta(t, out, map[string]any{"version": 2})
	if got := mustRead(t, h.env, "artifacts/a.md"); got != "v2" {
		t.Errorf("the file = %q, want v2", got)
	}

	read := execute(t, NewReadTool(Options{}), h.tc, `{"name":"a.md","max_bytes":1e999}`)
	if isError(read) || !strings.Contains(read.Content, "v2") {
		t.Errorf("read with a huge max_bytes = %+v", read)
	}
}

// The agent is the one of the tool context: an agent id, session id or artifact id of
// another agent in the arguments reaches nothing (T-06-24, T-11-13).
func TestArgumentsNeverNameAnAgent(t *testing.T) {
	ph := persistence.NewPersistenceHub()
	hub := artifacthub.NewArtifactHub(func() persistence.ArtifactStore { return ph.Artifacts })
	env := sandboxOf(t, "a1", sandboxhub.KindDocker)

	theirs, err := hub.ForAgent("a2", "s1")
	if err != nil {
		t.Fatal(err)
	}
	content := "secret"
	saved, err := theirs.Save(t.Context(), artifact.SaveInput{Name: "secret.md", Type: "text/markdown", Content: &content}, env)
	if err != nil {
		t.Fatal(err)
	}

	tc := contextOn(t, hub, env, "s1", nil)
	for _, h := range []struct {
		tool toolhub.Handler
		args string
	}{
		{NewReadTool(Options{}), `{"id":"` + saved.Artifact.ID + `","name":"secret.md","agentId":"a2","agent_id":"a2"}`},
		{NewUpdateTool(Options{}), `{"id":"` + saved.Artifact.ID + `","content":"hijacked","agentId":"a2"}`},
	} {
		out := execute(t, h.tool, tc, h.args)
		if !notRetryable(out) {
			t.Errorf("%s: output = %+v, want a non-retryable error", h.tool.Definition().Name, out)
		}
		hasMeta(t, out, map[string]any{"code": "not_found"})
	}
	list := execute(t, NewListTool(Options{}), tc, `{"agentId":"a2"}`)
	hasMeta(t, list, map[string]any{"count": 0})
}

// A tool run outside a hub has no Emit; the call must not panic. A context whose
// GetArtifacts returns no view is the same as having none.
func TestNoEmitAndNoView(t *testing.T) {
	h := setup(t, "s1")
	tc := h.tc
	tc.Emit = nil
	out := execute(t, NewSaveTool(Options{}), tc, `{"name":"a.md","type":"text/markdown","content":"x"}`)
	if isError(out) {
		t.Fatalf("error output: %s", out.Content)
	}

	tc.GetArtifacts = func() artifact.View { return nil }
	out = execute(t, NewListTool(Options{}), tc, `{}`)
	if !notRetryable(out) || !strings.Contains(out.Content, "no artifact store") {
		t.Errorf("output = %+v, want the no artifact store error", out)
	}
}

// A sandbox accessor that returns no view is the same as having no accessor.
func TestAccessorWithoutAView(t *testing.T) {
	h := setup(t, "s1")
	tc := h.tc
	tc.GetSandbox = func(context.Context) (sandboxhub.View, error) { return nil, nil }
	out := execute(t, NewSaveTool(Options{}), tc, `{"name":"a.md","type":"text/markdown","content":"x"}`)
	if !notRetryable(out) || !strings.Contains(out.Content, "no sandbox") {
		t.Errorf("output = %+v, want the no sandbox error", out)
	}
}

// The definitions are fresh values: a caller that changes one cannot change the next.
func TestDefinitionIsFresh(t *testing.T) {
	for _, h := range NewTools(Options{}) {
		d := h.Definition()
		d.Name = "changed"
		d.Parameters.Required = append(d.Parameters.Required, "changed")
		if again := h.Definition(); again.Name == "changed" || len(again.Parameters.Required) == len(d.Parameters.Required) {
			t.Errorf("%s: the definition is shared between calls", h.Name())
		}
	}
}
