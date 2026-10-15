package subagenthub

import (
	"github.com/Phong95/aec-agent-go/components/agent/subagent"
	"github.com/Phong95/aec-agent-go/components/subagenthub/subagenttype"
)

// A-105: the subagent types every server has. An agent uses one only when it is enabled
// in its profile (AgentProfile.Subagents.Types) and it has spawn_subagent.

// BuiltInTypes returns the built-in subagent types (general-purpose, researcher), the
// BUILT_IN_SUBAGENT_TYPES of core/ts, as fresh values. AgentCore registers them as
// SourceBuiltIn.
func BuiltInTypes() []subagenttype.Definition {
	return []subagenttype.Definition{
		{
			Name: "general-purpose",
			Description: "A general assistant for one self-contained task: look something up, go through files, compare options, check facts. " +
				"Use it when no more specific subagent fits, or to keep a long search out of your own context.",
			SystemPrompt: "You are a focused assistant doing one self-contained task for another agent. Use your tools to get the facts; " +
				"don't guess. Be thorough but stop when the task is done. Your final reply is your report: lead with the answer, " +
				"then the evidence (file paths, sources, values), then anything you could not confirm.",
			Access: subagent.AccessReadOnly,
		},
		{
			Name: "researcher",
			Description: "Researches a question on the web and in the workspace and reports findings with sources. " +
				"Use it for standards, product data, regulations or background reading.",
			SystemPrompt: "You are a researcher working for another agent. Search, open the most relevant sources and read them before " +
				"concluding. Prefer primary sources (standards bodies, manufacturers, official documents). Your final reply is your " +
				"report: the findings as short bullets, each with its source URL or file path, then open questions or conflicts " +
				"between sources.",
			Tools:  []string{"web_search", "web_extract", "read_file", "list_files", "search_files", "read_artifact", "list_artifacts"},
			Access: subagent.AccessReadOnly,
		},
	}
}
