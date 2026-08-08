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

	Primaries   string `json:"primaries"`
	TSIGKeyID   int64  `json:"tsig_key_id"`
	ExpiresAt   int64  `json:"expires_at"`
	RefreshedAt int64  `json:"refreshed_at"`
	CreatedAt   int64  `json:"created_at"`
	ModifiedAt  int64  `json:"modified_at"`
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
}

type zoneStore struct{ s *sqlStore }

const zoneColumns = `id, name, type, enabled, soa_ns, soa_mbox, soa_serial, soa_refresh, soa_retry, soa_expire, soa_minimum, soa_ttl, primaries, tsig_key_id, expires_at, refreshed_at, created_at, modified_at`

func scanZone(row interface{ Scan(...any) error }, z *Zone) error {
	return row.Scan(&z.ID, &z.Name, &z.Type, &z.Enabled, &z.SOANS, &z.SOAMbox, &z.SOASerial, &z.SOARefresh, &z.SOARetry, &z.SOAExpire, &z.SOAMinimum, &z.SOATTL, &z.Primaries, &z.TSIGKeyID, &z.ExpiresAt, &z.RefreshedAt, &z.CreatedAt, &z.ModifiedAt)
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
	return z.s.insert(ctx, `INSERT INTO zones (name, type, enabled, soa_ns, soa_mbox, soa_serial, soa_refresh, soa_retry, soa_expire, soa_minimum, soa_ttl, primaries, tsig_key_id, expires_at, refreshed_at, created_at, modified_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		zn.Name, zn.Type, zn.Enabled, zn.SOANS, zn.SOAMbox, zn.SOASerial, zn.SOARefresh, zn.SOARetry, zn.SOAExpire, zn.SOAMinimum, zn.SOATTL, zn.Primaries, zn.TSIGKeyID, zn.ExpiresAt, zn.RefreshedAt, zn.CreatedAt, zn.ModifiedAt)
}

func (z *zoneStore) UpdateZone(ctx context.Context, zn Zone) error {
	return z.s.execOne(ctx, `UPDATE zones SET name = ?, type = ?, enabled = ?, soa_ns = ?, soa_mbox = ?, soa_serial = ?, soa_refresh = ?, soa_retry = ?, soa_expire = ?, soa_minimum = ?, soa_ttl = ?, primaries = ?, tsig_key_id = ?, expires_at = ?, refreshed_at = ?, modified_at = ? WHERE id = ?`,
		zn.Name, zn.Type, zn.Enabled, zn.SOANS, zn.SOAMbox, zn.SOASerial, zn.SOARefresh, zn.SOARetry, zn.SOAExpire, zn.SOAMinimum, zn.SOATTL, zn.Primaries, zn.TSIGKeyID, zn.ExpiresAt, zn.RefreshedAt, zn.ModifiedAt, zn.ID)
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
	return z.s.insert(ctx, `INSERT INTO zone_records (zone_id, name, type, ttl, rdata, enabled, comment) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		r.ZoneID, r.Name, r.Type, r.TTL, r.RData, r.Enabled, r.Comment)
}

func (z *zoneStore) UpdateRecord(ctx context.Context, r ZoneRecord) error {
	// zone_id is deliberately not in the SET list: a record is deleted and
	// recreated to move between zones, not updated in place.
	return z.s.execOne(ctx, `UPDATE zone_records SET name = ?, type = ?, ttl = ?, rdata = ?, enabled = ?, comment = ? WHERE id = ?`,
		r.Name, r.Type, r.TTL, r.RData, r.Enabled, r.Comment, r.ID)
}

func (z *zoneStore) DeleteRecord(ctx context.Context, id int64) error {
	return z.s.execOne(ctx, `DELETE FROM zone_records WHERE id = ?`, id)
}
