package web

import (
	"context"
	"errors"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/internal/jsstr"
	"github.com/Phong95/aec-agent-go/internal/toolkit"
	"github.com/Phong95/aec-agent-go/kernel"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// The defaults of web_extract.
const (
	extractDefaultMaxURLs       = 5
	extractDefaultMaxChars      = 20_000
	extractDefaultMaxCharsLimit = 50_000
	extractDefaultMaxRedirects  = 5
	extractMinCharsLimit        = 1_000
	extractMinMaxChars          = 100
	// extractHTMLSniffUnits is how many UTF-16 units of a body with no content type are
	// searched for an <html tag.
	extractHTMLSniffUnits = 2_000
)

// extractTextContentTypes are the content types web_extract
// reads besides every text/* type.
var extractTextContentTypes = []string{
	"text/html", "application/xhtml+xml", "text/plain", "text/markdown", "text/csv",
	"application/json", "application/ld+json", "text/xml", "application/xml", "application/rss+xml",
	"application/atom+xml",
}

// ExtractTool is web_extract: read-only. It fetches public web pages
// and returns their readable text. Every URL and every redirect hop passes the SSRF guard
// (http and https only, no private, loopback, link-local or metadata address); bodies are
// size-capped and time-boxed.
//
// An ExtractTool is safe for concurrent use. Build one with NewExtractTool.
type ExtractTool struct {
	maxURLs         int
	timeout         time.Duration
	maxBytes        int64
	defaultMaxChars int
	maxCharsLimit   int
	maxRedirects    int
	userAgent       string
	allowPrivate    bool
	resolver        Resolver
	transport       http.RoundTripper
	// own is the transport the tool built itself (nil when the host injected one).
	own        *http.Transport
	definition toolhub.Definition
}

var _ toolhub.Handler = (*ExtractTool)(nil)

// NewExtractTool builds the tool with the defaults for every zero option. The default
// transport is built once and shared by every page of every call; its keep-alives are off
// (the SDK never disposes its tools on its own), and Dispose closes whatever idles on it.
func NewExtractTool(o ExtractToolOptions) *ExtractTool {
	t := &ExtractTool{
		maxURLs:         extractDefaultMaxURLs,
		timeout:         fetchDefaultTimeout,
		maxBytes:        fetchDefaultMaxBytes,
		maxCharsLimit:   extractDefaultMaxCharsLimit,
		defaultMaxChars: extractDefaultMaxChars,
		maxRedirects:    extractDefaultMaxRedirects,
		userAgent:       fetchDefaultUserAgent,
		allowPrivate:    o.AllowPrivateNetwork,
		resolver:        o.Resolver,
		transport:       o.Transport,
	}
	if o.MaxUrls > 0 {
		t.maxURLs = o.MaxUrls
	}
	if o.TimeoutMs > 0 {
		t.timeout = time.Duration(o.TimeoutMs) * time.Millisecond
	}
	if o.MaxBytes > 0 {
		t.maxBytes = int64(o.MaxBytes)
	}
	if o.MaxCharsLimit > 0 {
		t.maxCharsLimit = max(extractMinCharsLimit, o.MaxCharsLimit)
	}
	if o.DefaultMaxChars > 0 {
		t.defaultMaxChars = o.DefaultMaxChars
	}
	t.defaultMaxChars = min(t.maxCharsLimit, t.defaultMaxChars)
	if o.MaxRedirects != nil {
		t.maxRedirects = max(0, *o.MaxRedirects)
	}
	if o.UserAgent != "" {
		t.userAgent = o.UserAgent
	}
	if t.transport == nil {
		t.own = newTransport(t.allowPrivate)
		t.transport = t.own
	}
	t.definition = toolhub.Definition{
		Name: "web_extract",
		Description: "Fetch one or more public web pages (http/https) and return their readable text content. " +
			"Use after web_search to read the most relevant results, or for a url the user gave you. " +
			"Supports HTML, plain text, JSON and XML; not PDFs or binaries. Long pages are truncated.",
		Parameters: toolhub.JSONSchema{
			Type: "object",
			Properties: toolhub.NewProperties(
				toolhub.Property{Name: "urls", Schema: toolhub.JSONSchema{
					Type:        "array",
					Items:       &toolhub.JSONSchema{Type: "string"},
					Description: "Absolute http(s) urls to read (1-" + strconv.Itoa(t.maxURLs) + ")",
				}},
				toolhub.Property{Name: "max_chars", Schema: toolhub.JSONSchema{
					Type: "number",
					Description: "Max characters of text returned per page (default " + strconv.Itoa(t.defaultMaxChars) +
						", max " + strconv.Itoa(t.maxCharsLimit) + ")",
				}},
			),
			Required: []string{"urls"},
		},
		Risk: toolhub.RiskReadOnly,
		// A-103: a tool job may run it on a schedule (its exact arguments approved up front).
		Schedulable: true,
		// A-137: whole pages are the largest tool outputs; a subagent reads them and reports the facts.
		Subagent: toolhub.SubagentPreferred,
	}
	return t
}

// Name returns the plugin name.
func (*ExtractTool) Name() string { return "tool:web-extract" }

// Version returns the plugin version.
func (*ExtractTool) Version() string { return "1.0.0" }

// Definition returns the tool definition (a fresh value on every call).
func (t *ExtractTool) Definition() toolhub.Definition { return t.definition.Clone() }

// Dispose closes the idle connections of the transport the tool built. The default
// transport keeps no connection alive, so this matters for a host that wants the tool's
// sockets gone at a known point; an injected Transport is the host's and is left alone. It
// is safe to call more than once.
func (t *ExtractTool) Dispose(context.Context) error {
	if t.own != nil {
		t.own.CloseIdleConnections()
	}
	return nil
}

// pagePanicText is the page error of a page whose reading panicked.
const pagePanicText = "internal error while reading the page"

// pageResult is the outcome of one page.
type pageResult struct {
	url       string
	ok        bool
	blocked   bool
	finalURL  string
	title     string
	text      string
	truncated bool
	err       string
}

// Execute fetches the pages of one call. Up to MaxUrls pages are fetched concurrently, one
// goroutine each, and the output lists them in the order the model gave them. Each page's
// context derives from the tool's context plus the per-page timeout, so stopping the run
// cancels its fetches. Execute never returns an error: a failed page is part of the output,
// and the call is an error only when every page failed.
func (t *ExtractTool) Execute(ctx context.Context, args kernel.Arguments, _ toolhub.Context) (toolhub.Output, error) {
	a := toolkit.DecodeArgs(args)
	urls := cleanURLs(a.Strings("urls"))
	if len(urls) == 0 {
		return toolkit.Fail(`web_extract: "urls" must be a non-empty array of url strings`), nil
	}
	if len(urls) > t.maxURLs {
		return toolkit.Fail("web_extract: at most " + strconv.Itoa(t.maxURLs) + " urls per call (got " + strconv.Itoa(len(urls)) + ")"), nil
	}

	requested := float64(t.defaultMaxChars)
	if n, ok := a.Number("max_chars"); ok {
		requested = math.Floor(n)
	}
	maxChars := int(math.Min(float64(t.maxCharsLimit), math.Max(extractMinMaxChars, requested)))

	pages := make([]pageResult, len(urls))
	var wg sync.WaitGroup
	for i, u := range urls {
		wg.Go(func() {
			// A panic in a page goroutine is not caught by the caller (the hub's per-call recover
			// runs in another goroutine) and would end the host process, so it becomes a failed
			// page; the panic value is not put in the output, it may carry page content (review
			// WR-A4, WR-B01).
			defer func() {
				if r := recover(); r != nil {
					pages[i] = pageResult{url: u, err: pagePanicText}
				}
			}()
			pages[i] = t.extractOne(ctx, u, maxChars)
		})
	}
	wg.Wait()

	sections := make([]string, len(pages))
	failed, blocked := 0, 0
	metaPages := make([]any, len(pages))
	for i, p := range pages {
		if !p.ok {
			failed++
			if p.blocked {
				blocked++
			}
			sections[i] = "## " + p.url + "\nError: " + p.err
		} else {
			shown := p.finalURL
			if shown == "" {
				shown = p.url
			}
			header := []string{"## " + shown}
			if p.finalURL != "" && p.finalURL != p.url {
				header = append(header, "(redirected from "+p.url+")")
			}
			if p.title != "" {
				header = append(header, "Title: "+p.title)
			}
			text := p.text
			if text == "" {
				text = "(no readable text)"
			}
			sections[i] = strings.Join(header, "\n") + "\n\n" + text
		}
		metaPages[i] = p.metadata()
	}

	meta := ordered.NewObject()
	meta.Set("pages", metaPages)
	content := strings.Join(sections, "\n\n---\n\n")
	out := toolhub.Text(content)
	if failed == len(pages) {
		out = toolhub.Error(content)
		// Blocked URLs fail the same way every time: retrying cannot help.
		if blocked == failed {
			out = out.NotRetryable()
		}
	}
	out.Metadata = meta
	return out, nil
}

// metadata is the metadata.pages entry: url, ok, finalUrl, title, truncated, error,
// with every undefined value left out.
func (p pageResult) metadata() ordered.Object {
	m := ordered.NewObject()
	m.Set("url", p.url)
	m.Set("ok", p.ok)
	if p.finalURL != "" {
		m.Set("finalUrl", p.finalURL)
	}
	if p.title != "" {
		m.Set("title", p.title)
	}
	if p.ok {
		m.Set("truncated", p.truncated)
	}
	if !p.ok {
		m.Set("error", p.err)
	}
	return m
}

// cleanURLs is the url list rule: the string elements, each trimmed with the JS trim,
// the empty ones dropped and the duplicates removed keeping the first. It never returns
// more than it was given.
func cleanURLs(raw []string) []string {
	out := make([]string, 0, len(raw))
	seen := make(map[string]struct{}, len(raw))
	for _, u := range raw {
		u = jsstr.Trim(u)
		if _, dup := seen[u]; u == "" || dup {
			continue
		}
		seen[u] = struct{}{}
		out = append(out, u)
	}
	return out
}

// extractOne fetches and reads one page. Every failure becomes the page's
// error text; nothing is returned as a Go error.
func (t *ExtractTool) extractOne(ctx context.Context, rawURL string, maxChars int) pageResult {
	fetch, err := fetchPage(ctx, rawURL, fetchOptions{
		Transport:    t.transport,
		Resolver:     t.resolver,
		AllowPrivate: t.allowPrivate,
		MaxRedirects: t.maxRedirects,
		MaxBytes:     t.maxBytes,
		UserAgent:    t.userAgent,
		Timeout:      t.timeout,
	})
	if err != nil {
		var be *blockedError
		if errors.As(err, &be) {
			return pageResult{url: rawURL, blocked: true, err: "blocked: " + be.Error()}
		}
		res := pageResult{url: rawURL, err: err.Error()}
		// An HTTP error names the page it came from; every other failure has no final URL.
		if fetch.Status != 0 && (fetch.Status < 200 || fetch.Status > 299) {
			res.finalURL = fetch.FinalURL
		}
		return res
	}

	contentType := strings.ToLower(fetch.ContentType)
	mime, _, _ := strings.Cut(contentType, ";")
	mime = jsstr.Trim(mime)
	if mime != "" && !slices.Contains(extractTextContentTypes, mime) && !strings.HasPrefix(mime, "text/") {
		return pageResult{url: rawURL, finalURL: fetch.FinalURL, err: `unsupported content type "` + mime + `"`}
	}

	body := decodeBody(fetch.Body, contentType)
	isHTML := mime == "text/html" || mime == "application/xhtml+xml" || (mime == "" && looksLikeHTML(body))
	var text, title string
	if isHTML {
		text = htmlToText(mainContent(body))
		title, _ = extractTitle(body)
	} else {
		text = jsstr.Trim(body)
	}
	cut, truncated := truncate(text, maxChars)
	return pageResult{url: rawURL, ok: true, finalURL: fetch.FinalURL, title: title, text: cut, truncated: truncated}
}

// looksLikeHTML is /<html[\s>]/i on the first 2 000 UTF-16 units of the body: an <html
// tag (any case) followed by white space or a closing angle bracket.
func looksLikeHTML(body string) bool {
	head := asciiLower(jsstr.TruncateUTF16(body, extractHTMLSniffUnits))
	for from := 0; ; {
		i := strings.Index(head[from:], "<html")
		if i < 0 {
			return false
		}
		p := from + i + len("<html")
		if p < len(head) {
			r, _ := utf8.DecodeRuneInString(head[p:])
			if r == '>' || jsstr.IsSpace(r) {
				return true
			}
		}
		from += i + 1
	}
}
