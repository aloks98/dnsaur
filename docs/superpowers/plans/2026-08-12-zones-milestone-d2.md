# Zones Milestone D2 — outbound AXFR Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** dnsaur as a secondary — it pulls a zone from another server over AXFR, keeps it fresh on the SOA's schedule, and stops answering when the zone expires.

**Architecture:** `dns.Transfer.In` does the wire framing. A refresher goroutine in the shape of `filter.Refresher` drives the schedule off each secondary zone's SOA. A received zone installs through `ZoneStore.ReplaceRecords` — the same single transaction zone-file import uses — with every received RR validated by `buildZoneRecord`, the same validator the REST API enforces.

**Tech Stack:** Go 1.26, `miekg/dns` v1.1.72, sqlite + Postgres, goose migrations.

**Spec:** §9.4 of `docs/superpowers/specs/2026-08-08-zones-design.md`.

## Global Constraints

- **The next free migration number is `0009`.** Do not derive this from the directory listing — `internal/store/migrate.go` claims version 7 for a Go migration with no SQL file, and a duplicate version fails goose at startup on both drivers. Write both `sqlite/` and `postgres/` variants.
- Store tests run under `forEachDriver`; sqlite and a real Postgres container both execute.
- New routes via `s.route(...)`, documented in `internal/api/openapi.yaml` in the same commit. `TestEveryRouteEnforcesAuth` and `TestOpenAPIDocumentsAuthStatuses` pick up new routes automatically and require `requireAuth` plus documented `401`/`403`.
- Gates on the **committed** tree, `git status --porcelain` empty: `go test -race ./...`, `~/go/bin/golangci-lint run ./...`, `gofmt -l internal cmd`, and for web tasks `cd web && pnpm lint && pnpm format:check && pnpm typecheck && pnpm test && pnpm build && pnpm test:e2e`.
- Every fix's test shown failing with the fix removed.
- **Automatic write paths use `buildZoneRecord`.** Milestones B and C each shipped a defect from an automatic path skipping a rule the human path enforced — three times in B alone. A transfer is the third automatic path and gets the same treatment, not its own copy of the rules.
- **Time is injected, never read directly.** `filter.Refresher` carries `now func() time.Time`; follow it. A scheduler that calls `time.Now()` internally cannot be tested without sleeping.

## File Structure

- `internal/store/migrations/{sqlite,postgres}/0009_zone_tsig_fk.sql` — FK and index for `zones.tsig_key_id`
- `internal/zones/primaries.go` — the `primaries` wire format, parse and format
- `internal/zones/transfer.go` — the AXFR client: fetch, convert, install
- `internal/zones/refresh.go` — the scheduler
- `internal/api/zones_handlers.go` — lift the primary-only restriction; validate `primaries` and `tsig_key_id`
- `internal/api/tsigkeys_handlers.go` — the in-use delete guard
- `web/src/pages/zones/` — secondary zone creation, `USED BY`, the in-use guard
- `docs/` — the format, the schedule, the expiry rule

---

### Task 1: `primaries`, secondary zones, and the key reference

**Files:**
- Create: `internal/zones/primaries.go`, `internal/store/migrations/{sqlite,postgres}/0009_zone_tsig_fk.sql`
- Modify: `internal/api/zones_handlers.go`, `internal/api/tsigkeys_handlers.go`, `internal/api/openapi.yaml`
- Test: `internal/zones/primaries_test.go`, `internal/api/zones_handlers_test.go`, `internal/api/tsigkeys_handlers_test.go`

**Interfaces:**
- Produces: `zones.ParsePrimaries(string) ([]netip.AddrPort, error)` and `zones.FormatPrimaries([]netip.AddrPort) string`.

**The format, which shipped with none.** `primaries` is a comma-separated list of `host[:port]`, port defaulting to 53. It is stored as written and resolved at transfer time, not at write — a primary named by hostname must survive its address changing.

**What lifts here:** `zones_handlers.go:139` currently answers `"only primary zones are supported"` for any other type. Secondary becomes creatable, with `primaries` required and non-empty for it, and `tsig_key_id` optional but validated to exist when set.

**What the FK is for.** `zones.tsig_key_id` has been an inert column since Milestone A. D1's review found it belonged to no milestone; D2 owns it. Deleting a key a zone references currently succeeds silently, which would leave a secondary unable to authenticate and no signal as to why.

- [ ] **Step 1: Write the failing tests**

```go
func TestParsePrimaries(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    []string
		wantErr bool
	}{
		{"192.168.150.5", []string{"192.168.150.5:53"}, false},
		{"192.168.150.5:5353", []string{"192.168.150.5:5353"}, false},
		{"192.168.150.5, 192.168.150.6:5353", []string{"192.168.150.5:53", "192.168.150.6:5353"}, false},
		// A secondary with no primary can never transfer, so an empty list is
		// a configuration error rather than a zone that quietly never updates.
		{"", nil, true},
		{"not a host", nil, true},
	} {
		got, err := zones.ParsePrimaries(tc.in)
		if (err != nil) != tc.wantErr {
			t.Fatalf("ParsePrimaries(%q) err = %v, wantErr = %v", tc.in, err, tc.wantErr)
		}
		// ... compare got against tc.want ...
	}
}

// A secondary is defined by where it pulls from; without that it is an empty
// zone that answers authoritatively for nothing.
func TestSecondaryZoneRequiresPrimaries(t *testing.T) {
	srv := newTestServer(t)
	rec := srv.do(t, "POST", "/api/v1/zones", `{"name":"e412.in","type":"secondary"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body = %s; want 400", rec.Code, rec.Body)
	}
}

func TestSecondaryZoneRejectsAnUnknownTSIGKey(t *testing.T) {
	srv := newTestServer(t)
	rec := srv.do(t, "POST", "/api/v1/zones",
		`{"name":"e412.in","type":"secondary","primaries":"192.168.150.5","tsig_key_id":9999}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400 — a key that does not exist cannot sign", rec.Code)
	}
}

// The guard D1 deferred: a key a zone depends on must not vanish silently.
func TestDeletingATSIGKeyInUseIsRefused(t *testing.T) {
	// ... create a key, create a secondary zone referencing it ...
	rec := srv.do(t, "DELETE", fmt.Sprintf("/api/v1/tsig-keys/%d", keyID), "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d; want 409 — deleting it would leave the zone unable to authenticate", rec.Code)
	}
}
```

- [ ] **Step 2: Run and watch them fail**

Run: `go test ./internal/zones/ ./internal/api/ -run 'Primaries|Secondary|TSIGKeyInUse' -v`
Expected: FAIL — `ParsePrimaries` undefined; the zone POST answers "only primary zones are supported"; the DELETE returns 204.

- [ ] **Step 3: Implement**

Parse with `net.SplitHostPort`, falling back to the bare host on `AddrErr`. Keep the stored string; do not normalise it into resolved addresses.

- [ ] **Step 4: Run, then commit**

```bash
git add internal/zones internal/api internal/store
git commit -m "feat(zones): secondary zones, the primaries format, and the TSIG key reference"
```

---

### Task 2: The AXFR client

**Files:**
- Create: `internal/zones/transfer.go`
- Test: `internal/zones/transfer_test.go`

**Interfaces:**
- Consumes: `zones.ParsePrimaries` (Task 1), `store.ZoneStore.ReplaceRecords`, `api.buildZoneRecord`'s validation.
- Produces: `Transfer(ctx, z store.Zone) (result, error)` — fetches from the zone's primaries in order and installs on the first success.

**Three things this must get right:**

1. **Install atomically.** `ReplaceRecords` (`store/zones.go:248`) applies deletes, updates, adds and the zone row — serial included — in one transaction. Its own doc comment already names transfers as its second caller. A partially-installed zone is the failure mode this avoids.
2. **Validate like a hand write.** Convert received RRs to relative-name rows and run them through the same validation `POST /records` uses. A primary sending something dnsaur would refuse from a human must be refused here too, and the refusal must name what and why.
3. **Try primaries in order.** A list exists so one being down is survivable. Record which one answered.

**TSIG:** when the zone has a `tsig_key_id`, sign the request. `dns.Transfer` carries `TsigProvider`; D1's provider is already store-backed.

- [ ] **Step 1: Write the failing test**

Stand up a real `dns.Server` in the test serving a small zone over AXFR — miekg can serve one with `dns.Transfer.Out` — and transfer from it.

```go
func TestTransferInstallsTheZone(t *testing.T) {
	primary := startTestPrimary(t, "e412.in.", []dns.RR{ /* SOA, NS, a couple of A */ })
	// ... a secondary zone row pointing at primary.Addr() ...
	if _, err := zones.Transfer(ctx, z); err != nil {
		t.Fatalf("transfer: %v", err)
	}
	// The records are queryable from the resolver, and the serial is the
	// primary's — a transfer adopts the primary's SOA rather than bumping.
}

// A primary that sends what the API would reject must not get a back door.
func TestTransferRejectsARecordTheAPIWouldRefuse(t *testing.T) { /* CNAME beside an A */ }

func TestTransferFallsBackToTheSecondPrimary(t *testing.T) { /* first refuses connections */ }
```

- [ ] **Steps 2-5:** run red, implement, run green, commit as `feat(zones): pull a zone from its primary over AXFR`.

---

### Task 3: The refresh schedule

**Files:**
- Create: `internal/zones/refresh.go`
- Modify: `internal/app/app.go` (start it)
- Test: `internal/zones/refresh_test.go`

**Follow `filter.Refresher`** (`internal/filter/refresh.go`): a struct holding its store dependencies, an injectable `now func() time.Time`, and a mutex serialising runs. `app.go:144` shows the ticker shape.

**The schedule is the SOA's, per zone:** re-transfer every `refresh` seconds; on failure retry every `retry`; if nothing has succeeded for `expire` seconds, the zone is expired. `refreshed_at` and `expires_at` are the columns for this and have been unused since Milestone A.

**Do not sleep in tests.** Drive `now` forward and assert what the scheduler decides.

- [ ] Steps as above; commit as `feat(zones): keep secondary zones fresh on the SOA schedule`.

---

### Task 4: Expiry stops answering

**Files:**
- Modify: `internal/zones/answer.go`, `internal/zones/resolver.go`
- Test: `internal/zones/answer_test.go`

**RFC 1034 §4.3.5.** A secondary past its SOA expire must stop answering for the zone — it can no longer confirm the data is current. Serving stale records is worse than serving none, because the resolver asking has no way to know.

The zone does not vanish from the UI; it stops answering. What it returns instead (SERVFAIL rather than an authoritative NXDOMAIN, which would assert the name does not exist) is the decision this task must make and pin.

- [ ] Steps as above; commit as `fix(zones): stop answering for an expired secondary`.

---

### Task 5: Per-zone reload, decided by measurement

**Files:**
- Modify: `internal/zones/resolver.go`
- Test: `internal/zones/resolver_test.go`, plus a benchmark

`Resolver.Reload` (`resolver.go:34`) rebuilds **every** zone from the whole store and swaps one atomic `Index`. A human writes a record occasionally; a fleet of secondaries re-transfers constantly.

**Benchmark first, then decide.** Measure `Reload` against a realistic store — say 20 zones of 500 records — and report the number. If it is cheap, say so and keep the whole-store rebuild, with the benchmark committed so the decision is revisitable. If it is not, add a per-zone path.

Do not implement a per-zone reload on the assumption it is needed. Either outcome is acceptable; an unmeasured guess is not.

---

### Task 6: The 512-byte TSIG overshoot

**Files:**
- Modify: `internal/dnssrv/server.go`
- Test: `internal/dnssrv/tsig_test.go`

Carried from D1's review as must-not-survive-D2. A signed reply to a **non-EDNS** UDP client can exceed 512 bytes with **no TC bit** — reproduced at 564 against an unsigned 479 — so the client gets an over-limit packet and no signal to retry over TCP.

Narrow while nothing required TSIG. D2 makes it load-bearing.

The TSIG stub's bytes must be reserved from the UDP budget *before* truncation, because `Msg.Truncate` silently no-ops on any message already carrying a TSIG.

---

### Task 7: Web — secondary zones and the key's dependants

**Files:**
- Modify: `web/src/pages/zones/list.tsx`, `web/src/pages/zones/detail.tsx`, `web/src/pages/tsig-keys.tsx`

**Needs a design artboard before it starts.** Three things change: creating a zone offers `secondary` (the type select is currently restricted), a secondary needs `primaries` and an optional TSIG key, and the zone detail shows transfer state — last refreshed, next refresh, expiry, and the last error if the most recent attempt failed.

Also lands the two pieces cut from the TSIG keys screen, which become buildable once a zone can reference a key: the `USED BY` column and the in-use delete guard, whose 409 Task 1 adds.

---

### Task 8: Docs

**Files:** `docs/api.md`, `docs/dashboard.md`

The `primaries` format; what a secondary does on the SOA's schedule; that an expired secondary stops answering and why; the 409 on deleting a key in use.

---

## Self-Review

**Spec coverage (§9.4):** `Transfer.In` framing → Task 2. Scheduling and expiry → Tasks 3-4. Atomic install via `ReplaceRecords` → Task 2. `buildZoneRecord` validation → Task 2. Per-zone reload → Task 5, explicitly as a measurement. `primaries` format → Task 1. `tsig_key_id` ownership → Task 1.

**Carried in from D1's review:** the 512-byte overshoot (Task 6) and the `USED BY` / in-use guard cut from the TSIG screen (Tasks 1 and 7).

**Ordering risk:** Task 7 depends on a design that does not exist yet. It is last for that reason, and the design prompt goes out when this plan does.

**Known risk:** Task 2 is the first code in this project to act as a DNS *client* against another server. Its test needs a real primary to transfer from — miekg can serve one — and a test that mocks the transfer instead would prove nothing about the wire.
