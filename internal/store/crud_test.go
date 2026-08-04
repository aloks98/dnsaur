package store

import (
	"context"
	"errors"
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
