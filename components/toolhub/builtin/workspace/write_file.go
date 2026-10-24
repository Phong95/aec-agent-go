package workspace

import (
	"context"
	"strconv"
	"strings"

	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/internal/jsstr"
	"github.com/Phong95/aec-agent-go/internal/toolkit"
	"github.com/Phong95/aec-agent-go/kernel"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// WriteFileTool is write_file: it creates a file, or replaces a
// file's whole content, in the agent's workspace, then verifies the write by reading the
// file back (the size and the sha256 must match what was written). Build one with
// NewWriteFileTool. The tool holds no state, so it is safe for concurrent use.
type WriteFileTool struct {
	opts resolved
}

var _ toolhub.Handler = (*WriteFileTool)(nil)

// NewWriteFileTool returns the write_file tool. The options are accepted for symmetry
// with the other tools; write_file has nothing to bound, so they change nothing.
func NewWriteFileTool(o Options) *WriteFileTool { return &WriteFileTool{opts: resolve(o)} }

// Name returns the plugin name.
func (*WriteFileTool) Name() string { return "tool:write-file" }

// Version returns the plugin version.
func (*WriteFileTool) Version() string { return "1.0.0" }

// Definition returns the tool definition (a fresh value on every call): a draft-risk
// tool that works on the sandbox operation fs.write, with a required path and content.
func (*WriteFileTool) Definition() toolhub.Definition {
	return toolhub.Definition{
		Name: "write_file",
		Description: "Create a file, or replace a file's entire content, in your workspace. Parent folders are created. " +
			"To change part of an existing file use edit_file instead. The write is verified by reading the file back.",
		Parameters: toolhub.JSONSchema{
			Type: "object",
			Properties: toolhub.NewProperties(
				toolhub.Property{Name: "path", Schema: toolhub.JSONSchema{Type: "string", Description: `File path, relative to your working directory (e.g. "reports/summary.md") or under /workspace/shared`}},
				toolhub.Property{Name: "content", Schema: toolhub.JSONSchema{Type: "string", Description: "The complete new content of the file"}},
			),
			Required: []string{"path", "content"},
		},
		Risk:             toolhub.RiskDraft,
		SandboxOperation: sandboxhub.OperationFSWrite,
		// A-153: works in the session's runner folder when the user attached one.
		SessionWorkspace: true,
	}
}

// Execute writes the file and verifies it by reading it back. The path is trimmed with
// the JS trim; the content is taken as it is, and an empty content is a valid (empty)
// file. The result text counts the lines the way an editor does: an empty text has 0
// lines and a final newline does not start another one.
func (*WriteFileTool) Execute(ctx context.Context, args kernel.Arguments, tc toolhub.Context) (toolhub.Output, error) {
	a := toolkit.DecodeArgs(args)
	rawPath, _ := a.String("path")
	path := jsstr.Trim(rawPath)
	content, hasContent := a.String("content")
	if path == "" {
		return argError("write_file", `"path" must be a non-empty string`), nil
	}
	if !hasContent {
		return argError("write_file", `"content" must be a string`), nil
	}

	return withSandbox(ctx, tc, "write_file", func(env sandboxhub.View) (toolhub.Output, error) {
		written, err := env.WriteFile(ctx, path, content, sandboxhub.WriteOptions{})
		if err != nil {
			return toolhub.Output{}, err
		}
		failure, err := verifyWrite(ctx, env, written)
		if err != nil {
			return toolhub.Output{}, err
		}
		if failure != nil {
			return *failure, nil
		}
		lines := 0
		if content != "" {
			lines = strings.Count(content, "\n") + 1
			if strings.HasSuffix(content, "\n") {
				lines--
			}
		}
		verb := "Overwrote"
		if written.Created {
			verb = "Created"
		}
		meta := ordered.NewObject()
		meta.Set("path", written.Path)
		meta.Set("bytes", written.Bytes)
		meta.Set("sha256", written.SHA256)
		meta.Set("created", written.Created)
		meta.Set("verified", true)
		out := toolkit.Result(
			verb+" "+written.Path+" ("+toolkit.FormatBytes(int64(written.Bytes))+", "+strconv.Itoa(lines)+" lines) "+emDash+
				" verified by read-back (sha256 "+short(written.SHA256)+")", meta)
		// A-129: later requests carry a reference to the file instead of its whole content
		out.CompactArguments = toolhub.WithContentReference(args, toolhub.ContentWritten{
			Size: toolkit.FormatBytes(int64(written.Bytes)), Path: written.Path, ReadWith: "read_file",
		})
		return out, nil
	}), nil
}

// verifyWrite is the read-after-write check: the file in the sandbox
// must hash to what was written and have the size that was written. It returns an
// error output on a mismatch and nil when the write is verified; the error is the
// failure of the read-back itself.
func verifyWrite(ctx context.Context, env sandboxhub.View, written sandboxhub.WriteResult) (*toolhub.Output, error) {
	one := 1
	check, err := env.ReadFile(ctx, written.Path, sandboxhub.ReadOptions{MaxBytes: &one})
	if err != nil {
		return nil, err
	}
	if check.SHA256 == written.SHA256 && check.Size == written.Bytes {
		return nil, nil
	}
	meta := ordered.NewObject()
	meta.Set("path", written.Path)
	meta.Set("expectedSha256", written.SHA256)
	meta.Set("actualSha256", check.SHA256)
	meta.Set("verified", false)
	out := toolkit.Failure(
		"The write to "+written.Path+" could not be verified: the file now has "+toolkit.FormatBytes(int64(check.Size))+
			" (sha256 "+short(check.SHA256)+") instead of what was written. Something else may be changing it "+emDash+
			" read it and try again.", false, meta)
	return &out, nil
}
