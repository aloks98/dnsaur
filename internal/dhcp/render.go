package dhcp

import (
	"cmp"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"path"
	"strconv"
	"strings"

	"github.com/aloks98/dnsaur/internal/store"
)

// ErrNoLocalAddress is §5.3's dead end: the box's DNS listener is on every
// address, and none of the host's own addresses is inside the scope, so
// there is nothing sensible to hand clients as their resolver. The operator
// fixes it by typing the scope's dns_servers.
var ErrNoLocalAddress = errors.New("set dns_servers, no local address is inside")

// ErrNoPeerHost is §6's dead end, and the one a pair falls into by changing
// nothing: dns_listen defaults to ":53", so a box builds its own peer entry
// from a host that does not exist and hands the hook "http://:8000/". Kea
// refuses that config itself, but its message is about a URL.
//
// This one is an instruction. It ends up on the DHCP page and in the log,
// where the reader has the box in front of them and not this file, so
// noPeerHost adds what dns_listen currently says and, when the machine can
// be asked, a value to put there instead. Never a peer's name: the names in
// a rendered pair are instance ids, and an instance id is not something
// anyone can act on.
var ErrNoPeerHost = errors.New("HA pair needs a host in this box's dns_listen")

// controlSocketsFrom is the first Kea that spells the control socket
// "control-sockets", as a list. Older engines know only the singular key and
// reject the plural outright, so the renderer is told the version rather than
// guessing, and it renders exactly one of the two: 3.0 refuses a config
// carrying both ("duplicate control-socket entries").
var controlSocketsFrom = [3]int{2, 7, 2}

// Peer is one member of the HA pair as the hook wants it. Role is "primary"
// or "standby"; URL arrives fully formed, because the caller is the one that
// knows each box's address and the HA port.
type Peer struct{ Name, URL, Role string }

// RenderInput is everything the engine's configuration is made of: the
// synced tables, the settings that back them, and the handful of local facts
// (socket, interfaces, hook directory, engine version, own address) that
// differ between the two boxes of a pair.
type RenderInput struct {
	Scopes       []store.Scope
	Reservations []store.Reservation

	// Domain and LeaseSeconds are the dhcp.domain and dhcp.lease_seconds
	// settings: what a scope that overrides neither gets.
	Domain       string
	LeaseSeconds int

	// Interfaces is serve.dhcp_interfaces; empty binds every interface.
	Interfaces []string
	// OwnListen is this box's first dns_listen entry, verbatim. Nothing is
	// rendered from it: it is there so a refusal about the host it does not
	// name can quote what it does say.
	OwnListen string
	// Socket is the bootstrap kea_socket path: the control socket as this
	// box reaches it. EngineSocket is the same socket as the engine's own
	// configuration names it (config-get), which is what is rendered back
	// when known: the two differ across a bind mount, and Kea insists its
	// socket stays where it was started with — a config naming another path
	// is refused, and Debian's build then drops the socket altogether
	// (DHCP4_CONFIG_UNRECOVERABLE_ERROR) until the engine is restarted.
	Socket       string
	EngineSocket string
	// HookDir is the directory config-get reported the engine's hooks in,
	// and KeaVersion what version-get reported.
	HookDir    string
	KeaVersion string

	// ThisServer is this box's instance id and Peers the pair, empty for a
	// single box, which gets no HA section at all. The port each box's HA
	// listener answers on is not a field: it is already in every peer URL,
	// and the HA hook opens that listener itself.
	ThisServer string
	Peers      []Peer

	// DNSServers is the automatic domain-name-servers list in the order
	// clients are handed it — the same order on both boxes of a pair, which
	// is what DHCP-level failover needs from DNS (§5.3). An empty entry
	// stands for this box's own address and is resolved per scope against
	// LocalAddrs, the host's interface prefixes, so each box spells its own
	// entry "" and the two lists stay identical.
	DNSServers []string
	LocalAddrs []netip.Prefix
}

// Render turns in into the Dhcp4 object Kea accepts on config-set (§5.2). It
// is pure: everything it needs was read before it was called, and the result
// is built from map[string]any and []any so that marshalling it sorts the
// keys and two renders of the same input are byte-identical.
func Render(in RenderInput) (map[string]any, error) {
	interfaces := in.Interfaces
	if len(interfaces) == 0 {
		interfaces = []string{"*"}
	}
	subnets, err := renderSubnets(in)
	if err != nil {
		return nil, err
	}
	hooks, err := renderHooks(in)
	if err != nil {
		return nil, err
	}
	cfg := map[string]any{
		"interfaces-config": map[string]any{
			"interfaces":       anySlice(interfaces),
			"dhcp-socket-type": "raw",
		},
		"lease-database":  map[string]any{"type": "memfile", "persist": true},
		"valid-lifetime":  in.LeaseSeconds,
		"renew-timer":     renewTimer(in.LeaseSeconds),
		"rebind-timer":    rebindTimer(in.LeaseSeconds),
		"hooks-libraries": hooks,
		"subnet4":         subnets,
	}
	// The unix socket dnsaur itself talks over, under whichever key this
	// engine knows. Never an http entry beside it: the HA hook opens its own
	// listener on the peer URL's port on both 2.6 and 3.0, and a second one
	// on that port fails the whole config with "CmdHttpListener::run
	// failed".
	socket := map[string]any{"socket-type": "unix", "socket-name": cmp.Or(in.EngineSocket, in.Socket)}
	if atLeast(in.KeaVersion, controlSocketsFrom) {
		cfg["control-sockets"] = []any{socket}
	} else {
		cfg["control-socket"] = socket
	}
	return cfg, nil
}

// renderHooks lists lease_cmds always — dnsaur reads leases over it — and
// the HA hook only for a box that has a partner.
func renderHooks(in RenderInput) ([]any, error) {
	hooks := []any{map[string]any{"library": path.Join(in.HookDir, "libdhcp_lease_cmds.so")}}
	if len(in.Peers) == 0 {
		return hooks, nil
	}
	peers := make([]any, 0, len(in.Peers))
	for _, p := range in.Peers {
		// Both entries, not just this box's own: the pair is rendered
		// identically on both boxes, so a partner with no host is this
		// box's configuration to refuse as much as its own is.
		if u, err := url.Parse(p.URL); err != nil || u.Hostname() == "" {
			return nil, noPeerHost(in)
		}
		peers = append(peers, map[string]any{
			"name": p.Name, "url": p.URL, "role": p.Role, "auto-failover": true,
		})
	}
	return append(hooks, map[string]any{
		"library": path.Join(in.HookDir, "libdhcp_ha.so"),
		"parameters": map[string]any{"high-availability": []any{map[string]any{
			"this-server-name": in.ThisServer,
			"mode":             "hot-standby",
			// Kea's defaults except these two, which is what puts takeover
			// inside a minute (§6).
			"heartbeat-delay":     10000,
			"max-response-delay":  60000,
			"max-ack-delay":       5000,
			"max-unacked-clients": 5,
			"peers":               peers,
		}}},
	}), nil
}

// noPeerHost is ErrNoPeerHost with the two things that make it actionable:
// what dns_listen says now, and an address on this machine that would work
// instead. Both are best-effort — a caller that supplied neither still gets
// the instruction, which is the part that matters.
func noPeerHost(in RenderInput) error {
	if in.OwnListen == "" {
		return ErrNoPeerHost
	}
	detail := " (it is " + in.OwnListen + ")"
	if addr := suggestedHost(in); addr != "" {
		if _, port, err := net.SplitHostPort(in.OwnListen); err == nil {
			detail += "; set one such as " + net.JoinHostPort(addr, port)
		}
	}
	return fmt.Errorf("%w%s", ErrNoPeerHost, detail)
}

// suggestedHost is an address this host actually holds, for the message
// above to name: the first non-loopback IPv4 among its interface prefixes,
// which is the same list §5.3 fills a scope's own resolver in from. Empty
// when there is nothing to suggest, which is a message with one clause
// fewer rather than a guess.
func suggestedHost(in RenderInput) string {
	for _, p := range in.LocalAddrs {
		if a := p.Addr().Unmap(); a.Is4() && !a.IsLoopback() {
			return a.String()
		}
	}
	return ""
}

func renderSubnets(in RenderInput) ([]any, error) {
	byScope := make(map[int64][]store.Reservation, len(in.Scopes))
	for _, r := range in.Reservations {
		byScope[r.ScopeID] = append(byScope[r.ScopeID], r)
	}
	subnets := make([]any, 0, len(in.Scopes))
	for _, s := range in.Scopes {
		if !s.Enabled {
			continue
		}
		options, err := renderOptions(in, s)
		if err != nil {
			return nil, err
		}
		subnet := map[string]any{"id": s.ID, "subnet": s.CIDR}
		// No pools at all is how Kea is told "reservations only": there is
		// no flag for it, and a pool the scope still carries is not one the
		// engine should be handing out of.
		if !s.ReservationsOnly {
			subnet["pools"] = []any{map[string]any{"pool": s.PoolStart + " - " + s.PoolEnd}}
		}
		if s.NextServer != "" {
			subnet["next-server"] = s.NextServer
		}
		if s.ServerHostname != "" {
			subnet["server-hostname"] = s.ServerHostname
		}
		if s.BootFile != "" {
			subnet["boot-file-name"] = s.BootFile
		}
		// Kea's own default is true, so the key is rendered only to turn it
		// off: the scopes that asked for it are the ones the config names.
		if !s.MatchClientID {
			subnet["match-client-id"] = false
		}
		// A subnet that overrides the lifetime needs its own timers too:
		// left to inherit the global ones, a short-lease scope would tell
		// clients to renew after their lease had already expired.
		if s.LeaseSeconds > 0 {
			subnet["valid-lifetime"] = s.LeaseSeconds
			subnet["renew-timer"] = renewTimer(s.LeaseSeconds)
			subnet["rebind-timer"] = rebindTimer(s.LeaseSeconds)
		}
		if len(options) > 0 {
			subnet["option-data"] = options
		}
		if res := renderReservations(byScope[s.ID]); len(res) > 0 {
			subnet["reservations"] = res
		}
		subnets = append(subnets, subnet)
	}
	return subnets, nil
}

// renderOptions emits an option only when it has a value: Kea reads an empty
// option-data entry as an error, not as "hand out nothing".
func renderOptions(in RenderInput, s store.Scope) ([]any, error) {
	var options []any
	if s.Gateway != "" {
		options = append(options, option("routers", s.Gateway))
	}
	servers, err := dnsServers(in, s)
	if err != nil {
		return nil, err
	}
	if servers != "" {
		options = append(options, option("domain-name-servers", servers))
	}
	domain := s.Domain
	if domain == "" {
		domain = in.Domain
	}
	if domain != "" {
		options = append(options, option("domain-name", domain))
	}
	if s.DomainSearch != "" {
		options = append(options, option("domain-search", s.DomainSearch))
	}
	if s.NTPServers != "" {
		options = append(options, option("ntp-servers", s.NTPServers))
	}
	if len(s.StaticRoutes) > 0 {
		options = append(options, option("classless-static-route", staticRoutes(s.StaticRoutes)))
	}
	// Last, and in the order they were typed: these are the codes dnsaur has
	// no opinion about, handed over as the bytes the operator gave.
	for _, o := range s.Options {
		options = append(options, map[string]any{"code": o.Code, "csv-format": false, "data": o.Hex})
	}
	return options, nil
}

// staticRoutes spells option 121 the way Kea's built-in definition takes it:
// "destination - router" pairs, comma-separated.
func staticRoutes(rs []store.StaticRoute) string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.Destination + " - " + r.Router
	}
	return strings.Join(out, ", ")
}

// Kea's own advice: renew at half the lifetime, rebind at seven eighths.
func renewTimer(lifetime int) int  { return lifetime / 2 }
func rebindTimer(lifetime int) int { return lifetime * 7 / 8 }

func option(name, data string) map[string]any {
	return map[string]any{"name": name, "data": data}
}

func renderReservations(rs []store.Reservation) []any {
	out := make([]any, 0, len(rs))
	for _, r := range rs {
		entry := map[string]any{"hw-address": r.MAC, "ip-address": r.IP}
		if r.Hostname != "" {
			entry["hostname"] = r.Hostname
		}
		out = append(out, entry)
	}
	return out
}

// dnsServers answers §5.3: the scope's own list wins whole, otherwise the
// caller's list with this box's own entry filled in. A DNS listener bound to
// every address names no single one, so the address on the scope's own
// segment stands in for it.
func dnsServers(in RenderInput, s store.Scope) (string, error) {
	if s.DNSServers != "" {
		return s.DNSServers, nil
	}
	servers := make([]string, len(in.DNSServers))
	for i, host := range in.DNSServers {
		if host == "" {
			local, err := localAddrIn(in.LocalAddrs, s)
			if err != nil {
				return "", err
			}
			host = local
		}
		servers[i] = host
	}
	return strings.Join(servers, ", "), nil
}

func localAddrIn(local []netip.Prefix, s store.Scope) (string, error) {
	cidr, err := netip.ParsePrefix(s.CIDR)
	if err != nil {
		return "", fmt.Errorf("scope %s: parsing cidr %q: %w", s.Name, s.CIDR, err)
	}
	for _, p := range local {
		// Unmap first: a v4 address the kernel reported in its 4-in-6
		// spelling is the same address, and neither Is4 nor Contains would
		// say so while it wears ::ffff:.
		if a := p.Addr().Unmap(); a.Is4() && !a.IsLoopback() && cidr.Contains(a) {
			return a.String(), nil
		}
	}
	return "", fmt.Errorf("scope %s: %w %s", s.Name, ErrNoLocalAddress, s.CIDR)
}

// atLeast compares the first three numeric fields of a version string
// against want. Kea reports major.minor.patch; a packager may add a fourth
// field to it, which says nothing about the engine's features. Anything that
// is not three numbers — too few fields, or a field carrying a suffix —
// counts as older, because the keys gated on a version are the ones an older
// engine refuses outright, taking the whole config with it.
func atLeast(version string, want [3]int) bool {
	fields := strings.Split(version, ".")
	if len(fields) < len(want) {
		return false
	}
	for i, w := range want {
		n, err := strconv.Atoi(fields[i])
		if err != nil {
			return false
		}
		if n != w {
			return n > w
		}
	}
	return true
}

func anySlice(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}
