package api

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/aloks98/dnsaur/internal/dnssrv"
	"github.com/aloks98/dnsaur/internal/zones"
	"github.com/miekg/dns"
)

// The export → import round trip, judged by what the resolver answers.
//
// This file exists because the round-trip test that came before it could not
// have caught the bug it was meant to guard. That test built its fixture zone
// by importing a file, then exported it and imported it again — and a zone
// that arrived through zones.Parse is already stored in the parser's own
// spelling, so re-rendering and re-parsing it is a fixed point by
// construction. Identity on a fixed point says nothing about the values that
// are not fixed points, and a dotless target typed into the form
// ("nas.e412.in", the spelling Cloudflare and Route 53 train users into) is
// exactly such a value: stored verbatim it was served as nas.e412.in. and
// exported under "$ORIGIN e412.in." — where the identical text is relative —
// so it came back as nas.e412.in.e412.in., a different name.
//
// Two rules follow, and every test here obeys both:
//
//  1. Records are seeded through the API write path (POST /records), never
//     through zones.Parse. Seeding through the parser is what reproduces the
//     hole: it can only produce values the parser already agrees with.
//  2. Assertions are on what the resolver answers, before and after the round
//     trip — not on the stored strings and not on the exported file. The
//     stored text stayed self-consistent through the entire bug; what changed
//     was the name it resolved to.
//
// The resolver half is the real zones.Resolver, queried through
// Resolver.Middleware with a dnssrv.Request, the way internal/zones' own
// resolver tests query it (resolver_test.go) and the way the DNS pipeline
// does — not a second, test-only way to ask a question. It is wired to the
// same store the handlers write to and reloaded from the same Reloader hook
// production uses, so the snapshot being queried is the one the write itself
// published.

// resolvingReloader drives a real zones.Resolver from the reload every write
// handler already performs (reloadZones → Deps.Reloader.ReloadZones). This is
// what internal/app does in production — App.ReloadZones is a
// resolver.Reload — so nothing here reloads on a schedule of the test's own
// invention.
type resolvingReloader struct {
	*fakeReloader
	res *zones.Resolver
}

func (rr *resolvingReloader) ReloadZones(ctx context.Context) error {
	if err := rr.res.Reload(ctx); err != nil {
		return err
	}
	return rr.fakeReloader.ReloadZones(ctx)
}

// resolver attaches a live Resolver to this server's store and returns it.
// Server reads deps.Reloader at call time, so swapping it after construction
// is enough (the same trick captureReloads uses, autoptr_test.go), and the
// embedded fakeReloader keeps ts.rl's counts working.
//
// Call this after the zone exists and before any record is written: from
// here on, every accepted write republishes the snapshot these queries see.
func (ts *zoneTestServer) resolver(t *testing.T) *zones.Resolver {
	t.Helper()
	res := zones.NewResolver(ts.store.Zones())
	if err := res.Reload(t.Context()); err != nil {
		t.Fatalf("resolver reload: %v", err)
	}
	ts.srv.deps.Reloader = &resolvingReloader{fakeReloader: ts.rl, res: res}
	return res
}

// dnsReply is one response, reduced to what a client can actually observe:
// the rcode, the AA bit, and every RR in it.
type dnsReply struct {
	rcode               string
	aa                  bool
	answer, auth, extra []string
}

func (r dnsReply) String() string {
	return fmt.Sprintf("rcode=%s aa=%t answer=[%s] authority=[%s] additional=[%s]",
		r.rcode, r.aa,
		strings.Join(r.answer, " ; "), strings.Join(r.auth, " ; "), strings.Join(r.extra, " ; "))
}

// ask puts one question to the resolver. next stands in for the rest of the
// pipeline: reaching it means the name left the zones we hold, which is a
// leak rather than an answer, so it is reported as one instead of being
// quietly compared as an empty reply.
func ask(t *testing.T, res *zones.Resolver, qname string, qtype uint16) dnsReply {
	t.Helper()
	forwarded := false
	next := dnssrv.HandlerFunc(func(ctx context.Context, req *dnssrv.Request) (*dnssrv.Response, error) {
		forwarded = true
		return &dnssrv.Response{Msg: new(dns.Msg), Decision: dnssrv.DecisionForwarded}, nil
	})
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(qname), qtype)

	resp, err := res.Middleware()(next).ServeDNS(t.Context(), &dnssrv.Request{Msg: m})
	if err != nil {
		t.Fatalf("resolving %s %s: %v", qname, dns.TypeToString[qtype], err)
	}
	if forwarded {
		return dnsReply{rcode: "FORWARDED"}
	}
	return dnsReply{
		rcode:  dns.RcodeToString[resp.Msg.Rcode],
		aa:     resp.Msg.Authoritative,
		answer: rrLines(resp.Msg.Answer),
		auth:   rrLines(resp.Msg.Ns),
		extra:  rrLines(resp.Msg.Extra),
	}
}

// rrLines prints a section canonically, sorted: an import rewrites rows, so
// the order records come back in is not part of what an RRSet means.
//
// The SOA's serial is blanked, and it is the only thing that is. An import
// legitimately advances the serial (applyZoneFile takes max(file, current)+1
// so a zone can never appear to go backwards), so it is the one value that
// must differ across a round trip. Everything else the SOA carries —
// including the negative-cache TTL every NXDOMAIN below rests on — is
// compared.
func rrLines(rrs []dns.RR) []string {
	out := make([]string, 0, len(rrs))
	for _, rr := range rrs {
		if soa, ok := rr.(*dns.SOA); ok {
			blanked := *soa
			blanked.Serial = 0
			rr = &blanked
		}
		out = append(out, rr.String())
	}
	slices.Sort(out)
	return out
}

// wantRRs canonicalises an expected answer written as ordinary master-file
// text, so a case can state what it wants in the form a zone file states it
// and still compare against exactly what miekg/dns prints.
func wantRRs(t *testing.T, lines []string) []string {
	t.Helper()
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		rr, err := dns.NewRR(line)
		if err != nil {
			t.Fatalf("the expectation %q is not a valid RR: %v", line, err)
		}
		out = append(out, rr.String())
	}
	slices.Sort(out)
	return out
}

// roundTrip is the operation the bug report describes: download the zone
// (GET /zones/{id}/file) and load that exact file back (POST /zones/{id}/file
// with dry_run false). Nothing about the file itself is asserted here — what
// it has to preserve is the answers, and those are checked around this call.
func (ts *zoneTestServer) roundTrip(t *testing.T, zid int64) {
	t.Helper()
	export := ts.do(t, "GET", fmt.Sprintf("/api/v1/zones/%d/file", zid), "")
	if export.Code != http.StatusOK {
		t.Fatalf("export: status = %d body = %s", export.Code, export.Body)
	}
	imp := ts.do(t, "POST", fmt.Sprintf("/api/v1/zones/%d/file", zid), importBody(t, export.Body.String(), false))
	if imp.Code != http.StatusOK {
		t.Fatalf("importing the zone's own export: status = %d body = %s\nfile:\n%s", imp.Code, imp.Body, export.Body)
	}
}

// roundTripSeed is one record written through POST /records. status is the
// HTTP status the write must get; 0 means 201 Created.
type roundTripSeed struct {
	name, recType, rdata string
	ttl                  uint32
	status               int
}

// roundTripAsk is one question the resolver must answer identically before
// and after the round trip, plus what that answer has to be. rcode 0 is
// NOERROR; want is the ANSWER section as master-file lines.
type roundTripAsk struct {
	qname string
	qtype uint16
	rcode int
	want  []string
}

func seedRecords(t *testing.T, srv *zoneTestServer, zid int64, seeds []roundTripSeed) {
	t.Helper()
	for _, s := range seeds {
		want := s.status
		if want == 0 {
			want = http.StatusCreated
		}
		rec := srv.do(t, "POST", fmt.Sprintf("/api/v1/zones/%d/records", zid),
			recordBody(t, s.name, s.recType, s.ttl, s.rdata))
		if rec.Code != want {
			t.Fatalf("POST %s %s %q: status = %d body = %s, want %d",
				s.name, s.recType, s.rdata, rec.Code, rec.Body, want)
		}
	}
}

// assertRoundTripPreservesAnswers is the shape of every test in this file:
// establish what the zone answers, run the file round trip, and require the
// answers to be unchanged.
//
// The "before" half is checked against the case's stated expectation rather
// than merely recorded, because before == after is only meaningful once
// before is known to be right — two identical wrong answers would otherwise
// read as a pass.
func assertRoundTripPreservesAnswers(t *testing.T, srv *zoneTestServer, res *zones.Resolver, zid int64, asks []roundTripAsk) {
	t.Helper()
	before := make([]dnsReply, len(asks))
	for i, q := range asks {
		label := fmt.Sprintf("%s %s", q.qname, dns.TypeToString[q.qtype])
		before[i] = ask(t, res, q.qname, q.qtype)
		if got, want := before[i].rcode, dns.RcodeToString[q.rcode]; got != want {
			t.Fatalf("%s: rcode = %s, want %s\nreply: %s", label, got, want, before[i])
		}
		if got, want := before[i].answer, wantRRs(t, q.want); !slices.Equal(got, want) {
			t.Fatalf("%s answered\n  %v\nwant\n  %v", label, got, want)
		}
	}

	srv.roundTrip(t, zid)

	for i, q := range asks {
		label := fmt.Sprintf("%s %s", q.qname, dns.TypeToString[q.qtype])
		if after := ask(t, res, q.qname, q.qtype); after.String() != before[i].String() {
			t.Errorf("%s changed across export → import:\n before: %s\n  after: %s", label, before[i], after)
		}
	}
}

// The apex NS every zone created through POST /api/v1/zones is seeded with
// (handleZoneCreate), which shares the apex with the NS cases below.
const apexNSAnswer = "e412.in. 3600 IN NS ns.e412.in."

// Every rdata type that embeds a domain name, in each of the three spellings
// a user arrives with. The three are not synonyms and the expectations do not
// pretend they are: dnsaur reads rdata under no origin, so a bare label is a
// name at the root and stays one. What must hold is that whatever the record
// resolves to when it is written is what it still resolves to after the zone
// has been through a file — the property that failed for the dotless
// spelling, which is a name at the root's *child*, i.e. an ordinary absolute
// name, right up until a master file re-reads it under $ORIGIN.
func TestZoneFileRoundTripPreservesNameValuedAnswers(t *testing.T) {
	cases := []struct {
		what    string
		zone    string
		name    string
		recType string
		ttl     uint32
		rdata   string
		qtype   uint16
		want    []string
	}{
		// The bug as reported.
		{
			what: "CNAME dotless absolute target", zone: "e412.in",
			name: "git", recType: "CNAME", ttl: 300, rdata: "nas.e412.in",
			qtype: dns.TypeCNAME, want: []string{"git.e412.in. 300 IN CNAME nas.e412.in."},
		},
		{
			what: "CNAME bare relative target", zone: "e412.in",
			name: "www", recType: "CNAME", ttl: 300, rdata: "nas",
			qtype: dns.TypeCNAME, want: []string{"www.e412.in. 300 IN CNAME nas."},
		},
		{
			what: "CNAME fully qualified target", zone: "e412.in",
			name: "vcs", recType: "CNAME", ttl: 300, rdata: "nas.e412.in.",
			qtype: dns.TypeCNAME, want: []string{"vcs.e412.in. 300 IN CNAME nas.e412.in."},
		},
		{
			what: "MX dotless absolute exchange", zone: "e412.in",
			name: "@", recType: "MX", ttl: 3600, rdata: "10 mail.e412.in",
			qtype: dns.TypeMX, want: []string{"e412.in. 3600 IN MX 10 mail.e412.in."},
		},
		{
			what: "MX bare relative exchange", zone: "e412.in",
			name: "@", recType: "MX", ttl: 3600, rdata: "10 mail",
			qtype: dns.TypeMX, want: []string{"e412.in. 3600 IN MX 10 mail."},
		},
		{
			what: "MX fully qualified exchange", zone: "e412.in",
			name: "@", recType: "MX", ttl: 3600, rdata: "10 mail.e412.in.",
			qtype: dns.TypeMX, want: []string{"e412.in. 3600 IN MX 10 mail.e412.in."},
		},
		// The apex already holds the NS the zone was created with, so these
		// answer with two records; the TTL matches it because RFC 2181 §5.2
		// (enforced in buildZoneRecord) requires one TTL across an RRSet.
		{
			what: "NS dotless absolute target", zone: "e412.in",
			name: "@", recType: "NS", ttl: 3600, rdata: "ns2.e412.in",
			qtype: dns.TypeNS, want: []string{apexNSAnswer, "e412.in. 3600 IN NS ns2.e412.in."},
		},
		{
			what: "NS bare relative target", zone: "e412.in",
			name: "@", recType: "NS", ttl: 3600, rdata: "ns2",
			qtype: dns.TypeNS, want: []string{apexNSAnswer, "e412.in. 3600 IN NS ns2."},
		},
		{
			what: "NS fully qualified target", zone: "e412.in",
			name: "@", recType: "NS", ttl: 3600, rdata: "ns2.e412.in.",
			qtype: dns.TypeNS, want: []string{apexNSAnswer, "e412.in. 3600 IN NS ns2.e412.in."},
		},
		// PTR in the reverse zone it belongs in, rather than as a curiosity in
		// a forward one.
		{
			what: "PTR dotless absolute target", zone: "150.168.192.in-addr.arpa",
			name: "10", recType: "PTR", ttl: 300, rdata: "nas.e412.in",
			qtype: dns.TypePTR,
			want:  []string{"10.150.168.192.in-addr.arpa. 300 IN PTR nas.e412.in."},
		},
		{
			what: "PTR bare relative target", zone: "150.168.192.in-addr.arpa",
			name: "10", recType: "PTR", ttl: 300, rdata: "nas",
			qtype: dns.TypePTR,
			want:  []string{"10.150.168.192.in-addr.arpa. 300 IN PTR nas."},
		},
		{
			what: "PTR fully qualified target", zone: "150.168.192.in-addr.arpa",
			name: "10", recType: "PTR", ttl: 300, rdata: "nas.e412.in.",
			qtype: dns.TypePTR,
			want:  []string{"10.150.168.192.in-addr.arpa. 300 IN PTR nas.e412.in."},
		},
		{
			what: "SRV dotless absolute target", zone: "e412.in",
			name: "_sip._tcp", recType: "SRV", ttl: 300, rdata: "10 20 5060 sip.e412.in",
			qtype: dns.TypeSRV, want: []string{"_sip._tcp.e412.in. 300 IN SRV 10 20 5060 sip.e412.in."},
		},
		{
			what: "SRV bare relative target", zone: "e412.in",
			name: "_sip._tcp", recType: "SRV", ttl: 300, rdata: "10 20 5060 sip",
			qtype: dns.TypeSRV, want: []string{"_sip._tcp.e412.in. 300 IN SRV 10 20 5060 sip."},
		},
		{
			what: "SRV fully qualified target", zone: "e412.in",
			name: "_sip._tcp", recType: "SRV", ttl: 300, rdata: "10 20 5060 sip.e412.in.",
			qtype: dns.TypeSRV, want: []string{"_sip._tcp.e412.in. 300 IN SRV 10 20 5060 sip.e412.in."},
		},
	}

	for _, c := range cases {
		t.Run(c.what, func(t *testing.T) {
			srv := newTestServer(t)
			// Through the create handler, so the zone is exactly what a user
			// gets — apex NS included, which the NS cases share a name with.
			zid := srv.createZone(t, c.zone)
			res := srv.resolver(t)

			seedRecords(t, srv, zid, []roundTripSeed{{name: c.name, recType: c.recType, ttl: c.ttl, rdata: c.rdata}})
			assertRoundTripPreservesAnswers(t, srv, res, zid, []roundTripAsk{{
				qname: zones.RecordFQDN(c.zone, zones.RelRecordName(c.name, c.zone)),
				qtype: c.qtype,
				want:  c.want,
			}})
		})
	}
}

// The rdata that is not a name, and is awkward for its own reasons. Each case
// is a value a master file can mangle, silently, in a way only the answer
// reveals.
func TestZoneFileRoundTripPreservesAwkwardRData(t *testing.T) {
	// One character-string cannot exceed 255 bytes (RFC 1035 §3.3.14), so
	// miekg/dns splits a longer one at 255 and the RR carries two strings.
	// That is a change to how the value is written down, and the point of the
	// case is that a file cannot turn it into a change of value.
	longTXT := strings.Repeat("x", 300)

	cases := []struct {
		what  string
		zone  string
		seeds []roundTripSeed
		asks  []roundTripAsk
	}{
		// The injection case, and the reason rdata is normalised for every
		// type rather than only the name-valued ones. dns.NewRR reads one RR
		// and silently discards whatever follows it, so this validates, is
		// served as nothing but 1.2.3.4 — and, stored verbatim, is written
		// into the exported file in full, where the trailing line is a second
		// record the zone never held. Importing that file publishes it.
		{
			what: "trailing rdata cannot smuggle a second record through the file",
			zone: "e412.in",
			seeds: []roundTripSeed{
				{name: "host", recType: "A", ttl: 300, rdata: "1.2.3.4\nevil.e412.in. 300 IN A 6.6.6.6"},
			},
			asks: []roundTripAsk{
				{qname: "host.e412.in", qtype: dns.TypeA, want: []string{"host.e412.in. 300 IN A 1.2.3.4"}},
				// The name the smuggled line would create. It does not exist
				// before the round trip and must not exist after it.
				{qname: "evil.e412.in", qtype: dns.TypeA, rcode: dns.RcodeNameError},
			},
		},
		{
			what: "TXT with a semicolon, embedded quotes, and a string over 255 bytes",
			zone: "e412.in",
			seeds: []roundTripSeed{
				// Quoted, so the semicolon is content. Unquoted it would start
				// a master-file comment — see the record below.
				{name: "spf", recType: "TXT", ttl: 300, rdata: `"v=spf1 -all; not a comment"`},
				{name: "quoted", recType: "TXT", ttl: 300, rdata: `"say \"hi\""`},
				{name: "long", recType: "TXT", ttl: 300, rdata: `"` + longTXT + `"`},
				// An unquoted semicolon is a comment in master-file syntax, so
				// dns.NewRR keeps only "hello" and that is what the record
				// means from the moment it is accepted. The round trip has to
				// preserve that meaning too — the failure to guard against is
				// the file and the zone disagreeing, not the parser's rule.
				{name: "note", recType: "TXT", ttl: 300, rdata: "hello; the rest is a comment"},
			},
			asks: []roundTripAsk{
				{qname: "spf.e412.in", qtype: dns.TypeTXT, want: []string{`spf.e412.in. 300 IN TXT "v=spf1 -all; not a comment"`}},
				{qname: "quoted.e412.in", qtype: dns.TypeTXT, want: []string{`quoted.e412.in. 300 IN TXT "say \"hi\""`}},
				{qname: "long.e412.in", qtype: dns.TypeTXT, want: []string{
					`long.e412.in. 300 IN TXT "` + longTXT[:255] + `" "` + longTXT[255:] + `"`,
				}},
				{qname: "note.e412.in", qtype: dns.TypeTXT, want: []string{`note.e412.in. 300 IN TXT "hello"`}},
			},
		},
		// RFC 2181 §8 permits it and it means "never cache this". A file
		// carries a $TTL default, so a TTL that goes missing on the way out
		// silently becomes 900 on the way back in — a record that was never
		// meant to be cached, cached.
		{
			what: "an explicit TTL of 0 survives",
			zone: "e412.in",
			seeds: []roundTripSeed{
				{name: "zero", recType: "A", ttl: 0, rdata: "1.2.3.4"},
			},
			asks: []roundTripAsk{
				{qname: "zero.e412.in", qtype: dns.TypeA, want: []string{"zero.e412.in. 0 IN A 1.2.3.4"}},
			},
		},
		// An rdata that is entirely a comment carries no value at all. For a
		// TXT it is not a parse error — dns.NewRR reads it as a TXT whose
		// rdata is simply absent — so the write is refused on the rdata the
		// parser produced being empty, not on an error. Stored, it would
		// render as a line with nothing after the type, which no parser reads
		// back: a zone that exports to a file it cannot reimport.
		{
			what: "rdata that is only a comment never reaches the zone",
			zone: "e412.in",
			seeds: []roundTripSeed{
				{name: "note", recType: "TXT", ttl: 300, rdata: "; just a note", status: http.StatusBadRequest},
				{name: "host", recType: "A", ttl: 300, rdata: "; just a note", status: http.StatusBadRequest},
			},
			asks: []roundTripAsk{
				{qname: "note.e412.in", qtype: dns.TypeTXT, rcode: dns.RcodeNameError},
				{qname: "host.e412.in", qtype: dns.TypeA, rcode: dns.RcodeNameError},
			},
		},
	}

	for _, c := range cases {
		t.Run(c.what, func(t *testing.T) {
			srv := newTestServer(t)
			zid := srv.createZone(t, c.zone)
			res := srv.resolver(t)

			seedRecords(t, srv, zid, c.seeds)
			assertRoundTripPreservesAnswers(t, srv, res, zid, c.asks)
		})
	}
}
