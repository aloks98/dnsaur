package store

import (
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
	sqlite3 "modernc.org/sqlite"
	sqlite3lib "modernc.org/sqlite/lib"
)

// wrapDBErr translates a driver-level uniqueness violation into
// ErrDuplicate so the API can answer 409 instead of 503. Everything else is
// returned untouched — a genuine storage failure must keep looking like one.
//
// Both drivers are matched on their typed errors and result codes rather
// than on message text, which differs between them and is not part of
// either driver's contract.
func wrapDBErr(err error) error {
	if err == nil {
		return nil
	}

	// modernc sqlite: SQLITE_CONSTRAINT_UNIQUE (2067) and
	// SQLITE_CONSTRAINT_PRIMARYKEY (1555). Both are extended result codes
	// whose low byte is SQLITE_CONSTRAINT (19).
	var serr *sqlite3.Error
	if errors.As(err, &serr) {
		switch serr.Code() {
		case sqlite3lib.SQLITE_CONSTRAINT_UNIQUE, sqlite3lib.SQLITE_CONSTRAINT_PRIMARYKEY:
			return errors.Join(ErrDuplicate, err)
		}
	}

	// postgres: 23505 unique_violation.
	var perr *pgconn.PgError
	if errors.As(err, &perr) && perr.Code == "23505" {
		return errors.Join(ErrDuplicate, err)
	}

	return err
}
