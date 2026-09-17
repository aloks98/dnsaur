package dhcp_test

import (
	"bytes"
	"context"
	"log/slog"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aloks98/dnsaur/internal/dhcp"
	"github.com/aloks98/dnsaur/internal/dnssrv"
	"github.com/aloks98/dnsaur/internal/store"
	"github.com/miekg/dns"
)

// namesScopes and namesDomain are what the app would hand the stage: the two
// scopes every manager test renders against, and the dhcp.domain setting the
// first of them has no suffix of its own to override.
func namesScopes() []store.Scope { return []store.Scope{lanScope(), iotScope()} }

func namesDomain() string { return "home.lan" }

// leased is a lease in Kea's own shape: active, valid for lft seconds from
// now, which is what the table turns into an expiry.
func leased(ip, mac, hostname string, scope int64, lft int64) dhcp.Lease {
	return dhcp.Lease{
		IP: ip, MAC: mac, Hostname: hostname, SubnetID: scope,
		CLTT: time.Now().Unix(), ValidLft: lft, State: 0,
	}
}

// namesManager is a manager over a fake engine, logging wherever the caller
// asks: the collision warning is the only thing this stage says out loud.
func namesManager(t *testing.T, log *slog.Logger, in dhcp.RenderInput) *dhcp.Manager {
	t.Helper()
	return dhcp.NewManager(dhcp.NewClient(in.Socket), &fakeInputs{in: in}, openSettings(t), log)
}

// namesStage seeds an engine with leases, polls them into the manager's
// table, and returns the real middleware chain the server runs: the stage in
// front of a terminal handler that records whether the query reached it.
func namesStage(t *testing.T, zc dhcp.ZoneCheck, leases ...dhcp.Lease) (dnssrv.Handler, *bool) {
	t.Helper()
	e := newEngine(t, "3.0.3", alpineHooks)
	m := namesManager(t, nil, managerInput(e.Socket()))
	e.setLeases(leases...)
	if err := m.Poll(t.Context()); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	terminal, passed := recorder()
	return dnssrv.Chain(terminal, dhcp.Names(m, namesScopes, namesDomain, zc)), passed
}

// recorder is the stage below this one: it answers nothing of substance and
// records that the query got that far.
func recorder() (dnssrv.Handler, *bool) {
	passed := new(bool)
	return dnssrv.HandlerFunc(func(_ context.Context, req *dnssrv.Request) (*dnssrv.Response, error) {
		*passed = true
		reply := new(dns.Msg)
		reply.SetReply(req.Msg)
		return &dnssrv.Response{Msg: reply, Decision: dnssrv.DecisionForwarded}, nil
	}), passed
}

func ask(t *testing.T, h dnssrv.Handler, qname string, qtype uint16) *dnssrv.Response {
	t.Helper()
	msg := new(dns.Msg)
	msg.SetQuestion(dns.Fqdn(qname), qtype)
	resp, err := h.ServeDNS(t.Context(), &dnssrv.Request{Msg: msg, ClientIP: netip.MustParseAddr("10.42.0.99")})
	if err != nil {
		t.Fatalf("%s %s: %v", dns.TypeToString[qtype], qname, err)
	}
	if resp == nil || resp.Msg == nil {
		t.Fatalf("%s %s: no response", dns.TypeToString[qtype], qname)
	}
	return resp
}

// TestNamesAnswersALeaseUnderItsScopeSuffix: a lease's sanitised hostname is
// a name under the suffix its scope hands out — the dhcp.domain setting for
// a scope with none of its own, the scope's own where it has one — and the
// answer is this server's, marked "dhcp" in the query log.
func TestNamesAnswersALeaseUnderItsScopeSuffix(t *testing.T) {
	h, passed := namesStage(t, nil,
		leased("10.42.0.50", "aa:bb:cc:dd:ee:01", "My Laptop", 1, 3600),
		leased("10.43.0.7", "aa:bb:cc:dd:ee:02", "sensor", 2, 3600),
	)

	for _, c := range []struct{ qname, ip string }{
		{"my-laptop.home.lan", "10.42.0.50"},
		{"sensor.iot.lan", "10.43.0.7"},
	} {
		resp := ask(t, h, c.qname, dns.TypeA)
		if *passed {
			t.Fatalf("%s reached the next stage, want it answered here", c.qname)
		}
		if len(resp.Msg.Answer) != 1 {
			t.Fatalf("%s: answer = %v, want one A", c.qname, resp.Msg.Answer)
		}
		a, ok := resp.Msg.Answer[0].(*dns.A)
		if !ok || a.A.String() != c.ip {
			t.Errorf("%s: answer = %v, want %s", c.qname, resp.Msg.Answer[0], c.ip)
		}
		if !resp.Msg.Authoritative {
			t.Errorf("%s: answered without AA set", c.qname)
		}
		if resp.Decision != dnssrv.DecisionAuthoritative || resp.Matched != "dhcp" {
			t.Errorf("%s: decision %q matched %q, want authoritative/dhcp", c.qname, resp.Decision, resp.Matched)
		}
	}
}

// TestNamesAnswersPTRInsideAScope: the reverse of the same lease, named
// under its scope's suffix.
func TestNamesAnswersPTRInsideAScope(t *testing.T) {
	h, passed := namesStage(t, nil,
		leased("10.42.0.50", "aa:bb:cc:dd:ee:01", "My Laptop", 1, 3600),
		leased("10.43.0.7", "aa:bb:cc:dd:ee:02", "sensor", 2, 3600),
	)

	for _, c := range []struct{ qname, target string }{
		{"50.0.42.10.in-addr.arpa", "my-laptop.home.lan."},
		{"7.0.43.10.in-addr.arpa", "sensor.iot.lan."},
	} {
		resp := ask(t, h, c.qname, dns.TypePTR)
		if *passed {
			t.Fatalf("%s reached the next stage, want it answered here", c.qname)
		}
		if len(resp.Msg.Answer) != 1 {
			t.Fatalf("%s: answer = %v, want one PTR", c.qname, resp.Msg.Answer)
		}
		ptr, ok := resp.Msg.Answer[0].(*dns.PTR)
		if !ok || ptr.Ptr != c.target {
			t.Errorf("%s: answer = %v, want %s", c.qname, resp.Msg.Answer[0], c.target)
		}
		if !resp.Msg.Authoritative || resp.Matched != "dhcp" {
			t.Errorf("%s: aa = %v matched = %q, want an authoritative dhcp answer", c.qname, resp.Msg.Authoritative, resp.Matched)
		}
	}
}

// TestNamesPassesOnWhatItHoldsNoLeaseFor is the whole of what this stage
// must not do: a name nothing leases, a name under the wrong scope's suffix,
// a spelling no lease was given, and an address nothing hands out all belong
// to whatever comes next, untouched.
func TestNamesPassesOnWhatItHoldsNoLeaseFor(t *testing.T) {
	cases := []struct {
		qname string
		qtype uint16
	}{
		{"unknown.home.lan", dns.TypeA},
		{"my-laptop.iot.lan", dns.TypeA},         // the lease is in the lan scope
		{"my_laptop.home.lan", dns.TypeA},        // not the label the lease was given
		{"home.lan", dns.TypeA},                  // the suffix itself is nobody's name
		{"9.9.9.9.in-addr.arpa", dns.TypePTR},    // outside every scope
		{"51.0.42.10.in-addr.arpa", dns.TypePTR}, // inside a scope, nothing leases it
	}
	for _, c := range cases {
		h, passed := namesStage(t, nil, leased("10.42.0.50", "aa:bb:cc:dd:ee:01", "My Laptop", 1, 3600))
		resp := ask(t, h, c.qname, c.qtype)
		if !*passed {
			t.Errorf("%s %s was answered here: %v", dns.TypeToString[c.qtype], c.qname, resp.Msg.Answer)
		}
	}
}

// TestNamesYieldsToAStaticZoneRecord: explicit configuration beats inferred
// state, so a name a zone holds a record for is passed on even when a lease
// would have answered it.
func TestNamesYieldsToAStaticZoneRecord(t *testing.T) {
	var asked []string
	zc := func(name string) bool {
		asked = append(asked, name)
		return name == "my-laptop.home.lan"
	}
	h, passed := namesStage(t, zc,
		leased("10.42.0.50", "aa:bb:cc:dd:ee:01", "My Laptop", 1, 3600),
		leased("10.42.0.51", "aa:bb:cc:dd:ee:03", "nas", 1, 3600),
	)

	if resp := ask(t, h, "my-laptop.home.lan", dns.TypeA); !*passed {
		t.Fatalf("a name a zone holds was answered from the lease table: %v", resp.Msg.Answer)
	}
	*passed = false
	if resp := ask(t, h, "nas.home.lan", dns.TypeA); *passed {
		t.Fatalf("a name no zone holds was passed on: %v", resp.Msg.Answer)
	}
	if len(asked) != 2 {
		t.Fatalf("the zone was asked %v, want both leased names", asked)
	}
	// A name no lease answers is not one this stage has an opinion on, so
	// the zone is not asked about it: that lookup would otherwise sit in
	// front of every forwarded query.
	asked = nil
	ask(t, h, "unknown.home.lan", dns.TypeA)
	if len(asked) != 0 {
		t.Errorf("the zone was asked about a name no lease answers: %v", asked)
	}
}

// TestNamesAnswersNODATAForAnotherTypeOnALeasedName: a name this server hands
// out is this server's name for every type, not just A. Forwarding the AAAA
// of a leased name sends an internal name to the public internet and answers
// it with whatever is out there.
func TestNamesAnswersNODATAForAnotherTypeOnALeasedName(t *testing.T) {
	h, passed := namesStage(t, nil, leased("10.42.0.50", "aa:bb:cc:dd:ee:01", "My Laptop", 1, 3600))

	for _, qtype := range []uint16{dns.TypeAAAA, dns.TypeTXT} {
		resp := ask(t, h, "my-laptop.home.lan", qtype)
		if *passed {
			t.Fatalf("%s of a leased name was forwarded", dns.TypeToString[qtype])
		}
		if len(resp.Msg.Answer) != 0 {
			t.Errorf("%s: answer = %v, want none", dns.TypeToString[qtype], resp.Msg.Answer)
		}
		if resp.Msg.Rcode != dns.RcodeSuccess {
			t.Errorf("%s: rcode = %s, want NOERROR", dns.TypeToString[qtype], dns.RcodeToString[resp.Msg.Rcode])
		}
		if !resp.Msg.Authoritative || resp.Matched != "dhcp" {
			t.Errorf("%s: aa = %v matched = %q, want an authoritative dhcp answer",
				dns.TypeToString[qtype], resp.Msg.Authoritative, resp.Matched)
		}
	}

	// A name no lease answers is still nobody's here, whatever the type.
	if resp := ask(t, h, "unknown.home.lan", dns.TypeAAAA); !*passed {
		t.Errorf("AAAA of a name with no lease was answered: %v", resp.Msg)
	}
	*passed = false
	if resp := ask(t, h, "ghost.home.lan", dns.TypeAAAA); !*passed {
		t.Errorf("AAAA of a name with no lease was answered: %v", resp.Msg)
	}
}

// TestNamesReadsScopesOffTheQueryPath: the scope list and the dhcp.domain
// setting are read when the table changes, not per query. Reading them on the
// query path puts whatever the app does to answer them — a lock, a store
// read — in front of every PTR this server sees.
func TestNamesReadsScopesOffTheQueryPath(t *testing.T) {
	e := newEngine(t, "3.0.3", alpineHooks)
	m := namesManager(t, nil, managerInput(e.Socket()))
	var scopeCalls, domainCalls atomic.Int64
	scopes := func() []store.Scope { scopeCalls.Add(1); return namesScopes() }
	domain := func() string { domainCalls.Add(1); return namesDomain() }
	terminal, passed := recorder()
	h := dnssrv.Chain(terminal, dhcp.Names(m, scopes, domain, nil))

	e.setLeases(leased("10.42.0.50", "aa:bb:cc:dd:ee:01", "My Laptop", 1, 3600))
	if err := m.Poll(t.Context()); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	scopeCalls.Store(0)
	domainCalls.Store(0)

	resp := ask(t, h, "50.0.42.10.in-addr.arpa", dns.TypePTR)
	if *passed || len(resp.Msg.Answer) != 1 {
		t.Fatalf("the PTR was not answered from the poll's snapshot: %v", resp.Msg.Answer)
	}
	ask(t, h, "my-laptop.home.lan", dns.TypeA)
	ask(t, h, "9.9.9.9.in-addr.arpa", dns.TypePTR)
	if n, d := scopeCalls.Load(), domainCalls.Load(); n != 0 || d != 0 {
		t.Errorf("queries read the scopes %d times and the domain %d times, want neither", n, d)
	}
}

// TestNamesTTLIsTheShorterOfFiveMinutesAndTheLease: a name must not outlive
// the lease it came from, and a long lease must not pin one for hours.
func TestNamesTTLIsTheShorterOfFiveMinutesAndTheLease(t *testing.T) {
	h, _ := namesStage(t, nil,
		leased("10.42.0.50", "aa:bb:cc:dd:ee:01", "long", 1, 3600),
		leased("10.42.0.51", "aa:bb:cc:dd:ee:02", "short", 1, 60),
	)

	if ttl := ask(t, h, "long.home.lan", dns.TypeA).Msg.Answer[0].Header().Ttl; ttl != 300 {
		t.Errorf("a 3600s lease answered with TTL %d, want the 300s cap", ttl)
	}
	ttl := ask(t, h, "short.home.lan", dns.TypeA).Msg.Answer[0].Header().Ttl
	if ttl > 60 || ttl < 55 {
		t.Errorf("a 60s lease answered with TTL %d, want what is left of the lease", ttl)
	}
	if ttl := ask(t, h, "51.0.42.10.in-addr.arpa", dns.TypePTR).Msg.Answer[0].Header().Ttl; ttl > 60 || ttl < 55 {
		t.Errorf("the PTR of a 60s lease answered with TTL %d, want what is left of the lease", ttl)
	}
}

// TestNamesSkipsAnExpiredLease: Kea keeps an expired lease in its database
// until it reclaims it, and the address may already be someone else's. It is
// not a name this server hands out, in either direction.
func TestNamesSkipsAnExpiredLease(t *testing.T) {
	expired := leased("10.42.0.50", "aa:bb:cc:dd:ee:01", "ghost", 1, 60)
	expired.CLTT = time.Now().Add(-10 * time.Minute).Unix()

	h, passed := namesStage(t, nil, expired)
	if resp := ask(t, h, "ghost.home.lan", dns.TypeA); !*passed {
		t.Errorf("an expired lease answered: %v", resp.Msg.Answer)
	}
	*passed = false
	if resp := ask(t, h, "50.0.42.10.in-addr.arpa", dns.TypePTR); !*passed {
		t.Errorf("an expired lease answered a PTR: %v", resp.Msg.Answer)
	}
}

// TestNamesAnswersAReservationWithNoLease: a reservation that has a hostname
// is a name before anything has ever leased it, and it has no expiry to cap
// the TTL with.
func TestNamesAnswersAReservationWithNoLease(t *testing.T) {
	e := newEngine(t, "3.0.3", alpineHooks)
	in := managerInput(e.Socket())
	in.Reservations = []store.Reservation{
		{ID: 1, ScopeID: 1, MAC: "aa:bb:cc:dd:ee:09", IP: "10.42.0.10", Hostname: "printer"},
	}
	m := namesManager(t, nil, in)
	if err := m.Poll(t.Context()); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	terminal, _ := recorder()
	h := dnssrv.Chain(terminal, dhcp.Names(m, namesScopes, namesDomain, nil))

	resp := ask(t, h, "printer.home.lan", dns.TypeA)
	if len(resp.Msg.Answer) != 1 {
		t.Fatalf("answer = %v, want the reservation's address", resp.Msg.Answer)
	}
	if a, ok := resp.Msg.Answer[0].(*dns.A); !ok || a.A.String() != "10.42.0.10" || a.Hdr.Ttl != 300 {
		t.Errorf("answer = %v, want 10.42.0.10 at TTL 300", resp.Msg.Answer[0])
	}
}

// TestNamesLogsTheLeaseThatLostItsName: two leases in one scope whose
// hostnames sanitise to the same label — the table keeps one, and the other
// has to be findable, by MAC, in the log. Once per table, on the poll, never
// on the query path.
func TestNamesLogsTheLeaseThatLostItsName(t *testing.T) {
	var buf bytes.Buffer
	e := newEngine(t, "3.0.3", alpineHooks)
	m := namesManager(t, slog.New(slog.NewTextHandler(&buf, nil)), managerInput(e.Socket()))
	dhcp.Names(m, namesScopes, namesDomain, nil)

	e.setLeases(
		leased("10.42.0.50", "aa:bb:cc:dd:ee:01", "My Laptop", 1, 60),
		leased("10.42.0.51", "aa:bb:cc:dd:ee:02", "my laptop!", 1, 3600),
	)
	if err := m.Poll(t.Context()); err != nil {
		t.Fatalf("Poll: %v", err)
	}

	line := buf.String()
	if !strings.Contains(line, "aa:bb:cc:dd:ee:01") || !strings.Contains(line, "aa:bb:cc:dd:ee:02") {
		t.Fatalf("the collision names neither MAC: %q", line)
	}
	if n := strings.Count(line, "level=WARN"); n != 1 {
		t.Errorf("logged %d warnings for one collision, want 1: %q", n, line)
	}
}
