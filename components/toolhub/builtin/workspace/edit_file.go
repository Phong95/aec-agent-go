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

// The context of the snippet edit_file shows after an edit: three lines on each side,
// and at most twenty lines in all.
const (
	contextLines    = 3
	maxSnippetLines = 20
)

// EditFileTool is edit_file: an exact-text patch. It replaces old_text
// with new_text and refuses an ambiguous match unless replace_all is set. The write is
// conditional on the file being unchanged since it was read (its sha256), then verified
// by reading it back. Build one with NewEditFileTool. The tool holds no state, so it is
// safe for concurrent use.
type EditFileTool struct {
	opts resolved
}

var _ toolhub.Handler = (*EditFileTool)(nil)

// NewEditFileTool returns the edit_file tool. The options change nothing; they are
// accepted for symmetry with the other tools.
func NewEditFileTool(o Options) *EditFileTool { return &EditFileTool{opts: resolve(o)} }

// Name returns the plugin name.
func (*EditFileTool) Name() string { return "tool:edit-file" }

// Version returns the plugin version.
func (*EditFileTool) Version() string { return "1.0.0" }

// Definition returns the tool definition (a fresh value on every call): a draft-risk
// tool on the sandbox operation fs.write.
func (*EditFileTool) Definition() toolhub.Definition {
	return toolhub.Definition{
		Name: "edit_file",
		Description: "Change part of a file in your workspace by replacing an exact piece of text. old_text must match the file " +
			"exactly (including whitespace, without read_file's line-number prefix) and be unique " + emDash + " include enough " +
			"surrounding lines, or set replace_all to change every occurrence. The edit is verified by reading the file back.",
		Parameters: toolhub.JSONSchema{
			Type: "object",
			Properties: toolhub.NewProperties(
				toolhub.Property{Name: "path", Schema: toolhub.JSONSchema{Type: "string", Description: "File to edit"}},
				toolhub.Property{Name: "old_text", Schema: toolhub.JSONSchema{Type: "string", Description: "Exact text currently in the file"}},
				toolhub.Property{Name: "new_text", Schema: toolhub.JSONSchema{Type: "string", Description: "Text to put in its place (may be empty to delete)"}},
				toolhub.Property{Name: "replace_all", Schema: toolhub.JSONSchema{Type: "boolean", Description: "Replace every occurrence instead of requiring a unique match (default false)"}},
			),
			Required: []string{"path", "old_text", "new_text"},
		},
		Risk:             toolhub.RiskDraft,
		SandboxOperation: sandboxhub.OperationFSWrite,
		// A-153: works in the session's runner folder when the user attached one.
		SessionWorkspace: true,
	}
}

// Execute edits the file. The path is trimmed with the JS trim; old_text must be
// non-empty and different from new_text, which may be empty (a deletion). The matches
// are the non-overlapping occurrences of old_text, found from the left. The write is
// made with the sha256 of the text that was read as ExpectedSHA256, so a file that
// changed in between is not overwritten: the sandbox reports a conflict.
func (*EditFileTool) Execute(ctx context.Context, args kernel.Arguments, tc toolhub.Context) (toolhub.Output, error) {
	a := toolkit.DecodeArgs(args)
	rawPath, _ := a.String("path")
	path := jsstr.Trim(rawPath)
	oldText, _ := a.String("old_text")
	newText, hasNew := a.String("new_text")
	replaceAll, _ := a.Bool("replace_all")
	if path == "" {
		return argError("edit_file", `"path" must be a non-empty string`), nil
	}
	if oldText == "" {
		return argError("edit_file", `"old_text" must be a non-empty string (use write_file to create a file)`), nil
	}
	if !hasNew {
		return argError("edit_file", `"new_text" must be a string`), nil
	}
	if oldText == newText {
		return argError("edit_file", "old_text and new_text are identical "+emDash+" nothing to change"), nil
	}

	return withSandbox(ctx, tc, "edit_file", func(env sandboxhub.View) (toolhub.Output, error) {
		current, err := env.ReadFile(ctx, path, sandboxhub.ReadOptions{})
		if err != nil {
			return toolhub.Output{}, err
		}
		if current.Truncated {
			return argError("edit_file", current.Path+" is too large to edit safely ("+toolkit.FormatBytes(int64(current.Size))+
				", limit "+toolkit.FormatBytes(int64(env.Limits().MaxReadBytes))+")"), nil
		}
		text := current.Content
		positions := occurrences(text, oldText)
		if len(positions) == 0 {
			return argError("edit_file", "old_text was not found in "+current.Path+". Read the file again and copy the text exactly "+
				"(whitespace and indentation matter; leave out read_file's line-number prefix)."), nil
		}
		if len(positions) > 1 && !replaceAll {
			return argError("edit_file", "old_text matches "+strconv.Itoa(len(positions))+" places in "+current.Path+
				" (lines "+lineList(text, positions)+"). "+
				"Include more surrounding text to make it unique, or set replace_all to change all of them."), nil
		}

		var updated string
		if replaceAll {
			updated = strings.ReplaceAll(text, oldText, newText)
		} else {
			updated = strings.Replace(text, oldText, newText, 1)
		}
		// Conditional write: it fails with "conflict" if the file changed after we read it.
		written, err := env.WriteFile(ctx, current.Path, updated, sandboxhub.WriteOptions{ExpectedSHA256: current.SHA256})
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

		n := len(positions)
		plural, linePlural := "s", "s"
		if n == 1 {
			plural, linePlural = "", ""
		}
		meta := ordered.NewObject()
		meta.Set("path", written.Path)
		meta.Set("replacements", n)
		meta.Set("sha256", written.SHA256)
		meta.Set("verified", true)
		return toolkit.Result(
			"Edited "+written.Path+": replaced "+strconv.Itoa(n)+" occurrence"+plural+
				" (line"+linePlural+" "+lineList(text, positions)+") "+emDash+" verified by read-back.\n"+
				snippet(updated, lineAt(text, positions[0]), newText), meta), nil
	}), nil
}

// occurrences returns the byte offsets of the non-overlapping occurrences of needle in
// text, found from the left. The match is on bytes, which finds the
// same occurrences as the JS UTF-16 search because UTF-8 is self-synchronising.
func occurrences(text, needle string) []int {
	var found []int
	for from := 0; from <= len(text); {
		i := strings.Index(text[from:], needle)
		if i < 0 {
			break
		}
		found = append(found, from+i)
		from += i + len(needle)
	}
	return found
}

// lineAt is the 1-based line of a byte offset: one plus the newlines before it.
func lineAt(text string, index int) int {
	return 1 + strings.Count(text[:index], "\n")
}

// lineList is the line numbers of the matches, joined with a comma and a space.
func lineList(text string, positions []int) string {
	parts := make([]string, len(positions))
	for i, p := range positions {
		parts[i] = strconv.Itoa(lineAt(text, p))
	}
	return strings.Join(parts, ", ")
}

// snippet is the edited region with a little context, numbered like read_file: from
// three lines before the first change to three lines after the new text,
// at most twenty lines in all.
func snippet(text string, firstLine int, newText string) string {
	lines := strings.Split(text, "\n")
	start := max(1, firstLine-contextLines)
	end := min(len(lines), firstLine+strings.Count(newText, "\n")+contextLines, start+maxSnippetLines-1)
	if start-1 >= end {
		return ""
	}
	width := len(strconv.Itoa(end))
	out := make([]string, 0, end-start+1)
	for i, line := range lines[start-1 : end] {
		num := strconv.Itoa(start + i)
		out = append(out, strings.Repeat(" ", max(width-len(num), 0))+num+"\t"+line)
	}
	return strings.Join(out, "\n")
}
