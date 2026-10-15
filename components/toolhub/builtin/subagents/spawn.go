package subagents

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"

	"github.com/Phong95/aec-agent-go/components/agent/subagent"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/internal/jsstr"
	"github.com/Phong95/aec-agent-go/internal/nilcheck"
	"github.com/Phong95/aec-agent-go/internal/toolkit"
	"github.com/Phong95/aec-agent-go/kernel"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// SpawnSubagentToolName is the tool name of spawn_subagent, for agent allowlists.
const SpawnSubagentToolName = "spawn_subagent"

// SpawnSubagentTool is spawn_subagent (A-105): it hands a bounded task to one of the
// agent's subagents and waits for the child's final report. It is not a hand-off - the
// child never talks to the user.
//
// Design revision: the target is a subagent type from the library (a specialist that
// runs inside this agent, with its tools and sandbox) or an allowed existing agent; the
// parameter is "subagent" ("agent", its name before subagent types, still works).
//
// Several calls in one model response run in parallel (up to the agent's
// MaxConcurrent); every child of a turn shares that turn's budget (runs, tokens, tool
// calls, time).
//
// Risk "consequential": a child spends tokens and acts with its own tools, so every call
// waits for the user's approval. The child's own approval-gated tools ask the user too -
// the parent's approval is never a grant for the child.
//
// Errors are never retried by the harness: a retry would start another child. Build one
// with NewSpawnSubagentTool. The tool holds no state, so it is safe for concurrent use.
type SpawnSubagentTool struct{}

var _ toolhub.Handler = (*SpawnSubagentTool)(nil)

// NewSpawnSubagentTool returns the spawn_subagent tool. Register it with
// AgentCore.UseTool and add SpawnSubagentToolName to the parent's tools.
func NewSpawnSubagentTool() *SpawnSubagentTool { return &SpawnSubagentTool{} }

// Name returns the plugin name.
func (*SpawnSubagentTool) Name() string { return "tool:spawn-subagent" }

// Version returns the plugin version.
func (*SpawnSubagentTool) Version() string { return "1.0.0" }

// Definition returns the tool definition (a fresh value on every call).
func (*SpawnSubagentTool) Definition() toolhub.Definition {
	str := func(desc string) toolhub.JSONSchema { return toolhub.JSONSchema{Type: "string", Description: desc} }
	return toolhub.Definition{
		Name: SpawnSubagentToolName,
		Description: "Delegate a bounded task to one of your subagents (listed in <available_subagents>) and wait for its final report. " +
			"The subagent starts fresh: it does NOT see this conversation, so the task must stand on its own " + "—" + " include every fact, " +
			"path, id and constraint it needs (put structured facts in context). It cannot ask the user questions. " +
			"Use it for independent work that benefits from a specialist or a separate context, not for something one of your own " +
			"tool calls can do. For independent tasks call it several times in the same response (they run in parallel); when a " +
			"task needs an earlier result, wait for that result first. The report is the subagent's own text: check it before you " +
			"rely on it, and write the answer to the user yourself.",
		Parameters: toolhub.JSONSchema{
			Type: "object",
			Properties: toolhub.NewProperties(
				toolhub.Property{Name: "subagent", Schema: str("Who does it, from <available_subagents>: a subagent type's name, or an agent's id")},
				toolhub.Property{Name: "task", Schema: str("What the subagent should do, self-contained (max " + strconv.Itoa(subagent.MaxTaskChars) + " characters)")},
				toolhub.Property{Name: "context", Schema: toolhub.JSONSchema{
					Type:        "object",
					Description: "Optional facts the subagent needs, as plain JSON data (file paths, ids, constraints, excerpts)",
				}},
				toolhub.Property{Name: "output_shape", Schema: str(`Optional shape of the report you want back, e.g. "a markdown table of clashes with element ids"`)},
			),
			// "subagent" is checked by the brief validation, so the old "agent" name still works.
			Required: []string{"task"},
		},
		Risk: toolhub.RiskConsequential,
	}
}

// Execute spawns the child and returns its result: an error output (not retried) for a
// refusal and for a run that did not complete or partly complete.
func (*SpawnSubagentTool) Execute(ctx context.Context, raw kernel.Arguments, tc toolhub.Context) (toolhub.Output, error) {
	if nilcheck.IsNil(tc.Subagents) {
		return toolkit.Fail("spawn_subagent: subagents are not available here"), nil
	}
	args := toolkit.DecodeArgs(raw)
	var contextArg ordered.Object
	if rawCtx, ok := args.Raw("context"); ok {
		var s string
		if json.Unmarshal(rawCtx, &s) == nil {
			// A model may send the context as a JSON string.
			if jsstr.Trim(s) != "" && !decodeObject([]byte(s), &contextArg) {
				return toolkit.Fail(`spawn_subagent: "context" must be a JSON object`), nil
			}
		} else {
			// Anything but an object is ignored, as in core/ts.
			decodeObject(rawCtx, &contextArg)
		}
	}
	// "agent" was the parameter's name before subagent types (A-105 revision).
	agentRef, _ := args.String("subagent")
	if jsstr.Trim(agentRef) == "" {
		agentRef, _ = args.String("agent")
	}
	task, _ := args.String("task")
	shape, _ := args.String("output_shape")
	if jsstr.Trim(shape) == "" {
		shape = ""
	}
	run, err := tc.Subagents.Spawn(ctx,
		subagent.Request{AgentID: agentRef, Task: task, Context: contextArg, OutputShape: shape},
		subagent.SpawnContext{SessionID: tc.SessionID, RunID: tc.RunID, ToolCallID: tc.ToolCallID},
	)
	if err != nil {
		var se *subagent.Error
		if errors.As(err, &se) {
			return toolkit.Fail("spawn_subagent refused: " + se.Message), nil
		}
		return toolkit.Fail("spawn_subagent failed: " + err.Error()), nil
	}
	meta := ordered.NewObject()
	meta.Set("subagentRun", run.Clone())
	content := subagent.DescribeResult(run)
	if run.Status == subagent.StatusCompleted || run.Status == subagent.StatusPartial {
		out := toolkit.Result(content, meta)
		retryable := false
		out.Retryable = &retryable
		return out, nil
	}
	return toolkit.Failure(content, false, meta), nil
}

// decodeObject decodes data into o when it is one JSON object and reports whether it
// was; anything else leaves o unchanged.
func decodeObject(data []byte, o *ordered.Object) bool {
	var probe any
	if json.Unmarshal(data, &probe) != nil {
		return false
	}
	if _, ok := probe.(map[string]any); !ok {
		return false
	}
	var out ordered.Object
	if out.UnmarshalJSON(data) != nil {
		return false
	}
	*o = out
	return true
}
