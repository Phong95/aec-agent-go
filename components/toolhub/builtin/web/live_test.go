package web

// Live DuckDuckGo test (CONTEXT "Test scope"). DuckDuckGo has no API and rate-limits
// automated clients, so a live search is never part of the default run: it needs an explicit
// opt-in.
//
//	RUN_LIVE_TESTS=1 go test ./builtin/web/ -run TestLiveDuckDuckGo -v
//
// It is the only test of the package that reaches a real site, and only when opted in. CI
// never sets RUN_LIVE_TESTS. A rate-limit answer (HTTP 202 or the bot-check page) skips the
// test instead of failing it: it says nothing about the provider's code.

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestLiveDuckDuckGo(t *testing.T) {
	if os.Getenv("RUN_LIVE_TESTS") != "1" {
		t.Skip("live test skipped: set RUN_LIVE_TESTS=1 to search DuckDuckGo for real")
	}
	ctx := t.Context()
	p := NewDuckDuckGoSearchProvider(DuckDuckGoSearchProviderOptions{TimeoutMs: int((30 * time.Second).Milliseconds())})
	results, err := p.Search(ctx, SearchQuery{Query: "ISO 19650 information management BIM", MaxResults: 5})
	if err != nil {
		if strings.Contains(err.Error(), "rate-limited") {
			t.Skipf("DuckDuckGo rate-limited the request: %v", err)
		}
		t.Fatalf("live search failed: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("the live search returned no result")
	}
	for _, r := range results {
		if !strings.HasPrefix(r.URL, "http://") && !strings.HasPrefix(r.URL, "https://") {
			t.Errorf("result %q has a non-http(s) URL %q", r.Title, r.URL)
		}
		if r.Title == "" {
			t.Errorf("result with URL %q has no title", r.URL)
		}
	}
}
