package upstream

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
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
	addr      string
	udp, tcp  *dns.Client
	ewmaMicro atomic.Int64
	fails     atomic.Int32
	downUntil atomic.Int64 // unix nano
}

func newUp(addr string, timeout time.Duration) *up {
	return &up{
		addr: addr,
		udp:  &dns.Client{Net: "udp", Timeout: timeout},
		tcp:  &dns.Client{Net: "tcp", Timeout: timeout},
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

type Forwarder struct {
	def        []*up
	condSet    *filter.DomainSet
	condRoutes map[string][]*up
	strategy   string
	timeout    time.Duration
	now        func() time.Time

	fmu       sync.Mutex
	failCache map[failKey]time.Time
}

// maxFailCacheEntries bounds failCache growth: once it's reached, New
// insertions trigger a sweep of expired entries before adding.
const maxFailCacheEntries = 4096

func New(cfg Config) (*Forwarder, error) {
	if len(cfg.Upstreams) == 0 {
		return nil, errors.New("no upstreams configured")
	}
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
	f := &Forwarder{strategy: cfg.Strategy, timeout: cfg.Timeout, now: time.Now, failCache: map[failKey]time.Time{}}
	for _, a := range cfg.Upstreams {
		f.def = append(f.def, newUp(a, cfg.Timeout))
	}
	if len(cfg.Conditional) > 0 {
		// One shared DomainSet across all conditional suffixes so Match's
		// most-specific-wins semantics give deterministic longest-suffix
		// routing, instead of iterating per-suffix sets in (randomized)
		// map order.
		f.condSet = filter.NewDomainSet()
		f.condRoutes = make(map[string][]*up, len(cfg.Conditional))
		for suffix, addrs := range cfg.Conditional {
			f.condSet.Add(suffix)
			var ups []*up
			for _, a := range addrs {
				ups = append(ups, newUp(a, cfg.Timeout))
			}
			f.condRoutes[strings.ToLower(strings.TrimSuffix(suffix, "."))] = ups
		}
	}
	return f, nil
}

func scramble(name string, rnd *rand.Rand) string {
	b := []byte(name)
	for i, c := range b {
		if c >= 'a' && c <= 'z' && rnd.IntN(2) == 1 {
			b[i] = c - 32
		}
	}
	return string(b)
}

// exchange sends m to u with 0x20 case randomization and TCP fallback.
func (f *Forwarder) exchange(ctx context.Context, m *dns.Msg, u *up) (*dns.Msg, error) {
	orig := m.Question[0].Name
	sent := m.Copy()
	rnd := rand.New(rand.NewPCG(rand.Uint64(), rand.Uint64()))
	sent.Question[0].Name = scramble(strings.ToLower(orig), rnd)
	start := f.now()
	r, _, err := u.udp.ExchangeContext(ctx, sent, u.addr)
	if err == nil && r.Truncated {
		r, _, err = u.tcp.ExchangeContext(ctx, sent, u.addr)
	}
	ok := err == nil && r != nil
	if ok && (len(r.Question) != 1 || r.Question[0].Name != sent.Question[0].Name) {
		ok = false
		err = fmt.Errorf("upstream %s: 0x20 case check failed", u.addr)
	}
	u.markResult(ok && r.Rcode != dns.RcodeServerFailure, f.now().Sub(start), f.now(), f.timeout.Microseconds())
	if !ok {
		return nil, err
	}
	// restore original case everywhere it echoes
	r.Question[0].Name = orig
	for _, sec := range [][]dns.RR{r.Answer, r.Ns, r.Extra} {
		for _, rr := range sec {
			if strings.EqualFold(rr.Header().Name, orig) {
				rr.Header().Name = orig
			}
		}
	}
	return r, nil
}

func (f *Forwarder) pick(qname string) []*up {
	if f.condSet != nil {
		if matched, ok := f.condSet.Match(qname); ok {
			return f.condRoutes[matched]
		}
	}
	return f.def
}

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
