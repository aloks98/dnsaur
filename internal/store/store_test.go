package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
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
		gid, err := s.Clients().AddGroup(ctx, "kids")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Clients().AddClient(ctx, Client{Name: "tablet", Matcher: "10.0.0.5", GroupID: gid}); err != nil {
			t.Fatal(err)
		}
		lid, err := s.Filters().AddList(ctx, List{URL: "https://example.com/hosts", Kind: "block", Enabled: true})
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
		if err != nil || len(ls) != 1 || ls[0].URL != "https://example.com/hosts" {
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
		if _, err := s.Records().Add(ctx, LocalRecord{Name: "nas.home.lan", Type: "A", Value: "10.0.0.9", TTL: 300}); err != nil {
			t.Fatal(err)
		}
		all, err := s.Records().All(ctx)
		if err != nil || len(all) != 1 || all[0].Value != "10.0.0.9" {
			t.Fatalf("records: %v %v", all, err)
		}
	})
}
