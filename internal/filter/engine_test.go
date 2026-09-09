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

// TestPausePerGroup: a pause on one group leaves every other group
// filtering. Only the global pause (id 0) is meant to stop everyone.
func TestPausePerGroup(t *testing.T) {
	e := engineWith(t, "null-ip")
	e.SetGroups(map[int64]*Ruleset{
		1: Compile(nil, []CompiledList{compiledList(10, "block", "ads.example")}),
		2: Compile(nil, []CompiledList{compiledList(10, "block", "ads.example")}),
	})
	h := e.Middleware()(passthrough(t))

	e.Pause(1, time.Minute)
	if resp, _ := h.ServeDNS(context.Background(), testReq("ads.example", dns.TypeA, 1)); resp.Decision != dnssrv.DecisionForwarded {
		t.Fatalf("paused group still blocked: %+v", resp)
	}
	if resp, _ := h.ServeDNS(context.Background(), testReq("ads.example", dns.TypeA, 2)); resp.Decision != dnssrv.DecisionBlocked {
		t.Fatalf("group 2 stopped blocking because group 1 is paused: %+v", resp)
	}
}

// TestPausedUntilTakesTheLater is the documented rule: a global pause and a
// group pause can both be in effect, and whichever ends later wins — in both
// directions, so neither can cut the other short.
func TestPausedUntilTakesTheLater(t *testing.T) {
	e := NewEngine()
	e.Pause(0, time.Minute)
	e.Pause(7, time.Hour)
	if until := e.PausedUntil(7); time.Until(until) < 30*time.Minute {
		t.Fatalf("PausedUntil(7) = %v, want the longer group pause", until)
	}

	e = NewEngine()
	e.Pause(0, time.Hour)
	e.Pause(7, time.Minute)
	if until := e.PausedUntil(7); time.Until(until) < 30*time.Minute {
		t.Fatalf("PausedUntil(7) = %v, want the longer global pause", until)
	}
	// And the shorter group pause must not resume the group early.
	h := e.Middleware()(passthrough(t))
	e.SetGroups(map[int64]*Ruleset{7: Compile(nil, []CompiledList{compiledList(10, "block", "ads.example")})})
	if resp, _ := h.ServeDNS(context.Background(), testReq("ads.example", dns.TypeA, 7)); resp.Decision != dnssrv.DecisionForwarded {
		t.Fatalf("global pause not honoured for a group with its own shorter one: %+v", resp)
	}
}

// TestBlockNullIPPerQType: null-ip is an answer shaped like the question. An
// AAAA gets ::, and a type with no null address (TXT, MX…) gets an empty
// NOERROR — still recorded as blocked, and never forwarded, which is the
// point.
func TestBlockNullIPPerQType(t *testing.T) {
	e := engineWith(t, "null-ip")
	h := e.Middleware()(passthrough(t))

	resp, err := h.ServeDNS(context.Background(), testReq("ads.example", dns.TypeAAAA, 1))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Decision != dnssrv.DecisionBlocked {
		t.Fatalf("AAAA not blocked: %+v", resp)
	}
	aaaa, ok := resp.Msg.Answer[0].(*dns.AAAA)
	if !ok || aaaa.AAAA.String() != "::" || aaaa.Hdr.Ttl != 30 {
		t.Fatalf("answer %v", resp.Msg.Answer)
	}

	for _, qt := range []uint16{dns.TypeTXT, dns.TypeMX, dns.TypeHTTPS} {
		resp, err := h.ServeDNS(context.Background(), testReq("ads.example", qt, 1))
		if err != nil {
			t.Fatal(err)
		}
		if resp.Decision != dnssrv.DecisionBlocked {
			t.Fatalf("%s forwarded instead of blocked: %+v", dns.TypeToString[qt], resp)
		}
		if len(resp.Msg.Answer) != 0 || resp.Msg.Rcode != dns.RcodeSuccess {
			t.Fatalf("%s answer = %v rcode = %d, want empty NOERROR", dns.TypeToString[qt], resp.Msg.Answer, resp.Msg.Rcode)
		}
	}
}

// TestBlockedResponseCarriesTheMatch: the ids say which rule or list
// decided, not what in it matched, and inside a million-entry list that is
// the only part anyone can act on.
func TestBlockedResponseCarriesTheMatch(t *testing.T) {
	e := engineWith(t, "null-ip")
	h := e.Middleware()(passthrough(t))
	resp, err := h.ServeDNS(context.Background(), testReq("x.ads.example", dns.TypeA, 1))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Matched != "ads.example" {
		t.Fatalf("Matched = %q, want the list entry that fired", resp.Matched)
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
