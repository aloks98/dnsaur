package app

import (
	"context"
	"log/slog"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/aloks98/dnsaur/internal/confsync"
	"github.com/aloks98/dnsaur/internal/dhcp"
	"github.com/aloks98/dnsaur/internal/store"
	"github.com/aloks98/dnsaur/internal/zones"
)

// The DHCP settings this file reads, and the defaults §4.2 gives them. The
// defaults are also seeded into the settings table (defaultSettings), so a
// fallback here is reached only by a row someone emptied or a store that
// would not answer.
const (
	dhcpDomainSetting     = "dhcp.domain"
	dhcpLeaseSetting      = "dhcp.lease_seconds"
	dhcpHAPortSetting     = "dhcp.ha_port"
	dhcpInterfacesSetting = "serve.dhcp_interfaces"
	// dhcpHAPrimarySetting and dhcpHAStandbySetting are the pair the main
	// chose, written here and read on both boxes (§6). Synced and internal;
	// internal/api's own copies of the names are what strip them from GET
	// /settings and refuse them on PUT.
	dhcpHAPrimarySetting = "dhcp.ha_primary"
	dhcpHAStandbySetting = "dhcp.ha_standby"

	defaultLeaseSeconds = 3600
	defaultHAPort       = 8000
)

// dhcpLookupTimeout bounds the store reads behind the DNS stage's two
// suppliers. They run on the poller's goroutine, immediately after a table
// swap, so a store that has stopped answering costs that poll two seconds
// rather than holding it.
const dhcpLookupTimeout = 2 * time.Second

// RenderInput is App's half of dhcp.Inputs: everything the engine's
// configuration is made of, gathered from the store, the settings and this
// box's own place in the pair. The manager asks for it before every render
// and on every poll.
//
// It is also where the HA choice is made (§6): the main decides which
// replica is the standby, and both boxes have to render the same two peers
// or Kea's hook has no partner to find. Made here and published in Rendered,
// once the engine has taken a configuration carrying it.
func (a *App) RenderInput(ctx context.Context) (dhcp.RenderInput, error) {
	scopes, err := a.st.DHCP().Scopes(ctx)
	if err != nil {
		return dhcp.RenderInput{}, err
	}
	reservations, err := a.st.DHCP().Reservations(ctx)
	if err != nil {
		return dhcp.RenderInput{}, err
	}
	in := dhcp.RenderInput{
		Scopes:       scopes,
		Reservations: reservations,
		Domain:       a.getSetting(ctx, dhcpDomainSetting),
		LeaseSeconds: int(a.getInt(ctx, dhcpLeaseSetting, defaultLeaseSeconds)),
		Interfaces:   commaList(a.getSetting(ctx, dhcpInterfacesSetting)),
		Socket:       a.cfg.KeaSocket,
		OwnListen:    firstListen(a.cfg.DNSListen),
		ThisServer:   a.getSetting(ctx, instanceIDSetting),
		LocalAddrs:   localV4Prefixes(),
	}
	peers, dnsServers, err := a.dhcpPair(ctx, in.ThisServer)
	if err != nil {
		return dhcp.RenderInput{}, err
	}
	in.Peers, in.DNSServers = peers, dnsServers
	return in, nil
}

// instanceIDSetting is this box's identity, and the name every peer in the
// HA pair is rendered under.
const instanceIDSetting = "instance.id"

// dhcpPair answers §6 and §5.3 together, because they are the same question
// asked twice: which two boxes are in the pair, and therefore which two
// addresses every scope hands out as its resolvers.
//
// The DNS list is the same two addresses in the same order on both boxes,
// which is what DHCP-level failover needs from DNS: a client that keeps its
// lease through a takeover keeps its resolvers with it. This box's own entry
// is the empty string when its first dns_listen names no host — the renderer
// fills it in per scope from the host's own addresses (§5.3), which is the
// only way a wildcard listener can name itself.
func (a *App) dhcpPair(ctx context.Context, thisServer string) ([]dhcp.Peer, []string, error) {
	own := a.ownDNSHost()
	port := strconv.FormatInt(a.getInt(ctx, dhcpHAPortSetting, defaultHAPort), 10)
	if peer := a.replica.PeerURL(); peer != "" {
		peers, dnsServers := a.replicaPair(ctx, thisServer, own, port, peer)
		return peers, dnsServers, nil
	}
	return a.mainPair(ctx, thisServer, own, port)
}

// mainPair is the choice itself: the non-stale registered replica with the
// lexicographically smallest instance.id. Rendered publishes it in the two
// synced settings once the engine has accepted a configuration built on it,
// so it travels in the next bundle.
//
// Lexicographic and not "the first to register": the choice has to be the
// same one every render makes, on a box that may have restarted since, and a
// timestamp is not a tie-break two boxes can agree on. A stale replica is
// skipped — it is a box nothing has heard from for three intervals, and
// pairing Kea with it would leave the primary waiting out max-response-delay
// on every client.
//
// A registry read that fails is returned rather than swallowed: rendering
// "no partner" because a settings row could not be read would tear down a
// working pair, and the render that does not happen leaves the engine on the
// configuration it already has.
//
// Nothing else has to notice a box pairing or being forgotten. Neither
// writes a setting — a registration arrives every interval from every
// replica, so the registry is deliberately written without moving
// config_version — so the settings watcher never wakes for one. The lease
// poll is what notices: it reads this input every dhcp.lease_poll_seconds
// and renders when the pair it names has moved, which is what puts the
// change on both engines within one poll.
func (a *App) mainPair(ctx context.Context, thisServer, own, port string) ([]dhcp.Peer, []string, error) {
	reps, err := a.replicas.List(ctx)
	if err != nil {
		return nil, nil, err
	}
	standby, standbyHost := "", ""
	for _, r := range reps {
		if r.Stale {
			continue
		}
		// Once per replica per reason, not once per poll: this is read every
		// dhcp.lease_poll_seconds.
		slot := standbyWarnKey + "/" + r.InstanceID
		if !r.DHCP {
			// A box with no engine. Naming it the standby would hand Kea a
			// hot-standby pair whose partner answers nothing, and the
			// primary would wait out max-response-delay for it on every
			// client before serving them itself — strictly worse than the
			// single box this renders instead.
			if prev, warned := a.badSettings.Swap(slot, "engine"); !warned || prev != "engine" {
				slog.Warn("a registered replica runs no dhcp engine, so it cannot be the standby; set kea_socket on it",
					"instance_id", r.InstanceID)
			}
			continue
		}
		host, ok := hostOf(r.DNSAddr)
		if !ok {
			// dns_addr is validated as host:port when it is recorded, so
			// this is a row from a box that registered a hostname. Naming
			// one in a peer URL would have Kea resolve it through whatever
			// the host's resolver is, which on this machine is usually
			// dnsaur.
			if prev, warned := a.badSettings.Swap(slot, r.DNSAddr); !warned || prev != r.DNSAddr {
				slog.Warn("a registered replica's dns_addr is not an address, so it cannot be the dhcp standby",
					"instance_id", r.InstanceID, "dns_addr", r.DNSAddr)
			}
			continue
		}
		a.badSettings.Delete(slot)
		if standby == "" || r.InstanceID < standby {
			standby, standbyHost = r.InstanceID, host
		}
	}
	if standby == "" {
		return nil, []string{own}, nil
	}
	return haPeers(thisServer, own, standby, standbyHost, port), []string{own, standbyHost}, nil
}

// replicaPair is the other end of that choice, read rather than made: a
// replica renders the pair only when the main named it, and every other
// replica renders no HA section and serves DHCP with no partner.
//
// A forgotten standby is the case it cannot read its way out of. Forget
// revokes the secret with the entry, so the cleared pair never reaches this
// box in a bundle, and dhcp.ha_standby goes on naming it. What does reach it
// is the refusal itself, and that is enough: a main that will not take this
// box's token has removed it from its configuration, so the pair it is
// holding on to describes nothing. Read live, so a pull that succeeds again
// — the operator paired it back — restores the pair on the next render
// rather than leaving it out until a restart.
func (a *App) replicaPair(ctx context.Context, thisServer, own, port, peer string) ([]dhcp.Peer, []string) {
	if thisServer == "" || thisServer != a.getSetting(ctx, dhcpHAStandbySetting) {
		return nil, []string{own}
	}
	if strings.Contains(a.getSetting(ctx, confsync.LastErrorSetting), confsync.TokenRefused) {
		return nil, []string{own}
	}
	primary := a.getSetting(ctx, dhcpHAPrimarySetting)
	mainHost := a.mainDNSHost(ctx, peer)
	if primary == "" || mainHost == "" {
		// The main named a standby without naming itself, or this box
		// cannot say where the main answers DNS. Either way there is no
		// second peer to render, and half a pair is a config Kea refuses.
		return nil, []string{own}
	}
	return haPeers(primary, mainHost, thisServer, own, port), []string{mainHost, own}
}

// haPeers is the pair as the hook takes it, in one place because both boxes
// render the identical list — the same names, the same URLs, the same order
// — and only this-server-name differs.
func haPeers(primary, primaryHost, standby, standbyHost, port string) []dhcp.Peer {
	return []dhcp.Peer{
		{Name: primary, URL: "http://" + net.JoinHostPort(primaryHost, port) + "/", Role: "primary"},
		{Name: standby, URL: "http://" + net.JoinHostPort(standbyHost, port) + "/", Role: "standby"},
	}
}

// Rendered is dhcp.Inputs' other half: what became of one render (§5.1).
//
// The HA pair hangs off a render having been *accepted*, and is wrong
// hanging off anything else. The choice used to be written while the input
// was still being read, so a main whose render was then refused had already
// told its standby they were paired, and the standby rendered a partner that
// was serving nothing.
//
// What the engine was given is the manager's own record, kept by the code
// that sent it while it still holds the lock — nothing here has to.
func (a *App) Rendered(ctx context.Context, in dhcp.RenderInput, err error) {
	a.recordHAPair(ctx, in, err)
}

// recordHAPair publishes the pair the engine has just accepted, and clears
// it when there is none to publish (§6).
//
// Only on a main: the two settings are the main's to write, they are synced,
// and a replica writing them would be writing over what the next bundle
// carries.
//
// A render nothing took a view of — an engine that was not there, a deadline,
// a shutdown — changes nothing. The engine is still running the last
// configuration it accepted, and the pair recorded is still the pair in that
// configuration; clearing it because a socket went quiet would drop the
// standby out of a pair that is working.
//
// Both halves move together, so a replica that reads one never sees the
// other's leftovers. Written only when they changed: these are bumping
// writes, and writing them on every poll would move config_version every ten
// seconds and have every replica fetch a bundle for it.
func (a *App) recordHAPair(ctx context.Context, in dhcp.RenderInput, err error) {
	if a.replica.PeerURL() != "" || (err != nil && dhcp.Unanswered(err)) {
		return
	}
	primary, standby := "", ""
	if err == nil && len(in.Peers) == 2 {
		primary, standby = in.Peers[0].Name, in.Peers[1].Name
	}
	if a.getSetting(ctx, dhcpHAPrimarySetting) == primary && a.getSetting(ctx, dhcpHAStandbySetting) == standby {
		return
	}
	// Set, not SetInternal: the choice is configuration both boxes render
	// from, so it has to reach the replica in a bundle (§6). One SetMany, so
	// the pair moves the version once and is never half-visible.
	if err := a.st.Settings().SetMany(ctx, map[string]string{
		dhcpHAPrimarySetting: primary,
		dhcpHAStandbySetting: standby,
	}); err != nil {
		// Losing the note does not change what the engine is running, and
		// the next accepted render writes it again.
		slog.Warn("recording the dhcp ha pair failed", "err", err)
	}
}

// firstListen is this box's first dns_listen entry as written, for the one
// message that has to quote it back (ErrNoPeerHost).
func firstListen(listen []string) string {
	if len(listen) == 0 {
		return ""
	}
	return listen[0]
}

// ownDNSHost is the host part of this box's first dns_listen entry, and ""
// when that names no host — a wildcard listener, which is the default.
//
// Empty means two different things downstream, both right: §5.3's "fill this
// box's own address in per scope" for the DNS list, and "there is no address
// to put in an HA peer URL" for the pair. A two-box deployment names a real
// address here, which is the same rule the config-sync registration already
// depends on (see App.New).
func (a *App) ownDNSHost() string {
	if len(a.cfg.DNSListen) == 0 {
		return ""
	}
	host, ok := hostOf(a.cfg.DNSListen[0])
	if !ok {
		return ""
	}
	return host
}

// mainDNSHost is where the main answers DNS, as a replica knows it (§8):
// the sync.primary_dns override, or the peer URL's host.
//
// A hostname is not an answer, by the same rule that keeps a replica whose
// dns_addr is a hostname out of the pair: the name would go into a peer URL
// for Kea to resolve through whatever the host's resolver is, which on this
// machine is usually dnsaur — and into option 6, which carries addresses and
// not names. A main reached by name pairs by setting sync.primary_dns.
func (a *App) mainDNSHost(ctx context.Context, peer string) string {
	if override := a.getSetting(ctx, "sync.primary_dns"); override != "" {
		if host, ok := hostOf(override); ok {
			return host
		}
	}
	u, err := url.Parse(peer)
	if err != nil {
		return ""
	}
	host := u.Hostname()
	if _, err := netip.ParseAddr(host); err != nil {
		// Once per distinct value, not once per poll: this is read every
		// dhcp.lease_poll_seconds, and a line every ten seconds for as long
		// as it takes someone to read one is how a log stops being read.
		if prev, warned := a.badSettings.Swap(syncPeerURLSetting, host); !warned || prev != host {
			slog.Warn("the main is reached by name, so this box renders no dhcp pair; set sync.primary_dns",
				"peer", peer)
		}
		return ""
	}
	return host
}

// standbyWarnKey is not a setting: it prefixes the badSettings slot, one
// per replica, that "this replica cannot be the standby" is throttled on,
// and it shares that map because the map is exactly "the last thing each of
// these was warned about". A line every ten seconds is how a log stops
// being read.
const standbyWarnKey = "dhcp.standby"

// syncPeerURLSetting is the key mainDNSHost warns about. internal/api has
// the same constant for the same row; neither package can name the other's.
const syncPeerURLSetting = "sync.peer_url"

// hostOf is the host half of a host:port address, and false when it names no
// address anything can use: an empty host or one of the unspecified ones
// (both "every interface" rather than somewhere to dial), and a hostname —
// these values become peer URLs the engine dials and option 6 entries
// clients are handed, and neither takes a name.
func hostOf(addr string) (string, bool) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil || host == "" {
		return "", false
	}
	ip, err := netip.ParseAddr(host)
	if err != nil || ip.IsUnspecified() {
		return "", false
	}
	return host, true
}

// commaList splits a comma-separated setting the way every other one in this
// codebase is split: entries trimmed, blanks dropped, because " eth0" is not
// an interface name and a trailing comma is not an entry.
func commaList(v string) []string {
	out := []string{}
	for _, part := range strings.Split(v, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// localV4Prefixes is the host's own IPv4 interface prefixes, which is how
// §5.3 fills in the address of a box whose DNS listener names no host: the
// one that is inside the scope being rendered.
//
// Read on every render rather than once at start, because an address that
// moved — a DHCP lease on the WAN side, a VLAN brought up — would otherwise
// leave every scope handing out an address this box no longer has.
func localV4Prefixes() []netip.Prefix {
	ifaces, err := net.Interfaces()
	if err != nil {
		slog.Warn("reading this host's interfaces for the dhcp render failed", "err", err)
		return nil
	}
	var out []netip.Prefix
	for _, iface := range ifaces {
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			n, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			p, err := netip.ParsePrefix(n.String())
			if err != nil || !p.Addr().Unmap().Is4() {
				continue
			}
			out = append(out, p)
		}
	}
	return out
}

// reconcileDHCP is the settings watcher's half of §5.1: a settings write may
// be something the engine's configuration is built from, and most of the
// time it is not — a blocking pause is a settings row, and so is a retention
// day count. Manager.ApplyIfChanged is what decides, by rendering and
// comparing, so the engine is sent a config-set only when what it would be
// given has actually moved. The alternative is Kea rebuilding every subnet
// and re-reading its lease file to arrive at exactly what it was already
// running.
//
// A scope or reservation written on this box does not come through here at
// all: it changes no setting, so the watcher skips before it reaches this,
// and the handler that made the write has already applied it. A scope that
// arrives in a bundle does come through here, which is the whole of a
// replica's own trigger. What comes through neither — a replica pairing,
// being forgotten, or going stale — belongs to the lease poll, which runs
// the same gate on every tick.
func (a *App) reconcileDHCP(ctx context.Context) {
	if a.dhcp == nil {
		return
	}
	in, err := a.RenderInput(ctx)
	if err != nil {
		// Not rendered from a half-read configuration: the next reconcile,
		// or the next poll's own render, tries again against a store that is
		// answering.
		slog.Warn("reading the dhcp configuration to see whether it changed failed", "err", err)
		return
	}
	// The compare and the render are one critical section inside the
	// manager, which is what lets a config-sync pull and the settings
	// watcher reach this at the same moment — the pull's own import is what
	// wakes the watcher — without one of them returning before the render it
	// is responsible for has happened.
	if err := a.dhcp.ApplyIfChanged(ctx, in); err != nil {
		// The refusal itself is on the dashboard, put there by the manager.
		slog.Warn("applying the changed dhcp configuration failed", "err", err)
	}
}

// dhcpScopes and dhcpDomain are what the DNS stage names leases under
// (§8.1). dhcp.Names calls them once when it is built and again on every
// table swap, so they are reads of the two rows the table is keyed on rather
// than anything the query path touches.
func (a *App) dhcpScopes() []store.Scope {
	ctx, cancel := context.WithTimeout(context.Background(), dhcpLookupTimeout)
	defer cancel()
	scopes, err := a.st.DHCP().Scopes(ctx)
	if err != nil {
		slog.Warn("reading the dhcp scopes for the names stage failed", "err", err)
		return nil
	}
	return scopes
}

func (a *App) dhcpDomain() string {
	ctx, cancel := context.WithTimeout(context.Background(), dhcpLookupTimeout)
	defer cancel()
	return a.getSetting(ctx, dhcpDomainSetting)
}

// zoneHas is dhcp.ZoneCheck: a record an operator typed beats a name
// inferred from a lease (§8.1). It reads the served snapshot, not the store,
// so it is the records this server is actually answering with and it costs
// no query.
func (a *App) zoneHas(name string) bool {
	z := a.resolver.Snapshot().Find(name)
	if z == nil {
		return false
	}
	return len(z.Records[zones.RelName(name, z.Name)]) > 0
}

// leaseHostname is api.Deps.LeaseHostname: the lease table's name for an
// address, joined onto query-log rows as they are read (§8.2).
func (a *App) leaseHostname(ip netip.Addr) (string, bool) {
	e, ok := a.dhcp.Table().ByIP(ip)
	if !ok || e.Hostname == "" {
		return "", false
	}
	return e.Hostname, true
}

// leaseAddrOf is what the client registry resolves a `mac` matcher through
// (§8.2). It closes over one table, so a matcher resolved from it is
// resolved against the leases as of one poll.
func leaseAddrOf(t *dhcp.Table) func(string) (netip.Addr, bool) {
	return func(mac string) (netip.Addr, bool) {
		e, ok := t.ByMAC(mac)
		return e.IP, ok
	}
}
