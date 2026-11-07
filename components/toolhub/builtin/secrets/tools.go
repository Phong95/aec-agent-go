package secrets

import "github.com/Phong95/aec-agent-go/components/toolhub"

// ToolNames returns the tool names of the secret tools (A-101, SECRET_TOOL_NAMES of
// core/ts), for agent allowlists, as a fresh slice.
// Update (A-212): one tool, "secret".
func ToolNames() []string { return []string{SecretToolName} }

// RetiredToolNames returns the secret tools before A-212 (list_secrets, manage_secret),
// each now the one "secret" tool, as a fresh slice (RETIRED_SECRET_TOOL_NAMES of core/ts).
// Profiles and subagent types that still name them get "secret" (toolhub.RenamedToolNames;
// a test checks the two agree).
func RetiredToolNames() []string { return []string{"list_secrets", "manage_secret"} }

// NewTools returns both secret tools - register them with AgentCore.UseTool (the
// createSecretTools of core/ts). http_request is a web tool (web.NewHTTPRequestTool).
// Update (A-212): the one secret tool, in a list like the other factories.
func NewTools() []toolhub.Handler {
	return []toolhub.Handler{NewTool()}
}
