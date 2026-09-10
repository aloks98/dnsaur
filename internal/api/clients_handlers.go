package api

import (
	"context"
	"log/slog"
	"net/http"
	"slices"
	"strconv"

	"github.com/aloks98/dnsaur/internal/clients"
	"github.com/aloks98/dnsaur/internal/store"
)

func (s *Server) clientsRoutes() {
	s.route("GET /api/v1/groups", s.requireAuth(s.handleGroupsList))
	s.route("POST /api/v1/groups", s.requireAuth(s.handleGroupCreate))
	s.route("PATCH /api/v1/groups/{id}", s.requireAuth(s.handleGroupPatch))
	s.route("DELETE /api/v1/groups/{id}", s.requireAuth(s.handleGroupDelete))
	s.route("GET /api/v1/clients", s.requireAuth(s.handleClientsList))
	s.route("POST /api/v1/clients", s.requireAuth(s.handleClientCreate))
	s.route("PUT /api/v1/clients/{id}", s.requireAuth(s.handleClientPut))
	s.route("DELETE /api/v1/clients/{id}", s.requireAuth(s.handleClientDelete))
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
	body, ok := decodeOr400[groupCreate](w, r)
	if !ok {
		return
	}
	if body.Name == "" {
		errJSON(w, http.StatusBadRequest, "name required")
		return
	}

	// The lists are resolved and checked *before* the group exists.
	//
	// This used to run after the insert, with every failure logged and 201
	// answered anyway: a body naming a list that does not exist produced a
	// group with no lists at all and no indication that anything had gone
	// wrong. Checking first means a request that cannot be carried out
	// leaves nothing behind for the caller to clean up.
	//
	// An explicit list_ids wins, empty array included — "assign nothing" is
	// a real choice and has to be distinguishable from not choosing.
	// Omitted still means every list: a group's ruleset compiles only from
	// its assigned lists, so a group created with none filters nothing at
	// all, and the clients moved into it would silently stop being
	// protected — the opposite of why groups exist.
	lists, err := s.deps.Store.Filters().Lists(r.Context())
	if err != nil {
		storeErr(w, err)
		return
	}
	known := make(map[int64]bool, len(lists))
	for _, l := range lists {
		known[l.ID] = true
	}
	listIDs := body.ListIDs
	if listIDs == nil {
		for _, l := range lists {
			listIDs = append(listIDs, l.ID)
		}
	}
	for _, lid := range listIDs {
		if !known[lid] {
			errJSON(w, http.StatusBadRequest,
				"list_ids names list "+strconv.FormatInt(lid, 10)+", which does not exist")
			return
		}
	}

	id, err := s.deps.Store.Clients().AddGroup(r.Context(), body.Name)
	if err != nil {
		storeErrDup(w, err, "a group with that name already exists")
		return
	}
	// Applied after creation rather than in AddGroup: the store's insert
	// takes only a name, and a second UPDATE is cheaper than a migration
	// for a field that is almost always its default. Its failure is
	// surfaced, not logged — answering 201 for a group that came out
	// enabled when the caller asked for disabled is a lie about the one
	// field they bothered to send.
	enabled := body.Enabled == nil || *body.Enabled
	if !enabled {
		if err := s.deps.Store.Clients().SetGroupEnabled(r.Context(), id, false); err != nil {
			storeErr(w, err)
			return
		}
	}
	if len(listIDs) > 0 {
		if err := s.deps.Store.Filters().ReplaceGroupLists(r.Context(), id, listIDs); err != nil {
			storeErr(w, err)
			return
		}
	}
	s.reloadClients(r)
	created(w, resourceURL("groups", id), store.Group{ID: id, Name: body.Name, Enabled: enabled})
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
	body, ok := decodeOr400[groupPatch](w, r)
	if !ok {
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

// handleClientsList answers every client, optionally narrowed to one
// group's by ?group_id=. Filtered in the handler, for the reason the zone
// listing gives: ClientStore.Clients takes no options, and the row count is
// a household's devices.
//
// A group_id naming no group is an empty array rather than a 404. The
// parameter narrows a listing; it does not address a resource, and a group
// that was deleted a moment ago has no clients, which is the true answer.
func (s *Server) handleClientsList(w http.ResponseWriter, r *http.Request) {
	groupID, byGroup, err := qID(r, "group_id")
	if err != nil {
		errJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	cs, err := s.deps.Store.Clients().Clients(r.Context())
	if err != nil {
		storeErr(w, err)
		return
	}
	if byGroup {
		cs = slices.DeleteFunc(cs, func(c store.Client) bool { return c.GroupID != groupID })
	}
	writeJSON(w, http.StatusOK, cs)
}

// badMatcher is what both write paths answer with. It names the interface
// zone because that is the one rejection whose cause isn't obvious from
// looking at the value: `fe80::1%eth0` is a perfectly good address that the
// request side can never produce (see clients.NormalizeMatcher).
const badMatcher = "matcher must be an IP or CIDR without an interface zone, and group_id set"

// missingGroupMsg is the answer to a client write whose group_id names no
// group. clients.group_id is a foreign key, and this one is named in the
// *body*, so it is a bad field (400) rather than a missing resource (404) —
// see storeErrDupRef. Before it was mapped at all, this was a 503 "storage
// unavailable" for a typo.
const missingGroupMsg = "group_id does not name an existing group"

func (s *Server) handleClientCreate(w http.ResponseWriter, r *http.Request) {
	body, ok := decodeOr400[store.Client](w, r)
	if !ok {
		return
	}
	if body.GroupID <= 0 {
		errJSON(w, http.StatusBadRequest, badMatcher)
		return
	}
	m, ok := clients.NormalizeMatcher(body.Matcher)
	if !ok {
		errJSON(w, http.StatusBadRequest, badMatcher)
		return
	}
	body.Matcher = m
	id, err := s.deps.Store.Clients().AddClient(r.Context(), body)
	if err != nil {
		storeErrDupRef(w, err, "another client already matches "+body.Matcher, missingGroupMsg)
		return
	}
	s.reloadClients(r)
	body.ID = id
	created(w, resourceURL("clients", id), body)
}

func (s *Server) handleClientPut(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		errJSON(w, http.StatusBadRequest, "bad id")
		return
	}
	body, ok := decodeOr400[store.Client](w, r)
	if !ok {
		return
	}
	if body.GroupID <= 0 {
		errJSON(w, http.StatusBadRequest, badMatcher)
		return
	}
	m, ok := clients.NormalizeMatcher(body.Matcher)
	if !ok {
		errJSON(w, http.StatusBadRequest, badMatcher)
		return
	}
	body.Matcher = m
	body.ID = id
	if err := s.deps.Store.Clients().UpdateClient(r.Context(), body); err != nil {
		storeErrDupRef(w, err, "another client already matches "+body.Matcher, missingGroupMsg)
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
