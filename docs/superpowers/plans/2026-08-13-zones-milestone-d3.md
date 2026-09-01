# Zones Milestone D3 — inbound AXFR Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** dnsaur as a primary — another nameserver asks it for a zone over AXFR, and gets one if the zone's `allow_transfer` says that peer may have it.

**Architecture:** A transfer cannot be a middleware (`dnssrv.Handler` returns exactly one `*Response`), so `Server.serve` branches on qtype AXFR/IXFR ahead of the pipeline and hands the raw `dns.ResponseWriter` to a `Transfers` implementation in `internal/zones`. The zone comes from the resolver's already-atomic `Index` snapshot, never the store. The envelope loop is written here rather than through `dns.Transfer.Out`, which cannot place the OPT RFC 5936 §2.2.5 asks for.

**Tech Stack:** Go 1.26, `miekg/dns` v1.1.72, sqlite + Postgres, goose migrations, React + TanStack Query.

**Spec:** §9.5 of `docs/superpowers/specs/2026-08-08-zones-design.md`. Read it first — it records *why* each rule below is the rule, and the RFC sentences it quotes were fetched rather than recalled.

## Global Constraints

- **The next free migration number is `0011`.** Do not derive this from the directory listing: versions 5 and 7 are Go migrations claimed in `internal/store/migrate.go` with no SQL file, and a duplicate version fails goose at startup on both drivers. Write both `sqlite/` and `postgres/` variants. Postgres gets `BIGINT` where sqlite gets `INTEGER` for unix-ms columns — see the note in `0010_zone_transfer_outcome.sql`.
- Store tests run under `forEachDriver`; sqlite and a real Postgres container both execute.
- **D3 adds no HTTP routes.** `allow_transfer` rides the existing `PATCH /zones/{id}`. `internal/api/openapi.yaml` still has to document the new fields in the same commit.
- Gates on the **committed** tree, `git status --porcelain` empty: `go test -race ./...`, `~/go/bin/golangci-lint run ./...`, `gofmt -l internal cmd`, and for web tasks `cd web && pnpm lint && pnpm format:check && pnpm typecheck && pnpm test && pnpm build && pnpm test:e2e`.
- Every fix's test shown failing with the fix removed.
- **Time is injected, never read directly** — except a TSIG timestamp, which is checked against the *peer's* clock inside a fudge window and must be real wall time. `Transferrer.now`'s doc comment (`internal/zones/transfer.go`) already states this distinction; D3 follows it.
- **Disjoint column sets.** `updateZoneSQL` binds every configuration column and must never bind `last_xfr_*`; `noteTransferServedSQL` binds only `last_xfr_*` and must never bind anything else. D2 shipped a defect from exactly this and has a store test pinning it — Task 2 mirrors that test rather than trusting the rule.

## File Structure

- `internal/zones/acl.go` — the `allow_transfer` format: parse, format, match. Mirrors `primaries.go`
- `internal/store/migrations/{sqlite,postgres}/0011_zone_transfer_out.sql` — `allow_transfer` + the three served-state columns
- `internal/store/zones.go` — the columns, `NoteTransferServed`
- `internal/store/tsigkeys.go` — the delete guard extended to `key:` references
- `internal/api/zones_handlers.go` — `allow_transfer` on create and patch
- `internal/dnssrv/transfers.go` — the `Transfers` interface and `WithTransfers`
- `internal/dnssrv/server.go` — the intercept branch
- `internal/zones/zone.go` — `Index.Apex`
- `internal/zones/transferserver.go` — the gate, the envelope loop, the recording
- `internal/app/app.go` — wiring
- `web/src/lib/acl.ts`, `web/src/pages/zones/detail.tsx`, `web/src/pages/tsig-keys.tsx` — the field, the served line, the usage count
- `docs/` — the format, the gate, the rcodes

---

### Task 1: The `allow_transfer` format

**Files:**
- Create: `internal/zones/acl.go`
- Test: `internal/zones/acl_test.go`

**Interfaces:**
- Produces:
  ```go
  type ACLEntry struct {
      Prefix netip.Prefix // zero value on a key entry
      Key    string       // canonical TSIG name; "" on a prefix entry
  }
  func ValidateACL(s string) error
  func ParseACL(s string) ([]ACLEntry, error)
  func FormatACL(es []ACLEntry) string
  func ACLKeys(s string) []string
  func ACLAllows(es []ACLEntry, peer netip.Addr, tsigKey string) bool
  ```

**Why this is not `primaries.go` with different words.** `ParsePrimaries` takes a context and a resolver because a primary may be named by hostname and must be resolved at transfer time. An ACL is matched against a socket address on every single request, so a hostname here would mean a DNS lookup inside the gate of the server answering DNS. `ParseACL` is pure, and that is a deliberate difference rather than an omission.

**Empty is legal here and is not legal there.** `ValidatePrimaries("")` is an error, because a secondary with nowhere to pull from is a configuration mistake. `ValidateACL("")` succeeds and means deny everything, which is the default a zone is created with.

- [ ] **Step 1: Write the failing tests**

```go
package zones_test

import (
	"net/netip"
	"testing"

	"github.com/aloks98/dnsaur/internal/zones"
)

func TestParseACL(t *testing.T) {
	for _, tc := range []struct {
		name    string
		in      string
		want    []string // FormatACL of the result
		wantErr bool
	}{
		{name: "empty is deny, not an error", in: "", want: nil},
		{name: "cidr", in: "10.0.0.0/24", want: []string{"10.0.0.0/24"}},
		{name: "bare v4 is a host route", in: "192.168.1.5", want: []string{"192.168.1.5"}},
		{name: "bare v6 is a host route", in: "2001:db8::5", want: []string{"2001:db8::5"}},
		{name: "v6 cidr", in: "2001:db8::/64", want: []string{"2001:db8::/64"}},
		{name: "key entry is canonicalised", in: "key:NS2", want: []string{"key:ns2."}},
		{name: "key prefix is case insensitive", in: "KEY:ns2", want: []string{"key:ns2."}},
		{name: "mixed list, whitespace tolerated", in: " 10.0.0.0/24 , key:ns2 , 192.168.1.5 ",
			want: []string{"10.0.0.0/24", "key:ns2.", "192.168.1.5"}},
		{name: "trailing comma is skipped", in: "10.0.0.0/24,", want: []string{"10.0.0.0/24"}},
		{name: "only separators is empty, not an error", in: " , , ", want: nil},
		{name: "bad mask", in: "10.0.0.0/33", wantErr: true},
		{name: "not an address", in: "not-an-ip", wantErr: true},
		{name: "hostname is refused, not resolved", in: "ns2.example.com", wantErr: true},
		{name: "empty key name", in: "key:", wantErr: true},
		{name: "key name with a space", in: "key:ns 2", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := zones.ParseACL(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseACL(%q) = %v, want error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseACL(%q): %v", tc.in, err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("ParseACL(%q) gave %d entries, want %d", tc.in, len(got), len(tc.want))
			}
			for i, w := range tc.want {
				if one := zones.FormatACL(got[i : i+1]); one != w {
					t.Errorf("entry %d = %q, want %q", i, one, w)
				}
			}
			// ValidateACL must agree with ParseACL on every input, or the
			// write-time check and the request-time parse disagree about what
			// is storable — which is a zone that saves and never matches.
			if err := zones.ValidateACL(tc.in); err != nil {
				t.Errorf("ValidateACL(%q) = %v, want nil", tc.in, err)
			}
		})
	}
}

func TestValidateACLRejectsWhatParseRejects(t *testing.T) {
	for _, in := range []string{"10.0.0.0/33", "not-an-ip", "key:"} {
		if err := zones.ValidateACL(in); err == nil {
			t.Errorf("ValidateACL(%q) = nil, want error", in)
		}
	}
}

func TestFormatACLRoundTrips(t *testing.T) {
	const in = "10.0.0.0/24, key:ns2., 192.168.1.5"
	es, err := zones.ParseACL(in)
	if err != nil {
		t.Fatalf("ParseACL: %v", err)
	}
	out := zones.FormatACL(es)
	if out != in {
		t.Fatalf("FormatACL = %q, want %q", out, in)
	}
	// And parsing the formatted form gives the same entries back, which is
	// what lets the API store the canonical spelling.
	again, err := zones.ParseACL(out)
	if err != nil {
		t.Fatalf("ParseACL(formatted): %v", err)
	}
	if zones.FormatACL(again) != out {
		t.Fatalf("second round trip = %q, want %q", zones.FormatACL(again), out)
	}
}

func TestACLKeys(t *testing.T) {
	got := zones.ACLKeys("10.0.0.0/24, key:NS2, key:other")
	want := []string{"ns2.", "other."}
	if len(got) != len(want) {
		t.Fatalf("ACLKeys gave %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("key %d = %q, want %q", i, got[i], want[i])
		}
	}
	// An unparseable value names no keys rather than panicking: callers use
	// this to validate and to count usage, and neither wants an error here.
	if k := zones.ACLKeys("nonsense"); len(k) != 0 {
		t.Errorf("ACLKeys(nonsense) = %v, want none", k)
	}
}

func TestACLAllows(t *testing.T) {
	es, err := zones.ParseACL("10.0.0.0/24, key:ns2")
	if err != nil {
		t.Fatalf("ParseACL: %v", err)
	}
	for _, tc := range []struct {
		name string
		peer string
		key  string
		want bool
	}{
		{name: "address in range, unsigned", peer: "10.0.0.5", want: true},
		{name: "address out of range, unsigned", peer: "10.0.1.5", want: false},
		{name: "wrong key, address out of range", peer: "10.0.1.5", key: "other.", want: false},
		{name: "right key, address out of range", peer: "10.0.1.5", key: "ns2.", want: true},
		{name: "key match is case insensitive", peer: "10.0.1.5", key: "NS2.", want: true},
		// The v4-mapped form is what a dual-stack listener hands back, and a
		// mapped address does not match a v4 prefix. Unmapping is done here so
		// no caller can forget it.
		{name: "v4-mapped peer still matches a v4 prefix", peer: "::ffff:10.0.0.5", want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			peer, err := netip.ParseAddr(tc.peer)
			if err != nil {
				t.Fatalf("ParseAddr: %v", err)
			}
			if got := zones.ACLAllows(es, peer, tc.key); got != tc.want {
				t.Errorf("ACLAllows(%s, %q) = %v, want %v", tc.peer, tc.key, got, tc.want)
			}
		})
	}
}

func TestACLAllowsNothingWhenEmpty(t *testing.T) {
	peer := netip.MustParseAddr("10.0.0.5")
	if zones.ACLAllows(nil, peer, "ns2.") {
		t.Fatal("an empty ACL allowed a transfer; default deny is the whole design")
	}
}
```

- [ ] **Step 2: Run and watch them fail**

Run: `go test ./internal/zones/ -run 'ACL' -v`
Expected: FAIL — `undefined: zones.ParseACL`.

- [ ] **Step 3: Implement**

```go
package zones

import (
	"fmt"
	"net/netip"
	"strings"

	"github.com/miekg/dns"
)

// The `allow_transfer` format: who may pull a zone off this server.
//
// A comma-separated list, whitespace tolerated, where each entry is one of:
//
//	10.0.0.0/24        a prefix
//	192.168.1.5        a single address (/32, or /128 for v6)
//	key:secondary-ns2  a request signed under that TSIG key
//
// Empty means deny every transfer, which is what a zone is created with.
// Entries are OR'd: any one match allows it.
//
// Unlike primaries.go, parsing is pure — no context, no resolver, no
// hostnames. An ACL is matched against a socket address on every request, so
// a hostname here would put a DNS lookup inside the gate of the server that
// answers DNS. A peer whose address moves is named by a key instead, which is
// the stronger check anyway.
//
// There are no negation entries. A default-deny list has nothing to subtract
// from, and `!` syntax would introduce an ordering question the OR does not
// have.

const aclKeyPrefix = "key:"

// ACLEntry is one parsed entry: an address prefix or a TSIG key name, never
// both. A key entry's Prefix is the zero Prefix; a prefix entry's Key is "".
type ACLEntry struct {
	Prefix netip.Prefix
	Key    string
}

// ValidateACL reports whether s is a well-formed allow_transfer list. Use it
// at write time. An empty list is valid and means deny.
func ValidateACL(s string) error {
	_, err := ParseACL(s)
	return err
}

// ParseACL parses s into the entries a request is matched against.
func ParseACL(s string) ([]ACLEntry, error) {
	var out []ACLEntry
	for _, field := range strings.Split(s, ",") {
		// Skipped rather than rejected, exactly as splitPrimaries does: a
		// trailing comma names no peer, so there is nothing to be wrong about.
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		e, err := parseACLEntry(field)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, nil
}

func parseACLEntry(field string) (ACLEntry, error) {
	if len(field) >= len(aclKeyPrefix) && strings.EqualFold(field[:len(aclKeyPrefix)], aclKeyPrefix) {
		name := strings.TrimSpace(field[len(aclKeyPrefix):])
		if !validACLKeyName(name) {
			return ACLEntry{}, fmt.Errorf("allow_transfer %q: key name must be a domain name", field)
		}
		// Canonical (lowercase, trailing dot) because that is how tsig_keys
		// stores names (normalizeTSIGName, internal/api) and how a verified
		// key name arrives from dnssrv.RequireTSIG. Three spellings of one
		// name is three chances for a match to silently fail.
		return ACLEntry{Key: dns.CanonicalName(name)}, nil
	}
	if p, err := netip.ParsePrefix(field); err == nil {
		// Masked so 10.0.0.5/24 is stored as the range it actually matches,
		// rather than keeping host bits that Prefix.Contains ignores and a
		// reader does not.
		return ACLEntry{Prefix: p.Masked()}, nil
	}
	if addr, err := netip.ParseAddr(field); err == nil {
		addr = addr.Unmap()
		return ACLEntry{Prefix: netip.PrefixFrom(addr, addr.BitLen())}, nil
	}
	return ACLEntry{}, fmt.Errorf("allow_transfer %q: expected an IP address, a CIDR prefix, or key:<name>", field)
}

// validACLKeyName applies the same guards normalizeTSIGName does, for the
// same reason: dns.IsDomainName documents itself as "extremely liberal —
// almost any string is a valid domain name", so alone it accepts "ns 2".
func validACLKeyName(name string) bool {
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

// FormatACL writes entries back in the spelling ParseACL reads. The API
// stores this canonical form rather than what was typed, which is what lets
// the tsig_keys delete guard match a key name in SQL exactly (see
// tsigKeyStore.Delete) instead of pattern-matching around whitespace.
func FormatACL(es []ACLEntry) string {
	parts := make([]string, 0, len(es))
	for _, e := range es {
		switch {
		case e.Key != "":
			parts = append(parts, aclKeyPrefix+e.Key)
		case e.Prefix.Bits() == e.Prefix.Addr().BitLen():
			// A single address prints without its all-ones mask: 192.168.1.5,
			// not 192.168.1.5/32. It parses back to the same entry, and it is
			// what the operator typed.
			parts = append(parts, e.Prefix.Addr().String())
		default:
			parts = append(parts, e.Prefix.String())
		}
	}
	return strings.Join(parts, ", ")
}

// ACLKeys returns the canonical TSIG key names s names, in order. An
// unparseable value names none: callers use this to validate a write and to
// count a key's usage, and neither has anything to do about an error here
// that its own caller is not already doing.
func ACLKeys(s string) []string {
	es, err := ParseACL(s)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range es {
		if e.Key != "" {
			out = append(out, e.Key)
		}
	}
	return out
}

// ACLAllows reports whether a peer at addr, having verified under tsigKey
// ("" when the request was unsigned or did not verify), matches any entry.
//
// addr is unmapped here rather than at the call site. A dual-stack listener
// reports a v4 peer as ::ffff:10.0.0.5, which does not match 10.0.0.0/24, and
// the failure mode is an ACL that looks correct and denies everything.
func ACLAllows(es []ACLEntry, addr netip.Addr, tsigKey string) bool {
	addr = addr.Unmap()
	key := dns.CanonicalName(tsigKey)
	for _, e := range es {
		if e.Key != "" {
			if tsigKey != "" && e.Key == key {
				return true
			}
			continue
		}
		if e.Prefix.Contains(addr) {
			return true
		}
	}
	return false
}
```

- [ ] **Step 4: Run, then commit**

Run: `go test ./internal/zones/ -run 'ACL' -v && gofmt -l internal && ~/go/bin/golangci-lint run ./internal/zones/`
Expected: PASS, no output from gofmt.

```bash
git add internal/zones/acl.go internal/zones/acl_test.go
git commit -m "feat(zones): the allow_transfer format"
```

---

### Task 2: The column, the served state, and the extended key guard

**Files:**
- Create: `internal/store/migrations/sqlite/0011_zone_transfer_out.sql`, `internal/store/migrations/postgres/0011_zone_transfer_out.sql`
- Modify: `internal/store/zones.go`, `internal/store/tsigkeys.go`
- Test: `internal/store/zones_test.go`, `internal/store/tsigkeys_test.go`

**Interfaces:**
- Consumes: `zones.ACLKeys` (Task 1) — used only by the test that pins the SQL guard against the Go parser.
- Produces:
  - `store.Zone.AllowTransfer string`, `.LastXfrAt int64`, `.LastXfrPeer string`, `.LastXfrError string`
  - `ZoneStore.NoteTransferServed(ctx context.Context, zoneID, at int64, peer, errText string) error`

**The guard, and why it is SQL rather than a read-then-delete.** `tsigKeyStore.Delete` already refuses to remove a key some zone's `tsig_key_id` names, in one statement, so there is no window between the check and the delete. A `key:` entry in `allow_transfer` is the same reference by a different spelling and gets the same treatment. Matching a name inside a text column needs a substring test with no wildcard semantics — `instr` on sqlite, `strpos` on postgres, same argument order — because a key name may legally contain `_`, which `LIKE` would treat as a wildcard and over-match. The dialect branch is precedented by `sqlStore.insert`.

The delimiters are what make it exact: the column is stored canonical (`FormatACL`, `", "`-separated), so wrapping both the haystack and the needle in commas means `key:ns2.` cannot match an entry for `key:xns2.`.

- [ ] **Step 1: Write the failing tests**

In `internal/store/zones_test.go`:

```go
func TestZoneAllowTransferRoundTrips(t *testing.T) {
	forEachDriver(t, func(t *testing.T, st store.Store) {
		ctx := context.Background()
		zs := st.Zones()
		id, err := zs.AddZone(ctx, store.Zone{
			Name: "e412.in", Type: "primary", Enabled: true,
			AllowTransfer: "10.0.0.0/24, key:ns2.",
		})
		if err != nil {
			t.Fatalf("AddZone: %v", err)
		}
		got, err := zs.Zone(ctx, id)
		if err != nil {
			t.Fatalf("Zone: %v", err)
		}
		if got.AllowTransfer != "10.0.0.0/24, key:ns2." {
			t.Fatalf("allow_transfer = %q after insert", got.AllowTransfer)
		}
		got.AllowTransfer = "192.168.1.5"
		if err := zs.UpdateZone(ctx, got); err != nil {
			t.Fatalf("UpdateZone: %v", err)
		}
		got, err = zs.Zone(ctx, id)
		if err != nil {
			t.Fatalf("Zone: %v", err)
		}
		if got.AllowTransfer != "192.168.1.5" {
			t.Fatalf("allow_transfer = %q after update", got.AllowTransfer)
		}
	})
}

func TestNoteTransferServedWritesOnlyItsOwnColumns(t *testing.T) {
	forEachDriver(t, func(t *testing.T, st store.Store) {
		ctx := context.Background()
		zs := st.Zones()
		id, err := zs.AddZone(ctx, store.Zone{Name: "e412.in", Type: "primary", Enabled: true})
		if err != nil {
			t.Fatalf("AddZone: %v", err)
		}
		if err := zs.NoteTransferServed(ctx, id, 1700000000000, "10.0.0.5", ""); err != nil {
			t.Fatalf("NoteTransferServed: %v", err)
		}
		got, err := zs.Zone(ctx, id)
		if err != nil {
			t.Fatalf("Zone: %v", err)
		}
		if got.LastXfrAt != 1700000000000 || got.LastXfrPeer != "10.0.0.5" || got.LastXfrError != "" {
			t.Fatalf("served state = (%d, %q, %q)", got.LastXfrAt, got.LastXfrPeer, got.LastXfrError)
		}
		if got.Name != "e412.in" || !got.Enabled {
			t.Fatalf("NoteTransferServed changed the zone's configuration: %+v", got)
		}
	})
}

// The mirror of TestNoteTransferAttemptSurvivesAZoneWrite on the outbound
// side: a whole-row UpdateZone binds every configuration column from a struct
// the caller read earlier, so if last_xfr_* were in that statement, an
// operator's edit would erase what a transfer recorded a moment before.
func TestOrdinaryZoneWritesCannotEraseServedState(t *testing.T) {
	forEachDriver(t, func(t *testing.T, st store.Store) {
		ctx := context.Background()
		zs := st.Zones()
		id, err := zs.AddZone(ctx, store.Zone{Name: "e412.in", Type: "primary", Enabled: true})
		if err != nil {
			t.Fatalf("AddZone: %v", err)
		}
		stale, err := zs.Zone(ctx, id) // read before the transfer
		if err != nil {
			t.Fatalf("Zone: %v", err)
		}
		if err := zs.NoteTransferServed(ctx, id, 1700000000000, "10.0.0.5", "refused"); err != nil {
			t.Fatalf("NoteTransferServed: %v", err)
		}
		stale.SOARefresh = 7200 // an edit made from the stale read
		if err := zs.UpdateZone(ctx, stale); err != nil {
			t.Fatalf("UpdateZone: %v", err)
		}
		got, err := zs.Zone(ctx, id)
		if err != nil {
			t.Fatalf("Zone: %v", err)
		}
		if got.SOARefresh != 7200 {
			t.Fatalf("the edit did not land: soa_refresh = %d", got.SOARefresh)
		}
		if got.LastXfrAt != 1700000000000 || got.LastXfrError != "refused" {
			t.Fatalf("the zone write erased the served state: (%d, %q)", got.LastXfrAt, got.LastXfrError)
		}
	})
}
```

In `internal/store/tsigkeys_test.go`:

```go
func TestDeleteRefusesAKeyNamedByAllowTransfer(t *testing.T) {
	forEachDriver(t, func(t *testing.T, st store.Store) {
		ctx := context.Background()
		keyID, err := st.TSIGKeys().Create(ctx, store.TSIGKey{
			Name: "ns2.", Algorithm: "hmac-sha256.", Secret: "c2VjcmV0",
		})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if _, err := st.Zones().AddZone(ctx, store.Zone{
			Name: "e412.in", Type: "primary", Enabled: true,
			AllowTransfer: "10.0.0.0/24, key:ns2.",
		}); err != nil {
			t.Fatalf("AddZone: %v", err)
		}
		if err := st.TSIGKeys().Delete(ctx, keyID); !errors.Is(err, store.ErrInUse) {
			t.Fatalf("Delete = %v, want ErrInUse: a key an ACL names must not vanish under it", err)
		}
	})
}

// The delimiters in the guard are what make it exact. Without them,
// "key:xns2." contains "key:ns2." and every unrelated key would be
// undeletable.
func TestDeleteAllowsAKeyOnlyResembledByAnACLEntry(t *testing.T) {
	forEachDriver(t, func(t *testing.T, st store.Store) {
		ctx := context.Background()
		keyID, err := st.TSIGKeys().Create(ctx, store.TSIGKey{
			Name: "ns2.", Algorithm: "hmac-sha256.", Secret: "c2VjcmV0",
		})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if _, err := st.Zones().AddZone(ctx, store.Zone{
			Name: "e412.in", Type: "primary", Enabled: true,
			AllowTransfer: "key:xns2., key:ns2extra.",
		}); err != nil {
			t.Fatalf("AddZone: %v", err)
		}
		if err := st.TSIGKeys().Delete(ctx, keyID); err != nil {
			t.Fatalf("Delete = %v, want nil: no ACL entry names this key", err)
		}
	})
}

// The SQL guard and the Go parser must agree about which keys a value names.
// Two implementations of one rule is how they drift.
func TestDeleteGuardAgreesWithACLKeys(t *testing.T) {
	forEachDriver(t, func(t *testing.T, st store.Store) {
		ctx := context.Background()
		const acl = "10.0.0.0/24, key:ns2., key:ns3."
		if names := zones.ACLKeys(acl); len(names) != 2 || names[0] != "ns2." || names[1] != "ns3." {
			t.Fatalf("ACLKeys(%q) = %v", acl, names)
		}
		if _, err := st.Zones().AddZone(ctx, store.Zone{
			Name: "e412.in", Type: "primary", Enabled: true, AllowTransfer: acl,
		}); err != nil {
			t.Fatalf("AddZone: %v", err)
		}
		for _, name := range []string{"ns2.", "ns3."} {
			id, err := st.TSIGKeys().Create(ctx, store.TSIGKey{
				Name: name, Algorithm: "hmac-sha256.", Secret: "c2VjcmV0",
			})
			if err != nil {
				t.Fatalf("Create %s: %v", name, err)
			}
			if err := st.TSIGKeys().Delete(ctx, id); !errors.Is(err, store.ErrInUse) {
				t.Errorf("Delete(%s) = %v, want ErrInUse", name, err)
			}
		}
	})
}
```

- [ ] **Step 2: Run and watch them fail**

Run: `go test ./internal/store/ -run 'AllowTransfer|TransferServed|ServedState|DeleteRefusesAKeyNamed|DeleteAllows|DeleteGuardAgrees' -v`
Expected: FAIL — `unknown field AllowTransfer in struct literal`.

- [ ] **Step 3: Implement**

`internal/store/migrations/sqlite/0011_zone_transfer_out.sql`:

```sql
-- +goose Up
-- Serving a zone to a secondary: who may ask, and how the last one that asked
-- went.
--
-- allow_transfer is the ACL, default deny. Empty means no peer may transfer
-- this zone, which is what every zone that exists before this migration gets:
-- turning on a transfer path and opening every zone through it in the same
-- release would be a silent change of what the server discloses.
--
-- Its format is a comma-separated list of address, CIDR, or key:<tsig name>,
-- parsed by zones.ParseACL and stored in that package's canonical spelling
-- rather than as typed. The canonical form is load-bearing: tsigKeyStore.Delete
-- matches a key name inside this column in SQL, and it can only do that
-- exactly because the separator and the name spelling are known.
--
-- The other three are the outbound twin of last_error/last_attempt from 0010,
-- and they are separate columns rather than a reuse of those two because the
-- questions are different: 0010's pair is "did our pull from our primary
-- work", these are "did a peer's pull from us work". A secondary that both
-- pulls and serves has an answer to each, and one pair could not hold both.
--
-- last_xfr_error is '' when the last attempt was served and the refusal
-- reason when it was not -- cleared on success, so it cannot become a
-- tombstone of a problem fixed weeks ago. last_xfr_peer is the address that
-- asked, which is the thing an operator is actually trying to identify when a
-- secondary is not updating. last_xfr_at is unix ms; 0 = never asked.
--
-- Who writes them: only zones.TransferServer, through
-- ZoneStore.NoteTransferServed, a three-column UPDATE that touches nothing
-- else -- not updateZoneSQL and not ReplaceRecords, both of which bind every
-- configuration column and would revert a concurrent edit. The same rule, for
-- the same reason, as 0010.
ALTER TABLE zones ADD COLUMN allow_transfer TEXT NOT NULL DEFAULT '';
ALTER TABLE zones ADD COLUMN last_xfr_at INTEGER NOT NULL DEFAULT 0;
ALTER TABLE zones ADD COLUMN last_xfr_peer TEXT NOT NULL DEFAULT '';
ALTER TABLE zones ADD COLUMN last_xfr_error TEXT NOT NULL DEFAULT '';
```

`internal/store/migrations/postgres/0011_zone_transfer_out.sql`: the same file, with the same comment, except:

```sql
-- BIGINT here, INTEGER on sqlite -- see 0010 for why: postgres INTEGER is
-- 32-bit and a unix millisecond timestamp passed that in 1970 + 24.8 days.
ALTER TABLE zones ADD COLUMN allow_transfer TEXT NOT NULL DEFAULT '';
ALTER TABLE zones ADD COLUMN last_xfr_at BIGINT NOT NULL DEFAULT 0;
ALTER TABLE zones ADD COLUMN last_xfr_peer TEXT NOT NULL DEFAULT '';
ALTER TABLE zones ADD COLUMN last_xfr_error TEXT NOT NULL DEFAULT '';
```

In `internal/store/zones.go`, add to `Zone` after `LastAttempt`:

```go
	// AllowTransfer is who may pull this zone: a comma-separated list of
	// address, CIDR, or key:<tsig name>, in zones.FormatACL's canonical
	// spelling. Empty means deny, and that is the default.
	AllowTransfer string `json:"allow_transfer"`
	// The outbound twin of LastAttempt/LastError: when a peer last asked for
	// this zone, which peer, and why it was refused if it was. Written only by
	// NoteTransferServed.
	LastXfrAt    int64  `json:"last_xfr_at"`
	LastXfrPeer  string `json:"last_xfr_peer"`
	LastXfrError string `json:"last_xfr_error"`
```

Then, in the same file:

```go
const zoneColumns = `id, name, type, enabled, soa_ns, soa_mbox, soa_serial, soa_refresh, soa_retry, soa_expire, soa_minimum, soa_ttl, primaries, tsig_key_id, expires_at, refreshed_at, last_error, last_attempt, allow_transfer, last_xfr_at, last_xfr_peer, last_xfr_error, created_at, modified_at`

func scanZone(row interface{ Scan(...any) error }, z *Zone) error {
	return row.Scan(&z.ID, &z.Name, &z.Type, &z.Enabled, &z.SOANS, &z.SOAMbox, &z.SOASerial, &z.SOARefresh, &z.SOARetry, &z.SOAExpire, &z.SOAMinimum, &z.SOATTL, &z.Primaries, &z.TSIGKeyID, &z.ExpiresAt, &z.RefreshedAt, &z.LastError, &z.LastAttempt, &z.AllowTransfer, &z.LastXfrAt, &z.LastXfrPeer, &z.LastXfrError, &z.CreatedAt, &z.ModifiedAt)
}
```

`AddZone` gains `allow_transfer` in its column list, one more `?`, and `zn.AllowTransfer` in its args — the three state columns are not inserted, they default. `updateZoneSQL` gains `allow_transfer = ?` (before `modified_at = ?`) and `updateZoneArgs` gains `zn.AllowTransfer` in the same position. **The three `last_xfr_*` columns go in neither.**

The new write, beside `noteTransferAttemptSQL`:

```go
// noteTransferServedSQL names the only three columns an outbound transfer
// outcome may write. Disjoint from updateZoneSQL and from
// noteTransferAttemptSQL — see ZoneStore.NoteTransferServed.
const noteTransferServedSQL = `UPDATE zones SET last_xfr_at = ?, last_xfr_peer = ?, last_xfr_error = ? WHERE id = ?`

func (z *zoneStore) NoteTransferServed(ctx context.Context, zoneID, at int64, peer, errText string) error {
	return z.s.execOne(ctx, noteTransferServedSQL, at, peer, errText, zoneID)
}
```

And on the interface, with a doc comment stating: it records one *outbound* transfer attempt, `errText` is "" when the zone was served and the refusal reason when it was not; it is a three-column UPDATE for the same reason `NoteTransferAttempt` is a two-column one, and its column set is disjoint from both other writers.

In `internal/store/tsigkeys.go`, `Delete` becomes:

```go
// aclKeyRef reports the SQL that finds a zone whose allow_transfer names
// tsig_keys.name. Both halves are wrapped in commas so a name cannot match a
// longer one that contains it, and the substring function is dialect-specific
// because LIKE would treat an underscore in a key name -- legal in a domain
// name -- as a wildcard, silently refusing to delete unrelated keys.
func aclKeyRef(dialect string) string {
	fn := "instr"
	if dialect == "postgres" {
		fn = "strpos"
	}
	return `SELECT 1 FROM zones WHERE ` + fn +
		`(',' || replace(zones.allow_transfer, ' ', '') || ',', ',key:' || tsig_keys.name || ',') > 0`
}

func (t *tsigKeyStore) Delete(ctx context.Context, id int64) error {
	res, err := t.s.db.ExecContext(ctx, t.s.q(
		`DELETE FROM tsig_keys WHERE id = ?
		   AND NOT EXISTS (SELECT 1 FROM zones WHERE zones.tsig_key_id = tsig_keys.id)
		   AND NOT EXISTS (`+aclKeyRef(t.s.dialect)+`)`), id)
	// ... the rest is unchanged: RowsAffected, then Get to tell 404 from 409.
}
```

- [ ] **Step 4: Run, then commit**

Run: `go test -race ./internal/store/ -v 2>&1 | tail -30`
Expected: PASS on both drivers.

Then delete the second `NOT EXISTS` clause, re-run `TestDeleteRefusesAKeyNamedByAllowTransfer`, and confirm it fails. Restore it.

```bash
git add internal/store
git commit -m "feat(store): allow_transfer, the served-transfer state, and a key an ACL names"
```

---

### Task 3: `allow_transfer` through the API

**Files:**
- Modify: `internal/api/zones_handlers.go`, `internal/api/openapi.yaml`
- Test: `internal/api/zones_handlers_test.go`

**Interfaces:**
- Consumes: `zones.ValidateACL`, `zones.ParseACL`, `zones.FormatACL`, `zones.ACLKeys` (Task 1); `store.Zone.AllowTransfer` (Task 2).
- Produces: `allow_transfer` accepted on `POST /zones` and `PATCH /zones/{id}`, stored canonical.

**Where the rule goes.** `checkZoneTransferConfig` already validates the (type, primaries, tsig_key_id) triple for create and patch alike, "a rule enforced on POST and not on PATCH is a rule with a way around it". `allow_transfer` joins it rather than getting a second validator. Its signature grows one parameter.

**Which types may carry one.** Serving a transfer is something a `primary` and a `secondary` both do (§9.5.3), so unlike `primaries` this is not secondary-only. The API only ever creates those two types, so there is no reachable branch for the others and none is written.

- [ ] **Step 1: Write the failing tests**

```go
func TestZoneCreateStoresAllowTransferCanonically(t *testing.T) {
	srv, st := newTestServer(t)
	body := `{"name":"e412.in","allow_transfer":" 10.0.0.5 , KEY:NS2 "}`
	rec := doJSON(t, srv, http.MethodPost, "/api/zones", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /zones = %d, body %s", rec.Code, rec.Body)
	}
	zs, err := st.Zones().Zones(context.Background())
	if err != nil {
		t.Fatalf("Zones: %v", err)
	}
	if got := zs[len(zs)-1].AllowTransfer; got != "10.0.0.5, key:ns2." {
		t.Fatalf("allow_transfer stored as %q, want the canonical spelling", got)
	}
}

func TestZoneCreateRejectsAMalformedAllowTransfer(t *testing.T) {
	srv, _ := newTestServer(t)
	rec := doJSON(t, srv, http.MethodPost, "/api/zones",
		`{"name":"e412.in","allow_transfer":"not-an-ip"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("POST /zones = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "allow_transfer") {
		t.Fatalf("error does not name the field: %s", rec.Body)
	}
}

func TestZoneCreateRejectsAnACLKeyThatDoesNotExist(t *testing.T) {
	srv, _ := newTestServer(t)
	rec := doJSON(t, srv, http.MethodPost, "/api/zones",
		`{"name":"e412.in","allow_transfer":"key:nobody"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("POST /zones = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "nobody") {
		t.Fatalf("error does not name the missing key: %s", rec.Body)
	}
}

func TestZonePatchValidatesAllowTransferToo(t *testing.T) {
	srv, st := newTestServer(t)
	id := mustCreateZone(t, st, "e412.in")
	rec := doJSON(t, srv, http.MethodPatch, fmt.Sprintf("/api/zones/%d", id),
		`{"allow_transfer":"10.0.0.0/33"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("PATCH = %d, want 400 — a rule enforced on POST only has a way around it", rec.Code)
	}
	rec = doJSON(t, srv, http.MethodPatch, fmt.Sprintf("/api/zones/%d", id),
		`{"allow_transfer":"10.0.0.0/24"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH = %d, body %s", rec.Code, rec.Body)
	}
	z, err := st.Zones().Zone(context.Background(), id)
	if err != nil {
		t.Fatalf("Zone: %v", err)
	}
	if z.AllowTransfer != "10.0.0.0/24" {
		t.Fatalf("allow_transfer = %q", z.AllowTransfer)
	}
}

// A secondary serves what it pulled (§9.5.3), so the field is not
// secondary-only the way primaries is.
func TestAllowTransferIsAcceptedOnBothServingTypes(t *testing.T) {
	srv, _ := newTestServer(t)
	for _, body := range []string{
		`{"name":"a.e412.in","type":"primary","allow_transfer":"10.0.0.0/24"}`,
		`{"name":"b.e412.in","type":"secondary","primaries":"192.0.2.1","allow_transfer":"10.0.0.0/24"}`,
	} {
		rec := doJSON(t, srv, http.MethodPost, "/api/zones", body)
		if rec.Code != http.StatusCreated {
			t.Fatalf("POST %s = %d, body %s", body, rec.Code, rec.Body)
		}
	}
}
```

Use whatever helpers `zones_handlers_test.go` already has in place of `newTestServer`/`doJSON`/`mustCreateZone` — match the file's existing style rather than introducing new helpers.

- [ ] **Step 2: Run and watch them fail**

Run: `go test ./internal/api/ -run 'AllowTransfer|ACLKey' -v`
Expected: FAIL — the field is ignored, so the stored value is `""`.

- [ ] **Step 3: Implement**

- `zoneCreate` gains `AllowTransfer string \`json:"allow_transfer"\`` with a doc comment naming the format and the default-deny meaning.
- `zonePatch` gains `AllowTransfer *string \`json:"allow_transfer"\`` beside `Primaries`/`TSIGKeyID`, applied in the same block.
- `checkZoneTransferConfig` grows an `allowTransfer string` parameter and, at its end:

```go
	if allowTransfer != "" {
		if err := zones.ValidateACL(allowTransfer); err != nil {
			return http.StatusBadRequest, err.Error()
		}
		// A key: entry that names nothing is an ACL entry that can never
		// match, so the transfer it was written to permit would be refused
		// with no indication why. Caught here, exactly as tsig_key_id is, and
		// guarded from the other end by tsigKeyStore.Delete.
		for _, name := range zones.ACLKeys(allowTransfer) {
			if _, found, err := s.deps.Store.TSIGKeys().ByName(ctx, name); err != nil {
				return http.StatusServiceUnavailable, "storage unavailable"
			} else if !found {
				return http.StatusBadRequest, "allow_transfer names TSIG key " + name + ", which does not exist"
			}
		}
	}
```

- Both handlers store the canonical spelling rather than the input: after validation, `parsed, _ := zones.ParseACL(input); z.AllowTransfer = zones.FormatACL(parsed)`. The error is discarded only because `ValidateACL` on the same string has already returned nil in the line above; write it as an explicit re-parse with the error checked if that reads better to you, but do not store the raw input — the store's delete guard matches the canonical form.
- `internal/api/openapi.yaml`: add `allow_transfer` to the `Zone` schema, the create body and the patch body, and `last_xfr_at` / `last_xfr_peer` / `last_xfr_error` to `Zone` as read-only. Descriptions state the format and that empty means deny.

- [ ] **Step 4: Run, then commit**

Run: `go test -race ./internal/api/ && go test -race ./...`
Expected: PASS. `TestOpenAPIDocumentsAuthStatuses` and `TestEveryRouteEnforcesAuth` must still pass — no route was added, so they should be untouched.

```bash
git add internal/api
git commit -m "feat(api): allow_transfer on create and patch"
```

---

### Task 4: The intercept

**Files:**
- Create: `internal/dnssrv/transfers.go`, `internal/dnssrv/transfers_test.go`
- Modify: `internal/dnssrv/server.go`

**Interfaces:**
- Produces:
  ```go
  type Transfers interface {
      ServeTransfer(ctx context.Context, w dns.ResponseWriter, q *dns.Msg, key string, tsigErr error)
  }
  func WithTransfers(t Transfers) Option
  const TransferTimeout = 2 * time.Minute
  ```

**The branch is before the `Request` is built and after `RequireTSIG` is asked.** The order matters both ways: the handler needs the TSIG verdict `serve` already computed (asking twice is two chances to disagree), and it must not inherit the 5-second context or reach the OPT force-add.

- [ ] **Step 1: Write the failing tests**

Three helpers these tests share, written once at the top of the file:

- `startServer(t, h dnssrv.Handler, opts ...dnssrv.Option) string` — a `dnssrv.NewServer("127.0.0.1:0", h, opts...)`, started, shut down in `t.Cleanup`, returning `s.Addr()`.
- `countingHandler(t, n *int) dnssrv.Handler` — increments `*n` and answers NOERROR, so "the pipeline ran" is observable.
- `exchangeTCP(t, addr string, m *dns.Msg) *dns.Msg` — `(&dns.Client{Net: "tcp"}).Exchange`, failing the test on error. **TCP, not `dns.Exchange`**, which is UDP: a transfer only ever arrives over TCP, and Task 6 makes an AXFR over UDP answer NOTIMP, which would make a UDP-based test here pass for the wrong reason.

```go
package dnssrv_test

// fakeTransfers records what the intercept handed it and answers NOTIMP, so
// a test can tell "the intercept fired" from "the pipeline answered".
type fakeTransfers struct {
	mu       sync.Mutex
	calls    int
	key      string
	tsigErr  error
	deadline time.Time
	qtype    uint16
}

func (f *fakeTransfers) ServeTransfer(ctx context.Context, w dns.ResponseWriter, q *dns.Msg, key string, tsigErr error) {
	f.mu.Lock()
	f.calls++
	f.key, f.tsigErr, f.qtype = key, tsigErr, q.Question[0].Qtype
	f.deadline, _ = ctx.Deadline()
	f.mu.Unlock()
	m := new(dns.Msg)
	m.SetRcode(q, dns.RcodeNotImplemented)
	_ = w.WriteMsg(m)
}

func TestAXFRGoesToTheTransferHandlerAndNotThePipeline(t *testing.T) {
	ft := &fakeTransfers{}
	pipelineCalls := 0
	h := dnssrv.HandlerFunc(func(ctx context.Context, req *dnssrv.Request) (*dnssrv.Response, error) {
		pipelineCalls++
		m := new(dns.Msg)
		m.SetReply(req.Msg)
		return &dnssrv.Response{Msg: m}, nil
	})
	addr := startServer(t, h, dnssrv.WithTransfers(ft))

	m := new(dns.Msg)
	m.SetQuestion("e412.in.", dns.TypeAXFR)
	reply := exchangeTCP(t, addr, m)
	if reply.Rcode != dns.RcodeNotImplemented {
		t.Fatalf("rcode = %s, want NOTIMP from the fake", dns.RcodeToString[reply.Rcode])
	}
	if ft.calls != 1 {
		t.Fatalf("ServeTransfer called %d times, want 1", ft.calls)
	}
	if pipelineCalls != 0 {
		t.Fatal("the pipeline ran for an AXFR; the intercept exists so it does not")
	}
}

func TestIXFRIsInterceptedToo(t *testing.T) {
	ft := &fakeTransfers{}
	addr := startServer(t, countingHandler(t, new(int)), dnssrv.WithTransfers(ft))
	m := new(dns.Msg)
	m.SetQuestion("e412.in.", dns.TypeIXFR)
	exchangeTCP(t, addr, m)
	if ft.calls != 1 || ft.qtype != dns.TypeIXFR {
		t.Fatalf("calls = %d, qtype = %d; an IXFR is the transfer handler's too", ft.calls, ft.qtype)
	}
}

func TestOrdinaryQueriesStillReachThePipeline(t *testing.T) {
	ft := &fakeTransfers{}
	pipelineCalls := 0
	addr := startServer(t, countingHandler(t, &pipelineCalls), dnssrv.WithTransfers(ft))
	m := new(dns.Msg)
	m.SetQuestion("e412.in.", dns.TypeA)
	exchangeTCP(t, addr, m)
	if ft.calls != 0 {
		t.Fatal("an A query reached the transfer handler")
	}
	if pipelineCalls != 1 {
		t.Fatalf("pipeline ran %d times, want 1", pipelineCalls)
	}
}

func TestWithoutATransferHandlerAnAXFRFallsThrough(t *testing.T) {
	// Unchanged behaviour is the point: this is the configuration every
	// server built before D3 runs in, including every existing test.
	pipelineCalls := 0
	addr := startServer(t, countingHandler(t, &pipelineCalls)) // no WithTransfers
	m := new(dns.Msg)
	m.SetQuestion("e412.in.", dns.TypeAXFR)
	exchangeTCP(t, addr, m)
	if pipelineCalls != 1 {
		t.Fatalf("pipeline ran %d times, want 1: with no handler attached the branch must not be taken", pipelineCalls)
	}
}

func TestTheTransferContextIsNotThePipelines(t *testing.T) {
	// The 5s handler context would abort a large transfer (spec §9.1). Assert
	// the deadline the handler received is more than a minute out.
	ft := &fakeTransfers{}
	addr := startServer(t, noopHandler(t), dnssrv.WithTransfers(ft))
	askAXFR(t, addr)
	if d := time.Until(ft.deadline); d < time.Minute {
		t.Fatalf("transfer deadline is %s away, want the transfer timeout, not the pipeline's 5s", d)
	}
}

func TestTheTSIGVerdictIsPassedToTheHandler(t *testing.T) {
	// With a key store attached and a correctly signed AXFR: ft.key is the
	// canonical key name and ft.tsigErr is nil.
	// With an unsigned AXFR: ft.key == "" and errors.Is(ft.tsigErr,
	// dnssrv.ErrTSIGUnsigned).
}
```

Fill in the sketched bodies completely — `internal/dnssrv/tsig_test.go` already has a server-with-keys fixture and a signed-exchange helper; reuse them rather than writing new ones.

- [ ] **Step 2: Run and watch them fail**

Run: `go test ./internal/dnssrv/ -run Transfer -v`
Expected: FAIL — `undefined: dnssrv.WithTransfers`.

- [ ] **Step 3: Implement**

`internal/dnssrv/transfers.go`:

```go
package dnssrv

import (
	"context"
	"time"

	"github.com/miekg/dns"
)

// TransferTimeout bounds one outbound zone transfer. It replaces the
// pipeline's 5-second handler context, which would abort a large transfer
// mid-zone — the first of the two consequences §9.1 of the zones design
// measured before this existed.
const TransferTimeout = 2 * time.Minute

// Transfers answers AXFR and IXFR, which the pipeline cannot: Handler returns
// exactly one *Response and serve writes exactly one message, while a transfer
// is a sequence of messages on one connection. So this takes the raw
// dns.ResponseWriter and owns the whole reply.
//
// key and tsigErr are what Server.RequireTSIG concluded about this message,
// passed rather than recomputed: two callers of one check are two chances for
// them to disagree, and this is the path where the disagreement would be a
// transfer served to an unauthenticated peer.
//
// Implementations must write exactly one reply on the refusal paths and are
// responsible for their own rcodes; nothing downstream inspects what they
// wrote.
type Transfers interface {
	ServeTransfer(ctx context.Context, w dns.ResponseWriter, q *dns.Msg, key string, tsigErr error)
}

// WithTransfers attaches the handler AXFR and IXFR are routed to. Without it
// the branch is not taken at all and a transfer query falls through the
// pipeline as any other query would, which is what every server built before
// Milestone D3 did.
func WithTransfers(t Transfers) Option {
	return func(s *Server) { s.transfers = t }
}

// isTransferQuery reports whether m is the kind of query the intercept owns.
// Exactly one question, because a transfer names one zone; the rest of the
// malformed-question handling belongs to the handler, which has the rcodes.
func isTransferQuery(m *dns.Msg) bool {
	if len(m.Question) != 1 {
		return false
	}
	switch m.Question[0].Qtype {
	case dns.TypeAXFR, dns.TypeIXFR:
		return true
	}
	return false
}
```

In `server.go`: add `transfers Transfers` to `Server`, and open `serve` with

```go
func (s *Server) serve(w dns.ResponseWriter, m *dns.Msg) {
	key, tsigErr := s.RequireTSIG(w, m)
	if s.transfers != nil && isTransferQuery(m) {
		// Its own deadline, its own writer, and none of the pipeline: qlog's
		// per-query accounting, the filter, the cache and the forwarder have
		// no meaning for a transfer, and the OPT force-add below would stamp
		// OPT onto every envelope.
		ctx, cancel := context.WithTimeout(context.Background(), TransferTimeout)
		defer cancel()
		s.transfers.ServeTransfer(ctx, w, m, key, tsigErr)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// ... the rest unchanged, with the existing RequireTSIG call removed and
	// req.tsig built from the key/tsigErr computed above.
}
```

- [ ] **Step 4: Run, then commit**

Run: `go test -race ./internal/dnssrv/ && go test -race ./...`
Expected: PASS — every existing server test runs without a `Transfers`, which must keep behaving exactly as before.

```bash
git add internal/dnssrv
git commit -m "feat(dns): intercept AXFR and IXFR ahead of the pipeline"
```

---

### Task 5: The gate

**Files:**
- Create: `internal/zones/transferserver.go`, `internal/zones/transferserver_test.go`
- Modify: `internal/zones/zone.go` (add `Index.Apex`), `internal/zones/resolver.go` (expose the snapshot)

**Interfaces:**
- Consumes: `dnssrv.Transfers` (Task 4); `zones.ParseACL`, `ACLAllows` (Task 1); `store.ZoneStore` (Task 2).
- Produces:
  ```go
  func NewTransferServer(r *Resolver, zs store.ZoneStore, opts ...TransferServerOption) *TransferServer
  func (t *TransferServer) ServeTransfer(ctx context.Context, w dns.ResponseWriter, q *dns.Msg, key string, tsigErr error)
  func WithTransferServerNow(now func() time.Time) TransferServerOption
  func (idx *Index) Apex(name string) *Zone
  func (r *Resolver) Snapshot() *Index
  ```

This task implements **only the gate** — every refusal in §9.5.5's table, plus a successful transfer answered as one envelope containing SOA, records, SOA. Batching, the UDP rules, the bounds and the recording arrive in Tasks 6–8, so keep the write path a single `WriteMsg` here and do not anticipate them.

**`Index.Apex`, not `Index.Find`.** `Find` walks suffixes for the closest enclosing zone, which for an AXFR of `sub.e412.in` would transfer `e412.in` — a zone the peer did not ask for and may not be allowed. An AXFR names an apex exactly.

- [ ] **Step 1: Write the failing tests**

The fixture starts a **real** `dnssrv.Server` with a real store behind a real `Resolver`, and asks it with a real client — the D2 posture (`transfer_test.go`'s `startTestPrimary`) pointed the other way.

```go
package zones_test

// xfrFixture is a running dnsaur that serves transfers: a store, a resolver
// reloaded from it, a TransferServer, and a dnssrv.Server on 127.0.0.1:0.
type xfrFixture struct {
	st   store.Store
	res  *zones.Resolver
	addr string
	zone store.Zone
}

func newXFRFixture(t *testing.T, z store.Zone, records []store.ZoneRecord, opts ...zones.TransferServerOption) *xfrFixture {
	t.Helper()
	ctx := context.Background()
	// store.Open on a temp sqlite file, closed in t.Cleanup — the same two
	// lines newTransferFixture opens with (transfer_test.go:233).
	st, err := store.Open(ctx, "sqlite", t.TempDir()+"/t.db")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	id, err := st.Zones().AddZone(ctx, z)
	if err != nil {
		t.Fatalf("AddZone: %v", err)
	}
	for _, r := range records {
		r.ZoneID = id
		if _, err := st.Zones().AddRecord(ctx, r); err != nil {
			t.Fatalf("AddRecord %s: %v", r.Name, err)
		}
	}
	res := zones.NewResolver(st.Zones())
	if err := res.Reload(ctx); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	ts := zones.NewTransferServer(res, st.Zones(), opts...)
	// The pipeline handler answers REFUSED: nothing in these tests should
	// reach it, and a distinctive rcode makes "the intercept did not fire"
	// look different from every rcode the gate produces.
	srv := dnssrv.NewServer("127.0.0.1:0", dnssrv.HandlerFunc(
		func(ctx context.Context, req *dnssrv.Request) (*dnssrv.Response, error) {
			m := new(dns.Msg)
			m.SetRcode(req.Msg, dns.RcodeRefused)
			return &dnssrv.Response{Msg: m}, nil
		}),
		dnssrv.WithTSIGKeys(st.TSIGKeys()),
		dnssrv.WithTransfers(ts))
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	})
	stored, err := st.Zones().Zone(ctx, id)
	if err != nil {
		t.Fatalf("Zone: %v", err)
	}
	return &xfrFixture{st: st, res: res, addr: srv.Addr(), zone: stored}
}

// axfr asks for a zone over TCP and returns every RR received. A refusal is
// one message with an rcode and no records, and dns.Transfer.In reports that
// as an error, so both are returned and the caller asserts on whichever it
// expects.
//
// tsig, when non-nil, is the (name -> base64 secret) map the request is
// signed with and the reply stream is verified against.
func (f *xfrFixture) axfr(t *testing.T, qname string, tsig map[string]string) ([]dns.RR, error) {
	t.Helper()
	m := new(dns.Msg)
	m.SetAxfr(dns.Fqdn(qname))
	tr := new(dns.Transfer)
	if tsig != nil {
		for name, secret := range tsig {
			m.SetTsig(name, dns.HmacSHA256, 300, time.Now().Unix())
			tr.TsigSecret = map[string]string{name: secret}
		}
	}
	ch, err := tr.In(m, f.addr)
	if err != nil {
		return nil, err
	}
	var out []dns.RR
	for env := range ch {
		if env.Error != nil {
			return out, env.Error
		}
		out = append(out, env.RR...)
	}
	return out, nil
}

// rcode asks the same question over plain TCP and returns the rcode of the
// first message, which is the whole of a refusal. dns.Transfer.In collapses
// every refusal into one error string, and these tests assert on the rcode
// itself — REFUSED and NOTAUTH mean different things to the operator reading
// the peer's log.
func (f *xfrFixture) rcode(t *testing.T, qname string, qtype uint16) *dns.Msg {
	t.Helper()
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(qname), qtype)
	c := &dns.Client{Net: "tcp"}
	reply, _, err := c.Exchange(m, f.addr)
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	return reply
}

func TestAXFRServesAPrimaryZoneToAnAllowedPeer(t *testing.T) {
	f := newXFRFixture(t, store.Zone{
		Name: "e412.in", Type: "primary", Enabled: true,
		AllowTransfer: "127.0.0.0/8",
		SOANS: "ns1.e412.in", SOAMbox: "hostmaster.e412.in", SOASerial: 3,
	}, []store.ZoneRecord{
		{Name: "@", Type: "NS", TTL: 300, RData: "ns1.e412.in.", Enabled: true},
		{Name: "bifrost", Type: "A", TTL: 300, RData: "10.0.0.1", Enabled: true},
		{Name: "disabled", Type: "A", TTL: 300, RData: "10.0.0.9", Enabled: false},
	})

	rrs, err := f.axfr(t, "e412.in", nil)
	if err != nil {
		t.Fatalf("axfr: %v", err)
	}
	for _, rr := range rrs {
		if strings.HasPrefix(rr.Header().Name, "disabled.") {
			t.Fatal("a disabled record was transferred; the snapshot drops it, and a querier never sees it either")
		}
	}
	if len(rrs) < 3 {
		t.Fatalf("got %d RRs, want the SOA, the records and the closing SOA", len(rrs))
	}
	if _, ok := rrs[0].(*dns.SOA); !ok {
		t.Fatalf("first RR is %T, want SOA (RFC 5936 §2.2)", rrs[0])
	}
	if _, ok := rrs[len(rrs)-1].(*dns.SOA); !ok {
		t.Fatalf("last RR is %T, want SOA (RFC 5936 §2.2)", rrs[len(rrs)-1])
	}
	// Every enabled record is present, and the disabled ones are not: the
	// snapshot dropped them at build, so this is what a querier sees too.
}

func TestAXFRRefusesAPeerOutsideTheACL(t *testing.T) {
	// AllowTransfer "10.0.0.0/24"; the client is 127.0.0.1. Want REFUSED.
}

func TestAXFRRefusesWhenAllowTransferIsEmpty(t *testing.T) {
	// Default deny. Want REFUSED, and no records at all.
}

func TestAXFRIsNotAuthForAnApexWeDoNotHold(t *testing.T) {
	// RFC 5936 §2.2.1. Want NOTAUTH.
}

func TestAXFRIsNotAuthForASubdomainOfAZoneWeHold(t *testing.T) {
	// sub.e412.in when only e412.in exists: NOTAUTH, not a transfer of the
	// parent. This is why Apex exists rather than Find.
}

func TestAXFRIsNotAuthForADisabledZone(t *testing.T) {
	// A disabled zone is not served to queries either.
}

func TestAXFRIsNotAuthForAnInternalZone(t *testing.T) {
	// One of the RFC 6303 built-ins, with an ACL set so the refusal is about
	// the type rather than the ACL.
}

func TestAXFROfASecondaryBeforeItsFirstTransferIsServfail(t *testing.T) {
	// refreshed_at = 0. Serving() is false, so we cannot vouch for it.
}

func TestAXFROfAnExpiredSecondaryIsServfail(t *testing.T) {
	// expires_at in the past, driven through WithTransferServerNow rather than
	// by sleeping.
}

func TestAXFROfALiveSecondaryIsServed(t *testing.T) {
	// refreshed_at set, expires_at ahead: a secondary re-serves what it pulled.
}

func TestAXFRWithAKeyEntryRequiresASignature(t *testing.T) {
	// AllowTransfer "key:ns2." only. An unsigned request from 127.0.0.1 is
	// REFUSED — an unsigned request is an ACL outcome, not a TSIG failure.
}

func TestAXFRSignedUnderTheNamedKeyIsServed(t *testing.T) {
	// The same zone, signed correctly: the transfer runs, and the stream
	// verifies (dns.Transfer with TsigProvider set).
}

func TestAXFRWithAnUnknownKeyIsNotAuthBadKey(t *testing.T) {
	// Signed under a key the server does not hold: NOTAUTH, and the reply
	// carries a TSIG RR with Error == dns.RcodeBadKey and an empty MAC.
}

func TestAXFRWithABadMACIsNotAuthBadSig(t *testing.T) {
	// Same key name, wrong secret: NOTAUTH + dns.RcodeBadSig, unsigned.
}

func TestAXFROutsideTheFudgeWindowIsNotAuthBadTime(t *testing.T) {
	// Signed with a TimeSigned far in the past: NOTAUTH + dns.RcodeBadTime,
	// and Other Data carries the server's time.
}
```

Every one of these must assert the rcode explicitly, and the `Error` code on the reply's TSIG RR where one is expected.

**Corrected 2026-08-13, after Task 5 caught it:** the three error replies are not signed alike, and an earlier draft of this plan said they were. RFC 8945 §5.2.1 (BADKEY) and §5.2.2 (BADSIG) each say "This response MUST be unsigned" — the server has no verified key, so signing would assert an authenticity it could not establish. §5.2.3 says the opposite for BADTIME: "A response indicating a BADTIME error MUST be signed by the same key as the request. It MUST include the client's current time in the Time Signed field, the server's current time (an unsigned 48-bit integer) in the Other Data field, and 6 in the Other Len field." There the key and the MAC did verify and only the clocks disagree, so the peer both can and must be told in a form it can trust. miekg encodes exactly this in `TsigGenerateWithProvider` (`tsig.go:196`), so routing every error reply through `WriteMsg` gets it right; a hand-rolled unsigned writer does not. Assert `MAC == ""` for BADKEY and BADSIG, and a *valid* MAC for BADTIME.

- [ ] **Step 2: Run and watch them fail**

Run: `go test ./internal/zones/ -run 'AXFR' -v`
Expected: FAIL — `undefined: zones.NewTransferServer`.

- [ ] **Step 3: Implement**

`Index.Apex` in `zone.go`:

```go
// Apex returns the zone whose apex is exactly name, or nil. Unlike Find it
// does not walk suffixes: a transfer names one zone, and answering an AXFR
// for sub.e412.in with e412.in would hand over a zone the peer did not ask
// for and may not be permitted.
func (idx *Index) Apex(name string) *Zone {
	i, ok := idx.byApex[normalizeName(name)]
	if !ok {
		return nil
	}
	return &idx.zones[i]
}

// Snapshot returns the Index currently being served. (resolver.go)
func (r *Resolver) Snapshot() *Index { return r.snap.Load() }
```

`transferserver.go` holds the gate. Structure it as one `decide` function returning a small struct, so the rcode table is readable in one place and every test above maps to one branch:

```go
// refusal is why a transfer will not happen, and what to say about it.
type refusal struct {
	rcode    int
	tsigCode uint16 // dns.RcodeBadKey/BadSig/BadTime, or 0 for no TSIG error RR
	reason   string // the log line, and last_xfr_error in Task 8
}

func (t *TransferServer) decide(q *dns.Msg, peer netip.Addr, key string, tsigErr error) (*Zone, *refusal)
```

`ServeTransfer` returns nothing — the interface owns the whole reply — so give it an inner form that can use `return`:

```go
func (t *TransferServer) ServeTransfer(ctx context.Context, w dns.ResponseWriter, q *dns.Msg, key string, tsigErr error) {
	if err := t.serve(ctx, w, q, key, tsigErr); err != nil {
		// Nothing above can act on it: the connection is this handler's, and
		// a peer that has gone away is the ordinary case rather than a fault.
		slog.Warn("zone transfer ended early", "qname", qnameOf(q), "err", err)
	}
}

func (t *TransferServer) serve(ctx context.Context, w dns.ResponseWriter, q *dns.Msg, key string, tsigErr error) error
```

The order is §9.5.5's, and each branch carries the comment saying which rule it is:

1. `len(q.Question) != 1` or `q.Question[0].Qclass != dns.ClassINET` → FORMERR.
2. `dns.CanonicalName` the qname; `idx.Apex(...)`; nil, `!z.Enabled`, or a type other than primary/secondary → NOTAUTH (RFC 5936 §2.2.1).
3. `z.Type == "secondary" && !z.Serving(t.now().UnixMilli())` → SERVFAIL.
4. `tsigErr != nil`, mapped: `dns.ErrSecret`/`dns.ErrKeyAlg` → BADKEY, `dns.ErrSig` → BADSIG, `dns.ErrTime` → BADTIME, `dnssrv.ErrTSIGUnsigned` → not a failure, fall through to the ACL with `key == ""`, `dnssrv.ErrTSIGUnavailable` and anything else → SERVFAIL with no TSIG error RR (a store that broke is not the peer's fault).
5. `ParseACL(z.AllowTransfer)`; a parse error → REFUSED, logged once, fails closed.
6. `!ACLAllows(entries, peer, key)` → REFUSED.

The refusal writer builds one message, `SetRcode`, attaches the TSIG error RR when `tsigErr != 0`:

```go
// RFC 8945: a request that did not verify is answered with NOTAUTH and a TSIG
// RR naming the failure. Whether that reply is signed depends on which failure
// it was — §5.2.1/§5.2.2 make BADKEY and BADSIG unsigned, because signing would
// assert an authenticity the server was unable to establish (the same
// conclusion dnssrv/server.go reached for the ordinary path), while §5.2.3
// requires BADTIME to be signed, because there the key and MAC did verify.
// Build the RR and let WriteMsg apply that rule; it already does (tsig.go:196).
func errorTSIG(q *dns.Msg, code uint16, now time.Time) *dns.TSIG {
	req := q.IsTsig() // never nil on this path: only a signed request gets here
	rr := &dns.TSIG{
		Hdr:        dns.RR_Header{Name: req.Hdr.Name, Rrtype: dns.TypeTSIG, Class: dns.ClassANY},
		Algorithm:  req.Algorithm,
		TimeSigned: uint64(now.Unix()),
		Fudge:      req.Fudge,
		MAC:        "",
		Error:      code,
		OrigId:     q.Id,
	}
	if code == dns.RcodeBadTime {
		// RFC 8945: Other Data carries the server's own time for BADTIME, so
		// the peer can tell a clock skew from a replay. It is the 48-bit time
		// in network order, and miekg stores the field hex-encoded with
		// OtherLen counting the bytes (tsig.go:106-107, `dns:"size-hex"`).
		var b [6]byte
		t := uint64(now.Unix())
		for i := 0; i < 6; i++ {
			b[5-i] = byte(t >> (8 * i))
		}
		rr.OtherLen = 6
		rr.OtherData = hex.EncodeToString(b[:])
	}
	return rr
}
```

The zone-type checks use `strings.EqualFold(z.Type, "secondary")`, which is how this package already spells it (`answer.go:93`, `refresh.go:587`) — there are no exported type constants and this task does not add any.

The success path, for this task only, is one message: SOA, every record through `ToRR`, SOA again. A record that will not render aborts with SERVFAIL — a zone silently missing a record is worse on a secondary than a visible failure, because nothing downstream will notice.

- [ ] **Step 4: Run, then commit**

Run: `go test -race ./internal/zones/ -run 'AXFR' -v`
Expected: PASS.

Then, one at a time, break each of the ACL check and the `Serving()` check and confirm the matching test fails. Restore.

```bash
git add internal/zones
git commit -m "feat(zones): serve a zone over AXFR, and refuse every other way"
```

---

### Task 6: Envelopes, and the two UDP rules

**Files:**
- Modify: `internal/zones/transferserver.go`
- Test: `internal/zones/transferserver_test.go`

**Interfaces:**
- Consumes: everything from Task 5.
- Produces: `const envelopeTargetBytes = 16 << 10`; the batching is internal.

- [ ] **Step 1: Write the failing tests**

```go
func TestALargeZoneArrivesInSeveralEnvelopes(t *testing.T) {
	// 2,000 A records at ~40 bytes each is well past a 16 KiB envelope.
	// Count messages by reading the channel dns.Transfer.In returns: each
	// envelope is one receive. Assert more than one, that every RR arrives
	// exactly once, and that no message exceeds 65535 bytes packed.
}

func TestTheSOAAppearsOnlyFirstAndLast(t *testing.T) {
	// Across a multi-envelope transfer: exactly two SOAs, at the two ends.
	// RFC 5936 §2.2 — "Intermediate messages MUST NOT contain the SOA".
}

func TestOPTIsEchoedOnTheFirstMessageOnly(t *testing.T) {
	// Query with EDNS0 set. First message has an OPT, later ones do not
	// (RFC 5936 §2.2.5: SHOULD on the first, MAY on the rest).
	// Requires reading raw messages rather than the RR channel — use
	// dns.Transfer's ReadMsg loop, or a hand-rolled TCP client.
}

func TestASignedMultiEnvelopeStreamVerifies(t *testing.T) {
	// dns.Transfer with TsigProvider set and TsigSecret naming the key: the
	// library verifies the running MAC across every envelope, so a transfer
	// that completes without error IS the assertion. Also assert the record
	// count, so a stream that verified but arrived empty cannot pass.
}

func TestIXFROverTCPIsAnsweredWithTheWholeZone(t *testing.T) {
	// RFC 1995 §2 permits it and D5 has not happened. Ask IXFR, get the same
	// RRs an AXFR gives.
}

func TestAXFROverUDPIsNotImplemented(t *testing.T) {
	// RFC 5936 §4.2 leaves it undefined; NOTIMP is our answer. Use
	// dns.Client{Net: "udp"}.
}

func TestIXFROverUDPIsAnsweredWithASingleSOA(t *testing.T) {
	// RFC 1995 §2: "the query is responded to with a single SOA record of the
	// server's current version to inform the client that a TCP query should be
	// initiated." Exactly one answer RR, an SOA, with the zone's serial.
}

func TestUDPIXFRStillObeysTheACL(t *testing.T) {
	// The gate runs before the transport handling: a peer outside the ACL gets
	// REFUSED, not a serial.
}
```

- [ ] **Step 2: Run and watch them fail**

Run: `go test ./internal/zones/ -run 'Envelope|SOAAppears|OPTIsEchoed|Signed|IXFR|OverUDP' -v`
Expected: FAIL — one envelope for everything, and UDP unhandled.

- [ ] **Step 3: Implement**

```go
// envelopeTargetBytes is what one AXFR message is filled to. Well under
// TCP's 65535 ceiling, and comfortably inside RFC 5936 §2.2's "sufficient
// number of RRs to reasonably amortize the per-message overhead, up to the
// largest number that will fit within a DNS message".
const envelopeTargetBytes = 16 << 10

// batch fills envelopes from rrs, closing one when the next RR would pass
// budget.
//
// Size is accumulated with dns.Len, which measures a record *uncompressed*,
// while the message that is sent has Compress set. The estimate is therefore
// an upper bound and the envelope always fits, at the price of slightly
// under-filled messages — which §2.2 permits, since it asks for amortisation
// rather than a maximum. Packing after each record to measure exactly is
// quadratic in a zone's record count to recover bytes nobody counts.
func batch(rrs []dns.RR, budget int) [][]dns.RR
```

The writer loop, replacing Task 5's single `WriteMsg`:

```go
for i, b := range batches {
	m := new(dns.Msg)
	m.SetReply(q)
	m.Authoritative = true
	m.Compress = true
	m.Answer = b
	if i == 0 && q.IsEdns0() != nil {
		m.SetEdns0(dns.DefaultMsgSize, false) // RFC 5936 §2.2.5
	}
	if req := q.IsTsig(); req != nil && tsigErr == nil {
		// RFC 8945: every message of the response is signed. WriteMsg fills
		// the MAC and chains it; TsigTimersOnly below is what makes the
		// second and later messages timers-only.
		m.SetTsig(req.Hdr.Name, req.Algorithm, tsigFudge, time.Now().Unix())
	}
	if err := w.WriteMsg(m); err != nil {
		return err
	}
	w.TsigTimersOnly(true)
}
```

`time.Now()` and not the injected clock, deliberately: the peer checks this timestamp against its own clock inside the fudge window, so a test clock would produce a signature a real peer rejects. `Transferrer.now`'s doc comment makes the same distinction for the same reason.

The budget subtracts the signature's room up front when the request is signed: `dns.Len(stub) + maxTSIGMACLen`. `maxTSIGMACLen` is unexported in `dnssrv`; define the same 64 here with a comment naming SHA-512 as the largest, rather than exporting it — one number in two packages with a comment beats an export that invites a third caller.

Transport handling sits in `ServeTransfer`, between the gate and the stream:

```go
_, isUDP := w.RemoteAddr().(*net.UDPAddr)
if isUDP {
	if q.Question[0].Qtype == dns.TypeAXFR {
		// RFC 5936 §4.2: "AXFR sessions over UDP transport are not defined."
		// The RFC names no rcode, so NOTIMP is ours, and it is the honest one
		// for a transport this server does not implement.
		return t.refuse(w, q, &refusal{rcode: dns.RcodeNotImplemented, reason: "axfr over udp"})
	}
	// RFC 1995 §2: "If the UDP reply does not fit, the query is responded to
	// with a single SOA record of the server's current version to inform the
	// client that a TCP query should be initiated." Our IXFR answer is the
	// whole zone, so it does not fit for any zone worth transferring, and the
	// single SOA is the answer for every UDP IXFR rather than a size branch.
	return t.answerSOAOnly(w, q, z)
}
```

- [ ] **Step 4: Run, then commit**

Run: `go test -race ./internal/zones/ -v`
Expected: PASS.

Then set `envelopeTargetBytes` to `1 << 20` and confirm `TestALargeZoneArrivesInSeveralEnvelopes` fails. Restore.

```bash
git add internal/zones
git commit -m "feat(zones): batch a transfer into envelopes, and answer the two UDP cases"
```

---

### Task 7: Runtime bounds

**Files:**
- Modify: `internal/zones/transferserver.go`
- Test: `internal/zones/transferserver_test.go`

**Interfaces:**
- Produces: `func WithMaxConcurrentTransfers(n int) TransferServerOption`; `const DefaultMaxConcurrentTransfers = 4`.

**Why a cap at all.** `dns.Server.WriteTimeout` is documented at `server.go:220-221` and never applied — there is no `SetWriteDeadline` anywhere on the serve path, and `dns.ResponseWriter` exposes no connection to set one on. A peer that stops reading blocks `WriteMsg` indefinitely, holding a goroutine and that zone's built RR slice. Bounding how many can do that at once is the only lever the library leaves.

- [ ] **Step 1: Write the failing tests**

```go
func TestTransfersBeyondTheCapAreRefused(t *testing.T) {
	// Cap of 1, and a hold hook that blocks the first transfer inside the
	// writer (the D2 test primary's withHold does the same job from the other
	// side). While it is held, a second AXFR gets SERVFAIL. Release, and a
	// third succeeds — the cap is concurrent, not cumulative.
}

func TestTheTransferContextBoundsAStalledTransfer(t *testing.T) {
	// The context comes from dnssrv.serve, so it cannot be shortened through
	// a TransferServer option. Call ServeTransfer directly instead — it is
	// exported and takes the context as its first argument — with a writer
	// that blocks in WriteMsg and a context that expires in 50ms.
	//
	//	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	//	defer cancel()
	//	done := make(chan struct{})
	//	go func() { ts.ServeTransfer(ctx, blockingWriter, axfrQuery, "", dnssrv.ErrTSIGUnsigned); close(done) }()
	//
	// Assert it returns within a second, and that a following transfer
	// through the real server succeeds — which is what proves the slot was
	// released rather than leaked.
}

// blockingWriter is a dns.ResponseWriter whose WriteMsg blocks until release
// is closed, standing in for a peer that stopped reading. Implement the whole
// interface: LocalAddr, RemoteAddr (a *net.TCPAddr, since the transport check
// reads it), WriteMsg, Write, Close, TsigStatus, TsigTimersOnly, Hijack.
type blockingWriter struct {
	release <-chan struct{}
	remote  *net.TCPAddr
}
```

The hold hook is test-only wiring on the `TransferServer` (an unexported func field set through an option in `export_test.go`, or an exported `WithTransferHook` documented as such). Choose the shape the package already uses for test seams and say so in the comment.

- [ ] **Step 2: Run and watch them fail**

Run: `go test ./internal/zones/ -run 'Cap|Stalled' -v`
Expected: FAIL — every transfer is served.

- [ ] **Step 3: Implement**

A buffered channel as the semaphore, acquired non-blockingly:

```go
select {
case t.slots <- struct{}{}:
	defer func() { <-t.slots }()
default:
	// Not a queue: a peer waiting on a slot is a goroutine held by exactly
	// the thing the cap exists to bound. SERVFAIL and it retries on its own
	// SOA schedule.
	return t.refuse(w, q, &refusal{rcode: dns.RcodeServerFailure, reason: "at the concurrent transfer limit"})
}
```

The context is checked between envelopes: `if err := ctx.Err(); err != nil { return err }` at the top of each loop iteration.

- [ ] **Step 4: Run, then commit**

Run: `go test -race ./internal/zones/ -v`

```bash
git add internal/zones
git commit -m "fix(zones): bound concurrent transfers, since the library's write timeout is never applied"
```

---

### Task 8: Recording what happened

**Files:**
- Modify: `internal/zones/transferserver.go`
- Test: `internal/zones/transferserver_test.go`

**Interfaces:**
- Consumes: `store.ZoneStore.NoteTransferServed` (Task 2).
- Produces: `const transferStateThrottle = 10 * time.Second`.

- [ ] **Step 1: Write the failing tests**

```go
func TestASuccessfulTransferRecordsWhoTookIt(t *testing.T) {
	// After an AXFR: last_xfr_at is the injected now, last_xfr_peer is
	// 127.0.0.1 (the address only, no port — the port is a new ephemeral
	// number every time and identifies nothing), last_xfr_error is "".
}

func TestARefusalRecordsItsReason(t *testing.T) {
	// A peer outside the ACL: last_xfr_error names the ACL, and last_xfr_peer
	// is that peer. This is the diagnostic the column exists for.
}

func TestStateWritesAreThrottled(t *testing.T) {
	// Ten refusals inside the throttle window with the clock held still: the
	// store is written once. Count with a ZoneStore wrapper that counts
	// NoteTransferServed calls.
}

func TestAChangedOutcomeIsWrittenThroughTheThrottle(t *testing.T) {
	// A refusal, then a success one second later: two writes, not one. A
	// screen that says "refused" for ten seconds after it started working is
	// wrong exactly when someone is watching it.
}

func TestTheThrottleIsPerZone(t *testing.T) {
	// A refusal on zone A does not throttle the first record on zone B.
}

func TestAFailedStateWriteDoesNotFailTheTransfer(t *testing.T) {
	// A store that errors on NoteTransferServed: the peer still gets its
	// zone. The records are what the transfer is for; the bookkeeping is not
	// worth failing it over.
}
```

- [ ] **Step 2: Run and watch them fail**

Run: `go test ./internal/zones/ -run 'Records|Throttl|ChangedOutcome' -v`

- [ ] **Step 3: Implement**

```go
// transferStateThrottle bounds how often a peer can make us write the zone
// row. Both outcomes are recorded — a refusal is the diagnostic an operator
// actually needs — which means an unauthenticated peer in a loop would
// otherwise become an UPDATE loop against sqlite's single connection
// (store.SetMaxOpenConns(1)). The log line is never throttled.
const transferStateThrottle = 10 * time.Second

// note records one outbound attempt, subject to the throttle.
//
// The exception is what keeps the throttle honest: a *changed* outcome is
// written whatever the window says. A run of refusals followed by a success
// would otherwise leave the screen saying "refused" for up to ten seconds
// after the thing started working, which is precisely when somebody is
// watching it.
func (t *TransferServer) note(ctx context.Context, z *Zone, peer netip.Addr, reason string) {
	now := t.now()
	t.mu.Lock()
	last, seen := t.lastNote[z.ID]
	changed := !seen || last.reason != reason
	if !changed && now.Sub(last.at) < transferStateThrottle {
		t.mu.Unlock()
		return
	}
	t.lastNote[z.ID] = noteState{at: now, reason: reason}
	t.mu.Unlock()
	if err := t.zs.NoteTransferServed(ctx, z.ID, now.UnixMilli(), peer.String(), reason); err != nil {
		slog.Warn("recording a served transfer failed", "zone", z.Name, "err", err)
	}
}
```

`ctx` here must not be the transfer's context if the transfer has already ended — use `context.WithoutCancel(ctx)` for the write on the success path, so a peer that hangs up the instant the last envelope lands does not cancel the record of it.

The log lines, at the same call site: `slog.Info("zone transfer served", "zone", ..., "peer", ..., "key", ..., "records", ..., "envelopes", ..., "dur", ...)` and `slog.Warn("zone transfer refused", "zone", ..., "peer", ..., "reason", ...)`.

- [ ] **Step 4: Run, then commit**

Run: `go test -race ./internal/zones/ -v`

Then remove the `changed` clause and confirm `TestAChangedOutcomeIsWrittenThroughTheThrottle` fails. Restore.

```bash
git add internal/zones
git commit -m "feat(zones): record who last took a zone, and why one was refused"
```

---

### Task 9: Wiring, and dnsaur transferring from dnsaur

**Files:**
- Modify: `internal/app/app.go`
- Test: `internal/zones/loopback_test.go` (create), `internal/app/app_e2e_test.go`

**Interfaces:**
- Consumes: `zones.NewTransferServer` (Tasks 5–8), `dnssrv.WithTransfers` (Task 4).

**The loopback test is the point of this task.** D2's client and D3's server were written against RFCs, not against each other. One dnsaur pulling a zone from another dnsaur is the only test in this milestone where both halves are ours and a disagreement between them has nowhere to hide.

- [ ] **Step 1: Write the failing tests**

```go
func TestADnsaurSecondaryTransfersFromADnsaurPrimary(t *testing.T) {
	// Primary: an xfrFixture serving e412.in with allow_transfer "127.0.0.0/8"
	// and half a dozen records of assorted types (A, AAAA, MX, TXT, CNAME,
	// SRV) — the point is that rdata survives a round trip through both
	// halves, not that a transfer happens.
	//
	// Secondary: a second store, a zone of type secondary whose primaries is
	// the first server's address, and D2's Transferrer.
	//
	// Transfer, then assert: the record set matches the primary's exactly
	// (name, type, ttl, rdata), the SOA serial matches, refreshed_at and
	// expires_at were stamped, and the secondary's resolver answers a query
	// for one of the names.
}

func TestTheLoopbackTransferIsSignedEndToEnd(t *testing.T) {
	// The same, with a TSIG key present in both stores: the primary's
	// allow_transfer is "key:ns2." alone, so an unsigned transfer would be
	// refused, and the secondary's zone names that key. A completed transfer
	// proves the signature this server generates is one this server accepts.
}

func TestAZoneServedOverAXFRExcludesDisabledRecords(t *testing.T) {
	// A disabled record on the primary does not reach the secondary — the
	// snapshot dropped it, which is the same thing a querier sees.
}
```

In `internal/app/app_e2e_test.go`, extend the existing end-to-end run: create a zone through the API with an `allow_transfer`, then AXFR it off the running server and assert the records arrive.

- [ ] **Step 2: Run and watch them fail**

Run: `go test ./internal/zones/ -run Loopback -v` and `go test ./internal/app/ -v`
Expected: FAIL — nothing wires `WithTransfers` yet, so the app's own server refuses.

- [ ] **Step 3: Implement**

In `app.go`, beside the existing `zoneRefresh` construction:

```go
	a.xfrOut = zones.NewTransferServer(a.resolver, st.Zones())
```

and in `Start`, on each listener:

```go
		s := dnssrv.NewServer(addr, handler,
			dnssrv.WithTSIGKeys(a.st.TSIGKeys()),
			dnssrv.WithTransfers(a.xfrOut))
```

- [ ] **Step 4: Run, then commit**

Run: `go test -race ./... && ~/go/bin/golangci-lint run ./... && gofmt -l internal cmd`
Expected: PASS, no gofmt output.

```bash
git add internal
git commit -m "feat(app): serve zone transfers, and pin dnsaur transferring from dnsaur"
```

---

### Task 10: Web — the ACL field and the served line

**Files:**
- Create: `web/src/lib/acl.ts`, `web/src/lib/acl.test.ts`
- Modify: `web/src/api/types.ts`, `web/src/pages/zones/detail.tsx`, `web/src/pages/zones/detail.test.tsx`, `web/src/pages/tsig-keys.tsx`, `web/src/pages/tsig-keys.test.tsx`, `web/e2e/smoke.spec.ts`

**Interfaces:**
- Consumes: `allow_transfer`, `last_xfr_at`, `last_xfr_peer`, `last_xfr_error` on the `Zone` JSON (Tasks 2–3).
- Produces: `parseACL(s: string): { ok: true; entries: ACLEntry[] } | { ok: false; error: string }`, `aclKeyNames(s: string): string[]` in `web/src/lib/acl.ts`.

**Where it goes.** Serving transfers applies to both primary and secondary zones, so this is neither part of `SoaBand` (primary-only) nor `TransferBand` (secondary-only). It is its own band, below both, and it renders for both types.

**Copy, plain and brief** — the fact, then stop:

- Field label: `Allow transfer`, placeholder `10.0.0.0/24, key:ns2`, empty state `No peer may transfer this zone.`
- Served: `Last served 2m ago to 10.0.0.5`
- Refused: `Refused 10.0.0.9 — not in allow transfer` (the reason verbatim from `last_xfr_error`, as `TransferBand` already shows `last_error` verbatim)
- Never: `Never asked for.`

- [ ] **Step 1: Write the failing tests**

`web/src/lib/acl.test.ts` mirrors Task 1's table: empty is valid, CIDR, bare address, `key:` canonicalisation, whitespace, bad mask, hostname refused, `key:` with no name refused. `aclKeyNames` returns canonical names and `[]` for an unparseable value.

`web/src/pages/zones/detail.test.tsx`:

```tsx
it("shows who may transfer the zone", async () => { /* renders the value */ });
it("says plainly when no peer may", async () => { /* empty -> the empty state */ });
it("rejects a malformed entry before sending it", async () => { /* the field shows the error, no PATCH is made */ });
it("shows the last peer served and when", async () => { /* last_xfr_at/peer, no error */ });
it("shows a refusal with its reason", async () => { /* last_xfr_error verbatim */ });
it("says nothing has asked when last_xfr_at is 0", async () => { /* the never state */ });
it("shows the band for a secondary too", async () => { /* §9.5.3 */ });
```

`web/src/pages/tsig-keys.test.tsx`: a key named only by a zone's `allow_transfer` counts as in use — `UsedByCell` shows 1 and the delete button is disabled with the existing "Remove it from them first." copy.

`web/e2e/smoke.spec.ts`: set an allow transfer on a zone through the UI and assert it persists across a reload.

- [ ] **Step 2: Run and watch them fail**

Run: `cd web && pnpm test`

- [ ] **Step 3: Implement**

- `web/src/api/types.ts`: the four new `Zone` fields, each with the same one-line explanation the Go struct carries.
- `web/src/lib/acl.ts`: the parser, a direct port of Task 1's rules. It exists so the field can reject a malformed entry without a round trip and so the TSIG screen can count usage; the server validates independently and remains the authority.
- `web/src/pages/zones/detail.tsx`: an `AllowTransferBand` following `SoaBand`'s form/field/mutation shape, mounted for primary and secondary.
- `web/src/pages/tsig-keys.tsx`: `usageByKeyID` counts a zone once if `zone.tsig_key_id === id` **or** `aclKeyNames(zone.allow_transfer)` contains the key's name.

- [ ] **Step 4: Run, then commit**

Run: `cd web && pnpm lint && pnpm format:check && pnpm typecheck && pnpm test && pnpm build && pnpm test:e2e`

```bash
git add web
git commit -m "feat(web): allow transfer, and who last took the zone"
```

---

### Task 11: Docs

**Files:**
- Modify: `README.md`, `docs/api.md`, `docs/architecture.md`, `docs/ui-contract.md`

- [ ] **Step 1: Write them**

- `README.md`: the feature list gains serving zone transfers to secondaries, with TSIG and an allow-transfer ACL.
- `docs/api.md`: `allow_transfer` on create and patch — the format, that empty means deny, that a `key:` name must exist, and that the stored value is the canonical spelling rather than what was typed. The three read-only `last_xfr_*` fields.
- `docs/architecture.md`: the intercept — why a transfer branches ahead of the pipeline rather than being a middleware, and that it therefore never appears in the query log. The rcode table from §9.5.5 belongs here in short form, since it is what an operator debugging a secondary reads.
- `docs/ui-contract.md`: the allow-transfer band and its four states.

- [ ] **Step 2: Check the tree**

Run: `grep -rn "allow_transfer" docs/ README.md`
Expected: the format is described in exactly one place and referenced from the others — the same rule the `primaries` docs follow.

- [ ] **Step 3: Commit**

```bash
git add README.md docs
git commit -m "docs: serving zone transfers, the ACL, and what each refusal means"
```

---

## Done when

- `go test -race ./...`, `~/go/bin/golangci-lint run ./...`, `gofmt -l internal cmd` clean on a committed tree.
- `cd web && pnpm lint && pnpm format:check && pnpm typecheck && pnpm test && pnpm build && pnpm test:e2e` clean.
- A dnsaur secondary transfers a zone from a dnsaur primary, signed, in a test that would fail if either half were wrong.
- Every row of §9.5.5's rcode table has a test asserting that rcode.
