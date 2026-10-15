package workspace

import (
	"context"
	"encoding/json"

	"github.com/Phong95/aec-agent-go/components/secrethub/secret"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/internal/jsstr"
	"github.com/Phong95/aec-agent-go/internal/nilcheck"
	"github.com/Phong95/aec-agent-go/internal/toolkit"
)

// secretsProperty is the A-101 secrets parameter of terminal and run_code (SECRETS_PARAM
// of core/ts).
func secretsProperty() toolhub.Property {
	return toolhub.Property{Name: "secrets", Schema: toolhub.JSONSchema{
		Type:        "array",
		Items:       &toolhub.JSONSchema{Type: "string"},
		Description: "Names of your secrets to pass as environment variables of the same name (read them as $NAME; never print them)",
	}}
}

// secretEnv is the A-101 environment of a command that asked for secrets - each value
// under its name, after the secret allowed this tool. It returns a tool error instead when
// the list is malformed or a secret can't be used here (not retried). No secrets (absent,
// null or an empty list) gives a nil environment and no error.
func secretEnv(ctx context.Context, toolName string, a toolkit.Args, tc toolhub.Context) (map[string]string, *toolhub.Output) {
	raw, ok := a.Raw("secrets")
	if !ok || string(raw) == "null" {
		return nil, nil
	}
	var items []any
	if err := json.Unmarshal(raw, &items); err != nil {
		out := argError(toolName, `"secrets" must be a list of secret names`)
		return nil, &out
	}
	if len(items) == 0 {
		return nil, nil
	}
	names := make([]string, 0, len(items))
	for _, it := range items {
		s, isString := it.(string)
		if !isString {
			out := argError(toolName, `"secrets" must be a list of secret names`)
			return nil, &out
		}
		names = append(names, jsstr.Trim(s))
	}
	if nilcheck.IsNil(tc.Secrets) {
		out := toolkit.Fail(toolName + ": this agent has no secret store")
		return nil, &out
	}
	env, err := tc.Secrets.Resolve(ctx, names, secret.Use{Tool: toolName}, secret.ResolveContext{SessionID: tc.SessionID})
	if err != nil {
		out := toolkit.Fail(toolName + ": " + err.Error())
		return nil, &out
	}
	return env, nil
}
