package store

import "context"

// ZoneNotify is one target's delivery state, which is also its queue entry:
// the two are the same row because pending-ness is derived from the serial
// rather than stored. See the 0012 migration.
type ZoneNotify struct {
	ID     int64
	ZoneID int64
	// Target is host:port as written — 'ns2.example.com:5353' — and carries
	// no key. It is the row identity; see the migration for why it is not a
	// resolved address.
	Target string
	// PendingSerial is the round currently being attempted.
	PendingSerial uint32
	// NotifiedSerial and NotifiedAt are the last round that landed.
	// NotifiedAt is unix ms; 0 = never delivered.
	NotifiedSerial uint32
	NotifiedAt     int64
	// Attempts is the count within the current round, reset on success.
	Attempts      int
	NextAttemptAt int64
	// LastError is the most recent failure, cleared on success.
	LastError string
	// CreatedAt is when this target was first seen, and never moves after.
	// It dates a target that has never been notified.
	CreatedAt int64
}

// NotifyStore manages the outbound NOTIFY queue.
type NotifyStore interface {
	// All returns every row, for the notifier's pass.
	All(ctx context.Context) ([]ZoneNotify, error)
	ByZone(ctx context.Context, zoneID int64) ([]ZoneNotify, error)
	// Reconcile makes the rows for zoneID exactly targets: it inserts any
	// that have no row, stamping created_at with now, and deletes any whose
	// target is not in the list. Existing rows are left alone — re-running it
	// must not restamp created_at, or every pass would reset the age of every
	// target.
	Reconcile(ctx context.Context, zoneID int64, targets []string, now int64) error
	// NoteDelivered records a round that landed: the serial the target
	// acknowledged and when. It clears attempts, next_attempt_at and
	// last_error, so a target that recovered stops reporting a problem it no
	// longer has — the rule NoteTransferAttempt follows for last_error.
	//
	// Its column set is disjoint from NoteAttempt's by design; see that
	// method, and NoteTransferAttempt's doc comment for the whole reasoning.
	NoteDelivered(ctx context.Context, id int64, serial uint32, at int64) error
	// NoteAttempt records a round that did not land: which round, how many
	// attempts it has had, when the next one may be made, and why the last
	// one failed. It never touches notified_serial or notified_at — a target
	// current for a week must not read as never notified because one retry
	// timed out.
	NoteAttempt(ctx context.Context, id int64, pendingSerial uint32, attempts int, nextAttemptAt int64, errText string) error
}

type notifyStore struct{ s *sqlStore }

const notifyColumns = `id, zone_id, target, pending_serial, notified_serial, notified_at, attempts, next_attempt_at, last_error, created_at`

func scanNotify(rows interface{ Scan(...any) error }, n *ZoneNotify) error {
	return rows.Scan(&n.ID, &n.ZoneID, &n.Target, &n.PendingSerial, &n.NotifiedSerial,
		&n.NotifiedAt, &n.Attempts, &n.NextAttemptAt, &n.LastError, &n.CreatedAt)
}

func (n *notifyStore) All(ctx context.Context) ([]ZoneNotify, error) {
	return n.query(ctx, `SELECT `+notifyColumns+` FROM zone_notifies ORDER BY zone_id, target`)
}

func (n *notifyStore) ByZone(ctx context.Context, zoneID int64) ([]ZoneNotify, error) {
	return n.query(ctx, `SELECT `+notifyColumns+` FROM zone_notifies WHERE zone_id = ? ORDER BY target`, zoneID)
}

func (n *notifyStore) query(ctx context.Context, q string, args ...any) ([]ZoneNotify, error) {
	rows, err := n.s.db.QueryContext(ctx, n.s.q(q), args...)
	if err != nil {
		return nil, wrapDBErr(err)
	}
	defer rows.Close()
	var out []ZoneNotify
	for rows.Next() {
		var zn ZoneNotify
		if err := scanNotify(rows, &zn); err != nil {
			return nil, err
		}
		out = append(out, zn)
	}
	return out, rows.Err()
}

// Reconcile runs both halves in one transaction. Separately, a pass that
// crashed between them would leave a zone whose rows match neither its old
// notify_to nor its new one — and the delete half alone would drop delivery
// history that the insert half was about to keep.
func (n *notifyStore) Reconcile(ctx context.Context, zoneID int64, targets []string, now int64) error {
	tx, err := n.s.db.BeginTx(ctx, nil)
	if err != nil {
		return wrapDBErr(err)
	}
	defer func() { _ = tx.Rollback() }()

	// Delete first, and by reading back rather than with a NOT IN list: the
	// list is operator-supplied and unbounded, and a NOT IN of a thousand
	// placeholders is a query planner's problem for no gain at this size.
	rows, err := tx.QueryContext(ctx, n.s.q(`SELECT id, target FROM zone_notifies WHERE zone_id = ?`), zoneID)
	if err != nil {
		return wrapDBErr(err)
	}
	want := make(map[string]bool, len(targets))
	for _, t := range targets {
		want[t] = true
	}
	have := map[string]bool{}
	var stale []int64
	for rows.Next() {
		var id int64
		var target string
		if err := rows.Scan(&id, &target); err != nil {
			rows.Close()
			return wrapDBErr(err)
		}
		if want[target] {
			have[target] = true
		} else {
			stale = append(stale, id)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return wrapDBErr(err)
	}

	for _, id := range stale {
		if _, err := tx.ExecContext(ctx, n.s.q(`DELETE FROM zone_notifies WHERE id = ?`), id); err != nil {
			return wrapDBErr(err)
		}
	}
	for _, t := range targets {
		if have[t] {
			continue
		}
		if _, err := tx.ExecContext(ctx, n.s.q(
			`INSERT INTO zone_notifies (zone_id, target, created_at) VALUES (?, ?, ?)`),
			zoneID, t, now); err != nil {
			return wrapDBErr(err)
		}
		// Marked as present, so a targets list naming the same address twice
		// inserts it once instead of violating UNIQUE(zone_id, target) and
		// rolling back the whole call. ParseNotifyTo does not dedupe and
		// ValidateNotifyTo is only ParseNotifyTo, so `10.0.0.2, 10.0.0.2` is
		// an accepted write — and without this line it would fail every
		// notify pass for that zone from then on, reported in a pass log far
		// from the edit that caused it.
		have[t] = true
	}
	return tx.Commit()
}

func (n *notifyStore) NoteDelivered(ctx context.Context, id int64, serial uint32, at int64) error {
	return n.s.execOne(ctx,
		`UPDATE zone_notifies SET pending_serial = ?, notified_serial = ?, notified_at = ?,
		   attempts = 0, next_attempt_at = 0, last_error = '' WHERE id = ?`,
		serial, serial, at, id)
}

func (n *notifyStore) NoteAttempt(ctx context.Context, id int64, pendingSerial uint32, attempts int, nextAttemptAt int64, errText string) error {
	return n.s.execOne(ctx,
		`UPDATE zone_notifies SET pending_serial = ?, attempts = ?, next_attempt_at = ?, last_error = ? WHERE id = ?`,
		pendingSerial, attempts, nextAttemptAt, errText, id)
}
