package zones

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strings"
	"sync"
	"time"

	"github.com/aloks98/dnsaur/internal/store"
)

// The refresh schedule: what makes a zone that pulls from a master a copy
// rather than a snapshot.
//
// Two types pull, and both are scheduled here. A secondary takes a whole zone
// by AXFR (Transferrer); a stub takes only the apex SOA and NS by ordinary
// query (StubFetcher). Each knows how to do that once. This is what decides
// *when*, and the schedule is not this server's to invent — it is the one the
// master published in the zone's own SOA (RFC 1035 §3.3.13):
//
//   - refresh: how long after a successful pull to ask again.
//   - retry: how long after a failed one to try again.
//   - expire: how long the data may still be served with nothing having
//     succeeded. A secondary's alone, and not enforced here at all: it is
//     stamped onto the zone row by the install and read at answer time by
//     Zone.Serving, which is the only place it can be enforced correctly — a
//     scheduler that stops a zone answering would be racing every query
//     already in flight. A stub is never given one, because it holds routing
//     information rather than data that could go stale (§9.11.8).
//
// refreshed_at is the source of truth for "when did this last succeed", read
// back from the store on every pass rather than cached here. The scheduler
// keeps in memory only what has no column: the retry back-off after a failure,
// the startup spread, and the counters that keep a week-long outage from
// writing a week of identical log lines.

const (
	// refreshTick is how often the scheduler asks which zones are due. It is
	// not the schedule — it is the resolution of it, so it bounds how late an
	// attempt can be rather than how often one happens. One indexed read of a
	// small table per tick.
	refreshTick = 30 * time.Second

	// minInterval is the floor under every interval taken from an SOA.
	//
	// refresh and retry arrive from a remote peer and are used, unaltered, as
	// the period of a loop that opens TCP connections to that same peer. A
	// zero is the case that has to be handled — a secondary that re-transfers
	// every 0 seconds is a busy loop against someone else's server, and a
	// primary can publish one by accident — but zero is not special: 1 is
	// just as much a busy loop, so the guard is a floor rather than a
	// zero-check.
	//
	// It is a clamp here and not a refusal in Transfer, deliberately. An
	// expire of 0 is refused there because installing it produces a zone that
	// answers nothing; a refresh of 0 says nothing about whether the records
	// are good, so refusing the transfer over it would take a zone that would
	// otherwise serve its primary's data correctly and leave it dark over a
	// timer. Clamping serves the data and polls sanely, which is also what
	// BIND does (min-refresh-time, min-retry-time).
	minInterval = 60 * time.Second

	// startupSpread bounds the delay given to a zone that fell due while this
	// process was not running. See firstAttempt.
	startupSpread = 60 * time.Second

	// failureLogEvery is how often a zone that keeps failing is allowed to say
	// so above debug level. A primary that is down for a week is one event,
	// not 2,000 of them.
	failureLogEvery = time.Hour
)

// Refresher keeps every zone that pulls from a master as close to that
// master as its SOA asks for: a secondary by AXFR, a stub by the two
// ordinary queries its delegation takes.
type Refresher struct {
	zs store.ZoneStore
	tr *Transferrer
	// sf fetches a stub's delegation. Nil is a scheduler built without one
	// (WithStubFetcher), which is a misconfiguration rather than a mode: a
	// stub that is due is then recorded as a failed attempt, where an
	// operator looking for why the zone is not routing will find it.
	sf *StubFetcher

	// now is the clock the whole schedule is decided against, injected for
	// the same reason Resolver's and Transferrer's are: a test that cannot
	// move time can only test a schedule by waiting for it.
	now func() time.Time
	// jitter picks a delay in [0, d) for the startup spread. Injected so a
	// test can pin the spread to one end of it instead of asserting about a
	// random number.
	jitter func(d time.Duration) time.Duration

	// mu guards zones and every field of the zoneState values in it. It is
	// never held across a transfer — a transfer takes seconds, and every
	// other zone's worker on the same pass wants this lock.
	mu    sync.Mutex
	zones map[int64]*zoneState
}

// zoneState is what the scheduler remembers about one zone between passes.
//
// All of it is process-local by design. A restart re-derives the schedule
// from refreshed_at, which is the column that survives; what is lost is a
// pending back-off, and losing that means a zone that was failing gets one
// early attempt after a restart rather than a wrong one.
type zoneState struct {
	// xfer serialises transfers *of this zone*.
	//
	// Two overlapping transfers of one zone corrupt it: each reads the zone's
	// current records, computes a diff against that snapshot, and installs
	// it, so the second one re-adds every row the first has already committed
	// — and zone_records has no uniqueness constraint to stop it. It cannot
	// be fixed inside Transfer, because two Transferrer values share no
	// state; it belongs to whatever creates concurrent callers, which is
	// this.
	//
	// Per zone rather than one lock for all of them: a global lock would put
	// every secondary in the process behind whichever one is currently
	// waiting out a dead primary's dial timeout.
	xfer sync.Mutex

	// notBefore is the earliest, in unix ms, the next attempt may be made. It
	// carries the retry back-off and the startup spread; a successful
	// transfer clears it and hands the decision back to refreshed_at.
	notBefore int64

	lastAttempt int64
	fails       int
	lastErr     string

	// lastFailLog and expiredLogged are the noise controls; clampLogged makes
	// the "your SOA asks for something we will not do" warning once-per-zone
	// rather than once-per-attempt.
	lastFailLog   int64
	expiredLogged bool
	clampLogged   bool
}

// RefreshOption configures a Refresher at construction.
type RefreshOption func(*Refresher)

// WithRefreshNow replaces the clock the schedule is decided against.
func WithRefreshNow(now func() time.Time) RefreshOption {
	return func(r *Refresher) { r.now = now }
}

// WithJitter replaces the startup spread's source of randomness. It is called
// with the width of the spread and must return a delay within it.
func WithJitter(jitter func(d time.Duration) time.Duration) RefreshOption {
	return func(r *Refresher) { r.jitter = jitter }
}

// WithStubFetcher gives the scheduler the worker a stub zone is refreshed
// with. It is an option rather than a constructor argument because a stub is
// fetched through a different object from a secondary's Transferrer — see
// StubFetcher — and a scheduler that will only ever see secondaries needs
// none.
//
// **In production the fetcher passed here must be built with
// WithStubReload(App.ReloadZones).** A stub's upstreams are derived from the
// NS records a fetch installs, so a fetch that republishes only the served
// snapshot leaves the zone claiming its suffix against a routing table that
// still has no addresses for it — SERVFAIL forever, and indistinguishable
// from a fetch that never happened.
func WithStubFetcher(sf *StubFetcher) RefreshOption {
	return func(r *Refresher) { r.sf = sf }
}

// NewRefresher returns a Refresher that reads its zones from zs and transfers
// them with tr.
func NewRefresher(zs store.ZoneStore, tr *Transferrer, opts ...RefreshOption) *Refresher {
	r := &Refresher{
		zs:     zs,
		tr:     tr,
		now:    time.Now,
		jitter: defaultJitter,
		zones:  map[int64]*zoneState{},
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Transferrer returns the *Transferrer this Refresher schedules against —
// the same component a scheduled transfer takes st.xfer's lock around. It is
// what NotifyServer's SOA probe (WithNotifyProbes) is built from in
// production, so a notify-triggered transfer takes that same per-zone lock
// rather than racing a scheduled one.
func (r *Refresher) Transferrer() *Transferrer { return r.tr }

// defaultJitter spreads uniformly over [0, d).
func defaultJitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	return rand.N(d) //nolint:gosec // spreading load, not generating a secret
}

// Run drives the schedule until ctx is cancelled, starting with one pass
// immediately — the shape App.runTokenCleanup uses, and for the same reason:
// the first thing a scheduler owes is the work that was already due when it
// started.
func (r *Refresher) Run(ctx context.Context) {
	t := time.NewTicker(refreshTick)
	defer t.Stop()
	r.pass(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.pass(ctx)
		}
	}
}

func (r *Refresher) pass(ctx context.Context) {
	if err := r.RefreshDue(ctx); err != nil {
		if ctx.Err() != nil {
			// Shutting down. The pass failing because the process is going
			// away is not a fault to report as one.
			return
		}
		slog.Error("zone refresh pass failed", "err", err)
	}
}

// RefreshDue transfers every secondary zone the schedule says is due, and
// returns once they have all finished.
//
// The zones of one pass run concurrently, one goroutine each, because they
// are independent and a primary that is refusing connections costs a dial
// timeout that nothing else should have to wait out. A zone whose previous
// transfer is somehow still running is skipped rather than queued: a refresh
// that is already happening is the refresh this pass wanted.
//
// The error reported is a failure to *schedule* — the store read. A zone that
// failed to transfer is recorded against that zone and does not fail the pass,
// because one unreachable primary must not stop the other zones being tried.
func (r *Refresher) RefreshDue(ctx context.Context) error {
	all, err := r.zs.Zones(ctx)
	if err != nil {
		return err
	}
	nowMs := r.now().UnixMilli()
	var wg sync.WaitGroup
	for _, z := range all {
		if !pullsFromAMaster(z.Type) {
			continue
		}
		// A disabled zone answers nothing (Index.Find skips it), so
		// transferring it would be load on someone else's server for data
		// this server would not serve. Re-enabling it puts it back in the
		// ordinary schedule on the next pass, which for a zone that has never
		// transferred means at once.
		if !z.Enabled {
			continue
		}
		st := r.stateFor(z, nowMs)
		r.warnIfClamped(z, st)
		if !r.due(z, nowMs, st) {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if !st.xfer.TryLock() {
				slog.Debug("zone transfer still running, skipping this pass", "zone", z.Name)
				return
			}
			defer st.xfer.Unlock()
			// Read the zone again under the lock, exactly as Refresh does.
			// The copy above was read before the lock, and what a transfer
			// does with a zone row — which primaries it contacts, which key it
			// signs with — must be what that row says now, not what it said
			// when the pass began. A zone deleted in between simply stops
			// being transferred.
			fresh, err := r.zs.Zone(ctx, z.ID)
			if err != nil {
				if ctx.Err() == nil {
					slog.Warn("reading a zone before transferring it failed", "zone", z.Name, "err", err)
				}
				return
			}
			_, _ = r.transfer(ctx, fresh, st)
		}()
	}
	wg.Wait()
	return nil
}

// Refresh transfers one zone now, whatever its schedule says. It is the
// manual path — an operator asking for it — and so it *waits* for a transfer
// already in flight rather than skipping, which is what a person who pressed
// a button meant.
func (r *Refresher) Refresh(ctx context.Context, zoneID int64) (TransferResult, error) {
	z, err := r.zs.Zone(ctx, zoneID)
	if err != nil {
		return TransferResult{}, err
	}
	st := r.stateFor(z, r.now().UnixMilli())
	st.xfer.Lock()
	defer st.xfer.Unlock()
	// Read again under the lock. If a transfer of this zone was running when
	// this call arrived, the copy read above is the one it has just
	// overwritten, and Transfer writes the whole zone row back — from a stale
	// copy it would undo it.
	if z, err = r.zs.Zone(ctx, zoneID); err != nil {
		return TransferResult{}, err
	}
	return r.transfer(ctx, z, st)
}

// pull runs the one attempt z's type calls for: a secondary's AXFR, or a
// stub's two ordinary queries.
//
// The branch is here, below the per-zone lock and above the recording, so
// everything either kind of attempt shares is shared by construction — the
// lock that stops two of them overlapping on one zone (the read-diff-install
// window is the same window in both), the retry back-off, and the durable
// record of how it went.
//
// A stub's outcome is reported in the scheduler's own vocabulary rather than
// the fetcher's, because the scheduler's callers ask one question of every
// zone. ExpiresAt stays zero for a stub, and that is not a gap: a stub is
// never given an expiry, because it does not expire (§9.11.8, and see
// StubFetcher.install).
func (r *Refresher) pull(ctx context.Context, z store.Zone) (TransferResult, error) {
	if !isStub(z) {
		return r.tr.Transfer(ctx, z)
	}
	if r.sf == nil {
		// Recorded as a failed attempt rather than skipped, so it lands in
		// last_error where somebody asking why the zone is not routing will
		// find it. A stub that were silently skipped would claim its suffix
		// and SERVFAIL with nothing anywhere saying why.
		return TransferResult{}, fmt.Errorf("zone %q is a stub and this scheduler was built with no stub fetcher", z.Name)
	}
	res, err := r.sf.Fetch(ctx, z)
	if err != nil {
		return TransferResult{}, err
	}
	return TransferResult{
		Primary:     res.Master,
		Serial:      res.Serial,
		Records:     res.Records,
		RefreshedAt: res.RefreshedAt,
	}, nil
}

// transfer runs one attempt and records what it did. Its caller holds
// st.xfer.
//
// It is the single funnel every outcome passes through, which is why the
// durable record of an attempt (zones.last_error / last_attempt) is written
// from here rather than from inside Transferrer.Transfer or
// StubFetcher.Fetch. Their job is to fetch and install a zone; deciding that
// an attempt happened, and that this one was the latest, is scheduling, and
// it belongs with the rest of the scheduling state this type already keeps.
func (r *Refresher) transfer(ctx context.Context, z store.Zone, st *zoneState) (TransferResult, error) {
	res, err := r.pull(ctx, z)
	nowMs := r.now().UnixMilli()
	if err != nil {
		if ctx.Err() != nil {
			// The attempt was cut short by shutdown, not by the primary.
			// Counting it would log a failure on every stop and push the
			// zone's next attempt a retry interval into a process that no
			// longer exists — and, now that the outcome is persisted, would
			// leave "connection closed" sitting in the database as this
			// zone's last known state across the whole downtime.
			slog.Debug("zone transfer abandoned", "zone", z.Name, "err", err)
			return TransferResult{}, err
		}
		r.noteFailure(ctx, z, st, nowMs, err)
		return TransferResult{}, err
	}
	r.noteSuccess(ctx, z, st, nowMs, res)
	return res, nil
}

// recordAttempt persists how an attempt went: errText is "" for a success.
//
// Best-effort, and logged rather than returned. The transfer itself has
// already happened by the time this runs — for a success the records are
// committed and the zone is serving them — so a failure to write the
// bookkeeping must not turn a transfer that worked into one that reports as
// failed. What it costs when it fails is one stale pair of columns, which the
// next attempt overwrites.
//
// The context is stripped of cancellation for the same reason the install's
// is: the caller going away (a dashboard tab closed during a manual refresh)
// must not abandon a local write of what already happened.
func (r *Refresher) recordAttempt(ctx context.Context, z store.Zone, at int64, errText string) {
	if err := r.zs.NoteTransferAttempt(context.WithoutCancel(ctx), z.ID, at, errText); err != nil {
		slog.Warn("recording a zone transfer attempt failed", "zone", z.Name, "err", err)
	}
}

// due reports whether z may be transferred now.
//
// The two clauses are different questions. notBefore is this process's own
// back-off — a failure to retry, or a place in the startup spread — and holds
// regardless of what the SOA says. Past it, the schedule is the SOA's, read
// off refreshed_at rather than off anything remembered here, so a transfer
// that happened through any other path (a manual Refresh, another process
// sharing the database) counts as the refresh it was.
func (r *Refresher) due(z store.Zone, nowMs int64, st *zoneState) bool {
	r.mu.Lock()
	notBefore := st.notBefore
	r.mu.Unlock()
	if nowMs < notBefore {
		return false
	}
	if z.RefreshedAt == 0 {
		// Never transferred: the zone is answering SERVFAIL to everything
		// (Zone.Serving) and has been since it was created.
		return true
	}
	return nowMs >= z.RefreshedAt+intervalMs(z.SOARefresh)
}

// stateFor returns the scheduler's state for z, creating it on first sight —
// which is where a zone that fell due while this process was down gets its
// place in the startup spread.
//
// State is never dropped, not even for a zone that has been deleted. The
// entry is small, and the alternative is worse than the leak: st.xfer is the
// identity that makes two transfers of one zone exclusive, so dropping an
// entry while something holds that lock would let the next caller build a
// second one and transfer alongside it — reintroducing, in a rare path, the
// exact corruption the lock exists to prevent.
func (r *Refresher) stateFor(z store.Zone, nowMs int64) *zoneState {
	r.mu.Lock()
	defer r.mu.Unlock()
	if st, ok := r.zones[z.ID]; ok {
		return st
	}
	st := &zoneState{notBefore: r.firstAttempt(z, nowMs)}
	r.zones[z.ID] = st
	return st
}

// firstAttempt decides when a zone this process has not seen before may be
// transferred.
//
// Two things are in tension. A zone whose refresh elapsed while the process
// was down should be brought up to date promptly — that is the whole point of
// restarting. But every secondary transferring the instant the process starts
// is a thundering herd against the primaries, and it is self-inflicted and
// perfectly synchronised: a crash loop would hammer them once per restart.
//
// The tie-break is what the zone can do meanwhile. A zone that is still
// serving valid data loses nothing by waiting up to a minute, so it gets a
// random place in that minute. A zone that is answering nothing — never
// transferred, or expired — loses a minute of service, so it goes now.
// Neither is a compromise: they are the same rule, which is that the delay is
// paid by whoever it costs least.
//
// The spread is also never allowed to run past the zone's own expiry, so
// politeness can never be what takes a zone off the air.
func (r *Refresher) firstAttempt(z store.Zone, nowMs int64) int64 {
	// The answering rule and the scheduling rule are the same rule, so it is
	// asked once, of the function that owns it.
	if !(&Zone{Zone: z}).Serving(nowMs) {
		return 0
	}
	// The same rule for the one zone Serving does not speak for. A stub always
	// serves, because it holds no data that could go stale (§9.11.8) — but one
	// that has never fetched holds no delegation either, so it claims its
	// suffix with no addresses behind it and SERVFAILs every name under it.
	// That is a zone answering nothing, reached by the other route, and it is
	// owed the same immediacy: nothing to lose by fetching now, a whole suffix
	// to lose by waiting out a spread.
	//
	// Named by type rather than left as a bare RefreshedAt == 0, which is the
	// same behaviour today — Serving has already returned for the only other
	// type here — and would silently hand "no startup spread" to whatever type
	// joins pullsFromAMaster next. Whether a new type has anything to lose by
	// waiting is a decision for whoever adds it, not one to inherit.
	if isStub(z) && z.RefreshedAt == 0 {
		return 0
	}
	if nowMs < z.RefreshedAt+intervalMs(z.SOARefresh) {
		// Not overdue at all — the ordinary schedule already says when, and a
		// spread on top of it would only ever delay it further.
		return 0
	}
	spread := startupSpread
	if z.ExpiresAt != 0 {
		if left := time.Duration(z.ExpiresAt-nowMs) * time.Millisecond; left < spread {
			spread = left
		}
	}
	if spread <= 0 {
		return 0
	}
	return nowMs + r.jitter(spread).Milliseconds()
}

// noteSuccess records a completed transfer. It clears the back-off rather
// than setting a next time: refreshed_at has just moved, and due() reads it.
//
// It clears last_error too, and that is not incidental: a column only ever
// written when something breaks becomes a permanent tombstone of a problem
// that was fixed weeks ago. Empty means "the last attempt succeeded".
func (r *Refresher) noteSuccess(ctx context.Context, z store.Zone, st *zoneState, nowMs int64, res TransferResult) {
	r.recordAttempt(ctx, z, nowMs, "")
	r.mu.Lock()
	fails := st.fails
	st.fails = 0
	st.lastErr = ""
	st.lastAttempt = nowMs
	st.notBefore = 0
	st.expiredLogged = false
	r.mu.Unlock()

	if fails > 0 {
		// The other half of the first-failure warning. Without it, an outage
		// has a beginning in the log and no end.
		slog.Info("zone transfer recovered",
			"zone", z.Name, "primary", res.Primary.String(), "serial", res.Serial,
			"records", res.Records, "failed_attempts", fails)
		return
	}
	slog.Debug("zone transferred",
		"zone", z.Name, "primary", res.Primary.String(), "serial", res.Serial, "records", res.Records)
}

// noteFailure records a failed attempt and schedules the retry.
//
// The logging is the part worth explaining. A zone whose primary is
// unreachable is retried every `retry` seconds for as long as that lasts —
// days, if that is how long it lasts — and every attempt fails identically.
// Logged at one line per attempt, a single outage becomes thousands of
// entries that push everything else out of the log and say nothing the first
// one did not. So the first failure of a run is loud, the rest are debug, and
// one is promoted back to loud every failureLogEvery so a long outage stays
// visible with a count of what it has cost so far.
//
// The one thing that is always loud is the zone crossing its expiry, because
// that is not another attempt failing — it is the moment the zone stops
// answering, and it happens exactly once per outage.
func (r *Refresher) noteFailure(ctx context.Context, z store.Zone, st *zoneState, nowMs int64, err error) {
	// Persisted as well as remembered. The in-memory copy below is what the
	// scheduler decides the next attempt from; this is what survives a
	// restart, so that a zone which has been failing for a week does not come
	// back looking healthy until its next attempt lands.
	r.recordAttempt(ctx, z, nowMs, err.Error())
	r.mu.Lock()
	st.fails++
	st.lastErr = err.Error()
	st.lastAttempt = nowMs
	retry := intervalMs(z.SOARetry)
	st.notBefore = nowMs + retry
	fails := st.fails
	loud := fails == 1 || nowMs-st.lastFailLog >= failureLogEvery.Milliseconds()
	if loud {
		st.lastFailLog = nowMs
	}
	// Expiry is read off the zone row, which the last successful transfer
	// stamped; it is reported here because a failed attempt is the only event
	// that can carry a zone across it.
	//
	// A secondary only, because a secondary is the only type that expires.
	// A stub's own install never writes expires_at (§9.11.8: its NS set is
	// routing information, not data held on loan), but a row retyped from
	// secondary to stub still carries the column — and announcing that such a
	// zone "has stopped answering" would send an operator hunting an outage
	// that is not happening, while the zone goes on routing perfectly well.
	expired := isSecondary(z) && z.RefreshedAt != 0 && z.ExpiresAt != 0 && nowMs >= z.ExpiresAt
	announceExpiry := expired && !st.expiredLogged
	if announceExpiry {
		st.expiredLogged = true
	}
	r.mu.Unlock()

	if announceExpiry {
		slog.Error("secondary zone expired and has stopped answering",
			"zone", z.Name, "expired_at", time.UnixMilli(z.ExpiresAt).UTC(),
			"failed_attempts", fails, "err", err)
	}
	// A zone that is expired keeps being retried, and this is deliberate: it
	// is already answering nothing, so there is nothing left to protect by
	// giving up, and the moment the primary comes back the zone should come
	// back with it rather than waiting for someone to notice and press a
	// button. The cost of being wrong about that is one connection attempt
	// per retry interval to a server that is already known to be down.
	switch {
	case loud && !announceExpiry:
		slog.Warn("zone transfer failed",
			"zone", z.Name, "failed_attempts", fails,
			// .String(), not the Duration itself: the JSON handler renders a
			// time.Duration as its int64 nanosecond count, so "5m0s" would
			// reach an operator's log as 300000000000.
			"retry_in", (time.Duration(retry) * time.Millisecond).String(), "err", err)
	default:
		slog.Debug("zone transfer failed",
			"zone", z.Name, "failed_attempts", fails,
			"retry_in", (time.Duration(retry) * time.Millisecond).String(), "err", err)
	}
}

// warnIfClamped tells an operator, once per zone, that this server will not
// poll as fast as the zone's SOA asks. Silently ignoring the published value
// would leave someone comparing their primary's SOA against dnsaur's
// behaviour with no explanation for the difference.
func (r *Refresher) warnIfClamped(z store.Zone, st *zoneState) {
	if int64(z.SOARefresh)*1000 >= minInterval.Milliseconds() && int64(z.SOARetry)*1000 >= minInterval.Milliseconds() {
		return
	}
	r.mu.Lock()
	first := !st.clampLogged
	st.clampLogged = true
	r.mu.Unlock()
	if !first {
		return
	}
	// In seconds, because the two values it is there to be compared against
	// are seconds: an SOA's own units. Logged as the raw Duration it read
	// "soa_refresh=60 soa_retry=30 floor=60000000000", which invites exactly
	// the wrong conclusion about which number is larger.
	slog.Warn("zone's SOA asks to be polled faster than this server will poll",
		"zone", z.Name, "soa_refresh", z.SOARefresh, "soa_retry", z.SOARetry,
		"floor_seconds", int64(minInterval.Seconds()))
}

// Status reports what the scheduler knows about one zone's transfers, or
// false if it has not seen that zone yet. Everything it returns is the part
// of a zone's transfer state that has no column: when the next attempt is
// allowed, and what the last one that failed said.
func (r *Refresher) Status(zoneID int64) (RefreshStatus, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st, ok := r.zones[zoneID]
	if !ok {
		return RefreshStatus{}, false
	}
	s := RefreshStatus{Failures: st.fails, LastError: st.lastErr}
	if st.notBefore > 0 {
		s.NotBefore = time.UnixMilli(st.notBefore)
	}
	if st.lastAttempt > 0 {
		s.LastAttempt = time.UnixMilli(st.lastAttempt)
	}
	return s, true
}

// RefreshStatus is one zone's scheduling state. The times are zero when they
// have not happened: a zone the scheduler has seen but not yet attempted has
// no LastAttempt, and one with no back-off pending has no NotBefore.
type RefreshStatus struct {
	// NotBefore is the earliest the scheduler will attempt this zone again.
	// It is set only while a back-off is pending; the ordinary schedule is
	// refreshed_at + soa_refresh, which the zone row already says.
	NotBefore time.Time
	// LastAttempt is when this process last tried, successfully or not.
	LastAttempt time.Time
	// Failures counts consecutive failed attempts; a success resets it.
	Failures int
	// LastError is the most recent failure's message, cleared by a success.
	LastError string
}

// intervalMs converts an SOA timer in seconds to the milliseconds the
// scheduler waits, with minInterval as a floor. See minInterval.
func intervalMs(seconds uint32) int64 {
	ms := int64(seconds) * 1000
	if floor := minInterval.Milliseconds(); ms < floor {
		return floor
	}
	return ms
}

// isSecondary reports whether z holds another server's zone on loan. It is
// the one type that expires: see noteFailure.
func isSecondary(z store.Zone) bool { return strings.EqualFold(z.Type, "secondary") }

// isStub reports whether z names where to send its suffix's queries by
// fetching a delegation rather than by holding data.
func isStub(z store.Zone) bool { return strings.EqualFold(z.Type, "stub") }

// pullsFromAMaster reports whether a zone of this type is one this scheduler
// refreshes.
//
// A secondary pulls a whole zone by AXFR; a stub pulls only the apex SOA and
// NS by ordinary query. Both are "ask a master, on the SOA's schedule, and
// record how it went", which is what this scheduler is, so both belong in it
// rather than in two loops that would drift.
//
// Nothing else is pulled at all: a primary is authored here, and a forwarder
// names its upstreams outright in forward_to with nobody to ask.
//
// It takes the type rather than the row because the scheduler is not its only
// caller: NotifyServer.decideZone asks the same question of a zones.Zone from
// the served snapshot, and "which types have a master" is one rule with one
// answer. (api.pullsFromAMaster is the third copy of it, in the package that
// cannot import this one — its own comment says so.)
func pullsFromAMaster(zoneType string) bool {
	switch strings.ToLower(zoneType) {
	case "secondary", "stub":
		return true
	}
	return false
}
