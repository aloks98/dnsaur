package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// testGroupName generates a unique group name to avoid conflicts in shared postgres DB across multiple test runs.
func testGroupName(base string) string {
	return base + "_" + time.Now().Format("150405.000")
}

func TestClientGroupCRUD(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		// Ensure id >= 2 so tests can check hardcoded id==1 constraint.
		// Use unique names to avoid conflicts in shared postgres DB across -count=2 runs.
		_, _ = s.Clients().AddGroup(ctx, testGroupName("default"))
		gid, _ := s.Clients().AddGroup(ctx, testGroupName("g1"))
		cid, _ := s.Clients().AddClient(ctx, Client{Name: "c", Matcher: "10.9.9.9", GroupID: gid})

		if err := s.Clients().UpdateClient(ctx, Client{ID: cid, Name: "c2", Matcher: "10.9.9.8", GroupID: gid}); err != nil {
			t.Fatal(err)
		}
		cls, _ := s.Clients().Clients(ctx)
		var found Client
		for _, c := range cls {
			if c.ID == cid {
				found = c
			}
		}
		if found.Name != "c2" || found.Matcher != "10.9.9.8" {
			t.Fatalf("update: %+v", found)
		}
		if err := s.Clients().DeleteGroup(ctx, gid); !errors.Is(err, ErrInUse) {
			t.Fatalf("delete referenced group: %v", err)
		}
		if err := s.Clients().DeleteClient(ctx, cid); err != nil {
			t.Fatal(err)
		}
		if err := s.Clients().DeleteGroup(ctx, gid); err != nil {
			t.Fatal(err)
		}
		if err := s.Clients().DeleteGroup(ctx, 1); !errors.Is(err, ErrInUse) {
			t.Fatalf("default group must be undeletable: %v", err)
		}
		if err := s.Clients().DeleteClient(ctx, 99999); !errors.Is(err, ErrNotFound) {
			t.Fatalf("missing row: %v", err)
		}
	})
}

func TestFilterAndRecordCRUD(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		// Ensure id >= 2 so tests can check hardcoded id==1 constraint.
		// Use unique names to avoid conflicts in shared postgres DB across -count=2 runs.
		_, _ = s.Clients().AddGroup(ctx, testGroupName("default"))
		gid, _ := s.Clients().AddGroup(ctx, testGroupName("gf"))
		lid, _ := s.Filters().AddList(ctx, List{URL: testGroupName("https://x.example/l1"), Kind: "block", Enabled: true})
		_ = s.Filters().AssignList(ctx, gid, lid)
		rid, _ := s.Filters().AddRule(ctx, Rule{GroupID: gid, Action: "block", Pattern: "x.example"})

		if err := s.Filters().SetListEnabled(ctx, lid, false); err != nil {
			t.Fatal(err)
		}
		ls, _ := s.Filters().Lists(ctx)
		if ls[len(ls)-1].Enabled {
			t.Fatal("list still enabled")
		}
		if err := s.Filters().UnassignList(ctx, gid, lid); err != nil {
			t.Fatal(err)
		}
		if got, _ := s.Filters().ListsForGroup(ctx, gid); len(got) != 0 {
			t.Fatalf("unassign failed: %v", got)
		}
		_ = s.Filters().AssignList(ctx, gid, lid)
		if err := s.Filters().DeleteList(ctx, lid); err != nil {
			t.Fatal(err)
		}
		if got, _ := s.Filters().ListsForGroup(ctx, gid); len(got) != 0 {
			t.Fatalf("delete left group_lists rows: %v", got)
		}
		if err := s.Filters().DeleteRule(ctx, rid); err != nil {
			t.Fatal(err)
		}
		if rs, _ := s.Filters().Rules(ctx, gid); len(rs) != 0 {
			t.Fatalf("rule not deleted: %v", rs)
		}

		recID, _ := s.Records().Add(ctx, LocalRecord{Name: "u.home.lan", Type: "A", Value: "10.0.0.1", TTL: 60})
		if err := s.Records().Update(ctx, LocalRecord{ID: recID, Name: "u.home.lan", Type: "A", Value: "10.0.0.2", TTL: 90}); err != nil {
			t.Fatal(err)
		}
		all, _ := s.Records().All(ctx)
		if all[len(all)-1].Value != "10.0.0.2" || all[len(all)-1].TTL != 90 {
			t.Fatalf("record update: %+v", all[len(all)-1])
		}
		if err := s.Records().Delete(ctx, recID); err != nil {
			t.Fatal(err)
		}
		if err := s.Records().Delete(ctx, recID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("double delete: %v", err)
		}
	})
}

// TestListRefreshStatusRoundTrip pins the persistence half of "make
// filter-list fetch failures visible": every outcome has to survive a write
// and a read on both dialects, a recovery has to *clear* the previous error
// (otherwise a healed list wears a permanent red badge and the signal gets
// ignored), and a stale list has to keep last_refreshed pointing at the copy
// it is still serving rather than at the attempt that just failed.
func TestListRefreshStatusRoundTrip(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		lid, err := s.Filters().AddList(ctx, List{URL: testGroupName("https://status.example/l"), Kind: "block", Enabled: true})
		if err != nil {
			t.Fatal(err)
		}
		get := func() List {
			t.Helper()
			ls, err := s.Filters().Lists(ctx)
			if err != nil {
				t.Fatal(err)
			}
			for _, l := range ls {
				if l.ID == lid {
					return l
				}
			}
			t.Fatalf("list %d vanished", lid)
			return List{}
		}

		// A brand-new subscription has never been attempted. This is the
		// only thing `0 entries / never refreshed` is allowed to mean.
		if got := get(); got.LastStatus != ListStatusPending || got.LastError != "" || got.LastAttempt != 0 {
			t.Fatalf("fresh list = %+v, want pending with no attempt", got)
		}

		// Failed: the attempt failed and nothing is being served.
		if err := s.Filters().MarkListFailed(ctx, lid, 1000, 0, "404 Not Found"); err != nil {
			t.Fatal(err)
		}
		got := get()
		if got.LastStatus != ListStatusFailed || got.LastError != "404 Not Found" || got.LastAttempt != 1000 {
			t.Fatalf("failed = %+v", got)
		}
		if got.LastRefreshed != 0 {
			t.Fatalf("failed list claims a successful refresh: %+v", got)
		}

		// OK: a success stores entries and clears the error.
		if err := s.Filters().TouchList(ctx, lid, 2000, 99277); err != nil {
			t.Fatal(err)
		}
		if got := get(); got.LastStatus != ListStatusOK || got.LastError != "" ||
			got.EntryCount != 99277 || got.LastRefreshed != 2000 || got.LastAttempt != 2000 {
			t.Fatalf("ok = %+v, want the error cleared and both timestamps at 2000", got)
		}

		// Stale: a later failure that still serves the cached copy keeps
		// last_refreshed at the copy's own date (2000), not the attempt's.
		if err := s.Filters().MarkListFailed(ctx, lid, 3000, 99277, "404 Not Found"); err != nil {
			t.Fatal(err)
		}
		if got := get(); got.LastStatus != ListStatusStale || got.LastRefreshed != 2000 ||
			got.LastAttempt != 3000 || got.EntryCount != 99277 {
			t.Fatalf("stale = %+v, want last_refreshed pinned to the served copy", got)
		}

		// Empty: the download worked, so last_refreshed advances; the
		// entries are gone and the reason says why.
		if err := s.Filters().MarkListEmpty(ctx, lid, 4000, "fetched 4.5 MB, no usable entries — 250,431 lines skipped"); err != nil {
			t.Fatal(err)
		}
		if got := get(); got.LastStatus != ListStatusEmpty || got.EntryCount != 0 ||
			got.LastRefreshed != 4000 || !strings.Contains(got.LastError, "250,431 lines skipped") {
			t.Fatalf("empty = %+v", got)
		}

		// ListsForGroup serves the same columns as Lists — the Groups &
		// Clients screen reads that one and must see the state too.
		gid, err := s.Clients().AddGroup(ctx, testGroupName("status-group"))
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Filters().AssignList(ctx, gid, lid); err != nil {
			t.Fatal(err)
		}
		gl, err := s.Filters().ListsForGroup(ctx, gid)
		if err != nil {
			t.Fatal(err)
		}
		if len(gl) != 1 || gl[0].LastStatus != ListStatusEmpty || gl[0].LastAttempt != 4000 {
			t.Fatalf("ListsForGroup dropped the status: %+v", gl)
		}
	})
}

// mustMarshalArray json.Marshals v and fails the test unless the result is
// exactly "[]" — the point of this helper (over just checking len(v) == 0)
// is that a nil Go slice and an empty-but-non-nil one both have len 0, yet
// encoding/json renders them completely differently ("null" vs "[]"). Every
// list-returning store method must produce a real JSON array even with zero
// rows, since API clients (the web dashboard chief among them) decode
// straight into a typed slice and call .length/.map on it unconditionally.
func mustMarshalArray(t *testing.T, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(b) != "[]" {
		t.Fatalf("got %s, want []  (a nil slice marshals to `null`, which crashes API clients that call .length/.map on an empty list without a null-check)", b)
	}
}

// cleanupCRUDTables deletes every row from the tables TestEmptyListsMarshal
// AsJSONArrayNotNull needs empty, child tables first to satisfy foreign
// keys. sqlite already gets test isolation for free (each forEachDriver
// case opens its own tmpdir DB, see openSQLite), but postgres is one
// container shared by every test in the package for the whole run (see
// TestMain/openPostgres) — without this, leftover rows from
// TestClientGroupCRUD/TestFilterAndRecordCRUD/etc. would make the
// "zero rows" assertions below flaky depending on test order, the same
// defensive pattern statsstore_test.go's cleanupStats already uses for the
// stats tables.
func cleanupCRUDTables(t *testing.T, s Store) {
	t.Helper()
	ss := s.(*sqlStore)
	ctx := context.Background()
	for _, table := range []string{"group_lists", "rules", "clients", "local_records", "lists", "auth_tokens", "groups"} {
		if _, err := ss.db.ExecContext(ctx, ss.q(`DELETE FROM `+table)); err != nil {
			t.Fatalf("cleanup %s: %v", table, err)
		}
	}
}

// TestEmptyListsMarshalAsJSONArrayNotNull guards against a real regression
// (found by actually driving the built dashboard in a browser against a
// fresh instance, Task 14): every list-returning store method here used to
// declare its accumulator as `var out []T`, which stays a nil slice — and
// therefore marshals to JSON `null`, not `[]` — whenever a query matches
// zero rows. A brand-new dnsaur instance has zero clients, zero rules, zero
// local records, and zero API tokens by default, so this wasn't a rare edge
// case: it broke the Local DNS, Filtering › Rules, Filtering › Groups &
// Clients, and Account › API Tokens pages on first run, every time.
func TestEmptyListsMarshalAsJSONArrayNotNull(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		cleanupCRUDTables(t, s)
		// With every relevant table now empty, each of these is the exact
		// zero-row case that used to marshal as `null`.
		groups, err := s.Clients().Groups(ctx)
		if err != nil {
			t.Fatal(err)
		}
		mustMarshalArray(t, groups)

		clients, err := s.Clients().Clients(ctx)
		if err != nil {
			t.Fatal(err)
		}
		mustMarshalArray(t, clients)

		lists, err := s.Filters().Lists(ctx)
		if err != nil {
			t.Fatal(err)
		}
		mustMarshalArray(t, lists)

		gid, err := s.Clients().AddGroup(ctx, testGroupName("empty-lists-group"))
		if err != nil {
			t.Fatal(err)
		}
		listsForGroup, err := s.Filters().ListsForGroup(ctx, gid)
		if err != nil {
			t.Fatal(err)
		}
		mustMarshalArray(t, listsForGroup)

		rules, err := s.Filters().Rules(ctx, gid)
		if err != nil {
			t.Fatal(err)
		}
		mustMarshalArray(t, rules)

		records, err := s.Records().All(ctx)
		if err != nil {
			t.Fatal(err)
		}
		mustMarshalArray(t, records)
	})
}

// A uniqueness violation must surface as ErrDuplicate, not as a raw driver
// error. The API maps the sentinel to 409; without it a duplicate name was
// answered with 503 "storage unavailable", i.e. user input error reported as
// infrastructure failure. Runs on both drivers because the detection differs:
// sqlite result codes vs postgres SQLSTATE 23505.
func TestDuplicateSurfacesAsErrDuplicate(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		c := s.Clients()

		name := testGroupName("dup")
		if _, err := c.AddGroup(ctx, name); err != nil {
			t.Fatalf("first AddGroup: %v", err)
		}
		if _, err := c.AddGroup(ctx, name); !errors.Is(err, ErrDuplicate) {
			t.Fatalf("duplicate group name: err = %v, want ErrDuplicate", err)
		}

		gid, err := c.AddGroup(ctx, testGroupName("dup2"))
		if err != nil {
			t.Fatalf("AddGroup: %v", err)
		}
		matcher := "10.77.0." + testGroupName("")[len(testGroupName(""))-2:]
		if _, err := c.AddClient(ctx, Client{Name: "a", Matcher: matcher, GroupID: gid}); err != nil {
			t.Fatalf("first AddClient: %v", err)
		}
		if _, err := c.AddClient(ctx, Client{Name: "b", Matcher: matcher, GroupID: gid}); !errors.Is(err, ErrDuplicate) {
			t.Fatalf("duplicate matcher: err = %v, want ErrDuplicate", err)
		}

		// Renames go through execOne, not insert — cover that path too.
		if err := c.RenameGroup(ctx, gid, name); !errors.Is(err, ErrDuplicate) {
			t.Fatalf("rename onto an existing name: err = %v, want ErrDuplicate", err)
		}

		// A genuine miss must still be ErrNotFound, not ErrDuplicate.
		if err := c.RenameGroup(ctx, 99999, testGroupName("nobody")); !errors.Is(err, ErrNotFound) {
			t.Fatalf("rename missing group: err = %v, want ErrNotFound", err)
		}
	})
}
