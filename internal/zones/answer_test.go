package zones_test

import (
	"maps"
	"testing"

	"github.com/aloks98/dnsaur/internal/store"
	"github.com/aloks98/dnsaur/internal/zones"
	"github.com/miekg/dns"
)

const testApex = "e412.in"

// testNow is the instant every Answer call in this file is made at, fixed so
// a test that says nothing about a zone's transfer state gets the same answer
// on every run. Only the secondary tests below give it meaning.
const testNow int64 = 1786000000000

// newZone builds a primary zone for e412.in holding recs, through the same
// zones.NewZone the resolver's snapshot build uses — so the disabled-record
// rule these tests rely on is the production one, not a test-local copy.
func newZone(t *testing.T, recs ...store.ZoneRecord) *zones.Zone {
	t.Helper()
	return newZoneWithSOA(t, 900, 900, recs...)
}

// newZoneWithSOA is newZone with the two TTLs RFC 2308 §5 takes a minimum of
// set independently: soaTTL is the SOA record's own header TTL, minimum is
// its MINIMUM rdata field.
func newZoneWithSOA(t *testing.T, soaTTL, minimum uint32, recs ...store.ZoneRecord) *zones.Zone {
	t.Helper()
	z := zones.NewZone(store.Zone{
		Name: testApex, Type: "primary", Enabled: true,
		SOANS: "ns1." + testApex, SOAMbox: "hostadmin." + testApex,
		SOASerial: 2026080801, SOARefresh: 900, SOARetry: 300, SOAExpire: 604800,
		SOAMinimum: minimum, SOATTL: soaTTL,
	}, recs)
	return &z
}

// reply builds the response message the server layer hands the zone: a reply
// to a question for qname/qtype, with nothing filled in yet.
func reply(qname string, qtype uint16) *dns.Msg {
	req := new(dns.Msg)
	req.SetQuestion(dns.Fqdn(qname), qtype)
	m := new(dns.Msg)
	m.SetReply(req)
	return m
}

func TestAnswerReturnsRecord(t *testing.T) {
	z := newZone(t, store.ZoneRecord{Name: "bifrost", Type: "A", TTL: 3600, RData: "57.129.69.158", Enabled: true})
	m := reply("bifrost.e412.in.", dns.TypeA)
	z.Answer(m, "bifrost.e412.in", dns.TypeA, testNow)
	if m.Rcode != dns.RcodeSuccess || len(m.Answer) != 1 || !m.Authoritative {
		t.Fatalf("got rcode=%d answers=%d aa=%v; want NOERROR/1/true", m.Rcode, len(m.Answer), m.Authoritative)
	}
}

// NODATA: the name exists, the type does not. NOERROR with an empty ANSWER
// and the SOA in AUTHORITY — the SOA is what lets a resolver cache the
// absence instead of re-asking on every lookup.
func TestAnswerNoDataCarriesSOA(t *testing.T) {
	z := newZone(t, store.ZoneRecord{Name: "bifrost", Type: "A", TTL: 3600, RData: "57.129.69.158", Enabled: true})
	m := reply("bifrost.e412.in.", dns.TypeAAAA)
	z.Answer(m, "bifrost.e412.in", dns.TypeAAAA, testNow)
	if m.Rcode != dns.RcodeSuccess || len(m.Answer) != 0 {
		t.Fatalf("got rcode=%d answers=%d; want NOERROR with no answers", m.Rcode, len(m.Answer))
	}
	if len(m.Ns) != 1 || m.Ns[0].Header().Rrtype != dns.TypeSOA {
		t.Fatalf("AUTHORITY = %v; want one SOA", m.Ns)
	}
}

// NXDOMAIN, and the reason this milestone exists: before zones this query
// was forwarded upstream, leaking an internal name and letting a public
// record shadow an undefined internal one.
func TestAnswerNXDomainCarriesSOA(t *testing.T) {
	z := newZone(t, store.ZoneRecord{Name: "bifrost", Type: "A", TTL: 3600, RData: "57.129.69.158", Enabled: true})
	m := reply("nothere.e412.in.", dns.TypeA)
	z.Answer(m, "nothere.e412.in", dns.TypeA, testNow)
	if m.Rcode != dns.RcodeNameError {
		t.Fatalf("rcode = %d, want NXDOMAIN", m.Rcode)
	}
	if len(m.Ns) != 1 || m.Ns[0].Header().Rrtype != dns.TypeSOA {
		t.Fatalf("AUTHORITY = %v; want one SOA", m.Ns)
	}
}

func TestWildcardSynthesises(t *testing.T) {
	z := newZone(t, store.ZoneRecord{Name: "*.nexus", Type: "A", TTL: 3600, RData: "192.168.160.200", Enabled: true})
	m := reply("git.nexus.e412.in.", dns.TypeA)
	z.Answer(m, "git.nexus.e412.in", dns.TypeA, testNow)
	if len(m.Answer) != 1 {
		t.Fatalf("answers = %v, want the synthesised A", m.Answer)
	}
	if m.Answer[0].Header().Name != "git.nexus.e412.in." {
		t.Errorf("synthesised name = %q, want the queried name", m.Answer[0].Header().Name)
	}
}

// RFC 4592 §2.2: a wildcard must not answer for a name that exists with
// other types. Getting this wrong makes every NODATA under the wildcard
// silently return the wildcard's address instead.
func TestWildcardDoesNotCoverExistingName(t *testing.T) {
	z := newZone(t,
		store.ZoneRecord{Name: "*", Type: "A", TTL: 3600, RData: "192.168.150.28", Enabled: true},
		store.ZoneRecord{Name: "api", Type: "TXT", TTL: 3600, RData: `"hello"`, Enabled: true},
	)
	m := reply("api.e412.in.", dns.TypeA)
	z.Answer(m, "api.e412.in", dns.TypeA, testNow)
	if len(m.Answer) != 0 || m.Rcode != dns.RcodeSuccess {
		t.Fatalf("got rcode=%d answers=%v; want NODATA, not the wildcard", m.Rcode, m.Answer)
	}
}

func TestDisabledRecordIsInvisible(t *testing.T) {
	z := newZone(t, store.ZoneRecord{Name: "bifrost", Type: "A", TTL: 3600, RData: "57.129.69.158", Enabled: false})
	m := reply("bifrost.e412.in.", dns.TypeA)
	z.Answer(m, "bifrost.e412.in", dns.TypeA, testNow)
	if m.Rcode != dns.RcodeNameError {
		t.Errorf("rcode = %d, want NXDOMAIN — a disabled record must not exist", m.Rcode)
	}
}

// An NS below the apex is a zone cut: we are not authoritative below it, so
// we refer rather than answer.
func TestDelegationReturnsReferral(t *testing.T) {
	z := newZone(t, store.ZoneRecord{Name: "sub", Type: "NS", TTL: 3600, RData: "ns1.other.test.", Enabled: true})
	m := reply("host.sub.e412.in.", dns.TypeA)
	z.Answer(m, "host.sub.e412.in", dns.TypeA, testNow)
	if m.Authoritative {
		t.Error("aa set on a referral")
	}
	if len(m.Ns) != 1 || m.Ns[0].Header().Rrtype != dns.TypeNS {
		t.Fatalf("AUTHORITY = %v; want the NS referral", m.Ns)
	}
}

// RFC 2308 §5: the negative answer's TTL is min(SOA.MINIMUM, the SOA
// record's own TTL). That number is how long every resolver on the network
// caches the absence, so taking the larger of the two would keep a
// newly-added record invisible for as long as the larger one.
func TestNegativeTTLIsMinOfMinimumAndSOATTL(t *testing.T) {
	// SOA record TTL 300, MINIMUM 900 — the negative TTL must be 300.
	// This test is the reason soa_ttl exists: with one column the two can
	// never differ and the min() is untestable.
	z := newZoneWithSOA(t, 300, 900)
	m := reply("nothere.e412.in.", dns.TypeA)
	z.Answer(m, "nothere.e412.in", dns.TypeA, testNow)
	if got := m.Ns[0].Header().Ttl; got != 300 {
		t.Errorf("negative TTL = %d, want 300 (min of SOA TTL and MINIMUM)", got)
	}
}

// The other direction of the same rule: whichever of the two is smaller
// wins, so a test that only ever set the SOA's TTL lower would also pass an
// implementation that just returned SOATTL.
func TestNegativeTTLTakesMinimumWhenItIsSmaller(t *testing.T) {
	z := newZoneWithSOA(t, 900, 60)
	m := reply("nothere.e412.in.", dns.TypeA)
	z.Answer(m, "nothere.e412.in", dns.TypeA, testNow)
	if got := m.Ns[0].Header().Ttl; got != 60 {
		t.Errorf("negative TTL = %d, want 60 (min of SOA TTL and MINIMUM)", got)
	}
	if soa, ok := m.Ns[0].(*dns.SOA); !ok || soa.Minttl != 60 {
		t.Errorf("AUTHORITY SOA = %v; MINIMUM rdata must still be the stored 60", m.Ns[0])
	}
}

// RFC 4592 §2.1.1: an asterisk is a wildcard only as the leftmost label.
// "a.*.e412.in" is an ordinary name that happens to contain an asterisk —
// it answers for itself and synthesises for nothing.
func TestNonLeftmostAsteriskIsALiteralName(t *testing.T) {
	z := newZone(t, store.ZoneRecord{Name: "a.*", Type: "A", TTL: 3600, RData: "10.0.0.7", Enabled: true})

	m := reply("a.b.e412.in.", dns.TypeA)
	z.Answer(m, "a.b.e412.in", dns.TypeA, testNow)
	if len(m.Answer) != 0 {
		t.Errorf("answers = %v; a non-leftmost asterisk must not match anything", m.Answer)
	}
	// NODATA rather than NXDOMAIN, and not because anything synthesised:
	// "a.*" puts something below "*.e412.in", which makes that name exist as
	// an empty non-terminal — see TestWildcardThatIsOnlyAnEmptyNonTerminalIsNoData.
	if m.Rcode != dns.RcodeSuccess {
		t.Errorf("rcode = %d, want NOERROR", m.Rcode)
	}

	lit := reply("a.*.e412.in.", dns.TypeA)
	z.Answer(lit, "a.*.e412.in", dns.TypeA, testNow)
	if len(lit.Answer) != 1 {
		t.Errorf("answers for the literal name = %v, want the stored A", lit.Answer)
	}
}

// RFC 8020: NXDOMAIN means this name and everything below it is absent. So
// a name that only exists because something below it does — an empty
// non-terminal — is NODATA, never NXDOMAIN. Answering NXDOMAIN here tells
// every RFC 8020 resolver to stop asking for the names below it too, which
// takes out the record that does exist.
func TestEmptyNonTerminalIsNoDataNotNXDomain(t *testing.T) {
	z := newZone(t, store.ZoneRecord{Name: "host.sub", Type: "A", TTL: 3600, RData: "10.0.0.9", Enabled: true})
	m := reply("sub.e412.in.", dns.TypeA)
	z.Answer(m, "sub.e412.in", dns.TypeA, testNow)
	if m.Rcode != dns.RcodeSuccess || len(m.Answer) != 0 {
		t.Fatalf("got rcode=%d answers=%v; want NODATA for an empty non-terminal", m.Rcode, m.Answer)
	}
	if len(m.Ns) != 1 || m.Ns[0].Header().Rrtype != dns.TypeSOA {
		t.Fatalf("AUTHORITY = %v; want one SOA", m.Ns)
	}
}

// RFC 4592 §3.3.1: synthesis comes from "*." + the closest encloser, not
// from any wildcard further up. sub.e412.in exists (as an empty
// non-terminal), so it is the closest encloser for x.sub.e412.in and
// *.e412.in is out of reach — wildcards do not match across a name that
// exists.
func TestWildcardDoesNotReachAcrossACloserEncloser(t *testing.T) {
	z := newZone(t,
		store.ZoneRecord{Name: "*", Type: "A", TTL: 3600, RData: "192.168.150.28", Enabled: true},
		store.ZoneRecord{Name: "host.sub", Type: "A", TTL: 3600, RData: "10.0.0.9", Enabled: true},
	)
	m := reply("x.sub.e412.in.", dns.TypeA)
	z.Answer(m, "x.sub.e412.in", dns.TypeA, testNow)
	if m.Rcode != dns.RcodeNameError || len(m.Answer) != 0 {
		t.Errorf("got rcode=%d answers=%v; want NXDOMAIN — *.e412.in cannot reach past sub.e412.in", m.Rcode, m.Answer)
	}
}

// A wildcard makes the queried name exist, so a query for a type it does not
// hold is NODATA — not NXDOMAIN, and not a fall-through to a wildcard higher
// up.
func TestWildcardMatchWithWrongTypeIsNoData(t *testing.T) {
	z := newZone(t, store.ZoneRecord{Name: "*.nexus", Type: "A", TTL: 3600, RData: "192.168.160.200", Enabled: true})
	m := reply("git.nexus.e412.in.", dns.TypeTXT)
	z.Answer(m, "git.nexus.e412.in", dns.TypeTXT, testNow)
	if m.Rcode != dns.RcodeSuccess || len(m.Answer) != 0 {
		t.Fatalf("got rcode=%d answers=%v; want NODATA", m.Rcode, m.Answer)
	}
	if len(m.Ns) != 1 || m.Ns[0].Header().Rrtype != dns.TypeSOA {
		t.Fatalf("AUTHORITY = %v; want one SOA", m.Ns)
	}
}

// RFC 1034 §3.6.2: a CNAME is the name's only data, so a query for another
// type follows it. An in-zone target is resolved here and appended, because
// sending back a CNAME whose answer we are holding costs the client a whole
// extra round trip.
func TestCNAMEIsFollowedInZone(t *testing.T) {
	z := newZone(t,
		store.ZoneRecord{Name: "www", Type: "CNAME", TTL: 3600, RData: "bifrost.e412.in.", Enabled: true},
		store.ZoneRecord{Name: "bifrost", Type: "A", TTL: 3600, RData: "57.129.69.158", Enabled: true},
	)
	m := reply("www.e412.in.", dns.TypeA)
	z.Answer(m, "www.e412.in", dns.TypeA, testNow)
	if m.Rcode != dns.RcodeSuccess || len(m.Answer) != 2 || !m.Authoritative {
		t.Fatalf("got rcode=%d answers=%v aa=%v; want the CNAME and the target's A", m.Rcode, m.Answer, m.Authoritative)
	}
	if m.Answer[0].Header().Rrtype != dns.TypeCNAME {
		t.Errorf("ANSWER[0] = %v, want the CNAME first", m.Answer[0])
	}
	a, ok := m.Answer[1].(*dns.A)
	if !ok || a.A.String() != "57.129.69.158" || a.Hdr.Name != "bifrost.e412.in." {
		t.Errorf("ANSWER[1] = %v, want bifrost.e412.in.'s A", m.Answer[1])
	}
}

// An out-of-zone target is not ours to resolve: the CNAME is an
// authoritative answer on its own, and the rest is left to the pipeline.
func TestCNAMEOutOfZoneStopsAtTheCNAME(t *testing.T) {
	z := newZone(t, store.ZoneRecord{Name: "www", Type: "CNAME", TTL: 3600, RData: "elsewhere.example.com.", Enabled: true})
	m := reply("www.e412.in.", dns.TypeA)
	z.Answer(m, "www.e412.in", dns.TypeA, testNow)
	if m.Rcode != dns.RcodeSuccess || len(m.Answer) != 1 {
		t.Fatalf("got rcode=%d answers=%v; want just the CNAME", m.Rcode, m.Answer)
	}
	if cn, ok := m.Answer[0].(*dns.CNAME); !ok || cn.Target != "elsewhere.example.com." {
		t.Errorf("ANSWER[0] = %v, want the CNAME to elsewhere.example.com.", m.Answer[0])
	}
	if len(m.Ns) != 0 {
		t.Errorf("AUTHORITY = %v; an unfinished CNAME is not a negative answer", m.Ns)
	}
}

// A CNAME whose in-zone target does not exist is still NXDOMAIN, with the
// CNAME kept in ANSWER: the rcode describes the end of the chain.
func TestCNAMEToMissingInZoneNameIsNXDomain(t *testing.T) {
	z := newZone(t, store.ZoneRecord{Name: "www", Type: "CNAME", TTL: 3600, RData: "gone.e412.in.", Enabled: true})
	m := reply("www.e412.in.", dns.TypeA)
	z.Answer(m, "www.e412.in", dns.TypeA, testNow)
	if m.Rcode != dns.RcodeNameError || len(m.Answer) != 1 {
		t.Fatalf("got rcode=%d answers=%v; want NXDOMAIN with the CNAME kept", m.Rcode, m.Answer)
	}
}

// Asking for the CNAME itself is answered directly — following it would
// return data for a name the client did not ask about.
func TestCNAMEQueriedDirectlyIsNotFollowed(t *testing.T) {
	z := newZone(t,
		store.ZoneRecord{Name: "www", Type: "CNAME", TTL: 3600, RData: "bifrost.e412.in.", Enabled: true},
		store.ZoneRecord{Name: "bifrost", Type: "A", TTL: 3600, RData: "57.129.69.158", Enabled: true},
	)
	m := reply("www.e412.in.", dns.TypeCNAME)
	z.Answer(m, "www.e412.in", dns.TypeCNAME, testNow)
	if len(m.Answer) != 1 || m.Answer[0].Header().Rrtype != dns.TypeCNAME {
		t.Fatalf("answers = %v, want exactly the CNAME", m.Answer)
	}
}

// A CNAME chain that points back at itself must terminate. Without a bound
// the chase recurses until the process dies, which is a remote crash
// triggered by one bad record.
func TestCNAMELoopTerminates(t *testing.T) {
	z := newZone(t,
		store.ZoneRecord{Name: "a", Type: "CNAME", TTL: 60, RData: "b.e412.in.", Enabled: true},
		store.ZoneRecord{Name: "b", Type: "CNAME", TTL: 60, RData: "a.e412.in.", Enabled: true},
	)
	m := reply("a.e412.in.", dns.TypeA)
	z.Answer(m, "a.e412.in", dns.TypeA, testNow)
	if len(m.Answer) > 16 {
		t.Errorf("answers = %d, want a bounded chase", len(m.Answer))
	}
}

// Glue is the point of a referral: without an address for the nameserver
// the client has just been told to ask, it cannot make progress. Only
// in-zone addresses go in — an address for a nameserver named outside this
// zone is not ours to vouch for.
func TestReferralCarriesInZoneGlueOnly(t *testing.T) {
	z := newZone(t,
		store.ZoneRecord{Name: "sub", Type: "NS", TTL: 3600, RData: "ns1.sub.e412.in.", Enabled: true},
		store.ZoneRecord{Name: "ns1.sub", Type: "A", TTL: 3600, RData: "10.0.0.53", Enabled: true},
	)
	m := reply("host.sub.e412.in.", dns.TypeA)
	z.Answer(m, "host.sub.e412.in", dns.TypeA, testNow)
	if len(m.Extra) != 1 {
		t.Fatalf("ADDITIONAL = %v, want ns1.sub.e412.in.'s A as glue", m.Extra)
	}
	if a, ok := m.Extra[0].(*dns.A); !ok || a.Hdr.Name != "ns1.sub.e412.in." || a.A.String() != "10.0.0.53" {
		t.Errorf("glue = %v, want ns1.sub.e412.in. A 10.0.0.53", m.Extra[0])
	}

	out := newZone(t, store.ZoneRecord{Name: "sub", Type: "NS", TTL: 3600, RData: "ns1.other.test.", Enabled: true})
	om := reply("host.sub.e412.in.", dns.TypeA)
	out.Answer(om, "host.sub.e412.in", dns.TypeA, testNow)
	if len(om.Extra) != 0 {
		t.Errorf("ADDITIONAL = %v, want none for an out-of-zone nameserver", om.Extra)
	}
}

// An apex NS answer carries the addresses of its own in-zone nameservers,
// the way ns1.google.com does for google.com. This is not the referral path
// — the apex is this zone's authority, not a cut — but the additional
// section serves the same purpose (RFC 1035 §3.3.11): a client handed a
// nameserver's name and no address has to go find one, and a stub zone
// fetching this NS set cannot go find one, because resolving an in-zone
// nameserver would route straight back into the zone it is trying to reach.
//
// The out-of-zone half is the same rule the referral obeys: an address for
// a nameserver named outside this zone is not ours to vouch for. A real
// master behaves exactly this way — a.iana-servers.net returns no
// additional section for example.com, whose nameservers are all out-of-zone.
func TestApexNSAnswerCarriesInZoneGlueOnly(t *testing.T) {
	// TWO nameservers, and the second is what makes the loop and the dedup
	// testable. With one, an implementation that handles only the first
	// nameserver and drops the dedup entirely passes the whole repository —
	// and a one-nameserver zone is the case that never occurs in practice.
	// NS1 in different case is stored verbatim and must not produce a second
	// copy of ns1's addresses: DNS compares names case-insensitively.
	z := newZone(t,
		store.ZoneRecord{Name: "@", Type: "NS", TTL: 3600, RData: "ns1.e412.in.", Enabled: true},
		store.ZoneRecord{Name: "@", Type: "NS", TTL: 3600, RData: "ns2.e412.in.", Enabled: true},
		store.ZoneRecord{Name: "@", Type: "NS", TTL: 3600, RData: "NS1.e412.in.", Enabled: true},
		store.ZoneRecord{Name: "ns1", Type: "A", TTL: 3600, RData: "10.0.0.53", Enabled: true},
		store.ZoneRecord{Name: "ns1", Type: "AAAA", TTL: 3600, RData: "fd00::53", Enabled: true},
		store.ZoneRecord{Name: "ns2", Type: "A", TTL: 3600, RData: "10.0.0.54", Enabled: true},
	)
	m := reply("e412.in.", dns.TypeNS)
	z.Answer(m, "e412.in", dns.TypeNS, testNow)
	if len(m.Answer) != 3 {
		t.Fatalf("ANSWER = %v, want all three apex NS records", m.Answer)
	}
	if !m.Authoritative {
		t.Error("aa = false, want an authoritative answer: the apex is not a cut")
	}
	if len(m.Ns) != 0 {
		t.Errorf("AUTHORITY = %v, want none: this is an answer, not a referral", m.Ns)
	}
	got := make(map[string]int)
	for _, rr := range m.Extra {
		switch rr := rr.(type) {
		case *dns.A:
			got[rr.Hdr.Name+" A "+rr.A.String()]++
		case *dns.AAAA:
			got[rr.Hdr.Name+" AAAA "+rr.AAAA.String()]++
		}
	}
	want := map[string]int{
		"ns1.e412.in. A 10.0.0.53":   1,
		"ns1.e412.in. AAAA fd00::53": 1,
		"ns2.e412.in. A 10.0.0.54":   1,
	}
	if !maps.Equal(got, want) {
		t.Errorf("ADDITIONAL = %v, want exactly one A/AAAA per in-zone nameserver", m.Extra)
	}

	// Out-of-zone nameserver, and the fixture is deliberately hostile: this
	// zone holds a record *named* "ns1.other.test", which is the name
	// ns1.other.test.e412.in. — a different name that relativises to the
	// same string the out-of-zone target does, because RelName leaves a name
	// it cannot strip the apex from unchanged.
	//
	// Without glue's ownership check that record's address is attached as
	// glue for ns1.other.test., asserting an address for a name this zone
	// does not own. A plain out-of-zone fixture cannot catch that: the
	// relative lookup finds nothing either way, so the assertion passes
	// whether the check is there or not.
	out := newZone(t,
		store.ZoneRecord{Name: "@", Type: "NS", TTL: 3600, RData: "ns1.other.test.", Enabled: true},
		store.ZoneRecord{Name: "ns1.other.test", Type: "A", TTL: 3600, RData: "10.0.0.66", Enabled: true},
	)
	om := reply("e412.in.", dns.TypeNS)
	out.Answer(om, "e412.in", dns.TypeNS, testNow)
	if len(om.Extra) != 0 {
		t.Errorf("ADDITIONAL = %v, want none: ns1.other.test. is not this zone's to address", om.Extra)
	}
}

// The cut wins over anything stored below it. Records under a delegated
// name are the child's to serve; answering them from here would hand out
// data the child may have replaced.
func TestDelegationWinsOverRecordsBelowTheCut(t *testing.T) {
	z := newZone(t,
		store.ZoneRecord{Name: "sub", Type: "NS", TTL: 3600, RData: "ns1.other.test.", Enabled: true},
		store.ZoneRecord{Name: "host.sub", Type: "A", TTL: 3600, RData: "10.0.0.9", Enabled: true},
	)
	m := reply("host.sub.e412.in.", dns.TypeA)
	z.Answer(m, "host.sub.e412.in", dns.TypeA, testNow)
	if len(m.Answer) != 0 || m.Authoritative {
		t.Fatalf("got answers=%v aa=%v; want a referral, not an answer", m.Answer, m.Authoritative)
	}
}

// NS at the apex is this zone's own authority (RFC 2181 §10.1), not a zone
// cut. Treating it as one would turn every miss in the zone into a referral
// to ourselves.
func TestApexNSIsNotADelegation(t *testing.T) {
	z := newZone(t, store.ZoneRecord{Name: "@", Type: "NS", TTL: 3600, RData: "ns1.e412.in.", Enabled: true})
	m := reply("nothere.e412.in.", dns.TypeA)
	z.Answer(m, "nothere.e412.in", dns.TypeA, testNow)
	if m.Rcode != dns.RcodeNameError || !m.Authoritative {
		t.Fatalf("got rcode=%d aa=%v; want an authoritative NXDOMAIN", m.Rcode, m.Authoritative)
	}
}

// The SOA lives on the zones row rather than in zone_records, so it has to
// be served from there — otherwise `dig SOA e412.in` is NODATA for a zone
// that plainly has one, and Milestone D's transfers have nothing to start
// from.
func TestApexSOAIsAnswered(t *testing.T) {
	z := newZone(t, store.ZoneRecord{Name: "bifrost", Type: "A", TTL: 3600, RData: "57.129.69.158", Enabled: true})
	m := reply("e412.in.", dns.TypeSOA)
	z.Answer(m, "e412.in", dns.TypeSOA, testNow)
	if len(m.Answer) != 1 || !m.Authoritative {
		t.Fatalf("answers = %v aa=%v; want the zone's SOA", m.Answer, m.Authoritative)
	}
	soa, ok := m.Answer[0].(*dns.SOA)
	if !ok || soa.Serial != 2026080801 || soa.Hdr.Name != "e412.in." {
		t.Errorf("ANSWER[0] = %v, want e412.in.'s SOA", m.Answer[0])
	}
}

// The apex exists by definition — its SOA is right there — so a query for a
// type it has no records of is NODATA. NXDOMAIN would deny the whole zone.
func TestApexWithoutRecordsIsNoDataNotNXDomain(t *testing.T) {
	z := newZone(t)
	m := reply("e412.in.", dns.TypeA)
	z.Answer(m, "e412.in", dns.TypeA, testNow)
	if m.Rcode != dns.RcodeSuccess {
		t.Fatalf("rcode = %d, want NODATA — the apex always exists", m.Rcode)
	}
	if len(m.Ns) != 1 || m.Ns[0].Header().Rrtype != dns.TypeSOA {
		t.Fatalf("AUTHORITY = %v; want one SOA", m.Ns)
	}
}

// Names are compared case-insensitively (RFC 4343), and the answer echoes
// the case the client asked in.
func TestAnswerIsCaseInsensitive(t *testing.T) {
	z := newZone(t, store.ZoneRecord{Name: "BiFrOsT", Type: "A", TTL: 3600, RData: "57.129.69.158", Enabled: true})
	m := reply("BIFROST.E412.IN.", dns.TypeA)
	z.Answer(m, "BIFROST.E412.IN.", dns.TypeA, testNow)
	if len(m.Answer) != 1 {
		t.Fatalf("answers = %v, want the A regardless of case", m.Answer)
	}
	if got := m.Answer[0].Header().Name; got != "BIFROST.E412.IN." {
		t.Errorf("owner name = %q, want the queried name echoed back", got)
	}
}

// A forwarder zone names somewhere else to ask rather than holding data, so
// it reports that it did not answer and leaves the message for the rest of
// the pipeline. Milestone D gives it its behaviour; until then it must not
// silently NXDOMAIN every name under the suffix.
func TestForwarderZoneDoesNotAnswer(t *testing.T) {
	z := newZone(t)
	z.Type = "forwarder"
	m := reply("nothere.e412.in.", dns.TypeA)
	if handled := z.Answer(m, "nothere.e412.in", dns.TypeA, testNow); handled {
		t.Fatal("a forwarder zone reported that it answered")
	}
	if m.Rcode != dns.RcodeSuccess || len(m.Answer) != 0 || len(m.Ns) != 0 || m.Authoritative {
		t.Errorf("message touched by a forwarder zone: %v", m)
	}
}

// A row whose rdata does not parse cannot be served. It reads as absent
// rather than taking the rest of the RRset down with it — writes go through
// the same dns.NewRR, so a row like this arrived around the API.
func TestUnparseableRDataReadsAsAbsent(t *testing.T) {
	z := newZone(t,
		store.ZoneRecord{Name: "bifrost", Type: "A", TTL: 3600, RData: "not-an-ip", Enabled: true},
		store.ZoneRecord{Name: "bifrost", Type: "A", TTL: 3600, RData: "57.129.69.158", Enabled: true},
	)
	m := reply("bifrost.e412.in.", dns.TypeA)
	z.Answer(m, "bifrost.e412.in", dns.TypeA, testNow)
	if len(m.Answer) != 1 {
		t.Fatalf("answers = %v, want the one servable A", m.Answer)
	}
}

// newSecondary builds a secondary zone with the two timestamps that decide
// whether it may answer at all: refreshedAt is when a transfer last landed
// (0 = never), expiresAt the deadline past which its data can no longer be
// confirmed (0 = no deadline recorded yet).
func newSecondary(t *testing.T, refreshedAt, expiresAt int64, recs ...store.ZoneRecord) *zones.Zone {
	t.Helper()
	z := zones.NewZone(store.Zone{
		Name: testApex, Type: "secondary", Enabled: true,
		SOANS: "ns1." + testApex, SOAMbox: "hostadmin." + testApex,
		SOASerial: 2026080801, SOARefresh: 900, SOARetry: 300, SOAExpire: 604800,
		SOAMinimum: 900, SOATTL: 900,
		Primaries: "192.168.150.5", RefreshedAt: refreshedAt, ExpiresAt: expiresAt,
	}, recs)
	return &z
}

// A secondary that has never transferred holds nothing, and "nothing" must
// not be spoken as authority. Answering NXDOMAIN + SOA here is a claim that
// every name under the apex does not exist — RFC 8020 makes that a claim
// about the whole subtree — so the moment such a zone is created it would
// black-hole its own suffix for every resolver that believed it. It is also
// not a fall-through: forwarding would leak an internal name upstream and
// let a public record shadow it, which is the leak zones exist to close.
func TestSecondaryThatHasNeverTransferredServesNothing(t *testing.T) {
	z := newSecondary(t, 0, 0)
	m := reply("bifrost.e412.in.", dns.TypeA)
	if !z.Answer(m, "bifrost.e412.in", dns.TypeA, testNow) {
		t.Fatal("handled = false; the query must not fall through to the forwarder")
	}
	if m.Rcode != dns.RcodeServerFailure {
		t.Errorf("rcode = %d, want SERVFAIL", m.Rcode)
	}
	if m.Authoritative {
		t.Error("aa = true; a zone with no data has no authority to assert")
	}
	if len(m.Answer) != 0 || len(m.Ns) != 0 {
		t.Errorf("answer=%v authority=%v; want both empty — an SOA here would cache the denial", m.Answer, m.Ns)
	}
}

// RFC 1034 §4.3.5: past the SOA expire a secondary can no longer confirm its
// data is current, and serving it anyway is worse than serving none because
// the resolver asking has no way to tell.
func TestSecondaryPastItsExpiryServesNothing(t *testing.T) {
	rec := store.ZoneRecord{Name: "bifrost", Type: "A", TTL: 3600, RData: "57.129.69.158", Enabled: true}
	z := newSecondary(t, testNow-1000, testNow-1, rec)
	m := reply("bifrost.e412.in.", dns.TypeA)
	if !z.Answer(m, "bifrost.e412.in", dns.TypeA, testNow) {
		t.Fatal("handled = false; an expired zone must not fall through either")
	}
	if m.Rcode != dns.RcodeServerFailure || len(m.Answer) != 0 {
		t.Fatalf("rcode = %d answers = %v; want SERVFAIL and nothing served", m.Rcode, m.Answer)
	}
}

// The other side of the same rule: a secondary that has transferred and is
// inside its expiry is an ordinary authoritative zone.
func TestSecondaryThatHasTransferredAnswersFromItsRecords(t *testing.T) {
	rec := store.ZoneRecord{Name: "bifrost", Type: "A", TTL: 3600, RData: "57.129.69.158", Enabled: true}
	z := newSecondary(t, testNow-1000, testNow+1000, rec)

	m := reply("bifrost.e412.in.", dns.TypeA)
	z.Answer(m, "bifrost.e412.in", dns.TypeA, testNow)
	if m.Rcode != dns.RcodeSuccess || len(m.Answer) != 1 || !m.Authoritative {
		t.Fatalf("rcode=%d answers=%d aa=%v; want NOERROR/1/true", m.Rcode, len(m.Answer), m.Authoritative)
	}
	// And a name it does not hold is an authoritative NXDOMAIN, exactly as
	// it would be from a primary — the denial is only wrong when the zone
	// has no confirmed data behind it.
	nx := reply("nothere.e412.in.", dns.TypeA)
	z.Answer(nx, "nothere.e412.in", dns.TypeA, testNow)
	if nx.Rcode != dns.RcodeNameError || len(nx.Ns) != 1 {
		t.Fatalf("rcode=%d authority=%v; want NXDOMAIN carrying the SOA", nx.Rcode, nx.Ns)
	}
}

// A secondary with a deadline it has not reached is serving; the expiry
// comparison must not be an off-by-one that retires a zone a millisecond
// early, nor a `!= 0` test that retires every zone Task 3 has not yet
// stamped a deadline onto.
func TestSecondaryExpiryBoundary(t *testing.T) {
	for _, tc := range []struct {
		name      string
		expiresAt int64
		wantRcode int
	}{
		{"one ms before the deadline", testNow + 1, dns.RcodeSuccess},
		{"exactly at the deadline", testNow, dns.RcodeServerFailure},
		{"no deadline recorded", 0, dns.RcodeSuccess},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := store.ZoneRecord{Name: "bifrost", Type: "A", TTL: 3600, RData: "57.129.69.158", Enabled: true}
			z := newSecondary(t, testNow-1000, tc.expiresAt, rec)
			m := reply("bifrost.e412.in.", dns.TypeA)
			z.Answer(m, "bifrost.e412.in", dns.TypeA, testNow)
			if m.Rcode != tc.wantRcode {
				t.Fatalf("rcode = %d, want %d", m.Rcode, tc.wantRcode)
			}
		})
	}
}

// RFC 1035 §5.1 escaping makes `foo\.e412.in.` a two-label name — a child of
// "in", not of "e412.in" — but a byte-suffix test reads it as ours. Chasing
// it re-entered the zone with the relative name `foo\` and answered NXDOMAIN
// carrying this zone's SOA for a name this zone does not hold, which every
// RFC 8020 resolver caches as "and nothing below it either".
func TestCNAMEToAnEscapedNameIsOutOfZone(t *testing.T) {
	z := newZone(t, store.ZoneRecord{Name: "git", Type: "CNAME", TTL: 300, RData: `foo\.e412.in.`, Enabled: true})
	m := reply("git.e412.in.", dns.TypeA)
	z.Answer(m, "git.e412.in", dns.TypeA, testNow)
	if m.Rcode != dns.RcodeSuccess {
		t.Errorf("rcode = %d, want NOERROR — the target is outside this zone, so the rest is the pipeline's", m.Rcode)
	}
	if len(m.Answer) != 1 || m.Answer[0].Header().Rrtype != dns.TypeCNAME {
		t.Errorf("ANSWER = %v, want the CNAME alone", m.Answer)
	}
	if len(m.Ns) != 0 {
		t.Errorf("AUTHORITY = %v; this zone has nothing to say about a name it does not hold", m.Ns)
	}
}

// RFC 4592 §2.2.3: a wildcard that owns no records but has something below
// it is an empty non-terminal like any other, so the source of synthesis
// *exists*. RFC 1034 §4.3.2 step 3(c) then matches no RRs at it and the
// answer is NODATA — answering NXDOMAIN would be a claim (RFC 8020) that
// nothing under this zone exists, made by a zone that holds a record.
func TestWildcardThatIsOnlyAnEmptyNonTerminalIsNoData(t *testing.T) {
	z := newZone(t, store.ZoneRecord{Name: "a.*", Type: "A", TTL: 3600, RData: "10.0.0.7", Enabled: true})
	m := reply("foo.e412.in.", dns.TypeA)
	z.Answer(m, "foo.e412.in", dns.TypeA, testNow)
	if m.Rcode != dns.RcodeSuccess || len(m.Answer) != 0 {
		t.Fatalf("got rcode=%d answers=%v; want NODATA — *.e412.in exists as an empty non-terminal", m.Rcode, m.Answer)
	}
	if len(m.Ns) != 1 || m.Ns[0].Header().Rrtype != dns.TypeSOA {
		t.Fatalf("AUTHORITY = %v; want one SOA", m.Ns)
	}
}
