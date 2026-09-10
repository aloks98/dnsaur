package app

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"sync"

	"github.com/aloks98/dnsaur/internal/dnssrv"
	"github.com/miekg/dns"
)

// zoneLookup resolves the hostnames a zone's configuration is allowed to
// carry — a secondary's `primaries`, a NOTIFY target, a stub's out-of-zone
// nameserver — through dnsaur's own cache and forwarder rather than through
// the host machine's resolver. It is what internal/zones' Lookup is for, and
// the only implementation of it besides *net.Resolver.
//
// **Two things follow from where it enters the pipeline, and both are the
// point.** It enters *below* the zones stage, so a hostname primary that
// lives inside the very zone it serves — `ns1.corp.example` as the primary
// of `corp.example` — is resolved from the configured upstreams instead of
// from a zone that has nothing in it yet. Through the host's resolver, which
// on a machine running dnsaur is usually dnsaur, that name reached this
// server's zones stage, where a secondary that has never transferred answers
// SERVFAIL for its whole suffix: the transfer that would fix that was the
// one thing that could not happen. And because it enters the pipeline rather
// than a socket, these lookups follow whatever `upstreams` says — DoT and
// DoH included — instead of leaking in the clear to the system resolver.
//
// The stage above the forwarder is the cache, which is deliberate too: the
// NOTIFY gate resolves a hostname primary to check an arriving packet's
// source against it, so an uncached lookup there is one outbound query per
// packet from anyone who can spell the zone's name.
type zoneLookup struct{ a *App }

// zoneLookup returns the App's own Lookup, exported to this package's tests
// so they can drive it without a transfer around it.
func (a *App) zoneLookup() zoneLookup { return zoneLookup{a: a} }

// tail is the cache-and-forwarder end of the pipeline a.handler composes:
// the same *cache.Cache the query path fills and the same swappable
// forwarder, so a settings change or a zone reload reaches these lookups
// without anything here re-reading a setting.
//
// a.dnsCache is written once, in Start, before any listener or background
// worker that could reach this exists; nothing can call a lookup before
// then. The nil branch is for an App built but never started, which is only
// a test.
func (l zoneLookup) tail() dnssrv.Handler {
	if l.a.dnsCache == nil {
		return l.a.fwd
	}
	return l.a.dnsCache.Middleware()(l.a.fwd)
}

// LookupNetIP resolves host, honouring network the way *net.Resolver does:
// "ip" asks for both families, "ip4" and "ip6" for one each.
//
// It reports an error whenever it has no address to return, never an empty
// slice — the invariant ParsePrimaries and udpSender.resolve both rely on to
// tell "this primary was tried" apart from "this primary was never dialled".
func (l zoneLookup) LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error) {
	var qtypes []uint16
	switch network {
	case "ip":
		qtypes = []uint16{dns.TypeA, dns.TypeAAAA}
	case "ip4":
		qtypes = []uint16{dns.TypeA}
	case "ip6":
		qtypes = []uint16{dns.TypeAAAA}
	default:
		return nil, net.UnknownNetworkError(network)
	}
	h := l.tail()
	// Concurrently, as the standard resolver does: run serially, a name whose
	// upstreams are silent costs two full forwarder timeouts, and NOTIFY's
	// whole budget is ten seconds.
	found := make([][]netip.Addr, len(qtypes))
	errs := make([]error, len(qtypes))
	var wg sync.WaitGroup
	for i, qtype := range qtypes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			found[i], errs[i] = ask(ctx, h, host, qtype)
		}()
	}
	wg.Wait()
	out := slices.Concat(found...)
	if len(out) == 0 {
		return nil, fmt.Errorf("lookup %s: %w", host, errors.Join(errs...))
	}
	return out, nil
}

// ask sends one question into h and reads the addresses out of the answer.
//
// The message is built the way upstream.upstreamQuery builds one — fresh id,
// RD, a single question, its own OPT — rather than by forwarding something a
// client sent, because there is no client here: this query is dnsaur's own.
//
// A v4 address is normalised to its 4-byte form and a v6 one is left exactly
// as it arrived, which is what *net.Resolver does and what the callers'
// Unmap calls are written against.
func ask(ctx context.Context, h dnssrv.Handler, host string, qtype uint16) ([]netip.Addr, error) {
	m := new(dns.Msg)
	m.Id = dns.Id()
	m.RecursionDesired = true
	m.Question = []dns.Question{{Name: dns.Fqdn(host), Qtype: qtype, Qclass: dns.ClassINET}}
	m.SetEdns0(1232, false)

	resp, err := h.ServeDNS(ctx, &dnssrv.Request{Msg: m})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", dns.TypeToString[qtype], err)
	}
	if resp == nil || resp.Msg == nil {
		return nil, fmt.Errorf("%s: no answer", dns.TypeToString[qtype])
	}
	if resp.Msg.Rcode != dns.RcodeSuccess {
		return nil, fmt.Errorf("%s: %s", dns.TypeToString[qtype], dns.RcodeToString[resp.Msg.Rcode])
	}
	var out []netip.Addr
	for _, rr := range resp.Msg.Answer {
		var ip net.IP
		switch v := rr.(type) {
		case *dns.A:
			ip = v.A.To4()
		case *dns.AAAA:
			ip = v.AAAA
		default:
			continue
		}
		if addr, ok := netip.AddrFromSlice(ip); ok {
			out = append(out, addr)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s: no addresses", dns.TypeToString[qtype])
	}
	return out, nil
}
