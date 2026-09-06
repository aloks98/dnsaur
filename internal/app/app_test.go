package app

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aloks98/dnsaur/internal/config"
	"github.com/aloks98/dnsaur/internal/dnssrv"
	"github.com/aloks98/dnsaur/internal/store"
	"github.com/miekg/dns"
)

// TestApplySettingsFallsBackWhenNoForwarderYet reproduces the "nil forwarder
// on first applySettings failure" bug: if upstream.New fails while
// swappable.h has never been set, every DNS query used to panic (recovered,
// but resolution was permanently dead) instead of falling back to something
// usable.
//
// The settings store is seeded with a corrupt upstream.strategy but valid
// (mock) upstreams *before* Start runs applySettings for the first time.
// Since real default upstreams point at the public internet, this asserts
// the fallback retries upstream.New with the *stored* upstreams under the
// default strategy (rather than jumping straight to hardcoded defaults),
// and that resolution actually goes through the mock upstream.
func TestApplySettingsFallsBackWhenNoForwarderYet(t *testing.T) {
	ctx := context.Background()
	var upstreamHits atomic.Int64
	upAddr := mockUpstream(t, &upstreamHits)

	dir := t.TempDir()
	cfg := &config.Config{DNSListen: []string{"127.0.0.1:0"}, HTTPListen: ":0", DataDir: dir, LogLevel: "error"}
	cfg.Storage.Driver = "sqlite"
	cfg.Storage.DSN = dir + "/t.db"

	a, err := New(ctx, cfg, "test")
	if err != nil {
		t.Fatal(err)
	}
	s := a.Store()
	if err := s.Settings().SetInternal(ctx, "upstream.strategy", "not-a-real-strategy"); err != nil {
		t.Fatal(err)
	}
	if err := s.Settings().SetInternal(ctx, "upstreams", upAddr); err != nil {
		t.Fatal(err)
	}

	if err := a.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Shutdown(context.Background()) }()
	a.WaitReady(5 * time.Second)

	if !a.fwd.isSet() {
		t.Fatal("forwarder was never installed; applySettings left swappable.h nil")
	}

	c := new(dns.Client)
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn("example.org"), dns.TypeA)
	r, _, err := c.Exchange(m, a.DNSAddr())
	if err != nil {
		t.Fatal(err)
	}
	if r.Rcode != dns.RcodeSuccess || len(r.Answer) == 0 {
		t.Fatalf("resolution failed after forwarder fallback: rcode=%v answer=%v", r.Rcode, r.Answer)
	}
	if upstreamHits.Load() == 0 {
		t.Fatal("mock upstream was never queried; fallback did not use the stored (valid) upstreams")
	}
}

// --- Milestone D6: forwarder and stub zone routing ---------------------------
//
// These drive the real App — store, resolver, pipeline and DNS listener — so
// they prove the wiring rather than the map builder in isolation. The
// forwarder's own suffix routing is covered in internal/upstream; what is
// only testable here is that a zone reload reaches it at all.

// mockDNS runs a DNS server on 127.0.0.1:0 answering with h and returns its
// address.
func mockDNS(t *testing.T, h dns.HandlerFunc) string {
	t.Helper()
	return mockDNSCounting(t, nil, h)
}

// mockDNSCounting is mockDNS with a hit counter, which is how a test tells
// "answered from the cache" apart from "asked the upstream again".
func mockDNSCounting(t *testing.T, hits *atomic.Int64, h dns.HandlerFunc) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &dns.Server{PacketConn: pc, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, m *dns.Msg) {
		if hits != nil {
			hits.Add(1)
		}
		h(w, m)
	})}
	go func() { _ = srv.ActivateAndServe() }()
	t.Cleanup(func() { _ = srv.Shutdown() })
	return pc.LocalAddr().String()
}

// answerA answers every query with one A record holding ip.
func answerA(ip string) dns.HandlerFunc {
	return func(w dns.ResponseWriter, m *dns.Msg) {
		r := new(dns.Msg)
		r.SetReply(m)
		rr, _ := dns.NewRR(m.Question[0].Name + " 300 IN A " + ip)
		r.Answer = []dns.RR{rr}
		_ = w.WriteMsg(r)
	}
}

// testAppOption seeds a setting before Start, i.e. before the first
// applySettings reads it.
type testAppOption func(map[string]string)

// withUpstreams points the default upstreams at addrs: where every name no
// zone claims is answered from.
func withUpstreams(addrs ...string) testAppOption {
	return func(s map[string]string) { s["upstreams"] = strings.Join(addrs, ",") }
}

// withSetting seeds any other setting, for the cases where the cache's own
// timings are what the test is about.
func withSetting(k, v string) testAppOption {
	return func(s map[string]string) { s[k] = v }
}

// newTestApp builds and starts a sqlite-backed App on loopback, shut down
// when the test ends. It is newTestAppOn's sqlite spelling, kept because most
// cases here have nothing to say about the driver.
func newTestApp(t *testing.T, opts ...testAppOption) *App {
	t.Helper()
	return newTestAppOn(t, "sqlite", opts...)
}

// newTestAppOn builds and starts an App on loopback over the named driver,
// shut down when the test ends.
func newTestAppOn(t *testing.T, driver string, opts ...testAppOption) *App {
	t.Helper()
	ctx := context.Background()
	settings := map[string]string{}
	for _, o := range opts {
		o(settings)
	}
	a, err := New(ctx, testConfigOn(t, driver), "test")
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range settings {
		if err := a.Store().Settings().SetInternal(ctx, k, v); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Shutdown(context.Background()) })
	a.WaitReady(5 * time.Second)
	return a
}

func mustAddZone(t *testing.T, a *App, z store.Zone) int64 {
	t.Helper()
	id, err := a.Store().Zones().AddZone(context.Background(), z)
	if err != nil {
		t.Fatalf("AddZone(%s): %v", z.Name, err)
	}
	return id
}

func mustZone(t *testing.T, a *App, id int64) store.Zone {
	t.Helper()
	z, err := a.Store().Zones().Zone(context.Background(), id)
	if err != nil {
		t.Fatalf("Zone(%d): %v", id, err)
	}
	return z
}

func mustReloadZones(t *testing.T, a *App) {
	t.Helper()
	if err := a.ReloadZones(context.Background()); err != nil {
		t.Fatalf("ReloadZones: %v", err)
	}
}

// askApp asks the running server for name's A record and returns the first
// answer's address.
func askApp(t *testing.T, a *App, name string) string {
	t.Helper()
	return digA(t, a.DNSAddr(), name)
}

// digAQuiet is digA for the goroutines: it reports the failure in its return
// value rather than calling t.Fatal, which is illegal off the test goroutine.
func digAQuiet(addr, name string) string {
	c := new(dns.Client)
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(name), dns.TypeA)
	r, _, err := c.Exchange(m, addr)
	if err != nil {
		return "error: " + err.Error()
	}
	if len(r.Answer) == 0 {
		return "no answer, rcode " + dns.RcodeToString[r.Rcode]
	}
	a, ok := r.Answer[0].(*dns.A)
	if !ok {
		return "not an A record"
	}
	return a.A.String()
}

// waitFor blocks until cond holds, failing the test rather than hanging if it
// never does. It is how a test that needs something to be genuinely in flight
// gets that guarantee instead of sleeping and hoping.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// askAppMsg is askApp for the cases where the answer section is empty and the
// rcode is the point.
func askAppMsg(t *testing.T, a *App, name string) *dns.Msg {
	t.Helper()
	c := new(dns.Client)
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(name), dns.TypeA)
	r, _, err := c.Exchange(m, a.DNSAddr())
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// A forwarder zone routes its suffix to its own upstreams, and the default
// upstreams still answer everything else.
func TestForwarderZoneRoutesToItsUpstreams(t *testing.T) {
	corp := mockDNS(t, answerA("10.0.0.1"))
	pub := mockDNS(t, answerA("5.6.7.8"))

	a := newTestApp(t, withUpstreams(pub))
	mustAddZone(t, a, store.Zone{
		Name: "corp.example", Type: "forwarder", Enabled: true, ForwardTo: corp,
	})
	mustReloadZones(t, a)

	if got := askApp(t, a, "vpn.corp.example"); got != "10.0.0.1" {
		t.Errorf("in-zone query answered %q, want the forwarder zone's own upstream", got)
	}
	if got := askApp(t, a, "elsewhere.example"); got != "5.6.7.8" {
		t.Errorf("out-of-zone query answered %q, want the default upstream", got)
	}
}

// Disabling releases the suffix. Without this a disabled forwarder zone would
// keep swallowing its suffix while every other path in the server treats
// disabled as "not handling that name".
func TestDisablingAForwarderZoneReleasesItsSuffix(t *testing.T) {
	forEachDriver(t, func(t *testing.T, driver string) {
		corp := mockDNS(t, answerA("10.0.0.1"))
		pub := mockDNS(t, answerA("5.6.7.8"))

		a := newTestAppOn(t, driver, withUpstreams(pub))
		id := mustAddZone(t, a, store.Zone{
			Name: "corp.example", Type: "forwarder", Enabled: true, ForwardTo: corp,
		})
		mustReloadZones(t, a)
		if got := askApp(t, a, "vpn.corp.example"); got != "10.0.0.1" {
			t.Fatalf("precondition: in-zone query answered %q, want the forwarder zone's own upstream", got)
		}

		z := mustZone(t, a, id)
		z.Enabled = false
		if err := a.Store().Zones().UpdateZone(context.Background(), z); err != nil {
			t.Fatalf("UpdateZone: %v", err)
		}
		mustReloadZones(t, a)

		// The same name the precondition asked for, whose *internal* answer is
		// in the pipeline's cache for its 300s TTL. Releasing a suffix has to
		// invalidate those entries too — this is the mirror of a claim being
		// bypassed by a cached public answer, and it points the other way: a name
		// that should now resolve publicly kept answering from behind the
		// corporate resolver until the entry aged out.
		if got := askApp(t, a, "vpn.corp.example"); got != "5.6.7.8" {
			t.Errorf("after disabling, answered %q, want the default upstream", got)
		}
		// And a name never asked before, which is what the assertion above used
		// to have to be, so a regression that reinstated the fall-through-only
		// half is still caught here.
		if got := askApp(t, a, "nas.corp.example"); got != "5.6.7.8" {
			t.Errorf("after disabling, answered %q, want the default upstream", got)
		}
	})
}

// A forwarder zone naming no upstreams keeps its claim and SERVFAILs. Falling
// through would let the public internet answer an internal name, which is the
// failure §9.11.5 exists to prevent.
func TestForwarderZoneWithNoUpstreamsServfails(t *testing.T) {
	pub := mockDNS(t, answerA("5.6.7.8"))
	a := newTestApp(t, withUpstreams(pub))
	mustAddZone(t, a, store.Zone{
		Name: "corp.example", Type: "forwarder", Enabled: true, ForwardTo: "",
	})
	mustReloadZones(t, a)

	m := askAppMsg(t, a, "vpn.corp.example")
	if m.Rcode != dns.RcodeServerFailure {
		t.Errorf("rcode = %s, want SERVFAIL", dns.RcodeToString[m.Rcode])
	}
	if len(m.Answer) != 0 {
		t.Errorf("answered %v, want nothing — the public internet must not be asked", m.Answer)
	}
}

// A stored forward_to that will not parse fails the same way as none at all.
// The API validates on write, so this is a hand-edited row — and the safe
// direction for one is a name that does not resolve, not a name the public
// internet gets to answer.
func TestForwarderZoneWithUnparseableForwardToServfails(t *testing.T) {
	pub := mockDNS(t, answerA("5.6.7.8"))
	a := newTestApp(t, withUpstreams(pub))
	mustAddZone(t, a, store.Zone{
		Name: "corp.example", Type: "forwarder", Enabled: true, ForwardTo: "10.0.0.1:99999",
	})
	mustReloadZones(t, a)

	m := askAppMsg(t, a, "vpn.corp.example")
	if m.Rcode != dns.RcodeServerFailure {
		t.Errorf("rcode = %s, want SERVFAIL", dns.RcodeToString[m.Rcode])
	}
	if len(m.Answer) != 0 {
		t.Errorf("answered %v, want nothing — an unparseable forward_to must not fall through", m.Answer)
	}
}

// A stub zone claims its suffix too, from the moment it is created and before
// it has fetched anything. Its upstreams are derived from its NS records
// (Task 7), so until the first fetch lands it names none — and a zone that
// names none claims and SERVFAILs rather than falling through.
func TestStubZoneClaimsItsSuffixBeforeItsFirstFetch(t *testing.T) {
	pub := mockDNS(t, answerA("5.6.7.8"))
	a := newTestApp(t, withUpstreams(pub))
	mustAddZone(t, a, store.Zone{
		Name: "corp.example", Type: "stub", Enabled: true, Primaries: "127.0.0.1:5399",
	})
	mustReloadZones(t, a)

	m := askAppMsg(t, a, "vpn.corp.example")
	if m.Rcode != dns.RcodeServerFailure {
		t.Errorf("rcode = %s, want SERVFAIL", dns.RcodeToString[m.Rcode])
	}
	if len(m.Answer) != 0 {
		t.Errorf("answered %v, want nothing — an unfetched stub must not fall through to the defaults", m.Answer)
	}
}

// A forwarder-zone answer is cached, which is the whole reason routing lives
// in the forwarder rather than in the resolver middleware.
func TestForwarderZoneAnswersAreCached(t *testing.T) {
	var hits atomic.Int64
	corp := mockDNSCounting(t, &hits, answerA("10.0.0.1"))
	pub := mockDNS(t, answerA("5.6.7.8"))

	a := newTestApp(t, withUpstreams(pub))
	mustAddZone(t, a, store.Zone{
		Name: "corp.example", Type: "forwarder", Enabled: true, ForwardTo: corp,
	})
	mustReloadZones(t, a)

	for i := range 3 {
		if got := askApp(t, a, "vpn.corp.example"); got != "10.0.0.1" {
			t.Fatalf("query %d answered %q", i, got)
		}
	}
	if n := hits.Load(); n != 1 {
		t.Errorf("upstream asked %d times for 3 identical queries, want 1 — answers are not being cached", n)
	}
}

// applySettings builds a *fresh* forwarder on every settings change, and a
// fresh forwarder has no conditional routes of its own. If the reload's table
// is not installed on it, a settings edit silently drops every zone's routing
// until the next unrelated zone edit — and a forwarder zone would quietly
// start resolving through the public internet, the exact failure §9.11.5
// exists to prevent.
func TestForwarderZoneRoutingSurvivesASettingsChange(t *testing.T) {
	forEachDriver(t, func(t *testing.T, driver string) {
		corp := mockDNS(t, answerA("10.0.0.1"))
		pub := mockDNS(t, answerA("5.6.7.8"))
		pub2 := mockDNS(t, answerA("6.6.6.6"))

		a := newTestAppOn(t, driver, withUpstreams(pub))
		mustAddZone(t, a, store.Zone{
			Name: "corp.example", Type: "forwarder", Enabled: true, ForwardTo: corp,
		})
		mustReloadZones(t, a)
		if got := askApp(t, a, "vpn.corp.example"); got != "10.0.0.1" {
			t.Fatalf("precondition: in-zone query answered %q, want the forwarder zone's own upstream", got)
		}

		// Set (unlike SetInternal) bumps config_version and notifies Changes(),
		// which the app's watcher goroutine picks up and calls applySettings on.
		if err := a.Store().Settings().Set(context.Background(), "upstreams", pub2); err != nil {
			t.Fatalf("Set(upstreams): %v", err)
		}
		// Poll on names outside every zone — a fresh one each time, since a
		// repeat would be answered from the cache — until the new default
		// answers. That is the observable proof the forwarder was rebuilt, so the
		// assertion below runs against the fresh one and not the old one.
		deadline := time.Now().Add(5 * time.Second)
		for i := 0; ; i++ {
			if got := askApp(t, a, fmt.Sprintf("probe%d.example", i)); got == "6.6.6.6" {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("the upstreams setting never took effect, so the forwarder was never rebuilt")
			}
			time.Sleep(20 * time.Millisecond)
		}

		if got := askApp(t, a, "nas.corp.example"); got != "10.0.0.1" {
			t.Errorf("after a settings change, in-zone query answered %q, want the zone's own upstream — the rebuilt forwarder lost its conditional routes", got)
		}
	})
}

// seedForwarderZones adds n enabled forwarder zones all pointing at addr.
//
// The size is the point. Both windows below are bounded by how long
// conditionalRoutes takes to build and SetConditional to install, and at a
// homelab's single-digit zone count (§4) that is a few microseconds — too
// short for either test to observe, which is why the wrong orderings looked
// unobservable rather than merely rare. At 400 zones the same code takes long
// enough to measure, and what is being pinned is an ordering, not a duration.
func seedForwarderZones(t *testing.T, a *App, n int, addr string) {
	t.Helper()
	for i := range n {
		mustAddZone(t, a, store.Zone{
			Name: fmt.Sprintf("bulk%d.example", i), Type: "forwarder", Enabled: true, ForwardTo: addr,
		})
	}
}

// forwardReq asks the pipeline's tail directly, below the cache and the
// resolver, so the answer says which upstream the routing table chose and
// nothing else.
func forwardReq(name string) *dnssrv.Request {
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(name), dns.TypeA)
	return &dnssrv.Request{Msg: m}
}

// applySettings must install the routing table on the fresh forwarder BEFORE
// swapping it in, not after.
//
// Reversed, the fresh forwarder goes live carrying an empty conditional table
// and every forwarder and stub zone's suffix resolves through the default
// upstreams until the install lands. That window is not merely brief and
// harmless: an answer that leaks is a public answer for a split-horizon name,
// and the cache above the forwarder keeps serving it for its TTL long after
// the table is repaired. §9.11.5 exists to prevent exactly that.
func TestApplySettingsInstallsRoutesBeforeTheForwarderGoesLive(t *testing.T) {
	forEachDriver(t, func(t *testing.T, driver string) {
		ctx := context.Background()
		corp := mockDNS(t, answerA("10.0.0.1"))
		pub := mockDNS(t, answerA("5.6.7.8"))

		a := newTestAppOn(t, driver, withUpstreams(pub))
		seedForwarderZones(t, a, 400, corp)
		mustReloadZones(t, a)

		// In-zone queries, a fresh name each time, for the whole of a settings
		// change and for no longer. Every one of them must reach the zone's own
		// upstream: there is no instant during a rebuild at which a claimed
		// suffix may be answered by the defaults.
		//
		// The sample is *guaranteed* rather than asserted. The reversed ordering
		// leaks roughly 15% of the queries that land in its window, so a run that
		// managed one query would miss it 85% of the time — and under heavy CPU
		// load a querier has been seen to land exactly one. A floor would turn
		// that into a failing run, which is a flake and not a finding; repeating
		// the settings change until enough queries have actually landed during
		// one turns it into a slower run that still measures what it claims.
		const wantSamples = 20
		var asked, leaked, seq atomic.Int64
		for pass := 0; asked.Load() < wantSamples; pass++ {
			if pass == 20 {
				t.Fatalf("only %d in-zone queries landed during %d settings changes; the fixture is not measuring anything", asked.Load(), pass)
			}
			stop := make(chan struct{})
			var wg sync.WaitGroup
			// Four queriers rather than one. A single querier samples the rebuild
			// every couple of milliseconds, which is the same order as the window
			// itself, so it would be measuring the window with a ruler as coarse
			// as the thing being measured.
			for range 4 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for {
						select {
						case <-stop:
							return
						default:
						}
						resp, err := a.fwd.ServeDNS(ctx, forwardReq(fmt.Sprintf("h%d.bulk0.example", seq.Add(1))))
						asked.Add(1)
						if err == nil && resp != nil && resp.Upstream == pub {
							leaked.Add(1)
						}
					}
				}()
			}
			a.applySettings(ctx)
			close(stop)
			wg.Wait()
		}

		if n := leaked.Load(); n != 0 {
			t.Errorf("%d of %d in-zone queries reached the default upstream during a settings change — the forwarder went live before its routing table did, and every one of those answers is now cached", n, asked.Load())
		}
	})
}

// A zone reload that overlaps a settings change must not lose its routes.
//
// Both write the conditional table, and both are read-modify-write at the App
// level: ReloadZones reads which forwarder is live, builds, then installs on
// it, while applySettings builds a replacement forwarder, installs on it, and
// makes it live. Interleaved, the reload can install onto the forwarder that
// applySettings is in the middle of retiring — and the table that survives is
// the one applySettings built, from a snapshot taken before the new zone
// existed.
//
// The failure is not symmetrical and that is what makes it worth a lock. A
// surviving table that still claims a suffix it should have released only
// over-SERVFAILs, which is safe. A surviving table missing a suffix it should
// claim falls through to the public internet and the answer is cached — and
// adding or enabling a zone produces that direction. Nothing repairs it until
// an unrelated zone edit happens to rebuild the table.
//
// Serialising in SetConditional is not enough: the read-modify-write being
// interleaved here spans two objects and lives in App, above it.
//
// Both drivers, and this is the case that most needed the second one. The
// interleaving both writers race through is bounded by how long each spends
// reading the store, and store.Open sets SetMaxOpenConns(1) only for sqlite
// — so under sqlite the two reads inside each Resolver.Reload queue behind
// one connection, while under postgres they genuinely overlap. The lock was
// measured on the narrow side of that. It holds on the wide one too: with
// routeMu removed, 12 of 120 iterations lose the route under postgres
// against 12 under sqlite (26 when this fixture was first written), and none
// of them are missing from the snapshot on either driver.
func TestZoneReloadDoesNotLoseRoutesAgainstASettingsChange(t *testing.T) {
	forEachDriver(t, func(t *testing.T, driver string) {
		ctx := context.Background()
		corp := mockDNS(t, answerA("10.0.0.1"))
		pub := mockDNS(t, answerA("5.6.7.8"))

		a := newTestAppOn(t, driver, withUpstreams(pub))
		seedForwarderZones(t, a, 400, corp)
		mustReloadZones(t, a)

		// inSnapshot is what scopes the assertion below to the lock under test.
		inSnapshot := func(name string) bool {
			for _, z := range a.resolver.Snapshot().Zones() {
				if z.Name == name {
					return true
				}
			}
			return false
		}

		const iterations = 120
		lost, notInSnapshot := 0, 0
		for i := range iterations {
			name := fmt.Sprintf("added%d.example", i)
			// The stagger is what "already in flight" means, and it is swept
			// rather than fixed because the window is an interleaving and not an
			// instant. Measured on this fixture, a reload starting 1-4ms into a
			// settings change loses its zone ~30% of the time; starting together
			// (0) never does, because applySettings reads the store twice before
			// it takes the snapshot and the add always beats it there.
			stagger := time.Duration(1+i%4) * time.Millisecond
			var wg sync.WaitGroup
			wg.Add(2)
			// applySettings is called directly rather than through the settings
			// watcher: it is the same function the watcher calls, and calling it
			// here means the assertion runs when both writers have *returned*
			// rather than when a change has merely looked settled.
			go func() {
				defer wg.Done()
				a.applySettings(ctx)
			}()
			go func() {
				defer wg.Done()
				time.Sleep(stagger)
				mustAddZone(t, a, store.Zone{
					Name: name, Type: "forwarder", Enabled: true, ForwardTo: corp,
				})
				if err := a.ReloadZones(ctx); err != nil {
					t.Errorf("iteration %d: ReloadZones: %v", i, err)
				}
			}()
			wg.Wait()

			resp, err := a.fwd.ServeDNS(ctx, forwardReq("host."+name))
			if err != nil || resp == nil {
				t.Fatalf("iteration %d: ServeDNS: %v", i, err)
			}
			if resp.Upstream == corp {
				continue
			}
			if inSnapshot(name) {
				lost++
				continue
			}
			// A zone that never reached the served snapshot had no route to
			// lose: conditionalRoutes walks the snapshot, so a zone absent from
			// it provably has no entry in the table this lock protects. That is
			// what makes the attribution sound rather than convenient — and with
			// routeMu removed, 27 and 31 of 120 iterations lost the route and
			// *none* of them were missing from the snapshot, so the two failures
			// do not overlap.
			//
			// **These iterations are not benign.** A zone missing from the
			// snapshot is a §9.11.5 leak too — Index.Find stops claiming it, so
			// its names reach the forwarder and the public internet answers for
			// a name this server holds. It is a different bug in a different
			// place: Resolver.Reload's own lost update between two concurrent
			// reloads, fixed by rmu in internal/zones and pinned by
			// TestConcurrentReloadsDoNotLoseAZone. What is excluded here is the
			// *attribution*, not the severity. This counter should read 0.
			notInSnapshot++
		}
		// If the resolver defect swallowed most of the run there is nothing left
		// for the assertion to be about, and a green result would be vacuous.
		if notInSnapshot > iterations/10 {
			t.Fatalf("%d of %d iterations never reached the served snapshot; this test is no longer measuring the routing table", notInSnapshot, iterations)
		}
		if lost != 0 {
			t.Errorf("%d of %d reloads lost the zone they had just added, while the zone was in the served snapshot: its suffix resolves through the default upstreams, and nothing repairs the table until an unrelated zone edit", lost, iterations)
		}
	})
}

// startAXFRPrimary serves one tiny zone over AXFR on a loopback TCP port. It
// is the trigger the test below needs and nothing more: a real transfer, so
// the reload it performs is the production one rather than a call this test
// makes itself.
func startAXFRPrimary(t *testing.T, zone string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	soa, err := dns.NewRR(fmt.Sprintf("%s. 900 IN SOA ns1.%s. hostadmin.%s. 7 900 300 604800 900", zone, zone, zone))
	if err != nil {
		t.Fatal(err)
	}
	ns, err := dns.NewRR(fmt.Sprintf("%s. 3600 IN NS ns1.%s.", zone, zone))
	if err != nil {
		t.Fatal(err)
	}
	mux := dns.NewServeMux()
	mux.HandleFunc(dns.Fqdn(zone), func(w dns.ResponseWriter, r *dns.Msg) {
		ch := make(chan *dns.Envelope)
		tr := new(dns.Transfer)
		var wg sync.WaitGroup
		wg.Add(1)
		go func() { defer wg.Done(); _ = tr.Out(w, r, ch) }()
		ch <- &dns.Envelope{RR: []dns.RR{soa, ns, soa}}
		close(ch)
		wg.Wait()
		_ = w.Close()
	})
	srv := &dns.Server{Listener: ln, Handler: mux}
	go func() { _ = srv.ActivateAndServe() }()
	t.Cleanup(func() { _ = srv.Shutdown() })
	return ln.Addr().String()
}

// Every path that publishes a new served snapshot has to publish the routing
// table that snapshot implies, and a transfer is one of those paths.
//
// App.New builds the Transferrer with a reload callback, and the obvious one
// to pass is a.resolver.Reload — the method that rebuilds the snapshot. It is
// the wrong one. conditionalRoutes is derived *from* the snapshot, so a
// transfer that republishes the snapshot alone leaves the forwarder holding
// the table built at the last App.ReloadZones: any zone that claimed a suffix
// in between is in the snapshot the resolver serves and absent from the table
// the forwarder routes with, so its names fall through to the default
// upstreams and the public internet answers for a name this server holds —
// §9.11.5's failure, reached through the one path that publishes half a
// reload.
//
// A stub zone is what makes this concrete rather than theoretical. Its
// upstreams come from NS records a fetch installs, and a fetch is an install
// like any other: the store changes, something reloads, and the suffix must
// be claimed from that moment. Here the store change is the zone appearing
// and the reload is a secondary's transfer, because that is the install path
// this milestone already has — but the defect and the fix are the same one.
func TestATransferPublishesTheRoutingTableItsSnapshotImplies(t *testing.T) {
	forEachDriver(t, func(t *testing.T, driver string) {
		ctx := context.Background()
		pub := mockDNS(t, answerA("5.6.7.8"))
		a := newTestAppOn(t, driver, withUpstreams(pub))

		// The trigger: a secondary with a primary that will actually answer.
		primary := startAXFRPrimary(t, "xfer.example")
		secID := mustAddZone(t, a, store.Zone{
			Name: "xfer.example", Type: "secondary", Enabled: true, Primaries: primary,
			SOANS: "ns1.xfer.example", SOAMbox: "hostadmin.xfer.example",
			SOASerial: 1, SOARefresh: 900, SOARetry: 300, SOAExpire: 604800,
			SOAMinimum: 900, SOATTL: 900,
		})

		// The zone whose claim is at stake, added with nothing reloading after
		// it: the routing table the forwarder holds does not name it yet.
		mustAddZone(t, a, store.Zone{
			Name: "corp.example", Type: "stub", Enabled: true, Primaries: "127.0.0.1:5399",
		})

		if _, err := a.zoneRefresh.Refresh(ctx, secID); err != nil {
			t.Fatalf("Refresh: %v", err)
		}

		// A stub that has not fetched names no upstreams, so claiming its suffix
		// is SERVFAIL and losing the claim is the default upstream's answer. The
		// two are unmistakable.
		m := askAppMsg(t, a, "vpn.corp.example")
		if m.Rcode != dns.RcodeServerFailure || len(m.Answer) != 0 {
			t.Errorf("after a transfer, vpn.corp.example answered %s %v, want SERVFAIL and nothing — "+
				"the transfer republished the snapshot without the routing table it implies, so the stub's suffix reached the public upstreams",
				dns.RcodeToString[m.Rcode], m.Answer)
		}
	})
}

// startStubMaster answers the two ordinary queries a stub's fetch makes — SOA
// for the schedule, NS with glue for the delegation — on a loopback UDP port.
// Ordinary queries and no zone transfer at all, which is the whole point of
// the type: a stub works against a master that will transfer its zone to
// nobody.
func startStubMaster(t *testing.T, zone, nsIP string) string {
	t.Helper()
	mustRR := func(line string) dns.RR {
		rr, err := dns.NewRR(line)
		if err != nil {
			t.Fatalf("dns.NewRR(%q): %v", line, err)
		}
		return rr
	}
	soa := mustRR(fmt.Sprintf("%s. 900 IN SOA ns1.%s. hostadmin.%s. 7 900 300 604800 900", zone, zone, zone))
	ns := mustRR(fmt.Sprintf("%s. 3600 IN NS ns1.%s.", zone, zone))
	glue := mustRR(fmt.Sprintf("ns1.%s. 3600 IN A %s", zone, nsIP))

	return mockDNS(t, func(w dns.ResponseWriter, m *dns.Msg) {
		r := new(dns.Msg)
		r.SetReply(m)
		r.Authoritative = true
		if len(m.Question) == 1 {
			switch m.Question[0].Qtype {
			case dns.TypeSOA:
				r.Answer = []dns.RR{soa}
			case dns.TypeNS:
				r.Answer, r.Extra = []dns.RR{ns}, []dns.RR{glue}
			}
		}
		_ = w.WriteMsg(r)
	})
}

// The second route to the trap TestATransferPublishesTheRoutingTableIts-
// SnapshotImplies closed, and the one this milestone's stub zones actually
// travel.
//
// A stub is fetched by StubFetcher, which is a different object from the
// Transferrer and carries its own reload callback (WithStubReload). Both are
// func(context.Context) error, so nothing mechanical stops the wrong one
// being passed, and the wrong one — a.resolver.Reload — rebuilds the served
// snapshot without reinstalling the conditional routing table that is derived
// from it. Wired that way a stub's fetch installs its NS rows, nothing
// publishes them, and every zone that claimed a suffix since the last full
// reload falls through to the default upstreams: the public internet
// answering for an internal name (§9.11.5), repaired only by the next
// unrelated zone edit.
//
// What makes it worth its own test rather than a code comment is how it
// presents. The fetch succeeds, the rows are in the database, the zone page
// shows a delegation — and the suffix does not route. Everything visible
// points at the fetch, which is the one thing that is working.
func TestAStubFetchPublishesTheRoutingTableItsSnapshotImplies(t *testing.T) {
	forEachDriver(t, func(t *testing.T, driver string) {
		ctx := context.Background()
		pub := mockDNS(t, answerA("5.6.7.8"))
		a := newTestAppOn(t, driver, withUpstreams(pub))

		// The trigger: a stub with a master that will actually answer.
		master := startStubMaster(t, "fetch.example", "10.9.0.1")
		fetchID := mustAddZone(t, a, store.Zone{
			Name: "fetch.example", Type: "stub", Enabled: true, Primaries: master,
		})

		// The zone whose claim is at stake, added with nothing reloading after
		// it: the routing table the forwarder holds does not name it yet.
		mustAddZone(t, a, store.Zone{
			Name: "corp.example", Type: "stub", Enabled: true, Primaries: "127.0.0.1:5399",
		})

		if _, err := a.zoneRefresh.Refresh(ctx, fetchID); err != nil {
			t.Fatalf("Refresh: %v", err)
		}
		// The sample this test needs, guaranteed rather than assumed: the fetch
		// really did install a delegation, so what the assertion below measures
		// is the publish and not a fetch that never happened. It reads the
		// snapshot live, so it passes either way — that is what makes it a
		// precondition rather than the assertion.
		if got := a.conditionalRoutes()["fetch.example"]; len(got) != 1 || got[0] != "10.9.0.1:53" {
			t.Fatalf("the fetched stub's routes = %v, want [10.9.0.1:53]: the fetch did not install its delegation", got)
		}

		// A stub that has not fetched names no upstreams, so claiming its suffix
		// is SERVFAIL and losing the claim is the default upstream's answer. The
		// two are unmistakable.
		m := askAppMsg(t, a, "vpn.corp.example")
		if m.Rcode != dns.RcodeServerFailure || len(m.Answer) != 0 {
			t.Errorf("after a stub fetch, vpn.corp.example answered %s %v, want SERVFAIL and nothing — "+
				"the fetch republished the snapshot without the routing table it implies, so a claimed suffix reached the public upstreams",
				dns.RcodeToString[m.Rcode], m.Answer)
		}
	})
}

// --- The cache and the claim ------------------------------------------------
//
// The pipeline is resolver -> cache -> forwarder, and only the forwarder
// knows about the routing table. A cache entry is keyed on (qname, qtype)
// with no record of which route produced it, so an entry made before a zone
// claimed the suffix is served afterwards without pick ever being reached.
// These two are the halves of that, and they are the feature's own motivating
// workflow rather than a corner: you add a split-horizon forwarder zone
// *because* the name resolves publicly today, which is exactly the condition
// that puts the public answer in the cache at the moment you claim it. Same
// for re-enabling a disabled zone, and for delete-then-recreate.

// The fresh half: an entry still inside its TTL is served on a hit, so pick
// is never reached and the claim is bypassed entirely.
func TestAClaimedSuffixIsNotAnsweredFromWhatTheDefaultUpstreamsCached(t *testing.T) {
	forEachDriver(t, func(t *testing.T, driver string) {
		var corpHits atomic.Int64
		corp := mockDNSCounting(t, &corpHits, answerA("10.0.0.1"))
		pub := mockDNS(t, answerA("5.6.7.8"))

		a := newTestAppOn(t, driver, withUpstreams(pub))

		// The name resolves publicly, which is why you are about to claim it.
		if got := askApp(t, a, "www.corp.example"); got != "5.6.7.8" {
			t.Fatalf("precondition: %q, want the public answer — the fixture is not set up", got)
		}
		// The sample this test needs, guaranteed rather than assumed: there is
		// actually an entry to be wrongly served. Without it the assertion below
		// would pass on a cache that simply stored nothing.
		if n := a.dnsCache.Len(); n == 0 {
			t.Fatal("precondition: the public answer was not cached, so there is nothing here to bypass the claim")
		}

		mustAddZone(t, a, store.Zone{
			Name: "corp.example", Type: "forwarder", Enabled: true, ForwardTo: corp,
		})
		mustReloadZones(t, a)

		if got := askApp(t, a, "www.corp.example"); got != "10.0.0.1" {
			t.Errorf("the claimed suffix answered %q, want the zone's own upstream — the pre-claim entry is still being served", got)
		}
		if corpHits.Load() == 0 {
			t.Error("the zone's own upstream was never asked: the answer came from the cache, above the routing table")
		}
	})
}

// The stale half, and the worse one. When the claimed suffix's upstreams all
// fail the forwarder returns an error, and the cache answers a *stale* entry
// with rcode NOERROR and TTL 30 instead. With the shipped defaults
// (serve_stale_for 86400) the public answer stays servable for a day past its
// own TTL and every SERVFAIL in that window re-serves it — while §9.11.5
// requires a claimed suffix to SERVFAIL rather than let the outside world
// answer for an internal name.
func TestAClaimedSuffixServfailsRatherThanServingThePublicAnswerStale(t *testing.T) {
	forEachDriver(t, func(t *testing.T, driver string) {
		pub := mockDNS(t, answerA("5.6.7.8"))
		// max_ttl 1 is how the entry is made stale in a second rather than in
		// the answer's own 300. serve_stale_for stays at its default, so a stale
		// entry is still very much servable — that is the state being pinned.
		a := newTestAppOn(t, driver, withUpstreams(pub), withSetting("cache.max_ttl", "1"))

		if got := askApp(t, a, "www.corp.example"); got != "5.6.7.8" {
			t.Fatalf("precondition: %q, want the public answer — the fixture is not set up", got)
		}
		if n := a.dnsCache.Len(); n == 0 {
			t.Fatal("precondition: the public answer was not cached, so there is nothing here to serve stale")
		}
		// Past the entry's TTL and nowhere near serve_stale_for: stale, and
		// servable. Guaranteed rather than raced — a second query before this
		// lands would take the fresh path, which is the test above.
		time.Sleep(1300 * time.Millisecond)

		// A claimed suffix whose every upstream is down: 127.0.0.1:1 refuses
		// immediately, so the forwarder fails fast rather than waiting out a
		// timeout.
		mustAddZone(t, a, store.Zone{
			Name: "corp.example", Type: "forwarder", Enabled: true, ForwardTo: "127.0.0.1:1",
		})
		mustReloadZones(t, a)

		m := askAppMsg(t, a, "www.corp.example")
		if m.Rcode != dns.RcodeServerFailure {
			t.Errorf("rcode = %s, want SERVFAIL", dns.RcodeToString[m.Rcode])
		}
		if len(m.Answer) != 0 {
			t.Errorf("answered %v, want nothing — a claimed suffix whose upstreams are down must not be answered by a stale public entry", m.Answer)
		}
	})
}

// The purge is scoped to the suffixes whose routing actually changed, and
// this is what says so. Every record edit calls ReloadZones, which rebuilds
// and reinstalls the whole table, and a purge on each of those would throw
// away exactly the answers a forwarder zone exists to cache — several times a
// minute on a server anybody is using. No other test above would notice: they
// all query a route once.
//
// So the name that has to survive is one *under a claimed suffix*. Purging
// unconditionally drops only what is under the suffixes purged, so a probe
// outside every zone would survive the wrong implementation too.
func TestAReloadThatChangesNoRoutingLeavesTheCacheAlone(t *testing.T) {
	forEachDriver(t, func(t *testing.T, driver string) {
		var corpHits atomic.Int64
		corp := mockDNSCounting(t, &corpHits, answerA("10.0.0.1"))
		pub := mockDNS(t, answerA("5.6.7.8"))

		a := newTestAppOn(t, driver, withUpstreams(pub))
		mustAddZone(t, a, store.Zone{
			Name: "corp.example", Type: "forwarder", Enabled: true, ForwardTo: corp,
		})
		mustReloadZones(t, a)
		if got := askApp(t, a, "vpn.corp.example"); got != "10.0.0.1" {
			t.Fatalf("precondition: %q, want the zone's own upstream", got)
		}
		if n := corpHits.Load(); n != 1 {
			t.Fatalf("precondition: %d upstream calls, want 1 — nothing was cached to preserve", n)
		}

		// A record edit's reload: the table is rebuilt and reinstalled, and it is
		// identical to the one already installed.
		mustAddZone(t, a, store.Zone{Name: "unrelated.example", Type: "primary", Enabled: true})
		mustReloadZones(t, a)

		if got := askApp(t, a, "vpn.corp.example"); got != "10.0.0.1" {
			t.Errorf("answered %q, want the zone's own upstream", got)
		}
		if n := corpHits.Load(); n != 1 {
			t.Errorf("the zone's upstream was asked %d times, want 1 — a reload that changed no routing threw its cached answers away", n)
		}
	})
}

// The third way a route set changes, after added and removed: altered. An
// operator retargets a forwarder zone at a different resolver — the suffix is
// claimed before and after, by the same zone — and every answer cached from
// the old one has to go with it. In a split-horizon move that is the whole
// point of the edit: the old resolver's view is the one being replaced.
func TestRetargetingAForwarderZoneDropsWhatTheOldUpstreamAnswered(t *testing.T) {
	forEachDriver(t, func(t *testing.T, driver string) {
		before := mockDNS(t, answerA("10.0.0.1"))
		after := mockDNS(t, answerA("10.0.0.2"))
		pub := mockDNS(t, answerA("5.6.7.8"))

		a := newTestAppOn(t, driver, withUpstreams(pub))
		id := mustAddZone(t, a, store.Zone{
			Name: "corp.example", Type: "forwarder", Enabled: true, ForwardTo: before,
		})
		mustReloadZones(t, a)
		if got := askApp(t, a, "vpn.corp.example"); got != "10.0.0.1" {
			t.Fatalf("precondition: %q, want the zone's first upstream", got)
		}

		z := mustZone(t, a, id)
		z.ForwardTo = after
		if err := a.Store().Zones().UpdateZone(context.Background(), z); err != nil {
			t.Fatalf("UpdateZone: %v", err)
		}
		mustReloadZones(t, a)

		if got := askApp(t, a, "vpn.corp.example"); got != "10.0.0.2" {
			t.Errorf("after retargeting, answered %q, want the zone's new upstream — the old one's answer is still cached", got)
		}
	})
}

// W3: the singleflight key is route-blind, so a query issued *entirely after*
// the claim can still be answered by one that went out before it.
//
// This is not the residual window the purge leaves. The follower here has
// never read the cache at the moment routing changed — it arrives afterwards,
// misses the now-empty cache, and is collapsed onto a leader that is still
// out at the *default* upstreams from before the claim existed. It is handed
// that leader's public answer for a name a split-horizon zone now claims.
// Nothing is stored (the epoch guard refuses the put), so it is transient —
// and it is one upstream round trip wide, up to the forwarder's 2s timeout,
// rather than one cache lookup.
//
// The fix is to key the singleflight on the epoch too, so a purge ends the
// group as well as the entries: a query arriving after it cannot join a
// lookup dispatched before it.
func TestAQueryIssuedAfterTheClaimIsNotAnsweredByALookupSentBeforeIt(t *testing.T) {
	forEachDriver(t, func(t *testing.T, driver string) {
		release := make(chan struct{})
		var pubHits, corpHits atomic.Int64
		pub := mockDNSCounting(t, &pubHits, func(w dns.ResponseWriter, m *dns.Msg) {
			<-release // held at the default upstream, from before the claim
			answerA("5.6.7.8")(w, m)
		})
		corp := mockDNSCounting(t, &corpHits, answerA("10.0.0.1"))

		a := newTestAppOn(t, driver, withUpstreams(pub))

		// The leader: dispatched to the default upstreams while nothing claims
		// the suffix, and still out there when the claim lands.
		leaderDone := make(chan string, 1)
		go func() { leaderDone <- digAQuiet(a.DNSAddr(), "www.corp.example") }()
		// The sample, guaranteed rather than slept for: the leader is inside the
		// upstream's handler, so it is genuinely in flight and genuinely
		// pre-claim.
		waitFor(t, "the leader to reach the default upstream", func() bool { return pubHits.Load() == 1 })

		mustAddZone(t, a, store.Zone{
			Name: "corp.example", Type: "forwarder", Enabled: true, ForwardTo: corp,
		})
		mustReloadZones(t, a)

		// The follower: issued after the claim is installed and the purge has
		// run, against an empty cache.
		followerDone := make(chan string, 1)
		go func() { followerDone <- digAQuiet(a.DNSAddr(), "www.corp.example") }()

		// Long enough for the follower to have reached the cache and joined the
		// leader's group if it was going to. The leader is still blocked
		// throughout — nothing has closed release — so this cannot accidentally
		// measure a follower that simply arrived late.
		var follower string
		select {
		case follower = <-followerDone:
		case <-time.After(500 * time.Millisecond):
		}
		close(release)
		if follower == "" {
			follower = <-followerDone
		}
		<-leaderDone

		if follower != "10.0.0.1" {
			t.Errorf("a query issued after the claim answered %q, want the zone's own upstream — "+
				"it was collapsed onto a lookup dispatched to the default upstreams before the suffix was claimed", follower)
		}
		if corpHits.Load() == 0 {
			t.Error("the zone's own upstream was never asked: the query never left the singleflight group it joined")
		}
	})
}

// The other side of the rule its sibling above pins, and the reason "a
// claimed suffix SERVFAILs" is not an absolute.
//
// Once a claimed suffix has answers of its own in the cache, an upstream
// failure serves those *stale* — NOERROR, TTL 30 — exactly as it would for
// any other forwarded name. That is not the §9.11.5 leak and must not be
// "fixed": the answer is the zone's own data going stale, produced by the
// upstreams the operator named, never the public internet's. The purge
// covers entries the *defaults* produced; it has nothing to say about these.
//
// Pinned rather than left to the docs because both directions are one line
// away. Make a claimed suffix refuse serve-stale and a corporate resolver's
// reboot takes the whole suffix down for the length of it; make the purge
// unconditional and every routing-neutral edit throws these away.
func TestAClaimedSuffixStillServesItsOwnAnswersStale(t *testing.T) {
	forEachDriver(t, func(t *testing.T, driver string) {
		var dead atomic.Bool
		corp := mockDNS(t, func(w dns.ResponseWriter, m *dns.Msg) {
			if dead.Load() {
				r := new(dns.Msg)
				r.SetRcode(m, dns.RcodeServerFailure)
				_ = w.WriteMsg(r)
				return
			}
			answerA("10.0.0.1")(w, m)
		})
		pub := mockDNS(t, answerA("5.6.7.8"))

		// max_ttl 1 ages the entry out in a second; serve_stale_for keeps its
		// default, so it is stale and still very much servable.
		a := newTestAppOn(t, driver, withUpstreams(pub), withSetting("cache.max_ttl", "1"))
		mustAddZone(t, a, store.Zone{
			Name: "corp.example", Type: "forwarder", Enabled: true, ForwardTo: corp,
		})
		mustReloadZones(t, a)

		if got := askApp(t, a, "vpn.corp.example"); got != "10.0.0.1" {
			t.Fatalf("precondition: %q, want the zone's own upstream — nothing was cached to go stale", got)
		}
		dead.Store(true)
		time.Sleep(1300 * time.Millisecond)

		m := askAppMsg(t, a, "vpn.corp.example")
		if m.Rcode != dns.RcodeSuccess || len(m.Answer) != 1 {
			t.Fatalf("rcode = %s, answer = %v; want NOERROR and the zone's own stale answer",
				dns.RcodeToString[m.Rcode], m.Answer)
		}
		if a, ok := m.Answer[0].(*dns.A); !ok || a.A.String() != "10.0.0.1" {
			t.Errorf("answered %v, want the zone's own upstream's answer served stale", m.Answer[0])
		}
		// The distinguishing fact: it is *stale*, not a fresh re-fetch — the
		// upstream is refusing every query by now.
		if ttl := m.Answer[0].Header().Ttl; ttl != 30 {
			t.Errorf("TTL %d, want 30 — a stale answer is served at 30, so this was not the stale path", ttl)
		}
	})
}

// blockingSOAPrimary is a primary that receives a query and never answers
// it: it reports the arrival on the returned channel and holds the handler
// until the test ends. It is how a test catches the work an inbound NOTIFY
// starts *while it is still running*, rather than racing it.
func blockingSOAPrimary(t *testing.T) (string, chan struct{}) {
	t.Helper()
	probed := make(chan struct{}, 4)
	release := make(chan struct{})
	addr := mockDNS(t, func(dns.ResponseWriter, *dns.Msg) {
		select {
		case probed <- struct{}{}:
		default:
		}
		<-release
	})
	// Registered *after* mockDNS's own cleanup so it runs before it:
	// dns.Server.Shutdown waits for its in-flight handlers, and a handler
	// still parked on release would hang the test binary rather than fail
	// the test.
	t.Cleanup(func() { close(release) })
	return addr, probed
}

// logWatcher closes seen the first time a record's message contains msg.
// A channel rather than a slice because the assertion below is about
// *ordering* — what had already happened when Shutdown returned — and a
// slice read afterwards cannot say that.
type logWatcher struct {
	msg  string
	once sync.Once
	seen chan struct{}
}

func (h *logWatcher) Enabled(context.Context, slog.Level) bool { return true }

func (h *logWatcher) Handle(_ context.Context, r slog.Record) error {
	if strings.Contains(r.Message, h.msg) {
		h.once.Do(func() { close(h.seen) })
	}
	return nil
}

func (h *logWatcher) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *logWatcher) WithGroup(string) slog.Handler      { return h }

// watchLogs installs h as the default logger for one test.
func watchLogs(t *testing.T, msg string) *logWatcher {
	t.Helper()
	h := &logWatcher{msg: msg, seen: make(chan struct{})}
	prev := slog.Default()
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return h
}

// An inbound NOTIFY replies first and works afterwards (RFC 1996 §4.7), so
// the work runs in a goroutine of its own under a context the request's
// cancellation cannot reach. That goroutine used to be tracked by nothing:
// a NOTIFY admitted moments before shutdown could still be probing a primary
// or installing a transferred zone while Shutdown closed the store under it.
//
// This is the ordering test, and it belongs here because this is where the
// lifetime is owned: App.Shutdown cancels runCtx, waits on App.wg, and only
// then closes the store. NotifyServer.Run is what puts the work goroutine
// inside that wait, and dropping it from a.bg is the production change this
// test exists to fail on.
//
// The primary never answers the SOA probe, so the work is reliably in flight
// when Shutdown is called; the log line act writes on its way out is the
// signal that the goroutine has finished, and it is written before wg.Done,
// so "seen by the time Shutdown returned" is exactly the property.
func TestShutdownWaitsForTheWorkAnInboundNotifyStarted(t *testing.T) {
	forEachDriver(t, func(t *testing.T, driver string) {
		primary, probed := blockingSOAPrimary(t)
		a := newTestAppOn(t, driver)

		const apex = "notified.example"
		mustAddZone(t, a, store.Zone{
			Name: apex, Type: "secondary", Enabled: true, Primaries: primary,
			SOANS: "ns1." + apex, SOAMbox: "hostadmin." + apex,
			SOASerial: 1, SOARefresh: 3600, SOARetry: 600, SOAExpire: 604800,
			SOAMinimum: 300, SOATTL: 900,
			// Non-zero, so the NOTIFY takes the probe path rather than the
			// never-transferred shortcut — and far from due, so the refresh
			// scheduler has no reason to touch this zone during the test.
			RefreshedAt: time.Now().UnixMilli(),
		})
		mustReloadZones(t, a)

		watcher := watchLogs(t, "SOA probe after a notify failed")

		m := new(dns.Msg).SetNotify(dns.Fqdn(apex))
		reply, _, err := new(dns.Client).Exchange(m, a.DNSAddr())
		if err != nil {
			t.Fatalf("NOTIFY: %v", err)
		}
		if reply.Rcode != dns.RcodeSuccess {
			t.Fatalf("NOTIFY answered %s, want NOERROR", dns.RcodeToString[reply.Rcode])
		}
		select {
		case <-probed:
		case <-time.After(5 * time.Second):
			t.Fatal("the NOTIFY was answered but no SOA probe reached the primary")
		}

		if err := a.Shutdown(context.Background()); err != nil {
			t.Fatalf("Shutdown: %v", err)
		}
		select {
		case <-watcher.seen:
		default:
			t.Fatal("Shutdown returned while the goroutine an admitted NOTIFY started was still probing a primary — " +
				"its lifetime is outside App.wg, so the store closes underneath it")
		}
	})
}

// applySettings rebuilds the forwarder on every settings write and must
// retire the one it displaces.
//
// swappable.set returns the displaced Forwarder and has exactly one call
// site. A second call site that dropped that return value would leak up to
// four pooled TLS connections per upstream, per save, with nothing failing —
// the symptom arrives weeks later as "too many open files". Nothing asserted
// this: app_test drives a live settings change, so the path executes, but
// the close itself was unobserved.
func TestApplySettingsClosesTheForwarderItDisplaces(t *testing.T) {
	ctx := context.Background()
	a := newTestApp(t, withUpstreams(mockDNS(t, answerA("5.6.7.8"))))

	old := a.fwd.forwarder()
	if old == nil {
		t.Fatal("no forwarder was ever installed")
	}
	if old.Closed() {
		t.Fatal("the live forwarder is already closed")
	}

	// SetInternal, not Set: this drives applySettings directly rather than
	// waking the watcher, so the assertion runs after exactly one swap.
	if err := a.Store().Settings().SetInternal(ctx, "upstreams", mockDNS(t, answerA("6.6.6.6"))); err != nil {
		t.Fatalf("SetInternal(upstreams): %v", err)
	}
	a.applySettings(ctx)

	if a.fwd.forwarder() == old {
		t.Fatal("the forwarder was not replaced, so there was nothing to close")
	}
	if !old.Closed() {
		t.Error("the displaced forwarder was never closed: every settings save leaks its pooled connections")
	}
}

// Rung three of the applySettings ladder — the hardcoded plaintext defaults
// — had never executed under test.
//
// Before this milestone it was unreachable for upstream reasons at all:
// parsing an upstream could not fail, so buildForwarder only failed on a bad
// strategy string, which rung two already handles (see
// TestApplySettingsFallsBackWhenNoForwarderYet). ParseUpstreams can now
// reject, which makes the rung reachable *because of the upstreams value
// itself*, and its defaults are 1.1.1.1:53,1.0.0.1:53,9.9.9.9:53 — plaintext.
//
// So an operator whose stored value asks for DNS-over-TLS can end up with
// every query in the clear against public resolvers, with the settings page
// still showing the encrypted value and an ERROR log as the only signal.
// The choice is to keep resolving; the requirement is that it is visible.
func TestApplySettingsRecordsAnEncryptionDowngradeOnTheLastRung(t *testing.T) {
	ctx := context.Background()
	// Parses as far as the scheme and no further: missing "#name". Nothing
	// dials here — the defaults' forwarder is built, not used.
	a := newTestApp(t, withUpstreams("tls://1.1.1.1:853"))

	if !a.fwd.isSet() {
		t.Fatal("no forwarder installed: the ladder did not reach its last rung")
	}
	active, reason := a.UpstreamDowngrade()
	if !active {
		t.Fatal("the server is resolving in the clear against the default resolvers and says nothing about it")
	}
	if !strings.Contains(reason, "tls://1.1.1.1:853") {
		t.Errorf("reason = %q, want the rejected entry named", reason)
	}

	// And it clears when the operator fixes the setting. A warning that
	// survives the fix is worse than no warning.
	if err := a.Store().Settings().SetInternal(ctx, "upstreams", "tls://1.1.1.1:853#cloudflare-dns.com"); err != nil {
		t.Fatalf("SetInternal(upstreams): %v", err)
	}
	a.applySettings(ctx)
	if active, reason := a.UpstreamDowngrade(); active {
		t.Errorf("still downgraded after a successful apply with encrypted upstreams: %q", reason)
	}
}

// The same rung with a *plaintext* stored value is not a downgrade: falling
// from one set of plaintext resolvers to another takes nothing away, and
// warning about it would spend the operator's attention on a non-event.
func TestApplySettingsDoesNotCallAPlaintextFallbackADowngrade(t *testing.T) {
	// "#name" has no meaning on a plain entry, so this reaches the same rung.
	a := newTestApp(t, withUpstreams("1.1.1.1:53#cloudflare-dns.com"))

	if !a.fwd.isSet() {
		t.Fatal("no forwarder installed: the ladder did not reach its last rung")
	}
	if active, reason := a.UpstreamDowngrade(); active {
		t.Errorf("a plaintext value that would not parse was reported as an encryption downgrade: %q", reason)
	}
}
