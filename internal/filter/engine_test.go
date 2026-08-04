package filter

import (
	"context"
	"testing"
	"time"

	"github.com/aloks98/dnsaur/internal/dnssrv"
	"github.com/miekg/dns"
)

func testReq(name string, qtype uint16, groupID int64) *dnssrv.Request {
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(name), qtype)
	return &dnssrv.Request{Msg: m, Client: dnssrv.ClientInfo{GroupID: groupID}}
}

func passthrough(t *testing.T) dnssrv.Handler {
	return dnssrv.HandlerFunc(func(ctx context.Context, req *dnssrv.Request) (*dnssrv.Response, error) {
		m := new(dns.Msg)
		m.SetReply(req.Msg)
		return &dnssrv.Response{Msg: m, Decision: dnssrv.DecisionForwarded}, nil
	})
}

func engineWith(t *testing.T, mode string) *Engine {
	e := NewEngine()
	e.SetBlocking(mode, 30)
	e.SetGroups(map[int64]*Ruleset{
		1: Compile(nil, []CompiledList{compiledList(10, "block", "ads.example")}),
	})
	return e
}

func TestPausedUntil(t *testing.T) {
	e := NewEngine()
	e.Pause(0, time.Minute)
	if until := e.PausedUntil(0); until.IsZero() {
		t.Fatal("global pause not reported")
	}
	if until := e.PausedUntil(5); until.IsZero() {
		t.Fatal("group pause should inherit global")
	}
	e.Pause(0, 0)
	if until := e.PausedUntil(0); !until.IsZero() {
		t.Fatal("resume didn't clear pause")
	}
	if until := e.PausedUntil(5); !until.IsZero() {
		t.Fatal("group pause should clear with global")
	}
}

func TestBlockNullIP(t *testing.T) {
	e := engineWith(t, "null-ip")
	h := e.Middleware()(passthrough(t))
	resp, err := h.ServeDNS(context.Background(), testReq("x.ads.example", dns.TypeA, 1))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Decision != dnssrv.DecisionBlocked || resp.ListID != 10 {
		t.Fatalf("%+v", resp)
	}
	a, ok := resp.Msg.Answer[0].(*dns.A)
	if !ok || a.A.String() != "0.0.0.0" || a.Hdr.Ttl != 30 {
		t.Fatalf("answer %v", resp.Msg.Answer)
	}
}

func TestBlockNXDOMAIN(t *testing.T) {
	e := engineWith(t, "nxdomain")
	h := e.Middleware()(passthrough(t))
	resp, _ := h.ServeDNS(context.Background(), testReq("ads.example", dns.TypeA, 1))
	if resp.Msg.Rcode != dns.RcodeNameError {
		t.Fatalf("rcode %d", resp.Msg.Rcode)
	}
}

func TestPauseDisablesBlocking(t *testing.T) {
	e := engineWith(t, "null-ip")
	h := e.Middleware()(passthrough(t))
	e.Pause(0, time.Minute)
	resp, _ := h.ServeDNS(context.Background(), testReq("ads.example", dns.TypeA, 1))
	if resp.Decision != dnssrv.DecisionForwarded {
		t.Fatalf("paused engine still blocked: %+v", resp)
	}
}

func TestUnknownGroupPassesThrough(t *testing.T) {
	e := engineWith(t, "null-ip")
	h := e.Middleware()(passthrough(t))
	resp, _ := h.ServeDNS(context.Background(), testReq("ads.example", dns.TypeA, 99))
	if resp.Decision != dnssrv.DecisionForwarded {
		t.Fatalf("%+v", resp)
	}
}
