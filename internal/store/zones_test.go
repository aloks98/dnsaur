package store

import (
	"context"
	"errors"
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
