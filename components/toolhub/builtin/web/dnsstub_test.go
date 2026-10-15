package web

import (
	"context"
	"encoding/binary"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
)

// dnsStub is a UDP DNS server on 127.0.0.1 that answers every A query with a configured
// list of addresses and every other query type (AAAA included) with an empty NOERROR
// answer. It exists to play DNS rebinding: a resolver whose Dial goes to the stub gives the
// transport's dialer an answer that differs from what the pre-check resolver said. Nothing
// leaves the machine: the stub and the resolver both live on the loopback interface.
type dnsStub struct {
	conn    net.PacketConn
	ips     []netip.Addr
	queries atomic.Int32
	wg      sync.WaitGroup
}

// startDNSStub starts the stub and returns a resolver for it. The second result is false
// when UDP on 127.0.0.1 is unavailable on this machine; the callers then use the rewriting
// dialer variant (rebindingTransport) instead of skipping.
func startDNSStub(t *testing.T, ips ...string) (*dnsStub, *net.Resolver, bool) {
	t.Helper()
	pc, err := (&net.ListenConfig{}).ListenPacket(t.Context(), "udp", "127.0.0.1:0")
	if err != nil {
		t.Logf("UDP on 127.0.0.1 is unavailable (%v)", err)
		return nil, nil, false
	}
	s := &dnsStub{conn: pc}
	for _, ip := range ips {
		s.ips = append(s.ips, netip.MustParseAddr(ip))
	}
	s.wg.Add(1)
	go s.serve()
	t.Cleanup(func() {
		_ = pc.Close()
		s.wg.Wait()
	})
	addr := pc.LocalAddr().String()
	resolver := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "udp", addr)
	}}
	return s, resolver, true
}

func (s *dnsStub) serve() {
	defer s.wg.Done()
	buf := make([]byte, 512)
	for {
		n, from, err := s.conn.ReadFrom(buf)
		if err != nil {
			return
		}
		s.queries.Add(1)
		if resp := s.answer(buf[:n]); resp != nil {
			_, _ = s.conn.WriteTo(resp, from)
		}
	}
}

// answer builds the response to one query: the header and the question are copied, the
// flags are 0x8180 (response, recursion desired and available, no error) and an A query
// gets one answer record per configured address.
func (s *dnsStub) answer(q []byte) []byte {
	if len(q) < 12 {
		return nil
	}
	i := 12
	for i < len(q) && q[i] != 0 {
		i += int(q[i]) + 1
	}
	qEnd := i + 1 + 4
	if qEnd > len(q) {
		return nil
	}
	qtype := binary.BigEndian.Uint16(q[i+1:])
	resp := append([]byte(nil), q[:qEnd]...)
	resp[2], resp[3] = 0x81, 0x80
	var answers []netip.Addr
	if qtype == 1 {
		for _, ip := range s.ips {
			if ip.Is4() {
				answers = append(answers, ip)
			}
		}
	}
	binary.BigEndian.PutUint16(resp[6:], uint16(len(answers)))
	for _, ip := range answers {
		// pointer to the question name, type A, class IN, TTL 5, four address bytes
		resp = append(resp, 0xC0, 0x0C, 0, 1, 0, 1, 0, 0, 0, 5, 0, 4)
		resp = append(resp, ip.AsSlice()...)
	}
	return resp
}
