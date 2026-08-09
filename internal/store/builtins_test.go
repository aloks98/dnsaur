package store

import (
	"context"
	"database/sql"
	"io/fs"
	"path/filepath"
	"slices"
	"testing"

	"github.com/pressly/goose/v3"
)

// TestBuiltinZonesIsTheRFC6303Set pins the set itself, as literals. Every
// other assertion in this file — and the exact zone counts in store_test.go
// and internal/api's zone tests — derives its expectation from BuiltinZones,
// the variable under test, so both sides of those comparisons move together:
// they catch a zone leaking *in*, and nothing at all when one goes missing.
// Deleting an entry from builtins.go left the whole package green. This
// branch's premise is that these names answer locally rather than leaving
// the network (RFC 6303 §3, plus "localhost" itself — RFC 6761 §6.3, not
// RFC 6303; see builtins.go's doc comments), so the names are the
// requirement and belong written out here, where changing them has to be
// deliberate.
func TestBuiltinZonesIsTheRFC6303Set(t *testing.T) {
	want := []string{
		"localhost",

		// RFC 6303 §4.2.
		"0.in-addr.arpa",
		"127.in-addr.arpa",
		"254.169.in-addr.arpa",
		"2.0.192.in-addr.arpa",
		"100.51.198.in-addr.arpa",
		"113.0.203.in-addr.arpa",
		"255.255.255.255.in-addr.arpa",

		// RFC 6303 §4.3 — ::, then ::1, one nibble per label, reversed
		// (RFC 3596 §2.5).
		"0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.ip6.arpa",
		"1.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.ip6.arpa",

		// RFC 6303 §4.4 (d.f.ip6.arpa) is deliberately absent: fd00::/8 is
		// IPv6 ULA (RFC 4193) — the IPv6 equivalent of RFC 1918 private
		// space, and exactly what a homelab numbers its own LAN with. See
		// TestPrivateReverseZonesNotSeeded and BuiltinZones's doc comment.

		// RFC 6303 §4.5.
		"8.e.f.ip6.arpa",
		"9.e.f.ip6.arpa",
		"a.e.f.ip6.arpa",
		"b.e.f.ip6.arpa",

		// RFC 6303 §4.6.
		"8.b.d.0.1.0.0.2.ip6.arpa",
	}
	if len(BuiltinZones) != len(want) {
		t.Fatalf("BuiltinZones has %d zones, want %d: %q", len(BuiltinZones), len(want), BuiltinZones)
	}
	if !slices.Equal(BuiltinZones, want) {
		t.Errorf("BuiltinZones = %q, want %q", BuiltinZones, want)
	}
}

func TestBuiltinZonesSeeded(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		zs, err := s.Zones().Zones(ctx)
		if err != nil {
			t.Fatalf("Zones: %v", err)
		}
		got := map[string]Zone{}
		for _, z := range zs {
			got[z.Name] = z
		}
		for _, name := range BuiltinZones {
			z, ok := got[name]
			if !ok {
				t.Errorf("built-in zone %q was not seeded", name)
				continue
			}
			if z.Type != "internal" {
				t.Errorf("%s type = %q, want internal", name, z.Type)
			}
			if !z.Enabled {
				t.Errorf("%s is disabled; a built-in that does not answer is worse than none", name)
			}
			recs, _ := s.Zones().Records(ctx, z.ID)
			var hasNS bool
			for _, r := range recs {
				if r.Name == "@" && r.Type == "NS" {
					hasNS = true
				}
			}
			if !hasNS {
				t.Errorf("%s has no apex NS record (RFC 2181 §10.1)", name)
			}
		}
	})
}

// RFC 6303 lists the RFC 1918 reverse ranges too, and (§4.4) d.f.ip6.arpa —
// fd00::/8, IPv6 ULA, the RFC 1918 of IPv6. Seeding any of them would make a
// PTR for the user's own LAN impossible: an empty authoritative zone
// NXDOMAINs everything under it.
func TestPrivateReverseZonesNotSeeded(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s Store) {
		zs, _ := s.Zones().Zones(context.Background())
		for _, z := range zs {
			for _, banned := range []string{"10.in-addr.arpa", "168.192.in-addr.arpa", "16.172.in-addr.arpa", "d.f.ip6.arpa"} {
				if z.Name == banned {
					t.Errorf("seeded %q — this blocks the user's own PTR records", banned)
				}
			}
		}
	})
}

// TestBuiltinZonesSeededExactlyOnce checks the steady state after a single
// Open: no built-in name appears more than once. It does NOT exercise the
// skip-if-exists guard in seedBuiltinZones — goose only ever invokes
// migration 7 once per database (its own version bookkeeping, not this
// package's code), so this test would pass identically even with the guard
// deleted. See TestBuiltinSeedSkipsPreexistingZone below for the guard
// itself: the real risk is a name that already exists the one time the
// migration does run, not the migration running twice.
func TestBuiltinZonesSeededExactlyOnce(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s Store) {
		zs, _ := s.Zones().Zones(context.Background())
		seen := map[string]int{}
		for _, z := range zs {
			seen[z.Name]++
		}
		for _, name := range BuiltinZones {
			if seen[name] > 1 {
				t.Errorf("%s seeded %d times", name, seen[name])
			}
		}
	})
}

// TestBuiltinSeedSkipsPreexistingZone stages the database exactly the way a
// real upgrade would find it: migrated up through version 6 (zones and
// soa_ttl exist), with a "localhost" zone already sitting there — created by
// hand, or by an older binary, before this package's version-7 migration
// ever runs — and then lets Open run that migration for the first time.
//
// This is the scenario seedBuiltinZones's existence check exists for, and
// TestBuiltinZonesSeededExactlyOnce above cannot cover it: that test only
// ever sees a fresh database, where migration 7 has nothing to collide
// with. Here it does, on its one and only invocation, so the guard is what
// stands between a clean upgrade and a unique-index violation on
// zones.name.
func TestBuiltinSeedSkipsPreexistingZone(t *testing.T) {
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "preexisting.db")

	raw, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	sub, err := fs.Sub(migrationsFS, "migrations/sqlite")
	if err != nil {
		t.Fatal(err)
	}
	// A provider that knows about migration 5 (needed to reach 6 at all)
	// but not yet 7 — version 7 is deliberately left unregistered here, so
	// UpTo(6) stops one migration short of the one under test, the same way
	// TestMigrateExistingZoneGainsSOATTL stages a pre-0006 database.
	provider, err := goose.NewProvider(goose.DialectSQLite3, raw, sub, goose.WithGoMigrations(
		goose.NewGoMigration(5, &goose.GoFunc{RunTx: func(ctx context.Context, tx *sql.Tx) error {
			return upZonesData(ctx, tx, "sqlite")
		}}, nil),
	))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.UpTo(ctx, 6); err != nil {
		t.Fatalf("legacy migrate: %v", err)
	}
	// The user's own zone, with data the seed's generated defaults do not
	// share (soa_serial, soa_ns, type) — so a mangled result is easy to
	// tell apart from an untouched one.
	if _, err := raw.ExecContext(ctx,
		`INSERT INTO zones (name, type, enabled, soa_ns, soa_mbox, soa_serial, soa_ttl) VALUES ('localhost', 'primary', 1, 'ns.mine.test', 'me.mine.test', 42, 111)`); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(ctx, "sqlite", dsn)
	if err != nil {
		t.Fatalf("upgrade with a pre-existing localhost zone failed: %v", err)
	}
	defer func() { _ = s.Close() }()

	zs, err := s.Zones().Zones(ctx)
	if err != nil {
		t.Fatalf("reading zones after upgrade: %v", err)
	}
	var localhosts []Zone
	for _, z := range zs {
		if z.Name == "localhost" {
			localhosts = append(localhosts, z)
		}
	}
	if len(localhosts) != 1 {
		t.Fatalf("got %d zones named localhost, want exactly 1: %+v", len(localhosts), localhosts)
	}
	// The pre-existing row must win untouched, not be overwritten by the
	// seed's generated defaults.
	if localhosts[0].SOANS != "ns.mine.test" || localhosts[0].SOASerial != 42 || localhosts[0].Type != "primary" {
		t.Errorf("pre-existing localhost was overwritten by the seed instead of skipped: %+v", localhosts[0])
	}
}

// TestBuiltinZoneRecordsIsTheRFC6303Content pins builtinRecords the same way
// TestBuiltinZonesIsTheRFC6303Set pins BuiltinZones above: as literals, not
// derived from anything else in the package. builtinRecords is what
// seedBuiltinZoneRecords reads, so if this test only checked "every zone in
// the store has the records builtinRecords says it should", a wrong entry
// in builtinRecords itself — a typo'd RData, a record attached to the wrong
// zone — would sail through undetected on both sides of every other
// assertion in this file that starts from it.
func TestBuiltinZoneRecordsIsTheRFC6303Content(t *testing.T) {
	want := []builtinZoneRecord{
		// RFC 6303 has no forward-zone entry for "localhost" — the reverse
		// zones are what it specifies. This pair is the well-known
		// loopback aliases (RFC 6761 §6.3) filling the gap the same way
		// this package's own "localhost" built-in already does: answering
		// locally instead of NXDOMAINing a name every stack expects to
		// resolve.
		{Zone: "localhost", Name: "@", Type: "A", RData: "127.0.0.1"},
		{Zone: "localhost", Name: "@", Type: "AAAA", RData: "::1"},
		// RFC 6303 §4.2.
		{Zone: "127.in-addr.arpa", Name: "1.0.0", Type: "PTR", RData: "localhost."},
		// RFC 6303 §4.2 as well — "Similar logic applies to the reverse
		// mapping for ::1" is the sentence right after the 127.in-addr.arpa
		// one above; §4.3 only tabulates the zone name. Zone name is ::1's
		// reverse mapping (RFC 3596 §2.5), one nibble per label, reversed;
		// the PTR sits at the apex because every nibble is already consumed
		// by the zone name itself.
		{Zone: "1.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.ip6.arpa", Name: "@", Type: "PTR", RData: "localhost."},
	}
	if !slices.Equal(builtinRecords, want) {
		t.Errorf("builtinRecords = %+v, want %+v", builtinRecords, want)
	}
}

// TestBuiltinZoneRecordsSeeded checks that each of the three built-in zones
// with content has exactly that content after a fresh Open — the apex NS
// record every built-in zone gets (TestBuiltinZonesSeeded above) plus
// exactly the records builtinRecords lists for it, by literal
// name/type/rdata, and nothing else.
func TestBuiltinZoneRecordsSeeded(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		zs, err := s.Zones().Zones(ctx)
		if err != nil {
			t.Fatalf("Zones: %v", err)
		}
		byName := map[string]Zone{}
		for _, z := range zs {
			byName[z.Name] = z
		}

		cases := []struct {
			zone string
			want []ZoneRecord
		}{
			{
				zone: "localhost",
				want: []ZoneRecord{
					{Name: "@", Type: "A", RData: "127.0.0.1"},
					{Name: "@", Type: "AAAA", RData: "::1"},
				},
			},
			{
				zone: "127.in-addr.arpa",
				want: []ZoneRecord{
					{Name: "1.0.0", Type: "PTR", RData: "localhost."},
				},
			},
			{
				zone: "1.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.ip6.arpa",
				want: []ZoneRecord{
					{Name: "@", Type: "PTR", RData: "localhost."},
				},
			},
		}

		for _, c := range cases {
			z, ok := byName[c.zone]
			if !ok {
				t.Fatalf("built-in zone %q was not seeded", c.zone)
			}
			recs, err := s.Zones().Records(ctx, z.ID)
			if err != nil {
				t.Fatalf("Records(%s): %v", c.zone, err)
			}
			var got []ZoneRecord
			for _, r := range recs {
				if r.Name == "@" && r.Type == "NS" {
					continue // the apex NS every built-in zone gets — not content.
				}
				got = append(got, ZoneRecord{Name: r.Name, Type: r.Type, RData: r.RData})
			}
			if len(got) != len(c.want) {
				t.Fatalf("%s non-NS records = %+v, want exactly %+v", c.zone, got, c.want)
			}
			for _, w := range c.want {
				var found bool
				for _, g := range got {
					if g == w {
						found = true
					}
				}
				if !found {
					t.Errorf("%s missing record %+v; got %+v", c.zone, w, got)
				}
			}
		}
	})
}

// TestEmptyReverseZonesStayEmpty asserts that every built-in zone other than
// "localhost", 127.in-addr.arpa and the ::1 zone carries only its apex NS
// after a fresh Open — nothing from builtinRecords, because nothing is in
// there for them. RFC 6303 gives no "a meaningful mapping should exist"
// language for the unspecified address, broadcast, link-local, the
// TEST-NETs, the IPv6 documentation prefix, or the IPv6 link-local ranges
// the way it does for 127.in-addr.arpa and ::1 (§4.2), so an empty
// authoritative zone — NXDOMAIN for everything under it — is the correct,
// deliberate answer for the rest. This test exists so a future "helpful"
// addition to any of them has to change this assertion, not silently pass
// it. The list is written out literally (not "every zone in BuiltinZones
// minus the three in builtinRecords") for the same reason
// TestBuiltinZonesIsTheRFC6303Set pins BuiltinZones as literals: it must not
// move in lockstep with the variables it is checking. d.f.ip6.arpa (RFC
// 6303 §4.4) is not in this list because it is not in BuiltinZones at all —
// see TestPrivateReverseZonesNotSeeded.
func TestEmptyReverseZonesStayEmpty(t *testing.T) {
	empty := []string{
		"0.in-addr.arpa",
		"254.169.in-addr.arpa",
		"2.0.192.in-addr.arpa",
		"100.51.198.in-addr.arpa",
		"113.0.203.in-addr.arpa",
		"255.255.255.255.in-addr.arpa",
		"0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.ip6.arpa",
		"8.e.f.ip6.arpa",
		"9.e.f.ip6.arpa",
		"a.e.f.ip6.arpa",
		"b.e.f.ip6.arpa",
		"8.b.d.0.1.0.0.2.ip6.arpa",
	}
	forEachDriver(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		zs, err := s.Zones().Zones(ctx)
		if err != nil {
			t.Fatalf("Zones: %v", err)
		}
		byName := map[string]Zone{}
		for _, z := range zs {
			byName[z.Name] = z
		}
		for _, name := range empty {
			z, ok := byName[name]
			if !ok {
				t.Fatalf("built-in zone %q was not seeded", name)
			}
			recs, err := s.Zones().Records(ctx, z.ID)
			if err != nil {
				t.Fatalf("Records(%s): %v", name, err)
			}
			if len(recs) != 1 || recs[0].Name != "@" || recs[0].Type != "NS" {
				t.Errorf("%s records = %+v, want exactly the apex NS", name, recs)
			}
		}
	})
}

// TestSeedBuiltinZoneRecordsIdempotent calls seedBuiltinZoneRecords directly
// a second time against an already-seeded store and checks nothing
// duplicates — exercised directly, not by relying on goose's own
// once-per-version bookkeeping to prove it, since that bookkeeping only ever
// invokes migration 7 once per database and would hide a broken existence
// check rather than catch one.
func TestSeedBuiltinZoneRecordsIdempotent(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		ss, ok := s.(*sqlStore)
		if !ok {
			t.Fatalf("Store is not *sqlStore: %T", s)
		}

		if err := seedBuiltinZoneRecords(ctx, ss.db, ss.dialect); err != nil {
			t.Fatalf("seedBuiltinZoneRecords (rerun): %v", err)
		}

		zs, err := s.Zones().Zones(ctx)
		if err != nil {
			t.Fatalf("Zones: %v", err)
		}
		byName := map[string]Zone{}
		for _, z := range zs {
			byName[z.Name] = z
		}
		for _, rec := range builtinRecords {
			z, ok := byName[rec.Zone]
			if !ok {
				t.Fatalf("built-in zone %q was not seeded", rec.Zone)
			}
			recs, err := s.Zones().Records(ctx, z.ID)
			if err != nil {
				t.Fatalf("Records(%s): %v", rec.Zone, err)
			}
			var n int
			for _, r := range recs {
				if r.Name == rec.Name && r.Type == rec.Type {
					n++
				}
			}
			if n != 1 {
				t.Errorf("%s %s %s appears %d times after a second seed pass, want 1", rec.Zone, rec.Name, rec.Type, n)
			}
		}
	})
}

// TestBuiltinZoneRecordsSkipPreexistingZone is TestBuiltinSeedSkipsPreexistingZone's
// content-side twin: a "localhost" zone created by hand (type "primary")
// before this package's migration ever ran must not receive the built-in
// records either, only the zone row itself must survive untouched.
// seedBuiltinZoneRecords's type check ("internal" only) is what stands
// between this and a homelab's own localhost zone gaining loopback A/AAAA
// records it never asked for.
func TestBuiltinZoneRecordsSkipPreexistingZone(t *testing.T) {
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "preexisting-localhost-records.db")

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
	))
	if err != nil {
		t.Fatal(err)
	}
	// Stop one migration short of 7, same staging as
	// TestBuiltinSeedSkipsPreexistingZone.
	if _, err := provider.UpTo(ctx, 6); err != nil {
		t.Fatalf("legacy migrate: %v", err)
	}
	if _, err := raw.ExecContext(ctx,
		`INSERT INTO zones (name, type, enabled, soa_ns, soa_mbox, soa_serial, soa_ttl) VALUES ('localhost', 'primary', 1, 'ns.mine.test', 'me.mine.test', 42, 111)`); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	// Open runs migration 7, which seeds zones and their content in one
	// pass — the point here is that the pre-existing "localhost" row is
	// skipped by both halves of it.
	s, err := Open(ctx, "sqlite", dsn)
	if err != nil {
		t.Fatalf("upgrade with a pre-existing localhost zone failed: %v", err)
	}
	defer func() { _ = s.Close() }()

	zs, err := s.Zones().Zones(ctx)
	if err != nil {
		t.Fatalf("reading zones after upgrade: %v", err)
	}
	var localhost *Zone
	for i := range zs {
		if zs[i].Name == "localhost" {
			localhost = &zs[i]
		}
	}
	if localhost == nil {
		t.Fatalf("localhost zone missing after upgrade")
	}
	if localhost.Type != "primary" {
		t.Fatalf("localhost zone type = %q, want primary — it must still be the user's own zone, not a replacement", localhost.Type)
	}
	recs, err := s.Zones().Records(ctx, localhost.ID)
	if err != nil {
		t.Fatalf("Records: %v", err)
	}
	if len(recs) != 0 {
		t.Errorf("hand-created localhost zone gained records from the built-in seed: %+v", recs)
	}
}
