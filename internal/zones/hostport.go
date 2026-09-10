package zones

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"

	"github.com/miekg/dns"
)

// What the four list columns — `primaries`, `notify_to`, `forward_to` and
// `allow_transfer` — genuinely share: how a list is split, what makes a name
// a name, and how one entry's host and port come apart. Each format keeps
// its own entry syntax and its own error nouns.

// splitList calls each with every entry of a comma-separated list column,
// trimmed, skipping the empty ones.
//
// Skipped rather than rejected: a trailing comma or a doubled separator
// names nothing, so there is nothing to be wrong about. What a list of
// *only* separators means is the caller's to decide — splitPrimaries makes
// no entries an error, the other three make it an empty list.
func splitList(s string, each func(field string) error) error {
	for _, field := range strings.Split(s, ",") {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		if err := each(field); err != nil {
			return err
		}
	}
	return nil
}

// keyNameForbidden are the characters a TSIG key name may not carry in
// either format that names one. It is stricter than normalizeTSIGName's
// guards (internal/api/tsigkeys_handlers.go) by ',' and ':', which are these
// formats' own delimiters — the entry separator and the `key:` prefix — so a
// name containing either would be indistinguishable from one. The ',' can
// never actually reach the check, since both parsers split on it first, and
// stays in the set so this fails closed rather than open if that splitting
// ever changes.
const keyNameForbidden = " \t\r\n/\\,:"

// validDNSName reports whether name is a domain name worth storing, refusing
// outright any character in forbidden.
//
// dns.IsDomainName documents itself as "extremely liberal — almost any
// string is a valid domain name", so on its own it accepts "ns 2" and "not a
// host"; the empty-label loop then catches "a..b" and a leading dot, which it
// also tolerates. forbidden is what each format adds on top: its own
// delimiters, plus the characters no host or key name has.
func validDNSName(name, forbidden string) bool {
	if name == "" || strings.ContainsAny(name, forbidden) {
		return false
	}
	for _, label := range strings.Split(strings.TrimSuffix(name, "."), ".") {
		if label == "" {
			return false
		}
	}
	_, ok := dns.IsDomainName(name)
	return ok
}

// parseHostPort splits one "host", "host:port" or "[v6]:port" entry into a
// validated host and a port, defaulting to DefaultPrimaryPort.
//
// It is the one implementation of a shape three stored formats share:
// `primaries`, `notify_to` and `forward_to`. It existed twice before
// `forward_to`, and a third copy would have been choosing to keep a
// duplication D4's review had already flagged.
//
// label is the noun the calling format uses in its errors ("primary",
// "notify target", "forward target"), and field is the whole original entry.
// They are separate from hostPart because notify_to strips a trailing
// ` key:name` before splitting but still quotes the entry the operator typed:
// one parameter would either split the key into the host or quote a fragment.
func parseHostPort(label, field, hostPart string) (string, uint16, error) {
	host, portStr, err := net.SplitHostPort(hostPart)
	if err != nil {
		// net.SplitHostPort reports every shape it cannot split as an
		// *net.AddrError, and two of those shapes are legal here: "missing
		// port in address" for a bare host, and "too many colons in address"
		// for a bare IPv6 literal. Both are the no-port form, so the whole
		// part is the host. Anything that is not an AddrError is a failure to
		// parse rather than a form to interpret.
		var addrErr *net.AddrError
		if !errors.As(err, &addrErr) {
			return "", 0, fmt.Errorf("%s %q: %w", label, field, err)
		}
		host, portStr = hostPart, ""
		// "[fd00::2]" is the same address as "fd00::2": the brackets are how
		// a v6 address is written when it carries a port, so an operator who
		// writes one and then drops the port arrives here. Refusing it with
		// "host must be an IP address or a domain name" sends them to inspect
		// an address that is fine. Only a bracketed *address* is unwrapped —
		// brackets around a hostname are not a form anything writes.
		if inner, ok := strings.CutPrefix(host, "["); ok {
			if inner, ok := strings.CutSuffix(inner, "]"); ok {
				if _, err := netip.ParseAddr(inner); err == nil {
					host = inner
				}
			}
		}
	}
	port := uint16(DefaultPrimaryPort)
	if portStr != "" {
		n, err := strconv.ParseUint(portStr, 10, 16)
		if err != nil || n == 0 {
			return "", 0, fmt.Errorf("%s %q: port must be between 1 and 65535", label, field)
		}
		port = uint16(n)
	}
	if !validPrimaryHost(host) {
		return "", 0, fmt.Errorf("%s %q: host must be an IP address or a domain name", label, field)
	}
	return host, port, nil
}
