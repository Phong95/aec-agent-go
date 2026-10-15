package web

import (
	"strings"
	"testing"

	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/kernel"
)

// A-101: http_request's risk depends on the method, and every URL passes the SSRF guard.
func TestHTTPRequestRiskAndGuard(t *testing.T) {
	tool := NewHTTPRequestTool(HTTPRequestToolOptions{})
	t.Cleanup(func() { _ = tool.Dispose(t.Context()) })
	if tool.Definition().Risk != toolhub.RiskConsequential {
		t.Fatal("the definition's risk is the worst case")
	}
	for args, want := range map[string]toolhub.Risk{
		`{"url":"https://a.example.com"}`:                   toolhub.RiskReadOnly,
		`{"method":"head","url":"https://a.example.com"}`:   toolhub.RiskReadOnly,
		`{"method":"POST","url":"https://a.example.com"}`:   toolhub.RiskConsequential,
		`{"method":"DELETE","url":"https://a.example.com"}`: toolhub.RiskConsequential,
	} {
		if got := tool.RiskFor(kernel.Arguments(args)); got != want {
			t.Errorf("RiskFor(%s) = %s, want %s", args, got, want)
		}
	}
	cases := map[string]string{
		`{"url":"http://127.0.0.1/admin"}`:                           "blocked: address 127.0.0.1 is not a public address",
		`{"method":"TRACE","url":"https://a.example.com"}`:           "method must be one of",
		`{"url":"https://a.example.com","body":"x"}`:                 "a GET request has no body",
		`{"url":"https://a.example.com","headers":{"Bad Name":"x"}}`: `"Bad Name" is not a header name`,
		`{"url":"https://a.example.com/{{secret:K}}"}`:               "this agent has no secret store",
	}
	for args, want := range cases {
		out, err := tool.Execute(t.Context(), kernel.Arguments(args), toolhub.Context{})
		if err != nil || out.IsError == nil || !*out.IsError || !strings.Contains(out.Content, want) {
			t.Errorf("Execute(%s) = %+v, %v; want %q", args, out, err, want)
		}
	}
}
