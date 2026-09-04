package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Zone is a suffix this server is authoritative for. A query for a name
// inside an enabled zone is answered from it or refused by it — it is
// never forwarded, which is what separates a zone from the override list
// it replaced.
type Zone struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"` // apex, lowercase, no trailing dot
	Type    string `json:"type"` // primary | secondary | stub | forwarder | internal
	Enabled bool   `json:"enabled"`

	SOANS      string `json:"soa_ns"`
	SOAMbox    string `json:"soa_mbox"`
	SOASerial  uint32 `json:"soa_serial"`
	SOARefresh uint32 `json:"soa_refresh"`
	SOARetry   uint32 `json:"soa_retry"`
	SOAExpire  uint32 `json:"soa_expire"`
	// SOAMinimum is the negative-cache TTL for NXDOMAINs this zone hands
	// out (RFC 2308), not a floor on positive answers.
	SOAMinimum uint32 `json:"soa_minimum"`
	// SOATTL is the header TTL of the SOA record itself, which is a
	// different thing from SOAMinimum: RFC 2308 §5 makes a negative
	// answer's TTL min(SOAMinimum, SOATTL), so the two have to be
	// separately settable for that minimum to mean anything.
	SOATTL uint32 `json:"soa_ttl"`

	Primaries string `json:"primaries"`
	TSIGKeyID int64  `json:"tsig_key_id"`
	ExpiresAt int64  `json:"expires_at"`
	// RefreshedAt is the last transfer that *succeeded*, unix ms; 0 = never.
	RefreshedAt int64 `json:"refreshed_at"`

	// LastError and LastAttempt are how the most recent transfer attempt
	// went, as opposed to the most recent one that worked. Written only by
	// NoteTransferAttempt — see the 0010 migration for the whole reasoning,
	// and for why there is no stored status beside them.
	//
	// LastError is "" when that attempt succeeded, so a zone that recovered
	// stops reporting one. LastAttempt dates it, successful or not; 0 = never
	// attempted. The pair is only meaningful read together: an error with no
	// date is a claim about now made by an unknown past.
	LastError   string `json:"last_error"`
	LastAttempt int64  `json:"last_attempt"`

	// AllowTransfer is who may pull this zone: a comma-separated list of
	// address, CIDR, or key:<tsig name>, in zones.FormatACL's canonical
	// spelling. Empty means deny, and that is the default.
	AllowTransfer string `json:"allow_transfer"`
	// The outbound twin of LastAttempt/LastError: when a peer last asked for
	// this zone, which peer, and why it was refused if it was. Written only by
	// NoteTransferRequest.
	LastXfrAt    int64  `json:"last_xfr_at"`
	LastXfrPeer  string `json:"last_xfr_peer"`
	LastXfrError string `json:"last_xfr_error"`

	// NotifyTo is who this zone tells when it changes: a comma-separated
	// list of host[:port] [key:<tsig name>], in zones.FormatNotifyTo's
	// canonical spelling. Empty means notify nobody, and that is the
	// default. Applies to a primary and to a secondary — a secondary that
	// re-serves what it pulled has its own downstream secondaries.
	NotifyTo string `json:"notify_to"`

	// ForwardTo is where a `forwarder` zone sends the queries it claims: a
	// comma-separated list of host[:port] in zones.FormatForwardTo's
	// canonical spelling. Empty means the zone names no upstreams, and a
	// forwarder zone with none answers SERVFAIL rather than falling
	// through — it still claims the suffix (§9.11.5).
	//
	// Only `forwarder` uses it. A `stub` names its master in Primaries.
	ForwardTo string `json:"forward_to"`

	CreatedAt  int64 `json:"created_at"`
	ModifiedAt int64 `json:"modified_at"`
}

// ZoneRecord is one RR, named relative to its zone's apex.
type ZoneRecord struct {
	ID     int64  `json:"id"`
	ZoneID int64  `json:"zone_id"`
	Name   string `json:"name"` // '@', 'bifrost', '*', '*.nexus'
	Type   string `json:"type"`
	TTL    uint32 `json:"ttl"`
	// RData is presentation format, rdata portion only.
	RData   string `json:"rdata"`
	Enabled bool   `json:"enabled"`
	Comment string `json:"comment"`
}

// ZoneStore manages authoritative zones and their records.
type ZoneStore interface {
	Zones(ctx context.Context) ([]Zone, error)
	Zone(ctx context.Context, id int64) (Zone, error)
	AddZone(ctx context.Context, z Zone) (int64, error)
	UpdateZone(ctx context.Context, z Zone) error
	DeleteZone(ctx context.Context, id int64) error
	// BumpSerial increments soa_serial in SQL rather than read-modify-write,
	// so two concurrent record edits on the same zone can't land on the
	// same serial.
	BumpSerial(ctx context.Context, zoneID int64) error
	Records(ctx context.Context, zoneID int64) ([]ZoneRecord, error)
	// AllRecords returns every record for every zone, grouped by zone ID, in
	// one query. The resolver rebuilds a whole snapshot on reload, so N+1
	// queries per zone would make reload cost scale with zone count.
	AllRecords(ctx context.Context) (map[int64][]ZoneRecord, error)
	AddRecord(ctx context.Context, r ZoneRecord) (int64, error)
	UpdateRecord(ctx context.Context, r ZoneRecord) error
	DeleteRecord(ctx context.Context, id int64) error
	// ReplaceRecords rewrites a zone in one transaction: the rows named by
	// deleteIDs go, updates are applied in place, adds are inserted, and z
	// replaces the zone row itself — all of it or none of it.
	//
	// It exists because a zone-file import (and, from Milestone D, a zone
	// transfer) is a destructive whole-zone replace, not a sequence of
	// independent edits. Run as separate writes, a storage failure part-way
	// through leaves the zone matching neither its new contents nor its old
	// ones, with no record of where it stopped.
	//
	// The zone row is in the same transaction as the records, and that is
	// the point of passing z at all rather than leaving the caller to follow
	// with UpdateZone. The SOA serial is how every consumer of this zone —
	// a secondary above all — decides whether it already has the current
	// contents. Records that committed while the serial that describes them
	// did not is the one inconsistency nothing downstream can detect: the
	// zone answers with new data under an old serial, and a secondary that
	// has already seen that serial will never ask again.
	//
	// The caller supplies z whole, serial included, because what the new
	// serial should be is a question about DNS (RFC 1982 arithmetic, never
	// going backwards), not about storage.
	//
	// It does not write last_error/last_attempt: those belong to
	// NoteTransferAttempt alone. See its doc comment.
	ReplaceRecords(ctx context.Context, z Zone, deleteIDs []int64, updates, adds []ZoneRecord) error
	// NoteTransferAttempt records how one transfer attempt went: at is when
	// it finished (unix ms) and errText is the transfer's own error, or ""
	// when it succeeded.
	//
	// It is a two-column UPDATE, and that narrowness is the whole design
	// rather than an optimisation. The *whole-row* writes to this table —
	// updateZoneSQL, and ReplaceRecords through it — bind every column from a
	// struct the caller read some time earlier, so a transfer outcome
	// recorded through one of those would silently revert any concurrent edit
	// to enabled, primaries or tsig_key_id: the exact defect the transfer
	// install itself had to be fixed for, reached again through a second
	// door. It is also why neither of them may write these two columns —
	// were last_error in updateZoneSQL, an API PATCH that read the row before
	// a failure and committed after it would erase the failure.
	//
	// It is not the only narrow write here (BumpSerial is another, for a
	// related reason: a serial must be incremented in SQL rather than
	// read-modify-written). What matters is that the column sets are
	// disjoint — this statement owns last_error and last_attempt and touches
	// nothing else, and nothing else touches them.
	NoteTransferAttempt(ctx context.Context, zoneID int64, at int64, errText string) error
	// NoteTransferRequest records one *outbound* transfer request: a peer
	// asking this server for the zone, rather than this server asking a
	// primary. errText is "" when the zone was served and the refusal reason
	// when it was not — a refusal is recorded like a success, which is why
	// this is named for the request rather than for one of its outcomes,
	// exactly as its inbound twin NoteTransferAttempt is.
	//
	// It is a three-column UPDATE for the same reason NoteTransferAttempt is
	// a two-column one — see that method's doc comment — and its column set
	// is disjoint from both other writers of this row: updateZoneSQL (and
	// ReplaceRecords through it) never binds last_xfr_at/last_xfr_peer/
	// last_xfr_error, and this statement never binds anything else.
	NoteTransferRequest(ctx context.Context, zoneID, at int64, peer, errText string) error
}

type zoneStore struct{ s *sqlStore }

const zoneColumns = `id, name, type, enabled, soa_ns, soa_mbox, soa_serial, soa_refresh, soa_retry, soa_expire, soa_minimum, soa_ttl, primaries, tsig_key_id, expires_at, refreshed_at, last_error, last_attempt, allow_transfer, last_xfr_at, last_xfr_peer, last_xfr_error, notify_to, forward_to, created_at, modified_at`

func scanZone(row interface{ Scan(...any) error }, z *Zone) error {
	return row.Scan(&z.ID, &z.Name, &z.Type, &z.Enabled, &z.SOANS, &z.SOAMbox, &z.SOASerial, &z.SOARefresh, &z.SOARetry, &z.SOAExpire, &z.SOAMinimum, &z.SOATTL, &z.Primaries, &z.TSIGKeyID, &z.ExpiresAt, &z.RefreshedAt, &z.LastError, &z.LastAttempt, &z.AllowTransfer, &z.LastXfrAt, &z.LastXfrPeer, &z.LastXfrError, &z.NotifyTo, &z.ForwardTo, &z.CreatedAt, &z.ModifiedAt)
}

func (z *zoneStore) Zones(ctx context.Context) ([]Zone, error) {
	rows, err := z.s.db.QueryContext(ctx, `SELECT `+zoneColumns+` FROM zones ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	// Non-nil so zero zones marshals to `[]`, not `null` — see
	// clientStore.Groups in sql.go. A brand-new instance starts with none.
	out := []Zone{}
	for rows.Next() {
		var zn Zone
		if err := scanZone(rows, &zn); err != nil {
			return nil, err
		}
		out = append(out, zn)
	}
	return out, rows.Err()
}

func (z *zoneStore) Zone(ctx context.Context, id int64) (Zone, error) {
	var zn Zone
	row := z.s.db.QueryRowContext(ctx, z.s.q(`SELECT `+zoneColumns+` FROM zones WHERE id = ?`), id)
	if err := scanZone(row, &zn); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Zone{}, ErrNotFound
		}
		return Zone{}, err
	}
	return zn, nil
}

func (z *zoneStore) AddZone(ctx context.Context, zn Zone) (int64, error) {
	// The three last_xfr_* columns are deliberately absent here — they
	// default, and are written only by NoteTransferRequest.
	return z.s.insert(ctx, `INSERT INTO zones (name, type, enabled, soa_ns, soa_mbox, soa_serial, soa_refresh, soa_retry, soa_expire, soa_minimum, soa_ttl, primaries, tsig_key_id, expires_at, refreshed_at, allow_transfer, notify_to, forward_to, created_at, modified_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		zn.Name, zn.Type, zn.Enabled, zn.SOANS, zn.SOAMbox, zn.SOASerial, zn.SOARefresh, zn.SOARetry, zn.SOAExpire, zn.SOAMinimum, zn.SOATTL, zn.Primaries, zn.TSIGKeyID, zn.ExpiresAt, zn.RefreshedAt, zn.AllowTransfer, zn.NotifyTo, zn.ForwardTo, zn.CreatedAt, zn.ModifiedAt)
}

// The four write statements below are each written once and used twice:
// on their own, through the sqlStore helpers that autocommit, and inside
// ReplaceRecords' transaction. Sharing the text is what keeps the two
// paths from drifting — a column added to the hand-write path and missed
// on the import path would be a difference no test of either path alone
// could see, and the zone would silently keep whatever the import didn't
// write.
const (
	updateZoneSQL   = `UPDATE zones SET name = ?, type = ?, enabled = ?, soa_ns = ?, soa_mbox = ?, soa_serial = ?, soa_refresh = ?, soa_retry = ?, soa_expire = ?, soa_minimum = ?, soa_ttl = ?, primaries = ?, tsig_key_id = ?, expires_at = ?, refreshed_at = ?, allow_transfer = ?, notify_to = ?, forward_to = ?, modified_at = ? WHERE id = ?`
	insertRecordSQL = `INSERT INTO zone_records (zone_id, name, type, ttl, rdata, enabled, comment) VALUES (?, ?, ?, ?, ?, ?, ?)`
	// zone_id is deliberately not in the SET list: a record is deleted and
	// recreated to move between zones, not updated in place.
	updateRecordSQL = `UPDATE zone_records SET name = ?, type = ?, ttl = ?, rdata = ?, enabled = ?, comment = ? WHERE id = ?`
	deleteRecordSQL = `DELETE FROM zone_records WHERE id = ?`
)

func updateZoneArgs(zn Zone) []any {
	return []any{zn.Name, zn.Type, zn.Enabled, zn.SOANS, zn.SOAMbox, zn.SOASerial, zn.SOARefresh, zn.SOARetry, zn.SOAExpire, zn.SOAMinimum, zn.SOATTL, zn.Primaries, zn.TSIGKeyID, zn.ExpiresAt, zn.RefreshedAt, zn.AllowTransfer, zn.NotifyTo, zn.ForwardTo, zn.ModifiedAt, zn.ID}
}

func insertRecordArgs(r ZoneRecord) []any {
	return []any{r.ZoneID, r.Name, r.Type, r.TTL, r.RData, r.Enabled, r.Comment}
}

func updateRecordArgs(r ZoneRecord) []any {
	return []any{r.Name, r.Type, r.TTL, r.RData, r.Enabled, r.Comment, r.ID}
}

func (z *zoneStore) UpdateZone(ctx context.Context, zn Zone) error {
	return z.s.execOne(ctx, updateZoneSQL, updateZoneArgs(zn)...)
}

// noteTransferAttemptSQL names the only two columns any transfer outcome is
// allowed to write. See ZoneStore.NoteTransferAttempt for why it is separate
// from updateZoneSQL rather than folded into it.
const noteTransferAttemptSQL = `UPDATE zones SET last_error = ?, last_attempt = ? WHERE id = ?`

func (z *zoneStore) NoteTransferAttempt(ctx context.Context, zoneID int64, at int64, errText string) error {
	return z.s.execOne(ctx, noteTransferAttemptSQL, errText, at, zoneID)
}

// noteTransferRequestSQL names the only three columns an outbound transfer
// outcome may write. Disjoint from updateZoneSQL and from
// noteTransferAttemptSQL — see ZoneStore.NoteTransferRequest.
const noteTransferRequestSQL = `UPDATE zones SET last_xfr_at = ?, last_xfr_peer = ?, last_xfr_error = ? WHERE id = ?`

func (z *zoneStore) NoteTransferRequest(ctx context.Context, zoneID, at int64, peer, errText string) error {
	return z.s.execOne(ctx, noteTransferRequestSQL, at, peer, errText, zoneID)
}

func (z *zoneStore) DeleteZone(ctx context.Context, id int64) error {
	// zone_records rows for this zone go with it — ON DELETE CASCADE in the
	// 0004 migration, not application logic here.
	return z.s.execOne(ctx, `DELETE FROM zones WHERE id = ?`, id)
}

func (z *zoneStore) BumpSerial(ctx context.Context, zoneID int64) error {
	return z.s.execOne(ctx, `UPDATE zones SET soa_serial = soa_serial + 1, modified_at = ? WHERE id = ?`, time.Now().UnixMilli(), zoneID)
}

func (z *zoneStore) Records(ctx context.Context, zoneID int64) ([]ZoneRecord, error) {
	rows, err := z.s.db.QueryContext(ctx, z.s.q(`SELECT id, zone_id, name, type, ttl, rdata, enabled, comment FROM zone_records WHERE zone_id = ? ORDER BY name, id`), zoneID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	// Non-nil so a zone with no records marshals to `[]`, not `null` — see
	// clientStore.Groups in sql.go.
	out := []ZoneRecord{}
	for rows.Next() {
		var r ZoneRecord
		if err := rows.Scan(&r.ID, &r.ZoneID, &r.Name, &r.Type, &r.TTL, &r.RData, &r.Enabled, &r.Comment); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (z *zoneStore) AllRecords(ctx context.Context) (map[int64][]ZoneRecord, error) {
	rows, err := z.s.db.QueryContext(ctx, `SELECT id, zone_id, name, type, ttl, rdata, enabled, comment FROM zone_records ORDER BY zone_id, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64][]ZoneRecord{}
	for rows.Next() {
		var r ZoneRecord
		if err := rows.Scan(&r.ID, &r.ZoneID, &r.Name, &r.Type, &r.TTL, &r.RData, &r.Enabled, &r.Comment); err != nil {
			return nil, err
		}
		out[r.ZoneID] = append(out[r.ZoneID], r)
	}
	return out, rows.Err()
}

func (z *zoneStore) AddRecord(ctx context.Context, r ZoneRecord) (int64, error) {
	return z.s.insert(ctx, insertRecordSQL, insertRecordArgs(r)...)
}

func (z *zoneStore) UpdateRecord(ctx context.Context, r ZoneRecord) error {
	return z.s.execOne(ctx, updateRecordSQL, updateRecordArgs(r)...)
}

func (z *zoneStore) DeleteRecord(ctx context.Context, id int64) error {
	return z.s.execOne(ctx, deleteRecordSQL, id)
}

// ReplaceRecords runs the whole replace — records and the zone row — as one
// transaction. See the interface for why the zone row is in it.
//
// The statements are the same ones the single-write methods above issue,
// one per row rather than one batched DELETE ... IN / multi-row INSERT.
// Round trips are not what a replace costs: this is one transaction and one
// commit, where the same work previously cost one commit *per row*, so even
// the largest import a zone file can express (the API caps a file at a
// megabyte, roughly 35,000 records) is cheaper here than it was before.
// Batching would buy the remaining round trips at the price of building
// placeholder lists that differ per dialect and can exceed postgres's
// parameter limit — a real correctness risk for a saving that isn't the
// bottleneck.
func (z *zoneStore) ReplaceRecords(ctx context.Context, zn Zone, deleteIDs []int64, updates, adds []ZoneRecord) error {
	tx, err := z.s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	// Undoes every statement below unless Commit ran, in which case it is a
	// no-op. Same shape as clientStore.DeleteGroup (crud.go).
	defer tx.Rollback()

	// Deletes, then updates, then adds — the order the API's import applied
	// them in when these were separate writes, kept so the sequence of
	// statements a replace issues is the one already in use rather than a
	// new one whose interaction with a future constraint on zone_records
	// nobody has thought about.
	for _, id := range deleteIDs {
		if err := execOneTx(ctx, tx, z.s.dialect, deleteRecordSQL, id); err != nil {
			return wrapDBErr(err)
		}
	}
	for _, r := range updates {
		if err := execOneTx(ctx, tx, z.s.dialect, updateRecordSQL, updateRecordArgs(r)...); err != nil {
			return wrapDBErr(err)
		}
	}
	for _, r := range adds {
		// Plain Exec rather than the insert helper: a replace has no use for
		// the new row ids, and skipping them is what lets one statement run
		// unchanged on both drivers — postgres needs RETURNING to report an
		// id, sqlite needs LastInsertId.
		if _, err := tx.ExecContext(ctx, z.s.q(insertRecordSQL), insertRecordArgs(r)...); err != nil {
			return wrapDBErr(err)
		}
	}
	// Last, and inside the same transaction: this is the write that carries
	// the new SOA serial, and the failure this method exists to prevent is
	// exactly this statement failing after the records committed.
	if err := execOneTx(ctx, tx, z.s.dialect, updateZoneSQL, updateZoneArgs(zn)...); err != nil {
		return wrapDBErr(err)
	}
	return tx.Commit()
}
