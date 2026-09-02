# Zones Milestone D4 — NOTIFY both directions Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A primary tells its secondaries the moment a zone changes, and a secondary told by its primary checks and transfers — in both directions, authenticated, with delivery state an operator can read.

**Architecture:** NOTIFY is an *opcode*, not a qtype, so `dnssrv.Server.serve` gains a third branch beside D3's transfer intercept and ahead of the pipeline — which also closes a live defect, since a NOTIFY today is handled as an ordinary SOA query and forwarded upstream. Inbound, `zones.NotifyServer` authorises against the zone's `primaries` plus TSIG, replies immediately, then probes the primary's SOA and transfers only when RFC 1982 says the serial advanced. Outbound, `zones.Notifier` drives a `zone_notifies` queue whose pending-ness is *derived* by comparing serials rather than stored, so no mutation path has a call site to forget.

**Tech Stack:** Go 1.26, `miekg/dns` v1.1.72, sqlite + Postgres, goose migrations, React + TanStack Query.

**Spec:** §9.10 of `docs/superpowers/specs/2026-08-08-zones-design.md`. Read it first — it records *why* each rule below is the rule, and every RFC 1996 sentence in it was fetched rather than recalled.

## Global Constraints

- **The next free migration number is `0012`.** Do not derive this from the directory listing: versions 5 and 7 are Go migrations claimed in `internal/store/migrate.go` (`zonemigrate.go`, `builtins.go`) with no SQL file, and a duplicate version fails goose at startup on both drivers. Write both `sqlite/` and `postgres/` variants. Postgres gets `BIGINT` where sqlite gets `INTEGER` for unix-ms and serial columns — see the note in `0010_zone_transfer_outcome.sql`.
- Store tests run under `forEachDriver` (`internal/store/store_test.go:38`); sqlite and a real Postgres container both execute. **No test in this plan makes a concurrency assertion, and if you add one, it must defeat `SetMaxOpenConns(1)` deliberately** — sqlite serialises individual statements, so a concurrency test written the obvious way passes without testing anything.
- **The Postgres container is shared across store tests, so fixed literal names collide.** A test that inserts a zone called `example.com` or a key called `ns2-xfer.` passes on sqlite (fresh temp file per test) and fails on Postgres once a second test uses the same literal. Use the existing `testGroupName(base)` helper (`internal/store/crud_test.go:13`), which suffixes a timestamp — the pattern this test suite already established for exactly this problem. The test bodies below show literals for readability; make them unique.
- **The TSIG key store's create method is `Create`, not `Add`** (`internal/store/tsigkeys.go:30`). Zone creation is `AddZone`; the two differ, and the store interfaces are the authority.
- **The `internal/api` test harness is not what the test bodies below assume.** The real shapes, from `internal/api/zones_handlers_test.go`:
  - `newTestServer(t) *zoneTestServer` — **one** return value, not two.
  - `ts.do(t, method, path, body string) *httptest.ResponseRecorder` — the body is a **JSON string**, not a map.
  - `createdID(t, rec) int64` — POST answers `{"id": N}`, **not** a full zone body.
  - `ts.zone(t, id) store.Zone` — reads the row back from the store to assert on what a handler wrote. PATCH answers **204 with an empty body**, so this is how you check a patch landed.
  - There is no `decodeJSON`.

  Task 4's implementer hit this and adapted; any later task writing `internal/api` tests should write against these shapes from the start. Read that file before writing API tests.
- **`internal/zones` tests are external** (`package zones_test`). An unexported helper is unreachable from them — either export it or add an internal test file, following `batch_internal_test.go`'s precedent. This plan does both, in different places, deliberately.
- Gates on the **committed** tree, `git status --porcelain` empty: `go test -race ./...`, `~/go/bin/golangci-lint run ./...`, `gofmt -l internal cmd`, and for web tasks `cd web && pnpm lint && pnpm format:check && pnpm typecheck && pnpm test && pnpm build && pnpm test:e2e`.
- Every fix's test shown failing with the fix removed.
- **Time is injected, never read directly** — except a TSIG timestamp, which is checked against the *peer's* clock inside a fudge window and must be real wall time. `Transferrer.now`'s doc comment states the distinction; D4 follows it, and `Notifier` takes a `now` for the same reason `Refresher` does.
- **Disjoint column sets.** `updateZoneSQL` must bind `notify_to` (or a PATCH silently drops it) and must never bind any `zone_notifies` column. Within `zone_notifies`, `NoteDelivered` and `NoteAttempt` are the only writers of delivery state and each owns its own set. D2 and D3 both shipped a defect from exactly this rule; Task 3 mirrors D3's test rather than trusting it.
- **The artboard is on disk** at `.superpowers/sdd/2026-09-02-zones-milestone-d4/zone-detail-artboard.dc.html` (gitignored). Read it from there. Do **not** call `DesignSync` — it is main-loop only and unreachable from a subagent.

## File Structure

- `internal/zones/serial.go` — `SerialNewer`, RFC 1982 §3.2 circular comparison
- `internal/zones/notifyto.go` — the `notify_to` format: parse, format, key names
- `internal/store/migrations/{sqlite,postgres}/0012_zone_notify.sql` — `notify_to` + the `zone_notifies` table
- `internal/store/zones.go` — the `notify_to` column
- `internal/store/notifies.go` — `NotifyStore`: read, reconcile, and the two narrow writers
- `internal/store/tsigkeys.go` — the delete guard extended to `notify_to`
- `internal/api/zones_handlers.go` — `notify_to` on create and patch
- `internal/api/notifies_handlers.go` — `GET /zones/{id}/notifies` and the derived state
- `internal/dnssrv/notifies.go` — the `Notifies` interface and `WithNotifies`
- `internal/dnssrv/server.go` — the opcode branch
- `internal/zones/notifyserver.go` — the inbound gate, the throttle, the handoff
- `internal/zones/transfer.go` — `ProbeSerial`
- `internal/zones/notifier.go` — the outbound pass, reconciliation, rounds, sending
- `internal/app/app.go` — wiring
- `web/src/lib/notify.ts`, `web/src/pages/zones/detail.tsx`, `web/src/pages/tsig-keys.tsx` — the field, the roll-up, the usage count
- `docs/` — the format, the gate, the states

---

### Task 1: `SerialNewer` — RFC 1982, and the pair it leaves undefined

**Files:**
- Create: `internal/zones/serial.go`
- Test: `internal/zones/serial_test.go`

**Interfaces:**
- Produces:
  ```go
  func SerialNewer(a, b uint32) bool
  ```

**Why it is exported.** Both halves of D4 need it, and so does `internal/api`, which derives each notify target's `state` by asking whether the zone's serial has moved past what that target acknowledged. A package-private helper would be copied into the API layer within a release, and then there would be two serial comparisons that could disagree — which for a wrap is a bug that appears once every four billion edits and is unreproducible when it does.

- [ ] **Step 1: Write the failing tests**

```go
package zones_test

import (
	"testing"

	"github.com/aloks98/dnsaur/internal/zones"
)

// RFC 1982 §3.2 defines the comparison over a circle, so `a > b` is right
// everywhere except the wrap and wrong exactly there — the worst possible
// failure shape, since it works for years and then strands a zone at the top
// of the space. Each row below names which half of the definition it pins.
func TestSerialNewer(t *testing.T) {
	const half = uint32(1) << 31

	tests := []struct {
		name string
		a, b uint32
		want bool
	}{
		// The ordinary case, which a naive `a > b` also gets right.
		{"one ahead", 2, 1, true},
		{"one behind", 1, 2, false},
		{"equal is not newer", 7, 7, false},
		{"far ahead, still inside the half", 1000, 1, true},

		// The wrap. A serial that has just passed 2^32-1 is newer than one
		// that has not, and this is the whole reason the helper exists.
		{"zero is the successor of the maximum", 0, 4294967295, true},
		{"the maximum is not newer than zero", 4294967295, 0, false},
		{"just past the wrap", 5, 4294967290, true},
		{"just before the wrap", 4294967290, 5, false},

		// The boundary of the defined region: a difference of 2^31 - 1 is
		// the largest that still has an answer.
		{"largest defined forward distance", half - 1, 0, true},
		{"largest defined backward distance", 0, half - 1, false},

		// §3.2 leaves a pair exactly 2^31 apart undefined. The policy is
		// "not newer", in both directions, because the two mistakes are not
		// symmetric: a transfer that does not happen is recovered by the
		// refresh timer, and one that should not have happened is a full
		// AXFR of someone else's zone — and on the outbound side it would
		// notify every target on every pass, forever.
		{"exactly half the space is undefined, forward", half, 0, false},
		{"exactly half the space is undefined, backward", 0, half, false},
		{"undefined, away from the origin", half + 100, 100, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := zones.SerialNewer(tc.a, tc.b); got != tc.want {
				t.Errorf("SerialNewer(%d, %d) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

// A serial is never newer than itself, at any point in the space including
// the two that a subtraction is most likely to get wrong.
func TestSerialNewerIsIrreflexive(t *testing.T) {
	for _, s := range []uint32{0, 1, 1 << 31, 4294967295} {
		if zones.SerialNewer(s, s) {
			t.Errorf("SerialNewer(%d, %d) = true, want false", s, s)
		}
	}
}
```

- [ ] **Step 2: Run and watch it fail**

Run: `go test ./internal/zones/ -run TestSerialNewer -v`
Expected: FAIL — `undefined: zones.SerialNewer`

- [ ] **Step 3: Implement**

```go
package zones

// SerialNewer reports whether serial a is strictly newer than serial b under
// RFC 1982 §3.2's circular arithmetic.
//
// A DNS serial is a uint32 that wraps, so ordinary `>` is wrong at exactly
// one place and right everywhere else. §3.2 defines s1 to be greater than s2
// when the forward distance from s2 to s1 is less than half the space; in
// unsigned arithmetic that whole definition collapses to the subtraction
// below, which wraps for free.
//
// **The comparison is not total, and that is the RFC's own doing.** §3.2
// leaves two serials exactly 2^31 apart with no defined ordering — the
// forward and backward distances are equal, so neither is "closer". This
// returns false for that pair, in both directions.
//
// The policy is a choice between two unequal mistakes rather than a
// preference. A transfer that does not happen is recovered by the next
// refresh timer, at the cost of some staleness. A transfer that should not
// have happened is a full AXFR of someone else's zone — and on the outbound
// side, an undefined pair resolving to "newer" would mean every target is
// notified on every pass, with nothing to stop it, because the comparison
// that decides "there is news" would never come out false.
func SerialNewer(a, b uint32) bool {
	// Wraps by definition of uint32 subtraction: for a=0, b=4294967295 this
	// is 1, which is what makes 0 the successor of the maximum.
	d := a - b
	// d == 0 is equality, d == 1<<31 is §3.2's undefined pair, and everything
	// above it is a backward distance. Only the open interval is "newer".
	return d != 0 && d < 1<<31
}
```

- [ ] **Step 4: Run, then commit**

Run: `go test ./internal/zones/ -run TestSerialNewer -v`
Expected: PASS

Verify the test would catch a naive implementation: temporarily change the body to `return a > b`, re-run, and confirm the wrap rows fail. Restore it.

```bash
gofmt -l internal cmd
~/go/bin/golangci-lint run ./internal/zones/
git add internal/zones/serial.go internal/zones/serial_test.go
git commit -m "feat(zones): RFC 1982 serial comparison

Both halves of D4 compare serials, and so does the API's per-target state
derivation. A naive a > b is correct everywhere except the wrap, where it
strands a zone at the top of the space.

The pair RFC 1982 section 3.2 leaves undefined compares as not-newer: a
transfer that does not happen is recovered by the refresh timer, and one
that should not have happened is a full AXFR — which outbound would repeat
on every pass forever."
```

---

### Task 2: The `notify_to` format

**Files:**
- Create: `internal/zones/notifyto.go`
- Test: `internal/zones/notifyto_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces:
  ```go
  type NotifyTarget struct {
      Host string // as written — may be a hostname
      Port uint16
      Key  string // canonical TSIG name (lowercase, trailing dot); "" = send unsigned
  }
  func ValidateNotifyTo(s string) error
  func ParseNotifyTo(s string) ([]NotifyTarget, error)
  func FormatNotifyTo(ts []NotifyTarget) string
  func NotifyToKeys(s string) []string
  func (t NotifyTarget) Addr() string // "10.0.0.2:53", "[fd00::2]:53" — the queue's row identity
  ```

**Why this is neither `primaries.go` nor `acl.go` with different words.** It borrows from both, so a reader will assume it is a copy of one of them, and it is not. `ParseACL` is pure because it is matched against a socket address on every request. `ParsePrimaries` takes a context and a resolver because a primary named by hostname must be followed at transfer time. `ParseNotifyTo` needs **both properties, split**: it is pure, and resolution happens separately at send time (Task 9). That is forced by the queue rather than chosen — `Addr()` is the row identity in `zone_notifies`, and a parser that resolved would make the identity an address, so a hostname that moved would orphan its delivery history and start a new row every time it changed.

**Why the per-target key.** A dnsaur primary has no key of its own: `zones_handlers.go:87` refuses `tsig_key_id` on a primary zone, because that column means "the key a *secondary* signs its transfer requests with". Without a per-target key here, a dnsaur primary would send unsigned NOTIFYs to a dnsaur secondary whose zone has a key, and Task 6's gate would refuse them — dnsaur unable to notify itself through a configuration it fully supports.

- [ ] **Step 1: Write the failing tests**

```go
package zones_test

import (
	"strings"
	"testing"

	"github.com/aloks98/dnsaur/internal/zones"
)

func TestParseNotifyTo(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []zones.NotifyTarget
	}{
		{
			name: "empty means notify nobody",
			in:   "",
			want: nil,
		},
		{
			name: "a bare address takes the default port",
			in:   "10.0.0.2",
			want: []zones.NotifyTarget{{Host: "10.0.0.2", Port: 53}},
		},
		{
			name: "an explicit port is kept",
			in:   "10.0.0.2:5353",
			want: []zones.NotifyTarget{{Host: "10.0.0.2", Port: 5353}},
		},
		{
			name: "a hostname is stored as written, not resolved",
			in:   "ns2.example.com",
			want: []zones.NotifyTarget{{Host: "ns2.example.com", Port: 53}},
		},
		{
			name: "a bare IPv6 literal is the no-port form",
			in:   "fd00::2",
			want: []zones.NotifyTarget{{Host: "fd00::2", Port: 53}},
		},
		{
			name: "a bracketed IPv6 literal may carry a port",
			in:   "[fd00::2]:5353",
			want: []zones.NotifyTarget{{Host: "fd00::2", Port: 5353}},
		},
		{
			name: "a key is canonicalised, lowercase with a trailing dot",
			in:   "10.0.0.2 key:NS2-Xfer",
			want: []zones.NotifyTarget{{Host: "10.0.0.2", Port: 53, Key: "ns2-xfer."}},
		},
		{
			name: "host, port and key together",
			in:   "ns2.hel1.example.com:5353 key:hetzner-xfer",
			want: []zones.NotifyTarget{{Host: "ns2.hel1.example.com", Port: 5353, Key: "hetzner-xfer."}},
		},
		{
			name: "several entries, only some keyed",
			in:   "10.0.0.2 key:ns2-xfer, ns3.example.com:5353, 10.0.0.3",
			want: []zones.NotifyTarget{
				{Host: "10.0.0.2", Port: 53, Key: "ns2-xfer."},
				{Host: "ns3.example.com", Port: 5353},
				{Host: "10.0.0.3", Port: 53},
			},
		},
		{
			name: "whitespace around separators is tolerated",
			in:   "  10.0.0.2   ,   10.0.0.3  ",
			want: []zones.NotifyTarget{{Host: "10.0.0.2", Port: 53}, {Host: "10.0.0.3", Port: 53}},
		},
		{
			// Skipped rather than rejected, exactly as splitPrimaries and
			// ParseACL do: a trailing comma names no target, so there is
			// nothing to be wrong about. Unlike primaries, a list that is
			// only separators is *valid* here and means notify nobody —
			// primaries requires at least one entry because a secondary
			// with none cannot transfer, and a zone with no notify targets
			// is the ordinary case.
			name: "a trailing comma names no target",
			in:   "10.0.0.2,",
			want: []zones.NotifyTarget{{Host: "10.0.0.2", Port: 53}},
		},
		{
			name: "a list of only separators is valid and empty",
			in:   " , , ",
			want: nil,
		},
		{
			// A comma is *always* the entry separator and can never be part
			// of a key name, because ParseNotifyTo splits on it before any
			// entry is parsed. So this is two targets, not one malformed
			// one — surprising written down, and exactly the property
			// ParseACL already has.
			//
			// It is also why validNotifyKeyName still forbids ',' even
			// though one can never reach it: the check fails closed rather
			// than open if this splitting ever changes. acl.go's
			// validACLKeyName carries the same guard for the same reason.
			name: "a comma separates entries even where a key name looks split",
			in:   "10.0.0.2 key:ns,2",
			want: []zones.NotifyTarget{
				{Host: "10.0.0.2", Port: 53, Key: "ns."},
				{Host: "2", Port: 53},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := zones.ParseNotifyTo(tc.in)
			if err != nil {
				t.Fatalf("ParseNotifyTo(%q) failed: %v", tc.in, err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("ParseNotifyTo(%q) = %+v, want %+v", tc.in, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("entry %d = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestParseNotifyToRejects(t *testing.T) {
	tests := []struct {
		name string
		in   string
		// The substring the message must carry, so a caller reading a 400
		// learns which entry was wrong rather than that "something" was.
		contains string
	}{
		{"port zero", "10.0.0.2:0", "port"},
		{"port above the range", "10.0.0.2:70000", "port"},
		{"a non-numeric port", "10.0.0.2:dns", "port"},
		// These two are the only cases that reach validPrimaryHost. Anything
		// with an internal space is claimed by the key branch first, so a
		// multi-word input would exercise that branch instead while still
		// producing a message containing "host" — passing for the wrong
		// reason and leaving the host validator with no coverage at all.
		{"a host with a forbidden character", "a/b", "host"},
		{"a host with an empty label", "a..b", "host"},
		// Named for what it actually tests: the token after the host is not
		// a key, so the key branch rejects it before any host check.
		{"a second token that is not a key", "not a host", "key"},
		{"an empty key name", "10.0.0.2 key:", "key"},
		{"a key name with a space", "10.0.0.2 key:ns 2", "key"},
		{"a key name with a colon", "10.0.0.2 key:ns:2", "key"},
		{"two keys on one entry", "10.0.0.2 key:a key:b", "key"},
		{"a bare key with no host", "key:ns2-xfer", "host"},
		{"trailing junk after the key", "10.0.0.2 key:ns2 extra", "key"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := zones.ParseNotifyTo(tc.in)
			if err == nil {
				t.Fatalf("ParseNotifyTo(%q) succeeded, want an error", tc.in)
			}
			if !strings.Contains(err.Error(), tc.contains) {
				t.Errorf("error %q does not mention %q", err, tc.contains)
			}
			// ValidateNotifyTo is ParseNotifyTo's error alone — the two must
			// never disagree about what is writable, or the API would accept
			// a value the sender then cannot parse.
			if zones.ValidateNotifyTo(tc.in) == nil {
				t.Errorf("ValidateNotifyTo(%q) accepted what ParseNotifyTo rejected", tc.in)
			}
		})
	}
}

// The stored form is FormatNotifyTo's spelling rather than what was typed,
// which is what lets the tsig_keys delete guard match key:<name> in SQL
// exactly (see tsigKeyStore.Delete) instead of pattern-matching whitespace.
func TestFormatNotifyToIsCanonicalAndRoundTrips(t *testing.T) {
	tests := []struct{ in, want string }{
		{"", ""},
		{"10.0.0.2", "10.0.0.2:53"},
		{"10.0.0.2:5353", "10.0.0.2:5353"},
		{"  10.0.0.2   ,10.0.0.3 ", "10.0.0.2:53, 10.0.0.3:53"},
		{"10.0.0.2 key:NS2-Xfer", "10.0.0.2:53 key:ns2-xfer."},
		{"fd00::2", "[fd00::2]:53"},
		{"ns2.example.com key:k", "ns2.example.com:53 key:k."},
	}

	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			ts, err := zones.ParseNotifyTo(tc.in)
			if err != nil {
				t.Fatalf("ParseNotifyTo(%q) failed: %v", tc.in, err)
			}
			got := zones.FormatNotifyTo(ts)
			if got != tc.want {
				t.Fatalf("FormatNotifyTo = %q, want %q", got, tc.want)
			}
			// Canonical means a fixed point: re-parsing and re-formatting the
			// stored value must not move it, or a PATCH that changed nothing
			// would still rewrite the column.
			again, err := zones.ParseNotifyTo(got)
			if err != nil {
				t.Fatalf("re-parsing %q failed: %v", got, err)
			}
			if second := zones.FormatNotifyTo(again); second != got {
				t.Errorf("not a fixed point: %q then %q", got, second)
			}
		})
	}
}

// Addr is the queue's row identity, so it must be stable and must never
// include the key — re-keying a target keeps its delivery history rather
// than orphaning it and starting a new row.
func TestNotifyTargetAddr(t *testing.T) {
	tests := []struct{ in, want string }{
		{"10.0.0.2", "10.0.0.2:53"},
		{"10.0.0.2:5353", "10.0.0.2:5353"},
		{"10.0.0.2 key:ns2-xfer", "10.0.0.2:53"},
		{"fd00::2", "[fd00::2]:53"},
		{"[fd00::2]:5353 key:k", "[fd00::2]:5353"},
		{"ns2.example.com:5353", "ns2.example.com:5353"},
	}

	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			ts, err := zones.ParseNotifyTo(tc.in)
			if err != nil {
				t.Fatalf("ParseNotifyTo(%q) failed: %v", tc.in, err)
			}
			if got := ts[0].Addr(); got != tc.want {
				t.Errorf("Addr() = %q, want %q", got, tc.want)
			}
		})
	}
}

// The delete guard and the TSIG keys screen's usage count both read this.
func TestNotifyToKeys(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{"none", "10.0.0.2, 10.0.0.3", nil},
		{"one", "10.0.0.2 key:ns2-xfer", []string{"ns2-xfer."}},
		{
			"several, in order, canonical",
			"10.0.0.2 key:B, 10.0.0.3, ns4.example.com key:a",
			[]string{"b.", "a."},
		},
		{
			// Fails closed, mirroring ACLKeys: an unparseable stored value
			// names no key. Both callers — a write's validation and a key's
			// usage count — already have their own account of the error, and
			// this has nothing to add to it.
			"an unparseable value names none",
			"10.0.0.2 key:ns 2",
			nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := zones.NotifyToKeys(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("NotifyToKeys(%q) = %v, want %v", tc.in, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("key %d = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}
```

- [ ] **Step 2: Run and watch them fail**

Run: `go test ./internal/zones/ -run 'NotifyTo|NotifyTarget' -v`
Expected: FAIL — `undefined: zones.ParseNotifyTo`

- [ ] **Step 3: Implement**

```go
package zones

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/miekg/dns"
)

// The `notify_to` format: who this zone tells when it changes.
//
// A comma-separated list, whitespace tolerated, where each entry is a host
// with an optional port and an optional TSIG key:
//
//	10.0.0.2                              default port, unsigned
//	10.0.0.2:5353                         explicit port, unsigned
//	ns2.example.com key:ns2-xfer          resolved at send time, signed
//	[fd00::2]:5353 key:hetzner-xfer       an IPv6 literal takes brackets with a port
//
// Empty means notify nobody, and that is what every zone is created with.
//
// **This is deliberately neither primaries.go nor acl.go.** ParseACL is pure
// because an ACL is matched against a socket address on every request, and a
// hostname there would put a DNS lookup inside the gate of the server that
// answers DNS. ParsePrimaries resolves because a primary named by hostname
// must be followed at transfer time. This needs both properties, split: the
// parse is pure, and resolution happens at send time in notifier.go.
//
// The split is forced rather than chosen. NotifyTarget.Addr is the row
// identity in zone_notifies, so a parser that resolved would make that
// identity an address — and a hostname whose address changed would orphan its
// delivery history and start a new row every time it moved.
//
// The key is per target rather than per zone because a primary has none of
// its own: zones.tsig_key_id is refused on a primary zone (it means "the key
// a secondary signs its transfer requests with"), so without this a dnsaur
// primary could not sign a NOTIFY to a dnsaur secondary that requires one.

const notifyKeyPrefix = "key:"

// NotifyTarget is one parsed entry.
type NotifyTarget struct {
	// Host is as written and may be a hostname; it is resolved at send time.
	Host string
	Port uint16
	// Key is the canonical TSIG name (lowercase, trailing dot) this target's
	// NOTIFY is signed under, or "" to send unsigned.
	Key string
}

// Addr is the target's host and port in dial form, and is the identity
// zone_notifies keys a row on. It deliberately excludes the key, so
// re-keying a target keeps its delivery history rather than orphaning it.
func (t NotifyTarget) Addr() string {
	return net.JoinHostPort(t.Host, strconv.Itoa(int(t.Port)))
}

// ValidateNotifyTo reports whether s is a well-formed notify_to list. Use it
// at write time. An empty list is valid and means notify nobody.
func ValidateNotifyTo(s string) error {
	_, err := ParseNotifyTo(s)
	return err
}

// ParseNotifyTo parses s into the targets a NOTIFY is sent to.
func ParseNotifyTo(s string) ([]NotifyTarget, error) {
	var out []NotifyTarget
	for _, field := range strings.Split(s, ",") {
		// Skipped rather than rejected, exactly as splitPrimaries and
		// ParseACL do: a trailing comma names no target.
		//
		// Unlike splitPrimaries there is no "at least one" check at the end.
		// A secondary with no primaries cannot transfer, so an empty list
		// there is a broken zone; a zone with no notify targets is the
		// ordinary case and the default.
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		t, err := parseNotifyTarget(field)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, nil
}

func parseNotifyTarget(field string) (NotifyTarget, error) {
	// The key is a suffix on the entry, so it comes off before the host is
	// looked at — otherwise SplitHostPort would see the whole string.
	hostPart := field
	key := ""
	if i := strings.IndexAny(field, " \t"); i >= 0 {
		hostPart = strings.TrimSpace(field[:i])
		rest := strings.TrimSpace(field[i:])
		// Exactly one trailing token, and it must be the key. Anything else
		// is refused rather than ignored: a second token is either a typo or
		// a syntax this format does not have, and silently dropping it would
		// store a target the operator believes is signed and is not.
		if !strings.HasPrefix(strings.ToLower(rest), notifyKeyPrefix) {
			return NotifyTarget{}, fmt.Errorf("notify target %q: expected `key:<name>` after the host", field)
		}
		name := strings.TrimSpace(rest[len(notifyKeyPrefix):])
		if strings.ContainsAny(name, " \t") {
			return NotifyTarget{}, fmt.Errorf("notify target %q: only one `key:<name>` is allowed, and it must be the last token", field)
		}
		if !validNotifyKeyName(name) {
			return NotifyTarget{}, fmt.Errorf("notify target %q: key name must be a domain name", field)
		}
		key = dns.CanonicalName(name)
	}

	// A field that is only a key names no target. It has to be caught here,
	// before SplitHostPort, because that function reads "key:ns2-xfer" as
	// host "key" with port "ns2-xfer" and returns no error at all — so
	// without this the operator's typo is reported as a bad port number,
	// which is both wrong and unactionable. (Verified against net's actual
	// behaviour, not assumed.)
	if strings.HasPrefix(strings.ToLower(hostPart), notifyKeyPrefix) {
		return NotifyTarget{}, fmt.Errorf("notify target %q: needs a host before the key", field)
	}

	host, portStr, err := net.SplitHostPort(hostPart)
	if err != nil {
		// Same two legal shapes parsePrimary documents: "missing port in
		// address" for a bare host, "too many colons in address" for a bare
		// IPv6 literal. Both are the no-port form.
		var addrErr *net.AddrError
		if !errors.As(err, &addrErr) {
			return NotifyTarget{}, fmt.Errorf("notify target %q: %w", field, err)
		}
		host, portStr = hostPart, ""
	}

	port := uint16(DefaultPrimaryPort)
	if portStr != "" {
		n, err := strconv.ParseUint(portStr, 10, 16)
		if err != nil || n == 0 {
			return NotifyTarget{}, fmt.Errorf("notify target %q: port must be between 1 and 65535", field)
		}
		port = uint16(n)
	}
	// validPrimaryHost is reused rather than copied: the question is
	// identical (an IP literal or a domain name, with dns.IsDomainName's
	// extreme liberality guarded), and two spellings of one rule is how they
	// come to disagree.
	if !validPrimaryHost(host) {
		return NotifyTarget{}, fmt.Errorf("notify target %q: host must be an IP address or a domain name", field)
	}
	return NotifyTarget{Host: host, Port: port, Key: key}, nil
}

// validNotifyKeyName mirrors validACLKeyName exactly, including that ',' and
// ':' are refused: they are this format's own delimiters, so a name carrying
// either would be indistinguishable from one. The ',' cannot actually reach
// here — ParseNotifyTo splits on it first — and stays in the set so this
// fails closed rather than open if that splitting ever changes.
func validNotifyKeyName(name string) bool {
	if name == "" || strings.ContainsAny(name, " \t\r\n/\\,:") {
		return false
	}
	for _, label := range strings.Split(strings.TrimSuffix(name, "."), ".") {
		if label == "" {
			return false
		}
	}
	_, ok := dns.IsDomainName(name)
	return ok
}

// FormatNotifyTo writes targets back in the spelling ParseNotifyTo reads. The
// API stores this canonical form rather than what was typed, which is what
// lets the tsig_keys delete guard match a key name in SQL exactly (see
// tsigKeyStore.Delete) instead of pattern-matching around whitespace.
//
// The port is always written, even when it is the default. Unlike FormatACL's
// bare-address case there is nothing to gain by hiding it — the value is a
// dial target, the port is part of it, and Addr has to produce the same
// string either way.
func FormatNotifyTo(ts []NotifyTarget) string {
	parts := make([]string, 0, len(ts))
	for _, t := range ts {
		s := t.Addr()
		if t.Key != "" {
			s += " " + notifyKeyPrefix + t.Key
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, ", ")
}

// NotifyToKeys returns the canonical TSIG key names s names, in order. An
// unparseable value names none, mirroring ACLKeys: callers use this to
// validate a write and to count a key's usage, and neither has anything to do
// about an error here that its own caller is not already doing.
func NotifyToKeys(s string) []string {
	ts, err := ParseNotifyTo(s)
	if err != nil {
		return nil
	}
	var out []string
	for _, t := range ts {
		if t.Key != "" {
			out = append(out, t.Key)
		}
	}
	return out
}
```

- [ ] **Step 4: Run, then commit**

Run: `go test ./internal/zones/ -run 'NotifyTo|NotifyTarget' -v`
Expected: PASS

```bash
gofmt -l internal cmd
~/go/bin/golangci-lint run ./internal/zones/
git add internal/zones/notifyto.go internal/zones/notifyto_test.go
git commit -m "feat(zones): the notify_to format

host[:port] [key:name], comma-separated, empty meaning notify nobody.

Pure like ParseACL and resolved-later like ParsePrimaries, which is forced
rather than chosen: NotifyTarget.Addr is the queue's row identity, so a
parser that resolved would make that identity an address and a hostname
that moved would orphan its delivery history.

The key is per target because a primary has no tsig_key_id of its own --
that column is refused on primary zones -- so without one dnsaur could not
sign a NOTIFY to a dnsaur secondary that requires a signature."
```

---

### Task 3: Migration 0012, the column, the queue store, and the extended key guard

**Files:**
- Create: `internal/store/migrations/sqlite/0012_zone_notify.sql`
- Create: `internal/store/migrations/postgres/0012_zone_notify.sql`
- Create: `internal/store/notifies.go`
- Modify: `internal/store/store.go` — `Notifies() NotifyStore` on the `Store` interface
- Modify: `internal/store/sql.go` — the accessor
- Modify: `internal/store/zones.go` — `notify_to` on `Zone`, `zoneColumns`, `scanZone`, `addZoneSQL`, `updateZoneSQL`
- Modify: `internal/store/tsigkeys.go` — `notifyKeyRef`, and the guard
- Test: `internal/store/notifies_test.go`, and additions to `internal/store/zones_test.go` and `internal/store/tsigkeys_test.go`

**Interfaces:**
- Consumes: `zones.NotifyToKeys` (Task 2) — only in the API layer, not here; the store matches key names in SQL.
- Produces:
  ```go
  // store.Zone gains:
  NotifyTo string `json:"notify_to"`

  // No JSON tags: nothing marshals this type directly. Task 11's route
  // builds its own DTO, because the wire shape carries a derived `state`
  // and `max_attempts` that are not columns — so tags here would be a
  // second, drifting description of a contract that lives there.
  type ZoneNotify struct {
      ID             int64
      ZoneID         int64
      Target         string
      PendingSerial  uint32
      NotifiedSerial uint32
      NotifiedAt     int64
      Attempts       int
      NextAttemptAt  int64
      LastError      string
      CreatedAt      int64
  }

  type NotifyStore interface {
      All(ctx context.Context) ([]ZoneNotify, error)
      ByZone(ctx context.Context, zoneID int64) ([]ZoneNotify, error)
      Reconcile(ctx context.Context, zoneID int64, targets []string, now int64) error
      NoteDelivered(ctx context.Context, id int64, serial uint32, at int64) error
      NoteAttempt(ctx context.Context, id int64, pendingSerial uint32, attempts int, nextAttemptAt int64, errText string) error
  }
  // Store gains: Notifies() NotifyStore
  ```

**Why the table takes a real foreign key when `zones.tsig_key_id` did not.** Migration 0009 declined one, and its reasoning does not transfer: that column is `NOT NULL DEFAULT 0` where 0 means "no key", a FK skips NULL rather than zero, and making it nullable is a whole-table rebuild on SQLite. None of that applies to `zone_id`, which has no zero-means-none case — and `zone_records` already sets the `ON DELETE CASCADE` precedent (`0004_zones.sql:50`), so deleting a zone needs no application code at all.

**Why pending-ness is not a column.** The serial already is one. A row records only what was *achieved*; whether there is work is derived by comparing it against the zone. That is what makes the trigger self-healing — see Task 8, and §9.10.6.

**Two narrow writers, not one wide one.** `NoteDelivered` and `NoteAttempt` own disjoint column sets, for the reason `NoteTransferAttempt`'s doc comment gives at length: a whole-row write binds every column from a struct read some time earlier, so it silently reverts concurrent edits. `Reconcile` is the only other writer and touches neither.

- [ ] **Step 1: Write the failing tests**

```go
// internal/store/notifies_test.go
package store

import (
	"context"
	"testing"
)

// seedNotifyZone inserts a primary zone and returns its id. Every test here
// needs one, because zone_notifies.zone_id is a real foreign key.
func seedNotifyZone(t *testing.T, s Store, name string) int64 {
	t.Helper()
	id, err := s.Zones().AddZone(context.Background(), Zone{
		Name: name, Type: "primary", Enabled: true,
		SOANS: "ns1." + name, SOAMbox: "hostmaster." + name,
		SOASerial: 1, SOARefresh: 3600, SOARetry: 600,
		SOAExpire: 604800, SOAMinimum: 300, SOATTL: 900,
	})
	if err != nil {
		t.Fatalf("AddZone: %v", err)
	}
	return id
}

func TestNotifyReconcileCreatesAndRemoves(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		zoneID := seedNotifyZone(t, s, "example.com")
		ns := s.Notifies()

		// Creating: two targets, neither with a row yet.
		if err := ns.Reconcile(ctx, zoneID, []string{"10.0.0.2:53", "10.0.0.3:53"}, 1000); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		rows, err := ns.ByZone(ctx, zoneID)
		if err != nil {
			t.Fatalf("ByZone: %v", err)
		}
		if len(rows) != 2 {
			t.Fatalf("got %d rows, want 2", len(rows))
		}
		for _, r := range rows {
			// created_at is what dates a target that has never been notified;
			// without it the screen's "added 2m ago" has nothing to read and
			// a never-notified row shows a blank cell instead of an answer.
			if r.CreatedAt != 1000 {
				t.Errorf("target %s: created_at = %d, want 1000", r.Target, r.CreatedAt)
			}
			if r.NotifiedAt != 0 {
				t.Errorf("target %s: notified_at = %d, want 0 (never)", r.Target, r.NotifiedAt)
			}
		}

		// Idempotent: reconciling the same list again must not duplicate a
		// row or restamp created_at, or every pass would reset the age of
		// every target.
		if err := ns.Reconcile(ctx, zoneID, []string{"10.0.0.2:53", "10.0.0.3:53"}, 2000); err != nil {
			t.Fatalf("second Reconcile: %v", err)
		}
		rows, _ = ns.ByZone(ctx, zoneID)
		if len(rows) != 2 {
			t.Fatalf("after re-reconcile got %d rows, want 2", len(rows))
		}
		for _, r := range rows {
			if r.CreatedAt != 1000 {
				t.Errorf("target %s: created_at moved to %d", r.Target, r.CreatedAt)
			}
		}

		// Removing: a target that left notify_to loses its row, and a new
		// one gains one, in the same call.
		if err := ns.Reconcile(ctx, zoneID, []string{"10.0.0.3:53", "ns4.example.com:5353"}, 3000); err != nil {
			t.Fatalf("third Reconcile: %v", err)
		}
		rows, _ = ns.ByZone(ctx, zoneID)
		got := map[string]int64{}
		for _, r := range rows {
			got[r.Target] = r.CreatedAt
		}
		if len(got) != 2 {
			t.Fatalf("got %v, want two targets", got)
		}
		if _, ok := got["10.0.0.2:53"]; ok {
			t.Error("10.0.0.2:53 left notify_to but kept its row")
		}
		if got["10.0.0.3:53"] != 1000 {
			t.Errorf("10.0.0.3:53 created_at = %d, want the original 1000", got["10.0.0.3:53"])
		}
		if got["ns4.example.com:5353"] != 3000 {
			t.Errorf("new target created_at = %d, want 3000", got["ns4.example.com:5353"])
		}

		// An empty list clears every row: a zone whose notify_to was emptied
		// notifies nobody and should carry no delivery state either.
		if err := ns.Reconcile(ctx, zoneID, nil, 4000); err != nil {
			t.Fatalf("clearing Reconcile: %v", err)
		}
		rows, _ = ns.ByZone(ctx, zoneID)
		if len(rows) != 0 {
			t.Fatalf("got %d rows after clearing, want 0", len(rows))
		}
	})
}

func TestNotifyWritersOwnDisjointColumns(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		zoneID := seedNotifyZone(t, s, "example.com")
		ns := s.Notifies()
		if err := ns.Reconcile(ctx, zoneID, []string{"10.0.0.2:53"}, 1000); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		rows, _ := ns.ByZone(ctx, zoneID)
		id := rows[0].ID

		// A failed attempt records the round, the count, the retry time and
		// the reason — and must not touch the delivered columns, or a target
		// that has been current for a week would read as never notified the
		// moment one retry failed.
		if err := ns.NoteAttempt(ctx, id, 47, 2, 5000, "i/o timeout"); err != nil {
			t.Fatalf("NoteAttempt: %v", err)
		}
		rows, _ = ns.ByZone(ctx, zoneID)
		r := rows[0]
		if r.PendingSerial != 47 || r.Attempts != 2 || r.NextAttemptAt != 5000 || r.LastError != "i/o timeout" {
			t.Fatalf("NoteAttempt wrote %+v", r)
		}
		if r.NotifiedSerial != 0 || r.NotifiedAt != 0 {
			t.Errorf("NoteAttempt touched the delivered columns: %+v", r)
		}
		if r.CreatedAt != 1000 {
			t.Errorf("NoteAttempt moved created_at to %d", r.CreatedAt)
		}

		// A success records delivery and clears the retry state, so a target
		// that recovered stops reporting an error it no longer has — the same
		// rule NoteTransferAttempt follows for last_error.
		if err := ns.NoteDelivered(ctx, id, 47, 6000); err != nil {
			t.Fatalf("NoteDelivered: %v", err)
		}
		rows, _ = ns.ByZone(ctx, zoneID)
		r = rows[0]
		if r.NotifiedSerial != 47 || r.NotifiedAt != 6000 {
			t.Fatalf("NoteDelivered wrote %+v", r)
		}
		if r.Attempts != 0 || r.LastError != "" || r.NextAttemptAt != 0 {
			t.Errorf("NoteDelivered left retry state behind: %+v", r)
		}
		if r.CreatedAt != 1000 {
			t.Errorf("NoteDelivered moved created_at to %d", r.CreatedAt)
		}
	})
}

// zone_id is a real foreign key with ON DELETE CASCADE, so deleting a zone
// takes its queue rows with it and no application code has to remember.
func TestNotifyRowsCascadeOnZoneDelete(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		zoneID := seedNotifyZone(t, s, "example.com")
		other := seedNotifyZone(t, s, "other.example")
		ns := s.Notifies()
		if err := ns.Reconcile(ctx, zoneID, []string{"10.0.0.2:53"}, 1000); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		if err := ns.Reconcile(ctx, other, []string{"10.0.0.9:53"}, 1000); err != nil {
			t.Fatalf("Reconcile other: %v", err)
		}

		if err := s.Zones().DeleteZone(ctx, zoneID); err != nil {
			t.Fatalf("DeleteZone: %v", err)
		}
		all, err := ns.All(ctx)
		if err != nil {
			t.Fatalf("All: %v", err)
		}
		for _, r := range all {
			if r.ZoneID == zoneID {
				t.Fatalf("row for the deleted zone survived: %+v", r)
			}
		}
		if len(all) != 1 {
			t.Fatalf("got %d rows, want only the other zone's", len(all))
		}
	})
}
```

```go
// internal/store/zones_test.go — append.

// updateZoneSQL binds every configuration column, and notify_to is one, so a
// PATCH that changes it has to persist. This is the positive half of the
// disjoint-column-sets rule; the negative half is that it never binds
// zone_notifies state, which it cannot, since that is a different table.
func TestUpdateZonePersistsNotifyTo(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		id := seedNotifyZone(t, s, "example.com")

		z, err := s.Zones().Zone(ctx, id)
		if err != nil {
			t.Fatalf("Zone: %v", err)
		}
		if z.NotifyTo != "" {
			t.Fatalf("a new zone has notify_to = %q, want empty", z.NotifyTo)
		}
		z.NotifyTo = "10.0.0.2:53 key:ns2-xfer., 10.0.0.3:53"
		if err := s.Zones().UpdateZone(ctx, z); err != nil {
			t.Fatalf("UpdateZone: %v", err)
		}
		got, err := s.Zones().Zone(ctx, id)
		if err != nil {
			t.Fatalf("Zone after update: %v", err)
		}
		if got.NotifyTo != z.NotifyTo {
			t.Errorf("notify_to = %q, want %q", got.NotifyTo, z.NotifyTo)
		}
	})
}
```

```go
// internal/store/tsigkeys_test.go — append.

// D3 extended the delete guard from tsig_key_id to key: references in
// allow_transfer. notify_to is a third reference, and without it deleting a
// key silently downgrades a signed NOTIFY to an unsigned one that the peer
// then refuses — a failure that surfaces nowhere near the delete that caused
// it.
func TestDeleteTSIGKeyRefusedWhileNotifyToNamesIt(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		keyID, err := s.TSIGKeys().Create(ctx, TSIGKey{
			Name: "ns2-xfer.", Algorithm: "hmac-sha256.", Secret: "c2VjcmV0",
		})
		if err != nil {
			t.Fatalf("Add: %v", err)
		}
		zoneID := seedNotifyZone(t, s, "example.com")
		z, _ := s.Zones().Zone(ctx, zoneID)
		z.NotifyTo = "10.0.0.2:53 key:ns2-xfer."
		if err := s.Zones().UpdateZone(ctx, z); err != nil {
			t.Fatalf("UpdateZone: %v", err)
		}

		if err := s.TSIGKeys().Delete(ctx, keyID); !errors.Is(err, ErrInUse) {
			t.Fatalf("Delete = %v, want ErrInUse", err)
		}

		// And it is released once the reference goes, rather than being
		// permanently undeletable.
		z.NotifyTo = ""
		if err := s.Zones().UpdateZone(ctx, z); err != nil {
			t.Fatalf("clearing UpdateZone: %v", err)
		}
		if err := s.TSIGKeys().Delete(ctx, keyID); err != nil {
			t.Fatalf("Delete after clearing = %v, want nil", err)
		}
	})
}

// The guard matches whole names between delimiters, so a key whose name is a
// substring of another's is not held hostage by it. The same rule aclKeyRef
// already documents, applied to the second column.
func TestDeleteTSIGKeyNotifyToMatchesWholeNamesOnly(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		shortID, err := s.TSIGKeys().Create(ctx, TSIGKey{
			Name: "ns2.", Algorithm: "hmac-sha256.", Secret: "c2VjcmV0",
		})
		if err != nil {
			t.Fatalf("Add: %v", err)
		}
		if _, err := s.TSIGKeys().Create(ctx, TSIGKey{
			Name: "ns2-xfer.", Algorithm: "hmac-sha256.", Secret: "c2VjcmV0",
		}); err != nil {
			t.Fatalf("Add long: %v", err)
		}
		zoneID := seedNotifyZone(t, s, "example.com")
		z, _ := s.Zones().Zone(ctx, zoneID)
		z.NotifyTo = "10.0.0.2:53 key:ns2-xfer."
		if err := s.Zones().UpdateZone(ctx, z); err != nil {
			t.Fatalf("UpdateZone: %v", err)
		}

		// Only ns2-xfer. is referenced. ns2. is a prefix of it and must
		// still be deletable.
		if err := s.TSIGKeys().Delete(ctx, shortID); err != nil {
			t.Fatalf("Delete(ns2.) = %v, want nil", err)
		}
	})
}
```

- [ ] **Step 2: Run and watch them fail**

Run: `go test ./internal/store/ -run 'Notify|NotifyTo' -v`
Expected: FAIL — `s.Notifies undefined`, `z.NotifyTo undefined`

- [ ] **Step 3: Implement**

`internal/store/migrations/sqlite/0012_zone_notify.sql`:

```sql
-- +goose Up
-- Telling secondaries a zone changed: who to tell, and how each of them went.
--
-- notify_to is the target list, default empty, which means notify nobody --
-- what every zone that exists before this migration gets. Turning on an
-- outbound path and pointing every zone down it in the same release would
-- start sending traffic nobody asked for.
--
-- Its format is a comma-separated list of host[:port] [key:<tsig name>],
-- parsed by zones.ParseNotifyTo and stored in that package's canonical
-- spelling rather than as typed. The canonical form is load-bearing for the
-- same reason allow_transfer's is: tsigKeyStore.Delete matches a key name
-- inside this column in SQL, and it can only do that because the separator
-- and the name spelling are known.
--
-- The key is per target rather than per zone. zones.tsig_key_id means "the
-- key a secondary signs its transfer requests with" and is refused on a
-- primary, so a primary has no key of its own to sign a NOTIFY with -- and
-- a dnsaur secondary whose zone names a key refuses an unsigned one.
ALTER TABLE zones ADD COLUMN notify_to TEXT NOT NULL DEFAULT '';

-- One row per (zone, target): the delivery state of one target, and the
-- queue entry for it, which are the same thing.
--
-- **There is no `pending` column, and that is the design.** The serial
-- already is one: a row records what was *achieved*, and whether there is
-- work is derived by comparing notified_serial against the zone's
-- soa_serial. That is what makes the trigger self-healing -- any write that
-- advances a serial is picked up by the next pass, through the API, an
-- import, auto-PTR, a transfer install, or a path nobody has thought of yet,
-- with no call site for a future mutation to forget.
--
-- target is host:port as *written* -- 'ns2.example.com:5353', not a resolved
-- address -- and carries no key. Both are deliberate. A hostname is resolved
-- at send time so a target that moves is followed, and if the identity were
-- the resolved address a target that moved would orphan its history and
-- start a new row; the key is read from the zone's current notify_to at send
-- time, so re-keying a target keeps its history rather than orphaning it.
--
-- pending_serial is the round currently being attempted, notified_serial and
-- notified_at the last round that landed (notified_at 0 = never), attempts
-- the count within the current round, and last_error the most recent
-- failure, cleared on success so a target that recovered stops reporting a
-- problem it no longer has.
--
-- created_at exists for one line on screen: a target that has never been
-- notified has no notify date, so it is dated by when it was added. Without
-- it the only honest rendering is a blank cell, and a blank cell in a row
-- whose whole point is "nothing has happened yet" reads as missing data
-- rather than as the answer.
--
-- Who writes them: only zones.Notifier, through NoteDelivered and
-- NoteAttempt, two UPDATEs with disjoint column sets -- the same rule, for
-- the same reason, as 0010 and 0011. Reconcile is the only other writer and
-- touches neither set.
--
-- Unlike zones.tsig_key_id this takes a real foreign key. 0009 declined one
-- there because that column is NOT NULL DEFAULT 0 where 0 means "no key" and
-- a foreign key skips NULL rather than zero; zone_id has no zero-means-none
-- case, and zone_records already sets the ON DELETE CASCADE precedent, so a
-- deleted zone takes its queue rows with it and no application code has to.
CREATE TABLE zone_notifies (
  id              INTEGER PRIMARY KEY,
  zone_id         INTEGER NOT NULL REFERENCES zones(id) ON DELETE CASCADE,
  target          TEXT    NOT NULL,
  pending_serial  INTEGER NOT NULL DEFAULT 0,
  notified_serial INTEGER NOT NULL DEFAULT 0,
  notified_at     INTEGER NOT NULL DEFAULT 0,
  attempts        INTEGER NOT NULL DEFAULT 0,
  next_attempt_at INTEGER NOT NULL DEFAULT 0,
  last_error      TEXT    NOT NULL DEFAULT '',
  created_at      INTEGER NOT NULL DEFAULT 0,
  UNIQUE(zone_id, target)
);
```

`internal/store/migrations/postgres/0012_zone_notify.sql` is the same file with the same comment, `BIGSERIAL PRIMARY KEY` for `id`, and `BIGINT` for `zone_id`, `notified_at`, `next_attempt_at`, `created_at`, `pending_serial` and `notified_serial` — a serial is a uint32 and does not fit a signed `INTEGER`'s positive range at the top of the space, which is exactly the region `SerialNewer` exists for.

`internal/store/notifies.go`:

```go
package store

import "context"

// ZoneNotify is one target's delivery state, which is also its queue entry:
// the two are the same row because pending-ness is derived from the serial
// rather than stored. See the 0012 migration.
type ZoneNotify struct {
	ID     int64
	ZoneID int64
	// Target is host:port as written — 'ns2.example.com:5353' — and carries
	// no key. It is the row identity; see the migration for why it is not a
	// resolved address.
	Target string
	// PendingSerial is the round currently being attempted.
	PendingSerial uint32
	// NotifiedSerial and NotifiedAt are the last round that landed.
	// NotifiedAt is unix ms; 0 = never delivered.
	NotifiedSerial uint32
	NotifiedAt     int64
	// Attempts is the count within the current round, reset on success.
	Attempts      int
	NextAttemptAt int64
	// LastError is the most recent failure, cleared on success.
	LastError string
	// CreatedAt is when this target was first seen, and never moves after.
	// It dates a target that has never been notified.
	CreatedAt int64
}

// NotifyStore manages the outbound NOTIFY queue.
type NotifyStore interface {
	// All returns every row, for the notifier's pass.
	All(ctx context.Context) ([]ZoneNotify, error)
	ByZone(ctx context.Context, zoneID int64) ([]ZoneNotify, error)
	// Reconcile makes the rows for zoneID exactly targets: it inserts any
	// that have no row, stamping created_at with now, and deletes any whose
	// target is not in the list. Existing rows are left alone — re-running it
	// must not restamp created_at, or every pass would reset the age of every
	// target.
	Reconcile(ctx context.Context, zoneID int64, targets []string, now int64) error
	// NoteDelivered records a round that landed: the serial the target
	// acknowledged and when. It clears attempts, next_attempt_at and
	// last_error, so a target that recovered stops reporting a problem it no
	// longer has — the rule NoteTransferAttempt follows for last_error.
	//
	// Its column set is disjoint from NoteAttempt's by design; see that
	// method, and NoteTransferAttempt's doc comment for the whole reasoning.
	NoteDelivered(ctx context.Context, id int64, serial uint32, at int64) error
	// NoteAttempt records a round that did not land: which round, how many
	// attempts it has had, when the next one may be made, and why the last
	// one failed. It never touches notified_serial or notified_at — a target
	// current for a week must not read as never notified because one retry
	// timed out.
	NoteAttempt(ctx context.Context, id int64, pendingSerial uint32, attempts int, nextAttemptAt int64, errText string) error
}

type notifyStore struct{ s *sqlStore }

const notifyColumns = `id, zone_id, target, pending_serial, notified_serial, notified_at, attempts, next_attempt_at, last_error, created_at`

func scanNotify(rows interface{ Scan(...any) error }, n *ZoneNotify) error {
	return rows.Scan(&n.ID, &n.ZoneID, &n.Target, &n.PendingSerial, &n.NotifiedSerial,
		&n.NotifiedAt, &n.Attempts, &n.NextAttemptAt, &n.LastError, &n.CreatedAt)
}

func (n *notifyStore) All(ctx context.Context) ([]ZoneNotify, error) {
	return n.query(ctx, `SELECT `+notifyColumns+` FROM zone_notifies ORDER BY zone_id, target`)
}

func (n *notifyStore) ByZone(ctx context.Context, zoneID int64) ([]ZoneNotify, error) {
	return n.query(ctx, `SELECT `+notifyColumns+` FROM zone_notifies WHERE zone_id = ? ORDER BY target`, zoneID)
}

func (n *notifyStore) query(ctx context.Context, q string, args ...any) ([]ZoneNotify, error) {
	rows, err := n.s.db.QueryContext(ctx, n.s.q(q), args...)
	if err != nil {
		return nil, wrapDBErr(err)
	}
	defer rows.Close()
	var out []ZoneNotify
	for rows.Next() {
		var zn ZoneNotify
		if err := scanNotify(rows, &zn); err != nil {
			return nil, err
		}
		out = append(out, zn)
	}
	return out, rows.Err()
}

// Reconcile runs both halves in one transaction. Separately, a pass that
// crashed between them would leave a zone whose rows match neither its old
// notify_to nor its new one — and the delete half alone would drop delivery
// history that the insert half was about to keep.
func (n *notifyStore) Reconcile(ctx context.Context, zoneID int64, targets []string, now int64) error {
	tx, err := n.s.db.BeginTx(ctx, nil)
	if err != nil {
		return wrapDBErr(err)
	}
	defer func() { _ = tx.Rollback() }()

	// Delete first, and by reading back rather than with a NOT IN list: the
	// list is operator-supplied and unbounded, and a NOT IN of a thousand
	// placeholders is a query planner's problem for no gain at this size.
	rows, err := tx.QueryContext(ctx, n.s.q(`SELECT id, target FROM zone_notifies WHERE zone_id = ?`), zoneID)
	if err != nil {
		return wrapDBErr(err)
	}
	want := make(map[string]bool, len(targets))
	for _, t := range targets {
		want[t] = true
	}
	have := map[string]bool{}
	var stale []int64
	for rows.Next() {
		var id int64
		var target string
		if err := rows.Scan(&id, &target); err != nil {
			rows.Close()
			return err
		}
		if want[target] {
			have[target] = true
		} else {
			stale = append(stale, id)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	for _, id := range stale {
		if _, err := tx.ExecContext(ctx, n.s.q(`DELETE FROM zone_notifies WHERE id = ?`), id); err != nil {
			return wrapDBErr(err)
		}
	}
	for _, t := range targets {
		if have[t] {
			continue
		}
		if _, err := tx.ExecContext(ctx, n.s.q(
			`INSERT INTO zone_notifies (zone_id, target, created_at) VALUES (?, ?, ?)`),
			zoneID, t, now); err != nil {
			return wrapDBErr(err)
		}
		// Marked as present, so a targets list naming the same address twice
		// inserts it once instead of violating UNIQUE(zone_id, target) and
		// rolling back the whole call. ParseNotifyTo does not dedupe and
		// ValidateNotifyTo is only ParseNotifyTo, so `10.0.0.2, 10.0.0.2` is
		// an accepted write — and without this line it would fail every
		// notify pass for that zone from then on, reported in a pass log far
		// from the edit that caused it.
		have[t] = true
	}
	return tx.Commit()
}

func (n *notifyStore) NoteDelivered(ctx context.Context, id int64, serial uint32, at int64) error {
	return n.s.execOne(ctx,
		`UPDATE zone_notifies SET pending_serial = ?, notified_serial = ?, notified_at = ?,
		   attempts = 0, next_attempt_at = 0, last_error = '' WHERE id = ?`,
		serial, serial, at, id)
}

func (n *notifyStore) NoteAttempt(ctx context.Context, id int64, pendingSerial uint32, attempts int, nextAttemptAt int64, errText string) error {
	return n.s.execOne(ctx,
		`UPDATE zone_notifies SET pending_serial = ?, attempts = ?, next_attempt_at = ?, last_error = ? WHERE id = ?`,
		pendingSerial, attempts, nextAttemptAt, errText, id)
}
```

`internal/store/store.go` — add to the `Store` interface, beside `TSIGKeys()`:

```go
	Notifies() NotifyStore
```

`internal/store/sql.go` — add the accessor beside the others:

```go
func (s *sqlStore) Notifies() NotifyStore { return &notifyStore{s: s} }
```

`internal/store/zones.go` — four edits, all mechanical, all required together:

```go
// 1. On the Zone struct, below AllowTransfer's block:

	// NotifyTo is who this zone tells when it changes: a comma-separated
	// list of host[:port] [key:<tsig name>], in zones.FormatNotifyTo's
	// canonical spelling. Empty means notify nobody, and that is the
	// default. Applies to a primary and to a secondary — a secondary that
	// re-serves what it pulled has its own downstream secondaries.
	NotifyTo string `json:"notify_to"`

// 2. zoneColumns — append `notify_to` immediately before created_at, so the
//    column order matches scanZone's argument order:
const zoneColumns = `id, name, type, enabled, soa_ns, soa_mbox, soa_serial, soa_refresh, soa_retry, soa_expire, soa_minimum, soa_ttl, primaries, tsig_key_id, expires_at, refreshed_at, last_error, last_attempt, allow_transfer, last_xfr_at, last_xfr_peer, last_xfr_error, notify_to, created_at, modified_at`

// 3. scanZone — &z.NotifyTo in the same position:
	return row.Scan(..., &z.LastXfrError, &z.NotifyTo, &z.CreatedAt, &z.ModifiedAt)

// 4. The insert and update argument lists gain zn.NotifyTo in their own
//    column order. Both bind every configuration column; notify_to is one.
```

`internal/store/tsigkeys.go` — the guard:

```go
// notifyKeyRef reports the SQL that finds a zone whose notify_to names
// tsig_keys.name. It is aclKeyRef's twin on the second column that can
// reference a key, and the two are separate functions rather than one
// parameterised by column name because each carries its own reasoning about
// the format it is matching inside.
//
// The format differs from allow_transfer's in one way that matters here: an
// entry is `host:port key:name`, so the key is preceded by a space rather
// than by the entry separator. Stripping spaces as aclKeyRef does would join
// the host to the key and stop `,key:` ever matching — so this strips nothing
// and anchors on ' key:' instead, which the canonical spelling guarantees is
// exactly how FormatNotifyTo writes it.
func notifyKeyRef(dialect string) string {
	fn := "instr"
	if dialect == "postgres" {
		fn = "strpos"
	}
	return `SELECT 1 FROM zones WHERE ` + fn +
		`(zones.notify_to || ',', ' key:' || tsig_keys.name || ',') > 0`
}
```

and in `Delete`, a third `NOT EXISTS`:

```go
	res, err := t.s.db.ExecContext(ctx, t.s.q(
		`DELETE FROM tsig_keys WHERE id = ?
		   AND NOT EXISTS (SELECT 1 FROM zones WHERE zones.tsig_key_id = tsig_keys.id)
		   AND NOT EXISTS (`+aclKeyRef(t.s.dialect)+`)
		   AND NOT EXISTS (`+notifyKeyRef(t.s.dialect)+`)`), id)
```

> **Note on the trailing-comma anchor.** `FormatNotifyTo` writes entries joined by `", "`, so a key is followed either by `", "` or by end-of-string. Appending `','` to the column makes the last entry look like every other one, which is why the pattern can be a single `' key:name,'` rather than two alternatives.
>
> **The coupling needs its own pin, and `TestDeleteTSIGKeyRefusedWhileNotifyToNamesIt` is not it.** That test hand-builds `z.NotifyTo = "10.0.0.2:53 key:" + name`, so it encodes the same assumption the SQL does: change `FormatNotifyTo` to join without the space and the guard and its test agree on the wrong thing, the guard silently stops matching, and a key that a signed NOTIFY depends on becomes deletable. Write the notify twin of `internal/zones/aclguard_test.go` — a `zones_test` package test that feeds `FormatNotifyTo`'s **actual output** through the guard. It lives in `internal/zones` rather than `internal/store` because `internal/store` cannot import `internal/zones` (that direction is an import cycle); `aclguard_test.go`'s own doc comment records this, and D3 solved the identical problem for `allow_transfer` the same way.

Add `internal/zones/notifyguard_test.go`, mirroring `aclguard_test.go`:

```go
// The store's notify_to key guard matches ` key:<name>,` inside the column
// in SQL (notifyKeyRef, internal/store/tsigkeys.go). That pattern is only
// correct because FormatNotifyTo writes exactly that spelling — so this
// feeds the formatter's real output to the guard rather than a hand-written
// string, which is the difference between pinning the coupling and
// restating it.
//
// It lives here rather than in internal/store because internal/store cannot
// import internal/zones — that direction is an import cycle. aclguard_test.go
// records the same reasoning for the allow_transfer half.
func TestNotifyToSpellingMatchesTheStoreGuard(t *testing.T) {
	// Mirror aclguard_test.go's structure: build a zone whose notify_to is
	// FormatNotifyTo's output for a keyed target, write it through the real
	// store, and assert TSIGKeys().Delete refuses the key with ErrInUse.
	// Then clear notify_to and assert the delete succeeds, so the test fails
	// if the guard matches nothing as readily as if it matches everything.
}
```

- [ ] **Step 4: Run, then commit**

Run:
```bash
go test ./internal/store/ -run 'Notify|NotifyTo|TSIGKey' -v
go test -race ./internal/store/
```
Expected: PASS on both sqlite and postgres subtests.

Verify the guard test is real: temporarily remove the `notifyKeyRef` `NOT EXISTS` clause, re-run `TestDeleteTSIGKeyRefusedWhileNotifyToNamesIt`, confirm it fails, restore.

```bash
gofmt -l internal cmd
~/go/bin/golangci-lint run ./internal/store/
git add internal/store
git commit -m "feat(store): the notify_to column and the zone_notifies queue

Migration 0012. notify_to defaults to empty, so no existing zone starts
notifying anything.

zone_notifies has one row per (zone, target) and no pending flag: the
serial is one. A row records what was achieved, and whether there is work
is derived by comparing notified_serial against the zone's soa_serial --
which is what lets any path that bumps a serial be picked up without a
call site to forget.

created_at is there for the screen: a target that has never been notified
has no notify date, so it is dated by when it was added.

Unlike tsig_key_id this takes a real foreign key -- 0009's reasoning does
not transfer, since zone_id has no zero-means-none case -- so a deleted
zone takes its queue rows with it.

The TSIG delete guard gains a third reference. Without it, deleting a key
silently downgrades a signed NOTIFY to an unsigned one the peer refuses,
far from the delete that caused it."
```

---

### Task 4: `notify_to` through the API

**Files:**
- Modify: `internal/api/zones_handlers.go` — `checkZoneTransferConfig`, `canonicalNotifyTo`, `zoneCreate`, `zonePatch`
- Modify: `internal/api/openapi.yaml`
- Test: `internal/api/zones_test.go` (append)

**Interfaces:**
- Consumes: `zones.ValidateNotifyTo`, `zones.ParseNotifyTo`, `zones.FormatNotifyTo`, `zones.NotifyToKeys` (Task 2); `store.Zone.NotifyTo` (Task 3).
- Produces: `notify_to` accepted on `POST /zones` and `PATCH /zones/{id}`, stored canonical.

**Which zone types may carry it.** Primary *and* secondary, unlike `primaries` and `tsig_key_id`, and like `allow_transfer`: a secondary needs it for the cascade, since a secondary that re-serves what it pulled has its own downstream secondaries. Refused on `internal`, `stub` and `forwarder` — the RFC 6303 built-ins are not transferable and the other two answer nothing, so a notify list on any of them is configuration nothing reads.

- [ ] **Step 1: Write the failing tests**

> **These bodies are written against a harness that does not exist — see the Global Constraints entry on the `internal/api` test harness.** `newTestServer` returns one value, `do` takes a JSON **string** body, POST answers `{"id": N}` rather than a zone, PATCH answers 204-empty, and there is no `decodeJSON`. Take the *coverage* from the bodies below — create on both transfer types, refusal on other types, malformed values, unknown keys, patch validation, canonicalisation, clearing to empty — and write them with `newTestServer(t)`, `ts.do(t, method, path, jsonString)`, `createdID(t, rec)` and `ts.zone(t, id)`. Task 4's implementer did exactly this; `internal/api/zones_handlers_test.go`'s `allow_transfer` tests are the model.

```go
// internal/api/zones_test.go — append.

func TestZoneCreateAcceptsNotifyToOnBothTransferTypes(t *testing.T) {
	for _, zoneType := range []string{"primary", "secondary"} {
		t.Run(zoneType, func(t *testing.T) {
			srv, _ := newTestServer(t)
			body := map[string]any{
				"name": "example.com", "type": zoneType,
				"notify_to": "10.0.0.2, 10.0.0.3:5353",
			}
			if zoneType == "secondary" {
				body["primaries"] = "10.0.0.1"
			}
			rec := srv.do(t, "POST", "/api/v1/zones", body)
			if rec.Code != http.StatusCreated {
				t.Fatalf("status %d, body %s", rec.Code, rec.Body)
			}
			var z store.Zone
			decodeJSON(t, rec, &z)
			// Stored canonical, not as typed — the delete guard matches
			// key:<name> inside this column in SQL and can only do that
			// because the spelling is known.
			if z.NotifyTo != "10.0.0.2:53, 10.0.0.3:5353" {
				t.Errorf("notify_to = %q, want the canonical spelling", z.NotifyTo)
			}
		})
	}
}

func TestZoneNotifyToRefusedOnNonTransferTypes(t *testing.T) {
	srv, st := newTestServer(t)
	// The built-ins are seeded by migration 7 and are the only reachable
	// `internal` zones; a PATCH of one 409s before validation, so the check
	// this test needs is on the create path with a type the API refuses
	// outright. Assert the message rather than only the code, since a 400
	// with the wrong reason is what an operator reads.
	rec := srv.do(t, "POST", "/api/v1/zones", map[string]any{
		"name": "example.com", "type": "internal", "notify_to": "10.0.0.2",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body)
	}
	_ = st
}

func TestZoneNotifyToRejectsMalformedAndUnknownKeys(t *testing.T) {
	tests := []struct {
		name     string
		notifyTo string
		contains string
	}{
		{"malformed entry", "10.0.0.2:0", "port"},
		{"unknown key", "10.0.0.2 key:nope", "which does not exist"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := newTestServer(t)
			rec := srv.do(t, "POST", "/api/v1/zones", map[string]any{
				"name": "example.com", "type": "primary", "notify_to": tc.notifyTo,
			})
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status %d, body %s", rec.Code, rec.Body)
			}
			if !strings.Contains(rec.Body.String(), tc.contains) {
				t.Errorf("body %s does not mention %q", rec.Body, tc.contains)
			}
		})
	}
}

// A rule enforced on POST and not on PATCH is a rule with a way around it —
// checkZoneTransferConfig's own doc comment, and the reason it validates the
// zone as it would be *stored* rather than the request body.
func TestZonePatchValidatesNotifyTo(t *testing.T) {
	srv, _ := newTestServer(t)
	rec := srv.do(t, "POST", "/api/v1/zones", map[string]any{
		"name": "example.com", "type": "primary",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create failed: %s", rec.Body)
	}
	var z store.Zone
	decodeJSON(t, rec, &z)

	bad := srv.do(t, "PATCH", "/api/v1/zones/"+strconv.FormatInt(z.ID, 10),
		map[string]any{"notify_to": "10.0.0.2:0"})
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("PATCH with a bad port: status %d, body %s", bad.Code, bad.Body)
	}

	ok := srv.do(t, "PATCH", "/api/v1/zones/"+strconv.FormatInt(z.ID, 10),
		map[string]any{"notify_to": "  10.0.0.2  "})
	if ok.Code != http.StatusOK {
		t.Fatalf("PATCH: status %d, body %s", ok.Code, ok.Body)
	}
	var patched store.Zone
	decodeJSON(t, ok, &patched)
	if patched.NotifyTo != "10.0.0.2:53" {
		t.Errorf("notify_to = %q, want the canonical spelling", patched.NotifyTo)
	}

	// Clearing it back to empty must work — a zone that stops notifying is
	// an ordinary edit, and an empty string must not be read as "unset, keep
	// what was there".
	cleared := srv.do(t, "PATCH", "/api/v1/zones/"+strconv.FormatInt(z.ID, 10),
		map[string]any{"notify_to": ""})
	if cleared.Code != http.StatusOK {
		t.Fatalf("clearing PATCH: status %d, body %s", cleared.Code, cleared.Body)
	}
	decodeJSON(t, cleared, &patched)
	if patched.NotifyTo != "" {
		t.Errorf("notify_to = %q after clearing, want empty", patched.NotifyTo)
	}
}
```

- [ ] **Step 2: Run and watch them fail**

Run: `go test ./internal/api/ -run 'NotifyTo' -v`
Expected: FAIL — `notify_to` is ignored, so the created zone's field is empty and the malformed value is accepted.

- [ ] **Step 3: Implement**

In `zoneCreate`, beside `AllowTransfer`:

```go
	// NotifyTo is who this zone tells when it changes. Like AllowTransfer and
	// unlike Primaries/TSIGKeyID, it applies to both transfer types: a
	// secondary that re-serves what it pulled has its own secondaries.
	NotifyTo string `json:"notify_to"`
```

In `zonePatch`, as a pointer so absent and empty are different:

```go
	NotifyTo *string `json:"notify_to"`
```

`checkZoneTransferConfig` gains a parameter and a block. Its signature becomes:

```go
func (s *Server) checkZoneTransferConfig(ctx context.Context, zoneType, primaries string, tsigKeyID int64, allowTransfer, notifyTo string) (int, string) {
```

and, after the `allowTransfer` block:

```go
	if notifyTo != "" {
		// Both transfer types may notify; nothing else may. internal is the
		// RFC 6303 built-ins, which are not transferable, and stub/forwarder
		// answer nothing of their own — a notify list on any of them is
		// configuration nothing would ever read, shown by the UI as though
		// it meant something.
		if zoneType != zoneTypePrimary && zoneType != zoneTypeSecondary {
			return http.StatusBadRequest, "notify_to applies to primary and secondary zones only"
		}
		if err := zones.ValidateNotifyTo(notifyTo); err != nil {
			return http.StatusBadRequest, err.Error()
		}
		// A key: name that names nothing would make every NOTIFY to that
		// target unsigned, and a peer requiring a signature refuses it with
		// no indication why. Caught here, exactly as allow_transfer's keys
		// are, and guarded from the other end by tsigKeyStore.Delete.
		for _, name := range zones.NotifyToKeys(notifyTo) {
			if _, found, err := s.deps.Store.TSIGKeys().ByName(ctx, name); err != nil {
				return http.StatusServiceUnavailable, "storage unavailable"
			} else if !found {
				return http.StatusBadRequest, "notify_to names TSIG key " + name + ", which does not exist"
			}
		}
	}
```

and a canonicaliser beside `canonicalAllowTransfer`:

```go
// canonicalNotifyTo parses input and returns it in zones.FormatNotifyTo's
// canonical spelling — the form tsigKeyStore.Delete matches ` key:<name>`
// against in SQL. input == "" returns "" without parsing.
//
// Called after checkZoneTransferConfig has already validated the same string,
// so the error here is unreachable in practice; it is checked rather than
// discarded for the reason canonicalAllowTransfer's comment gives — errcheck
// cannot know that, and a re-parse that swallowed a failure would be one call
// away from storing whatever ParseNotifyTo gave up on.
func canonicalNotifyTo(input string) (string, error) {
	if input == "" {
		return "", nil
	}
	ts, err := zones.ParseNotifyTo(input)
	if err != nil {
		return "", err
	}
	return zones.FormatNotifyTo(ts), nil
}
```

Both call sites of `checkZoneTransferConfig` pass the new argument — `body.NotifyTo` on create, and on patch the *resolved* value `z.NotifyTo` after the pointer has been applied, since the check validates the zone as it would be stored. Both then run the value through `canonicalNotifyTo` before it reaches the store.

`internal/api/openapi.yaml`: `notify_to` on the `Zone` schema (read-only in responses, writable on create and patch), described as the comma-separated `host[:port] [key:<tsig name>]` list, empty meaning notify nobody, stored canonical.

- [ ] **Step 4: Run, then commit**

Run:
```bash
go test ./internal/api/ -run 'NotifyTo|OpenAPI' -v
go test -race ./internal/api/
```
Expected: PASS. The OpenAPI test checks real routes against the document (see `fix/openapi-route-check`, commit `656e435`), so a field added to the schema and not to the handler — or the reverse — fails here.

**Pinning the PATCH validation, and the probe that does not work.** The property worth proving is `checkZoneTransferConfig`'s own rule — a rule enforced on POST and not on PATCH is a rule with a way around it. Skip that call on the patch path and confirm a test fails.

Do **not** use the malformed-port case as the probe: `canonicalNotifyTo` re-parses the value immediately afterwards, so a bad port is caught twice and removing the validation call changes nothing observable — the experiment reports a false pass. Use the **unknown-TSIG-key** case, since key existence is checked only by `checkZoneTransferConfig`. (Found by Task 4's implementer against a dispatch that prescribed the wrong probe.)

```bash
gofmt -l internal cmd
~/go/bin/golangci-lint run ./internal/api/
git add internal/api
git commit -m "feat(api): notify_to on zone create and patch

Validated as the zone would be stored, on POST and PATCH alike, and stored
in the canonical spelling the TSIG delete guard matches against.

Allowed on primary and secondary -- a secondary that re-serves what it
pulled has its own downstream secondaries -- and refused on internal, stub
and forwarder, where it would be configuration nothing reads.

A key: name that does not exist is a 400 rather than a target whose every
NOTIFY goes unsigned and is refused with no indication why."
```

---

### Task 5: The intercept, and the defect it closes

**Files:**
- Create: `internal/dnssrv/notifies.go`
- Modify: `internal/dnssrv/server.go` — the branch in `serve`
- Test: `internal/dnssrv/notifies_test.go`

**Interfaces:**
- Consumes: `Server.RequireTSIG` (`internal/dnssrv/tsig.go:199`).
- Produces:
  ```go
  type Notifies interface {
      ServeNotify(ctx context.Context, w dns.ResponseWriter, m *dns.Msg, key string, tsigErr error)
  }
  func WithNotifies(n Notifies) Option
  const NotifyTimeout = 10 * time.Second
  ```

**This closes a live defect.** `DefaultMsgAcceptFunc` accepts opcode NOTIFY (`acceptfunc.go:40`, and it allows the answer-section SOA on purpose), so a NOTIFY reaches `Server.serve` today, carries one question of qtype SOA, and is handled as an ordinary query: counted in the query log, evaluated by the filter, eligible for the cache, and — for an apex this server does not hold — **forwarded upstream** by the terminal handler (`app.go:290`, chain `logger → registry → filter → resolver → cache → forwarder`). dnsaur asks Cloudflare an SOA question on behalf of a peer trying to notify it. The regression test below fails on the commit before this one.

**Why a branch and not a middleware.** Unlike a transfer, a NOTIFY reply *is* one message, so it would fit `Handler` mechanically. It must not: the pipeline is exactly what has no meaning for it, and the forwarding above is what that costs. The justification is therefore weaker than §9.1's for transfers — a transfer *cannot* be a `Handler`, a NOTIFY merely must not be one — and the comment says so rather than implying the two are the same case.

- [ ] **Step 1: Write the failing tests**

```go
package dnssrv_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/aloks98/dnsaur/internal/dnssrv"
)

// recordingNotifies captures what the intercept handed over and answers
// NOERROR, so a test can assert on the routing rather than on any policy.
//
// It mirrors fakeTransfers' shape, snapshot method included, rather than
// inventing a second convention in the same package — and the snapshot is
// what lets a test assert the TSIG verdict actually arrived, instead of
// merely recording it and never looking.
type recordingNotifies struct {
	mu      sync.Mutex
	calls   int
	key     string
	tsigErr error
	opcode  int
}

func (r *recordingNotifies) ServeNotify(_ context.Context, w dns.ResponseWriter, m *dns.Msg, key string, tsigErr error) {
	r.mu.Lock()
	r.calls++
	r.key = key
	r.tsigErr = tsigErr
	r.opcode = m.Opcode
	r.mu.Unlock()

	reply := new(dns.Msg)
	reply.SetReply(m)
	_ = w.WriteMsg(reply)
}

func (r *recordingNotifies) snapshot() (calls int, key string, opcode int, tsigErr error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls, r.key, r.opcode, r.tsigErr
}

// notifyPipelineHandler is the pipeline. A NOTIFY reaching it is the defect.
type notifyPipelineHandler struct{ calls atomic.Int64 }

func (c *notifyPipelineHandler) ServeDNS(_ context.Context, req *dnssrv.Request) (*dnssrv.Response, error) {
	c.calls.Add(1)
	m := new(dns.Msg)
	m.SetReply(req.Msg)
	return &dnssrv.Response{Msg: m, Decision: dnssrv.DecisionForwarded}, nil
}

// THE REGRESSION. On the commit before this task, a NOTIFY falls through to
// the pipeline and — for an apex this server does not hold — is forwarded
// upstream. The assertion is on the pipeline never being entered, because
// that is the property, not on any particular downstream behaviour.
func TestNotifyNeverReachesThePipeline(t *testing.T) {
	h := &notifyPipelineHandler{}
	n := &recordingNotifies{}
	srv := dnssrv.NewServer("127.0.0.1:0", h, dnssrv.WithNotifies(n))
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })

	m := new(dns.Msg).SetNotify("example.com.")
	c := new(dns.Client)
	reply, _, err := c.Exchange(m, srv.Addr())
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if reply == nil {
		t.Fatal("no reply")
	}
	if got := h.calls.Load(); got != 0 {
		t.Errorf("the pipeline handled %d NOTIFY messages, want 0", got)
	}
	if calls, _, _, _ := n.snapshot(); calls != 1 {
		t.Errorf("ServeNotify called %d times, want 1", calls)
	}
}

// Without the option the branch is not taken at all, which is what every
// server built before D4 did. A NOTIFY then falls through as it always has —
// pinned so that removing the option is a known behaviour change rather than
// a silent one.
func TestNotifyFallsThroughWithoutTheOption(t *testing.T) {
	h := &notifyPipelineHandler{}
	srv := dnssrv.NewServer("127.0.0.1:0", h)
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })

	m := new(dns.Msg).SetNotify("example.com.")
	c := new(dns.Client)
	if _, _, err := c.Exchange(m, srv.Addr()); err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if got := h.calls.Load(); got != 1 {
		t.Errorf("the pipeline handled %d messages, want 1", got)
	}
}

// An ordinary SOA query is not a NOTIFY, and must keep reaching the
// pipeline. The two differ only in the opcode, so a branch that keyed on
// qtype would swallow every SOA query in the server.
func TestOrdinarySOAQueryStillReachesThePipeline(t *testing.T) {
	h := &notifyPipelineHandler{}
	n := &recordingNotifies{}
	srv := dnssrv.NewServer("127.0.0.1:0", h, dnssrv.WithNotifies(n))
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })

	m := new(dns.Msg).SetQuestion("example.com.", dns.TypeSOA)
	c := new(dns.Client)
	if _, _, err := c.Exchange(m, srv.Addr()); err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if got := h.calls.Load(); got != 1 {
		t.Errorf("the pipeline handled %d messages, want 1", got)
	}
	if calls, _, _, _ := n.snapshot(); calls != 0 {
		t.Errorf("ServeNotify handled %d messages, want 0", calls)
	}
}

// A NOTIFY arrives over TCP as readily as UDP, and both listeners share one
// handler, so the branch has to be on the shared path rather than on either
// socket's.
func TestNotifyIsInterceptedOverTCP(t *testing.T) {
	h := &notifyPipelineHandler{}
	n := &recordingNotifies{}
	srv := dnssrv.NewServer("127.0.0.1:0", h, dnssrv.WithNotifies(n))
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })

	m := new(dns.Msg).SetNotify("example.com.")
	c := &dns.Client{Net: "tcp"}
	if _, _, err := c.Exchange(m, srv.Addr()); err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if got := h.calls.Load(); got != 0 {
		t.Errorf("the pipeline handled %d NOTIFY messages over TCP, want 0", got)
	}
	if calls, _, _, _ := n.snapshot(); calls != 1 {
		t.Errorf("ServeNotify called %d times over TCP, want 1", calls)
	}
}

// A transfer is still a transfer: the two intercepts must not shadow one
// another. AXFR carries opcode QUERY, so only the qtype branch may claim it.
func TestTransferStillRoutesToTransfers(t *testing.T) {
	h := &notifyPipelineHandler{}
	n := &recordingNotifies{}
	tr := &fakeTransfers{} // defined in transfers_test.go
	srv := dnssrv.NewServer("127.0.0.1:0", h,
		dnssrv.WithNotifies(n), dnssrv.WithTransfers(tr))
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })

	m := new(dns.Msg)
	m.SetAxfr("example.com.")
	c := &dns.Client{Net: "tcp"}
	_, _, _ = c.Exchange(m, srv.Addr())

	// Both halves. Asserting only that Notifies did not claim it would pass
	// just as well if the AXFR reached *neither* handler — which is the bug
	// a future change to isTransferQuery would actually cause.
	if calls, _, _, _ := n.snapshot(); calls != 0 {
		t.Errorf("ServeNotify claimed an AXFR %d times, want 0", calls)
	}
	if calls, _, _, qtype, _ := tr.snapshot(); calls != 1 || qtype != dns.TypeAXFR {
		t.Errorf("Transfers saw %d calls of qtype %d, want 1 of AXFR", calls, qtype)
	}
}

// The one invariant the Notifies doc comment names: key and tsigErr are what
// Server.RequireTSIG concluded, passed rather than recomputed, because a
// disagreement between two computations would be a NOTIFY acted on without
// the signature the zone requires.
//
// Mirrors D3's TestTheTSIGVerdictIsPassedToTheHandler for the transfer path.
// Without it the interface's central promise is documented and untested.
func TestTheTSIGVerdictIsPassedToTheNotifyHandler(t *testing.T) {
	const keyName = "notify.e412.in."
	const secret = "c2VjcmV0LXNlY3JldC1zZWNyZXQ="
	keys := newFakeKeyStore(store.TSIGKey{Name: keyName, Algorithm: dns.HmacSHA256, Secret: secret})

	n := &recordingNotifies{}
	addr := startServer(t, noopHandler(t), dnssrv.WithTSIGKeys(keys), dnssrv.WithNotifies(n))

	// Signed: the handler sees the canonical key name and no error.
	signed := new(dns.Msg).SetNotify("e412.in.")
	signed.SetTsig(keyName, dns.HmacSHA256, 300, time.Now().Unix())
	c := &dns.Client{TsigSecret: map[string]string{keyName: secret}}
	if _, _, err := c.Exchange(signed, addr); err != nil {
		t.Fatalf("signed notify exchange: %v", err)
	}
	if _, key, _, tsigErr := n.snapshot(); key != keyName || tsigErr != nil {
		t.Fatalf("signed notify: key = %q, err = %v; want %q, nil", key, tsigErr, keyName)
	}

	// Unsigned: no key, and ErrTSIGUnsigned.
	plain := new(dns.Msg).SetNotify("e412.in.")
	if _, _, err := (&dns.Client{}).Exchange(plain, addr); err != nil {
		t.Fatalf("unsigned notify exchange: %v", err)
	}
	if _, key, _, tsigErr := n.snapshot(); key != "" || !errors.Is(tsigErr, dnssrv.ErrTSIGUnsigned) {
		t.Fatalf("unsigned notify: key = %q, err = %v; want \"\", ErrTSIGUnsigned", key, tsigErr)
	}
}

// The deadline is the transfer branch's reasoning applied to a smaller job:
// the pipeline's 5s context is cancelled when serve returns, and the SOA
// probe and any transfer the notify triggers outlive the reply. NotifyTimeout
// bounds the reply itself; the work behind it takes its own background
// context (Task 7).
func TestNotifyTimeoutIsItsOwn(t *testing.T) {
	if dnssrv.NotifyTimeout <= 0 || dnssrv.NotifyTimeout >= dnssrv.TransferTimeout {
		t.Fatalf("NotifyTimeout = %v, want a positive bound below TransferTimeout (%v)",
			dnssrv.NotifyTimeout, dnssrv.TransferTimeout)
	}
}
```

> `fakeTransfers` is D3's existing type in `internal/dnssrv/transfers_test.go` — reuse it rather than adding a second one. `notifyPipelineHandler` is named to avoid colliding with an existing top-level function in `transfers_test.go`; the package is shared, so a duplicate identifier will not compile.

- [ ] **Step 2: Run and watch them fail**

Run: `go test ./internal/dnssrv/ -run 'Notify|Transfer' -v`

**Getting real RED evidence here takes two phases, and asking for it in one is a mistake.** Go compiles a package as a unit, so while `WithNotifies` and `Notifies` do not exist the whole test binary fails to build — you get a compile error, not `TestNotifyNeverReachesThePipeline` failing on the defect. A build failure is not evidence that a NOTIFY reaches the forwarder.

So: first add `internal/dnssrv/notifies.go` with the interface, the option and the constant, but **do not** add the branch to `serve`. The package now compiles, every other new test passes, and `TestNotifyNeverReachesThePipeline` fails at runtime with `the pipeline handled 1 NOTIFY messages, want 0` — **that is the defect, and that output is what to record.** Only then wire the branch into `serve`.

- [ ] **Step 3: Implement**

`internal/dnssrv/notifies.go`:

```go
package dnssrv

import (
	"context"
	"time"

	"github.com/miekg/dns"
)

// NotifyTimeout bounds answering one NOTIFY. It is much smaller than
// TransferTimeout because the reply is one message and goes out *before* any
// work is done: RFC 1996 §4.7 has the responder enter its refresh state, and
// §3.6 has the sender retransmitting until it gets a response, so a responder
// that waited for a transfer would earn a second NOTIFY for the transfer
// already in flight. The probe and the transfer take their own background
// context — see zones.NotifyServer.
const NotifyTimeout = 10 * time.Second

// Notifies answers NOTIFY, which the pipeline must not.
//
// The justification is deliberately weaker than Transfers'. A transfer
// *cannot* be a Handler — Handler returns one *Response and a transfer is a
// sequence of messages — while a NOTIFY reply is a single message and would
// fit it mechanically. It still must not go through the chain: qlog's
// per-query accounting, the filter, the cache and the forwarder have no
// meaning for it, and before this interface existed the consequence was
// concrete. A NOTIFY for an apex this server does not hold fell through every
// middleware to the terminal forwarder and was sent upstream, so dnsaur asked
// its own upstream resolver an SOA question on behalf of a peer that was
// trying to notify it.
//
// key and tsigErr are what Server.RequireTSIG concluded about this message,
// passed rather than recomputed, for the reason Transfers documents: two
// callers of one check are two chances to disagree, and here the disagreement
// would be a NOTIFY acted on without the signature the zone requires.
//
// Implementations own the whole reply and are responsible for their own
// rcodes; nothing downstream inspects what they wrote. m carries exactly one
// question — miekg's DefaultMsgAcceptFunc rejects any other QDCOUNT with
// FORMERR before serve runs — of whatever qtype the sender chose, which the
// implementation is expected to check.
type Notifies interface {
	ServeNotify(ctx context.Context, w dns.ResponseWriter, m *dns.Msg, key string, tsigErr error)
}

// WithNotifies attaches the handler NOTIFY is routed to. Without it the
// branch is not taken and a NOTIFY falls through the pipeline as any other
// query would, which is what every server built before Milestone D4 did.
func WithNotifies(n Notifies) Option {
	return func(s *Server) { s.notifies = n }
}

// isNotify reports whether m is a NOTIFY: an opcode, not a qtype, which is
// exactly why isTransferQuery does not catch it and why this is a second
// branch rather than another case in that one.
//
// The question count is not checked here. miekg rejects any message whose
// header QDCOUNT is not 1 with FORMERR of its own accord
// (DefaultMsgAcceptFunc, acceptfunc.go:44, applied at server.go:639-660), and
// dnsaur does not replace that accept function — the same reasoning
// isTransferQuery records. Zero questions remains reachable by the one shape
// that comment describes, so ServeNotify checks for itself rather than
// assuming a question exists.
func isNotify(m *dns.Msg) bool {
	return m.Opcode == dns.OpcodeNotify
}
```

`internal/dnssrv/server.go` — a field beside `transfers`:

```go
	notifies  Notifies
```

and the branch in `serve`, immediately after the transfer one so the two read as the pair they are:

```go
	if s.notifies != nil && isNotify(m) {
		// Its own deadline and its own writer, and none of the pipeline —
		// which for a NOTIFY is not merely inapplicable but actively wrong:
		// before this branch existed, a NOTIFY for an apex this server does
		// not hold reached the terminal forwarder and was sent upstream.
		ctx, cancel := context.WithTimeout(context.Background(), NotifyTimeout)
		defer cancel()
		s.notifies.ServeNotify(ctx, w, m, key, tsigErr)
		return
	}
```

- [ ] **Step 4: Run, then commit**

Run:
```bash
go test ./internal/dnssrv/ -run 'Notify|Transfer|SOA' -v
go test -race ./internal/dnssrv/
```
Expected: PASS

Confirm the regression is real: `git stash` the `server.go` branch alone, re-run `TestNotifyNeverReachesThePipeline`, watch it fail, restore.

```bash
gofmt -l internal cmd
~/go/bin/golangci-lint run ./internal/dnssrv/
git add internal/dnssrv
git commit -m "fix(dnssrv): intercept NOTIFY ahead of the pipeline

NOTIFY is an opcode, not a qtype, so isTransferQuery never caught it and
nothing else branched on it -- meaning a NOTIFY was handled as an ordinary
SOA query: query-logged, filtered, cacheable, and for an apex this server
does not hold, forwarded upstream. dnsaur asked its own upstream an SOA
question on behalf of a peer trying to notify it.

TestNotifyNeverReachesThePipeline fails on the commit before this one.

Unlike a transfer, a NOTIFY reply is one message and would fit Handler
mechanically -- it simply must not, and the comment says so rather than
implying the two intercepts have the same justification."
```

---

### Task 6: The inbound gate

**Files:**
- Create: `internal/zones/notifyserver.go`
- Test: `internal/zones/notifyserver_test.go`

**Interfaces:**
- Consumes: `dnssrv.Notifies` (Task 5); `Resolver.Snapshot`, `Index.Apex` (D3); `ParsePrimaries` (D2); `store.ZoneStore`.
- Produces:
  ```go
  type NotifyServer struct{ /* ... */ }
  type NotifyServerOption func(*NotifyServer)
  func NewNotifyServer(r *Resolver, zs store.ZoneStore, rf Refreshes, opts ...NotifyServerOption) *NotifyServer
  func WithNotifyServerNow(now func() time.Time) NotifyServerOption
  // WithNotifyProbes is declared here and used from Task 7 onward; a
  // NotifyServer built without it skips the probe and always refreshes,
  // which is what Task 6's own tests exercise.
  func WithNotifyProbes(p Probes) NotifyServerOption
  func (n *NotifyServer) ServeNotify(ctx context.Context, w dns.ResponseWriter, m *dns.Msg, key string, tsigErr error)

  // Refreshes is the half of *Refresher this needs, taken as an interface so
  // a test can drive the gate without a Transferrer or a live primary.
  type Refreshes interface {
      Refresh(ctx context.Context, zoneID int64) (TransferResult, error)
  }
  ```

**The gate, in order.** `decide` is pure — message, peer, parsed primaries, key, TSIG error in; a zone or a refusal out — so the table below is directly a test table.

| Condition | Rcode | Reply carries |
|---|---|---|
| no question, or qtype is not SOA | FORMERR | |
| no zone at that apex | NOTAUTH | |
| zone is not type `secondary` | NOTAUTH | |
| zone is disabled | NOTAUTH | |
| source address matched no entry in `primaries` | REFUSED | |
| TSIG present, key unknown or algorithm mismatched (`dns.ErrSecret`, `dns.ErrKeyAlg`) — **whether or not the zone names a key** | REFUSED | TSIG RR, BADKEY (17), **unsigned** |
| TSIG present, MAC did not verify (`dns.ErrSig`) — **whether or not the zone names a key** | REFUSED | TSIG RR, BADSIG (16), **unsigned** |
| TSIG present, outside the fudge window (`dns.ErrTime`) — **whether or not the zone names a key** | REFUSED | TSIG RR, BADTIME (18), **signed** |
| zone names a TSIG key and the message is unsigned (`ErrTSIGUnsigned`) | REFUSED | |
| zone names **no** key and the message is unsigned (`ErrTSIGUnsigned`) | NOERROR — accepted | |
| nothing verified the message — this server has no key store (`ErrTSIGUnavailable`) | SERVFAIL | never a TSIG error |
| TSIG lookup failed because the store failed | SERVFAIL | never a TSIG error |
| otherwise | NOERROR, AA set, question echoed | signed iff the request verified |

**Where these differ from D3's, and why.** The TSIG rows are §9.5.5's exactly, including that BADKEY and BADSIG are unsigned while BADTIME is signed — reuse `TransferServer.errorTSIG` and `signIfVerified` rather than writing second copies. The *rcode* differs: D3 answers a refused transfer NOTAUTH under RFC 5936 §2.2.1, which is about authority over the zone, while a refused NOTIFY is a policy statement by a server that does hold the zone and declines to be told by this peer.

**REFUSED for an unknown source is a deliberate divergence from RFC 1996 §3.10**, which says such a request "should ignore the request". Recorded in §9.9. The reasoning is in §9.10.2 and the short form is: silence is indistinguishable from a firewall drop, the common cause is a `primaries` list one address wrong, and there is no amplification to worry about — a NOTIFY and a REFUSED are both roughly 50 bytes, so 1:1 reflection is not a useful attack primitive.

- [ ] **Step 1: Write the failing tests**

```go
package zones_test

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/aloks98/dnsaur/internal/store"
	"github.com/aloks98/dnsaur/internal/zones"
)

const notifyApex = "example.com"

// fakeRefresher stands in for *Refresher. Refresh records the zone it was
// asked for and blocks on gate when the test supplies one, so a test can
// observe the reply while a transfer is still running.
type fakeRefresher struct {
	mu    sync.Mutex
	calls []int64
	// gate, when non-nil, blocks Refresh until it is closed.
	gate chan struct{}
	// ctxAlive records whether the context Refresh was handed was still
	// live when it ran — the whole point of the background handoff.
	ctxAlive bool
}

func (f *fakeRefresher) Refresh(ctx context.Context, zoneID int64) (zones.TransferResult, error) {
	if f.gate != nil {
		<-f.gate
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, zoneID)
	f.ctxAlive = ctx.Err() == nil
	return zones.TransferResult{}, nil
}

func (f *fakeRefresher) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// notifyFixture builds a NotifyServer over a real store and resolver, the
// way newXFRFixture does for the transfer gate.
type notifyFixture struct {
	ns   *zones.NotifyServer
	rf   *fakeRefresher
	st   store.Store
	zone store.Zone
	now  func() time.Time
}

func newNotifyFixture(t *testing.T, z store.Zone, opts ...zones.NotifyServerOption) *notifyFixture {
	t.Helper()
	ctx := context.Background()

	st, err := store.Open(ctx, "sqlite", t.TempDir()+"/t.db")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("store.Close: %v", err)
		}
	})
	if z.Name != "" {
		id, err := st.Zones().AddZone(ctx, z)
		if err != nil {
			t.Fatalf("AddZone: %v", err)
		}
		z.ID = id
	}
	res := zones.NewResolver(st.Zones())
	if err := res.Reload(ctx); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	rf := &fakeRefresher{}
	return &notifyFixture{
		ns:   zones.NewNotifyServer(res, st.Zones(), rf, opts...),
		rf:   rf,
		st:   st,
		zone: z,
	}
}

// notify sends one NOTIFY through the gate and returns the reply. peer is
// the source address the gate matches against `primaries`.
func (f *notifyFixture) notify(t *testing.T, apex string, qtype uint16, peer, key string, tsigErr error) *dns.Msg {
	t.Helper()
	m := new(dns.Msg).SetNotify(dns.Fqdn(apex))
	m.Question[0].Qtype = qtype
	w := &captureWriter{remote: &net.UDPAddr{IP: net.ParseIP(peer), Port: 40000}}
	f.ns.ServeNotify(context.Background(), w, m, key, tsigErr)
	if w.msg == nil {
		t.Fatal("the gate wrote no reply")
	}
	return w.msg
}

// notifyZone is the secondary every gate row starts from; each case
// overrides the one field it is about.
func notifyZone() store.Zone {
	return store.Zone{
		Name: notifyApex, Type: "secondary", Enabled: true,
		Primaries: "10.0.0.1",
		SOANS:     "ns1." + notifyApex, SOAMbox: "hostmaster." + notifyApex,
		SOASerial: 10, SOARefresh: 3600, SOARetry: 600,
		SOAExpire: 604800, SOAMinimum: 300, SOATTL: 900,
		// A non-zero refreshed_at, so these rows exercise the gate rather
		// than the never-transferred shortcut (Task 7).
		RefreshedAt: 1,
	}
}

// tsigErrorOf returns the TSIG RR's Error field, or 0 when the reply carries
// no TSIG at all.
func tsigErrorOf(m *dns.Msg) uint16 {
	if t := m.IsTsig(); t != nil {
		return t.Error
	}
	return 0
}

// The gate, one case per row of §9.10.2's table. Each asserts the rcode
// *and* the TSIG error code where there is one, because a refusal with the
// right rcode and the wrong TSIG error tells the peer to fix the wrong
// thing.
func TestNotifyGate(t *testing.T) {
	tests := []struct {
		name        string
		mutate      func(z *store.Zone)
		qtype       uint16
		peer        string
		key         string
		tsigErr     error
		wantRcode   int
		wantTSIG    uint16 // 0 = no TSIG error RR
		wantRefresh bool
	}{
		{
			name:      "an authorised notify is accepted and triggers a refresh",
			qtype:     dns.TypeSOA,
			peer:      "10.0.0.1",
			wantRcode: dns.RcodeSuccess, wantRefresh: true,
		},
		{
			name:      "a qtype other than SOA is malformed",
			qtype:     dns.TypeA,
			peer:      "10.0.0.1",
			wantRcode: dns.RcodeFormatError,
		},
		{
			name:      "a primary zone is not told by anyone",
			mutate:    func(z *store.Zone) { z.Type = "primary"; z.Primaries = "" },
			qtype:     dns.TypeSOA,
			peer:      "10.0.0.1",
			wantRcode: dns.RcodeNotAuth,
		},
		{
			name:      "a disabled zone",
			mutate:    func(z *store.Zone) { z.Enabled = false },
			qtype:     dns.TypeSOA,
			peer:      "10.0.0.1",
			wantRcode: dns.RcodeNotAuth,
		},
		{
			name:      "a source that is not a configured primary",
			qtype:     dns.TypeSOA,
			peer:      "10.9.9.9",
			wantRcode: dns.RcodeRefused,
		},
		{
			// tsigErr carries the sentinel, because that is what
			// Server.RequireTSIG actually produces for an unsigned message.
			// Leaving it nil exercises a branch production cannot reach:
			// RequireTSIG returns ("", err) or (name, nil) and never
			// ("", nil), so a row without the sentinel would pass even if
			// the live ErrTSIGUnsigned arm were deleted outright.
			name:      "a keyed zone told by an unsigned message",
			mutate:    func(z *store.Zone) { z.TSIGKeyID = 1 },
			qtype:     dns.TypeSOA,
			peer:      "10.0.0.1", tsigErr: dnssrv.ErrTSIGUnsigned,
			wantRcode: dns.RcodeRefused,
		},
		{
			// The other half of the same rule: a zone naming no key accepts
			// an unsigned NOTIFY. Without this, gating the whole TSIG block
			// on TSIGKeyID would look correct.
			name:      "a keyless zone accepts an unsigned message",
			qtype:     dns.TypeSOA,
			peer:      "10.0.0.1", tsigErr: dnssrv.ErrTSIGUnsigned,
			wantRcode: dns.RcodeSuccess, wantRefresh: true,
		},
		{
			// A broken MAC is refused even though the zone names no key.
			// RFC 8945 §5.2.2 makes BADSIG mandatory regardless of local
			// policy, and accepting this would answer an unsigned NOERROR
			// the peer must discard, making it retransmit forever.
			name:      "a broken MAC to a keyless zone is still refused",
			qtype:     dns.TypeSOA,
			peer:      "10.0.0.1", key: "k.", tsigErr: dns.ErrSig,
			wantRcode: dns.RcodeRefused, wantTSIG: dns.RcodeBadSig,
		},
		{
			name:      "an unknown key",
			mutate:    func(z *store.Zone) { z.TSIGKeyID = 1 },
			qtype:     dns.TypeSOA,
			peer:      "10.0.0.1", key: "other.", tsigErr: dns.ErrSecret,
			wantRcode: dns.RcodeRefused, wantTSIG: dns.RcodeBadKey,
		},
		{
			name:      "a bad MAC",
			mutate:    func(z *store.Zone) { z.TSIGKeyID = 1 },
			qtype:     dns.TypeSOA,
			peer:      "10.0.0.1", key: "k.", tsigErr: dns.ErrSig,
			wantRcode: dns.RcodeRefused, wantTSIG: dns.RcodeBadSig,
		},
		{
			name:      "a clock outside the fudge window",
			mutate:    func(z *store.Zone) { z.TSIGKeyID = 1 },
			qtype:     dns.TypeSOA,
			peer:      "10.0.0.1", key: "k.", tsigErr: dns.ErrTime,
			wantRcode: dns.RcodeRefused, wantTSIG: dns.RcodeBadTime,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			z := notifyZone()
			if tc.mutate != nil {
				tc.mutate(&z)
			}
			f := newNotifyFixture(t, z)
			reply := f.notify(t, notifyApex, tc.qtype, tc.peer, tc.key, tc.tsigErr)

			if reply.Rcode != tc.wantRcode {
				t.Errorf("rcode = %s, want %s",
					dns.RcodeToString[reply.Rcode], dns.RcodeToString[tc.wantRcode])
			}
			if got := tsigErrorOf(reply); got != tc.wantTSIG {
				t.Errorf("TSIG error = %d, want %d", got, tc.wantTSIG)
			}
			if reply.Rcode == dns.RcodeSuccess && !reply.Authoritative {
				t.Error("an accepted NOTIFY reply must set AA")
			}
			if len(reply.Question) != 1 || reply.Question[0].Name != dns.Fqdn(notifyApex) {
				t.Errorf("question not echoed: %v", reply.Question)
			}

			// The handoff is asynchronous, so give it a bounded moment to
			// land rather than sleeping a fixed interval.
			deadline := time.Now().Add(2 * time.Second)
			for f.rf.count() == 0 && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if got := f.rf.count() > 0; got != tc.wantRefresh {
				t.Errorf("refresh triggered = %v, want %v", got, tc.wantRefresh)
			}
		})
	}
}

// An apex this server does not hold at all — a separate fixture, since the
// zone simply is not there.
func TestNotifyForAnUnheldZoneIsNotAuth(t *testing.T) {
	f := newNotifyFixture(t, store.Zone{})
	reply := f.notify(t, "nowhere.example", dns.TypeSOA, "10.0.0.1", "", nil)
	if reply.Rcode != dns.RcodeNotAuth {
		t.Errorf("rcode = %s, want NOTAUTH", dns.RcodeToString[reply.Rcode])
	}
}

// RFC 1996 §4.7 has the responder answer before it acts, and §3.6 has the
// sender retransmitting until it does — so a reply that waited for a
// transfer would earn a second NOTIFY for the transfer already running.
func TestNotifyRepliesBeforeTransferring(t *testing.T) {
	f := newNotifyFixture(t, notifyZone())
	f.rf.gate = make(chan struct{})

	m := new(dns.Msg).SetNotify(dns.Fqdn(notifyApex))
	w := &captureWriter{remote: &net.UDPAddr{IP: net.ParseIP("10.0.0.1"), Port: 40000}}

	done := make(chan struct{})
	go func() {
		f.ns.ServeNotify(context.Background(), w, m, "", nil)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ServeNotify blocked on the transfer instead of replying first")
	}
	if w.msg == nil || w.msg.Rcode != dns.RcodeSuccess {
		t.Fatalf("reply = %v, want NOERROR written before the transfer", w.msg)
	}
	if f.rf.count() != 0 {
		t.Error("the transfer ran before the reply went out")
	}
	close(f.rf.gate)
}

// The handoff must not inherit the request's context: dnssrv cancels it when
// serve returns, and a transfer started under it would be cut off mid-zone.
func TestNotifyWorkOutlivesTheRequestContext(t *testing.T) {
	f := newNotifyFixture(t, notifyZone())

	ctx, cancel := context.WithCancel(context.Background())
	m := new(dns.Msg).SetNotify(dns.Fqdn(notifyApex))
	w := &captureWriter{remote: &net.UDPAddr{IP: net.ParseIP("10.0.0.1"), Port: 40000}}
	f.ns.ServeNotify(ctx, w, m, "", nil)
	// Exactly what dnssrv does the instant ServeNotify returns.
	cancel()

	deadline := time.Now().Add(2 * time.Second)
	for f.rf.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if f.rf.count() == 0 {
		t.Fatal("the transfer never ran")
	}
	f.rf.mu.Lock()
	alive := f.rf.ctxAlive
	f.rf.mu.Unlock()
	if !alive {
		t.Error("the transfer inherited the cancelled request context")
	}
}

// A flood of NOTIFYs is the ordinary case — a primary editing ten records
// sends ten — and each gets its own immediate NOERROR while they collapse to
// one probe. Without this, "NOTIFY is cheap" is a probe amplifier pointed at
// our own primary.
func TestNotifyThrottlesTheWorkNotTheReply(t *testing.T) {
	clock := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	f := newNotifyFixture(t, notifyZone(),
		zones.WithNotifyServerNow(func() time.Time { return clock }))

	const sent = 5
	for i := 0; i < sent; i++ {
		reply := f.notify(t, notifyApex, dns.TypeSOA, "10.0.0.1", "", nil)
		if reply.Rcode != dns.RcodeSuccess {
			t.Fatalf("notify %d: rcode = %s, want NOERROR — the throttle must "+
				"never suppress the reply", i, dns.RcodeToString[reply.Rcode])
		}
	}

	deadline := time.Now().Add(2 * time.Second)
	for f.rf.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	// Let any wrongly-admitted extra work land before counting, or this
	// passes by winning a race rather than by throttling.
	time.Sleep(50 * time.Millisecond)
	if got := f.rf.count(); got != 1 {
		t.Errorf("%d NOTIFYs inside the window caused %d refreshes, want 1", sent, got)
	}
}
```

> `captureWriter` already exists in `internal/zones/transferserver_test.go` (line 927) — reuse it rather than adding a second one, so both gates are tested through the same writer. It is in the same `zones_test` package, so no import is needed.

- [ ] **Step 2: Run and watch them fail**

Run: `go test ./internal/zones/ -run 'TestNotify' -v`
Expected: FAIL — `undefined: zones.NewNotifyServer`

- [ ] **Step 3: Implement**

`internal/zones/notifyserver.go`. The shape mirrors `TransferServer`: a refusal struct, a pure `decide`, a `serve` that writes exactly one reply, and a throttle map guarded by a mutex held only across the check.

```go
// notifyRefusal is why a NOTIFY will not be acted on, and what to say about
// it. It is refusal's twin from transferserver.go; the two are separate types
// rather than one shared one because the rcodes mean different things — see
// the table above.
type notifyRefusal struct {
	rcode    int
	tsigCode uint16 // dns.RcodeBadKey/BadSig/BadTime, or 0 for no TSIG error RR
	reason   string // the log line
}

// notifyThrottle is the shortest interval between two SOA probes for one
// zone. A primary editing ten records sends ten NOTIFYs; each gets its own
// immediate NOERROR, because that is the peer's business, and they collapse
// to one probe. Without this, "NOTIFY is cheap" becomes a probe amplifier
// pointed at our own primary.
//
// It is the same shape as transferStateThrottle and deliberately not the same
// constant: that one throttles a database write, this one throttles a network
// round trip to somebody else's server.
const notifyThrottle = 5 * time.Second

// decide is the gate, and it is pure: everything it reads is an argument.
// That is what makes §9.10.2's table a test table rather than a description.
//
// primaries is resolved by the caller rather than here. ParsePrimaries takes
// a context and may do a DNS lookup for a hostname primary, and a lookup
// inside the gate of the server that answers DNS is the thing acl.go's own
// comment refuses for the same reason.
// decideZone runs the checks that need nothing but the message and the
// served snapshot. Split from decidePeer so ServeNotify can refuse a NOTIFY
// for a zone it does not hold *before* resolving any hostname — see
// ServeNotify's own comment on why that ordering matters.
func (n *NotifyServer) decideZone(m *dns.Msg) (*Zone, *notifyRefusal) {
	if len(m.Question) != 1 || m.Question[0].Qtype != dns.TypeSOA {
		return nil, &notifyRefusal{rcode: dns.RcodeFormatError, reason: "not a single SOA question"}
	}
	// The snapshot, never the store: a gate that read the store would decide
	// from data this server is not currently serving. D3's gate reads the
	// same snapshot for the same reason.
	z := n.res.Snapshot().Apex(qnameOf(m))
	if z == nil {
		return nil, &notifyRefusal{rcode: dns.RcodeNotAuth, reason: "no such zone"}
	}
	// Only a secondary is told by anyone. A primary owns its data, so a
	// NOTIFY for one is either a misconfiguration or an attempt to make this
	// server pull its own zone from somewhere else.
	if !strings.EqualFold(z.Type, "secondary") {
		return nil, &notifyRefusal{rcode: dns.RcodeNotAuth, reason: "zone is type " + z.Type}
	}
	if !z.Enabled {
		return nil, &notifyRefusal{rcode: dns.RcodeNotAuth, reason: "zone is disabled"}
	}
	return z, nil
}

// decidePeer runs the checks about *who sent this*, against a zone
// decideZone has already accepted and primaries the caller resolved.
func (n *NotifyServer) decidePeer(z *Zone, peer netip.Addr, primaries []netip.AddrPort, key string, tsigErr error) *notifyRefusal {
	// RFC 1996 §3.10 says to ignore a NOTIFY from a host that is not a known
	// master. dnsaur answers REFUSED instead — a deliberate divergence,
	// recorded in §9.9. Silence is indistinguishable from a firewall drop,
	// the usual cause here is a primaries list one address wrong, and a
	// NOTIFY and its refusal are both ~50 bytes, so there is no
	// amplification and 1:1 reflection is not a useful attack primitive.
	if !notifyPeerAllowed(primaries, peer) {
		return &notifyRefusal{rcode: dns.RcodeRefused, reason: "source is not a configured primary"}
	}

	// TSIG. The rows and their error codes are §9.5.5's exactly; only the
	// rcode differs, because this refusal is policy rather than authority.
	//
	// **The dns.Err* arms are deliberately NOT gated on the zone naming a
	// key**, and D3's sibling checks tsigErr unconditionally for the same
	// reason (transferserver.go:515). RFC 8945 §5.2.1 and §5.2.2 make BADKEY
	// and BADSIG mandatory regardless of local policy. Gate them and a NOTIFY
	// carrying a broken MAC to a keyless zone is *accepted*: signIfVerified
	// declines to sign, so the peer receives an unsigned NOERROR it must
	// discard, and retransmits per RFC 1996 §3.6 indefinitely. The zone's own
	// key requirement decides only whether an *absent* signature is refused.
	if tsigErr != nil {
		switch {
		case errors.Is(tsigErr, dns.ErrSecret), errors.Is(tsigErr, dns.ErrKeyAlg):
			return &notifyRefusal{rcode: dns.RcodeRefused, tsigCode: dns.RcodeBadKey, reason: "unknown TSIG key"}
		case errors.Is(tsigErr, dns.ErrSig):
			return &notifyRefusal{rcode: dns.RcodeRefused, tsigCode: dns.RcodeBadSig, reason: "TSIG did not verify"}
		case errors.Is(tsigErr, dns.ErrTime):
			return &notifyRefusal{rcode: dns.RcodeRefused, tsigCode: dns.RcodeBadTime, reason: "TSIG outside the fudge window"}
		case errors.Is(tsigErr, dnssrv.ErrTSIGUnsigned):
			// An absent signature is only a refusal for a zone that requires
			// one. A keyless zone accepts an unsigned NOTIFY and falls
			// through — which is the ordinary case.
			if z.TSIGKeyID != 0 {
				return &notifyRefusal{rcode: dns.RcodeRefused, reason: "zone requires TSIG and the message is unsigned"}
			}
		default:
			// dnssrv.ErrTSIGUnavailable (this server has no key store, so
			// nothing verified the message) and a key lookup that failed
			// because the store failed. Neither is the peer's fault and
			// neither is a TSIG error.
			//
			// SERVFAIL rather than REFUSED, matching TransferServer.decide
			// for the identical error value. REFUSED tells a correctly
			// configured peer we decline it, when in fact we were unable —
			// which sends the operator to the far end of the connection to
			// debug a problem that is on this one. D3 recorded that
			// reasoning; a second rule for one error value in one codebase
			// would be worse than either rule alone.
			return &notifyRefusal{rcode: dns.RcodeServerFailure, reason: "TSIG check failed: " + tsigErr.Error()}
		}
	}
	return nil
}

// notifyPeerAllowed reports whether peer is one of the zone's primaries.
//
// The port is deliberately not compared. A primary's `primaries` entry names
// where *we dial it*, and a NOTIFY arrives from an ephemeral source port —
// requiring a match would refuse every real NOTIFY ever sent.
func notifyPeerAllowed(primaries []netip.AddrPort, peer netip.Addr) bool {
	peer = peer.Unmap()
	for _, ap := range primaries {
		if ap.Addr().Unmap() == peer {
			return true
		}
	}
	return false
}

// ServeNotify answers one NOTIFY, then acts on it.
//
// The order is RFC 1996's and is not an optimisation. §4.7 has the responder
// enter its refresh state and §3.6 has the sender retransmitting until it
// gets a response, so a responder that waited for a transfer before replying
// would earn itself a second NOTIFY for the transfer already in flight.
func (n *NotifyServer) ServeNotify(ctx context.Context, w dns.ResponseWriter, m *dns.Msg, key string, tsigErr error) {
	peer := peerAddr(w)

	// The cheap, purely local checks first — qtype, apex, type, enabled — and
	// only then the primaries.
	//
	// **The ordering is load-bearing, not tidiness.** ParsePrimaries resolves
	// hostname entries with a live DNS lookup and no cache (primaries.go's
	// LookupNetIP). Resolving before these checks means one unauthenticated,
	// trivially source-spoofable UDP packet drives one outbound recursive
	// lookup, bounded only by NotifyTimeout and throttled by nothing — the
	// throttle below covers the transfer, not this. That is the hazard
	// acl.go's own comment refuses, and lifting the call out of decide
	// achieved purity without removing it. A zone whose primaries are IP
	// literals short-circuits with no lookup at all (primaries.go), which is
	// the common case; this ordering makes the hostname case cost nothing
	// for a NOTIFY that was going to be refused anyway.
	z, ref := n.decideZone(m)
	if ref == nil {
		var primaries []netip.AddrPort
		if aps, err := ParsePrimaries(ctx, n.dnsRes, z.Primaries); err == nil {
			primaries = aps
		} else {
			slog.Warn("resolving a zone's primaries for a notify failed",
				"zone", z.Name, "err", err)
		}
		ref = n.decidePeer(z, peer, primaries, key, tsigErr)
	}
	if ref != nil {
		slog.Debug("notify refused", "peer", peer, "qname", qnameOf(m),
			"rcode", dns.RcodeToString[ref.rcode], "reason", ref.reason)
		n.writeRefusal(w, m, ref, key, tsigErr)
		return
	}

	reply := new(dns.Msg)
	reply.SetReply(m)
	reply.Authoritative = true
	signIfVerified(reply, m, key, tsigErr)
	if err := w.WriteMsg(reply); err != nil {
		// The peer has gone; there is nothing to act on its behalf for, but
		// the zone may still be behind, so the work goes ahead anyway.
		slog.Debug("writing a notify reply failed", "peer", peer, "err", err)
	}

	// The throttle is on the work, never on the reply above.
	if !n.admit(z.ID) {
		slog.Debug("notify throttled", "zone", z.Name, "peer", peer)
		return
	}

	// A background context, not ctx: dnssrv cancels ctx the moment
	// ServeNotify returns, and a transfer started under it would be cut off
	// mid-zone. The same reasoning recordAttempt uses for its own write.
	go func() {
		workCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), dnssrv.TransferTimeout)
		defer cancel()
		row, err := n.zs.Zone(workCtx, z.ID)
		if err != nil {
			slog.Warn("reading a zone after a notify failed", "zone", z.Name, "err", err)
			return
		}
		n.act(workCtx, row)
	}()
}

// admit reports whether this zone may do the work now, and records that it
// did. The lock is held only across the check — the probe and the transfer
// happen well outside it, so a slow primary cannot serialise other zones.
func (n *NotifyServer) admit(zoneID int64) bool {
	now := n.now()
	n.mu.Lock()
	defer n.mu.Unlock()
	if last, ok := n.lastAct[zoneID]; ok && now.Sub(last) < notifyThrottle {
		return false
	}
	n.lastAct[zoneID] = now
	return true
}
```

`writeRefusal` is `TransferServer.writeRefusal`'s logic with this type's rcodes — reuse `errorTSIG` and `signIfVerified` from `transferserver.go` rather than writing second copies, since the BADKEY/BADSIG-unsigned and BADTIME-signed rule is identical and two implementations of it would drift.

`act` is Task 7's; until that task lands, this one's version calls `n.refresher.Refresh` directly, which is exactly what Task 6's tests assert.

- [ ] **Step 4: Run, then commit**

Run:
```bash
go test ./internal/zones/ -run 'TestNotify' -v
go test -race ./internal/zones/
```
Expected: PASS

```bash
gofmt -l internal cmd
~/go/bin/golangci-lint run ./internal/zones/
git add internal/zones/notifyserver.go internal/zones/notifyserver_test.go
git commit -m "feat(zones): the inbound NOTIFY gate

Authorises on the zone's primaries plus TSIG when the zone names a key,
answers before doing any work, and throttles the work rather than the
reply.

The TSIG rows are section 9.5.5's exactly, error codes included, and reuse
D3's errorTSIG and signIfVerified rather than a second copy. The rcode
differs: a refused transfer is NOTAUTH, about authority over the zone,
while a refused NOTIFY is a policy statement by a server that does hold
the zone.

REFUSED for an unknown source is a deliberate divergence from RFC 1996
section 3.10, which says to ignore. Silence is indistinguishable from a
firewall drop, and the usual cause is a primaries list one address wrong."
```

---

### Task 7: The SOA probe, and the never-transferred shortcut

**Files:**
- Modify: `internal/zones/transfer.go` — `ProbeSerial`
- Modify: `internal/zones/notifyserver.go` — the probe step in the handoff
- Test: `internal/zones/probe_test.go`

**Interfaces:**
- Consumes: `ParsePrimaries`, `Transferrer.zoneKey` (D2); `SerialNewer` (Task 1).
- Produces:
  ```go
  // ProbeSerial asks z's primaries for the zone's current serial.
  func (t *Transferrer) ProbeSerial(ctx context.Context, z store.Zone) (uint32, netip.AddrPort, error)

  // Probes is the half of *Transferrer NotifyServer needs, so a test can
  // drive the decision without a live primary.
  type Probes interface {
      ProbeSerial(ctx context.Context, z store.Zone) (uint32, netip.AddrPort, error)
  }
  ```

**Why the probe exists at all.** RFC 1996 §4.7 has the responder "enter the state it would if the zone's refresh timer had expired", which under RFC 1034 means asking for the SOA and comparing serials before transferring — and §9.9 has committed to that line since Milestone D was written, with nothing implementing it. A NOTIFY per record edit would otherwise be a full AXFR per record edit.

**Why the configured primaries and not the sender's address.** They are the same set by the time this runs — the gate admitted the message *because* the source matched `primaries` — so using the configured list costs nothing and keeps one answer to "who is this zone's primary".

**The `refreshed_at == 0` shortcut is the trap worth naming.** A secondary created through the API starts at `soa_serial = 1` (`zones_handlers.go:285`). A primary also at serial 1 makes `SerialNewer(1, 1)` false forever, so a probe-gated transfer would leave the zone permanently empty while reporting nothing wrong. Never-transferred is not a serial question.

- [ ] **Step 1: Write the failing tests**

```go
package zones_test

import (
	"context"
	"net"
	"testing"

	"github.com/miekg/dns"

	"github.com/aloks98/dnsaur/internal/store"
	"github.com/aloks98/dnsaur/internal/zones"
)

// soaServer answers one SOA question with the serial it was built with, and
// counts how many queries it saw. refuse makes it answer REFUSED instead,
// standing in for a primary that is up but unwilling.
type soaServer struct {
	addr    string
	queries atomic.Int64
	serial  uint32
	refuse  bool
}

func newSOAServer(t *testing.T, apex string, serial uint32, refuse bool) *soaServer {
	t.Helper()
	s := &soaServer{serial: serial, refuse: refuse}
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &dns.Server{PacketConn: pc, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, m *dns.Msg) {
		s.queries.Add(1)
		reply := new(dns.Msg)
		if s.refuse {
			reply.SetRcode(m, dns.RcodeRefused)
			_ = w.WriteMsg(reply)
			return
		}
		reply.SetReply(m)
		reply.Authoritative = true
		reply.Answer = []dns.RR{&dns.SOA{
			Hdr:     dns.RR_Header{Name: dns.Fqdn(apex), Rrtype: dns.TypeSOA, Class: dns.ClassINET, Ttl: 900},
			Ns:      "ns1." + dns.Fqdn(apex),
			Mbox:    "hostmaster." + dns.Fqdn(apex),
			Serial:  s.serial,
			Refresh: 3600, Retry: 600, Expire: 604800, Minttl: 300,
		}}
		_ = w.WriteMsg(reply)
	})}
	s.addr = pc.LocalAddr().String()
	go func() { _ = srv.ActivateAndServe() }()
	t.Cleanup(func() { _ = srv.Shutdown() })
	return s
}

func TestProbeSerialReadsThePrimarysSerial(t *testing.T) {
	primary := newSOAServer(t, notifyApex, 47, false)
	st := openTestStore(t)
	tr := zones.NewTransferrer(st.Zones(), st.TSIGKeys())

	z := notifyZone()
	z.Primaries = primary.addr

	serial, from, err := tr.ProbeSerial(context.Background(), z)
	if err != nil {
		t.Fatalf("ProbeSerial: %v", err)
	}
	if serial != 47 {
		t.Errorf("serial = %d, want 47", serial)
	}
	if from.String() != primary.addr {
		t.Errorf("answered by %s, want %s", from, primary.addr)
	}
}

// The primaries are tried in order and any failure moves to the next one,
// exactly as Transfer does — a primary that is up but refusing is a reason
// to ask another rather than to give up.
func TestProbeSerialFallsThroughToTheNextPrimary(t *testing.T) {
	refusing := newSOAServer(t, notifyApex, 0, true)
	good := newSOAServer(t, notifyApex, 47, false)
	st := openTestStore(t)
	tr := zones.NewTransferrer(st.Zones(), st.TSIGKeys())

	z := notifyZone()
	z.Primaries = refusing.addr + ", " + good.addr

	serial, from, err := tr.ProbeSerial(context.Background(), z)
	if err != nil {
		t.Fatalf("ProbeSerial: %v", err)
	}
	if serial != 47 || from.String() != good.addr {
		t.Errorf("got serial %d from %s, want 47 from %s", serial, from, good.addr)
	}
	if refusing.queries.Load() == 0 {
		t.Error("the first primary was never asked")
	}
}

// Every primary failing names every attempt, rather than reporting one.
func TestProbeSerialReportsEveryFailure(t *testing.T) {
	a := newSOAServer(t, notifyApex, 0, true)
	b := newSOAServer(t, notifyApex, 0, true)
	st := openTestStore(t)
	tr := zones.NewTransferrer(st.Zones(), st.TSIGKeys())

	z := notifyZone()
	z.Primaries = a.addr + ", " + b.addr

	if _, _, err := tr.ProbeSerial(context.Background(), z); err == nil {
		t.Fatal("ProbeSerial succeeded with every primary refusing")
	} else {
		for _, want := range []string{a.addr, b.addr} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not name %s", err, want)
			}
		}
	}
}

// ── The decision the probe feeds ─────────────────────────────────────────

// fakeProbes returns a fixed serial and counts calls.
type fakeProbes struct {
	serial uint32
	err    error
	calls  atomic.Int64
}

func (f *fakeProbes) ProbeSerial(context.Context, store.Zone) (uint32, netip.AddrPort, error) {
	f.calls.Add(1)
	return f.serial, netip.AddrPortFrom(netip.MustParseAddr("10.0.0.1"), 53), f.err
}

func TestNotifyTransfersOnlyWhenTheSerialAdvanced(t *testing.T) {
	tests := []struct {
		name         string
		localSerial  uint32
		remoteSerial uint32
		refreshedAt  int64
		wantProbe    bool
		wantTransfer bool
	}{
		{
			name: "the primary is ahead", localSerial: 10, remoteSerial: 11,
			refreshedAt: 1, wantProbe: true, wantTransfer: true,
		},
		{
			name: "we are already current", localSerial: 10, remoteSerial: 10,
			refreshedAt: 1, wantProbe: true, wantTransfer: false,
		},
		{
			name: "the primary went backwards", localSerial: 10, remoteSerial: 9,
			refreshedAt: 1, wantProbe: true, wantTransfer: false,
		},
		{
			// RFC 1982: a wrapped serial is newer, and this is the case a
			// naive `>` gets wrong.
			name: "the primary wrapped", localSerial: 4294967295, remoteSerial: 0,
			refreshedAt: 1, wantProbe: true, wantTransfer: true,
		},
		{
			// THE TRAP. A secondary created through the API starts at
			// soa_serial 1, so a primary also at 1 would make every
			// comparison say "not newer" and the zone would stay
			// permanently empty while reporting nothing wrong.
			name: "never transferred, identical serials", localSerial: 1, remoteSerial: 1,
			refreshedAt: 0, wantProbe: false, wantTransfer: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			z := notifyZone()
			z.SOASerial = tc.localSerial
			z.RefreshedAt = tc.refreshedAt
			probes := &fakeProbes{serial: tc.remoteSerial}
			f := newNotifyFixture(t, z, zones.WithNotifyProbes(probes))

			reply := f.notify(t, notifyApex, dns.TypeSOA, "10.0.0.1", "", nil)
			if reply.Rcode != dns.RcodeSuccess {
				t.Fatalf("rcode = %s, want NOERROR", dns.RcodeToString[reply.Rcode])
			}

			deadline := time.Now().Add(2 * time.Second)
			for probes.calls.Load() == 0 && f.rf.count() == 0 && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			time.Sleep(50 * time.Millisecond) // let a wrong extra call land

			if got := probes.calls.Load() > 0; got != tc.wantProbe {
				t.Errorf("probed = %v, want %v", got, tc.wantProbe)
			}
			if got := f.rf.count() > 0; got != tc.wantTransfer {
				t.Errorf("transferred = %v, want %v", got, tc.wantTransfer)
			}
		})
	}
}

// RFC 1996 §3.7–3.8: the answer-section SOA is "an unsecure hint", and "In no
// case shall the answer section of a NOTIFY request be used to update a
// slave's local data, or to indicate that a zone transfer needs to be
// undertaken, or to change the slave's zone refresh timers." Using it to skip
// the probe would be exactly the second of those three.
func TestNotifyIgnoresTheAnswerSectionSOA(t *testing.T) {
	z := notifyZone()
	z.SOASerial = 10
	probes := &fakeProbes{serial: 10} // the primary is NOT ahead
	f := newNotifyFixture(t, z, zones.WithNotifyProbes(probes))

	// A sender claiming, unsigned and unverifiable, that the zone is far
	// ahead. Believing it would transfer; the RFC forbids that.
	m := new(dns.Msg).SetNotify(dns.Fqdn(notifyApex))
	m.Answer = []dns.RR{&dns.SOA{
		Hdr:    dns.RR_Header{Name: dns.Fqdn(notifyApex), Rrtype: dns.TypeSOA, Class: dns.ClassINET, Ttl: 900},
		Ns:     "ns1." + dns.Fqdn(notifyApex),
		Mbox:   "hostmaster." + dns.Fqdn(notifyApex),
		Serial: 9999,
	}}
	w := &captureWriter{remote: &net.UDPAddr{IP: net.ParseIP("10.0.0.1"), Port: 40000}}
	f.ns.ServeNotify(context.Background(), w, m, "", nil)

	deadline := time.Now().Add(2 * time.Second)
	for probes.calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)

	if probes.calls.Load() == 0 {
		t.Error("the answer-section SOA was used instead of probing")
	}
	if f.rf.count() != 0 {
		t.Error("transferred on the strength of an unauthenticated hint")
	}
}
```

> `openTestStore` is a two-line helper (`store.Open` on `t.TempDir()`, registered for cleanup) — `newNotifyFixture` in Task 6 already does exactly this; factor it out there and reuse it here rather than opening a store twice.

- [ ] **Step 2: Run and watch them fail**

Run: `go test ./internal/zones/ -run 'Probe|AnswerSection|SerialAdvanced' -v`
Expected: FAIL — `tr.ProbeSerial undefined`, `zones.WithNotifyProbes undefined`

- [ ] **Step 3: Implement**

In `internal/zones/transfer.go`, beside `fetch`:

```go
// ProbeSerial asks z's primaries for the zone's current serial, and returns
// the first answer with the address that gave it.
//
// It is the mechanism RFC 1996 §4.7 leaves when it says a notified secondary
// "should enter the state it would if the zone's refresh timer had expired":
// under RFC 1034 that means asking for the SOA and comparing serials, not
// transferring outright. Without it a primary that edits ten records causes
// ten full zone transfers.
//
// The primaries are tried in the order written and any failure moves to the
// next, exactly as Transfer does and for the same reason — primaries are
// meant to be replicas of each other, so one refusing is a reason to ask
// another. The error names every attempt when they all fail.
//
// It is signed with the zone's key when it names one. A primary that requires
// TSIG on ordinary queries would otherwise refuse the probe and make a
// perfectly healthy zone look unreachable.
func (t *Transferrer) ProbeSerial(ctx context.Context, z store.Zone) (uint32, netip.AddrPort, error) {
	primaries, err := ParsePrimaries(ctx, t.res, z.Primaries)
	if err != nil {
		return 0, netip.AddrPort{}, fmt.Errorf("zone %q: %w", z.Name, err)
	}
	key, err := t.zoneKey(ctx, z)
	if err != nil {
		return 0, netip.AddrPort{}, err
	}

	var failures []error
	for _, ap := range primaries {
		if err := ctx.Err(); err != nil {
			return 0, netip.AddrPort{}, fmt.Errorf("zone %q: %w", z.Name, err)
		}
		serial, err := t.probeOne(ctx, z.Name, ap, key)
		if err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", ap, err))
			continue
		}
		return serial, ap, nil
	}
	if err := ctx.Err(); err != nil {
		return 0, netip.AddrPort{}, fmt.Errorf("zone %q: %w", z.Name, err)
	}
	return 0, netip.AddrPort{}, fmt.Errorf("zone %q: every primary failed: %w",
		z.Name, errors.Join(failures...))
}

// probeOne runs one SOA query against one primary.
func (t *Transferrer) probeOne(ctx context.Context, zoneName string, ap netip.AddrPort, key *store.TSIGKey) (uint32, error) {
	m := new(dns.Msg).SetQuestion(dns.Fqdn(zoneName), dns.TypeSOA)
	c := new(dns.Client)
	if key != nil {
		c.TsigProvider = tsigProviderFor(key) // the same helper fetch uses
		m.SetTsig(key.Name, key.Algorithm, tsigFudge, time.Now().Unix())
	}
	reply, _, err := c.ExchangeContext(ctx, m, ap.String())
	if err != nil {
		return 0, err
	}
	if reply.Rcode != dns.RcodeSuccess {
		return 0, fmt.Errorf("SOA probe answered %s", dns.RcodeToString[reply.Rcode])
	}
	for _, rr := range reply.Answer {
		soa, ok := rr.(*dns.SOA)
		if !ok {
			continue
		}
		// The SOA must name the zone we asked about. build() applies the
		// identical check to a transferred SOA (transfer.go, "the transfer
		// carries the SOA of %q, not of %q") and the reason is stronger
		// here: a probe is a single unsigned UDP exchange for a keyless
		// zone, so a misconfigured multi-tenant primary or an off-path
		// forgery could hand back an unrelated zone's serial. Accepting it
		// would make SerialNewer compare against the wrong number and, when
		// that number happens to be higher, silently decline a transfer
		// that should have happened — leaving a stale zone reporting
		// nothing wrong, which is the exact failure this task exists to
		// prevent.
		if !strings.EqualFold(dns.CanonicalName(soa.Hdr.Name), dns.CanonicalName(dns.Fqdn(zoneName))) {
			return 0, fmt.Errorf("SOA probe answered with the SOA of %q, not of %q", soa.Hdr.Name, zoneName)
		}
		return soa.Serial, nil
	}
	// A NOERROR with no SOA is a primary that answered the wrong question,
	// which is a failure of this probe rather than of the zone — reported so
	// the next primary is tried instead of the zone being called current.
	return 0, errors.New("SOA probe answered NOERROR with no SOA record")
}
```

> `tsigProviderFor` and `tsigFudge` are whatever `fetch` already uses for the same job (`internal/zones/transfer.go:299`). Read that function and reuse its exact mechanism rather than constructing a second one — a probe signed differently from a transfer is a bug that only appears against a TSIG-requiring primary.

In `internal/zones/notifyserver.go`, the handoff becomes:

```go
// act is what a NOTIFY causes, after the reply has already gone out.
//
// The order is RFC 1996's, and the middle step is the one that looks
// skippable and is not.
func (n *NotifyServer) act(ctx context.Context, z store.Zone) {
	// A zone that has never transferred is not a serial question. A secondary
	// created through the API starts at soa_serial 1, so a primary also at 1
	// would make every comparison say "not newer" and leave the zone
	// permanently empty while reporting nothing wrong.
	// n.probes != nil is not defensive padding: WithNotifyProbes is optional,
	// every Task 6 test omits it, and production does not wire it until
	// Task 10 — so without this guard act panics on a nil interface.
	//
	// The fallback is to transfer unconditionally, which is the pre-probe
	// behaviour and errs the safe way: an unnecessary AXFR costs bandwidth,
	// a skipped one leaves a secondary serving stale data. It is logged
	// rather than silent, because a NotifyServer running without probes has
	// lost the whole point of this task and nothing else would say so.
	if n.probes == nil {
		// Not silent. A NotifyServer without probes transfers on every
		// admitted NOTIFY — this task's whole protection, gone — and the
		// only other evidence would be a load graph nobody is watching.
		// See NewNotifyServer, which says the same thing once at startup.
		slog.Warn("notify: no SOA probe configured, transferring unconditionally",
			"zone", z.Name)
	}
	if z.RefreshedAt != 0 && n.probes != nil {
		remote, from, err := n.probes.ProbeSerial(ctx, z)
		if err != nil {
			slog.Warn("SOA probe after a notify failed", "zone", z.Name, "err", err)
			return
		}
		if !SerialNewer(remote, z.SOASerial) {
			// The NOTIFY was true and we were already current. RFC 1996 calls
			// it a hint, and this is the hint being correctly declined.
			slog.Debug("notify: already current",
				"zone", z.Name, "serial", z.SOASerial, "primary", from)
			return
		}
		slog.Info("notify: primary is ahead, transferring",
			"zone", z.Name, "ours", z.SOASerial, "theirs", remote, "primary", from)
	}
	if _, err := n.refresher.Refresh(ctx, z.ID); err != nil {
		slog.Warn("transfer after a notify failed", "zone", z.Name, "err", err)
	}
}
```

`WithNotifyProbes(p Probes)` is the option the tests inject through; production passes the `*Transferrer` the `Refresher` already holds.

- [ ] **Step 4: Run, then commit**

Run:
```bash
go test ./internal/zones/ -run 'Probe|AnswerSection|SerialAdvanced|TestNotify' -v
go test -race ./internal/zones/
```
Expected: PASS

Verify the trap test is real: temporarily drop the `z.RefreshedAt != 0` guard, re-run `TestNotifyTransfersOnlyWhenTheSerialAdvanced/never_transferred,_identical_serials`, confirm it fails, restore.

```bash
gofmt -l internal cmd
~/go/bin/golangci-lint run ./internal/zones/
git add internal/zones
git commit -m "feat(zones): probe the SOA before transferring on a notify

RFC 1996 section 4.7 has a notified secondary enter the state it would if
its refresh timer had expired, which under RFC 1034 means asking for the
SOA and comparing serials -- not transferring outright. Ten record edits
on a primary were otherwise ten full zone transfers.

A zone that has never transferred skips the probe and transfers outright.
A secondary created through the API starts at soa_serial 1, so a primary
also at 1 makes every comparison say not-newer and the zone would stay
permanently empty while reporting nothing wrong.

The answer-section SOA is ignored, per RFC 1996 section 3.8: in no case
shall it be used to indicate that a transfer needs to be undertaken."
```

---

### Task 8: The notifier pass — reconciliation and rounds

**Files:**
- Create: `internal/zones/notifier.go`
- Test: `internal/zones/notifier_test.go`

**Interfaces:**
- Consumes: `store.NotifyStore`, `store.ZoneStore` (Task 3); `ParseNotifyTo` (Task 2); `SerialNewer` (Task 1).
- Produces:
  ```go
  const MaxNotifyAttempts = 5

  type Notifier struct{ /* ... */ }
  type NotifyOption func(*Notifier)
  func NewNotifier(zs store.ZoneStore, ns store.NotifyStore, keys TSIGKeys, opts ...NotifyOption) *Notifier
  func WithNotifyNow(now func() time.Time) NotifyOption
  func WithNotifySender(s Sender) NotifyOption
  func (n *Notifier) Run(ctx context.Context)
  func (n *Notifier) Pass(ctx context.Context) error
  func (n *Notifier) Wake()

  // Sender is the one message-sending step, taken as an interface so the
  // pass can be tested without a socket. Task 9 implements it, and
  // NewNotifier defaults to that implementation — WithNotifySender is for
  // tests, so production never has to remember to supply one.
  type Sender interface {
      Send(ctx context.Context, target NotifyTarget, zone string, key *store.TSIGKey) error
  }
  ```

> **Ordering note.** Task 9 writes the real `Sender`, so while this task is in flight `NewNotifier`'s default is a stub that returns "not implemented". Task 9's Step 3 replaces it. Do not leave the stub reachable after Task 9 — `TestLoopbackNotifyDrivesTheTransfer` (Task 10) is what catches it if you do.

**Pending-ness is derived, never stored.** The pass, per row:

```
want := zone.soa_serial
if notified_at != 0 && !SerialNewer(want, notified_serial) { continue } // nothing to say
attempts := row.attempts
if row.pending_serial != want { attempts = 0 }                          // a new round
if attempts >= MaxNotifyAttempts { continue }                           // this round gave up
if now < row.next_attempt_at { continue }
send
```

This is the deliberate answer to the failure §9.4 names as this project's most repeated: an automatic path that skips a rule the human path enforces. There is no enqueue call for a future mutation path to forget.

**`Wake` is promptness, never correctness.** It goes beside the existing `s.reloadZones(r)` calls and into `Transferrer`'s install. Missing a call site makes a notify late by one tick; it cannot lose one. Say so in the doc comment, so a later reader does not "fix" it into a mandatory call.

- [ ] **Step 1: Write the failing tests**

```go
package zones_test

// fakeSender records what would have gone on the wire and fails on demand.
type fakeSender struct {
	mu   sync.Mutex
	sent []string // "target|zone"
	err  error
}

func (f *fakeSender) Send(_ context.Context, target zones.NotifyTarget, zone string, _ *store.TSIGKey) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.sent = append(f.sent, target.Addr()+"|"+zone)
	return nil
}

func (f *fakeSender) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}

// newNotifierFixture builds a Notifier over a real store with one primary
// zone carrying notifyTo, and an injected clock the test advances.
type notifierFixture struct {
	n      *zones.Notifier
	st     store.Store
	sender *fakeSender
	zoneID int64
	clock  time.Time
}

func newNotifierFixture(t *testing.T, notifyTo string, serial uint32) *notifierFixture {
	t.Helper()
	ctx := context.Background()
	st := openTestStore(t)
	z := store.Zone{
		Name: notifyApex, Type: "primary", Enabled: true, NotifyTo: notifyTo,
		SOANS: "ns1." + notifyApex, SOAMbox: "hostmaster." + notifyApex,
		SOASerial: serial, SOARefresh: 3600, SOARetry: 600,
		SOAExpire: 604800, SOAMinimum: 300, SOATTL: 900,
	}
	id, err := st.Zones().AddZone(ctx, z)
	if err != nil {
		t.Fatalf("AddZone: %v", err)
	}
	f := &notifierFixture{
		st: st, sender: &fakeSender{}, zoneID: id,
		clock: time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC),
	}
	f.n = zones.NewNotifier(st.Zones(), st.Notifies(), st.TSIGKeys(),
		zones.WithNotifyNow(func() time.Time { return f.clock }),
		zones.WithNotifySender(f.sender))
	return f
}

func (f *notifierFixture) pass(t *testing.T) {
	t.Helper()
	if err := f.n.Pass(context.Background()); err != nil {
		t.Fatalf("Pass: %v", err)
	}
}

func (f *notifierFixture) rows(t *testing.T) []store.ZoneNotify {
	t.Helper()
	rows, err := f.st.Notifies().ByZone(context.Background(), f.zoneID)
	if err != nil {
		t.Fatalf("ByZone: %v", err)
	}
	return rows
}

func (f *notifierFixture) setSerial(t *testing.T, serial uint32) {
	t.Helper()
	z, err := f.st.Zones().Zone(context.Background(), f.zoneID)
	if err != nil {
		t.Fatalf("Zone: %v", err)
	}
	z.SOASerial = serial
	if err := f.st.Zones().UpdateZone(context.Background(), z); err != nil {
		t.Fatalf("UpdateZone: %v", err)
	}
}

// The pass creates rows for targets that have none and removes rows whose
// target has left the list, so nothing has to hook zone PATCH and a
// hand-edited notify_to converges on the next pass.
func TestNotifierPassReconcilesRows(t *testing.T) {
	f := newNotifierFixture(t, "10.0.0.2:53, 10.0.0.3:53", 10)
	f.pass(t)
	if got := len(f.rows(t)); got != 2 {
		t.Fatalf("got %d rows, want 2", got)
	}

	z, _ := f.st.Zones().Zone(context.Background(), f.zoneID)
	z.NotifyTo = "10.0.0.3:53, 10.0.0.4:53"
	if err := f.st.Zones().UpdateZone(context.Background(), z); err != nil {
		t.Fatalf("UpdateZone: %v", err)
	}
	f.pass(t)

	got := map[string]bool{}
	for _, r := range f.rows(t) {
		got[r.Target] = true
	}
	if got["10.0.0.2:53"] || !got["10.0.0.3:53"] || !got["10.0.0.4:53"] {
		t.Errorf("rows = %v, want 10.0.0.3:53 and 10.0.0.4:53 only", got)
	}
}

// A new target is told at the current serial, because notified_at == 0 is
// "never told" — adding a secondary tells it rather than leaving it silent
// until the next unrelated edit.
func TestNotifierTellsANewTargetAtTheCurrentSerial(t *testing.T) {
	f := newNotifierFixture(t, "10.0.0.2:53", 47)
	f.pass(t)
	if f.sender.count() != 1 {
		t.Fatalf("sent %d notifies, want 1", f.sender.count())
	}
	rows := f.rows(t)
	if rows[0].NotifiedSerial != 47 || rows[0].NotifiedAt == 0 {
		t.Errorf("row = %+v, want delivered at serial 47", rows[0])
	}
}

// Nothing fires at startup or on a repeat pass: every row already records
// delivery at the current serial, so a restart is not news.
func TestNotifierIsQuietWhenNothingChanged(t *testing.T) {
	f := newNotifierFixture(t, "10.0.0.2:53", 47)
	f.pass(t)
	before := f.sender.count()
	f.pass(t)
	f.pass(t)
	if got := f.sender.count(); got != before {
		t.Errorf("sent %d notifies over three passes, want %d", got, before)
	}
}

// The trigger is serial detection, so a serial bumped by *any* path — here,
// written straight to the store with no notifier call at all — is picked up.
// This is the property that makes a forgotten call site a delay rather than
// a lost notify.
func TestNotifierNoticesASerialBumpNobodyAnnounced(t *testing.T) {
	f := newNotifierFixture(t, "10.0.0.2:53", 47)
	f.pass(t)
	f.setSerial(t, 48)
	f.pass(t)
	if got := f.sender.count(); got != 2 {
		t.Errorf("sent %d notifies, want 2 — the second serial was missed", got)
	}
}

// A round is defined by its serial. Attempts accumulate within it, back off,
// and stop at the budget.
func TestNotifierRetriesThenRestsWithinARound(t *testing.T) {
	f := newNotifierFixture(t, "10.0.0.2:53", 47)
	f.sender.err = errors.New("i/o timeout")

	// The first attempt fails and schedules the next.
	f.pass(t)
	rows := f.rows(t)
	if rows[0].Attempts != 1 || rows[0].LastError == "" {
		t.Fatalf("after one failure: %+v", rows[0])
	}
	if rows[0].NextAttemptAt <= f.clock.UnixMilli() {
		t.Errorf("next_attempt_at = %d, want it in the future", rows[0].NextAttemptAt)
	}

	// A pass before next_attempt_at does nothing at all.
	attemptsBefore := rows[0].Attempts
	f.pass(t)
	if got := f.rows(t)[0].Attempts; got != attemptsBefore {
		t.Errorf("attempts = %d before the backoff elapsed, want %d", got, attemptsBefore)
	}

	// Drive it to the budget.
	for i := 0; i < zones.MaxNotifyAttempts+2; i++ {
		f.clock = f.clock.Add(10 * time.Minute)
		f.pass(t)
	}
	if got := f.rows(t)[0].Attempts; got != zones.MaxNotifyAttempts {
		t.Errorf("attempts = %d, want it to stop at %d", got, zones.MaxNotifyAttempts)
	}
	if got := f.rows(t)[0].NotifiedAt; got != 0 {
		t.Errorf("notified_at = %d, want 0 — nothing was ever delivered", got)
	}
}

// Giving up is per round, not per target: the next serial bump resets the
// count and tries again, so a secondary that was down for an hour is retried
// the moment there is news, with no operator action.
func TestNotifierGiveUpResetsOnTheNextSerial(t *testing.T) {
	f := newNotifierFixture(t, "10.0.0.2:53", 47)
	f.sender.err = errors.New("i/o timeout")
	for i := 0; i < zones.MaxNotifyAttempts+2; i++ {
		f.clock = f.clock.Add(10 * time.Minute)
		f.pass(t)
	}
	exhausted := f.sender.count()
	if f.rows(t)[0].Attempts != zones.MaxNotifyAttempts {
		t.Fatalf("the round did not exhaust: %+v", f.rows(t)[0])
	}

	// News. The round resets and the target is tried again.
	f.sender.err = nil
	f.setSerial(t, 48)
	f.pass(t)
	if got := f.sender.count(); got != exhausted+1 {
		t.Errorf("sent %d after the serial advanced, want %d", got, exhausted+1)
	}
	rows := f.rows(t)
	if rows[0].NotifiedSerial != 48 || rows[0].Attempts != 0 || rows[0].LastError != "" {
		t.Errorf("row = %+v, want a clean delivery at serial 48", rows[0])
	}
}

// A zone with no targets does nothing, which is the overwhelmingly common
// case and must cost nothing.
func TestNotifierIgnoresZonesWithNoTargets(t *testing.T) {
	f := newNotifierFixture(t, "", 47)
	f.pass(t)
	if f.sender.count() != 0 {
		t.Errorf("sent %d notifies for a zone with no targets", f.sender.count())
	}
	if got := len(f.rows(t)); got != 0 {
		t.Errorf("created %d rows for a zone with no targets", got)
	}
}

// An unparseable stored notify_to notifies nobody rather than erroring the
// whole pass — one bad zone must not stop every other zone being told.
func TestNotifierFailsClosedOnAnUnparseableList(t *testing.T) {
	f := newNotifierFixture(t, "10.0.0.2:53", 47)
	z, _ := f.st.Zones().Zone(context.Background(), f.zoneID)
	z.NotifyTo = "10.0.0.2 key:bad name"
	if err := f.st.Zones().UpdateZone(context.Background(), z); err != nil {
		t.Fatalf("UpdateZone: %v", err)
	}
	if err := f.n.Pass(context.Background()); err != nil {
		t.Fatalf("Pass returned %v, want nil — one bad zone must not fail the pass", err)
	}
	if f.sender.count() != 0 {
		t.Errorf("sent %d notifies from an unparseable list", f.sender.count())
	}
}

// Wake makes the next pass happen now rather than at the tick, and is
// non-blocking however many times it is called.
func TestWakeIsNonBlockingAndCoalesces(t *testing.T) {
	f := newNotifierFixture(t, "10.0.0.2:53", 47)
	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			f.n.Wake()
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Wake blocked")
	}
}
```

- [ ] **Step 2: Run and watch them fail**

Run: `go test ./internal/zones/ -run 'Notifier|Wake' -v`
Expected: FAIL — `undefined: zones.NewNotifier`

- [ ] **Step 3: Implement**

`internal/zones/notifier.go`. The structure mirrors `Refresher`: a `Run` that passes immediately then ticks, an injected clock, and process-local state kept only where there is no column for it — which here is nothing at all, since the queue is durable.

```go
// MaxNotifyAttempts is how many times one round is tried before it rests.
//
// Five, backing off 5s → 10 → 20 → 40 → 80: about two and a half minutes,
// against a secondary refresh interval measured in hours. The budget can be
// this small precisely because it is not load-bearing — the target's own
// refresh timer picks the zone up regardless, which is what keeps NOTIFY a
// delivery optimisation rather than a correctness dependency.
const MaxNotifyAttempts = 5

const notifyBaseBackoff = 5 * time.Second

// notifyTick is how often the pass runs without a Wake. It bounds how late a
// notify can be, not how often one happens, and it is also the coalescer: a
// burst of edits inside one tick is one round at the newest serial.
const notifyTick = 5 * time.Second
```

The pass:

```go
// Pass reconciles every zone's rows and sends whatever is due.
//
// The error reported is a failure to *read* — a zone whose list will not
// parse, or a target that would not send, is recorded against that zone and
// does not fail the pass, because one bad zone must not stop the others being
// told.
func (n *Notifier) Pass(ctx context.Context) error {
	all, err := n.zs.Zones(ctx)
	if err != nil {
		return err
	}
	nowMs := n.now().UnixMilli()

	// **Two phases, and the order is a correctness requirement rather than
	// tidiness.** Reconcile every zone first, then read the queue back once.
	//
	// Reading the rows before reconciling looks like it saves nothing and
	// costs nothing, and it is a real bug: a brand-new target has no row
	// yet, so the lookup yields a zero ZoneNotify whose ID is 0, and the
	// NoteDelivered/NoteAttempt that follows addresses `WHERE id = 0` and
	// matches nothing. The send happens, the outcome is never recorded, and
	// because notified_at stays 0 the target is "never told" on the next
	// pass too — so every newly added target is notified again on every
	// pass, forever.
	parsed := make(map[int64][]NotifyTarget, len(all))
	for _, z := range all {
		// A disabled zone tells nobody, for the mirror of the reason the
		// refresher skips one: it answers nothing, so this server would
		// REFUSE the transfer its own NOTIFY invited. Telling a third
		// party's secondary to come and fetch a zone we will then refuse
		// is worse than silence — it is someone else's retry loop.
		//
		// The rows are left in place rather than deleted, so delivery
		// history survives a disable/enable cycle and re-enabling notifies
		// only if the serial actually moved. Same shape as refresh.go's
		// "puts it back in the ordinary schedule".
		if !z.Enabled {
			continue
		}
		targets, err := ParseNotifyTo(z.NotifyTo)
		if err != nil {
			// Fails closed, and loudly enough to fix: an unparseable stored
			// value notifies nobody. The API validates on write, so reaching
			// this means a hand-edited row. One bad zone does not fail the
			// pass — every other zone is still told.
			slog.Warn("zone notify_to will not parse; notifying nobody",
				"zone", z.Name, "err", err)
			continue
		}
		if err := n.reconcile(ctx, z, targets, nowMs); err != nil {
			slog.Warn("reconciling notify targets failed", "zone", z.Name, "err", err)
			continue
		}
		parsed[z.ID] = targets
	}

	// One query, after every row that should exist does.
	rowsByZone, err := n.rowsByZone(ctx)
	if err != nil {
		return err
	}
	for _, z := range all {
		// Deduped by address: ParseNotifyTo does not dedupe, and Reconcile
		// deliberately creates one row for a repeated target — so without
		// this both copies resolve to that row, pass the gates against the
		// same stale snapshot, and put two packets on the wire per pass.
		// A five-attempt round would cost ten sends.
		seen := make(map[string]bool, len(parsed[z.ID]))
		for _, t := range parsed[z.ID] {
			addr := t.Addr()
			if seen[addr] {
				continue
			}
			seen[addr] = true
			row, ok := rowsByZone[z.ID][addr]
			if !ok {
				// Reconcile just created it, so this means a concurrent
				// delete. Skipping is right: there is nothing to record
				// against, and the next pass will recreate it.
				continue
			}
			n.maybeSend(ctx, z, t, row, nowMs)
		}
	}
	return nil
}
```

`maybeSend` is the five-line decision from the top of this task, then the send, then exactly one of `NoteDelivered` / `NoteAttempt`. The key for a target is looked up by name through `n.keys` at send time — never from the row — so re-keying keeps history.

`Run` and `Wake`:

```go
// Run drives the pass until ctx is cancelled, starting with one pass
// immediately — Refresher.Run's shape, and for the same reason.
func (n *Notifier) Run(ctx context.Context) {
	t := time.NewTicker(notifyTick)
	defer t.Stop()
	n.pass(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n.pass(ctx)
		case <-n.wake:
			n.pass(ctx)
		}
	}
}

// Wake asks for a pass now rather than at the next tick.
//
// **It is promptness, never correctness, and that distinction is the design.**
// Whether there is anything to send is derived by comparing serials (see the
// 0012 migration), so a caller that forgets to Wake makes a notify late by one
// tick and cannot lose one. Do not turn this into a mandatory call on every
// mutation path: that is exactly the shape — an automatic path that has to
// remember a rule — that §9.4 records as this project's most repeated defect,
// and avoiding it is why the queue has no pending flag.
//
// Non-blocking, and coalescing: a thousand Wakes between two passes are one
// pass.
func (n *Notifier) Wake() {
	select {
	case n.wake <- struct{}{}:
	default:
	}
}
```

- [ ] **Step 4: Run, then commit**

Run:
```bash
go test ./internal/zones/ -run 'Notifier|Wake' -v
go test -race ./internal/zones/
```
Expected: PASS

```bash
gofmt -l internal cmd
~/go/bin/golangci-lint run ./internal/zones/
git add internal/zones/notifier.go internal/zones/notifier_test.go
git commit -m "feat(zones): the outbound notify pass

One pass reconciles each zone's target rows and sends whatever is due.
Pending-ness is derived by comparing the zone's serial against what each
target acknowledged, never stored -- so a serial bumped by any path,
including one nobody instrumented, is picked up by the next pass.

Rounds are defined by their serial: attempts accumulate and back off within
one, and giving up is per round, so the next serial bump retries a target
that was down without an operator clearing anything.

Wake is promptness, not correctness. Missing a call site delays a notify by
one tick and cannot lose one, and the doc comment says so -- turning it
into a mandatory call would reintroduce exactly the failure the derived
design avoids."
```

---

### Task 9: Sending, retrying, and stopping

**Files:**
- Create: `internal/zones/notifysend.go`
- Test: `internal/zones/notifysend_test.go`

**Interfaces:**
- Consumes: `NotifyTarget` (Task 2); `Sender` (Task 8).
- Produces:
  ```go
  type udpSender struct{ /* ... */ }
  func newUDPSender(res *net.Resolver) *udpSender
  func (s *udpSender) Send(ctx context.Context, target NotifyTarget, zone string, key *store.TSIGKey) error
  ```

**The three stop conditions are RFC 1996 §3.6's**, quoted rather than recalled: a master retransmits "until either too many copies have been sent (a 'timeout'), an ICMP message indicating that the port is unreachable, or until a NOTIFY response is received from the slave with a matching query ID, QNAME, IP source address, and UDP source port number." §4.8 adds: "When a master server receives a NOTIFY response, it deletes this query from the retry queue."

**So any response ends the round, whatever its rcode.** The RFC does not distinguish them, and a REFUSED will not become a NOERROR on retransmission. A non-NOERROR rcode is still written to `last_error`, because ending the round and being satisfied with the outcome are different things and only the second one is what the operator sees.

**ICMP port unreachable is its own case, not one more timeout.** In Go it surfaces as `ECONNREFUSED` on the read from a connected UDP socket. It is positive evidence that nothing is listening, so burning four more attempts on it delays nothing but the truth.

**The matching rule is not free.** A connected socket gives the source address and port, but the query ID and QNAME must be checked against what was sent, or an off-path response with a guessed ID ends a round that never landed.

- [ ] **Step 1: Write the failing tests**

```go
package zones_test

// notifyResponder answers NOTIFY with a fixed rcode and records what arrived.
type notifyResponder struct {
	addr     string
	received atomic.Int64
	rcode    int
	// silent drops every request, standing in for a target that is up but
	// not answering.
	silent bool
	last   atomic.Value // *dns.Msg
}

func newNotifyResponder(t *testing.T, rcode int, silent bool) *notifyResponder {
	t.Helper()
	r := &notifyResponder{rcode: rcode, silent: silent}
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &dns.Server{PacketConn: pc, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, m *dns.Msg) {
		r.received.Add(1)
		r.last.Store(m)
		if r.silent {
			return
		}
		reply := new(dns.Msg)
		reply.SetRcode(m, r.rcode)
		_ = w.WriteMsg(reply)
	})}
	r.addr = pc.LocalAddr().String()
	go func() { _ = srv.ActivateAndServe() }()
	t.Cleanup(func() { _ = srv.Shutdown() })
	return r
}

func targetFor(t *testing.T, addr string) zones.NotifyTarget {
	t.Helper()
	ts, err := zones.ParseNotifyTo(addr)
	if err != nil {
		t.Fatalf("ParseNotifyTo(%q): %v", addr, err)
	}
	return ts[0]
}

// The message is a NOTIFY: opcode 4, AA set, one SOA question for the zone.
func TestSendBuildsANotify(t *testing.T) {
	r := newNotifyResponder(t, dns.RcodeSuccess, false)
	s := zones.NewUDPSenderForTest()

	if err := s.Send(context.Background(), targetFor(t, r.addr), notifyApex, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	m, _ := r.last.Load().(*dns.Msg)
	if m == nil {
		t.Fatal("nothing arrived")
	}
	if m.Opcode != dns.OpcodeNotify {
		t.Errorf("opcode = %d, want %d (NOTIFY)", m.Opcode, dns.OpcodeNotify)
	}
	if !m.Authoritative {
		t.Error("AA not set")
	}
	if len(m.Question) != 1 || m.Question[0].Qtype != dns.TypeSOA ||
		m.Question[0].Name != dns.Fqdn(notifyApex) {
		t.Errorf("question = %v, want one SOA question for %s", m.Question, notifyApex)
	}
}

// RFC 1996 §3.6 and §4.8: a response ends the round whatever its rcode. A
// REFUSED will not become a NOERROR on retransmission.
func TestSendTreatsAnyResponseAsDelivered(t *testing.T) {
	for _, rcode := range []int{dns.RcodeSuccess, dns.RcodeRefused, dns.RcodeNotAuth, dns.RcodeServerFailure} {
		t.Run(dns.RcodeToString[rcode], func(t *testing.T) {
			r := newNotifyResponder(t, rcode, false)
			s := zones.NewUDPSenderForTest()
			err := s.Send(context.Background(), targetFor(t, r.addr), notifyApex, nil)
			if rcode == dns.RcodeSuccess {
				if err != nil {
					t.Fatalf("Send = %v, want nil", err)
				}
				return
			}
			// Delivered, but not satisfactory: the rcode is reported so it
			// reaches last_error and the screen, while the round still ends.
			if err == nil {
				t.Fatalf("Send = nil, want the rcode reported")
			}
			if !errors.Is(err, zones.ErrNotifyDelivered) {
				t.Errorf("error %v does not wrap ErrNotifyDelivered, so the "+
					"round would retransmit a refusal", err)
			}
			if !strings.Contains(err.Error(), dns.RcodeToString[rcode]) {
				t.Errorf("error %q does not name the rcode", err)
			}
		})
	}
}

// A target that is up but silent is a timeout, which does retry.
func TestSendTimesOutOnASilentTarget(t *testing.T) {
	r := newNotifyResponder(t, 0, true)
	s := zones.NewUDPSenderForTest()
	err := s.Send(context.Background(), targetFor(t, r.addr), notifyApex, nil)
	if err == nil {
		t.Fatal("Send = nil against a silent target")
	}
	if errors.Is(err, zones.ErrNotifyDelivered) {
		t.Error("a timeout was treated as delivered")
	}
	if errors.Is(err, zones.ErrNotifyUnreachable) {
		t.Error("a timeout was treated as unreachable")
	}
}

// RFC 1996 §3.6's second stop condition. Nothing is listening on a closed
// port, so the kernel returns ECONNREFUSED — positive evidence, distinct
// from a timeout, and worth reporting as its own case rather than burning
// four more attempts on it.
func TestSendReportsPortUnreachableDistinctly(t *testing.T) {
	// Bind and immediately release, so the port is almost certainly closed.
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := pc.LocalAddr().String()
	_ = pc.Close()

	s := zones.NewUDPSenderForTest()
	err = s.Send(context.Background(), targetFor(t, addr), notifyApex, nil)
	if err == nil {
		t.Fatal("Send = nil against a closed port")
	}
	if !errors.Is(err, zones.ErrNotifyUnreachable) {
		t.Skipf("the platform did not surface ICMP port unreachable: %v", err)
	}
}

// §3.6 requires the response to match the request's query ID and QNAME. A
// connected socket covers the source address and port; these two do not come
// for free, and without them an off-path response with a guessed ID ends a
// round that never landed.
func TestSendRejectsAMismatchedResponse(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	// A responder that answers with the wrong ID.
	go func() {
		buf := make([]byte, 512)
		n, from, err := pc.ReadFrom(buf)
		if err != nil {
			return
		}
		m := new(dns.Msg)
		if m.Unpack(buf[:n]) != nil {
			return
		}
		reply := new(dns.Msg)
		reply.SetReply(m)
		reply.Id = m.Id + 1 // the mismatch
		out, _ := reply.Pack()
		_, _ = pc.WriteTo(out, from)
	}()

	s := zones.NewUDPSenderForTest()
	if err := s.Send(context.Background(), targetFor(t, pc.LocalAddr().String()), notifyApex, nil); err == nil {
		t.Fatal("Send accepted a response whose ID did not match")
	}
}

// A hostname target is resolved at send time, so a target that moves is
// followed — the reason ParseNotifyTo does not resolve.
func TestSendResolvesAHostnameAtSendTime(t *testing.T) {
	// localhost resolves without a network, and is enough to prove the
	// resolution step happens here rather than at parse time.
	r := newNotifyResponder(t, dns.RcodeSuccess, false)
	_, port, _ := net.SplitHostPort(r.addr)
	s := zones.NewUDPSenderForTest()
	if err := s.Send(context.Background(), targetFor(t, "localhost:"+port), notifyApex, nil); err != nil {
		t.Fatalf("Send to a hostname target: %v", err)
	}
	if r.received.Load() == 0 {
		t.Error("nothing arrived at the resolved address")
	}
}
```

**Three tests the list above does not cover, and must.** `TestSendRejectsAMismatchedResponse` mismatches only the **ID**, so the QNAME half of RFC 1996 §3.6's matching rule goes untested — and nothing exercises TSIG signing on this path at all, even though a NOTIFY signed differently from a transfer is the exact bug the "reuse `fetch`'s mechanism" instruction exists to prevent. Add:

- `TestSendRejectsAResponseForTheWrongQuestion` — a responder that echoes the right ID but a different QNAME.
- `TestSendSignsWithTSIGWhenAKeyIsGiven` — against a responder that verifies, mirroring `transfer_test.go`'s existing pair.
- `TestSendAgainstATSIGResponderFailsUnsigned` — the negative half, so the first cannot pass against a responder that ignores signatures.
- `TestSendRejectsAnUnsignedResponseToASignedNotify` — a responder that verifies the request and answers **without** signing. Without this the TSIG pair above proves only that we sign correctly, never that the reply is authenticated, which is the half an attacker cares about.

> `NewUDPSenderForTest` is an exported constructor over the unexported `udpSender`, needed because this package's tests are external. Give it the same body as the production constructor with a short send timeout, and say in its doc comment that it exists for the external test package rather than for production — the same note `WithTransferHook` already carries.

- [ ] **Step 2: Run and watch them fail**

Run: `go test ./internal/zones/ -run 'TestSend' -v`
Expected: FAIL — `undefined: zones.NewUDPSenderForTest`

- [ ] **Step 3: Implement**

`internal/zones/notifysend.go`. The two sentinel errors carry the classification the pass acts on:

```go
// ErrNotifyDelivered marks a response that arrived and ended the round, but
// was not NOERROR. RFC 1996 §3.6 stops retransmitting on *any* response and
// §4.8 deletes the query from the retry queue without inspecting the rcode —
// a REFUSED will not become a NOERROR on retransmission. It is still an error
// so the rcode reaches last_error and the screen: ending the round and being
// satisfied with the outcome are different things.
var ErrNotifyDelivered = errors.New("notify answered")

// ErrNotifyUnreachable marks RFC 1996 §3.6's second stop condition, "an ICMP
// message indicating that the port is unreachable", which Go surfaces as
// ECONNREFUSED on the read from a connected UDP socket. It is positive
// evidence that nothing is listening rather than an absence of evidence, so
// the pass stops the round on it instead of spending the rest of the budget
// re-establishing the same fact.
var ErrNotifyUnreachable = errors.New("notify target unreachable")
```

`Send` resolves the host (via `net.Resolver`, at send time), dials a connected UDP socket, packs `new(dns.Msg).SetNotify(dns.Fqdn(zone))`, signs it when `key != nil` with the same mechanism `Transferrer.fetch` uses, writes, and reads one response — checking ID **and question name** before accepting it.

**Resolution fans out over every address, not the first.** A hostname commonly resolves to both an A and an AAAA record, and picking one deterministically breaks on ordinary hosts: on Linux `localhost` resolves IPv6-first, so a responder bound to `127.0.0.1` is never reached and the failure looks like an unreachable peer rather than a resolution choice. Try each resolved address in turn, exactly as `Transferrer.Transfer` already does when walking a zone's primaries.

Four properties of that walk that are easy to get subtly wrong, each of which has bitten this file:

- **Each address gets its own timeout, not a share of one deadline.** A single `context.WithTimeout` installed on every socket means a first address that silently *drops* packets — the ordinary DROP-firewalled AAAA case, as opposed to the REJECT case `localhost` happens to produce — consumes the entire budget, and the second address inherits an already-expired deadline. That is not a second attempt, it is an instant `i/o timeout`. `Transferrer.fetch` gives each primary its own dial and read timeouts for exactly this reason; sharing one deadline rescues only the fail-fast half of the case the fan-out was added for.
- **Accumulate every address's error and `errors.Join` them**, as `Transfer` does under "every primary failed". Keeping only the last one shows an operator half the story — and worse, lets the *classification* be decided by whichever address happened to be last: address 1 timing out while address 2 answers ECONNREFUSED would return `ErrNotifyUnreachable` and end the round after a single attempt, even though the address that mattered was merely slow.
- **Check `ctx.Err()` before each address**, so a cancellation mid-fan-out reports as one rather than as a confusing per-address I/O error. `transfer.go` does this in its own loop.
- **`Send` must not return `nil` for an empty address list.** Returning the accumulated error implicitly assumes `resolve` never yields an empty non-error slice; if that invariant ever slips, `Send` reports "delivered" having sent nothing.

**The read waits out the deadline; it does not stop at the first packet.** Reading exactly once means a datagram failing the ID or QNAME check ends the attempt, and a legitimate response arriving a millisecond later is never read — so an off-path sender who reaches the ephemeral port with a guessed ID burns one of the five attempts in the round, repeatably. Loop on the read, discard non-matching packets, and let the connection deadline end the attempt.

**A signed NOTIFY requires a signed response, and the library will not do this for you.** `miekg/dns@v1.1.72`'s `client.go:267` verifies only `if t := m.IsTsig(); t != nil` — a reply carrying no TSIG record skips verification **entirely**. So when the target names a key, a spoofed *unsigned* reply that guesses the ID and QNAME ends the round exactly as an authentic signed one would, and TSIG contributes nothing at all to the response path. RFC 8945 §5.4 requires a signed request's response to be signed. After the read, when `key != nil`, reject `reply.IsTsig() == nil`.

The pass in Task 8 then classifies:

```go
	err := n.sender.Send(ctx, t, z.Name, key)
	switch {
	case err == nil:
		n.ns.NoteDelivered(ctx, row.ID, want, nowMs)
	case errors.Is(err, ErrNotifyDelivered), errors.Is(err, ErrNotifyUnreachable):
		// The round is over either way — one because the peer answered, the
		// other because nothing is there to answer. Recorded, not retried.
		n.ns.NoteAttempt(ctx, row.ID, want, MaxNotifyAttempts, 0, err.Error())
	default:
		n.ns.NoteAttempt(ctx, row.ID, want, attempts+1,
			nowMs+backoffFor(attempts).Milliseconds(), err.Error())
	}
```

> Note the `ErrNotifyDelivered` branch writes `MaxNotifyAttempts` rather than `attempts+1`: the round is finished, and reusing the give-up marker is what stops the next pass picking it up again without inventing a fourth state. Add a test in Task 8's file asserting a REFUSED target is not retried on the following pass.

- [ ] **Step 4: Run, then commit**

Run:
```bash
go test ./internal/zones/ -run 'TestSend|Notifier' -v
go test -race ./internal/zones/
```
Expected: PASS. `TestSendReportsPortUnreachableDistinctly` may skip on a platform that does not surface ICMP; that is the documented behaviour of the skip, not a failure.

```bash
gofmt -l internal cmd
~/go/bin/golangci-lint run ./internal/zones/
git add internal/zones/notifysend.go internal/zones/notifysend_test.go internal/zones/notifier.go internal/zones/notifier_test.go
git commit -m "feat(zones): send NOTIFY over UDP, with RFC 1996's stop conditions

All three of section 3.6's: a response, the attempt budget, and ICMP port
unreachable -- which Go surfaces as ECONNREFUSED on a connected socket and
which is positive evidence rather than an absence of it, so the round stops
instead of spending the rest of its budget.

Any response ends the round whatever its rcode, per section 4.8, since a
REFUSED will not become a NOERROR on retransmission -- but the rcode is
still recorded, because ending the round and being satisfied with the
outcome are different things.

The response's ID and QNAME are checked against what was sent. The
connected socket covers the source address and port; those two do not come
for free, and an off-path response with a guessed ID would otherwise end a
round that never landed."
```

---

### Task 10: Wiring, and dnsaur notifying dnsaur

**Files:**
- Modify: `internal/app/app.go` — construction, the `WithNotifies` option, `a.bg`, and `Wake` from `WithNotifyWake`
- Modify: `internal/zones/transfer.go` — the `WithNotifyWake` option and its call in `install`
- Modify: `internal/api/zones_handlers.go`, `zonerecords_handlers.go`, `zonefile_handlers.go`, `autoptr.go` — `Wake` beside each `reloadZones`
- Test: `internal/zones/loopback_test.go` (extend)

**Interfaces:**
- Consumes: everything above.
- Produces: a running system; `App.NotifyZones()` on the `Reloader` interface the API already uses.

**The end-to-end proof.** D3's `loopback_test.go` already stands up a dnsaur primary and a dnsaur secondary and transfers between them. D4 extends it: the primary's `notify_to` names the secondary, a record changes on the primary, and the secondary ends up holding it *without its refresh timer ever firing* — the whole point of the milestone. It must fail if either half is wrong.

- [ ] **Step 1: Write the failing tests**

```go
// internal/zones/loopback_test.go — append.

// listenNotify stands the secondary's DNS listener up around a NotifyServer,
// which newTransferFixture does not do — it asks its resolver's middleware
// directly and never binds a socket. A real NOTIFY needs a real listener.
//
// Returns the address the primary's notify_to should name.
func listenNotify(t *testing.T, f *transferFixture) string {
	t.Helper()
	ns := zones.NewNotifyServer(f.resolver, f.st.Zones(), f.refresher(),
		zones.WithNotifyProbes(f.transferrer()))
	// Reaching the pipeline means the intercept did not fire, and NOTIMP is
	// an rcode the gate never produces — the same trick newXFRFixture uses.
	srv := dnssrv.NewServer("127.0.0.1:0", dnssrv.HandlerFunc(
		func(_ context.Context, req *dnssrv.Request) (*dnssrv.Response, error) {
			t.Errorf("a NOTIFY reached the pipeline: %v", req.Msg.Question)
			m := new(dns.Msg)
			m.SetRcode(req.Msg, dns.RcodeNotImplemented)
			return &dnssrv.Response{Msg: m}, nil
		}),
		dnssrv.WithTSIGKeys(f.st.TSIGKeys()),
		dnssrv.WithNotifies(ns))
	if err := srv.Start(); err != nil {
		t.Fatalf("secondary listener: %v", err)
	}
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	return srv.Addr()
}

// awaitAnswer polls the secondary's resolver until qname resolves, or the
// deadline passes. Polling rather than sleeping a fixed interval: the whole
// point is that the transfer happens promptly, and a fixed sleep would either
// be flaky or slow.
func awaitAnswer(t *testing.T, f *transferFixture, qname string, qtype uint16) *dns.Msg {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if err := f.resolver.Reload(context.Background()); err != nil {
			t.Fatalf("Reload: %v", err)
		}
		m := askResolver(t, f.resolver, qname, qtype)
		if m.Rcode == dns.RcodeSuccess && len(m.Answer) > 0 {
			return m
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s never resolved on the secondary within the deadline", qname)
	return nil
}

// addRecordAndBump adds one A record to the primary and advances its serial,
// the way an API write does — through the store, with no notifier call, so
// what makes this reach the secondary is the pass noticing the serial.
func addRecordAndBump(t *testing.T, f *xfrFixture, name, addr string) {
	t.Helper()
	ctx := context.Background()
	if _, err := f.st.Zones().AddRecord(ctx, store.ZoneRecord{
		ZoneID: f.zoneID, Name: name, Type: "A", TTL: 300, Rdata: addr, Enabled: true,
	}); err != nil {
		t.Fatalf("AddRecord: %v", err)
	}
	if err := f.st.Zones().BumpSerial(ctx, f.zoneID); err != nil {
		t.Fatalf("BumpSerial: %v", err)
	}
	if err := f.res.Reload(ctx); err != nil {
		t.Fatalf("primary Reload: %v", err)
	}
}

// dnsaur notifying dnsaur, end to end: a record changes on the primary, the
// primary notifies, the secondary probes, sees a newer serial, transfers, and
// answers the new record — all before any refresh timer could have fired.
//
// **What actually makes this test discriminate — corrected 2026-09-02.**
//
// The plan originally claimed the long refresh interval was load-bearing:
// that without it the scheduler would eventually transfer anyway and the
// test would pass on a build where NOTIFY does nothing. **That is false for
// this fixture, and was verified empirically rather than reasoned about.**
// `newTransferFixture` constructs no `Refresher` and starts no `Run`
// goroutine, and its clock is frozen (`transfer_test.go`), so no scheduled
// transfer can occur here at any interval.
//
// What makes the test discriminate is simpler and stronger: nothing in this
// fixture drives a transfer except the NOTIFY. The record appears only if
// the notify path works end to end.
//
// The long interval stays as defensive practice — it keeps the test honest
// if a future fixture ever does run a scheduler — but it is documentation,
// not the mechanism, and a reader should not trust it as the guard.
func TestLoopbackNotifyDrivesTheTransfer(t *testing.T) {
	ctx := context.Background()
	primary := newXFRFixture(t, loopbackPrimaryZone("127.0.0.0/8"), loopbackRecords())
	sec := newTransferFixture(t, primary.addr, 0)
	secAddr := listenNotify(t, sec)

	// A first transfer, so refreshed_at != 0 and the rest of this exercises
	// the probe path rather than Task 7's never-transferred shortcut.
	if _, err := sec.transferrer().Transfer(ctx, sec.zone(t)); err != nil {
		t.Fatalf("initial transfer: %v", err)
	}

	// The scheduler must not be able to explain what follows.
	z := sec.zone(t)
	z.SOARefresh = 86400
	z.SOARetry = 86400
	if err := sec.st.Zones().UpdateZone(ctx, z); err != nil {
		t.Fatalf("UpdateZone: %v", err)
	}

	// The primary now names the secondary, and gains a record.
	pz, err := primary.st.Zones().Zone(ctx, primary.zoneID)
	if err != nil {
		t.Fatalf("primary Zone: %v", err)
	}
	pz.NotifyTo = secAddr
	if err := primary.st.Zones().UpdateZone(ctx, pz); err != nil {
		t.Fatalf("primary UpdateZone: %v", err)
	}
	addRecordAndBump(t, primary, "notified", "10.20.0.99")

	notifier := zones.NewNotifier(primary.st.Zones(), primary.st.Notifies(), primary.st.TSIGKeys())
	if err := notifier.Pass(ctx); err != nil {
		t.Fatalf("notifier pass: %v", err)
	}

	m := awaitAnswer(t, sec, "notified."+transferApex, dns.TypeA)
	a, ok := m.Answer[0].(*dns.A)
	if !ok || a.A.String() != "10.20.0.99" {
		t.Fatalf("answer = %v, want the record added on the primary", m.Answer)
	}

	// The whole zone arrived, not a coincidence: the serials agree.
	after := sec.zone(t)
	pzAfter, _ := primary.st.Zones().Zone(ctx, primary.zoneID)
	if after.SOASerial != pzAfter.SOASerial {
		t.Errorf("secondary serial = %d, primary = %d", after.SOASerial, pzAfter.SOASerial)
	}
}

// The signed path, which is what a per-target key exists for: without it a
// dnsaur primary sends unsigned, and a dnsaur secondary whose zone names a
// key refuses it — dnsaur unable to notify itself through a configuration it
// fully supports.
//
// The secondary's zone carries tsig_key_id, so Task 6's gate REFUSES an
// unsigned NOTIFY. A transfer that happens here can only have been caused by
// a signed one.
func TestLoopbackNotifyIsSignedWhenTheTargetNamesAKey(t *testing.T) {
	ctx := context.Background()
	keyName := "ns2." + transferApex
	primary := newXFRFixture(t, loopbackPrimaryZone("key:"+dns.CanonicalName(keyName)), loopbackRecords())
	sec := newTransferFixture(t, primary.addr, 0)
	_, secKeyID := loopbackKey(t, primary.st, sec.st, keyName)
	secAddr := listenNotify(t, sec)

	z := sec.zone(t)
	z.TSIGKeyID = secKeyID
	z.SOARefresh = 86400
	z.SOARetry = 86400
	if err := sec.st.Zones().UpdateZone(ctx, z); err != nil {
		t.Fatalf("UpdateZone: %v", err)
	}
	if _, err := sec.transferrer().Transfer(ctx, sec.zone(t)); err != nil {
		t.Fatalf("initial signed transfer: %v", err)
	}

	pz, _ := primary.st.Zones().Zone(ctx, primary.zoneID)
	pz.NotifyTo = secAddr + " key:" + dns.CanonicalName(keyName)
	if err := primary.st.Zones().UpdateZone(ctx, pz); err != nil {
		t.Fatalf("primary UpdateZone: %v", err)
	}
	addRecordAndBump(t, primary, "signed", "10.20.0.98")

	notifier := zones.NewNotifier(primary.st.Zones(), primary.st.Notifies(), primary.st.TSIGKeys())
	if err := notifier.Pass(ctx); err != nil {
		t.Fatalf("notifier pass: %v", err)
	}

	m := awaitAnswer(t, sec, "signed."+transferApex, dns.TypeA)
	if a, ok := m.Answer[0].(*dns.A); !ok || a.A.String() != "10.20.0.98" {
		t.Fatalf("answer = %v, want the record added on the primary", m.Answer)
	}
}

// The unsigned control for the test above: with the secondary's zone keyed
// and the primary's notify_to naming no key, the NOTIFY is refused and
// nothing transfers inside the window. Without this, the signed test could
// pass on a build that ignores keys entirely.
func TestLoopbackUnsignedNotifyToAKeyedSecondaryIsRefused(t *testing.T) {
	ctx := context.Background()
	keyName := "ns2." + transferApex
	primary := newXFRFixture(t, loopbackPrimaryZone("key:"+dns.CanonicalName(keyName)), loopbackRecords())
	sec := newTransferFixture(t, primary.addr, 0)
	_, secKeyID := loopbackKey(t, primary.st, sec.st, keyName)
	secAddr := listenNotify(t, sec)

	z := sec.zone(t)
	z.TSIGKeyID = secKeyID
	z.SOARefresh = 86400
	z.SOARetry = 86400
	if err := sec.st.Zones().UpdateZone(ctx, z); err != nil {
		t.Fatalf("UpdateZone: %v", err)
	}
	if _, err := sec.transferrer().Transfer(ctx, sec.zone(t)); err != nil {
		t.Fatalf("initial signed transfer: %v", err)
	}

	pz, _ := primary.st.Zones().Zone(ctx, primary.zoneID)
	pz.NotifyTo = secAddr // deliberately no key:
	if err := primary.st.Zones().UpdateZone(ctx, pz); err != nil {
		t.Fatalf("primary UpdateZone: %v", err)
	}
	addRecordAndBump(t, primary, "unsigned", "10.20.0.97")

	notifier := zones.NewNotifier(primary.st.Zones(), primary.st.Notifies(), primary.st.TSIGKeys())
	if err := notifier.Pass(ctx); err != nil {
		t.Fatalf("notifier pass: %v", err)
	}

	// Give it the same window the positive test gets, then assert nothing
	// arrived. The refusal is the point.
	time.Sleep(500 * time.Millisecond)
	if err := sec.resolver.Reload(ctx); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	m := askResolver(t, sec.resolver, "unsigned."+transferApex, dns.TypeA)
	if m.Rcode == dns.RcodeSuccess && len(m.Answer) > 0 {
		t.Fatal("an unsigned NOTIFY to a keyed secondary caused a transfer")
	}
}
```

> The cascade needs a third dnsaur and adds no new mechanism — the pass compares serials whatever wrote them, and `Transferrer`'s install writes the primary's serial verbatim. It is covered by `TestNotifierNoticesASerialBumpNobodyAnnounced` (Task 8), which proves exactly the property the cascade relies on, so a third fixture here would test the same code twice. If you want it end to end anyway, build it from `TestLoopbackNotifyDrivesTheTransfer` with the middle zone carrying `notify_to`.

> `xfrFixture` may not currently export `zoneID`, `st` or `res` — check `internal/zones/transferserver_test.go` and add whatever accessor is missing rather than reaching into unexported state from another file. `transferFixture.refresher()` likewise may need adding, mirroring its existing `transferrer()`.

- [ ] **Step 2: Run and watch them fail**

Run: `go test ./internal/zones/ -run 'Loopback' -v`
Expected: FAIL — the new name never appears on the secondary within the deadline, because nothing sends a NOTIFY yet.

- [ ] **Step 3: Implement**

`internal/app/app.go`, in the constructor. Note the ordering constraint: the `Transferrer` needs the notifier's `Wake` and the notifier needs nothing from it, so the notifier is built first.

```go
	// Built before the Transferrer, which takes its Wake for the cascade.
	a.notifier = zones.NewNotifier(st.Zones(), st.Notifies(), st.TSIGKeys())
	a.zoneRefresh = zones.NewRefresher(st.Zones(),
		zones.NewTransferrer(st.Zones(), st.TSIGKeys(),
			zones.WithReload(a.resolver.Reload),
			// The cascade: a secondary that just installed a zone may have
			// downstream secondaries of its own. Nothing cascade-specific
			// happens here — the pass compares serials, and the install has
			// just written the primary's serial verbatim.
			zones.WithNotifyWake(a.notifier.Wake)))
	a.xfrOut = zones.NewTransferServer(a.resolver, st.Zones())
	// The inbound half. It shares the Refresher the scheduler drives, so a
	// notify-triggered transfer takes the same per-zone lock a scheduled one
	// does rather than racing it.
	//
	// **WithNotifyProbes is not optional here even though the option is.**
	// Without it act skips the SOA probe and transfers on every NOTIFY —
	// the pre-Task-7 behaviour, silently, with a log line as the only
	// signal. A primary editing ten records would cause ten full zone
	// transfers. Task 10's test posture includes asserting the wired
	// server actually probes.
	a.notifyIn = zones.NewNotifyServer(a.resolver, st.Zones(), a.zoneRefresh,
		zones.WithNotifyProbes(a.zoneRefresh.Transferrer()))
```

> `Refresher.Transferrer()` is a one-line accessor to add — the `Refresher` already holds the `*Transferrer` in its `tr` field, and `NotifyServer` needs it for `ProbeSerial`. Alternatively construct the `Transferrer` into a local and pass it to both; prefer whichever reads better against the surrounding code, but do not construct two.

In `Start`, on every listener:

```go
		s := dnssrv.NewServer(addr, handler,
			dnssrv.WithTSIGKeys(st.TSIGKeys()),
			dnssrv.WithTransfers(a.xfrOut),
			dnssrv.WithNotifies(a.notifyIn))
```

In `a.bg`, beside `a.zoneRefresh.Run`:

```go
		// Outbound NOTIFY. It passes once before its first tick, which finds
		// nothing on a healthy restart — every row already records delivery
		// at the current serial — and catches up anything that changed while
		// the process was down.
		a.notifier.Run,
```

And the API's `Reloader` gains a method, so handlers can nudge:

```go
func (a *App) NotifyZones() { a.notifier.Wake() }
```

with `s.notifyZones()` called beside each existing `s.reloadZones(r)` in `zones_handlers.go` (3 sites), `zonerecords_handlers.go` (3 sites), `zonefile_handlers.go` (2 sites), and after the `BumpSerial` in `autoptr.go`. Each call site gets no comment; the mechanism is documented once, on `Wake`.

- [ ] **Step 4: Run, then commit**

Run:
```bash
go test -race ./...
~/go/bin/golangci-lint run ./...
gofmt -l internal cmd
```
Expected: PASS, clean.

```bash
git add internal/app internal/zones internal/api
git commit -m "feat: wire NOTIFY in both directions

The inbound gate shares the Refresher the scheduler drives, so a
notify-triggered transfer takes the same per-zone lock a scheduled one
does rather than racing it. The outbound pass joins the background
runners, and passes once at startup -- which on a healthy restart finds
nothing, since every row already records delivery at the current serial.

The cascade needs no cascade-specific code: the Transferrer's install
writes the primary's serial verbatim and wakes the pass, which compares
serials like anywhere else.

The loopback test now proves the whole path -- a record changes on a
dnsaur primary and reaches a dnsaur secondary, signed, with the
secondary's refresh interval set beyond the test's lifetime so only the
NOTIFY can have caused it."
```

---

### Task 11: The notifies read route

**Files:**
- Create: `internal/api/notifies_handlers.go`
- Modify: `internal/api/routes.go` (or wherever `/zones/{id}/records` is registered)
- Modify: `internal/api/openapi.yaml`
- Test: `internal/api/notifies_test.go`

**Interfaces:**
- Consumes: `store.NotifyStore` (Task 3); `zones.SerialNewer` (Task 1); `zones.MaxNotifyAttempts` (Task 8).
- Produces:
  ```
  GET /api/v1/zones/{id}/notifies
  → [{ target, state, notified_serial, notified_at, attempts, max_attempts, last_error, created_at }]
  ```

**`state` is derived server-side** — `never` | `current` | `retrying` | `gave_up` — rather than left to the dashboard to infer from the columns. The rule D3 applied to `last_xfr_*`: a status two clients could compute differently is not a status.

**`max_attempts` ships beside `attempts`** because the screen renders "try 3/5" and the budget is a server constant. Sending only the numerator would make the dashboard hardcode a number it does not own.

**The order of the state checks is the whole logic**, and the trap is that `never` is not simply `notified_at == 0`: a target that has never been delivered *and* has exhausted its attempts is `gave_up`, not `never`.

- [ ] **Step 1: Write the failing tests**

```go
// internal/api/notifies_test.go

func TestNotifyStateDerivation(t *testing.T) {
	tests := []struct {
		name        string
		zoneSerial  uint32
		row         store.ZoneNotify
		want        string
	}{
		{
			name: "never told and never tried",
			zoneSerial: 47,
			row: store.ZoneNotify{NotifiedAt: 0, Attempts: 0},
			want: "never",
		},
		{
			name: "acknowledged the current serial",
			zoneSerial: 47,
			row: store.ZoneNotify{NotifiedAt: 1000, NotifiedSerial: 47},
			want: "current",
		},
		{
			name: "behind, still trying",
			zoneSerial: 48,
			row: store.ZoneNotify{NotifiedAt: 1000, NotifiedSerial: 47, Attempts: 3},
			want: "retrying",
		},
		{
			name: "behind, budget exhausted",
			zoneSerial: 48,
			row: store.ZoneNotify{NotifiedAt: 1000, NotifiedSerial: 47, Attempts: zones.MaxNotifyAttempts},
			want: "gave_up",
		},
		{
			// THE TRAP: never delivered *and* exhausted is gave_up, not
			// never. Reading notified_at alone would show a target that has
			// failed five times as though nothing had been tried.
			name: "never delivered and exhausted",
			zoneSerial: 47,
			row: store.ZoneNotify{NotifiedAt: 0, Attempts: zones.MaxNotifyAttempts},
			want: "gave_up",
		},
		{
			name: "never delivered, partway through the budget",
			zoneSerial: 47,
			row: store.ZoneNotify{NotifiedAt: 0, Attempts: 2},
			want: "retrying",
		},
		{
			// A wrapped serial is behind, not current — the API's own use of
			// SerialNewer, and the reason it is one shared helper.
			name: "the zone wrapped past the target",
			zoneSerial: 0,
			row: store.ZoneNotify{NotifiedAt: 1000, NotifiedSerial: 4294967295},
			want: "retrying",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := notifyStateOf(tc.zoneSerial, tc.row); got != tc.want {
				t.Errorf("state = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestListZoneNotifies(t *testing.T) {
	ts := newTestServer(t)
	ctx := t.Context()

	rec := ts.do(t, "POST", "/api/v1/zones",
		`{"name":"example.com","type":"primary","notify_to":"10.0.0.2, 10.0.0.3"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %s", rec.Body)
	}
	zoneID := createdID(t, rec)
	if err := ts.store.Notifies().Reconcile(ctx, zoneID, []string{"10.0.0.2:53", "10.0.0.3:53"}, 1000); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	got := ts.do(t, "GET", "/api/v1/zones/"+strconv.FormatInt(zoneID, 10)+"/notifies", "")
	if got.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", got.Code, got.Body)
	}
	var rows []map[string]any
	if err := json.Unmarshal(got.Body.Bytes(), &rows); err != nil {
		t.Fatalf("unmarshal %s: %v", got.Body, err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	for _, r := range rows {
		if r["state"] != "never" {
			t.Errorf("target %v: state = %v, want never", r["target"], r["state"])
		}
		// The screen renders "try 3/5" and must not hardcode the 5.
		if r["max_attempts"] != float64(zones.MaxNotifyAttempts) {
			t.Errorf("max_attempts = %v, want %d", r["max_attempts"], zones.MaxNotifyAttempts)
		}
		// created_at is what dates a never-notified target.
		if r["created_at"] != float64(1000) {
			t.Errorf("created_at = %v, want 1000", r["created_at"])
		}
	}
}

func TestListZoneNotifiesUnknownZoneIs404(t *testing.T) {
	ts := newTestServer(t)
	got := ts.do(t, "GET", "/api/v1/zones/9999/notifies", "")
	if got.Code != http.StatusNotFound {
		t.Fatalf("status %d, body %s", got.Code, got.Body)
	}
}

// A zone with no targets returns an empty array, not null — the dashboard
// maps over it, and null would be a runtime error on the common case.
func TestListZoneNotifiesEmptyIsAnArray(t *testing.T) {
	ts := newTestServer(t)
	rec := ts.do(t, "POST", "/api/v1/zones", `{"name":"example.com","type":"primary"}`)
	zoneID := createdID(t, rec)

	got := ts.do(t, "GET", "/api/v1/zones/"+strconv.FormatInt(zoneID, 10)+"/notifies", "")
	if body := strings.TrimSpace(got.Body.String()); body != "[]" {
		t.Errorf("body = %s, want []", body)
	}
}
```

- [ ] **Step 2: Run and watch them fail**

Run: `go test ./internal/api/ -run 'Notif' -v`
Expected: FAIL — `undefined: notifyStateOf`, and the route 404s.

- [ ] **Step 3: Implement**

```go
// notifyStateOf reduces a target's row to the one word the dashboard shows.
//
// Derived here rather than in the client for the reason D3's transfer status
// is: a status two clients could compute differently is not a status.
//
// **The guard is the logic, and it is not the ordering.** `never` is not simply
// notified_at == 0 — a target that has never been delivered *and* has
// exhausted its attempts is gave_up, and reading notified_at alone would show
// a target that has failed five times as though nothing had been tried.
//
// Note for anyone verifying this by mutation: **reordering the checks proves
// nothing.** `never` (notified_at == 0 && attempts == 0) and `gave_up`
// (behind && attempts >= Max) are disjoint, so either order yields the same
// answer for every input. The mutation that actually discriminates is
// dropping the `attempts == 0` conjunct, which makes the never-delivered
// exhausted row report `never`. (The plan originally prescribed the
// reordering probe; Task 11's implementer ran it, found it a no-op, and
// substituted the real one.)
func notifyStateOf(zoneSerial uint32, n store.ZoneNotify) string {
	behind := n.NotifiedAt == 0 || zones.SerialNewer(zoneSerial, n.NotifiedSerial)
	if !behind {
		return "current"
	}
	if n.Attempts >= zones.MaxNotifyAttempts {
		return "gave_up"
	}
	if n.NotifiedAt == 0 && n.Attempts == 0 {
		return "never"
	}
	return "retrying"
}
```

The handler reads the zone (404 if absent), reads its rows, maps each through `notifyStateOf`, and marshals a slice initialised as `[]notifyRow{}` rather than a nil slice, so the empty case is `[]`.

`openapi.yaml` gains the route and a `ZoneNotify` schema with all eight fields and the four-value `state` enum.

- [ ] **Step 4: Run, then commit**

Run:
```bash
go test ./internal/api/ -run 'Notif|OpenAPI' -v
go test -race ./internal/api/
```
Expected: PASS. The OpenAPI route check fails if the route is registered and undocumented, or documented and unregistered.

```bash
gofmt -l internal cmd
~/go/bin/golangci-lint run ./internal/api/
git add internal/api
git commit -m "feat(api): GET /zones/{id}/notifies

Per-target delivery state, with state derived server-side rather than left
to the client to infer from four columns -- the rule D3 applied to
last_xfr_*.

never is not simply notified_at == 0: a target never delivered and out of
attempts is gave_up, and reading notified_at alone would show a target
that failed five times as though nothing had been tried.

max_attempts ships beside attempts because the screen renders 'try 3/5'
and the budget is a server constant the dashboard should not hardcode."
```

---

### Task 12: Web — the NOTIFY OUT row

**Files:**
- Create: `web/src/lib/notify.ts` — the client-side `notify_to` grammar and the roll-up arithmetic
- Create: `web/src/lib/notify.test.ts`
- Modify: `web/src/hooks/use-zones.ts` — a `useZoneNotifies(zoneID)` query
- Modify: `web/src/pages/zones/detail.tsx` — the row
- Modify: `web/src/pages/tsig-keys.tsx` — the usage count
- Modify: `web/tests/` — the e2e smoke

**Design — read the artboard, do not improvise from this summary.** It is on disk at `.superpowers/sdd/2026-09-02-zones-milestone-d4/zone-detail-artboard.dc.html`. Do **not** call `DesignSync`; it is main-loop only and unreachable from a subagent. The artboard is the source of truth for layout, colour, copy and states; the lines below are orientation only.

- A **NOTIFY OUT** row below TRANSFERS OUT, sharing `AllowTransferBand`'s 152px label gutter so the captions land on one edge. Read by default, edit behind a pencil, exactly as `allow_transfer` works.
- The read line is the `notify_to` value (mono, ellipsised, `title` for the full string), a clickable caret, and a **roll-up**: `no targets` / `all 4 current` / `2 of 4 behind`, tinted `--warning-foreground` when anything is behind.
- **Only targets that are behind get a row by default.** The caret expands the rest. Four identical `current` rows say nothing four times; the roll-up is the glance.
- Per-target grid: target, state, serial, when, error. **The state column is the only column carrying colour**, so it is the scan path.
- Labels: `current`, `never notified`, `retrying · try 3/5`, **`not acknowledged`** — never "gave up". `gave_up` is drawn **amber-on-hollow**, distinguishable from `retrying`'s filled amber without claiming to be a failure. It resolves itself on the next edit and must not read as something to act on.
- A `never` target shows `—` for serial and `added 2m ago` from `created_at`.
- Mount on the **positive** zone-type set (primary or secondary), as `AllowTransferBand`'s call site does — not the artboard's `!builtIn`. A stub or forwarder reaching this control is a control that fails on click rather than one the screen declined to draw.

**Artboard sample data is not spec.** Placeholder strings are written for looks and have contradicted shipped behaviour before. Render server text verbatim — the `state`, the `last_error`, the serial — rather than copying sample strings.

- [ ] **Step 1: Write the failing tests**

```ts
// web/src/lib/notify.test.ts
import { describe, expect, it } from "vitest";
import { notifyRollup, parseNotifyTo, notifyKeyNames } from "./notify";

describe("parseNotifyTo", () => {
  it("accepts what the Go parser accepts", () => {
    expect(parseNotifyTo("10.0.0.2")).toEqual({
      ok: true,
      targets: [{ host: "10.0.0.2", port: 53, key: "" }],
    });
    expect(parseNotifyTo("ns2.example.com:5353 key:Hetzner-Xfer")).toEqual({
      ok: true,
      targets: [{ host: "ns2.example.com", port: 5353, key: "hetzner-xfer." }],
    });
    expect(parseNotifyTo("")).toEqual({ ok: true, targets: [] });
  });

  it("rejects what the Go parser rejects", () => {
    for (const bad of ["10.0.0.2:0", "10.0.0.2:70000", "not a host", "10.0.0.2 key:", "key:only"]) {
      expect(parseNotifyTo(bad).ok, bad).toBe(false);
    }
  });
});

describe("notifyKeyNames", () => {
  it("returns canonical names in order, and none from an unparseable value", () => {
    expect(notifyKeyNames("10.0.0.2 key:B, 10.0.0.3, ns4.example.com key:a")).toEqual(["b.", "a."]);
    expect(notifyKeyNames("10.0.0.2 key:bad name")).toEqual([]);
  });
});

// The one piece of screen logic that can be *wrong* rather than merely ugly.
describe("notifyRollup", () => {
  const current = { state: "current" } as const;
  const behind = { state: "gave_up" } as const;
  const retrying = { state: "retrying" } as const;
  const never = { state: "never" } as const;

  it("says no targets when there are none", () => {
    expect(notifyRollup([])).toEqual({ label: "no targets", behind: 0, total: 0 });
  });

  it("says all N current when nothing is behind", () => {
    expect(notifyRollup([current, current, current, current])).toEqual({
      label: "all 4 current",
      behind: 0,
      total: 4,
    });
  });

  it("counts retrying and gave_up as behind, and never as not", () => {
    expect(notifyRollup([current, behind, retrying, never])).toEqual({
      label: "2 of 4 behind",
      behind: 2,
      total: 4,
    });
  });

  // Decided 2026-09-02 by the design's author, overriding the artboard.
  //
  // The artboard's own arithmetic emits "all N current" whenever nothing is
  // behind — so a zone that has just gained a target reads "all 4 current"
  // while one of them has never been told anything. That is a false
  // statement on a row whose whole job is being scannable, and this
  // project's copy rule is to state the fact and stop.
  //
  // `behind` still takes precedence when both are present: it is the
  // actionable signal, and naming both would make the line a paragraph.
  it("names never-notified targets rather than calling them current", () => {
    expect(notifyRollup([current, current, current, never])).toEqual({
      label: "3 of 4 current, 1 never notified",
      behind: 0,
      total: 4,
    });
  });

  it("still leads with behind when both are present", () => {
    expect(notifyRollup([current, never, behind]).label).toBe("1 of 3 behind");
  });

  it("says all N current only when every target really is", () => {
    expect(notifyRollup([current, current]).label).toBe("all 2 current");
  });

  it("handles a single target without pluralising wrongly", () => {
    expect(notifyRollup([current]).label).toBe("all 1 current");
    expect(notifyRollup([behind]).label).toBe("1 of 1 behind");
  });
});
```

> `never` counting as *not* behind is deliberate and worth stating: a target added a moment ago has not failed at anything, and rolling it into "behind" would make adding a secondary look like breaking something.

- [ ] **Step 2: Run and watch them fail**

Run: `cd web && pnpm test notify`
Expected: FAIL — the module does not exist.

- [ ] **Step 3: Implement**

`web/src/lib/notify.ts` carries the same top-comment `lib/acl.ts` does — **this is not the source of truth**; `internal/zones/notifyto.go` validates independently at write time, and a bug here can make the field too strict or too lax but can never change what the server does.

Then the row in `detail.tsx`, the `useZoneNotifies` query in `use-zones.ts`, and the TSIG usage count extended to union `aclKeyNames(zone.allow_transfer)` with `notifyKeyNames(zone.notify_to)`.

- [ ] **Step 4: Run, then commit**

Run:
```bash
cd web && pnpm lint && pnpm format:check && pnpm typecheck && pnpm test && pnpm build && pnpm test:e2e
```
Expected: PASS

```bash
git add web
git commit -m "feat(web): the NOTIFY OUT row on zone detail

A roll-up rather than N rows: four targets all current say nothing four
times, so only targets that are behind get a row by default and a caret
expands the rest. The glance is the summary line.

gave_up is labelled 'not acknowledged' and drawn amber-on-hollow, not red
-- distinguishable from retrying without claiming to be a failure it is
not. It resolves itself on the next edit.

A never-notified target is dated by created_at, since it has no notify
date to show.

lib/notify.ts is a client-side port of the Go grammar and says so: it can
make the field too strict or too lax, never change what the server does."
```

---

### Task 13: Docs

**Files:**
- Modify: `README.md`, `docs/api.md`, `docs/architecture.md`, `docs/ui-contract.md`

- [ ] **Step 1: Write them**

- `README.md`: the feature list gains NOTIFY in both directions — telling secondaries when a zone changes, and transferring when a primary says so.
- `docs/api.md`: `notify_to` on create and patch — the format, that empty means notify nobody, that a `key:` name must exist, and that the stored value is the canonical spelling rather than what was typed. The `GET /zones/{id}/notifies` route, its eight fields, and the four `state` values with what each means and which of them the operator need not act on.
- `docs/architecture.md`: the opcode intercept and why NOTIFY branches ahead of the pipeline — including the defect that existed before it, since an operator reading a changelog will want to know whether they were affected. The inbound sequence (reply, throttle, probe, compare, transfer) and the rcode table in short form. The outbound queue, and **why the trigger is serial detection rather than call sites** — this is the design decision most likely to be "simplified" later by someone who has not read the reasoning.
- `docs/ui-contract.md`: the NOTIFY OUT row, the roll-up, and the four states with their labels.

- [ ] **Step 2: Check the tree**

Run: `grep -rn "notify_to" docs/ README.md`
Expected: the format is described in exactly one place and referenced from the others — the rule the `primaries` and `allow_transfer` docs already follow.

- [ ] **Step 3: Commit**

```bash
git add README.md docs
git commit -m "docs: NOTIFY in both directions, and what each state means"
```

---

## Done when

- `go test -race ./...`, `~/go/bin/golangci-lint run ./...`, `gofmt -l internal cmd` clean on a committed tree.
- `cd web && pnpm lint && pnpm format:check && pnpm typecheck && pnpm test && pnpm build && pnpm test:e2e` clean.
- A NOTIFY no longer reaches the pipeline, proven by a test that fails on the commit before Task 5.
- A record changed on a dnsaur primary reaches a dnsaur secondary, signed, with the secondary's refresh interval set beyond the test's lifetime — so only the NOTIFY can have caused it.
- Every row of §9.10.2's table has a test asserting that rcode and its TSIG error code.
- A zone that has never transferred still transfers when notified, with identical serials on both sides.
