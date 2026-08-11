// Package zones implements the zone cut, relative-name conversion, and RR
// construction used to answer queries authoritatively for the zones stored
// in internal/store. It provides only lookup primitives — NODATA/NXDOMAIN,
// wildcard synthesis, and referral logic live in the answering layer built
// on top of it.
package zones

import (
	"fmt"
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
// itself, otherwise the labels left of the apex suffix, dot-joined. Both
// arguments are compared case-insensitively and trailing-dot-insensitively.
func RelName(qname, apex string) string {
	qname = normalizeName(qname)
	apex = normalizeName(apex)
	if qname == apex {
		return "@"
	}
	return strings.TrimSuffix(qname, "."+apex)
}

// ToRR rebuilds an RR by handing miekg/dns a master-file line. This is the
// same code path the API validates writes with, so a row that parses here
// is a row that can be served — the two cannot drift.
func ToRR(fqdn string, rec store.ZoneRecord) (dns.RR, error) {
	return dns.NewRR(fmt.Sprintf("%s %d IN %s %s", dns.Fqdn(fqdn), rec.TTL, rec.Type, rec.RData))
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

// Find returns the most specific enabled zone authoritative for qname, or
// nil if no zone claims it. It walks qname's labels right-to-left — the
// full name first, then each successively shorter suffix — so a nested
// zone (e412.in) wins over its parent (in) when both exist. A disabled
// zone is skipped rather than treated as a match, so a shallower enabled
// zone (or nothing at all, sending the query upstream) can still claim the
// name.
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
