package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/aloks98/dnsaur/internal/store"
	"github.com/aloks98/dnsaur/internal/zones"
)

// zoneTestServer is a thin convenience wrapper over the package's existing
// testServer/doReq/login harness (server_test.go): it adds the zone-focused
// helpers (do/zone/records) the brief's tests are written against, without
// duplicating any of what testServer/doReq/login already do — every request
// still goes through the real Server.Handler(), and store.Zones() is used
// only to read back what a handler wrote, never to bypass one.
type zoneTestServer struct {
	srv    *Server
	store  store.Store
	rl     *fakeReloader
	cookie *http.Cookie
}

func newTestServer(t *testing.T) *zoneTestServer {
	t.Helper()
	srv, s, rl := testServer(t)
	cookie := login(t, srv, s)
	return &zoneTestServer{srv: srv, store: s, rl: rl, cookie: cookie}
}

func (ts *zoneTestServer) do(t *testing.T, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	return doReq(t, ts.srv.Handler(), method, path, body, ts.cookie)
}

func (ts *zoneTestServer) zone(t *testing.T, id int64) store.Zone {
	t.Helper()
	z, err := ts.store.Zones().Zone(t.Context(), id)
	if err != nil {
		t.Fatalf("zone %d: %v", id, err)
	}
	return z
}

func (ts *zoneTestServer) records(t *testing.T, zoneID int64) []store.ZoneRecord {
	t.Helper()
	recs, err := ts.store.Zones().Records(t.Context(), zoneID)
	if err != nil {
		t.Fatalf("records for zone %d: %v", zoneID, err)
	}
	return recs
}

// recordsByType is records() narrowed to one RR type — for assertions about
// a single type in a zone that also holds records the test didn't write
// (every API-created zone gets an apex NS, and a reverse zone gets its PTRs
// alongside it).
func (ts *zoneTestServer) recordsByType(t *testing.T, zoneID int64, recType string) []store.ZoneRecord {
	t.Helper()
	var out []store.ZoneRecord
	for _, r := range ts.records(t, zoneID) {
		if r.Type == recType {
			out = append(out, r)
		}
	}
	return out
}

// allZones is every zone in the store, built-ins included — for assertions
// about the zone set as a whole rather than one zone the test created.
func (ts *zoneTestServer) allZones(t *testing.T) []store.Zone {
	t.Helper()
	zs, err := ts.store.Zones().Zones(t.Context())
	if err != nil {
		t.Fatalf("zones: %v", err)
	}
	return zs
}

// createZone creates a zone through POST /api/v1/zones and returns its id,
// failing the test if the create didn't. Unlike newTestServerWithZone
// (zonerecords_handlers_test.go), which seeds through the store to leave the
// apex bare, this goes through the handler — so the zone is exactly what a
// user would get, apex NS included.
func (ts *zoneTestServer) createZone(t *testing.T, name string) int64 {
	t.Helper()
	rec := ts.do(t, "POST", "/api/v1/zones", fmt.Sprintf(`{"name":%q}`, name))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create zone %q: status = %d body = %s", name, rec.Code, rec.Body)
	}
	return createdID(t, rec)
}

// createRecord creates a record through POST /api/v1/zones/{id}/records and
// returns its id, failing the test if the create didn't.
func (ts *zoneTestServer) createRecord(t *testing.T, zoneID int64, body string) int64 {
	t.Helper()
	rec := ts.do(t, "POST", fmt.Sprintf("/api/v1/zones/%d/records", zoneID), body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create record in zone %d: status = %d body = %s", zoneID, rec.Code, rec.Body)
	}
	return createdID(t, rec)
}

// createdID reads the {"id": N} body both create handlers answer with.
func createdID(t *testing.T, rec *httptest.ResponseRecorder) int64 {
	t.Helper()
	var got struct{ ID int64 }
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal create response %s: %v", rec.Body, err)
	}
	if got.ID == 0 {
		t.Fatalf("create response carried no id: %s", rec.Body)
	}
	return got.ID
}

// zoneIDByName looks up a seeded zone's id by name. The built-in zones
// (store.BuiltinZones) are seeded by migration rather than created through
// the API, so tests that need one of their ids can't get it back from a
// create response the way every other zone test does.
func (ts *zoneTestServer) zoneIDByName(t *testing.T, name string) int64 {
	t.Helper()
	zs := ts.allZones(t)
	for _, z := range zs {
		if z.Name == name {
			return z.ID
		}
	}
	t.Fatalf("no zone named %q among %+v", name, zs)
	return 0
}

func TestZoneCreateDefaultsSOA(t *testing.T) {
	srv := newTestServer(t)
	rec := srv.do(t, "POST", "/api/v1/zones", `{"name":"e412.in"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body)
	}
	var got struct{ ID int64 }
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	z := srv.zone(t, got.ID)
	// A zone with no SOA cannot answer NXDOMAIN, so the API fills one in
	// rather than accepting a zone that is authoritative in name only.
	if z.SOANS == "" || z.SOAMbox == "" || z.SOAMinimum == 0 || z.SOASerial != 1 {
		t.Errorf("SOA defaults not applied: %+v", z)
	}
	if z.Type != "primary" || !z.Enabled {
		t.Errorf("type/enabled = %q/%v, want primary/true", z.Type, z.Enabled)
	}
}

func TestZoneCreateRejectsBadName(t *testing.T) {
	srv := newTestServer(t)
	for _, name := range []string{"", "not a domain", "e412..in"} {
		if rec := srv.do(t, "POST", "/api/v1/zones", `{"name":"`+name+`"}`); rec.Code != http.StatusBadRequest {
			t.Errorf("POST name=%q status = %d, want 400", name, rec.Code)
		}
	}
}

// primary, secondary, forwarder and stub are the four types this API can
// create. internal is the RFC 6303 built-ins seeded at migration and stays
// refused — a type that cannot be created cannot misbehave. Any type outside
// the four must 400, not just nonsense values.
//
// stub and forwarder were in this list through Milestone D6's Task 4 (see
// TestZoneCreateAcceptsForwarderAndStub); dropped once each became
// creatable, the same way secondary was dropped when D2 landed — keeping a
// type here after it becomes creatable would measure nothing, the failure
// mode TestZonePatchRejectsAnUnservableType's doc comment already names.
func TestZoneCreateRejectsAnUnservableType(t *testing.T) {
	srv := newTestServer(t)
	for _, zt := range []string{"internal", "banana"} {
		body := `{"name":"e412.in","type":"` + zt + `"}`
		if rec := srv.do(t, "POST", "/api/v1/zones", body); rec.Code != http.StatusBadRequest {
			t.Errorf("POST type=%q status = %d, want 400", zt, rec.Code)
		}
	}
}

func TestZoneCreateRejectsDuplicate(t *testing.T) {
	srv := newTestServer(t)
	srv.do(t, "POST", "/api/v1/zones", `{"name":"e412.in"}`)
	if rec := srv.do(t, "POST", "/api/v1/zones", `{"name":"e412.in"}`); rec.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409", rec.Code)
	}
}

func TestZoneDeleteTakesRecords(t *testing.T) {
	srv := newTestServer(t)
	rec := srv.do(t, "POST", "/api/v1/zones", `{"name":"e412.in"}`)
	var got struct{ ID int64 }
	_ = json.Unmarshal(rec.Body.Bytes(), &got)

	// Add a second record beyond the auto-created apex NS, so the delete
	// assertion below proves every record went with the zone, not just the
	// seeded one.
	if _, err := srv.store.Zones().AddRecord(t.Context(), store.ZoneRecord{
		ZoneID: got.ID, Name: "www", Type: "A", TTL: 300, RData: "192.168.1.9", Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	if recs := srv.records(t, got.ID); len(recs) != 2 {
		t.Fatalf("records before delete = %+v, want 2", recs)
	}

	if rec := srv.do(t, "DELETE", fmt.Sprintf("/api/v1/zones/%d", got.ID), ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d body = %s", rec.Code, rec.Body)
	}
	if recs := srv.records(t, got.ID); len(recs) != 0 {
		t.Fatalf("records survived zone delete: %+v", recs)
	}
	if _, _, filters := srv.rl.counts(); filters != 0 {
		t.Errorf("unexpected filter reload count: %d", filters)
	}
}

// RFC 2181 §10.1: a zone's apex must have NS records, or the zone is
// malformed from the moment it exists.
func TestZoneCreateMakesApexNS(t *testing.T) {
	srv := newTestServer(t)
	rec := srv.do(t, "POST", "/api/v1/zones", `{"name":"e412.in"}`)
	var got struct{ ID int64 }
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	recs := srv.records(t, got.ID)
	if len(recs) != 1 || recs[0].Name != "@" || recs[0].Type != "NS" {
		t.Fatalf("records = %+v; want one apex NS (RFC 2181 10.1)", recs)
	}
}

// store.AddZone binds soa_ttl explicitly, so a zero SOATTL is written as 0
// rather than falling back to the schema's DEFAULT 900. The negative-answer
// TTL is min(SOAMinimum, SOATTL), so a zero here silently makes every
// NXDOMAIN this zone hands out uncacheable.
func TestZoneCreateDefaultsSOATTLNonZero(t *testing.T) {
	srv := newTestServer(t)
	rec := srv.do(t, "POST", "/api/v1/zones", `{"name":"e412.in"}`)
	var got struct{ ID int64 }
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	// Zero here silently disables negative caching for the whole zone.
	if z := srv.zone(t, got.ID); z.SOATTL == 0 {
		t.Fatal("SOATTL = 0 — every NXDOMAIN from this zone would be uncacheable")
	}
}

func TestZonesListAndGet(t *testing.T) {
	srv := newTestServer(t)
	if rec := srv.do(t, "GET", "/api/v1/zones", ""); rec.Code != http.StatusOK {
		t.Fatalf("list on empty store: %d %s", rec.Code, rec.Body)
	} else {
		var zs []store.Zone
		if err := json.Unmarshal(rec.Body.Bytes(), &zs); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if zs == nil {
			t.Fatal("zones list marshaled to null, want []")
		}
	}

	rec := srv.do(t, "POST", "/api/v1/zones", `{"name":"e412.in"}`)
	var got struct{ ID int64 }
	_ = json.Unmarshal(rec.Body.Bytes(), &got)

	if rec := srv.do(t, "GET", "/api/v1/zones", ""); rec.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rec.Code, rec.Body)
	} else {
		var zs []store.Zone
		_ = json.Unmarshal(rec.Body.Bytes(), &zs)
		// Every fresh store also carries the RFC 6303 built-in zones
		// (store.BuiltinZones), so the list holds those plus the one zone
		// this test created — an exact count, not just "contains it", so a
		// bug that duplicates or leaks an extra zone through this exact
		// create+list path would still be caught.
		if want := 1 + len(store.BuiltinZones); len(zs) != want {
			t.Fatalf("list = %d zones (%+v), want %d (the built-ins + the created zone)", len(zs), zs, want)
		}
		var found bool
		for _, z := range zs {
			if z.ID == got.ID {
				found = true
			}
		}
		if !found {
			t.Fatalf("list = %+v, want it to contain the created zone id %d", zs, got.ID)
		}
	}

	rec = srv.do(t, "GET", fmt.Sprintf("/api/v1/zones/%d", got.ID), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get: %d %s", rec.Code, rec.Body)
	}
	var z store.Zone
	_ = json.Unmarshal(rec.Body.Bytes(), &z)
	if z.Name != "e412.in" {
		t.Fatalf("get name = %q, want e412.in", z.Name)
	}

	if rec := srv.do(t, "GET", "/api/v1/zones/999999", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("get missing: %d, want 404", rec.Code)
	}
}

func TestZonePatch(t *testing.T) {
	srv := newTestServer(t)
	rec := srv.do(t, "POST", "/api/v1/zones", `{"name":"e412.in"}`)
	var got struct{ ID int64 }
	_ = json.Unmarshal(rec.Body.Bytes(), &got)

	path := fmt.Sprintf("/api/v1/zones/%d", got.ID)
	if rec := srv.do(t, "PATCH", path, `{"enabled":false,"soa_refresh":1800}`); rec.Code != http.StatusNoContent {
		t.Fatalf("patch: %d %s", rec.Code, rec.Body)
	}
	z := srv.zone(t, got.ID)
	if z.Enabled || z.SOARefresh != 1800 {
		t.Fatalf("patch not applied: %+v", z)
	}
	// The rest of the SOA is untouched by a partial patch.
	if z.SOAMinimum != 900 || z.SOAMbox != "hostadmin.e412.in" {
		t.Fatalf("patch touched fields it shouldn't have: %+v", z)
	}

	if rec := srv.do(t, "PATCH", path, `{"name":""}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("patch empty name: %d, want 400", rec.Code)
	}
	if rec := srv.do(t, "PATCH", "/api/v1/zones/999999", `{"enabled":true}`); rec.Code != http.StatusNotFound {
		t.Fatalf("patch missing zone: %d, want 404", rec.Code)
	}

	if clients, records, _ := srv.rl.counts(); clients != 0 || records == 0 {
		t.Errorf("mutations did not trigger ReloadZones: clients=%d records=%d", clients, records)
	}
	if n := srv.rl.notifyCount(); n == 0 {
		t.Error("mutations did not trigger NotifyZones")
	}
}

// The PATCH twin of TestZoneCreateRejectsAnUnservableType. Renamed from
// TestZonePatchRejectsNonPrimaryType, which used "secondary" as its rejected
// type and so measured nothing once secondary became creatable — it would
// have kept passing on the missing primaries alone, which is
// TestPatchToSecondaryRequiresPrimaries' job. stub and forwarder were
// dropped from the list the same way, once D6's Task 4 made them creatable
// too — see TestZoneCreateAcceptsForwarderAndStub.
func TestZonePatchRejectsAnUnservableType(t *testing.T) {
	srv := newTestServer(t)
	rec := srv.do(t, "POST", "/api/v1/zones", `{"name":"e412.in"}`)
	var got struct{ ID int64 }
	_ = json.Unmarshal(rec.Body.Bytes(), &got)

	path := fmt.Sprintf("/api/v1/zones/%d", got.ID)
	for _, zt := range []string{"internal", "banana"} {
		if rec := srv.do(t, "PATCH", path, `{"type":"`+zt+`"}`); rec.Code != http.StatusBadRequest {
			t.Errorf("PATCH type=%q status = %d, want 400", zt, rec.Code)
		}
		if z := srv.zone(t, got.ID); z.Type != "primary" {
			t.Fatalf("type changed despite rejection: %q", z.Type)
		}
	}
}

func TestZonePatchRejectsDuplicateName(t *testing.T) {
	srv := newTestServer(t)
	srv.do(t, "POST", "/api/v1/zones", `{"name":"e412.in"}`)
	rec := srv.do(t, "POST", "/api/v1/zones", `{"name":"other.test"}`)
	var got struct{ ID int64 }
	_ = json.Unmarshal(rec.Body.Bytes(), &got)

	path := fmt.Sprintf("/api/v1/zones/%d", got.ID)
	if rec := srv.do(t, "PATCH", path, `{"name":"e412.in"}`); rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
}

func TestZoneDeleteUnknownIs404(t *testing.T) {
	srv := newTestServer(t)
	if rec := srv.do(t, "DELETE", "/api/v1/zones/999999", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// A built-in zone is infrastructure, not content. The UI already hides these
// controls; without a server guard the API would honour a request the UI
// never offers, and a built-in could be edited or deleted out from under it.
func TestInternalZoneRejectsWrites(t *testing.T) {
	srv := newTestServer(t)
	id := srv.zoneIDByName(t, "localhost")

	if rec := srv.do(t, "PATCH", fmt.Sprintf("/api/v1/zones/%d", id), `{"enabled":false}`); rec.Code != http.StatusConflict {
		t.Errorf("PATCH status = %d, want 409", rec.Code)
	}
	if rec := srv.do(t, "DELETE", fmt.Sprintf("/api/v1/zones/%d", id), ""); rec.Code != http.StatusConflict {
		t.Errorf("DELETE status = %d, want 409", rec.Code)
	}
	body := `{"name":"x","type":"A","ttl":300,"rdata":"1.2.3.4"}`
	if rec := srv.do(t, "POST", fmt.Sprintf("/api/v1/zones/%d/records", id), body); rec.Code != http.StatusConflict {
		t.Errorf("record POST status = %d, want 409", rec.Code)
	}
}

// Reading is fine — the UI lists them and shows their records.
func TestInternalZoneAllowsReads(t *testing.T) {
	srv := newTestServer(t)
	id := srv.zoneIDByName(t, "localhost")
	if rec := srv.do(t, "GET", fmt.Sprintf("/api/v1/zones/%d", id), ""); rec.Code != http.StatusOK {
		t.Errorf("GET zone status = %d, want 200", rec.Code)
	}
	if rec := srv.do(t, "GET", fmt.Sprintf("/api/v1/zones/%d/records", id), ""); rec.Code != http.StatusOK {
		t.Errorf("GET records status = %d, want 200", rec.Code)
	}
}

// A secondary is defined by where it pulls from; without that it is an empty
// zone that answers authoritatively for nothing.
func TestSecondaryZoneRequiresPrimaries(t *testing.T) {
	srv := newTestServer(t)
	rec := srv.do(t, "POST", "/api/v1/zones", `{"name":"e412.in","type":"secondary"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body = %s; want 400", rec.Code, rec.Body)
	}
}

func TestSecondaryZoneRejectsAnUnknownTSIGKey(t *testing.T) {
	srv := newTestServer(t)
	rec := srv.do(t, "POST", "/api/v1/zones",
		`{"name":"e412.in","type":"secondary","primaries":"192.168.150.5","tsig_key_id":9999}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400 — a key that does not exist cannot sign", rec.Code)
	}
}

// The two tests above assert a refusal, and a handler that refused every
// secondary would satisfy both. This is the one that says a secondary is
// creatable at all, and it pins the three things that make it one: the
// primaries string is stored exactly as written (a hostname primary has to
// survive its address changing, so nothing is resolved at write), the TSIG
// key is recorded, and no apex NS is invented — a secondary's contents come
// from its primary, and seeding a record dnsaur made up would put data in a
// zone it does not own.
func TestSecondaryZoneIsCreatable(t *testing.T) {
	srv := newTestServer(t)
	keyID := createTSIGKey(t, srv, "xfer.e412.in.")

	body := `{"name":"e412.in","type":"secondary","primaries":"ns1.upstream.example, 192.168.150.6:5353","tsig_key_id":` + itoa(keyID) + `}`
	rec := srv.do(t, "POST", "/api/v1/zones", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d body = %s; want 201", rec.Code, rec.Body)
	}
	var got struct{ ID int64 }
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	z := srv.zone(t, got.ID)
	if z.Type != "secondary" {
		t.Errorf("type = %q, want secondary", z.Type)
	}
	if z.Primaries != "ns1.upstream.example, 192.168.150.6:5353" {
		t.Errorf("primaries = %q, want it stored as written", z.Primaries)
	}
	if z.TSIGKeyID != keyID {
		t.Errorf("tsig_key_id = %d, want %d", z.TSIGKeyID, keyID)
	}
	if recs := srv.records(t, got.ID); len(recs) != 0 {
		t.Errorf("records = %+v, want none until the first transfer", recs)
	}
}

func TestSecondaryZoneRejectsMalformedPrimaries(t *testing.T) {
	srv := newTestServer(t)
	for _, p := range []string{"not a host", "192.168.150.5:0", "192.168.150.5:banana"} {
		body := `{"name":"e412.in","type":"secondary","primaries":"` + p + `"}`
		if rec := srv.do(t, "POST", "/api/v1/zones", body); rec.Code != http.StatusBadRequest {
			t.Errorf("POST primaries=%q status = %d, want 400", p, rec.Code)
		}
	}
}

// primaries and tsig_key_id describe a transfer. A primary zone has none, so
// accepting them there would store configuration that nothing will ever read
// and that the UI would show as if it meant something.
func TestPrimaryZoneRejectsTransferFields(t *testing.T) {
	srv := newTestServer(t)
	if rec := srv.do(t, "POST", "/api/v1/zones",
		`{"name":"e412.in","primaries":"192.168.150.5"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("primaries on a primary: status = %d, want 400", rec.Code)
	}
	keyID := createTSIGKey(t, srv, "xfer.e412.in.")
	if rec := srv.do(t, "POST", "/api/v1/zones",
		`{"name":"e412.in","tsig_key_id":`+itoa(keyID)+`}`); rec.Code != http.StatusBadRequest {
		t.Errorf("tsig_key_id on a primary: status = %d, want 400", rec.Code)
	}
}

// PATCH has to enforce the same rule as POST or it is the way around it: a
// primary patched to secondary without primaries would be exactly the zone
// TestSecondaryZoneRequiresPrimaries refuses to create.
func TestPatchToSecondaryRequiresPrimaries(t *testing.T) {
	srv := newTestServer(t)
	rec := srv.do(t, "POST", "/api/v1/zones", `{"name":"e412.in"}`)
	var got struct{ ID int64 }
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	path := fmt.Sprintf("/api/v1/zones/%d", got.ID)

	if rec := srv.do(t, "PATCH", path, `{"type":"secondary"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body = %s; want 400", rec.Code, rec.Body)
	}
	if rec := srv.do(t, "PATCH", path, `{"type":"secondary","primaries":"192.168.150.5"}`); rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d body = %s; want 204", rec.Code, rec.Body)
	}
	if z := srv.zone(t, got.ID); z.Type != "secondary" || z.Primaries != "192.168.150.5" {
		t.Fatalf("zone = %+v, want a secondary with its primaries", z)
	}
}

// allow_transfer joins checkZoneTransferConfig's (type, primaries,
// tsig_key_id) triple for the same reason primaries and tsig_key_id do — "a
// rule enforced on POST and not on PATCH is a rule with a way around it".
// What's stored is zones.FormatACL's canonical spelling, never the raw
// input: Task 2's TSIG-key delete guard matches key:<name> inside the
// stored string in SQL and depends on that spelling exactly.
func TestZoneCreateStoresAllowTransferCanonically(t *testing.T) {
	srv := newTestServer(t)
	createTSIGKey(t, srv, "NS2")
	body := `{"name":"e412.in","allow_transfer":" 10.0.0.5 , KEY:NS2 "}`
	rec := srv.do(t, "POST", "/api/v1/zones", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body)
	}
	id := createdID(t, rec)
	if got := srv.zone(t, id).AllowTransfer; got != "10.0.0.5, key:ns2." {
		t.Fatalf("allow_transfer stored as %q, want the canonical spelling", got)
	}
}

func TestZoneCreateRejectsAMalformedAllowTransfer(t *testing.T) {
	srv := newTestServer(t)
	rec := srv.do(t, "POST", "/api/v1/zones", `{"name":"e412.in","allow_transfer":"not-an-ip"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "allow_transfer") {
		t.Fatalf("error does not name the field: %s", rec.Body)
	}
}

func TestZoneCreateRejectsAnACLKeyThatDoesNotExist(t *testing.T) {
	srv := newTestServer(t)
	rec := srv.do(t, "POST", "/api/v1/zones", `{"name":"e412.in","allow_transfer":"key:nobody"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "nobody") {
		t.Fatalf("error does not name the missing key: %s", rec.Body)
	}
}

func TestZonePatchValidatesAllowTransferToo(t *testing.T) {
	srv := newTestServer(t)
	id := srv.createZone(t, "e412.in")
	path := fmt.Sprintf("/api/v1/zones/%d", id)

	if rec := srv.do(t, "PATCH", path, `{"allow_transfer":"10.0.0.0/33"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 — a rule enforced on POST only has a way around it", rec.Code)
	}
	if rec := srv.do(t, "PATCH", path, `{"allow_transfer":"10.0.0.0/24"}`); rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body)
	}
	if z := srv.zone(t, id); z.AllowTransfer != "10.0.0.0/24" {
		t.Fatalf("allow_transfer = %q", z.AllowTransfer)
	}
}

// A secondary re-serves what it pulled (§9.5.3), so allow_transfer is not
// secondary-only the way primaries is.
func TestAllowTransferIsAcceptedOnBothServingTypes(t *testing.T) {
	srv := newTestServer(t)
	for _, body := range []string{
		`{"name":"a.e412.in","type":"primary","allow_transfer":"10.0.0.0/24"}`,
		`{"name":"b.e412.in","type":"secondary","primaries":"192.0.2.1","allow_transfer":"10.0.0.0/24"}`,
	} {
		if rec := srv.do(t, "POST", "/api/v1/zones", body); rec.Code != http.StatusCreated {
			t.Fatalf("POST %s = %d, body %s", body, rec.Code, rec.Body)
		}
	}
}

// notify_to joins allow_transfer as applying to both primary and secondary —
// unlike primaries and tsig_key_id, which are secondary-only — because a
// secondary that re-serves what it pulled has its own downstream secondaries.
func TestZoneCreateAcceptsNotifyToOnBothTransferTypes(t *testing.T) {
	for _, zoneType := range []string{"primary", "secondary"} {
		t.Run(zoneType, func(t *testing.T) {
			srv := newTestServer(t)
			body := fmt.Sprintf(`{"name":"example.com","type":%q,"notify_to":"10.0.0.2, 10.0.0.3:5353"`, zoneType)
			if zoneType == "secondary" {
				body += `,"primaries":"10.0.0.1"`
			}
			body += "}"
			rec := srv.do(t, "POST", "/api/v1/zones", body)
			if rec.Code != http.StatusCreated {
				t.Fatalf("status %d, body %s", rec.Code, rec.Body)
			}
			id := createdID(t, rec)
			// Stored canonical, not as typed — the delete guard matches
			// key:<name> inside this column in SQL and can only do that
			// because the spelling is known.
			if got := srv.zone(t, id).NotifyTo; got != "10.0.0.2:53, 10.0.0.3:5353" {
				t.Errorf("notify_to = %q, want the canonical spelling", got)
			}
		})
	}
}

// The built-ins are the only reachable `internal` zones, and stub/forwarder
// cannot be created at all — see the type gate at the top of
// handleZoneCreate — so the create path with a type the API refuses outright
// is where this rule is observable; a notify list on any of them is
// configuration nothing reads.
func TestZoneNotifyToRefusedOnNonTransferTypes(t *testing.T) {
	srv := newTestServer(t)
	rec := srv.do(t, "POST", "/api/v1/zones", `{"name":"example.com","type":"internal","notify_to":"10.0.0.2"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body)
	}
}

func TestZoneNotifyToRejectsMalformedAndUnknownKeys(t *testing.T) {
	tests := []struct {
		name     string
		notifyTo string
		contains string
	}{
		{"malformed entry", "10.0.0.2:0", "port"},
		{"unknown key", "10.0.0.2 key:nope", "which does not exist"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := newTestServer(t)
			body := fmt.Sprintf(`{"name":"example.com","type":"primary","notify_to":%q}`, tc.notifyTo)
			rec := srv.do(t, "POST", "/api/v1/zones", body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status %d, body %s", rec.Code, rec.Body)
			}
			if !strings.Contains(rec.Body.String(), tc.contains) {
				t.Errorf("body %s does not mention %q", rec.Body, tc.contains)
			}
		})
	}
}

// A rule enforced on POST and not on PATCH is a rule with a way around it —
// checkZoneTransferConfig's own doc comment, and the reason it validates the
// zone as it would be *stored* rather than the request body.
func TestZonePatchValidatesNotifyTo(t *testing.T) {
	srv := newTestServer(t)
	rec := srv.do(t, "POST", "/api/v1/zones", `{"name":"example.com","type":"primary"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create failed: %s", rec.Body)
	}
	id := createdID(t, rec)
	path := fmt.Sprintf("/api/v1/zones/%d", id)

	if bad := srv.do(t, "PATCH", path, `{"notify_to":"10.0.0.2:0"}`); bad.Code != http.StatusBadRequest {
		t.Fatalf("PATCH with a bad port: status %d, body %s", bad.Code, bad.Body)
	}
	// A malformed value is also caught by canonicalNotifyTo's own re-parse,
	// so the case above alone doesn't pin checkZoneTransferConfig's presence
	// on the patch path — an unknown key is checked nowhere else, so this
	// one does: skipping checkZoneTransferConfig on PATCH (verified by hand)
	// lets this 204 through with a notify_to naming a key that does not
	// exist, which is exactly the "rule with a way around it" the create
	// path already refuses (TestZoneNotifyToRejectsMalformedAndUnknownKeys).
	if badKey := srv.do(t, "PATCH", path, `{"notify_to":"10.0.0.2 key:nope"}`); badKey.Code != http.StatusBadRequest {
		t.Fatalf("PATCH with an unknown key: status %d, body %s", badKey.Code, badKey.Body)
	}

	if ok := srv.do(t, "PATCH", path, `{"notify_to":"  10.0.0.2  "}`); ok.Code != http.StatusNoContent {
		t.Fatalf("PATCH: status %d, body %s", ok.Code, ok.Body)
	}
	if got := srv.zone(t, id).NotifyTo; got != "10.0.0.2:53" {
		t.Errorf("notify_to = %q, want the canonical spelling", got)
	}

	// Clearing it back to empty must work — a zone that stops notifying is
	// an ordinary edit, and an empty string must not be read as "unset, keep
	// what was there".
	if cleared := srv.do(t, "PATCH", path, `{"notify_to":""}`); cleared.Code != http.StatusNoContent {
		t.Fatalf("clearing PATCH: status %d, body %s", cleared.Code, cleared.Body)
	}
	if got := srv.zone(t, id).NotifyTo; got != "" {
		t.Errorf("notify_to = %q after clearing, want empty", got)
	}
}

// fakeZoneRefresher stands in for zones.Refresher. The real one transfers by
// opening a TCP connection to a primary and speaking AXFR; what this handler
// owes is the plumbing around that — which zone it asks for, what it does
// with the answer, and what it does with the error — and none of that needs
// a primary to be standing up. internal/zones/refresh_test.go is where the
// transfer itself is tested.
type fakeZoneRefresher struct {
	mu     sync.Mutex
	calls  []int64
	result zones.TransferResult
	err    error
}

func (f *fakeZoneRefresher) Refresh(_ context.Context, zoneID int64) (zones.TransferResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, zoneID)
	return f.result, f.err
}

func (f *fakeZoneRefresher) called() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int64(nil), f.calls...)
}

// newRefreshTestServer is newTestServer with a ZoneRefresher wired in. The
// default harness deliberately leaves that dependency nil — see Deps.
func newRefreshTestServer(t *testing.T, fake *fakeZoneRefresher) *zoneTestServer {
	t.Helper()
	srv, s, rl := testServer(t, func(d *Deps) { d.ZoneRefresher = fake })
	return &zoneTestServer{srv: srv, store: s, rl: rl, cookie: login(t, srv, s)}
}

// createSecondary makes a secondary zone through the API and returns its id.
func createSecondary(t *testing.T, srv *zoneTestServer, name, primaries string) int64 {
	t.Helper()
	rec := srv.do(t, "POST", "/api/v1/zones",
		fmt.Sprintf(`{"name":%q,"type":"secondary","primaries":%q}`, name, primaries))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create secondary %q: status = %d body = %s", name, rec.Code, rec.Body)
	}
	return createdID(t, rec)
}

// The response is the outcome, not an acknowledgement — see
// handleZoneRefresh on why this endpoint is synchronous. `primary` and
// `records` are the two fields the zone row cannot supply afterwards, so
// they are the ones worth asserting on.
func TestZoneRefreshReportsTheTransferOutcome(t *testing.T) {
	fake := &fakeZoneRefresher{result: zones.TransferResult{
		Primary:     netip.MustParseAddrPort("203.0.113.9:53"),
		Serial:      2026080601,
		Records:     17,
		RefreshedAt: 1754000000000,
		ExpiresAt:   1754604800000,
	}}
	srv := newRefreshTestServer(t, fake)
	id := createSecondary(t, srv, "e412.in", "203.0.113.9")

	rec := srv.do(t, "POST", fmt.Sprintf("/api/v1/zones/%d/refresh", id), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s; want 200", rec.Code, rec.Body)
	}
	var got zoneRefreshResult
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal %s: %v", rec.Body, err)
	}
	want := zoneRefreshResult{
		Primary:     "203.0.113.9:53",
		Serial:      2026080601,
		Records:     17,
		RefreshedAt: 1754000000000,
		ExpiresAt:   1754604800000,
	}
	if got != want {
		t.Errorf("body = %+v, want %+v", got, want)
	}
	if calls := fake.called(); len(calls) != 1 || calls[0] != id {
		t.Errorf("refreshed %v, want exactly [%d]", calls, id)
	}
}

// The error text is the only account of a failed transfer the API ever gives
// — the scheduler's own record of one is process-local and does not survive a
// restart — so it goes through verbatim rather than being replaced with a
// generic message. 502 because nothing here is broken: a server this one
// depends on could not be reached.
func TestZoneRefreshPassesTheTransferErrorThrough(t *testing.T) {
	fake := &fakeZoneRefresher{err: errors.New(
		"203.0.113.9:53: dial tcp 203.0.113.9:53: connect: connection refused")}
	srv := newRefreshTestServer(t, fake)
	id := createSecondary(t, srv, "e412.in", "203.0.113.9")

	rec := srv.do(t, "POST", fmt.Sprintf("/api/v1/zones/%d/refresh", id), "")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d body = %s; want 502", rec.Code, rec.Body)
	}
	var got struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal %s: %v", rec.Body, err)
	}
	if got.Error != fake.err.Error() {
		t.Errorf("error = %q, want the transfer's own text %q", got.Error, fake.err)
	}
}

// A stub pulls from a master too — two ordinary queries for the apex SOA and
// NS instead of an AXFR — so the button means something for it, and the
// scheduler behind it has handled one since stub zones were put on the
// schedule.
//
// expires_at comes back 0 and that is the answer, not a missing value: a stub
// is never given an expiry (§9.11.8). A client must render it as "does not
// expire" rather than as an expiry at the epoch.
func TestZoneRefreshAcceptsAStubZone(t *testing.T) {
	fake := &fakeZoneRefresher{result: zones.TransferResult{
		Primary:     netip.MustParseAddrPort("203.0.113.9:53"),
		Serial:      2026080601,
		Records:     2,
		RefreshedAt: 1754000000000,
	}}
	srv := newRefreshTestServer(t, fake)
	rec := srv.do(t, "POST", "/api/v1/zones",
		`{"name":"ad.corp.example","type":"stub","primaries":"203.0.113.9"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create stub: status = %d body = %s", rec.Code, rec.Body)
	}
	id := createdID(t, rec)

	rec = srv.do(t, "POST", fmt.Sprintf("/api/v1/zones/%d/refresh", id), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s; want 200", rec.Code, rec.Body)
	}
	if calls := fake.called(); len(calls) != 1 || calls[0] != id {
		t.Fatalf("refreshed %v, want exactly [%d]", calls, id)
	}
	var got zoneRefreshResult
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal %s: %v", rec.Body, err)
	}
	if got.ExpiresAt != 0 {
		t.Errorf("expires_at = %d, want 0 — a stub does not expire", got.ExpiresAt)
	}
	if got.Serial != 2026080601 || got.Records != 2 {
		t.Errorf("body = %+v, want the fetch's own serial and rows", got)
	}
}

// The other side of the same boundary, which is what stops it from being
// widened to "any zone at all".
//
// A primary is authored here and a forwarder names its upstreams outright in
// forward_to: neither has a master, so refreshing one is a no-op wearing a
// button. Refused here rather than left to come back as a 502 from the
// fetcher's own type check, which would read as "the other server failed"
// about a fetch that was never attempted.
func TestZoneRefreshRefusesAZoneWithNoMaster(t *testing.T) {
	for _, tc := range []struct{ zoneType, body string }{
		{"primary", `{"name":"e412.in","type":"primary"}`},
		{"forwarder", `{"name":"corp.example","type":"forwarder","forward_to":"10.0.0.1"}`},
	} {
		t.Run(tc.zoneType, func(t *testing.T) {
			fake := &fakeZoneRefresher{}
			srv := newRefreshTestServer(t, fake)
			rec := srv.do(t, "POST", "/api/v1/zones", tc.body)
			if rec.Code != http.StatusCreated {
				t.Fatalf("create %s: status = %d body = %s", tc.zoneType, rec.Code, rec.Body)
			}
			id := createdID(t, rec)

			rec = srv.do(t, "POST", fmt.Sprintf("/api/v1/zones/%d/refresh", id), "")
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d body = %s; want 400", rec.Code, rec.Body)
			}
			if calls := fake.called(); len(calls) != 0 {
				t.Errorf("refreshed %v, want no attempt at all: a %s has no master to ask", calls, tc.zoneType)
			}
		})
	}
}

func TestZoneRefreshOnAnUnknownZoneIs404(t *testing.T) {
	fake := &fakeZoneRefresher{}
	srv := newRefreshTestServer(t, fake)

	if rec := srv.do(t, "POST", "/api/v1/zones/999999/refresh", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d body = %s; want 404", rec.Code, rec.Body)
	}
	if calls := fake.called(); len(calls) != 0 {
		t.Errorf("refreshed %v, want no transfer attempted at all", calls)
	}
}

// A server built without the scheduler (every test server, and any future
// mode that runs the API without the DNS side) answers rather than panicking
// on a nil dependency. 503, not 404: the zone exists, this server just is not
// the thing that transfers it.
func TestZoneRefreshWithoutASchedulerIsUnavailable(t *testing.T) {
	srv := newTestServer(t)
	id := createSecondary(t, srv, "e412.in", "203.0.113.9")

	rec := srv.do(t, "POST", fmt.Sprintf("/api/v1/zones/%d/refresh", id), "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d body = %s; want 503", rec.Code, rec.Body)
	}
}

func TestZoneCreateAcceptsForwarderAndStub(t *testing.T) {
	tests := []struct {
		name, body string
		check      func(t *testing.T, z store.Zone)
	}{
		{
			name: "forwarder with upstreams",
			body: `{"name":"corp.example","type":"forwarder","forward_to":"10.0.0.1, 10.0.0.2:5353"}`,
			check: func(t *testing.T, z store.Zone) {
				// Canonical, not as typed.
				if z.ForwardTo != "10.0.0.1:53, 10.0.0.2:5353" {
					t.Errorf("forward_to = %q, want the canonical spelling", z.ForwardTo)
				}
			},
		},
		{
			name: "stub with a master",
			body: `{"name":"ad.corp.example","type":"stub","primaries":"10.0.0.9"}`,
			check: func(t *testing.T, z store.Zone) {
				if z.Primaries != "10.0.0.9" {
					t.Errorf("primaries = %q, want it kept", z.Primaries)
				}
				if z.ForwardTo != "" {
					t.Errorf("forward_to = %q on a stub, want empty", z.ForwardTo)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ts := newTestServer(t)
			rec := ts.do(t, "POST", "/api/v1/zones", tc.body)
			if rec.Code != http.StatusCreated {
				t.Fatalf("status %d, body %s", rec.Code, rec.Body)
			}
			tc.check(t, ts.zone(t, createdID(t, rec)))
		})
	}
}

func TestZoneForwardToRefusedOnOtherTypes(t *testing.T) {
	for _, zoneType := range []string{"primary", "secondary", "stub"} {
		t.Run(zoneType, func(t *testing.T) {
			ts := newTestServer(t)
			body := `{"name":"x.example","type":"` + zoneType + `","forward_to":"10.0.0.1"`
			if zoneType == "secondary" || zoneType == "stub" {
				body += `,"primaries":"10.0.0.9"`
			}
			body += `}`
			rec := ts.do(t, "POST", "/api/v1/zones", body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status %d, body %s", rec.Code, rec.Body)
			}
			if !strings.Contains(rec.Body.String(), "forward_to") {
				t.Errorf("body %s does not name the offending field", rec.Body)
			}
		})
	}
}

// A stub may name a TSIG key: its SOA and NS queries are ordinary queries, and
// a master requiring TSIG on those would refuse the fetch outright.
func TestZoneStubAcceptsATSIGKey(t *testing.T) {
	ts := newTestServer(t)
	keyID, err := ts.store.TSIGKeys().Create(t.Context(), store.TSIGKey{
		Name: "stub-key.", Algorithm: "hmac-sha256.", Secret: "c2VjcmV0LXNlY3JldC1zZWNyZXQ=",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	rec := ts.do(t, "POST", "/api/v1/zones",
		`{"name":"ad.corp.example","type":"stub","primaries":"10.0.0.9","tsig_key_id":`+
			strconv.FormatInt(keyID, 10)+`}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body)
	}
	if z := ts.zone(t, createdID(t, rec)); z.TSIGKeyID != keyID {
		t.Errorf("tsig_key_id = %d, want %d", z.TSIGKeyID, keyID)
	}
}

// Neither new type serves a zone or has secondaries, so both transfer-facing
// fields stay refused.
func TestZoneTransferFieldsRefusedOnForwarderAndStub(t *testing.T) {
	cases := []struct{ zoneType, field, value string }{
		{"forwarder", "allow_transfer", "10.0.0.0/24"},
		{"forwarder", "notify_to", "10.0.0.2"},
		{"stub", "allow_transfer", "10.0.0.0/24"},
		{"stub", "notify_to", "10.0.0.2"},
	}
	for _, tc := range cases {
		t.Run(tc.zoneType+"/"+tc.field, func(t *testing.T) {
			ts := newTestServer(t)
			body := `{"name":"x.example","type":"` + tc.zoneType + `","` + tc.field + `":"` + tc.value + `"`
			if tc.zoneType == "stub" {
				body += `,"primaries":"10.0.0.9"`
			}
			body += `}`
			rec := ts.do(t, "POST", "/api/v1/zones", body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status %d, body %s", rec.Code, rec.Body)
			}
		})
	}
}

// A rule enforced on POST and not on PATCH is a rule with a way around it.
func TestZonePatchValidatesForwardTo(t *testing.T) {
	ts := newTestServer(t)
	rec := ts.do(t, "POST", "/api/v1/zones", `{"name":"corp.example","type":"forwarder","forward_to":"10.0.0.1"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %s", rec.Body)
	}
	id := strconv.FormatInt(createdID(t, rec), 10)

	if bad := ts.do(t, "PATCH", "/api/v1/zones/"+id, `{"forward_to":"10.0.0.1:0"}`); bad.Code != http.StatusBadRequest {
		t.Fatalf("PATCH with a bad port: status %d, body %s", bad.Code, bad.Body)
	}
	if ok := ts.do(t, "PATCH", "/api/v1/zones/"+id, `{"forward_to":"  10.0.0.7  "}`); ok.Code != http.StatusNoContent {
		t.Fatalf("PATCH: status %d, body %s", ok.Code, ok.Body)
	}
	if z := ts.zone(t, createdID(t, rec)); z.ForwardTo != "10.0.0.7:53" {
		t.Errorf("forward_to = %q, want the canonical spelling", z.ForwardTo)
	}
	// Clearing back to empty is an ordinary edit, and must not be read as
	// "absent, keep what was there".
	if cleared := ts.do(t, "PATCH", "/api/v1/zones/"+id, `{"forward_to":""}`); cleared.Code != http.StatusNoContent {
		t.Fatalf("clearing PATCH: status %d, body %s", cleared.Code, cleared.Body)
	}
	if z := ts.zone(t, createdID(t, rec)); z.ForwardTo != "" {
		t.Errorf("forward_to = %q after clearing, want empty", z.ForwardTo)
	}
}

// PATCH forward_to onto a zone that is not a forwarder. Only
// checkZoneTransferConfig refuses this — canonicalForwardTo parses the
// value happily, because the value itself is well-formed. So this is the
// case that fails if the type gate is removed or reordered on the patch
// path, and the malformed-port case above is not.
func TestZonePatchRefusesForwardToOnAnotherType(t *testing.T) {
	ts := newTestServer(t)
	rec := ts.do(t, "POST", "/api/v1/zones", `{"name":"ad.corp.example","type":"stub","primaries":"10.0.0.9"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %s", rec.Body)
	}
	id := strconv.FormatInt(createdID(t, rec), 10)

	if bad := ts.do(t, "PATCH", "/api/v1/zones/"+id, `{"forward_to":"10.0.0.1"}`); bad.Code != http.StatusBadRequest {
		t.Fatalf("PATCH forward_to onto a stub: status %d, body %s", bad.Code, bad.Body)
	}
}

// A second shape of the same gap: patching a forwarder's type to stub
// without touching forward_to in the same request leaves the merged zone —
// the value checkZoneTransferConfig actually validates — with forward_to
// still set from before. Refused for the same reason as the case above: the
// stored value was well-formed when it was written, so only the type gate
// catches this, not a re-parse.
func TestZonePatchRefusesTypeChangeThatLeavesForwardToStale(t *testing.T) {
	ts := newTestServer(t)
	rec := ts.do(t, "POST", "/api/v1/zones", `{"name":"corp.example","type":"forwarder","forward_to":"10.0.0.1"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %s", rec.Body)
	}
	id := strconv.FormatInt(createdID(t, rec), 10)

	if bad := ts.do(t, "PATCH", "/api/v1/zones/"+id, `{"type":"stub","primaries":"10.0.0.9"}`); bad.Code != http.StatusBadRequest {
		t.Fatalf("PATCH type to stub without clearing forward_to: status %d, body %s", bad.Code, bad.Body)
	}
}
