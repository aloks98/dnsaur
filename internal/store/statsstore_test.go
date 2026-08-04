package store

import (
	"context"
	"testing"
)

func cleanupStats(t *testing.T, s Store) {
	// Helper to clean up stats tables between tests
	ctx := context.Background()
	ss := s.(*sqlStore)
	_, _ = ss.db.ExecContext(ctx, ss.q(`DELETE FROM stats_hourly`))
	_, _ = ss.db.ExecContext(ctx, ss.q(`DELETE FROM query_log`))
}

func TestRollupAndCounter(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		cleanupStats(t, s)
		hourMs := int64(3600 * 1000)
		batch := []QueryLogEntry{
			{At: hourMs + 1, InstanceID: "i", ClientIP: "10.0.0.5", QName: "a.example", QType: "A", Decision: "forwarded", RCode: "NOERROR"},
			{At: hourMs + 2, InstanceID: "i", ClientIP: "10.0.0.5", QName: "a.example", QType: "A", Decision: "cached", RCode: "NOERROR"},
			{At: hourMs + 3, InstanceID: "i", ClientIP: "10.0.0.6", QName: "ads.example", QType: "A", Decision: "blocked", RCode: "NOERROR"},
		}
		if err := s.QueryLog().InsertBatch(ctx, batch); err != nil {
			t.Fatal(err)
		}
		last, err := s.Stats().Rollup(ctx, 0)
		if err != nil || last == 0 {
			t.Fatalf("rollup: last=%d err=%v", last, err)
		}
		dec, err := s.Stats().Counter(ctx, 0, "decision")
		if err != nil {
			t.Fatal(err)
		}
		if dec["forwarded"] != 1 || dec["cached"] != 1 || dec["blocked"] != 1 {
			t.Fatalf("decisions: %v", dec)
		}
		dom, _ := s.Stats().Counter(ctx, 0, "domain")
		if dom["a.example"] != 2 || dom["ads.example"] != 0 {
			t.Fatalf("domains: %v", dom)
		}
		bd, _ := s.Stats().Counter(ctx, 0, "blocked_domain")
		if bd["ads.example"] != 1 {
			t.Fatalf("blocked domains: %v", bd)
		}
		// idempotent: second rollup from watermark adds nothing
		if _, err := s.Stats().Rollup(ctx, last); err != nil {
			t.Fatal(err)
		}
		dec2, _ := s.Stats().Counter(ctx, 0, "decision")
		if dec2["forwarded"] != 1 {
			t.Fatalf("double counted: %v", dec2)
		}
	})
}
