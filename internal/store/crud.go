package store

import (
	"context"
	"database/sql"
	"strings"
)

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
	return c.s.execOne(ctx, `UPDATE clients SET name = ?, matcher = ?, group_id = ? WHERE id = ?`, cl.Name, cl.Matcher, cl.GroupID, cl.ID)
}

func (c *clientStore) DeleteClient(ctx context.Context, id int64) error {
	return c.s.execOne(ctx, `DELETE FROM clients WHERE id = ?`, id)
}

func (c *clientStore) RenameGroup(ctx context.Context, id int64, name string) error {
	return c.s.execOne(ctx, `UPDATE groups SET name = ? WHERE id = ?`, name, id)
}

func (c *clientStore) SetGroupEnabled(ctx context.Context, id int64, enabled bool) error {
	return c.s.execOne(ctx, `UPDATE groups SET enabled = ? WHERE id = ?`, enabled, id)
}

func (c *clientStore) DeleteGroup(ctx context.Context, id int64) error {
	if id == 1 {
		return ErrInUse // the default group is structural
	}
	tx, err := c.s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

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
		// Check if this is a foreign key constraint error and translate to ErrInUse
		if err == ErrNotFound {
			return err
		}
		errStr := err.Error()
		if strings.Contains(errStr, "FOREIGN KEY") || strings.Contains(errStr, "foreign key") {
			return ErrInUse
		}
		return err
	}

	return tx.Commit()
}

func (f *filterStore) SetListEnabled(ctx context.Context, id int64, enabled bool) error {
	return f.s.execOne(ctx, `UPDATE lists SET enabled = ? WHERE id = ?`, enabled, id)
}

func (f *filterStore) DeleteList(ctx context.Context, id int64) error {
	tx, err := f.s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Delete all group_lists associations for this list
	if _, err := tx.ExecContext(ctx, f.s.q(`DELETE FROM group_lists WHERE list_id = ?`), id); err != nil {
		return err
	}

	// Delete the list itself
	if err := execOneTx(ctx, tx, f.s.dialect, `DELETE FROM lists WHERE id = ?`, id); err != nil {
		return err
	}

	return tx.Commit()
}

func (f *filterStore) UnassignList(ctx context.Context, groupID, listID int64) error {
	return f.s.execOne(ctx, `DELETE FROM group_lists WHERE group_id = ? AND list_id = ?`, groupID, listID)
}

func (f *filterStore) DeleteRule(ctx context.Context, id int64) error {
	return f.s.execOne(ctx, `DELETE FROM rules WHERE id = ?`, id)
}

func (r *recordStore) Update(ctx context.Context, rec LocalRecord) error {
	return r.s.execOne(ctx, `UPDATE local_records SET name = ?, type = ?, value = ?, ttl = ? WHERE id = ?`, rec.Name, rec.Type, rec.Value, rec.TTL, rec.ID)
}

func (r *recordStore) Delete(ctx context.Context, id int64) error {
	return r.s.execOne(ctx, `DELETE FROM local_records WHERE id = ?`, id)
}
