package zones

import (
	"errors"
	"fmt"
	"net"
	"strconv"
)

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
