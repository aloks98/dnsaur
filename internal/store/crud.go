package store

import (
	"context"
	"database/sql"
	"errors"
)

// configWrite is every write that changes this instance's configuration:
// settings (through settingsStore.SetMany), and every table that travels in
// a bundle — groups, clients, lists and their group assignments, rules, TSIG
// keys and zone definitions. Each of them advances config_version in its own
// transaction and publishes the new value after the commit, because that
// counter is what a replica polls to decide whether the main's configuration
// moved (the config-sync design, §3) and what this box's own settings
// watcher wakes on.
//
// Records, serial bumps and the transfer-state writers are deliberately not
// in it: none of them is in a bundle, and moving the counter for a
// secondary's hourly refresh would have the whole fleet pull an identical
// bundle every hour.
//
// The three wrappers below are the whole vocabulary — an insert, a write
// that must match one row, and a conditional write that may legitimately
// match none. ImportBundle is the one caller that does not go through them:
// it is a hundred statements under one bump, so it holds its own
// transaction and calls bumpVersionTx itself.
func (s *sqlStore) configWrite(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // undoes everything unless Commit ran
	if err := fn(tx); err != nil {
		return err
	}
	v, err := bumpVersionTx(ctx, tx)
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.hub.publish(v)
	return nil
}

// configInsert is insert for a synced table: the new row id, and the version
// moved in the same transaction.
func (s *sqlStore) configInsert(ctx context.Context, q string, args ...any) (int64, error) {
	var id int64
	err := s.configWrite(ctx, func(tx *sql.Tx) error {
		if s.dialect == "postgres" {
			return wrapDBErr(tx.QueryRowContext(ctx, s.q(q+" RETURNING id"), args...).Scan(&id))
		}
		res, err := tx.ExecContext(ctx, s.q(q), args...)
		if err != nil {
			return wrapDBErr(err)
		}
		id, err = res.LastInsertId()
		return err
	})
	if err != nil {
		return 0, err
	}
	return id, nil
}

// configExecOne is execOne for a synced table. A statement that matched no
// row is ErrNotFound and rolls back, so the version does not move for a
// write that changed nothing.
func (s *sqlStore) configExecOne(ctx context.Context, q string, args ...any) error {
	n, err := s.configExecN(ctx, q, args...)
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// configExecN is configExecOne for a statement whose predicate may
// legitimately match nothing — an optimistic update, a delete guarded by a
// reference check — and which answers that case itself. The version moves
// only when a row actually changed.
func (s *sqlStore) configExecN(ctx context.Context, q string, args ...any) (int64, error) {
	var n int64
	err := s.configWrite(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, s.q(q), args...)
		if err != nil {
			return wrapDBErr(err)
		}
		if n, err = res.RowsAffected(); err != nil {
			return err
		}
		if n == 0 {
			return errNoRows
		}
		return nil
	})
	if errors.Is(err, errNoRows) {
		return 0, nil
	}
	return n, err
}

// errNoRows unwinds configWrite's transaction from inside fn when the
// statement matched nothing. It never reaches a caller: configExecN turns it
// back into (0, nil), and the two callers above decide what that means.
var errNoRows = errors.New("no rows affected")

// execOne runs a statement expected to affect exactly one row.
func (s *sqlStore) execOne(ctx context.Context, q string, args ...any) error {
	res, err := s.db.ExecContext(ctx, s.q(q), args...)
	if err != nil {
		return wrapDBErr(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// execOneTx runs a statement on a transaction expected to affect exactly one row.
func execOneTx(ctx context.Context, tx *sql.Tx, dialect, q string, args ...any) error {
	res, err := tx.ExecContext(ctx, rebind(dialect, q), args...)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (c *clientStore) UpdateClient(ctx context.Context, cl Client) error {
	return c.s.configExecOne(ctx, `UPDATE clients SET name = ?, matcher = ?, group_id = ? WHERE id = ?`, cl.Name, cl.Matcher, cl.GroupID, cl.ID)
}

func (c *clientStore) DeleteClient(ctx context.Context, id int64) error {
	return c.s.configExecOne(ctx, `DELETE FROM clients WHERE id = ?`, id)
}

func (c *clientStore) RenameGroup(ctx context.Context, id int64, name string) error {
	return c.s.configExecOne(ctx, `UPDATE groups SET name = ? WHERE id = ?`, name, id)
}

func (c *clientStore) SetGroupEnabled(ctx context.Context, id int64, enabled bool) error {
	return c.s.configExecOne(ctx, `UPDATE groups SET enabled = ? WHERE id = ?`, enabled, id)
}

func (c *clientStore) DeleteGroup(ctx context.Context, id int64) error {
	if id == 1 {
		return ErrInUse // the default group is structural
	}
	return c.s.configWrite(ctx, func(tx *sql.Tx) error {
		return c.deleteGroup(ctx, tx, id)
	})
}

func (c *clientStore) deleteGroup(ctx context.Context, tx *sql.Tx, id int64) error {
	// Check for clients referencing this group
	var n int64
	if err := tx.QueryRowContext(ctx, c.s.q(`SELECT COUNT(*) FROM clients WHERE group_id = ?`), id).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return ErrInUse
	}

	// Delete group_lists referencing this group
	if _, err := tx.ExecContext(ctx, c.s.q(`DELETE FROM group_lists WHERE group_id = ?`), id); err != nil {
		return err
	}

	// Delete rules in this group
	if _, err := tx.ExecContext(ctx, c.s.q(`DELETE FROM rules WHERE group_id = ?`), id); err != nil {
		return err
	}

	// Delete the group itself
	if err := execOneTx(ctx, tx, c.s.dialect, `DELETE FROM groups WHERE id = ?`, id); err != nil {
		// Some row this transaction did not clear still references the
		// group, which is the same answer the clients check above gives —
		// arrived at from the driver instead. Matched on the typed error
		// rather than on "FOREIGN KEY" in the message: that text is sqlite's
		// and postgres words it differently, so the string match answered
		// ErrInUse on one driver and a raw error on the other.
		if errors.Is(wrapDBErr(err), ErrReference) {
			return ErrInUse
		}
		return err
	}
	return nil
}

func (f *filterStore) SetListEnabled(ctx context.Context, id int64, enabled bool) error {
	return f.s.configExecOne(ctx, `UPDATE lists SET enabled = ? WHERE id = ?`, enabled, id)
}

func (f *filterStore) DeleteList(ctx context.Context, id int64) error {
	return f.s.configWrite(ctx, func(tx *sql.Tx) error {
		// Delete all group_lists associations for this list
		if _, err := tx.ExecContext(ctx, f.s.q(`DELETE FROM group_lists WHERE list_id = ?`), id); err != nil {
			return err
		}
		// Delete the list itself
		return execOneTx(ctx, tx, f.s.dialect, `DELETE FROM lists WHERE id = ?`, id)
	})
}

func (f *filterStore) UnassignList(ctx context.Context, groupID, listID int64) error {
	return f.s.configExecOne(ctx, `DELETE FROM group_lists WHERE group_id = ? AND list_id = ?`, groupID, listID)
}

func (f *filterStore) DeleteRule(ctx context.Context, id int64) error {
	return f.s.configExecOne(ctx, `DELETE FROM rules WHERE id = ?`, id)
}
