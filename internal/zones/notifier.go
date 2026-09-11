package zones

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/aloks98/dnsaur/internal/store"
	"golang.org/x/sync/errgroup"
)

// The outbound half of NOTIFY: deciding who to tell when a zone this server
// owns has changed, and driving the durable queue that tracks it.
//
// **Pending-ness is derived, never stored.** zone_notifies (the 0012
// migration) carries no "pending" column. Whether a target needs telling is
// computed, every pass, by comparing the zone's current soa_serial against
// what that target last acknowledged (notified_serial). That is what makes
// the trigger self-healing: any code path that bumps a serial — the API, an
// import, auto-PTR, a transfer install, or one nobody has written yet — is
// picked up by the next pass, with no call site to forget. §9.4 names the
// failure this is the deliberate answer to: an automatic path that skips a
// rule the human path enforces. There is no enqueue call for a future
// mutation path to forget, and there must never be one.
//
// A round is the set of attempts made while a given serial is the one being
// chased (row.pending_serial). Attempts accumulate and back off within a
// round; giving up is scoped to that round, so the next serial bump — the
// next round — resets the count and tries the target again with no operator
// action.

// MaxNotifyAttempts is how many times one round is tried before it rests.
//
// Five, backing off 5s → 10 → 20 → 40 → 80: about two and a half minutes,
// against a secondary refresh interval measured in hours. The budget can be
// this small precisely because it is not load-bearing — the target's own
// refresh timer picks the zone up regardless, which is what keeps NOTIFY a
// delivery optimisation rather than a correctness dependency.
const MaxNotifyAttempts = 5

const notifyBaseBackoff = 5 * time.Second

// notifySendConcurrency is how many targets one pass may be sending to at
// once. See Pass.
const notifySendConcurrency = 8

// notifyTick is how often the pass runs without a Wake. It bounds how late a
// notify can be, not how often one happens, and it is also the coalescer: a
// burst of edits inside one tick is one round at the newest serial.
const notifyTick = 5 * time.Second

// Sender is the one message-sending step, taken as an interface so the pass
// can be tested without a socket. notifysend.go's udpSender is the real
// implementation, and NewNotifier defaults to it — WithNotifySender is for
// tests, so production never has to remember to supply one.
type Sender interface {
	Send(ctx context.Context, target NotifyTarget, zone string, key *store.TSIGKey) error
}

// Notifier decides who to tell when a zone changes, and drives the durable
// zone_notifies queue that tracks it. The structure mirrors Refresher: a Run
// that passes immediately then ticks, an injected clock, and — because the
// queue is durable — no process-local state at all beyond the clock, the
// sender and the wake channel.
type Notifier struct {
	zs   store.ZoneStore
	ns   store.NotifyStore
	keys TSIGKeys

	// now is the clock the pass is decided against, injected for the same
	// reason Refresher's is: a test that cannot move time can only test a
	// schedule by waiting for it.
	now    func() time.Time
	sender Sender

	// replicaTargets is the config-sync clause beside notify_to: §6 of
	// docs/superpowers/specs/2026-09-11-config-sync-design.md has every
	// primary zone tell every registered replica, so registering one does
	// not mean editing every zone. nil means notify_to is the whole list.
	replicaTargets ReplicaTargets

	// wake is buffered to exactly one, which is what makes Wake both
	// non-blocking and coalescing: a pending wake already in the channel
	// absorbs every further Wake until Run drains it.
	wake chan struct{}
}

// NotifyOption configures a Notifier at construction.
type NotifyOption func(*Notifier)

// WithNotifyNow replaces the clock the pass is decided against.
func WithNotifyNow(now func() time.Time) NotifyOption {
	return func(n *Notifier) { n.now = now }
}

// WithNotifyResolver sets the resolver a target named by hostname is looked
// up through at send time. nil (the default) means net.DefaultResolver;
// production passes dnsaur's own forwarder — see Lookup.
//
// It replaces the sender, so a caller passing WithNotifySender as well gets
// whichever came last — the two say the same thing about a different half of
// the same field.
func WithNotifyResolver(res Lookup) NotifyOption {
	return func(n *Notifier) { n.sender = newUDPSender(res) }
}

// WithNotifySender replaces how a NOTIFY is actually sent. Production never
// calls this — NewNotifier's default is the real Sender — it exists so a
// test can observe what would have gone on the wire without a socket.
func WithNotifySender(s Sender) NotifyOption {
	return func(n *Notifier) { n.sender = s }
}

// ReplicaTargets returns the NOTIFY targets to add to every primary zone.
// internal/app answers it from the replica registry: one target per
// registered, non-stale replica, signed with the designated sync key.
//
// It is asked once per pass rather than once per zone — the answer is the
// same for every zone, and a pass is the unit the rest of this file already
// takes its snapshots in.
type ReplicaTargets func(ctx context.Context) []NotifyTarget

// WithReplicaTargets installs that clause. Production passes it on a main;
// without it a zone tells exactly what its notify_to names.
func WithReplicaTargets(f ReplicaTargets) NotifyOption {
	return func(n *Notifier) { n.replicaTargets = f }
}

// NewNotifier returns a Notifier that reads zones from zs, tracks delivery
// in ns, and resolves per-target signing keys through keys.
func NewNotifier(zs store.ZoneStore, ns store.NotifyStore, keys TSIGKeys, opts ...NotifyOption) *Notifier {
	n := &Notifier{
		zs:     zs,
		ns:     ns,
		keys:   keys,
		now:    time.Now,
		sender: newUDPSender(nil),
		wake:   make(chan struct{}, 1),
	}
	for _, opt := range opts {
		opt(n)
	}
	return n
}

// Run drives the pass until ctx is cancelled, starting with one pass
// immediately — Refresher.Run's shape, and for the same reason: the first
// thing this owes is whatever changed while the process was not running.
func (n *Notifier) Run(ctx context.Context) {
	t := time.NewTicker(notifyTick)
	defer t.Stop()
	n.pass(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n.pass(ctx)
		case <-n.wake:
			n.pass(ctx)
		}
	}
}

func (n *Notifier) pass(ctx context.Context) {
	if err := n.Pass(ctx); err != nil {
		if ctx.Err() != nil {
			// Shutting down. The pass failing because the process is going
			// away is not a fault to report as one.
			return
		}
		slog.Error("notify pass failed", "err", err)
	}
}

// Wake asks for a pass now rather than at the next tick.
//
// **It is promptness, never correctness, and that distinction is the
// design.** Whether there is anything to send is derived by comparing
// serials (see maybeSend), so a caller that forgets to Wake makes a notify
// late by one tick and cannot lose one. Do not turn this into a mandatory
// call on every mutation path: that is exactly the shape — an automatic path
// that has to remember a rule — that §9.4 records as this project's most
// repeated defect, and avoiding it is why the queue has no pending flag.
//
// Non-blocking, and coalescing: a thousand Wakes between two passes are one
// pass.
func (n *Notifier) Wake() {
	select {
	case n.wake <- struct{}{}:
	default:
	}
}

// Pass reconciles every zone's rows and sends whatever is due.
//
// **The queue is read first, reconciled second, and re-read only if
// something changed — and the *order of the last two* is a correctness
// requirement rather than tidiness.** Nothing may be sent against a snapshot
// taken before that zone's reconcile ran: maybeSend would get a zero-value
// row for a target Reconcile is about to insert, and a send decided against
// a row with no id can only fail to record itself — the send happens,
// NoteDelivered/NoteAttempt addresses `WHERE id = 0` and matches nothing,
// and because notified_at stays 0 the target looks "never told" again on the
// very next pass too.
//
// The read *before* the reconciles is what makes the reconciles skippable.
// Reconcile is a transaction, and this used to open one per enabled zone per
// tick whether or not that zone had anything to reconcile — every install
// carries the fifteen RFC 6303 built-ins, most configure no NOTIFY at all,
// so the steady state was fifteen-odd read-only transactions every five
// seconds discovering, each time, that there was nothing to do. Now one
// query answers that for every zone at once, and a zone whose rows already
// match its notify_to is not written to.
//
// It costs a second read of the queue on the passes that *do* reconcile, and
// that is the right way round: reconciling is rare (an operator edited a
// zone), and the alternative — trusting the pre-reconcile snapshot for the
// zones just written — is the `WHERE id = 0` bug above.
//
// A stale pre-read cannot cause a missed reconcile that matters. The
// notifier is the only writer of these rows; the only other way they change
// is a zone being deleted, which takes them with it (ON DELETE CASCADE), and
// a zone that no longer exists is not in `all` either.
//
// The error reported is a failure to *read* — the zone list, or the queue
// itself. A zone whose notify_to will not parse, or whose reconcile failed,
// is recorded against that zone and does not fail the pass, because one bad
// zone must not stop the others being told.
func (n *Notifier) Pass(ctx context.Context) error {
	all, err := n.zs.Zones(ctx)
	if err != nil {
		return err
	}
	nowMs := n.now().UnixMilli()
	// Once for the pass: the registered replicas are the same set for every
	// zone, and asking per zone would read the registry sixteen times to get
	// sixteen identical answers.
	var replicas []NotifyTarget
	if n.replicaTargets != nil {
		replicas = n.replicaTargets(ctx)
	}

	// One query, before anything is written: what every zone's rows are now.
	rowsByZone, err := n.rowsByZone(ctx)
	if err != nil {
		return err
	}

	parsed := make(map[int64][]NotifyTarget, len(all))
	reconciled := false
	for _, z := range all {
		// A disabled zone tells nobody, for the mirror of the reason
		// refresh.go skips one: it answers nothing (Zone.Serving), so this
		// server would REFUSE the transfer its own NOTIFY had just invited.
		// Telling a third party's secondary to come fetch a zone we will
		// then refuse is worse than silence — it is someone else's retry
		// loop.
		//
		// The rows are left in place rather than deleted — reconcile is
		// simply not called for this zone this pass — so delivery history
		// survives a disable/enable cycle, and re-enabling notifies only if
		// the serial actually moved while it was down. Same shape as
		// refresh.go's "puts it back in the ordinary schedule".
		if !z.Enabled {
			continue
		}
		// The same rule reached the other way, and asked of the function that
		// owns it. A secondary that has never transferred, or whose data has
		// expired, may not answer (Zone.Serving) and its own TransferServer
		// refuses the AXFR — so a NOTIFY from it is an invitation to a
		// SERVFAIL. It matters most at the one moment it is easiest to miss:
		// a secondary created through the API sits at the placeholder
		// soa_serial 1 with notify_to already set, and would otherwise notify
		// every target before it holds a single record.
		if !(&Zone{Zone: z}).Serving(nowMs) {
			continue
		}
		targets, err := ParseNotifyTo(z.NotifyTo)
		if err != nil {
			// Fails closed, and loudly enough to fix: an unparseable stored
			// value notifies nobody. The API validates on write, so reaching
			// this means a hand-edited row.
			slog.Warn("zone notify_to will not parse; notifying nobody",
				"zone", z.Name, "err", err)
			continue
		}
		// Before rowsMatch and reconcile, so a replica's target gets a
		// zone_notifies row like any other — tracked, backed off, and pruned
		// on the pass after the operator forgets the replica.
		targets = withReplicaTargets(targets, z, replicas)
		if rowsMatch(rowsByZone[z.ID], targets) {
			// Nothing to insert and nothing to delete: the overwhelmingly
			// common case, and the one that must not cost a transaction.
			parsed[z.ID] = targets
			continue
		}
		if err := n.reconcile(ctx, z, targets, nowMs); err != nil {
			slog.Warn("reconciling notify targets failed", "zone", z.Name, "err", err)
			continue
		}
		reconciled = true
		parsed[z.ID] = targets
	}

	// Re-read only when a row was actually written; see this function's own
	// comment for why sending against the earlier snapshot would not do.
	if reconciled {
		if rowsByZone, err = n.rowsByZone(ctx); err != nil {
			return err
		}
	}
	// The sends of one pass run concurrently, bounded, for the reason
	// Refresher.RefreshDue runs its zones concurrently: they are independent
	// — one socket and one row each — and the cost of one is a timeout
	// against a target that is up and not answering. Run one at a time, that
	// target's silence was paid for by everything behind it in the list, on
	// every pass, for as long as it stayed silent: a household with one
	// mothballed secondary delayed every other zone's notification by
	// seconds it had no reason to spend.
	//
	// Nothing about *what* is sent or recorded changes: maybeSend is called
	// once per deduped target with the same row and the same nowMs it would
	// have had, and its bookkeeping writes address one row each.
	//
	// The bound exists so a pass cannot open one socket per target on a
	// server with hundreds of them. It is a limit on the pathological case
	// rather than a tuning knob: a target that answers is done in a
	// millisecond, so the only thing that ever occupies a slot is a silent
	// one.
	g := new(errgroup.Group)
	g.SetLimit(notifySendConcurrency)
	for _, z := range all {
		// Deduped by address: ParseNotifyTo does not dedupe, and Reconcile
		// deliberately creates one row for a repeated target — so without
		// this both copies resolve to that row, pass the gates against the
		// same stale snapshot, and put two packets on the wire per pass. A
		// five-attempt round would cost ten sends.
		seen := make(map[string]bool, len(parsed[z.ID]))
		for _, t := range parsed[z.ID] {
			addr := t.Addr()
			if seen[addr] {
				continue
			}
			seen[addr] = true
			row, ok := rowsByZone[z.ID][addr]
			if !ok {
				// Reconcile just created it, so this means a concurrent
				// delete. Skipping is right: there is nothing to record
				// against, and the next pass will recreate it.
				continue
			}
			g.Go(func() error {
				n.maybeSend(ctx, z, t, row, nowMs)
				return nil
			})
		}
	}
	// maybeSend reports every outcome against its own row and returns
	// nothing, so there is no error to collect here — only the wait, which
	// is what keeps Pass meaning "the pass is over" for Wake and for Run.
	_ = g.Wait()
	return nil
}

// withReplicaTargets appends the registered replicas to a primary zone's own
// targets (§6). A secondary gets none: it holds another server's zone on
// loan, and the replica follows that zone from its own primary.
//
// Nothing is deduped here, deliberately. Addr is the row identity, and a
// repeated address is already one row (the store's Reconcile), one
// comparison (rowsMatch) and one packet (Pass's seen map) — the handling a
// notify_to naming an address twice has always had. A replica an operator
// also wrote into notify_to by hand is therefore told once, under the entry
// they wrote, because that one comes first.
func withReplicaTargets(targets []NotifyTarget, z store.Zone, replicas []NotifyTarget) []NotifyTarget {
	if !strings.EqualFold(z.Type, "primary") {
		return targets
	}
	return append(targets, replicas...)
}

// rowsMatch reports whether the rows a zone already has are exactly the ones
// its targets call for, in which case Reconcile would be a transaction that
// wrote nothing.
//
// Deduped by address on the way through, for the reason Reconcile itself
// dedupes: ParseNotifyTo does not, so `10.0.0.2, 10.0.0.2` is two targets and
// one row, and comparing the lengths without deduping would report a
// difference on every pass forever.
func rowsMatch(have map[string]store.ZoneNotify, targets []NotifyTarget) bool {
	want := make(map[string]bool, len(targets))
	for _, t := range targets {
		addr := t.Addr()
		if want[addr] {
			continue
		}
		want[addr] = true
		if _, ok := have[addr]; !ok {
			return false
		}
	}
	// Every wanted target has a row; the only remaining difference is a row
	// with no target, which is a delete Reconcile has to make.
	return len(want) == len(have)
}

// reconcile makes zone_notifies match z's current notify_to exactly.
func (n *Notifier) reconcile(ctx context.Context, z store.Zone, targets []NotifyTarget, nowMs int64) error {
	addrs := make([]string, len(targets))
	for i, t := range targets {
		addrs[i] = t.Addr()
	}
	return n.ns.Reconcile(ctx, z.ID, addrs, nowMs)
}

// rowsByZone reads the whole queue in one query and groups it by zone and
// target address — the row identity NotifyTarget.Addr defines. One read for
// the whole pass, mirroring why Resolver's reload reads AllRecords once
// rather than per zone.
func (n *Notifier) rowsByZone(ctx context.Context) (map[int64]map[string]store.ZoneNotify, error) {
	all, err := n.ns.All(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[int64]map[string]store.ZoneNotify, len(all))
	for _, r := range all {
		m, ok := out[r.ZoneID]
		if !ok {
			m = map[string]store.ZoneNotify{}
			out[r.ZoneID] = m
		}
		m[r.Target] = r
	}
	return out, nil
}

// maybeSend applies the pass's decision to one target and sends when it says
// to:
//
//	want := zone.soa_serial
//	if notified_at != 0 && !SerialNewer(want, notified_serial) { continue } // nothing to say
//	attempts := row.attempts
//	if row.pending_serial != want { attempts = 0 }                          // a new round
//	if attempts >= MaxNotifyAttempts { continue }                           // this round gave up
//	if now < row.next_attempt_at { continue }
//	send
//
// row is this pass's snapshot of the target's queue row, taken after every
// zone's reconcile has run — see Pass. For a target Reconcile has just
// inserted it is the zero value, which carries the same meaning a freshly
// inserted row does: never delivered, no round in flight, no back-off
// pending, so the decision above sends to it exactly as it would to a row
// read fresh from the store.
//
// The key is looked up by name through n.keys here, at send time, rather
// than stored on the row — so re-keying a target keeps its delivery history
// instead of orphaning it.
func (n *Notifier) maybeSend(ctx context.Context, z store.Zone, t NotifyTarget, row store.ZoneNotify, nowMs int64) {
	want := z.SOASerial
	if row.NotifiedAt != 0 && !SerialNewer(want, row.NotifiedSerial) {
		return // nothing to say
	}
	attempts := row.Attempts
	if row.PendingSerial != want {
		attempts = 0 // a new round
	}
	if attempts >= MaxNotifyAttempts {
		return // this round gave up
	}
	if nowMs < row.NextAttemptAt {
		return
	}

	key, err := n.lookupKey(ctx, t.Key)
	if err == nil {
		err = n.sender.Send(ctx, t, z.Name, key)
	}
	// The bookkeeping writes use a context stripped of cancellation, exactly
	// as Refresher.recordAttempt's do: the send has already happened — a
	// packet may already be on the wire, or NOTIFY already answered — by the
	// time this runs, so the caller going away (process shutdown mid-pass)
	// must not abandon the local record of what just happened.
	writeCtx := context.WithoutCancel(ctx)

	switch {
	case err == nil:
		if nerr := n.ns.NoteDelivered(writeCtx, row.ID, want, nowMs); nerr != nil {
			slog.Warn("recording a delivered notify failed",
				"zone", z.Name, "target", t.Addr(), "err", nerr)
		}
	case errors.Is(err, ErrNotifyDelivered), errors.Is(err, ErrNotifyUnreachable):
		// RFC 1996 §3.6/§4.8: the round is over either way — one because the
		// peer answered (whatever its rcode), the other because nothing is
		// there to answer. MaxNotifyAttempts is written directly, rather than
		// attempts+1, so this round is done: reusing the give-up marker is
		// what stops the next pass picking it back up without inventing a
		// fourth state. Still logged and still recorded to last_error — ending
		// the round and being satisfied with the outcome are different
		// things, and only the second one is what the operator sees.
		slog.Warn("notify send failed", "zone", z.Name, "target", t.Addr(), "err", err)
		if nerr := n.ns.NoteAttempt(writeCtx, row.ID, want, MaxNotifyAttempts, 0, err.Error()); nerr != nil {
			slog.Warn("recording a failed notify attempt failed",
				"zone", z.Name, "target", t.Addr(), "err", nerr)
		}
	default:
		// Logged here, not just on a bookkeeping failure: a send that fails
		// writes last_error to the row and would otherwise say nothing in
		// the log at all. refresh.go's failures get exactly this treatment,
		// for the reason its own comment gives — an outage needs a
		// beginning in the log. The round budget caps this at
		// MaxNotifyAttempts lines per round, so there is no unbounded-outage
		// case here to throttle the way refresh.go's retry-forever does.
		slog.Warn("notify send failed", "zone", z.Name, "target", t.Addr(), "err", err)
		if nerr := n.ns.NoteAttempt(writeCtx, row.ID, want, attempts+1,
			nowMs+backoffFor(attempts).Milliseconds(), err.Error()); nerr != nil {
			slog.Warn("recording a failed notify attempt failed",
				"zone", z.Name, "target", t.Addr(), "err", nerr)
		}
	}
}

// lookupKey resolves a target's TSIG key by name, or returns (nil, nil) for
// an unsigned target. A name that does not resolve to an existing key is
// reported as an error — the same as a send that failed on the wire — so it
// takes part in the same round-and-backoff accounting rather than being
// silently skipped or retried with no back-off at all.
func (n *Notifier) lookupKey(ctx context.Context, name string) (*store.TSIGKey, error) {
	if name == "" {
		return nil, nil
	}
	// Mirrors Transferrer.zoneKey's guard: a nil key store is a wiring bug,
	// and failing this one attempt is the answer to it, not a panic inside
	// Run's goroutine.
	if n.keys == nil {
		return nil, fmt.Errorf("notify target names TSIG key %q but no key store is attached", name)
	}
	k, found, err := n.keys.ByName(ctx, name)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("TSIG key %q not found", name)
	}
	return &k, nil
}

// backoffFor returns the delay before the next attempt within a round, given
// how many attempts it has already had. Doubling from notifyBaseBackoff: 5s,
// 10s, 20s, 40s, 80s for attempts 0..4 — the fifth failure is the one that
// reaches MaxNotifyAttempts and ends the round, so this is never called with
// an attempts value that would shift past it.
func backoffFor(attempts int) time.Duration {
	return notifyBaseBackoff * time.Duration(1<<attempts)
}
