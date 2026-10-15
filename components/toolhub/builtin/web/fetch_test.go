package web

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// countingServer starts a loopback server that counts its hits and serves handler.
func countingServer(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// TestFetchPage_LoopbackEndToEnd is the tracer of the guarded fetch path: a loopback URL
// is refused before any request with the guard text, and with allowPrivateNetwork the same
// fetch reaches the server through the hardened transport and the manual hop loop.
func TestFetchPage_LoopbackEndToEnd(t *testing.T) {
	srv, hits := countingServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/hop1":
			http.Redirect(w, r, "/hop2", http.StatusFound)
		case "/hop2":
			http.Redirect(w, r, "/final?x=1", http.StatusMovedPermanently)
		default:
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			_, _ = fmt.Fprint(w, "hello from the loopback server")
		}
	})
	root := srv.URL + "/"

	t.Run("refused before any request", func(t *testing.T) {
		before := hits.Load()
		_, err := fetchPage(context.Background(), root, fetchOptions{Transport: newTransport(false)})
		var blocked *blockedError
		if !errors.As(err, &blocked) {
			t.Fatalf("want a *blockedError, got %T %v", err, err)
		}
		if got, want := err.Error(), "address 127.0.0.1 is not a public address"; got != want {
			t.Fatalf("error text: got %q, want %q", got, want)
		}
		if n := hits.Load() - before; n != 0 {
			t.Fatalf("the server saw %d requests, want 0", n)
		}
	})

	t.Run("allowPrivateNetwork reaches the server", func(t *testing.T) {
		before := hits.Load()
		page, err := fetchPage(context.Background(), root, fetchOptions{AllowPrivate: true, Transport: newTransport(true)})
		if err != nil {
			t.Fatalf("fetchPage: %v", err)
		}
		if page.Status != http.StatusOK || page.FinalURL != root || page.URL != root {
			t.Fatalf("page = %+v, want status 200 and final URL %q", page, root)
		}
		if got := string(page.Body); got != "hello from the loopback server" {
			t.Fatalf("body = %q", got)
		}
		if !strings.HasPrefix(page.ContentType, "text/plain") {
			t.Fatalf("content type = %q", page.ContentType)
		}
		if n := hits.Load() - before; n != 1 {
			t.Fatalf("the server saw %d requests, want exactly 1", n)
		}
	})

	t.Run("a redirect chain is followed and the final URL reported", func(t *testing.T) {
		before := hits.Load()
		page, err := fetchPage(context.Background(), srv.URL+"/hop1", fetchOptions{AllowPrivate: true, MaxRedirects: 5, Transport: newTransport(true)})
		if err != nil {
			t.Fatalf("fetchPage: %v", err)
		}
		if want := srv.URL + "/final?x=1"; page.FinalURL != want {
			t.Fatalf("final URL = %q, want %q", page.FinalURL, want)
		}
		if page.URL != srv.URL+"/hop1" {
			t.Fatalf("URL = %q", page.URL)
		}
		if n := hits.Load() - before; n != 3 {
			t.Fatalf("the server saw %d requests, want 3 (two hops and the page)", n)
		}
	})
}

// TestFetch_BodyCap: the body is cut at MaxBytes (the server's remaining bytes are never
// read into the result).
func TestFetch_BodyCap(t *testing.T) {
	srv, _ := countingServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = fmt.Fprint(w, strings.Repeat("x", 5000))
	})
	page, err := fetchPage(t.Context(), srv.URL+"/", fetchOptions{AllowPrivate: true, MaxBytes: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Body) != 1000 {
		t.Fatalf("body length = %d, want 1000", len(page.Body))
	}
	// a body that fits is returned whole, and the default cap is 2 MB
	small, err := fetchPage(t.Context(), srv.URL+"/", fetchOptions{AllowPrivate: true, MaxBytes: 6000})
	if err != nil || len(small.Body) != 5000 {
		t.Fatalf("a body below the cap must be whole: %d %v", len(small.Body), err)
	}
	if fetchDefaultMaxBytes != 2_000_000 || fetchDefaultTimeout.Milliseconds() != 15000 {
		t.Fatal("the fetch defaults must be the TS defaults")
	}
}

// hangingServer answers nothing until the test ends or the client goes away.
func hangingServer(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	release := make(chan struct{})
	srv, hits := countingServer(t, func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	t.Cleanup(func() { close(release) })
	return srv, hits
}

// TestFetch_Timeout: a hung server ends at the page deadline with the timeout text.
func TestFetch_Timeout(t *testing.T) {
	srv, _ := hangingServer(t)
	start := time.Now()
	_, err := fetchPage(t.Context(), srv.URL+"/", fetchOptions{AllowPrivate: true, Timeout: 50 * time.Millisecond})
	var pe *pageError
	if !errors.As(err, &pe) {
		t.Fatalf("want a *pageError, got %T %v", err, err)
	}
	if err.Error() != "timed out after 50 ms" {
		t.Fatalf("error text = %q", err.Error())
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("the timeout must wrap context.DeadlineExceeded")
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("the timeout took %v", d)
	}
}

// TestFetch_TimeoutCoversTheBody: the deadline also stops a body that stalls after the
// headers, not just a hung request.
func TestFetch_TimeoutCoversTheBody(t *testing.T) {
	release := make(chan struct{})
	srv, _ := countingServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, "partial")
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	t.Cleanup(func() { close(release) })
	_, err := fetchPage(t.Context(), srv.URL+"/", fetchOptions{AllowPrivate: true, Timeout: 100 * time.Millisecond})
	if err == nil || err.Error() != "timed out after 100 ms" {
		t.Fatalf("got %v", err)
	}
}

// TestFetch_ParentCancelled: the page ctx derives from the tool ctx, so stopping the run
// stops the fetch at once with `cancelled`.
func TestFetch_ParentCancelled(t *testing.T) {
	srv, hits := hangingServer(t)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := fetchPage(ctx, srv.URL+"/", fetchOptions{AllowPrivate: true, Timeout: time.Minute})
		done <- err
	}()
	for hits.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err == nil || err.Error() != "cancelled" || !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("fetchPage did not return after the context was cancelled")
	}
}

func TestFetch_AlreadyCancelled(t *testing.T) {
	srv, hits := countingServer(t, func(http.ResponseWriter, *http.Request) {})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := fetchPage(ctx, srv.URL+"/", fetchOptions{AllowPrivate: true})
	if err == nil || err.Error() != "cancelled" {
		t.Fatalf("got %v", err)
	}
	if hits.Load() != 0 {
		t.Fatal("a cancelled fetch must not reach the server")
	}
}

// TestFetch_HTTPStatus: a non-2xx is `HTTP <status>` with the final URL and status.
func TestFetch_HTTPStatus(t *testing.T) {
	srv, _ := countingServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/gone" {
			w.WriteHeader(http.StatusGone)
			return
		}
		http.Redirect(w, r, "/gone", http.StatusFound)
	})
	page, err := fetchPage(t.Context(), srv.URL+"/start", fetchOptions{AllowPrivate: true, MaxRedirects: 5})
	var pe *pageError
	if !errors.As(err, &pe) || err.Error() != "HTTP 410" {
		t.Fatalf("got %T %v", err, err)
	}
	if page.Status != http.StatusGone || page.FinalURL != srv.URL+"/gone" || page.URL != srv.URL+"/start" {
		t.Fatalf("page = %+v", page)
	}
}

// TestFetch_Headers: the default User-Agent and Accept headers are sent (and the options win over
// the defaults); no credentials or cookies are ever sent.
func TestFetch_Headers(t *testing.T) {
	var got http.Header
	srv, _ := countingServer(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		_, _ = fmt.Fprint(w, "ok")
	})
	if _, err := fetchPage(t.Context(), srv.URL+"/", fetchOptions{AllowPrivate: true}); err != nil {
		t.Fatal(err)
	}
	if ua := got.Get("User-Agent"); ua != "AECAgent/1.0 (+web_extract)" {
		t.Fatalf("User-Agent = %q", ua)
	}
	if accept := got.Get("Accept"); accept != "text/html,application/xhtml+xml,text/plain,application/json;q=0.9,*/*;q=0.5" {
		t.Fatalf("Accept = %q", accept)
	}
	for _, h := range []string{"Authorization", "Cookie", "Referer"} {
		if v := got.Get(h); v != "" {
			t.Fatalf("%s = %q must not be sent", h, v)
		}
	}
	if _, err := fetchPage(t.Context(), srv.URL+"/", fetchOptions{AllowPrivate: true, UserAgent: "custom/1"}); err != nil {
		t.Fatal(err)
	}
	if ua := got.Get("User-Agent"); ua != "custom/1" {
		t.Fatalf("User-Agent = %q", ua)
	}
}

// TestFetch_Gzip: the default transport asks for gzip on its own and decodes it
// transparently, so the bytes returned are the decoded ones.
func TestFetch_Gzip(t *testing.T) {
	srv, _ := countingServer(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			_, _ = fmt.Fprint(w, "no gzip offered")
			return
		}
		w.Header().Set("Content-Encoding", "gzip")
		zw := gzip.NewWriter(w)
		_, _ = zw.Write([]byte("zipped text"))
		_ = zw.Close()
	})
	page, err := fetchPage(t.Context(), srv.URL+"/", fetchOptions{AllowPrivate: true})
	if err != nil || string(page.Body) != "zipped text" {
		t.Fatalf("body %q err %v", page.Body, err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestFetch_GuardErrorsAreBlocked: the guard runs before the transport, whatever the
// transport is, and a blocked URL never reaches a round tripper.
func TestFetch_GuardErrorsAreBlocked(t *testing.T) {
	rt := roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Error("the round tripper must not be called")
		return nil, errors.New("unreachable")
	})
	for _, raw := range []string{"file:///etc/passwd", "http://user:pw@example.com/", "http://127.0.0.1/", "not a url", "http://[::1]/"} {
		_, err := fetchPage(t.Context(), raw, fetchOptions{Transport: rt, Resolver: publicResolver(nil)})
		var blocked *blockedError
		if !errors.As(err, &blocked) {
			t.Errorf("%s: want a *blockedError, got %T %v", raw, err, err)
		}
	}
}
