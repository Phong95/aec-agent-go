package web

import (
	"net/http"
	"testing"

	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/conformance"
	"github.com/Phong95/aec-agent-go/kernel"
)

// TestWebToolsConformance runs the tool-handler conformance suite on both web tools. The
// suite makes no network request: web_search runs over a stub provider, and web_extract over
// a route-table transport and a table resolver.
func TestWebToolsConformance(t *testing.T) {
	cases := []struct {
		name    string
		factory func(*testing.T) toolhub.Handler
		args    string
	}{
		{
			"web_search",
			func(*testing.T) toolhub.Handler {
				return NewSearchTool(SearchToolOptions{Provider: &stubProvider{name: "stub", kind: "results"}})
			},
			`{"query":"ISO 19650"}`,
		},
		{
			"web_extract",
			func(*testing.T) toolhub.Handler {
				rt := &routeTransport{routes: map[string]routeSpec{
					"https://example.com/": page(http.StatusOK, "text/html; charset=utf-8", "<html><head><title>T</title></head><body><p>hello</p></body></html>"),
				}}
				return NewExtractTool(ExtractToolOptions{Transport: rt, Resolver: testResolver()})
			},
			`{"urls":["https://example.com/"]}`,
		},
	}
	for _, c := range cases {
		args, err := kernel.NewArguments([]byte(c.args))
		if err != nil {
			t.Fatal(err)
		}
		t.Run(c.name, func(t *testing.T) {
			conformance.ToolHandler(t, c.factory, args)
		})
	}
}
