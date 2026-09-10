package zones

import (
	"fmt"
	"net/netip"
	"strings"

	"github.com/miekg/dns"
)

// The `allow_transfer` format: who may pull a zone off this server.
//
// A comma-separated list, whitespace tolerated, where each entry is one of:
//
//	10.0.0.0/24        a prefix
//	192.168.1.5        a single address (/32, or /128 for v6)
//	key:secondary-ns2  a request signed under that TSIG key
//
// Empty means deny every transfer, which is what a zone is created with.
// Entries are OR'd: any one match allows it.
//
// Unlike primaries.go, parsing is pure — no context, no resolver, no
// hostnames. An ACL is matched against a socket address on every request, so
// a hostname here would put a DNS lookup inside the gate of the server that
// answers DNS. A peer whose address moves is named by a key instead, which is
// the stronger check anyway.
//
// There are no negation entries. A default-deny list has nothing to subtract
// from, and `!` syntax would introduce an ordering question the OR does not
// have.

const aclKeyPrefix = "key:"

// ACLEntry is one parsed entry: an address prefix or a TSIG key name, never
// both. A key entry's Prefix is the zero Prefix; a prefix entry's Key is "".
type ACLEntry struct {
	Prefix netip.Prefix
	Key    string
}

// ValidateACL reports whether s is a well-formed allow_transfer list. Use it
// at write time. An empty list is valid and means deny.
func ValidateACL(s string) error {
	_, err := ParseACL(s)
	return err
}

// ParseACL parses s into the entries a request is matched against.
func ParseACL(s string) ([]ACLEntry, error) {
	var out []ACLEntry
	err := splitList(s, func(field string) error {
		e, err := parseACLEntry(field)
		if err != nil {
			return err
		}
		out = append(out, e)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func parseACLEntry(field string) (ACLEntry, error) {
	if len(field) >= len(aclKeyPrefix) && strings.EqualFold(field[:len(aclKeyPrefix)], aclKeyPrefix) {
		name := strings.TrimSpace(field[len(aclKeyPrefix):])
		if !validDNSName(name, keyNameForbidden) {
			return ACLEntry{}, fmt.Errorf("allow_transfer %q: key name must be a domain name", field)
		}
		// Canonical (lowercase, trailing dot) because that is how tsig_keys
		// stores names (normalizeTSIGName, internal/api) and how a verified
		// key name arrives from dnssrv.RequireTSIG. Three spellings of one
		// name is three chances for a match to silently fail.
		return ACLEntry{Key: dns.CanonicalName(name)}, nil
	}
	if p, err := netip.ParsePrefix(field); err == nil {
		// A bare v4-mapped address is unambiguous — ::ffff:10.0.0.5 names one
		// host, and unmapping it (below) is that host's canonical spelling. A
		// *prefix* is not: ::ffff:10.0.0.0/120 and 10.0.0.0/24 are the same
		// range written two ways, and silently rewriting one into the other
		// would mean the value read back is not the value written, while
		// accepting both spellings would give one range two stored forms. So
		// this is refused rather than unmapped: an operator who typed a
		// mapped prefix meant the IPv4 prefix, and is told to write it that
		// way instead of having it rewritten with 96-bit arithmetic.
		if p.Addr().Is4In6() {
			return ACLEntry{}, fmt.Errorf("allow_transfer %q: write this as an IPv4 prefix (e.g. 10.0.0.0/24), not an IPv4-mapped IPv6 one", field)
		}
		// Masked so 10.0.0.5/24 is stored as the range it actually matches,
		// rather than keeping host bits that Prefix.Contains ignores and a
		// reader does not.
		return ACLEntry{Prefix: p.Masked()}, nil
	}
	if addr, err := netip.ParseAddr(field); err == nil {
		addr = addr.Unmap()
		return ACLEntry{Prefix: netip.PrefixFrom(addr, addr.BitLen())}, nil
	}
	return ACLEntry{}, fmt.Errorf("allow_transfer %q: expected an IP address, a CIDR prefix, or key:<name>", field)
}

// FormatACL writes entries back in the spelling ParseACL reads. The API
// stores this canonical form rather than what was typed, which is what lets
// the tsig_keys delete guard match a key name in SQL exactly (see
// tsigKeyStore.Delete) instead of pattern-matching around whitespace.
func FormatACL(es []ACLEntry) string {
	parts := make([]string, 0, len(es))
	for _, e := range es {
		switch {
		case e.Key != "":
			parts = append(parts, aclKeyPrefix+e.Key)
		case e.Prefix.Bits() == e.Prefix.Addr().BitLen():
			// A single address prints without its all-ones mask: 192.168.1.5,
			// not 192.168.1.5/32. It parses back to the same entry, and it is
			// what the operator typed.
			parts = append(parts, e.Prefix.Addr().String())
		default:
			parts = append(parts, e.Prefix.String())
		}
	}
	return strings.Join(parts, ", ")
}

// ACLKeys returns the canonical TSIG key names s names, in order. An
// unparseable value names none: callers use this to validate a write and to
// count a key's usage, and neither has anything to do about an error here
// that its own caller is not already doing.
func ACLKeys(s string) []string {
	es, err := ParseACL(s)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range es {
		if e.Key != "" {
			out = append(out, e.Key)
		}
	}
	return out
}

// ACLAllows reports whether a peer at addr, having verified under tsigKey
// ("" when the request was unsigned or did not verify), matches any entry.
//
// addr is unmapped here rather than at the call site. A dual-stack listener
// reports a v4 peer as ::ffff:10.0.0.5, which does not match 10.0.0.0/24, and
// the failure mode is an ACL that looks correct and denies everything.
func ACLAllows(es []ACLEntry, addr netip.Addr, tsigKey string) bool {
	addr = addr.Unmap()
	key := dns.CanonicalName(tsigKey)
	for _, e := range es {
		if e.Key != "" {
			if tsigKey != "" && e.Key == key {
				return true
			}
			continue
		}
		if e.Prefix.Contains(addr) {
			return true
		}
	}
	return false
}
