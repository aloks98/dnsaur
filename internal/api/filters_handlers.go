package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"

	"github.com/aloks98/dnsaur/internal/filter"
	"github.com/aloks98/dnsaur/internal/store"
)

func (s *Server) filtersRoutes() {
	s.route("GET /api/v1/filters/lists", s.requireAuth(s.handleListsGet))
	s.route("POST /api/v1/filters/lists", s.requireAuth(s.handleListCreate))
	s.route("PATCH /api/v1/filters/lists/{id}", s.requireAuth(s.handleListPatch))
	s.route("DELETE /api/v1/filters/lists/{id}", s.requireAuth(s.handleListDelete))
	s.route("POST /api/v1/filters/lists/{id}/refresh", s.requireAuth(s.handleListRefresh))
	s.route("GET /api/v1/groups/{id}/lists", s.requireAuth(s.handleGroupListsGet))
	s.route("PUT /api/v1/groups/{id}/lists", s.requireAuth(s.handleGroupListsPut))
	s.route("GET /api/v1/groups/{id}/rules", s.requireAuth(s.handleRulesGet))
	s.route("POST /api/v1/groups/{id}/rules", s.requireAuth(s.handleRuleCreate))
	s.route("DELETE /api/v1/filters/rules/{id}", s.requireAuth(s.handleRuleDelete))
	s.route("POST /api/v1/filters/refresh", s.requireAuth(s.handleRefresh))
}

// refreshFilters rebuilds the compiled rulesets after a write. It
// recompiles from the list copies already on disk and never downloads: a
// rule, list or assignment write changes what is compiled, not what has been
// fetched, and fusing the two made every such request wait out the fetch
// timeout of the slowest subscribed URL — with the rest of the writes queued
// behind it. Downloading stays on the ticker, POST /filters/refresh and
// list creation.
//
// It runs with a context stripped of cancellation: the write already
// committed, so a client disconnecting mid-request must not abort the
// recompile.
func (s *Server) refreshFilters(r *http.Request) {
	if err := s.deps.Reloader.RecompileFilters(context.WithoutCancel(r.Context())); err != nil {
		slog.Error("filter recompile after api write failed", "err", err)
	}
}

// downloadLists kicks off a full network refresh in the background, for the
// two writes that mean "go and fetch": subscribing to a list, and asking for
// a refresh. A client disconnect must not cancel it, hence WithoutCancel.
func (s *Server) downloadLists(r *http.Request) {
	ctx := context.WithoutCancel(r.Context())
	go func() {
		if err := s.deps.Reloader.RefreshFilters(ctx); err != nil {
			slog.Error("filter refresh failed", "err", err)
		}
	}()
}

// listRow is a list as GET /filters/lists serves it: the stored row plus
// next_refresh_at, which is not stored anywhere. It comes from the running
// refresher's ticker, so it is the same value on every row — there are no
// per-list intervals — and it is what lets the table say how long the
// copies it is showing have left rather than only how old they are.
//
// The embedded store.List is flattened by encoding/json, so the wire shape
// is the list's own fields with one more beside them.
type listRow struct {
	store.List
	NextRefreshAt int64 `json:"next_refresh_at"`
}

func (s *Server) listRows(ls []store.List) []listRow {
	next := s.deps.Reloader.NextFilterRefresh()
	rows := make([]listRow, len(ls))
	for i, l := range ls {
		rows[i] = listRow{List: l, NextRefreshAt: next}
	}
	return rows
}

// findList reads one list by id, answering 404 itself when nothing has that
// id. FilterStore has no read-by-id and a homelab's subscription count is a
// handful of rows, so scanning the same read the table already does beats
// adding a query for it.
func (s *Server) findList(w http.ResponseWriter, r *http.Request, id int64) (store.List, bool) {
	ls, err := s.deps.Store.Filters().Lists(r.Context())
	if err != nil {
		storeErr(w, err)
		return store.List{}, false
	}
	for _, l := range ls {
		if l.ID == id {
			return l, true
		}
	}
	errJSON(w, http.StatusNotFound, "not found")
	return store.List{}, false
}

func (s *Server) handleListsGet(w http.ResponseWriter, r *http.Request) {
	ls, err := s.deps.Store.Filters().Lists(r.Context())
	if err != nil {
		storeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.listRows(ls))
}

type listCreate struct {
	URL string `json:"url"`
	// Name is optional; a blank one is derived from the URL by
	// store.AddList so the UI always has a label to show.
	Name string `json:"name"`
	Kind string `json:"kind"`
}

func (s *Server) handleListCreate(w http.ResponseWriter, r *http.Request) {
	body, ok := decodeOr400[listCreate](w, r)
	if !ok {
		return
	}
	u, uerr := url.Parse(body.URL)
	if uerr != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		errJSON(w, http.StatusBadRequest, "url must be http(s)")
		return
	}
	if body.Kind != "block" && body.Kind != "allow" {
		errJSON(w, http.StatusBadRequest, "kind must be block or allow")
		return
	}
	if len(body.Name) > maxListNameLen {
		errJSON(w, http.StatusBadRequest, "name too long (max 120)")
		return
	}
	id, err := s.deps.Store.Filters().AddList(r.Context(), store.List{URL: body.URL, Name: body.Name, Kind: body.Kind, Enabled: true})
	if err != nil {
		storeErrDup(w, err, "that list URL is already subscribed")
		return
	}
	// Apply it to every group straight away.
	//
	// A group's ruleset is compiled only from the lists assigned to it
	// (internal/filter/refresh.go's ListsForGroup), so a list that exists
	// but is assigned nowhere filters nothing — while the UI shows it
	// enabled with a six-figure entry count, which reads as working. That
	// gap is not theoretical: a 99,559-entry blocklist subscribed this way
	// blocked zero queries until it was assigned by hand on another screen.
	//
	// Subscribing to a blocklist means "block these", so the useful default
	// is on. Groups that shouldn't have it can unassign it — an explicit,
	// visible act — and a group created later starts from the same default
	// (see handleGroupCreate).
	groups, gerr := s.deps.Store.Clients().Groups(r.Context())
	if gerr != nil {
		slog.Error("assigning new list to groups failed", "list", id, "err", gerr)
	}
	for _, g := range groups {
		if err := s.deps.Store.Filters().AssignList(r.Context(), g.ID, id); err != nil {
			slog.Error("assigning new list to group failed", "list", id, "group", g.ID, "err", err)
		}
	}
	// Unlike the other list/rule mutations (recompiles from the on-disk
	// copies), a list nobody has fetched yet has nothing to compile, so
	// this one really does download — in the background, so the request
	// doesn't block on it.
	s.downloadLists(r)
	writeJSON(w, http.StatusCreated, map[string]int64{"id": id})
}

// maxListNameLen bounds a user-supplied list name. It is rendered in table
// cells, dropdown items and toasts; the derived defaults are far shorter.
const maxListNameLen = 120

// listPatch is the mutable surface of a list: what it's called and whether
// it's on. `url` and `kind` stay immutable — changing either would silently
// invalidate the on-disk cache and the compiled ruleset — and `decode`'s
// DisallowUnknownFields rejects them outright rather than ignoring them.
type listPatch struct {
	Enabled *bool   `json:"enabled"`
	Name    *string `json:"name"`
}

func (s *Server) handleListPatch(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		errJSON(w, http.StatusBadRequest, "bad id")
		return
	}
	body, ok := decodeOr400[listPatch](w, r)
	if !ok {
		return
	}
	// Either field alone is a valid patch. An `enabled`-only body — what
	// the row's toggle sends — must keep behaving exactly as it did.
	if body.Enabled == nil && body.Name == nil {
		errJSON(w, http.StatusBadRequest, "enabled or name required")
		return
	}
	if body.Name != nil {
		if len(*body.Name) > maxListNameLen {
			errJSON(w, http.StatusBadRequest, "name too long (max 120)")
			return
		}
		// A blank name is not an error: RenameList trims it and reads
		// blank as "go back to the URL-derived default".
		if err := s.deps.Store.Filters().RenameList(r.Context(), id, *body.Name); err != nil {
			storeErr(w, err)
			return
		}
	}
	if body.Enabled != nil {
		if err := s.deps.Store.Filters().SetListEnabled(r.Context(), id, *body.Enabled); err != nil {
			storeErr(w, err)
			return
		}
		if *body.Enabled {
			// Re-enabling compiles from the cached copy immediately; the
			// download is for the case where there isn't one yet (a list
			// added while the WAN was down), so it isn't stuck off until
			// the next tick.
			s.downloadLists(r)
		}
	}
	s.refreshFilters(r)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		errJSON(w, http.StatusBadRequest, "bad id")
		return
	}
	if err := s.deps.Store.Filters().DeleteList(r.Context(), id); err != nil {
		storeErr(w, err)
		return
	}
	s.refreshFilters(r)
	w.WriteHeader(http.StatusNoContent)
}

// groupExists reports whether the group named in the path is there,
// answering 404 itself when it is not.
//
// A sub-resource of a group that does not exist has to be a 404, the same as
// /zones/{id}/records already answered: `GET /groups/999/lists` returning
// `200 []` says the group exists and has nothing assigned, which is a claim
// a client cannot tell from the truth. The write routes get this from the
// foreign key instead — a read has none to violate.
func (s *Server) groupExists(w http.ResponseWriter, r *http.Request, gid int64) bool {
	groups, err := s.deps.Store.Clients().Groups(r.Context())
	if err != nil {
		storeErr(w, err)
		return false
	}
	for _, g := range groups {
		if g.ID == gid {
			return true
		}
	}
	errJSON(w, http.StatusNotFound, "not found")
	return false
}

func (s *Server) handleGroupListsGet(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		errJSON(w, http.StatusBadRequest, "bad id")
		return
	}
	if !s.groupExists(w, r, id) {
		return
	}
	ls, err := s.deps.Store.Filters().ListsForGroup(r.Context(), id)
	if err != nil {
		storeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ls)
}

type groupListsPut struct {
	ListIDs []int64 `json:"list_ids"`
}

func (s *Server) handleGroupListsPut(w http.ResponseWriter, r *http.Request) {
	gid, ok := pathID(r)
	if !ok {
		errJSON(w, http.StatusBadRequest, "bad id")
		return
	}
	body, ok := decodeOr400[groupListsPut](w, r)
	if !ok {
		return
	}
	// One transaction, in the store. This used to unassign the current rows
	// one at a time and then assign the new ones, so a list id naming
	// nothing failed *after* the unassigns had committed: the caller got an
	// error and the group was left with no lists at all — the destructive
	// half of a replace it had been told did not happen.
	if err := s.deps.Store.Filters().ReplaceGroupLists(r.Context(), gid, body.ListIDs); err != nil {
		var missing *store.MissingRef
		if errors.As(err, &missing) {
			errJSON(w, http.StatusBadRequest,
				"list_ids names list "+strconv.FormatInt(missing.ID, 10)+", which does not exist")
			return
		}
		storeErr(w, err)
		return
	}
	s.refreshFilters(r)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleRulesGet(w http.ResponseWriter, r *http.Request) {
	gid, ok := pathID(r)
	if !ok {
		errJSON(w, http.StatusBadRequest, "bad id")
		return
	}
	if !s.groupExists(w, r, gid) {
		return
	}
	rs, err := s.deps.Store.Filters().Rules(r.Context(), gid)
	if err != nil {
		storeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rs)
}

type ruleCreate struct {
	Action  string `json:"action"`
	Pattern string `json:"pattern"`
	IsRegex bool   `json:"is_regex"`
}

func (s *Server) handleRuleCreate(w http.ResponseWriter, r *http.Request) {
	gid, ok := pathID(r)
	if !ok {
		errJSON(w, http.StatusBadRequest, "bad id")
		return
	}
	body, ok := decodeOr400[ruleCreate](w, r)
	if !ok {
		return
	}
	if (body.Action != "allow" && body.Action != "block") || body.Pattern == "" {
		errJSON(w, http.StatusBadRequest, "action allow|block and pattern required")
		return
	}
	pattern := body.Pattern
	if body.IsRegex {
		if len(body.Pattern) > 512 {
			errJSON(w, http.StatusBadRequest, "regex pattern too long (max 512)")
			return
		}
		if _, err := regexp.Compile(body.Pattern); err != nil {
			errJSON(w, http.StatusBadRequest, "invalid regex: "+err.Error())
			return
		}
	} else {
		// Through the same normalisation a list entry gets. Stored
		// verbatim, a Pi-hole-style `*.doubleclick.net` or an ABP `||x^`
		// becomes a label no query can carry: a rule that matches nothing,
		// reports nothing, and looks exactly like one that works.
		norm, ok := filter.RulePattern(body.Pattern)
		if !ok {
			errJSON(w, http.StatusBadRequest, "pattern must be a domain like example.com, *.example.com or localhost")
			return
		}
		pattern = norm
	}
	// storeErr, not storeErrDupRef: rules.group_id is a foreign key and the
	// group it names came from the path, so a violation means the resource
	// this URL addresses does not exist — 404, which is what storeErr
	// answers for ErrReference with no field to name.
	id, err := s.deps.Store.Filters().AddRule(r.Context(), store.Rule{GroupID: gid, Action: body.Action, Pattern: pattern, IsRegex: body.IsRegex})
	if err != nil {
		storeErr(w, err)
		return
	}
	s.refreshFilters(r)
	writeJSON(w, http.StatusCreated, map[string]int64{"id": id})
}

func (s *Server) handleRuleDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		errJSON(w, http.StatusBadRequest, "bad id")
		return
	}
	if err := s.deps.Store.Filters().DeleteRule(r.Context(), id); err != nil {
		storeErr(w, err)
		return
	}
	s.refreshFilters(r)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleRefresh(w http.ResponseWriter, r *http.Request) {
	s.downloadLists(r)
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "refreshing"})
}

// handleListRefresh downloads one list on demand — the row's own "Refresh
// now", as against POST /filters/refresh, which re-fetches every
// subscription and so costs the slowest URL's timeout even when only one
// row is being looked at.
//
// 202, the same status the all-lists refresh answers, so a client reads one
// code for one verb. Unlike that one it is not fire-and-forget: the
// download runs inside the request and the body is the list as the refresh
// left it, which is what the row that asked for it needs and what saves the
// UI from polling to find out.
func (s *Server) handleListRefresh(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		errJSON(w, http.StatusBadRequest, "bad id")
		return
	}
	l, ok := s.findList(w, r, id)
	if !ok {
		return
	}
	if !l.Enabled {
		// The request is well-formed and the caller is allowed; what
		// forbids it is the state of this list, which is what 409 means
		// here as it does for a built-in zone. Fetching a copy that
		// nothing will compile would only rewrite the row's state with
		// the outcome of a download that changes nothing.
		errJSON(w, http.StatusConflict, "this list is disabled")
		return
	}
	// WithoutCancel: the download records what it found on the list's own
	// row, so a client that disconnects mid-fetch must not leave that row
	// describing an attempt abandoned halfway.
	if err := s.deps.Reloader.RefreshList(context.WithoutCancel(r.Context()), id); err != nil {
		storeErr(w, err)
		return
	}
	// Re-read rather than reuse l: the refresh is the whole point, and the
	// row it wrote is what the caller asked for.
	l, ok = s.findList(w, r, id)
	if !ok {
		return
	}
	writeJSON(w, http.StatusAccepted, listRow{List: l, NextRefreshAt: s.deps.Reloader.NextFilterRefresh()})
}
