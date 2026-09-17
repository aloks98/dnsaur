package app

import (
	"encoding/json"
	"net"
	"net/http"
	"net/netip"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aloks98/dnsaur/internal/api"
	"github.com/aloks98/dnsaur/internal/config"
	"github.com/aloks98/dnsaur/internal/dhcp"
	"github.com/aloks98/dnsaur/internal/dhcp/keatest"
	"github.com/aloks98/dnsaur/internal/store"
)

// DHCP end to end: a main and a replica, both real Apps with a real engine
// socket each, in one process. The pieces Tasks 1-5 built are all testable on
// their own, and none of them can catch the thing this file is for — the two
// boxes disagreeing about the pair they are in. The HA hook needs both Keas
// to be given the same peer list under the same names, and each dnsaur
// arrives at that list from a different direction: the main from its registry
// of replicas, the replica from a setting the main wrote and a bundle carried.
// A mismatch is two Keas that never find each other, with both configs
// perfectly valid.
//
// Spec: docs/superpowers/specs/2026-09-13-dhcp-design.md — §5.1 (when to
// render), §5.3 (the DNS list), §6 (the pair from the pairing), §7.1 (leases
// are read from the engine and never copied between boxes).

// keaFake is a fake kea-dhcp4 on a unix socket: it answers what the manager
// asks and records every Dhcp4 object it was given.
type keaFake struct {
	srv *keatest.Server

	mu      sync.Mutex
	leases  []dhcp.Lease
	configs []map[string]any
	reject  string
}

func newKeaFake(t *testing.T) *keaFake {
	t.Helper()
	f := &keaFake{srv: keatest.NewServer(t)}
	f.srv.Handle("version-get", func(json.RawMessage) dhcp.Response {
		return dhcp.Response{Text: "2.6.3"}
	})
	f.srv.Handle("config-get", func(json.RawMessage) dhcp.Response {
		return dhcp.Response{Arguments: json.RawMessage(
			`{"Dhcp4":{"hooks-libraries":[{"library":"/usr/lib/kea/hooks/libdhcp_lease_cmds.so"}]}}`)}
	})
	f.srv.Handle("config-set", func(args json.RawMessage) dhcp.Response {
		var body struct {
			Dhcp4 map[string]any `json:"Dhcp4"`
		}
		if err := json.Unmarshal(args, &body); err != nil {
			return dhcp.Response{Result: 1, Text: err.Error()}
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.reject != "" {
			// Kea's own shape for a config it will not run: it answered,
			// with a no, and it stays on the configuration it had.
			return dhcp.Response{Result: 1, Text: f.reject}
		}
		f.configs = append(f.configs, body.Dhcp4)
		return dhcp.Response{Text: "Configuration successful."}
	})
	f.srv.Handle("lease4-get-page", func(json.RawMessage) dhcp.Response {
		f.mu.Lock()
		defer f.mu.Unlock()
		page, err := json.Marshal(map[string]any{"leases": f.leases})
		if err != nil {
			return dhcp.Response{Result: 1, Text: err.Error()}
		}
		return dhcp.Response{Arguments: page}
	})
	f.srv.Handle("status-get", func(json.RawMessage) dhcp.Response {
		return dhcp.Response{Arguments: json.RawMessage(`{"uptime": 42}`)}
	})
	return f
}

func (f *keaFake) serve(leases ...dhcp.Lease) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.leases = leases
}

// last is the configuration this engine is running, and fails the test if it
// has never been given one.
func (f *keaFake) last(t *testing.T) map[string]any {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.configs) == 0 {
		t.Fatal("the engine was never sent a configuration")
	}
	return f.configs[len(f.configs)-1]
}

// refuse makes every config-set from here on come back with why, the way
// an engine that cannot load a hook library or bind an interface does.
// Passing "" accepts again.
func (f *keaFake) refuse(why string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reject = why
}

func (f *keaFake) renders() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.configs)
}

// dhcpBox is a config bound to host with an engine socket of its own.
func dhcpBox(t *testing.T, host string) (*config.Config, *keaFake) {
	t.Helper()
	fake := newKeaFake(t)
	cfg := boxConfig(t, host)
	cfg.KeaSocket = fake.srv.Socket()
	return cfg, fake
}

// dhcpStarted waits for the manager's first discovery, render and poll.
// App.Start does not: a DNS listener must not wait on an engine that is
// down, so the start runs on its own goroutine and this is the gate a test
// asserting about that first render has to pass through.
func dhcpStarted(t *testing.T, a *App) {
	t.Helper()
	select {
	case <-a.dhcpReady:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the dhcp engine manager to start")
	}
}

// haOf is the high-availability block of a rendered configuration, and nil
// when the config carries no HA hook at all — which is what a box with no
// partner renders, and the difference this test turns on.
func haOf(t *testing.T, cfg map[string]any) map[string]any {
	t.Helper()
	hooks, _ := cfg["hooks-libraries"].([]any)
	for _, h := range hooks {
		hook, _ := h.(map[string]any)
		lib, _ := hook["library"].(string)
		if filepath.Base(lib) != "libdhcp_ha.so" {
			continue
		}
		params, _ := hook["parameters"].(map[string]any)
		relationships, _ := params["high-availability"].([]any)
		if len(relationships) != 1 {
			t.Fatalf("the ha hook carries %d relationships, want one hot-standby pair", len(relationships))
		}
		ha, _ := relationships[0].(map[string]any)
		return ha
	}
	return nil
}

// TestDHCPPairRendersFromThePairing is §6 end to end: one pairing, and both
// boxes render the same two peers under the same names, each naming itself.
func TestDHCPPairRendersFromThePairing(t *testing.T) {
	ctx := t.Context()
	pub := mockDNS(t, answerA("9.9.9.9"))

	mainCfg, mainKea := dhcpBox(t, "127.0.0.1")
	main := newTestAppWith(t, mainCfg, withUpstreams(pub),
		withSetting("dhcp.domain", "home.lan"))
	replicaCfg, replicaKea := dhcpBox(t, "127.0.0.2")
	// An hour between the replica's own config pulls, so its background loop
	// can overlap the pulls this test drives at most once.
	replica := newTestAppWith(t, replicaCfg, withUpstreams(pub),
		withSetting("sync.interval_seconds", "3600"))

	dhcpStarted(t, main)
	dhcpStarted(t, replica)
	mainID := mustGetSetting(t, main, "instance.id")
	replicaID := mustGetSetting(t, replica, "instance.id")

	// Before the pairing: a single box runs plain Kea. Rendered at start,
	// because that is when Manager.Start renders (§5.1).
	if ha := haOf(t, mainKea.last(t)); ha != nil {
		t.Fatalf("an unpaired main rendered an HA section: %v", ha)
	}

	// --- the pairing, over the real listeners, exactly as the Sync band does
	// it. The DNS override goes in first: it is what the replica renders the
	// main's peer URL from, and the fixture's peer URL is an HTTP port.
	mustSetMany(t, replica, map[string]string{"sync.primary_dns": main.DNSAddr()})
	mustFollow(t, replica, main)
	token := writeAPIToken(t, main)

	// --- a scope on the main, through the API, which is what renders (§5.1).
	// The choice of standby is made while that render's input is read, and
	// published once the engine has taken a configuration carrying it (§6).
	code, body := apiPost(t, httpURL(t, main)+"/api/v1/dhcp/scopes", token,
		`{"name":"lan","cidr":"192.168.7.0/24","pool_start":"192.168.7.100",`+
			`"pool_end":"192.168.7.200","gateway":"192.168.7.1","enabled":true}`)
	if code != http.StatusCreated {
		t.Fatalf("POST /dhcp/scopes on the main = %d %q, want 201", code, body)
	}
	var scope struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal([]byte(body), &scope); err != nil || scope.ID == 0 {
		t.Fatalf("POST /dhcp/scopes answered %q: %v", body, err)
	}
	if got := mustGetSetting(t, main, "dhcp.ha_standby"); got != replicaID {
		t.Fatalf("the main chose standby %q, want its one registered replica %q", got, replicaID)
	}
	if got := mustGetSetting(t, main, "dhcp.ha_primary"); got != mainID {
		t.Fatalf("the main recorded primary %q, want its own instance.id %q", got, mainID)
	}

	// --- the replica applies the bundle those writes moved, which is the
	// other trigger of §5.1.
	mustPull(t, replica)
	if got := mustGetSetting(t, replica, "dhcp.ha_standby"); got != replicaID {
		t.Fatalf("the replica reads standby %q, want its own id %q — the setting did not travel", got, replicaID)
	}

	mainHA, replicaHA := haOf(t, mainKea.last(t)), haOf(t, replicaKea.last(t))
	if mainHA == nil || replicaHA == nil {
		t.Fatalf("one box rendered no HA section: main=%v replica=%v", mainHA, replicaHA)
	}
	// The hook's own requirement: the same peers, in the same order, under
	// the same names, on both boxes.
	if !reflect.DeepEqual(mainHA["peers"], replicaHA["peers"]) {
		t.Fatalf("the two boxes rendered different HA peers:\n  main:    %v\n  replica: %v",
			mainHA["peers"], replicaHA["peers"])
	}
	// And the one thing that must differ, or both Keas believe they are the
	// same server.
	if mainHA["this-server-name"] != mainID || replicaHA["this-server-name"] != replicaID {
		t.Fatalf("this-server-name = %q on the main and %q on the replica, want %q and %q",
			mainHA["this-server-name"], replicaHA["this-server-name"], mainID, replicaID)
	}
	peers, _ := mainHA["peers"].([]any)
	if len(peers) != 2 {
		t.Fatalf("the pair has %d peers, want two", len(peers))
	}
	primary, _ := peers[0].(map[string]any)
	standby, _ := peers[1].(map[string]any)
	if primary["name"] != mainID || primary["role"] != "primary" {
		t.Errorf("first peer = %v, want the main as primary", primary)
	}
	if standby["name"] != replicaID || standby["role"] != "standby" {
		t.Errorf("second peer = %v, want the replica as standby", standby)
	}
	// The URLs are each box's own DNS host on dhcp.ha_port, and the two
	// boxes reach them from opposite directions: the main from its dns_listen
	// and its registry, the replica from sync.primary_dns and its own
	// dns_listen.
	if primary["url"] != "http://127.0.0.1:8000/" || standby["url"] != "http://127.0.0.2:8000/" {
		t.Errorf("peer urls = %q and %q, want each box's own DNS host on the ha port",
			primary["url"], standby["url"])
	}

	// §5.3: both boxes hand out the same two resolvers in the same order,
	// which is what DHCP-level failover needs from DNS.
	want := "127.0.0.1, 127.0.0.2"
	if got := dnsServersOf(t, mainKea.last(t)); got != want {
		t.Errorf("the main hands out %q, want %q", got, want)
	}
	if got := dnsServersOf(t, replicaKea.last(t)); got != want {
		t.Errorf("the replica hands out %q, want %q", got, want)
	}
	// And the scope itself travelled, or the replica is a DHCP server with
	// nothing to serve.
	if subnets, _ := replicaKea.last(t)["subnet4"].([]any); len(subnets) != 1 {
		t.Fatalf("the replica rendered %d subnets, want the main's one", len(subnets))
	}

	// --- §7.1: dnsaur never copies leases between boxes. Kea's HA does that,
	// and there is no Kea here — so a lease the main's engine hands out is
	// invisible on the replica until its own engine serves it.
	mainKea.serve(dhcp.Lease{
		IP: "192.168.7.120", MAC: "aa:bb:cc:dd:ee:11", Hostname: "nas",
		SubnetID: scope.ID, CLTT: 4_000_000_000, ValidLft: 3600,
	})
	if err := main.dhcp.Poll(ctx); err != nil {
		t.Fatalf("polling the main's engine: %v", err)
	}
	if _, held := main.dhcp.Table().ByIP(netip.MustParseAddr("192.168.7.120")); !held {
		t.Fatal("the main's table does not hold the lease its own engine served")
	}
	mustPull(t, replica)
	if err := replica.dhcp.Poll(ctx); err != nil {
		t.Fatalf("polling the replica's engine: %v", err)
	}
	if _, held := replica.dhcp.Table().ByIP(netip.MustParseAddr("192.168.7.120")); held {
		t.Fatal("the replica holds a lease its own engine never served; leases are not synced by dnsaur")
	}
	replicaKea.serve(dhcp.Lease{
		IP: "192.168.7.120", MAC: "aa:bb:cc:dd:ee:11", Hostname: "nas",
		SubnetID: scope.ID, CLTT: 4_000_000_000, ValidLft: 3600,
	})
	if err := replica.dhcp.Poll(ctx); err != nil {
		t.Fatalf("polling the replica's engine: %v", err)
	}
	if _, held := replica.dhcp.Table().ByIP(netip.MustParseAddr("192.168.7.120")); !held {
		t.Fatal("the replica's own poll did not pick the lease up")
	}

	// --- §10: a replica that is forgotten drops off the pair on the next
	// render, on both boxes.
	code, body = apiSend(t, http.MethodDelete,
		httpURL(t, main)+"/api/v1/sync/replicas/"+replicaID, token, "")
	if code != http.StatusNoContent {
		t.Fatalf("DELETE /sync/replicas/%s = %d %q, want 204", replicaID, code, body)
	}
	// No write follows, and none would: Forget is a registry write, which
	// moves no config_version by design. The lease poll is what notices
	// (§10), on both boxes.
	if err := main.dhcp.Poll(ctx); err != nil {
		t.Fatalf("polling the main after the replica was forgotten: %v", err)
	}
	if ha := haOf(t, mainKea.last(t)); ha != nil {
		t.Fatalf("the main still renders an HA section after forgetting its only replica: %v", ha)
	}
	if got := mustGetSetting(t, main, "dhcp.ha_standby"); got != "" {
		t.Fatalf("dhcp.ha_standby is %q after the replica was forgotten, want it cleared", got)
	}
	// The other box is not told, and cannot be: Forget revokes the secret
	// with the entry, so the replica's next pull is a 401 and the cleared
	// setting never reaches it. It goes on rendering a pair whose partner has
	// dropped it, which Kea reports as communication-interrupted until the
	// operator pairs it again or promotes it. That is config sync's rule (§6
	// of its own design), not something this render can work around.
	if err := replica.replica.PullOnce(ctx); err == nil {
		t.Fatal("a forgotten replica pulled; the premise of the line above is gone")
	}
	if got := dnsServersOf(t, mainKea.last(t)); got != "127.0.0.1" {
		t.Errorf("the unpaired main hands out %q, want its own address alone", got)
	}

	// What the replica can do for itself: a pull the main refused the token
	// on is this box having been removed from the main's configuration, not
	// a network hiccup, so its next render drops the pair rather than
	// heartbeating a partner that has dropped it (§10).
	if err := replica.dhcp.Poll(ctx); err != nil {
		t.Fatalf("polling the forgotten replica: %v", err)
	}
	if ha := haOf(t, replicaKea.last(t)); ha != nil {
		t.Fatalf("a replica whose pull was refused still renders an HA section: %v", ha)
	}
	if got := dnsServersOf(t, replicaKea.last(t)); got != "127.0.0.2" {
		t.Errorf("the forgotten replica hands out %q, want its own address alone", got)
	}
	// And it is the refusal that does it, not a one-way door: a pull that
	// works again — the operator paired the box back — clears sync.last_error,
	// and the next render is the pair again.
	if err := replica.Store().Settings().SetInternal(ctx, "sync.last_error", ""); err != nil {
		t.Fatalf("clearing sync.last_error: %v", err)
	}
	if err := replica.dhcp.Poll(ctx); err != nil {
		t.Fatalf("polling the replica after a pull that worked: %v", err)
	}
	if ha := haOf(t, replicaKea.last(t)); ha == nil {
		t.Error("a replica whose last pull worked renders no HA section; the pair does not come back")
	}
}

// TestDHCPRendersOnlyWhatChanged is §5.1's gate, driven through the real
// settings watcher. Every configuration write wakes that watcher, and most
// of them are nothing DHCP has an opinion about — a blocking pause is a
// settings row, and so is a retention day count. A config-set for each of
// them would have Kea rebuild every subnet and re-read its lease file to
// arrive at exactly what it was already running.
func TestDHCPRendersOnlyWhatChanged(t *testing.T) {
	pub := mockDNS(t, answerA("127.0.0.1"))
	cfg, kea := dhcpBox(t, "127.0.0.1")
	// The scope is in the store before the box starts, so the render Start
	// makes is the one the engine is holding and the hash beside it describes
	// that configuration rather than nothing.
	a := newTestAppWith(t, cfg, withUpstreams(pub), withScope(t, store.Scope{
		Name: "lan", CIDR: "192.168.7.0/24",
		PoolStart: "192.168.7.100", PoolEnd: "192.168.7.200",
		Enabled: true, MatchClientID: true,
	}))
	dhcpStarted(t, a)
	if got := kea.renders(); got != 1 {
		t.Fatalf("the engine was sent %d configurations at start, want one", got)
	}

	// write makes a settings write and waits for the watcher's pass over it,
	// which is what lets the assertion after it be about a pass that has
	// happened rather than about one that has not started.
	write := func(what string, values map[string]string) {
		t.Helper()
		before := a.settingsPasses.Load()
		mustSetMany(t, a, values)
		waitFor(t, what, func() bool { return a.settingsPasses.Load() > before })
	}

	write("a settings write DHCP has no opinion about", map[string]string{"blocking.mode": "nxdomain"})
	if got := kea.renders(); got != 1 {
		t.Fatalf("an unrelated settings write sent the engine %d more configurations, want none", got-1)
	}

	write("a settings write that changes what the engine is handed", map[string]string{"dhcp.domain": "home.lan"})
	if got := kea.renders(); got != 2 {
		t.Fatalf("dhcp.domain left the engine on %d configurations in total, want two", got)
	}
	if got := domainOf(t, kea.last(t)); got != "home.lan" {
		t.Fatalf("the engine hands out domain %q, want the one that was just set", got)
	}

	// The same value again is not a change, whatever else moved the version.
	write("a write that re-states what DHCP already renders",
		map[string]string{"dhcp.domain": "home.lan", "blocking.ttl": "45"})
	if got := kea.renders(); got != 2 {
		t.Fatalf("re-writing the same dhcp.domain sent the engine %d configurations, want two", got)
	}
	// And a local one that does reach the render.
	write("a serve.dhcp_interfaces write", map[string]string{"serve.dhcp_interfaces": "eth0"})
	if got := kea.renders(); got != 3 {
		t.Fatalf("serve.dhcp_interfaces left the engine on %d configurations, want three", got)
	}
}

// TestDHCPStandbyIsTheSmallestNonStaleReplica pins §6's choice, which has to
// be made the same way by a box that has just restarted as by one that has
// been up for a month: the lexicographically smallest instance.id among the
// replicas that are not stale. A timestamp would not do — it is not a
// tie-break two boxes could agree on — and a stale box would leave the
// primary waiting out max-response-delay on every client before serving it.
func TestDHCPStandbyIsTheSmallestNonStaleReplica(t *testing.T) {
	ctx := t.Context()
	cfg, _ := dhcpBox(t, "127.0.0.1")
	a := newTestAppWith(t, cfg, withUpstreams(mockDNS(t, answerA("9.9.9.9"))))
	dhcpStarted(t, a)

	// Three registered boxes. The smallest id of the three is stale, so the
	// choice is the smaller of the two that are not — which no "first
	// registered" or "last heard from" rule would land on.
	fresh := time.Now().UnixMilli()
	stale := time.Now().Add(-10 * time.Minute).UnixMilli()
	mustRegister(t, a, map[string]api.Replica{
		"AAA": {DNSAddr: "10.0.0.2:53", LastSeen: stale, DHCP: true},
		"BBB": {DNSAddr: "10.0.0.3:53", LastSeen: fresh, DHCP: true},
		"CCC": {DNSAddr: "10.0.0.4:53", LastSeen: fresh, DHCP: true},
	})

	in, err := a.RenderInput(ctx)
	if err != nil {
		t.Fatalf("RenderInput: %v", err)
	}
	if len(in.Peers) != 2 {
		t.Fatalf("rendered %d peers, want the pair", len(in.Peers))
	}
	if got := in.Peers[1]; got.Name != "BBB" || got.URL != "http://10.0.0.3:8000/" || got.Role != "standby" {
		t.Fatalf("standby = %+v, want BBB at its own dns_addr host", got)
	}
	// Published by the render, not by the read above: the two settings say
	// "these two boxes are paired", and only an engine that took the
	// configuration makes that true (§6).
	if err := a.dhcp.Apply(ctx); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := mustGetSetting(t, a, "dhcp.ha_standby"); got != "BBB" {
		t.Fatalf("dhcp.ha_standby = %q, want BBB — the choice has to travel in the bundle", got)
	}
	// §5.3: the standby's address is the second resolver every scope hands
	// out, so the same choice decides that list too.
	if want := []string{"127.0.0.1", "10.0.0.3"}; !reflect.DeepEqual(in.DNSServers, want) {
		t.Fatalf("dns servers = %v, want %v", in.DNSServers, want)
	}

	// Every replica gone stale is a box with no partner, and both halves of
	// the recorded pair are cleared together.
	mustRegister(t, a, map[string]api.Replica{"BBB": {DNSAddr: "10.0.0.3:53", LastSeen: stale, DHCP: true}})
	in, err = a.RenderInput(ctx)
	if err != nil {
		t.Fatalf("RenderInput: %v", err)
	}
	if len(in.Peers) != 0 {
		t.Fatalf("rendered %+v with nothing but stale replicas, want no pair", in.Peers)
	}
	if err := a.dhcp.Apply(ctx); err != nil {
		t.Fatalf("Apply with no partner left: %v", err)
	}
	if p, s := mustGetSetting(t, a, "dhcp.ha_primary"), mustGetSetting(t, a, "dhcp.ha_standby"); p != "" || s != "" {
		t.Fatalf("the recorded pair is {%q %q} with no partner left, want both cleared", p, s)
	}
}

// TestDHCPRefusesAPairWithNoAddressToPairOn is §6's trap, and a box falls
// into it by changing nothing: dns_listen defaults to ":53", so a main that
// pairs has no address to put in its own HA peer URL. The renderer refuses
// the whole configuration rather than handing Kea "http://:8000/", and the
// message names dns_listen, which the engine's own refusal would not.
//
// Refused, not rendered without the pair: a second DHCP server on one segment
// with no HA between them hands the same pool out twice.
func TestDHCPRefusesAPairWithNoAddressToPairOn(t *testing.T) {
	ctx := t.Context()
	cfg, kea := dhcpBox(t, "127.0.0.1")
	_, port, err := net.SplitHostPort(cfg.DNSListen[0])
	if err != nil {
		t.Fatal(err)
	}
	// The default shape: a port and every interface.
	cfg.DNSListen = []string{":" + port}
	// The scope names its own resolvers, so the only thing missing from this
	// render is the peer's host — §5.3's own dead end is a different test.
	a := newTestAppWith(t, cfg, withUpstreams(mockDNS(t, answerA("9.9.9.9"))), withScope(t, store.Scope{
		Name: "lan", CIDR: "192.168.7.0/24",
		PoolStart: "192.168.7.100", PoolEnd: "192.168.7.200",
		DNSServers: "192.168.7.1", Enabled: true, MatchClientID: true,
	}))
	dhcpStarted(t, a)
	// Unpaired it renders fine, which is what makes the refusal below about
	// the pair and not about the scope.
	if got := kea.renders(); got != 1 {
		t.Fatalf("an unpaired box on a wildcard listener sent %d configurations, want one", got)
	}

	mustRegister(t, a, map[string]api.Replica{
		"BBB": {DNSAddr: "10.0.0.3:53", LastSeen: time.Now().UnixMilli(), DHCP: true},
	})
	if err := a.dhcp.Apply(ctx); err == nil {
		t.Fatal("a pair with no address to pair on rendered")
	}
	status := a.dhcp.Status()
	if status.Engine != "config rejected" {
		t.Fatalf("engine = %q (%q), want config rejected", status.Engine, status.Message)
	}
	// The message is what the operator acts on, so it names the setting, and
	// what that setting says now, and an address on this machine that would
	// do instead. Never a peer's name: those are instance ids.
	if !strings.Contains(status.Message, "dns_listen") {
		t.Errorf("message = %q, want it to name the setting to fix", status.Message)
	}
	if !strings.Contains(status.Message, cfg.DNSListen[0]) {
		t.Errorf("message = %q, want it to quote dns_listen's current value %q",
			status.Message, cfg.DNSListen[0])
	}
	if !strings.Contains(status.Message, "set one such as ") {
		t.Errorf("message = %q, want it to name an address this box holds", status.Message)
	}
	if id := mustGetSetting(t, a, "instance.id"); strings.Contains(status.Message, id) {
		t.Errorf("message = %q names an instance id, which is not something anyone can act on", status.Message)
	}
	if got := kea.renders(); got != 1 {
		t.Fatalf("the engine was sent %d configurations, want the one from before the pairing — "+
			"a configuration that cannot be built must never reach it", got)
	}
	// And nothing was published for the replica to read: a standby told it is
	// in a pair whose primary could not build a configuration for it is worse
	// off than one told nothing (§6).
	if p, s := mustGetSetting(t, a, "dhcp.ha_primary"), mustGetSetting(t, a, "dhcp.ha_standby"); p != "" || s != "" {
		t.Errorf("the pair was published as {%q %q} from a render that was refused, want both empty", p, s)
	}
}

// TestDHCPStartDoesNotHoldUpTheListeners: Manager.Start is three round trips
// to a socket that may not be there, and an engine that accepts a connection
// and then says nothing costs each of them the client's whole timeout. None
// of that may stand between the process starting and DNS being answered —
// the resolver is what the LAN cannot do without, and DHCP is the part that
// degrades quietly (§10).
func TestDHCPStartDoesNotHoldUpTheListeners(t *testing.T) {
	cfg, kea := dhcpBox(t, "127.0.0.1")
	// An engine that reads the command and never answers, which is the one
	// failure a dial timeout does not cover.
	kea.srv.Hang()
	pub := mockDNS(t, answerA("9.9.9.9"))

	start := time.Now()
	a := newTestAppWith(t, cfg, withUpstreams(pub))
	if got := digA(t, a.DNSAddr(), "example.org"); got != "9.9.9.9" {
		t.Fatalf("the listener answers %q, want the upstream's address", got)
	}
	// The client's own timeout is five seconds per exchange; anything near
	// that means the listener waited for one.
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("starting and answering took %s with a hung engine; the listeners waited on it", elapsed)
	}
}

// TestDHCPReplicaNeedsAnAddressForItsMain is the other half of the hostname
// rule: a peer URL is a name an operator typed for a browser, and it goes
// into a peer URL Kea dials and an option 6 list clients are handed, neither
// of which takes one. sync.primary_dns is the answer, and until it is set the
// box renders no pair rather than one built on a name.
func TestDHCPReplicaNeedsAnAddressForItsMain(t *testing.T) {
	ctx := t.Context()
	cfg, _ := dhcpBox(t, "127.0.0.2")
	a := newTestAppWith(t, cfg, withUpstreams(mockDNS(t, answerA("9.9.9.9"))),
		withSetting("sync.peer_url", "http://main.lan"),
		withSetting("sync.token", "s3cr3t"))
	dhcpStarted(t, a)
	// The main named this box as its standby, and the choice travelled.
	id := mustGetSetting(t, a, "instance.id")
	mustSetMany(t, a, map[string]string{"dhcp.ha_primary": "the-main", "dhcp.ha_standby": id})

	in, err := a.RenderInput(ctx)
	if err != nil {
		t.Fatalf("RenderInput: %v", err)
	}
	if len(in.Peers) != 0 {
		t.Fatalf("rendered %+v from a peer url that names a host, want no pair", in.Peers)
	}
	// And with an address for it, the pair renders — so the refusal above is
	// about the name and not about the pairing.
	mustSetMany(t, a, map[string]string{"sync.primary_dns": "10.0.0.1:53"})
	in, err = a.RenderInput(ctx)
	if err != nil {
		t.Fatalf("RenderInput: %v", err)
	}
	if len(in.Peers) != 2 || in.Peers[0].URL != "http://10.0.0.1:8000/" {
		t.Fatalf("peers = %+v, want the pair on the override's address", in.Peers)
	}
	if want := []string{"10.0.0.1", "127.0.0.2"}; !reflect.DeepEqual(in.DNSServers, want) {
		t.Fatalf("dns servers = %v, want %v", in.DNSServers, want)
	}
}

// mustRegister writes the main's replica registry by hand. Pairing is the
// only way in through the front door and it stamps last_seen as now, so a
// stale entry — which is half of what §6's choice is about — cannot be made
// any other way. DHCP is the other half and is stamped from the version
// probe, so an entry meant to be eligible has to say so here.
func mustRegister(t *testing.T, a *App, replicas map[string]api.Replica) {
	t.Helper()
	raw, err := json.Marshal(replicas)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Store().Settings().SetInternal(t.Context(), "sync.replicas", string(raw)); err != nil {
		t.Fatalf("writing the replica registry: %v", err)
	}
}

// withScope puts a scope in the store before the box starts, so the first
// render has something in it.
func withScope(t *testing.T, sc store.Scope) testAppOption {
	t.Helper()
	return func(a *App, _ map[string]string) {
		if _, err := a.Store().DHCP().AddScope(t.Context(), sc); err != nil {
			t.Fatalf("AddScope(%s): %v", sc.Name, err)
		}
	}
}

// optionOf reads one rendered option out of the first subnet.
func optionOf(t *testing.T, cfg map[string]any, name string) string {
	t.Helper()
	subnets, _ := cfg["subnet4"].([]any)
	if len(subnets) == 0 {
		t.Fatalf("the configuration carries no subnets: %v", cfg)
	}
	subnet, _ := subnets[0].(map[string]any)
	options, _ := subnet["option-data"].([]any)
	for _, o := range options {
		option, _ := o.(map[string]any)
		if option["name"] == name {
			data, _ := option["data"].(string)
			return data
		}
	}
	return ""
}

func dnsServersOf(t *testing.T, cfg map[string]any) string {
	t.Helper()
	return optionOf(t, cfg, "domain-name-servers")
}

func domainOf(t *testing.T, cfg map[string]any) string {
	t.Helper()
	return optionOf(t, cfg, "domain-name")
}

// TestDHCPWriteDoesNotWaitOutAHungEngine: the write has committed by the
// time the render is sent, and the answer never depends on what the engine
// says about it (§5.1). So the render is the one thing that must not hold
// the request open — an engine that accepts a connection and then says
// nothing costs the whole ceiling, and forty seconds of it is a dashboard
// that looks broken while the scope it is asking about is already stored.
func TestDHCPWriteDoesNotWaitOutAHungEngine(t *testing.T) {
	cfg, kea := dhcpBox(t, "127.0.0.1")
	a := newTestAppWith(t, cfg, withUpstreams(mockDNS(t, answerA("9.9.9.9"))))
	dhcpStarted(t, a)
	// From here the engine reads every command and never answers, which is
	// the failure a dial timeout does not cover.
	kea.srv.Hang()
	token := writeAPIToken(t, a)

	start := time.Now()
	code, body := apiPost(t, httpURL(t, a)+"/api/v1/dhcp/scopes", token,
		`{"name":"lan","cidr":"192.168.7.0/24","pool_start":"192.168.7.100",`+
			`"pool_end":"192.168.7.200","dns_servers":"192.168.7.1","enabled":true}`)
	elapsed := time.Since(start)
	if code != http.StatusCreated {
		t.Fatalf("POST /dhcp/scopes = %d %q, want 201 — the row is stored whatever the engine says", code, body)
	}
	if elapsed > 20*time.Second {
		t.Fatalf("the write took %s against a hung engine; the api's wait on the render is unbounded", elapsed)
	}
	// And the state is on the dashboard, where an operator acts on it.
	if got := a.dhcp.Status().Engine; got == "ok" {
		t.Errorf("engine = %q after a render the engine never answered, want unreachable or config rejected", got)
	}
}

// TestDHCPPublishesThePairOnlyFromAnAcceptedRender is §6's ordering, and the
// one thing dhcp.ha_standby means: the standby is in a pair whose primary is
// serving it. The choice used to be written while the input was still being
// read — before anything had tried to build a configuration out of it, let
// alone send one — so a main whose own render was refused had already told
// its replica they were paired, and Kea on the replica spent its time
// heartbeating a partner that was running a configuration without it.
func TestDHCPPublishesThePairOnlyFromAnAcceptedRender(t *testing.T) {
	ctx := t.Context()
	cfg, kea := dhcpBox(t, "127.0.0.1")
	a := newTestAppWith(t, cfg, withUpstreams(mockDNS(t, answerA("9.9.9.9"))), withScope(t, store.Scope{
		Name: "lan", CIDR: "192.168.7.0/24",
		PoolStart: "192.168.7.100", PoolEnd: "192.168.7.200",
		DNSServers: "192.168.7.1", Enabled: true, MatchClientID: true,
	}))
	dhcpStarted(t, a)
	mustRegister(t, a, map[string]api.Replica{
		"BBB": {DNSAddr: "10.0.0.3:53", LastSeen: time.Now().UnixMilli(), DHCP: true},
	})

	kea.refuse("hooks-libraries: cannot open '/usr/lib/kea/hooks/libdhcp_ha.so'")
	if err := a.dhcp.Apply(ctx); err == nil {
		t.Fatal("the engine refused the configuration and Apply reported success")
	}
	if status := a.dhcp.Status(); status.Engine != "config rejected" {
		t.Fatalf("engine = %q (%q), want config rejected", status.Engine, status.Message)
	}
	if p, s := mustGetSetting(t, a, "dhcp.ha_primary"), mustGetSetting(t, a, "dhcp.ha_standby"); p != "" || s != "" {
		t.Fatalf("the pair was published as {%q %q} from a render the engine refused, want both empty — "+
			"a standby reading this would pair with an engine running a configuration without it", p, s)
	}

	// And once the engine takes it, the same choice is published and travels.
	kea.refuse("")
	if err := a.dhcp.Apply(ctx); err != nil {
		t.Fatalf("Apply against an engine that accepts: %v", err)
	}
	if got := mustGetSetting(t, a, "dhcp.ha_standby"); got != "BBB" {
		t.Errorf("dhcp.ha_standby = %q after an accepted render, want BBB", got)
	}
	if got, want := mustGetSetting(t, a, "dhcp.ha_primary"), mustGetSetting(t, a, "instance.id"); got != want {
		t.Errorf("dhcp.ha_primary = %q after an accepted render, want this box's own id %q", got, want)
	}
	if ha := haOf(t, kea.last(t)); ha == nil {
		t.Error("the accepted configuration carries no HA section")
	}
}

// TestDHCPPairConvergesOnTheLeasePoll is §6 and §10 together: a replica that
// pairs, is forgotten or goes stale changes the pair on both boxes, and none
// of those three writes a setting. The registry is deliberately written
// without moving config_version — a registration arrives from every replica
// every interval, and bumping the version for each would have every box
// fetch a bundle every interval — so the settings watcher never wakes, no
// handler runs, and nothing else would ever notice. The lease poll is what
// notices: it already reads the render input to build its table, so the
// render is one comparison away.
//
// Without it the pair converges only when an operator makes an unrelated
// write or presses Apply again, which is a pair that exists on one box.
func TestDHCPPairConvergesOnTheLeasePoll(t *testing.T) {
	ctx := t.Context()
	pub := mockDNS(t, answerA("9.9.9.9"))
	cfg, kea := dhcpBox(t, "127.0.0.1")
	a := newTestAppWith(t, cfg, withUpstreams(pub), withScope(t, store.Scope{
		Name: "lan", CIDR: "192.168.7.0/24",
		PoolStart: "192.168.7.100", PoolEnd: "192.168.7.200",
		DNSServers: "192.168.7.1", Enabled: true, MatchClientID: true,
	}))
	dhcpStarted(t, a)
	if got := kea.renders(); got != 1 {
		t.Fatalf("the engine was sent %d configurations at start, want one", got)
	}

	// A replica registers. Nothing else happens on this box — no settings
	// write, no API call, no bundle.
	mustRegister(t, a, map[string]api.Replica{
		"BBB": {DNSAddr: "10.0.0.3:53", LastSeen: time.Now().UnixMilli(), DHCP: true},
	})
	if err := a.dhcp.Poll(ctx); err != nil {
		t.Fatalf("Poll after the pairing: %v", err)
	}
	ha := haOf(t, kea.last(t))
	if ha == nil {
		t.Fatal("the poll after a replica paired sent no HA section; the pair never reaches Kea")
	}
	peers, _ := ha["peers"].([]any)
	if len(peers) != 2 {
		t.Fatalf("the rendered pair has %d peers, want two", len(peers))
	}
	if standby, _ := peers[1].(map[string]any); standby["name"] != "BBB" {
		t.Errorf("the standby peer is %v, want the replica that just registered", standby)
	}
	if got := mustGetSetting(t, a, "dhcp.ha_standby"); got != "BBB" {
		t.Errorf("dhcp.ha_standby = %q after the poll rendered the pair, want BBB", got)
	}

	// And the same poll does not re-send what the engine is already running:
	// §5.1's gate is the whole reason the poll can be the convergence loop.
	before := kea.renders()
	if err := a.dhcp.Poll(ctx); err != nil {
		t.Fatalf("Poll with nothing changed: %v", err)
	}
	if got := kea.renders(); got != before {
		t.Fatalf("a poll with nothing changed sent %d more configurations, want none", got-before)
	}

	// Forgotten — here by going stale, which is the same change to the pair
	// and the one an operator never makes.
	mustRegister(t, a, map[string]api.Replica{
		"BBB": {DNSAddr: "10.0.0.3:53", LastSeen: time.Now().Add(-10 * time.Minute).UnixMilli(), DHCP: true},
	})
	if err := a.dhcp.Poll(ctx); err != nil {
		t.Fatalf("Poll after the replica went stale: %v", err)
	}
	if ha := haOf(t, kea.last(t)); ha != nil {
		t.Fatalf("the poll after the standby went stale still renders a pair: %v", ha)
	}
	if p, s := mustGetSetting(t, a, "dhcp.ha_primary"), mustGetSetting(t, a, "dhcp.ha_standby"); p != "" || s != "" {
		t.Errorf("the recorded pair is {%q %q} with no partner left, want both cleared", p, s)
	}
}

// TestDHCPStandbyMustRunAnEngine is §6's other requirement of a standby, and
// the one nothing on the main can see for itself: a replica is a box that
// follows this one's configuration, and whether it also runs a DHCP engine
// is a bootstrap key on that box. Named as the standby without one, Kea is
// handed a hot-standby pair whose partner answers nothing — and the primary
// waits out max-response-delay for it on every client before serving them
// itself, which is strictly worse than the single box it would otherwise be.
//
// The version probe is the only thing the main ever hears from a replica, so
// it is what carries the answer.
func TestDHCPStandbyMustRunAnEngine(t *testing.T) {
	ctx := t.Context()
	pub := mockDNS(t, answerA("9.9.9.9"))
	mainCfg, mainKea := dhcpBox(t, "127.0.0.1")
	main := newTestAppWith(t, mainCfg, withUpstreams(pub), withScope(t, store.Scope{
		Name: "lan", CIDR: "192.168.7.0/24",
		PoolStart: "192.168.7.100", PoolEnd: "192.168.7.200",
		DNSServers: "192.168.7.1", Enabled: true, MatchClientID: true,
	}))
	dhcpStarted(t, main)

	// A replica with no kea_socket: DHCP is off on that box entirely, which
	// is every install that does not run Kea.
	plain := newTestAppWith(t, boxConfig(t, "127.0.0.2"), withUpstreams(pub),
		withSetting("sync.interval_seconds", "3600"))
	mustSetMany(t, plain, map[string]string{"sync.primary_dns": main.DNSAddr()})
	mustFollow(t, plain, main)
	if err := main.dhcp.Poll(ctx); err != nil {
		t.Fatalf("polling the main after a replica with no engine paired: %v", err)
	}
	if ha := haOf(t, mainKea.last(t)); ha != nil {
		t.Fatalf("the main paired with a replica that runs no engine: %v", ha)
	}
	if p, s := mustGetSetting(t, main, "dhcp.ha_primary"), mustGetSetting(t, main, "dhcp.ha_standby"); p != "" || s != "" {
		t.Fatalf("the recorded pair is {%q %q} with no eligible replica, want both cleared", p, s)
	}

	// And a second replica that does run one. It is the standby whatever its
	// instance id sorts as, because it is the only candidate.
	engineCfg, _ := dhcpBox(t, "127.0.0.3")
	engine := newTestAppWith(t, engineCfg, withUpstreams(pub),
		withSetting("sync.interval_seconds", "3600"))
	dhcpStarted(t, engine)
	mustSetMany(t, engine, map[string]string{"sync.primary_dns": main.DNSAddr()})
	mustFollow(t, engine, main)
	engineID := mustGetSetting(t, engine, "instance.id")

	if err := main.dhcp.Poll(ctx); err != nil {
		t.Fatalf("polling the main after a replica with an engine paired: %v", err)
	}
	ha := haOf(t, mainKea.last(t))
	if ha == nil {
		t.Fatal("the main renders no pair with an eligible replica registered")
	}
	peers, _ := ha["peers"].([]any)
	if len(peers) != 2 {
		t.Fatalf("the rendered pair has %d peers, want two", len(peers))
	}
	if standby, _ := peers[1].(map[string]any); standby["name"] != engineID {
		t.Errorf("the standby peer is %v, want the one replica that runs an engine (%s)", standby, engineID)
	}
	if got := mustGetSetting(t, main, "dhcp.ha_standby"); got != engineID {
		t.Errorf("dhcp.ha_standby = %q, want the replica with an engine %q", got, engineID)
	}
}
