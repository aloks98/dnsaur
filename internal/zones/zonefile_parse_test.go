package zones_test

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aloks98/dnsaur/internal/store"
	"github.com/aloks98/dnsaur/internal/zones"
	"github.com/miekg/dns"
)

func TestParseRelativizesNames(t *testing.T) {
	const f = `$ORIGIN e412.in.
$TTL 3600
@   IN SOA ns.e412.in. hostadmin.e412.in. ( 7 900 300 604800 900 )
@   IN NS  ns.e412.in.
bifrost 300 IN A 57.129.69.158
*.nexus 300 IN A 192.168.160.200
`
	pz, errs := zones.Parse(f, "e412.in")
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if pz.SOA == nil || pz.SOA.Serial != 7 {
		t.Fatalf("SOA = %+v; want serial 7", pz.SOA)
	}
	got := map[string]string{}
	for _, r := range pz.Records {
		got[r.Name+" "+r.Type] = r.RData
	}
	// Names come back RELATIVE to the apex, matching how they are stored.
	if got["bifrost A"] != "57.129.69.158" || got["*.nexus A"] != "192.168.160.200" || got["@ NS"] == "" {
		t.Fatalf("records = %v; want relative names", got)
	}
	// The SOA is returned separately, not as a record.
	for _, r := range pz.Records {
		if r.Type == "SOA" {
			t.Error("SOA leaked into Records; it belongs on the zone row")
		}
	}
}

// $TTL supplies the TTL for records that omit one (RFC 2308 §4).
func TestParseAppliesDollarTTL(t *testing.T) {
	const f = `$ORIGIN e412.in.
$TTL 1234
@ IN SOA ns.e412.in. hostadmin.e412.in. ( 1 900 300 604800 900 )
noTTL IN A 1.2.3.4
`
	pz, errs := zones.Parse(f, "e412.in")
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	for _, r := range pz.Records {
		if r.Name == "notttl" || r.Name == "noTTL" {
			if r.TTL != 1234 {
				t.Errorf("TTL = %d, want 1234 from $TTL", r.TTL)
			}
		}
	}
}

// Every bad line is reported, not just the first — Task 4 shows them all.
func TestParseReportsEveryBadLine(t *testing.T) {
	const f = `$ORIGIN e412.in.
$TTL 300
@ IN SOA ns.e412.in. hostadmin.e412.in. ( 1 900 300 604800 900 )
good IN A 1.2.3.4
bad1 IN A not-an-ip
bad2 IN MX Mx
`
	_, errs := zones.Parse(f, "e412.in")
	if len(errs) < 2 {
		t.Fatalf("errs = %v; want both bad lines reported", errs)
	}
}

// A file whose records are absolute under a different apex is a user error
// worth naming, not something to silently accept.
func TestParseRejectsOutOfZoneNames(t *testing.T) {
	const f = `$ORIGIN e412.in.
$TTL 300
@ IN SOA ns.e412.in. hostadmin.e412.in. ( 1 900 300 604800 900 )
stranger.example.com. 300 IN A 1.2.3.4
`
	_, errs := zones.Parse(f, "e412.in")
	if len(errs) == 0 {
		t.Fatal("an out-of-zone name was accepted")
	}
}

// A mid-file $ORIGIN directive applies to every record after it, not just
// the one that immediately follows — RFC 1035 §5.1 has it persist until
// the next $ORIGIN or EOF.
func TestParseOriginPersistsAcrossRecords(t *testing.T) {
	const f = `$ORIGIN e412.in.
$TTL 300
@ IN SOA ns.e412.in. hostadmin.e412.in. ( 1 900 300 604800 900 )
$ORIGIN dev.e412.in.
w IN A 1.1.1.1
x IN A 2.2.2.2
y IN A 3.3.3.3
`
	pz, errs := zones.Parse(f, "e412.in")
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	got := map[string]string{}
	for _, r := range pz.Records {
		got[r.Name] = r.RData
	}
	want := map[string]string{"w.dev": "1.1.1.1", "x.dev": "2.2.2.2", "y.dev": "3.3.3.3"}
	for name, rdata := range want {
		if got[name] != rdata {
			t.Errorf("record %q = %q, want %q — $ORIGIN dev.e412.in. must still be in effect, not just for the record right after it (records: %v)", name, got[name], rdata, got)
		}
	}
}

// A record's reported Line must be its own source line even when a
// directive replayed ahead of it (for $ORIGIN/$TTL state — see
// splitStatements) is not physically adjacent: blank lines and comments
// sit between them in the original file.
func TestParseLineNumberSkipsBlankAndCommentLines(t *testing.T) {
	const f = "$ORIGIN e412.in.\n" + // line 1
		"$TTL 300\n" + // line 2
		"\n" + // line 3 (blank)
		"; a comment between the directives and the record\n" + // line 4
		"@ IN SOA ns.e412.in. hostadmin.e412.in. ( notanum 900 300 604800 900 )\n" // line 5, malformed
	_, errs := zones.Parse(f, "e412.in")
	if len(errs) != 1 {
		t.Fatalf("errs = %v; want exactly 1", errs)
	}
	if !strings.HasPrefix(errs[0], "line 5:") {
		t.Fatalf("errs[0] = %q; want it to name line 5 (the actual bad record) — a naive offset from the folded directive lines undercounts once blank/comment lines are skipped", errs[0])
	}
}

// A backslash escapes the next character everywhere, not just inside a
// quoted string (RFC 1035 §5.1) — a literal '(' after one must not be
// counted as opening a parenthesised group, or the splitter folds every
// line after it into one statement, and one statement is one parser: the
// first bad line in the swallowed span silences every bad line after it,
// so the file's second problem is never named.
func TestParseParenDeltaRespectsBackslashEscapes(t *testing.T) {
	const f = `$ORIGIN e412.in.
$TTL 300
@ IN SOA ns.e412.in. hostadmin.e412.in. ( 1 900 300 604800 900 )
weird\(escaped IN A 1.2.3.4
bad1 IN A not-an-ip
bad2 IN MX Mx
`
	_, errs := zones.Parse(f, "e412.in")
	assertErrorLines(t, errs, 5, 6)
}

// RFC 2308 §4 makes $TTL the default TTL for a record that omits one, and
// a file that has neither has named no TTL at all. dns.ZoneParser supplies
// no default and reports no error for that — it hands back Ttl 0, a value
// the file never stated, for the SOA and (by carry-forward) every record
// after it. Stored, that is worse than a silent normalisation: a zone
// whose SOA TTL is 0 answers every NXDOMAIN with a TTL of
// min(SOAMinimum, SOATTL) = 0 (RFC 2308 §5, answer.go), so nothing this
// zone denies is ever negatively cached and every miss re-queries us
// forever. Milestone A made soa_ttl non-client-settable precisely so no
// hand write could produce it (see defaultSOATTL in the api package); a
// zone file must not be the door that lets it back in.
func TestParseRejectsSOAWithNoTTL(t *testing.T) {
	const f = `$ORIGIN e412.in.
@ IN SOA ns.e412.in. hostadmin.e412.in. ( 1 900 300 604800 900 )
@ IN NS ns.e412.in.
bifrost IN A 57.129.69.158
`
	_, errs := zones.Parse(f, "e412.in")
	if len(errs) != 1 {
		t.Fatalf("errs = %v; want exactly one, naming the missing TTL", errs)
	}
	if !strings.Contains(errs[0], "$TTL") || !strings.Contains(errs[0], "SOA") {
		t.Errorf("errs = %v; want the message to name both the SOA and $TTL", errs)
	}
}

// An explicit TTL of 0 is a legal RR that says "do not cache me", and
// POST /zones/{id}/records accepts one. Import must not be stricter than a
// hand write, or a record created by hand stops surviving an export and
// re-import — the round trip the whole of spec §8 rests on. Only the SOA's
// own TTL is refused, and only because a zero there breaks negative
// caching for the entire zone rather than for one record.
func TestParseKeepsAnExplicitZeroTTLOnARecord(t *testing.T) {
	const f = `$ORIGIN e412.in.
$TTL 300
@ IN SOA ns.e412.in. hostadmin.e412.in. ( 1 900 300 604800 900 )
uncached 0 IN A 57.129.69.158
`
	pz, errs := zones.Parse(f, "e412.in")
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(pz.Records) != 1 || pz.Records[0].TTL != 0 {
		t.Fatalf("records = %+v; want one record with TTL 0 kept as written", pz.Records)
	}
}

// A double-quoted character-string may span physical lines inside a
// parenthesised group (verified against dns.ZoneParser directly), so the
// splitter's "is this paren quoted?" state has to carry from one line to
// the next. Reset per line, it desynchronises from miekg: the ')' closing
// record `b` on line 9 sits inside a quote that opened on line 6, and a
// per-line reset reads line 6's quoted '(' as a real one and line 9's
// unquoted... — the net effect being that `b`'s statement is judged to
// start on line 9 rather than 6.
//
// The damage is a *wrong* line, not a missing one: the two spurious ')'
// lines inside b's own rdata split the run back apart, so the statement
// count still matches the RR count and parseClean's self-check (Parse's
// "Line numbers on an accepted file") sees nothing to distrust. An import
// rejecting `b` would then point the user at an innocent line.
func TestParseKeepsQuoteStateAcrossLinesInsideParens(t *testing.T) {
	const f = `$ORIGIN e412.in.
$TTL 300
@ 300 IN SOA ns.e412.in. hostadmin.e412.in. ( 1 900 300 604800 900 )
a 300 IN TXT ( "one ( two
three" )
b 300 IN TXT ( "four
) five
) six
seven" )
c 300 IN A 1.2.3.4
`
	pz, errs := zones.Parse(f, "e412.in")
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	got := map[string]int{}
	for _, r := range pz.Records {
		got[r.Name] = r.Line
	}
	want := map[string]int{"a": 4, "b": 6, "c": 10}
	for name, line := range want {
		if got[name] != line {
			t.Errorf("%s is on line %d, Parse says %d", name, line, got[name])
		}
	}
}

// The same desync on the reject path costs error messages rather than
// accuracy: a quoted '(' that the splitter miscounts leaves the
// parenthesis depth permanently positive, so every remaining line is
// folded into one statement — and one statement is one dns.ZoneParser,
// which latches its first error and never reports the second. Both bad
// lines here must be named.
func TestParseQuoteStateDesyncDoesNotSwallowLaterBadLines(t *testing.T) {
	const f = `$ORIGIN e412.in.
$TTL 300
@ 300 IN SOA ns.e412.in. hostadmin.e412.in. ( 1 900 300 604800 900 )
a 300 IN TXT ( "one ( two
three" )
bad1 IN A not-an-ip
bad2 IN MX Mx
`
	_, errs := zones.Parse(f, "e412.in")
	assertErrorLines(t, errs, 6, 7)
}

// $TTL is never replayed into a recovered statement, but it is still
// validated, and that check is the only thing that reports a malformed
// $TTL line when the file has a second problem too.
//
// TestParseBadDirectiveReportedOnce cannot pin this. Its file's only error
// is the bad directive, so recovery returning nothing at all still yields
// the right message — Parse falls back to parseClean's, which names the
// same line ("Recovery is a fallback, not a second opinion"). That
// fallback is exactly what hides the check being gone. Here the bad record
// on line 6 gives recovery an error of its own, so the fallback never
// runs, and line 4 is reported only if the $TTL line was checked.
func TestParseReportsAMalformedDollarTTLAlongsideOtherErrors(t *testing.T) {
	const f = `$ORIGIN e412.in.
$TTL 300
@ 900 IN SOA ns.e412.in. hostadmin.e412.in. ( 1 900 300 604800 900 )
$TTL banana
a 100 IN A 1.1.1.1
bad IN A not-an-ip
`
	_, errs := zones.Parse(f, "e412.in")
	assertErrorLines(t, errs, 4, 6)
}

// assertErrorLines checks that errs names exactly the given lines, in
// order — the whole of what Parse promises about a file it rejects.
func assertErrorLines(t *testing.T, errs []string, want ...int) {
	t.Helper()
	var got []int
	for _, e := range errs {
		var n int
		if _, err := fmt.Sscanf(e, "line %d:", &n); err != nil {
			t.Errorf("error %q does not start with a line number", e)
			continue
		}
		got = append(got, n)
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("errors named lines %v, want %v: %v", got, want, errs)
	}
}

// $TTL sets the default TTL for records that omit one — it is not "inherit
// the last TTL any record happened to carry". A record with its own
// explicit TTL must not change what later omitted-TTL records fall back
// to (RFC 2308 §4).
func TestParseTTLDirectiveIsTheDefaultNotLastExplicitTTL(t *testing.T) {
	const f = `$ORIGIN e412.in.
$TTL 300
@ IN SOA ns.e412.in. hostadmin.e412.in. ( 1 900 300 604800 900 )
a 7200 IN A 1.1.1.1
b IN A 2.2.2.2
`
	pz, errs := zones.Parse(f, "e412.in")
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	got := map[string]uint32{}
	for _, r := range pz.Records {
		got[r.Name] = r.TTL
	}
	if got["a"] != 7200 {
		t.Errorf("a's TTL = %d, want 7200 (its own explicit TTL)", got["a"])
	}
	if got["b"] != 300 {
		t.Errorf("b's TTL = %d, want 300 from $TTL — got a's explicit 7200 instead, meaning the default is tracking \"last TTL seen\" rather than the $TTL directive", got["b"])
	}
}

// Three bad records in a row, with good ones interleaved, must all be
// reported — not just the first — and each on its own line.
func TestParseThreeBadLinesAllReported(t *testing.T) {
	const f = `$ORIGIN e412.in.
$TTL 300
@ IN SOA ns.e412.in. hostadmin.e412.in. ( 1 900 300 604800 900 )
bad1 IN A not-an-ip
good1 IN A 1.2.3.4
bad2 IN MX Mx
bad3 IN AAAA nope
good2 IN A 5.6.7.8
`
	_, errs := zones.Parse(f, "e412.in")
	assertErrorLines(t, errs, 4, 6, 7)
}

// A directive's effect must still reach the statements after it even when
// the statement in between fails to parse — the directive line itself was
// syntactically fine and is replayed regardless of what came after it in
// the original file. $ORIGIN is the directive this is observable through:
// where it points decides whether a later name is in this zone at all, so
// dropping the replay turns the out-of-zone line below into a line Parse
// silently accepts.
func TestParseDirectiveSurvivesErrorInPrecedingStatement(t *testing.T) {
	const f = `$ORIGIN e412.in.
$TTL 300
@ IN SOA ns.e412.in. hostadmin.e412.in. ( notanum 900 300 604800 900 )
$ORIGIN example.com.
stranger IN A 1.2.3.4
`
	_, errs := zones.Parse(f, "e412.in")
	assertErrorLines(t, errs, 3, 5)
	if !strings.Contains(errs[1], "outside zone") {
		t.Errorf("errs[1] = %q; want the out-of-zone name named — $ORIGIN example.com. must still be in effect on line 5", errs[1])
	}
}

// The file Render emits must parse back into the records it was built
// from — the only round trip that actually matters for zone file export
// and import to agree with each other.
func TestParseRenderRoundTrip(t *testing.T) {
	recs := []store.ZoneRecord{
		{Name: "@", Type: "NS", TTL: 3600, RData: "ns.e412.in.", Enabled: true},
		{Name: "bifrost", Type: "A", TTL: 300, RData: "57.129.69.158", Enabled: true},
		{Name: "@", Type: "MX", TTL: 3600, RData: "10 mail.e412.in.", Enabled: true},
		{Name: "@", Type: "SRV", TTL: 3600, RData: "10 20 5223 im.e412.in.", Enabled: true},
		{Name: "@", Type: "TXT", TTL: 3600, RData: `"he said \"hi\""`, Enabled: true},
		{Name: "*.nexus", Type: "A", TTL: 300, RData: "192.168.160.200", Enabled: true},
	}
	out := zones.Render(testZone(), recs)
	pz, errs := zones.Parse(out, "e412.in")
	if len(errs) != 0 {
		t.Fatalf("errs = %v; rendered file must parse:\n%s", errs, out)
	}
	if pz.SOA == nil || pz.SOA.Serial != 7 {
		t.Fatalf("SOA = %+v", pz.SOA)
	}
	if len(pz.Records) != len(recs) {
		t.Fatalf("got %d records, want %d:\n%s", len(pz.Records), len(recs), out)
	}

	// TXT fidelity has to be checked on the wire, not dns.TXT.Txt: miekg
	// keeps presentation escapes in .Txt and only decodes them in
	// packTxtString, so a naive comparison of presentation strings would
	// pass even if the wire bytes differed. Same technique as
	// internal/store/zonemigrate_test.go's txtWireStrings.
	var origTXT, gotTXT string
	for _, r := range recs {
		if r.Type == "TXT" {
			origTXT = r.RData
		}
	}
	for _, r := range pz.Records {
		if r.Type == "TXT" {
			gotTXT = r.RData
		}
	}
	origWire := wireCharacterStrings(t, origTXT)
	gotWire := wireCharacterStrings(t, gotTXT)
	if fmt.Sprint(origWire) != fmt.Sprint(gotWire) {
		t.Fatalf("TXT wire bytes = %v, want %v (presentation forms: got %q, orig %q)", gotWire, origWire, gotTXT, origTXT)
	}
}

// wireCharacterStrings returns the character-strings rdata actually packs
// to on the wire, by packing and re-reading the raw bytes rather than
// trusting dns.TXT.Txt — see TestParseRenderRoundTrip's doc comment.
func wireCharacterStrings(t *testing.T, rdata string) []string {
	t.Helper()
	rr, err := dns.NewRR(fmt.Sprintf("x.example. 300 IN TXT %s", rdata))
	if err != nil {
		t.Fatalf("rdata %q does not parse: %v", rdata, err)
	}
	buf := make([]byte, 65535)
	end, err := dns.PackRR(rr, buf, 0, nil, false)
	if err != nil {
		t.Fatalf("rdata %q does not pack: %v", rdata, err)
	}
	_, off, err := dns.UnpackDomainName(buf[:end], 0)
	if err != nil {
		t.Fatalf("unpack owner name: %v", err)
	}
	off += 2 + 2 + 4 // type, class, ttl
	rdlen := int(buf[off])<<8 | int(buf[off+1])
	off += 2
	var out []string
	rdataEnd := off + rdlen
	for off < rdataEnd {
		l := int(buf[off])
		off++
		out = append(out, string(buf[off:off+l]))
		off += l
	}
	return out
}

// A file with CRLF line endings — one edited on Windows, downloaded
// through a web UI, or pasted through some tooling — must parse
// identically to the same content with LF endings, including line
// numbers: import is exactly where a foreign file's line endings show up,
// and nothing but this test stops a future change to the line-splitting
// in splitStatements from breaking it silently.
func TestParseHandlesCRLFLineEndings(t *testing.T) {
	f := strings.Join([]string{
		"$ORIGIN e412.in.",
		"$TTL 300",
		"@ IN SOA ns.e412.in. hostadmin.e412.in. ( 1 900 300 604800 900 )",
		"bifrost 300 IN A 57.129.69.158",
		"*.nexus 300 IN A 192.168.160.200",
		"",
	}, "\r\n")
	pz, errs := zones.Parse(f, "e412.in")
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if pz.SOA == nil || pz.SOA.Serial != 1 {
		t.Fatalf("SOA = %+v", pz.SOA)
	}
	got := map[string]zones.ParsedRecord{}
	for _, r := range pz.Records {
		got[r.Name] = r
	}
	if r, ok := got["bifrost"]; !ok || r.RData != "57.129.69.158" || r.Line != 4 {
		t.Errorf("bifrost = %+v, ok=%v; want RData 57.129.69.158 on line 4", r, ok)
	}
	if r, ok := got["*.nexus"]; !ok || r.RData != "192.168.160.200" || r.Line != 5 {
		t.Errorf("*.nexus = %+v, ok=%v; want RData 192.168.160.200 on line 5", r, ok)
	}
}

// CRLF must not skew a reported error line either — the per-statement
// recovery pass does its own line splitting, and both bad lines here sit
// past the point where a miscount would start showing.
func TestParseHandlesCRLFLineEndingsInRecovery(t *testing.T) {
	f := strings.Join([]string{
		"$ORIGIN e412.in.",
		"$TTL 300",
		"@ IN SOA ns.e412.in. hostadmin.e412.in. ( 1 900 300 604800 900 )",
		"bad1 IN A not-an-ip",
		"bifrost 300 IN A 57.129.69.158",
		"bad2 IN MX Mx",
		"",
	}, "\r\n")
	_, errs := zones.Parse(f, "e412.in")
	assertErrorLines(t, errs, 4, 6)
}

// NEW-1: a line starting with whitespace inherits the previous RR's owner
// (RFC 1035 §5.1) — the shape BIND, Knot and nsd all emit for a second (or
// third, ...) RR at the same name. A clean file takes Parse's happy path,
// a single continuous dns.ZoneParser, which carries the owner forward
// itself; nothing here reimplements it.
func TestParseKeepsBlankOwnerContinuationLines(t *testing.T) {
	const f = `$ORIGIN e412.in.
$TTL 300
@ IN SOA ns.e412.in. hostadmin.e412.in. ( 1 900 300 604800 900 )
@   IN NS ns1.e412.in.
    IN NS ns2.e412.in.
`
	pz, errs := zones.Parse(f, "e412.in")
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	got := map[string]bool{}
	for _, r := range pz.Records {
		if r.Type == "NS" {
			got[r.RData] = true
		}
	}
	if !got["ns1.e412.in."] || !got["ns2.e412.in."] {
		t.Fatalf("NS records = %v; want both ns1.e412.in. and ns2.e412.in. — the blank-owner continuation line must not be dropped", got)
	}
}

// NEW-2: a $GENERATE as the file's last statement must still expand — it
// produces its RRs itself, with nothing needing to follow it.
func TestParseGenerateAtEndOfFileIsExpanded(t *testing.T) {
	const f = `$ORIGIN e412.in.
$TTL 300
@ IN SOA ns.e412.in. hostadmin.e412.in. ( 1 900 300 604800 900 )
$GENERATE 1-3 host$ IN A 10.0.0.$
`
	pz, errs := zones.Parse(f, "e412.in")
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(pz.Records) != 3 {
		t.Fatalf("got %d records, want 3 from $GENERATE 1-3: %+v", len(pz.Records), pz.Records)
	}
	// One statement, three records: the statement count and the RR count
	// legitimately disagree, so line attribution stands down and every
	// record comes back with Line 0 rather than three records all claiming
	// line 4. The records themselves are still complete and correct — see
	// Parse's "Line numbers on an accepted file".
	for _, r := range pz.Records {
		if r.Line != 0 {
			t.Errorf("record %+v has Line %d; want 0 — $GENERATE expands one line into N records, so no record is on a line of its own", r, r.Line)
		}
	}
	got := map[string]string{}
	for _, r := range pz.Records {
		got[r.Name] = r.RData
	}
	want := map[string]string{"host1": "10.0.0.1", "host2": "10.0.0.2", "host3": "10.0.0.3"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("records = %v, want %v", got, want)
	}
}

// NEW-2: a $INCLUDE as the file's last statement must not be silently
// dropped — dns.ZoneParser disallows $INCLUDE unless SetIncludeAllowed(true)
// is called, which Parse never does, so this must be reported, not ignored.
func TestParseIncludeAtEndOfFileIsRejected(t *testing.T) {
	const f = `$ORIGIN e412.in.
$TTL 300
@ IN SOA ns.e412.in. hostadmin.e412.in. ( 1 900 300 604800 900 )
$INCLUDE /etc/passwd
`
	_, errs := zones.Parse(f, "e412.in")
	if len(errs) == 0 {
		t.Fatal("a trailing $INCLUDE directive was silently accepted")
	}
}

// NEW-3: a $INCLUDE directive must not take the statement after it down
// with it — each gets its own statement and its own parser, so a second
// problem on the line after $INCLUDE is still named rather than hidden
// behind $INCLUDE's own failure.
func TestParseIncludeDoesNotSwallowTheStatementAfterIt(t *testing.T) {
	const f = `$ORIGIN e412.in.
$TTL 300
@ IN SOA ns.e412.in. hostadmin.e412.in. ( 1 900 300 604800 900 )
$INCLUDE /etc/passwd
bad IN A not-an-ip
`
	_, errs := zones.Parse(f, "e412.in")
	assertErrorLines(t, errs, 4, 5)
}

// NEW-4: a broken $ORIGIN/$TTL directive must be reported once, not once
// per record after it — replaying a directive that is already known to be
// bad into every later statement would fail every one of them with a copy
// of the same root cause.
func TestParseBadDirectiveReportedOnce(t *testing.T) {
	const f = `$ORIGIN e412.in.
$TTL banana
@ 900 IN SOA ns.e412.in. hostadmin.e412.in. ( 1 900 300 604800 900 )
a 100 IN A 1.1.1.1
b 200 IN A 2.2.2.2
c 300 IN A 3.3.3.3
`
	_, errs := zones.Parse(f, "e412.in")
	assertErrorLines(t, errs, 2)
}

// NEW-5: a field truncated by true EOF must be reported the same way the
// same truncation followed by a newline would be — not silently defaulted.
func TestParseRejectsSOAMissingField(t *testing.T) {
	const f = `$ORIGIN e412.in.
$TTL 300
@ IN SOA ns.e412.in. hostadmin.e412.in. ( 1 900 300 604800 )
`
	_, errs := zones.Parse(f, "e412.in")
	if len(errs) == 0 {
		t.Fatal("an SOA missing its minimum field was silently accepted with minimum defaulted to 0")
	}
}

// NEW-5: an SOA anywhere but the zone apex must not be silently adopted as
// the zone's SOA — a zone's SOA belongs at "@" by definition.
func TestParseRejectsNonApexSOA(t *testing.T) {
	const f = `$ORIGIN e412.in.
$TTL 300
notapex IN SOA ns.e412.in. hostadmin.e412.in. ( 1 900 300 604800 900 )
`
	pz, errs := zones.Parse(f, "e412.in")
	if len(errs) == 0 {
		t.Fatal("an SOA at a non-apex name was silently accepted")
	}
	if pz.SOA != nil {
		t.Errorf("SOA = %+v; want nil — a non-apex SOA must not be adopted as the zone's", pz.SOA)
	}
}

// Round 3, Finding 1 (structural): recovery re-parsing a file statement by
// statement can find nothing wrong even though the whole-file parse
// genuinely rejected it — the reviewer's own adversarial input, confirmed
// directly against dns.NewZoneParser before writing the fix: a
// parenthesised SOA followed by a line starting '\(' fails whole-file
// ("no blank after owner") purely because of lexer state carried across
// the statement boundary, but parses cleanly once isolated into its own
// statement with no preceding context — recovery's fresh-parser-per-record
// design can never see that context (Parse's doc comment). Recovery
// finding zero errors must not turn into Parse returning zero errors for
// a file dns.ZoneParser itself refuses.
func TestParseNeverSilentlyAcceptsAFileMiekgRejects(t *testing.T) {
	const f = `$ORIGIN e412.in.
$TTL 300
@ IN SOA ns.e412.in. hostadmin.e412.in. (
	1
	900
	300
	604800
	900 )
\( IN A 1.2.3.4
`
	_, errs := zones.Parse(f, "e412.in")
	if len(errs) == 0 {
		t.Fatal(`miekg rejects this file ("no blank after owner") but Parse accepted it`)
	}
}

// Round 3, Finding 2: recovery must not report a line once per record
// affected by it — built at the scale the review used to find this: 25
// owners, each followed by one blank-owner continuation line, and a
// single bad continuation buried among them (at line 29, matching the
// review's own numbers). Before this was fixed, every one of the 25
// continuations came back as its own "owner name is blank" error, since
// recovery gave every continuation line its own parser with no owner to
// inherit, burying the one real error among 24 identical false ones.
//
// Round 4 retired the fix rather than kept it: recovery no longer returns
// records, so an owner it cannot resolve costs nothing and is skipped in
// silence instead of being reconstructed. The behaviour this test pins is
// unchanged — one bad line, one error, on the right line.
func TestParseHandlesManyBlankOwnerContinuationsWithOneBadLine(t *testing.T) {
	lines := []string{
		"$ORIGIN e412.in.",
		"$TTL 300",
		"@ IN SOA ns.e412.in. hostadmin.e412.in. ( 1 900 300 604800 900 )",
	}
	const badAtOwner = 13
	var badLine int
	for i := 1; i <= 25; i++ {
		lines = append(lines, fmt.Sprintf("host%d IN A 10.0.0.%d", i, i))
		if i == badAtOwner {
			badLine = len(lines) + 1
			lines = append(lines, "    IN A not-an-ip")
		} else {
			lines = append(lines, fmt.Sprintf("    IN A 10.1.0.%d", i))
		}
	}
	f := strings.Join(lines, "\n") + "\n"

	_, errs := zones.Parse(f, "e412.in")
	assertErrorLines(t, errs, badLine)
}

// Round 3, Finding 4: a second SOA at the zone apex must not silently win
// over the first — same class of bug as a non-apex SOA being silently
// adopted, just with both SOAs actually at "@".
func TestParseRejectsDuplicateApexSOA(t *testing.T) {
	const f = `$ORIGIN e412.in.
$TTL 300
@ IN SOA ns.e412.in. hostadmin.e412.in. ( 1 900 300 604800 900 )
@ IN SOA ns2.e412.in. hostadmin.e412.in. ( 2 900 300 604800 900 )
`
	_, errs := zones.Parse(f, "e412.in")
	if len(errs) == 0 {
		t.Fatal("a second SOA at the zone apex was silently accepted, with the last one winning")
	}
}

// Round 3, Finding 5: a zone file needs exactly one SOA at its apex (RFC
// 1034 §4.1.1) — none at all is rejected here rather than handed to a
// caller as a ParsedZone with a nil SOA and no error to explain it (see
// ParsedZone's doc comment).
func TestParseRejectsFileWithNoSOA(t *testing.T) {
	cases := []struct {
		name string
		file string
	}{
		{"empty", ""},
		{"comments only", "; nothing here\n; still nothing\n"},
		{"origin and ttl only", "$ORIGIN e412.in.\n$TTL 300\n"},
		{"records but no soa", "$ORIGIN e412.in.\n$TTL 300\nbifrost IN A 1.2.3.4\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pz, errs := zones.Parse(tc.file, "e412.in")
			if len(errs) == 0 {
				t.Fatalf("a file with no SOA was silently accepted: SOA=%v records=%+v", pz.SOA, pz.Records)
			}
		})
	}
}

// Round 3, Finding 6: pz.SOA.Hdr.Name must use the same lowercase
// convention every ParsedRecord.Name already gets from RelName — a file
// whose $ORIGIN happens to use a different case must not leave the SOA's
// own name as the odd one out.
func TestParseNormalizesSOANameCase(t *testing.T) {
	const f = `$ORIGIN E412.IN.
$TTL 300
@ IN SOA ns.e412.in. hostadmin.e412.in. ( 1 900 300 604800 900 )
`
	pz, errs := zones.Parse(f, "e412.in")
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if pz.SOA == nil {
		t.Fatal("no SOA")
	}
	if pz.SOA.Hdr.Name != "e412.in." {
		t.Errorf("SOA.Hdr.Name = %q, want %q", pz.SOA.Hdr.Name, "e412.in.")
	}
}

// Round 4, Change 2: a file Parse accepts still needs real line numbers on
// its records. Nothing here is wrong, so recovery never runs and there is
// no error list to carry lines — ParsedRecord.Line is the only thing an
// importer can name a line with, and it has to be right through a
// parenthesised multi-line record, a blank-owner continuation line, a
// quoted string carrying an unbalanced '(' and a ';' that is not a
// comment, blank lines and comments — none of which line up with a naive
// record counter, and every one of which the statement splitter has to
// agree with dns.ZoneParser about or hand back no line numbers at all.
func TestParseCleanFileRecordsCarryLineNumbers(t *testing.T) {
	const f = `$ORIGIN e412.in.
$TTL 300

; the apex
@ IN SOA ns.e412.in. hostadmin.e412.in. (
    7 900 300 604800 900 )
@ IN NS ns1.e412.in.
    IN NS ns2.e412.in.
@ IN TXT "unbalanced ( and ; here"

bifrost 300 IN A 57.129.69.158
*.nexus 300 IN A 192.168.160.200
`
	pz, errs := zones.Parse(f, "e412.in")
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	got := map[string]int{}
	for _, r := range pz.Records {
		got[r.Name+" "+r.RData] = r.Line
	}
	want := map[string]int{
		"@ ns1.e412.in.":              7,
		"@ ns2.e412.in.":              8,
		`@ "unbalanced ( and ; here"`: 9,
		"bifrost 57.129.69.158":       11,
		"*.nexus 192.168.160.200":     12,
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("record lines = %v, want %v", got, want)
	}
}

// Round 4, Change 2: the case the line numbers actually exist for. Every
// line here is syntactically valid — Parse returns no errors at all — and
// every one of them is rejected by the record validator above Parse: two
// TTLs in one RRSet (RFC 2181 §5.2) and a CNAME beside another type (RFC
// 1034 §3.6.2). That validator can only name the offending lines if the
// records it is handed carry them.
func TestParseLinesOnASyntacticallyCleanButUnimportableFile(t *testing.T) {
	const f = `$ORIGIN e412.in.
$TTL 300
@ IN SOA ns.e412.in. hostadmin.e412.in. ( 1 900 300 604800 900 )
web 300 IN A 1.1.1.1
web 600 IN A 2.2.2.2
alias 300 IN CNAME target.e412.in.
alias 300 IN A 3.3.3.3
`
	pz, errs := zones.Parse(f, "e412.in")
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v — every line of this file is syntactically valid", errs)
	}
	var lines []int
	for _, r := range pz.Records {
		lines = append(lines, r.Line)
	}
	if fmt.Sprint(lines) != fmt.Sprint([]int{4, 5, 6, 7}) {
		t.Fatalf("record lines = %v, want [4 5 6 7]: %+v", lines, pz.Records)
	}
}

// Round 4, defect 1: an owner token is relative to the origin in effect
// where it was written. Recovery used to remember the token and replay it
// under whatever $ORIGIN it had reached by the time a later continuation
// line needed one, which resolved "host" against "dev.e412.in." and
// invented a record at host.dev that is in neither the file nor
// dns.ZoneParser's reading of it. Records come from the whole-file parse
// now, so the owner is never re-derived at all.
func TestParseDoesNotReattributeOwnersUnderALaterOrigin(t *testing.T) {
	const f = `$ORIGIN e412.in.
$TTL 300
@ IN SOA ns.e412.in. hostadmin.e412.in. ( 1 900 300 604800 900 )
host IN A 1.1.1.1
$ORIGIN dev.e412.in.
	IN A 2.2.2.2
bad IN A not-an-ip
`
	pz, errs := zones.Parse(f, "e412.in")
	assertErrorLines(t, errs, 7)
	var names []string
	for _, r := range pz.Records {
		names = append(names, r.Name)
	}
	if fmt.Sprint(names) != fmt.Sprint([]string{"host", "host"}) {
		t.Fatalf("record names = %v, want [host host] — the continuation line inherits the owner already resolved for line 4, not the token \"host\" re-resolved under $ORIGIN dev.e412.in.: %+v", names, pz.Records)
	}
}

// Round 4, defect 2: a backslash escapes the following character anywhere
// in a master file (RFC 1035 §5.1), so `foo\ bar` is one owner containing
// a space. Recovery used to pick the owner off the line with
// strings.Fields, which is blind to that and cut it to `foo\` — the same
// root cause as the escaped-paren bug parenDelta was fixed for. Nothing
// picks an owner off a line any more.
func TestParseDoesNotSplitOwnersOnEscapedSpaces(t *testing.T) {
	const f = `$ORIGIN e412.in.
$TTL 300
@ IN SOA ns.e412.in. hostadmin.e412.in. ( 1 900 300 604800 900 )
foo\ bar IN A 1.1.1.1
	IN A 2.2.2.2
bad IN A not-an-ip
`
	pz, errs := zones.Parse(f, "e412.in")
	assertErrorLines(t, errs, 6)
	var names []string
	for _, r := range pz.Records {
		names = append(names, r.Name)
	}
	if fmt.Sprint(names) != fmt.Sprint([]string{`foo\ bar`, `foo\ bar`}) {
		t.Fatalf(`record names = %v, want [foo\ bar foo\ bar] — the escaped space is part of the owner, not a field separator: %+v`, names, pz.Records)
	}
}

// Round 4, defect 3: an unterminated '(' runs to the end of the file, and
// the line Parse names for it must be a line the file actually has.
// strings.Split leaves an empty element after the file's final newline;
// counting it as a line let the paren span absorb it and report the error
// one line past EOF — line 6 of a 5-line file, where dns.ZoneParser itself
// says line 5.
func TestParseErrorLineIsNeverPastEndOfFile(t *testing.T) {
	const f = `$ORIGIN e412.in.
$TTL 300
@ IN SOA ns.e412.in. hostadmin.e412.in. ( 1 900 300 604800 900 )
a IN A (
1.1.1.1
`
	if n := len(strings.Split(strings.TrimSuffix(f, "\n"), "\n")); n != 5 {
		t.Fatalf("fixture is %d lines, expected 5", n)
	}
	_, errs := zones.Parse(f, "e412.in")
	assertErrorLines(t, errs, 5)
}

// A table of inputs run through both Parse and a raw dns.ZoneParser,
// checked for the same accept/reject verdict; when both accept, the same
// records — name, type, TTL and rdata, all four, not just name/type/rdata
// — and the same SOA, compared field by field rather than skipped; when
// both reject, that a real per-line error is actually present, spot-checked
// against a known line number where the case calls for it. This is the
// check meant to catch the next divergence between Parse's happy path and
// dns.ZoneParser's own semantics generically, without needing a bespoke
// test for the specific shape of the regression — so it needs to be able
// to notice a corrupted TTL or a corrupted SOA field, and it needs to
// exercise the reject side as more than "some error came back".
//
// Cases where Parse is deliberately stricter than raw dns.ZoneParser — an
// out-of-zone name, a non-apex SOA, more than one SOA, no SOA at all — are
// not in this table: miekg has no concept of a zone apex or of "exactly
// one SOA" to disagree about, so comparing verdicts there would not be
// testing agreement, just re-asserting Parse's own policy. Those are
// covered separately (TestParseRejectsOutOfZoneNames,
// TestParseRejectsNonApexSOA, TestParseRejectsDuplicateApexSOA,
// TestParseRejectsFileWithNoSOA).
//
// The reject side is checked against the same oracle, which matters more
// than it used to: line numbers are now the whole of what the recovery
// pass produces, so nothing else in this file compares them against
// anything but a hand-written constant. dns.ZoneParser stops at a file's
// first failure and names the line it stopped on; whatever else Parse goes
// on to report, that line must be among them.
func TestParseAgreesWithMiekg(t *testing.T) {
	const apex = "e412.in"
	cases := []struct {
		name string
		file string
		// wantRejectLine, if non-zero, is a line number at least one of
		// Parse's error messages must start with when miekg also rejects
		// this file — not just "some error", the right one.
		wantRejectLine int
	}{
		{"basic", `$ORIGIN e412.in.
$TTL 300
@ IN SOA ns.e412.in. hostadmin.e412.in. ( 1 900 300 604800 900 )
bifrost 300 IN A 1.2.3.4
`, 0},
		{"blank owner continuation", `$ORIGIN e412.in.
$TTL 300
@ IN SOA ns.e412.in. hostadmin.e412.in. ( 1 900 300 604800 900 )
@ IN NS ns1.e412.in.
  IN NS ns2.e412.in.
`, 0},
		{"generate at eof", `$ORIGIN e412.in.
$TTL 300
@ IN SOA ns.e412.in. hostadmin.e412.in. ( 1 900 300 604800 900 )
$GENERATE 1-3 host$ IN A 10.0.0.$
`, 0},
		{"mid-file origin persists", `$ORIGIN e412.in.
$TTL 300
@ IN SOA ns.e412.in. hostadmin.e412.in. ( 1 900 300 604800 900 )
$ORIGIN dev.e412.in.
w IN A 1.1.1.1
x IN A 2.2.2.2
`, 0},
		{"ttl directive is the default, not last explicit ttl", `$ORIGIN e412.in.
$TTL 300
@ IN SOA ns.e412.in. hostadmin.e412.in. ( 1 900 300 604800 900 )
a 7200 IN A 1.1.1.1
b IN A 2.2.2.2
`, 0},
		{"parenthesised soa with comments", `$ORIGIN e412.in.
$TTL 300
@ IN SOA ns.e412.in. hostadmin.e412.in. ( ; comment right after the paren
	1  ; serial
	900 ; refresh
	300 ; retry
	604800 ; expire
	900 ) ; minimum
bifrost IN A 1.2.3.4
`, 0},
		{"wildcard", `$ORIGIN e412.in.
$TTL 300
@ IN SOA ns.e412.in. hostadmin.e412.in. ( 1 900 300 604800 900 )
*.nexus 300 IN A 192.168.160.200
`, 0},
		{"escaped quote txt", `$ORIGIN e412.in.
$TTL 300
@ IN SOA ns.e412.in. hostadmin.e412.in. ( 1 900 300 604800 900 )
@ IN TXT "he said \"hi\""
`, 0},
		{"crlf", strings.Join([]string{
			"$ORIGIN e412.in.", "$TTL 300",
			"@ IN SOA ns.e412.in. hostadmin.e412.in. ( 1 900 300 604800 900 )",
			"bifrost IN A 1.2.3.4", "",
		}, "\r\n"), 0},
		{"backslash escaped paren in owner", `$ORIGIN e412.in.
$TTL 300
@ IN SOA ns.e412.in. hostadmin.e412.in. ( 1 900 300 604800 900 )
weird\(escaped IN A 1.2.3.4
`, 0},
		{"bad A rdata", `$ORIGIN e412.in.
$TTL 300
@ IN SOA ns.e412.in. hostadmin.e412.in. ( 1 900 300 604800 900 )
bad IN A not-an-ip
`, 4},
		{"bad MX rdata", `$ORIGIN e412.in.
$TTL 300
@ IN SOA ns.e412.in. hostadmin.e412.in. ( 1 900 300 604800 900 )
bad IN MX Mx
`, 4},
		{"missing soa field", `$ORIGIN e412.in.
$TTL 300
@ IN SOA ns.e412.in. hostadmin.e412.in. ( 1 900 300 604800 )
`, 3},
		{"trailing include", `$ORIGIN e412.in.
$TTL 300
@ IN SOA ns.e412.in. hostadmin.e412.in. ( 1 900 300 604800 900 )
$INCLUDE /etc/passwd
`, 4},
		// Finding 1 (round 3): a whole-file error rooted in lexer state
		// carried across a statement boundary — the parenthesised SOA
		// closes, and the very next line starts with a backslash-escaped
		// '(' that only fails to parse in that context. Recovery's
		// necessarily-fresh-per-statement parsers cannot reproduce that
		// context (Parse's doc comment), so this is exactly the case the
		// whole-file-verdict guard exists for.
		{"paren-closed soa then backslash-owner line", `$ORIGIN e412.in.
$TTL 300
@ IN SOA ns.e412.in. hostadmin.e412.in. (
	1
	900
	300
	604800
	900 )
\( IN A 1.2.3.4
`, 9},
		// Round 4's three attack inputs. Each was a case where recovery
		// re-derived something dns.ZoneParser already owns — an owner name
		// under a later $ORIGIN, an owner split on an escaped space, a
		// paren span running past the last line — so each belongs in the
		// table that checks the two against each other.
		{"continuation after a later origin", `$ORIGIN e412.in.
$TTL 300
@ IN SOA ns.e412.in. hostadmin.e412.in. ( 1 900 300 604800 900 )
host IN A 1.1.1.1
$ORIGIN dev.e412.in.
	IN A 2.2.2.2
bad IN A not-an-ip
`, 7},
		{"escaped space in owner", `$ORIGIN e412.in.
$TTL 300
@ IN SOA ns.e412.in. hostadmin.e412.in. ( 1 900 300 604800 900 )
foo\ bar IN A 1.1.1.1
	IN A 2.2.2.2
bad IN A not-an-ip
`, 6},
		{"unterminated paren at eof", `$ORIGIN e412.in.
$TTL 300
@ IN SOA ns.e412.in. hostadmin.e412.in. ( 1 900 300 604800 900 )
a IN A (
1.1.1.1
`, 5},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pz, errs := zones.Parse(tc.file, apex)

			zp := dns.NewZoneParser(strings.NewReader(tc.file), dns.Fqdn(apex), "")
			var want []dns.RR
			for rr, ok := zp.Next(); ok; rr, ok = zp.Next() {
				want = append(want, rr)
			}
			wantErr := zp.Err()

			if wantErr != nil {
				if len(errs) == 0 {
					t.Fatalf("miekg rejected this file (%v) but Parse accepted it", wantErr)
				}
				if tc.wantRejectLine != 0 {
					assertNamesLine(t, errs, tc.wantRejectLine, "want one to name line %d", tc.wantRejectLine)
				}
				// The oracle: whatever else Parse reports, the line
				// dns.ZoneParser stopped on has to be one of them.
				if oracle := miekgErrLine(wantErr); oracle != 0 {
					assertNamesLine(t, errs, oracle, "miekg reports its first failure on line %d (%v); Parse must name that line too", oracle, wantErr)
				}
				return
			}
			if len(errs) != 0 {
				t.Fatalf("miekg accepted this file (%d RRs) but Parse rejected it: %v", len(want), errs)
			}

			var wantSOA *dns.SOA
			var wantNonSOA []string
			for _, rr := range want {
				if s, ok := rr.(*dns.SOA); ok {
					wantSOA = s
					continue
				}
				wantNonSOA = append(wantNonSOA, recordSignature(rr, apex))
			}

			if (wantSOA == nil) != (pz.SOA == nil) {
				t.Fatalf("SOA presence mismatch: miekg has one = %v, Parse has one = %v", wantSOA != nil, pz.SOA != nil)
			}
			if wantSOA != nil {
				if pz.SOA.Ns != wantSOA.Ns || pz.SOA.Mbox != wantSOA.Mbox || pz.SOA.Serial != wantSOA.Serial ||
					pz.SOA.Refresh != wantSOA.Refresh || pz.SOA.Retry != wantSOA.Retry ||
					pz.SOA.Expire != wantSOA.Expire || pz.SOA.Minttl != wantSOA.Minttl ||
					pz.SOA.Hdr.Ttl != wantSOA.Hdr.Ttl {
					t.Fatalf("SOA = %+v, want (miekg's) %+v", pz.SOA, wantSOA)
				}
			}

			var got []string
			for _, r := range pz.Records {
				got = append(got, fmt.Sprintf("%s %s %d %s", r.Name, r.Type, r.TTL, r.RData))
			}
			sort.Strings(wantNonSOA)
			sort.Strings(got)
			if fmt.Sprint(got) != fmt.Sprint(wantNonSOA) {
				t.Fatalf("records = %v, want %v", got, wantNonSOA)
			}
		})
	}
}

// assertNamesLine checks that at least one of errs is about line n.
func assertNamesLine(t *testing.T, errs []string, n int, why string, args ...any) {
	t.Helper()
	prefix := fmt.Sprintf("line %d:", n)
	for _, e := range errs {
		if strings.HasPrefix(e, prefix) {
			return
		}
	}
	t.Fatalf("errs = %v; "+why, append([]any{errs}, args...)...)
}

// miekgAtLineRE reads the position dns.ParseError formats into its own
// Error() string. ParseError keeps the line and column unexported with no
// accessor, so its message is the only place to read them from. Written
// out again here rather than reusing the package's own errLine on
// purpose: an oracle that shares code with the thing it is checking agrees
// with it by construction.
var miekgAtLineRE = regexp.MustCompile(`at line: (\d+):\d+`)

// miekgErrLine returns the 1-based line err names, or 0 if it names none.
func miekgErrLine(err error) int {
	m := miekgAtLineRE.FindStringSubmatch(err.Error())
	if m == nil {
		return 0
	}
	n, convErr := strconv.Atoi(m[1])
	if convErr != nil {
		return 0
	}
	return n
}

// recordSignature renders rr — as returned directly by a raw
// dns.NewZoneParser — the same way Parse's ParsedRecord fields would, so
// TestParseAgreesWithMiekg can compare dnsaur's output against miekg's own
// without re-deriving RelName's conversion separately (and risking that
// second copy quietly drifting from the real one).
func recordSignature(rr dns.RR, apex string) string {
	name := rr.Header().Name
	rel := name
	if strings.EqualFold(strings.TrimSuffix(name, "."), apex) {
		rel = "@"
	} else if strings.HasSuffix(strings.ToLower(name), "."+strings.ToLower(apex)+".") {
		rel = name[:len(name)-len(apex)-2]
	}
	return fmt.Sprintf("%s %s %d %s", rel, dns.TypeToString[rr.Header().Rrtype], rr.Header().Ttl, strings.TrimPrefix(rr.String(), rr.Header().String()))
}

// $GENERATE expands one line into up to 65,536 records, and nothing counted
// what a whole file expands to: under the 1 MiB import cap a file of ~34k
// directives asks this process for ~2.2 billion RRs before any check runs.
// The budget is the file's, not the directive's.
func TestParseRefusesAFileThatExpandsPastTheRecordCap(t *testing.T) {
	const f = "$ORIGIN e412.in.\n" +
		"$TTL 300\n" +
		"@ IN SOA ns.e412.in. hostadmin.e412.in. ( 1 900 300 604800 900 )\n" +
		"$GENERATE 0-65535 h$ 300 IN A 1.2.3.4\n" +
		"$GENERATE 0-65535 g$ 300 IN A 1.2.3.4\n"
	pz, errs := zones.Parse(f, "e412.in")
	if len(errs) == 0 {
		t.Fatalf("Parse accepted a file expanding to %d records", len(pz.Records))
	}
	if !strings.Contains(errs[0], "more than") {
		t.Errorf("errs = %v; want a message naming the record limit", errs)
	}
}

// Every statement used to be parsed with the file's whole $ORIGIN history
// replayed ahead of it, so a file of n directives cost O(n^2) line parses —
// 1 MiB of them is ~95k lines and ~4.5 billion parses. One synthesised
// origin line carries the same state at a fixed cost per statement.
//
// The budget is wall clock because the cost is the point: 4,000 directives
// took ~7s replayed and a few milliseconds resolved, so anything near the
// budget is the quadratic walk coming back rather than a slow machine.
func TestParseOfManyOriginDirectivesStaysLinear(t *testing.T) {
	var b strings.Builder
	b.WriteString("$ORIGIN e412.in.\n$TTL 300\n@ IN SOA ns.e412.in. hostadmin.e412.in. ( 1 900 300 604800 900 )\n")
	for i := range 4000 {
		fmt.Fprintf(&b, "$ORIGIN sub%d.e412.in.\n", i)
	}
	// Back to the apex, so the file ends with a record the zone can hold —
	// this has to be a file Parse accepts, not one it bails out of early.
	b.WriteString("$ORIGIN e412.in.\nhost 300 IN A 1.2.3.4\n")

	start := time.Now()
	pz, errs := zones.Parse(b.String(), "e412.in")
	elapsed := time.Since(start)

	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(pz.Records) != 1 || pz.Records[0].Name != "host" {
		t.Fatalf("records = %+v, want the single host A", pz.Records)
	}
	// The line number still has to be right: the synthesised origin line is
	// not a line of the file, and attributing it as one would shift every
	// record's line by however many directives came before it.
	if pz.Records[0].Line != 4005 {
		t.Errorf("host is on line %d, want 4005", pz.Records[0].Line)
	}
	if elapsed > 2*time.Second {
		t.Errorf("parsing 4000 $ORIGIN lines took %v", elapsed)
	}
}

// A relative $ORIGIN resolves against the one in force, not against the zone
// apex (RFC 1035 §5.1) — the property the replayed history had for free and
// a synthesised line has to keep.
func TestParseResolvesARelativeOriginAgainstTheCurrentOne(t *testing.T) {
	const f = "$ORIGIN e412.in.\n" +
		"$TTL 300\n" +
		"@ IN SOA ns.e412.in. hostadmin.e412.in. ( 1 900 300 604800 900 )\n" +
		"$ORIGIN ab\n" +
		"$ORIGIN cd\n" +
		"host 300 IN A 1.2.3.4\n"
	pz, errs := zones.Parse(f, "e412.in")
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(pz.Records) != 1 || pz.Records[0].Name != "host.cd.ab" {
		t.Fatalf("records = %+v, want one record named host.cd.ab", pz.Records)
	}
	if pz.Records[0].Line != 6 {
		t.Errorf("host is on line %d, want 6", pz.Records[0].Line)
	}
}
