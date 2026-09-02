package zones_test

import (
	"context"
	"errors"
	"net"
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
}

func (f *fakeRefresher) Refresh(ctx context.Context, zoneID int64) (zones.TransferResult, error) {
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
			name:      "a primary zone is not told by anyone",
			mutate:    func(z *store.Zone) { z.Type = "primary"; z.Primaries = "" },
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
func TestNotifyThrottlesTheWorkNotTheReply(t *testing.T) {
	clock := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	f := newNotifyFixture(t, notifyZone(),
		zones.WithNotifyServerNow(func() time.Time { return clock }))

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
	clock := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	ns := zones.NewNotifyServer(res, st.Zones(), rf, zones.WithNotifyServerNow(func() time.Time { return clock }))

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
	clock := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	f := newNotifyFixture(t, notifyZone(),
		zones.WithNotifyServerNow(func() time.Time { return clock }))

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
	clock = clock.Add(6 * time.Second)
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
