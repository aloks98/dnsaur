package api

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aloks98/dnsaur/internal/store"
)

// touchingReloader is fakeReloader with a RefreshList that actually records
// the list's new state, the way filter.Refresher does. Counting the call
// alone would let the handler read the row *before* the refresh and never
// be caught: the response is supposed to be the list as the refresh left
// it.
type touchingReloader struct {
	*fakeReloader
	fs store.FilterStore
	at int64
	n  int64
}

func (t *touchingReloader) RefreshList(ctx context.Context, id int64) error {
	if err := t.fakeReloader.RefreshList(ctx, id); err != nil {
		return err
	}
	return t.fs.TouchList(ctx, id, t.at, t.n)
}

// TestListRefreshRefreshesOneListAndReturnsItsRow is the row's own "Refresh
// now": one list is downloaded and the updated row comes straight back, so
// the dashboard does not have to poll the table to find out what happened
// the way it does after the all-lists 202.
func TestListRefreshRefreshesOneListAndReturnsItsRow(t *testing.T) {
	srv, s, rl := testServer(t)
	cookie := login(t, srv, s)
	h := srv.Handler()
	lid, err := s.Filters().AddList(t.Context(), store.List{URL: "https://x.example/hosts", Kind: "block", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.Filters().AddList(t.Context(), store.List{URL: "https://y.example/hosts", Kind: "block", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	tr := &touchingReloader{fakeReloader: rl, fs: s.Filters(), at: 1_700_000_000_000, n: 99_277}
	srv.deps.Reloader = tr
	rl.nextRefreshAt = 1_700_000_600_000

	w := doReq(t, h, "POST", fmt.Sprintf("/api/v1/filters/lists/%d/refresh", lid), "", cookie)
	if w.Code != 202 {
		t.Fatalf("refresh: %d %s", w.Code, w.Body.String())
	}
	var row struct {
		store.List
		NextRefreshAt int64 `json:"next_refresh_at"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &row); err != nil {
		t.Fatal(err)
	}
	if row.ID != lid || row.LastStatus != store.ListStatusOK || row.EntryCount != 99_277 {
		t.Fatalf("row = %+v; want list %d as the refresh left it", row.List, lid)
	}
	if row.LastAttempt != 1_700_000_000_000 {
		t.Fatalf("last_attempt = %d; the row was read before the refresh", row.LastAttempt)
	}
	if row.NextRefreshAt != 1_700_000_600_000 {
		t.Fatalf("next_refresh_at = %d, want the refresher's next tick", row.NextRefreshAt)
	}
	if got := rl.refreshedLists(); len(got) != 1 || got[0] != lid {
		t.Fatalf("refreshed %v; want only list %d", got, lid)
	}
	if _, _, filters := rl.counts(); filters != 0 {
		t.Fatalf("a per-list refresh also ran the all-lists download %d times", filters)
	}
	ls, _ := s.Filters().Lists(t.Context())
	for _, l := range ls {
		if l.ID == other && l.LastStatus == store.ListStatusOK {
			t.Fatal("the other list was refreshed too")
		}
	}
}

// TestListRefreshRejectsUnknownAndDisabledLists: an id naming nothing is a
// 404, and a disabled list is a 409 — the request is well-formed and the
// caller is allowed, and what forbids it is the state of this list.
func TestListRefreshRejectsUnknownAndDisabledLists(t *testing.T) {
	srv, s, rl := testServer(t)
	cookie := login(t, srv, s)
	h := srv.Handler()
	lid, err := s.Filters().AddList(t.Context(), store.List{URL: "https://x.example/hosts", Kind: "block", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Filters().SetListEnabled(t.Context(), lid, false); err != nil {
		t.Fatal(err)
	}

	if w := doReq(t, h, "POST", "/api/v1/filters/lists/999999/refresh", "", cookie); w.Code != 404 {
		t.Fatalf("unknown id: %d %s, want 404", w.Code, w.Body.String())
	}
	if w := doReq(t, h, "POST", "/api/v1/filters/lists/nope/refresh", "", cookie); w.Code != 400 {
		t.Fatalf("non-numeric id: %d %s, want 400", w.Code, w.Body.String())
	}
	w := doReq(t, h, "POST", fmt.Sprintf("/api/v1/filters/lists/%d/refresh", lid), "", cookie)
	if w.Code != 409 {
		t.Fatalf("disabled list: %d %s, want 409", w.Code, w.Body.String())
	}
	if got := rl.refreshedLists(); len(got) != 0 {
		t.Fatalf("a rejected request still downloaded %v", got)
	}
}

// TestListsCarryTheNextRefreshTime: every row reports when the periodic
// download runs next, which is a property of the server-wide cadence rather
// than of any one list — there are no per-list intervals.
func TestListsCarryTheNextRefreshTime(t *testing.T) {
	srv, s, rl := testServer(t)
	cookie := login(t, srv, s)
	h := srv.Handler()
	for _, u := range []string{"https://x.example/1", "https://x.example/2"} {
		if _, err := s.Filters().AddList(t.Context(), store.List{URL: u, Kind: "block", Enabled: true}); err != nil {
			t.Fatal(err)
		}
	}
	rl.nextRefreshAt = 1_700_000_600_000

	w := doReq(t, h, "GET", "/api/v1/filters/lists", "", cookie)
	if w.Code != 200 {
		t.Fatalf("lists: %d %s", w.Code, w.Body.String())
	}
	var rows []struct {
		ID            int64 `json:"id"`
		LastAttempt   int64 `json:"last_attempt"`
		NextRefreshAt int64 `json:"next_refresh_at"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %+v, want 2", rows)
	}
	for _, r := range rows {
		if r.NextRefreshAt != 1_700_000_600_000 {
			t.Fatalf("row %d next_refresh_at = %d, want the refresher's next tick", r.ID, r.NextRefreshAt)
		}
	}
}

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

// TestRulePatternValidation: a literal rule stored verbatim can be a pattern
// no query will ever carry — `*.doubleclick.net` becomes a label `*`, `||x^`
// a label with punctuation in it — and nothing anywhere said so. Accepted
// shapes are normalised to what the trie matches; the rest are 400s that
// name what a pattern may be.
func TestRulePatternValidation(t *testing.T) {
	srv, s, _ := testServer(t)
	cookie := login(t, srv, s)
	h := srv.Handler()
	gid, _ := s.Clients().AddGroup(t.Context(), "patterns")
	path := fmt.Sprintf("/api/v1/groups/%d/rules", gid)

	for _, bad := range []string{"||doubleclick.net^", "not a domain", "ads.*.example.com", "*", "a..b"} {
		body, _ := json.Marshal(map[string]any{"action": "block", "pattern": bad})
		w := doReq(t, h, "POST", path, string(body), cookie)
		if w.Code != 400 {
			t.Errorf("pattern %q accepted: %d %s", bad, w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "example.com") {
			t.Errorf("pattern %q rejected without saying what is accepted: %s", bad, w.Body.String())
		}
	}

	// Accepted, and stored in the form the matcher actually uses.
	for _, tc := range []struct{ in, want string }{
		{"*.doubleclick.net", "doubleclick.net"},
		{"Ads.Example.COM.", "ads.example.com"},
		{"localhost", "localhost"},
	} {
		body, _ := json.Marshal(map[string]any{"action": "block", "pattern": tc.in})
		if w := doReq(t, h, "POST", path, string(body), cookie); w.Code != 201 {
			t.Fatalf("pattern %q rejected: %d %s", tc.in, w.Code, w.Body.String())
		}
	}
	w := doReq(t, h, "GET", path, "", cookie)
	var rs []store.Rule
	_ = json.Unmarshal(w.Body.Bytes(), &rs)
	got := map[string]bool{}
	for _, r := range rs {
		got[r.Pattern] = true
	}
	for _, want := range []string{"doubleclick.net", "ads.example.com", "localhost"} {
		if !got[want] {
			t.Errorf("stored patterns %v, want %q among them", got, want)
		}
	}

	// A regex is a regex: it is not a domain and must not be normalised.
	body, _ := json.Marshal(map[string]any{"action": "block", "pattern": `^ads[0-9]+\.`, "is_regex": true})
	if w := doReq(t, h, "POST", path, string(body), cookie); w.Code != 201 {
		t.Fatalf("regex rule rejected: %d %s", w.Code, w.Body.String())
	}
}

// TestRuleWritesRecompileWithoutDownloading is the split: a rule save
// rebuilds the ruleset from what is already on disk. Fused with the
// download, one unreachable list URL made this request wait out the fetch
// timeout while every other write queued behind it.
func TestRuleWritesRecompileWithoutDownloading(t *testing.T) {
	srv, s, rl := testServer(t)
	cookie := login(t, srv, s)
	h := srv.Handler()
	gid, _ := s.Clients().AddGroup(t.Context(), "norefetch")

	w := doReq(t, h, "POST", fmt.Sprintf("/api/v1/groups/%d/rules", gid), `{"action":"block","pattern":"ads.example.com"}`, cookie)
	if w.Code != 201 {
		t.Fatalf("create rule: %d %s", w.Code, w.Body.String())
	}
	var created map[string]int64
	_ = json.Unmarshal(w.Body.Bytes(), &created)
	if w := doReq(t, h, "DELETE", fmt.Sprintf("/api/v1/filters/rules/%d", created["id"]), "", cookie); w.Code != 204 {
		t.Fatalf("delete rule: %d", w.Code)
	}
	if w := doReq(t, h, "PUT", fmt.Sprintf("/api/v1/groups/%d/lists", gid), `{"list_ids":[]}`, cookie); w.Code != 204 {
		t.Fatalf("assign lists: %d", w.Code)
	}

	if got := rl.recompileCount(); got != 3 {
		t.Fatalf("recompiles = %d, want 3 (rule create, rule delete, assignment)", got)
	}
	if _, _, downloads := rl.counts(); downloads != 0 {
		t.Fatalf("%d list downloads triggered by rule/assignment writes, want none", downloads)
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

// rules.group_id is a foreign key, and the group here comes from the path.
// The violation used to fall through to storeErr's default branch, so
// POST /groups/999/rules answered 503 "storage unavailable" — an
// infrastructure failure reported for a group that simply does not exist.
func TestRuleCreateOnAMissingGroupIs404(t *testing.T) {
	srv, s, _ := testServer(t)
	cookie := login(t, srv, s)
	w := doReq(t, srv.Handler(), "POST", "/api/v1/groups/999999/rules", `{"action":"block","pattern":"x.example"}`, cookie)
	if w.Code != 404 {
		t.Fatalf("status = %d body = %s, want 404", w.Code, w.Body.String())
	}
}

// A sub-resource of a group that does not exist is a missing resource, the
// same answer /zones/{id}/records already gave. Answering 200 [] instead
// said the group existed and had nothing assigned, which a dashboard cannot
// tell from the truth.
func TestGroupSubresourceGetsOnAMissingGroupAre404(t *testing.T) {
	srv, s, _ := testServer(t)
	cookie := login(t, srv, s)
	h := srv.Handler()
	for _, path := range []string{"/api/v1/groups/999999/lists", "/api/v1/groups/999999/rules"} {
		if w := doReq(t, h, "GET", path, "", cookie); w.Code != 404 {
			t.Errorf("GET %s status = %d body = %s, want 404", path, w.Code, w.Body.String())
		}
	}
}

// PUT /groups/{id}/lists replaced the set by unassigning row by row and then
// assigning, so a bad id failed after the unassigns had committed: the
// request answered an error and the group was left with nothing. The replace
// is one transaction now, and a bad id has to leave the group exactly as it
// was.
func TestGroupListsPutIsAllOrNothing(t *testing.T) {
	srv, s, _ := testServer(t)
	cookie := login(t, srv, s)
	h := srv.Handler()
	gid, _ := s.Clients().AddGroup(t.Context(), "g")
	l1, _ := s.Filters().AddList(t.Context(), store.List{URL: "https://x.example/1", Kind: "block", Enabled: true})
	l2, _ := s.Filters().AddList(t.Context(), store.List{URL: "https://x.example/2", Kind: "block", Enabled: true})
	path := fmt.Sprintf("/api/v1/groups/%d/lists", gid)
	if w := doReq(t, h, "PUT", path, fmt.Sprintf(`{"list_ids":[%d]}`, l1), cookie); w.Code != 204 {
		t.Fatalf("seed assignment: %d %s", w.Code, w.Body.String())
	}

	w := doReq(t, h, "PUT", path, fmt.Sprintf(`{"list_ids":[%d,999999]}`, l2), cookie)
	if w.Code != 400 {
		t.Fatalf("unknown list id: status = %d body = %s, want 400", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "999999") {
		t.Errorf("body = %s; want the offending id named", w.Body.String())
	}
	got, err := s.Filters().ListsForGroup(t.Context(), gid)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != l1 {
		t.Fatalf("assignments = %+v; want the pre-failure set intact", got)
	}

	// The same list named twice is one assignment, not a duplicate-key
	// failure.
	if w := doReq(t, h, "PUT", path, fmt.Sprintf(`{"list_ids":[%d,%d,%d]}`, l1, l2, l1), cookie); w.Code != 204 {
		t.Fatalf("duplicate ids: %d %s", w.Code, w.Body.String())
	}
	if got, _ := s.Filters().ListsForGroup(t.Context(), gid); len(got) != 2 {
		t.Fatalf("assignments = %+v; want both lists once each", got)
	}

	// An unknown group is the path's problem, not the body's.
	if w := doReq(t, h, "PUT", "/api/v1/groups/999999/lists", `{"list_ids":[]}`, cookie); w.Code != 404 {
		t.Fatalf("unknown group: status = %d body = %s, want 404", w.Code, w.Body.String())
	}
}
