package zones

import (
	"net"
	"strconv"
	"strings"
)

// The `forward_to` format: where a forwarder zone sends its queries.
//
// A comma-separated list, whitespace tolerated, each entry a host with an
// optional port:
//
//	10.0.0.1
//	10.0.0.2:5353
//	ns.corp.example
//	[fd00::2]:5353
//
// Empty means the zone names no upstreams, which §9.11.5 makes a SERVFAIL
// rather than a fall-through: the zone still claims the suffix.
//
// Simpler than its two neighbours on purpose. There is no `key:` because a
// forwarder signs nothing — it sends ordinary queries, not transfers — and
// the parse is pure because it never resolves anything at all.
//
// **That is a real difference from `primaries`, not a deferral of the same
// work.** ParsePrimaries resolves to []netip.AddrPort at transfer time. A
// forward target is never resolved by this package or by internal/app:
// conditionalRoutes appends ForwardTarget.Addr() verbatim, SetConditional
// hands that string to newUp, and the Go dialer resolves it on every
// exchange. So a hostname forward target follows DNS per query — it cannot
// go stale between zone reloads, because no reload ever pinned it to an
// address.

// ForwardTarget is one parsed entry.
type ForwardTarget struct {
	// Host is as written and may be a hostname. Nothing here or in
	// internal/app resolves it: it reaches the forwarder as a dial string
	// and the Go dialer resolves it per exchange. See the file comment.
	Host string
	Port uint16
}

// Addr is the target in dial form, which is what reaches the forwarder's
// conditional routing table.
func (t ForwardTarget) Addr() string {
	return net.JoinHostPort(t.Host, strconv.Itoa(int(t.Port)))
}

// ValidateForwardTo reports whether s is a well-formed forward_to list. Use it
// at write time. An empty list is valid.
func ValidateForwardTo(s string) error {
	_, err := ParseForwardTo(s)
	return err
}

// ParseForwardTo parses s into the targets a forwarder zone's queries go to.
func ParseForwardTo(s string) ([]ForwardTarget, error) {
	var out []ForwardTarget
	for _, field := range strings.Split(s, ",") {
		// Skipped rather than rejected, as splitPrimaries and ParseNotifyTo
		// both do: a trailing comma names no target.
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		host, port, err := parseHostPort("forward target", field, field)
		if err != nil {
			return nil, err
		}
		out = append(out, ForwardTarget{Host: host, Port: port})
	}
	return out, nil
}

// FormatForwardTo writes targets back in the spelling ParseForwardTo reads.
// The port is always written, as FormatNotifyTo does and for the same reason:
// the value is a dial target and the port is part of it.
func FormatForwardTo(ts []ForwardTarget) string {
	parts := make([]string, 0, len(ts))
	for _, t := range ts {
		parts = append(parts, t.Addr())
	}
	return strings.Join(parts, ", ")
}
