package web

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/internal/wiretest"
	"github.com/Phong95/aec-agent-go/kernel"
)

// ---------------------------------------------------------------------------
// test doubles shared by the tool tests: a route-table RoundTripper (the injected fetch)
// and a table-driven Resolver (the injected lookup)
// ---------------------------------------------------------------------------

// routeSpec is one canned response of a route table.
type routeSpec struct {
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers"`
	// Body is the base64 of the response bytes, nil for no body.
	Body *string `json:"body"`
}

// recordedCall is one request a route table saw: the URL and the two headers the tool sets.
type recordedCall struct {
	URL       string `json:"url"`
	UserAgent string `json:"userAgent"`
	Accept    string `json:"accept"`
}

// routeTransport serves a table of url -> response and records the requests. A URL that is
// not in the table fails with `unexpected fetch <url>`.
type routeTransport struct {
	routes map[string]routeSpec
	mu     sync.Mutex
	calls  []recordedCall
}

func (rt *routeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	key := req.URL.String()
	rt.mu.Lock()
	rt.calls = append(rt.calls, recordedCall{URL: key, UserAgent: req.Header.Get("User-Agent"), Accept: req.Header.Get("Accept")})
	rt.mu.Unlock()
	spec, ok := rt.routes[key]
	if !ok {
		return nil, errors.New("unexpected fetch " + key)
	}
	var body []byte
	if spec.Body != nil {
		var err error
		if body, err = base64.StdEncoding.DecodeString(*spec.Body); err != nil {
			return nil, err
		}
	}
	h := http.Header{}
	for k, v := range spec.Headers {
		h.Set(k, v)
	}
	return &http.Response{
		StatusCode: spec.Status,
		Status:     http.StatusText(spec.Status),
		Header:     h,
		Body:       io.NopCloser(bytes.NewReader(body)),
		Request:    req,
	}, nil
}

// seen returns the recorded requests sorted by URL (the pages of one call are fetched
// concurrently, so the order of the requests is not part of the contract).
func (rt *routeTransport) seen() []recordedCall {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	out := slices.Clone(rt.calls)
	slices.SortStableFunc(out, func(a, b recordedCall) int { return strings.Compare(a.URL, b.URL) })
	return out
}

// tableResolver is the injected lookup: a name -> addresses table, nil for a name that does
// not resolve, and the public address 93.184.216.34 for every name not listed.
type tableResolver struct {
	table map[string][]string
	mu    sync.Mutex
	asked []string
}

func (r *tableResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	r.mu.Lock()
	r.asked = append(r.asked, host)
	r.mu.Unlock()
	list, listed := r.table[host]
	if !listed {
		return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
	}
	if list == nil {
		return nil, errors.New("getaddrinfo ENOTFOUND " + host)
	}
	out := make([]netip.Addr, 0, len(list))
	for _, s := range list {
		out = append(out, netip.MustParseAddr(s))
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// builtin-web-extract
// ---------------------------------------------------------------------------

// extractFixtureOptions are the recorded constructor options (a zero field is absent).
type extractFixtureOptions struct {
	MaxUrls             int    `json:"maxUrls"`
	TimeoutMs           int    `json:"timeoutMs"`
	MaxBytes            int    `json:"maxBytes"`
	DefaultMaxChars     int    `json:"defaultMaxChars"`
	MaxCharsLimit       int    `json:"maxCharsLimit"`
	MaxRedirects        *int   `json:"maxRedirects"`
	UserAgent           string `json:"userAgent"`
	AllowPrivateNetwork bool   `json:"allowPrivateNetwork"`
}

func (o extractFixtureOptions) toolOptions(rt http.RoundTripper, res Resolver) ExtractToolOptions {
	return ExtractToolOptions{
		MaxUrls:             o.MaxUrls,
		TimeoutMs:           o.TimeoutMs,
		MaxBytes:            o.MaxBytes,
		DefaultMaxChars:     o.DefaultMaxChars,
		MaxCharsLimit:       o.MaxCharsLimit,
		MaxRedirects:        o.MaxRedirects,
		UserAgent:           o.UserAgent,
		AllowPrivateNetwork: o.AllowPrivateNetwork,
		Transport:           rt,
		Resolver:            res,
	}
}

type extractFixture struct {
	Tool struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"tool"`
	Lookup      map[string][]string `json:"lookup"`
	Definitions []struct {
		Options    extractFixtureOptions `json:"options"`
		Definition json.RawMessage       `json:"definition"`
	} `json:"definitions"`
	Cases []extractCase `json:"cases"`
}

type extractCase struct {
	Name    string                `json:"name"`
	Options extractFixtureOptions `json:"options"`
	Routes  map[string]routeSpec  `json:"routes"`
	Args    json.RawMessage       `json:"args"`
	ArgsRaw string                `json:"argsRaw"`
	Output  json.RawMessage       `json:"output"`
	Calls   []recordedCall        `json:"calls"`
}

func decodeStrict(t *testing.T, name string, v any) {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(wiretest.Load(t, name)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		t.Fatalf("decode %s: %v", name, err)
	}
}

func encodeDef(t *testing.T, d toolhub.Definition) string {
	t.Helper()
	b, err := kernel.EncodeJSON(d)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestWebExtractMatchesGolden replays builtin-web-extract.json: a reference WebExtractTool
// over a route table and a lookup table. For every recorded case the output bytes and the
// requests the fetch saw must be the recorded ones, and the definition of every recorded
// option set too.
func TestWebExtractMatchesGolden(t *testing.T) {
	var fx extractFixture
	decodeStrict(t, "builtin-web-extract.json", &fx)
	if len(fx.Cases) < 100 {
		t.Fatalf("the corpus holds %d cases, want at least 100", len(fx.Cases))
	}

	probe := NewExtractTool(ExtractToolOptions{})
	if probe.Name() != fx.Tool.Name || probe.Version() != fx.Tool.Version {
		t.Errorf("tool = %s %s, want the TS %s %s", probe.Name(), probe.Version(), fx.Tool.Name, fx.Tool.Version)
	}
	for _, d := range fx.Definitions {
		got := encodeDef(t, NewExtractTool(d.Options.toolOptions(nil, nil)).Definition())
		if got != string(d.Definition) {
			t.Errorf("definition for %+v differs from TS\n got: %s\nwant: %s", d.Options, got, d.Definition)
		}
	}

	for _, c := range fx.Cases {
		t.Run(c.Name, func(t *testing.T) {
			rt := &routeTransport{routes: c.Routes}
			h := NewExtractTool(c.Options.toolOptions(rt, &tableResolver{table: fx.Lookup}))
			raw := c.Args
			if c.ArgsRaw != "" {
				raw = json.RawMessage(c.ArgsRaw)
			}
			args, err := kernel.NewArguments(raw)
			if err != nil {
				t.Fatalf("arguments: %v", err)
			}
			out, err := h.Execute(t.Context(), args, toolhub.Context{SessionID: "s1", Emit: func(string, any) {}})
			if err != nil {
				t.Fatalf("Execute returned an error (every failure must be an output): %v", err)
			}
			got, err := kernel.EncodeJSON(out)
			if err != nil {
				t.Fatal(err)
			}
			// The recorded text names the address a blocked host resolved to; the guard names
			// the host only (review IN-A1, goGuardText).
			if want := goGuardText(string(c.Output)); string(got) != want {
				t.Errorf("output differs from TS\n got: %s\nwant: %s", got, want)
			}
			wantCalls := c.Calls
			if wantCalls == nil {
				wantCalls = []recordedCall{}
			}
			gotCalls := rt.seen()
			if gotCalls == nil {
				gotCalls = []recordedCall{}
			}
			if !reflect.DeepEqual(gotCalls, wantCalls) {
				t.Errorf("requests differ from TS\n got: %+v\nwant: %+v", gotCalls, wantCalls)
			}
		})
	}
}

// TestWebExtractCorpusCoversEveryBranch keeps the recorder honest: it fails when the corpus
// stops exercising a branch of the tool.
func TestWebExtractCorpusCoversEveryBranch(t *testing.T) {
	var fx extractFixture
	decodeStrict(t, "builtin-web-extract.json", &fx)
	classes := map[string]int{}
	for _, c := range fx.Cases {
		out := string(c.Output)
		for class, marker := range map[string]string{
			"redirected":         "(redirected from ",
			"blocked":            "Error: blocked: ",
			"retryable false":    `"retryable":false`,
			"error result":       `"isError":true`,
			"http error":         "Error: HTTP ",
			"unsupported type":   "unsupported content type",
			"too many redirects": "too many redirects",
			"truncated":          "[truncated ",
			"title":              `\nTitle: `,
			"no readable text":   "(no readable text)",
			"too many urls":      "urls per call",
			"no urls":            "must be a non-empty array",
			"invalid url":        "Error: Invalid URL",
			"fetch failure":      "unexpected fetch",
			"separator":          `\n\n---\n\n`,
			"final url in meta":  `"finalUrl":`,
		} {
			if strings.Contains(out, marker) {
				classes[class]++
			}
		}
		if c.Options.AllowPrivateNetwork {
			classes["allowPrivateNetwork"]++
		}
		if c.Options.MaxRedirects != nil {
			classes["maxRedirects option"]++
		}
	}
	for _, class := range []string{
		"redirected", "blocked", "retryable false", "error result", "http error", "unsupported type", "too many redirects",
		"truncated", "title", "no readable text", "too many urls", "no urls", "invalid url", "fetch failure", "separator",
		"final url in meta", "allowPrivateNetwork", "maxRedirects option",
	} {
		if classes[class] == 0 {
			t.Errorf("no recorded case covers %q", class)
		}
	}
}

// ---------------------------------------------------------------------------
// builtin-web-search
// ---------------------------------------------------------------------------

type searchFixture struct {
	Tool struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"tool"`
	Definitions []struct {
		Options searchFixtureOptions `json:"options"`
		Def     json.RawMessage      `json:"definition"`
	} `json:"definitions"`
	Cases []searchCase `json:"cases"`
}

type searchFixtureOptions struct {
	DefaultMaxResults int `json:"defaultMaxResults"`
	MaxResultsLimit   int `json:"maxResultsLimit"`
}

type searchCase struct {
	Name     string               `json:"name"`
	Options  searchFixtureOptions `json:"options"`
	Provider struct {
		Kind    string `json:"kind"`
		Name    string `json:"name"`
		Error   string `json:"error"`
		Results []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Snippet string `json:"snippet"`
		} `json:"results"`
	} `json:"provider"`
	Args    json.RawMessage `json:"args"`
	ArgsRaw string          `json:"argsRaw"`
	Output  json.RawMessage `json:"output"`
	Calls   []searchCall    `json:"calls"`
}

// searchCall is the query a provider received, in the JSON shape of the recording.
type searchCall struct {
	Query      string `json:"query"`
	MaxResults int    `json:"maxResults"`
	Region     string `json:"region,omitempty"`
}

// stubProvider is the stub provider of the recorder.
type stubProvider struct {
	name  string
	kind  string
	err   string
	fixed []SearchResult
	mu    sync.Mutex
	calls []searchCall
}

func (p *stubProvider) Name() string { return p.name }

func (p *stubProvider) Search(_ context.Context, q SearchQuery) ([]SearchResult, error) {
	p.mu.Lock()
	p.calls = append(p.calls, searchCall(q))
	p.mu.Unlock()
	switch p.kind {
	case "results":
		return stubResultList(20)[:min(q.MaxResults, 20)], nil
	case "ignoresMax":
		return stubResultList(20), nil
	case "empty":
		return []SearchResult{}, nil
	case "custom":
		return p.fixed, nil
	case "throws", "throwsNonError":
		return nil, errors.New(p.err)
	}
	return nil, errors.New("unknown provider kind " + p.kind)
}

// stubResultList is the list of 20 stub results of the recorder: Result i, example.com/i,
// Snippet i.
func stubResultList(n int) []SearchResult {
	out := make([]SearchResult, n)
	for i := range out {
		s := strconv.Itoa(i + 1)
		out[i] = SearchResult{Title: "Result " + s, URL: "https://example.com/" + s, Snippet: "Snippet " + s}
	}
	return out
}

func (c searchCase) newProvider() *stubProvider {
	p := &stubProvider{name: c.Provider.Name, kind: c.Provider.Kind, err: c.Provider.Error}
	if p.name == "" {
		p.name = "stub"
	}
	for _, r := range c.Provider.Results {
		p.fixed = append(p.fixed, SearchResult{Title: r.Title, URL: r.URL, Snippet: r.Snippet})
	}
	return p
}

// TestWebSearchMatchesGolden replays builtin-web-search.json: a reference WebSearchTool over
// stub providers. For every recorded case the output bytes and the queries the provider
// received must be the recorded ones, and the definition of every recorded option set too.
func TestWebSearchMatchesGolden(t *testing.T) {
	var fx searchFixture
	decodeStrict(t, "builtin-web-search.json", &fx)
	if len(fx.Cases) < 40 {
		t.Fatalf("the corpus holds %d cases, want at least 40", len(fx.Cases))
	}

	probe := NewSearchTool(SearchToolOptions{Provider: &stubProvider{name: "stub", kind: "empty"}})
	if probe.Name() != fx.Tool.Name || probe.Version() != fx.Tool.Version {
		t.Errorf("tool = %s %s, want the TS %s %s", probe.Name(), probe.Version(), fx.Tool.Name, fx.Tool.Version)
	}
	for _, d := range fx.Definitions {
		o := SearchToolOptions{Provider: &stubProvider{name: "stub", kind: "empty"}, DefaultMaxResults: d.Options.DefaultMaxResults, MaxResultsLimit: d.Options.MaxResultsLimit}
		if got := encodeDef(t, NewSearchTool(o).Definition()); got != string(d.Def) {
			t.Errorf("definition for %+v differs from TS\n got: %s\nwant: %s", d.Options, got, d.Def)
		}
	}

	for _, c := range fx.Cases {
		t.Run(c.Name, func(t *testing.T) {
			provider := c.newProvider()
			h := NewSearchTool(SearchToolOptions{Provider: provider, DefaultMaxResults: c.Options.DefaultMaxResults, MaxResultsLimit: c.Options.MaxResultsLimit})
			raw := c.Args
			if c.ArgsRaw != "" {
				raw = json.RawMessage(c.ArgsRaw)
			}
			args, err := kernel.NewArguments(raw)
			if err != nil {
				t.Fatalf("arguments: %v", err)
			}
			out, err := h.Execute(t.Context(), args, toolhub.Context{SessionID: "s1", Emit: func(string, any) {}})
			if err != nil {
				t.Fatalf("Execute returned an error (every failure must be an output): %v", err)
			}
			got, err := kernel.EncodeJSON(out)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != string(c.Output) {
				t.Errorf("output differs from TS\n got: %s\nwant: %s", got, c.Output)
			}
			wantCalls := c.Calls
			if wantCalls == nil {
				wantCalls = []searchCall{}
			}
			gotCalls := provider.calls
			if gotCalls == nil {
				gotCalls = []searchCall{}
			}
			if !reflect.DeepEqual(gotCalls, wantCalls) {
				t.Errorf("provider queries differ from TS\n got: %+v\nwant: %+v", gotCalls, wantCalls)
			}
		})
	}
}

// TestWebSearchCorpusCoversEveryBranch keeps the recorder honest.
func TestWebSearchCorpusCoversEveryBranch(t *testing.T) {
	var fx searchFixture
	decodeStrict(t, "builtin-web-search.json", &fx)
	classes := map[string]int{}
	for _, c := range fx.Cases {
		out := string(c.Output)
		for class, marker := range map[string]string{
			"results":         "Search results for ",
			"no results":      "No results for ",
			"provider failed": "web_search failed (",
			"query missing":   `\"query\" must be a non-empty string`,
			"query too long":  "query is too long",
			"retryable false": `"retryable":false`,
			"error result":    `"isError":true`,
		} {
			if strings.Contains(out, marker) {
				classes[class]++
			}
		}
		if len(c.Calls) > 0 && c.Calls[0].Region != "" {
			classes["region passed"]++
		}
		if c.Options.MaxResultsLimit != 0 || c.Options.DefaultMaxResults != 0 {
			classes["options"]++
		}
	}
	for _, class := range []string{"results", "no results", "provider failed", "query missing", "query too long", "retryable false", "error result", "region passed", "options"} {
		if classes[class] == 0 {
			t.Errorf("no recorded case covers %q", class)
		}
	}
}
