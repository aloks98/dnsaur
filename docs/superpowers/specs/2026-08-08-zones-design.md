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
Milestone D that becomes zone type `forwarder`, so there is one place a suffix
is claimed.

**Corrected 2026-09-03.** This used to say the config key was "migrated into a
zone row rather than two that can disagree". There is no config key —
`Conditional` has never had a producer — so nothing is migrated and there were
never two places. See §9.11.1.

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

**D3 declined the `zone_acl` table (decided 2026-08-13).** What shipped is a
single `allow_transfer` column on `zones`, in the shape `primaries` already
has, parsed by `zones.ParseACL` into the same `[]ACLEntry` a table would have
produced. An entry carries no per-entry state worth a row — no timestamp, no
status, nothing that changes without the operator editing it — so a table
would have bought per-entry comments in exchange for a migration, four routes,
an OpenAPI section and a list editor, while the column rides `PATCH
/zones/{id}` and the field beside Primaries. If per-entry metadata ever earns
its keep, moving to a table is a data move rather than a redesign, because the
parsed form is already what the table would hold. See §9.5.4 for the format.

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

The hardest item, and entirely greenfield. Designed 2026-08-13. Every library
claim below was read out of `miekg/dns@v1.1.72` before it was written, and
every RFC sentence is quoted from the RFC rather than recalled.

A note on vocabulary, because the two halves of Milestone D invert it. §9.4's
"outbound" and this section's "inbound" name the direction the *zone data*
moves relative to dnsaur. The code says what it does instead: `Transferrer`
pulls a zone (D2), `TransferServer` serves one (D3).

#### 9.5.1 The intercept

§9.1 established that this cannot be a middleware. The branch goes in
`Server.serve` (`dnssrv/server.go:112`), taken on qtype AXFR or IXFR before
the `Request` is built, handing the raw `dns.ResponseWriter` to a handler
supplied at construction:

```go
// dnssrv
type Transfers interface {
	ServeTransfer(ctx context.Context, w dns.ResponseWriter, q *dns.Msg, key string, tsigErr error)
}

func WithTransfers(t Transfers) Option
```

The TSIG verdict is passed in rather than recomputed. `serve` already asks
`RequireTSIG` once (`dnssrv/tsig.go:199`), and a transfer must act on the same
answer the rest of the server would have acted on — two callers of the same
check are two chances for them to disagree. `internal/zones` implements the
interface, so `dnssrv` learns nothing about zones, exactly as `WithTSIGKeys`
taught it nothing about the store beyond one lookup.

With no handler attached the branch is not taken and an AXFR falls through the
pipeline as it does today. That is a test-only configuration — `app.go` always
attaches one — and leaving it alone keeps this change from altering behaviour
it is not about.

The three hazards the intercept exists to route around, all named in §9.1: the
5-second handler context (`dnssrv/server.go:113`) that would abort a large
transfer, the OPT force-add (`dnssrv/server.go:140`) that would stamp OPT onto
every envelope, and qlog, the filter, the cache and the forwarder, none of
which mean anything for a transfer.

#### 9.5.2 Where the zone comes from

The served snapshot, not the store: the handler reads the same
`atomic.Pointer[Index]` the query path reads (`zones/resolver.go:118`).

Three consequences, all of them wanted. A transfer costs no database read, so
a peer cannot drive store load. What a secondary receives is by construction
what a querier is being answered from — disabled records were already dropped
at snapshot build (`zones/zone.go:37`), and a disabled zone is already not
served. And a transfer that overlaps a `Reload` sees one consistent zone
rather than a mixture, which is what the atomic swap is for.

The cost is that a write becomes transferable only once `Reload` has run.
Every write path already calls it (`app.go`, `ReloadZones`), so it is the same
lag queries have, and a secondary is a cache with an SOA refresh timer rather
than a synchronous replica.

`Index.Find` walks suffixes for the closest enclosing zone
(`zones/zone.go:135`), which is the wrong lookup here: an AXFR names an apex
and nothing else, and a query for `sub.e412.in` must not transfer `e412.in`.
D3 adds `Index.Apex(name)`, an exact match on the `byApex` map already built.

#### 9.5.3 What may be transferred

| Zone type | Answer |
|---|---|
| `primary` | the zone |
| `secondary` | the zone, but only while `Serving()` (`zones/answer.go:92`) — never before its first transfer, never past `expires_at` |
| `internal` | refused: the RFC 6303 built-ins are empty by construction |
| `forwarder`, `stub` | refused: they hold no data to send |

A secondary re-serving what it pulled is deliberate and nearly free — the
`Serving()` gate D2 built is the whole of it. Refusing while not serving is
the same judgement §9.8 records for queries: a copy this server cannot vouch
for is one it does not hand on.

#### 9.5.4 `allow_transfer`: the format and the gate

Comma-separated, whitespace tolerated, empty means deny every transfer. Each
entry is one of:

- an IP address — `192.168.1.5`, `2001:db8::5` — matched as `/32` or `/128`
- a CIDR prefix — `10.0.0.0/24`
- `key:<name>` — the request must carry a TSIG that verified under that key

```
allow_transfer = "10.0.0.0/24, 192.168.1.5, key:secondary-ns2"
```

Entries are OR'd: any one match allows the transfer. The peer address matched
against them comes from `w.RemoteAddr()` and **must be unmapped first**, the
way `serve` already unmaps it (`dnssrv/server.go:122`): a v4-mapped
`::ffff:10.0.0.5` arriving on a dual-stack socket does not match
`10.0.0.0/24`, and the failure mode is an ACL that looks right and denies
everything.

Unlike `primaries`, parsing needs no context and no resolver
(`zones/primaries.go:80` takes both, because a primary may be named by
hostname) — an ACL is matched against a socket address on every request, so a
hostname here would mean a DNS lookup inside the gate of the server answering
DNS.

There are no negation entries. A default-deny list has nothing to subtract
from, and adding `!` syntax would create an ordering question the OR above
does not have.

The API validates on write, so a stored value always parses. A value that
nonetheless fails to parse — a hand-edited database — **fails closed**: the
whole ACL is treated as deny and the parse error is logged once per attempt,
rather than the entries before the bad one being honoured.

A `key:` entry names a TSIG key that must exist at write time, validated the
way D2 validates `tsig_key_id`, and it joins that column under the same
in-use guard: deleting a key some zone's `allow_transfer` names is refused
with the dependants named, because the alternative is a secondary that
silently stops being able to transfer.

#### 9.5.5 Refusals, and what each one says

The gate runs in this order, and each failure has its own rcode and its own
log reason. Nothing below is a judgement call left to implementation.

| Condition | Rcode | Reply carries |
|---|---|---|
| not exactly one question, class not IN, or qtype neither AXFR nor IXFR | FORMERR | |
| AXFR arrived over UDP (checked after the rows below — see the bullet) | NOTIMP | |
| no zone at that apex, zone disabled, or type `internal`/`forwarder`/`stub` | NOTAUTH | |
| `secondary` that is not `Serving()` | SERVFAIL | |
| TSIG present, key unknown or algorithm mismatched (`dns.ErrSecret`, `dns.ErrKeyAlg`) | NOTAUTH | TSIG RR, BADKEY (17), **unsigned** |
| TSIG present, MAC did not verify (`dns.ErrSig`) | NOTAUTH | TSIG RR, BADSIG (16), **unsigned** |
| TSIG present, outside the fudge window (`dns.ErrTime`) | NOTAUTH | TSIG RR, BADTIME (18) with our time in Other Data and the client's echoed in Time Signed, **signed** |
| TSIG lookup failed because the store failed | SERVFAIL | never a TSIG error |
| `allow_transfer` empty, or no entry matched | REFUSED | |
| already at the concurrency cap | SERVFAIL | |

Where each of these comes from:

- **NOTAUTH for an apex we do not hold** is RFC 5936 §2.2.1: "If a server is
  not authoritative for the queried zone, the server SHOULD set the value to
  NotAuth(9)." REFUSED is kept for the ACL denial, where it means what it
  says — a policy refusal by a server that does hold the zone.
- **NOTIMP for AXFR over UDP** is ours, and the RFC is explicit that it has to
  be: §4.2 says "this document does not update RFC 1035 in this respect: AXFR
  sessions over UDP transport are not defined", and offers no rcode. NOTIMP is
  the honest one for a transport we do not implement. It is listed second
  because that is where it belongs in the reading order, but it is *checked*
  after every row below it, and the difference matters: a peer outside the ACL
  asking for AXFR over UDP gets REFUSED, and an apex we do not hold gets
  NOTAUTH, rather than either being told about the transport. Authorisation is
  decided in one place for every transport, and the transport then decides only
  how a peer that may be told anything is answered. The concurrency cap is the
  one row this puts a constraint on: a UDP AXFR is answered NOTIMP without
  streaming anything, so the cap has to be taken *after* the transport branch,
  or a transport we do not implement would hold one of the four slots and be
  answered SERVFAIL rather than NOTIMP. (Recorded 2026-09-01, when D3's task 6
  implemented both branches and the two orders turned out to differ.)
- **The TSIG error codes** are RFC 8945's, including that the reply "MUST be
  unsigned" for BADKEY and BADSIG. That is the same conclusion
  `dnssrv/server.go` already reached for the ordinary path — "signing it would
  assert an authenticity the server was unable to establish" — so D3 adds the
  error RR that names the failure without changing the signing rule.
- **BADTIME is signed, and the other two are not.** This looks like an
  inconsistency and is the rule; it follows from what the server managed to
  establish before it answered. For BADKEY and BADSIG there is nothing to sign
  with — the key is unknown, or the MAC did not verify — and RFC 8945 says of
  each of them "This response MUST be unsigned", where each error is raised:
  §5.2.1 for BADKEY and §5.2.2 for BADSIG, both pointing at §5.3.2 for the
  shape of an error return rather than for the sentence itself. For BADTIME the
  key and the MAC *did* verify and only the two clocks disagree, so the server
  both can sign and must: §5.2.3, "A response indicating a BADTIME error MUST be signed
  by the same key as the request. It MUST include the client's current time in
  the Time Signed field, the server's current time (an unsigned 48-bit
  integer) in the Other Data field, and 6 in the Other Len field." Echoing the
  client's own time is what makes the reply checkable by the peer whose clock
  is wrong — its own clock is the one thing it can verify against — and an
  unsigned clock report is one an off-path attacker could forge, which would
  turn the mechanism for fixing skew into a mechanism for causing it. (This
  table said "unsigned" for all three until 2026-09-01, when implementing it
  turned up the difference; the bullet above was always careful to claim the
  MUST for BADKEY and BADSIG only. All three sentences were attributed to
  §5.3.2 until the same day, when they were checked against the RFC and found
  to live in §5.2.1, §5.2.2 and §5.2.3; the quoted text was right throughout.)
- **miekg can sign a BADTIME reply and cannot verify one**, which is worth
  knowing before reading a peer's logs: `stripTsig` (`tsig.go:341-343`)
  rejects any message whose rcode is NOTAUTH with `ErrAuth` before it looks at
  the signature, and every TSIG error reply is NOTAUTH by definition. A
  dnsaur secondary therefore reports a BADTIME from its primary as "bad
  authentication" rather than as a clock problem. That is the library's
  verification path, not ours, and it is not a reason to send the reply in a
  shape the RFC forbids — a BIND peer checks it and reads the clock out of it.
- **A store failure is never a TSIG error.** `tsigProvider.key` wraps a failed
  lookup rather than collapsing it into "no such key" (`dnssrv/tsig.go:87`),
  and the distinction must survive to the wire: telling a correctly configured
  peer its key is bad, because our database was briefly unavailable, sends the
  operator to the wrong end of the system.
- **An unsigned request is not a TSIG failure.** It is an ACL outcome: if
  every entry is a `key:` entry, an unsigned request matches nothing and gets
  REFUSED. `ErrTSIGUnsigned` exists precisely so these two cannot blur.

**IXFR is answered with a full AXFR**, which RFC 1995 §2 permits — "the server
may choose to transfer the entire zone just as in a normal full zone transfer"
— and is what dnsaur does until D5 builds the journal (§9.6).

**An IXFR over UDP that passes the gate is answered with a single SOA.** RFC
1995 §2: "If the UDP reply does not fit, the query is responded to with a
single SOA record of the server's current version to inform the client that a
TCP query should be initiated." Since our IXFR answer is the whole zone, it
does not fit by construction for any zone worth transferring, so the single
SOA is the answer for every UDP IXFR rather than a size-dependent branch. It
is signed like any reply to a request that verified, and it goes out through
the existing `fitUDP` reservation (`dnssrv/server.go:195`), so a signed reply
that would overshoot 512 becomes RFC 8945 §5.3's TC reply rather than an
over-size packet.

#### 9.5.6 Envelopes, and why `dns.Transfer.Out` is not used

`Out` (`xfr.go:223`) is nineteen lines, six of which do anything: `SetReply`,
`Authoritative`, append the RRs, sign if the request was signed, write, then
`TsigTimersOnly(true)`. It cannot be used here, for a reason beyond the
`xfr.go:229` "assume it fits TODO(miek): fix" that §9.5 has always cited:

RFC 5936 §2.2.5 — "If the client has supplied an EDNS OPT RR in the AXFR query
and if the server supports EDNS as well, it SHOULD include one OPT RR in the
first response message and MAY do so in subsequent response messages." `Out`
builds each message itself and only ever appends to `Answer`, so there is no
seam at which an OPT could be added. It also leaves `Compress` unset — the
source comment is literally `// Compress?` — which costs bytes on every
envelope of every transfer.

Writing the loop ourselves gives up nothing, because the TSIG stream state
does not live in `Transfer`. The running MAC and the timers-only flag are
fields on miekg's `response` (`server.go:755`, `server.go:824-827`), threaded
through `WriteMsg` and `TsigTimersOnly` on the `ResponseWriter` — the same two
methods `Out` calls. The chaining stays the library's:

```go
for i, batch := range batches {
	m := new(dns.Msg)
	m.SetReply(q)
	m.Authoritative = true
	m.Compress = true
	m.Answer = batch
	if i == 0 && q.IsEdns0() != nil {
		m.SetEdns0(dns.DefaultMsgSize, false)   // RFC 5936 §2.2.5
	}
	if reqTSIG != nil {
		// RFC 8945: every message in the response is signed.
		m.SetTsig(reqTSIG.Hdr.Name, reqTSIG.Algorithm, tsigFudge, time.Now().Unix())
	}
	if err := w.WriteMsg(m); err != nil {
		return err
	}
	w.TsigTimersOnly(true)       // after the first, per RFC 8945
}
```

**Batching.** SOA first, every record, the same SOA last, and never an SOA in
between — RFC 5936 §2.2: "The first message MUST begin with the SOA resource
record of the zone, and the last message MUST conclude with the same SOA
resource record. Intermediate messages MUST NOT contain the SOA resource
record." A batch is closed when adding the next RR would pass a 16 KiB target,
well under TCP's 65535 ceiling and comfortably inside §2.2's "sufficient
number of RRs to reasonably amortize the per-message overhead, up to the
largest number that will fit within a DNS message".

Size is accumulated with `dns.Len`, which measures the record **uncompressed**,
against a target the compressed message is then packed into. The estimate is
therefore an upper bound and the message always fits, at the cost of slightly
under-filled envelopes — which §2.2 permits, since it asks for amortisation
rather than a maximum. The alternative, packing after each record to measure
exactly, is quadratic in a zone's record count to recover bytes nobody counts.
When the request is signed, the signature's room comes off the target first:
`dns.Len` of the stub plus `maxTSIGMACLen` (`dnssrv/tsig.go:257`), the same
reservation `fitUDP` makes.

Every record is rendered through `ToRR` (`zones/zone.go:92`), the renderer the
query path uses. A row that will not render aborts the transfer with SERVFAIL
rather than being skipped: a zone silently missing a record is worse on a
secondary than a transfer that visibly failed, because nothing downstream will
ever notice.

**A `zone_records` row typed SOA is left out of the body**, and it is the one
exclusion. The zone's SOA comes from the zone row, and §2.2 gives the stream
exactly two places for one: in the body such a row would be an intermediate
message's SOA, which is forbidden outright, and at the ends it would contradict
the SOA already there. Leaving it out loses the peer nothing, since the zone
row's SOA is transferred for that same name. Nothing writes such a row today —
its provenance is a hand-edited database, the same one `ParseACL`'s fail-closed
branch is written for.

#### 9.5.7 Runtime bounds

**Its own deadline.** A 2-minute transfer context replaces the pipeline's 5
seconds, checked between envelopes.

**A cap of 4 concurrent outbound transfers**, and the reason is a library
finding rather than caution. `dns.Server.WriteTimeout` is documented at
`server.go:220-221` as "the net.Conn.SetWriteTimeout value for new
connections, defaults to 2 * time.Second" — and nothing in the server ever
applies it; there is no `SetWriteDeadline` call on the serve path, and
`dns.ResponseWriter` exposes no connection to set one on. A peer that stops
reading therefore blocks `WriteMsg` indefinitely, holding a goroutine and that
zone's built RR slice. Bounding how many such transfers can exist at once is
the only lever the library leaves. Over the cap is SERVFAIL and a log line;
the peer retries on its own SOA schedule.

**The zone is rendered inside the slot**, once it has been taken and never
before. The built RR slice, and the `dns.NewRR` per record that produces it,
are half of what the sentence above says the cap bounds; `dns.Server` bounds
concurrent TCP connections at nothing, so a render ahead of the slot would be
one whole zone built per connection the ACL admits, with everything over the
cap built, refused and discarded. The one behaviour this settles: a zone
holding an unrenderable row, asked for while the cap is full, is refused by
the cap rather than by the row — the same SERVFAIL, a different reason in the
log and in `last_xfr_error`. (Recorded 2026-09-01, when the whole-branch
review found the build sitting outside the cap it is part of.)

No outbound twin of `DefaultMaxTransferRecords` (`zones/transfer.go`). That
limit exists because an inbound transfer's size is chosen by a remote peer; an
outbound one is chosen by this server's own zone, already resident in the
snapshot.

#### 9.5.8 What the operator sees

Migration `0011` (next free — 1 through 10 are taken, 5 and 7 being the Go
migrations `migrate.go` claims without SQL files) adds four columns to
`zones`: `allow_transfer`, `last_xfr_at`, `last_xfr_peer`, `last_xfr_error`.

The write is `NoteTransferRequest`, a single UPDATE touching only the three
state columns — `allow_transfer` is configuration, written by the API like any
other zone field — mirroring `NoteTransferAttempt` (`store/zones.go:220`) for the
same reason D2 needed it: a full-row `UpdateZone` racing a transfer would
erase what the transfer recorded, and D2 already pins that property in a store
test — this mirrors it for the outbound side.

It mirrors that method's *name* for a second reason. Both record a refused
attempt as readily as a successful one, so neither can be named for an
outcome. (It was `NoteTransferServed` until 2026-09-01, when the whole-branch
review pointed out that the name left its own doc comment arguing with it.)

Both outcomes are recorded, refusals included, because "ns2 is not updating"
is usually "ns2 is not in `allow_transfer`" and the log is not where an
operator looks first. Since a remote peer therefore triggers a write, state
writes are **throttled to one per zone per 10 seconds** — an unauthenticated
peer in a loop must not become an UPDATE loop against sqlite's single
connection (`store/store.go`, `SetMaxOpenConns(1)`). The log line is never
throttled.

**Only a request that arrived over TCP is recorded** (decided 2026-09-02,
after a UDP AXFR probe was watched live overwriting a real transfer's row —
`Last served 2m ago to 192.168.150.40` replaced by `Refused
192.168.150.40 — AXFR over UDP is not defined`, the same peer and zone, the
line an operator actually needed gone). A zone transfer is a TCP protocol;
a UDP arrival — the `NOTIMP` a UDP AXFR gets, the single-SOA reply a UDP
IXFR gets (below), or a UDP peer the gate refuses before either of those is
reached — is not a transfer attempt, it is a peer using the wrong
transport, so none of it writes `last_xfr_at`/`last_xfr_peer`/
`last_xfr_error`. It is still logged, at the same level (`slog.Warn` for a
refusal, `slog.Info` for the IXFR probe) and never throttled — the log is
now the only record a UDP arrival gets. The gate itself, its rcodes and
their ordering are unchanged; only whether the outcome reaches
`NoteTransferRequest` is affected, and the check lives in `refuse`, at the
point that already decides whether there is a row to write to at all (`z !=
nil`) — not inside `note`, which stays an unconditional write once called,
and not repeated at `refuse`'s three call sites.

This buys two things:

- **`last_xfr_peer` becomes non-spoofable.** A UDP source address is
  trivial to forge; a TCP handshake off-path is not. Before this, the
  column had to be documented with a caveat — a UDP AXFR's refusal, peer
  address included, was recorded like any other outcome, so `docs/api.md`
  warned that the address was not proof of who asked. After this, every
  recorded peer completed a TCP handshake, and the caveat is gone rather
  than repeated.
- **The IXFR-over-UDP probe stops being a special case.** RFC 1995 §2's
  single-SOA reply to a UDP IXFR was, before this, the one refusal-shaped
  outcome that was logged but deliberately never recorded — recording it
  would have used `reason == ""`, indistinguishable from a served transfer,
  and `last_xfr_at` would then point at a probe rather than at a transfer.
  That exclusion needed its own justification. Now it needs none: it is a
  UDP arrival like any other, covered by the one rule above rather than
  carved out from the general one of "both outcomes are recorded".

The throttle has one override, and it is narrower than "a changed outcome":
**a served↔refused transition writes anyway**, whichever direction. A run of
refusals followed by a success would otherwise leave the screen saying
"refused" for up to ten seconds after the thing started working, which is
exactly when somebody is watching it — and that is the only transition
worth breaking the throttle for. Two different refusal reasons for the same
zone and peer are still both refusals, not a change an operator watching
the screen is waiting for; overriding on *any* differing reason would let a
peer that alternates between two refusal shapes (say, an unsigned request
and one signed under a key this server does not hold) write on every
request, at packet rate — the exact UPDATE loop the throttle exists to
prevent, reopened by its own escape hatch.

#### 9.5.9 Test posture

D2's posture inverted: a real client against a real server against a real
store, no fakes at the seams.

- `dns.Transfer.In` against a real `dnssrv.Server` on `127.0.0.1:0` — the zone
  arrives whole, SOA first and last and nowhere else, a zone large enough to
  force several envelopes, OPT on the first message only, and a signed stream
  that verifies end to end.
- **The loopback test**: D2's `Transferrer` pulling from D3's `TransferServer`
  — one dnsaur secondary transferring from a dnsaur primary. Both halves exist
  now, and neither was written against the other.
- One test per row of §9.5.5, asserting the rcode *and* the TSIG error code
  where there is one — including that a store failure is SERVFAIL carrying no
  TSIG error at all.
- The ACL parser gets `primaries_test.go`'s table treatment, including the
  fails-closed case for an unparseable stored value.
- Store tests under `forEachDriver`; API tests for `allow_transfer`
  validation and the extended key-in-use guard; web tests; the e2e smoke
  extended to set an ACL and transfer through it.

#### 9.5.10 Deliberately not in D3

NOTIFY in either direction (D4). The journal, deltas and real IXFR (D5).
Per-record or per-view ACLs, which nothing has asked for. Transferring the
built-in RFC 6303 zones. And TSIG enforcement on ordinary queries, which stays
non-enforcing by design (§9.3) — D3 enforces it on the one path RFC 8945 gates.

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

> **Superseded by §9.11, and one claim here is withdrawn.** This section, like
> §4, describes migrating a conditional-forwarding config key into a zone row.
> There is no such key and there never was — see §9.11.1. D6 is additive, not
> a migration. The rest holds: the fall-through and the second-input problem
> are both real, and §9.11.4 solves the second with a swappable table rather
> than a second construction input.

### 9.8 Sub-milestones

D is too large to land as one plan. Each of these ships something that works:

| | Scope | Depends on |
|---|---|---|
| **D1** | TSIG keys: table, CRUD, dynamic provider, and the `TsigStatus` helper transfers call | — |
| **D2** | Outbound AXFR: `primaries` format, scheduling, expiry, atomic install, per-zone reload, **and ownership of `zones.tsig_key_id`** — the column and the guard against deleting a key a zone still references. D2 declined the schema-level foreign key with reasoning recorded in migration 0009: the column is `NOT NULL DEFAULT 0` where 0 means "no key", a FK skips NULL rather than zero, and making it nullable is a whole-table rebuild on SQLite where `zones` is the parent of `zone_records … ON DELETE CASCADE`. The guard is a single-statement delete instead | D1 |
| **D3** | Inbound AXFR: the `dnssrv` intercept, the `allow_transfer` ACL (a column, not the table §9.2 sketched), envelope batching written here rather than through `dns.Transfer.Out`, TSIG enforcement with RFC 8945 error codes, and last-served state on the zone | D1 |
| **D4** | NOTIFY both directions: the `dnssrv` opcode intercept, the inbound gate and its SOA probe, `notify_to` with per-target TSIG, and a `zone_notifies` queue whose pending-ness is derived from the serial rather than stored — **designed in §9.10** | D2, D3 |
| **D5** | IXFR: journal, deltas, serial arithmetic | D2, D3 |
| **D6** | `forwarder` **and** `stub` zone types: `forward_to`, a swappable conditional table on the forwarder, and a stub's SOA/NS fetch with glue handling — **designed in §9.11**. No `Conditional` migration: there was no key to migrate | — |

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
| **5936** | AXFR is TCP-only (§4.2 leaves UDP undefined); SOA first and last, never in between (§2.2); messages carry enough RRs to amortize the overhead (§2.2); NOTAUTH when not authoritative (§2.2.1); OPT echoed in the first response message (§2.2.5) |
| **1995** | IXFR falls back to a full AXFR when no delta is available (§2); a UDP IXFR whose reply does not fit is answered with a single SOA (§2) |
| **1996** | NOTIFY is a hint, not an instruction — the secondary still checks the SOA before transferring (§4.7), and the answer-section SOA is never acted on: §3.7–3.8, "In no case shall the answer section of a NOTIFY request be used to update a slave's local data, or to indicate that a zone transfer needs to be undertaken, or to change the slave's zone refresh timers." The sender retransmits until a response, a timeout, or ICMP port unreachable (§3.6), and any response ends the round whatever its rcode (§4.8). **One deliberate divergence:** §3.10 says a NOTIFY from a host that is not a known master "should ignore the request"; dnsaur answers REFUSED instead — reasoning in §9.10.2 |
| **8945** | TSIG: signed request, signed reply, time-window enforcement, and an unsigned request rejected rather than ignored; every message of a multi-message response signed, timers-only after the first; BADKEY and BADSIG reported in an unsigned reply (§5.2.1 and §5.2.2, each "This response MUST be unsigned", both pointing at §5.3.2 for the shape of an error return rather than for that sentence), BADTIME in a *signed* one carrying the client's time in Time Signed and ours in Other Data (§5.2.3) |
| **1982** | Serial arithmetic is circular — comparison is not `<` |
| **1034 §4.3.5** | A secondary past its SOA expire must stop answering for the zone |

### 9.10 Milestone D4: NOTIFY, both directions

Designed 2026-09-02, once D1–D3 had landed and the two halves could be costed
against code that exists. Every RFC 1996 sentence below was fetched rather
than recalled, and the one place this design knowingly disagrees with the RFC
is named as a divergence in §9.9 rather than left for a reader to discover.

The vocabulary inverts here exactly as it does across §9.4 and §9.5, and for
the same reason. **Outbound NOTIFY** is dnsaur as a *primary*, telling
secondaries the zone changed. **Inbound NOTIFY** is dnsaur as a *secondary*,
being told. That is the opposite pairing to §9.4/§9.5, where outbound meant
dnsaur pulling as a secondary — because a transfer is pulled and a NOTIFY is
pushed, so the same word follows the data in one case and the message in the
other. Read the direction off the zone type, never off the word.

#### 9.10.1 The intercept, and the defect it closes

`isTransferQuery` branches on qtype. NOTIFY is an *opcode*, so it is not
caught by it, and there is no other branch: a NOTIFY arriving at dnsaur today
falls into the ordinary pipeline.

That is a live defect, and it is worth stating plainly because it exists on
`main` independently of whether anyone ever configures a secondary.
`DefaultMsgAcceptFunc` accepts opcode NOTIFY — `acceptfunc.go:40`, and it
allows the answer-section SOA on purpose ("NOTIFY requests can have a SOA in
the ANSWER section. See RFC 1996 Section 3.7 and 3.11") — so the message
reaches `Server.serve`, carries one question of qtype SOA, and is handled as
though it were an ordinary SOA query. It is counted in the query log,
evaluated by the filter, eligible for the cache, and for an apex this server
does not hold it is **forwarded upstream**: dnsaur asks Cloudflare an SOA
question on behalf of a peer that was trying to notify it. D4 closes this,
and the regression test for it fails on the commit before D4's first.

So `serve` gains a third branch, beside the transfer one and ahead of the
chain:

```go
// internal/dnssrv/notifies.go
type Notifies interface {
    ServeNotify(ctx context.Context, w dns.ResponseWriter, m *dns.Msg, key string, tsigErr error)
}
func WithNotifies(n Notifies) Option
```

Unlike a transfer, a NOTIFY reply *is* a single message, so it would fit
`Handler` mechanically. It still must not go through it: the pipeline is
precisely what has no meaning for it, and the forwarding above is what that
costs. The branch is therefore an interception for the same reason §9.1's is,
without the same justification — a transfer cannot be a `Handler`, a NOTIFY
merely must not be one.

`key` and `tsigErr` are what `Server.RequireTSIG` already concluded at the top
of `serve`, passed rather than recomputed. The reasoning is `Transfers`' own:
two callers of one check are two chances to disagree, and here the
disagreement would be a NOTIFY acted on without the signature the zone
requires.

Without `WithNotifies` the branch is not taken and a NOTIFY falls through as
it does now, which is what every server built before D4 did. That is the same
shape `WithTransfers` uses and it keeps `dnssrv` free of any zones dependency.

**Naming.** `zones.TransferServer` answers a peer pulling from us;
`zones.NotifyServer` answers a peer notifying us. `zones.Notifier` is the
outbound half. The two halves share `serialNewer` and nothing else.

#### 9.10.2 Inbound: the gate, and what each refusal says

`NotifyServer.decide` is pure — it takes the message, the peer, the parsed
primaries, the key and the TSIG error, and returns a zone or a refusal — so
the table below is directly a test table, one case per row, as §9.5.5's is.

| Condition | Rcode | Reply carries |
|---|---|---|
| qtype is not SOA | FORMERR | |
| no zone at that apex | NOTAUTH | |
| zone is a type with no master to re-ask — `primary`, `forwarder`, `internal` | NOTAUTH | |
| zone is disabled | NOTAUTH | |
| source address matched no entry in `primaries` | REFUSED | |
| zone names a TSIG key and the message is unsigned | REFUSED | |
| TSIG present, key unknown or algorithm mismatched | REFUSED | TSIG RR, BADKEY (17), **unsigned** |
| TSIG present, MAC did not verify | REFUSED | TSIG RR, BADSIG (16), **unsigned** |
| TSIG present, outside the fudge window | REFUSED | TSIG RR, BADTIME (18), **signed** |
| TSIG lookup failed because the store failed | SERVFAIL | never a TSIG error |
| otherwise | NOERROR, AA set, question echoed | signed iff the request verified |

Where these come from, and where they are ours:

- **The type row is "pulls from a master", not "is a secondary".** A stub
  pulls its delegation on the same schedule, through the same
  `Refresher.Refresh`, under the same per-zone lock and recording the same
  attempt (`pullsFromAMaster`), so a master that has moved its delegation
  tells a stub exactly the way it tells a secondary — and a gate naming only
  `secondary` would have been the one path that made a stub wait out its SOA
  `refresh` instead. A `primary` owns its data and a `forwarder` names its
  upstreams outright in `forward_to`; neither has anybody to ask, so a NOTIFY
  for one has no work it could cause.

- **The TSIG rows follow §9.5.5 exactly**, including that BADKEY and BADSIG
  are unsigned and BADTIME is signed. The rcode differs — D3 answers a
  transfer NOTAUTH and this answers REFUSED — because the two are refusing
  different things. A transfer refusal under RFC 5936 §2.2.1 is about
  authority over the zone; a NOTIFY refusal is a policy statement by a server
  that *does* hold the zone and declines to be told by this peer.
- **NOTAUTH for a zone we do not hold, or hold as a primary**, is ours: RFC
  1996 specifies no rcode for it. It is the honest one, and it matches what
  §9.5.5 already chose for the same condition on the transfer path, so an
  operator reads one rule rather than two.
- **REFUSED for an unknown source is a deliberate divergence from RFC 1996
  §3.10**, which says: "If a slave receives a NOTIFY request from a host that
  is not a known master for the zone containing the QNAME, it should ignore
  the request." Decided 2026-09-02 to answer instead, because silence is
  indistinguishable from a firewall drop, and the overwhelmingly common cause
  of this condition in a homelab is a `primaries` list that is one address
  wrong — a case the operator can fix in seconds if told and may not diagnose
  at all if not. The security argument for silence does not survive the
  numbers: a NOTIFY is roughly 50 bytes and a REFUSED reply roughly the same,
  so there is no amplification, and 1:1 reflection is not a useful attack
  primitive. It is a SHOULD, not a MUST. Recorded in §9.9.
- **Nothing here is rate-limited by rcode.** The throttle that matters is on
  the work a NOTIFY causes, not on the reply, and it is §9.10.3's.

**The reply goes out before any work happens.** RFC 1996 §4.7 has the slave
"enter the state it would if the zone's refresh timer had expired", and §3.6
has the master retransmitting until it gets a response — so a responder that
waited for an AXFR before answering would earn itself a second NOTIFY for the
transfer already in flight. `ServeNotify` writes its reply, then hands off.

**The handoff does not inherit the request's context.** `serve`'s context is
cancelled when it returns, and a transfer started under it would be cut off
mid-zone. The goroutine takes a background context with its own timeout, for
the reason `recordAttempt` already takes `context.WithoutCancel`: work that
outlives the request that triggered it needs a lifetime that does too.

#### 9.10.3 Inbound: what happens after the reply

In this order.

1. **Throttle, per zone.** A primary editing ten records sends ten NOTIFYs.
   Each gets its own immediate NOERROR — that is the peer's business — but
   they collapse to one SOA probe. Without this, "NOTIFY is cheap" becomes a
   probe amplifier pointed at our own primary. The shape is
   `transferStateThrottle`'s, which already exists for the same class of
   problem on the transfer path.

2. **`refreshed_at == 0` transfers unconditionally, with no probe.** A
   secondary created through the API starts at `soa_serial = 1`
   (`zones_handlers.go:285`). A primary that is also at serial 1 would make
   every serial comparison say "not newer", and the zone would stay
   permanently empty while reporting nothing wrong. Never-transferred is not a
   serial question, and this row is why the probe is a step in a sequence
   rather than a gate on the whole thing.

3. **Otherwise, probe.** `Transferrer.ProbeSerial` queries SOA against the
   zone's configured primaries in order — not the notifier's address. The two
   sets are the same by the time this runs, since row 5 of §9.10.2 is what let
   the message get here, so using the configured list costs nothing and keeps
   one answer to "who is this zone's primary". Signed with the zone's key when
   it has one, first answer wins, failures fall through to the next primary
   exactly as `Transfer` does.

4. **Compare, and transfer if newer.** `serialNewer(probed, local)` →
   `Refresher.Refresh(ctx, zoneID)`, which already holds the per-zone transfer
   lock correctly and is the same call the manual path makes. Not newer is a
   debug log and nothing else: the NOTIFY was true, we were already current.

**The answer-section SOA is ignored, and this is not an oversight.** RFC 1996
§3.7 calls it "an unsecure hint at the new RRset", and §3.7–3.8 then forbid
acting on it outright: "In no case shall the answer section of a NOTIFY
request be used to update a slave's local data, or to indicate that a zone
transfer needs to be undertaken, or to change the slave's zone refresh
timers." Comparing against it to skip the probe would be exactly the second of
those three. The probe is the mechanism the RFC leaves; the hint is not a
shortcut through it.

**The scheduled refresh path is unchanged.** D2's `RefreshDue` still AXFRs
without probing. Probing there too would be less wasteful on large zones and
is what BIND does, but it changes behaviour that has shipped and pulls D2's
scheduling tests into a NOTIFY milestone. Recorded here as a candidate, not
taken.

#### 9.10.4 `serialNewer`, and the pair RFC 1982 leaves undefined

Both halves need one comparison and the repo has none. `zonefile_handlers.go:352`
reasons about wraparound in a comment and never compares; every other serial
site assigns.

```go
// internal/zones/serial.go
func serialNewer(a, b uint32) bool
```

RFC 1982 §3.2 defines the comparison over a circle, so `a > b` is wrong at the
wrap and right everywhere else, which is the worst possible failure shape — it
works for years and then strands a zone at 4294967295. The subtlety worth a
test rather than a comment is that the comparison is **not total**: two serials
exactly 2^31 apart have no defined ordering.

**Policy: undefined compares as not-newer.** A transfer that does not happen
is recovered by the refresh timer on the next tick. A transfer that should not
have happened is a full AXFR of someone else's zone, and on the outbound side
an undefined pair resolving to "newer" would notify every target on every pass
forever. The asymmetry of the two mistakes is the whole argument, and the test
pins the undefined pair explicitly so a later refactor cannot quietly flip it.

#### 9.10.5 `notify_to`: the format

A column on `zones`, `TEXT NOT NULL DEFAULT ''`, comma-separated, each entry
`host[:port] [key:name]`. Empty means notify nobody and is the default, so
nothing changes for an existing install. Port defaults to
`DefaultPrimaryPort`. Stored in `FormatNotifyTo`'s canonical spelling rather
than as typed, the rule `allow_transfer` and `primaries` both already follow.

```go
// internal/zones/notifyto.go
type NotifyTarget struct {
    Host string // as written — may be a hostname
    Port uint16
    Key  string // canonical TSIG name; "" means send unsigned
}
func ValidateNotifyTo(s string) error
func ParseNotifyTo(s string) ([]NotifyTarget, error)
func FormatNotifyTo(ts []NotifyTarget) string
func NotifyToKeys(s string) []string
```

**Why this is neither `primaries.go` nor `acl.go` with different words**, since
it borrows from both and a reader will assume it is a copy of one of them.
`ParseACL` is pure because it is matched against a socket address on every
request and a hostname there would mean a DNS lookup inside the gate of the
server answering DNS. `ParsePrimaries` takes a context and a resolver because a
primary named by hostname must be followed at transfer time. `ParseNotifyTo`
needs **both properties, split**: it is pure, and resolution happens separately
at send time. That is not a compromise between the two, it is forced by the
queue — §9.10.6's row identity is the *written* host, and a parser that
resolved would make the identity an address, so a hostname that moved would
orphan its delivery history and start a new row every time it changed.

**The per-target key is why the format is richer than `primaries`'.** A dnsaur
primary has no key of its own to sign with: `zones_handlers.go:87` refuses
`tsig_key_id` on a primary zone, because that column means "the key a
*secondary* signs its transfer requests with". Without a per-target key, a
dnsaur primary would send unsigned NOTIFYs to a dnsaur secondary whose zone
has a key, and §9.10.2's row 6 would refuse them — dnsaur unable to notify
itself through a configuration it fully supports. The alternatives were
considered and rejected: relaxing row 6 loosens a rule to fit a gap, and
overloading `tsig_key_id` to mean something different depending on zone type
gives one column two meanings and still cannot give two secondaries two keys.

**The TSIG delete guard extends again.** D3 widened the key-delete guard from
`tsig_key_id` to `key:` references in `allow_transfer`. `notify_to` is a third
reference, and without it deleting a key silently downgrades a signed NOTIFY to
an unsigned one that the peer then refuses — a failure that shows up nowhere
near the delete that caused it. `NotifyToKeys` exists for that guard and for
the usage count on the TSIG keys screen.

**Which zone types may carry it.** Primary and secondary both: a secondary
needs it for the cascade (§9.10.7). Refused on `internal`, since the RFC 6303
built-ins are not transferable — D3's rule, unchanged. This makes `notify_to` a
both-types field like `allow_transfer`, unlike `primaries` and `tsig_key_id`,
and `checkZoneTransferConfig` is where that is enforced.

#### 9.10.6 Outbound: the queue, and why pending-ness is not stored

Migration **0012** — the next free version. It cannot be read off the
directory listing: 5 and 7 are Go migrations claimed in `migrate.go`
(`zonemigrate.go`, `builtins.go`) with no SQL file, and a duplicate version
fails goose at startup on both drivers. Both `sqlite/` and `postgres/`
variants, `BIGINT` where sqlite has `INTEGER` for the unix-ms and serial
columns, per 0010's note.

```sql
CREATE TABLE zone_notifies (
  id              INTEGER PRIMARY KEY,
  zone_id         INTEGER NOT NULL REFERENCES zones(id) ON DELETE CASCADE,
  target          TEXT    NOT NULL,           -- 'ns2.example.com:53', as written
  pending_serial  INTEGER NOT NULL DEFAULT 0, -- the round being attempted
  notified_serial INTEGER NOT NULL DEFAULT 0, -- the last round that landed
  notified_at     INTEGER NOT NULL DEFAULT 0, -- unix ms; 0 = never
  attempts        INTEGER NOT NULL DEFAULT 0, -- within the current round
  next_attempt_at INTEGER NOT NULL DEFAULT 0,
  last_error      TEXT    NOT NULL DEFAULT '',
  created_at      INTEGER NOT NULL DEFAULT 0, -- when this target was first seen
  UNIQUE(zone_id, target)
);
```

**`created_at` exists for one line on screen**, and is recorded here so it does
not look like habit. A target in state `never` has no notify date to show — that
is what the state means — so the artboard dates it by when it was *added*
("added 2m ago") rather than leaving the column blank. Without this column the
only honest rendering is an empty cell, and an empty cell in a row whose whole
purpose is "nothing has happened yet" reads as missing data rather than as the
answer. It is written by the reconciliation pass in §9.10.6 when it creates the
row, and never updated afterwards.

**This one takes a real foreign key, unlike `zones.tsig_key_id`.** Migration
0009 declined one there with reasoning worth not re-deriving: that column is
`NOT NULL DEFAULT 0` where 0 means "no key", a FK skips NULL rather than zero,
and making it nullable is a whole-table rebuild on SQLite. None of that applies
here — `zone_id` has no zero-means-none case — and `zone_records` already sets
the `ON DELETE CASCADE` precedent, so deleting a zone needs no application code
at all.

**The key is not in the row.** `target` is host and port only. The key is read
from the zone's current `notify_to` at send time, so re-keying a target keeps
its delivery history instead of orphaning it — the same reasoning that keeps
the hostname unresolved in §9.10.5, applied to the other half of the entry.

**There is no `pending` column, and that is the design.** The serial already is
one. A row records only what was *achieved*, and whether there is work is
derived by comparing it against the zone:

```
want := zone.soa_serial
if notified_at != 0 && !serialNewer(want, notified_serial) { continue } // nothing to say
if pending_serial != want { pending_serial, attempts = want, 0 }        // a new round
if attempts >= maxNotifyAttempts { continue }                           // this round gave up
if now < next_attempt_at { continue }
send
```

This is what makes the trigger self-healing, and it is the deliberate answer to
the failure §9.4 names as this project's most repeated: an automatic path that
skips a rule the human path enforces. There is no "remember to enqueue a
notify" call for a future mutation path to forget. Any write that advances a
serial — through the API, an import, auto-PTR, a transfer install, or one
nobody has thought of yet — is picked up by the next pass because the serial is
the only thing consulted.

Three consequences that follow from it and are all wanted:

- **A new target is notified at the current serial**, because `notified_at == 0`
  is "never told". Adding a secondary tells it, rather than leaving it silent
  until the next unrelated edit.
- **Nothing fires at startup.** Every row already records delivery at the
  current serial, so the first pass finds no work. A restart is not news.
- **Giving up is per round, not per target.** The next serial bump resets
  `attempts` and tries again, so a secondary that was down for an hour is
  retried the moment there is something to say — without an operator clearing
  a flag.

**The same pass reconciles rows**: it creates any (zone, target) pair present in
`notify_to` with no row, and deletes any row whose target has left the list. So
nothing hooks zone PATCH, and a hand-edited `notify_to` converges on the next
pass rather than depending on which code path wrote it.

#### 9.10.7 Outbound: sending, retrying, and stopping

`zones.Notifier`. `Run(ctx)` is `Refresher.Run`'s shape — one pass immediately,
then on a ticker — and the pass is the loop in §9.10.6.

```go
func (n *Notifier) Wake() // non-blocking: run a pass now, not at the next tick
```

**`Wake` is promptness, never correctness.** It goes beside the existing
`s.reloadZones(r)` calls and into `Transferrer`'s install through a new
`WithNotifyWake` option. Missing a call site makes a notify late by one tick; it
cannot lose one, because §9.10.6's detection does not depend on being told. This
property is the point of the design and the spec says so here so that a later
reader does not "fix" `Wake` into a mandatory call and quietly reintroduce the
failure mode it was built to avoid.

**The cascade** is that same `Wake`, from the secondary side: dnsaur transfers a
zone from its upstream primary, the install writes the primary's serial
verbatim (`transfer.go:610`), and the pass then finds every downstream target
behind. No separate cascade logic exists, and `contentChanged` is not consulted
— the serial decides, as it does everywhere else here.

**Sending.** UDP, built with `dns.Msg.SetNotify(zone)`, which sets opcode,
AA and the SOA question (`defaults.go:44`). Signed when the target names a key.
A response ends the round, and RFC 1996 §3.6 is explicit that it does not matter
which one: a master retransmits "until either too many copies have been sent (a
'timeout'), an ICMP message indicating that the port is unreachable, or until a
NOTIFY response is received from the slave with a matching query ID, QNAME, IP
source address, and UDP source port number", and §4.8 adds "When a master server
receives a NOTIFY response, it deletes this query from the retry queue." So a
REFUSED ends the round exactly as a NOERROR does — the peer heard us, and
retransmitting will not change its mind — but a non-NOERROR rcode is still
written to `last_error`, because ending the round and being satisfied with the
outcome are different things and the operator needs to see the second one.

**Three stop conditions, all of them §3.6's.** A response, an attempt budget,
and ICMP port unreachable — which in Go surfaces as `ECONNREFUSED` on the read
from a connected UDP socket, and is worth handling as its own case rather than
as one more timeout: it is positive evidence that nothing is listening, and
burning four more attempts on it delays nothing but the truth.

**The matching rule is §3.6's too**, and it is not free: query ID and QNAME must
be checked against what was sent. A connected UDP socket gives the source
address and port for nothing, but an off-path response with a guessed ID would
otherwise end a round that never landed.

**Retry budget**: 5 attempts, 5s → 10 → 20 → 40 → 80, then the round rests. Two
and a half minutes, against a secondary refresh interval measured in hours. The
secondary's own refresh timer is the backstop it always was, which is what keeps
this a delivery optimisation rather than a correctness dependency — and is why
the budget can be small.

#### 9.10.8 What the operator sees

`notify_to` rides the existing `POST /zones` and `PATCH /zones/{id}`. No new
mutation route: a manual "notify now" button was considered and dropped, because
§9.10.6's give-up-per-round already retries on the next edit, which is when
there is something to say. Nothing to press means nothing to explain.

One new read route, because per-target state on every row of the zone list
would be a join nobody asked for:

```
GET /api/v1/zones/{id}/notifies
→ [{ target, state, notified_serial, notified_at, attempts, max_attempts,
     last_error, created_at }]
```

`state` is derived server-side — `never` | `current` | `retrying` | `gave_up` —
rather than left to the dashboard to infer from the columns. Same rule D3
applied to `last_xfr_*`: a status that two clients could compute differently is
not a status. `max_attempts` ships beside `attempts` for the same reason: the
retry budget is a server constant (§9.10.7), and the screen renders it as
"try 3/5", so sending only the numerator would make the dashboard hardcode a
number it does not own. `openapi.yaml` documents the fields and the route in
the same commit that adds them.

**The artboard settles the presentation** (Zone Detail, updated 2026-09-02), and
what it decided is worth recording because two of its choices are not what the
brief asked for and both are better:

- **The resting state is one line, not N.** Four targets all `current` is four
  identical rows saying nothing, so the row collapses to a roll-up — `no
  targets` / `all 4 current` / `2 of 4 behind` — and only targets that are
  *behind* (`retrying` or `gave_up`) get a row of their own by default. A caret
  expands the rest. This is a better answer to "can the operator tell at a
  glance" than any styling of four equal rows: the glance is the summary line,
  and the roll-up turns amber only when something is behind.
- **`gave_up` is labelled "not acknowledged"**, never "gave up", and is drawn
  amber-on-hollow rather than red — distinguishable from `retrying`'s filled
  amber without claiming to be a failure. The state column is the only column
  carrying colour, so it is the scan path and nothing competes with it.

The four labels are `current`, `never notified`, `retrying · try 3/5`, and
`not acknowledged`. They are presentation and the API enum stays as it is above;
an implementer maps one to the other rather than renaming either.

Layout is a five-column grid — target, state, serial, when, error — sharing
`AllowTransferBand`'s 152px label gutter so `SOA` (or `TRANSFERS IN`),
`TRANSFERS OUT` and `NOTIFY OUT` land on one edge. Read is default, edit is
opt-in behind a pencil, exactly as `allow_transfer` works. Errors are shown
verbatim and right-aligned, tinted with the state's own colour.

The artboard gates the row on `!builtIn`, which is the same set as
`AllowTransferBand`'s primary-or-secondary gate given the API creates no other
type. Mount it on the **positive set** regardless, as that band's call site
already does: a stub or forwarder row reaching this control is a control that
fails on click rather than one the screen declined to draw.

The empty state carries most of the weight: a zone with no targets is the
common case and is *normal*, not unconfigured — it reads `none`, muted, with
`no targets` beside it. The TSIG keys screen's usage count gains `notify_to`
references alongside `allow_transfer`'s.

#### 9.10.9 Test posture

- `notifyto_test.go` gets `primaries_test.go`'s table treatment, including the
  fails-closed case for an unparseable stored value.
- `serialNewer` gets an RFC 1982 table: ordinary pairs, the wrap at 2^32, and
  the 2^31-apart pair §3.2 leaves undefined, pinning §9.10.4's policy so a
  later refactor cannot flip it silently.
- One test per row of §9.10.2, asserting the rcode *and* the TSIG error code
  where there is one — including that a store failure is SERVFAIL carrying no
  TSIG error at all.
- **The disjoint-column-sets rule, a third time.** D2 shipped a defect from it
  and D3 wrote a test rather than trusting the rule. D4 has two more surfaces:
  `updateZoneSQL` must bind `notify_to` and must never bind `zone_notifies`
  state, and the queue's writer must bind only its own columns. Same test
  shape, mirrored, rather than a comment saying to be careful.
- Store tests under `forEachDriver`; sqlite and a real Postgres container both
  execute. Any concurrency assertion here has to defeat `SetMaxOpenConns(1)`
  deliberately, or it passes without testing anything.
- The pass: round start, give-up, reset-on-serial-advance, and reconciliation
  both adding and removing rows.
- **The §9.10.1 regression**: a NOTIFY must not reach the pipeline. Assert it
  is neither query-logged nor forwarded upstream. This one fails on the commit
  before D4's first, which is what makes it a regression test rather than a
  restatement.
- The loopback, extending D3's round trip: a dnsaur primary notifies a dnsaur
  secondary, signed, the secondary probes and transfers, and the test fails if
  either half is wrong.
- Every fix's test shown failing with the fix removed.
- The roll-up's own arithmetic — `no targets` / `all N current` / `M of N
  behind`, and which rows survive the default filter — is the one piece of
  screen logic with a wrong answer rather than a wrong look, so it is unit
  tested apart from the component.
- Web tests, and the e2e smoke extended to set `notify_to` and observe a target
  reach `current`.

#### 9.10.10 Deliberately not in D4

The journal, deltas and real IXFR (D5). The `forwarder` zone type (D6). A
manual notify button (§9.10.8). SOA probing on the scheduled refresh path
(§9.10.3) — a real improvement, deliberately not folded into a NOTIFY
milestone. NOTIFY over TCP: RFC 1996 §3.6 describes the UDP case and permits
TCP, and nothing in a homelab needs a NOTIFY too large for a datagram, so the
responder accepts one on either transport but the sender only ever uses UDP.
And a per-target notify *delay*, BIND's `notify-delay`: the pass ticker already
bounds how often a target can be told, so a second timer would be two
mechanisms for one job.

#### 9.10.11 Known follow-ups, carried out of D4

Recorded here rather than in a scratch file so they survive the milestone.
Each was found by review, judged real, and deliberately not fixed in D4.

- ~~**`Transferrer.fetch` accepts an unsigned AXFR reply**~~ — **withdrawn
  2026-09-03: the claim was wrong, and the code was already correct.** D4's
  final review asserted that `xfr.go` "also only verifies a TSIG that's
  present, exactly as `client.go:267` does". It does not. The two read paths
  differ on precisely this point: `dns.Client.ReadMsg` gates verification on
  `if t := m.IsTsig(); t != nil`, while `Transfer.ReadMsg` calls
  `TsigVerifyWithProvider` **unconditionally** once a provider is set — so an
  unsigned reply reaches `stripTsig`, which returns `ErrNoSig` on
  `Arcount == 0`, and the envelope carries that error.
  `TestTransferRejectsAnUnsignedReplyToASignedRequest` pins it.

  Worth recording rather than quietly deleting, for two reasons. The review
  that found a real unsigned-reply hole in `probeOne` generalised from it to a
  second function without reading that function's read path — the same
  unverified-assertion failure the milestone spent thirteen tasks catching,
  committed by the review that caught them. And the property now depends on an
  asymmetry between two functions in one library, which is exactly what a
  dependency bump can align without anyone noticing; the pin exists so that
  would fail a test rather than silently open the hole.
All five landed on 2026-09-05, one commit each, and are struck through
below. The statement of each problem is left standing rather than deleted:
this section is the record of what D4 knew and carried, and a list that
erases the finding once it is fixed cannot be read back as one.

- ~~**`BumpSerial` cannot wrap.**~~ — **fixed 2026-09-05 (`b770be2`).**
  `soa_serial = soa_serial + 1` on a zone at
  4294967295 writes 4294967296, and every later `scanZone` into `uint32` fails
  with a range error, taking the zone out of service. Pre-existing since
  Milestone A. It undercuts D4's own wrap story: `SerialNewer` is careful
  across the wrap and the import path wraps correctly, while the commonest
  serial-advancing path in the product cannot reach 0 at all.
  The increment is now `(soa_serial + 1) % 4294967296`, which is the modulo
  operator in both dialects and keeps the increment atomic in SQL.
  `TestBumpSerialWrapsAtMaxUint32` runs on both drivers and asserts the
  read-back, since the failure was an unreadable row rather than a wrong
  number.
- ~~**`NotifyServer`'s work goroutine is detached from `App.wg`.**~~ —
  **fixed 2026-09-05 (`6f00e3a`).** A NOTIFY
  admitted near shutdown can probe or transfer against a closed store.
  Bounded to a confusing log line — the transaction rolls back atomically and
  nothing panics or hangs — but the lifetime is genuinely unmanaged.
  `NotifyServer.Run` is now that lifetime, in `a.bg` like every other worker
  App owns: on shutdown it stops admitting work, cancels what is running and
  waits for it. Two tests — the mechanism in `internal/zones`, the wiring in
  `internal/app` (both drivers), where the lifetime is actually owned.
- ~~**`notifyStateOf` ignores `pending_serial`, so the API and the pass
  disagree about "gave up".**~~ — **fixed 2026-09-05 (`713c382`).**
  `maybeSend` scopes give-up to the *round* and resets
  `attempts` when the serial advances; the API scopes it to the *target* and
  does not. A target that exhausted its budget at serial 100 and then sees 101
  reads `gave_up` on screen for up to the remaining back-off while the
  notifier considers it a fresh round. Not a lie — the target genuinely has
  not acknowledged — but the two components mean different things by one word.
  **The round won**: it is what the pass acts on, and it is what `docs/api.md`
  already documented ("it starts over from `attempts: 0` on the next serial
  bump"), so the code was the outlier. `attempts` is scoped with it, or the
  screen would read "retrying · try 5/5" against a serial nothing has been
  tried for. `never` still reads `attempts` raw: it is the one state the
  dashboard does not count as behind, so it has to keep meaning "nothing has
  ever happened here".
- ~~**`isTransferQuery` does not check the opcode**~~ — **fixed 2026-09-05
  (`81ff78d`)**, so a message with
  `Opcode == NOTIFY, Qtype == AXFR` routes to `Transfers` rather than
  `Notifies`. The ACL still applies, so it is not a hole; it is now two
  branches deep in `serve` and the next person adding a third will not
  re-derive it. The branch now requires `Opcode == QUERY`, with the mirror
  test — an ordinary AXFR still reaching `Transfers` — so the fix cannot
  degenerate into routing everything to `Notifies`.
- ~~**`Reconcile` opens a transaction per enabled zone per pass**~~ — **fixed
  2026-09-05 (`a349f13`)**, including the
  common case of a zone with no targets at all. With the RFC 6303 built-ins
  seeded, that is roughly twenty read-only transactions every five seconds on
  an install using no NOTIFY at all. **Batched in the pass, not skipped in the
  store**: the pass reads the queue once up front — a query it already made,
  moved earlier — and calls `Reconcile` only for the zones whose rows
  disagree with their `notify_to`, so the store-side alternative's per-zone
  `SELECT` is avoided too and `Reconcile` stays exactly as atomic for the
  calls that remain. It re-reads the queue after reconciling, and only then,
  because sending against the pre-reconcile snapshot is the `WHERE id = 0`
  bug the two-phase order exists to prevent.

### 9.11 Milestone D6: `forwarder` and `stub` zones

Designed 2026-09-03, after D4 landed. §9.7's sketch is superseded by this
section, and one of its claims is withdrawn below.

#### 9.11.1 What §9.7 got wrong

§4 and §9.7 both say the `Conditional` config key is "migrated into a zone row
so there is one place a suffix is claimed rather than two that can disagree".
**There is no config key.** `defaultSettings()` carries `upstreams` and
`upstream.strategy` and nothing else, the API's settings validator accepts the
same two, and `buildForwarder` constructs `upstream.Config` with only
`Upstreams` and `Strategy`. `Conditional` has four references in the whole
repository — the struct field and the three lines of `New` that read it.

So there is no migration, and there never were two places that could disagree:
there is one, and it is empty. `upstream.Config.Conditional` is a complete,
tested capability with no producer. D6 is purely additive — it writes the
producer.

#### 9.11.2 One mechanism, two sources

Both types answer the same question — *given this suffix, which addresses do
its queries go to?* — and differ only in where the addresses come from.

| | Suffix | Addresses |
|---|---|---|
| `forwarder` | the zone's apex | `forward_to`, typed by the operator |
| `stub` | the zone's apex | the zone's NS records, fetched from `primaries` and resolved |

One routing path, one map, one set of tests for matching. A stub is a
forwarder whose address list is derived rather than typed.

Neither type holds data the operator authors, so `Zone.Answer` keeps
returning `false` for both and the resolver middleware keeps falling through.
That is not a placeholder — it is what routes the query through the **cache**
on its way to the forwarder, and §9.11.4 explains why that decides the whole
design. The middleware's "until that lands" comment is replaced with one
saying the fall-through is deliberate and permanent.

#### 9.11.3 Schema

Migration **0013**. The next free version is not derivable from the directory
listing: 5 and 7 are Go migrations in `migrate.go` with no SQL file, and 0012
is D4's.

```sql
ALTER TABLE zones ADD COLUMN forward_to TEXT NOT NULL DEFAULT '';
```

One column, and `stub` reuses `primaries` rather than getting its own.

**That reuse is not the overloading D2 refused.** D4 declined to let
`tsig_key_id` mean one thing on a primary and another on a secondary, because
its meaning would have flipped by zone type. `primaries` does not flip: for a
secondary it is "the server I pull this zone from", and for a stub it is "the
server I fetch this zone's NS set from". Same sentence, smaller payload.

`forward_to` uses the `host[:port]` grammar `ParsePrimaries` and
`ParseNotifyTo` already implement. **The third copy is the one that stops
being a copy**: the shared host-and-port parse is extracted into one function
all three call, with their distinct rules — `primaries` resolving at use,
`notify_to`'s per-target `key:`, `forward_to`'s plain list — layered on top.
D4's review flagged this duplication when the second copy landed; a third
without extracting it would be choosing to have the problem.

#### 9.11.4 Routing: a swappable table, and why not the alternatives

`Forwarder` gains one method:

```go
// SetConditional replaces the suffix routing table without disturbing the
// default upstreams, their health state, or the failure cache.
func (f *Forwarder) SetConditional(routes map[string][]string) error
```

`New` keeps taking `Config.Conditional` and delegates to it, so there is one
construction path.

`condSet` and `condRoutes` move behind an `atomic.Pointer` to an immutable
`condTable`, built fresh and swapped — `Resolver.snap`'s shape, for
`Resolver.snap`'s reason: a reader on the query path must never take a lock,
and a rebuild must never mutate a table someone is reading.

**Two rejected alternatives, recorded because both look reasonable:**

*Dispatching from the resolver middleware* — having the middleware call a
per-zone forwarder directly — puts routing next to the zone that declares it
and leaves the global forwarder untouched by zone edits. It also **loses
caching entirely**: the middleware order is `resolver → cache → forwarder`
(`app.go`), so a resolver that answers is never seen by the cache. Every
repeat query to an internal zone would re-ask the corporate resolver.

*Rebuilding the whole forwarder on zone change* is the smallest diff and the
worst behaviour. Every record edit calls `reloadZones`, and a rebuild discards
each upstream's latency EWMA, its 15-second health backoff after three
failures, and the RFC 9520 failure cache. Editing records would degrade
resolution.

**`*up` values are reused across swaps, keyed by address.** A new table takes
the existing `*up` for an address it already knows and mints one only for an
address it does not. Without this the swap preserves health state for the
default upstreams and discards it for exactly the conditional ones most likely
to be flaky, which is the problem the swap exists to solve, one level down.

**Wiring.** `reloadZones` gains a step: walk the snapshot, collect
`apex → addresses` for every **enabled** `forwarder` and `stub`, call
`SetConditional`. A disabled zone contributes nothing, so disabling a
forwarder zone genuinely releases its suffix back to the global upstreams —
consistent with every other path, where disabled means "this server is not
handling that name".

#### 9.11.5 Failure is SERVFAIL, and it is already implemented

A zone that claims a suffix keeps its claim when its upstreams are down. The
query gets SERVFAIL; it does not fall through to the default resolvers.

This is the rule §9.8 sets for an expired secondary, applied to a
configuration failure instead of a data one: a server that cannot answer for a
name it has claimed must not let the public internet answer instead, because a
split-horizon zone would then resolve to whatever the outside world says it is.

**`pick` was necessary and not sufficient.** A matched suffix returns
`f.condRoutes[matched]` and never falls back to `f.def`; when every upstream
in that list fails, the handler returns SERVFAIL on the RFC 9520 path. That
much the forwarder has had since it was written, and this section was
originally written believing it was the whole of the rule.

It is not, because **the cache sits above the forwarder**. The pipeline is
`zones → cache → forwarder` (§9.11.4's fall-through is what puts it there, on
purpose), an entry is keyed on `(qname, qtype)` with no record of which route
produced it, and a hit is answered without `pick` being reached at all. So a
name cached from the *default* resolvers before a zone claimed its suffix went
on being served from that entry afterwards; and when the claimed suffix's own
upstreams then failed, the cache served that public entry **stale** — NOERROR,
TTL 30, for `cache.serve_stale_for` (a day, by default) past its own TTL, with
every SERVFAIL in the window re-serving it. That is precisely the leak this
section forbids, and it was reachable through the type's own motivating
workflow: you claim a suffix *because* the name resolves publicly today, which
is what put the public answer in the cache at the moment you claimed it. The
same for re-enabling a disabled zone and for delete-then-recreate. A `primary`
never had it (`Zone.Answer` returns true, so the cache is never consulted); it
is specific to the two types D6 adds.

So the rule takes one piece of code after all. `Cache.Purge` drops a suffix's
entries, and `App.installConditional` calls it for every suffix whose route
set changed — added, removed or altered — after installing the table and never
before, since purging first would leave a window in which a miss re-fills the
cache from the defaults. It also bumps an epoch, which does two jobs the sweep
alone cannot: `put` compares against it, so a lookup that reached the
upstreams before the claim existed cannot store its pre-claim answer after the
sweep has passed; and the epoch is part of the singleflight key, so a query
arriving *after* the purge is not collapsed onto a leader that went out before
it and handed that leader's answer from the old route. Keyed on name and qtype
alone, that group was route-blind, and a query with no connection to the old
route at all — issued after the claim, against an empty cache — was answered
by it.

What holds now, stated exactly:

- A claimed suffix is never answered from an entry the default resolvers
  produced. Releasing a suffix is the mirror and is closed the same way: the
  internal answers go too, so a name that should now resolve publicly does.
- With nothing cached beneath it, a claimed suffix whose upstreams all fail is
  SERVFAIL, which is the sentence at the top of this section.
- With something cached beneath it **that the zone's own upstreams produced**,
  serve-stale applies exactly as it does to any other forwarded answer. That
  is not a fall-through and not an exception to the rule: the reply is that
  zone's own data going stale, never the outside world's. An operator who
  wants it gone sets `cache.serve_stale_for` to 0.
- **The residual is one upstream round trip, not one cache lookup.** What
  survives is a query that was already *somewhere in the pipeline* when the
  routing changed, and the longest of those has already gone out to the old
  route and is waiting on it. Three of them, all bounded by the forwarder's
  own 2s timeout and none of them able to store anything (`put` refuses a
  stale epoch):

  - a query that had already read the cache and *hit*, answered from the
    entry it read before the sweep removed it — this one really is a cache
    lookup wide;
  - a query that had already read the cache and *missed*, and is out at the
    old route: it is answered by that route, one round trip later;
  - a query landing between `SetConditional` and `Purge`, which finds the
    new table and the old entries. The install-then-purge order is still the
    right one — purging first would leave a window in which a miss re-fills
    the cache from the defaults and the wrong answer then lasts a whole TTL —
    but it is a window, and the honest thing is to name it rather than to
    describe the order as closing everything.

  Closing any of them means serialising the cache read against the routing
  swap, which is a lock on the query path.

  The *stored* state is not affected by any of this: after the purge, nothing
  under a claimed suffix can be an entry the defaults produced. What the three
  windows leak is a single in-flight answer to a single client, once.

The consequence worth stating plainly, because it is surprising: a
misconfigured `forwarder` zone makes its whole suffix stop resolving. That is
the correct trade and the screen says so rather than leaving it to these docs.

#### 9.11.6 Stub: two queries, not a transfer

A stub is a secondary that keeps only the apex. Two ordinary queries to the
master:

1. `<apex> SOA` — serial, refresh, retry, expire. Drives the schedule.
2. `<apex> NS` — the delegation, with glue in ADDITIONAL.

Not an AXFR, which is the point: a stub needs no `allow_transfer` permission
on the far end. Both queries are signed when the zone names a TSIG key — the
artboard offers a key on stub creation alongside secondary, and a master that
requires TSIG on ordinary queries would otherwise refuse the fetch.

Installed through `ReplaceRecords`, exactly as a transfer installs a zone: SOA
fields onto the zone row, NS records into `zone_records`. Persisting them
means the set is visible on the zone page, survives a restart with no
cold-start window, and reuses the atomic install rather than inventing a
second one.

**Scheduling reuses `Refresher` rather than growing a second scheduler.** It
already does per-zone locking, retry back-off, `last_error`/`last_attempt`
recording and `refreshed_at` stamping. `isSecondary` widens to admit stub, and
`transfer()` branches on type — `Transferrer.Transfer` for a secondary, a new
`StubFetcher.Fetch` for a stub.

#### 9.11.7 The loop a stub must not take

A stub for `corp.example` fetches `ns1.corp.example` as its nameserver.
Resolving that name matches the stub's own suffix, routes into the stub zone,
and needs the address being resolved. Infinite regress, and it would present
as a hang rather than an error.

DNS's own answer is glue, and it is the right one:

- **In-zone nameserver** (the name ends with the apex): the address **must**
  come from glue in the master's ADDITIONAL section. No glue, no usable
  nameserver — it is skipped. If every nameserver is skipped the zone has no
  upstreams, keeps its claim, and SERVFAILs per §9.11.5. It is never resolved.
- **Out-of-zone nameserver**: resolved normally. It cannot re-enter this zone,
  because the suffix differs.

The guard needs a test that would **hang** without it, not one that merely
passes with it.

**Out-of-zone names resolve through `net.Resolver`**, the same resolver
`ParsePrimaries` uses for a hostname primary. Consistent with the existing
precedent, and an out-of-zone nameserver like `ns1.example.net` is normally
public. The cost is that a stub whose nameservers are named only inside
another internal zone will not resolve them; the alternative — routing through
dnsaur's own default upstreams — needs a bypass so two stubs naming each
other's servers cannot cycle, and buys a case nobody has asked for.

#### 9.11.8 A stub does not expire

A secondary past its SOA expire stops answering (§9.8, RFC 1034 §4.3.5). A
stub does not, and the divergence is deliberate.

A secondary's records are data it serves and can no longer vouch for. A stub's
NS set is *routing information* — where to ask — and an old-but-working
nameserver beats a self-inflicted SERVFAIL. If those nameservers really are
gone, §9.11.5's path returns SERVFAIL anyway: the outcome is the same when it
should be, and better when it should not.

The screen says so rather than hiding it: a stub whose last fetch failed reads
`serving the NS set from 3 days ago`, with the failure and its date beside it.

#### 9.11.9 What the operator sees

The artboards (Zone Detail and Zones, both updated 2026-09-03) are the source
of truth. What they settle:

**One row serves both types**, its label switching — `FORWARD TO` for a
forwarder, `MASTER` for a stub — in the same 152px gutter as `TRANSFERS OUT`
and `NOTIFY OUT`, read by default with a pencil.

**Every band that assumes authored data is dropped** for both: no SOA band, no
create-record row, no record filter, no allow-transfer, no notify row. A
forwarder additionally drops the records grid entirely; a stub keeps it,
read-only, because its NS set is worth seeing.

**A forwarder's page is short, and that is the answer to "does it look
broken".** It carries the header, the `FORWARD TO` row, zone actions, and a
line stating the consequence: *with every upstream unreachable, queries for it
get SERVFAIL — they do not fall through to the default resolvers.* Surfacing
the surprising behaviour on screen is what stops the short page reading as an
incomplete one.

**A stub has three states**, all dated: `NS set fetched 26 minutes ago`,
`no NS set yet` while the first fetch is outstanding, and on failure the error
verbatim under `LAST FETCH · 12 MINUTES AGO` with `serving the NS set from 3
days ago`. Never a state without its date — `TransferBand`'s rule.

**A stub can be refreshed on demand; a forwarder cannot** — the artboard's
`Refresh: secondary || stub` is the source of truth, and it is right: a
forwarder has no master and nothing to fetch, so the action would be a no-op
wearing a button. An earlier draft of this section said "both types", which
contradicted the artboard it cites. A stub can also be exported (it has
records); a forwarder cannot. The create row offers all four types with one
type-dependent field, and a stub may name a TSIG key.

#### 9.11.10 Test posture

- The `host[:port]` extraction is refactor-first: the existing
  `primaries_test.go` and `notifyto_test.go` tables must pass unchanged before
  `forward_to` uses the shared parser. A behaviour change to either format is
  a defect, not a merge conflict.
- `SetConditional` under concurrent reads, with `-race`: a swap during
  in-flight queries must never tear, and the reused-`*up` path must preserve
  health state across a swap. This is the one place a concurrency assertion
  must defeat the temptation to test it single-threaded.
- The glue guard: a stub whose in-zone nameserver has no glue must skip it and
  SERVFAIL, in a test that would hang if the name were resolved instead.
- One test per row of §9.11.2's table — that a forwarder routes to its typed
  addresses and a stub to its fetched ones, through the same `pick`.
- That a disabled forwarder zone releases its suffix, and an enabled one with
  unresolvable upstreams keeps it and SERVFAILs.
- Store tests under `forEachDriver`; API tests for the two new types on create
  and patch, with `forward_to` refused on any type but `forwarder`.
- Web tests for the two page shapes, and the e2e smoke extended to create a
  forwarder zone.
- Every fix's test shown failing with the fix removed.

#### 9.11.11 Deliberately not in D6

IXFR (D5). Per-zone forwarding *strategy* — the global `upstream.strategy`
applies to conditional routes as it does to defaults, and nothing has asked
for more. Stub zones serving anything but NS. A forwarder zone holding records
of its own, which would make it a primary with a fallback and is a different
feature. And any conditional-forwarding **setting**: the zone row is the one
place a suffix is claimed, which is what §4 wanted and never had.
