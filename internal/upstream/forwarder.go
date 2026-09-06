package upstream

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aloks98/dnsaur/internal/dnssrv"
	"github.com/aloks98/dnsaur/internal/filter"
	"github.com/miekg/dns"
)

type Config struct {
	Upstreams   []string
	Strategy    string
	Timeout     time.Duration
	Conditional map[string][]string
}

type up struct {
	addr      string // Upstream.Canonical — identity, display, reuse key
	ex        exchanger
	ewmaMicro atomic.Int64
	fails     atomic.Int32
	downUntil atomic.Int64 // unix nano
}

// encryptedTimeout is the floor for a DoT or DoH exchange.
//
// A warm pooled connection answers in one round trip, well inside the 2s
// default. A cold one pays a TCP handshake and a TLS handshake first, and
// 2s is tight enough that the first query to a distant resolver fails on a
// configuration that is working. Only raised, never lowered: a caller
// asking for more still gets it.
const encryptedTimeout = 5 * time.Second

func newUp(u Upstream, timeout time.Duration) *up {
	switch u.Scheme {
	case SchemeDoT:
		return &up{addr: u.Canonical, ex: newDoTExchanger(u, max(timeout, encryptedTimeout), nil)}
	case SchemeDoH:
		return &up{addr: u.Canonical, ex: newDoHExchanger(u, max(timeout, encryptedTimeout), nil)}
	default:
		return &up{addr: u.Canonical, ex: newPlainExchanger(u.Addr, timeout)}
	}
}

func (u *up) healthy(now time.Time) bool { return now.UnixNano() >= u.downUntil.Load() }

// markResult records the outcome of one exchange. On failure it also
// clamps ewmaMicro up to at least timeoutMicro so a currently-failing
// upstream can't keep winning "fastest" sorts on a stale/cold-start ewma
// of 0. Genuinely-new upstreams (ewma 0, zero failures so far) still sort
// first and get probed once — that's intended, not a bug.
func (u *up) markResult(ok bool, latency time.Duration, now time.Time, timeoutMicro int64) {
	if ok {
		u.fails.Store(0)
		old := u.ewmaMicro.Load()
		u.ewmaMicro.Store((old*7 + latency.Microseconds()) / 8)
		return
	}
	u.ewmaMicro.Store(max(u.ewmaMicro.Load(), timeoutMicro))
	if u.fails.Add(1) >= 3 {
		u.downUntil.Store(now.Add(15 * time.Second).UnixNano())
		u.fails.Store(0)
	}
}

type failKey struct {
	name  string
	qtype uint16
}

// condTable is the conditional routing table: immutable once built, replaced
// wholesale by SetConditional. A reader on the query path loads the pointer
// and never takes a lock, so the table it is reading can never be mutated
// underneath it -- the shape zones.Resolver uses for its snapshot, for the
// same reason.
type condTable struct {
	set    *filter.DomainSet
	routes map[string][]*up
}

type Forwarder struct {
	def      []*up
	cond     atomic.Pointer[condTable]
	strategy string
	timeout  time.Duration
	now      func() time.Time

	// cmu serialises writers of cond against each other. Readers never take
	// it — pick does a bare cond.Load() — so the query path stays lock-free;
	// see SetConditional for why the writers cannot do the same.
	cmu sync.Mutex

	fmu       sync.Mutex
	failCache map[failKey]time.Time

	// closed records that Close has run, so a second call is a real no-op
	// rather than a second walk over every exchanger, and so an owner's
	// lifecycle handling can be asserted on. App.applySettings closes the
	// Forwarder it displaces on every settings write; without something to
	// read here, the only evidence a future call site had not silently
	// dropped one would be a descriptor count. See App's swap and
	// TestApplySettingsClosesTheForwarderItDisplaces.
	closed atomic.Bool
}

// Closed reports whether Close has been called.
func (f *Forwarder) Closed() bool { return f.closed.Load() }

// maxFailCacheEntries bounds failCache growth: once it's reached, New
// insertions trigger a sweep of expired entries before adding.
const maxFailCacheEntries = 4096

func New(cfg Config) (*Forwarder, error) {
	if cfg.Timeout == 0 {
		cfg.Timeout = 2 * time.Second
	}
	if cfg.Strategy == "" {
		cfg.Strategy = "race"
	}
	switch cfg.Strategy {
	case "failover", "fastest", "race":
	default:
		return nil, fmt.Errorf("unknown strategy %q: must be one of failover, fastest, race", cfg.Strategy)
	}
	// Parsed here rather than by the caller so a Forwarder built directly in
	// a test takes exactly the grammar a stored setting does. Config.Upstreams
	// stays []string: a bare "127.0.0.1:5353" is a plain upstream, which is
	// what every existing caller means by it.
	ups, err := parseEntries(cfg.Upstreams)
	if err != nil {
		return nil, err
	}
	f := &Forwarder{strategy: cfg.Strategy, timeout: cfg.Timeout, now: time.Now, failCache: map[failKey]time.Time{}}
	for _, u := range ups {
		f.def = append(f.def, newUp(u, cfg.Timeout))
	}
	// The conditional table is built through the same path a runtime swap
	// takes, so there is one construction path and the existing
	// conditional-routing tests are the proof construction did not change.
	if err := f.SetConditional(cfg.Conditional); err != nil {
		return nil, err
	}
	return f, nil
}

// exchange sends m to u and records the outcome.
//
// The copy is made here because the exchanger is free to mutate what it is
// given — plainExchanger scrambles the question's case, the encrypted ones
// add padding and rewrite the ID — and req.Msg belongs to the caller, who
// may still be racing this exchange against another upstream.
func (f *Forwarder) exchange(ctx context.Context, m *dns.Msg, u *up) (*dns.Msg, error) {
	start := f.now()
	r, err := u.ex.Exchange(ctx, m.Copy())
	ok := err == nil && r != nil
	f.markLatency(u, ok && r.Rcode != dns.RcodeServerFailure, start)
	if !ok {
		return nil, err
	}
	return r, nil
}

func (f *Forwarder) markLatency(u *up, ok bool, start time.Time) {
	now := f.now()
	u.markResult(ok, now.Sub(start), now, f.timeout.Microseconds())
}

func (f *Forwarder) pick(qname string) []*up {
	if t := f.cond.Load(); t != nil {
		if matched, ok := t.set.Match(qname); ok {
			return t.routes[matched]
		}
	}
	return f.def
}

// SetConditional replaces the suffix routing table.
//
// The default upstreams, their health state and the failure cache are all
// untouched: this exists precisely so a zone reload -- which happens on every
// record edit -- does not discard them by rebuilding the whole Forwarder.
//
// Upstreams are reused across swaps by address, so a conditional upstream
// that survives a reload keeps its latency history and its down-marking too.
// Rebuilding them would fix the problem for the defaults and leave it for the
// conditional routes, which are the ones a zone edit is about.
//
// A nil or empty map releases every suffix back to the defaults.
//
// Writers serialise on cmu, held across the whole load-build-store below.
// This is a read-modify-write on live state -- the outgoing table is read to
// adopt its upstreams -- so two overlapping calls would both adopt from the
// same outgoing table and the first to store would have its newly minted
// upstreams discarded by the second, taking their EWMA, failure count and
// backoff window with them. That is a lost update rather than a data race, so
// it is invisible to -race and cannot be left to the caller: a zone reload
// arrives here from api.Server.reloadZones on every zone write with nothing
// on the path serialising it.
//
// Readers are deliberately not part of this. pick loads the atomic pointer
// and takes no lock, so a query never waits on a swap; the lock is between
// writers only, and writers are as frequent as zone edits.
func (f *Forwarder) SetConditional(routes map[string][]string) error {
	f.cmu.Lock()
	defer f.cmu.Unlock()

	// Upstreams the outgoing table had and the new one does not are orphans:
	// nothing will route to them again, and each may be holding pooled TLS
	// connections. Reuse-by-address above means the ones that survive keep
	// their pool along with their EWMA and backoff, which is the point —
	// a zone edit must not cost every upstream its connections.
	//
	// **This closes without draining, and that is only safe because every
	// conditional upstream is plaintext.** A reader that loaded the previous
	// table can still be exchanging on an orphan when this runs; readers take
	// no lock, by design (see the note above). Today every *up built below is
	// constructed SchemePlain, and plainExchanger.Close is a no-op, so there
	// is nothing to take away mid-query. If per-zone encrypted forwarding
	// ever lands (deferred, spec §12) that stops being true and this needs
	// revisiting — App.applySettings' swap has the same shape and gets away
	// with it for a different reason (only idle pooled connections are
	// touched), which does not transfer here, because closing an orphan can
	// race a query that is still choosing a connection.
	closeOrphans := func(old *condTable, kept map[*up]bool) {
		if old == nil {
			return
		}
		for _, ups := range old.routes {
			for _, u := range ups {
				if !kept[u] {
					_ = u.ex.Close()
				}
			}
		}
	}

	if len(routes) == 0 {
		closeOrphans(f.cond.Load(), nil)
		f.cond.Store(nil)
		return nil
	}

	// Every *up this table will use, keyed by address. Seeded from the
	// outgoing table so a swap adopts rather than rebuilds, and added to as
	// new ones are minted so one address named under two suffixes shares a
	// single *up -- and with it a single failure count and backoff window.
	//
	// old is captured once, here, rather than re-loaded below: re-loading
	// after f.cond.Store(t) would return the table this call just installed.
	byAddr := map[string]*up{}
	old := f.cond.Load()
	if old != nil {
		for _, ups := range old.routes {
			for _, u := range ups {
				byAddr[u.addr] = u
			}
		}
	}

	t := &condTable{
		// One shared DomainSet across all suffixes so Match's
		// most-specific-wins gives deterministic longest-suffix routing,
		// instead of iterating per-suffix sets in randomized map order.
		set:    filter.NewDomainSet(),
		routes: make(map[string][]*up, len(routes)),
	}
	kept := map[*up]bool{}
	for suffix, addrs := range routes {
		if len(addrs) == 0 {
			// A suffix with no upstreams still claims the name: pick returns
			// an empty slice, every attempt fails, and the handler answers
			// SERVFAIL rather than falling through to the defaults.
			t.set.Add(suffix)
			t.routes[strings.ToLower(strings.TrimSuffix(suffix, "."))] = nil
			continue
		}
		t.set.Add(suffix)
		var ups []*up
		for _, a := range addrs {
			u, ok := byAddr[a]
			if !ok {
				// Zone forwarding (forwarder and stub zones) stays plaintext:
				// a is a bare address string, never a scheme URL.
				u = newUp(Upstream{Scheme: SchemePlain, Addr: a, Canonical: a}, f.timeout)
				byAddr[a] = u
			}
			ups = append(ups, u)
			kept[u] = true
		}
		t.routes[strings.ToLower(strings.TrimSuffix(suffix, "."))] = ups
	}
	closeOrphans(old, kept)
	f.cond.Store(t)
	return nil
}

// Close releases every upstream's transport. A Forwarder is not usable
// afterwards.
//
// This exists because App.applySettings replaces the Forwarder on every
// settings write and drops the old one. That was free while every transport
// was a dns.Client holding nothing; with pooled TLS connections it is a
// file-descriptor leak per save.
func (f *Forwarder) Close() error {
	f.cmu.Lock()
	defer f.cmu.Unlock()
	if f.closed.Swap(true) {
		return nil // already closed; every exchanger's Close is a no-op by now
	}
	seen := map[*up]bool{}
	var first error
	closeAll := func(ups []*up) {
		for _, u := range ups {
			if seen[u] {
				continue // one *up can be named under several suffixes
			}
			seen[u] = true
			if err := u.ex.Close(); err != nil && first == nil {
				first = err
			}
		}
	}
	closeAll(f.def)
	if t := f.cond.Load(); t != nil {
		for _, ups := range t.routes {
			closeAll(ups)
		}
	}
	return first
}

func (f *Forwarder) condTableLoad() *condTable { return f.cond.Load() }

func (f *Forwarder) Handler() dnssrv.Handler {
	return dnssrv.HandlerFunc(func(ctx context.Context, req *dnssrv.Request) (*dnssrv.Response, error) {
		k := failKey{req.QName(), req.QType()}
		f.fmu.Lock()
		until, failed := f.failCache[k]
		if failed {
			if f.now().Before(until) {
				f.fmu.Unlock()
				return dnssrv.Servfail(req), nil // RFC 9520
			}
			delete(f.failCache, k) // expired: prune on read
		}
		f.fmu.Unlock()
		candidates := f.pick(req.QName())
		var healthy []*up
		for _, u := range candidates {
			if u.healthy(f.now()) {
				healthy = append(healthy, u)
			}
		}
		if len(healthy) == 0 {
			healthy = candidates // all down: try anyway rather than refusing
		}
		var r *dns.Msg
		var lastErr error
		var winner *up
		switch f.strategy {
		case "race":
			r, winner, lastErr = f.race(ctx, req.Msg, healthy)
		default: // failover, fastest
			ordered := healthy
			if f.strategy == "fastest" {
				ordered = append([]*up{}, healthy...)
				sort.Slice(ordered, func(i, j int) bool {
					return ordered[i].ewmaMicro.Load() < ordered[j].ewmaMicro.Load()
				})
			}
			for _, u := range ordered {
				if r, lastErr = f.exchange(ctx, req.Msg, u); lastErr == nil && r.Rcode != dns.RcodeServerFailure {
					winner = u
					break
				}
				r = nil
			}
		}
		if r == nil {
			f.fmu.Lock()
			if len(f.failCache) >= maxFailCacheEntries {
				now := f.now()
				for kk, until := range f.failCache {
					if !now.Before(until) {
						delete(f.failCache, kk)
					}
				}
			}
			f.failCache[k] = f.now().Add(30 * time.Second)
			f.fmu.Unlock()
			if lastErr == nil {
				lastErr = errors.New("all upstreams failed")
			}
			return nil, lastErr
		}
		return &dnssrv.Response{Msg: r, Decision: dnssrv.DecisionForwarded, Upstream: winner.addr}, nil
	})
}

// race queries all ups concurrently and returns the first success. The
// cancel below is best-effort: it stops queued/not-yet-dialed attempts and
// unblocks anything selecting on ctx.Done, but miekg/dns's ExchangeContext
// only reads ctx at the start (for the dial) and does not select on
// ctx.Done() while blocked in a read — so losing goroutines that are
// already waiting on a response run to completion, bounded by the
// client's Timeout, not by cancellation. At our scale (a handful of
// upstreams) that's an acceptable resource cost, not a correctness issue:
// callers only ever see the winning result.
func (f *Forwarder) race(ctx context.Context, m *dns.Msg, ups []*up) (*dns.Msg, *up, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	type result struct {
		r   *dns.Msg
		u   *up
		err error
	}
	ch := make(chan result, len(ups))
	for _, u := range ups {
		go func(u *up) {
			r, err := f.exchange(ctx, m, u)
			ch <- result{r, u, err}
		}(u)
	}
	var lastErr error
	for range ups {
		res := <-ch
		if res.err == nil && res.r.Rcode != dns.RcodeServerFailure {
			return res.r, res.u, nil
		}
		lastErr = res.err
	}
	return nil, nil, lastErr
}
