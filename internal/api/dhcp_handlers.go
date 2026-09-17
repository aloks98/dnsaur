package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/aloks98/dnsaur/internal/dhcp"
	"github.com/aloks98/dnsaur/internal/store"
)

func (s *Server) dhcpRoutes() {
	// Scopes and reservations are synced configuration (design §4.3, §4.4),
	// so every write to them carries the replica guard. dhcpEnabled sits
	// *inside* managed on purpose: a replica refuses the write whether or
	// not it runs an engine of its own, and "managed by the main" is the
	// answer the operator can act on.
	s.route("GET /api/v1/dhcp/scopes", s.requireAuth(s.dhcpEnabled(s.handleDHCPScopes)))
	s.route("POST /api/v1/dhcp/scopes", s.requireAuth(s.managed(s.dhcpEnabled(s.handleDHCPScopeCreate))))
	s.route("PATCH /api/v1/dhcp/scopes/{id}", s.requireAuth(s.managed(s.dhcpEnabled(s.handleDHCPScopePatch))))
	s.route("DELETE /api/v1/dhcp/scopes/{id}", s.requireAuth(s.managed(s.dhcpEnabled(s.handleDHCPScopeDelete))))
	s.route("GET /api/v1/dhcp/reservations", s.requireAuth(s.dhcpEnabled(s.handleDHCPReservations)))
	s.route("POST /api/v1/dhcp/reservations", s.requireAuth(s.managed(s.dhcpEnabled(s.handleDHCPReservationCreate))))
	s.route("PATCH /api/v1/dhcp/reservations/{id}", s.requireAuth(s.managed(s.dhcpEnabled(s.handleDHCPReservationPatch))))
	s.route("DELETE /api/v1/dhcp/reservations/{id}", s.requireAuth(s.managed(s.dhcpEnabled(s.handleDHCPReservationDelete))))
	s.route("GET /api/v1/dhcp/leases", s.requireAuth(s.dhcpEnabled(s.handleDHCPLeases)))
	// Not managed: a lease belongs to the engine rather than to the
	// configuration, and HA propagates the release (§8.3). A replica's
	// Release button stays live.
	s.route("DELETE /api/v1/dhcp/leases/{ip}", s.requireAuth(s.dhcpEnabled(s.handleDHCPLeaseDelete)))
	s.route("POST /api/v1/dhcp/leases/{ip}/reserve", s.requireAuth(s.managed(s.dhcpEnabled(s.handleDHCPLeaseReserve))))
	// The one /dhcp route that answers on a box with no engine: "is DHCP
	// running here" is exactly what it is for.
	s.route("GET /api/v1/dhcp/status", s.requireAuth(s.handleDHCPStatus))
	s.route("POST /api/v1/dhcp/apply", s.requireAuth(s.managed(s.dhcpEnabled(s.handleDHCPApply))))
}

// dhcpEnabled refuses every DHCP route on a box whose kea_socket is empty
// (§4.1, §10): there is no engine, no lease table and nothing to configure,
// so the resources behind these URLs do not exist. 404 rather than 503,
// because nothing is temporarily unavailable — DHCP is off, and turning it
// on is a bootstrap change and a restart.
func (s *Server) dhcpEnabled(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.deps.DHCP == nil {
			errJSON(w, http.StatusNotFound, "dhcp is not enabled")
			return
		}
		h(w, r)
	}
}

// dhcpApplyTimeout bounds the render a write kicks, and with it the request
// that kicked it: this context carries a deadline, so it is what the engine
// client's own config-set timeout gives way to.
//
// Ten seconds, not the thirty a config-set is allowed on a poll. The write
// has already committed and its answer never depends on the render, so
// everything past the point where the engine has plainly stopped answering
// is a dashboard sitting on a spinner for a scope that is already stored —
// and the refusal, when there is one, is on the DHCP page either way.
const dhcpApplyTimeout = 10 * time.Second

// applyDHCP re-renders and sends the configuration after a write that
// changed it, and reports what the engine said only to the log.
//
// **The write's own answer never depends on this.** A config the engine
// refuses leaves the row stored and the engine on its previous
// configuration (§5.1); the refusal is in GET /dhcp/status, where it stays
// until a render is accepted, and telling the caller its scope was not
// saved — which is what a 5xx here would say — would be false.
//
// The context is stripped of cancellation for the reason refreshFilters
// strips it: the write has committed, and a client that hung up must not
// leave the engine running a configuration nothing asked for.
//
// Apply, which reads the configuration itself, rather than ApplyInput with
// something read here. There is nothing here to hand it: this package sees
// the scopes and reservations it validated against, and a render is also the
// settings behind them, this box's place in the pair and its own addresses —
// all of which the app reads, through the interface the manager already
// holds. Apply's read happens inside the lock that sends it, so there is no
// window for the two to drift; the read that F4 removed was a second one,
// made by a caller that had already compared against the first.
func (s *Server) applyDHCP(r *http.Request) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), dhcpApplyTimeout)
	defer cancel()
	if err := s.deps.DHCP.Apply(ctx); err != nil {
		slog.Warn("applying the dhcp configuration after an api write failed", "err", err)
	}
}

// dhcpScopes reads every scope, which is what both the list and the overlap
// rule need: ValidateScope is a claim about a set of rows (§4.3).
func (s *Server) dhcpScopes(w http.ResponseWriter, r *http.Request) ([]store.Scope, bool) {
	all, err := s.deps.Store.DHCP().Scopes(r.Context())
	if err != nil {
		storeErr(w, err)
		return nil, false
	}
	return all, true
}

func (s *Server) dhcpReservations(w http.ResponseWriter, r *http.Request) ([]store.Reservation, bool) {
	all, err := s.deps.Store.DHCP().Reservations(r.Context())
	if err != nil {
		storeErr(w, err)
		return nil, false
	}
	return all, true
}

func (s *Server) handleDHCPScopes(w http.ResponseWriter, r *http.Request) {
	all, ok := s.dhcpScopes(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, all)
}

func (s *Server) handleDHCPReservations(w http.ResponseWriter, r *http.Request) {
	all, ok := s.dhcpReservations(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, all)
}

// dhcpInvalid answers a validator's refusal. The message is the validator's
// own, because it names the column and the value — and the two sentinels
// §4.3 and §4.4 keep apart are refusals of the same kind, not failures:
// ErrDuplicate is the one that is about another row rather than this one.
func dhcpInvalid(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrDuplicate) {
		errJSON(w, http.StatusConflict, err.Error())
		return
	}
	errJSON(w, http.StatusBadRequest, err.Error())
}

// scopeWire is store.Scope without its UnmarshalJSON. The method is what
// supplies match_client_id's default, and it is also what makes
// DisallowUnknownFields inert: encoding/json hands the whole object to the
// custom unmarshaler and never looks at the keys itself, so `{"poolstart":
// "10.0.0.5"}` was accepted and silently did nothing.
//
// Strictness belongs here rather than inside that method: a config bundle
// decodes through it too, and a main one version ahead would have every
// replica refuse the whole bundle over a column it does not know yet.
type scopeWire store.Scope

// decodeScope reads a scope body twice over the same bytes: once strictly,
// through the alias, so an unknown key is a 400 rather than a field silently
// doing nothing, and once through store.Scope so match_client_id's
// absent-means-true default is the store's own answer rather than a second
// copy of it here.
func decodeScope(w http.ResponseWriter, r *http.Request) (store.Scope, bool) {
	raw, ok := readBody(w, r)
	if !ok {
		return store.Scope{}, false
	}
	var strict scopeWire
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&strict); err != nil {
		errJSON(w, http.StatusBadRequest, "invalid json")
		return store.Scope{}, false
	}
	var sc store.Scope
	if err := json.Unmarshal(raw, &sc); err != nil {
		errJSON(w, http.StatusBadRequest, "invalid json")
		return store.Scope{}, false
	}
	return sc, true
}

// readBody is the body as bytes, for the two handlers that decode it twice.
// The same megabyte cap decode() applies.
func readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		errJSON(w, http.StatusBadRequest, "invalid json")
		return nil, false
	}
	return raw, true
}

func (s *Server) handleDHCPScopeCreate(w http.ResponseWriter, r *http.Request) {
	sc, ok := decodeScope(w, r)
	if !ok {
		return
	}
	others, ok := s.dhcpScopes(w, r)
	if !ok {
		return
	}
	sc.ID = 0
	if err := store.ValidateScope(sc, others); err != nil {
		dhcpInvalid(w, err)
		return
	}
	now := time.Now().UnixMilli()
	sc.CreatedAt, sc.ModifiedAt = now, now
	id, err := s.deps.Store.DHCP().AddScope(r.Context(), sc)
	if err != nil {
		storeErrDup(w, err, "a scope with that name already exists")
		return
	}
	sc.ID = id
	s.applyDHCP(r)
	created(w, resourceURL("dhcp/scopes", id), sc)
}

// patchScope merges the request body onto the scope as it is stored, so a
// body naming one field leaves the other eighteen alone.
//
// Through scopeWire, which is what makes the merge a merge. Decoding into a
// store.Scope hands the whole object to its UnmarshalJSON, and that reads an
// absent match_client_id as "true" — the right answer for a create, where
// absent means "the column default", and the wrong one here, where absent
// means "unchanged": a PATCH sending nothing but `{"enabled": false}` would
// turn client-id matching back on for a scope of cloned VMs that had it off,
// and nothing on the screen would say so. It is also what lets
// DisallowUnknownFields see the keys at all.
func patchScope(w http.ResponseWriter, r *http.Request, cur store.Scope) (store.Scope, bool) {
	raw, ok := readBody(w, r)
	if !ok {
		return cur, false
	}
	merged := scopeWire(cur)
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&merged); err != nil {
		errJSON(w, http.StatusBadRequest, "invalid json")
		return cur, false
	}
	return store.Scope(merged), true
}

func (s *Server) handleDHCPScopePatch(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		errJSON(w, http.StatusBadRequest, "bad id")
		return
	}
	cur, err := s.deps.Store.DHCP().Scope(r.Context(), id)
	if err != nil {
		storeErr(w, err)
		return
	}
	sc, ok := patchScope(w, r, cur)
	if !ok {
		return
	}
	// The URL names the row, not the body: an id in the body would move the
	// edit to another scope, and created_at is not the caller's to rewrite.
	sc.ID, sc.CreatedAt = cur.ID, cur.CreatedAt
	others, ok := s.dhcpScopes(w, r)
	if !ok {
		return
	}
	if err := store.ValidateScope(sc, others); err != nil {
		dhcpInvalid(w, err)
		return
	}
	// Renumbering a scope, or moving its gateway, can strand the
	// reservations inside it: each one was checked against the subnet this
	// edit is replacing. They are checked against the new one here, because
	// the alternative is a scope whose own reservations the renderer then
	// refuses — the whole configuration, including every other scope, stuck
	// behind one address nobody can find without reading the engine's
	// message.
	if strings.TrimSpace(sc.CIDR) != cur.CIDR || strings.TrimSpace(sc.Gateway) != cur.Gateway {
		all, ok := s.dhcpReservations(w, r)
		if !ok {
			return
		}
		for _, res := range all {
			if res.ScopeID != id {
				continue
			}
			if err := store.ValidateReservation(res, sc, all); err != nil {
				errJSON(w, http.StatusBadRequest, "reservation "+res.IP+" ("+res.MAC+") would be stranded: "+err.Error())
				return
			}
		}
	}
	sc.ModifiedAt = time.Now().UnixMilli()
	if err := s.deps.Store.DHCP().UpdateScope(r.Context(), sc); err != nil {
		storeErrDup(w, err, "a scope with that name already exists")
		return
	}
	s.applyDHCP(r)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleDHCPScopeDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		errJSON(w, http.StatusBadRequest, "bad id")
		return
	}
	if err := s.deps.Store.DHCP().DeleteScope(r.Context(), id); err != nil {
		storeErr(w, err)
		return
	}
	s.applyDHCP(r)
	w.WriteHeader(http.StatusNoContent)
}

// dhcpScopeFor reads the parent of a reservation. Every rule in §4.4 is
// relative to it, so an id naming no scope is a 404 rather than a
// reservation checked against nothing.
func (s *Server) dhcpScopeFor(w http.ResponseWriter, r *http.Request, scopeID int64) (store.Scope, bool) {
	sc, err := s.deps.Store.DHCP().Scope(r.Context(), scopeID)
	if err != nil {
		storeErr(w, err)
		return store.Scope{}, false
	}
	return sc, true
}

func (s *Server) handleDHCPReservationCreate(w http.ResponseWriter, r *http.Request) {
	res, ok := decodeOr400[store.Reservation](w, r)
	if !ok {
		return
	}
	res.ID = 0
	sc, ok := s.dhcpScopeFor(w, r, res.ScopeID)
	if !ok {
		return
	}
	others, ok := s.dhcpReservations(w, r)
	if !ok {
		return
	}
	if err := store.ValidateReservation(res, sc, others); err != nil {
		dhcpInvalid(w, err)
		return
	}
	id, ok := s.addReservation(w, r, res)
	if !ok {
		return
	}
	res.ID = id
	s.applyDHCP(r)
	created(w, resourceURL("dhcp/reservations", id), res)
}

// addReservation stores one row, canonicalising nothing: the store does
// that (normaliseReservation), which is what makes the two unique indexes
// mean "this NIC" and "this address".
func (s *Server) addReservation(w http.ResponseWriter, r *http.Request, res store.Reservation) (int64, bool) {
	now := time.Now().UnixMilli()
	res.CreatedAt, res.ModifiedAt = now, now
	id, err := s.deps.Store.DHCP().AddReservation(r.Context(), res)
	if err != nil {
		storeErrDupRef(w, err, "that mac or address is already reserved in this scope", "scope_id names no scope")
		return 0, false
	}
	return id, true
}

func (s *Server) handleDHCPReservationPatch(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		errJSON(w, http.StatusBadRequest, "bad id")
		return
	}
	all, ok := s.dhcpReservations(w, r)
	if !ok {
		return
	}
	i := slices.IndexFunc(all, func(res store.Reservation) bool { return res.ID == id })
	if i < 0 {
		errJSON(w, http.StatusNotFound, "not found")
		return
	}
	cur := all[i]
	res := cur
	if !decodeInto(w, r, &res) {
		return
	}
	// A reservation does not change scope: the store refuses to move
	// scope_id (UpdateReservation), and an address validated against one
	// subnet must not be carried into another.
	res.ID, res.ScopeID, res.CreatedAt = cur.ID, cur.ScopeID, cur.CreatedAt
	sc, ok := s.dhcpScopeFor(w, r, res.ScopeID)
	if !ok {
		return
	}
	if err := store.ValidateReservation(res, sc, all); err != nil {
		dhcpInvalid(w, err)
		return
	}
	res.ModifiedAt = time.Now().UnixMilli()
	if err := s.deps.Store.DHCP().UpdateReservation(r.Context(), res); err != nil {
		storeErrDup(w, err, "that mac or address is already reserved in this scope")
		return
	}
	s.applyDHCP(r)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleDHCPReservationDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		errJSON(w, http.StatusBadRequest, "bad id")
		return
	}
	if err := s.deps.Store.DHCP().DeleteReservation(r.Context(), id); err != nil {
		storeErr(w, err)
		return
	}
	s.applyDHCP(r)
	w.WriteHeader(http.StatusNoContent)
}

// leaseRow is one entry of the lease table as §8.3 reports it. dhcp.Table
// carries netip.Addr and time.Time, which are this API's string and unix-ms
// spellings once they are on the wire.
//
// expires_at is 0 for a reservation nothing has leased yet: the operator
// pinned the address, and no client has asked for it. That is the one thing
// that tells such a row from a live lease, so it is reported rather than
// hidden behind an omitempty.
type leaseRow struct {
	ScopeID   int64  `json:"scope_id"`
	IP        string `json:"ip"`
	MAC       string `json:"mac"`
	Hostname  string `json:"hostname"`
	ExpiresAt int64  `json:"expires_at"`
	Reserved  bool   `json:"reserved"`
}

func (s *Server) handleDHCPLeases(w http.ResponseWriter, r *http.Request) {
	entries := s.deps.DHCP.Table().All()
	// By address, which is the order the page shows them in and the only
	// one that is stable across polls: the engine pages its lease database
	// in whatever order it holds it.
	slices.SortFunc(entries, func(a, b dhcp.LeaseEntry) int { return a.IP.Compare(b.IP) })
	rows := make([]leaseRow, 0, len(entries))
	for _, e := range entries {
		row := leaseRow{
			ScopeID: e.ScopeID, IP: e.IP.String(), MAC: e.MAC,
			Hostname: e.Hostname, Reserved: e.Reserved,
		}
		if !e.ExpiresAt.IsZero() {
			row.ExpiresAt = e.ExpiresAt.UnixMilli()
		}
		rows = append(rows, row)
	}
	writeJSON(w, http.StatusOK, rows)
}

// leaseAddr is the address in the URL of the two lease routes.
func leaseAddr(w http.ResponseWriter, r *http.Request) (netip.Addr, bool) {
	ip, err := netip.ParseAddr(r.PathValue("ip"))
	if err != nil {
		errJSON(w, http.StatusBadRequest, "not an ip address")
		return netip.Addr{}, false
	}
	return ip.Unmap(), true
}

func (s *Server) handleDHCPLeaseDelete(w http.ResponseWriter, r *http.Request) {
	ip, ok := leaseAddr(w, r)
	if !ok {
		return
	}
	err := s.deps.DHCP.Release(r.Context(), ip)
	var rejected *dhcp.RejectedError
	switch {
	case errors.Is(err, dhcp.ErrUnreachable):
		errJSON(w, http.StatusServiceUnavailable, "the dhcp engine is unreachable")
	case errors.As(err, &rejected):
		// The engine answered, with a no, and the only no lease4-del gives
		// in practice is "no such lease" — a row the table still showed and
		// the engine has since reclaimed. Kea's own words, as every other
		// refusal is reported (§5.1).
		errJSON(w, http.StatusNotFound, rejected.Text)
	case err != nil:
		errJSON(w, http.StatusServiceUnavailable, err.Error())
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *Server) handleDHCPLeaseReserve(w http.ResponseWriter, r *http.Request) {
	ip, ok := leaseAddr(w, r)
	if !ok {
		return
	}
	e, held := s.deps.DHCP.Table().ByIP(ip)
	if !held {
		errJSON(w, http.StatusNotFound, "no lease holds that address")
		return
	}
	sc, ok := s.dhcpScopeFor(w, r, e.ScopeID)
	if !ok {
		return
	}
	others, ok := s.dhcpReservations(w, r)
	if !ok {
		return
	}
	res := store.Reservation{
		ScopeID: sc.ID, MAC: e.MAC, IP: e.IP.String(),
		// The client's own name, sanitised the way the DNS stage sanitises
		// it (§8.1): what a reservation may hold is an RFC 1123 label, and a
		// device that called itself "Anna's iPad" would otherwise make this
		// button answer 400 with a complaint about a name nobody typed.
		Hostname: dhcp.SanitizeLabel(e.Hostname),
	}
	if err := store.ValidateReservation(res, sc, others); err != nil {
		dhcpInvalid(w, err)
		return
	}
	id, ok := s.addReservation(w, r, res)
	if !ok {
		return
	}
	res.ID = id
	s.applyDHCP(r)
	created(w, resourceURL("dhcp/reservations", id), res)
}

// dhcpStatusView is §8.3's status object: dhcp.Status in this API's
// spelling, with "enabled" false and nothing else on a box with no engine.
type dhcpStatusView struct {
	Enabled         bool             `json:"enabled"`
	Engine          string           `json:"engine,omitempty"`
	EngineVersion   string           `json:"engine_version,omitempty"`
	Message         string           `json:"message,omitempty"`
	TableAgeSeconds int64            `json:"table_age_seconds"`
	HA              *dhcpHAView      `json:"ha,omitempty"`
	Scopes          []dhcpScopeUsage `json:"scopes"`
}

// dhcpHAView is what status-get reports about the pair (§7.2), and nil on a
// single box — which is a different fact from a pair that is not talking.
type dhcpHAView struct {
	Mode       string `json:"mode"`
	LocalState string `json:"local_state"`
	// Peer is what the partner calls itself, as the engine that is talking
	// to it reports the name — the "<peer>" in the Scopes page's "Engine
	// 2.6.3 · hot-standby with <peer> · hot-standby". Absent on an engine
	// whose status-get does not carry it, which the line has to handle
	// anyway for a box with no partner at all.
	Peer                     string `json:"peer,omitempty"`
	RemoteState              string `json:"remote_state"`
	CommunicationInterrupted bool   `json:"communication_interrupted"`
	UnackedClients           int    `json:"unacked_clients"`
}

// dhcpScopeUsage is how full one scope's pool is. leased may exceed
// pool_size after a pool is shrunk: the engine keeps the leases it has
// already handed out until they expire (§10).
type dhcpScopeUsage struct {
	ID       int64 `json:"id"`
	PoolSize int   `json:"pool_size"`
	Leased   int   `json:"leased"`
}

// dhcpStatus is the whole of what this instance knows about its engine, and
// the only DHCP answer a box with no engine gives.
func (s *Server) dhcpStatus() dhcpStatusView {
	out := dhcpStatusView{Scopes: []dhcpScopeUsage{}}
	if s.deps.DHCP == nil {
		return out
	}
	st := s.deps.DHCP.Status()
	out.Enabled = st.Enabled
	out.Engine, out.EngineVersion, out.Message = st.Engine, st.EngineVersion, st.Message
	out.TableAgeSeconds = st.TableAgeSeconds
	if st.HA != nil {
		out.HA = &dhcpHAView{
			Mode: st.HA.Mode, LocalState: st.HA.LocalState,
			Peer: st.HA.RemoteName, RemoteState: st.HA.RemoteState,
			CommunicationInterrupted: st.HA.CommunicationInterrupted,
			UnackedClients:           st.HA.UnackedClients,
		}
	}
	for _, u := range st.Scopes {
		out.Scopes = append(out.Scopes, dhcpScopeUsage{ID: u.ID, PoolSize: u.PoolSize, Leased: u.Leased})
	}
	return out
}

func (s *Server) handleDHCPStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.dhcpStatus())
}

// handleDHCPApply is the Scopes page's "Apply again" (§5.1): nothing
// retries a refused configuration on a timer, so this is how an operator
// re-sends one after fixing whatever the engine complained about — a hook
// library installed, a socket permission corrected.
//
// It answers the status object rather than 204, and 200 whatever the engine
// said: the message the render produced is the answer, and reading it out
// of a second request would show the state before this one landed.
func (s *Server) handleDHCPApply(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), dhcpApplyTimeout)
	defer cancel()
	if err := s.deps.DHCP.Apply(ctx); err != nil {
		slog.Warn("re-applying the dhcp configuration failed", "err", err)
	}
	writeJSON(w, http.StatusOK, s.dhcpStatus())
}
