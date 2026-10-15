package secrets

import (
	"context"
	"strings"

	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/internal/nilcheck"
	"github.com/Phong95/aec-agent-go/internal/toolkit"
	"github.com/Phong95/aec-agent-go/kernel"
)

// ListSecretsToolName is the tool name of list_secrets, for agent allowlists.
const ListSecretsToolName = "list_secrets"

// ListSecretsTool is list_secrets (A-101): read-only. The secrets this agent can use: its
// own and the global ones it enabled, with what each is for and where it may be used.
// Never values. It holds no state, so it is safe for concurrent use.
type ListSecretsTool struct{}

var _ toolhub.Handler = (*ListSecretsTool)(nil)

// NewListSecretsTool returns the list_secrets tool.
func NewListSecretsTool() *ListSecretsTool { return &ListSecretsTool{} }

// Name returns the plugin name.
func (*ListSecretsTool) Name() string { return "tool:list-secrets" }

// Version returns the plugin version.
func (*ListSecretsTool) Version() string { return "1.0.0" }

// Definition returns the tool definition (a fresh value on every call).
func (*ListSecretsTool) Definition() toolhub.Definition {
	return toolhub.Definition{
		Name: ListSecretsToolName,
		Description: "List the secrets (API keys, tokens) you can use: name, what it is for, which tools may use it and which hosts it may be sent to. " +
			"Values are never shown " + emDash + " use a secret with {{secret:NAME}} in http_request, or `secrets` in terminal / run_code.",
		Parameters: toolhub.JSONSchema{Type: "object", Properties: toolhub.NewProperties()},
		Risk:       toolhub.RiskReadOnly,
	}
}

// Execute lists the agent's secrets.
func (*ListSecretsTool) Execute(ctx context.Context, _ kernel.Arguments, tc toolhub.Context) (toolhub.Output, error) {
	if nilcheck.IsNil(tc.Secrets) {
		return toolkit.Fail("list_secrets: this agent has no secret store."), nil
	}
	list, err := tc.Secrets.List(ctx)
	if err != nil {
		return secretToolError("list_secrets", err), nil
	}
	if len(list) == 0 {
		return toolhub.Text("You have no secrets yet."), nil
	}
	lines := make([]string, 0, len(list))
	for _, s := range list {
		lines = append(lines, "- "+describeSecret(s))
	}
	return toolhub.Text(strings.Join(lines, "\n")), nil
}

// emDash is U+2014, written as an escape so this source stays ASCII.
const emDash = "—"
