package filter

import (
	"strings"
	"testing"

	"github.com/aloks98/dnsaur/internal/store"
)

func compiledList(id int64, kind string, domains ...string) CompiledList {
	s := NewDomainSet()
	for _, d := range domains {
		s.Add(d)
	}
	return CompiledList{ID: id, Kind: kind, Set: s}
}

func TestPrecedence(t *testing.T) {
	rs := Compile(
		[]store.Rule{
			{ID: 1, Action: "allow", Pattern: "ok.blocked.example"},
			{ID: 2, Action: "block", Pattern: "manual-block.example"},
			{ID: 3, Action: "block", Pattern: `^ad[0-9]+\.regex\.example$`, IsRegex: true},
			{ID: 4, Action: "block", Pattern: "localhost"},
		},
		[]CompiledList{
			compiledList(10, "block", "blocked.example", "manual-block.example", "listallow.example"),
			compiledList(11, "allow", "listallow.example"),
		},
	)
	cases := []struct {
		q      string
		action string
		ruleID int64
		listID int64
	}{
		{"sub.blocked.example", "block", 0, 10},
		{"ok.blocked.example", "allow", 1, 0},   // manual allow beats list block
		{"manual-block.example", "block", 2, 0}, // manual block beats list block attribution
		{"listallow.example", "allow", 0, 11},   // list allow beats list block
		{"ad42.regex.example", "block", 3, 0},
		{"localhost", "block", 4, 0},
		{"clean.example", "none", 0, 0},
	}
	for _, c := range cases {
		v := rs.Evaluate(c.q)
		if v.Action != c.action || v.RuleID != c.ruleID || v.ListID != c.listID {
			t.Errorf("Evaluate(%q) = %+v, want action=%s rule=%d list=%d", c.q, v, c.action, c.ruleID, c.listID)
		}
	}
}

// TestPrecedenceRulesBeatLists covers the two crossings the table above
// leaves out, both of which are the documented promise that a rule always
// beats a list: a block rule over an allow list, and a regex on either side.
func TestPrecedenceRulesBeatLists(t *testing.T) {
	rs := Compile(
		[]store.Rule{
			{ID: 1, Action: "block", Pattern: "hardblock.example"},
			{ID: 2, Action: "allow", Pattern: `^good[0-9]+\.blocked\.example$`, IsRegex: true},
			{ID: 3, Action: "block", Pattern: `^bad[0-9]+\.listallowed\.example$`, IsRegex: true},
		},
		[]CompiledList{
			compiledList(10, "block", "blocked.example"),
			compiledList(11, "allow", "listallowed.example", "hardblock.example"),
		},
	)
	cases := []struct {
		q, action string
		ruleID    int64
		listID    int64
	}{
		{"hardblock.example", "block", 1, 0},          // block rule beats an allow list
		{"good7.blocked.example", "allow", 2, 0},      // regex allow beats a block list
		{"bad7.listallowed.example", "block", 3, 0},   // regex block beats an allow list
		{"other.listallowed.example", "allow", 0, 11}, // and the allow list still applies
		{"deep.sub.blocked.example", "block", 0, 10},
		{"unrelated.example", "none", 0, 0},
	}
	for _, c := range cases {
		v := rs.Evaluate(c.q)
		if v.Action != c.action || v.RuleID != c.ruleID || v.ListID != c.listID {
			t.Errorf("Evaluate(%q) = %+v, want action=%s rule=%d list=%d", c.q, v, c.action, c.ruleID, c.listID)
		}
	}
}

// TestCompileSkipsAnInvalidRegex: one unparseable pattern must cost its own
// rule and nothing else, which is what the dashboard tells people.
func TestCompileSkipsAnInvalidRegex(t *testing.T) {
	rs := Compile([]store.Rule{
		{ID: 1, Action: "block", Pattern: "ad[", IsRegex: true},
		{ID: 2, Action: "block", Pattern: `^ads\.example$`, IsRegex: true},
	}, nil)
	if v := rs.Evaluate("ads.example"); v.Action != "block" || v.RuleID != 2 {
		t.Fatalf("Evaluate = %+v, want the valid regex still enforcing", v)
	}
}

// TestListExceptionsExcuseTheirOwnList: a block list's `@@||` lines are its
// author's own carve-outs. Dropping them let subdomain semantics block
// exactly what the list said to spare — and the carve-out is scoped to that
// list, so a second list naming the same host still blocks it.
func TestListExceptionsExcuseTheirOwnList(t *testing.T) {
	res, err := ParseList(strings.NewReader("||example.com^\n@@||cdn.example.com^\n"))
	if err != nil {
		t.Fatal(err)
	}
	withException := compileList(store.List{ID: 10, Kind: "block"}, res)

	rs := Compile(nil, []CompiledList{withException})
	if v := rs.Evaluate("cdn.example.com"); v.Action == "block" {
		t.Fatalf("Evaluate(cdn.example.com) = %+v, want the list's own exception honoured", v)
	}
	if v := rs.Evaluate("assets.cdn.example.com"); v.Action == "block" {
		t.Fatalf("the exception must cover subdomains too: %+v", v)
	}
	if v := rs.Evaluate("ads.example.com"); v.Action != "block" || v.ListID != 10 {
		t.Fatalf("Evaluate(ads.example.com) = %+v, want the list still blocking", v)
	}

	rs = Compile(nil, []CompiledList{withException, compiledList(11, "block", "cdn.example.com")})
	if v := rs.Evaluate("cdn.example.com"); v.Action != "block" || v.ListID != 11 {
		t.Fatalf("Evaluate = %+v, want list 11 still blocking what list 10 excused", v)
	}
}

// TestAllowKindListTakesBothSides: a plain domains file subscribed as an
// allowlist parses into Block, and an ABP one into Allow. Both have to
// allow.
func TestAllowKindListTakesBothSides(t *testing.T) {
	res, err := ParseList(strings.NewReader("plain.example.com\n@@||abp.example.com^\n"))
	if err != nil {
		t.Fatal(err)
	}
	allow := compileList(store.List{ID: 20, Kind: "allow"}, res)
	rs := Compile(nil, []CompiledList{allow, compiledList(21, "block", "plain.example.com", "abp.example.com")})
	for _, q := range []string{"plain.example.com", "abp.example.com"} {
		if v := rs.Evaluate(q); v.Action != "allow" || v.ListID != 20 {
			t.Errorf("Evaluate(%q) = %+v, want allow by list 20", q, v)
		}
	}
}
