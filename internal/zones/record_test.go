package zones_test

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/aloks98/dnsaur/internal/store"
	"github.com/aloks98/dnsaur/internal/zones"
)

// recordZone is the zone every BuildRecord case below is written into.
func recordZone() store.Zone {
	return store.Zone{
		ID: 1, Name: "e412.in", Type: "primary", Enabled: true,
		SOANS: "ns.e412.in", SOAMbox: "hostadmin.e412.in",
		SOASerial: 1, SOARefresh: 900, SOARetry: 300, SOAExpire: 604800,
		SOAMinimum: 900, SOATTL: 900,
	}
}

func buildA(t *testing.T, name string) (store.ZoneRecord, error) {
	t.Helper()
	return zones.BuildRecord(recordZone(), zones.RecordWrite{
		Name: name, Type: "A", TTL: 300, RData: "1.2.3.4",
	}, nil, 0, false)
}

// A record name is written into a master file on every export and read back
// out of one on every import, so a name that does not survive that trip is
// not a name this zone can hold. Nothing checked it: BuildRecord took
// whatever dns.NewRR tolerated, which is nearly anything — including an
// owner beginning ';', where the whole line lexes as a comment and NewRR
// returns (nil, nil) rather than an error.
func TestBuildRecordRefusesNamesAMasterFileCannotCarry(t *testing.T) {
	for _, tc := range []struct{ name, why string }{
		{";x", "the whole exported line would lex as a comment"},
		{"a;b", "the rest of the exported line would lex as a comment"},
		{"$ttl", "the exported line would lex as a $TTL directive"},
		{"$origin", "the exported line would lex as an $ORIGIN directive"},
		{`a"b`, "an unbalanced quote"},
		{"a(b", "an unbalanced parenthesis"},
		{"a b", "whitespace splits the exported line"},
		{"a\tb", "whitespace splits the exported line"},
		{"a\nb", "a newline splits the exported line"},
		{`a\.b`, "an escaped dot is not the label boundary the zone counts on"},
		{"a..b", "an empty label"},
		{"a/b", "a path separator is not a domain name"},
	} {
		if _, err := buildA(t, tc.name); err == nil {
			t.Errorf("BuildRecord accepted the name %q: %s", tc.name, tc.why)
		}
	}
}

// The guard must not narrow what a zone can already hold. Every name here is
// one this project's own zones, fixtures or tests use.
func TestBuildRecordKeepsAcceptingOrdinaryNames(t *testing.T) {
	for _, name := range []string{
		"@", "", "bifrost", "BiFrOsT", "a.b", "host.sub",
		"*", "*.nexus", "a.*",
		"_sip._tcp", "_dmarc", "_acme-challenge",
		"e412.in", "www.e412.in", "www.e412.in.",
		"ns.example.net", "xn--80ak6aa92e",
	} {
		if _, err := buildA(t, name); err != nil {
			t.Errorf("BuildRecord refused the name %q: %v", name, err)
		}
	}
}

// The zone's SOA lives on the zones row — its serial needs managed
// increments — so a zone_records row of type SOA is a second SOA that the
// apex answer ignores and Render writes out beside the real one. One
// accepted write and the export carries two apex SOAs, which is exactly what
// Parse rejects: the round trip spec §8 rests on is broken by a record
// nothing ever serves.
func TestBuildRecordRefusesSOARows(t *testing.T) {
	const rdata = "ns.e412.in. hostadmin.e412.in. 1 900 300 604800 900"
	for _, name := range []string{"@", "sub"} {
		_, err := zones.BuildRecord(recordZone(), zones.RecordWrite{
			Name: name, Type: "SOA", TTL: 900, RData: rdata,
		}, nil, 0, false)
		if err == nil {
			t.Errorf("BuildRecord accepted an SOA row at %q", name)
			continue
		}
		if !strings.Contains(err.Error(), "SOA") {
			t.Errorf("BuildRecord refused the SOA at %q without naming it: %v", name, err)
		}
	}
}

// An RFC 3597 unknown type is accepted by dns.NewRR and then broken on every
// path that reads it back: RDataOf stores the whole RR text (RFC3597.String
// prints CLASS1 where the header prints IN, so the prefix trim is a no-op),
// rrType cannot map TYPEnnn to a wire type so no query ever matches it, and
// Render exports the result. Refused at the door instead, with the type in
// the message.
func TestBuildRecordRefusesTypesTheServerCannotServe(t *testing.T) {
	for _, tc := range []struct{ recType, rdata string }{
		{"TYPE65280", `\# 4 01020304`},
		{"TYPE1", `\# 4 01020304`},
		{"", "1.2.3.4"},
		{"NOTATYPE", "1.2.3.4"},
	} {
		_, err := zones.BuildRecord(recordZone(), zones.RecordWrite{
			Name: "x", Type: tc.recType, TTL: 300, RData: tc.rdata,
		}, nil, 0, false)
		if err == nil {
			t.Errorf("BuildRecord accepted type %q, which no query can match", tc.recType)
			continue
		}
		if !strings.Contains(err.Error(), tc.recType) {
			t.Errorf("BuildRecord refused type %q without naming it: %v", tc.recType, err)
		}
	}
}

// A row whose rdata does not parse is skipped at query time and says
// nothing — the record simply stops existing, and inside a zone that is an
// authoritative NODATA rather than a lookup that goes elsewhere. Nothing
// written through BuildRecord can be such a row, but internal/store's
// local_records migration inserts rows directly, so they are possible. The
// snapshot build is where one row can be named once per reload instead of
// silently skipped on every query.
func TestNewZoneWarnsAboutARowItCannotServe(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	zones.NewZone(recordZone(), []store.ZoneRecord{
		{ID: 12, ZoneID: 1, Name: "bifrost", Type: "A", TTL: 300, RData: "not-an-ip", Enabled: true},
		{ID: 13, ZoneID: 1, Name: "nas", Type: "A", TTL: 300, RData: "10.0.0.9", Enabled: true},
	})

	got := buf.String()
	if !strings.Contains(got, "e412.in") || !strings.Contains(got, "record=12") {
		t.Errorf("the unservable row was not named with its zone and id: %q", got)
	}
	if strings.Contains(got, "record=13") {
		t.Errorf("a row that parses was reported too: %q", got)
	}
}

// "@" is the zone apex in a master file, and the import path resolves it
// that way (dns.ZoneParser reads the file under $ORIGIN <zone>.). The hand
// write read the same token under the root, so `MX 10 @` was stored as the
// null MX `10 .` (RFC 7505) — a record that means the opposite of what its
// author typed, and a value the two writers disagreed about.
//
// The substitution is miekg's own, not a text replacement: "@" is a name
// only where the type says it is, so a TXT keeps the one-character string it
// has always been on both paths.
func TestBuildRecordReadsBareAtAsTheApex(t *testing.T) {
	for _, tc := range []struct{ what, recType, raw, want string }{
		{"MX exchange", "MX", "10 @", "10 e412.in."},
		{"CNAME target", "CNAME", "@", "e412.in."},
		{"SRV target", "SRV", "10 20 5060 @", "10 20 5060 e412.in."},
		{"TXT is a string, not a name", "TXT", "@", `"@"`},
		{"an @ inside a quoted string is text", "TXT", `"hello @ there"`, `"hello @ there"`},
	} {
		t.Run(tc.what, func(t *testing.T) {
			name := "x"
			if tc.recType == "MX" {
				name = "@"
			}
			rec, err := zones.BuildRecord(recordZone(), zones.RecordWrite{
				Name: name, Type: tc.recType, TTL: 300, RData: tc.raw,
			}, nil, 0, false)
			if err != nil {
				t.Fatalf("BuildRecord(%s %q): %v", tc.recType, tc.raw, err)
			}
			if rec.RData != tc.want {
				t.Errorf("stored %q, want %q", rec.RData, tc.want)
			}
			// Whatever is stored is read back by the resolver through ToRR,
			// under no origin — so the stored spelling has to carry its own
			// meaning with no origin to lean on.
			if _, err := zones.ToRR(zones.RecordFQDN("e412.in", rec.Name), rec); err != nil {
				t.Errorf("stored rdata %q does not parse back: %v", rec.RData, err)
			}
		})
	}
}
