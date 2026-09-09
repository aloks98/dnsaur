package zones_test

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aloks98/dnsaur/internal/dnssrv"
	"github.com/aloks98/dnsaur/internal/store"
	"github.com/aloks98/dnsaur/internal/zones"
	"github.com/miekg/dns"
)

// The AXFR client's tests transfer from a *real* primary — a dns.Server on
// 127.0.0.1:0 answering AXFR through dns.Transfer.Out — into a *real* store,
// and then query the zone back through the ordinary resolver.
//
// Nothing here mocks the transfer. A fake that hands the installer a slice of
// dns.RR would exercise every line of transfer.go and prove nothing about the
// one thing that is actually new in this task: that this process can speak
// the client half of RFC 5936 to a server that is not it. The framing, the
// SOA-first-and-last envelope sequence and the TSIG round trip only exist on
// the wire.

// testPrimary is a DNS server standing in for the zone's primary.
type testPrimary struct {
	addr string
	// requests counts AXFR queries that reached the handler, so a test can
	// tell "the second primary answered" from "the first one did".
	requests func() int
}

// primaryOption configures startTestPrimary.
type primaryOption func(*primaryConfig)

type primaryConfig struct {
	keys dnssrv.TSIGKeys
	// requireTSIG makes the primary answer REFUSED to anything that did not
	// arrive correctly signed — the posture a primary that hands out an
	// internal zone actually runs with.
	requireTSIG bool
	// rcode, when non-zero, is answered instead of the zone. Used to make a
	// primary refuse without making it unreachable.
	rcode int
	// rcodeFn, when set, is consulted per request and overrides rcode. It is
	// what lets a scheduling test make a primary fail for a while and then
	// recover, which is the sequence the retry schedule exists for.
	rcodeFn func() int
	// hold, when set, runs at the top of the handler before anything is
	// answered. A test uses it to keep two transfers of one zone genuinely
	// overlapping rather than hoping they do.
	hold func()
	// unsignedReplies makes the primary verify a signed request and then
	// answer *without* signing — RFC 8945 §5.4's requirement, violated.
	unsignedReplies bool
}

func withTSIG(keys dnssrv.TSIGKeys) primaryOption {
	return func(c *primaryConfig) { c.keys, c.requireTSIG = keys, true }
}

func withRcode(rcode int) primaryOption {
	return func(c *primaryConfig) { c.rcode = rcode }
}

// withRcodeFn makes the answered rcode a decision taken per request: 0 serves
// the zone, anything else refuses it. A primary that fails and then recovers
// is the only way to watch a retry schedule do its job.
func withRcodeFn(fn func() int) primaryOption {
	return func(c *primaryConfig) { c.rcodeFn = fn }
}

// withUnsignedReplies makes the primary accept and verify a correctly signed
// AXFR request and then answer it unsigned.
//
// It exists to pin a property this code depends on and does not own. A
// signed request's response must be signed (RFC 8945 §5.4), and for the AXFR
// path that rule is enforced inside miekg rather than by us: Transfer.ReadMsg
// calls TsigVerifyWithProvider *unconditionally* whenever a provider is set
// (xfr.go), unlike dns.Client.ReadMsg, which only verifies a TSIG that is
// already present (client.go). An unsigned reply therefore reaches
// stripTsig, which returns ErrNoSig on Arcount == 0.
//
// That asymmetry between two functions in one library is exactly the kind of
// thing a dependency bump can change silently, so it is pinned here rather
// than assumed.
func withUnsignedReplies() primaryOption {
	return func(c *primaryConfig) { c.unsignedReplies = true }
}

// withHold runs fn at the start of every request, before anything is
// answered.
func withHold(fn func()) primaryOption {
	return func(c *primaryConfig) { c.hold = fn }
}

// startTestPrimary serves rrs as zone over AXFR on a loopback TCP port,
// shutting down when the test ends. AXFR is TCP-only (RFC 5936 §2.2), so
// there is no UDP listener at all — a client that tried UDP would find
// nothing, which is the correct thing for it to find.
func startTestPrimary(t *testing.T, zone string, rrs []dns.RR, opts ...primaryOption) *testPrimary {
	t.Helper()

	var cfg primaryConfig
	for _, o := range opts {
		o(&cfg)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	var mu sync.Mutex
	requests := 0

	mux := dns.NewServeMux()
	mux.HandleFunc(dns.Fqdn(zone), func(w dns.ResponseWriter, r *dns.Msg) {
		mu.Lock()
		requests++
		mu.Unlock()

		if cfg.hold != nil {
			cfg.hold()
		}

		refuse := func() {
			m := new(dns.Msg)
			m.SetRcode(r, dns.RcodeRefused)
			_ = w.WriteMsg(m)
		}
		rcode := cfg.rcode
		if cfg.rcodeFn != nil {
			rcode = cfg.rcodeFn()
		}
		if rcode != 0 {
			m := new(dns.Msg)
			m.SetRcode(r, rcode)
			_ = w.WriteMsg(m)
			return
		}
		if cfg.requireTSIG && (r.IsTsig() == nil || w.TsigStatus() != nil) {
			refuse()
			return
		}
		if len(r.Question) != 1 || r.Question[0].Qtype != dns.TypeAXFR {
			refuse()
			return
		}

		// Transfer.Out signs each envelope only when the request it is given
		// carries a TSIG (xfr.go). Handing it a copy with the TSIG stripped
		// is therefore how a primary answers unsigned without reimplementing
		// the envelope loop.
		out := r
		if cfg.unsignedReplies {
			stripped := r.Copy()
			var extra []dns.RR
			for _, rr := range stripped.Extra {
				if _, isTSIG := rr.(*dns.TSIG); !isTSIG {
					extra = append(extra, rr)
				}
			}
			stripped.Extra = extra
			out = stripped
		}

		ch := make(chan *dns.Envelope)
		tr := new(dns.Transfer)
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = tr.Out(w, out, ch)
		}()
		// One envelope carrying the whole zone, SOA first and last (RFC 5936
		// §2.2). Splitting it across envelopes is the primary's choice and
		// the client must cope with either; TestTransferReadsAMultiEnvelope
		// covers the other shape.
		ch <- &dns.Envelope{RR: rrs}
		close(ch)
		wg.Wait()
		_ = w.Close()
	})

	srv := &dns.Server{Listener: ln, Handler: mux, TsigProvider: dnssrv.NewTSIGProvider(cfg.keys)}
	go func() { _ = srv.ActivateAndServe() }()
	t.Cleanup(func() { _ = srv.Shutdown() })

	return &testPrimary{
		addr: ln.Addr().String(),
		requests: func() int {
			mu.Lock()
			defer mu.Unlock()
			return requests
		},
	}
}

// deadPort returns a loopback address nothing is listening on, by binding one
// and letting it go. Nothing can guarantee the port stays free, but a
// connection to it is refused rather than hanging, which is the state the
// fall-back test needs.
func deadPort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return addr
}

// transferTestZone is the zone every test below transfers, and mustRR the
// shorthand for writing its records as master-file lines.
const (
	transferApex   = "xfer.e412.in"
	primarySerial  = 2026081201
	primaryExpire  = 604800
	primaryRefresh = 900
)

func mustRR(t *testing.T, line string) dns.RR {
	t.Helper()
	rr, err := dns.NewRR(line)
	if err != nil {
		t.Fatalf("dns.NewRR(%q): %v", line, err)
	}
	return rr
}

// primaryZoneRRs is a small but complete zone: SOA, apex NS, two A records
// sharing a name, a CNAME elsewhere, and the closing SOA.
func primaryZoneRRs(t *testing.T) []dns.RR {
	t.Helper()
	soa := mustRR(t, fmt.Sprintf("%s. 900 IN SOA ns1.%s. hostadmin.%s. %d %d 300 %d 900",
		transferApex, transferApex, transferApex, primarySerial, primaryRefresh, primaryExpire))
	return []dns.RR{
		soa,
		mustRR(t, fmt.Sprintf("%s. 3600 IN NS ns1.%s.", transferApex, transferApex)),
		mustRR(t, fmt.Sprintf("ns1.%s. 3600 IN A 10.9.0.1", transferApex)),
		mustRR(t, fmt.Sprintf("bifrost.%s. 300 IN A 10.9.0.10", transferApex)),
		mustRR(t, fmt.Sprintf("bifrost.%s. 300 IN A 10.9.0.11", transferApex)),
		mustRR(t, fmt.Sprintf("www.%s. 300 IN CNAME bifrost.%s.", transferApex, transferApex)),
		soa,
	}
}

// transferFixture is a real store holding one secondary zone, plus the
// resolver that answers from it. Which store depends on how it was built:
// sqlite by default, or whichever driver forEachDriver named for the cases
// that run on both (main_test.go).
type transferFixture struct {
	st       store.Store
	resolver *zones.Resolver
	zoneID   int64
	now      time.Time
}

// fixtureOption adjusts the zone newTransferFixture creates before it is
// written. It exists so the stub fetcher's tests (stub_test.go) can have the
// same store, resolver and clock around a zone of type "stub" rather than a
// second fixture that would drift from this one.
type fixtureOption func(*store.Zone)

// asZoneType makes the fixture's zone typ instead of "secondary".
func asZoneType(typ string) fixtureOption {
	return func(z *store.Zone) { z.Type = typ }
}

// newTransferFixture builds the fixture on sqlite, which is what the cases
// that turn on pure logic rather than on connection behaviour want.
func newTransferFixture(t *testing.T, primaries string, tsigKeyID int64, opts ...fixtureOption) *transferFixture {
	t.Helper()
	return newTransferFixtureOn(t, "sqlite", primaries, tsigKeyID, opts...)
}

// newTransferFixtureOn is newTransferFixture with the driver named, for the
// cases forEachDriver runs on both. Everything below this line is identical
// between the two, so a postgres half exercises the same fixture as its
// sqlite twin and not a second one that could drift.
func newTransferFixtureOn(t *testing.T, driver, primaries string, tsigKeyID int64, opts ...fixtureOption) *transferFixture {
	t.Helper()
	ctx := context.Background()

	st := openTestStoreOn(t, driver)

	// A secondary as Task 1's API creates one: enabled, no records, and no
	// transfer behind it — refreshed_at and expires_at both zero, which is
	// what Zone.Serving reads as "must not answer yet".
	z := store.Zone{
		Name: transferApex, Type: "secondary", Enabled: true,
		SOANS: "ns1." + transferApex, SOAMbox: "hostadmin." + transferApex,
		SOASerial: 1, SOARefresh: 900, SOARetry: 300, SOAExpire: 604800,
		SOAMinimum: 900, SOATTL: 900,
		Primaries: primaries, TSIGKeyID: tsigKeyID,
	}
	for _, o := range opts {
		o(&z)
	}
	id, err := st.Zones().AddZone(ctx, z)
	if err != nil {
		t.Fatalf("AddZone: %v", err)
	}

	f := &transferFixture{st: st, zoneID: id, now: time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC)}
	f.resolver = zones.NewResolver(st.Zones(), zones.WithNow(func() time.Time { return f.now }))
	if err := f.resolver.Reload(ctx); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	return f
}

func (f *transferFixture) transferrer() *zones.Transferrer {
	return zones.NewTransferrer(f.st.Zones(), f.st.TSIGKeys(),
		zones.WithTransferNow(func() time.Time { return f.now }),
		zones.WithReload(f.resolver.Reload))
}

func (f *transferFixture) zone(t *testing.T) store.Zone {
	t.Helper()
	z, err := f.st.Zones().Zone(context.Background(), f.zoneID)
	if err != nil {
		t.Fatalf("Zone: %v", err)
	}
	return z
}

// ask queries the resolver's own middleware, so what is asserted is what a
// client would actually receive from this server.
func (f *transferFixture) ask(t *testing.T, qname string, qtype uint16) *dns.Msg {
	t.Helper()
	return askResolver(t, f.resolver, qname, qtype)
}

// askResolver is ask without a fixture around it, for the tests that build
// their resolver themselves. Falling through to the forwarder fails the test:
// every caller is asking about a name the server is supposed to be
// authoritative for, and a fall-through is silent — the query would simply be
// answered by somebody else.
func askResolver(t *testing.T, r *zones.Resolver, qname string, qtype uint16) *dns.Msg {
	t.Helper()
	next := dnssrv.HandlerFunc(func(context.Context, *dnssrv.Request) (*dnssrv.Response, error) {
		t.Error("a query for a name inside the zone fell through to the forwarder")
		return &dnssrv.Response{Msg: new(dns.Msg), Decision: dnssrv.DecisionForwarded}, nil
	})
	resp, err := r.Middleware()(next).ServeDNS(context.Background(), request(qname, qtype))
	if err != nil {
		t.Fatalf("ServeDNS: %v", err)
	}
	return resp.Msg
}

func TestTransferInstallsTheZone(t *testing.T) {
	forEachDriver(t, func(t *testing.T, driver string) {
		primary := startTestPrimary(t, transferApex, primaryZoneRRs(t))
		f := newTransferFixtureOn(t, driver, primary.addr, 0)

		// Before the transfer the zone holds nothing and must say so by saying
		// nothing (Zone.Serving) — the state Task 1 established.
		if got := f.ask(t, "bifrost."+transferApex, dns.TypeA).Rcode; got != dns.RcodeServerFailure {
			t.Fatalf("before the transfer: rcode = %s, want SERVFAIL", dns.RcodeToString[got])
		}

		res, err := f.transferrer().Transfer(context.Background(), f.zone(t))
		if err != nil {
			t.Fatalf("transfer: %v", err)
		}

		if res.Primary.String() != primary.addr {
			t.Errorf("answered by %s, want %s", res.Primary, primary.addr)
		}
		if res.Serial != primarySerial {
			t.Errorf("serial = %d, want the primary's %d", res.Serial, primarySerial)
		}
		if res.Records != 5 {
			t.Errorf("installed %d records, want 5 (the zone's RRs, less its SOA)", res.Records)
		}

		// A transfer adopts the primary's SOA whole, serial included. A secondary
		// that invented its own serial would advertise a zone version nobody
		// else has, and its own next comparison against the primary would be
		// against a number of its own making.
		z := f.zone(t)
		if z.SOASerial != primarySerial {
			t.Errorf("stored serial = %d, want %d — a transfer must not bump", z.SOASerial, primarySerial)
		}
		if z.SOARefresh != primaryRefresh || z.SOAExpire != primaryExpire {
			t.Errorf("stored SOA timers = refresh %d expire %d, want %d/%d",
				z.SOARefresh, z.SOAExpire, primaryRefresh, primaryExpire)
		}
		if z.SOANS != "ns1."+transferApex {
			t.Errorf("stored soa_ns = %q, want %q", z.SOANS, "ns1."+transferApex)
		}

		// The two columns that decide whether the zone may answer at all.
		wantRefreshed := f.now.UnixMilli()
		wantExpires := f.now.Add(primaryExpire * time.Second).UnixMilli()
		if z.RefreshedAt != wantRefreshed {
			t.Errorf("refreshed_at = %d, want %d", z.RefreshedAt, wantRefreshed)
		}
		if z.ExpiresAt != wantExpires {
			t.Errorf("expires_at = %d, want %d", z.ExpiresAt, wantExpires)
		}

		// And the whole point: the zone answers now.
		m := f.ask(t, "bifrost."+transferApex, dns.TypeA)
		if m.Rcode != dns.RcodeSuccess || !m.Authoritative {
			t.Fatalf("after the transfer: rcode = %s aa = %v, want NOERROR aa=true", dns.RcodeToString[m.Rcode], m.Authoritative)
		}
		got := map[string]bool{}
		for _, rr := range m.Answer {
			a, ok := rr.(*dns.A)
			if !ok {
				t.Fatalf("answer carried a %T, want only A records", rr)
			}
			got[a.A.String()] = true
		}
		if !got["10.9.0.10"] || !got["10.9.0.11"] || len(got) != 2 {
			t.Errorf("bifrost A = %v, want both 10.9.0.10 and 10.9.0.11", got)
		}

		// A record whose owner arrived fully qualified is stored relative, or it
		// would be served at name.zone.zone.
		if m := f.ask(t, "www."+transferApex, dns.TypeCNAME); len(m.Answer) != 1 {
			t.Errorf("www CNAME: %d answers, want 1 — %v", len(m.Answer), m.Answer)
		}
	})
}

// The primary's SOA must not become a zone_records row: the zone's SOA lives
// on the zones row, and a second copy in the records table would be served
// beside it.
func TestTransferDoesNotStoreTheSOAAsARecord(t *testing.T) {
	primary := startTestPrimary(t, transferApex, primaryZoneRRs(t))
	f := newTransferFixture(t, primary.addr, 0)
	if _, err := f.transferrer().Transfer(context.Background(), f.zone(t)); err != nil {
		t.Fatalf("transfer: %v", err)
	}
	recs, err := f.st.Zones().Records(context.Background(), f.zoneID)
	if err != nil {
		t.Fatalf("Records: %v", err)
	}
	for _, r := range recs {
		if r.Type == "SOA" {
			t.Fatalf("an SOA was stored as a record row: %+v", r)
		}
	}
	m := f.ask(t, transferApex, dns.TypeSOA)
	if len(m.Answer) != 1 {
		t.Fatalf("apex SOA: %d answers, want exactly 1 — %v", len(m.Answer), m.Answer)
	}
	if soa := m.Answer[0].(*dns.SOA); soa.Serial != primarySerial {
		t.Errorf("served serial = %d, want %d", soa.Serial, primarySerial)
	}
}

// A primary sending what POST /zones/{id}/records would refuse does not get a
// back door. The zone is left exactly as it was.
func TestTransferRejectsARecordTheAPIWouldRefuse(t *testing.T) {
	soa := mustRR(t, fmt.Sprintf("%s. 900 IN SOA ns1.%s. hostadmin.%s. %d 900 300 %d 900",
		transferApex, transferApex, transferApex, primarySerial, primaryExpire))
	// RFC 1034 §3.6.2: a CNAME cannot share a name with another record.
	primary := startTestPrimary(t, transferApex, []dns.RR{
		soa,
		mustRR(t, fmt.Sprintf("%s. 3600 IN NS ns1.%s.", transferApex, transferApex)),
		mustRR(t, fmt.Sprintf("bifrost.%s. 300 IN A 10.9.0.10", transferApex)),
		mustRR(t, fmt.Sprintf("bifrost.%s. 300 IN CNAME nas.%s.", transferApex, transferApex)),
		soa,
	})
	f := newTransferFixture(t, primary.addr, 0)

	_, err := f.transferrer().Transfer(context.Background(), f.zone(t))
	if err == nil {
		t.Fatal("transfer succeeded; want the CNAME beside an A to be refused")
	}
	// The refusal has to name what and why, or an operator sees "transfer
	// failed" against a zone of 400 records with nowhere to look.
	msg := err.Error()
	for _, want := range []string{"bifrost", "CNAME"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error does not mention %q: %v", want, msg)
		}
	}

	// Nothing was written, so the zone still may not answer.
	z := f.zone(t)
	if z.RefreshedAt != 0 || z.SOASerial != 1 {
		t.Errorf("a rejected transfer changed the zone row: refreshed_at = %d serial = %d", z.RefreshedAt, z.SOASerial)
	}
	recs, err := f.st.Zones().Records(context.Background(), f.zoneID)
	if err != nil {
		t.Fatalf("Records: %v", err)
	}
	if len(recs) != 0 {
		t.Errorf("a rejected transfer wrote %d records", len(recs))
	}
}

// An RR the primary sends for a name outside the zone it was asked for is not
// ours to hold. Accepted, it would be stored under a name relative to our
// apex and served as though the primary had said so.
func TestTransferRejectsAnOutOfZoneRecord(t *testing.T) {
	soa := mustRR(t, fmt.Sprintf("%s. 900 IN SOA ns1.%s. hostadmin.%s. %d 900 300 %d 900",
		transferApex, transferApex, transferApex, primarySerial, primaryExpire))
	primary := startTestPrimary(t, transferApex, []dns.RR{
		soa,
		mustRR(t, fmt.Sprintf("%s. 3600 IN NS ns1.%s.", transferApex, transferApex)),
		mustRR(t, "login.bank.example. 300 IN A 10.9.0.66"),
		soa,
	})
	f := newTransferFixture(t, primary.addr, 0)

	_, err := f.transferrer().Transfer(context.Background(), f.zone(t))
	if err == nil {
		t.Fatal("transfer succeeded; want the out-of-zone record to be refused")
	}
	if !strings.Contains(err.Error(), "login.bank.example") {
		t.Errorf("error does not name the offending owner: %v", err)
	}
}

func TestTransferFallsBackToTheSecondPrimary(t *testing.T) {
	good := startTestPrimary(t, transferApex, primaryZoneRRs(t))
	dead := deadPort(t)
	f := newTransferFixture(t, dead+", "+good.addr, 0)

	res, err := f.transferrer().Transfer(context.Background(), f.zone(t))
	if err != nil {
		t.Fatalf("transfer: %v", err)
	}
	if res.Primary.String() != good.addr {
		t.Errorf("answered by %s, want the second primary %s", res.Primary, good.addr)
	}
	if got := f.zone(t).SOASerial; got != primarySerial {
		t.Errorf("serial = %d, want %d", got, primarySerial)
	}
}

// A primary that is reachable and refuses is the other half of "try the list
// in order" — an ACL that has not been updated yet looks like this, not like
// a closed port.
func TestTransferFallsBackPastARefusingPrimary(t *testing.T) {
	refuser := startTestPrimary(t, transferApex, nil, withRcode(dns.RcodeRefused))
	good := startTestPrimary(t, transferApex, primaryZoneRRs(t))
	f := newTransferFixture(t, refuser.addr+","+good.addr, 0)

	res, err := f.transferrer().Transfer(context.Background(), f.zone(t))
	if err != nil {
		t.Fatalf("transfer: %v", err)
	}
	if res.Primary.String() != good.addr {
		t.Errorf("answered by %s, want %s", res.Primary, good.addr)
	}
	if refuser.requests() != 1 {
		t.Errorf("the refusing primary saw %d requests, want 1 — the list is tried in order", refuser.requests())
	}
}

// Every primary failing has to say why each one did, not just that they all
// did: the two failures are usually different and the difference is the fix.
func TestTransferNamesEveryPrimaryThatFailed(t *testing.T) {
	refuser := startTestPrimary(t, transferApex, nil, withRcode(dns.RcodeRefused))
	dead := deadPort(t)
	f := newTransferFixture(t, dead+","+refuser.addr, 0)

	_, err := f.transferrer().Transfer(context.Background(), f.zone(t))
	if err == nil {
		t.Fatal("transfer succeeded with no working primary")
	}
	for _, want := range []string{dead, refuser.addr} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not name primary %s: %v", want, err)
		}
	}
}

// storeTSIGKey writes a key and returns its id, in the canonical form the
// store holds and the provider looks up by.
func storeTSIGKey(t *testing.T, st store.Store, name string) int64 {
	t.Helper()
	secret := base64.StdEncoding.EncodeToString([]byte("a-transfer-secret-of-sufficient-length"))
	id, err := st.TSIGKeys().Create(context.Background(), store.TSIGKey{
		Name: dns.CanonicalName(name), Algorithm: dns.HmacSHA256, Secret: secret,
		CreatedAt: time.Now().UnixMilli(),
	})
	if err != nil {
		t.Fatalf("TSIGKeys().Create: %v", err)
	}
	return id
}

func TestTransferSignsWithTSIGWhenTheZoneNamesAKey(t *testing.T) {
	// The store is built first so the primary can verify against the same
	// key the secondary signs with — one key, two ends, as a real pair is
	// configured.
	f := newTransferFixture(t, "127.0.0.1:0", 0)
	keyID := storeTSIGKey(t, f.st, "xfer-key."+transferApex)

	primary := startTestPrimary(t, transferApex, primaryZoneRRs(t), withTSIG(f.st.TSIGKeys()))

	// Point the zone at the primary now that it has an address, and at the
	// key, the way a PATCH would.
	z := f.zone(t)
	z.Primaries, z.TSIGKeyID = primary.addr, keyID
	if err := f.st.Zones().UpdateZone(context.Background(), z); err != nil {
		t.Fatalf("UpdateZone: %v", err)
	}

	res, err := f.transferrer().Transfer(context.Background(), f.zone(t))
	if err != nil {
		t.Fatalf("signed transfer: %v", err)
	}
	if res.Serial != primarySerial {
		t.Errorf("serial = %d, want %d", res.Serial, primarySerial)
	}
	if m := f.ask(t, "bifrost."+transferApex, dns.TypeA); m.Rcode != dns.RcodeSuccess {
		t.Errorf("after a signed transfer: rcode = %s, want NOERROR", dns.RcodeToString[m.Rcode])
	}
}

// A primary that verifies our signature and then answers unsigned must be
// refused. RFC 8945 §5.4: a signed request's response has to be signed too.
//
// Without this, TSIG would authenticate the *request* and contribute nothing
// to the *reply* — an attacker who can answer before the real primary hands
// us a whole zone we then install. That is the identical shape as the defect
// found in the SOA probe and the NOTIFY sender during D4, both of which use
// dns.Client, whose ReadMsg verifies only a TSIG that is already present.
//
// The AXFR path is safe for a reason we do not control: Transfer.ReadMsg
// verifies unconditionally once a provider is set, so an unsigned envelope
// fails in stripTsig with ErrNoSig. This test is the pin on that, so a
// dependency bump that aligned the two ReadMsg implementations would fail
// here rather than silently open the hole.
func TestTransferRejectsAnUnsignedReplyToASignedRequest(t *testing.T) {
	f := newTransferFixture(t, "127.0.0.1:0", 0)
	keyID := storeTSIGKey(t, f.st, "xfer-key."+transferApex)

	primary := startTestPrimary(t, transferApex, primaryZoneRRs(t),
		withTSIG(f.st.TSIGKeys()), withUnsignedReplies())

	z := f.zone(t)
	z.Primaries, z.TSIGKeyID = primary.addr, keyID
	if err := f.st.Zones().UpdateZone(context.Background(), z); err != nil {
		t.Fatalf("UpdateZone: %v", err)
	}

	_, err := f.transferrer().Transfer(context.Background(), f.zone(t))
	if err == nil {
		t.Fatal("transfer succeeded against a primary that answered unsigned")
	}
	// The request has to have reached the primary and been accepted by it —
	// otherwise this passes for the wrong reason, on a primary that refused
	// us outright or was never contacted at all.
	if primary.requests() == 0 {
		t.Fatal("the primary was never asked, so nothing about signing was tested")
	}
	if !errors.Is(err, dns.ErrNoSig) {
		t.Errorf("error = %v, want it to wrap dns.ErrNoSig", err)
	}

	// Nothing was installed: a refused transfer must leave the zone exactly
	// as it was, holding no records at all.
	recs, rerr := f.st.Zones().Records(context.Background(), f.zoneID)
	if rerr != nil {
		t.Fatalf("Records: %v", rerr)
	}
	if len(recs) != 0 {
		t.Errorf("installed %d records from an unsigned transfer, want 0", len(recs))
	}
}

// The same primary, unsigned. This is what makes the test above mean
// something: without it, a transfer that silently sent no TSIG would pass it
// if the primary happened not to care.
func TestTransferAgainstATSIGPrimaryFailsUnsigned(t *testing.T) {
	f := newTransferFixture(t, "127.0.0.1:0", 0)
	primary := startTestPrimary(t, transferApex, primaryZoneRRs(t), withTSIG(f.st.TSIGKeys()))

	z := f.zone(t)
	z.Primaries = primary.addr // and no tsig_key_id
	if err := f.st.Zones().UpdateZone(context.Background(), z); err != nil {
		t.Fatalf("UpdateZone: %v", err)
	}

	if _, err := f.transferrer().Transfer(context.Background(), f.zone(t)); err == nil {
		t.Fatal("unsigned transfer succeeded against a primary that requires TSIG")
	}
}

// A zone naming a key that is no longer there fails saying so, rather than
// quietly transferring unsigned — the residual the D1 delete guard documents.
func TestTransferFailsWhenTheZonesKeyIsGone(t *testing.T) {
	primary := startTestPrimary(t, transferApex, primaryZoneRRs(t))
	f := newTransferFixture(t, primary.addr, 4242)

	_, err := f.transferrer().Transfer(context.Background(), f.zone(t))
	if err == nil {
		t.Fatal("transfer succeeded with a tsig_key_id nothing answers to")
	}
	if !strings.Contains(err.Error(), "4242") {
		t.Errorf("error does not name the missing key: %v", err)
	}
	if primary.requests() != 0 {
		t.Errorf("the primary was contacted %d times; a zone that cannot sign must not ask", primary.requests())
	}
}

// A re-transfer is a replace, not a merge: what the primary dropped goes.
func TestTransferReplacesRatherThanMerges(t *testing.T) {
	forEachDriver(t, func(t *testing.T, driver string) {
		first := startTestPrimary(t, transferApex, primaryZoneRRs(t))
		f := newTransferFixtureOn(t, driver, first.addr, 0)
		if _, err := f.transferrer().Transfer(context.Background(), f.zone(t)); err != nil {
			t.Fatalf("first transfer: %v", err)
		}

		// A second primary serving a smaller zone at a higher serial.
		soa := mustRR(t, fmt.Sprintf("%s. 900 IN SOA ns1.%s. hostadmin.%s. %d 900 300 %d 900",
			transferApex, transferApex, transferApex, primarySerial+1, primaryExpire))
		second := startTestPrimary(t, transferApex, []dns.RR{
			soa,
			mustRR(t, fmt.Sprintf("%s. 3600 IN NS ns1.%s.", transferApex, transferApex)),
			mustRR(t, fmt.Sprintf("ns1.%s. 3600 IN A 10.9.0.1", transferApex)),
			soa,
		})
		z := f.zone(t)
		z.Primaries = second.addr
		if err := f.st.Zones().UpdateZone(context.Background(), z); err != nil {
			t.Fatalf("UpdateZone: %v", err)
		}
		if _, err := f.transferrer().Transfer(context.Background(), f.zone(t)); err != nil {
			t.Fatalf("second transfer: %v", err)
		}

		if m := f.ask(t, "bifrost."+transferApex, dns.TypeA); m.Rcode != dns.RcodeNameError {
			t.Errorf("a name the second transfer dropped: rcode = %s, want NXDOMAIN", dns.RcodeToString[m.Rcode])
		}
		if got := f.zone(t).SOASerial; got != primarySerial+1 {
			t.Errorf("serial = %d, want %d", got, primarySerial+1)
		}
	})
}

// An unchanged zone re-transferred writes no record statements at all — the
// reason the install diffs rather than deleting and re-adding every row on
// every refresh.
func TestTransferOfAnUnchangedZoneTouchesNoRecords(t *testing.T) {
	forEachDriver(t, func(t *testing.T, driver string) {
		primary := startTestPrimary(t, transferApex, primaryZoneRRs(t))
		f := newTransferFixtureOn(t, driver, primary.addr, 0)
		tr := f.transferrer()
		if _, err := tr.Transfer(context.Background(), f.zone(t)); err != nil {
			t.Fatalf("first transfer: %v", err)
		}
		before, err := f.st.Zones().Records(context.Background(), f.zoneID)
		if err != nil {
			t.Fatalf("Records: %v", err)
		}
		if _, err := tr.Transfer(context.Background(), f.zone(t)); err != nil {
			t.Fatalf("second transfer: %v", err)
		}
		after, err := f.st.Zones().Records(context.Background(), f.zoneID)
		if err != nil {
			t.Fatalf("Records: %v", err)
		}
		if len(before) != len(after) {
			t.Fatalf("record count changed across an identical transfer: %d then %d", len(before), len(after))
		}
		for i := range before {
			if before[i].ID != after[i].ID {
				t.Errorf("row %d changed id across an identical transfer: %d then %d — the install is not diffing",
					i, before[i].ID, after[i].ID)
			}
		}
	})
}

// A zone split across several envelopes is the shape a large transfer
// actually arrives in, and the client must not assume one message.
func TestTransferReadsAMultiEnvelopeZone(t *testing.T) {
	rrs := primaryZoneRRs(t)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	mux := dns.NewServeMux()
	mux.HandleFunc(dns.Fqdn(transferApex), func(w dns.ResponseWriter, r *dns.Msg) {
		ch := make(chan *dns.Envelope)
		tr := new(dns.Transfer)
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = tr.Out(w, r, ch)
		}()
		for _, rr := range rrs {
			ch <- &dns.Envelope{RR: []dns.RR{rr}}
		}
		close(ch)
		wg.Wait()
		_ = w.Close()
	})
	srv := &dns.Server{Listener: ln, Handler: mux}
	go func() { _ = srv.ActivateAndServe() }()
	t.Cleanup(func() { _ = srv.Shutdown() })

	f := newTransferFixture(t, ln.Addr().String(), 0)
	res, err := f.transferrer().Transfer(context.Background(), f.zone(t))
	if err != nil {
		t.Fatalf("transfer: %v", err)
	}
	if res.Records != 5 {
		t.Errorf("installed %d records, want 5", res.Records)
	}
}

// A zone whose SOA expires the moment it lands would install and answer
// nothing — a transfer that reports success into silence.
func TestTransferRejectsAnSOAThatExpiresImmediately(t *testing.T) {
	soa := mustRR(t, fmt.Sprintf("%s. 900 IN SOA ns1.%s. hostadmin.%s. %d 900 300 0 900",
		transferApex, transferApex, transferApex, primarySerial))
	primary := startTestPrimary(t, transferApex, []dns.RR{
		soa,
		mustRR(t, fmt.Sprintf("%s. 3600 IN NS ns1.%s.", transferApex, transferApex)),
		soa,
	})
	f := newTransferFixture(t, primary.addr, 0)

	_, err := f.transferrer().Transfer(context.Background(), f.zone(t))
	if err == nil {
		t.Fatal("transfer succeeded with an SOA expire of 0")
	}
	if !strings.Contains(err.Error(), "expire") {
		t.Errorf("error does not name the SOA field at fault: %v", err)
	}
}

// The closing SOA delimits the zone and says which version of it just
// arrived. A primary that edited the zone mid-stream sends a different serial
// on the way out — BIND aborts such a transfer — and installing it anyway
// files a half-old, half-new zone under the opening serial, which every
// downstream comparison then reads as "already have that one".
func TestTransferRejectsAClosingSOAWithADifferentSerial(t *testing.T) {
	soaAt := func(serial uint32) dns.RR {
		return mustRR(t, fmt.Sprintf("%s. 900 IN SOA ns1.%s. hostadmin.%s. %d %d 300 %d 900",
			transferApex, transferApex, transferApex, serial, primaryRefresh, primaryExpire))
	}
	primary := startTestPrimary(t, transferApex, []dns.RR{
		soaAt(primarySerial),
		mustRR(t, fmt.Sprintf("%s. 3600 IN NS ns1.%s.", transferApex, transferApex)),
		soaAt(primarySerial + 1),
	})
	f := newTransferFixture(t, primary.addr, 0)

	_, err := f.transferrer().Transfer(context.Background(), f.zone(t))
	if err == nil {
		t.Fatal("transfer succeeded with a closing SOA at a different serial")
	}
	if !strings.Contains(err.Error(), "serial") {
		t.Errorf("error does not name what disagreed: %v", err)
	}
	if z := f.zone(t); z.RefreshedAt != 0 {
		t.Error("a refused transfer stamped refreshed_at")
	}
}

// A primary answering something that is not a zone must not leave the
// secondary holding an empty one.
func TestTransferRejectsAnAnswerThatDoesNotStartWithTheSOA(t *testing.T) {
	primary := startTestPrimary(t, transferApex, []dns.RR{
		mustRR(t, fmt.Sprintf("%s. 3600 IN NS ns1.%s.", transferApex, transferApex)),
	})
	f := newTransferFixture(t, primary.addr, 0)
	if _, err := f.transferrer().Transfer(context.Background(), f.zone(t)); err == nil {
		t.Fatal("transfer succeeded on an answer with no SOA")
	}
}

// A primary that is not a secondary's is not transferred from at all: the
// call is a programming error, and installing a primary zone's own contents
// from somewhere else would destroy data this server owns.
func TestTransferRefusesANonSecondaryZone(t *testing.T) {
	primary := startTestPrimary(t, transferApex, primaryZoneRRs(t))
	f := newTransferFixture(t, primary.addr, 0)
	z := f.zone(t)
	z.Type = "primary"
	if _, err := f.transferrer().Transfer(context.Background(), z); err == nil {
		t.Fatal("transferred into a primary zone")
	}
	if primary.requests() != 0 {
		t.Errorf("the primary was contacted %d times for a non-secondary zone", primary.requests())
	}
}

// A zone larger than the client will accept is abandoned rather than
// allocated: how much this process holds must not be the primary's decision.
func TestTransferRefusesAZoneOverTheRecordLimit(t *testing.T) {
	primary := startTestPrimary(t, transferApex, primaryZoneRRs(t))
	f := newTransferFixture(t, primary.addr, 0)
	tr := zones.NewTransferrer(f.st.Zones(), f.st.TSIGKeys(),
		zones.WithTransferNow(func() time.Time { return f.now }),
		zones.WithMaxRecords(3))

	_, err := tr.Transfer(context.Background(), f.zone(t))
	if err == nil {
		t.Fatal("transfer succeeded past the record limit")
	}
	if !strings.Contains(err.Error(), "limit") {
		t.Errorf("error does not say what was exceeded: %v", err)
	}
	if f.zone(t).RefreshedAt != 0 {
		t.Error("an abandoned transfer installed a zone")
	}
}

// A cancelled context stops the transfer rather than being noticed only
// after the whole zone has been read and installed.
func TestTransferHonoursContextCancellation(t *testing.T) {
	primary := startTestPrimary(t, transferApex, primaryZoneRRs(t))
	f := newTransferFixture(t, primary.addr, 0)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := f.transferrer().Transfer(ctx, f.zone(t))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if f.zone(t).RefreshedAt != 0 {
		t.Error("a cancelled transfer installed a zone")
	}
}

// emptyPrimary serves a correctly delimited transfer that carries no zone:
// the apex SOA and nothing else. It is well-formed on the wire, so nothing
// in dns.Transfer.In objects to it.
func emptyPrimary(t *testing.T) *testPrimary {
	t.Helper()
	soa := mustRR(t, fmt.Sprintf("%s. 900 IN SOA ns1.%s. hostadmin.%s. %d 900 300 %d 900",
		transferApex, transferApex, transferApex, primarySerial, primaryExpire))
	return startTestPrimary(t, transferApex, []dns.RR{soa, soa})
}

// A zone's apex has NS records by definition (RFC 1034 §4.2.1), so a
// transfer that carries none did not carry a zone. BIND and NSD both refuse
// to load one, and dnsaur must too — for a reason sharper than conformance.
//
// Installed, an empty transfer stamps refreshed_at and leaves the zone
// holding nothing, and a zone that holds nothing and is entitled to answer
// gives an AUTHORITATIVE NXDOMAIN with its SOA for every name beneath the
// apex. RFC 8020 makes that a claim about the whole subtree, and the SOA
// tells every resolver to cache it. That is precisely the black hole
// Zone.Serving's doc comment says must never happen, reached through the
// front door instead: a misconfigured or hostile primary gets to erase a
// zone and have this server assert the erasure as fact.
func TestTransferRejectsATransferCarryingNoApexNS(t *testing.T) {
	primary := emptyPrimary(t)
	f := newTransferFixture(t, primary.addr, 0)

	_, err := f.transferrer().Transfer(context.Background(), f.zone(t))
	if err == nil {
		t.Fatal("transfer succeeded with no apex NS; want it refused")
	}
	if !strings.Contains(err.Error(), "NS") {
		t.Errorf("error does not name what was missing: %v", err)
	}

	// Nothing installed, so the zone is still one that has never
	// transferred — which answers nothing rather than denying everything.
	if z := f.zone(t); z.RefreshedAt != 0 {
		t.Errorf("refreshed_at = %d, want 0 — a refused transfer must not stamp one", z.RefreshedAt)
	}
	m := f.ask(t, "bifrost."+transferApex, dns.TypeA)
	if m.Rcode != dns.RcodeServerFailure {
		t.Errorf("rcode = %s aa = %v, want SERVFAIL — an authoritative NXDOMAIN here black-holes the whole suffix (RFC 8020)",
			dns.RcodeToString[m.Rcode], m.Authoritative)
	}
}

// The guard is "an apex NS", not "any record at all", and this is what makes
// the difference observable: a stream of A records with no NS is just as much
// not-a-zone as an empty one, and the weaker check would install it. Without
// this test, replacing the condition with `len(recs) > 0` passes everything.
func TestTransferRejectsRecordsWithNoApexNS(t *testing.T) {
	soa := mustRR(t, fmt.Sprintf("%s. 900 IN SOA ns1.%s. hostadmin.%s. %d 900 300 %d 900",
		transferApex, transferApex, transferApex, primarySerial, primaryExpire))
	primary := startTestPrimary(t, transferApex, []dns.RR{
		soa,
		mustRR(t, fmt.Sprintf("bifrost.%s. 300 IN A 10.9.0.10", transferApex)),
		mustRR(t, fmt.Sprintf("nas.%s. 300 IN A 10.9.0.20", transferApex)),
		soa,
	})
	f := newTransferFixture(t, primary.addr, 0)

	_, err := f.transferrer().Transfer(context.Background(), f.zone(t))
	if err == nil {
		t.Fatal("transfer succeeded carrying records but no apex NS; want it refused")
	}
	if !strings.Contains(err.Error(), "NS") {
		t.Errorf("error does not name what was missing: %v", err)
	}
	if z := f.zone(t); z.RefreshedAt != 0 {
		t.Errorf("refreshed_at = %d, want 0", z.RefreshedAt)
	}
}

// An NS somewhere below the apex is not the zone's own NS — it is a
// delegation, and a zone made only of delegations still has no apex.
func TestTransferRejectsADelegationOnlyZone(t *testing.T) {
	soa := mustRR(t, fmt.Sprintf("%s. 900 IN SOA ns1.%s. hostadmin.%s. %d 900 300 %d 900",
		transferApex, transferApex, transferApex, primarySerial, primaryExpire))
	primary := startTestPrimary(t, transferApex, []dns.RR{
		soa,
		mustRR(t, fmt.Sprintf("sub.%s. 3600 IN NS ns1.elsewhere.example.", transferApex)),
		soa,
	})
	f := newTransferFixture(t, primary.addr, 0)

	if _, err := f.transferrer().Transfer(context.Background(), f.zone(t)); err == nil {
		t.Fatal("transfer succeeded with an NS below the apex but none at it")
	}
}

// The same refusal from the other starting state, which is the one that
// costs something: a zone that is already serving must keep serving exactly
// what it had. An empty transfer that got through would delete every record
// and then deny the names it had been answering a moment earlier.
func TestAnEmptyTransferLeavesAServingZoneIntact(t *testing.T) {
	forEachDriver(t, func(t *testing.T, driver string) {
		good := startTestPrimary(t, transferApex, primaryZoneRRs(t))
		f := newTransferFixtureOn(t, driver, good.addr, 0)
		if _, err := f.transferrer().Transfer(context.Background(), f.zone(t)); err != nil {
			t.Fatalf("first transfer: %v", err)
		}
		before := f.zone(t)

		empty := emptyPrimary(t)
		z := before
		z.Primaries = empty.addr
		if err := f.st.Zones().UpdateZone(context.Background(), z); err != nil {
			t.Fatalf("UpdateZone: %v", err)
		}
		if _, err := f.transferrer().Transfer(context.Background(), f.zone(t)); err == nil {
			t.Fatal("empty transfer succeeded against a serving zone")
		}

		recs, err := f.st.Zones().Records(context.Background(), f.zoneID)
		if err != nil {
			t.Fatalf("Records: %v", err)
		}
		if len(recs) != 5 {
			t.Errorf("the zone holds %d records after a refused transfer, want its original 5", len(recs))
		}
		if after := f.zone(t); after.RefreshedAt != before.RefreshedAt || after.SOASerial != before.SOASerial {
			t.Errorf("a refused transfer moved the zone row: refreshed_at %d -> %d, serial %d -> %d",
				before.RefreshedAt, after.RefreshedAt, before.SOASerial, after.SOASerial)
		}
		m := f.ask(t, "bifrost."+transferApex, dns.TypeA)
		if m.Rcode != dns.RcodeSuccess || len(m.Answer) != 2 {
			t.Errorf("after a refused transfer: rcode = %s with %d answers, want NOERROR with 2 — the zone must answer as before",
				dns.RcodeToString[m.Rcode], len(m.Answer))
		}
	})
}

// refreshed_at is "we checked" and moves every cycle; modified_at is "the
// contents changed" and must not, or every zone looks edited on every
// refresh and the no-op diff above is undone in the one column an operator
// reads to find out what actually happened.
func TestAnUnchangedTransferDoesNotTouchModifiedAt(t *testing.T) {
	primary := startTestPrimary(t, transferApex, primaryZoneRRs(t))
	f := newTransferFixture(t, primary.addr, 0)
	tr := f.transferrer()
	if _, err := tr.Transfer(context.Background(), f.zone(t)); err != nil {
		t.Fatalf("first transfer: %v", err)
	}
	before := f.zone(t)

	f.now = f.now.Add(time.Hour)
	if _, err := tr.Transfer(context.Background(), f.zone(t)); err != nil {
		t.Fatalf("second transfer: %v", err)
	}
	after := f.zone(t)

	if after.ModifiedAt != before.ModifiedAt {
		t.Errorf("modified_at moved on an unchanged transfer: %d -> %d", before.ModifiedAt, after.ModifiedAt)
	}
	// The check that stops "never write modified_at" from passing this.
	if after.RefreshedAt == before.RefreshedAt {
		t.Errorf("refreshed_at did not move: still %d — the transfer did happen", after.RefreshedAt)
	}
}

func TestAChangedTransferDoesTouchModifiedAt(t *testing.T) {
	first := startTestPrimary(t, transferApex, primaryZoneRRs(t))
	f := newTransferFixture(t, first.addr, 0)
	if _, err := f.transferrer().Transfer(context.Background(), f.zone(t)); err != nil {
		t.Fatalf("first transfer: %v", err)
	}
	before := f.zone(t)

	soa := mustRR(t, fmt.Sprintf("%s. 900 IN SOA ns1.%s. hostadmin.%s. %d 900 300 %d 900",
		transferApex, transferApex, transferApex, primarySerial+1, primaryExpire))
	second := startTestPrimary(t, transferApex, []dns.RR{
		soa,
		mustRR(t, fmt.Sprintf("%s. 3600 IN NS ns1.%s.", transferApex, transferApex)),
		mustRR(t, fmt.Sprintf("ns1.%s. 3600 IN A 10.9.0.1", transferApex)),
		soa,
	})
	z := before
	z.Primaries = second.addr
	if err := f.st.Zones().UpdateZone(context.Background(), z); err != nil {
		t.Fatalf("UpdateZone: %v", err)
	}
	f.now = f.now.Add(time.Hour)
	if _, err := f.transferrer().Transfer(context.Background(), f.zone(t)); err != nil {
		t.Fatalf("second transfer: %v", err)
	}
	if after := f.zone(t); after.ModifiedAt != f.now.UnixMilli() {
		t.Errorf("modified_at = %d after a transfer that dropped records, want %d", after.ModifiedAt, f.now.UnixMilli())
	}
}

// unusableKeys is a TSIGKeys whose methods dereference the receiver, so a
// nil one panics — the shape store.TSIGKeyStore's own implementation has.
type unusableKeys struct{ keys map[int64]store.TSIGKey }

func (u *unusableKeys) Get(_ context.Context, id int64) (store.TSIGKey, bool, error) {
	k, ok := u.keys[id]
	return k, ok, nil
}

func (u *unusableKeys) ByName(_ context.Context, name string) (store.TSIGKey, bool, error) {
	for _, k := range u.keys {
		if k.Name == name {
			return k, true, nil
		}
	}
	return store.TSIGKey{}, false, nil
}

// A nil *T stored in a TSIGKeys interface is not nil, so `keys != nil` lets
// it through and every call on it panics — at the first signed transfer, a
// long way from the wiring mistake that caused it. Both halves are asserted
// because "reject the key store" and "still transfer zones that need no key"
// are different claims.
func TestTransferSurvivesATypedNilKeyStore(t *testing.T) {
	primary := startTestPrimary(t, transferApex, primaryZoneRRs(t))
	f := newTransferFixture(t, primary.addr, 0)
	var nilKeys *unusableKeys
	newTr := func() *zones.Transferrer {
		return zones.NewTransferrer(f.st.Zones(), nilKeys,
			zones.WithTransferNow(func() time.Time { return f.now }),
			zones.WithReload(f.resolver.Reload))
	}

	// A zone that needs no key transfers normally.
	if _, err := newTr().Transfer(context.Background(), f.zone(t)); err != nil {
		t.Fatalf("unsigned transfer with a typed-nil key store: %v", err)
	}

	// A zone that needs one is told so, rather than panicking.
	z := f.zone(t)
	z.TSIGKeyID = 7
	_, err := newTr().Transfer(context.Background(), z)
	if err == nil {
		t.Fatal("a zone naming a key transferred against a key store that cannot answer")
	}
	if !strings.Contains(err.Error(), "key store") {
		t.Errorf("error does not name the missing key store: %v", err)
	}
}
