package api

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/aloks98/dnsaur/internal/store"
	"github.com/aloks98/dnsaur/internal/zones"
	"github.com/miekg/dns"
)

func (s *Server) zonesRoutes() {
	s.route("GET /api/v1/zones", s.requireAuth(s.handleZonesList))
	s.route("POST /api/v1/zones", s.requireAuth(s.handleZoneCreate))
	s.route("GET /api/v1/zones/{id}", s.requireAuth(s.handleZoneGet))
	s.route("PATCH /api/v1/zones/{id}", s.requireAuth(s.handleZonePatch))
	s.route("DELETE /api/v1/zones/{id}", s.requireAuth(s.handleZoneDelete))
	s.route("POST /api/v1/zones/{id}/refresh", s.requireAuth(s.handleZoneRefresh))
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

// The two zone types this API can create. Still narrower than the schema,
// which has allowed secondary | stub | forwarder | internal since Milestone
// A: internal/zones/answer.go treats forwarder and stub as non-answering and
// nothing populates them, and internal is reserved for the RFC 6303
// built-ins seeded at migration. The rule is unchanged from Milestone A —
// the API refuses any type it cannot serve correctly, because a type that
// cannot be created cannot misbehave — and what changed is that secondary is
// now one it can: Milestone D2 gives it a transfer to fill it from.
//
// A secondary is only servable because it is *configured*, which is what
// zoneCreate.Primaries and zoneCreate.TSIGKeyID are for and why they are
// validated rather than merely stored.
const (
	zoneTypePrimary   = "primary"
	zoneTypeSecondary = "secondary"
)

// checkZoneTransferConfig validates the (type, primaries, tsig_key_id)
// triple as the zone would be stored, for create and patch alike — a rule
// enforced on POST and not on PATCH is a rule with a way around it. It
// returns the status code and message to answer with, or 0 when the
// configuration is sound.
func (s *Server) checkZoneTransferConfig(ctx context.Context, zoneType, primaries string, tsigKeyID int64) (int, string) {
	if zoneType == zoneTypeSecondary {
		// Syntax only, deliberately: zones.ValidatePrimaries does not
		// resolve, so a primary named by hostname is stored as written and
		// looked up at transfer time. See internal/zones/primaries.go.
		if err := zones.ValidatePrimaries(primaries); err != nil {
			return http.StatusBadRequest, "primaries: " + err.Error()
		}
	} else {
		// primaries and tsig_key_id describe a transfer, and a zone that
		// never transfers has none. Storing them anyway would leave
		// configuration nothing reads, shown by the UI as though it meant
		// something.
		if primaries != "" {
			return http.StatusBadRequest, "primaries applies to secondary zones only"
		}
		if tsigKeyID != 0 {
			return http.StatusBadRequest, "tsig_key_id applies to secondary zones only"
		}
	}
	if tsigKeyID != 0 {
		// zones.tsig_key_id carries no foreign key — see the 0009 migration
		// for why — so this is where a reference to a key that does not
		// exist is caught. The other half of the same rule is
		// tsigKeyStore.Delete, which refuses to remove a key a zone names.
		if _, found, err := s.deps.Store.TSIGKeys().Get(ctx, tsigKeyID); err != nil {
			return http.StatusServiceUnavailable, "storage unavailable"
		} else if !found {
			return http.StatusBadRequest, "tsig_key_id does not name an existing TSIG key"
		}
	}
	return 0, ""
}

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
	// Type omitted means primary. primary and secondary are the two this
	// API creates — see zoneTypePrimary.
	Type string `json:"type"`
	// Primaries is where a secondary pulls from: a comma-separated list of
	// host[:port], port defaulting to 53. Required for a secondary, refused
	// on any other type, and stored exactly as written — see
	// internal/zones/primaries.go.
	Primaries string `json:"primaries"`
	// TSIGKeyID names the key a secondary signs its transfer requests with.
	// Optional (0 means the transfer is unsigned), but when set it must name
	// a key that exists.
	TSIGKeyID int64 `json:"tsig_key_id"`
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
	} else if zoneType != zoneTypePrimary && zoneType != zoneTypeSecondary {
		errJSON(w, http.StatusBadRequest, "only primary and secondary zones are supported")
		return
	}
	if code, msg := s.checkZoneTransferConfig(r.Context(), zoneType, body.Primaries, body.TSIGKeyID); code != 0 {
		errJSON(w, code, msg)
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
		SOATTL: defaultSOATTL,
		// Empty and 0 for a primary, both already checked by
		// checkZoneTransferConfig above.
		Primaries:  body.Primaries,
		TSIGKeyID:  body.TSIGKeyID,
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
	//
	// Primary zones only. A secondary's contents are its primary's, arriving
	// whole on the first transfer and replacing whatever is there; seeding
	// an NS record here would be dnsaur authoring data in a zone it does not
	// own, and serving it as authoritative in the window before that
	// transfer lands.
	if zoneType == zoneTypePrimary {
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

	// The transfer configuration, editable for the same reason type is: a
	// primary that becomes a secondary needs somewhere to pull from in the
	// same request, and a secondary whose primary moves needs to be able to
	// say so. Validated against the zone as it will be — see
	// checkZoneTransferConfig.
	Primaries *string `json:"primaries"`
	TSIGKeyID *int64  `json:"tsig_key_id"`
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
	if body.Type != nil && *body.Type != zoneTypePrimary && *body.Type != zoneTypeSecondary {
		errJSON(w, http.StatusBadRequest, "only primary and secondary zones are supported")
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
	if body.Primaries != nil {
		z.Primaries = *body.Primaries
	}
	if body.TSIGKeyID != nil {
		z.TSIGKeyID = *body.TSIGKeyID
	}
	// Checked on the merged zone rather than on the body: a patch that sets
	// type without primaries, or clears primaries without changing type,
	// leaves a secondary with nowhere to pull from either way, and only the
	// result says which.
	if code, msg := s.checkZoneTransferConfig(r.Context(), z.Type, z.Primaries, z.TSIGKeyID); code != 0 {
		errJSON(w, code, msg)
		return
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

// zoneRefreshResult is what a completed manual transfer reports back.
//
// Everything here is also readable from the zone row afterwards except
// `primary` and `records`, and those two are the reason the body exists: a
// zone with three primaries says nothing about which one answered, and the
// record count is what tells an operator the transfer actually carried a
// zone rather than an empty one.
type zoneRefreshResult struct {
	Primary     string `json:"primary"`
	Serial      uint32 `json:"serial"`
	Records     int    `json:"records"`
	RefreshedAt int64  `json:"refreshed_at"`
	ExpiresAt   int64  `json:"expires_at"`
}

// handleZoneRefresh transfers one secondary zone now, whatever its schedule
// says, and answers only once the transfer has finished. That is deliberately
// synchronous: the caller pressed a button to find out whether the transfer
// works, and a 202 would hand back "started" — which is the one thing they
// already knew — leaving the answer (and the error, which is the whole point)
// nowhere to be read. A transfer takes as long as one TCP conversation with
// the primary, bounded by the Transferrer's own timeouts.
//
// A failed transfer is a 502, not a 500: nothing here is broken, a server
// this one depends on refused or could not be reached, and the message names
// every primary that was tried. The zone is left exactly as it was — a failed
// transfer changes nothing (see zones.Transferrer.Transfer) — so a 502 here
// means "still serving what it had", never "half-applied".
func (s *Server) handleZoneRefresh(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		errJSON(w, http.StatusBadRequest, "bad id")
		return
	}
	// Nil in a server built without the scheduler (every test server that has
	// no business opening TCP connections to a primary). 503 rather than a
	// panic, and rather than a 404 that would read as "no such zone".
	if s.deps.ZoneRefresher == nil {
		errJSON(w, http.StatusServiceUnavailable, "zone transfers are not running")
		return
	}
	z, err := s.deps.Store.Zones().Zone(r.Context(), id)
	if err != nil {
		storeErr(w, err)
		return
	}
	// Only a secondary is a copy of someone else's zone. Asking a primary to
	// transfer is not a failure to report against a primary — there is
	// nowhere for it to pull from — so it is refused here rather than left to
	// come back as a confusing 502 from Transfer's own type check.
	if !strings.EqualFold(z.Type, zoneTypeSecondary) {
		errJSON(w, http.StatusBadRequest, "only secondary zones are transferred")
		return
	}
	res, err := s.deps.ZoneRefresher.Refresh(r.Context(), id)
	if err != nil {
		// The error is passed through verbatim. It is the only account of why
		// this transfer failed that the dashboard will ever see — the
		// scheduler's own record of a failure is process-local (see
		// zones.Refresher.Status) — and rewriting "dial tcp 203.0.113.9:53:
		// connect: connection refused" into "couldn't transfer" would throw
		// away the whole of what makes it actionable.
		errJSON(w, http.StatusBadGateway, err.Error())
		return
	}
	// The transfer already republished the served snapshot itself (the
	// Transferrer is built WithReload), so there is no reloadZones here —
	// unlike every other write in this file.
	writeJSON(w, http.StatusOK, zoneRefreshResult{
		Primary:     res.Primary.String(),
		Serial:      res.Serial,
		Records:     res.Records,
		RefreshedAt: res.RefreshedAt,
		ExpiresAt:   res.ExpiresAt,
	})
}
