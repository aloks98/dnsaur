package zones_test

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aloks98/dnsaur/internal/dnssrv"
	"github.com/aloks98/dnsaur/internal/store"
	"github.com/aloks98/dnsaur/internal/zones"
	"github.com/miekg/dns"
)

// The gate, tested the way §9.5.9 asks for: a real client against a real
// dnssrv.Server against a real store, with no fake at any seam. D2's tests
// pointed the other way — dnsaur pulling from a test primary — so nothing
// here reuses that fixture.
//
// Every row of §9.5.5's table that this change implements has its own test
// below, and each asserts the rcode itself rather than "the transfer failed".
// NOTAUTH and REFUSED are the two an operator reads differently: NOTAUTH says
// "you asked the wrong server", REFUSED says "you asked the right one and it
// will not".

const xfrApex = "e412.in"

// xfrFudge is the TSIG fudge every signed request below carries, in seconds.
const xfrFudge = 300

// xfrZone is the primary the tests ask for: one apex, one SOA, and whatever
// ACL the test is about.
func xfrZone(acl string) store.Zone {
	return store.Zone{
		Name: xfrApex, Type: "primary", Enabled: true, AllowTransfer: acl,
		SOANS: "ns1." + xfrApex, SOAMbox: "hostmaster." + xfrApex,
		SOASerial: 3, SOARefresh: 900, SOARetry: 300, SOAExpire: 604800,
		SOAMinimum: 900, SOATTL: 900,
	}
}

// xfrRecords is the zone's content: an apex NS, one host, and one disabled
// row that must never leave this server.
func xfrRecords() []store.ZoneRecord {
	return []store.ZoneRecord{
		{Name: "@", Type: "NS", TTL: 300, RData: "ns1." + xfrApex + ".", Enabled: true},
		{Name: "bifrost", Type: "A", TTL: 300, RData: "10.0.0.1", Enabled: true},
		{Name: "disabled", Type: "A", TTL: 300, RData: "10.0.0.9", Enabled: false},
	}
}

// xfrBigRecords is a zone that cannot be transferred in one message: 2,000 A
// records under 20-character labels, about 44 uncompressed bytes each. That
// is roughly 88 KiB, past both the 16 KiB envelope target and the 65535 a
// single DNS message can hold — which is the shape that used to fail inside
// the writer with the peer receiving nothing at all.
func xfrBigRecords(n int) []store.ZoneRecord {
	recs := make([]store.ZoneRecord, 0, n+1)
	recs = append(recs, store.ZoneRecord{Name: "@", Type: "NS", TTL: 300, RData: "ns1." + xfrApex + ".", Enabled: true})
	for i := range n {
		recs = append(recs, store.ZoneRecord{
			Name: fmt.Sprintf("host%04d-of-big-zone", i), Type: "A", TTL: 300,
			RData: fmt.Sprintf("10.%d.%d.%d", i>>16&0xff, i>>8&0xff, i&0xff), Enabled: true,
		})
	}
	return recs
}

// xfrFixture is a running dnsaur that serves transfers: a store, a resolver
// reloaded from it, a TransferServer, and a dnssrv.Server on 127.0.0.1:0.
type xfrFixture struct {
	st     store.Store
	res    *zones.Resolver
	addr   string
	zoneID int64
	// probeQueries counts queries this fixture answered through the ordinary
	// resolver pipeline rather than a transfer or notify intercept. Non-nil
	// only with withResolvingPipeline; see its own comment for what this is
	// for.
	probeQueries *atomic.Int64
}

type xfrConfig struct {
	server []zones.TransferServerOption
	keys   dnssrv.TSIGKeys // nil means the store's own key store
	noKeys bool            // attach no key store at all
	// resolving swaps the fixture's terminal pipeline handler for one that
	// actually answers through the resolver, rather than failing the test on
	// any reach. See withResolvingPipeline.
	resolving bool
}

type xfrOption func(*xfrConfig)

// withXFRNow drives the clock the gate reads when deciding whether a
// secondary's data has expired, so a deadline is crossed rather than waited
// for.
func withXFRNow(now func() time.Time) xfrOption {
	return func(c *xfrConfig) {
		c.server = append(c.server, zones.WithTransferServerNow(now))
	}
}

// withMaxConcurrentTransfers overrides the concurrency cap (§9.5.7), so a
// test can cross it without starting DefaultMaxConcurrentTransfers real
// transfers.
func withMaxConcurrentTransfers(n int) xfrOption {
	return func(c *xfrConfig) {
		c.server = append(c.server, zones.WithMaxConcurrentTransfers(n))
	}
}

// withTransferHold installs a hook that runs once a transfer has taken its
// concurrency slot and is about to stream, so a test can hold that slot open
// on demand — the same job D2's testPrimary withHold does from the other
// side of a transfer.
func withTransferHold(fn func()) xfrOption {
	return func(c *xfrConfig) {
		c.server = append(c.server, zones.WithTransferHook(fn))
	}
}

// withReplicaAllow installs the hook that admits a registered replica's
// pull (§6 of the config-sync design), standing in for the App method that
// answers it from the replica registry.
func withReplicaAllow(f zones.ReplicaAllow) xfrOption {
	return func(c *xfrConfig) {
		c.server = append(c.server, zones.WithReplicaAllow(f))
	}
}

// withKeyStore replaces the key store TSIG is verified against.
func withKeyStore(keys dnssrv.TSIGKeys) xfrOption {
	return func(c *xfrConfig) { c.keys = keys }
}

// withoutKeyStore starts the server with no TSIG provider at all, which is
// what makes RequireTSIG report ErrTSIGUnavailable: nothing verified this
// message, signed or not.
func withoutKeyStore() xfrOption {
	return func(c *xfrConfig) { c.noKeys = true }
}

// withResolvingPipeline swaps the fixture's terminal pipeline handler for one
// that actually answers ordinary queries through the resolver, instead of
// failing the test on any reach.
//
// Every other xfrFixture test sends only transfer queries, so for them
// reaching the pipeline at all means the transfer intercept did not fire —
// the strict default stays. But Transferrer.ProbeSerial (the notify loopback
// tests) sends the primary a plain SOA question, which is not a transfer
// query and is never intercepted by dnssrv.WithTransfers; a real dnsaur
// primary answers it from its ordinary pipeline; res.Middleware() is that
// pipeline's own zone-answering half, chained ahead of the strict stub so a
// name this fixture's zone does not cover still fails loudly.
func withResolvingPipeline() xfrOption {
	return func(c *xfrConfig) { c.resolving = true }
}

func newXFRFixture(t *testing.T, z store.Zone, records []store.ZoneRecord, opts ...xfrOption) *xfrFixture {
	t.Helper()
	ctx := context.Background()

	var cfg xfrConfig
	for _, o := range opts {
		o(&cfg)
	}

	st, err := store.Open(ctx, "sqlite", t.TempDir()+"/t.db")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("store.Close: %v", err)
		}
	})
	id, err := st.Zones().AddZone(ctx, z)
	if err != nil {
		t.Fatalf("AddZone: %v", err)
	}
	for _, r := range records {
		r.ZoneID = id
		if _, err := st.Zones().AddRecord(ctx, r); err != nil {
			t.Fatalf("AddRecord %s: %v", r.Name, err)
		}
	}
	res := zones.NewResolver(st.Zones())
	if err := res.Reload(ctx); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	ts := zones.NewTransferServer(res, st.Zones(), cfg.server...)

	srvOpts := []dnssrv.Option{dnssrv.WithTransfers(ts)}
	switch {
	case cfg.noKeys:
	case cfg.keys != nil:
		srvOpts = append(srvOpts, dnssrv.WithTSIGKeys(cfg.keys))
	default:
		srvOpts = append(srvOpts, dnssrv.WithTSIGKeys(st.TSIGKeys()))
	}
	// The pipeline handler answers NOTIMP and fails the test: every query
	// below is a transfer query, so reaching the pipeline at all means the
	// intercept did not fire, and NOTIMP is a rcode the gate never produces.
	var terminal dnssrv.Handler = dnssrv.HandlerFunc(
		func(_ context.Context, req *dnssrv.Request) (*dnssrv.Response, error) {
			t.Errorf("the pipeline answered a transfer query for %s: the intercept did not fire", req.QName())
			m := new(dns.Msg)
			m.SetRcode(req.Msg, dns.RcodeNotImplemented)
			return &dnssrv.Response{Msg: m}, nil
		})
	var probeQueries atomic.Int64
	if cfg.resolving {
		// Counts every query answered here rather than by a transfer
		// intercept. In the notify loopback tests nothing else reaches this
		// primary through its ordinary pipeline — AXFR is intercepted by
		// dnssrv.WithTransfers and NOTIFY never arrives here at all — so this
		// is, by construction, a count of Transferrer.ProbeSerial's SOA
		// questions: the signal a test uses to prove WithNotifyProbes is
		// actually wired rather than silently skipped (see notifyserver.go's
		// own comment on why that failure mode produces no error, only a
		// needless transfer).
		resolving := res.Middleware()
		terminal = dnssrv.Chain(terminal, func(next dnssrv.Handler) dnssrv.Handler {
			wrapped := resolving(next)
			return dnssrv.HandlerFunc(func(ctx context.Context, req *dnssrv.Request) (*dnssrv.Response, error) {
				probeQueries.Add(1)
				return wrapped.ServeDNS(ctx, req)
			})
		})
	}
	srv := dnssrv.NewServer("127.0.0.1:0", terminal, srvOpts...)
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		// dns.Server marks itself started inside ActivateAndServe, which runs
		// on its own goroutine, so a test that never sends a packet can reach
		// Shutdown first and be told the server was never started
		// (server.go:415). That race is the library's and says nothing about
		// the code under test; every other error does, so only this one
		// message is tolerated — it is not an exported sentinel to match on.
		if err := srv.Shutdown(shutCtx); err != nil && err.Error() != "dns: server not started" {
			t.Errorf("Shutdown: %v", err)
		}
	})
	return &xfrFixture{st: st, res: res, addr: srv.Addr(), zoneID: id, probeQueries: &probeQueries}
}

// setACL writes allow_transfer onto a zone the fixture did not create — the
// RFC 6303 built-ins arrive with the store — and reloads the snapshot.
func (f *xfrFixture) setACL(t *testing.T, zoneName, acl string) {
	t.Helper()
	ctx := context.Background()
	all, err := f.st.Zones().Zones(ctx)
	if err != nil {
		t.Fatalf("Zones: %v", err)
	}
	for _, z := range all {
		if z.Name != zoneName {
			continue
		}
		z.AllowTransfer = acl
		if err := f.st.Zones().UpdateZone(ctx, z); err != nil {
			t.Fatalf("UpdateZone: %v", err)
		}
		if err := f.res.Reload(ctx); err != nil {
			t.Fatalf("Reload: %v", err)
		}
		return
	}
	t.Fatalf("no zone named %q in the store", zoneName)
}

// addZone puts a second zone in the fixture's store and reloads the
// snapshot, for the tests whose subject is one transfer's effect on another
// zone's — newXFRFixture builds exactly one, which is all every other test
// here needs.
func (f *xfrFixture) addZone(t *testing.T, z store.Zone, records []store.ZoneRecord) {
	t.Helper()
	ctx := context.Background()
	id, err := f.st.Zones().AddZone(ctx, z)
	if err != nil {
		t.Fatalf("AddZone %s: %v", z.Name, err)
	}
	for _, r := range records {
		r.ZoneID = id
		if _, err := f.st.Zones().AddRecord(ctx, r); err != nil {
			t.Fatalf("AddRecord %s: %v", r.Name, err)
		}
	}
	if err := f.res.Reload(ctx); err != nil {
		t.Fatalf("Reload: %v", err)
	}
}

// axfr asks for a zone over TCP and returns every RR received. A refusal is
// one message with an rcode and no records, and dns.Transfer.In reports that
// as an error, so both are returned and the caller asserts on whichever it
// expects.
//
// keyName, when non-empty, is the key the request is signed with and the
// reply stream is verified against.
func (f *xfrFixture) axfr(t *testing.T, qname, keyName, secret string) ([]dns.RR, error) {
	t.Helper()
	m := new(dns.Msg)
	m.SetAxfr(dns.Fqdn(qname))
	tr := new(dns.Transfer)
	if keyName != "" {
		m.SetTsig(dns.CanonicalName(keyName), dns.HmacSHA256, xfrFudge, time.Now().Unix())
		tr.TsigSecret = map[string]string{dns.CanonicalName(keyName): secret}
	}
	ch, err := tr.In(m, f.addr)
	if err != nil {
		return nil, err
	}
	var out []dns.RR
	for env := range ch {
		if env.Error != nil {
			return out, env.Error
		}
		out = append(out, env.RR...)
	}
	return out, nil
}

// axfrEventually retries axfr until it succeeds or 2 seconds pass. It exists
// for the concurrency-cap slot: a client sees a transfer as complete the
// instant it finishes reading the wire, which races the server's own release
// of that transfer's slot (transferserver.go, serve, "released = true") —
// the two are on different goroutines connected by a TCP stream, and nothing
// about the client finishing its read implies the server has returned from
// serve yet. A caller that wants to observe the slot as freed, rather than
// merely infer it from timing, retries through the race instead of asserting
// on a single attempt.
func (f *xfrFixture) axfrEventually(t *testing.T, qname string) []dns.RR {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		rrs, err := f.axfr(t, qname, "", "")
		if err == nil {
			return rrs
		}
		if time.Now().After(deadline) {
			t.Fatalf("axfr never succeeded within 2s: %v", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// axfrMessages sends q over TCP and returns every message of the response
// stream, together with the wire length each arrived in.
//
// dns.Transfer.In hands back records and drops the message boundaries, and
// three of the rules below are about the boundaries themselves: the SOA at
// the two ends and nowhere between, the OPT on the first message only, and an
// envelope that fits in a DNS message.
//
// The stream ends where RFC 5936 §2.2 says it does: at the message that
// "MUST conclude with the same SOA resource record". Deliberately not at the
// second SOA wherever it falls, which is the simpler rule and the one
// dns.Transfer uses — it would stop this reader early on exactly the stream
// the mid-stream-SOA assertion exists to catch, leaving that assertion unable
// to fire. Stopping on a message that *ends* with one still terminates on
// every well-formed stream, and lets a malformed one be read whole and named.
//
// A server that never ends a message with an SOA is caught by the connection
// deadline dial sets, not by looping forever.
func (f *xfrFixture) axfrMessages(t *testing.T, q *dns.Msg) (msgs []*dns.Msg, sizes []int) {
	t.Helper()
	wire, err := q.Pack()
	if err != nil {
		t.Fatalf("Pack: %v", err)
	}
	c := f.dial(t)
	c.send(wire)
	for {
		m, n := c.recv()
		if m.Rcode != dns.RcodeSuccess {
			t.Fatalf("message %d came back %s, want a served transfer", len(msgs)+1, dns.RcodeToString[m.Rcode])
		}
		msgs, sizes = append(msgs, m), append(sizes, n)
		if last := m.Answer; len(last) > 0 && last[len(last)-1].Header().Rrtype == dns.TypeSOA {
			return msgs, sizes
		}
	}
}

// exchangeUDP asks one question over UDP, the transport RFC 5936 §4.2 leaves
// undefined for AXFR and RFC 1995 §2 answers with a single SOA for IXFR.
func (f *xfrFixture) exchangeUDP(t *testing.T, qname string, qtype uint16) *dns.Msg {
	t.Helper()
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(qname), qtype)
	c := &dns.Client{Net: "udp"}
	reply, _, err := c.Exchange(m, f.addr)
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	return reply
}

// exchange asks one question over plain TCP and returns the reply, which is
// the whole of a refusal. dns.Transfer.In collapses every refusal into one
// error string, and these tests assert on the rcode itself — REFUSED and
// NOTAUTH mean different things to the operator reading the peer's log.
func (f *xfrFixture) exchange(t *testing.T, qname string, qtype uint16) *dns.Msg {
	t.Helper()
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(qname), qtype)
	c := &dns.Client{Net: "tcp"}
	reply, _, err := c.Exchange(m, f.addr)
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	return reply
}

// exchangeSigned signs an AXFR for qname under (keyName, alg, secret) at
// timeSigned and returns the reply exactly as it arrived.
func (f *xfrFixture) exchangeSigned(t *testing.T, qname, keyName, alg, secret string, timeSigned int64) *dns.Msg {
	t.Helper()
	reply, _ := f.exchangeSignedRaw(t, qname, keyName, alg, secret, timeSigned)
	return reply
}

// exchangeSignedRaw is exchangeSigned plus the MAC of the request it answers,
// which RFC 8945 §5.4.2 folds into a reply's own MAC and which a caller
// checking that signature therefore needs.
func (f *xfrFixture) exchangeSignedRaw(t *testing.T, qname, keyName, alg, secret string, timeSigned int64) (reply *dns.Msg, requestMAC string) {
	t.Helper()
	m := new(dns.Msg)
	m.SetAxfr(dns.Fqdn(qname))
	m.SetTsig(dns.CanonicalName(keyName), alg, xfrFudge, timeSigned)
	sent, requestMAC, err := dns.TsigGenerate(m, secret, "", false)
	if err != nil {
		t.Fatalf("TsigGenerate: %v", err)
	}
	return f.exchangeWire(t, sent), requestMAC
}

// exchangeWire writes one already-packed message over TCP and unpacks what
// comes back, deliberately without verifying the reply's own TSIG: RFC 8945
// has the server answer a signature it could not verify with an *unsigned*
// TSIG error record, and a verifying client would reject precisely the
// replies these tests exist to read.
func (f *xfrFixture) exchangeWire(t *testing.T, wire []byte) *dns.Msg {
	t.Helper()
	c := f.dial(t)
	c.send(wire)
	reply, _ := c.recv()
	return reply
}

// xfrConn is one TCP connection to the fixture, framed the way DNS over TCP
// is: a two-byte length in front of every message. A transfer is a sequence
// of messages on one connection, so reading one needs the connection itself
// rather than a single exchange.
type xfrConn struct {
	t    *testing.T
	conn net.Conn
}

// dial opens that connection, with a deadline so a stream that never ends
// fails the test rather than hanging it.
func (f *xfrFixture) dial(t *testing.T) *xfrConn {
	t.Helper()
	conn, err := net.DialTimeout("tcp", f.addr, 3*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() {
		if err := conn.Close(); err != nil {
			t.Errorf("conn.Close: %v", err)
		}
	})
	if err := conn.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
		t.Fatalf("SetDeadline: %v", err)
	}
	return &xfrConn{t: t, conn: conn}
}

func (c *xfrConn) send(wire []byte) {
	c.t.Helper()
	framed := make([]byte, 2+len(wire))
	binary.BigEndian.PutUint16(framed, uint16(len(wire)))
	copy(framed[2:], wire)
	if _, err := c.conn.Write(framed); err != nil {
		c.t.Fatalf("write: %v", err)
	}
}

// recv reads one message and reports the wire length it arrived in, which is
// the number RFC 5936's 65535 ceiling is about.
func (c *xfrConn) recv() (*dns.Msg, int) {
	c.t.Helper()
	var n uint16
	if err := binary.Read(c.conn, binary.BigEndian, &n); err != nil {
		c.t.Fatalf("read length: %v", err)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(c.conn, buf); err != nil {
		c.t.Fatalf("read message: %v", err)
	}
	m := new(dns.Msg)
	if err := m.Unpack(buf); err != nil {
		c.t.Fatalf("unpack: %v", err)
	}
	return m, int(n)
}

// xfrKey writes a TSIG key and returns the base64 secret a client signs with.
// storeTSIGKey (transfer_test.go) writes one too, but keeps its secret to
// itself: every test here is the far end of the same key and has to sign.
func xfrKey(t *testing.T, st store.Store, name, alg string) string {
	t.Helper()
	secret := base64.StdEncoding.EncodeToString([]byte("an-outbound-transfer-secret-of-length"))
	if _, err := st.TSIGKeys().Create(context.Background(), store.TSIGKey{
		Name: dns.CanonicalName(name), Algorithm: alg, Secret: secret,
		CreatedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("TSIGKeys().Create: %v", err)
	}
	return secret
}

// assertRcode fails unless m carries want, naming both in the DNS spelling an
// operator would see in a peer's log.
func assertRcode(t *testing.T, m *dns.Msg, want int) {
	t.Helper()
	if m.Rcode != want {
		t.Fatalf("rcode = %s, want %s", dns.RcodeToString[m.Rcode], dns.RcodeToString[want])
	}
}

// assertTSIGRecord fails unless m carries a TSIG record reporting want, and
// returns it. Whether that record is signed is the caller's to assert: RFC
// 8945 requires the opposite answers for BADKEY/BADSIG (§5.2.1, §5.2.2) and
// for BADTIME (§5.2.3), so one helper cannot check both.
func assertTSIGRecord(t *testing.T, m *dns.Msg, want uint16) *dns.TSIG {
	t.Helper()
	rr := m.IsTsig()
	if rr == nil {
		t.Fatalf("reply carries no TSIG record, want one reporting %d", want)
	}
	if rr.Error != want {
		t.Fatalf("TSIG error = %d, want %d", rr.Error, want)
	}
	return rr
}

// assertUnsignedTSIGError fails unless m carries a TSIG record reporting want
// with no MAC at all. The MAC half is the point: for a key it does not hold
// or a MAC that did not verify, the server has nothing to sign with, and RFC
// 8945 says of both "This response MUST be unsigned" — §5.2.1 for BADKEY and
// §5.2.2 for BADSIG, each pointing at §5.3.2 for the shape. A signed one
// would assert an authenticity the server never established, which the rcode
// alone would not catch.
func assertUnsignedTSIGError(t *testing.T, m *dns.Msg, want uint16) *dns.TSIG {
	t.Helper()
	rr := assertTSIGRecord(t, m, want)
	if rr.MAC != "" || rr.MACSize != 0 {
		t.Fatalf("TSIG error reply is signed (MAC %q, size %d); RFC 8945 §5.2.1/§5.2.2 have this one unsigned", rr.MAC, rr.MACSize)
	}
	return rr
}

// rrNames is every RR's owner name and type, for asserting what a transfer
// contained without depending on the order records come back in.
func rrNames(rrs []dns.RR) []string {
	out := make([]string, 0, len(rrs))
	for _, rr := range rrs {
		out = append(out, rr.Header().Name+" "+dns.TypeToString[rr.Header().Rrtype])
	}
	return out
}

func TestAXFRServesAPrimaryZoneToAnAllowedPeer(t *testing.T) {
	f := newXFRFixture(t, xfrZone("127.0.0.0/8"), xfrRecords())

	rrs, err := f.axfr(t, xfrApex, "", "")
	if err != nil {
		t.Fatalf("axfr: %v", err)
	}
	for _, rr := range rrs {
		if strings.HasPrefix(rr.Header().Name, "disabled.") {
			t.Fatal("a disabled record was transferred; the snapshot drops it, and a querier never sees it either")
		}
	}
	// SOA, apex NS, bifrost A, SOA. The disabled row is not in the snapshot
	// at all, so this is exactly what a querier is answered from.
	if len(rrs) != 4 {
		t.Fatalf("got %d RRs (%v), want the SOA, both enabled records and the closing SOA", len(rrs), rrNames(rrs))
	}
	if _, ok := rrs[0].(*dns.SOA); !ok {
		t.Fatalf("first RR is %T, want SOA (RFC 5936 §2.2)", rrs[0])
	}
	if _, ok := rrs[len(rrs)-1].(*dns.SOA); !ok {
		t.Fatalf("last RR is %T, want SOA (RFC 5936 §2.2)", rrs[len(rrs)-1])
	}
	if soa := rrs[0].(*dns.SOA); soa.Serial != 3 {
		t.Errorf("SOA serial = %d, want the zone's 3", soa.Serial)
	}
	want := map[string]bool{xfrApex + ". NS": false, "bifrost." + xfrApex + ". A": false}
	for _, got := range rrNames(rrs[1 : len(rrs)-1]) {
		if _, ok := want[got]; !ok {
			t.Errorf("unexpected record in the transfer: %s", got)
			continue
		}
		want[got] = true
	}
	for name, seen := range want {
		if !seen {
			t.Errorf("%s was not transferred", name)
		}
	}
}

func TestAXFRRefusesAPeerOutsideTheACL(t *testing.T) {
	// The client is 127.0.0.1 and the ACL names somewhere else entirely.
	f := newXFRFixture(t, xfrZone("10.0.0.0/24"), xfrRecords())

	m := f.exchange(t, xfrApex, dns.TypeAXFR)
	// REFUSED, not NOTAUTH: this server does hold the zone, and says so by
	// declining rather than by disclaiming it.
	assertRcode(t, m, dns.RcodeRefused)
	if len(m.Answer) != 0 {
		t.Fatalf("a refusal carried %d records: %v", len(m.Answer), rrNames(m.Answer))
	}
}

func TestAXFRRefusesWhenAllowTransferIsEmpty(t *testing.T) {
	// Default deny: a zone is created with no ACL, and that means nobody.
	f := newXFRFixture(t, xfrZone(""), xfrRecords())

	m := f.exchange(t, xfrApex, dns.TypeAXFR)
	assertRcode(t, m, dns.RcodeRefused)
	if len(m.Answer) != 0 {
		t.Fatalf("a refusal carried %d records: %v", len(m.Answer), rrNames(m.Answer))
	}
}

func TestAXFRRefusesWhenAllowTransferDoesNotParse(t *testing.T) {
	// A hand-edited database: the API validates on write, so this value can
	// only arrive around it. It fails closed — the whole ACL is deny, rather
	// than the entries before the bad one being honoured.
	f := newXFRFixture(t, xfrZone("127.0.0.0/8, not-an-address"), xfrRecords())

	assertRcode(t, f.exchange(t, xfrApex, dns.TypeAXFR), dns.RcodeRefused)
}

func TestAXFRIsNotAuthForAnApexWeDoNotHold(t *testing.T) {
	// RFC 5936 §2.2.1: "If a server is not authoritative for the queried
	// zone, the server SHOULD set the value to NotAuth(9)."
	f := newXFRFixture(t, xfrZone("127.0.0.0/8"), xfrRecords())

	assertRcode(t, f.exchange(t, "somewhere.else", dns.TypeAXFR), dns.RcodeNotAuth)
}

func TestAXFRIsNotAuthForASubdomainOfAZoneWeHold(t *testing.T) {
	// This is why Index.Apex exists rather than Index.Find: Find walks
	// suffixes and would answer this with the parent zone — a zone the peer
	// did not ask for and may not be allowed.
	f := newXFRFixture(t, xfrZone("127.0.0.0/8"), xfrRecords())

	m := f.exchange(t, "sub."+xfrApex, dns.TypeAXFR)
	assertRcode(t, m, dns.RcodeNotAuth)
	if len(m.Answer) != 0 {
		t.Fatalf("the parent zone was transferred for a subdomain query: %v", rrNames(m.Answer))
	}
}

func TestAXFRIsNotAuthForADisabledZone(t *testing.T) {
	// A disabled zone is not served to queries either — Index.Find skips it.
	z := xfrZone("127.0.0.0/8")
	z.Enabled = false
	f := newXFRFixture(t, z, xfrRecords())

	assertRcode(t, f.exchange(t, xfrApex, dns.TypeAXFR), dns.RcodeNotAuth)
}

func TestAXFRIsNotAuthForAnInternalZone(t *testing.T) {
	// One of the RFC 6303 built-ins, with an ACL set so the refusal is about
	// the zone type rather than the ACL: those zones are empty by
	// construction and there is nothing in them to hand over.
	f := newXFRFixture(t, xfrZone("127.0.0.0/8"), xfrRecords())
	f.setACL(t, "localhost", "127.0.0.0/8")

	assertRcode(t, f.exchange(t, "localhost", dns.TypeAXFR), dns.RcodeNotAuth)
}

// secondaryZone is a secondary with an ACL that admits the test client, whose
// transfer state the caller sets.
func secondaryZone(refreshedAt, expiresAt int64) store.Zone {
	z := xfrZone("127.0.0.0/8")
	z.Type = "secondary"
	z.Primaries = "127.0.0.1:5300"
	z.RefreshedAt, z.ExpiresAt = refreshedAt, expiresAt
	return z
}

func TestAXFROfASecondaryBeforeItsFirstTransferIsServfail(t *testing.T) {
	// refreshed_at = 0: it holds nothing it can vouch for, so it hands
	// nothing on. SERVFAIL, not REFUSED — the peer should ask again later,
	// and this is not a policy decision about the peer.
	f := newXFRFixture(t, secondaryZone(0, 0), xfrRecords())

	assertRcode(t, f.exchange(t, xfrApex, dns.TypeAXFR), dns.RcodeServerFailure)
}

func TestAXFROfAnExpiredSecondaryIsServfail(t *testing.T) {
	// RFC 1034 §4.3.5: past expires_at a secondary can no longer confirm
	// what it holds is current. The deadline is crossed with the clock
	// rather than by waiting.
	base := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)
	f := newXFRFixture(t,
		secondaryZone(base.UnixMilli(), base.Add(time.Hour).UnixMilli()), xfrRecords(),
		withXFRNow(func() time.Time { return base.Add(2 * time.Hour) }))

	assertRcode(t, f.exchange(t, xfrApex, dns.TypeAXFR), dns.RcodeServerFailure)
}

func TestAXFROfALiveSecondaryIsServed(t *testing.T) {
	// The same zone one hour earlier: a secondary re-serves what it pulled,
	// which is the whole of §9.5.3's rule for the type.
	base := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)
	f := newXFRFixture(t,
		secondaryZone(base.UnixMilli(), base.Add(time.Hour).UnixMilli()), xfrRecords(),
		withXFRNow(func() time.Time { return base.Add(30 * time.Minute) }))

	rrs, err := f.axfr(t, xfrApex, "", "")
	if err != nil {
		t.Fatalf("axfr: %v", err)
	}
	if len(rrs) != 4 {
		t.Fatalf("got %d RRs (%v), want the SOA, both enabled records and the closing SOA", len(rrs), rrNames(rrs))
	}
}

func TestAXFRWithAKeyEntryRequiresASignature(t *testing.T) {
	// An unsigned request is not a TSIG failure: it is an ACL outcome. With
	// only a key: entry there is nothing for an unsigned request to match,
	// so it is REFUSED — never NOTAUTH, which would claim the peer's key was
	// wrong when the peer presented none.
	f := newXFRFixture(t, xfrZone("key:ns2."+xfrApex+"."), xfrRecords())
	xfrKey(t, f.st, "ns2."+xfrApex, dns.HmacSHA256)

	m := f.exchange(t, xfrApex, dns.TypeAXFR)
	assertRcode(t, m, dns.RcodeRefused)
	if rr := m.IsTsig(); rr != nil {
		t.Fatalf("an unsigned request was answered with a TSIG record reporting %d", rr.Error)
	}
}

func TestAXFRSignedUnderTheNamedKeyIsServed(t *testing.T) {
	// The same zone, signed correctly. dns.Transfer verifies the reply
	// stream, so a transfer that arrives at all arrives verified.
	f := newXFRFixture(t, xfrZone("key:ns2."+xfrApex+"."), xfrRecords())
	secret := xfrKey(t, f.st, "ns2."+xfrApex, dns.HmacSHA256)

	rrs, err := f.axfr(t, xfrApex, "ns2."+xfrApex, secret)
	if err != nil {
		t.Fatalf("axfr: %v", err)
	}
	if len(rrs) != 4 {
		t.Fatalf("got %d RRs (%v), want the SOA, both enabled records and the closing SOA", len(rrs), rrNames(rrs))
	}
}

func TestAXFRRefusalToAVerifiedPeerIsSigned(t *testing.T) {
	// The key verified; it is the address the ACL does not admit. RFC 8945
	// §5.3 signs the answer to a signed request, and a refusal is still an
	// answer: an unsigned one is something an off-path attacker could have
	// forged, so a peer that required authentication would be right to ignore
	// it — and would then never learn why its transfers stopped.
	f := newXFRFixture(t, xfrZone("10.0.0.0/24"), xfrRecords())
	name := dns.CanonicalName("ns2." + xfrApex)
	secret := xfrKey(t, f.st, name, dns.HmacSHA256)

	m := new(dns.Msg)
	m.SetAxfr(dns.Fqdn(xfrApex))
	m.SetTsig(name, dns.HmacSHA256, xfrFudge, time.Now().Unix())
	// dns.Client verifies the reply against the same secret, so a signature
	// this client cannot check is an error here rather than a passing test.
	c := &dns.Client{Net: "tcp", TsigSecret: map[string]string{name: secret}}
	reply, _, err := c.Exchange(m, f.addr)
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	assertRcode(t, reply, dns.RcodeRefused)
	rr := reply.IsTsig()
	if rr == nil {
		t.Fatal("the refusal carried no TSIG record: it went out unsigned")
	}
	if rr.MAC == "" {
		t.Fatal("the refusal's TSIG carries no MAC: it went out unsigned")
	}
}

func TestAXFRWithAnUnknownKeyIsNotAuthBadKey(t *testing.T) {
	// Signed under a key this server does not hold (dns.ErrSecret).
	f := newXFRFixture(t, xfrZone("127.0.0.0/8"), xfrRecords())
	secret := base64.StdEncoding.EncodeToString([]byte("a-secret-nobody-here-has-heard-of"))

	m := f.exchangeSigned(t, xfrApex, "nosuch."+xfrApex, dns.HmacSHA256, secret, time.Now().Unix())
	assertRcode(t, m, dns.RcodeNotAuth)
	assertUnsignedTSIGError(t, m, dns.RcodeBadKey)
	if len(m.Answer) != 0 {
		t.Fatalf("a BADKEY refusal carried %d records: %v", len(m.Answer), rrNames(m.Answer))
	}
}

func TestAXFRUnderTheWrongAlgorithmIsNotAuthBadKey(t *testing.T) {
	// The other half of the same row: a key that exists, named with an
	// algorithm it was not created with (dns.ErrKeyAlg). A key is the triple
	// (name, algorithm, secret), so this is as unknown as an unknown name.
	f := newXFRFixture(t, xfrZone("127.0.0.0/8"), xfrRecords())
	secret := xfrKey(t, f.st, "ns2."+xfrApex, dns.HmacSHA512)

	m := f.exchangeSigned(t, xfrApex, "ns2."+xfrApex, dns.HmacSHA256, secret, time.Now().Unix())
	assertRcode(t, m, dns.RcodeNotAuth)
	assertUnsignedTSIGError(t, m, dns.RcodeBadKey)
}

func TestAXFRWithABadMACIsNotAuthBadSig(t *testing.T) {
	// The right key name, the wrong secret (dns.ErrSig).
	f := newXFRFixture(t, xfrZone("127.0.0.0/8"), xfrRecords())
	xfrKey(t, f.st, "ns2."+xfrApex, dns.HmacSHA256)
	wrong := base64.StdEncoding.EncodeToString([]byte("not-the-secret-the-server-holds!!"))

	m := f.exchangeSigned(t, xfrApex, "ns2."+xfrApex, dns.HmacSHA256, wrong, time.Now().Unix())
	assertRcode(t, m, dns.RcodeNotAuth)
	assertUnsignedTSIGError(t, m, dns.RcodeBadSig)
}

func TestAXFROutsideTheFudgeWindowIsNotAuthBadTime(t *testing.T) {
	// A valid MAC over a TimeSigned an hour in the past (dns.ErrTime). Unlike
	// BADKEY and BADSIG, this reply is signed: the key and the MAC did
	// verify, only the clocks disagree, and RFC 8945 §5.2.3 is explicit —
	// "A response indicating a BADTIME error MUST be signed by the same key
	// as the request. It MUST include the client's current time in the Time
	// Signed field, the server's current time (an unsigned 48-bit integer) in
	// the Other Data field, and 6 in the Other Len field."
	f := newXFRFixture(t, xfrZone("127.0.0.0/8"), xfrRecords())
	secret := xfrKey(t, f.st, "ns2."+xfrApex, dns.HmacSHA256)

	signed := time.Now().Add(-time.Hour).Unix()
	m, requestMAC := f.exchangeSignedRaw(t, xfrApex, "ns2."+xfrApex, dns.HmacSHA256, secret, signed)
	assertRcode(t, m, dns.RcodeNotAuth)
	rr := assertTSIGRecord(t, m, dns.RcodeBadTime)

	if rr.MAC == "" || rr.MACSize == 0 {
		t.Fatal("the BADTIME reply is unsigned; a peer cannot trust a clock report it cannot verify")
	}
	// The client's own time comes back, which is the one clock a skewed peer
	// can check a reply against.
	if rr.TimeSigned != uint64(signed) {
		t.Errorf("Time Signed = %d, want the client's own %d", rr.TimeSigned, signed)
	}
	if rr.OtherLen != 6 || len(rr.OtherData) != 12 {
		t.Fatalf("Other Data is %q (len field %d), want the 48-bit server time", rr.OtherData, rr.OtherLen)
	}
	raw, err := hex.DecodeString(rr.OtherData)
	if err != nil {
		t.Fatalf("Other Data %q is not hex: %v", rr.OtherData, err)
	}
	var served uint64
	for _, b := range raw {
		served = served<<8 | uint64(b)
	}
	if skew := time.Since(time.Unix(int64(served), 0)); skew < -time.Minute || skew > time.Minute {
		t.Fatalf("Other Data says %s, which is %s from now: it must carry the server's own time",
			time.Unix(int64(served), 0), skew)
	}

	// And the signature is one that validates under the key — a MAC that is
	// merely present proves nothing.
	//
	// Not through dns.TsigVerify, and the reason is a library finding worth
	// keeping: stripTsig rejects any message whose rcode is NOTAUTH before it
	// looks at the signature at all (tsig.go:341-343, the only place in the
	// library that returns ErrAuth), and every TSIG error reply is NOTAUTH by
	// definition. So miekg can sign a BADTIME reply and cannot verify one.
	// This asserts the same property from the other side: re-derive the MAC
	// over the reply as it arrived, under the same secret and the same
	// request MAC, and require the one on the wire to equal it. TsigGenerate
	// strips the TSIG off the message it is given, so it gets a copy.
	_, want, err := dns.TsigGenerate(m.Copy(), secret, requestMAC, false)
	if err != nil {
		t.Fatalf("re-deriving the reply MAC: %v", err)
	}
	if rr.MAC != want {
		t.Fatalf("reply MAC %q does not validate under the key; want %q", rr.MAC, want)
	}
}

// errKeyStoreDown is what a store that has stopped answering returns.
var errKeyStoreDown = errors.New("tsig key store is down")

// brokenKeys is a key store that fails every lookup, which is how a signed
// request reaches the gate with an error that is nobody's fault but ours.
type brokenKeys struct{}

func (brokenKeys) ByName(context.Context, string) (store.TSIGKey, bool, error) {
	return store.TSIGKey{}, false, errKeyStoreDown
}

func TestAXFRWhenTheKeyStoreFailsIsServfailAndNeverATSIGError(t *testing.T) {
	// A store that broke is not the peer's fault. Telling a correctly
	// configured peer its key is bad, because our database was briefly
	// unavailable, sends the operator to the wrong end of the system.
	f := newXFRFixture(t, xfrZone("127.0.0.0/8"), xfrRecords(), withKeyStore(brokenKeys{}))
	secret := base64.StdEncoding.EncodeToString([]byte("a-secret-the-store-cannot-look-up"))

	m := f.exchangeSigned(t, xfrApex, "ns2."+xfrApex, dns.HmacSHA256, secret, time.Now().Unix())
	assertRcode(t, m, dns.RcodeServerFailure)
	if rr := m.IsTsig(); rr != nil {
		t.Fatalf("a store failure was reported as TSIG error %d", rr.Error)
	}
}

func TestAXFRWithNothingToVerifyItIsServfail(t *testing.T) {
	// No key store at all, so nothing verified this message either way
	// (dnssrv.ErrTSIGUnavailable). "No one checked" must never be served as
	// though it were "checked and fine", and it is not the peer's fault
	// either — SERVFAIL, carrying no TSIG error.
	f := newXFRFixture(t, xfrZone("127.0.0.0/8"), xfrRecords(), withoutKeyStore())

	m := f.exchange(t, xfrApex, dns.TypeAXFR)
	assertRcode(t, m, dns.RcodeServerFailure)
	if rr := m.IsTsig(); rr != nil {
		t.Fatalf("a missing provider was reported as TSIG error %d", rr.Error)
	}
}

func TestAXFRInAClassOtherThanINIsFormerr(t *testing.T) {
	f := newXFRFixture(t, xfrZone("127.0.0.0/8"), xfrRecords())

	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(xfrApex), dns.TypeAXFR)
	m.Question[0].Qclass = dns.ClassCHAOS
	c := &dns.Client{Net: "tcp"}
	reply, _, err := c.Exchange(m, f.addr)
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	assertRcode(t, reply, dns.RcodeFormatError)
}

func TestIXFRIsAnsweredWithTheWholeZone(t *testing.T) {
	// RFC 1995 §2: "the server may choose to transfer the entire zone just
	// as in a normal full zone transfer", which is what dnsaur does until
	// there is a journal to compute a delta from.
	f := newXFRFixture(t, xfrZone("127.0.0.0/8"), xfrRecords())

	m := f.exchange(t, xfrApex, dns.TypeIXFR)
	assertRcode(t, m, dns.RcodeSuccess)
	if len(m.Answer) != 4 {
		t.Fatalf("got %d RRs (%v), want the whole zone", len(m.Answer), rrNames(m.Answer))
	}
}

// ixfr asks for an incremental transfer over TCP, naming the serial the client
// already holds in the authority section the way RFC 1995 §3 specifies. The
// reply is read as one message: a client that is current gets exactly one, and
// a client that is behind gets a stream whose first message is what the
// assertions here are about.
func (f *xfrFixture) ixfr(t *testing.T, qname string, serial uint32) *dns.Msg {
	t.Helper()
	m := new(dns.Msg)
	m.SetIxfr(dns.Fqdn(qname), serial, dns.Fqdn("ns1."+qname), dns.Fqdn("hostmaster."+qname))
	c := &dns.Client{Net: "tcp"}
	reply, _, err := c.Exchange(m, f.addr)
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	return reply
}

// RFC 1995 §2: "If an IXFR query with the same or newer version number than
// that of the server is received, it is replied to with a single SOA record of
// the server's current version." §4's permission to answer IXFR with a whole
// AXFR — which is what this server does, having no journal — is about a client
// that is genuinely behind; it is not licence to hand the entire zone to a
// secondary that already has it, on its own refresh timer, forever.
func TestIXFRFromACurrentClientIsAnsweredWithOneSOA(t *testing.T) {
	f := newXFRFixture(t, xfrZone("127.0.0.0/8"), xfrRecords())

	// xfrZone's serial is 3: the same, and one ahead — the client's clock
	// having run away is still not a reason to send it the zone.
	for _, serial := range []uint32{3, 4} {
		m := f.ixfr(t, xfrApex, serial)
		assertRcode(t, m, dns.RcodeSuccess)
		if len(m.Answer) != 1 {
			t.Fatalf("client serial %d: got %d RRs (%v), want one SOA",
				serial, len(m.Answer), rrNames(m.Answer))
		}
		soa, ok := m.Answer[0].(*dns.SOA)
		if !ok {
			t.Fatalf("client serial %d: answer is %T, want an SOA", serial, m.Answer[0])
		}
		if soa.Serial != 3 {
			t.Errorf("client serial %d: answered serial %d, want this server's 3", serial, soa.Serial)
		}
	}
}

// The other half: a client that really is behind still gets the zone, so the
// check above cannot be satisfied by never transferring anything.
func TestIXFRFromABehindClientStillGetsTheZone(t *testing.T) {
	f := newXFRFixture(t, xfrZone("127.0.0.0/8"), xfrRecords())

	m := f.ixfr(t, xfrApex, 2)
	assertRcode(t, m, dns.RcodeSuccess)
	if len(m.Answer) != 4 {
		t.Fatalf("got %d RRs (%v), want the whole zone", len(m.Answer), rrNames(m.Answer))
	}
}

// captureWriter is a dns.ResponseWriter that keeps what was written, for the
// tests that call ServeTransfer directly because what they assert on is not
// reachable through the fixture: a context shorter than dnssrv's own
// (TransferTimeout is not an option on this server), and a store that counts
// or stalls its writes (newXFRFixture builds the TransferServer around its
// own store).
type captureWriter struct {
	msg *dns.Msg
	// remote overrides RemoteAddr's default TCP peer, for the recording
	// tests that need a *net.UDPAddr to exercise the TCP-only gate (§9.5.8)
	// without a real listener. nil for every other test, which gets TCP.
	remote net.Addr
}

func (c *captureWriter) LocalAddr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 53}
}

func (c *captureWriter) RemoteAddr() net.Addr {
	if c.remote != nil {
		return c.remote
	}
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 40000}
}

func (c *captureWriter) WriteMsg(m *dns.Msg) error { c.msg = m; return nil }

func (c *captureWriter) Write(b []byte) (int, error) {
	m := new(dns.Msg)
	if err := m.Unpack(b); err != nil {
		return 0, err
	}
	c.msg = m
	return len(b), nil
}

func (c *captureWriter) Close() error        { return nil }
func (c *captureWriter) TsigStatus() error   { return nil }
func (c *captureWriter) TsigTimersOnly(bool) {}
func (c *captureWriter) Hijack()             {}

// blockingWriter is a dns.ResponseWriter whose WriteMsg (and Write) blocks
// until release closes, standing in for a peer that has stopped reading —
// the situation §9.5.7's cap exists for, since nothing on the serve path
// ever applies dns.Server.WriteTimeout to unblock it. When release instead
// closes partway through a multi-envelope transfer, it stands in for a peer
// that is merely slow, which is what the per-envelope context check is for.
//
// writes counts calls, so a test can assert how many envelopes were
// attempted rather than only how long the whole call took — the two are not
// the same thing once release closes mid-stream, because every call after
// release closes returns immediately. It is read only after a happens-before
// established by a channel close (see its callers), so it is a plain int
// rather than an atomic one.
type blockingWriter struct {
	release <-chan struct{}
	remote  *net.TCPAddr
	writes  int
}

func (w *blockingWriter) LocalAddr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 53}
}

func (w *blockingWriter) RemoteAddr() net.Addr { return w.remote }

func (w *blockingWriter) WriteMsg(*dns.Msg) error {
	w.writes++
	<-w.release
	return nil
}

func (w *blockingWriter) Write(b []byte) (int, error) {
	w.writes++
	<-w.release
	return len(b), nil
}

func (w *blockingWriter) Close() error        { return nil }
func (w *blockingWriter) TsigStatus() error   { return nil }
func (w *blockingWriter) TsigTimersOnly(bool) {}
func (w *blockingWriter) Hijack()             {}

func TestTheGateAnswersFormerrWhenAQueryIsNotOneQuestion(t *testing.T) {
	// The one test in this file that does not go through the fixture, and it
	// says so rather than implying it covers the wire. §9.5.5's first row is
	// answered FORMERR to a peer, but by the library: miekg's accept function
	// rejects any header whose QDCOUNT is not 1 before dnssrv.Server.serve
	// runs, so neither the intercept nor this gate sees such a message.
	// dnssrv's TestAMalformedQuestionCountIsRejectedBeforeEitherBranch and
	// TestAHeaderOnlyQueryReachesThePipelineWithNoQuestion pin that, both
	// halves of it, on a real listener.
	//
	// What is left here is a guard on an exported method: ServeTransfer is
	// part of dnssrv.Transfers, anything may call it, and everything below
	// this check reads Question[0].
	f := newXFRFixture(t, xfrZone("127.0.0.0/8"), xfrRecords())
	ts := zones.NewTransferServer(f.res, f.st.Zones())

	two := new(dns.Msg)
	two.SetAxfr(dns.Fqdn(xfrApex))
	two.Question = append(two.Question, dns.Question{
		Name: dns.Fqdn("second." + xfrApex), Qtype: dns.TypeAXFR, Qclass: dns.ClassINET,
	})
	none := new(dns.Msg)
	none.Id = dns.Id()

	for _, tc := range []struct {
		name string
		q    *dns.Msg
	}{
		{"two questions", two},
		{"no question at all", none},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := new(captureWriter)
			ts.ServeTransfer(context.Background(), w, tc.q, "", dnssrv.ErrTSIGUnsigned)
			if w.msg == nil {
				t.Fatal("nothing was written; a refusal is still a reply")
			}
			assertRcode(t, w.msg, dns.RcodeFormatError)
		})
	}
}

// The envelope stream, and the two UDP rules. RFC 5936 §2.2 makes an AXFR
// response a sequence of messages rather than one, and everything below is a
// property of that sequence — how many there are, what is at its two ends,
// what only its first message carries, and that its running signature holds
// across all of it.

// bigZoneRecordCount is how many A records the multi-envelope tests transfer.
const bigZoneRecordCount = 2000

// answersOf is every answer record across a whole stream, in arrival order.
func answersOf(msgs []*dns.Msg) []dns.RR {
	var out []dns.RR
	for _, m := range msgs {
		out = append(out, m.Answer...)
	}
	return out
}

func TestALargeZoneArrivesInSeveralEnvelopes(t *testing.T) {
	// The zone is past 65535 bytes, so before batching this transfer failed
	// inside the writer and the peer received nothing at all — the failure
	// mode a single-message transfer has for every zone worth transferring.
	f := newXFRFixture(t, xfrZone("127.0.0.0/8"), xfrBigRecords(bigZoneRecordCount))

	q := new(dns.Msg)
	q.SetAxfr(dns.Fqdn(xfrApex))
	msgs, sizes := f.axfrMessages(t, q)

	if len(msgs) < 2 {
		t.Fatalf("the zone arrived in %d message(s); a zone this size does not fit in one", len(msgs))
	}
	// RFC 1035 §4.2.2 caps a message on TCP at 65535 bytes, and miekg refuses
	// to write a larger one — which is what "the peer received nothing" was.
	for i, n := range sizes {
		if n > 65535 {
			t.Errorf("message %d is %d bytes, past the 65535 a DNS message can hold", i+1, n)
		}
	}

	// Every record exactly once, counted by owner name so a record duplicated
	// across an envelope boundary is caught as well as one dropped at it.
	seen := make(map[string]int, bigZoneRecordCount+1)
	for _, rr := range answersOf(msgs) {
		if rr.Header().Rrtype == dns.TypeSOA {
			continue
		}
		seen[rr.Header().Name+" "+dns.TypeToString[rr.Header().Rrtype]]++
	}
	want := map[string]int{xfrApex + ". NS": 1}
	for i := range bigZoneRecordCount {
		want[fmt.Sprintf("host%04d-of-big-zone.%s. A", i, xfrApex)] = 1
	}
	if len(seen) != len(want) {
		t.Fatalf("the stream carried %d distinct records, want %d", len(seen), len(want))
	}
	for name, n := range want {
		if seen[name] != n {
			t.Errorf("%s arrived %d times, want %d", name, seen[name], n)
		}
	}
}

func TestTheSOAAppearsOnlyFirstAndLast(t *testing.T) {
	// RFC 5936 §2.2: "The first message MUST begin with the SOA resource
	// record of the zone, and the last message MUST conclude with the same
	// SOA resource record. Intermediate messages MUST NOT contain the SOA
	// resource record."
	f := newXFRFixture(t, xfrZone("127.0.0.0/8"), xfrBigRecords(bigZoneRecordCount))

	q := new(dns.Msg)
	q.SetAxfr(dns.Fqdn(xfrApex))
	msgs, _ := f.axfrMessages(t, q)
	if len(msgs) < 3 {
		t.Fatalf("the zone arrived in %d message(s); there is no intermediate message to check", len(msgs))
	}

	first := msgs[0].Answer
	if len(first) == 0 || first[0].Header().Rrtype != dns.TypeSOA {
		t.Fatalf("the first message does not begin with the SOA: %v", rrNames(first))
	}
	last := msgs[len(msgs)-1].Answer
	if len(last) == 0 || last[len(last)-1].Header().Rrtype != dns.TypeSOA {
		t.Fatalf("the last message does not end with the SOA: %v", rrNames(last))
	}
	for i, m := range msgs[1 : len(msgs)-1] {
		for _, rr := range m.Answer {
			if rr.Header().Rrtype == dns.TypeSOA {
				t.Errorf("message %d carries an SOA; intermediate messages must not", i+2)
			}
		}
	}

	// And two in the whole stream, not three: the same SOA at each end.
	var soas []*dns.SOA
	for _, rr := range answersOf(msgs) {
		if soa, ok := rr.(*dns.SOA); ok {
			soas = append(soas, soa)
		}
	}
	if len(soas) != 2 {
		t.Fatalf("the stream carried %d SOA records, want exactly 2", len(soas))
	}
	if soas[0].Serial != soas[1].Serial || soas[0].Serial != 3 {
		t.Errorf("SOA serials are %d and %d, want the zone's 3 twice", soas[0].Serial, soas[1].Serial)
	}
}

func TestAStoredSOARecordIsNotSentMidStream(t *testing.T) {
	// The zone's SOA comes from the zone row, so a zone_records row typed SOA
	// is a second, contradictory one — and RFC 5936 §2.2 has no place to put
	// it: "Intermediate messages MUST NOT contain the SOA resource record."
	// Nothing writes such a row today; a hand-edited database is where it
	// comes from, and this is what happens when it does.
	recs := append(xfrRecords(), store.ZoneRecord{
		Name: "@", Type: "SOA", TTL: 900, Enabled: true,
		RData: "ns9." + xfrApex + ". hostmaster." + xfrApex + ". 99 900 300 604800 900",
	})
	f := newXFRFixture(t, xfrZone("127.0.0.0/8"), recs)

	rrs, err := f.axfr(t, xfrApex, "", "")
	if err != nil {
		t.Fatalf("axfr: %v", err)
	}
	var soas []*dns.SOA
	for _, rr := range rrs {
		if soa, ok := rr.(*dns.SOA); ok {
			soas = append(soas, soa)
		}
	}
	if len(soas) != 2 {
		t.Fatalf("the stream carried %d SOA records (%v), want exactly the two ends", len(soas), rrNames(rrs))
	}
	for i, soa := range soas {
		if soa.Serial != 3 {
			t.Errorf("SOA %d has serial %d, want the zone row's 3: the stored row must not be transferred", i+1, soa.Serial)
		}
	}
	// The rest of the zone still arrives: excluding the row is not dropping
	// the zone's own records with it.
	if len(rrs) != 4 {
		t.Fatalf("got %d RRs (%v), want the SOA, both enabled records and the closing SOA", len(rrs), rrNames(rrs))
	}
}

func TestOPTIsEchoedOnTheFirstMessageOnly(t *testing.T) {
	// RFC 5936 §2.2.5: "If the client has supplied an EDNS OPT RR in the AXFR
	// query and if the server supports EDNS as well, it SHOULD include one
	// OPT RR in the first response message and MAY do so in subsequent
	// response messages." One on the first, none after: the SHOULD is met and
	// every later envelope keeps the eleven bytes for records.
	f := newXFRFixture(t, xfrZone("127.0.0.0/8"), xfrBigRecords(bigZoneRecordCount))

	q := new(dns.Msg)
	q.SetAxfr(dns.Fqdn(xfrApex))
	q.SetEdns0(4096, false)
	msgs, _ := f.axfrMessages(t, q)
	if len(msgs) < 2 {
		t.Fatalf("the zone arrived in %d message(s); there is no subsequent message to check", len(msgs))
	}

	if msgs[0].IsEdns0() == nil {
		t.Error("the first message carries no OPT record, though the query supplied one")
	}
	for i, m := range msgs[1:] {
		if m.IsEdns0() != nil {
			t.Errorf("message %d carries an OPT record; only the first one does", i+2)
		}
	}
}

func TestAnAXFRWithoutEDNSGetsNoOPTAtAll(t *testing.T) {
	// The other half of §2.2.5's condition: the OPT is echoed *if the client
	// supplied one*, so a query without EDNS must not be answered with one.
	f := newXFRFixture(t, xfrZone("127.0.0.0/8"), xfrRecords())

	q := new(dns.Msg)
	q.SetAxfr(dns.Fqdn(xfrApex))
	msgs, _ := f.axfrMessages(t, q)
	for i, m := range msgs {
		if m.IsEdns0() != nil {
			t.Errorf("message %d carries an OPT record, though the query carried none", i+1)
		}
	}
}

func TestASignedMultiEnvelopeStreamVerifies(t *testing.T) {
	// RFC 8945 §5.3.1 chains one running MAC across a transfer's messages,
	// and dns.Transfer checks it on every one. A stream that arrives without
	// an error is therefore the assertion — but a stream that verified and
	// arrived empty would too, so the record count is asserted as well.
	f := newXFRFixture(t, xfrZone("key:ns2."+xfrApex+"."), xfrBigRecords(bigZoneRecordCount))
	secret := xfrKey(t, f.st, "ns2."+xfrApex, dns.HmacSHA256)

	rrs, err := f.axfr(t, xfrApex, "ns2."+xfrApex, secret)
	if err != nil {
		t.Fatalf("the signed stream did not verify: %v", err)
	}
	// SOA, the apex NS, every A record, SOA.
	if want := bigZoneRecordCount + 3; len(rrs) != want {
		t.Fatalf("got %d RRs, want %d", len(rrs), want)
	}
}

func TestASignedTransferSpansMoreThanOneMessage(t *testing.T) {
	// The guard for the test above: it can only be about a *chained*
	// signature if the stream it verifies is longer than one message, and
	// nothing in dns.Transfer reports how many it read.
	f := newXFRFixture(t, xfrZone("key:ns2."+xfrApex+"."), xfrBigRecords(bigZoneRecordCount))
	name := dns.CanonicalName("ns2." + xfrApex)
	secret := xfrKey(t, f.st, "ns2."+xfrApex, dns.HmacSHA256)

	q := new(dns.Msg)
	q.SetAxfr(dns.Fqdn(xfrApex))
	q.SetTsig(name, dns.HmacSHA256, xfrFudge, time.Now().Unix())
	wire, _, err := dns.TsigGenerate(q, secret, "", false)
	if err != nil {
		t.Fatalf("TsigGenerate: %v", err)
	}
	c := f.dial(t)
	c.send(wire)

	var msgs int
	for soas := 0; soas < 2; msgs++ {
		m, _ := c.recv()
		if m.IsTsig() == nil {
			t.Fatalf("message %d is unsigned; every message of a signed transfer is signed", msgs+1)
		}
		for _, rr := range m.Answer {
			if rr.Header().Rrtype == dns.TypeSOA {
				soas++
			}
		}
	}
	if msgs < 2 {
		t.Fatalf("the signed transfer was %d message(s); the chained-MAC test needs more than one", msgs)
	}
}

func TestAXFROverUDPIsNotImplemented(t *testing.T) {
	// RFC 5936 §4.2: "AXFR sessions over UDP transport are not defined." The
	// RFC names no rcode, so NOTIMP is ours, and it is the honest one for a
	// transport this server does not implement.
	f := newXFRFixture(t, xfrZone("127.0.0.0/8"), xfrRecords())

	m := f.exchangeUDP(t, xfrApex, dns.TypeAXFR)
	assertRcode(t, m, dns.RcodeNotImplemented)
	if len(m.Answer) != 0 {
		t.Fatalf("a UDP AXFR was answered with %d records: %v", len(m.Answer), rrNames(m.Answer))
	}
}

func TestIXFROverUDPIsAnsweredWithASingleSOA(t *testing.T) {
	// RFC 1995 §2: "If the UDP reply does not fit, the query is responded to
	// with a single SOA record of the server's current version to inform the
	// client that a TCP query should be initiated." Our IXFR answer is the
	// whole zone, so it does not fit for any zone worth transferring.
	f := newXFRFixture(t, xfrZone("127.0.0.0/8"), xfrRecords())

	m := f.exchangeUDP(t, xfrApex, dns.TypeIXFR)
	assertRcode(t, m, dns.RcodeSuccess)
	if len(m.Answer) != 1 {
		t.Fatalf("got %d records (%v), want the single SOA", len(m.Answer), rrNames(m.Answer))
	}
	soa, ok := m.Answer[0].(*dns.SOA)
	if !ok {
		t.Fatalf("the answer is %T, want the zone's SOA", m.Answer[0])
	}
	// The serial is the whole point: it is what tells the peer whether a TCP
	// transfer is worth starting.
	if soa.Serial != 3 {
		t.Errorf("SOA serial = %d, want the zone's current 3", soa.Serial)
	}
	if m.Truncated {
		t.Error("the reply is truncated; a single SOA fits in a datagram and TC would send the peer to TCP for nothing")
	}
}

func TestUDPIXFRStillObeysTheACL(t *testing.T) {
	// The gate runs before the transport branch, so a peer outside the ACL
	// gets the refusal it would get over TCP rather than this server's
	// current serial.
	f := newXFRFixture(t, xfrZone("10.0.0.0/24"), xfrRecords())

	m := f.exchangeUDP(t, xfrApex, dns.TypeIXFR)
	assertRcode(t, m, dns.RcodeRefused)
	if len(m.Answer) != 0 {
		t.Fatalf("a refused UDP IXFR carried %d records: %v", len(m.Answer), rrNames(m.Answer))
	}
}

func TestUDPAXFRFromAPeerOutsideTheACLIsRefusedNotNotimp(t *testing.T) {
	// §9.5.5 spends its longest bullet on this one ordering: the AXFR-over-UDP
	// row is listed second and *checked* after every row below it, so a peer
	// that may not be told anything is not told about the transport either.
	// REFUSED, therefore, and not NOTIMP — the same answer the peer would get
	// over TCP. The IXFR half of it has a test above; this is the AXFR half,
	// which is the one the transport branch is actually about.
	f := newXFRFixture(t, xfrZone("10.0.0.0/24"), xfrRecords())

	m := f.exchangeUDP(t, xfrApex, dns.TypeAXFR)
	assertRcode(t, m, dns.RcodeRefused)
	if len(m.Answer) != 0 {
		t.Fatalf("a refused UDP AXFR carried %d records: %v", len(m.Answer), rrNames(m.Answer))
	}
}

func TestAUDPAXFROverTheCapIsStillNotimp(t *testing.T) {
	// The other constraint on where the slot is taken, and the reason it sits
	// after the transport branch rather than before it: a UDP AXFR streams
	// nothing, so it must not spend a slot, and at the cap it must still be
	// answered NOTIMP — the transport is undefined whether or not this server
	// is busy, and SERVFAIL would tell a peer to try the same impossible
	// thing again.
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	hold := func() {
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
	}
	f := newXFRFixture(t, xfrZone("127.0.0.0/8"), xfrRecords(),
		withMaxConcurrentTransfers(1), withTransferHold(hold))

	firstDone := make(chan error, 1)
	go func() {
		_, err := f.axfr(t, xfrApex, "", "")
		firstDone <- err
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("the first transfer never reached the hold hook")
	}

	// The only slot is held. A TCP AXFR here would be SERVFAIL
	// (TestTransfersBeyondTheCapAreRefused); this one is not TCP.
	m := f.exchangeUDP(t, xfrApex, dns.TypeAXFR)
	assertRcode(t, m, dns.RcodeNotImplemented)

	close(release)
	select {
	case err := <-firstDone:
		if err != nil {
			t.Fatalf("the held transfer: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the held transfer never finished after release")
	}
}

func TestUDPIXFRToAnApexWeDoNotHoldIsNotAuth(t *testing.T) {
	// The same ordering from the other side: the transport branch answers a
	// zone the gate approved, so a zone we do not hold is still NOTAUTH and
	// never a serial for something else.
	f := newXFRFixture(t, xfrZone("127.0.0.0/8"), xfrRecords())

	m := f.exchangeUDP(t, "somewhere.else", dns.TypeIXFR)
	assertRcode(t, m, dns.RcodeNotAuth)
	if len(m.Answer) != 0 {
		t.Fatalf("a NOTAUTH UDP IXFR carried %d records: %v", len(m.Answer), rrNames(m.Answer))
	}
}

func TestASignedUDPIXFRIsAnsweredSigned(t *testing.T) {
	// RFC 8945 §5.3 signs the answer to a signed request, and the single SOA
	// is an answer like any other. dns.Client verifies it against the same
	// secret, so a signature it cannot check fails the test here.
	f := newXFRFixture(t, xfrZone("key:ns2."+xfrApex+"."), xfrRecords())
	name := dns.CanonicalName("ns2." + xfrApex)
	secret := xfrKey(t, f.st, "ns2."+xfrApex, dns.HmacSHA256)

	q := new(dns.Msg)
	q.SetQuestion(dns.Fqdn(xfrApex), dns.TypeIXFR)
	q.SetTsig(name, dns.HmacSHA256, xfrFudge, time.Now().Unix())
	c := &dns.Client{Net: "udp", TsigSecret: map[string]string{name: secret}}
	reply, _, err := c.Exchange(q, f.addr)
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	assertRcode(t, reply, dns.RcodeSuccess)
	rr := reply.IsTsig()
	if rr == nil || rr.MAC == "" {
		t.Fatal("the UDP IXFR answer went out unsigned")
	}
	if len(reply.Answer) != 1 {
		t.Fatalf("got %d records (%v), want the single SOA", len(reply.Answer), rrNames(reply.Answer))
	}
}

func TestAUDPIXFRWithEDNSGetsAnOPTBack(t *testing.T) {
	// RFC 6891 §6.1.1: "If an OPT record is present in a received request,
	// compliant responders MUST include an OPT record in their respective
	// responses." RFC 5936 §2.2.5's first-message-only rule is about a
	// sequence of messages; a single datagram falls under the plain one.
	f := newXFRFixture(t, xfrZone("127.0.0.0/8"), xfrRecords())

	q := new(dns.Msg)
	q.SetQuestion(dns.Fqdn(xfrApex), dns.TypeIXFR)
	q.SetEdns0(1232, false)
	c := &dns.Client{Net: "udp"}
	reply, _, err := c.Exchange(q, f.addr)
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	assertRcode(t, reply, dns.RcodeSuccess)
	if reply.IsEdns0() == nil {
		t.Error("the reply carries no OPT record, though the query supplied one")
	}
	if len(reply.Answer) != 1 {
		t.Fatalf("got %d records (%v), want the single SOA", len(reply.Answer), rrNames(reply.Answer))
	}
}

// §9.5.7's two runtime bounds: the concurrency cap, and the per-envelope
// context check that catches a transfer that is merely slow rather than
// gone for good.

func TestTransfersBeyondTheCapAreRefused(t *testing.T) {
	// Cap of 1, and a hold hook that blocks the first transfer after it has
	// taken that one slot but before it streams anything — the same job D2's
	// testPrimary withHold does from the primary's side of a transfer.
	// started is buffered so every call after the first can send to it
	// without blocking; only the first send is ever received.
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	hold := func() {
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
	}
	f := newXFRFixture(t, xfrZone("127.0.0.0/8"), xfrRecords(),
		withMaxConcurrentTransfers(1), withTransferHold(hold))

	firstDone := make(chan error, 1)
	go func() {
		_, err := f.axfr(t, xfrApex, "", "")
		firstDone <- err
	}()

	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("the first transfer never reached the hold hook")
	}

	// Held: a second AXFR is refused at the cap rather than queued behind
	// it. SERVFAIL is the row's rcode (§9.5.5's last), because the reply
	// says "ask again", not "you may never ask".
	over := f.exchange(t, xfrApex, dns.TypeAXFR)
	assertRcode(t, over, dns.RcodeServerFailure)
	if len(over.Answer) != 0 {
		t.Fatalf("an over-capacity AXFR carried %d records: %v", len(over.Answer), rrNames(over.Answer))
	}

	close(release)

	select {
	case err := <-firstDone:
		if err != nil {
			t.Fatalf("the held transfer: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the held transfer never finished after release")
	}

	// Released: a third transfer succeeds, which is what proves the cap is
	// concurrent rather than cumulative — a permanently spent budget of 1
	// would refuse this one too. axfrEventually rather than a single axfr:
	// firstDone firing only proves the client finished reading the first
	// transfer's wire, not that the server has returned from serve and freed
	// its slot yet — those are racing, not ordered.
	rrs := f.axfrEventually(t, xfrApex)
	if len(rrs) == 0 {
		t.Fatal("the third transfer carried no records")
	}
}

// xfrBrokenApex is a second zone whose one record will not render: "A" with
// an rdata that is not an address. Nothing writes such a row — the API
// validates rdata through the same dns.NewRR the transfer renders with — so
// its provenance is a hand-edited database, which is what the store write
// below stands in for.
const xfrBrokenApex = "broken.e412.in"

// brokenZone is that zone, ACL and all.
func brokenZone() (store.Zone, []store.ZoneRecord) {
	z := xfrZone("127.0.0.0/8")
	z.Name = xfrBrokenApex
	z.SOANS, z.SOAMbox = "ns1."+xfrBrokenApex, "hostmaster."+xfrBrokenApex
	return z, []store.ZoneRecord{
		{Name: "bad", Type: "A", TTL: 300, RData: "not-an-address", Enabled: true},
	}
}

func TestTheZoneIsBuiltInsideTheConcurrencySlotAndNotBeforeIt(t *testing.T) {
	// §9.5.7 justifies the cap by what a stalled peer holds: "a goroutine and
	// that zone's built RR slice". Both have to be inside the cap for that to
	// be true — a zone rendered before the slot is taken is a whole zone
	// rendered per connection, at the cost of one dns.NewRR per record, for
	// peers that are then refused and throw the result away. dns.Server caps
	// concurrent TCP connections at nothing, so "peers that reach here" is
	// bounded only by the ACL, and a CIDR ACL is the ordinary configuration.
	//
	// Two zones and one slot make the order observable. The good zone's
	// transfer holds the only slot at the hook; the broken zone is asked for
	// while it is held. Both outcomes are SERVFAIL, so the rcode cannot tell
	// which check ran first — the reason recorded against the zone row can:
	// the cap's if the slot came first, the unrenderable row's if the render
	// did.
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	hold := func() {
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
	}
	f := newXFRFixture(t, xfrZone("127.0.0.0/8"), xfrRecords(),
		withMaxConcurrentTransfers(1), withTransferHold(hold))
	bz, brecs := brokenZone()
	f.addZone(t, bz, brecs)

	firstDone := make(chan error, 1)
	go func() {
		_, err := f.axfr(t, xfrApex, "", "")
		firstDone <- err
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("the first transfer never reached the hold hook")
	}

	over := f.exchange(t, xfrBrokenApex, dns.TypeAXFR)
	assertRcode(t, over, dns.RcodeServerFailure)
	// refuse records before it replies, so the reply having arrived means the
	// row has already been written.
	if got := f.zoneRowByName(t, xfrBrokenApex).LastXfrError; got != "at the concurrent transfer limit" {
		t.Fatalf("last_xfr_error = %q, want %q: the zone was rendered before the slot was taken",
			got, "at the concurrent transfer limit")
	}

	close(release)
	select {
	case err := <-firstDone:
		if err != nil {
			t.Fatalf("the held transfer: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the held transfer never finished after release")
	}
}

// TestAnUnrenderableRowRefusesTheWholeTransfer is the other half of the pair:
// with a slot free, the row is what refuses, and the reason names it.
func TestAnUnrenderableRowRefusesTheWholeTransfer(t *testing.T) {
	f := newXFRFixture(t, xfrZone("127.0.0.0/8"), xfrRecords())
	bz, brecs := brokenZone()
	f.addZone(t, bz, brecs)

	m := f.exchange(t, xfrBrokenApex, dns.TypeAXFR)
	assertRcode(t, m, dns.RcodeServerFailure)
	if len(m.Answer) != 0 {
		t.Fatalf("a refused transfer carried %d records: %v", len(m.Answer), rrNames(m.Answer))
	}
	if got := f.zoneRowByName(t, xfrBrokenApex).LastXfrError; !strings.Contains(got, "will not render") {
		t.Fatalf("last_xfr_error = %q, want the unrenderable row's reason", got)
	}

	// The slot the refused transfer took is back: with the default cap that
	// is not otherwise visible, and a render failure is the one path that
	// returns between taking a slot and streaming.
	if rrs := f.axfrEventually(t, xfrApex); len(rrs) == 0 {
		t.Fatal("the transfer after the unrenderable one carried no records")
	}
}

func TestACapBelowOneIsClampedRatherThanFatal(t *testing.T) {
	// WithMaxConcurrentTransfers is exported, so its argument comes from
	// outside. -1 panics inside make(chan struct{}, n) at construction, and 0
	// leaves slots unbuffered, which makes the non-blocking send take its
	// default branch every time and every transfer this server serves
	// SERVFAIL. Both are clamped to one, which is the smallest number that is
	// still a cap — and the assertion is that a transfer is actually served,
	// not merely that construction returned.
	for _, n := range []int{0, -1} {
		t.Run(fmt.Sprintf("cap %d", n), func(t *testing.T) {
			f := newXFRFixture(t, xfrZone("127.0.0.0/8"), xfrRecords(), withMaxConcurrentTransfers(n))
			rrs, err := f.axfr(t, xfrApex, "", "")
			if err != nil {
				t.Fatalf("axfr: %v", err)
			}
			if len(rrs) == 0 {
				t.Fatal("the transfer carried no records")
			}
		})
	}
}

// stalledZoneRecordCount is deliberately smaller than bigZoneRecordCount:
// TestTheTransferContextBoundsAStalledTransfer needs at least two envelopes
// (xfrBigRecords(400) reliably produces them — 400 records past ~44 bytes
// each is past the 16 KiB target with margin), and no more, because building
// the answer and batching it happens before the context is ever checked, and
// -race can make that construction slow enough to eat into a short deadline
// on a loaded machine. Fewer records means less of the deadline is spent on
// setup rather than on the thing being tested.
const stalledZoneRecordCount = 400

func TestTheTransferContextBoundsAStalledTransfer(t *testing.T) {
	// The context comes from dnssrv.serve (TransferTimeout), so it cannot be
	// shortened through a TransferServer option; ServeTransfer is called
	// directly instead, with a writer that blocks in WriteMsg and a context
	// that expires in 100ms. The zone gives the loop at least two envelopes
	// to try, which is what makes this test able to fail: a single envelope
	// would return the moment the (eventually released) write unblocks,
	// context or no context, and prove nothing about the check between
	// envelopes.
	f := newXFRFixture(t, xfrZone("127.0.0.0/8"), xfrBigRecords(stalledZoneRecordCount))
	ts := zones.NewTransferServer(f.res, f.st.Zones(), zones.WithMaxConcurrentTransfers(1))

	release := make(chan struct{})
	// Closed well after the 100ms deadline below, so the context has already
	// expired by the time WriteMsg returns — standing in for a peer that is
	// slow rather than gone for good. If the per-envelope check did not run,
	// the loop would go on to every remaining envelope: release is already
	// closed by then, so nothing would block it.
	time.AfterFunc(400*time.Millisecond, func() { close(release) })
	w := &blockingWriter{release: release, remote: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 40000}}

	q := new(dns.Msg)
	q.SetAxfr(dns.Fqdn(xfrApex))

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() {
		ts.ServeTransfer(ctx, w, q, "", dnssrv.ErrTSIGUnsigned)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("ServeTransfer did not return within 3 seconds of its context expiring")
	}
	// At most one: the loop may notice the expired context before the first
	// envelope (0 writes) or only after the first one returns (1 write) —
	// both are the check doing its job. Two or more would mean it kept
	// going once release made every further write instant, which is what
	// happens with the check removed.
	if w.writes > 1 {
		t.Fatalf("WriteMsg was called %d times; the context should have stopped the loop after at most one stalled envelope", w.writes)
	}

	// The slot is what proves it was released rather than leaked: with the
	// cap at 1, a transfer that left its slot held would refuse this one.
	w2 := new(captureWriter)
	ts.ServeTransfer(context.Background(), w2, q, "", dnssrv.ErrTSIGUnsigned)
	if w2.msg == nil || w2.msg.Rcode != dns.RcodeSuccess {
		got := "no message"
		if w2.msg != nil {
			got = dns.RcodeToString[w2.msg.Rcode]
		}
		t.Fatalf("the transfer after the stalled one got %s, want a served transfer", got)
	}
}

// deadWriter is the peer §9.5.7 is a bound on: one that stops reading
// altogether, so WriteMsg blocks with no deadline the library will ever apply
// (dns.Server.WriteTimeout is documented and never used, and
// dns.ResponseWriter exposes no connection to set one on).
//
// Close is the only lever there is, and it is the one this models: in
// production it closes the TCP connection, which makes the blocked write fail.
// A writer whose WriteMsg returned on a timer of its own would test a peer
// that is slow, which the test above already covers, rather than one that is
// gone.
type deadWriter struct {
	mu     sync.Mutex
	writes int
	closed chan struct{}
	once   sync.Once
}

func newDeadWriter() *deadWriter { return &deadWriter{closed: make(chan struct{})} }

func (w *deadWriter) LocalAddr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 53}
}

func (w *deadWriter) RemoteAddr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 40000}
}

func (w *deadWriter) WriteMsg(*dns.Msg) error {
	w.mu.Lock()
	w.writes++
	w.mu.Unlock()
	<-w.closed
	return net.ErrClosed
}

func (w *deadWriter) Write(b []byte) (int, error) {
	if err := w.WriteMsg(nil); err != nil {
		return 0, err
	}
	return len(b), nil
}

func (w *deadWriter) Close() error {
	w.once.Do(func() { close(w.closed) })
	return nil
}

func (w *deadWriter) TsigStatus() error   { return nil }
func (w *deadWriter) TsigTimersOnly(bool) {}
func (w *deadWriter) Hijack()             {}

// The case the concurrency cap exists for and nothing exercised: a peer whose
// WriteMsg never returns at all. The deadline is only checked between
// envelopes, so a transfer already blocked inside a write stays blocked, and
// with it a slot that four such peers can exhaust — every later secondary
// SERVFAILs until those TCP connections die of their own accord, which for a
// peer that is merely not reading may be never.
//
// Closing the writer is the lever, and it is the same one Transferrer.fetch
// already uses from the client side.
func TestATransferToAPeerThatNeverReadsReleasesItsSlot(t *testing.T) {
	f := newXFRFixture(t, xfrZone("127.0.0.0/8"), xfrBigRecords(stalledZoneRecordCount))
	ts := zones.NewTransferServer(f.res, f.st.Zones(), zones.WithMaxConcurrentTransfers(1))

	w := newDeadWriter()
	q := new(dns.Msg)
	q.SetAxfr(dns.Fqdn(xfrApex))

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() {
		ts.ServeTransfer(ctx, w, q, "", dnssrv.ErrTSIGUnsigned)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("ServeTransfer never returned: the write it is blocked in outlives its own deadline")
	}

	// The slot is the assertion. With the cap at 1, a transfer that left its
	// slot held would refuse this one.
	w2 := new(captureWriter)
	ts.ServeTransfer(context.Background(), w2, q, "", dnssrv.ErrTSIGUnsigned)
	if w2.msg == nil || w2.msg.Rcode != dns.RcodeSuccess {
		got := "no message"
		if w2.msg != nil {
			got = dns.RcodeToString[w2.msg.Rcode]
		}
		t.Fatalf("the transfer after the stalled one got %s, want a served transfer", got)
	}
}

// A refusal that says nothing about a zone this server holds is not an
// operator's problem: an apex it holds nothing for, or a peer using UDP for a
// TCP protocol, is one unauthenticated packet away for anybody, so a Warn per
// packet is a log an attacker writes. The NOTIFY gate's twin already logs its
// refusals at debug for exactly this reason. A refusal *about a zone this
// server holds* stays loud, because that is the one an operator is hunting
// when a secondary stops updating.
func TestARefusalAboutNoZoneOfOursIsNotWarned(t *testing.T) {
	logs := captureLogs(t)
	f := newXFRFixture(t, xfrZone("key:ns2."+xfrApex), xfrRecords())

	// No zone at that apex, and an AXFR over UDP: the two rows any source can
	// reach without being anywhere near the ACL.
	f.exchange(t, "somewhere.else", dns.TypeAXFR)
	f.exchangeUDP(t, xfrApex, dns.TypeAXFR)
	for _, r := range logs() {
		if r.Message == "zone transfer refused" && r.Level >= slog.LevelWarn {
			t.Errorf("a refusal any UDP source can provoke was logged at %s: %s", r.Level, r.Message)
		}
	}

	// The control: the ACL refusing a peer for a zone this server does hold is
	// still a warning, or this check would pass by silencing everything.
	f.exchange(t, xfrApex, dns.TypeAXFR)
	warned := false
	for _, r := range logs() {
		if r.Message == "zone transfer refused" && r.Level >= slog.LevelWarn {
			warned = true
		}
	}
	if !warned {
		t.Error("a peer refused by allow_transfer for a zone we hold was not warned about")
	}
}

// §9.5.8: recording what happened. Both outcomes are written to the zone
// row, refusals included, because "ns2 is not updating" is usually "ns2 is
// not in allow_transfer" and the log is not where an operator looks first.
// The tests below call ServeTransfer directly, the way the concurrency-cap
// and stalled-transfer tests above already do, so the store handed to
// NewTransferServer can be a counting wrapper rather than the fixture's own.

// countingZoneStore counts calls to NoteTransferRequest and delegates
// everything else, NoteTransferRequest included, to the store.ZoneStore it
// wraps — unless err is set, in which case NoteTransferRequest is counted and
// fails without reaching the wrapped store at all. It exists to assert how
// many state writes a run of transfers produced, which the store's own
// tests (zones_test.go, package store) have no reason to expose.
type countingZoneStore struct {
	store.ZoneStore
	mu  sync.Mutex
	n   int
	err error
}

func (c *countingZoneStore) NoteTransferRequest(ctx context.Context, zoneID, at int64, peer, errText string) error {
	c.mu.Lock()
	c.n++
	c.mu.Unlock()
	if c.err != nil {
		return c.err
	}
	return c.ZoneStore.NoteTransferRequest(ctx, zoneID, at, peer, errText)
}

func (c *countingZoneStore) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

// stallingZoneStore blocks inside its first NoteTransferRequest until
// release is closed, standing in for a state write that is slow rather than
// failing — sqlite serialises every write behind one connection
// (SetMaxOpenConns(1)), so one is not hypothetical. entered closes as that
// first call begins, so a test can act while it is still in flight. Every
// later call delegates straight through, which the throttle means there
// usually is not one of.
type stallingZoneStore struct {
	store.ZoneStore
	mu      sync.Mutex
	calls   int
	entered chan struct{}
	release chan struct{}
}

func (s *stallingZoneStore) NoteTransferRequest(ctx context.Context, zoneID, at int64, peer, errText string) error {
	s.mu.Lock()
	s.calls++
	first := s.calls == 1
	s.mu.Unlock()
	if first {
		close(s.entered)
		<-s.release
	}
	return s.ZoneStore.NoteTransferRequest(ctx, zoneID, at, peer, errText)
}

// xfrAxfrMsg is a plain AXFR query for qname, unsigned.
func xfrAxfrMsg(qname string) *dns.Msg {
	m := new(dns.Msg)
	m.SetAxfr(dns.Fqdn(qname))
	return m
}

// zoneRowByName reads the current state of the zone named name back out of
// the fixture's store — the only way to see what NoteTransferRequest wrote,
// since dns.Transfer.In discards everything but the wire records.
func (f *xfrFixture) zoneRowByName(t *testing.T, name string) store.Zone {
	t.Helper()
	all, err := f.st.Zones().Zones(context.Background())
	if err != nil {
		t.Fatalf("Zones: %v", err)
	}
	for _, z := range all {
		if z.Name == name {
			return z
		}
	}
	t.Fatalf("no zone named %q in the store", name)
	return store.Zone{}
}

func TestASuccessfulTransferRecordsWhoTookIt(t *testing.T) {
	f := newXFRFixture(t, xfrZone("127.0.0.0/8"), xfrRecords())
	now := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	ts := zones.NewTransferServer(f.res, f.st.Zones(), zones.WithTransferServerNow(func() time.Time { return now }))

	w := new(captureWriter)
	ts.ServeTransfer(context.Background(), w, xfrAxfrMsg(xfrApex), "", dnssrv.ErrTSIGUnsigned)
	if w.msg == nil || w.msg.Rcode != dns.RcodeSuccess {
		t.Fatalf("the transfer did not succeed: %v", w.msg)
	}

	zn := f.zoneRowByName(t, xfrApex)
	if zn.LastXfrAt != now.UnixMilli() {
		t.Errorf("last_xfr_at = %d, want the injected now (%d)", zn.LastXfrAt, now.UnixMilli())
	}
	// The address only, no port: captureWriter.RemoteAddr is
	// 127.0.0.1:40000, and the port is a new ephemeral number every time —
	// it identifies nothing.
	if zn.LastXfrPeer != "127.0.0.1" {
		t.Errorf("last_xfr_peer = %q, want 127.0.0.1 with no port", zn.LastXfrPeer)
	}
	if zn.LastXfrError != "" {
		t.Errorf("last_xfr_error = %q, want empty for a served transfer", zn.LastXfrError)
	}
}

func TestARefusalRecordsItsReason(t *testing.T) {
	// The client is 127.0.0.1 and the ACL names somewhere else, so the
	// transfer is refused — the diagnostic last_xfr_error exists for.
	f := newXFRFixture(t, xfrZone("10.0.0.0/24"), xfrRecords())
	now := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	ts := zones.NewTransferServer(f.res, f.st.Zones(), zones.WithTransferServerNow(func() time.Time { return now }))

	w := new(captureWriter)
	ts.ServeTransfer(context.Background(), w, xfrAxfrMsg(xfrApex), "", dnssrv.ErrTSIGUnsigned)
	if w.msg == nil || w.msg.Rcode != dns.RcodeRefused {
		t.Fatalf("the transfer was not refused: %v", w.msg)
	}

	zn := f.zoneRowByName(t, xfrApex)
	if zn.LastXfrPeer != "127.0.0.1" {
		t.Errorf("last_xfr_peer = %q, want 127.0.0.1", zn.LastXfrPeer)
	}
	if !strings.Contains(zn.LastXfrError, "allow_transfer") {
		t.Errorf("last_xfr_error = %q, want it to name allow_transfer", zn.LastXfrError)
	}
}

// xfrUDPAXFRMsg is a plain AXFR query for qname, the message shape
// TestAUDPAXFRRefusalWritesNothingToAnExistingRow and
// TestAUDPGateRefusalWritesNothing hand to ServeTransfer directly, alongside
// a captureWriter whose remote is a *net.UDPAddr — the same query
// xfrAxfrMsg builds, but naming it separately keeps the UDP-recording tests
// from reading as though the qtype were the thing under test. It is not: the
// transport is.
func xfrUDPAXFRMsg(qname string) *dns.Msg {
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(qname), dns.TypeAXFR)
	return m
}

// TestAUDPAXFRRefusalWritesNothingToAnExistingRow pins the case hit live: a
// UDP AXFR probe (RFC 5936 §4.2's undefined transport, answered NOTIMP)
// followed a real TCP transfer and overwrote what that transfer had just
// recorded — "Last served 2m ago to 192.168.150.40" replaced by "Refused
// 192.168.150.40 — AXFR over UDP is not defined", from the same peer,
// describing the same zone. §9.5.8's TCP-only rule exists precisely so a
// probe like this cannot displace a transfer's own record of itself.
//
// ServeTransfer is called directly, as the rest of this recording section
// does, rather than through the fixture's real listener: a client sees a
// transfer as complete the instant it finishes reading the wire, which races
// the server's own note() write (see axfrEventually's comment above), and
// that race would make this test flaky in the direction that hides the bug.
func TestAUDPAXFRRefusalWritesNothingToAnExistingRow(t *testing.T) {
	f := newXFRFixture(t, xfrZone("127.0.0.0/8"), xfrRecords())
	clock := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	ts := zones.NewTransferServer(f.res, f.st.Zones(), zones.WithTransferServerNow(func() time.Time { return clock }))

	// A real TCP transfer first, so the row carries state a UDP write could
	// clobber.
	tcpW := new(captureWriter)
	ts.ServeTransfer(context.Background(), tcpW, xfrAxfrMsg(xfrApex), "", dnssrv.ErrTSIGUnsigned)
	if tcpW.msg == nil || tcpW.msg.Rcode != dns.RcodeSuccess {
		t.Fatalf("the TCP transfer did not succeed: %v", tcpW.msg)
	}
	before := f.zoneRowByName(t, xfrApex)
	if before.LastXfrAt == 0 {
		t.Fatal("the TCP transfer recorded nothing to compare against")
	}

	// An hour later — past transferStateThrottle by a wide margin, so a
	// write here is not merely one the throttle happened to suppress. If
	// the UDP refusal reached note() at all, this would overwrite
	// last_xfr_at/last_xfr_error with the probe's own.
	clock = clock.Add(time.Hour)
	udpW := &captureWriter{remote: &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 40000}}
	ts.ServeTransfer(context.Background(), udpW, xfrUDPAXFRMsg(xfrApex), "", dnssrv.ErrTSIGUnsigned)
	if udpW.msg == nil || udpW.msg.Rcode != dns.RcodeNotImplemented {
		t.Fatalf("the UDP AXFR was not answered NOTIMP: %v", udpW.msg)
	}

	after := f.zoneRowByName(t, xfrApex)
	if after != before {
		t.Fatalf("a UDP AXFR refusal changed the zone row:\nbefore: %+v\nafter:  %+v", before, after)
	}
}

// TestAUDPGateRefusalWritesNothing is the other half: a peer outside
// allow_transfer, asking over UDP, is the write the security review flagged
// as spoofable — a UDP source address is trivial to forge, so recording it
// would let an off-path attacker plant an arbitrary last_xfr_peer. TCP-only
// recording closes it for every refusal the gate can produce, not only the
// AXFR-over-UDP one above.
func TestAUDPGateRefusalWritesNothing(t *testing.T) {
	f := newXFRFixture(t, xfrZone("10.0.0.0/24"), xfrRecords())
	clock := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	ts := zones.NewTransferServer(f.res, f.st.Zones(), zones.WithTransferServerNow(func() time.Time { return clock }))

	// A real TCP refusal first, so the row carries state a UDP write could
	// clobber.
	tcpW := new(captureWriter)
	ts.ServeTransfer(context.Background(), tcpW, xfrAxfrMsg(xfrApex), "", dnssrv.ErrTSIGUnsigned)
	if tcpW.msg == nil || tcpW.msg.Rcode != dns.RcodeRefused {
		t.Fatalf("the TCP refusal was not REFUSED: %v", tcpW.msg)
	}
	before := f.zoneRowByName(t, xfrApex)
	if before.LastXfrAt == 0 {
		t.Fatal("the TCP refusal recorded nothing to compare against")
	}

	clock = clock.Add(time.Hour) // past transferStateThrottle, as above
	udpW := &captureWriter{remote: &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 40000}}
	ts.ServeTransfer(context.Background(), udpW, xfrUDPAXFRMsg(xfrApex), "", dnssrv.ErrTSIGUnsigned)
	if udpW.msg == nil || udpW.msg.Rcode != dns.RcodeRefused {
		t.Fatalf("the UDP gate refusal was not REFUSED: %v", udpW.msg)
	}

	after := f.zoneRowByName(t, xfrApex)
	if after != before {
		t.Fatalf("a UDP gate refusal changed the zone row:\nbefore: %+v\nafter:  %+v", before, after)
	}
}

func TestStateWritesAreThrottled(t *testing.T) {
	// A peer outside the ACL, asked ten times with the clock held still: an
	// unauthenticated peer in a loop must not become an UPDATE loop against
	// sqlite's single connection.
	f := newXFRFixture(t, xfrZone("10.0.0.0/24"), xfrRecords())
	now := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	counting := &countingZoneStore{ZoneStore: f.st.Zones()}
	ts := zones.NewTransferServer(f.res, counting, zones.WithTransferServerNow(func() time.Time { return now }))

	for i := 0; i < 10; i++ {
		ts.ServeTransfer(context.Background(), new(captureWriter), xfrAxfrMsg(xfrApex), "", dnssrv.ErrTSIGUnsigned)
	}
	if got := counting.count(); got != 1 {
		t.Fatalf("NoteTransferRequest was called %d times for 10 refusals inside the throttle window, want 1", got)
	}
}

func TestAChangedOutcomeIsWrittenThroughTheThrottle(t *testing.T) {
	// A refusal, then — one second later, well inside the ten-second window
	// — a success. A screen that kept saying "refused" for the other nine
	// seconds would be wrong exactly when someone is watching it.
	f := newXFRFixture(t, xfrZone(""), xfrRecords()) // empty ACL: default deny
	now := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	counting := &countingZoneStore{ZoneStore: f.st.Zones()}
	ts := zones.NewTransferServer(f.res, counting, zones.WithTransferServerNow(func() time.Time { return now }))

	w1 := new(captureWriter)
	ts.ServeTransfer(context.Background(), w1, xfrAxfrMsg(xfrApex), "", dnssrv.ErrTSIGUnsigned)
	if w1.msg == nil || w1.msg.Rcode != dns.RcodeRefused {
		t.Fatalf("the first transfer was not refused: %v", w1.msg)
	}

	f.setACL(t, xfrApex, "127.0.0.0/8")
	now = now.Add(time.Second)
	w2 := new(captureWriter)
	ts.ServeTransfer(context.Background(), w2, xfrAxfrMsg(xfrApex), "", dnssrv.ErrTSIGUnsigned)
	if w2.msg == nil || w2.msg.Rcode != dns.RcodeSuccess {
		t.Fatalf("the second transfer did not succeed: %v", w2.msg)
	}

	if got := counting.count(); got != 2 {
		t.Fatalf("a changed outcome inside the throttle window produced %d write(s), want 2", got)
	}
}

func TestDifferentRefusalReasonsInsideTheWindowWriteOnce(t *testing.T) {
	// Two different refusal reasons for the same zone and peer are still
	// both refusals — not the served↔refused transition the override
	// exists for — so this must stay throttled to one write. A peer that
	// alternates between refusal shapes (here: an unsigned request, then one
	// signed under a key this server does not hold) must not turn the
	// override into an unthrottled write on every request.
	f := newXFRFixture(t, xfrZone("10.0.0.0/24"), xfrRecords())
	now := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	counting := &countingZoneStore{ZoneStore: f.st.Zones()}
	ts := zones.NewTransferServer(f.res, counting, zones.WithTransferServerNow(func() time.Time { return now }))

	w1 := new(captureWriter)
	ts.ServeTransfer(context.Background(), w1, xfrAxfrMsg(xfrApex), "", dnssrv.ErrTSIGUnsigned)
	if w1.msg == nil || w1.msg.Rcode != dns.RcodeRefused {
		t.Fatalf("the first refusal was %v, want REFUSED (peer outside the ACL)", w1.msg)
	}

	w2 := new(captureWriter)
	ts.ServeTransfer(context.Background(), w2, xfrAxfrMsg(xfrApex), "nosuch."+xfrApex, dns.ErrSecret)
	if w2.msg == nil || w2.msg.Rcode != dns.RcodeNotAuth {
		t.Fatalf("the second refusal was %v, want NOTAUTH (unknown TSIG key)", w2.msg)
	}

	if got := counting.count(); got != 1 {
		t.Fatalf("two different refusal reasons inside the throttle window produced %d write(s), want 1", got)
	}
}

func TestTheRefusalLogIsNeverThrottled(t *testing.T) {
	// The throttle bounds the database write; the log line is what an
	// operator watching a live log sees, and it fires on every refusal
	// regardless of what the throttle did to the write.
	f := newXFRFixture(t, xfrZone("10.0.0.0/24"), xfrRecords())
	now := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	counting := &countingZoneStore{ZoneStore: f.st.Zones()}
	ts := zones.NewTransferServer(f.res, counting, zones.WithTransferServerNow(func() time.Time { return now }))

	logs := captureLogs(t)
	const attempts = 5
	for i := 0; i < attempts; i++ {
		ts.ServeTransfer(context.Background(), new(captureWriter), xfrAxfrMsg(xfrApex), "", dnssrv.ErrTSIGUnsigned)
	}

	if got := counting.count(); got != 1 {
		t.Fatalf("the store was written %d time(s) for %d refusals inside the throttle window, want 1", got, attempts)
	}
	if got := countAtLeast(logs(), slog.LevelWarn); got != attempts {
		t.Fatalf("%d WARN log line(s) for %d refusals, want %d: the log line must never be throttled", got, attempts, attempts)
	}
}

func TestTheThrottleIsPerZone(t *testing.T) {
	// A refusal on zone A, immediately followed by a refusal on zone B, the
	// clock held still: the throttle must be keyed by zone, not global, or
	// the second write would be dropped as though it were a repeat of the
	// first.
	f := newXFRFixture(t, xfrZone("10.0.0.0/24"), xfrRecords())
	ctx := context.Background()

	zoneB := xfrZone("10.0.0.0/24")
	zoneB.Name = "zoneb.test"
	if _, err := f.st.Zones().AddZone(ctx, zoneB); err != nil {
		t.Fatalf("AddZone: %v", err)
	}
	if err := f.res.Reload(ctx); err != nil {
		t.Fatalf("Reload: %v", err)
	}

	now := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	counting := &countingZoneStore{ZoneStore: f.st.Zones()}
	ts := zones.NewTransferServer(f.res, counting, zones.WithTransferServerNow(func() time.Time { return now }))

	wA := new(captureWriter)
	ts.ServeTransfer(ctx, wA, xfrAxfrMsg(xfrApex), "", dnssrv.ErrTSIGUnsigned)
	wB := new(captureWriter)
	ts.ServeTransfer(ctx, wB, xfrAxfrMsg("zoneb.test"), "", dnssrv.ErrTSIGUnsigned)

	// Both refused: the throttle must not be reachable through the
	// changed-outcome override either, which a served zone B would leave
	// this test unable to tell apart from.
	if wA.msg == nil || wA.msg.Rcode != dns.RcodeRefused {
		t.Fatalf("zone A's transfer was %v, want REFUSED", wA.msg)
	}
	if wB.msg == nil || wB.msg.Rcode != dns.RcodeRefused {
		t.Fatalf("zone B's transfer was %v, want REFUSED", wB.msg)
	}

	if got := counting.count(); got != 2 {
		t.Fatalf("a refusal on zone A throttled the first record on zone B: got %d write(s), want 2", got)
	}
}

func TestAFailedStateWriteDoesNotFailTheTransfer(t *testing.T) {
	// The records are what the transfer is for; the bookkeeping is not
	// worth failing it over.
	f := newXFRFixture(t, xfrZone("127.0.0.0/8"), xfrRecords())
	counting := &countingZoneStore{ZoneStore: f.st.Zones(), err: errors.New("store is down")}
	ts := zones.NewTransferServer(f.res, counting)

	w := new(captureWriter)
	ts.ServeTransfer(context.Background(), w, xfrAxfrMsg(xfrApex), "", dnssrv.ErrTSIGUnsigned)
	if w.msg == nil || w.msg.Rcode != dns.RcodeSuccess {
		t.Fatalf("the transfer failed even though only the state write did: %v", w.msg)
	}
	if len(w.msg.Answer) == 0 {
		t.Fatal("the peer got no records even though the transfer should have succeeded")
	}
	if got := counting.count(); got != 1 {
		t.Fatalf("NoteTransferRequest was called %d time(s), want 1 (the attempt still happened)", got)
	}
}

func TestTheSlotIsFreedBeforeTheStateWrite(t *testing.T) {
	// Two runtime bounds that must not be chained together: §9.5.7's slot
	// bounds a peer that has stopped reading, and §9.5.8's write is a
	// synchronous UPDATE behind sqlite's one connection. Freeing the slot
	// after the write would make a slow database shrink the transfer cap for
	// peers that already have their zone and have gone.
	//
	// It is a one-line invariant with no rcode of its own, and replacing the
	// explicit release with a plain deferred one passes every other test in
	// this package. The cap is 1 here, so a slot still held is a second
	// transfer refused.
	f := newXFRFixture(t, xfrZone("127.0.0.0/8"), xfrRecords())
	stalling := &stallingZoneStore{
		ZoneStore: f.st.Zones(),
		entered:   make(chan struct{}),
		release:   make(chan struct{}),
	}
	ts := zones.NewTransferServer(f.res, stalling, zones.WithMaxConcurrentTransfers(1))

	done := make(chan struct{})
	go func() {
		ts.ServeTransfer(context.Background(), new(captureWriter), xfrAxfrMsg(xfrApex), "", dnssrv.ErrTSIGUnsigned)
		close(done)
	}()
	select {
	case <-stalling.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("the first transfer never reached its state write")
	}

	// It has streamed its whole zone and is now inside note, which is not
	// going to return. The only slot has to be back already. (This second
	// transfer's own note is dropped by the throttle — same zone, same
	// outcome, inside the window — so it does not stall in turn.)
	w := new(captureWriter)
	ts.ServeTransfer(context.Background(), w, xfrAxfrMsg(xfrApex), "", dnssrv.ErrTSIGUnsigned)
	if w.msg == nil {
		t.Fatal("nothing was written for the second transfer")
	}
	assertRcode(t, w.msg, dns.RcodeSuccess)
	if len(w.msg.Answer) == 0 {
		t.Fatal("the second transfer carried no records")
	}

	close(stalling.release)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the first transfer never returned after its state write completed")
	}
}

// §6 of docs/superpowers/specs/2026-09-11-config-sync-design.md: a replica
// registered with its main transfers every primary zone on the strength of
// the sync key alone. The zone here is untouched — allow_transfer is the ""
// every zone is created with — because the point of the clause is that
// adding a replica does not mean editing each zone's ACL.
func TestRegisteredReplicaMayTransferWithoutAnACLEntry(t *testing.T) {
	syncName := dns.CanonicalName("sync." + xfrApex)
	otherName := dns.CanonicalName("ns2." + xfrApex)
	f := newXFRFixture(t, xfrZone(""), xfrRecords(),
		withReplicaAllow(func(key string, peer netip.Addr) bool {
			return key == syncName && peer.IsLoopback()
		}))
	syncSecret := xfrKey(t, f.st, syncName, dns.HmacSHA256)
	otherSecret := xfrKey(t, f.st, otherName, dns.HmacSHA256)

	rrs, err := f.axfr(t, xfrApex, syncName, syncSecret)
	if err != nil {
		t.Fatalf("axfr signed with the sync key: %v", err)
	}
	if len(rrs) != 4 {
		t.Fatalf("got %d RRs (%v), want the SOA, both enabled records and the closing SOA", len(rrs), rrNames(rrs))
	}

	// The same peer under a key the main has not designated. The clause is
	// about one key, not about "any signature from a box we have heard of".
	assertRcode(t, f.exchangeSigned(t, xfrApex, otherName, dns.HmacSHA256, otherSecret, time.Now().Unix()), dns.RcodeRefused)

	// And the sync key from a box that is not registered: knowing the secret
	// is not the whole of the rule, or a replica the operator removed would
	// keep transferring.
	g := newXFRFixture(t, xfrZone(""), xfrRecords(),
		withReplicaAllow(func(string, netip.Addr) bool { return false }))
	xfrKey(t, g.st, syncName, dns.HmacSHA256)
	assertRcode(t, g.exchangeSigned(t, xfrApex, syncName, dns.HmacSHA256, syncSecret, time.Now().Unix()), dns.RcodeRefused)

	// An unsigned request, against a hook that admits everything it is
	// asked about. It is never asked: a peer that presented no signature is
	// only an address, and an address alone is what allow_transfer is for.
	// The clause authenticates a replica by its key, so there has to be one.
	h := newXFRFixture(t, xfrZone(""), xfrRecords(),
		withReplicaAllow(func(string, netip.Addr) bool { return true }))
	assertRcode(t, h.exchange(t, xfrApex, dns.TypeAXFR), dns.RcodeRefused)
}

// The implicit allow is the main handing out the zones it authors (§6). A
// secondary holds someone else's data on loan, and the operator's
// allow_transfer is the only thing that says who may have a copy of it.
func TestTheReplicaAllowDoesNotReachASecondaryZone(t *testing.T) {
	syncName := dns.CanonicalName("sync." + xfrApex)
	base := time.Now()
	z := secondaryZone(base.Add(-time.Hour).UnixMilli(), base.Add(time.Hour).UnixMilli())
	z.AllowTransfer = ""
	f := newXFRFixture(t, z, xfrRecords(),
		withReplicaAllow(func(string, netip.Addr) bool { return true }))
	secret := xfrKey(t, f.st, syncName, dns.HmacSHA256)

	assertRcode(t, f.exchangeSigned(t, xfrApex, syncName, dns.HmacSHA256, secret, time.Now().Unix()), dns.RcodeRefused)
}
