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
