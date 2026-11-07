package workspace

import (
	"context"
	"strings"
	"testing"

	"github.com/Phong95/aec-agent-go/components/persistence"
	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/components/secrethub"
	"github.com/Phong95/aec-agent-go/components/secrethub/secret"
	"github.com/Phong95/aec-agent-go/components/variablehub/variable"
)

// mapVariables resolves names from a map (the agent's variable manager does the owner
// lookup in a real agent).
type mapVariables struct {
	variable.Variables
	values map[string]string
}

func (m mapVariables) Resolve(_ context.Context, names []string) (map[string]string, error) {
	out := map[string]string{}
	for _, n := range names {
		v, ok := m.values[n]
		if !ok {
			return nil, variable.NewError("no variable named "+n+" is available to this agent", variable.CodeNotFound)
		}
		out[n] = v
	}
	return out, nil
}

// A-215: terminal / run_code pass the named variables as environment variables; a secret
// of the same name wins; an unknown name refuses the call.
func TestTerminalPassesVariablesAsEnvironmentVariables(t *testing.T) {
	ctx := t.Context()
	env, _ := setup(t)
	view := &envView{View: env}
	tc := contextFor(view)
	tc.GetSandbox = func(context.Context) (sandboxhub.View, error) { return view, nil }
	tc.Variables = mapVariables{values: map[string]string{"REGION": "eu", "TOKEN": "from-variable"}}
	store := persistence.NewMemorySecretStore()
	hub := secrethub.NewSecretHub(func() persistence.SecretStore { return store }, secrethub.WithSecretHubProvider(secrethub.NewEphemeralSecretHubProvider()))
	if _, err := hub.Create(ctx, secrethub.CreateInput{Definition: secret.Definition{Name: "TOKEN", AllowedTools: []string{"terminal"}}, Value: "from-secret-123"}); err != nil {
		t.Fatal(err)
	}
	tc.Secrets = hubSecrets{hub: hub}

	terminal := NewTerminalTool(Options{})
	if out := execute(t, terminal, tc, `{"command":"deploy","variables":["REGION","TOKEN"],"secrets":["TOKEN"]}`); isError(out) {
		t.Fatalf("terminal = %+v", out)
	}
	if len(view.envs) != 1 || view.envs[0]["REGION"] != "eu" || view.envs[0]["TOKEN"] != "from-secret-123" {
		t.Fatalf("envs = %v", view.envs)
	}
	if out := execute(t, terminal, tc, `{"command":"ls","variables":[]}`); isError(out) {
		t.Fatalf("empty list = %+v", out)
	}
	if view.envs[1] != nil {
		t.Fatalf("an empty list gave an environment: %v", view.envs[1])
	}
	refused := execute(t, terminal, tc, `{"command":"deploy","variables":["NOPE"]}`)
	if !notRetryable(refused) || !strings.Contains(refused.Content, "no variable named NOPE") {
		t.Fatalf("refused = %+v", refused)
	}
	if out := execute(t, terminal, tc, `{"command":"x","variables":"REGION"}`); !isError(out) {
		t.Fatalf("a string is not a list: %+v", out)
	}
	if out := execute(t, NewRunCodeTool(Options{}), tc, `{"language":"python","code":"print(1)","variables":["REGION"]}`); isError(out) {
		t.Fatalf("run_code = %+v", out)
	}
	if view.envs[len(view.envs)-1]["REGION"] != "eu" {
		t.Fatalf("run_code env = %v", view.envs[len(view.envs)-1])
	}
	tc.Variables = nil
	if out := execute(t, terminal, tc, `{"command":"deploy","variables":["REGION"]}`); !strings.Contains(out.Content, "this agent has no variables") {
		t.Fatalf("no store = %+v", out)
	}
}
