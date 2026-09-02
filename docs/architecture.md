# Architecture

See also: [`README.md`](../README.md) ·
[`docs/superpowers/specs/2026-08-03-dnsaur-design.md`](superpowers/specs/2026-08-03-dnsaur-design.md)
for the full design rationale.

dnsaur is a single Go binary. It runs a DNS engine, a storage layer,
background jobs, and a REST API + auth server on top of the same storage
layer (see [`docs/api.md`](api.md)). The web dashboard (`web/`, a React
SPA) is built separately but embedded into that same binary via
`//go:embed` and served by the same HTTP server as the API — there is no
separate frontend process or listener. Planned services — DHCP, HA config
sync — are additional listeners that feed the same internal packages
rather than separate processes.

## The middleware pipeline

Every DNS query is handled by an ordered chain of middleware wrapping a
terminal upstream forwarder. Each stage can answer the query outright
(short-circuit) or pass it to the next stage:

```
                 ┌─────────────────────────────────────────────────────────┐
 UDP/TCP :53 ───►│  qlog  →  recovery  →  client-id  →  filter  →  zones   │
                 │  →  cache  →  upstream forwarder                        │
                 └─────────────────────────────────────────────────────────┘
```

1. **qlog** — wraps the whole chain; records the outcome (client, qname,
   qtype, decision, upstream, latency, rcode) to a buffered async channel.
   Logging never blocks resolution.
2. **recovery** — recovers panics from any inner stage into a `SERVFAIL`
   response instead of crashing the server. One bad query never takes down
   the resolver.
3. **client-id** — maps the requester IP to a known client (name, group) via
   the client registry. Unknown IPs fall into the default group.
4. **filter** — checks the qname against the client's group blocklists,
   allowlists, and regex rules. A block returns a configured response
   (null IP or NXDOMAIN) and is tagged `blocked` in the log. Rules are
   evaluated before lists and allow before block — see
   [`dashboard.md`](dashboard.md#the-order-that-matters) for the full
   six-stage precedence.
5. **zones** — answers authoritatively for the suffixes this server holds,
   before the cache or any upstream is consulted, and tags the result
   `authoritative` in the query log. This is a zone cut, not a set of
   overrides: once a zone claims a name, that name is *never* forwarded.
   It either answers, or returns NODATA (name exists, wrong type) or
   NXDOMAIN (name absent), each carrying the zone's SOA so resolvers cache
   the absence. A qname no zone claims passes straight through untouched.
   The same stage answers reverse (`PTR`) queries — an `in-addr.arpa` or
   `ip6.arpa` name is just another apex a zone can claim, forward and
   reverse are not different code paths. A fixed set of zones is always
   present — `localhost` plus every RFC 6303 §4 reverse zone except the
   private ranges (`BuiltinZones` in `internal/store/builtins.go`) —
   seeded at migration so those names never reach an upstream; every
   write to one of them is refused with `409` at the API layer instead.
   Future DHCP-registered hostnames register into a zone at this stage.
   See [`dashboard.md`](dashboard.md#zones) for the user-facing rules.
6. **cache** — in-memory cache keyed on (qname, qtype), respecting upstream
   TTLs with configurable min/max clamps, negative caching, and
   serve-stale-on-failure with background refresh.
7. **upstream forwarder** — the terminal handler; sends unresolved queries
   to configured upstreams with a selectable strategy (race today; failover
   and fastest planned).

Stages implement a small `Handler`/`Middleware` Go interface
(`internal/dnssrv`), so each one is unit-testable in isolation and new
stages are purely additive — no rewiring of existing ones.

## Zone transfers (AXFR out)

Serving a zone to another nameserver does not go through the pipeline
above. `dnssrv.Handler` returns exactly one `*Response`, and `serve` writes
exactly one message — but an AXFR answer is a *sequence* of messages on one
TCP connection, which that one-response shape cannot express. So
`Server.serve` branches on qtype AXFR/IXFR before a `Request` is even
built, and hands the raw `dns.ResponseWriter` straight to a `Transfers`
handler (`internal/zones.TransferServer`, wired in via `dnssrv.WithTransfers`
in `internal/app`). This is an intercept ahead of the chain, not a
pipeline stage.

It's also correct on the merits, not just a shape workaround: none of
qlog's per-query accounting, the filter, the cache or the upstream
forwarder mean anything for a transfer, and a transfer needs its own
multi-minute deadline (2 minutes, checked between envelopes) rather than
the pipeline's 5-second one.

**A transfer never appears in the query log**, as a direct consequence.
qlog wraps the whole middleware chain (step 1 above); the intercept runs
before that chain is ever entered, so nothing about an AXFR or IXFR —
served or refused — reaches it. An operator hunting a secondary's transfer
in `GET /queries` will not find it there: `last_xfr_at`/`last_xfr_peer`/
`last_xfr_error` on the zone ([`docs/api.md`](api.md)) and the server's own
log lines (`"zone transfer served"` / `"zone transfer refused"`) are where
it shows up instead — except that the zone fields record only a request
that arrived over TCP; a UDP arrival (an AXFR answered `NOTIMP`, an IXFR
answered a single SOA, or a UDP peer the ACL refuses) only ever reaches the
log line, never the zone row. A zone transfer is a TCP protocol, so that is
the whole of what a UDP arrival is: a peer using the wrong transport, not a
transfer attempt.

What can be transferred: an enabled `primary` zone in full, or an enabled
`secondary` zone while it is currently serving — never before its first
successful pull, and never past its SOA `expires_at`. `internal`, `stub`
and `forwarder` zones hold nothing to send and refuse every request. The
zone handed over is read from the same served snapshot the query path
answers from, not a fresh database read, so a peer can't drive store load,
and a transfer racing a reload sees one consistent zone rather than a
mixture.

The gate answers with one of five rcodes, and the first three are what an
operator debugging a secondary that has stopped updating actually needs to
read:

| Rcode | Means |
|---|---|
| `NOTAUTH` | dnsaur doesn't hold that zone — wrong or disabled apex, or a type (`internal`/`stub`/`forwarder`) that holds no data to send — or the request's TSIG didn't verify |
| `REFUSED` | dnsaur holds the zone, but the peer's address or key isn't in its `allow_transfer` |
| `SERVFAIL` | the zone is a `secondary` dnsaur can't currently vouch for (nothing transferred yet, or past `expires_at`), or the server is already at its concurrent-transfer limit |
| `NOTIMP` | the AXFR arrived over UDP, which RFC 5936 §4.2 leaves undefined. Checked *after* every row above, so a peer outside the ACL is still told `REFUSED` rather than told about the transport |
| `FORMERR` | the query isn't a well-formed transfer request — in practice, a class other than IN. The other half of that rule, a question count other than one, is answered `FORMERR` by miekg's own accept function before dnsaur sees the message at all |

A TSIG failure is a `NOTAUTH` with a TSIG error record attached naming
which check failed — BADKEY (unknown key or algorithm), BADSIG (signature
didn't verify), or BADTIME (outside the fudge window) — and the three
don't get signed alike. RFC 8945 requires the BADKEY and BADSIG replies to
go back **unsigned**: there's no verified key to sign with, so signing one
would assert an authenticity the server never established. BADTIME goes
back **signed**, because there the key and MAC *did* verify and only the
two clocks disagree — an unsigned clock report is something an off-path
attacker could forge to move a peer's clock the wrong way rather than fix
it.

See `docs/superpowers/specs/2026-08-08-zones-design.md` §9.5.5 for the
full gate, in order, with every row's reasoning.

## Zone NOTIFY (RFC 1996)

Like AXFR/IXFR above, NOTIFY does not go through the middleware pipeline.
`Server.serve` branches on it — `dns.OpcodeNotify` is an *opcode*, not a
qtype, so the transfer branch's qtype check does not catch it and this is a
second, independent branch ahead of the pipeline — and hands the raw
`dns.ResponseWriter` to a `Notifies` handler (`internal/zones.NotifyServer`,
wired in via `dnssrv.WithNotifies`). Unlike a transfer, a NOTIFY reply
*could* fit through `Handler`'s one-`*Response` shape mechanically, so the
justification here is not the same shape mismatch as AXFR's. It still must
not reach the chain, and getting this wrong had a concrete, shipped
consequence rather than a theoretical one: **before this branch existed, a
NOTIFY arriving at dnsaur was handled as an ordinary SOA query** —
query-logged, filtered, and, for an apex the server does not hold,
cacheable and forwarded upstream to the configured resolver, so dnsaur
asked its own upstream an SOA question on behalf of a peer that was trying
to notify *it*. `internal/dnssrv/notifies.go`'s `Notifies` doc comment
records this as the reason the interface exists, not as a nice-to-have.

**Inbound: who may notify this server, and what it says back.**
`NotifyServer.ServeNotify` runs the cheap, purely local checks first —
question shape, apex, zone type, enabled — and only *then* resolves the
zone's `primaries` to check the sender against them. That ordering is
load-bearing, not tidiness: `ParsePrimaries` may do a live, uncached DNS
lookup for a hostname primary, and doing that before the local checks would
let one unauthenticated, trivially spoofable UDP packet drive an outbound
recursive lookup for a NOTIFY that was going to be refused anyway. The gate,
in order:

| Rcode | TSIG error | Means |
|---|---|---|
| `FORMERR` | — | not a single SOA question |
| `NOTAUTH` | — | no zone at that apex, the zone isn't type `secondary`, or it's disabled |
| `REFUSED` | — | the peer's address isn't one of the zone's configured `primaries` |
| `REFUSED` | `BADKEY` | the TSIG names an unknown key or algorithm |
| `REFUSED` | `BADSIG` | the TSIG signature didn't verify |
| `REFUSED` | `BADTIME` | the TSIG timestamp is outside the fudge window |
| `REFUSED` | — | the zone requires TSIG (`tsig_key_id` set) and the message carries none |
| `SERVFAIL` | — | this server's own TSIG check couldn't run — no key store attached, or a key lookup itself failed |

Two things in that table are deliberate, not incidental:

- **The peer-not-a-primary row answering `REFUSED` is a deliberate
  divergence from RFC 1996 §3.10**, which says to *ignore* such a request.
  dnsaur answers instead — recorded as a conformance exception in
  `docs/superpowers/specs/2026-08-08-zones-design.md` §9.9. Silence is
  indistinguishable from a firewall drop, the usual cause of this row is a
  `primaries` list with one address wrong, and a NOTIFY and its refusal are
  both ~50 bytes, so answering costs nothing an attacker could turn into
  amplification.
- **The three TSIG rows (`BADKEY`/`BADSIG`/`BADTIME`) fire whether or not
  the zone names a key at all** — they are *not* gated on `tsig_key_id`
  being set. RFC 8945 §5.2.1/§5.2.2 make them mandatory regardless of local
  policy: gating them would let a NOTIFY carrying a broken MAC to a keyless
  zone be *accepted* unsigned, which the sender then has to discard and
  retransmit forever under RFC 1996 §3.6. The zone's own key requirement
  decides only the *unsigned*-message row — a keyless zone accepts a NOTIFY
  carrying no TSIG at all; a keyed one refuses it.

The `SERVFAIL` row is its own deliberate choice, not a leftover default:
`dnssrv.ErrTSIGUnavailable` (no key store attached to this server at all)
answers `SERVFAIL`, not `REFUSED` — this server has nothing that could have
verified the message, so none of this is the peer's fault, and answering
`REFUSED` would send an operator debugging an otherwise-correct peer to the
wrong end of the connection. This mirrors `TransferServer.decide`'s
identical treatment of the same error value (D3, above). A `BADKEY`/`BADSIG`
TSIG error record goes back **unsigned** and `BADTIME`'s goes back
**signed** — the identical rule the AXFR/IXFR table above uses, reused from
the same code (`errorTSIG`, `signIfVerified`) rather than reimplemented.

**Inbound sequence, once a NOTIFY is accepted:** reply, throttle, probe,
compare, transfer.

1. **Reply first, before any work** — an authoritative `NOERROR`, signed if
   the request verified. RFC 1996 §4.7 has the responder enter its refresh
   state on receipt and §3.6 has the sender retransmitting until it gets
   *any* response, so a responder that waited for the transfer to finish
   before replying would earn itself a second NOTIFY for the transfer
   already in flight.
2. **Throttle** (5s per zone): a primary editing ten records sends ten
   NOTIFYs; each gets its own immediate reply, and they collapse to one
   probe. Without this, "NOTIFY is cheap" becomes a probe amplifier pointed
   at dnsaur's own configured primary.
3. **Probe**: an SOA query against the zone's primaries checks whether they
   are actually ahead, skipped only when no probe is wired in or for a zone
   that has never transferred (see below, where a serial comparison would
   be meaningless).
4. **Compare**: a serial comparison (RFC 1982). Not newer means the NOTIFY
   was true and the zone was already current — RFC 1996 calls a NOTIFY a
   hint, and this is that hint being correctly declined; nothing
   transfers.
5. **Transfer**: only once the probe shows the primary genuinely ahead does
   the zone actually get pulled.

A zone that has never transferred skips the probe and compare steps and
transfers unconditionally on the first accepted NOTIFY. A freshly created
secondary starts at `soa_serial 1`; if its primary also happens to start at
`1`, a serial comparison would say "not newer" and leave the secondary
permanently empty while reporting nothing wrong — the very first transfer
has no honest baseline to be gated against.

**Outbound: the queue, and why the trigger is serial detection, never call
sites.** `internal/zones.Notifier` runs on its own tick (5s) plus an
explicit `Wake()`, and reconciles a durable queue (`zone_notifies`, the
0012 migration) against every enabled zone's `notify_to`. That queue carries
no "pending" column at all. Whether a target needs telling is *derived*,
every pass, by comparing the zone's current `soa_serial` against what that
target last acknowledged — never stored as a flag some code path has to
remember to set.

This is the design decision most likely to look "simplifiable" into an
enqueue call at each place a zone changes, and it is not simplifiable that
way on purpose. An enqueue-at-the-call-site design has exactly as many
chances to be *wrong* as there are places that can bump a serial — the
ordinary API write, a zone-file import, auto-PTR, a transfer installing a
new copy of a secondary's zone, and any path nobody has written yet — and
missing one produces a target that silently stops hearing about changes
made through it. Deriving pending-ness from the serial comparison instead
means every one of those paths is covered with no call to forget, because
none of them has to know the `Notifier` exists at all.
`docs/superpowers/specs/2026-08-08-zones-design.md` §9.4 names exactly this
shape — an automatic path skipping a rule the human path enforces — as this
project's most repeated defect, and the serial-derived queue is the
deliberate answer to it here.

`Notifier.Wake()` (`internal/app.App.NotifyZones`, called from several
`internal/api` write paths — zone create/patch/delete, record
create/update/delete, auto-PTR, zone-file import — after each one's own
write lands) only ever affects *promptness*, never correctness: it asks for
a pass before the next tick, so any one of those call sites forgetting to
call it only changes how many seconds early a NOTIFY goes out for that
write — nothing can be lost, because the next tick (at most 5s later)
re-derives the same pending set from the serial regardless, with no call
site involved at all. A round is
retried up to 5 times, backing off 5s/10s/20s/40s/80s (about two and a half
minutes total) and then rests; that budget is deliberately small because it
is not load-bearing for correctness either — the target's own SOA `refresh`
timer picks the zone up regardless of how a round ends, which is what keeps
NOTIFY a delivery optimisation rather than something dnsaur has to get
right to stay correct.

## Package map

| Package | Responsibility |
|---|---|
| `cmd/dnsaur` | Entry point: flag/config parsing, wiring, graceful shutdown |
| `internal/app` | Top-level app object: builds the pipeline, owns settings hot-reload and background jobs |
| `internal/config` | Bootstrap YAML + env config loading and validation |
| `internal/dnssrv` | DNS listeners, the `Handler`/`Middleware` pipeline abstraction, panic recovery, and TSIG (RFC 8945): a `dns.TsigProvider` that verifies signed messages against the stored keys on every message, plus `RequireTSIG` for the paths that must refuse an unsigned one; also the AXFR/IXFR intercept that routes a transfer's raw `ResponseWriter` to a `Transfers` handler ahead of the pipeline, and the NOTIFY opcode intercept that routes to a `Notifies` handler the same way (see Zone transfers and Zone NOTIFY above) |
| `internal/clients` | Client registry: IP/CIDR matching to client + group |
| `internal/filter` | Blocklist/allowlist engine, list parsing, per-client-group rules, background refresh |
| `internal/zones` | Authoritative zones: zone cut and deepest-match lookup, apex-relative names, RR construction from stored presentation-format rdata, and the NODATA/NXDOMAIN/wildcard/CNAME/referral answering rules; also `TransferServer`, which answers AXFR/IXFR requests the `allow_transfer` ACL permits; `NotifyServer`, which answers inbound NOTIFY and probes/transfers on it; and `Notifier`, which drives the outbound NOTIFY queue (see Zone NOTIFY above) |
| `internal/cache` | In-memory DNS response cache (TTL clamps, negative caching, serve-stale) |
| `internal/upstream` | Upstream forwarders and selection strategy |
| `internal/qlog` | Async query logging and retention pruning |
| `internal/stats` | Hourly stats rollups from the query log |
| `internal/store` | Storage interfaces plus SQLite/Postgres implementations, migrations, settings |
| `internal/api` | HTTP REST API server + handlers (`/api/v1`: setup, settings, blocking, groups, clients, filters, zones, queries, stats, tokens), embedded OpenAPI 3.1 doc; also mounts the web dashboard's static files (`internal/api.StaticHandler`) on every non-`/api` path when `Deps.Static` is set. The static mount — and only the static mount, so SSE on `/api/v1/queries/tail` stays unbuffered — gzips responses and sets `Content-Security-Policy` (`frame-ancestors 'none'`), `X-Content-Type-Options: nosniff`, `Referrer-Policy: same-origin`, and `X-Frame-Options: DENY` |
| `internal/auth` | Auth service: argon2id password hashing, session + scoped (read/write) API tokens, optional TOTP 2FA |
| `web` | The dashboard's Go-side glue: `//go:embed all:dist` over the React SPA's Vite build output, exposed as `web.Dist() fs.FS` for `internal/app` to hand to `internal/api.Deps.Static`. The actual frontend source (React 19 + TypeScript + Tailwind + TanStack Query, see `web/README.md`) lives under `web/src`, built independently (`pnpm build`) before the Go build embeds its output |

Planned, not yet present: `internal/dhcp` (Phase 2), `internal/sync`
(Phase 1.5 HA config sync).

## Storage model

- `internal/store` defines storage-agnostic interfaces (`Store`,
  `SettingsStore`, `ClientStore`, `FilterStore`, `ZoneStore`,
  `QueryLogStore`, `StatsStore`, `UserStore`, `TokenStore`); SQLite
  (`modernc.org/sqlite`, no CGO) is the default driver, Postgres (`pgx`) is
  an opt-in alternative, selected at startup by `storage.driver` /
  `storage.dsn`.
- Schema is managed by versioned, per-dialect SQL migrations
  (`internal/store/migrations/{sqlite,postgres}`), applied automatically at
  startup via `goose`. One of them is a Go migration rather than SQL
  (`internal/store/zonemigrate.go`): converting the pre-zones flat
  `local_records` table into zones needs to infer a zone apex per record,
  reconcile data a flat table allowed and a zone does not, and rewrite TXT
  values from literal strings into presentation format — none of which is
  expressible in SQL. Its `local_records` source rows are deliberately kept
  after the conversion, not dropped: the conversion alters data, so the
  originals are the only surviving record of what the user wrote. `Store`
  therefore still exposes `RecordStore` over that table, but nothing serves
  from it.
- Almost all runtime configuration lives in a `settings` key/value table in
  the database, not in the bootstrap YAML. `internal/app` seeds sane
  defaults on first run, and components subscribe to a change notification
  channel (`SettingsStore.Changes()`) so writes hot-reload live components
  without a restart. See [`docs/configuration.md`](configuration.md) for the
  full settings list and the handful of exceptions that still require a
  restart.
- The DNS cache and the compiled filter trie are memory-only — never
  persisted to the DB. Downloaded blocklist files are cached on disk so a
  restart doesn't force a re-download.

## "DNS must not die"

The guiding rule for error handling: DNS resolution degrades last, and
everything else is expendable before it.

- Upstream failures fail over to healthy upstreams, then serve-stale cache
  entries, and only return `SERVFAIL` as a last resort.
- Storage failures don't stop resolution — the filter trie and cache are
  in-memory — while DB-dependent API endpoints return `503` and the query
  log buffer drops oldest entries with a surfaced warning rather than
  blocking.
- A failed blocklist refresh keeps serving the previous compiled list and
  retries with backoff instead of going unfiltered or falling over.
- Bad configuration is validated and rejected at write time; the running
  config is always the last-known-good one.
- Pipeline panics are recovered per-query (`SERVFAIL` + logged) so one
  poisoned query can't take the whole server down.

This shows up concretely in `internal/app`'s settings reload logic: if a new
upstream configuration fails to build, it keeps the previous working
forwarder rather than replacing it with something broken, falling back
through progressively safer defaults only if no forwarder has ever been
installed.
