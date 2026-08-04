package clients

import (
	"context"
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
		if ip, err := netip.ParseAddr(c.Matcher); err == nil {
			s.exact[ip.Unmap()] = info
			continue
		}
		if p, err := netip.ParsePrefix(c.Matcher); err == nil {
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
