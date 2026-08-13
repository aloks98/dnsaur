package api

import (
	"context"
	"log/slog"
	"net/netip"
	"strings"

	"github.com/aloks98/dnsaur/internal/store"
	"github.com/aloks98/dnsaur/internal/zones"
	"github.com/miekg/dns"
)

// ptrType is the RR type auto-PTR reads and writes. RFC 1034: a PTR's rdata
// is a domain name, not an address — which is why every comparison below is
// against an FQDN and never against the address itself.
const ptrType = "PTR"

// syncPTR keeps the reverse zone in step with an A or AAAA write that has
// already committed to the forward zone, so reverse DNS follows forward DNS
// without a second call from the client. old is nil on create, new is nil on
// delete, and both are set on update; zoneName is the forward zone both
// belong to (a record cannot change zones — see zoneStore.UpdateRecord).
//
// It returns nothing on purpose. The forward write is done and its response
// is already decided by the time this runs, so a PTR that cannot be written
// is logged, never surfaced: reverse DNS lagging is a smaller failure than a
// forward write that reports an error it did not actually suffer.
//
// The five rules it applies are spec §7
// (docs/superpowers/specs/2026-08-08-zones-design.md): write the PTR when a
// reverse zone covers the address; do nothing when none does — never create
// a zone; leave an address that already has a PTR to its first writer; and
// on update and delete, move or remove the PTR only while it still points at
// this name.
func (s *Server) syncPTR(ctx context.Context, old, new *store.ZoneRecord, zoneName string) {
	oldAddr, oldOK := recordAddr(old)
	newAddr, newOK := recordAddr(new)
	if !oldOK && !newOK {
		// Neither side is an address record, so there is no reverse to
		// maintain — the overwhelmingly common case, and the cheapest exit.
		return
	}

	// The forward write already committed, so the reverse half must not be
	// abandoned because the client hung up mid-request — same reasoning as
	// reloadZones (zones_handlers.go).
	ctx = context.WithoutCancel(ctx)

	zs, err := s.deps.Store.Zones().Zones(ctx)
	if err != nil {
		slog.Error("auto-ptr: reading zones failed", "err", err)
		return
	}
	names, byName := ptrZoneCandidates(zs)

	// Retire the old address before claiming the new one. An update that
	// leaves the address unchanged would otherwise meet its own PTR on the
	// way in and decline to touch it under the first-wins rule, so a renamed
	// or retyped record would keep pointing the reverse at its old name.
	if oldOK {
		s.removePTR(ctx, names, byName, oldAddr, recordFQDN(zoneName, old.Name))
	}
	if !newOK {
		return
	}
	fqdn := recordFQDN(zoneName, new.Name)
	if !new.Enabled {
		// A disabled record is dropped from the served snapshot
		// (zones.NewZone), so it answers nothing forward; giving it a PTR
		// would publish a reverse answer for a name that resolves to
		// nothing.
		slog.Warn("auto-ptr: forward record is disabled, not writing a PTR", "addr", newAddr, "name", fqdn)
		return
	}
	s.addPTR(ctx, names, byName, newAddr, fqdn, new.TTL)
}

// retireZonePTRs drops the PTRs the records of a just-deleted zone owned.
// recs is that zone's records, read *before* the delete: its own rows go with
// it through ON DELETE CASCADE (the 0004 migration), but the PTRs auto-PTR
// wrote on their behalf live in a different zone, which no foreign key
// reaches — without this the reverse zone keeps answering with names that no
// longer resolve anywhere.
//
// It is syncPTR's delete half applied to a whole zone at once, and shares its
// contract exactly: the zone list is read once rather than per record, every
// removal goes through removePTR's "only while it still points at this name"
// guard, so a PTR the user typed by hand and a PTR owned by a record in some
// other zone that happens to sit at the same address both survive; and a
// failure is logged, never surfaced, because the zone is already gone.
func (s *Server) retireZonePTRs(ctx context.Context, recs []store.ZoneRecord, zoneName string) {
	type owned struct {
		addr netip.Addr
		fqdn string
	}
	var todo []owned
	for i := range recs {
		addr, ok := recordAddr(&recs[i])
		if !ok {
			continue
		}
		todo = append(todo, owned{addr: addr, fqdn: recordFQDN(zoneName, recs[i].Name)})
	}
	if len(todo) == 0 {
		// No address record in the zone means no PTR was ever written for
		// it, so there is nothing to read the zone list for.
		return
	}

	// Same reasoning as syncPTR: the delete already committed.
	ctx = context.WithoutCancel(ctx)
	zs, err := s.deps.Store.Zones().Zones(ctx)
	if err != nil {
		slog.Error("auto-ptr: reading zones failed", "err", err)
		return
	}
	names, byName := ptrZoneCandidates(zs)
	for _, o := range todo {
		s.removePTR(ctx, names, byName, o.addr, o.fqdn)
	}
}

// recordAddr returns the address rec points at. ok is false when rec is
// absent or is not an address record: A and AAAA are the only types whose
// rdata is an address, and every other type has no reverse name to derive.
func recordAddr(rec *store.ZoneRecord) (netip.Addr, bool) {
	if rec == nil || (rec.Type != "A" && rec.Type != "AAAA") {
		return netip.Addr{}, false
	}
	addr, err := netip.ParseAddr(strings.TrimSpace(rec.RData))
	if err != nil {
		// buildZoneRecord parses every write with dns.NewRR before it lands,
		// so an A/AAAA row whose rdata isn't an address should not exist;
		// declining to guess at one is better than writing a PTR for an
		// address nobody asked for.
		slog.Warn("auto-ptr: address record has unparseable rdata", "rdata", rec.RData, "err", err)
		return netip.Addr{}, false
	}
	return addr, true
}

// recordFQDN is a record's owner name in the form PTR rdata takes: fully
// qualified, trailing dot included.
func recordFQDN(zoneName, relName string) string {
	return dns.Fqdn(zones.RecordFQDN(zoneName, relName))
}

// ptrZoneCandidates is the set of zones auto-PTR may write into, as the
// apex list zones.ReverseZoneFor wants plus a lookup back to the zone it
// picked.
//
// Only enabled primary zones qualify. "internal" is excluded because those
// are the seeded RFC 6303 built-ins (store.BuiltinZones), which every other
// write path refuses with a 409 — an automatic write must not become the one
// exception, and 127.in-addr.arpa covers every loopback address a forward
// record might carry.
//
// `secondary` is excluded by the same test, and since Milestone D2 that is
// the load-bearing half of it: a secondary is a zone the API *can* now
// create, holding a copy of someone else's data that a transfer replaces
// wholesale. This is the fifth write path into a zone — the four explicit
// ones (POST/PUT/DELETE records, POST file) answer 409 for a secondary, and
// this one is automatic, so nothing would refuse it on the caller's behalf.
// Admitting one here would write a PTR the next transfer deletes and, worse,
// bump a serial the primary owns, so this server would advertise a version
// of the zone that no one else has. `stub` and `forwarder` are excluded for
// the original reason: neither holds data at all.
//
// Disabled zones are skipped for the reason (*zones.Index).Find skips them —
// a PTR in a zone that answers nothing is a PTR nobody can look up, and
// skipping it lets an enabled parent zone take the address instead, which is
// where a reverse query for it would actually be answered.
func ptrZoneCandidates(zs []store.Zone) ([]string, map[string]store.Zone) {
	names := make([]string, 0, len(zs))
	byName := make(map[string]store.Zone, len(zs))
	for _, z := range zs {
		if !z.Enabled || z.Type != zoneTypePrimary {
			continue
		}
		// ReverseZoneFor normalizes the apexes it is given and returns the
		// normalized form, so the map has to be keyed the same way.
		name := strings.ToLower(strings.TrimSuffix(z.Name, "."))
		names = append(names, name)
		byName[name] = z
	}
	return names, byName
}

// reverseZoneFor resolves addr to the zone that holds its PTR and the record
// name within it. ok is false when nothing covers the address.
func reverseZoneFor(names []string, byName map[string]store.Zone, addr netip.Addr, fqdn string) (store.Zone, string, bool) {
	zoneName, rel, ok := zones.ReverseZoneFor(addr, names)
	if !ok {
		// Neither an error nor a warning: most deployments hold no reverse
		// zone at all, and auto-PTR must never invent one. At WARN this
		// would fire on every A record ever written.
		slog.Debug("auto-ptr: no reverse zone covers this address", "addr", addr, "name", fqdn)
		return store.Zone{}, "", false
	}
	rev, ok := byName[zoneName]
	if !ok {
		// Unreachable: ReverseZoneFor only ever returns a name it was given.
		return store.Zone{}, "", false
	}
	return rev, rel, true
}

// addPTR writes the PTR for addr, unless the address already has one or the
// reverse zone would refuse the record from a user.
func (s *Server) addPTR(ctx context.Context, names []string, byName map[string]store.Zone, addr netip.Addr, fqdn string, ttl uint32) {
	rev, rel, ok := reverseZoneFor(names, byName, addr, fqdn)
	if !ok {
		return
	}
	recs, err := s.deps.Store.Zones().Records(ctx, rev.ID)
	if err != nil {
		slog.Error("auto-ptr: reading reverse zone records failed", "zone", rev.Name, "err", err)
		return
	}
	for _, e := range recs {
		if e.Type != ptrType || !strings.EqualFold(e.Name, rel) {
			continue
		}
		// First writer keeps the address. A PTR is effectively one per
		// address, so two names at one address means one of them owns the
		// reverse; taking it from whoever got there first is a decision, not
		// a derivation (spec §7), and it is also what keeps a PTR the user
		// wrote by hand from being replaced the moment a forward record
		// lands on the same address.
		slog.Warn("auto-ptr: address already has a PTR, leaving it", "addr", addr, "ptr", e.RData, "name", fqdn)
		return
	}
	// Then the same gate a user's own write goes through. buildZoneRecord is
	// the validator behind POST /zones/{id}/records, so running it here means
	// the automatic path cannot produce zone data the API would refuse from a
	// human — a CNAME already at this name being the case that bites (RFC
	// 1034 §3.6.2: a CNAME must be the only record at its name, and the
	// resolver would happily serve the pair this side effect had written).
	//
	// Deliberately this and not "bail on any record already here": a PTR
	// beside an NS or a TXT is legal and the API accepts it from a user, and
	// rel is "@" whenever the reverse zone's apex is itself the address's
	// reverse name — where the zone's own apex NS always sits, so a blanket
	// check would permanently refuse the PTR in that shape.
	rec, _, reason, ok := buildZoneRecord(rev, zoneRecordWrite{
		Name:  rel,
		Type:  ptrType,
		TTL:   ttl,
		RData: fqdn,
	}, recs, 0, false)
	if !ok {
		slog.Warn("auto-ptr: reverse zone will not accept this PTR, leaving it alone",
			"addr", addr, "zone", rev.Name, "record", rel, "name", fqdn, "reason", reason)
		return
	}
	if _, err := s.deps.Store.Zones().AddRecord(ctx, rec); err != nil {
		slog.Error("auto-ptr: writing PTR failed", "zone", rev.Name, "name", rel, "err", err)
		return
	}
	s.bumpPTRZoneSerial(ctx, rev)
}

// removePTR drops the PTR for addr, but only while it still points at fqdn.
func (s *Server) removePTR(ctx context.Context, names []string, byName map[string]store.Zone, addr netip.Addr, fqdn string) {
	rev, rel, ok := reverseZoneFor(names, byName, addr, fqdn)
	if !ok {
		return
	}
	recs, err := s.deps.Store.Zones().Records(ctx, rev.ID)
	if err != nil {
		slog.Error("auto-ptr: reading reverse zone records failed", "zone", rev.Name, "err", err)
		return
	}
	var removed bool
	for _, e := range recs {
		if e.Type != ptrType || !strings.EqualFold(e.Name, rel) {
			continue
		}
		// Only if it still points at this name. A PTR naming anything else
		// belongs to whoever claimed the address first or to the user who
		// typed it, and an automatic write must not clobber either.
		// dns.Fqdn on both sides so a hand-written name without the trailing
		// dot still compares equal.
		if !strings.EqualFold(dns.Fqdn(e.RData), fqdn) {
			slog.Warn("auto-ptr: PTR points at another name, leaving it", "addr", addr, "ptr", e.RData, "name", fqdn)
			continue
		}
		if err := s.deps.Store.Zones().DeleteRecord(ctx, e.ID); err != nil {
			slog.Error("auto-ptr: removing PTR failed", "zone", rev.Name, "name", rel, "err", err)
			continue
		}
		removed = true
	}
	if removed {
		s.bumpPTRZoneSerial(ctx, rev)
	}
}

// bumpPTRZoneSerial advances the reverse zone's SOA serial: auto-PTR changed
// that zone's contents, not just the forward zone the request addressed. It
// is bumpZoneSerial's context-taking twin — the side effect outlives the
// *http.Request that one is bound to. A failure is logged for the same
// reason: the record write already committed, and only the advertised serial
// lags.
func (s *Server) bumpPTRZoneSerial(ctx context.Context, rev store.Zone) {
	if err := s.deps.Store.Zones().BumpSerial(ctx, rev.ID); err != nil {
		slog.Error("auto-ptr: bumping reverse zone serial failed", "zone", rev.Name, "err", err)
	}
}
