package zones_test

import (
	"context"
	"testing"

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
