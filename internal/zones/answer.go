package zones

import (
	"strings"

	"github.com/aloks98/dnsaur/internal/store"
	"github.com/miekg/dns"
)

// maxCNAMEChase bounds how far an in-zone CNAME chain is followed. A chain
// longer than this is a loop or a mistake; either way the answer stops here
// rather than recursing until the process dies, which would make one bad
// record a remote crash.
const maxCNAMEChase = 8

// Answer fills m with this zone's response to qname/qtype and reports
// whether the zone answered at all.
//
// handled is false only for zone types that name somewhere else to ask
// rather than holding data (forwarder, stub); for those, m is left
// untouched for the rest of the pipeline, which routes the query to that
// zone's own upstreams through the conditional table — see
// Resolver.Middleware. Every other type answers, and that is the whole
// difference between a zone and the override list it replaced: inside a
// zone we hold, a query never leaves. A name we do not have is an
// authoritative NXDOMAIN carrying our SOA, not a lookup upstream that leaks
// an internal name and lets a public record shadow it.
//
// The order below is RFC 1034 §4.3.2:
//
//	zone cut above or at the name → referral, aa=0
//	name exists  → its CNAME, else its records of qtype, else NODATA + SOA
//	name absent  → the wildcard at the closest encloser, else NXDOMAIN + SOA
//
// qname must be inside this zone; Index.Find is what establishes that.
//
// nowMs is unix milliseconds, passed in rather than read here, and is used
// for exactly one thing: deciding whether a secondary is still entitled to
// answer at all (see Serving).
func (z *Zone) Answer(m *dns.Msg, qname string, qtype uint16, nowMs int64) (handled bool) {
	switch strings.ToLower(z.Type) {
	case "forwarder", "stub":
		return false
	}
	if !z.Serving(nowMs) {
		// Handled, but with nothing: no AA, no records, and above all no
		// SOA. See Serving for why this is not a denial and not a
		// fall-through.
		m.Rcode = dns.RcodeServerFailure
		return true
	}
	m.Authoritative = true
	m.Rcode = dns.RcodeSuccess
	z.resolve(m, dns.Fqdn(qname), RelName(qname, z.Name), qtype, 0)
	return true
}

// Serving reports whether z is entitled to answer from the records it holds,
// as of nowMs (unix milliseconds). Every type but secondary always is: a
// primary owns its data outright.
//
// A secondary holds its primary's data on loan, and there are two states in
// which it holds nothing it may speak for:
//
//   - it has never transferred (RefreshedAt == 0). This is every secondary
//     from the moment it is created until the first transfer lands.
//   - its data has expired (RFC 1034 §4.3.5): past ExpiresAt it can no
//     longer confirm what it holds is current, and serving it anyway is
//     worse than serving nothing, because the resolver asking cannot tell.
//     ExpiresAt is 0 until a transfer records a deadline, which is why the
//     comparison is guarded rather than being nowMs >= ExpiresAt outright.
//
// **A stub must never be added to that type check, and this is the line
// somebody widening it would edit.** It looks like it belongs: a stub pulls
// from a master, on the same SOA schedule, through the same scheduler, and
// "both pull from a master, so both expire" is a sentence that writes itself.
// It does not follow. A secondary serves its master's *data* and past the
// expire cannot vouch for what it holds; a stub serves no data at all — its NS
// set is routing information, so an old-but-working nameserver beats a
// self-inflicted SERVFAIL, and if those nameservers really are gone the query
// fails anyway through the forwarder's own path. Same outcome when it should
// be, better when it should not (§9.11.8, and see StubFetcher.install for the
// other end of it: a stub is never given an expires_at in the first place).
//
// Nothing nearby will remind you of that. Answer returns handled=false for a
// stub *before* it consults Serving, so this function read in isolation has no
// visible connection to the type at all. TestAStubDoesNotExpire is what fails
// if the check is widened.
//
// What such a zone must not do is answer NXDOMAIN + SOA. That is an
// authoritative claim that the name does not exist — RFC 8020 makes it a
// claim about everything beneath it too — so an empty secondary would
// black-hole its own suffix for every resolver that believed it, and the
// SOA would have them cache the black hole for soa_minimum seconds. Nor may
// it fall through to the forwarder: a name inside a zone we claim would then
// leak upstream and a public record could shadow the internal one, which is
// the leak zones exist to close. Answer answers SERVFAIL — "ask again, I
// cannot say" — which asserts nothing and caches nowhere.
// Disabled is deliberately NOT part of this rule, and the difference is worth
// stating because it looks like an inconsistency. A zone that cannot vouch for
// its data answers SERVFAIL rather than falling through, since a public record
// would otherwise shadow the internal one. A *disabled* zone falls through:
// Index.Find skips it entirely, so the query reaches the forwarder and the
// public answer wins.
//
// That is a decision, not an oversight (2026-08-13). Disabling a zone means
// dnsaur gives up the name, so the internet's answer applies; it matches
// Technitium, and §1's scope line is full Technitium parity. The consequence
// to know is that disabling a split-horizon zone exposes its names to public
// answers rather than making them fail.
func (z *Zone) Serving(nowMs int64) bool {
	// Secondary only. See above for the type that looks like it belongs here
	// and must not be added: a stub does not expire (§9.11.8).
	if !strings.EqualFold(z.Type, "secondary") {
		return true
	}
	if z.RefreshedAt == 0 {
		return false
	}
	return z.ExpiresAt == 0 || nowMs < z.ExpiresAt
}

// resolve answers for one name, and is re-entered for each in-zone CNAME
// target so a chased name gets the same treatment the queried one did —
// including the zone-cut check, which a target below a delegation still
// needs.
func (z *Zone) resolve(m *dns.Msg, fqdn, rel string, qtype uint16, depth int) {
	// RFC 1034 §4.3.2 step 3(b): a name at or below a zone cut is the
	// child's to serve. Checked first, because records stored below the cut
	// must not be answered from here even though they are in our table.
	if z.referral(m, rel) {
		return
	}

	// The apex's SOA is a zones-row field rather than a zone_records row
	// (its serial needs managed increments), so it has to be served from
	// there or the zone has no answer for its own SOA.
	if rel == apexName && qtype == dns.TypeSOA {
		m.Answer = append(m.Answer, z.SOA())
		return
	}

	if exact := z.rrs(rel); len(exact) > 0 {
		if z.fill(m, fqdn, exact, qtype, depth) {
			return
		}
		// The name exists with other types. RFC 4592 §2.2: a wildcard must
		// not answer for a name that exists, so this is NODATA and the
		// wildcard below is not consulted. Without this, every NODATA under
		// a wildcard silently returns the wildcard's address instead.
		z.deny(m, dns.RcodeSuccess)
		return
	}

	// A name with descendants exists as an empty non-terminal even with no
	// records of its own, and so does the apex (its SOA is there). RFC 8020
	// makes NXDOMAIN a claim about the name *and everything below it*, so
	// answering it here would tell every resolver to stop asking for the
	// names below that do exist. It is NODATA — and, being an existing
	// name, it is not a wildcard match either (RFC 4592 §3.3.1).
	if rel == apexName || z.hasDescendant(rel) {
		z.deny(m, dns.RcodeSuccess)
		return
	}

	if w := z.wildcard(rel); len(w) > 0 {
		if z.fill(m, fqdn, w, qtype, depth) {
			return
		}
		z.deny(m, dns.RcodeSuccess)
		return
	}

	z.deny(m, dns.RcodeNameError)
}

// fill appends the records of qtype from recs to m.Answer under the owner
// name fqdn, following a CNAME instead if one is present, and reports
// whether it produced anything. A false return is the NODATA case: this
// name exists, but not with the type asked for.
func (z *Zone) fill(m *dns.Msg, fqdn string, recs []store.ZoneRecord, qtype uint16, depth int) bool {
	// RFC 1034 §3.6.2: a CNAME is the only data a name may hold, so any
	// query for a different type is answered by following it. Asking for
	// the CNAME itself is answered directly — following it would return
	// data for a name the client did not ask about.
	if qtype != dns.TypeCNAME {
		for _, r := range recs {
			if rrType(r) == dns.TypeCNAME {
				return z.chase(m, fqdn, r, qtype, depth)
			}
		}
	}
	n := 0
	for _, r := range recs {
		if rrType(r) != qtype {
			continue
		}
		rr, err := ToRR(fqdn, r)
		if err != nil {
			// Unservable rdata reads as absent rather than taking the rest
			// of the RRset down with it — but note what "absent" costs
			// inside a zone: the name can fall to authoritative NODATA and
			// is never forwarded, so a row that lands here is a record that
			// silently disappeared, not one that degrades to an upstream
			// lookup.
			//
			// The API validates writes through this same dns.NewRR, so no
			// row written through it can land here. It is not the only
			// writer, though: internal/store's local_records migration
			// (zonemigrate.go) inserts rows directly, which is exactly why
			// it converts a TXT value into quoted presentation format
			// rather than copying the stored bytes across.
			continue
		}
		m.Answer = append(m.Answer, rr)
		n++
	}
	// The qtype test is a shortcut, not a gate: attachNSGlue only reacts to
	// NS records in ANSWER, and no other qtype puts one there. It saves a
	// scan of ANSWER on every successful query, so removing it changes
	// nothing observable — do not read it as load-bearing.
	if n > 0 && qtype == dns.TypeNS {
		z.attachNSGlue(m)
	}
	return n > 0
}

// attachNSGlue does RFC 1035 §3.3.11's additional-section processing for the
// NS records just written into m.Answer: a client handed a nameserver's name
// and no address has to go and find one.
//
// referral covers the delegation case, where glue is load-bearing because the
// child is the only one who could answer and we have just told the client to
// ask it. This covers NS records that reach the ANSWER section instead — the
// zone's own apex set, and (pre-existing, see fill) a wildcard NS synthesised
// for a name that does not exist. Neither is a cut, so neither reaches
// referral's walk. Every real master does this: ns1.google.com returns A and
// AAAA for all four of google.com's nameservers.
//
// It is what makes a stub zone workable. A stub takes an in-zone nameserver's
// address from glue and never resolves it, because resolving ns1.corp.example
// for zone corp.example would route back into the zone being reached (§9.11.7)
// — so a master that omits glue leaves the stub with no usable nameserver at
// all. Omitting it here is what made dnsaur unusable as a stub's master.
//
// Only in-zone addresses go in, the same rule referral obeys. a.iana-servers.net
// shows both halves in a single response: asked for iana-servers.net NS it
// returns A and AAAA for a., b. and c.iana-servers.net and nothing at all for
// ns.icann.org, which it is not authoritative for. An address for a name
// outside this zone is not ours to vouch for, even when we hold one.
func (z *Zone) attachNSGlue(m *dns.Msg) {
	seen := make(map[string]bool, len(m.Answer))
	for _, rr := range m.Answer {
		ns, ok := rr.(*dns.NS)
		if !ok {
			continue
		}
		// Canonicalised because rdata is stored verbatim and DNS compares
		// names case-insensitively: two apex NS rows naming one host in
		// different case both pass the RRSet checks on write, and a raw-string
		// key would emit that host's address twice under two spellings.
		key := dns.CanonicalName(ns.Ns)
		if seen[key] {
			continue
		}
		seen[key] = true
		m.Extra = append(m.Extra, z.glue(ns.Ns)...)
	}
}

// chase appends the CNAME and, when its target is inside this zone,
// continues resolving there so the client gets the address in the same
// response instead of paying a round trip for it. An out-of-zone target is
// left for the pipeline: the CNAME alone is still an authoritative answer.
func (z *Zone) chase(m *dns.Msg, fqdn string, rec store.ZoneRecord, qtype uint16, depth int) bool {
	rr, err := ToRR(fqdn, rec)
	if err != nil {
		return false
	}
	cn, ok := rr.(*dns.CNAME)
	if !ok {
		return false
	}
	m.Answer = append(m.Answer, cn)
	if depth+1 >= maxCNAMEChase || !z.owns(cn.Target) {
		return true
	}
	// The rcode describes the end of the chain: an in-zone target that does
	// not exist is NXDOMAIN, with the CNAME kept in ANSWER.
	z.resolve(m, dns.Fqdn(cn.Target), RelName(cn.Target, z.Name), qtype, depth+1)
	return true
}

// deny writes a negative answer: rcode, and the zone's SOA in AUTHORITY so
// a resolver can cache the absence instead of re-asking on every lookup
// (RFC 2308 §3). AA stays set — a negative answer from the zone that holds
// the name is authoritative.
func (z *Zone) deny(m *dns.Msg, rcode int) {
	m.Rcode = rcode
	soa := z.SOA()
	// RFC 2308 §5: the negative TTL is min(MINIMUM, the SOA record's own
	// TTL). That number is how long every resolver on the network caches
	// this absence, so taking the larger of the two would keep a
	// newly-added record invisible for exactly that long.
	soa.Hdr.Ttl = min(z.SOAMinimum, z.SOATTL)
	m.Ns = append(m.Ns, soa)
}

// referral walks from rel up to (not including) the apex looking for a zone
// cut — an NS record below the apex — and writes the referral if it finds
// one. NS *at* the apex is this zone's own authority (RFC 2181 §10.1), not
// a cut, so it is never reached by this walk.
func (z *Zone) referral(m *dns.Msg, rel string) bool {
	if rel == apexName {
		return false
	}
	labels := dns.SplitDomainName(normalizeName(rel))
	for i := range labels {
		cut := strings.Join(labels[i:], ".")
		owner := dns.Fqdn(cut + "." + z.Name)
		var auth, glue []dns.RR
		for _, r := range z.rrs(cut) {
			if rrType(r) != dns.TypeNS {
				continue
			}
			rr, err := ToRR(owner, r)
			if err != nil {
				continue
			}
			ns, ok := rr.(*dns.NS)
			if !ok {
				continue
			}
			auth = append(auth, ns)
			glue = append(glue, z.glue(ns.Ns)...)
		}
		if len(auth) == 0 {
			continue
		}
		// We are not authoritative below a cut, and saying otherwise would
		// make a stale copy of the child's data look official.
		m.Authoritative = false
		m.Ns = append(m.Ns, auth...)
		m.Extra = append(m.Extra, glue...)
		return true
	}
	return false
}

// glue returns this zone's addresses for a delegated nameserver. Only
// in-zone names qualify: an address for a nameserver named outside this
// zone is not ours to vouch for, and the resolver can look it up itself.
func (z *Zone) glue(target string) []dns.RR {
	if !z.owns(target) {
		return nil
	}
	fqdn := dns.Fqdn(target)
	var out []dns.RR
	for _, r := range z.rrs(RelName(target, z.Name)) {
		switch rrType(r) {
		case dns.TypeA, dns.TypeAAAA:
			if rr, err := ToRR(fqdn, r); err == nil {
				out = append(out, rr)
			}
		}
	}
	return out
}

// wildcard returns the records of the source of synthesis for rel, which
// RFC 4592 §3.3.1 defines as "*." + the closest encloser — not any wildcard
// further up. Because the asterisk is only ever placed leftmost here and
// lookups are exact, a stored name like "a.*" is a literal that answers for
// itself and synthesises for nothing (RFC 4592 §2.1.1).
func (z *Zone) wildcard(rel string) []store.ZoneRecord {
	if ce := z.closestEncloser(rel); ce != apexName {
		return z.rrs("*." + ce)
	}
	return z.rrs("*")
}

// closestEncloser returns the deepest ancestor of rel that exists — with
// records of its own or as an empty non-terminal. rel itself is excluded:
// this is only called once rel is known not to exist. The apex always
// exists, so the walk always terminates.
func (z *Zone) closestEncloser(rel string) string {
	labels := dns.SplitDomainName(normalizeName(rel))
	for i := 1; i < len(labels); i++ {
		anc := strings.Join(labels[i:], ".")
		if len(z.rrs(anc)) > 0 || z.hasDescendant(anc) {
			return anc
		}
	}
	return apexName
}

// hasDescendant reports whether any name in the zone sits below rel, which
// is what makes rel exist as an empty non-terminal. It scans the zone's
// names, which costs O(records) on the miss paths that reach it; a
// homelab zone is small enough that a precomputed set of non-terminals
// would be more state to keep correct than it saves.
func (z *Zone) hasDescendant(rel string) bool {
	suffix := "." + normalizeName(rel)
	for name := range z.Records {
		if strings.HasSuffix(name, suffix) {
			return true
		}
	}
	return false
}

// rrs returns the records stored at rel, a name relative to the apex.
func (z *Zone) rrs(rel string) []store.ZoneRecord {
	return z.Records[normalizeName(rel)]
}

// owns reports whether name is inside this zone.
func (z *Zone) owns(name string) bool {
	n, apex := normalizeName(name), normalizeName(z.Name)
	return n == apex || strings.HasSuffix(n, "."+apex)
}

// rrType is a record's stored type as a wire type, or 0 for a type
// miekg/dns does not know — which matches no query.
func rrType(r store.ZoneRecord) uint16 {
	return dns.StringToType[strings.ToUpper(r.Type)]
}
