package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestQueryLogInsertAndPrune(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		batch := []QueryLogEntry{
			{At: 1000, InstanceID: "i1", ClientIP: "10.0.0.5", QName: "old.example", QType: "A", Decision: "forwarded", RCode: "NOERROR", DurationMs: 12},
			{At: 2000, InstanceID: "i1", ClientIP: "10.0.0.5", QName: "new.example", QType: "A", Decision: "blocked", RCode: "NOERROR", DurationMs: 1},
		}
		if err := s.QueryLog().InsertBatch(ctx, batch); err != nil {
			t.Fatal(err)
		}
		n, err := s.QueryLog().DeleteBefore(ctx, 1500)
		if err != nil || n != 1 {
			t.Fatalf("deleted %d err %v", n, err)
		}
	})
}

// smallPruneChunk shrinks the retention delete's chunk size for one test, so
// a handful of rows exercises the same loop a real prune runs over millions.
func smallPruneChunk(t *testing.T, n int64) {
	t.Helper()
	prev := pruneChunk
	pruneChunk = n
	t.Cleanup(func() { pruneChunk = prev })
}

// TestQueryLogPruneDeletesEveryChunk: retention pruning runs as a series of
// bounded deletes rather than one statement, so it must keep going until the
// expired rows are gone — a loop that stopped after the first chunk would
// leave most of them behind, and rows the operator's retention says are gone
// would still be in the log and in the search results.
func TestQueryLogPruneDeletesEveryChunk(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		cleanupStats(t, s)
		smallPruneChunk(t, 10)
		var batch []QueryLogEntry
		for i := 0; i < 25; i++ {
			batch = append(batch, QueryLogEntry{
				At: 1000, InstanceID: "i1", ClientIP: "10.0.0.5",
				QName: fmt.Sprintf("old%d.example", i), QType: "A", Decision: "forwarded", RCode: "NOERROR",
			})
		}
		batch = append(batch,
			QueryLogEntry{At: 9000, InstanceID: "i1", ClientIP: "10.0.0.5", QName: "kept.example", QType: "A", Decision: "forwarded", RCode: "NOERROR"},
			QueryLogEntry{At: 9001, InstanceID: "i1", ClientIP: "10.0.0.5", QName: "kept2.example", QType: "A", Decision: "blocked", RCode: "NOERROR"})
		if err := s.QueryLog().InsertBatch(ctx, batch); err != nil {
			t.Fatal(err)
		}
		n, err := s.QueryLog().DeleteBefore(ctx, 5000)
		if err != nil {
			t.Fatal(err)
		}
		if n != 25 {
			t.Fatalf("deleted %d rows, want 25", n)
		}
		left, err := s.QueryLog().Search(ctx, QueryLogFilter{})
		if err != nil {
			t.Fatal(err)
		}
		if len(left) != 2 {
			t.Fatalf("%d rows survived the prune, want the 2 inside retention", len(left))
		}
	})
}

// TestDeleteInChunksLoops pins the loop itself, which the driver tests above
// cannot distinguish from a single unbounded DELETE: it keeps asking until a
// pass comes back short, and sums what every pass removed.
func TestDeleteInChunksLoops(t *testing.T) {
	remaining := int64(25)
	calls := 0
	n, err := deleteInChunks(context.Background(), 10, func(ctx context.Context, limit int64) (int64, error) {
		calls++
		if remaining < limit {
			limit = remaining
		}
		remaining -= limit
		return limit, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n != 25 {
		t.Fatalf("reported %d rows, want 25", n)
	}
	if calls != 3 {
		t.Fatalf("%d passes, want 3 (10, 10, 5)", calls)
	}
}

// TestDeleteInChunksReportsWhatItDeleted: a failure part-way through has
// still deleted the earlier chunks, and the count has to say so rather than
// claiming nothing happened.
func TestDeleteInChunksReportsWhatItDeleted(t *testing.T) {
	boom := errors.New("database is locked")
	calls := 0
	n, err := deleteInChunks(context.Background(), 10, func(ctx context.Context, limit int64) (int64, error) {
		calls++
		if calls == 2 {
			return 0, boom
		}
		return limit, nil
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want %v", err, boom)
	}
	if n != 10 {
		t.Fatalf("reported %d rows, want the 10 the first pass deleted", n)
	}
}

// TestQueryLogRoundTripsMatched: the entry that fired is stored with the
// row, not merely computed for the live tail. `list_id` names the list, not
// the line in it, so a stored row without this can't say which of a hundred
// thousand entries matched — and a column the writer fills but the reader
// drops is the same as no column at all, which is why both halves are
// asserted here rather than just the INSERT succeeding.
func TestQueryLogRoundTripsMatched(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		cleanupStats(t, s)
		if err := s.QueryLog().InsertBatch(ctx, []QueryLogEntry{
			{At: 1000, InstanceID: "i1", ClientIP: "10.0.0.5", QName: "tracker.ads.example", QType: "A", Decision: "blocked", RCode: "NOERROR", ListID: 3, Matched: "ads.example"},
			{At: 2000, InstanceID: "i1", ClientIP: "10.0.0.5", QName: "ok.example", QType: "A", Decision: "forwarded", RCode: "NOERROR"},
		}); err != nil {
			t.Fatal(err)
		}
		got, err := s.QueryLog().Search(ctx, QueryLogFilter{})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 {
			t.Fatalf("searched %d rows, want 2", len(got))
		}
		// ORDER BY id DESC: the forwarded row first.
		if got[0].Matched != "" {
			t.Errorf("forwarded row Matched = %q, want empty", got[0].Matched)
		}
		if got[1].Matched != "ads.example" {
			t.Errorf("blocked row Matched = %q, want the list entry that fired", got[1].Matched)
		}
	})
}
