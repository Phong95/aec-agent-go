package web

import (
	"context"
	"encoding/json"
	"slices"
	"strings"

	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/internal/jsonx"
	"github.com/Phong95/aec-agent-go/internal/jsstr"
	"github.com/Phong95/aec-agent-go/internal/toolkit"
	"github.com/Phong95/aec-agent-go/kernel"
)

// webActions are the actions of the web tool (A-212, WEB_ACTIONS of core/ts), in the
// order of its enum.
var webActions = []string{"search", "extract"}

// WebActions returns the actions of the web tool (search, extract) as a fresh slice.
func WebActions() []string { return slices.Clone(webActions) }

// ToolNames returns the names of the web tools, for agent allowlists (A-212: one tool,
// "web"; http_request stays its own tool), as a fresh slice.
func ToolNames() []string { return []string{"web"} }

// RetiredToolNames returns the web tools before A-212 and the action each became in the
// web tool, as a fresh map (RETIRED_WEB_TOOL_NAMES of core/ts). Profiles and subagent types
// that still name them get "web" (toolhub.RenamedToolNames), and a stored tool job that
// calls one runs it as that action (toolhub.RenamedToolCall); a test checks they agree.
func RetiredToolNames() map[string]string {
	return map[string]string{"web_search": "search", "web_extract": "extract"}
}

// ToolOptions configures the web tool: the options of its two actions.
type ToolOptions struct {
	Search  SearchToolOptions
	Extract ExtractToolOptions
}

// WebActionOf is the call's action, trimmed and lower-cased; "" when it is not one of
// WebActions (webActionOf of core/ts).
func WebActionOf(args kernel.Arguments) string {
	raw, _ := toolkit.DecodeArgs(args).String("action")
	action := strings.ToLower(jsstr.Trim(raw))
	if slices.Contains(webActions, action) {
		return action
	}
	return ""
}

// Tool is web (A-212): web_search and web_extract in one read-only tool. The actions are
// the two tools as they were (SearchTool and ExtractTool, still exported and checked
// against the recorded goldens); this tool only picks one, passes the other arguments on,
// and names its errors "web search: ..." / "web extract: ...". Schedulable (tool jobs) and
// preferred for subagents, like both were; the web-research skill says how to research.
type Tool struct {
	search     *SearchTool
	extract    *ExtractTool
	definition toolhub.Definition
}

var _ toolhub.Handler = (*Tool)(nil)

// NewTool builds the web tool over its two actions.
func NewTool(o ToolOptions) *Tool {
	t := &Tool{search: NewSearchTool(o.Search), extract: NewExtractTool(o.Extract)}
	desc := func(h toolhub.Handler, name string) string {
		s, _ := h.Definition().Parameters.Properties.Get(name)
		return s.Description
	}
	t.definition = toolhub.Definition{
		Name: "web",
		Description: "Search the public web and read pages, for current information, news, standards or documentation. " +
			"search: query " + emDash + " a short list of results (title, url, snippet), not full pages. " +
			"extract: urls " + emDash + " the readable text of public http(s) pages (HTML, plain text, JSON, XML; not PDFs or binaries); " +
			"long pages are truncated. Search first, then extract the most relevant urls. Cite the urls you rely on.",
		Parameters: toolhub.JSONSchema{
			Type: "object",
			Properties: toolhub.NewProperties(
				toolhub.Property{Name: "action", Schema: toolhub.Enum("search or extract", webActions...)},
				toolhub.Property{Name: "query", Schema: toolhub.JSONSchema{Type: "string", Description: "search: " + desc(t.search, "query")}},
				toolhub.Property{Name: "max_results", Schema: toolhub.JSONSchema{Type: "number", Description: "search: " + desc(t.search, "max_results")}},
				toolhub.Property{Name: "region", Schema: toolhub.JSONSchema{Type: "string", Description: "search: " + desc(t.search, "region")}},
				toolhub.Property{Name: "urls", Schema: toolhub.JSONSchema{Type: "array", Items: &toolhub.JSONSchema{Type: "string"}, Description: "extract: " + desc(t.extract, "urls")}},
				toolhub.Property{Name: "max_chars", Schema: toolhub.JSONSchema{Type: "number", Description: "extract: " + desc(t.extract, "max_chars")}},
			),
			Required: []string{"action"},
		},
		Risk: toolhub.RiskReadOnly,
		// A-103: a tool job may run it on a schedule (its exact arguments approved up front).
		Schedulable: true,
		// A-137: search results and whole pages stay in a subagent's context; it reports the facts.
		Subagent: toolhub.SubagentPreferred,
	}
	return t
}

// NewTools returns the web tool in a list, like the other tool factories.
func NewTools(o ToolOptions) []toolhub.Handler { return []toolhub.Handler{NewTool(o)} }

// Name returns the plugin name.
func (*Tool) Name() string { return "tool:web" }

// Version returns the plugin version.
func (*Tool) Version() string { return "1.0.0" }

// Definition returns the tool definition (a fresh value on every call).
func (t *Tool) Definition() toolhub.Definition { return t.definition.Clone() }

// Dispose releases the extract action's connections.
func (t *Tool) Dispose(ctx context.Context) error { return t.extract.Dispose(ctx) }

// Execute runs the action with the other arguments.
func (t *Tool) Execute(ctx context.Context, args kernel.Arguments, tc toolhub.Context) (toolhub.Output, error) {
	action := WebActionOf(args)
	if action == "" {
		return toolkit.Fail(`web: "action" must be one of ` + strings.Join(webActions, ", ")), nil
	}
	rest := withoutAction(args)
	var out toolhub.Output
	var err error
	if action == "search" {
		out, err = t.search.Execute(ctx, rest, tc)
	} else {
		out, err = t.extract.Execute(ctx, rest, tc)
	}
	// The actions' own texts name the old tools ("web_search: ..."); the model knows "web".
	old := "web_" + action
	if after, ok := strings.CutPrefix(out.Content, old); ok {
		out.Content = "web " + action + after
	}
	return out, err
}

// withoutAction is the arguments without "action" and without the empty fields a
// strict-schema model fills in (the others are passed on as they are).
func withoutAction(args kernel.Arguments) kernel.Arguments {
	var m map[string]json.RawMessage
	if json.Unmarshal(args, &m) != nil {
		return args
	}
	delete(m, "action")
	// Strict-schema models (OpenAI) fill every field of both actions: drop the empty ones
	// (an empty string, 0 or []), so a search does not ask for 0 results and pages are not
	// cut to 0.
	for key, value := range m {
		switch strings.TrimSpace(string(value)) {
		case `""`, "0", "[]", "null":
			delete(m, key)
		}
	}
	b, err := jsonx.Marshal(m)
	if err != nil {
		return args
	}
	return kernel.Arguments(b)
}
