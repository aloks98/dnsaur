package zones_test

import (
	"net/netip"
	"testing"

	"github.com/aloks98/dnsaur/internal/zones"
)

func TestParseACL(t *testing.T) {
	for _, tc := range []struct {
		name    string
		in      string
		want    []string // FormatACL of the result
		wantErr bool
	}{
		{name: "empty is deny, not an error", in: "", want: nil},
		{name: "cidr", in: "10.0.0.0/24", want: []string{"10.0.0.0/24"}},
		{name: "bare v4 is a host route", in: "192.168.1.5", want: []string{"192.168.1.5"}},
		{name: "bare v6 is a host route", in: "2001:db8::5", want: []string{"2001:db8::5"}},
		{name: "v6 cidr", in: "2001:db8::/64", want: []string{"2001:db8::/64"}},
		{name: "key entry is canonicalised", in: "key:NS2", want: []string{"key:ns2."}},
		{name: "key prefix is case insensitive", in: "KEY:ns2", want: []string{"key:ns2."}},
		{name: "mixed list, whitespace tolerated", in: " 10.0.0.0/24 , key:ns2 , 192.168.1.5 ",
			want: []string{"10.0.0.0/24", "key:ns2.", "192.168.1.5"}},
		{name: "trailing comma is skipped", in: "10.0.0.0/24,", want: []string{"10.0.0.0/24"}},
		{name: "only separators is empty, not an error", in: " , , ", want: nil},
		{name: "bad mask", in: "10.0.0.0/33", wantErr: true},
		{name: "not an address", in: "not-an-ip", wantErr: true},
		{name: "hostname is refused, not resolved", in: "ns2.example.com", wantErr: true},
		{name: "empty key name", in: "key:", wantErr: true},
		{name: "key name with a space", in: "key:ns 2", wantErr: true},
		// A bare v4-mapped address names one host unambiguously, so it is
		// unmapped to that host's canonical spelling rather than rejected.
		{name: "v4-mapped bare address unmaps to its IPv4 spelling", in: "::ffff:10.0.0.5", want: []string{"10.0.0.5"}},
		// A v4-mapped *prefix* is refused rather than silently rewritten:
		// ::ffff:10.0.0.0/120 and 10.0.0.0/24 are the same range in two
		// spellings, and unmapping one into the other would mean the value
		// read back is not the value written.
		{name: "v4-mapped prefix is refused, not silently rewritten", in: "::ffff:10.0.0.0/120", wantErr: true},
		// An explicit /128 on a mapped address takes the same prefix branch
		// as any other CIDR, so it must be refused too, not treated as the
		// bare-address case above.
		{name: "v4-mapped /128 prefix is refused too", in: "::ffff:10.0.0.5/128", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := zones.ParseACL(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseACL(%q) = %v, want error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseACL(%q): %v", tc.in, err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("ParseACL(%q) gave %d entries, want %d", tc.in, len(got), len(tc.want))
			}
			for i, w := range tc.want {
				if one := zones.FormatACL(got[i : i+1]); one != w {
					t.Errorf("entry %d = %q, want %q", i, one, w)
				}
			}
			// ValidateACL must agree with ParseACL on every input, or the
			// write-time check and the request-time parse disagree about what
			// is storable — which is a zone that saves and never matches.
			if err := zones.ValidateACL(tc.in); err != nil {
				t.Errorf("ValidateACL(%q) = %v, want nil", tc.in, err)
			}
		})
	}
}

func TestValidateACLRejectsWhatParseRejects(t *testing.T) {
	for _, in := range []string{"10.0.0.0/33", "not-an-ip", "key:", "::ffff:10.0.0.0/120"} {
		if err := zones.ValidateACL(in); err == nil {
			t.Errorf("ValidateACL(%q) = nil, want error", in)
		}
	}
}

func TestFormatACLRoundTrips(t *testing.T) {
	const in = "10.0.0.0/24, key:ns2., 192.168.1.5"
	es, err := zones.ParseACL(in)
	if err != nil {
		t.Fatalf("ParseACL: %v", err)
	}
	out := zones.FormatACL(es)
	if out != in {
		t.Fatalf("FormatACL = %q, want %q", out, in)
	}
	// And parsing the formatted form gives the same entries back, which is
	// what lets the API store the canonical spelling.
	again, err := zones.ParseACL(out)
	if err != nil {
		t.Fatalf("ParseACL(formatted): %v", err)
	}
	if zones.FormatACL(again) != out {
		t.Fatalf("second round trip = %q, want %q", zones.FormatACL(again), out)
	}
}

// A v4-mapped bare address must round-trip through its unmapped spelling
// exactly once — format(parse(s)) already differs from s here, and the bug
// this guards against is format(parse(format(parse(s)))) differing from
// format(parse(s)) too, which is what happened when the CIDR branch left a
// mapped /128 unmapped while the address branch did not: two entry points
// for the same value, only one of which normalised it.
func TestFormatACLRoundTripsAV4MappedAddress(t *testing.T) {
	es, err := zones.ParseACL("::ffff:10.0.0.5")
	if err != nil {
		t.Fatalf("ParseACL: %v", err)
	}
	const want = "10.0.0.5"
	if out := zones.FormatACL(es); out != want {
		t.Fatalf("FormatACL = %q, want %q", out, want)
	}
	again, err := zones.ParseACL(want)
	if err != nil {
		t.Fatalf("ParseACL(%q): %v", want, err)
	}
	if out := zones.FormatACL(again); out != want {
		t.Fatalf("second round trip = %q, want %q", out, want)
	}
}

func TestACLKeys(t *testing.T) {
	got := zones.ACLKeys("10.0.0.0/24, key:NS2, key:other")
	want := []string{"ns2.", "other."}
	if len(got) != len(want) {
		t.Fatalf("ACLKeys gave %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("key %d = %q, want %q", i, got[i], want[i])
		}
	}
	// An unparseable value names no keys rather than panicking: callers use
	// this to validate and to count usage, and neither wants an error here.
	if k := zones.ACLKeys("nonsense"); len(k) != 0 {
		t.Errorf("ACLKeys(nonsense) = %v, want none", k)
	}
}

func TestACLAllows(t *testing.T) {
	es, err := zones.ParseACL("10.0.0.0/24, key:ns2")
	if err != nil {
		t.Fatalf("ParseACL: %v", err)
	}
	for _, tc := range []struct {
		name string
		peer string
		key  string
		want bool
	}{
		{name: "address in range, unsigned", peer: "10.0.0.5", want: true},
		{name: "address out of range, unsigned", peer: "10.0.1.5", want: false},
		{name: "wrong key, address out of range", peer: "10.0.1.5", key: "other.", want: false},
		{name: "right key, address out of range", peer: "10.0.1.5", key: "ns2.", want: true},
		{name: "key match is case insensitive", peer: "10.0.1.5", key: "NS2.", want: true},
		// The v4-mapped form is what a dual-stack listener hands back, and a
		// mapped address does not match a v4 prefix. Unmapping is done here so
		// no caller can forget it.
		{name: "v4-mapped peer still matches a v4 prefix", peer: "::ffff:10.0.0.5", want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			peer, err := netip.ParseAddr(tc.peer)
			if err != nil {
				t.Fatalf("ParseAddr: %v", err)
			}
			if got := zones.ACLAllows(es, peer, tc.key); got != tc.want {
				t.Errorf("ACLAllows(%s, %q) = %v, want %v", tc.peer, tc.key, got, tc.want)
			}
		})
	}
}

func TestACLAllowsNothingWhenEmpty(t *testing.T) {
	peer := netip.MustParseAddr("10.0.0.5")
	if zones.ACLAllows(nil, peer, "ns2.") {
		t.Fatal("an empty ACL allowed a transfer; default deny is the whole design")
	}
}
