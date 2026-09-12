package filter

import (
	"context"
	"maps"
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

// PausesKey names the settings row the pause state is stored under. It is
// bookkeeping rather than configuration — nothing edits it by hand, and
// GET /settings strips it, the same treatment stats.watermark gets.
const PausesKey = "blocking.pauses"

// PauseKind says what one pause covers.
type PauseKind string

const (
	PauseGlobal PauseKind = "global"
	PauseGroup  PauseKind = "group"
	PauseClient PauseKind = "client"
)

// Pauses is the pause state, in the shape it is persisted in: unix
// milliseconds per scope, so a restart can put back what is still in force.
// Groups and clients are numbered separately, so they are two maps rather
// than one keyed by id.
type Pauses struct {
	Global  int64           `json:"global,omitempty"`
	Groups  map[int64]int64 `json:"groups,omitempty"`
	Clients map[int64]int64 `json:"clients,omitempty"`
}

// clone copies the state, with both maps allocated, so the one currently
// stored is never written through.
func (p *Pauses) clone() Pauses {
	n := Pauses{Global: p.Global, Groups: maps.Clone(p.Groups), Clients: maps.Clone(p.Clients)}
	if n.Groups == nil {
		n.Groups = map[int64]int64{}
	}
	if n.Clients == nil {
		n.Clients = map[int64]int64{}
	}
	return n
}

// prune drops what has already run out. An expired entry and a missing one
// mean the same thing to every reader, so keeping it only grows the row that
// gets persisted — and would make a restart install pauses that are over.
func (p *Pauses) prune(now time.Time) {
	ms := now.UnixMilli()
	if p.Global <= ms {
		p.Global = 0
	}
	expired := func(_, until int64) bool { return until <= ms }
	maps.DeleteFunc(p.Groups, expired)
	maps.DeleteFunc(p.Clients, expired)
}

type Engine struct {
	groups   atomic.Pointer[map[int64]*Ruleset]
	blocking atomic.Pointer[blockingCfg]

	// paused is the pause state for all three scopes. Immutable once stored
	// and replaced wholesale, like groups: every query reads it, and it
	// changes a few times a day, so readers must not queue behind a mutex.
	paused atomic.Pointer[Pauses]

	// mu serialises Pause's read-copy-write of that state and orders the
	// persist calls with it. Readers take nothing.
	mu      sync.Mutex
	persist func(Pauses)
	now     func() time.Time
}

func NewEngine() *Engine {
	e := &Engine{now: time.Now}
	empty := map[int64]*Ruleset{}
	e.groups.Store(&empty)
	e.paused.Store(&Pauses{})
	e.blocking.Store(&blockingCfg{mode: "null-ip", ttl: 30})
	return e
}

func (e *Engine) SetGroups(g map[int64]*Ruleset)      { e.groups.Store(&g) }
func (e *Engine) SetBlocking(mode string, ttl uint32) { e.blocking.Store(&blockingCfg{mode, ttl}) }

// OnPauseChange registers a sink for the pause state, called with the new
// state after every change. It is how a pause outlives the process: the app
// writes the state to a settings row and hands it back to Restore at the
// next start.
func (e *Engine) OnPauseChange(f func(Pauses)) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.persist = f
}

// Restore installs a persisted pause state, dropping anything that ran out
// since it was written. It replaces whatever is there, so the caller must be
// one that holds the whole state: the startup read of the stored row, and
// the settings reload that follows a config-sync pull — a pause is synced
// configuration, and on a replica this is the only thing that sets one.
//
// A state that matches what is already in force is left alone rather than
// stored again, so a reload that changed nothing leaves the pointer every
// query reads exactly where it was. It never calls the OnPauseChange sink:
// installing what was read is not a change to write back.
func (e *Engine) Restore(p Pauses) {
	e.mu.Lock()
	defer e.mu.Unlock()
	next := p.clone()
	next.prune(e.now())
	cur := e.paused.Load()
	if cur.Global == next.Global && maps.Equal(cur.Groups, next.Groups) && maps.Equal(cur.Clients, next.Clients) {
		return
	}
	e.paused.Store(&next)
}

// Pause holds blocking for d in one scope: one group, one client, or —
// for PauseGlobal, and for any other kind — the whole server. A d of 0
// resumes: the entry is dropped, along with every other that has already
// run out.
func (e *Engine) Pause(kind PauseKind, id int64, d time.Duration) {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()
	next := e.paused.Load().clone()
	until := now.Add(d).UnixMilli()
	switch kind {
	case PauseGroup:
		next.Groups[id] = until
	case PauseClient:
		next.Clients[id] = until
	default:
		next.Global = until
	}
	next.prune(now)
	e.paused.Store(&next)
	// Under the lock, so the stored row can never end up describing an
	// older state than the one in memory. A pause is a handful of writes a
	// day, so serialising them behind it costs nothing measurable.
	if e.persist != nil {
		e.persist(next)
	}
}

func (e *Engine) isPaused(groupID, clientID int64) bool {
	until, _ := e.PausedUntil(groupID, clientID)
	return !until.IsZero()
}

// PausedUntil reports when blocking resumes for a client in a group, and
// which scope decided it (zero time and "" when nothing is paused). The
// global pause, the group's own and the client's own are all considered and
// the later wins, so no scope can cut another short: a client pause extends
// its group's, and a group pause extends its clients'.
//
// Groups and clients are numbered from 1, so 0 matches nothing — pass it for
// a query whose client is not known, or when asking about a group alone.
func (e *Engine) PausedUntil(groupID, clientID int64) (time.Time, PauseKind) {
	p := e.paused.Load()
	until, kind := p.Global, PauseGlobal
	if v := p.Groups[groupID]; v > until {
		until, kind = v, PauseGroup
	}
	if v := p.Clients[clientID]; v > until {
		until, kind = v, PauseClient
	}
	if until <= e.now().UnixMilli() {
		return time.Time{}, ""
	}
	return time.UnixMilli(until), kind
}

func (e *Engine) Middleware() dnssrv.Middleware {
	return func(next dnssrv.Handler) dnssrv.Handler {
		return dnssrv.HandlerFunc(func(ctx context.Context, req *dnssrv.Request) (*dnssrv.Response, error) {
			rs, ok := (*e.groups.Load())[req.Client.GroupID]
			if !ok || e.isPaused(req.Client.GroupID, req.Client.ID) {
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
