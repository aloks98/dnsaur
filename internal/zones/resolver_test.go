package zones_test

import (
	"context"
	"sync"
	"sync/atomic"
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
func (f *fakeZoneStore) NoteTransferRequest(ctx context.Context, zoneID, at int64, peer, errText string) error {
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

// parkingZoneStore holds the *first* Reload between its two store reads,
// which is the window Resolver.Reload's lost update lives in. Every later
// read runs straight through, so a second Reload is free to overtake the
// parked one — that overtaking is the whole experiment.
type parkingZoneStore struct {
	*fakeZoneStore

	mu    sync.Mutex
	zones []store.Zone

	reads   atomic.Int64
	parked  chan struct{} // closed once the first reload has read the list
	release chan struct{} // closed to let it continue
}

func (p *parkingZoneStore) Zones(ctx context.Context) ([]store.Zone, error) {
	p.mu.Lock()
	out := append([]store.Zone(nil), p.zones...)
	p.mu.Unlock()
	if p.reads.Add(1) == 1 {
		close(p.parked)
		<-p.release
	}
	return out, nil
}

func (p *parkingZoneStore) add(z store.Zone) {
	p.mu.Lock()
	p.zones = append(p.zones, z)
	p.mu.Unlock()
}

// Two concurrent Reloads must not lose the later one's zones.
//
// Reload derives its whole Index from the store and swaps it in atomically,
// which is what makes it safe against *tearing*: no reader ever sees half an
// Index. It is not safe against *ordering*. Let two reloads overlap and the
// one that read the store first can Store its Index last, so every zone that
// appeared between the two reads is gone from what the server serves — and
// gone permanently, since nothing revisits it until something else reloads.
//
// The consequence is not a stale answer. Index.Find stops claiming the zone
// altogether, so a name inside it falls through to the forwarder and the
// public internet answers for a name this server holds. That is the §9.11.5
// leak reached by another route, and it costs a *primary* zone its own names,
// not just a forwarder zone its routing.
//
// None of this is hypothetical or specific to how App calls it.
// Refresher.RefreshDue starts a goroutine per due secondary and every install
// reloads, so concurrent whole-store reloads are what that path does by
// design. sqlite's single connection does not prevent it either:
// SetMaxOpenConns(1) serialises individual queries, while Reload's two reads
// are separate QueryContext calls with the connection released between them
// and no transaction around either, and it governs nothing about snap.Store.
//
// The store parks the first reload rather than leaving the interleaving to
// chance, so this fails on every run rather than one run in ten.
func TestConcurrentReloadsDoNotLoseAZone(t *testing.T) {
	ctx := context.Background()
	ps := &parkingZoneStore{
		fakeZoneStore: &fakeZoneStore{records: map[int64][]store.ZoneRecord{}},
		zones: []store.Zone{{
			ID: 1, Name: testApex, Type: "primary", Enabled: true,
			SOANS: "ns1." + testApex, SOAMbox: "hostadmin." + testApex, SOASerial: 1,
		}},
		parked:  make(chan struct{}),
		release: make(chan struct{}),
	}
	r := zones.NewResolver(ps)

	// A: reads the zone list, then parks before it can build or store.
	var first sync.WaitGroup
	first.Add(1)
	go func() {
		defer first.Done()
		if err := r.Reload(ctx); err != nil {
			t.Errorf("first Reload: %v", err)
		}
	}()
	<-ps.parked

	// B: a zone appears and is reloaded while A is still parked. This is an
	// API zone create, or a secondary finishing its transfer.
	const added = "added.test"
	ps.add(store.Zone{ID: 2, Name: added, Type: "primary", Enabled: true, SOASerial: 1})
	done := make(chan struct{})
	var second sync.WaitGroup
	second.Add(1)
	go func() {
		defer second.Done()
		defer close(done)
		if err := r.Reload(ctx); err != nil {
			t.Errorf("second Reload: %v", err)
		}
	}()

	// Give B room to overtake. Serialised it cannot, and being unable to is
	// the fix — so this waits rather than requiring the overtake to happen.
	select {
	case <-done:
	case <-time.After(250 * time.Millisecond):
	}

	close(ps.release)
	first.Wait()
	second.Wait()

	if r.Snapshot().Apex(added) == nil {
		t.Errorf("%s is gone from the served snapshot: the reload that read the store first stored its Index last, and every name in that zone now falls through to the forwarder", added)
	}
}

// The same property as TestConcurrentReloadsDoNotLoseAZone, against a real
// store on both drivers.
//
// That test parks a *fake* store, which is what makes it deterministic — and
// also what makes it say nothing about either driver. The lost update it
// pins is an ordering bug, so the store underneath is irrelevant to whether
// it reproduces; what is not irrelevant is the claim Reload's comment makes
// about why the window is open at all. It says sqlite's SetMaxOpenConns(1)
// (internal/store/store.go) does *not* serialise the two reads, because they
// are separate QueryContext calls with the connection released in between.
// Only a real sqlite store can show that, and only a real postgres one can
// show the other half: postgres sets no such limit, so its two reads are on
// two connections and genuinely overlap.
//
// So: a real store, parked at the same seam (gatedZoneStore.holdList, just
// after the whole-zone list read returns), so this fails on every run rather
// than one run in ten — on both drivers. If the sqlite connection *did*
// serialise the reads, the second reload here could never overtake the first
// and this test would deadlock on its own gate rather than pass.
//
// Parked rather than left to chance deliberately, and that was measured, not
// assumed: an unparked version of this — two reloads simply started together
// with a zone appearing between them, repeated — was written and thrown away
// because it could not be made to fail for the right reason. With rmu
// removed it still passed at 40 rounds, and only failed on two runs in three
// at 600, by which point it cost ~10s. An assertion that misses the bug a
// third of the time is not coverage, so it is not here.
func TestConcurrentReloadsAgainstARealStoreDoNotLoseAZone(t *testing.T) {
	forEachDriver(t, func(t *testing.T, driver string) {
		ctx := context.Background()
		st := openTestStoreOn(t, driver)

		// Held after the first reload has read the zone list and before it
		// can build or store its Index. Every later read runs straight
		// through, so the second reload is free to overtake.
		hold := newOneShotHold()
		defer hold.free()
		zs := &gatedZoneStore{ZoneStore: st.Zones(), hold: func() {}, holdList: hold.hold}
		r := zones.NewResolver(zs)

		var first sync.WaitGroup
		first.Add(1)
		go func() {
			defer first.Done()
			if err := r.Reload(ctx); err != nil {
				t.Errorf("first Reload: %v", err)
			}
		}()
		hold.wait(t, "the first reload's zone list read")

		// A zone appears and is reloaded while the first is still parked:
		// an API zone create, or a secondary finishing its transfer.
		const added = "added.test"
		if _, err := st.Zones().AddZone(ctx, store.Zone{
			ID: 0, Name: added, Type: "primary", Enabled: true,
			SOANS: "ns1." + added, SOAMbox: "hostadmin." + added,
			SOASerial: 1, SOARefresh: 900, SOARetry: 300, SOAExpire: 604800,
			SOAMinimum: 900, SOATTL: 900,
		}); err != nil {
			t.Fatalf("AddZone: %v", err)
		}
		done := make(chan struct{})
		var second sync.WaitGroup
		second.Add(1)
		go func() {
			defer second.Done()
			defer close(done)
			if err := r.Reload(ctx); err != nil {
				t.Errorf("second Reload: %v", err)
			}
		}()

		// Room to overtake. Serialised it cannot, and being unable to is the
		// fix — so this waits rather than requiring the overtake to happen.
		select {
		case <-done:
		case <-time.After(250 * time.Millisecond):
		}

		hold.free()
		first.Wait()
		second.Wait()

		if r.Snapshot().Apex(added) == nil {
			t.Errorf("%s is gone from the served snapshot: the reload that read the store first stored its Index last, and every name in that zone now falls through to the forwarder", added)
		}
	})
}
