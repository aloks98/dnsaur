package store

import (
	"context"
	"database/sql"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/pressly/goose/v3"
)

func openSQLite(t *testing.T) Store {
	t.Helper()
	s, err := Open(context.Background(), "sqlite", filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func openPostgres(t *testing.T) Store {
	t.Helper()
	dsn := os.Getenv("DNSAUR_TEST_POSTGRES_DSN") // set by TestMain via testcontainers when Docker present
	if dsn == "" {
		t.Skip("no postgres available")
	}
	s, err := Open(context.Background(), "postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func forEachDriver(t *testing.T, fn func(t *testing.T, s Store)) {
	t.Run("sqlite", func(t *testing.T) { fn(t, openSQLite(t)) })
	t.Run("postgres", func(t *testing.T) { fn(t, openPostgres(t)) })
}

// TestMigrateExistingDatabaseGainsListStatus is the upgrade path, not the
// fresh-install one: 0003 must apply to a database an *older* binary created
// and left at version 2, with real rows already in `lists`. A migration that
// only ever runs on an empty new file would pass every other test here and
// still brick every existing install.
//
// It also pins the backfill. A list that had already refreshed successfully
// under 0001/0002 has no recorded attempt or status, and defaulting it to
// 'pending' would make a perfectly healthy list read as "never attempted" the
// moment the user upgraded.
func TestMigrateExistingDatabaseGainsListStatus(t *testing.T) {
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "legacy.db")

	// Stand up a database exactly as the pre-0003 binary would have: run
	// the real 0001 and 0002 and stop there.
	raw, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	sub, err := fs.Sub(migrationsFS, "migrations/sqlite")
	if err != nil {
		t.Fatal(err)
	}
	provider, err := goose.NewProvider(goose.DialectSQLite3, raw, sub)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.UpTo(ctx, 2); err != nil {
		t.Fatalf("legacy migrate: %v", err)
	}
	// Two rows an old binary could plausibly have left behind: one that
	// refreshed fine, and one that never did.
	if _, err := raw.ExecContext(ctx,
		`INSERT INTO lists (id, url, kind, enabled, last_refreshed, entry_count) VALUES
		  (1, 'https://healthy.example/hosts', 'block', 1, 1785946876638, 99277),
		  (2, 'https://never-ran.example/hosts', 'block', 1, 0, 0)`); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	// Now open it the way the new binary does, which runs the migrator.
	s, err := Open(ctx, "sqlite", dsn)
	if err != nil {
		t.Fatalf("upgrading an existing database failed: %v", err)
	}
	defer func() { _ = s.Close() }()

	ls, err := s.Filters().Lists(ctx)
	if err != nil {
		t.Fatalf("reading lists after upgrade: %v", err)
	}
	if len(ls) != 2 {
		t.Fatalf("existing rows lost: %+v", ls)
	}
	// Pre-existing data survives untouched.
	if ls[0].URL != "https://healthy.example/hosts" || ls[0].EntryCount != 99277 || ls[0].LastRefreshed != 1785946876638 {
		t.Fatalf("row 1 mangled by the migration: %+v", ls[0])
	}
	// Backfilled: it had refreshed, so it reads ok and dates its attempt
	// from that refresh instead of "never".
	if ls[0].LastStatus != ListStatusOK || ls[0].LastAttempt != 1785946876638 || ls[0].LastError != "" {
		t.Fatalf("healthy legacy row = %+v, want ok backfilled from last_refreshed", ls[0])
	}
	// Never ran: genuinely pending, and must not be dressed up as ok.
	if ls[1].LastStatus != ListStatusPending || ls[1].LastAttempt != 0 {
		t.Fatalf("never-refreshed legacy row = %+v, want pending", ls[1])
	}

	// And the new columns are writable, not just readable.
	if err := s.Filters().MarkListFailed(ctx, 2, 5000, 0, "404 Not Found"); err != nil {
		t.Fatal(err)
	}
	after, _ := s.Filters().Lists(ctx)
	if after[1].LastStatus != ListStatusFailed || after[1].LastError != "404 Not Found" {
		t.Fatalf("post-upgrade write = %+v", after[1])
	}
}

func TestMigrateIdempotent(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "t.db")
	ctx := context.Background()
	s, err := Open(ctx, "sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	s2, err := Open(ctx, "sqlite", dsn) // re-open re-runs migrator; must be a no-op
	if err != nil {
		t.Fatal(err)
	}
	_ = s2.Close()
}

func TestClientAndFilterCRUD(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		groupName := testGroupName("kids")
		listURL := testGroupName("https://example.com/hosts")
		gid, err := s.Clients().AddGroup(ctx, groupName)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Clients().AddClient(ctx, Client{Name: "tablet", Matcher: groupName, GroupID: gid}); err != nil {
			t.Fatal(err)
		}
		lid, err := s.Filters().AddList(ctx, List{URL: listURL, Kind: "block", Enabled: true})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Filters().AssignList(ctx, gid, lid); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Filters().AddRule(ctx, Rule{GroupID: gid, Action: "allow", Pattern: "ok.example.com"}); err != nil {
			t.Fatal(err)
		}
		ls, err := s.Filters().ListsForGroup(ctx, gid)
		if err != nil || len(ls) != 1 || ls[0].URL != listURL {
			t.Fatalf("lists: %v %v", ls, err)
		}
		rs, err := s.Filters().Rules(ctx, gid)
		if err != nil || len(rs) != 1 || rs[0].Action != "allow" {
			t.Fatalf("rules: %v %v", rs, err)
		}
	})
}

func TestLocalRecords(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		// Clean up local_records to handle shared postgres DB
		ss := s.(*sqlStore)
		_, _ = ss.db.ExecContext(ctx, ss.q(`DELETE FROM local_records`))

		if _, err := s.Records().Add(ctx, LocalRecord{Name: "nas.home.lan", Type: "A", Value: "10.0.0.9", TTL: 300}); err != nil {
			t.Fatal(err)
		}
		all, err := s.Records().All(ctx)
		if err != nil || len(all) != 1 || all[0].Value != "10.0.0.9" {
			t.Fatalf("records: %v %v", all, err)
		}
	})
}
