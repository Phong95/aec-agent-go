package artifacts

import (
	"context"
	"math"
	"strconv"

	"github.com/Phong95/aec-agent-go/components/artifacthub/artifact"
	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/internal/toolkit"
	"github.com/Phong95/aec-agent-go/kernel"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// updateLabel is the label update errors carry (A-209: the artifact tool's "update" action).
const updateLabel = "artifact update"

// updateArtifact is update_artifact: it appends a version to
// an existing artifact: it rewrites the tracked file with new content, tracks a different
// file with from_path, or, with neither, re-tracks the same file's current state after the
// agent edited it with edit_file or write_file. The history is a content-less log;
// expected_version gives the model optimistic concurrency when a person may have changed
// the artifact in between.
// Update (A-209): it is the artifact tool's "update" action (updateArtifact of core/ts).
//
// The artifact is named by id or name (trimmed, one of them
// is needed). The content is taken as it is; the description replaces the stored one when
// it is a string, even an empty one (that clears it); the tags replace the stored ones
// when the list has a non-blank string in it.
//
// expected_version must be a whole number: the view compares it with the head version,
// which is an integer, so a fractional value is refused with an argument error before
// the view is called.
func updateArtifact(ctx context.Context, args kernel.Arguments, a toolkit.Args, tc toolhub.Context) toolhub.Output {
	ref := refArg(a)
	if ref.ID == "" && ref.Name == "" {
		return argError(updateLabel, `give "name" or "id" to say which artifact to update`)
	}

	return withArtifacts(tc, updateLabel, func(view artifact.View) (toolhub.Output, error) {
		env, refusal, err := sandboxFor(ctx, tc, updateLabel)
		if refusal != nil {
			return *refusal, nil
		}
		if err != nil {
			return toolhub.Output{}, err
		}
		src := sourceFrom(a)

		in := artifact.UpdateInput{Ref: ref, Content: src.Content, Path: src.Path, Tool: artifactToolName, Tags: stringList(a, "tags")}
		in.Note, _ = a.String("note")
		if description, ok := a.String("description"); ok {
			in.Description = &description
		}
		if f, ok := a.Number("expected_version"); ok {
			if f != math.Trunc(f) || math.Abs(f) > 1<<53 {
				refused := argError(updateLabel, `"expected_version" must be a whole number`)
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
		// (A-209: read with read_file on the tracked path)
		out.CompactArguments = toolhub.WithContentReference(args, toolhub.ContentWritten{
			Size: toolkit.FormatBytes(int64(got.Bytes)), Path: got.SandboxPath, ReadWith: `read_file "` + got.SandboxPath + `"`,
		})
		return out, nil
	})
}
