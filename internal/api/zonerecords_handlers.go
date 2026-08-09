package api

import (
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/aloks98/dnsaur/internal/store"
	"github.com/aloks98/dnsaur/internal/zones"
)

func (s *Server) zoneRecordsRoutes() {
	s.mux.HandleFunc("GET /api/v1/zones/{id}/records", s.requireAuth(s.handleZoneRecordsList))
	s.mux.HandleFunc("POST /api/v1/zones/{id}/records", s.requireAuth(s.handleZoneRecordCreate))
	s.mux.HandleFunc("PUT /api/v1/zones/{id}/records/{rid}", s.requireAuth(s.handleZoneRecordUpdate))
	s.mux.HandleFunc("DELETE /api/v1/zones/{id}/records/{rid}", s.requireAuth(s.handleZoneRecordDelete))
}

// pathRID reads the {rid} path segment used by the record-scoped routes,
// mirroring pathID's {id} handling (see clients_handlers.go).
func pathRID(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("rid"), 10, 64)
	return id, err == nil && id > 0
}

// maxRecordTTL is the largest TTL RFC 2181 §8 allows a record to carry: the
// top of a signed 32-bit range. A resolver reads anything above this as its
// two's-complement wraparound — for a value with the high bit set, that
// wraps to zero, meaning "never cache", the opposite of what was typed.
const maxRecordTTL uint32 = 2147483647

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

// normalizeRecordName lowercases a record name and resolves it to
// zoneName-relative form. "" and the zone apex both fold to "@" — the
// convention zone_records.name and internal/zones already use
// (zones.apexName).
//
// A name that already carries the zone's own apex as a suffix — a fully
// qualified name typed out of habit, e.g. "www.e412.in" (or
// "www.e412.in.") in zone "e412.in" — has that suffix stripped so it lands
// on the same relative name as "www". Left un-stripped, absoluteRecordName
// would silently double it into "www.e412.in.e412.in": it parses (ToRR
// doesn't know any better), so nothing else would catch it.
func normalizeRecordName(raw, zoneName string) string {
	name := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(raw), "."))
	zoneName = strings.ToLower(strings.TrimSuffix(zoneName, "."))
	if name == "" || name == zoneName {
		return "@"
	}
	if suffix := "." + zoneName; strings.HasSuffix(name, suffix) {
		if rel := strings.TrimSuffix(name, suffix); rel != "" {
			return rel
		}
	}
	return name
}

// absoluteRecordName rebuilds a record's owner FQDN from its zone-relative
// name — the same "relative + apex" construction internal/zones/answer.go
// uses when it builds an owner name to hand to ToRR.
func absoluteRecordName(zoneName, relName string) string {
	if relName == "@" {
		return zoneName
	}
	return relName + "." + zoneName
}

// buildZoneRecord validates body against zone and its existing records and
// returns the store.ZoneRecord ready to write. existing is every record
// already in the zone (the full set, not pre-filtered by name); selfID and
// hasSelf identify the record being replaced by a PUT so it is excluded
// from the sibling/RRSet checks below — otherwise replacing a record would
// always conflict with itself.
//
// Check order is part of the contract, not an implementation detail: parse
// (400), TTL range (400), apex CNAME (409), CNAME siblings both directions
// (409), RRSet TTL match (409). See
// docs/superpowers/specs/2026-08-08-zones-design.md §6.
func buildZoneRecord(zone store.Zone, body zoneRecordWrite, existing []store.ZoneRecord, selfID int64, hasSelf bool) (store.ZoneRecord, int, string, bool) {
	name := normalizeRecordName(body.Name, zone.Name)
	recType := strings.ToUpper(strings.TrimSpace(body.Type))
	enabled := true
	if body.Enabled != nil {
		enabled = *body.Enabled
	}
	rec := store.ZoneRecord{
		ZoneID:  zone.ID,
		Name:    name,
		Type:    recType,
		TTL:     body.TTL,
		RData:   body.RData,
		Enabled: enabled,
		Comment: body.Comment,
	}

	// Validation is dns.NewRR itself, via the same ToRR the resolver uses to
	// build the RR it serves. One validator, so an accepted record is by
	// construction a servable one — there is no second copy to drift from
	// the parser.
	if _, err := zones.ToRR(absoluteRecordName(zone.Name, name), rec); err != nil {
		return store.ZoneRecord{}, http.StatusBadRequest, err.Error(), false
	}

	// RFC 2181 §8: reject a TTL a resolver would not read back as typed.
	if rec.TTL > maxRecordTTL {
		return store.ZoneRecord{}, http.StatusBadRequest, "ttl must not exceed 2147483647", false
	}

	// RFC 1912 §2.4: no CNAME at the zone apex. The zone's SOA lives on the
	// zones row, not a zone_records row, so the sibling check below would
	// see an apex with nothing recorded there and miss this on its own.
	if name == "@" && recType == "CNAME" {
		return store.ZoneRecord{}, http.StatusConflict, "CNAME is not allowed at the zone apex", false
	}

	for _, sib := range existing {
		if sib.Name != name || (hasSelf && sib.ID == selfID) {
			continue
		}
		// RFC 1034 §3.6.2: a CNAME must be the only record at its name, in
		// both write orders — a CNAME landing beside an existing record, or
		// a record landing beside an existing CNAME.
		if recType == "CNAME" || sib.Type == "CNAME" {
			return store.ZoneRecord{}, http.StatusConflict, "CNAME cannot coexist with another record at the same name", false
		}
		// RFC 2181 §5.2: every RR in an RRSet (same name, same type) must
		// share one TTL, or the zone answers differently depending on which
		// row a lookup happens to read first.
		if sib.Type == recType && sib.TTL != rec.TTL {
			return store.ZoneRecord{}, http.StatusConflict, "records in the same RRSet must share one TTL", false
		}
	}

	return rec, 0, "", true
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
	// A built-in zone is seeded infrastructure (RFC 6303), not user content —
	// see internal/store/builtins.go. Reads are fine; writes are not.
	if zone.Type == "internal" {
		errJSON(w, http.StatusConflict, "built-in zones cannot be changed")
		return
	}
	body, err := decode[zoneRecordWrite](r)
	if err != nil {
		errJSON(w, http.StatusBadRequest, "invalid json")
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
	writeJSON(w, http.StatusCreated, map[string]int64{"id": id})
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
	// A built-in zone is seeded infrastructure (RFC 6303), not user content —
	// see internal/store/builtins.go. Reads are fine; writes are not.
	if zone.Type == "internal" {
		errJSON(w, http.StatusConflict, "built-in zones cannot be changed")
		return
	}
	body, err := decode[zoneRecordWrite](r)
	if err != nil {
		errJSON(w, http.StatusBadRequest, "invalid json")
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
	// A built-in zone is seeded infrastructure (RFC 6303), not user content —
	// see internal/store/builtins.go. Reads are fine; writes are not.
	if zone.Type == "internal" {
		errJSON(w, http.StatusConflict, "built-in zones cannot be changed")
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
	w.WriteHeader(http.StatusNoContent)
}
