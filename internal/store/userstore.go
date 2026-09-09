package store

import (
	"context"
	"database/sql"
	"errors"
)

type userStore struct{ s *sqlStore }

// userColumns is the select list every user read shares, so a column added
// to User is added to one place rather than three that can disagree.
const userColumns = `id, username, password_hash, totp_secret, totp_last_step, created_at`

func scanUser(row interface{ Scan(...any) error }) (User, error) {
	var usr User
	err := row.Scan(&usr.ID, &usr.Username, &usr.PasswordHash, &usr.TOTPSecret, &usr.TOTPLastStep, &usr.CreatedAt)
	return usr, err
}

func (u *userStore) Create(ctx context.Context, usr User) (int64, error) {
	return u.s.insert(ctx, `INSERT INTO users (username, password_hash, totp_secret, created_at) VALUES (?, ?, ?, ?)`,
		usr.Username, usr.PasswordHash, usr.TOTPSecret, usr.CreatedAt)
}

const createIfNoneQuery = `INSERT INTO users (username, password_hash, totp_secret, created_at)
	SELECT ?, ?, ?, ? WHERE NOT EXISTS (SELECT 1 FROM users)`

// pgSetupLockKey is an arbitrary fixed key for the postgres advisory lock
// CreateIfNone takes around first-admin creation; the value has no meaning
// beyond being a stable constant distinct from other advisory locks this
// codebase might take in the future.
const pgSetupLockKey = 8199244102

// CreateIfNone inserts usr only if the users table is currently empty, so
// two concurrent first-run setup requests can't both create an admin
// account (Count-then-Create is not atomic and races).
//
// The INSERT ... SELECT ... WHERE NOT EXISTS statement is atomic against
// concurrent callers racing for the *same* username (the UNIQUE constraint
// on username arbitrates that), but on its own it is NOT sufficient under
// postgres' default READ COMMITTED isolation when concurrent callers use
// *different* usernames: each transaction's snapshot is taken at statement
// start, so two overlapping transactions can each see an empty table and
// each insert — verified empirically with a prewarmed-connection stress
// test (40 concurrent distinct usernames reliably produced >1 row without
// this lock). A postgres advisory lock, held only for the duration of the
// statement, serializes the check-and-insert across concurrent
// transactions and closes that gap. SQLite needs no equivalent: the store
// already pins it to a single physical connection (SetMaxOpenConns(1) in
// Open), so statements on it can never truly overlap.
func (u *userStore) CreateIfNone(ctx context.Context, usr User) (bool, error) {
	if u.s.dialect == "postgres" {
		return u.createIfNonePostgres(ctx, usr)
	}
	res, err := u.s.db.ExecContext(ctx, u.s.q(createIfNoneQuery),
		usr.Username, usr.PasswordHash, usr.TOTPSecret, usr.CreatedAt)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

func (u *userStore) createIfNonePostgres(ctx context.Context, usr User) (bool, error) {
	tx, err := u.s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }() // no-op once committed

	// Transaction-scoped advisory lock: blocks concurrent CreateIfNone
	// callers until this transaction commits or rolls back, so the
	// WHERE NOT EXISTS check below can't race with another caller's insert.
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, pgSetupLockKey); err != nil {
		return false, err
	}
	res, err := tx.ExecContext(ctx, u.s.q(createIfNoneQuery),
		usr.Username, usr.PasswordHash, usr.TOTPSecret, usr.CreatedAt)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return n == 1, nil
}

func (u *userStore) ByUsername(ctx context.Context, name string) (User, bool, error) {
	usr, err := scanUser(u.s.db.QueryRowContext(ctx, u.s.q(`SELECT `+userColumns+` FROM users WHERE username = ?`), name))
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, false, nil
	}
	return usr, err == nil, err
}

func (u *userStore) ByID(ctx context.Context, id int64) (User, bool, error) {
	usr, err := scanUser(u.s.db.QueryRowContext(ctx, u.s.q(`SELECT `+userColumns+` FROM users WHERE id = ?`), id))
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, false, nil
	}
	return usr, err == nil, err
}

func (u *userStore) Count(ctx context.Context) (int64, error) {
	var n int64
	err := u.s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

// SetTOTP also clears totp_last_step: the counter only means anything
// relative to the secret it was recorded against, and a new secret (or the
// removal of one) starts a fresh sequence. Leaving a stale high-water mark
// behind would refuse the first code of a re-enrolment for up to 90 seconds
// for no reason anyone could observe.
func (u *userStore) SetTOTP(ctx context.Context, id int64, secret string) error {
	_, err := u.s.db.ExecContext(ctx, u.s.q(`UPDATE users SET totp_secret = ?, totp_last_step = 0 WHERE id = ?`), secret, id)
	return err
}

func (u *userStore) ClaimTOTPStep(ctx context.Context, id, step int64) (bool, error) {
	// One conditional UPDATE, not a read followed by a write: two logins
	// presenting the same code at the same moment must not both find the
	// stored step lower than theirs and both proceed.
	res, err := u.s.db.ExecContext(ctx,
		u.s.q(`UPDATE users SET totp_last_step = ? WHERE id = ? AND totp_last_step < ?`), step, id, step)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}
