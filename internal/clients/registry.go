package clients

import (
	"context"
	"log/slog"
	"net/netip"
	"sort"
	"sync/atomic"

	"github.com/aloks98/dnsaur/internal/dnssrv"
	"github.com/aloks98/dnsaur/internal/store"
)

type cidrEntry struct {
	prefix netip.Prefix
	info   dnssrv.ClientInfo
}

type snapshot struct {
	exact map[netip.Addr]dnssrv.ClientInfo
	cidrs []cidrEntry // sorted by prefix length, longest first
}

type Registry struct {
	cs   store.ClientStore
	snap atomic.Pointer[snapshot]
}

// NormalizeMatcher validates a client matcher and returns it in the one
// spelling Lookup can match, or reports that it is neither an IP nor a CIDR.
//
// Three shapes used to be accepted and then never match anything, silently:
//
//   - an unmasked prefix (`10.0.0.1/24`), which Contains still answers for,
//     but which sorts and reads as something it isn't — it is stored masked;
//   - a v4-mapped address or prefix (`::ffff:10.0.0.0/120`), where Lookup
//     unmaps the request address and Contains then mismatches on family —
//     both sides are unmapped here;
//   - a zoned address (`fe80::1%eth0`), where the request side is built with
//     netip.AddrFromSlice and never carries a zone, so the two can never be
//     equal. There is nothing to canonicalise it to, so it is refused.
func NormalizeMatcher(m string) (string, bool) {
	if ip, err := netip.ParseAddr(m); err == nil {
		if ip.Zone() != "" {
			return "", false
		}
		return ip.Unmap().String(), true
	}
	p, err := netip.ParsePrefix(m)
	if err != nil {
		return "", false
	}
	addr, bits := p.Addr(), p.Bits()
	if addr.Is4In6() {
		// ::ffff:10.0.0.0/120 is 10.0.0.0/24; anything shorter than the
		// 96-bit mapping prefix is not a v4 range at all.
		if bits < 96 {
			return "", false
		}
		addr, bits = addr.Unmap(), bits-96
	}
	return netip.PrefixFrom(addr, bits).Masked().String(), true
}

func NewRegistry(cs store.ClientStore) *Registry {
	r := &Registry{cs: cs}
	r.snap.Store(&snapshot{exact: map[netip.Addr]dnssrv.ClientInfo{}})
	return r
}

func (r *Registry) Reload(ctx context.Context) error {
	groups, err := r.cs.Groups(ctx)
	if err != nil {
		return err
	}
	gname := map[int64]string{}
	for _, g := range groups {
		gname[g.ID] = g.Name
	}
	cls, err := r.cs.Clients(ctx)
	if err != nil {
		return err
	}
	s := &snapshot{exact: map[netip.Addr]dnssrv.ClientInfo{}}
	for _, c := range cls {
		info := dnssrv.ClientInfo{ID: c.ID, Name: c.Name, GroupID: c.GroupID, GroupName: gname[c.GroupID]}
		// Canonicalised here as well as at the API, because rows written
		// before that validation existed are still in the table and a
		// matcher that can never match is indistinguishable, from the
		// dashboard, from one that simply hasn't seen its device yet.
		m, ok := NormalizeMatcher(c.Matcher)
		if !ok {
			slog.Warn("client matcher is not an IP or CIDR, skipping it", "client", c.ID, "matcher", c.Matcher)
			continue
		}
		if ip, err := netip.ParseAddr(m); err == nil {
			s.exact[ip] = info
			continue
		}
		if p, err := netip.ParsePrefix(m); err == nil {
			s.cidrs = append(s.cidrs, cidrEntry{prefix: p, info: info})
		}
	}
	sort.Slice(s.cidrs, func(i, j int) bool { return s.cidrs[i].prefix.Bits() > s.cidrs[j].prefix.Bits() })
	r.snap.Store(s)
	return nil
}

func (r *Registry) Lookup(ip netip.Addr) dnssrv.ClientInfo {
	ip = ip.Unmap()
	s := r.snap.Load()
	if info, ok := s.exact[ip]; ok {
		return info
	}
	for _, e := range s.cidrs {
		if e.prefix.Contains(ip) {
			return e.info
		}
	}
	return dnssrv.ClientInfo{GroupID: 1, GroupName: "default"}
}

func (r *Registry) Middleware() dnssrv.Middleware {
	return func(next dnssrv.Handler) dnssrv.Handler {
		return dnssrv.HandlerFunc(func(ctx context.Context, req *dnssrv.Request) (*dnssrv.Response, error) {
			req.Client = r.Lookup(req.ClientIP)
			return next.ServeDNS(ctx, req)
		})
	}
}
