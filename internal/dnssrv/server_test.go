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
