package artifacts

import (
	"context"
	"math"
	"strconv"

	"github.com/Phong95/aec-agent-go/components/artifacthub/artifact"
	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/internal/toolkit"
	"github.com/Phong95/aec-agent-go/kernel"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// ReadTool is read_artifact: it reads an artifact's tracked
// file, live from the agent's sandbox. The content is bounded for the model's context by
// max_bytes and by Options.MaxReadBytes; the file itself is never truncated, and the
// user's Artifacts panel always shows it whole. Build one with NewReadTool. The
// tool holds no state, so it is safe for concurrent use.
type ReadTool struct {
	opts resolved
}

var _ toolhub.Handler = (*ReadTool)(nil)

// NewReadTool returns the read_artifact tool. A zero field of the options means
// the default.
func NewReadTool(o Options) *ReadTool { return &ReadTool{opts: resolve(o)} }

// Name returns the plugin name.
func (*ReadTool) Name() string { return "tool:read-artifact" }

// Version returns the plugin version.
func (*ReadTool) Version() string { return "1.0.0" }

// Definition returns the tool definition (a fresh value on every call): a read-only tool
// that works on the sandbox operation fs.read. Its required list is empty, not absent:
// the artifact is named by name or by id, and the tool checks that itself.
func (*ReadTool) Definition() toolhub.Definition {
	return toolhub.Definition{
		Name: "read_artifact",
		Description: "Read an artifact's tracked file, live from your workspace. Use this instead of keeping a long deliverable " +
			"in the conversation: refer to the artifact by name and read it when you need it.",
		Parameters: toolhub.JSONSchema{
			Type: "object",
			Properties: toolhub.NewProperties(
				toolhub.Property{Name: "name", Schema: toolhub.JSONSchema{Type: "string", Description: "Name of the artifact (or use id)"}},
				toolhub.Property{Name: "id", Schema: toolhub.JSONSchema{Type: "string", Description: "Id of the artifact (or use name)"}},
				toolhub.Property{Name: "max_bytes", Schema: toolhub.JSONSchema{Type: "number", Description: "Most bytes of content to return"}},
			),
			Required: []string{},
		},
		Risk:             toolhub.RiskReadOnly,
		SandboxOperation: sandboxhub.OperationFSRead,
	}
}

// Execute reads the artifact. The most bytes of content is max_bytes when it is a number
// above zero, else Options.MaxReadBytes, never above Options.MaxReadBytes, floored and at
// least 1. A file that changed since it was tracked is not an error: a note says so and
// suggests re-tracking it. A tracked file that is gone is a non-retryable error.
func (t *ReadTool) Execute(ctx context.Context, args kernel.Arguments, tc toolhub.Context) (toolhub.Output, error) {
	a := toolkit.DecodeArgs(args)
	ref := refArg(a)
	if ref.ID == "" && ref.Name == "" {
		return argError("read_artifact", `give "name" or "id" to say which artifact to read`), nil
	}

	return withArtifacts(tc, "read_artifact", func(view artifact.View) (toolhub.Output, error) {
		env, refusal, err := sandboxFor(ctx, tc, "read_artifact")
		if refusal != nil {
			return *refusal, nil
		}
		if err != nil {
			return toolhub.Output{}, err
		}

		maxBytes := boundedLimit(a, "max_bytes", t.opts.maxReadBytes)
		got, err := view.Read(ctx, ref, env, artifact.ReadOptions{MaxBytes: &maxBytes})
		if err != nil {
			return toolhub.Output{}, err
		}
		art, file := got.Artifact, got.File

		if file.Status == artifact.FileMissing {
			meta := ordered.NewObject()
			meta.Set("code", "file_missing")
			meta.Set("artifactId", art.ID)
			meta.Set("name", art.Name)
			meta.Set("sandboxPath", file.Path)
			return toolkit.Failure(`read_artifact: "`+art.Name+`" (tracks `+file.Path+") is missing "+emDash+" "+file.Reason, false, meta), nil
		}

		header := art.Name + " (" + art.Type + ") v" + strconv.Itoa(art.Version) + " " + midDot + " " +
			toolkit.FormatBytes(int64(file.Size)) + " " + midDot + " tracks " + file.Path
		notice := ""
		if file.Status == artifact.FileChanged {
			notice = "Note: this file changed since v" + strconv.Itoa(art.Version) + " was tracked " + emDash +
				" call update_artifact with just the name to record it.\n\n"
		}
		truncationNote := ""
		if file.Truncated {
			truncationNote = "\n\n" + ellipsis + " [showing " + toolkit.FormatBytes(int64(len(file.Content))) + " of " +
				toolkit.FormatBytes(int64(file.Size)) + " " + emDash + " raise max_bytes to see more]"
		}

		meta := ordered.NewObject()
		meta.Set("artifactId", art.ID)
		meta.Set("name", art.Name)
		meta.Set("type", art.Type)
		meta.Set("version", art.Version)
		meta.Set("status", string(file.Status))
		meta.Set("bytes", file.Size)
		meta.Set("sha256", file.SHA256)
		meta.Set("truncated", file.Truncated)
		meta.Set("sandboxPath", file.Path)
		return toolkit.Result(notice+header+"\n\n"+file.Content+truncationNote, meta), nil
	}), nil
}

// boundedLimit is the rule of read_artifact (max_bytes) and list_artifacts (limit):
// Math.max(1, Math.floor(Math.min(requested && requested > 0 ? requested : ceiling,
// ceiling))). A requested value that is not a finite number above zero is not used.
func boundedLimit(a toolkit.Args, key string, ceiling int) int {
	limit := float64(ceiling)
	if requested, ok := a.Number(key); ok && requested > 0 {
		limit = requested
	}
	limit = math.Floor(math.Min(limit, float64(ceiling)))
	return int(math.Max(1, limit))
}
