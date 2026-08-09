package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/miekg/dns"
)

// BuiltinZones is "localhost" plus every zone RFC 6303 §4 lists, except the
// ranges a homelab might actually number its own LAN with:
//
//   - The RFC 1918 private-address ranges in §4.1 (10.in-addr.arpa,
//     16.172.in-addr.arpa through 31.172.in-addr.arpa, 168.192.in-addr.arpa).
//   - d.f.ip6.arpa (§4.4): fd00::/8 is IPv6 Unique Local Addresses (RFC
//     4193) — the IPv6 equivalent of RFC 1918 private space, and exactly
//     what a homelab numbers its LAN with if it uses IPv6 at all. RFC 6303
//     §4.4 lists this zone because RFC 4193 §4.4 already required treating
//     it as locally served, but that is a recommendation about not leaking
//     ULA queries to the public Internet, not a reason to make PTRs under
//     it impossible. Do not re-add this from a reading of §4.4 alone.
//
// All of the above stay excluded deliberately: an empty authoritative zone
// NXDOMAINs everything under it, so seeding e.g. 168.192.in-addr.arpa or
// d.f.ip6.arpa would make a PTR record for the user's own LAN impossible —
// the main reason a homelab wants reverse DNS in the first place. (Proof
// this isn't hypothetical: TestAutoPTRCreatesRecordForAAAA in
// internal/api/autoptr_test.go uses an ordinary fd00::/8 address as its
// fixture specifically so that re-adding d.f.ip6.arpa here breaks that test
// and explains why.)
//
// "localhost" itself is not in RFC 6303 — that RFC's §4 lists only reverse
// zones, nothing forward. It answers locally for the same reason the RFC
// 6303 zones do (RFC 6761 §6.3; see builtinRecords's doc comment), and has
// been part of this built-in set from the start, so it stays first in the
// list below rather than being carved out separately.
//
// Every entry after "localhost" was checked directly against the RFC 6303
// text (not transcribed from a prior summary), grouped and commented by
// the RFC subsection it came from so a future re-check has something to
// diff against.
//
// Exported (not just package-private) because any caller outside this
// package that needs to name or count the built-in set — e.g. the
// exact-count assertions in internal/api's zone tests — must be able to
// read the same apexes this seed uses, not a second hardcoded copy that
// could drift from it.
var BuiltinZones = []string{
	"localhost",

	// §4.2: RFC 5735/5737 IPv4 ranges not expected as source or
	// destination addresses on the public Internet.
	"0.in-addr.arpa",               // "THIS" NETWORK
	"127.in-addr.arpa",             // Loopback NETWORK
	"254.169.in-addr.arpa",         // LINK LOCAL
	"2.0.192.in-addr.arpa",         // TEST-NET-1
	"100.51.198.in-addr.arpa",      // TEST-NET-2
	"113.0.203.in-addr.arpa",       // TEST-NET-3
	"255.255.255.255.in-addr.arpa", // BROADCAST

	// §4.3: reverse mappings for the IPv6 Unspecified (::) and Loopback
	// (::1) addresses (RFC 3596 §2.5: 32 nibbles, one per label, reversed
	// — every nibble of these two addresses is consumed by the zone name
	// itself, leaving no room for a delegated sub-zone under either).
	"0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.ip6.arpa", // ::
	"1.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.ip6.arpa", // ::1

	// §4.4 (IPv6 locally-assigned local addresses, d.f.ip6.arpa) is
	// deliberately not here — see the exclusion list in the doc comment
	// above: fd00::/8 is ULA, a homelab's own IPv6 LAN space.

	// §4.5: IPv6 link-local addresses (RFC 4291 §2.5.6), four zones —
	// fe80::/10 leaves two bits free within this label's nibble, which
	// gives it four possible hex values (8-b), and the RFC lists all four
	// individually rather than one zone per possible boundary.
	"8.e.f.ip6.arpa",
	"9.e.f.ip6.arpa",
	"a.e.f.ip6.arpa",
	"b.e.f.ip6.arpa",

	// §4.6: the RFC 3849 IPv6 documentation prefix (2001:db8::/32). The
	// RFC's own footnote on this entry — "8.B.D.0.1.0.0.2.IP6.ARPA is not
	// being used as an example here" — says this literal zone name is the
	// one to register, not a placeholder standing in for some other name.
	"8.b.d.0.1.0.0.2.ip6.arpa",
}

// builtinZoneRecord is one content record for a built-in zone, named
// relative to that zone's apex — the same "@", "1.0.0" shape a
// zone_records row's Name column holds. Not every entry in builtinRecords
// is RFC 6303 §4 content — see that variable's doc comment for which
// citation applies to which record.
type builtinZoneRecord struct {
	Zone  string // one of BuiltinZones
	Name  string
	Type  string
	RData string
}

// builtinRecords is the content for the three built-in zones that have
// any — two different RFCs, not one. Every other zone in BuiltinZones is
// deliberately empty (SOA + apex NS only): RFC 6303 gives no "a meaningful
// mapping should exist" language for any range besides the two below, and
// an empty authoritative zone NXDOMAINing everything under it is exactly
// the correct answer for the rest — the unspecified address, broadcast,
// link-local, the TEST-NETs, the IPv6 documentation prefix, and the IPv6
// link-local ranges.
//
//   - "localhost" resolves to the loopback addresses itself: RFC 6761
//     §6.3, not RFC 6303 — RFC 6303 §4 has no forward-zone entry for
//     "localhost" at all, only reverse zones. Without this pair, RFC
//     6303's empty-zone default would NXDOMAIN "localhost A", which is
//     exactly the leak-to-the-network behaviour this package exists to
//     stop — see the package doc comment above.
//   - 127.in-addr.arpa, RFC 6303 §4.2: "The recommendation to serve an
//     empty zone ... is not an attempt to discourage any practice to
//     provide a PTR RR for 1.0.0.127.IN-ADDR.ARPA locally. In fact, a
//     meaningful reverse mapping should exist ..." 1.0.0 is that name's
//     label sequence relative to the 127.in-addr.arpa apex.
//   - The ::1 reverse zone, RFC 6303 §4.2 again: "Similar logic applies
//     to the reverse mapping for ::1" is the very next sentence after the
//     127.in-addr.arpa one above — §4.3 only tabulates the zone name
//     itself, it doesn't repeat the recommendation. Every nibble of ::1's
//     reverse name is consumed by the zone apex itself (RFC 3596 §2.5),
//     so the PTR sits at "@", not a relative label.
var builtinRecords = []builtinZoneRecord{
	{Zone: "localhost", Name: "@", Type: "A", RData: "127.0.0.1"},
	{Zone: "localhost", Name: "@", Type: "AAAA", RData: "::1"},
	{Zone: "127.in-addr.arpa", Name: "1.0.0", Type: "PTR", RData: "localhost."},
	{Zone: "1.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.ip6.arpa", Name: "@", Type: "PTR", RData: "localhost."},
}

// upBuiltinZones is the goose Go migration body for version 7 — the only
// migration this package registers. It must run as RunTx, not RunDB, for
// exactly the reason upZonesData (version 5, see zonemigrate.go) does:
// sqlite runs with SetMaxOpenConns(1), so a RunDB migration asking the
// pool for a second connection while goose's own reserved connection is
// still checked out deadlocks every fresh install.
func upBuiltinZones(ctx context.Context, tx *sql.Tx, dialect string) error {
	return seedBuiltinZones(ctx, tx, dialect)
}

// seedBuiltinZones inserts each name in BuiltinZones that doesn't already
// exist, with a generated SOA and an apex NS record — the same shape
// migrateLocalRecords gives a migrated zone and handleZoneCreate
// (internal/api/zones_handlers.go) gives an API-created one, so a built-in
// zone is indistinguishable from either — and then seeds each zone's
// content via seedBuiltinZoneRecords (see that variable's and function's
// doc comments for which zones get content and which RFC each citation
// is). It talks to the database through the small dbtx interface
// (zonemigrate.go) rather than Store/ZoneStore for the same reason
// migrateLocalRecords does: db here may be the *sql.Tx a goose Go
// migration runs in, and sqlStore.db is a concrete *sql.DB that can't be
// pointed at it.
//
// The existence check is what makes the zone half of this idempotent in
// practice, not just in goose's bookkeeping: a user who already created
// "localhost" by hand before upgrading must not hit the unique index on
// zones.name, so an existing name is skipped rather than inserted blindly.
// This is deliberately the only migration involved — zones and their
// content are seeded in one pass, so there is nothing later to backfill.
func seedBuiltinZones(ctx context.Context, db dbtx, dialect string) error {
	for _, name := range BuiltinZones {
		var id int64
		err := db.QueryRowContext(ctx, rebind(dialect, `SELECT id FROM zones WHERE name = ?`), name).Scan(&id)
		if err == nil {
			continue // a zone of this name already exists — leave it alone.
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("check existing zone %q: %w", name, err)
		}

		now := time.Now().UnixMilli()
		soaNS := "ns." + name
		zoneID, err := migrateInsert(ctx, db, dialect,
			`INSERT INTO zones (name, type, enabled, soa_ns, soa_mbox, soa_serial, soa_refresh, soa_retry, soa_expire, soa_minimum, soa_ttl, primaries, tsig_key_id, expires_at, refreshed_at, created_at, modified_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			// type "internal", not "primary": a later task teaches the API to
			// refuse writes to it, which only makes sense for a zone type the
			// user didn't create through the API in the first place. The SOA
			// timer defaults (900/300/604800/900) and soa_ttl (900) match
			// handleZoneCreate's defaults exactly — see defaultSOATTL there.
			name, "internal", true, soaNS, "hostadmin."+name, 1, 900, 300, 604800, 900, 900, "", 0, 0, 0, now, now,
		)
		if err != nil {
			return fmt.Errorf("create built-in zone %q: %w", name, err)
		}

		// RFC 2181 §10.1: a zone's apex must have NS records, or the zone is
		// malformed from the moment it's created — mirrors upZonesData's and
		// handleZoneCreate's apex NS insert exactly (same "@" name, same
		// TTL, same fqdn'd soa_ns), so all three creation paths agree on
		// what a well-formed zone looks like.
		if _, err := migrateInsert(ctx, db, dialect,
			`INSERT INTO zone_records (zone_id, name, type, ttl, rdata, enabled, comment) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			zoneID, "@", "NS", apexNSTTL, dns.Fqdn(soaNS), true, "",
		); err != nil {
			return fmt.Errorf("create apex NS record for built-in zone %q: %w", name, err)
		}
	}
	return seedBuiltinZoneRecords(ctx, db, dialect)
}

// seedBuiltinZoneRecords inserts each builtinRecords entry that doesn't
// already exist at its (zone, name, type) — the record-level twin of
// seedBuiltinZones's zone-level existence check. Kept as its own function,
// and idempotent in its own right rather than relying on only ever being
// called once, so it can be exercised directly by
// TestSeedBuiltinZoneRecordsIdempotent without depending on goose's
// once-per-version bookkeeping to prove the property.
//
// The TTL is apexNSTTL, the same constant the apex NS record already in
// this file uses — not RFC 6303's own illustrative 10800, which the RFC
// itself calls arbitrary ("SOA timer values MAY be chosen arbitrarily").
// Matching the apex NS keeps every record this file writes at one TTL
// instead of two unrelated numbers.
//
// It only ever writes into a zone of type "internal". A zone named e.g.
// "localhost" that a user created by hand (type "primary") before
// upgrading already keeps its rows through seedBuiltinZones's own
// existence check (see TestBuiltinSeedSkipsPreexistingZone); this is that
// same rule applied to the content, so it never drops records into a zone
// it does not own.
func seedBuiltinZoneRecords(ctx context.Context, db dbtx, dialect string) error {
	for _, rec := range builtinRecords {
		var zoneID int64
		var zoneType string
		err := db.QueryRowContext(ctx, rebind(dialect, `SELECT id, type FROM zones WHERE name = ?`), rec.Zone).Scan(&zoneID, &zoneType)
		if errors.Is(err, sql.ErrNoRows) {
			// The zone itself hasn't been seeded (shouldn't happen when
			// called from seedBuiltinZones, but a missing zone means there
			// is nothing to add a record to either way).
			continue
		}
		if err != nil {
			return fmt.Errorf("find built-in zone %q: %w", rec.Zone, err)
		}
		if zoneType != "internal" {
			continue // a user's own zone by this name — not ours to touch.
		}

		var existingID int64
		err = db.QueryRowContext(ctx, rebind(dialect, `SELECT id FROM zone_records WHERE zone_id = ? AND name = ? AND type = ?`), zoneID, rec.Name, rec.Type).Scan(&existingID)
		if err == nil {
			continue // already present — leave it alone.
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("check existing record %s %s %s: %w", rec.Zone, rec.Name, rec.Type, err)
		}

		if _, err := migrateInsert(ctx, db, dialect,
			`INSERT INTO zone_records (zone_id, name, type, ttl, rdata, enabled, comment) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			zoneID, rec.Name, rec.Type, apexNSTTL, rec.RData, true, "",
		); err != nil {
			return fmt.Errorf("add built-in record %s %s %s: %w", rec.Zone, rec.Name, rec.Type, err)
		}
	}
	return nil
}
