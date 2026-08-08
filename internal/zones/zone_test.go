package zones_test

import (
	"testing"

	"github.com/aloks98/dnsaur/internal/store"
	"github.com/aloks98/dnsaur/internal/zones"
	"github.com/miekg/dns"
)

func TestFindDeepestZoneWins(t *testing.T) {
	idx := zones.NewIndex([]zones.Zone{
		{Zone: store.Zone{Name: "in", Enabled: true}},
		{Zone: store.Zone{Name: "e412.in", Enabled: true}},
	})
	if z := idx.Find("bifrost.e412.in"); z == nil || z.Name != "e412.in" {
		t.Fatalf("Find = %v, want e412.in — a nested zone must win over its parent", z)
	}
	if z := idx.Find("example.com"); z != nil {
		t.Errorf("Find(example.com) = %v, want nil", z)
	}
}

func TestFindSkipsDisabledZone(t *testing.T) {
	idx := zones.NewIndex([]zones.Zone{{Zone: store.Zone{Name: "e412.in", Enabled: false}}})
	// A disabled zone must not claim authority: the query has to be free to
	// go upstream, which is the whole point of the toggle.
	if z := idx.Find("bifrost.e412.in"); z != nil {
		t.Errorf("Find = %v, want nil for a disabled zone", z)
	}
}

// TestFindFallsBackToShallowerEnabledZone distinguishes "skip the disabled
// zone and keep looking" from "stop searching the moment a disabled zone
// matches." Both behaviours pass TestFindSkipsDisabledZone, since that test
// only has one zone in play. Here, e412.in is disabled but its parent in is
// enabled, so bifrost.e412.in must fall back to in rather than going
// upstream.
func TestFindFallsBackToShallowerEnabledZone(t *testing.T) {
	idx := zones.NewIndex([]zones.Zone{
		{Zone: store.Zone{Name: "in", Enabled: true}},
		{Zone: store.Zone{Name: "e412.in", Enabled: false}},
	})
	z := idx.Find("bifrost.e412.in")
	if z == nil {
		t.Fatal("Find = nil, want the shallower enabled zone \"in\" — a disabled zone must not block a parent zone from answering")
	}
	if z.Name != "in" {
		t.Errorf("Find = %v, want in", z)
	}
}

func TestRelName(t *testing.T) {
	for _, tc := range []struct{ q, apex, want string }{
		{"e412.in", "e412.in", "@"},
		{"bifrost.e412.in", "e412.in", "bifrost"},
		{"a.b.e412.in", "e412.in", "a.b"},
	} {
		if got := zones.RelName(tc.q, tc.apex); got != tc.want {
			t.Errorf("RelName(%q,%q) = %q, want %q", tc.q, tc.apex, got, tc.want)
		}
	}
}

// The presentation-format rdata decision earns its keep here: MX and CAA
// were never supported by the old per-type switch and need no code.
func TestToRRHandlesTypesTheOldSwitchDidNot(t *testing.T) {
	for _, tc := range []struct{ rtype, rdata string }{
		{"A", "192.168.150.28"},
		{"MX", "10 mail.e412.in."},
		{"SRV", "0 5 5060 sip.e412.in."},
		{"CAA", `0 issue "letsencrypt.org"`},
	} {
		rr, err := zones.ToRR("e412.in.", store.ZoneRecord{Type: tc.rtype, TTL: 3600, RData: tc.rdata})
		if err != nil {
			t.Errorf("ToRR(%s %q) error: %v", tc.rtype, tc.rdata, err)
			continue
		}
		if dns.TypeToString[rr.Header().Rrtype] != tc.rtype {
			t.Errorf("ToRR(%s) built a %s", tc.rtype, dns.TypeToString[rr.Header().Rrtype])
		}
	}
}

func TestToRRRejectsGarbage(t *testing.T) {
	if _, err := zones.ToRR("e412.in.", store.ZoneRecord{Type: "A", TTL: 300, RData: "not-an-ip"}); err == nil {
		t.Error("ToRR accepted a non-IP as an A record")
	}
}

// TestZoneSOAFieldMapping pins each SOA rdata field to the store.Zone column
// it must come from. Every numeric field below is a distinct value, so a
// transposed pair (e.g. Refresh/Retry swapped) fails loudly instead of
// compiling clean and shipping wrong.
//
// The header TTL and MINIMUM are given different values on purpose: they are
// separate columns (soa_ttl and soa_minimum) because RFC 2308 §5 makes a
// negative answer's TTL the smaller of the two, and a Zone.SOA that read
// both from one field would make that minimum degenerate.
func TestZoneSOAFieldMapping(t *testing.T) {
	z := &zones.Zone{Zone: store.Zone{
		Name:       "e412.in",
		SOANS:      "ns1.e412.in",
		SOAMbox:    "hostmaster.e412.in",
		SOASerial:  2024010101,
		SOARefresh: 7200,
		SOARetry:   3600,
		SOAExpire:  1209600,
		SOAMinimum: 300,
		SOATTL:     120,
	}}
	soa := z.SOA()

	if soa.Hdr.Ttl != 120 {
		t.Errorf("Hdr.Ttl = %d, want 120 (SOATTL, not SOAMinimum)", soa.Hdr.Ttl)
	}

	if soa.Hdr.Name != "e412.in." {
		t.Errorf("Hdr.Name = %q, want e412.in.", soa.Hdr.Name)
	}
	if soa.Hdr.Rrtype != dns.TypeSOA {
		t.Errorf("Hdr.Rrtype = %v, want SOA", soa.Hdr.Rrtype)
	}
	if soa.Hdr.Class != dns.ClassINET {
		t.Errorf("Hdr.Class = %v, want IN", soa.Hdr.Class)
	}
	if soa.Ns != "ns1.e412.in." {
		t.Errorf("Ns = %q, want ns1.e412.in.", soa.Ns)
	}
	if soa.Mbox != "hostmaster.e412.in." {
		t.Errorf("Mbox = %q, want hostmaster.e412.in.", soa.Mbox)
	}
	if soa.Serial != 2024010101 {
		t.Errorf("Serial = %d, want 2024010101", soa.Serial)
	}
	if soa.Refresh != 7200 {
		t.Errorf("Refresh = %d, want 7200", soa.Refresh)
	}
	if soa.Retry != 3600 {
		t.Errorf("Retry = %d, want 3600", soa.Retry)
	}
	if soa.Expire != 1209600 {
		t.Errorf("Expire = %d, want 1209600", soa.Expire)
	}
	if soa.Minttl != 300 {
		t.Errorf("Minttl = %d, want 300", soa.Minttl)
	}
}
