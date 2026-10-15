package secrets

import "github.com/Phong95/aec-agent-go/components/toolhub"

// ToolNames returns the tool names of the secret tools (A-101, SECRET_TOOL_NAMES of
// core/ts), for agent allowlists, as a fresh slice.
func ToolNames() []string { return []string{ListSecretsToolName, ManageSecretToolName} }

// NewTools returns both secret tools - register them with AgentCore.UseTool (the
// createSecretTools of core/ts). http_request is a web tool (web.NewHTTPRequestTool).
func NewTools() []toolhub.Handler {
	return []toolhub.Handler{NewListSecretsTool(), NewManageSecretTool()}
}
