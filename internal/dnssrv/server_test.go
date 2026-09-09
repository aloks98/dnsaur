package dnssrv

import (
	"context"
	"fmt"
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

// noOPTHandler answers without an OPT, so the reply the client sees carries
// only the one the server synthesised for it. That is the case the DO bit
// can be lost in: an upstream reply arrives with its own OPT and is echoed
// through untouched, but a locally synthesised answer -- a zone, a block, a
// SERVFAIL -- has whatever the server puts on it and nothing else.
func noOPTHandler() Handler {
	return HandlerFunc(func(_ context.Context, req *Request) (*Response, error) {
		m := new(dns.Msg)
		m.SetReply(req.Msg)
		rr, _ := dns.NewRR(req.Msg.Question[0].Name + " 300 IN A 1.2.3.4")
		m.Answer = append(m.Answer, rr)
		return &Response{Msg: m, Decision: DecisionAuthoritative}, nil
	})
}

// RFC 3225 §3: "The DO bit of the query MUST be copied in the response."
// A synthesised OPT built with DO cleared tells a validating client the
// server is not DNSSEC-aware for an answer it asked to be able to validate.
func TestSynthesizedOPTCopiesTheDOBit(t *testing.T) {
	s := NewServer("127.0.0.1:0", noOPTHandler())
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := s.Shutdown(context.Background()); err != nil {
			t.Logf("shutdown error: %v", err)
		}
	}()

	for _, do := range []bool{true, false} {
		t.Run(fmt.Sprintf("do=%t", do), func(t *testing.T) {
			c := &dns.Client{Net: "udp"}
			m := new(dns.Msg)
			m.SetQuestion("hello.test.", dns.TypeA)
			m.SetEdns0(4096, do)

			r, _, err := c.Exchange(m, s.Addr())
			if err != nil {
				t.Fatalf("exchange: %v", err)
			}
			opt := r.IsEdns0()
			if opt == nil {
				t.Fatal("an EDNS query got no OPT back")
			}
			if opt.Do() != do {
				t.Errorf("reply DO = %t, want the query's own %t", opt.Do(), do)
			}
		})
	}
}

// optCarryingHandler answers with a reply that already carries an OPT, the
// way the cache does: it stores what an upstream sent, OPT and all, so the
// EDNS client that populated an entry decides what the next client gets
// back for the same name.
//
// The cookie is not decoration. RFC 6891 §7's "MUST NOT" is about the OPT
// itself, but what makes the leak concrete is the options riding in it: a
// cookie minted for one client, handed to another.
func optCarryingHandler() Handler {
	return HandlerFunc(func(_ context.Context, req *Request) (*Response, error) {
		m := new(dns.Msg)
		m.SetReply(req.Msg)
		rr, _ := dns.NewRR(req.Msg.Question[0].Name + " 300 IN A 1.2.3.4")
		m.Answer = append(m.Answer, rr)
		opt := &dns.OPT{Hdr: dns.RR_Header{Name: ".", Rrtype: dns.TypeOPT, Class: 4096}}
		opt.Option = append(opt.Option, &dns.EDNS0_COOKIE{Code: dns.EDNS0COOKIE, Cookie: "0123456789abcdef"})
		m.Extra = append(m.Extra, opt)
		return &Response{Msg: m, Decision: DecisionForwarded}, nil
	})
}

// RFC 6891 §7: a responder that receives a query with no OPT "MUST NOT
// include an OPT record in the response". Adding one when the reply lacked
// it was already handled; removing one the reply arrived with was not, and
// that is the case the cache produces on every second client.
func TestNonEDNSGetsNoOPTFromACachedReply(t *testing.T) {
	s := NewServer("127.0.0.1:0", optCarryingHandler())
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := s.Shutdown(context.Background()); err != nil {
			t.Logf("shutdown error: %v", err)
		}
	}()

	for _, transport := range []string{"udp", "tcp"} {
		t.Run(transport, func(t *testing.T) {
			c := &dns.Client{Net: transport}
			m := new(dns.Msg)
			m.SetQuestion("hello.test.", dns.TypeA)
			// No EDNS: this client never said it could read an OPT.

			r, _, err := c.Exchange(m, s.Addr())
			if err != nil {
				t.Fatalf("exchange: %v", err)
			}
			if opt := r.IsEdns0(); opt != nil {
				t.Errorf("a non-EDNS query got an OPT back carrying %v", opt.Option)
			}
			if len(r.Answer) != 1 {
				t.Errorf("answer records = %d, want the answer itself left alone", len(r.Answer))
			}
		})
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
