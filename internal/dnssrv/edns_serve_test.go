package dnssrv_test

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/aloks98/dnsaur/internal/cache"
	"github.com/aloks98/dnsaur/internal/dnssrv"
	"github.com/miekg/dns"
)

// The OPT that reaches a non-EDNS client is not a hypothetical one a test
// handler had to be talked into producing: it is what the real cache does.
// It stores an upstream reply whole, OPT and all, and hands a copy to
// whoever asks next — so the first EDNS client to populate an entry decides
// what every later non-EDNS client receives for that name, along with
// whatever options that OPT carried.
//
// The cache here is the real one for exactly that reason. A stub handler
// returning an OPT proves that the strip works on a message shaped like the
// cache's; only the cache itself proves the message really is shaped that
// way. The upstream call count is what separates the two clients: without
// it, a second query that quietly went upstream again would pass this test
// while testing nothing about a cached reply at all.
func TestCachedReplyLosesItsOPTForANonEDNSClient(t *testing.T) {
	var upstreamCalls atomic.Int32
	terminal := dnssrv.HandlerFunc(func(_ context.Context, req *dnssrv.Request) (*dnssrv.Response, error) {
		upstreamCalls.Add(1)
		m := new(dns.Msg)
		m.SetReply(req.Msg)
		rr, err := dns.NewRR("cached.test. 300 IN A 1.2.3.4")
		if err != nil {
			t.Errorf("building the answer: %v", err)
			return nil, err
		}
		m.Answer = []dns.RR{rr}
		// What an upstream sends back to an EDNS query dnsaur made: an OPT
		// of its own, with a cookie minted for this exchange.
		opt := &dns.OPT{Hdr: dns.RR_Header{Name: ".", Rrtype: dns.TypeOPT, Class: 1232}}
		opt.Option = []dns.EDNS0{&dns.EDNS0_COOKIE{Code: dns.EDNS0COOKIE, Cookie: "0123456789abcdef"}}
		m.Extra = []dns.RR{opt}
		// Only a forwarded answer is cached, which is the case this is about.
		return &dnssrv.Response{Msg: m, Decision: dnssrv.DecisionForwarded}, nil
	})

	c := cache.New(cache.Options{})
	srv := dnssrv.NewServer("127.0.0.1:0", dnssrv.Chain(terminal, c.Middleware()))
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		if err := srv.Shutdown(context.Background()); err != nil {
			t.Logf("shutdown error: %v", err)
		}
	})

	client := &dns.Client{Net: "udp"}

	// Client A speaks EDNS and populates the entry.
	first := new(dns.Msg)
	first.SetQuestion("cached.test.", dns.TypeA)
	first.SetEdns0(1232, false)
	if _, _, err := client.Exchange(first, srv.Addr()); err != nil {
		t.Fatalf("first exchange: %v", err)
	}

	// Client B, on the same network, speaks no EDNS at all.
	second := new(dns.Msg)
	second.SetQuestion("cached.test.", dns.TypeA)
	r, _, err := client.Exchange(second, srv.Addr())
	if err != nil {
		t.Fatalf("second exchange: %v", err)
	}

	if n := upstreamCalls.Load(); n != 1 {
		t.Fatalf("the terminal handler ran %d times, want 1 -- the second query has to be answered from the cache for this test to be about a cached reply at all", n)
	}
	if opt := r.IsEdns0(); opt != nil {
		t.Errorf("a non-EDNS client was served the cached reply's OPT, carrying %v (RFC 6891 §7)", opt.Option)
	}
	if len(r.Answer) != 1 {
		t.Errorf("answer records = %d, want the cached answer itself left alone", len(r.Answer))
	}

	// The cache must still hold what it stored: the strip happens on the
	// copy going out, so client A's next query still gets its OPT back.
	third := new(dns.Msg)
	third.SetQuestion("cached.test.", dns.TypeA)
	third.SetEdns0(1232, false)
	again, _, err := client.Exchange(third, srv.Addr())
	if err != nil {
		t.Fatalf("third exchange: %v", err)
	}
	if again.IsEdns0() == nil {
		t.Error("an EDNS client got no OPT back after a non-EDNS client was served from the same entry: the strip reached into the cache instead of the outgoing copy")
	}
	if n := upstreamCalls.Load(); n != 1 {
		t.Errorf("the terminal handler ran %d times, want 1", n)
	}
}
