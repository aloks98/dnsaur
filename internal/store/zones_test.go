package store

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestZoneStoreRoundTrip(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		id, err := s.Zones().AddZone(ctx, Zone{
			Name: testGroupName("e412.in"), Type: "primary", Enabled: true,
			SOANS: "ns.e412.in", SOAMbox: "hostadmin.e412.in",
			SOASerial: 1, SOARefresh: 900, SOARetry: 300,
			SOAExpire: 604800, SOAMinimum: 900,
		})
		if err != nil {
			t.Fatalf("AddZone: %v", err)
		}
		if _, err := s.Zones().AddRecord(ctx, ZoneRecord{
			ZoneID: id, Name: "bifrost", Type: "A", TTL: 3600,
			RData: "57.129.69.158", Enabled: true,
		}); err != nil {
			t.Fatalf("AddRecord: %v", err)
		}

		// A second zone with its own record: Records must filter by
		// zone_id, not just return every row in zone_records. With only one
		// zone in play (the original brief test), a broken `WHERE zone_id =
		// ?` would still pass.
		otherID, err := s.Zones().AddZone(ctx, Zone{Name: testGroupName("other.test"), Type: "primary", Enabled: true, SOASerial: 1})
		if err != nil {
			t.Fatalf("AddZone(other): %v", err)
		}
		if _, err := s.Zones().AddRecord(ctx, ZoneRecord{
			ZoneID: otherID, Name: "host", Type: "A", TTL: 300, RData: "10.0.0.9", Enabled: true,
		}); err != nil {
			t.Fatalf("AddRecord(other): %v", err)
		}

		recs, err := s.Zones().Records(ctx, id)
		if err != nil || len(recs) != 1 {
			t.Fatalf("Records = %v, %v; want 1 record", recs, err)
		}
		if recs[0].RData != "57.129.69.158" {
			t.Errorf("RData = %q, want 57.129.69.158", recs[0].RData)
		}
		// Deleting a zone must take its records with it (ON DELETE CASCADE);
		// orphan records would resurface if the apex were ever recreated.
		if err := s.Zones().DeleteZone(ctx, id); err != nil {
			t.Fatalf("DeleteZone: %v", err)
		}
		if recs, _ := s.Zones().Records(ctx, id); len(recs) != 0 {
			t.Errorf("records survived zone delete: %v", recs)
		}
		// The unrelated zone's record must survive — the cascade must not
		// have reached beyond the deleted zone's own rows.
		if recs, _ := s.Zones().Records(ctx, otherID); len(recs) != 1 {
			t.Errorf("unrelated zone's records = %v, want 1 (untouched)", recs)
		}
	})
}

func TestBumpSerialIncrements(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		id, err := s.Zones().AddZone(ctx, Zone{Name: testGroupName("a.test"), Type: "primary", Enabled: true, SOASerial: 7})
		if err != nil {
			t.Fatalf("AddZone: %v", err)
		}
		if err := s.Zones().BumpSerial(ctx, id); err != nil {
			t.Fatalf("BumpSerial: %v", err)
		}
		z, err := s.Zones().Zone(ctx, id)
		if err != nil {
			t.Fatalf("Zone: %v", err)
		}
		if z.SOASerial != 8 {
			t.Errorf("SOASerial = %d, want 8", z.SOASerial)
		}
	})
}

// TestZoneNotFound pins the ErrNotFound path Zone() takes on a missing row —
// the API's 404 for GET /api/v1/zones/{id} depends on this sentinel, not on
// a generic error or a zero-value Zone with no way to tell it apart from a
// real one.
func TestZoneNotFound(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		if _, err := s.Zones().Zone(ctx, 999999999); !errors.Is(err, ErrNotFound) {
			t.Fatalf("Zone(missing) err = %v, want ErrNotFound", err)
		}
	})
}

// TestUpdateZone covers the field-overwrite path the zones API's PATCH
// handler (Task 7) builds on: every editable column must round-trip, and a
// missing id must report ErrNotFound the same way every other execOne-backed
// mutation in this package does.
func TestUpdateZone(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		name := testGroupName("update.test")
		id, err := s.Zones().AddZone(ctx, Zone{
			Name: name, Type: "primary", Enabled: true,
			SOANS: "ns." + name, SOAMbox: "hostadmin." + name,
			SOASerial: 1, SOARefresh: 900, SOARetry: 300, SOAExpire: 604800, SOAMinimum: 900,
			// Deliberately different from SOAMinimum: the two are separate
			// columns precisely so RFC 2308 §5's min() can pick between
			// them, and a soa_ttl bound in the wrong position of the INSERT
			// or SELECT list would read back as 900 here.
			SOATTL: 300,
		})
		if err != nil {
			t.Fatalf("AddZone: %v", err)
		}
		z, err := s.Zones().Zone(ctx, id)
		if err != nil {
			t.Fatalf("Zone: %v", err)
		}
		if z.SOATTL != 300 || z.SOAMinimum != 900 {
			t.Fatalf("AddZone round-trip: SOATTL = %d, SOAMinimum = %d; want 300 and 900", z.SOATTL, z.SOAMinimum)
		}
		z.Enabled = false
		z.SOARefresh = 1800
		z.SOAMbox = "changed." + name
		z.SOATTL = 60
		z.Primaries = "10.0.0.1,10.0.0.2"
		if err := s.Zones().UpdateZone(ctx, z); err != nil {
			t.Fatalf("UpdateZone: %v", err)
		}
		got, err := s.Zones().Zone(ctx, id)
		if err != nil {
			t.Fatalf("Zone after update: %v", err)
		}
		if got.Enabled || got.SOARefresh != 1800 || got.SOAMbox != "changed."+name || got.Primaries != "10.0.0.1,10.0.0.2" {
			t.Errorf("UpdateZone did not persist: %+v", got)
		}
		if got.SOATTL != 60 || got.SOAMinimum != 900 {
			t.Errorf("UpdateZone: SOATTL = %d, SOAMinimum = %d; want 60 and 900", got.SOATTL, got.SOAMinimum)
		}

		if err := s.Zones().UpdateZone(ctx, Zone{ID: 999999999, Name: testGroupName("nobody")}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("UpdateZone(missing) err = %v, want ErrNotFound", err)
		}
	})
}

// TestDeleteRecord covers the standalone record delete the zone records API
// (Task 8) calls directly — distinct from DeleteZone's cascade, which
// TestZoneStoreRoundTrip already covers.
func TestDeleteRecord(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		zid, err := s.Zones().AddZone(ctx, Zone{Name: testGroupName("delrec.test"), Type: "primary", Enabled: true, SOASerial: 1})
		if err != nil {
			t.Fatalf("AddZone: %v", err)
		}
		rid, err := s.Zones().AddRecord(ctx, ZoneRecord{ZoneID: zid, Name: "www", Type: "A", TTL: 300, RData: "192.168.1.1", Enabled: true})
		if err != nil {
			t.Fatalf("AddRecord: %v", err)
		}
		if err := s.Zones().DeleteRecord(ctx, rid); err != nil {
			t.Fatalf("DeleteRecord: %v", err)
		}
		if recs, _ := s.Zones().Records(ctx, zid); len(recs) != 0 {
			t.Errorf("record survived delete: %v", recs)
		}
		if err := s.Zones().DeleteRecord(ctx, rid); !errors.Is(err, ErrNotFound) {
			t.Fatalf("double delete err = %v, want ErrNotFound", err)
		}
	})
}

// TestAllRecordsGroupsByZone pins the grouping Task 6's resolver depends on:
// it rebuilds its whole snapshot from one AllRecords call on every reload, so
// a record landing under the wrong zone key (or a broken zone_id filter)
// would corrupt every zone's answers, not just one. Assertions are scoped to
// the zones this test creates, since postgres here is one database shared
// across the whole package's test run.
func TestAllRecordsGroupsByZone(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		z1, err := s.Zones().AddZone(ctx, Zone{Name: testGroupName("all1.test"), Type: "primary", Enabled: true, SOASerial: 1})
		if err != nil {
			t.Fatalf("AddZone z1: %v", err)
		}
		z2, err := s.Zones().AddZone(ctx, Zone{Name: testGroupName("all2.test"), Type: "primary", Enabled: true, SOASerial: 1})
		if err != nil {
			t.Fatalf("AddZone z2: %v", err)
		}
		if _, err := s.Zones().AddRecord(ctx, ZoneRecord{ZoneID: z1, Name: "a", Type: "A", TTL: 300, RData: "1.1.1.1", Enabled: true}); err != nil {
			t.Fatalf("AddRecord z1/a: %v", err)
		}
		if _, err := s.Zones().AddRecord(ctx, ZoneRecord{ZoneID: z1, Name: "b", Type: "A", TTL: 300, RData: "1.1.1.2", Enabled: true}); err != nil {
			t.Fatalf("AddRecord z1/b: %v", err)
		}
		if _, err := s.Zones().AddRecord(ctx, ZoneRecord{ZoneID: z2, Name: "c", Type: "A", TTL: 300, RData: "2.2.2.2", Enabled: true}); err != nil {
			t.Fatalf("AddRecord z2/c: %v", err)
		}

		all, err := s.Zones().AllRecords(ctx)
		if err != nil {
			t.Fatalf("AllRecords: %v", err)
		}
		if got := len(all[z1]); got != 2 {
			t.Fatalf("all[z1] = %d records, want 2: %+v", got, all[z1])
		}
		if got := len(all[z2]); got != 1 {
			t.Fatalf("all[z2] = %d records, want 1: %+v", got, all[z2])
		}
		names := map[string]bool{}
		for _, r := range all[z1] {
			names[r.Name] = true
		}
		if !names["a"] || !names["b"] {
			t.Errorf("all[z1] names = %+v, want a and b", all[z1])
		}
		if all[z2][0].Name != "c" {
			t.Errorf("all[z2] = %+v, want the single record named c", all[z2])
		}
	})
}

// seedReplaceZone creates a zone holding the two records every
// ReplaceRecords test works from — one that will be updated, one that will
// be deleted — and returns their ids. Serial 7 is arbitrary but non-zero,
// so a serial that fails to move and a serial that was never set apart.
func seedReplaceZone(t *testing.T, s Store, name string) (zoneID, keepID, doomedID int64) {
	t.Helper()
	ctx := context.Background()
	zoneID, err := s.Zones().AddZone(ctx, Zone{
		Name: name, Type: "primary", Enabled: true,
		SOANS: "ns." + name, SOAMbox: "hostadmin." + name,
		SOASerial: 7, SOARefresh: 900, SOARetry: 300, SOAExpire: 604800,
		SOAMinimum: 900, SOATTL: 900,
	})
	if err != nil {
		t.Fatalf("AddZone: %v", err)
	}
	if keepID, err = s.Zones().AddRecord(ctx, ZoneRecord{
		ZoneID: zoneID, Name: "www", Type: "A", TTL: 300, RData: "1.1.1.1", Enabled: true,
	}); err != nil {
		t.Fatalf("AddRecord(www): %v", err)
	}
	if doomedID, err = s.Zones().AddRecord(ctx, ZoneRecord{
		ZoneID: zoneID, Name: "doomed", Type: "A", TTL: 300, RData: "9.9.9.9", Enabled: true,
	}); err != nil {
		t.Fatalf("AddRecord(doomed): %v", err)
	}
	return zoneID, keepID, doomedID
}

// zoneSnapshot reads a zone and its records the way every reader of this
// store sees them, so "unchanged" is asserted against what a query would
// actually return rather than against anything the writer kept.
func zoneSnapshot(t *testing.T, s Store, zoneID int64) (Zone, []ZoneRecord) {
	t.Helper()
	ctx := context.Background()
	z, err := s.Zones().Zone(ctx, zoneID)
	if err != nil {
		t.Fatalf("Zone: %v", err)
	}
	recs, err := s.Zones().Records(ctx, zoneID)
	if err != nil {
		t.Fatalf("Records: %v", err)
	}
	return z, recs
}

// TestReplaceRecordsAppliesTheWholeDiff is the success half: a replace has
// to actually delete, update, insert and re-stamp the zone row. Without it
// the rollback tests below would pass just as well against a ReplaceRecords
// that wrote nothing at all and returned an error.
func TestReplaceRecordsAppliesTheWholeDiff(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		zid, keepID, doomedID := seedReplaceZone(t, s, testGroupName("replace-ok.test"))
		before, _ := zoneSnapshot(t, s, zid)

		next := before
		next.SOASerial = 9
		next.SOAMbox = "changed." + before.Name
		next.ModifiedAt = 1785946876638

		if err := s.Zones().ReplaceRecords(ctx, next,
			[]int64{doomedID},
			[]ZoneRecord{{ID: keepID, ZoneID: zid, Name: "www", Type: "A", TTL: 600, RData: "1.1.1.1", Enabled: true}},
			[]ZoneRecord{{ZoneID: zid, Name: "new", Type: "A", TTL: 300, RData: "2.2.2.2", Enabled: true}},
		); err != nil {
			t.Fatalf("ReplaceRecords: %v", err)
		}

		gotZone, gotRecs := zoneSnapshot(t, s, zid)
		if !reflect.DeepEqual(gotZone, next) {
			t.Errorf("zone row after replace:\n got %+v\nwant %+v", gotZone, next)
		}
		byName := map[string]ZoneRecord{}
		for _, r := range gotRecs {
			byName[r.Name] = r
		}
		if len(gotRecs) != 2 {
			t.Fatalf("records after replace = %+v, want www (updated) and new (added)", gotRecs)
		}
		if got := byName["www"]; got.ID != keepID || got.TTL != 600 {
			t.Errorf("www = %+v, want row %d carrying the updated TTL 600", got, keepID)
		}
		if got := byName["new"]; got.RData != "2.2.2.2" || !got.Enabled {
			t.Errorf("new = %+v, want the added record", got)
		}
		if _, still := byName["doomed"]; still {
			t.Errorf("records after replace = %+v, want the deleted record gone", gotRecs)
		}
	})
}

// TestReplaceRecordsRollsBackEverythingOnFailure is the reason this method
// exists. An import is a destructive whole-zone replace, and when it ran as
// four separate writes a storage failure part-way through left the zone
// matching neither the file nor what was there before.
//
// Both failure points are exercised, because they leave different wreckage:
//
//   - The zone row's write is the last one, so a failure there is the case
//     that used to commit every record change and then fail to move the
//     serial that describes them. That is the one inconsistency nothing
//     downstream can detect — a secondary compares serials, sees the one it
//     already has, and never asks for the new contents.
//   - A record write failing mid-loop used to leave every earlier delete
//     and update committed, with no record of where it stopped.
//
// Each asserts the zone is *exactly* as it was — row and records, by value,
// ids included — rather than merely "still has some records".
func TestReplaceRecordsRollsBackEverythingOnFailure(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s Store) {
		ctx := context.Background()

		// A second zone, whose name is what makes the zone row's write fail:
		// zones.name is UNIQUE on both drivers (migration 0004), so renaming
		// onto it is a genuine driver-level constraint violation raised
		// part-way through the transaction — not an error the store code
		// invented for the test's benefit.
		occupied := testGroupName("replace-occupied.test")
		if _, err := s.Zones().AddZone(ctx, Zone{Name: occupied, Type: "primary", Enabled: true, SOASerial: 1}); err != nil {
			t.Fatalf("AddZone(occupied): %v", err)
		}

		t.Run("the zone row's write fails", func(t *testing.T) {
			zid, keepID, doomedID := seedReplaceZone(t, s, testGroupName("replace-zonefail.test"))
			wantZone, wantRecs := zoneSnapshot(t, s, zid)

			doomedZone := wantZone
			doomedZone.Name = occupied // UNIQUE violation on zones.name
			doomedZone.SOASerial = wantZone.SOASerial + 1

			err := s.Zones().ReplaceRecords(ctx, doomedZone,
				[]int64{doomedID},
				[]ZoneRecord{{ID: keepID, ZoneID: zid, Name: "www", Type: "A", TTL: 600, RData: "1.1.1.1", Enabled: true}},
				[]ZoneRecord{{ZoneID: zid, Name: "new", Type: "A", TTL: 300, RData: "2.2.2.2", Enabled: true}},
			)
			if err == nil {
				t.Fatal("ReplaceRecords succeeded with a colliding zone name")
			}
			// The sentinel has to survive the transaction path too, or the
			// API answers 503 "storage unavailable" to what is a name
			// collision (storeErrDup in internal/api/server.go).
			if !errors.Is(err, ErrDuplicate) {
				t.Errorf("err = %v, want ErrDuplicate", err)
			}
			assertZoneUnchanged(t, s, zid, wantZone, wantRecs)
		})

		t.Run("a record write fails", func(t *testing.T) {
			zid, _, doomedID := seedReplaceZone(t, s, testGroupName("replace-recfail.test"))
			wantZone, wantRecs := zoneSnapshot(t, s, zid)

			nextZone := wantZone
			nextZone.SOASerial = wantZone.SOASerial + 1

			// The delete lands, then the update names a row that isn't there
			// — the shape of a record deleted by another request between the
			// diff and the write. The delete must not survive it.
			err := s.Zones().ReplaceRecords(ctx, nextZone,
				[]int64{doomedID},
				[]ZoneRecord{{ID: 999999999, ZoneID: zid, Name: "www", Type: "A", TTL: 600, RData: "1.1.1.1", Enabled: true}},
				[]ZoneRecord{{ZoneID: zid, Name: "new", Type: "A", TTL: 300, RData: "2.2.2.2", Enabled: true}},
			)
			if err == nil {
				t.Fatal("ReplaceRecords succeeded with an update to a row that does not exist")
			}
			if !errors.Is(err, ErrNotFound) {
				t.Errorf("err = %v, want ErrNotFound", err)
			}
			assertZoneUnchanged(t, s, zid, wantZone, wantRecs)
		})
	})
}

// assertZoneUnchanged fails unless the zone row and every record are
// byte-for-byte what they were, ids included. "Exactly as it was" is the
// whole claim a rollback makes: a zone left with the right *number* of
// records but a moved serial, or the right serial and a churned row id, is
// still a zone that matches neither the file nor what it was.
func assertZoneUnchanged(t *testing.T, s Store, zoneID int64, wantZone Zone, wantRecs []ZoneRecord) {
	t.Helper()
	gotZone, gotRecs := zoneSnapshot(t, s, zoneID)
	if !reflect.DeepEqual(gotZone, wantZone) {
		t.Errorf("zone row changed despite the failure:\n got %+v\nwant %+v", gotZone, wantZone)
	}
	if gotZone.SOASerial != wantZone.SOASerial {
		t.Errorf("SOA serial moved to %d despite the failure, want %d — records and serial must move together or not at all", gotZone.SOASerial, wantZone.SOASerial)
	}
	if !reflect.DeepEqual(gotRecs, wantRecs) {
		t.Errorf("records changed despite the failure:\n got %+v\nwant %+v", gotRecs, wantRecs)
	}
}

// NoteTransferAttempt is the only write allowed to touch last_error and
// last_attempt, and it must touch nothing else. That narrowness is what stops
// a transfer outcome — written every retry interval for as long as a primary
// is down — from carrying a stale copy of any other column back over an
// operator's concurrent edit. Asserted as a whole-struct comparison rather
// than field by field, so a column added later is covered without anyone
// remembering to add it here.
func TestNoteTransferAttemptWritesOnlyItsOwnColumns(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		id, err := s.Zones().AddZone(ctx, Zone{
			Name: testGroupName("e412.in"), Type: "secondary", Enabled: true,
			SOANS: "ns1.e412.in", SOAMbox: "hostadmin.e412.in",
			SOASerial: 7, SOARefresh: 900, SOARetry: 300,
			SOAExpire: 604800, SOAMinimum: 900, SOATTL: 900,
			Primaries: "203.0.113.9", TSIGKeyID: 0,
			ExpiresAt: 1754604800000, RefreshedAt: 1754000000000,
			CreatedAt: 1753000000000, ModifiedAt: 1753500000000,
		})
		if err != nil {
			t.Fatalf("AddZone: %v", err)
		}
		before, err := s.Zones().Zone(ctx, id)
		if err != nil {
			t.Fatalf("Zone: %v", err)
		}

		const msg = "203.0.113.9:53: dial tcp: connect: connection refused"
		if err := s.Zones().NoteTransferAttempt(ctx, id, 1754111111000, msg); err != nil {
			t.Fatalf("NoteTransferAttempt: %v", err)
		}
		after, err := s.Zones().Zone(ctx, id)
		if err != nil {
			t.Fatalf("Zone: %v", err)
		}

		if after.LastError != msg {
			t.Errorf("last_error = %q, want %q", after.LastError, msg)
		}
		if after.LastAttempt != 1754111111000 {
			t.Errorf("last_attempt = %d, want 1754111111000", after.LastAttempt)
		}
		// Everything else, unchanged. refreshed_at especially: a failed
		// attempt must not read as a success, and modified_at, which is the
		// column an operator reads to find out what happened to a zone.
		want := before
		want.LastError, want.LastAttempt = msg, 1754111111000
		if !reflect.DeepEqual(after, want) {
			t.Errorf("NoteTransferAttempt changed more than its own columns:\n got %+v\nwant %+v", after, want)
		}

		// Cleared on success, so a zone that recovered stops reporting a
		// problem that is over.
		if err := s.Zones().NoteTransferAttempt(ctx, id, 1754222222000, ""); err != nil {
			t.Fatalf("NoteTransferAttempt(success): %v", err)
		}
		recovered, err := s.Zones().Zone(ctx, id)
		if err != nil {
			t.Fatalf("Zone: %v", err)
		}
		if recovered.LastError != "" {
			t.Errorf("last_error = %q after a successful attempt, want it cleared", recovered.LastError)
		}
	})
}

// The other direction of the same rule, and the one a doc comment alone was
// guarding: nothing but NoteTransferAttempt may write these two columns.
//
// The danger is not hypothetical. Every ordinary zone write binds the row
// entire from a struct the caller read some time earlier, so if last_error
// were in that statement, an API PATCH that read the zone before a transfer
// failed and committed after it would silently erase the failure — and the
// zone would go back to reading as healthy while its primary stayed down. A
// zone-file import (ReplaceRecords, which shares updateZoneArgs) would do the
// same.
//
// Both paths are exercised with a deliberately *stale* struct: exactly what a
// handler that read the row before the failure would hold.
func TestOrdinaryZoneWritesCannotEraseARecordedFailure(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		id, err := s.Zones().AddZone(ctx, Zone{
			Name: testGroupName("e412.in"), Type: "secondary", Enabled: true,
			SOANS: "ns1.e412.in", SOAMbox: "hostadmin.e412.in",
			SOASerial: 7, SOARefresh: 900, SOARetry: 300,
			SOAExpire: 604800, SOAMinimum: 900, SOATTL: 900,
			Primaries: "203.0.113.9",
		})
		if err != nil {
			t.Fatalf("AddZone: %v", err)
		}
		// The copy a handler read before anything failed: no error, no attempt.
		stale, err := s.Zones().Zone(ctx, id)
		if err != nil {
			t.Fatalf("Zone: %v", err)
		}
		if stale.LastError != "" || stale.LastAttempt != 0 {
			t.Fatalf("setup: fresh zone already carries an attempt: %+v", stale)
		}

		const msg = "203.0.113.9:53: dial tcp: connect: connection refused"
		if err := s.Zones().NoteTransferAttempt(ctx, id, 1754111111000, msg); err != nil {
			t.Fatalf("NoteTransferAttempt: %v", err)
		}

		// A PATCH lands, built from the copy read before the failure.
		patched := stale
		patched.Enabled = false
		if err := s.Zones().UpdateZone(ctx, patched); err != nil {
			t.Fatalf("UpdateZone: %v", err)
		}
		afterPatch, err := s.Zones().Zone(ctx, id)
		if err != nil {
			t.Fatalf("Zone: %v", err)
		}
		if afterPatch.LastError != msg || afterPatch.LastAttempt != 1754111111000 {
			t.Errorf("UpdateZone erased the recorded failure: last_error = %q last_attempt = %d, want %q / 1754111111000.\n"+
				"These columns must not be in updateZoneSQL — a handler that read the row before a failure "+
				"would carry an empty error over it and the zone would read as healthy while its primary stayed down.",
				afterPatch.LastError, afterPatch.LastAttempt, msg)
		}
		// The PATCH itself still applied; the failure surviving must not be
		// the write having been lost.
		if afterPatch.Enabled {
			t.Errorf("the patch did not apply at all")
		}

		// And the same through ReplaceRecords, which shares updateZoneArgs —
		// a zone-file import, again built from the stale copy.
		replaced := stale
		replaced.SOASerial = 9
		if err := s.Zones().ReplaceRecords(ctx, replaced, nil, nil, []ZoneRecord{
			{ZoneID: id, Name: "@", Type: "NS", TTL: 3600, RData: "ns1.e412.in.", Enabled: true},
		}); err != nil {
			t.Fatalf("ReplaceRecords: %v", err)
		}
		afterImport, err := s.Zones().Zone(ctx, id)
		if err != nil {
			t.Fatalf("Zone: %v", err)
		}
		if afterImport.LastError != msg || afterImport.LastAttempt != 1754111111000 {
			t.Errorf("ReplaceRecords erased the recorded failure: last_error = %q last_attempt = %d, want %q / 1754111111000",
				afterImport.LastError, afterImport.LastAttempt, msg)
		}
		if afterImport.SOASerial != 9 {
			t.Errorf("the replace did not apply at all")
		}
	})
}

func TestZoneAllowTransferRoundTrips(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		zs := s.Zones()
		id, err := zs.AddZone(ctx, Zone{
			Name: testGroupName("e412.in"), Type: "primary", Enabled: true,
			AllowTransfer: "10.0.0.0/24, key:ns2.",
		})
		if err != nil {
			t.Fatalf("AddZone: %v", err)
		}
		got, err := zs.Zone(ctx, id)
		if err != nil {
			t.Fatalf("Zone: %v", err)
		}
		if got.AllowTransfer != "10.0.0.0/24, key:ns2." {
			t.Fatalf("allow_transfer = %q after insert", got.AllowTransfer)
		}
		got.AllowTransfer = "192.168.1.5"
		if err := zs.UpdateZone(ctx, got); err != nil {
			t.Fatalf("UpdateZone: %v", err)
		}
		got, err = zs.Zone(ctx, id)
		if err != nil {
			t.Fatalf("Zone: %v", err)
		}
		if got.AllowTransfer != "192.168.1.5" {
			t.Fatalf("allow_transfer = %q after update", got.AllowTransfer)
		}
	})
}

func TestNoteTransferRequestWritesOnlyItsOwnColumns(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		zs := s.Zones()
		name := testGroupName("e412.in")
		id, err := zs.AddZone(ctx, Zone{Name: name, Type: "primary", Enabled: true})
		if err != nil {
			t.Fatalf("AddZone: %v", err)
		}
		if err := zs.NoteTransferRequest(ctx, id, 1700000000000, "10.0.0.5", ""); err != nil {
			t.Fatalf("NoteTransferRequest: %v", err)
		}
		got, err := zs.Zone(ctx, id)
		if err != nil {
			t.Fatalf("Zone: %v", err)
		}
		if got.LastXfrAt != 1700000000000 || got.LastXfrPeer != "10.0.0.5" || got.LastXfrError != "" {
			t.Fatalf("served state = (%d, %q, %q)", got.LastXfrAt, got.LastXfrPeer, got.LastXfrError)
		}
		if got.Name != name || !got.Enabled {
			t.Fatalf("NoteTransferRequest changed the zone's configuration: %+v", got)
		}
	})
}

// The mirror of TestNoteTransferAttemptSurvivesAZoneWrite on the outbound
// side: a whole-row UpdateZone binds every configuration column from a struct
// the caller read earlier, so if last_xfr_* were in that statement, an
// operator's edit would erase what a transfer recorded a moment before.
func TestOrdinaryZoneWritesCannotEraseServedState(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		zs := s.Zones()
		id, err := zs.AddZone(ctx, Zone{Name: testGroupName("e412.in"), Type: "primary", Enabled: true})
		if err != nil {
			t.Fatalf("AddZone: %v", err)
		}
		stale, err := zs.Zone(ctx, id) // read before the transfer
		if err != nil {
			t.Fatalf("Zone: %v", err)
		}
		if err := zs.NoteTransferRequest(ctx, id, 1700000000000, "10.0.0.5", "refused"); err != nil {
			t.Fatalf("NoteTransferRequest: %v", err)
		}
		stale.SOARefresh = 7200 // an edit made from the stale read
		if err := zs.UpdateZone(ctx, stale); err != nil {
			t.Fatalf("UpdateZone: %v", err)
		}
		got, err := zs.Zone(ctx, id)
		if err != nil {
			t.Fatalf("Zone: %v", err)
		}
		if got.SOARefresh != 7200 {
			t.Fatalf("the edit did not land: soa_refresh = %d", got.SOARefresh)
		}
		if got.LastXfrAt != 1700000000000 || got.LastXfrError != "refused" {
			t.Fatalf("the zone write erased the served state: (%d, %q)", got.LastXfrAt, got.LastXfrError)
		}
	})
}

// updateZoneSQL binds every configuration column, and notify_to is one, so a
// PATCH that changes it has to persist. This is the positive half of the
// disjoint-column-sets rule; the negative half is that it never binds
// zone_notifies state, which it cannot, since that is a different table.
func TestUpdateZonePersistsNotifyTo(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		id := seedNotifyZone(t, s, "example.com")

		z, err := s.Zones().Zone(ctx, id)
		if err != nil {
			t.Fatalf("Zone: %v", err)
		}
		if z.NotifyTo != "" {
			t.Fatalf("a new zone has notify_to = %q, want empty", z.NotifyTo)
		}
		z.NotifyTo = "10.0.0.2:53 key:ns2-xfer., 10.0.0.3:53"
		if err := s.Zones().UpdateZone(ctx, z); err != nil {
			t.Fatalf("UpdateZone: %v", err)
		}
		got, err := s.Zones().Zone(ctx, id)
		if err != nil {
			t.Fatalf("Zone after update: %v", err)
		}
		if got.NotifyTo != z.NotifyTo {
			t.Errorf("notify_to = %q, want %q", got.NotifyTo, z.NotifyTo)
		}
	})
}

// updateZoneSQL binds every configuration column, and forward_to is one, so a
// PATCH that changes it has to persist. The negative half of the
// disjoint-column-sets rule is automatic here — there is no second writer of
// this column — so this is the positive half only.
func TestUpdateZonePersistsForwardTo(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		name := testGroupName("fwd") + ".example"
		id, err := s.Zones().AddZone(ctx, Zone{
			Name: name, Type: "forwarder", Enabled: true,
			SOANS: "ns1." + name, SOAMbox: "hostmaster." + name,
			SOASerial: 1, SOARefresh: 3600, SOARetry: 600,
			SOAExpire: 604800, SOAMinimum: 300, SOATTL: 900,
		})
		if err != nil {
			t.Fatalf("AddZone: %v", err)
		}

		z, err := s.Zones().Zone(ctx, id)
		if err != nil {
			t.Fatalf("Zone: %v", err)
		}
		if z.ForwardTo != "" {
			t.Fatalf("a new zone has forward_to = %q, want empty", z.ForwardTo)
		}

		z.ForwardTo = "10.0.0.1:53, 10.0.0.2:5353"
		if err := s.Zones().UpdateZone(ctx, z); err != nil {
			t.Fatalf("UpdateZone: %v", err)
		}
		got, err := s.Zones().Zone(ctx, id)
		if err != nil {
			t.Fatalf("Zone after update: %v", err)
		}
		if got.ForwardTo != z.ForwardTo {
			t.Errorf("forward_to = %q, want %q", got.ForwardTo, z.ForwardTo)
		}
		// Every other column survived the round trip. A column-order
		// mismatch between zoneColumns and scanZone shows up here as a
		// neighbouring field holding this one's value.
		if got.Name != name || got.Type != "forwarder" || got.SOASerial != 1 {
			t.Errorf("neighbouring columns moved: %+v", got)
		}
	})
}

// TestBumpSerialWrapsAtMaxUint32 is the input that discriminates: a zone at
// 4294967295. `soa_serial + 1` writes 4294967296, which is a perfectly happy
// value in an sqlite INTEGER and a postgres BIGINT and no longer a serial —
// every subsequent read of that zone fails scanning it into uint32, and the
// zone is out of service until somebody edits the row by hand.
//
// A serial is uint32 and wraps, which the rest of the product already knows:
// zones.SerialNewer compares across the wrap (RFC 1982) and the zone-file
// import path increments in Go arithmetic that wraps for free. This is the
// commonest serial-advancing path of the three, and it is the one that could
// not reach 0 at all.
//
// Bumping 7 to 8 (above) cannot fail this way, which is why that test alone
// left the defect in place from Milestone A to D4.
func TestBumpSerialWrapsAtMaxUint32(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		id, err := s.Zones().AddZone(ctx, Zone{
			Name: testGroupName("wrap.test"), Type: "primary", Enabled: true,
			SOASerial: 4294967295,
		})
		if err != nil {
			t.Fatalf("AddZone: %v", err)
		}
		if err := s.Zones().BumpSerial(ctx, id); err != nil {
			t.Fatalf("BumpSerial: %v", err)
		}
		// Reading it back is half the assertion: the failure mode is not a
		// wrong serial but an unreadable row.
		z, err := s.Zones().Zone(ctx, id)
		if err != nil {
			t.Fatalf("Zone after bumping 4294967295: %v", err)
		}
		if z.SOASerial != 0 {
			t.Errorf("SOASerial = %d, want 0 (wrapped)", z.SOASerial)
		}
		// And the zone must keep working afterwards: 0 -> 1, not stuck.
		if err := s.Zones().BumpSerial(ctx, id); err != nil {
			t.Fatalf("BumpSerial after wrap: %v", err)
		}
		z, err = s.Zones().Zone(ctx, id)
		if err != nil {
			t.Fatalf("Zone after the wrap: %v", err)
		}
		if z.SOASerial != 1 {
			t.Errorf("SOASerial = %d, want 1", z.SOASerial)
		}
	})
}
