package web

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/kernel"
)

// runSearch runs the tool once with a JSON argument object.
func runSearch(t *testing.T, h *SearchTool, argsJSON string) toolhub.Output {
	t.Helper()
	args, err := kernel.NewArguments([]byte(argsJSON))
	if err != nil {
		t.Fatalf("arguments %s: %v", argsJSON, err)
	}
	out, err := h.Execute(t.Context(), args, toolhub.Context{SessionID: "s1", Emit: func(string, any) {}})
	if err != nil {
		t.Fatalf("Execute returned an error (every failure must be an output): %v", err)
	}
	return out
}

// TestWebSearchTool runs the six WebSearchTool cases. The provider stub records the queries it receives and returns the first MaxResults of 20
// numbered results.
func TestWebSearchTool(t *testing.T) {
	newStub := func(kind string) *stubProvider { return &stubProvider{name: "stub", kind: kind} }

	t.Run("is read-only and defaults to DuckDuckGo", func(t *testing.T) {
		h := NewSearchTool(SearchToolOptions{})
		def := h.Definition()
		if def.Name != "web_search" || def.Risk != toolhub.RiskReadOnly {
			t.Fatalf("definition = %s %s, want web_search read-only", def.Name, def.Risk)
		}
		if got := h.provider.Name(); got != "duckduckgo" {
			t.Errorf("default provider = %q, want duckduckgo", got)
		}
	})

	t.Run("returns a numbered shortlist with urls and snippets", func(t *testing.T) {
		p := newStub("results")
		out := runSearch(t, NewSearchTool(SearchToolOptions{Provider: p}), `{"query":"  rebar spacing  "}`)
		if isErr(out) {
			t.Fatalf("unexpected error: %s", out.Content)
		}
		if len(p.calls) != 1 || p.calls[0] != (searchCall{Query: "rebar spacing", MaxResults: 5}) {
			t.Errorf("provider calls = %+v, want one call with the trimmed query and 5 results", p.calls)
		}
		if want := "1. Result 1\n   https://example.com/1\n   Snippet 1"; !strings.Contains(out.Content, want) {
			t.Errorf("content lacks %q: %q", want, out.Content)
		}
		if n, _ := out.Metadata.Get("resultCount"); n != 5 {
			t.Errorf("resultCount = %v, want 5", n)
		}
	})

	t.Run("clamps max_results to the configured limit", func(t *testing.T) {
		p := newStub("results")
		runSearch(t, NewSearchTool(SearchToolOptions{Provider: p}), `{"query":"q","max_results":50}`)
		runSearch(t, NewSearchTool(SearchToolOptions{Provider: p}), `{"query":"q","max_results":0}`)
		if len(p.calls) != 2 || p.calls[0].MaxResults != 10 || p.calls[1].MaxResults != 1 {
			t.Errorf("provider calls = %+v, want max results [10 1]", p.calls)
		}
	})

	t.Run("rejects an empty query without retry", func(t *testing.T) {
		out := runSearch(t, NewSearchTool(SearchToolOptions{Provider: newStub("results")}), `{"query":"   "}`)
		if !isErr(out) || out.Retryable == nil || *out.Retryable {
			t.Errorf("want an error with retryable false, got %+v", out)
		}
	})

	t.Run("reports provider failures as a (retryable) error", func(t *testing.T) {
		p := &stubProvider{name: "stub", kind: "throws", err: "boom"}
		out := runSearch(t, NewSearchTool(SearchToolOptions{Provider: p}), `{"query":"q"}`)
		if !isErr(out) {
			t.Fatalf("want an error, got %+v", out)
		}
		if out.Retryable != nil {
			t.Errorf("retryable = %v, want unset (a provider failure may be retried)", *out.Retryable)
		}
		if !strings.Contains(out.Content, "boom") {
			t.Errorf("content = %q, want it to carry the provider message", out.Content)
		}
	})

	t.Run("reports no results without an error", func(t *testing.T) {
		out := runSearch(t, NewSearchTool(SearchToolOptions{Provider: newStub("empty")}), `{"query":"zzz"}`)
		if isErr(out) {
			t.Errorf("no results is not an error: %q", out.Content)
		}
		if !strings.Contains(out.Content, "No results") {
			t.Errorf("content = %q", out.Content)
		}
	})
}

// TestSearchToolOptions: a zero field is the default, the default count never exceeds the
// limit, and a nil provider (also a typed nil) is DuckDuckGo.
func TestSearchToolOptions(t *testing.T) {
	d := NewSearchTool(SearchToolOptions{})
	if d.defaultMaxResults != 5 || d.maxResultsLimit != 10 {
		t.Errorf("defaults = %d/%d, want 5/10", d.defaultMaxResults, d.maxResultsLimit)
	}
	if c := NewSearchTool(SearchToolOptions{DefaultMaxResults: 50, MaxResultsLimit: 4}); c.defaultMaxResults != 4 {
		t.Errorf("default above the limit = %d, want 4", c.defaultMaxResults)
	}
	var typedNil *DuckDuckGoSearchProvider
	if got := NewSearchTool(SearchToolOptions{Provider: typedNil}).provider.Name(); got != "duckduckgo" {
		t.Errorf("a typed nil provider must fall back to DuckDuckGo, got %q", got)
	}
	desc, _ := d.Definition().Parameters.Properties.Get("max_results")
	if want := "Number of results to return (1-10, default 5)"; desc.Description != want {
		t.Errorf("max_results description = %q, want %q", desc.Description, want)
	}
}

// ctxKey keys the value TestWebSearch_ProviderGetsTheRunContext puts into the run context.
type ctxKey struct{}

// TestWebSearch_ProviderGetsTheRunContext: stopping the run reaches the provider, because
// the tool hands its own context to Search.
func TestWebSearch_ProviderGetsTheRunContext(t *testing.T) {
	ctx := context.WithValue(t.Context(), ctxKey{}, "run")
	p := &ctxProvider{}
	h := NewSearchTool(SearchToolOptions{Provider: p})
	args, _ := kernel.NewArguments([]byte(`{"query":"q"}`))
	out, err := h.Execute(ctx, args, toolhub.Context{})
	if err != nil {
		t.Fatal(err)
	}
	if p.got != "run" {
		t.Error("the provider did not receive the tool's context")
	}
	if want := "web_search failed (ctx): " + errCtxProvider.Error(); out.Content != want {
		t.Errorf("content = %q, want %q", out.Content, want)
	}
}

var errCtxProvider = errors.New("ctx provider says no")

// ctxProvider records the value of the context it is given and fails.
type ctxProvider struct{ got any }

func (*ctxProvider) Name() string { return "ctx" }

func (p *ctxProvider) Search(ctx context.Context, _ SearchQuery) ([]SearchResult, error) {
	p.got = ctx.Value(ctxKey{})
	return nil, errCtxProvider
}
