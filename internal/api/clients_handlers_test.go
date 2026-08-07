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

// POST /groups accepts the two fields the UI's add row now sets. Both are
// optional, and their defaults are the ones that keep a new group useful:
// enabled, carrying every list. An explicitly empty list_ids is the one way
// to say "assign nothing", and has to survive being distinguished from
// omitting the field entirely.
func TestGroupCreateEnabledAndLists(t *testing.T) {
	srv, s, _ := testServer(t)
	cookie := login(t, srv, s)
	h := srv.Handler()

	lid, err := s.Filters().AddList(t.Context(), store.List{
		URL: "https://example.com/a", Kind: "block", Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	create := func(body string) int64 {
		t.Helper()
		w := doReq(t, h, "POST", "/api/v1/groups", body, cookie)
		if w.Code != 201 {
			t.Fatalf("create %s: %d %s", body, w.Code, w.Body.String())
		}
		var created map[string]int64
		_ = json.Unmarshal(w.Body.Bytes(), &created)
		return created["id"]
	}
	assigned := func(gid int64) int {
		t.Helper()
		ls, err := s.Filters().ListsForGroup(t.Context(), gid)
		if err != nil {
			t.Fatal(err)
		}
		return len(ls)
	}
	enabled := func(gid int64) bool {
		t.Helper()
		gs, err := s.Clients().Groups(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		for _, g := range gs {
			if g.ID == gid {
				return g.Enabled
			}
		}
		t.Fatalf("group %d not found", gid)
		return false
	}

	// Omitted: enabled, and inherits the one list that exists.
	bare := create(`{"name":"bare"}`)
	if !enabled(bare) {
		t.Error("a group created without `enabled` should be enabled")
	}
	if n := assigned(bare); n != 1 {
		t.Errorf("omitted list_ids should inherit every list, got %d", n)
	}

	// Explicitly disabled.
	off := create(`{"name":"off","enabled":false}`)
	if enabled(off) {
		t.Error(`"enabled":false was ignored`)
	}

	// An explicit empty array is "no lists" — not "inherit everything".
	none := create(`{"name":"none","list_ids":[]}`)
	if n := assigned(none); n != 0 {
		t.Errorf("empty list_ids should assign nothing, got %d", n)
	}

	// An explicit set is used verbatim.
	one := create(fmt.Sprintf(`{"name":"one","list_ids":[%d]}`, lid))
	if n := assigned(one); n != 1 {
		t.Errorf("explicit list_ids should assign exactly those, got %d", n)
	}
}
