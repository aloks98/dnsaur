package zones

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"

	"github.com/miekg/dns"
)

// The `primaries` wire format, which shipped with none.
//
// zones.primaries has been a TEXT column with no defined semantics since
// Milestone A. It is a comma-separated list of host[:port], port defaulting
// to 53, where host is an IP literal or a domain name:
//
//	192.168.150.5
//	192.168.150.5:5353, ns1.upstream.example
//	2001:db8::1, [2001:db8::1]:5353
//
// **It is stored exactly as written and resolved at transfer time, not at
// write time.** That is the whole reason a hostname is allowed at all: a
// primary named by hostname has to survive its address changing, which it
// cannot do if the address is baked into the column the day the zone is
// created. It also means a primary that is unreachable — or unresolvable —
// right now does not block the configuration from being saved; it makes the
// next transfer fail, which is where that failure belongs and where it can
// be reported against the attempt that suffered it.
//
// So the two directions have two entry points, and which one to call is not
// a matter of taste:
//
//   - ValidatePrimaries is the write-time check. Syntax only, no network.
//   - ParsePrimaries is the transfer-time one. It resolves, so every call
//     picks up whatever the hostname points at now.

// DefaultPrimaryPort is the port assumed for an entry that names none —
// 53, the port any other DNS server is reached on.
const DefaultPrimaryPort = 53

// primary is one entry of the list in the form it was written: a host that
// may still be a name, and a port that has already had the default applied.
type primary struct {
	host string
	port uint16
}

// ValidatePrimaries reports whether s is a well-formed primaries list, doing
// no name resolution: a hostname is accepted on its spelling alone. Use this
// at write time — see the package-level comment above for why resolving here
// would be wrong.
//
// An empty list is an error rather than a zone with no primaries: a
// secondary that names nowhere to pull from can never transfer, so it would
// be a zone that quietly never updates instead of a configuration mistake
// anyone is told about.
func ValidatePrimaries(s string) error {
	_, err := splitPrimaries(s)
	return err
}

// ParsePrimaries parses s and resolves each entry to the addresses to try,
// in the order they were written. Call it at transfer time: the resolution
// happens on every call, which is what makes a hostname primary follow its
// address rather than the one it had when the zone was created.
//
// A hostname with several addresses contributes all of them — a list of
// primaries exists so that one being unreachable is survivable, and that
// argument does not stop applying at the boundary between two names.
//
// res is the resolver to look names up through; nil means
// net.DefaultResolver, which is what a caller with no reason to care should
// pass. It is a parameter rather than a hard-wired default because a test
// that resolves through the machine's own nameserver is not a test — it is
// a dependency on whatever that machine happens to be answering today.
func ParsePrimaries(ctx context.Context, res *net.Resolver, s string) ([]netip.AddrPort, error) {
	ps, err := splitPrimaries(s)
	if err != nil {
		return nil, err
	}
	if res == nil {
		res = net.DefaultResolver
	}
	var out []netip.AddrPort
	for _, p := range ps {
		aps, err := p.resolve(ctx, res)
		if err != nil {
			return nil, err
		}
		out = append(out, aps...)
	}
	return out, nil
}

// FormatPrimaries writes addresses back in the format ParsePrimaries reads,
// so a resolved list can be recorded (which primary answered, say) in the
// same spelling the column uses.
func FormatPrimaries(aps []netip.AddrPort) string {
	parts := make([]string, 0, len(aps))
	for _, ap := range aps {
		parts = append(parts, ap.String())
	}
	return strings.Join(parts, ",")
}

func (p primary) resolve(ctx context.Context, res *net.Resolver) ([]netip.AddrPort, error) {
	// An IP literal is already the answer; going to the resolver for it
	// would make a transfer to a literal address depend on DNS working.
	if addr, err := netip.ParseAddr(p.host); err == nil {
		return []netip.AddrPort{netip.AddrPortFrom(addr.Unmap(), p.port)}, nil
	}
	addrs, err := res.LookupNetIP(ctx, "ip", p.host)
	if err != nil {
		return nil, fmt.Errorf("resolving primary %q: %w", p.host, err)
	}
	if len(addrs) == 0 {
		// Defensive, and knowingly untested: the standard resolver reports
		// "no addresses" as an error rather than an empty slice, so nothing
		// reachable through net.Resolver produces this. It is here so that
		// ParsePrimaries can never hand back an empty list with a nil error
		// — a caller that then reports "every primary failed" would be
		// describing something that never happened.
		return nil, fmt.Errorf("primary %q resolved to no addresses", p.host)
	}
	out := make([]netip.AddrPort, 0, len(addrs))
	for _, a := range addrs {
		// Unmap so a v4 address prints and dials as 10.0.0.1:53 rather than
		// [::ffff:10.0.0.1]:53. Not every answer needs it — an A comes back
		// as a 4-byte address already — but an AAAA carrying ::ffff:10.0.0.1
		// does not, and LookupNetIP preserves that form rather than
		// normalising it.
		out = append(out, netip.AddrPortFrom(a.Unmap(), p.port))
	}
	return out, nil
}

func splitPrimaries(s string) ([]primary, error) {
	var out []primary
	for _, field := range strings.Split(s, ",") {
		// Skipped rather than rejected: a trailing comma or a doubled
		// separator names no primary, so there is nothing to be wrong about.
		// A list that is *only* separators still fails, on the emptiness
		// check below.
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		p, err := parsePrimary(field)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	if len(out) == 0 {
		return nil, errors.New("at least one primary is required")
	}
	return out, nil
}

func parsePrimary(field string) (primary, error) {
	host, port, err := parseHostPort("primary", field, field)
	if err != nil {
		return primary{}, err
	}
	return primary{host: host, port: port}, nil
}

// validPrimaryHost accepts an IP literal or a domain name. The domain-name
// half needs the same guards normalizeZoneName (internal/api) applies for
// the same reason: dns.IsDomainName documents itself as "extremely liberal
// — almost any string is a valid domain name", so on its own it would
// accept "not a host".
func validPrimaryHost(host string) bool {
	if _, err := netip.ParseAddr(host); err == nil {
		return true
	}
	if host == "" || strings.ContainsAny(host, " \t\r\n/\\:") {
		return false
	}
	for _, label := range strings.Split(strings.TrimSuffix(host, "."), ".") {
		if label == "" {
			return false
		}
	}
	_, ok := dns.IsDomainName(host)
	return ok
}
