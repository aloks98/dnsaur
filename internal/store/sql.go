package store

import (
	"context"
	"database/sql"
)

type sqlStore struct {
	db      *sql.DB
	dialect string
	hub     notifyHub
}

func (s *sqlStore) Close() error         { return s.db.Close() }
func (s *sqlStore) Clients() ClientStore { return &clientStore{s} }
func (s *sqlStore) Filters() FilterStore { return &filterStore{s} }
func (s *sqlStore) Records() RecordStore { return &recordStore{s} }
func (s *sqlStore) Settings() SettingsStore { return &settingsStore{s: s} }
func (s *sqlStore) QueryLog() QueryLogStore { return &queryLogStore{s} }
func (s *sqlStore) Stats() StatsStore { return &statsStore{s: s} }
func (s *sqlStore) Users() UserStore { return &userStore{s} }
func (s *sqlStore) Tokens() TokenStore { return &tokenStore{s} }

func (s *sqlStore) q(q string) string { return rebind(s.dialect, q) }

// insert runs an INSERT and returns the new row id, handling the
// sqlite (LastInsertId) vs postgres (RETURNING) difference in one place.
func (s *sqlStore) insert(ctx context.Context, q string, args ...any) (int64, error) {
	if s.dialect == "postgres" {
		var id int64
		err := s.db.QueryRowContext(ctx, s.q(q+" RETURNING id"), args...).Scan(&id)
		return id, err
	}
	res, err := s.db.ExecContext(ctx, s.q(q), args...)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

type clientStore struct{ s *sqlStore }

func (c *clientStore) AddGroup(ctx context.Context, name string) (int64, error) {
	return c.s.insert(ctx, `INSERT INTO groups (name) VALUES (?)`, name)
}

func (c *clientStore) AddClient(ctx context.Context, cl Client) (int64, error) {
	return c.s.insert(ctx, `INSERT INTO clients (name, matcher, group_id) VALUES (?, ?, ?)`, cl.Name, cl.Matcher, cl.GroupID)
}

func (c *clientStore) Groups(ctx context.Context) ([]Group, error) {
	rows, err := c.s.db.QueryContext(ctx, `SELECT id, name, enabled FROM groups ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	// Initialized non-nil (not `var out []Group`) so a zero-row result
	// marshals to JSON `[]`, not `null` — the API's list endpoints are
	// documented (and consumed by the dashboard) as always returning an
	// array. A brand-new instance with no groups yet is the exact case
	// this matters for.
	out := []Group{}
	for rows.Next() {
		var g Group
		if err := rows.Scan(&g.ID, &g.Name, &g.Enabled); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

func (c *clientStore) Clients(ctx context.Context) ([]Client, error) {
	rows, err := c.s.db.QueryContext(ctx, `SELECT id, name, matcher, group_id FROM clients ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	// Non-nil so zero clients marshals to `[]`, not `null` — see Groups above.
	out := []Client{}
	for rows.Next() {
		var cl Client
		if err := rows.Scan(&cl.ID, &cl.Name, &cl.Matcher, &cl.GroupID); err != nil {
			return nil, err
		}
		out = append(out, cl)
	}
	return out, rows.Err()
}

type filterStore struct{ s *sqlStore }

func (f *filterStore) AddList(ctx context.Context, l List) (int64, error) {
	return f.s.insert(ctx, `INSERT INTO lists (url, kind, enabled) VALUES (?, ?, ?)`, l.URL, l.Kind, l.Enabled)
}

func (f *filterStore) AssignList(ctx context.Context, groupID, listID int64) error {
	_, err := f.s.db.ExecContext(ctx, f.s.q(`INSERT INTO group_lists (group_id, list_id) VALUES (?, ?)`), groupID, listID)
	return err
}

func (f *filterStore) AddRule(ctx context.Context, r Rule) (int64, error) {
	return f.s.insert(ctx, `INSERT INTO rules (group_id, action, pattern, is_regex) VALUES (?, ?, ?, ?)`, r.GroupID, r.Action, r.Pattern, r.IsRegex)
}

func (f *filterStore) TouchList(ctx context.Context, id, refreshedAt, entryCount int64) error {
	_, err := f.s.db.ExecContext(ctx, f.s.q(`UPDATE lists SET last_refreshed = ?, entry_count = ? WHERE id = ?`), refreshedAt, entryCount, id)
	return err
}

func (f *filterStore) scanLists(rows *sql.Rows) ([]List, error) {
	defer rows.Close()
	// Non-nil so zero lists marshals to `[]`, not `null` — see Groups above.
	// Backs both Lists() and ListsForGroup(), the latter of which is
	// commonly empty (a group with no filter lists assigned yet).
	out := []List{}
	for rows.Next() {
		var l List
		if err := rows.Scan(&l.ID, &l.URL, &l.Kind, &l.Enabled, &l.LastRefreshed, &l.EntryCount); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func (f *filterStore) Lists(ctx context.Context) ([]List, error) {
	rows, err := f.s.db.QueryContext(ctx, `SELECT id, url, kind, enabled, last_refreshed, entry_count FROM lists ORDER BY id`)
	if err != nil {
		return nil, err
	}
	return f.scanLists(rows)
}

func (f *filterStore) ListsForGroup(ctx context.Context, groupID int64) ([]List, error) {
	rows, err := f.s.db.QueryContext(ctx, f.s.q(`SELECT l.id, l.url, l.kind, l.enabled, l.last_refreshed, l.entry_count
		FROM lists l JOIN group_lists gl ON gl.list_id = l.id WHERE gl.group_id = ? ORDER BY l.id`), groupID)
	if err != nil {
		return nil, err
	}
	return f.scanLists(rows)
}

func (f *filterStore) Rules(ctx context.Context, groupID int64) ([]Rule, error) {
	rows, err := f.s.db.QueryContext(ctx, f.s.q(`SELECT id, group_id, action, pattern, is_regex FROM rules WHERE group_id = ? ORDER BY id`), groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	// Non-nil so zero rules marshals to `[]`, not `null` — see Groups above.
	// Every group starts with no rules, so this is the common case, not an
	// edge case.
	out := []Rule{}
	for rows.Next() {
		var r Rule
		if err := rows.Scan(&r.ID, &r.GroupID, &r.Action, &r.Pattern, &r.IsRegex); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

type recordStore struct{ s *sqlStore }

func (r *recordStore) Add(ctx context.Context, rec LocalRecord) (int64, error) {
	return r.s.insert(ctx, `INSERT INTO local_records (name, type, value, ttl) VALUES (?, ?, ?, ?)`, rec.Name, rec.Type, rec.Value, rec.TTL)
}

func (r *recordStore) All(ctx context.Context) ([]LocalRecord, error) {
	rows, err := r.s.db.QueryContext(ctx, `SELECT id, name, type, value, ttl FROM local_records ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	// Non-nil so zero records marshals to `[]`, not `null` — see Groups
	// above. Every fresh instance starts with no local records.
	out := []LocalRecord{}
	for rows.Next() {
		var rec LocalRecord
		if err := rows.Scan(&rec.ID, &rec.Name, &rec.Type, &rec.Value, &rec.TTL); err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}
