package dhcp_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/aloks98/dnsaur/internal/dhcp"
	"github.com/aloks98/dnsaur/internal/store"
)

var update = flag.Bool("update", false, "rewrite the testdata goldens from the rendered output")

const (
	debianHooks = "/usr/lib/x86_64-linux-gnu/kea/hooks"
	alpineHooks = "/usr/lib/kea/hooks"
)

// baseInput is one enabled scope with one reservation on a single box. Every
// field carries a value distinct from every other, so a golden that swapped
// two of them (pool start for pool end, own DNS for partner) would not match.
func baseInput() dhcp.RenderInput {
	return dhcp.RenderInput{
		Scopes: []store.Scope{{
			ID: 1, Name: "lan", CIDR: "10.42.0.0/24",
			PoolStart: "10.42.0.100", PoolEnd: "10.42.0.200",
			Gateway: "10.42.0.1", Enabled: true, MatchClientID: true,
		}},
		Reservations: []store.Reservation{
			{ID: 1, ScopeID: 1, MAC: "aa:bb:cc:dd:ee:01", IP: "10.42.0.50", Hostname: "printer"},
		},
		Domain:       "home.lan",
		LeaseSeconds: 3600,
		Socket:       "/run/kea/kea.sock",
		HookDir:      debianHooks,
		KeaVersion:   "2.6.3",
		ThisServer:   "main",
		DNSServers:   []string{"10.42.0.2"},
	}
}

func pairPeers() []dhcp.Peer {
	return []dhcp.Peer{
		{Name: "main", URL: "http://10.42.0.2:8000/", Role: "primary"},
		{Name: "replica", URL: "http://10.42.0.3:8000/", Role: "standby"},
	}
}

// paired and pairedStandby are the two boxes of one pair. Both hand out the
// main's address first and the standby's second — the list is the caller's,
// in hand-out order — and each box spells its own entry "", so the two inputs
// differ only in which entry is empty, in this-server-name, and in version.
func paired() dhcp.RenderInput {
	in := baseInput()
	in.Interfaces = []string{"eth0", "eth0.10"}
	in.Peers = pairPeers()
	in.DNSServers = []string{"", "10.42.0.3"}
	in.LocalAddrs = []netip.Prefix{netip.MustParsePrefix("10.42.0.2/24")}
	in.KeaVersion = "3.0.3"
	return in
}

func pairedStandby() dhcp.RenderInput {
	in := paired()
	in.ThisServer = "replica"
	in.DNSServers = []string{"10.42.0.2", ""}
	in.LocalAddrs = []netip.Prefix{netip.MustParsePrefix("10.42.0.3/24")}
	in.KeaVersion = "2.6.3"
	return in
}

// twoScopes overrides everything a scope may override, keeps a second scope
// on the settings' defaults, and disables a third that owns a reservation of
// its own: neither the subnet nor its reservation may reach the engine.
func twoScopes() dhcp.RenderInput {
	in := baseInput()
	in.Scopes = []store.Scope{
		{
			ID: 1, Name: "lan", CIDR: "10.42.0.0/24",
			PoolStart: "10.42.0.100", PoolEnd: "10.42.0.200",
			Gateway: "10.42.0.1", DNSServers: "1.1.1.1, 8.8.8.8",
			Domain: "lab.lan", LeaseSeconds: 900, Enabled: true, MatchClientID: true,
		},
		{
			ID: 2, Name: "guest", CIDR: "10.43.0.0/24",
			PoolStart: "10.43.0.10", PoolEnd: "10.43.0.250", Enabled: true, MatchClientID: true,
		},
		{
			ID: 3, Name: "retired", CIDR: "10.44.0.0/24",
			PoolStart: "10.44.0.10", PoolEnd: "10.44.0.250", Enabled: false, MatchClientID: true,
		},
	}
	in.Reservations = []store.Reservation{
		{ID: 1, ScopeID: 1, MAC: "aa:bb:cc:dd:ee:01", IP: "10.42.0.50", Hostname: "printer"},
		{ID: 2, ScopeID: 2, MAC: "aa:bb:cc:dd:ee:02", IP: "10.43.0.50"},
		{ID: 3, ScopeID: 3, MAC: "aa:bb:cc:dd:ee:03", IP: "10.44.0.50", Hostname: "gone"},
		{ID: 4, ScopeID: 1, MAC: "aa:bb:cc:dd:ee:04", IP: "10.42.0.51", Hostname: "scanner"},
	}
	in.DNSServers = []string{"", "10.9.9.9"}
	in.LocalAddrs = []netip.Prefix{netip.MustParsePrefix("10.43.0.2/24")}
	return in
}

// optionsFull sets every option column a scope has: the search list, NTP,
// two static routes (two, so the separator between them is pinned as well as
// the "dest - router" inside one), PXE's three fields, two generic options,
// and match-client-id off. Every value is distinct, so an option rendered
// from the wrong column would not match.
func optionsFull() dhcp.RenderInput {
	in := baseInput()
	sc := &in.Scopes[0]
	sc.DomainSearch = "home.lan, lab.lan"
	sc.NTPServers = "10.42.0.5, 10.42.0.6"
	sc.StaticRoutes = []store.StaticRoute{
		{Destination: "10.10.0.0/16", Router: "10.42.0.1"},
		{Destination: "192.168.5.0/24", Router: "10.42.0.2"},
	}
	sc.NextServer, sc.ServerHostname, sc.BootFile = "10.42.0.9", "boot.lan", "pxelinux.0"
	sc.Options = []store.GenericOption{{Code: 150, Hex: "0A2A0005"}, {Code: 44, Hex: "0A2A000A"}}
	sc.MatchClientID = false
	return in
}

// reservationsOnly keeps the scope's pool columns filled in and asks for
// reservations only anyway — the store allows either — so the golden says
// the subnet reaches Kea with no pools at all rather than only that an empty
// pool renders to nothing.
func reservationsOnly() dhcp.RenderInput {
	in := baseInput()
	in.Scopes[0].ReservationsOnly = true
	return in
}

func TestRenderGolden(t *testing.T) {
	cases := []struct {
		name string
		in   dhcp.RenderInput
	}{
		// No peers: no HA hook, no http control socket, and the automatic
		// DNS list is the box's own address alone. Interfaces unset → "*".
		{"single-box", baseInput()},
		// Kea 3.0.3 with a pair: HA hook plus the http control socket.
		{"paired-main", paired()},
		// The same pair rendered on the replica: this-server-name moves,
		// the DNS list does not. 2.6.3 has no http control socket.
		{"paired-standby", pairedStandby()},
		{"two-scopes-overrides", twoScopes()},
		// Every scope option at once (§5.2), and a subnet that hands out
		// nothing but its reservations.
		{"options-full", optionsFull()},
		{"reservations-only", reservationsOnly()},
		// Alpine's hook directory, and a version new enough for the http
		// control socket on a box that is not in a pair.
		{"alpine-hookdir", func() dhcp.RenderInput {
			in := baseInput()
			in.HookDir = alpineHooks
			in.KeaVersion = "3.0.3"
			return in
		}()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := dhcp.Render(tc.in)
			if err != nil {
				t.Fatalf("Render: %v", err)
			}
			b, err := json.MarshalIndent(got, "", "  ")
			if err != nil {
				t.Fatalf("marshalling the rendered config: %v", err)
			}
			b = append(b, '\n')
			path := filepath.Join("testdata", tc.name+".golden.json")
			if *update {
				if err := os.WriteFile(path, b, 0o644); err != nil {
					t.Fatalf("writing %s: %v", path, err)
				}
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("reading %s (run with -update to create it): %v", path, err)
			}
			oneControlSocket(t, got)
			if !bytes.Equal(b, want) {
				t.Errorf("rendered config does not match %s\n--- got ---\n%s\n--- want ---\n%s", path, b, want)
			}
		})
	}
}

// The control socket's key is spelled differently either side of 2.7.2, and
// an engine refuses the spelling it does not know, taking the whole config
// with it — so the boundary is pinned on both sides, as is the rule that only
// one of the two keys is ever rendered. A string comparison would pass
// "3.0.3" and fail "10.0.0" and "2.7.10".
func TestRenderGatesTheControlSocketSpellingOnTheVersion(t *testing.T) {
	for version, plural := range map[string]bool{
		"2.6.3":  false,
		"2.7.1":  false,
		"2.7.2":  true,
		"2.7.10": true,
		"2.8.0":  true,
		"3.0.3":  true,
		"10.0.0": true,
		// A fourth field is a packaging suffix on a version that already
		// qualifies, not a reason to withhold the key.
		"2.7.2.1": true,
		// Neither is a major.minor.patch: too few fields, or a field that
		// is not a number.
		"2.7":       false,
		"":          false,
		"2.7.2-git": false,
		"2.6.3-1":   false,
	} {
		t.Run(version, func(t *testing.T) {
			in := baseInput()
			in.KeaVersion = version
			cfg, err := dhcp.Render(in)
			if err != nil {
				t.Fatalf("Render: %v", err)
			}
			// Unix, never http: the HA hook owns the HTTP listener.
			unix := map[string]any{"socket-type": "unix", "socket-name": in.Socket}
			var want any = unix
			key := "control-socket"
			if plural {
				want, key = []any{unix}, "control-sockets"
			}
			if got := cfg[key]; !reflect.DeepEqual(got, want) {
				t.Errorf("Kea %q rendered %s = %#v, want %#v", version, key, got, want)
			}
			oneControlSocket(t, cfg)
		})
	}
}

// §5.3: the empty entry means "this box's address on this scope's segment",
// which is not simply the first address the host has. It keeps its place in
// the list wherever the caller put it.
func TestRenderEmptyDNSEntryUsesTheScopeLocalAddress(t *testing.T) {
	// Loopback first, then an address on another segment: only the third
	// prefix is inside the scope, so "take the first address" does not pass.
	scattered := []netip.Prefix{
		netip.MustParsePrefix("127.0.0.1/8"),
		netip.MustParsePrefix("192.168.1.5/24"),
		netip.MustParsePrefix("10.42.0.2/24"),
	}
	for _, tc := range []struct {
		name    string
		servers []string
		local   []netip.Prefix // nil: scattered
		want    string
	}{
		{name: "own first", servers: []string{"", "10.42.0.3"}, want: "10.42.0.2, 10.42.0.3"},
		{name: "own second", servers: []string{"10.42.0.3", ""}, want: "10.42.0.3, 10.42.0.2"},
		{name: "own alone", servers: []string{""}, want: "10.42.0.2"},
		// Each empty entry is resolved on its own and keeps its place.
		{name: "own twice", servers: []string{"", ""}, want: "10.42.0.2, 10.42.0.2"},
		// A v6 address cannot be a v4 client's resolver, however early in
		// the host's interface list it sits.
		{name: "ipv6 skipped", servers: []string{""}, local: []netip.Prefix{
			netip.MustParsePrefix("2001:db8::2/64"),
			netip.MustParsePrefix("10.42.0.2/24"),
		}, want: "10.42.0.2"},
		// A v4 address the kernel handed over in its 4-in-6 spelling is the
		// same address, and is handed out in dotted quad.
		{name: "4-in-6 unmapped", servers: []string{""}, local: []netip.Prefix{
			netip.MustParsePrefix("::ffff:10.42.0.2/120"),
		}, want: "10.42.0.2"},
		// Two interfaces on the same segment: the first wins, so the answer
		// does not depend on map ordering anywhere upstream.
		{name: "first of two on the segment", servers: []string{""}, local: []netip.Prefix{
			netip.MustParsePrefix("10.42.0.2/24"),
			netip.MustParsePrefix("10.42.0.9/24"),
		}, want: "10.42.0.2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := baseInput()
			in.DNSServers = tc.servers
			in.LocalAddrs = scattered
			if tc.local != nil {
				in.LocalAddrs = tc.local
			}
			cfg, err := dhcp.Render(in)
			if err != nil {
				t.Fatalf("Render: %v", err)
			}
			if got := option(t, cfg, 0, "domain-name-servers"); got != tc.want {
				t.Errorf("domain-name-servers = %q, want %q", got, tc.want)
			}
		})
	}
}

// What the hand-out order is for: a client that renews against the other box
// must be told the same resolvers in the same order (§5.3).
func TestRenderPairHandsOutTheSameDNSServers(t *testing.T) {
	main, err := dhcp.Render(paired())
	if err != nil {
		t.Fatalf("Render main: %v", err)
	}
	standby, err := dhcp.Render(pairedStandby())
	if err != nil {
		t.Fatalf("Render standby: %v", err)
	}
	got, want := option(t, standby, 0, "domain-name-servers"), option(t, main, 0, "domain-name-servers")
	if got != want {
		t.Errorf("standby hands out %q, main hands out %q", got, want)
	}
	if want != "10.42.0.2, 10.42.0.3" {
		t.Errorf("the pair hands out %q, want %q", want, "10.42.0.2, 10.42.0.3")
	}
}

func TestRenderRefusesWhenNoLocalAddressFits(t *testing.T) {
	in := baseInput()
	in.DNSServers = []string{"", "10.42.0.3"}
	in.LocalAddrs = []netip.Prefix{netip.MustParsePrefix("192.168.1.5/24")}

	_, err := dhcp.Render(in)
	if !errors.Is(err, dhcp.ErrNoLocalAddress) {
		t.Fatalf("Render error = %v, want ErrNoLocalAddress", err)
	}
	// The operator has to know which scope to fix and what its subnet is.
	if !strings.Contains(err.Error(), "lan") || !strings.Contains(err.Error(), "10.42.0.0/24") {
		t.Errorf("error %q names neither the scope nor its CIDR", err)
	}
}

// oneControlSocket is Kea 3.0's rule, which every rendered config obeys: the
// two spellings of the key are alternatives, and a config carrying both is
// refused with "duplicate control-socket entries".
func oneControlSocket(t *testing.T, cfg map[string]any) {
	t.Helper()
	_, singular := cfg["control-socket"]
	_, plural := cfg["control-sockets"]
	if singular == plural {
		t.Errorf("control-socket present = %v, control-sockets present = %v; want exactly one", singular, plural)
	}
}

// option digs one option-data value out of subnet4[i].
func option(t *testing.T, cfg map[string]any, subnet int, name string) string {
	t.Helper()
	subnets, ok := cfg["subnet4"].([]any)
	if !ok || len(subnets) <= subnet {
		t.Fatalf("subnet4 is %#v, want at least %d entries", cfg["subnet4"], subnet+1)
	}
	sub, ok := subnets[subnet].(map[string]any)
	if !ok {
		t.Fatalf("subnet4[%d] is %#v, want an object", subnet, subnets[subnet])
	}
	options, ok := sub["option-data"].([]any)
	if !ok {
		t.Fatalf("subnet4[%d] option-data is %#v, want a list", subnet, sub["option-data"])
	}
	for _, o := range options {
		o, ok := o.(map[string]any)
		if !ok {
			t.Fatalf("subnet4[%d] has an option-data entry %#v, want an object", subnet, o)
		}
		if o["name"] == name {
			data, ok := o["data"].(string)
			if !ok {
				t.Fatalf("subnet4[%d] option %s has data %#v, want a string", subnet, name, o["data"])
			}
			return data
		}
	}
	t.Fatalf("subnet4[%d] has no %s option: %#v", subnet, name, sub["option-data"])
	return ""
}

// TestRenderRefusesAPeerURLWithNoHost is §6's other dead end, and it is the
// one a two-box deployment falls into by not changing anything: dns_listen
// defaults to ":53", so the box's own peer entry is built from a host that
// does not exist and the hook is handed "http://:8000/".
//
// Kea would refuse that config itself, which is one way to find out. This
// refuses it here instead, because the message an operator can act on names
// dns_listen, and the engine's does not.
func TestRenderRefusesAPeerURLWithNoHost(t *testing.T) {
	in := paired()
	in.DNSServers = []string{"10.42.0.2", "10.42.0.3"}
	in.Peers = []dhcp.Peer{
		{Name: "main", URL: "http://:8000/", Role: "primary"},
		{Name: "replica", URL: "http://10.42.0.3:8000/", Role: "standby"},
	}

	// What the operator is actually looking at, and what to put there. The
	// message ends up on the DHCP page and in the log; an instance id in it
	// names nothing anyone can act on.
	in.OwnListen = "0.0.0.0:5353"
	in.LocalAddrs = []netip.Prefix{
		netip.MustParsePrefix("127.0.0.1/8"), netip.MustParsePrefix("192.168.150.40/24"),
	}
	_, err := dhcp.Render(in)
	if !errors.Is(err, dhcp.ErrNoPeerHost) {
		t.Fatalf("Render error = %v, want ErrNoPeerHost", err)
	}
	const want = "HA pair needs a host in this box's dns_listen (it is 0.0.0.0:5353); " +
		"set one such as 192.168.150.40:5353"
	if err.Error() != want {
		t.Errorf("error is\n  %q\nwant\n  %q", err.Error(), want)
	}
	// Nothing to suggest is still a message that names the setting and what
	// is in it — never a bare instance id.
	bare := in
	bare.LocalAddrs = nil
	if _, err := dhcp.Render(bare); err == nil {
		t.Fatal("a host-less peer rendered")
	} else if got := err.Error(); got != "HA pair needs a host in this box's dns_listen (it is 0.0.0.0:5353)" {
		t.Errorf("with no address to suggest the error is %q", got)
	}
	// The other box's entry is checked too, or half a pair renders.
	in.Peers[0].URL, in.Peers[1].URL = "http://10.42.0.2:8000/", "http://:8000/"
	if _, err := dhcp.Render(in); !errors.Is(err, dhcp.ErrNoPeerHost) {
		t.Fatalf("a host-less standby entry rendered: %v", err)
	}
	// And a pair that names both hosts still renders.
	in.Peers[1].URL = "http://10.42.0.3:8000/"
	if _, err := dhcp.Render(in); err != nil {
		t.Fatalf("a complete pair would not render: %v", err)
	}
}

// The socket-name rendered back is the one the engine's own configuration
// carries when the manager learned it, not the path this box dials: across
// a bind mount they differ, and Kea refuses to move its socket.
func TestRenderKeepsTheSocketWhereTheEngineHasIt(t *testing.T) {
	in := baseInput()
	in.Socket = "/srv/kea-host/kea.sock"
	in.EngineSocket = "/run/kea/kea.sock"
	cfg, err := dhcp.Render(in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	one, _ := cfg["control-socket"].(map[string]any)
	if got := one["socket-name"]; got != "/run/kea/kea.sock" {
		t.Errorf("socket-name = %v, want the engine's own /run/kea/kea.sock", got)
	}
}
