package zones_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aloks98/dnsaur/internal/store"
	"github.com/aloks98/dnsaur/internal/zones"
	"github.com/miekg/dns"
)

// The scheduler's tests never sleep. Every one of them drives the fixture's
// clock and then asks the scheduler to make a pass, so what is asserted is
// what it *decided* — whether a transfer happened at that instant, and how
// many times the primary was actually asked.
//
// The primary is the real one from transfer_test.go, so a pass that decides a
// zone is due goes all the way out over TCP and back. That is what makes the
// request count a trustworthy assertion: it counts AXFRs that arrived at
// another server, not calls to a stub.

const (
	// primaryRetry is the retry in the SOA primaryZoneRRs serves, in seconds.
	// It differs from primaryRefresh (900) precisely so a test can tell which
	// of the two the scheduler used.
	primaryRetry = 300
	// scheduleFloor mirrors the unexported minInterval in refresh.go: no
	// zone is polled faster than this whatever its SOA asks for.
	scheduleFloor = 60 * time.Second
)

// refresher builds a scheduler over the fixture's store and clock, with both
// workers wired — the Transferrer a secondary is pulled with and the
// StubFetcher a stub is, which is the shape App.New builds.
//
// The jitter is pinned to the far end of the spread rather than left random:
// a test that only passes because the random delay happened to be small is
// not testing anything, and this way any spread applied where none was meant
// is the difference between "transferred" and "did not".
func (f *transferFixture) refresher(opts ...zones.RefreshOption) *zones.Refresher {
	opts = append([]zones.RefreshOption{
		zones.WithRefreshNow(func() time.Time { return f.now }),
		zones.WithJitter(func(d time.Duration) time.Duration { return d }),
		zones.WithStubFetcher(f.stubFetcher()),
	}, opts...)
	return zones.NewRefresher(f.st.Zones(), f.transferrer(), opts...)
}

// advance moves the fixture's clock. Safe between passes: RefreshDue joins
// every worker it starts before returning.
func (f *transferFixture) advance(d time.Duration) { f.now = f.now.Add(d) }

func (f *transferFixture) refreshDue(t *testing.T, r *zones.Refresher) {
	t.Helper()
	if err := r.RefreshDue(context.Background()); err != nil {
		t.Fatalf("RefreshDue: %v", err)
	}
}

func (f *transferFixture) records(t *testing.T) []store.ZoneRecord {
	t.Helper()
	recs, err := f.st.Zones().Records(context.Background(), f.zoneID)
	if err != nil {
		t.Fatalf("Records: %v", err)
	}
	return recs
}

func (f *transferFixture) updateZone(t *testing.T, mutate func(z *store.Zone)) {
	t.Helper()
	z := f.zone(t)
	mutate(&z)
	if err := f.st.Zones().UpdateZone(context.Background(), z); err != nil {
		t.Fatalf("UpdateZone: %v", err)
	}
	if err := f.resolver.Reload(context.Background()); err != nil {
		t.Fatalf("Reload: %v", err)
	}
}

// A secondary that has never transferred is answering SERVFAIL to everything
// it is asked. It is the one state with nothing to lose by transferring at
// once, and the scheduler must not make it wait for anything.
func TestSchedulerTransfersAZoneThatHasNeverTransferred(t *testing.T) {
	primary := startTestPrimary(t, transferApex, primaryZoneRRs(t))
	f := newTransferFixture(t, primary.addr, 0)
	ref := f.refresher()

	f.refreshDue(t, ref)

	if got := primary.requests(); got != 1 {
		t.Fatalf("the primary was asked %d times, want 1", got)
	}
	if z := f.zone(t); z.RefreshedAt != f.now.UnixMilli() {
		t.Errorf("refreshed_at = %d, want %d", z.RefreshedAt, f.now.UnixMilli())
	}
	if m := f.ask(t, "bifrost."+transferApex, dns.TypeA); m.Rcode != dns.RcodeSuccess {
		t.Errorf("after the scheduled transfer: rcode = %s, want NOERROR", dns.RcodeToString[m.Rcode])
	}
	// Nothing is pending: the next attempt is the SOA's business, which the
	// zone row already answers.
	st, ok := ref.Status(f.zoneID)
	if !ok {
		t.Fatalf("the scheduler kept no state for the zone it transferred")
	}
	if st.Failures != 0 || !st.NotBefore.IsZero() {
		t.Errorf("status = %+v, want no failures and no back-off after a success", st)
	}
}

// The schedule between successful transfers is the SOA's refresh, to the
// second — not a fixed interval of the scheduler's own.
func TestSchedulerWaitsOutTheSOARefresh(t *testing.T) {
	primary := startTestPrimary(t, transferApex, primaryZoneRRs(t))
	f := newTransferFixture(t, primary.addr, 0)
	ref := f.refresher()

	f.refreshDue(t, ref)
	firstRefreshedAt := f.zone(t).RefreshedAt

	f.advance(primaryRefresh*time.Second - time.Second)
	f.refreshDue(t, ref)
	if got := primary.requests(); got != 1 {
		t.Fatalf("one second before the refresh elapsed the primary was asked %d times, want 1", got)
	}
	if z := f.zone(t); z.RefreshedAt != firstRefreshedAt {
		t.Errorf("refreshed_at moved without a transfer")
	}

	f.advance(time.Second)
	f.refreshDue(t, ref)
	if got := primary.requests(); got != 2 {
		t.Fatalf("once the refresh elapsed the primary was asked %d times, want 2", got)
	}
	if z := f.zone(t); z.RefreshedAt != f.now.UnixMilli() {
		t.Errorf("refreshed_at = %d after the second transfer, want %d", z.RefreshedAt, f.now.UnixMilli())
	}
}

// RFC 1034 §4.3.5's refresh timer is "check to see if the zone has been
// updated" — an SOA query, and a transfer only if the serial says so. The
// schedule used to transfer unconditionally, which on a default soa_refresh
// meant a whole zone on the wire, a record diff, a whole-store snapshot
// rebuild and a NOTIFY pass every fifteen minutes, per secondary, for a zone
// nobody had touched.
//
// The primary here is a dnsaur one (newXFRFixture with the resolving
// pipeline), because a probe is an ordinary SOA query and a fake that only
// speaks AXFR cannot answer one. It loses a record *without* moving its
// serial, which is what makes "did it transfer" observable rather than
// inferred: a refresh that pulled the zone would carry the loss across.
func TestAScheduledRefreshOfAnUnchangedZoneDoesNotTransfer(t *testing.T) {
	ctx := context.Background()
	primary := newXFRFixture(t, loopbackPrimaryZone("127.0.0.0/8"), loopbackRecords(), withResolvingPipeline())
	f := newTransferFixture(t, primary.addr, 0)
	ref := f.refresher()

	// The first pass: never transferred, so nothing to compare against and no
	// probe at all.
	f.refreshDue(t, ref)
	if z := f.zone(t); z.RefreshedAt != f.now.UnixMilli() {
		t.Fatalf("the first pass did not transfer: refreshed_at = %d", z.RefreshedAt)
	}
	before := f.records(t)
	if len(before) == 0 {
		t.Fatal("the first transfer installed nothing")
	}

	// A record disappears from the primary and the serial does not move.
	dropped := primaryRecord(t, primary, "bifrost")
	if err := primary.st.Zones().DeleteRecord(ctx, dropped.ID); err != nil {
		t.Fatalf("DeleteRecord: %v", err)
	}
	if err := primary.res.Reload(ctx); err != nil {
		t.Fatalf("primary Reload: %v", err)
	}

	f.advance(primaryRefresh * time.Second)
	f.refreshDue(t, ref)

	z := f.zone(t)
	// A successful check restarts both timers: RFC 1034 §4.3.5 does that when
	// the primary *answers*, not only when it answers with something new. A
	// secondary that probed successfully and stamped nothing would expire on
	// schedule with its primary reachable the whole time.
	if z.RefreshedAt != f.now.UnixMilli() {
		t.Errorf("refreshed_at = %d, want %d: a successful check is a refresh", z.RefreshedAt, f.now.UnixMilli())
	}
	if want := f.now.UnixMilli() + int64(z.SOAExpire)*1000; z.ExpiresAt != want {
		t.Errorf("expires_at = %d, want %d: the expire timer restarts on a successful check", z.ExpiresAt, want)
	}
	if got := len(f.records(t)); got != len(before) {
		t.Errorf("the zone holds %d records, want the %d it had: the pass transferred a zone whose serial had not moved",
			got, len(before))
	}
	if primary.probeQueries.Load() == 0 {
		t.Error("the pass never asked the primary for its SOA")
	}
	// And the zone still answers, from the copy it already had.
	if m := f.ask(t, "bifrost."+transferApex, dns.TypeA); m.Rcode != dns.RcodeSuccess {
		t.Errorf("after the check: rcode = %s, want NOERROR", dns.RcodeToString[m.Rcode])
	}
}

// primaryRecord returns one of the primary fixture's records by relative name.
func primaryRecord(t *testing.T, f *xfrFixture, name string) store.ZoneRecord {
	t.Helper()
	recs, err := f.st.Zones().Records(context.Background(), f.zoneID)
	if err != nil {
		t.Fatalf("Records: %v", err)
	}
	for _, r := range recs {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("the primary holds no record named %q", name)
	return store.ZoneRecord{}
}

// A probe that fails must not fail the attempt. The probe is one UDP exchange
// and the transfer is TCP, so a primary — or a middlebox — that answers one
// and not the other is an ordinary misconfiguration, and of the two ways to be
// wrong about it, an AXFR nobody needed costs bandwidth while an attempt
// abandoned over a probe leaves a secondary stale and then expired with its
// primary answering the whole time.
//
// startTestPrimary is exactly that shape: it serves AXFR over TCP and listens
// on no UDP port at all, so every probe against it fails.
func TestAScheduledRefreshTransfersWhenTheProbeFails(t *testing.T) {
	primary := startTestPrimary(t, transferApex, primaryZoneRRs(t))
	f := newTransferFixture(t, primary.addr, 0)
	ref := f.refresher()

	f.refreshDue(t, ref)
	f.advance(primaryRefresh * time.Second)
	f.refreshDue(t, ref)

	if got := primary.requests(); got != 2 {
		t.Errorf("the primary was asked for the zone %d times, want 2: a failed probe must not skip the transfer", got)
	}
	if z := f.zone(t); z.RefreshedAt != f.now.UnixMilli() {
		t.Errorf("refreshed_at = %d, want %d", z.RefreshedAt, f.now.UnixMilli())
	}
	st, ok := ref.Status(f.zoneID)
	if !ok {
		t.Fatal("the scheduler kept no state for the zone")
	}
	if st.Failures != 0 {
		t.Errorf("failures = %d, want 0: the probe failed, the attempt did not", st.Failures)
	}
}

// An SOA whose expire is below its refresh takes the secondary dark on a
// schedule: expires_at lands before the next attempt is even due, so the zone
// SERVFAILs everything in between with nothing wrong anywhere. The schedule
// is the SOA's, but it is not obliged to honour a value that means "stop
// answering and do not check" — so the attempt happens at half the expiry
// instead, and the operator is told once which of their two numbers this
// server is not following.
func TestSchedulerPollsAheadOfAnExpiryBelowTheRefresh(t *testing.T) {
	logs := captureLogs(t)
	const (
		refresh = 3600
		expire  = 600
	)
	soa := mustRR(t, fmt.Sprintf("%s. 900 IN SOA ns1.%s. hostadmin.%s. %d %d 300 %d 900",
		transferApex, transferApex, transferApex, primarySerial, refresh, expire))
	primary := startTestPrimary(t, transferApex, []dns.RR{
		soa,
		mustRR(t, fmt.Sprintf("%s. 3600 IN NS ns1.%s.", transferApex, transferApex)),
		soa,
	})
	f := newTransferFixture(t, primary.addr, 0)
	ref := f.refresher()

	f.refreshDue(t, ref)
	if got := primary.requests(); got != 1 {
		t.Fatalf("the first pass asked %d times, want 1", got)
	}

	// One second short of half the expiry: still nothing due.
	f.advance((expire/2)*time.Second - time.Second)
	f.refreshDue(t, ref)
	if got := primary.requests(); got != 1 {
		t.Fatalf("the primary was asked %d times before half the expiry had passed, want 1", got)
	}

	f.advance(time.Second)
	f.refreshDue(t, ref)
	if got := primary.requests(); got != 2 {
		t.Errorf("the primary was asked %d times at half the expiry, want 2: the zone would have gone dark "+
			"for %ds of every %ds waiting out a refresh longer than its own expiry", got, refresh-expire, refresh)
	}
	if z := f.zone(t); z.ExpiresAt <= f.now.UnixMilli() {
		t.Error("the zone expired before the schedule came back to it")
	}

	warned := 0
	for _, r := range logs() {
		if strings.Contains(r.Message, "expire") && r.Level >= slog.LevelWarn {
			warned++
		}
	}
	if warned != 1 {
		t.Errorf("the SOA's expire-below-refresh was warned about %d times, want exactly 1 per zone", warned)
	}
}

// A manual refresh waits for a transfer already in flight, which is what a
// person who pressed the button meant — but the button is an HTTP request, and
// a request that has gone away must not leave its handler parked on a lock
// behind a primary that has stopped talking. The same wait is what a NOTIFY's
// own work takes, under a context that ends when the server stops.
func TestAManualRefreshStopsWaitingWhenItsCallerDoes(t *testing.T) {
	hold := newOneShotHold()
	defer hold.free()
	primary := startTestPrimary(t, transferApex, primaryZoneRRs(t), withHold(hold.hold))
	f := newTransferFixture(t, primary.addr, 0)
	ref := f.refresher()

	// One transfer, held inside the primary, owning the zone's lock.
	go func() { _, _ = ref.Refresh(context.Background(), f.zoneID) }()
	hold.wait(t, "the primary")

	ctx, cancel := context.WithCancel(context.Background())
	waited := make(chan error, 1)
	go func() { _, err := ref.Refresh(ctx, f.zoneID); waited <- err }()
	// Nothing can make the second refresh proceed: the first still holds the
	// lock. Cancelling is what must end the wait.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-waited:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("the waiting refresh returned %v, want a context cancellation", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a manual refresh whose caller gave up is still waiting for the lock")
	}
	hold.free()
}

// A transfer that runs out of its own time is the primary's failure and has to
// be recorded as one: a zone whose transfers all time out would otherwise show
// a clean last_error and no back-off, and an operator would have nothing to
// read. It is only a shutdown that is not counted — see the test above, which
// is the case this must not swallow.
func TestATransferThatRunsOutOfTimeIsCountedAsAFailure(t *testing.T) {
	hold := newOneShotHold()
	defer hold.free()
	primary := startTestPrimary(t, transferApex, primaryZoneRRs(t), withHold(hold.hold))
	f := newTransferFixture(t, primary.addr, 0)
	ref := f.refresher()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := ref.Refresh(ctx, f.zoneID); err == nil {
		t.Fatal("a transfer that ran past its deadline reported success")
	}
	hold.free()

	st, ok := ref.Status(f.zoneID)
	if !ok {
		t.Fatal("the scheduler kept no state for the zone it tried")
	}
	if st.Failures != 1 {
		t.Errorf("failures = %d, want 1: a deadline is the attempt failing, not the process stopping", st.Failures)
	}
	if z := f.zone(t); z.LastError == "" {
		t.Error("last_error is empty: nothing anywhere says why this zone is not updating")
	}
}

// A failed transfer is retried on the SOA's *retry*, which is the shorter of
// the two timers and the whole reason the SOA carries both.
func TestSchedulerRetriesAFailureOnTheRetryNotTheRefresh(t *testing.T) {
	var refusing atomic.Bool
	primary := startTestPrimary(t, transferApex, primaryZoneRRs(t), withRcodeFn(func() int {
		if refusing.Load() {
			return dns.RcodeRefused
		}
		return 0
	}))
	f := newTransferFixture(t, primary.addr, 0)
	ref := f.refresher()

	f.refreshDue(t, ref) // succeeds; refreshed_at = T0
	refusing.Store(true)

	f.advance(primaryRefresh * time.Second)
	f.refreshDue(t, ref) // due, and fails
	if got := primary.requests(); got != 2 {
		t.Fatalf("the primary was asked %d times after the refresh elapsed, want 2", got)
	}
	failedAt := f.now

	f.advance(primaryRetry*time.Second - time.Second)
	f.refreshDue(t, ref)
	if got := primary.requests(); got != 2 {
		t.Fatalf("one second before the retry elapsed the primary was asked %d times, want 2", got)
	}

	f.advance(time.Second)
	f.refreshDue(t, ref)
	if got := primary.requests(); got != 3 {
		// A scheduler retrying on `refresh` would not have asked again until
		// 900s after the failure; this is 300.
		t.Fatalf("once the retry elapsed the primary was asked %d times, want 3", got)
	}

	st, ok := ref.Status(f.zoneID)
	if !ok {
		t.Fatalf("no status for a zone that has failed twice")
	}
	if st.Failures != 2 {
		t.Errorf("failures = %d, want 2", st.Failures)
	}
	if st.LastError == "" {
		t.Errorf("last error is empty after two failed transfers")
	}
	if want := failedAt.Add(primaryRetry * time.Second); st.NotBefore.Before(want) {
		t.Errorf("next attempt = %s, want no earlier than %s", st.NotBefore, want)
	}

	// And it recovers: the zone is back on the refresh schedule, not stuck
	// on the retry one.
	refusing.Store(false)
	f.advance(primaryRetry * time.Second)
	f.refreshDue(t, ref)
	if got := primary.requests(); got != 4 {
		t.Fatalf("the primary was asked %d times, want 4", got)
	}
	if st, _ := ref.Status(f.zoneID); st.Failures != 0 || st.LastError != "" {
		t.Errorf("status = %+v after a successful transfer, want the failure state cleared", st)
	}
}

// zeroTimerZoneRRs is the same zone with an SOA asking to be refreshed and
// retried every 0 seconds. expire and the SOA's own TTL stay valid, because
// Transfer refuses a zero in either of those and this test is about the two
// timers it does not.
func zeroTimerZoneRRs(t *testing.T) []dns.RR {
	t.Helper()
	soa := mustRR(t, fmt.Sprintf("%s. 900 IN SOA ns1.%s. hostadmin.%s. %d 0 0 %d 900",
		transferApex, transferApex, transferApex, primarySerial, primaryExpire))
	rrs := primaryZoneRRs(t)
	rrs[0], rrs[len(rrs)-1] = soa, soa
	return rrs
}

// A refresh of 0 is a busy loop against someone else's server. The value is
// stored as the primary sent it — it is the primary's SOA — and clamped where
// it is used.
func TestSchedulerClampsARefreshOfZero(t *testing.T) {
	primary := startTestPrimary(t, transferApex, zeroTimerZoneRRs(t))
	f := newTransferFixture(t, primary.addr, 0)
	ref := f.refresher()

	f.refreshDue(t, ref)
	if z := f.zone(t); z.SOARefresh != 0 {
		t.Fatalf("stored soa_refresh = %d, want the primary's 0 verbatim", z.SOARefresh)
	}

	f.advance(time.Second)
	f.refreshDue(t, ref)
	if got := primary.requests(); got != 1 {
		t.Fatalf("a second after a transfer the primary was asked %d times, want 1 — refresh 0 must not mean 'again now'", got)
	}

	f.advance(scheduleFloor - time.Second)
	f.refreshDue(t, ref)
	if got := primary.requests(); got != 2 {
		t.Fatalf("at the floor the primary was asked %d times, want 2", got)
	}
}

// The same clamp on the other timer, which is reached by a different path:
// retry gates a zone that has never transferred, where refresh does not.
func TestSchedulerClampsARetryOfZero(t *testing.T) {
	primary := startTestPrimary(t, transferApex, primaryZoneRRs(t), withRcode(dns.RcodeRefused))
	f := newTransferFixture(t, primary.addr, 0)
	f.updateZone(t, func(z *store.Zone) { z.SOARetry = 0 })
	ref := f.refresher()

	f.refreshDue(t, ref)
	if got := primary.requests(); got != 1 {
		t.Fatalf("the primary was asked %d times, want 1", got)
	}

	f.advance(scheduleFloor - time.Second)
	f.refreshDue(t, ref)
	if got := primary.requests(); got != 1 {
		t.Fatalf("just under the floor the primary was asked %d times, want 1 — retry 0 must not mean 'again now'", got)
	}

	f.advance(time.Second)
	f.refreshDue(t, ref)
	if got := primary.requests(); got != 2 {
		t.Fatalf("at the floor the primary was asked %d times, want 2", got)
	}
}

// Only a secondary is pulled from anywhere. A primary zone is authored on
// this server, and a disabled zone answers nothing, so transferring either
// would be load on someone else's server for no benefit.
func TestSchedulerLeavesPrimaryAndDisabledZonesAlone(t *testing.T) {
	primary := startTestPrimary(t, transferApex, primaryZoneRRs(t))
	f := newTransferFixture(t, primary.addr, 0)
	ref := f.refresher()

	f.updateZone(t, func(z *store.Zone) { z.Type = "primary" })
	f.refreshDue(t, ref)
	if got := primary.requests(); got != 0 {
		t.Fatalf("a primary zone caused %d transfer attempts, want 0", got)
	}
	if _, ok := ref.Status(f.zoneID); ok {
		t.Errorf("the scheduler is tracking a primary zone")
	}

	f.updateZone(t, func(z *store.Zone) { z.Type, z.Enabled = "secondary", false })
	f.refreshDue(t, ref)
	if got := primary.requests(); got != 0 {
		t.Fatalf("a disabled secondary caused %d transfer attempts, want 0", got)
	}
	if _, ok := ref.Status(f.zoneID); ok {
		t.Errorf("the scheduler is tracking a disabled zone")
	}

	// Re-enabled, it is a first sight again and transfers at once, because a
	// zone that has never transferred is answering nothing.
	f.updateZone(t, func(z *store.Zone) { z.Enabled = true })
	f.refreshDue(t, ref)
	if got := primary.requests(); got != 1 {
		t.Fatalf("after re-enabling, the primary was asked %d times, want 1", got)
	}
}

// gate returns a function that blocks every caller until n of them have
// called it, or until timeout, whichever comes first.
//
// The timeout is what makes it usable as a *test* for serialisation rather
// than a deadlock: when the code under test is correct the callers arrive one
// at a time, so each pays the timeout once and moves on. When it is not, they
// all arrive together and it opens immediately.
func gate(n int, timeout time.Duration) func() {
	var mu sync.Mutex
	arrived := 0
	open := make(chan struct{})
	return func() {
		mu.Lock()
		arrived++
		if arrived == n {
			close(open)
		}
		mu.Unlock()
		t := time.NewTimer(timeout)
		defer t.Stop()
		select {
		case <-open:
		case <-t.C:
		}
	}
}

// gatedZoneStore holds every caller at the exact point the overlap bug needs
// them to overlap — just after each has read the zone's current records and
// before any of them has written — and counts how many were ever held there
// at once. Everything else is the real store.
type gatedZoneStore struct {
	store.ZoneStore
	hold func()
	// holdList, when set, runs after the whole-zone list read a scheduling
	// pass starts with — the seam between "the pass decided what to do" and
	// "the transfer did it".
	holdList func()

	mu       sync.Mutex
	inFlight int
	peak     int
}

func (g *gatedZoneStore) Zones(ctx context.Context) ([]store.Zone, error) {
	zs, err := g.ZoneStore.Zones(ctx)
	if g.holdList != nil {
		g.holdList()
	}
	return zs, err
}

func (g *gatedZoneStore) Records(ctx context.Context, zoneID int64) ([]store.ZoneRecord, error) {
	recs, err := g.ZoneStore.Records(ctx, zoneID)
	g.mu.Lock()
	g.inFlight++
	g.peak = max(g.peak, g.inFlight)
	g.mu.Unlock()
	g.hold()
	g.mu.Lock()
	g.inFlight--
	g.mu.Unlock()
	return recs, err
}

func (g *gatedZoneStore) overlapped() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.peak
}

// Two transfers of one zone must never overlap.
//
// Each one reads the zone's records, diffs what arrived against that
// snapshot, and installs the result. Run concurrently, the second one's
// snapshot predates the first one's commit, so every row the first added is
// missing from it and gets added a second time — and zone_records has no
// uniqueness constraint that would refuse the duplicate. The zone would then
// answer every name twice over.
//
// The store is wrapped rather than the timing being left to chance: without
// the per-zone lock this fails every run, not one run in ten.
func TestConcurrentRefreshesOfOneZoneDoNotDuplicateItsRecords(t *testing.T) {
	forEachDriver(t, func(t *testing.T, driver string) {
		primary := startTestPrimary(t, transferApex, primaryZoneRRs(t))
		f := newTransferFixtureOn(t, driver, primary.addr, 0)

		zs := &gatedZoneStore{ZoneStore: f.st.Zones(), hold: gate(2, 200*time.Millisecond)}
		tr := zones.NewTransferrer(zs, f.st.TSIGKeys(),
			zones.WithTransferNow(func() time.Time { return f.now }),
			zones.WithReload(f.resolver.Reload))
		ref := zones.NewRefresher(f.st.Zones(), tr,
			zones.WithRefreshNow(func() time.Time { return f.now }))

		var wg sync.WaitGroup
		errs := make([]error, 2)
		for i := range errs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, errs[i] = ref.Refresh(context.Background(), f.zoneID)
			}()
		}
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Fatalf("refresh %d: %v", i, err)
			}
		}

		// Both transfers ran — the second waited for the first rather than being
		// dropped — and the zone holds one copy of itself.
		if got := primary.requests(); got != 2 {
			t.Errorf("the primary was asked %d times, want 2: a manual refresh waits, it does not skip", got)
		}
		if got := zs.overlapped(); got != 1 {
			t.Errorf("%d transfers of one zone were inside the read-then-write window at once, want 1", got)
		}
		if got := len(f.records(t)); got != 5 {
			t.Fatalf("the zone holds %d records after two concurrent transfers, want 5", got)
		}
		if m := f.ask(t, "bifrost."+transferApex, dns.TypeA); len(m.Answer) != 2 {
			t.Errorf("bifrost answered with %d records, want 2", len(m.Answer))
		}
	})
}

// oneShotHold blocks the first caller until the test releases it and lets
// every later one straight through. It is how a test gets inside a transfer
// that is already running, at a point of its choosing, without timing.
type oneShotHold struct {
	held    chan struct{}
	release chan struct{}
	first   sync.Once
	freed   sync.Once
}

func newOneShotHold() *oneShotHold {
	return &oneShotHold{held: make(chan struct{}), release: make(chan struct{})}
}

func (h *oneShotHold) hold() {
	taken := false
	h.first.Do(func() { taken = true })
	if !taken {
		return
	}
	close(h.held)
	<-h.release
}

func (h *oneShotHold) free() { h.freed.Do(func() { close(h.release) }) }

func (h *oneShotHold) wait(t *testing.T, what string) {
	t.Helper()
	select {
	case <-h.held:
	case <-time.After(30 * time.Second):
		t.Fatalf("nothing ever reached %s", what)
	}
}

// A transfer takes as long as the zone takes to arrive, and the row it writes
// back carries every column. An operator who disables a secondary, or
// repoints it, while one is in flight must not have that edit undone by the
// transfer landing on top of it.
func TestAnEditMadeDuringATransferSurvivesTheInstall(t *testing.T) {
	forEachDriver(t, func(t *testing.T, driver string) {
		primary := startTestPrimary(t, transferApex, primaryZoneRRs(t))
		f := newTransferFixtureOn(t, driver, primary.addr, 0)

		// Held after the transfer has read the zone's records and before it has
		// written anything — the middle of the install.
		hold := newOneShotHold()
		defer hold.free()
		zs := &gatedZoneStore{ZoneStore: f.st.Zones(), hold: hold.hold}
		tr := zones.NewTransferrer(zs, f.st.TSIGKeys(),
			zones.WithTransferNow(func() time.Time { return f.now }),
			zones.WithReload(f.resolver.Reload))
		ref := zones.NewRefresher(f.st.Zones(), tr,
			zones.WithRefreshNow(func() time.Time { return f.now }))

		done := make(chan struct{})
		go func() { defer close(done); _ = ref.RefreshDue(context.Background()) }()

		hold.wait(t, "the install")
		const repointed = "192.0.2.9:53"
		f.updateZone(t, func(z *store.Zone) { z.Enabled, z.Primaries = false, repointed })
		hold.free()

		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Fatal("the pass did not finish")
		}

		z := f.zone(t)
		if z.Enabled {
			t.Errorf("a transfer in flight re-enabled a zone the operator disabled")
		}
		if z.Primaries != repointed {
			t.Errorf("primaries = %q, want %q — the transfer reverted them", z.Primaries, repointed)
		}
		// And it is the *edit* that survived, not the transfer that was lost.
		if z.RefreshedAt == 0 || z.SOASerial != primarySerial {
			t.Errorf("refreshed_at = %d serial = %d, want the transfer to have landed", z.RefreshedAt, z.SOASerial)
		}
		if got := len(f.records(t)); got != 5 {
			t.Errorf("the zone holds %d records, want 5", got)
		}
	})
}

// The destructive corner of the same window. Transfer refuses to install into
// anything but a secondary, but it checks that before contacting anyone — so
// a zone made a primary while the transfer was in flight would be overwritten
// by data this server is now supposed to be the author of.
func TestAZoneRetypedDuringATransferIsNotOverwritten(t *testing.T) {
	forEachDriver(t, func(t *testing.T, driver string) {
		primary := startTestPrimary(t, transferApex, primaryZoneRRs(t))
		f := newTransferFixtureOn(t, driver, primary.addr, 0)

		hold := newOneShotHold()
		defer hold.free()
		zs := &gatedZoneStore{ZoneStore: f.st.Zones(), hold: hold.hold}
		tr := zones.NewTransferrer(zs, f.st.TSIGKeys(),
			zones.WithTransferNow(func() time.Time { return f.now }),
			zones.WithReload(f.resolver.Reload))
		ref := zones.NewRefresher(f.st.Zones(), tr,
			zones.WithRefreshNow(func() time.Time { return f.now }))

		done := make(chan struct{})
		go func() { defer close(done); _ = ref.RefreshDue(context.Background()) }()

		hold.wait(t, "the install")
		f.updateZone(t, func(z *store.Zone) { z.Type = "primary" })
		hold.free()

		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Fatal("the pass did not finish")
		}

		z := f.zone(t)
		if z.Type != "primary" {
			t.Errorf("type = %q, want primary — the transfer wrote back its own idea of the zone", z.Type)
		}
		if z.RefreshedAt != 0 {
			t.Errorf("refreshed_at = %d, want 0 — nothing may be installed into a zone that is no longer a secondary", z.RefreshedAt)
		}
		if got := len(f.records(t)); got != 0 {
			t.Errorf("the zone holds %d records, want 0", got)
		}
		// And it is recorded as what it was: an attempt that failed.
		if st, _ := ref.Status(f.zoneID); st.Failures != 1 {
			t.Errorf("failures = %d, want 1", st.Failures)
		}
	})
}

// The same hazard one step earlier: what a transfer *does* — which primary it
// contacts — must be what the zone row says when the transfer starts, not what
// it said when the pass read the list.
func TestAScheduledTransferReadsTheZoneAgainBeforeContactingAnyone(t *testing.T) {
	first := startTestPrimary(t, transferApex, primaryZoneRRs(t))
	second := startTestPrimary(t, transferApex, primaryZoneRRs(t))
	f := newTransferFixture(t, first.addr, 0)

	// Held between the pass's list read and its transfers.
	hold := newOneShotHold()
	defer hold.free()
	zs := &gatedZoneStore{ZoneStore: f.st.Zones(), holdList: hold.hold}
	ref := zones.NewRefresher(zs, f.transferrer(),
		zones.WithRefreshNow(func() time.Time { return f.now }))

	done := make(chan struct{})
	go func() { defer close(done); _ = ref.RefreshDue(context.Background()) }()

	hold.wait(t, "the scheduling pass")
	f.updateZone(t, func(z *store.Zone) { z.Primaries = second.addr })
	hold.free()

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the pass did not finish")
	}

	if got := second.requests(); got != 1 {
		t.Errorf("the zone's new primary was asked %d times, want 1", got)
	}
	if got := first.requests(); got != 0 {
		t.Errorf("the zone's former primary was asked %d times, want 0", got)
	}
}

// A transfer cut short because the process is stopping is not the zone's
// failure. Counted as one it would log an error on every shutdown and install
// a retry back-off into a process that no longer exists — and the next start
// would then be *later* than it needed to be for having been restarted.
func TestATransferCutShortByShutdownIsNotCountedAsAFailure(t *testing.T) {
	hold := newOneShotHold()
	defer hold.free()
	primary := startTestPrimary(t, transferApex, primaryZoneRRs(t), withHold(hold.hold))
	f := newTransferFixture(t, primary.addr, 0)
	ref := f.refresher()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); _ = ref.RefreshDue(ctx) }()

	hold.wait(t, "the primary")
	cancel() // the server is stopping, mid-transfer
	hold.free()

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the pass did not return when its context was cancelled")
	}

	if z := f.zone(t); z.RefreshedAt != 0 {
		t.Fatalf("refreshed_at = %d, want 0 — the transfer was cut short", z.RefreshedAt)
	}
	st, ok := ref.Status(f.zoneID)
	if !ok {
		t.Fatalf("the scheduler kept no state for the zone it tried")
	}
	if st.Failures != 0 {
		t.Errorf("failures = %d, want 0 — a shutdown is not the primary's fault", st.Failures)
	}
	if !st.NotBefore.IsZero() {
		t.Errorf("the next attempt is held off until %s, want no back-off from a shutdown", st.NotBefore)
	}
}

// zoneRRsFor is primaryZoneRRs for any apex, so a test can hold two secondary
// zones at once.
func zoneRRsFor(t *testing.T, apex string) []dns.RR {
	t.Helper()
	soa := mustRR(t, fmt.Sprintf("%s. 900 IN SOA ns1.%s. hostadmin.%s. %d %d %d %d 900",
		apex, apex, apex, primarySerial, primaryRefresh, primaryRetry, primaryExpire))
	return []dns.RR{
		soa,
		mustRR(t, fmt.Sprintf("%s. 3600 IN NS ns1.%s.", apex, apex)),
		mustRR(t, fmt.Sprintf("bifrost.%s. 300 IN A 10.9.0.10", apex)),
		soa,
	}
}

// The lock is per zone, and that is not an implementation detail.
//
// One lock for every zone would be simpler and would fix the duplication just
// as well — and it would also put every secondary this server holds behind
// whichever one is currently waiting out a dead primary's dial timeout. Two
// zones with nothing in common must transfer at the same time.
func TestTwoZonesTransferConcurrently(t *testing.T) {
	forEachDriver(t, func(t *testing.T, driver string) {
		const otherApex = "other.e412.in"
		first := startTestPrimary(t, transferApex, primaryZoneRRs(t))
		second := startTestPrimary(t, otherApex, zoneRRsFor(t, otherApex))
		f := newTransferFixtureOn(t, driver, first.addr, 0)
		if _, err := f.st.Zones().AddZone(context.Background(), store.Zone{
			Name: otherApex, Type: "secondary", Enabled: true,
			SOANS: "ns1." + otherApex, SOAMbox: "hostadmin." + otherApex,
			SOASerial: 1, SOARefresh: primaryRefresh, SOARetry: primaryRetry,
			SOAExpire: primaryExpire, SOAMinimum: 900, SOATTL: 900,
			Primaries: second.addr,
		}); err != nil {
			t.Fatalf("AddZone: %v", err)
		}

		// Each zone's transfer is held after reading its records until the other
		// one gets there too. Serialised, neither ever sees the other and the
		// gate can only time out.
		zs := &gatedZoneStore{ZoneStore: f.st.Zones(), hold: gate(2, 5*time.Second)}
		tr := zones.NewTransferrer(zs, f.st.TSIGKeys(),
			zones.WithTransferNow(func() time.Time { return f.now }),
			zones.WithReload(f.resolver.Reload))
		ref := zones.NewRefresher(f.st.Zones(), tr,
			zones.WithRefreshNow(func() time.Time { return f.now }))

		f.refreshDue(t, ref)

		if got := zs.overlapped(); got != 2 {
			t.Errorf("%d zones were transferring at once, want 2 — one zone must not wait for another", got)
		}
		if got := first.requests(); got != 1 {
			t.Errorf("the first zone's primary was asked %d times, want 1", got)
		}
		if got := second.requests(); got != 1 {
			t.Errorf("the second zone's primary was asked %d times, want 1", got)
		}
	})
}

// Startup, the polite half: a zone that fell due while this process was down
// but is still serving valid data waits its turn in a spread rather than
// joining a herd of every other secondary at second zero.
func TestSchedulerSpreadsZonesThatFellDueWhileTheProcessWasDown(t *testing.T) {
	primary := startTestPrimary(t, transferApex, primaryZoneRRs(t))
	f := newTransferFixture(t, primary.addr, 0)
	// Transferred two hours ago — well past its 900s refresh — and expiring
	// two hours from now, so it is still answering correctly.
	f.updateZone(t, func(z *store.Zone) {
		z.RefreshedAt = f.now.Add(-2 * time.Hour).UnixMilli()
		z.ExpiresAt = f.now.Add(2 * time.Hour).UnixMilli()
	})
	ref := f.refresher() // jitter pinned to the far end of the spread

	f.refreshDue(t, ref)
	if got := primary.requests(); got != 0 {
		t.Fatalf("an overdue but still-serving zone was transferred %d times on the first pass, want 0", got)
	}

	f.advance(scheduleFloor - time.Second)
	f.refreshDue(t, ref)
	if got := primary.requests(); got != 0 {
		t.Fatalf("just before the spread elapsed the primary was asked %d times, want 0", got)
	}

	f.advance(time.Second)
	f.refreshDue(t, ref)
	if got := primary.requests(); got != 1 {
		t.Fatalf("once the spread elapsed the primary was asked %d times, want 1", got)
	}
}

// Startup, the other half: a zone that is answering nothing has nothing to
// lose by transferring at once and a minute of downtime to lose by waiting,
// so the spread does not apply to it.
func TestSchedulerDoesNotSpreadAZoneThatIsAnsweringNothing(t *testing.T) {
	primary := startTestPrimary(t, transferApex, primaryZoneRRs(t))
	f := newTransferFixture(t, primary.addr, 0)
	f.updateZone(t, func(z *store.Zone) {
		z.RefreshedAt = f.now.Add(-2 * time.Hour).UnixMilli()
		z.ExpiresAt = f.now.Add(-time.Second).UnixMilli() // expired
	})
	ref := f.refresher()

	f.refreshDue(t, ref)
	if got := primary.requests(); got != 1 {
		t.Fatalf("an expired zone was transferred %d times on the first pass, want 1", got)
	}
}

// And the spread never outlives the zone: a zone about to expire is
// transferred before it does, not politely a minute later.
func TestTheStartupSpreadNeverOutlastsTheZone(t *testing.T) {
	primary := startTestPrimary(t, transferApex, primaryZoneRRs(t))
	f := newTransferFixture(t, primary.addr, 0)
	f.updateZone(t, func(z *store.Zone) {
		z.RefreshedAt = f.now.Add(-2 * time.Hour).UnixMilli()
		z.ExpiresAt = f.now.Add(10 * time.Second).UnixMilli()
	})
	ref := f.refresher() // the far end of whatever spread it is given

	f.advance(9 * time.Second)
	f.refreshDue(t, ref)
	if got := primary.requests(); got != 0 {
		t.Fatalf("the primary was asked %d times, want 0", got)
	}

	f.advance(time.Second)
	f.refreshDue(t, ref)
	if got := primary.requests(); got != 1 {
		t.Fatalf("a zone reaching its expiry was asked for %d times, want 1 — the spread must never outlast the zone", got)
	}
}

// Expiry stops the zone answering; it does not stop the scheduler trying.
// The data is already unserved, so giving up protects nothing, and the point
// of retrying is that the zone comes back on its own when the primary does.
func TestSchedulerKeepsRetryingAnExpiredZone(t *testing.T) {
	var refusing atomic.Bool
	primary := startTestPrimary(t, transferApex, primaryZoneRRs(t), withRcodeFn(func() int {
		if refusing.Load() {
			return dns.RcodeRefused
		}
		return 0
	}))
	f := newTransferFixture(t, primary.addr, 0)
	ref := f.refresher()

	f.refreshDue(t, ref)
	refusing.Store(true)

	// Past the SOA's expire with nothing having succeeded since.
	f.advance(primaryExpire*time.Second + time.Second)
	f.refreshDue(t, ref)
	if got := primary.requests(); got != 2 {
		t.Fatalf("the primary was asked %d times, want 2", got)
	}
	if m := f.ask(t, "bifrost."+transferApex, dns.TypeA); m.Rcode != dns.RcodeServerFailure {
		t.Fatalf("an expired zone answered %s, want SERVFAIL", dns.RcodeToString[m.Rcode])
	}

	f.advance(primaryRetry * time.Second)
	f.refreshDue(t, ref)
	if got := primary.requests(); got != 3 {
		t.Fatalf("an expired zone was asked for %d times, want 3 — expiry must not stop the retries", got)
	}

	refusing.Store(false)
	f.advance(primaryRetry * time.Second)
	f.refreshDue(t, ref)
	if got := primary.requests(); got != 4 {
		t.Fatalf("the primary was asked %d times, want 4", got)
	}
	if m := f.ask(t, "bifrost."+transferApex, dns.TypeA); m.Rcode != dns.RcodeSuccess {
		t.Errorf("after the primary recovered the zone answered %s, want NOERROR", dns.RcodeToString[m.Rcode])
	}
}

// captureLogs redirects the default logger for the duration of one test and
// returns a reader over what was written. Records arrive from the scheduler's
// workers, so it is locked.
func captureLogs(t *testing.T) func() []slog.Record {
	t.Helper()
	h := &recordingHandler{}
	prev := slog.Default()
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return h.records
}

type recordingHandler struct {
	mu  sync.Mutex
	rec []slog.Record
}

func (h *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *recordingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.rec = append(h.rec, r)
	return nil
}

func (h *recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recordingHandler) WithGroup(string) slog.Handler      { return h }

func (h *recordingHandler) records() []slog.Record {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]slog.Record(nil), h.rec...)
}

func countAtLeast(recs []slog.Record, level slog.Level) int {
	n := 0
	for _, r := range recs {
		if r.Level >= level {
			n++
		}
	}
	return n
}

// A primary that is down for a week is one event, not one per retry. The
// first failure is loud, the rest are debug, and one is promoted back to
// loud every hour so a long outage stays visible.
func TestAFailingZoneIsNotLoggedOncePerAttempt(t *testing.T) {
	primary := startTestPrimary(t, transferApex, primaryZoneRRs(t), withRcode(dns.RcodeRefused))
	f := newTransferFixture(t, primary.addr, 0)
	ref := f.refresher()
	logs := captureLogs(t)

	f.refreshDue(t, ref)
	if n := countAtLeast(logs(), slog.LevelWarn); n != 1 {
		t.Fatalf("the first failure produced %d warnings, want 1", n)
	}

	// Fifty more minutes of failing, one attempt every retry interval.
	for range 10 {
		f.advance(primaryRetry * time.Second)
		f.refreshDue(t, ref)
	}
	if got := primary.requests(); got != 11 {
		t.Fatalf("the primary was asked %d times, want 11 — the retries must still be happening", got)
	}
	if n := countAtLeast(logs(), slog.LevelWarn); n != 1 {
		t.Fatalf("ten more identical failures produced %d warnings in total, want 1", n)
	}

	// Past the hour, the outage says so again rather than going silent
	// forever.
	f.advance(time.Hour)
	f.refreshDue(t, ref)
	if n := countAtLeast(logs(), slog.LevelWarn); n != 2 {
		t.Fatalf("after an hour of failing there were %d warnings, want 2", n)
	}
}

// Run is what app.go starts. It transfers what is already due before its
// first tick — a scheduler that waited a tick to notice work that was due
// when it started would leave a restarted server answering nothing for no
// reason — and it stops when its context is cancelled, which is what makes
// App.Shutdown terminate.
func TestRunTransfersAtStartupAndStopsOnCancel(t *testing.T) {
	arrived := make(chan struct{}, 1)
	primary := startTestPrimary(t, transferApex, primaryZoneRRs(t), withHold(func() {
		select {
		case arrived <- struct{}{}:
		default:
		}
	}))
	f := newTransferFixture(t, primary.addr, 0)
	ref := f.refresher()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stopped := make(chan struct{})
	go func() {
		ref.Run(ctx)
		close(stopped)
	}()

	select {
	case <-arrived:
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not transfer the due zone before its first tick")
	}

	cancel()
	select {
	case <-stopped:
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not return when its context was cancelled")
	}
}

// gatedNoteStore holds just before whichever write records a transfer's
// outcome, so a test can get between the moment that outcome is decided and
// the moment it lands.
//
// Both candidate writes are gated, deliberately. NoteTransferAttempt is the
// one that should be used; UpdateZone is the whole-row write it must not be
// folded into, and gating that one too is what makes the test fail with the
// revert it is about rather than with "the hold was never reached". Only the
// Refresher is given this wrapper — the test's own edits go straight to the
// store underneath, so they are never held by it.
type gatedNoteStore struct {
	store.ZoneStore
	hold func()
}

func (g *gatedNoteStore) NoteTransferAttempt(ctx context.Context, zoneID, at int64, errText string) error {
	g.hold()
	return g.ZoneStore.NoteTransferAttempt(ctx, zoneID, at, errText)
}

func (g *gatedNoteStore) UpdateZone(ctx context.Context, z store.Zone) error {
	g.hold()
	return g.ZoneStore.UpdateZone(ctx, z)
}

// A failure used to leave no trace in the database at all: refreshed_at is
// the last transfer that *worked*, and everything the scheduler knew about
// one that did not was in memory. So a zone whose primary had been refusing
// connections for a week came back from a restart looking healthy until its
// next attempt landed, up to a retry interval later.
func TestAFailedTransferRecordsWhyItFailed(t *testing.T) {
	// A port nothing is listening on: the dial is refused rather than timing
	// out, so the failure is immediate and its text is the resolver's own.
	f := newTransferFixture(t, deadPort(t), 0)
	r := f.refresher()

	f.refreshDue(t, r)

	z := f.zone(t)
	if z.LastError == "" {
		t.Fatalf("last_error is empty after a failed transfer; the failure left no trace at all")
	}
	if z.LastAttempt != f.now.UnixMilli() {
		t.Errorf("last_attempt = %d, want %d — an error with no date says nothing about whether it is still true",
			z.LastAttempt, f.now.UnixMilli())
	}
	// The failure is recorded; the zone is not otherwise touched. A failed
	// transfer changes nothing about what the zone holds or may serve.
	if z.RefreshedAt != 0 {
		t.Errorf("refreshed_at = %d, want 0 — a failed transfer must not read as a success", z.RefreshedAt)
	}
}

// The other half, and the one that keeps the column from becoming a permanent
// tombstone of a problem that was fixed weeks ago: empty means "the last
// attempt succeeded".
func TestASuccessfulTransferClearsTheRecordedError(t *testing.T) {
	f := newTransferFixture(t, deadPort(t), 0)
	f.refreshDue(t, f.refresher())
	if f.zone(t).LastError == "" {
		t.Fatal("setup: the first pass was meant to fail and record why")
	}

	// Repoint at a primary that answers, and let the retry come round.
	primary := startTestPrimary(t, transferApex, primaryZoneRRs(t))
	f.updateZone(t, func(z *store.Zone) { z.Primaries = primary.addr })
	f.advance(2 * primaryRetry * time.Second)
	f.refreshDue(t, f.refresher())

	z := f.zone(t)
	if z.LastError != "" {
		t.Errorf("last_error = %q after a transfer that worked, want it cleared", z.LastError)
	}
	if z.LastAttempt != f.now.UnixMilli() {
		t.Errorf("last_attempt = %d, want %d — a success is an attempt too", z.LastAttempt, f.now.UnixMilli())
	}
	if z.RefreshedAt != f.now.UnixMilli() {
		t.Errorf("refreshed_at = %d, want %d", z.RefreshedAt, f.now.UnixMilli())
	}
}

// The trap this column had to be designed around. Every other write to the
// zones table binds the row entire, and the transfer install already had to
// be fixed once for reverting an operator's edit made while it was in flight
// (TestAnEditMadeDuringATransferSurvivesTheInstall). Recording a *failure*
// through any of those paths would reintroduce exactly that, through a second
// door — and a failing zone is retried every retry interval, so it would keep
// undoing the edit until someone noticed.
//
// Held inside the write itself rather than inside the transfer: what is being
// proved is that this statement, whatever happened just before it, cannot
// carry a stale copy of any other column with it.
func TestRecordingAFailureDoesNotRevertAnEditMadeBeforeIt(t *testing.T) {
	forEachDriver(t, func(t *testing.T, driver string) {
		f := newTransferFixtureOn(t, driver, deadPort(t), 0)
		hold := newOneShotHold()
		defer hold.free()
		r := zones.NewRefresher(&gatedNoteStore{ZoneStore: f.st.Zones(), hold: hold.hold}, f.transferrer(),
			zones.WithRefreshNow(func() time.Time { return f.now }))

		done := make(chan struct{})
		go func() { defer close(done); _ = r.RefreshDue(context.Background()) }()

		hold.wait(t, "the failure being recorded")
		const repointed = "192.0.2.9:5353"
		f.updateZone(t, func(z *store.Zone) {
			z.Primaries = repointed
			z.SOAMbox = "someone-else." + transferApex
		})
		hold.free()

		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Fatal("the pass did not finish")
		}

		z := f.zone(t)
		if z.Primaries != repointed {
			t.Errorf("primaries = %q, want %q — recording a failure reverted the operator's edit", z.Primaries, repointed)
		}
		if z.SOAMbox != "someone-else."+transferApex {
			t.Errorf("soa_mbox = %q — recording a failure reverted an unrelated column too", z.SOAMbox)
		}
		// And the failure was still recorded: the edit surviving must not be the
		// write having been lost.
		if z.LastError == "" {
			t.Errorf("last_error is empty; the edit survived because nothing was written")
		}
	})
}

// stubDelegation is the master a stub's tests fetch from: one nameserver
// inside the zone, with glue, which is the shape every deployment of the type
// has.
func stubDelegation(t *testing.T) *stubMaster {
	t.Helper()
	return startStubMaster(t, transferApex, stubMasterConfig{
		serial: primarySerial,
		ns:     []string{stubNSLine("ns1." + transferApex)},
		glue:   []string{fmt.Sprintf("ns1.%s. 3600 IN A 10.9.0.1", transferApex)},
	})
}

// stubUpstream is the dial address stubDelegation's glue implies.
const stubUpstream = "10.9.0.1:53"

// A stub pulls from a master too — two ordinary queries instead of an AXFR —
// and it is this same loop that decides when. One scheduler for both, because
// both are "ask a master on the SOA's schedule and record how it went", and
// two loops would drift.
//
// Until this landed a stub was never scheduled at all: it fetched only when
// an operator asked, so a stub left alone claimed its suffix and answered
// SERVFAIL forever.
func TestRefreshDueFetchesAStubZone(t *testing.T) {
	forEachDriver(t, func(t *testing.T, driver string) {
		master := stubDelegation(t)
		f := newTransferFixtureOn(t, driver, master.addr, 0, asZoneType("stub"))
		ref := f.refresher()

		// Nothing fetched yet: the zone claims its suffix with no addresses
		// behind it, which is SERVFAIL for everything under it.
		if u := upstreamsFromSnapshot(t, f); len(u) != 0 {
			t.Fatalf("precondition: the unfetched stub already names %v", u)
		}

		f.refreshDue(t, ref)

		// SOA then NS. A scheduler that handed a stub to the Transferrer would
		// have contacted nobody at all — Transfer refuses a non-secondary before
		// it dials — and one that held the zone in the startup spread (the
		// fixture pins the jitter to the far end of it) would have too.
		if got := master.queries.Load(); got != 2 {
			t.Fatalf("the master was asked %d times, want 2 (SOA then NS)", got)
		}
		if u := upstreamsFromSnapshot(t, f); len(u) != 1 || u[0] != stubUpstream {
			t.Fatalf("StubUpstreams from the snapshot = %v, want [%s]", u, stubUpstream)
		}

		z := f.zone(t)
		if z.RefreshedAt != f.now.UnixMilli() {
			t.Errorf("refreshed_at = %d, want %d", z.RefreshedAt, f.now.UnixMilli())
		}
		if z.SOASerial != primarySerial || z.SOARefresh != primaryRefresh {
			t.Errorf("zone row: serial=%d refresh=%d, want the master's %d/%d",
				z.SOASerial, z.SOARefresh, primarySerial, primaryRefresh)
		}
		// The schedule is adopted; the expiry is not written at all. See
		// TestAStubDoesNotExpire.
		if z.ExpiresAt != 0 {
			t.Errorf("expires_at = %d, want 0 — a stub is given no expiry", z.ExpiresAt)
		}
		st, ok := ref.Status(f.zoneID)
		if !ok {
			t.Fatalf("the scheduler kept no state for the stub it fetched")
		}
		if st.Failures != 0 || !st.NotBefore.IsZero() {
			t.Errorf("status = %+v, want no failures and no back-off after a success", st)
		}

		// And the interval between fetches is the master's own SOA refresh, to
		// the second, exactly as a secondary's is.
		f.advance(primaryRefresh*time.Second - time.Second)
		f.refreshDue(t, ref)
		if got := master.queries.Load(); got != 2 {
			t.Fatalf("one second before the refresh elapsed the master was asked %d times, want 2", got)
		}

		f.advance(time.Second)
		f.refreshDue(t, ref)
		if got := master.queries.Load(); got != 4 {
			t.Fatalf("once the refresh elapsed the master was asked %d times, want 4", got)
		}
		if z := f.zone(t); z.RefreshedAt != f.now.UnixMilli() {
			t.Errorf("refreshed_at = %d after the second fetch, want %d", z.RefreshedAt, f.now.UnixMilli())
		}
	})
}

// §9.11.8, and the divergence that must not be inherited away.
//
// A secondary past its SOA expire stops answering (Zone.Serving): it holds
// its primary's *data* on loan and can no longer confirm that what it holds
// is current. A stub holds no data. Its NS set is routing information, and an
// old-but-working nameserver beats a self-inflicted SERVFAIL — if those
// nameservers really are gone the query fails anyway, through the forwarder's
// own path, so the outcome is the same when it should be and better when it
// should not.
//
// This test is the only thing standing between that decision and a later
// change that widens Serving to cover stub "for consistency", which is
// exactly the reasoning "both pull from a master" invites.
func TestAStubDoesNotExpire(t *testing.T) {
	master := stubDelegation(t)
	f := newTransferFixture(t, master.addr, 0, asZoneType("stub"))
	ref := f.refresher()

	f.refreshDue(t, ref)
	if u := upstreamsFromSnapshot(t, f); len(u) != 1 {
		t.Fatalf("setup: the fetch installed %v, want one upstream", u)
	}

	// Long past any expiry the master's SOA implies, with the column set as
	// though something had written one — a stub's own install never does, so
	// this is the row a zone retyped from secondary to stub carries.
	f.advance(primaryExpire*time.Second + time.Hour)
	f.updateZone(t, func(z *store.Zone) { z.ExpiresAt = f.now.Add(-time.Hour).UnixMilli() })

	z := f.resolver.Snapshot().Apex(transferApex)
	if z == nil {
		t.Fatalf("the zone is not in the served snapshot")
	}
	if !z.Serving(f.now.UnixMilli()) {
		t.Errorf("a stub an hour past its expires_at stopped serving: its NS set is routing information, " +
			"not data held on loan, so it keeps forwarding (§9.11.8)")
	}
	if u := zones.StubUpstreams(*z); len(u) != 1 || u[0] != stubUpstream {
		t.Errorf("StubUpstreams = %v, want [%s] — an expired stub stopped contributing to the routing table", u, stubUpstream)
	}

	// The contrast that makes this a divergence rather than a tautology: the
	// very same row, as a secondary, has stopped answering. A copy, because
	// the snapshot's zone is shared with every reader of it.
	asSecondary := *z
	asSecondary.Type = "secondary"
	if asSecondary.Serving(f.now.UnixMilli()) {
		t.Errorf("the same row as a secondary is still serving; the two types are not being told apart at all")
	}

	// The other half: a failed attempt is the only event that can carry a
	// zone across its expiry, and it is where a secondary announces that it
	// has stopped answering. A stub has not stopped, so it must not say so —
	// an operator reading that line would go looking for an outage that is
	// not happening.
	logs := captureLogs(t)
	f.updateZone(t, func(z *store.Zone) { z.Primaries = deadPort(t) })
	f.refreshDue(t, ref)
	if f.zone(t).LastError == "" {
		t.Fatalf("the attempt that was meant to fail did not, so there is no sample to assert on")
	}
	for _, r := range logs() {
		if r.Level >= slog.LevelError {
			t.Errorf("a stub past its expires_at logged %q at %s; a stub does not expire and has not stopped answering", r.Message, r.Level)
		}
	}
}

// A failed fetch is recorded on the zone exactly as a failed transfer is —
// same columns, same back-off, same retry timer — and changes nothing else.
// The zone keeps the NS set it already had and goes on routing to it: a stub
// that dropped its delegation because a master was briefly unreachable would
// SERVFAIL its whole suffix over an outage somewhere else.
func TestStubFetchFailureKeepsThePreviousNSSet(t *testing.T) {
	master := stubDelegation(t)
	f := newTransferFixture(t, master.addr, 0, asZoneType("stub"))
	ref := f.refresher()

	f.refreshDue(t, ref)
	fetchedAt := f.zone(t).RefreshedAt
	if fetchedAt == 0 {
		t.Fatalf("setup: the first fetch did not land")
	}

	// A master that is up and unwilling, so the failure is immediate and the
	// attempts are countable.
	refuser := startStubMaster(t, transferApex, stubMasterConfig{rcode: dns.RcodeRefused})
	f.updateZone(t, func(z *store.Zone) { z.Primaries = refuser.addr })
	f.advance(primaryRefresh * time.Second)
	f.refreshDue(t, ref)

	if got := refuser.queries.Load(); got != 1 {
		t.Fatalf("the refusing master was asked %d times, want 1", got)
	}
	z := f.zone(t)
	if z.LastError == "" {
		t.Errorf("last_error is empty after a failed fetch; the failure left no trace at all")
	}
	if z.LastAttempt != f.now.UnixMilli() {
		t.Errorf("last_attempt = %d, want %d", z.LastAttempt, f.now.UnixMilli())
	}
	if z.RefreshedAt != fetchedAt {
		t.Errorf("refreshed_at = %d, want %d — a failed fetch must not read as a success", z.RefreshedAt, fetchedAt)
	}
	if u := upstreamsFromSnapshot(t, f); len(u) != 1 || u[0] != stubUpstream {
		t.Errorf("StubUpstreams = %v after a failed fetch, want [%s] — the zone dropped a delegation it still had", u, stubUpstream)
	}
	st, ok := ref.Status(f.zoneID)
	if !ok {
		t.Fatalf("the scheduler kept no state for the stub it tried")
	}
	if st.Failures != 1 || st.LastError == "" {
		t.Errorf("status = %+v, want one recorded failure with its message", st)
	}
	if want := f.now.Add(primaryRetry * time.Second); st.NotBefore.Before(want) {
		t.Errorf("next attempt = %s, want no earlier than %s", st.NotBefore, want)
	}

	// And the retry lands on the SOA's retry rather than its refresh — the
	// shorter of the two timers, through the same back-off a secondary gets.
	f.advance(primaryRetry*time.Second - time.Second)
	f.refreshDue(t, ref)
	if got := refuser.queries.Load(); got != 1 {
		t.Fatalf("one second before the retry elapsed the master was asked %d times, want 1", got)
	}

	f.advance(time.Second)
	f.refreshDue(t, ref)
	if got := refuser.queries.Load(); got != 2 {
		t.Fatalf("once the retry elapsed the master was asked %d times, want 2 — a stub's back-off is the SOA's retry", got)
	}
}

// The manual path — an operator pressing refresh, and the one a NOTIFY takes
// — reports a stub's fetch in the same shape it reports a transfer, so a
// caller does not have to know which kind of zone it asked about.
//
// ExpiresAt is the one field that differs, and it is zero rather than
// unwritten: a stub is never given an expiry (§9.11.8), so there is nothing
// for a caller to display or compare against.
func TestRefreshingAStubOnDemandReportsWhatItFetched(t *testing.T) {
	master := stubDelegation(t)
	f := newTransferFixture(t, master.addr, 0, asZoneType("stub"))

	res, err := f.refresher().Refresh(context.Background(), f.zoneID)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if got := master.queries.Load(); got != 2 {
		t.Fatalf("the master was asked %d times, want 2 (SOA then NS)", got)
	}
	if res.Primary.String() != master.addr {
		t.Errorf("Primary = %s, want the master that answered, %s", res.Primary, master.addr)
	}
	if res.Serial != primarySerial {
		t.Errorf("Serial = %d, want the master's %d", res.Serial, primarySerial)
	}
	// The delegation and the glue beside it: what the zone holds after the
	// install, the same thing the number means for a transfer.
	if res.Records != 2 {
		t.Errorf("Records = %d, want 2 (the NS record and its glue)", res.Records)
	}
	if res.RefreshedAt != f.now.UnixMilli() {
		t.Errorf("RefreshedAt = %d, want %d — the stamp the install actually wrote", res.RefreshedAt, f.now.UnixMilli())
	}
	if res.ExpiresAt != 0 {
		t.Errorf("ExpiresAt = %d, want 0 — a stub is given no expiry", res.ExpiresAt)
	}
	if z := f.zone(t); z.RefreshedAt != res.RefreshedAt || z.ExpiresAt != 0 {
		t.Errorf("the zone row says refreshed_at=%d expires_at=%d, want %d and 0",
			z.RefreshedAt, z.ExpiresAt, res.RefreshedAt)
	}
}

// A scheduler built with no stub fetcher is a misconfiguration, and there are
// two wrong ways to meet one. Handing the zone to a nil fetcher panics on a
// worker goroutine, which takes the process down; skipping it quietly leaves
// the stub claiming its suffix and answering SERVFAIL with nothing anywhere
// saying why. It is recorded as the failed attempt it is, in the column an
// operator asking "why is this zone not routing" already reads.
func TestAStubWithNoFetcherConfiguredIsRecordedNotSkipped(t *testing.T) {
	master := stubDelegation(t)
	f := newTransferFixture(t, master.addr, 0, asZoneType("stub"))
	ref := zones.NewRefresher(f.st.Zones(), f.transferrer(),
		zones.WithRefreshNow(func() time.Time { return f.now }))

	f.refreshDue(t, ref)

	if got := master.queries.Load(); got != 0 {
		t.Errorf("the master was asked %d times, want 0 — there is nothing to ask it with", got)
	}
	z := f.zone(t)
	if z.LastError == "" {
		t.Errorf("last_error is empty: a stub this server cannot fetch left no trace at all")
	}
	if z.RefreshedAt != 0 {
		t.Errorf("refreshed_at = %d, want 0 — nothing was fetched", z.RefreshedAt)
	}
	st, ok := ref.Status(f.zoneID)
	if !ok || st.Failures != 1 {
		t.Errorf("status = %+v (tracked = %v), want one recorded failure", st, ok)
	}
}

// What actually changed is the one thing a transfer log line has to carry.
// The serial on its own says a new version arrived; "+1/-1" says whether it
// was a routine re-sign or somebody deleting a nameserver, and it is the
// difference between reading the log and going to look at the zone.
//
// It is Info because it is the whole account of a change to served data. The
// unchanged case stays at Debug, where a refresh that found nothing belongs.
func TestAChangedInstallLogsTheSerialAndTheRecordDelta(t *testing.T) {
	primary := startTestPrimary(t, transferApex, primaryZoneRRs(t))
	f := newTransferFixture(t, primary.addr, 0)
	ref := f.refresher()
	logs := captureLogs(t)

	f.refreshDue(t, ref)

	first := transferChangeLines(logs())
	if len(first) != 1 {
		t.Fatalf("the first transfer logged %d change lines, want 1: %v", len(first), first)
	}
	// A zone created through the API sits at serial 1 with no records, so the
	// first transfer is five additions and nothing else.
	want := map[string]any{
		// slog widens every integer attribute, so the wants are spelled in the
		// types that come back out of a Record rather than the ones passed in.
		"zone": transferApex, "old_serial": uint64(1), "new_serial": uint64(primarySerial),
		"added": int64(5), "removed": int64(0), "changed": int64(0),
	}
	for k, v := range want {
		if got := first[0][k]; got != v {
			t.Errorf("the first transfer logged %s = %v (%T), want %v (%T)", k, got, got, v, v)
		}
	}

	// A second primary at the next serial, one record swapped for another.
	rrs := primaryZoneRRs(t)
	soa := mustRR(t, fmt.Sprintf("%s. 900 IN SOA ns1.%s. hostadmin.%s. %d %d 300 %d 900",
		transferApex, transferApex, transferApex, primarySerial+1, primaryRefresh, primaryExpire))
	moved := []dns.RR{soa}
	for _, rr := range rrs[1 : len(rrs)-1] {
		if rr.Header().Name == "bifrost."+transferApex+"." && rr.String() != "" &&
			strings.HasSuffix(rr.String(), "10.9.0.11") {
			continue
		}
		moved = append(moved, rr)
	}
	moved = append(moved, mustRR(t, fmt.Sprintf("nas.%s. 300 IN A 10.9.0.20", transferApex)), soa)
	second := startTestPrimary(t, transferApex, moved)
	f.updateZone(t, func(z *store.Zone) { z.Primaries = second.addr })

	f.advance(primaryRefresh * time.Second)
	f.refreshDue(t, ref)

	lines := transferChangeLines(logs())
	if len(lines) != 2 {
		t.Fatalf("after the second transfer there were %d change lines, want 2: %v", len(lines), lines)
	}
	want = map[string]any{
		"zone": transferApex, "old_serial": uint64(primarySerial), "new_serial": uint64(primarySerial + 1),
		"added": int64(1), "removed": int64(1), "changed": int64(0),
	}
	for k, v := range want {
		if got := lines[1][k]; got != v {
			t.Errorf("the second transfer logged %s = %v (%T), want %v (%T)", k, got, got, v, v)
		}
	}
}

// A refresh that found the zone already current installs nothing, so there is
// no delta to report and no line to report it on.
func TestAnUnchangedRefreshLogsNoChangeLine(t *testing.T) {
	primary := startTestPrimary(t, transferApex, primaryZoneRRs(t))
	f := newTransferFixture(t, primary.addr, 0)
	ref := f.refresher()

	f.refreshDue(t, ref)
	logs := captureLogs(t)

	// The same primary at the same serial: the transfer re-runs (this fake
	// answers no SOA probe, so the schedule falls through to a full AXFR) and
	// the diff comes out empty.
	f.advance(primaryRefresh * time.Second)
	f.refreshDue(t, ref)

	if lines := transferChangeLines(logs()); len(lines) != 0 {
		t.Errorf("a transfer that changed nothing logged %d change lines, want 0: %v", len(lines), lines)
	}
}

// transferChangeLines is every Info-or-louder record for a transfer that
// installed a change, with its attributes flattened for comparison.
func transferChangeLines(recs []slog.Record) []map[string]any {
	var out []map[string]any
	for _, r := range recs {
		if r.Message != "zone transferred with changes" || r.Level < slog.LevelInfo {
			continue
		}
		attrs := map[string]any{}
		r.Attrs(func(a slog.Attr) bool {
			attrs[a.Key] = a.Value.Any()
			return true
		})
		out = append(out, attrs)
	}
	return out
}
