package e2e_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"

	aec "github.com/Phong95/aec-agent-go"
	"github.com/Phong95/aec-agent-go/aecmock"
	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/components/toolhub/builtin/web"
	"github.com/Phong95/aec-agent-go/components/toolhub/builtin/workspace"
	"github.com/Phong95/aec-agent-go/internal/wiretest"
)

// recordingRoundTripper answers every request with one canned HTML page and records the
// requests it saw (method, URL and body), so a test can assert what the tool sent without
// any network.
type recordingRoundTripper struct {
	mu       sync.Mutex
	status   int
	body     string
	requests []recordedRequest
}

type recordedRequest struct {
	Method string
	URL    string
	Body   string
}

func (rt *recordingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		body, _ = io.ReadAll(req.Body)
		_ = req.Body.Close()
	}
	rt.mu.Lock()
	rt.requests = append(rt.requests, recordedRequest{Method: req.Method, URL: req.URL.String(), Body: string(body)})
	rt.mu.Unlock()
	h := http.Header{}
	h.Set("Content-Type", "text/html; charset=utf-8")
	return &http.Response{
		StatusCode: rt.status,
		Status:     http.StatusText(rt.status),
		Header:     h,
		Body:       io.NopCloser(strings.NewReader(rt.body)),
		Request:    req,
	}, nil
}

func (rt *recordingRoundTripper) seen() []recordedRequest {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return append([]recordedRequest(nil), rt.requests...)
}

// recordedDDGPage returns the organic-results HTML of the recorded DuckDuckGo search case
// "region from the query" of builtin-ddg.json (one ad, two organic results and one
// javascript: link the parser skips), as the provider was fed it.
func recordedDDGPage(t *testing.T) string {
	t.Helper()
	var fx struct {
		Search []struct {
			Name     string `json:"name"`
			Response struct {
				Status int    `json:"status"`
				Body   string `json:"body"`
			} `json:"response"`
		} `json:"search"`
	}
	if err := json.Unmarshal(wiretest.Load(t, "builtin-ddg.json"), &fx); err != nil {
		t.Fatalf("decode builtin-ddg.json: %v", err)
	}
	for _, c := range fx.Search {
		if c.Name == "region from the query" {
			return c.Response.Body
		}
	}
	t.Fatal(`builtin-ddg.json has no search case "region from the query"`)
	return ""
}

// toolResults returns the content of every tool result the scripted provider's model has
// received, in order. Every call carries the whole history, so the last call holds them all.
func toolResults(provider *aecmock.ScriptedProvider) []string {
	calls := provider.Calls()
	if len(calls) == 0 {
		return nil
	}
	var out []string
	for _, m := range calls[len(calls)-1].Messages {
		for _, r := range m.ToolResults {
			out = append(out, r.Content)
		}
	}
	return out
}

// TestCreateAgent_WebSearchEndToEnd is the tracer of plan 11-05: one CreateAgent call
// builds an agent whose model searches the web through web_search. The DuckDuckGo provider
// runs over an injected RoundTripper that serves recorded DuckDuckGo HTML (no network), and
// the numbered shortlist of the tool comes back to the model inside the same turn.
func TestCreateAgent_WebSearchEndToEnd(t *testing.T) {
	ctx := t.Context()

	rt := &recordingRoundTripper{status: http.StatusOK, body: recordedDDGPage(t)}
	search := web.NewSearchTool(web.SearchToolOptions{
		Provider: web.NewDuckDuckGoSearchProvider(web.DuckDuckGoSearchProviderOptions{Transport: rt}),
	})

	provider := aecmock.NewScriptedProvider("scripted", []aecmock.ScriptedTurn{
		toolTurn(toolCallOf("tc-1", "web_search", `{"query":"ISO 19650","max_results":2}`)),
		{Content: "Found two sources"},
	})

	a, err := aec.CreateAgent(ctx, aec.AgentConfig{
		Provider: "scripted",
		APIKey:   "k",
		Model:    "m",
		Tools:    []toolhub.Handler{search},
	}, aec.WithProviderRegistry(registryFor(provider)))
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	disposed := false
	dispose := func() {
		t.Helper()
		if disposed {
			return
		}
		disposed = true
		if err := a.Dispose(ctx); err != nil {
			t.Errorf("Dispose: %v", err)
		}
	}
	defer dispose()

	if _, err := a.StartSession(ctx); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	res, err := a.Chat(ctx, "Search the standard")
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if res.Message.Content != "Found two sources" {
		t.Errorf("reply = %q, want %q", res.Message.Content, "Found two sources")
	}

	reqs := rt.seen()
	if len(reqs) != 1 {
		t.Fatalf("DuckDuckGo saw %d requests, want 1: %+v", len(reqs), reqs)
	}
	if reqs[0].Method != http.MethodPost || reqs[0].URL != "https://html.duckduckgo.com/html/" {
		t.Errorf("request = %s %s, want POST https://html.duckduckgo.com/html/", reqs[0].Method, reqs[0].URL)
	}
	if !strings.HasPrefix(reqs[0].Body, "q=ISO+19650") {
		t.Errorf("request body = %q, want it to start with q=ISO+19650", reqs[0].Body)
	}

	// The shortlist reached the model in the same turn.
	results := toolResults(provider)
	if len(results) != 1 {
		t.Fatalf("the model received %d tool results, want 1: %q", len(results), results)
	}
	want := "Search results for \"ISO 19650\" (duckduckgo):\n\n" +
		"1. ISO 19650-1:2018 BIM\n   https://www.iso.org/standard/68078.html\n   Organization and digitization of information about buildings & civil works\n\n" +
		"2. ISO 19650 - Wikipedia\n   https://en.wikipedia.org/wiki/ISO_19650"
	if results[0] != want {
		t.Errorf("tool result seen by the model\n got: %q\nwant: %q", results[0], want)
	}
	dispose()
}

// publicResolver is the name lookup of the web_extract tool in these tests: every host
// resolves to one public test address, so no test asks a real DNS server.
type publicResolver struct{}

func (publicResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
}

// stubSearchProvider is a search backend with a fixed answer.
type stubSearchProvider struct{}

func (stubSearchProvider) Name() string { return "stub" }

func (stubSearchProvider) Search(context.Context, web.SearchQuery) ([]web.SearchResult, error) {
	return []web.SearchResult{{Title: "Spec", URL: "https://docs.example.com/spec", Snippet: "The spec"}}, nil
}

// TestCreateAgent_CompletesTurnWithBuiltInTools is ROADMAP criterion 4 with built-in tools:
// one CreateAgent call, with the workspace tools and the two web tools passed explicitly
// (nothing is auto-registered), completes a chat turn in which the model extracts a
// page and writes the page's title into a file of its sandbox, then answers. The page is
// served by an injected RoundTripper and the host name resolves through an injected
// Resolver, so the turn makes no network request.
func TestCreateAgent_CompletesTurnWithBuiltInTools(t *testing.T) {
	ctx := t.Context()
	wc := newWorkspaceCore(t, workspaceCoreOptions{})
	core := wc.core

	rt := &recordingRoundTripper{
		status: http.StatusOK,
		body:   "<html><head><title>Spec Sheet</title></head><body><h1>Spec</h1><p>Concrete C30/37</p></body></html>",
	}
	extract := web.NewExtractTool(web.ExtractToolOptions{Transport: rt, Resolver: publicResolver{}})
	defer func() { _ = extract.Dispose(ctx) }()

	tools := slices.Concat(
		workspace.NewTools(workspace.Options{}),
		[]toolhub.Handler{web.NewSearchTool(web.SearchToolOptions{Provider: stubSearchProvider{}}), extract},
	)
	var names []string
	for _, h := range tools {
		names = append(names, h.Definition().Name)
	}
	wantNames := []string{
		"read_file", "write_file", "edit_file", "list_files", "search_files", "terminal", "run_code",
		"web_search", "web_extract",
	}
	if !slices.Equal(names, wantNames) {
		t.Fatalf("the passed tools are %v, want %v", names, wantNames)
	}

	model := aecmock.NewScriptedProvider("scripted", []aecmock.ScriptedTurn{
		toolTurn(toolCallOf("tc-1", "web_extract", `{"urls":["https://docs.example.com/spec"]}`)),
		toolTurn(toolCallOf("tc-2", "write_file", `{"path":"title.txt","content":"Spec Sheet\n"}`)),
		{Content: "Saved the title"},
	})

	a, err := aec.CreateAgent(ctx, aec.AgentConfig{
		Provider: "scripted",
		APIKey:   "k",
		Model:    "m",
		Tools:    tools,
	}, aec.WithCore(core), aec.WithProviderRegistry(registryFor(model)))
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	disposed := false
	dispose := func() {
		t.Helper()
		if disposed {
			return
		}
		disposed = true
		if err := a.Dispose(ctx); err != nil {
			t.Errorf("Dispose: %v", err)
		}
	}
	defer dispose()

	// Nothing but the passed tools is registered, and the profile lists them in order.
	var hubNames []string
	for _, d := range core.ToolHub().List() {
		hubNames = append(hubNames, d.Name)
	}
	if !slices.Equal(hubNames, wantNames) {
		t.Errorf("the hub holds %v, want exactly %v", hubNames, wantNames)
	}
	if got := a.Profile().Tools; !slices.Equal(got, wantNames) {
		t.Errorf("profile tools = %v, want %v", got, wantNames)
	}

	if _, err := a.StartSession(ctx); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	res, err := a.Chat(ctx, "Read the spec page and save its title")
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if res.Message.Content != "Saved the title" {
		t.Errorf("reply = %q, want %q", res.Message.Content, "Saved the title")
	}

	// The page was fetched once, through the injected transport.
	reqs := rt.seen()
	if len(reqs) != 1 || reqs[0].Method != http.MethodGet || reqs[0].URL != "https://docs.example.com/spec" {
		t.Errorf("fetched %+v, want one GET of https://docs.example.com/spec", reqs)
	}

	// The extract result reached the model, and the file is in the agent's sandbox.
	results := toolResults(model)
	if len(results) != 2 {
		t.Fatalf("the model received %d tool results, want 2: %q", len(results), results)
	}
	if want := "## https://docs.example.com/spec\nTitle: Spec Sheet\n\n"; !strings.HasPrefix(results[0], want) {
		t.Errorf("web_extract result = %q, want it to start with %q", results[0], want)
	}
	if !strings.HasPrefix(results[1], "Created ") {
		t.Errorf("write_file result = %q, want it to start with Created", results[1])
	}
	view, err := a.Sandbox(ctx)
	if err != nil {
		t.Fatalf("Sandbox: %v", err)
	}
	got, err := view.ReadFile(ctx, "title.txt", sandboxhub.ReadOptions{})
	if err != nil || got.Content != "Spec Sheet\n" {
		t.Fatalf("title.txt = %+v, %v; want content %q", got, err, "Spec Sheet\n")
	}
	dispose()
}
