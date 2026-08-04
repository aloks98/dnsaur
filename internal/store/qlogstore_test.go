package store

import (
	"context"
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
