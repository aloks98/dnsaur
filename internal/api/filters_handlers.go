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
	s.mux.HandleFunc("GET /api/v1/filters/lists", s.requireAuth(s.handleListsGet))
	s.mux.HandleFunc("POST /api/v1/filters/lists", s.requireAuth(s.handleListCreate))
	s.mux.HandleFunc("PATCH /api/v1/filters/lists/{id}", s.requireAuth(s.handleListPatch))
	s.mux.HandleFunc("DELETE /api/v1/filters/lists/{id}", s.requireAuth(s.handleListDelete))
	s.mux.HandleFunc("GET /api/v1/groups/{id}/lists", s.requireAuth(s.handleGroupListsGet))
	s.mux.HandleFunc("PUT /api/v1/groups/{id}/lists", s.requireAuth(s.handleGroupListsPut))
	s.mux.HandleFunc("GET /api/v1/groups/{id}/rules", s.requireAuth(s.handleRulesGet))
	s.mux.HandleFunc("POST /api/v1/groups/{id}/rules", s.requireAuth(s.handleRuleCreate))
	s.mux.HandleFunc("DELETE /api/v1/filters/rules/{id}", s.requireAuth(s.handleRuleDelete))
	s.mux.HandleFunc("POST /api/v1/filters/refresh", s.requireAuth(s.handleRefresh))
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
	URL  string `json:"url"`
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
	id, err := s.deps.Store.Filters().AddList(r.Context(), store.List{URL: body.URL, Kind: body.Kind, Enabled: true})
	if err != nil {
		storeErrDup(w, err, "that list URL is already subscribed")
		return
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

type listPatch struct {
	Enabled *bool `json:"enabled"`
}

func (s *Server) handleListPatch(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		errJSON(w, http.StatusBadRequest, "bad id")
		return
	}
	body, err := decode[listPatch](r)
	if err != nil || body.Enabled == nil {
		errJSON(w, http.StatusBadRequest, "enabled required")
		return
	}
	if err := s.deps.Store.Filters().SetListEnabled(r.Context(), id, *body.Enabled); err != nil {
		storeErr(w, err)
		return
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
