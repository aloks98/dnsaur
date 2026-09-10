package zones

import (
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/miekg/dns"
)

// The `notify_to` format: who this zone tells when it changes.
//
// A comma-separated list, whitespace tolerated, where each entry is a host
// with an optional port and an optional TSIG key:
//
//	10.0.0.2                              default port, unsigned
//	10.0.0.2:5353                         explicit port, unsigned
//	ns2.example.com key:ns2-xfer          resolved at send time, signed
//	[fd00::2]:5353 key:hetzner-xfer       an IPv6 literal takes brackets with a port
//
// Empty means notify nobody, and that is what every zone is created with.
//
// **This is deliberately neither primaries.go nor acl.go.** ParseACL is pure
// because an ACL is matched against a socket address on every request, and a
// hostname there would put a DNS lookup inside the gate of the server that
// answers DNS. ParsePrimaries resolves because a primary named by hostname
// must be followed at transfer time. This needs both properties, split: the
// parse is pure, and resolution happens at send time in notifier.go.
//
// The split is forced rather than chosen. NotifyTarget.Addr is the row
// identity in zone_notifies, so a parser that resolved would make that
// identity an address — and a hostname whose address changed would orphan its
// delivery history and start a new row every time it moved.
//
// The key is per target rather than per zone because a primary has none of
// its own: zones.tsig_key_id is refused on a primary zone (it means "the key
// a secondary signs its transfer requests with"), so without this a dnsaur
// primary could not sign a NOTIFY to a dnsaur secondary that requires one.

const notifyKeyPrefix = "key:"

// NotifyTarget is one parsed entry.
type NotifyTarget struct {
	// Host is as written and may be a hostname; it is resolved at send time.
	Host string
	Port uint16
	// Key is the canonical TSIG name (lowercase, trailing dot) this target's
	// NOTIFY is signed under, or "" to send unsigned.
	Key string
}

// Addr is the target's host and port in dial form, and is the identity
// zone_notifies keys a row on. It deliberately excludes the key, so
// re-keying a target keeps its delivery history rather than orphaning it.
func (t NotifyTarget) Addr() string {
	return net.JoinHostPort(t.Host, strconv.Itoa(int(t.Port)))
}

// ValidateNotifyTo reports whether s is a well-formed notify_to list. Use it
// at write time. An empty list is valid and means notify nobody.
func ValidateNotifyTo(s string) error {
	_, err := ParseNotifyTo(s)
	return err
}

// ParseNotifyTo parses s into the targets a NOTIFY is sent to.
func ParseNotifyTo(s string) ([]NotifyTarget, error) {
	// Unlike splitPrimaries there is no "at least one" check at the end. A
	// secondary with no primaries cannot transfer, so an empty list there is
	// a broken zone; a zone with no notify targets is the ordinary case and
	// the default.
	var out []NotifyTarget
	err := splitList(s, func(field string) error {
		t, err := parseNotifyTarget(field)
		if err != nil {
			return err
		}
		out = append(out, t)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func parseNotifyTarget(field string) (NotifyTarget, error) {
	// The key is a suffix on the entry, so it comes off before the host is
	// looked at — otherwise SplitHostPort would see the whole string.
	hostPart := field
	key := ""
	if i := strings.IndexAny(field, " \t"); i >= 0 {
		hostPart = strings.TrimSpace(field[:i])
		rest := strings.TrimSpace(field[i:])
		// Exactly one trailing token, and it must be the key. Anything else
		// is refused rather than ignored: a second token is either a typo or
		// a syntax this format does not have, and silently dropping it would
		// store a target the operator believes is signed and is not.
		if !strings.HasPrefix(strings.ToLower(rest), notifyKeyPrefix) {
			return NotifyTarget{}, fmt.Errorf("notify target %q: expected `key:<name>` after the host", field)
		}
		name := strings.TrimSpace(rest[len(notifyKeyPrefix):])
		if strings.ContainsAny(name, " \t") {
			return NotifyTarget{}, fmt.Errorf("notify target %q: only one `key:<name>` is allowed, and it must be the last token", field)
		}
		if !validDNSName(name, keyNameForbidden) {
			return NotifyTarget{}, fmt.Errorf("notify target %q: key name must be a domain name", field)
		}
		key = dns.CanonicalName(name)
	}

	// A field that is only a key names no target. It has to be caught here,
	// before SplitHostPort, because that function reads "key:ns2-xfer" as
	// host "key" with port "ns2-xfer" and returns no error at all — so
	// without this the operator's typo is reported as a bad port number,
	// which is both wrong and unactionable. (Verified against net's actual
	// behaviour, not assumed.)
	if strings.HasPrefix(strings.ToLower(hostPart), notifyKeyPrefix) {
		return NotifyTarget{}, fmt.Errorf("notify target %q: needs a host before the key", field)
	}

	host, port, err := parseHostPort("notify target", field, hostPart)
	if err != nil {
		return NotifyTarget{}, err
	}
	return NotifyTarget{Host: host, Port: port, Key: key}, nil
}

// FormatNotifyTo writes targets back in the spelling ParseNotifyTo reads. The
// API stores this canonical form rather than what was typed, which is what
// lets the tsig_keys delete guard match a key name in SQL exactly (see
// tsigKeyStore.Delete) instead of pattern-matching around whitespace.
//
// The port is always written, even when it is the default. Unlike FormatACL's
// bare-address case there is nothing to gain by hiding it — the value is a
// dial target, the port is part of it, and Addr has to produce the same
// string either way.
func FormatNotifyTo(ts []NotifyTarget) string {
	parts := make([]string, 0, len(ts))
	for _, t := range ts {
		s := t.Addr()
		if t.Key != "" {
			s += " " + notifyKeyPrefix + t.Key
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, ", ")
}

// NotifyToKeys returns the canonical TSIG key names s names, in order. An
// unparseable value names none, mirroring ACLKeys: callers use this to
// validate a write and to count a key's usage, and neither has anything to do
// about an error here that its own caller is not already doing.
func NotifyToKeys(s string) []string {
	ts, err := ParseNotifyTo(s)
	if err != nil {
		return nil
	}
	var out []string
	for _, t := range ts {
		if t.Key != "" {
			out = append(out, t.Key)
		}
	}
	return out
}
