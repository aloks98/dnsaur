# Zones Milestone C Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Export a zone as a BIND master file, and import one back — with a dry run that shows exactly what a replace would destroy before it happens.

**Architecture:** Milestone A stores `rdata` in DNS presentation format, so the stored form already *is* master-file format: export is string assembly and import is `dns.ZoneParser`. Import is replace-semantics in two phases — a dry run returning a diff, then a commit — and every record it produces passes through `buildZoneRecord`, the same validator behind `POST /records`, so a bulk write cannot create data the UI would refuse to edit.

**Tech Stack:** Go 1.26; `github.com/miekg/dns` (`NewZoneParser` for import, `dns.RR` presentation strings for export); existing `internal/store` and `internal/api`; React 19 + TanStack Query v5 + rnui.

## Global Constraints

- Module `github.com/aloks98/dnsaur`; Go 1.26. Branch: `feat/zone-files`.
- Before every commit: `go test -race ./...`, `~/go/bin/golangci-lint run ./...` (0 issues), AND `gofmt -l internal cmd` empty — golangci-lint does NOT check gofmt.
- For web tasks, from `web/`: `pnpm lint && pnpm format:check && pnpm typecheck && pnpm test && pnpm build && pnpm test:e2e`. **`format:check` is a CI gate and has broken CI twice.**
- **Verify the committed tree, not the working tree.** Run the formatters against what you staged; a fix formatted locally and never staged failed CI on an earlier milestone.
- Conventional commits. Revert-verify discipline: every new test shown to fail with its fix removed, and the failure output reported.
- **Error strings state the problem and stop — no RFC citations in user-facing messages.** Citations go in a comment above the check.
- **New routes register through `s.route(...)`, never `s.mux.HandleFunc`.** The mux is built lazily in `Handler()`, so a direct registration panics on a nil mux. `TestOpenAPIServedAndCoversRoutes` asserts a two-way path+method match against the spec, so every new endpoint must be documented in `internal/api/openapi.yaml` in the same commit or the suite fails.
- `DisallowUnknownFields` is on for JSON decode. UI copy plain and brief.
- Do not weaken an existing assertion to make something pass. Several were caught doing that on earlier milestones; if a change breaks a test, adapt it so it proves what it proved, or report it.

## File Structure

```
internal/zones/zonefile.go            — Task 1 (render a zone to master-file text)
internal/zones/zonefile_parse.go      — Task 2 (parse master-file text to records)
internal/api/zonefile_handlers.go     — Tasks 3-4 (export endpoint, import dry-run + commit)
internal/api/openapi.yaml             — Tasks 3-4 (required: the route test enforces it)
web/src/hooks/use-zones.ts            — Task 5 (export download, import mutations)
web/src/pages/zones/detail.tsx        — Task 5 (Export / Import actions + the diff)
docs/dashboard.md, docs/api.md        — Task 6
```

---

### Task 1: Render a zone as a master file

**Files:**
- Create: `internal/zones/zonefile.go`
- Test: `internal/zones/zonefile_test.go`

**Interfaces:**
- Produces: `func Render(z store.Zone, recs []store.ZoneRecord) string`

**Why this is small:** `rdata` is already presentation format. Rendering is assembling `$ORIGIN`, `$TTL`, an SOA line from the `zones` row, and one line per record.

- [ ] **Step 1: Write the failing test**

```go
package zones_test

import (
	"strings"
	"testing"

	"github.com/aloks98/dnsaur/internal/store"
	"github.com/aloks98/dnsaur/internal/zones"
	"github.com/miekg/dns"
)

func testZone() store.Zone {
	return store.Zone{
		ID: 1, Name: "e412.in", Type: "primary", Enabled: true,
		SOANS: "ns.e412.in", SOAMbox: "hostadmin.e412.in",
		SOASerial: 7, SOARefresh: 900, SOARetry: 300,
		SOAExpire: 604800, SOAMinimum: 900, SOATTL: 900,
	}
}

// The rendered file must parse back with miekg — that is the only bar that
// matters, and it is stronger than comparing against a golden string, which
// would pin whitespace rather than correctness.
func TestRenderRoundTripsThroughTheParser(t *testing.T) {
	recs := []store.ZoneRecord{
		{Name: "@", Type: "NS", TTL: 3600, RData: "ns.e412.in.", Enabled: true},
		{Name: "bifrost", Type: "A", TTL: 300, RData: "57.129.69.158", Enabled: true},
		{Name: "@", Type: "MX", TTL: 3600, RData: "10 mail.e412.in.", Enabled: true},
		{Name: "@", Type: "TXT", TTL: 3600, RData: `"v=spf1 -all"`, Enabled: true},
		{Name: "*.nexus", Type: "A", TTL: 300, RData: "192.168.160.200", Enabled: true},
	}
	out := zones.Render(testZone(), recs)

	zp := dns.NewZoneParser(strings.NewReader(out), "", "")
	var got []dns.RR
	for rr, ok := zp.Next(); ok; rr, ok = zp.Next() {
		got = append(got, rr)
	}
	if err := zp.Err(); err != nil {
		t.Fatalf("rendered file does not parse: %v\n---\n%s", err, out)
	}
	// 5 records + the SOA.
	if len(got) != 6 {
		t.Fatalf("parsed %d RRs, want 6:\n%s", len(got), out)
	}
	var haveSOA bool
	for _, rr := range got {
		if rr.Header().Rrtype == dns.TypeSOA {
			haveSOA = true
			soa := rr.(*dns.SOA)
			if soa.Serial != 7 || soa.Ns != "ns.e412.in." || soa.Mbox != "hostadmin.e412.in." {
				t.Errorf("SOA = %+v; want serial 7, ns.e412.in., hostadmin.e412.in.", soa)
			}
		}
	}
	if !haveSOA {
		t.Error("no SOA in the rendered file")
	}
}

// A zone file has no concept of a disabled record. Emitting one would
// silently enable it on whatever imports the file.
func TestRenderOmitsDisabledRecords(t *testing.T) {
	recs := []store.ZoneRecord{
		{Name: "on", Type: "A", TTL: 300, RData: "1.2.3.4", Enabled: true},
		{Name: "off", Type: "A", TTL: 300, RData: "5.6.7.8", Enabled: false},
	}
	out := zones.Render(testZone(), recs)
	if strings.Contains(out, "5.6.7.8") {
		t.Errorf("disabled record was rendered:\n%s", out)
	}
	if !strings.Contains(out, "1.2.3.4") {
		t.Errorf("enabled record missing:\n%s", out)
	}
}

func TestRenderStartsWithOriginAndTTL(t *testing.T) {
	out := zones.Render(testZone(), nil)
	if !strings.HasPrefix(out, "$ORIGIN e412.in.\n") {
		t.Errorf("want $ORIGIN first, got:\n%s", out)
	}
	if !strings.Contains(out, "$TTL ") {
		t.Errorf("want a $TTL directive, got:\n%s", out)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/zones/ -run TestRender -v`
Expected: FAIL — `zones.Render` undefined.

- [ ] **Step 3: Implement**

`Render` writes `$ORIGIN <name>.`, `$TTL <SOATTL>`, then the SOA as
`@ <SOATTL> IN SOA <ns>. <mbox>. ( serial refresh retry expire minimum )`, then one line
per enabled record as `<name> <ttl> IN <type> <rdata>`. Names are already relative and
`@` is already the apex convention, so they pass through unchanged.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/zones/ -run TestRender -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/zones/zonefile.go internal/zones/zonefile_test.go
git commit -m "feat(zones): render a zone as a BIND master file"
```

---

### Task 2: Parse a master file into records

**Files:**
- Create: `internal/zones/zonefile_parse.go`
- Test: `internal/zones/zonefile_parse_test.go`

**Interfaces:**
- Consumes: nothing from Task 1 — parsing is independent of rendering.
- Produces:
  - `type ParsedZone struct { SOA *dns.SOA; Records []ParsedRecord }`
  - `type ParsedRecord struct { Name, Type, RData string; TTL uint32; Line int }`
  - `func Parse(text, apex string) (ParsedZone, []string)` — the second return is parse errors, one per line, empty when the file is clean.

`Line` exists so Task 4 can name every offending line rather than just the first. Do not drop it.

- [ ] **Step 1: Write the failing test**

```go
func TestParseRelativizesNames(t *testing.T) {
	const f = `$ORIGIN e412.in.
$TTL 3600
@   IN SOA ns.e412.in. hostadmin.e412.in. ( 7 900 300 604800 900 )
@   IN NS  ns.e412.in.
bifrost 300 IN A 57.129.69.158
*.nexus 300 IN A 192.168.160.200
`
	pz, errs := zones.Parse(f, "e412.in")
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if pz.SOA == nil || pz.SOA.Serial != 7 {
		t.Fatalf("SOA = %+v; want serial 7", pz.SOA)
	}
	got := map[string]string{}
	for _, r := range pz.Records {
		got[r.Name+" "+r.Type] = r.RData
	}
	// Names come back RELATIVE to the apex, matching how they are stored.
	if got["bifrost A"] != "57.129.69.158" || got["*.nexus A"] != "192.168.160.200" || got["@ NS"] == "" {
		t.Fatalf("records = %v; want relative names", got)
	}
	// The SOA is returned separately, not as a record.
	for _, r := range pz.Records {
		if r.Type == "SOA" {
			t.Error("SOA leaked into Records; it belongs on the zone row")
		}
	}
}

// $TTL supplies the TTL for records that omit one (RFC 2308 §4).
func TestParseAppliesDollarTTL(t *testing.T) {
	const f = `$ORIGIN e412.in.
$TTL 1234
@ IN SOA ns.e412.in. hostadmin.e412.in. ( 1 900 300 604800 900 )
noTTL IN A 1.2.3.4
`
	pz, errs := zones.Parse(f, "e412.in")
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	for _, r := range pz.Records {
		if r.Name == "notttl" || r.Name == "noTTL" {
			if r.TTL != 1234 {
				t.Errorf("TTL = %d, want 1234 from $TTL", r.TTL)
			}
		}
	}
}

// Every bad line is reported, not just the first — Task 4 shows them all.
func TestParseReportsEveryBadLine(t *testing.T) {
	const f = `$ORIGIN e412.in.
$TTL 300
@ IN SOA ns.e412.in. hostadmin.e412.in. ( 1 900 300 604800 900 )
good IN A 1.2.3.4
bad1 IN A not-an-ip
bad2 IN MX Mx
`
	_, errs := zones.Parse(f, "e412.in")
	if len(errs) < 2 {
		t.Fatalf("errs = %v; want both bad lines reported", errs)
	}
}

// A file whose records are absolute under a different apex is a user error
// worth naming, not something to silently accept.
func TestParseRejectsOutOfZoneNames(t *testing.T) {
	const f = `$ORIGIN e412.in.
$TTL 300
@ IN SOA ns.e412.in. hostadmin.e412.in. ( 1 900 300 604800 900 )
stranger.example.com. 300 IN A 1.2.3.4
`
	_, errs := zones.Parse(f, "e412.in")
	if len(errs) == 0 {
		t.Fatal("an out-of-zone name was accepted")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/zones/ -run TestParse -v`
Expected: FAIL — `zones.Parse` undefined.

- [ ] **Step 3: Implement**

`dns.NewZoneParser(strings.NewReader(text), dns.Fqdn(apex), "")` handles `$ORIGIN`,
`$TTL`, `@`, parentheses and comments. Iterate `zp.Next()`; on each RR, split the SOA
out, and convert every other RR to a `ParsedRecord` whose `Name` is relative (use
`RelName` from `zone.go`) and whose `RData` is the RR's presentation string with its
header stripped — `strings.TrimPrefix(rr.String(), rr.Header().String())`.
`zp.Err()` reports the first parse error and stops; to report every bad line, capture
each error and continue where the parser allows, otherwise record the error with the
line number `zp` reports and stop. Say in your report which behaviour miekg gives you.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/zones/ -run TestParse -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/zones/zonefile_parse.go internal/zones/zonefile_parse_test.go
git commit -m "feat(zones): parse a BIND master file into relative records"
```

---

### Task 3: Export endpoint

**Files:**
- Create: `internal/api/zonefile_handlers.go`
- Modify: `internal/api/server.go` (call `s.zoneFileRoutes()` from `registerRoutes`)
- Modify: `internal/api/openapi.yaml`
- Test: `internal/api/zonefile_handlers_test.go`

**Interfaces:**
- Consumes: `zones.Render` (Task 1).
- Produces: `GET /api/v1/zones/{id}/file`.

- [ ] **Step 1: Write the failing test**

```go
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
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/api/ -run TestZoneFileExport -v`
Expected: FAIL — route not registered, 404 on all three.

- [ ] **Step 3: Implement and document**

Register with `s.route("GET /api/v1/zones/{id}/file", s.requireAuth(s.handleZoneFileExport))`.
Load the zone (404 if absent), read its records, `zones.Render`, write with
`Content-Type: text/dns; charset=utf-8` and
`Content-Disposition: attachment; filename="<apex>.zone"`.

**Add the path to `openapi.yaml` in this same commit** — `TestOpenAPIServedAndCoversRoutes`
asserts a two-way path+method match and will fail otherwise. Document 200, 401, 404, 503.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/api/ -run 'TestZoneFileExport|TestOpenAPI' -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/api/zonefile_handlers.go internal/api/zonefile_handlers_test.go internal/api/server.go internal/api/openapi.yaml
git commit -m "feat(api): export a zone as a BIND master file"
```

---

### Task 4: Import — dry run and commit

**Files:**
- Modify: `internal/api/zonefile_handlers.go`
- Modify: `internal/api/openapi.yaml`
- Test: `internal/api/zonefile_handlers_test.go`

**Interfaces:**
- Consumes: `zones.Parse` (Task 2), `buildZoneRecord` (existing, `zonerecords_handlers.go:92`).
- Produces: `POST /api/v1/zones/{id}/file` with body `{"content": "...", "dry_run": true|false}`, returning `{"add": [...], "change": [...], "delete": [...], "errors": [...]}`.

**The four rules from spec §8, each with a test:**

1. **Replace, not merge** — anything in the zone and not in the file is deleted.
2. **The whole file is rejected if any record is invalid**, and the error names every offending line. Validation is `buildZoneRecord`, the same one behind `POST /records` — not a second copy.
3. **Import does NOT trigger auto-PTR.** It is the only exception to §7's rule, so it is deliberate and must be pinned.
4. **Serial becomes `max(file, current) + 1`** — never lower than what has already been served.

- [ ] **Step 1: Write the failing test**

```go
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
// value would ignore the zone forever after (§4 D).
func TestZoneFileImportNeverLowersTheSerial(t *testing.T) {
	srv, zid := newTestServerWithZone(t, "e412.in")
	// Push the zone's serial well above the file's 5.
	for range 3 {
		srv.createRecord(t, zid, fmt.Sprintf(`{"name":"r%d","type":"A","ttl":300,"rdata":"1.2.3.4"}`, rand.IntN(1<<30)))
	}
	before := srv.zone(t, zid).SOASerial

	if rec := srv.do(t, "POST", fmt.Sprintf("/api/v1/zones/%d/file", zid), importBody(t, importFile, false)); rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body)
	}
	if after := srv.zone(t, zid).SOASerial; after <= before {
		t.Fatalf("serial went %d -> %d; must never decrease", before, after)
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
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/api/ -run TestZoneFileImport -v`
Expected: FAIL — route not registered.

- [ ] **Step 3: Implement and document**

Register `s.route("POST /api/v1/zones/{id}/file", s.requireAuth(s.handleZoneFileImport))`.
Flow: load zone (404) → reject `internal` (409) → `zones.Parse` → for each parsed record
call `buildZoneRecord` against the accumulating set, collecting *every* failure with its
line number → if any errors, **422** with the full list and no writes → otherwise compute
the diff against current records → if `dry_run`, return the diff → else apply it inside
the existing store calls, set the SOA from the file with
`serial = max(file, current) + 1`, and call `reloadZones` once at the end.

Do **not** call `syncPTR`. Add a comment saying so and why, or someone will "fix" it.

**Add the path to `openapi.yaml` in this same commit.** Document 200, 400, 401, 404, 409, 422, 503.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/api/ -run 'TestZoneFileImport|TestOpenAPI' -v`
Expected: PASS

- [ ] **Step 5: Revert-verify three rules separately**

| Remove | Test that must fail |
|---|---|
| the delete phase | `TestZoneFileImportReplaces` |
| the validate-before-write ordering | `TestZoneFileImportRejectsWholeFileAndNamesEveryProblem` |
| the `max(file, current) + 1` clamp | `TestZoneFileImportNeverLowersTheSerial` |

Report all three failure outputs.

- [ ] **Step 6: Commit**

```bash
git add internal/api/zonefile_handlers.go internal/api/zonefile_handlers_test.go internal/api/openapi.yaml
git commit -m "feat(api): import a zone file, replacing contents after a dry run"
```

---

### Task 5: Web — Export and Import on the zone detail page

**Files:**
- Modify: `web/src/hooks/use-zones.ts`
- Modify: `web/src/pages/zones/detail.tsx`
- Test: `web/src/pages/zones/detail.test.tsx`

**Interfaces:**
- Consumes: both endpoints from Tasks 3-4.

**Design:** no artboard exists for this. Follow the page's established idiom — the header
already holds `Add record` / `Disable zone` / `Delete zone`; Export and Import join them.
The diff is a band under the header, in the same shape as the SOA band, not a dialog:
this page has no dialogs and adding one would be the only modal in the app.

- [ ] **Step 1: Write the failing test**

```tsx
test("Export downloads the zone file", async () => {
  const user = userEvent.setup();
  renderZoneDetail({ zone: zone({ id: 1, name: "e412.in" }) });
  await user.click(await screen.findByRole("button", { name: /export/i }));
  // The click must reach the endpoint; asserting the anchor's download
  // attribute alone would pass even if the request never fired.
  await waitFor(() => expect(fetchedPaths).toContain("/api/v1/zones/1/file"));
});

test("Import shows the diff before anything is written", async () => {
  const user = userEvent.setup();
  let committed = false;
  server.use(
    http.post("*/zones/1/file", async ({ request }) => {
      const body = (await request.json()) as { dry_run: boolean };
      if (!body.dry_run) committed = true;
      return HttpResponse.json({ add: [{ name: "new", type: "A" }], change: [], delete: [{ name: "old", type: "A" }], errors: [] });
    }),
  );
  renderZoneDetail({ zone: zone({ id: 1, name: "e412.in" }) });
  await user.click(await screen.findByRole("button", { name: /import/i }));
  await user.upload(screen.getByLabelText(/zone file/i), new File(["$ORIGIN e412.in.\n"], "e412.in.zone"));

  expect(await screen.findByText(/1 to add/i)).toBeInTheDocument();
  expect(screen.getByText(/1 to delete/i)).toBeInTheDocument();
  expect(committed).toBe(false);
});

test("a rejected import shows every problem the server named", async () => {
  const user = userEvent.setup();
  server.use(
    http.post("*/zones/1/file", () =>
      HttpResponse.json(
        { errors: ["line 5: records in the same RRSet must share one TTL", "line 7: CNAME cannot coexist with another record at the same name"] },
        { status: 422 },
      ),
    ),
  );
  renderZoneDetail({ zone: zone({ id: 1, name: "e412.in" }) });
  await user.click(await screen.findByRole("button", { name: /import/i }));
  await user.upload(screen.getByLabelText(/zone file/i), new File(["bad"], "bad.zone"));

  expect(await screen.findByText(/line 5:/)).toBeInTheDocument();
  expect(screen.getByText(/line 7:/)).toBeInTheDocument();
});

test("a built-in zone offers no Import", async () => {
  renderZoneDetail({ zone: zone({ id: 1, name: "localhost", type: "internal" }) });
  await screen.findByText("localhost");
  expect(screen.queryByRole("button", { name: /import/i })).not.toBeInTheDocument();
  // Export stays — reading a built-in is allowed.
  expect(screen.getByRole("button", { name: /export/i })).toBeInTheDocument();
});
```

- [ ] **Step 2: Run to verify it fails**

Run: `cd web && pnpm vitest run src/pages/zones/detail.test.tsx`
Expected: FAIL — no Export or Import button.

- [ ] **Step 3: Implement**

`useExportZoneFile` fetches and triggers a download; `useImportZoneFile` posts
`{content, dry_run}`. The Import flow is: pick a file → post with `dry_run: true` → render
the diff with an Apply button → post again with `dry_run: false`. A 422 renders the
server's `errors` verbatim, one per line — they already name the line number, and
rewriting them would lose that.

Import is hidden for `type === "internal"`; Export is not.

- [ ] **Step 4: Run the suite**

Run: `cd web && pnpm lint && pnpm format:check && pnpm typecheck && pnpm test && pnpm build && pnpm test:e2e`
Expected: all clean.

- [ ] **Step 5: Commit**

```bash
git add web/src/hooks/use-zones.ts web/src/pages/zones/detail.tsx web/src/pages/zones/detail.test.tsx
git commit -m "feat(web): export and import a zone file from the zone page"
```

---

### Task 6: Docs

**Files:**
- Modify: `docs/dashboard.md` (the Zones section), `docs/api.md`

- [ ] **Step 1: Write it**

Cover, in `docs/dashboard.md`: that export gives a standard BIND master file portable to
any DNS server; that **import replaces** — the file becomes the zone and anything not in
it is deleted, which is why there is a dry run; that an invalid file is rejected whole
with every problem listed rather than partially applied; that **import does not create
PTR records** even though adding an A record by hand does, so a reverse zone is imported
from its own file; and that built-in zones can be exported but not imported into.

In `docs/api.md`: both endpoints, the `dry_run` flag, and the 422 error shape.

- [ ] **Step 2: Verify links**

```bash
python3 - <<'PY'
import re,os,glob
bad=[]
for f in glob.glob("docs/*.md")+["README.md"]:
    d=os.path.dirname(f) or "."
    for m in re.finditer(r'\]\((?!https?:)([^)#]+)(#[^)]*)?\)', open(f).read()):
        t=os.path.normpath(os.path.join(d,m.group(1)))
        if not os.path.exists(t): bad.append(f"{f} -> {m.group(1)}")
print("\n".join(bad) if bad else "all relative links resolve")
PY
```

Expected: `all relative links resolve`

- [ ] **Step 3: Commit**

```bash
git add docs
git commit -m "docs: zone file import and export"
```

---

## Self-Review

**Spec coverage:** §8 export → Task 1 (render) + Task 3 (endpoint), including the
disabled-record omission and built-ins being exportable. §8 replace-not-merge → Task 4
(`TestZoneFileImportReplaces`). §8 dry run → Task 4 + Task 5. §8 reject-whole-file-naming-every-line
→ Task 2 (`Line` on `ParsedRecord`, every bad line reported) + Task 4 (422 with the full
list). §8 no auto-PTR → Task 4, pinned. §8 built-ins reject import → Task 4. §8 serial
`max(file, current) + 1` → Task 4, revert-verified. §8's RFC table: 1035 §5 → Tasks 1-2;
1034 §3.6.1 (the file's SOA is the zone's) → Task 4; 2308 §4 (`$TTL` default) → Task 2.

**Gap found and closed:** nothing in the plan told the user that import is destructive
before they click it. The dry run makes it visible in the UI, but the docs needed to say
it plainly too — added to Task 6.

**Type consistency:** `Render(store.Zone, []store.ZoneRecord) string` and
`Parse(text, apex string) (ParsedZone, []string)` defined in Tasks 1-2 and used with
those names in Tasks 3-4. `ParsedRecord.Line` defined in Task 2 and consumed by Task 4's
error messages. `buildZoneRecord`'s existing signature
`(store.Zone, zoneRecordWrite, []store.ZoneRecord, int64, bool)` is quoted in Task 4's
Interfaces so the implementer does not have to guess it.

**Placeholder scan:** none. One test in Task 5 references `fetchedPaths`, a helper that
must exist in the web test harness — if it does not, the implementer adds it, which is
ordinary test scaffolding rather than an elided assertion.
