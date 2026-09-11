package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"slices"
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
	s.route("POST /api/v1/zones/{id}/clone", s.requireAuth(s.handleZoneClone))
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

// notifyZones wakes the outbound NOTIFY pass after a zone mutation that may
// have moved a serial. See Notifier.Wake.
func (s *Server) notifyZones() {
	s.deps.Reloader.NotifyZones()
}

// apexNSTTL is the TTL given to the apex NS record every new zone is seeded
// with (RFC 2181 §10.1, see handleZoneCreate). It matches the TTL convention
// used elsewhere for generated/example zone_records rows.
const apexNSTTL = 3600

// The refusals for the two SOA domain-name fields. Spelled out once because
// create and patch both answer them, and §1 of docs/ui-contract.md holds
// this project to the exact string.
const (
	soaNSMsg   = "soa_ns must be a valid domain name"
	soaMboxMsg = `soa_mbox must be a valid domain name (a dot in the local part is written \.)`
)

// defaultSOATTL is the SOA record's own header TTL, fixed rather than
// client-settable in Milestone A (zoneCreate/zonePatch have no soa_ttl
// field). store.AddZone binds soa_ttl explicitly on every insert, so leaving
// it unset would write a literal 0 instead of falling back to the schema's
// DEFAULT 900 — and RFC 2308 §5 makes a negative answer's TTL
// min(SOAMinimum, SOATTL), so a zero here would make every NXDOMAIN this
// zone hands out uncacheable, forever re-querying us on every miss.
const defaultSOATTL uint32 = 900

// The four zone types this API can create. Still narrower than the schema,
// which has allowed secondary | stub | forwarder | internal since Milestone
// A: internal is reserved for the RFC 6303 built-ins seeded at migration and
// stays refused. The rule is unchanged from Milestone A — the API refuses
// any type it cannot serve correctly, because a type that cannot be created
// cannot misbehave — and what changed since is that each of the other three
// became one it can, as the milestone that makes it servable landed:
// secondary in D2 (a transfer to fill it from), forwarder and stub here in
// D6 (forward_to and primaries, respectively, to route their queries by).
//
// A secondary or stub is only servable because it is *configured*, which is
// what zoneCreate.Primaries and zoneCreate.TSIGKeyID are for and why they
// are validated rather than merely stored; a forwarder likewise through
// zoneCreate.ForwardTo.
const (
	zoneTypePrimary   = "primary"
	zoneTypeSecondary = "secondary"
	zoneTypeForwarder = "forwarder"
	zoneTypeStub      = "stub"
)

// pullsFromAMaster reports whether a zone of this type has somewhere to pull
// from: a secondary fetches the whole zone by AXFR, a stub only the apex SOA
// and NS by ordinary query.
//
// One predicate for two questions that are the same question. It is the set
// primaries/tsig_key_id apply to, because those describe the master; and it
// is the set POST /zones/{id}/refresh acts on, because refreshing is asking
// that master. Answering them separately is how the two drift — the refresh
// gate went on saying "secondary" after stub zones learned to pull, which
// left the dashboard offering a button the API refused.
//
// It is the counterpart of zones.pullsFromAMaster, which is the same rule for
// the scheduler. They are deliberately not shared: this one answers about a
// type an HTTP client just typed, that one about a stored row, and exporting
// either into the other would tie an API contract to a scheduler's internals.
func pullsFromAMaster(zoneType string) bool {
	return strings.EqualFold(zoneType, zoneTypeSecondary) || strings.EqualFold(zoneType, zoneTypeStub)
}

// checkZoneTransferConfig validates the (type, primaries, tsig_key_id,
// allow_transfer, notify_to, forward_to) tuple as the zone would be stored,
// for create and patch alike — a rule enforced on POST and not on PATCH is a
// rule with a way around it. It returns the status code and message to
// answer with, or 0 when the configuration is sound.
func (s *Server) checkZoneTransferConfig(ctx context.Context, zoneType, primaries string, tsigKeyID int64, allowTransfer, notifyTo, forwardTo string) (int, string) {
	// primaries/tsig_key_id describe where a zone pulls from: a secondary by
	// AXFR, a stub by ordinary SOA/NS query. Both are configured the same
	// way, which is why they widen together rather than stub getting its own
	// gate.
	pullsAZone := pullsFromAMaster(zoneType)
	if pullsAZone {
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
			return http.StatusBadRequest, "primaries applies to secondary and stub zones only"
		}
		if tsigKeyID != 0 {
			return http.StatusBadRequest, "tsig_key_id applies to secondary and stub zones only"
		}
	}

	if forwardTo != "" {
		// A forwarder is the only type that sends queries somewhere of its
		// own choosing. On anything else this would be configuration nothing
		// reads, shown by the UI as though it meant something.
		if zoneType != zoneTypeForwarder {
			return http.StatusBadRequest, "forward_to applies to forwarder zones only"
		}
		if err := zones.ValidateForwardTo(forwardTo); err != nil {
			return http.StatusBadRequest, err.Error()
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
	if allowTransfer != "" {
		// Only a primary or secondary serves a zone at all — a forwarder and
		// a stub answer nothing of their own, so an ACL naming who may pull
		// one from them would be configuration nothing reads, shown by the
		// UI as though it meant something.
		if zoneType != zoneTypePrimary && zoneType != zoneTypeSecondary {
			return http.StatusBadRequest, "allow_transfer applies to primary and secondary zones only"
		}
		if err := zones.ValidateACL(allowTransfer); err != nil {
			return http.StatusBadRequest, err.Error()
		}
		// A key: entry that names nothing is an ACL entry that can never
		// match, so the transfer it was written to permit would be refused
		// with no indication why. Caught here, exactly as tsig_key_id is, and
		// guarded from the other end by tsigKeyStore.Delete.
		for _, name := range zones.ACLKeys(allowTransfer) {
			if _, found, err := s.deps.Store.TSIGKeys().ByName(ctx, name); err != nil {
				return http.StatusServiceUnavailable, "storage unavailable"
			} else if !found {
				return http.StatusBadRequest, "allow_transfer names TSIG key " + name + ", which does not exist"
			}
		}
	}
	if notifyTo != "" {
		// Both transfer types may notify; nothing else may. internal is the
		// RFC 6303 built-ins, which are not transferable, and stub/forwarder
		// answer nothing of their own — a notify list on any of them is
		// configuration nothing would ever read, shown by the UI as though
		// it meant something.
		if zoneType != zoneTypePrimary && zoneType != zoneTypeSecondary {
			return http.StatusBadRequest, "notify_to applies to primary and secondary zones only"
		}
		if err := zones.ValidateNotifyTo(notifyTo); err != nil {
			return http.StatusBadRequest, err.Error()
		}
		// A key: name that names nothing would make every NOTIFY to that
		// target unsigned, and a peer requiring a signature refuses it with
		// no indication why. Caught here, exactly as allow_transfer's keys
		// are, and guarded from the other end by tsigKeyStore.Delete.
		for _, name := range zones.NotifyToKeys(notifyTo) {
			if _, found, err := s.deps.Store.TSIGKeys().ByName(ctx, name); err != nil {
				return http.StatusServiceUnavailable, "storage unavailable"
			} else if !found {
				return http.StatusBadRequest, "notify_to names TSIG key " + name + ", which does not exist"
			}
		}
	}
	return 0, ""
}

// canonicalAllowTransfer parses input and returns it in zones.FormatACL's
// canonical spelling — the form Task 2's TSIG-key delete guard matches
// key:<name> against in SQL. input == "" (deny) returns "" without parsing.
//
// Called after checkZoneTransferConfig has already validated the same
// string via zones.ValidateACL, so the error here is unreachable in
// practice; it is checked rather than discarded because errcheck cannot
// know that, and because a re-parse that silently swallowed a failure would
// be one call away from storing whatever ParseACL gave up on.
func canonicalAllowTransfer(input string) (string, error) {
	if input == "" {
		return "", nil
	}
	parsed, err := zones.ParseACL(input)
	if err != nil {
		return "", err
	}
	return zones.FormatACL(parsed), nil
}

// canonicalNotifyTo parses input and returns it in zones.FormatNotifyTo's
// canonical spelling — the form tsigKeyStore.Delete matches ` key:<name>`
// against in SQL. input == "" returns "" without parsing.
//
// Called after checkZoneTransferConfig has already validated the same string,
// so the error here is unreachable in practice; it is checked rather than
// discarded for the reason canonicalAllowTransfer's comment gives — errcheck
// cannot know that, and a re-parse that swallowed a failure would be one call
// away from storing whatever ParseNotifyTo gave up on.
func canonicalNotifyTo(input string) (string, error) {
	if input == "" {
		return "", nil
	}
	ts, err := zones.ParseNotifyTo(input)
	if err != nil {
		return "", err
	}
	return zones.FormatNotifyTo(ts), nil
}

// canonicalForwardTo returns input in zones.FormatForwardTo's spelling — the
// form the routing table is built from, so what is stored is what the router
// will parse. input == "" returns "" without parsing.
//
// Called after checkZoneTransferConfig has validated the same string, so the
// error is unreachable in practice; checked rather than discarded because
// errcheck cannot know that, and a swallowed failure would be one call away
// from storing whatever ParseForwardTo gave up on.
func canonicalForwardTo(input string) (string, error) {
	if input == "" {
		return "", nil
	}
	ts, err := zones.ParseForwardTo(input)
	if err != nil {
		return "", err
	}
	return zones.FormatForwardTo(ts), nil
}

// validDomainLabels reports whether name — already trimmed of whitespace and
// of a trailing dot — is built only from labels this API will accept.
//
// dns.IsDomainName is the RFC 1035 §2.3.4 length check and nothing more; it
// documents itself as "extremely liberal — almost any string is a valid
// domain name" and, verified, accepts `"`, `;`, `!` and NUL inside a label.
// None of those survive where a zone or key name actually goes: a zone named
// "a;b.lan" exports `$ORIGIN a;b.lan.`, where `;` opens a comment, so the
// file it hands the operator cannot be read back; `a"b.lan` closes the
// quoted filename in Content-Disposition; and a NUL truncates whatever
// reads it as a C string.
//
// The accepted set is letters, digits, hyphen and underscore — RFC 1035's
// LDH plus the underscore that `_dmarc` and every other service label needs,
// which is also what a punycode `xn--` label is spelled in. No wildcard:
// neither caller can be one, since a zone apex and a TSIG key's owner name
// name a specific thing. (Record names, which *can* be wildcards, are
// validated in internal/zones.)
//
// escapeDot admits `\.` — a literal dot inside a label rather than the
// separator between two. Only soa_mbox sets it: a mailbox is carried as a
// domain name (RFC 1035 §8), so a dot in the local part has to be escaped,
// and that escape is the only backslash anything here accepts.
func validDomainLabels(name string, escapeDot bool) bool {
	if name == "" {
		return false
	}
	labelLen := 0
	for i := 0; i < len(name); i++ {
		switch c := name[i]; {
		case c == '\\' && escapeDot:
			if i+1 >= len(name) || name[i+1] != '.' {
				return false
			}
			i++
			labelLen++
		case c == '.':
			// An empty label — "e412..in", or a leading dot.
			if labelLen == 0 {
				return false
			}
			labelLen = 0
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
			labelLen++
		default:
			return false
		}
	}
	// A trailing dot would leave the last label empty; callers strip it
	// before calling, so one here is the same defect as an empty label
	// anywhere else.
	return labelLen > 0
}

// normalizeZoneName lowercases name, strips a trailing dot, and validates it
// — see validDomainLabels for what "valid" means and why dns.IsDomainName
// alone is not it.
func normalizeZoneName(raw string) (string, bool) {
	name := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(raw), "."))
	if !validDomainLabels(name, false) {
		return "", false
	}
	if _, ok := dns.IsDomainName(name); !ok {
		return "", false
	}
	return name, true
}

// normalizeSOAName validates one of the SOA's two domain-name fields and
// returns it in the form the rest of the codebase stores: trimmed, with no
// trailing dot.
//
// The trailing dot is not cosmetic. zones.Render writes `SOA %s. %s.`, and a
// transfer stores what it received with the dot trimmed off
// (Transferrer.applySOA), so a hand-written "ns1.e412.in." would export as
// "ns1.e412.in.." — a zone file no parser accepts, produced by a zone that
// looked fine everywhere else.
//
// Neither field was checked at all before. `POST /zones {"soa_ns":"not a
// hostname"}` answered 201 and seeded an apex NS record whose rdata fails
// ToRR ("garbage after rdata"): the answer path drops such a record
// silently (zones/answer.go) and every outbound AXFR errors on it.
func normalizeSOAName(raw string, escapeDot bool) (string, bool) {
	name := strings.TrimSuffix(strings.TrimSpace(raw), ".")
	if !validDomainLabels(name, escapeDot) {
		return "", false
	}
	if _, ok := dns.IsDomainName(name); !ok {
		return "", false
	}
	return name, true
}

// zoneTypeInternal is the built-in zones' type (store.BuiltinZones): not
// creatable through this API, but every install has sixteen of them, so a
// ?type= filter that could not name them would be missing the value that
// matches most of the table.
const zoneTypeInternal = "internal"

// listableZoneTypes is what ?type= accepts. A value outside it is refused
// rather than answered with an empty array: a caller that mistyped
// "forwarders" would otherwise read "you have no forwarders", which is a
// different and wrong answer.
var listableZoneTypes = map[string]bool{
	zoneTypePrimary:   true,
	zoneTypeSecondary: true,
	zoneTypeForwarder: true,
	zoneTypeStub:      true,
	zoneTypeInternal:  true,
}

// handleZonesList answers every zone, optionally narrowed by ?type= and
// ?enabled=.
//
// Filtered here rather than in SQL. ZoneStore.Zones takes no options, and
// widening it means a new query, a new method or an options struct threaded
// through the store interface and both dialects — to save scanning a slice
// that is sixteen built-ins plus however many zones one household has. If a
// zone table ever gets big enough for that to matter, the filter moves; the
// endpoint's contract does not change when it does.
func (s *Server) handleZonesList(w http.ResponseWriter, r *http.Request) {
	zoneType := r.URL.Query().Get("type")
	if zoneType != "" && !listableZoneTypes[zoneType] {
		errJSON(w, http.StatusBadRequest,
			"type must be primary, secondary, forwarder, stub or internal")
		return
	}
	enabled, byEnabled, err := qBool(r, "enabled")
	if err != nil {
		errJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	zs, err := s.deps.Store.Zones().Zones(r.Context())
	if err != nil {
		storeErr(w, err)
		return
	}
	zs = slices.DeleteFunc(zs, func(z store.Zone) bool {
		return (zoneType != "" && z.Type != zoneType) || (byEnabled && z.Enabled != enabled)
	})
	out := make([]zoneResponse, len(zs))
	for i, z := range zs {
		out[i] = s.zoneResponse(z)
	}
	writeJSON(w, http.StatusOK, out)
}

// zoneResponse is the stored row plus the part of a pulled zone's state that
// has no column: the scheduler's pending back-off and its consecutive failure
// count (zones.RefreshStatus).
//
// Both are process-local and die with the process, which is why they are
// *additions* to the durable last_error/last_attempt pair rather than a
// replacement for it — a dashboard that read only these would show a zone
// that had been failing for a week as healthy after every restart. What they
// add is the two things the columns cannot say: when the next attempt is
// actually due, and how many have failed in a row.
//
// Embedded rather than nested, so every existing field of a zone is spelled
// exactly where it was and no client has to learn a new shape to keep reading
// what it already read.
type zoneResponse struct {
	store.Zone
	// NextAttemptAt is unix ms, and 0 whenever there is no back-off pending —
	// which is every zone that is not currently failing, every type that does
	// not pull, and every zone in a process that has not scheduled one yet.
	// The ordinary schedule is refreshed_at + soa_refresh, which the row above
	// already says.
	NextAttemptAt int64 `json:"next_attempt_at"`
	// Failures counts consecutive failed attempts *in this process*; a
	// success resets it to 0.
	Failures int `json:"failures"`
}

// zoneResponse pairs a zone with the scheduler's view of it. A server built
// without a scheduler — every test server with no business opening a TCP
// connection to a primary — reports the zeroes, which is the truthful answer
// for a process where nothing is scheduling anything.
func (s *Server) zoneResponse(z store.Zone) zoneResponse {
	out := zoneResponse{Zone: z}
	if s.deps.ZoneRefresher == nil {
		return out
	}
	st, ok := s.deps.ZoneRefresher.Status(z.ID)
	if !ok {
		return out
	}
	out.Failures = st.Failures
	if !st.NotBefore.IsZero() {
		out.NextAttemptAt = st.NotBefore.UnixMilli()
	}
	return out
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
	writeJSON(w, http.StatusOK, s.zoneResponse(z))
}

// zoneCreate mirrors groupCreate's pointer-for-optional pattern (see
// clients_handlers.go).
type zoneCreate struct {
	Name string `json:"name"`
	// Type omitted means primary. primary, secondary, forwarder and stub are
	// the four this API creates — see zoneTypePrimary.
	Type string `json:"type"`
	// Primaries is where a secondary or stub pulls from: a comma-separated
	// list of host[:port], port defaulting to 53. Required for a secondary
	// or stub, refused on any other type, and stored exactly as written —
	// see internal/zones/primaries.go.
	Primaries string `json:"primaries"`
	// TSIGKeyID names the key a secondary or stub signs its requests with.
	// Optional (0 means unsigned), but when set it must name a key that
	// exists.
	TSIGKeyID int64 `json:"tsig_key_id"`
	// AllowTransfer is who may pull this zone by AXFR: a comma-separated
	// list of address, CIDR, or key:<tsig name> — see zones.ParseACL for the
	// format. Optional; empty means deny every transfer, which is the
	// default. Unlike Primaries, this applies to both primary and secondary
	// zones — a secondary re-serves what it pulled (§9.5.3).
	AllowTransfer string `json:"allow_transfer"`
	// NotifyTo is who this zone tells when it changes. Like AllowTransfer and
	// unlike Primaries/TSIGKeyID, it applies to both transfer types: a
	// secondary that re-serves what it pulled has its own secondaries.
	NotifyTo string `json:"notify_to"`
	// ForwardTo is where a forwarder zone sends the queries it claims: a
	// comma-separated list of host[:port]. Refused on every other type — see
	// zoneTypeForwarder.
	ForwardTo string `json:"forward_to"`
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
	body, ok := decodeOr400[zoneCreate](w, r)
	if !ok {
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
	} else if zoneType != zoneTypePrimary && zoneType != zoneTypeSecondary && zoneType != zoneTypeForwarder && zoneType != zoneTypeStub {
		errJSON(w, http.StatusBadRequest, "only primary, secondary, forwarder and stub zones are supported")
		return
	}
	if code, msg := s.checkZoneTransferConfig(r.Context(), zoneType, body.Primaries, body.TSIGKeyID, body.AllowTransfer, body.NotifyTo, body.ForwardTo); code != 0 {
		errJSON(w, code, msg)
		return
	}
	allowTransfer, err := canonicalAllowTransfer(body.AllowTransfer)
	if err != nil {
		errJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	notifyTo, err := canonicalNotifyTo(body.NotifyTo)
	if err != nil {
		errJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	forwardTo, err := canonicalForwardTo(body.ForwardTo)
	if err != nil {
		errJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	enabled := true
	if body.Enabled != nil {
		enabled = *body.Enabled
	}

	soaNS := "ns." + name
	if body.SOANS != "" {
		var ok bool
		if soaNS, ok = normalizeSOAName(body.SOANS, false); !ok {
			errJSON(w, http.StatusBadRequest, soaNSMsg)
			return
		}
	}
	soaMbox := "hostadmin." + name
	if body.SOAMbox != "" {
		var ok bool
		if soaMbox, ok = normalizeSOAName(body.SOAMbox, true); !ok {
			errJSON(w, http.StatusBadRequest, soaMboxMsg)
			return
		}
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
	zone := store.Zone{
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
		// Empty and 0 unless the type calls for them, both already checked
		// by checkZoneTransferConfig above.
		Primaries: body.Primaries,
		TSIGKeyID: body.TSIGKeyID,
		// The canonical spelling, not body.AllowTransfer — see
		// canonicalAllowTransfer.
		AllowTransfer: allowTransfer,
		// The canonical spelling, not body.NotifyTo — see canonicalNotifyTo.
		NotifyTo: notifyTo,
		// The canonical spelling, not body.ForwardTo — see canonicalForwardTo.
		ForwardTo:  forwardTo,
		CreatedAt:  now,
		ModifiedAt: now,
	}

	// RFC 2181 §10.1: a zone's apex must have NS records, or the zone is
	// malformed from the moment it exists — every future zone-file export
	// and transfer would carry the defect outward.
	//
	// Built through buildZoneRecord, the same validator behind a hand write,
	// and built *before* the zone is inserted. Writing it straight into
	// AddRecord skipped every rule the API enforces on a human, so an
	// unvalidated soa_ns produced an NS row whose rdata fails ToRR: the
	// answer path drops it and every outbound AXFR errors on it, in a zone
	// that reports itself created and healthy. soa_ns is checked above now
	// too, which makes a refusal here all but unreachable — but "all but"
	// is why the record is built first and the zone written only if it
	// holds, rather than logged after the fact against a zone that already
	// exists.
	//
	// Primary zones only. A secondary's contents are its primary's, arriving
	// whole on the first transfer and replacing whatever is there; seeding
	// an NS record here would be dnsaur authoring data in a zone it does not
	// own, and serving it as authoritative in the window before that
	// transfer lands.
	var apexNS store.ZoneRecord
	if zoneType == zoneTypePrimary {
		rec, code, msg, ok := buildZoneRecord(zone, zoneRecordWrite{
			Name:  "@",
			Type:  "NS",
			TTL:   apexNSTTL,
			RData: dns.Fqdn(soaNS),
		}, nil, 0, false)
		if !ok {
			errJSON(w, code, msg)
			return
		}
		apexNS = rec
	}

	id, err := s.deps.Store.Zones().AddZone(r.Context(), zone)
	if err != nil {
		storeErrDup(w, err, "a zone with that name already exists")
		return
	}

	if zoneType == zoneTypePrimary {
		apexNS.ZoneID = id
		// Still best-effort at the storage layer: the zone itself already
		// committed, so a failure here is logged rather than turned into a
		// response the caller cannot reconcile with the id just handed back.
		if _, err := s.deps.Store.Zones().AddRecord(r.Context(), apexNS); err != nil {
			slog.Error("creating apex NS record for new zone failed", "zone", id, "err", err)
		}
	}

	s.reloadZones(r)
	s.notifyZones()
	zone.ID = id
	created(w, resourceURL("zones", id), zone)
}

// zoneClone is the whole request body: the one thing a copy cannot inherit.
type zoneClone struct {
	Name string `json:"name"`
}

// handleZoneClone creates a second zone from an existing one: the same
// configuration and the same records under a different apex.
//
// It exists because that is what a second site, a staging domain or a
// vanity alias actually is, and building one by hand is thirty record
// writes and one forgotten allow_transfer — the kind of near-copy where the
// thing that gets missed is whichever field was not on screen.
//
// **Everything is copied verbatim except the name and the serial.** The
// records need no rewriting at all: they are stored relative to the apex
// ("@", "bifrost"), so they already mean the same thing under a different
// one. The SOA's two names are copied as they stand because they name
// *servers* rather than anything inside the zone, and rewriting them would
// silently repoint the copy at a nameserver nobody chose. rdata is copied
// as it stands for the mirror of that reason: an absolute target in it is a
// deliberate absolute target, and a clone is not the place to guess which
// ones were meant to follow the apex.
//
// The serial restarts at 1, like any other new zone: a serial is this
// zone's name for a version of its own contents, and inheriting one would
// claim a publishing history the copy does not have.
//
// What is deliberately *not* copied is the transfer history — refreshed_at,
// expires_at, last_error and the rest are simply never set on the new row.
// A clone has pulled nothing, and a stamp saying otherwise would put it on
// screen as serving a copy it does not hold.
func (s *Server) handleZoneClone(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		errJSON(w, http.StatusBadRequest, "bad id")
		return
	}
	src, err := s.deps.Store.Zones().Zone(r.Context(), id)
	if err != nil {
		storeErr(w, err)
		return
	}
	// A built-in is seeded by a migration and refused on create (see the
	// zone-type block above), so a copy of one is a zone this API would not
	// have made in the first place. 409 rather than 400 for the reason
	// recordWriteRefusal gives: the request is well formed and the caller is
	// permitted, and what refuses it is which zone was addressed.
	if strings.EqualFold(src.Type, "internal") {
		errJSON(w, http.StatusConflict, "built-in zones cannot be cloned")
		return
	}
	body, ok := decodeOr400[zoneClone](w, r)
	if !ok {
		return
	}
	name, ok := normalizeZoneName(body.Name)
	if !ok {
		errJSON(w, http.StatusBadRequest, "name must be a valid domain name")
		return
	}

	recs, err := s.deps.Store.Zones().Records(r.Context(), id)
	if err != nil {
		storeErr(w, err)
		return
	}

	now := time.Now().UnixMilli()
	zone := src
	zone.ID = 0
	zone.Name = name
	zone.SOASerial = 1
	zone.ExpiresAt, zone.RefreshedAt, zone.LastAttempt, zone.LastError = 0, 0, 0, ""
	zone.LastXfrAt, zone.LastXfrPeer, zone.LastXfrError = 0, "", ""
	zone.CreatedAt, zone.ModifiedAt = now, now

	newID, err := s.deps.Store.Zones().AddZone(r.Context(), zone)
	if err != nil {
		storeErrDup(w, err, "a zone with that name already exists")
		return
	}
	zone.ID = newID

	if len(recs) > 0 {
		for i := range recs {
			recs[i].ID = 0
			recs[i].ZoneID = newID
		}
		// One transaction for the whole record set, through the same call the
		// import and the transfer install use: a half-copied zone is worse
		// than no copy, because it looks like a finished one. The zone row
		// goes in the same statement group and is written back unchanged.
		if err := s.deps.Store.Zones().ReplaceRecords(r.Context(), zone, nil, nil, recs); err != nil {
			// The zone row already committed, so this cannot be undone into
			// "nothing happened". Saying so is better than a 500 the caller
			// cannot reconcile with a zone that now exists — same reasoning
			// as handleZoneCreate's apex NS.
			slog.Error("copying records into a cloned zone failed",
				"source", id, "clone", newID, "err", err)
		}
	}

	s.reloadZones(r)
	s.notifyZones()
	created(w, resourceURL("zones", newID), zone)
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
	// AllowTransfer, unlike Primaries and TSIGKeyID, applies to both
	// primary and secondary zones — see zoneCreate.AllowTransfer.
	AllowTransfer *string `json:"allow_transfer"`
	// NotifyTo, like AllowTransfer, applies to both primary and secondary
	// zones — see zoneCreate.NotifyTo. A pointer so absent (leave alone) and
	// "" (clear it) are different.
	NotifyTo *string `json:"notify_to"`
	// ForwardTo, like Primaries, is a pointer so absent (leave alone) and ""
	// (clear it — the zone names no upstreams) are different. See
	// zoneCreate.ForwardTo.
	ForwardTo *string `json:"forward_to"`
}

// mergeZonePatch applies body to z and validates the result, returning the
// zone as it should be stored or the status and message to refuse with.
//
// Separate from the handler because the handler may have to run it twice —
// see handleZonePatch's retry. A merge computed from a row that has since
// moved is worthless, so redoing it, rather than replaying the same merged
// struct, is the whole of what makes the retry safe.
func (s *Server) mergeZonePatch(ctx context.Context, z store.Zone, body zonePatch) (store.Zone, int, string) {
	if body.Name != nil {
		name, ok := normalizeZoneName(*body.Name)
		if !ok {
			return z, http.StatusBadRequest, "name must be a valid domain name"
		}
		z.Name = name
	}
	if body.Type != nil {
		z.Type = *body.Type
	}
	if body.Enabled != nil {
		z.Enabled = *body.Enabled
	}
	// Both SOA names are validated here for the same reason they are on
	// create: soa_ns becomes the zone's advertised MNAME and, on a zone
	// created through this API, matches the apex NS record beside it. A
	// pointer field, unlike zoneCreate's plain string, makes "" a value the
	// caller chose rather than one they omitted — and an empty MNAME is not
	// a name.
	if body.SOANS != nil {
		soaNS, ok := normalizeSOAName(*body.SOANS, false)
		if !ok {
			return z, http.StatusBadRequest, soaNSMsg
		}
		z.SOANS = soaNS
	}
	if body.SOAMbox != nil {
		soaMbox, ok := normalizeSOAName(*body.SOAMbox, true)
		if !ok {
			return z, http.StatusBadRequest, soaMboxMsg
		}
		z.SOAMbox = soaMbox
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
	if body.AllowTransfer != nil {
		z.AllowTransfer = *body.AllowTransfer
	}
	if body.NotifyTo != nil {
		z.NotifyTo = *body.NotifyTo
	}
	if body.ForwardTo != nil {
		z.ForwardTo = *body.ForwardTo
	}
	// Checked on the merged zone rather than on the body: a patch that sets
	// type without primaries, or clears primaries without changing type,
	// leaves a secondary with nowhere to pull from either way, and only the
	// result says which.
	if code, msg := s.checkZoneTransferConfig(ctx, z.Type, z.Primaries, z.TSIGKeyID, z.AllowTransfer, z.NotifyTo, z.ForwardTo); code != 0 {
		return z, code, msg
	}
	// The canonical spelling, not whatever was sent — see
	// canonicalAllowTransfer. A no-op when AllowTransfer wasn't in the
	// request: the value just read back from the store is already
	// canonical.
	allowTransfer, err := canonicalAllowTransfer(z.AllowTransfer)
	if err != nil {
		return z, http.StatusBadRequest, err.Error()
	}
	z.AllowTransfer = allowTransfer
	// Same reasoning for NotifyTo — see canonicalNotifyTo.
	notifyTo, err := canonicalNotifyTo(z.NotifyTo)
	if err != nil {
		return z, http.StatusBadRequest, err.Error()
	}
	z.NotifyTo = notifyTo
	// Same reasoning for ForwardTo — see canonicalForwardTo.
	forwardTo, err := canonicalForwardTo(z.ForwardTo)
	if err != nil {
		return z, http.StatusBadRequest, err.Error()
	}
	z.ForwardTo = forwardTo
	return z, 0, ""
}

// zonePatchAttempts is how many times handleZonePatch will read, merge and
// write before giving up with a 409.
//
// Two, not one and not many. One would surface a benign race — two PATCHes
// touching different fields, which is the shape a dashboard produces when an
// operator edits two panels — as an error the user has to understand and
// retry by hand. Many would let a row under continuous write pressure hold
// a request open indefinitely; the second failure is evidence of genuine
// contention, and saying so is more useful than trying again.
const zonePatchAttempts = 2

func (s *Server) handleZonePatch(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		errJSON(w, http.StatusBadRequest, "bad id")
		return
	}
	body, ok := decodeOr400[zonePatch](w, r)
	if !ok {
		return
	}
	if body.Type != nil && *body.Type != zoneTypePrimary && *body.Type != zoneTypeSecondary && *body.Type != zoneTypeForwarder && *body.Type != zoneTypeStub {
		errJSON(w, http.StatusBadRequest, "only primary, secondary, forwarder and stub zones are supported")
		return
	}

	// A PATCH is a read, a merge and a write of the whole row, and nothing
	// used to stop two of them interleaving: whichever wrote second wrote
	// every column from a snapshot taken before the first one landed, so the
	// first one's field was silently reverted. UpdateZoneIfUnchanged refuses
	// to land on a row that moved, and the loop redoes the merge against the
	// row as it now is — so an ordinary race costs a second attempt rather
	// than a lost edit or an error the operator has to interpret.
	for attempt := 0; attempt < zonePatchAttempts; attempt++ {
		before, err := s.deps.Store.Zones().Zone(r.Context(), id)
		if err != nil {
			storeErr(w, err)
			return
		}
		// A built-in zone is seeded infrastructure (RFC 6303), not user
		// content — see internal/store/builtins.go. Reads are fine; writes
		// are not.
		if before.Type == "internal" {
			errJSON(w, http.StatusConflict, "built-in zones cannot be changed")
			return
		}
		z, code, msg := s.mergeZonePatch(r.Context(), before, body)
		if code != 0 {
			errJSON(w, code, msg)
			return
		}
		// Strictly greater than the value being replaced, not simply "now":
		// modified_at is what the write predicates on, and it has
		// millisecond resolution, so two PATCHes landing inside one
		// millisecond would otherwise write the same value the second one is
		// checking against and go undetected — the exact race this guard
		// exists for.
		z.ModifiedAt = time.Now().UnixMilli()
		if z.ModifiedAt <= before.ModifiedAt {
			z.ModifiedAt = before.ModifiedAt + 1
		}

		err = s.deps.Store.Zones().UpdateZoneIfUnchanged(r.Context(), z, before.ModifiedAt)
		if errors.Is(err, store.ErrStale) {
			continue
		}
		if err != nil {
			storeErrDup(w, err, "a zone with that name already exists")
			return
		}
		// Before the reload, so the same snapshot rebuild publishes the zone
		// change and the reverse zone's new PTRs together — see
		// handleZoneRecordCreate for the same ordering.
		s.syncZonePTRsAfterPatch(r.Context(), before, z)
		s.reloadZones(r)
		s.notifyZones()
		w.WriteHeader(http.StatusNoContent)
		return
	}
	errJSON(w, http.StatusConflict, "zone changed since it was read")
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
	s.notifyZones()
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

// handleZoneRefresh pulls one zone from its master now, whatever its schedule
// says, and answers only once that has finished. A secondary transfers the
// whole zone by AXFR and a stub fetches its apex SOA and NS by ordinary
// query; which of the two happens is the scheduler's business, and this
// handler's answer has the same shape either way — except for expires_at,
// which is 0 for a stub because a stub does not expire (§9.11.8).
//
// Deliberately synchronous: the caller pressed a button to find out whether
// it works, and a 202 would hand back "started" — which is the one thing they
// already knew — leaving the answer (and the error, which is the whole point)
// nowhere to be read. It takes as long as one conversation with the master,
// bounded by that worker's own timeouts.
//
// A failure is a 502, not a 500: nothing here is broken, a server this one
// depends on refused or could not be reached, and the message names every
// master that was tried. The zone is left exactly as it was — a failed
// transfer or fetch changes nothing (see zones.Transferrer.Transfer and
// zones.StubFetcher.Fetch) — so a 502 here means "still serving what it had",
// never "half-applied".
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
	// Only a zone with a master has anywhere to refresh from. A primary is
	// authored on this server and a forwarder names its upstreams outright in
	// forward_to, so for either this would be a button that does nothing —
	// and asking for it is not a failure to report against anybody, so it is
	// refused here rather than left to come back as a confusing 502 from the
	// worker's own type check.
	if !pullsFromAMaster(z.Type) {
		errJSON(w, http.StatusBadRequest, "only secondary and stub zones pull from a master")
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
