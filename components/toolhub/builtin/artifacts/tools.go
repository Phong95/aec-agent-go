package artifacts

import (
	"context"
	"slices"
	"strings"

	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/internal/jsstr"
	"github.com/Phong95/aec-agent-go/internal/toolkit"
	"github.com/Phong95/aec-agent-go/kernel"
)

// The actions of the artifact tool (A-209, ARTIFACT_ACTIONS of core/ts).
const (
	ActionSave   = "save"
	ActionUpdate = "update"
	ActionList   = "list"
)

// actions are the actions of the artifact tool, in the order of its schema.
var actions = []string{ActionSave, ActionUpdate, ActionList}

// Actions returns the actions of the artifact tool (save, update, list) as a fresh slice.
func Actions() []string { return slices.Clone(actions) }

// toolNames are the tool names of the artifact tools, for agent allowlists, in the order
// of NewTools.
// Update (A-209): one tool, "artifact" (it was save_artifact, update_artifact,
// read_artifact, list_artifacts).
var toolNames = []string{"artifact"}

// ToolNames returns the names of the artifact tools, for agent allowlists, in the order
// of NewTools. It returns a fresh slice on every call, so a caller can change its copy
// freely.
func ToolNames() []string {
	return append([]string(nil), toolNames...)
}

// retiredToolNames (A-209) are the artifact tools before A-209, each now the one
// "artifact" tool (RETIRED_ARTIFACT_TOOL_NAMES of core/ts).
var retiredToolNames = []string{"save_artifact", "update_artifact", "read_artifact", "list_artifacts"}

// RetiredToolNames returns the names of the artifact tools before A-209 (save_artifact,
// update_artifact, read_artifact, list_artifacts), each now the one "artifact" tool.
// Profiles and subagent types that still name them get "artifact" instead (see
// toolhub.RenamedToolNames). Every call returns a fresh slice.
func RetiredToolNames() []string { return slices.Clone(retiredToolNames) }

// NewTools returns all four artifact tools, built with the same Options, in the order
// save_artifact, update_artifact, read_artifact, list_artifacts. Register them with
// AgentCore.UseTool, or pass them in the Tools of an AgentConfig. A zero field of Options
// means the default.
// Update (A-209): it returns the one artifact tool (NewTool), as a list so a host can
// append it like the other tool factories.
func NewTools(o Options) []toolhub.Handler {
	return []toolhub.Handler{NewTool(o)}
}

// Tool is the artifact tool (A-209, ArtifactTool of core/ts): save, update or list the
// agent's lasting outputs in one tool, so a request carries one artifact definition
// instead of four. When and how to use artifacts lives in the essential "artifacts"
// skill; the description is a short reference.
//
// Risk and sandbox operation follow the action (toolhub.CallRisker and
// toolhub.CallSandboxOperator): "list" is read-only and touches no sandbox approval;
// "save" and "update" are drafts that write in the sandbox (fs.write), so on a host
// folder they ask like write_file does. The definition states the worst case. Build one
// with NewTool. The tool holds no state, so it is safe for concurrent use.
type Tool struct {
	opts resolved
}

var (
	_ toolhub.Handler             = (*Tool)(nil)
	_ toolhub.CallRisker          = (*Tool)(nil)
	_ toolhub.CallSandboxOperator = (*Tool)(nil)
)

// NewTool returns the artifact tool. A zero field of the options means the default.
func NewTool(o Options) *Tool { return &Tool{opts: resolve(o)} }

// Name returns the plugin name.
func (*Tool) Name() string { return "tool:artifact" }

// Version returns the plugin version.
func (*Tool) Version() string { return "1.0.0" }

// Definition returns the tool definition (a fresh value on every call): the worst case of
// its actions, a draft-risk tool that works on the sandbox operation fs.write, with a
// required action.
func (*Tool) Definition() toolhub.Definition {
	str := func(description string) toolhub.JSONSchema {
		return toolhub.JSONSchema{Type: "string", Description: description}
	}
	return toolhub.Definition{
		Name: "artifact",
		Description: "Save, update or list your artifacts: lasting outputs (reports, takeoffs, estimates, pages, apps) kept as " +
			"versioned files in your workspace, which the user keeps and later turns refer to by name. save: name, type and " +
			"either content (written to artifacts/<name>) or from_path (a file you wrote). update: name or id, then the new " +
			"full content, a different from_path, or neither to record your own edit of the file. list: what you made, with " +
			"each file's path and whether it changed. Read an artifact with read_file. The artifacts skill says when to save one.",
		Parameters: toolhub.JSONSchema{
			Type: "object",
			Properties: toolhub.NewProperties(
				toolhub.Property{Name: "action", Schema: toolhub.JSONSchema{Type: "string", Enum: slices.Clone(actions), Description: "save, update or list"}},
				toolhub.Property{Name: "name", Schema: str(`save: a stable name, unique for this agent (e.g. "clash-report.md"). update: the artifact to change (or use id)`)},
				toolhub.Property{Name: "id", Schema: str("update: the artifact's id, instead of its name")},
				toolhub.Property{Name: "type", Schema: str(`save: MIME type ("text/markdown", "text/csv", "text/html", "application/json") or a domain type ` +
					`("aec/takeoff"); a real MIME type lets the Artifacts panel render it. list: only artifacts of this type`)},
				toolhub.Property{Name: "content", Schema: str("save/update: the complete text to write and track. Leave it out when you use from_path")},
				toolhub.Property{Name: "from_path", Schema: str("save/update: instead of content, a workspace file to track (not copied). Content wins if both are given")},
				toolhub.Property{Name: "description", Schema: str("save/update: one line on what this artifact is, for the user's list")},
				toolhub.Property{Name: "tags", Schema: toolhub.JSONSchema{Type: "array", Items: &toolhub.JSONSchema{Type: "string"}, Description: "save/update: labels. list: only artifacts carrying any of these"}},
				toolhub.Property{Name: "note", Schema: str("save/update: why this version exists, shown in the version history")},
				toolhub.Property{Name: "expected_version", Schema: toolhub.JSONSchema{Type: "number", Description: "update: only update if this is still the current version number; otherwise the call fails"}},
				toolhub.Property{Name: "this_session_only", Schema: toolhub.JSONSchema{Type: "boolean", Description: "list: only artifacts first created in this conversation"}},
				toolhub.Property{Name: "limit", Schema: toolhub.JSONSchema{Type: "number", Description: "list: most artifacts to return"}},
			),
			Required: []string{"action"},
		},
		Risk:             toolhub.RiskDraft,
		SandboxOperation: sandboxhub.OperationFSWrite,
	}
}

// RiskFor is the risk of one call: "list" is read-only; "save", "update" and an unknown
// action are drafts (the worst case).
func (*Tool) RiskFor(args kernel.Arguments) toolhub.Risk {
	if ActionOf(args) == ActionList {
		return toolhub.RiskReadOnly
	}
	return toolhub.RiskDraft
}

// SandboxOperationFor is the sandbox operation of one call: "list" needs no sandbox
// approval (""); everything else may write in the sandbox (fs.write).
func (*Tool) SandboxOperationFor(args kernel.Arguments) sandboxhub.Operation {
	if ActionOf(args) == ActionList {
		return ""
	}
	return sandboxhub.OperationFSWrite
}

// Execute runs the call's action. A missing or unknown action is a non-retryable
// argument error naming the actions.
func (t *Tool) Execute(ctx context.Context, args kernel.Arguments, tc toolhub.Context) (toolhub.Output, error) {
	a := toolkit.DecodeArgs(args)
	switch actionOf(a) {
	case ActionSave:
		return saveArtifact(ctx, args, a, tc), nil
	case ActionUpdate:
		return updateArtifact(ctx, args, a, tc), nil
	case ActionList:
		return listArtifacts(ctx, a, tc, t.opts), nil
	default:
		return argError("artifact", `"action" must be one of `+strings.Join(actions, ", ")), nil
	}
}

// ActionOf is the call's action, trimmed with the JS trim and lower-cased; "" when it is
// not one of Actions.
func ActionOf(args kernel.Arguments) string {
	return actionOf(toolkit.DecodeArgs(args))
}

// actionOf is ActionOf over decoded arguments.
func actionOf(a toolkit.Args) string {
	s, _ := a.String("action")
	action := strings.ToLower(jsstr.Trim(s))
	if slices.Contains(actions, action) {
		return action
	}
	return ""
}
