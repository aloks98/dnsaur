package api

import (
	"context"
	"log/slog"
	"net/http"
	"net/netip"
	"strconv"

	"github.com/aloks98/dnsaur/internal/store"
)

func (s *Server) clientsRoutes() {
	s.mux.HandleFunc("GET /api/v1/groups", s.requireAuth(s.handleGroupsList))
	s.mux.HandleFunc("POST /api/v1/groups", s.requireAuth(s.handleGroupCreate))
	s.mux.HandleFunc("PATCH /api/v1/groups/{id}", s.requireAuth(s.handleGroupPatch))
	s.mux.HandleFunc("DELETE /api/v1/groups/{id}", s.requireAuth(s.handleGroupDelete))
	s.mux.HandleFunc("GET /api/v1/clients", s.requireAuth(s.handleClientsList))
	s.mux.HandleFunc("POST /api/v1/clients", s.requireAuth(s.handleClientCreate))
	s.mux.HandleFunc("PUT /api/v1/clients/{id}", s.requireAuth(s.handleClientPut))
	s.mux.HandleFunc("DELETE /api/v1/clients/{id}", s.requireAuth(s.handleClientDelete))
}

func pathID(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id, err == nil && id > 0
}

// reloadClients is called after successful mutations; failures are logged,
// never surfaced — the write already committed. It runs with a context
// stripped of cancellation: the write already landed, so a client
// disconnecting mid-request must not abort the reload.
func (s *Server) reloadClients(r *http.Request) {
	if err := s.deps.Reloader.ReloadClients(context.WithoutCancel(r.Context())); err != nil {
		slog.Error("client reload after api write failed", "err", err)
	}
}

func (s *Server) handleGroupsList(w http.ResponseWriter, r *http.Request) {
	gs, err := s.deps.Store.Clients().Groups(r.Context())
	if err != nil {
		storeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, gs)
}

type groupCreate struct {
	Name string `json:"name"`
	// Enabled is a pointer so "not sent" is distinguishable from "false".
	// Omitted means enabled, which is what creating a group almost always
	// means; a group created disabled filters nothing for anyone moved
	// into it.
	Enabled *bool `json:"enabled"`
	// ListIDs, when present, is the exact set to assign — including an
	// empty array, which means "no lists". Omitted keeps the historical
	// behaviour of inheriting every list that exists.
	ListIDs []int64 `json:"list_ids"`
}

func (s *Server) handleGroupCreate(w http.ResponseWriter, r *http.Request) {
	body, err := decode[groupCreate](r)
	if err != nil || body.Name == "" {
		errJSON(w, http.StatusBadRequest, "name required")
		return
	}
	id, err := s.deps.Store.Clients().AddGroup(r.Context(), body.Name)
	if err != nil {
		storeErrDup(w, err, "a group with that name already exists")
		return
	}
	// Applied after creation rather than in AddGroup: the store's insert
	// takes only a name, and a second UPDATE is cheaper than a migration
	// for a field that is almost always its default.
	if body.Enabled != nil && !*body.Enabled {
		if err := s.deps.Store.Clients().SetGroupEnabled(r.Context(), id, false); err != nil {
			slog.Error("disabling new group failed", "group", id, "err", err)
		}
	}

	// An explicit list_ids wins, empty array included — "assign nothing" is
	// a real choice and has to be distinguishable from not choosing.
	//
	// Omitted still means every list. A group's ruleset compiles only from
	// its assigned lists, so a group created with none filters nothing at
	// all, and the clients moved into it would silently stop being
	// protected — the opposite of why groups exist.
	listIDs := body.ListIDs
	if listIDs == nil {
		lists, lerr := s.deps.Store.Filters().Lists(r.Context())
		if lerr != nil {
			slog.Error("assigning lists to new group failed", "group", id, "err", lerr)
		}
		for _, l := range lists {
			listIDs = append(listIDs, l.ID)
		}
	}
	for _, lid := range listIDs {
		if err := s.deps.Store.Filters().AssignList(r.Context(), id, lid); err != nil {
			slog.Error("assigning list to new group failed", "group", id, "list", lid, "err", err)
		}
	}
	s.reloadClients(r)
	writeJSON(w, http.StatusCreated, map[string]int64{"id": id})
}

type groupPatch struct {
	Name    *string `json:"name"`
	Enabled *bool   `json:"enabled"`
}

func (s *Server) handleGroupPatch(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		errJSON(w, http.StatusBadRequest, "bad id")
		return
	}
	body, err := decode[groupPatch](r)
	if err != nil {
		errJSON(w, http.StatusBadRequest, "invalid json")
		return
	}
	if body.Name != nil && *body.Name == "" {
		errJSON(w, http.StatusBadRequest, "name cannot be empty")
		return
	}
	if body.Name != nil {
		if err := s.deps.Store.Clients().RenameGroup(r.Context(), id, *body.Name); err != nil {
			storeErrDup(w, err, "a group with that name already exists")
			return
		}
	}
	if body.Enabled != nil {
		if err := s.deps.Store.Clients().SetGroupEnabled(r.Context(), id, *body.Enabled); err != nil {
			storeErr(w, err)
			return
		}
	}
	s.reloadClients(r)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleGroupDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		errJSON(w, http.StatusBadRequest, "bad id")
		return
	}
	if err := s.deps.Store.Clients().DeleteGroup(r.Context(), id); err != nil {
		storeErr(w, err)
		return
	}
	s.reloadClients(r)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleClientsList(w http.ResponseWriter, r *http.Request) {
	cs, err := s.deps.Store.Clients().Clients(r.Context())
	if err != nil {
		storeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, cs)
}

func validMatcher(m string) bool {
	if _, err := netip.ParseAddr(m); err == nil {
		return true
	}
	_, err := netip.ParsePrefix(m)
	return err == nil
}

func (s *Server) handleClientCreate(w http.ResponseWriter, r *http.Request) {
	body, err := decode[store.Client](r)
	if err != nil || !validMatcher(body.Matcher) || body.GroupID <= 0 {
		errJSON(w, http.StatusBadRequest, "matcher must be an IP or CIDR and group_id set")
		return
	}
	id, err := s.deps.Store.Clients().AddClient(r.Context(), body)
	if err != nil {
		storeErrDup(w, err, "another client already matches "+body.Matcher)
		return
	}
	s.reloadClients(r)
	writeJSON(w, http.StatusCreated, map[string]int64{"id": id})
}

func (s *Server) handleClientPut(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		errJSON(w, http.StatusBadRequest, "bad id")
		return
	}
	body, err := decode[store.Client](r)
	if err != nil || !validMatcher(body.Matcher) || body.GroupID <= 0 {
		errJSON(w, http.StatusBadRequest, "matcher must be an IP or CIDR and group_id set")
		return
	}
	body.ID = id
	if err := s.deps.Store.Clients().UpdateClient(r.Context(), body); err != nil {
		storeErrDup(w, err, "another client already matches "+body.Matcher)
		return
	}
	s.reloadClients(r)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleClientDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		errJSON(w, http.StatusBadRequest, "bad id")
		return
	}
	if err := s.deps.Store.Clients().DeleteClient(r.Context(), id); err != nil {
		storeErr(w, err)
		return
	}
	s.reloadClients(r)
	w.WriteHeader(http.StatusNoContent)
}
