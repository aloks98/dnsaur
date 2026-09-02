package zones_test

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/aloks98/dnsaur/internal/dnssrv"
	"github.com/aloks98/dnsaur/internal/store"
	"github.com/aloks98/dnsaur/internal/zones"
)

// soaServer answers one SOA question with the serial it was built with, and
// counts how many queries it saw. refuse makes it answer REFUSED instead,
// standing in for a primary that is up but unwilling.
type soaServer struct {
	addr    string
	queries atomic.Int64
	serial  uint32
	refuse  bool
}

func newSOAServer(t *testing.T, apex string, serial uint32, refuse bool) *soaServer {
	t.Helper()
	s := &soaServer{serial: serial, refuse: refuse}
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &dns.Server{PacketConn: pc, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, m *dns.Msg) {
		s.queries.Add(1)
		reply := new(dns.Msg)
		if s.refuse {
			reply.SetRcode(m, dns.RcodeRefused)
			_ = w.WriteMsg(reply)
			return
		}
		reply.SetReply(m)
		reply.Authoritative = true
		reply.Answer = []dns.RR{&dns.SOA{
			Hdr:     dns.RR_Header{Name: dns.Fqdn(apex), Rrtype: dns.TypeSOA, Class: dns.ClassINET, Ttl: 900},
			Ns:      "ns1." + dns.Fqdn(apex),
			Mbox:    "hostmaster." + dns.Fqdn(apex),
			Serial:  s.serial,
			Refresh: 3600, Retry: 600, Expire: 604800, Minttl: 300,
		}}
		_ = w.WriteMsg(reply)
	})}
	s.addr = pc.LocalAddr().String()
	go func() { _ = srv.ActivateAndServe() }()
	t.Cleanup(func() { _ = srv.Shutdown() })
	return s
}

func TestProbeSerialReadsThePrimarysSerial(t *testing.T) {
	primary := newSOAServer(t, notifyApex, 47, false)
	st := openTestStore(t)
	tr := zones.NewTransferrer(st.Zones(), st.TSIGKeys())

	z := notifyZone()
	z.Primaries = primary.addr

	serial, from, err := tr.ProbeSerial(context.Background(), z)
	if err != nil {
		t.Fatalf("ProbeSerial: %v", err)
	}
	if serial != 47 {
		t.Errorf("serial = %d, want 47", serial)
	}
	if from.String() != primary.addr {
		t.Errorf("answered by %s, want %s", from, primary.addr)
	}
}

// The primaries are tried in order and any failure moves to the next one,
// exactly as Transfer does — a primary that is up but refusing is a reason
// to ask another rather than to give up.
func TestProbeSerialFallsThroughToTheNextPrimary(t *testing.T) {
	refusing := newSOAServer(t, notifyApex, 0, true)
	good := newSOAServer(t, notifyApex, 47, false)
	st := openTestStore(t)
	tr := zones.NewTransferrer(st.Zones(), st.TSIGKeys())

	z := notifyZone()
	z.Primaries = refusing.addr + ", " + good.addr

	serial, from, err := tr.ProbeSerial(context.Background(), z)
	if err != nil {
		t.Fatalf("ProbeSerial: %v", err)
	}
	if serial != 47 || from.String() != good.addr {
		t.Errorf("got serial %d from %s, want 47 from %s", serial, from, good.addr)
	}
	if refusing.queries.Load() == 0 {
		t.Error("the first primary was never asked")
	}
}

// Every primary failing names every attempt, rather than reporting one.
func TestProbeSerialReportsEveryFailure(t *testing.T) {
	a := newSOAServer(t, notifyApex, 0, true)
	b := newSOAServer(t, notifyApex, 0, true)
	st := openTestStore(t)
	tr := zones.NewTransferrer(st.Zones(), st.TSIGKeys())

	z := notifyZone()
	z.Primaries = a.addr + ", " + b.addr

	if _, _, err := tr.ProbeSerial(context.Background(), z); err == nil {
		t.Fatal("ProbeSerial succeeded with every primary refusing")
	} else {
		for _, want := range []string{a.addr, b.addr} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not name %s", err, want)
			}
		}
	}
}

// A primary can answer with a valid, well-formed SOA that simply does not
// name the zone being probed -- a misconfigured multi-tenant primary, or an
// off-path forgery for a keyless zone's single unsigned UDP exchange. Trusting
// it would let SerialNewer compare against the wrong number and, when that
// number happens to be higher, silently decline a transfer that should have
// happened. probeOne must treat this exactly like any other failure and move
// on to the next primary.
func TestProbeSerialRejectsTheWrongZonesSOA(t *testing.T) {
	wrongZone := newSOAServer(t, "other.example", 99, false)
	good := newSOAServer(t, notifyApex, 47, false)
	st := openTestStore(t)
	tr := zones.NewTransferrer(st.Zones(), st.TSIGKeys())

	z := notifyZone()
	z.Primaries = wrongZone.addr + ", " + good.addr

	serial, from, err := tr.ProbeSerial(context.Background(), z)
	if err != nil {
		t.Fatalf("ProbeSerial: %v", err)
	}
	if serial != 47 || from.String() != good.addr {
		t.Errorf("got serial %d from %s, want 47 from %s (the wrong-zone answer must not have been accepted)",
			serial, from, good.addr)
	}
	if wrongZone.queries.Load() == 0 {
		t.Error("the first primary was never asked")
	}
}

// ── Signing: the probe reuses fetch's exact mechanism ─────────────────────

// tsigSOAServer is newSOAServer with a key store attached: it verifies TSIG
// on every query and refuses anything that does not verify, mirroring the
// posture startTestPrimary's withTSIG gives the AXFR primary
// (transfer_test.go) -- the same proof, for the probe's UDP query instead of
// the transfer's TCP stream. signReply controls whether a request that
// verified gets a signed reply back -- RFC 8945 §5.4 requires it, mirroring
// notifysend_test.go's newTSIGNotifyResponder -- so the tests that pass true
// and false for it are what prove probeOne checks this on the way in, not
// merely produces it on the way out. (Before this parameter existed, every
// caller got an unsigned reply regardless -- reply.SetReply(m) does not
// carry the request's TSIG forward, so WriteMsg's "t := m.IsTsig(); t !=
// nil" signing branch never fired. That silently made
// TestProbeSerialSignsWithTSIGWhenTheZoneNamesAKey prove nothing about the
// response being signed.)
func newTSIGSOAServer(t *testing.T, apex string, serial uint32, keys dnssrv.TSIGKeys, signReply bool) *soaServer {
	t.Helper()
	s := &soaServer{serial: serial}
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &dns.Server{
		PacketConn:   pc,
		TsigProvider: dnssrv.NewTSIGProvider(keys),
		Handler: dns.HandlerFunc(func(w dns.ResponseWriter, m *dns.Msg) {
			s.queries.Add(1)
			req := m.IsTsig()
			if req == nil || w.TsigStatus() != nil {
				reply := new(dns.Msg)
				reply.SetRcode(m, dns.RcodeRefused)
				_ = w.WriteMsg(reply)
				return
			}
			reply := new(dns.Msg)
			reply.SetReply(m)
			reply.Authoritative = true
			reply.Answer = []dns.RR{&dns.SOA{
				Hdr:     dns.RR_Header{Name: dns.Fqdn(apex), Rrtype: dns.TypeSOA, Class: dns.ClassINET, Ttl: 900},
				Ns:      "ns1." + dns.Fqdn(apex),
				Mbox:    "hostmaster." + dns.Fqdn(apex),
				Serial:  s.serial,
				Refresh: 3600, Retry: 600, Expire: 604800, Minttl: 300,
			}}
			if signReply {
				// The stub carries no MAC yet -- WriteMsg computes it via
				// the server's own TsigProvider, set above, exactly as
				// transferserver.go's signIfVerified relies on.
				reply.Extra = append(reply.Extra, dnssrv.ReplyTSIG(reply, req.Hdr.Name, req))
			}
			_ = w.WriteMsg(reply)
		}),
	}
	s.addr = pc.LocalAddr().String()
	go func() { _ = srv.ActivateAndServe() }()
	t.Cleanup(func() { _ = srv.Shutdown() })
	return s
}

// The store is built first so the primary can verify against the same key
// the probe signs with -- one key, two ends, as a real pair is configured.
// Mirrors TestTransferSignsWithTSIGWhenTheZoneNamesAKey.
func TestProbeSerialSignsWithTSIGWhenTheZoneNamesAKey(t *testing.T) {
	st := openTestStore(t)
	keyID := storeTSIGKey(t, st, "probe-key."+notifyApex)
	primary := newTSIGSOAServer(t, notifyApex, 47, st.TSIGKeys(), true)

	tr := zones.NewTransferrer(st.Zones(), st.TSIGKeys())
	z := notifyZone()
	z.Primaries, z.TSIGKeyID = primary.addr, keyID

	serial, _, err := tr.ProbeSerial(context.Background(), z)
	if err != nil {
		t.Fatalf("signed probe: %v", err)
	}
	if serial != 47 {
		t.Errorf("serial = %d, want 47", serial)
	}
}

// The same primary, unsigned. This is what makes the test above mean
// something: without it, a probe that silently sent no TSIG would pass it if
// the primary happened not to care. Mirrors
// TestTransferAgainstATSIGPrimaryFailsUnsigned.
func TestProbeSerialAgainstATSIGPrimaryFailsUnsigned(t *testing.T) {
	st := openTestStore(t)
	primary := newTSIGSOAServer(t, notifyApex, 47, st.TSIGKeys(), true)

	tr := zones.NewTransferrer(st.Zones(), st.TSIGKeys())
	z := notifyZone()
	z.Primaries = primary.addr // and no tsig_key_id

	if _, _, err := tr.ProbeSerial(context.Background(), z); err == nil {
		t.Fatal("unsigned probe succeeded against a primary that requires TSIG")
	}
}

// RFC 8945 §5.4: a signed request's response must be signed too. miekg's
// ExchangeContext only verifies a TSIG that is present (client.go:267, "if
// t := m.IsTsig(); t != nil") -- a reply carrying none skips verification
// entirely. So without a check in probeOne, a spoofed *unsigned* reply that
// merely guesses the query ID, source port and owner name -- and names a
// serial <= ours -- would end the round exactly as an authentic signed one
// would, and the transfer that should have happened would silently not
// happen. Mirrors TestSendRejectsAnUnsignedResponseToASignedNotify.
func TestProbeSerialRejectsAnUnsignedResponseToASignedProbe(t *testing.T) {
	st := openTestStore(t)
	keyID := storeTSIGKey(t, st, "probe-key."+notifyApex)
	// Verifies the request correctly, but never signs its own reply.
	primary := newTSIGSOAServer(t, notifyApex, 47, st.TSIGKeys(), false)

	tr := zones.NewTransferrer(st.Zones(), st.TSIGKeys())
	z := notifyZone()
	z.Primaries, z.TSIGKeyID = primary.addr, keyID

	if _, _, err := tr.ProbeSerial(context.Background(), z); err == nil {
		t.Fatal("ProbeSerial accepted an unsigned response to a signed probe")
	}
	if primary.queries.Load() == 0 {
		t.Fatal("the primary never received the request, so this proves nothing about response signing")
	}
}

// ── The decision the probe feeds ─────────────────────────────────────────

// fakeProbes returns a fixed serial and counts calls.
type fakeProbes struct {
	serial uint32
	err    error
	calls  atomic.Int64
}

func (f *fakeProbes) ProbeSerial(context.Context, store.Zone) (uint32, netip.AddrPort, error) {
	f.calls.Add(1)
	return f.serial, netip.AddrPortFrom(netip.MustParseAddr("10.0.0.1"), 53), f.err
}

func TestNotifyTransfersOnlyWhenTheSerialAdvanced(t *testing.T) {
	tests := []struct {
		name         string
		localSerial  uint32
		remoteSerial uint32
		refreshedAt  int64
		wantProbe    bool
		wantTransfer bool
	}{
		{
			name: "the primary is ahead", localSerial: 10, remoteSerial: 11,
			refreshedAt: 1, wantProbe: true, wantTransfer: true,
		},
		{
			name: "we are already current", localSerial: 10, remoteSerial: 10,
			refreshedAt: 1, wantProbe: true, wantTransfer: false,
		},
		{
			name: "the primary went backwards", localSerial: 10, remoteSerial: 9,
			refreshedAt: 1, wantProbe: true, wantTransfer: false,
		},
		{
			// RFC 1982: a wrapped serial is newer, and this is the case a
			// naive `>` gets wrong.
			name: "the primary wrapped", localSerial: 4294967295, remoteSerial: 0,
			refreshedAt: 1, wantProbe: true, wantTransfer: true,
		},
		{
			// THE TRAP. A secondary created through the API starts at
			// soa_serial 1, so a primary also at 1 would make every
			// comparison say "not newer" and the zone would stay
			// permanently empty while reporting nothing wrong.
			name: "never transferred, identical serials", localSerial: 1, remoteSerial: 1,
			refreshedAt: 0, wantProbe: false, wantTransfer: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			z := notifyZone()
			z.SOASerial = tc.localSerial
			z.RefreshedAt = tc.refreshedAt
			probes := &fakeProbes{serial: tc.remoteSerial}
			f := newNotifyFixture(t, z, zones.WithNotifyProbes(probes))

			reply := f.notify(t, notifyApex, dns.TypeSOA, "10.0.0.1", "", nil)
			if reply.Rcode != dns.RcodeSuccess {
				t.Fatalf("rcode = %s, want NOERROR", dns.RcodeToString[reply.Rcode])
			}

			deadline := time.Now().Add(2 * time.Second)
			for probes.calls.Load() == 0 && f.rf.count() == 0 && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			time.Sleep(50 * time.Millisecond) // let a wrong extra call land

			if got := probes.calls.Load() > 0; got != tc.wantProbe {
				t.Errorf("probed = %v, want %v", got, tc.wantProbe)
			}
			if got := f.rf.count() > 0; got != tc.wantTransfer {
				t.Errorf("transferred = %v, want %v", got, tc.wantTransfer)
			}
		})
	}
}

// RFC 1996 §3.7–3.8: the answer-section SOA is "an unsecure hint", and "In no
// case shall the answer section of a NOTIFY request be used to update a
// slave's local data, or to indicate that a zone transfer needs to be
// undertaken, or to change the slave's zone refresh timers." Using it to skip
// the probe would be exactly the second of those three.
func TestNotifyIgnoresTheAnswerSectionSOA(t *testing.T) {
	z := notifyZone()
	z.SOASerial = 10
	probes := &fakeProbes{serial: 10} // the primary is NOT ahead
	f := newNotifyFixture(t, z, zones.WithNotifyProbes(probes))

	// A sender claiming, unsigned and unverifiable, that the zone is far
	// ahead. Believing it would transfer; the RFC forbids that.
	m := new(dns.Msg).SetNotify(dns.Fqdn(notifyApex))
	m.Answer = []dns.RR{&dns.SOA{
		Hdr:    dns.RR_Header{Name: dns.Fqdn(notifyApex), Rrtype: dns.TypeSOA, Class: dns.ClassINET, Ttl: 900},
		Ns:     "ns1." + dns.Fqdn(notifyApex),
		Mbox:   "hostmaster." + dns.Fqdn(notifyApex),
		Serial: 9999,
	}}
	w := &captureWriter{remote: &net.UDPAddr{IP: net.ParseIP("10.0.0.1"), Port: 40000}}
	f.ns.ServeNotify(context.Background(), w, m, "", nil)

	deadline := time.Now().Add(2 * time.Second)
	for probes.calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)

	if probes.calls.Load() == 0 {
		t.Error("the answer-section SOA was used instead of probing")
	}
	if f.rf.count() != 0 {
		t.Error("transferred on the strength of an unauthenticated hint")
	}
}

// A probe that fails outright -- every primary unreachable or refusing --
// must not be treated as "the primary is ahead". act's error branch returns
// before ever calling Refresh; this is the level-above pin for that, since
// TestProbeSerialReportsEveryFailure only proves ProbeSerial's own error, not
// what act does with it.
func TestNotifyDoesNotTransferWhenTheProbeFails(t *testing.T) {
	z := notifyZone()
	z.SOASerial = 10
	probes := &fakeProbes{err: errors.New("every primary refused")}
	f := newNotifyFixture(t, z, zones.WithNotifyProbes(probes))

	reply := f.notify(t, notifyApex, dns.TypeSOA, "10.0.0.1", "", nil)
	if reply.Rcode != dns.RcodeSuccess {
		t.Fatalf("rcode = %s, want NOERROR", dns.RcodeToString[reply.Rcode])
	}

	deadline := time.Now().Add(2 * time.Second)
	for probes.calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)

	if probes.calls.Load() == 0 {
		t.Fatal("the probe was never called")
	}
	if f.rf.count() != 0 {
		t.Error("transferred despite the probe failing")
	}
}
