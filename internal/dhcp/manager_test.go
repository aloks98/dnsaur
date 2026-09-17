package dhcp_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aloks98/dnsaur/internal/dhcp"
	"github.com/aloks98/dnsaur/internal/dhcp/keatest"
	"github.com/aloks98/dnsaur/internal/store"
)

// The manager logs an engine that is not there, and two tests take one away
// deliberately. Nothing reads those lines, so they go nowhere rather than
// into the middle of a passing run.
func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.DiscardHandler))
	os.Exit(m.Run())
}

// fakeEngine is keatest's socket with the handlers a manager's start and poll
// need, plus a record of what each command carried. Handlers run on the
// server's own goroutine, so everything they touch is behind the mutex.
type fakeEngine struct {
	*keatest.Server

	mu       sync.Mutex
	reject   string
	pageFail string
	leases   []dhcp.Lease
	configs  []map[string]any
	limits   []int
	deleted  []string
	ha       *dhcp.HAStatus
}

// newEngine answers version-get with version and config-get with a
// configuration whose one hook library sits in hookDir, which is where the
// manager is meant to read the directory from.
func newEngine(t *testing.T, version, hookDir string) *fakeEngine {
	t.Helper()
	e := &fakeEngine{Server: keatest.NewServer(t)}

	e.Handle("version-get", func(json.RawMessage) dhcp.Response {
		return dhcp.Response{Text: version}
	})
	config := marshal(t, map[string]any{"Dhcp4": map[string]any{
		"hooks-libraries": []any{map[string]any{"library": hookDir + "/libdhcp_lease_cmds.so"}},
	}})
	e.Handle("config-get", func(json.RawMessage) dhcp.Response {
		return dhcp.Response{Arguments: config}
	})
	e.Handle("config-set", func(raw json.RawMessage) dhcp.Response {
		var in struct {
			Dhcp4 map[string]any `json:"Dhcp4"`
		}
		if err := json.Unmarshal(raw, &in); err != nil {
			return dhcp.Response{Result: 1, Text: err.Error()}
		}
		e.mu.Lock()
		defer e.mu.Unlock()
		if e.reject != "" {
			return dhcp.Response{Result: 1, Text: e.reject}
		}
		e.configs = append(e.configs, in.Dhcp4)
		return dhcp.Response{Text: "Configuration successful."}
	})
	e.Handle("lease4-get-page", e.page)
	e.Handle("lease4-del", func(raw json.RawMessage) dhcp.Response {
		var in struct {
			IP string `json:"ip-address"`
		}
		if err := json.Unmarshal(raw, &in); err != nil {
			return dhcp.Response{Result: 1, Text: err.Error()}
		}
		e.mu.Lock()
		defer e.mu.Unlock()
		e.deleted = append(e.deleted, in.IP)
		return dhcp.Response{Text: "IPv4 lease deleted."}
	})
	e.Handle("status-get", func(json.RawMessage) dhcp.Response {
		e.mu.Lock()
		ha := e.ha
		e.mu.Unlock()
		args := map[string]any{"uptime": 42}
		if ha != nil {
			args["high-availability"] = []any{map[string]any{
				"ha-mode": ha.Mode,
				"ha-servers": map[string]any{
					"local":  map[string]any{"state": ha.LocalState},
					"remote": map[string]any{"last-state": ha.RemoteState},
				},
			}}
		}
		b, err := json.Marshal(args)
		if err != nil {
			return dhcp.Response{Result: 1, Text: err.Error()}
		}
		return dhcp.Response{Arguments: b}
	})
	return e
}

// page is Kea's lease4-get-page: the leases after "from", at most limit of
// them, and its "nothing found" result for the page past the last lease.
func (e *fakeEngine) page(raw json.RawMessage) dhcp.Response {
	var in struct {
		From  string `json:"from"`
		Limit int    `json:"limit"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return dhcp.Response{Result: 1, Text: err.Error()}
	}
	e.mu.Lock()
	e.limits = append(e.limits, in.Limit)
	leases := slices.Clone(e.leases)
	fail := e.pageFail
	e.pageFail = ""
	e.mu.Unlock()

	if fail != "" {
		return dhcp.Response{Result: 1, Text: fail}
	}

	start := 0
	if in.From != "start" {
		at := slices.IndexFunc(leases, func(l dhcp.Lease) bool { return l.IP == in.From })
		if at < 0 {
			return dhcp.Response{Result: 3, Text: "0 IPv4 lease(s) found."}
		}
		start = at + 1
	}
	page := leases[min(start, len(leases)):]
	if len(page) > in.Limit {
		page = page[:in.Limit]
	}
	if len(page) == 0 {
		return dhcp.Response{Result: 3, Text: "0 IPv4 lease(s) found."}
	}
	b, err := json.Marshal(map[string]any{"count": len(page), "leases": page})
	if err != nil {
		return dhcp.Response{Result: 1, Text: err.Error()}
	}
	return dhcp.Response{Arguments: b}
}

func (e *fakeEngine) setLeases(ls ...dhcp.Lease) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.leases = ls
}

// failNextPage makes the next lease4-get-page, and only the next, come back
// refused: one command the engine would not run, not a configuration it is
// refusing to serve.
func (e *fakeEngine) failNextPage(text string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.pageFail = text
}

func (e *fakeEngine) setHA(ha *dhcp.HAStatus) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.ha = ha
}

func (e *fakeEngine) setReject(text string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.reject = text
}

func (e *fakeEngine) sent() []map[string]any {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.configs)
}

func (e *fakeEngine) pageLimits() []int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.limits)
}

func (e *fakeEngine) deletedLeases() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.deleted)
}

func marshal(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshalling %T: %v", v, err)
	}
	return b
}

// fakeInputs is the App of a later task reduced to the one method the manager
// asks of it.
type fakeInputs struct {
	mu       sync.Mutex
	in       dhcp.RenderInput
	err      error
	rendered []renderOutcome
}

func (f *fakeInputs) RenderInput(context.Context) (dhcp.RenderInput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.in, f.err
}

// Rendered records every outcome the manager reports, which is what the
// tests about publishing the pair and recording the hash assert on.
func (f *fakeInputs) Rendered(_ context.Context, in dhcp.RenderInput, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rendered = append(f.rendered, renderOutcome{in: in, err: err})
}

// last is the outcome of the most recent render, and fails the test if
// nothing has rendered.
func (f *fakeInputs) last(t *testing.T) renderOutcome {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.rendered) == 0 {
		t.Fatal("nothing reported a render")
	}
	return f.rendered[len(f.rendered)-1]
}

type renderOutcome struct {
	in  dhcp.RenderInput
	err error
}

func openSettings(t *testing.T) store.SettingsStore {
	t.Helper()
	st, err := store.Open(t.Context(), "sqlite", filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st.Settings()
}

// lanScope and iotScope are the two scopes every manager test renders and
// polls against: one on the settings' domain, one with its own suffix.
func lanScope() store.Scope {
	return store.Scope{
		ID: 1, Name: "lan", CIDR: "10.42.0.0/24",
		PoolStart: "10.42.0.100", PoolEnd: "10.42.0.200",
		Gateway: "10.42.0.1", Enabled: true,
	}
}

func iotScope() store.Scope {
	return store.Scope{
		ID: 2, Name: "iot", CIDR: "10.43.0.0/16",
		PoolStart: "10.43.0.1", PoolEnd: "10.43.1.250",
		Domain: "iot.lan", Enabled: true,
	}
}

func managerInput(socket string) dhcp.RenderInput {
	return dhcp.RenderInput{
		Scopes:       []store.Scope{lanScope(), iotScope()},
		Domain:       "home.lan",
		LeaseSeconds: 3600,
		Socket:       socket,
		ThisServer:   "main",
		DNSServers:   []string{"10.42.0.2"},
	}
}

// newManager wires a manager to whatever is listening on socket, with in as
// everything the app would have read for it.
func newManager(t *testing.T, socket string, in dhcp.RenderInput) *dhcp.Manager {
	t.Helper()
	return dhcp.NewManager(dhcp.NewClient(socket), &fakeInputs{in: in}, openSettings(t), nil)
}

func TestStartRendersOnceAndDiscoversHookDir(t *testing.T) {
	e := newEngine(t, "3.0.3", alpineHooks)
	m := newManager(t, e.Socket(), managerInput(e.Socket()))

	if err := m.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Discovery first, then one render, then the first poll — Start returns
	// after that, with the loop left to the goroutine.
	calls := e.Calls()
	want := []string{"version-get", "config-get", "config-set", "lease4-get-page"}
	if len(calls) < len(want) || !slices.Equal(calls[:len(want)], want) {
		t.Fatalf("calls = %v, want it to begin %v", calls, want)
	}
	if n := count(calls, "config-set"); n != 1 {
		t.Errorf("config-set sent %d times, want once", n)
	}
	if n := count(calls, "lease4-get-page"); n != 1 {
		t.Errorf("lease4-get-page sent %d times, want once", n)
	}

	sent := e.sent()
	if len(sent) != 1 {
		t.Fatalf("recorded %d configurations, want 1", len(sent))
	}
	hooks, _ := sent[0]["hooks-libraries"].([]any)
	if len(hooks) == 0 {
		t.Fatalf("no hooks-libraries in %v", sent[0])
	}
	first, _ := hooks[0].(map[string]any)
	if lib, _ := first["library"].(string); lib != alpineHooks+"/libdhcp_lease_cmds.so" {
		t.Errorf("hook library = %q, want it under %s", lib, alpineHooks)
	}
	// version-get said 3.0.3, so the renderer was told a version that spells
	// the control socket in the plural.
	if _, ok := sent[0]["control-sockets"]; !ok {
		t.Errorf("no control-sockets in the rendered config: the engine version was not discovered (%v)", sent[0])
	}

	if st := m.Status(); st.Engine != "ok" || st.EngineVersion != "3.0.3" || !st.Enabled {
		t.Errorf("status = %+v, want an enabled, ok engine at 3.0.3", st)
	}
}

func TestStartDiscoversTheHookDirOfAnOlderEngine(t *testing.T) {
	e := newEngine(t, "2.6.3", debianHooks)
	m := newManager(t, e.Socket(), managerInput(e.Socket()))

	if err := m.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	sent := e.sent()
	if len(sent) != 1 {
		t.Fatalf("recorded %d configurations, want 1", len(sent))
	}
	hooks, _ := sent[0]["hooks-libraries"].([]any)
	first, _ := hooks[0].(map[string]any)
	if lib, _ := first["library"].(string); lib != debianHooks+"/libdhcp_lease_cmds.so" {
		t.Errorf("hook library = %q, want it under %s", lib, debianHooks)
	}
	if _, ok := sent[0]["control-sockets"]; ok {
		t.Errorf("2.6.3 was rendered the plural control-sockets key: %v", sent[0])
	}
}

func TestApplyRecordsARejectedConfig(t *testing.T) {
	ctx := t.Context()
	e := newEngine(t, "2.6.3", debianHooks)
	m := newManager(t, e.Socket(), managerInput(e.Socket()))

	const refusal = "subnet configuration failed: Invalid pool definition"
	e.setReject(refusal)

	err := m.Apply(ctx)
	var rejected *dhcp.RejectedError
	if !errors.As(err, &rejected) {
		t.Fatalf("Apply on a refused config = %v, want a *RejectedError", err)
	}
	if rejected.Text != refusal {
		t.Errorf("rejection text = %q, want %q", rejected.Text, refusal)
	}
	if st := m.Status(); st.Engine != "config rejected" || st.Message != refusal {
		t.Errorf("status = %+v, want config rejected carrying the engine's message", st)
	}

	// Polling reaches the engine, but the engine is serving the configuration
	// it had before the one it refused: the message stands.
	if err := m.Poll(ctx); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if st := m.Status(); st.Engine != "config rejected" || st.Message != refusal {
		t.Errorf("status = %+v after a poll, want the refusal to stand until a render is accepted", st)
	}
	// The engine answered when it refused, so no poll reads as it coming
	// back, and a refused config is not retried on a timer (§5.1).
	if n := count(e.Calls(), "config-set"); n != 1 {
		t.Errorf("config-set sent %d times, want only the one Apply sent", n)
	}

	// The next accepted render clears it, in the setting and in the status.
	e.setReject("")
	if err := m.Apply(ctx); err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	if st := m.Status(); st.Engine != "ok" || st.Message != "" {
		t.Errorf("status = %+v, want a clean ok", st)
	}
}

func TestApplyRecordsARenderThatCannotBeBuilt(t *testing.T) {
	ctx := t.Context()
	e := newEngine(t, "2.6.3", debianHooks)
	// An automatic DNS list with no address of this box inside the scope is
	// §5.3's dead end: nothing is sent, and the operator sees it the same way
	// they see a config the engine refused.
	in := managerInput(e.Socket())
	in.DNSServers = []string{""}
	m := newManager(t, e.Socket(), in)

	err := m.Apply(ctx)
	if !errors.Is(err, dhcp.ErrNoLocalAddress) {
		t.Fatalf("Apply = %v, want ErrNoLocalAddress", err)
	}
	if slices.Contains(e.Calls(), "config-set") {
		t.Errorf("a config that could not be rendered was still sent: %v", e.Calls())
	}
	st := m.Status()
	if st.Engine != "config rejected" || !strings.Contains(st.Message, "no local address is inside") {
		t.Errorf("status = %+v, want config rejected carrying the renderer's message", st)
	}
}

func TestPollBuildsTheTable(t *testing.T) {
	ctx := t.Context()
	e := newEngine(t, "2.6.3", debianHooks)

	// A full first page is what makes the poller ask for a second: the client
	// ends pagination on a page shorter than the limit it asked for.
	filler := make([]dhcp.Lease, 500)
	for i := range filler {
		ip := netip.AddrFrom4([4]byte{10, 43, byte(i / 250), byte(i%250 + 1)})
		filler[i] = dhcp.Lease{
			IP: ip.String(), MAC: mac(i), Hostname: "sensor-" + strconv.Itoa(i),
			SubnetID: 2, CLTT: 1000, ValidLft: 600,
		}
	}
	e.setLeases(append(filler,
		// Reserved by MAC, and its hostname is the client's own spelling.
		dhcp.Lease{IP: "10.42.0.100", MAC: "aa:bb:cc:dd:ee:01", Hostname: "My Laptop!", SubnetID: 1, CLTT: 1000, ValidLft: 3600},
		dhcp.Lease{IP: "10.42.0.101", MAC: "aa:bb:cc:dd:ee:02", Hostname: "printer", SubnetID: 1, CLTT: 1500, ValidLft: 3600},
		// Declined: state 1 is not an active lease and never reaches the table.
		dhcp.Lease{IP: "10.42.0.102", MAC: "aa:bb:cc:dd:ee:03", Hostname: "declined", SubnetID: 1, CLTT: 1000, ValidLft: 3600, State: 1},
		// A reservation pins a MAC and an address inside the scope that holds
		// it. These two carry scope 1's, in scope 2, and are reserved by
		// nothing. The MAC is the laptop's, on an older lease and later in the
		// page, so the name and the MAC index both have to prefer the newer.
		dhcp.Lease{IP: "10.43.5.5", MAC: "aa:bb:cc:dd:ee:01", SubnetID: 2, CLTT: 500, ValidLft: 600},
		dhcp.Lease{IP: "10.42.0.70", MAC: "aa:bb:cc:dd:ee:44", SubnetID: 2, CLTT: 1000, ValidLft: 3600},
	)...)

	in := managerInput(e.Socket())
	in.Reservations = []store.Reservation{
		{ID: 1, ScopeID: 1, MAC: "aa:bb:cc:dd:ee:01", IP: "10.42.0.100", Hostname: "laptop"},
		{ID: 2, ScopeID: 1, MAC: "aa:bb:cc:dd:ee:09", IP: "10.42.0.60", Hostname: "nas"},
		// No hostname, so it is a pin and nothing else: it is in the table
		// (the mac matcher and the leases page ask for it) with no name.
		{ID: 3, ScopeID: 1, MAC: "aa:bb:cc:dd:ee:0a", IP: "10.42.0.70"},
	}
	m := newManager(t, e.Socket(), in)

	if err := m.Poll(ctx); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if limits := e.pageLimits(); len(limits) != 2 || limits[0] != 500 || limits[1] != 500 {
		t.Fatalf("lease4-get-page limits = %v, want two pages of 500", limits)
	}

	table := m.Table()
	// 500 fillers, four active leases and the two reservations nothing has
	// leased, named or not; the declined lease is gone.
	if got := len(table.All()); got != 506 {
		t.Fatalf("table holds %d entries, want 506", got)
	}

	laptop, ok := table.ByIP(netip.MustParseAddr("10.42.0.100"))
	if !ok {
		t.Fatalf("10.42.0.100 is not in the table")
	}
	if !laptop.Reserved {
		t.Errorf("10.42.0.100 is not marked reserved: %+v", laptop)
	}
	if want := time.Unix(4600, 0); !laptop.ExpiresAt.Equal(want) {
		t.Errorf("expiry = %v, want cltt+valid-lft = %v", laptop.ExpiresAt, want)
	}
	if laptop.ScopeID != 1 || laptop.MAC != "aa:bb:cc:dd:ee:01" || laptop.Hostname != "My Laptop!" {
		t.Errorf("entry = %+v, want the lease's own scope, mac and hostname", laptop)
	}

	printer, ok := table.ByMAC("aa:bb:cc:dd:ee:02")
	if !ok || printer.IP != netip.MustParseAddr("10.42.0.101") {
		t.Errorf("ByMAC(printer) = %+v, %v", printer, ok)
	}
	if printer.Reserved {
		t.Errorf("10.42.0.101 is reserved by nothing: %+v", printer)
	}
	if _, ok := table.ByIP(netip.MustParseAddr("10.42.0.102")); ok {
		t.Errorf("the declined lease reached the table")
	}

	// Neither half of a reservation reaches out of its own scope: the laptop's
	// MAC on a scope 2 lease is not reserved by its scope 1 reservation, and
	// neither is scope 2's lease of an address scope 1 has pinned. That last
	// one is asked for by MAC rather than by address, because the address now
	// carries two entries — the lease and the pin — and this is about the
	// lease.
	if got, ok := table.ByIP(netip.MustParseAddr("10.43.5.5")); !ok || got.Reserved {
		t.Errorf("10.43.5.5 is in scope 2 and reserved by a reservation of scope 1: %+v, %v", got, ok)
	}
	if got, ok := table.ByMAC("aa:bb:cc:dd:ee:44"); !ok || got.Reserved {
		t.Errorf("scope 2's lease of 10.42.0.70 is reserved by a reservation of scope 1: %+v, %v", got, ok)
	}
	// One MAC on two leases is the newer lease, wherever it sits in the page.
	if got, ok := table.ByMAC("aa:bb:cc:dd:ee:01"); !ok || got.IP != netip.MustParseAddr("10.42.0.100") {
		t.Errorf("ByMAC on a mac with two leases = %+v, %v, want the newer one", got, ok)
	}

	// Names: the client's hostname sanitised under the scope's suffix, which
	// is the settings' domain for the lan scope and its own for iot.
	if got, ok := table.ByName("my-laptop", "home.lan"); !ok || got.IP != laptop.IP {
		t.Errorf("ByName(my-laptop, home.lan) = %+v, %v", got, ok)
	}
	if got, ok := table.ByName("sensor-7", "iot.lan"); !ok || got.IP != netip.MustParseAddr("10.43.0.8") {
		t.Errorf("ByName(sensor-7, iot.lan) = %+v, %v", got, ok)
	}
	if _, ok := table.ByName("sensor-7", "home.lan"); ok {
		t.Errorf("a name answered under another scope's suffix")
	}
	// A query carries a name, not something to be cleaned up into one: the
	// spelling that would sanitise to a lease's label is not that label.
	if _, ok := table.ByName("my_laptop", "home.lan"); ok {
		t.Errorf("my_laptop resolved the lease named my-laptop")
	}
	if got, ok := table.ByName("MY-LAPTOP", "HOME.LAN"); !ok || got.IP != laptop.IP {
		t.Errorf("ByName is case sensitive: %+v, %v", got, ok)
	}

	// A reservation with a hostname and no lease is in the table too, so the
	// device has a name before it has an address.
	nas, ok := table.ByName("nas", "home.lan")
	if !ok {
		t.Fatalf("the unleased reservation is not in the table")
	}
	if !nas.Reserved || !nas.ExpiresAt.IsZero() || nas.IP != netip.MustParseAddr("10.42.0.60") {
		t.Errorf("unleased reservation = %+v, want reserved with no expiry", nas)
	}
}

func TestPollKeepsTheTableWhenTheEngineVanishes(t *testing.T) {
	ctx := t.Context()
	e := newEngine(t, "2.6.3", debianHooks)
	e.setLeases(dhcp.Lease{IP: "10.42.0.100", MAC: "aa:bb:cc:dd:ee:01", Hostname: "laptop", SubnetID: 1, CLTT: 1000, ValidLft: 3600})
	m := newManager(t, e.Socket(), managerInput(e.Socket()))

	if err := m.Poll(ctx); err != nil {
		t.Fatalf("first Poll: %v", err)
	}
	before := m.Table()
	age := before.Age()

	e.Close()
	err := m.Poll(ctx)
	if !errors.Is(err, dhcp.ErrUnreachable) {
		t.Fatalf("Poll against a closed socket = %v, want ErrUnreachable", err)
	}
	after := m.Table()
	if len(after.All()) != 1 {
		t.Fatalf("table holds %d entries after the engine vanished, want the 1 it held", len(after.All()))
	}
	if _, ok := after.ByIP(netip.MustParseAddr("10.42.0.100")); !ok {
		t.Errorf("the lease read before the engine vanished is gone")
	}
	if after.Age() <= age {
		t.Errorf("table age %v did not grow past %v: the table was rebuilt", after.Age(), age)
	}
	st := m.Status()
	if st.Engine != "unreachable" || st.Message == "" {
		t.Errorf("status = %+v, want unreachable with a message", st)
	}
}

func TestPollRerendersWhenTheEngineComesBack(t *testing.T) {
	ctx := t.Context()
	// The socket dnsaur was configured with, with nothing listening on it: an
	// engine that has not been started yet (§10).
	socket := filepath.Join(t.TempDir(), "kea.sock")
	m := newManager(t, socket, managerInput(socket))

	if err := m.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if st := m.Status(); st.Engine != "unreachable" {
		t.Fatalf("status = %+v, want unreachable", st)
	}

	// The engine starts, on the path dnsaur was told it would be on.
	e := newEngine(t, "3.0.3", alpineHooks)
	if err := os.Symlink(e.Socket(), socket); err != nil {
		t.Fatalf("putting the engine on %s: %v", socket, err)
	}
	if err := m.Poll(ctx); err != nil {
		t.Fatalf("Poll: %v", err)
	}

	calls := e.Calls()
	for _, want := range []string{"lease4-get-page", "version-get", "config-get", "config-set"} {
		if !slices.Contains(calls, want) {
			t.Errorf("%s never reached the engine that came back: %v", want, calls)
		}
	}
	sent := e.sent()
	if len(sent) != 1 {
		t.Fatalf("recorded %d configurations, want the one render the engine coming back is owed", len(sent))
	}
	// Discovery ran again too: this engine is a 3.0, and the one dnsaur could
	// not reach at start told it nothing.
	if _, ok := sent[0]["control-sockets"]; !ok {
		t.Errorf("the config was not rendered for the engine's own version: %v", sent[0])
	}
	if st := m.Status(); st.Engine != "ok" || st.EngineVersion != "3.0.3" {
		t.Errorf("status = %+v, want ok at 3.0.3", st)
	}

	// Only the poll that found it again renders; the ones after it do not.
	if err := m.Poll(ctx); err != nil {
		t.Fatalf("second Poll: %v", err)
	}
	if n := count(e.Calls(), "config-set"); n != 1 {
		t.Errorf("config-set sent %d times, want once", n)
	}
}

func TestPollDoesNotLatchARefusedCommand(t *testing.T) {
	ctx := t.Context()
	e := newEngine(t, "2.6.3", debianHooks)
	e.setLeases(dhcp.Lease{IP: "10.42.0.100", MAC: "aa:bb:cc:dd:ee:01", SubnetID: 1, CLTT: 1000, ValidLft: 3600})
	m := newManager(t, e.Socket(), managerInput(e.Socket()))

	const refusal = "lease_cmds library is not loaded"
	e.failNextPage(refusal)

	err := m.Poll(ctx)
	var rejected *dhcp.RejectedError
	if !errors.As(err, &rejected) {
		t.Fatalf("Poll against a refused command = %v, want a *RejectedError", err)
	}
	// One command refused is not a configuration the engine is refusing to
	// serve: it is shown, and the engine's own state is what it was.
	if st := m.Status(); st.Engine != "ok" || st.Message != refusal {
		t.Errorf("status = %+v, want ok carrying the engine's message", st)
	}

	// An accepted render does not answer for a poll, so the message stands.
	if err := m.Apply(ctx); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if st := m.Status(); st.Engine != "ok" || st.Message != refusal {
		t.Errorf("status = %+v, want the poll's message to survive a render", st)
	}

	// The next poll gets what it asked for, so the message goes with it.
	if err := m.Poll(ctx); err != nil {
		t.Fatalf("second Poll: %v", err)
	}
	if st := m.Status(); st.Engine != "ok" || st.Message != "" {
		t.Errorf("status = %+v, want a clean ok", st)
	}
}

func TestARefusedPollDoesNotClearARefusedConfig(t *testing.T) {
	ctx := t.Context()
	e := newEngine(t, "2.6.3", debianHooks)
	e.setLeases(dhcp.Lease{IP: "10.42.0.100", MAC: "aa:bb:cc:dd:ee:01", SubnetID: 1, CLTT: 1000, ValidLft: 3600})
	m := newManager(t, e.Socket(), managerInput(e.Socket()))

	const refusal = "subnet configuration failed: Invalid pool definition"
	e.setReject(refusal)
	if err := m.Apply(ctx); err == nil {
		t.Fatalf("Apply against a refused config returned no error")
	}

	// A command refused on a poll, and then a poll that works. The engine is
	// still running the configuration it had before it refused dnsaur's, so
	// neither of them may report it as ok: nothing but an accepted render
	// knows which configuration is in force.
	e.failNextPage("lease_cmds library is not loaded")
	if err := m.Poll(ctx); err == nil {
		t.Fatalf("Poll against a refused command returned no error")
	}
	if st := m.Status(); st.Engine != "config rejected" || st.Message != refusal {
		t.Errorf("status = %+v, want the refused config to stand", st)
	}
	if err := m.Poll(ctx); err != nil {
		t.Fatalf("second Poll: %v", err)
	}
	if st := m.Status(); st.Engine != "config rejected" || st.Message != refusal {
		t.Errorf("status = %+v after a poll that worked, want the refused config to stand", st)
	}
	// And a refused config is not retried on a timer (§5.1): the engine
	// answered, so no poll counts as it coming back.
	if n := count(e.Calls(), "config-set"); n != 1 {
		t.Errorf("config-set sent %d times, want only the one Apply sent", n)
	}

	e.setReject("")
	if err := m.Apply(ctx); err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	if st := m.Status(); st.Engine != "ok" || st.Message != "" {
		t.Errorf("status = %+v, want a clean ok", st)
	}
}

func TestSubscribeFiresOnChange(t *testing.T) {
	ctx := t.Context()
	e := newEngine(t, "2.6.3", debianHooks)
	e.setLeases(dhcp.Lease{IP: "10.42.0.100", MAC: "aa:bb:cc:dd:ee:01", SubnetID: 1, CLTT: 1000, ValidLft: 3600})
	m := newManager(t, e.Socket(), managerInput(e.Socket()))

	// Called on the goroutine that polls, which here is this one.
	var got []*dhcp.Table
	m.Subscribe(func(tb *dhcp.Table) { got = append(got, tb) })

	if err := m.Poll(ctx); err != nil {
		t.Fatalf("first Poll: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("subscriber called %d times after one poll, want once", len(got))
	}
	if got[0] != m.Table() {
		t.Errorf("the subscriber was handed a table the manager does not hold")
	}
	if _, ok := got[0].ByIP(netip.MustParseAddr("10.42.0.100")); !ok {
		t.Errorf("the subscriber's table is empty")
	}

	e.setLeases(dhcp.Lease{IP: "10.42.0.101", MAC: "aa:bb:cc:dd:ee:02", SubnetID: 1, CLTT: 1000, ValidLft: 3600})
	if err := m.Poll(ctx); err != nil {
		t.Fatalf("second Poll: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("subscriber called %d times after two polls, want twice", len(got))
	}
	if _, ok := got[1].ByIP(netip.MustParseAddr("10.42.0.101")); !ok {
		t.Errorf("the second table does not hold the second poll's lease")
	}

	// A poll that reaches nothing changes no table, so it notifies nobody.
	e.Close()
	if err := m.Poll(ctx); err == nil {
		t.Fatalf("Poll against a closed socket returned no error")
	}
	if len(got) != 2 {
		t.Errorf("subscriber called %d times, want the failed poll to notify nobody", len(got))
	}
}

func TestReleaseSendsLeaseDel(t *testing.T) {
	ctx := t.Context()
	e := newEngine(t, "2.6.3", debianHooks)
	m := newManager(t, e.Socket(), managerInput(e.Socket()))

	if err := m.Release(ctx, netip.MustParseAddr("10.42.0.100")); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if got := e.deletedLeases(); !slices.Equal(got, []string{"10.42.0.100"}) {
		t.Errorf("lease4-del carried %v, want [10.42.0.100]", got)
	}
}

// TestReleaseDropsTheLeaseFromTheTable: the Leases page reads the table, not
// the engine, so a row that survives its own Release until the next poll
// reads as a button that did nothing — and the DNS stage goes on answering
// with a name for an address the engine has already taken back.
//
// A reservation is the exception in the other direction: the operator's pin
// outlives the lease, so the row stays with its expiry cleared, which is the
// state the next poll would rebuild it in. Dropping it whole would take that
// device's `mac` matcher and its name with it for an interval.
func TestReleaseDropsTheLeaseFromTheTable(t *testing.T) {
	ctx := t.Context()
	e := newEngine(t, "2.6.3", debianHooks)
	e.setLeases(
		dhcp.Lease{IP: "10.42.0.100", MAC: "aa:bb:cc:dd:ee:01", Hostname: "laptop", SubnetID: 1, CLTT: 1000, ValidLft: 3600},
		dhcp.Lease{IP: "10.42.0.50", MAC: "aa:bb:cc:dd:ee:02", Hostname: "printer", SubnetID: 1, CLTT: 1000, ValidLft: 3600},
	)
	in := managerInput(e.Socket())
	in.Reservations = []store.Reservation{{ID: 1, ScopeID: 1, MAC: "aa:bb:cc:dd:ee:02", IP: "10.42.0.50", Hostname: "printer"}}
	m := newManager(t, e.Socket(), in)
	if err := m.Poll(ctx); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	tables := make(chan *dhcp.Table, 4)
	m.Subscribe(func(t *dhcp.Table) { tables <- t })

	if err := m.Release(ctx, netip.MustParseAddr("10.42.0.100")); err != nil {
		t.Fatalf("Release: %v", err)
	}
	table := m.Table()
	if _, held := table.ByIP(netip.MustParseAddr("10.42.0.100")); held {
		t.Error("the released lease is still in the table")
	}
	if _, held := table.ByMAC("aa:bb:cc:dd:ee:01"); held {
		t.Error("the released lease still answers its own mac matcher")
	}
	if _, held := table.ByName("laptop", "home.lan"); held {
		t.Error("the released lease still answers its own name")
	}
	// Everyone who reads the table is told, on the spot: the client registry
	// re-resolves its `mac` matchers from whatever it was last handed.
	select {
	case got := <-tables:
		if got != table {
			t.Error("subscribers were handed a table other than the one now in force")
		}
	default:
		t.Error("the release did not notify the table's subscribers")
	}

	// A reserved address keeps its row, with no expiry: that is the pin, not
	// the lease.
	if err := m.Release(ctx, netip.MustParseAddr("10.42.0.50")); err != nil {
		t.Fatalf("Release: %v", err)
	}
	entry, held := m.Table().ByIP(netip.MustParseAddr("10.42.0.50"))
	if !held {
		t.Fatal("releasing a reserved address dropped the reservation with it")
	}
	if !entry.ExpiresAt.IsZero() || !entry.Reserved {
		t.Errorf("the reserved row is %+v, want it reserved with no expiry", entry)
	}
	if _, named := m.Table().ByName("printer", "home.lan"); !named {
		t.Error("the reservation stopped answering to its name")
	}

	// An address nothing holds changes nothing — a second click on Release,
	// or a row someone else released first.
	before := m.Table()
	if err := m.Release(ctx, netip.MustParseAddr("10.42.0.199")); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if m.Table() != before {
		t.Error("releasing an address the table never held rebuilt it anyway")
	}
}

// TestReleaseKeepsTheTableWhenTheEngineRefuses: the table describes what the
// engine holds, so a lease4-del it would not run must not take the row out of
// it — the address is still leased.
func TestReleaseKeepsTheTableWhenTheEngineRefuses(t *testing.T) {
	ctx := t.Context()
	e := newEngine(t, "2.6.3", debianHooks)
	e.setLeases(dhcp.Lease{IP: "10.42.0.100", MAC: "aa:bb:cc:dd:ee:01", SubnetID: 1, CLTT: 1000, ValidLft: 3600})
	m := newManager(t, e.Socket(), managerInput(e.Socket()))
	if err := m.Poll(ctx); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	e.Handle("lease4-del", func(json.RawMessage) dhcp.Response {
		return dhcp.Response{Result: 3, Text: "IPv4 lease not found."}
	})

	if err := m.Release(ctx, netip.MustParseAddr("10.42.0.100")); err == nil {
		t.Fatal("Release reported success on a command the engine refused")
	}
	if _, held := m.Table().ByIP(netip.MustParseAddr("10.42.0.100")); !held {
		t.Error("a refused release dropped the lease from the table anyway")
	}
}

// TestWaitReturnsWhenThePollLoopStops: the loop reads the app's scopes on
// every tick, so a shutdown that closes the store without waiting for it
// races a poll that is already inside RenderInput.
func TestWaitReturnsWhenThePollLoopStops(t *testing.T) {
	e := newEngine(t, "2.6.3", debianHooks)
	m := newManager(t, e.Socket(), managerInput(e.Socket()))
	ctx, cancel := context.WithCancel(t.Context())
	if err := m.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	stopped := make(chan struct{})
	go func() { m.Wait(); close(stopped) }()
	select {
	case <-stopped:
		t.Fatal("Wait returned while the poll loop was still running")
	case <-time.After(50 * time.Millisecond):
	}
	cancel()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Wait did not return after the context ended")
	}
}

func TestStatusCountsLeasedPerScope(t *testing.T) {
	ctx := t.Context()
	e := newEngine(t, "2.6.3", debianHooks)
	e.setLeases(
		dhcp.Lease{IP: "10.42.0.100", MAC: "aa:bb:cc:dd:ee:01", SubnetID: 1, CLTT: 1000, ValidLft: 3600},
		dhcp.Lease{IP: "10.42.0.101", MAC: "aa:bb:cc:dd:ee:02", SubnetID: 1, CLTT: 1000, ValidLft: 3600},
		dhcp.Lease{IP: "10.43.0.5", MAC: "aa:bb:cc:dd:ee:03", SubnetID: 2, CLTT: 1000, ValidLft: 3600},
		// Declined leases are not handed out, so they are not leased.
		dhcp.Lease{IP: "10.42.0.102", MAC: "aa:bb:cc:dd:ee:04", SubnetID: 1, CLTT: 1000, ValidLft: 3600, State: 1},
	)
	in := managerInput(e.Socket())
	// A reservation nobody has leased is a name, not a lease, and is counted
	// as neither.
	in.Reservations = []store.Reservation{{ID: 1, ScopeID: 1, MAC: "aa:bb:cc:dd:ee:09", IP: "10.42.0.60", Hostname: "nas"}}
	m := newManager(t, e.Socket(), in)
	e.setHA(&dhcp.HAStatus{Mode: "hot-standby", LocalState: "hot-standby", RemoteState: "waiting"})

	if err := m.Poll(ctx); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	st := m.Status()
	want := []dhcp.ScopeUsage{
		{ID: 1, PoolSize: 101, Leased: 2},
		{ID: 2, PoolSize: 506, Leased: 1},
	}
	if !slices.Equal(st.Scopes, want) {
		t.Errorf("scopes = %+v, want %+v", st.Scopes, want)
	}
	if st.HA == nil || st.HA.Mode != "hot-standby" || st.HA.RemoteState != "waiting" {
		t.Errorf("ha = %+v, want the state status-get reported", st.HA)
	}
}

func count(ss []string, want string) int {
	n := 0
	for _, s := range ss {
		if s == want {
			n++
		}
	}
	return n
}

// mac spells i into the last two bytes of a hardware address, so 500 filler
// leases are 500 distinct devices.
func mac(i int) string { return fmt.Sprintf("aa:bb:cc:00:%02x:%02x", i>>8, i&0xff) }

// TestARefusedPollDoesNotSwallowTheReconnect: the engine that comes back is
// owed a discovery and a render (§10), and the poll that finds it again may
// be one it refuses a command on — a lease_cmds hook it has not loaded yet,
// say. The refusal answers "it is there", which cleared the reconnect with
// nothing left to act on it: every later poll then saw an engine that had
// never been down, and it went on serving whatever it started with.
func TestARefusedPollDoesNotSwallowTheReconnect(t *testing.T) {
	ctx := t.Context()
	socket := filepath.Join(t.TempDir(), "kea.sock")
	m := newManager(t, socket, managerInput(socket))
	if err := m.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if st := m.Status(); st.Engine != "unreachable" {
		t.Fatalf("status = %+v, want unreachable", st)
	}

	e := newEngine(t, "3.0.3", alpineHooks)
	if err := os.Symlink(e.Socket(), socket); err != nil {
		t.Fatalf("putting the engine on %s: %v", socket, err)
	}
	e.failNextPage("lease_cmds library is not loaded")
	if err := m.Poll(ctx); err == nil {
		t.Fatal("Poll against a refused command = nil, want the refusal")
	}
	if n := len(e.sent()); n != 0 {
		t.Fatalf("a poll that read no leases rendered %d configurations, want none", n)
	}

	if err := m.Poll(ctx); err != nil {
		t.Fatalf("second Poll: %v", err)
	}
	calls := e.Calls()
	for _, want := range []string{"version-get", "config-get", "config-set"} {
		if !slices.Contains(calls, want) {
			t.Errorf("%s never reached the engine that came back through a refused poll: %v", want, calls)
		}
	}
	if n := len(e.sent()); n != 1 {
		t.Errorf("recorded %d configurations, want the one render the engine is owed", n)
	}
	if st := m.Status(); st.Engine != "ok" || st.Message != "" {
		t.Errorf("status = %+v, want a clean ok", st)
	}
}

// TestConcurrentReleasesDoNotLoseEachOther: Release corrects the table on
// the spot rather than waiting for the next poll, which makes it a
// read-modify-write over the same pointer a poll swaps. Two of them at once
// — two rows on the Leases page, two API requests — each read the table the
// other is about to replace, and whichever stored second put the other's row
// back. The row then sits there answering DNS with a name for an address the
// engine has already taken back, until a poll rebuilds the table.
func TestConcurrentReleasesDoNotLoseEachOther(t *testing.T) {
	ctx := t.Context()
	const n = 8
	e := newEngine(t, "2.6.3", debianHooks)
	leases := make([]dhcp.Lease, n)
	addrs := make([]netip.Addr, n)
	for i := range leases {
		addrs[i] = netip.AddrFrom4([4]byte{10, 42, 0, byte(100 + i)})
		leases[i] = dhcp.Lease{
			IP: addrs[i].String(), MAC: fmt.Sprintf("aa:bb:cc:dd:ee:%02d", i),
			SubnetID: 1, CLTT: 1000, ValidLft: 3600,
		}
	}
	e.setLeases(leases...)
	m := newManager(t, e.Socket(), managerInput(e.Socket()))
	if err := m.Poll(ctx); err != nil {
		t.Fatalf("Poll: %v", err)
	}

	// One barrier, so the eight read-modify-writes overlap rather than
	// queueing behind each other's round trip to the engine.
	start := make(chan struct{})
	var wg sync.WaitGroup
	for _, ip := range addrs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if err := m.Release(ctx, ip); err != nil {
				t.Errorf("Release(%s): %v", ip, err)
			}
		}()
	}
	close(start)
	wg.Wait()

	table := m.Table()
	for _, ip := range addrs {
		if _, held := table.ByIP(ip); held {
			t.Fatalf("%s is back in the table after being released; one release overwrote another", ip)
		}
	}
}

// TestEveryRenderReportsWhatBecameOfIt is the contract App's half rests on
// (§5.1, §6): the HA pair a main publishes and the record of what the engine
// is holding both hang off a render having been *accepted*, so every path
// that renders has to say which it was — including the two that never reach
// the engine at all.
func TestEveryRenderReportsWhatBecameOfIt(t *testing.T) {
	ctx := t.Context()
	e := newEngine(t, "2.6.3", debianHooks)
	inputs := &fakeInputs{in: managerInput(e.Socket())}
	m := dhcp.NewManager(dhcp.NewClient(e.Socket()), inputs, openSettings(t), nil)

	if err := m.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	got := inputs.last(t)
	if got.err != nil {
		t.Fatalf("the first render reported %v, want the acceptance", got.err)
	}
	if len(got.in.Scopes) != len(inputs.in.Scopes) || got.in.Domain != inputs.in.Domain {
		t.Errorf("the accepted render was reported as %+v, want the input it was built from", got.in)
	}

	// A config the engine answers with a no.
	e.setReject("hooks-libraries: cannot open libdhcp_ha.so")
	if err := m.Apply(ctx); err == nil {
		t.Fatal("a refused config-set reported success")
	}
	var rejected *dhcp.RejectedError
	if got = inputs.last(t); !errors.As(got.err, &rejected) {
		t.Errorf("a refused config-set was reported as %v, want the engine's refusal", got.err)
	}

	// And one that never reaches the engine: half a pair, which the renderer
	// refuses itself (§6).
	e.setReject("")
	in := managerInput(e.Socket())
	in.Peers = []dhcp.Peer{
		{Name: "main", URL: "http://:8000/", Role: "primary"},
		{Name: "backup", URL: "http://10.0.0.3:8000/", Role: "standby"},
	}
	if err := m.ApplyInput(ctx, in); err == nil {
		t.Fatal("a configuration that cannot be built rendered")
	}
	if got = inputs.last(t); !errors.Is(got.err, dhcp.ErrNoPeerHost) {
		t.Errorf("a render that could not be built was reported as %v, want the renderer's refusal", got.err)
	}
}

// TestApplyIfChangedIsAboutWhatTheEngineIsGiven is §5.1's gate, and what it
// compares: the Dhcp4 object, not the input. The input carries facts that
// never reach the engine, and LocalAddrs is all of them — every IPv4 prefix
// on the host, re-read on every pass — so a lease renewed on the WAN side or
// a VLAN brought up would have Kea rebuild every subnet to arrive at exactly
// what it was already running.
func TestApplyIfChangedIsAboutWhatTheEngineIsGiven(t *testing.T) {
	ctx := t.Context()
	e := newEngine(t, "2.6.3", debianHooks)
	in := managerInput(e.Socket())
	// The scopes name their own resolvers, so no rendered value is drawn
	// from LocalAddrs and changing it can only be noise.
	for i := range in.Scopes {
		in.Scopes[i].DNSServers = "10.42.0.2"
	}
	m := newManager(t, e.Socket(), in)
	if err := m.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := len(e.sent()); got != 1 {
		t.Fatalf("the engine was sent %d configurations at start, want one", got)
	}

	moved := in
	moved.LocalAddrs = []netip.Prefix{
		netip.MustParsePrefix("10.8.0.4/24"), netip.MustParsePrefix("172.17.0.1/16"),
	}
	if err := m.ApplyIfChanged(ctx, moved); err != nil {
		t.Fatalf("ApplyIfChanged: %v", err)
	}
	if got := len(e.sent()); got != 1 {
		t.Fatalf("an address that moved on an interface no scope uses sent %d more configurations, "+
			"want none — the gate is about what the engine is given, not about the host", got-1)
	}

	// And the thing the gate exists to let through still goes.
	edited := in
	edited.Scopes = slices.Clone(in.Scopes)
	edited.Scopes[0].PoolEnd = "10.42.0.150"
	if err := m.ApplyIfChanged(ctx, edited); err != nil {
		t.Fatalf("ApplyIfChanged after a scope edit: %v", err)
	}
	if got := len(e.sent()); got != 2 {
		t.Fatalf("a scope edit left the engine on %d configurations, want two", got)
	}

	// A configuration the engine refused is not re-sent while nothing about
	// it has changed: §5.1's "nothing retries on a timer", which is what
	// makes the lease poll safe to run the gate on every tick.
	e.setReject("hooks-libraries: cannot open libdhcp_ha.so")
	refused := edited
	refused.Domain = "other.lan"
	if err := m.ApplyIfChanged(ctx, refused); err == nil {
		t.Fatal("the engine refused the configuration and ApplyIfChanged reported success")
	}
	// Counted as commands, not as accepted configurations: a refused
	// config-set reaches the engine and rebuilds nothing, and re-sending it
	// is exactly what this is about.
	setsBefore := configSets(e)
	for range 3 {
		if err := m.ApplyIfChanged(ctx, refused); err != nil {
			t.Fatalf("ApplyIfChanged after a refusal: %v", err)
		}
	}
	if got := configSets(e); got != setsBefore {
		t.Fatalf("a refused configuration was re-sent %d times with nothing changed, want none", got-setsBefore)
	}
}

// configSets is how many config-set commands the engine has been asked for,
// refusals included — e.sent() counts only the ones it accepted.
func configSets(e *fakeEngine) int {
	n := 0
	for _, c := range e.Calls() {
		if c == "config-set" {
			n++
		}
	}
	return n
}

// The engine is asked to keep its control socket where its own configuration
// has it, whatever path the manager dialled: a config naming another path is
// refused, and Debian's build then drops the socket until a restart.
func TestApplyKeepsTheSocketWhereTheEngineHasIt(t *testing.T) {
	ctx := t.Context()
	e := newEngine(t, "2.6.3", debianHooks)
	e.Handle("config-get", func(json.RawMessage) dhcp.Response {
		return dhcp.Response{Arguments: marshal(t, map[string]any{"Dhcp4": map[string]any{
			"hooks-libraries": []any{map[string]any{"library": debianHooks + "/libdhcp_lease_cmds.so"}},
			"control-socket":  map[string]any{"socket-type": "unix", "socket-name": "/run/kea/kea.sock"},
		}})}
	})
	m := newManager(t, e.Socket(), managerInput(e.Socket()))
	// Start is where the engine is asked about itself; Apply alone renders
	// with whatever discovery last found.
	if err := m.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	sent := e.sent()
	one, _ := sent[len(sent)-1]["control-socket"].(map[string]any)
	if got := one["socket-name"]; got != "/run/kea/kea.sock" {
		t.Errorf("rendered socket-name = %v, want the engine's own /run/kea/kea.sock, not %s", got, e.Socket())
	}
}
