package zones

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aloks98/dnsaur/internal/dnssrv"
	"github.com/aloks98/dnsaur/internal/store"
	"github.com/miekg/dns"
)

// Resolver serves queries authoritatively from the zones this server holds.
// Its snapshot is rebuilt wholesale on every Reload and swapped in
// atomically, so a query mid-flight always sees one consistent Index rather
// than a mix of old and new zone data.
type Resolver struct {
	zs   store.ZoneStore
	snap atomic.Pointer[Index]
	// rmu serialises Reload against itself. The snapshot is derived whole
	// from the store, which makes the atomic swap enough to stop a reader
	// seeing a torn Index -- but not enough to order two writers. See Reload.
	rmu sync.Mutex
	// now is the clock Answer is given, injected rather than read inside
	// the zone so "this secondary's data has expired" is a decision a test
	// can drive instead of one it has to wait for. Same purpose as
	// filter.Refresher's field of the same name, but reachable through
	// WithNow rather than by assignment: this package's tests are external
	// (package zones_test), so an unexported field alone would make the
	// seam decorative — nothing outside could drive it, and a broken clock
	// would pass every test.
	now func() time.Time
}

// ResolverOption configures a Resolver at construction.
type ResolverOption func(*Resolver)

// WithNow replaces the clock the Resolver reads when deciding whether a
// secondary's data has expired. Tests use it to cross a deadline without
// waiting for one; production leaves it alone and gets time.Now.
func WithNow(now func() time.Time) ResolverOption {
	return func(r *Resolver) { r.now = now }
}

// NewResolver returns a Resolver with an empty snapshot; call Reload before
// serving traffic.
func NewResolver(zs store.ZoneStore, opts ...ResolverOption) *Resolver {
	r := &Resolver{zs: zs, now: time.Now}
	for _, opt := range opts {
		opt(r)
	}
	r.snap.Store(NewIndex(nil))
	return r
}

// Reload rebuilds the snapshot from zs: every zone and, in one query, every
// zone's records, keyed by zone ID so each zone's rows can be matched back
// to it. Each zone is built through NewZone, so the disabled-record rule it
// enforces is applied here exactly as it is everywhere else that serves a
// zone.
//
// Whole-store rebuild, deliberately, even though the refresh scheduler
// (refresh.go) now calls this once per zone per successful transfer rather
// than once per human edit. That was a reason to measure, not a reason to
// assume a per-zone path is needed — see
// internal/zones/reload_bench_test.go, committed so this decision is
// re-checkable rather than taken on faith.
//
// The spec (docs/superpowers/specs/2026-08-08-zones-design.md) never gives a
// homelab a size in one place; it describes one as "single-digit zone
// counts" (§4) of "tens to hundreds of records" (§9.6) — call it ~5 zones of
// ~100. 20 zones of 500 records is a deliberately conservative fixture, an
// order of magnitude above that, chosen so the measurement would not flatter
// the decision. At that size Reload costs ~30ms, ~90% of which is the two
// store reads (BenchmarkReloadStoreRead) and ~7% is rebuilding the Index in
// memory (BenchmarkReloadIndexBuild). That split holds however the same
// 10,000 records are spread across zones — one zone of 10,000 costs about
// the same as 200 zones of 50 — so it is total record count that drives the
// cost, not zone count, and a per-zone path would only ever shave the ~7%
// half. At five of these already-conservative fixtures on one server (50
// zones, 50,000 records) it is still ~155-160ms, and the store read's share
// stays dominant there too, ~90-97% depending on hardware.
//
// The worst realistic case is several secondaries finishing their transfers
// in the same scheduler tick once the startup spread (refresh.go,
// startupSpread) has expired, each triggering a whole-store Reload —
// Refresher.RefreshDue starts one goroutine per due zone, so those reloads
// are concurrent by design. Measured too (BenchmarkReloadConcurrent): end to
// end they cost no more than the same calls made one after another, nowhere
// near the 60-second floor (refresh.go, minInterval) under how often any one
// zone can trigger this.
//
// **That cost measurement is not a safety argument, and this comment used to
// read as though it were.** It said sqlite's single connection
// (internal/store/store.go, SetMaxOpenConns(1)) meant concurrent Reloads
// "cannot overlap". They can, and did. SetMaxOpenConns(1) serialises
// individual queries; the two reads below are separate QueryContext calls
// with the connection released in between and no transaction around them, and
// it says nothing at all about snap.Store. Postgres does not set it in the
// first place. The lost update that claim concealed is what rmu now prevents.
//
// And none of it sits on the query path: Middleware reads r.snap, an atomic
// pointer, and never touches zs, so a Reload in progress burns CPU and the
// store's connection beside query answering, not instead of it. If a future
// deployment's scale looks nothing like these numbers, re-run the
// benchmark before reaching for a per-zone rebuild on the strength of this
// comment alone.
// Reload is serialised against itself by rmu, held across both reads and the
// swap. Two overlapping reloads would each derive a complete Index — no
// tearing — but the one that read the store *first* could store its Index
// *last*, and every zone that appeared between the two reads would vanish
// from what the server serves, permanently, until something reloaded again.
// Index.Find would stop claiming those zones, so their names would fall
// through to the forwarder and the public internet would answer for a name
// this server holds (§9.11.5), a primary's own names included. A lost update
// rather than a data race, so -race never saw it.
//
// **What rmu does not do is make the two reads one transaction, and that is a
// separate window it deliberately leaves open.** A reload racing a *writer*
// can read Zones() before a write and AllRecords() after it, and serve a
// newer record set against an older zone row. It is transient and
// self-repairing — every write reaches here again through
// api.Server.reloadZones, and the next pass installs a pair that agrees —
// where the lost update above was permanent, which is the whole reason one is
// closed here and the other is not.
//
// Readers are untouched: Snapshot and Middleware load the atomic pointer and
// take no lock, so a query never waits on a reload.
func (r *Resolver) Reload(ctx context.Context) error {
	r.rmu.Lock()
	defer r.rmu.Unlock()
	zs, err := r.zs.Zones(ctx)
	if err != nil {
		return err
	}
	all, err := r.zs.AllRecords(ctx)
	if err != nil {
		return err
	}
	built := make([]Zone, len(zs))
	for i, z := range zs {
		built[i] = NewZone(z, all[z.ID])
	}
	r.snap.Store(NewIndex(built))
	return nil
}

// Snapshot returns the Index currently being served — the same one
// Middleware answers queries from, and the whole of what a zone transfer
// reads. Serving a transfer from here rather than from the store is what
// makes what a secondary receives, by construction, what a querier is being
// answered from: disabled records were dropped at snapshot build, and a
// transfer that overlaps a Reload sees one consistent zone rather than a
// mixture.
//
// **The Index and everything reachable from it are read-only.** It is shared
// by every goroutine answering a query and every goroutine serving a
// transfer, and the only synchronisation it has is the atomic swap that
// installed it: a caller that writes through it (assigning to a Zone's
// Records, say) corrupts what is being served, with no lock anywhere to
// notice. Reload replaces the whole Index rather than editing the one in
// place, which is the pattern every caller has to hold to.
func (r *Resolver) Snapshot() *Index { return r.snap.Load() }

// Middleware finds the zone authoritative for the queried name and answers
// from it. A name outside every zone we hold falls through to next — that is
// the only case in which this middleware forwards anything. A name inside a
// zone we hold is always answered here, positively or negatively, and never
// reaches next: that is what closes the leak this milestone exists for.
func (r *Resolver) Middleware() dnssrv.Middleware {
	return func(next dnssrv.Handler) dnssrv.Handler {
		return dnssrv.HandlerFunc(func(ctx context.Context, req *dnssrv.Request) (*dnssrv.Response, error) {
			idx := r.snap.Load()
			z := idx.Find(req.QName())
			if z == nil {
				return next.ServeDNS(ctx, req)
			}
			m := new(dns.Msg)
			m.SetReply(req.Msg)
			if !z.Answer(m, req.QName(), req.QType(), r.now().UnixMilli()) {
				// A forwarder or stub zone names somewhere else to ask rather
				// than holding data, so it declines here. **The fall-through
				// is deliberate and permanent, not a stage waiting to be
				// written**, and it is not the same as the name being
				// uncovered: it routes the query through the *cache* on its
				// way to the terminal forwarder, which holds a routing table
				// keyed on exactly these zones' apexes
				// (upstream.Forwarder.SetConditional, installed by
				// app.ReloadZones) and sends it to that zone's own upstreams,
				// never to the defaults.
				//
				// The cache in between may answer it first, which is the
				// whole point of routing through it — and the routing table
				// is below the cache, so a hit is served without the table
				// being consulted. What makes that safe rather than a hole is
				// that installing the table also purges the suffixes it
				// changed (cache.Purge, from app.installConditional), so the
				// only entries left beneath a claimed suffix are ones its own
				// route produced. Answering here instead — the shape
				// that looks tidier, since this middleware already found the
				// zone — would short-circuit the cache and re-ask the
				// corporate resolver on every repeat query. See §9.11.4.
				return next.ServeDNS(ctx, req)
			}
			// A zone that answered SERVFAIL did not answer authoritatively —
			// it declined to, which is the whole point (see Zone.Serving).
			// Logging it as "authoritative" would put decision=authoritative
			// beside rcode=SERVFAIL in the query log, and an admin reading
			// that row would be looking for a zone defect rather than a
			// secondary that has not transferred. DecisionError is the same
			// label the pipeline's own failure path uses for a SERVFAIL.
			decision := dnssrv.DecisionAuthoritative
			if m.Rcode == dns.RcodeServerFailure {
				decision = dnssrv.DecisionError
			}
			return &dnssrv.Response{Msg: m, Decision: decision}, nil
		})
	}
}
