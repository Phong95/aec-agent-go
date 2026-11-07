package toolhub_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/kernel"
)

// A-209: RenamedToolNames maps the retired artifact tools to "artifact", keeping the
// order and dropping duplicates.
func TestRenamedToolNames(t *testing.T) {
	if got := toolhub.RenamedToolNames([]string{"read_file", "save_artifact", "list_artifacts", "terminal", "read_artifact"}); !slices.Equal(got, []string{"read_file", "artifact", "terminal"}) {
		t.Errorf("RenamedToolNames = %v", got)
	}
	if got := toolhub.RenamedToolNames([]string{"artifact", "update_artifact"}); !slices.Equal(got, []string{"artifact"}) {
		t.Errorf("RenamedToolNames = %v", got)
	}
	if toolhub.RenamedToolNames(nil) != nil {
		t.Error("RenamedToolNames(nil) is not nil")
	}
	if got := toolhub.RenamedToolNames([]string{}); got == nil || len(got) != 0 {
		t.Errorf("RenamedToolNames([]) = %#v, want an empty non-nil slice", got)
	}
	m := toolhub.RenamedTools()
	m["save_artifact"] = "changed"
	if toolhub.RenamedTools()["save_artifact"] != "artifact" {
		t.Error("RenamedTools returned a shared map")
	}
}

// perCallTool is a tool whose risk and sandbox operation follow its "action" argument,
// like the artifact tool: "list" is read-only with no sandbox operation.
type perCallTool struct{}

func (perCallTool) Name() string    { return "tool:per-call" }
func (perCallTool) Version() string { return "1.0.0" }
func (perCallTool) Definition() toolhub.Definition {
	return toolhub.Definition{Name: "per_call", Parameters: toolhub.JSONSchema{Type: "object"}, Risk: toolhub.RiskDraft, SandboxOperation: sandboxhub.OperationFSWrite}
}

func (perCallTool) Execute(context.Context, kernel.Arguments, toolhub.Context) (toolhub.Output, error) {
	return toolhub.Output{Content: "ok"}, nil
}

func (perCallTool) RiskFor(args kernel.Arguments) toolhub.Risk {
	if strings.Contains(string(args), `"list"`) {
		return toolhub.RiskReadOnly
	}
	return toolhub.RiskDraft
}

func (perCallTool) SandboxOperationFor(args kernel.Arguments) sandboxhub.Operation {
	if strings.Contains(string(args), `"list"`) {
		return ""
	}
	return sandboxhub.OperationFSWrite
}

// plainTool answers with its definition only.
type plainTool struct{}

func (plainTool) Name() string    { return "tool:plain" }
func (plainTool) Version() string { return "1.0.0" }
func (plainTool) Definition() toolhub.Definition {
	return toolhub.Definition{Name: "plain", Parameters: toolhub.JSONSchema{Type: "object"}, Risk: toolhub.RiskDraft, SandboxOperation: sandboxhub.OperationFSRead}
}

func (plainTool) Execute(context.Context, kernel.Arguments, toolhub.Context) (toolhub.Output, error) {
	return toolhub.Output{}, nil
}

// A-209: Authorized.SandboxOperation and ToolHub.CallRisk follow a handler's per-call
// answers, and fall back to the definition for a handler without them.
func TestPerCallRiskAndSandboxOperation(t *testing.T) {
	h := toolhub.NewToolHub()
	if err := h.Register(perCallTool{}); err != nil {
		t.Fatal(err)
	}
	if err := h.Register(plainTool{}); err != nil {
		t.Fatal(err)
	}
	list, save := kernel.Arguments(`{"action":"list"}`), kernel.Arguments(`{"action":"save"}`)

	for _, c := range []struct {
		name string
		args kernel.Arguments
		risk toolhub.Risk
		op   sandboxhub.Operation
	}{
		{"per_call", list, toolhub.RiskReadOnly, ""},
		{"per_call", save, toolhub.RiskDraft, sandboxhub.OperationFSWrite},
		{"plain", list, toolhub.RiskDraft, sandboxhub.OperationFSRead},
	} {
		auth, d, err := h.Authorize(t.Context(), c.name, c.args, toolhub.Context{}, nil)
		if err != nil || !d.Allowed {
			t.Fatalf("Authorize(%s): %v %+v", c.name, err, d)
		}
		if auth.Risk() != c.risk || auth.SandboxOperation() != c.op {
			t.Errorf("%s %s: risk %q op %q, want %q %q", c.name, c.args, auth.Risk(), auth.SandboxOperation(), c.risk, c.op)
		}
		if risk, ok := h.CallRisk(c.name, c.args); !ok || risk != c.risk {
			t.Errorf("CallRisk(%s, %s) = %q %v, want %q", c.name, c.args, risk, ok, c.risk)
		}
	}
	if _, ok := h.CallRisk("missing", list); ok {
		t.Error("CallRisk of an unregistered tool reports a risk")
	}
}

// A-211: CompanionSkills adds a merged tool's skill only for old names, once, and never
// one the profile has (companionSkills of core/ts).
func TestCompanionSkills(t *testing.T) {
	if got := toolhub.CompanionSkills([]string{"save_artifact", "list_artifacts", "open_in_app", "manage_job", "read_file"}, []string{"plan"}, nil); !slices.Equal(got, []string{"artifacts", "live-preview", "jobs"}) {
		t.Errorf("CompanionSkills = %v", got)
	}
	if got := toolhub.CompanionSkills([]string{"manage_job"}, []string{"jobs"}, nil); len(got) != 0 {
		t.Errorf("CompanionSkills = %v", got)
	}
	if got := toolhub.CompanionSkills([]string{"artifact", "preview", "job"}, nil, nil); len(got) != 0 {
		t.Errorf("already converted: CompanionSkills = %v", got)
	}
	if m := toolhub.CompanionSkillsOf(); len(m) != 3 || m["artifact"] != "artifacts" || m["preview"] != "live-preview" || m["job"] != "jobs" {
		t.Errorf("CompanionSkillsOf = %v", m)
	}
}

// A-212: an old name a host still registers stays (RenamedToolNamesKeeping, CompanionSkills
// with keep), and a stored old call runs as the merged tool's action (RenamedToolCall).
func TestRenamedKeepingAndCalls(t *testing.T) {
	keep := func(n string) bool { return n == "web_search" || n == "manage_job" }
	if got := toolhub.RenamedToolNamesKeeping([]string{"web_search", "list_jobs", "list_secrets"}, keep); !slices.Equal(got, []string{"web_search", "job", "secret"}) {
		t.Errorf("RenamedToolNamesKeeping = %v", got)
	}
	if got := toolhub.CompanionSkills([]string{"manage_job"}, nil, keep); len(got) != 0 {
		t.Errorf("CompanionSkills = %v", got)
	}
	for _, c := range []struct {
		old, tool, action string
		args              map[string]any
	}{
		{"web_search", "web", "search", map[string]any{"query": "q"}},
		{"web_extract", "web", "extract", map[string]any{"urls": []any{"u"}}},
		{"list_jobs", "job", "list", map[string]any{}},
		{"manage_job", "job", "delete", map[string]any{"action": "delete", "job": "x"}},
	} {
		tool, args := toolhub.RenamedToolCall(c.old, c.args)
		if tool != c.tool || args["action"] != c.action {
			t.Errorf("RenamedToolCall(%s) = %s %v", c.old, tool, args)
		}
	}
	in := map[string]any{"query": "q"}
	toolhub.RenamedToolCall("web_search", in)
	if _, changed := in["action"]; changed {
		t.Error("RenamedToolCall changed its argument map")
	}
	if tool, args := toolhub.RenamedToolCall("terminal", map[string]any{"command": "ls"}); tool != "terminal" || args["command"] != "ls" {
		t.Errorf("RenamedToolCall(terminal) = %s %v", tool, args)
	}
}
