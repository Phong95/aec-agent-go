package web

import (
	"context"
	"math"
	"strconv"
	"strings"

	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/internal/jsstr"
	"github.com/Phong95/aec-agent-go/internal/nilcheck"
	"github.com/Phong95/aec-agent-go/internal/toolkit"
	"github.com/Phong95/aec-agent-go/kernel"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// The defaults of web_search and the longest query it accepts. The query length counts
// UTF-16 code units, as the JS String length does.
const (
	searchDefaultMaxResults = 5
	searchDefaultLimit      = 10
	searchMaxQueryChars     = 500
)

// SearchTool is web_search: read-only. It returns a shortlist (title,
// url, snippet) for fresh external facts and documentation; the agent then opens only the
// relevant links with web_extract.
//
// A SearchTool holds no per-call state, so it is safe for concurrent use as far as its
// provider is. Build one with NewSearchTool.
type SearchTool struct {
	provider          SearchProvider
	defaultMaxResults int
	maxResultsLimit   int
	definition        toolhub.Definition
}

var _ toolhub.Handler = (*SearchTool)(nil)

// NewSearchTool builds the tool with the defaults for every zero option: the DuckDuckGo
// provider, five results by default and ten at most. The definition is built from the
// resolved limits, so its description names them.
func NewSearchTool(o SearchToolOptions) *SearchTool {
	t := &SearchTool{provider: o.Provider}
	if nilcheck.IsNil(t.provider) {
		t.provider = NewDuckDuckGoSearchProvider(DuckDuckGoSearchProviderOptions{})
	}
	t.maxResultsLimit = searchDefaultLimit
	if o.MaxResultsLimit > 0 {
		t.maxResultsLimit = o.MaxResultsLimit
	}
	t.defaultMaxResults = searchDefaultMaxResults
	if o.DefaultMaxResults > 0 {
		t.defaultMaxResults = o.DefaultMaxResults
	}
	t.defaultMaxResults = min(t.maxResultsLimit, t.defaultMaxResults)
	t.definition = toolhub.Definition{
		Name: "web_search",
		Description: "Search the public web for current information, news, standards or documentation. " +
			"Returns a short list of results (title, url, snippet) " + emDash + " not full pages. " +
			"Use web_extract on the most relevant urls to read them. Cite the urls you rely on.",
		Parameters: toolhub.JSONSchema{
			Type: "object",
			Properties: toolhub.NewProperties(
				toolhub.Property{Name: "query", Schema: toolhub.JSONSchema{Type: "string", Description: "Search query, as you would type it into a search engine"}},
				toolhub.Property{Name: "max_results", Schema: toolhub.JSONSchema{
					Type:        "number",
					Description: "Number of results to return (1-" + strconv.Itoa(t.maxResultsLimit) + ", default " + strconv.Itoa(t.defaultMaxResults) + ")",
				}},
				toolhub.Property{Name: "region", Schema: toolhub.JSONSchema{
					Type:        "string",
					Description: `Optional region code to localise results, e.g. "us-en", "uk-en", "vn-vi"`,
				}},
			),
			Required: []string{"query"},
		},
		Risk: toolhub.RiskReadOnly,
		// A-103: a tool job may run it on a schedule (its exact arguments approved up front).
		Schedulable: true,
		// A-137: web research runs in a subagent, so search results stay out of the parent's context.
		Subagent: toolhub.SubagentPreferred,
	}
	return t
}

// Name returns the plugin name.
func (*SearchTool) Name() string { return "tool:web-search" }

// Version returns the plugin version.
func (*SearchTool) Version() string { return "1.0.0" }

// Definition returns the tool definition (a fresh value on every call).
func (t *SearchTool) Definition() toolhub.Definition { return t.definition.Clone() }

// Execute runs one search. The query is trimmed with the JS trim; an empty one and one of
// more than 500 UTF-16 units are errors the model must fix, so they are not retried.
// max_results is floored and clamped to [1, limit]. A provider failure is an error result
// the harness may retry (a rate limit passes). Execute never returns an error.
func (t *SearchTool) Execute(ctx context.Context, args kernel.Arguments, _ toolhub.Context) (toolhub.Output, error) {
	a := toolkit.DecodeArgs(args)
	raw, _ := a.String("query")
	query := jsstr.Trim(raw)
	if query == "" {
		return toolkit.Fail(`web_search: "query" must be a non-empty string`), nil
	}
	if kernel.UTF16Len(query) > searchMaxQueryChars {
		return toolkit.Fail("web_search: query is too long (max " + strconv.Itoa(searchMaxQueryChars) + " characters)"), nil
	}

	requested := float64(t.defaultMaxResults)
	if n, ok := a.Number("max_results"); ok {
		requested = math.Floor(n)
	}
	maxResults := int(math.Min(float64(t.maxResultsLimit), math.Max(1, requested)))
	region := ""
	if r, ok := a.String("region"); ok {
		region = jsstr.Trim(r)
	}

	results, err := t.provider.Search(ctx, SearchQuery{Query: query, MaxResults: maxResults, Region: region})
	if err != nil {
		// A provider failure is an output the model reads, never a returned error. It is
		// retryable (retryable left unset).
		//nolint:nilerr // a failing provider is a tool-level error output, never a returned error
		return toolhub.Error("web_search failed (" + t.provider.Name() + "): " + err.Error()), nil
	}
	if len(results) > maxResults {
		results = results[:maxResults]
	}

	if len(results) == 0 {
		meta := ordered.NewObject()
		meta.Set("provider", t.provider.Name())
		meta.Set("query", query)
		meta.Set("resultCount", 0)
		return toolkit.Result(`No results for "`+query+`". Try a broader or differently worded query.`, meta), nil
	}

	lines := make([]string, len(results))
	listed := make([]any, len(results))
	for i, r := range results {
		line := strconv.Itoa(i+1) + ". " + r.Title + "\n   " + r.URL
		if r.Snippet != "" {
			line += "\n   " + r.Snippet
		}
		lines[i] = line
		item := ordered.NewObject()
		item.Set("title", r.Title)
		item.Set("url", r.URL)
		item.Set("snippet", r.Snippet)
		listed[i] = item
	}
	meta := ordered.NewObject()
	meta.Set("provider", t.provider.Name())
	meta.Set("query", query)
	meta.Set("resultCount", len(results))
	meta.Set("results", listed)
	return toolkit.Result(`Search results for "`+query+`" (`+t.provider.Name()+"):\n\n"+strings.Join(lines, "\n\n"), meta), nil
}

// emDash is U+2014, built from its code point so that this source stays ASCII.
var emDash = string(rune(0x2014))
