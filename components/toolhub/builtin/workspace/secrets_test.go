package workspace

import (
	"context"
	"maps"
	"strings"
	"sync"
	"testing"

	"github.com/Phong95/aec-agent-go/components/persistence"
	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/components/secrethub"
	"github.com/Phong95/aec-agent-go/components/secrethub/secret"
)

// envView records the environment of every Exec and answers with exit code 0.
type envView struct {
	sandboxhub.View
	mu   sync.Mutex
	envs []map[string]string
}

func (v *envView) Exec(_ context.Context, req sandboxhub.ExecRequest, _ sandboxhub.ExecOptions) (sandboxhub.ExecResult, error) {
	v.mu.Lock()
	v.envs = append(v.envs, maps.Clone(req.Env))
	v.mu.Unlock()
	zero := 0
	return sandboxhub.ExecResult{ExitCode: &zero, Stdout: "ok"}, nil
}

// hubSecrets resolves names straight from a SecretHub's global secrets (the agent's
// secret manager does the owner lookup in a real agent).
type hubSecrets struct {
	secret.Secrets
	hub *secrethub.SecretHub
}

func (s hubSecrets) Resolve(ctx context.Context, names []string, use secret.Use, _ secret.ResolveContext) (map[string]string, error) {
	out := map[string]string{}
	for _, n := range names {
		v, err := s.hub.Reveal(ctx, "", n, use, secrethub.RevealContext{})
		if err != nil {
			return nil, err
		}
		out[n] = v
	}
	return out, nil
}

// A-101: terminal / run_code pass allowed secrets as environment variables and refuse
// the others (the "terminal / run_code secrets" part of tests/unit/secrets.test.ts).
func TestTerminalPassesAllowedSecretsAsEnvironmentVariables(t *testing.T) {
	const apiKey = "sk-live-0123456789abcdef"
	ctx := t.Context()
	store := persistence.NewMemorySecretStore()
	hub := secrethub.NewSecretHub(func() persistence.SecretStore { return store }, secrethub.WithSecretHubProvider(secrethub.NewEphemeralSecretHubProvider()))
	if _, err := hub.Create(ctx, secrethub.CreateInput{Definition: secret.Definition{Name: "CLI_TOKEN", AllowedTools: []string{"terminal", "run_code"}}, Value: apiKey}); err != nil {
		t.Fatal(err)
	}
	if _, err := hub.Create(ctx, secrethub.CreateInput{Definition: secret.Definition{Name: "HTTP_ONLY"}, Value: apiKey + "2"}); err != nil {
		t.Fatal(err)
	}
	env, _ := setup(t)
	view := &envView{View: env}
	tc := contextFor(view)
	tc.GetSandbox = func(context.Context) (sandboxhub.View, error) { return view, nil }
	tc.Secrets = hubSecrets{hub: hub}

	terminal := NewTerminalTool(Options{})
	if out := execute(t, terminal, tc, `{"command":"deploy","secrets":["CLI_TOKEN"]}`); isError(out) {
		t.Fatalf("terminal = %+v", out)
	}
	if len(view.envs) != 1 || view.envs[0]["CLI_TOKEN"] != apiKey || len(view.envs[0]) != 1 {
		t.Fatalf("envs = %v", view.envs)
	}
	refused := execute(t, terminal, tc, `{"command":"deploy","secrets":["HTTP_ONLY"]}`)
	if !notRetryable(refused) || !strings.Contains(refused.Content, "may not be used by terminal") {
		t.Fatalf("refused = %+v", refused)
	}
	if out := execute(t, terminal, tc, `{"command":"x","secrets":"CLI_TOKEN"}`); !isError(out) {
		t.Fatalf("a string is not a list: %+v", out)
	}
	if len(view.envs) != 1 {
		t.Fatalf("a refused call ran: %v", view.envs)
	}
	// run_code takes them the same way; no secrets means no environment.
	if out := execute(t, NewRunCodeTool(Options{}), tc, `{"language":"python","code":"print(1)","secrets":["CLI_TOKEN"]}`); isError(out) {
		t.Fatalf("run_code = %+v", out)
	}
	if out := execute(t, terminal, tc, `{"command":"ls","secrets":[]}`); isError(out) {
		t.Fatalf("terminal = %+v", out)
	}
	if view.envs[1]["CLI_TOKEN"] != apiKey || view.envs[2] != nil {
		t.Fatalf("envs = %v", view.envs)
	}
	// Without a secret store the call is refused.
	tc.Secrets = nil
	if out := execute(t, terminal, tc, `{"command":"deploy","secrets":["CLI_TOKEN"]}`); !strings.Contains(out.Content, "this agent has no secret store") {
		t.Fatalf("no store = %+v", out)
	}
}
