package zones_test

import (
	"context"
	"testing"
	"time"

	"github.com/aloks98/dnsaur/internal/dnssrv"
	"github.com/aloks98/dnsaur/internal/store"
	"github.com/aloks98/dnsaur/internal/zones"
	"github.com/miekg/dns"
)

// fakeZoneStore is the store.ZoneStore this package's tests build a Resolver
// against. Only Zones and AllRecords are exercised — Reload is the sole
// caller of either — so the rest return zero values.
type fakeZoneStore struct {
	zones   []store.Zone
	records map[int64][]store.ZoneRecord
}

func (f *fakeZoneStore) Zones(ctx context.Context) ([]store.Zone, error) { return f.zones, nil }
func (f *fakeZoneStore) Zone(ctx context.Context, id int64) (store.Zone, error) {
	return store.Zone{}, nil
}
func (f *fakeZoneStore) AddZone(ctx context.Context, z store.Zone) (int64, error) { return 0, nil }
func (f *fakeZoneStore) UpdateZone(ctx context.Context, z store.Zone) error       { return nil }
func (f *fakeZoneStore) DeleteZone(ctx context.Context, id int64) error           { return nil }
func (f *fakeZoneStore) BumpSerial(ctx context.Context, zoneID int64) error       { return nil }
func (f *fakeZoneStore) Records(ctx context.Context, zoneID int64) ([]store.ZoneRecord, error) {
	return f.records[zoneID], nil
}
func (f *fakeZoneStore) AllRecords(ctx context.Context) (map[int64][]store.ZoneRecord, error) {
	return f.records, nil
}
func (f *fakeZoneStore) AddRecord(ctx context.Context, r store.ZoneRecord) (int64, error) {
	return 0, nil
}
func (f *fakeZoneStore) UpdateRecord(ctx context.Context, r store.ZoneRecord) error { return nil }
func (f *fakeZoneStore) DeleteRecord(ctx context.Context, id int64) error           { return nil }
func (f *fakeZoneStore) ReplaceRecords(ctx context.Context, z store.Zone, deleteIDs []int64, updates, adds []store.ZoneRecord) error {
	return nil
}
func (f *fakeZoneStore) NoteTransferAttempt(ctx context.Context, zoneID, at int64, errText string) error {
	return nil
}

// resolverWith builds a Resolver whose only zone is the enabled primary zone
// e412.in, holding recs, reloaded once so its snapshot is populated.
func resolverWith(t *testing.T, recs ...store.ZoneRecord) *zones.Resolver {
	t.Helper()
	fs := &fakeZoneStore{
		zones: []store.Zone{{
			ID: 1, Name: testApex, Type: "primary", Enabled: true,
			SOANS: "ns1." + testApex, SOAMbox: "hostadmin." + testApex,
			SOASerial: 1, SOARefresh: 900, SOARetry: 300, SOAExpire: 604800,
			SOAMinimum: 900, SOATTL: 900,
		}},
		records: map[int64][]store.ZoneRecord{1: recs},
	}
	r := zones.NewResolver(fs)
	if err := r.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	return r
}

// request builds a dnssrv.Request asking name/qtype, with nothing else set —
// what the pipeline hands every middleware.
func request(name string, qtype uint16) *dnssrv.Request {
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(name), qtype)
	return &dnssrv.Request{Msg: m}
}

func TestMiddlewarePassesUncoveredNamesThrough(t *testing.T) {
	called := false
	next := dnssrv.HandlerFunc(func(ctx context.Context, req *dnssrv.Request) (*dnssrv.Response, error) {
		called = true
		return &dnssrv.Response{Msg: new(dns.Msg), Decision: dnssrv.DecisionForwarded}, nil
	})
	r := resolverWith(t /* zone e412.in */)
	_, _ = r.Middleware()(next).ServeDNS(t.Context(), request("example.com.", dns.TypeA))
	if !called {
		t.Error("a name outside every zone must reach the next handler")
	}
}

func TestMiddlewareDoesNotForwardInsideAZone(t *testing.T) {
	called := false
	next := dnssrv.HandlerFunc(func(ctx context.Context, req *dnssrv.Request) (*dnssrv.Response, error) {
		called = true
		return &dnssrv.Response{Msg: new(dns.Msg)}, nil
	})
	r := resolverWith(t, store.ZoneRecord{Name: "bifrost", Type: "A", TTL: 3600, RData: "57.129.69.158", Enabled: true}) // zone e412.in with only bifrost
	resp, _ := r.Middleware()(next).ServeDNS(t.Context(), request("nothere.e412.in.", dns.TypeA))
	if called {
		t.Fatal("an undefined name inside our zone was forwarded upstream — the leak this milestone exists to close")
	}
	if resp.Msg.Rcode != dns.RcodeNameError {
		t.Errorf("rcode = %d, want NXDOMAIN", resp.Msg.Rcode)
	}
	if resp.Decision != dnssrv.DecisionAuthoritative {
		t.Errorf("decision = %q, want authoritative", resp.Decision)
	}
}

// The middleware must not treat "this secondary has nothing to say" as "this
// name is not covered". Falling through would send a name inside a zone we
// claim to the upstream forwarder — the leak zones exist to close — and a
// split-horizon secondary would answer with the public record it was created
// to shadow.
func TestMiddlewareDoesNotForwardForANeverTransferredSecondary(t *testing.T) {
	called := false
	next := dnssrv.HandlerFunc(func(ctx context.Context, req *dnssrv.Request) (*dnssrv.Response, error) {
		called = true
		return &dnssrv.Response{Msg: new(dns.Msg), Decision: dnssrv.DecisionForwarded}, nil
	})
	fs := &fakeZoneStore{
		zones: []store.Zone{{
			ID: 1, Name: testApex, Type: "secondary", Enabled: true,
			SOANS: "ns1." + testApex, SOAMbox: "hostadmin." + testApex,
			SOASerial: 1, SOARefresh: 900, SOARetry: 300, SOAExpire: 604800,
			SOAMinimum: 900, SOATTL: 900,
			Primaries: "192.168.150.5", // RefreshedAt 0: no transfer has landed
		}},
		records: map[int64][]store.ZoneRecord{},
	}
	r := zones.NewResolver(fs)
	if err := r.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	resp, err := r.Middleware()(next).ServeDNS(t.Context(), request("bifrost.e412.in.", dns.TypeA))
	if err != nil {
		t.Fatal(err)
	}
	if called {
		t.Error("a name inside a secondary must not reach the forwarder")
	}
	if resp.Msg.Rcode != dns.RcodeServerFailure {
		t.Errorf("rcode = %d, want SERVFAIL", resp.Msg.Rcode)
	}
	if resp.Msg.Authoritative {
		t.Error("aa = true; the zone holds nothing to be authoritative about")
	}
	// And the query log must not call it an authoritative answer: an admin
	// reading decision=authoritative beside rcode=SERVFAIL would go looking
	// for a defect in the zone rather than for a transfer that has not
	// happened.
	if resp.Decision != dnssrv.DecisionError {
		t.Errorf("decision = %q, want %q", resp.Decision, dnssrv.DecisionError)
	}
}

// secondaryResolver builds a Resolver over one secondary zone that has
// transferred (refreshedAt) and expires at expiresAt, holding one A record.
func secondaryResolver(t *testing.T, refreshedAt, expiresAt int64, opts ...zones.ResolverOption) *zones.Resolver {
	t.Helper()
	fs := &fakeZoneStore{
		zones: []store.Zone{{
			ID: 1, Name: testApex, Type: "secondary", Enabled: true,
			SOANS: "ns1." + testApex, SOAMbox: "hostadmin." + testApex,
			SOASerial: 1, SOARefresh: 900, SOARetry: 300, SOAExpire: 604800,
			SOAMinimum: 900, SOATTL: 900,
			Primaries: "192.168.150.5", RefreshedAt: refreshedAt, ExpiresAt: expiresAt,
		}},
		records: map[int64][]store.ZoneRecord{1: {
			{Name: "bifrost", Type: "A", TTL: 3600, RData: "57.129.69.158", Enabled: true},
		}},
	}
	r := zones.NewResolver(fs, opts...)
	if err := r.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	return r
}

func rcodeFor(t *testing.T, r *zones.Resolver, name string) int {
	t.Helper()
	next := dnssrv.HandlerFunc(func(ctx context.Context, req *dnssrv.Request) (*dnssrv.Response, error) {
		t.Error("a name inside a zone must not reach the next handler")
		return &dnssrv.Response{Msg: new(dns.Msg)}, nil
	})
	resp, err := r.Middleware()(next).ServeDNS(t.Context(), request(name, dns.TypeA))
	if err != nil {
		t.Fatal(err)
	}
	return resp.Msg.Rcode
}

// The clock is a seam, not a comment. WithNow is what Task 3's scheduler
// needs to drive expiry without waiting for it, so this drives a zone across
// its own deadline by moving the clock and nothing else — same zone, same
// snapshot, two different answers.
func TestResolverClockIsInjectable(t *testing.T) {
	const deadline int64 = 1786000000000
	clock := deadline - 1000
	r := secondaryResolver(t, deadline-100000, deadline, zones.WithNow(func() time.Time {
		return time.UnixMilli(clock)
	}))

	if got := rcodeFor(t, r, "bifrost.e412.in."); got != dns.RcodeSuccess {
		t.Fatalf("before the deadline: rcode = %d, want NOERROR", got)
	}
	clock = deadline
	if got := rcodeFor(t, r, "bifrost.e412.in."); got != dns.RcodeServerFailure {
		t.Fatalf("at the deadline: rcode = %d, want SERVFAIL — the clock is not being read", got)
	}
}

// And the default clock is the real one. Without this, WithNow could be the
// only path that works and every deployment would read whatever constant the
// default happened to be — the two zones here are far enough either side of
// now that no plausible wrong default answers both correctly.
func TestResolverDefaultClockIsWallTime(t *testing.T) {
	now := time.Now().UnixMilli()

	fresh := secondaryResolver(t, now-3600_000, now+3600_000)
	if got := rcodeFor(t, fresh, "bifrost.e412.in."); got != dns.RcodeSuccess {
		t.Errorf("a zone that expires in an hour: rcode = %d, want NOERROR", got)
	}
	stale := secondaryResolver(t, now-7200_000, now-3600_000)
	if got := rcodeFor(t, stale, "bifrost.e412.in."); got != dns.RcodeServerFailure {
		t.Errorf("a zone that expired an hour ago: rcode = %d, want SERVFAIL", got)
	}
}
