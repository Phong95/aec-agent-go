package secrets_test

import (
	"context"
	"testing"

	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/components/toolhub/builtin/secrets"
	"github.com/Phong95/aec-agent-go/kernel"
)

// A-212: the secret tool (list_secrets + manage_secret) - the "the secret tool" cases of
// core/ts tests/unit/merged-tools.test.ts; the flows are in agentcore_secrets_test.go.

func TestSecretToolRisksAndNames(t *testing.T) {
	tool := secrets.NewTool()
	d := tool.Definition()
	if d.Name != "secret" || d.Risk != toolhub.RiskConsequential || d.Subagent != toolhub.SubagentNever ||
		len(d.Parameters.Required) != 1 || d.Parameters.Required[0] != "action" {
		t.Fatalf("definition = %+v", d)
	}
	if tool.RiskFor(kernel.Arguments(`{"action":"list"}`)) != toolhub.RiskReadOnly {
		t.Fatal("list must be read-only")
	}
	for _, action := range []string{"create", "update", "set_value", "delete", "nope"} {
		if tool.RiskFor(kernel.Arguments(`{"action":"`+action+`"}`)) != toolhub.RiskConsequential {
			t.Fatalf("%s must be consequential", action)
		}
	}
	renamed := toolhub.RenamedTools()
	for _, old := range secrets.RetiredToolNames() {
		if renamed[old] != "secret" {
			t.Errorf("%s is renamed to %q, want secret", old, renamed[old])
		}
	}
}

func TestSecretToolRefusals(t *testing.T) {
	tool := secrets.NewTool()
	for args, want := range map[string]string{
		`{"action":"show"}`: "secret: action must be one of list, create, update, set_value, delete",
		`{"action":"list"}`: "secret list: this agent has no secret store.",
	} {
		out, err := tool.Execute(context.Background(), kernel.Arguments(args), toolhub.Context{})
		if err != nil || out.Content != want {
			t.Errorf("%s: %q, %v", args, out.Content, err)
		}
	}
}
