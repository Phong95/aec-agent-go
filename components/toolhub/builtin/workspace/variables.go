package workspace

import (
	"context"
	"encoding/json"
	"maps"

	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/internal/jsstr"
	"github.com/Phong95/aec-agent-go/internal/nilcheck"
	"github.com/Phong95/aec-agent-go/internal/toolkit"
)

// variablesProperty is the A-215 variables parameter of terminal and run_code
// (VARIABLES_PARAM of core/ts).
func variablesProperty() toolhub.Property {
	return toolhub.Property{Name: "variables", Schema: toolhub.JSONSchema{
		Type:        "array",
		Items:       &toolhub.JSONSchema{Type: "string"},
		Description: "Names of your variables to pass as environment variables of the same name (read them as $NAME)",
	}}
}

// variableEnv is the A-215 environment of a command that asked for variables - each value
// under its name. It returns a tool error instead when the list is malformed or a name is
// unknown (not retried). No variables (absent, null, an empty list, or only blank names)
// gives a nil environment and no error.
func variableEnv(ctx context.Context, toolName string, a toolkit.Args, tc toolhub.Context) (map[string]string, *toolhub.Output) {
	raw, ok := a.Raw("variables")
	if !ok || string(raw) == "null" {
		return nil, nil
	}
	var items []any
	if err := json.Unmarshal(raw, &items); err != nil {
		out := argError(toolName, `"variables" must be a list of variable names`)
		return nil, &out
	}
	names := make([]string, 0, len(items))
	for _, it := range items {
		s, isString := it.(string)
		if !isString {
			out := argError(toolName, `"variables" must be a list of variable names`)
			return nil, &out
		}
		if n := jsstr.Trim(s); n != "" {
			names = append(names, n)
		}
	}
	if len(names) == 0 {
		return nil, nil
	}
	if nilcheck.IsNil(tc.Variables) {
		out := toolkit.Fail(toolName + ": this agent has no variables")
		return nil, &out
	}
	env, err := tc.Variables.Resolve(ctx, names)
	if err != nil {
		out := toolkit.Fail(toolName + ": " + err.Error())
		return nil, &out
	}
	return env, nil
}

// commandEnv is a command's environment (A-215): its variables, then its secrets (a secret
// of the same name wins). Nil when there are neither.
func commandEnv(ctx context.Context, toolName string, a toolkit.Args, tc toolhub.Context) (map[string]string, *toolhub.Output) {
	secretVars, failed := secretEnv(ctx, toolName, a, tc)
	if failed != nil {
		return nil, failed
	}
	vars, failed := variableEnv(ctx, toolName, a, tc)
	if failed != nil {
		return nil, failed
	}
	if vars == nil && secretVars == nil {
		return nil, nil
	}
	env := map[string]string{}
	maps.Copy(env, vars)
	maps.Copy(env, secretVars)
	return env, nil
}
