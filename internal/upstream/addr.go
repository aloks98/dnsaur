package upstream

import (
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
)

// Scheme is the transport an upstream is reached over.
type Scheme string

const (
	SchemePlain Scheme = "udp"
	SchemeDoT   Scheme = "tls"
	SchemeDoH   Scheme = "https"
)

// Default ports and path, applied when the entry omits them.
const (
	defaultPlainPort = "53"
	defaultDoTPort   = "853"
	defaultDoHPort   = "443"
	defaultDoHPath   = "/dns-query"
)

// Upstream is one parsed entry of the comma-separated `upstreams` setting.
//
// Addr is always a literal host:port. For SchemeDoT and SchemeDoH the host
// is always an IP, so reaching the upstream never requires a DNS lookup —
// that is what makes a DNS server able to use another DNS server over TLS
// without needing DNS first (spec §3). For SchemePlain the host may be a
// name, exactly as it could before this milestone.
type Upstream struct {
	Scheme     Scheme
	Addr       string
	VerifyName string
	Path       string
	Canonical  string
}

// ParseError is a rejected entry, carrying a stable Code beside the human
// message. web/src/lib/upstreams.ts mirrors these codes and renders its own
// copy, so the two parsers can be held to the same fixture without either
// one's wording becoming an interface.
type ParseError struct {
	Entry string
	Code  string
	Msg   string
}

func (e *ParseError) Error() string {
	if e.Entry == "" {
		return e.Msg
	}
	return fmt.Sprintf("upstream %q: %s", e.Entry, e.Msg)
}

func failf(entry, code, format string, args ...any) *ParseError {
	return &ParseError{Entry: entry, Code: code, Msg: fmt.Sprintf(format, args...)}
}

// ParseUpstreams parses the comma-separated `upstreams` setting value.
func ParseUpstreams(s string) ([]Upstream, error) {
	return parseEntries(strings.Split(s, ","))
}

// parseEntries parses already-split entries. upstream.New calls this with
// Config.Upstreams, so a Forwarder built directly in a test takes the same
// grammar as one built from the stored setting.
//
// Empty entries are dropped rather than rejected — that is what the old
// app.parseUpstreams did, and a trailing comma is a typo that costs nothing
// to forgive. A list that is *entirely* empty is a different thing: it
// leaves the server with nowhere to forward, so it is an error.
func parseEntries(entries []string) ([]Upstream, error) {
	out := make([]Upstream, 0, len(entries))
	for _, raw := range entries {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		u, err := parseEntry(raw)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	if len(out) == 0 {
		return nil, &ParseError{Code: "empty", Msg: "no upstreams configured"}
	}
	// Mixing transports would mean some queries are encrypted and some are
	// not, with nothing on screen or in the log saying which — a privacy
	// property that is unpredictable rather than partial. Spec §4 rule 4.
	for _, u := range out[1:] {
		if u.Scheme != out[0].Scheme {
			return nil, failf("", "mixed_schemes",
				"every upstream must use the same transport, but the list mixes %s and %s", out[0].Scheme, u.Scheme)
		}
	}
	return out, nil
}

func parseEntry(raw string) (Upstream, error) {
	if !strings.Contains(raw, "://") {
		return parsePlainEntry(raw, raw)
	}
	// The port has to be validated before url.Parse ever sees the entry: a
	// non-numeric port such as ":domain" makes url.Parse fail outright
	// (Go's own port-syntax check runs during parsing, before a Host/Port is
	// ever available to inspect), and Go's net/url is not the last word on
	// which inputs are malformed here -- web/src/lib/upstreams.ts holds this
	// same fixture to a WHATWG URL parser, which has a different, more
	// permissive error surface for non-special schemes like tls: and
	// https:. If the port decision depended on whichever inputs Go happens
	// to reject, the TypeScript mirror could not reproduce it by rule; it
	// would have to reproduce Go's parser instead. So the port is checked by
	// hand, by both languages, before either one's URL parser gets a vote.
	// Only tls:// and https:// get this check -- see parsePlainEntry.
	if scheme := strings.ToLower(raw[:strings.Index(raw, "://")]); scheme == string(SchemeDoT) || scheme == string(SchemeDoH) {
		if perr := checkEncryptedAuthority(raw); perr != nil {
			return Upstream{}, perr
		}
	}
	u, err := url.Parse(raw)
	if err != nil {
		// Deliberately uncovered by any fixture case. A malformed port is
		// already ruled out above, so reaching here means url.Parse refused
		// the entry for some other syntactic reason (bad percent-encoding, a
		// stray control character, ...). Go's net/url and a WHATWG URL
		// parser do not agree on which such inputs are errors at all, so a
		// fixture case pinned to this branch could not be satisfied by one
		// shared rule in both languages -- unlike every other code in this
		// grammar, which is decided by a rule this package owns.
		return Upstream{}, failf(raw, "bad_url", "not a valid URL: %v", err)
	}
	if u.User != nil {
		return Upstream{}, failf(raw, "bad_url", "a username or password has no meaning on a DNS upstream")
	}
	if u.RawQuery != "" {
		return Upstream{}, failf(raw, "bad_url", "a query string has no meaning on a DNS upstream")
	}
	switch Scheme(u.Scheme) {
	case SchemePlain:
		if u.Fragment != "" {
			return Upstream{}, failf(raw, "name_on_plain",
				`"#%s" only applies to tls:// and https:// upstreams, where it names the certificate to check`, u.Fragment)
		}
		if p := strings.Trim(u.Path, "/"); p != "" {
			return Upstream{}, failf(raw, "bad_url", "a path has no meaning on a plain DNS upstream")
		}
		return parsePlainEntry(raw, u.Host)
	case SchemeDoT:
		return parseEncryptedEntry(raw, u, SchemeDoT, defaultDoTPort, "")
	case SchemeDoH:
		return parseEncryptedEntry(raw, u, SchemeDoH, defaultDoHPort, defaultDoHPath)
	default:
		return Upstream{}, failf(raw, "bad_scheme",
			"unknown transport %q: use udp://, tls:// or https:// (or no scheme for plain DNS)", u.Scheme)
	}
}

// parsePlainEntry applies exactly the treatment app.parseUpstreams gave
// every entry before this milestone: split, default the port to 53, bracket
// a bare IPv6 literal so the appended port parses. It validates nothing
// else, on purpose — a plain upstream has always been allowed to be a
// hostname, and tightening that here would reject working configurations.
func parsePlainEntry(raw, hostPort string) (Upstream, error) {
	if strings.Contains(hostPort, "#") {
		name := hostPort[strings.Index(hostPort, "#")+1:]
		return Upstream{}, failf(raw, "name_on_plain",
			`"#%s" only applies to tls:// and https:// upstreams, where it names the certificate to check`, name)
	}
	addr := hostPort
	if _, _, err := net.SplitHostPort(addr); err != nil {
		if strings.Contains(addr, ":") && !strings.HasPrefix(addr, "[") {
			addr = "[" + addr + "]:" + defaultPlainPort
		} else {
			addr += ":" + defaultPlainPort
		}
	}
	return Upstream{Scheme: SchemePlain, Addr: addr, Canonical: addr}, nil
}

// checkEncryptedAuthority validates the authority of a tls:// or https://
// entry by hand, independent of url.Parse: an unbracketed IPv6 literal
// first, then the port -- literal digits, in range 1-65535. It runs before
// url.Parse is even called, so a malformed port such as ":domain" is
// rejected as bad_addr by a rule this package owns, rather than by whichever
// inputs Go's net/url happens to error on -- see the comment in parseEntry.
//
// The IPv6 case has to be caught here for a second reason: nothing after
// url.Parse can still tell it apart. Everything from the last colon on is
// read as the port, so tls://2606:4700:4700::1111 arrives with the host
// "2606:4700:4700:" and the operator is told that their address is a name.
func checkEncryptedAuthority(raw string) *ParseError {
	authority := raw[strings.Index(raw, "://")+3:]
	if i := strings.IndexAny(authority, "/?#"); i >= 0 {
		authority = authority[:i]
	}
	if i := strings.LastIndexByte(authority, '@'); i >= 0 {
		authority = authority[i+1:] // drop userinfo; bad_url handles it later
	}
	// Two or more colons with no bracket in front of them: an IPv6 literal
	// written bare. One colon is host:port, which is the ordinary case.
	if strings.Count(authority, ":") >= 2 && !strings.HasPrefix(authority, "[") {
		return failf(raw, "bad_addr",
			"%q is an IPv6 address and needs brackets here: write [%s]", authority, authority)
	}
	_, port := splitHostPort(authority)
	if port == "" {
		return nil // no port stated: the scheme's default applies
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return failf(raw, "bad_addr", "%q is not a port number", port)
	}
	return nil
}

// splitHostPort splits an authority (no userinfo) into host and port,
// understanding the bracketed [ipv6]:port form the way net.SplitHostPort
// does, but -- unlike net.SplitHostPort -- tolerating a missing port, since
// a tls:// or https:// entry may omit one and take the scheme's default.
func splitHostPort(hostport string) (host, port string) {
	if strings.HasPrefix(hostport, "[") {
		end := strings.IndexByte(hostport, ']')
		if end < 0 {
			return hostport, "" // malformed; the caller's host/IP check rejects it
		}
		host = hostport[1:end]
		if rest := hostport[end+1:]; strings.HasPrefix(rest, ":") {
			port = rest[1:]
		}
		return host, port
	}
	if i := strings.LastIndexByte(hostport, ':'); i >= 0 {
		return hostport[:i], hostport[i+1:]
	}
	return hostport, ""
}

// parseEncryptedEntry handles tls:// and https://, which share every rule
// that makes an encrypted upstream different: the host must be an address,
// and the certificate name must be stated.
func parseEncryptedEntry(raw string, u *url.URL, scheme Scheme, defaultPort, defaultPath string) (Upstream, error) {
	host, port := u.Hostname(), u.Port()
	if host == "" {
		return Upstream{}, failf(raw, "bad_addr", "no address")
	}
	// Camp 1 (spec §3): the operator supplies the address. Resolving a name
	// here would need DNS to configure DNS, and every mainstream resolver
	// that takes this seriously — Unbound, systemd-resolved, Stubby, Knot —
	// asks for the address instead.
	if _, err := netip.ParseAddr(host); err != nil {
		return Upstream{}, failf(raw, "host_not_ip",
			"%q is a name, and an encrypted upstream needs an address here: write %s://<address>#%s", host, scheme, host)
	}
	if port == "" {
		port = defaultPort
	} else {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return Upstream{}, failf(raw, "bad_addr", "%q is not a port number", port)
		}
	}
	if u.Fragment == "" {
		return Upstream{}, failf(raw, "missing_name",
			`missing "#name": an encrypted upstream needs the name its certificate must present, e.g. %s#dns.example.net`, raw)
	}
	up := Upstream{
		Scheme:     scheme,
		Addr:       net.JoinHostPort(host, port),
		VerifyName: u.Fragment,
	}
	if defaultPath != "" {
		// EscapedPath, not Path: u.Path is decoded, so a configured "%2F"
		// would arrive here as a real path separator and doh.go would ask
		// the server for a different resource than the operator typed.
		// EscapedPath keeps the operator's spelling, which is also what
		// WHATWG's URL.pathname gives the TypeScript mirror -- so the two
		// agree for free. Everything downstream (Canonical, doh.go's request
		// URL) therefore holds an already-escaped path; see newDoHExchanger.
		up.Path = u.EscapedPath()
		if up.Path == "" || up.Path == "/" {
			up.Path = defaultPath
		}
	}
	up.Canonical = string(scheme) + "://" + up.Addr + up.Path + "#" + up.VerifyName
	return up, nil
}
