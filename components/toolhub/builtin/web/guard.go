package web

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strings"
)

// ---------------------------------------------------------------------------
// SSRF guard: a model-supplied URL must never reach the host's own network (loopback
// services, the Postgres box, cloud metadata endpoints, LAN devices). This file is the
// pre-check; the dial-time half of the defence is transport.go.
// ---------------------------------------------------------------------------

// ipv4Blocked and ipv6Blocked are the block tables as prefixes.
// IPv4: "this network", private, CGNAT, loopback, link-local (incl. 169.254.169.254
// metadata), IETF protocol assignments, TEST-NETs, benchmarking, multicast, reserved and
// broadcast. IPv6: unspecified, loopback, NAT64, discard, documentation, unique-local,
// link-local, site-local and multicast. The lists are separate per family: an
// IPv4-mapped IPv6 address is checked against the IPv4 list (isBlockedTableAddr unmaps it).
//
// The tables have no entry for IPv4-compatible (::a.b.c.d) and 6to4 (2002::/16) addresses
// that embed a private IPv4 address, so the tables alone would let those through. The
// pre-check (isBlockedAddr) therefore decodes the embedded IPv4 address and also refuses
// the translator ranges, the same logic as the dial-time check (isBlockedDialAddr).
var (
	ipv4Blocked = mustPrefixes(
		"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16",
		"172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24", "192.168.0.0/16", "198.18.0.0/15",
		"198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4",
	)
	ipv6Blocked = mustPrefixes(
		"::/128", "::1/128", "64:ff9b::/96", "100::/64", "2001:db8::/32", "fc00::/7", "fe80::/10", "fec0::/10", "ff00::/8",
	)
)

func mustPrefixes(ss ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(ss))
	for i, s := range ss {
		out[i] = netip.MustParsePrefix(s)
	}
	return out
}

func inAny(list []netip.Prefix, a netip.Addr) bool {
	for _, p := range list {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// isBlockedAddr is the netip form of isBlockedAddress, used by the URL pre-check. It is the
// block tables (isBlockedTableAddr) plus the embedded-IPv4 and translator ranges of
// isBlockedDialAddr: the pre-check and the dial-time check give the same answer, so
// ::7f00:1 and 2002:7f00:1:: are refused before any request is made.
func isBlockedAddr(a netip.Addr) bool {
	return isBlockedDialAddr(a)
}

// isBlockedTableAddr checks an address against the block tables only. An IPv4-mapped
// address is checked against the IPv4 list (Unmap), and a zone is dropped first because
// Prefix.Contains never matches an address that has one (a zoned address is still a valid
// literal). An invalid address is blocked.
func isBlockedTableAddr(a netip.Addr) bool {
	if !a.IsValid() {
		return true
	}
	a = a.WithZone("").Unmap()
	if a.Is4() {
		return inAny(ipv4Blocked, a)
	}
	return inAny(ipv6Blocked, a)
}

// isBlockedDialAddr is the stricter check of the dial-time half (transport.go); the
// pre-check (isBlockedAddr) reuses it. It refuses everything isBlockedTableAddr refuses
// (the block tables) and the IPv6 ranges that embed an IPv4 address or lead to a
// translator the tables do not list (review WR-A1):
//   - ::/96 (IPv4-compatible) and ::ffff:0:0/96 (SIIT, IPv4-translated) and 2002::/16
//     (6to4): the embedded IPv4 address is decoded and checked against the IPv4 table, so
//     ::7f00:1 and 2002:7f00:1:: are refused and ::808:808 stays allowed;
//   - 64:ff9b:1::/48 (RFC 8215 local-use NAT64) and 2001::/32 (Teredo): refused outright,
//     because the embedded address is local policy or obfuscated and cannot be decoded.
//
// The connection is about to leave the host, so an address whose real destination is
// ambiguous is refused rather than guessed.
func isBlockedDialAddr(a netip.Addr) bool {
	if isBlockedTableAddr(a) {
		return true
	}
	a = a.WithZone("").Unmap()
	if a.Is4() {
		return false
	}
	b := a.As16()
	switch {
	case isZeroBytes(b[:12]):
		return isBlockedTableAddr(netip.AddrFrom4([4]byte(b[12:16])))
	case isZeroBytes(b[:8]) && b[8] == 0xff && b[9] == 0xff && b[10] == 0 && b[11] == 0:
		// ::ffff:0:a.b.c.d (the bytes 8-9 are ffff, 10-11 zero)
		return isBlockedTableAddr(netip.AddrFrom4([4]byte(b[12:16])))
	case b[0] == 0x20 && b[1] == 0x02:
		return isBlockedTableAddr(netip.AddrFrom4([4]byte(b[2:6])))
	}
	return inAny(dialOnlyBlocked, a)
}

// dialOnlyBlocked are the ranges the dial-time check (and the pre-check) refuses outright
// on top of the block tables (isBlockedDialAddr).
var dialOnlyBlocked = mustPrefixes("64:ff9b:1::/48", "2001::/32")

func isZeroBytes(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

// isBlockedAddress is the string form: unknown input is blocked. It uses the pre-check
// (isBlockedAddr), so the embedded-IPv4 and translator addresses are blocked too.
func isBlockedAddress(s string) bool {
	a, err := netip.ParseAddr(s)
	if err != nil {
		return true
	}
	return isBlockedAddr(a)
}

// dnsFailureText is the model-visible text of a failed name lookup, in the form Node uses
// (`getaddrinfo ENOTFOUND host`, `getaddrinfo EAI_AGAIN host` for a temporary failure). The
// text of a Go *net.DNSError names the local nameserver and sometimes the local source
// address (`lookup h on 10.0.0.2:53: ...`), which must not reach the model or a page that
// steers it (review WR-A3); the full error stays available as the cause for logs. ok is
// false when err is not a DNS error. host names the lookup when the error carries no name.
func dnsFailureText(err error, host string) (string, bool) {
	var de *net.DNSError
	if !errors.As(err, &de) {
		return "", false
	}
	name := de.Name
	if name == "" {
		name = host
	}
	if de.IsNotFound {
		return "getaddrinfo ENOTFOUND " + name, true
	}
	return "getaddrinfo EAI_AGAIN " + name, true
}

// resolveFailureText is the text after `could not resolve host "h": `: the Node-style text
// of a DNS error, and the error text itself for any other error (an injected Resolver
// reports its own).
func resolveFailureText(err error, host string) string {
	if t, ok := dnsFailureText(err, host); ok {
		return t
	}
	return err.Error()
}

// blockedError is a blocked URL: the URL must not be fetched. Its type lets a caller tell
// a blocked URL (never retry) from any other failure.
type blockedError struct{ msg string }

func (e *blockedError) Error() string { return e.msg }

// guardOptions configure the guard: the resolver (nil means net.DefaultResolver)
// and allowPrivateNetwork, which disables the private and loopback address check for
// trusted deployments (the scheme and credential checks still apply).
type guardOptions struct {
	Resolver            Resolver
	AllowPrivateNetwork bool
}

// assertPublicHTTPURL validates a URL before every request and every redirect hop: http(s) only, no embedded credentials, and the host, literal
// or every resolved address, must be a public address. raw is resolved against base when
// base is not nil. The error is a *blockedError when the URL must not be fetched.
//
// A syntactically valid scheme other than http and https is reported as the scheme error
// even when the rest of the URL would not parse, and a trailing dot on localhost
// (localhost., foo.localhost.) is stripped before the name check, so it is refused at once
// rather than left to the resolve-time check.
func assertPublicHTTPURL(ctx context.Context, raw string, base *webURL, o guardOptions) (*webURL, error) { //nolint:unparam // base is kept for redirect resolution; callers resolve redirects first and pass nil today
	if scheme := schemeOf(raw); scheme != "" && scheme != "http" && scheme != "https" {
		return nil, &blockedError{`scheme "` + scheme + `:" is not allowed (http/https only)`}
	}
	u, err := parseURL(raw, base)
	if err != nil {
		return nil, &blockedError{"invalid URL: " + raw}
	}
	if u.hasCredentials() {
		return nil, &blockedError{"URLs with embedded credentials are not allowed"}
	}
	if o.AllowPrivateNetwork {
		return u, nil
	}

	host := u.hostname()
	name := strings.TrimRight(host, ".")
	if name == "localhost" || strings.HasSuffix(name, ".localhost") {
		return nil, &blockedError{`host "` + host + `" is not a public address`}
	}
	if a, perr := netip.ParseAddr(host); perr == nil {
		if isBlockedAddr(a) {
			return nil, &blockedError{"address " + host + " is not a public address"}
		}
		return u, nil
	}

	r := o.Resolver
	if r == nil {
		r = net.DefaultResolver
	}
	addrs, err := r.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, &blockedError{`could not resolve host "` + host + `": ` + resolveFailureText(err, host)}
	}
	if len(addrs) == 0 {
		return nil, &blockedError{`host "` + host + `" did not resolve`}
	}
	// The error names the host only, not the resolved address: the DNS answer is internal
	// detail (often a private address of the deployment) that must not reach the model or a
	// page that steers it.
	for _, a := range addrs {
		if isBlockedAddr(a) {
			return nil, &blockedError{`host "` + host + `" resolves to a non-public address`}
		}
	}
	// fetch re-resolves the host when it connects; the transport re-validates the address
	// it dials (transport.go), which closes the DNS-rebinding window.
	return u, nil
}
