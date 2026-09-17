package dhcp

import (
	"context"
	"log/slog"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/aloks98/dnsaur/internal/dnssrv"
	"github.com/aloks98/dnsaur/internal/store"
	"github.com/miekg/dns"
)

// maxNameTTL caps what a lease's name is cached for (§8.1). A lease is state
// this server infers rather than configuration it was given, and five
// minutes is how long a resolver may go on believing an address that has
// since been handed to someone else.
const maxNameTTL = 300 * time.Second

// arpaSuffix is the IPv4 reverse tree. DHCPv4 hands out nothing under
// ip6.arpa, so a query there is never one of these names.
const arpaSuffix = ".in-addr.arpa"

// ZoneCheck reports whether a zone this server holds has an exact record for
// name. The stage asks before it answers, so a record an operator typed
// beats a name inferred from a lease (§8.1); the app wires it to the zone
// index, and a nil one is a server with no zones to lose to.
type ZoneCheck func(name string) bool

// scopeView is the scope list and the dhcp.domain setting as they were when
// the lease table was last built. The reverse path needs both to name a
// scope's suffix, and reading them per query would put whatever the app does
// to answer them in front of every PTR this server sees. Taking them with
// the table also makes the two directions agree: a dhcp.domain edit reaches
// the forward path at the next poll (the table is keyed on the suffix) and
// now reaches the reverse path at the same one.
type scopeView struct {
	scopes []store.Scope
	domain string
}

// Names answers DNS from the lease table: A for a lease's sanitised hostname
// under the suffix its scope hands out, PTR for an address inside a scope,
// and NODATA for any other type of a name a lease does answer to. Everything
// else goes on to the next stage untouched, which is what keeps this in
// front of the zones stage rather than in place of it.
//
// **Call it once.** It subscribes to the manager, and a second call would
// report every name collision twice.
func Names(m *Manager, scopes func() []store.Scope, domain func() string, zc ZoneCheck) dnssrv.Middleware {
	var view atomic.Pointer[scopeView]
	view.Store(&scopeView{scopes: scopes(), domain: domain()})
	// Collisions are a property of the table, not of a query, so they are
	// reported once when the table changes. On the query path this would be
	// a line per lookup for as long as both leases live.
	m.Subscribe(func(t *Table) {
		v := &scopeView{scopes: scopes(), domain: domain()}
		view.Store(v)
		logCollisions(m.log, t, v.scopes, v.domain)
	})

	return func(next dnssrv.Handler) dnssrv.Handler {
		return dnssrv.HandlerFunc(func(ctx context.Context, req *dnssrv.Request) (*dnssrv.Response, error) {
			// Every lease is an IN address; answering a CH or HS question
			// with one would be a wrong answer rather than a missing one,
			// which is the rule the zones stage below holds to as well.
			if len(req.Msg.Question) == 0 || req.Msg.Question[0].Qclass != dns.ClassINET {
				return next.ServeDNS(ctx, req)
			}
			name, table := req.QName(), m.Table()
			var rr dns.RR
			// held is a name this server hands out but has no record of this
			// type for. Answering it NODATA rather than forwarding it is
			// what keeps an internal name off the public internet: a
			// resolver asked for the AAAA of every A it looks up, and a
			// forwarded one both leaks the name and comes back with
			// whatever the internet says about it.
			var held bool
			switch req.QType() {
			case dns.TypeA:
				rr = forward(table, req.Msg.Question[0].Name, name)
			case dns.TypePTR:
				v := view.Load()
				rr = reverse(table, req.Msg.Question[0].Name, name, v.scopes, v.domain)
			default:
				_, held = leaseFor(table, name)
			}
			// The zone is asked only about a name this stage would answer:
			// every other query is one it has no opinion on, and asking
			// about all of them would put a zone lookup in front of every
			// forwarded query for nothing.
			if (rr == nil && !held) || (zc != nil && zc(name)) {
				return next.ServeDNS(ctx, req)
			}
			reply := new(dns.Msg)
			reply.SetReply(req.Msg)
			reply.Authoritative = true
			if rr != nil {
				reply.Answer = []dns.RR{rr}
			}
			return &dnssrv.Response{Msg: reply, Decision: dnssrv.DecisionAuthoritative, Matched: "dhcp"}, nil
		})
	}
}

// leaseFor is the live lease a name resolves to. A lease that has run out is
// not one, which is what keeps the NODATA answer and the A answer agreeing
// about which names this server holds.
func leaseFor(t *Table, name string) (LeaseEntry, bool) {
	label, suffix, ok := strings.Cut(name, ".")
	if !ok {
		return LeaseEntry{}, false
	}
	e, ok := t.ByName(label, suffix)
	if !ok {
		return LeaseEntry{}, false
	}
	if _, live := ttlFor(e); !live {
		return LeaseEntry{}, false
	}
	return e, true
}

// forward is the A record for a leased name, or nil when no lease answers to
// it. qname is the question as asked (the answer echoes it); name is the
// lowercased form the table is keyed on.
func forward(t *Table, qname, name string) dns.RR {
	e, ok := leaseFor(t, name)
	if !ok || !e.IP.Is4() {
		return nil
	}
	ttl, _ := ttlFor(e)
	return &dns.A{
		Hdr: dns.RR_Header{Name: qname, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: ttl},
		A:   net.IP(e.IP.AsSlice()),
	}
}

// reverse is the PTR record for an address a scope hands out, or nil when
// the address is outside every scope, nothing leases it, or the lease has no
// name to point at.
func reverse(t *Table, qname, name string, scopes []store.Scope, domain string) dns.RR {
	addr, ok := addrFromArpa(name)
	if !ok || !inAnyScope(addr, scopes) {
		return nil
	}
	e, ok := t.ByIP(addr)
	if !ok {
		return nil
	}
	target := nameKey(SanitizeLabel(e.Hostname), suffixOf(scopes, domain, e.ScopeID))
	if target == "" {
		return nil
	}
	ttl, ok := ttlFor(e)
	if !ok {
		return nil
	}
	return &dns.PTR{
		Hdr: dns.RR_Header{Name: qname, Rrtype: dns.TypePTR, Class: dns.ClassINET, Ttl: ttl},
		Ptr: dns.Fqdn(target),
	}
}

// ttlFor is min(300, what is left of the lease), and reports false for a
// lease that has already run out: Kea keeps an expired lease in its database
// until it reclaims it, and the address may be someone else's by then, so it
// is not a name this server hands out at all. A reservation nothing has
// leased has no expiry to shorten the TTL with.
func ttlFor(e LeaseEntry) (uint32, bool) {
	if e.ExpiresAt.IsZero() {
		return uint32(maxNameTTL / time.Second), true
	}
	remaining := time.Until(e.ExpiresAt)
	if remaining <= 0 {
		return 0, false
	}
	return uint32(min(remaining, maxNameTTL) / time.Second), true
}

// addrFromArpa reads the address out of an in-addr.arpa name. It is the
// inverse of dns.ReverseAddr, which miekg has no counterpart for.
func addrFromArpa(name string) (netip.Addr, bool) {
	rest, ok := strings.CutSuffix(name, arpaSuffix)
	if !ok {
		return netip.Addr{}, false
	}
	labels := strings.Split(rest, ".")
	if len(labels) != 4 {
		return netip.Addr{}, false
	}
	slices.Reverse(labels)
	// ParseAddr is strict about what each label may be — no leading zeros,
	// nothing out of range — so "010.0.0.1.in-addr.arpa" is not an address
	// this server answers for rather than another spelling of one that is.
	addr, err := netip.ParseAddr(strings.Join(labels, "."))
	if err != nil || !addr.Is4() {
		return netip.Addr{}, false
	}
	return addr, true
}

func inAnyScope(addr netip.Addr, scopes []store.Scope) bool {
	for _, s := range scopes {
		if p, err := netip.ParsePrefix(s.CIDR); err == nil && p.Contains(addr) {
			return true
		}
	}
	return false
}

// suffixOf is the domain a scope's names sit under: its own, or the
// dhcp.domain setting, matching what the table was built with. A scope with
// neither hands out no names.
func suffixOf(scopes []store.Scope, domain string, id int64) string {
	for _, s := range scopes {
		if s.ID == id {
			if s.Domain != "" {
				return s.Domain
			}
			return domain
		}
	}
	return ""
}

// logCollisions reports every lease that wanted a name another lease kept
// (§8.1). The table has already picked the winner; without this the loser is
// simply missing from DNS, with nothing anywhere saying why, and "why does
// my-laptop resolve to the wrong machine" is the question it answers.
func logCollisions(log *slog.Logger, t *Table, scopes []store.Scope, domain string) {
	wanted := map[string][]LeaseEntry{}
	for _, e := range t.All() {
		key := nameKey(SanitizeLabel(e.Hostname), suffixOf(scopes, domain, e.ScopeID))
		if key == "" {
			continue
		}
		wanted[key] = append(wanted[key], e)
	}
	for key, entries := range wanted {
		if len(entries) < 2 {
			continue
		}
		label, suffix, _ := strings.Cut(key, ".")
		kept, ok := t.ByName(label, suffix)
		if !ok {
			continue
		}
		for _, e := range entries {
			// One device with a lease in two scopes under one suffix is not
			// a collision: it is the same machine either way.
			if e.MAC == kept.MAC {
				continue
			}
			log.Warn("two dhcp leases want the same name, the newer keeps it",
				"name", key, "kept", kept.MAC, "dropped", e.MAC)
		}
	}
}
