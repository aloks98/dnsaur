package dnssrv

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/miekg/dns"
)

func TestServerServesPipeline(t *testing.T) {
	h := HandlerFunc(func(ctx context.Context, req *Request) (*Response, error) {
		if !req.ClientIP.IsValid() || !req.ClientIP.IsLoopback() {
			t.Errorf("client ip not set: %v", req.ClientIP)
		}
		m := new(dns.Msg)
		m.SetReply(req.Msg)
		rr, _ := dns.NewRR(req.Msg.Question[0].Name + " 300 IN A 1.2.3.4")
		m.Answer = append(m.Answer, rr)
		return &Response{Msg: m, Decision: DecisionForwarded}, nil
	})
	s := NewServer("127.0.0.1:0", h)
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := s.Shutdown(context.Background()); err != nil {
			t.Logf("shutdown error: %v", err)
		}
	}()

	c := new(dns.Client)
	m := new(dns.Msg)
	m.SetQuestion("hello.test.", dns.TypeA)
	for _, net := range []string{"udp", "tcp"} {
		c.Net = net
		r, _, err := c.Exchange(m, s.Addr())
		if err != nil {
			t.Fatalf("%s exchange: %v", net, err)
		}
		if len(r.Answer) != 1 {
			t.Fatalf("%s: no answer", net)
		}
	}
	_ = netip.Addr{}
	_ = time.Second
}

func TestEDNSEchoOnSynthesizedResponse(t *testing.T) {
	// Handler returns synthesized response via SetReply (no OPT)
	h := HandlerFunc(func(ctx context.Context, req *Request) (*Response, error) {
		m := new(dns.Msg)
		m.SetReply(req.Msg)
		rr, _ := dns.NewRR(req.Msg.Question[0].Name + " 300 IN A 1.2.3.4")
		m.Answer = append(m.Answer, rr)
		return &Response{Msg: m, Decision: DecisionAuthoritative}, nil
	})
	s := NewServer("127.0.0.1:0", h)
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := s.Shutdown(context.Background()); err != nil {
			t.Logf("shutdown error: %v", err)
		}
	}()

	c := new(dns.Client)
	c.Net = "udp"
	m := new(dns.Msg)
	m.SetQuestion("hello.test.", dns.TypeA)
	m.SetEdns0(4096, false) // EDNS query

	r, _, err := c.Exchange(m, s.Addr())
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}

	// Response should have OPT echoed with UDPSize 1232
	opt := r.IsEdns0()
	if opt == nil {
		t.Fatal("EDNS response should have OPT RR")
	}
	if opt.UDPSize() != 1232 {
		t.Errorf("OPT UDPSize = %d, want 1232", opt.UDPSize())
	}
}

func TestNonEDNSNoUnsolicited(t *testing.T) {
	// Handler returns response without EDNS
	h := HandlerFunc(func(ctx context.Context, req *Request) (*Response, error) {
		m := new(dns.Msg)
		m.SetReply(req.Msg)
		rr, _ := dns.NewRR(req.Msg.Question[0].Name + " 300 IN A 1.2.3.4")
		m.Answer = append(m.Answer, rr)
		return &Response{Msg: m, Decision: DecisionAuthoritative}, nil
	})
	s := NewServer("127.0.0.1:0", h)
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := s.Shutdown(context.Background()); err != nil {
			t.Logf("shutdown error: %v", err)
		}
	}()

	c := new(dns.Client)
	c.Net = "udp"
	m := new(dns.Msg)
	m.SetQuestion("hello.test.", dns.TypeA)
	// NO EDNS in query

	r, _, err := c.Exchange(m, s.Addr())
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}

	// Response should NOT have OPT added unsolicited
	if r.IsEdns0() != nil {
		t.Error("non-EDNS query should not get unsolicited OPT")
	}
}

func TestUDPSizeFloor(t *testing.T) {
	// Handler returns response with multiple records to exercise truncation
	h := HandlerFunc(func(ctx context.Context, req *Request) (*Response, error) {
		m := new(dns.Msg)
		m.SetReply(req.Msg)
		// Add several TXT records to exceed small size
		for i := 0; i < 5; i++ {
			rr, _ := dns.NewRR(req.Msg.Question[0].Name + " 300 IN TXT \"very long text record for testing truncation behavior with multiple entries\"")
			m.Answer = append(m.Answer, rr)
		}
		return &Response{Msg: m, Decision: DecisionAuthoritative}, nil
	})
	s := NewServer("127.0.0.1:0", h)
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := s.Shutdown(context.Background()); err != nil {
			t.Logf("shutdown error: %v", err)
		}
	}()

	c := new(dns.Client)
	c.Net = "udp"
	m := new(dns.Msg)
	m.SetQuestion("hello.test.", dns.TypeTXT)
	m.SetEdns0(256, false) // Advertise size < 512 (miekg may floor internally)

	r, _, err := c.Exchange(m, s.Addr())
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}

	// Response should use 512-byte floor, not 256
	// Verify answers are intact (not truncated by enforced floor)
	if r.Truncated {
		t.Error("response should not be truncated when 512 floor applied")
	}
	if len(r.Answer) == 0 {
		t.Error("response should contain answers (proves truncation didn't happen)")
	}
}
