package filter

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aloks98/dnsaur/internal/dnssrv"
	"github.com/miekg/dns"
)

type blockingCfg struct {
	mode string
	ttl  uint32
}

type Engine struct {
	groups   atomic.Pointer[map[int64]*Ruleset]
	blocking atomic.Pointer[blockingCfg]

	// paused maps groupID -> until (0 = global). Immutable once stored and
	// replaced wholesale, like groups: every query reads it, and it changes
	// a few times a day, so readers must not queue behind a mutex.
	paused atomic.Pointer[map[int64]time.Time]

	// mu serialises Pause's read-copy-write of that map. Readers take
	// nothing.
	mu  sync.Mutex
	now func() time.Time
}

func NewEngine() *Engine {
	e := &Engine{now: time.Now}
	empty := map[int64]*Ruleset{}
	e.groups.Store(&empty)
	noPauses := map[int64]time.Time{}
	e.paused.Store(&noPauses)
	e.blocking.Store(&blockingCfg{mode: "null-ip", ttl: 30})
	return e
}

func (e *Engine) SetGroups(g map[int64]*Ruleset)      { e.groups.Store(&g) }
func (e *Engine) SetBlocking(mode string, ttl uint32) { e.blocking.Store(&blockingCfg{mode, ttl}) }

func (e *Engine) Pause(groupID int64, d time.Duration) {
	e.mu.Lock()
	defer e.mu.Unlock()
	next := make(map[int64]time.Time, len(*e.paused.Load())+1)
	for k, v := range *e.paused.Load() {
		next[k] = v
	}
	next[groupID] = e.now().Add(d)
	e.paused.Store(&next)
}

func (e *Engine) isPaused(groupID int64) bool {
	p := *e.paused.Load()
	now := e.now()
	return p[0].After(now) || p[groupID].After(now)
}

// PausedUntil reports when blocking resumes for the group (zero time when
// not paused). The global pause (id 0) and the group's own pause are both
// considered; the later wins.
func (e *Engine) PausedUntil(groupID int64) time.Time {
	p := *e.paused.Load()
	t := p[0]
	if g := p[groupID]; g.After(t) {
		t = g
	}
	if !t.After(e.now()) {
		return time.Time{}
	}
	return t
}

func (e *Engine) Middleware() dnssrv.Middleware {
	return func(next dnssrv.Handler) dnssrv.Handler {
		return dnssrv.HandlerFunc(func(ctx context.Context, req *dnssrv.Request) (*dnssrv.Response, error) {
			rs, ok := (*e.groups.Load())[req.Client.GroupID]
			if !ok || e.isPaused(req.Client.GroupID) {
				return next.ServeDNS(ctx, req)
			}
			v := rs.Evaluate(req.QName())
			if v.Action != "block" {
				return next.ServeDNS(ctx, req)
			}
			cfg := e.blocking.Load()
			m := new(dns.Msg)
			if cfg.mode == "nxdomain" {
				m.SetRcode(req.Msg, dns.RcodeNameError)
			} else {
				m.SetReply(req.Msg)
				hdr := dns.RR_Header{Name: req.Msg.Question[0].Name, Class: dns.ClassINET, Ttl: cfg.ttl}
				switch req.QType() {
				case dns.TypeA:
					hdr.Rrtype = dns.TypeA
					m.Answer = []dns.RR{&dns.A{Hdr: hdr, A: net.IPv4zero}}
				case dns.TypeAAAA:
					hdr.Rrtype = dns.TypeAAAA
					m.Answer = []dns.RR{&dns.AAAA{Hdr: hdr, AAAA: net.IPv6zero}}
				}
			}
			return &dnssrv.Response{Msg: m, Decision: dnssrv.DecisionBlocked,
				RuleID: v.RuleID, ListID: v.ListID, Matched: v.Matched}, nil
		})
	}
}
