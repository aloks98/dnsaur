package zones_test

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aloks98/dnsaur/internal/dnssrv"
	"github.com/aloks98/dnsaur/internal/store"
	"github.com/aloks98/dnsaur/internal/zones"
	"github.com/miekg/dns"
)

// The stub fetcher's tests run against a *real* master — a dns.Server on
// 127.0.0.1:0 answering ordinary SOA and NS queries, over both UDP and TCP —
// into a *real* store, and read the routing table back out of the resolver's
// own snapshot.
//
// Ordinary queries rather than an AXFR is the whole point of the type, so
// the master here is deliberately not startTestPrimary: it serves no zone at
// all, has no allow_transfer, and would refuse a transfer. A fetch that
// works against it is a fetch that needs no transfer permission.

// stubMaster is a DNS server standing in for a stub zone's master. It answers
// the two queries a fetch makes and counts what it was asked, which is how a
// test tells "the second master answered" from "the first one did" and
// "re-asked over TCP" from "believed the truncated answer".
type stubMaster struct {
	addr    string
	queries atomic.Int64
	tcp     atomic.Int64
}

// stubMasterConfig is what one master answers. A struct rather than the
// option funcs startTestPrimary takes, because every field here is data the
// master serves rather than a posture it adopts, and the literal reads as the
// zone it is standing in for.
type stubMasterConfig struct {
	// serial goes into the SOA; the rest of the SOA is the same shape
	// primaryZoneRRs uses, so a test can assert the schedule was adopted.
	serial uint32
	// soaOwner overrides the SOA's and NS records' owner name. Empty means
	// the zone the master was started for; anything else is the misconfigured
	// or forged answer the owner check exists to refuse.
	soaOwner string
	// ns is the delegation, and glue the ADDITIONAL section, both as
	// master-file lines. An empty glue is the case an in-zone nameserver
	// cannot survive.
	ns   []string
	glue []string
	// nsInAuthority moves the delegation from the ANSWER section to
	// AUTHORITY, leaving the SOA answer authoritative and untouched: a master
	// that is referring us to those nameservers rather than speaking for the
	// zone as them. The one shape that tells the two sections apart.
	nsInAuthority bool
	// keys, when set, makes the master verify TSIG on every query and refuse
	// anything that did not arrive correctly signed — the posture a master
	// handing out an internal delegation actually runs with.
	keys dnssrv.TSIGKeys
	// rcode, when non-zero, is answered instead of the zone: a master that is
	// up but unwilling, rather than unreachable.
	rcode int
	// truncate answers every UDP query with TC set and an empty body, so a
	// fetch that believes it loses the whole delegation.
	truncate bool
	// unsignedReplies verifies a correctly signed request and then answers
	// *without* signing — RFC 8945 §5.4's requirement, violated. The
	// counterpart of transfer_test.go's withUnsignedReplies, for the ordinary
	// query path rather than the AXFR one.
	unsignedReplies bool
}

// startStubMaster serves cfg as zone on one loopback port, on UDP and TCP
// both, shutting down when the test ends. Both transports, because a DNS
// client that meets TC=1 has to be able to re-ask over TCP on the same
// address (RFC 1035 §4.2.1) and there is no way to prove it does with a
// UDP-only server.
func startStubMaster(t *testing.T, zone string, cfg stubMasterConfig) *stubMaster {
	t.Helper()

	m := &stubMaster{}
	owner := dns.Fqdn(zone)
	if cfg.soaOwner != "" {
		owner = dns.Fqdn(cfg.soaOwner)
	}
	soa := mustRR(t, fmt.Sprintf("%s 900 IN SOA ns1.%s hostadmin.%s %d %d 300 %d 900",
		owner, owner, owner, cfg.serial, primaryRefresh, primaryExpire))

	handle := func(w dns.ResponseWriter, r *dns.Msg) {
		m.queries.Add(1)
		_, overTCP := w.RemoteAddr().(*net.TCPAddr)
		if overTCP {
			m.tcp.Add(1)
		}

		refuse := func(rcode int) {
			reply := new(dns.Msg)
			reply.SetRcode(r, rcode)
			_ = w.WriteMsg(reply)
		}
		if cfg.rcode != 0 {
			refuse(cfg.rcode)
			return
		}
		req := r.IsTsig()
		if cfg.keys != nil && (req == nil || w.TsigStatus() != nil) {
			refuse(dns.RcodeRefused)
			return
		}
		if len(r.Question) != 1 {
			refuse(dns.RcodeFormatError)
			return
		}

		reply := new(dns.Msg)
		reply.SetReply(r)
		reply.Authoritative = true
		if cfg.truncate && !overTCP {
			reply.Truncated = true
		} else {
			switch r.Question[0].Qtype {
			case dns.TypeSOA:
				reply.Answer = []dns.RR{soa}
			case dns.TypeNS:
				for _, line := range cfg.ns {
					if cfg.nsInAuthority {
						reply.Ns = append(reply.Ns, mustRR(t, line))
						continue
					}
					reply.Answer = append(reply.Answer, mustRR(t, line))
				}
				for _, line := range cfg.glue {
					reply.Extra = append(reply.Extra, mustRR(t, line))
				}
			default:
				refuse(dns.RcodeRefused)
				return
			}
		}
		if req != nil && !cfg.unsignedReplies {
			// The stub carries no MAC yet — WriteMsg computes it through the
			// server's own TsigProvider, exactly as newTSIGSOAServer relies on.
			// Appended last, which is where a TSIG belongs (RFC 8945 §5.1).
			reply.Extra = append(reply.Extra, dnssrv.ReplyTSIG(reply, req.Hdr.Name, req))
		}
		_ = w.WriteMsg(reply)
	}

	mux := dns.NewServeMux()
	mux.HandleFunc(".", handle)

	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	m.addr = pc.LocalAddr().String()
	ln, err := net.Listen("tcp", m.addr)
	if err != nil {
		t.Fatalf("listen tcp on the udp port: %v", err)
	}
	tsig := dnssrv.NewTSIGProvider(cfg.keys)
	for _, srv := range []*dns.Server{
		{PacketConn: pc, Handler: mux, TsigProvider: tsig},
		{Listener: ln, Handler: mux, TsigProvider: tsig},
	} {
		go func() { _ = srv.ActivateAndServe() }()
		t.Cleanup(func() { _ = srv.Shutdown() })
	}
	return m
}

// stubTestResolver is the resolver an out-of-zone nameserver is looked up
// through, and the guard on the one that must never be looked up at all.
//
// A name under rejectUnder fails the test the moment it is asked for, by
// name, with no dependence on timing: resolving a nameserver inside the stub's
// own suffix routes the query back into this very zone and needs the address
// being resolved, so the defect it catches presents as a hang rather than as
// an error.
func stubTestResolver(t *testing.T, rejectUnder string, answers map[string][]net.IP) *net.Resolver {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &dns.Server{PacketConn: pc, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, m *dns.Msg) {
		reply := new(dns.Msg)
		reply.SetReply(m)
		reply.Authoritative = true
		if len(m.Question) == 1 {
			q := dns.CanonicalName(m.Question[0].Name)
			if rejectUnder != "" && dns.IsSubDomain(dns.CanonicalName(dns.Fqdn(rejectUnder)), q) {
				// Errorf, not Fatalf: this runs on the server's goroutine.
				//
				// In the one case where this fires *after* the test has
				// already ended — a genuine hang, where the 5s backstop
				// t.Fatal'd first and the lookup completed later — testing
				// panics with "Log in goroutine after test has completed".
				// Noise on top of an already-failing run, not a wrong result,
				// and the alternative (plumbing a done channel through the
				// handler) buys nothing: the run has failed either way.
				t.Errorf("the fetcher looked up %s, a name inside the stub's own suffix — "+
					"that query routes back into this zone and never terminates; it must come from glue or be skipped", q)
			}
			if m.Question[0].Qtype == dns.TypeA {
				for _, ip := range answers[strings.TrimSuffix(q, ".")] {
					reply.Answer = append(reply.Answer, &dns.A{
						Hdr: dns.RR_Header{Name: q, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 5},
						A:   ip,
					})
				}
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

// stubFetcher is the fixture's fetcher, on the fixture's clock and wired to
// republish the fixture's resolver — production's shape, so a test reads the
// routing table back out of the snapshot rather than out of the store.
func (f *transferFixture) stubFetcher(opts ...zones.StubOption) *zones.StubFetcher {
	return zones.NewStubFetcher(f.st.Zones(), f.st.TSIGKeys(),
		append([]zones.StubOption{
			zones.WithStubNow(func() time.Time { return f.now }),
			zones.WithStubReload(f.resolver.Reload),
		}, opts...)...)
}

// upstreamsFromSnapshot is what internal/app's conditionalRoutes does: derive
// the zone's dial addresses from the served snapshot, with no query of any
// kind. Reading it here is what proves a fetch left the routing table
// derivable rather than merely leaving rows in a table.
func upstreamsFromSnapshot(t *testing.T, f *transferFixture) []string {
	t.Helper()
	z := f.resolver.Snapshot().Apex(transferApex)
	if z == nil {
		t.Fatalf("zone %q is not in the served snapshot", transferApex)
	}
	return zones.StubUpstreams(*z)
}

func stubNSLine(ns string) string {
	return fmt.Sprintf("%s. 3600 IN NS %s.", transferApex, ns)
}

// A master serving the zone's delegation with glue for both nameservers.
// The fetch installs the NS set, adopts the SOA's serial and schedule, and
// derives both addresses from the glue — including the v6 one, which has to
// come back bracketed to be a dial address at all.
//
// The ADDITIONAL section also carries an address for a name that is not in
// the delegation, and that is the point of the fixture rather than
// decoration. An ADDITIONAL section is whatever the master chose to put
// there, and for a keyless stub it arrives in a single unsigned UDP exchange,
// so its contents are attacker-choosable. Anything in it that becomes an
// upstream is a destination this zone's internal names are then sent to —
// §9.11.5's failure with the destination chosen by somebody else. Only the
// records at exactly a nameserver's own owner name are glue for it.
func TestStubFetchInstallsTheNSSet(t *testing.T) {
	forEachDriver(t, func(t *testing.T, driver string) {
		master := startStubMaster(t, transferApex, stubMasterConfig{
			serial: primarySerial,
			ns:     []string{stubNSLine("ns1." + transferApex), stubNSLine("ns2." + transferApex)},
			glue: []string{
				fmt.Sprintf("ns1.%s. 3600 IN A 10.9.0.1", transferApex),
				fmt.Sprintf("ns2.%s. 3600 IN AAAA fd00::2", transferApex),
				// In the ADDITIONAL section, named by no NS record. It must not
				// be glue for ns1, for ns2, or for anything else.
				fmt.Sprintf("mail.%s. 3600 IN A 10.9.9.9", transferApex),
			},
		})
		f := newTransferFixtureOn(t, driver, master.addr, 0, asZoneType("stub"))

		res, err := f.stubFetcher().Fetch(context.Background(), f.zone(t))
		if err != nil {
			t.Fatalf("Fetch: %v", err)
		}
		if res.Master.String() != master.addr {
			t.Errorf("Master = %s, want %s", res.Master, master.addr)
		}
		if res.Serial != primarySerial {
			t.Errorf("Serial = %d, want %d", res.Serial, primarySerial)
		}
		if res.NS != 2 {
			t.Errorf("NS = %d, want 2", res.NS)
		}
		want := []string{"10.9.0.1:53", "[fd00::2]:53"}
		if got := strings.Join(res.Upstreams, ","); got != strings.Join(want, ",") {
			t.Errorf("Upstreams = %v, want %v", res.Upstreams, want)
		}
		// Named separately from the comparison above so the failure says which
		// rule broke: an ADDITIONAL record at a name no NS points at became a
		// destination for this zone's queries.
		for _, u := range res.Upstreams {
			if u == "10.9.9.9:53" {
				t.Errorf("Upstreams = %v: an address from ADDITIONAL that no nameserver is named by "+
					"became an upstream — a master (or anyone who can answer as one) chooses where this zone's queries go", res.Upstreams)
			}
		}

		// The schedule the master keeps, adopted verbatim, and the stamp that
		// dates the fetch.
		z := f.zone(t)
		if z.SOASerial != primarySerial || z.SOARefresh != primaryRefresh || z.SOAExpire != primaryExpire {
			t.Errorf("zone row: serial=%d refresh=%d expire=%d, want %d/%d/%d",
				z.SOASerial, z.SOARefresh, z.SOAExpire, primarySerial, primaryRefresh, primaryExpire)
		}
		if z.RefreshedAt != f.now.UnixMilli() {
			t.Errorf("refreshed_at = %d, want %d", z.RefreshedAt, f.now.UnixMilli())
		}

		// And the same addresses again from the served snapshot, which is where
		// the routing table is rebuilt from on every reload.
		if got := strings.Join(upstreamsFromSnapshot(t, f), ","); got != strings.Join(want, ",") {
			t.Errorf("StubUpstreams from the snapshot = %v, want %v", got, want)
		}
	})
}

// THE HANG TEST. The master answers NS ns1.<apex> with no glue.
// ns1.<apex> is inside the stub's own suffix, so resolving it would route
// back into this zone and never terminate.
//
// Two independent guards, and the ordering matters. The PRIMARY assertion is
// not the timeout — it is that the resolver is never ASKED (stubTestResolver's
// rejectUnder), so removing the guard fails deterministically, by name, with
// no dependence on timing at all. The timeout is only a backstop that turns a
// genuine hang into this test failing rather than the whole package timing
// out, and its deadline is separate from and much shorter than the context
// Fetch runs under: one shared context would make the two select cases ready
// at the same instant and the test a coin flip.
func TestStubFetchSkipsAnInZoneNameserverWithNoGlue(t *testing.T) {
	master := startStubMaster(t, transferApex, stubMasterConfig{
		serial: primarySerial,
		ns:     []string{stubNSLine("ns1." + transferApex)},
	})
	f := newTransferFixture(t, master.addr, 0, asZoneType("stub"))
	res := stubTestResolver(t, transferApex, nil)
	z := f.zone(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var (
		got zones.StubResult
		err error
	)
	done := make(chan struct{})
	go func() {
		defer close(done)
		got, err = f.stubFetcher(zones.WithStubResolver(res)).Fetch(ctx, z)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Fetch did not return: an in-zone nameserver was resolved instead of skipped")
	}

	// The outcome, not merely that Fetch returned. Returning early for an
	// unrelated reason — an unreachable master, a parse failure — also closes
	// done, and a test that only waited would pass on any of them.
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got.NS != 1 {
		t.Errorf("NS = %d, want 1: the delegation is installed even though it is unusable", got.NS)
	}
	// No usable upstreams is what makes the zone claim its suffix and answer
	// SERVFAIL (§9.11.5) rather than falling through to the public internet.
	if len(got.Upstreams) != 0 {
		t.Errorf("Upstreams = %v, want none: a nameserver inside the zone with no glue is unusable", got.Upstreams)
	}
	if u := upstreamsFromSnapshot(t, f); len(u) != 0 {
		t.Errorf("StubUpstreams from the snapshot = %v, want none", u)
	}
}

// A nameserver outside the zone cannot re-enter it — the suffix differs — so
// it is resolved normally, through the same net.Resolver ParsePrimaries uses.
func TestStubFetchResolvesAnOutOfZoneNameserver(t *testing.T) {
	master := startStubMaster(t, transferApex, stubMasterConfig{
		serial: primarySerial,
		ns:     []string{stubNSLine("ns.example.net")},
	})
	f := newTransferFixture(t, master.addr, 0, asZoneType("stub"))
	res := stubTestResolver(t, transferApex, map[string][]net.IP{
		"ns.example.net": {net.ParseIP("10.9.0.7")},
	})

	got, err := f.stubFetcher(zones.WithStubResolver(res)).Fetch(context.Background(), f.zone(t))
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(got.Upstreams) != 1 || got.Upstreams[0] != "10.9.0.7:53" {
		t.Fatalf("Upstreams = %v, want [10.9.0.7:53]", got.Upstreams)
	}
	// And it survives a reload with no lookup of its own: the address is
	// stored beside the NS record, exactly as glue is.
	if u := upstreamsFromSnapshot(t, f); len(u) != 1 || u[0] != "10.9.0.7:53" {
		t.Errorf("StubUpstreams from the snapshot = %v, want [10.9.0.7:53]", u)
	}
}

// A master that refuses anything not correctly signed. A fetch that completes
// against it can only have been signed — both queries, since either one
// failing fails the fetch.
func TestStubFetchSignsWithTSIGWhenTheZoneNamesAKey(t *testing.T) {
	// The store is built first so the master can verify against the same key
	// the fetch signs with — one key, two ends, as a real pair is configured.
	f := newTransferFixture(t, "127.0.0.1:0", 0, asZoneType("stub"))
	keyID := storeTSIGKey(t, f.st, "stub-key."+transferApex)
	master := startStubMaster(t, transferApex, stubMasterConfig{
		serial: primarySerial,
		ns:     []string{stubNSLine("ns1." + transferApex)},
		glue:   []string{fmt.Sprintf("ns1.%s. 3600 IN A 10.9.0.1", transferApex)},
		keys:   f.st.TSIGKeys(),
	})

	z := f.zone(t)
	z.Primaries, z.TSIGKeyID = master.addr, keyID
	if err := f.st.Zones().UpdateZone(context.Background(), z); err != nil {
		t.Fatalf("UpdateZone: %v", err)
	}

	got, err := f.stubFetcher().Fetch(context.Background(), f.zone(t))
	if err != nil {
		t.Fatalf("signed fetch: %v", err)
	}
	if len(got.Upstreams) != 1 || got.Upstreams[0] != "10.9.0.1:53" {
		t.Errorf("Upstreams = %v, want [10.9.0.1:53]", got.Upstreams)
	}
}

// The negative half: without it, the test above would pass against a fetch
// that sent no TSIG at all if the master happened not to care.
func TestStubFetchAgainstATSIGMasterFailsUnsigned(t *testing.T) {
	f := newTransferFixture(t, "127.0.0.1:0", 0, asZoneType("stub"))
	master := startStubMaster(t, transferApex, stubMasterConfig{
		serial: primarySerial,
		ns:     []string{stubNSLine("ns1." + transferApex)},
		glue:   []string{fmt.Sprintf("ns1.%s. 3600 IN A 10.9.0.1", transferApex)},
		keys:   f.st.TSIGKeys(),
	})

	z := f.zone(t)
	z.Primaries = master.addr // and no tsig_key_id
	if err := f.st.Zones().UpdateZone(context.Background(), z); err != nil {
		t.Fatalf("UpdateZone: %v", err)
	}

	if _, err := f.stubFetcher().Fetch(context.Background(), f.zone(t)); err == nil {
		t.Fatal("unsigned fetch succeeded against a master that requires TSIG")
	}
	if master.queries.Load() == 0 {
		t.Fatal("the master was never asked, so this proves nothing about signing")
	}
}

// RFC 8945 §5.4: a signed request's response must be signed too. The master
// here verifies our signature and then answers unsigned.
//
// Without the check, TSIG would authenticate the *request* and contribute
// nothing to the *reply*: whoever answers first — an off-path attacker who
// guesses the query ID and ephemeral port — hands back a delegation, and this
// zone's internal names go wherever its glue says. miekg's dns.Client makes
// that the default, since its ReadMsg verifies only a TSIG that is already
// present (client.go:267); the identical defect has been found twice on this
// project, in the SOA probe and in the NOTIFY sender.
//
// signedExchange is one implementation and TestProbeSerialRejectsAnUnsigned-
// ResponseToASignedProbe already pins it. This is the stub's own pin on it,
// because a stub reaches that code down a path a probe does not — two queries,
// either of which may be the one answered unsigned, and a TCP retry underneath
// — and a defect introduced twice deserves a test on each path that depends
// on it rather than one shared one somewhere else in the package.
func TestStubFetchRejectsAnUnsignedReplyToASignedQuery(t *testing.T) {
	f := newTransferFixture(t, "127.0.0.1:0", 0, asZoneType("stub"))
	keyID := storeTSIGKey(t, f.st, "stub-key."+transferApex)
	master := startStubMaster(t, transferApex, stubMasterConfig{
		serial:          primarySerial,
		ns:              []string{stubNSLine("ns1." + transferApex)},
		glue:            []string{fmt.Sprintf("ns1.%s. 3600 IN A 10.9.0.1", transferApex)},
		keys:            f.st.TSIGKeys(),
		unsignedReplies: true,
	})

	z := f.zone(t)
	z.Primaries, z.TSIGKeyID = master.addr, keyID
	if err := f.st.Zones().UpdateZone(context.Background(), z); err != nil {
		t.Fatalf("UpdateZone: %v", err)
	}

	if _, err := f.stubFetcher().Fetch(context.Background(), f.zone(t)); err == nil {
		t.Fatal("Fetch accepted an unsigned reply to a signed query")
	}
	// Otherwise a fetch that failed to reach the master at all would pass
	// this, and it would prove nothing about response signing.
	if master.queries.Load() == 0 {
		t.Fatal("the master never received the request")
	}
	if u := upstreamsFromSnapshot(t, f); len(u) != 0 {
		t.Errorf("StubUpstreams = %v, want none: nothing may have been installed", u)
	}
}

// The masters are walked in the order written, any failure moves to the next,
// and the all-failed error names every attempt — Transfer's own semantics,
// and for the same reason: masters are meant to be replicas, so one refusing
// is a reason to ask another.
func TestStubFetchTriesEveryMasterAndNamesEachFailure(t *testing.T) {
	refuser := startStubMaster(t, transferApex, stubMasterConfig{rcode: dns.RcodeRefused})
	dead := deadPort(t)
	good := startStubMaster(t, transferApex, stubMasterConfig{
		serial: primarySerial,
		ns:     []string{stubNSLine("ns1." + transferApex)},
		glue:   []string{fmt.Sprintf("ns1.%s. 3600 IN A 10.9.0.1", transferApex)},
	})

	f := newTransferFixture(t, dead+","+refuser.addr+","+good.addr, 0, asZoneType("stub"))
	got, err := f.stubFetcher().Fetch(context.Background(), f.zone(t))
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got.Master.String() != good.addr {
		t.Errorf("answered by %s, want %s", got.Master, good.addr)
	}
	if refuser.queries.Load() == 0 {
		t.Error("the refusing master was never asked")
	}

	// And with none of them working, the error names each attempt: the
	// failures are usually different and the difference is the fix.
	f2 := newTransferFixture(t, dead+","+refuser.addr, 0, asZoneType("stub"))
	_, err = f2.stubFetcher().Fetch(context.Background(), f2.zone(t))
	if err == nil {
		t.Fatal("Fetch succeeded with no working master")
	}
	for _, want := range []string{dead, refuser.addr} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not name master %s: %v", want, err)
		}
	}
}

// A master can answer with a well-formed SOA and NS set that simply do not
// name the zone being fetched — a misconfigured multi-tenant master, or an
// off-path forgery for the single unsigned UDP exchange a keyless zone makes.
// Believing it would point this zone's suffix at somebody else's nameservers,
// which is the whole of what a stub zone decides. Mirrors
// TestProbeSerialRejectsTheWrongZonesSOA.
func TestStubFetchRejectsTheWrongZonesAnswer(t *testing.T) {
	// Each half separately, because each is a separate check and one test
	// covering both would pass with either of them deleted.
	tests := []struct {
		name string
		cfg  stubMasterConfig
	}{
		{
			name: "the SOA names another zone",
			cfg: stubMasterConfig{
				serial:   primarySerial,
				soaOwner: "other.example",
				ns:       []string{stubNSLine("ns1." + transferApex)},
				glue:     []string{fmt.Sprintf("ns1.%s. 3600 IN A 10.9.0.1", transferApex)},
			},
		},
		{
			name: "the delegation names another zone",
			cfg: stubMasterConfig{
				serial: primarySerial,
				ns:     []string{"other.example. 3600 IN NS ns1.other.example."},
				glue:   []string{"ns1.other.example. 3600 IN A 10.9.9.9"},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			wrong := startStubMaster(t, transferApex, tc.cfg)
			f := newTransferFixture(t, wrong.addr, 0, asZoneType("stub"))

			if _, err := f.stubFetcher().Fetch(context.Background(), f.zone(t)); err == nil {
				t.Fatal("Fetch accepted an answer for a different zone")
			}
			if u := upstreamsFromSnapshot(t, f); len(u) != 0 {
				t.Errorf("StubUpstreams = %v, want none: nothing may have been installed", u)
			}
		})
	}
}

// A delegation in AUTHORITY is a referral, and a referral is not this zone's
// delegation to install: it is somebody else's, handed over by a master that
// does not speak for this zone. ask reads the ANSWER section only, and this
// is what says so.
//
// The fixture is built so the rule is the only thing under test. The SOA
// query is still answered authoritatively, in ANSWER and for this very apex,
// so the fetch gets past the soa == nil check — which is the check that
// happens to refuse a real-world referring master first, and which would
// otherwise hide whether ask reads AUTHORITY at all. What is left is a
// master whose only NS records are in the section ask does not read.
//
// Glue is present in ADDITIONAL, deliberately: reading AUTHORITY would find
// a nameserver with an address and install a working-looking route to it, so
// the mistake this pins is not a fetch that fails noisily but one that
// succeeds and points the suffix somewhere the master never claimed to speak
// for.
func TestStubFetchIgnoresADelegationInTheAuthoritySection(t *testing.T) {
	master := startStubMaster(t, transferApex, stubMasterConfig{
		serial:        primarySerial,
		ns:            []string{stubNSLine("ns1." + transferApex)},
		glue:          []string{fmt.Sprintf("ns1.%s. 3600 IN A 10.9.0.1", transferApex)},
		nsInAuthority: true,
	})
	f := newTransferFixture(t, master.addr, 0, asZoneType("stub"))

	_, err := f.stubFetcher().Fetch(context.Background(), f.zone(t))
	if err == nil {
		t.Fatal("Fetch installed a delegation the master carried in AUTHORITY: a referral is not the zone's own NS set")
	}
	// The message, so a fetch that failed for some other reason — an
	// unreachable master, a rejected SOA — cannot pass as this rule holding.
	if !strings.Contains(err.Error(), "no NS for "+transferApex) {
		t.Errorf("Fetch failed with %v, want the NS query to have found nothing in ANSWER", err)
	}
	if u := upstreamsFromSnapshot(t, f); len(u) != 0 {
		t.Errorf("StubUpstreams = %v, want none: nothing may have been installed", u)
	}
}

// A delegation with a lot of glue does not fit in a UDP answer, and a master
// that says so sets TC. Believing the truncated answer would lose every
// nameserver silently and leave the zone claiming its suffix with nothing to
// send there — a SERVFAIL that looks exactly like a master that is down.
func TestStubFetchReAsksOverTCPWhenTheAnswerIsTruncated(t *testing.T) {
	master := startStubMaster(t, transferApex, stubMasterConfig{
		serial:   primarySerial,
		ns:       []string{stubNSLine("ns1." + transferApex)},
		glue:     []string{fmt.Sprintf("ns1.%s. 3600 IN A 10.9.0.1", transferApex)},
		truncate: true,
	})
	f := newTransferFixture(t, master.addr, 0, asZoneType("stub"))

	got, err := f.stubFetcher().Fetch(context.Background(), f.zone(t))
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(got.Upstreams) != 1 || got.Upstreams[0] != "10.9.0.1:53" {
		t.Errorf("Upstreams = %v, want [10.9.0.1:53]", got.Upstreams)
	}
	if master.tcp.Load() == 0 {
		t.Error("nothing was re-asked over TCP, so the truncated answer was believed")
	}
}

// Only a stub is fetched this way. Pointing it at a primary would replace a
// zone this server authors with a delegation, and at a secondary would
// replace the zone it holds on loan — the same destruction Transfer's own
// type check exists to prevent.
func TestStubFetchRefusesANonStubZone(t *testing.T) {
	master := startStubMaster(t, transferApex, stubMasterConfig{
		serial: primarySerial,
		ns:     []string{stubNSLine("ns1." + transferApex)},
		glue:   []string{fmt.Sprintf("ns1.%s. 3600 IN A 10.9.0.1", transferApex)},
	})
	f := newTransferFixture(t, master.addr, 0) // the default: a secondary

	if _, err := f.stubFetcher().Fetch(context.Background(), f.zone(t)); err == nil {
		t.Fatal("a secondary zone was fetched as a stub")
	}
	if master.queries.Load() != 0 {
		t.Error("the master was contacted before the zone's type was checked")
	}
}

// A fetch that installs a delegation nothing has reloaded has written the
// right rows and is still routing from the previous ones — for a stub that
// means SERVFAIL, indistinguishable from a fetch that never happened.
func TestStubFetchRepublishesTheSnapshot(t *testing.T) {
	forEachDriver(t, func(t *testing.T, driver string) {
		master := startStubMaster(t, transferApex, stubMasterConfig{
			serial: primarySerial,
			ns:     []string{stubNSLine("ns1." + transferApex)},
			glue:   []string{fmt.Sprintf("ns1.%s. 3600 IN A 10.9.0.1", transferApex)},
		})
		f := newTransferFixtureOn(t, driver, master.addr, 0, asZoneType("stub"))

		if u := upstreamsFromSnapshot(t, f); len(u) != 0 {
			t.Fatalf("precondition: the unfetched stub already names %v", u)
		}
		if _, err := f.stubFetcher().Fetch(context.Background(), f.zone(t)); err != nil {
			t.Fatalf("Fetch: %v", err)
		}
		// No reload of the test's own between the fetch and this read: the fetch
		// is what has to have published it.
		if u := upstreamsFromSnapshot(t, f); len(u) != 1 || u[0] != "10.9.0.1:53" {
			t.Errorf("StubUpstreams from the snapshot = %v, want [10.9.0.1:53]: the fetch did not republish", u)
		}
	})
}

// The claim the whole glue fix exists to make: a dnsaur stub, pointed at a
// dnsaur primary, ends up with usable upstreams.
//
// Every other test here drives startStubMaster, a hand-written handler that
// emits whatever its config says — so the master half and the stub half are
// each tested against a fixture of the other, and neither pins that they fit
// together. This runs the real answering path (zones.Zone.Answer on a real
// primary) as the master, so it fails if dnsaur ever stops attaching glue to
// its own apex NS set: the stub is then handed nameserver names with no
// addresses, may not resolve them (they are in-zone), skips every one, and
// claims a suffix it cannot route.
//
// The resolver is stubTestResolver with rejectUnder set, so it doubles as the
// guard that this path never resolves an in-zone nameserver — the loop the
// glue rule exists to prevent.
func TestStubFetchAgainstADnsaurMasterGetsGlue(t *testing.T) {
	apex := dns.Fqdn(transferApex)
	master := zones.NewZone(store.Zone{
		Name: transferApex, Type: "primary", Enabled: true,
		SOANS: "ns1." + transferApex, SOAMbox: "hostadmin." + transferApex,
		SOASerial: primarySerial, SOARefresh: primaryRefresh, SOARetry: 300,
		SOAExpire: primaryExpire, SOAMinimum: 900, SOATTL: 900,
	}, []store.ZoneRecord{
		{Name: "@", Type: "NS", TTL: 3600, RData: "ns1." + apex, Enabled: true},
		{Name: "@", Type: "NS", TTL: 3600, RData: "ns2." + apex, Enabled: true},
		{Name: "ns1", Type: "A", TTL: 3600, RData: "10.9.0.1", Enabled: true},
		{Name: "ns2", Type: "AAAA", TTL: 3600, RData: "fd00::2", Enabled: true},
	})

	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &dns.Server{PacketConn: pc, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, r *dns.Msg) {
		reply := new(dns.Msg)
		reply.SetReply(r)
		q := r.Question[0]
		if !master.Answer(reply, strings.TrimSuffix(q.Name, "."), q.Qtype, 0) {
			reply.SetRcode(r, dns.RcodeRefused)
		}
		_ = w.WriteMsg(reply)
	})}
	go func() { _ = srv.ActivateAndServe() }()
	t.Cleanup(func() { _ = srv.Shutdown() })

	f := newTransferFixture(t, pc.LocalAddr().String(), 0, asZoneType("stub"))
	res, err := f.stubFetcher(zones.WithStubResolver(
		stubTestResolver(t, transferApex, map[string][]net.IP{}),
	)).Fetch(context.Background(), f.zone(t))
	if err != nil {
		t.Fatalf("Fetch against a dnsaur master: %v", err)
	}
	if res.NS != 2 {
		t.Errorf("NS = %d, want 2", res.NS)
	}
	want := []string{"10.9.0.1:53", "[fd00::2]:53"}
	if got := strings.Join(res.Upstreams, ","); got != strings.Join(want, ",") {
		t.Errorf("Upstreams = %v, want %v — a dnsaur master must hand a stub "+
			"addresses, not just nameserver names", res.Upstreams, want)
	}
	if got := upstreamsFromSnapshot(t, f); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("upstreams from the served snapshot = %v, want %v", got, want)
	}
}

// StubUpstreams reads the zone's NS records and the addresses stored beside
// them and nothing else. It takes no context and no resolver, so it *cannot*
// query — which is what lets the routing table be rebuilt on every zone
// reload without touching the network.
func TestStubUpstreamsIsPureAndDerivesFromStoredRecords(t *testing.T) {
	rec := func(name, typ, rdata string, enabled bool) store.ZoneRecord {
		return store.ZoneRecord{Name: name, Type: typ, TTL: 3600, RData: rdata, Enabled: enabled}
	}
	z := store.Zone{Name: transferApex, Type: "stub", Enabled: true}

	tests := []struct {
		name string
		recs []store.ZoneRecord
		want []string
	}{
		{
			name: "no records at all",
			recs: nil,
			want: nil,
		},
		{
			name: "glue for an in-zone nameserver",
			recs: []store.ZoneRecord{
				rec("@", "NS", "ns1."+transferApex+".", true),
				rec("ns1", "A", "10.9.0.1", true),
			},
			want: []string{"10.9.0.1:53"},
		},
		{
			name: "both families, and v6 comes back dialable",
			recs: []store.ZoneRecord{
				rec("@", "NS", "ns1."+transferApex+".", true),
				rec("ns1", "A", "10.9.0.1", true),
				rec("ns1", "AAAA", "fd00::1", true),
			},
			want: []string{"10.9.0.1:53", "[fd00::1]:53"},
		},
		{
			name: "an out-of-zone nameserver's address, stored under its own name",
			recs: []store.ZoneRecord{
				rec("@", "NS", "ns.example.net.", true),
				rec("ns.example.net", "A", "10.9.0.7", true),
			},
			want: []string{"10.9.0.7:53"},
		},
		{
			// The state the hang guard leaves behind: the delegation is there
			// and nothing resolves it, which claims the suffix and SERVFAILs.
			name: "a nameserver with no address contributes nothing",
			recs: []store.ZoneRecord{rec("@", "NS", "ns1."+transferApex+".", true)},
			want: nil,
		},
		{
			// NewZone drops disabled records before the snapshot is built, so
			// this is the same case as the address not being there.
			name: "a disabled address is not an upstream",
			recs: []store.ZoneRecord{
				rec("@", "NS", "ns1."+transferApex+".", true),
				rec("ns1", "A", "10.9.0.1", false),
			},
			want: nil,
		},
		{
			// Two nameservers behind one address is a real deployment; the
			// forwarder must not be told to try it twice.
			name: "duplicate addresses collapse",
			recs: []store.ZoneRecord{
				rec("@", "NS", "ns1."+transferApex+".", true),
				rec("@", "NS", "ns2."+transferApex+".", true),
				rec("ns1", "A", "10.9.0.1", true),
				rec("ns2", "A", "10.9.0.1", true),
			},
			want: []string{"10.9.0.1:53"},
		},
		{
			// Only the apex's NS set is the delegation. An NS below it is a
			// child's, and dialling it would send this zone's queries to a
			// server authoritative for something else.
			name: "an NS below the apex is not this zone's delegation",
			recs: []store.ZoneRecord{
				rec("sub", "NS", "ns1."+transferApex+".", true),
				rec("ns1", "A", "10.9.0.1", true),
			},
			want: nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := zones.StubUpstreams(zones.NewZone(z, tc.recs))
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("StubUpstreams = %v, want %v", got, tc.want)
			}
		})
	}
}
