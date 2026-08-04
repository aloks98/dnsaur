package api

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/aloks98/dnsaur/internal/filter"
)

func TestSettingsGetPut(t *testing.T) {
	srv, s, _ := testServer(t)
	cookie := login(t, srv, s)
	h := srv.Handler()
	_ = s.Settings().SetInternal(t.Context(), "blocking.mode", "null-ip")
	_ = s.Settings().SetInternal(t.Context(), "instance.id", "secret-id")

	w := doReq(t, h, "GET", "/api/v1/settings", "", cookie)
	var m map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &m)
	if w.Code != 200 || m["blocking.mode"] != "null-ip" {
		t.Fatalf("get: %d %v", w.Code, m)
	}
	if _, leaked := m["instance.id"]; leaked {
		t.Fatal("internal key leaked")
	}
	if w := doReq(t, h, "PUT", "/api/v1/settings", `{"key":"blocking.mode","value":"nxdomain"}`, cookie); w.Code != 204 {
		t.Fatalf("put: %d %s", w.Code, w.Body.String())
	}
	if v, _, _ := s.Settings().Get(t.Context(), "blocking.mode"); v != "nxdomain" {
		t.Fatalf("not persisted: %s", v)
	}
	if w := doReq(t, h, "PUT", "/api/v1/settings", `{"key":"blocking.mode","value":"weird"}`, cookie); w.Code != 400 {
		t.Fatalf("bad enum accepted: %d", w.Code)
	}
	if w := doReq(t, h, "PUT", "/api/v1/settings", `{"key":"instance.id","value":"x"}`, cookie); w.Code != 400 {
		t.Fatalf("non-allowlisted key accepted: %d", w.Code)
	}
	if w := doReq(t, h, "PUT", "/api/v1/settings", `{"key":"cache.max_entries","value":"-3"}`, cookie); w.Code != 400 {
		t.Fatalf("negative numeric accepted: %d", w.Code)
	}
}

func TestBlockingPauseResume(t *testing.T) {
	srv, s, _ := testServer(t)
	srv.deps.Engine = filter.NewEngine()
	cookie := login(t, srv, s)
	h := srv.Handler()

	if w := doReq(t, h, "POST", "/api/v1/blocking/pause", `{"group_id":0,"minutes":5}`, cookie); w.Code != 204 {
		t.Fatalf("pause: %d %s", w.Code, w.Body.String())
	}
	w := doReq(t, h, "GET", "/api/v1/blocking", "", cookie)
	var st map[string]int64
	_ = json.Unmarshal(w.Body.Bytes(), &st)
	if st["paused_until"] <= time.Now().UnixMilli() {
		t.Fatalf("paused_until: %v", st)
	}
	if w := doReq(t, h, "DELETE", "/api/v1/blocking/pause?group_id=0", "", cookie); w.Code != 204 {
		t.Fatalf("resume: %d", w.Code)
	}
	w = doReq(t, h, "GET", "/api/v1/blocking", "", cookie)
	_ = json.Unmarshal(w.Body.Bytes(), &st)
	if st["paused_until"] != 0 {
		t.Fatalf("still paused: %v", st)
	}
	if w := doReq(t, h, "POST", "/api/v1/blocking/pause", `{"group_id":0,"minutes":0}`, cookie); w.Code != 400 {
		t.Fatalf("zero minutes accepted: %d", w.Code)
	}
}
