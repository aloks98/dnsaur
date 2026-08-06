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

// TestListEndpointsSerializeFetchStatus: the dashboard can only report a
// failed fetch if the API actually ships the state. Both list endpoints —
// Filtering › Lists reads one, Groups & Clients reads the other — must carry
// last_status/last_error/last_attempt, and a brand-new subscription must
// arrive as "pending" rather than as a bare `0 / 0` that the UI would have
// to guess about.
func TestListEndpointsSerializeFetchStatus(t *testing.T) {
	srv, s, _ := testServer(t)
	cookie := login(t, srv, s)
	h := srv.Handler()
	gid, _ := s.Clients().AddGroup(t.Context(), "g")

	w := doReq(t, h, "POST", "/api/v1/filters/lists", `{"url":"https://x.example/404.txt","kind":"block"}`, cookie)
	if w.Code != 201 {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	var created map[string]int64
	_ = json.Unmarshal(w.Body.Bytes(), &created)
	lid := created["id"]
	if w := doReq(t, h, "PUT", fmt.Sprintf("/api/v1/groups/%d/lists", gid), fmt.Sprintf(`{"list_ids":[%d]}`, lid), cookie); w.Code != 204 {
		t.Fatalf("assign: %d", w.Code)
	}

	// The raw JSON, not the decoded struct: a missing json tag would still
	// unmarshal into a zero-valued field and pass a typed assertion.
	body := doReq(t, h, "GET", "/api/v1/filters/lists", "", cookie).Body.String()
	for _, field := range []string{`"last_status"`, `"last_error"`, `"last_attempt"`} {
		if !strings.Contains(body, field) {
			t.Fatalf("GET /filters/lists omits %s: %s", field, body)
		}
	}
	var ls []store.List
	if err := json.Unmarshal([]byte(body), &ls); err != nil {
		t.Fatal(err)
	}
	if len(ls) != 1 || ls[0].LastStatus != store.ListStatusPending {
		t.Fatalf("a never-fetched list must read pending: %+v", ls)
	}

	// Now record a real failure and confirm it reaches both endpoints.
	if err := s.Filters().MarkListFailed(t.Context(), lid, 1700000000000, 0, "404 Not Found"); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/v1/filters/lists", fmt.Sprintf("/api/v1/groups/%d/lists", gid)} {
		var got []store.List
		r := doReq(t, h, "GET", path, "", cookie)
		if err := json.Unmarshal(r.Body.Bytes(), &got); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if len(got) != 1 {
			t.Fatalf("%s returned %d lists", path, len(got))
		}
		if got[0].LastStatus != store.ListStatusFailed || got[0].LastError != "404 Not Found" || got[0].LastAttempt != 1700000000000 {
			t.Fatalf("%s did not surface the failure: %+v", path, got[0])
		}
	}
}

// TestListNamingOverTheAPI: the name is what the UI shows instead of a raw
// URL, so it has to be settable at create, renameable afterwards, and never
// absent. The enabled-only PATCH — what the row's toggle sends, and what the
// contract documents — must keep working untouched.
func TestListNamingOverTheAPI(t *testing.T) {
	srv, s, _ := testServer(t)
	cookie := login(t, srv, s)
	h := srv.Handler()

	create := func(payload string) int64 {
		t.Helper()
		w := doReq(t, h, "POST", "/api/v1/filters/lists", payload, cookie)
		if w.Code != 201 {
			t.Fatalf("create %s: %d %s", payload, w.Code, w.Body.String())
		}
		var created map[string]int64
		_ = json.Unmarshal(w.Body.Bytes(), &created)
		return created["id"]
	}
	nameOf := func(id int64) string {
		t.Helper()
		var ls []store.List
		_ = json.Unmarshal(doReq(t, h, "GET", "/api/v1/filters/lists", "", cookie).Body.Bytes(), &ls)
		for _, l := range ls {
			if l.ID == id {
				return l.Name
			}
		}
		t.Fatalf("list %d missing", id)
		return ""
	}

	named := create(`{"url":"https://x.example/a.txt","kind":"block","name":"Household baseline"}`)
	if got := nameOf(named); got != "Household baseline" {
		t.Fatalf("explicit name = %q", got)
	}

	// Omitted entirely: derived, never blank.
	derived := create(`{"url":"https://raw.githubusercontent.com/StevenBlack/hosts/master/hosts","kind":"block"}`)
	if got := nameOf(derived); got != "StevenBlack hosts" {
		t.Fatalf("derived name = %q, want \"StevenBlack hosts\"", got)
	}
	// Supplied but blank: same.
	blank := create(`{"url":"https://raw.githubusercontent.com/hagezi/dns-blocklists/main/wildcard/pro.txt","kind":"block","name":"   "}`)
	if got := nameOf(blank); got != "hagezi wildcard/pro.txt" {
		t.Fatalf("blank name = %q, want the derivation", got)
	}

	// Rename.
	if w := doReq(t, h, "PATCH", fmt.Sprintf("/api/v1/filters/lists/%d", derived), `{"name":"Steven's list"}`, cookie); w.Code != 204 {
		t.Fatalf("rename: %d %s", w.Code, w.Body.String())
	}
	if got := nameOf(derived); got != "Steven's list" {
		t.Fatalf("after rename = %q", got)
	}
	// Blank rename resets to the derived default rather than emptying it.
	if w := doReq(t, h, "PATCH", fmt.Sprintf("/api/v1/filters/lists/%d", derived), `{"name":""}`, cookie); w.Code != 204 {
		t.Fatalf("reset: %d", w.Code)
	}
	if got := nameOf(derived); got != "StevenBlack hosts" {
		t.Fatalf("blank rename = %q, want the derived default back", got)
	}

	// The pre-existing shape must be untouched: enabled-only still 204s and
	// still toggles, without clobbering the name.
	if w := doReq(t, h, "PATCH", fmt.Sprintf("/api/v1/filters/lists/%d", named), `{"enabled":false}`, cookie); w.Code != 204 {
		t.Fatalf("enabled-only patch: %d %s", w.Code, w.Body.String())
	}
	if got := nameOf(named); got != "Household baseline" {
		t.Fatalf("enabled-only patch clobbered the name: %q", got)
	}
	// An empty patch is still a 400 — it just says so about both fields now.
	if w := doReq(t, h, "PATCH", fmt.Sprintf("/api/v1/filters/lists/%d", named), `{}`, cookie); w.Code != 400 {
		t.Fatalf("empty patch: %d", w.Code)
	}
	// url/kind stay immutable; DisallowUnknownFields rejects them outright.
	if w := doReq(t, h, "PATCH", fmt.Sprintf("/api/v1/filters/lists/%d", named), `{"url":"https://evil.example/x"}`, cookie); w.Code != 400 {
		t.Fatalf("url patch must be rejected: %d %s", w.Code, w.Body.String())
	}
	if w := doReq(t, h, "PATCH", fmt.Sprintf("/api/v1/filters/lists/%d", named), `{"name":"`+strings.Repeat("x", 200)+`"}`, cookie); w.Code != 400 {
		t.Fatalf("overlong name: %d", w.Code)
	}
	if w := doReq(t, h, "PATCH", "/api/v1/filters/lists/999999", `{"name":"ghost"}`, cookie); w.Code != 404 {
		t.Fatalf("renaming a missing list: %d", w.Code)
	}
}
