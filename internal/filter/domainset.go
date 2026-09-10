package filter

import "strings"

// DomainSet holds normalised domain names and matches on whole-label
// boundaries: an entry matches itself and every subdomain of it.
//
// It is one flat map rather than a per-label trie because a blocklist is
// mostly leaves, and a trie pays for a Go map at every node on the way down:
// at 766k entries that is 107 MB live and 226 MB allocated while building,
// against 32 MB and 56 MB here. Matching is a suffix walk instead of a
// descent, which also drops the per-lookup allocations — and a query runs one
// per set, over `2 + len(lists)` sets.
type DomainSet struct {
	m map[string]struct{}
}

func NewDomainSet() *DomainSet { return &DomainSet{m: map[string]struct{}{}} }

func (s *DomainSet) Add(domain string) { s.m[normalize(domain)] = struct{}{} }

// Match returns the most-specific entry that is qname or a parent
// domain of qname, matching only on whole-label boundaries.
func (s *DomainSet) Match(qname string) (string, bool) {
	// Walking outwards from the full name means the first hit is the most
	// specific one, so there is nothing to compare afterwards. Each step
	// reslices past a label separator: no allocation, and `notexample.com`
	// never reaches `example.com` because the walk only lands on boundaries.
	for q := normalize(qname); ; {
		if _, ok := s.m[q]; ok {
			return q, true
		}
		i := strings.IndexByte(q, '.')
		if i < 0 {
			return "", false
		}
		q = q[i+1:]
	}
}

func (s *DomainSet) Len() int { return len(s.m) }
