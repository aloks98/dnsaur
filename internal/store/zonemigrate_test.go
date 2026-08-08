package store

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"github.com/miekg/dns"
)

// captureLog swaps in a buffer-backed slog handler for the duration of fn
// and returns everything logged through the package-level slog.* calls
// (what migrateLocalRecords uses) while it ran. Tests use this where the
// only observable effect of the behavior under test is a WARN line — e.g.
// keeping a non-leftmost wildcard as a literal label is not new behavior on
// its own (nothing ever dropped it); only the warning is.
func captureLog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(prev)
	fn()
	return buf.String()
}

func TestSplitZone(t *testing.T) {
	for _, tc := range []struct{ in, apex, rel string }{
		{"bifrost.e412.in", "e412.in", "bifrost"},
		{"*.nexus.e412.in", "e412.in", "*.nexus"},
		{"nas.home.lan", "home.lan", "nas"},
		{"e412.in", "e412.in", "@"},
		{"localhost", "localhost", "@"},
	} {
		apex, rel := splitZone(tc.in)
		if apex != tc.apex || rel != tc.rel {
			t.Errorf("splitZone(%q) = %q,%q; want %q,%q", tc.in, apex, rel, tc.apex, tc.rel)
		}
	}
}

// TestClampTTL pins the RFC 2181 §8 ceiling: a TTL with the high bit set
// (anything above 2^31-1) is read by a receiving resolver as a negative
// number, which in practice means "never cache" — not what an unclamped
// value stored in local_records ever meant. Not reachable through the app
// today (the old API already capped TTL at 86400), so this is a direct
// unit test of the helper rather than a full migration test with its own
// revert-verify, unlike findings 1-3.
func TestClampTTL(t *testing.T) {
	for _, tc := range []struct {
		in, want uint32
	}{
		{300, 300},
		{maxTTL, maxTTL},
		{maxTTL + 1, maxTTL},
		{4294967295, maxTTL}, // uint32 max
	} {
		if got := clampTTL(tc.in); got != tc.want {
			t.Errorf("clampTTL(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// seedLocalRecords writes through the pre-zone RecordStore — the API
// local_records had before zones existed — rather than through ZoneStore,
// since it's migrateLocalRecords itself that is under test here.
func seedLocalRecords(t *testing.T, s Store, recs []LocalRecord) {
	t.Helper()
	ctx := context.Background()
	for _, r := range recs {
		if _, err := s.Records().Add(ctx, r); err != nil {
			t.Fatalf("seedLocalRecords: %v", err)
		}
	}
}

// resetZoneMigrationTables empties zones, zone_records and local_records so
// the exact-count and literal-name assertions below hold against the
// postgres container shared across the whole package's test run — the same
// defensive pattern TestLocalRecords (store_test.go) and cleanupCRUDTables
// (crud_test.go) already use for this and other tables. sqlite already gets
// isolation for free from forEachDriver's per-case tmpdir. Returns the
// underlying *sqlStore so callers can drive migrateLocalRecords directly
// against its *sql.DB, the same way upZonesData drives it against a
// *sql.Tx.
func resetZoneMigrationTables(t *testing.T, s Store) *sqlStore {
	t.Helper()
	ss := s.(*sqlStore)
	ctx := context.Background()
	for _, table := range []string{"zone_records", "zones", "local_records"} {
		if _, err := ss.db.ExecContext(ctx, ss.q(`DELETE FROM `+table)); err != nil {
			t.Fatalf("cleanup %s: %v", table, err)
		}
	}
	return ss
}

// withoutApexNS strips the automatic "@ NS" record every migrated zone now
// gets (RFC 2181 §10.1, see TestMigrateCreatesApexNSRecord) out of recs, so
// tests written before that record existed can keep asserting on exactly
// the local_records-derived content without also re-encoding the apex NS
// record's shape in every one of them.
func withoutApexNS(recs []ZoneRecord) []ZoneRecord {
	out := make([]ZoneRecord, 0, len(recs))
	for _, r := range recs {
		if r.Name == "@" && r.Type == "NS" {
			continue
		}
		out = append(out, r)
	}
	return out
}

func TestMigrateLocalRecordsToZones(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		ss := resetZoneMigrationTables(t, s)
		// Seed the pre-zone table directly: the migration is what we're
		// testing, so it must not depend on the store API that replaces it.
		seedLocalRecords(t, s, []LocalRecord{
			{Name: "bifrost.e412.in", Type: "A", Value: "57.129.69.158", TTL: 3600},
			{Name: "*.nexus.e412.in", Type: "A", Value: "192.168.160.200", TTL: 3600},
			{Name: "nas.home.lan", Type: "A", Value: "192.168.1.5", TTL: 300},
		})
		if err := migrateLocalRecords(ctx, ss.db, ss.dialect); err != nil {
			t.Fatalf("migrateLocalRecords: %v", err)
		}
		zones, _ := s.Zones().Zones(ctx)
		if len(zones) != 2 {
			t.Fatalf("got %d zones, want 2 (e412.in, home.lan): %+v", len(zones), zones)
		}
		byName := map[string]Zone{}
		for _, z := range zones {
			byName[z.Name] = z
		}
		e412, ok := byName["e412.in"]
		if !ok {
			t.Fatalf("no e412.in zone in %+v", zones)
		}
		// A generated SOA is what makes the zone answerable at all: without it
		// there is no MINIMUM to put in an NXDOMAIN's AUTHORITY section.
		if e412.SOAMbox != "hostadmin.e412.in" || e412.SOASerial != 1 {
			t.Errorf("SOA = %q/%d, want hostadmin.e412.in/1", e412.SOAMbox, e412.SOASerial)
		}
		recs, _ := s.Zones().Records(ctx, e412.ID)
		got := map[string]string{}
		for _, r := range withoutApexNS(recs) {
			got[r.Name] = r.RData
		}
		want := map[string]string{"bifrost": "57.129.69.158", "*.nexus": "192.168.160.200"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("records = %v, want %v", got, want)
		}
	})
}

// A CNAME beside another type at the same name was legal in local_records
// and is illegal in a zone (RFC 1034 3.6.2). The migration must not
// silently produce a zone that violates the rule the API will enforce.
func TestMigrateDropsConflictingCNAME(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		ss := resetZoneMigrationTables(t, s)
		seedLocalRecords(t, s, []LocalRecord{
			{Name: "www.e412.in", Type: "A", Value: "192.168.1.9", TTL: 300},
			{Name: "www.e412.in", Type: "CNAME", Value: "bifrost.e412.in", TTL: 300},
		})
		if err := migrateLocalRecords(ctx, ss.db, ss.dialect); err != nil {
			t.Fatalf("migrateLocalRecords: %v", err)
		}
		zones, _ := s.Zones().Zones(ctx)
		rawRecs, _ := s.Zones().Records(ctx, zones[0].ID)
		recs := withoutApexNS(rawRecs)
		if len(recs) != 1 || recs[0].Type != "A" {
			t.Fatalf("records = %+v; want only the A (CNAME dropped)", recs)
		}
	})
}

// A CNAME at the zone apex is illegal (RFC 1912 §2.4): SOA and NS always
// occupy "@", even though neither is a local_records row — which is
// exactly why the sibling-conflict check above never sees it. A lone
// CNAME at "e412.in" has no other local_records row to conflict against,
// so it must be dropped on its own; only the automatic apex NS record
// (RFC 2181 §10.1) should remain.
func TestMigrateDropsApexCNAME(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		ss := resetZoneMigrationTables(t, s)
		seedLocalRecords(t, s, []LocalRecord{
			{Name: "e412.in", Type: "CNAME", Value: "bifrost.e412.in", TTL: 300},
		})
		if err := migrateLocalRecords(ctx, ss.db, ss.dialect); err != nil {
			t.Fatalf("migrateLocalRecords: %v", err)
		}
		zones, _ := s.Zones().Zones(ctx)
		if len(zones) != 1 {
			t.Fatalf("got %d zones, want 1: %+v", len(zones), zones)
		}
		rawRecs, _ := s.Zones().Records(ctx, zones[0].ID)
		recs := withoutApexNS(rawRecs)
		if len(recs) != 0 {
			t.Fatalf("records = %+v; want none besides the automatic apex NS (apex CNAME dropped)", recs)
		}
	})
}

// RFC 2181 §5.2 requires every RR in an RRset to carry one TTL.
// local_records had no such constraint, so two A rows at the same name
// could disagree; the migration must reconcile them to the lowest TTL in
// the set rather than drop or reject either — a migration has to convert
// what it finds.
func TestMigrateNormalizesRRSetTTLToLowest(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		ss := resetZoneMigrationTables(t, s)
		seedLocalRecords(t, s, []LocalRecord{
			{Name: "www.e412.in", Type: "A", Value: "192.168.1.1", TTL: 300},
			{Name: "www.e412.in", Type: "A", Value: "192.168.1.2", TTL: 600},
		})
		if err := migrateLocalRecords(ctx, ss.db, ss.dialect); err != nil {
			t.Fatalf("migrateLocalRecords: %v", err)
		}
		zones, _ := s.Zones().Zones(ctx)
		rawRecs, _ := s.Zones().Records(ctx, zones[0].ID)
		recs := withoutApexNS(rawRecs)
		if len(recs) != 2 {
			t.Fatalf("records = %+v; want 2 (both kept)", recs)
		}
		for _, r := range recs {
			if r.TTL != 300 {
				t.Errorf("record %+v TTL = %d, want 300 (lowest in the set)", r, r.TTL)
			}
		}
	})
}

// RFC 4592 §2.1.1: an asterisk that is not the leftmost label is a literal
// label, not a wildcard, so "a.*.e412.in" is legal owner-name data and must
// survive the migration intact — splitZone puts it at relative name "a.*"
// under zone e412.in, not under some invented wildcard. Keeping it isn't
// new behavior by itself (nothing ever dropped it); what the migration must
// add is making it visible in the log, so this asserts on the WARN too —
// otherwise reverting the warning would leave this test passing.
func TestMigrateKeepsNonLeftmostWildcardAsLiteral(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		ss := resetZoneMigrationTables(t, s)
		seedLocalRecords(t, s, []LocalRecord{
			{Name: "a.*.e412.in", Type: "A", Value: "192.168.1.1", TTL: 300},
		})
		logOutput := captureLog(t, func() {
			if err := migrateLocalRecords(ctx, ss.db, ss.dialect); err != nil {
				t.Fatalf("migrateLocalRecords: %v", err)
			}
		})
		zones, _ := s.Zones().Zones(ctx)
		rawRecs, _ := s.Zones().Records(ctx, zones[0].ID)
		recs := withoutApexNS(rawRecs)
		if len(recs) != 1 || recs[0].Name != "a.*" {
			t.Fatalf("records = %+v; want one record named %q (kept as a literal label)", recs, "a.*")
		}
		if !strings.Contains(logOutput, "not the leftmost label") || !strings.Contains(logOutput, "a.*.e412.in") {
			t.Errorf("log = %q; want a WARN naming a.*.e412.in as a non-leftmost asterisk", logOutput)
		}
	})
}

// RFC 2181 §10.1: a zone's apex must have NS records, or the zone is
// malformed from the moment it exists. handleZoneCreate
// (internal/api/zones_handlers.go) already seeds one for every zone made
// through the API; every zone the migration creates must get the same —
// same name ("@"), same type, same TTL, same fqdn'd target — or a zone
// inherited from local_records would be the one place a malformed zone
// could still be produced.
func TestMigrateCreatesApexNSRecord(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		ss := resetZoneMigrationTables(t, s)
		seedLocalRecords(t, s, []LocalRecord{
			{Name: "bifrost.e412.in", Type: "A", Value: "57.129.69.158", TTL: 3600},
			{Name: "nas.home.lan", Type: "A", Value: "192.168.1.5", TTL: 300},
		})
		if err := migrateLocalRecords(ctx, ss.db, ss.dialect); err != nil {
			t.Fatalf("migrateLocalRecords: %v", err)
		}
		zones, _ := s.Zones().Zones(ctx)
		if len(zones) != 2 {
			t.Fatalf("got %d zones, want 2: %+v", len(zones), zones)
		}
		for _, z := range zones {
			recs, _ := s.Zones().Records(ctx, z.ID)
			var ns *ZoneRecord
			for i := range recs {
				if recs[i].Name == "@" && recs[i].Type == "NS" {
					ns = &recs[i]
					break
				}
			}
			if ns == nil {
				t.Fatalf("zone %q has no apex NS record: %+v", z.Name, recs)
			}
			if want := "ns." + z.Name + "."; ns.RData != want {
				t.Errorf("zone %q apex NS RData = %q, want %q", z.Name, ns.RData, want)
			}
			if ns.TTL != apexNSTTL {
				t.Errorf("zone %q apex NS TTL = %d, want %d", z.Name, ns.TTL, apexNSTTL)
			}
		}
	})
}

// txtWireStrings returns the character-strings a stored TXT rdata actually
// puts on the wire.
//
// It goes all the way to the packed bytes rather than reading dns.TXT.Txt,
// because Txt is not the answer: miekg/dns keeps presentation escapes in
// that field and only decodes them in packTxtString, so a Txt entry of
// `he said \"hi\"` and one of `he said "hi"` look different there and are
// identical on the wire. The wire is what a client receives and the only
// level at which "the record survived the migration" means anything.
func txtWireStrings(t *testing.T, rdata string) []string {
	t.Helper()
	rr, err := dns.NewRR(fmt.Sprintf("x.example. 300 IN TXT %s", rdata))
	if err != nil {
		t.Fatalf("rdata %q does not parse — the record would be unservable and the name would answer NODATA: %v", rdata, err)
	}
	buf := make([]byte, 65535)
	end, err := dns.PackRR(rr, buf, 0, nil, false)
	if err != nil {
		t.Fatalf("rdata %q does not pack: %v", rdata, err)
	}
	_, off, err := dns.UnpackDomainName(buf[:end], 0)
	if err != nil {
		t.Fatalf("unpack owner name: %v", err)
	}
	off += 2 + 2 + 4 // type, class, ttl
	rdlen := int(buf[off])<<8 | int(buf[off+1])
	off += 2
	var out []string
	for stop := off + rdlen; off < stop; {
		n := int(buf[off])
		off++
		out = append(out, string(buf[off:off+n]))
		off += n
	}
	return out
}

// TXT is the one type whose local_records value is not already valid
// presentation format, and the migration is where the two languages meet.
// The pre-zones resolver held the stored value as a single literal
// character-string (dns.TXT{Txt: []string{value}}); zone_records rdata is
// parsed as a master-file line. Handing the stored bytes across unchanged
// therefore reinterprets them, and every failure mode is silent — which is
// why each case below asserts on the wire form, not on the stored string.
//
// The three cases are the three ways real data breaks:
//
//   - ';' opens a comment in presentation format. A DKIM key parses cleanly
//     as just "v=DKIM1" and the public key is gone, with no error anywhere.
//     This is the case that matters most: DKIM and SPF records are the
//     overwhelming majority of TXT rows anyone actually stores.
//   - unquoted whitespace separates character-strings, so one string
//     silently becomes several. A different record on the wire, and
//     verifiers that do not concatenate see a different value.
//   - an embedded '"' makes the line unparseable, and internal/zones's fill
//     skips a row whose rdata will not parse. Inside a zone that is not a
//     fall-through to upstream: the name answers authoritative NODATA, so
//     the record does not degrade, it disappears.
func TestMigrateTXTValuesSurviveIntact(t *testing.T) {
	const dkim = "v=DKIM1; k=rsa; p=MIGfMA0GCSqGSIb3DQEBAQUAA4GNADCBiQKBgQDd7Fn"
	cases := []struct {
		rel   string
		value string
		want  []string
	}{
		// Semicolon: without quoting this parses to ["v=DKIM1"].
		{"dkim._domainkey", dkim, []string{dkim}},
		// Spaces: without quoting this parses to ["v=spf1","a","~all"].
		{"spf", "v=spf1 a ~all", []string{"v=spf1 a ~all"}},
		// Embedded quote: without quoting this does not parse at all.
		{"verify", `token="abc123"`, []string{`token="abc123"`}},
	}

	forEachDriver(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		ss := resetZoneMigrationTables(t, s)
		seed := make([]LocalRecord, 0, len(cases))
		for _, tc := range cases {
			seed = append(seed, LocalRecord{Name: tc.rel + ".e412.in", Type: "TXT", Value: tc.value, TTL: 300})
		}
		seedLocalRecords(t, s, seed)
		if err := migrateLocalRecords(ctx, ss.db, ss.dialect); err != nil {
			t.Fatalf("migrateLocalRecords: %v", err)
		}
		zones, _ := s.Zones().Zones(ctx)
		if len(zones) != 1 {
			t.Fatalf("got %d zones, want 1: %+v", len(zones), zones)
		}
		recs, _ := s.Zones().Records(ctx, zones[0].ID)
		byName := map[string]ZoneRecord{}
		for _, r := range withoutApexNS(recs) {
			byName[r.Name] = r
		}
		for _, tc := range cases {
			r, ok := byName[tc.rel]
			if !ok {
				t.Errorf("no record %q survived the migration: %+v", tc.rel, recs)
				continue
			}
			got := txtWireStrings(t, r.RData)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("%s: wire character-strings = %q, want %q (stored rdata %q)", tc.rel, got, tc.want, r.RData)
			}
		}
	})
}

// A TXT value longer than one character-string (255 bytes, RFC 1035 §3.3) —
// a 2048-bit DKIM key is the everyday example — is split at 255-byte
// boundaries per §3.3.14, which the consumer concatenates back. This is the
// one TXT conversion that changes what is served, and it can only improve
// it: the pre-zones resolver put the whole value in a single
// character-string, and packing that failed outright ("string exceeded 255
// bytes in txt"), so the record was never answerable in the first place.
// The WARN is asserted alongside the value because the change in served
// form is the whole reason it is logged.
func TestMigrateTXTSplitsOverlongValue(t *testing.T) {
	value := "v=DKIM1; k=rsa; p=" + strings.Repeat("A", 400)
	forEachDriver(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		ss := resetZoneMigrationTables(t, s)
		seedLocalRecords(t, s, []LocalRecord{
			{Name: "big._domainkey.e412.in", Type: "TXT", Value: value, TTL: 300},
		})
		logOutput := captureLog(t, func() {
			if err := migrateLocalRecords(ctx, ss.db, ss.dialect); err != nil {
				t.Fatalf("migrateLocalRecords: %v", err)
			}
		})
		zones, _ := s.Zones().Zones(ctx)
		recs, _ := s.Zones().Records(ctx, zones[0].ID)
		kept := withoutApexNS(recs)
		if len(kept) != 1 {
			t.Fatalf("records = %+v, want 1", kept)
		}
		got := txtWireStrings(t, kept[0].RData)
		if len(got) != 2 {
			t.Errorf("got %d character-strings, want 2 (255 + remainder)", len(got))
		}
		for i, s := range got {
			if len(s) > maxCharacterString {
				t.Errorf("character-string %d is %d bytes, over the %d-byte limit", i, len(s), maxCharacterString)
			}
		}
		if joined := strings.Join(got, ""); joined != value {
			t.Errorf("concatenated value = %q, want %q", joined, value)
		}
		if !strings.Contains(logOutput, "255-byte character-string") || !strings.Contains(logOutput, "big._domainkey.e412.in") {
			t.Errorf("log = %q; want a WARN naming big._domainkey.e412.in as split", logOutput)
		}
	})
}

// quoteCharacterString has to be lossless for every byte a value can hold,
// not only for the shapes the three realistic cases above cover: the old
// API accepted any non-empty string, so control bytes, UTF-8, backslashes
// and lone quotes all exist in the wild. This walks every byte 0-255 plus
// the awkward literals directly, since seeding one local_records row per
// case through two database drivers would be a far slower way to test the
// same function.
func TestQuoteCharacterStringRoundTrips(t *testing.T) {
	allBytes := make([]byte, 256)
	for i := range allBytes {
		allBytes[i] = byte(i)
	}
	for _, v := range []string{
		"",
		"v=spf1 a ~all",
		"v=DKIM1; k=rsa; p=abc",
		`he said "hi"`,
		`c:\path\to`,
		`trailing\`,
		`"`,
		"tab\there",
		"line\nbreak",
		"héllo wörld",
		strings.Repeat("a", maxCharacterString),
		string(allBytes[:maxCharacterString]),
	} {
		got := txtWireStrings(t, quoteCharacterString(v))
		if len(got) != 1 || got[0] != v {
			t.Errorf("quoteCharacterString(%q) -> %q, want exactly one string equal to the input", v, got)
		}
	}
}
