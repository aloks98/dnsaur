package api

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/aloks98/dnsaur/internal/store"
	"github.com/aloks98/dnsaur/internal/zones"
)

func (s *Server) zoneRecordsRoutes() {
	s.route("GET /api/v1/zones/{id}/records", s.requireAuth(s.handleZoneRecordsList))
	s.route("POST /api/v1/zones/{id}/records", s.requireAuth(s.handleZoneRecordCreate))
	s.route("PUT /api/v1/zones/{id}/records/{rid}", s.requireAuth(s.handleZoneRecordUpdate))
	s.route("DELETE /api/v1/zones/{id}/records/{rid}", s.requireAuth(s.handleZoneRecordDelete))
}

// pathRID reads the {rid} path segment used by the record-scoped routes,
// mirroring pathID's {id} handling (see clients_handlers.go).
func pathRID(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("rid"), 10, 64)
	return id, err == nil && id > 0
}

// zoneRecordWrite is the request body for both creating and replacing a
// zone record.
type zoneRecordWrite struct {
	Name  string `json:"name"`
	Type  string `json:"type"`
	TTL   uint32 `json:"ttl"`
	RData string `json:"rdata"`
	// Enabled is a pointer so "not sent" differs from "false" — mirrors
	// zoneCreate's pattern (see zones_handlers.go).
	Enabled *bool  `json:"enabled"`
	Comment string `json:"comment"`
}

// recordWriteRefusal reports why zone's records may not be written through
// the API, or "" when they may. Reads are never refused by it — a built-in,
// a secondary and a stub are all listable and exportable.
//
// Three zone types own their contents somewhere other than here:
//
//   - internal is seeded infrastructure (RFC 6303, internal/store/builtins.go),
//     authored by a migration.
//   - secondary is its primary's. A record written into one is not merged
//     with what the next transfer sends and is not preserved by it — the
//     transfer is a whole-zone replace (Transferrer.install), so the write
//     is deleted at the next refresh. Until then the server answers it
//     authoritatively, which is the part that makes silently accepting it
//     worse than refusing it: the operator is told nothing, and the zone
//     serves an answer that disagrees with its primary.
//   - stub is its master's, on the same terms and by the same mechanism:
//     StubFetcher.Fetch installs what comes back through the very same
//     DiffRecords, so a hand write is deleted on the SOA's own schedule with
//     nothing to say so. It differs from the secondary in what the write does
//     while it survives, and the difference is worse rather than milder. A
//     stub answers from none of its records; StubUpstreams reads them back to
//     rebuild the conditional routing table on every zone reload, so a
//     hand-written apex NS record silently redirects the whole claimed suffix
//     until the next fetch undoes it.
//
// A forwarder is deliberately not among them. Nothing overwrites its records
// — it has no master, its routing comes from forward_to, and no scheduled job
// touches its rows — so the rule this function encodes ("authored elsewhere,
// and this write will be destroyed") is not true of one. A record written
// into a forwarder is inert rather than lost, which is a different complaint
// and not a 409's to make.
//
// 409 rather than 403 or 405: the request is well-formed and the caller is
// permitted, and the same route accepts it for another zone. What refuses it
// is the state of this zone — which is what 409 means, and what the built-in
// refusal has always answered.
func recordWriteRefusal(zone store.Zone) string {
	switch strings.ToLower(zone.Type) {
	case "internal":
		return "built-in zones cannot be changed"
	case "secondary":
		return "a secondary zone's records come from its primary; change them there"
	case "stub":
		// Fetched, never transferred. A stub asks its master two ordinary
		// questions — SOA and NS with glue — precisely so it needs no
		// allow_transfer permission on the far end, and that is the whole of
		// what distinguishes it from a secondary. A message naming a transfer
		// would send the operator looking for a mechanism that does not run
		// here; zoneTSIGKey and handleZoneRefresh both had to have the same
		// word taken out of them this milestone.
		return "a stub zone's records are the NS set it fetches from its master; change them there"
	}
	return ""
}

// buildZoneRecord is the HTTP face of zones.BuildRecord: it validates body
// against zone and its existing records and returns the store.ZoneRecord
// ready to write, or the status and message to answer a refusal with.
//
// The rules themselves live in internal/zones (record.go) because the AXFR
// transfer has to enforce exactly these and cannot import this package. All
// that is left here is the mapping from a refusal's kind to a status code:
// a value that is wrong on its own terms is 400, a record that cannot
// coexist with one already in the zone is 409. See
// docs/superpowers/specs/2026-08-08-zones-design.md §6.
func buildZoneRecord(zone store.Zone, body zoneRecordWrite, existing []store.ZoneRecord, selfID int64, hasSelf bool) (store.ZoneRecord, int, string, bool) {
	rec, err := zones.BuildRecord(zone, zones.RecordWrite{
		Name:    body.Name,
		Type:    body.Type,
		TTL:     body.TTL,
		RData:   body.RData,
		Enabled: body.Enabled,
		Comment: body.Comment,
	}, existing, selfID, hasSelf)
	if err == nil {
		return rec, 0, "", true
	}
	// Anything that is not a *RecordProblem would be a bug in BuildRecord
	// rather than a caller error, so it falls to 400 with its own message
	// rather than being swallowed: the alternative is a 500 that says
	// nothing about a record the caller can see.
	code := http.StatusBadRequest
	var problem *zones.RecordProblem
	if errors.As(err, &problem) && problem.Conflict {
		code = http.StatusConflict
	}
	return store.ZoneRecord{}, code, err.Error(), false
}

// bumpZoneSerial increments the zone's SOA serial after a record mutation.
// Failures are logged, not surfaced (same reasoning as reloadZones): the
// record write already committed, and the reload that follows this call
// serves the new record correctly regardless — only the advertised serial
// would lag by one until the zone's next successful write.
func (s *Server) bumpZoneSerial(r *http.Request, zoneID int64) {
	if err := s.deps.Store.Zones().BumpSerial(r.Context(), zoneID); err != nil {
		slog.Error("bumping zone serial after record write failed", "zone", zoneID, "err", err)
	}
}

// findRecord returns the record with this id from existing — used to check a
// {rid} path segment actually belongs to the zone in the {id} segment
// before an update or delete touches it, since UpdateRecord/DeleteRecord
// key on record id alone and don't themselves check zone_id.
//
// It hands back the row rather than just reporting that it exists because
// auto-PTR needs the pre-write record: on update and delete, the address the
// record used to carry is the only way to find the PTR that has to move or
// go with it.
func findRecord(existing []store.ZoneRecord, id int64) (store.ZoneRecord, bool) {
	for _, e := range existing {
		if e.ID == id {
			return e, true
		}
	}
	return store.ZoneRecord{}, false
}

func (s *Server) handleZoneRecordsList(w http.ResponseWriter, r *http.Request) {
	zid, ok := pathID(r)
	if !ok {
		errJSON(w, http.StatusBadRequest, "bad id")
		return
	}
	if _, err := s.deps.Store.Zones().Zone(r.Context(), zid); err != nil {
		storeErr(w, err)
		return
	}
	recs, err := s.deps.Store.Zones().Records(r.Context(), zid)
	if err != nil {
		storeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, recs)
}

func (s *Server) handleZoneRecordCreate(w http.ResponseWriter, r *http.Request) {
	zid, ok := pathID(r)
	if !ok {
		errJSON(w, http.StatusBadRequest, "bad id")
		return
	}
	zone, err := s.deps.Store.Zones().Zone(r.Context(), zid)
	if err != nil {
		storeErr(w, err)
		return
	}
	if msg := recordWriteRefusal(zone); msg != "" {
		errJSON(w, http.StatusConflict, msg)
		return
	}
	body, ok := decodeOr400[zoneRecordWrite](w, r)
	if !ok {
		return
	}
	existing, err := s.deps.Store.Zones().Records(r.Context(), zid)
	if err != nil {
		storeErr(w, err)
		return
	}
	rec, code, msg, ok := buildZoneRecord(zone, body, existing, 0, false)
	if !ok {
		errJSON(w, code, msg)
		return
	}
	id, err := s.deps.Store.Zones().AddRecord(r.Context(), rec)
	if err != nil {
		storeErr(w, err)
		return
	}
	s.bumpZoneSerial(r, zid)
	// Before the reload, not after: the reload below then publishes the
	// forward record and its PTR together, in one snapshot rebuild, so the
	// reverse answer is live by the time this request is answered.
	s.syncPTR(r.Context(), nil, &rec, zone.Name)
	s.reloadZones(r)
	s.notifyZones()
	rec.ID = id
	created(w, resourceURL("zones/"+strconv.FormatInt(zid, 10)+"/records", id), rec)
}

func (s *Server) handleZoneRecordUpdate(w http.ResponseWriter, r *http.Request) {
	zid, ok := pathID(r)
	if !ok {
		errJSON(w, http.StatusBadRequest, "bad id")
		return
	}
	rid, ok := pathRID(r)
	if !ok {
		errJSON(w, http.StatusBadRequest, "bad id")
		return
	}
	zone, err := s.deps.Store.Zones().Zone(r.Context(), zid)
	if err != nil {
		storeErr(w, err)
		return
	}
	if msg := recordWriteRefusal(zone); msg != "" {
		errJSON(w, http.StatusConflict, msg)
		return
	}
	body, ok := decodeOr400[zoneRecordWrite](w, r)
	if !ok {
		return
	}
	existing, err := s.deps.Store.Zones().Records(r.Context(), zid)
	if err != nil {
		storeErr(w, err)
		return
	}
	old, ok := findRecord(existing, rid)
	if !ok {
		errJSON(w, http.StatusNotFound, "not found")
		return
	}
	rec, code, msg, ok := buildZoneRecord(zone, body, existing, rid, true)
	if !ok {
		errJSON(w, code, msg)
		return
	}
	rec.ID = rid
	if err := s.deps.Store.Zones().UpdateRecord(r.Context(), rec); err != nil {
		storeErr(w, err)
		return
	}
	s.bumpZoneSerial(r, zid)
	// old carries the address the record used to have, which is the only
	// way to find the PTR this write has to move. See handleZoneRecordCreate
	// for why this runs before the reload.
	s.syncPTR(r.Context(), &old, &rec, zone.Name)
	s.reloadZones(r)
	s.notifyZones()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleZoneRecordDelete(w http.ResponseWriter, r *http.Request) {
	zid, ok := pathID(r)
	if !ok {
		errJSON(w, http.StatusBadRequest, "bad id")
		return
	}
	rid, ok := pathRID(r)
	if !ok {
		errJSON(w, http.StatusBadRequest, "bad id")
		return
	}
	zone, err := s.deps.Store.Zones().Zone(r.Context(), zid)
	if err != nil {
		storeErr(w, err)
		return
	}
	if msg := recordWriteRefusal(zone); msg != "" {
		errJSON(w, http.StatusConflict, msg)
		return
	}
	existing, err := s.deps.Store.Zones().Records(r.Context(), zid)
	if err != nil {
		storeErr(w, err)
		return
	}
	old, ok := findRecord(existing, rid)
	if !ok {
		errJSON(w, http.StatusNotFound, "not found")
		return
	}
	if err := s.deps.Store.Zones().DeleteRecord(r.Context(), rid); err != nil {
		storeErr(w, err)
		return
	}
	s.bumpZoneSerial(r, zid)
	// The record is gone, so its PTR goes with it — old is the only copy of
	// the address left. See handleZoneRecordCreate for why this runs before
	// the reload.
	s.syncPTR(r.Context(), &old, nil, zone.Name)
	s.reloadZones(r)
	s.notifyZones()
	w.WriteHeader(http.StatusNoContent)
}
