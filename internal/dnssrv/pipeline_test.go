package dnssrv

import (
	"context"
	"testing"

	"github.com/miekg/dns"
)

func q(name string, qtype uint16) *Request {
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(name), qtype)
	return &Request{Msg: m}
}

func TestChainOrderAndShortCircuit(t *testing.T) {
	var order []string
	mw := func(name string, answer bool) Middleware {
		return func(next Handler) Handler {
			return HandlerFunc(func(ctx context.Context, req *Request) (*Response, error) {
				order = append(order, name)
				if answer {
					return &Response{Msg: new(dns.Msg), Decision: DecisionLocal}, nil
				}
				return next.ServeDNS(ctx, req)
			})
		}
	}
	terminal := HandlerFunc(func(ctx context.Context, req *Request) (*Response, error) {
		order = append(order, "terminal")
		return &Response{Msg: new(dns.Msg), Decision: DecisionForwarded}, nil
	})
	resp, err := Chain(terminal, mw("a", false), mw("b", true)).ServeDNS(context.Background(), q("x.test", dns.TypeA))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Decision != DecisionLocal {
		t.Fatalf("decision %s", resp.Decision)
	}
	if len(order) != 2 || order[0] != "a" || order[1] != "b" {
		t.Fatalf("order %v", order)
	}
}

func TestQNameNormalized(t *testing.T) {
	req := q("WWW.Example.COM.", dns.TypeA)
	if req.QName() != "www.example.com" {
		t.Fatalf("got %q", req.QName())
	}
}

func TestRecoverConvertsPanic(t *testing.T) {
	h := Chain(HandlerFunc(func(ctx context.Context, req *Request) (*Response, error) {
		panic("boom")
	}), Recover())
	resp, err := h.ServeDNS(context.Background(), q("x.test", dns.TypeA))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Decision != DecisionError || resp.Msg.Rcode != dns.RcodeServerFailure {
		t.Fatalf("panic not converted: %+v", resp)
	}
}
