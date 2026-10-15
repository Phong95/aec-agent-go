package web

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"time"
)

// Defaults of the fetch path, the web_extract defaults.
const (
	fetchDefaultTimeout   = 15 * time.Second
	fetchDefaultMaxBytes  = 2_000_000
	fetchDefaultUserAgent = "AECAgent/1.0 (+web_extract)"
	fetchAccept           = "text/html,application/xhtml+xml,text/plain,application/json;q=0.9,*/*;q=0.5"
)

// fetchOptions configure fetchPage. A zero Timeout, MaxBytes or empty UserAgent means the
// default; a negative MaxRedirects is read as zero (follow no redirect).
type fetchOptions struct {
	// Transport carries every request. Nil means the hardened default of newTransport
	// (dial-time check unless AllowPrivate). Tests inject a route table.
	Transport http.RoundTripper
	// Resolver is the lookup of the pre-check. Nil means net.DefaultResolver.
	Resolver Resolver
	// AllowPrivate (allowPrivateNetwork) disables the address checks (the
	// pre-check and the default transport's dial-time check). The scheme and credential
	// checks still apply.
	AllowPrivate bool
	// MaxRedirects is the number of redirect hops followed, each one re-validated.
	MaxRedirects int
	// MaxBytes is how many body bytes are read before the body is cut.
	MaxBytes int64
	// UserAgent is the User-Agent header.
	UserAgent string
	// Timeout covers every hop and the body of the page.
	Timeout time.Duration
}

// fetched is one fetched page: the URL the caller asked for, the final URL after the
// redirects, the status and Content-Type of the final response and its body (at most
// MaxBytes). A failed fetch returns the fields known so far next to the error (a non-2xx
// status still has the final URL and the status).
type fetched struct {
	URL         string
	FinalURL    string
	Status      int
	ContentType string
	Body        []byte
}

// pageError is a fetch failure that is not a blocked URL: the page error text
// (`HTTP 404`, `too many redirects (max 5)`, `timed out after 15000 ms`, a network error).
// cause is the underlying error when there is one (a cancelled run keeps context.Canceled).
type pageError struct {
	msg   string
	cause error
}

func (e *pageError) Error() string { return e.msg }
func (e *pageError) Unwrap() error { return e.cause }

// fetchPage fetches one page the way web_extract does:
// the URL and every redirect hop pass assertPublicHTTPURL before any request is made,
// redirects are followed by hand (the client never follows one, so no credentials or
// private target can slip through a hop), each Location is resolved WHATWG-style against the
// URL it came from, and the body is read through a LimitReader.
//
// One deadline, derived from the caller's context, covers every hop and the body. When it
// fires the error is `timed out after <ms> ms`; when the caller's context ends first the
// error is `cancelled`. The errors are a *blockedError for a URL that must not be
// fetched (including a dial the transport refused) and a *pageError for everything else.
func fetchPage(parent context.Context, rawURL string, o fetchOptions) (fetched, error) {
	if o.Timeout <= 0 {
		o.Timeout = fetchDefaultTimeout
	}
	if o.MaxBytes <= 0 {
		o.MaxBytes = fetchDefaultMaxBytes
	}
	if o.UserAgent == "" {
		o.UserAgent = fetchDefaultUserAgent
	}
	if o.MaxRedirects < 0 {
		o.MaxRedirects = 0
	}
	ctx, cancel := context.WithTimeout(parent, o.Timeout)
	defer cancel()

	rt := o.Transport
	if rt == nil {
		tr := newTransport(o.AllowPrivate)
		defer tr.CloseIdleConnections()
		rt = tr
	}
	// The requests go straight to the round tripper, not through an http.Client: a Client
	// follows redirects itself (http.ErrUseLastResponse only switches that off after the
	// fact) and refuses, before it asks, a Location header that net/url cannot parse, which
	// would turn a Location the WHATWG parser accepts (a bad percent escape, say) into a
	// failed page. Here no redirect is ever followed by the HTTP stack: the loop below runs
	// every hop, and the context deadline is the page deadline.
	guard := guardOptions{Resolver: o.Resolver, AllowPrivateNetwork: o.AllowPrivate}

	out := fetched{URL: rawURL}
	next := rawURL
	for hop := 0; ; hop++ {
		u, err := assertPublicHTTPURL(ctx, next, nil, guard)
		if err != nil {
			if cerr := o.contextError(parent, ctx); cerr != nil {
				return out, cerr
			}
			return out, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			return out, &pageError{msg: errorText(err), cause: err}
		}
		req.Header.Set("User-Agent", o.UserAgent)
		req.Header.Set("Accept", fetchAccept)
		// The URL was validated by assertPublicHTTPURL and the transport re-validates the
		// dialed address, so this request cannot reach a private target.
		resp, err := rt.RoundTrip(req) //nolint:gosec // url validated by assertPublicHTTPURL and the dial-time check
		if err != nil {
			return out, o.requestError(parent, ctx, err, u.hostname())
		}

		location := resp.Header.Get("Location")
		if resp.StatusCode >= 300 && resp.StatusCode < 400 && location != "" {
			_ = resp.Body.Close()
			resolved, rerr := resolveLocation(location, u)
			if rerr != nil {
				return out, rerr
			}
			if hop >= o.MaxRedirects {
				return out, &pageError{msg: "too many redirects (max " + strconv.Itoa(o.MaxRedirects) + ")"}
			}
			next = resolved
			continue
		}

		out.FinalURL = u.String()
		out.Status = resp.StatusCode
		out.ContentType = resp.Header.Get("Content-Type")
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			_ = resp.Body.Close()
			return out, &pageError{msg: "HTTP " + strconv.Itoa(resp.StatusCode)}
		}
		body, rerr := io.ReadAll(io.LimitReader(resp.Body, o.MaxBytes))
		_ = resp.Body.Close()
		if rerr != nil {
			return out, o.requestError(parent, ctx, rerr, u.hostname())
		}
		out.Body = body
		return out, nil
	}
}

// resolveLocation resolves a Location header against the URL it came from (WHATWG
// `new URL(location, url).toString()`, which runs before the guard). A Location that does
// not parse is the generic page error `Invalid URL`, not a blocked page. A syntactically
// valid scheme other than http(s) is handed on as it is, so the guard refuses it with the
// scheme error.
func resolveLocation(location string, from *webURL) (string, error) {
	if sc := schemeOf(location); sc != "" && sc != "http" && sc != "https" {
		return location, nil
	}
	nu, err := parseURL(location, from)
	if err != nil {
		return "", &pageError{msg: "Invalid URL", cause: err}
	}
	return nu.String(), nil
}

// contextError reports the page deadline or the caller's cancellation, or nil when the
// context is still live.
func (o fetchOptions) contextError(parent, ctx context.Context) error {
	if parent.Err() != nil {
		return &pageError{msg: "cancelled", cause: parent.Err()}
	}
	if ctx.Err() != nil {
		return &pageError{msg: "timed out after " + strconv.FormatInt(o.Timeout.Milliseconds(), 10) + " ms", cause: context.DeadlineExceeded}
	}
	return nil
}

// requestError maps a failure of the request or the body read: a dial the transport
// refused is a blocked page, a context that ended is the cancellation or the timeout, and
// anything else is a page error with the text of the network error (without the URL that
// net/http puts in front of it). host is the request's host: when it is a name, a refused
// dial names the host only, never the address it resolved to (the DNS answer is internal
// detail, review IN-A1); a literal address is the caller's own input and is named.
func (o fetchOptions) requestError(parent, ctx context.Context, err error, host string) error {
	var blocked *dialBlockedError
	if errors.As(err, &blocked) {
		if _, perr := netip.ParseAddr(host); perr != nil && host != "" {
			return &blockedError{msg: `host "` + host + `" resolves to a non-public address`}
		}
		return &blockedError{msg: "address " + blocked.addr + " is not a public address"}
	}
	if cerr := o.contextError(parent, ctx); cerr != nil {
		return cerr
	}
	return &pageError{msg: errorText(err), cause: err}
}

// errorText is the message of err without the `Get "<url>":` prefix of a *url.Error. A DNS
// error is reduced to its Node-style cause (dnsFailureText) so the local resolver address
// never reaches the model.
func errorText(err error) string {
	if t, ok := dnsFailureText(err, ""); ok {
		return t
	}
	var ue *url.Error
	if errors.As(err, &ue) && ue.Err != nil {
		return ue.Err.Error()
	}
	return err.Error()
}
