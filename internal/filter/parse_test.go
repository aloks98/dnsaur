package filter

import (
	"strings"
	"testing"
)

func TestParseListAutoDetect(t *testing.T) {
	in := `# comment
! abp comment
0.0.0.0 ads.example.com
127.0.0.1 tracker.example.net
||abp-block.example^
@@||abp-allow.example^
plain.example.org
this is not a domain line
`
	res, err := ParseList(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	wantBlock := []string{"ads.example.com", "tracker.example.net", "abp-block.example", "plain.example.org"}
	if len(res.Block) != len(wantBlock) {
		t.Fatalf("block %v", res.Block)
	}
	for i, d := range wantBlock {
		if res.Block[i] != d {
			t.Fatalf("block[%d]=%q want %q", i, res.Block[i], d)
		}
	}
	if len(res.Allow) != 1 || res.Allow[0] != "abp-allow.example" {
		t.Fatalf("allow %v", res.Allow)
	}
	if res.Skipped != 1 {
		t.Fatalf("skipped %d", res.Skipped)
	}
}

// TestParseListWildcardLines covers hagezi's wildcard/* format, which is
// mainstream and which the parser used to reject line by line: validDomain's
// charset has no `*`, so a 4.5 MB file of `*.host.tld` produced zero entries
// and a silently-useless list. Only a *leading* `*.` is stripped — a star
// anywhere else is still not a domain.
func TestParseListWildcardLines(t *testing.T) {
	in := `*.analytics.004gmbh.de
*.cdn.007moms.com
0.0.0.0 *.ads.01film.cc
ads.*.example.com
*ads.example.net
*.com
*.*.doubled.example
`
	res, err := ParseList(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"analytics.004gmbh.de", "cdn.007moms.com", "ads.01film.cc"}
	if len(res.Block) != len(want) {
		t.Fatalf("block %v, want %v", res.Block, want)
	}
	for i, d := range want {
		if res.Block[i] != d {
			t.Fatalf("block[%d]=%q want %q", i, res.Block[i], d)
		}
	}
	// ads.*.example.com, *ads.example.net (star not its own label), *.com
	// (bare TLD once stripped) and *.*.doubled.example (only one leading
	// label is stripped) must all still be rejected.
	if res.Skipped != 4 {
		t.Fatalf("skipped %d, want 4", res.Skipped)
	}
}

// TestParseListWildcardKeepsABPGuard pins the boundary: stripping a leading
// `*.` for plain/hosts lines must not loosen abpDomain, which deliberately
// refuses ABP rules containing / ^ $ * | because their semantics aren't
// implemented.
func TestParseListWildcardKeepsABPGuard(t *testing.T) {
	res, err := ParseList(strings.NewReader("||*.starred.example^\n@@||*.starred.example^\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Block) != 0 || len(res.Allow) != 0 {
		t.Fatalf("ABP star rules leaked through: block=%v allow=%v", res.Block, res.Allow)
	}
	if res.Skipped != 2 {
		t.Fatalf("skipped %d, want 2", res.Skipped)
	}
}

// TestWildcardEntryBlocksApexAndSubdomains documents the one deliberate
// difference from strict AdGuard `*.x` semantics: the stripped entry covers
// the apex too, matching dnsmasq's address=/x/ reading. Both directions are
// asserted so a future "fix" to exclude the apex has to argue with a test.
func TestWildcardEntryBlocksApexAndSubdomains(t *testing.T) {
	res, err := ParseList(strings.NewReader("*.analytics.example.com\n"))
	if err != nil {
		t.Fatal(err)
	}
	set := NewDomainSet()
	for _, d := range res.Block {
		set.Add(d)
	}
	for _, q := range []string{"analytics.example.com", "deep.sub.analytics.example.com"} {
		if _, ok := set.Match(q); !ok {
			t.Fatalf("%q not matched by *.analytics.example.com", q)
		}
	}
	if _, ok := set.Match("example.com"); ok {
		t.Fatal("parent domain example.com must not be matched")
	}
}

// TestParseListSkipsTheBOM: a UTF-8 BOM is not whitespace, so it used to
// glue itself to the first line — the first entry of a BOM-prefixed list was
// silently dropped, and a BOM-prefixed comment stopped being a comment.
func TestParseListSkipsTheBOM(t *testing.T) {
	res, err := ParseList(strings.NewReader("\uFEFF0.0.0.0 first.example.com\n0.0.0.0 second.example.com\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Block) != 2 || res.Block[0] != "first.example.com" {
		t.Fatalf("block = %v, want the first line kept", res.Block)
	}
	res, err = ParseList(strings.NewReader("\uFEFF# a commented first line\n"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Skipped != 0 || len(res.Block) != 0 {
		t.Fatalf("BOM-prefixed comment was parsed as an entry: %+v", res)
	}
}

// TestParseListConvertsIDN: entries arrive in Unicode, queries arrive as
// punycode. Rejecting the Unicode spelling on the ASCII charset meant a list
// naming пример.рф never blocked xn--e1afmkfd.xn--p1ai.
func TestParseListConvertsIDN(t *testing.T) {
	res, err := ParseList(strings.NewReader("0.0.0.0 пример.рф\n||münchen.example^\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"xn--e1afmkfd.xn--p1ai", "xn--mnchen-3ya.example"}
	if len(res.Block) != len(want) {
		t.Fatalf("block = %v, want %v", res.Block, want)
	}
	for i, d := range want {
		if res.Block[i] != d {
			t.Fatalf("block[%d] = %q, want %q", i, res.Block[i], d)
		}
	}
}

// TestParseListSkipsOverlongLines: one absurd line used to abort the scanner
// with ErrTooLong, which failed the whole list — a million usable entries
// discarded because of one. It counts as skipped, and parsing continues.
func TestParseListSkipsOverlongLines(t *testing.T) {
	var b strings.Builder
	b.WriteString("0.0.0.0 before.example.com\n")
	b.WriteString(strings.Repeat("x", maxLineBytes+10))
	b.WriteString("\n0.0.0.0 after.example.com\n")

	res, err := ParseList(strings.NewReader(b.String()))
	if err != nil {
		t.Fatalf("one long line failed the whole list: %v", err)
	}
	if len(res.Block) != 2 || res.Block[1] != "after.example.com" {
		t.Fatalf("block = %v, want both short lines", res.Block)
	}
	if res.Skipped != 1 {
		t.Fatalf("skipped = %d, want 1", res.Skipped)
	}
}

// TestParseListLineShapes pins the formats a list may arrive in: CRLF from a
// Windows-authored file, a comment after an entry, the `::`/`::1` hosts
// prefixes, `||domain` without the trailing `^`, ABP `$`-modifier lines
// (unsupported, skipped), and uppercase normalisation.
func TestParseListLineShapes(t *testing.T) {
	in := "0.0.0.0 CRLF.Example.com\r\n" +
		"0.0.0.0 mid.example.com # why it is here\r\n" +
		":: six.example.com\n" +
		"::1 sixone.example.com\n" +
		"||nocaret.example\n" +
		"||mod.example^$third-party\n"
	res, err := ParseList(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"crlf.example.com", "mid.example.com", "six.example.com", "sixone.example.com", "nocaret.example"}
	if len(res.Block) != len(want) {
		t.Fatalf("block = %v, want %v", res.Block, want)
	}
	for i, d := range want {
		if res.Block[i] != d {
			t.Fatalf("block[%d] = %q, want %q", i, res.Block[i], d)
		}
	}
	if res.Skipped != 1 {
		t.Fatalf("skipped = %d, want 1 (the $-modifier rule)", res.Skipped)
	}
}

// TestRulePattern is what a manual rule is validated against: the same label
// rules a list entry gets, plus single labels, minus the "must contain a
// dot" requirement — and with the leading `*.` stripped, because stored
// verbatim it becomes a label the trie can never match.
func TestRulePattern(t *testing.T) {
	for _, tc := range []struct {
		in, want string
	}{
		{"doubleclick.net", "doubleclick.net"},
		{"*.doubleclick.net", "doubleclick.net"},
		{"Ads.Example.COM.", "ads.example.com"},
		{"localhost", "localhost"},
		{"пример.рф", "xn--e1afmkfd.xn--p1ai"},
	} {
		got, ok := RulePattern(tc.in)
		if !ok || got != tc.want {
			t.Errorf("RulePattern(%q) = %q, %v; want %q, true", tc.in, got, ok, tc.want)
		}
	}
	for _, in := range []string{"", "||x^", "ads.*.example.com", "*", "a..b", "has space.com", strings.Repeat("x", 64) + ".com"} {
		if got, ok := RulePattern(in); ok {
			t.Errorf("RulePattern(%q) = %q, true; want it rejected", in, got)
		}
	}
}
