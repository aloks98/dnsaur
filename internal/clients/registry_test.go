package clients

import (
	"context"
	"net/netip"
	"testing"

	"github.com/aloks98/dnsaur/internal/store"
)

type fakeClientStore struct {
	groups  []store.Group
	clients []store.Client
}

func (f *fakeClientStore) Groups(ctx context.Context) ([]store.Group, error)   { return f.groups, nil }
func (f *fakeClientStore) Clients(ctx context.Context) ([]store.Client, error) { return f.clients, nil }
func (f *fakeClientStore) AddGroup(ctx context.Context, name string) (int64, error) {
	return 0, nil
}
func (f *fakeClientStore) AddClient(ctx context.Context, c store.Client) (int64, error) {
	return 0, nil
}
func (f *fakeClientStore) UpdateClient(ctx context.Context, c store.Client) error {
	return nil
}
func (f *fakeClientStore) DeleteClient(ctx context.Context, id int64) error {
	return nil
}
func (f *fakeClientStore) RenameGroup(ctx context.Context, id int64, name string) error {
	return nil
}
func (f *fakeClientStore) SetGroupEnabled(ctx context.Context, id int64, enabled bool) error {
	return nil
}
func (f *fakeClientStore) DeleteGroup(ctx context.Context, id int64) error {
	return nil
}

func TestLookupPrecedence(t *testing.T) {
	fs := &fakeClientStore{
		groups: []store.Group{{ID: 1, Name: "default", Enabled: true}, {ID: 2, Name: "kids", Enabled: true}, {ID: 3, Name: "iot", Enabled: true}},
		clients: []store.Client{
			{ID: 10, Name: "tablet", Matcher: "10.0.0.5", GroupID: 2},
			{ID: 11, Name: "iot-net", Matcher: "10.0.0.0/24", GroupID: 3},
		},
	}
	r := NewRegistry(fs)
	if err := r.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c := r.Lookup(netip.MustParseAddr("10.0.0.5")); c.GroupName != "kids" {
		t.Fatalf("exact should beat cidr: %+v", c)
	}
	if c := r.Lookup(netip.MustParseAddr("::ffff:10.0.0.5")); c.GroupName != "kids" {
		t.Fatalf("mapped v4 addr should exact-match: %+v", c)
	}
	if c := r.Lookup(netip.MustParseAddr("10.0.0.77")); c.GroupName != "iot" {
		t.Fatalf("cidr match: %+v", c)
	}
	if c := r.Lookup(netip.MustParseAddr("192.168.1.1")); c.GroupName != "default" || c.GroupID != 1 {
		t.Fatalf("unknown -> default: %+v", c)
	}
}

// TestLookupMostSpecificCIDR is the documented rule (/32 before /24 before
// /16) and, until now, an untested one: the entries are sorted by prefix
// length rather than left in insertion order.
func TestLookupMostSpecificCIDR(t *testing.T) {
	fs := &fakeClientStore{
		groups: []store.Group{{ID: 1, Name: "default", Enabled: true}, {ID: 2, Name: "wide"}, {ID: 3, Name: "narrow"}, {ID: 4, Name: "v6"}},
		clients: []store.Client{
			// Deliberately widest-first, so insertion order is the wrong answer.
			{ID: 10, Matcher: "10.0.0.0/8", GroupID: 2},
			{ID: 11, Matcher: "10.1.2.0/24", GroupID: 3},
			{ID: 12, Matcher: "2001:db8::/32", GroupID: 4},
		},
	}
	r := NewRegistry(fs)
	if err := r.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c := r.Lookup(netip.MustParseAddr("10.1.2.9")); c.GroupName != "narrow" {
		t.Fatalf("most specific CIDR should win: %+v", c)
	}
	if c := r.Lookup(netip.MustParseAddr("10.9.9.9")); c.GroupName != "wide" {
		t.Fatalf("wider CIDR should still cover the rest: %+v", c)
	}
	if c := r.Lookup(netip.MustParseAddr("2001:db8::5")); c.GroupName != "v6" {
		t.Fatalf("IPv6 CIDR: %+v", c)
	}
}

// TestReloadCanonicalisesStoredMatchers: rows written before the API
// validated matchers are still in the table. An unmasked prefix, a v4-mapped
// one and a zoned address all used to load and then match nothing.
func TestReloadCanonicalisesStoredMatchers(t *testing.T) {
	fs := &fakeClientStore{
		groups: []store.Group{{ID: 1, Name: "default", Enabled: true}, {ID: 2, Name: "unmasked"}, {ID: 3, Name: "mapped"}},
		clients: []store.Client{
			{ID: 10, Matcher: "10.0.0.1/24", GroupID: 2},
			{ID: 11, Matcher: "::ffff:192.168.4.0/120", GroupID: 3},
			{ID: 12, Matcher: "fe80::1%eth0", GroupID: 3},
			{ID: 13, Matcher: "definitely not an address", GroupID: 3},
		},
	}
	r := NewRegistry(fs)
	if err := r.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c := r.Lookup(netip.MustParseAddr("10.0.0.77")); c.GroupName != "unmasked" {
		t.Fatalf("unmasked prefix never matched: %+v", c)
	}
	if c := r.Lookup(netip.MustParseAddr("192.168.4.9")); c.GroupName != "mapped" {
		t.Fatalf("v4-mapped prefix never matched: %+v", c)
	}
	// The unusable rows are dropped rather than shadowing the default.
	if c := r.Lookup(netip.MustParseAddr("203.0.113.1")); c.GroupID != 1 {
		t.Fatalf("unknown -> default: %+v", c)
	}
}

func TestNormalizeMatcher(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"10.0.0.1", "10.0.0.1"},
		{"::ffff:10.0.0.1", "10.0.0.1"},
		{"10.0.0.1/24", "10.0.0.0/24"},
		{"::ffff:10.0.0.0/120", "10.0.0.0/24"},
		{"2001:db8::1/64", "2001:db8::/64"},
	} {
		if got, ok := NormalizeMatcher(tc.in); !ok || got != tc.want {
			t.Errorf("NormalizeMatcher(%q) = %q, %v; want %q, true", tc.in, got, ok, tc.want)
		}
	}
	for _, bad := range []string{"", "fe80::1%eth0", "::ffff:10.0.0.0/64", "10.0.0.0/33", "nonsense"} {
		if got, ok := NormalizeMatcher(bad); ok {
			t.Errorf("NormalizeMatcher(%q) = %q, true; want it rejected", bad, got)
		}
	}
}
