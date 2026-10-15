package web

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/kernel"
)

// page builds one canned response of a route table.
func page(status int, contentType, body string) routeSpec {
	enc := base64.StdEncoding.EncodeToString([]byte(body))
	h := map[string]string{}
	if contentType != "" {
		h["Content-Type"] = contentType
	}
	return routeSpec{Status: status, Headers: h, Body: &enc}
}

func htmlPage(body string) routeSpec { return page(200, "text/html; charset=utf-8", body) }

// redirectTo is a redirect response with no body.
func redirectTo(status int, location string) routeSpec {
	return routeSpec{Status: status, Headers: map[string]string{"Location": location}}
}

// testResolver is the test lookup: internal.example is private, every other name
// is public.
func testResolver() *tableResolver {
	return &tableResolver{table: map[string][]string{"internal.example": {"10.0.0.7"}}}
}

// runExtract runs the tool once with a JSON argument object and returns the output.
func runExtract(t *testing.T, h *ExtractTool, argsJSON string) toolhub.Output {
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

func isErr(o toolhub.Output) bool { return o.IsError != nil && *o.IsError }

// pagesMeta decodes the metadata.pages array of an output.
func pagesMeta(t *testing.T, o toolhub.Output) []map[string]any {
	t.Helper()
	raw, err := json.Marshal(o.Metadata)
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Pages []map[string]any `json:"pages"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m.Pages
}

// TestWebExtractTool runs the nine WebExtractTool cases. The fetch double is a route-table
// RoundTripper and the lookup double a Resolver.
func TestWebExtractTool(t *testing.T) {
	newTool := func(routes map[string]routeSpec, o ExtractToolOptions) (*ExtractTool, *routeTransport) {
		rt := &routeTransport{routes: routes}
		o.Transport = rt
		o.Resolver = testResolver()
		return NewExtractTool(o), rt
	}

	t.Run("is read-only", func(t *testing.T) {
		def := NewExtractTool(ExtractToolOptions{}).Definition()
		if def.Name != "web_extract" || def.Risk != toolhub.RiskReadOnly {
			t.Fatalf("definition = %s %s, want web_extract read-only", def.Name, def.Risk)
		}
	})

	t.Run("returns title and readable text of an HTML page", func(t *testing.T) {
		h, _ := newTool(map[string]routeSpec{
			"https://example.com/": htmlPage("<html><head><title>Spec</title></head><body><script>x()</script><p>Concrete C30/37</p></body></html>"),
		}, ExtractToolOptions{})
		out := runExtract(t, h, `{"urls":["https://example.com/"]}`)
		if isErr(out) {
			t.Fatalf("unexpected error: %s", out.Content)
		}
		for _, want := range []string{"## https://example.com/", "Title: Spec", "Concrete C30/37"} {
			if !strings.Contains(out.Content, want) {
				t.Errorf("content lacks %q: %q", want, out.Content)
			}
		}
		if strings.Contains(out.Content, "x()") {
			t.Errorf("the script body leaked into the text: %q", out.Content)
		}
	})

	t.Run("follows a public redirect but blocks a redirect into the private network", func(t *testing.T) {
		h, rt := newTool(map[string]routeSpec{
			"https://example.com/a":    redirectTo(302, "/b"),
			"https://example.com/b":    htmlPage("<p>landed</p>"),
			"https://example.com/evil": redirectTo(301, "http://169.254.169.254/latest/meta-data/"),
		}, ExtractToolOptions{})
		ok := runExtract(t, h, `{"urls":["https://example.com/a"]}`)
		if !strings.Contains(ok.Content, "landed") || !strings.Contains(ok.Content, "(redirected from https://example.com/a)") {
			t.Errorf("redirect not followed and reported: %q", ok.Content)
		}
		blocked := runExtract(t, h, `{"urls":["https://example.com/evil"]}`)
		if !isErr(blocked) || blocked.Retryable == nil || *blocked.Retryable {
			t.Errorf("a redirect into the private network must be a non-retryable error: %+v", blocked)
		}
		if !strings.Contains(blocked.Content, "blocked") {
			t.Errorf("content lacks blocked: %q", blocked.Content)
		}
		for _, c := range rt.seen() {
			if c.URL == "http://169.254.169.254/latest/meta-data/" {
				t.Errorf("the metadata address was fetched")
			}
		}
	})

	t.Run("never fetches private hosts or non-http schemes", func(t *testing.T) {
		h, rt := newTool(nil, ExtractToolOptions{})
		out := runExtract(t, h, `{"urls":["http://internal.example/admin","file:///etc/passwd","http://127.0.0.1:5432/"]}`)
		if !isErr(out) || out.Retryable == nil || *out.Retryable {
			t.Errorf("want an error with retryable false, got %+v", out)
		}
		if n := len(rt.seen()); n != 0 {
			t.Errorf("the transport saw %d requests, want 0", n)
		}
	})

	t.Run("returns partial success when only some pages fail", func(t *testing.T) {
		h, _ := newTool(map[string]routeSpec{
			"https://example.com/ok":      htmlPage("<p>fine</p>"),
			"https://example.com/missing": page(404, "text/html; charset=utf-8", "nope"),
		}, ExtractToolOptions{})
		out := runExtract(t, h, `{"urls":["https://example.com/ok","https://example.com/missing"]}`)
		if isErr(out) {
			t.Errorf("a partial success is not an error: %q", out.Content)
		}
		if !strings.Contains(out.Content, "fine") || !strings.Contains(out.Content, "Error: HTTP 404") {
			t.Errorf("content = %q", out.Content)
		}
	})

	t.Run("rejects binary content types and passes JSON/plain text through", func(t *testing.T) {
		h, _ := newTool(map[string]routeSpec{
			"https://example.com/doc.pdf":   page(200, "application/pdf", "%PDF"),
			"https://example.com/data.json": page(200, "application/json", `{"a":1}`),
		}, ExtractToolOptions{})
		pdf := runExtract(t, h, `{"urls":["https://example.com/doc.pdf"]}`)
		if !strings.Contains(pdf.Content, `unsupported content type "application/pdf"`) {
			t.Errorf("pdf content = %q", pdf.Content)
		}
		js := runExtract(t, h, `{"urls":["https://example.com/data.json"]}`)
		if !strings.Contains(js.Content, `{"a":1}`) {
			t.Errorf("json content = %q", js.Content)
		}
	})

	t.Run("caps the downloaded bytes and truncates to max_chars", func(t *testing.T) {
		h, _ := newTool(map[string]routeSpec{
			"https://example.com/big": page(200, "text/plain", strings.Repeat("a", 100_000)),
		}, ExtractToolOptions{MaxBytes: 10_000})
		out := runExtract(t, h, `{"urls":["https://example.com/big"],"max_chars":500}`)
		if want := ellipsis + "[truncated 9500 chars]"; !strings.Contains(out.Content, want) {
			t.Errorf("content lacks %q", want)
		}
		pages := pagesMeta(t, out)
		if len(pages) != 1 || pages[0]["truncated"] != true {
			t.Errorf("metadata pages = %v, want one truncated page", pages)
		}
	})

	t.Run("validates the urls argument", func(t *testing.T) {
		h, _ := newTool(nil, ExtractToolOptions{})
		for _, args := range []string{`{"urls":[]}`, `{"urls":["https://example.com/0","https://example.com/1","https://example.com/2","https://example.com/3","https://example.com/4","https://example.com/5"]}`} {
			out := runExtract(t, h, args)
			if !isErr(out) || out.Retryable == nil || *out.Retryable {
				t.Errorf("%s: want an error with retryable false, got %+v", args, out)
			}
		}
	})

	t.Run("reports a timeout", func(t *testing.T) {
		h := NewExtractTool(ExtractToolOptions{
			TimeoutMs: 50,
			Resolver:  testResolver(),
			Transport: hungTransport{},
		})
		out := runExtract(t, h, `{"urls":["https://slow.example/"]}`)
		if !strings.Contains(out.Content, "timed out after 50 ms") {
			t.Errorf("content = %q", out.Content)
		}
	})
}

// hungTransport never answers: it waits for the request's context to end.
type hungTransport struct{}

func (hungTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	<-req.Context().Done()
	return nil, req.Context().Err()
}

// TestWebExtract_ContextCancelled: stopping the run stops the fetches. Every page ends
// promptly with `cancelled`, never with the timeout
// text, although the per-page timeout is the long default.
func TestWebExtract_ContextCancelled(t *testing.T) {
	var inFlight atomic.Int32
	started := make(chan struct{}, 8)
	rt := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		inFlight.Add(1)
		started <- struct{}{}
		<-req.Context().Done()
		return nil, req.Context().Err()
	})
	h := NewExtractTool(ExtractToolOptions{Transport: rt, Resolver: testResolver()})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	type result struct {
		out toolhub.Output
		dur time.Duration
	}
	done := make(chan result, 1)
	go func() {
		start := time.Now()
		args, _ := kernel.NewArguments([]byte(`{"urls":["https://a.example/","https://b.example/","https://c.example/"]}`))
		out, _ := h.Execute(ctx, args, toolhub.Context{Emit: func(string, any) {}})
		done <- result{out, time.Since(start)}
	}()
	for range 3 {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("the three fetches did not start")
		}
	}
	cancel()
	select {
	case r := <-done:
		if r.dur > 5*time.Second {
			t.Errorf("Execute took %v after the cancel, want it prompt", r.dur)
		}
		if !isErr(r.out) {
			t.Errorf("every page was cancelled, the call must be an error: %+v", r.out)
		}
		if n := strings.Count(r.out.Content, "Error: cancelled"); n != 3 {
			t.Errorf("%d pages report cancelled, want 3: %q", n, r.out.Content)
		}
		if strings.Contains(r.out.Content, "timed out") {
			t.Errorf("a cancelled run must not read as a timeout: %q", r.out.Content)
		}
		if r.out.Retryable != nil && !*r.out.Retryable {
			t.Errorf("a cancelled page is not a blocked page, retryable must stay unset")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Execute did not return after the cancel")
	}
	if inFlight.Load() != 3 {
		t.Errorf("%d fetches started, want 3", inFlight.Load())
	}
}

// TestWebExtract_ConcurrentPages: five pages are fetched concurrently (the transport
// blocks each request until all five are in flight, so a sequential tool would hang), and the
// output lists them in the order of the urls.
func TestWebExtract_ConcurrentPages(t *testing.T) {
	const n = 5
	var mu sync.Mutex
	arrived := 0
	all := make(chan struct{})
	rt := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		mu.Lock()
		arrived++
		if arrived == n {
			close(all)
		}
		mu.Unlock()
		select {
		case <-all:
		case <-time.After(10 * time.Second):
			return nil, errors.New("the pages were not fetched concurrently")
		case <-req.Context().Done():
			return nil, req.Context().Err()
		}
		spec := htmlPage("<p>body of " + req.URL.Path + "</p>")
		raw, _ := base64.StdEncoding.DecodeString(*spec.Body)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"text/html"}},
			Body:       io.NopCloser(strings.NewReader(string(raw))),
			Request:    req,
		}, nil
	})
	h := NewExtractTool(ExtractToolOptions{Transport: rt, Resolver: testResolver()})

	urls := make([]string, n)
	for i := range urls {
		urls[i] = fmt.Sprintf("https://example.com/p%d", n-i) // reverse numbering: order is the input's
	}
	argsJSON, _ := json.Marshal(map[string]any{"urls": urls})
	out := runExtract(t, h, string(argsJSON))
	if isErr(out) {
		t.Fatalf("unexpected error: %q", out.Content)
	}
	var want []string
	for _, u := range urls {
		path := strings.TrimPrefix(u, "https://example.com")
		want = append(want, "## "+u+"\n\nbody of "+path)
	}
	if got := out.Content; got != strings.Join(want, "\n\n---\n\n") {
		t.Errorf("output not in input order\n got: %q\nwant: %q", got, strings.Join(want, "\n\n---\n\n"))
	}
	for i, p := range pagesMeta(t, out) {
		if p["url"] != urls[i] || p["ok"] != true {
			t.Errorf("metadata page %d = %v, want url %s ok", i, p, urls[i])
		}
	}
}

// endless is a body that never ends: it serves 'a' forever and counts what was read.
type endless struct{ read *atomic.Int64 }

func (e endless) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'a'
	}
	e.read.Add(int64(len(p)))
	return len(p), nil
}
func (endless) Close() error { return nil }

// TestWebExtract_BodyCap: a body that never ends is read for MaxBytes bytes and no more, and
// the page text is cut to max_chars, so an attacker's page cannot fill the memory.
func TestWebExtract_BodyCap(t *testing.T) {
	var read atomic.Int64
	rt := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"text/plain"}},
			Body:       endless{read: &read},
			Request:    req,
		}, nil
	})

	t.Run("custom cap", func(t *testing.T) {
		read.Store(0)
		h := NewExtractTool(ExtractToolOptions{Transport: rt, Resolver: testResolver(), MaxBytes: 4096})
		out := runExtract(t, h, `{"urls":["https://example.com/"],"max_chars":100}`)
		if got := read.Load(); got < 4096 || got > 4096+32*1024 {
			t.Errorf("read %d bytes of an endless body, want about 4096", got)
		}
		if want := ellipsis + "[truncated 3996 chars]"; !strings.Contains(out.Content, want) {
			t.Errorf("content lacks %q: %.200q", want, out.Content)
		}
	})

	t.Run("default cap of 2 MB", func(t *testing.T) {
		read.Store(0)
		h := NewExtractTool(ExtractToolOptions{Transport: rt, Resolver: testResolver()})
		out := runExtract(t, h, `{"urls":["https://example.com/"]}`)
		if got := read.Load(); got < 2_000_000 || got > 2_000_000+64*1024 {
			t.Errorf("read %d bytes of an endless body, want about 2000000", got)
		}
		if want := ellipsis + "[truncated 1980000 chars]"; !strings.Contains(out.Content, want) {
			t.Errorf("content lacks %q", want)
		}
	})
}

// TestWebExtract_DefaultTransportIsHardened: with no Transport the tool refuses a loopback
// server at dial time and the server sees nothing; AllowPrivateNetwork reaches it.
func TestWebExtract_DefaultTransportIsHardened(t *testing.T) {
	srv, hits := countingServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("loopback body"))
	})

	t.Run("refused", func(t *testing.T) {
		h := NewExtractTool(ExtractToolOptions{})
		defer func() { _ = h.Dispose(t.Context()) }()
		out := runExtract(t, h, `{"urls":["`+srv.URL+`/"]}`)
		if !isErr(out) || out.Retryable == nil || *out.Retryable {
			t.Errorf("want a non-retryable error, got %+v", out)
		}
		if !strings.Contains(out.Content, "blocked: address 127.0.0.1 is not a public address") {
			t.Errorf("content = %q", out.Content)
		}
		if hits.Load() != 0 {
			t.Errorf("the server saw %d requests, want 0", hits.Load())
		}
	})

	t.Run("allowPrivateNetwork", func(t *testing.T) {
		h := NewExtractTool(ExtractToolOptions{AllowPrivateNetwork: true})
		defer func() { _ = h.Dispose(t.Context()) }()
		out := runExtract(t, h, `{"urls":["`+srv.URL+`/"]}`)
		if isErr(out) || !strings.Contains(out.Content, "loopback body") {
			t.Errorf("content = %q", out.Content)
		}
		if hits.Load() != 1 {
			t.Errorf("the server saw %d requests, want 1", hits.Load())
		}
	})
}

// TestWebExtract_Dispose: Dispose is safe to call twice and on a tool with an injected
// transport (which is the host's and is left alone).
func TestWebExtract_Dispose(t *testing.T) {
	srv, _ := countingServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("hi"))
	})
	t.Run("default transport", func(t *testing.T) {
		h := NewExtractTool(ExtractToolOptions{AllowPrivateNetwork: true})
		if h.own == nil {
			t.Fatal("the tool must build its own transport when none is injected")
		}
		runExtract(t, h, `{"urls":["`+srv.URL+`/"]}`)
		for range 2 {
			if err := h.Dispose(t.Context()); err != nil {
				t.Fatalf("Dispose: %v", err)
			}
		}
	})
	t.Run("injected transport", func(t *testing.T) {
		h := NewExtractTool(ExtractToolOptions{Transport: hungTransport{}})
		if h.own != nil {
			t.Fatal("an injected transport must not be replaced")
		}
		for range 2 {
			if err := h.Dispose(t.Context()); err != nil {
				t.Fatalf("Dispose: %v", err)
			}
		}
	})
}

// TestExtractToolOptions: a zero field is the default, MaxRedirects is a pointer so that
// zero follows none, the limit has a floor of 1 000, and the default never exceeds the limit.
func TestExtractToolOptions(t *testing.T) {
	d := NewExtractTool(ExtractToolOptions{})
	if d.maxURLs != 5 || d.timeout != 15*time.Second || d.maxBytes != 2_000_000 || d.defaultMaxChars != 20_000 ||
		d.maxCharsLimit != 50_000 || d.maxRedirects != 5 || d.userAgent != "AECAgent/1.0 (+web_extract)" {
		t.Errorf("defaults = %+v, want the TS defaults", d)
	}
	zero := 0
	if got := NewExtractTool(ExtractToolOptions{MaxRedirects: &zero}).maxRedirects; got != 0 {
		t.Errorf("MaxRedirects 0 = %d, want 0 (follow none)", got)
	}
	if got := NewExtractTool(ExtractToolOptions{MaxCharsLimit: 200}).maxCharsLimit; got != 1000 {
		t.Errorf("MaxCharsLimit 200 = %d, want the floor of 1000", got)
	}
	c := NewExtractTool(ExtractToolOptions{DefaultMaxChars: 90_000, MaxCharsLimit: 1500})
	if c.defaultMaxChars != 1500 {
		t.Errorf("default above the limit = %d, want %d", c.defaultMaxChars, 1500)
	}
	desc := d.Definition().Parameters.Properties
	if desc == nil {
		t.Fatal("no properties")
	}
	if p, _ := desc.Get("urls"); !strings.Contains(p.Description, "(1-"+strconv.Itoa(5)+")") {
		t.Errorf("urls description = %q", p.Description)
	}
}

// TestWebExtract_PagePanicBecomesPageError (review WR-A4 / WR-B01): a panic while one page is
// being read, here in the injected Transport, must not end the process; it becomes a
// failed page. The other page still succeeds, the panic value (it may carry
// page content) stays out of the output, and the call is not an error.
func TestWebExtract_PagePanicBecomesPageError(t *testing.T) {
	rt := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host == "boom.example" {
			panic("secret panic value from page content")
		}
		return (&routeTransport{routes: map[string]routeSpec{"https://ok.example/": htmlPage("<p>fine</p>")}}).RoundTrip(req)
	})
	h := NewExtractTool(ExtractToolOptions{Transport: rt, Resolver: testResolver()})
	out := runExtract(t, h, `{"urls":["https://boom.example/","https://ok.example/"]}`)
	if isErr(out) {
		t.Fatalf("one page succeeded, the call must not be an error: %s", out.Content)
	}
	if !strings.Contains(out.Content, "## https://boom.example/\nError: "+pagePanicText) {
		t.Errorf("the panicking page must be a page error: %q", out.Content)
	}
	if !strings.Contains(out.Content, "fine") {
		t.Errorf("the other page must still be read: %q", out.Content)
	}
	if strings.Contains(out.Content, "secret panic value") {
		t.Errorf("the panic value leaked into the output: %q", out.Content)
	}
	pages := pagesMeta(t, out)
	if len(pages) != 2 || pages[0]["ok"] != false || pages[1]["ok"] != true {
		t.Errorf("metadata pages: %+v", pages)
	}
}

// TestWebSearchAndExtract_DefinitionIsACopy (review IN-A4): the definition is a fresh deep
// value on every call, as its comment says; changing one copy changes neither the tool nor
// the next copy.
func TestWebSearchAndExtract_DefinitionIsACopy(t *testing.T) {
	for name, h := range map[string]interface{ Definition() toolhub.Definition }{
		"web_extract": NewExtractTool(ExtractToolOptions{}),
		"web_search":  NewSearchTool(SearchToolOptions{Provider: NewDuckDuckGoSearchProvider(DuckDuckGoSearchProviderOptions{})}),
	} {
		d1 := h.Definition()
		want := d1.Clone()
		if len(d1.Parameters.Required) > 0 {
			d1.Parameters.Required[0] = "tampered"
		}
		for _, p := range d1.Parameters.Properties.All() {
			if p.Items != nil {
				p.Items.Type = "tampered"
			}
		}
		d1.Name = "tampered"
		d2 := h.Definition()
		got2, _ := json.Marshal(d2)
		want2, _ := json.Marshal(want)
		if string(got2) != string(want2) {
			t.Errorf("%s: definition changed through a returned copy:\n got %s\nwant %s", name, got2, want2)
		}
	}
}
