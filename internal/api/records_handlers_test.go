package api

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/aloks98/dnsaur/internal/store"
)

func TestRecordsCRUD(t *testing.T) {
	srv, s, rl := testServer(t)
	cookie := login(t, srv, s)
	h := srv.Handler()

	bad := []string{
		`{"name":"nas.home.lan","type":"A","value":"not-an-ip","ttl":300}`,
		`{"name":"nas.home.lan","type":"AAAA","value":"10.0.0.9","ttl":300}`,
		`{"name":"nas.home.lan","type":"MX","value":"x","ttl":300}`,
		`{"name":"","type":"A","value":"10.0.0.9","ttl":300}`,
		`{"name":"nas.home.lan","type":"A","value":"10.0.0.9","ttl":0}`,
	}
	for _, b := range bad {
		if w := doReq(t, h, "POST", "/api/v1/records", b, cookie); w.Code != 400 {
			t.Fatalf("accepted invalid record %s: %d", b, w.Code)
		}
	}
	w := doReq(t, h, "POST", "/api/v1/records", `{"name":"*.Apps.Home.Lan.","type":"A","value":"10.0.0.10","ttl":60}`, cookie)
	if w.Code != 201 {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	var created map[string]int64
	_ = json.Unmarshal(w.Body.Bytes(), &created)
	all, _ := s.Records().All(t.Context())
	if all[0].Name != "*.apps.home.lan" {
		t.Fatalf("name not normalized: %q", all[0].Name)
	}
	if w := doReq(t, h, "PUT", fmt.Sprintf("/api/v1/records/%d", created["id"]), `{"name":"*.apps.home.lan","type":"A","value":"10.0.0.11","ttl":60}`, cookie); w.Code != 204 {
		t.Fatalf("update: %d %s", w.Code, w.Body.String())
	}
	if w := doReq(t, h, "DELETE", fmt.Sprintf("/api/v1/records/%d", created["id"]), "", cookie); w.Code != 204 {
		t.Fatalf("delete: %d", w.Code)
	}
	if _, records, _ := rl.counts(); records < 3 {
		t.Fatalf("reload count: %d", records)
	}
	_ = store.LocalRecord{}
}
