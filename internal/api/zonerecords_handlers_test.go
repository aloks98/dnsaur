package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/aloks98/dnsaur/internal/store"
)

// newTestServerWithZone builds a zoneTestServer (server_test.go/
// zones_handlers_test.go's harness) and seeds one zone directly through the
// store, returning its id.
//
// The seed goes through store.Zones().AddZone rather than POST
// /api/v1/zones on purpose: handleZoneCreate (Task 7) auto-seeds an apex NS
// record, and TestRecordCreateRejectsCNAMEAtApex exists specifically to
// prove the apex-CNAME check catches a case the CNAME-sibling check
// can't — an apex with no sibling records at all, because the zone's SOA
// lives on the zones row rather than in zone_records. Going through the
// create handler here would give every seeded zone an apex sibling and
// make that test (and its revert-verify row) pass for the wrong reason.
func newTestServerWithZone(t *testing.T, name string) (*zoneTestServer, int64) {
	t.Helper()
	srv := newTestServer(t)
	now := time.Now().UnixMilli()
	zid, err := srv.store.Zones().AddZone(t.Context(), store.Zone{
		Name: name, Type: "primary", Enabled: true,
		SOANS: "ns." + name, SOAMbox: "hostadmin." + name,
		SOASerial: 1, SOARefresh: 900, SOARetry: 300, SOAExpire: 604800,
		SOAMinimum: 900, SOATTL: 900,
		CreatedAt: now, ModifiedAt: now,
	})
	if err != nil {
		t.Fatalf("seed zone %q: %v", name, err)
	}
	return srv, zid
}

// Validation is dns.NewRR itself, so an accepted record is by construction
// a servable one — there is no second validator to drift from the parser.
func TestRecordCreateRejectsUnparseableRData(t *testing.T) {
	srv, zid := newTestServerWithZone(t, "e412.in")
	body := `{"name":"bifrost","type":"A","ttl":3600,"rdata":"not-an-ip"}`
	rec := srv.do(t, "POST", fmt.Sprintf("/api/v1/zones/%d/records", zid), body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body = %s, want 400", rec.Code, rec.Body)
	}
}

func TestRecordCreateAcceptsMX(t *testing.T) {
	srv, zid := newTestServerWithZone(t, "e412.in")
	body := `{"name":"@","type":"MX","ttl":3600,"rdata":"10 mail.e412.in."}`
	if rec := srv.do(t, "POST", fmt.Sprintf("/api/v1/zones/%d/records", zid), body); rec.Code != http.StatusCreated {
		t.Fatalf("status = %d body = %s, want 201", rec.Code, rec.Body)
	}
}

// RFC 1034 3.6.2. Allowed at write time this produces a zone that answers
// inconsistently depending on which record is found first.
func TestRecordCreateRejectsCNAMEBesideSibling(t *testing.T) {
	srv, zid := newTestServerWithZone(t, "e412.in")
	path := fmt.Sprintf("/api/v1/zones/%d/records", zid)
	srv.do(t, "POST", path, `{"name":"www","type":"A","ttl":300,"rdata":"192.168.1.9"}`)
	rec := srv.do(t, "POST", path, `{"name":"www","type":"CNAME","ttl":300,"rdata":"bifrost.e412.in."}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
	// And the reverse order must be refused too.
	srv2, zid2 := newTestServerWithZone(t, "e412.in")
	path2 := fmt.Sprintf("/api/v1/zones/%d/records", zid2)
	srv2.do(t, "POST", path2, `{"name":"www","type":"CNAME","ttl":300,"rdata":"bifrost.e412.in."}`)
	if rec := srv2.do(t, "POST", path2, `{"name":"www","type":"A","ttl":300,"rdata":"192.168.1.9"}`); rec.Code != http.StatusConflict {
		t.Errorf("reverse order status = %d, want 409", rec.Code)
	}
}

func TestRecordCreateBumpsSerial(t *testing.T) {
	srv, zid := newTestServerWithZone(t, "e412.in")
	before := srv.zone(t, zid).SOASerial
	srv.do(t, "POST", fmt.Sprintf("/api/v1/zones/%d/records", zid), `{"name":"a","type":"A","ttl":300,"rdata":"1.2.3.4"}`)
	if after := srv.zone(t, zid).SOASerial; after != before+1 {
		t.Errorf("serial %d -> %d, want +1", before, after)
	}
}

// The brief's Step 1 pinned this as an empty "/* 404 */" placeholder,
// which was never filled in — it compiled and reported PASS while pinning
// nothing. Filled in per fix-round-1 finding 1: a zone id that doesn't
// exist must 404, not fall through to some other status.
func TestRecordCreateRejectsUnknownZone(t *testing.T) {
	srv := newTestServer(t)
	body := `{"name":"a","type":"A","ttl":300,"rdata":"1.2.3.4"}`
	if rec := srv.do(t, "POST", "/api/v1/zones/999999/records", body); rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// RFC 2181 5.2. Two A records at one name with different TTLs make a zone that
// answers differently depending on which row is read first.
func TestRecordCreateRejectsMismatchedRRSetTTL(t *testing.T) {
	srv, zid := newTestServerWithZone(t, "e412.in")
	path := fmt.Sprintf("/api/v1/zones/%d/records", zid)
	srv.do(t, "POST", path, `{"name":"web","type":"A","ttl":300,"rdata":"192.168.1.1"}`)
	rec := srv.do(t, "POST", path, `{"name":"web","type":"A","ttl":600,"rdata":"192.168.1.2"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 — an RRSet must have one TTL", rec.Code)
	}
	// Same TTL is fine: multiple addresses at one name is normal.
	if rec := srv.do(t, "POST", path, `{"name":"web","type":"A","ttl":300,"rdata":"192.168.1.2"}`); rec.Code != http.StatusCreated {
		t.Errorf("second A with matching TTL: status = %d, want 201", rec.Code)
	}
}

// RFC 2181 8. A TTL with the high bit set is read as ZERO by resolvers, so an
// unclamped value means "never cache" — the opposite of what was typed.
func TestRecordCreateRejectsTTLAboveInt32Max(t *testing.T) {
	srv, zid := newTestServerWithZone(t, "e412.in")
	body := `{"name":"a","type":"A","ttl":2147483648,"rdata":"1.2.3.4"}`
	if rec := srv.do(t, "POST", fmt.Sprintf("/api/v1/zones/%d/records", zid), body); rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for TTL > 2147483647", rec.Code)
	}
}

// RFC 1912 2.4. The sibling check cannot catch this on its own: our SOA lives
// on the zones row, so the apex has no sibling record to conflict with.
func TestRecordCreateRejectsCNAMEAtApex(t *testing.T) {
	srv, zid := newTestServerWithZone(t, "e412.in")
	body := `{"name":"@","type":"CNAME","ttl":300,"rdata":"elsewhere.test."}`
	if rec := srv.do(t, "POST", fmt.Sprintf("/api/v1/zones/%d/records", zid), body); rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 — no CNAME at a zone apex", rec.Code)
	}
}

// A name typed out fully qualified — carrying the zone's own apex as a
// trailing suffix, as a user habituated to FQDNs would type it — must land
// on the same relative name as writing it short. Left un-stripped,
// absoluteRecordName would silently double it into
// "www.e412.in.e412.in" (fix-round-1 finding 3): ToRR would still parse
// that, so nothing else catches it.
func TestRecordCreateStripsRedundantApexSuffix(t *testing.T) {
	for _, name := range []string{"www.e412.in", "www.e412.in."} {
		t.Run(name, func(t *testing.T) {
			srv, zid := newTestServerWithZone(t, "e412.in")
			body := fmt.Sprintf(`{"name":%q,"type":"A","ttl":300,"rdata":"192.168.1.9"}`, name)
			if rec := srv.do(t, "POST", fmt.Sprintf("/api/v1/zones/%d/records", zid), body); rec.Code != http.StatusCreated {
				t.Fatalf("status = %d body = %s, want 201", rec.Code, rec.Body)
			}
			recs := srv.records(t, zid)
			if len(recs) != 1 || recs[0].Name != "www" {
				t.Fatalf("records = %+v, want one record named \"www\"", recs)
			}
		})
	}
}

// addZone is a second-zone helper for tests that need two zones in the same
// store, e.g. the cross-zone guard below. newTestServerWithZone can't be
// reused for the second zone since it also builds a fresh server/store.
func addZone(t *testing.T, ts *zoneTestServer, name string) int64 {
	t.Helper()
	now := time.Now().UnixMilli()
	zid, err := ts.store.Zones().AddZone(t.Context(), store.Zone{
		Name: name, Type: "primary", Enabled: true,
		SOANS: "ns." + name, SOAMbox: "hostadmin." + name,
		SOASerial: 1, SOARefresh: 900, SOARetry: 300, SOAExpire: 604800,
		SOAMinimum: 900, SOATTL: 900,
		CreatedAt: now, ModifiedAt: now,
	})
	if err != nil {
		t.Fatalf("seed zone %q: %v", name, err)
	}
	return zid
}

// findRecord (zonerecords_handlers.go) is the only thing stopping a caller
// from mutating or deleting a record that belongs to a different zone,
// since UpdateRecord/DeleteRecord key on record id alone in SQL. Without
// this test a refactor could quietly drop that guard and let zone A's
// owner delete zone B's records by id (fix-round-1 finding 2).
func TestRecordUpdateAndDeleteRejectCrossZoneID(t *testing.T) {
	srv, zidA := newTestServerWithZone(t, "a.e412.in")
	zidB := addZone(t, srv, "b.e412.in")

	rec := srv.do(t, "POST", fmt.Sprintf("/api/v1/zones/%d/records", zidA), `{"name":"www","type":"A","ttl":300,"rdata":"192.168.1.9"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("seed record: status = %d body = %s", rec.Code, rec.Body)
	}
	var created struct{ ID int64 }
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// zone A's record, addressed through zone B's URL.
	crossPath := fmt.Sprintf("/api/v1/zones/%d/records/%d", zidB, created.ID)
	if r := srv.do(t, "PUT", crossPath, `{"name":"www","type":"A","ttl":300,"rdata":"192.168.1.10"}`); r.Code != http.StatusNotFound {
		t.Errorf("cross-zone PUT status = %d, want 404", r.Code)
	}
	if r := srv.do(t, "DELETE", crossPath, ""); r.Code != http.StatusNotFound {
		t.Errorf("cross-zone DELETE status = %d, want 404", r.Code)
	}

	// The record must be untouched by either attempt.
	recs := srv.records(t, zidA)
	if len(recs) != 1 || recs[0].RData != "192.168.1.9" {
		t.Fatalf("record mutated via the wrong zone's URL: %+v", recs)
	}
}

func TestRecordsListReturnsZoneRecords(t *testing.T) {
	srv, zid := newTestServerWithZone(t, "e412.in")
	path := fmt.Sprintf("/api/v1/zones/%d/records", zid)
	srv.do(t, "POST", path, `{"name":"a","type":"A","ttl":300,"rdata":"1.2.3.4"}`)
	srv.do(t, "POST", path, `{"name":"b","type":"A","ttl":300,"rdata":"1.2.3.5"}`)

	rec := srv.do(t, "GET", path, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s, want 200", rec.Code, rec.Body)
	}
	var recs []store.ZoneRecord
	if err := json.Unmarshal(rec.Body.Bytes(), &recs); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("records = %+v, want 2", recs)
	}
}

func TestRecordUpdateReplacesFields(t *testing.T) {
	srv, zid := newTestServerWithZone(t, "e412.in")
	rec := srv.do(t, "POST", fmt.Sprintf("/api/v1/zones/%d/records", zid), `{"name":"www","type":"A","ttl":300,"rdata":"192.168.1.9"}`)
	var created struct{ ID int64 }
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	path := fmt.Sprintf("/api/v1/zones/%d/records/%d", zid, created.ID)
	if r := srv.do(t, "PUT", path, `{"name":"www","type":"A","ttl":600,"rdata":"192.168.1.20"}`); r.Code != http.StatusNoContent {
		t.Fatalf("put status = %d body = %s, want 204", r.Code, r.Body)
	}
	recs := srv.records(t, zid)
	if len(recs) != 1 || recs[0].TTL != 600 || recs[0].RData != "192.168.1.20" {
		t.Fatalf("record not updated: %+v", recs)
	}
}

func TestRecordDeleteRemovesRecord(t *testing.T) {
	srv, zid := newTestServerWithZone(t, "e412.in")
	rec := srv.do(t, "POST", fmt.Sprintf("/api/v1/zones/%d/records", zid), `{"name":"www","type":"A","ttl":300,"rdata":"192.168.1.9"}`)
	var created struct{ ID int64 }
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	path := fmt.Sprintf("/api/v1/zones/%d/records/%d", zid, created.ID)
	if r := srv.do(t, "DELETE", path, ""); r.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204", r.Code)
	}
	if recs := srv.records(t, zid); len(recs) != 0 {
		t.Fatalf("record survived delete: %+v", recs)
	}
}
