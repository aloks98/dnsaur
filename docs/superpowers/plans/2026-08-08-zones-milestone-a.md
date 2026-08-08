# Zones Milestone A Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the flat `local_records` override table with authoritative forward zones — so a name inside a zone we hold is answered by us or refused by us, and never forwarded upstream.

**Architecture:** Two new tables (`zones`, `zone_records`) with records named relative to the zone apex and rdata stored in DNS presentation format. `internal/records` is replaced by `internal/zones`, whose middleware first finds the deepest enabled zone covering the qname (the *zone cut*) and then answers only from that zone — ANSWER, NODATA, NXDOMAIN or referral. RRs are built by handing `miekg/dns` a reassembled master-file line, so record-type support is a data question rather than a code question.

**Tech Stack:** Go 1.26; `github.com/miekg/dns` (`dns.NewRR` for parse+validate); `github.com/pressly/goose/v3` (SQL + Go migrations); existing `internal/store` sql layer; React 19 + TanStack Query v5 + rnui for the UI.

## Global Constraints

- Module `github.com/aloks98/dnsaur`; Go 1.26. Branch: `feat/zones`.
- Before every commit: `go test -race ./...` AND `~/go/bin/golangci-lint run ./...` (0 issues). For web tasks additionally `pnpm lint && pnpm typecheck && pnpm test && pnpm build` from `web/`.
- Conventional commits; PR titles too.
- **Revert-verify discipline:** every new test must be shown to fail with its fix removed. A step that says "run it and watch it fail" is not optional.
- Error envelope stays `{"error":"<message>"}`. DB errors → 503 `storage unavailable` via `storeErr`. Unauthed → 401.
- UI copy is plain and brief: state the fact and stop. Explanation belongs in `docs/`, not on screen.
- Timestamps in API JSON are Unix milliseconds.
- `DisallowUnknownFields` is on for API decode — every new request field must be in the struct or requests 400.
- Both SQLite and Postgres migrations must be written and both must pass.
- **RFC conformance is binding, not aspirational.** See the spec's §6 table
  (`docs/superpowers/specs/2026-08-08-zones-design.md`). The rules that apply at
  *write time* — enforce every one, each with its own test:
  - **RFC 2181 §5.2** — all RRs in an RRSet (same name + type) must share one
    TTL. Adding a second A at a name with a different TTL is a 409.
  - **RFC 2181 §8** — TTL is 31-bit. Reject anything above 2147483647 with 400.
    A TTL with the high bit set is read by resolvers as *zero*, so an unclamped
    value silently means "never cache" rather than "cache for a long time".
  - **RFC 1912 §2.4** — no CNAME at the zone apex (`@`). The sibling check does
    not catch this: our SOA lives on the `zones` row, so the apex looks empty.
  - **RFC 1034 §3.6.2** — no CNAME beside any other type at the same name.
  - **RFC 4592 §2.1.1** — `*` is a wildcard only as the *leftmost* label.
    `a.*.example` is a literal name containing an asterisk, not a wildcard.
  - **RFC 1035 §2.3.4** — label ≤ 63 octets, name ≤ 255 octets.

## File Structure

```
internal/store/migrations/{sqlite,postgres}/0004_zones.sql   — Task 1 (DONE, on branch)
internal/store/zones.go                                      — Task 2 (Zone/ZoneRecord types, ZoneStore)
internal/store/migrate.go                                    — Task 3 (register Go migration)
internal/store/zonemigrate.go                                — Task 3 (local_records → zones conversion)
internal/zones/zone.go                                       — Task 4 (zone cut, relative names, RR build)
internal/zones/answer.go                                     — Task 5 (ANSWER/NODATA/NXDOMAIN/referral)
internal/zones/resolver.go                                   — Task 6 (snapshot, Reload, Middleware)
internal/dnssrv/pipeline.go                                  — Task 6 (DecisionAuthoritative; drop DecisionAllowed)
internal/app/app.go                                          — Task 6 (wire zones.Resolver)
internal/api/zones_handlers.go                               — Task 7 (zones CRUD)
internal/api/zonerecords_handlers.go                         — Task 8 (records CRUD)
internal/api/openapi.yaml                                    — Task 9
web/src/api/types.ts, web/src/hooks/use-zones.ts             — Task 10
web/src/test/msw-handlers.ts                                 — Task 10
web/src/pages/zones/list.tsx                                 — Task 11
web/src/pages/zones/detail.tsx                               — Task 12
web/src/lib/nav.ts, web/src/App.tsx                          — Task 11 (route rename)
docs/dashboard.md, docs/api.md, docs/ui-contract.md, README  — Task 13
```

Deleted at the end of Task 6: `internal/records/` (whole package). Deleted in Task 8: `internal/api/records_handlers.go`. Deleted in Task 12: `web/src/pages/dns.tsx` and its test.

---

### Task 1: Schema migration — DONE

Already on branch `feat/zones` as `0004_zones.sql` for both dialects. Verified with `go test ./internal/store/`. No action; listed so task numbering matches the commit history.

---

### Task 2: Zone and ZoneRecord types + ZoneStore

**Files:**
- Create: `internal/store/zones.go`
- Modify: `internal/store/store.go` (add `Zones() ZoneStore` to the `Store` interface)
- Modify: `internal/store/sql.go:18` (add `func (s *sqlStore) Zones() ZoneStore { return &zoneStore{s} }`)
- Test: `internal/store/zones_test.go`

**Interfaces:**
- Produces: `store.Zone`, `store.ZoneRecord`, `store.ZoneStore` with `Zones(ctx) ([]Zone, error)`, `Zone(ctx, id) (Zone, error)`, `AddZone(ctx, Zone) (int64, error)`, `UpdateZone(ctx, Zone) error`, `DeleteZone(ctx, id) error`, `BumpSerial(ctx, zoneID) error`, `Records(ctx, zoneID) ([]ZoneRecord, error)`, `AllRecords(ctx) (map[int64][]ZoneRecord, error)`, `AddRecord(ctx, ZoneRecord) (int64, error)`, `UpdateRecord(ctx, ZoneRecord) error`, `DeleteRecord(ctx, id) error`.

- [ ] **Step 1: Write the failing test**

```go
func TestZoneStoreRoundTrip(t *testing.T) {
	st := newTestStore(t)
	ctx := t.Context()
	id, err := st.Zones().AddZone(ctx, store.Zone{
		Name: "e412.in", Type: "primary", Enabled: true,
		SOANS: "ns.e412.in", SOAMbox: "hostadmin.e412.in",
		SOASerial: 1, SOARefresh: 900, SOARetry: 300,
		SOAExpire: 604800, SOAMinimum: 900,
	})
	if err != nil {
		t.Fatalf("AddZone: %v", err)
	}
	if _, err := st.Zones().AddRecord(ctx, store.ZoneRecord{
		ZoneID: id, Name: "bifrost", Type: "A", TTL: 3600,
		RData: "57.129.69.158", Enabled: true,
	}); err != nil {
		t.Fatalf("AddRecord: %v", err)
	}
	recs, err := st.Zones().Records(ctx, id)
	if err != nil || len(recs) != 1 {
		t.Fatalf("Records = %v, %v; want 1 record", recs, err)
	}
	if recs[0].RData != "57.129.69.158" {
		t.Errorf("RData = %q, want 57.129.69.158", recs[0].RData)
	}
	// Deleting a zone must take its records with it (ON DELETE CASCADE);
	// orphan records would resurface if the apex were ever recreated.
	if err := st.Zones().DeleteZone(ctx, id); err != nil {
		t.Fatalf("DeleteZone: %v", err)
	}
	if recs, _ := st.Zones().Records(ctx, id); len(recs) != 0 {
		t.Errorf("records survived zone delete: %v", recs)
	}
}

func TestBumpSerialIncrements(t *testing.T) {
	st := newTestStore(t)
	ctx := t.Context()
	id, _ := st.Zones().AddZone(ctx, store.Zone{Name: "a.test", Type: "primary", Enabled: true, SOASerial: 7})
	if err := st.Zones().BumpSerial(ctx, id); err != nil {
		t.Fatalf("BumpSerial: %v", err)
	}
	z, _ := st.Zones().Zone(ctx, id)
	if z.SOASerial != 8 {
		t.Errorf("SOASerial = %d, want 8", z.SOASerial)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/store/ -run 'TestZoneStore|TestBumpSerial' -v`
Expected: FAIL — compile error, `st.Zones` undefined.

- [ ] **Step 3: Write the types and store**

`internal/store/zones.go`. Mirror the existing `recordStore` patterns exactly: `s.insert` for INSERT (handles the sqlite/postgres id difference), `s.q(...)` to rebind placeholders, `execOne` for UPDATE/DELETE so a missing row returns `ErrNotFound`, and non-nil empty slices so JSON marshals to `[]` not `null`.

```go
// Zone is a suffix this server is authoritative for. A query for a name
// inside an enabled zone is answered from it or refused by it — it is
// never forwarded, which is what separates a zone from the override list
// it replaced.
type Zone struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"` // apex, lowercase, no trailing dot
	Type    string `json:"type"` // primary | secondary | stub | forwarder | internal
	Enabled bool   `json:"enabled"`

	SOANS     string `json:"soa_ns"`
	SOAMbox   string `json:"soa_mbox"`
	SOASerial uint32 `json:"soa_serial"`
	SOARefresh uint32 `json:"soa_refresh"`
	SOARetry   uint32 `json:"soa_retry"`
	SOAExpire  uint32 `json:"soa_expire"`
	// SOAMinimum is the negative-cache TTL for NXDOMAINs this zone hands
	// out (RFC 2308), not a floor on positive answers.
	SOAMinimum uint32 `json:"soa_minimum"`

	Primaries   string `json:"primaries"`
	TSIGKeyID   int64  `json:"tsig_key_id"`
	ExpiresAt   int64  `json:"expires_at"`
	RefreshedAt int64  `json:"refreshed_at"`
	CreatedAt   int64  `json:"created_at"`
	ModifiedAt  int64  `json:"modified_at"`
}

// ZoneRecord is one RR, named relative to its zone's apex.
type ZoneRecord struct {
	ID      int64  `json:"id"`
	ZoneID  int64  `json:"zone_id"`
	Name    string `json:"name"` // '@', 'bifrost', '*', '*.nexus'
	Type    string `json:"type"`
	TTL     uint32 `json:"ttl"`
	// RData is presentation format, rdata portion only.
	RData   string `json:"rdata"`
	Enabled bool   `json:"enabled"`
	Comment string `json:"comment"`
}
```

`BumpSerial` is `UPDATE zones SET soa_serial = soa_serial + 1, modified_at = ? WHERE id = ?` — done in SQL rather than read-modify-write so two concurrent record edits can't land on the same serial.

`AllRecords` returns every record grouped by zone in one query (`SELECT ... FROM zone_records ORDER BY zone_id, name`), because the resolver rebuilds a whole snapshot on reload and N+1 queries per zone would make reload cost scale with zone count.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/store/ -run 'TestZoneStore|TestBumpSerial' -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/store/zones.go internal/store/zones_test.go internal/store/store.go internal/store/sql.go
git commit -m "feat(store): zones and zone_records, with serial bumped in SQL"
```

---

### Task 3: Data migration from local_records

**Files:**
- Create: `internal/store/zonemigrate.go`
- Modify: `internal/store/migrate.go:35` (register the Go migration on the provider)
- Test: `internal/store/zonemigrate_test.go` — **`package store` (internal)**, matching every other test file in this directory. `splitZone` is unexported, so an external `store_test` package cannot call it; types are therefore written unqualified (`LocalRecord`, not `store.LocalRecord`).

**Interfaces:**
- Consumes: `store.Zone`, `store.ZoneRecord` (Task 2).
- Produces: `func splitZone(name string) (apex, rel string)` — used only here, but tested directly because the grouping rule is the part a user can disagree with.

**Why Go and not SQL:** the conversion needs suffix grouping, relative-name rewriting, and detection of CNAME-alongside-siblings — which was legal in `local_records` and is not legal in a zone. That is logic with edge cases, so it gets tests. SQLite has no `regexp_replace`, so doing it in SQL would also mean two divergent dialect implementations of the same rule.

**The grouping rule, stated plainly:** the apex is the **last two labels**; everything before is the relative name. `bifrost.e412.in` → zone `e412.in`, name `bifrost`. `*.nexus.e412.in` → zone `e412.in`, name `*.nexus`. `nas.home.lan` → zone `home.lan`, name `nas`. A name with two or fewer labels is its own apex with name `@`. This is wrong for `foo.co.uk`-shaped names and right for every homelab case; the alternative (a public-suffix list) is a dependency and a download for a one-time conversion.

- [ ] **Step 1: Write the failing test**

```go
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

func TestMigrateLocalRecordsToZones(t *testing.T) {
	st := newTestStore(t)
	ctx := t.Context()
	// Seed the pre-zone table directly: the migration is what we're testing,
	// so it must not depend on the store API that replaces it.
	seedLocalRecords(t, st, []LocalRecord{
		{Name: "bifrost.e412.in", Type: "A", Value: "57.129.69.158", TTL: 3600},
		{Name: "*.nexus.e412.in", Type: "A", Value: "192.168.160.200", TTL: 3600},
		{Name: "nas.home.lan", Type: "A", Value: "192.168.1.5", TTL: 300},
	})
	if err := migrateLocalRecords(ctx, st); err != nil {
		t.Fatalf("migrateLocalRecords: %v", err)
	}
	zones, _ := st.Zones().Zones(ctx)
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
	recs, _ := st.Zones().Records(ctx, e412.ID)
	got := map[string]string{}
	for _, r := range recs {
		got[r.Name] = r.RData
	}
	want := map[string]string{"bifrost": "57.129.69.158", "*.nexus": "192.168.160.200"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("records = %v, want %v", got, want)
	}
}

// A CNAME beside another type at the same name was legal in local_records
// and is illegal in a zone (RFC 1034 3.6.2). The migration must not
// silently produce a zone that violates the rule the API will enforce.
func TestMigrateDropsConflictingCNAME(t *testing.T) {
	st := newTestStore(t)
	ctx := t.Context()
	seedLocalRecords(t, st, []LocalRecord{
		{Name: "www.e412.in", Type: "A", Value: "192.168.1.9", TTL: 300},
		{Name: "www.e412.in", Type: "CNAME", Value: "bifrost.e412.in", TTL: 300},
	})
	if err := migrateLocalRecords(ctx, st); err != nil {
		t.Fatalf("migrateLocalRecords: %v", err)
	}
	zones, _ := st.Zones().Zones(ctx)
	recs, _ := st.Zones().Records(ctx, zones[0].ID)
	if len(recs) != 1 || recs[0].Type != "A" {
		t.Fatalf("records = %+v; want only the A (CNAME dropped)", recs)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/store/ -run 'TestSplitZone|TestMigrateLocal|TestMigrateDrops' -v`
Expected: FAIL — `splitZone` and `migrateLocalRecords` undefined.

- [ ] **Step 3: Implement the conversion**

```go
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
```

`migrateLocalRecords` reads every `local_records` row, groups by `splitZone` apex, creates one primary zone per apex with `SOANS = "ns." + apex`, `SOAMbox = "hostadmin." + apex`, serial 1 and the schema defaults for the timers, then inserts each record with its relative name, mapping `Value` → `RData` unchanged (both are presentation format for A/AAAA/CNAME/TXT). Within one name, if a CNAME appears alongside any other type, the **CNAME is dropped and the others kept** — dropping the CNAME loses an alias, dropping the others loses an address, and an address is the thing a client fails hardest without. Log each drop at WARN with the name so it is recoverable from the journal.

The conversion runs as a goose Go migration at version 5, registered in `migrate.go`:

```go
provider, err := goose.NewProvider(gooseDialect, db, sub,
	goose.WithGoMigrations(goose.NewGoMigration(5, &goose.GoFunc{RunTx: upZonesData}, nil)))
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/store/ -run 'TestSplitZone|TestMigrateLocal|TestMigrateDrops' -v`
Expected: PASS

- [ ] **Step 5: Verify the drop is real**

Run: `go test ./internal/store/ -run TestMigrateDropsConflictingCNAME` with the CNAME-conflict branch commented out.
Expected: FAIL with `want only the A (CNAME dropped)`. Restore the branch.

- [ ] **Step 6: Commit**

```bash
git add internal/store/zonemigrate.go internal/store/zonemigrate_test.go internal/store/migrate.go
git commit -m "feat(store): convert local_records into zones on migrate"
```

---

### Task 4: Zone cut and RR construction

**Files:**
- Create: `internal/zones/zone.go`
- Test: `internal/zones/zone_test.go`

**Interfaces:**
- Produces: `type Zone struct { store.Zone; Records map[string][]store.ZoneRecord }` (records keyed by relative name); `func (idx *Index) Find(qname string) *Zone`; `func relName(qname, apex string) string`; `func toRR(fqdn string, rec store.ZoneRecord) (dns.RR, error)`; `func (z *Zone) SOA() *dns.SOA`.

- [ ] **Step 1: Write the failing test**

```go
func TestFindDeepestZoneWins(t *testing.T) {
	idx := zones.NewIndex([]zones.Zone{
		{Zone: store.Zone{Name: "in", Enabled: true}},
		{Zone: store.Zone{Name: "e412.in", Enabled: true}},
	})
	if z := idx.Find("bifrost.e412.in"); z == nil || z.Name != "e412.in" {
		t.Fatalf("Find = %v, want e412.in — a nested zone must win over its parent", z)
	}
	if z := idx.Find("example.com"); z != nil {
		t.Errorf("Find(example.com) = %v, want nil", z)
	}
}

func TestFindSkipsDisabledZone(t *testing.T) {
	idx := zones.NewIndex([]zones.Zone{{Zone: store.Zone{Name: "e412.in", Enabled: false}}})
	// A disabled zone must not claim authority: the query has to be free to
	// go upstream, which is the whole point of the toggle.
	if z := idx.Find("bifrost.e412.in"); z != nil {
		t.Errorf("Find = %v, want nil for a disabled zone", z)
	}
}

func TestRelName(t *testing.T) {
	for _, tc := range []struct{ q, apex, want string }{
		{"e412.in", "e412.in", "@"},
		{"bifrost.e412.in", "e412.in", "bifrost"},
		{"a.b.e412.in", "e412.in", "a.b"},
	} {
		if got := zones.RelName(tc.q, tc.apex); got != tc.want {
			t.Errorf("RelName(%q,%q) = %q, want %q", tc.q, tc.apex, got, tc.want)
		}
	}
}

// The presentation-format rdata decision earns its keep here: MX and CAA
// were never supported by the old per-type switch and need no code.
func TestToRRHandlesTypesTheOldSwitchDidNot(t *testing.T) {
	for _, tc := range []struct{ rtype, rdata string }{
		{"A", "192.168.150.28"},
		{"MX", "10 mail.e412.in."},
		{"SRV", "0 5 5060 sip.e412.in."},
		{"CAA", `0 issue "letsencrypt.org"`},
	} {
		rr, err := zones.ToRR("e412.in.", store.ZoneRecord{Type: tc.rtype, TTL: 3600, RData: tc.rdata})
		if err != nil {
			t.Errorf("ToRR(%s %q) error: %v", tc.rtype, tc.rdata, err)
			continue
		}
		if dns.TypeToString[rr.Header().Rrtype] != tc.rtype {
			t.Errorf("ToRR(%s) built a %s", tc.rtype, dns.TypeToString[rr.Header().Rrtype])
		}
	}
}

func TestToRRRejectsGarbage(t *testing.T) {
	if _, err := zones.ToRR("e412.in.", store.ZoneRecord{Type: "A", TTL: 300, RData: "not-an-ip"}); err == nil {
		t.Error("ToRR accepted a non-IP as an A record")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/zones/ -v`
Expected: FAIL — package `internal/zones` does not exist.

- [ ] **Step 3: Implement**

```go
// ToRR rebuilds an RR by handing miekg/dns a master-file line. This is the
// same code path the API validates writes with, so a row that parses here
// is a row that can be served — the two cannot drift.
func ToRR(fqdn string, rec store.ZoneRecord) (dns.RR, error) {
	return dns.NewRR(fmt.Sprintf("%s %d IN %s %s", dns.Fqdn(fqdn), rec.TTL, rec.Type, rec.RData))
}
```

`Index.Find` walks labels right-to-left, longest match first, skipping zones with `Enabled == false`. `RelName` strips the apex suffix and returns `"@"` when the qname *is* the apex.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/zones/ -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/zones/zone.go internal/zones/zone_test.go
git commit -m "feat(zones): zone cut, relative names, and RRs built by miekg"
```

---

### Task 5: Answering — NODATA, NXDOMAIN, wildcards, referral

**Files:**
- Create: `internal/store/migrations/{sqlite,postgres}/0006_soa_ttl.sql`
- Modify: `internal/store/zones.go` (add `SOATTL` to `Zone`, to every SELECT/INSERT/UPDATE)
- Create: `internal/zones/answer.go`
- Test: `internal/zones/answer_test.go`

**Schema addition — do this first.** RFC 2308 §5 defines a negative answer's TTL
as `min(SOA.MINIMUM, TTL of the SOA record)`. Those are two independent values:
MINIMUM is an rdata field, and the SOA record carries its own header TTL like any
RR. The `zones` table has `soa_minimum` and no `soa_ttl`, so today the two cannot
differ and `min()` is degenerate.

That still emits RFC-legal output, but it removes a control every other DNS server
has, and Milestone C's zone-file export needs the SOA's own TTL to round-trip.
Add it as migration **0006** (0001-0004 are SQL, 5 is the Go data migration):

```sql
-- +goose Up
-- RFC 2308 5: a negative answer's TTL is min(SOA.MINIMUM, the SOA record's own
-- TTL). MINIMUM is rdata; this is the record's header TTL. Without both, the
-- minimum is degenerate and a zone file cannot round-trip its SOA.
ALTER TABLE zones ADD COLUMN soa_ttl INTEGER NOT NULL DEFAULT 900;
```

Postgres: `BIGINT NOT NULL DEFAULT 900`. Then `Zone.SOATTL uint32` with json tag
`soa_ttl`, threaded through `zoneStore`'s SELECT, INSERT and UPDATE column lists,
and `Zone.SOA()` uses it for the RR header instead of `SOAMinimum`.

**Interfaces:**
- Consumes: `Zone`, `Index`, `ToRR`, `RelName` (Task 4).
- Produces: `func (z *Zone) Answer(m *dns.Msg, qname string, qtype uint16) (handled bool)` — fills `m` and reports whether the zone answered. `handled == false` only for a forwarder/stub zone type, which Milestone D uses; primary and internal always handle.

- [ ] **Step 1: Write the failing test**

```go
func newZone(t *testing.T, recs ...store.ZoneRecord) *zones.Zone { /* helper: apex e412.in, SOA minimum 900 */ }

func TestAnswerReturnsRecord(t *testing.T) {
	z := newZone(t, store.ZoneRecord{Name: "bifrost", Type: "A", TTL: 3600, RData: "57.129.69.158", Enabled: true})
	m := reply("bifrost.e412.in.", dns.TypeA)
	z.Answer(m, "bifrost.e412.in", dns.TypeA)
	if m.Rcode != dns.RcodeSuccess || len(m.Answer) != 1 || !m.Authoritative {
		t.Fatalf("got rcode=%d answers=%d aa=%v; want NOERROR/1/true", m.Rcode, len(m.Answer), m.Authoritative)
	}
}

// NODATA: the name exists, the type does not. NOERROR with an empty ANSWER
// and the SOA in AUTHORITY — the SOA is what lets a resolver cache the
// absence instead of re-asking on every lookup.
func TestAnswerNoDataCarriesSOA(t *testing.T) {
	z := newZone(t, store.ZoneRecord{Name: "bifrost", Type: "A", TTL: 3600, RData: "57.129.69.158", Enabled: true})
	m := reply("bifrost.e412.in.", dns.TypeAAAA)
	z.Answer(m, "bifrost.e412.in", dns.TypeAAAA)
	if m.Rcode != dns.RcodeSuccess || len(m.Answer) != 0 {
		t.Fatalf("got rcode=%d answers=%d; want NOERROR with no answers", m.Rcode, len(m.Answer))
	}
	if len(m.Ns) != 1 || m.Ns[0].Header().Rrtype != dns.TypeSOA {
		t.Fatalf("AUTHORITY = %v; want one SOA", m.Ns)
	}
}

// NXDOMAIN, and the reason this milestone exists: before zones this query
// was forwarded upstream, leaking an internal name and letting a public
// record shadow an undefined internal one.
func TestAnswerNXDomainCarriesSOA(t *testing.T) {
	z := newZone(t, store.ZoneRecord{Name: "bifrost", Type: "A", TTL: 3600, RData: "57.129.69.158", Enabled: true})
	m := reply("nothere.e412.in.", dns.TypeA)
	z.Answer(m, "nothere.e412.in", dns.TypeA)
	if m.Rcode != dns.RcodeNameError {
		t.Fatalf("rcode = %d, want NXDOMAIN", m.Rcode)
	}
	if len(m.Ns) != 1 || m.Ns[0].Header().Rrtype != dns.TypeSOA {
		t.Fatalf("AUTHORITY = %v; want one SOA", m.Ns)
	}
}

func TestWildcardSynthesises(t *testing.T) {
	z := newZone(t, store.ZoneRecord{Name: "*.nexus", Type: "A", TTL: 3600, RData: "192.168.160.200", Enabled: true})
	m := reply("git.nexus.e412.in.", dns.TypeA)
	z.Answer(m, "git.nexus.e412.in", dns.TypeA)
	if len(m.Answer) != 1 {
		t.Fatalf("answers = %v, want the synthesised A", m.Answer)
	}
	if m.Answer[0].Header().Name != "git.nexus.e412.in." {
		t.Errorf("synthesised name = %q, want the queried name", m.Answer[0].Header().Name)
	}
}

// RFC 4592 2.2: a wildcard must not answer for a name that exists with
// other types. Getting this wrong makes every NODATA under the wildcard
// silently return the wildcard's address instead.
func TestWildcardDoesNotCoverExistingName(t *testing.T) {
	z := newZone(t,
		store.ZoneRecord{Name: "*", Type: "A", TTL: 3600, RData: "192.168.150.28", Enabled: true},
		store.ZoneRecord{Name: "api", Type: "TXT", TTL: 3600, RData: `"hello"`, Enabled: true},
	)
	m := reply("api.e412.in.", dns.TypeA)
	z.Answer(m, "api.e412.in", dns.TypeA)
	if len(m.Answer) != 0 || m.Rcode != dns.RcodeSuccess {
		t.Fatalf("got rcode=%d answers=%v; want NODATA, not the wildcard", m.Rcode, m.Answer)
	}
}

func TestDisabledRecordIsInvisible(t *testing.T) {
	z := newZone(t, store.ZoneRecord{Name: "bifrost", Type: "A", TTL: 3600, RData: "57.129.69.158", Enabled: false})
	m := reply("bifrost.e412.in.", dns.TypeA)
	z.Answer(m, "bifrost.e412.in", dns.TypeA)
	if m.Rcode != dns.RcodeNameError {
		t.Errorf("rcode = %d, want NXDOMAIN — a disabled record must not exist", m.Rcode)
	}
}

// An NS below the apex is a zone cut: we are not authoritative below it, so
// we refer rather than answer.
func TestDelegationReturnsReferral(t *testing.T) {
	z := newZone(t, store.ZoneRecord{Name: "sub", Type: "NS", TTL: 3600, RData: "ns1.other.test.", Enabled: true})
	m := reply("host.sub.e412.in.", dns.TypeA)
	z.Answer(m, "host.sub.e412.in", dns.TypeA)
	if m.Authoritative {
		t.Error("aa set on a referral")
	}
	if len(m.Ns) != 1 || m.Ns[0].Header().Rrtype != dns.TypeNS {
		t.Fatalf("AUTHORITY = %v; want the NS referral", m.Ns)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/zones/ -run TestAnswer -v`
Expected: FAIL — `Answer` undefined.

- [ ] **Step 3: Implement `Answer`**

Order matters and is the spec's lookup table:
1. Delegation check — walk from the qname up to (not including) the apex looking for an NS record; if found, referral with `Authoritative = false`, NS in `m.Ns`, in-zone glue A/AAAA in `m.Extra`.
2. Exact name present → CNAME first (if present and qtype != CNAME, append and continue in-zone or hand back for the pipeline), else records of qtype → ANSWER; name present but no qtype → NODATA + SOA.
3. Name absent → wildcard search from the closest encloser up; **only if the exact name has no records of any type** (RFC 4592 §2.2). A stored name whose `*` is not leftmost (`a.*.x`) is a literal, never a wildcard (RFC 4592 §2.1.1).

The SOA placed in AUTHORITY for NODATA and NXDOMAIN carries TTL
`min(SOAMinimum, SOA record TTL)` per **RFC 2308 §5**, not `SOAMinimum` alone —
that value is how long every resolver on the network caches the absence, so
overstating it makes a newly-added record invisible for as long as the larger
number. Pin it:

```go
func TestNegativeTTLIsMinOfMinimumAndSOATTL(t *testing.T) {
	// SOA record TTL 300, MINIMUM 900 — the negative TTL must be 300.
	// This test is the reason soa_ttl exists: with one column the two can
	// never differ and the min() is untestable.
	z := newZoneWithSOA(t, /*soaTTL*/ 300, /*minimum*/ 900)
	m := reply("nothere.e412.in.", dns.TypeA)
	z.Answer(m, "nothere.e412.in", dns.TypeA)
	if got := m.Ns[0].Header().Ttl; got != 300 {
		t.Errorf("negative TTL = %d, want 300 (min of SOA TTL and MINIMUM)", got)
	}
}
```
4. Nothing → NXDOMAIN + SOA.

`Enabled == false` records are filtered out of the zone's map at snapshot build time, so "disabled" and "absent" are the same case here and cannot diverge.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/zones/ -v`
Expected: PASS

- [ ] **Step 5: Revert-verify the wildcard rule**

Delete the "exact name has no records of any type" guard from step 3. Run `go test ./internal/zones/ -run TestWildcardDoesNotCoverExistingName`.
Expected: FAIL with `want NODATA, not the wildcard`. Restore the guard.

- [ ] **Step 6: Commit**

```bash
git add internal/zones/answer.go internal/zones/answer_test.go
git commit -m "feat(zones): authoritative answering — NODATA, NXDOMAIN, wildcards, referrals"
```

---

### Task 6: Resolver middleware, pipeline decision, app wiring

**Files:**
- Create: `internal/zones/resolver.go`
- Modify: `internal/dnssrv/pipeline.go:15-21`
- Modify: `internal/app/app.go:85,123,370`
- Delete: `internal/records/` (whole package, after tests pass)
- Test: `internal/zones/resolver_test.go`

**Interfaces:**
- Consumes: `Index`, `Zone.Answer` (Tasks 4-5), `store.ZoneStore` (Task 2).
- Produces: `zones.NewResolver(zs store.ZoneStore) *Resolver` with `Reload(ctx) error` and `Middleware() dnssrv.Middleware` — same shape `records.Resolver` had, so `app.go` swaps one constructor.

- [ ] **Step 1: Write the failing test**

```go
func TestMiddlewarePassesUncoveredNamesThrough(t *testing.T) {
	called := false
	next := dnssrv.HandlerFunc(func(ctx context.Context, req *dnssrv.Request) (*dnssrv.Response, error) {
		called = true
		return &dnssrv.Response{Msg: new(dns.Msg), Decision: dnssrv.DecisionForwarded}, nil
	})
	r := resolverWith(t, /* zone e412.in */)
	_, _ = r.Middleware()(next).ServeDNS(t.Context(), request("example.com.", dns.TypeA))
	if !called {
		t.Error("a name outside every zone must reach the next handler")
	}
}

func TestMiddlewareDoesNotForwardInsideAZone(t *testing.T) {
	called := false
	next := dnssrv.HandlerFunc(func(ctx context.Context, req *dnssrv.Request) (*dnssrv.Response, error) {
		called = true
		return &dnssrv.Response{Msg: new(dns.Msg)}, nil
	})
	r := resolverWith(t, /* zone e412.in with only bifrost */)
	resp, _ := r.Middleware()(next).ServeDNS(t.Context(), request("nothere.e412.in.", dns.TypeA))
	if called {
		t.Fatal("an undefined name inside our zone was forwarded upstream — the leak this milestone exists to close")
	}
	if resp.Msg.Rcode != dns.RcodeNameError {
		t.Errorf("rcode = %d, want NXDOMAIN", resp.Msg.Rcode)
	}
	if resp.Decision != dnssrv.DecisionAuthoritative {
		t.Errorf("decision = %q, want authoritative", resp.Decision)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/zones/ -run TestMiddleware -v`
Expected: FAIL — `NewResolver` and `DecisionAuthoritative` undefined.

- [ ] **Step 3: Implement**

`Resolver` holds `atomic.Pointer[Index]`, same as `records.Resolver` does today. `Reload` calls `zs.Zones()` + `zs.AllRecords()` and builds one `Index`.

**Use `zones.NewZone` to build each snapshot zone — do not write your own.** Task 5
added it precisely so the "disabled records are invisible" rule lives in production
code with a test pinning it, rather than being re-implemented per caller. A second
filtering path here is how the two drift and one of them starts serving disabled
records. `Middleware` finds the zone; nil → `next.ServeDNS`; otherwise `z.Answer(...)` and return `Decision: dnssrv.DecisionAuthoritative`.

In `pipeline.go`: rename `DecisionLocal` → `DecisionAuthoritative` (value `"authoritative"`), and **delete `DecisionAllowed`** — declared since the file was written and never once emitted, so it has only ever been a false option in a switch.

In `app.go`: `resolver: zones.NewResolver(st.Zones())` and `ReloadRecords` → `ReloadZones`, with the `api.Reloader` interface method renamed to match.

- [ ] **Step 4: Run the whole suite**

Run: `go test -race ./... && ~/go/bin/golangci-lint run ./...`
Expected: PASS, 0 issues. The query-log decision string changes, so fix any test asserting `"local"`.

- [ ] **Step 5: Revert-verify the leak test**

In `Middleware`, change the zone-found branch to call `next.ServeDNS` when the answer is NXDOMAIN. Run `go test ./internal/zones/ -run TestMiddlewareDoesNotForwardInsideAZone`.
Expected: FAIL with `the leak this milestone exists to close`. Restore.

- [ ] **Step 6: Delete the old package and commit**

```bash
git rm -r internal/records
go test -race ./...
git add -A
git commit -m "feat(zones): serve zones authoritatively, retiring internal/records"
```

---

### Task 7: Zones API

**Files:**
- Create: `internal/api/zones_handlers.go`
- Modify: `internal/api/server.go` (call `s.zonesRoutes()`; rename `Reloader.ReloadRecords` → `ReloadZones`)
- Test: `internal/api/zones_handlers_test.go`

**Interfaces:**
- Consumes: `store.ZoneStore` (Task 2).
- Produces: `GET/POST /api/v1/zones`, `GET/PATCH/DELETE /api/v1/zones/{id}`.

- [ ] **Step 1: Write the failing test**

```go
func TestZoneCreateDefaultsSOA(t *testing.T) {
	srv := newTestServer(t)
	rec := srv.do(t, "POST", "/api/v1/zones", `{"name":"e412.in"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body)
	}
	var got struct{ ID int64 }
	json.Unmarshal(rec.Body.Bytes(), &got)
	z := srv.zone(t, got.ID)
	// A zone with no SOA cannot answer NXDOMAIN, so the API fills one in
	// rather than accepting a zone that is authoritative in name only.
	if z.SOANS == "" || z.SOAMbox == "" || z.SOAMinimum == 0 || z.SOASerial != 1 {
		t.Errorf("SOA defaults not applied: %+v", z)
	}
	if z.Type != "primary" || !z.Enabled {
		t.Errorf("type/enabled = %q/%v, want primary/true", z.Type, z.Enabled)
	}
}

func TestZoneCreateRejectsBadName(t *testing.T) {
	srv := newTestServer(t)
	for _, name := range []string{"", "not a domain", "e412..in"} {
		if rec := srv.do(t, "POST", "/api/v1/zones", `{"name":"`+name+`"}`); rec.Code != http.StatusBadRequest {
			t.Errorf("POST name=%q status = %d, want 400", name, rec.Code)
		}
	}
}

func TestZoneCreateRejectsDuplicate(t *testing.T) {
	srv := newTestServer(t)
	srv.do(t, "POST", "/api/v1/zones", `{"name":"e412.in"}`)
	if rec := srv.do(t, "POST", "/api/v1/zones", `{"name":"e412.in"}`); rec.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409", rec.Code)
	}
}

func TestZoneDeleteTakesRecords(t *testing.T) { /* create zone + record, DELETE, assert 204 and records gone */ }
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/api/ -run TestZone -v`
Expected: FAIL — route not registered, 404.

- [ ] **Step 3: Implement**

`zoneCreate` mirrors `groupCreate`'s pointer-for-optional pattern:

```go
type zoneCreate struct {
	Name string `json:"name"`
	// Type omitted means primary — the only type Milestone A can serve.
	Type string `json:"type"`
	// Enabled is a pointer so "not sent" differs from "false".
	Enabled *bool `json:"enabled"`
	// SOA fields, all optional: omitted means the generated default, so a
	// zone can be created from a name alone.
	SOANS      string `json:"soa_ns"`
	SOAMbox    string `json:"soa_mbox"`
	SOARefresh uint32 `json:"soa_refresh"`
	SOARetry   uint32 `json:"soa_retry"`
	SOAExpire  uint32 `json:"soa_expire"`
	SOAMinimum uint32 `json:"soa_minimum"`
}
```

**Zone `type` must be validated against a known set, and Milestone A accepts
only `primary`.** Today the field is stored unvalidated, so `{"type":"banana"}`
creates a zone — and worse, `secondary` is not in `Answer`'s
forwarder/stub fall-through list, so it is served like a primary. A secondary
zone has no transfer mechanism until #72, so it holds only its apex NS record
and every other name under it becomes an authoritative NXDOMAIN: creating one
for a domain you own takes that domain offline from inside your network, while
the UI shows a healthy "Enabled" row.

Reject anything but `primary` with 400 (`"only primary zones are supported"`),
and reject unknown strings outright. The UI's create select offers `primary`
alone until #72 lands, at which point both are widened together.

Name validation: lowercase, strip trailing dot, then `dns.IsDomainName` plus a reject on empty labels (this also gives RFC 1035 §2.3.4's 63/255 octet limits).

**Creating a zone must also create its apex NS record** (RFC 2181 §10.1) — a zone
with no NS at the apex is malformed, and every zone-file export and future
transfer would carry the defect outward. Insert one `@ NS <soa_ns>` alongside the
zone:

```go
func TestZoneCreateMakesApexNS(t *testing.T) {
	srv := newTestServer(t)
	rec := srv.do(t, "POST", "/api/v1/zones", `{"name":"e412.in"}`)
	var got struct{ ID int64 }
	json.Unmarshal(rec.Body.Bytes(), &got)
	recs := srv.records(t, got.ID)
	if len(recs) != 1 || recs[0].Name != "@" || recs[0].Type != "NS" {
		t.Fatalf("records = %+v; want one apex NS (RFC 2181 10.1)", recs)
	}
}
``` Defaults: `SOANS = "ns." + name`, `SOAMbox = "hostadmin." + name`, refresh 900 / retry 300 / expire 604800 / minimum 900, **soa_ttl 900**, serial 1.

**`soa_ttl` is not optional and the column default will not save you.** `store.AddZone`
binds `soa_ttl` explicitly, so a zero `SOATTL` is written as 0 rather than falling back
to the schema's `DEFAULT 900`. And the negative-answer TTL is `min(SOAMinimum, SOATTL)`,
so a zero there makes **every NXDOMAIN this zone hands out uncacheable** — each miss
re-queries us forever. Pin it:

```go
func TestZoneCreateDefaultsSOATTLNonZero(t *testing.T) {
	srv := newTestServer(t)
	rec := srv.do(t, "POST", "/api/v1/zones", `{"name":"e412.in"}`)
	var got struct{ ID int64 }
	json.Unmarshal(rec.Body.Bytes(), &got)
	// Zero here silently disables negative caching for the whole zone.
	if z := srv.zone(t, got.ID); z.SOATTL == 0 {
		t.Fatal("SOATTL = 0 — every NXDOMAIN from this zone would be uncacheable")
	}
}
``` Every mutation calls `s.reloadZones(r)` then returns.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/api/ -run TestZone -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/api/zones_handlers.go internal/api/zones_handlers_test.go internal/api/server.go
git commit -m "feat(api): zones CRUD with generated SOA defaults"
```

---

### Task 8: Zone records API

**Files:**
- Create: `internal/api/zonerecords_handlers.go`
- Delete: `internal/api/records_handlers.go` and `records_handlers_test.go`
- Test: `internal/api/zonerecords_handlers_test.go`

**Interfaces:**
- Consumes: `zones.ToRR` (Task 4), `store.ZoneStore` (Task 2).
- Produces: `GET/POST /api/v1/zones/{id}/records`, `PUT/DELETE /api/v1/zones/{id}/records/{rid}`.

- [ ] **Step 1: Write the failing test**

```go
// Validation is dns.NewRR itself, so an accepted record is by construction
// a servable one — there is no second validator to drift from the parser.
func TestRecordCreateRejectsUnparseableRData(t *testing.T) {
	srv, zid := newTestServerWithZone(t, "e412.in")
	body := `{"name":"bifrost","type":"A","ttl":3600,"rdata":"not-an-ip"}`
	rec := srv.do(t, "POST", fmt.Sprintf("/api/v1/zones/%d/records", zid), body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body = %s, want 400", rec.Code, rec.Body)
	}
}

func TestRecordCreateAcceptsMX(t *testing.T) {
	srv, zid := newTestServerWithZone(t, "e412.in")
	body := `{"name":"@","type":"MX","ttl":3600,"rdata":"10 mail.e412.in."}`
	if rec := srv.do(t, "POST", fmt.Sprintf("/api/v1/zones/%d/records", zid), body); rec.Code != http.StatusCreated {
		t.Fatalf("status = %d body = %s, want 201", rec.Code, rec.Body)
	}
}

// RFC 1034 3.6.2. Allowed at write time this produces a zone that answers
// inconsistently depending on which record is found first.
func TestRecordCreateRejectsCNAMEBesideSibling(t *testing.T) {
	srv, zid := newTestServerWithZone(t, "e412.in")
	path := fmt.Sprintf("/api/v1/zones/%d/records", zid)
	srv.do(t, "POST", path, `{"name":"www","type":"A","ttl":300,"rdata":"192.168.1.9"}`)
	rec := srv.do(t, "POST", path, `{"name":"www","type":"CNAME","ttl":300,"rdata":"bifrost.e412.in."}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
	// And the reverse order must be refused too.
	srv2, zid2 := newTestServerWithZone(t, "e412.in")
	path2 := fmt.Sprintf("/api/v1/zones/%d/records", zid2)
	srv2.do(t, "POST", path2, `{"name":"www","type":"CNAME","ttl":300,"rdata":"bifrost.e412.in."}`)
	if rec := srv2.do(t, "POST", path2, `{"name":"www","type":"A","ttl":300,"rdata":"192.168.1.9"}`); rec.Code != http.StatusConflict {
		t.Errorf("reverse order status = %d, want 409", rec.Code)
	}
}

func TestRecordCreateBumpsSerial(t *testing.T) {
	srv, zid := newTestServerWithZone(t, "e412.in")
	before := srv.zone(t, zid).SOASerial
	srv.do(t, "POST", fmt.Sprintf("/api/v1/zones/%d/records", zid), `{"name":"a","type":"A","ttl":300,"rdata":"1.2.3.4"}`)
	if after := srv.zone(t, zid).SOASerial; after != before+1 {
		t.Errorf("serial %d -> %d, want +1", before, after)
	}
}

func TestRecordCreateRejectsUnknownZone(t *testing.T) { /* 404 */ }

// RFC 2181 5.2. Two A records at one name with different TTLs make a zone that
// answers differently depending on which row is read first.
func TestRecordCreateRejectsMismatchedRRSetTTL(t *testing.T) {
	srv, zid := newTestServerWithZone(t, "e412.in")
	path := fmt.Sprintf("/api/v1/zones/%d/records", zid)
	srv.do(t, "POST", path, `{"name":"web","type":"A","ttl":300,"rdata":"192.168.1.1"}`)
	rec := srv.do(t, "POST", path, `{"name":"web","type":"A","ttl":600,"rdata":"192.168.1.2"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 — an RRSet must have one TTL", rec.Code)
	}
	// Same TTL is fine: multiple addresses at one name is normal.
	if rec := srv.do(t, "POST", path, `{"name":"web","type":"A","ttl":300,"rdata":"192.168.1.2"}`); rec.Code != http.StatusCreated {
		t.Errorf("second A with matching TTL: status = %d, want 201", rec.Code)
	}
}

// RFC 2181 8. A TTL with the high bit set is read as ZERO by resolvers, so an
// unclamped value means "never cache" — the opposite of what was typed.
func TestRecordCreateRejectsTTLAboveInt32Max(t *testing.T) {
	srv, zid := newTestServerWithZone(t, "e412.in")
	body := `{"name":"a","type":"A","ttl":2147483648,"rdata":"1.2.3.4"}`
	if rec := srv.do(t, "POST", fmt.Sprintf("/api/v1/zones/%d/records", zid), body); rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for TTL > 2147483647", rec.Code)
	}
}

// RFC 1912 2.4. The sibling check cannot catch this on its own: our SOA lives
// on the zones row, so the apex has no sibling record to conflict with.
func TestRecordCreateRejectsCNAMEAtApex(t *testing.T) {
	srv, zid := newTestServerWithZone(t, "e412.in")
	body := `{"name":"@","type":"CNAME","ttl":300,"rdata":"elsewhere.test."}`
	if rec := srv.do(t, "POST", fmt.Sprintf("/api/v1/zones/%d/records", zid), body); rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 — no CNAME at a zone apex", rec.Code)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/api/ -run TestRecord -v`
Expected: FAIL — route not registered.

- [ ] **Step 3: Implement**

Validate by building the RR: `zones.ToRR(absName, rec)` and return its error text on failure — one validator, shared with the resolver. Then the four RFC checks above, in this order: TTL range (400), apex CNAME (409), CNAME siblings both directions (409), RRSet TTL match (409). Name is normalized to lowercase, `""` and `"@"` both meaning the apex. CNAME-sibling check reads the zone's existing records for that name and 409s in both directions. Every mutation calls `BumpSerial` then `s.reloadZones(r)`.

- [ ] **Step 4: Run tests, delete the old handlers**

Run: `go test ./internal/api/ -v && go test -race ./...`
Expected: PASS

- [ ] **Step 5: Revert-verify each RFC rule separately**

One at a time — remove the check, run the named test, confirm it fails, restore:

| Remove | Test that must fail |
|---|---|
| sibling check | `TestRecordCreateRejectsCNAMEBesideSibling` |
| apex-CNAME check | `TestRecordCreateRejectsCNAMEAtApex` |
| RRSet TTL check | `TestRecordCreateRejectsMismatchedRRSetTTL` |
| TTL range check | `TestRecordCreateRejectsTTLAboveInt32Max` |

A rule whose test still passes without it is not being enforced by the code you
think is enforcing it. Report the four failure outputs.

- [ ] **Step 6: Commit**

```bash
git rm internal/api/records_handlers.go internal/api/records_handlers_test.go
git add internal/api/zonerecords_handlers.go internal/api/zonerecords_handlers_test.go
git commit -m "feat(api): zone records validated by the DNS parser itself"
```

---

### Task 9: OpenAPI

**Files:**
- Modify: `internal/api/openapi.yaml` (remove `/records*`, add `/zones*`)
- Test: `internal/api/openapi_test.go` (already asserts spec-vs-routes; must stay green)

- [ ] **Step 1: Run the existing spec test to watch it fail**

Run: `go test ./internal/api/ -run TestOpenAPI -v`
Expected: FAIL — routes exist with no spec entry, and `/records` is specced but gone.

- [ ] **Step 2: Write the spec entries**

Document both paths with request/response schemas, and state the two defaults that matter in `description`: SOA fields omitted are generated, and `rdata` is presentation format validated by the DNS parser.

- [ ] **Step 3: Run to verify it passes**

Run: `go test ./internal/api/ -run TestOpenAPI -v`
Expected: PASS

- [ ] **Step 4: Commit**

```bash
git add internal/api/openapi.yaml
git commit -m "docs(api): spec the zones endpoints, drop /records"
```

---

### Task 10: Web types and hooks

**Files:**
- Modify: `web/src/api/types.ts` (add `Zone`, `ZoneRecord`). **Keep `LocalRecord`** — `dns.tsx` still imports it until Task 12 deletes that page.
- Create: `web/src/hooks/use-zones.ts`
- Modify: `web/src/test/msw-handlers.ts` (MSW: zones + records)
- Test: covered by Tasks 11-12 page tests

- [ ] **Step 1: Add the types**

```ts
export interface Zone {
  id: number;
  name: string;
  type: "primary" | "secondary" | "stub" | "forwarder" | "internal";
  enabled: boolean;
  soa_ns: string;
  soa_mbox: string;
  soa_serial: number;
  soa_refresh: number;
  soa_retry: number;
  soa_expire: number;
  soa_minimum: number;
  primaries: string;
  modified_at: number;
}

export interface ZoneRecord {
  id: number;
  zone_id: number;
  name: string; // '@' | 'bifrost' | '*' | '*.nexus'
  type: string;
  ttl: number;
  rdata: string;
  enabled: boolean;
  comment: string;
}
```

- [ ] **Step 2: Write the hooks**

`useZones()`, `useZone(id)`, `useCreateZone()`, `useUpdateZone()`, `useDeleteZone()`, `useZoneRecords(zoneId)`, `useCreateZoneRecord()`, `useUpdateZoneRecord()`, `useDeleteZoneRecord()`. Follow `use-clients.ts` exactly for query keys and invalidation. Record mutations must invalidate the **zone** query too, since every record write bumps the serial the list shows.

**Also in this task: the query log's decision values changed.** Task 6 renamed the
pipeline decision `local` → `authoritative` and deleted `allowed`. `web/src/pages/queries.tsx`
and its tests still reference `"local"`, which the server no longer emits — so that
filter now matches nothing and any badge keyed on it is dead. Update both, and check
for other decision strings while you are there (`cached`, `stale`, `forwarded`,
`blocked`, `error` are unchanged).

- [ ] **Step 3: Run typecheck**

Run: `cd web && pnpm typecheck`
Expected: PASS

- [ ] **Step 4: Commit**

```bash
git add web/src/lib/api-types.ts web/src/hooks/use-zones.ts web/src/mocks/handlers.ts
git commit -m "feat(web): zone types, hooks, and MSW handlers"
```

---

### Task 11: Zones list page

**Files:**
- Create: `web/src/pages/zones/list.tsx`
- Modify: `web/src/lib/nav.ts` (`LOCAL_DNS` → `ZONES`, label "Zones", path `/zones`)
- Modify: `web/src/App.tsx` (routes)
- Test: `web/src/pages/zones/list.test.tsx`

**Design — read the artboard, do not improvise from this summary.** Call `DesignSync(method: "get_file", projectId: "ccd34137-8691-4f66-8e84-86be52c4bab2", path: "Dnsaur RNUI Zones.dc.html")`. It is the source of truth for layout, columns, copy and states; the line below is orientation only. Full-bleed, header band with count + New zone, grid of zone rows, inline create row (no dialog), `internal` zones muted and read-only.

- [ ] **Step 1: Write the failing test**

```tsx
test("internal zones cannot be deleted", async () => {
  mockZones([
    zone({ id: 1, name: "e412.in", type: "primary" }),
    zone({ id: 2, name: "localhost", type: "internal" }),
  ]);
  renderWithProviders(<ZonesList />);
  const rows = await screen.findAllByTestId("zone-row");
  // The RFC 6303 zones exist to stop junk queries reaching the roots;
  // offering a delete that the API refuses is a button that lies.
  expect(within(rows[0]).getByRole("button", { name: /delete/i })).toBeInTheDocument();
  expect(within(rows[1]).queryByRole("button", { name: /delete/i })).not.toBeInTheDocument();
});

test("the create row posts the typed name and closes", async () => { /* … */ });
test("shows an empty state with a New zone action when there are none", async () => { /* … */ });
```

- [ ] **Step 2: Run to verify it fails**

Run: `cd web && pnpm vitest run src/pages/zones/list.test.tsx`
Expected: FAIL — module not found.

- [ ] **Step 3: Implement the page and rename the route**

- [ ] **Step 4: Run the suite**

Run: `cd web && pnpm lint && pnpm typecheck && pnpm test`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add web/src/pages/zones web/src/lib/nav.ts web/src/App.tsx
git commit -m "feat(web): zones list, replacing the Local DNS nav entry"
```

---

### Task 12: Zone detail page

**Files:**
- Create: `web/src/pages/zones/detail.tsx`
- Delete: `web/src/pages/dns.tsx`, `web/src/pages/dns.test.tsx`
- Modify: `web/e2e/*.spec.ts` (the Local DNS spec drove an `<h1>` and a side sheet that no longer exist)
- Test: `web/src/pages/zones/detail.test.tsx`

**Design — read the artboard, do not improvise from this summary.** Call `DesignSync(method: "get_file", projectId: "ccd34137-8691-4f66-8e84-86be52c4bab2", path: "Dnsaur RNUI Zone Detail.dc.html")`. It is the source of truth; the line below is orientation only. Header with name/type/status/serial, SOA band, records grid, inline create row, name/type filters, no pagination.

- [ ] **Step 1: Write the failing test**

```tsx
test("the apex row shows @ rather than the zone name", async () => { /* … */ });

test("a parser error from the API lands on the data field", async () => {
  // The API validates with dns.NewRR, so its message is the parser's. It is
  // more precise than anything the form could invent, so it is surfaced
  // verbatim rather than replaced with "invalid value".
  server.use(http.post("*/records", () =>
    HttpResponse.json({ error: "dns: bad MX Mx" }, { status: 400 })));
  /* … assert the message is rendered next to the data input … */
});

test("the placeholder follows the selected type", async () => { /* A → 192.168.1.10, MX → 10 mail.example.com. */ });
```

- [ ] **Step 2: Run to verify it fails**

Run: `cd web && pnpm vitest run src/pages/zones/detail.test.tsx`
Expected: FAIL — module not found.

- [ ] **Step 3: Implement**

- [ ] **Step 4: Run everything including Playwright**

Run: `cd web && pnpm lint && pnpm typecheck && pnpm test && pnpm build && pnpm e2e`
Expected: PASS. Playwright is the step most likely to be skipped and the one that caught the last regression of this kind — run it.

- [ ] **Step 5: Commit**

```bash
git rm web/src/pages/dns.tsx web/src/pages/dns.test.tsx
git add web/src/pages/zones web/e2e
git commit -m "feat(web): zone detail with records, retiring the Local DNS page"
```

---

### Task 13: Docs

**Files:**
- Modify: `docs/dashboard.md` (replace the "Local DNS" section with "Zones")
- Modify: `docs/api.md`, `docs/ui-contract.md` (endpoints, error strings)
- Modify: `README.md` (feature list wording)

- [ ] **Step 1: Rewrite the Local DNS section as Zones**

Cover what a zone is, that records are named relative to the apex, and the NODATA/NXDOMAIN behaviour — specifically that a name inside a zone is never forwarded, and why that matters for a publicly-registered domain used internally.

- [ ] **Step 2: Verify every relative link still resolves**

Run the link check from the docs PR:

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

- [ ] **Step 3: Commit and open the PR**

```bash
git add docs README.md
git commit -m "docs: zones replace local DNS records"
git push -u origin feat/zones
```

PR title: `feat: authoritative zones replace flat local DNS records`. Body must state the behaviour change for existing users — names under a migrated zone now return NXDOMAIN instead of being forwarded — and how the migration grouped their records.

---

## Self-Review

**Spec coverage:** §2 storage → Tasks 1-3. §2 rdata decision → Tasks 4, 8. §2 migration → Task 3. §3 zone cut → Task 4. §3 lookup table (NODATA/NXDOMAIN/wildcard/CNAME/delegation) → Task 5. §3 decision rename → Task 6. Milestone A scope (API, UI, docs) → Tasks 7-13. §5 test posture: all five pinned cases have tasks — NODATA/NXDOMAIN/referral (Task 5), wildcard non-match (Task 5, with revert-verify), CNAME siblings (Task 8, with revert-verify), deepest-zone-wins (Task 4), migration round-trip (Task 3).

**Gap found and closed:** the spec's `internal` zone type is referenced by the Task 11 test but is only *seeded* in Milestone B. Task 7 must therefore accept `type: "internal"` on create and Task 11 must render it read-only, even though nothing creates one until #70. Left as-is deliberately: the type exists in the schema from Task 1, so the UI honouring it now costs nothing and avoids a second pass.

**Type consistency:** `ToRR`/`RelName` exported from Task 4 and used with those names in Tasks 5 and 8. `store.ZoneRecord.RData` (Go) ↔ `rdata` (JSON) ↔ `ZoneRecord.rdata` (TS) consistent across Tasks 2, 8, 10. `Reloader.ReloadZones` renamed in Task 6 and used in Tasks 7-8.

**Placeholder scan:** Tasks 11 and 12 carry `/* … */` inside two test bodies whose full content depends on artboards not yet delivered. Every *assertion that pins behaviour* is written out; the elided parts are render scaffolding. Flagged rather than faked.
