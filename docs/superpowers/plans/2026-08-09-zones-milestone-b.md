# Zones Milestone B Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Reverse DNS — `in-addr.arpa` / `ip6.arpa` zones, PTR records maintained automatically from A/AAAA writes, and the five RFC 6303 built-in zones so loopback lookups stop reaching the root servers.

**Architecture:** Reverse zones are ordinary `primary` zones whose apex happens to end in `.arpa`, so the existing zone cut, answering and API all work unchanged — the new code is address↔name conversion (`internal/zones/reverse.go`) and an auto-PTR side-effect layer in the records handler. The five RFC 6303 zones are seeded by a Go migration as type `internal`, which the API must then refuse to modify.

**Tech Stack:** Go 1.26; `github.com/miekg/dns` (`dns.ReverseAddr` for address→name); `net/netip`; `github.com/pressly/goose/v3`; existing `internal/store` and `internal/zones`; React 19 + TanStack Query v5 + rnui.

## Global Constraints

- Module `github.com/aloks98/dnsaur`; Go 1.26. Branch: `feat/zones-reverse`.
- Before every commit: `go test -race ./...` AND `~/go/bin/golangci-lint run ./...` (0 issues) AND `gofmt -l internal cmd` (empty — golangci-lint does NOT check gofmt, and a slip shipped in Milestone A because of it).
- For web tasks additionally, from `web/`: `pnpm lint && pnpm format:check && pnpm typecheck && pnpm test && pnpm build`. **`format:check` is a CI gate and is the one most often skipped** — Milestone A failed CI twice on it.
- **Verify the committed tree, not the working tree.** After committing, confirm the formatters pass against what was actually staged. A Milestone A fix was formatted locally and never staged.
- Conventional commits. Revert-verify discipline: every new test shown to fail with its fix removed, and the failure output reported.
- Error strings state the problem and stop — **no RFC citations in user-facing messages**; the citation goes in a comment above the check.
- UI copy plain and brief. Timestamps Unix ms. `DisallowUnknownFields` is on for API decode.
- Both SQLite and Postgres migrations written, and tests run under `forEachDriver` so both dialects execute.
- Next free migration number is **0007** (0001–0004 and 0006 are SQL; 5 is the Go data migration in `internal/store/zonemigrate.go`).

## File Structure

```
internal/zones/reverse.go                                    — Task 1 (address ↔ .arpa name)
internal/store/migrations/{sqlite,postgres}/0007_builtins.sql — Task 2 (nothing; see task)
internal/store/builtins.go                                   — Task 2 (Go migration 7: seed the five)
internal/store/migrate.go                                    — Task 2 (register migration 7)
internal/api/zones_handlers.go                               — Task 3 (reject writes to internal zones)
internal/api/zonerecords_handlers.go                         — Task 3 + Task 4
internal/api/autoptr.go                                      — Task 4 (the auto-PTR side effect)
internal/api/openapi.yaml                                    — Task 5
web/src/pages/zones/detail.tsx                               — Task 6 (PTR type, read-only internal)
web/src/pages/zones/list.tsx                                 — Task 6 (long .arpa names)
docs/dashboard.md, docs/architecture.md, docs/api.md          — Task 7
```

---

### Task 1: Address ↔ reverse-name conversion

**Files:**
- Create: `internal/zones/reverse.go`
- Test: `internal/zones/reverse_test.go`

**Interfaces:**
- Produces:
  - `func ReverseName(addr netip.Addr) string` — `192.168.150.10` → `10.150.168.192.in-addr.arpa` (no trailing dot, lowercase, matching how zone and record names are stored everywhere else).
  - `func ReverseZoneFor(addr netip.Addr, zoneNames []string) (zone string, rel string, ok bool)` — given the apexes that exist, pick the **deepest** one covering this address and return the record name relative to it. `192.168.150.10` with `["150.168.192.in-addr.arpa", "168.192.in-addr.arpa"]` → `("150.168.192.in-addr.arpa", "10", true)`.

- [ ] **Step 1: Write the failing test**

```go
package zones_test

import (
	"net/netip"
	"testing"

	"github.com/aloks98/dnsaur/internal/zones"
)

func TestReverseName(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		// RFC 1035 §3.5: the four octets, reversed.
		{"192.168.150.10", "10.150.168.192.in-addr.arpa"},
		{"10.0.0.1", "1.0.0.10.in-addr.arpa"},
		// RFC 3596 §2.5: 32 nibbles, reversed, dot-separated — every nibble
		// present, including the zeros an address literal elides.
		{"::1", "1.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.ip6.arpa"},
		{"fd00::28", "8.2.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.d.f.ip6.arpa"},
	} {
		addr := netip.MustParseAddr(tc.in)
		if got := zones.ReverseName(addr); got != tc.want {
			t.Errorf("ReverseName(%s) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// An IPv4-mapped IPv6 address must reverse as IPv4 — otherwise the same host
// lands in two different zones depending on how its address was parsed.
func TestReverseNameUnmapsV4(t *testing.T) {
	addr := netip.MustParseAddr("::ffff:192.168.150.10")
	if got := zones.ReverseName(addr); got != "10.150.168.192.in-addr.arpa" {
		t.Errorf("ReverseName(v4-mapped) = %q, want the in-addr.arpa form", got)
	}
}

func TestReverseZoneForPicksDeepest(t *testing.T) {
	have := []string{"168.192.in-addr.arpa", "150.168.192.in-addr.arpa", "e412.in"}
	zone, rel, ok := zones.ReverseZoneFor(netip.MustParseAddr("192.168.150.10"), have)
	if !ok || zone != "150.168.192.in-addr.arpa" || rel != "10" {
		t.Fatalf("got (%q, %q, %v); want the /24 zone and rel \"10\"", zone, rel, ok)
	}
}

func TestReverseZoneForFallsBackToShallower(t *testing.T) {
	have := []string{"168.192.in-addr.arpa"}
	zone, rel, ok := zones.ReverseZoneFor(netip.MustParseAddr("192.168.150.10"), have)
	if !ok || zone != "168.192.in-addr.arpa" || rel != "10.150" {
		t.Fatalf("got (%q, %q, %v); want the /16 zone and rel \"10.150\"", zone, rel, ok)
	}
}

// No reverse zone means no PTR. Auto-PTR must never invent a zone, so this
// returning ok=false is what stops it.
func TestReverseZoneForNoMatch(t *testing.T) {
	if _, _, ok := zones.ReverseZoneFor(netip.MustParseAddr("8.8.8.8"), []string{"e412.in"}); ok {
		t.Error("ReverseZoneFor matched a zone that does not cover the address")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/zones/ -run TestReverse -v`
Expected: FAIL — `ReverseName` and `ReverseZoneFor` undefined.

- [ ] **Step 3: Implement**

`ReverseName` wraps `dns.ReverseAddr` (which returns a trailing-dotted name) and strips the dot, after `addr.Unmap()` so a v4-mapped address takes the IPv4 path. `ReverseZoneFor` computes the full reverse name, then finds the longest entry in `zoneNames` that is a suffix of it on a label boundary, returning the remaining prefix as the relative name.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/zones/ -run TestReverse -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/zones/reverse.go internal/zones/reverse_test.go
git commit -m "feat(zones): address to in-addr.arpa/ip6.arpa conversion"
```

---

### Task 2: Seed the five RFC 6303 built-in zones

**Files:**
- Create: `internal/store/builtins.go`
- Modify: `internal/store/migrate.go` (register a Go migration at version 7, beside the existing version-5 registration)
- Test: `internal/store/builtins_test.go`

**Interfaces:**
- Consumes: the `zones` / `zone_records` schema and the `dbtx` interface already in `internal/store/zonemigrate.go`.
- Produces: `var builtinZones = []string{...}` (the five apexes) — read by Task 3's tests.

**Why a Go migration and not SQL:** it inserts into two tables with a generated SOA and an apex NS per zone, exactly as `zonemigrate.go` does, and reusing that file's `dbtx`-based helpers keeps one definition of "what a well-formed new zone looks like". A `0007_builtins.sql` file is therefore **not** created — the version number is claimed by the Go migration.

**The five, and only these five** (spec §7): `localhost`, `127.in-addr.arpa`, `0.in-addr.arpa`, `255.in-addr.arpa`, `1.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.ip6.arpa`. The RFC 1918 reverse ranges are deliberately excluded — seeding them would make PTRs for the user's own LAN impossible.

- [ ] **Step 1: Write the failing test**

```go
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
		for _, name := range builtinZones {
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

// RFC 6303 lists the RFC 1918 reverse ranges too. Seeding them would make a
// PTR for the user's own LAN impossible: an empty authoritative zone
// NXDOMAINs everything under it.
func TestPrivateReverseZonesNotSeeded(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s Store) {
		zs, _ := s.Zones().Zones(context.Background())
		for _, z := range zs {
			for _, banned := range []string{"10.in-addr.arpa", "168.192.in-addr.arpa", "16.172.in-addr.arpa"} {
				if z.Name == banned {
					t.Errorf("seeded %q — this blocks the user's own PTR records", banned)
				}
			}
		}
	})
}

// The migration runs once. A second Open must not duplicate the zones, and
// must not fail on the unique index either.
func TestBuiltinSeedIsIdempotent(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s Store) {
		zs, _ := s.Zones().Zones(context.Background())
		seen := map[string]int{}
		for _, z := range zs {
			seen[z.Name]++
		}
		for _, name := range builtinZones {
			if seen[name] > 1 {
				t.Errorf("%s seeded %d times", name, seen[name])
			}
		}
	})
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/store/ -run 'TestBuiltin|TestPrivateReverse' -v`
Expected: FAIL — `builtinZones` undefined.

- [ ] **Step 3: Implement**

`builtins.go` holds the five names and an `upBuiltinZones(ctx, tx)` that, for each, inserts a zone (`type` `internal`, enabled, `soa_ns` = `ns.<apex>`, `soa_mbox` = `hostadmin.<apex>`, serial 1, the same timer defaults `handleZoneCreate` uses, `soa_ttl` 900) plus one `@ NS` record at TTL 3600 — mirroring `zonemigrate.go`'s apex-NS insert so both creation paths agree. Insert with a "skip if the name already exists" guard so a user who created `localhost` by hand before upgrading is not collided with.

Register in `migrate.go` next to the existing version-5 registration.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/store/ -run 'TestBuiltin|TestPrivateReverse' -v`
Expected: PASS on both sqlite and postgres subtests.

- [ ] **Step 5: Revert-verify the exclusion**

Add `"168.192.in-addr.arpa"` to `builtinZones`, run `go test ./internal/store/ -run TestPrivateReverseZonesNotSeeded`.
Expected: FAIL with `this blocks the user's own PTR records`. Remove it again.

- [ ] **Step 6: Commit**

```bash
git add internal/store/builtins.go internal/store/builtins_test.go internal/store/migrate.go
git commit -m "feat(store): seed the five RFC 6303 built-in zones"
```

---

### Task 3: Internal zones are read-only at the API

**Files:**
- Modify: `internal/api/zones_handlers.go` (`handleZonePatch`, `handleZoneDelete`)
- Modify: `internal/api/zonerecords_handlers.go` (`handleZoneRecordCreate`, `handleZoneRecordUpdate`, `handleZoneRecordDelete`)
- Test: `internal/api/zones_handlers_test.go`, `internal/api/zonerecords_handlers_test.go`

**Interfaces:**
- Consumes: `builtinZones` (Task 2) for test fixtures.
- Produces: nothing new; five handlers gain one guard each.

This was a deferred finding from Milestone A's review. It was harmless while nothing created internal zones. Task 2 creates five, so it stops being harmless now.

- [ ] **Step 1: Write the failing test**

```go
// A built-in zone is infrastructure, not content. The UI already hides these
// controls; without a server guard the API would honour a request the UI
// never offers, and a built-in could be edited or deleted out from under it.
func TestInternalZoneRejectsWrites(t *testing.T) {
	srv := newTestServer(t)
	id := srv.zoneIDByName(t, "localhost")

	if rec := srv.do(t, "PATCH", fmt.Sprintf("/api/v1/zones/%d", id), `{"enabled":false}`); rec.Code != http.StatusConflict {
		t.Errorf("PATCH status = %d, want 409", rec.Code)
	}
	if rec := srv.do(t, "DELETE", fmt.Sprintf("/api/v1/zones/%d", id), ""); rec.Code != http.StatusConflict {
		t.Errorf("DELETE status = %d, want 409", rec.Code)
	}
	body := `{"name":"x","type":"A","ttl":300,"rdata":"1.2.3.4"}`
	if rec := srv.do(t, "POST", fmt.Sprintf("/api/v1/zones/%d/records", id), body); rec.Code != http.StatusConflict {
		t.Errorf("record POST status = %d, want 409", rec.Code)
	}
}

// Reading is fine — the UI lists them and shows their records.
func TestInternalZoneAllowsReads(t *testing.T) {
	srv := newTestServer(t)
	id := srv.zoneIDByName(t, "localhost")
	if rec := srv.do(t, "GET", fmt.Sprintf("/api/v1/zones/%d", id), ""); rec.Code != http.StatusOK {
		t.Errorf("GET zone status = %d, want 200", rec.Code)
	}
	if rec := srv.do(t, "GET", fmt.Sprintf("/api/v1/zones/%d/records", id), ""); rec.Code != http.StatusOK {
		t.Errorf("GET records status = %d, want 200", rec.Code)
	}
}
```

Add a `zoneIDByName(t, name)` helper to the existing `zoneTestServer` beside its `zone`/`records` helpers.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/api/ -run TestInternalZone -v`
Expected: FAIL — PATCH/DELETE/POST all succeed.

- [ ] **Step 3: Implement**

Each of the five handlers loads its zone already; add, after that load:

```go
// A built-in zone is seeded infrastructure (RFC 6303), not user content —
// see internal/store/builtins.go. Reads are fine; writes are not.
if z.Type == "internal" {
	errJSON(w, http.StatusConflict, "built-in zones cannot be changed")
	return
}
```

Note the message states the problem with no RFC citation, per the global constraints.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/api/ -run TestInternalZone -v`
Expected: PASS

- [ ] **Step 5: Revert-verify**

Remove the guard from `handleZoneDelete` only, run `go test ./internal/api/ -run TestInternalZoneRejectsWrites`.
Expected: FAIL on `DELETE status = 204, want 409`. Restore.

- [ ] **Step 6: Commit**

```bash
git add internal/api/zones_handlers.go internal/api/zonerecords_handlers.go internal/api/zones_handlers_test.go internal/api/zonerecords_handlers_test.go
git commit -m "feat(api): refuse writes to built-in zones"
```

---

### Task 4: Auto-PTR

**Files:**
- Create: `internal/api/autoptr.go`
- Modify: `internal/api/zonerecords_handlers.go` (call the side effect from create/update/delete)
- Test: `internal/api/autoptr_test.go`

**Interfaces:**
- Consumes: `zones.ReverseZoneFor` (Task 1), `store.ZoneStore`.
- Produces: `func (s *Server) syncPTR(ctx context.Context, old, new *store.ZoneRecord, zoneName string)` — `old` nil on create, `new` nil on delete. Never returns an error: the forward write already committed, so a PTR failure is logged, not surfaced.

**The rules (spec §7), all five pinned by tests:**

| Event | Behaviour |
|---|---|
| A/AAAA created, matching reverse zone exists | write the PTR |
| no matching reverse zone | do nothing — never auto-create a zone |
| a PTR already exists for that address | leave it, WARN. First wins. |
| A/AAAA updated | move the PTR, only if it still points at this name |
| A/AAAA deleted | remove the PTR, only if it still points at this name |

The "still points at this name" guard is what stops an automatic write clobbering a PTR the user set by hand.

- [ ] **Step 1: Write the failing test**

```go
func TestAutoPTRCreatesRecord(t *testing.T) {
	srv := newTestServer(t)
	fwd := srv.createZone(t, "e412.in")
	rev := srv.createZone(t, "150.168.192.in-addr.arpa")

	srv.do(t, "POST", fmt.Sprintf("/api/v1/zones/%d/records", fwd),
		`{"name":"bifrost","type":"A","ttl":300,"rdata":"192.168.150.10"}`)

	ptr := srv.recordsByType(t, rev, "PTR")
	if len(ptr) != 1 || ptr[0].Name != "10" || ptr[0].RData != "bifrost.e412.in." {
		t.Fatalf("PTR = %+v; want one at \"10\" pointing to bifrost.e412.in.", ptr)
	}
}

func TestAutoPTRSkippedWithoutReverseZone(t *testing.T) {
	srv := newTestServer(t)
	fwd := srv.createZone(t, "e412.in")
	rec := srv.do(t, "POST", fmt.Sprintf("/api/v1/zones/%d/records", fwd),
		`{"name":"bifrost","type":"A","ttl":300,"rdata":"192.168.150.10"}`)
	// The forward write must still succeed — a missing reverse zone is not
	// an error, and auto-PTR must never create one.
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", rec.Code)
	}
	for _, z := range srv.allZones(t) {
		if strings.HasSuffix(z.Name, ".arpa") {
			t.Fatalf("auto-PTR created zone %q", z.Name)
		}
	}
}

// First wins: a second name at the same address must not steal the reverse.
func TestAutoPTRDoesNotOverwriteExisting(t *testing.T) {
	srv := newTestServer(t)
	fwd := srv.createZone(t, "e412.in")
	rev := srv.createZone(t, "150.168.192.in-addr.arpa")
	path := fmt.Sprintf("/api/v1/zones/%d/records", fwd)

	srv.do(t, "POST", path, `{"name":"bifrost","type":"A","ttl":300,"rdata":"192.168.150.10"}`)
	srv.do(t, "POST", path, `{"name":"nas","type":"A","ttl":300,"rdata":"192.168.150.10"}`)

	ptr := srv.recordsByType(t, rev, "PTR")
	if len(ptr) != 1 || ptr[0].RData != "bifrost.e412.in." {
		t.Fatalf("PTR = %+v; want the first writer to keep it", ptr)
	}
}

func TestAutoPTRMovesOnUpdate(t *testing.T) {
	srv := newTestServer(t)
	fwd := srv.createZone(t, "e412.in")
	rev := srv.createZone(t, "150.168.192.in-addr.arpa")
	path := fmt.Sprintf("/api/v1/zones/%d/records", fwd)

	rid := srv.createRecord(t, fwd, `{"name":"bifrost","type":"A","ttl":300,"rdata":"192.168.150.10"}`)
	srv.do(t, "PUT", fmt.Sprintf("%s/%d", path, rid),
		`{"name":"bifrost","type":"A","ttl":300,"rdata":"192.168.150.11"}`)

	ptr := srv.recordsByType(t, rev, "PTR")
	if len(ptr) != 1 || ptr[0].Name != "11" {
		t.Fatalf("PTR = %+v; want it moved to \"11\"", ptr)
	}
}

func TestAutoPTRRemovedOnDelete(t *testing.T) {
	srv := newTestServer(t)
	fwd := srv.createZone(t, "e412.in")
	rev := srv.createZone(t, "150.168.192.in-addr.arpa")
	rid := srv.createRecord(t, fwd, `{"name":"bifrost","type":"A","ttl":300,"rdata":"192.168.150.10"}`)

	srv.do(t, "DELETE", fmt.Sprintf("/api/v1/zones/%d/records/%d", fwd, rid), "")
	if ptr := srv.recordsByType(t, rev, "PTR"); len(ptr) != 0 {
		t.Fatalf("PTR = %+v; want it removed with the A record", ptr)
	}
}

// A PTR the user wrote by hand must survive the A record being deleted.
func TestAutoPTRLeavesForeignPTRAlone(t *testing.T) {
	srv := newTestServer(t)
	fwd := srv.createZone(t, "e412.in")
	rev := srv.createZone(t, "150.168.192.in-addr.arpa")
	srv.createRecord(t, rev, `{"name":"10","type":"PTR","ttl":300,"rdata":"handmade.e412.in."}`)
	rid := srv.createRecord(t, fwd, `{"name":"bifrost","type":"A","ttl":300,"rdata":"192.168.150.10"}`)

	srv.do(t, "DELETE", fmt.Sprintf("/api/v1/zones/%d/records/%d", fwd, rid), "")
	ptr := srv.recordsByType(t, rev, "PTR")
	if len(ptr) != 1 || ptr[0].RData != "handmade.e412.in." {
		t.Fatalf("PTR = %+v; want the hand-written one untouched", ptr)
	}
}
```

Add `createZone`, `createRecord`, `recordsByType` and `allZones` helpers to `zoneTestServer` alongside the existing ones.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/api/ -run TestAutoPTR -v`
Expected: FAIL — no PTR is ever written.

- [ ] **Step 3: Implement**

`syncPTR` runs after the forward write commits and after `BumpSerial`. It parses the rdata as an address (non-A/AAAA types return immediately), asks `zones.ReverseZoneFor` against the current zone list, and on a hit reads that zone's records to apply the table above. Every branch that declines to write logs at WARN with the address and the reason. It bumps the reverse zone's serial when it does write, and calls `reloadZones` once at the end.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/api/ -run TestAutoPTR -v`
Expected: PASS

- [ ] **Step 5: Revert-verify the two guards**

| Remove | Test that must fail |
|---|---|
| the "PTR already exists" check | `TestAutoPTRDoesNotOverwriteExisting` |
| the "still points at this name" check | `TestAutoPTRLeavesForeignPTRAlone` |

One at a time; restore after each. Report both failure outputs.

- [ ] **Step 6: Commit**

```bash
git add internal/api/autoptr.go internal/api/autoptr_test.go internal/api/zonerecords_handlers.go
git commit -m "feat(api): maintain PTR records from A and AAAA writes"
```

---

### Task 5: OpenAPI

**Files:**
- Modify: `internal/api/openapi.yaml`
- Test: `internal/api/openapi_test.go` (must stay green)

- [ ] **Step 1: Document the new behaviour**

Three things a client cannot guess: `PTR` joins the record types; a write to a zone of type `internal` returns **409** with `built-in zones cannot be changed`; and an A/AAAA write may also create, move or delete a PTR in another zone, which is why that zone's serial can change without a direct write to it. Add `409` to the record write operations that did not already list it.

- [ ] **Step 2: Verify**

Run: `go test ./internal/api/ -run TestOpenAPI -v`
Expected: PASS

- [ ] **Step 3: Commit**

```bash
git add internal/api/openapi.yaml
git commit -m "docs(api): spec PTR, built-in zones, and the auto-PTR side effect"
```

---

### Task 6: Web — PTR type and read-only built-ins

**Files:**
- Modify: `web/src/pages/zones/detail.tsx`
- Modify: `web/src/pages/zones/list.tsx`
- Test: `web/src/pages/zones/detail.test.tsx`, `list.test.tsx`

**Design:** no new artboard. `Dnsaur RNUI Zones.dc.html` already specifies the read-only treatment (padlock, muted non-link name, `BUILT-IN` in the Actions cell) and the list already implements it — Task 2 is simply the first thing that produces such a zone. This task extends the same treatment to the detail page, which has no read-only state yet.

- [ ] **Step 1: Write the failing test**

```tsx
test("PTR is offered as a record type and its placeholder is a name", async () => {
  const user = userEvent.setup();
  renderZoneDetail({ zone: zone({ id: 1, name: "150.168.192.in-addr.arpa" }) });
  const row = await screen.findByTestId("record-form-row");
  await user.selectOptions(within(row).getByLabelText(/type/i), "PTR");
  // PTR rdata is a domain name, not an address (RFC 1034) — a placeholder
  // showing an IP would teach exactly the wrong thing.
  expect(within(row).getByLabelText(/data/i)).toHaveAttribute("placeholder", "bifrost.e412.in.");
});

test("a built-in zone offers no way to change it", async () => {
  renderZoneDetail({ zone: zone({ id: 1, name: "localhost", type: "internal" }) });
  await screen.findByText("localhost");
  expect(screen.queryByRole("button", { name: /add record/i })).not.toBeInTheDocument();
  expect(screen.queryByRole("button", { name: /delete zone/i })).not.toBeInTheDocument();
  expect(screen.queryByRole("button", { name: /disable zone/i })).not.toBeInTheDocument();
  // Its records are still listed — reading is the point of showing it at all.
  expect(await screen.findByText("Built-in")).toBeInTheDocument();
});

test("a long .arpa apex does not break the zones-list grid", async () => {
  mockZones([zone({ id: 1, name: "1.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.ip6.arpa", type: "internal" })]);
  renderWithProviders(<ZonesList />);
  const cell = await screen.findByTitle(/ip6\.arpa$/);
  expect(cell).toHaveClass("truncate");
});
```

- [ ] **Step 2: Run to verify it fails**

Run: `cd web && pnpm vitest run src/pages/zones`
Expected: FAIL — PTR absent from the type select; built-in controls still rendered.

- [ ] **Step 3: Implement**

Add `PTR` to `RECORD_TYPES` and to `DATA_PLACEHOLDER` (`bifrost.e412.in.`) and `RECORD_TYPE_VARIANT` (`secondary`). Gate the header's three action buttons and the create/edit row on `zone.type !== "internal"`, and show the same `Built-in` marker the list uses. Confirm the list's Name cell truncates rather than wrapping.

- [ ] **Step 4: Run the suite**

Run: `cd web && pnpm lint && pnpm format:check && pnpm typecheck && pnpm test && pnpm build && pnpm test:e2e`
Expected: all clean.

- [ ] **Step 5: Commit**

```bash
git add web/src/pages/zones
git commit -m "feat(web): PTR records, and built-in zones read-only on the detail page"
```

---

### Task 7: Docs

**Files:**
- Modify: `docs/dashboard.md` (the Zones section), `docs/architecture.md`, `docs/api.md`

- [ ] **Step 1: Write it**

Cover: what the five built-in zones are and why they exist (queries about loopback would otherwise reach the root servers); why the RFC 1918 reverse ranges are **not** among them, and that the user should create `168.192.in-addr.arpa` (or their own range) as a primary zone to get PTRs for their LAN; that adding an A or AAAA record writes the matching PTR automatically when such a zone exists, including the first-wins rule and that a hand-written PTR is never overwritten.

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
git commit -m "docs: reverse zones, PTR, and the built-in zones"
```

---

## Self-Review

**Spec coverage:** §7 "which zones ship" → Task 2 (with the exclusion revert-verified). §7 auto-PTR table, all five rows → Task 4, one test each. §7 "internal read-only at the API" → Task 3. §7 RFC table: 1035 §3.5 and 3596 §2.5 → Task 1; 6303 → Task 2; 1034 (PTR rdata is a name) → Task 6's placeholder test; 2317 explicitly not implemented, and no task claims it.

**Gap found and closed:** the spec says reverse zones are ordinary primary zones, so nothing in the plan teaches the *user* how to create one — added to Task 7, since a feature nobody can find is not shipped.

**Type consistency:** `ReverseName` / `ReverseZoneFor` defined in Task 1 and used with those names in Task 4. `builtinZones` defined in Task 2, consumed by Task 3's fixtures. `syncPTR`'s `(old, new *store.ZoneRecord)` signature is used consistently in Task 4's three call sites.

**Placeholder scan:** none. Every test body is complete; the helpers Tasks 3, 4 and 6 add to the existing harness are named and their purpose stated.
