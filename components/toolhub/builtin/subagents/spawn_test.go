package subagents_test

import (
	"context"
	"strings"
	"testing"

	"github.com/Phong95/aec-agent-go/components/agent/subagent"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/components/toolhub/builtin/subagents"
	"github.com/Phong95/aec-agent-go/kernel"
)

// fakeSubagents records the spawn it gets and answers with run or err.
type fakeSubagents struct {
	req subagent.Request
	sc  subagent.SpawnContext
	run subagent.Run
	err error
}

func (f *fakeSubagents) Spawn(_ context.Context, req subagent.Request, sc subagent.SpawnContext) (subagent.Run, error) {
	f.req, f.sc = req, sc
	return f.run, f.err
}
func (*fakeSubagents) Targets(context.Context) []subagent.TargetInfo { return nil }
func (*fakeSubagents) List(context.Context, string) ([]subagent.Run, error) {
	return nil, nil
}
func (*fakeSubagents) Get(context.Context, string) (*subagent.Run, error) { return nil, nil }
func (*fakeSubagents) Stop(string) bool                                   { return false }
func (*fakeSubagents) Running() int                                       { return 0 }

func isErr(o toolhub.Output) bool { return o.IsError != nil && *o.IsError }

func TestSpawnSubagentDefinition(t *testing.T) {
	d := subagents.NewSpawnSubagentTool().Definition()
	if d.Name != subagents.SpawnSubagentToolName || d.Risk != toolhub.RiskConsequential {
		t.Fatalf("definition = %+v", d)
	}
	if got := d.Parameters.Properties.Names(); strings.Join(got, ",") != "subagent,task,context,output_shape" {
		t.Fatalf("properties = %v", got)
	}
	// "subagent" is checked by the brief validation, so the old "agent" name still works.
	if strings.Join(d.Parameters.Required, ",") != "task" {
		t.Fatalf("required = %v", d.Parameters.Required)
	}
}

func TestSpawnSubagentWithoutSubagents(t *testing.T) {
	out, err := subagents.NewSpawnSubagentTool().Execute(t.Context(), kernel.Arguments(`{"agent":"a","task":"t"}`), toolhub.Context{})
	if err != nil || !isErr(out) || out.Retryable == nil || *out.Retryable || !strings.Contains(out.Content, "not available here") {
		t.Fatalf("out = %+v (%v)", out, err)
	}
}

func TestSpawnSubagentPassesTheBriefAndTheTurn(t *testing.T) {
	d := int64(1000)
	f := &fakeSubagents{run: subagent.Run{ID: "r1", ChildAgentID: "a", ChildAgentName: "A", Status: subagent.StatusCompleted, Output: "report", DurationMs: &d}}
	tc := toolhub.Context{SessionID: "s1", RunID: "run-1", ToolCallID: "tc-1", Subagents: f}
	out, err := subagents.NewSpawnSubagentTool().Execute(t.Context(), kernel.Arguments(`{"subagent":"a","task":"t","context":"{\"k\":1}","output_shape":"  "}`), tc)
	if err != nil || isErr(out) || out.Retryable == nil || *out.Retryable {
		t.Fatalf("out = %+v (%v)", out, err)
	}
	if f.req.AgentID != "a" || f.req.Task != "t" || f.req.OutputShape != "" || f.req.Context.Len() != 1 {
		t.Fatalf("request = %+v", f.req)
	}
	if f.sc != (subagent.SpawnContext{SessionID: "s1", RunID: "run-1", ToolCallID: "tc-1"}) {
		t.Fatalf("spawn context = %+v", f.sc)
	}
	if !strings.Contains(out.Content, "<subagent_report>\nreport\n</subagent_report>") || !out.Metadata.Has("subagentRun") {
		t.Fatalf("out = %+v", out)
	}
}

func TestSpawnSubagentRefusalsAndFailedRuns(t *testing.T) {
	tool := subagents.NewSpawnSubagentTool()
	f := &fakeSubagents{err: subagent.NewError("nope", subagent.CodeNotAllowed)}
	out, _ := tool.Execute(t.Context(), kernel.Arguments(`{"agent":"a","task":"t"}`), toolhub.Context{Subagents: f})
	if !isErr(out) || out.Content != "spawn_subagent refused: nope" {
		t.Fatalf("refusal = %+v", out)
	}
	out, _ = tool.Execute(t.Context(), kernel.Arguments(`{"agent":"a","task":"t","context":"[1]"}`), toolhub.Context{Subagents: f})
	if !isErr(out) || !strings.Contains(out.Content, `"context" must be a JSON object`) {
		t.Fatalf("bad context = %+v", out)
	}
	f = &fakeSubagents{run: subagent.Run{ID: "r1", Status: subagent.StatusTimedOut}}
	out, _ = tool.Execute(t.Context(), kernel.Arguments(`{"agent":"a","task":"t"}`), toolhub.Context{Subagents: f})
	if !isErr(out) || out.Retryable == nil || *out.Retryable || !strings.Contains(out.Content, "status: timed_out") {
		t.Fatalf("timed out = %+v", out)
	}
}
