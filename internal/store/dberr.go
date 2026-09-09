package store

import (
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
	sqlite3 "modernc.org/sqlite"
	sqlite3lib "modernc.org/sqlite/lib"
)

// wrapDBErr translates the two driver-level constraint violations that are
// user input error rather than storage failure — a uniqueness violation into
// ErrDuplicate, a foreign-key violation into ErrReference — so the API can
// answer 409, or 400/404, instead of 503. Everything else is returned
// untouched: a genuine storage failure must keep looking like one.
//
// Both drivers are matched on their typed errors and result codes rather
// than on message text, which differs between them and is not part of
// either driver's contract. That rule is why DeleteGroup's FK branch was
// moved here: matching "FOREIGN KEY" in a message worked on sqlite and on
// nothing else.
func wrapDBErr(err error) error {
	if err == nil {
		return nil
	}

	// modernc sqlite: SQLITE_CONSTRAINT_UNIQUE (2067),
	// SQLITE_CONSTRAINT_PRIMARYKEY (1555) and SQLITE_CONSTRAINT_FOREIGNKEY
	// (787). All are extended result codes whose low byte is
	// SQLITE_CONSTRAINT (19), and the foreign-key one is only ever raised
	// because store.Open turns on foreign_keys (see sqliteDSN).
	var serr *sqlite3.Error
	if errors.As(err, &serr) {
		switch serr.Code() {
		case sqlite3lib.SQLITE_CONSTRAINT_UNIQUE, sqlite3lib.SQLITE_CONSTRAINT_PRIMARYKEY:
			return errors.Join(ErrDuplicate, err)
		case sqlite3lib.SQLITE_CONSTRAINT_FOREIGNKEY:
			return errors.Join(ErrReference, err)
		}
	}

	// postgres: 23505 unique_violation, 23503 foreign_key_violation.
	var perr *pgconn.PgError
	if errors.As(err, &perr) {
		switch perr.Code {
		case "23505":
			return errors.Join(ErrDuplicate, err)
		case "23503":
			return errors.Join(ErrReference, err)
		}
	}

	return err
}
