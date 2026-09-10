package filter

import (
	"context"
	"testing"
	"time"

	"github.com/aloks98/dnsaur/internal/dnssrv"
	"github.com/miekg/dns"
)

func testReq(name string, qtype uint16, groupID int64) *dnssrv.Request {
	return testReqFrom(name, qtype, groupID, 0)
}

// testReqFrom is testReq with the client identified too, for the pauses that
// are keyed by client rather than by group.
func testReqFrom(name string, qtype uint16, groupID, clientID int64) *dnssrv.Request {
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(name), qtype)
	return &dnssrv.Request{Msg: m, Client: dnssrv.ClientInfo{ID: clientID, GroupID: groupID}}
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
	e.Pause(PauseGlobal, 0, time.Minute)
	if until, kind := e.PausedUntil(0, 0); until.IsZero() || kind != PauseGlobal {
		t.Fatalf("global pause not reported: %v %q", until, kind)
	}
	if until, kind := e.PausedUntil(5, 9); until.IsZero() || kind != PauseGlobal {
		t.Fatalf("a group and a client both inherit the global pause: %v %q", until, kind)
	}
	e.Pause(PauseGlobal, 0, 0)
	if until, _ := e.PausedUntil(0, 0); !until.IsZero() {
		t.Fatal("resume didn't clear pause")
	}
	if until, _ := e.PausedUntil(5, 9); !until.IsZero() {
		t.Fatal("group pause should clear with global")
	}
}

// TestPausedUntilAcrossScopes: the three scopes share one "the later wins"
// rule, so a client pause extends its group's and a group pause extends its
// clients', with neither able to cut the other short. Groups and clients are
// numbered separately, so the same id in the two scopes is two pauses.
func TestPausedUntilAcrossScopes(t *testing.T) {
	e := NewEngine()
	e.Pause(PauseClient, 3, time.Hour)
	if until, kind := e.PausedUntil(3, 3); kind != PauseClient || time.Until(until) < 30*time.Minute {
		t.Fatalf("PausedUntil(3, 3) = %v %q, want the client's own hour", until, kind)
	}
	if until, kind := e.PausedUntil(3, 0); !until.IsZero() {
		t.Fatalf("a pause on client 3 also paused group 3: %v %q", until, kind)
	}

	// A shorter group pause must not cut the client's short…
	e.Pause(PauseGroup, 3, time.Minute)
	if until, kind := e.PausedUntil(3, 3); kind != PauseClient || time.Until(until) < 30*time.Minute {
		t.Fatalf("PausedUntil(3, 3) = %v %q, want the longer client pause", until, kind)
	}
	// …and a longer global one wins over both.
	e.Pause(PauseGlobal, 0, 2*time.Hour)
	if until, kind := e.PausedUntil(3, 3); kind != PauseGlobal || time.Until(until) < 90*time.Minute {
		t.Fatalf("PausedUntil(3, 3) = %v %q, want the longest, the global pause", until, kind)
	}
	// Resuming one scope leaves the others running.
	e.Pause(PauseGlobal, 0, 0)
	if until, kind := e.PausedUntil(3, 3); kind != PauseClient || time.Until(until) < 30*time.Minute {
		t.Fatalf("resuming globally cleared the client pause too: %v %q", until, kind)
	}
}

// TestPausePerClient: a pause on one device stops blocking for that device
// and leaves the rest of its group filtering.
func TestPausePerClient(t *testing.T) {
	e := engineWith(t, "null-ip")
	h := e.Middleware()(passthrough(t))

	e.Pause(PauseClient, 4, time.Minute)
	if resp, _ := h.ServeDNS(context.Background(), testReqFrom("ads.example", dns.TypeA, 1, 4)); resp.Decision != dnssrv.DecisionForwarded {
		t.Fatalf("paused client still blocked: %+v", resp)
	}
	if resp, _ := h.ServeDNS(context.Background(), testReqFrom("ads.example", dns.TypeA, 1, 5)); resp.Decision != dnssrv.DecisionBlocked {
		t.Fatalf("client 5 stopped blocking because client 4 is paused: %+v", resp)
	}
}

// TestRestoreDropsExpiredPauses: a restart puts back the pauses that are
// still running and none of the ones that ran out while the process was
// down — the difference between "the operator paused for an hour and we
// rebooted" and "blocking is off again for no reason".
func TestRestoreDropsExpiredPauses(t *testing.T) {
	now := time.Now()
	e := NewEngine()
	e.Restore(Pauses{
		Global:  now.Add(-time.Minute).UnixMilli(),
		Groups:  map[int64]int64{2: now.Add(time.Hour).UnixMilli()},
		Clients: map[int64]int64{4: now.Add(-time.Hour).UnixMilli()},
	})
	if until, _ := e.PausedUntil(0, 0); !until.IsZero() {
		t.Fatalf("an expired global pause came back: %v", until)
	}
	if until, kind := e.PausedUntil(2, 4); until.IsZero() || kind != PauseGroup {
		t.Fatalf("PausedUntil(2, 4) = %v %q, want the group pause that is still running", until, kind)
	}
	if until, _ := e.PausedUntil(9, 4); !until.IsZero() {
		t.Fatalf("an expired client pause came back: %v", until)
	}
}

// TestPauseNotifiesAndPrunes: what the app persists is what is still in
// force, so a pause that has run out never survives a restart, and the row
// does not grow a dead entry per pause ever taken.
func TestPauseNotifiesAndPrunes(t *testing.T) {
	e := NewEngine()
	var last Pauses
	e.OnPauseChange(func(p Pauses) { last = p })

	e.Pause(PauseGroup, 2, time.Hour)
	if last.Groups[2] == 0 {
		t.Fatalf("the pause was not published: %+v", last)
	}
	e.Pause(PauseClient, 4, time.Hour)
	e.Pause(PauseClient, 4, 0)
	if _, ok := last.Clients[4]; ok {
		t.Fatalf("resume left the client entry behind: %+v", last)
	}
	if last.Groups[2] == 0 {
		t.Fatalf("resuming one scope dropped another's pause: %+v", last)
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
	e.Pause(PauseGlobal, 0, time.Minute)
	resp, _ := h.ServeDNS(context.Background(), testReq("ads.example", dns.TypeA, 1))
	if resp.Decision != dnssrv.DecisionForwarded {
		t.Fatalf("paused engine still blocked: %+v", resp)
	}
}

// TestPausePerGroup: a pause on one group leaves every other group
// filtering. Only the global pause is meant to stop everyone.
func TestPausePerGroup(t *testing.T) {
	e := engineWith(t, "null-ip")
	e.SetGroups(map[int64]*Ruleset{
		1: Compile(nil, []CompiledList{compiledList(10, "block", "ads.example")}),
		2: Compile(nil, []CompiledList{compiledList(10, "block", "ads.example")}),
	})
	h := e.Middleware()(passthrough(t))

	e.Pause(PauseGroup, 1, time.Minute)
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
	e.Pause(PauseGlobal, 0, time.Minute)
	e.Pause(PauseGroup, 7, time.Hour)
	if until, _ := e.PausedUntil(7, 0); time.Until(until) < 30*time.Minute {
		t.Fatalf("PausedUntil(7, 0) = %v, want the longer group pause", until)
	}

	e = NewEngine()
	e.Pause(PauseGlobal, 0, time.Hour)
	e.Pause(PauseGroup, 7, time.Minute)
	if until, _ := e.PausedUntil(7, 0); time.Until(until) < 30*time.Minute {
		t.Fatalf("PausedUntil(7, 0) = %v, want the longer global pause", until)
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
