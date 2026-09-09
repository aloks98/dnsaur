package zones

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/miekg/dns"
)

// ParsedZone is a BIND master file decoded, but not yet stored: the SOA
// pulled out onto its own field (a zone's SOA lives on the zones row, not
// as a zone_records row — see store.Zone) and every other RR in Records.
//
// SOA is nil only when Parse's second return is non-empty — a zone file
// with no SOA, or more than one at the apex, is rejected (RFC 1034 §4.1.1:
// a zone has exactly one), not returned with a nil SOA for the caller to
// separately check for. Whenever Parse's errs is empty, SOA is guaranteed
// non-nil, and its header TTL is guaranteed non-zero (soaTTLProblem).
type ParsedZone struct {
	SOA     *dns.SOA
	Records []ParsedRecord
}

// ParsedRecord is one non-SOA RR read from a master file, already in the
// shape store.ZoneRecord stores a record in: Name relative to the zone
// apex ("@", "bifrost", "*.nexus" — see RelName) and RData as
// presentation-format rdata with no header.
//
// Line is the 1-based master-file line the record's statement starts on.
// It matters most for a file Parse itself accepts: an importer validates
// every record a second time, against rules a master file parser has no
// concept of (an RRSet whose TTLs disagree — RFC 2181 §5.2, a CNAME beside
// another type — RFC 1034 §3.6.2, a TTL past 2147483647 — RFC 2181 §8),
// and a file can be syntactically perfect and still fail every one of
// them. Those rejections never appear in Parse's own error list, so Line
// is the only thing that can name the lines they are on.
//
// Line is 0 on every record in the slice — never on just some of them —
// when Parse cannot attribute lines with certainty. See Parse's "Line
// numbers on an accepted file" for exactly when that happens, and why
// reporting no line at all is the right answer there rather than a guess.
type ParsedRecord struct {
	Name, Type, RData string
	TTL               uint32
	Line              int
}

// atLineRE matches the position dns.ParseError appends to its own Error()
// string ("... at line: 12:5"). ParseError keeps the line and column as
// unexported fields — Error() formats them but there is no accessor — so
// this regexp is the only way Parse can recover the line number for a
// failed statement, and also the only way to strip that (statement-local,
// and therefore misleading once re-reported against the whole file) suffix
// before building Parse's own file-absolute message.
var atLineRE = regexp.MustCompile(`\s*at line: (\d+):\d+$`)

// errLine returns the 1-based line number dns.ParseError embedded in err's
// message, relative to whatever text was handed to that parser, or 0 if
// the message doesn't have the expected shape.
func errLine(err error) int {
	m := atLineRE.FindStringSubmatch(err.Error())
	if m == nil {
		return 0
	}
	n, convErr := strconv.Atoi(m[1])
	if convErr != nil {
		return 0
	}
	return n
}

// withoutLine strips the "at line: L:C" dns.ParseError appends, since that
// position is relative to a single statement's own text and Parse reports
// its own file-absolute line number instead.
func withoutLine(err error) string {
	return atLineRE.ReplaceAllString(err.Error(), "")
}

// atLine prefixes msg with the line it is about, or returns it unchanged
// when there is no line to name (line == 0 — see ParsedRecord.Line).
func atLine(line int, msg string) string {
	if line == 0 {
		return msg
	}
	return fmt.Sprintf("line %d: %s", line, msg)
}

// Directive tokens dns.ZoneParser recognizes. $ORIGIN and $TTL are
// persistent state — RFC 1035 §5.1 has both apply to every record between
// them and the next directive of the same kind, or EOF — so splitStatements
// handles them separately from $INCLUDE and $GENERATE, which are not: a
// $GENERATE line is itself what produces RRs, one time, not a modifier of
// later ones, and $INCLUDE reads a second file once. Of the two persistent
// ones only $ORIGIN is actually replayed; see splitStatements for why $TTL
// is checked and then dropped.
const (
	dirOrigin   = "$ORIGIN"
	dirTTL      = "$TTL"
	dirInclude  = "$INCLUDE"
	dirGenerate = "$GENERATE"
)

var zoneDirectives = []string{dirOrigin, dirTTL, dirInclude, dirGenerate}

// directiveKind returns which directive trimmed (a line with surrounding
// whitespace already removed) opens with, or "" if it isn't one. Matched
// the same way dns.ZoneParser's own lexer does it: the first word on a
// line, case-insensitively (scan.go's zlexer upper-cases before comparing).
func directiveKind(trimmed string) string {
	upper := strings.ToUpper(trimmed)
	for _, d := range zoneDirectives {
		if upper == d || strings.HasPrefix(upper, d+" ") || strings.HasPrefix(upper, d+"\t") {
			return d
		}
	}
	return ""
}

// isBlankOrComment reports whether trimmed is nothing worth parsing on its
// own: an empty line, or one that is entirely a ';' comment.
func isBlankOrComment(trimmed string) bool {
	return trimmed == "" || strings.HasPrefix(trimmed, ";")
}

// parenDelta returns line's net change in open-parenthesis depth, and the
// quote state it ends in. RFC 1035 §5.1 lets '(' ... ')' span physical
// lines to continue one RR onto several lines. Two things on the line
// never count as one of those parentheses: one inside a double-quoted
// character-string, and — the more easily missed case, since it needs no
// quotes — one immediately preceded by a backslash, RFC 1035's general
// escape for "this character is literal", which applies equally inside and
// outside a quoted string (a bare `weird\(name` owner is legal and opens
// nothing). Either way the character after the backslash is skipped
// outright rather than interpreted, matching how quoteCharacterString's
// counterpart escaping works in internal/store/zonemigrate.go. A ';'
// outside a quote starts a comment that runs to the end of the physical
// line.
//
// inQuote is the state the previous physical line ended in, and the second
// result is this line's, because a quoted character-string may itself span
// physical lines inside a parenthesised group — confirmed against
// dns.ZoneParser directly, not assumed. Restarting each line outside a
// quote desynchronises the splitter from miekg in both directions: a
// quoted '(' on a continuation line gets counted (the span runs on past
// the record's real end, folding every following record into one statement
// — and one statement is one parser, which latches its first error and
// never names the second), and a quoted ')' gets counted too (the span
// ends early, and the ordinal line attribution that follows can then point
// at an innocent line while its own count check still balances).
func parenDelta(line string, inQuote bool) (int, bool) {
	delta := 0
	for i := 0; i < len(line); i++ {
		switch c := line[i]; {
		case c == '\\':
			i++ // skip the escaped character, whatever it is, quoted or not
		case c == '"':
			inQuote = !inQuote
		case !inQuote && c == ';':
			return delta, inQuote
		case !inQuote && c == '(':
			delta++
		case !inQuote && c == ')':
			delta--
		}
	}
	return delta, inQuote
}

// statement is one statement's worth of a master file, as splitStatements
// carved it up: one record (with whatever $ORIGIN replay it needs
// prefixed — see splitStatements), or one $INCLUDE/$GENERATE line on its
// own, or — when a new $ORIGIN/$TTL line itself fails validation — that
// directive on its own, so recovery's normal per-statement error handling
// reports it without special-casing.
type statement struct {
	// text is those lines joined with "\n", plus a final "\n": dns.
	// ZoneParser treats a field cut off by true EOF far more leniently
	// than the same field cut off by a newline (an SOA missing its last
	// field silently defaults that field to 0 at EOF instead of erroring
	// — confirmed against dns.ZoneParser directly, not assumed), so a
	// statement's text always ends the way a line embedded in a larger
	// file actually would.
	//
	// lineNumbers[i] is the 1-based source line that text's i'th line
	// actually came from — a full mapping, not a single starting offset,
	// because text is not necessarily a contiguous slice of the file: a
	// replayed $ORIGIN or $TTL directive can be many lines, and any number
	// of blank or comment lines, earlier.
	lineNumbers []int
	text        string
	// directive is the directive token this statement consists of when it
	// is one ($ORIGIN, $TTL, $INCLUDE, $GENERATE), and "" when it is a
	// record. Only the record statements carry an ordinal position an RR
	// can be matched against — see recordStatementLines.
	directive string
	// recordLine is the 1-based line this statement is "about" — the
	// record's own first line, or the directive's own line — and what
	// recovery falls back to if a parse error's line can't be recovered
	// from its message.
	recordLine int
}

// splitStatements carves text into statements: one per record, one per
// $INCLUDE/$GENERATE, and one per $ORIGIN/$TTL line that fails validation.
// It has two callers with quite different needs, and it is worth being
// precise about which is which, because only one of them is on the path a
// clean file takes:
//
//   - parseWithRecovery parses every statement it returns, to enumerate
//     the bad lines of a file that is already being rejected.
//   - recordStatementLines uses nothing but recordLine, to attribute a
//     source line to each RR of a file that parsed cleanly. It never looks
//     at text, and its result is discarded outright if the statement count
//     and the RR count disagree (Parse's "Line numbers on an accepted
//     file"), so a mis-split degrades to "no line numbers" rather than to
//     wrong ones.
//
// Every statement recovery parses gets its own short-lived
// *dns.ZoneParser (recovery's central idea: one bad record only ends its
// own parser, not every record after it — Parse's doc comment). That
// parser needs to see whatever $ORIGIN was actually in effect for this
// record in the original file, which is not necessarily anything nearby
// (RFC 1035 §5.1: it persists until the next $ORIGIN, or EOF — not just
// for the one record that happens to follow it). So splitStatements
// tracks the origin in effect and replays it ahead of every later
// statement — as one synthesised "$ORIGIN <effective>." line, not as the
// file's own history of them.
//
// The history is what this used to replay, and the cost was quadratic:
// every statement re-parsed every $ORIGIN line before it, so a 1 MiB file
// of ~95k directives (the import cap allows exactly that) came to ~4.5
// billion line parses. One line carries the same state, because a
// relative $ORIGIN is resolved *before* it is stored: "$ORIGIN dev" after
// an earlier "$ORIGIN e412.in." becomes the one line
// "$ORIGIN dev.e412.in.". Which origin is in effect decides whether a name
// is in this zone at all, and so whether recovery has an out-of-zone line
// to report, so the resolution is dns.ZoneParser's own rather than a rule
// re-derived here — see effectiveOrigin.
//
// $TTL is deliberately NOT replayed, though it persists exactly the same
// way. All it decides is the TTL a record that omits its own falls back to
// (RFC 2308 §4), and recovery returns no records — only messages and line
// numbers. A record with no TTL and no $TTL in scope is not an error
// either: dns.ZoneParser hands back Ttl 0 and says nothing. So replaying
// $TTL costs a parse per directive and cannot change a single message.
// A $TTL line that is itself malformed is still checked and reported,
// because that is the one observable thing it can do here.
//
// "Verified" matters for $ORIGIN: a new one is checked, against the origin
// in effect and nothing else, before it becomes the origin in effect. A
// directive that fails the check is reported once, as its own statement,
// and never takes effect — so one broken $ORIGIN doesn't also fail every
// record after it with a copy of the same error (it would, every time, if
// the bad line became the replayed prefix: the prefix is always parsed
// before a statement's own record, so the same failure would recur on
// every single one).
//
// $INCLUDE and $GENERATE are not persistent state, so neither is folded
// into a following record's statement or deferred — each gets its own
// statement immediately, right where it appears. Folding either onto the
// next record would make that record's fate depend on the directive's:
// $INCLUDE is never allowed here (dns.ZoneParser rejects it unless
// SetIncludeAllowed(true), which Parse never calls), so folding it onto a
// record would hide whatever is wrong with that record behind the
// directive's own failure. Emitting it standalone means a directive at the
// very end of the file — with no following record to (maybe) fold it onto
// — is never simply dropped either.
//
// A record whose line starts with whitespace has no owner of its own: it
// inherits the previous RR's (RFC 1035 §5.1), the shape BIND, Knot and nsd
// all emit for a second-or-later RR at one name. splitStatements does
// nothing about that, deliberately. Handing such a line to a fresh parser
// yields an RR with an empty owner name, which recovery skips outright —
// it is an artefact of splitting, not a defect in the file, and recovery
// returns no records for a missing owner to spoil. Supplying the owner
// back textually is what earlier revisions did, and it was wrong twice
// over: the remembered token is the file's *relative* token, so replaying
// it under a later $ORIGIN re-resolved it against the wrong origin, and
// picking it out of the line at all needs an RFC 1035-aware tokenizer
// (strings.Fields splits `foo\ bar` in the middle of an escaped space).
// Both were symptoms of recovery reconstructing records, which it no
// longer does.
//
// Blank lines and whole-line comments are dropped. A parenthesised record
// is kept as one statement spanning every physical line up to its closing
// paren (parenDelta).
func splitStatements(text, origin string) []statement {
	lines := strings.Split(text, "\n")
	// Parse guarantees text ends in "\n", so strings.Split always leaves a
	// final empty element behind it. That element is not a line of the
	// file: counting it as one lets an unterminated '(' span absorb it and
	// report an error one line past EOF.
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}

	var out []statement
	// The origin in effect, as the one synthesised line replayed ahead of
	// every statement, and the file line the directive that set it was on —
	// so that a parse error reported against the synthesised line (nothing
	// produces one: it is a line dns.ZoneParser itself just accepted) still
	// names something real. Empty until the file's first $ORIGIN.
	var originLine string
	var originLineNo int

	linesFor := func(idx []int) (string, []int) {
		stmtLines := make([]string, 0, len(idx)+1)
		lineNumbers := make([]int, 0, len(idx)+1)
		if originLine != "" {
			stmtLines = append(stmtLines, originLine)
			lineNumbers = append(lineNumbers, originLineNo)
		}
		for _, li := range idx {
			stmtLines = append(stmtLines, lines[li])
			lineNumbers = append(lineNumbers, li+1)
		}
		return strings.Join(stmtLines, "\n") + "\n", lineNumbers
	}

	// validated reports whether idx's lines, replayed after the origin in
	// effect and with no record following, parse without error — see
	// splitStatements' doc comment for why a $TTL line is checked this way
	// and then dropped.
	validated := func(idx []int) bool {
		text, _ := linesFor(idx)
		zp := dns.NewZoneParser(strings.NewReader(text), origin, "")
		for _, ok := zp.Next(); ok; _, ok = zp.Next() {
		}
		return zp.Err() == nil
	}

	// directiveStatement builds the standalone statement emitted for a
	// directive line: the origin in effect, then the line itself.
	directiveStatement := func(kind string, idx []int, at int) statement {
		text, lineNumbers := linesFor(idx)
		return statement{text: text, lineNumbers: lineNumbers, directive: kind, recordLine: at + 1}
	}

	for i := 0; i < len(lines); {
		trimmed := strings.TrimSpace(lines[i])
		if isBlankOrComment(trimmed) {
			i++
			continue
		}

		switch kind := directiveKind(trimmed); kind {
		case dirOrigin:
			if eff, ok := effectiveOrigin(originLine, lines[i], origin); ok {
				originLine, originLineNo = dirOrigin+" "+eff, i+1
			} else {
				out = append(out, directiveStatement(kind, []int{i}, i))
			}
			i++
			continue
		case dirTTL:
			// Checked on its own, never replayed — see the doc comment.
			// What it would set is unobservable here; being malformed is
			// not.
			if probe := []int{i}; !validated(probe) {
				out = append(out, directiveStatement(kind, probe, i))
			}
			i++
			continue
		case dirInclude, dirGenerate:
			out = append(out, directiveStatement(kind, []int{i}, i))
			i++
			continue
		}

		// A statement always begins outside a quoted character-string, so
		// the span starts with inQuote false and carries the state forward
		// from line to line — see parenDelta.
		start := i
		depth, inQuote := parenDelta(lines[i], false)
		span := 1
		for depth > 0 && start+span < len(lines) {
			var d int
			d, inQuote = parenDelta(lines[start+span], inQuote)
			depth += d
			span++
		}

		idx := make([]int, 0, span)
		for k := range span {
			idx = append(idx, start+k)
		}
		text, lineNumbers := linesFor(idx)
		out = append(out, statement{text: text, lineNumbers: lineNumbers, recordLine: start + 1})
		i = start + span
	}
	return out
}

// originProbe is the record effectiveOrigin appends to a $ORIGIN line to
// read the origin back out of dns.ZoneParser. Its owner is "@", so the
// parser resolves it to whatever origin is in effect — which is the answer —
// and the rest of the line is the cheapest RR that parses.
const originProbe = "@ 0 IN A 0.0.0.0\n"

// effectiveOrigin returns the origin in effect after line (a $ORIGIN
// directive), given the origin line currently in effect and the file's base
// origin, and reports whether line is usable at all.
//
// The resolution is dns.ZoneParser's, not this file's. A relative $ORIGIN is
// resolved against the origin already in effect (RFC 1035 §5.1), and
// re-deriving that rule here is precisely the class of defect this parser's
// recovery pass has shipped three times — every one of them recovery
// reimplementing something dns.ZoneParser already owns. Asking the parser
// for the owner name it gives a record owned by "@" costs one line and
// cannot disagree with it.
//
// It doubles as the check the directive has to pass before it takes effect:
// a $ORIGIN the parser rejects yields ok=false, and the caller reports the
// line instead of adopting it.
func effectiveOrigin(originLine, line, base string) (string, bool) {
	text := line + "\n" + originProbe
	if originLine != "" {
		text = originLine + "\n" + text
	}
	zp := dns.NewZoneParser(strings.NewReader(text), base, "")
	rr, ok := zp.Next()
	for _, more := zp.Next(); more; _, more = zp.Next() {
	}
	if !ok || rr == nil || zp.Err() != nil {
		return "", false
	}
	return rr.Header().Name, true
}

// recordStatementLines returns the 1-based start line of every record
// statement in text, in file order — the ordinal map parseClean attributes
// ParsedRecord.Line from. Directive statements are left out: none of them
// is an RR, and the one that does produce RRs ($GENERATE) produces any
// number of them from a single line, which is exactly the disagreement the
// count check is there to notice (Parse's "Line numbers on an accepted
// file").
func recordStatementLines(text, origin string) []int {
	var out []int
	for _, stmt := range splitStatements(text, origin) {
		if stmt.directive == "" {
			out = append(out, stmt.recordLine)
		}
	}
	return out
}

// recordProblem returns a message naming why rr is not this zone's data to
// store, or "" if it is. It is the one place either of Parse's two passes
// decides that, so the accepted file and the rejected one can never
// disagree about what counts as acceptable. Three things disqualify an RR:
//
//   - It has no owner name at all. A line whose owner is blank inherits
//     the previous RR's (RFC 1035 §5.1); dns.ZoneParser resolves that
//     itself while it is reading one continuous file, so the only way an
//     empty name reaches here from parseClean is a file whose very first
//     record line has no owner and nothing before it to inherit from.
//     (Recovery reaches this case constantly, by construction, and skips
//     such RRs before asking — see splitStatements.)
//   - Its name is not apex or a subdomain of it — RelName trims ".apex"
//     off qname and returns "@" when qname is the apex itself; if the
//     result is neither, the trim was a no-op and name never had that
//     suffix, meaning it's an absolute name under some other apex (RelName
//     has no other way to signal that — see zone.go).
//   - It's an SOA anywhere but the zone apex. A zone's SOA belongs at "@"
//     by definition; silently adopting one found elsewhere in the file as
//     "the" zone's SOA would misrepresent which record actually is.
func recordProblem(rr dns.RR, apex string) string {
	name := rr.Header().Name
	if name == "" {
		return "record has no owner name, and no earlier record to inherit one from (RFC 1035 §5.1)"
	}
	rel := RelName(name, apex)
	if rel != apexName && rel == normalizeName(name) {
		return fmt.Sprintf("%s is outside zone %s", name, apex)
	}
	if _, isSOA := rr.(*dns.SOA); isSOA && rel != apexName {
		return fmt.Sprintf("SOA at %s is not the zone apex (%s)", name, apex)
	}
	return ""
}

// isApexSOA reports whether rr is this zone's own SOA — the one record a
// zone must have exactly one of (RFC 1034 §4.1.1), which both passes
// therefore have to count.
func isApexSOA(rr dns.RR, apex string) bool {
	_, isSOA := rr.(*dns.SOA)
	return isSOA && RelName(rr.Header().Name, apex) == apexName
}

// classify turns a successfully parsed, already-acceptable RR into either
// a ParsedRecord (Name relative to apex via RelName, Line as given) or,
// for an SOA, the zone's SOA. Whether it is acceptable at all is
// recordProblem's call, not this one's.
func classify(rr dns.RR, apex string, line int) (*ParsedRecord, *dns.SOA) {
	if s, isSOA := rr.(*dns.SOA); isSOA {
		// Normalized the same way ParsedRecord.Name is (RelName's
		// normalizeName): dns.ZoneParser preserves whatever case the file
		// actually used ("$ORIGIN E412.IN." would otherwise leave
		// Hdr.Name as "E412.IN." while every ParsedRecord.Name is already
		// lowercase), and a caller storing both should see one convention,
		// not two.
		s.Hdr.Name = dns.Fqdn(normalizeName(s.Hdr.Name))
		return nil, s
	}
	return &ParsedRecord{
		Name: RelName(rr.Header().Name, apex),
		// dns.Type.String, not the TypeToString map, so a type miekg/dns has
		// no name for comes back as "TYPE65280" rather than as the empty
		// string: BuildRecord refuses it either way, and only one of those
		// two can name it in the message the importer reports.
		Type:  dns.Type(rr.Header().Rrtype).String(),
		RData: RDataOf(rr),
		TTL:   rr.Header().Ttl,
		Line:  line,
	}, nil
}

// Parse reads text as a BIND master file for the zone apex, and returns
// its SOA and records — Records named relative to apex, the form
// zone_records.name stores (RelName does the conversion) — alongside one
// message per line Parse could not accept, empty when the file is clean.
//
// # What dns.ZoneParser actually does after an error
//
// dns.NewZoneParser(...).Next() latches the first error permanently: once
// it sets zp's internal parseErr, every subsequent Next() call on that same
// *ZoneParser returns (nil, false) immediately without reading any more of
// the input (scan.go: "if zp.parseErr != nil { return nil, false }" is the
// first line of Next). There is no flag or method that resumes it — one
// bad line and that parser is permanently done, and zp.Err() only ever
// reports that first failure.
//
// # Two passes, not one
//
// Parse reads the entire file with a single, ordinary *dns.ZoneParser
// (parseClean). That is the only pass that ever produces a record: blank-
// owner continuation lines (RFC 1035 §5.1: a line starting with whitespace
// inherits the previous RR's owner), $INCLUDE, $GENERATE, $ORIGIN/$TTL
// persistence, all of it, exactly as dns.ZoneParser implements them,
// because dns.ZoneParser is what's running. Nothing is reimplemented, so
// nothing here can disagree with it.
//
// Only when that single pass fails — a real syntax error, or (parseClean's
// other job) an RR recordProblem rejects, an absolute name under some
// other apex or a non-apex SOA — does Parse also run per-statement
// recovery (parseWithRecovery), whose entire output is messages and line
// numbers: it hand-splits the file into statements and parses each with
// its own *dns.ZoneParser (splitStatements), which is the only way to keep
// going past a bad statement and so the only way to name the second bad
// line and the third rather than stopping at the first.
//
// Recovery builds no records, and that is the point. Returning records
// from it is what would force it to reproduce owner inheritance, $ORIGIN
// resolution and RFC 1035 escaping on its own — three separate defects in
// three consecutive reviews, every one of them recovery re-deriving
// something dns.ZoneParser already owns. Naming a bad line needs none of
// that. Whatever records Parse returns alongside a rejection are simply
// what parseClean managed to read before it stopped, which is often
// nothing; a rejected file's records are not going to be stored anyway.
//
// # Recovery is a fallback, not a second opinion
//
// parseClean and parseWithRecovery can disagree — recovery gives every
// statement a fresh *dns.ZoneParser, with no memory of lexer state from
// the statement before it, and some of dns.ZoneParser's own errors are
// rooted in exactly that: a stray '(' left open, or (concretely, found by
// feeding dns.ZoneParser adversarial input directly, not hypothesized) a
// line beginning '\(' right after a parenthesised SOA, which the whole
// file parser rejects ("no blank after owner") but which parses cleanly
// once split into its own statement with no preceding context. Recovery
// re-parsing such a statement and finding nothing wrong does not mean the
// file is fine — it means recovery's own splitting is blind to that
// particular failure, which is expected (splitStatements' doc comment)
// rather than a bug to chase.
//
// So parseClean's verdict is authoritative when the two disagree: if it
// rejected the file and recovery comes back with zero errors of its own,
// Parse still returns parseClean's original error rather than accept a
// file dns.ZoneParser itself would refuse to serve. That is the one
// guarantee Parse makes unconditionally: it will under-report which lines
// are wrong before it will ever report none.
//
// # Line numbers on an accepted file
//
// A file Parse accepts still has lines worth naming: the importer above it
// validates every record again, against rules no master file parser has
// (an RRSet whose TTLs disagree, a CNAME beside another type, a TTL past
// 2147483647), and rejects files that are syntactically flawless. Those
// lines never pass through recovery — recovery never runs for such a file
// — so ParsedRecord.Line has to be filled in on the accepting path too.
//
// dns.ZoneParser exposes no line for an RR it parsed successfully (only a
// failure's line is recoverable, and only by regexp — see atLineRE), so
// Parse attributes lines by ordinal: it asks splitStatements for the start
// line of every record statement and gives the i'th RR the i'th of those.
// That puts the hand-rolled splitter within reach of a file that has
// nothing wrong with it, which the two-pass design otherwise avoids — so
// the attribution is made self-checking. If the two counts differ at all,
// every record gets Line 0 and the records are returned regardless: a
// count mismatch means the splitter and dns.ZoneParser disagree about
// where statements begin, and the honest response to that is no line
// number rather than a wrong one pointing at an innocent line.
//
// A file using $GENERATE lands in that branch by construction — one
// statement, N records — and correctly so: there is no single line those N
// records are on. Their names, types and rdata are still returned intact,
// which is what a caller has to fall back to naming them by.
//
// # One rule dns.ZoneParser does not have
//
// A file whose SOA carries a TTL of 0 is rejected even though
// dns.ZoneParser accepts it without complaint — the case RFC 2308 §4 is
// about, a file that names no default TTL at all. See soaTTLProblem for
// why that one value cannot be stored and why no other zero TTL is
// refused.
func Parse(text, apex string) (ParsedZone, []string) {
	// A master file is meant to end with a trailing newline — every writer
	// this package deals with, including its own Render, produces one.
	// Without it, a field truncated by true EOF is treated far more
	// leniently by dns.ZoneParser than the same field cut off by a
	// newline — an SOA missing its last field silently defaults that
	// field to 0 instead of erroring, confirmed directly against
	// dns.ZoneParser, not assumed — so Parse normalizes this away up
	// front rather than let a file's only problem be the one difference
	// between "silently wrong" and "reported".
	if !strings.HasSuffix(text, "\n") {
		text += "\n"
	}

	origin := dns.Fqdn(apex)

	pz, soaLine, cleanErrs := parseClean(text, apex, origin)
	if len(cleanErrs) == 0 {
		if problem := soaTTLProblem(pz.SOA); problem != "" {
			// Deliberately not routed through parseWithRecovery: recovery
			// exists to enumerate the bad lines of a file dns.ZoneParser
			// itself refused, and dns.ZoneParser accepted this one. Running
			// it here could only replace this message with one of its own —
			// or, finding nothing, fall back to it anyway.
			return pz, []string{atLine(soaLine, problem)}
		}
		return pz, nil
	}
	if errs := parseWithRecovery(text, apex, origin); len(errs) > 0 {
		return pz, errs
	}
	// See "Recovery is a fallback, not a second opinion" above: parseClean
	// already found a reason to reject this file, and recovery finding
	// none of its own does not overrule that.
	return pz, cleanErrs
}

// soaTTLProblem returns a message naming why soa's own header TTL is not
// usable as this zone's soa_ttl, or "" if it is. The only thing wrong with
// it is a zero, and a zero arrives two ways that a master file parser
// cannot tell apart after the fact:
//
//   - The file names no TTL at all. RFC 2308 §4 makes $TTL the default for
//     a record that omits one; with no $TTL in scope and no TTL on the SOA
//     line, dns.ZoneParser supplies neither a default nor an error and
//     simply returns 0 — a value the file never stated. (It then carries
//     that 0 forward onto every record after the SOA, since its fallback
//     with no $TTL set is the last explicit TTL seen.)
//   - The file says zero outright, as `$TTL 0` or `@ 0 IN SOA ...`.
//
// Either way the stored zone would answer every NXDOMAIN with a TTL of
// min(SOAMinimum, SOATTL) = 0 (RFC 2308 §5 — see answer.go), so nothing
// the zone denies is negatively cacheable and every miss re-queries it
// forever. Milestone A made soa_ttl non-client-settable to keep any hand
// write from producing that (defaultSOATTL, internal/api/zones_handlers.go);
// a zone file must not be the door that lets it back in.
//
// Only the SOA's TTL is checked. An explicit 0 on an ordinary record is a
// legal "do not cache me" RR that POST /zones/{id}/records accepts, and
// refusing it here would make import stricter than a hand write — an
// exported zone would stop importing back, which is the round trip spec §8
// rests on.
func soaTTLProblem(soa *dns.SOA) string {
	if soa == nil || soa.Hdr.Ttl != 0 {
		return ""
	}
	return "the zone's SOA has a TTL of 0: add a $TTL directive (RFC 2308 §4) or a TTL on the SOA record, or every negative answer this zone gives becomes uncacheable (RFC 2308 §5)"
}

// maxZoneFileRecords bounds how many RRs one file may expand to, on both of
// Parse's passes.
//
// $GENERATE is why a byte cap on the file is not one: dns.ZoneParser bounds
// a single directive to the 65,536 RRs its range can name, but nothing
// bounded a file, and under the 1 MiB import cap ~34k such lines expand to
// ~2.2 billion RRs — all of them allocated before any rule got to look at
// one. It is DefaultMaxTransferRecords because a file and a transfer are the
// same question asked twice: how large a zone may this process be made to
// build in one go. Both are far above any zone this server is likely to hold
// and far below anything that would threaten it.
const maxZoneFileRecords = DefaultMaxTransferRecords

// tooManyRecords is the message both passes report when the budget runs out.
var tooManyRecords = fmt.Sprintf("the file expands to more than %d records", maxZoneFileRecords)

// parseClean reads the whole file with one dns.ZoneParser — see Parse's
// doc comment. It is the only producer of records, on both the accepting
// and the rejecting path; on the latter it returns whatever it read before
// it stopped, which the caller passes on unchanged.
//
// The second result is the 1-based line the zone's apex SOA was read from,
// or 0 when there was none to read or lines could not be attributed at all
// (Parse's "Line numbers on an accepted file"). Parse needs it to name the
// line for the one SOA problem it checks after the fact — see
// soaTTLProblem — which is not something this pass can decide while it is
// still reading records.
//
// A non-empty message list always has at least one message: dns.ZoneParser
// itself failed, or recordProblem rejected an RR, or the file had more
// than one SOA at the apex, or none at all (RFC 1034 §4.1.1 — every zone
// has exactly one). Those messages are kept as the fallback for when
// recovery does not independently reproduce a reason to reject the file
// (Parse's doc comment).
func parseClean(text, apex, origin string) (ParsedZone, int, []string) {
	zp := dns.NewZoneParser(strings.NewReader(text), origin, "")
	var rrs []dns.RR
	for rr, ok := zp.Next(); ok; rr, ok = zp.Next() {
		if len(rrs) >= maxZoneFileRecords {
			return ParsedZone{}, 0, []string{tooManyRecords}
		}
		rrs = append(rrs, rr)
	}
	parseErr := zp.Err()

	// Ordinal line attribution, and its self-check — Parse's "Line numbers
	// on an accepted file". A file that stopped early has read fewer RRs
	// than it has statements almost by definition, so it is not asked at
	// all: those records go back with Line 0, and the lines that matter
	// for it are the ones in the error list.
	var starts []int
	if parseErr == nil {
		starts = recordStatementLines(text, origin)
	}
	attributed := parseErr == nil && len(starts) == len(rrs)

	var pz ParsedZone
	var soaLine int
	for i, rr := range rrs {
		line := 0
		if attributed {
			line = starts[i]
		}
		if problem := recordProblem(rr, apex); problem != "" {
			return pz, soaLine, []string{atLine(line, problem)}
		}
		if isApexSOA(rr, apex) && pz.SOA != nil {
			return pz, soaLine, []string{atLine(line, fmt.Sprintf("more than one SOA record at the zone apex %s", apex))}
		}
		rec, soa := classify(rr, apex, line)
		if soa != nil {
			pz.SOA = soa
			soaLine = line
			continue
		}
		pz.Records = append(pz.Records, *rec)
	}

	if parseErr != nil {
		if line := errLine(parseErr); line > 0 {
			return pz, soaLine, []string{fmt.Sprintf("line %d: %s", line, withoutLine(parseErr))}
		}
		return pz, soaLine, []string{withoutLine(parseErr)}
	}
	if pz.SOA == nil {
		return pz, soaLine, []string{fmt.Sprintf("no SOA record found for zone apex %s", apex)}
	}
	return pz, soaLine, nil
}

// parseWithRecovery names the bad lines of a file parseClean has already
// rejected, and returns nothing else — see Parse's doc comment for why
// records are not among them.
func parseWithRecovery(text, apex, origin string) []string {
	var errs []string
	var seenSOA bool
	// Recovery expands every $GENERATE again, so it needs the same budget
	// parseClean has — without it, the file that made parseClean give up
	// would simply be expanded a second time to say so.
	budget := maxZoneFileRecords

	for _, stmt := range splitStatements(text, origin) {
		zp := dns.NewZoneParser(strings.NewReader(stmt.text), origin, "")

		for rr, ok := zp.Next(); ok; rr, ok = zp.Next() {
			if budget--; budget < 0 {
				return append(errs, fmt.Sprintf("line %d: %s", stmt.recordLine, tooManyRecords))
			}
			if rr.Header().Name == "" {
				// A continuation line, cut off from the owner it inherits
				// (RFC 1035 §5.1) by the very splitting that lets recovery
				// get past a bad statement. That is recovery's own doing,
				// not a defect in the file, and with no record to drop
				// there is nothing left to report about it.
				continue
			}
			if problem := recordProblem(rr, apex); problem != "" {
				errs = append(errs, fmt.Sprintf("line %d: %s", stmt.recordLine, problem))
				continue
			}
			if isApexSOA(rr, apex) {
				if seenSOA {
					errs = append(errs, fmt.Sprintf("line %d: more than one SOA record at the zone apex %s", stmt.recordLine, apex))
				}
				seenSOA = true
			}
		}

		if err := zp.Err(); err != nil {
			line := stmt.recordLine
			if rel := errLine(err); rel >= 1 && rel <= len(stmt.lineNumbers) {
				line = stmt.lineNumbers[rel-1]
			}
			errs = append(errs, fmt.Sprintf("line %d: %s", line, withoutLine(err)))
		}
	}

	return errs
}
