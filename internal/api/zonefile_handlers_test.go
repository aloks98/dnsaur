package api

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/http"
	"strings"
	"testing"
)

func TestZoneFileExport(t *testing.T) {
	srv, zid := newTestServerWithZone(t, "e412.in")
	srv.createRecord(t, zid, `{"name":"bifrost","type":"A","ttl":300,"rdata":"57.129.69.158"}`)

	rec := srv.do(t, "GET", fmt.Sprintf("/api/v1/zones/%d/file", zid), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/dns") {
		t.Errorf("Content-Type = %q, want text/dns", ct)
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "e412.in.zone") {
		t.Errorf("Content-Disposition = %q, want a filename", cd)
	}
	if !strings.Contains(rec.Body.String(), "57.129.69.158") {
		t.Errorf("body missing the record:\n%s", rec.Body)
	}
}

// Built-ins are read-only, not unreadable — exporting one is fine.
func TestZoneFileExportAllowedForBuiltin(t *testing.T) {
	srv := newTestServer(t)
	id := srv.zoneIDByName(t, "localhost")
	if rec := srv.do(t, "GET", fmt.Sprintf("/api/v1/zones/%d/file", id), ""); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestZoneFileExportUnknownZone(t *testing.T) {
	srv := newTestServer(t)
	if rec := srv.do(t, "GET", "/api/v1/zones/999999/file", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

const importFile = `$ORIGIN e412.in.
$TTL 300
@ IN SOA ns.e412.in. hostadmin.e412.in. ( 5 900 300 604800 900 )
@ IN NS ns.e412.in.
bifrost 300 IN A 57.129.69.158
`

func importBody(t *testing.T, content string, dry bool) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{"content": content, "dry_run": dry})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// A dry run changes nothing — that is the entire point of showing the diff
// before a destructive replace.
func TestZoneFileImportDryRunChangesNothing(t *testing.T) {
	srv, zid := newTestServerWithZone(t, "e412.in")
	srv.createRecord(t, zid, `{"name":"doomed","type":"A","ttl":300,"rdata":"9.9.9.9"}`)
	before := srv.records(t, zid)

	rec := srv.do(t, "POST", fmt.Sprintf("/api/v1/zones/%d/file", zid), importBody(t, importFile, true))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body)
	}
	var diff struct{ Add, Change, Delete []map[string]any }
	if err := json.Unmarshal(rec.Body.Bytes(), &diff); err != nil {
		t.Fatal(err)
	}
	if len(diff.Add) == 0 || len(diff.Delete) == 0 {
		t.Errorf("diff = %+v; want the new record added and `doomed` deleted", diff)
	}
	if after := srv.records(t, zid); len(after) != len(before) {
		t.Fatalf("dry run mutated the zone: %d records became %d", len(before), len(after))
	}
}

// The file is the zone. Anything not in it goes.
func TestZoneFileImportReplaces(t *testing.T) {
	srv, zid := newTestServerWithZone(t, "e412.in")
	srv.createRecord(t, zid, `{"name":"doomed","type":"A","ttl":300,"rdata":"9.9.9.9"}`)

	if rec := srv.do(t, "POST", fmt.Sprintf("/api/v1/zones/%d/file", zid), importBody(t, importFile, false)); rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body)
	}
	for _, r := range srv.records(t, zid) {
		if r.Name == "doomed" {
			t.Fatal("a record absent from the file survived the import")
		}
	}
}

// One bad record rejects the file, and every bad line is named — not just
// the first, because fixing them one round-trip at a time is miserable.
func TestZoneFileImportRejectsWholeFileAndNamesEveryProblem(t *testing.T) {
	srv, zid := newTestServerWithZone(t, "e412.in")
	before := srv.records(t, zid)
	bad := `$ORIGIN e412.in.
$TTL 300
@ IN SOA ns.e412.in. hostadmin.e412.in. ( 5 900 300 604800 900 )
@ IN NS ns.e412.in.
web 300 IN A 1.1.1.1
web 600 IN A 2.2.2.2
alias 300 IN CNAME target.e412.in.
alias 300 IN A 3.3.3.3
`
	rec := srv.do(t, "POST", fmt.Sprintf("/api/v1/zones/%d/file", zid), importBody(t, bad, false))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d body = %s; want 422", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	// Both problems named: the RRSet TTL mismatch and the CNAME sibling.
	if !strings.Contains(body, "TTL") || !strings.Contains(body, "CNAME") {
		t.Errorf("errors = %s; want both problems named", body)
	}
	if after := srv.records(t, zid); len(after) != len(before) {
		t.Fatalf("a rejected import mutated the zone: %d became %d", len(before), len(after))
	}
}

// The only exception to §7's "A/AAAA writes maintain the PTR" rule.
func TestZoneFileImportDoesNotWritePTRs(t *testing.T) {
	srv, fwd := newTestServerWithZone(t, "e412.in")
	rev := srv.createZone(t, "150.168.192.in-addr.arpa")
	f := `$ORIGIN e412.in.
$TTL 300
@ IN SOA ns.e412.in. hostadmin.e412.in. ( 5 900 300 604800 900 )
@ IN NS ns.e412.in.
bifrost 300 IN A 192.168.150.10
`
	if rec := srv.do(t, "POST", fmt.Sprintf("/api/v1/zones/%d/file", fwd), importBody(t, f, false)); rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body)
	}
	if ptr := srv.recordsByType(t, rev, "PTR"); len(ptr) != 0 {
		t.Fatalf("import wrote PTRs into another zone: %+v", ptr)
	}
}

// A serial must never go backwards — a secondary that has seen the higher
// value would ignore the zone forever after (§4 D). The rule is
// max(file, current) + 1, and both halves are asserted: "greater than
// before" alone would hold for an implementation that simply took the
// file's serial verbatim.
//
// The loop count is load-bearing. newTestServerWithZone seeds the serial
// at 1 and each record write bumps it by one, so the zone has to be
// written to more than four times before its serial passes importFile's 5
// — and only then does taking the file's value verbatim show up as a
// decrease at all.
func TestZoneFileImportNeverLowersTheSerial(t *testing.T) {
	srv, zid := newTestServerWithZone(t, "e412.in")
	// Push the zone's serial well above the file's 5.
	for range 6 {
		srv.createRecord(t, zid, fmt.Sprintf(`{"name":"r%d","type":"A","ttl":300,"rdata":"1.2.3.4"}`, rand.IntN(1<<30)))
	}
	before := srv.zone(t, zid).SOASerial
	if before <= importFileSerial {
		t.Fatalf("zone serial is %d, not above the file's %d — the test cannot detect a decrease", before, importFileSerial)
	}

	if rec := srv.do(t, "POST", fmt.Sprintf("/api/v1/zones/%d/file", zid), importBody(t, importFile, false)); rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body)
	}
	after := srv.zone(t, zid).SOASerial
	if after <= before {
		t.Fatalf("serial went %d -> %d; must never decrease", before, after)
	}
	if want := max(importFileSerial, before) + 1; after != want {
		t.Fatalf("serial went %d -> %d; want max(file %d, current %d) + 1 = %d", before, after, importFileSerial, before, want)
	}
}

// The serial importFile's SOA carries.
const importFileSerial uint32 = 5

// The other half of max(file, current) + 1: when the file is the one
// ahead, the zone adopts its serial rather than merely stepping its own.
// A zone that ignored the file here would keep answering with a serial
// below the one the file's author has already published elsewhere.
func TestZoneFileImportAdoptsTheFilesSerialWhenItIsAhead(t *testing.T) {
	srv, zid := newTestServerWithZone(t, "e412.in")
	before := srv.zone(t, zid).SOASerial
	if before >= importFileSerial {
		t.Fatalf("zone serial is %d, not below the file's %d — the test cannot detect the file being ignored", before, importFileSerial)
	}

	if rec := srv.do(t, "POST", fmt.Sprintf("/api/v1/zones/%d/file", zid), importBody(t, importFile, false)); rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body)
	}
	if after, want := srv.zone(t, zid).SOASerial, importFileSerial+1; after != want {
		t.Fatalf("serial went %d -> %d; want the file's %d + 1 = %d", before, after, importFileSerial, want)
	}
}

// Built-in zones reject every write, import included.
func TestZoneFileImportRejectedForBuiltin(t *testing.T) {
	srv := newTestServer(t)
	id := srv.zoneIDByName(t, "localhost")
	if rec := srv.do(t, "POST", fmt.Sprintf("/api/v1/zones/%d/file", id), importBody(t, importFile, false)); rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
}

// A file that names no TTL anywhere is refused rather than stored as
// zeros. dns.ZoneParser returns Ttl 0 for a record that omits its TTL with
// no $TTL in scope (RFC 2308 §4) without reporting anything, and a zone
// whose SOA TTL is 0 answers every NXDOMAIN with a TTL of
// min(SOAMinimum, SOATTL) = 0 — nothing it denies would ever be negatively
// cached. Milestone A made soa_ttl non-client-settable so no hand write
// could produce that (defaultSOATTL); import is not the way around it.
func TestZoneFileImportRejectsAFileWithNoTTL(t *testing.T) {
	srv, zid := newTestServerWithZone(t, "e412.in")
	noTTL := `$ORIGIN e412.in.
@ IN SOA ns.e412.in. hostadmin.e412.in. ( 5 900 300 604800 900 )
@ IN NS ns.e412.in.
bifrost IN A 57.129.69.158
`
	rec := srv.do(t, "POST", fmt.Sprintf("/api/v1/zones/%d/file", zid), importBody(t, noTTL, false))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d body = %s; want 422", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "$TTL") {
		t.Errorf("errors = %s; want the message to name $TTL", rec.Body)
	}
	if z := srv.zone(t, zid); z.SOATTL == 0 {
		t.Fatalf("zone's SOA TTL became 0 anyway: %+v", z)
	}
}

// $GENERATE turns one line into any number of records, so no record it
// produced is on a line of its own and zones.Parse reports Line 0 for
// every record in such a file. An error message must not print "line 0" —
// it names the record by what it is instead, which is the only handle its
// author has on it.
func TestZoneFileImportNamesRecordsWithoutALineByValue(t *testing.T) {
	srv, zid := newTestServerWithZone(t, "e412.in")
	// host1 arrives twice with different TTLs: once from the $GENERATE at
	// 300, once explicitly at 600. RFC 2181 §5.2 rejects the RRSet.
	f := `$ORIGIN e412.in.
$TTL 300
@ IN SOA ns.e412.in. hostadmin.e412.in. ( 5 900 300 604800 900 )
@ IN NS ns.e412.in.
$GENERATE 1-2 host$ 300 IN A 10.0.0.$
host1 600 IN A 10.0.0.9
`
	rec := srv.do(t, "POST", fmt.Sprintf("/api/v1/zones/%d/file", zid), importBody(t, f, false))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d body = %s; want 422", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if strings.Contains(body, "line 0") {
		t.Errorf("errors = %s; a record with no attributable line must not be reported as \"line 0\"", body)
	}
	if !strings.Contains(body, "host1") || !strings.Contains(body, "10.0.0.9") {
		t.Errorf("errors = %s; want the offending record named by name/type/rdata", body)
	}
}

// Export then import is the round trip the whole milestone rests on: a
// zone handed back its own file must come out as no change at all. It is
// also the only test that catches the two sides disagreeing about how one
// RR is spelled — a record stored exactly as it was typed ("hello") and
// the same record read back from a master file ("\"hello\"") are one RR,
// and comparing the strings rather than the RRs would report a delete and
// an add of every such record on every import forever.
func TestZoneFileImportRoundTripsAnExportedZone(t *testing.T) {
	srv := newTestServer(t)
	zid := srv.createZone(t, "e412.in")
	srv.createRecord(t, zid, `{"name":"bifrost","type":"A","ttl":300,"rdata":"57.129.69.158"}`)
	srv.createRecord(t, zid, `{"name":"@","type":"MX","ttl":3600,"rdata":"10 mail.e412.in."}`)
	// Unquoted on the way in, quoted on the way back out of a master file.
	srv.createRecord(t, zid, `{"name":"@","type":"TXT","ttl":300,"rdata":"hello"}`)

	export := srv.do(t, "GET", fmt.Sprintf("/api/v1/zones/%d/file", zid), "")
	if export.Code != http.StatusOK {
		t.Fatalf("export status = %d body = %s", export.Code, export.Body)
	}

	rec := srv.do(t, "POST", fmt.Sprintf("/api/v1/zones/%d/file", zid), importBody(t, export.Body.String(), true))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body)
	}
	var diff struct {
		Add, Change, Delete []map[string]any
		Errors              []string
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &diff); err != nil {
		t.Fatal(err)
	}
	if len(diff.Add) != 0 || len(diff.Change) != 0 || len(diff.Delete) != 0 {
		t.Fatalf("re-importing a zone's own export is not a no-op: +%v ~%v -%v", diff.Add, diff.Change, diff.Delete)
	}
}

// A master file has no way to say "present but disabled", so export leaves
// a disabled record out (zones.Render) and import cannot see it. The file
// is the zone, so it goes — and the dry run says so before it happens
// rather than after.
func TestZoneFileImportDeletesDisabledRecordsAbsentFromTheFile(t *testing.T) {
	srv, zid := newTestServerWithZone(t, "e412.in")
	srv.createRecord(t, zid, `{"name":"parked","type":"A","ttl":300,"rdata":"9.9.9.9","enabled":false}`)

	rec := srv.do(t, "POST", fmt.Sprintf("/api/v1/zones/%d/file", zid), importBody(t, importFile, true))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body)
	}
	var diff struct{ Delete []map[string]any }
	if err := json.Unmarshal(rec.Body.Bytes(), &diff); err != nil {
		t.Fatal(err)
	}
	var named bool
	for _, d := range diff.Delete {
		if d["name"] == "parked" {
			named = true
		}
	}
	if !named {
		t.Fatalf("delete = %+v; want the disabled record named — it is not in the file, so it goes", diff.Delete)
	}
}

// zone_records has an index on (zone_id, name, type) but no uniqueness
// constraint (migration 0004), and POST /records will happily write the
// same record twice. When the file then carries one copy, exactly one row
// has to survive — and the diff has to name the *other* one as the delete.
// Naming the surviving row instead still leaves one record standing, so
// the zone looks right; what breaks is the row the change was going to be
// applied to being deleted first, which loses the change and fails the
// write.
func TestZoneFileImportMatchesDuplicateRowsOneForOne(t *testing.T) {
	srv, zid := newTestServerWithZone(t, "e412.in")
	first := srv.createRecord(t, zid, `{"name":"dup","type":"A","ttl":300,"rdata":"9.9.9.9"}`)
	second := srv.createRecord(t, zid, `{"name":"dup","type":"A","ttl":300,"rdata":"9.9.9.9"}`)

	// One copy of dup, at a different TTL: one row changes, the other goes.
	f := `$ORIGIN e412.in.
$TTL 300
@ IN SOA ns.e412.in. hostadmin.e412.in. ( 5 900 300 604800 900 )
dup 600 IN A 9.9.9.9
`
	rec := srv.do(t, "POST", fmt.Sprintf("/api/v1/zones/%d/file", zid), importBody(t, f, false))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body)
	}
	after := srv.records(t, zid)
	if len(after) != 1 {
		t.Fatalf("records = %+v; want exactly one dup row left", after)
	}
	if after[0].ID != first || after[0].TTL != 600 {
		t.Fatalf("surviving record = %+v; want row %d (the one the diff matched) carrying the file's TTL 600, with row %d deleted", after[0], first, second)
	}
}

// docs/api.md promises that every error this API returns is a flat
// {"error": "<message>"} envelope. A rejected import needs a list to name
// every offending line, so it carries both — a client with one error
// handler for all endpoints still finds what it expects.
func TestZoneFileImportRejectionCarriesTheFlatErrorEnvelope(t *testing.T) {
	srv, zid := newTestServerWithZone(t, "e412.in")
	rec := srv.do(t, "POST", fmt.Sprintf("/api/v1/zones/%d/file", zid), importBody(t, "not a zone file at all\n", false))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d body = %s; want 422", rec.Code, rec.Body)
	}
	var got struct {
		Error  string
		Errors []string
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Error == "" {
		t.Errorf("body = %s; want a flat \"error\" summary alongside the list", rec.Body)
	}
	if len(got.Errors) == 0 {
		t.Errorf("body = %s; want the per-problem list populated", rec.Body)
	}
}

// ...and a successful import must not carry one, or a client keying off
// the presence of "error" would read every success as a failure.
func TestZoneFileImportSuccessCarriesNoError(t *testing.T) {
	srv, zid := newTestServerWithZone(t, "e412.in")
	rec := srv.do(t, "POST", fmt.Sprintf("/api/v1/zones/%d/file", zid), importBody(t, importFile, false))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), `"error"`) {
		t.Fatalf("body = %s; a successful import must carry no error field", rec.Body)
	}
}

// importBodyWithoutDryRun is importBody with the dry_run field left out
// entirely — the shape a client sends when it forgot the field, or when a
// form serialized an unchecked box away.
func importBodyWithoutDryRun(t *testing.T, content string) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{"content": content})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Omitting dry_run must not commit. Spec §8 makes the two-step dry run the
// safety mechanism for a destructive whole-zone replace, so a field the
// client never sent must not be what selects the destructive branch —
// Go's bool zero value is the wrong default here, and openapi.yaml already
// declares the field required.
func TestZoneFileImportRequiresDryRun(t *testing.T) {
	srv, zid := newTestServerWithZone(t, "e412.in")
	srv.createRecord(t, zid, `{"name":"doomed","type":"A","ttl":300,"rdata":"9.9.9.9"}`)
	before := srv.records(t, zid)

	rec := srv.do(t, "POST", fmt.Sprintf("/api/v1/zones/%d/file", zid), importBodyWithoutDryRun(t, importFile))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body = %s; want 400", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "dry_run") {
		t.Errorf("error = %s; want it to name the missing field", rec.Body)
	}
	if after := srv.records(t, zid); len(after) != len(before) {
		t.Fatalf("a request with no dry_run mutated the zone: %d records became %d", len(before), len(after))
	}
	for _, r := range srv.records(t, zid) {
		if r.Name == "doomed" {
			return
		}
	}
	t.Fatal("a request with no dry_run deleted a record")
}

// The other direction: an explicit false is a commit and must stay one.
// Rejecting the missing field must not turn into rejecting the value.
func TestZoneFileImportExplicitFalseStillCommits(t *testing.T) {
	srv, zid := newTestServerWithZone(t, "e412.in")
	srv.createRecord(t, zid, `{"name":"doomed","type":"A","ttl":300,"rdata":"9.9.9.9"}`)

	if rec := srv.do(t, "POST", fmt.Sprintf("/api/v1/zones/%d/file", zid), importBody(t, importFile, false)); rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body)
	}
	after := srv.records(t, zid)
	for _, r := range after {
		if r.Name == "doomed" {
			t.Fatal("an explicit dry_run:false did not commit")
		}
	}
	if len(after) != 2 {
		t.Fatalf("records = %+v; want the file's two records", after)
	}
}

// importBodyOfExactly returns an import request body of exactly n bytes,
// carrying a valid two-record zone file.
//
// The padding is a trailing comment line rather than more records, for the
// same reason the cap exists: validation is quadratic in record count, and
// a megabyte of records would make a test about the size check cost five
// seconds of parsing it never reaches. Comments are dropped by the
// splitter, so a body at the cap still resolves to the same two records
// and the test runs in well under a second.
//
// The padding byte is 'x' inside that comment, which JSON encodes as
// itself — one content byte is one body byte, so the size lands exactly
// rather than approximately. That exactness is the point: the boundary is
// what needs pinning.
func importBodyOfExactly(t *testing.T, n int, dry bool) string {
	t.Helper()
	const base = importFile + "; "
	empty := importBody(t, base, dry)
	if len(empty) > n {
		t.Fatalf("cannot build a %d-byte body: the unpadded one is already %d", n, len(empty))
	}
	body := importBody(t, base+strings.Repeat("x", n-len(empty)), dry)
	if len(body) != n {
		t.Fatalf("built a %d-byte body, want exactly %d", len(body), n)
	}
	return body
}

// One byte over the cap must be refused, and refused as too large. The
// shared decode helper wraps the body in an io.LimitReader, which
// truncates instead of erroring, so a valid file one byte over reaches the
// JSON decoder as a document that stops mid-token and comes back as
// "invalid json" — sending the user hunting for a syntax error in a file
// that has none. This is the only endpoint whose body is a file the user
// picked, so it is the only one where that mistake is reachable in normal
// use.
//
// Exactly one byte over, not comfortably over: paired with
// TestZoneFileImportAcceptsABodyAtExactlyTheCap this pins which side of
// the comparison the cap falls on, which a body merely somewhere past the
// limit leaves free to drift.
func TestZoneFileImportRejectsAnOversizeBodyAsTooLarge(t *testing.T) {
	srv, zid := newTestServerWithZone(t, "e412.in")
	body := importBodyOfExactly(t, zoneFileMaxBytes+1, true)

	rec := srv.do(t, "POST", fmt.Sprintf("/api/v1/zones/%d/file", zid), body)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d body = %s; want 413", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "invalid json") {
		t.Errorf("error = %s; a valid file that is merely too big must not be called malformed", rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "too large") {
		t.Errorf("error = %s; want it to say the body is too large", rec.Body)
	}
}

// The other side of the boundary, one byte away: a body of exactly the cap
// is imported, not refused. The limit is inclusive, so this is the largest
// body the endpoint accepts — and the test that stops the check drifting
// from > to >= and rejecting it.
func TestZoneFileImportAcceptsABodyAtExactlyTheCap(t *testing.T) {
	srv, zid := newTestServerWithZone(t, "e412.in")
	body := importBodyOfExactly(t, zoneFileMaxBytes, false)

	if rec := srv.do(t, "POST", fmt.Sprintf("/api/v1/zones/%d/file", zid), body); rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s; want 200", rec.Code, rec.Body)
	}
	if got := len(srv.records(t, zid)); got != 2 {
		t.Fatalf("records = %d, want the file's 2", got)
	}
}
