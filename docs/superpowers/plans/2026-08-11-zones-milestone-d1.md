# Zones Milestone D1 — TSIG keys Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** TSIG keys as a first-class resource — stored, managed over the API, and live on the DNS server via a provider that picks up new keys without a restart.

**Architecture:** A `tsig_keys` table and store CRUD in the existing pattern; REST endpoints alongside the other resources; and a `dns.TsigProvider` implementation backed by the store, wired into both the UDP and TCP `dns.Server`s. Verification is enforced by explicitly checking `TsigStatus()`, because miekg sets a status but does not refuse an unsigned message.

**Tech Stack:** Go 1.26, `miekg/dns` v1.1.72, goose migrations (SQL), sqlite (`modernc.org/sqlite`) + Postgres (`pgx`).

**Spec:** §9.3 of `docs/superpowers/specs/2026-08-08-zones-design.md`.

## Global Constraints

- Migrations: `0008` was used by Task 1 and **the next free number is `0009`**. Do not derive this from the directory listing alone — `internal/store/migrate.go` claims version 7 for a Go migration (`upBuiltinZones`) with no corresponding SQL file, and filing a duplicate version fails goose at startup on both drivers. Write **both** `internal/store/migrations/sqlite/` and `internal/store/migrations/postgres/` variants.
- Store tests run under `forEachDriver` so sqlite and Postgres both execute.
- New routes MUST be registered with `s.route(pattern, handler)` — never `s.mux.HandleFunc`; `Handler()` builds the mux lazily under a `sync.Once` and a direct registration panics on a nil mux.
- Any new route MUST be documented in `internal/api/openapi.yaml` **in the same commit**, or `openapi_test.go` fails. `TestEveryRouteEnforcesAuth` will also require it to be behind `requireAuth`, or listed in that test's exemption allowlist with a reason.
- Gates, verified against the **committed** tree with `git status --porcelain` empty: `go test -race ./...`, `~/go/bin/golangci-lint run ./...`, `gofmt -l internal cmd`.
- Every fix's test must be shown failing with the fix removed.
- **No web UI in this milestone.** It lands with D3, where transfers give a key something to point at, and needs a design artboard first.

## File Structure

- `internal/store/migrations/{sqlite,postgres}/0008_tsig_keys.sql` — the table
- `internal/store/tsigkeys.go` — `tsigKeyStore`, CRUD in the `tokenStore` shape
- `internal/store/types.go` (or wherever `AuthToken` lives) — the `TSIGKey` struct
- `internal/api/tsigkeys_handlers.go` — REST CRUD
- `internal/dnssrv/tsig.go` — the `dns.TsigProvider` implementation and its wiring
- `docs/api.md` — the endpoints, and the secret-at-rest note

---

### Task 1: The table and the store

**Files:**
- Create: `internal/store/migrations/sqlite/0008_tsig_keys.sql`, `internal/store/migrations/postgres/0008_tsig_keys.sql`
- Create: `internal/store/tsigkeys.go`
- Modify: wherever the store interfaces are declared, to expose `TSIGKeys()`
- Test: `internal/store/tsigkeys_test.go`

**Interfaces:**
- Produces: `store.TSIGKey{ID int64; Name, Algorithm, Secret string; CreatedAt int64}` and a `TSIGKeyStore` interface with `List`, `Get`, `ByName`, `Create`, `Update`, `Delete`.

- [ ] **Step 1: Write the failing test**

```go
func TestTSIGKeyCRUD(t *testing.T) {
	forEachDriver(t, func(t *testing.T, st store.Store) {
		ctx := context.Background()
		id, err := st.TSIGKeys().Create(ctx, store.TSIGKey{
			Name: "xfer.e412.in.", Algorithm: "hmac-sha256.", Secret: "c2VjcmV0LXNlY3JldC1zZWNyZXQ=",
		})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		got, ok, err := st.TSIGKeys().ByName(ctx, "xfer.e412.in.")
		if err != nil || !ok {
			t.Fatalf("byName: ok=%v err=%v", ok, err)
		}
		if got.Secret != "c2VjcmV0LXNlY3JldC1zZWNyZXQ=" || got.Algorithm != "hmac-sha256." {
			t.Fatalf("round trip lost fields: %+v", got)
		}
		// The name is the lookup key on every signed message, so it must be unique.
		if _, err := st.TSIGKeys().Create(ctx, store.TSIGKey{
			Name: "xfer.e412.in.", Algorithm: "hmac-sha256.", Secret: "b3RoZXI=",
		}); err == nil {
			t.Fatal("a duplicate key name was accepted; the provider would have to guess which one signs")
		}
		if err := st.TSIGKeys().Delete(ctx, id); err != nil {
			t.Fatalf("delete: %v", err)
		}
		if _, ok, _ := st.TSIGKeys().ByName(ctx, "xfer.e412.in."); ok {
			t.Fatal("key still resolvable after delete")
		}
	})
}
```

- [ ] **Step 2: Run it and watch it fail**

Run: `go test ./internal/store/ -run TestTSIGKeyCRUD -v`
Expected: FAIL — `st.TSIGKeys` undefined.

- [ ] **Step 3: Write the migration**

sqlite (`0008_tsig_keys.sql`):

```sql
-- +goose Up
-- A TSIG key authenticates a zone transfer (RFC 8945). Unlike an API token,
-- which is stored as a sha256 hash because it only ever needs comparing, a
-- TSIG secret must be recoverable: the server signs with it. That follows the
-- TOTP precedent in users, not the auth_tokens one -- and it means this table
-- is credential material in the clear.
CREATE TABLE tsig_keys (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  -- A domain name, canonical form (lowercase, trailing dot): it arrives as
  -- the owner name of the TSIG RR on a signed message, and that is how the
  -- provider looks it up.
  name TEXT NOT NULL UNIQUE,
  -- 'hmac-sha256.' etc -- miekg's own constants, trailing dot included.
  algorithm TEXT NOT NULL,
  -- base64, as it is written in every other DNS server's config, so a key can
  -- be pasted between them.
  secret TEXT NOT NULL,
  created_at INTEGER NOT NULL DEFAULT 0
);
```

Postgres: same, with `BIGSERIAL PRIMARY KEY` and `BIGINT` for `created_at`, matching `0004_zones.sql`.

- [ ] **Step 4: Write the store**

Follow `internal/store/tokenstore.go` exactly — `t.s.insert(...)`, `t.s.q(...)` for placeholder rewriting, `sql.ErrNoRows` → `(zero, false, nil)`, and a non-nil slice from `List` so JSON marshals to `[]` not `null`.

- [ ] **Step 5: Run the test**

Run: `go test -race ./internal/store/ -run TestTSIGKeyCRUD -v`
Expected: PASS on both drivers.

- [ ] **Step 6: Commit**

```bash
git add internal/store
git commit -m "feat(store): store TSIG keys"
```

---

### Task 2: The REST endpoints

**Files:**
- Create: `internal/api/tsigkeys_handlers.go`
- Modify: `internal/api/server.go` (route registration), `internal/api/openapi.yaml`
- Test: `internal/api/tsigkeys_handlers_test.go`

**Interfaces:**
- Consumes: `store.TSIGKeyStore` from Task 1.
- Produces: `GET|POST /api/v1/tsig-keys`, `GET|PUT|DELETE /api/v1/tsig-keys/{id}`.

**Validation, each with a test:**

1. `name` must be a valid domain name — it is one (RFC 8945 §4.2), and it is normalised to canonical form (`dns.CanonicalName`) on write so lookup at verify time cannot miss on case or a missing dot.
2. `algorithm` must be one miekg supports: `hmac-sha1.`, `hmac-sha224.`, `hmac-sha256.`, `hmac-sha384.`, `hmac-sha512.` (`tsig.go:18`). MD5 was removed from the library and must be rejected, not silently accepted.
3. `secret` must be valid base64, because that is what the provider decodes at sign time — an invalid secret must fail at write, not on the first transfer.

**The secret is returned on read.** Unlike an API token, which is shown once because only its holder needs it, a TSIG secret has to be configured identically on the *peer*. Hiding it would make the feature unusable without direct DB access. Say so in the OpenAPI description rather than leaving it as an apparent oversight.

- [ ] **Step 1: Write the failing tests**

```go
func TestTSIGKeyRejectsAnUnsupportedAlgorithm(t *testing.T) {
	srv := newTestServer(t)
	rec := srv.do(t, "POST", "/api/v1/tsig-keys",
		`{"name":"xfer.e412.in.","algorithm":"hmac-md5.sig-alg.reg.int.","secret":"c2VjcmV0"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body = %s; want 400 -- MD5 was removed from the library and cannot sign", rec.Code, rec.Body)
	}
}

func TestTSIGKeyRejectsANonBase64Secret(t *testing.T) {
	srv := newTestServer(t)
	rec := srv.do(t, "POST", "/api/v1/tsig-keys",
		`{"name":"xfer.e412.in.","algorithm":"hmac-sha256.","secret":"not base64!!"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400 -- an unusable secret must fail at write, not at the first transfer", rec.Code)
	}
}

// The name is how a signed message finds its key, so "XFER.E412.IN" and
// "xfer.e412.in." must be the same key rather than two.
func TestTSIGKeyNameIsCanonicalised(t *testing.T) {
	srv := newTestServer(t)
	if rec := srv.do(t, "POST", "/api/v1/tsig-keys",
		`{"name":"XFER.E412.IN","algorithm":"hmac-sha256.","secret":"c2VjcmV0"}`); rec.Code != http.StatusCreated {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body)
	}
	rec := srv.do(t, "GET", "/api/v1/tsig-keys", "")
	if !strings.Contains(rec.Body.String(), `"xfer.e412.in."`) {
		t.Fatalf("name not canonicalised: %s", rec.Body)
	}
}
```

- [ ] **Step 2: Run them and watch them fail**

Run: `go test ./internal/api/ -run TestTSIGKey -v`
Expected: FAIL — 404, no such route.

- [ ] **Step 3: Implement the handlers**

Mirror `internal/api/tokens_handlers.go` for shape: `pathID`, `decode`, `storeErr`, the flat `{"error": ...}` envelope, `requireAuth`. Register with `s.route`.

- [ ] **Step 4: Document in openapi.yaml — same commit**

All five operations, with `401` and (for the mutating ones) `403` via the existing `$ref`s, plus `400` and `404`. `TestOpenAPIDocumentsAuthStatuses` enforces this.

- [ ] **Step 5: Run the suite**

Run: `go test -race ./internal/api/`
Expected: PASS, including `TestOpenAPIServedAndCoversRoutes` and `TestEveryRouteEnforcesAuth`.

- [ ] **Step 6: Commit**

```bash
git add internal/api
git commit -m "feat(api): manage TSIG keys"
```

---

### Task 3: The live provider

**Files:**
- Create: `internal/dnssrv/tsig.go`
- Modify: `internal/dnssrv/server.go` (attach the provider to both servers), `internal/app/app.go` (construct it)
- Test: `internal/dnssrv/tsig_test.go`

**Interfaces:**
- Consumes: `store.TSIGKeyStore`.
- Produces: a `dns.TsigProvider` that resolves keys per message, and a helper the D3 transfer path will use to require a valid signature.

**Two facts this task exists because of:**

- `srv.tsigProvider()` is read **per connection** (`server.go:260`), so a provider that consults the store on each call picks up a new key with no restart. The static `TsigSecret` map cannot.
- Verification is **automatic but non-enforcing**: miekg sets a status (`server.go:673`) and carries on. An unsigned message reaches the handler unless the handler checks `w.TsigStatus()`.

**Security note for the implementer:** `tsigHMACProvider` (`tsig.go:35-76`) is unexported, so `Generate`/`Verify` are ours to write. **Read that implementation and mirror its semantics exactly** — algorithm→hash mapping, base64 secret decoding, MAC handling, and `hmac.Equal` for comparison. A deviation here is a security bug, not a style difference. Do not hand-roll a comparison with `==` or `bytes.Equal`.

- [ ] **Step 1: Write the failing test**

```go
// A signed query verifies and the reply is signed back. RFC 8945 permits TSIG
// on any message, so this is exercisable before transfers exist.
func TestSignedQueryVerifies(t *testing.T) {
	// ... start a dnssrv with a store holding key "xfer.e412.in." ...
	c := new(dns.Client)
	c.TsigProvider = <the same secret>
	m := new(dns.Msg).SetQuestion("bifrost.e412.in.", dns.TypeA)
	m.SetTsig("xfer.e412.in.", dns.HmacSHA256, 300, time.Now().Unix())
	reply, _, err := c.Exchange(m, addr)
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if reply.IsTsig() == nil {
		t.Fatal("reply carried no TSIG; a signed request must get a signed answer")
	}
}

// The half that matters: a wrong secret must not verify.
func TestWrongSecretFailsVerification(t *testing.T) {
	// ... sign with a different secret under the same key name ...
	// assert the server's TsigStatus() is non-nil, via the helper this task adds
}

// An unknown key name is not an error the server can sign its way out of.
func TestUnknownKeyNameFailsVerification(t *testing.T) { /* ... */ }
```

- [ ] **Step 2: Run and watch them fail**

Run: `go test ./internal/dnssrv/ -run Tsig -v`
Expected: FAIL — no provider is attached, so nothing verifies.

- [ ] **Step 3: Implement**

The provider looks the key up by `dns.CanonicalName(t.Hdr.Name)`, returns an error for an unknown name, and otherwise computes the MAC per the mirrored implementation. Attach it to both `s.udp` and `s.tcp` at `server.go:53-54`.

- [ ] **Step 4: Run**

Run: `go test -race ./internal/dnssrv/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/dnssrv internal/app
git commit -m "feat(dns): verify TSIG-signed messages against stored keys"
```

---

### Task 4: Docs

**Files:**
- Modify: `docs/api.md`

- [ ] **Step 1: Write it**

The five endpoints and their fields. Two things stated plainly rather than implied:

- the secret is returned on read, and why — the peer needs the same value
- **the database stores TSIG secrets in the clear**, so the DB file is credential material. Nothing else in dnsaur is encrypted at rest either, but nothing else has to be recoverable to sign with, which is why it is worth saying here.

Match the surrounding voice: plain and brief, state the fact and stop.

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
git commit -m "docs: TSIG keys"
```

---

## Self-Review

**Spec coverage (§9.3):** keys as a first-class resource → Tasks 1-2. Dynamic provider so a key needs no restart → Task 3. Explicit `TsigStatus()` enforcement → Task 3's helper, consumed by D3. Plaintext-at-rest stated → Task 1's migration comment and Task 4.

**Deliberately out of scope:** the web UI (D3, needs an artboard); the transfer paths that will consume the enforcement helper (D2, D3).

**Type consistency:** `store.TSIGKey` defined in Task 1 and used with those field names in Tasks 2-3. `TSIGKeyStore` methods named once and referenced unchanged.

**Known risk:** Task 3 is the only task writing cryptographic code. Its brief must carry the "mirror `tsig.go:35-76` exactly" instruction, and its review must verify the comparison is constant-time rather than take it on trust.
