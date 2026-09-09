package qlog

import (
	"context"
	"log/slog"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aloks98/dnsaur/internal/dnssrv"
	"github.com/aloks98/dnsaur/internal/store"
	"github.com/miekg/dns"
)

type Options struct {
	BatchSize  int
	FlushEvery time.Duration
	Buffer     int
	Privacy    string
	InstanceID string
	Now        func() time.Time
}

type Logger struct {
	qs      store.QueryLogStore
	o       Options
	ch      chan store.QueryLogEntry
	dropped atomic.Int64
	privacy atomic.Value // string
	hub     subHub
	// reported and lastReport are the drop warning's rate limit. Plain
	// fields, no atomics: only Run's goroutine touches them.
	reported   int64
	lastReport time.Time
}

func New(qs store.QueryLogStore, o Options) *Logger {
	if o.BatchSize == 0 {
		o.BatchSize = 1000
	}
	if o.FlushEvery == 0 {
		o.FlushEvery = time.Second
	}
	if o.Buffer == 0 {
		o.Buffer = 10000
	}
	if o.Privacy == "" {
		o.Privacy = "full"
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	l := &Logger{qs: qs, o: o, ch: make(chan store.QueryLogEntry, o.Buffer)}
	l.privacy.Store(o.Privacy)
	return l
}

// Dropped is how many entries the buffer has discarded since start. It is
// also reported on GET /api/v1/stats/overview, because a query log with
// holes in it that looks complete is worse than one that says so.
func (l *Logger) Dropped() int64 { return l.dropped.Load() }

// dropWarnEvery bounds how often the buffer's losses are logged. The
// condition that causes them — a burst arriving while the database is busy
// — lasts for as long as it lasts, and a warning per flush would bury the
// storage error that is usually the actual cause.
const dropWarnEvery = time.Minute

// reportDropped warns when entries have been discarded since the last
// warning, at most once per dropWarnEvery. Called from the flush loop, so
// the count is reported by the same goroutine that is failing to keep up
// rather than from the query path.
func (l *Logger) reportDropped(now time.Time) {
	n := l.dropped.Load()
	if n == l.reported {
		return
	}
	if !l.lastReport.IsZero() && now.Sub(l.lastReport) < dropWarnEvery {
		return
	}
	slog.Warn("query log entries dropped, buffer full", "dropped", n-l.reported, "total", n)
	l.reported, l.lastReport = n, now
}

// SetPrivacy updates the privacy mode read by Middleware on every query,
// letting qlog.privacy be changed live (e.g. via settings hot-reload)
// without recreating the Logger.
func (l *Logger) SetPrivacy(p string) {
	if p == "" {
		p = "full"
	}
	l.privacy.Store(p)
}

func (l *Logger) getPrivacy() string {
	if v, ok := l.privacy.Load().(string); ok {
		return v
	}
	return "full"
}

func anonymize(ip string) string {
	a, err := netip.ParseAddr(ip)
	if err != nil {
		return ip
	}
	if a.Is4() {
		b := a.As4()
		b[3] = 0
		return netip.AddrFrom4(b).String()
	}
	b := a.As16()
	for i := 6; i < 16; i++ {
		b[i] = 0
	}
	return netip.AddrFrom16(b).String()
}

// subscribers holds live-tail channels; publish is non-blocking (slow SSE
// clients miss entries rather than backpressuring the DNS path).
type subHub struct {
	mu   sync.Mutex
	subs map[int]chan store.QueryLogEntry
	next int
}

func (l *Logger) Subscribe() (<-chan store.QueryLogEntry, func()) {
	l.hub.mu.Lock()
	defer l.hub.mu.Unlock()
	if l.hub.subs == nil {
		l.hub.subs = map[int]chan store.QueryLogEntry{}
	}
	id := l.hub.next
	l.hub.next++
	ch := make(chan store.QueryLogEntry, 64)
	l.hub.subs[id] = ch
	return ch, func() {
		l.hub.mu.Lock()
		defer l.hub.mu.Unlock()
		if c, ok := l.hub.subs[id]; ok {
			delete(l.hub.subs, id)
			close(c)
		}
	}
}

func (l *Logger) Publish(e store.QueryLogEntry) {
	l.hub.mu.Lock()
	defer l.hub.mu.Unlock()
	for _, ch := range l.hub.subs {
		select {
		case ch <- e:
		default:
		}
	}
}

func (l *Logger) emit(e store.QueryLogEntry) {
	for {
		select {
		case l.ch <- e:
			return
		default: // full: drop oldest, keep newest
			select {
			case <-l.ch:
				l.dropped.Add(1)
			default:
			}
		}
	}
}

func (l *Logger) Middleware() dnssrv.Middleware {
	return func(next dnssrv.Handler) dnssrv.Handler {
		return dnssrv.HandlerFunc(func(ctx context.Context, req *dnssrv.Request) (*dnssrv.Response, error) {
			start := l.o.Now()
			resp, err := next.ServeDNS(ctx, req)
			privacy := l.getPrivacy()
			if privacy == "none" {
				return resp, err
			}
			e := store.QueryLogEntry{
				At: start.UnixMilli(), InstanceID: l.o.InstanceID,
				ClientIP: req.ClientIP.String(), ClientID: req.Client.ID,
				QName: req.QName(), QType: dns.TypeToString[req.QType()],
				DurationMs: l.o.Now().Sub(start).Milliseconds(),
			}
			if privacy == "anon" {
				e.ClientIP = anonymize(e.ClientIP)
			}
			if err != nil || resp == nil {
				e.Decision, e.RCode = string(dnssrv.DecisionError), "SERVFAIL"
			} else {
				e.Decision = string(resp.Decision)
				e.RuleID, e.ListID, e.Upstream = resp.RuleID, resp.ListID, resp.Upstream
				if resp.Msg != nil {
					e.RCode = dns.RcodeToString[resp.Msg.Rcode]
				}
			}
			l.emit(e)
			l.Publish(e)
			return resp, err
		})
	}
}

// Run starts the flush loop. It accumulates log entries until BatchSize or FlushEvery
// triggers a database write. On context cancel, it performs a best-effort drain:
// buffered entries are flushed, but entries emitted after the drain observes an empty
// channel are dropped (logging is lossy-by-design, never blocking).
func (l *Logger) Run(ctx context.Context) {
	t := time.NewTicker(l.o.FlushEvery)
	defer t.Stop()
	batch := make([]store.QueryLogEntry, 0, l.o.BatchSize)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		fctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := l.qs.InsertBatch(fctx, batch); err != nil {
			slog.Warn("query log flush failed", "n", len(batch), "err", err)
			// Batches are discarded on flush failure (no retry) by design.
		}
		cancel()
		batch = batch[:0]
	}
	for {
		select {
		case <-ctx.Done():
			for { // drain what's buffered, then final flush
				select {
				case e := <-l.ch:
					batch = append(batch, e)
					if len(batch) >= l.o.BatchSize {
						flush()
					}
				default:
					flush()
					return
				}
			}
		case e := <-l.ch:
			batch = append(batch, e)
			if len(batch) >= l.o.BatchSize {
				flush()
			}
		case <-t.C:
			flush()
			l.reportDropped(l.o.Now())
		}
	}
}

// Pruner enforces retention on the two tables that grow with traffic: the
// query log itself and the hourly counters derived from it. They keep
// separate retentions — the log answers "what happened at 14:02 yesterday"
// and is the bulky one, the rollups answer "how did last March compare" and
// are small enough to keep far longer — but one daily pass covers both.
type Pruner struct {
	qs                 store.QueryLogStore
	ss                 store.StatsStore
	retentionDays      func() int64
	statsRetentionDays func() int64
}

func NewPruner(qs store.QueryLogStore, ss store.StatsStore, retentionDays, statsRetentionDays func() int64) *Pruner {
	return &Pruner{qs: qs, ss: ss, retentionDays: retentionDays, statsRetentionDays: statsRetentionDays}
}

func (p *Pruner) pruneOnce(ctx context.Context, now time.Time) {
	cutoff := now.UnixMilli() - p.retentionDays()*24*3600*1000
	if n, err := p.qs.DeleteBefore(ctx, cutoff); err != nil {
		slog.Warn("query log prune failed", "err", err)
	} else if n > 0 {
		slog.Info("query log pruned", "rows", n)
	}
	// stats_hourly.bucket is a unix second, so this cutoff is in seconds.
	statsCutoff := now.Unix() - p.statsRetentionDays()*24*3600
	if n, err := p.ss.PruneBefore(ctx, statsCutoff); err != nil {
		slog.Warn("stats prune failed", "err", err)
	} else if n > 0 {
		slog.Info("stats pruned", "rows", n)
	}
}

func (p *Pruner) Run(ctx context.Context) {
	t := time.NewTicker(24 * time.Hour)
	defer t.Stop()
	p.pruneOnce(ctx, time.Now())
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.pruneOnce(ctx, time.Now())
		}
	}
}
