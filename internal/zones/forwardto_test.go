package zones_test

import (
	"strings"
	"testing"

	"github.com/aloks98/dnsaur/internal/zones"
)

func TestParseForwardTo(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []zones.ForwardTarget
	}{
		{"empty forwards nowhere", "", nil},
		{"a bare address takes the default port", "10.0.0.1", []zones.ForwardTarget{{Host: "10.0.0.1", Port: 53}}},
		{"an explicit port is kept", "10.0.0.1:5353", []zones.ForwardTarget{{Host: "10.0.0.1", Port: 5353}}},
		{"a hostname is stored as written", "ns.corp.example", []zones.ForwardTarget{{Host: "ns.corp.example", Port: 53}}},
		{"a bare IPv6 literal is the no-port form", "fd00::2", []zones.ForwardTarget{{Host: "fd00::2", Port: 53}}},
		{"a bracketed IPv6 literal may carry a port", "[fd00::2]:5353", []zones.ForwardTarget{{Host: "fd00::2", Port: 5353}}},
		{
			"several, in order",
			"10.0.0.1, 10.0.0.2:5353, ns.corp.example",
			[]zones.ForwardTarget{
				{Host: "10.0.0.1", Port: 53},
				{Host: "10.0.0.2", Port: 5353},
				{Host: "ns.corp.example", Port: 53},
			},
		},
		{"whitespace around separators is tolerated", "  10.0.0.1 ,  10.0.0.2  ", []zones.ForwardTarget{{Host: "10.0.0.1", Port: 53}, {Host: "10.0.0.2", Port: 53}}},
		{"a trailing comma names no target", "10.0.0.1,", []zones.ForwardTarget{{Host: "10.0.0.1", Port: 53}}},
		{"a list of only separators is valid and empty", " , , ", nil},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := zones.ParseForwardTo(tc.in)
			if err != nil {
				t.Fatalf("ParseForwardTo(%q) failed: %v", tc.in, err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("ParseForwardTo(%q) = %+v, want %+v", tc.in, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("entry %d = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestParseForwardToRejects(t *testing.T) {
	tests := []struct{ name, in, contains string }{
		{"port zero", "10.0.0.1:0", "port"},
		{"port above the range", "10.0.0.1:70000", "port"},
		{"a non-numeric port", "10.0.0.1:dns", "port"},
		// These two are the only inputs that reach validPrimaryHost: anything
		// with an internal space is rejected as a whole-field parse failure
		// first, so a multi-word input would exercise a different branch while
		// still producing a message containing "host".
		{"a host with a forbidden character", "a/b", "host"},
		{"a host with an empty label", "a..b", "host"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := zones.ParseForwardTo(tc.in)
			if err == nil {
				t.Fatalf("ParseForwardTo(%q) succeeded, want an error", tc.in)
			}
			if !strings.Contains(err.Error(), tc.contains) {
				t.Errorf("error %q does not mention %q", err, tc.contains)
			}
			// Validate is Parse's error alone; the two must never disagree
			// about what is writable, or the API would accept a value the
			// router then cannot parse.
			if zones.ValidateForwardTo(tc.in) == nil {
				t.Errorf("ValidateForwardTo(%q) accepted what ParseForwardTo rejected", tc.in)
			}
		})
	}
}

// The stored form is FormatForwardTo's spelling rather than what was typed,
// so the value read back is the value the router will parse.
func TestFormatForwardToIsCanonicalAndRoundTrips(t *testing.T) {
	tests := []struct{ in, want string }{
		{"", ""},
		{"10.0.0.1", "10.0.0.1:53"},
		{"10.0.0.1:5353", "10.0.0.1:5353"},
		{"  10.0.0.1 ,10.0.0.2 ", "10.0.0.1:53, 10.0.0.2:53"},
		{"fd00::2", "[fd00::2]:53"},
		{"ns.corp.example", "ns.corp.example:53"},
	}

	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			ts, err := zones.ParseForwardTo(tc.in)
			if err != nil {
				t.Fatalf("ParseForwardTo(%q) failed: %v", tc.in, err)
			}
			got := zones.FormatForwardTo(ts)
			if got != tc.want {
				t.Fatalf("FormatForwardTo = %q, want %q", got, tc.want)
			}
			again, err := zones.ParseForwardTo(got)
			if err != nil {
				t.Fatalf("re-parsing %q failed: %v", got, err)
			}
			if second := zones.FormatForwardTo(again); second != got {
				t.Errorf("not a fixed point: %q then %q", got, second)
			}
		})
	}
}

// Addr is what reaches the routing table, so it must be dialable.
func TestForwardTargetAddr(t *testing.T) {
	tests := []struct{ in, want string }{
		{"10.0.0.1", "10.0.0.1:53"},
		{"10.0.0.1:5353", "10.0.0.1:5353"},
		{"fd00::2", "[fd00::2]:53"},
		{"ns.corp.example:5353", "ns.corp.example:5353"},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			ts, err := zones.ParseForwardTo(tc.in)
			if err != nil {
				t.Fatalf("ParseForwardTo(%q): %v", tc.in, err)
			}
			if got := ts[0].Addr(); got != tc.want {
				t.Errorf("Addr() = %q, want %q", got, tc.want)
			}
		})
	}
}
