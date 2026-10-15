package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Phong95/aec-agent-go/components/secrethub/secret"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/internal/jsstr"
	"github.com/Phong95/aec-agent-go/internal/nilcheck"
	"github.com/Phong95/aec-agent-go/internal/toolkit"
	"github.com/Phong95/aec-agent-go/kernel"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// The defaults of http_request.
const (
	httpDefaultTimeout      = 30 * time.Second
	httpDefaultMaxBytes     = 2_000_000
	httpDefaultMaxChars     = 20_000
	httpDefaultMaxBodyBytes = 1_000_000
	httpDefaultMaxRedirects = 5
	httpDefaultUserAgent    = "aec-agent/1.0 (+http_request)"
	httpMaxHeaders          = 32
)

// httpMethods are the methods http_request offers, in schema order.
var httpMethods = []string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE"}

// httpShownHeaders are the response headers worth showing the model.
var httpShownHeaders = []string{"content-type", "content-length", "location", "retry-after", "x-ratelimit-remaining", "x-request-id"}

// headerNameRe is an HTTP header name (an RFC 9110 token).
var headerNameRe = regexp.MustCompile("^[A-Za-z0-9!#$%&'*+.^_`|~-]+$")

// HTTPRequestTool is http_request (A-101): it calls an HTTP API, with the agent's secrets
// put in where the model wrote {{secret:NAME}} (url, header values, body). Each secret must
// allow http_request and the request's host, and travels over https only; a redirect to
// another host is not followed once a secret is in the request. Every URL and redirect hop
// passes the SSRF guard (the one of web_extract). The response is cut to a size the model
// can read, and the harness redacts any secret value echoed back.
//
// Risk per call (toolhub.CallRisker): GET and HEAD are read-only (no approval); POST, PUT,
// PATCH and DELETE can change something at the API, so they wait for the user's approval.
// A read-only subagent never gets the tool (its definition's risk is the worst case,
// consequential).
//
// An HTTPRequestTool is safe for concurrent use. Build one with NewHTTPRequestTool.
type HTTPRequestTool struct {
	timeout      time.Duration
	maxBytes     int64
	maxChars     int
	maxBodyBytes int
	maxRedirects int
	userAgent    string
	allowPrivate bool
	resolver     Resolver
	transport    http.RoundTripper
	// own is the transport the tool built itself (nil when the host injected one).
	own        *http.Transport
	definition toolhub.Definition
}

var (
	_ toolhub.Handler    = (*HTTPRequestTool)(nil)
	_ toolhub.CallRisker = (*HTTPRequestTool)(nil)
)

// NewHTTPRequestTool builds the tool with the defaults for every zero option.
func NewHTTPRequestTool(o HTTPRequestToolOptions) *HTTPRequestTool {
	t := &HTTPRequestTool{
		timeout:      httpDefaultTimeout,
		maxBytes:     httpDefaultMaxBytes,
		maxChars:     httpDefaultMaxChars,
		maxBodyBytes: httpDefaultMaxBodyBytes,
		maxRedirects: httpDefaultMaxRedirects,
		userAgent:    httpDefaultUserAgent,
		allowPrivate: o.AllowPrivateNetwork,
		resolver:     o.Resolver,
		transport:    o.Transport,
	}
	if o.TimeoutMs > 0 {
		t.timeout = time.Duration(o.TimeoutMs) * time.Millisecond
	}
	if o.MaxBytes > 0 {
		t.maxBytes = int64(o.MaxBytes)
	}
	if o.MaxChars > 0 {
		t.maxChars = o.MaxChars
	}
	if o.MaxBodyBytes > 0 {
		t.maxBodyBytes = o.MaxBodyBytes
	}
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
	str := func(desc string) toolhub.JSONSchema { return toolhub.JSONSchema{Type: "string", Description: desc} }
	t.definition = toolhub.Definition{
		Name: "http_request",
		Description: "Call an HTTP API and get the status, main headers and body. To use one of your secrets (see <available_secrets>), write " +
			"{{secret:NAME}} where the value goes " + emDash + " e.g. header Authorization: Bearer {{secret:NAME}} " + emDash + " you never see the value. " +
			"GET/HEAD run right away; POST, PUT, PATCH and DELETE ask the user first. Public https/http URLs only; " +
			"long bodies are cut to " + strconv.Itoa(t.maxChars) + " characters.",
		Parameters: toolhub.JSONSchema{
			Type: "object",
			Properties: toolhub.NewProperties(
				toolhub.Property{Name: "method", Schema: toolhub.JSONSchema{Type: "string", Enum: slices.Clone(httpMethods), Description: "HTTP method (default GET)"}},
				toolhub.Property{Name: "url", Schema: str("Full URL, e.g. https://api.example.com/v1/items?limit=10")},
				toolhub.Property{Name: "headers", Schema: toolhub.JSONSchema{Type: "object", Description: `Request headers as {"Name": "value"}`}},
				toolhub.Property{Name: "body", Schema: str("Raw request body (set Content-Type in headers)")},
				toolhub.Property{Name: "json", Schema: toolhub.JSONSchema{Type: "object", Description: "JSON body " + emDash + " sent with Content-Type: application/json (instead of body)"}},
			),
			Required: []string{"url"},
		},
		Risk: toolhub.RiskConsequential,
	}
	return t
}

// Name returns the plugin name.
func (*HTTPRequestTool) Name() string { return "tool:http-request" }

// Version returns the plugin version.
func (*HTTPRequestTool) Version() string { return "1.0.0" }

// Definition returns the tool definition (a fresh value on every call).
func (t *HTTPRequestTool) Definition() toolhub.Definition { return t.definition.Clone() }

// Dispose closes the idle connections of the transport the tool built; an injected
// Transport is the host's and is left alone. It is safe to call more than once.
func (t *HTTPRequestTool) Dispose(context.Context) error {
	if t.own != nil {
		t.own.CloseIdleConnections()
	}
	return nil
}

// methodOf is the call's method, upper-cased; GET when absent.
func methodOf(a toolkit.Args) string {
	if m, ok := a.String("method"); ok {
		return strings.ToUpper(m)
	}
	return http.MethodGet
}

// RiskFor is the risk of one call: read-only for GET and HEAD, consequential otherwise.
func (*HTTPRequestTool) RiskFor(args kernel.Arguments) toolhub.Risk {
	if m := methodOf(toolkit.DecodeArgs(args)); m == http.MethodGet || m == http.MethodHead {
		return toolhub.RiskReadOnly
	}
	return toolhub.RiskConsequential
}

// httpHeader is one request header, in the order the model wrote it.
type httpHeader struct{ name, value string }

// Execute makes the request.
func (t *HTTPRequestTool) Execute(ctx context.Context, args kernel.Arguments, tc toolhub.Context) (toolhub.Output, error) {
	fail := func(message string) toolhub.Output { return toolkit.Fail("http_request: " + message) }
	a := toolkit.DecodeArgs(args)
	method := methodOf(a)
	if !slices.Contains(httpMethods, method) {
		return fail("method must be one of " + strings.Join(httpMethods, ", ")), nil
	}
	rawURL, _ := a.String("url")
	rawURL = jsstr.Trim(rawURL)
	if rawURL == "" {
		return fail(`"url" must be a URL`), nil
	}

	var headers []httpHeader
	if raw, ok := a.Raw("headers"); ok && string(raw) != "null" {
		var obj ordered.Object
		if !isJSONObject(raw) || obj.UnmarshalJSON(raw) != nil {
			return fail(`"headers" must be an object of strings`), nil
		}
		if obj.Len() > httpMaxHeaders {
			return fail("at most " + strconv.Itoa(httpMaxHeaders) + " headers"), nil
		}
		for k, v := range obj.All() {
			var value string
			switch x := v.(type) {
			case string:
				value = x
			case json.Number:
				value = jsNumberText(x)
			default:
				return fail(`header "` + k + `" must be a string`), nil
			}
			if !headerNameRe.MatchString(k) {
				return fail(`"` + k + `" is not a header name`), nil
			}
			headers = append(headers, httpHeader{k, value})
		}
	}
	var body *string
	if raw, ok := a.Raw("json"); ok && string(raw) != "null" {
		var buf bytes.Buffer
		if err := json.Compact(&buf, raw); err != nil {
			return fail(`"json" must be JSON`), nil
		}
		b := buf.String()
		body = &b
		if !slices.ContainsFunc(headers, func(h httpHeader) bool { return strings.EqualFold(h.name, "content-type") }) {
			headers = append(headers, httpHeader{"Content-Type", "application/json"})
		}
	} else if b, ok := a.String("body"); ok {
		body = &b
	}
	if body != nil && (method == http.MethodGet || method == http.MethodHead) {
		return fail("a " + method + " request has no body"), nil
	}
	if body != nil && len(*body) > t.maxBodyBytes {
		return fail("the body is larger than " + strconv.Itoa(t.maxBodyBytes) + " bytes"), nil
	}

	// Secrets: every {{secret:NAME}} in the url, the header values and the body.
	texts := []string{rawURL}
	for _, h := range headers {
		texts = append(texts, h.value)
	}
	if body != nil {
		texts = append(texts, *body)
	} else {
		texts = append(texts, "")
	}
	names := secret.PlaceholderNames(strings.Join(texts, "\n"))
	// The host is decided before any value is put in, and a placeholder may not be part of it.
	probe, err := parseURL(secret.Placeholder.ReplaceAllString(rawURL, "x"), nil)
	if err != nil {
		return fail("invalid URL: " + rawURL), nil
	}
	if other, err := parseURL(secret.Placeholder.ReplaceAllString(rawURL, "y"), nil); err != nil || other.hostname() != probe.hostname() {
		return fail("a secret can't be part of the host name"), nil
	}
	values := map[string]string{}
	if len(names) > 0 {
		if probe.Scheme != "https" {
			return fail("secrets are only sent over https"), nil
		}
		if nilcheck.IsNil(tc.Secrets) {
			return fail("this agent has no secret store"), nil
		}
		values, err = tc.Secrets.Resolve(ctx, names, secret.Use{Tool: "http_request", Host: probe.hostname()}, secret.ResolveContext{SessionID: tc.SessionID})
		if err != nil {
			var se *secret.Error
			if errors.As(err, &se) {
				return fail(se.Message), nil
			}
			return fail(err.Error()), nil
		}
	}
	target, err := parseURL(secret.FillPlaceholders(rawURL, values), nil)
	if err != nil {
		return fail("invalid URL: " + rawURL), nil
	}
	// A value that changes the host once filled in (e.g. contains "@" or "/") is refused too.
	if target.hostname() != probe.hostname() {
		return fail("a secret can't change the URL's host"), nil
	}
	for i := range headers {
		headers[i].value = secret.FillPlaceholders(headers[i].value, values)
	}
	if body != nil {
		filled := secret.FillPlaceholders(*body, values)
		body = &filled
	}
	return t.send(ctx, method, target, headers, body, len(names) > 0), nil
}

// send makes the request and follows redirects by hand, each hop re-validated by the SSRF
// guard. Once secrets are in the request, a redirect to another host (or off https) is not
// followed: the redirect response itself is returned.
func (t *HTTPRequestTool) send(parent context.Context, method string, target *webURL, headers []httpHeader, body *string, carriesSecrets bool) toolhub.Output {
	fail := func(message string, retryable bool) toolhub.Output {
		return toolkit.Failure("http_request: "+message, retryable, ordered.Object{})
	}
	ctx, cancel := context.WithTimeout(parent, t.timeout)
	defer cancel()
	fo := fetchOptions{Timeout: t.timeout}
	guard := guardOptions{Resolver: t.resolver, AllowPrivateNetwork: t.allowPrivate}
	mapErr := func(err error) toolhub.Output {
		var blocked *blockedError
		if errors.As(err, &blocked) {
			return fail("blocked: "+blocked.msg, false)
		}
		var pe *pageError
		if errors.As(err, &pe) {
			return fail(pe.msg, pe.msg != "cancelled")
		}
		return fail(err.Error(), true)
	}

	current := target
	currentMethod := method
	currentBody := body
	for hop := 0; ; hop++ {
		checked, err := assertPublicHTTPURL(ctx, current.String(), nil, guard)
		if err != nil {
			if cerr := fo.contextError(parent, ctx); cerr != nil {
				return mapErr(cerr)
			}
			return mapErr(err)
		}
		var reader io.Reader
		if currentBody != nil {
			reader = strings.NewReader(*currentBody)
		}
		req, err := http.NewRequestWithContext(ctx, currentMethod, checked.String(), reader)
		if err != nil {
			return fail(errorText(err), false)
		}
		req.Header.Set("User-Agent", t.userAgent)
		for _, h := range headers {
			req.Header.Set(h.name, h.value)
		}
		// The URL was validated by assertPublicHTTPURL and the transport re-validates the
		// dialed address, so this request cannot reach a private target.
		resp, err := t.transport.RoundTrip(req) //nolint:gosec // url validated by assertPublicHTTPURL and the dial-time check
		if err != nil {
			return mapErr(fo.requestError(parent, ctx, err, checked.hostname()))
		}
		location := resp.Header.Get("Location")
		if resp.StatusCode < 300 || resp.StatusCode >= 400 || location == "" {
			return t.format(parent, ctx, fo, currentMethod, checked, resp)
		}
		next, err := parseURL(location, checked)
		if err != nil || (carriesSecrets && (next.hostname() != target.hostname() || next.Scheme != "https")) {
			// Not followed: the request carries secrets that may go to this host only (or
			// the Location can't be read).
			return t.format(parent, ctx, fo, currentMethod, checked, resp)
		}
		_ = resp.Body.Close()
		if hop >= t.maxRedirects {
			return fail("too many redirects (max "+strconv.Itoa(t.maxRedirects)+")", false)
		}
		if resp.StatusCode == http.StatusSeeOther || ((resp.StatusCode == http.StatusMovedPermanently || resp.StatusCode == http.StatusFound) && currentMethod == http.MethodPost) {
			currentMethod = http.MethodGet
			currentBody = nil
		}
		current = next
	}
}

// format is the response as the model sees it: the request line and status, the main
// headers, and the body cut to MaxBytes bytes and MaxChars characters. An HTTP error
// status is an error result, retryable for 5xx and 429.
func (t *HTTPRequestTool) format(parent, ctx context.Context, fo fetchOptions, method string, u *webURL, resp *http.Response) toolhub.Output {
	defer func() { _ = resp.Body.Close() }()
	origin := u.Scheme + "://" + u.Host
	if u.Port != "" {
		origin += ":" + u.Port
	}
	statusText := strings.TrimSpace(strings.TrimPrefix(resp.Status, strconv.Itoa(resp.StatusCode)))
	head := method + " " + origin + u.Path + " → HTTP " + strconv.Itoa(resp.StatusCode)
	if statusText != "" {
		head += " " + statusText
	}
	lines := []string{head}
	for _, h := range httpShownHeaders {
		if v := resp.Header.Get(h); v != "" {
			lines = append(lines, h+": "+v)
		}
	}
	text, cut := "", false
	if method != http.MethodHead && resp.Body != nil {
		raw, err := io.ReadAll(io.LimitReader(resp.Body, t.maxBytes))
		if err != nil {
			var pe *pageError
			if errors.As(fo.requestError(parent, ctx, err, u.hostname()), &pe) && pe.msg == "cancelled" {
				return toolkit.Fail("http_request: cancelled")
			}
		}
		cut = int64(len(raw)) >= t.maxBytes
		text = jsstr.DecodeUTF8(raw)
		if kernel.UTF16Len(text) > t.maxChars {
			text = jsstr.TruncateUTF16(text, t.maxChars)
			cut = true
		}
	}
	if text != "" {
		lines = append(lines, "", text)
	}
	if cut {
		lines = append(lines, "", "[response cut to "+strconv.Itoa(t.maxChars)+" characters]")
	}
	out := toolhub.Text(strings.Join(lines, "\n"))
	if resp.StatusCode >= 400 {
		isErr, retryable := true, resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests
		out.IsError, out.Retryable = &isErr, &retryable
	}
	return out
}

// isJSONObject reports whether raw JSON is an object.
func isJSONObject(raw []byte) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && trimmed[0] == '{'
}

// jsNumberText is String(n) of a JSON number for the common cases (integers and plain
// decimals); anything strconv can't read is kept as written.
func jsNumberText(n json.Number) string {
	f, err := strconv.ParseFloat(string(n), 64)
	if err != nil {
		return string(n)
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
}
