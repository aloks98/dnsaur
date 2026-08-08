package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAPITokenLifecycle(t *testing.T) {
	srv, s, _ := testServer(t)
	cookie := login(t, srv, s)
	h := srv.Handler()

	w := doReq(t, h, "POST", "/api/v1/tokens", `{"name":"homeassistant"}`, cookie)
	if w.Code != 201 {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	var created map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &created)
	plain, _ := created["token"].(string)
	if len(plain) < 40 {
		t.Fatalf("no plain token returned: %v", created)
	}
	// read-scope token: GET allowed, mutation forbidden
	w = doReq(t, h, "POST", "/api/v1/tokens", `{"name":"ro","scope":"read"}`, cookie)
	var roCreated map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &roCreated)
	ro, _ := roCreated["token"].(string)
	// Any mutating, requireAuth-wrapped route works as the scope probe here;
	// POST /api/v1/zones is used since /api/v1/records (Task 8) no longer
	// exists.
	req0 := httptest.NewRequest("POST", "/api/v1/zones", stringsReader(`{"name":"ro-probe.test"}`))
	req0.Header.Set("Authorization", "Bearer "+ro)
	req0.Header.Set("Content-Type", "application/json")
	rec0 := httptest.NewRecorder()
	h.ServeHTTP(rec0, req0)
	if rec0.Code != http.StatusForbidden {
		t.Fatalf("read token allowed mutation: %d", rec0.Code)
	}
	req := httptest.NewRequest("GET", "/api/v1/tokens", nil)
	req.Header.Set("Authorization", "Bearer "+plain)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("bearer list: %d", rec.Code)
	}
	var list []map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	if len(list) < 1 {
		t.Fatalf("list empty: %v", list)
	}
	// Find the homeassistant token in the list
	found := false
	for _, token := range list {
		if token["name"] == "homeassistant" {
			found = true
			if _, hashLeaked := token["token_hash"]; hashLeaked {
				t.Fatal("token hash serialized")
			}
			break
		}
	}
	if !found {
		t.Fatalf("homeassistant token not found in list: %v", list)
	}
	id := int64(list[0]["id"].(float64))
	if w := doReq(t, h, "DELETE", fmt.Sprintf("/api/v1/tokens/%d", id), "", cookie); w.Code != 204 {
		t.Fatalf("revoke: %d", w.Code)
	}
	req = httptest.NewRequest("GET", "/api/v1/tokens", nil)
	req.Header.Set("Authorization", "Bearer "+plain)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("revoked bearer still works: %d", rec.Code)
	}
}
