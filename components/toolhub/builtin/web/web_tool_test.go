package web_test

import (
	"context"
	"testing"

	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/components/toolhub/builtin/web"
	"github.com/Phong95/aec-agent-go/kernel"
)

// A-212: the web tool over web_search and web_extract - the "the web tool" cases of core/ts
// tests/unit/merged-tools.test.ts (its definition bytes are in builtin-tool-definitions).

type oneResult struct{ queries []string }

func (*oneResult) Name() string { return "stub" }

func (o *oneResult) Search(_ context.Context, q web.SearchQuery) ([]web.SearchResult, error) {
	o.queries = append(o.queries, q.Query)
	return []web.SearchResult{{Title: "Rebar", URL: "https://example.com/rebar", Snippet: "spacing"}}, nil
}

func TestWebToolDefinitionAndNames(t *testing.T) {
	d := web.NewTool(web.ToolOptions{}).Definition()
	if d.Name != "web" || d.Risk != toolhub.RiskReadOnly || !d.Schedulable || d.Subagent != toolhub.SubagentPreferred {
		t.Fatalf("definition = %+v", d)
	}
	if got := d.Parameters.Properties.Names(); len(got) != 6 || got[0] != "action" || got[5] != "max_chars" {
		t.Fatalf("properties = %v", got)
	}
	renamed := toolhub.RenamedTools()
	for old := range web.RetiredToolNames() {
		if renamed[old] != "web" {
			t.Errorf("%s is renamed to %q, want web", old, renamed[old])
		}
	}
}

func TestWebToolRunsTheActionAndNamesItsErrors(t *testing.T) {
	search := &oneResult{}
	tool := web.NewTool(web.ToolOptions{Search: web.SearchToolOptions{Provider: search}})
	run := func(args string) toolhub.Output {
		t.Helper()
		out, err := tool.Execute(context.Background(), kernel.Arguments(args), toolhub.Context{})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	if out := run(`{"action":"search","query":" rebar spacing "}`); out.IsError != nil && *out.IsError || len(search.queries) != 1 || search.queries[0] != "rebar spacing" {
		t.Fatalf("search = %+v, queries %v", out, search.queries)
	}
	for args, want := range map[string]string{
		`{"action":"search"}`:            `web search: "query" must be a non-empty string`,
		`{"action":"extract","urls":[]}`: `web extract: "urls" must be a non-empty array of url strings`,
		`{"action":"browse"}`:            `web: "action" must be one of search, extract`,
	} {
		if out := run(args); out.Content != want {
			t.Errorf("%s: %q, want %q", args, out.Content, want)
		}
	}
}

// Strict-schema models (OpenAI) fill every field of both actions: the empty ones are dropped.
func TestWebToolWithEveryFieldFilled(t *testing.T) {
	search := &oneResult{}
	tool := web.NewTool(web.ToolOptions{Search: web.SearchToolOptions{Provider: search}})
	out, err := tool.Execute(context.Background(), kernel.Arguments(`{"action":"search","query":"rebar","max_results":0,"region":"","urls":[],"max_chars":0}`), toolhub.Context{})
	if err != nil || (out.IsError != nil && *out.IsError) || len(search.queries) != 1 {
		t.Fatalf("out = %+v, %v", out, err)
	}
	if count, _ := out.Metadata.Get("resultCount"); count != 1 {
		t.Fatalf("resultCount = %v", count)
	}
}
