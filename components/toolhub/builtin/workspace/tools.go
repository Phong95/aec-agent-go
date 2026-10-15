package workspace

import "github.com/Phong95/aec-agent-go/components/toolhub"

// toolNames are the tool names of the workspace tools, for agent allowlists, in the
// order of NewTools.
var toolNames = []string{"read_file", "write_file", "edit_file", "list_files", "search_files", "terminal", "run_code"}

// ToolNames returns the names of the workspace tools, for agent allowlists, in the order
// of NewTools. It returns a fresh slice on every call, so a caller can change its copy
// freely.
func ToolNames() []string {
	return append([]string(nil), toolNames...)
}

// NewTools returns all seven workspace tools, built with the same Options, in the order
// read_file, write_file, edit_file, list_files, search_files, terminal, run_code.
// Register them with AgentCore.UseTool, or pass them in the Tools of an AgentConfig. A
// zero field of Options means the default.
func NewTools(o Options) []toolhub.Handler {
	return []toolhub.Handler{
		NewReadFileTool(o),
		NewWriteFileTool(o),
		NewEditFileTool(o),
		NewListFilesTool(o),
		NewSearchFilesTool(o),
		NewTerminalTool(o),
		NewRunCodeTool(o),
	}
}
