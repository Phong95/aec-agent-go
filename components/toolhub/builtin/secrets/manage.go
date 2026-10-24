package secrets

import (
	"context"
	"slices"
	"strings"

	"github.com/Phong95/aec-agent-go/components/secrethub/secret"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/internal/nilcheck"
	"github.com/Phong95/aec-agent-go/internal/toolkit"
	"github.com/Phong95/aec-agent-go/kernel"
)

// ManageSecretToolName is the tool name of manage_secret, for agent allowlists.
const ManageSecretToolName = "manage_secret"

// manageActions are the actions of manage_secret, in schema order.
var manageActions = []string{"create", "update", "set_value", "delete"}

// ManageSecretTool is manage_secret (A-101): it creates, changes and deletes this agent's
// own secrets.
//
// The model never handles a value: "create" and "set_value" ask the user to type it into a
// masked field (toolhub.SecretRequester on the interaction handler), and it goes from there
// straight to the SecretHub. The result only says what was stored. A chat app or an
// unattended run can't take a value, so those actions fail there.
//
// Risk "consequential": every call waits for the user's approval (they see the name, the
// allowed hosts and tools). Global secrets are managed by people only (the host's UI/API).
// Subagents never get this tool. It holds no state, so it is safe for concurrent use.
type ManageSecretTool struct{}

var _ toolhub.Handler = (*ManageSecretTool)(nil)

// NewManageSecretTool returns the manage_secret tool.
func NewManageSecretTool() *ManageSecretTool { return &ManageSecretTool{} }

// Name returns the plugin name.
func (*ManageSecretTool) Name() string { return "tool:manage-secret" }

// Version returns the plugin version.
func (*ManageSecretTool) Version() string { return "1.0.0" }

// Definition returns the tool definition (a fresh value on every call).
func (*ManageSecretTool) Definition() toolhub.Definition {
	str := func(desc string) toolhub.JSONSchema { return toolhub.JSONSchema{Type: "string", Description: desc} }
	list := func(desc string) toolhub.JSONSchema {
		return toolhub.JSONSchema{Type: "array", Items: &toolhub.JSONSchema{Type: "string"}, Description: desc}
	}
	return toolhub.Definition{
		Name: ManageSecretToolName,
		Description: "Create, change or delete one of your secrets (an API key or token the user wants you to use). " +
			"create / set_value: the user types the value into a masked field " + emDash + " never ask for it in the chat and never put a value in these arguments. " +
			"Set allowed_hosts to the API host(s) the key is for (e.g. api.example.com, or *.example.com); a secret with no hosts can't be sent anywhere. " +
			"allowed_tools defaults to http_request; add terminal or run_code only when a command needs the key as an environment variable.",
		Parameters: toolhub.JSONSchema{
			Type: "object",
			Properties: toolhub.NewProperties(
				toolhub.Property{Name: "action", Schema: toolhub.JSONSchema{Type: "string", Enum: slices.Clone(manageActions), Description: "create, update (description/hosts/tools), set_value (a new value), delete"}},
				toolhub.Property{Name: "name", Schema: str("Upper-case name, e.g. OPENWEATHER_API_KEY " + emDash + " what you write in {{secret:NAME}}")},
				toolhub.Property{Name: "description", Schema: str("What the key is for (create/update)")},
				toolhub.Property{Name: "allowed_hosts", Schema: list("Hosts it may be sent to (create/update)")},
				toolhub.Property{Name: "allowed_tools", Schema: list(`Tools that may use it (create/update), default ["http_request"]`)},
			),
			Required: []string{"action", "name"},
		},
		Risk: toolhub.RiskConsequential,
		// A-137: no lasting changes from a subagent.
		Subagent: toolhub.SubagentNever,
	}
}

// Execute runs one action on one of the agent's own secrets.
func (t *ManageSecretTool) Execute(ctx context.Context, raw kernel.Arguments, tc toolhub.Context) (toolhub.Output, error) {
	secrets := tc.Secrets
	if nilcheck.IsNil(secrets) {
		return toolkit.Fail("manage_secret: this agent has no secret store."), nil
	}
	a := toolkit.DecodeArgs(raw)
	action, _ := a.String("action")
	if !slices.Contains(manageActions, action) {
		return toolkit.Fail("manage_secret: action must be one of " + strings.Join(manageActions, ", ")), nil
	}
	out, err := t.run(ctx, secrets, action, a, tc)
	if err != nil {
		return secretToolError("manage_secret", err), nil
	}
	return out, nil
}

func (t *ManageSecretTool) run(ctx context.Context, secrets secret.Secrets, action string, a toolkit.Args, tc toolhub.Context) (toolhub.Output, error) {
	rawName, _ := a.String("name")
	name, err := secret.CheckName(rawName)
	if err != nil {
		return toolhub.Output{}, err
	}
	var def secret.Definition
	if d, ok := a.String("description"); ok {
		def.Description = &d
	}
	if def.AllowedHosts, err = stringList(a, "allowed_hosts"); err != nil {
		return toolhub.Output{}, err
	}
	if def.AllowedTools, err = stringList(a, "allowed_tools"); err != nil {
		return toolhub.Output{}, err
	}
	list, err := secrets.List(ctx)
	if err != nil {
		return toolhub.Output{}, err
	}
	var own *secret.AgentInfo
	for i := range list {
		if list[i].Name == name && list[i].Scope == secret.ScopeAgent {
			own = &list[i]
			break
		}
	}
	notFound := secret.NewError("you have no secret named "+name, secret.CodeNotFound)
	switch action {
	case "create":
		if own != nil {
			return toolhub.Output{}, secret.NewError("you already have a secret named "+name+" "+emDash+" use set_value to give it a new value", secret.CodeAlreadyExists)
		}
		meta, err := secret.NormalizeDefinition(def, nil)
		if err != nil {
			return toolhub.Output{}, err
		}
		value, err := askValue(ctx, tc, toolhub.SecretRequest{Name: name, Description: meta.Description, AllowedHosts: meta.AllowedHosts, AllowedTools: meta.AllowedTools})
		if err != nil {
			return toolhub.Output{}, err
		}
		if value == "" {
			return toolhub.Text("The user cancelled " + emDash + " secret " + name + " was not created.").NotRetryable(), nil
		}
		desc := meta.Description
		created, err := secrets.Create(ctx, secret.Definition{Name: name, Description: &desc, AllowedHosts: meta.AllowedHosts, AllowedTools: meta.AllowedTools}, value)
		if err != nil {
			return toolhub.Output{}, err
		}
		return toolhub.Text("Stored secret " + describeSecret(created) + ". Use it as {{secret:" + name + "}}."), nil
	case "update":
		if own == nil {
			return toolhub.Output{}, notFound
		}
		updated, err := secrets.Update(ctx, name, def)
		if err != nil {
			return toolhub.Output{}, err
		}
		return toolhub.Text("Updated secret " + describeSecret(updated) + "."), nil
	case "set_value":
		if own == nil {
			return toolhub.Output{}, notFound
		}
		value, err := askValue(ctx, tc, toolhub.SecretRequest{
			Name: name, Description: own.Description, AllowedHosts: own.AllowedHosts, AllowedTools: own.AllowedTools, Replacing: true,
		})
		if err != nil {
			return toolhub.Output{}, err
		}
		if value == "" {
			return toolhub.Text("The user cancelled " + emDash + " secret " + name + " keeps its value.").NotRetryable(), nil
		}
		changed, err := secrets.SetValue(ctx, name, value)
		if err != nil {
			return toolhub.Output{}, err
		}
		return toolhub.Text("Secret " + describeSecret(changed) + " has a new value."), nil
	default: // delete
		if own == nil {
			return toolhub.Output{}, notFound
		}
		if err := secrets.Delete(ctx, name); err != nil {
			return toolhub.Output{}, err
		}
		return toolhub.Text("Deleted secret " + name + "."), nil
	}
}

// askValue is the value the user typed into the masked field, or "" when they cancelled
// (or the run was stopped). A handler that can't take values is an *secret.Error
// ("unavailable").
func askValue(ctx context.Context, tc toolhub.Context, req toolhub.SecretRequest) (string, error) {
	requester, ok := tc.Interaction.(toolhub.SecretRequester)
	if nilcheck.IsNil(tc.Interaction) || !ok || nilcheck.IsNil(requester) {
		return "", secret.NewError("no one can enter a secret value here "+emDash+" ask the user to add it in the web app (Capabilities → Secrets)", secret.CodeUnavailable)
	}
	req.SessionID, req.AgentID, req.ToolCallID = tc.SessionID, tc.AgentID, tc.ToolCallID
	if req.AllowedHosts == nil {
		req.AllowedHosts = []string{}
	}
	if req.AllowedTools == nil {
		req.AllowedTools = []string{}
	}
	resp, err := requester.RequestSecret(ctx, req)
	if ctx.Err() != nil {
		return "", nil //nolint:nilerr // a stopped run is a cancelled entry, not a failure
	}
	if err != nil {
		return "", err
	}
	return resp.Value, nil
}
