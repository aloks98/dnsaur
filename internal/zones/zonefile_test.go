package zones_test

import (
	"strings"
	"testing"

	"github.com/aloks98/dnsaur/internal/store"
	"github.com/aloks98/dnsaur/internal/zones"
	"github.com/miekg/dns"
)

func testZone() store.Zone {
	// Every SOA field gets a distinct value. If any two were equal, a test
	// that swapped them (e.g. SOATTL and SOAMinimum, or Refresh and Retry)
	// could still pass — the exact class of bug this test exists to catch.
	return store.Zone{
		ID: 1, Name: "e412.in", Type: "primary", Enabled: true,
		SOANS: "ns.e412.in", SOAMbox: "hostadmin.e412.in",
		SOASerial: 7, SOARefresh: 7200, SOARetry: 3600,
		SOAExpire: 604800, SOAMinimum: 86400, SOATTL: 900,
	}
}

// The rendered file must parse back with miekg — that is the only bar that
// matters, and it is stronger than comparing against a golden string, which
// would pin whitespace rather than correctness.
func TestRenderRoundTripsThroughTheParser(t *testing.T) {
	recs := []store.ZoneRecord{
		{Name: "@", Type: "NS", TTL: 3600, RData: "ns.e412.in.", Enabled: true},
		{Name: "bifrost", Type: "A", TTL: 300, RData: "57.129.69.158", Enabled: true},
		{Name: "@", Type: "MX", TTL: 3600, RData: "10 mail.e412.in.", Enabled: true},
		{Name: "@", Type: "TXT", TTL: 3600, RData: `"v=spf1 -all"`, Enabled: true},
		{Name: "*.nexus", Type: "A", TTL: 300, RData: "192.168.160.200", Enabled: true},
	}
	out := zones.Render(testZone(), recs)

	zp := dns.NewZoneParser(strings.NewReader(out), "", "")
	var got []dns.RR
	for rr, ok := zp.Next(); ok; rr, ok = zp.Next() {
		got = append(got, rr)
	}
	if err := zp.Err(); err != nil {
		t.Fatalf("rendered file does not parse: %v\n---\n%s", err, out)
	}
	// 5 records + the SOA.
	if len(got) != 6 {
		t.Fatalf("parsed %d RRs, want 6:\n%s", len(got), out)
	}
	var haveSOA bool
	for _, rr := range got {
		if rr.Header().Rrtype == dns.TypeSOA {
			haveSOA = true
			soa := rr.(*dns.SOA)
			// Every field checked, not just Serial/Ns/Mbox: with distinct
			// values in testZone(), a SOATTL/SOAMinimum swap or a
			// Refresh/Retry transposition in Render now fails here.
			if soa.Hdr.Ttl != 900 || soa.Ns != "ns.e412.in." || soa.Mbox != "hostadmin.e412.in." ||
				soa.Serial != 7 || soa.Refresh != 7200 || soa.Retry != 3600 ||
				soa.Expire != 604800 || soa.Minttl != 86400 {
				t.Errorf("SOA = %+v; want ttl 900, ns.e412.in., hostadmin.e412.in., serial 7, refresh 7200, retry 3600, expire 604800, minttl 86400", soa)
			}
		}
	}
	if !haveSOA {
		t.Error("no SOA in the rendered file")
	}
}

// A zone file has no concept of a disabled record. Emitting one would
// silently enable it on whatever imports the file.
func TestRenderOmitsDisabledRecords(t *testing.T) {
	recs := []store.ZoneRecord{
		{Name: "on", Type: "A", TTL: 300, RData: "1.2.3.4", Enabled: true},
		{Name: "off", Type: "A", TTL: 300, RData: "5.6.7.8", Enabled: false},
	}
	out := zones.Render(testZone(), recs)
	if strings.Contains(out, "5.6.7.8") {
		t.Errorf("disabled record was rendered:\n%s", out)
	}
	if !strings.Contains(out, "1.2.3.4") {
		t.Errorf("enabled record missing:\n%s", out)
	}
}

func TestRenderStartsWithOriginAndTTL(t *testing.T) {
	out := zones.Render(testZone(), nil)
	if !strings.HasPrefix(out, "$ORIGIN e412.in.\n") {
		t.Errorf("want $ORIGIN first, got:\n%s", out)
	}
	if !strings.Contains(out, "$TTL ") {
		t.Errorf("want a $TTL directive, got:\n%s", out)
	}
}

// renderedOwners parses out and returns, per A record address, the owner name
// the file itself resolves it to — under the $ORIGIN the file carries, which
// is the whole of what the two tests below are about. Comparing owner names
// after the parser has applied the origin is what makes the assertion
// discriminating: the wrong spelling still parses, it just names a different
// host.
func renderedOwners(t *testing.T, out string) map[string]string {
	t.Helper()
	got := map[string]string{}
	zp := dns.NewZoneParser(strings.NewReader(out), "", "")
	for rr, ok := zp.Next(); ok; rr, ok = zp.Next() {
		if a, isA := rr.(*dns.A); isA {
			got[a.A.String()] = rr.Header().Name
		}
	}
	if err := zp.Err(); err != nil {
		t.Fatalf("rendered file does not parse: %v\n---\n%s", err, out)
	}
	return got
}

// A stub stores an out-of-zone nameserver's addresses under the nameserver's
// own name (stub.go, nsRecords), because RelRecordName has no apex to strip
// off "ns.example.net" in zone e412.in and leaves it alone. Every other
// zone_records.name in the server is apex-relative, so writing that one
// through Render unchanged puts it under $ORIGIN and the exported file claims
// an address for ns.example.net.e412.in. — a name nobody asked about, while
// the nameserver the delegation actually names has no address at all.
//
// The in-zone nameserver in the same fixture is the other half of the rule:
// its name IS relative ("ns1" for ns1.e412.in), and it has to keep rendering
// relative. A fix that qualified every name would export ns1. at the root.
func TestRenderWritesAStubsOutOfZoneGlueUnderItsOwnName(t *testing.T) {
	z := testZone()
	z.Type = "stub"
	recs := []store.ZoneRecord{
		{Name: "@", Type: "NS", TTL: 3600, RData: "ns.example.net.", Enabled: true},
		{Name: "@", Type: "NS", TTL: 3600, RData: "ns1.e412.in.", Enabled: true},
		{Name: "ns.example.net", Type: "A", TTL: 3600, RData: "10.9.0.7", Enabled: true},
		{Name: "ns1", Type: "A", TTL: 3600, RData: "10.9.0.1", Enabled: true},
		// Deliberately hostile, and synthetic: a record stored under the
		// *full* spelling of an in-zone nameserver. A stub cannot produce it
		// (nsRecords strips the apex off an in-zone target, and the record
		// API refuses to write into a stub at all), so it is here to keep the
		// rule standing on its own two conditions — named by the apex NS set
		// AND outside the apex — rather than on RelRecordName's stripping
		// two functions away. This name is relative like any other: it means
		// ns1.e412.in.e412.in.
		{Name: "ns1.e412.in", Type: "A", TTL: 3600, RData: "10.9.0.2", Enabled: true},
	}

	owners := renderedOwners(t, zones.Render(z, recs))
	if got := owners["10.9.0.7"]; got != "ns.example.net." {
		t.Errorf("out-of-zone glue is owned by %q, want ns.example.net. — the exported file addresses a name the delegation does not name", got)
	}
	if got := owners["10.9.0.1"]; got != "ns1.e412.in." {
		t.Errorf("in-zone glue is owned by %q, want ns1.e412.in.", got)
	}
	if got := owners["10.9.0.2"]; got != "ns1.e412.in.e412.in." {
		t.Errorf("owner = %q, want ns1.e412.in.e412.in. — a name the apex NS set spells in full is still relative unless it is outside the apex", got)
	}
}

// The same stored name on any other zone type is a *relative* name and must
// keep rendering as one. A record written through POST /zones/{id}/records
// named "ns.example.net" in zone e412.in is served at ns.example.net.e412.in.
// (RecordFQDN), so exporting it absolute would change where it points.
// Render is shared by every zone type; only a stub stores an absolute name in
// that column.
func TestRenderKeepsARelativeNameRelativeOnAPrimary(t *testing.T) {
	z := testZone()
	recs := []store.ZoneRecord{
		{Name: "@", Type: "NS", TTL: 3600, RData: "ns.example.net.", Enabled: true},
		{Name: "ns.example.net", Type: "A", TTL: 3600, RData: "10.9.0.7", Enabled: true},
	}

	owners := renderedOwners(t, zones.Render(z, recs))
	if got := owners["10.9.0.7"]; got != "ns.example.net.e412.in." {
		t.Errorf("owner = %q, want ns.example.net.e412.in. — a primary's stored name is relative and is served under the apex", got)
	}
}
