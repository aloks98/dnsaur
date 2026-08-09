package zones

import (
	"net/netip"
	"strings"

	"github.com/miekg/dns"
)

// ReverseName returns addr's reverse-DNS name (RFC 1035 §3.5 for IPv4,
// RFC 3596 §2.5 for IPv6) without a trailing dot, matching how zone and
// record names are stored everywhere else in this codebase.
//
// addr is unmapped first: netip.Addr can hold an IPv4 address in its
// IPv4-mapped IPv6 form (::ffff:a.b.c.d), and without unmapping, the same
// host would reverse into in-addr.arpa or ip6.arpa depending on how its
// address happened to be parsed — the two forms would then serve different
// PTR records for the same host.
func ReverseName(addr netip.Addr) string {
	arpa, err := dns.ReverseAddr(addr.Unmap().String())
	if err != nil {
		// addr is a valid netip.Addr, so its String() form always parses;
		// this is unreachable outside the invalid zero Addr.
		return ""
	}
	return strings.TrimSuffix(arpa, ".")
}

// ReverseZoneFor picks the deepest zone in zoneNames that covers addr and
// returns addr's record name relative to that zone. It mirrors
// (*Index).Find: walk addr's reverse name label-by-label, right to left,
// so a nested zone (150.168.192.in-addr.arpa) wins over its parent
// (168.192.in-addr.arpa) when both exist.
//
// The comparison is on whole labels, not raw string suffixes — matching
// zoneNames by strings.HasSuffix would let "0.168.192.in-addr.arpa" match
// as a suffix of "10.150.168.192.in-addr.arpa" (the "0" borrowed from
// "150" lines up with a label boundary that isn't really there). Splitting
// into labels first rules that out.
//
// ok is false when no entry in zoneNames covers addr. Auto-PTR relies on
// that to know when to do nothing rather than invent a zone.
func ReverseZoneFor(addr netip.Addr, zoneNames []string) (zone string, rel string, ok bool) {
	labels := dns.SplitDomainName(ReverseName(addr))

	have := make(map[string]bool, len(zoneNames))
	for _, zn := range zoneNames {
		have[normalizeName(zn)] = true
	}

	for i := range labels {
		candidate := strings.Join(labels[i:], ".")
		if !have[candidate] {
			continue
		}
		if i == 0 {
			return candidate, apexName, true
		}
		return candidate, strings.Join(labels[:i], "."), true
	}
	return "", "", false
}
