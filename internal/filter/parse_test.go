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
