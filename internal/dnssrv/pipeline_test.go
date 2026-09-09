package dnssrv

import (
	"context"
	"testing"

	"github.com/miekg/dns"
)

// The three deadlines in this package are defined against each other:
// PipelineTimeout is what an ordinary query gets, and TransferTimeout and
// NotifyTimeout exist precisely because a transfer and a NOTIFY need
// something other than it. Their sibling relation is asserted in
// notifies_test.go; this is the third side of it, and the thing that
// notices if the number this package hands every listener stops being a
// number at all.
func TestPipelineTimeoutIsTheOrdinaryQuerysDeadline(t *testing.T) {
	if PipelineTimeout <= 0 {
		t.Fatalf("PipelineTimeout = %v, want a positive bound", PipelineTimeout)
	}
	if PipelineTimeout >= TransferTimeout {
		t.Errorf("PipelineTimeout = %v, want less than TransferTimeout (%v) -- a transfer needs its own, longer deadline, which is why TransferTimeout exists",
			PipelineTimeout, TransferTimeout)
	}
}

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
					return &Response{Msg: new(dns.Msg), Decision: DecisionAuthoritative}, nil
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
	if resp.Decision != DecisionAuthoritative {
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
