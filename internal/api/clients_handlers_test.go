package api

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/aloks98/dnsaur/internal/store"
)

func TestGroupsCRUD(t *testing.T) {
	srv, s, rl := testServer(t)
	cookie := login(t, srv, s)
	h := srv.Handler()
	_, _ = s.Clients().AddGroup(t.Context(), "default") // ensure group 1 exists in fresh store

	w := doReq(t, h, "POST", "/api/v1/groups", `{"name":"kids"}`, cookie)
	if w.Code != 201 {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	var created map[string]int64
	_ = json.Unmarshal(w.Body.Bytes(), &created)
	gid := created["id"]

	w = doReq(t, h, "GET", "/api/v1/groups", "", cookie)
	var groups []store.Group
	_ = json.Unmarshal(w.Body.Bytes(), &groups)
	if len(groups) < 1 {
		t.Fatalf("list: %v", groups)
	}
	if w := doReq(t, h, "PATCH", fmt.Sprintf("/api/v1/groups/%d", gid), `{"name":"kids2","enabled":false}`, cookie); w.Code != 204 {
		t.Fatalf("patch: %d %s", w.Code, w.Body.String())
	}
	if w := doReq(t, h, "PATCH", fmt.Sprintf("/api/v1/groups/%d", gid), `{"name":""}`, cookie); w.Code != 400 {
		t.Fatalf("empty name accepted: %d", w.Code)
	}
	w = doReq(t, h, "GET", "/api/v1/groups", "", cookie)
	var groupsList []store.Group
	_ = json.Unmarshal(w.Body.Bytes(), &groupsList)
	var found bool
	for _, g := range groupsList {
		if g.ID == gid {
			if g.Name != "kids2" {
				t.Fatalf("name changed despite rejection: %s", g.Name)
			}
			found = true
			break
		}
	}
	if !found {
		t.Fatal("group not found after empty name rejection")
	}
	if w := doReq(t, h, "DELETE", fmt.Sprintf("/api/v1/groups/%d", gid), "", cookie); w.Code != 204 {
		t.Fatalf("delete: %d", w.Code)
	}
	if w := doReq(t, h, "DELETE", "/api/v1/groups/1", "", cookie); w.Code != 409 {
		t.Fatalf("default group delete: %d", w.Code)
	}
	if clients, _, _ := rl.counts(); clients == 0 {
		t.Fatal("mutations did not trigger ReloadClients")
	}
}

func TestClientsCRUDAndValidation(t *testing.T) {
	srv, s, rl := testServer(t)
	cookie := login(t, srv, s)
	h := srv.Handler()
	_, _ = s.Clients().AddGroup(t.Context(), "default") // ensure group 1 exists in fresh store

	if w := doReq(t, h, "POST", "/api/v1/clients", `{"name":"tv","matcher":"not-an-ip","group_id":1}`, cookie); w.Code != 400 {
		t.Fatalf("bad matcher accepted: %d", w.Code)
	}
	w := doReq(t, h, "POST", "/api/v1/clients", `{"name":"tv","matcher":"10.0.0.7","group_id":1}`, cookie)
	if w.Code != 201 {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	var created map[string]int64
	_ = json.Unmarshal(w.Body.Bytes(), &created)
	cid := created["id"]
	if w := doReq(t, h, "PUT", fmt.Sprintf("/api/v1/clients/%d", cid), `{"name":"tv2","matcher":"10.0.0.0/24","group_id":1}`, cookie); w.Code != 204 {
		t.Fatalf("update: %d %s", w.Code, w.Body.String())
	}
	if w := doReq(t, h, "DELETE", fmt.Sprintf("/api/v1/clients/%d", cid), "", cookie); w.Code != 204 {
		t.Fatalf("delete: %d", w.Code)
	}
	if w := doReq(t, h, "DELETE", fmt.Sprintf("/api/v1/clients/%d", cid), "", cookie); w.Code != 404 {
		t.Fatalf("double delete: %d", w.Code)
	}
	if clients, _, _ := rl.counts(); clients < 3 {
		t.Fatalf("reload count: %d", clients)
	}
}
