package artifacts

import (
	"context"
	"math"
	"strconv"

	"github.com/Phong95/aec-agent-go/components/artifacthub/artifact"
	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/internal/toolkit"
	"github.com/Phong95/aec-agent-go/kernel"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// UpdateTool is update_artifact: it appends a version to
// an existing artifact: it rewrites the tracked file with new content, tracks a different
// file with from_path, or, with neither, re-tracks the same file's current state after the
// agent edited it with edit_file or write_file. The history is a content-less log;
// expected_version gives the model optimistic concurrency when a person may have changed
// the artifact in between. Build one with NewUpdateTool. The tool holds no state,
// so it is safe for concurrent use.
type UpdateTool struct {
	opts resolved
}

var _ toolhub.Handler = (*UpdateTool)(nil)

// NewUpdateTool returns the update_artifact tool. The options are accepted for
// symmetry with the other tools; update_artifact has nothing to bound, so they change
// nothing.
func NewUpdateTool(o Options) *UpdateTool {
	return &UpdateTool{opts: resolve(o)}
}

// Name returns the plugin name.
func (*UpdateTool) Name() string { return "tool:update-artifact" }

// Version returns the plugin version.
func (*UpdateTool) Version() string { return "1.0.0" }

// Definition returns the tool definition (a fresh value on every call): a draft-risk tool
// that works on the sandbox operation fs.write. Its required list is empty, not absent:
// the artifact is named by name or by id, and the tool checks that itself.
func (*UpdateTool) Definition() toolhub.Definition {
	return toolhub.Definition{
		Name: "update_artifact",
		Description: `Record a new version of a tracked artifact. Identify it by "name" or "id", then give either the ` +
			`complete new content (not a patch) in "content" (rewrites the tracked file), or a different workspace ` +
			`file to track with "from_path" ` + emDash + ` one or the other, not both. Give neither to record the tracked file's ` +
			"current state after you edited it yourself with edit_file/write_file.",
		Parameters: toolhub.JSONSchema{
			Type: "object",
			Properties: toolhub.NewProperties(
				toolhub.Property{Name: "name", Schema: toolhub.JSONSchema{Type: "string", Description: "Name of the artifact to update (or use id)"}},
				toolhub.Property{Name: "id", Schema: toolhub.JSONSchema{Type: "string", Description: "Id of the artifact to update (or use name)"}},
				toolhub.Property{Name: "content", Schema: toolhub.JSONSchema{Type: "string", Description: "The complete new content, written to the tracked file. Leave it out to track without rewriting."}},
				toolhub.Property{Name: "from_path", Schema: toolhub.JSONSchema{
					Type: "string",
					Description: "Instead of content: a different workspace file to track from now on. " +
						"Leave it out when you pass content " + emDash + " content wins if both are given.",
				}},
				toolhub.Property{Name: "note", Schema: toolhub.JSONSchema{Type: "string", Description: "What changed in this version " + emDash + " shown in the version history"}},
				toolhub.Property{Name: "description", Schema: toolhub.JSONSchema{Type: "string", Description: "Replace the artifact's description"}},
				toolhub.Property{Name: "tags", Schema: toolhub.JSONSchema{Type: "array", Items: &toolhub.JSONSchema{Type: "string"}, Description: "Replace the artifact's tags"}},
				toolhub.Property{Name: "expected_version", Schema: toolhub.JSONSchema{Type: "number", Description: "Only update if this is still the current version number; otherwise the call fails"}},
			),
			Required: []string{},
		},
		Risk:             toolhub.RiskDraft,
		SandboxOperation: sandboxhub.OperationFSWrite,
	}
}

// Execute appends the version. The artifact is named by id or name (trimmed, one of them
// is needed). The content is taken as it is; the description replaces the stored one when
// it is a string, even an empty one (that clears it); the tags replace the stored ones
// when the list has a non-blank string in it.
//
// expected_version must be a whole number: the view compares it with the head version,
// which is an integer, so a fractional value is refused with an argument error before
// the view is called.
func (*UpdateTool) Execute(ctx context.Context, args kernel.Arguments, tc toolhub.Context) (toolhub.Output, error) {
	a := toolkit.DecodeArgs(args)
	ref := refArg(a)
	if ref.ID == "" && ref.Name == "" {
		return argError("update_artifact", `give "name" or "id" to say which artifact to update`), nil
	}

	return withArtifacts(tc, "update_artifact", func(view artifact.View) (toolhub.Output, error) {
		env, refusal, err := sandboxFor(ctx, tc, "update_artifact")
		if refusal != nil {
			return *refusal, nil
		}
		if err != nil {
			return toolhub.Output{}, err
		}
		src := sourceFrom(a)

		in := artifact.UpdateInput{Ref: ref, Content: src.Content, Path: src.Path, Tool: "update_artifact", Tags: stringList(a, "tags")}
		in.Note, _ = a.String("note")
		if description, ok := a.String("description"); ok {
			in.Description = &description
		}
		if f, ok := a.Number("expected_version"); ok {
			if f != math.Trunc(f) || math.Abs(f) > 1<<53 {
				refused := argError("update_artifact", `"expected_version" must be a whole number`)
				return refused, nil
			}
			version := int(f)
			in.ExpectedVersion = &version
		}
		updated, err := view.Update(ctx, in, env)
		if err != nil {
			return toolhub.Output{}, err
		}
		got := updated.Artifact
		emitWritten(tc, events.ArtifactUpdated, got)

		meta := ordered.NewObject()
		meta.Set("artifactId", got.ID)
		meta.Set("name", got.Name)
		meta.Set("version", got.Version)
		meta.Set("previousVersion", got.Version-1)
		meta.Set("bytes", got.Bytes)
		meta.Set("sha256", got.SHA256)
		meta.Set("sandboxPath", got.SandboxPath)
		if src.IgnoredFromPath != "" {
			meta.Set("ignoredFromPath", src.IgnoredFromPath)
		}
		link := artifactLink(got)
		out := toolkit.Result(
			`Updated "`+got.Name+`" to v`+strconv.Itoa(got.Version)+" "+emDash+" tracks "+got.SandboxPath+
				" ("+toolkit.FormatBytes(int64(got.Bytes))+")."+toolhub.CiteLinkNote(*link)+ignoredFromPathNote(src.IgnoredFromPath), meta)
		out.Link = link
		// A-129: later requests carry a reference to the artifact instead of its whole content
		out.CompactArguments = toolhub.WithContentReference(args, toolhub.ContentWritten{
			Size: toolkit.FormatBytes(int64(got.Bytes)), Path: got.SandboxPath, ReadWith: `read_artifact "` + got.Name + `"`,
		})
		return out, nil
	}), nil
}
