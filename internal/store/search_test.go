package store

import (
	"context"
	"testing"
)

func seedQlog(t *testing.T, s Store) {
	t.Helper()
	batch := []QueryLogEntry{
		{At: 1000, InstanceID: "i", ClientIP: "10.0.0.5", QName: "a.example", QType: "A", Decision: "forwarded", RCode: "NOERROR"},
		{At: 2000, InstanceID: "i", ClientIP: "10.0.0.6", QName: "ads.example", QType: "A", Decision: "blocked", RCode: "NOERROR"},
		{At: 3000, InstanceID: "i", ClientIP: "10.0.0.5", QName: "b.example", QType: "AAAA", Decision: "cached", RCode: "NOERROR"},
	}
	if err := s.QueryLog().InsertBatch(context.Background(), batch); err != nil {
		t.Fatal(err)
	}
}

func TestQueryLogSearch(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		// Clean up any pre-existing query_log and stats data
		cleanupStats(t, s)

		// Regression coverage for Task 14's finding — Search used to
		// declare `var out []QueryLogEntry`, which marshals to JSON `null`
		// (not `[]`) on zero matches. The query_log table is genuinely
		// empty here (cleanupStats just wiped it, seedQlog hasn't run yet),
		// so this is the exact zero-row case a fresh instance's query log
		// starts in.
		empty, err := s.QueryLog().Search(ctx, QueryLogFilter{})
		if err != nil {
			t.Fatal(err)
		}
		mustMarshalArray(t, empty)

		seedQlog(t, s)
		all, err := s.QueryLog().Search(ctx, QueryLogFilter{})
		if err != nil || len(all) != 3 {
			t.Fatalf("all: %d %v", len(all), err)
		}
		if all[0].QName != "b.example" {
			t.Fatalf("order not newest-first: %v", all[0].QName)
		}
		blocked, _ := s.QueryLog().Search(ctx, QueryLogFilter{Decision: "blocked"})
		if len(blocked) != 1 || blocked[0].QName != "ads.example" {
			t.Fatalf("decision filter: %v", blocked)
		}
		sub, _ := s.QueryLog().Search(ctx, QueryLogFilter{QNameContains: "ds.exa"})
		if len(sub) != 1 {
			t.Fatalf("substring filter: %v", sub)
		}
		ranged, _ := s.QueryLog().Search(ctx, QueryLogFilter{FromMs: 1500, ToMs: 2500})
		if len(ranged) != 1 || ranged[0].QName != "ads.example" {
			t.Fatalf("range filter: %v", ranged)
		}
		client, _ := s.QueryLog().Search(ctx, QueryLogFilter{ClientIP: "10.0.0.5", Limit: 1})
		if len(client) != 1 || client[0].QName != "b.example" {
			t.Fatalf("client+limit: %v", client)
		}
		page2, _ := s.QueryLog().Search(ctx, QueryLogFilter{ClientIP: "10.0.0.5", Limit: 1, Offset: 1})
		if len(page2) != 1 || page2[0].QName != "a.example" {
			t.Fatalf("offset: %v", page2)
		}
	})
}

func TestTimelineAndSettingsAll(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		// Clean up any pre-existing query_log and stats data
		cleanupStats(t, s)

		seedQlog(t, s)
		if _, err := s.Stats().Rollup(ctx, 0); err != nil {
			t.Fatal(err)
		}
		tl, err := s.Stats().Timeline(ctx, 0)
		if err != nil || len(tl) == 0 {
			t.Fatalf("timeline: %v %v", tl, err)
		}
		var total int64
		for _, decisions := range tl {
			for _, n := range decisions {
				total += n
			}
		}
		if total != 3 {
			t.Fatalf("timeline total %d", total)
		}
		_ = s.Settings().SetInternal(ctx, "k1", "v1")
		m, err := s.Settings().All(ctx)
		if err != nil || m["k1"] != "v1" {
			t.Fatalf("settings all: %v %v", m, err)
		}
	})
}
