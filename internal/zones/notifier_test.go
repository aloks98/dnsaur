package zones_test

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aloks98/dnsaur/internal/store"
	"github.com/aloks98/dnsaur/internal/zones"
	"github.com/miekg/dns"
)

// fakeSender records what would have gone on the wire and fails on demand.
type fakeSender struct {
	mu   sync.Mutex
	sent []string // "target|zone"
	err  error
}

func (f *fakeSender) Send(_ context.Context, target zones.NotifyTarget, zone string, _ *store.TSIGKey) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.sent = append(f.sent, target.Addr()+"|"+zone)
	return nil
}

func (f *fakeSender) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
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

func newNotifierFixture(t *testing.T, notifyTo string, serial uint32) *notifierFixture {
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
		zones.WithNotifyNow(func() time.Time { return f.clock }),
		zones.WithNotifySender(f.sender))
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
