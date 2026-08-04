package filter

import "strings"

type dsNode struct {
	children map[string]*dsNode
	terminal bool
}

type DomainSet struct {
	root *dsNode
	n    int
}

func NewDomainSet() *DomainSet {
	return &DomainSet{root: &dsNode{children: map[string]*dsNode{}}}
}

func (s *DomainSet) Add(domain string) {
	labels := splitRev(strings.ToLower(strings.TrimSuffix(domain, ".")))
	cur := s.root
	for _, lbl := range labels {
		next, ok := cur.children[lbl]
		if !ok {
			next = &dsNode{children: map[string]*dsNode{}}
			cur.children[lbl] = next
		}
		cur = next
	}
	if !cur.terminal {
		cur.terminal = true
		s.n++
	}
}

// Match returns the most-specific entry that is qname or a parent
// domain of qname, matching only on whole-label boundaries.
func (s *DomainSet) Match(qname string) (string, bool) {
	labels := splitRev(strings.ToLower(strings.TrimSuffix(qname, ".")))
	cur := s.root
	matched := ""
	found := false
	for i, lbl := range labels {
		next, ok := cur.children[lbl]
		if !ok {
			break
		}
		cur = next
		if cur.terminal {
			matched = strings.Join(reverse(labels[:i+1]), ".")
			found = true
		}
	}
	return matched, found
}

func (s *DomainSet) Len() int { return s.n }

func splitRev(d string) []string { return reverse(strings.Split(d, ".")) }

func reverse(in []string) []string {
	out := make([]string, len(in))
	for i, v := range in {
		out[len(in)-1-i] = v
	}
	return out
}
