package llmtransport

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

// loopbackClient is a base client for a test server: its own transport with no
// proxy and no keep-alive, so the test leaves no connection behind.
func loopbackClient(t *testing.T) *http.Client {
	t.Helper()
	tr := &http.Transport{DisableKeepAlives: true}
	t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr}
}

// get sends a GET to url with the test's context.
func get(t *testing.T, c *http.Client, url string) (*http.Response, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	return c.Do(req)
}

// closeIfAny closes the body of a response that came back with no error, so a test
// that only looks at the error leaves no connection open.
func closeIfAny(resp *http.Response, err error) {
	if err == nil && resp != nil {
		_ = resp.Body.Close()
	}
}

// TestErrorBodyIsCapped serves a 2 MiB error body and requires the reader to see
// exactly the first MaxErrorBody bytes and a clean end.
func TestErrorBodyIsCapped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, strings.Repeat("e", 2<<20))
	}))
	t.Cleanup(srv.Close)
	c := NewClient(loopbackClient(t), Config{})
	resp, err := get(t, c, srv.URL)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil || len(b) != MaxErrorBody {
		t.Fatalf("read %d bytes, err %v; want %d bytes and no error", len(b), err, MaxErrorBody)
	}
}

// TestSuccessBodyAboveTheCapFails serves a body one byte over MaxResponseBody and
// requires ErrBodyTooLarge after exactly MaxResponseBody bytes, while a body of
// exactly the limit reads fully.
func TestSuccessBodyAboveTheCapFails(t *testing.T) {
	for _, tc := range []struct {
		name string
		size int
		fail bool
	}{
		{"at the limit", MaxResponseBody, false},
		{"one over", MaxResponseBody + 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, strings.Repeat("p", tc.size))
			}))
			t.Cleanup(srv.Close)
			c := NewClient(loopbackClient(t), Config{})
			resp, err := get(t, c, srv.URL)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			b, err := io.ReadAll(resp.Body)
			switch {
			case tc.fail && !errors.Is(err, ErrBodyTooLarge):
				t.Fatalf("err = %v, want ErrBodyTooLarge", err)
			case tc.fail && len(b) != MaxResponseBody:
				t.Fatalf("read %d bytes before the error, want %d", len(b), MaxResponseBody)
			case !tc.fail && (err != nil || len(b) != tc.size):
				t.Fatalf("read %d bytes, err %v; want %d and no error", len(b), err, tc.size)
			}
		})
	}
}

// TestEventStreamIsNotCapped streams more than MaxResponseBody as an event stream
// and requires every byte to arrive: the cap is for bodies, not for streams.
func TestEventStreamIsNotCapped(t *testing.T) {
	total := MaxResponseBody + 1<<20
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		line := strings.Repeat("d", 1023) + "\n"
		for written := 0; written < total; written += len(line) {
			_, _ = io.WriteString(w, line)
		}
	}))
	t.Cleanup(srv.Close)
	c := NewClient(loopbackClient(t), Config{})
	resp, err := get(t, c, srv.URL)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	n, err := io.Copy(io.Discard, resp.Body)
	if err != nil || n < int64(total) {
		t.Fatalf("read %d bytes, err %v; want at least %d and no error", n, err, total)
	}
}

// TestHeaderTimeoutOnlyCoversTheHeaders holds the response headers back until the
// timeout expires and requires a TimeoutError that matches context.DeadlineExceeded,
// and then serves headers at once but holds the body back for twice the timeout and
// requires the whole body: a long stream must not be cut. Nothing here depends on how
// fast the machine is: the body is released by a channel after the test itself waited
// longer than the timeout, and the one wait that can only get later under load (the
// silent server) is a wait for the timeout itself (review IN-204).
func TestHeaderTimeoutOnlyCoversTheHeaders(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	bodyGo := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/silent" {
			select {
			case <-r.Context().Done():
			case <-release:
			}
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		for range 4 {
			select {
			case <-bodyGo:
			case <-r.Context().Done():
				return
			}
			_, _ = io.WriteString(w, "x")
			w.(http.Flusher).Flush()
		}
	}))
	t.Cleanup(srv.Close)

	// The silent server never answers, so any timeout expires; a short one keeps the
	// test quick.
	short := NewClient(loopbackClient(t), Config{RequestTimeout: 100 * time.Millisecond})
	start := time.Now()
	silent, err := get(t, short, srv.URL+"/silent")
	closeIfAny(silent, err)
	var te *TimeoutError
	if !errors.As(err, &te) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want a *TimeoutError that is context.DeadlineExceeded", err)
	}
	if time.Since(start) > 30*time.Second {
		t.Fatalf("the timeout took %v", time.Since(start))
	}

	// The headers of the second request arrive at once, and a timeout this long is
	// not reached by a slow machine.
	const timeout = time.Second
	c := NewClient(loopbackClient(t), Config{RequestTimeout: timeout})
	resp, err := get(t, c, srv.URL+"/slow-body")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	// The timer was stopped when the headers came back; let it have expired twice over
	// before the first body byte is written.
	time.Sleep(2 * timeout)
	go func() {
		for range 4 {
			select {
			case bodyGo <- struct{}{}:
			case <-t.Context().Done():
				return
			}
		}
	}()
	b, err := io.ReadAll(resp.Body)
	if err != nil || string(b) != "xxxx" {
		t.Fatalf("body %q, err %v; want xxxx and no error (the body outlives the %v timeout)", b, err, timeout)
	}
}

// TestCallerContextWinsOverTheTimeout cancels the caller's context while the request
// waits for headers, long before the timeout, and requires the context's own error,
// not a TimeoutError. The cancel waits for the handler to have the request, not for
// a time (review IN-204).
func TestCallerContextWinsOverTheTimeout(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	arrived := make(chan struct{})
	var arrivedOnce sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		arrivedOnce.Do(func() { close(arrived) })
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(srv.Close)
	c := NewClient(loopbackClient(t), Config{RequestTimeout: time.Hour})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() {
		select {
		case <-arrived:
			cancel()
		case <-ctx.Done():
		}
	}()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(req)
	closeIfAny(resp, err)
	var te *TimeoutError
	if errors.As(err, &te) || !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want the cancellation of the caller's context and no *TimeoutError", err)
	}
}

// TestStreamRequestIsNotCapped serves 2xx bodies above MaxResponseBody under a
// Content-Type the guard does not know as a stream: a request made with StreamContext
// reads them whole, a request without it fails at the cap, and an error answer to a
// stream request is still cut at MaxErrorBody (review IN-201).
func TestStreamRequestIsNotCapped(t *testing.T) {
	total := MaxResponseBody + 1<<20
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		if r.URL.Path == "/error" {
			w.WriteHeader(http.StatusInternalServerError)
		}
		line := strings.Repeat("d", 1023) + "\n"
		for written := 0; written < total; written += len(line) {
			if _, err := io.WriteString(w, line); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	c := NewClient(loopbackClient(t), Config{})
	do := func(path string, stream bool) (int64, error) {
		ctx := t.Context()
		if stream {
			ctx = StreamContext(ctx)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := c.Do(req)
		if err != nil {
			t.Fatalf("Do: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		return io.Copy(io.Discard, resp.Body)
	}
	if n, err := do("/ok", true); err != nil || n < int64(total) {
		t.Errorf("stream request: read %d bytes, err %v; want at least %d and no error", n, err, total)
	}
	if _, err := do("/ok", false); !errors.Is(err, ErrBodyTooLarge) {
		t.Errorf("plain request: err = %v, want ErrBodyTooLarge", err)
	}
	if n, err := do("/error", true); err != nil || n != MaxErrorBody {
		t.Errorf("error answer to a stream request: read %d bytes, err %v; want %d and no error", n, err, MaxErrorBody)
	}
}

// roundTripperFunc adapts a function to http.RoundTripper.
type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestNilBodyReleasesTheTimeoutContext hands the guard a RoundTripper that answers
// with a nil Body (http.Transport never does) and requires the derived timeout
// context to be released when RoundTrip returns, not when the parent ends (review
// IN-203).
func TestNilBodyReleasesTheTimeoutContext(t *testing.T) {
	var derived context.Context
	base := &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		derived = r.Context()
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Request: r}, nil
	})}
	c := NewClient(base, Config{RequestTimeout: time.Hour})
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://example.invalid/", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Transport.RoundTrip(req) //nolint:bodyclose // the answer under test has no body
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if resp.Body != nil {
		t.Fatalf("the test answer has a body %v", resp.Body)
	}
	if derived == nil || derived.Err() == nil {
		t.Fatal("the timeout context of a response with no body was not released")
	}
	if t.Context().Err() != nil {
		t.Fatal("the parent context ended")
	}
}

// TestRedirectDropsCredentialHeadersOnHostChange redirects from 127.0.0.1 to
// localhost (another host, the same machine) and requires the second server to see
// none of the credential headers, while a redirect inside one host keeps them and a
// header that is not a credential is kept either way.
func TestRedirectDropsCredentialHeadersOnHostChange(t *testing.T) {
	var mu sync.Mutex
	var seen []http.Header
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Clone())
		mu.Unlock()
		_, _ = io.WriteString(w, "ok")
	}))
	t.Cleanup(second.Close)
	target := strings.Replace(second.URL, "127.0.0.1", "localhost", 1)
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/same":
			http.Redirect(w, r, "/landed", http.StatusTemporaryRedirect)
		case "/landed":
			mu.Lock()
			seen = append(seen, r.Header.Clone())
			mu.Unlock()
			_, _ = io.WriteString(w, "ok")
		default:
			http.Redirect(w, r, target, http.StatusTemporaryRedirect)
		}
	}))
	t.Cleanup(first.Close)

	c := NewClient(loopbackClient(t), Config{CredentialHeaders: []string{"X-Api-Key", "OpenAI-Organization"}})
	do := func(path string) {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, first.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-Api-Key", "sk-secret")
		req.Header.Set("OpenAI-Organization", "org-secret")
		req.Header.Set("X-Request-Tag", "tag")
		resp, err := c.Do(req)
		if err != nil {
			t.Skipf("localhost does not reach the loopback listener on this host: %v", err)
		}
		_ = resp.Body.Close()
	}

	do("/cross")
	do("/same")
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 {
		t.Fatalf("the servers saw %d redirected requests, want 2", len(seen))
	}
	cross, same := seen[0], seen[1]
	if cross.Get("X-Api-Key") != "" || cross.Get("OpenAI-Organization") != "" {
		t.Errorf("the redirect to another host carried a credential: %v", cross)
	}
	if cross.Get("X-Request-Tag") != "tag" {
		t.Errorf("a header that is not a credential was dropped: %v", cross)
	}
	if same.Get("X-Api-Key") != "sk-secret" || same.Get("OpenAI-Organization") != "org-secret" {
		t.Errorf("a redirect inside one host lost the credentials: %v", same)
	}
}

// TestRedirectLimit requires a redirect loop to end after ten redirects and the
// caller's own CheckRedirect to run after the shared policy.
func TestRedirectLimit(t *testing.T) {
	var hits int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		http.Redirect(w, r, "/loop", http.StatusTemporaryRedirect)
	}))
	t.Cleanup(srv.Close)
	base := loopbackClient(t)
	prevCalls := 0
	base.CheckRedirect = func(*http.Request, []*http.Request) error { prevCalls++; return nil }
	c := NewClient(base, Config{})
	resp, err := get(t, c, srv.URL)
	closeIfAny(resp, err)
	if err == nil || !strings.Contains(err.Error(), "10 redirects") {
		t.Fatalf("err = %v, want the redirect limit", err)
	}
	if hits != MaxRedirects {
		t.Errorf("the server saw %d requests, want %d", hits, MaxRedirects)
	}
	if prevCalls != MaxRedirects-1 {
		t.Errorf("the caller's CheckRedirect ran %d times, want %d", prevCalls, MaxRedirects-1)
	}
	if base.Transport == nil || base.CheckRedirect == nil {
		t.Error("NewClient changed its base client")
	}
}

// fakeDoer answers every request with one prepared response.
type fakeDoer struct{ resp *http.Response }

func (f fakeDoer) Do(*http.Request) (*http.Response, error) { return f.resp, nil }

// TestBoundBodiesWrapsADoer is the AWS path: an error body is cut, a body over the
// cap fails, and an AWS event stream is left alone.
func TestBoundBodiesWrapsADoer(t *testing.T) {
	read := func(status int, ct string, size int) (int, error) {
		resp := &http.Response{
			StatusCode: status,
			Header:     http.Header{"Content-Type": []string{ct}},
			Body:       io.NopCloser(strings.NewReader(strings.Repeat("b", size))),
		}
		out, err := BoundBodies(fakeDoer{resp}).Do(&http.Request{})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = out.Body.Close() }()
		n, err := io.Copy(io.Discard, out.Body)
		return int(n), err
	}
	if n, err := read(400, "application/json", 2<<20); err != nil || n != MaxErrorBody {
		t.Errorf("error body: %d bytes, err %v; want %d and no error", n, err, MaxErrorBody)
	}
	if _, err := read(200, "application/json", MaxResponseBody+1); !errors.Is(err, ErrBodyTooLarge) {
		t.Errorf("large body: err = %v, want ErrBodyTooLarge", err)
	}
	if n, err := read(200, "application/vnd.amazon.eventstream", MaxResponseBody+10); err != nil || n != MaxResponseBody+10 {
		t.Errorf("event stream: %d bytes, err %v; want all of them", n, err)
	}
}

// TestTruncateMessage cuts a long text at MaxErrorMessage bytes on a rune boundary
// and marks the cut; a short text is unchanged.
func TestTruncateMessage(t *testing.T) {
	if got := TruncateMessage("short"); got != "short" {
		t.Errorf("a short text became %q", got)
	}
	exact := strings.Repeat("a", MaxErrorMessage)
	if got := TruncateMessage(exact); got != exact {
		t.Errorf("a text of exactly the limit was cut to %d bytes", len(got))
	}
	// A three-byte character straddles the cut point.
	long := strings.Repeat("a", MaxErrorMessage-4) + strings.Repeat("ế", 10)
	got := TruncateMessage(long)
	if len(got) > MaxErrorMessage || !strings.HasSuffix(got, "…") || !utf8.ValidString(got) {
		t.Errorf("cut text: %d bytes, valid %v, suffix %q", len(got), utf8.ValidString(got), got[len(got)-3:])
	}
}

// TestIsTransient tells network failures from everything else.
func TestIsTransient(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"plain", errors.New("boom"), false},
		{"net op error", &net.OpError{Op: "read", Net: "tcp", Err: errors.New("connection reset by peer")}, true},
		{"url error", &url.Error{Op: "Post", URL: "http://x", Err: errors.New("refused")}, true},
		{"unexpected EOF", fmt.Errorf("read: %w", io.ErrUnexpectedEOF), true},
		{"header timeout", &TimeoutError{After: time.Second}, true},
		{"idle timeout", &IdleTimeoutError{After: time.Second}, true},
		{"deadline", context.DeadlineExceeded, true},
		{"canceled", context.Canceled, false},
		{"body too large", ErrBodyTooLarge, false},
	}
	for _, c := range cases {
		if got := IsTransient(c.err); got != c.want {
			t.Errorf("%s: IsTransient = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestIdleTimeoutEndsAStalledStream serves one event and then nothing, and requires
// the read of a stream request to fail with an *IdleTimeoutError once the idle
// timeout expires, while a stream that keeps writing is never cut and a request that
// is not a stream gets no idle timeout.
func TestIdleTimeoutEndsAStalledStream(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		steady := r.URL.Path == "/steady"
		for range 6 {
			_, _ = io.WriteString(w, "data: x\n\n")
			w.(http.Flusher).Flush()
			if !steady {
				break
			}
			time.Sleep(40 * time.Millisecond)
		}
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })
	c := NewClient(loopbackClient(t), Config{IdleTimeout: 150 * time.Millisecond})

	read := func(t *testing.T, path string, stream bool, limit time.Duration) ([]byte, error) {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), limit)
		defer cancel()
		if stream {
			ctx = StreamContext(ctx)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := c.Do(req)
		if err != nil {
			t.Fatalf("Do: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		return io.ReadAll(resp.Body)
	}

	t.Run("stalled stream", func(t *testing.T) {
		b, err := read(t, "/stalled", true, 30*time.Second)
		var idle *IdleTimeoutError
		if !errors.As(err, &idle) || !errors.Is(err, context.DeadlineExceeded) || string(b) != "data: x\n\n" {
			t.Fatalf("read %q, err %v; want the event and an *IdleTimeoutError", b, err)
		}
	})
	t.Run("steady stream", func(t *testing.T) {
		// Six events 40 ms apart (240 ms in all) under a 150 ms idle timeout: the
		// timer is re-armed by every read, so only the stall at the end cuts it.
		b, err := read(t, "/steady", true, 30*time.Second)
		var idle *IdleTimeoutError
		if !errors.As(err, &idle) || strings.Count(string(b), "data: x") != 6 {
			t.Fatalf("read %d events, err %v; want all 6 and then the idle timeout", strings.Count(string(b), "data: x"), err)
		}
	})
	t.Run("not a stream", func(t *testing.T) {
		_, err := read(t, "/stalled", false, 400*time.Millisecond)
		var idle *IdleTimeoutError
		if errors.As(err, &idle) || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err %v; want the caller's deadline, not the idle timeout", err)
		}
	})
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name   string
		header map[string]string
		want   time.Duration
	}{
		{"none", nil, 0},
		{"seconds", map[string]string{"Retry-After": "7"}, 7 * time.Second},
		{"seconds with spaces", map[string]string{"Retry-After": " 2 "}, 2 * time.Second},
		{"fractional seconds", map[string]string{"Retry-After": "1.5"}, 1500 * time.Millisecond},
		{"zero", map[string]string{"Retry-After": "0"}, 0},
		{"negative", map[string]string{"Retry-After": "-3"}, 0},
		{"absurd", map[string]string{"Retry-After": "1e30"}, 0},
		{"garbage", map[string]string{"Retry-After": "soon"}, 0},
		{"http date", map[string]string{"Retry-After": now.Add(90 * time.Second).Format(http.TimeFormat)}, 90 * time.Second},
		{"past http date", map[string]string{"Retry-After": now.Add(-time.Minute).Format(http.TimeFormat)}, 0},
		{"milliseconds win", map[string]string{"Retry-After-Ms": "250", "Retry-After": "9"}, 250 * time.Millisecond},
		{"bad milliseconds fall back", map[string]string{"Retry-After-Ms": "x", "Retry-After": "9"}, 9 * time.Second},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var h http.Header
			if c.header != nil {
				h = http.Header{}
				for k, v := range c.header {
					h.Set(k, v)
				}
			}
			if got := ParseRetryAfter(h, now); got != c.want {
				t.Errorf("ParseRetryAfter = %v, want %v", got, c.want)
			}
		})
	}
	if RetryAfter(nil) != 0 {
		t.Error("RetryAfter(nil) must be 0")
	}
}
