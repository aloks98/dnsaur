package zones_test

import (
	"context"
	"errors"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/aloks98/dnsaur/internal/dnssrv"
	"github.com/aloks98/dnsaur/internal/zones"
	"github.com/miekg/dns"
)

// notifyResponder answers NOTIFY with a fixed rcode and records what arrived.
type notifyResponder struct {
	addr     string
	received atomic.Int64
	rcode    int
	// silent drops every request, standing in for a target that is up but
	// not answering.
	silent bool
	last   atomic.Value // *dns.Msg
}

func newNotifyResponder(t *testing.T, rcode int, silent bool) *notifyResponder {
	t.Helper()
	r := &notifyResponder{rcode: rcode, silent: silent}
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &dns.Server{PacketConn: pc, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, m *dns.Msg) {
		r.received.Add(1)
		r.last.Store(m)
		if r.silent {
			return
		}
		reply := new(dns.Msg)
		reply.SetRcode(m, r.rcode)
		_ = w.WriteMsg(reply)
	})}
	r.addr = pc.LocalAddr().String()
	go func() { _ = srv.ActivateAndServe() }()
	t.Cleanup(func() { _ = srv.Shutdown() })
	return r
}

// newTSIGNotifyResponder answers only a NOTIFY that verifies against keys,
// REFUSED otherwise — mirrors probe_test.go's newTSIGSOAServer, the pattern
// this package already uses to prove a client signs rather than merely
// carries a TsigProvider that never gets exercised. signReply controls
// whether a request that verified gets a signed reply back — RFC 8945 §5.4
// requires it — mirroring transferserver.go's signIfVerified. The tests that
// pass true and false for it are what prove Send checks this on the way in,
// not merely produces it on the way out.
func newTSIGNotifyResponder(t *testing.T, keys dnssrv.TSIGKeys, signReply bool) *notifyResponder {
	t.Helper()
	r := &notifyResponder{rcode: dns.RcodeSuccess}
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &dns.Server{
		PacketConn:   pc,
		TsigProvider: dnssrv.NewTSIGProvider(keys),
		Handler: dns.HandlerFunc(func(w dns.ResponseWriter, m *dns.Msg) {
			r.received.Add(1)
			r.last.Store(m)
			reply := new(dns.Msg)
			req := m.IsTsig()
			verified := req != nil && w.TsigStatus() == nil
			if !verified {
				reply.SetRcode(m, dns.RcodeRefused)
				_ = w.WriteMsg(reply)
				return
			}
			reply.SetRcode(m, dns.RcodeSuccess)
			if signReply {
				// The stub carries no MAC yet — WriteMsg computes it via the
				// server's own TsigProvider, set above, exactly as
				// transferserver.go's signIfVerified relies on.
				reply.Extra = append(reply.Extra, dnssrv.ReplyTSIG(reply, req.Hdr.Name, req))
			}
			_ = w.WriteMsg(reply)
		}),
	}
	r.addr = pc.LocalAddr().String()
	go func() { _ = srv.ActivateAndServe() }()
	t.Cleanup(func() { _ = srv.Shutdown() })
	return r
}

// freeUDPPort finds a currently-free UDP port on 127.0.0.1, by binding and
// immediately releasing it — the same trick TestSendReportsPortUnreachableDistinctly
// already uses for a single address. Used by the fan-out tests, which need
// two loopback addresses sharing one port number (a NotifyTarget carries one
// Port applied to every address a hostname resolves to).
func freeUDPPort(t *testing.T) int {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, portStr, _ := net.SplitHostPort(pc.LocalAddr().String())
	_ = pc.Close()
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatal(err)
	}
	return port
}

// fakeAResolver returns a *net.Resolver that answers only an A query for
// host, with ips in order, and NOERROR/no-records for anything else —
// notably AAAA, which LookupNetIP queries in parallel with A. A real
// resolver gives a test no control over which of a hostname's addresses
// come back or in what order; this does, which is the only way to exercise
// Send's multi-address fan-out deterministically.
func fakeAResolver(t *testing.T, host string, ips []net.IP) *net.Resolver {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &dns.Server{PacketConn: pc, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, m *dns.Msg) {
		reply := new(dns.Msg)
		reply.SetReply(m)
		reply.Authoritative = true
		if len(m.Question) == 1 && m.Question[0].Qtype == dns.TypeA &&
			strings.EqualFold(m.Question[0].Name, dns.Fqdn(host)) {
			for _, ip := range ips {
				reply.Answer = append(reply.Answer, &dns.A{
					Hdr: dns.RR_Header{Name: dns.Fqdn(host), Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 5},
					A:   ip,
				})
			}
		}
		_ = w.WriteMsg(reply)
	})}
	addr := pc.LocalAddr().String()
	go func() { _ = srv.ActivateAndServe() }()
	t.Cleanup(func() { _ = srv.Shutdown() })
	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		},
	}
}

func targetFor(t *testing.T, addr string) zones.NotifyTarget {
	t.Helper()
	ts, err := zones.ParseNotifyTo(addr)
	if err != nil {
		t.Fatalf("ParseNotifyTo(%q): %v", addr, err)
	}
	return ts[0]
}

// The message is a NOTIFY: opcode 4, AA set, one SOA question for the zone.
func TestSendBuildsANotify(t *testing.T) {
	r := newNotifyResponder(t, dns.RcodeSuccess, false)
	s := zones.NewUDPSenderForTest()

	if err := s.Send(context.Background(), targetFor(t, r.addr), notifyApex, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	m, _ := r.last.Load().(*dns.Msg)
	if m == nil {
		t.Fatal("nothing arrived")
	}
	if m.Opcode != dns.OpcodeNotify {
		t.Errorf("opcode = %d, want %d (NOTIFY)", m.Opcode, dns.OpcodeNotify)
	}
	if !m.Authoritative {
		t.Error("AA not set")
	}
	if len(m.Question) != 1 || m.Question[0].Qtype != dns.TypeSOA ||
		m.Question[0].Name != dns.Fqdn(notifyApex) {
		t.Errorf("question = %v, want one SOA question for %s", m.Question, notifyApex)
	}
}

// RFC 1996 §3.6 and §4.8: a response ends the round whatever its rcode. A
// REFUSED will not become a NOERROR on retransmission.
func TestSendTreatsAnyResponseAsDelivered(t *testing.T) {
	for _, rcode := range []int{dns.RcodeSuccess, dns.RcodeRefused, dns.RcodeNotAuth, dns.RcodeServerFailure} {
		t.Run(dns.RcodeToString[rcode], func(t *testing.T) {
			r := newNotifyResponder(t, rcode, false)
			s := zones.NewUDPSenderForTest()
			err := s.Send(context.Background(), targetFor(t, r.addr), notifyApex, nil)
			if rcode == dns.RcodeSuccess {
				if err != nil {
					t.Fatalf("Send = %v, want nil", err)
				}
				return
			}
			// Delivered, but not satisfactory: the rcode is reported so it
			// reaches last_error and the screen, while the round still ends.
			if err == nil {
				t.Fatalf("Send = nil, want the rcode reported")
			}
			if !errors.Is(err, zones.ErrNotifyDelivered) {
				t.Errorf("error %v does not wrap ErrNotifyDelivered, so the "+
					"round would retransmit a refusal", err)
			}
			if !strings.Contains(err.Error(), dns.RcodeToString[rcode]) {
				t.Errorf("error %q does not name the rcode", err)
			}
		})
	}
}

// A target that is up but silent is a timeout, which does retry.
func TestSendTimesOutOnASilentTarget(t *testing.T) {
	r := newNotifyResponder(t, 0, true)
	s := zones.NewUDPSenderForTest()
	err := s.Send(context.Background(), targetFor(t, r.addr), notifyApex, nil)
	if err == nil {
		t.Fatal("Send = nil against a silent target")
	}
	if errors.Is(err, zones.ErrNotifyDelivered) {
		t.Error("a timeout was treated as delivered")
	}
	if errors.Is(err, zones.ErrNotifyUnreachable) {
		t.Error("a timeout was treated as unreachable")
	}
}

// RFC 1996 §3.6's second stop condition. Nothing is listening on a closed
// port, so the kernel returns ECONNREFUSED — positive evidence, distinct
// from a timeout, and worth reporting as its own case rather than burning
// four more attempts on it.
func TestSendReportsPortUnreachableDistinctly(t *testing.T) {
	// Bind and immediately release, so the port is almost certainly closed.
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := pc.LocalAddr().String()
	_ = pc.Close()

	s := zones.NewUDPSenderForTest()
	err = s.Send(context.Background(), targetFor(t, addr), notifyApex, nil)
	if err == nil {
		t.Fatal("Send = nil against a closed port")
	}
	if !errors.Is(err, zones.ErrNotifyUnreachable) {
		t.Skipf("the platform did not surface ICMP port unreachable: %v", err)
	}
}

// §3.6 requires the response to match the request's query ID and QNAME. A
// connected socket covers the source address and port; these two do not come
// for free, and without them an off-path response with a guessed ID ends a
// round that never landed.
func TestSendRejectsAMismatchedResponse(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	var gotRequest atomic.Bool
	// A responder that answers with the wrong ID.
	go func() {
		buf := make([]byte, 512)
		n, from, err := pc.ReadFrom(buf)
		if err != nil {
			return
		}
		m := new(dns.Msg)
		if m.Unpack(buf[:n]) != nil {
			return
		}
		gotRequest.Store(true)
		reply := new(dns.Msg)
		reply.SetReply(m)
		reply.Id = m.Id + 1 // the mismatch
		out, _ := reply.Pack()
		_, _ = pc.WriteTo(out, from)
	}()

	s := zones.NewUDPSenderForTest()
	if err := s.Send(context.Background(), targetFor(t, pc.LocalAddr().String()), notifyApex, nil); err == nil {
		t.Fatal("Send accepted a response whose ID did not match")
	}
	if !gotRequest.Load() {
		t.Fatal("the fixture never received the request, so this proves nothing about ID matching")
	}
}

// §3.6 also requires the response's QNAME to match. A correct ID answering
// about the wrong zone is just as much a response that never landed, and
// without this check on top of the ID check a confused or off-path reply
// naming a different zone would end this round.
func TestSendRejectsAResponseForTheWrongQuestion(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	var gotRequest atomic.Bool
	// A responder that answers with the right ID but the wrong question.
	go func() {
		buf := make([]byte, 512)
		n, from, err := pc.ReadFrom(buf)
		if err != nil {
			return
		}
		m := new(dns.Msg)
		if m.Unpack(buf[:n]) != nil {
			return
		}
		gotRequest.Store(true)
		reply := new(dns.Msg)
		reply.SetReply(m)
		reply.Question[0].Name = dns.Fqdn("not-" + notifyApex) // the mismatch
		out, _ := reply.Pack()
		_, _ = pc.WriteTo(out, from)
	}()

	s := zones.NewUDPSenderForTest()
	if err := s.Send(context.Background(), targetFor(t, pc.LocalAddr().String()), notifyApex, nil); err == nil {
		t.Fatal("Send accepted a response whose question did not match")
	}
	if !gotRequest.Load() {
		t.Fatal("the fixture never received the request, so this proves nothing about QNAME matching")
	}
}

// A hostname target is resolved at send time, so a target that moves is
// followed — the reason ParseNotifyTo does not resolve.
func TestSendResolvesAHostnameAtSendTime(t *testing.T) {
	// localhost resolves without a network, and is enough to prove the
	// resolution step happens here rather than at parse time.
	r := newNotifyResponder(t, dns.RcodeSuccess, false)
	_, port, _ := net.SplitHostPort(r.addr)
	s := zones.NewUDPSenderForTest()
	if err := s.Send(context.Background(), targetFor(t, "localhost:"+port), notifyApex, nil); err != nil {
		t.Fatalf("Send to a hostname target: %v", err)
	}
	if r.received.Load() == 0 {
		t.Error("nothing arrived at the resolved address")
	}
}

// Send is supposed to reuse Transferrer.fetch's signing mechanism rather
// than a second HMAC implementation (notifysend.go's package comment). This
// is the proof: a responder that verifies the request and REFUSES anything
// that does not, exactly the pairing probe_test.go and transfer_test.go use
// for their own signing client sides.
func TestSendSignsWithTSIGWhenAKeyIsGiven(t *testing.T) {
	st := openTestStore(t)
	keyID := storeTSIGKey(t, st, "notify-key."+notifyApex)
	key, found, err := st.TSIGKeys().Get(context.Background(), keyID)
	if err != nil || !found {
		t.Fatalf("Get(%d): found=%v err=%v", keyID, found, err)
	}
	r := newTSIGNotifyResponder(t, st.TSIGKeys(), true)

	s := zones.NewUDPSenderForTest()
	if err := s.Send(context.Background(), targetFor(t, r.addr), notifyApex, &key); err != nil {
		t.Fatalf("signed Send: %v", err)
	}
}

// The same responder, unsigned. This is what makes the test above mean
// something: without it, a Send that silently carried no TSIG would pass
// against a responder that happened not to care.
func TestSendAgainstATSIGResponderFailsUnsigned(t *testing.T) {
	st := openTestStore(t)
	r := newTSIGNotifyResponder(t, st.TSIGKeys(), true)

	s := zones.NewUDPSenderForTest()
	if err := s.Send(context.Background(), targetFor(t, r.addr), notifyApex, nil); err == nil {
		t.Fatal("unsigned Send succeeded against a responder that requires TSIG")
	}
	if r.received.Load() == 0 {
		t.Fatal("the responder never received the request, so this proves nothing about signing")
	}
}

// RFC 8945 §5.4: a signed request's response must be signed too. miekg's
// ReadMsg only verifies a TSIG that is present (client.go:267, "if t :=
// m.IsTsig(); t != nil") — a reply carrying none skips verification
// entirely. So without a check here, a spoofed *unsigned* reply that merely
// guesses the ID and QNAME would end the round exactly as an authentic
// signed one would, and TSIG would contribute nothing at all to the
// response path.
func TestSendRejectsAnUnsignedResponseToASignedNotify(t *testing.T) {
	st := openTestStore(t)
	keyID := storeTSIGKey(t, st, "notify-key."+notifyApex)
	key, found, err := st.TSIGKeys().Get(context.Background(), keyID)
	if err != nil || !found {
		t.Fatalf("Get(%d): found=%v err=%v", keyID, found, err)
	}
	// Verifies the request correctly, but never signs its own reply.
	r := newTSIGNotifyResponder(t, st.TSIGKeys(), false)

	s := zones.NewUDPSenderForTest()
	if err := s.Send(context.Background(), targetFor(t, r.addr), notifyApex, &key); err == nil {
		t.Fatal("Send accepted an unsigned response to a signed NOTIFY")
	}
	if r.received.Load() == 0 {
		t.Fatal("the responder never received the request, so this proves nothing about response signing")
	}
}

// §3.6's matching rule plus §4.8: reading exactly once would let a single
// non-matching datagram end the attempt before the real response has a
// chance to arrive — so an off-path sender who reaches the ephemeral port
// with one guessed ID would burn one of the round's five attempts,
// repeatably. The read has to wait out the deadline, discarding whatever
// doesn't match, rather than stopping at the first packet.
func TestSendIgnoresAStrayPacketAndWaitsForTheRealResponse(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	go func() {
		buf := make([]byte, 512)
		n, from, err := pc.ReadFrom(buf)
		if err != nil {
			return
		}
		m := new(dns.Msg)
		if m.Unpack(buf[:n]) != nil {
			return
		}

		// A stray packet with a guessed-but-wrong ID, standing in for an
		// off-path attacker — sent (and so read) before the real response,
		// so the loop must not stop here.
		bad := new(dns.Msg)
		bad.SetReply(m)
		bad.Id = m.Id + 1
		if out, err := bad.Pack(); err == nil {
			_, _ = pc.WriteTo(out, from)
		}

		// The real response, right behind it.
		good := new(dns.Msg)
		good.SetReply(m)
		if out, err := good.Pack(); err == nil {
			_, _ = pc.WriteTo(out, from)
		}
	}()

	s := zones.NewUDPSenderForTest()
	if err := s.Send(context.Background(), targetFor(t, pc.LocalAddr().String()), notifyApex, nil); err != nil {
		t.Fatalf("Send: %v, want the real response found after the stray one", err)
	}
}

// Finding 3: each resolved address must get its own timeout, not a share of
// one deadline installed before the fan-out starts. A first address that
// silently drops every packet must not leave the second address's own
// attempt with an already-expired deadline instead of a real attempt.
func TestSendTriesEachAddressWithItsOwnTimeout(t *testing.T) {
	port := freeUDPPort(t)

	// Silent, tried first: consumes a full attempt's timeout if — and only
	// if — each address gets its own, rather than sharing one deadline
	// installed before the loop starts.
	silentPC, err := net.ListenPacket("udp", "127.0.0.2:"+strconv.Itoa(port))
	if err != nil {
		t.Skipf("could not bind 127.0.0.2 (needed as a second loopback address): %v", err)
	}
	silentSrv := &dns.Server{PacketConn: silentPC, Handler: dns.HandlerFunc(func(dns.ResponseWriter, *dns.Msg) {})}
	go func() { _ = silentSrv.ActivateAndServe() }()
	t.Cleanup(func() { _ = silentSrv.Shutdown() })

	// A working responder, tried second.
	workingPC, err := net.ListenPacket("udp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		t.Fatal(err)
	}
	var workingReceived atomic.Bool
	workingSrv := &dns.Server{PacketConn: workingPC, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, m *dns.Msg) {
		workingReceived.Store(true)
		reply := new(dns.Msg)
		reply.SetRcode(m, dns.RcodeSuccess)
		_ = w.WriteMsg(reply)
	})}
	go func() { _ = workingSrv.ActivateAndServe() }()
	t.Cleanup(func() { _ = workingSrv.Shutdown() })

	const host = "two-addr.notify.test."
	res := fakeAResolver(t, host, []net.IP{net.ParseIP("127.0.0.2"), net.ParseIP("127.0.0.1")})
	s := zones.NewUDPSenderForTestWithResolver(res)

	err = s.Send(context.Background(), targetFor(t, host+":"+strconv.Itoa(port)), notifyApex, nil)
	if err != nil {
		t.Fatalf("Send: %v, want the second address to still get its own attempt", err)
	}
	if !workingReceived.Load() {
		t.Error("the working address never received a request")
	}
}

// Finding 4: the fan-out's failures are accumulated, and the classification
// is decided from the whole set — not from whichever address's attempt
// happened to run last. A target with one address that is merely slow (a
// timeout, an unknown outcome) and one address that is confirmed
// unreachable (ECONNREFUSED, positive evidence) must not end the round: the
// address that matters might still answer.
func TestSendDoesNotEndTheRoundWhenOnlySomeAddressesAreUnreachable(t *testing.T) {
	port := freeUDPPort(t)

	// Silent at 127.0.0.1: a timeout, an unknown outcome.
	silentPC, err := net.ListenPacket("udp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		t.Fatal(err)
	}
	silentSrv := &dns.Server{PacketConn: silentPC, Handler: dns.HandlerFunc(func(dns.ResponseWriter, *dns.Msg) {})}
	go func() { _ = silentSrv.ActivateAndServe() }()
	t.Cleanup(func() { _ = silentSrv.Shutdown() })

	// Closed at 127.0.0.2: bind and release, so nothing is listening there
	// and the kernel answers with ICMP port-unreachable — positive evidence,
	// the same trick TestSendReportsPortUnreachableDistinctly uses.
	closedPC, err := net.ListenPacket("udp", "127.0.0.2:"+strconv.Itoa(port))
	if err != nil {
		t.Skipf("could not bind 127.0.0.2 (needed as a second loopback address): %v", err)
	}
	_ = closedPC.Close()

	const host = "mixed-addr.notify.test."
	res := fakeAResolver(t, host, []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("127.0.0.2")})
	s := zones.NewUDPSenderForTestWithResolver(res)

	err = s.Send(context.Background(), targetFor(t, host+":"+strconv.Itoa(port)), notifyApex, nil)
	if err == nil {
		t.Fatal("Send = nil, want an error — neither address answered")
	}
	if errors.Is(err, zones.ErrNotifyUnreachable) {
		t.Errorf("error %v classified as unreachable, but only one of two addresses was — "+
			"the round would end after a single attempt while the slow address might still answer", err)
	}
	if errors.Is(err, zones.ErrNotifyDelivered) {
		t.Errorf("error %v classified as delivered, but neither address answered", err)
	}
}
