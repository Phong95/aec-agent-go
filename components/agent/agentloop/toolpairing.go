package agentloop

import "github.com/Phong95/aec-agent-go/kernel"

// carriesToolResults is true when a message carries tool results: on ToolResults,
// which is how the agent loop records them, or as a legacy role tool message.
func carriesToolResults(m kernel.Message) bool {
	return m.Role == kernel.RoleTool || len(m.ToolResults) > 0
}

// repairToolPairs makes the model input well formed with respect to tool calls:
// every tool result must follow the tool call it answers, and every tool call must
// get a result. Providers reject anything else (OpenAI: "No
// tool call found for function call output", Anthropic: "tool_use ids were found
// without tool_result blocks").
//
// The history can break this when compaction summarises a tool call but keeps its
// result, or when a run stops between a call and its result. Orphaned calls and
// results are dropped, and a message left with nothing is dropped too. A call is
// kept only when a result for its id exists somewhere in the input; a result is
// kept only when its call appeared earlier in the filtered input.
//
// It returns msgs itself, the same slice, when nothing needs repairing, so a
// caller can tell by identity. It never changes msgs or the messages in it.
func repairToolPairs(msgs []kernel.Message) []kernel.Message {
	answered := make(map[string]struct{})
	for _, m := range msgs {
		for _, r := range m.ToolResults {
			answered[r.ToolCallID] = struct{}{}
		}
	}

	called := make(map[string]struct{})
	changed := false
	repaired := make([]kernel.Message, 0, len(msgs))

	for _, m := range msgs {
		calls := make([]kernel.ToolCall, 0, len(m.ToolCalls))
		for _, c := range m.ToolCalls {
			if _, ok := answered[c.ID]; ok {
				calls = append(calls, c)
			}
		}
		// Results only count when their call came earlier in the input.
		results := make([]kernel.ToolResult, 0, len(m.ToolResults))
		for _, r := range m.ToolResults {
			if _, ok := called[r.ToolCallID]; ok {
				results = append(results, r)
			}
		}
		for _, c := range calls {
			called[c.ID] = struct{}{}
		}

		if len(m.ToolCalls) == len(calls) && len(m.ToolResults) == len(results) {
			repaired = append(repaired, m)
			continue
		}

		changed = true
		next := m
		next.ToolCalls, next.ToolResults = nil, nil
		if len(calls) > 0 {
			next.ToolCalls = calls
		}
		if len(results) > 0 {
			next.ToolResults = results
		}
		if next.Content != "" || next.ToolCalls != nil || next.ToolResults != nil {
			repaired = append(repaired, next)
		}
	}

	if !changed {
		return msgs
	}
	return repaired
}
