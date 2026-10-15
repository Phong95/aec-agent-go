package llmtest

import (
	"context"
	"errors"
	"io"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Phong95/aec-agent-go/kernel"
)

// do sends one request with the given context and returns the response.
func do(ctx context.Context, t *testing.T, c *http.Client, method, url, body string, header map[string]string) (*http.Response, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	return c.Do(req)
}

func TestUpstreamRecordsRequests(t *testing.T) {
	u := NewUpstream(t, func(Request) Reply { return Reply{} })
	c := u.Client()

	resp, err := do(t.Context(), t, c, http.MethodPost, u.URL+"/model/a%3Ab/converse?x=1&y=two", `{"k":1}`, map[string]string{"X-Probe": "yes"})
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	resp, err = do(t.Context(), t, c, http.MethodGet, u.URL+"/models", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	got := u.Requests()
	if len(got) != 2 {
		t.Fatalf("recorded %d requests, want 2", len(got))
	}
	first := got[0]
	if first.Method != http.MethodPost || first.Path != "/model/a%3Ab/converse" || first.RawQuery != "x=1&y=two" ||
		string(first.Body) != `{"k":1}` || first.Header.Get("X-Probe") != "yes" {
		t.Fatalf("first request recorded as %+v", first)
	}
	if got[1].Method != http.MethodGet || got[1].Path != "/models" || len(got[1].Body) != 0 {
		t.Fatalf("second request recorded as %+v", got[1])
	}
	// The returned slice is a copy.
	got[0].Method = "CHANGED"
	if u.Requests()[0].Method != http.MethodPost {
		t.Fatal("Requests returned the internal slice")
	}
}

func TestUpstreamServesScriptedReply(t *testing.T) {
	var calls int
	var mu sync.Mutex
	u := NewUpstream(t, func(r Request) Reply {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if r.Path == "/second" {
			return Reply{Status: http.StatusTeapot, Header: map[string]string{"X-N": "second"}, Chunks: []string{"short and stout"}}
		}
		return Reply{Status: http.StatusAccepted, Header: map[string]string{"Content-Type": "text/event-stream", "X-N": string(rune('0' + n))}, Chunks: []string{"data: one\n\n", "data: two\n\n"}}
	})
	c := u.Client()

	resp, err := do(t.Context(), t, c, http.MethodGet, u.URL+"/first", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted || resp.Header.Get("Content-Type") != "text/event-stream" || resp.Header.Get("X-N") != "1" ||
		string(body) != "data: one\n\ndata: two\n\n" {
		t.Fatalf("first reply: %d %v %q", resp.StatusCode, resp.Header, body)
	}

	resp, err = do(t.Context(), t, c, http.MethodGet, u.URL+"/second", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusTeapot || resp.Header.Get("X-N") != "second" || string(body) != "short and stout" {
		t.Fatalf("second reply: %d %v %q", resp.StatusCode, resp.Header, body)
	}
}

// TestUpstreamFlushesChunks proves a chunk reaches the client while the handler
// is still running: the first chunk is read before the request is cancelled.
func TestUpstreamFlushesChunks(t *testing.T) {
	u := NewUpstream(t, func(Request) Reply { return Reply{Chunks: []string{"first"}, Hang: true} })
	c := u.Client()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	resp, err := do(ctx, t, c, http.MethodGet, u.URL+"/stream", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	buf := make([]byte, 5)
	if _, err := io.ReadFull(resp.Body, buf); err != nil || string(buf) != "first" {
		t.Fatalf("read the flushed chunk: %q %v", buf, err)
	}
	cancel()
	if _, err := io.ReadAll(resp.Body); err == nil {
		t.Fatal("reading after cancel must fail")
	}
}

// TestUpstreamHangEndsWithRequestContext proves a Hang reply with no chunks
// blocks until the request context ends, and that the handler then returns, so
// the goleak check of TestMain sees no goroutine left behind.
func TestUpstreamHangEndsWithRequestContext(t *testing.T) {
	u := NewUpstream(t, func(Request) Reply { return Reply{Hang: true} })
	c := u.Client()

	ctx, cancel := context.WithTimeout(t.Context(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	resp, err := do(ctx, t, c, http.MethodGet, u.URL+"/hang", "", nil)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("a hanging upstream must not answer")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want a deadline error", err)
	}
	if time.Since(start) < 100*time.Millisecond {
		t.Fatalf("the request ended after %v, before its context", time.Since(start))
	}
	if len(u.Requests()) != 1 {
		t.Fatalf("recorded %d requests, want 1", len(u.Requests()))
	}
}

func TestUpstreamClientHasNoTimeout(t *testing.T) {
	u := NewUpstream(t, func(Request) Reply { return Reply{} })
	if c := u.Client(); c.Timeout != 0 {
		t.Fatalf("Client().Timeout = %v, want none", c.Timeout)
	}
}

// countingTransport records how often the base transport was reached.
type countingTransport struct {
	mu    sync.Mutex
	hosts []string
}

func (c *countingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	c.mu.Lock()
	c.hosts = append(c.hosts, req.URL.Hostname())
	c.mu.Unlock()
	return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Request: req}, nil
}

func TestLoopbackOnlyRefusesNonLoopbackBeforeDialing(t *testing.T) {
	base := &countingTransport{}
	c := &http.Client{Transport: LoopbackOnly(base)}

	for _, url := range []string{
		"http://example.invalid/",
		"https://api.anthropic.com/v1/messages",
		"http://10.0.0.5:8080/",
		"http://[2001:db8::1]/",
		"http://127.0.0.1.example.com/",
	} {
		resp, err := c.Get(url) //nolint:noctx // the refusal happens before any request context matters
		if err == nil {
			_ = resp.Body.Close()
			t.Errorf("%s was not refused", url)
			continue
		}
		if !strings.Contains(err.Error(), "loopback-only") {
			t.Errorf("%s: error %q does not say the client is loopback-only", url, err)
		}
	}
	if len(base.hosts) != 0 {
		t.Fatalf("the base transport was reached for %v", base.hosts)
	}

	for _, url := range []string{"http://127.0.0.1:1/", "http://localhost:1/", "http://[::1]:1/", "http://LOCALHOST/"} {
		resp, err := c.Get(url) //nolint:noctx // the base transport is a stub
		if err != nil {
			t.Fatalf("%s: %v", url, err)
		}
		_ = resp.Body.Close()
	}
	if len(base.hosts) != 4 {
		t.Fatalf("the base transport saw %v, want the four loopback requests", base.hosts)
	}
}

func TestLoopbackOnlyClosesRequestBodyOnRefusal(t *testing.T) {
	closed := false
	body := &closeSpy{Reader: strings.NewReader("x"), onClose: func() { closed = true }}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://example.invalid/", body)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LoopbackOnly(nil).RoundTrip(req); err == nil { //nolint:bodyclose // refused: there is no response
		t.Fatal("request was not refused")
	}
	if !closed {
		t.Fatal("the request body was not closed on refusal")
	}
}

type closeSpy struct {
	io.Reader
	onClose func()
}

func (c *closeSpy) Close() error { c.onClose(); return nil }

func TestClosedURLRefusesConnection(t *testing.T) {
	u := NewUpstream(t, func(Request) Reply { return Reply{} })
	c := u.Client()
	url := ClosedURL(t)
	if !strings.HasPrefix(url, "http://127.0.0.1:") {
		t.Fatalf("ClosedURL = %q, want a loopback URL", url)
	}
	resp, err := do(t.Context(), t, c, http.MethodGet, url+"/x", "", nil)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("nothing should listen on ClosedURL")
	}
	if strings.Contains(err.Error(), "loopback-only") {
		t.Fatalf("a loopback URL was refused as non-loopback: %v", err)
	}
	if !strings.Contains(strings.ToLower(err.Error()), "refused") {
		t.Fatalf("error = %v, want connection refused", err)
	}
}

func TestCanonicalJSON(t *testing.T) {
	got, err := CanonicalJSON([]byte(` { "b" : 1.50 , "a" : [ 2 , {"z":null,"y":12345678901234567890} ] , "s":"<&>" } `))
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"a":[2,{"y":12345678901234567890,"z":null}],"b":1.50,"s":"<&>"}`
	if string(got) != want {
		t.Fatalf("CanonicalJSON = %s, want %s", got, want)
	}
	for _, bad := range []string{``, `{`, `{"a":1} {"b":2}`, `{"a":1}x`} {
		if out, err := CanonicalJSON([]byte(bad)); err == nil {
			t.Errorf("CanonicalJSON(%q) = %s, want an error", bad, out)
		}
	}
}

// fakeTB records a Fatalf instead of failing the real test.
type fakeTB struct {
	testing.TB
	msg    string
	failed bool
}

func (f *fakeTB) Helper() {}
func (f *fakeTB) Fatalf(format string, args ...any) {
	f.failed = true
	f.msg = format
	_ = args
	runtime.Goexit()
}

// runFake runs fn on its own goroutine so a Fatalf can stop it with Goexit.
func runFake(fn func(tb *fakeTB)) *fakeTB {
	tb := &fakeTB{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn(tb)
	}()
	<-done
	return tb
}

func TestAssertJSONEqual(t *testing.T) {
	AssertJSONEqual(t, []byte(`{"a":1,"b":[1,2]}`), []byte(` {"b":[1,2], "a":1}`))

	if tb := runFake(func(tb *fakeTB) { AssertJSONEqual(tb, []byte(`{"a":1}`), []byte(`{"a":2}`)) }); !tb.failed || !strings.Contains(tb.msg, "differs") {
		t.Fatalf("different values must fail with a diff message: %+v", tb)
	}
	if tb := runFake(func(tb *fakeTB) { AssertJSONEqual(tb, []byte(`{"a":1`), []byte(`{"a":1}`)) }); !tb.failed {
		t.Fatal("invalid got must fail")
	}
	if tb := runFake(func(tb *fakeTB) { AssertJSONEqual(tb, []byte(`{"a":1}`), []byte(`nope`)) }); !tb.failed {
		t.Fatal("invalid want must fail")
	}
}

func TestChunkTexts(t *testing.T) {
	text := "hi"
	got := ChunkTexts(t, []kernel.StreamChunk{
		{Type: kernel.ChunkText, Content: &text},
		{Type: kernel.ChunkDone},
	})
	want := []string{`{"type":"text","content":"hi"}`, `{"type":"done"}`}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("ChunkTexts = %q, want %q", got, want)
	}
	if got := ChunkTexts(t, nil); len(got) != 0 {
		t.Fatalf("no chunks gave %q", got)
	}
}
