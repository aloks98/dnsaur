package cache

import (
	"container/list"
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aloks98/dnsaur/internal/dnssrv"
	"github.com/miekg/dns"
	"golang.org/x/sync/singleflight"
)

type Options struct {
	MinTTL        time.Duration
	MaxTTL        time.Duration
	NegTTL        time.Duration
	ServeStaleFor time.Duration
	MaxEntries    int
	Now           func() time.Time
}

type ckey struct {
	name  string
	qtype uint16
}

type entry struct {
	msg      *dns.Msg
	storedAt time.Time
	ttl      time.Duration
	elem     *list.Element
}

type Cache struct {
	mu      sync.Mutex
	entries map[ckey]*entry
	lru     *list.List // front = most recent; values are ckey
	o       Options
	sf      singleflight.Group
	// epoch counts purges. A lookup reads it before going to the upstreams
	// and put refuses an entry whose epoch has moved on since — see Purge.
	// Bumped and compared under mu; the read at the top of Middleware is a
	// bare atomic load, so the query path takes no extra lock for it.
	epoch atomic.Uint64
}

func New(o Options) *Cache {
	if o.MaxTTL == 0 {
		o.MaxTTL = 24 * time.Hour
	}
	if o.NegTTL == 0 {
		o.NegTTL = 30 * time.Second
	}
	if o.ServeStaleFor == 0 {
		o.ServeStaleFor = 24 * time.Hour
	}
	if o.MaxEntries == 0 {
		o.MaxEntries = 10000
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Cache{entries: map[ckey]*entry{}, lru: list.New(), o: o}
}

func (c *Cache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

// Purge drops every entry for a name at or under one of suffixes, and
// reports how many it dropped.
//
// This is how a routing change reaches the cache. An entry is keyed on
// (qname, qtype) and records nothing about which upstream produced it, so an
// answer cached from the default resolvers before a zone claimed that suffix
// keeps being served afterwards — the routing table is below the cache and is
// never consulted on a hit. The mirror case is the same defect pointing the
// other way: releasing a suffix leaves internal answers cached for names that
// should now resolve publicly. Both are closed by dropping the suffix's
// entries when its routing changes, which is what app.installConditional
// calls this for.
//
// A name matches a suffix when it *is* that suffix or sits beneath it on a
// label boundary. Entries are keyed by dnssrv.Request.QName — lower-cased and
// with the root dot trimmed — so suffixes are normalised the same way here
// rather than at every call site.
//
// It also invalidates the lookups already in flight, which is the half a
// sweep of the map alone would miss: a query that reached the upstreams
// *before* the claim existed is still going to come back with a pre-claim
// answer and put it into the cache, and it would land after the sweep had
// already passed. Every in-flight put carries the epoch it started under and
// is dropped if a purge has happened since. The comparison is made under mu,
// which Purge also holds while bumping, so there is no window between the
// check and the store; the cost is that a purge for one suffix also drops
// whatever unrelated lookups were in flight at that instant, which is one
// extra upstream query each and no wrong answers.
func (c *Cache) Purge(suffixes ...string) int {
	norm := make([]string, 0, len(suffixes))
	for _, s := range suffixes {
		if s = strings.ToLower(strings.TrimSuffix(s, ".")); s != "" {
			norm = append(norm, s)
		}
	}
	if len(norm) == 0 {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.epoch.Add(1)
	n := 0
	for k := range c.entries {
		if nameUnder(k.name, norm) {
			c.removeLocked(k)
			n++
		}
	}
	return n
}

// nameUnder reports whether name is one of suffixes or sits beneath one of
// them. The label-boundary check is what keeps notcorp.example out of
// corp.example.
func nameUnder(name string, suffixes []string) bool {
	for _, s := range suffixes {
		if name == s {
			return true
		}
		if len(name) > len(s)+1 && name[len(name)-len(s)-1] == '.' && strings.HasSuffix(name, s) {
			return true
		}
	}
	return false
}

func (c *Cache) get(k ckey) (fresh *entry, stale *entry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[k]
	if !ok {
		return nil, nil
	}
	age := c.o.Now().Sub(e.storedAt)
	c.lru.MoveToFront(e.elem)
	if age < e.ttl {
		return e, nil
	}
	if age < e.ttl+c.o.ServeStaleFor {
		return nil, e
	}
	c.removeLocked(k)
	return nil, nil
}

func (c *Cache) removeLocked(k ckey) {
	if e, ok := c.entries[k]; ok {
		c.lru.Remove(e.elem)
		delete(c.entries, k)
	}
}

// put stores msg under k, unless a Purge has happened since epoch — see
// Purge's last paragraph.
func (c *Cache) put(k ckey, msg *dns.Msg, ttl time.Duration, epoch uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.epoch.Load() != epoch {
		return
	}
	c.removeLocked(k)
	e := &entry{msg: msg.Copy(), storedAt: c.o.Now(), ttl: ttl}
	e.elem = c.lru.PushFront(k)
	c.entries[k] = e
	for len(c.entries) > c.o.MaxEntries {
		back := c.lru.Back()
		c.removeLocked(back.Value.(ckey))
	}
}

func respTTL(o Options, m *dns.Msg) (time.Duration, bool) {
	if m.Rcode != dns.RcodeSuccess && m.Rcode != dns.RcodeNameError {
		return 0, false
	}
	if len(m.Answer) == 0 { // negative: NXDOMAIN or NODATA, RFC 2308
		ttl := o.NegTTL
		for _, rr := range m.Ns {
			if soa, ok := rr.(*dns.SOA); ok {
				ttl = time.Duration(min(soa.Minttl, uint32(soa.Hdr.Ttl))) * time.Second
				break
			}
		}
		return clamp(ttl, o.MinTTL, o.MaxTTL), true
	}
	minTTL := uint32(1<<32 - 1)
	for _, rr := range m.Answer {
		if rr.Header().Ttl < minTTL {
			minTTL = rr.Header().Ttl
		}
	}
	return clamp(time.Duration(minTTL)*time.Second, o.MinTTL, o.MaxTTL), true
}

func clamp(d, lo, hi time.Duration) time.Duration {
	if d < lo {
		return lo
	}
	if d > hi {
		return hi
	}
	return d
}

// rewriteQuestion rewrites m's Question section to req's exact question and
// updates the owner Name of any Answer/Ns/Extra RR that case-insensitively
// matched m's previous question name, so a cache-constructed response
// reflects the current requester's casing rather than whichever client's
// casing happened to populate (or last echo through) the cache entry.
// Mirrors the 0x20 restore loop in internal/upstream/forwarder.go.
func rewriteQuestion(m *dns.Msg, req *dnssrv.Request) {
	if len(req.Msg.Question) == 0 || len(m.Question) == 0 {
		return
	}
	oldName := m.Question[0].Name
	newName := req.Msg.Question[0].Name
	m.Question = append([]dns.Question(nil), req.Msg.Question...)
	for _, sec := range [][]dns.RR{m.Answer, m.Ns, m.Extra} {
		for _, rr := range sec {
			if strings.EqualFold(rr.Header().Name, oldName) {
				rr.Header().Name = newName
			}
		}
	}
}

func withTTLs(m *dns.Msg, set func(cur uint32) uint32) *dns.Msg {
	out := m.Copy()
	for _, sec := range [][]dns.RR{out.Answer, out.Ns, out.Extra} {
		for _, rr := range sec {
			if rr.Header().Rrtype != dns.TypeOPT {
				rr.Header().Ttl = set(rr.Header().Ttl)
			}
		}
	}
	return out
}

func (c *Cache) Middleware() dnssrv.Middleware {
	return func(next dnssrv.Handler) dnssrv.Handler {
		return dnssrv.HandlerFunc(func(ctx context.Context, req *dnssrv.Request) (*dnssrv.Response, error) {
			if len(req.Msg.Question) != 1 || req.Msg.Question[0].Qclass != dns.ClassINET {
				return next.ServeDNS(ctx, req)
			}
			// Read before the lookup, not after: it is the epoch this query's
			// answer will have been produced under.
			epoch := c.epoch.Load()
			k := ckey{name: req.QName(), qtype: req.QType()}
			if fresh, _ := c.get(k); fresh != nil {
				age := uint32(c.o.Now().Sub(fresh.storedAt) / time.Second)
				m := withTTLs(fresh.msg, func(cur uint32) uint32 {
					if cur <= age {
						return 1
					}
					return cur - age
				})
				m.Id = req.Msg.Id
				rewriteQuestion(m, req)
				return &dnssrv.Response{Msg: m, Decision: dnssrv.DecisionCached}, nil
			}
			// **The epoch is part of the key, not decoration.** Without
			// it the group is route-blind: a query arriving entirely
			// *after* a claim is installed and the purge has run misses
			// the now-empty cache and is collapsed onto a leader that was
			// dispatched to the default upstreams before the claim
			// existed — and is handed that leader's public answer for a
			// name a split-horizon zone now claims. Nothing is stored (the
			// put below is refused for the same reason), so it is
			// transient; but transient is not the same as harmless, and
			// the window is one upstream round trip rather than one cache
			// lookup. Keying on the epoch makes a purge end the in-flight
			// group as well as the stored entries: a query that arrives
			// after it cannot join a lookup sent before it.
			v, err, _ := c.sf.Do(fmt.Sprintf("%d|%s|%d", epoch, k.name, k.qtype), func() (any, error) {
				resp, err := next.ServeDNS(ctx, req)
				if err == nil && resp != nil && resp.Msg != nil && resp.Decision == dnssrv.DecisionForwarded {
					if ttl, ok := respTTL(c.o, resp.Msg); ok {
						c.put(k, resp.Msg, ttl, epoch)
					}
				}
				return resp, err
			})
			resp, _ := v.(*dnssrv.Response)
			failed := err != nil || resp == nil || resp.Msg == nil || resp.Msg.Rcode == dns.RcodeServerFailure
			if failed {
				// get() is called again here (rather than reusing the earlier
				// fresh-check result) because the fresh-check above and the
				// singleflight call below it may race with a concurrent
				// put()/eviction. Re-checking under the lock is cheap; the TOCTOU
				// between this get() and any concurrent mutation is intentional and
				// harmless here — worst case we serve a slightly-more-stale entry or
				// fall through to the error, both acceptable on the failure path.
				if _, stale := c.get(k); stale != nil {
					m := withTTLs(stale.msg, func(uint32) uint32 { return 30 })
					m.Id = req.Msg.Id
					rewriteQuestion(m, req)
					return &dnssrv.Response{Msg: m, Decision: dnssrv.DecisionStale}, nil
				}
				// resp is shared across every caller collapsed into this
				// singleflight call; give each caller its own copy with its own
				// request Id so collapsed followers don't receive a mismatched Id
				// (a mismatched Id causes clients to drop the reply and time out).
				if resp != nil && resp.Msg != nil {
					out := *resp
					out.Msg = resp.Msg.Copy()
					out.Msg.Id = req.Msg.Id
					rewriteQuestion(out.Msg, req)
					return &out, err
				}
				return resp, err
			}
			out := *resp
			out.Msg = resp.Msg.Copy()
			out.Msg.Id = req.Msg.Id
			rewriteQuestion(out.Msg, req)
			return &out, nil
		})
	}
}
