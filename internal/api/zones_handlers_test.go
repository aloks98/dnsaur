package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aloks98/dnsaur/internal/store"
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

// Milestone A ships primary zones only. internal/zones/answer.go only
// special-cases "forwarder"/"stub" as non-answering, so a "secondary" zone
// would otherwise be served exactly like a primary — except a secondary has
// no transfer mechanism until Milestone D, so it holds only its apex NS
// record and every other name under it comes back an authoritative
// NXDOMAIN, silently taking a domain offline. Any type other than the
// primary default must 400, not just nonsense values.
func TestZoneCreateRejectsNonPrimaryType(t *testing.T) {
	srv := newTestServer(t)
	for _, zt := range []string{"secondary", "banana"} {
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
		if len(zs) != 1 || zs[0].ID != got.ID {
			t.Fatalf("list = %+v, want one zone with id %d", zs, got.ID)
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
}

func TestZonePatchRejectsNonPrimaryType(t *testing.T) {
	srv := newTestServer(t)
	rec := srv.do(t, "POST", "/api/v1/zones", `{"name":"e412.in"}`)
	var got struct{ ID int64 }
	_ = json.Unmarshal(rec.Body.Bytes(), &got)

	path := fmt.Sprintf("/api/v1/zones/%d", got.ID)
	if rec := srv.do(t, "PATCH", path, `{"type":"secondary"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if z := srv.zone(t, got.ID); z.Type != "primary" {
		t.Fatalf("type changed despite rejection: %q", z.Type)
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
