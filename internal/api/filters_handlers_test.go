package api

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aloks98/dnsaur/internal/store"
)

func TestListsCRUDAndAssignment(t *testing.T) {
	srv, s, rl := testServer(t)
	cookie := login(t, srv, s)
	h := srv.Handler()
	gid, _ := s.Clients().AddGroup(t.Context(), "g")

	if w := doReq(t, h, "POST", "/api/v1/filters/lists", `{"url":"ftp://bad","kind":"block"}`, cookie); w.Code != 400 {
		t.Fatalf("bad url accepted: %d", w.Code)
	}
	w := doReq(t, h, "POST", "/api/v1/filters/lists", `{"url":"https://x.example/hosts","kind":"block"}`, cookie)
	if w.Code != 201 {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	var created map[string]int64
	_ = json.Unmarshal(w.Body.Bytes(), &created)
	lid := created["id"]

	if w := doReq(t, h, "PUT", fmt.Sprintf("/api/v1/groups/%d/lists", gid), fmt.Sprintf(`{"list_ids":[%d]}`, lid), cookie); w.Code != 204 {
		t.Fatalf("assign: %d %s", w.Code, w.Body.String())
	}
	w = doReq(t, h, "GET", fmt.Sprintf("/api/v1/groups/%d/lists", gid), "", cookie)
	var ls []store.List
	_ = json.Unmarshal(w.Body.Bytes(), &ls)
	if len(ls) != 1 || ls[0].ID != lid {
		t.Fatalf("group lists: %v", ls)
	}
	if w := doReq(t, h, "PUT", fmt.Sprintf("/api/v1/groups/%d/lists", gid), `{"list_ids":[]}`, cookie); w.Code != 204 {
		t.Fatalf("clear assignment: %d", w.Code)
	}
	if w := doReq(t, h, "PATCH", fmt.Sprintf("/api/v1/filters/lists/%d", lid), `{"enabled":false}`, cookie); w.Code != 204 {
		t.Fatalf("patch: %d", w.Code)
	}
	if w := doReq(t, h, "DELETE", fmt.Sprintf("/api/v1/filters/lists/%d", lid), "", cookie); w.Code != 204 {
		t.Fatalf("delete: %d", w.Code)
	}
	// DELETE above refreshes synchronously, so the count is already >0 by
	// now regardless of whether the list-create's background refresh (Fix
	// 4: handleListCreate refreshes asynchronously) has finished yet.
	if _, _, filters := rl.counts(); filters == 0 {
		t.Fatal("filter mutations did not refresh")
	}
}

// TestListCreateRefreshesAsynchronously asserts POST /filters/lists returns
// 201 without waiting for the (potentially slow, full-network) filter
// refresh, and that the refresh still eventually happens in the background.
func TestListCreateRefreshesAsynchronously(t *testing.T) {
	srv, s, rl := testServer(t)
	cookie := login(t, srv, s)
	h := srv.Handler()

	w := doReq(t, h, "POST", "/api/v1/filters/lists", `{"url":"https://x.example/hosts","kind":"block"}`, cookie)
	if w.Code != 201 {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	var created map[string]int64
	_ = json.Unmarshal(w.Body.Bytes(), &created)
	if created["id"] == 0 {
		t.Fatalf("no id in response: %s", w.Body.String())
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, _, filters := rl.counts(); filters > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("background refresh after list create never ran")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestRules(t *testing.T) {
	srv, s, _ := testServer(t)
	cookie := login(t, srv, s)
	h := srv.Handler()
	gid, _ := s.Clients().AddGroup(t.Context(), "gr")

	if w := doReq(t, h, "POST", fmt.Sprintf("/api/v1/groups/%d/rules", gid), `{"action":"block","pattern":"([","is_regex":true}`, cookie); w.Code != 400 {
		t.Fatalf("bad regex accepted: %d", w.Code)
	}
	w := doReq(t, h, "POST", fmt.Sprintf("/api/v1/groups/%d/rules", gid), `{"action":"allow","pattern":"ok.example"}`, cookie)
	if w.Code != 201 {
		t.Fatalf("create rule: %d %s", w.Code, w.Body.String())
	}
	var created map[string]int64
	_ = json.Unmarshal(w.Body.Bytes(), &created)
	w = doReq(t, h, "GET", fmt.Sprintf("/api/v1/groups/%d/rules", gid), "", cookie)
	var rs []store.Rule
	_ = json.Unmarshal(w.Body.Bytes(), &rs)
	if len(rs) != 1 || rs[0].Action != "allow" {
		t.Fatalf("rules: %v", rs)
	}
	if w := doReq(t, h, "POST", fmt.Sprintf("/api/v1/groups/%d/rules", gid), fmt.Sprintf(`{"action":"block","pattern":"%s","is_regex":true}`, strings.Repeat("a", 600)), cookie); w.Code != 400 {
		t.Fatalf("long regex pattern accepted: %d", w.Code)
	}
	if w := doReq(t, h, "DELETE", fmt.Sprintf("/api/v1/filters/rules/%d", created["id"]), "", cookie); w.Code != 204 {
		t.Fatalf("delete rule: %d", w.Code)
	}
}

// A list assigned to no group filters nothing: a group's ruleset is compiled
// only from its assigned lists (internal/filter/refresh.go's ListsForGroup).
// Subscribing to a blocklist and having it block zero queries — while the UI
// reports it enabled with a six-figure entry count — is the failure this
// guards, in both directions: a list added after a group, and a group added
// after a list.
func TestNewListsAndGroupsInheritEachOther(t *testing.T) {
	srv, s, _ := testServer(t)
	cookie := login(t, srv, s)
	h := srv.Handler()

	existing, _ := s.Clients().AddGroup(t.Context(), "before")

	w := doReq(t, h, "POST", "/api/v1/filters/lists", `{"url":"https://x.example/block.txt","kind":"block"}`, cookie)
	if w.Code != 201 {
		t.Fatalf("create list: %d %s", w.Code, w.Body.String())
	}
	var created map[string]int64
	_ = json.Unmarshal(w.Body.Bytes(), &created)
	lid := created["id"]

	assigned := func(gid int64) []store.List {
		t.Helper()
		r := doReq(t, h, "GET", fmt.Sprintf("/api/v1/groups/%d/lists", gid), "", cookie)
		var ls []store.List
		_ = json.Unmarshal(r.Body.Bytes(), &ls)
		return ls
	}

	if ls := assigned(existing); len(ls) != 1 || ls[0].ID != lid {
		t.Fatalf("a group that predates the list did not get it: %v", ls)
	}

	w = doReq(t, h, "POST", "/api/v1/groups", `{"name":"after"}`, cookie)
	if w.Code != 201 {
		t.Fatalf("create group: %d %s", w.Code, w.Body.String())
	}
	var g map[string]int64
	_ = json.Unmarshal(w.Body.Bytes(), &g)

	if ls := assigned(g["id"]); len(ls) != 1 || ls[0].ID != lid {
		t.Fatalf("a group created after the list did not inherit it: %v", ls)
	}
}
