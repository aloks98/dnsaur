package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"strings"

	"github.com/pressly/goose/v3"
)

//go:embed migrations
var migrationsFS embed.FS

// migrate applies all pending migrations for dialect using goose as an
// embedded library (github.com/pressly/goose/v3). Goose tracks applied
// versions in its own goose_db_version table, so calling migrate again on an
// already-current database (e.g. on every Open) is a no-op.
func migrate(ctx context.Context, db *sql.DB, dialect string) error {
	var gooseDialect goose.Dialect
	switch dialect {
	case "sqlite":
		gooseDialect = goose.DialectSQLite3
	case "postgres":
		gooseDialect = goose.DialectPostgres
	default:
		return fmt.Errorf("unknown driver %q", dialect)
	}
	sub, err := fs.Sub(migrationsFS, "migrations/"+dialect)
	if err != nil {
		return err
	}
	// Version 5 (zonemigrate.go) is a Go migration, not SQL: converting
	// local_records into zones needs suffix grouping and CNAME-conflict
	// detection that SQLite has no regexp_replace to express, and doing it
	// in Go once instead of twice (per dialect) keeps one tested
	// implementation instead of two that could disagree. It runs via
	// RunTx, not RunDB — see upZonesData's doc comment: with
	// SetMaxOpenConns(1) on sqlite, RunDB deadlocks every fresh-install
	// migration by asking the (already fully checked-out) connection pool
	// for a second connection.
	provider, err := goose.NewProvider(gooseDialect, db, sub, goose.WithGoMigrations(
		goose.NewGoMigration(5, &goose.GoFunc{RunTx: func(ctx context.Context, tx *sql.Tx) error {
			return upZonesData(ctx, tx, dialect)
		}}, nil),
	))
	if err != nil {
		return fmt.Errorf("goose provider: %w", err)
	}
	// Deliberately not calling provider.Close(): it closes the *sql.DB we were
	// given, which this store keeps using after migrating.
	if _, err := provider.Up(ctx); err != nil {
		return fmt.Errorf("goose up: %w", err)
	}
	return nil
}

// rebind converts ? placeholders to $1,$2… for postgres.
func rebind(dialect, q string) string {
	if dialect != "postgres" {
		return q
	}
	var b strings.Builder
	n := 0
	for _, r := range q {
		if r == '?' {
			n++
			fmt.Fprintf(&b, "$%d", n)
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}
