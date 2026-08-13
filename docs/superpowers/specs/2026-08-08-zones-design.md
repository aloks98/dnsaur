# dnsaur Zones: authoritative DNS design

**Status:** design, approved for implementation 2026-08-08
**Replaces:** the flat `local_records` model (Pi-hole-style overrides)
**Scope decision:** full Technitium parity — primary + secondary zones, transfers,
DNSSEC signing, zone file import/export. Five milestones (A–E), each shippable.

---

## 1. Why this is not a rename

Today `local_records` is a flat `name/type/value/ttl` table. A lookup that misses
falls through to the forwarder. That is the Pi-hole model: **overrides on a
forwarder**, not authority.

The consequence is concrete and currently affects this deployment. `e412.in` is a
publicly registered domain that also carries internal names. With overrides, a
query for a name under `e412.in` that isn't in the table is **forwarded to a
public resolver** — the internal name leaks, and a public record can shadow an
internal one that hasn't been defined yet.

A zone is a claim of authority over a suffix, and it changes what a *miss* means:

| Case | Override model (today) | Zone model |
|---|---|---|
| Name exists, type matches | answer | answer, **AA set** |
| Name exists, wrong type | fall through to upstream | **NOERROR + SOA in AUTHORITY** (NODATA) |
| Name absent, inside a zone we hold | fall through to upstream | **NXDOMAIN + SOA in AUTHORITY** |
| Name outside every zone | fall through | fall through (unchanged) |

The SOA in the AUTHORITY section is not decoration: its MINIMUM field is the TTL
resolvers use to negatively cache that NXDOMAIN. Without it, every miss re-queries.

Split-horizon only works once this is true.

---

## 2. Storage model

### Two tables, replacing `local_records`

```sql
CREATE TABLE zones (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  name          TEXT NOT NULL UNIQUE,   -- apex, lowercase, no trailing dot ('e412.in')
  type          TEXT NOT NULL,          -- primary | secondary | stub | forwarder | internal
  enabled       INTEGER NOT NULL DEFAULT 1,
  -- SOA lives on the zone, not in records: serial needs managed increments
  soa_ns        TEXT NOT NULL,
  soa_mbox      TEXT NOT NULL,
  soa_serial    INTEGER NOT NULL DEFAULT 1,
  soa_refresh   INTEGER NOT NULL DEFAULT 900,
  soa_retry     INTEGER NOT NULL DEFAULT 300,
  soa_expire    INTEGER NOT NULL DEFAULT 604800,
  soa_minimum   INTEGER NOT NULL DEFAULT 900,
  soa_ttl       INTEGER NOT NULL DEFAULT 900,  -- added 0006: RFC 2308 5 needs
                                               -- the SOA record's own TTL as a
                                               -- value independent of MINIMUM
  -- secondary/stub/forwarder only
  primaries     TEXT NOT NULL DEFAULT '',  -- comma-separated host:port
  tsig_key_id   INTEGER NOT NULL DEFAULT 0,
  expires_at    INTEGER NOT NULL DEFAULT 0, -- secondary: SOA expire deadline
  refreshed_at  INTEGER NOT NULL DEFAULT 0,
  created_at    INTEGER NOT NULL,
  modified_at   INTEGER NOT NULL
);

CREATE TABLE zone_records (
  id       INTEGER PRIMARY KEY AUTOINCREMENT,
  zone_id  INTEGER NOT NULL REFERENCES zones(id) ON DELETE CASCADE,
  name     TEXT NOT NULL,   -- RELATIVE to apex: '@', 'bifrost', '*', '*.nexus'
  type     TEXT NOT NULL,   -- A | AAAA | CNAME | TXT | MX | SRV | NS | CAA | PTR | ...
  ttl      INTEGER NOT NULL DEFAULT 3600,
  rdata    TEXT NOT NULL,   -- PRESENTATION FORMAT, rdata portion only
  enabled  INTEGER NOT NULL DEFAULT 1,
  comment  TEXT NOT NULL DEFAULT ''
);
CREATE INDEX idx_zone_records_zone ON zone_records(zone_id, name, type);
```

### The `rdata` decision

`rdata` holds the **presentation-format rdata portion** — `192.168.150.28` for A,
`10 mail.example.com.` for MX, `0 issue "letsencrypt.org"` for CAA. An RR is built
by handing miekg/dns a reassembled master-file line:

```go
rr, err := dns.NewRR(fmt.Sprintf("%s %d IN %s %s", fqdn, ttl, rtype, rdata))
```

This replaces the current per-type `switch` in `records.toRR` (which supports four
types and needs a new case per type) with one code path. Three payoffs:

1. **New record types cost nothing** — MX, SRV, CAA, NAPTR, SSHFP all work the day
   the row is inserted, because miekg/dns already parses them.
2. **Zone file import/export becomes near-trivial** (Milestone C) — the stored form
   *is* master-file format, so export is string assembly and import is
   `dns.ZoneParser`.
3. **Validation is the same code as parsing.** The API validates by attempting
   `dns.NewRR` and returning its error, so accepted-but-unservable rows can't exist.

Cost: no typed columns, so "find every record pointing at 192.168.1.5" is a string
match. Acceptable at homelab scale, and the alternative (per-type columns or a JSON
blob) buys nothing that a zone-scoped index doesn't.

### Migration from `local_records`

Group existing rows by parent suffix, create a primary zone per distinct group,
rewrite names as relative, generate a default SOA (`soa_ns` = the zone apex,
`soa_mbox` = `hostadmin.<zone>`, serial 1). A wildcard `*.nexus.e412.in` groups
under `e412.in` as name `*.nexus`.

**`local_records` is NOT dropped.** An earlier draft of this section said the
table is dropped and the copy verified by row count; neither is true, and the
correction is deliberate rather than an omission. The conversion *alters* data —
it drops apex CNAMEs, normalises mismatched RRSet TTLs down to the lowest, and
clamps oversized TTLs — so the original rows are the only record of what the
user actually wrote. Keeping the table costs a few kilobytes and is the one
thing that makes a bad conversion recoverable.

Consequence to tidy up: `store.RecordStore` and `sqlStore.Records()` are now
dead. The migration reads `local_records` through raw SQL on the goose
transaction, not through the store API, so the interface has no callers.

---

## 3. Answering: the zone cut

`internal/records` becomes `internal/zones`, and its middleware changes from
"look up a name" to "find the authoritative zone, then answer for it".

```
findZone(qname):  walk labels from the right, deepest enabled zone wins
                  e412.in beats in (if both exist); no match → next handler
```

Once a zone is found, **the query never leaves** (except forwarder/stub types):

```
lookup(zone, relname, qtype):
  exact name + type          → ANSWER, aa=1
  exact name, other types    → NODATA:   NOERROR, empty ANSWER, SOA in AUTHORITY
  CNAME at name              → follow (in-zone: append and continue;
                               out-of-zone: append CNAME, resolve rest via pipeline)
  wildcard                   → `*.<closest encloser>` ONLY, per RFC 4592 3.3.1 —
                               NOT a walk upward looking for any wildcard
  nothing, name not in zone  → NXDOMAIN, SOA in AUTHORITY
```

Three rules that are easy to get wrong and get their own tests:

- **Wildcards do not match names that exist.** If `a.e412.in` exists with only a
  TXT, then `a.e412.in A` is NODATA — `*.e412.in` must not answer it (RFC 4592 §2.2).
- **CNAME cannot coexist** with other types at the same name (RFC 1034 §3.6.2).
  Enforced at write time, returning 409.
- **Delegation:** an `NS` record below the apex is a zone cut. We return a referral
  (NS in AUTHORITY, glue in ADDITIONAL, aa=0) rather than answering.

The `DecisionLocal` value in `internal/dnssrv/pipeline.go` becomes
`DecisionAuthoritative`, and `DecisionAllowed` — currently declared but never
emitted (dead since it was written) — is removed.

---

## 4. Milestones

Each is a shippable PR series. Later ones assume earlier ones.

### A — Zone model + authoritative forward zones
Schema, migration, `internal/zones` with the zone cut and NODATA/NXDOMAIN/wildcard
rules, record types A/AAAA/CNAME/TXT/MX/SRV/NS/CAA, `/api/v1/zones` +
`/api/v1/zones/{id}/records`, and the UI: **Local DNS → Zones**, a zone list
(name, type, status, serial, modified) and a zone detail page. Nav, docs and
`openapi.yaml` updated.

### B — Reverse zones, PTR, RFC 6303 internal zones
`in-addr.arpa` / `ip6.arpa` zone handling with address↔name conversion, PTR
records, optional auto-PTR maintained from A/AAAA writes, and the built-in
`internal` zones (`localhost`, `127.in-addr.arpa`, `0.in-addr.arpa`,
`255.in-addr.arpa`, `1.0.0…0.ip6.arpa`) seeded at migration so junk queries stop
reaching the root servers. Zone type `internal` is read-only in the UI.

### C — Zone file import/export
BIND master-file format both directions via `dns.ZoneParser` and string assembly.
Import is transactional with a dry-run diff. Export is a plain download.

### D — Secondary zones and transfers
Outbound: AXFR client honouring SOA refresh/retry/expire, `expires_at`
enforcement. Inbound: serving AXFR to secondaries behind an allow-transfer
ACL. NOTIFY in both directions. TSIG keys as a first-class resource, since
transfers without them are unauthenticated.

**Split into D1-D6 and IXFR deferred — see §9**, written once the work was
costed against the code. IXFR is an optimisation over a complete AXFR
implementation, and RFC 1995 §2 permits answering it with a full AXFR.

### E — DNSSEC
Online signing: KSK/ZSK generation and storage, DNSKEY/RRSIG/DS, NSEC or NSEC3
for authenticated denial, scheduled re-signing and key rollover. Largest and
last; parity with Technitium's DNSSEC menu is the bar.

**Deliberately excluded:** per-zone permissions (single-admin product) and
page-number pagination (a homelab has single-digit zone counts; the existing
search-and-scroll pattern covers it).

### Relationship to conditional forwarding

`upstream.Config.Conditional` already routes a suffix to specific upstreams. In
Milestone D that becomes zone type `forwarder`, and the config key is migrated
into a zone row so there is one place a suffix is claimed rather than two that can
disagree.

---

## 5. Test posture

Unchanged from the rest of the repo: every behavioural claim above gets a test,
and every fix gets a test shown to fail without it. Specifically pinned:

- NODATA vs NXDOMAIN vs referral, each with the SOA/NS section asserted
- wildcard non-match against an existing name (RFC 4592 §2.2)
- CNAME-with-siblings rejected at write
- deepest-zone-wins when nested zones both match
- migration: a `local_records` fixture round-trips to zones with equal answers

---

## 6. RFC conformance

Authoritative DNS is a protocol other people's resolvers depend on. Getting it
approximately right produces failures that look like someone else's bug — a
name that resolves from one resolver and not another, an NXDOMAIN cached for a
week, an RRSet that half the internet sees with the wrong TTL. Every rule below
is testable, and each is pinned by a named test.

### Binding on Milestone A

| RFC | Rule | Where enforced |
|---|---|---|
| **1034 §3.6.2** | CNAME must not coexist with any other type at a name | write-time, 409 |
| **1912 §2.4** | **CNAME must not exist at the zone apex** — SOA and NS live there | write-time, 409 |
| **1035 §2.3.4** | Label ≤ 63 octets, name ≤ 255 octets | `dns.IsDomainName` at write |
| **2181 §5.2** | **Every RR in an RRSet (same name+type) must carry the same TTL** | write-time, 409 |
| **2181 §8** | **TTL is 31-bit: the top bit must be 0, so max 2147483647** | write-time, 400 |
| **2181 §10.1** | A zone's apex must have NS records | default NS generated at zone create |
| **2308 §3** | NXDOMAIN and NODATA carry the zone's SOA in AUTHORITY | answering |
| **2308 §5** | **Negative TTL is `min(SOA.MINIMUM, TTL of the SOA record)`** | answering |
| **4592 §2.1.1** | **`*` is a wildcard only as the leftmost label** — `a.*.x` is a literal name | write + answering |
| **4592 §2.2** | A wildcard must not match a name that exists with other types | answering |
| **8020** | NXDOMAIN means the name *and everything below it* does not exist | answering |

The five in bold were absent from this plan before the RFCs were checked
against it. Two of them are silent-corruption bugs rather than missing
features: per-record TTLs in one RRSet (§5.2) make a zone that answers
differently depending on which row is read first, and an unclamped TTL (§8)
with the high bit set is interpreted by resolvers as **zero**, so a record
meant to be cached for a day is instead never cached at all.

`@ CNAME` deserves its own note: the write-time sibling check does not catch it,
because our SOA lives on the `zones` row rather than in `zone_records` — so the
apex looks empty and the CNAME appears to conflict with nothing. It needs an
explicit rule.

### Binding on later milestones

- **B:** 6303 (locally-served zones and which ones), 1035 §3.5 / 3596 §2.5
  (in-addr.arpa and ip6.arpa nibble form), 1034 (PTR)
- **C:** 1035 §5 (master file format — `$ORIGIN`, `$TTL`, `@`, parentheses,
  escaping)
- **D:** 5936 (AXFR), 1995 (IXFR), 1996 (NOTIFY), 8945 (TSIG)
- **E:** 4033/4034/4035 (DNSSEC), 5155 (NSEC3), 6781 (operational practice),
  8624 (algorithm requirements)

### Deliberately not implemented in A, and why

- **8482** (minimal ANY): we do not special-case `ANY`; it falls out as NODATA
  or the matching set. Revisit if it becomes an amplification vector.
- **6891** (EDNS0): handled by miekg/dns at the server layer, not here.
- **9460** (SVCB/HTTPS): storable as rdata today because `dns.NewRR` parses
  them; no special serving logic.

---

## 7. Milestone B: reverse zones, PTR, and the built-ins

Decided 2026-08-09.

### Which RFC 6303 zones ship

**Only the five that can never hold a useful record**: `localhost`,
`127.in-addr.arpa`, `0.in-addr.arpa`, `255.in-addr.arpa`, and
`1.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.ip6.arpa`
(`::1`). Type `internal`, read-only, seeded at migration.

RFC 6303 also lists the RFC 1918 reverse ranges — `10.in-addr.arpa`,
`168.192.in-addr.arpa`, `16`–`31.172.in-addr.arpa` — and dnsaur deliberately
does **not** seed them. An empty authoritative zone NXDOMAINs everything under
it, so seeding `168.192.in-addr.arpa` would make PTR records for the user's own
LAN impossible: the very thing a homelab wants reverse DNS for. Those stay
unclaimed so the user can create them as `primary` zones and put real PTRs in.
The cost is that reverse lookups for un-recorded private addresses still leak
upstream; that is the right trade for a server whose LAN is the point.

### Auto-PTR

A/AAAA writes maintain the matching PTR **server-side, inside the same
request** — no second call from any client.

| Event | Behaviour |
|---|---|
| A/AAAA created, matching reverse zone exists | write the PTR |
| no matching reverse zone | do nothing — never auto-create a zone |
| a PTR already exists for that address | leave it, WARN. First wins. |
| A/AAAA updated | move the PTR, only if it still points at this name |
| A/AAAA deleted | remove the PTR, only if it still points at this name |

The "still points at this name" guard is what stops an automatic write
clobbering a PTR the user set by hand.

**Two names, one address** has no correct answer — a PTR is effectively
one-per-address, so `bifrost` and `nas` both at `192.168.150.10` means one owns
the reverse. First-write-wins with a WARN is the chosen rule; it is stated here
because it is a decision, not a derivation.

### Internal zones must be read-only at the API, not just in the UI

Carried from Milestone A's review as a deferred finding: `PATCH` and `DELETE`
on a zone, and every write under `/zones/{id}/records`, must reject
`type == "internal"` with **409**. Today only the UI hides those controls,
which was harmless while nothing created internal zones and stops being
harmless the moment this milestone seeds five.

### RFC conformance added here

| RFC | Rule |
|---|---|
| **6303** | which zones are served locally, and why the RFC 1918 ones are excluded |
| **1035 §3.5** | `in-addr.arpa` is the four octets reversed |
| **3596 §2.5** | `ip6.arpa` is 32 nibbles, reversed, dot-separated |
| **1034** | PTR names a host; the rdata is a domain name, not an address |
| **2317** | classless delegation of `in-addr.arpa` — noted, NOT implemented; a `/24`-aligned homelab does not need it |

---

## 8. Milestone C: zone file import and export

Decided 2026-08-10.

Milestone A stores `rdata` in DNS presentation format precisely so this
milestone would be cheap: **the stored form already is master-file format**.
Export is string assembly; import is `dns.ZoneParser`. Neither needs a second
serializer that could disagree with the resolver.

### Export

A zone renders as a BIND master file: `$ORIGIN`, `$TTL`, the SOA reconstructed
from the `zones` row, then every record in `zone_records`. Names stay relative
— that is what they already are — so the file is portable to any other server.

Disabled records are **omitted**. A zone file has no concept of a disabled
record, and emitting one would silently enable it on whatever imports the file.

Built-in (`internal`) zones export like any other. Reading them is allowed;
only writing is not.

### Import: the file is the zone

**Replace, not merge.** A zone file is the zone, not a patch. Anything in the
zone and not in the file is deleted. Every other DNS server treats a zone file
this way, and merge semantics produce a result that is neither the file nor
what was there before — with the specific trap that deleting a record from the
file and re-importing does nothing.

Because that is destructive, import is **two steps**: a dry run returning the
adds, changes and deletes, then a commit. The diff is what makes the
destruction visible before it happens.

### Import applies the same validation as a hand write

A zone file can legitimately contain what `buildZoneRecord` rejects — a CNAME
beside another type, mismatched TTLs within one RRSet, a TTL above 2147483647,
a CNAME at the apex.

**The whole file is rejected, and the error names every offending line**, not
just the first. Two reasons. Partial import leaves a zone matching neither the
file nor any intent, and a report of what was skipped is easy to miss. And
silently normalising — as the `local_records` migration did — edits the user's
data without asking and means the zone will not round-trip back to the file
they supplied.

Import routes through `buildZoneRecord`, the same validator behind
`POST /records`. Milestone B's recurring defect was an automatic path
bypassing a rule the human path enforced; it happened three times. Import is a
bulk write path and gets the same treatment, not its own copy of the rules.

### Import does NOT trigger auto-PTR

Import writes exactly what the file contains and nothing else. A 200-record
import stays one predictable transaction, and a forward-zone import never
silently rewrites a reverse zone the user did not name. PTRs arrive by
importing the reverse zone's own file.

This is a deliberate exception to "A/AAAA writes maintain the matching PTR"
(§7) and is the only one.

### Built-in zones cannot be imported into

`type == "internal"` rejects import with the same 409 every other write gets.

### RFC conformance added here

| RFC | Rule |
|---|---|
| **1035 §5** | master file format: `$ORIGIN`, `$TTL`, `@`, parentheses for multi-line records, `;` comments, escaping |
| **1034 §3.6.1** | the file's SOA is the zone's SOA |
| **2308 §4** | `$TTL` is the default for records that omit one |

**Serial handling on import.** The file's SOA carries a serial. Taking it
verbatim can move the zone's serial *backwards*, which breaks any secondary
that has already seen the higher value (§4 D). Import takes the file's SOA
timers, NS and mbox, but the serial becomes `max(file, current) + 1` — never
lower than what has already been served.

---

## 9. Milestone D: secondary zones and transfers

Drafted 2026-08-11, from §4 D. Every claim below was established against the
code before it was written; the constraining findings are named inline.

### 9.1 Why this is not another middleware

Milestones A–C all hung off the query chain. This one cannot.

`dnssrv.Handler` returns exactly one `*Response` (`pipeline.go:58`) and `serve`
writes exactly one message (`server.go:121`). The real `dns.ResponseWriter`
exists only inside `serve` (`server.go:85`) and is never passed on. An AXFR
answer is a *sequence* of messages on one connection, so it cannot be expressed
as a `*Response` at all.

**So transfers branch at `dnssrv/server.go:85`, ahead of the chain**, on
qtype AXFR/IXFR and on opcode NOTIFY. This is an intercept, not a link. It is
also correct on the merits: a transfer must not pass through qlog's per-query
accounting, the filter, the cache, or the upstream forwarder — none of which
have any meaning for it.

Two consequences the intercept must handle, both measured:

- the 5s handler context (`server.go:86`) would abort a large transfer; the
  transfer path needs its own deadline
- `server.go:104` force-adds OPT to any OPT-less reply to an EDNS query, which
  would stamp OPT onto every AXFR envelope

### 9.2 Storage: three tables that do not exist

`zones` already carries `primaries`, `tsig_key_id`, `expires_at` and
`refreshed_at`, all unused and all without defined semantics. This milestone
gives them meaning and adds:

**`tsig_keys`** — name, algorithm, secret. Unlike API tokens (sha256 hash,
`auth/token.go:19`) a TSIG key must be **recoverable to sign with**, so it
follows the TOTP precedent (`store/userstore.go:112`): stored as retrievable
plaintext. Nothing in this codebase is encrypted at rest today, and inventing
key management here would be a larger and worse-tested change than the feature.
Stated plainly rather than implied: **the DB holds signing secrets in the
clear, so the DB file is now credential material.**

**`zone_acl`** — which peers may transfer which zone. No ACL concept exists
anywhere today. Default deny.

**`zone_journal`** — IXFR deltas: per zone, per serial, the RRs added and
removed. Greenfield; miekg gives no journal, no delta computation and no serial
history, and `BumpSerial` (`store/zones.go:183`) increments while recording
nothing. See 9.6 for whether this is in scope at all.

**`primaries` format** must be defined, having shipped with none: a
comma-separated list of `host[:port]`, port defaulting to 53, resolved at
transfer time rather than at write.

### 9.3 TSIG (RFC 8945)

Algorithms are library-complete (hmac-sha1 through hmac-sha512, `tsig.go:18`;
MD5 removed). Three things are ours:

- **Verification is automatic but non-enforcing.** miekg sets a status
  (`server.go:673`); an unsigned request sails through unless the handler
  checks `w.TsigStatus()`. Every transfer path checks it explicitly.
- **Hot reload.** `srv.tsigProvider()` is read per connection
  (`server.go:260`), so a dynamic `TsigProvider` backed by the store is what
  lets a key be added without a restart. A static `TsigSecret` map would not.
- Keys are a first-class resource with their own CRUD, because a transfer
  without one is unauthenticated.

### 9.4 Outbound: dnsaur as secondary

`dns.Transfer.In` does the framing. The work is scheduling and installation.

**Installation is already solved and must be reused**: `ReplaceRecords`
(`store/zones.go:248`) applies deletes, updates, adds and the zone row — serial
included — in one transaction. A received zone installs as one atomic replace,
exactly like an import. Received RRs convert to relative-name rows through the
**same validation the API enforces** (`buildZoneRecord`), for the reason
Milestone B and C both had to learn: an automatic path that skips a rule the
human path enforces is this project's most repeated defect.

**Scheduling** honours the SOA: refresh, retry on failure, and `expires_at` as
a hard stop — a secondary past expiry must stop answering rather than serve
data it can no longer confirm.

**Per-zone reload is new work.** `Resolver.Reload` (`resolver.go:34`) rebuilds
every zone from the whole store and swaps one atomic `Index`. Transfers call it
far more often than a human writes a record, so this milestone adds a per-zone
path or accepts a measured cost — decide with a benchmark, not by assertion.

### 9.5 Inbound: dnsaur as primary

The hardest item, and entirely greenfield.

- The intercept of 9.1 is what gives `Transfer.Out` the raw writer.
- **Envelope batching is ours.** `xfr.go:229` reads "assume it fits
  TODO(miek): fix" — the library does not split a zone into messages that fit.
- The allow-transfer ACL gates it, default deny, TSIG-or-address.

### 9.6 IXFR, and whether it belongs here

§4 D commits to IXFR. Having now costed it, it is the largest single piece of
this milestone — a journal table, a retention policy, delta computation, and
RFC 1982 serial arithmetic — and it is an **optimisation**: AXFR alone is a
complete, correct transfer mechanism. RFC 1995 §2 permits a primary to answer
IXFR with a full AXFR, which is a conforming implementation and what dnsaur
would do until the journal exists.

For homelab zones of tens to hundreds of records the bandwidth saved is
negligible. The honest recommendation is **AXFR first, IXFR as its own
milestone**, and the spec should say so rather than carrying a commitment made
before the cost was known.

### 9.7 `forwarder` zone type

Shares nothing with transfers but the type enum, and is the most
groundwork-complete of the six items: `answer.go:35` already falls through for
`forwarder`/`stub`, and `upstream.Config.Conditional` works — nothing populates
it.

The catch: the fall-through hands the query to the **global** forwarder, which
`applySettings` rebuilds from settings alone (`app.go:215`). Per-zone upstreams
need a second input to forwarder construction. That is a change to the upstream
package, not the zones package, and it is why this belongs on its own rather
than inside a transfers milestone.

### 9.8 Sub-milestones

D is too large to land as one plan. Each of these ships something that works:

| | Scope | Depends on |
|---|---|---|
| **D1** | TSIG keys: table, CRUD, dynamic provider, and the `TsigStatus` helper transfers call | — |
| **D2** | Outbound AXFR: `primaries` format, scheduling, expiry, atomic install, per-zone reload, **and ownership of `zones.tsig_key_id`** — the column and the guard against deleting a key a zone still references. D2 declined the schema-level foreign key with reasoning recorded in migration 0009: the column is `NOT NULL DEFAULT 0` where 0 means "no key", a FK skips NULL rather than zero, and making it nullable is a whole-table rebuild on SQLite where `zones` is the parent of `zone_records … ON DELETE CASCADE`. The guard is a single-statement delete instead | D1 |
| **D3** | Inbound AXFR: the `dnssrv` intercept, ACL, envelope batching | D1 |
| **D4** | NOTIFY both directions | D2, D3 |
| **D5** | IXFR: journal, deltas, serial arithmetic | D2, D3 |
| **D6** | `forwarder` zone type and the `Conditional` migration | — |

D1 is the foundation both directions need. D6 is independent of all of it.

**Disabled is not expired, and that is deliberate (decided 2026-08-13).** A
secondary that cannot vouch for its data answers SERVFAIL rather than falling
through, because a public record would otherwise shadow the internal one. A
*disabled* zone does fall through — the resolver skips it, the query reaches the
forwarder, and the public answer wins. Disabling means dnsaur gives up the name,
so the internet's answer applies. It matches Technitium, and §1's scope line is
full Technitium parity. The consequence worth knowing: disabling a split-horizon
zone exposes its names to public answers rather than making them fail. The UI
says so rather than implying the zone goes quiet.

**Enforcement is deliberately not D1's.** D1 ships the mechanism — a provider
that verifies, and a helper that reports the result — but nothing in D1 rejects
an unsigned message, because nothing in D1 requires a signature. D2 and D3 are
where a transfer path calls it. A reader who assumes D1 made TSIG mandatory will
be wrong.

### 9.9 RFC conformance added here

| RFC | Rule |
|---|---|
| **5936** | AXFR is TCP-only; SOA first and last; the transfer is a sequence of messages |
| **1995** | IXFR falls back to a full AXFR when no delta is available (§2) |
| **1996** | NOTIFY is a hint, not an instruction — the secondary still checks the SOA before transferring |
| **8945** | TSIG: signed request, signed reply, time-window enforcement, and an unsigned request rejected rather than ignored |
| **1982** | Serial arithmetic is circular — comparison is not `<` |
| **1034 §4.3.5** | A secondary past its SOA expire must stop answering for the zone |
