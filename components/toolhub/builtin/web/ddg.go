package web

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Phong95/aec-agent-go/internal/jsstr"
)

// ---------------------------------------------------------------------------
// DuckDuckGo: free, key-less search through the HTML endpoint (the same source Hermes
// Agent uses through the ddgs library). There is no official API: the markup can change
// and heavy use gets rate-limited, so the provider sits behind SearchProvider and a keyed
// provider can replace it.
// ---------------------------------------------------------------------------

const (
	ddgDefaultEndpoint  = "https://html.duckduckgo.com/html/"
	ddgDefaultUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0 Safari/537.36"
	ddgDefaultTimeout   = 10 * time.Second
	// ddgMaxBodyChars is the body limit: a response body above this many UTF-16
	// units is cut before parsing.
	ddgMaxBodyChars = 2_000_000
	// ddgMaxBodyBytes is how many bytes of the response are read: 6 000 000 hold at least
	// ddgMaxBodyChars UTF-16 units of any text (a UTF-16 unit takes at most 3 UTF-8 bytes),
	// so the result equals reading the whole body and then slicing it.
	ddgMaxBodyBytes = 3 * ddgMaxBodyChars
)

// DuckDuckGoSearchProvider is the key-less search backend.
type DuckDuckGoSearchProvider struct {
	endpoint  string
	timeout   time.Duration
	region    string
	userAgent string
	client    *http.Client
}

// NewDuckDuckGoSearchProvider builds the provider with the defaults for every zero option.
// A zero field cannot be told from an absent one and means the default.
func NewDuckDuckGoSearchProvider(o DuckDuckGoSearchProviderOptions) *DuckDuckGoSearchProvider {
	p := &DuckDuckGoSearchProvider{
		endpoint:  o.Endpoint,
		timeout:   ddgDefaultTimeout,
		region:    o.Region,
		userAgent: o.UserAgent,
	}
	if p.endpoint == "" {
		p.endpoint = ddgDefaultEndpoint
	}
	if o.TimeoutMs > 0 {
		p.timeout = time.Duration(o.TimeoutMs) * time.Millisecond
	}
	if p.userAgent == "" {
		p.userAgent = ddgDefaultUserAgent
	}
	rt := o.Transport
	if rt == nil {
		// No proxy (Node's fetch reads no proxy variable) and no kept-alive connection (the
		// SDK never disposes its tools). The endpoint is developer-configured, so the
		// dial-time SSRF check does not apply here.
		rt = &http.Transport{
			Proxy:               nil,
			DialContext:         (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
			ForceAttemptHTTP2:   true,
			DisableKeepAlives:   true,
			TLSHandshakeTimeout: 10 * time.Second,
		}
	}
	p.client = &http.Client{Transport: rt, CheckRedirect: sameHostRedirect}
	return p
}

// sameHostRedirect is the redirect policy of the DuckDuckGo client: a redirect is followed only
// to the host of the original request (at most 5 hops); any other target stops the chain and
// the 3xx answer is reported as `DuckDuckGo returned HTTP <n>`. The endpoint is developer
// configuration, but a compromised or misconfigured one must not turn the provider into an
// SSRF source that reaches another host (review IN-A2).
func sameHostRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 5 || req.URL.Host != via[0].URL.Host {
		return http.ErrUseLastResponse
	}
	return nil
}

// Name is "duckduckgo".
func (p *DuckDuckGoSearchProvider) Name() string { return "duckduckgo" }

// Search POSTs the query (and the region, the query's over the provider's default) to the
// HTML endpoint and parses the result list. A throttled client gets the rate-limit
// errors: HTTP 202, or a 200 page with the bot-check markers. The per-call deadline is
// derived from ctx, so stopping the run stops the request.
func (p *DuckDuckGoSearchProvider) Search(ctx context.Context, q SearchQuery) ([]SearchResult, error) {
	region := q.Region
	if region == "" {
		region = p.region
	}
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint, strings.NewReader(encodeSearchForm(q.Query, region)))
	if err != nil {
		return nil, &pageError{msg: errorText(err), cause: err}
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", p.userAgent)
	req.Header.Set("Accept", "text/html")

	resp, err := p.client.Do(req) //nolint:gosec // the endpoint is developer configuration, not model input
	if err != nil {
		return nil, p.requestError(ctx, err)
	}
	defer func() { _ = resp.Body.Close() }()

	// DuckDuckGo answers a throttled client with 202 and a bot-check page.
	if resp.StatusCode == http.StatusAccepted {
		return nil, errors.New("DuckDuckGo rate-limited the request (HTTP 202); try again later")
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, errors.New("DuckDuckGo returned HTTP " + strconv.Itoa(resp.StatusCode))
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, ddgMaxBodyBytes))
	if err != nil {
		return nil, p.requestError(ctx, err)
	}
	if len(raw) >= 3 && raw[0] == 0xEF && raw[1] == 0xBB && raw[2] == 0xBF {
		raw = raw[3:] // response.text() strips a UTF-8 byte order mark
	}
	html := jsstr.TruncateUTF16(jsstr.DecodeUTF8(raw), ddgMaxBodyChars)
	if strings.Contains(html, "anomaly-modal") || strings.Contains(html, "bots use DuckDuckGo too") {
		return nil, errors.New("DuckDuckGo rate-limited the request (bot check); try again later")
	}
	return parseDuckDuckGoHTML(html, q.MaxResults), nil
}

// requestError maps a failure of the request or the body read: the call's deadline is the
// text `The operation was aborted due to timeout`, a cancelled run is `cancelled`, anything else
// keeps the network error's text (without the URL net/http puts in front of it).
func (p *DuckDuckGoSearchProvider) requestError(ctx context.Context, err error) error {
	switch ctxErr := ctx.Err(); {
	case errors.Is(ctxErr, context.DeadlineExceeded):
		return &pageError{msg: "The operation was aborted due to timeout", cause: context.DeadlineExceeded}
	case ctxErr != nil:
		return &pageError{msg: "cancelled", cause: ctxErr}
	}
	return &pageError{msg: errorText(err), cause: err}
}

// ---------------------------------------------------------------------------
// form encoding (URLSearchParams)
// ---------------------------------------------------------------------------

// formEncode is the application/x-www-form-urlencoded byte serializer that
// URLSearchParams.toString() uses: ASCII alphanumerics and * - . _ stay, a space is +, every
// other byte is %XX (uppercase) of its UTF-8 form. It is not url.QueryEscape, which also
// escapes * and leaves ~ alone. Invalid UTF-8 becomes U+FFFD first (a JS string cannot hold
// it).
func formEncode(s string) string {
	b := []byte(s)
	if !utf8.ValidString(s) {
		b = []byte(jsstr.DecodeUTF8(b))
	}
	var sb strings.Builder
	sb.Grow(len(b))
	for _, c := range b {
		switch {
		case c == ' ':
			sb.WriteByte('+')
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '*', c == '-', c == '.', c == '_':
			sb.WriteByte(c)
		default:
			sb.WriteByte('%')
			sb.WriteByte(upperHex[c>>4])
			sb.WriteByte(upperHex[c&0xf])
		}
	}
	return sb.String()
}

// encodeSearchForm is new URLSearchParams({q}).toString() plus kl when a region is set:
// `q=<query>` then `&kl=<region>`.
func encodeSearchForm(query, region string) string {
	body := "q=" + formEncode(query)
	if region != "" {
		body += "&kl=" + formEncode(region)
	}
	return body
}

// ---------------------------------------------------------------------------
// result parser
// ---------------------------------------------------------------------------

const ddgResultMarker = `class="result__a"`

// parseDuckDuckGoHTML parses the result list from html.duckduckgo.com markup. Each organic
// result has an `<a class="result__a" href="//duckduckgo.com/l/?uddg=<target>">title</a>` and
// an optional `class="result__snippet"` element after it. Ads (/y.js links) and targets that
// are not http(s) are skipped, and a URL is listed once.
//
// The scan is an indexOf-style scan with three searches bounded so the whole parse is
// linear: the `>` and `</a>` searches stop at the end of the result's own segment, and the
// backward search for the opening `<a` resumes where the previous one stopped. The results
// equal an unbounded scan: it only uses a `</a>` that starts before
// the next marker, and the nearest `<a` before a marker is the nearest one in the window
// since the previous marker, or the previous answer when the window has none. That stale
// anchor can span many markers, so the href scan of its tag is incremental and memoized
// (hrefScan): without that a page of markers with no "<a" of their own was quadratic.
func parseDuckDuckGoHTML(html string, maxResults int) []SearchResult {
	return parseDuckDuckGoHTMLStats(html, maxResults, nil)
}

// parseDuckDuckGoHTMLStats is parseDuckDuckGoHTML that also counts the work of the href
// scan in st (nil to skip): the tests bound it by the size of the page, a deterministic
// complexity check that does not depend on the clock.
func parseDuckDuckGoHTMLStats(html string, maxResults int, st *ddgStats) []SearchResult {
	results := make([]SearchResult, 0)
	var hs hrefScan
	hs.st = st
	seen := map[string]bool{}
	pos := strings.Index(html, ddgResultMarker)
	tagStart := -1 // the last "<a" that starts at or before the previous marker
	from := 0      // the first offset not yet searched for "<a"
	for pos != -1 && len(results) < maxResults {
		next := -1
		if k := strings.Index(html[pos+len(ddgResultMarker):], ddgResultMarker); k >= 0 {
			next = pos + len(ddgResultMarker) + k
		}
		segmentEnd := len(html)
		if next != -1 {
			segmentEnd = next
		}

		// lastIndexOf("<a", pos): the last occurrence that starts at or before pos.
		if k := strings.LastIndex(html[from:pos+len("<a")], "<a"); k >= 0 {
			tagStart = from + k
		}
		from = pos + 1

		tagEnd, closeA := -1, -1
		if k := strings.IndexByte(html[pos:segmentEnd], '>'); k >= 0 {
			tagEnd = pos + k
			if c := strings.Index(html[tagEnd:segmentEnd], "</a>"); c >= 0 {
				closeA = tagEnd + c
			}
		}
		if tagStart != -1 && tagEnd != -1 && closeA != -1 {
			// A marker window without its own "<a" reuses the previous anchor (a
			// lastIndexOf), so the tag can span many markers: the href scan resumes where it
			// stopped and its result is memoized, which keeps the whole parse linear.
			url := hs.url(html, tagStart, tagEnd)
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

// ddgStats counts the work of the href scan of one parse: runes stepped and bytes searched
// for the closing quote.
type ddgStats struct{ steps int }

// hrefScan is hrefOf made incremental. The tag of a marker runs from the last "<a" to the
// marker's ">", and when a window has no "<a" of its own the tag keeps its start and only
// its end moves on; scanning each tag from its start again would be quadratic in the number
// of markers. The first match of /\shref="([^"]*)"/ is decided by the first \shref=" alone
// (a later one would have to end in a quote, and that quote already closes the first), so
// the scan keeps that position, the closing quote and the resolved link for the current
// tag start, and only looks at the bytes that are new. It returns what hrefOf returns.
type hrefScan struct {
	st      *ddgStats
	started bool
	start   int // the tag start the state belongs to
	i       int // the first offset not yet examined for \shref="
	vstart  int // the offset after href=", or -1 before one is found
	quote   int // the first quote at or after vstart: -2 unknown, -1 none
	urlFor  int // the vstart the cached link belongs to, -1 for none
	urlVal  string
}

const hrefAttr = `href="`

func (h *hrefScan) reset(tagStart int) {
	*h = hrefScan{st: h.st, started: true, start: tagStart, i: tagStart, vstart: -1, quote: -2, urlFor: -1}
}

// find is hrefOf(html[tagStart:tagEnd+1]) for tag ends that only grow while tagStart stays.
func (h *hrefScan) find(html string, tagStart, tagEnd int) (string, bool) {
	end := tagEnd + 1
	if !h.started || h.start != tagStart {
		h.reset(tagStart)
	}
	for h.vstart < 0 && h.i < end {
		r, size := utf8.DecodeRuneInString(html[h.i:end])
		if h.i+size+len(hrefAttr) > end {
			break // a match cannot fit yet; look again when the tag has grown
		}
		if h.st != nil {
			h.st.steps++
		}
		if jsstr.IsSpace(r) && strings.HasPrefix(html[h.i+size:end], hrefAttr) {
			h.vstart = h.i + size + len(hrefAttr)
			break
		}
		h.i += size
	}
	if h.vstart < 0 {
		return "", false
	}
	if h.quote == -2 {
		h.quote = -1
		searched := len(html) - h.vstart
		if q := strings.IndexByte(html[h.vstart:], '"'); q >= 0 {
			h.quote = h.vstart + q
			searched = q + 1
		}
		if h.st != nil {
			h.st.steps += searched
		}
	}
	if h.quote < 0 || h.quote >= end {
		return "", false
	}
	return html[h.vstart:h.quote], true
}

// url is the resolved result link of the tag ("" when it has no href). The link is
// resolved once per href: a stale tag that spans many markers must not decode it again for
// each of them.
func (h *hrefScan) url(html string, tagStart, tagEnd int) string {
	href, ok := h.find(html, tagStart, tagEnd)
	if !ok {
		return ""
	}
	if h.urlFor != h.vstart {
		h.urlFor = h.vstart
		h.urlVal = resolveResultURL(decodeEntities(href))
	}
	return h.urlVal
}

// hrefOf is /\shref="([^"]*)"/.exec(tag)?.[1]: the first whitespace character followed by
// href=" and a closing quote.
func hrefOf(tag string) (string, bool) {
	for i := 0; i < len(tag); {
		r, size := utf8.DecodeRuneInString(tag[i:])
		if jsstr.IsSpace(r) && strings.HasPrefix(tag[i+size:], `href="`) {
			rest := tag[i+size+len(`href="`):]
			if q := strings.IndexByte(rest, '"'); q >= 0 {
				return rest[:q], true
			}
		}
		i += size
	}
	return "", false
}

// extractSnippet is extractSnippet: the text of the element that carries
// class="result__snippet" in the result's segment.
func extractSnippet(segment string) string {
	idx := strings.Index(segment, `class="result__snippet"`)
	if idx == -1 {
		return ""
	}
	tagStart := strings.LastIndexByte(segment[:idx+1], '<')
	tagEnd := -1
	if k := strings.IndexByte(segment[idx:], '>'); k >= 0 {
		tagEnd = idx + k
	}
	if tagStart == -1 || tagEnd == -1 {
		return ""
	}
	tagName := "a"
	t := segment[tagStart:tagEnd]
	if len(t) > 1 && t[0] == '<' {
		j := 1
		for j < len(t) && (t[j] >= 'a' && t[j] <= 'z' || t[j] >= 'A' && t[j] <= 'Z' || t[j] >= '0' && t[j] <= '9') {
			j++
		}
		if j > 1 {
			tagName = asciiLower(t[1:j])
		}
	}
	end := len(segment)
	if c := strings.Index(segment[tagEnd:], "</"+tagName+">"); c != -1 {
		end = tagEnd + c
	}
	return collapseSpaces(htmlToText(segment[tagEnd+1 : end]))
}

// ddgBase is the base DuckDuckGo result links are resolved against.
var ddgBase = func() *webURL {
	u, err := parseURL("https://duckduckgo.com", nil)
	if err != nil {
		return nil
	}
	return u
}()

// resolveResultURL unwraps DuckDuckGo's redirect link and returns "" for ads and for
// targets that are not http(s). The host test is duckduckgo.com or a subdomain of it
// (case-insensitive), not a bare suffix match, so a link to notduckduckgo.com is kept as
// it is. A link whose scheme is neither http nor https is dropped before it is parsed.
func resolveResultURL(href string) string {
	in := href
	if strings.HasPrefix(href, "//") {
		in = "https:" + href
	}
	if sc := schemeOf(in); sc != "" && sc != "http" && sc != "https" {
		return ""
	}
	u, err := parseURL(in, ddgBase)
	if err != nil {
		return ""
	}
	if isDuckDuckGoHost(u.hostname()) {
		if strings.HasPrefix(u.Path, "/y.js") {
			return "" // sponsored result
		}
		target, ok := searchParamsGet(u.Query, "uddg")
		if !ok || target == "" {
			return ""
		}
		if sc := schemeOf(target); sc != "http" && sc != "https" {
			return ""
		}
		u, err = parseURL(target, nil)
		if err != nil {
			return ""
		}
	}
	if u.Scheme == "http" || u.Scheme == "https" {
		return u.String()
	}
	return ""
}

// isDuckDuckGoHost reports whether host is duckduckgo.com or one of its subdomains. A host
// that only ends in the same letters (evil-duckduckgo.com) is not.
func isDuckDuckGoHost(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	return host == "duckduckgo.com" || strings.HasSuffix(host, ".duckduckgo.com")
}

// searchParamsGet is URLSearchParams.get for the first pair with the name: form decoding,
// so + is a space and a percent escape is a UTF-8 byte (invalid UTF-8 becomes U+FFFD).
func searchParamsGet(query, name string) (string, bool) {
	for _, pair := range strings.Split(query, "&") {
		if pair == "" {
			continue
		}
		k, v, _ := strings.Cut(pair, "=")
		if formDecode(k) == name {
			return formDecode(v), true
		}
	}
	return "", false
}

func formDecode(s string) string {
	s = strings.ReplaceAll(s, "+", " ")
	b := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) {
			h, hok := hexVal(s[i+1])
			l, lok := hexVal(s[i+2])
			if hok && lok {
				b = append(b, h<<4|l)
				i += 2
				continue
			}
		}
		b = append(b, s[i])
	}
	return jsstr.DecodeUTF8(b)
}
