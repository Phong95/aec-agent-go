package artifacts

import "github.com/Phong95/aec-agent-go/components/toolhub"

// toolNames are the tool names of the artifact tools, for agent allowlists, in the order
// of NewTools.
var toolNames = []string{"save_artifact", "update_artifact", "read_artifact", "list_artifacts"}

// ToolNames returns the names of the artifact tools, for agent allowlists, in the order
// of NewTools. It returns a fresh slice on every call, so a caller can change its copy
// freely.
func ToolNames() []string {
	return append([]string(nil), toolNames...)
}

// NewTools returns all four artifact tools, built with the same Options, in the order
// save_artifact, update_artifact, read_artifact, list_artifacts. Register them with
// AgentCore.UseTool, or pass them in the Tools of an AgentConfig. A zero field of Options
// means the default.
func NewTools(o Options) []toolhub.Handler {
	return []toolhub.Handler{
		NewSaveTool(o),
		NewUpdateTool(o),
		NewReadTool(o),
		NewListTool(o),
	}
}
