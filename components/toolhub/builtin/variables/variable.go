// Package variables holds the variable tool (A-215): list (read-only, values included -
// variables are not secret), create, update and delete the agent's own variables (each
// asks first). http_request fills {{var:NAME}}; terminal and run_code take variables as
// environment variables. The tool sees the agent's variables only through
// toolhub.Context.Variables, which is already scoped to the agent. The tool texts are those
// of the core/ts tool of the same name.
package variables

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/components/variablehub/variable"
	"github.com/Phong95/aec-agent-go/internal/jsonx"
	"github.com/Phong95/aec-agent-go/internal/jsstr"
	"github.com/Phong95/aec-agent-go/internal/nilcheck"
	"github.com/Phong95/aec-agent-go/internal/toolkit"
	"github.com/Phong95/aec-agent-go/kernel"
)

// VariableToolName is the tool name of the variable tool, for agent allowlists.
const VariableToolName = "variable"

// emDash is U+2014, written as an escape so this source stays ASCII.
const emDash = "—"

// midDot is U+00B7.
const midDot = "·"

// actions are the actions of the variable tool, in schema order (VARIABLE_ACTIONS of
// core/ts).
var actions = []string{"list", "create", "update", "delete"}

// Actions returns the actions of the variable tool, in schema order, as a fresh slice.
func Actions() []string { return slices.Clone(actions) }

// ActionOf is the call's action, trimmed and lower-cased; "" when it is not one of Actions
// (variableActionOf of core/ts).
func ActionOf(args kernel.Arguments) string {
	raw, _ := toolkit.DecodeArgs(args).String("action")
	action := strings.ToLower(jsstr.Trim(raw))
	if slices.Contains(actions, action) {
		return action
	}
	return ""
}

// ToolNames returns the tool names of the variable tools (VARIABLE_TOOL_NAMES of core/ts),
// as a fresh slice.
func ToolNames() []string { return []string{VariableToolName} }

// NewTools returns the variable tool - register it with AgentCore.UseTool (the
// createVariableTools of core/ts).
func NewTools() []toolhub.Handler { return []toolhub.Handler{NewTool()} }

// Tool is the variable tool (A-215). Global variables are managed by people (the host's
// UI / API). Subagents never get it. Strict-schema fillers: an empty value or description
// means "not given" (update keeps the old one; create refuses an empty value). It holds no
// state, so it is safe for concurrent use.
type Tool struct{}

var (
	_ toolhub.Handler    = (*Tool)(nil)
	_ toolhub.CallRisker = (*Tool)(nil)
)

// NewTool returns the variable tool.
func NewTool() *Tool { return &Tool{} }

// Name returns the plugin name.
func (*Tool) Name() string { return "tool:variable" }

// Version returns the plugin version.
func (*Tool) Version() string { return "1.0.0" }

// RiskFor is the risk of one call: list is read-only; every other action (and an unknown
// one) is consequential.
func (*Tool) RiskFor(args kernel.Arguments) toolhub.Risk {
	if ActionOf(args) == "list" {
		return toolhub.RiskReadOnly
	}
	return toolhub.RiskConsequential
}

// Definition returns the tool definition (a fresh value on every call).
func (*Tool) Definition() toolhub.Definition {
	str := func(desc string) toolhub.JSONSchema { return toolhub.JSONSchema{Type: "string", Description: desc} }
	return toolhub.Definition{
		Name: VariableToolName,
		Description: "Your variables: settings that are not secret (a base URL, a region, an id), listed with their values in <available_variables>. " +
			"Use one as {{var:NAME}} in http_request (url, headers, body) or job prompts, or through `variables` in terminal / run_code. " +
			"list: the variables you can use. create / update / delete: your own (every action but list asks the user first). " +
			"Never store a secret's value here " + emDash + " that stays a secret.",
		Parameters: toolhub.JSONSchema{
			Type: "object",
			Properties: toolhub.NewProperties(
				toolhub.Property{Name: "action", Schema: toolhub.JSONSchema{Type: "string", Enum: slices.Clone(actions), Description: "list, create, update (value and/or description) or delete"}},
				toolhub.Property{Name: "name", Schema: str("Every action but list: upper-case name, e.g. API_BASE_URL " + emDash + " what you write in {{var:NAME}}")},
				toolhub.Property{Name: "value", Schema: str("create: the value; update: the new value (leave empty to keep it)")},
				toolhub.Property{Name: "description", Schema: str("What it is for (create / update; leave empty to keep it)")},
			),
			Required: []string{"action"},
		},
		Risk: toolhub.RiskConsequential,
		// No lasting changes from a subagent.
		Subagent: toolhub.SubagentNever,
	}
}

// Execute runs one action.
func (t *Tool) Execute(ctx context.Context, raw kernel.Arguments, tc toolhub.Context) (toolhub.Output, error) {
	action := ActionOf(raw)
	if action == "" {
		return toolkit.Fail("variable: action must be one of " + strings.Join(actions, ", ")), nil
	}
	label := "variable " + action
	vars := tc.Variables
	if nilcheck.IsNil(vars) {
		return toolkit.Fail(label + ": this agent has no variable store."), nil
	}
	out, err := t.run(ctx, vars, action, toolkit.DecodeArgs(raw))
	if err != nil {
		var ve *variable.Error
		if errors.As(err, &ve) {
			return toolkit.Fail(label + ": " + ve.Message), nil
		}
		return toolhub.Error(label + ": " + err.Error()), nil
	}
	return out, nil
}

func (t *Tool) run(ctx context.Context, vars variable.Variables, action string, a toolkit.Args) (toolhub.Output, error) {
	if action == "list" {
		all, err := vars.List(ctx)
		if err != nil {
			return toolhub.Output{}, err
		}
		if len(all) == 0 {
			return toolhub.Text("You have no variables yet."), nil
		}
		lines := make([]string, 0, len(all))
		for _, v := range all {
			lines = append(lines, "- "+Describe(v))
		}
		return toolhub.Text(strings.Join(lines, "\n")), nil
	}
	rawName, _ := a.String("name")
	name, err := variable.CheckName(rawName)
	if err != nil {
		return toolhub.Output{}, err
	}
	value, hasValue := a.String("value")
	description, _ := a.String("description")
	if jsstr.Trim(description) == "" {
		description = ""
	}
	all, err := vars.List(ctx)
	if err != nil {
		return toolhub.Output{}, err
	}
	own := slices.ContainsFunc(all, func(v variable.AgentInfo) bool { return v.Name == name && v.Scope == variable.ScopeAgent })
	switch action {
	case "create":
		if own {
			return toolhub.Output{}, variable.NewError("you already have a variable named "+name+" "+emDash+" use update to change it", variable.CodeAlreadyExists)
		}
		if !hasValue || value == "" {
			return toolhub.Output{}, variable.NewError("create needs a value", variable.CodeInvalidArgument)
		}
		created, err := vars.Create(ctx, variable.Definition{Name: name, Value: value, Description: description})
		if err != nil {
			return toolhub.Output{}, err
		}
		return toolhub.Text("Stored variable " + Describe(created) + ". Use it as {{var:" + name + "}}."), nil
	case "update":
		if !own {
			return toolhub.Output{}, variable.NewError("you have no variable named "+name, variable.CodeNotFound)
		}
		var patch variable.Patch
		if hasValue && value != "" {
			patch.Value = &value
		}
		if description != "" {
			patch.Description = &description
		}
		if patch.Value == nil && patch.Description == nil {
			return toolhub.Output{}, variable.NewError("update needs a new value or description", variable.CodeInvalidArgument)
		}
		updated, err := vars.Update(ctx, name, patch)
		if err != nil {
			return toolhub.Output{}, err
		}
		return toolhub.Text("Updated variable " + Describe(updated) + "."), nil
	default: // delete
		if !own {
			return toolhub.Output{}, variable.NewError("you have no variable named "+name, variable.CodeNotFound)
		}
		if err := vars.Delete(ctx, name); err != nil {
			return toolhub.Output{}, err
		}
		return toolhub.Text("Deleted variable " + name + "."), nil
	}
}

// Describe is one variable as the model sees it (value included: variables are not
// secret) - describeVariable of core/ts.
func Describe(v variable.AgentInfo) string {
	whose := "global"
	if v.Scope == variable.ScopeAgent {
		whose = "yours"
	}
	quoted, err := jsonx.Marshal(v.Value)
	if err != nil {
		quoted = []byte(`""`)
	}
	parts := []string{v.Name + " = " + string(quoted) + " (" + whose + ")"}
	if v.Description != "" {
		parts = append(parts, v.Description)
	}
	return strings.Join(parts, " "+midDot+" ")
}
