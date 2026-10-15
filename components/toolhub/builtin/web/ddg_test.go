package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Phong95/aec-agent-go/internal/wiretest"
)

// ddgHTML is the sample results page: an ad, two organic results (one with a snippet) and a
// result whose target is not http.
const ddgHTML = `
<div class="result results_links results_links_deep result--ad">
  <h2 class="result__title"><a rel="nofollow" class="result__a" href="https://duckduckgo.com/y.js?ad_domain=ads.example&amp;u3=x">Sponsored</a></h2>
  <a class="result__snippet" href="#">Buy now</a>
</div>
<div class="result results_links results_links_deep web-result ">
  <h2 class="result__title"><a rel="nofollow" class="result__a" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fwww.iso.org%2Fstandard%2F68078.html&amp;rut=abc">ISO 19650-1:2018 <b>BIM</b></a></h2>
  <a class="result__snippet" href="//duckduckgo.com/l/?uddg=x">Organization and digitization of information about <b>buildings</b> &amp; civil works</a>
</div>
<div class="result results_links results_links_deep web-result ">
  <h2 class="result__title"><a rel="nofollow" class="result__a" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fen.wikipedia.org%2Fwiki%2FISO_19650&amp;rut=def">ISO 19650 - Wikipedia</a></h2>
</div>
<div class="result results_links results_links_deep web-result ">
  <h2 class="result__title"><a rel="nofollow" class="result__a" href="//duckduckgo.com/l/?uddg=javascript%3Aalert(1)">Bad</a></h2>
</div>`

// recordedRequest is what a scripted round tripper saw.
type recordedRequest struct {
	URL         string
	Method      string
	Header      http.Header
	Body        string
	Deadline    time.Time
	HasDeadline bool
}

// scriptedTransport answers every request with the scripted status and body and records the
// request, like an injected fetch double.
type scriptedTransport struct {
	mu     sync.Mutex
	status int
	body   io.Reader
	calls  []recordedRequest
}

func newScripted(status int, body string) *scriptedTransport {
	return &scriptedTransport{status: status, body: strings.NewReader(body)}
}

func (s *scriptedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var sent []byte
	if req.Body != nil {
		sent, _ = io.ReadAll(req.Body)
		_ = req.Body.Close()
	}
	deadline, has := req.Context().Deadline()
	s.mu.Lock()
	s.calls = append(s.calls, recordedRequest{URL: req.URL.String(), Method: req.Method, Header: req.Header.Clone(), Body: string(sent), Deadline: deadline, HasDeadline: has})
	s.mu.Unlock()
	return &http.Response{
		StatusCode: s.status,
		Status:     http.StatusText(s.status),
		Header:     http.Header{"Content-Type": {"text/html; charset=utf-8"}},
		Body:       io.NopCloser(s.body),
		Request:    req,
	}, nil
}

func (s *scriptedTransport) last(t *testing.T) recordedRequest {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.calls) == 0 {
		t.Fatal("the transport saw no request")
	}
	return s.calls[len(s.calls)-1]
}

// TestDuckDuckGoParse checks the parser: organic results, redirect
// links unwrapped, ads and non-http targets skipped, a result without a snippet.
func TestDuckDuckGoParse(t *testing.T) {
	want := []SearchResult{
		{Title: "ISO 19650-1:2018 BIM", URL: "https://www.iso.org/standard/68078.html", Snippet: "Organization and digitization of information about buildings & civil works"},
		{Title: "ISO 19650 - Wikipedia", URL: "https://en.wikipedia.org/wiki/ISO_19650", Snippet: ""},
	}
	if got := parseDuckDuckGoHTML(ddgHTML, 10); !slices.Equal(got, want) {
		t.Fatalf("results = %+v, want %+v", got, want)
	}
	if got := parseDuckDuckGoHTML(ddgHTML, 1); len(got) != 1 {
		t.Fatalf("max results 1 gave %d results", len(got))
	}
	if got := parseDuckDuckGoHTML(ddgHTML, 0); got == nil || len(got) != 0 {
		t.Fatalf("max results 0 must give an empty, non-nil list: %#v", got)
	}
}

// TestDuckDuckGoSuffixQuirk pins the host check: a bare suffix match on "duckduckgo.com"
// would unwrap a link to notduckduckgo.com like one to duckduckgo.com. The provider unwraps
// duckduckgo.com and its subdomains only (case-insensitive); a look-alike host is kept as
// the plain result URL.
func TestDuckDuckGoSuffixQuirk(t *testing.T) {
	cases := []struct{ href, want string }{
		{"https://notduckduckgo.com/l/?uddg=https%3A%2F%2Ft.example", "https://notduckduckgo.com/l/?uddg=https%3A%2F%2Ft.example"},
		{"https://evil-duckduckgo.com/l/?uddg=https%3A%2F%2Ft.example", "https://evil-duckduckgo.com/l/?uddg=https%3A%2F%2Ft.example"},
		{"https://duckduckgo.com.evil.example/l/?uddg=https%3A%2F%2Ft.example", "https://duckduckgo.com.evil.example/l/?uddg=https%3A%2F%2Ft.example"},
		{"https://duckduckgo.com/l/?uddg=https%3A%2F%2Ft.example", "https://t.example/"},
		{"https://html.DuckDuckGo.com/l/?uddg=https%3A%2F%2Ft.example", "https://t.example/"},
		{"//duckduckgo.com/l/?uddg=https%3A%2F%2Ft.example", "https://t.example/"},
	}
	for _, c := range cases {
		html := `<a class="result__a" href="` + c.href + `">Suffix</a>`
		got := parseDuckDuckGoHTML(html, 5)
		if len(got) != 1 || got[0].URL != c.want {
			t.Errorf("%s: results = %+v, want URL %q", c.href, got, c.want)
		}
	}
}

// ddgFixture is builtin-ddg.json, recorded from a reference parser and provider by
// tools/wiregen/builtin/ddg.cases.mjs.
type ddgFixture struct {
	Parse []struct {
		HTML       string      `json:"html"`
		MaxResults int         `json:"maxResults"`
		Results    []ddgResult `json:"results"`
	} `json:"parse"`
	Search []struct {
		Name    string `json:"name"`
		Options struct {
			Endpoint  string `json:"endpoint"`
			Region    string `json:"region"`
			UserAgent string `json:"userAgent"`
			TimeoutMs int    `json:"timeoutMs"`
		} `json:"options"`
		Query struct {
			Query      string `json:"query"`
			MaxResults int    `json:"maxResults"`
			Region     string `json:"region"`
		} `json:"query"`
		Response struct {
			Status int     `json:"status"`
			Body   *string `json:"body"`
		} `json:"response"`
		Request struct {
			URL       string            `json:"url"`
			Method    string            `json:"method"`
			Headers   map[string]string `json:"headers"`
			Body      string            `json:"body"`
			HasSignal bool              `json:"hasSignal"`
		} `json:"request"`
		Outcome struct {
			Results []ddgResult `json:"results"`
			Error   *string     `json:"error"`
		} `json:"outcome"`
	} `json:"search"`
}

type ddgResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet"`
}

func (r ddgResult) result() SearchResult {
	return SearchResult(r)
}

func loadDDGFixture(t *testing.T) ddgFixture {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(wiretest.Load(t, "builtin-ddg.json")))
	dec.DisallowUnknownFields()
	var fx ddgFixture
	if err := dec.Decode(&fx); err != nil {
		t.Fatalf("decode builtin-ddg: %v", err)
	}
	if len(fx.Parse) < 60 || len(fx.Search) < 15 {
		t.Fatalf("fixture too small: %d parse rows, %d search rows", len(fx.Parse), len(fx.Search))
	}
	return fx
}

// TestDDGCorpus replays the recorded corpus: the parser over 17 pages at four result limits,
// and the whole provider (request and outcome) over the scripted responses, against the
// recorded values.
func TestDDGCorpus(t *testing.T) {
	fx := loadDDGFixture(t)

	t.Run("parse", func(t *testing.T) {
		for _, row := range fx.Parse {
			got := parseDuckDuckGoHTML(row.HTML, row.MaxResults)
			want := make([]SearchResult, 0, len(row.Results))
			for _, r := range row.Results {
				want = append(want, r.result())
			}
			// The recording unwraps the look-alike notduckduckgo.com redirect link to its uddg
			// target; the provider keeps the link itself (TestDuckDuckGoSuffixQuirk).
			const lookAlike = "https://notduckduckgo.com/l/?uddg=https%3A%2F%2Ft.example"
			for i := range want {
				if want[i].Title == "Suffix" && want[i].URL == "https://t.example/" && strings.Contains(row.HTML, `href="`+lookAlike+`"`) {
					want[i].URL = lookAlike
				}
			}
			if !slices.Equal(got, want) {
				t.Errorf("parse(%q, %d):\n  TS %+v\n  Go %+v", row.HTML, row.MaxResults, want, got)
			}
		}
	})

	t.Run("search", func(t *testing.T) {
		for _, row := range fx.Search {
			t.Run(row.Name, func(t *testing.T) {
				body := ""
				if row.Response.Body != nil {
					body = *row.Response.Body
				}
				tr := newScripted(row.Response.Status, body)
				p := NewDuckDuckGoSearchProvider(DuckDuckGoSearchProviderOptions{
					Endpoint: row.Options.Endpoint, Region: row.Options.Region, UserAgent: row.Options.UserAgent, TimeoutMs: row.Options.TimeoutMs, Transport: tr,
				})
				got, err := p.Search(t.Context(), SearchQuery{Query: row.Query.Query, MaxResults: row.Query.MaxResults, Region: row.Query.Region})

				req := tr.last(t)
				if req.URL != row.Request.URL || req.Method != row.Request.Method || req.Body != row.Request.Body {
					t.Errorf("request: got %s %s %q, TS %s %s %q", req.Method, req.URL, req.Body, row.Request.Method, row.Request.URL, row.Request.Body)
				}
				if len(req.Header) != len(row.Request.Headers) {
					t.Errorf("request headers: got %v, TS %v", req.Header, row.Request.Headers)
				}
				for name, want := range row.Request.Headers {
					if v := req.Header.Get(name); v != want {
						t.Errorf("request header %s: got %q, TS %q", name, v, want)
					}
				}
				if req.HasDeadline != row.Request.HasSignal {
					t.Errorf("request deadline: got %v, TS signal %v", req.HasDeadline, row.Request.HasSignal)
				}

				if row.Outcome.Error != nil {
					if err == nil || err.Error() != *row.Outcome.Error {
						t.Fatalf("error: got %v, TS %q", err, *row.Outcome.Error)
					}
					return
				}
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				want := make([]SearchResult, 0, len(row.Outcome.Results))
				for _, r := range row.Outcome.Results {
					want = append(want, r.result())
				}
				if !slices.Equal(got, want) {
					t.Errorf("results:\n  TS %+v\n  Go %+v", want, got)
				}
			})
		}
	})
}

// TestDuckDuckGoPost checks the request: the query and the region
// are POSTed to the HTML endpoint.
func TestDuckDuckGoPost(t *testing.T) {
	tr := newScripted(200, ddgHTML)
	p := NewDuckDuckGoSearchProvider(DuckDuckGoSearchProviderOptions{Transport: tr})
	if p.Name() != "duckduckgo" {
		t.Fatalf("Name = %q", p.Name())
	}
	results, err := p.Search(t.Context(), SearchQuery{Query: "ISO 19650", MaxResults: 5, Region: "uk-en"})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("got %d results", len(results))
	}
	req := tr.last(t)
	if req.URL != "https://html.duckduckgo.com/html/" || req.Method != http.MethodPost || req.Body != "q=ISO+19650&kl=uk-en" {
		t.Fatalf("request = %s %s %q", req.Method, req.URL, req.Body)
	}
	wantHeaders := map[string]string{
		"Content-Type": "application/x-www-form-urlencoded",
		"Accept":       "text/html",
		"User-Agent":   "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0 Safari/537.36",
	}
	for name, want := range wantHeaders {
		if got := req.Header.Get(name); got != want {
			t.Errorf("header %s = %q, want %q", name, got, want)
		}
	}
	// the default timeout is 10 s and covers the whole call
	if !req.HasDeadline {
		t.Fatal("the request must carry the call deadline")
	}
	if d := time.Until(req.Deadline); d < 8*time.Second || d > 10*time.Second {
		t.Errorf("deadline in %v, want about 10 s", d)
	}
}

// TestDuckDuckGoRateLimited checks throttling: HTTP 202 and the
// bot-check pages are the rate-limit errors, and another non-2xx is `returned HTTP <status>`.
func TestDuckDuckGoRateLimited(t *testing.T) {
	for _, c := range []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"http_202", 202, "", "DuckDuckGo rate-limited the request (HTTP 202); try again later"},
		{"anomaly_modal_page", 200, `<div class="anomaly-modal"></div>`, "DuckDuckGo rate-limited the request (bot check); try again later"},
		{"bots_use_duckduckgo_too_page", 200, "<p>bots use DuckDuckGo too</p>", "DuckDuckGo rate-limited the request (bot check); try again later"},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := NewDuckDuckGoSearchProvider(DuckDuckGoSearchProviderOptions{Transport: newScripted(c.status, c.body)})
			results, err := p.Search(t.Context(), SearchQuery{Query: "q", MaxResults: 5})
			if err == nil || err.Error() != c.want || results != nil {
				t.Fatalf("got %v %v, want error %q", results, err, c.want)
			}
			if !strings.Contains(err.Error(), "rate-limited") {
				t.Fatal("the error must say rate-limited")
			}
		})
	}
	p := NewDuckDuckGoSearchProvider(DuckDuckGoSearchProviderOptions{Transport: newScripted(503, "")})
	if _, err := p.Search(t.Context(), SearchQuery{Query: "q", MaxResults: 5}); err == nil || err.Error() != "DuckDuckGo returned HTTP 503" {
		t.Fatalf("got %v", err)
	}
}

// countingBody is an endless body that counts how many bytes were read from it.
type countingBody struct {
	n    int64
	fill byte
}

func (c *countingBody) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = c.fill
	}
	c.n += int64(len(p))
	return len(p), nil
}

// TestDuckDuckGoBodyCap: the body is read through a LimitReader (6 000 000 bytes, enough for
// 2 000 000 UTF-16 units of any text) and cut to 2 000 000 UTF-16 units before parsing, so a
// marker beyond the cut is never seen and an endless body cannot exhaust memory.
func TestDuckDuckGoBodyCap(t *testing.T) {
	result := func(n string) string {
		return `<a class="result__a" href="https://` + n + `.example/">` + n + `</a>`
	}
	search := func(t *testing.T, body string) ([]SearchResult, error) {
		t.Helper()
		p := NewDuckDuckGoSearchProvider(DuckDuckGoSearchProviderOptions{Transport: newScripted(200, body)})
		return p.Search(t.Context(), SearchQuery{Query: "q", MaxResults: 10})
	}

	t.Run("a result past the cut is not parsed", func(t *testing.T) {
		pad := strings.Repeat("x", 2_000_000-100)
		got, err := search(t, pad+result("a")+strings.Repeat(" ", 100)+result("b"))
		if err != nil || len(got) != 1 || got[0].Title != "a" {
			t.Fatalf("got %+v %v, want only a", got, err)
		}
	})
	t.Run("the cut counts UTF-16 units, not bytes", func(t *testing.T) {
		// 1 999 900 two-byte characters are 3 999 800 bytes but 1 999 900 units
		pad := strings.Repeat(ux("{U+00E9}"), 1_999_900)
		got, err := search(t, pad+result("a")+strings.Repeat(" ", 100)+result("b"))
		if err != nil || len(got) != 1 || got[0].Title != "a" {
			t.Fatalf("got %+v %v, want only a", got, err)
		}
		// an astral character counts 2 units: 999 950 of them are 1 999 900 units
		pad = strings.Repeat(ux("{U+1F600}"), 999_950)
		got, err = search(t, pad+result("a")+strings.Repeat(" ", 100)+result("b"))
		if err != nil || len(got) != 1 || got[0].Title != "a" {
			t.Fatalf("got %+v %v, want only a", got, err)
		}
	})
	t.Run("a bot-check marker past the cut is not seen", func(t *testing.T) {
		got, err := search(t, strings.Repeat("x", 2_000_000)+"anomaly-modal"+result("a"))
		if err != nil || len(got) != 0 {
			t.Fatalf("got %+v %v", got, err)
		}
		if _, err := search(t, strings.Repeat("x", 2_000_000-5)+"anomaly-modal"); err != nil {
			t.Fatalf("a marker cut in half must not count: %v", err)
		}
		if _, err := search(t, strings.Repeat("x", 2_000_000-13)+"anomaly-modal"); err == nil {
			t.Fatal("a marker that ends exactly at the cut must count")
		}
	})
	t.Run("an endless body is read only up to the byte cap", func(t *testing.T) {
		body := &countingBody{fill: 'x'}
		tr := &scriptedTransport{status: 200, body: body}
		p := NewDuckDuckGoSearchProvider(DuckDuckGoSearchProviderOptions{Transport: tr})
		got, err := p.Search(t.Context(), SearchQuery{Query: "q", MaxResults: 10})
		if err != nil || len(got) != 0 {
			t.Fatalf("got %+v %v", got, err)
		}
		if body.n != 6_000_000 {
			t.Fatalf("read %d bytes, want exactly the cap of 6000000", body.n)
		}
	})
}

// TestDuckDuckGoContext: the call deadline and the caller's context both stop the request.
func TestDuckDuckGoContext(t *testing.T) {
	hang := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		<-req.Context().Done()
		return nil, req.Context().Err()
	})
	t.Run("timeout", func(t *testing.T) {
		p := NewDuckDuckGoSearchProvider(DuckDuckGoSearchProviderOptions{Transport: hang, TimeoutMs: 40})
		_, err := p.Search(t.Context(), SearchQuery{Query: "q", MaxResults: 5})
		if err == nil || err.Error() != "The operation was aborted due to timeout" || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("cancelled", func(t *testing.T) {
		p := NewDuckDuckGoSearchProvider(DuckDuckGoSearchProviderOptions{Transport: hang})
		ctx, cancel := context.WithCancel(t.Context())
		time.AfterFunc(20*time.Millisecond, cancel)
		_, err := p.Search(ctx, SearchQuery{Query: "q", MaxResults: 5})
		if err == nil || err.Error() != "cancelled" || !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("a network error keeps its text without the URL", func(t *testing.T) {
		boom := roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("connection refused") })
		p := NewDuckDuckGoSearchProvider(DuckDuckGoSearchProviderOptions{Transport: boom})
		_, err := p.Search(t.Context(), SearchQuery{Query: "q", MaxResults: 5})
		if err == nil || err.Error() != "connection refused" {
			t.Fatalf("got %v", err)
		}
	})
}

// TestDuckDuckGoDefaultTransport: the provider's own transport reads no proxy variable and
// keeps no connection alive.
func TestDuckDuckGoDefaultTransport(t *testing.T) {
	p := NewDuckDuckGoSearchProvider(DuckDuckGoSearchProviderOptions{})
	tr, ok := p.client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("default transport is %T", p.client.Transport)
	}
	if tr.Proxy != nil || !tr.DisableKeepAlives {
		t.Fatal("the default transport must read no proxy variable and keep no connection")
	}
}

// formFixture is builtin-form-encode.json: new URLSearchParams({q, kl}).toString().
type formFixture struct {
	Rows []struct {
		Q   string  `json:"q"`
		KL  *string `json:"kl"`
		Out string  `json:"out"`
	} `json:"rows"`
}

// TestFormEncodeCorpus replays URLSearchParams.toString() over printable ASCII, escapes,
// non-ASCII text and regions: a space is +, ~ is escaped, * stays.
func TestFormEncodeCorpus(t *testing.T) {
	var fx formFixture
	dec := json.NewDecoder(bytes.NewReader(wiretest.Load(t, "builtin-form-encode.json")))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&fx); err != nil {
		t.Fatalf("decode builtin-form-encode: %v", err)
	}
	if len(fx.Rows) < 100 {
		t.Fatalf("fixture too small: %d rows", len(fx.Rows))
	}
	for _, row := range fx.Rows {
		region := ""
		if row.KL != nil {
			region = *row.KL
		}
		if got := encodeSearchForm(row.Q, region); got != row.Out {
			t.Errorf("encodeSearchForm(%q, %q) = %q, TS %q", row.Q, region, got, row.Out)
		}
	}
	// the three characters that differ from net/url
	if got := formEncode("a b~*"); got != "a+b%7E*" {
		t.Errorf("formEncode = %q", got)
	}
	// invalid UTF-8 becomes U+FFFD first
	if got := formEncode("a\xffb"); got != "a%EF%BF%BDb" {
		t.Errorf("formEncode(invalid) = %q", got)
	}
}

// parseDuckDuckGoHTMLReference is the unbounded reference parser: every search runs to the
// end of the page. parseDuckDuckGoHTML bounds three of them; the test below proves the
// results are the same.
func parseDuckDuckGoHTMLReference(html string, maxResults int) []SearchResult {
	results := make([]SearchResult, 0)
	seen := map[string]bool{}
	indexFrom := func(s, sub string, from int) int {
		if from > len(s) {
			return -1
		}
		if k := strings.Index(s[from:], sub); k >= 0 {
			return from + k
		}
		return -1
	}
	pos := strings.Index(html, ddgResultMarker)
	for pos != -1 && len(results) < maxResults {
		next := indexFrom(html, ddgResultMarker, pos+len(ddgResultMarker))
		segmentEnd := len(html)
		if next != -1 {
			segmentEnd = next
		}
		tagStart := strings.LastIndex(html[:pos+len("<a")], "<a")
		tagEnd := indexFrom(html, ">", pos)
		closeA := -1
		if tagEnd != -1 {
			closeA = indexFrom(html, "</a>", tagEnd)
		}
		if tagStart != -1 && tagEnd != -1 && closeA != -1 && closeA < segmentEnd {
			tag := html[tagStart : tagEnd+1]
			url := ""
			if href, ok := hrefOf(tag); ok {
				url = resolveResultURL(decodeEntities(href))
			}
			title := collapseSpaces(htmlToText(html[tagEnd+1 : closeA]))
			if url != "" && title != "" && !seen[url] {
				seen[url] = true
				results = append(results, SearchResult{Title: title, URL: url, Snippet: extractSnippet(html[closeA:segmentEnd])})
			}
		}
		pos = next
	}
	return results
}

// TestDuckDuckGoParse_MatchesTheLiteralScan runs random pages built from the parser's own
// tokens through the bounded parser and the literal reference scan: the lists are equal.
func TestDuckDuckGoParse_MatchesTheLiteralScan(t *testing.T) {
	tokens := []string{
		`<a `, `<a class="result__a" href="https://x.example/1">`, `<a class="result__a" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fy.example%2F2">`,
		`class="result__a"`, `class="result__a" href="https://z.example/3"`, `>`, `</a>`, ` href="https://e.example/p"`, `class="result__snippet"`,
		`<span class="result__snippet">`, `</span>`, `<div `, `</div>`, `text `, `<`, ` `, `&amp;`, `<b>`, `</b>`, `<a href="https://q.example/4" `,
	}
	rng := rand.New(rand.NewPCG(11, 4))
	for range 4000 {
		var b strings.Builder
		for n := rng.IntN(60) + 1; n > 0; n-- {
			b.WriteString(tokens[rng.IntN(len(tokens))])
		}
		html := b.String()
		for _, limit := range []int{1, 3, 10} {
			got := parseDuckDuckGoHTML(html, limit)
			want := parseDuckDuckGoHTMLReference(html, limit)
			if !slices.Equal(got, want) {
				t.Fatalf("page %q limit %d:\n  literal %+v\n  bounded %+v", html, limit, want, got)
			}
		}
	}
}

// TestDuckDuckGoParse_StaleAnchorStaysLinear is the hostile page of review WR-A2: one
// opening "<a" followed by ten thousands of markers that have no "<a" of their own, so the
// tag of every marker is the stale one from the first. The literal scan walked that tag from
// its start for every marker (quadratic); the work counter must stay a small multiple of the
// page size, a deterministic bound that does not depend on the clock.
func TestDuckDuckGoParse_StaleAnchorStaysLinear(t *testing.T) {
	filler := strings.Repeat(`class="result__a">t</a>`, 90_000)
	pages := map[string]string{
		"no href":                  `<a class="result__a">t</a>` + filler,
		"unterminated big href":    `<a href="` + strings.Repeat("x", 500_000) + `class="result__a">t</a>` + filler,
		"href after many markers":  `<a ` + filler + ` href="https://late.example/">`,
		"markers without anything": filler,
	}
	for name, html := range pages {
		t.Run(name, func(t *testing.T) {
			var st ddgStats
			start := time.Now()
			got := parseDuckDuckGoHTMLStats(html, 10, &st)
			if limit := 4 * len(html); st.steps > limit {
				t.Fatalf("href scan did %d steps for a %d-byte page (limit %d): not linear", st.steps, len(html), limit)
			}
			t.Logf("%d bytes, %d steps, %v, %d results", len(html), st.steps, time.Since(start), len(got))
			// none of the hostile pages holds a result; the literal scan on small pages is compared
			// in TestDuckDuckGoParse_MatchesTheLiteralScan
			if len(got) != 0 {
				t.Fatalf("unexpected results: %+v", got)
			}
		})
	}
}

// TestHrefScan_MatchesHrefOf feeds growing tag ends to the incremental scan and compares each
// answer with hrefOf on the same tag, over random pages made of href-shaped tokens.
func TestHrefScan_MatchesHrefOf(t *testing.T) {
	tokens := []string{` href="a"`, ` href="`, `"`, ` `, "\u00a0href=\"b\"", `href="c"`, `x`, `>`, "\n", ` hre`, `f="d"`, "\u00e9"}
	rng := rand.New(rand.NewPCG(5, 9))
	for range 3000 {
		var b strings.Builder
		for n := rng.IntN(40) + 1; n > 0; n-- {
			b.WriteString(tokens[rng.IntN(len(tokens))])
		}
		html := b.String()
		var hs hrefScan
		for end := 0; end < len(html); end += rng.IntN(7) + 1 {
			gotV, gotOK := hs.find(html, 0, end)
			wantV, wantOK := hrefOf(html[:end+1])
			if gotV != wantV || gotOK != wantOK {
				t.Fatalf("page %q end %d: scan (%q,%v), hrefOf (%q,%v)", html, end, gotV, gotOK, wantV, wantOK)
			}
		}
	}
}

// TestDuckDuckGoRedirectsStayOnTheHost (review IN-A2): a redirect to another host is not
// followed (the 3xx is reported), a redirect on the same host is.
func TestDuckDuckGoRedirectsStayOnTheHost(t *testing.T) {
	other, otherHits := countingServer(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("secret")) })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/away":
			http.Redirect(w, r, other.URL+"/x", http.StatusTemporaryRedirect)
		case "/same":
			http.Redirect(w, r, "/landed", http.StatusTemporaryRedirect)
		default:
			_, _ = w.Write([]byte(ddgHTML))
		}
	}))
	t.Cleanup(srv.Close)

	away := NewDuckDuckGoSearchProvider(DuckDuckGoSearchProviderOptions{Endpoint: srv.URL + "/away"})
	if _, err := away.Search(t.Context(), SearchQuery{Query: "q"}); err == nil || err.Error() != "DuckDuckGo returned HTTP 307" {
		t.Fatalf("a redirect to another host must stop at the 307, got %v", err)
	}
	if n := otherHits.Load(); n != 0 {
		t.Fatalf("the other host was reached %d times", n)
	}
	same := NewDuckDuckGoSearchProvider(DuckDuckGoSearchProviderOptions{Endpoint: srv.URL + "/same"})
	res, err := same.Search(t.Context(), SearchQuery{Query: "q", MaxResults: 5})
	if err != nil || len(res) == 0 {
		t.Fatalf("a same-host redirect must be followed: %v %v", res, err)
	}
}
