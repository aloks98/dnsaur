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
