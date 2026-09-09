// Package zones implements the zone cut, relative-name conversion, and RR
// construction used to answer queries authoritatively for the zones stored
// in internal/store. It provides only lookup primitives — NODATA/NXDOMAIN,
// wildcard synthesis, and referral logic live in the answering layer built
// on top of it.
package zones

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/aloks98/dnsaur/internal/store"
	"github.com/miekg/dns"
)

// Zone pairs a stored zone with its records, keyed by name relative to the
// zone apex ("@" for the apex itself, "bifrost" for bifrost.<apex>, etc.).
type Zone struct {
	store.Zone
	Records map[string][]store.ZoneRecord
}

// apexName is the relative name of the zone apex, as stored in
// zone_records.name and used as the key for the apex's own records.
const apexName = "@"

// NewZone builds the served form of a stored zone: its records keyed by
// name relative to the apex, with disabled ones dropped.
//
// Dropping them here rather than in the answering layer is deliberate. A
// disabled record must be indistinguishable from one that was never
// written — otherwise it could still make a name "exist" and turn an
// NXDOMAIN into a NODATA, or suppress a wildcard that should have matched.
// Filtering once, at snapshot build, is what makes those two cases the
// same case everywhere downstream instead of a rule every lookup has to
// remember.
func NewZone(z store.Zone, recs []store.ZoneRecord) Zone {
	out := Zone{Zone: z, Records: make(map[string][]store.ZoneRecord, len(recs))}
	for _, r := range recs {
		if !r.Enabled {
			continue
		}
		key := normalizeName(r.Name)
		// A row ToRR cannot parse is skipped by every reader downstream
		// (answer.go's fill, glue and chase all do), and skipping it there
		// says nothing: the record disappears, and inside a zone that is an
		// authoritative NODATA rather than a lookup that goes anywhere else.
		// Nothing written through BuildRecord can be such a row, but the
		// local_records migration (internal/store/zonemigrate.go) inserts
		// rows directly. Naming it once per reload is what makes it findable.
		if _, err := ToRR(RecordFQDN(z.Name, r.Name), r); err != nil {
			slog.Warn("zone record cannot be served and is being skipped",
				"zone", z.Name, "record", r.ID, "name", r.Name, "type", r.Type, "err", err)
		}
		out.Records[key] = append(out.Records[key], r)
	}
	return out
}

// SOA builds this zone's SOA record from its stored fields. The header TTL
// comes from SOATTL and the MINIMUM rdata field from SOAMinimum: they are
// two independent values, and RFC 2308 §5 makes a negative answer's TTL the
// smaller of the two, which is only expressible if they can differ.
func (z *Zone) SOA() *dns.SOA {
	return &dns.SOA{
		Hdr: dns.RR_Header{
			Name:   dns.Fqdn(z.Name),
			Rrtype: dns.TypeSOA,
			Class:  dns.ClassINET,
			Ttl:    z.SOATTL,
		},
		Ns:      dns.Fqdn(z.SOANS),
		Mbox:    dns.Fqdn(z.SOAMbox),
		Serial:  z.SOASerial,
		Refresh: z.SOARefresh,
		Retry:   z.SOARetry,
		Expire:  z.SOAExpire,
		Minttl:  z.SOAMinimum,
	}
}

// normalizeName lowercases s and strips a trailing dot, so callers can pass
// either presentation form interchangeably.
func normalizeName(s string) string {
	return strings.ToLower(strings.TrimSuffix(s, "."))
}

// RelName returns qname's name relative to apex: "@" if qname is the apex
// itself, otherwise the labels left of the apex suffix, dot-joined. A qname
// that is not inside apex at all is returned unchanged (normalized), which
// is the only signal this function has for "not in the zone" — recordProblem
// reads it that way.
//
// The comparison is by label, not by bytes, because RFC 1035 §5.1 lets a dot
// be escaped into a label: `foo\.e412.in` is a two-label name under "in" and
// shares no labels with "e412.in", but ends with those very bytes. Trimming
// the byte suffix hands back the name `foo\` and puts a name this zone does
// not hold inside it — the same mistake owns made, and the one Index.Find
// never made because it splits labels.
func RelName(qname, apex string) string {
	qname = normalizeName(qname)
	apex = normalizeName(apex)
	if qname == apex {
		return "@"
	}
	if !dns.IsSubDomain(dns.Fqdn(apex), dns.Fqdn(qname)) {
		return qname
	}
	labels := dns.SplitDomainName(qname)
	if n := len(labels) - dns.CountLabel(dns.Fqdn(apex)); n > 0 {
		return strings.Join(labels[:n], ".")
	}
	return qname
}

// errParsesToNothing is what a record whose line lexes to no record at all
// is refused with. dns.NewRR answers (nil, nil) for such a line — an owner
// beginning ';' makes the whole line a comment — and every caller here reads
// "no error" as "an RR", so passing that pair on made a nil RR each of their
// problems: BuildRecord dereferenced it through RDataOf, and fill would have
// appended it for Pack to trip over inside the response writer.
var errParsesToNothing = errors.New("the record parses to nothing: a ';' makes the rest of the line a comment")

// ToRR rebuilds an RR by handing miekg/dns a master-file line. This is the
// same code path the API validates writes with, so a row that parses here
// is a row that can be served — the two cannot drift.
func ToRR(fqdn string, rec store.ZoneRecord) (dns.RR, error) {
	rr, err := dns.NewRR(fmt.Sprintf("%s %d IN %s %s", dns.Fqdn(fqdn), rec.TTL, rec.Type, rec.RData))
	if err != nil {
		return nil, err
	}
	if rr == nil {
		return nil, errParsesToNothing
	}
	return rr, nil
}

// RDataOf returns rr's rdata in presentation format: the record as
// miekg/dns prints it, with its own header removed. It is the spelling
// zone_records.rdata stores, and the one Render writes back into a master
// file.
//
// This is the derivation Parse has always used to fill ParsedRecord.RData
// (see classify), lifted out so the hand-write path can reach it too. Both
// paths arriving at rdata by the same route is what makes a record typed
// into the form and the same record read out of a zone file store as one
// string rather than two — a property the import diff depends on, and one
// that would be quietly untrue if either side spelled the RR itself.
func RDataOf(rr dns.RR) string {
	return strings.TrimPrefix(rr.String(), rr.Header().String())
}

// Index is a snapshot of the zones this server is authoritative for, built
// fresh on every store reload.
type Index struct {
	zones  []Zone
	byApex map[string]int // normalized apex name -> index into zones
}

// NewIndex builds an Index over zs. Zone names are expected to be unique;
// if two entries share a name, the later one wins lookups.
func NewIndex(zs []Zone) *Index {
	idx := &Index{zones: zs, byApex: make(map[string]int, len(zs))}
	for i, z := range zs {
		idx.byApex[normalizeName(z.Name)] = i
	}
	return idx
}

// Apex returns a pointer into the Index's own zone slice, or nil — the
// served zone itself and not a copy, so it is to be read and never written
// (see Resolver.Snapshot for what writing through it costs).
//
// Unlike Find it does not walk suffixes, and unlike Find it does not skip a
// disabled zone: a transfer names one zone, so answering an AXFR for
// sub.e412.in with e412.in would hand over a zone the peer did not ask for
// and may not be permitted, and "we hold this zone but it is disabled" is a
// different refusal from "we do not hold it" that the caller has to be able
// to tell apart.
func (idx *Index) Apex(name string) *Zone {
	i, ok := idx.byApex[normalizeName(name)]
	if !ok {
		return nil
	}
	return &idx.zones[i]
}

// Find returns the most specific enabled zone authoritative for qname, or
// nil if no zone claims it. It walks qname's labels right-to-left — the
// full name first, then each successively shorter suffix — so a nested
// zone (e412.in) wins over its parent (in) when both exist. A disabled
// zone is skipped rather than treated as a match, so a shallower enabled
// zone (or nothing at all, sending the query upstream) can still claim the
// name.
//
// Like Apex, the Zone it points at belongs to the served snapshot: read it,
// never write it.
func (idx *Index) Find(qname string) *Zone {
	labels := dns.SplitDomainName(normalizeName(qname))
	for i := range labels {
		zi, ok := idx.byApex[strings.Join(labels[i:], ".")]
		if !ok {
			continue
		}
		if z := &idx.zones[zi]; z.Enabled {
			return z
		}
	}
	return nil
}

// Zones returns every zone in this snapshot, disabled ones included — the
// caller decides what "disabled" means for what it is building, the same way
// Apex leaves that decision to a transfer and Find makes it for a query.
//
// The slice is the Index's own and must not be mutated: an Index is
// immutable once built and shared by every reader of the snapshot. Reload
// replaces the whole Index rather than editing one in place, which is what
// makes handing the slice out safe.
func (idx *Index) Zones() []Zone { return idx.zones }
