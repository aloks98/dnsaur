package upstream

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aloks98/dnsaur/internal/dnssrv"
	"github.com/miekg/dns"
)

// mockUpstream runs a real DNS server on 127.0.0.1:0.
func mockUpstream(t *testing.T, handler dns.HandlerFunc) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", pc.LocalAddr().String())
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
	u := newUp("127.0.0.1:1", 100*time.Millisecond)
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
