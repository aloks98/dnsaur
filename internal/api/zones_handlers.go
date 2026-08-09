package api

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/aloks98/dnsaur/internal/store"
	"github.com/miekg/dns"
)

func (s *Server) zonesRoutes() {
	s.mux.HandleFunc("GET /api/v1/zones", s.requireAuth(s.handleZonesList))
	s.mux.HandleFunc("POST /api/v1/zones", s.requireAuth(s.handleZoneCreate))
	s.mux.HandleFunc("GET /api/v1/zones/{id}", s.requireAuth(s.handleZoneGet))
	s.mux.HandleFunc("PATCH /api/v1/zones/{id}", s.requireAuth(s.handleZonePatch))
	s.mux.HandleFunc("DELETE /api/v1/zones/{id}", s.requireAuth(s.handleZoneDelete))
}

// reloadZones is called after successful zone mutations; failures are
// logged, never surfaced — the write already committed. It runs with a
// context stripped of cancellation, same reasoning as reloadClients: a
// client disconnecting mid-request must not abort the reload.
func (s *Server) reloadZones(r *http.Request) {
	if err := s.deps.Reloader.ReloadZones(context.WithoutCancel(r.Context())); err != nil {
		slog.Error("zone reload after api write failed", "err", err)
	}
}

// apexNSTTL is the TTL given to the apex NS record every new zone is seeded
// with (RFC 2181 §10.1, see handleZoneCreate). It matches the TTL convention
// used elsewhere for generated/example zone_records rows.
const apexNSTTL = 3600

// defaultSOATTL is the SOA record's own header TTL, fixed rather than
// client-settable in Milestone A (zoneCreate/zonePatch have no soa_ttl
// field). store.AddZone binds soa_ttl explicitly on every insert, so leaving
// it unset would write a literal 0 instead of falling back to the schema's
// DEFAULT 900 — and RFC 2308 §5 makes a negative answer's TTL
// min(SOAMinimum, SOATTL), so a zero here would make every NXDOMAIN this
// zone hands out uncacheable, forever re-querying us on every miss.
const defaultSOATTL uint32 = 900

// zoneTypePrimary is the only zone type Milestone A can create. This is
// deliberately narrower than the schema (which already allows secondary |
// stub | forwarder | internal for later milestones): internal/zones/answer.go
// only special-cases "forwarder" and "stub" as non-answering, so a
// "secondary" zone today would be served exactly like a primary — except a
// secondary has no transfer mechanism until Milestone D, so it holds only
// its apex NS record and every other name under it comes back an
// authoritative NXDOMAIN. That silently takes a domain offline instead of
// refusing to create it. Rather than teach the resolver to treat an
// untransferable secondary as inert, the API refuses to create (or patch a
// zone into) any type it cannot yet serve correctly — a type that cannot be
// created cannot misbehave. Widen this once Milestone D adds transfers.
const zoneTypePrimary = "primary"

// normalizeZoneName lowercases name, strips a trailing dot, and validates
// it. dns.IsDomainName gives RFC 1035 §2.3.4 (label <= 63 octets, name <=
// 255 octets) but documents itself as "extremely liberal — almost any
// string is a valid domain name", so it alone would accept "not a domain".
// The extra checks here catch what it deliberately doesn't: an empty label
// (e.g. "e412..in") and whitespace/path characters that never appear in a
// real hostname.
func normalizeZoneName(raw string) (string, bool) {
	name := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(raw), "."))
	if name == "" || strings.ContainsAny(name, " \t\r\n/\\") {
		return "", false
	}
	for _, label := range strings.Split(name, ".") {
		if label == "" {
			return "", false
		}
	}
	if _, ok := dns.IsDomainName(name); !ok {
		return "", false
	}
	return name, true
}

func (s *Server) handleZonesList(w http.ResponseWriter, r *http.Request) {
	zs, err := s.deps.Store.Zones().Zones(r.Context())
	if err != nil {
		storeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, zs)
}

func (s *Server) handleZoneGet(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		errJSON(w, http.StatusBadRequest, "bad id")
		return
	}
	z, err := s.deps.Store.Zones().Zone(r.Context(), id)
	if err != nil {
		storeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, z)
}

// zoneCreate mirrors groupCreate's pointer-for-optional pattern (see
// clients_handlers.go).
type zoneCreate struct {
	Name string `json:"name"`
	// Type omitted means primary — the only type Milestone A can serve.
	Type string `json:"type"`
	// Enabled is a pointer so "not sent" differs from "false".
	Enabled *bool `json:"enabled"`
	// SOA fields, all optional: omitted means the generated default, so a
	// zone can be created from a name alone.
	SOANS      string `json:"soa_ns"`
	SOAMbox    string `json:"soa_mbox"`
	SOARefresh uint32 `json:"soa_refresh"`
	SOARetry   uint32 `json:"soa_retry"`
	SOAExpire  uint32 `json:"soa_expire"`
	SOAMinimum uint32 `json:"soa_minimum"`
}

func (s *Server) handleZoneCreate(w http.ResponseWriter, r *http.Request) {
	body, err := decode[zoneCreate](r)
	if err != nil {
		errJSON(w, http.StatusBadRequest, "invalid json")
		return
	}
	name, ok := normalizeZoneName(body.Name)
	if !ok {
		errJSON(w, http.StatusBadRequest, "name must be a valid domain name")
		return
	}

	zoneType := body.Type
	if zoneType == "" {
		zoneType = zoneTypePrimary
	} else if zoneType != zoneTypePrimary {
		errJSON(w, http.StatusBadRequest, "only primary zones are supported")
		return
	}
	enabled := true
	if body.Enabled != nil {
		enabled = *body.Enabled
	}

	soaNS := body.SOANS
	if soaNS == "" {
		soaNS = "ns." + name
	}
	soaMbox := body.SOAMbox
	if soaMbox == "" {
		soaMbox = "hostadmin." + name
	}
	soaRefresh := body.SOARefresh
	if soaRefresh == 0 {
		soaRefresh = 900
	}
	soaRetry := body.SOARetry
	if soaRetry == 0 {
		soaRetry = 300
	}
	soaExpire := body.SOAExpire
	if soaExpire == 0 {
		soaExpire = 604800
	}
	soaMinimum := body.SOAMinimum
	if soaMinimum == 0 {
		soaMinimum = 900
	}

	now := time.Now().UnixMilli()
	id, err := s.deps.Store.Zones().AddZone(r.Context(), store.Zone{
		Name:       name,
		Type:       zoneType,
		Enabled:    enabled,
		SOANS:      soaNS,
		SOAMbox:    soaMbox,
		SOASerial:  1,
		SOARefresh: soaRefresh,
		SOARetry:   soaRetry,
		SOAExpire:  soaExpire,
		SOAMinimum: soaMinimum,
		// Fixed, not client-settable — see defaultSOATTL.
		SOATTL:     defaultSOATTL,
		CreatedAt:  now,
		ModifiedAt: now,
	})
	if err != nil {
		storeErrDup(w, err, "a zone with that name already exists")
		return
	}

	// RFC 2181 §10.1: a zone's apex must have NS records, or the zone is
	// malformed from the moment it exists — every future zone-file export
	// and transfer would carry the defect outward. Best-effort like
	// handleGroupCreate's list assignment below it: the zone itself already
	// committed, so a failure here is logged rather than turned into a
	// response the caller can't reconcile with the id it was just handed.
	if _, err := s.deps.Store.Zones().AddRecord(r.Context(), store.ZoneRecord{
		ZoneID:  id,
		Name:    "@",
		Type:    "NS",
		TTL:     apexNSTTL,
		RData:   dns.Fqdn(soaNS),
		Enabled: true,
	}); err != nil {
		slog.Error("creating apex NS record for new zone failed", "zone", id, "err", err)
	}

	s.reloadZones(r)
	writeJSON(w, http.StatusCreated, map[string]int64{"id": id})
}

// zonePatch is groupPatch's pointer-per-field pattern extended to every zone
// property Milestone A allows editing. soa_ttl has no field here for the
// same reason it has none in zoneCreate — see defaultSOATTL.
type zonePatch struct {
	Name    *string `json:"name"`
	Type    *string `json:"type"`
	Enabled *bool   `json:"enabled"`

	SOANS      *string `json:"soa_ns"`
	SOAMbox    *string `json:"soa_mbox"`
	SOARefresh *uint32 `json:"soa_refresh"`
	SOARetry   *uint32 `json:"soa_retry"`
	SOAExpire  *uint32 `json:"soa_expire"`
	SOAMinimum *uint32 `json:"soa_minimum"`
}

func (s *Server) handleZonePatch(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		errJSON(w, http.StatusBadRequest, "bad id")
		return
	}
	body, err := decode[zonePatch](r)
	if err != nil {
		errJSON(w, http.StatusBadRequest, "invalid json")
		return
	}
	if body.Type != nil && *body.Type != zoneTypePrimary {
		errJSON(w, http.StatusBadRequest, "only primary zones are supported")
		return
	}

	z, err := s.deps.Store.Zones().Zone(r.Context(), id)
	if err != nil {
		storeErr(w, err)
		return
	}
	// A built-in zone is seeded infrastructure (RFC 6303), not user content —
	// see internal/store/builtins.go. Reads are fine; writes are not.
	if z.Type == "internal" {
		errJSON(w, http.StatusConflict, "built-in zones cannot be changed")
		return
	}

	if body.Name != nil {
		name, ok := normalizeZoneName(*body.Name)
		if !ok {
			errJSON(w, http.StatusBadRequest, "name must be a valid domain name")
			return
		}
		z.Name = name
	}
	if body.Type != nil {
		z.Type = *body.Type
	}
	if body.Enabled != nil {
		z.Enabled = *body.Enabled
	}
	if body.SOANS != nil {
		z.SOANS = *body.SOANS
	}
	if body.SOAMbox != nil {
		z.SOAMbox = *body.SOAMbox
	}
	if body.SOARefresh != nil {
		z.SOARefresh = *body.SOARefresh
	}
	if body.SOARetry != nil {
		z.SOARetry = *body.SOARetry
	}
	if body.SOAExpire != nil {
		z.SOAExpire = *body.SOAExpire
	}
	if body.SOAMinimum != nil {
		z.SOAMinimum = *body.SOAMinimum
	}
	z.ModifiedAt = time.Now().UnixMilli()

	if err := s.deps.Store.Zones().UpdateZone(r.Context(), z); err != nil {
		storeErrDup(w, err, "a zone with that name already exists")
		return
	}
	s.reloadZones(r)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleZoneDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		errJSON(w, http.StatusBadRequest, "bad id")
		return
	}
	z, err := s.deps.Store.Zones().Zone(r.Context(), id)
	if err != nil {
		storeErr(w, err)
		return
	}
	// A built-in zone is seeded infrastructure (RFC 6303), not user content —
	// see internal/store/builtins.go. Reads are fine; writes are not.
	if z.Type == "internal" {
		errJSON(w, http.StatusConflict, "built-in zones cannot be changed")
		return
	}
	// Read the records before the delete takes them: they are the only copy
	// of the addresses whose PTRs this zone owns, and retireZonePTRs needs
	// them after the rows are gone.
	recs, err := s.deps.Store.Zones().Records(r.Context(), id)
	if err != nil {
		storeErr(w, err)
		return
	}
	// zone_records rows for this zone go with it — ON DELETE CASCADE in the
	// 0004 migration (zoneStore.DeleteZone), not application logic here.
	if err := s.deps.Store.Zones().DeleteZone(r.Context(), id); err != nil {
		storeErr(w, err)
		return
	}
	// The cascade stops at this zone's own rows. Every PTR auto-PTR wrote for
	// them sits in a *reverse* zone that survives the delete, so retiring
	// them is application logic and has to happen here — deleting one A
	// record removes its PTR (handleZoneRecordDelete), and deleting the zone
	// that holds it must not be the way to leave the reverse pointing at a
	// name that no longer exists. Before the reload, so the same rebuild
	// serves both halves — see handleZoneRecordCreate.
	s.retireZonePTRs(r.Context(), recs, z.Name)
	s.reloadZones(r)
	w.WriteHeader(http.StatusNoContent)
}
