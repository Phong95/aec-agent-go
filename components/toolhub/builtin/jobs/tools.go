package jobs

import "github.com/Phong95/aec-agent-go/components/toolhub"

// toolNames are the names of the job tools, for agent allowlists, in the order of
// NewTools (the JOB_TOOL_NAMES of core/ts). They replace the scheduled-job tools
// (schedule_job, list_scheduled_jobs - A-69) and the trigger tools (manage_trigger,
// list_triggers - A-70).
var toolNames = []string{"manage_job", "list_jobs"}

// ToolNames returns the names of the job tools, for agent allowlists, in the order of
// NewTools, as a fresh slice.
func ToolNames() []string {
	return append([]string(nil), toolNames...)
}

// NewTools returns both job tools, manage_job then list_jobs (the createJobTools of
// core/ts). Register them with AgentCore.UseTool.
func NewTools() []toolhub.Handler {
	return []toolhub.Handler{NewManageTool(), NewListTool()}
}
