package zones

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/aloks98/dnsaur/internal/dnssrv"
	"github.com/aloks98/dnsaur/internal/store"
	"github.com/miekg/dns"
)

// The stub fetcher: where a stub zone's queries are sent, and where that
// answer comes from.
//
// A stub claims a suffix and routes it, exactly as a forwarder zone does. The
// difference is the source of the addresses: a forwarder's are typed into
// `forward_to` by the operator, a stub's are *derived from its own NS
// records*. One routing mechanism, two sources.
//
// Which makes a stub a secondary that keeps only the apex. It asks its master
// two ordinary questions — SOA for the serial and the schedule, NS for the
// delegation, with glue in ADDITIONAL — and installs what comes back. Not an
// AXFR, and that is the point rather than an optimisation: a stub needs no
// `allow_transfer` permission on the far end, so it works against a master
// that will not transfer its zone to anybody.
//
// Three things are worth stating before the code, because each is a decision
// that looks like an oversight from inside a single function:
//
//   - **An in-zone nameserver is never resolved.** Its name ends with the
//     apex, so looking it up would match this very zone's suffix, route into
//     the stub, and need the address being resolved. That is a hang, not a
//     slow failure. DNS's own answer is glue, and it is the right one here:
//     the address comes from the master's ADDITIONAL section or the
//     nameserver is unusable. See nsRecords.
//   - **Every nameserver being unusable is not an error.** The zone keeps its
//     claim on the suffix and answers SERVFAIL (§9.11.5), which is what an
//     empty upstream list produces. Falling through instead would let a
//     public record shadow an internal name, which is the failure the whole
//     zone type exists to prevent.
//   - **The glue is stored.** A stub's records are never served — Zone.Answer
//     returns handled=false for the type — so they exist for exactly one
//     reason: StubUpstreams rebuilds the routing table from them on every
//     zone reload, and it must do that without touching the network.

// StubFetcher fetches a stub zone's delegation from its masters and installs
// it.
type StubFetcher struct {
	zs   store.ZoneStore
	keys TSIGKeys
	tsig dns.TsigProvider
	// res resolves both the masters named by hostname (ParsePrimaries) and
	// any *out-of-zone* nameserver. nil means net.DefaultResolver — see
	// Lookup.
	res Lookup
	// now is the clock refreshed_at is stamped from, injected for the same
	// reason Transferrer.now is — and, like it, deliberately not the clock a
	// TSIG request is signed with. See signedExchange.
	now func() time.Time
	// reload republishes the resolver's snapshot after an install.
	// See WithStubReload.
	reload func(context.Context) error
}

// StubOption configures a StubFetcher at construction.
type StubOption func(*StubFetcher)

// WithStubNow replaces the clock refreshed_at is stamped from.
func WithStubNow(now func() time.Time) StubOption {
	return func(f *StubFetcher) { f.now = now }
}

// WithStubResolver sets the resolver masters named by hostname, and
// out-of-zone nameservers, are looked up through. nil (the default) means
// net.DefaultResolver; production passes dnsaur's own forwarder — see Lookup.
func WithStubResolver(res Lookup) StubOption {
	return func(f *StubFetcher) { f.res = res }
}

// WithStubReload gives the fetcher the callback that republishes the served
// snapshot, and it is what completes an install from a querier's point of
// view — the exact counterpart of WithReload on the Transferrer.
//
// **In production this must be App.ReloadZones, never Resolver.Reload.** A
// stub's upstreams are derived from the snapshot, so republishing the
// snapshot without reinstalling the conditional routing table leaves the zone
// claiming its suffix against a table that still has no addresses for it: a
// SERVFAIL indistinguishable from a fetch that never happened. Optional, and
// a fetch without it still lands correctly in the store — it is simply not
// being routed from yet.
func WithStubReload(reload func(context.Context) error) StubOption {
	return func(f *StubFetcher) { f.reload = reload }
}

// NewStubFetcher returns a StubFetcher that installs into zs and signs with
// the keys in keys. keys may be nil, in which case a zone naming a TSIG key
// fails rather than fetching unsigned.
func NewStubFetcher(zs store.ZoneStore, keys TSIGKeys, opts ...StubOption) *StubFetcher {
	if !usableKeys(keys) {
		keys = nil
	}
	f := &StubFetcher{zs: zs, keys: keys, now: time.Now}
	// The provider that verifies inbound signatures is the one that generates
	// outbound ones — see NewTransferrer for the whole reasoning.
	if keys != nil {
		f.tsig = dnssrv.NewTSIGProvider(keys)
	}
	for _, opt := range opts {
		opt(f)
	}
	return f
}

// StubResult describes one completed fetch.
type StubResult struct {
	// Master is the address that answered — not the whole list, the one.
	Master netip.AddrPort
	// Serial is the master's, adopted verbatim, the same way a transfer
	// adopts its primary's.
	Serial uint32
	// NS is how many nameservers the delegation names, installed or not
	// usable. It is deliberately not len(Upstreams): a delegation of two whose
	// glue is missing installs 2 and routes to none, and the difference
	// between those two numbers is the whole diagnosis.
	NS int
	// Upstreams is the dial addresses derived from the NS set, in the order
	// the routing table will try them.
	Upstreams []string
	// Records is how many rows the zone holds after the install: the
	// delegation and the addresses stored beside it. Distinct from NS again,
	// and for the same reason.
	Records int
	// RefreshedAt is the stamp the install wrote, in unix milliseconds. It is
	// the fetcher's clock rather than any caller's, so a scheduler reporting
	// when this zone was last refreshed reports what the row actually says.
	//
	// There is no ExpiresAt beside it, and that is the whole of §9.11.8: a
	// stub is never given one.
	RefreshedAt int64
}

// Fetch queries z's masters for the zone's SOA and NS records and installs
// what comes back.
//
// Two ordinary queries rather than an AXFR: a stub needs no allow_transfer
// permission on the master, which is the whole reason the type exists apart
// from a secondary. Both are signed when the zone names a key — a master that
// requires TSIG on ordinary queries would otherwise refuse the fetch — through
// the same signedExchange the SOA probe uses.
//
// The masters are tried in the order written and any failure moves to the
// next, exactly as Transfer does: masters are meant to be replicas, so one
// refusing is a reason to ask another. The error names every attempt.
//
// A failed fetch changes nothing at all. The zone keeps whatever delegation
// and upstreams it already had, which for a stub that has never fetched means
// it keeps claiming its suffix and answering SERVFAIL.
func (f *StubFetcher) Fetch(ctx context.Context, z store.Zone) (StubResult, error) {
	// Checked before anything is contacted. Installing a delegation into a
	// primary would replace a zone this server authors, and into a secondary
	// the zone it holds on loan — the destruction Transfer's own type check
	// exists to prevent, in the other direction.
	if !strings.EqualFold(z.Type, "stub") {
		return StubResult{}, fmt.Errorf("zone %q is type %q: only a stub is fetched this way", z.Name, z.Type)
	}

	// Resolved now rather than at write time, so a master named by hostname
	// follows its address (see ParsePrimaries). A stub names its masters in
	// the same `primaries` column a secondary does.
	masters, err := ParsePrimaries(ctx, f.res, z.Primaries)
	if err != nil {
		return StubResult{}, fmt.Errorf("zone %q: %w", z.Name, err)
	}
	key, err := zoneTSIGKey(ctx, f.keys, z)
	if err != nil {
		return StubResult{}, err
	}

	var failures []error
	for _, ap := range masters {
		// Checked before each attempt rather than only inside the query, so a
		// cancellation reports itself as one instead of as "every master
		// failed" with a list of confusing i/o errors.
		if err := ctx.Err(); err != nil {
			return StubResult{}, fmt.Errorf("zone %q: %w", z.Name, err)
		}
		soa, ns, extra, err := f.ask(ctx, z.Name, ap, key)
		if err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", ap, err))
			continue
		}
		recs := f.nsRecords(ctx, z, ns, extra)
		res, err := f.install(ctx, z, ap, soa, recs, len(ns))
		if err != nil {
			// The install is this server's storage rather than any master's
			// fault, so it stops the loop instead of moving on.
			return StubResult{}, fmt.Errorf("zone %q from %s: %w", z.Name, ap, err)
		}
		return res, nil
	}
	if err := ctx.Err(); err != nil {
		return StubResult{}, fmt.Errorf("zone %q: %w", z.Name, err)
	}
	return StubResult{}, fmt.Errorf("zone %q: every master failed: %w", z.Name, errors.Join(failures...))
}

// ask runs the two queries against one master and returns the zone's SOA, its
// apex NS records, and the ADDITIONAL section the NS answer carried.
func (f *StubFetcher) ask(ctx context.Context, zoneName string, ap netip.AddrPort, key *store.TSIGKey) (*dns.SOA, []dns.RR, []dns.RR, error) {
	apex := dns.CanonicalName(dns.Fqdn(zoneName))

	soaReply, err := f.exchange(ctx, ap, apex, dns.TypeSOA, key)
	if err != nil {
		return nil, nil, nil, err
	}
	var soa *dns.SOA
	for _, rr := range soaReply.Answer {
		s, ok := rr.(*dns.SOA)
		// The SOA must name the zone we asked about, for the reason probeOne
		// states at length: a keyless fetch is a single unsigned UDP
		// exchange, so a misconfigured multi-tenant master or an off-path
		// forgery could hand back another zone's answer. Believing it would
		// point this suffix at somebody else's nameservers, which is the
		// whole of what a stub decides.
		if ok && strings.EqualFold(dns.CanonicalName(s.Hdr.Name), apex) {
			soa = s
			break
		}
	}
	if soa == nil {
		return nil, nil, nil, fmt.Errorf("the master answered %s with no SOA for %s",
			dns.RcodeToString[soaReply.Rcode], strings.TrimSuffix(apex, "."))
	}

	nsReply, err := f.exchange(ctx, ap, apex, dns.TypeNS, key)
	if err != nil {
		return nil, nil, nil, err
	}
	var ns []dns.RR
	// The ANSWER section only. A master that answers the delegation in
	// AUTHORITY is referring us elsewhere rather than speaking for the zone,
	// and its SOA answer above would already have failed — so requiring the
	// master to be authoritative is a property this fetch has, not one it
	// checks twice.
	for _, rr := range nsReply.Answer {
		if n, ok := rr.(*dns.NS); ok && strings.EqualFold(dns.CanonicalName(n.Hdr.Name), apex) {
			ns = append(ns, n)
		}
	}
	if len(ns) == 0 {
		return nil, nil, nil, fmt.Errorf("the master answered %s with no NS for %s",
			dns.RcodeToString[nsReply.Rcode], strings.TrimSuffix(apex, "."))
	}
	return soa, ns, nsReply.Extra, nil
}

// exchange asks one question, re-asking over TCP when the answer came back
// truncated.
//
// The retry is not optional for the NS query: a delegation with several
// nameservers and glue for each is exactly the shape that overflows a 512-byte
// UDP answer, and a client that believed TC=1 would lose every address
// silently — leaving the zone claiming its suffix with nothing to send there,
// a SERVFAIL indistinguishable from a master that is down. RFC 1035 §4.2.1.
//
// A fresh message per attempt, deliberately: a TSIG-signed request carries its
// signature in the message, and re-sending the one the UDP attempt already
// mutated would sign it twice.
func (f *StubFetcher) exchange(ctx context.Context, ap netip.AddrPort, qname string, qtype uint16, key *store.TSIGKey) (*dns.Msg, error) {
	reply, err := signedExchange(ctx, "", f.tsig, ap, new(dns.Msg).SetQuestion(qname, qtype), key)
	if err != nil {
		return nil, err
	}
	if reply.Truncated {
		reply, err = signedExchange(ctx, "tcp", f.tsig, ap, new(dns.Msg).SetQuestion(qname, qtype), key)
		if err != nil {
			return nil, err
		}
	}
	if reply.Rcode != dns.RcodeSuccess {
		return nil, fmt.Errorf("%s query answered %s", dns.Type(qtype), dns.RcodeToString[reply.Rcode])
	}
	return reply, nil
}

// nsRecords turns the fetched NS RRset and the master's ADDITIONAL section
// into the rows to store: the delegation itself, and beside each nameserver
// the addresses that resolve it.
//
// **An in-zone nameserver is never resolved.** Its name ends with the apex, so
// resolving it would match this very zone's suffix, route into the stub, and
// need the address being resolved — a hang, not an error. DNS's own answer is
// glue, and it is the right one: the address must come from the master's
// ADDITIONAL section or the nameserver is unusable and contributes nothing.
//
// An out-of-zone name cannot re-enter this zone, so it is resolved normally,
// through the same Lookup ParsePrimaries uses. Its addresses are stored
// beside it exactly as glue is, because StubUpstreams has to rebuild the
// routing table on a zone reload without querying anything.
//
// A nameserver with no usable address is still installed. The delegation is
// what the master said, and the difference between "two nameservers, no
// addresses" and "no delegation at all" is the diagnosis an operator needs;
// StubUpstreams simply finds nothing to dial for it.
//
// Nothing here fails. Every record goes through BuildRecord, the same
// validator POST /zones/{id}/records enforces, but one that will not pass is
// skipped rather than rejecting the whole delegation — the opposite of
// Transferrer.build, and deliberately so. A transfer's records are *served*,
// so a zone installed minus some of them would be answered authoritatively and
// wrong. A stub's are never served (Zone.Answer returns handled=false for the
// type); they only name where to send queries, and routing to the nameservers
// that were usable is strictly better than SERVFAILing the whole suffix
// because one glue record was malformed.
func (f *StubFetcher) nsRecords(ctx context.Context, z store.Zone, ns []dns.RR, extra []dns.RR) []store.ZoneRecord {
	apex := dns.CanonicalName(dns.Fqdn(z.Name))
	recs := make([]store.ZoneRecord, 0, len(ns)*2)

	add := func(name, typ string, ttl uint32, rdata string) {
		rec, err := BuildRecord(z, RecordWrite{Name: name, Type: typ, TTL: ttl, RData: rdata}, recs, 0, false)
		if err != nil {
			slog.Warn("stub zone: a record from the master was not stored",
				"zone", z.Name, "name", name, "type", typ, "err", err)
			return
		}
		recs = append(recs, rec)
	}

	seen := make(map[string]bool, len(ns))
	for _, rr := range ns {
		n, ok := rr.(*dns.NS)
		if !ok {
			continue
		}
		add(apex, "NS", n.Hdr.Ttl, RDataOf(n))

		target := dns.CanonicalName(n.Ns)
		// One RRset can name the same server twice only if the master is
		// misbehaving, but resolving it twice would be two lookups and two
		// copies of every address.
		if seen[target] {
			continue
		}
		seen[target] = true

		var addrs []netip.Addr
		if dns.IsSubDomain(apex, target) {
			addrs = glueFor(target, extra)
			if len(addrs) == 0 {
				// Not an error, and above all not a lookup. See the glue rule
				// above: this nameserver is simply unusable.
				slog.Warn("stub zone: an in-zone nameserver arrived with no glue and is unusable",
					"zone", z.Name, "nameserver", strings.TrimSuffix(target, "."))
				continue
			}
		} else {
			res := f.res
			if res == nil {
				res = net.DefaultResolver
			}
			found, err := res.LookupNetIP(ctx, "ip", strings.TrimSuffix(target, "."))
			if err != nil {
				slog.Warn("stub zone: an out-of-zone nameserver did not resolve and is unusable",
					"zone", z.Name, "nameserver", strings.TrimSuffix(target, "."), "err", err)
				continue
			}
			addrs = found
		}
		for _, a := range addrs {
			// Unmapped so a v4 address is stored and dialled as 10.0.0.1
			// rather than ::ffff:10.0.0.1 — the same normalisation
			// primary.resolve applies, and for the same reason.
			a = a.Unmap()
			typ := "A"
			if a.Is6() {
				typ = "AAAA"
			}
			add(strings.TrimSuffix(target, "."), typ, n.Hdr.Ttl, a.String())
		}
	}
	return recs
}

// stubAbsoluteNames returns the record names a stub holds that are *absolute*
// names rather than apex-relative ones, keyed the way normalizeName spells a
// stored name.
//
// A stub is the one zone type whose zone_records.name column is not uniformly
// relative. nsRecords stores an out-of-zone nameserver's addresses under the
// nameserver's own name ("ns.example.net" in zone corp.lan), because
// RelRecordName has no apex to strip off it and leaves it alone — see the
// contrast RelRecordName's own comment draws with RelName. Nothing that
// *serves* a zone is affected, since a stub serves no records at all
// (Zone.Answer returns handled=false for the type) and StubUpstreams looks
// the name up by exactly that spelling. Render is affected: a master file
// under "$ORIGIN corp.lan." reads that same text as relative and the exported
// file would address ns.example.net.corp.lan.
//
// Which name is which cannot be decided from the name alone — "ns.example.net"
// under a primary is a perfectly ordinary relative name, served at
// ns.example.net.corp.lan (RecordFQDN), and it must keep exporting that way.
// It is decided the same way it was written: a glue name is absolute exactly
// when the apex NS RRset names it and it is not under the apex, which is the
// nsRecords branch that stored it unstripped. Disabled records count here —
// they are not rendered, but a disabled NS still says what its glue's name
// means.
func stubAbsoluteNames(z store.Zone, recs []store.ZoneRecord) map[string]bool {
	if !strings.EqualFold(z.Type, "stub") {
		return nil
	}
	apex := dns.CanonicalName(dns.Fqdn(z.Name))
	var out map[string]bool
	for _, r := range recs {
		if !strings.EqualFold(r.Type, "NS") || normalizeName(r.Name) != apexName {
			continue
		}
		// The rdata of an NS is its target, in the spelling RDataOf produced —
		// the same read StubUpstreams makes of the same column.
		target := dns.CanonicalName(dns.Fqdn(r.RData))
		if target == "." || dns.IsSubDomain(apex, target) {
			continue
		}
		if out == nil {
			out = make(map[string]bool)
		}
		out[normalizeName(r.RData)] = true
	}
	return out
}

// glueFor returns the addresses extra carries for name. Only A and AAAA, and
// only at exactly that owner name: everything else in an ADDITIONAL section
// is somebody else's business.
func glueFor(name string, extra []dns.RR) []netip.Addr {
	var out []netip.Addr
	for _, rr := range extra {
		if !strings.EqualFold(dns.CanonicalName(rr.Header().Name), name) {
			continue
		}
		switch v := rr.(type) {
		case *dns.A:
			if a, ok := netip.AddrFromSlice(v.A.To4()); ok {
				out = append(out, a)
			}
		case *dns.AAAA:
			if a, ok := netip.AddrFromSlice(v.AAAA); ok {
				out = append(out, a)
			}
		}
	}
	return out
}

// install writes the fetched delegation as one transaction and republishes it.
func (f *StubFetcher) install(ctx context.Context, z store.Zone, ap netip.AddrPort, soa *dns.SOA, recs []store.ZoneRecord, nsCount int) (StubResult, error) {
	// The write half must not be abandoned because whoever asked for the
	// fetch went away — same reasoning as Transferrer.install, and the same
	// transaction underneath. Everything cancellable has already happened.
	ctx = context.WithoutCancel(ctx)

	existing, err := f.zs.Records(ctx, z.ID)
	if err != nil {
		return StubResult{}, fmt.Errorf("reading the zone's current records: %w", err)
	}
	// Diffed rather than deleted-and-re-added, through the same function the
	// import and the transfer use: a fetch that found the delegation unchanged
	// issues no record statements at all instead of churning every row id on a
	// schedule.
	diff := DiffRecords(z.Name, existing, recs)

	// The operator's *current* row with the new SOA applied on top, not the
	// copy the fetch started from — see Transferrer.install for the edit this
	// re-read stops a slow fetch from silently undoing.
	current, err := f.zs.Zone(ctx, z.ID)
	if err != nil {
		return StubResult{}, fmt.Errorf("re-reading the zone before installing it: %w", err)
	}
	if !strings.EqualFold(current.Type, "stub") {
		return StubResult{}, fmt.Errorf("zone %q became type %q while the fetch was running", current.Name, current.Type)
	}
	z = current

	now := f.now()
	nowMs := now.UnixMilli()
	before := z
	z.SOANS = strings.TrimSuffix(soa.Ns, ".")
	z.SOAMbox = strings.TrimSuffix(soa.Mbox, ".")
	z.SOASerial = soa.Serial
	z.SOARefresh = soa.Refresh
	z.SOARetry = soa.Retry
	z.SOAExpire = soa.Expire
	z.SOAMinimum = soa.Minttl
	z.SOATTL = soa.Hdr.Ttl
	z.RefreshedAt = nowMs
	// **expires_at is deliberately not written, and a stub deliberately does
	// not expire.** Zone.Serving's expiry rule exists because a secondary
	// serves its primary's *data* on loan, and answering from data it can no
	// longer confirm is worse than answering nothing. A stub serves no data at
	// all: it holds a delegation and forwards. A stub that "expired" would
	// stop routing and start SERVFAILing a suffix whose nameservers are
	// probably still perfectly reachable — it would break the zone in order to
	// protect against staleness that cannot hurt anyone.
	if contentChanged(before, z, diff) {
		z.ModifiedAt = nowMs
	}

	if err := f.zs.ReplaceRecords(ctx, z, diff.DeleteIDs(), diff.Updates(), diff.Add); err != nil {
		return StubResult{}, err
	}

	// The install committed, so a reload failure does not undo it and must not
	// be reported as a failed fetch — same reasoning as Transferrer.install.
	// What it costs is that the zone goes on routing from the previous
	// snapshot, which for a stub's first fetch means SERVFAIL.
	if f.reload != nil {
		if err := f.reload(ctx); err != nil {
			slog.Error("zone reload after a stub fetch failed", "zone", z.Name, "master", ap.String(), "err", err)
		}
	}

	return StubResult{
		Master:      ap,
		Serial:      soa.Serial,
		NS:          nsCount,
		Records:     len(recs),
		RefreshedAt: nowMs,
		// Derived through the very function the routing table is rebuilt with,
		// over the very records just written, so what this fetch reports and
		// what the next reload installs cannot disagree.
		Upstreams: StubUpstreams(NewZone(z, recs)),
	}, nil
}

// StubUpstreams returns the addresses a stub zone's queries go to, derived
// from the NS records it has fetched and the addresses stored beside them.
//
// Pure: it takes no context and no resolver, so it *cannot* query. That is
// what lets internal/app rebuild the conditional routing table on every zone
// reload without touching the network, and it is why nsRecords stores an
// out-of-zone nameserver's addresses rather than leaving them to be looked up
// here.
//
// Only the apex's NS set counts. An NS below the apex is a child zone's
// delegation, and dialling it would send this zone's queries to a server
// authoritative for something else.
//
// A stub that has not fetched yet names none, and so does one whose every
// nameserver turned out to be unusable. A zone that names none claims its
// suffix and answers SERVFAIL rather than falling through (§9.11.5) — so the
// empty return is a correct answer here, never a placeholder.
func StubUpstreams(z Zone) []string {
	port := strconv.Itoa(DefaultPrimaryPort)
	var out []string
	seen := map[string]bool{}
	for _, rec := range z.Records[apexName] {
		if !strings.EqualFold(rec.Type, "NS") {
			continue
		}
		// The rdata of an NS is its target, in the spelling RDataOf produced.
		rel := RelRecordName(normalizeName(rec.RData), z.Name)
		for _, addr := range z.Records[rel] {
			if !strings.EqualFold(addr.Type, "A") && !strings.EqualFold(addr.Type, "AAAA") {
				continue
			}
			ip, err := netip.ParseAddr(addr.RData)
			if err != nil {
				continue
			}
			dial := net.JoinHostPort(ip.Unmap().String(), port)
			// Two nameservers behind one address is a real deployment, and the
			// forwarder must not be told to try it twice.
			if seen[dial] {
				continue
			}
			seen[dial] = true
			out = append(out, dial)
		}
	}
	return out
}
