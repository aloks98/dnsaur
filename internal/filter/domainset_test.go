package filter

import "testing"

func TestDomainSetLabelBoundaries(t *testing.T) {
	s := NewDomainSet()
	s.Add("example.com")
	s.Add("Ads.Example.NET") // must normalize case
	cases := []struct {
		q  string
		ok bool
	}{
		{"example.com", true},
		{"a.b.example.com", true},
		{"notexample.com", false},
		{"ads.example.net", true},
		{"sub.ads.example.net", true},
		{"example.net", false},
		{"com", false},
	}
	for _, c := range cases {
		if _, ok := s.Match(c.q); ok != c.ok {
			t.Errorf("Match(%q)=%v want %v", c.q, ok, c.ok)
		}
	}
	if m, _ := s.Match("deep.example.com"); m != "example.com" {
		t.Errorf("matched=%q", m)
	}
	if s.Len() != 2 {
		t.Errorf("len %d", s.Len())
	}
}

func TestDomainSetNormalisesAndDeduplicates(t *testing.T) {
	s := NewDomainSet()
	s.Add("example.com")
	s.Add("EXAMPLE.com.")
	s.Add("example.com.")
	if s.Len() != 1 {
		t.Fatalf("Len=%d want 1, the same name added three ways", s.Len())
	}
	for _, q := range []string{"example.com", "example.com.", "EXAMPLE.COM", "Sub.Example.Com."} {
		if m, ok := s.Match(q); !ok || m != "example.com" {
			t.Errorf("Match(%q)=%q,%v want %q,true", q, m, ok, "example.com")
		}
	}
}

func TestDomainSetMatchEdges(t *testing.T) {
	s := NewDomainSet()
	s.Add("localhost")
	s.Add("ads.example.com")

	cases := []struct {
		q       string
		matched string
		ok      bool
	}{
		{"localhost", "localhost", true},
		{"a.localhost", "localhost", true},
		{"notlocalhost", "", false},
		{"ads.example.com", "ads.example.com", true},
		{"x.y.ads.example.com", "ads.example.com", true},
		{"example.com", "", false}, // a parent of an entry is not itself an entry
		{"com", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		m, ok := s.Match(c.q)
		if ok != c.ok || m != c.matched {
			t.Errorf("Match(%q)=%q,%v want %q,%v", c.q, m, ok, c.matched, c.ok)
		}
	}
}

// The empty name is a legal entry but must not act as a root wildcard: the
// suffix walk stops at the last label rather than falling through to "".
func TestDomainSetEmptyEntryIsNotAWildcard(t *testing.T) {
	s := NewDomainSet()
	s.Add("")
	if s.Len() != 1 {
		t.Fatalf("Len=%d want 1", s.Len())
	}
	if m, ok := s.Match(""); !ok || m != "" {
		t.Errorf("Match(%q)=%q,%v want %q,true", "", m, ok, "")
	}
	if m, ok := s.Match("example.com"); ok {
		t.Errorf("Match(%q)=%q,true, the empty entry swallowed a real name", "example.com", m)
	}
}

// Most specific wins: both entries sit on the path, the longer one answers.
func TestDomainSetMostSpecificWins(t *testing.T) {
	s := NewDomainSet()
	s.Add("example.com")
	s.Add("ads.example.com")
	if m, _ := s.Match("deep.ads.example.com"); m != "ads.example.com" {
		t.Errorf("matched=%q want %q", m, "ads.example.com")
	}
	if m, _ := s.Match("other.example.com"); m != "example.com" {
		t.Errorf("matched=%q want %q", m, "example.com")
	}
}
