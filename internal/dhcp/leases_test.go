package dhcp_test

import (
	"net/netip"
	"strings"
	"testing"

	"github.com/aloks98/dnsaur/internal/dhcp"
	"github.com/aloks98/dnsaur/internal/store"
)

func TestSanitizeLabel(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"My Laptop!", "my-laptop"},
		{"nas", "nas"},
		{"NAS", "nas"},
		{"---", ""},
		{"", ""},
		{"!!!", ""},
		{"  spaced  out  ", "spaced-out"},
		{"foo--bar", "foo--bar"},
		{"-leading", "leading"},
		{"trailing-", "trailing"},
		{"desktop.home.lan", "desktop-home-lan"},
		{"Björns iPhone", "bj-rns-iphone"},
		{"host_1", "host-1"},
		{strings.Repeat("a", 70), strings.Repeat("a", 63)},
		// Truncation must not leave a hyphen at the end: 62 letters, a
		// hyphen, then more is cut at 63 exactly on the hyphen.
		{strings.Repeat("a", 62) + "-b" + strings.Repeat("c", 10), strings.Repeat("a", 62)},
	}
	for _, c := range cases {
		if got := dhcp.SanitizeLabel(c.in); got != c.want {
			t.Errorf("SanitizeLabel(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestUnnamedReservationsAreStillReservations: a reservation with no hostname
// is not a name, but it is still the address that MAC is pinned to — which is
// what the `mac` client matcher and the leases page ask the table for. It used
// to be dropped from the table entirely, so a reservation the operator had
// made was invisible to both until something leased it.
func TestUnnamedReservationsAreStillReservations(t *testing.T) {
	e := newEngine(t, "3.0.3", alpineHooks)
	in := managerInput(e.Socket())
	in.Reservations = []store.Reservation{
		{ID: 1, ScopeID: 1, MAC: "aa:bb:cc:dd:ee:09", IP: "10.42.0.10"},
		{ID: 2, ScopeID: 1, MAC: "aa:bb:cc:dd:ee:0a", IP: "10.42.0.11", Hostname: "printer"},
	}
	m := dhcp.NewManager(dhcp.NewClient(e.Socket()), &fakeInputs{in: in}, openSettings(t), nil)
	if err := m.Poll(t.Context()); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	table := m.Table()

	if n := len(table.All()); n != 2 {
		t.Fatalf("the table holds %d entries, want both reservations: %v", n, table.All())
	}
	entry, ok := table.ByMAC("aa:bb:cc:dd:ee:09")
	if !ok {
		t.Fatalf("the unnamed reservation is not in the table by MAC: %v", table.All())
	}
	if !entry.Reserved || entry.IP != netip.MustParseAddr("10.42.0.10") {
		t.Errorf("by MAC = %+v, want the reserved 10.42.0.10", entry)
	}
	if entry, ok := table.ByIP(netip.MustParseAddr("10.42.0.10")); !ok || !entry.Reserved {
		t.Errorf("by IP = %+v, %v; want the reserved entry", entry, ok)
	}
	// It has no name, so it took none: the only name under the suffix is the
	// reservation that has one.
	named, ok := table.ByName("printer", "home.lan")
	if !ok || named.MAC != "aa:bb:cc:dd:ee:0a" {
		t.Errorf("printer.home.lan = %+v, %v; want the named reservation", named, ok)
	}
}

// An address inside a pool whose class sets a domain is named under the
// class's suffix, a reservation's too, leased or not: Kea hands the pool's
// domain to whatever address it gives out there. One from the scope's other
// pool is named under the scope's. The suffix belongs to the entry, so a
// release — which rebuilds the table from its entries — keeps every other
// name where it was.
func TestLeaseNamesFollowThePoolsClass(t *testing.T) {
	ctx := t.Context()
	e := newEngine(t, "3.0.3", alpineHooks)
	in := managerInput(e.Socket())
	in.Classes = []store.Class{
		{ID: 1, Name: "iot", Matchers: []string{"mac:a4:cf:12"}, ClientOptions: store.ClientOptions{Domain: "things.lan"}},
		// A domain on a class no pool names moves no name.
		{ID: 2, Name: "pxe", Matchers: []string{"vendor:PXEClient"}, ClientOptions: store.ClientOptions{Domain: "boot.lan"}},
	}
	in.Scopes[0].Pools = []store.Pool{
		{Start: "10.42.0.100", End: "10.42.0.149"},
		{Start: "10.42.0.150", End: "10.42.0.200", ClassID: 1},
	}
	in.Reservations = []store.Reservation{
		{ID: 1, ScopeID: 1, MAC: "aa:bb:cc:dd:ee:01", IP: "10.42.0.170", Hostname: "printer"},
		{ID: 2, ScopeID: 1, MAC: "aa:bb:cc:dd:ee:02", IP: "10.42.0.180"},
	}
	e.setLeases(
		dhcp.Lease{IP: "10.42.0.160", MAC: "a4:cf:12:00:00:01", Hostname: "plug", SubnetID: 1, CLTT: 1000, ValidLft: 3600},
		dhcp.Lease{IP: "10.42.0.161", MAC: "a4:cf:12:00:00:02", Hostname: "bulb", SubnetID: 1, CLTT: 1000, ValidLft: 3600},
		dhcp.Lease{IP: "10.42.0.110", MAC: "aa:bb:cc:dd:ee:10", Hostname: "laptop", SubnetID: 1, CLTT: 1000, ValidLft: 3600},
		// Reserved, leased, and inside the class's pool: named like any
		// other address there.
		dhcp.Lease{IP: "10.42.0.180", MAC: "aa:bb:cc:dd:ee:02", Hostname: "cam", SubnetID: 1, CLTT: 1000, ValidLft: 3600},
	)
	m := dhcp.NewManager(dhcp.NewClient(e.Socket()), &fakeInputs{in: in}, openSettings(t), nil)
	if err := m.Poll(ctx); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	names := func(when string) {
		t.Helper()
		table := m.Table()
		for _, c := range []struct {
			label, suffix string
			want          bool
		}{
			{"plug", "things.lan", true}, {"plug", "home.lan", false},
			{"laptop", "home.lan", true}, {"laptop", "things.lan", false},
			{"printer", "things.lan", true}, {"printer", "home.lan", false},
			{"cam", "things.lan", true}, {"cam", "home.lan", false},
		} {
			if _, ok := table.ByName(c.label, c.suffix); ok != c.want {
				t.Errorf("%s: %s.%s resolves = %v, want %v", when, c.label, c.suffix, ok, c.want)
			}
		}
	}
	names("after a poll")
	if err := m.Release(ctx, netip.MustParseAddr("10.42.0.161")); err != nil {
		t.Fatalf("Release: %v", err)
	}
	names("after a release")
}
