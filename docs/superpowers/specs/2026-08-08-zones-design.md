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
Outbound: AXFR/IXFR client honouring SOA refresh/retry/expire, `expires_at`
enforcement. Inbound: serving AXFR/IXFR to secondaries behind an allow-transfer
ACL. NOTIFY in both directions. TSIG keys as a first-class resource, since
transfers without them are unauthenticated.

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
