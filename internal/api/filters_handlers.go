package api

import (
	"context"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"

	"github.com/aloks98/dnsaur/internal/store"
)

func (s *Server) filtersRoutes() {
	s.route("GET /api/v1/filters/lists", s.requireAuth(s.handleListsGet))
	s.route("POST /api/v1/filters/lists", s.requireAuth(s.handleListCreate))
	s.route("PATCH /api/v1/filters/lists/{id}", s.requireAuth(s.handleListPatch))
	s.route("DELETE /api/v1/filters/lists/{id}", s.requireAuth(s.handleListDelete))
	s.route("GET /api/v1/groups/{id}/lists", s.requireAuth(s.handleGroupListsGet))
	s.route("PUT /api/v1/groups/{id}/lists", s.requireAuth(s.handleGroupListsPut))
	s.route("GET /api/v1/groups/{id}/rules", s.requireAuth(s.handleRulesGet))
	s.route("POST /api/v1/groups/{id}/rules", s.requireAuth(s.handleRuleCreate))
	s.route("DELETE /api/v1/filters/rules/{id}", s.requireAuth(s.handleRuleDelete))
	s.route("POST /api/v1/filters/refresh", s.requireAuth(s.handleRefresh))
}

// refreshFilters runs with a context stripped of cancellation: the write
// already committed, so a client disconnecting mid-request must not abort
// the refresh.
func (s *Server) refreshFilters(r *http.Request) {
	if err := s.deps.Reloader.RefreshFilters(context.WithoutCancel(r.Context())); err != nil {
		slog.Error("filter refresh after api write failed", "err", err)
	}
}

func (s *Server) handleListsGet(w http.ResponseWriter, r *http.Request) {
	ls, err := s.deps.Store.Filters().Lists(r.Context())
	if err != nil {
		storeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ls)
}

type listCreate struct {
	URL string `json:"url"`
	// Name is optional; a blank one is derived from the URL by
	// store.AddList so the UI always has a label to show.
	Name string `json:"name"`
	Kind string `json:"kind"`
}

func (s *Server) handleListCreate(w http.ResponseWriter, r *http.Request) {
	body, err := decode[listCreate](r)
	if err != nil {
		errJSON(w, http.StatusBadRequest, "invalid json")
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
	// Unlike the other list/rule mutations (cheap metadata ops refreshed
	// synchronously), adding a list triggers a full network refresh of
	// every list. Do that in the background so the request doesn't block
	// on it, mirroring handleRefresh; a client disconnect must not cancel
	// it either, hence WithoutCancel.
	go func() {
		if err := s.deps.Reloader.RefreshFilters(context.WithoutCancel(r.Context())); err != nil {
			slog.Error("filter refresh after list create failed", "err", err)
		}
	}()
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
	body, err := decode[listPatch](r)
	// Either field alone is a valid patch. An `enabled`-only body — what
	// the row's toggle sends — must keep behaving exactly as it did.
	if err != nil || (body.Enabled == nil && body.Name == nil) {
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

func (s *Server) handleGroupListsGet(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		errJSON(w, http.StatusBadRequest, "bad id")
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
	body, err := decode[groupListsPut](r)
	if err != nil {
		errJSON(w, http.StatusBadRequest, "invalid json")
		return
	}
	current, err := s.deps.Store.Filters().ListsForGroup(r.Context(), gid)
	if err != nil {
		storeErr(w, err)
		return
	}
	for _, l := range current {
		if err := s.deps.Store.Filters().UnassignList(r.Context(), gid, l.ID); err != nil {
			storeErr(w, err)
			return
		}
	}
	for _, id := range body.ListIDs {
		if err := s.deps.Store.Filters().AssignList(r.Context(), gid, id); err != nil {
			storeErr(w, err)
			return
		}
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
	body, err := decode[ruleCreate](r)
	if err != nil || (body.Action != "allow" && body.Action != "block") || body.Pattern == "" {
		errJSON(w, http.StatusBadRequest, "action allow|block and pattern required")
		return
	}
	if body.IsRegex {
		if len(body.Pattern) > 512 {
			errJSON(w, http.StatusBadRequest, "regex pattern too long (max 512)")
			return
		}
		if _, err := regexp.Compile(body.Pattern); err != nil {
			errJSON(w, http.StatusBadRequest, "invalid regex: "+err.Error())
			return
		}
	}
	id, err := s.deps.Store.Filters().AddRule(r.Context(), store.Rule{GroupID: gid, Action: body.Action, Pattern: body.Pattern, IsRegex: body.IsRegex})
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
	go func() {
		if err := s.deps.Reloader.RefreshFilters(context.WithoutCancel(r.Context())); err != nil {
			slog.Error("manual filter refresh failed", "err", err)
		}
	}()
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "refreshing"})
}
