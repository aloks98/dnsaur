package app

import (
	"context"
	"net"
	"net/netip"
	"slices"
	"strings"
	"testing"

	"github.com/aloks98/dnsaur/internal/store"
	"github.com/aloks98/dnsaur/internal/zones"
	"github.com/miekg/dns"
)

// answerAddrs answers A and AAAA for name and NXDOMAIN for every other name,
// which is what makes "the lookup went through the forwarder" and "the lookup
// went somewhere else" distinguishable: nothing on the machine running the
// test answers for a name under `.test` (RFC 6761).
func answerAddrs(name, v4, v6 string) dns.HandlerFunc {
	return func(w dns.ResponseWriter, m *dns.Msg) {
		r := new(dns.Msg)
		r.SetReply(m)
		r.Authoritative = true
		q := m.Question[0]
		if !strings.EqualFold(q.Name, dns.Fqdn(name)) {
			r.Rcode = dns.RcodeNameError
			_ = w.WriteMsg(r)
			return
		}
		switch q.Qtype {
		case dns.TypeA:
			rr, _ := dns.NewRR(dns.Fqdn(name) + " 300 IN A " + v4)
			r.Answer = []dns.RR{rr}
		case dns.TypeAAAA:
			rr, _ := dns.NewRR(dns.Fqdn(name) + " 300 IN AAAA " + v6)
			r.Answer = []dns.RR{rr}
		}
		_ = w.WriteMsg(r)
	}
}

func addrStrings(addrs []netip.Addr) []string {
	out := make([]string, 0, len(addrs))
	for _, a := range addrs {
		out = append(out, a.String())
	}
	slices.Sort(out)
	return out
}

// The adapter is a *net.Resolver replacement, so it owes the same answers:
// every address a name has for "ip", and only the family asked for otherwise.
// The upstream here is the only thing in the process that can answer, so an
// answer at all proves the query went through the forwarder.
func TestZoneLookupResolvesThroughTheForwarder(t *testing.T) {
	up := mockDNS(t, answerAddrs("ns1.primary.test", "10.0.0.1", "2001:db8::1"))
	a := newTestApp(t, withUpstreams(up))
	l := a.zoneLookup()

	for _, tc := range []struct {
		network string
		want    []string
	}{
		{"ip", []string{"10.0.0.1", "2001:db8::1"}},
		{"ip4", []string{"10.0.0.1"}},
		{"ip6", []string{"2001:db8::1"}},
	} {
		got, err := l.LookupNetIP(context.Background(), tc.network, "ns1.primary.test")
		if err != nil {
			t.Fatalf("LookupNetIP(%q): %v", tc.network, err)
		}
		if s := addrStrings(got); !slices.Equal(s, tc.want) {
			t.Errorf("LookupNetIP(%q) = %v, want %v", tc.network, s, tc.want)
		}
	}

	if _, err := l.LookupNetIP(context.Background(), "unix", "ns1.primary.test"); err == nil {
		t.Error("LookupNetIP with an address family that is not an IP one returned no error")
	}
}

// A name that does not resolve has to fail the way the four call sites
// already handle: an error, which ParsePrimaries turns into "no primary
// resolved" while naming the entry that failed. Anything else — an empty
// slice with a nil error above all — would report a list of primaries that
// nothing tried to dial as a list that was tried and refused.
func TestZoneLookupFailureReadsLikeAResolverFailure(t *testing.T) {
	up := mockDNS(t, answerAddrs("ns1.primary.test", "10.0.0.1", "2001:db8::1"))
	a := newTestApp(t, withUpstreams(up))
	l := a.zoneLookup()

	got, err := l.LookupNetIP(context.Background(), "ip", "gone.primary.test")
	if err == nil {
		t.Fatalf("LookupNetIP for a name with no answer = %v, want an error", got)
	}

	_, err = zones.ParsePrimaries(context.Background(), l, "gone.primary.test")
	if err == nil {
		t.Fatal("ParsePrimaries over a failing lookup returned no error")
	}
	for _, want := range []string{"no primary resolved", "gone.primary.test"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to contain %q", err, want)
		}
	}
}

// The bootstrap case the OS resolver can never serve: a secondary whose only
// primary is named *inside the zone it has not transferred yet*. Resolved
// through the host's resolver — which on a machine running dnsaur is usually
// dnsaur — the name lands on this very server's zones stage, where a
// secondary that has never transferred SERVFAILs, so the transfer that would
// fix that is the one thing that cannot happen. Entered below the zones
// stage, the same name resolves from the configured upstreams and the zone
// bootstraps.
//
// Both ends are real: a dnsaur primary serving the AXFR to a dnsaur
// secondary pulling it, the loopback pairing internal/zones uses, with the
// name resolution in between the thing under test.
func TestASecondaryBootstrapsFromAPrimaryNamedInsideItsOwnZone(t *testing.T) {
	const apex = "xfer.test"
	ctx := context.Background()

	primary := newTestApp(t)
	pid := mustAddZone(t, primary, store.Zone{
		Name: apex, Type: "primary", Enabled: true, AllowTransfer: "127.0.0.0/8",
		SOANS: "ns1." + apex, SOAMbox: "hostmaster." + apex,
		SOASerial: 42, SOARefresh: 900, SOARetry: 300, SOAExpire: 604800,
		SOAMinimum: 900, SOATTL: 900,
	})
	for _, r := range []store.ZoneRecord{
		{ZoneID: pid, Name: "@", Type: "NS", TTL: 300, RData: "ns1." + apex + ".", Enabled: true},
		{ZoneID: pid, Name: "ns1", Type: "A", TTL: 300, RData: "127.0.0.1", Enabled: true},
		{ZoneID: pid, Name: "bifrost", Type: "A", TTL: 300, RData: "10.20.0.2", Enabled: true},
	} {
		if _, err := primary.Store().Zones().AddRecord(ctx, r); err != nil {
			t.Fatalf("AddRecord(%s): %v", r.Name, err)
		}
	}
	mustReloadZones(t, primary)

	_, port, err := net.SplitHostPort(primary.DNSAddr())
	if err != nil {
		t.Fatal(err)
	}

	// The upstream the secondary forwards to knows ns1.xfer.test. Its own
	// zones stage does not — the zone is there, claiming the suffix, with
	// nothing transferred into it.
	up := mockDNS(t, answerAddrs("ns1."+apex, "127.0.0.1", "2001:db8::1"))
	sec := newTestApp(t, withUpstreams(up))
	sid := mustAddZone(t, sec, store.Zone{
		Name: apex, Type: "secondary", Enabled: true,
		Primaries: "ns1." + apex + ":" + port,
		SOANS:     "ns1." + apex, SOAMbox: "hostmaster." + apex,
		SOASerial: 1, SOARefresh: 900, SOARetry: 300, SOAExpire: 604800,
		SOAMinimum: 900, SOATTL: 900,
	})
	mustReloadZones(t, sec)

	if got := digRcodeQuiet(sec.DNSAddr(), "ns1."+apex); got != dns.RcodeServerFailure {
		t.Fatalf("precondition: the secondary answered %s for its own ns1, want SERVFAIL — "+
			"the name has to be one its zones stage cannot resolve",
			dns.RcodeToString[got])
	}

	res, err := sec.zoneRefresh.Refresh(ctx, sid)
	if err != nil {
		t.Fatalf("refreshing a secondary whose primary is named inside its own zone: %v", err)
	}
	if res.Serial != 42 {
		t.Errorf("transferred serial = %d, want the primary's 42", res.Serial)
	}
	if got := askApp(t, sec, "bifrost."+apex); got != "10.20.0.2" {
		t.Errorf("after the transfer the secondary answered %q for bifrost.%s, want 10.20.0.2", got, apex)
	}
}
