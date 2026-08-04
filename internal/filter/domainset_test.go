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
