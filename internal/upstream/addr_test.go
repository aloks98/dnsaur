package upstream

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
)

type grammarFixture struct {
	Accept []struct {
		In, Scheme, Addr, VerifyName, Path, Canonical string
	} `json:"accept"`
	Reject []struct {
		In, Code string
	} `json:"reject"`
	AcceptList []struct {
		In    string
		Count int
	} `json:"acceptList"`
	RejectList []struct {
		In, Code string
	} `json:"rejectList"`
}

func loadGrammar(t *testing.T) grammarFixture {
	t.Helper()
	b, err := os.ReadFile("testdata/grammar.json")
	if err != nil {
		t.Fatalf("reading the grammar fixture: %v", err)
	}
	var f grammarFixture
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatalf("parsing the grammar fixture: %v", err)
	}
	return f
}

// code returns the ParseError code, or "" when err is not one. Comparing
// codes rather than messages is what lets web/src/lib/upstreams.ts assert
// the same rejections against the same fixture with its own copy.
func code(err error) string {
	var pe *ParseError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

func TestParseUpstreamsAccepts(t *testing.T) {
	for _, c := range loadGrammar(t).Accept {
		t.Run(c.In, func(t *testing.T) {
			got, err := ParseUpstreams(c.In)
			if err != nil {
				t.Fatalf("ParseUpstreams(%q): %v", c.In, err)
			}
			if len(got) != 1 {
				t.Fatalf("ParseUpstreams(%q) returned %d entries, want 1", c.In, len(got))
			}
			u := got[0]
			if string(u.Scheme) != c.Scheme || u.Addr != c.Addr ||
				u.VerifyName != c.VerifyName || u.Path != c.Path || u.Canonical != c.Canonical {
				t.Errorf("ParseUpstreams(%q) =\n  scheme=%q addr=%q name=%q path=%q canonical=%q\nwant\n  scheme=%q addr=%q name=%q path=%q canonical=%q",
					c.In, u.Scheme, u.Addr, u.VerifyName, u.Path, u.Canonical,
					c.Scheme, c.Addr, c.VerifyName, c.Path, c.Canonical)
			}
		})
	}
}

func TestParseUpstreamsRejects(t *testing.T) {
	f := loadGrammar(t)
	cases := append(append([]struct{ In, Code string }{}, f.Reject...), f.RejectList...)
	for _, c := range cases {
		t.Run(c.In, func(t *testing.T) {
			_, err := ParseUpstreams(c.In)
			if err == nil {
				t.Fatalf("ParseUpstreams(%q) succeeded, want rejection %q", c.In, c.Code)
			}
			if got := code(err); got != c.Code {
				t.Errorf("ParseUpstreams(%q) code = %q, want %q (message: %v)", c.In, got, c.Code, err)
			}
		})
	}
}

func TestParseUpstreamsLists(t *testing.T) {
	for _, c := range loadGrammar(t).AcceptList {
		t.Run(c.In, func(t *testing.T) {
			got, err := ParseUpstreams(c.In)
			if err != nil {
				t.Fatalf("ParseUpstreams(%q): %v", c.In, err)
			}
			if len(got) != c.Count {
				t.Errorf("ParseUpstreams(%q) returned %d entries, want %d", c.In, len(got), c.Count)
			}
		})
	}
}

// Canonical is the identity *up is reused by, so parsing it again has to
// produce the same upstream — otherwise two spellings of one server would
// keep separate health records after a settings round trip.
func TestCanonicalRoundTrips(t *testing.T) {
	for _, c := range loadGrammar(t).Accept {
		t.Run(c.In, func(t *testing.T) {
			first, err := ParseUpstreams(c.In)
			if err != nil {
				t.Fatalf("ParseUpstreams(%q): %v", c.In, err)
			}
			again, err := ParseUpstreams(strings.Join([]string{first[0].Canonical}, ","))
			if err != nil {
				t.Fatalf("re-parsing %q: %v", first[0].Canonical, err)
			}
			if again[0] != first[0] {
				t.Errorf("round trip changed the upstream:\n first = %+v\n again = %+v", first[0], again[0])
			}
		})
	}
}
