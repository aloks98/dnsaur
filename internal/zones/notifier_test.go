package zones_test

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aloks98/dnsaur/internal/store"
	"github.com/aloks98/dnsaur/internal/zones"
	"github.com/miekg/dns"
)

// notifySend is one NOTIFY as it would have gone on the wire: where, for
// which zone, and under which TSIG key ("" for unsigned).
type notifySend struct{ target, zone, key string }

// fakeSender records what would have gone on the wire and fails on demand.
type fakeSender struct {
	mu   sync.Mutex
	sent []notifySend
	err  error
}

func (f *fakeSender) Send(_ context.Context, target zones.NotifyTarget, zone string, key *store.TSIGKey) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	name := ""
	if key != nil {
		name = key.Name
	}
	f.sent = append(f.sent, notifySend{target: target.Addr(), zone: zone, key: name})
	return nil
}

func (f *fakeSender) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}

func (f *fakeSender) sends() []notifySend {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.sent)
}

// newNotifierFixture builds a Notifier over a real store with one primary
// zone carrying notifyTo, and an injected clock the test advances.
type notifierFixture struct {
	n      *zones.Notifier
	st     store.Store
	sender *fakeSender
	zoneID int64
	clock  time.Time
}

func newNotifierFixture(t *testing.T, notifyTo string, serial uint32, opts ...zones.NotifyOption) *notifierFixture {
	t.Helper()
	ctx := context.Background()
	st := openTestStore(t)
	z := store.Zone{
		Name: notifyApex, Type: "primary", Enabled: true, NotifyTo: notifyTo,
		SOANS: "ns1." + notifyApex, SOAMbox: "hostmaster." + notifyApex,
		SOASerial: serial, SOARefresh: 3600, SOARetry: 600,
		SOAExpire: 604800, SOAMinimum: 300, SOATTL: 900,
	}
	id, err := st.Zones().AddZone(ctx, z)
	if err != nil {
		t.Fatalf("AddZone: %v", err)
	}
	f := &notifierFixture{
		st: st, sender: &fakeSender{}, zoneID: id,
		clock: time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC),
	}
	f.n = zones.NewNotifier(st.Zones(), st.Notifies(), st.TSIGKeys(),
		append([]zones.NotifyOption{
			zones.WithNotifyNow(func() time.Time { return f.clock }),
			zones.WithNotifySender(f.sender),
		}, opts...)...)
	return f
}

func (f *notifierFixture) pass(t *testing.T) {
	t.Helper()
	if err := f.n.Pass(context.Background()); err != nil {
		t.Fatalf("Pass: %v", err)
	}
}

func (f *notifierFixture) rows(t *testing.T) []store.ZoneNotify {
	t.Helper()
	rows, err := f.st.Notifies().ByZone(context.Background(), f.zoneID)
	if err != nil {
		t.Fatalf("ByZone: %v", err)
	}
	return rows
}

func (f *notifierFixture) setSerial(t *testing.T, serial uint32) {
	t.Helper()
	z, err := f.st.Zones().Zone(context.Background(), f.zoneID)
	if err != nil {
		t.Fatalf("Zone: %v", err)
	}
	z.SOASerial = serial
	if err := f.st.Zones().UpdateZone(context.Background(), z); err != nil {
		t.Fatalf("UpdateZone: %v", err)
	}
}

// The pass creates rows for targets that have none and removes rows whose
// target has left the list, so nothing has to hook zone PATCH and a
// hand-edited notify_to converges on the next pass.
func TestNotifierPassReconcilesRows(t *testing.T) {
	f := newNotifierFixture(t, "10.0.0.2:53, 10.0.0.3:53", 10)
	f.pass(t)
	if got := len(f.rows(t)); got != 2 {
		t.Fatalf("got %d rows, want 2", got)
	}

	z, _ := f.st.Zones().Zone(context.Background(), f.zoneID)
	z.NotifyTo = "10.0.0.3:53, 10.0.0.4:53"
	if err := f.st.Zones().UpdateZone(context.Background(), z); err != nil {
		t.Fatalf("UpdateZone: %v", err)
	}
	f.pass(t)

	got := map[string]bool{}
	for _, r := range f.rows(t) {
		got[r.Target] = true
	}
	if got["10.0.0.2:53"] || !got["10.0.0.3:53"] || !got["10.0.0.4:53"] {
		t.Errorf("rows = %v, want 10.0.0.3:53 and 10.0.0.4:53 only", got)
	}
}

// A new target is told at the current serial, because notified_at == 0 is
// "never told" — adding a secondary tells it rather than leaving it silent
// until the next unrelated edit.
func TestNotifierTellsANewTargetAtTheCurrentSerial(t *testing.T) {
	f := newNotifierFixture(t, "10.0.0.2:53", 47)
	f.pass(t)
	if f.sender.count() != 1 {
		t.Fatalf("sent %d notifies, want 1", f.sender.count())
	}
	rows := f.rows(t)
	if rows[0].NotifiedSerial != 47 || rows[0].NotifiedAt == 0 {
		t.Errorf("row = %+v, want delivered at serial 47", rows[0])
	}
}

// Nothing fires at startup or on a repeat pass: every row already records
// delivery at the current serial, so a restart is not news.
func TestNotifierIsQuietWhenNothingChanged(t *testing.T) {
	f := newNotifierFixture(t, "10.0.0.2:53", 47)
	f.pass(t)
	before := f.sender.count()
	f.pass(t)
	f.pass(t)
	if got := f.sender.count(); got != before {
		t.Errorf("sent %d notifies over three passes, want %d", got, before)
	}
}

// The trigger is serial detection, so a serial bumped by *any* path — here,
// written straight to the store with no notifier call at all — is picked up.
// This is the property that makes a forgotten call site a delay rather than
// a lost notify.
func TestNotifierNoticesASerialBumpNobodyAnnounced(t *testing.T) {
	f := newNotifierFixture(t, "10.0.0.2:53", 47)
	f.pass(t)
	f.setSerial(t, 48)
	f.pass(t)
	if got := f.sender.count(); got != 2 {
		t.Errorf("sent %d notifies, want 2 — the second serial was missed", got)
	}
}

// A round is defined by its serial. Attempts accumulate within it, back off,
// and stop at the budget.
func TestNotifierRetriesThenRestsWithinARound(t *testing.T) {
	f := newNotifierFixture(t, "10.0.0.2:53", 47)
	f.sender.err = errors.New("i/o timeout")

	// The first attempt fails and schedules the next.
	f.pass(t)
	rows := f.rows(t)
	if rows[0].Attempts != 1 || rows[0].LastError == "" {
		t.Fatalf("after one failure: %+v", rows[0])
	}
	if rows[0].NextAttemptAt <= f.clock.UnixMilli() {
		t.Errorf("next_attempt_at = %d, want it in the future", rows[0].NextAttemptAt)
	}

	// A pass before next_attempt_at does nothing at all.
	attemptsBefore := rows[0].Attempts
	f.pass(t)
	if got := f.rows(t)[0].Attempts; got != attemptsBefore {
		t.Errorf("attempts = %d before the backoff elapsed, want %d", got, attemptsBefore)
	}

	// Drive it to the budget.
	for i := 0; i < zones.MaxNotifyAttempts+2; i++ {
		f.clock = f.clock.Add(10 * time.Minute)
		f.pass(t)
	}
	if got := f.rows(t)[0].Attempts; got != zones.MaxNotifyAttempts {
		t.Errorf("attempts = %d, want it to stop at %d", got, zones.MaxNotifyAttempts)
	}
	if got := f.rows(t)[0].NotifiedAt; got != 0 {
		t.Errorf("notified_at = %d, want 0 — nothing was ever delivered", got)
	}
}

// Giving up is per round, not per target: the next serial bump resets the
// count and tries again, so a secondary that was down for an hour is retried
// the moment there is news, with no operator action.
func TestNotifierGiveUpResetsOnTheNextSerial(t *testing.T) {
	f := newNotifierFixture(t, "10.0.0.2:53", 47)
	f.sender.err = errors.New("i/o timeout")
	for i := 0; i < zones.MaxNotifyAttempts+2; i++ {
		f.clock = f.clock.Add(10 * time.Minute)
		f.pass(t)
	}
	exhausted := f.sender.count()
	if f.rows(t)[0].Attempts != zones.MaxNotifyAttempts {
		t.Fatalf("the round did not exhaust: %+v", f.rows(t)[0])
	}

	// News. The round resets and the target is tried again.
	f.sender.err = nil
	f.setSerial(t, 48)
	f.pass(t)
	if got := f.sender.count(); got != exhausted+1 {
		t.Errorf("sent %d after the serial advanced, want %d", got, exhausted+1)
	}
	rows := f.rows(t)
	if rows[0].NotifiedSerial != 48 || rows[0].Attempts != 0 || rows[0].LastError != "" {
		t.Errorf("row = %+v, want a clean delivery at serial 48", rows[0])
	}
}

// A zone with no targets does nothing, which is the overwhelmingly common
// case and must cost nothing.
func TestNotifierIgnoresZonesWithNoTargets(t *testing.T) {
	f := newNotifierFixture(t, "", 47)
	f.pass(t)
	if f.sender.count() != 0 {
		t.Errorf("sent %d notifies for a zone with no targets", f.sender.count())
	}
	if got := len(f.rows(t)); got != 0 {
		t.Errorf("created %d rows for a zone with no targets", got)
	}
}

// An unparseable stored notify_to notifies nobody rather than erroring the
// whole pass — one bad zone must not stop every other zone being told.
func TestNotifierFailsClosedOnAnUnparseableList(t *testing.T) {
	f := newNotifierFixture(t, "10.0.0.2:53", 47)
	z, _ := f.st.Zones().Zone(context.Background(), f.zoneID)
	z.NotifyTo = "10.0.0.2 key:bad name"
	if err := f.st.Zones().UpdateZone(context.Background(), z); err != nil {
		t.Fatalf("UpdateZone: %v", err)
	}
	if err := f.n.Pass(context.Background()); err != nil {
		t.Fatalf("Pass returned %v, want nil — one bad zone must not fail the pass", err)
	}
	if f.sender.count() != 0 {
		t.Errorf("sent %d notifies from an unparseable list", f.sender.count())
	}
}

// A secondary that has never transferred, or whose data has expired, is the
// disabled zone's twin: Zone.Serving says it may not answer, and its own
// TransferServer refuses the AXFR its NOTIFY would invite. A freshly created
// secondary sits at the placeholder soa_serial 1, so without this it notifies
// every target the moment it is created and invites a transfer that SERVFAILs.
func TestNotifierSkipsAZoneThatIsNotServing(t *testing.T) {
	ctx := context.Background()
	f := newNotifierFixture(t, "10.0.0.2:53", 1)

	// A secondary as the API creates one: serial 1, nothing transferred.
	z, _ := f.st.Zones().Zone(ctx, f.zoneID)
	z.Type = "secondary"
	z.Primaries = "10.0.0.9"
	z.RefreshedAt = 0
	if err := f.st.Zones().UpdateZone(ctx, z); err != nil {
		t.Fatalf("UpdateZone: %v", err)
	}
	f.pass(t)
	if got := f.sender.count(); got != 0 {
		t.Errorf("a never-transferred secondary sent %d notifies, want 0", got)
	}

	// Expired is the same state reached the other way.
	z, _ = f.st.Zones().Zone(ctx, f.zoneID)
	z.RefreshedAt = f.clock.Add(-2 * time.Hour).UnixMilli()
	z.ExpiresAt = f.clock.Add(-time.Hour).UnixMilli()
	if err := f.st.Zones().UpdateZone(ctx, z); err != nil {
		t.Fatalf("UpdateZone: %v", err)
	}
	f.pass(t)
	if got := f.sender.count(); got != 0 {
		t.Errorf("an expired secondary sent %d notifies, want 0", got)
	}

	// And a transfer that lands is what lets it speak: the zone is serving
	// data it can vouch for, so a target asking for it is answered.
	z, _ = f.st.Zones().Zone(ctx, f.zoneID)
	z.RefreshedAt = f.clock.UnixMilli()
	z.ExpiresAt = f.clock.Add(time.Hour).UnixMilli()
	z.SOASerial = 47
	if err := f.st.Zones().UpdateZone(ctx, z); err != nil {
		t.Fatalf("UpdateZone: %v", err)
	}
	f.pass(t)
	if got := f.sender.count(); got != 1 {
		t.Errorf("a secondary serving its primary's zone sent %d notifies, want 1", got)
	}
}

// A disabled zone answers nothing (Zone.Serving), so its own NOTIFY would
// invite a transfer this server would REFUSE — handing a third party's
// secondary a retry loop. The mirror of refresh.go's disabled-zone skip.
//
// The rows are left in place rather than deleted, which is what re-enabling
// proves: it delivers exactly the one round of news that piled up while the
// zone was down, not a blast repeated on every pass after — because
// pending-ness is still derived from the same serial comparison it always
// is, against history that was never disturbed.
func TestNotifierSkipsDisabledZones(t *testing.T) {
	f := newNotifierFixture(t, "10.0.0.2:53", 47)
	f.pass(t)
	if got := f.sender.count(); got != 1 {
		t.Fatalf("sent %d notifies before disabling, want 1", got)
	}
	rowsBefore := f.rows(t)

	// Disabled, with news piling up while it's down.
	z, _ := f.st.Zones().Zone(context.Background(), f.zoneID)
	z.Enabled = false
	z.SOASerial = 48
	if err := f.st.Zones().UpdateZone(context.Background(), z); err != nil {
		t.Fatalf("UpdateZone: %v", err)
	}
	f.pass(t)
	f.pass(t)
	if got := f.sender.count(); got != 1 {
		t.Errorf("sent %d notifies while disabled, want 1 — no more", got)
	}
	if got := f.rows(t); len(got) != 1 || got[0] != rowsBefore[0] {
		t.Errorf("rows changed while disabled: got %+v, want unchanged %+v", got, rowsBefore)
	}

	// Re-enable. Exactly one more send lands, for the serial that piled up.
	z, _ = f.st.Zones().Zone(context.Background(), f.zoneID)
	z.Enabled = true
	if err := f.st.Zones().UpdateZone(context.Background(), z); err != nil {
		t.Fatalf("UpdateZone: %v", err)
	}
	f.pass(t)
	if got := f.sender.count(); got != 2 {
		t.Errorf("sent %d notifies after re-enabling, want 2", got)
	}

	// Not a blast: further passes stay quiet, exactly as any other target
	// current for its serial does.
	f.pass(t)
	f.pass(t)
	if got := f.sender.count(); got != 2 {
		t.Errorf("sent %d notifies after further passes, want 2 — quiet once caught up", got)
	}
}

// A send that fails writes last_error to the row, but that is not the same
// as being seen: without a log line, the failure is visible only to someone
// who goes looking at that one row. refresh.go's failures get exactly this
// treatment, for the reason its own comment gives — an outage needs a
// beginning in the log.
func TestNotifierLogsAFailedSend(t *testing.T) {
	f := newNotifierFixture(t, "10.0.0.2:53", 47)
	f.sender.err = errors.New("i/o timeout")
	logs := captureLogs(t)

	f.pass(t)

	found := false
	for _, r := range logs() {
		if r.Level >= slog.LevelWarn && strings.Contains(r.Message, "notify send failed") {
			found = true
		}
	}
	if !found {
		t.Error("a failed send produced no warning log line")
	}
}

// Wake makes the next pass happen now rather than at the tick, and is
// non-blocking however many times it is called.
func TestWakeIsNonBlockingAndCoalesces(t *testing.T) {
	f := newNotifierFixture(t, "10.0.0.2:53", 47)
	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			f.n.Wake()
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Wake blocked")
	}
}

// RFC 1996 §3.6/§4.8, exercised end to end through NewNotifier's real
// default Sender (notifysend.go's udpSender) rather than fakeSender: a
// response ends the round whatever its rcode, so a REFUSED target is
// recorded — attempts jumps straight to MaxNotifyAttempts and last_error
// names the rcode — and is not retried on the following pass, exactly as
// maybeSend's ErrNotifyDelivered branch documents.
func TestNotifierDoesNotRetryARefusedTarget(t *testing.T) {
	r := newNotifyResponder(t, dns.RcodeRefused, false)
	ctx := context.Background()
	st := openTestStore(t)
	z := store.Zone{
		Name: notifyApex, Type: "primary", Enabled: true, NotifyTo: r.addr,
		SOANS: "ns1." + notifyApex, SOAMbox: "hostmaster." + notifyApex,
		SOASerial: 47, SOARefresh: 3600, SOARetry: 600,
		SOAExpire: 604800, SOAMinimum: 300, SOATTL: 900,
	}
	zoneID, err := st.Zones().AddZone(ctx, z)
	if err != nil {
		t.Fatalf("AddZone: %v", err)
	}
	clock := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	// No WithNotifySender: this is NewNotifier's production default, the
	// real udpSender, put on the wire against a real UDP responder.
	n := zones.NewNotifier(st.Zones(), st.Notifies(), st.TSIGKeys(),
		zones.WithNotifyNow(func() time.Time { return clock }))

	if err := n.Pass(ctx); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if got := r.received.Load(); got != 1 {
		t.Fatalf("responder saw %d requests after one pass, want 1", got)
	}
	rows, err := st.Notifies().ByZone(ctx, zoneID)
	if err != nil {
		t.Fatalf("ByZone: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if rows[0].Attempts != zones.MaxNotifyAttempts {
		t.Errorf("attempts = %d, want %d — a response ends the round even when REFUSED",
			rows[0].Attempts, zones.MaxNotifyAttempts)
	}
	if rows[0].NotifiedAt != 0 {
		t.Errorf("notified_at = %d, want 0 — REFUSED is not a successful delivery", rows[0].NotifiedAt)
	}
	if !strings.Contains(rows[0].LastError, "REFUSED") {
		t.Errorf("last_error = %q, want it to name REFUSED", rows[0].LastError)
	}

	// A second pass, well past any backoff, must not retry: the round
	// already ended on the first response.
	clock = clock.Add(time.Hour)
	if err := n.Pass(ctx); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if got := r.received.Load(); got != 1 {
		t.Errorf("responder saw %d requests after a second pass, want 1 — a REFUSED target was retried", got)
	}
}

// countingNotifies is the real store with a tally of Reconcile calls. The
// embedded interface keeps it honest: every other method is the store's own,
// so the pass under test is the one that runs against real rows.
type countingNotifies struct {
	store.NotifyStore
	reconciles atomic.Int64
}

func (c *countingNotifies) Reconcile(ctx context.Context, zoneID int64, targets []string, now int64) error {
	c.reconciles.Add(1)
	return c.NotifyStore.Reconcile(ctx, zoneID, targets, now)
}

// Reconcile is a transaction — two statements that must land together — and
// the pass used to open one per enabled zone per tick, including for a zone
// with no targets at all. Every install has the fifteen RFC 6303 built-ins
// seeded (internal/store/builtins.go) and most have no NOTIFY configured
// anywhere, so the steady state was fifteen-odd read-only transactions every
// five seconds to discover, each time, that there was nothing to do.
//
// The pass reads the queue once up front and reconciles only the zones whose
// rows disagree with their notify_to. The store's Reconcile is unchanged:
// when it is called, it still does the whole thing atomically.
//
// The fixture is the real one — a store carrying its built-in zones — so
// "nothing to reconcile" is the shape an ordinary install actually has,
// rather than one zone in isolation.
func TestPassReconcilesOnlyTheZonesWhoseTargetsChanged(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	id, err := st.Zones().AddZone(ctx, store.Zone{
		Name: notifyApex, Type: "primary", Enabled: true,
		NotifyTo: "10.0.0.2:53, 10.0.0.3:53",
		SOANS:    "ns1." + notifyApex, SOAMbox: "hostmaster." + notifyApex,
		SOASerial: 10, SOARefresh: 3600, SOARetry: 600,
		SOAExpire: 604800, SOAMinimum: 300, SOATTL: 900,
	})
	if err != nil {
		t.Fatalf("AddZone: %v", err)
	}
	ns := &countingNotifies{NotifyStore: st.Notifies()}
	clock := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	n := zones.NewNotifier(st.Zones(), ns, st.TSIGKeys(),
		zones.WithNotifyNow(func() time.Time { return clock }),
		zones.WithNotifySender(&fakeSender{}))
	pass := func() {
		t.Helper()
		if err := n.Pass(ctx); err != nil {
			t.Fatalf("Pass: %v", err)
		}
	}
	targets := func() map[string]bool {
		t.Helper()
		rows, err := st.Notifies().ByZone(ctx, id)
		if err != nil {
			t.Fatalf("ByZone: %v", err)
		}
		out := map[string]bool{}
		for _, r := range rows {
			out[r.Target] = true
		}
		return out
	}

	// The first pass has real work: the two rows do not exist yet. One zone
	// has targets, so one reconcile — not sixteen.
	pass()
	if got := ns.reconciles.Load(); got != 1 {
		t.Fatalf("first pass reconciled %d zones, want 1 (the other fifteen have no targets)", got)
	}
	if got := targets(); len(got) != 2 {
		t.Fatalf("rows = %v, want the two targets", got)
	}

	// The steady state: every row already matches every notify_to.
	pass()
	pass()
	if got := ns.reconciles.Load(); got != 1 {
		t.Errorf("two further passes reconciled %d zones in total, want the original 1 — "+
			"a pass with nothing to reconcile must open no transaction", got)
	}

	// And the skip must not be "never reconcile": an edited list still
	// converges on the next pass.
	z, err := st.Zones().Zone(ctx, id)
	if err != nil {
		t.Fatalf("Zone: %v", err)
	}
	z.NotifyTo = "10.0.0.3:53, 10.0.0.9:53"
	if err := st.Zones().UpdateZone(ctx, z); err != nil {
		t.Fatalf("UpdateZone: %v", err)
	}
	pass()
	if got := ns.reconciles.Load(); got != 2 {
		t.Errorf("after an edit, %d reconciles in total, want 2", got)
	}
	if got := targets(); !got["10.0.0.3:53"] || !got["10.0.0.9:53"] || len(got) != 2 {
		t.Errorf("rows = %v, want exactly the edited list", got)
	}

	// Clearing the list is a change too, and the one a skip keyed on
	// "targets is empty" would get wrong: the rows have to go.
	z, err = st.Zones().Zone(ctx, id)
	if err != nil {
		t.Fatalf("Zone: %v", err)
	}
	z.NotifyTo = ""
	if err := st.Zones().UpdateZone(ctx, z); err != nil {
		t.Fatalf("UpdateZone: %v", err)
	}
	pass()
	if got := ns.reconciles.Load(); got != 3 {
		t.Errorf("after clearing notify_to, %d reconciles in total, want 3", got)
	}
	if got := targets(); len(got) != 0 {
		t.Errorf("rows = %v, want none: the zone notifies nobody now", got)
	}
	// ...and once they are gone it is back to nothing to do.
	pass()
	if got := ns.reconciles.Load(); got != 3 {
		t.Errorf("a pass after the rows were deleted reconciled again (%d in total, want 3)", got)
	}
}

// One pass, one packet per target, and no target waiting on another's
// timeout.
//
// The sends are independent — different sockets, different rows — and the
// only thing they share is the pass they happen in. Run one at a time, a
// target that is up and silent costs a full send timeout, and everything
// behind it in the list waits that out: two silent secondaries in front of a
// live one delayed every notification to that live one by four seconds, on
// every pass, for as long as they stayed silent.
//
// The real UDP sender, not the fake one, because the fake never blocks and so
// cannot tell a concurrent pass from a serial one.
func TestNotifyPassSendsToTargetsConcurrently(t *testing.T) {
	// oneTimeout mirrors testNotifySendTimeout (notifysend.go), which is what
	// NewUDPSenderForTest waits per address.
	const oneTimeout = time.Second

	silentA := newNotifyResponder(t, dns.RcodeSuccess, true)
	silentB := newNotifyResponder(t, dns.RcodeSuccess, true)
	// Last in the list on purpose: sent one at a time it is the one that pays
	// for both silences.
	live := newNotifyResponder(t, dns.RcodeSuccess, false)

	f := newNotifierFixture(t, strings.Join([]string{silentA.addr, silentB.addr, live.addr}, ", "), 10)
	n := zones.NewNotifier(f.st.Zones(), f.st.Notifies(), f.st.TSIGKeys(),
		zones.WithNotifyNow(func() time.Time { return f.clock }),
		zones.WithNotifySender(zones.NewUDPSenderForTest()))

	done := make(chan error, 1)
	go func() { done <- n.Pass(context.Background()) }()

	// Half a timeout: unreachable if the live target is queued behind two
	// silent ones, and a thousandfold more than it needs when it is not.
	deadline := time.Now().Add(oneTimeout / 2)
	for live.received.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("the live target had received nothing after %v; "+
				"it is waiting out the silent targets' timeouts", oneTimeout/2)
		}
		time.Sleep(5 * time.Millisecond)
	}

	if err := <-done; err != nil {
		t.Fatalf("Pass: %v", err)
	}

	// Concurrency is not permission to skip anyone: every target is still
	// tried once in the pass.
	for name, r := range map[string]*notifyResponder{"silentA": silentA, "silentB": silentB, "live": live} {
		if got := r.received.Load(); got != 1 {
			t.Errorf("%s received %d notifies, want 1", name, got)
		}
	}

	// And the per-target bookkeeping is what it was: the one that answered is
	// delivered at the zone's serial, the two that did not carry a first
	// attempt and the reason.
	for _, row := range f.rows(t) {
		switch row.Target {
		case live.addr:
			if row.NotifiedSerial != 10 || row.LastError != "" {
				t.Errorf("the live target's row = %+v, want serial 10 delivered and no error", row)
			}
		default:
			if row.Attempts != 1 || row.LastError == "" {
				t.Errorf("%s's row = %+v, want one recorded attempt and its error", row.Target, row)
			}
		}
	}
}

// syncTarget is a registered replica as the App hands one to the notifier:
// the address it answers DNS on, signed with the main's designated sync key.
var syncTarget = zones.NotifyTarget{
	Host: "10.0.0.6", Port: 53, Key: dns.CanonicalName("sync." + notifyApex),
}

// replicaTargets is the WithReplicaTargets hook, standing in for the App
// method that reads the replica registry.
func replicaTargets(ts ...zones.NotifyTarget) zones.NotifyOption {
	return zones.WithReplicaTargets(func(context.Context) ([]zones.NotifyTarget, error) { return ts, nil })
}

// addSecondary puts a serving secondary beside the fixture's primary, so a
// pass has one zone of each kind to decide about.
func addSecondary(t *testing.T, f *notifierFixture) {
	t.Helper()
	_, err := f.st.Zones().AddZone(context.Background(), store.Zone{
		Name: "loaned." + notifyApex, Type: "secondary", Enabled: true,
		Primaries: "10.0.0.9:53", SOANS: "ns1.loaned." + notifyApex,
		SOAMbox: "hostmaster.loaned." + notifyApex, SOASerial: 12,
		SOARefresh: 3600, SOARetry: 600, SOAExpire: 604800, SOAMinimum: 300, SOATTL: 900,
		RefreshedAt: f.clock.Add(-time.Hour).UnixMilli(),
		ExpiresAt:   f.clock.Add(time.Hour).UnixMilli(),
	})
	if err != nil {
		t.Fatalf("AddZone: %v", err)
	}
}

// §6 of docs/superpowers/specs/2026-09-11-config-sync-design.md: every
// primary zone notifies every registered replica, signed with the sync key,
// without the operator adding the replica to each zone's notify_to. A
// secondary is someone else's zone on loan and gets none.
func TestNotifierTellsRegisteredReplicasWithoutANotifyToEntry(t *testing.T) {
	f := newNotifierFixture(t, "", 47, replicaTargets(syncTarget))
	xfrKey(t, f.st, syncTarget.Key, dns.HmacSHA256)
	addSecondary(t, f)

	f.pass(t)

	sends := f.sender.sends()
	if len(sends) != 1 {
		t.Fatalf("sent %d notifies, want 1 — the replica for the primary zone only: %v", len(sends), sends)
	}
	if sends[0].target != "10.0.0.6:53" || sends[0].zone != notifyApex {
		t.Errorf("notified %s about %s, want 10.0.0.6:53 about %s", sends[0].target, sends[0].zone, notifyApex)
	}
	if sends[0].key != syncTarget.Key {
		t.Errorf("notify signed under %q, want the sync key %q", sends[0].key, syncTarget.Key)
	}
	// Delivery is tracked like any other target's, which is what prunes the
	// row when the operator forgets the replica.
	rows := f.rows(t)
	if len(rows) != 1 || rows[0].Target != "10.0.0.6:53" || rows[0].NotifiedSerial != 47 {
		t.Fatalf("rows = %+v, want one row for 10.0.0.6:53 delivered at serial 47", rows)
	}

	// And a replica the operator has removed loses its row on the next pass,
	// rather than being notified forever.
	f.n = zones.NewNotifier(f.st.Zones(), f.st.Notifies(), f.st.TSIGKeys(),
		zones.WithNotifyNow(func() time.Time { return f.clock }),
		zones.WithNotifySender(f.sender))
	f.pass(t)
	if got := len(f.rows(t)); got != 0 {
		t.Errorf("a forgotten replica left %d rows behind, want 0", got)
	}
}

// A replica the operator also wrote into notify_to by hand is one target,
// not two: the address is the row identity, so the implicit entry and the
// written one are the same target and the pass sends one packet.
func TestAReplicaAlreadyInNotifyToIsToldOnce(t *testing.T) {
	f := newNotifierFixture(t, "10.0.0.6:53", 47, replicaTargets(syncTarget))
	xfrKey(t, f.st, syncTarget.Key, dns.HmacSHA256)

	f.pass(t)

	if got := f.sender.count(); got != 1 {
		t.Errorf("sent %d notifies to one replica, want 1", got)
	}
	if got := len(f.rows(t)); got != 1 {
		t.Errorf("created %d rows for one replica, want 1", got)
	}
}

// A registry that will not answer is not "no replicas". Read that way, the
// pass would reconcile every primary zone against a list its replicas are
// missing from — deleting the rows their delivery history lives in, and
// re-notifying them from scratch when the registry came back. So the pass
// fails, exactly as a failed zone or queue read does, having written
// nothing.
func TestNotifierFailsThePassWhenTheReplicaRegistryFails(t *testing.T) {
	failing := false
	f := newNotifierFixture(t, "10.0.0.2:53", 47,
		zones.WithReplicaTargets(func(context.Context) ([]zones.NotifyTarget, error) {
			if failing {
				return nil, errors.New("sync.replicas is unreadable")
			}
			return []zones.NotifyTarget{syncTarget}, nil
		}))
	xfrKey(t, f.st, syncTarget.Key, dns.HmacSHA256)

	f.pass(t)
	if got := len(f.rows(t)); got != 2 {
		t.Fatalf("got %d rows, want the notify_to target and the replica", got)
	}
	if got := f.sender.count(); got != 2 {
		t.Fatalf("sent %d notifies, want 2", got)
	}

	failing = true
	if err := f.n.Pass(context.Background()); err == nil {
		// Not fatal: the rows below are the damage this guards against, and
		// a pass that swallowed the error should report both.
		t.Error("Pass returned nil with the registry failing: the failure was read as an empty list")
	}
	if got := len(f.rows(t)); got != 2 {
		t.Errorf("got %d rows after a failed registry read, want the 2 that were already there", got)
	}
	if got := f.sender.count(); got != 2 {
		t.Errorf("sent %d notifies in total, want the original 2", got)
	}
}
