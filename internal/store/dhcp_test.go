package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/pressly/goose/v3"
)

// scopeFixture is a valid scope on the fixture's own subnet, so the postgres
// half — which shares one database across this package — never collides on
// dhcp_scopes.name.
func scopeFixture(f fixture) Scope {
	return Scope{
		Name: f.scope, CIDR: f.cidr(),
		// Two, around a gap, so an order the store loses shows.
		Pools:   []Pool{{Start: f.host(100), End: f.host(149)}, {Start: f.host(160), End: f.host(200)}},
		Gateway: f.host(1), LeaseSeconds: 3600, Enabled: true,
		ClientOptions: ClientOptions{
			DNSServers:   f.host(3) + ", " + f.host(4),
			DomainSearch: "home.lan, lab.lan",
			NTPServers:   f.host(5) + ", " + f.host(6),
			StaticRoutes: []StaticRoute{{Destination: "10.10.0.0/16", Router: f.host(2)}},
			NextServer:   f.host(9), ServerHostname: "boot.lan", BootFile: "pxelinux.0",
			Options: []GenericOption{{Code: 150, Hex: "0A2A0005"}},
		},
		MatchClientID: true,
		CreatedAt:     1757800000000, ModifiedAt: 1757800000000,
	}
}

// storedAs is want as the store hands it back: each pool numbered by the
// database and owned by want's scope. The ids are copied from got — the
// store picks them — but the count, order, bounds and scope_id are still
// want's to decide.
func storedAs(want, got Scope) Scope {
	want.Pools = slices.Clone(want.Pools)
	for i := range want.Pools {
		if i < len(got.Pools) {
			want.Pools[i].ID = got.Pools[i].ID
		}
		want.Pools[i].ScopeID = want.ID
	}
	if want.Pools == nil {
		want.Pools = []Pool{}
	}
	return want
}

// TestScopeRoundTrip is the store's half of §4.3 and §4.4: what goes in comes
// back, and a scope takes its reservations with it when it goes. The second
// scope is not decoration — a cascade that deleted every reservation rather
// than this scope's would pass every assertion about the first one.
func TestScopeRoundTrip(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s Store) {
		ctx := t.Context()
		one, two := newFixture(), newFixture()
		dhcp := s.DHCP()

		id, err := dhcp.AddScope(ctx, scopeFixture(one))
		if err != nil {
			t.Fatal(err)
		}
		other, err := dhcp.AddScope(ctx, scopeFixture(two))
		if err != nil {
			t.Fatal(err)
		}

		got, err := dhcp.Scope(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		want := scopeFixture(one)
		want.ID = id
		want = storedAs(want, got)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("Scope(%d) = %+v, want %+v", id, got, want)
		}
		if _, err := dhcp.Scope(ctx, 987654321); !errors.Is(err, ErrNotFound) {
			t.Errorf("Scope(unknown) = %v, want ErrNotFound", err)
		}

		scopes, err := dhcp.Scopes(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !hasScope(scopes, id) || !hasScope(scopes, other) {
			t.Errorf("Scopes() = %+v, want both %d and %d", scopes, id, other)
		}

		// A rename and a disable, which is most of what an edit is, and
		// the two lists cleared: they are JSON text columns, and a scope
		// that holds no static route reads back holding an empty list
		// rather than the null a nil slice would marshal to.
		edited := want
		edited.Name, edited.Enabled, edited.Domain, edited.ModifiedAt = one.scope+"-renamed", false, "lan.example", 1757800001000
		edited.StaticRoutes, edited.Options = nil, nil
		// The pools are replaced whole: one of the two goes, and the one
		// that stays moves to position 0 with a new class-free bound.
		edited.Pools = []Pool{{Start: one.host(170), End: one.host(180)}}
		if err := dhcp.UpdateScope(ctx, edited); err != nil {
			t.Fatal(err)
		}
		cleared := edited
		cleared.StaticRoutes, cleared.Options = []StaticRoute{}, []GenericOption{}
		got, err = dhcp.Scope(ctx, id)
		cleared = storedAs(cleared, got)
		if err != nil || !reflect.DeepEqual(got, cleared) {
			t.Errorf("Scope(%d) after update = %+v (err %v), want %+v", id, got, err, cleared)
		}
		if err := dhcp.UpdateScope(ctx, Scope{ID: 987654321}); !errors.Is(err, ErrNotFound) {
			t.Errorf("UpdateScope(unknown) = %v, want ErrNotFound", err)
		}

		kept, err := dhcp.AddReservation(ctx, Reservation{
			ScopeID: other, MAC: "aa:bb:cc:dd:ee:01", IP: two.host(50),
			Hostname: "printer-1", Comment: "the one that must survive",
			CreatedAt: 1757800000000, ModifiedAt: 1757800000000,
		})
		if err != nil {
			t.Fatal(err)
		}
		doomed, err := dhcp.AddReservation(ctx, Reservation{
			ScopeID: id, MAC: "aa:bb:cc:dd:ee:02", IP: one.host(51),
			CreatedAt: 1757800000000, ModifiedAt: 1757800000000,
		})
		if err != nil {
			t.Fatal(err)
		}
		gone, err := dhcp.AddReservation(ctx, Reservation{
			ScopeID: id, MAC: "aa:bb:cc:dd:ee:03", IP: one.host(52),
			CreatedAt: 1757800000000, ModifiedAt: 1757800000000,
		})
		if err != nil {
			t.Fatal(err)
		}

		rs, err := dhcp.Reservations(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if got := reservationByID(rs, kept); got.ScopeID != other || got.IP != two.host(50) || got.Hostname != "printer-1" {
			t.Errorf("reservation %d = %+v, want the row that was written", kept, got)
		}

		byID := reservationByID(rs, doomed)
		byID.Hostname, byID.Comment, byID.ModifiedAt = "nas", "renamed", 1757800002000
		if err := dhcp.UpdateReservation(ctx, byID); err != nil {
			t.Fatal(err)
		}
		if rs, err = dhcp.Reservations(ctx); err != nil {
			t.Fatal(err)
		} else if got := reservationByID(rs, doomed); got != byID {
			t.Errorf("reservation %d after update = %+v, want %+v", doomed, got, byID)
		}
		if err := dhcp.UpdateReservation(ctx, Reservation{ID: 987654321, ScopeID: id}); !errors.Is(err, ErrNotFound) {
			t.Errorf("UpdateReservation(unknown) = %v, want ErrNotFound", err)
		}

		if err := dhcp.DeleteReservation(ctx, gone); err != nil {
			t.Fatal(err)
		}
		if err := dhcp.DeleteReservation(ctx, gone); !errors.Is(err, ErrNotFound) {
			t.Errorf("DeleteReservation twice = %v, want ErrNotFound", err)
		}

		// The cascade: this scope's reservations go with it, in the same
		// transaction, and nobody else's do.
		if err := dhcp.DeleteScope(ctx, id); err != nil {
			t.Fatal(err)
		}
		if _, err := dhcp.Scope(ctx, id); !errors.Is(err, ErrNotFound) {
			t.Errorf("Scope(%d) after DeleteScope = %v, want ErrNotFound", id, err)
		}
		rs, err = dhcp.Reservations(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rs {
			if r.ScopeID == id {
				t.Errorf("reservation %+v outlived its scope", r)
			}
		}
		if got := reservationByID(rs, kept); got.ID != kept {
			t.Errorf("deleting scope %d took reservation %d of scope %d with it", id, kept, other)
		}
		if err := dhcp.DeleteScope(ctx, id); !errors.Is(err, ErrNotFound) {
			t.Errorf("DeleteScope twice = %v, want ErrNotFound", err)
		}
	})
}

func hasScope(scopes []Scope, id int64) bool {
	for _, s := range scopes {
		if s.ID == id {
			return true
		}
	}
	return false
}

func reservationByID(rs []Reservation, id int64) Reservation {
	for _, r := range rs {
		if r.ID == id {
			return r
		}
	}
	return Reservation{}
}

// TestDHCPWritesNormaliseWhatTheyStore is what the two unique indexes rest
// on. (scope_id, mac) means "this NIC" only if every spelling of one NIC
// reaches the column as the same string — otherwise the same machine is
// reservable twice and the engine is handed two answers for it. The
// whitespace is not a contrived case either: it is what a paste from a label
// or a spreadsheet carries.
func TestDHCPWritesNormaliseWhatTheyStore(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s Store) {
		ctx := t.Context()
		f := newFixture()
		dhcp := s.DHCP()

		pad := func(sc Scope) Scope {
			sc.CIDR, sc.Gateway = " "+sc.CIDR+" ", " "+sc.Gateway+" "
			sc.Pools = slices.Clone(sc.Pools)
			for i, p := range sc.Pools {
				sc.Pools[i].Start, sc.Pools[i].End = "\t"+p.Start, p.End+"\n"
			}
			sc.DNSServers = " " + f.host(3) + " ,\t" + f.host(4) + " "
			sc.DomainSearch = " home.lan ,\tlab.lan "
			sc.NTPServers = " " + f.host(5) + " ,  " + f.host(6) + "\n"
			sc.StaticRoutes = []StaticRoute{{Destination: " 10.10.0.0/16 ", Router: "\t" + f.host(2)}}
			sc.NextServer, sc.ServerHostname = " "+f.host(9), "boot.lan "
			sc.BootFile = " pxelinux.0"
			// Kea reads the generic option's value as plain hex, so the
			// colons an operator pastes from a vendor document are stripped
			// and the case is settled once — here — rather than per reader.
			sc.Options = []GenericOption{{Code: 150, Hex: " 0a:2a:00:05 "}}
			return sc
		}
		want := scopeFixture(f)
		id, err := dhcp.AddScope(ctx, pad(want))
		if err != nil {
			t.Fatal(err)
		}
		want.ID = id
		if got, err := dhcp.Scope(ctx, id); err != nil || !reflect.DeepEqual(got, storedAs(want, got)) {
			t.Errorf("AddScope stored %+v (err %v), want the trimmed %+v", got, err, want)
		}
		want.ModifiedAt = 1757800005000
		if err := dhcp.UpdateScope(ctx, pad(want)); err != nil {
			t.Fatal(err)
		}
		if got, err := dhcp.Scope(ctx, id); err != nil || !reflect.DeepEqual(got, storedAs(want, got)) {
			t.Errorf("UpdateScope stored %+v (err %v), want the trimmed %+v", got, err, want)
		}

		res := Reservation{
			ScopeID: id, MAC: "AA-BB-CC-DD-EE-FF", IP: " " + f.host(50),
			CreatedAt: 1757800000000, ModifiedAt: 1757800000000,
		}
		resID, err := dhcp.AddReservation(ctx, res)
		if err != nil {
			t.Fatal(err)
		}
		stored := func() Reservation {
			t.Helper()
			rs, err := dhcp.Reservations(ctx)
			if err != nil {
				t.Fatal(err)
			}
			return reservationByID(rs, resID)
		}
		if got := stored(); got.MAC != "aa:bb:cc:dd:ee:ff" || got.IP != f.host(50) {
			t.Errorf("AddReservation stored mac %q ip %q, want %q and %q", got.MAC, got.IP, "aa:bb:cc:dd:ee:ff", f.host(50))
		}

		// The point of all of it: the same NIC, typed the other way, is the
		// duplicate the index is there to catch.
		if _, err := dhcp.AddReservation(ctx, Reservation{ScopeID: id, MAC: "aa:bb:cc:dd:ee:ff", IP: f.host(51)}); !errors.Is(err, ErrDuplicate) {
			t.Errorf("re-reserving the same NIC in another spelling = %v, want ErrDuplicate", err)
		}
		if _, err := dhcp.AddReservation(ctx, Reservation{ScopeID: id, MAC: "aa:bb:cc:dd:ee:aa", IP: f.host(50) + " "}); !errors.Is(err, ErrDuplicate) {
			t.Errorf("re-reserving the same address with a trailing space = %v, want ErrDuplicate", err)
		}

		update := stored()
		update.MAC, update.IP, update.ModifiedAt = "11-22-33-44-55-66", f.host(52)+"\t", 1757800006000
		if err := dhcp.UpdateReservation(ctx, update); err != nil {
			t.Fatal(err)
		}
		if got := stored(); got.MAC != "11:22:33:44:55:66" || got.IP != f.host(52) {
			t.Errorf("UpdateReservation stored mac %q ip %q, want %q and %q", got.MAC, got.IP, "11:22:33:44:55:66", f.host(52))
		}

		// A MAC that is not one is stored as typed rather than refused here:
		// the store validates nothing (ValidateReservation is the API's), and
		// a half-rule in the write path would be a second owner of the same
		// question.
		odd, err := dhcp.AddReservation(ctx, Reservation{ScopeID: id, MAC: "not-a-mac", IP: f.host(53)})
		if err != nil {
			t.Fatal(err)
		}
		rs, err := dhcp.Reservations(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if got := reservationByID(rs, odd).MAC; got != "not-a-mac" {
			t.Errorf("an unparseable mac was stored as %q, want it left as typed", got)
		}
	})
}

// TestValidateScope is §4.3's column table, one case per rule. These are the
// rules the API enforces before a write; the store trusts its caller, so a
// gap here is a gap everywhere.
func TestValidateScope(t *testing.T) {
	valid := Scope{
		ID: 1, Name: "lan", CIDR: "10.0.0.0/24",
		Pools: []Pool{{Start: "10.0.0.100", End: "10.0.0.200"}}, Gateway: "10.0.0.1",
		LeaseSeconds: 3600, Enabled: true,
	}
	// pool is a fresh one-pool list: a case that edited valid.Pools in
	// place would edit every later case's scope with it.
	pool := func(start, end string) []Pool { return []Pool{{Start: start, End: end}} }
	with := func(fn func(s *Scope)) Scope {
		s := valid
		fn(&s)
		return s
	}
	// The scope every overlap case is measured against: 10.0.0.128/25 is
	// inside valid's /24.
	overlapping := Scope{ID: 2, Name: "guest", CIDR: "10.0.0.128/25", Enabled: true}
	neighbour := Scope{ID: 3, Name: "dmz", CIDR: "10.1.0.0/24", Enabled: true}

	for _, tc := range []struct {
		name    string
		scope   Scope
		others  []Scope
		classes []Class
		want    error // nil = accepted; errAny = refused, reason unpinned
	}{
		{name: "valid", scope: valid},
		{name: "empty name", scope: with(func(s *Scope) { s.Name = "" }), want: errAny},
		{name: "bad cidr", scope: with(func(s *Scope) { s.CIDR = "10.0.0.0/33" }), want: errAny},
		{name: "host bits in cidr", scope: with(func(s *Scope) { s.CIDR = "10.0.0.5/24" }), want: errAny},
		{name: "ipv6 cidr", scope: with(func(s *Scope) { s.CIDR = "fd00::/64" }), want: errAny},
		{name: "pool start outside", scope: with(func(s *Scope) { s.Pools = pool("10.9.0.100", "10.0.0.200") }), want: errAny},
		{name: "pool end outside", scope: with(func(s *Scope) { s.Pools = pool("10.0.0.100", "10.9.0.200") }), want: errAny},
		{name: "pool start after end", scope: with(func(s *Scope) { s.Pools = pool("10.0.0.200", "10.0.0.100") }), want: errAny},
		{name: "pool includes the network address", scope: with(func(s *Scope) { s.Pools = pool("10.0.0.0", "10.0.0.200") }), want: errAny},
		{name: "pool includes the broadcast address", scope: with(func(s *Scope) { s.Pools = pool("10.0.0.100", "10.0.0.255") }), want: errAny},
		{name: "single-address pool", scope: with(func(s *Scope) { s.Pools = pool("10.0.0.100", "10.0.0.100") })},
		{name: "half a pool", scope: with(func(s *Scope) { s.Pools = pool("10.0.0.100", "") }), want: errAny},
		{name: "two pools around a gap", scope: with(func(s *Scope) {
			s.Pools = []Pool{{Start: "10.0.0.10", End: "10.0.0.49"}, {Start: "10.0.0.60", End: "10.0.0.99"}}
		})},
		// Listed out of order on purpose: the overlap rule is about
		// addresses, not about the order the operator typed the rows in.
		{name: "two pools back to back, listed high first", scope: with(func(s *Scope) {
			s.Pools = []Pool{{Start: "10.0.0.50", End: "10.0.0.99"}, {Start: "10.0.0.10", End: "10.0.0.49"}}
		})},
		// Listed after the pool it sits in, so it is the sort that finds it.
		{name: "a pool inside another", scope: with(func(s *Scope) {
			s.Pools = []Pool{{Start: "10.0.0.150", End: "10.0.0.160"}, {Start: "10.0.0.10", End: "10.0.0.200"}}
		}), want: errAny},
		{name: "pool naming a known class", scope: with(func(s *Scope) {
			s.Pools = []Pool{{Start: "10.0.0.100", End: "10.0.0.200", ClassID: 1}}
		}), classes: []Class{{ID: 1, Name: "iot"}}},
		{name: "gateway outside", scope: with(func(s *Scope) { s.Gateway = "10.9.0.1" }), want: errAny},
		{name: "no gateway", scope: with(func(s *Scope) { s.Gateway = "" })},
		{name: "dns servers", scope: with(func(s *Scope) { s.DNSServers = "1.1.1.1, 8.8.8.8" })},
		{name: "one bad dns server", scope: with(func(s *Scope) { s.DNSServers = "1.1.1.1, bad" }), want: errAny},
		{name: "automatic dns servers", scope: with(func(s *Scope) { s.DNSServers = "" })},
		{name: "lease below the floor", scope: with(func(s *Scope) { s.LeaseSeconds = 100 }), want: errAny},
		{name: "lease from the setting", scope: with(func(s *Scope) { s.LeaseSeconds = 0 })},
		{name: "lease at the floor", scope: with(func(s *Scope) { s.LeaseSeconds = 300 })},
		{name: "negative lease", scope: with(func(s *Scope) { s.LeaseSeconds = -1 }), want: errAny},

		// Option 119's list is suffixes, not labels: a dot separates them
		// inside one entry, a comma separates the entries.
		// The scope's own suffix is the dhcp.domain setting's grammar, and
		// for the same reason: it is handed to clients as option 15 and is
		// the suffix a lease's name is published under (§8.1).
		{name: "domain suffix", scope: with(func(s *Scope) { s.Domain = "home.lan" })},
		{name: "domain with a trailing dot", scope: with(func(s *Scope) { s.Domain = "home.lan." }), want: errAny},
		{name: "domain that is not a hostname", scope: with(func(s *Scope) { s.Domain = "home_lan" }), want: errAny},
		{name: "domain with a space in it", scope: with(func(s *Scope) { s.Domain = "home lan" }), want: errAny},
		{name: "domain over 253 bytes", scope: with(func(s *Scope) { s.Domain = strings.Repeat("a.", 130) + "lan" }), want: errAny},
		{name: "domain search list", scope: with(func(s *Scope) { s.DomainSearch = "home.lan, lab.lan" })},
		{name: "domain search with a trailing dot", scope: with(func(s *Scope) { s.DomainSearch = "home.lan." }), want: errAny},
		{name: "domain search with a leading dot", scope: with(func(s *Scope) { s.DomainSearch = ".home.lan" }), want: errAny},
		{name: "domain search that is not a hostname", scope: with(func(s *Scope) { s.DomainSearch = "home.lan, lab_lan" }), want: errAny},
		{name: "ntp servers", scope: with(func(s *Scope) { s.NTPServers = "10.0.0.5, 10.0.0.6" })},
		{name: "ntp server by name", scope: with(func(s *Scope) { s.NTPServers = "10.0.0.5, ntp.lan" }), want: errAny},
		{name: "static route", scope: with(func(s *Scope) {
			s.StaticRoutes = []StaticRoute{{Destination: "10.10.0.0/16", Router: "10.0.0.1"}}
		})},
		{name: "static route destination with host bits", scope: with(func(s *Scope) {
			s.StaticRoutes = []StaticRoute{{Destination: "10.10.0.5/16", Router: "10.0.0.1"}}
		}), want: errAny},
		// The router has to be reachable without the route it is announcing,
		// which on a DHCP segment means "inside this subnet".
		{name: "static route router outside the scope", scope: with(func(s *Scope) {
			s.StaticRoutes = []StaticRoute{{Destination: "10.10.0.0/16", Router: "10.9.0.1"}}
		}), want: errAny},
		{name: "next server", scope: with(func(s *Scope) { s.NextServer = "10.0.0.9" })},
		{name: "next server by name", scope: with(func(s *Scope) { s.NextServer = "boot.lan" }), want: errAny},
		{name: "server hostname", scope: with(func(s *Scope) { s.ServerHostname = "boot.lan" })},
		{name: "server hostname that is not a hostname", scope: with(func(s *Scope) { s.ServerHostname = "boot_lan" }), want: errAny},
		// sname holds 64 bytes and file 128, each NUL-terminated, so the
		// longest name either carries is one byte shorter. Both cases are
		// dotted names of legal labels: it is the cap that has to refuse
		// them, not the label rule, which stops at 63 bytes per label.
		{name: "server hostname at the limit", scope: with(func(s *Scope) { s.ServerHostname = strings.Repeat("a", 59) + ".lan" })},
		{name: "server hostname above the limit", scope: with(func(s *Scope) { s.ServerHostname = strings.Repeat("a", 60) + ".lan" }), want: errAny},
		{name: "boot file at the limit", scope: with(func(s *Scope) { s.BootFile = strings.Repeat("a", 127) })},
		{name: "boot file above the limit", scope: with(func(s *Scope) { s.BootFile = strings.Repeat("a", 128) }), want: errAny},
		{name: "generic option", scope: with(func(s *Scope) { s.Options = []GenericOption{{Code: 150, Hex: "0A2A0005"}} })},
		{name: "generic option written with colons", scope: with(func(s *Scope) {
			s.Options = []GenericOption{{Code: 44, Hex: "0a:2a:00:05"}}
		})},
		// 42 is ntp-servers, which the scope has its own column for: two
		// owners of one option means whichever Kea reads last wins.
		{name: "generic option kea renders by name", scope: with(func(s *Scope) {
			s.Options = []GenericOption{{Code: 42, Hex: "0A2A0005"}}
		}), want: errAny},
		{name: "generic option 121", scope: with(func(s *Scope) { s.Options = []GenericOption{{Code: 121, Hex: "00"}} }), want: errAny},
		{name: "generic option code 0", scope: with(func(s *Scope) { s.Options = []GenericOption{{Code: 0, Hex: "00"}} }), want: errAny},
		{name: "generic option code 255", scope: with(func(s *Scope) { s.Options = []GenericOption{{Code: 255, Hex: "00"}} }), want: errAny},
		{name: "generic option with an odd hex length", scope: with(func(s *Scope) {
			s.Options = []GenericOption{{Code: 150, Hex: "0A2A000"}}
		}), want: errAny},
		{name: "generic option with no value", scope: with(func(s *Scope) { s.Options = []GenericOption{{Code: 150, Hex: ""}} }), want: errAny},
		{name: "generic option that is not hex", scope: with(func(s *Scope) { s.Options = []GenericOption{{Code: 150, Hex: "zzzz"}} }), want: errAny},
		// reservations_only renders a subnet with no pool, so the pool rules
		// are the ones that stop applying — not the rest of the scope's.
		{name: "reservations only without a pool", scope: with(func(s *Scope) {
			s.Pools, s.ReservationsOnly = nil, true
		})},
		{name: "reservations only with a pool", scope: with(func(s *Scope) { s.ReservationsOnly = true })},
		{name: "reservations only with a pool outside the scope", scope: with(func(s *Scope) {
			s.Pools, s.ReservationsOnly = pool("10.9.0.100", "10.0.0.200"), true
		}), want: errAny},
		{name: "reservations only with half a pool", scope: with(func(s *Scope) { s.Pools, s.ReservationsOnly = pool("10.0.0.100", ""), true }), want: errAny},
		{name: "no pool and not reservations only", scope: with(func(s *Scope) { s.Pools = nil }), want: errAny},
		{name: "name of 63 characters", scope: with(func(s *Scope) { s.Name = strings.Repeat("a", 63) })},
		{name: "name of 64 characters", scope: with(func(s *Scope) { s.Name = strings.Repeat("a", 64) }), want: errAny},

		{name: "overlaps an enabled scope", scope: valid, others: []Scope{overlapping}, want: ErrScopeOverlap},
		{
			name:   "overlaps a disabled scope",
			scope:  valid,
			others: []Scope{{ID: 2, Name: "guest", CIDR: "10.0.0.128/25", Enabled: false}},
		},
		{
			name:   "disabled itself, overlapping an enabled scope",
			scope:  with(func(s *Scope) { s.Enabled = false }),
			others: []Scope{overlapping},
		},
		{name: "overlaps only itself", scope: valid, others: []Scope{{ID: 1, Name: "lan", CIDR: "10.0.0.0/24", Enabled: true}}},
		{name: "beside a scope it does not overlap", scope: valid, others: []Scope{neighbour}},
		{name: "contained by an enabled scope", scope: with(func(s *Scope) {
			s.CIDR, s.Pools, s.Gateway = "10.0.0.128/25", pool("10.0.0.150", "10.0.0.200"), "10.0.0.129"
		}), others: []Scope{{ID: 4, Name: "big", CIDR: "10.0.0.0/16", Enabled: true}}, want: ErrScopeOverlap},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateScope(tc.scope, tc.others, tc.classes)
			assertValidation(t, err, tc.want)
		})
	}
}

// TestScopeJSONDefaultsMatchClientID is the one place the column's default
// and Go's zero value disagree. match_client_id defaults to true, and a
// Scope decoded from a body that says nothing about it — an API request that
// predates the field, or a bundle written by an older main — would otherwise
// arrive asking for the opposite and quietly turn client-id matching off on
// every scope it carried.
func TestScopeJSONDefaultsMatchClientID(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want bool
	}{
		{name: "absent", body: `{"name":"lan","cidr":"10.0.0.0/24"}`, want: true},
		{name: "false", body: `{"name":"lan","cidr":"10.0.0.0/24","match_client_id":false}`},
		{name: "true", body: `{"name":"lan","cidr":"10.0.0.0/24","match_client_id":true}`, want: true},
		{name: "null", body: `{"name":"lan","cidr":"10.0.0.0/24","match_client_id":null}`, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got Scope
			if err := json.Unmarshal([]byte(tc.body), &got); err != nil {
				t.Fatal(err)
			}
			if got.MatchClientID != tc.want {
				t.Errorf("match_client_id = %v, want %v", got.MatchClientID, tc.want)
			}
			// The rest of the scope still has to decode: the default is a
			// field read apart from the others, not instead of them.
			if got.Name != "lan" || got.CIDR != "10.0.0.0/24" {
				t.Errorf("decoded %+v, want the name and cidr the body carried", got)
			}
		})
	}
}

// TestImportBundleDefaultsMatchClientID is the same default on the path that
// actually carries an older main's rows. Driver-independent — it is the JSON
// decode that has to supply the default — so one store is the whole test.
func TestImportBundleDefaultsMatchClientID(t *testing.T) {
	ctx := t.Context()
	s := openSQLite(t)
	body := fmt.Sprintf(`{"format":%d,"scopes":[{"id":1,"name":"lan","cidr":"10.0.0.0/24","pools":[{"id":1,"start":"10.0.0.100","end":"10.0.0.200"}],"enabled":true}]}`, BundleFormat)
	var b Bundle
	if err := json.Unmarshal([]byte(body), &b); err != nil {
		t.Fatal(err)
	}
	if err := s.ImportBundle(ctx, b); err != nil {
		t.Fatal(err)
	}
	got, err := s.DHCP().Scope(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !got.MatchClientID {
		t.Error("a bundle scope with no match_client_id imported with client-id matching off")
	}
}

// TestScopesReportUnreadableJSONColumns is what a hand-edited database, or a
// column a future migration got wrong, does to the read path: the two JSON
// columns are the only ones here that are parsed rather than copied, and a
// parse that fails has to come back as an error naming the column rather
// than as a panic or as a scope silently missing its routes.
func TestScopesReportUnreadableJSONColumns(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s Store) {
		ctx := t.Context()
		f := newFixture()
		id, err := s.DHCP().AddScope(ctx, scopeFixture(f))
		if err != nil {
			t.Fatal(err)
		}
		// The row is corrupt for the rest of this test, and postgres shares
		// one database across the package: it goes before anything else runs.
		t.Cleanup(func() {
			if err := s.DHCP().DeleteScope(context.Background(), id); err != nil {
				t.Errorf("removing the corrupted scope: %v", err)
			}
		})
		raw := s.(*sqlStore)
		if _, err := raw.db.ExecContext(ctx, raw.q(`UPDATE dhcp_scopes SET options = '{' WHERE id = ?`), id); err != nil {
			t.Fatal(err)
		}

		var syntax *json.SyntaxError
		for _, tc := range []struct {
			name string
			err  error
		}{
			{name: "Scopes", err: func() error { _, err := s.DHCP().Scopes(ctx); return err }()},
			{name: "Scope", err: func() error { _, err := s.DHCP().Scope(ctx, id); return err }()},
		} {
			switch {
			case tc.err == nil:
				t.Errorf("%s() read a scope whose options column is not JSON without complaining", tc.name)
			case !errors.As(tc.err, &syntax):
				t.Errorf("%s() = %v, want the json error wrapped", tc.name, tc.err)
			case !strings.Contains(tc.err.Error(), "options"):
				t.Errorf("%s() = %v, want the column named", tc.name, tc.err)
			}
		}
	})
}

// TestValidateReservation is §4.4's column table. The scope is the parent the
// rules are measured against — an IP is "outside" relative to a subnet, and a
// duplicate is a duplicate only within one scope.
func TestValidateReservation(t *testing.T) {
	scope := Scope{
		ID: 1, Name: "lan", CIDR: "10.0.0.0/24",
		Pools: []Pool{{Start: "10.0.0.100", End: "10.0.0.200"}}, Gateway: "10.0.0.1", Enabled: true,
	}
	valid := Reservation{ID: 1, ScopeID: 1, MAC: "aa:bb:cc:dd:ee:ff", IP: "10.0.0.50", Hostname: "printer-1"}
	with := func(fn func(r *Reservation)) Reservation {
		r := valid
		fn(&r)
		return r
	}
	// A second reservation in the same scope, which every uniqueness case
	// collides with.
	taken := Reservation{ID: 2, ScopeID: 1, MAC: "11:22:33:44:55:66", IP: "10.0.0.60", Hostname: "nas"}

	for _, tc := range []struct {
		name   string
		res    Reservation
		others []Reservation
		want   error
	}{
		{name: "valid", res: valid},
		{name: "dashed uppercase mac", res: with(func(r *Reservation) { r.MAC = "AA-BB-CC-DD-EE-FF" })},
		{name: "not a mac", res: with(func(r *Reservation) { r.MAC = "zz:bb:cc:dd:ee:ff" }), want: errAny},
		{name: "no mac", res: with(func(r *Reservation) { r.MAC = "" }), want: errAny},
		{name: "eui-64 mac", res: with(func(r *Reservation) { r.MAC = "aa:bb:cc:dd:ee:ff:00:11" }), want: errAny},
		{name: "ip outside the scope", res: with(func(r *Reservation) { r.IP = "10.9.0.5" }), want: ErrOutsideScope},
		{name: "not an ip", res: with(func(r *Reservation) { r.IP = "10.0.0.256" }), want: errAny},
		{name: "the gateway", res: with(func(r *Reservation) { r.IP = "10.0.0.1" }), want: errAny},
		// §4.4: a reservation may sit inside the dynamic pool or outside it.
		{name: "inside the pool", res: with(func(r *Reservation) { r.IP = "10.0.0.150" })},

		{name: "duplicate mac", res: with(func(r *Reservation) { r.MAC = taken.MAC }), others: []Reservation{taken}, want: ErrDuplicate},
		{name: "duplicate mac, other spelling", res: with(func(r *Reservation) { r.MAC = "11-22-33-44-55-66" }), others: []Reservation{taken}, want: ErrDuplicate},
		{name: "duplicate ip", res: with(func(r *Reservation) { r.IP = taken.IP }), others: []Reservation{taken}, want: ErrDuplicate},
		{name: "duplicate hostname", res: with(func(r *Reservation) { r.Hostname = taken.Hostname }), others: []Reservation{taken}, want: ErrDuplicate},
		{name: "its own row", res: valid, others: []Reservation{{ID: 1, ScopeID: 1, MAC: valid.MAC, IP: valid.IP, Hostname: valid.Hostname}}},
		{
			name:   "the same mac in another scope",
			res:    with(func(r *Reservation) { r.MAC = taken.MAC }),
			others: []Reservation{{ID: 3, ScopeID: 2, MAC: taken.MAC, IP: "10.0.0.60"}},
		},

		{name: "underscore in the hostname", res: with(func(r *Reservation) { r.Hostname = "Printer_1" }), want: errAny},
		{name: "hostname as a label", res: with(func(r *Reservation) { r.Hostname = "printer-1" })},
		{name: "uppercase hostname", res: with(func(r *Reservation) { r.Hostname = "NAS" })},
		{name: "hostname of 63 characters", res: with(func(r *Reservation) { r.Hostname = strings.Repeat("a", 63) })},
		{name: "hostname of 64 characters", res: with(func(r *Reservation) { r.Hostname = strings.Repeat("a", 64) }), want: errAny},
		{name: "hostname ending in a hyphen", res: with(func(r *Reservation) { r.Hostname = "printer-" }), want: errAny},
		{name: "dotted hostname", res: with(func(r *Reservation) { r.Hostname = "printer-1.lan" }), want: errAny},
		{name: "hostname starting with a hyphen", res: with(func(r *Reservation) { r.Hostname = "-printer" }), want: errAny},
		{name: "no hostname", res: with(func(r *Reservation) { r.Hostname = "" })},
		{
			// Two blanks are not a collision: most reservations have no
			// hostname at all.
			name:   "no hostname, beside another without one",
			res:    with(func(r *Reservation) { r.Hostname = "" }),
			others: []Reservation{{ID: 2, ScopeID: 1, MAC: taken.MAC, IP: taken.IP, Hostname: ""}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertValidation(t, ValidateReservation(tc.res, scope, tc.others), tc.want)
		})
	}
}

// TestCanonicalMAC pins the spelling the unique index rests on: two operators
// typing the same NIC three different ways must produce one row, not three.
func TestCanonicalMAC(t *testing.T) {
	for in, want := range map[string]string{
		"AA-BB-CC-DD-EE-FF": "aa:bb:cc:dd:ee:ff",
		"aa:bb:cc:dd:ee:ff": "aa:bb:cc:dd:ee:ff",
		"AABB.CCDD.EEFF":    "aa:bb:cc:dd:ee:ff",
		// Bare hex is what a vendor's label and most web UIs print.
		"aabbccddeeff": "aa:bb:cc:dd:ee:ff",
	} {
		if got, ok := CanonicalMAC(in); !ok || got != want {
			t.Errorf("CanonicalMAC(%q) = %q, %v; want %q, true", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "zz:bb:cc:dd:ee:ff", "aa:bb:cc:dd:ee", "aa:bb:cc:dd:ee:ff:00:11", "aabbccddee"} {
		if got, ok := CanonicalMAC(in); ok {
			t.Errorf("CanonicalMAC(%q) = %q, true; want it refused", in, got)
		}
	}
}

// errAny stands for "refused, and this case does not pin which sentinel".
var errAny = errors.New("some validation error")

func assertValidation(t *testing.T, err, want error) {
	t.Helper()
	switch {
	case want == nil && err != nil:
		t.Errorf("refused a valid value: %v", err)
	case want != nil && err == nil:
		t.Error("accepted a value that breaks the rule")
	case want != nil && want != errAny && !errors.Is(err, want):
		t.Errorf("error = %v, want %v", err, want)
	}
}

// TestDHCPWritesMoveTheConfigVersion is TestConfigVersionTracksSyncedWrites
// for these two tables: both travel in a bundle, so a write a replica cannot
// see is a scope or a reservation the fleet silently disagrees about.
func TestDHCPWritesMoveTheConfigVersion(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s Store) {
		ctx := t.Context()
		f := newFixture()
		dhcp := s.DHCP()
		version := func() int64 {
			v, err := s.Settings().ConfigVersion(ctx)
			if err != nil {
				t.Fatalf("ConfigVersion: %v", err)
			}
			return v
		}
		moves := func(what string, want int64, fn func() error) {
			t.Helper()
			before := version()
			if err := fn(); err != nil {
				t.Fatalf("%s: %v", what, err)
			}
			if got := version() - before; got != want {
				t.Errorf("%s moved config_version by %d, want %d", what, got, want)
			}
		}

		var scopeID, resID int64
		moves("AddScope", 1, func() (err error) {
			scopeID, err = dhcp.AddScope(ctx, scopeFixture(f))
			return err
		})
		moves("UpdateScope", 1, func() error {
			sc, err := dhcp.Scope(ctx, scopeID)
			if err != nil {
				return err
			}
			sc.Domain, sc.ModifiedAt = "lan.example", 1757800003000
			return dhcp.UpdateScope(ctx, sc)
		})
		moves("AddReservation", 1, func() (err error) {
			resID, err = dhcp.AddReservation(ctx, Reservation{
				ScopeID: scopeID, MAC: "aa:bb:cc:dd:ee:10", IP: f.host(60),
				CreatedAt: 1757800000000, ModifiedAt: 1757800000000,
			})
			return err
		})
		moves("UpdateReservation", 1, func() error {
			rs, err := dhcp.Reservations(ctx)
			if err != nil {
				return err
			}
			r := reservationByID(rs, resID)
			r.Hostname, r.ModifiedAt = "nas", 1757800004000
			return dhcp.UpdateReservation(ctx, r)
		})
		moves("DeleteReservation", 1, func() error { return dhcp.DeleteReservation(ctx, resID) })

		// The cascade is several statements under one bump, not one bump per
		// statement: a replica polls this number, and a delete that moved it
		// twice would have the fleet pull the same bundle twice.
		for i, mac := range []string{"aa:bb:cc:dd:ee:11", "aa:bb:cc:dd:ee:12"} {
			if _, err := dhcp.AddReservation(ctx, Reservation{
				ScopeID: scopeID, MAC: mac, IP: f.host(70 + i),
				CreatedAt: 1757800000000, ModifiedAt: 1757800000000,
			}); err != nil {
				t.Fatalf("AddReservation: %v", err)
			}
		}
		moves("DeleteScope holding two reservations", 1, func() error { return dhcp.DeleteScope(ctx, scopeID) })

		// A write that matched nothing changed no configuration — including
		// DeleteScope, whose cascade must roll back with it.
		moves("DeleteScope on an unknown id", 0, func() error {
			if err := dhcp.DeleteScope(ctx, 987654321); !errors.Is(err, ErrNotFound) {
				t.Fatalf("DeleteScope(unknown) = %v, want ErrNotFound", err)
			}
			return nil
		})
	})
}

// TestScopePoolsRoundTrip is §3.2's store half: a scope's pools come back in
// the order they were written, each owned by the scope.
func TestScopePoolsRoundTrip(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s Store) {
		ctx := t.Context()
		f := newFixture()
		id, err := s.DHCP().AddScope(ctx, Scope{Name: f.scope, CIDR: f.cidr(), Enabled: true, MatchClientID: true,
			Pools: []Pool{{Start: f.host(10), End: f.host(49)}, {Start: f.host(60), End: f.host(99)}}})
		if err != nil {
			t.Fatal(err)
		}
		got, err := s.DHCP().Scope(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Pools) != 2 || got.Pools[1].Start != f.host(60) || got.Pools[0].ScopeID != id {
			t.Fatalf("pools = %+v", got.Pools)
		}
		// Scopes() attaches pools by a second query across every scope; the
		// scope has to get its own two, not none and not another's.
		all, err := s.DHCP().Scopes(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, sc := range all {
			if sc.ID == id && !reflect.DeepEqual(sc.Pools, got.Pools) {
				t.Errorf("Scopes() pools = %+v, want Scope()'s %+v", sc.Pools, got.Pools)
			}
			if sc.Pools == nil {
				t.Errorf("scope %d has nil pools; it marshals null rather than []", sc.ID)
			}
		}
		// The scope's pools go with it, in the same transaction.
		if err := s.DHCP().DeleteScope(ctx, id); err != nil {
			t.Fatal(err)
		}
		var n int
		raw := s.(*sqlStore)
		if err := raw.db.QueryRowContext(ctx, raw.q(`SELECT COUNT(*) FROM dhcp_pools WHERE scope_id = ?`), id).Scan(&n); err != nil || n != 0 {
			t.Errorf("%d pools (err %v) outlived their scope", n, err)
		}
	})
}

func TestValidateScopeRefusesOverlappingPools(t *testing.T) {
	s := Scope{Name: "lan", CIDR: "10.0.0.0/24", Enabled: true,
		Pools: []Pool{{Start: "10.0.0.10", End: "10.0.0.50"}, {Start: "10.0.0.50", End: "10.0.0.60"}}}
	err := ValidateScope(s, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "overlap") {
		t.Fatalf("err = %v, want an overlap error", err)
	}
	if want := "pools[0] 10.0.0.10-10.0.0.50 overlaps pools[1] 10.0.0.50-10.0.0.60"; err.Error() != want {
		t.Errorf("err = %q, want %q", err, want)
	}
}

func TestValidateScopeRefusesUnknownClass(t *testing.T) {
	s := Scope{Name: "lan", CIDR: "10.0.0.0/24", Enabled: true, Pools: []Pool{{Start: "10.0.0.10", End: "10.0.0.50", ClassID: 7}}}
	if err := ValidateScope(s, nil, []Class{{ID: 1, Name: "iot"}}); err == nil || !strings.Contains(err.Error(), "class") {
		t.Fatalf("err = %v, want an unknown-class error", err)
	}
}

// TestValidateClassMatchers is the matcher grammar the renderer rests on:
// anything accepted here becomes a Kea expression, so what the engine could
// not evaluate has to stop here.
func TestValidateClassMatchers(t *testing.T) {
	ok := Class{Name: "iot", Matchers: []string{"vendor:PXEClient", "mac:A4:CF:12", "mac:a4", "mac:a4:cf:12:00:11:22"}}
	if err := ValidateClass(ok, nil); err != nil {
		t.Fatal(err)
	}
	for _, bad := range [][]string{
		nil,
		{"mac:a4:cf:12:00:11:22:33"}, // seven octets: longer than any hardware address it prefixes
		{"vendor:it's"},              // the renderer quotes the prefix, and Kea has no escape
		{"host:x"},
		{"mac:zz"},
		{"mac:a4cf12"}, // bytes are colon-separated
		{"mac:a"},
		{"mac:"},
		{"vendor:"},
		{"vendor:PXE\x01"},
		{"vendor:" + strings.Repeat("a", 256)},
		{"PXEClient"},
	} {
		if err := ValidateClass(Class{Name: "c", Matchers: bad}, nil); err == nil {
			t.Errorf("matchers %q accepted", bad)
		}
	}
	if err := ValidateClass(Class{Name: "c", Matchers: []string{"vendor:" + strings.Repeat("a", 255)}}, nil); err != nil {
		t.Errorf("a 255-byte vendor prefix refused: %v", err)
	}
	if err := ValidateClass(Class{Name: "IOT", Matchers: []string{"mac:aa"}}, []Class{{ID: 2, Name: "iot"}}); !errors.Is(err, ErrDuplicate) {
		t.Errorf("duplicate name (case-insensitive) = %v, want ErrDuplicate", err)
	}
	if err := ValidateClass(Class{ID: 2, Name: "IOT", Matchers: []string{"mac:aa"}}, []Class{{ID: 2, Name: "iot"}}); err != nil {
		t.Errorf("renaming a class's own case refused: %v", err)
	}
	for _, name := range []string{"", "  ", strings.Repeat("a", 64)} {
		if err := ValidateClass(Class{Name: name, Matchers: []string{"mac:aa"}}, nil); err == nil {
			t.Errorf("name %q accepted", name)
		}
	}
	// Names Kea's namespace already holds: the renderer's guards and Kea's
	// own classes, in any case.
	for name, want := range map[string]string{
		"dnsaur-open-1": "names starting with dnsaur- are reserved",
		"DNSAUR-x":      "names starting with dnsaur- are reserved",
		"drop":          "drop is a class Kea defines itself",
		"KNOWN":         "KNOWN is a class Kea defines itself",
		"Unknown":       "Unknown is a class Kea defines itself",
		"all":           "all is a class Kea defines itself",
		"booting":       "booting is a class Kea defines itself",
	} {
		err := ValidateClass(Class{Name: name, Matchers: []string{"mac:aa"}}, nil)
		if err == nil || err.Error() != want {
			t.Errorf("name %q: error %v, want %q", name, err, want)
		}
	}
	if err := ValidateClass(Class{Name: "dnsaur", Matchers: []string{"mac:aa"}}, nil); err != nil {
		t.Errorf("name dnsaur (no hyphen) refused: %v", err)
	}
	// The options are the scope's rules, applied to a class.
	for _, o := range []ClientOptions{
		{DNSServers: "bad"},
		{Domain: "home.lan."},
		{Options: []GenericOption{{Code: 6, Hex: "0A000001"}}},
		{StaticRoutes: []StaticRoute{{Destination: "10.10.0.5/16", Router: "10.0.0.1"}}},
		{BootFile: strings.Repeat("a", 128)},
	} {
		if err := ValidateClass(Class{Name: "c", Matchers: []string{"mac:aa"}, ClientOptions: o}, nil); err == nil {
			t.Errorf("class options %+v accepted", o)
		}
	}
}

// TestClassRoundTrip is the class half of the store: what goes in comes back
// normalised, an edit lands, and an unknown id is ErrNotFound on every path.
func TestClassRoundTrip(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s Store) {
		ctx := t.Context()
		f := newFixture()
		dhcp := s.DHCP()
		in := Class{
			Name: " " + f.group + " ", Matchers: []string{" MAC:ignored", "mac:A4:CF:12 ", "vendor:PXEClient"},
			ClientOptions: ClientOptions{
				DNSServers: " 10.0.0.2 ,10.0.0.3", Domain: "iot.lan", NextServer: " 10.0.0.9",
				StaticRoutes: []StaticRoute{{Destination: " 10.10.0.0/16", Router: "10.0.0.1 "}},
				Options:      []GenericOption{{Code: 150, Hex: "0a:2a"}},
			},
			CreatedAt: 1757800000000, ModifiedAt: 1757800000000,
		}
		id, err := dhcp.AddClass(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		want := Class{
			ID: id, Name: f.group, Matchers: []string{"MAC:ignored", "mac:a4:cf:12", "vendor:PXEClient"},
			ClientOptions: ClientOptions{
				DNSServers: "10.0.0.2, 10.0.0.3", Domain: "iot.lan", NextServer: "10.0.0.9",
				StaticRoutes: []StaticRoute{{Destination: "10.10.0.0/16", Router: "10.0.0.1"}},
				Options:      []GenericOption{{Code: 150, Hex: "0A2A"}},
			},
			CreatedAt: 1757800000000, ModifiedAt: 1757800000000,
		}
		if got, err := dhcp.Class(ctx, id); err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("Class(%d) = %+v (err %v), want %+v", id, got, err, want)
		}
		if in.Matchers[1] != "mac:A4:CF:12 " {
			t.Error("AddClass normalised the caller's own matchers slice in place")
		}
		want.Matchers, want.StaticRoutes, want.Options, want.ModifiedAt = []string{"vendor:MSFT"}, nil, nil, 1757800001000
		if err := dhcp.UpdateClass(ctx, want); err != nil {
			t.Fatal(err)
		}
		want.StaticRoutes, want.Options = []StaticRoute{}, []GenericOption{}
		all, err := dhcp.Classes(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if i := slices.IndexFunc(all, func(c Class) bool { return c.ID == id }); i < 0 || !reflect.DeepEqual(all[i], want) {
			t.Errorf("Classes() = %+v, want it to hold %+v", all, want)
		}
		if _, err := dhcp.Class(ctx, 987654321); !errors.Is(err, ErrNotFound) {
			t.Errorf("Class(unknown) = %v, want ErrNotFound", err)
		}
		if err := dhcp.UpdateClass(ctx, Class{ID: 987654321, Name: "x"}); !errors.Is(err, ErrNotFound) {
			t.Errorf("UpdateClass(unknown) = %v, want ErrNotFound", err)
		}
		if err := dhcp.DeleteClass(ctx, 987654321); !errors.Is(err, ErrNotFound) {
			t.Errorf("DeleteClass(unknown) = %v, want ErrNotFound", err)
		}
		if _, err := dhcp.AddClass(ctx, Class{Name: f.group, Matchers: []string{"mac:aa"}}); !errors.Is(err, ErrDuplicate) {
			t.Errorf("a second class named %q = %v, want ErrDuplicate", f.group, err)
		}
		if err := dhcp.DeleteClass(ctx, id); err != nil {
			t.Fatal(err)
		}
		if _, err := dhcp.Class(ctx, id); !errors.Is(err, ErrNotFound) {
			t.Errorf("Class(%d) after DeleteClass = %v, want ErrNotFound", id, err)
		}
	})
}

// TestDeleteClassInUseIsRefused is §7's first failure mode: a class a pool
// names cannot go, and the refusal names every scope holding such a pool so
// the operator knows where to look. Nothing moves until it is refused.
func TestDeleteClassInUseIsRefused(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s Store) {
		ctx := t.Context()
		f, g := newFixture(), newFixture()
		cid, err := s.DHCP().AddClass(ctx, Class{Name: f.group, Matchers: []string{"mac:a4:cf:12"}})
		if err != nil {
			t.Fatal(err)
		}
		for _, sc := range []Scope{
			{Name: f.scope, CIDR: f.cidr(), Enabled: true, MatchClientID: true,
				Pools: []Pool{{Start: f.host(10), End: f.host(49), ClassID: cid}, {Start: f.host(50), End: f.host(59), ClassID: cid}}},
			{Name: g.scope, CIDR: g.cidr(), Enabled: true, MatchClientID: true,
				Pools: []Pool{{Start: g.host(10), End: g.host(49), ClassID: cid}}},
		} {
			if _, err := s.DHCP().AddScope(ctx, sc); err != nil {
				t.Fatal(err)
			}
		}
		before, _ := s.Settings().ConfigVersion(ctx)
		err = s.DHCP().DeleteClass(ctx, cid)
		if !errors.Is(err, ErrClassInUse) || !strings.Contains(err.Error(), f.scope) {
			t.Fatalf("DeleteClass = %v, want ErrClassInUse naming %s", err, f.scope)
		}
		want := fmt.Sprintf("class %q is in use by 3 pools (%s, %s)", f.group, f.scope, g.scope)
		if err.Error() != want {
			t.Errorf("DeleteClass = %q, want %q", err, want)
		}
		if after, _ := s.Settings().ConfigVersion(ctx); after != before {
			t.Errorf("a refused DeleteClass moved config_version %d -> %d", before, after)
		}
		if _, err := s.DHCP().Class(ctx, cid); err != nil {
			t.Errorf("the class is gone after a refused delete: %v", err)
		}
	})
}

// TestMigration0017CopiesThePool is 0017's upgrade path: a scope an older
// binary wrote, with its one pool in two columns, comes out with that pool as
// pool 0, and the two columns are gone. A reservations_only scope that held
// no pool keeps none rather than gaining an empty one.
func TestMigration0017CopiesThePool(t *testing.T) {
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "pre0017.db")
	raw, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	sub, err := fs.Sub(migrationsFS, "migrations/sqlite")
	if err != nil {
		t.Fatal(err)
	}
	provider, err := goose.NewProvider(goose.DialectSQLite3, raw, sub, goose.WithGoMigrations(
		goose.NewGoMigration(5, &goose.GoFunc{RunTx: func(ctx context.Context, tx *sql.Tx) error {
			return upZonesData(ctx, tx, "sqlite")
		}}, nil),
		goose.NewGoMigration(7, &goose.GoFunc{RunTx: func(ctx context.Context, tx *sql.Tx) error {
			return upBuiltinZones(ctx, tx, "sqlite")
		}}, nil),
	))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.UpTo(ctx, 16); err != nil {
		t.Fatalf("legacy migrate: %v", err)
	}
	if _, err := raw.ExecContext(ctx, `INSERT INTO dhcp_scopes (id, name, cidr, pool_start, pool_end, reservations_only, created_at, modified_at) VALUES
		(1, 'lan', '10.0.0.0/24', '10.0.0.100', '10.0.0.200', 0, 1, 1),
		(2, 'fixed', '10.1.0.0/24', '', '', 1, 1, 1)`); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(ctx, "sqlite", dsn)
	if err != nil {
		t.Fatalf("upgrading to 0017: %v", err)
	}
	defer func() { _ = s.Close() }()
	lan, err := s.DHCP().Scope(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(lan.Pools) != 1 {
		t.Fatalf("migrated pools = %+v, want exactly one", lan.Pools)
	}
	if want := []Pool{{ID: lan.Pools[0].ID, ScopeID: 1, Start: "10.0.0.100", End: "10.0.0.200"}}; !reflect.DeepEqual(lan.Pools, want) {
		t.Errorf("migrated pools = %+v, want %+v", lan.Pools, want)
	}
	if fixed, err := s.DHCP().Scope(ctx, 2); err != nil || len(fixed.Pools) != 0 {
		t.Errorf("reservations_only scope pools = %+v (err %v), want none", fixed.Pools, err)
	}
	rows, err := s.(*sqlStore).db.QueryContext(ctx, `SELECT name FROM pragma_table_info('dhcp_scopes')`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var col string
		if err := rows.Scan(&col); err != nil {
			t.Fatal(err)
		}
		if col == "pool_start" || col == "pool_end" {
			t.Errorf("dhcp_scopes still has %s after 0017", col)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}

// TestScopeWriteRefusesADeletedClass is the race between the API's
// ValidateScope and the write: a class deleted after the form was validated
// must not end up named by a pool. Both write paths share the check.
func TestScopeWriteRefusesADeletedClass(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s Store) {
		ctx := t.Context()
		f := newFixture()
		dhcp := s.DHCP()
		cid, err := dhcp.AddClass(ctx, Class{Name: f.group, Matchers: []string{"mac:aa"}})
		if err != nil {
			t.Fatal(err)
		}
		sc := Scope{Name: f.scope, CIDR: f.cidr(), Enabled: true, MatchClientID: true,
			Pools: []Pool{{Start: f.host(10), End: f.host(19)}, {Start: f.host(20), End: f.host(29), ClassID: cid}}}
		classes, err := dhcp.Classes(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := ValidateScope(sc, nil, classes); err != nil {
			t.Fatal(err)
		}
		// Validated; now the class goes before the write lands.
		if err := dhcp.DeleteClass(ctx, cid); err != nil {
			t.Fatal(err)
		}
		before, _ := s.Settings().ConfigVersion(ctx)
		want := fmt.Sprintf("pools[1]: no class with id %d", cid)
		_, err = dhcp.AddScope(ctx, sc)
		if !errors.Is(err, ErrReference) || !strings.HasPrefix(err.Error(), want) {
			t.Fatalf("AddScope = %v, want ErrReference starting %q", err, want)
		}
		if after, _ := s.Settings().ConfigVersion(ctx); after != before {
			t.Errorf("a refused AddScope moved config_version %d -> %d", before, after)
		}
		all, err := dhcp.Scopes(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if slices.ContainsFunc(all, func(x Scope) bool { return x.Name == f.scope }) {
			t.Error("the scope was created despite the refused pool")
		}

		sc.Pools = sc.Pools[:1]
		id, err := dhcp.AddScope(ctx, sc)
		if err != nil {
			t.Fatal(err)
		}
		sc.ID, sc.Pools = id, []Pool{{Start: f.host(10), End: f.host(19), ClassID: cid}}
		if err := dhcp.UpdateScope(ctx, sc); !errors.Is(err, ErrReference) {
			t.Errorf("UpdateScope = %v, want ErrReference", err)
		}
		if got, err := dhcp.Scope(ctx, id); err != nil || len(got.Pools) != 1 || got.Pools[0].ClassID != 0 {
			t.Errorf("pools after a refused UpdateScope = %+v (err %v), want the one class-free pool kept", got.Pools, err)
		}
	})
}
