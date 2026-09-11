package zones_test

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/aloks98/dnsaur/internal/dnssrv"
	"github.com/aloks98/dnsaur/internal/store"
	"github.com/aloks98/dnsaur/internal/zones"
)

const notifyApex = "example.com"

// fakeRefresher stands in for *Refresher. Refresh records the zone it was
// asked for and blocks on gate when the test supplies one, so a test can
// observe the reply while a transfer is still running.
type fakeRefresher struct {
	mu    sync.Mutex
	calls []int64
	// gate, when non-nil, blocks Refresh until it is closed.
	gate chan struct{}
	// ctxAlive records whether the context Refresh was handed was still
	// live when it ran — the whole point of the background handoff.
	ctxAlive bool
	// entered, when non-nil, gets one non-blocking send per call *before*
	// gate is waited on. gate alone cannot tell a test that the work
	// goroutine is running, only that it has finished, and a lifetime test
	// has to catch it in flight.
	entered chan struct{}
}

func (f *fakeRefresher) Refresh(ctx context.Context, zoneID int64) (zones.TransferResult, error) {
	if f.entered != nil {
		select {
		case f.entered <- struct{}{}:
		default:
		}
	}
	if f.gate != nil {
		<-f.gate
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, zoneID)
	f.ctxAlive = ctx.Err() == nil
	return zones.TransferResult{}, nil
}

func (f *fakeRefresher) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// fakeProbe stands in for *Transferrer's SOA probe: it answers whatever
// serial the test has last put on the primary, and counts how many times it
// was asked. The count is the assertion the throttle tests actually turn on —
// "one probe per window" is a statement about this number, not about how many
// packets arrived.
type fakeProbe struct {
	mu     sync.Mutex
	serial uint32
	calls  int
}

func (p *fakeProbe) ProbeSerial(context.Context, store.Zone) (uint32, netip.AddrPort, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	return p.serial, netip.MustParseAddrPort("10.0.0.1:53"), nil
}

func (p *fakeProbe) setSerial(s uint32) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.serial = s
}

func (p *fakeProbe) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

// fakeClock is a clock a test can move while the server's own goroutines are
// reading it. A plain variable closed over by WithNotifyServerNow was enough
// while only the request goroutine read the clock; the work a deferred NOTIFY
// waits out reads it from a goroutine of its own, so anything a test moves
// under it has to be synchronised or it is a data race rather than a test.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock(t time.Time) *fakeClock { return &fakeClock{t: t} }

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// notifyFixture builds a NotifyServer over a real store and resolver, the
// way newXFRFixture does for the transfer gate.
type notifyFixture struct {
	ns   *zones.NotifyServer
	rf   *fakeRefresher
	st   store.Store
	zone store.Zone
}

// openTestStore returns a fresh sqlite store rooted in t.TempDir(), closed
// on cleanup. Every fixture in this package that needs a real store rather
// than a fake goes through this rather than opening one itself, so there is
// one place that knows how a test store is built.
func openTestStore(t *testing.T) store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), "sqlite", t.TempDir()+"/t.db")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("store.Close: %v", err)
		}
	})
	return st
}

func newNotifyFixture(t *testing.T, z store.Zone, opts ...zones.NotifyServerOption) *notifyFixture {
	t.Helper()
	ctx := context.Background()

	st := openTestStore(t)
	if z.Name != "" {
		id, err := st.Zones().AddZone(ctx, z)
		if err != nil {
			t.Fatalf("AddZone: %v", err)
		}
		z.ID = id
	}
	res := zones.NewResolver(st.Zones())
	if err := res.Reload(ctx); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	rf := &fakeRefresher{}
	// The key store is wired the way production wires it (internal/app), so
	// every row here exercises the real shape. A zone naming a key id no row
	// answers to — which is most of the table — reads exactly as it did
	// before it was attached.
	opts = append([]zones.NotifyServerOption{zones.WithNotifyKeys(st.TSIGKeys())}, opts...)
	return &notifyFixture{
		ns:   zones.NewNotifyServer(res, st.Zones(), rf, opts...),
		rf:   rf,
		st:   st,
		zone: z,
	}
}

// notify sends one NOTIFY through the gate and returns the reply. peer is
// the source address the gate matches against `primaries`.
//
// key and tsigErr are passed straight through, exactly as dnssrv.Server.serve
// passes what RequireTSIG already concluded rather than recomputing it. But
// RequireTSIG never strips the TSIG RR from the message it verified — a
// signed-but-failing request still carries one on the wire, which is what
// lets errorTSIG (reused from D3) build a reply naming the key and algorithm
// the request claimed. So a non-empty key here attaches a matching stub TSIG
// RR to m, the way a real signed request would arrive with one whether or
// not it goes on to verify.
func (f *notifyFixture) notify(t *testing.T, apex string, qtype uint16, peer, key string, tsigErr error) *dns.Msg {
	t.Helper()
	m := new(dns.Msg).SetNotify(dns.Fqdn(apex))
	m.Question[0].Qtype = qtype
	if key != "" {
		m.SetTsig(key, dns.HmacSHA256, 300, time.Now().Unix())
	}
	w := &captureWriter{remote: &net.UDPAddr{IP: net.ParseIP(peer), Port: 40000}}
	f.ns.ServeNotify(context.Background(), w, m, key, tsigErr)
	if w.msg == nil {
		t.Fatal("the gate wrote no reply")
	}
	return w.msg
}

// notifyZone is the secondary every gate row starts from; each case
// overrides the one field it is about.
func notifyZone() store.Zone {
	return store.Zone{
		Name: notifyApex, Type: "secondary", Enabled: true,
		Primaries: "10.0.0.1",
		SOANS:     "ns1." + notifyApex, SOAMbox: "hostmaster." + notifyApex,
		SOASerial: 10, SOARefresh: 3600, SOARetry: 600,
		SOAExpire: 604800, SOAMinimum: 300, SOATTL: 900,
		// A non-zero refreshed_at, so these rows exercise the gate rather
		// than the never-transferred shortcut (Task 7).
		RefreshedAt: 1,
	}
}

// tsigErrorOf returns the TSIG RR's Error field, or 0 when the reply carries
// no TSIG at all.
func tsigErrorOf(m *dns.Msg) uint16 {
	if t := m.IsTsig(); t != nil {
		return t.Error
	}
	return 0
}

// The gate, one case per row of §9.10.2's table. Each asserts the rcode
// *and* the TSIG error code where there is one, because a refusal with the
// right rcode and the wrong TSIG error tells the peer to fix the wrong
// thing.
func TestNotifyGate(t *testing.T) {
	tests := []struct {
		name        string
		mutate      func(z *store.Zone)
		qtype       uint16
		peer        string
		key         string
		tsigErr     error
		wantRcode   int
		wantTSIG    uint16 // 0 = no TSIG error RR
		wantRefresh bool
		wantSigned  bool // true: assert the reply carries a TSIG record
	}{
		{
			name:      "an authorised notify is accepted and triggers a refresh",
			qtype:     dns.TypeSOA,
			peer:      "10.0.0.1",
			wantRcode: dns.RcodeSuccess, wantRefresh: true,
		},
		{
			name:      "a qtype other than SOA is malformed",
			qtype:     dns.TypeA,
			peer:      "10.0.0.1",
			wantRcode: dns.RcodeFormatError,
		},
		{
			// The types that hold no data of their own and pull from a
			// master are the ones a master gets to tell. A stub pulls the
			// same way a secondary does — the same scheduler, the same
			// per-zone lock, the same recorded attempt (pullsFromAMaster) —
			// so a master that has just moved its delegation says so the
			// same way, and this gate is the one place that used to say
			// otherwise.
			name:      "a stub zone is told by its master, like the secondary it pulls like",
			mutate:    func(z *store.Zone) { z.Type = "stub" },
			qtype:     dns.TypeSOA,
			peer:      "10.0.0.1",
			wantRcode: dns.RcodeSuccess, wantRefresh: true,
		},
		{
			name:      "a primary zone is not told by anyone",
			mutate:    func(z *store.Zone) { z.Type = "primary"; z.Primaries = "" },
			qtype:     dns.TypeSOA,
			peer:      "10.0.0.1",
			wantRcode: dns.RcodeNotAuth,
		},
		{
			// The other half of the widened rule: "pulls from a master" is
			// the test, not "is not a primary". A forwarder claims a suffix
			// and names its upstreams outright in forward_to; there is
			// nobody to pull from, so a NOTIFY for one has no work it could
			// possibly cause.
			name:      "a forwarder zone has no master, so nobody tells it",
			mutate:    func(z *store.Zone) { z.Type = "forwarder"; z.ForwardTo = "10.0.0.1:53" },
			qtype:     dns.TypeSOA,
			peer:      "10.0.0.1",
			wantRcode: dns.RcodeNotAuth,
		},
		{
			name:      "a disabled zone",
			mutate:    func(z *store.Zone) { z.Enabled = false },
			qtype:     dns.TypeSOA,
			peer:      "10.0.0.1",
			wantRcode: dns.RcodeNotAuth,
		},
		{
			name:      "a source that is not a configured primary",
			qtype:     dns.TypeSOA,
			peer:      "10.9.9.9",
			wantRcode: dns.RcodeRefused,
		},
		{
			// tsigErr carries the sentinel, because that is what
			// Server.RequireTSIG actually produces for an unsigned message.
			// Leaving it nil exercises a branch production cannot reach:
			// RequireTSIG returns ("", err) or (name, nil) and never
			// ("", nil), so a row without the sentinel would pass even if
			// the live ErrTSIGUnsigned arm were deleted outright. (This is
			// exactly the defect a prior round of this gate had: the row
			// used tsigErr: nil, fell to a dead `key == ""` arm that
			// production can never reach, and the real ErrTSIGUnsigned arm
			// went untested. See the fix report for the RED evidence.)
			name:   "a keyed zone told by an unsigned message",
			mutate: func(z *store.Zone) { z.TSIGKeyID = 1 },
			qtype:  dns.TypeSOA,
			peer:   "10.0.0.1", tsigErr: dnssrv.ErrTSIGUnsigned,
			wantRcode: dns.RcodeRefused,
		},
		{
			// The other half of the same rule: a zone naming no key accepts
			// an unsigned NOTIFY. Without this, gating the whole TSIG block
			// on TSIGKeyID would look correct.
			name:  "a keyless zone accepts an unsigned message",
			qtype: dns.TypeSOA,
			peer:  "10.0.0.1", tsigErr: dnssrv.ErrTSIGUnsigned,
			wantRcode: dns.RcodeSuccess, wantRefresh: true,
		},
		{
			// A broken MAC is refused even though the zone names no key.
			// RFC 8945 §5.2.2 makes BADSIG mandatory regardless of local
			// policy, and accepting this would answer an unsigned NOERROR
			// the peer must discard, making it retransmit forever. This is
			// Finding 1: the dns.Err* arms must not be gated on
			// z.TSIGKeyID != 0, or exactly this case is wrongly accepted.
			name:  "a broken MAC to a keyless zone is still refused",
			qtype: dns.TypeSOA,
			peer:  "10.0.0.1", key: "k.", tsigErr: dns.ErrSig,
			wantRcode: dns.RcodeRefused, wantTSIG: dns.RcodeBadSig,
		},
		{
			name:   "an unknown key",
			mutate: func(z *store.Zone) { z.TSIGKeyID = 1 },
			qtype:  dns.TypeSOA,
			peer:   "10.0.0.1", key: "other.", tsigErr: dns.ErrSecret,
			wantRcode: dns.RcodeRefused, wantTSIG: dns.RcodeBadKey,
		},
		{
			name:   "a bad MAC",
			mutate: func(z *store.Zone) { z.TSIGKeyID = 1 },
			qtype:  dns.TypeSOA,
			peer:   "10.0.0.1", key: "k.", tsigErr: dns.ErrSig,
			wantRcode: dns.RcodeRefused, wantTSIG: dns.RcodeBadSig,
		},
		{
			name:   "a clock outside the fudge window",
			mutate: func(z *store.Zone) { z.TSIGKeyID = 1 },
			qtype:  dns.TypeSOA,
			peer:   "10.0.0.1", key: "k.", tsigErr: dns.ErrTime,
			wantRcode: dns.RcodeRefused, wantTSIG: dns.RcodeBadTime,
		},
		{
			// The row the rest of the table leaves out: a store that could
			// not answer the TSIG lookup at all. Not the peer's fault and
			// not a TSIG error, so SERVFAIL rather than blaming the key --
			// mirrors D3's TestAXFRWhenTheKeyStoreFailsIsServfailAndNeverATSIGError.
			// key is "" because that is what Server.RequireTSIG actually
			// returns alongside any non-nil error (server.go/tsig.go), never
			// a name paired with a failure.
			name:   "TSIG lookup failed because the store failed",
			mutate: func(z *store.Zone) { z.TSIGKeyID = 1 },
			qtype:  dns.TypeSOA,
			peer:   "10.0.0.1", key: "", tsigErr: errors.New("tsig key store is down"),
			wantRcode: dns.RcodeServerFailure,
		},
		{
			// A server with no TSIG subsystem at all (dnssrv.ErrTSIGUnavailable)
			// means nothing verified the message -- this server's fault, not
			// the peer's. Groups with the store-failure row above, not with
			// the plainly-unsigned row above it: TransferServer.decide
			// (transferserver.go) applies the identical rule to the identical
			// error value, and a second rule for one error value in one
			// codebase would be worse than either rule alone. REFUSED would
			// tell a correctly configured peer we decline it, when in fact we
			// were unable -- sending the operator to the wrong end of the
			// connection to debug a problem that is on this one.
			name:   "the server has no TSIG subsystem at all",
			mutate: func(z *store.Zone) { z.TSIGKeyID = 1 },
			qtype:  dns.TypeSOA,
			peer:   "10.0.0.1", key: "", tsigErr: dnssrv.ErrTSIGUnavailable,
			wantRcode: dns.RcodeServerFailure,
		},
		{
			// The last row of the table: "signed iff the request verified".
			// A verified request (tsigErr: nil) carrying a real key name is
			// accepted, and the reply must itself be signed -- RFC 8945 §5.3,
			// "When a server has generated a response to a signed request,
			// it signs the response using the same algorithm and key."
			name:   "a verified, signed notify is accepted and the reply is signed",
			mutate: func(z *store.Zone) { z.TSIGKeyID = 1 },
			qtype:  dns.TypeSOA,
			peer:   "10.0.0.1", key: "k.", tsigErr: nil,
			wantRcode: dns.RcodeSuccess, wantRefresh: true, wantSigned: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			z := notifyZone()
			if tc.mutate != nil {
				tc.mutate(&z)
			}
			f := newNotifyFixture(t, z)
			reply := f.notify(t, notifyApex, tc.qtype, tc.peer, tc.key, tc.tsigErr)

			if reply.Rcode != tc.wantRcode {
				t.Errorf("rcode = %s, want %s",
					dns.RcodeToString[reply.Rcode], dns.RcodeToString[tc.wantRcode])
			}
			if got := tsigErrorOf(reply); got != tc.wantTSIG {
				t.Errorf("TSIG error = %d, want %d", got, tc.wantTSIG)
			}
			if reply.Rcode == dns.RcodeSuccess && !reply.Authoritative {
				t.Error("an accepted NOTIFY reply must set AA")
			}
			if len(reply.Question) != 1 || reply.Question[0].Name != dns.Fqdn(notifyApex) {
				t.Errorf("question not echoed: %v", reply.Question)
			}
			if tc.wantSigned && reply.IsTsig() == nil {
				t.Error("the reply should have been signed, but carries no TSIG record")
			}

			// The handoff is asynchronous, so give it a bounded moment to
			// land rather than sleeping a fixed interval.
			deadline := time.Now().Add(2 * time.Second)
			for f.rf.count() == 0 && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if got := f.rf.count() > 0; got != tc.wantRefresh {
				t.Errorf("refresh triggered = %v, want %v", got, tc.wantRefresh)
			}
		})
	}
}

// An apex this server does not hold at all — a separate fixture, since the
// zone simply is not there.
func TestNotifyForAnUnheldZoneIsNotAuth(t *testing.T) {
	f := newNotifyFixture(t, store.Zone{})
	reply := f.notify(t, "nowhere.example", dns.TypeSOA, "10.0.0.1", "", nil)
	if reply.Rcode != dns.RcodeNotAuth {
		t.Errorf("rcode = %s, want NOTAUTH", dns.RcodeToString[reply.Rcode])
	}
}

// The one test in this file that does not go through the notify() helper,
// and it says so rather than implying it covers the wire: a real listener
// never delivers a NOTIFY with zero questions, because miekg's accept
// function rejects any header whose QDCOUNT is not 1 before
// dnssrv.Server.serve runs (the same fact TestTheGateAnswersFormerrWhenAQueryIsNotOneQuestion
// documents for the transfer gate). What is left here is a guard on an
// exported method: ServeNotify is part of dnssrv.Notifies, anything may call
// it, and decideZone's first check reads Question[0] once past the length
// test.
func TestNotifyWithNoQuestionAtAllIsFormerr(t *testing.T) {
	f := newNotifyFixture(t, notifyZone())
	m := new(dns.Msg)
	m.Id = dns.Id()
	w := &captureWriter{remote: &net.UDPAddr{IP: net.ParseIP("10.0.0.1"), Port: 40000}}
	f.ns.ServeNotify(context.Background(), w, m, "", nil)
	if w.msg == nil {
		t.Fatal("nothing was written; a refusal is still a reply")
	}
	if w.msg.Rcode != dns.RcodeFormatError {
		t.Errorf("rcode = %s, want FORMERR", dns.RcodeToString[w.msg.Rcode])
	}
}

// RFC 1996 §4.7 has the responder answer before it acts, and §3.6 has the
// sender retransmitting until it does — so a reply that waited for a
// transfer would earn a second NOTIFY for the transfer already running.
func TestNotifyRepliesBeforeTransferring(t *testing.T) {
	f := newNotifyFixture(t, notifyZone())
	f.rf.gate = make(chan struct{})

	m := new(dns.Msg).SetNotify(dns.Fqdn(notifyApex))
	w := &captureWriter{remote: &net.UDPAddr{IP: net.ParseIP("10.0.0.1"), Port: 40000}}

	done := make(chan struct{})
	go func() {
		f.ns.ServeNotify(context.Background(), w, m, "", nil)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ServeNotify blocked on the transfer instead of replying first")
	}
	if w.msg == nil || w.msg.Rcode != dns.RcodeSuccess {
		t.Fatalf("reply = %v, want NOERROR written before the transfer", w.msg)
	}
	if f.rf.count() != 0 {
		t.Error("the transfer ran before the reply went out")
	}
	close(f.rf.gate)
}

// The handoff must not inherit the request's context: dnssrv cancels it when
// serve returns, and a transfer started under it would be cut off mid-zone.
func TestNotifyWorkOutlivesTheRequestContext(t *testing.T) {
	f := newNotifyFixture(t, notifyZone())

	ctx, cancel := context.WithCancel(context.Background())
	m := new(dns.Msg).SetNotify(dns.Fqdn(notifyApex))
	w := &captureWriter{remote: &net.UDPAddr{IP: net.ParseIP("10.0.0.1"), Port: 40000}}
	f.ns.ServeNotify(ctx, w, m, "", nil)
	// Exactly what dnssrv does the instant ServeNotify returns.
	cancel()

	deadline := time.Now().Add(2 * time.Second)
	for f.rf.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if f.rf.count() == 0 {
		t.Fatal("the transfer never ran")
	}
	f.rf.mu.Lock()
	alive := f.rf.ctxAlive
	f.rf.mu.Unlock()
	if !alive {
		t.Error("the transfer inherited the cancelled request context")
	}
}

// A flood of NOTIFYs is the ordinary case — a primary editing ten records
// sends ten — and each gets its own immediate NOERROR while they collapse to
// one probe. Without this, "NOTIFY is cheap" is a probe amplifier pointed at
// our own primary.
//
// The window closing is part of the same property and is asserted here rather
// than left to the trailing-edge test below: the collapse is to one probe *per
// window*, so a throttle that never reopened would satisfy the first half of
// this test and starve the zone.
func TestNotifyThrottlesTheWorkNotTheReply(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC))
	f := newNotifyFixture(t, notifyZone(), zones.WithNotifyServerNow(clock.now))

	const sent = 5
	for i := 0; i < sent; i++ {
		reply := f.notify(t, notifyApex, dns.TypeSOA, "10.0.0.1", "", nil)
		if reply.Rcode != dns.RcodeSuccess {
			t.Fatalf("notify %d: rcode = %s, want NOERROR — the throttle must "+
				"never suppress the reply", i, dns.RcodeToString[reply.Rcode])
		}
	}

	deadline := time.Now().Add(2 * time.Second)
	for f.rf.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	// Let any wrongly-admitted extra work land before counting, or this
	// passes by winning a race rather than by throttling.
	time.Sleep(50 * time.Millisecond)
	if got := f.rf.count(); got != 1 {
		t.Errorf("%d NOTIFYs inside the window caused %d refreshes, want 1", sent, got)
	}

	// The window closes, and the suppressed NOTIFYs are worth exactly one more
	// round between them — never one per packet.
	clock.advance(6 * time.Second)
	deadline = time.Now().Add(2 * time.Second)
	for f.rf.count() < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	if got := f.rf.count(); got != 2 {
		t.Errorf("%d NOTIFYs inside one window caused %d refreshes in total, want 2 "+
			"(one inside the window and one when it closed)", sent, got)
	}
}

// The defect the trailing edge exists for. A primary edits at t=0 (serial N)
// and again at t=2s (N+1). The first NOTIFY is admitted, probes, and finds
// nothing new — we were already at N. The second lands inside the window; a
// throttle that *drops* it loses N+1 until the SOA refresh fires, which for a
// default soa_refresh is fifteen minutes of serving records the primary has
// already replaced, with the peer told NOERROR so it stops retransmitting
// (RFC 1996 §3.6).
//
// So the window remembers a suppressed NOTIFY and runs one more probe when it
// closes: two probes and one transfer, not one probe and a stale zone.
func TestNotifyDefersASuppressedNotifyToTheEndOfTheWindow(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC))
	// notifyZone() is at serial 10, so a primary also at 10 is "already
	// current" and the first NOTIFY must transfer nothing.
	probe := &fakeProbe{serial: 10}
	f := newNotifyFixture(t, notifyZone(),
		zones.WithNotifyServerNow(clock.now), zones.WithNotifyProbes(probe))

	f.notify(t, notifyApex, dns.TypeSOA, "10.0.0.1", "", nil)
	deadline := time.Now().Add(2 * time.Second)
	for probe.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := probe.count(); got != 1 {
		t.Fatalf("the first NOTIFY caused %d probe(s), want 1", got)
	}
	if got := f.rf.count(); got != 0 {
		t.Fatalf("a NOTIFY for a serial we already hold caused %d transfer(s), want 0", got)
	}

	// The primary edits again and notifies, two seconds into the window.
	clock.advance(2 * time.Second)
	probe.setSerial(11)
	f.notify(t, notifyApex, dns.TypeSOA, "10.0.0.1", "", nil)
	time.Sleep(50 * time.Millisecond)
	if got := probe.count(); got != 1 {
		t.Fatalf("a NOTIFY inside the window caused %d probe(s) in total, want 1 — "+
			"the throttle must still collapse the work", got)
	}

	// The window closes. Nothing else arrives: the deferred NOTIFY is the only
	// thing that can make the newer serial appear.
	clock.advance(4 * time.Second)
	deadline = time.Now().Add(3 * time.Second)
	for f.rf.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := f.rf.count(); got != 1 {
		t.Fatalf("the serial bump carried by a throttled NOTIFY caused %d transfer(s), want 1 — "+
			"it was dropped rather than deferred", got)
	}
	if got := probe.count(); got != 2 {
		t.Errorf("probes = %d, want 2: one per window, and the window that closed owes one", got)
	}
}

// The throttle must be keyed by zone, not global — a NOTIFY on zone A that
// admits a refresh must not throttle zone B's, or one primary serving two
// zones would starve every zone but whichever happened to be notified first
// inside the window. Mirrors D3's TestTheThrottleIsPerZone.
func TestNotifyThrottleIsPerZone(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, "sqlite", t.TempDir()+"/t.db")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("store.Close: %v", err)
		}
	})

	zoneA := notifyZone()
	zoneA.Name = "a." + notifyApex
	zoneB := notifyZone()
	zoneB.Name = "b." + notifyApex
	for _, z := range []*store.Zone{&zoneA, &zoneB} {
		id, err := st.Zones().AddZone(ctx, *z)
		if err != nil {
			t.Fatalf("AddZone(%s): %v", z.Name, err)
		}
		z.ID = id
	}

	res := zones.NewResolver(st.Zones())
	if err := res.Reload(ctx); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	rf := &fakeRefresher{}
	clock := newFakeClock(time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC))
	ns := zones.NewNotifyServer(res, st.Zones(), rf, zones.WithNotifyServerNow(clock.now))

	for _, apex := range []string{zoneA.Name, zoneB.Name} {
		m := new(dns.Msg).SetNotify(dns.Fqdn(apex))
		w := &captureWriter{remote: &net.UDPAddr{IP: net.ParseIP("10.0.0.1"), Port: 40000}}
		ns.ServeNotify(ctx, w, m, "", nil)
		if w.msg == nil || w.msg.Rcode != dns.RcodeSuccess {
			t.Fatalf("notify for %s: reply = %v, want NOERROR", apex, w.msg)
		}
	}

	deadline := time.Now().Add(2 * time.Second)
	for rf.count() < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := rf.count(); got != 2 {
		t.Errorf("a refresh admitted for zone A throttled zone B: got %d refresh(es), want 2", got)
	}
}

// The throttle window must actually expire, or a zone notified once would
// never be transferred again. Setting the window absurdly wide (a day, say)
// would pass every other throttle test in this file; only moving the clock
// past it and observing a second admitted refresh catches that.
func TestNotifyThrottleWindowExpires(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC))
	f := newNotifyFixture(t, notifyZone(), zones.WithNotifyServerNow(clock.now))

	reply := f.notify(t, notifyApex, dns.TypeSOA, "10.0.0.1", "", nil)
	if reply.Rcode != dns.RcodeSuccess {
		t.Fatalf("first notify: rcode = %s, want NOERROR", dns.RcodeToString[reply.Rcode])
	}
	deadline := time.Now().Add(2 * time.Second)
	for f.rf.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := f.rf.count(); got != 1 {
		t.Fatalf("first notify triggered %d refresh(es), want 1", got)
	}

	// Past notifyThrottle (5s, notifyserver.go), with a margin.
	clock.advance(6 * time.Second)
	reply = f.notify(t, notifyApex, dns.TypeSOA, "10.0.0.1", "", nil)
	if reply.Rcode != dns.RcodeSuccess {
		t.Fatalf("second notify: rcode = %s, want NOERROR", dns.RcodeToString[reply.Rcode])
	}
	deadline = time.Now().Add(2 * time.Second)
	for f.rf.count() < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := f.rf.count(); got != 2 {
		t.Errorf("a notify past the throttle window caused %d refresh(es) in total, want 2", got)
	}
}

// countingDialer stands in for the network a hostname primary would be
// resolved across. Wiring it into a *net.Resolver via PreferGo+Dial (the
// standard way to stub Go's own resolver in a test, since net.Resolver is a
// concrete struct rather than an interface) makes every underlying
// connection attempt LookupNetIP would make visible without touching a real
// network — a held count of zero means ParsePrimaries was never reached at
// all, not merely that it failed quietly.
type countingDialer struct {
	calls atomic.Int64
}

func (d *countingDialer) Dial(_ context.Context, _, _ string) (net.Conn, error) {
	d.calls.Add(1)
	return nil, errors.New("dial refused: test stub")
}

// Finding 3: ParsePrimaries must not run until decideZone has already
// accepted the NOTIFY. Resolving first would let one unauthenticated,
// trivially source-spoofable UDP packet for a zone this server does not
// hold, or holds disabled, drive a live outbound recursive DNS lookup,
// bounded only by dnssrv.NotifyTimeout and throttled by nothing.
func TestNotifyResolvesPrimariesOnlyAfterTheZoneChecksPass(t *testing.T) {
	newStubResolver := func() (*countingDialer, *net.Resolver) {
		d := &countingDialer{}
		return d, &net.Resolver{PreferGo: true, Dial: d.Dial}
	}

	t.Run("a zone this server does not hold", func(t *testing.T) {
		dialer, res := newStubResolver()
		z := notifyZone()
		z.Primaries = "primary.invalid" // a hostname: resolving it means a lookup
		f := newNotifyFixture(t, z, zones.WithNotifyServerResolver(res))

		reply := f.notify(t, "nowhere.example", dns.TypeSOA, "10.0.0.1", "", nil)
		if reply.Rcode != dns.RcodeNotAuth {
			t.Fatalf("rcode = %s, want NOTAUTH", dns.RcodeToString[reply.Rcode])
		}
		if got := dialer.calls.Load(); got != 0 {
			t.Errorf("a NOTIFY for an unheld zone made %d resolver call(s), want 0", got)
		}
	})

	t.Run("a disabled zone", func(t *testing.T) {
		dialer, res := newStubResolver()
		z := notifyZone()
		z.Primaries = "primary.invalid"
		z.Enabled = false
		f := newNotifyFixture(t, z, zones.WithNotifyServerResolver(res))

		reply := f.notify(t, notifyApex, dns.TypeSOA, "10.0.0.1", "", nil)
		if reply.Rcode != dns.RcodeNotAuth {
			t.Fatalf("rcode = %s, want NOTAUTH", dns.RcodeToString[reply.Rcode])
		}
		if got := dialer.calls.Load(); got != 0 {
			t.Errorf("a NOTIFY for a disabled zone made %d resolver call(s), want 0", got)
		}
	})

	t.Run("a zone that passes decideZone does resolve", func(t *testing.T) {
		// The positive control: proves the dialer actually fires once
		// ParsePrimaries is reached, so the zero counts above mean "never
		// called" rather than "this stub never fires at all".
		dialer, res := newStubResolver()
		z := notifyZone()
		z.Primaries = "primary.invalid"
		f := newNotifyFixture(t, z, zones.WithNotifyServerResolver(res))

		f.notify(t, notifyApex, dns.TypeSOA, "10.0.0.1", "", nil)
		if got := dialer.calls.Load(); got == 0 {
			t.Error("a NOTIFY that passed decideZone never resolved its primaries — the stub is not wired up")
		}
	})
}

// Ordering the gate's checks removed the lookup for a NOTIFY that was going to
// be refused anyway; it left it in place for every NOTIFY that reaches a zone
// with a hostname primary, which is the ordinary configuration a mixed
// `primaries` list produces. A source that is one of the zone's IP literals is
// already answerable from the column, so it must cost no lookup at all.
func TestNotifyMatchesALiteralPrimaryWithoutResolvingAHostnameBesideIt(t *testing.T) {
	dialer := &countingDialer{}
	z := notifyZone()
	z.Primaries = "10.0.0.1, ns1.primary.invalid"
	f := newNotifyFixture(t, z,
		zones.WithNotifyServerResolver(&net.Resolver{PreferGo: true, Dial: dialer.Dial}))

	reply := f.notify(t, notifyApex, dns.TypeSOA, "10.0.0.1", "", nil)
	if reply.Rcode != dns.RcodeSuccess {
		t.Fatalf("rcode = %s, want NOERROR: the literal primary is the source", dns.RcodeToString[reply.Rcode])
	}
	if got := dialer.calls.Load(); got != 0 {
		t.Errorf("a NOTIFY from a literal primary made %d resolver call(s), want 0", got)
	}
}

// The other half: a zone whose primaries are only hostnames does have to
// resolve, and a source that matches none of them is exactly the flood this
// bound exists for — one trivially spoofable UDP packet per outbound recursive
// lookup, throttled by nothing. The resolution is reused for the same window
// the work throttle uses, so a flood costs one lookup rather than one each.
func TestNotifyResolvesAHostnamePrimaryOncePerWindow(t *testing.T) {
	dialer := &countingDialer{}
	clock := newFakeClock(time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC))
	z := notifyZone()
	z.Primaries = "ns1.primary.invalid"
	f := newNotifyFixture(t, z,
		zones.WithNotifyServerResolver(&net.Resolver{PreferGo: true, Dial: dialer.Dial}),
		zones.WithNotifyServerNow(clock.now))

	reply := f.notify(t, notifyApex, dns.TypeSOA, "10.9.9.9", "", nil)
	if reply.Rcode != dns.RcodeRefused {
		t.Fatalf("rcode = %s, want REFUSED: the source is not a primary", dns.RcodeToString[reply.Rcode])
	}
	// One lookup is several dials (A and AAAA, and a retry apiece), so the
	// count of the first is the baseline rather than a number to write down.
	first := dialer.calls.Load()
	if first == 0 {
		t.Fatal("the first NOTIFY never resolved the hostname primary — the stub is not wired up")
	}

	for range 4 {
		f.notify(t, notifyApex, dns.TypeSOA, "10.9.9.9", "", nil)
	}
	if got := dialer.calls.Load(); got != first {
		t.Errorf("four more NOTIFYs inside one window made %d resolver call(s) in total, want the first lookup's %d", got, first)
	}

	// And the reuse expires with the window, or a primary that moved would
	// never be matched again.
	clock.advance(6 * time.Second)
	f.notify(t, notifyApex, dns.TypeSOA, "10.9.9.9", "", nil)
	if got := dialer.calls.Load(); got == first {
		t.Error("a NOTIFY past the window reused the previous resolution: it never expires")
	}
}

// The work a NOTIFY starts outlives the request (the test above), which used
// to mean it outlived everything: the goroutine was `go func()` with nothing
// holding a reference to it, so a NOTIFY admitted moments before shutdown
// could still be probing a primary, or transferring a zone into the store,
// while App.Shutdown closed that store underneath it.
//
// Run is the lifetime. It is the shape every other long-lived worker in App
// has — a func(context.Context) in a.bg, waited on by App.wg — and it does
// two things when its context ends: stops the server admitting new work, and
// waits for the work already admitted to unwind.
//
// The three assertions are the three halves of that (the middle one is what
// makes the first meaningful):
//
//   - Run does not return while admitted work is in flight;
//   - it does return once that work finishes;
//   - and after it has returned, a NOTIFY that still gets a reply starts no
//     goroutine at all — not even one that is going to give up, because
//     that one would be outside the WaitGroup Wait has already returned
//     from, and it still reaches the store on its way to giving up.
//
// The last assertion is about the store read, not about the transfer.
// Cancelling the work context alone stops the transfer, so a server that
// only cancelled would pass an assertion phrased as "no refresh ran" while
// still spawning an untracked goroutine that reads a store which is about to
// close. The log line that read writes on failure is what tells the two
// apart.
func TestNotifyServerRunWaitsForTheWorkItAdmitted(t *testing.T) {
	logs := captureLogs(t)
	clock := newFakeClock(time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC))
	f := newNotifyFixture(t, notifyZone(), zones.WithNotifyServerNow(clock.now))
	f.rf.entered = make(chan struct{}, 4)
	f.rf.gate = make(chan struct{})

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() { defer close(runDone); f.ns.Run(ctx) }()

	if rc := f.notify(t, notifyApex, dns.TypeSOA, "10.0.0.1", "", nil).Rcode; rc != dns.RcodeSuccess {
		t.Fatalf("rcode = %s, want NOERROR", dns.RcodeToString[rc])
	}
	select {
	case <-f.rf.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the work the NOTIFY admitted never started")
	}

	// Shutdown, with the transfer still running.
	cancel()
	select {
	case <-runDone:
		t.Fatal("Run returned while the work it admitted was still in flight — " +
			"App.wg would have released and the store would close underneath it")
	case <-time.After(100 * time.Millisecond):
	}

	close(f.rf.gate)
	select {
	case <-runDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Run never returned after the work it was waiting for finished")
	}
	if f.rf.count() != 1 {
		t.Fatalf("Refresh ran %d times, want 1", f.rf.count())
	}

	// A NOTIFY after the lifetime has ended. The clock moves past the
	// throttle so that a server which *would* have worked is not excused by
	// admit() instead.
	clock.advance(time.Minute)
	if rc := f.notify(t, notifyApex, dns.TypeSOA, "10.0.0.1", "", nil).Rcode; rc != dns.RcodeSuccess {
		t.Fatalf("rcode after shutdown = %s, want NOERROR", dns.RcodeToString[rc])
	}
	select {
	case <-f.rf.entered:
		t.Fatal("a NOTIFY admitted after Run returned started work nothing is waiting for")
	case <-time.After(200 * time.Millisecond):
	}
	for _, r := range logs() {
		if r.Message == "reading a zone after a notify failed" {
			t.Fatal("a NOTIFY admitted after Run returned still spawned a goroutine that read the store — " +
				"it is outside the WaitGroup Shutdown already waited on")
		}
	}
}

// A primary behind NAT reaches its secondary from an address that is not in
// `primaries` — the NAT's, not its own — so the source check refuses a NOTIFY
// it has no other way to send. A TSIG that verifies under *this zone's own
// key* is a stronger statement about who sent it than a source address is, so
// it stands in for the address rather than being checked after it.
//
// Only the zone's own key. A message signed under some other key this server
// holds says nothing about being this zone's master, and an unsigned or
// wrongly signed one from an unlisted source is refused exactly as before.
func TestNotifyFromAnUnlistedSourceIsAcceptedWhenItVerifiesUnderTheZoneKey(t *testing.T) {
	const (
		zoneKey  = "primary-nat."
		otherKey = "someone-else."
	)

	tests := []struct {
		name        string
		peer        string
		key         string
		tsigErr     error
		wantRcode   int
		wantTSIG    uint16
		wantRefresh bool
	}{
		{
			name: "signed under the zone's key from an address nobody listed",
			peer: "203.0.113.77", key: zoneKey,
			wantRcode: dns.RcodeSuccess, wantRefresh: true,
		},
		{
			// The rule is not "signed by anything we hold": a key that
			// exists is not a claim to be this zone's master.
			name: "signed under another zone's key from an unlisted address",
			peer: "203.0.113.77", key: otherKey,
			wantRcode: dns.RcodeRefused,
		},
		{
			name: "unsigned from an unlisted address",
			peer: "203.0.113.77", tsigErr: dnssrv.ErrTSIGUnsigned,
			wantRcode: dns.RcodeRefused,
		},
		{
			// A TSIG that did not verify is a TSIG error wherever it came
			// from, and it is still not a way past the source check.
			name: "a broken MAC from an unlisted address",
			peer: "203.0.113.77", key: zoneKey, tsigErr: dns.ErrSig,
			wantRcode: dns.RcodeRefused,
		},
		{
			// The listed source still needs no signature at all — this
			// widens the gate, it does not tighten it.
			name: "the listed primary, signed under the zone's key",
			peer: "10.0.0.1", key: zoneKey,
			wantRcode: dns.RcodeSuccess, wantRefresh: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			z := notifyZone()
			z.TSIGKeyID = 1
			f := newNotifyFixture(t, z)
			// Ids start at 1 in a store this test just opened, so the first
			// key created is the one the zone names — asserted rather than
			// assumed.
			id := addTSIGKey(t, f.st, zoneKey)
			if id != z.TSIGKeyID {
				t.Fatalf("the zone's key got id %d, want %d", id, z.TSIGKeyID)
			}
			addTSIGKey(t, f.st, otherKey)

			reply := f.notify(t, notifyApex, dns.TypeSOA, tc.peer, tc.key, tc.tsigErr)
			if reply.Rcode != tc.wantRcode {
				t.Errorf("rcode = %s, want %s",
					dns.RcodeToString[reply.Rcode], dns.RcodeToString[tc.wantRcode])
			}
			if got := tsigErrorOf(reply); got != tc.wantTSIG {
				t.Errorf("TSIG error = %d, want %d", got, tc.wantTSIG)
			}

			deadline := time.Now().Add(2 * time.Second)
			for f.rf.count() == 0 && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if got := f.rf.count() > 0; got != tc.wantRefresh {
				t.Errorf("refresh triggered = %v, want %v", got, tc.wantRefresh)
			}
		})
	}
}

func addTSIGKey(t *testing.T, st store.Store, name string) int64 {
	t.Helper()
	id, err := st.TSIGKeys().Create(context.Background(), store.TSIGKey{
		Name: name, Algorithm: dns.HmacSHA256, Secret: "c2VjcmV0LXNlY3JldC1zZWNyZXQ=",
	})
	if err != nil {
		t.Fatalf("creating TSIG key %q: %v", name, err)
	}
	return id
}
