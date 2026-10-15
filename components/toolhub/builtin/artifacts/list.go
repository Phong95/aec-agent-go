package artifacts

import (
	"context"
	"strconv"
	"strings"

	"github.com/Phong95/aec-agent-go/components/artifacthub/artifact"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/internal/toolkit"
	"github.com/Phong95/aec-agent-go/kernel"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// ListTool is list_artifacts: what this agent has already
// made, most recently updated first. It returns metadata only (no content), so the model
// can decide what to read or update without pulling whole deliverables into context. It
// reads no file, so it needs no sandbox. Build one with NewListTool. The tool
// holds no state, so it is safe for concurrent use.
type ListTool struct {
	opts resolved
}

var _ toolhub.Handler = (*ListTool)(nil)

// NewListTool returns the list_artifacts tool. A zero field of the options means
// the default.
func NewListTool(o Options) *ListTool { return &ListTool{opts: resolve(o)} }

// Name returns the plugin name.
func (*ListTool) Name() string { return "tool:list-artifacts" }

// Version returns the plugin version.
func (*ListTool) Version() string { return "1.0.0" }

// Definition returns the tool definition (a fresh value on every call): a read-only tool
// with no sandbox operation. Its required list is empty, not absent.
func (*ListTool) Definition() toolhub.Definition {
	return toolhub.Definition{
		Name: "list_artifacts",
		Description: "List the artifacts you have made " + emDash + " name, type, version, size and description, newest first. " +
			"Check this before saving, so you update an existing deliverable instead of creating a near-duplicate.",
		Parameters: toolhub.JSONSchema{
			Type: "object",
			Properties: toolhub.NewProperties(
				toolhub.Property{Name: "type", Schema: toolhub.JSONSchema{Type: "string", Description: `Only artifacts of this type (e.g. "text/markdown")`}},
				toolhub.Property{Name: "tags", Schema: toolhub.JSONSchema{Type: "array", Items: &toolhub.JSONSchema{Type: "string"}, Description: "Only artifacts carrying any of these tags"}},
				toolhub.Property{Name: "this_session_only", Schema: toolhub.JSONSchema{Type: "boolean", Description: "Only artifacts first created in this conversation"}},
				toolhub.Property{Name: "limit", Schema: toolhub.JSONSchema{Type: "number", Description: "Most artifacts to return"}},
			),
			Required: []string{},
		},
		Risk: toolhub.RiskReadOnly,
	}
}

// Execute lists the artifacts. The limit is the requested limit when it is a number above
// zero, else Options.MaxListEntries, never above Options.MaxListEntries, floored and at
// least 1. this_session_only filters by the session of the artifact view, and is ignored
// when the view carries no session.
func (t *ListTool) Execute(ctx context.Context, args kernel.Arguments, tc toolhub.Context) (toolhub.Output, error) {
	a := toolkit.DecodeArgs(args)

	return withArtifacts(tc, "list_artifacts", func(view artifact.View) (toolhub.Output, error) {
		limit := boundedLimit(a, "limit", t.opts.maxListEntries)
		q := artifact.Query{Tags: stringList(a, "tags"), Limit: &limit}
		if typ := trimmedString(a, "type"); typ != "" {
			q.Types = []string{typ}
		}
		if only, _ := a.Bool("this_session_only"); only {
			q.SessionID = view.SessionID()
		}

		found, err := view.List(ctx, q)
		if err != nil {
			return toolhub.Output{}, err
		}

		if len(found) == 0 {
			meta := ordered.NewObject()
			meta.Set("count", 0)
			return toolkit.Result("No artifacts yet. Use save_artifact when you produce a deliverable worth keeping.", meta), nil
		}

		lines := make([]string, len(found))
		listed := make([]any, len(found))
		for i, art := range found {
			parts := []string{
				art.Name + " (" + art.Type + ") v" + strconv.Itoa(art.Version),
				toolkit.FormatBytes(int64(art.Bytes)),
				"tracks " + art.SandboxPath,
				"updated " + art.UpdatedAt,
			}
			if len(art.Tags) > 0 {
				parts = append(parts, "tags: "+strings.Join(art.Tags, ", "))
			}
			if art.Description != "" {
				parts = append(parts, art.Description)
			}
			parts = append(parts, "id "+art.ID)
			lines[i] = strings.Join(parts, " "+midDot+" ")

			entry := ordered.NewObject()
			entry.Set("id", art.ID)
			entry.Set("name", art.Name)
			entry.Set("type", art.Type)
			entry.Set("version", art.Version)
			entry.Set("bytes", art.Bytes)
			entry.Set("sandboxPath", art.SandboxPath)
			listed[i] = entry
		}
		plural := "s"
		if len(found) == 1 {
			plural = ""
		}
		meta := ordered.NewObject()
		meta.Set("count", len(found))
		meta.Set("artifacts", listed)
		return toolkit.Result(strconv.Itoa(len(found))+" artifact"+plural+":\n"+strings.Join(lines, "\n"), meta), nil
	}), nil
}
