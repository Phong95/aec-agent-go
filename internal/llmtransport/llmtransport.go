// Package llmtransport is the HTTP hardening the provider leaves share (Phase 8
// review fixes WR-01 to WR-03). The Anthropic, OpenAI-compatible and Bedrock
// providers hand their requests to a vendor SDK, which brings its own client; this
// package gives that client the three properties the ChatGPT-subscription provider
// (a raw net/http client) has always had:
//
//   - a redirect to another scheme or host drops the credential headers, because
//     net/http alone removes only Authorization, Cookie and WWW-Authenticate and
//     forwards a header such as X-Api-Key (CheckRedirect);
//   - no response is read without a bound: an error body is cut at MaxErrorBody and
//     any other body that is neither an event stream nor the answer to a streaming
//     request (StreamContext) fails at MaxResponseBody, so a hostile or broken
//     BaseURL cannot exhaust memory (NewClient, BoundBodies);
//   - a request that gets no response headers within a time is abandoned (10
//     minutes by default), and the caller's context still wins (Config);
//   - a stream that stays silent for longer than Config.IdleTimeout between two
//     reads is abandoned with an *IdleTimeoutError (opt-in per provider).
//
// It also holds the error helpers the leaves share: TruncateMessage bounds the
// vendor text put into a kernel.ModelError, IsTransient tells a network failure
// (worth a retry) from everything else, and RetryAfter reads the wait an error
// response asks for (kernel.ModelError.RetryAfter).
//
// It imports only the standard library, and no environment, process or file
// system package (the architecture guard checks it like the llm tree).
package llmtransport

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

const (
	// MaxErrorBody is how much of an error response body is read: a megabyte is far
	// more than any real error, and the text becomes an error message. It is the cap
	// of the subscription provider.
	MaxErrorBody = 1 << 20
	// MaxResponseBody is the largest body of a successful response that is not an
	// event stream (a model list, a non-streaming completion). It is the model list
	// cap of the subscription provider. A larger body fails the read with
	// ErrBodyTooLarge.
	MaxResponseBody = 32 << 20
	// DefaultRequestTimeout is how long a request may wait for the response headers:
	// the 10 minutes the official Anthropic and OpenAI SDKs use.
	DefaultRequestTimeout = 10 * time.Minute
	// MaxRedirects is the redirect limit of net/http's default policy.
	MaxRedirects = 10
	// MaxModelPages and MaxModels bound a model listing that the server keeps
	// answering with another page (has_more never clears). A real catalogue is a
	// page or two of at most a few hundred entries.
	MaxModelPages = 100
	MaxModels     = 10_000
	// MaxErrorMessage is the longest vendor text (in bytes) a provider puts into a
	// kernel.ModelError message: an error body can be up to MaxErrorBody, and a
	// megabyte of HTML in a log line or a UI toast helps nobody.
	MaxErrorMessage = 2 << 10
	// DefaultStreamIdleTimeout is the idle timeout a provider that opts into one uses
	// by default: the longest silence between two reads of a streaming body.
	DefaultStreamIdleTimeout = 5 * time.Minute
)

// TruncateMessage cuts s to at most MaxErrorMessage bytes, on a rune boundary, and
// marks the cut with an ellipsis. A shorter s is returned unchanged.
func TruncateMessage(s string) string {
	if len(s) <= MaxErrorMessage {
		return s
	}
	const ellipsis = "\u2026"
	cut := MaxErrorMessage - len(ellipsis)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + ellipsis
}

// IsTransient reports whether err is a network failure that a retry may cure: a
// net.Error (a refused or reset connection, a DNS failure, a dial or read timeout;
// *url.Error is one), a body cut before its end (io.ErrUnexpectedEOF), or a
// deadline of the transport (TimeoutError, IdleTimeoutError, which match
// context.DeadlineExceeded). The caller checks its own context first: an abort or a
// deadline of the caller is never a model failure.
func IsTransient(err error) bool {
	if err == nil {
		return false
	}
	var netErr net.Error
	return errors.As(err, &netErr) ||
		errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, context.DeadlineExceeded)
}

// RetryAfter is the wait an error response asks for, read from its headers at the
// current time (ParseRetryAfter). It is 0 for a nil header.
func RetryAfter(h http.Header) time.Duration { return ParseRetryAfter(h, time.Now()) }

// ParseRetryAfter reads the wait an error response asks for, as the vendor SDKs do:
// the non-standard retry-after-ms header (milliseconds, OpenAI and Anthropic send
// it) wins, else Retry-After as a number of seconds or as an HTTP-date, which is
// measured from now. A missing, malformed, negative or past value is 0.
func ParseRetryAfter(h http.Header, now time.Time) time.Duration {
	if h == nil {
		return 0
	}
	if ms := strings.TrimSpace(h.Get("Retry-After-Ms")); ms != "" {
		if v, err := strconv.ParseFloat(ms, 64); err == nil && v > 0 && v < maxRetryAfterMs {
			return time.Duration(v * float64(time.Millisecond))
		}
	}
	ra := strings.TrimSpace(h.Get("Retry-After"))
	if ra == "" {
		return 0
	}
	if v, err := strconv.ParseFloat(ra, 64); err == nil {
		if v > 0 && v < maxRetryAfterMs/1000 {
			return time.Duration(v * float64(time.Second))
		}
		return 0
	}
	if at, err := http.ParseTime(ra); err == nil {
		if d := at.Sub(now); d > 0 {
			return d
		}
	}
	return 0
}

// maxRetryAfterMs bounds a numeric Retry-After so the conversion to a Duration
// cannot overflow: a year, far beyond any real advice.
const maxRetryAfterMs = 365 * 24 * 3600 * 1000.0

// streamKey marks a request context as the one of a streaming request.
type streamKey struct{}

// StreamContext marks ctx as the context of a streaming request. The body of a 2xx
// answer to such a request is read without the MaxResponseBody cap whatever its
// Content-Type says: an OpenAI-compatible gateway can label a stream text/plain,
// application/json or application/x-ndjson, and a stream that lives as long as the
// server writes would die at the cap (review IN-201). The SDK decoders still cap one
// event, and an error answer (status 300 and up) is cut at MaxErrorBody as always.
// The official vendor SDKs cap nothing, so this is the cap that is closest to them
// where a stream is concerned.
func StreamContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, streamKey{}, true)
}

// isStreamRequest reports whether req was made with a StreamContext.
func isStreamRequest(req *http.Request) bool {
	if req == nil {
		return false
	}
	on, _ := req.Context().Value(streamKey{}).(bool)
	return on
}

// ErrBodyTooLarge is the error a read of a response body returns when the body is
// larger than MaxResponseBody.
var ErrBodyTooLarge = errors.New("the response body is larger than the limit of the provider")

// TimeoutError is the error of a request that got no response headers within the
// request timeout. It matches context.DeadlineExceeded with errors.Is and is a
// net.Error with Timeout() true, so a provider maps it to its "Request timed out."
// text as it maps a deadline.
type TimeoutError struct {
	// After is the timeout that expired.
	After time.Duration
}

// Error implements error. The text carries no URL and no header.
func (e *TimeoutError) Error() string {
	return "no response headers within " + e.After.String()
}

// Timeout reports true, as a net.Error.
func (*TimeoutError) Timeout() bool { return true }

// Temporary reports true, as a net.Error.
func (*TimeoutError) Temporary() bool { return true }

// Is makes the error match context.DeadlineExceeded.
func (*TimeoutError) Is(target error) bool { return target == context.DeadlineExceeded }

// IdleTimeoutError is the read error of a streaming body that delivered no byte
// within the idle timeout. Like TimeoutError it matches context.DeadlineExceeded
// with errors.Is and is a net.Error with Timeout() true.
type IdleTimeoutError struct {
	// After is the idle timeout that expired.
	After time.Duration
}

// Error implements error. The text carries no URL and no header.
func (e *IdleTimeoutError) Error() string {
	return "the stream sent no data within " + e.After.String()
}

// Timeout reports true, as a net.Error.
func (*IdleTimeoutError) Timeout() bool { return true }

// Temporary reports true, as a net.Error.
func (*IdleTimeoutError) Temporary() bool { return true }

// Is makes the error match context.DeadlineExceeded.
func (*IdleTimeoutError) Is(target error) bool { return target == context.DeadlineExceeded }

// Config chooses what NewClient adds to a client.
type Config struct {
	// CredentialHeaders are the headers that carry a credential. A redirect to
	// another scheme or host removes them.
	CredentialHeaders []string
	// RequestTimeout is how long a request may wait for the response headers. Zero
	// means no timeout (the caller's context alone governs the request).
	RequestTimeout time.Duration
	// IdleTimeout is the longest silence between two reads of the 2xx body of a
	// streaming request (StreamContext). When it expires the request is cancelled
	// and the pending read fails with an *IdleTimeoutError. Zero means no idle
	// timeout. It never applies to a request that is not a stream.
	IdleTimeout time.Duration
}

// NewClient returns the client a provider hands to its SDK. With a nil base it
// builds one from a clone of the default transport; with a base (a test's
// loopback client, or an operator's client) it works on a copy, so base itself is
// never changed, and keeps the base's transport and Timeout. The result has the
// redirect policy of CheckRedirect (a CheckRedirect of base still runs after it)
// and a transport that bounds the response bodies and applies cfg.RequestTimeout.
func NewClient(base *http.Client, cfg Config) *http.Client {
	var c http.Client
	var rt http.RoundTripper
	if base != nil {
		c = *base
		rt = base.Transport
	}
	if rt == nil {
		rt = newTransport()
	}
	c.Transport = &guard{next: rt, timeout: cfg.RequestTimeout, idle: cfg.IdleTimeout}
	c.CheckRedirect = CheckRedirect(c.CheckRedirect, cfg.CredentialHeaders...)
	return &c
}

// newTransport is a clone of the default transport. It sets no timeout of its own.
func newTransport() http.RoundTripper {
	if base, ok := http.DefaultTransport.(*http.Transport); ok {
		return base.Clone()
	}
	return &http.Transport{Proxy: http.ProxyFromEnvironment}
}

// CheckRedirect follows redirects as net/http does by default (at most ten) but
// removes every header of credentials when the target has another scheme or host
// than the first request: a redirect can come from anyone who answers the request,
// and the credentials are for the configured endpoint only. The request body of a
// 307 or 308 still follows, as it does with fetch. prev is the policy of a client
// given by the caller and runs after.
func CheckRedirect(prev func(*http.Request, []*http.Request) error, credentialHeaders ...string) func(*http.Request, []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if len(via) >= MaxRedirects {
			return errors.New("stopped after 10 redirects")
		}
		first := via[0]
		if req.URL.Scheme != first.URL.Scheme || req.URL.Host != first.URL.Host {
			for _, h := range credentialHeaders {
				req.Header.Del(h)
			}
		}
		if prev != nil {
			return prev(req, via)
		}
		return nil
	}
}

// Doer is what an AWS client calls: the Do method of an HTTP client.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// BoundBodies wraps next so that the bodies it returns are bounded as NewClient
// bounds them (an error body at MaxErrorBody, any other body that is neither an
// event stream nor the answer to a StreamContext request at MaxResponseBody). It adds no timeout and no redirect policy: the AWS
// default client has its own redirect rules.
func BoundBodies(next Doer) Doer { return bounded{next: next} }

type bounded struct{ next Doer }

// Do implements Doer.
func (b bounded) Do(req *http.Request) (*http.Response, error) {
	resp, err := b.next.Do(req)
	if err == nil && resp != nil {
		boundBody(req, resp, nil)
	}
	return resp, err
}

// guard is the RoundTripper NewClient installs.
type guard struct {
	next    http.RoundTripper
	timeout time.Duration
	idle    time.Duration
}

// CloseIdleConnections forwards to the wrapped transport, so
// http.Client.CloseIdleConnections reaches it.
func (g *guard) CloseIdleConnections() {
	if c, ok := g.next.(interface{ CloseIdleConnections() }); ok {
		c.CloseIdleConnections()
	}
}

// RoundTrip implements http.RoundTripper.
func (g *guard) RoundTrip(req *http.Request) (*http.Response, error) {
	idle := g.idle > 0 && isStreamRequest(req)
	if g.timeout <= 0 && !idle {
		resp, err := g.next.RoundTrip(req)
		if err == nil && resp != nil {
			boundBody(req, resp, nil)
		}
		return resp, err
	}
	// The timer covers the wait for the response headers only, as the vendor SDKs do: it
	// is stopped as soon as RoundTrip returns, so a long stream or a slow body is
	// never cut by it. The context it cancels is released when the body is closed.
	ctx, cancel := context.WithCancel(req.Context())
	var timer *time.Timer
	if g.timeout > 0 {
		timer = time.AfterFunc(g.timeout, cancel)
	}
	resp, err := g.next.RoundTrip(req.WithContext(ctx))
	expired := timer != nil && !timer.Stop()
	if expired || err != nil {
		cancel()
		if err == nil {
			_ = resp.Body.Close()
		}
		switch {
		case expired && req.Context().Err() == nil:
			return nil, &TimeoutError{After: g.timeout}
		case err != nil:
			return nil, err
		default:
			return nil, req.Context().Err()
		}
	}
	boundBody(req, resp, cancel)
	if idle && resp.StatusCode < 300 && resp.Body != nil {
		resp.Body = newIdleBody(resp.Body, g.idle, cancel)
	}
	return resp, nil
}

// idleBody fails a read with an *IdleTimeoutError when the body delivered nothing
// for longer than after: a timer, re-armed after every read, cancels the request
// context, which makes the pending read return.
type idleBody struct {
	body    io.ReadCloser
	after   time.Duration
	timer   *time.Timer
	expired atomic.Bool
}

// newIdleBody wraps body; cancel is the cancel of the request context.
func newIdleBody(body io.ReadCloser, after time.Duration, cancel context.CancelFunc) *idleBody {
	b := &idleBody{body: body, after: after}
	b.timer = time.AfterFunc(after, func() {
		b.expired.Store(true)
		cancel()
	})
	return b
}

// Read implements io.Reader.
func (b *idleBody) Read(p []byte) (int, error) {
	if b.expired.Load() {
		return 0, &IdleTimeoutError{After: b.after}
	}
	n, err := b.body.Read(p)
	if b.expired.Load() {
		return n, &IdleTimeoutError{After: b.after}
	}
	if n > 0 || err == nil {
		b.timer.Reset(b.after)
	}
	return n, err
}

// Close stops the timer and closes the body (which releases the request context).
func (b *idleBody) Close() error {
	b.timer.Stop()
	return b.body.Close()
}

// boundBody replaces resp.Body by a bounded reader that, on Close, closes the
// original body and then calls cancel (when it is not nil). A response with no body
// has nothing to close, so cancel is released at once (review IN-203; http.Transport
// never returns a nil Body, a custom RoundTripper can).
func boundBody(req *http.Request, resp *http.Response, cancel context.CancelFunc) {
	body := resp.Body
	if body == nil {
		if cancel != nil {
			cancel()
		}
		return
	}
	var r io.Reader = body
	switch {
	case resp.StatusCode >= 300:
		r = io.LimitReader(body, MaxErrorBody)
	case !isEventStream(resp) && !isStreamRequest(req):
		r = &capped{r: body, left: MaxResponseBody}
	case cancel == nil:
		return
	}
	resp.Body = &boundedBody{Reader: r, body: body, cancel: cancel}
}

// isEventStream reports whether the response is a server-sent or AWS event stream,
// which lives as long as the server writes and is bounded per event by the SDK.
func isEventStream(resp *http.Response) bool {
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	return strings.HasPrefix(ct, "text/event-stream") || strings.HasPrefix(ct, "application/vnd.amazon.eventstream")
}

// boundedBody reads through Reader and closes the original body.
type boundedBody struct {
	io.Reader
	body   io.Closer
	cancel context.CancelFunc
}

// Close closes the original body and releases the request context.
func (b *boundedBody) Close() error {
	err := b.body.Close()
	if b.cancel != nil {
		b.cancel()
	}
	return err
}

// capped reads at most left bytes and then fails with ErrBodyTooLarge when the
// body has more, where io.LimitReader would report a clean end of a truncated body.
type capped struct {
	r    io.Reader
	left int64
	err  error
}

// Read implements io.Reader.
func (c *capped) Read(p []byte) (int, error) {
	if c.err != nil {
		return 0, c.err
	}
	if len(p) == 0 {
		return 0, nil
	}
	// One byte beyond the limit tells a body of exactly the limit from a longer one.
	if int64(len(p)) > c.left+1 {
		p = p[:c.left+1]
	}
	n, err := c.r.Read(p)
	c.left -= int64(n)
	if c.left < 0 {
		n += int(c.left)
		c.err = ErrBodyTooLarge
		return n, c.err
	}
	return n, err
}
