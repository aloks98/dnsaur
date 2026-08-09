package zones_test

import (
	"net/netip"
	"testing"

	"github.com/aloks98/dnsaur/internal/zones"
)

func TestReverseName(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		// RFC 1035 §3.5: the four octets, reversed.
		{"192.168.150.10", "10.150.168.192.in-addr.arpa"},
		{"10.0.0.1", "1.0.0.10.in-addr.arpa"},
		// RFC 3596 §2.5: 32 nibbles, reversed, dot-separated — every nibble
		// present, including the zeros an address literal elides.
		{"::1", "1.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.ip6.arpa"},
		{"fd00::28", "8.2.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.d.f.ip6.arpa"},
	} {
		addr := netip.MustParseAddr(tc.in)
		if got := zones.ReverseName(addr); got != tc.want {
			t.Errorf("ReverseName(%s) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// An IPv4-mapped IPv6 address must reverse as IPv4 — otherwise the same host
// lands in two different zones depending on how its address was parsed.
func TestReverseNameUnmapsV4(t *testing.T) {
	addr := netip.MustParseAddr("::ffff:192.168.150.10")
	if got := zones.ReverseName(addr); got != "10.150.168.192.in-addr.arpa" {
		t.Errorf("ReverseName(v4-mapped) = %q, want the in-addr.arpa form", got)
	}
}

func TestReverseZoneForPicksDeepest(t *testing.T) {
	have := []string{"168.192.in-addr.arpa", "150.168.192.in-addr.arpa", "e412.in"}
	zone, rel, ok := zones.ReverseZoneFor(netip.MustParseAddr("192.168.150.10"), have)
	if !ok || zone != "150.168.192.in-addr.arpa" || rel != "10" {
		t.Fatalf("got (%q, %q, %v); want the /24 zone and rel \"10\"", zone, rel, ok)
	}
}

func TestReverseZoneForFallsBackToShallower(t *testing.T) {
	have := []string{"168.192.in-addr.arpa"}
	zone, rel, ok := zones.ReverseZoneFor(netip.MustParseAddr("192.168.150.10"), have)
	if !ok || zone != "168.192.in-addr.arpa" || rel != "10.150" {
		t.Fatalf("got (%q, %q, %v); want the /16 zone and rel \"10.150\"", zone, rel, ok)
	}
}

// No reverse zone means no PTR. Auto-PTR must never invent a zone, so this
// returning ok=false is what stops it.
func TestReverseZoneForNoMatch(t *testing.T) {
	if _, _, ok := zones.ReverseZoneFor(netip.MustParseAddr("8.8.8.8"), []string{"e412.in"}); ok {
		t.Error("ReverseZoneFor matched a zone that does not cover the address")
	}
}

// "0.168.192.in-addr.arpa" is a character-suffix of
// "10.150.168.192.in-addr.arpa" — it's the trailing 22 characters, with the
// "0" borrowed from "150" — but it is not a label-aligned suffix: the label
// there is "150", not "0". A strings.HasSuffix-based matcher would select
// this decoy; nothing else in this zoneNames list can match at all, so if
// the label walk in ReverseZoneFor is ever swapped back to a raw string
// suffix check, this test must start failing.
func TestReverseZoneForRejectsCharacterSuffixDecoy(t *testing.T) {
	have := []string{"0.168.192.in-addr.arpa"}
	if zone, rel, ok := zones.ReverseZoneFor(netip.MustParseAddr("192.168.150.10"), have); ok {
		t.Errorf("ReverseZoneFor matched a character-suffix decoy: zone=%q rel=%q", zone, rel)
	}
}

// A reverse zone can be as narrow as a single host: when the zone name
// equals the address's full reverse name, the record relative to it is the
// apex, "@" — the same convention RelName uses everywhere else in this
// package, not "".
func TestReverseZoneForApexMatch(t *testing.T) {
	full := "10.150.168.192.in-addr.arpa"
	zone, rel, ok := zones.ReverseZoneFor(netip.MustParseAddr("192.168.150.10"), []string{full})
	if !ok || zone != full || rel != "@" {
		t.Fatalf("got (%q, %q, %v); want (%q, \"@\", true)", zone, rel, ok, full)
	}
}
