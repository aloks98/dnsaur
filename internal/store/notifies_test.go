package store

import (
	"context"
	"testing"
)

// seedNotifyZone inserts a primary zone and returns its id. Every test here
// needs one, because zone_notifies.zone_id is a real foreign key.
//
// name is passed through testGroupName rather than used as-is — the design's
// fixed "example.com"/"other.example" would collide with themselves the same
// way TestDeleteRefusesAKeyNamedByAllowTransfer's comment (tsigkeys_test.go)
// already explains: zones.name is UNIQUE and postgres here is one database
// shared across this whole package's test run, so a literal name reused
// across the several test functions in this file would collide. None of the
// assertions below read the zone's name back, so this has no effect on what
// is actually being tested.
func seedNotifyZone(t *testing.T, s Store, name string) int64 {
	t.Helper()
	name = testGroupName(name)
	id, err := s.Zones().AddZone(context.Background(), Zone{
		Name: name, Type: "primary", Enabled: true,
		SOANS: "ns1." + name, SOAMbox: "hostmaster." + name,
		SOASerial: 1, SOARefresh: 3600, SOARetry: 600,
		SOAExpire: 604800, SOAMinimum: 300, SOATTL: 900,
	})
	if err != nil {
		t.Fatalf("AddZone: %v", err)
	}
	return id
}

func TestNotifyReconcileCreatesAndRemoves(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		zoneID := seedNotifyZone(t, s, "example.com")
		ns := s.Notifies()

		// Creating: two targets, neither with a row yet.
		if err := ns.Reconcile(ctx, zoneID, []string{"10.0.0.2:53", "10.0.0.3:53"}, 1000); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		rows, err := ns.ByZone(ctx, zoneID)
		if err != nil {
			t.Fatalf("ByZone: %v", err)
		}
		if len(rows) != 2 {
			t.Fatalf("got %d rows, want 2", len(rows))
		}
		for _, r := range rows {
			// created_at is what dates a target that has never been notified;
			// without it the screen's "added 2m ago" has nothing to read and
			// a never-notified row shows a blank cell instead of an answer.
			if r.CreatedAt != 1000 {
				t.Errorf("target %s: created_at = %d, want 1000", r.Target, r.CreatedAt)
			}
			if r.NotifiedAt != 0 {
				t.Errorf("target %s: notified_at = %d, want 0 (never)", r.Target, r.NotifiedAt)
			}
		}

		// Idempotent: reconciling the same list again must not duplicate a
		// row or restamp created_at, or every pass would reset the age of
		// every target.
		if err := ns.Reconcile(ctx, zoneID, []string{"10.0.0.2:53", "10.0.0.3:53"}, 2000); err != nil {
			t.Fatalf("second Reconcile: %v", err)
		}
		rows, _ = ns.ByZone(ctx, zoneID)
		if len(rows) != 2 {
			t.Fatalf("after re-reconcile got %d rows, want 2", len(rows))
		}
		for _, r := range rows {
			if r.CreatedAt != 1000 {
				t.Errorf("target %s: created_at moved to %d", r.Target, r.CreatedAt)
			}
		}

		// Removing: a target that left notify_to loses its row, and a new
		// one gains one, in the same call.
		if err := ns.Reconcile(ctx, zoneID, []string{"10.0.0.3:53", "ns4.example.com:5353"}, 3000); err != nil {
			t.Fatalf("third Reconcile: %v", err)
		}
		rows, _ = ns.ByZone(ctx, zoneID)
		got := map[string]int64{}
		for _, r := range rows {
			got[r.Target] = r.CreatedAt
		}
		if len(got) != 2 {
			t.Fatalf("got %v, want two targets", got)
		}
		if _, ok := got["10.0.0.2:53"]; ok {
			t.Error("10.0.0.2:53 left notify_to but kept its row")
		}
		if got["10.0.0.3:53"] != 1000 {
			t.Errorf("10.0.0.3:53 created_at = %d, want the original 1000", got["10.0.0.3:53"])
		}
		if got["ns4.example.com:5353"] != 3000 {
			t.Errorf("new target created_at = %d, want 3000", got["ns4.example.com:5353"])
		}

		// An empty list clears every row: a zone whose notify_to was emptied
		// notifies nobody and should carry no delivery state either.
		if err := ns.Reconcile(ctx, zoneID, nil, 4000); err != nil {
			t.Fatalf("clearing Reconcile: %v", err)
		}
		rows, _ = ns.ByZone(ctx, zoneID)
		if len(rows) != 0 {
			t.Fatalf("got %d rows after clearing, want 0", len(rows))
		}
	})
}

// ParseNotifyTo does not dedupe, and ValidateNotifyTo is only ParseNotifyTo,
// so notify_to = "10.0.0.2, 10.0.0.2" is an accepted write. Reconcile must
// survive reconciling it: a targets list naming the same address twice has
// to insert it once, not violate UNIQUE(zone_id, target) and roll back —
// which would fail every notify pass for that zone from then on, reported
// far from the edit that caused it.
func TestNotifyReconcileDedupesTargets(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		zoneID := seedNotifyZone(t, s, "example.com")
		ns := s.Notifies()
		if err := ns.Reconcile(ctx, zoneID, []string{"10.0.0.2:53", "10.0.0.2:53"}, 1000); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		rows, err := ns.ByZone(ctx, zoneID)
		if err != nil {
			t.Fatalf("ByZone: %v", err)
		}
		if len(rows) != 1 {
			t.Fatalf("got %d rows, want 1", len(rows))
		}
	})
}

func TestNotifyWritersOwnDisjointColumns(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		zoneID := seedNotifyZone(t, s, "example.com")
		ns := s.Notifies()
		if err := ns.Reconcile(ctx, zoneID, []string{"10.0.0.2:53"}, 1000); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		rows, _ := ns.ByZone(ctx, zoneID)
		id := rows[0].ID

		// A failed attempt records the round, the count, the retry time and
		// the reason — and must not touch the delivered columns, or a target
		// that has been current for a week would read as never notified the
		// moment one retry failed.
		if err := ns.NoteAttempt(ctx, id, 47, 2, 5000, "i/o timeout"); err != nil {
			t.Fatalf("NoteAttempt: %v", err)
		}
		rows, _ = ns.ByZone(ctx, zoneID)
		r := rows[0]
		if r.PendingSerial != 47 || r.Attempts != 2 || r.NextAttemptAt != 5000 || r.LastError != "i/o timeout" {
			t.Fatalf("NoteAttempt wrote %+v", r)
		}
		if r.NotifiedSerial != 0 || r.NotifiedAt != 0 {
			t.Errorf("NoteAttempt touched the delivered columns: %+v", r)
		}
		if r.CreatedAt != 1000 {
			t.Errorf("NoteAttempt moved created_at to %d", r.CreatedAt)
		}

		// A success records delivery and clears the retry state, so a target
		// that recovered stops reporting an error it no longer has — the same
		// rule NoteTransferAttempt follows for last_error.
		if err := ns.NoteDelivered(ctx, id, 47, 6000); err != nil {
			t.Fatalf("NoteDelivered: %v", err)
		}
		rows, _ = ns.ByZone(ctx, zoneID)
		r = rows[0]
		if r.NotifiedSerial != 47 || r.NotifiedAt != 6000 {
			t.Fatalf("NoteDelivered wrote %+v", r)
		}
		if r.Attempts != 0 || r.LastError != "" || r.NextAttemptAt != 0 {
			t.Errorf("NoteDelivered left retry state behind: %+v", r)
		}
		if r.CreatedAt != 1000 {
			t.Errorf("NoteDelivered moved created_at to %d", r.CreatedAt)
		}

		// A second case: a serial above 2^31 must round-trip intact through
		// the driver. This is the entire reason the postgres migration uses
		// BIGINT rather than INTEGER for pending_serial/notified_serial
		// (0012_zone_notify.sql) — nothing else here pins that round trip on
		// either driver.
		const bigSerial = 0xFFFFFFFF
		if err := ns.NoteAttempt(ctx, id, bigSerial, 3, 9000, "timeout"); err != nil {
			t.Fatalf("NoteAttempt(big serial): %v", err)
		}
		rows, _ = ns.ByZone(ctx, zoneID)
		if rows[0].PendingSerial != bigSerial {
			t.Errorf("PendingSerial round-tripped as %d, want %d", rows[0].PendingSerial, bigSerial)
		}
		if err := ns.NoteDelivered(ctx, id, bigSerial, 10000); err != nil {
			t.Fatalf("NoteDelivered(big serial): %v", err)
		}
		rows, _ = ns.ByZone(ctx, zoneID)
		if rows[0].NotifiedSerial != bigSerial {
			t.Errorf("NotifiedSerial round-tripped as %d, want %d", rows[0].NotifiedSerial, bigSerial)
		}
	})
}

// zone_id is a real foreign key with ON DELETE CASCADE, so deleting a zone
// takes its queue rows with it and no application code has to remember.
func TestNotifyRowsCascadeOnZoneDelete(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		zoneID := seedNotifyZone(t, s, "example.com")
		other := seedNotifyZone(t, s, "other.example")
		ns := s.Notifies()
		if err := ns.Reconcile(ctx, zoneID, []string{"10.0.0.2:53"}, 1000); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		if err := ns.Reconcile(ctx, other, []string{"10.0.0.9:53"}, 1000); err != nil {
			t.Fatalf("Reconcile other: %v", err)
		}

		if err := s.Zones().DeleteZone(ctx, zoneID); err != nil {
			t.Fatalf("DeleteZone: %v", err)
		}
		all, err := ns.All(ctx)
		if err != nil {
			t.Fatalf("All: %v", err)
		}
		// otherCount, not len(all): postgres is one database shared across
		// this whole package's test run (see seedNotifyZone's comment), and
		// TestNotifyWritersOwnDisjointColumns above leaves its own row
		// behind on purpose (it is not testing deletion) — so the table can
		// hold rows unrelated to either zone here. What this test owns is
		// that the deleted zone's row is gone and the untouched zone's row
		// is not, not the table's total size.
		var otherCount int
		for _, r := range all {
			if r.ZoneID == zoneID {
				t.Fatalf("row for the deleted zone survived: %+v", r)
			}
			if r.ZoneID == other {
				otherCount++
			}
		}
		if otherCount != 1 {
			t.Fatalf("got %d rows for the untouched zone, want 1", otherCount)
		}
	})
}
