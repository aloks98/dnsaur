package clients

import (
	"cmp"
	"context"
	"log/slog"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/aloks98/dnsaur/internal/dnssrv"
	"github.com/aloks98/dnsaur/internal/store"
)

// macPrefix opens the third matcher kind (design §8.2). A MAC is not an
// address, so it is spelled differently rather than guessed at: everything
// after it is one hardware address in CanonicalMAC's spelling.
const macPrefix = "mac:"

type cidrEntry struct {
	prefix netip.Prefix
	info   dnssrv.ClientInfo
}

type snapshot struct {
	exact map[netip.Addr]dnssrv.ClientInfo
	cidrs []cidrEntry // sorted by prefix length, longest first
}

// LeaseLookup is the current address of a MAC, from the DHCP lease table. It
// is a map lookup on a table the DHCP manager swaps in whole, never a query
// of anything: it is called while the snapshot is being rebuilt, and a
// blocking one would stall client resolution for every query behind it.
type LeaseLookup func(mac string) (netip.Addr, bool)

type Registry struct {
	cs   store.ClientStore
	snap atomic.Pointer[snapshot]
	// mu serialises every rebuild of the snapshot and the reads that feed
	// one: two of them racing would each derive a whole snapshot, and the one
	// that read *first* could store *last*, leaving the registry permanently
	// out of date with no reader the wiser. It is held across Reload's store
	// reads for the same reason — nothing on the query path takes it, so a
	// reload never makes a query wait; Lookup loads the atomic pointer.
	mu sync.Mutex
	// rows is the last client list read from the store, kept so a new lease
	// table can re-resolve the mac matchers without going back to the store
	// on the DHCP poll's goroutine.
	rows   []client
	leases LeaseLookup
}

// client is one stored row reduced to what the snapshot is built from.
type client struct {
	matcher string
	info    dnssrv.ClientInfo
}

// NormalizeMatcher validates a client matcher and returns it in the one
// spelling Lookup can match, or reports that it is none of the three kinds:
// an IP, a CIDR, or `mac:<hardware address>` (§8.2). A MAC is canonicalised
// to CanonicalMAC's spelling, which is what the lease table is keyed on, and
// anything net.ParseMAC will not read as a 6-byte address is refused —
// a MAC that can never be looked up is a row that can never match.
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
	if mac, ok := strings.CutPrefix(m, macPrefix); ok {
		canonical, ok := store.CanonicalMAC(mac)
		if !ok {
			return "", false
		}
		return macPrefix + canonical, true
	}
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
	r.mu.Lock()
	defer r.mu.Unlock()
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
	rows := make([]client, 0, len(cls))
	for _, c := range cls {
		rows = append(rows, client{
			matcher: c.Matcher,
			info:    dnssrv.ClientInfo{ID: c.ID, Name: c.Name, GroupID: c.GroupID, GroupName: gname[c.GroupID]},
		})
	}
	r.rows = rows
	r.rebuild()
	return nil
}

// SetLeaseLookup installs the lease table the `mac` matchers resolve
// through and re-resolves them against it now. The DHCP manager calls it
// with every new table, which is what keeps a device in its group across a
// renewal that moved it to another address (§8.2).
func (r *Registry) SetLeaseLookup(fn LeaseLookup) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.leases = fn
	r.rebuild()
}

// rebuild derives the snapshot from the last client list read and the
// current lease table, and swaps it in. Callers hold mu.
func (r *Registry) rebuild() {
	s := &snapshot{exact: map[netip.Addr]dnssrv.ClientInfo{}}
	// Which matcher claimed each address, so the second one to claim it can
	// say so. A `mac` matcher resolves to whatever address the lease table
	// hands it, which can be one an explicit matcher already names — and the
	// row that loses is a client the dashboard shows with a group it never
	// gets. Nothing here can pick a winner between them, so it is reported
	// rather than decided.
	claims := map[netip.Addr]string{}
	claim := func(ip netip.Addr, matcher string, info dnssrv.ClientInfo) {
		if held, taken := claims[ip]; taken {
			slog.Warn("two client matchers resolve to the same address, the later one wins",
				"address", ip, "matcher", held, "also", matcher)
		}
		claims[ip] = matcher
		s.exact[ip] = info
	}
	for _, c := range r.rows {
		// Canonicalised here as well as at the API, because rows written
		// before that validation existed are still in the table and a
		// matcher that can never match is indistinguishable, from the
		// dashboard, from one that simply hasn't seen its device yet.
		m, ok := NormalizeMatcher(c.matcher)
		if !ok {
			slog.Warn("client matcher is not an IP, CIDR or MAC, skipping it", "client", c.info.ID, "matcher", c.matcher)
			continue
		}
		if mac, ok := strings.CutPrefix(m, macPrefix); ok {
			// A MAC with no lease matches nothing: this server knows which
			// NIC the client named and no address to bind it to, and
			// guessing one would put somebody else in its group.
			if r.leases == nil {
				continue
			}
			ip, ok := r.leases(mac)
			if !ok {
				continue
			}
			claim(ip.Unmap(), m, c.info)
			continue
		}
		if ip, err := netip.ParseAddr(m); err == nil {
			claim(ip, m, c.info)
			continue
		}
		if p, err := netip.ParsePrefix(m); err == nil {
			s.cidrs = append(s.cidrs, cidrEntry{prefix: p, info: c.info})
		}
	}
	slices.SortFunc(s.cidrs, func(a, b cidrEntry) int { return cmp.Compare(b.prefix.Bits(), a.prefix.Bits()) })
	r.snap.Store(s)
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
