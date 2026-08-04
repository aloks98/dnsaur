package filter

import (
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
