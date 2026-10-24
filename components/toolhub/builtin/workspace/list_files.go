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

// ListFilesTool is list_files: a read-only directory listing,
// optionally recursive, bounded to MaxListEntries. Build one with NewListFilesTool. The
// tool holds only its resolved options, so it is safe for concurrent use.
type ListFilesTool struct {
	opts resolved
}

var _ toolhub.Handler = (*ListFilesTool)(nil)

// NewListFilesTool returns the list_files tool for the given options.
func NewListFilesTool(o Options) *ListFilesTool { return &ListFilesTool{opts: resolve(o)} }

// Name returns the plugin name.
func (*ListFilesTool) Name() string { return "tool:list-files" }

// Version returns the plugin version.
func (*ListFilesTool) Version() string { return "1.0.0" }

// Definition returns the tool definition (a fresh value on every call). Every parameter
// is optional, so the schema has no required list. The description of max_entries prints
// the entry limit of the options.
func (t *ListFilesTool) Definition() toolhub.Definition {
	return toolhub.Definition{
		Name: "list_files",
		Description: "List the files and folders in a folder of your workspace (default: your working directory). " +
			`Use recursive to see a whole tree. "/workspace/shared" holds files shared with other agents.`,
		Parameters: toolhub.JSONSchema{
			Type: "object",
			Properties: toolhub.NewProperties(
				toolhub.Property{Name: "path", Schema: toolhub.JSONSchema{Type: "string", Description: "Folder to list (default: your working directory)"}},
				toolhub.Property{Name: "recursive", Schema: toolhub.JSONSchema{Type: "boolean", Description: "Include sub-folders (default false)"}},
				toolhub.Property{Name: "include_hidden", Schema: toolhub.JSONSchema{Type: "boolean", Description: "Include dot-files (default false)"}},
				toolhub.Property{Name: "max_entries", Schema: toolhub.JSONSchema{Type: "number", Description: "Most entries to return (default and max " + strconv.Itoa(t.opts.maxListEntries) + ")"}},
			),
		},
		Risk:             toolhub.RiskReadOnly,
		SandboxOperation: sandboxhub.OperationFSRead,
		// A-153: works in the session's runner folder when the user attached one.
		SessionWorkspace: true,
	}
}

// limitArg is the shared rule of max_entries and max_matches: a positive number is
// floored and capped at the option maximum, anything else means the maximum.
func limitArg(a toolkit.Args, key string, maxAllowed int) int {
	requested, ok := a.Number(key)
	if !ok || requested <= 0 {
		return maxAllowed
	}
	return int(math.Min(math.Floor(requested), float64(maxAllowed)))
}

// Execute lists the folder. The path is trimmed with the JS trim and defaults to the
// working directory. Entries are shown relative to the listed folder: "dir/" for a
// folder, "name  (size)" for a file and "name  [type]" for anything else.
func (t *ListFilesTool) Execute(ctx context.Context, args kernel.Arguments, tc toolhub.Context) (toolhub.Output, error) {
	a := toolkit.DecodeArgs(args)
	rawPath, _ := a.String("path")
	path := jsstr.Trim(rawPath)
	if path == "" {
		path = "."
	}
	recursive, _ := a.Bool("recursive")
	includeHidden, _ := a.Bool("include_hidden")
	maxEntries := limitArg(a, "max_entries", t.opts.maxListEntries)

	return withSandbox(ctx, tc, "list_files", func(env sandboxhub.View) (toolhub.Output, error) {
		result, err := env.List(ctx, path, sandboxhub.ListOptions{Recursive: &recursive, IncludeHidden: &includeHidden, MaxEntries: &maxEntries})
		if err != nil {
			return toolhub.Output{}, err
		}
		base, err := sandboxhub.ResolveLogicalPath(path, env.WorkingDir(), env.Roots(), env.ID())
		if err != nil {
			return toolhub.Output{}, err
		}
		lines := make([]string, len(result.Entries))
		for i, entry := range result.Entries {
			shown := entry.Path
			if rest, ok := strings.CutPrefix(entry.Path, base+"/"); ok {
				shown = rest
			}
			switch entry.Type {
			case sandboxhub.EntryDirectory:
				lines[i] = shown + "/"
			case sandboxhub.EntryFile:
				lines[i] = shown + "  (" + toolkit.FormatBytes(int64(entry.Size)) + ")"
			default:
				lines[i] = shown + "  [" + string(entry.Type) + "]"
			}
		}
		count := len(result.Entries)
		noun := "ies"
		if count == 1 {
			noun = "y"
		}
		recursiveNote := ""
		if recursive {
			recursiveNote = " (recursive)"
		}
		header := base + recursiveNote + " " + emDash + " " + strconv.Itoa(count) + " entr" + noun
		footer := ""
		if result.Truncated {
			footer = "\n[truncated at " + strconv.Itoa(maxEntries) + " entries " + emDash + " list a sub-folder or turn off recursive]"
		}
		body := strings.Join(lines, "\n")
		if body == "" {
			body = "(empty folder)"
		}
		meta := ordered.NewObject()
		meta.Set("path", base)
		meta.Set("count", count)
		meta.Set("truncated", result.Truncated)
		return toolkit.Result(header+"\n"+body+footer, meta), nil
	}), nil
}
