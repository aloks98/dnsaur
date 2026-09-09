package store

import (
	"context"
	"fmt"
	"strconv"
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

// TestStatsPruneBefore: stats_hourly holds a row per hour per name and per
// client, so it grows for as long as the instance runs. Retention deletes
// whole buckets older than the cutoff and leaves everything at or after it
// alone — and, like the query-log prune, does it in bounded chunks so a
// year's worth of rows is not one exclusive statement.
func TestStatsPruneBefore(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		cleanupStats(t, s)
		smallPruneChunk(t, 10)
		ss := s.(*sqlStore)
		const oldBucket, keptBucket = int64(3600), int64(360000)
		for i := 0; i < 25; i++ {
			if _, err := ss.db.ExecContext(ctx,
				ss.q(`INSERT INTO stats_hourly (bucket, metric, key, value) VALUES (?, 'domain', ?, 1)`),
				oldBucket, fmt.Sprintf("old%d.example", i)); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := ss.db.ExecContext(ctx,
			ss.q(`INSERT INTO stats_hourly (bucket, metric, key, value) VALUES (?, 'domain', 'kept.example', 3)`),
			keptBucket); err != nil {
			t.Fatal(err)
		}
		n, err := s.Stats().PruneBefore(ctx, keptBucket)
		if err != nil {
			t.Fatal(err)
		}
		if n != 25 {
			t.Fatalf("pruned %d rows, want 25", n)
		}
		dom, err := s.Stats().Counter(ctx, 0, "domain")
		if err != nil {
			t.Fatal(err)
		}
		if len(dom) != 1 || dom["kept.example"] != 3 {
			t.Fatalf("survivors: %v", dom)
		}
	})
}

// TestRollupRecordsItsOwnWatermark: the counters and the record of how far
// they got are one fact. Written separately — counters committed, watermark
// upserted after — a failure in between left the batch counted and the
// progress unrecorded, and because the counters are additive (`value +
// excluded.value`) the next tick added the same rows again, permanently. So
// Rollup writes the watermark in its own transaction, and the caller reads
// it back rather than storing it itself.
func TestRollupRecordsItsOwnWatermark(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		cleanupStats(t, s)
		hourMs := int64(3600 * 1000)
		batch := []QueryLogEntry{
			{At: hourMs + 1, InstanceID: "i", ClientIP: "10.0.0.5", QName: "a.example", QType: "A", Decision: "forwarded", RCode: "NOERROR"},
			{At: hourMs + 2, InstanceID: "i", ClientIP: "10.0.0.5", QName: "a.example", QType: "A", Decision: "forwarded", RCode: "NOERROR"},
		}
		if err := s.QueryLog().InsertBatch(ctx, batch); err != nil {
			t.Fatal(err)
		}
		last, err := s.Stats().Rollup(ctx, 0)
		if err != nil {
			t.Fatal(err)
		}
		v, ok, err := s.Settings().Get(ctx, StatsWatermarkKey)
		if err != nil {
			t.Fatal(err)
		}
		if !ok || v != strconv.FormatInt(last, 10) {
			t.Fatalf("watermark %q (found=%v), want %d", v, ok, last)
		}
		// What the caller does on the next tick: read the watermark the
		// rollup stored, and roll up from there.
		stored, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Stats().Rollup(ctx, stored); err != nil {
			t.Fatal(err)
		}
		dec, _ := s.Stats().Counter(ctx, 0, "decision")
		if dec["forwarded"] != 2 {
			t.Fatalf("double counted: %v", dec)
		}
	})
}
