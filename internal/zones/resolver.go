package zones

import (
	"context"
	"sync/atomic"

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
}

// NewResolver returns a Resolver with an empty snapshot; call Reload before
// serving traffic.
func NewResolver(zs store.ZoneStore) *Resolver {
	r := &Resolver{zs: zs}
	r.snap.Store(NewIndex(nil))
	return r
}

// Reload rebuilds the snapshot from zs: every zone and, in one query, every
// zone's records, keyed by zone ID so each zone's rows can be matched back
// to it. Each zone is built through NewZone, so the disabled-record rule it
// enforces is applied here exactly as it is everywhere else that serves a
// zone.
func (r *Resolver) Reload(ctx context.Context) error {
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
			if !z.Answer(m, req.QName(), req.QType()) {
				// forwarder/stub zone types name somewhere else to ask rather
				// than holding data (Milestone D); until that lands, treat
				// them the same as not being covered at all.
				return next.ServeDNS(ctx, req)
			}
			return &dnssrv.Response{Msg: m, Decision: dnssrv.DecisionAuthoritative}, nil
		})
	}
}
