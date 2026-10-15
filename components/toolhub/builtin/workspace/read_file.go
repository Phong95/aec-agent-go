package workspace

import (
	"context"
	"math"
	"strconv"
	"strings"

	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/internal/jsstr"
	"github.com/Phong95/aec-agent-go/internal/toolkit"
	"github.com/Phong95/aec-agent-go/kernel"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// ReadFileTool is read_file: a read-only tool that returns a text file
// from the agent's workspace with line numbers, bounded to MaxReadBytes; a larger file
// is read in line ranges. Build one with NewReadFileTool. The tool holds only its
// resolved options, so it is safe for concurrent use.
type ReadFileTool struct {
	opts resolved
}

var _ toolhub.Handler = (*ReadFileTool)(nil)

// NewReadFileTool returns the read_file tool for the given options.
func NewReadFileTool(o Options) *ReadFileTool { return &ReadFileTool{opts: resolve(o)} }

// Name returns the plugin name.
func (*ReadFileTool) Name() string { return "tool:read-file" }

// Version returns the plugin version.
func (*ReadFileTool) Version() string { return "1.0.0" }

// Definition returns the tool definition (a fresh value on every call). The description
// prints the byte limit of the options.
func (t *ReadFileTool) Definition() toolhub.Definition {
	return toolhub.Definition{
		Name: "read_file",
		Description: "Read a text file from your workspace. Lines are prefixed with their line number and a tab " +
			"(not part of the file " + emDash + " never copy that prefix into edit_file). " +
			"Returns at most " + toolkit.FormatBytes(int64(t.opts.maxReadBytes)) + "; for bigger files read a range with start_line/end_line.",
		Parameters: toolhub.Object(
			toolhub.Prop("path", toolhub.String(`File path, relative to your working directory (e.g. "src/app.ts") or under /workspace/shared`)),
			toolhub.Prop("start_line", toolhub.Number("First line to read (1-based, optional)")),
			toolhub.Prop("end_line", toolhub.Number("Last line to read (inclusive, optional)")),
		).Require("path"),
		Risk:             toolhub.RiskReadOnly,
		SandboxOperation: sandboxhub.OperationFSRead,
	}
}

// maxLineNumber bounds a line number taken from the arguments before it becomes an int,
// so a huge number cannot overflow on a 32-bit platform. No file has that many lines.
const maxLineNumber = math.MaxInt32

// lineArg floors a line number argument into an int in 1 to maxLineNumber.
func lineArg(f float64) int {
	return int(math.Min(math.Floor(f), maxLineNumber))
}

// Execute reads the file. The path is trimmed with the JS trim. start_line and end_line
// are 1-based, end_line must not be before start_line, and a fractional value is
// floored. A file that holds a NUL character is reported as binary and not shown.
// A trailing newline ends the last line and is not shown as an extra empty line.
func (t *ReadFileTool) Execute(ctx context.Context, args kernel.Arguments, tc toolhub.Context) (toolhub.Output, error) {
	a := toolkit.DecodeArgs(args)
	rawPath, _ := a.String("path")
	path := jsstr.Trim(rawPath)
	if path == "" {
		return argError("read_file", `"path" must be a non-empty string`), nil
	}
	startLine, hasStart := a.Number("start_line")
	endLine, hasEnd := a.Number("end_line")
	if (hasStart && startLine < 1) || (hasEnd && endLine < 1) {
		return argError("read_file", "start_line and end_line are 1-based"), nil
	}
	if hasStart && hasEnd && endLine < startLine {
		return argError("read_file", "end_line must not be before start_line"), nil
	}

	return withSandbox(ctx, tc, "read_file", func(env sandboxhub.View) (toolhub.Output, error) {
		maxBytes := t.opts.maxReadBytes
		opts := sandboxhub.ReadOptions{MaxBytes: &maxBytes}
		first := 1
		if hasStart {
			first = lineArg(startLine)
			opts.StartLine = &first
		}
		if hasEnd {
			last := lineArg(endLine)
			opts.EndLine = &last
		}
		file, err := env.ReadFile(ctx, path, opts)
		if err != nil {
			return toolhub.Output{}, err
		}
		meta := ordered.NewObject()
		meta.Set("path", file.Path)
		meta.Set("size", file.Size)
		meta.Set("sha256", file.SHA256)
		meta.Set("truncated", file.Truncated)

		if strings.ContainsRune(file.Content, 0) {
			return toolkit.Result(file.Path+" is a binary file ("+toolkit.FormatBytes(int64(file.Size))+") "+emDash+
				" it cannot be shown as text.", meta), nil
		}

		var lines []string
		if file.Content != "" {
			lines = strings.Split(file.Content, "\n")
		}
		// A trailing newline is the end of the last line, not an extra empty line.
		if len(lines) > 1 && lines[len(lines)-1] == "" {
			lines = lines[:len(lines)-1]
		}
		width := len(strconv.Itoa(first + len(lines)))
		numbered := make([]string, len(lines))
		for i, line := range lines {
			num := strconv.Itoa(first + i)
			numbered[i] = strings.Repeat(" ", max(width-len(num), 0)) + num + "\t" + line
		}
		body := strings.Join(numbered, "\n")
		if body == "" {
			body = "(empty file)"
		}

		rng := ""
		if hasStart || hasEnd {
			rng = ", lines " + strconv.Itoa(first) + "-" + strconv.Itoa(first+max(len(lines)-1, 0))
		}
		header := file.Path + " (" + toolkit.FormatBytes(int64(file.Size)) + rng + ")"
		footer := ""
		if file.Truncated {
			footer = "\n[truncated " + emDash + " the file continues; read more with start_line/end_line]"
		}
		return toolkit.Result(header+"\n"+body+footer, meta), nil
	}), nil
}
