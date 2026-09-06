package upstream

import (
	"context"
	"fmt"
	"maps"
	"net"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aloks98/dnsaur/internal/dnssrv"
	"github.com/miekg/dns"
)

// listenPair binds a UDP socket and a TCP listener on the same 127.0.0.1
// port, and reports how many port numbers it had to step over to get one.
//
// Binding UDP :0 and then TCP on whatever port the kernel handed back is not
// enough, which is what this exists to fix. Linux allocates UDP and TCP
// ephemeral ports from independent ranges and a UDP bind reserves nothing on
// TCP, so the TCP bind fails outright whenever anything else already holds
// that port number — including an earlier mockUpstream in the same run.
// Measured at one reddened package per 25 sequential runs of this one, and
// this milestone took the call sites from 12 to 26.
//
// So the pair is retried rather than assumed. The retry count is returned
// rather than swallowed because a retry that never fires is untested: it is
// how TestListenPairRetriesPastAPortHeldOnTCP knows it met a real collision
// instead of passing on a run that happened not to.
func listenPair() (net.PacketConn, net.Listener, int, error) {
	const maxTries = 100
	for try := range maxTries {
		pc, err := net.ListenPacket("udp", "127.0.0.1:0")
		if err != nil {
			return nil, nil, try, err
		}
		ln, err := net.Listen("tcp", pc.LocalAddr().String())
		if err == nil {
			return pc, ln, try, nil
		}
		_ = pc.Close()
	}
	return nil, nil, maxTries, fmt.Errorf("no port free on both udp and tcp in %d tries", maxTries)
}

// The retry above is only load-bearing when it actually fires, so this
// guarantees the collision rather than asserting on a run that may never have
// met one: a block of TCP ports is held open, and pairs are bound until one
// of them steps over a held port. At ~256 held ports out of the ephemeral
// range a try collides on the order of 1% of the time, so the loop bound is
// four orders of magnitude past what is needed and a run that reaches it is a
// broken fixture, not a rare one.
//
// Drop the retry (maxTries = 1) and this fails on the first collision with
// exactly the bind error the flake reported.
func TestListenPairRetriesPastAPortHeldOnTCP(t *testing.T) {
	for range 256 {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = ln.Close() })
	}
	stepped := 0
	for i := 0; stepped == 0 && i < 20000; i++ {
		pc, ln, retries, err := listenPair()
		if err != nil {
			t.Fatalf("listenPair gave up after %d collisions on try %d: %v", retries, i, err)
		}
		stepped += retries
		_ = pc.Close()
		_ = ln.Close()
	}
	if stepped == 0 {
		t.Fatal("no bind ever collided with a held TCP port: this proved nothing about the retry")
	}
}

// mockUpstream runs a real DNS server on 127.0.0.1:0, on UDP and TCP alike.
func mockUpstream(t *testing.T, handler dns.HandlerFunc) string {
	t.Helper()
	pc, ln, _, err := listenPair()
	if err != nil {
		t.Fatal(err)
	}
	u := &dns.Server{PacketConn: pc, Handler: handler}
	s := &dns.Server{Listener: ln, Handler: handler}
	go func() { _ = u.ActivateAndServe() }()
	go func() { _ = s.ActivateAndServe() }()
	t.Cleanup(func() { _ = u.Shutdown(); _ = s.Shutdown() })
	return pc.LocalAddr().String()
}

func answerA(ip string) dns.HandlerFunc {
	return func(w dns.ResponseWriter, m *dns.Msg) {
		r := new(dns.Msg)
		r.SetReply(m)
		rr, _ := dns.NewRR(m.Question[0].Name + " 300 IN A " + ip)
		r.Answer = []dns.RR{rr}
		_ = w.WriteMsg(r)
	}
}

func req(name string) *dnssrv.Request {
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(name), dns.TypeA)
	return &dnssrv.Request{Msg: m}
}

func TestForwardSuccessAndCaseRestore(t *testing.T) {
	addr := mockUpstream(t, answerA("5.6.7.8"))
	f, err := New(Config{Upstreams: []string{addr}, Strategy: "failover"})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := f.Handler().ServeDNS(context.Background(), req("MiXeD.Example.COM"))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Decision != dnssrv.DecisionForwarded || resp.Upstream != addr {
		t.Fatalf("%+v", resp)
	}
	if resp.Msg.Question[0].Name != "MiXeD.Example.COM." {
		t.Fatalf("question case not restored: %s", resp.Msg.Question[0].Name)
	}
}

func TestFailoverSkipsDeadUpstream(t *testing.T) {
	good := mockUpstream(t, answerA("5.6.7.8"))
	f, _ := New(Config{Upstreams: []string{"127.0.0.1:1", good}, Strategy: "failover", Timeout: 200 * time.Millisecond})
	resp, err := f.Handler().ServeDNS(context.Background(), req("x.test"))
	if err != nil || resp.Upstream != good {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}
}

func TestTruncationFallsBackToTCP(t *testing.T) {
	addr := mockUpstream(t, func(w dns.ResponseWriter, m *dns.Msg) {
		r := new(dns.Msg)
		r.SetReply(m)
		if _, isUDP := w.RemoteAddr().(*net.UDPAddr); isUDP {
			r.Truncated = true
		} else {
			rr, _ := dns.NewRR(m.Question[0].Name + " 300 IN A 5.6.7.8")
			r.Answer = []dns.RR{rr}
		}
		_ = w.WriteMsg(r)
	})
	f, _ := New(Config{Upstreams: []string{addr}, Strategy: "failover"})
	resp, err := f.Handler().ServeDNS(context.Background(), req("x.test"))
	if err != nil || len(resp.Msg.Answer) != 1 {
		t.Fatalf("tcp fallback failed: %+v %v", resp, err)
	}
}

func TestSpoofedCaseRejected(t *testing.T) {
	addr := mockUpstream(t, func(w dns.ResponseWriter, m *dns.Msg) {
		r := new(dns.Msg)
		r.SetReply(m)
		r.Question[0].Name = "spoofed.example.com." // fails 0x20 echo check
		_ = w.WriteMsg(r)
	})
	f, _ := New(Config{Upstreams: []string{addr}, Strategy: "failover", Timeout: 300 * time.Millisecond})
	if _, err := f.Handler().ServeDNS(context.Background(), req("real.example.com")); err == nil {
		t.Fatal("spoofed response accepted")
	}
}

func TestConditionalRouting(t *testing.T) {
	corp := mockUpstream(t, answerA("10.0.0.1"))
	pub := mockUpstream(t, answerA("5.6.7.8"))
	f, _ := New(Config{Upstreams: []string{pub}, Strategy: "failover", Conditional: map[string][]string{"corp.example": {corp}}})
	resp, _ := f.Handler().ServeDNS(context.Background(), req("vpn.corp.example"))
	if resp.Upstream != corp {
		t.Fatalf("conditional missed: %+v", resp)
	}
	resp, _ = f.Handler().ServeDNS(context.Background(), req("other.example"))
	if resp.Upstream != pub {
		t.Fatalf("default missed: %+v", resp)
	}
}

func TestFailureCacheShortCircuits(t *testing.T) {
	var hits atomic.Int64
	dead := mockUpstream(t, func(w dns.ResponseWriter, m *dns.Msg) {
		hits.Add(1) // count then never answer usefully
		r := new(dns.Msg)
		r.SetRcode(m, dns.RcodeServerFailure)
		_ = w.WriteMsg(r)
	})
	f, _ := New(Config{Upstreams: []string{dead}, Strategy: "failover", Timeout: 200 * time.Millisecond})
	h := f.Handler()
	_, _ = h.ServeDNS(context.Background(), req("down.test"))
	before := hits.Load()
	_, _ = h.ServeDNS(context.Background(), req("down.test")) // within 30s failure cache
	if hits.Load() != before {
		t.Fatalf("failure cache did not short-circuit: %d -> %d", before, hits.Load())
	}
}

// TestConditionalRoutingLongestSuffixWins guards against non-deterministic
// suffix routing: with overlapping conditional suffixes, the most-specific
// one must win every time, not flip based on Go's randomized map iteration
// order.
func TestConditionalRoutingLongestSuffixWins(t *testing.T) {
	corp := mockUpstream(t, answerA("10.0.0.1"))
	vpn := mockUpstream(t, answerA("10.0.0.2"))
	pub := mockUpstream(t, answerA("5.6.7.8"))
	f, err := New(Config{
		Upstreams: []string{pub},
		Strategy:  "failover",
		Conditional: map[string][]string{
			"corp.example":     {corp},
			"vpn.corp.example": {vpn},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	h := f.Handler()
	for i := 0; i < 20; i++ {
		resp, err := h.ServeDNS(context.Background(), req("vpn.corp.example"))
		if err != nil || resp.Upstream != vpn {
			t.Fatalf("iteration %d: expected most-specific match %s, got %+v err=%v", i, vpn, resp, err)
		}
	}
	resp, err := h.ServeDNS(context.Background(), req("other.corp.example"))
	if err != nil || resp.Upstream != corp {
		t.Fatalf("expected %s, got %+v err=%v", corp, resp, err)
	}
}

// TestFailureCacheExpiresAndPrunes verifies that a failCache entry is
// honored while live, but once its 30s TTL has passed (per the Forwarder's
// injected clock), the entry is deleted and the upstream is retried again.
// The mock fails only on the first call and succeeds afterward, so a
// failCache entry still present after the second ServeDNS call could only
// mean the retry never happened (short-circuited) or the prune-on-read
// path didn't run.
func TestFailureCacheExpiresAndPrunes(t *testing.T) {
	var calls atomic.Int64
	dead := mockUpstream(t, func(w dns.ResponseWriter, m *dns.Msg) {
		n := calls.Add(1)
		r := new(dns.Msg)
		r.SetReply(m)
		if n == 1 {
			r.Rcode = dns.RcodeServerFailure
		} else {
			rr, _ := dns.NewRR(m.Question[0].Name + " 300 IN A 5.6.7.8")
			r.Answer = []dns.RR{rr}
		}
		_ = w.WriteMsg(r)
	})
	f, err := New(Config{Upstreams: []string{dead}, Strategy: "failover", Timeout: 200 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	fake := time.Now()
	f.now = func() time.Time { return fake }
	h := f.Handler()

	_, _ = h.ServeDNS(context.Background(), req("expire.test"))
	if calls.Load() != 1 {
		t.Fatalf("expected 1 upstream call after initial failure, got %d", calls.Load())
	}

	f.fmu.Lock()
	_, cached := f.failCache[failKey{"expire.test", dns.TypeA}]
	f.fmu.Unlock()
	if !cached {
		t.Fatal("expected failure to populate failCache")
	}

	fake = fake.Add(31 * time.Second) // advance past the 30s TTL
	resp, err := h.ServeDNS(context.Background(), req("expire.test"))
	if err != nil || resp.Upstream != dead {
		t.Fatalf("expected retry to succeed against upstream: resp=%+v err=%v", resp, err)
	}
	if calls.Load() != 2 {
		t.Fatalf("expected upstream to be retried after failure cache expiry, calls=%d", calls.Load())
	}

	f.fmu.Lock()
	_, stillCached := f.failCache[failKey{"expire.test", dns.TypeA}]
	f.fmu.Unlock()
	if stillCached {
		t.Fatal("expired failCache entry was not pruned")
	}
}

func TestNewRejectsUnknownStrategy(t *testing.T) {
	_, err := New(Config{Upstreams: []string{"127.0.0.1:53"}, Strategy: "bogus"})
	if err == nil {
		t.Fatal("expected error for unknown strategy")
	}
}

// TestMarkResultPenalizesFailingUpstreamEwma is a direct, deterministic
// unit test of the ewma cold-start fix: on failure, ewmaMicro must be
// clamped up to at least the configured timeout (not left at its
// cold-start/reset value of 0), so a failing upstream can't keep winning
// "fastest" sorts forever. A later fast success must still be able to pull
// ewma back down, i.e. the clamp isn't a permanent floor.
func TestMarkResultPenalizesFailingUpstreamEwma(t *testing.T) {
	u := newUp(Upstream{Scheme: SchemePlain, Addr: "127.0.0.1:1", Canonical: "127.0.0.1:1"}, 100*time.Millisecond)
	timeoutMicro := (100 * time.Millisecond).Microseconds()

	u.markResult(false, 0, time.Now(), timeoutMicro)
	if got := u.ewmaMicro.Load(); got < timeoutMicro {
		t.Fatalf("expected ewma clamped to >= %d micros after failure, got %d", timeoutMicro, got)
	}

	u.markResult(true, 5*time.Millisecond, time.Now(), timeoutMicro)
	if got := u.ewmaMicro.Load(); got >= timeoutMicro {
		t.Fatalf("expected ewma to move back down after a fast success, got %d (timeout=%d)", got, timeoutMicro)
	}
}

// The table can be replaced after construction, and the new routes take
// effect for subsequent queries.
func TestSetConditionalReplacesRoutes(t *testing.T) {
	corp := mockUpstream(t, answerA("10.0.0.1"))
	other := mockUpstream(t, answerA("10.0.0.2"))
	pub := mockUpstream(t, answerA("5.6.7.8"))

	f, err := New(Config{Upstreams: []string{pub}, Strategy: "failover"})
	if err != nil {
		t.Fatal(err)
	}
	h := f.Handler()

	// Fails on the error rather than dereferencing a nil response: a
	// regression that returns (nil, err) must report which step broke, not
	// panic.
	routedTo := func(stage string) string {
		t.Helper()
		resp, err := h.ServeDNS(context.Background(), req("vpn.corp.example"))
		if err != nil {
			t.Fatalf("%s: ServeDNS: %v", stage, err)
		}
		return resp.Upstream
	}

	// The route itself, not merely which upstream answered. Under failover
	// the first candidate answers and resp.Upstream reports it, so a
	// replacement that *merged* -- new upstreams first, the outgoing ones
	// appended behind them -- serves every query from the new one and is
	// invisible to routedTo. It is not invisible to a zone: the old
	// nameserver stays a fallback the operator has already removed, and it
	// answers the moment the new one is unhealthy. The addresses are compared
	// in order because the order is what the strategies consume.
	routeFor := func(stage, suffix string) []string {
		t.Helper()
		tbl := f.condTableLoad()
		if tbl == nil {
			t.Fatalf("%s: no conditional table at all", stage)
		}
		ups, ok := tbl.routes[suffix]
		if !ok {
			t.Fatalf("%s: no route for %s; table has %v", stage, suffix, slices.Sorted(maps.Keys(tbl.routes)))
		}
		addrs := make([]string, 0, len(ups))
		for _, u := range ups {
			addrs = append(addrs, u.addr)
		}
		return addrs
	}

	// No conditional routes yet: the default answers.
	if got := routedTo("before SetConditional"); got != pub {
		t.Fatalf("before SetConditional: upstream = %s, want the default %s", got, pub)
	}

	if err := f.SetConditional(map[string][]string{"corp.example": {corp}}); err != nil {
		t.Fatalf("SetConditional: %v", err)
	}
	if got := routedTo("after SetConditional"); got != corp {
		t.Fatalf("after SetConditional: upstream = %s, want %s", got, corp)
	}
	if got := routeFor("after SetConditional", "corp.example"); !slices.Equal(got, []string{corp}) {
		t.Fatalf("after SetConditional: route = %v, want exactly [%s]", got, corp)
	}

	// Replacing the table re-routes; the old route is gone, not merged.
	if err := f.SetConditional(map[string][]string{"corp.example": {other}}); err != nil {
		t.Fatalf("second SetConditional: %v", err)
	}
	if got := routedTo("after replacement"); got != other {
		t.Fatalf("after replacement: upstream = %s, want %s", got, other)
	}
	if got := routeFor("after replacement", "corp.example"); !slices.Equal(got, []string{other}) {
		t.Fatalf("after replacement: route = %v, want exactly [%s] — %s is gone, not demoted behind it", got, other, corp)
	}

	// An empty table releases the suffix back to the defaults.
	if err := f.SetConditional(nil); err != nil {
		t.Fatalf("clearing SetConditional: %v", err)
	}
	if got := routedTo("after clearing"); got != pub {
		t.Fatalf("after clearing: upstream = %s, want the default %s", got, pub)
	}
	if tbl := f.condTableLoad(); tbl != nil {
		t.Errorf("after clearing: the conditional table is still installed with %v", slices.Sorted(maps.Keys(tbl.routes)))
	}
}

// A swap must not disturb the default upstreams' health state. This is the
// whole reason SetConditional exists instead of a rebuild.
func TestSetConditionalPreservesDefaultUpstreamState(t *testing.T) {
	pub := mockUpstream(t, answerA("5.6.7.8"))
	f, err := New(Config{Upstreams: []string{pub}, Strategy: "failover"})
	if err != nil {
		t.Fatal(err)
	}
	// Give the default upstream some recorded latency by using it.
	if _, err := f.Handler().ServeDNS(context.Background(), req("a.example")); err != nil {
		t.Fatal(err)
	}
	before := f.def[0].ewmaMicro.Load()
	if before == 0 {
		t.Fatal("no latency recorded, so this test cannot detect a reset")
	}

	if err := f.SetConditional(map[string][]string{"corp.example": {pub}}); err != nil {
		t.Fatalf("SetConditional: %v", err)
	}
	if after := f.def[0].ewmaMicro.Load(); after != before {
		t.Errorf("default upstream ewma changed across a swap: %d -> %d", before, after)
	}
}

// A conditional upstream that survives a swap keeps its own health state too.
// Without reuse, the swap fixes the problem for the defaults and leaves it for
// exactly the upstreams a zone edit is most likely to be about.
func TestSetConditionalReusesUpstreamsByAddress(t *testing.T) {
	corp := mockUpstream(t, answerA("10.0.0.1"))
	pub := mockUpstream(t, answerA("5.6.7.8"))
	f, err := New(Config{Upstreams: []string{pub}, Strategy: "failover"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.SetConditional(map[string][]string{"corp.example": {corp}}); err != nil {
		t.Fatal(err)
	}
	// Use the conditional route so it records latency.
	if _, err := f.Handler().ServeDNS(context.Background(), req("vpn.corp.example")); err != nil {
		t.Fatal(err)
	}
	first := f.condTableLoad().routes["corp.example"][0]
	if first.ewmaMicro.Load() == 0 {
		t.Fatal("no latency recorded on the conditional upstream")
	}

	// A swap that still names the same address must keep the same *up. The
	// new suffix is the discriminating half: reuse keyed by suffix rather
	// than by address would satisfy corp.example -- whose key and address
	// list are both unchanged -- while silently minting a fresh *up for
	// vpn.corp.example, which is exactly what a zone edit that adds, renames
	// or re-addresses a suffix looks like.
	if err := f.SetConditional(map[string][]string{
		"corp.example":     {corp},
		"vpn.corp.example": {corp},
	}); err != nil {
		t.Fatal(err)
	}
	tbl := f.condTableLoad()
	if second := tbl.routes["corp.example"][0]; second != first {
		t.Errorf("conditional upstream %s was rebuilt across a swap, losing its health state", corp)
	}
	if adopted := tbl.routes["vpn.corp.example"][0]; adopted != first {
		t.Errorf("new suffix did not adopt the existing *up for %s by address: it starts with a blank ewma, no failure count and no backoff window", corp)
	}
}

// Longest-suffix routing still wins after a swap, deterministically.
func TestSetConditionalKeepsLongestSuffixWins(t *testing.T) {
	corp := mockUpstream(t, answerA("10.0.0.1"))
	vpn := mockUpstream(t, answerA("10.0.0.2"))
	pub := mockUpstream(t, answerA("5.6.7.8"))
	f, _ := New(Config{Upstreams: []string{pub}, Strategy: "failover"})
	if err := f.SetConditional(map[string][]string{
		"corp.example":     {corp},
		"vpn.corp.example": {vpn},
	}); err != nil {
		t.Fatal(err)
	}
	h := f.Handler()
	for i := 0; i < 20; i++ {
		resp, err := h.ServeDNS(context.Background(), req("vpn.corp.example"))
		if err != nil || resp.Upstream != vpn {
			t.Fatalf("iteration %d: want the most specific match %s, got %+v err=%v", i, vpn, resp, err)
		}
	}
}

// A swap during in-flight queries must not tear. The reader takes no lock, so
// this is the one place the concurrency has to be exercised rather than
// reasoned about -- which means the test has to prove the readers actually
// ran, not merely that it finished without complaint.
func TestSetConditionalIsSafeUnderConcurrentQueries(t *testing.T) {
	corp := mockUpstream(t, answerA("10.0.0.1"))
	pub := mockUpstream(t, answerA("5.6.7.8"))
	f, _ := New(Config{Upstreams: []string{pub}, Strategy: "failover"})
	h := f.Handler()

	stop := make(chan struct{})
	var attempts, failures atomic.Int64
	var wg sync.WaitGroup
	// awaitQuery below calls t.Fatal, which runs Goexit: the close and the
	// Wait at the end of the function are skipped, and four readers are left
	// querying a server t.Cleanup is about to shut down under them. Deferred
	// and idempotent, the readers are stopped on the failure path too, and
	// the explicit stop before the assertions still happens where it did.
	stopReaders := sync.OnceFunc(func() { close(stop); wg.Wait() })
	defer stopReaders()
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					if _, err := h.ServeDNS(context.Background(), req("vpn.corp.example")); err != nil {
						failures.Add(1)
					}
					attempts.Add(1)
				}
			}
		}()
	}

	// The writer loop below finishes in well under a millisecond and never
	// blocks, so at GOMAXPROCS=1 it runs to completion without ever yielding:
	// left to itself it closes stop before a single reader is scheduled, and
	// the test passes having exercised no concurrency at all. Waiting for a
	// query to complete between swaps forces the interleaving instead of
	// hoping for it. Gosched hands over a turn without creating a
	// happens-before edge, and the writer only ever *loads* attempts, so
	// nothing here orders a swap before a reader's load of f.cond -- the race
	// detector still sees that pair as unsynchronised.
	deadline := time.Now().Add(30 * time.Second)
	awaitQuery := func() {
		t.Helper()
		for target := attempts.Load() + 1; attempts.Load() < target; {
			if time.Now().After(deadline) {
				t.Fatal("readers completed no query: no read of f.cond ran against a swap, so this proved nothing")
			}
			runtime.Gosched()
		}
	}
	awaitQuery()

	for i := 0; i < 50; i++ {
		if err := f.SetConditional(map[string][]string{"corp.example": {corp}}); err != nil {
			t.Fatal(err)
		}
		awaitQuery()
		if err := f.SetConditional(nil); err != nil {
			t.Fatal(err)
		}
		awaitQuery()
	}
	stopReaders()

	if n := attempts.Load(); n == 0 {
		t.Fatal("no query ran against the swaps: this test proved nothing")
	}
	// A single failure writes a 30s failCache entry, and every later query for
	// this name then short-circuits before pick ever loads f.cond -- leaving
	// the readers spinning without touching the table under test.
	if n := failures.Load(); n != 0 {
		t.Fatalf("%d of %d queries failed: the failure cache now short-circuits pick, so the readers stopped exercising the swap", n, attempts.Load())
	}
}

// A suffix that is claimed but has no upstreams must SERVFAIL rather than fall
// through to the defaults -- the rule the forwarder/stub zone types rest on.
// Run on every strategy because "race" reaches an empty candidate set by a
// different path (a zero-capacity channel and no goroutines), so a wrong one
// would hang rather than fail.
func TestSetConditionalEmptyUpstreamsClaimTheSuffix(t *testing.T) {
	for _, strategy := range []string{"failover", "fastest", "race"} {
		t.Run(strategy, func(t *testing.T) {
			pub := mockUpstream(t, answerA("5.6.7.8"))
			f, err := New(Config{Upstreams: []string{pub}, Strategy: strategy})
			if err != nil {
				t.Fatal(err)
			}
			if err := f.SetConditional(map[string][]string{"corp.example": nil}); err != nil {
				t.Fatal(err)
			}

			type outcome struct {
				resp *dnssrv.Response
				err  error
			}
			done := make(chan outcome, 1)
			go func() {
				resp, err := f.Handler().ServeDNS(context.Background(), req("vpn.corp.example"))
				done <- outcome{resp, err}
			}()
			select {
			case got := <-done:
				if got.err == nil {
					t.Fatalf("claimed suffix with no upstreams fell through to the defaults: resp=%+v", got.resp)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("ServeDNS never returned: an empty candidate set hangs on this strategy")
			}
		})
	}
}

// One address named under two suffixes in a single call must yield one *up.
// Two would track that resolver's failure count and 15s backoff twice, so one
// suffix could keep hammering an upstream the other has already marked down --
// and several internal zones forwarded to the same corporate resolver is the
// ordinary configuration, not an exotic one.
func TestSetConditionalDedupesUpstreamsWithinOneCall(t *testing.T) {
	corp := mockUpstream(t, answerA("10.0.0.1"))
	pub := mockUpstream(t, answerA("5.6.7.8"))
	f, err := New(Config{Upstreams: []string{pub}, Strategy: "failover"})
	if err != nil {
		t.Fatal(err)
	}
	// Neither suffix exists in an outgoing table, so adoption cannot supply
	// the shared *up: the dedup has to happen within this one call.
	if err := f.SetConditional(map[string][]string{
		"corp.example": {corp},
		"lab.example":  {corp},
	}); err != nil {
		t.Fatal(err)
	}
	tbl := f.condTableLoad()
	if a, b := tbl.routes["corp.example"][0], tbl.routes["lab.example"][0]; a != b {
		t.Errorf("address %s got two distinct *up in one table: its failure count and 15s backoff are tracked twice", corp)
	}
}

// A swap must adopt from the table it replaces even when swaps overlap.
//
// SetConditional is a read-modify-write on live in-memory state: it Loads the
// outgoing table to adopt its *up values by address, builds the replacement,
// then Stores it. Two overlapping calls therefore both adopt from the *same*
// outgoing table, and the one that Stores first has its mints discarded by
// the other. That is a lost update, not a data race, so -race cannot see it —
// and what it loses is precisely what the adoption exists to keep: the
// upstream's latency EWMA, its failure count and its 15s backoff window.
//
// The caller does not serialise this. A zone reload reaches here from
// api.Server.reloadZones on every zone write, through App.ReloadZones, with
// no lock anywhere on the way, so two overlapping API writes are the ordinary
// case rather than a corner one. Resolver.Reload gets away with the same
// shape only because it derives its whole value from the store and never
// reads the previous snapshot.
//
// The shape that exposes it is a *mint* racing a swap, which is why every
// round starts from a table with no b.example in it: the first caller to
// build it has to mint a fresh *up, and every later caller must adopt that
// one. Serialised, exactly one *up for b.example exists across every table
// stored during a round. Unserialised, two callers read the same pre-mint
// table, mint one each, and both reach a stored table — which is what the
// pointer set below counts.
func TestSetConditionalAdoptsAcrossConcurrentSwaps(t *testing.T) {
	f, err := New(Config{Upstreams: []string{"127.0.0.1:1"}, Strategy: "failover"})
	if err != nil {
		t.Fatal(err)
	}

	// A wide table so the build between Load and Store lasts long enough for
	// an overlapping caller to get into it. With a two-entry map the window
	// is a few hundred nanoseconds and an unserialised implementation would
	// pass on luck rather than on correctness.
	base := map[string][]string{}
	for i := range 200 {
		base[fmt.Sprintf("z%d.example", i)] = []string{fmt.Sprintf("10.0.%d.%d:53", i/256, i%256)}
	}
	withB := map[string][]string{"b.example": {"10.9.9.9:53"}}
	maps.Copy(withB, base)

	for round := range 40 {
		// b.example is absent here, so the next swap has to mint it.
		if err := f.SetConditional(base); err != nil {
			t.Fatalf("round %d: reset: %v", round, err)
		}

		var mu sync.Mutex
		seen := map[*up]bool{}
		observe := func() {
			if ups := f.condTableLoad().routes["b.example"]; len(ups) == 1 {
				mu.Lock()
				seen[ups[0]] = true
				mu.Unlock()
			}
		}

		// A sampler, so a table that is stored and immediately replaced is
		// still counted: without it a round could miss the loser's table
		// entirely and read only the winner's.
		stop := make(chan struct{})
		var sampler sync.WaitGroup
		sampler.Add(1)
		go func() {
			defer sampler.Done()
			for {
				select {
				case <-stop:
					return
				default:
					observe()
				}
			}
		}()

		start := make(chan struct{})
		var wg sync.WaitGroup
		errs := make([]error, 4)
		for i := range 4 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				errs[i] = f.SetConditional(withB)
				observe()
			}()
		}
		close(start)
		wg.Wait()
		close(stop)
		sampler.Wait()

		for i, err := range errs {
			if err != nil {
				t.Fatalf("round %d: swapper %d: %v", round, i, err)
			}
		}
		mu.Lock()
		n := len(seen)
		mu.Unlock()
		if n != 1 {
			t.Fatalf("round %d: %d distinct upstreams for b.example reached a stored table, want 1 — an overlapping swap re-minted it, discarding the health state adoption exists to keep", round, n)
		}
	}
}
