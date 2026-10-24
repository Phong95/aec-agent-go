package artifacts

import (
	"context"

	"github.com/Phong95/aec-agent-go/components/artifacthub/artifact"
	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/internal/toolkit"
	"github.com/Phong95/aec-agent-go/kernel"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// SaveTool is save_artifact: it starts tracking a lasting
// output at version 1, either text written to the agent's workspace first (inline
// content) or an existing workspace file (from_path). A name the agent already used is
// refused: changing an existing artifact is update_artifact's job, so its file is never
// clobbered. Build one with NewSaveTool. The tool holds no state, so it is safe
// for concurrent use.
type SaveTool struct {
	opts resolved
}

var _ toolhub.Handler = (*SaveTool)(nil)

// NewSaveTool returns the save_artifact tool. The options are accepted for
// symmetry with the other tools; save_artifact has nothing to bound, so they change
// nothing.
func NewSaveTool(o Options) *SaveTool { return &SaveTool{opts: resolve(o)} }

// Name returns the plugin name.
func (*SaveTool) Name() string { return "tool:save-artifact" }

// Version returns the plugin version.
func (*SaveTool) Version() string { return "1.0.0" }

// Definition returns the tool definition (a fresh value on every call): a draft-risk
// tool that works on the sandbox operation fs.write, with a required name and type.
func (*SaveTool) Definition() toolhub.Definition {
	return toolhub.Definition{
		Name: "save_artifact",
		Description: "Save a lasting output " + emDash + " a report, a spec section, a takeoff, an estimate " + emDash + " as an artifact the user keeps " +
			"and can hand to another agent. Artifacts track a file in your workspace, versioned by name, so later turns " +
			"refer to them by name instead of re-pasting their content. Pass \"from_path\" to track a file you already " +
			"wrote (write_file first), or \"content\" to have it written to artifacts/<name> in your workspace and " +
			"tracked. Use one or the other, not both.",
		Parameters: toolhub.JSONSchema{
			Type: "object",
			Properties: toolhub.NewProperties(
				toolhub.Property{Name: "name", Schema: toolhub.JSONSchema{Type: "string", Description: `Stable name, unique for this agent (e.g. "clash-report.md" or "reports/q3-takeoff.csv")`}},
				toolhub.Property{Name: "type", Schema: toolhub.JSONSchema{
					Type: "string",
					Description: `MIME type ("text/markdown", "text/csv", "text/html", "application/json") or a domain type ` +
						`("aec/takeoff", "aec/clash-report"). A real MIME type lets the user's Artifacts panel render it ` +
						"(formatted markdown, a table, a rendered page) instead of plain text.",
				}},
				toolhub.Property{Name: "content", Schema: toolhub.JSONSchema{Type: "string", Description: "Text to write to your workspace and track. Leave it out when you use from_path."}},
				toolhub.Property{Name: "from_path", Schema: toolhub.JSONSchema{
					Type: "string",
					Description: "Instead of content: a workspace file to track (its bytes are not copied). " +
						"Leave it out when you pass content " + emDash + " content wins if both are given.",
				}},
				toolhub.Property{Name: "description", Schema: toolhub.JSONSchema{Type: "string", Description: "One line on what this artifact is, for the user's list"}},
				toolhub.Property{Name: "tags", Schema: toolhub.JSONSchema{Type: "array", Items: &toolhub.JSONSchema{Type: "string"}, Description: "Optional labels for filtering"}},
				toolhub.Property{Name: "note", Schema: toolhub.JSONSchema{Type: "string", Description: "Why this version exists " + emDash + " shown in the version history"}},
			),
			Required: []string{"name", "type"},
		},
		Risk:             toolhub.RiskDraft,
		SandboxOperation: sandboxhub.OperationFSWrite,
	}
}

// Execute saves the artifact. The name and the type are trimmed with the JS trim and must
// not be empty. The content is taken as it is, and an empty content with no from_path is
// a valid empty artifact. The description and the note are passed on as given when they
// are not empty; the tags are trimmed and de-duplicated by the view.
func (*SaveTool) Execute(ctx context.Context, args kernel.Arguments, tc toolhub.Context) (toolhub.Output, error) {
	a := toolkit.DecodeArgs(args)
	name := trimmedString(a, "name")
	typ := trimmedString(a, "type")
	if name == "" {
		return argError("save_artifact", `"name" must be a non-empty string`), nil
	}
	if typ == "" {
		return argError("save_artifact", `"type" must be a non-empty string`), nil
	}

	return withArtifacts(tc, "save_artifact", func(view artifact.View) (toolhub.Output, error) {
		env, refusal, err := sandboxFor(ctx, tc, "save_artifact")
		if refusal != nil {
			return *refusal, nil
		}
		if err != nil {
			return toolhub.Output{}, err
		}
		src := sourceFrom(a)

		in := artifact.SaveInput{Name: name, Type: typ, Content: src.Content, Path: src.Path, Tool: "save_artifact", Tags: stringList(a, "tags")}
		in.Description, _ = a.String("description")
		in.Note, _ = a.String("note")
		saved, err := view.Save(ctx, in, env)
		if err != nil {
			return toolhub.Output{}, err
		}
		got := saved.Artifact
		emitWritten(tc, events.ArtifactSaved, got)

		meta := ordered.NewObject()
		meta.Set("artifactId", got.ID)
		meta.Set("name", got.Name)
		meta.Set("type", got.Type)
		meta.Set("version", got.Version)
		meta.Set("bytes", got.Bytes)
		meta.Set("sha256", got.SHA256)
		meta.Set("sandboxPath", got.SandboxPath)
		if src.IgnoredFromPath != "" {
			meta.Set("ignoredFromPath", src.IgnoredFromPath)
		}
		link := artifactLink(got)
		out := toolkit.Result(
			`Saved artifact "`+got.Name+`" (`+got.Type+") as v1 "+emDash+" tracks "+got.SandboxPath+
				" ("+toolkit.FormatBytes(int64(got.Bytes))+"), id "+got.ID+". Refer to it by name from now on; use update_artifact to change it."+
				toolhub.CiteLinkNote(*link)+ignoredFromPathNote(src.IgnoredFromPath), meta)
		out.Link = link
		// A-129: later requests carry a reference to the artifact instead of its whole content
		out.CompactArguments = toolhub.WithContentReference(args, toolhub.ContentWritten{
			Size: toolkit.FormatBytes(int64(got.Bytes)), Path: got.SandboxPath, ReadWith: `read_artifact "` + got.Name + `"`,
		})
		return out, nil
	}), nil
}
