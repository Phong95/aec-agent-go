package artifacts

import (
	"context"

	"github.com/Phong95/aec-agent-go/components/artifacthub/artifact"
	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/internal/toolkit"
	"github.com/Phong95/aec-agent-go/kernel"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// saveLabel is the label save errors carry (A-209: the artifact tool's "save" action).
const saveLabel = "artifact save"

// artifactToolName is the tool the version origin records (A-209: the artifact tool).
const artifactToolName = "artifact"

// saveArtifact is save_artifact: it starts tracking a lasting
// output at version 1, either text written to the agent's workspace first (inline
// content) or an existing workspace file (from_path). A name the agent already used is
// refused: changing an existing artifact is update_artifact's job, so its file is never
// clobbered.
// Update (A-209): it is the artifact tool's "save" action (saveArtifact of core/ts), and
// changing an existing artifact is the "update" action's job.
//
// The name and the type are trimmed with the JS trim and must
// not be empty. The content is taken as it is, and an empty content with no from_path is
// a valid empty artifact. The description and the note are passed on as given when they
// are not empty; the tags are trimmed and de-duplicated by the view.
func saveArtifact(ctx context.Context, args kernel.Arguments, a toolkit.Args, tc toolhub.Context) toolhub.Output {
	name := trimmedString(a, "name")
	typ := trimmedString(a, "type")
	if name == "" {
		return argError(saveLabel, `"name" must be a non-empty string`)
	}
	if typ == "" {
		return argError(saveLabel, `"type" must be a non-empty string`)
	}

	return withArtifacts(tc, saveLabel, func(view artifact.View) (toolhub.Output, error) {
		env, refusal, err := sandboxFor(ctx, tc, saveLabel)
		if refusal != nil {
			return *refusal, nil
		}
		if err != nil {
			return toolhub.Output{}, err
		}
		src := sourceFrom(a)

		in := artifact.SaveInput{Name: name, Type: typ, Content: src.Content, Path: src.Path, Tool: artifactToolName, Tags: stringList(a, "tags")}
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
				" ("+toolkit.FormatBytes(int64(got.Bytes))+"), id "+got.ID+`. Refer to it by name from now on; use action "update" to change it.`+
				toolhub.CiteLinkNote(*link)+ignoredFromPathNote(src.IgnoredFromPath), meta)
		out.Link = link
		// A-129: later requests carry a reference to the artifact instead of its whole content
		// (A-209: read with read_file on the tracked path)
		out.CompactArguments = toolhub.WithContentReference(args, toolhub.ContentWritten{
			Size: toolkit.FormatBytes(int64(got.Bytes)), Path: got.SandboxPath, ReadWith: `read_file "` + got.SandboxPath + `"`,
		})
		return out, nil
	})
}
