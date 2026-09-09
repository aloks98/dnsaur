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

// TestMigrateExistingZoneGainsSOATTL is 0006's upgrade path. A zone row
// written before soa_ttl existed has to come out of the migration with a
// usable header TTL: 0 would mean every negative answer this zone hands out
// is uncacheable (RFC 2308 §5 takes min(SOAMinimum, SOATTL)), turning a
// silent schema addition into a query-rate regression for every resolver
// pointed at it. The default matches soa_minimum's, so nothing changes for
// a row that never set either.
func TestMigrateExistingZoneGainsSOATTL(t *testing.T) {
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "prezones.db")

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
	// Stop at 0004: zones exists, soa_ttl does not.
	if _, err := provider.UpTo(ctx, 4); err != nil {
		t.Fatalf("legacy migrate: %v", err)
	}
	if _, err := raw.ExecContext(ctx,
		`INSERT INTO zones (name, type, enabled, soa_ns, soa_mbox, soa_serial, soa_minimum)
		 VALUES ('legacy.test', 'primary', 1, 'ns.legacy.test', 'hostadmin.legacy.test', 4, 600)`); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(ctx, "sqlite", dsn)
	if err != nil {
		t.Fatalf("upgrading a database with zones failed: %v", err)
	}
	defer func() { _ = s.Close() }()

	zs, err := s.Zones().Zones(ctx)
	if err != nil {
		t.Fatalf("reading zones after upgrade: %v", err)
	}
	// The migrator also seeds the RFC 6303 built-in zones (version 7, see
	// builtins.go) on the same Open, so the total is the pre-existing row
	// plus exactly BuiltinZones — not "at least" that many, which would let
	// a duplicate- or extra-zone bug in this exact upgrade path slip past.
	if want := 1 + len(BuiltinZones); len(zs) != want {
		t.Fatalf("zones after upgrade = %d (%+v), want %d (legacy.test + the built-ins)", len(zs), zs, want)
	}
	var legacy *Zone
	for i := range zs {
		if zs[i].Name == "legacy.test" {
			legacy = &zs[i]
		}
	}
	if legacy == nil {
		t.Fatalf("zones after upgrade = %+v, want the pre-existing legacy.test among them", zs)
	}
	if legacy.SOATTL != 900 {
		t.Errorf("SOATTL = %d, want the 900 default — a 0 here makes every negative answer uncacheable", legacy.SOATTL)
	}
	// The rest of the row is untouched.
	if legacy.SOAMinimum != 600 || legacy.SOASerial != 4 {
		t.Errorf("row mangled by the migration: %+v", legacy)
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

// The pragmas are a query string, so a DSN that already carries one has to
// be extended rather than restarted: "file:x.db?mode=ro" + "?_pragma=..." is
// not a URI any driver reads back the way it was meant.
func TestSQLiteDSNKeepsAnExistingQuery(t *testing.T) {
	const pragmas = "_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	for _, tc := range []struct {
		name, dsn, want string
	}{
		{"plain path", "/var/lib/dnsaur/dnsaur.db", "/var/lib/dnsaur/dnsaur.db?" + pragmas},
		{"uri with a query", "file:dnsaur.db?mode=ro", "file:dnsaur.db?mode=ro&" + pragmas},
		{"uri with an empty query", "file:dnsaur.db?", "file:dnsaur.db?&" + pragmas},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sqliteDSN(tc.dsn); got != tc.want {
				t.Errorf("sqliteDSN(%q) = %q, want %q", tc.dsn, got, tc.want)
			}
		})
	}
}

// The pragmas have to survive the join, not merely be separated correctly: a
// URI DSN that lost foreign_keys(1) would drop every cascade the schema
// relies on, silently.
func TestSQLiteDSNAppliesPragmasToAURIWithAQuery(t *testing.T) {
	dsn := "file:" + filepath.Join(t.TempDir(), "t.db") + "?_txlock=immediate"
	s, err := Open(context.Background(), "sqlite", dsn)
	if err != nil {
		t.Fatalf("Open(%q): %v", dsn, err)
	}
	t.Cleanup(func() { _ = s.Close() })

	var fk int
	if err := s.(*sqlStore).db.QueryRowContext(context.Background(), "PRAGMA foreign_keys").Scan(&fk); err != nil {
		t.Fatal(err)
	}
	if fk != 1 {
		t.Error("foreign_keys is off: the pragmas did not survive being appended to a DSN that already had a query")
	}
}
