package store

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/miekg/dns"
)

// apexNSTTL is the TTL given to the apex NS record every migrated zone is
// seeded with (RFC 2181 §10.1). It duplicates apexNSTTL in
// internal/api/zones_handlers.go's handleZoneCreate rather than importing
// it: store must not depend on api, and the two are kept at the same value
// deliberately, so a migrated zone and an API-created zone look identical.
const apexNSTTL = 3600

// splitZone picks the apex and the relative name for a flat local_records
// name. The apex is the last two labels: a homelab's names are
// host.domain.tld, and inferring anything cleverer needs a public-suffix
// list — a dependency and a download for a one-time conversion.
func splitZone(name string) (apex, rel string) {
	labels := strings.Split(name, ".")
	if len(labels) <= 2 {
		return name, "@"
	}
	cut := len(labels) - 2
	return strings.Join(labels[cut:], "."), strings.Join(labels[:cut], ".")
}

// hasNonLeftmostWildcard reports whether rel contains an asterisk label
// anywhere but the very first position — "a.*" (from "a.*.e412.in"), not
// "*.nexus" (from "*.nexus.e412.in"). RFC 4592 §2.1.1 makes such an
// asterisk a literal label, not a wildcard: legal owner-name data, but
// almost certainly not what whoever wrote the local_records row meant.
func hasNonLeftmostWildcard(rel string) bool {
	labels := strings.Split(rel, ".")
	for _, l := range labels[1:] {
		if l == "*" {
			return true
		}
	}
	return false
}

// maxTTL is the largest TTL RFC 2181 §8 allows (2^31 - 1). A TTL with the
// high bit set is read by a receiving resolver as a negative number, which
// in practice means "never cache" — the opposite of whatever value was
// actually stored.
const maxTTL uint32 = 2147483647

func clampTTL(ttl uint32) uint32 {
	if ttl > maxTTL {
		return maxTTL
	}
	return ttl
}

// maxCharacterString is the largest a single DNS character-string can be
// (RFC 1035 §3.3: one length octet, so 255 bytes of payload). A TXT rdata
// longer than this is expressed as several character-strings that the
// consumer concatenates (§3.3.14) — that is how every DKIM key over 255
// bytes is published.
const maxCharacterString = 255

// quoteCharacterString renders v as one master-file character-string:
// double-quoted, with the two characters the quoting itself gives meaning
// to ('"' and '\') backslash-escaped, and everything outside printable
// ASCII written as the \DDD decimal escape.
//
// The escaping deliberately mirrors miekg/dns's own writeTXTStringByte
// (types.go), because it is miekg/dns that reads the result back: the
// escape set has to be exactly the one its parser and packer understand,
// or the bytes on the wire stop matching the bytes that were stored. The
// \DDD arm is not optional cosmetics — a raw newline inside a quoted token
// terminates the record for the zone-file lexer, and a value carrying one
// would fail to parse rather than round-trip.
func quoteCharacterString(v string) string {
	var b strings.Builder
	b.Grow(len(v) + 2)
	b.WriteByte('"')
	for i := 0; i < len(v); i++ {
		switch c := v[i]; {
		case c == '"' || c == '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		case c < ' ' || c > '~':
			fmt.Fprintf(&b, "\\%03d", c)
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// txtRData converts a local_records TXT value into the presentation-format
// rdata a zone_records row holds.
//
// These are two different languages and the migration is the seam between
// them. The pre-zones resolver built a TXT answer as dns.TXT{Txt:
// []string{value}} — the stored value taken literally, as one
// character-string. The zones path hands rdata to dns.NewRR, which reads it
// as master-file presentation format. Copying the value across unchanged
// therefore does not preserve it, it reinterprets it, and every way it can
// go wrong is silent:
//
//   - "v=DKIM1; k=rsa; p=MIGf..." — ';' opens a comment in presentation
//     format, so the record parses cleanly as just "v=DKIM1" and the key is
//     gone. Nothing errors, nothing logs.
//   - "v=spf1 a ~all" — unquoted whitespace separates character-strings, so
//     one string becomes three. On the wire that is a different TXT record.
//   - any unbalanced '"' — a parse error, and ToRR failures are skipped by
//     the answering layer (internal/zones/answer.go, fill), so the name
//     returns authoritative NODATA and is never forwarded. The record is
//     simply gone.
//
// Quoting is what makes the conversion lossless: the result parses back to
// character-strings whose concatenated wire bytes are byte-for-byte the
// original value, for every possible value including empty, UTF-8, control
// bytes, and embedded quotes and backslashes.
//
// A value longer than one character-string is split at 255-byte boundaries
// of the original bytes per RFC 1035 §3.3.14 (the consumer concatenates,
// so the meaning is unchanged) rather than left as a single oversized
// token. Two reasons: miekg/dns's parser would chunk an oversized quoted
// token itself, and storing the chunks explicitly means the rdata in the
// database and the dashboard is exactly what gets served instead of relying
// on that leniency; and the alternative of storing the value whole is not a
// "keep it as it was" option, because the pre-zones resolver could not
// serve an over-long TXT either — packing it failed with "string exceeded
// 255 bytes in txt", so the record never made it into an answer at all.
// Splitting can only fix such a row, never regress it. It is still logged
// at WARN: the served form does change, from unservable to several
// concatenated strings.
func txtRData(name, v string) string {
	if len(v) <= maxCharacterString {
		return quoteCharacterString(v)
	}
	slog.Warn("migrate: TXT value exceeds one 255-byte character-string; split per RFC 1035 §3.3.14 (it could not be served at all before)",
		"name", name, "bytes", len(v))
	var parts []string
	for len(v) > maxCharacterString {
		parts = append(parts, quoteCharacterString(v[:maxCharacterString]))
		v = v[maxCharacterString:]
	}
	return strings.Join(append(parts, quoteCharacterString(v)), " ")
}

// migrateRData is the value a zone_records row gets for a local_records
// row. A, AAAA and CNAME values are already valid presentation format for
// their type — an address or a domain name, neither of which the
// master-file syntax gives any special meaning to — so only TXT needs
// converting. The type is matched case-insensitively because the pre-zones
// resolver did the same (records.go's toRR upper-cased before dispatching),
// so a row stored as "txt" was served as TXT and has to be converted as
// one.
func migrateRData(r LocalRecord) string {
	if strings.EqualFold(r.Type, "TXT") {
		return txtRData(r.Name, r.Value)
	}
	return r.Value
}

// dbtx is the subset of *sql.DB and *sql.Tx that migrateLocalRecords needs.
// It lets the exact same conversion code run both in tests (against the
// *sql.DB of a fully-opened test Store) and in production (against the
// *sql.Tx goose's Go migration hands it) — see upZonesData's doc comment
// for why production must pass a *sql.Tx and not a second *sql.DB
// acquisition.
type dbtx interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// migrateLocalRecords reads every local_records row, groups it by splitZone
// apex, and creates one primary zone per apex — with a generated SOA,
// since without one the zone has no MINIMUM to hand out in an NXDOMAIN's
// AUTHORITY section, and a generated apex NS record pointing at that SOA's
// MNAME (RFC 2181 §10.1: a zone without apex NS is malformed, and
// handleZoneCreate already seeds one for every zone made through the API —
// a migrated zone must look the same or a zone-file export or transfer is
// where the gap would eventually surface) — then inserts each record under
// its relative name, with Value converted to presentation-format RData by
// migrateRData.
//
// local_records had no constraints a zone now enforces, so this also
// reconciles three shapes of data that were legal there and are not here:
//
//   - A CNAME beside any other type at the same name, or a CNAME at the
//     zone apex (RFC 1034 3.6.2, RFC 1912 §2.4 — the apex always has an
//     SOA, even though that SOA is a zones-row field and never shows up as
//     a local_records row to conflict against): the CNAME is dropped and
//     the rest kept, since losing an alias beats losing an address, logged
//     at WARN with the record's name.
//   - Multiple RRs of the same type at the same name disagreeing on TTL
//     (RFC 2181 §5.2, which requires one TTL per RRset): normalized to the
//     lowest TTL in the set — never dropped or rejected, since a migration
//     has to convert what it finds — logged at WARN for each record whose
//     TTL actually changes.
//   - A TTL above the RFC 2181 §8 ceiling: clamped to maxTTL rather than
//     left to be read as a negative number by a receiving resolver.
//
// A fourth conversion is the TXT one, and it is the only one that touches
// the record's value rather than its name, type or TTL: local_records held
// a TXT value as a literal string, zone_records holds presentation format,
// and migrateRData is what carries it across without changing what is
// served. See its doc comment for why copying the bytes unchanged is a
// silent data loss and not a no-op.
//
// A wildcard label that isn't leftmost ("a.*.e412.in") is legal DNS data
// (RFC 4592 §2.1.1 makes a non-leftmost "*" a literal label, not a
// wildcard) and is kept as-is, but is logged at WARN since it's very
// unlikely to be what was intended.
//
// It talks to the database through the small dbtx interface, not through
// Store/ZoneStore: sqlStore.db is a concrete *sql.DB, so it can't be
// pointed at the *sql.Tx a goose Go migration runs in, and raw SQL here is
// what lets the same function serve both callers — see upZonesData.
func migrateLocalRecords(ctx context.Context, db dbtx, dialect string) error {
	rows, err := db.QueryContext(ctx, `SELECT id, name, type, value, ttl FROM local_records ORDER BY id`)
	if err != nil {
		return fmt.Errorf("read local_records: %w", err)
	}
	var recs []LocalRecord
	for rows.Next() {
		var r LocalRecord
		if err := rows.Scan(&r.ID, &r.Name, &r.Type, &r.Value, &r.TTL); err != nil {
			rows.Close()
			return fmt.Errorf("scan local_records: %w", err)
		}
		recs = append(recs, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("read local_records: %w", err)
	}
	rows.Close()

	byApex := map[string][]LocalRecord{}
	var apexOrder []string
	for _, r := range recs {
		apex, _ := splitZone(r.Name)
		if _, ok := byApex[apex]; !ok {
			apexOrder = append(apexOrder, apex)
		}
		byApex[apex] = append(byApex[apex], r)
	}

	now := time.Now().UnixMilli()
	for _, apex := range apexOrder {
		soaNS := "ns." + apex
		zoneID, err := migrateInsert(ctx, db, dialect,
			`INSERT INTO zones (name, type, enabled, soa_ns, soa_mbox, soa_serial, soa_refresh, soa_retry, soa_expire, soa_minimum, primaries, tsig_key_id, expires_at, refreshed_at, created_at, modified_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			apex, "primary", true, soaNS, "hostadmin."+apex, 1, 900, 300, 604800, 900, "", 0, 0, 0, now, now,
		)
		if err != nil {
			return fmt.Errorf("create zone %q: %w", apex, err)
		}

		// RFC 2181 §10.1: a zone's apex must have NS records, or the zone is
		// malformed from the moment it's created — matches handleZoneCreate's
		// apex NS record exactly (same "@" name, same TTL, same fqdn'd
		// soa_ns), so an API-created zone and a migrated one are
		// indistinguishable. Unlike handleZoneCreate, a failure here is a
		// hard error, not a logged best-effort: the whole migration runs in
		// one transaction (RunTx — see upZonesData), so a zone the migration
		// can't finish seeding correctly should roll back rather than exist
		// half-built.
		if _, err := migrateInsert(ctx, db, dialect,
			`INSERT INTO zone_records (zone_id, name, type, ttl, rdata, enabled, comment) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			zoneID, "@", "NS", apexNSTTL, dns.Fqdn(soaNS), true, "",
		); err != nil {
			return fmt.Errorf("create apex NS record for zone %q: %w", apex, err)
		}

		byName := map[string][]LocalRecord{}
		var nameOrder []string
		for _, r := range byApex[apex] {
			_, rel := splitZone(r.Name)
			if _, ok := byName[rel]; !ok {
				nameOrder = append(nameOrder, rel)
			}
			byName[rel] = append(byName[rel], r)
		}

		for _, rel := range nameOrder {
			group := byName[rel]
			hasCNAME, hasOther := false, false
			for _, r := range group {
				if r.Type == "CNAME" {
					hasCNAME = true
				} else {
					hasOther = true
				}
			}
			// The zone's SOA (and, once set up, its NS) always occupies the
			// apex, even though neither is a local_records row — so a CNAME
			// there conflicts unconditionally, not only when some other
			// local_records row also names "@".
			siblingConflict := hasCNAME && hasOther
			apexCNAME := rel == "@" && hasCNAME

			if hasNonLeftmostWildcard(rel) {
				slog.Warn("migrate: asterisk is not the leftmost label; kept as a literal label, not a wildcard (RFC 4592 §2.1.1)", "name", group[0].Name)
			}

			// Drop illegal CNAMEs first, then reconcile TTLs within each
			// surviving (name, type) RRset — a dropped CNAME must not pull
			// down the TTL of the type it was illegally sharing a name with.
			var kept []LocalRecord
			for _, r := range group {
				if r.Type == "CNAME" {
					switch {
					case apexCNAME:
						slog.Warn("migrate: dropping CNAME at zone apex — SOA/NS already occupy it (RFC 1912 §2.4)", "name", r.Name)
						continue
					case siblingConflict:
						slog.Warn("migrate: dropping CNAME beside other record types at same name", "name", r.Name)
						continue
					}
				}
				kept = append(kept, r)
			}

			minTTLByType := map[string]uint32{}
			for _, r := range kept {
				ttl := clampTTL(r.TTL)
				if cur, ok := minTTLByType[r.Type]; !ok || ttl < cur {
					minTTLByType[r.Type] = ttl
				}
			}

			for _, r := range kept {
				ttl := clampTTL(r.TTL)
				if want := minTTLByType[r.Type]; want != ttl {
					slog.Warn("migrate: normalizing RRset TTL to the lowest value in the set (RFC 2181 §5.2)", "name", r.Name, "type", r.Type, "from", ttl, "to", want)
					ttl = want
				}
				if _, err := migrateInsert(ctx, db, dialect,
					`INSERT INTO zone_records (zone_id, name, type, ttl, rdata, enabled, comment) VALUES (?, ?, ?, ?, ?, ?, ?)`,
					zoneID, rel, r.Type, ttl, migrateRData(r), true, "",
				); err != nil {
					return fmt.Errorf("add record %q: %w", r.Name, err)
				}
			}
		}
	}
	return nil
}

// migrateInsert runs an INSERT against db and returns the new row id,
// mirroring sqlStore.insert (sql.go) — duplicated in miniature rather than
// shared, because db here may be a *sql.Tx and sqlStore.db is a concrete
// *sql.DB, so sqlStore's own insert helper can't be reused directly.
func migrateInsert(ctx context.Context, db dbtx, dialect, q string, args ...any) (int64, error) {
	if dialect == "postgres" {
		var id int64
		err := db.QueryRowContext(ctx, rebind(dialect, q+" RETURNING id"), args...).Scan(&id)
		return id, err
	}
	res, err := db.ExecContext(ctx, rebind(dialect, q), args...)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// upZonesData is the goose Go migration body for version 5. It must run as
// RunTx, not RunDB: goose reserves a single *sql.Conn for the whole Up()
// call (provider_run.go's initialize) and, for a RunTx migration, derives
// tx from that same reserved connection — so migrateLocalRecords never asks
// the pool for a connection of its own. Passing RunDB instead hands the
// migration the shared *sql.DB, and calling QueryContext/ExecContext on it
// makes a second, independent pool acquisition; sqlite here runs with
// SetMaxOpenConns(1) (store.go, Open), so that second acquisition can never
// succeed while goose's reserved connection is still checked out, and every
// fresh-install migration deadlocks forever. This was not a hunch: an
// earlier RunDB version of this migration was verified to hang exactly
// there (database/sql.(*DB).conn, waiting on the pool) — goose's own
// provider_run.go documents this exact deadlock class in a comment on
// runIndividually.
func upZonesData(ctx context.Context, tx *sql.Tx, dialect string) error {
	return migrateLocalRecords(ctx, tx, dialect)
}
