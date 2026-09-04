package cache

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aloks98/dnsaur/internal/dnssrv"
	"github.com/miekg/dns"
)

type fakeClock struct{ t time.Time }

func (f *fakeClock) Now() time.Time { return f.t }

func answer(name string, ttl uint32, ip string) dnssrv.Handler {
	return dnssrv.HandlerFunc(func(ctx context.Context, req *dnssrv.Request) (*dnssrv.Response, error) {
		m := new(dns.Msg)
		m.SetReply(req.Msg)
		rr, _ := dns.NewRR(dns.Fqdn(name) + " 300 IN A " + ip)
		rr.Header().Ttl = ttl
		m.Answer = []dns.RR{rr}
		return &dnssrv.Response{Msg: m, Decision: dnssrv.DecisionForwarded}, nil
	})
}

func req(name string) *dnssrv.Request {
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(name), dns.TypeA)
	return &dnssrv.Request{Msg: m}
}

func TestHitDecrementsTTLAndSkipsUpstream(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	var calls atomic.Int64
	counted := dnssrv.HandlerFunc(func(ctx context.Context, r *dnssrv.Request) (*dnssrv.Response, error) {
		calls.Add(1)
		return answer("x.test", 300, "1.2.3.4").ServeDNS(ctx, r)
	})
	c := New(Options{Now: clk.Now})
	h := c.Middleware()(counted)

	if resp, _ := h.ServeDNS(context.Background(), req("x.test")); resp.Decision != dnssrv.DecisionForwarded {
		t.Fatal("first query should forward")
	}
	clk.t = clk.t.Add(100 * time.Second)
	resp, _ := h.ServeDNS(context.Background(), req("x.test"))
	if resp.Decision != dnssrv.DecisionCached {
		t.Fatalf("want cached, got %s", resp.Decision)
	}
	if ttl := resp.Msg.Answer[0].Header().Ttl; ttl != 200 {
		t.Fatalf("ttl %d want 200", ttl)
	}
	if calls.Load() != 1 {
		t.Fatalf("upstream calls %d", calls.Load())
	}
}

func TestExpiryRefetches(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	var calls atomic.Int64
	counted := dnssrv.HandlerFunc(func(ctx context.Context, r *dnssrv.Request) (*dnssrv.Response, error) {
		calls.Add(1)
		return answer("x.test", 60, "1.2.3.4").ServeDNS(ctx, r)
	})
	c := New(Options{Now: clk.Now})
	h := c.Middleware()(counted)
	_, _ = h.ServeDNS(context.Background(), req("x.test"))
	clk.t = clk.t.Add(61 * time.Second)
	_, _ = h.ServeDNS(context.Background(), req("x.test"))
	if calls.Load() != 2 {
		t.Fatalf("expired entry not refetched, calls=%d", calls.Load())
	}
}

func TestServeStaleOnUpstreamFailure(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	var fail atomic.Bool
	up := dnssrv.HandlerFunc(func(ctx context.Context, r *dnssrv.Request) (*dnssrv.Response, error) {
		if fail.Load() {
			return nil, errors.New("upstream down")
		}
		return answer("x.test", 60, "1.2.3.4").ServeDNS(ctx, r)
	})
	c := New(Options{Now: clk.Now})
	h := c.Middleware()(up)
	_, _ = h.ServeDNS(context.Background(), req("x.test"))
	fail.Store(true)
	clk.t = clk.t.Add(2 * time.Hour) // expired but within 24h stale window
	resp, err := h.ServeDNS(context.Background(), req("x.test"))
	if err != nil || resp.Decision != dnssrv.DecisionStale {
		t.Fatalf("want stale, got %v %v", resp, err)
	}
	if resp.Msg.Answer[0].Header().Ttl != 30 {
		t.Fatalf("stale ttl %d", resp.Msg.Answer[0].Header().Ttl)
	}
}

func TestNegativeCaching(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	var calls atomic.Int64
	up := dnssrv.HandlerFunc(func(ctx context.Context, r *dnssrv.Request) (*dnssrv.Response, error) {
		calls.Add(1)
		m := new(dns.Msg)
		m.SetRcode(r.Msg, dns.RcodeNameError)
		soa, _ := dns.NewRR("test. 3600 IN SOA a.test. b.test. 1 1 1 1 600")
		m.Ns = []dns.RR{soa}
		return &dnssrv.Response{Msg: m, Decision: dnssrv.DecisionForwarded}, nil
	})
	c := New(Options{Now: clk.Now})
	h := c.Middleware()(up)
	_, _ = h.ServeDNS(context.Background(), req("nope.test"))
	clk.t = clk.t.Add(30 * time.Second)
	resp, _ := h.ServeDNS(context.Background(), req("nope.test"))
	if resp.Decision != dnssrv.DecisionCached || resp.Msg.Rcode != dns.RcodeNameError {
		t.Fatalf("%+v", resp)
	}
	if calls.Load() != 1 {
		t.Fatalf("negative response not cached, calls=%d", calls.Load())
	}
}

// TestConcurrentMissCollapsesAndPreservesPerCallerId reproduces the collapsed
// singleflight failure path: N concurrent cold-cache queries for the same key
// hit an upstream that returns a real SERVFAIL response (Decision forwarded,
// err nil, no stale entry available). Every collapsed caller must get back its
// own copy of the response with ITS OWN request Id rewritten in — otherwise
// clients whose Id doesn't match their query drop the reply and time out
// instead of failing fast.
func TestConcurrentMissCollapsesAndPreservesPerCallerId(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	var calls atomic.Int64
	up := dnssrv.HandlerFunc(func(ctx context.Context, r *dnssrv.Request) (*dnssrv.Response, error) {
		calls.Add(1)
		time.Sleep(10 * time.Millisecond) // widen the window so callers collapse into one flight
		m := new(dns.Msg)
		m.SetRcode(r.Msg, dns.RcodeServerFailure)
		return &dnssrv.Response{Msg: m, Decision: dnssrv.DecisionForwarded}, nil
	})
	c := New(Options{Now: clk.Now})
	h := c.Middleware()(up)

	const n = 8
	var wg sync.WaitGroup
	resps := make([]*dnssrv.Response, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r := req("x.test")
			r.Msg.Id = uint16(i + 1)
			resps[i], errs[i] = h.ServeDNS(context.Background(), r)
		}(i)
	}
	wg.Wait()

	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("goroutine %d: unexpected error %v", i, errs[i])
		}
		if resps[i] == nil || resps[i].Msg == nil {
			t.Fatalf("goroutine %d: nil response", i)
		}
		want := uint16(i + 1)
		if got := resps[i].Msg.Id; got != want {
			t.Fatalf("goroutine %d: msg id %d want %d (mismatched id on collapsed singleflight response)", i, got, want)
		}
	}
	if calls.Load() >= n {
		t.Fatalf("singleflight did not collapse concurrent misses: upstream calls=%d want <%d", calls.Load(), n)
	}
}

// TestCachedResponseUsesRequesterQuestionCasing reproduces the cross-client
// casing leak: a cache entry populated by one client's question casing must
// not be echoed back verbatim to a later client that queries the same name
// in different casing. Both the Question section and any Answer RR owner
// names must be rewritten to match the current requester's exact casing.
func TestCachedResponseUsesRequesterQuestionCasing(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	c := New(Options{Now: clk.Now})
	h := c.Middleware()(answer("x.test", 300, "1.2.3.4"))

	if resp, err := h.ServeDNS(context.Background(), req("x.test")); err != nil || resp.Decision != dnssrv.DecisionForwarded {
		t.Fatalf("first query should forward: resp=%+v err=%v", resp, err)
	}
	resp, err := h.ServeDNS(context.Background(), req("X.TEST"))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Decision != dnssrv.DecisionCached {
		t.Fatalf("want cached, got %s", resp.Decision)
	}
	if got := resp.Msg.Question[0].Name; got != "X.TEST." {
		t.Fatalf("question name = %q, want %q", got, "X.TEST.")
	}
	if got := resp.Msg.Answer[0].Header().Name; got != "X.TEST." {
		t.Fatalf("answer owner name = %q, want %q", got, "X.TEST.")
	}
}

func TestEviction(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	c := New(Options{Now: clk.Now, MaxEntries: 2})
	h := c.Middleware()(answer("a.test", 300, "1.1.1.1"))
	for _, n := range []string{"a.test", "b.test", "c.test"} {
		_, _ = h.ServeDNS(context.Background(), req(n))
	}
	if c.Len() != 2 {
		t.Fatalf("len %d want 2", c.Len())
	}
}

// Purge is what a routing change uses to invalidate the entries a route it no
// longer has produced. It is scoped: only the suffix given, and only on a
// label boundary.
func TestPurgeDropsASuffixAndNothingElse(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	c := New(Options{Now: clk.Now})
	for _, name := range []string{"corp.example", "www.corp.example", "a.b.corp.example", "notcorp.example", "corp.example.net", "elsewhere.test"} {
		h := c.Middleware()(answer(name, 300, "1.2.3.4"))
		if _, err := h.ServeDNS(context.Background(), req(name)); err != nil {
			t.Fatal(err)
		}
	}
	if c.Len() != 6 {
		t.Fatalf("precondition: %d entries cached, want 6", c.Len())
	}

	// "CORP.example." rather than "corp.example": a caller passes a zone
	// name in whatever spelling the store holds, and entries are keyed by
	// QName's lower-cased, dot-trimmed form.
	if n := c.Purge("CORP.example."); n != 3 {
		t.Errorf("purged %d entries, want the 3 at or under corp.example", n)
	}
	if c.Len() != 3 {
		t.Errorf("%d entries left, want the 3 outside the suffix — notcorp.example, corp.example.net and elsewhere.test are not under it", c.Len())
	}
	// Named individually, because a count alone would pass if it had dropped
	// the wrong three.
	for _, name := range []string{"notcorp.example", "corp.example.net", "elsewhere.test"} {
		resp, err := c.Middleware()(dnssrv.HandlerFunc(func(context.Context, *dnssrv.Request) (*dnssrv.Response, error) {
			return nil, errors.New("upstream must not be reached")
		})).ServeDNS(context.Background(), req(name))
		if err != nil || resp.Decision != dnssrv.DecisionCached {
			t.Errorf("%s was purged, and it is not under the suffix", name)
		}
	}
}

// The half a sweep of the map alone would miss.
//
// A query that reached the upstreams *before* a zone claimed the suffix comes
// back with a pre-claim answer and puts it in the cache — and it lands after
// the purge has already swept past. Left in, that entry is served for its
// whole TTL and then stale for serve_stale_for beyond it, which is the same
// day-long wrong answer the purge exists to prevent, reached through a window
// one upstream round trip wide. And the window is not unlikely: the queries
// that motivated claiming the suffix are the ones in flight while you claim
// it.
//
// The purge lands here mid-lookup, which is what makes this deterministic
// rather than a race the test hopes to lose.
func TestPurgeDropsALookupThatWasAlreadyInFlight(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	c := New(Options{Now: clk.Now})
	var calls atomic.Int64
	up := dnssrv.HandlerFunc(func(ctx context.Context, r *dnssrv.Request) (*dnssrv.Response, error) {
		calls.Add(1)
		// The claim landing while this lookup is out at the upstream.
		c.Purge("corp.example")
		return answer("www.corp.example", 300, "5.6.7.8").ServeDNS(ctx, r)
	})
	h := c.Middleware()(up)

	if _, err := h.ServeDNS(context.Background(), req("www.corp.example")); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("precondition: %d upstream calls, want 1 — the purge did not run mid-lookup", calls.Load())
	}
	if n := c.Len(); n != 0 {
		t.Fatalf("%d entries cached, want 0: an answer produced before the purge was stored after it, and the claimed suffix now serves it", n)
	}
	if _, err := h.ServeDNS(context.Background(), req("www.corp.example")); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Error("the second query was answered from the cache, so the pre-purge answer outlived the purge")
	}
}
