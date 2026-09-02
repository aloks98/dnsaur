package zones_test

import (
	"strings"
	"testing"

	"github.com/aloks98/dnsaur/internal/zones"
)

func TestParseNotifyTo(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []zones.NotifyTarget
	}{
		{
			name: "empty means notify nobody",
			in:   "",
			want: nil,
		},
		{
			name: "a bare address takes the default port",
			in:   "10.0.0.2",
			want: []zones.NotifyTarget{{Host: "10.0.0.2", Port: 53}},
		},
		{
			name: "an explicit port is kept",
			in:   "10.0.0.2:5353",
			want: []zones.NotifyTarget{{Host: "10.0.0.2", Port: 5353}},
		},
		{
			name: "a hostname is stored as written, not resolved",
			in:   "ns2.example.com",
			want: []zones.NotifyTarget{{Host: "ns2.example.com", Port: 53}},
		},
		{
			name: "a bare IPv6 literal is the no-port form",
			in:   "fd00::2",
			want: []zones.NotifyTarget{{Host: "fd00::2", Port: 53}},
		},
		{
			name: "a bracketed IPv6 literal may carry a port",
			in:   "[fd00::2]:5353",
			want: []zones.NotifyTarget{{Host: "fd00::2", Port: 5353}},
		},
		{
			name: "a key is canonicalised, lowercase with a trailing dot",
			in:   "10.0.0.2 key:NS2-Xfer",
			want: []zones.NotifyTarget{{Host: "10.0.0.2", Port: 53, Key: "ns2-xfer."}},
		},
		{
			name: "host, port and key together",
			in:   "ns2.hel1.example.com:5353 key:hetzner-xfer",
			want: []zones.NotifyTarget{{Host: "ns2.hel1.example.com", Port: 5353, Key: "hetzner-xfer."}},
		},
		{
			name: "several entries, only some keyed",
			in:   "10.0.0.2 key:ns2-xfer, ns3.example.com:5353, 10.0.0.3",
			want: []zones.NotifyTarget{
				{Host: "10.0.0.2", Port: 53, Key: "ns2-xfer."},
				{Host: "ns3.example.com", Port: 5353},
				{Host: "10.0.0.3", Port: 53},
			},
		},
		{
			name: "whitespace around separators is tolerated",
			in:   "  10.0.0.2   ,   10.0.0.3  ",
			want: []zones.NotifyTarget{{Host: "10.0.0.2", Port: 53}, {Host: "10.0.0.3", Port: 53}},
		},
		{
			// Skipped rather than rejected, exactly as splitPrimaries and
			// ParseACL do: a trailing comma names no target, so there is
			// nothing to be wrong about. Unlike primaries, a list that is
			// only separators is *valid* here and means notify nobody —
			// primaries requires at least one entry because a secondary
			// with none cannot transfer, and a zone with no notify targets
			// is the ordinary case.
			name: "a trailing comma names no target",
			in:   "10.0.0.2,",
			want: []zones.NotifyTarget{{Host: "10.0.0.2", Port: 53}},
		},
		{
			name: "a list of only separators is valid and empty",
			in:   " , , ",
			want: nil,
		},
		{
			// A comma is *always* the entry separator and can never be part
			// of a key name, because ParseNotifyTo splits on it before any
			// entry is parsed. So this is two targets, not one malformed
			// one — surprising written down, and exactly the property
			// ParseACL already has.
			//
			// It is also why validNotifyKeyName still forbids ',' even
			// though one can never reach it: the check fails closed rather
			// than open if this splitting ever changes. acl.go's
			// validACLKeyName carries the same guard for the same reason.
			name: "a comma separates entries even where a key name looks split",
			in:   "10.0.0.2 key:ns,2",
			want: []zones.NotifyTarget{
				{Host: "10.0.0.2", Port: 53, Key: "ns."},
				{Host: "2", Port: 53},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := zones.ParseNotifyTo(tc.in)
			if err != nil {
				t.Fatalf("ParseNotifyTo(%q) failed: %v", tc.in, err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("ParseNotifyTo(%q) = %+v, want %+v", tc.in, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("entry %d = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestParseNotifyToRejects(t *testing.T) {
	tests := []struct {
		name string
		in   string
		// The substring the message must carry, so a caller reading a 400
		// learns which entry was wrong rather than that "something" was.
		contains string
	}{
		{"port zero", "10.0.0.2:0", "port"},
		{"port above the range", "10.0.0.2:70000", "port"},
		{"a non-numeric port", "10.0.0.2:dns", "port"},
		// These two are the only cases that reach validPrimaryHost. Anything
		// with an internal space is claimed by the key branch first, so a
		// multi-word input would exercise that branch instead while still
		// producing a message containing "host" — passing for the wrong
		// reason and leaving the host validator with no coverage at all.
		{"a host with a forbidden character", "a/b", "host"},
		{"a host with an empty label", "a..b", "host"},
		// Named for what it actually tests: the token after the host is not
		// a key, so the key branch rejects it before any host check.
		{"a second token that is not a key", "not a host", "key"},
		{"an empty key name", "10.0.0.2 key:", "key"},
		{"a key name with a space", "10.0.0.2 key:ns 2", "key"},
		{"a key name with a colon", "10.0.0.2 key:ns:2", "key"},
		{"two keys on one entry", "10.0.0.2 key:a key:b", "key"},
		{"a bare key with no host", "key:ns2-xfer", "host"},
		{"trailing junk after the key", "10.0.0.2 key:ns2 extra", "key"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := zones.ParseNotifyTo(tc.in)
			if err == nil {
				t.Fatalf("ParseNotifyTo(%q) succeeded, want an error", tc.in)
			}
			if !strings.Contains(err.Error(), tc.contains) {
				t.Errorf("error %q does not mention %q", err, tc.contains)
			}
			// ValidateNotifyTo is ParseNotifyTo's error alone — the two must
			// never disagree about what is writable, or the API would accept
			// a value the sender then cannot parse.
			if zones.ValidateNotifyTo(tc.in) == nil {
				t.Errorf("ValidateNotifyTo(%q) accepted what ParseNotifyTo rejected", tc.in)
			}
		})
	}
}

// The stored form is FormatNotifyTo's spelling rather than what was typed,
// which is what lets the tsig_keys delete guard match key:<name> in SQL
// exactly (see tsigKeyStore.Delete) instead of pattern-matching whitespace.
func TestFormatNotifyToIsCanonicalAndRoundTrips(t *testing.T) {
	tests := []struct{ in, want string }{
		{"", ""},
		{"10.0.0.2", "10.0.0.2:53"},
		{"10.0.0.2:5353", "10.0.0.2:5353"},
		{"  10.0.0.2   ,10.0.0.3 ", "10.0.0.2:53, 10.0.0.3:53"},
		{"10.0.0.2 key:NS2-Xfer", "10.0.0.2:53 key:ns2-xfer."},
		{"fd00::2", "[fd00::2]:53"},
		{"ns2.example.com key:k", "ns2.example.com:53 key:k."},
	}

	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			ts, err := zones.ParseNotifyTo(tc.in)
			if err != nil {
				t.Fatalf("ParseNotifyTo(%q) failed: %v", tc.in, err)
			}
			got := zones.FormatNotifyTo(ts)
			if got != tc.want {
				t.Fatalf("FormatNotifyTo = %q, want %q", got, tc.want)
			}
			// Canonical means a fixed point: re-parsing and re-formatting the
			// stored value must not move it, or a PATCH that changed nothing
			// would still rewrite the column.
			again, err := zones.ParseNotifyTo(got)
			if err != nil {
				t.Fatalf("re-parsing %q failed: %v", got, err)
			}
			if second := zones.FormatNotifyTo(again); second != got {
				t.Errorf("not a fixed point: %q then %q", got, second)
			}
		})
	}
}

// Addr is the queue's row identity, so it must be stable and must never
// include the key — re-keying a target keeps its delivery history rather
// than orphaning it and starting a new row.
func TestNotifyTargetAddr(t *testing.T) {
	tests := []struct{ in, want string }{
		{"10.0.0.2", "10.0.0.2:53"},
		{"10.0.0.2:5353", "10.0.0.2:5353"},
		{"10.0.0.2 key:ns2-xfer", "10.0.0.2:53"},
		{"fd00::2", "[fd00::2]:53"},
		{"[fd00::2]:5353 key:k", "[fd00::2]:5353"},
		{"ns2.example.com:5353", "ns2.example.com:5353"},
	}

	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			ts, err := zones.ParseNotifyTo(tc.in)
			if err != nil {
				t.Fatalf("ParseNotifyTo(%q) failed: %v", tc.in, err)
			}
			if got := ts[0].Addr(); got != tc.want {
				t.Errorf("Addr() = %q, want %q", got, tc.want)
			}
		})
	}
}

// The delete guard and the TSIG keys screen's usage count both read this.
func TestNotifyToKeys(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{"none", "10.0.0.2, 10.0.0.3", nil},
		{"one", "10.0.0.2 key:ns2-xfer", []string{"ns2-xfer."}},
		{
			"several, in order, canonical",
			"10.0.0.2 key:B, 10.0.0.3, ns4.example.com key:a",
			[]string{"b.", "a."},
		},
		{
			// Fails closed, mirroring ACLKeys: an unparseable stored value
			// names no key. Both callers — a write's validation and a key's
			// usage count — already have their own account of the error, and
			// this has nothing to add to it.
			"an unparseable value names none",
			"10.0.0.2 key:ns 2",
			nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := zones.NotifyToKeys(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("NotifyToKeys(%q) = %v, want %v", tc.in, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("key %d = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}
