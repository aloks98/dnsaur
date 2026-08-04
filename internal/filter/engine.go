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

	mu     sync.Mutex
	paused map[int64]time.Time // groupID -> until; 0 = global
	now    func() time.Time
}

func NewEngine() *Engine {
	e := &Engine{paused: map[int64]time.Time{}, now: time.Now}
	empty := map[int64]*Ruleset{}
	e.groups.Store(&empty)
	e.blocking.Store(&blockingCfg{mode: "null-ip", ttl: 30})
	return e
}

func (e *Engine) SetGroups(g map[int64]*Ruleset)      { e.groups.Store(&g) }
func (e *Engine) SetBlocking(mode string, ttl uint32) { e.blocking.Store(&blockingCfg{mode, ttl}) }

func (e *Engine) Pause(groupID int64, d time.Duration) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.paused[groupID] = e.now().Add(d)
}

func (e *Engine) isPaused(groupID int64) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()
	return e.paused[0].After(now) || e.paused[groupID].After(now)
}

// PausedUntil reports when blocking resumes for the group (zero time when
// not paused). The global pause (id 0) and the group's own pause are both
// considered; the later wins.
func (e *Engine) PausedUntil(groupID int64) time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	t := e.paused[0]
	if g := e.paused[groupID]; g.After(t) {
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
			return &dnssrv.Response{Msg: m, Decision: dnssrv.DecisionBlocked, RuleID: v.RuleID, ListID: v.ListID}, nil
		})
	}
}
