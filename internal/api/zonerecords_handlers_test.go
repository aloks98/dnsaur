package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aloks98/dnsaur/internal/store"
	"github.com/aloks98/dnsaur/internal/zones"
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

// recordBody builds a POST/PUT body without hand-escaping rdata — several
// of the cases below carry quotes and newlines, which is the whole point of
// them.
func recordBody(t *testing.T, name, recType string, ttl uint32, rdata string) string {
	t.Helper()
	b, err := json.Marshal(zoneRecordWrite{Name: name, Type: recType, TTL: ttl, RData: rdata})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// reimport runs the Render -> Parse round trip an operator performs by
// downloading a zone and loading it into another server: the real export
// endpoint, then zones.Parse. It returns the reimported rdata keyed by
// "<name> <TYPE>".
func (ts *zoneTestServer) reimport(t *testing.T, zid int64, zoneName string) map[string]string {
	t.Helper()
	export := ts.do(t, "GET", fmt.Sprintf("/api/v1/zones/%d/file", zid), "")
	if export.Code != http.StatusOK {
		t.Fatalf("export status = %d body = %s", export.Code, export.Body)
	}
	pz, errs := zones.Parse(export.Body.String(), zoneName)
	if len(errs) > 0 {
		t.Fatalf("reimporting the zone's own export failed: %v\n%s", errs, export.Body)
	}
	out := map[string]string{}
	for _, r := range pz.Records {
		out[r.Name+" "+r.Type] = r.RData
	}
	return out
}

// A record's rdata is stored once and then read in two places that do not
// agree about what the same text means. zones.ToRR reads it under no origin,
// where "nas.e412.in" is already absolute; zones.Render writes it into a
// master file under "$ORIGIN e412.in.", where that identical text is
// *relative* and resolves to nas.e412.in.e412.in. Storing the request body
// verbatim let those two readings disagree, so a record was served at one
// target and exported pointing at another — silently, and only visible after
// a round trip through a file.
//
// It is the common spelling, not an exotic one: Cloudflare and Route 53 both
// accept a dotless absolute target, so users are trained to omit the trailing
// dot, and dnsaur's form accepts it too.
//
// The fix stores the rdata zones.ToRR itself printed, so every case asserts
// the three things that buys:
//
//   - the stored spelling is the one the server uses for the RR it parsed;
//   - the RR served from the stored value is byte-identical to the RR served
//     from the raw text, so normalising never changes an answer — it only
//     changes how the answer is written down;
//   - Render -> Parse hands the stored value back unchanged, so the text has
//     exactly one meaning in every context it is read in.
func TestRecordWriteStoresRDataWithOneMeaningEverywhere(t *testing.T) {
	cases := []struct {
		what, name, recType, raw, want string
	}{
		// The reported bug, in the shape it was reported: a dotless absolute
		// CNAME target. Served as nas.e412.in.; exported, it used to reimport
		// as nas.e412.in.e412.in.
		{"dotless absolute CNAME target", "git", "CNAME", "nas.e412.in", "nas.e412.in."},
		// Already unambiguous, and must survive untouched.
		{"fully qualified CNAME target", "vcs", "CNAME", "nas.e412.in.", "nas.e412.in."},
		// A bare label. dnsaur reads rdata under no origin (zones.ToRR), so
		// what this record *serves* is the root-level name nas. — not
		// nas.e412.in., which is what the same token would mean inside a
		// master file. Storing "nas." is what makes the stored text say the
		// thing that is actually served, and it makes the mismatch visible in
		// the UI instead of leaving it to be discovered after an export. What
		// it is emphatically not is a change to the answer: the RR assertion
		// below pins that this record resolves exactly where it did before.
		{"bare relative CNAME target", "www", "CNAME", "nas", "nas."},
		{"dotless MX exchange", "@", "MX", "10 mail.e412.in", "10 mail.e412.in."},
		{"dotless SRV target", "_sip._tcp", "SRV", "10 20 5060 sip.e412.in", "10 20 5060 sip.e412.in."},
		// Types with no name in their rdata carry no origin ambiguity at all,
		// and are normalised anyway — see the test below for the reason the
		// scope is every type rather than the name-valued ones.
		{"A address", "host", "A", "192.168.1.9", "192.168.1.9"},
		{"long-form AAAA address", "host6", "AAAA", "2001:0db8:0000:0000:0000:0000:0000:0001", "2001:db8::1"},
		{"unquoted TXT", "note", "TXT", "hello", `"hello"`},
		{"quoted TXT", "spf", "TXT", `"v=spf1 -all"`, `"v=spf1 -all"`},
	}

	for _, c := range cases {
		t.Run(c.what, func(t *testing.T) {
			srv, zid := newTestServerWithZone(t, "e412.in")
			body := recordBody(t, c.name, c.recType, 300, c.raw)
			if rec := srv.do(t, "POST", fmt.Sprintf("/api/v1/zones/%d/records", zid), body); rec.Code != http.StatusCreated {
				t.Fatalf("status = %d body = %s, want 201", rec.Code, rec.Body)
			}
			recs := srv.recordsByType(t, zid, c.recType)
			if len(recs) != 1 {
				t.Fatalf("records = %+v, want one %s", recs, c.recType)
			}
			stored := recs[0]
			if stored.RData != c.want {
				t.Errorf("stored rdata = %q, want %q", stored.RData, c.want)
			}

			// Normalising must never change what the resolver answers. Both
			// sides go through zones.ToRR, the same call answer.go builds its
			// RR with, so this compares the served records themselves rather
			// than their spellings.
			raw := stored
			raw.RData = c.raw
			fqdn := zones.RecordFQDN("e412.in", stored.Name)
			fromStored, err := zones.ToRR(fqdn, stored)
			if err != nil {
				t.Fatalf("stored rdata %q no longer parses: %v", stored.RData, err)
			}
			fromRaw, err := zones.ToRR(fqdn, raw)
			if err != nil {
				t.Fatalf("raw rdata %q does not parse: %v", c.raw, err)
			}
			if fromStored.String() != fromRaw.String() {
				t.Errorf("normalising changed the served record:\n raw -> %s\nstored -> %s", fromRaw, fromStored)
			}

			// And the round trip the whole thing is about.
			key := c.name + " " + c.recType
			if got := srv.reimport(t, zid, "e412.in")[key]; got != c.want {
				t.Errorf("Render -> Parse gave %q for %s, want %q — the exported record is not the stored one", got, key, c.want)
			}
		})
	}
}

// The same defect with a different trigger, and the reason the fix normalises
// every type rather than only the ones whose rdata embeds a domain name.
//
// dns.NewRR reads one RR and silently ignores whatever follows it, so
// "1.2.3.4\nevil.e412.in. 300 IN A 6.6.6.6" validates, and is served as
// nothing but 1.2.3.4. Stored verbatim, zones.Render then writes all of it
// into the master file — and the trailing line is a second, entirely
// unrelated record that the zone never held and that no page in the UI shows.
// An A record's rdata carries no name and no origin ambiguity, so a fix
// scoped to name-valued types would leave this standing.
func TestRecordCreateRDataCannotSmuggleARecordIntoTheExport(t *testing.T) {
	srv, zid := newTestServerWithZone(t, "e412.in")
	body := recordBody(t, "host", "A", 300, "1.2.3.4\nevil.e412.in. 300 IN A 6.6.6.6")
	if rec := srv.do(t, "POST", fmt.Sprintf("/api/v1/zones/%d/records", zid), body); rec.Code != http.StatusCreated {
		t.Fatalf("status = %d body = %s, want 201", rec.Code, rec.Body)
	}
	recs := srv.recordsByType(t, zid, "A")
	if len(recs) != 1 || recs[0].RData != "1.2.3.4" {
		t.Fatalf("stored = %+v, want a single A of exactly 1.2.3.4", recs)
	}
	export := srv.do(t, "GET", fmt.Sprintf("/api/v1/zones/%d/file", zid), "")
	if strings.Contains(export.Body.String(), "evil") || strings.Contains(export.Body.String(), "6.6.6.6") {
		t.Errorf("the export carries a record the zone does not hold:\n%s", export.Body)
	}
}

// PUT goes through buildZoneRecord too, so it normalises on the same terms —
// otherwise editing a record would be the way to put a raw value back.
func TestRecordUpdateNormalizesRData(t *testing.T) {
	srv, zid := newTestServerWithZone(t, "e412.in")
	id := srv.createRecord(t, zid, `{"name":"git","type":"CNAME","ttl":300,"rdata":"nas.e412.in."}`)

	path := fmt.Sprintf("/api/v1/zones/%d/records/%d", zid, id)
	if r := srv.do(t, "PUT", path, recordBody(t, "git", "CNAME", 300, "other.e412.in")); r.Code != http.StatusNoContent {
		t.Fatalf("put status = %d body = %s, want 204", r.Code, r.Body)
	}
	recs := srv.recordsByType(t, zid, "CNAME")
	if len(recs) != 1 || recs[0].RData != "other.e412.in." {
		t.Fatalf("stored = %+v, want rdata other.e412.in.", recs)
	}
}

// dns.NewRR does not treat a value that is entirely a ';' comment as an
// error — the comment is simply not part of the line, so it hands back a
// TXT whose rdata is absent and reports nothing. Stored, that record
// renders as "note 300 IN TXT " with nothing after the type, and the zone
// exports to a file that will not reimport.
func TestRecordCreateRejectsRDataThatParsesToNothing(t *testing.T) {
	for _, raw := range []string{"; just a comment", "   ", ""} {
		srv, zid := newTestServerWithZone(t, "e412.in")
		body := recordBody(t, "note", "TXT", 300, raw)
		rec := srv.do(t, "POST", fmt.Sprintf("/api/v1/zones/%d/records", zid), body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("rdata %q: status = %d body = %s, want 400", raw, rec.Code, rec.Body)
		}
		if recs := srv.records(t, zid); len(recs) != 0 {
			t.Errorf("rdata %q: stored an unexportable record: %+v", raw, recs)
		}
	}
}

// A secondary zone's records are its primary's, arriving whole on every
// transfer. A hand write into one is not merged with what the primary sends
// and is not preserved by it — it is silently deleted by the next refresh,
// which is worse than being refused, because between the write and the
// refresh the server serves it authoritatively.
//
// This became reachable with Task 2: before a transfer existed, a record
// written into a secondary simply stayed there.
func TestRecordWritesIntoASecondaryAreRefused(t *testing.T) {
	srv := newTestServer(t)
	now := time.Now().UnixMilli()
	zid, err := srv.store.Zones().AddZone(t.Context(), store.Zone{
		Name: "e412.in", Type: "secondary", Enabled: true,
		SOANS: "ns1.upstream.example", SOAMbox: "hostadmin.e412.in",
		SOASerial: 7, SOARefresh: 900, SOARetry: 300, SOAExpire: 604800,
		SOAMinimum: 900, SOATTL: 900,
		Primaries: "192.168.150.5", CreatedAt: now, ModifiedAt: now,
	})
	if err != nil {
		t.Fatalf("seed secondary: %v", err)
	}
	// A row the transfer would have installed, seeded through the store so
	// there is something for PUT and DELETE to aim at.
	rid, err := srv.store.Zones().AddRecord(t.Context(), store.ZoneRecord{
		ZoneID: zid, Name: "bifrost", Type: "A", TTL: 300, RData: "10.9.0.10", Enabled: true,
	})
	if err != nil {
		t.Fatalf("seed record: %v", err)
	}

	base := fmt.Sprintf("/api/v1/zones/%d", zid)
	for _, c := range []struct{ method, path, body string }{
		{"POST", base + "/records", `{"name":"nas","type":"A","ttl":300,"rdata":"10.9.0.20"}`},
		{"PUT", fmt.Sprintf("%s/records/%d", base, rid), `{"name":"bifrost","type":"A","ttl":60,"rdata":"10.9.0.99"}`},
		{"DELETE", fmt.Sprintf("%s/records/%d", base, rid), ""},
		{"POST", base + "/file", `{"content":"@ 900 IN SOA ns1.e412.in. h.e412.in. 1 900 300 604800 900\n","dry_run":false}`},
	} {
		rec := srv.do(t, c.method, c.path, c.body)
		if rec.Code != http.StatusConflict {
			t.Errorf("%s %s: status = %d body = %s, want 409", c.method, c.path, rec.Code, rec.Body)
		}
		if !strings.Contains(rec.Body.String(), "primary") {
			t.Errorf("%s %s: %s does not say where the records come from", c.method, c.path, rec.Body)
		}
	}

	// Nothing got through, including the delete.
	if recs := srv.records(t, zid); len(recs) != 1 || recs[0].RData != "10.9.0.10" {
		t.Errorf("records = %+v, want the one seeded row unchanged", recs)
	}
}

// Reads are untouched: a secondary's records are listable and its zone file
// exportable, which is what the zone detail screen shows.
func TestSecondaryZoneRecordsStayReadable(t *testing.T) {
	srv := newTestServer(t)
	now := time.Now().UnixMilli()
	zid, err := srv.store.Zones().AddZone(t.Context(), store.Zone{
		Name: "e412.in", Type: "secondary", Enabled: true,
		SOANS: "ns1.upstream.example", SOAMbox: "hostadmin.e412.in",
		SOASerial: 7, SOARefresh: 900, SOARetry: 300, SOAExpire: 604800,
		SOAMinimum: 900, SOATTL: 900,
		Primaries: "192.168.150.5", CreatedAt: now, ModifiedAt: now,
	})
	if err != nil {
		t.Fatalf("seed secondary: %v", err)
	}
	base := fmt.Sprintf("/api/v1/zones/%d", zid)
	for _, path := range []string{base + "/records", base + "/file"} {
		if rec := srv.do(t, "GET", path, ""); rec.Code != http.StatusOK {
			t.Errorf("GET %s: status = %d, want 200", path, rec.Code)
		}
	}
}

// A stub zone's records are the NS set it fetched, and the next fetch
// replaces the whole set through the same DiffRecords a transfer uses
// (StubFetcher.Fetch) — so a hand write into one is deleted on the SOA's own
// schedule, exactly as it is in a secondary, and with nothing to say so.
//
// It is worse here than in a secondary in one way. A stub's records are not
// answered from at all; they are read back by StubUpstreams to rebuild the
// conditional routing table on every zone reload. So a hand-written apex NS
// record does not merely get served and then vanish — until the next fetch
// it *changes where the whole suffix is routed*, which is the one effect an
// operator writing a record here would least expect to have.
//
// This became reachable with Task 7: before the fetcher existed, a record
// written into a stub simply stayed there.
func TestRecordWritesIntoAStubAreRefused(t *testing.T) {
	srv := newTestServer(t)
	now := time.Now().UnixMilli()
	zid, err := srv.store.Zones().AddZone(t.Context(), store.Zone{
		Name: "ad.corp.example", Type: "stub", Enabled: true,
		SOANS: "dc01.ad.corp.example", SOAMbox: "hostadmin.ad.corp.example",
		SOASerial: 7, SOARefresh: 900, SOARetry: 300, SOAExpire: 604800,
		SOAMinimum: 900, SOATTL: 900,
		Primaries: "10.0.0.9", CreatedAt: now, ModifiedAt: now,
	})
	if err != nil {
		t.Fatalf("seed stub: %v", err)
	}
	// A row the fetch would have installed, seeded through the store so there
	// is something for PUT and DELETE to aim at.
	rid, err := srv.store.Zones().AddRecord(t.Context(), store.ZoneRecord{
		ZoneID: zid, Name: "@", Type: "NS", TTL: 86400, RData: "dc01.ad.corp.example.", Enabled: true,
	})
	if err != nil {
		t.Fatalf("seed record: %v", err)
	}

	base := fmt.Sprintf("/api/v1/zones/%d", zid)
	// All four entry points, the file import included: gating only the
	// record routes would leave an import able to replace the whole set
	// silently, which is the larger of the two writes.
	for _, c := range []struct{ method, path, body string }{
		{"POST", base + "/records", `{"name":"@","type":"NS","ttl":86400,"rdata":"dc99.ad.corp.example."}`},
		{"PUT", fmt.Sprintf("%s/records/%d", base, rid), `{"name":"@","type":"NS","ttl":60,"rdata":"dc99.ad.corp.example."}`},
		{"DELETE", fmt.Sprintf("%s/records/%d", base, rid), ""},
		{"POST", base + "/file", `{"content":"@ 900 IN SOA dc01.ad.corp.example. h.ad.corp.example. 1 900 300 604800 900\n","dry_run":false}`},
	} {
		rec := srv.do(t, c.method, c.path, c.body)
		if rec.Code != http.StatusConflict {
			t.Errorf("%s %s: status = %d body = %s, want 409", c.method, c.path, rec.Code, rec.Body)
		}
		// Where they come from, in the words that are true of a stub. A stub
		// does not transfer — it asks its master two ordinary questions,
		// which is the whole reason the type exists apart from a secondary —
		// so a message naming a transfer would point an operator at a
		// mechanism that never runs here. Two strings this milestone has
		// already had to fix for that exact reason.
		if body := rec.Body.String(); !strings.Contains(body, "master") {
			t.Errorf("%s %s: %s does not say where the records come from", c.method, c.path, body)
		}
		if body := rec.Body.String(); strings.Contains(body, "transfer") {
			t.Errorf("%s %s: %s calls a stub's fetch a transfer", c.method, c.path, body)
		}
	}

	// Nothing got through, including the delete.
	if recs := srv.records(t, zid); len(recs) != 1 || recs[0].RData != "dc01.ad.corp.example." {
		t.Errorf("records = %+v, want the one seeded row unchanged", recs)
	}
}

// Reads are untouched, on the same terms as a secondary's: a stub's fetched
// NS set is what the zone detail screen lists, read-only, and its zone file
// is exportable.
func TestStubZoneRecordsStayReadable(t *testing.T) {
	srv := newTestServer(t)
	now := time.Now().UnixMilli()
	zid, err := srv.store.Zones().AddZone(t.Context(), store.Zone{
		Name: "ad.corp.example", Type: "stub", Enabled: true,
		SOANS: "dc01.ad.corp.example", SOAMbox: "hostadmin.ad.corp.example",
		SOASerial: 7, SOARefresh: 900, SOARetry: 300, SOAExpire: 604800,
		SOAMinimum: 900, SOATTL: 900,
		Primaries: "10.0.0.9", CreatedAt: now, ModifiedAt: now,
	})
	if err != nil {
		t.Fatalf("seed stub: %v", err)
	}
	base := fmt.Sprintf("/api/v1/zones/%d", zid)
	for _, path := range []string{base + "/records", base + "/file"} {
		if rec := srv.do(t, "GET", path, ""); rec.Code != http.StatusOK {
			t.Errorf("GET %s: status = %d, want 200", path, rec.Code)
		}
	}
}

// The other half of the refusal, and the half that tells "refuses a stub"
// apart from "refuses everything that is not a primary".
//
// A forwarder is deliberately **not** refused. Nothing overwrites its
// records: it answers from none of them (Zone.Answer returns handled=false
// for the type) and its routing comes from forward_to rather than from an NS
// set, so no scheduled job ever replaces what is written here. The rule this
// function encodes is "these records are authored somewhere else and this
// write will be destroyed", and neither half of that is true of a forwarder.
// A write into one is inert, not lost — a different complaint, and not a
// 409's to make.
//
// Without this case the test above passes just as happily against a
// `default: return "..."` that refuses every non-primary type, which is a
// stricter rule than the one intended and would need its own decision.
func TestRecordWritesIntoAForwarderAreStillAccepted(t *testing.T) {
	srv := newTestServer(t)
	now := time.Now().UnixMilli()
	zid, err := srv.store.Zones().AddZone(t.Context(), store.Zone{
		Name: "corp.example", Type: "forwarder", Enabled: true,
		SOANS: "ns.corp.example", SOAMbox: "hostadmin.corp.example",
		SOASerial: 7, SOARefresh: 900, SOARetry: 300, SOAExpire: 604800,
		SOAMinimum: 900, SOATTL: 900,
		ForwardTo: "10.0.0.1:53", CreatedAt: now, ModifiedAt: now,
	})
	if err != nil {
		t.Fatalf("seed forwarder: %v", err)
	}
	base := fmt.Sprintf("/api/v1/zones/%d", zid)
	rec := srv.do(t, "POST", base+"/records", `{"name":"nas","type":"A","ttl":300,"rdata":"10.9.0.20"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST records: status = %d body = %s, want 201", rec.Code, rec.Body)
	}
	if recs := srv.records(t, zid); len(recs) != 1 {
		t.Errorf("records = %+v, want the written row", recs)
	}
}

// And a primary, which is the type the whole route exists for — the same
// discrimination from the other side, so a refusal that spread to everything
// could not hide behind the forwarder case alone.
func TestRecordWritesIntoAPrimaryAreStillAccepted(t *testing.T) {
	srv, zid := newTestServerWithZone(t, "e412.in")
	rec := srv.do(t, "POST", fmt.Sprintf("/api/v1/zones/%d/records", zid),
		`{"name":"nas","type":"A","ttl":300,"rdata":"10.9.0.20"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST records: status = %d body = %s, want 201", rec.Code, rec.Body)
	}
}

// The rdata half of this is TestRecordCreateRejectsRDataThatParsesToNothing;
// the name half went further than a bad value. An owner beginning ';' makes
// dns.NewRR read the whole line as a comment and return (nil, nil), which
// BuildRecord dereferenced — so this request left with no response at all
// rather than a 400.
//
// The rest are names that are stored and served happily and then cannot be
// read back out of the zone's own export: Render writes the name at the
// start of a line, where '$' opens a directive and a quote or parenthesis
// runs on into whatever follows.
func TestRecordCreateRejectsNamesTheExportCannotCarry(t *testing.T) {
	for _, name := range []string{";x", "$ttl", "$origin", `a"b`, "a(b", "a b", "a..b", `a\.b`} {
		srv, zid := newTestServerWithZone(t, "e412.in")
		body := recordBody(t, name, "A", 300, "1.2.3.4")
		rec := srv.do(t, "POST", fmt.Sprintf("/api/v1/zones/%d/records", zid), body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("name %q: status = %d body = %s, want 400", name, rec.Code, rec.Body)
		}
		if recs := srv.records(t, zid); len(recs) != 0 {
			t.Errorf("name %q: stored a record the export cannot carry: %+v", name, recs)
		}
	}
}

// A zone's SOA is a zones-row field, because its serial needs managed
// increments. A zone_records row of type SOA is therefore a second SOA that
// the apex answer ignores, Render writes out beside the real one, and Parse
// then rejects on the way back in — one accepted write and the zone no
// longer round-trips through its own export.
func TestRecordCreateRefusesSOARows(t *testing.T) {
	srv, zid := newTestServerWithZone(t, "e412.in")
	body := recordBody(t, "@", "SOA", 900, "ns.e412.in. hostadmin.e412.in. 9 900 300 604800 900")
	rec := srv.do(t, "POST", fmt.Sprintf("/api/v1/zones/%d/records", zid), body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body = %s, want 400", rec.Code, rec.Body)
	}
	if recs := srv.records(t, zid); len(recs) != 0 {
		t.Fatalf("stored a second SOA: %+v", recs)
	}
	// The export still has to reimport, which is what the refusal protects.
	srv.reimport(t, zid, "e412.in")
}

// Importing a file *with* an SOA keeps working: the file's apex SOA becomes
// the zone's own (it is taken onto the zones row, not stored as a record),
// so the refusal above must not reach it.
func TestZoneFileImportStillTakesTheFilesSOA(t *testing.T) {
	const file = `$ORIGIN e412.in.
$TTL 300
@ IN SOA ns.e412.in. hostadmin.e412.in. ( 42 900 300 604800 900 )
@ IN NS ns.e412.in.
host 300 IN A 1.2.3.4
`
	srv, zid := newTestServerWithZone(t, "e412.in")
	if rec := srv.do(t, "POST", fmt.Sprintf("/api/v1/zones/%d/file", zid), importBody(t, file, false)); rec.Code != http.StatusOK {
		t.Fatalf("import status = %d body = %s, want 200", rec.Code, rec.Body)
	}
	if got := srv.zone(t, zid).SOASerial; got != 43 {
		t.Errorf("zone serial = %d, want the file's 42 + 1", got)
	}
	for _, r := range srv.records(t, zid) {
		if r.Type == "SOA" {
			t.Errorf("the file's SOA was stored as a record: %+v", r)
		}
	}
}

// An RFC 3597 unknown type parses, stores and exports, and is unservable at
// every step in between: rrType maps TYPEnnn to no wire type, so no query
// ever matches it, and RDataOf stores the whole RR text because RFC3597
// prints its class as CLASS1 where the header prints IN. Refused on both
// write paths, naming the type.
func TestUnknownRecordTypesAreRefusedOnBothWritePaths(t *testing.T) {
	srv, zid := newTestServerWithZone(t, "e412.in")

	body := recordBody(t, "x", "TYPE65280", 300, `\# 4 01020304`)
	rec := srv.do(t, "POST", fmt.Sprintf("/api/v1/zones/%d/records", zid), body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("hand write: status = %d body = %s, want 400", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "TYPE65280") {
		t.Errorf("hand write: refusal does not name the type: %s", rec.Body)
	}

	const file = `$ORIGIN e412.in.
$TTL 300
@ IN SOA ns.e412.in. hostadmin.e412.in. ( 1 900 300 604800 900 )
x 300 IN TYPE65280 \# 4 01020304
`
	imp := srv.do(t, "POST", fmt.Sprintf("/api/v1/zones/%d/file", zid), importBody(t, file, false))
	if imp.Code != http.StatusUnprocessableEntity {
		t.Fatalf("import: status = %d body = %s, want 422", imp.Code, imp.Body)
	}
	if !strings.Contains(imp.Body.String(), "TYPE65280") {
		t.Errorf("import: refusal does not name the type: %s", imp.Body)
	}
	if recs := srv.records(t, zid); len(recs) != 0 {
		t.Errorf("stored an unservable record: %+v", recs)
	}
}

// "@" is the zone apex in a master file, so `10 @` imported into e412.in is
// the MX 10 e412.in. The hand-write path read the same two characters under
// the root and stored the null MX `10 .` (RFC 7505) — the same text, two
// meanings, which is the drift the stored-rdata normalisation exists to
// prevent (TestRecordWriteStoresRDataWithOneMeaningEverywhere).
func TestBothWritersReadBareAtAsTheApex(t *testing.T) {
	for _, tc := range []struct{ what, name, recType, rdata string }{
		{"MX exchange", "@", "MX", "10 @"},
		{"CNAME target", "git", "CNAME", "@"},
		// A TXT's rdata is a string, not a name, and neither writer resolves
		// it — the agreement has to hold for the type where "@" stays "@".
		{"TXT string", "note", "TXT", "@"},
	} {
		t.Run(tc.what, func(t *testing.T) {
			handSrv, handZID := newTestServerWithZone(t, "e412.in")
			body := recordBody(t, tc.name, tc.recType, 300, tc.rdata)
			if rec := handSrv.do(t, "POST", fmt.Sprintf("/api/v1/zones/%d/records", handZID), body); rec.Code != http.StatusCreated {
				t.Fatalf("hand write: status = %d body = %s, want 201", rec.Code, rec.Body)
			}
			hand := handSrv.recordsByType(t, handZID, tc.recType)
			if len(hand) != 1 {
				t.Fatalf("hand write stored %+v, want one %s", hand, tc.recType)
			}

			file := fmt.Sprintf("$ORIGIN e412.in.\n$TTL 300\n"+
				"@ IN SOA ns.e412.in. hostadmin.e412.in. ( 1 900 300 604800 900 )\n"+
				"%s 300 IN %s %s\n", tc.name, tc.recType, tc.rdata)
			impSrv, impZID := newTestServerWithZone(t, "e412.in")
			if rec := impSrv.do(t, "POST", fmt.Sprintf("/api/v1/zones/%d/file", impZID), importBody(t, file, false)); rec.Code != http.StatusOK {
				t.Fatalf("import: status = %d body = %s, want 200", rec.Code, rec.Body)
			}
			imported := impSrv.recordsByType(t, impZID, tc.recType)
			if len(imported) != 1 {
				t.Fatalf("import stored %+v, want one %s", imported, tc.recType)
			}

			if hand[0].RData != imported[0].RData {
				t.Errorf("the same %s rdata %q is stored as %q by hand and %q by import",
					tc.recType, tc.rdata, hand[0].RData, imported[0].RData)
			}
		})
	}
}
