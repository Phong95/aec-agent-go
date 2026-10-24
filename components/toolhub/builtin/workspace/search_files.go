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

// maxLineChars is how much of a matching line search_files shows, in UTF-16 code units
// (the JS String length).
const maxLineChars = 300

// SearchFilesTool is search_files: a read-only content search that
// runs where the files are, bounded to MaxSearchMatches. Build one with
// NewSearchFilesTool. The tool holds only its resolved options, so it is safe for
// concurrent use.
type SearchFilesTool struct {
	opts resolved
}

var _ toolhub.Handler = (*SearchFilesTool)(nil)

// NewSearchFilesTool returns the search_files tool for the given options.
func NewSearchFilesTool(o Options) *SearchFilesTool { return &SearchFilesTool{opts: resolve(o)} }

// Name returns the plugin name.
func (*SearchFilesTool) Name() string { return "tool:search-files" }

// Version returns the plugin version.
func (*SearchFilesTool) Version() string { return "1.0.0" }

// Definition returns the tool definition (a fresh value on every call). The description
// of max_matches prints the match limit of the options.
func (t *SearchFilesTool) Definition() toolhub.Definition {
	return toolhub.Definition{
		Name: "search_files",
		Description: "Search the contents of files in your workspace. Returns matching lines as path:line: text. " +
			`Plain text by default; set regex for a regular expression. Narrow with path and glob (e.g. "*.md").`,
		Parameters: toolhub.JSONSchema{
			Type: "object",
			Properties: toolhub.NewProperties(
				toolhub.Property{Name: "pattern", Schema: toolhub.JSONSchema{Type: "string", Description: "Text (or regular expression when regex is true) to look for"}},
				toolhub.Property{Name: "path", Schema: toolhub.JSONSchema{Type: "string", Description: "Folder or file to search (default: your working directory)"}},
				toolhub.Property{Name: "regex", Schema: toolhub.JSONSchema{Type: "boolean", Description: "Treat pattern as a JavaScript regular expression (default false)"}},
				toolhub.Property{Name: "case_sensitive", Schema: toolhub.JSONSchema{Type: "boolean", Description: "Match case exactly (default true)"}},
				toolhub.Property{Name: "glob", Schema: toolhub.JSONSchema{Type: "string", Description: `File name filter, e.g. "*.ts" (optional)`}},
				toolhub.Property{Name: "max_matches", Schema: toolhub.JSONSchema{Type: "number", Description: "Most matches to return (default and max " + strconv.Itoa(t.opts.maxSearchMatches) + ")"}},
			),
			Required: []string{"pattern"},
		},
		Risk:             toolhub.RiskReadOnly,
		SandboxOperation: sandboxhub.OperationFSSearch,
		// A-153: works in the session's runner folder when the user attached one.
		SessionWorkspace: true,
		// A-137: broad searches return many matches; a subagent returns the conclusion.
		Subagent: toolhub.SubagentPreferred,
	}
}

// Execute searches the files. The pattern is used as given (it is not trimmed); path and
// glob are trimmed with the JS trim and left out when empty. The search is a plain text
// match unless regex is true, and case sensitive unless case_sensitive is false. A
// matching line is cut at 300 UTF-16 units with an ellipsis.
func (t *SearchFilesTool) Execute(ctx context.Context, args kernel.Arguments, tc toolhub.Context) (toolhub.Output, error) {
	a := toolkit.DecodeArgs(args)
	pattern, _ := a.String("pattern")
	if pattern == "" {
		return argError("search_files", `"pattern" must be a non-empty string`), nil
	}
	rawPath, _ := a.String("path")
	path := jsstr.Trim(rawPath)
	rawGlob, _ := a.String("glob")
	glob := jsstr.Trim(rawGlob)
	maxMatches := limitArg(a, "max_matches", t.opts.maxSearchMatches)
	regex, _ := a.Bool("regex")
	caseSensitive := true
	if v, ok := a.Bool("case_sensitive"); ok && !v {
		caseSensitive = false
	}
	literal := !regex

	return withSandbox(ctx, tc, "search_files", func(env sandboxhub.View) (toolhub.Output, error) {
		result, err := env.Search(ctx, sandboxhub.SearchQuery{
			Pattern:       pattern,
			Path:          path,
			Literal:       &literal,
			CaseSensitive: &caseSensitive,
			Glob:          glob,
			MaxMatches:    &maxMatches,
		})
		if err != nil {
			return toolhub.Output{}, err
		}
		where := path
		if where == "" {
			where = env.WorkingDir()
		}
		meta := ordered.NewObject()
		meta.Set("count", len(result.Matches))
		meta.Set("truncated", result.Truncated)
		if len(result.Matches) == 0 {
			return toolkit.Result("No matches for \""+pattern+"\" in "+where+".", meta), nil
		}
		lines := make([]string, len(result.Matches))
		for i, m := range result.Matches {
			text := m.Text
			if kernel.UTF16Len(text) > maxLineChars {
				text = jsstr.TruncateUTF16(text, maxLineChars) + ellipsis
			}
			lines[i] = m.Path + ":" + strconv.Itoa(m.Line) + ": " + text
		}
		footer := ""
		if result.Truncated {
			footer = "\n[more matches not shown " + emDash + " narrow the search with path or glob]"
		}
		noun := "es"
		if len(result.Matches) == 1 {
			noun = ""
		}
		return toolkit.Result(strconv.Itoa(len(result.Matches))+" match"+noun+" for \""+pattern+"\" in "+where+":\n"+
			strings.Join(lines, "\n")+footer, meta), nil
	}), nil
}
