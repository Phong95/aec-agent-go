package web

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"time"
)

// errBlockedDial is the sentinel of a dial the transport refused: the connection was about
// to go to an address that is not public. The error a dial returns wraps it, so
// errors.Is(err, errBlockedDial) tells a refused dial from any other network failure.
var errBlockedDial = errors.New("blocked: address is not a public address")

// dialBlockedError is the dial-time refusal. It carries the address that was about to be
// dialed, so the page error can name it like the pre-check does.
type dialBlockedError struct{ addr string }

func (e *dialBlockedError) Error() string { return errBlockedDial.Error() + ": " + e.addr }
func (e *dialBlockedError) Unwrap() error { return errBlockedDial }

// rejectNonPublic is the dial-time re-check: it runs for every ip:port the dialer is about
// to connect to, after the name has been resolved, so it sees the address the connection
// really uses. That catches DNS rebinding (a name that resolved to a public address for the
// pre-check and to a private one for the connect), a name that resolves to several
// addresses, and any host form the pre-check canonicalization missed. It is stateless: the
// transport installs it only when allowPrivateNetwork is off.
//
// It uses isBlockedDialAddr, the block tables plus the IPv6 ranges that embed an IPv4
// address (::/96, 6to4) or lead to a translator (local-use NAT64, Teredo), which the tables
// alone let through; the pre-check reuses the same logic.
//
// It is generic in the connection parameter on purpose. net.Dialer.ControlContext takes a
// syscall.RawConn, and the package must not import syscall (the host-access guard forbids
// it); assigning the uninstantiated generic function to the field makes the compiler infer
// C = syscall.RawConn, so the source never names the type.
func rejectNonPublic[C any](_ context.Context, _, address string, _ C) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return &dialBlockedError{addr: address}
	}
	if isBlockedDialAddr(ap.Addr()) {
		return &dialBlockedError{addr: ap.Addr().String()}
	}
	return nil
}

// newTransport builds the default transport of the web tools: no proxy (Node's fetch reads
// no proxy variable, and a proxy would also move the connection past the dial-time check),
// the default client TLS configuration, and unless allowPrivate is set a dial-time check of
// the real ip:port. Keep-alives are off: the SDK never disposes its tools, so a pooled
// connection would linger, and a page is fetched once. The per-page deadline of fetchPage,
// not a transport timeout, bounds the response headers and the body.
func newTransport(allowPrivate bool) *http.Transport {
	return newTransportWith(allowPrivate, nil)
}

// newTransportWith is newTransport with the resolver the dialer uses (nil means the
// default). The tests give the dialer a resolver of their own, to play DNS rebinding.
func newTransportWith(allowPrivate bool, resolver *net.Resolver) *http.Transport {
	d := &net.Dialer{Timeout: 10 * time.Second, Resolver: resolver}
	if !allowPrivate {
		d.ControlContext = rejectNonPublic
	}
	return &http.Transport{
		Proxy:               nil,
		DialContext:         d.DialContext,
		ForceAttemptHTTP2:   true,
		DisableKeepAlives:   true,
		TLSHandshakeTimeout: 10 * time.Second,
	}
}
