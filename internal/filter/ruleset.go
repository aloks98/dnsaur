package filter

import (
	"log/slog"
	"regexp"
	"strings"

	"github.com/aloks98/dnsaur/internal/store"
)

type Verdict struct {
	Action  string // allow | block | none
	RuleID  int64
	ListID  int64
	Matched string
}

type CompiledList struct {
	ID   int64
	Kind string // block | allow
	Set  *DomainSet
	// Except holds a block list's own `@@||` exceptions, nil when it has
	// none. It excuses this list and no other: a domain one list's author
	// exempted is still blocked by the next list that names it.
	Except *DomainSet
}

type regexRule struct {
	id     int64
	action string
	re     *regexp.Regexp
}

type Ruleset struct {
	manualAllow   *DomainSet
	manualBlock   *DomainSet
	manualAllowID map[string]int64 // matched pattern -> rule id
	manualBlockID map[string]int64
	regexes       []regexRule
	lists         []CompiledList
}

func Compile(rules []store.Rule, lists []CompiledList) *Ruleset {
	rs := &Ruleset{
		manualAllow: NewDomainSet(), manualBlock: NewDomainSet(),
		manualAllowID: map[string]int64{}, manualBlockID: map[string]int64{},
		lists: lists,
	}
	for _, r := range rules {
		if r.IsRegex {
			re, err := regexp.Compile(r.Pattern)
			if err != nil {
				slog.Warn("skipping invalid regex rule", "id", r.ID, "pattern", r.Pattern, "err", err)
				continue
			}
			rs.regexes = append(rs.regexes, regexRule{id: r.ID, action: r.Action, re: re})
			continue
		}
		if r.Action == "allow" {
			rs.manualAllow.Add(r.Pattern)
			rs.manualAllowID[normalize(r.Pattern)] = r.ID
		} else {
			rs.manualBlock.Add(r.Pattern)
			rs.manualBlockID[normalize(r.Pattern)] = r.ID
		}
	}
	return rs
}

func normalize(d string) string { return strings.ToLower(strings.TrimSuffix(d, ".")) }

func (rs *Ruleset) Evaluate(qname string) Verdict {
	if m, ok := rs.manualAllow.Match(qname); ok {
		return Verdict{Action: "allow", RuleID: rs.manualAllowID[m], Matched: m}
	}
	for _, rr := range rs.regexes {
		if rr.action == "allow" && rr.re.MatchString(qname) {
			return Verdict{Action: "allow", RuleID: rr.id, Matched: rr.re.String()}
		}
	}
	if m, ok := rs.manualBlock.Match(qname); ok {
		return Verdict{Action: "block", RuleID: rs.manualBlockID[m], Matched: m}
	}
	for _, rr := range rs.regexes {
		if rr.action == "block" && rr.re.MatchString(qname) {
			return Verdict{Action: "block", RuleID: rr.id, Matched: rr.re.String()}
		}
	}
	for _, l := range rs.lists {
		if l.Kind != "allow" {
			continue
		}
		if m, ok := l.Set.Match(qname); ok {
			return Verdict{Action: "allow", ListID: l.ID, Matched: m}
		}
	}
	for _, l := range rs.lists {
		if l.Kind != "block" {
			continue
		}
		if l.Except != nil {
			if _, ok := l.Except.Match(qname); ok {
				continue
			}
		}
		if m, ok := l.Set.Match(qname); ok {
			return Verdict{Action: "block", ListID: l.ID, Matched: m}
		}
	}
	return Verdict{Action: "none"}
}
