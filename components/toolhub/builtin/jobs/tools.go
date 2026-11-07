package jobs

import "github.com/Phong95/aec-agent-go/components/toolhub"

// toolNames are the names of the job tools, for agent allowlists, in the order of
// NewTools (the JOB_TOOL_NAMES of core/ts). They replace the scheduled-job tools
// (schedule_job, list_scheduled_jobs - A-69) and the trigger tools (manage_trigger,
// list_triggers - A-70).
//
// Update (A-211): one tool, "job" (manage_job + list_jobs, with action list).
var toolNames = []string{"job"}

// ToolNames returns the names of the job tools, for agent allowlists, in the order of
// NewTools, as a fresh slice.
func ToolNames() []string {
	return append([]string(nil), toolNames...)
}

// RetiredToolNames returns the job tools before A-211 (manage_job, list_jobs), each now
// the one "job" tool, as a fresh slice (RETIRED_JOB_TOOL_NAMES of core/ts). Profiles and
// subagent types that still name them get "job" (toolhub.RenamedToolNames; a test checks
// the two agree).
func RetiredToolNames() []string { return []string{"manage_job", "list_jobs"} }

// NewTools returns both job tools, manage_job then list_jobs (the createJobTools of
// core/ts). Register them with AgentCore.UseTool.
// Update (A-211): the one job tool, in a list like the other factories.
func NewTools() []toolhub.Handler {
	return []toolhub.Handler{NewTool()}
}
