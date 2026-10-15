package web

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// serverPort is the TCP port of a test server.
func serverPort(t *testing.T, srv *httptest.Server) int {
	t.Helper()
	addr, ok := srv.Listener.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("listener address is %T", srv.Listener.Addr())
	}
	return addr.Port
}

// rebindingTransport is the transport the rebinding tests use: its dialer resolves names
// through a UDP DNS stub that answers ips, so the address it dials differs from what the
// pre-check resolver said. When UDP on 127.0.0.1 is unavailable it falls back to a dialer
// that maps every name to the first address (the second result says which variant ran).
func rebindingTransport(t *testing.T, allowPrivate bool, ips ...string) (*http.Transport, string) {
	t.Helper()
	if _, res, ok := startDNSStub(t, ips...); ok {
		return newTransportWith(allowPrivate, res), "udp-dns-stub"
	}
	d := &net.Dialer{}
	if !allowPrivate {
		d.ControlContext = rejectNonPublic
	}
	return &http.Transport{
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			_, port, err := net.SplitHostPort(address)
			if err != nil {
				return nil, err
			}
			return d.DialContext(ctx, network, net.JoinHostPort(ips[0], port))
		},
		DisableKeepAlives: true,
	}, "rewriting-dialer"
}

func TestRejectNonPublic(t *testing.T) {
	for _, a := range []string{
		"127.0.0.1:80", "10.0.0.5:1", "169.254.169.254:80", "192.168.1.1:443", "100.64.0.1:80", "0.0.0.0:80",
		"[::1]:80", "[::]:80", "[::ffff:127.0.0.1]:80", "[::ffff:a00:1]:80", "[fe80::1%eth0]:80", "[fd00::1]:80", "[64:ff9b::1]:80",
		"localhost:80", "example.com:80", "127.0.0.1", "", "[::1]",
	} {
		err := rejectNonPublic(t.Context(), "tcp", a, struct{}{})
		if !errors.Is(err, errBlockedDial) {
			t.Errorf("%q must be refused with errBlockedDial, got %v", a, err)
		}
	}
	for _, a := range []string{"93.184.216.34:443", "[2606:4700:4700::1111]:443", "8.8.8.8:53", "[::ffff:8.8.8.8]:80"} {
		if err := rejectNonPublic(t.Context(), "tcp", a, struct{}{}); err != nil {
			t.Errorf("%q must be allowed: %v", a, err)
		}
	}
	var blocked *dialBlockedError
	if err := rejectNonPublic(t.Context(), "tcp", "[::ffff:127.0.0.1]:80", 0); !errors.As(err, &blocked) || blocked.addr != "::ffff:127.0.0.1" {
		t.Errorf("the refusal must name the address that was dialed: %v", err)
	}
}

// TestNewTransport pins what the default transport must never do: read a proxy variable,
// and skip the dial-time check (unless allowPrivate says so).
func TestNewTransport(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	t.Setenv("http_proxy", "http://127.0.0.1:1")
	t.Setenv("https_proxy", "http://127.0.0.1:1")
	tr := newTransport(false)
	if tr.Proxy != nil {
		t.Fatal("the transport must not read the proxy environment")
	}
	if !tr.DisableKeepAlives || tr.DialContext == nil {
		t.Fatal("keep-alives must be off and the dialer set")
	}
}

// TestDialControlCatchesRebinding plays DNS rebinding: the pre-check resolver says the name
// is public, the dialer's own lookup (a UDP DNS stub) says 127.0.0.1. The dial-time check
// refuses the connection and the server sees nothing; with allowPrivateNetwork the same URL
// reaches it (the positive control, proving the refusal came from the check).
func TestDialControlCatchesRebinding(t *testing.T) {
	srv, hits := countingServer(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = fmt.Fprint(w, "secret") })
	raw := "http://rebind.test:" + strconv.Itoa(serverPort(t, srv)) + "/"
	pre := publicResolver(map[string][]string{"rebind.test": {"93.184.216.34"}})

	guarded, variant := rebindingTransport(t, false, "127.0.0.1")
	t.Logf("rebinding variant: %s", variant)

	// the pre-check alone is fooled
	if _, err := assertPublicHTTPURL(t.Context(), raw, nil, guardOptions{Resolver: pre}); err != nil {
		t.Fatalf("the pre-check must pass: %v", err)
	}

	t.Run("refused at dial time", func(t *testing.T) {
		_, err := fetchPage(t.Context(), raw, fetchOptions{Transport: guarded, Resolver: pre})
		var blocked *blockedError
		if !errors.As(err, &blocked) {
			t.Fatalf("want a *blockedError, got %T %v", err, err)
		}
		// The host is a name, so the refusal names it and not the address it resolved to
		// (review IN-A1).
		if got, want := err.Error(), `host "rebind.test" resolves to a non-public address`; got != want {
			t.Fatalf("error text: got %q, want %q", got, want)
		}
		if n := hits.Load(); n != 0 {
			t.Fatalf("the server was reached %d times", n)
		}
	})

	t.Run("the raw client error wraps errBlockedDial", func(t *testing.T) {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, raw, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := (&http.Client{Transport: guarded}).Do(req)
		if err == nil {
			_ = resp.Body.Close()
			t.Fatal("the request must fail")
		}
		if !errors.Is(err, errBlockedDial) {
			t.Fatalf("errors.Is(err, errBlockedDial) is false for %v", err)
		}
		if n := hits.Load(); n != 0 {
			t.Fatalf("the server was reached %d times", n)
		}
	})

	t.Run("allowPrivateNetwork reaches the server", func(t *testing.T) {
		open, _ := rebindingTransport(t, true, "127.0.0.1")
		page, err := fetchPage(t.Context(), raw, fetchOptions{Transport: open, Resolver: pre, AllowPrivate: true})
		if err != nil {
			t.Fatalf("positive control: %v", err)
		}
		if string(page.Body) != "secret" || hits.Load() != 1 {
			t.Fatalf("positive control: body %q, hits %d", page.Body, hits.Load())
		}
	})
}

// controlLog records the addresses the dial-time check of TestDialControlTwoAddresses saw
// and what became of each.
var controlLog = struct {
	sync.Mutex
	results map[string]error
}{results: map[string]error{}}

var errTestPublicDial = errors.New("test: the dial to the public address is not made")

// recordingControl wraps rejectNonPublic for TestDialControlTwoAddresses: the address the
// test calls public is refused before any packet is sent (a test must never reach a real
// host), every other address goes through the production check, and each outcome is
// recorded. It is generic like rejectNonPublic, so it assigns to ControlContext.
func recordingControl[C any](ctx context.Context, network, address string, c C) error {
	var err error
	if strings.HasPrefix(address, "93.184.216.34:") {
		err = errTestPublicDial
	} else {
		err = rejectNonPublic(ctx, network, address, c)
	}
	controlLog.Lock()
	controlLog.results[address] = err
	controlLog.Unlock()
	return err
}

// TestDialControlTwoAddresses covers a name that resolves to a public and a loopback
// address: whatever order the dialer tries them in, the loopback one passes through the
// check (and is refused), so the server never sees the request (RESEARCH A3).
func TestDialControlTwoAddresses(t *testing.T) {
	srv, hits := countingServer(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = fmt.Fprint(w, "secret") })
	port := serverPort(t, srv)
	_, res, ok := startDNSStub(t, "93.184.216.34", "127.0.0.1")
	if !ok {
		t.Skip("UDP on 127.0.0.1 is unavailable; TestDialControlCatchesRebinding ran the rewriting-dialer variant")
	}
	controlLog.Lock()
	controlLog.results = map[string]error{}
	controlLog.Unlock()

	d := &net.Dialer{Resolver: res, Timeout: 5 * time.Second}
	d.ControlContext = recordingControl
	tr := &http.Transport{DialContext: d.DialContext, DisableKeepAlives: true}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://two.test:"+strconv.Itoa(port)+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("the request must not succeed")
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("the server was reached %d times", n)
	}
	controlLog.Lock()
	defer controlLog.Unlock()
	loop := "127.0.0.1:" + strconv.Itoa(port)
	got, seen := controlLog.results[loop]
	if !seen {
		t.Fatalf("the loopback address never passed through the dial-time check: %v", controlLog.results)
	}
	if !errors.Is(got, errBlockedDial) {
		t.Fatalf("the loopback address must be refused: %v", got)
	}
	if !errors.Is(controlLog.results["93.184.216.34:"+strconv.Itoa(port)], errTestPublicDial) {
		t.Fatalf("the public address must have been tried too: %v", controlLog.results)
	}
}

// TestDialControlBlocksLiteralsAndMapped dials literal loopback forms straight through the
// default transport, bypassing the pre-check on purpose (a bug in it must not be fatal):
// the dial-time check refuses each, and the server sees nothing. The numeric forms are also
// refused by the pre-check, which canonicalizes them before it looks at the address.
func TestDialControlBlocksLiteralsAndMapped(t *testing.T) {
	srv, hits := countingServer(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = fmt.Fprint(w, "secret") })
	port := strconv.Itoa(serverPort(t, srv))
	client := &http.Client{Transport: newTransport(false), Timeout: 5 * time.Second}

	for _, host := range []string{"127.0.0.1", "[::1]", "[::ffff:127.0.0.1]", "localhost"} {
		t.Run("dial/"+host, func(t *testing.T) {
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+host+":"+port+"/", nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := client.Do(req)
			if err == nil {
				_ = resp.Body.Close()
				t.Fatalf("%s: connected to the loopback server", host)
			}
			if !errors.Is(err, errBlockedDial) {
				t.Fatalf("%s: the dial must be refused by the check, got %v", host, err)
			}
		})
	}
	// Numeric forms that a platform resolver (glibc getaddrinfo, say) turns into 127.0.0.1:
	// the dialer's own lookup is a DNS stub that answers 127.0.0.1 for every name, so the
	// dial-time check is what stops the connection. The pure Go resolver never looks up a name
	// whose labels are all numeric (2130706433, 127.1), so for those the lookup itself fails;
	// 0x7f.1 reaches the stub and the check. No real name is ever looked up.
	if _, res, ok := startDNSStub(t, "127.0.0.1"); ok {
		numeric := &http.Client{Transport: newTransportWith(false, res), Timeout: 5 * time.Second}
		for _, host := range []string{"2130706433", "0x7f.1", "127.1", "0177.0.0.1"} {
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+host+":"+port+"/", nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := numeric.Do(req)
			if err == nil {
				_ = resp.Body.Close()
				t.Errorf("%s: connected to the loopback server", host)
			} else if host == "0x7f.1" && !errors.Is(err, errBlockedDial) {
				t.Errorf("%s: the dial must be refused by the check, got %v", host, err)
			}
		}
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("the server was reached %d times", n)
	}

	for _, raw := range []string{"http://[::ffff:127.0.0.1]/", "http://2130706433/", "http://0x7f.1/", "http://017700000001/", "http://127.1/"} {
		_, err := assertPublicHTTPURL(t.Context(), raw, nil, guardOptions{Resolver: publicResolver(nil)})
		var blocked *blockedError
		if !errors.As(err, &blocked) || !strings.Contains(err.Error(), "is not a public address") {
			t.Errorf("%s must be refused by the pre-check: %v", raw, err)
		}
	}
}

// redirectServer serves the redirect shapes of TestManualRedirectsValidatedPerHop and
// records the paths it was asked for, in order.
type redirectServer struct {
	srv   *httptest.Server
	mu    sync.Mutex
	paths []string
}

func newRedirectServer(t *testing.T) *redirectServer {
	t.Helper()
	rs := &redirectServer{}
	redirect := func(w http.ResponseWriter, status int, location string) {
		w.Header().Set("Location", location)
		w.WriteHeader(status)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/a", func(w http.ResponseWriter, _ *http.Request) { redirect(w, http.StatusFound, "/b") })
	mux.HandleFunc("/b", func(w http.ResponseWriter, _ *http.Request) { _, _ = fmt.Fprint(w, "landed") })
	mux.HandleFunc("/evil", func(w http.ResponseWriter, _ *http.Request) {
		redirect(w, http.StatusMovedPermanently, "http://169.254.169.254/latest/meta-data/")
	})
	mux.HandleFunc("/evil2", func(w http.ResponseWriter, _ *http.Request) { redirect(w, http.StatusFound, "http://2130706433:1/") })
	mux.HandleFunc("/evil3", func(w http.ResponseWriter, _ *http.Request) { redirect(w, http.StatusFound, "http://[::ffff:7f00:1]/") })
	mux.HandleFunc("/evil4", func(w http.ResponseWriter, _ *http.Request) { redirect(w, http.StatusFound, "http://localhost:8080/") })
	mux.HandleFunc("/evil5", func(w http.ResponseWriter, _ *http.Request) { redirect(w, http.StatusFound, "http://priv.example/x") })
	mux.HandleFunc("/loop", func(w http.ResponseWriter, _ *http.Request) { redirect(w, http.StatusFound, "/loop") })
	mux.HandleFunc("/badloc", func(w http.ResponseWriter, _ *http.Request) { redirect(w, http.StatusFound, "http://[::1") })
	mux.HandleFunc("/ftp", func(w http.ResponseWriter, _ *http.Request) { redirect(w, http.StatusFound, "ftp://example.com/x") })
	mux.HandleFunc("/creds", func(w http.ResponseWriter, _ *http.Request) {
		redirect(w, http.StatusFound, "http://user:pw@pub.example/x")
	})
	mux.HandleFunc("/dir/sub/page", func(w http.ResponseWriter, _ *http.Request) { redirect(w, http.StatusFound, "../up?x=1#frag") })
	mux.HandleFunc("/dir/up", func(w http.ResponseWriter, r *http.Request) { _, _ = fmt.Fprint(w, "up:"+r.URL.RawQuery) })
	mux.HandleFunc("/abs", func(w http.ResponseWriter, _ *http.Request) { redirect(w, http.StatusFound, "HTTP://PUB.EXAMPLE:80/B") })
	mux.HandleFunc("/B", func(w http.ResponseWriter, _ *http.Request) { _, _ = fmt.Fprint(w, "upper") })
	mux.HandleFunc("/nolocation", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusFound) })
	mux.HandleFunc("/secret", func(http.ResponseWriter, *http.Request) {})
	rs.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rs.mu.Lock()
		rs.paths = append(rs.paths, r.URL.Path)
		rs.mu.Unlock()
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(rs.srv.Close)
	return rs
}

func (rs *redirectServer) requested() []string {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return append([]string(nil), rs.paths...)
}

func (rs *redirectServer) reset() {
	rs.mu.Lock()
	rs.paths = nil
	rs.mu.Unlock()
}

// TestManualRedirectsValidatedPerHop follows redirects by hand: each Location is resolved
// WHATWG-style and guarded before the next request, so a public page cannot send the fetch
// to the private network. The test transport rewrites every dial to the test server, so the
// pre-check runs on public-looking names while no real host is contacted.
func TestManualRedirectsValidatedPerHop(t *testing.T) {
	rs := newRedirectServer(t)
	addr := rs.srv.Listener.Addr().String()
	d := &net.Dialer{}
	tr := &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return d.DialContext(ctx, network, addr)
		},
		DisableKeepAlives: true,
	}
	resolver := publicResolver(map[string][]string{"pub.example": {"93.184.216.34"}, "priv.example": {"10.0.0.5"}})
	fetch := func(path string, maxRedirects int) (fetched, error) {
		rs.reset()
		return fetchPage(t.Context(), "http://pub.example"+path, fetchOptions{Transport: tr, Resolver: resolver, MaxRedirects: maxRedirects})
	}

	t.Run("a public redirect is followed", func(t *testing.T) {
		page, err := fetch("/a", 5)
		if err != nil {
			t.Fatal(err)
		}
		if string(page.Body) != "landed" || page.FinalURL != "http://pub.example/b" || page.URL != "http://pub.example/a" {
			t.Fatalf("page = %+v body %q", page, page.Body)
		}
		if got := rs.requested(); strings.Join(got, ",") != "/a,/b" {
			t.Fatalf("requests = %v", got)
		}
	})

	t.Run("a relative Location resolves against the URL it came from", func(t *testing.T) {
		page, err := fetch("/dir/sub/page", 5)
		if err != nil {
			t.Fatal(err)
		}
		if page.FinalURL != "http://pub.example/dir/up?x=1#frag" || string(page.Body) != "up:x=1" {
			t.Fatalf("page = %+v body %q", page, page.Body)
		}
	})

	t.Run("an absolute Location is normalized", func(t *testing.T) {
		page, err := fetch("/abs", 5)
		if err != nil {
			t.Fatal(err)
		}
		if page.FinalURL != "http://pub.example/B" || string(page.Body) != "upper" {
			t.Fatalf("page = %+v body %q", page, page.Body)
		}
	})

	blockedCases := []struct{ path, want string }{
		{"/evil", "address 169.254.169.254 is not a public address"},
		{"/evil2", "address 127.0.0.1 is not a public address"},
		{"/evil3", "address ::ffff:7f00:1 is not a public address"},
		{"/evil4", `host "localhost" is not a public address`},
		{"/evil5", `host "priv.example" resolves to a non-public address`}, // IN-A1: no resolved address
		{"/ftp", `scheme "ftp:" is not allowed (http/https only)`},
		{"/creds", "URLs with embedded credentials are not allowed"},
	}
	for _, c := range blockedCases {
		t.Run("blocked "+c.path, func(t *testing.T) {
			_, err := fetch(c.path, 5)
			var blocked *blockedError
			if !errors.As(err, &blocked) {
				t.Fatalf("want a *blockedError, got %T %v", err, err)
			}
			if err.Error() != c.want {
				t.Fatalf("error text: got %q, want %q", err.Error(), c.want)
			}
			// the guard refused the Location before a second request was made
			if got := rs.requested(); len(got) != 1 || got[0] != c.path {
				t.Fatalf("requests = %v, want only %s", got, c.path)
			}
		})
	}

	t.Run("a self-redirect is too many redirects", func(t *testing.T) {
		_, err := fetch("/loop", 5)
		var pe *pageError
		if !errors.As(err, &pe) || err.Error() != "too many redirects (max 5)" {
			t.Fatalf("got %T %v", err, err)
		}
		if n := len(rs.requested()); n != 6 {
			t.Fatalf("%d requests, want 6 (the first and five hops)", n)
		}
	})

	t.Run("maxRedirects 0 follows none", func(t *testing.T) {
		_, err := fetch("/a", 0)
		if err == nil || err.Error() != "too many redirects (max 0)" {
			t.Fatalf("got %v", err)
		}
		if got := rs.requested(); len(got) != 1 {
			t.Fatalf("requests = %v", got)
		}
	})

	t.Run("an unparsable Location is Invalid URL, not a blocked page", func(t *testing.T) {
		_, err := fetch("/badloc", 5)
		var pe *pageError
		var blocked *blockedError
		if !errors.As(err, &pe) || errors.As(err, &blocked) || err.Error() != "Invalid URL" {
			t.Fatalf("got %T %v", err, err)
		}
	})

	t.Run("a redirect with no Location is the final response", func(t *testing.T) {
		page, err := fetch("/nolocation", 5)
		if err == nil || err.Error() != "HTTP 302" || page.Status != http.StatusFound || page.FinalURL != "http://pub.example/nolocation" {
			t.Fatalf("page = %+v err %v", page, err)
		}
	})

	t.Run("an unlisted private target never receives a request", func(t *testing.T) {
		rs.reset()
		_, err := fetchPage(t.Context(), "http://priv.example/secret", fetchOptions{Transport: tr, Resolver: resolver})
		if err == nil {
			t.Fatal("must be refused")
		}
		if got := rs.requested(); len(got) != 0 {
			t.Fatalf("the server saw %v", got)
		}
	})
}

// TestBlockedDialAddr (review WR-A1): the dial-time check refuses the IPv6 ranges that embed
// an IPv4 address or lead to a translator, which the block tables alone let through. The
// pre-check (isBlockedAddr) gives the same answer; the recorded fixtures pin those rows as
// named divergences (guard_test.go).
func TestBlockedDialAddr(t *testing.T) {
	refused := []string{
		"::7f00:1", "::a00:1", "::c0a8:101", "::a9fe:a9fe", // IPv4-compatible ::/96 over a blocked IPv4 address
		"::ffff:0:7f00:1", "::ffff:0:a00:1", // SIIT ::ffff:0:0/96
		"2002:7f00:1::1", "2002:a00:1::", "2002:c0a8:101::5", "2002:a9fe:a9fe::", // 6to4 over a blocked IPv4 address
		"64:ff9b:1::a00:1", "64:ff9b:1::808:808", // local-use NAT64, refused outright
		"2001::1", "2001:0:4136:e378:8000:63bf:3fff:fdd2", // Teredo, refused outright
		"::1", "::", "fe80::1%eth0", "64:ff9b::1", "::ffff:127.0.0.1", "10.0.0.1", // still refused by the block tables
	}
	for _, a := range refused {
		if !isBlockedDialAddr(netip.MustParseAddr(a)) {
			t.Errorf("%s must be refused at dial time", a)
		}
	}
	allowed := []string{
		"93.184.216.34", "2606:4700:4700::1111", "::ffff:8.8.8.8", "::808:808", "::ffff:0:808:808", "2002:808:808::1",
		"2001:db9::1", "2001:4860:4860::8888", "64:ff9c::1",
	}
	for _, a := range allowed {
		if isBlockedDialAddr(netip.MustParseAddr(a)) {
			t.Errorf("%s must be allowed at dial time", a)
		}
	}
	if !isBlockedDialAddr(netip.Addr{}) {
		t.Error("the zero address must be refused")
	}
	// The pre-check reuses the dial-time logic: the embedded forms the block tables allow
	// are refused there too.
	for _, a := range []string{"::7f00:1", "2002:7f00:1::1", "64:ff9b:1::a00:1", "2001::1"} {
		if !isBlockedAddress(a) {
			t.Errorf("the pre-check must refuse %s like the dial-time check (Q27)", a)
		}
	}
	// ... and the dial control, the layer that runs for every connection, refuses them.
	for _, a := range []string{"[::7f00:1]:80", "[2002:7f00:1::1]:80", "[64:ff9b:1::a00:1]:80", "[2001::1]:80", "[::ffff:0:a00:1]:80"} {
		if err := rejectNonPublic(t.Context(), "tcp", a, struct{}{}); !errors.Is(err, errBlockedDial) {
			t.Errorf("rejectNonPublic(%s) = %v, want errBlockedDial", a, err)
		}
	}
	if err := rejectNonPublic(t.Context(), "tcp", "[2002:808:808::1]:443", struct{}{}); err != nil {
		t.Errorf("a 6to4 address over a public IPv4 address must be allowed: %v", err)
	}
}
