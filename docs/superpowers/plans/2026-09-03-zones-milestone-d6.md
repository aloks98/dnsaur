# Zones Milestone D6 — `forwarder` and `stub` zones Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A zone can claim a suffix and send its queries somewhere else — to upstreams the operator types (`forwarder`), or to the zone's own nameservers fetched from a master (`stub`).

**Architecture:** Both types answer one question — *given this suffix, which addresses do its queries go to?* — and differ only in where the addresses come from. `Zone.Answer` keeps returning `false` for both, so the resolver middleware keeps falling through and the query reaches the cache before the forwarder. A zone reload pushes a `suffix → addresses` map into the forwarder through a new `SetConditional`, and the forwarder's existing longest-suffix routing does the rest.

**Tech Stack:** Go 1.26, `miekg/dns` v1.1.72, sqlite + Postgres, goose migrations, React + TanStack Query.

**Spec:** §9.11 of `docs/superpowers/specs/2026-08-08-zones-design.md`. Read it first — it records *why* each rule below is the rule, and §9.11.1 explains why the "Conditional migration" §9.7 promised does not exist.

## Global Constraints

- **The next free migration number is `0013`.** Do not derive this from the directory listing: versions 5 and 7 are Go migrations claimed in `internal/store/migrate.go` (`zonemigrate.go`, `builtins.go`) with no SQL file, and a duplicate version fails goose at startup on both drivers. Write both `sqlite/` and `postgres/` variants.
- **Refactor-first on the shared parser.** Task 1 extracts a function two shipped formats already implement. `primaries_test.go` and `notifyto_test.go` must pass **unchanged** — not adjusted, not re-baselined. A behaviour change to either is a defect, and the error strings are asserted on by substring in both suites.
- `internal/zones` tests are **external** (`package zones_test`); `internal/store` tests are **internal** (`package store`); `internal/upstream` tests are **internal** (`package upstream`).
- **The Postgres container is shared across store tests, so fixed literal names collide.** A test inserting a zone called `example.com` passes on sqlite (fresh temp file per test) and fails on Postgres once a second test uses the same literal. Use `testGroupName(base)` (`internal/store/crud_test.go:13`).
- **The TSIG key store's create method is `Create`, not `Add`** (`internal/store/tsigkeys.go:30`). Zone creation is `AddZone`.
- **The `internal/api` test harness** (`internal/api/zones_handlers_test.go`): `newTestServer(t) *zoneTestServer` — **one** return value; `ts.do(t, method, path, body string)` — body is a **JSON string**; `createdID(t, rec) int64` — POST answers `{"id": N}`; `ts.zone(t, id) store.Zone` — PATCH answers **204 with no body**, so this is how you check a patch landed. There is no `decodeJSON`.
- `staticcheck ST1008`: `error` is the last return value.
- Gates on the **committed** tree, `git status --porcelain` empty: `go test -race ./...`, `~/go/bin/golangci-lint run ./...`, `gofmt -l internal cmd`, and for web tasks `cd web && pnpm lint && pnpm format:check && pnpm typecheck && pnpm test && pnpm build && pnpm test:e2e`.
- **Make the mutation's landing observable, not inferred.** A patch whose anchor no longer matches — because a formatter reindented the line, or an earlier fix moved it — prints its error and leaves the suite to run against unmutated code. It then reports green, and **a green run against a mutation that never applied is indistinguishable from a green run against a mutation the suite cannot catch**. Have the probe print that it applied, or diff the file, before believing the result. Task 9 hit this twice in one round, from two different causes.
- **A probe against a built artefact proves nothing unless the build ran between the edit and the run.** Playwright serves whatever is already in `web/dist`, so an e2e probe that edits source and immediately re-runs the suite exercises the *previous* build and passes vacuously — the same shape as a mutation that never applied, with none of the visible signs. Task 9 hit this on its first e2e probe. Any probe of built output needs `pnpm build` in between, and any e2e evidence in a report is only as good as whether that happened.
- **A run that could not gather a sample is a flake, not a finding — guarantee the sample rather than asserting on it.** Timing-shaped tests need a guard proving they measured something, but a guard that *fails* when the machine was too loaded to sample turns a scheduling fact into a red build. Task 6 hit both halves: its window test had no floor and could pass having measured nothing, and then the floor prescribed for it failed 2 runs in 12 under load at `asked=11` and `asked=4`, with zero leaks both times. The fix is a fixture that repeats until it has its sample, bounded, with the assertion unchanged and accumulating across passes — extra passes give the defect more chances to be caught, never fewer. Assert on what you measured; do not assert that the scheduler cooperated.
- **If proving a probe requires an input the suite does not have, that input is a missing test — commit it.** A mutation has two parts: a temporary change to production code, and the input that exposes it. The code change is always reverted; the *input* usually belongs in the suite permanently. Task 4 hit this — its discriminating input (PATCH `forward_to` onto a non-forwarder) was exercised once by hand and then discarded, leaving the committed suite unable to fail for the right reason even though the code was correct. Reverting the mutation is not the same as reverting the test.
- Every fix's test shown failing with the fix removed. **Choose a probe that discriminates**: D4 twice prescribed a mutation that could not fail, and both were caught by implementers rather than by the plan. If a probe would pass against a broken implementation, say so instead of running it.
- **Some test bodies are deliberately sketches, and that is a correction rather than a shortcut.** Where a test depends on fixture shapes this plan has not read — `transfer_test.go`'s master helpers, `app_test.go`'s harness, the web page's existing queries — it states the behaviour to pin and tells you to write it against the real fixtures. D4's plan did the opposite: it invented bodies against assumed helpers (`newTestServer` with two return values, a `decodeJSON` that does not exist, a `recordingTransfers` named `fakeTransfers`), and every one cost an implementer a round. A named behaviour you write correctly beats a body you have to repair. **The exceptions are written out in full** — Task 5's concurrency tests and Task 7's hang test — because their *structure* is the point and getting it wrong makes them silently vacuous.
- **The artboard is on disk** at `.superpowers/sdd/2026-09-03-zones-milestone-d6/zone-detail-artboard.dc.html`. Read it from there. Do **not** call `DesignSync` — it is main-loop only and unreachable from a subagent.

## File Structure

- `internal/zones/hostport.go` — the shared `host[:port]` parse, extracted from two existing copies
- `internal/zones/forwardto.go` — the `forward_to` format: parse, format, validate
- `internal/store/migrations/{sqlite,postgres}/0013_zone_forward.sql` — the `forward_to` column
- `internal/store/zones.go` — the column
- `internal/api/zones_handlers.go` — `forward_to` and the two new types on create and patch
- `internal/upstream/forwarder.go` — `SetConditional`, the atomic `condTable`, `*up` reuse
- `internal/app/app.go` — `reloadZones` pushes the routing table
- `internal/zones/stub.go` — `StubFetcher`: the SOA/NS fetch, glue, and the install
- `internal/zones/refresh.go` — widened to schedule stub zones
- `web/src/pages/zones/detail.tsx`, `web/src/pages/zones/list.tsx` — the two page shapes and the create row
- `docs/` — the two types, the SERVFAIL rule, and why a stub does not expire

---

### Task 1: Extract the shared `host[:port]` parser

**Files:**
- Create: `internal/zones/hostport.go`
- Modify: `internal/zones/primaries.go` — `parsePrimary` calls it
- Modify: `internal/zones/notifyto.go` — `parseNotifyTarget` calls it
- Test: `internal/zones/primaries_test.go` and `internal/zones/notifyto_test.go` **unchanged**

**Interfaces:**
- Produces:
  ```go
  // parseHostPort splits a "host", "host:port", or "[v6]:port" entry.
  //
  // label is the noun each format uses in its errors ("primary", "notify
  // target"), and field is the whole original entry, so the message quotes
  // what the operator typed rather than the fragment being split.
  func parseHostPort(label, field, hostPart string) (host string, port uint16, err error)
  ```

**This is a pure refactor and its test is the existing suites.** `parsePrimary` and `parseNotifyTarget` contain the same twenty lines twice, differing only in the noun in their error strings. Both suites assert on those strings by substring, so passing them unchanged *is* the proof the extraction preserved behaviour — there is no new test to write, and writing one would be testing the same thing a third time.

**Why `field` and `hostPart` are separate parameters.** `parsePrimary` splits the whole field; `parseNotifyTarget` splits only the part before the key, having already stripped ` key:name` off. But both quote the *whole* entry in their errors. Collapsing them to one parameter would either split the key into the host or quote a fragment.

- [ ] **Step 1: Run the existing suites and record the baseline**

Run:
```bash
go test ./internal/zones/ -run 'Primaries|NotifyTo|NotifyTarget' -v 2>&1 | tail -20
```
Expected: PASS. This is the baseline the refactor must not move. Record the pass count.

- [ ] **Step 2: Write the shared function**

```go
package zones

import (
	"errors"
	"fmt"
	"net"
	"strconv"
)

// parseHostPort splits one "host", "host:port" or "[v6]:port" entry into a
// validated host and a port, defaulting to DefaultPrimaryPort.
//
// It is the one implementation of a shape three stored formats share:
// `primaries`, `notify_to` and `forward_to`. It existed twice before
// `forward_to`, and a third copy would have been choosing to keep a
// duplication D4's review had already flagged.
//
// label is the noun the calling format uses in its errors ("primary",
// "notify target", "forward target"), and field is the whole original entry.
// They are separate from hostPart because notify_to strips a trailing
// ` key:name` before splitting but still quotes the entry the operator typed:
// one parameter would either split the key into the host or quote a fragment.
func parseHostPort(label, field, hostPart string) (string, uint16, error) {
	host, portStr, err := net.SplitHostPort(hostPart)
	if err != nil {
		// net.SplitHostPort reports every shape it cannot split as an
		// *net.AddrError, and two of those shapes are legal here: "missing
		// port in address" for a bare host, and "too many colons in address"
		// for a bare IPv6 literal. Both are the no-port form, so the whole
		// part is the host. Anything that is not an AddrError is a failure to
		// parse rather than a form to interpret.
		var addrErr *net.AddrError
		if !errors.As(err, &addrErr) {
			return "", 0, fmt.Errorf("%s %q: %w", label, field, err)
		}
		host, portStr = hostPart, ""
	}
	port := uint16(DefaultPrimaryPort)
	if portStr != "" {
		n, err := strconv.ParseUint(portStr, 10, 16)
		if err != nil || n == 0 {
			return "", 0, fmt.Errorf("%s %q: port must be between 1 and 65535", label, field)
		}
		port = uint16(n)
	}
	if !validPrimaryHost(host) {
		return "", 0, fmt.Errorf("%s %q: host must be an IP address or a domain name", label, field)
	}
	return host, port, nil
}
```

- [ ] **Step 3: Rewrite both callers to use it**

`parsePrimary` becomes:

```go
func parsePrimary(field string) (primary, error) {
	host, port, err := parseHostPort("primary", field, field)
	if err != nil {
		return primary{}, err
	}
	return primary{host: host, port: port}, nil
}
```

In `parseNotifyTarget`, replace the `net.SplitHostPort` block, the port block and the `validPrimaryHost` check with one call — **keeping the key-prefix guard above it**, which must still run before any splitting:

```go
	// (the ` key:name` handling and the "needs a host before the key" guard
	// stay exactly as they are — see their comments)

	host, port, err := parseHostPort("notify target", field, hostPart)
	if err != nil {
		return NotifyTarget{}, err
	}
	return NotifyTarget{Host: host, Port: port, Key: key}, nil
```

- [ ] **Step 4: Run the same suites and compare**

Run:
```bash
go test ./internal/zones/ -run 'Primaries|NotifyTo|NotifyTarget' -v 2>&1 | tail -20
gofmt -l internal cmd
~/go/bin/golangci-lint run ./internal/zones/
```
Expected: the **same** pass count as Step 1, no failures, no skips.

**If any test needed changing, stop and report it.** A refactor that requires editing its own tests is not a refactor; it means an error string or a boundary moved, and the plan wants to know which.

- [ ] **Step 5: Prove the extraction is load-bearing**

Temporarily change `parseHostPort`'s port bound from `n == 0` to `n < 0` (which is never true for a `uint64`), re-run, and confirm the port-zero cases in **both** suites now fail. That shows both formats really route through the one function rather than one of them keeping a private copy. Restore, re-run, confirm green.

- [ ] **Step 6: Commit**

```bash
git add internal/zones/hostport.go internal/zones/primaries.go internal/zones/notifyto.go
git commit -m "refactor(zones): one host[:port] parser for primaries and notify_to

The same twenty lines existed twice, differing only in the noun in their
error strings. D6 adds a third format with the same shape, and D4's review
flagged the duplication when the second copy landed -- a third without
extracting would be choosing to keep it.

No behaviour change: both existing test tables pass unchanged, which is the
proof. Verified load-bearing by breaking the shared port bound and watching
cases in both suites fail."
```

---

### Task 2: The `forward_to` format

**Files:**
- Create: `internal/zones/forwardto.go`
- Test: `internal/zones/forwardto_test.go`

**Interfaces:**
- Consumes: `parseHostPort` (Task 1), `DefaultPrimaryPort`, `validPrimaryHost`.
- Produces:
  ```go
  type ForwardTarget struct {
      Host string // as written — may be a hostname
      Port uint16
  }
  func ValidateForwardTo(s string) error
  func ParseForwardTo(s string) ([]ForwardTarget, error)
  func FormatForwardTo(ts []ForwardTarget) string
  func (t ForwardTarget) Addr() string // "10.0.0.1:53", "[fd00::2]:53"
  ```

**Simpler than its two neighbours, deliberately.** No `key:` (a forwarder signs nothing — it sends ordinary queries), and no resolver (`ParseForwardTo` is pure; hostnames resolve when the routing table is built, Task 6). It is `ParseNotifyTo` minus the key half.

- [ ] **Step 1: Write the failing tests**

```go
package zones_test

import (
	"strings"
	"testing"

	"github.com/aloks98/dnsaur/internal/zones"
)

func TestParseForwardTo(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []zones.ForwardTarget
	}{
		{"empty forwards nowhere", "", nil},
		{"a bare address takes the default port", "10.0.0.1", []zones.ForwardTarget{{Host: "10.0.0.1", Port: 53}}},
		{"an explicit port is kept", "10.0.0.1:5353", []zones.ForwardTarget{{Host: "10.0.0.1", Port: 5353}}},
		{"a hostname is stored as written", "ns.corp.example", []zones.ForwardTarget{{Host: "ns.corp.example", Port: 53}}},
		{"a bare IPv6 literal is the no-port form", "fd00::2", []zones.ForwardTarget{{Host: "fd00::2", Port: 53}}},
		{"a bracketed IPv6 literal may carry a port", "[fd00::2]:5353", []zones.ForwardTarget{{Host: "fd00::2", Port: 5353}}},
		{
			"several, in order",
			"10.0.0.1, 10.0.0.2:5353, ns.corp.example",
			[]zones.ForwardTarget{
				{Host: "10.0.0.1", Port: 53},
				{Host: "10.0.0.2", Port: 5353},
				{Host: "ns.corp.example", Port: 53},
			},
		},
		{"whitespace around separators is tolerated", "  10.0.0.1 ,  10.0.0.2  ", []zones.ForwardTarget{{Host: "10.0.0.1", Port: 53}, {Host: "10.0.0.2", Port: 53}}},
		{"a trailing comma names no target", "10.0.0.1,", []zones.ForwardTarget{{Host: "10.0.0.1", Port: 53}}},
		{"a list of only separators is valid and empty", " , , ", nil},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := zones.ParseForwardTo(tc.in)
			if err != nil {
				t.Fatalf("ParseForwardTo(%q) failed: %v", tc.in, err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("ParseForwardTo(%q) = %+v, want %+v", tc.in, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("entry %d = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestParseForwardToRejects(t *testing.T) {
	tests := []struct{ name, in, contains string }{
		{"port zero", "10.0.0.1:0", "port"},
		{"port above the range", "10.0.0.1:70000", "port"},
		{"a non-numeric port", "10.0.0.1:dns", "port"},
		// These two are the only inputs that reach validPrimaryHost: anything
		// with an internal space is rejected as a whole-field parse failure
		// first, so a multi-word input would exercise a different branch while
		// still producing a message containing "host".
		{"a host with a forbidden character", "a/b", "host"},
		{"a host with an empty label", "a..b", "host"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := zones.ParseForwardTo(tc.in)
			if err == nil {
				t.Fatalf("ParseForwardTo(%q) succeeded, want an error", tc.in)
			}
			if !strings.Contains(err.Error(), tc.contains) {
				t.Errorf("error %q does not mention %q", err, tc.contains)
			}
			// Validate is Parse's error alone; the two must never disagree
			// about what is writable, or the API would accept a value the
			// router then cannot parse.
			if zones.ValidateForwardTo(tc.in) == nil {
				t.Errorf("ValidateForwardTo(%q) accepted what ParseForwardTo rejected", tc.in)
			}
		})
	}
}

// The stored form is FormatForwardTo's spelling rather than what was typed,
// so the value read back is the value the router will parse.
func TestFormatForwardToIsCanonicalAndRoundTrips(t *testing.T) {
	tests := []struct{ in, want string }{
		{"", ""},
		{"10.0.0.1", "10.0.0.1:53"},
		{"10.0.0.1:5353", "10.0.0.1:5353"},
		{"  10.0.0.1 ,10.0.0.2 ", "10.0.0.1:53, 10.0.0.2:53"},
		{"fd00::2", "[fd00::2]:53"},
		{"ns.corp.example", "ns.corp.example:53"},
	}

	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			ts, err := zones.ParseForwardTo(tc.in)
			if err != nil {
				t.Fatalf("ParseForwardTo(%q) failed: %v", tc.in, err)
			}
			got := zones.FormatForwardTo(ts)
			if got != tc.want {
				t.Fatalf("FormatForwardTo = %q, want %q", got, tc.want)
			}
			again, err := zones.ParseForwardTo(got)
			if err != nil {
				t.Fatalf("re-parsing %q failed: %v", got, err)
			}
			if second := zones.FormatForwardTo(again); second != got {
				t.Errorf("not a fixed point: %q then %q", got, second)
			}
		})
	}
}

// Addr is what reaches the routing table, so it must be dialable.
func TestForwardTargetAddr(t *testing.T) {
	tests := []struct{ in, want string }{
		{"10.0.0.1", "10.0.0.1:53"},
		{"10.0.0.1:5353", "10.0.0.1:5353"},
		{"fd00::2", "[fd00::2]:53"},
		{"ns.corp.example:5353", "ns.corp.example:5353"},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			ts, err := zones.ParseForwardTo(tc.in)
			if err != nil {
				t.Fatalf("ParseForwardTo(%q): %v", tc.in, err)
			}
			if got := ts[0].Addr(); got != tc.want {
				t.Errorf("Addr() = %q, want %q", got, tc.want)
			}
		})
	}
}
```

- [ ] **Step 2: Run and watch them fail**

Run: `go test ./internal/zones/ -run 'ForwardTo|ForwardTarget' -v`
Expected: FAIL — `undefined: zones.ParseForwardTo`

- [ ] **Step 3: Implement**

```go
package zones

import (
	"net"
	"strconv"
	"strings"
)

// The `forward_to` format: where a forwarder zone sends its queries.
//
// A comma-separated list, whitespace tolerated, each entry a host with an
// optional port:
//
//	10.0.0.1
//	10.0.0.2:5353
//	ns.corp.example
//	[fd00::2]:5353
//
// Empty means the zone names no upstreams, which §9.11.5 makes a SERVFAIL
// rather than a fall-through: the zone still claims the suffix.
//
// Simpler than its two neighbours on purpose. There is no `key:` because a
// forwarder signs nothing — it sends ordinary queries, not transfers — and
// the parse is pure because hostnames are resolved when the routing table is
// built (internal/app), not when the value is written.

// ForwardTarget is one parsed entry.
type ForwardTarget struct {
	// Host is as written and may be a hostname; it is resolved when the
	// routing table is built.
	Host string
	Port uint16
}

// Addr is the target in dial form, which is what reaches the forwarder's
// conditional routing table.
func (t ForwardTarget) Addr() string {
	return net.JoinHostPort(t.Host, strconv.Itoa(int(t.Port)))
}

// ValidateForwardTo reports whether s is a well-formed forward_to list. Use it
// at write time. An empty list is valid.
func ValidateForwardTo(s string) error {
	_, err := ParseForwardTo(s)
	return err
}

// ParseForwardTo parses s into the targets a forwarder zone's queries go to.
func ParseForwardTo(s string) ([]ForwardTarget, error) {
	var out []ForwardTarget
	for _, field := range strings.Split(s, ",") {
		// Skipped rather than rejected, as splitPrimaries and ParseNotifyTo
		// both do: a trailing comma names no target.
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		host, port, err := parseHostPort("forward target", field, field)
		if err != nil {
			return nil, err
		}
		out = append(out, ForwardTarget{Host: host, Port: port})
	}
	return out, nil
}

// FormatForwardTo writes targets back in the spelling ParseForwardTo reads.
// The port is always written, as FormatNotifyTo does and for the same reason:
// the value is a dial target and the port is part of it.
func FormatForwardTo(ts []ForwardTarget) string {
	parts := make([]string, 0, len(ts))
	for _, t := range ts {
		parts = append(parts, t.Addr())
	}
	return strings.Join(parts, ", ")
}
```

- [ ] **Step 4: Run, then commit**

Run:
```bash
go test ./internal/zones/ -run 'ForwardTo|ForwardTarget' -v
go test ./internal/zones/
gofmt -l internal cmd
~/go/bin/golangci-lint run ./internal/zones/
```
Expected: PASS, and the whole package still green — Task 1 touched two shipped formats.

```bash
git add internal/zones/forwardto.go internal/zones/forwardto_test.go
git commit -m "feat(zones): the forward_to format

host[:port], comma-separated, empty meaning the zone names no upstreams --
which stays a SERVFAIL rather than a fall-through, since the zone still
claims the suffix.

Deliberately simpler than primaries and notify_to: no key: (a forwarder
sends ordinary queries, not transfers) and a pure parse (hostnames resolve
when the routing table is built). It is ParseNotifyTo minus the key half,
over the shared host[:port] parser."
```

---

### Task 3: Migration 0013 and the `forward_to` column

**Files:**
- Create: `internal/store/migrations/sqlite/0013_zone_forward.sql`
- Create: `internal/store/migrations/postgres/0013_zone_forward.sql`
- Modify: `internal/store/zones.go` — the struct field, `zoneColumns`, `scanZone`, and both argument lists
- Test: `internal/store/zones_test.go` (append)

**Interfaces:**
- Produces: `store.Zone` gains `ForwardTo string \`json:"forward_to"\``

**Four edits that must agree, in separate hunks.** `zoneColumns`, `scanZone`, `AddZone`'s argument list and `updateZoneArgs` all carry the column order. A mismatch between any two corrupts every zone read or write, and D4's review caught this by checking all four rather than one. Put `forward_to` immediately after `notify_to` and before `created_at` in all of them.

- [ ] **Step 1: Write the failing test**

```go
// internal/store/zones_test.go — append.

// updateZoneSQL binds every configuration column, and forward_to is one, so a
// PATCH that changes it has to persist. The negative half of the
// disjoint-column-sets rule is automatic here — there is no second writer of
// this column — so this is the positive half only.
func TestUpdateZonePersistsForwardTo(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		name := testGroupName("fwd") + ".example"
		id, err := s.Zones().AddZone(ctx, Zone{
			Name: name, Type: "forwarder", Enabled: true,
			SOANS: "ns1." + name, SOAMbox: "hostmaster." + name,
			SOASerial: 1, SOARefresh: 3600, SOARetry: 600,
			SOAExpire: 604800, SOAMinimum: 300, SOATTL: 900,
		})
		if err != nil {
			t.Fatalf("AddZone: %v", err)
		}

		z, err := s.Zones().Zone(ctx, id)
		if err != nil {
			t.Fatalf("Zone: %v", err)
		}
		if z.ForwardTo != "" {
			t.Fatalf("a new zone has forward_to = %q, want empty", z.ForwardTo)
		}

		z.ForwardTo = "10.0.0.1:53, 10.0.0.2:5353"
		if err := s.Zones().UpdateZone(ctx, z); err != nil {
			t.Fatalf("UpdateZone: %v", err)
		}
		got, err := s.Zones().Zone(ctx, id)
		if err != nil {
			t.Fatalf("Zone after update: %v", err)
		}
		if got.ForwardTo != z.ForwardTo {
			t.Errorf("forward_to = %q, want %q", got.ForwardTo, z.ForwardTo)
		}
		// Every other column survived the round trip. A column-order
		// mismatch between zoneColumns and scanZone shows up here as a
		// neighbouring field holding this one's value.
		if got.Name != name || got.Type != "forwarder" || got.SOASerial != 1 {
			t.Errorf("neighbouring columns moved: %+v", got)
		}
	})
}
```

- [ ] **Step 2: Run and watch it fail**

Run: `go test ./internal/store/ -run TestUpdateZonePersistsForwardTo -v`
Expected: FAIL — `z.ForwardTo undefined`

- [ ] **Step 3: Implement**

`internal/store/migrations/sqlite/0013_zone_forward.sql`:

```sql
-- +goose Up
-- Where a `forwarder` zone sends the queries it claims.
--
-- A comma-separated list of host[:port], parsed by zones.ParseForwardTo and
-- stored in that package's canonical spelling. Empty is the default and means
-- the zone names no upstreams -- which is NOT a fall-through: a forwarder zone
-- keeps its claim on the suffix and answers SERVFAIL, because a split-horizon
-- name that fell through would resolve to whatever the public internet says it
-- is. See section 9.11.5 of the zones design.
--
-- Only `forwarder` uses this column. A `stub` zone names its master in
-- `primaries` instead, which is not an overload: for a secondary that column
-- means "the server I pull this zone from", and for a stub it means "the
-- server I fetch this zone's NS set from" -- the same sentence with a smaller
-- payload. Contrast zones.tsig_key_id, which D4 refused to overload precisely
-- because its meaning would have flipped by zone type.
--
-- No foreign key and no second table: this is one text column on the zone that
-- owns it, and nothing else reads or writes it.
ALTER TABLE zones ADD COLUMN forward_to TEXT NOT NULL DEFAULT '';
```

`internal/store/migrations/postgres/0013_zone_forward.sql` is the same file with the same comment. There is no type to vary — `TEXT NOT NULL DEFAULT ''` is identical on both drivers — but write both files anyway, because goose reads one directory per dialect and a missing file is a version the Postgres install never applies.

`internal/store/zones.go`, four edits:

```go
// 1. On the Zone struct, immediately after the NotifyTo block:

	// ForwardTo is where a `forwarder` zone sends the queries it claims: a
	// comma-separated list of host[:port] in zones.FormatForwardTo's
	// canonical spelling. Empty means the zone names no upstreams, and a
	// forwarder zone with none answers SERVFAIL rather than falling
	// through — it still claims the suffix (§9.11.5).
	//
	// Only `forwarder` uses it. A `stub` names its master in Primaries.
	ForwardTo string `json:"forward_to"`

// 2. zoneColumns — forward_to between notify_to and created_at:
const zoneColumns = `id, name, type, enabled, soa_ns, soa_mbox, soa_serial, soa_refresh, soa_retry, soa_expire, soa_minimum, soa_ttl, primaries, tsig_key_id, expires_at, refreshed_at, last_error, last_attempt, allow_transfer, last_xfr_at, last_xfr_peer, last_xfr_error, notify_to, forward_to, created_at, modified_at`

// 3. scanZone — &z.ForwardTo in the same slot:
	return row.Scan(..., &z.NotifyTo, &z.ForwardTo, &z.CreatedAt, &z.ModifiedAt)

// 4. AddZone's INSERT column list and argument list, and updateZoneArgs, each
//    gain zn.ForwardTo in their own column order — both bind every
//    configuration column, and this is one.
```

- [ ] **Step 4: Run, then commit**

Run:
```bash
go test ./internal/store/ -run 'ForwardTo|Zone' -v
go test -race ./internal/store/
```
Expected: PASS on both the sqlite and postgres subtests. Confirm in your report that the **postgres** subtests ran, not just sqlite.

Verify the column order is really pinned: temporarily swap `&z.ForwardTo` and `&z.NotifyTo` in `scanZone`, re-run, confirm `TestUpdateZonePersistsForwardTo` fails, restore.

```bash
gofmt -l internal cmd
~/go/bin/golangci-lint run ./internal/store/
git add internal/store
git commit -m "feat(store): the forward_to column

Migration 0013, both drivers. Empty by default, so no existing zone starts
forwarding anywhere.

Only forwarder uses it; a stub names its master in primaries, which is the
same sentence that column already means for a secondary rather than the
type-dependent overload D4 refused for tsig_key_id."
```

---

### Task 4: `forward_to` and the two new zone types through the API

**Files:**
- Modify: `internal/api/zones_handlers.go` — `checkZoneTransferConfig`, `canonicalForwardTo`, `zoneCreate`, `zonePatch`, and the creatable-type set
- Modify: `internal/api/openapi.yaml`
- Test: `internal/api/zones_handlers_test.go` (append)

**Interfaces:**
- Consumes: `zones.ValidateForwardTo`, `zones.ParseForwardTo`, `zones.FormatForwardTo` (Task 2); `store.Zone.ForwardTo` (Task 3).
- Produces: `POST /zones` and `PATCH /zones/{id}` accept `forward_to`, and `type` accepts `forwarder` and `stub`.

**The rules, which the tests below pin one each:**

- `forwarder` and `stub` become creatable. `internal` stays refused — it is the RFC 6303 built-ins, seeded at migration.
- `forward_to` is accepted **only** on `forwarder`. On any other type it is a 400, for the reason `primaries` is: configuration nothing reads, shown by the UI as though it meant something.
- `primaries` becomes valid on `stub` as well as `secondary`, and `tsig_key_id` with it — a master that requires TSIG on ordinary queries would refuse a stub's SOA/NS fetch.
- `allow_transfer` and `notify_to` stay refused on both new types: neither serves a zone or has secondaries.
- Validated as the zone would be **stored**, on POST and PATCH alike — `checkZoneTransferConfig`'s own rule. On patch that means the resolved value after the pointer merge.

- [ ] **Step 1: Write the failing tests**

```go
// internal/api/zones_handlers_test.go — append.

func TestZoneCreateAcceptsForwarderAndStub(t *testing.T) {
	tests := []struct {
		name, body string
		check      func(t *testing.T, z store.Zone)
	}{
		{
			name: "forwarder with upstreams",
			body: `{"name":"corp.example","type":"forwarder","forward_to":"10.0.0.1, 10.0.0.2:5353"}`,
			check: func(t *testing.T, z store.Zone) {
				// Canonical, not as typed.
				if z.ForwardTo != "10.0.0.1:53, 10.0.0.2:5353" {
					t.Errorf("forward_to = %q, want the canonical spelling", z.ForwardTo)
				}
			},
		},
		{
			name: "stub with a master",
			body: `{"name":"ad.corp.example","type":"stub","primaries":"10.0.0.9"}`,
			check: func(t *testing.T, z store.Zone) {
				if z.Primaries != "10.0.0.9" {
					t.Errorf("primaries = %q, want it kept", z.Primaries)
				}
				if z.ForwardTo != "" {
					t.Errorf("forward_to = %q on a stub, want empty", z.ForwardTo)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ts := newTestServer(t)
			rec := ts.do(t, "POST", "/api/v1/zones", tc.body)
			if rec.Code != http.StatusCreated {
				t.Fatalf("status %d, body %s", rec.Code, rec.Body)
			}
			tc.check(t, ts.zone(t, createdID(t, rec)))
		})
	}
}

func TestZoneForwardToRefusedOnOtherTypes(t *testing.T) {
	for _, zoneType := range []string{"primary", "secondary", "stub"} {
		t.Run(zoneType, func(t *testing.T) {
			ts := newTestServer(t)
			body := `{"name":"x.example","type":"` + zoneType + `","forward_to":"10.0.0.1"`
			if zoneType == "secondary" || zoneType == "stub" {
				body += `,"primaries":"10.0.0.9"`
			}
			body += `}`
			rec := ts.do(t, "POST", "/api/v1/zones", body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status %d, body %s", rec.Code, rec.Body)
			}
			if !strings.Contains(rec.Body.String(), "forward_to") {
				t.Errorf("body %s does not name the offending field", rec.Body)
			}
		})
	}
}

// A stub may name a TSIG key: its SOA and NS queries are ordinary queries, and
// a master requiring TSIG on those would refuse the fetch outright.
func TestZoneStubAcceptsATSIGKey(t *testing.T) {
	ts := newTestServer(t)
	keyID, err := ts.store.TSIGKeys().Create(t.Context(), store.TSIGKey{
		Name: "stub-key.", Algorithm: "hmac-sha256.", Secret: "c2VjcmV0LXNlY3JldC1zZWNyZXQ=",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	rec := ts.do(t, "POST", "/api/v1/zones",
		`{"name":"ad.corp.example","type":"stub","primaries":"10.0.0.9","tsig_key_id":`+
			strconv.FormatInt(keyID, 10)+`}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body)
	}
	if z := ts.zone(t, createdID(t, rec)); z.TSIGKeyID != keyID {
		t.Errorf("tsig_key_id = %d, want %d", z.TSIGKeyID, keyID)
	}
}

// Neither new type serves a zone or has secondaries, so both transfer-facing
// fields stay refused.
func TestZoneTransferFieldsRefusedOnForwarderAndStub(t *testing.T) {
	cases := []struct{ zoneType, field, value string }{
		{"forwarder", "allow_transfer", "10.0.0.0/24"},
		{"forwarder", "notify_to", "10.0.0.2"},
		{"stub", "allow_transfer", "10.0.0.0/24"},
		{"stub", "notify_to", "10.0.0.2"},
	}
	for _, tc := range cases {
		t.Run(tc.zoneType+"/"+tc.field, func(t *testing.T) {
			ts := newTestServer(t)
			body := `{"name":"x.example","type":"` + tc.zoneType + `","` + tc.field + `":"` + tc.value + `"`
			if tc.zoneType == "stub" {
				body += `,"primaries":"10.0.0.9"`
			}
			body += `}`
			rec := ts.do(t, "POST", "/api/v1/zones", body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status %d, body %s", rec.Code, rec.Body)
			}
		})
	}
}

// A rule enforced on POST and not on PATCH is a rule with a way around it.
func TestZonePatchValidatesForwardTo(t *testing.T) {
	ts := newTestServer(t)
	rec := ts.do(t, "POST", "/api/v1/zones", `{"name":"corp.example","type":"forwarder","forward_to":"10.0.0.1"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %s", rec.Body)
	}
	id := strconv.FormatInt(createdID(t, rec), 10)

	if bad := ts.do(t, "PATCH", "/api/v1/zones/"+id, `{"forward_to":"10.0.0.1:0"}`); bad.Code != http.StatusBadRequest {
		t.Fatalf("PATCH with a bad port: status %d, body %s", bad.Code, bad.Body)
	}
	if ok := ts.do(t, "PATCH", "/api/v1/zones/"+id, `{"forward_to":"  10.0.0.7  "}`); ok.Code != http.StatusNoContent {
		t.Fatalf("PATCH: status %d, body %s", ok.Code, ok.Body)
	}
	if z := ts.zone(t, createdID(t, rec)); z.ForwardTo != "10.0.0.7:53" {
		t.Errorf("forward_to = %q, want the canonical spelling", z.ForwardTo)
	}
	// Clearing back to empty is an ordinary edit, and must not be read as
	// "absent, keep what was there".
	if cleared := ts.do(t, "PATCH", "/api/v1/zones/"+id, `{"forward_to":""}`); cleared.Code != http.StatusNoContent {
		t.Fatalf("clearing PATCH: status %d, body %s", cleared.Code, cleared.Body)
	}
	if z := ts.zone(t, createdID(t, rec)); z.ForwardTo != "" {
		t.Errorf("forward_to = %q after clearing, want empty", z.ForwardTo)
	}
}
```

- [ ] **Step 2: Run and watch them fail**

Run: `go test ./internal/api/ -run 'Forwarder|Stub|ForwardTo' -v`
Expected: FAIL — the types are refused by `handleZoneCreate` and `forward_to` is ignored.

- [ ] **Step 3: Implement**

Add the type constants beside the existing two:

```go
const (
	zoneTypePrimary   = "primary"
	zoneTypeSecondary = "secondary"
	zoneTypeForwarder = "forwarder"
	zoneTypeStub      = "stub"
)
```

`zoneCreate` gains `ForwardTo string \`json:"forward_to"\``; `zonePatch` gains `ForwardTo *string \`json:"forward_to"\`` — a pointer, so absent and empty differ.

`checkZoneTransferConfig` gains a `forwardTo string` parameter and these rules. Note `primaries`/`tsig_key_id` widen from secondary-only to "secondary or stub":

```go
	pullsAZone := zoneType == zoneTypeSecondary || zoneType == zoneTypeStub
	if pullsAZone {
		// Syntax only: ValidatePrimaries does not resolve, so a hostname is
		// stored as written and looked up at use.
		if err := zones.ValidatePrimaries(primaries); err != nil {
			return http.StatusBadRequest, "primaries: " + err.Error()
		}
	} else {
		if primaries != "" {
			return http.StatusBadRequest, "primaries applies to secondary and stub zones only"
		}
		if tsigKeyID != 0 {
			return http.StatusBadRequest, "tsig_key_id applies to secondary and stub zones only"
		}
	}

	if forwardTo != "" {
		// A forwarder is the only type that sends queries somewhere of its
		// own choosing. On anything else this would be configuration nothing
		// reads, shown by the UI as though it meant something.
		if zoneType != zoneTypeForwarder {
			return http.StatusBadRequest, "forward_to applies to forwarder zones only"
		}
		if err := zones.ValidateForwardTo(forwardTo); err != nil {
			return http.StatusBadRequest, err.Error()
		}
	}
```

`allow_transfer` needs a type gate **added**, not widened — corrected 2026-09-03. The plan originally said "the existing gate widens", which asserted a gate that does not exist: `checkZoneTransferConfig`'s `allow_transfer` block goes straight to `ValidateACL`. It never needed one, because until this task only `primary` and `secondary` were creatable, so the field could not arrive on any other type. Making two more types creatable is exactly what creates the need:

```go
	if allowTransfer != "" && zoneType != zoneTypePrimary && zoneType != zoneTypeSecondary {
		return http.StatusBadRequest, "allow_transfer applies to primary and secondary zones only"
	}
```

and `notify_to`'s existing gate already refuses anything but primary/secondary, so it needs no change — confirm that by reading it rather than assuming.

A canonicaliser beside `canonicalNotifyTo`:

```go
// canonicalForwardTo returns input in zones.FormatForwardTo's spelling — the
// form the routing table is built from, so what is stored is what the router
// will parse. input == "" returns "" without parsing.
//
// Called after checkZoneTransferConfig has validated the same string, so the
// error is unreachable in practice; checked rather than discarded because
// errcheck cannot know that, and a swallowed failure would be one call away
// from storing whatever ParseForwardTo gave up on.
func canonicalForwardTo(input string) (string, error) {
	if input == "" {
		return "", nil
	}
	ts, err := zones.ParseForwardTo(input)
	if err != nil {
		return "", err
	}
	return zones.FormatForwardTo(ts), nil
}
```

Finally, widen the creatable-type check in `handleZoneCreate` to accept all four, and update its comment — it currently explains that `stub`/`forwarder` are refused because nothing serves them, which stops being true in this task.

`internal/api/openapi.yaml`: `forward_to` on the `Zone` schema and on both request bodies, the `type` enum extended, and the 400 descriptions updated. The OpenAPI test checks the document against the live route table, so a field in one and not the other fails.

- [ ] **Step 4: Run, then commit**

Run:
```bash
go test ./internal/api/ -run 'Forwarder|Stub|ForwardTo|OpenAPI' -v
go test -race ./internal/api/
```
Expected: PASS

Prove the PATCH gate is load-bearing, and **pick the probe that discriminates**: skipping `checkZoneTransferConfig` on the patch path does *not* fail the malformed-port case, because `canonicalForwardTo` re-parses and catches it anyway. Use the **wrong-type** case instead — `forward_to` on a `stub` is refused only by `checkZoneTransferConfig`. (This exact trap cost D4 a round; the plan is telling you rather than letting you find it.)

```bash
gofmt -l internal cmd
~/go/bin/golangci-lint run ./internal/api/
git add internal/api
git commit -m "feat(api): forwarder and stub zones, and forward_to

Both new types become creatable, validated as the zone would be stored on
POST and PATCH alike.

forward_to is accepted only on a forwarder. primaries and tsig_key_id widen
from secondary-only to secondary-or-stub, because a stub fetches its NS set
by ordinary query and a master requiring TSIG would refuse an unsigned one.
allow_transfer and notify_to stay refused on both: neither type serves a
zone or has secondaries."
```

---

### Task 5: `SetConditional` — a swappable routing table

**Files:**
- Modify: `internal/upstream/forwarder.go`
- Test: `internal/upstream/forwarder_test.go` (append)

**Interfaces:**
- Produces:
  ```go
  func (f *Forwarder) SetConditional(routes map[string][]string) error
  ```

**Why this exists rather than rebuilding the forwarder.** Every record edit calls `reloadZones`. A rebuild discards each upstream's latency EWMA, its 15-second health backoff after three failures, and the RFC 9520 failure cache — so editing records would degrade resolution. The swap replaces only the routing table.

**`*up` values are reused across swaps, keyed by address.** Without it the swap preserves health state for the default upstreams and discards it for exactly the conditional ones most likely to be flaky — the problem the swap exists to solve, one level down.

**The state moves behind an atomic pointer** to an immutable `condTable`, built fresh and swapped: `Resolver.snap`'s shape, for its reason. A reader on the query path must never take a lock, and a rebuild must never mutate a table someone is reading.

- [ ] **Step 1: Write the failing tests**

```go
// internal/upstream/forwarder_test.go — append.

// The table can be replaced after construction, and the new routes take
// effect for subsequent queries.
func TestSetConditionalReplacesRoutes(t *testing.T) {
	corp := mockUpstream(t, answerA("10.0.0.1"))
	other := mockUpstream(t, answerA("10.0.0.2"))
	pub := mockUpstream(t, answerA("5.6.7.8"))

	f, err := New(Config{Upstreams: []string{pub}, Strategy: "failover"})
	if err != nil {
		t.Fatal(err)
	}
	h := f.Handler()

	// No conditional routes yet: the default answers.
	if resp, _ := h.ServeDNS(context.Background(), req("vpn.corp.example")); resp.Upstream != pub {
		t.Fatalf("before SetConditional: upstream = %s, want the default %s", resp.Upstream, pub)
	}

	if err := f.SetConditional(map[string][]string{"corp.example": {corp}}); err != nil {
		t.Fatalf("SetConditional: %v", err)
	}
	if resp, _ := h.ServeDNS(context.Background(), req("vpn.corp.example")); resp.Upstream != corp {
		t.Fatalf("after SetConditional: upstream = %s, want %s", resp.Upstream, corp)
	}

	// Replacing the table re-routes; the old route is gone, not merged.
	if err := f.SetConditional(map[string][]string{"corp.example": {other}}); err != nil {
		t.Fatalf("second SetConditional: %v", err)
	}
	if resp, _ := h.ServeDNS(context.Background(), req("vpn.corp.example")); resp.Upstream != other {
		t.Fatalf("after replacement: upstream = %s, want %s", resp.Upstream, other)
	}

	// An empty table releases the suffix back to the defaults.
	if err := f.SetConditional(nil); err != nil {
		t.Fatalf("clearing SetConditional: %v", err)
	}
	if resp, _ := h.ServeDNS(context.Background(), req("vpn.corp.example")); resp.Upstream != pub {
		t.Fatalf("after clearing: upstream = %s, want the default %s", resp.Upstream, pub)
	}
}

// A swap must not disturb the default upstreams' health state. This is the
// whole reason SetConditional exists instead of a rebuild.
func TestSetConditionalPreservesDefaultUpstreamState(t *testing.T) {
	pub := mockUpstream(t, answerA("5.6.7.8"))
	f, err := New(Config{Upstreams: []string{pub}, Strategy: "failover"})
	if err != nil {
		t.Fatal(err)
	}
	// Give the default upstream some recorded latency by using it.
	if _, err := f.Handler().ServeDNS(context.Background(), req("a.example")); err != nil {
		t.Fatal(err)
	}
	before := f.def[0].ewmaMicro.Load()
	if before == 0 {
		t.Fatal("no latency recorded, so this test cannot detect a reset")
	}

	if err := f.SetConditional(map[string][]string{"corp.example": {pub}}); err != nil {
		t.Fatalf("SetConditional: %v", err)
	}
	if after := f.def[0].ewmaMicro.Load(); after != before {
		t.Errorf("default upstream ewma changed across a swap: %d -> %d", before, after)
	}
}

// A conditional upstream that survives a swap keeps its own health state too.
// Without reuse, the swap fixes the problem for the defaults and leaves it for
// exactly the upstreams a zone edit is most likely to be about.
func TestSetConditionalReusesUpstreamsByAddress(t *testing.T) {
	corp := mockUpstream(t, answerA("10.0.0.1"))
	pub := mockUpstream(t, answerA("5.6.7.8"))
	f, err := New(Config{Upstreams: []string{pub}, Strategy: "failover"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.SetConditional(map[string][]string{"corp.example": {corp}}); err != nil {
		t.Fatal(err)
	}
	// Use the conditional route so it records latency.
	if _, err := f.Handler().ServeDNS(context.Background(), req("vpn.corp.example")); err != nil {
		t.Fatal(err)
	}
	first := f.condTableLoad().routes["corp.example"][0]
	if first.ewmaMicro.Load() == 0 {
		t.Fatal("no latency recorded on the conditional upstream")
	}

	// A swap that still names the same address must keep the same *up.
	if err := f.SetConditional(map[string][]string{
		"corp.example":     {corp},
		"vpn.corp.example": {corp},
	}); err != nil {
		t.Fatal(err)
	}
	second := f.condTableLoad().routes["corp.example"][0]
	if second != first {
		t.Errorf("conditional upstream %s was rebuilt across a swap, losing its health state", corp)
	}
	// THE DISCRIMINATING ASSERTION. Without it this test passes against an
	// implementation that adopts by SUFFIX instead of by address — one that
	// reuses old.routes[key] wholesale when the key and its address list are
	// unchanged, and mints everything else fresh. That mutant loses the EWMA,
	// the 15s backoff and the failure history exactly when a reload adds a
	// suffix, renames one, or edits an address list: the ordinary zone edit.
	// corp.example above is the suffix whose key AND list are unchanged, so it
	// cannot tell the two apart. vpn.corp.example is new and names an address
	// the outgoing table already knows, so only address-keyed reuse adopts it.
	if adopted := f.condTableLoad().routes["vpn.corp.example"][0]; adopted != first {
		t.Errorf("a new suffix naming a known address did not adopt the existing *up")
	}
}

// Two suffixes naming the SAME address in ONE call must share one *up.
// Several internal zones forwarded to the same corporate resolver is the
// ordinary case, and without this they get independent fails/downUntil: the
// resolver goes down, and each zone has to discover it separately.
func TestSetConditionalDedupesWithinOneCall(t *testing.T) {
	corp := mockUpstream(t, answerA("10.0.0.1"))
	pub := mockUpstream(t, answerA("5.6.7.8"))
	f, err := New(Config{Upstreams: []string{pub}, Strategy: "failover"})
	if err != nil {
		t.Fatal(err)
	}
	// Both suffixes are new, so neither can be adopted from an outgoing
	// table — the sharing has to happen within this one call.
	if err := f.SetConditional(map[string][]string{
		"corp.example": {corp},
		"lab.example":  {corp},
	}); err != nil {
		t.Fatal(err)
	}
	tbl := f.condTableLoad()
	if a, b := tbl.routes["corp.example"][0], tbl.routes["lab.example"][0]; a != b {
		t.Errorf("two suffixes naming %s built separate *up with independent health state", corp)
	}
}

// Longest-suffix routing still wins after a swap, deterministically.
func TestSetConditionalKeepsLongestSuffixWins(t *testing.T) {
	corp := mockUpstream(t, answerA("10.0.0.1"))
	vpn := mockUpstream(t, answerA("10.0.0.2"))
	pub := mockUpstream(t, answerA("5.6.7.8"))
	f, _ := New(Config{Upstreams: []string{pub}, Strategy: "failover"})
	if err := f.SetConditional(map[string][]string{
		"corp.example":     {corp},
		"vpn.corp.example": {vpn},
	}); err != nil {
		t.Fatal(err)
	}
	h := f.Handler()
	for i := 0; i < 20; i++ {
		resp, err := h.ServeDNS(context.Background(), req("vpn.corp.example"))
		if err != nil || resp.Upstream != vpn {
			t.Fatalf("iteration %d: want the most specific match %s, got %+v err=%v", i, vpn, resp, err)
		}
	}
}

// A swap during in-flight queries must not tear. The reader takes no lock, so
// this is the one place the concurrency has to be exercised rather than
// reasoned about.
func TestSetConditionalIsSafeUnderConcurrentQueries(t *testing.T) {
	corp := mockUpstream(t, answerA("10.0.0.1"))
	pub := mockUpstream(t, answerA("5.6.7.8"))
	f, _ := New(Config{Upstreams: []string{pub}, Strategy: "failover"})
	h := f.Handler()

	// The readers must be proven to have run AGAINST A SWAP, and getting this
	// right took two attempts -- the second only because the first was
	// measured rather than reasoned about.
	//
	// A plain ready-barrier (reader closes a channel after its first query,
	// writer waits on it) is NOT enough, and it fails in a way that looks
	// like success. close(ready) is synchronized-before the <-ready that
	// observes it, so the query that closed the channel is ordered strictly
	// BEFORE every swap. It satisfies attempts != 0 while proving nothing.
	// Against a plain *condTable field in place of the atomic.Pointer, that
	// version catches the race only 15/20 at -cpu=1 -- and the counter reads
	// ATTEMPTS=5 on all twenty runs, identical in the fifteen that caught it
	// and the five that did not, so the guard cannot tell them apart.
	//
	// What is needed is a per-round handshake: the writer waits for a reader
	// to complete a query BETWEEN every swap. That is 20/20 at -cpu=1 and
	// 20/20 at default.
	//
	// THE RULE, stated carefully because the obvious version of it is wrong:
	// the handshake must be ONE-WAY (reader -> writer) and must TRAIL the
	// swap -- the writer waits after swapping and never signals before it.
	// It is NOT that "atomics are safe and channels are not". A one-way
	// unbuffered-channel handshake measures identically at 20/20, even with a
	// single reader. An unbuffered handoff does install a bidirectional edge,
	// but it orders swap N before the reader's next read, and swap N+1 is the
	// one that races -- nothing orders that. What genuinely masks the race is
	// a two-way ping-pong (writer signals "go", reader signals "done") or any
	// writer-side store to what the readers read. The atomic-load spin is
	// kept over the channel only because buffered channels reuse slots and a
	// blocking send turns the handoff into exactly that ping-pong: it is the
	// variant with the fewest footguns for whoever edits this test next.
	var attempts, failures atomic.Int64
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					if _, err := h.ServeDNS(context.Background(), req("vpn.corp.example")); err != nil {
						failures.Add(1)
					}
					attempts.Add(1)
				}
			}
		}()
	}

	deadline := time.Now().Add(30 * time.Second)
	awaitQuery := func() {
		t.Helper()
		for target := attempts.Load() + 1; attempts.Load() < target; {
			if time.Now().After(deadline) {
				t.Fatal("readers completed no query: no read of f.cond ran against a swap, so this proved nothing")
			}
			runtime.Gosched()
		}
	}
	awaitQuery()

	for i := 0; i < 50; i++ {
		if err := f.SetConditional(map[string][]string{"corp.example": {corp}}); err != nil {
			t.Fatal(err)
		}
		awaitQuery()
		if err := f.SetConditional(nil); err != nil {
			t.Fatal(err)
		}
		awaitQuery()
	}
	close(stop)
	wg.Wait()

	if n := attempts.Load(); n == 0 {
		t.Fatal("no query ran against the swaps: this test proved nothing")
	}
	// Closes the second vacuity path: one failed query arms the 30s failCache
	// short-circuit, after which ServeDNS returns before ever reaching pick —
	// the readers would spin without touching the table under test.
	if n := failures.Load(); n != 0 {
		t.Fatalf("%d of %d queries failed: the failure cache now short-circuits pick, so the readers stopped exercising the swap", n, attempts.Load())
	}
}
```

> **Verify this one at `-cpu=1` as well as at the default.** `go test -race -cpu=1 -run SetConditionalIsSafe ./internal/upstream/` must still execute queries and must still catch the plain-field mutant. A concurrency test that only discriminates on a well-provisioned machine is one pinned CI runner away from being live.

> `f.condTableLoad()` is a small unexported accessor these tests need; `internal/upstream` tests are internal (`package upstream`), so it does not have to be exported. Add it beside the field.

- [ ] **Step 2: Run and watch them fail**

Run: `go test ./internal/upstream/ -run SetConditional -v`
Expected: FAIL — `f.SetConditional undefined`

- [ ] **Step 3: Implement**

```go
// condTable is the conditional routing table: immutable once built, replaced
// wholesale by SetConditional. A reader on the query path loads the pointer
// and never takes a lock, so the table it is reading can never be mutated
// underneath it — the shape zones.Resolver uses for its snapshot, for the
// same reason.
type condTable struct {
	set    *filter.DomainSet
	routes map[string][]*up
}
```

`Forwarder`'s `condSet`/`condRoutes` fields are replaced by `cond atomic.Pointer[condTable]`, and `pick` becomes:

```go
func (f *Forwarder) pick(qname string) []*up {
	if t := f.cond.Load(); t != nil {
		if matched, ok := t.set.Match(qname); ok {
			return t.routes[matched]
		}
	}
	return f.def
}
```

`New` builds the table through `SetConditional` rather than inline, so there is one construction path. `SetConditional` itself:

```go
// SetConditional replaces the suffix routing table.
//
// The default upstreams, their health state and the failure cache are all
// untouched: this exists precisely so a zone reload — which happens on every
// record edit — does not discard them by rebuilding the whole Forwarder.
//
// Upstreams are reused across swaps by address, so a conditional upstream
// that survives a reload keeps its latency history and its down-marking too.
// Rebuilding them would fix the problem for the defaults and leave it for the
// conditional routes, which are the ones a zone edit is about.
//
// A nil or empty map releases every suffix back to the defaults.
func (f *Forwarder) SetConditional(routes map[string][]string) error {
	if len(routes) == 0 {
		f.cond.Store(nil)
		return nil
	}

	// Every *up currently in use, keyed by address, so a swap can adopt
	// rather than rebuild.
	existing := map[string]*up{}
	if old := f.cond.Load(); old != nil {
		for _, ups := range old.routes {
			for _, u := range ups {
				existing[u.addr] = u
			}
		}
	}

	t := &condTable{
		// One shared DomainSet across all suffixes so Match's
		// most-specific-wins gives deterministic longest-suffix routing,
		// instead of iterating per-suffix sets in randomised map order.
		set:    filter.NewDomainSet(),
		routes: make(map[string][]*up, len(routes)),
	}
	for suffix, addrs := range routes {
		if len(addrs) == 0 {
			// A suffix with no upstreams still claims the name: pick returns
			// an empty slice, every attempt fails, and the handler answers
			// SERVFAIL rather than falling through to the defaults. That is
			// §9.11.5's rule, and it is why this is not skipped.
			t.set.Add(suffix)
			t.routes[strings.ToLower(strings.TrimSuffix(suffix, "."))] = nil
			continue
		}
		t.set.Add(suffix)
		var ups []*up
		for _, a := range addrs {
			if u, ok := existing[a]; ok {
				ups = append(ups, u)
				continue
			}
			ups = append(ups, newUp(a, f.timeout))
		}
		t.routes[strings.ToLower(strings.TrimSuffix(suffix, "."))] = ups
	}
	f.cond.Store(t)
	return nil
}

func (f *Forwarder) condTableLoad() *condTable { return f.cond.Load() }
```

> **The empty-upstreams path is verified, not assumed** — traced through all three strategies before this plan relied on it, because §9.11.5's whole rule rests on it.
>
> `pick` returns an empty slice → the `healthy` filter adds nothing → `len(healthy) == 0` sets `healthy = candidates`, still empty. Then: `failover`/`fastest` iterate zero times, leaving `r == nil`; `race` builds a zero-capacity channel, spawns no goroutines, and its `for range ups` runs zero times, so it returns `(nil, nil, nil)` immediately rather than blocking. **No strategy hangs.** All three reach `if r == nil`, which writes a 30-second fail-cache entry and returns `nil, errors.New("all upstreams failed")` — and `Server.serve` turns a handler error into `Servfail(req)`.
>
> So a claimed suffix with no reachable upstreams answers SERVFAIL and never falls through, on every strategy including the default `race`.

- [ ] **Step 4: Run, then commit**

Run:
```bash
go test -race ./internal/upstream/ -v 2>&1 | tail -20
```
Expected: PASS, including the pre-existing `TestConditionalRouting` and `TestConditionalRoutingLongestSuffixWins` — `New` now routes through `SetConditional`, so those are the proof the construction path did not change behaviour.

Prove the reuse is real: make `SetConditional` always call `newUp` instead of adopting, re-run `TestSetConditionalReusesUpstreamsByAddress`, confirm it fails, restore.

```bash
gofmt -l internal cmd
~/go/bin/golangci-lint run ./internal/upstream/
git add internal/upstream
git commit -m "feat(upstream): a swappable conditional routing table

SetConditional replaces the suffix routes without disturbing the default
upstreams, their health state, or the failure cache -- which a rebuild
would discard on every record edit, since each one calls reloadZones.

The table sits behind an atomic pointer and is immutable once built, so
pick takes no lock and can never read a table being mutated underneath it.
Upstreams are adopted across swaps by address, so a conditional upstream
keeps its latency history and down-marking too; rebuilding them would fix
the problem for the defaults and leave it for the routes a zone edit is
actually about.

New now builds through the same path, so the existing conditional-routing
tests are the proof construction did not change."
```

---

### Task 6: Wiring — zone reloads push the routing table

**Files:**
- Modify: `internal/app/app.go` — `reloadZones` (or its callee) builds the map and calls `SetConditional`
- Test: `internal/app/app_test.go` (append)

**Interfaces:**
- Consumes: `zones.ParseForwardTo` (Task 2), `store.Zone.ForwardTo` (Task 3), `Forwarder.SetConditional` (Task 5).
- Produces: a running server where a `forwarder` zone actually routes.

**This is the task that makes tasks 2–5 reachable.** Until it lands, `forward_to` is a column nothing reads.

**Three rules the tests pin one each:**

- **Only enabled zones contribute.** Disabling a forwarder zone releases its suffix back to the global upstreams — consistent with every other path, where disabled means "this server is not handling that name".
- **A zone with no upstreams still claims its suffix**, contributing an empty list so §9.11.5's SERVFAIL applies rather than a fall-through. This is where a hostname that will not resolve ends up, and it is the safe direction.
- **`stub` zones contribute too**, from their fetched NS records rather than `forward_to`. Task 7 writes the derivation; this task calls it and tolerates a zone that has not fetched yet by claiming the suffix with no upstreams.

- [ ] **Step 1: Write the failing tests**

```go
// internal/app/app_test.go — append.
//
// These drive the real App, so they prove the wiring rather than the map
// builder in isolation.

// A forwarder zone routes its suffix to its own upstreams, and the default
// upstreams still answer everything else.
func TestForwarderZoneRoutesToItsUpstreams(t *testing.T) {
	corp := mockDNS(t, answerA("10.0.0.1"))
	pub := mockDNS(t, answerA("5.6.7.8"))

	a := newTestApp(t, withUpstreams(pub))
	mustAddZone(t, a, store.Zone{
		Name: "corp.example", Type: "forwarder", Enabled: true,
		ForwardTo: corp,
	})
	mustReloadZones(t, a)

	if got := askApp(t, a, "vpn.corp.example", dns.TypeA); got != "10.0.0.1" {
		t.Errorf("in-zone query answered %q, want the forwarder's upstream", got)
	}
	if got := askApp(t, a, "elsewhere.example", dns.TypeA); got != "5.6.7.8" {
		t.Errorf("out-of-zone query answered %q, want the default upstream", got)
	}
}

// Disabling releases the suffix. Without this a disabled forwarder zone would
// keep swallowing its suffix while every other path in the server treats
// disabled as "not handling that name".
func TestDisablingAForwarderZoneReleasesItsSuffix(t *testing.T) {
	corp := mockDNS(t, answerA("10.0.0.1"))
	pub := mockDNS(t, answerA("5.6.7.8"))

	a := newTestApp(t, withUpstreams(pub))
	id := mustAddZone(t, a, store.Zone{
		Name: "corp.example", Type: "forwarder", Enabled: true, ForwardTo: corp,
	})
	mustReloadZones(t, a)
	if got := askApp(t, a, "vpn.corp.example", dns.TypeA); got != "10.0.0.1" {
		t.Fatalf("precondition: in-zone query answered %q", got)
	}

	z := mustZone(t, a, id)
	z.Enabled = false
	if err := a.Store().Zones().UpdateZone(t.Context(), z); err != nil {
		t.Fatalf("UpdateZone: %v", err)
	}
	mustReloadZones(t, a)

	// A DIFFERENT name under the same suffix, and that is not fussiness.
	// The middleware order is resolver -> cache -> forwarder, so the
	// precondition query above put a 300s answer for vpn.corp.example in the
	// cache. Re-asking that name after disabling returns 10.0.0.1 from cache
	// no matter what the routing table now says: the test would fail against
	// CORRECT code and could never observe the release it exists to check.
	// The precondition proves the suffix was claimed; a fresh name under the
	// same suffix proves it has been released.
	if got := askApp(t, a, "other.corp.example", dns.TypeA); got != "5.6.7.8" {
		t.Errorf("after disabling, answered %q, want the default upstream", got)
	}
}

// A forwarder zone naming no upstreams keeps its claim and SERVFAILs. Falling
// through would let the public internet answer an internal name, which is the
// failure §9.11.5 exists to prevent.
func TestForwarderZoneWithNoUpstreamsServfails(t *testing.T) {
	pub := mockDNS(t, answerA("5.6.7.8"))
	a := newTestApp(t, withUpstreams(pub))
	mustAddZone(t, a, store.Zone{
		Name: "corp.example", Type: "forwarder", Enabled: true, ForwardTo: "",
	})
	mustReloadZones(t, a)

	m := askAppMsg(t, a, "vpn.corp.example", dns.TypeA)
	if m.Rcode != dns.RcodeServerFailure {
		t.Errorf("rcode = %s, want SERVFAIL", dns.RcodeToString[m.Rcode])
	}
	if len(m.Answer) != 0 {
		t.Errorf("answered %v, want nothing — the public internet must not be asked", m.Answer)
	}
}

// A forwarder-zone answer is cached, which is the whole reason routing lives
// in the forwarder rather than in the resolver middleware.
func TestForwarderZoneAnswersAreCached(t *testing.T) {
	var hits atomic.Int64
	corp := mockDNSCounting(t, &hits, answerA("10.0.0.1"))
	pub := mockDNS(t, answerA("5.6.7.8"))

	a := newTestApp(t, withUpstreams(pub))
	mustAddZone(t, a, store.Zone{
		Name: "corp.example", Type: "forwarder", Enabled: true, ForwardTo: corp,
	})
	mustReloadZones(t, a)

	for i := 0; i < 3; i++ {
		if got := askApp(t, a, "vpn.corp.example", dns.TypeA); got != "10.0.0.1" {
			t.Fatalf("query %d answered %q", i, got)
		}
	}
	if n := hits.Load(); n != 1 {
		t.Errorf("upstream asked %d times for 3 identical queries, want 1 — answers are not being cached", n)
	}
}
```

> `newTestApp`, `withUpstreams`, `mustAddZone`, `mustReloadZones`, `mustZone`, `askApp`, `askAppMsg`, `mockDNS` and `mockDNSCounting` are helpers this file may or may not already have. **Read `internal/app/app_test.go` first** and reuse whatever it defines; add only what is genuinely missing, and tell me if its shapes differ from these names.

- [ ] **Step 2: Run and watch them fail**

Run: `go test ./internal/app/ -run 'Forwarder' -v`
Expected: FAIL — the in-zone query is answered by the default upstream, because nothing routes yet.

- [ ] **Step 3: Implement**

Add a builder beside `reloadZones`'s callee:

```go
// conditionalRoutes maps each enabled forwarder and stub zone's apex to the
// addresses its queries go to.
//
// A zone that names no usable upstreams is still included, with an empty
// list. That is deliberate: pick returns the empty slice, every attempt
// fails, and the handler answers SERVFAIL — so the zone keeps its claim on
// the suffix instead of falling through to the default resolvers and letting
// a public answer shadow an internal name (§9.11.5). Omitting it would be the
// fall-through this design refuses.
//
// A disabled zone is skipped entirely, which releases its suffix back to the
// defaults — the same meaning "disabled" has on every other path.
// It walks the resolver's snapshot rather than the store, for two reasons.
// The snapshot is what the server is actually serving, so the routing table
// cannot describe a zone the resolver has not loaded yet. And a stub's
// upstreams are derived from its NS records and their glue, which live on
// zones.Zone — store.Zone carries the row only, so reading the store would
// make StubUpstreams impossible to call without a second query per zone.
func (a *App) conditionalRoutes() map[string][]string {
	routes := map[string][]string{}
	for _, z := range a.resolver.Snapshot().Zones() {
		if !z.Enabled {
			continue
		}
		switch strings.ToLower(z.Type) {
		case "forwarder":
			targets, err := zones.ParseForwardTo(z.ForwardTo)
			if err != nil {
				// Fails closed: an unparseable stored value names no
				// upstreams, so the zone SERVFAILs rather than forwarding
				// somewhere unintended. The API validates on write, so
				// reaching this means a hand-edited row.
				slog.Warn("zone forward_to will not parse; the zone will answer SERVFAIL",
					"zone", z.Name, "err", err)
				routes[z.Name] = nil
				continue
			}
			addrs := make([]string, 0, len(targets))
			for _, t := range targets {
				addrs = append(addrs, t.Addr())
			}
			routes[z.Name] = addrs
		case "stub":
			// Derived from the fetched NS set; empty until the first fetch
			// lands, which claims the suffix and SERVFAILs meanwhile.
			routes[z.Name] = zones.StubUpstreams(z)
		}
	}
	return routes
}
```

`nil` from `conditionalRoutes` (the read-failed case) must **not** reach `SetConditional`, since that would clear every route. Guard at the call site:

```go
	if err := a.forwarder.SetConditional(a.conditionalRoutes()); err != nil {
		slog.Error("installing conditional routes failed", "err", err)
	}
```

**Two things this task must add, both verified as missing rather than assumed:**

**`Index` cannot be iterated.** Its `zones` slice is unexported and it exposes only `Apex` and `Find`. Add an accessor returning the snapshot's zones — the `Index` is immutable once built and replaced wholesale by `Reload`, so returning the slice is safe as long as the doc comment says callers must not mutate it:

```go
// Zones returns every zone in this snapshot.
//
// The slice is the Index's own and must not be mutated: an Index is
// immutable once built and shared by every reader of the snapshot. Reload
// replaces the whole Index rather than editing one in place, which is what
// makes handing the slice out safe.
func (idx *Index) Zones() []Zone { return idx.zones }
```

**`a.fwd` is a `*swappable` holding a `dnssrv.Handler`, not a `*upstream.Forwarder`** (`app.go:49-53`), so `SetConditional` is unreachable through it. `App` keeps the `*upstream.Forwarder` alongside it — `a.forwarder` — and `applySettings` sets both.

**That creates the ordering hazard, and it is the subtle part of this task.** `applySettings` builds a *fresh* forwarder on every settings change, and a fresh forwarder has no conditional routes. Install them before it goes live, or a settings edit silently drops every zone's routing until the next unrelated zone edit — a forwarder zone would quietly start resolving through the public internet, which is the exact failure §9.11.5 exists to prevent. Add a test: change a setting, then assert an in-zone query still reaches the zone's own upstream.

`zones.StubUpstreams` is Task 7's; until it lands, add it returning `nil` so this task's forwarder tests pass and stub zones simply claim-and-SERVFAIL.

- [ ] **Step 4: Run, then commit**

Run:
```bash
go test -race ./internal/app/ -v 2>&1 | tail -20
go test -race ./...
```
Expected: PASS

Prove the caching claim discriminates: temporarily move the routing into the resolver middleware (answer directly from there for a forwarder zone) and confirm `TestForwarderZoneAnswersAreCached` fails with 3 upstream hits instead of 1. Restore. That is the measurement behind §9.11.4's rejection of resolver-side dispatch, and it is worth having run once.

```bash
gofmt -l internal cmd
~/go/bin/golangci-lint run ./...
git add internal/app
git commit -m "feat: route forwarder zones through the conditional table

A zone reload builds apex -> addresses for every enabled forwarder and stub
zone and installs it on the forwarder. Disabled zones are skipped, which
releases their suffix back to the defaults.

A zone naming no usable upstreams is still included with an empty list, so
it keeps its claim and answers SERVFAIL rather than falling through and
letting a public answer shadow an internal name. An unparseable stored
value fails the same way.

A failed zone read keeps the existing table rather than clearing it: losing
every claimed suffix because one query failed would send internal names to
the public internet."
```

---

### Task 7: The stub fetcher

**Files:**
- Modify: `internal/zones/stub.go` — **it already exists.** Task 6 created it holding
  `StubUpstreams` returning `nil`, so its forwarder wiring could compile and stub
  zones would claim-and-SERVFAIL until you land. You are filling it in, not
  creating it.
- Test: `internal/zones/stub_test.go`
- `internal/app/app.go` — **no change needed.** Task 6 already calls
  `zones.StubUpstreams(z)` against the resolver snapshot (`app.go`, the
  conditional-route collection). Making `StubUpstreams` return real addresses is
  the whole of the wiring; there is no second edit in `app.go`.

**Interfaces:**
- Consumes: `ParsePrimaries`, `Transferrer.zoneKey`'s TSIG mechanism, `store.ReplaceRecords`.
- Produces:
  ```go
  type StubFetcher struct{ /* ... */ }
  func NewStubFetcher(zs store.ZoneStore, keys TSIGKeys, opts ...StubOption) *StubFetcher
  func WithStubNow(now func() time.Time) StubOption
  func WithStubResolver(res *net.Resolver) StubOption
  // Fetch queries the master for the zone's SOA and NS, and installs them.
  func (f *StubFetcher) Fetch(ctx context.Context, z store.Zone) (StubResult, error)

  type StubResult struct {
      Master    netip.AddrPort
      Serial    uint32
      NS        int   // nameservers installed
      Upstreams []string // dial addresses derived from the NS set
  }

  // StubUpstreams derives dial addresses from a stub zone's stored NS
  // records, for the routing table. Pure: no queries, no lookups.
  func StubUpstreams(z Zone) []string
  ```

**Two ordinary queries, not an AXFR** — so a stub needs no `allow_transfer` permission on the master, which is the point of the type. Both signed when the zone names a key, using the same mechanism `Transferrer.fetch` and `probeOne` use. Read one of those and reuse it; a third signing implementation is the bug that only shows against a TSIG-requiring master.

**The glue rule is the hard part, and it is the reason this task exists rather than being folded into Task 6:**

- **In-zone nameserver** (its name ends with the apex): the address **must** come from glue in the master's ADDITIONAL section. No glue, no usable nameserver — skip it. **Never resolve it.** Resolving `ns1.corp.example` would match the stub's own suffix, route into the stub zone, and need the address being resolved. That is not a slow failure, it is a hang.
- **Out-of-zone nameserver**: resolved through `net.Resolver`, the same resolver `ParsePrimaries` uses. It cannot re-enter this zone because the suffix differs.

- [ ] **Step 1: Write the failing tests**

```go
package zones_test

// stubMaster answers SOA and NS for one zone, with optional glue, and counts
// what it was asked.
type stubMaster struct {
	addr    string
	queries atomic.Int64
	// glue, when false, answers the NS RRset with an empty ADDITIONAL —
	// the case an in-zone nameserver cannot survive.
	glue bool
}

func TestStubFetchInstallsTheNSSet(t *testing.T) {
	// A master serving corp.example with two in-zone nameservers and glue
	// for both. Fetch must install the NS records, adopt the SOA's serial
	// and schedule, and derive both addresses from the glue.
}

func TestStubFetchSkipsAnInZoneNameserverWithNoGlue(t *testing.T) {
	// THE HANG TEST. The master answers NS ns1.corp.example with no glue.
	// ns1.corp.example is inside the stub's own suffix, so resolving it
	// would route back into this zone and never terminate.
	//
	// TWO independent guards, and the ordering matters.
	//
	// The PRIMARY assertion is not the timeout — it is that the resolver is
	// never ASKED. Inject a resolver that fails the test on any name inside
	// the apex; then removing the guard fails deterministically, with a
	// message naming the defect, and with no dependence on timing at all.
	//
	//     res := &recordingResolver{t: t, rejectUnder: "corp.example."}
	//
	// The timeout is only a backstop that converts a genuine hang into a
	// failure instead of a package-wide timeout. Its deadline must be
	// SEPARATE from and much shorter than the context Fetch runs under.
	// The obvious shape -- one 5s context shared by Fetch and the select --
	// is broken: if Fetch respects that context (net.Resolver does), then
	// with the guard removed Fetch returns an error at ~5s and done closes
	// at the same instant ctx.Done() fires. select picks at random between
	// two ready cases, so the test passes about half the time. A flaky pass
	// on the one test whose whole purpose is a hang is worse than no test.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() { defer close(done); /* ... f.Fetch(ctx, z) ... */ }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Fetch did not return: an in-zone nameserver was resolved instead of skipped")
	}
	// And assert the OUTCOME, not merely that Fetch returned. Returning
	// early for an unrelated reason -- an unreachable master, a parse
	// failure -- also closes done, and a test that only waits would pass on
	// any of them. The zone must end with no usable upstreams, which is what
	// makes it claim its suffix and SERVFAIL per §9.11.5.
}

func TestStubFetchResolvesAnOutOfZoneNameserver(t *testing.T) {
	// NS ns.example.net for zone corp.example: different suffix, so it
	// cannot re-enter, and it is resolved through the injected resolver.
}

func TestStubFetchSignsWithTSIGWhenTheZoneNamesAKey(t *testing.T) {
	// A master that REFUSES anything not correctly signed. A fetch that
	// completes can only have been signed.
}

func TestStubFetchAgainstATSIGMasterFailsUnsigned(t *testing.T) {
	// The negative half: without it the test above passes against a master
	// that does not care.
}

func TestStubFetchTriesEveryMasterAndNamesEachFailure(t *testing.T) {
	// primaries are walked in order, any failure moves to the next, and the
	// all-failed error names every attempt — Transfer's own semantics.
}

func TestStubUpstreamsIsPureAndDerivesFromStoredRecords(t *testing.T) {
	// StubUpstreams reads the zone's NS records and the glue stored beside
	// them; it makes no queries, so the routing table can be rebuilt on
	// every zone reload without touching the network.
}
```

> These are behaviour sketches, not bodies. **Write each one out fully against the fixtures `transfer_test.go` already provides** — `startTestPrimary`, `withTSIG`, `newTransferFixture`, `storeTSIGKey` — extending them where a stub needs something a transfer does not (an NS/SOA responder rather than an AXFR one). Do not invent a parallel harness. The hang test is the one that must be written exactly as described, because it has two guards that do different jobs and the ordering between them matters.

- [ ] **Step 2: Run and watch them fail**

Run: `go test ./internal/zones/ -run Stub -v`
Expected: FAIL — `undefined: zones.NewStubFetcher`

- [ ] **Step 3: Implement**

`Fetch`'s shape, with the parts that matter spelled out:

```go
// Fetch queries z's masters for the zone's SOA and NS records and installs
// what comes back.
//
// Two ordinary queries rather than an AXFR: a stub needs no allow_transfer
// permission on the master, which is the whole reason the type exists apart
// from a secondary. Both are signed when the zone names a key — a master that
// requires TSIG on ordinary queries would otherwise refuse the fetch.
//
// The masters are tried in the order written and any failure moves to the
// next, exactly as Transfer does: masters are meant to be replicas, so one
// refusing is a reason to ask another. The error names every attempt.
func (f *StubFetcher) Fetch(ctx context.Context, z store.Zone) (StubResult, error)
```

The glue rule inside it:

```go
// nsAddresses turns the fetched NS RRset into dial addresses.
//
// **An in-zone nameserver is never resolved.** Its name ends with the apex,
// so resolving it would match this very zone's suffix, route into the stub,
// and need the address being resolved — a hang, not an error. DNS's own
// answer is glue, and it is the right one: the address must come from the
// master's ADDITIONAL section or the nameserver is unusable and is skipped.
//
// An out-of-zone name cannot re-enter this zone, so it is resolved normally,
// through the same net.Resolver ParsePrimaries uses.
//
// Every nameserver being unusable is not an error: the zone keeps its claim
// on the suffix and answers SERVFAIL (§9.11.5), which is what an empty
// upstream list produces.
func (f *StubFetcher) nsAddresses(ctx context.Context, apex string, ns []dns.RR, extra []dns.RR) []string
```

Install through `ReplaceRecords` — SOA fields onto the zone row, NS records into `zone_records` — exactly as a transfer installs a zone. **Glue must be stored too**, or `StubUpstreams` cannot rebuild the routing table on a reload without re-querying: store each glue address as an A/AAAA record under the nameserver's own relative name, the way a real zone would carry it.

`StubUpstreams(z Zone) []string` then reads the installed NS records and their glue and returns dial addresses, purely.

Task 6 already wired `zones.StubUpstreams(z)` into the conditional-route
collection against the resolver's snapshot, so **no `app.go` change is needed
here** — the stub returning `nil` becomes a stub returning addresses, and the
routing table picks them up on the next reload.

**THE TRAP, and it is this task's to close.** `App.New` builds the Transferrer
with `zones.WithReload(a.resolver.Reload)` — the resolver method directly, not
`App.ReloadZones`. So an install triggered by a transfer or a NOTIFY rebuilds
the zone snapshot **without reinstalling the conditional routing table**. That
is invisible today because forwarder zones are never transferred. The moment
your `StubFetcher` installs NS records through that path, the stub claims its
suffix with a table that still has no addresses for it, and SERVFAILs forever —
looking exactly like a fetch that never happened, which is the most expensive
possible presentation of this bug.

The fix is one line — `zones.WithReload(a.ReloadZones)`, signature-compatible,
and the conditional install is already nil-safe. Task 6 found it but could not
build a test that fails without it at its own scope, and correctly declined to
ship an unpinned change to a working D2/D4 path. **You have the test that gives
it a reason:** a stub whose fetch installs its NS set must answer from its own
upstreams immediately, without waiting for an unrelated zone edit. Write that
test, watch it fail against `a.resolver.Reload`, then change the line.

- [ ] **Step 4: Run, then commit**

Run:
```bash
go test -race ./internal/zones/ -run Stub -v
go test -race ./internal/zones/
```
Expected: PASS, with the hang test completing well inside its deadline.

Prove the glue guard: remove the in-zone check so the name is resolved, re-run
`TestStubFetchSkipsAnInZoneNameserverWithNoGlue`, and confirm it fails **by
assertion, naming the lookup, in well under a second** — the recording resolver
refuses the in-zone name, so the guard's absence is caught deterministically
rather than by waiting. That is the primary guard and it is the one this
mutation exercises.

Prove the backstop **separately**, because the two guards catch different
failures and neither substitutes for the other: temporarily inject a resolver
that blocks forever, and confirm the `select` fires at its own deadline —
inside the much longer context `Fetch` runs under — rather than hanging the
package. Restore both.

```bash
gofmt -l internal cmd
~/go/bin/golangci-lint run ./internal/zones/
git add internal/zones/stub.go internal/zones/stub_test.go internal/app
git commit -m "feat(zones): the stub fetcher

Two ordinary queries to the master -- SOA for the schedule, NS for the
delegation -- rather than an AXFR, so a stub needs no allow_transfer
permission on the far end. Both signed when the zone names a key, reusing
fetch's mechanism.

An in-zone nameserver is never resolved: its name ends with the apex, so
resolving it would route into this very zone and hang rather than fail. Its
address must come from glue or the nameserver is skipped, and a zone with
none keeps its claim and answers SERVFAIL. The test for that guard fails by
assertion, deterministically, which is the only reliable way to catch it.

Glue is installed alongside the NS records so the routing table can be
rebuilt on a zone reload without touching the network."
```

---

### Task 8: Scheduling a stub's refresh

**Files:**
- Modify: `internal/zones/refresh.go` — `isSecondary` widens, `transfer()` branches
- Test: `internal/zones/refresh_test.go` (append)

**Interfaces:**
- Consumes: `StubFetcher.Fetch` (Task 7).

> **THE ONE THING THIS TASK MUST NOT GET WRONG.** Task 7 added
> `zones.WithStubReload`, an option this plan's original `Produces` list did not
> anticipate. It exists because `StubFetcher` is a **separate object from
> `Transferrer` and does not inherit its reload** — without it a fetch installs
> its NS rows and nothing publishes them, so the stub claims its suffix against
> a routing table that has no addresses for it and SERVFAILs forever, looking
> exactly like a fetch that never happened.
>
> That is the same trap Task 7 just closed on the transfer path, arriving by a
> second route. **Wire it with `App.ReloadZones`, never `a.resolver.Reload`** —
> the resolver method rebuilds the zone snapshot without reinstalling the
> conditional routing table, which is precisely the bug. `App.ReloadZones`
> releases the resolver's `rmu` before taking `routeMu`, so the lock order is
> safe from the refresh path's per-zone `xfer` lock.
>
> Pin it: a stub whose fetch succeeds must have its suffix routable without any
> unrelated zone edit. If you cannot make that observable end to end — Task 7
> reports that glue carries an IP with no port, so a stub's upstreams are always
> `addr:53` and no loopback mock can be one — then pin the mechanism instead: the
> install must republish the routing table, not merely the snapshot.
- Produces: stub zones refresh on the same schedule secondaries do.

**Reuses `Refresher` rather than growing a second scheduler.** It already does per-zone locking, retry back-off, `last_error`/`last_attempt` recording and `refreshed_at` stamping. Two changes: the type gate admits stub, and `transfer()` picks the right worker.

**A stub does not expire (§9.11.8).** A secondary past its SOA expire stops answering; a stub keeps forwarding, because its NS set is routing information rather than data it serves, and an old-but-working nameserver beats a self-inflicted SERVFAIL. This is a deliberate divergence and it needs its own test — inheriting the secondary behaviour by accident is the likely failure.

- [ ] **Step 1: Write the failing tests**

```go
// internal/zones/refresh_test.go — append.

// A stub is scheduled by the same loop, on the SOA schedule its master gave
// it.
func TestRefreshDueFetchesAStubZone(t *testing.T) { /* ... */ }

// The divergence, stated as a test so it cannot be inherited away: a stub
// past its SOA expire keeps its NS set and keeps routing. A secondary in the
// same position stops answering.
func TestAStubDoesNotExpire(t *testing.T) {
	// Build a stub whose expires_at is in the past, with NS records
	// installed. Assert StubUpstreams still returns them, and that the zone
	// is still contributing to routing — the opposite of what
	// Zone.Serving does for a secondary.
}

// A failed fetch is recorded and retried like any other attempt, and the zone
// keeps serving the NS set it already had.
func TestStubFetchFailureKeepsThePreviousNSSet(t *testing.T) { /* ... */ }
```

> Write these out fully against `refresh_test.go`'s existing fixtures. `TestAStubDoesNotExpire` is the one that must not be skipped: it is the only thing standing between §9.11.8 and a future change that widens `Serving` to cover stubs "for consistency".

- [ ] **Step 2: Run and watch them fail**

Run: `go test ./internal/zones/ -run 'Stub|Refresh' -v`
Expected: FAIL — stub zones are skipped by the scheduler.

- [ ] **Step 3: Implement**

```go
// pullsFromAMaster reports whether z is a type this scheduler refreshes.
//
// A secondary pulls a whole zone by AXFR; a stub pulls only the apex SOA and
// NS by ordinary query. Both are "ask a master, on the SOA's schedule, and
// record how it went", which is what this scheduler is, so both belong in it
// rather than in two loops that would drift.
func pullsFromAMaster(z store.Zone) bool {
	switch strings.ToLower(z.Type) {
	case "secondary", "stub":
		return true
	}
	return false
}
```

`RefreshDue`'s `isSecondary` gate becomes `pullsFromAMaster`, and `transfer()` branches on the type to call either `Transferrer.Transfer` or `StubFetcher.Fetch`, funnelling both outcomes through the existing `noteSuccess`/`noteFailure` recording so a stub's failures show up on the zone exactly as a secondary's do.

**Leave `Zone.Serving` alone.** It is what makes an expired secondary answer SERVFAIL, and a stub must not be added to it — §9.11.8. Add a comment there saying so, because "secondary and stub both pull from a master" is exactly the reasoning that would later widen it by mistake.

- [ ] **Step 4: Run, then commit**

Run:
```bash
go test -race ./internal/zones/
```
Expected: PASS

Prove the expiry divergence: add stub to `Serving`'s type check, re-run `TestAStubDoesNotExpire`, confirm it fails, restore.

```bash
gofmt -l internal cmd
~/go/bin/golangci-lint run ./internal/zones/
git add internal/zones
git commit -m "feat(zones): schedule stub refreshes on the existing loop

isSecondary becomes pullsFromAMaster and transfer() branches, so a stub
inherits per-zone locking, retry backoff and outcome recording rather than
getting a second scheduler that would drift.

A stub deliberately does not expire: its NS set is routing information, not
data it serves, so an old-but-working nameserver beats a self-inflicted
SERVFAIL -- and if those nameservers really are gone, the forwarder's own
failure path returns SERVFAIL anyway. Zone.Serving is left alone, with a
comment, because widening it 'for consistency' is the likely regression."
```

---

### Task 9: Web — the two page shapes and the create row

**Files:**
- Modify: `web/src/pages/zones/detail.tsx`, `web/src/pages/zones/list.tsx`
- Modify: `web/src/api/types.ts`, `web/src/lib/zones.ts`
- Test: `web/src/pages/zones/detail.test.tsx`, `web/src/pages/zones/list.test.tsx`, `web/e2e/smoke.spec.ts`

**Design — read the artboard, do not improvise from this summary.** It is on disk at `.superpowers/sdd/2026-09-03-zones-milestone-d6/zone-detail-artboard.dc.html`. **Do not call `DesignSync`** — main-loop only, unreachable from a subagent. The artboard is the source of truth for layout, colour, copy and states; the lines below are orientation.

What it settles:

- **One row serves both types**, label switching: `FORWARD TO` for a forwarder, `MASTER` for a stub. Same 152px gutter as `TRANSFERS OUT` and `NOTIFY OUT`, read by default with a pencil.
- **Every band assuming authored data is dropped for both** — `soaBandDisplay: none`, no create-record row, no record filter, no allow-transfer, no notify row. A forwarder additionally drops the records grid (`recordsDisplay: forwarder ? 'none' : 'block'`); a stub keeps it, **read-only**.
- **A forwarder's page carries the consequence in words**: *"with every upstream unreachable, queries for it get SERVFAIL — they do not fall through to the default resolvers."* That line is what stops a short page reading as a broken one. It is not decoration.
- **A stub has three dated states**: `NS set fetched 26 minutes ago`; `no NS set yet` while the first fetch is outstanding; and on failure the error verbatim under `LAST FETCH · 12 MINUTES AGO` with `serving the NS set from 3 days ago`. Never a state without its date.
- **A stub can be refreshed on demand, a forwarder cannot** — `Refresh: secondary || stub`,
  straight from the artboard. A forwarder has no master and nothing to fetch. A stub can
  be exported, a forwarder cannot (`canExport: !forwarder`).
- **The API gate is widened in Task 8, not here.** `handleZoneRefresh`
  (`internal/api/zones_handlers.go:706`) answered `400 "only secondary zones are
  transferred"`, which would have made the Refresh action fail on every stub. Task 8
  widens it to `secondary || stub`. What is left for this task: a stub's refresh
  response carries `expires_at: 0`, because a stub deliberately does not expire
  (§9.11.8) — **the page must not render that as an expiry**, or every stub reads as
  having expired at the epoch.
- The list's create row offers all four types with one type-dependent field, and a **stub may name a TSIG key** (`needsTsig: secondary || stub`).

**Artboard sample data is not spec.** Placeholder strings have contradicted shipped behaviour in this project before. Render server text verbatim.

- [ ] **Step 1: Write the failing tests**

`detail.test.tsx`: a forwarder renders the `FORWARD TO` row, the SERVFAIL line, and **no** SOA band, records grid, create row, allow-transfer or notify row. A stub renders `MASTER`, its three dated states, and read-only NS records with no create row. Both are refused the record-edit affordances.

`list.test.tsx`: the create row offers four types; the extra field's label and placeholder switch per type; a stub offers the TSIG select and a forwarder does not.

`smoke.spec.ts`: create a forwarder zone through the UI, set its upstreams, and assert they persist across a reload.

- [ ] **Step 2: Run and watch them fail**

Run: `cd web && pnpm test zones`

- [ ] **Step 3: Implement**

`types.ts` gains `forward_to` on `Zone`. `detail.tsx` gains the shared upstream row and the per-type gating — note the current `isSecondary ? <TransferBand/> : <SoaBand/>` at `detail.tsx:2722` puts forwarder and stub into the **else** branch and would show them an editable SOA band; that is the line to change. `recordsReadOnly` widens from `isInternal || isSecondary` to include stub.

- [ ] **Step 4: Run, then commit**

Run: `cd web && pnpm lint && pnpm format:check && pnpm typecheck && pnpm test && pnpm build && pnpm test:e2e`

```bash
git add web
git commit -m "feat(web): forwarder and stub zone pages

A forwarder's page is short by design -- header, one row, zone actions --
and carries the SERVFAIL consequence in words, which is what stops it
reading as a page that failed to load rather than one that is complete.

A stub keeps its records grid, read-only, because a fetched NS set is worth
seeing, and shows three dated states including the stale-set case."
```

---

### Task 10: Docs

**Files:**
- Modify: `README.md`, `docs/api.md`, `docs/architecture.md`, `docs/configuration.md`

- [ ] **Step 1: Write them**

- `README.md`: the feature list gains conditional forwarding and stub zones.
- `docs/api.md`: `forward_to`'s format described **once**, referenced elsewhere — the rule `primaries` and `notify_to` already follow. The two new `type` values, which fields apply to which type, and that `primaries`/`tsig_key_id` now also apply to a stub.
- `docs/architecture.md`: why routing lives in the forwarder rather than the resolver (the cache sits between them); that a claimed suffix SERVFAILs rather than falling through, and why; the stub's two-query fetch and the glue rule, including that an in-zone nameserver is never resolved because it would route into its own zone; and that a stub does not expire, with the reasoning.
- `docs/configuration.md`: that `Conditional` has no settings key and a suffix is claimed by a zone row — correcting the impression §4 of the design spec left for two milestones.

- [ ] **Step 2: Check the tree**

Run: `grep -rn "forward_to" docs/ README.md`
Expected: the format described in exactly one place and referenced from the others.

- [ ] **Step 3: Commit**

```bash
git add README.md docs
git commit -m "docs: forwarder and stub zones, and why a claimed suffix SERVFAILs"
```

---

## Done when

- `go test -race ./...`, `~/go/bin/golangci-lint run ./...`, `gofmt -l internal cmd` clean on a committed tree.
- `cd web && pnpm lint && pnpm format:check && pnpm typecheck && pnpm test && pnpm build && pnpm test:e2e` clean.
- `primaries_test.go` and `notifyto_test.go` pass **unchanged** — the refactor moved no behaviour.
- A forwarder zone routes its suffix to its own upstreams, and those answers are cached; disabling it releases the suffix; naming no upstreams SERVFAILs rather than falling through.
- A stub fetches its NS set, installs it with glue, and routes there — and an in-zone nameserver with no glue is skipped, proven by a test whose primary guard fails by assertion (the resolver is never asked) with a timeout only as the backstop against a genuine hang.
- A stub past its SOA expire still routes, proven by a test that fails if stub is added to `Zone.Serving`.
