package dhcp_test

import (
	"cmp"
	"context"
	"errors"
	"net"
	"net/netip"
	"os"
	"path"
	"slices"
	"testing"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/insomniacslk/dhcp/dhcpv4/nclient4"
	"github.com/mdlayher/packet"

	"github.com/aloks98/dnsaur/internal/dhcp"
	"github.com/aloks98/dnsaur/internal/store"
)

// TestRealEngine drives a real kea-dhcp4 over its control socket, which is
// the one thing keatest's stand-in cannot answer: whether Kea accepts what
// Render produces, and whether what it hands back decodes. Everything here
// is a command the manager sends on a normal run — version-get, config-get,
// config-set, lease4-get-page, status-get — plus the lease4-add that puts a
// lease in the database for the page to find.
//
// Gated on DNSAUR_TEST_KEA_SOCKET, the path of a control socket this process
// can read and write. CI starts one from the Debian package in a container
// (.forgejo/workflows/ci.yml, job test-kea); docs/development.md has the
// same container as a local one-liner.
//
// It replaces the engine's whole configuration, so point it at a throwaway
// engine and not at one serving a segment.
func TestRealEngine(t *testing.T) {
	socket := os.Getenv("DNSAUR_TEST_KEA_SOCKET")
	if socket == "" {
		t.Skip("set DNSAUR_TEST_KEA_SOCKET to a kea-dhcp4 control socket to run this")
	}
	ctx := t.Context()
	c := dhcp.NewClient(socket)

	version, err := c.Version(ctx)
	if err != nil {
		t.Fatalf("version-get on %s: %v", socket, err)
	}
	if version == "" {
		t.Fatal("version-get answered with no version")
	}
	t.Logf("engine version %s", version)

	cfg, err := c.ConfigGet(ctx)
	if err != nil {
		t.Fatalf("config-get: %v", err)
	}
	hookDir, socketName := engineFacts(t, cfg, socket)

	in := dhcp.RenderInput{
		Scopes: []store.Scope{{
			ID: 1, Name: "lan", CIDR: "10.42.0.0/24",
			Pools:   []store.Pool{{Start: "10.42.0.100", End: "10.42.0.200"}},
			Gateway: "10.42.0.1", Enabled: true, MatchClientID: true,
		}},
		Reservations: []store.Reservation{
			{ID: 1, ScopeID: 1, MAC: "aa:bb:cc:dd:ee:01", IP: "10.42.0.50", Hostname: "printer"},
		},
		Domain:       "home.lan",
		LeaseSeconds: 3600,
		Socket:       socket,
		EngineSocket: socketName,
		HookDir:      hookDir,
		KeaVersion:   version,
		ThisServer:   "main",
		DNSServers:   []string{"10.42.0.2"},
	}
	dhcp4, err := dhcp.Render(in)
	if err != nil {
		t.Fatalf("rendering: %v", err)
	}
	if err := c.ConfigSet(ctx, dhcp4); err != nil {
		t.Fatalf("config-set of the rendered configuration: %v", err)
	}

	// subnet-id 1 is the scope just rendered, and Kea refuses a lease for a
	// subnet it does not have — so an accepted lease4-add is also the proof
	// that the config-set above took effect.
	const (
		leaseIP  = "10.42.0.123"
		leaseMAC = "aa:bb:cc:dd:ee:02"
	)
	// The engine's lease database outlives a run (memfile, persist: true),
	// so the address is cleared before it is taken: running this twice
	// against the same engine is the normal local case, and Kea refuses to
	// add a lease it already holds.
	_ = c.DeleteLease(ctx, leaseIP)
	resp, err := c.Command(ctx, "lease4-add", map[string]any{
		"ip-address": leaseIP, "hw-address": leaseMAC, "hostname": "laptop",
		"subnet-id": 1, "valid-lft": 3600,
	})
	if err != nil {
		t.Fatalf("lease4-add: %v", err)
	}
	if resp.Result != 0 {
		t.Fatalf("lease4-add answered %d %q, want 0", resp.Result, resp.Text)
	}

	leases, next, err := c.LeasePage(ctx, "", 100)
	if err != nil {
		t.Fatalf("lease4-get-page: %v", err)
	}
	if next != "" {
		t.Errorf("a page of %d leases says there is another; want the last page", len(leases))
	}
	var found *dhcp.Lease
	for i, l := range leases {
		if l.IP == leaseIP {
			found = &leases[i]
		}
	}
	if found == nil {
		t.Fatalf("the lease page holds %+v, not the %s added above", leases, leaseIP)
	}
	if found.MAC != leaseMAC || found.Hostname != "laptop" || found.SubnetID != 1 {
		t.Errorf("the lease decoded as %+v, want mac %s, hostname laptop, subnet 1", *found, leaseMAC)
	}
	if found.ValidLft != 3600 || found.CLTT == 0 || found.State != 0 {
		t.Errorf("the lease decoded as %+v, want valid-lft 3600, a cltt and state 0", *found)
	}

	// lease4-del is what Release sends (design §8.3), and it leaves the
	// engine as this test found it.
	if err := c.DeleteLease(ctx, leaseIP); err != nil {
		t.Fatalf("lease4-del: %v", err)
	}

	uptime, ha, err := c.Status(ctx)
	if err != nil {
		t.Fatalf("status-get: %v", err)
	}
	if uptime < 0 {
		t.Errorf("status-get reports an uptime of %d seconds", uptime)
	}
	// A single box renders no HA section (design §6), so the engine running
	// what was rendered above has no HA relationship to report.
	if ha != nil {
		t.Errorf("status-get reports %+v, want no HA on a single box", *ha)
	}

	// Classes and pools, before the pair: a hot-standby primary whose
	// partner never answers does not serve, and this half needs a server.
	realClasses(t, c, in)

	// And the pair, which is the half keatest cannot answer at all: whether
	// Kea loads libdhcp_ha.so out of the same directory, takes the
	// relationship this renders, and reports a partner back through
	// status-get — the "<peer>" in the dashboard's engine line (§7.2, §8.4).
	//
	// Both peer URLs are on loopback, and this box is the primary, so the
	// hook's own HTTP listener binds an address the engine has. The standby
	// is never reachable; that is a partner that is down, not a
	// configuration Kea refuses, and it is the state this asserts against.
	in.ThisServer = "main"
	in.Peers = []dhcp.Peer{
		{Name: "main", URL: "http://127.0.0.1:18000/", Role: "primary"},
		{Name: "backup", URL: "http://127.0.0.2:18000/", Role: "standby"},
	}
	dhcp4, err = dhcp.Render(in)
	if err != nil {
		t.Fatalf("rendering the pair: %v", err)
	}
	if err := c.ConfigSet(ctx, dhcp4); err != nil {
		t.Fatalf("config-set of the rendered pair: %v", err)
	}
	if _, ha, err = c.Status(ctx); err != nil {
		t.Fatalf("status-get with the ha hook loaded: %v", err)
	}
	if ha == nil {
		t.Fatal("status-get reports no HA relationship on an engine running the rendered pair")
	}
	if ha.RemoteName != "backup" {
		t.Errorf("status-get reports partner %q, want the name this config gave it", ha.RemoteName)
	}
	if ha.Mode != "hot-standby" || ha.LocalState == "" {
		t.Errorf("ha = %+v, want hot-standby with a local state", *ha)
	}
}

// engineFacts is what the manager reads out of config-get before it renders:
// the directory the engine's own hook libraries sit in, and the control
// socket it was started with.
//
// The socket is read back rather than taken from the environment because the
// test may reach the engine through a path the engine does not have — a
// bind-mounted directory, in CI — and Kea keeps its control socket where it
// was started.
func engineFacts(t *testing.T, cfg map[string]any, socket string) (hookDir, socketName string) {
	t.Helper()
	dhcp4, _ := cfg["Dhcp4"].(map[string]any)
	hooks, _ := dhcp4["hooks-libraries"].([]any)
	for _, h := range hooks {
		entry, _ := h.(map[string]any)
		if lib, _ := entry["library"].(string); lib != "" {
			hookDir = path.Dir(lib)
			break
		}
	}
	if hookDir == "" {
		t.Fatal("the engine names no hook library, so there is no hook directory to render with: start it with libdhcp_lease_cmds.so")
	}
	// Either spelling: "control-socket" below 2.7.2, "control-sockets" from
	// 2.7.2 on (design §6).
	if one, ok := dhcp4["control-socket"].(map[string]any); ok {
		socketName, _ = one["socket-name"].(string)
	}
	if many, ok := dhcp4["control-sockets"].([]any); ok && len(many) > 0 {
		if first, ok := many[0].(map[string]any); ok {
			socketName, _ = first["socket-name"].(string)
		}
	}
	if socketName == "" {
		socketName = socket
	}
	t.Logf("engine hooks in %s, control socket %s", hookDir, socketName)
	return hookDir, socketName
}

// realClasses is design §4's precedence claim, asked of the engine itself
// with real DISCOVER/REQUEST exchanges rather than read out of its manual:
// a class's options beat the scope's only on the class's own pool, and a
// class with no pool fills in only what the scope leaves blank.
//
// The exchange runs from inside the engine's container, on the interface
// DNSAUR_TEST_KEA_IFACE names (eth0 by default, "none" for an engine this
// process shares no segment with). That interface sits on Docker's default
// bridge, so the scope is the bridge's subnet: Kea answers a client on a
// directly attached segment only from the subnet that interface is in. The
// pools sit far above the addresses Docker hands its containers.
func realClasses(t *testing.T, c *dhcp.Client, base dhcp.RenderInput) {
	t.Helper()
	ctx := t.Context()
	iface := cmp.Or(os.Getenv("DNSAUR_TEST_KEA_IFACE"), "eth0")
	in := base
	in.Reservations = nil
	in.Interfaces = nil
	if iface != "none" {
		in.Interfaces = []string{iface}
	}
	in.DNSServers = []string{"172.17.0.2"}
	in.Classes = []store.Class{
		{ID: 1, Name: "iot", Matchers: []string{"mac:a4:cf:12"},
			ClientOptions: store.ClientOptions{DNSServers: "172.17.0.9", Domain: "iot.lan"}},
		// No pool names it. Its DNS server is one the scope sets, so it must
		// lose; its boot file is one the scope leaves blank, so it must win.
		{ID: 2, Name: "pxe", Matchers: []string{"vendor:PXEClient"},
			ClientOptions: store.ClientOptions{DNSServers: "172.17.0.99", BootFile: "bootx64.efi"}},
	}
	in.Scopes = []store.Scope{{
		ID: 2, Name: "bridge", CIDR: "172.17.0.0/16",
		Pools: []store.Pool{
			{Start: "172.17.100.100", End: "172.17.100.149"},
			{Start: "172.17.100.150", End: "172.17.100.200", ClassID: 1},
		},
		Gateway: "172.17.0.1", Enabled: true, MatchClientID: true,
	}}
	dhcp4, err := dhcp.Render(in)
	if err != nil {
		t.Fatalf("rendering classes and pools: %v", err)
	}
	if err := c.ConfigSet(ctx, dhcp4); err != nil {
		t.Fatalf("config-set of the rendered classes and pools: %v", err)
	}
	if iface == "none" {
		t.Log("DNSAUR_TEST_KEA_IFACE=none: the engine took the classes, no exchange was run")
		return
	}
	if !onBridge(t, iface) {
		t.Fatalf("%s has no address in 172.17.0.0/16, the subnet this renders; "+
			"run the engine on Docker's default bridge, or set DNSAUR_TEST_KEA_IFACE=none", iface)
	}

	anyPool := [2]string{"172.17.100.100", "172.17.100.149"}
	iotPool := [2]string{"172.17.100.150", "172.17.100.200"}
	for _, tc := range []struct {
		name, mac, vendor string
		pool              [2]string
		dns, domain, boot string
	}{
		// The class's pool, though the open pool is listed first: the guard
		// closes it to members. Its options from there beat the subnet's.
		{"iot member", "a4:cf:12:00:00:01", "", iotPool, "172.17.0.9", "iot.lan", ""},
		// No pool of its own: the any pool, the subnet's DNS over the
		// class's, and the class's boot file where the subnet has none.
		{"pxe member", "02:00:00:00:00:02", "PXEClient:Arch:00007", anyPool, "172.17.0.2", "home.lan", "bootx64.efi"},
		{"no class", "02:00:00:00:00:03", "", anyPool, "172.17.0.2", "home.lan", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ack := exchange(t, iface, tc.mac, tc.vendor)
			ip, _ := netip.AddrFromSlice(ack.YourIPAddr.To4())
			t.Cleanup(func() { _ = c.DeleteLease(context.Background(), ip.String()) })
			dns := ack.DNS()
			t.Logf("%s: address %s, dns %v, domain %q, file %q, option 67 %q",
				tc.mac, ip, dns, ack.DomainName(), ack.BootFileName, ack.BootFileNameOption())
			if ip.Compare(netip.MustParseAddr(tc.pool[0])) < 0 || ip.Compare(netip.MustParseAddr(tc.pool[1])) > 0 {
				t.Errorf("address %s, want one in %s-%s", ip, tc.pool[0], tc.pool[1])
			}
			if len(dns) != 1 || dns[0].String() != tc.dns {
				t.Errorf("dns %v, want [%s]", dns, tc.dns)
			}
			if got := ack.DomainName(); got != tc.domain {
				t.Errorf("domain %q, want %q", got, tc.domain)
			}
			if ack.BootFileName != tc.boot {
				t.Errorf("boot file %q, want %q", ack.BootFileName, tc.boot)
			}
		})
	}
}

// exchange runs one DISCOVER/OFFER/REQUEST/ACK as the client mac, sending
// vendor as option 60 when it is set, and returns the ACK. Broadcast, since
// the client has no address to be answered at.
func exchange(t *testing.T, iface, mac, vendor string) *dhcpv4.DHCPv4 {
	t.Helper()
	hw, err := net.ParseMAC(mac)
	if err != nil {
		t.Fatal(err)
	}
	ifc, err := net.InterfaceByName(iface)
	if err != nil {
		t.Fatalf("interface %s: %v", iface, err)
	}
	// Two sockets, because the client shares the engine's interface. Kea's
	// raw socket hands its answer to the driver, and the kernel shows a
	// frame this host sends only to packet sockets bound to every protocol,
	// never to one bound to IPv4 alone — which is what nclient4.New opens.
	// Reading needs the first kind; writing needs the second, because a
	// packet socket stamps its bound protocol on what it sends.
	const ethAll, ethIPv4 = 0x0003, 0x0800
	r, err := packet.Listen(ifc, packet.Datagram, ethAll, nil)
	if err != nil {
		t.Fatalf("opening a packet socket on %s: %v", iface, err)
	}
	w, err := packet.Listen(ifc, packet.Datagram, ethIPv4, nil)
	if err != nil {
		_ = r.Close()
		t.Fatalf("opening a packet socket on %s: %v", iface, err)
	}
	conn := nclient4.NewBroadcastUDPConn(splitConn{r, w}, &net.UDPAddr{Port: nclient4.ClientPort})
	client, err := nclient4.NewWithConn(conn, hw, nclient4.WithTimeout(2*time.Second), nclient4.WithRetry(3))
	if err != nil {
		t.Fatalf("opening a dhcp client on %s: %v", iface, err)
	}
	defer func() { _ = client.Close() }()
	mods := []dhcpv4.Modifier{dhcpv4.WithBroadcast(true)}
	if vendor != "" {
		mods = append(mods, dhcpv4.WithOption(dhcpv4.OptClassIdentifier(vendor)))
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	lease, err := client.Request(ctx, mods...)
	if err != nil {
		t.Fatalf("exchange as %s: %v", mac, err)
	}
	return lease.ACK
}

// splitConn reads from one packet socket and writes through another.
type splitConn struct {
	net.PacketConn
	w net.PacketConn
}

func (c splitConn) WriteTo(b []byte, addr net.Addr) (int, error) { return c.w.WriteTo(b, addr) }

func (c splitConn) Close() error { return errors.Join(c.PacketConn.Close(), c.w.Close()) }

// onBridge reports whether iface holds an address in 172.17.0.0/16.
func onBridge(t *testing.T, iface string) bool {
	t.Helper()
	ifc, err := net.InterfaceByName(iface)
	if err != nil {
		t.Fatalf("interface %s: %v", iface, err)
	}
	addrs, err := ifc.Addrs()
	if err != nil {
		t.Fatalf("addresses of %s: %v", iface, err)
	}
	bridge := netip.MustParsePrefix("172.17.0.0/16")
	return slices.ContainsFunc(addrs, func(a net.Addr) bool {
		p, err := netip.ParsePrefix(a.String())
		return err == nil && bridge.Contains(p.Addr())
	})
}
