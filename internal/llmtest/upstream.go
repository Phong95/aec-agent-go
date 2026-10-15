// Package llmtest is the test harness shared by the four LLM provider packages
// (Phase 8): a loopback-only HTTP upstream that records what a provider sent and
// replays a scripted reply, an HTTP client that refuses every non-loopback host,
// and canonical JSON helpers for comparing request bodies and stream chunks.
//
// It is a non-test package that imports "testing" and net/http/httptest, so the
// provider leaves and the conformance runners can all use it. It sits under the
// module's internal/ directory (not under llm/) for that reason, and it is not
// part of the public API. It must not import internal/wiretest: callers load
// fixtures in their own test files.
//
// No test may reach a real provider (T-08-06): every HTTP test builds its client
// from Upstream.Client, whose transport refuses any host that is not loopback
// before it dials, so not even a DNS lookup leaves the machine.
package llmtest

import (
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// Request is what the upstream recorded of one request.
type Request struct {
	Method string
	// Path is the escaped path as the client sent it (a %3A stays %3A), without
	// the query string.
	Path     string
	RawQuery string
	Header   http.Header
	Body     []byte
}

// Reply is the scripted answer to one request.
type Reply struct {
	// Status is the HTTP status; zero means 200.
	Status int
	// Header holds the response headers, set before the status is written.
	Header map[string]string
	// Chunks are written to the body one by one and flushed after each, so a
	// reader sees them as separate network writes.
	Chunks []string
	// Hang makes the handler block after the chunks until the request context is
	// done (the client gave up or cancelled) or the upstream is closed. With no
	// chunks nothing at all is written, not even the status, so the client waits
	// for response headers.
	Hang bool
}

// Upstream is a loopback HTTP server that records every request and answers it
// with the Reply its respond function returns.
type Upstream struct {
	// URL is the base URL of the server, for example http://127.0.0.1:41233.
	URL string

	t       testing.TB
	srv     *httptest.Server
	closing chan struct{}

	mu   sync.Mutex
	reqs []Request
}

// NewUpstream starts the server and closes it when the test ends. respond is
// called once per request, outside the lock; it must be safe for concurrent use
// when the test sends requests concurrently.
func NewUpstream(t testing.TB, respond func(Request) Reply) *Upstream {
	t.Helper()
	u := &Upstream{t: t, closing: make(chan struct{})}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		req := Request{
			Method:   r.Method,
			Path:     r.URL.EscapedPath(),
			RawQuery: r.URL.RawQuery,
			Header:   r.Header.Clone(),
			Body:     body,
		}
		u.mu.Lock()
		u.reqs = append(u.reqs, req)
		u.mu.Unlock()
		u.serve(w, r, respond(req))
	}))
	u.URL = u.srv.URL
	t.Cleanup(func() {
		// Release a hanging handler first: Server.Close waits for every
		// outstanding request, and an idle client cannot be relied on to hang up.
		close(u.closing)
		u.srv.CloseClientConnections()
		u.srv.Close()
	})
	return u
}

// serve writes one scripted reply.
func (u *Upstream) serve(w http.ResponseWriter, r *http.Request, rep Reply) {
	if rep.Hang && len(rep.Chunks) == 0 {
		u.wait(r)
		return
	}
	for k, v := range rep.Header {
		w.Header().Set(k, v)
	}
	status := rep.Status
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	flusher, _ := w.(http.Flusher)
	if flusher != nil {
		flusher.Flush()
	}
	for _, c := range rep.Chunks {
		if _, err := io.WriteString(w, c); err != nil {
			return
		}
		if flusher != nil {
			flusher.Flush()
		}
	}
	if rep.Hang {
		u.wait(r)
	}
}

// wait blocks until the request context ends or the upstream is closed.
func (u *Upstream) wait(r *http.Request) {
	select {
	case <-r.Context().Done():
	case <-u.closing:
	}
}

// Requests returns a copy of the requests recorded so far, oldest first.
func (u *Upstream) Requests() []Request {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := make([]Request, len(u.reqs))
	copy(out, u.reqs)
	return out
}

// Client returns an HTTP client for this upstream: a clone of the default
// transport wrapped in LoopbackOnly, no proxy and no Timeout (the request
// lifetime comes from the request context, as in the providers). Its idle
// connections are closed when the test ends.
//
// Keep-alive is off: a response whose body was read to its end (a stream that
// finished just as the consumer stopped) would otherwise leave its connection
// idle in the pool, and the three goroutines of that connection (two of the client,
// one of the server) outlive the call. The Provider conformance suite counts
// goroutines after a stream is abandoned, and a pooled connection is a leak in that
// count, not in the provider. Nothing in the tests depends on connection reuse.
func (u *Upstream) Client() *http.Client {
	u.t.Helper()
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		u.t.Fatal("llmtest: http.DefaultTransport is not an *http.Transport")
	}
	tr := base.Clone()
	tr.Proxy = nil
	tr.DisableKeepAlives = true
	c := &http.Client{Transport: LoopbackOnly(tr)}
	u.t.Cleanup(c.CloseIdleConnections)
	return c
}

// loopbackTransport refuses a request to any host that is not loopback.
type loopbackTransport struct {
	base http.RoundTripper
}

// LoopbackOnly wraps base (http.DefaultTransport when nil) so that a request
// whose URL host is not 127.0.0.1, ::1 or localhost fails with an error that says
// the test client is loopback-only. The check runs before base is called, so no
// connection and no DNS lookup is made. A redirect to another host is refused the
// same way.
func LoopbackOnly(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return &loopbackTransport{base: base}
}

// RoundTrip implements http.RoundTripper.
func (l *loopbackTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	host := req.URL.Hostname()
	switch strings.ToLower(host) {
	case "127.0.0.1", "::1", "localhost":
		return l.base.RoundTrip(req)
	}
	if req.Body != nil {
		_ = req.Body.Close()
	}
	return nil, errors.New("llmtest: the test client is loopback-only and refuses host \"" + host + "\" (no test may reach a real provider)")
}

// CloseIdleConnections forwards to the base transport, so
// http.Client.CloseIdleConnections reaches it.
func (l *loopbackTransport) CloseIdleConnections() {
	if c, ok := l.base.(interface{ CloseIdleConnections() }); ok {
		c.CloseIdleConnections()
	}
}

// ClosedURL returns a loopback base URL that nothing listens on: a request to it
// fails with connection refused. The port was free a moment ago and is not
// reused by the test.
func ClosedURL(t testing.TB) string {
	t.Helper()
	var lc net.ListenConfig
	l, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("llmtest: reserve a port: %v", err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatalf("llmtest: release the port: %v", err)
	}
	return "http://" + addr
}
