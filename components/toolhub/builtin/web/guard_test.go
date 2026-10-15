package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/Phong95/aec-agent-go/internal/wiretest"
)

// fakeResolver is the injected test lookup: names listed in the map resolve to
// the listed addresses (a nil list is a lookup failure, an empty one resolves to nothing),
// every other name resolves to one public address.
type fakeResolver struct{ m map[string][]string }

func (f fakeResolver) LookupNetIP(_ context.Context, network, host string) ([]netip.Addr, error) {
	if network != "ip" {
		return nil, errors.New("unexpected network " + network)
	}
	list, ok := f.m[host]
	if !ok {
		return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
	}
	if list == nil {
		return nil, errors.New("getaddrinfo ENOTFOUND " + host)
	}
	out := make([]netip.Addr, 0, len(list))
	for _, s := range list {
		out = append(out, netip.MustParseAddr(s))
	}
	return out, nil
}

// publicResolver is the permissive test lookup: every name resolves to a public address
// unless listed.
func publicResolver(m map[string][]string) fakeResolver { return fakeResolver{m: m} }

// guardFixture is builtin-network-guard.json, recorded from a reference assertPublicHttpUrl
// and isBlockedAddress by tools/wiregen/builtin/network_guard.cases.mjs.
type guardFixture struct {
	Lookups map[string][]string `json:"lookups"`
	URLs    []guardURLRow       `json:"urls"`
	Address []struct {
		Address string `json:"address"`
		Blocked bool   `json:"blocked"`
	} `json:"addresses"`
}

type guardURLRow struct {
	Raw                 string `json:"raw"`
	AllowPrivateNetwork bool   `json:"allowPrivateNetwork"`
	Result              struct {
		Href    string `json:"href"`
		Error   string `json:"error"`
		Blocked bool   `json:"blocked"`
	} `json:"result"`
}

func loadGuardFixture(t *testing.T) guardFixture {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(wiretest.Load(t, "builtin-network-guard.json")))
	dec.DisallowUnknownFields()
	var fx guardFixture
	if err := dec.Decode(&fx); err != nil {
		t.Fatalf("decode builtin-network-guard: %v", err)
	}
	if len(fx.URLs) < 100 || len(fx.Address) < 60 {
		t.Fatalf("fixture too small: %d urls, %d addresses", len(fx.URLs), len(fx.Address))
	}
	return fx
}

// TestBlockedAddress is the block and allow vectors (14 and 4), plus the shapes the block
// tables cover and the embedded-IPv4 forms they do not.
func TestBlockedAddress(t *testing.T) {
	block := []string{
		"127.0.0.1", "10.1.2.3", "172.16.0.1", "192.168.1.1", "169.254.169.254", "100.64.0.1",
		"0.0.0.0", "224.0.0.1", "::1", "::", "fe80::1", "fd00::1", "::ffff:127.0.0.1", "::ffff:7f00:1",
		// zoned link-local (Prefix.Contains alone never matches a zoned address), mapped private,
		// NAT64, broadcast, site-local, multicast, benchmarking, IETF assignments, discard
		"fe80::1%eth0", "::ffff:10.0.0.1", "::ffff:a00:1", "64:ff9b::1", "64:ff9b::808:808", "255.255.255.255",
		"fec0::1", "ff02::1", "198.18.0.1", "198.19.255.255", "192.0.0.8", "100::", "2001:db8::1", "fc00::1",
		// IPv4-compatible and 6to4 forms that embed a private IPv4 address have no entry in
		// the block tables; the pre-check decodes the embedded address and refuses them, like
		// the dial-time check.
		"::7f00:1", "2002:7f00:1::1",
	}
	allow := []string{
		"93.184.216.34", "8.8.8.8", "2606:4700:4700::1111", "::ffff:8.8.8.8",
		"172.32.0.1", "172.15.255.255", "198.20.0.0", "100:0:0:1::", "2001:db9::1",
		// The embedded forms over a public IPv4 address stay allowed.
		"::808:808", "2002:808:808::1",
	}
	for _, a := range block {
		t.Run("block/"+a, func(t *testing.T) {
			if !isBlockedAddress(a) {
				t.Fatalf("%s must be blocked", a)
			}
		})
	}
	for _, a := range allow {
		t.Run("allow/"+a, func(t *testing.T) {
			if isBlockedAddress(a) {
				t.Fatalf("%s must be allowed", a)
			}
		})
	}
	// An address that does not parse is blocked.
	for _, a := range []string{"", "example.com", "1.2.3", "::g", "999.1.1.1", "01.1.1.1"} {
		if !isBlockedAddress(a) {
			t.Errorf("unparsable input %q must be blocked", a)
		}
	}
	if !isBlockedAddr(netip.Addr{}) {
		t.Error("the zero netip.Addr must be blocked")
	}
}

// TestAssertPublicHTTPURL_Rejects is the 10 rejected URLs: each is a *blockedError with the
// expected text.
func TestAssertPublicHTTPURL_Rejects(t *testing.T) {
	cases := []struct{ url, want string }{
		{"file:///etc/passwd", `scheme "file:" is not allowed (http/https only)`},
		{"ftp://example.com/x", `scheme "ftp:" is not allowed (http/https only)`},
		{"gopher://example.com", `scheme "gopher:" is not allowed (http/https only)`},
		{"http://localhost:5432/", `host "localhost" is not a public address`},
		{"http://api.localhost/", `host "api.localhost" is not a public address`},
		{"http://127.0.0.1/", "address 127.0.0.1 is not a public address"},
		{"http://[::1]/", "address ::1 is not a public address"},
		{"http://169.254.169.254/latest/meta-data/", "address 169.254.169.254 is not a public address"},
		{"http://user:pass@example.com/", "URLs with embedded credentials are not allowed"},
		{"not a url", "invalid URL: not a url"},
	}
	for _, c := range cases {
		t.Run(c.url, func(t *testing.T) {
			_, err := assertPublicHTTPURL(t.Context(), c.url, nil, guardOptions{Resolver: publicResolver(nil)})
			var blocked *blockedError
			if !errors.As(err, &blocked) {
				t.Fatalf("want a *blockedError, got %T %v", err, err)
			}
			if err.Error() != c.want {
				t.Fatalf("error text: got %q, want %q", err.Error(), c.want)
			}
		})
	}
}

func TestAssertPublicHTTPURL_AnyResolvedPrivate(t *testing.T) {
	r := publicResolver(map[string][]string{"evil.example": {"93.184.216.34", "10.0.0.5"}})
	_, err := assertPublicHTTPURL(t.Context(), "https://evil.example/", nil, guardOptions{Resolver: r})
	// Review IN-A1: the error names the host only, never the resolved address, so the text
	// is no oracle for the internal DNS answer.
	if err == nil || strings.Contains(err.Error(), "10.0.0.5") {
		t.Fatalf("a hostname with a private resolved address must be rejected without naming it: %v", err)
	}
	if want := `host "evil.example" resolves to a non-public address`; err.Error() != want {
		t.Fatalf("error text: got %q, want %q", err.Error(), want)
	}
}

func TestAssertPublicHTTPURL_AcceptsPublicHostname(t *testing.T) {
	u, err := assertPublicHTTPURL(t.Context(), "https://example.com/a?b=1", nil, guardOptions{Resolver: publicResolver(nil)})
	if err != nil {
		t.Fatalf("a public hostname must be accepted: %v", err)
	}
	if u.hostname() != "example.com" || u.String() != "https://example.com/a?b=1" {
		t.Fatalf("hostname %q, href %q", u.hostname(), u.String())
	}
}

func TestAssertPublicHTTPURL_AllowPrivateNetwork(t *testing.T) {
	o := guardOptions{AllowPrivateNetwork: true}
	if u, err := assertPublicHTTPURL(t.Context(), "http://127.0.0.1/", nil, o); err != nil || u.String() != "http://127.0.0.1/" {
		t.Fatalf("allowPrivateNetwork must skip the address check: %v %v", u, err)
	}
	_, err := assertPublicHTTPURL(t.Context(), "file:///x", nil, o)
	var blocked *blockedError
	if !errors.As(err, &blocked) {
		t.Fatalf("allowPrivateNetwork must not skip the scheme check: %v", err)
	}
	if _, err := assertPublicHTTPURL(t.Context(), "http://u:p@127.0.0.1/", nil, o); err == nil || err.Error() != "URLs with embedded credentials are not allowed" {
		t.Fatalf("allowPrivateNetwork must not skip the credential check: %v", err)
	}
}

// TestAssertPublicHTTPURL_ResolveErrors are the resolve-time texts: a failed lookup, no
// address, and a resolver that is asked for "ip" with the host name.
func TestAssertPublicHTTPURL_ResolveErrors(t *testing.T) {
	r := publicResolver(map[string][]string{"nx.example": nil, "empty.example": {}})
	for _, c := range []struct{ url, want string }{
		{"http://nx.example/", `could not resolve host "nx.example": getaddrinfo ENOTFOUND nx.example`},
		{"http://empty.example/", `host "empty.example" did not resolve`},
	} {
		_, err := assertPublicHTTPURL(t.Context(), c.url, nil, guardOptions{Resolver: r})
		if err == nil || err.Error() != c.want {
			t.Errorf("%s: got %v, want %q", c.url, err, c.want)
		}
	}
}

// divergentURLRows are the rows of the recorded URL corpus where the guard deliberately
// differs from the recorded result. Each one is asserted to have the result below, so a
// difference that is fixed, or a new one, fails the test.
//   - an internationalized host: there is no x/net/idna, so the guard refuses it as an
//     invalid URL instead of converting it to Punycode;
//   - localhost with a trailing dot: the guard strips trailing dots first and refuses it
//     at the name check;
//   - an IPv4-compatible or 6to4 literal over a private IPv4 address: the guard decodes
//     the embedded address and refuses it at the pre-check.
//
// Every recorded error of the form `resolves to non-public address X` is compared as the
// text `resolves to a non-public address` (goGuardText, review IN-A1).
func divergentURLRows() map[string]string {
	return map[string]string{
		"http://[::7f00:1]/":                           "address ::7f00:1 is not a public address",
		"http://[2002:7f00:1::1]/":                     "address 2002:7f00:1::1 is not a public address",
		"http://[2002:a00:1::]/":                       "address 2002:a00:1:: is not a public address",
		"http://ex" + string(rune(0xe4)) + "mple.com/": "invalid URL: http://ex" + string(rune(0xe4)) + "mple.com/",
		"http://localhost./":                           `host "localhost." is not a public address`,
		"http://foo.localhost./":                       `host "foo.localhost." is not a public address`,
		"http://localhost%2e/":                         `host "localhost." is not a public address`,
		"http://LocalHost./":                           `host "localhost." is not a public address`,
	}
}

// TestURLCorpus replays the recorded URL corpus (a reference assertPublicHttpUrl over an
// injected lookup) through assertPublicHTTPURL: the result is the same href or the same
// error text and kind on every row except the pinned divergent ones.
func TestURLCorpus(t *testing.T) {
	fx := loadGuardFixture(t)
	resolver := publicResolver(fx.Lookups)
	pinned := divergentURLRows()
	seen := map[string]bool{}
	for _, row := range fx.URLs {
		u, err := assertPublicHTTPURL(t.Context(), row.Raw, nil, guardOptions{Resolver: resolver, AllowPrivateNetwork: row.AllowPrivateNetwork})
		var gotHref, gotErr string
		if err != nil {
			gotErr = err.Error()
			var blocked *blockedError
			if !errors.As(err, &blocked) {
				t.Errorf("%q: error %T is not a *blockedError", row.Raw, err)
			}
		} else {
			gotHref = u.String()
		}
		if want, ok := pinned[row.Raw]; ok && !row.AllowPrivateNetwork {
			seen[row.Raw] = true
			if gotErr != want {
				t.Errorf("%q: pinned divergent row: Go gave href %q error %q, want error %q", row.Raw, gotHref, gotErr, want)
			}
			if row.Result.Error == want && row.Result.Href == "" {
				t.Errorf("%q: the TS result equals the Go result now; drop the row from divergentURLRows", row.Raw)
			}
			continue
		}
		if gotHref != row.Result.Href || gotErr != goGuardText(row.Result.Error) {
			t.Errorf("%q (allow %v): TS gave href %q error %q, Go gave href %q error %q", row.Raw, row.AllowPrivateNetwork, row.Result.Href, row.Result.Error, gotHref, gotErr)
		}
	}
	for raw := range pinned {
		if !seen[raw] {
			t.Errorf("pinned divergent row %q is not in the recorded corpus", raw)
		}
	}
}

// tsResolvedAddrText matches the recorded tail that names the resolved address.
var tsResolvedAddrText = regexp.MustCompile(`resolves to non-public address [^"\\]+`)

// goGuardText is the guard's form of a recorded guard error text: the guard names the host
// only, not the address it resolved to (review IN-A1), so `resolves to non-public
// address X` becomes `resolves to a non-public address`. Any other text is unchanged.
func goGuardText(ts string) string {
	return tsResolvedAddrText.ReplaceAllString(ts, "resolves to a non-public address")
}

// divergentAddresses are the recorded addresses the pre-check refuses although the
// recording allows them: IPv4-compatible (::/96) and 6to4 (2002::/16) addresses whose
// embedded IPv4 address is in the IPv4 table. ::2 embeds 0.0.0.2 ("this network").
var divergentAddresses = map[string]bool{
	"::7f00:1": true, "::a00:1": true, "2002:7f00:1::1": true, "2002:a00:1::": true, "::2": true,
}

// TestBlockedAddressCorpus replays isBlockedAddress over the recorded addresses; the
// embedded-IPv4 rows are pinned to the expected answer.
func TestBlockedAddressCorpus(t *testing.T) {
	fx := loadGuardFixture(t)
	for _, row := range fx.Address {
		if want, ok := divergentAddresses[row.Address]; ok {
			if row.Blocked == want {
				t.Errorf("%q: the TS answer equals the Go answer now; drop it from divergentAddresses", row.Address)
			}
			if got := isBlockedAddress(row.Address); got != want {
				t.Errorf("isBlockedAddress(%q): Go %v, want %v (Q27)", row.Address, got, want)
			}
			continue
		}
		if got := isBlockedAddress(row.Address); got != row.Blocked {
			t.Errorf("isBlockedAddress(%q): TS %v, Go %v", row.Address, row.Blocked, got)
		}
	}
}

// TestResolveFailureTextHidesTheResolver (review WR-A3): the Go resolver's own error text names
// the local nameserver and source address; the model-visible page error carries the Node-style
// cause instead (getaddrinfo ENOTFOUND / EAI_AGAIN). An injected resolver's own
// error text is kept (the recorded failed-lookup row uses one).
func TestResolveFailureTextHidesTheResolver(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"not found", &net.DNSError{Err: "no such host", Name: "gone.example", Server: "10.0.0.2:53", IsNotFound: true}, "getaddrinfo ENOTFOUND gone.example"},
		{"misbehaving", &net.DNSError{Err: "server misbehaving", Name: "h.example", Server: "127.0.0.53:53"}, "getaddrinfo EAI_AGAIN h.example"},
		{"timeout", fmt.Errorf("wrapped: %w", &net.DNSError{Err: "read udp 10.1.2.3:41234->10.0.0.2:53: i/o timeout", Name: "t.example", Server: "10.0.0.2:53", IsTimeout: true}), "getaddrinfo EAI_AGAIN t.example"},
		{"nameless", &net.DNSError{Err: "boom", Server: "10.0.0.2:53"}, "getaddrinfo EAI_AGAIN lookup.example"},
		{"injected", errors.New("getaddrinfo ENOTFOUND lookup.example"), "getaddrinfo ENOTFOUND lookup.example"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := errResolver{err: c.err}
			_, err := assertPublicHTTPURL(t.Context(), "http://lookup.example/", nil, guardOptions{Resolver: r})
			var blocked *blockedError
			if !errors.As(err, &blocked) {
				t.Fatalf("want a *blockedError, got %T %v", err, err)
			}
			if want := `could not resolve host "lookup.example": ` + c.want; err.Error() != want {
				t.Fatalf("error text: got %q, want %q", err.Error(), want)
			}
			for _, leak := range []string{"10.0.0.2", "127.0.0.53", "10.1.2.3", ":53", "no such host", "misbehaving"} {
				if strings.Contains(err.Error(), leak) {
					t.Errorf("the page error leaks resolver detail %q: %q", leak, err.Error())
				}
			}
		})
	}
	// errorText (the dial and request phase) hides the resolver the same way.
	de := &net.DNSError{Err: "no such host", Name: "x.example", Server: "10.0.0.2:53", IsNotFound: true}
	if got := errorText(&url.Error{Op: "Get", URL: "https://x.example/", Err: &net.OpError{Op: "dial", Err: de}}); got != "getaddrinfo ENOTFOUND x.example" {
		t.Errorf("errorText = %q", got)
	}
}

type errResolver struct{ err error }

func (r errResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	return nil, r.err
}
