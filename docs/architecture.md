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
   Two zone types are the exception that proves the rule — a `forwarder`
   and a `stub` claim a suffix without holding any data for it, so
   `Zone.Answer` returns `handled=false` and the query goes on to the next
   stage deliberately and permanently. It is still never forwarded to the
   *default* upstreams: the forwarder stage below routes it to that zone's
   own, and passing through the cache on the way is the whole reason the
   routing lives there rather than here. See Conditional routing below.
   See [`dashboard.md`](dashboard.md#zones) for the user-facing rules.
6. **cache** — in-memory cache keyed on (qname, qtype), respecting upstream
   TTLs with configurable min/max clamps, negative caching, and
   serve-stale-on-failure with background refresh. An entry records no
   route, so a change to the conditional routing table below purges the
   suffixes it changed — see Conditional routing below.
7. **upstream forwarder** — the terminal handler; sends unresolved queries
   to configured upstreams with a selectable strategy (`race`, `failover`
   or `fastest` — see [`configuration.md`](configuration.md)). It also holds
   the conditional routing table: a query under a suffix a `forwarder` or
   `stub` zone claims goes to that zone's upstreams instead of the default
   ones, and never falls back to them. See Conditional routing below.

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
| `NOTAUTH` | — | no zone at that apex, the zone is a type with no master to re-ask (`primary`, `forwarder`, `internal` — a `secondary` and a `stub` both pull from one), or it's disabled |
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

## Conditional routing (`forwarder` and `stub` zones)

Two zone types claim a suffix without holding any data for it. A
**forwarder** sends every query beneath its apex to addresses the operator
typed into `forward_to`; a **stub** sends them to nameservers it *fetched*
from a master. One routing mechanism with two sources for the same list of
addresses — a stub is a forwarder whose upstreams are derived rather than
typed. `zones.ParseForwardTo` and `zones.StubUpstreams` are the two
producers; `internal/app`'s `conditionalRoutes` is the one consumer.

### Why the routing lives in the forwarder, not the zones stage

The obvious shape is for the zones stage to dispatch: it has already found
the zone, and the zone declares where its queries go. **That loses caching
entirely.** The pipeline order is `zones → cache → upstream forwarder`, and
a stage that answers short-circuits every stage after it — so a zones stage
that resolved a forwarder zone's query itself would never be seen by the
cache, and every repeat query to an internal suffix would go back out to the
corporate resolver. Instead `Zone.Answer` (`internal/zones/answer.go`)
returns `handled=false` for both types, the query falls through to the cache
and then to the terminal forwarder, and the forwarder is where the suffix is
matched. That fall-through is deliberate and permanent, not a stage waiting
to be written. It also means such a query is logged `forwarded`, with the
conditional upstream's address, rather than `authoritative` — no zone
answered it.

The other candidate shape — rebuilding the whole `upstream.Forwarder`
whenever a zone changes — is the smallest diff and the worst behaviour.
Every record edit calls `reloadZones`, and a rebuild discards each
upstream's latency EWMA, its 15-second backoff after three consecutive
failures, and the RFC 9520 failure cache. Editing a record would degrade
resolution.

So the table is swapped rather than rebuilt around.
`Forwarder.SetConditional` (`internal/upstream/forwarder.go`) builds a
fresh, immutable `condTable` and stores it behind an `atomic.Pointer`:
`pick` loads that pointer and takes no lock, so a query never waits on a
swap and the table it is reading can never be mutated underneath it — the
shape `zones.Resolver`'s snapshot uses, for the same reason. Writers *do*
take a lock, because a swap is a read-modify-write: the outgoing table is
read so that **an address already in it keeps its `*up`**, and with it its
latency history and its health backoff. Without that reuse a swap would
preserve health state for the default upstreams and discard it for exactly
the conditional routes a zone edit is about. One shared `filter.DomainSet`
across every suffix makes matching deterministic longest-suffix routing, so
`internal.corp.example` beats `corp.example` when both are claimed.

`App.ReloadZones` rebuilds the table from the served snapshot after every
zone reload, in that order: the table is derived from the snapshot, so
pushing it first would publish the previous reload's routes. It walks the
snapshot rather than the store because a stub's upstreams are derived from
its NS records and their glue, which only the snapshot carries. A settings
change that rebuilds the forwarder installs the table onto the new one
*before* it goes live, for the same reason — a window in which every claimed
suffix resolves through the default upstreams is a window in which a
split-horizon name is answered by the public internet.

**Only enabled zones contribute.** Disabling a forwarder or a stub releases
its suffix back to the default upstreams, which is what "disabled" means on
every other path: dnsaur gives up the name, so the internet's answer applies.

### A claimed suffix SERVFAILs; it never falls through

**A misconfigured forwarder or stub makes its whole suffix stop resolving,
deliberately.** A zone that claims a suffix keeps its claim when its
upstreams are down or absent: `pick` returns that suffix's list and never
falls back to the defaults, so when every address in it fails — or there are
none — the handler returns an error, the query is answered `SERVFAIL`, and
that failure is negatively cached for 30 seconds per (qname, qtype) per RFC
9520.

This is the rule an expired secondary already follows (`Zone.Serving`),
applied to a configuration failure instead of a data one. A server that
cannot answer for a name it has claimed must not let the public internet
answer instead: for a split-horizon zone, falling through would resolve an
internal name to whatever the outside world says it is, which is the leak
zones exist to close.

**`pick` is necessary and is not sufficient, because the cache is above
it.** A cache entry is keyed on (qname, qtype) and records nothing about
which route produced it, so a hit is served without `pick` being reached at
all — and the name you are claiming is, by the nature of the type, one that
resolves publicly right now, so the public answer is in the cache at the
moment you claim it. Left alone, that entry keeps being served; and when the
claimed suffix's own upstreams then fail, the cache serves it **stale** —
`NOERROR`, TTL 30, for `cache.serve_stale_for` past its own TTL — where this
section requires `SERVFAIL`.

So installing the routing table also purges it. `App.installConditional`
calls `Cache.Purge` for every suffix whose route set changed — added,
removed or altered — *after* the install, never before, since purging first
would leave a window in which a miss re-fills the cache from the defaults.
The purge also bumps an epoch, which does two jobs a sweep of the map alone
cannot. `put` compares against it, so a lookup that reached the upstreams
before the claim existed cannot store its pre-claim answer after the sweep
has passed; and it is part of the singleflight key, so a query arriving
*after* the purge is not collapsed onto a leader that went out before it and
handed that leader's answer from the old route. It is scoped to what changed:
every record edit reinstalls this table, and purging on each would throw away
exactly the answers conditional routing exists to cache.

`Purge` is a linear scan of the cache once per changed suffix, and it holds
the cache's single mutex throughout — so it stalls the query path for that
long, not just the purging goroutine. Measured against a full 10,000-entry
cache: ~0.6 ms for one changed suffix, ~7 ms for a hundred, ~18 ms for five
hundred. At the documented scale (a homelab's single-digit zone count) that
is nothing, and it only runs when routing actually changed. An index from
suffix to keys would be the answer if hundreds of forwarder zones ever met a
large cache.

Two things that follow, and are worth having stated:

- **The mirror case is closed by the same code.** Disabling or deleting a
  forwarder releases its suffix, and the internal answers cached under it go
  with it — so a name that should now resolve publicly does, rather than
  answering from behind the corporate resolver until the entry ages out.
- **Serve-stale still applies to the zone's own answers, and that is not a
  fall-through.** Once a claimed suffix has answers of its own in the cache,
  an upstream failure serves those stale exactly as it would for any other
  forwarded name. What can never happen is the outside world's answer being
  served for a claimed name.
- **The residual window is one upstream round trip, not one cache lookup.**
  What survives is a query that was already somewhere in the pipeline when
  the routing changed: one that had read the cache and *hit* (answered from
  what it read — that one really is a lookup wide); one that had read it and
  *missed* and is out at the old route, answered a round trip later; and one
  landing between `SetConditional` and `Purge`, which sees the new table and
  the old entries. All three are bounded by the forwarder's 2s timeout, and
  none of them can *store* anything — `put` refuses an entry whose epoch has
  moved on, and the singleflight key carries the epoch so a query arriving
  after the purge cannot join a lookup dispatched before it. What leaks is one
  in-flight answer to one client, once; the stored state is clean the instant
  the purge returns. Closing even that would mean serialising the cache read
  against the routing swap, which is a lock on the query path.

There are three ways into that state and they are all the same state:

- a `forwarder` whose `forward_to` is empty, which the API accepts on
  purpose rather than refusing;
- a `forwarder` whose stored `forward_to` will not parse — a hand-edited
  row, since the API validates on write — which fails closed to no upstreams
  rather than open to an unintended destination;
- a `stub` that has not fetched yet, or whose every nameserver turned out to
  be unusable.

The dashboard states this consequence on the zone page (see
[`dashboard.md`](dashboard.md#forwarder-zones)) without explaining it; the
explanation is here.

### The stub's two queries, and the glue rule

A stub is a secondary that keeps only the apex. `zones.StubFetcher.Fetch`
(`internal/zones/stub.go`) asks its master two ordinary questions —
`<apex> SOA` for the serial and the schedule, `<apex> NS` for the delegation
with glue in ADDITIONAL — and installs what comes back through the same
`DiffRecords`/`ReplaceRecords` a transfer installs a zone with. **Not an
AXFR, and that is the point rather than an optimisation**: a stub needs no
`allow_transfer` permission on the far end, so it works against a master
that will not transfer its zone to anybody. Both queries are signed when the
zone names a TSIG key, since a master that requires TSIG on ordinary queries
would otherwise refuse the fetch, and both re-ask over TCP on a truncated
answer — a delegation with several nameservers and glue for each is exactly
the shape that overflows a 512-byte UDP answer, and a client that believed
`TC=1` would lose every address silently.

**An in-zone nameserver is never resolved.** `ns1.corp.example` ends with
the apex of zone `corp.example`, so resolving it would match that stub's own
suffix, route into the stub, and need the address being resolved — a hang,
not a slow failure. DNS's own answer is glue, and it is the right one here:
the address comes from the master's ADDITIONAL section or the nameserver is
unusable. An out-of-zone nameserver cannot re-enter the zone, so it is
resolved normally, through the same `net.Resolver` a hostname in `primaries`
is looked up through, and its addresses are stored beside it exactly as glue
is.

**This is why dnsaur emits glue for its own apex NS set.** A master that
answers an apex `NS` query with names and no addresses leaves a stub with
nothing it is allowed to use: it may not resolve an in-zone nameserver, so it
skips every one and ends up claiming a suffix it cannot route. That is what
`ns1.google.com` avoids by returning A and AAAA for all four of `google.com`'s
nameservers, and it is RFC 1035 §3.3.11's additional-section processing.
dnsaur did it for referrals — where glue is load-bearing because the child is
the only one who could answer — and not for its own apex, which made it
unusable as a stub's master until `Zone.attachNSGlue` closed the gap.

Only in-zone addresses go in, and `a.iana-servers.net` shows both halves of
that rule in a single response: asked for `iana-servers.net NS` it answers with
four nameservers and attaches A and AAAA for `a.`, `b.` and `c.iana-servers.net`
— and nothing at all for `ns.icann.org`. It is authoritative for the zone and
ICANN runs that host, and it still declines to vouch for an address outside
the zone. (Its empty additional section for `example.com` looks like the same
rule but does not prove it: a server with `minimal-responses` set would answer
identically.)

A nameserver with no usable address is skipped rather than failing the whole
fetch — routing to the ones that worked beats SERVFAILing a suffix because
one glue record was malformed — and if every one is skipped, the zone claims
its suffix and SERVFAILs by the rule above. What gets stored is what
`zones.StubUpstreams` reads back to rebuild the routing table on every zone
reload; it takes no context and no resolver, so it *cannot* query, which is
what makes a reload free.

Two consequences are worth stating because they stay invisible until they
surprise someone:

- **A stub's upstreams are always port 53.** Neither glue rdata nor an
  address lookup carries a port, so `StubUpstreams` joins 53
  unconditionally. `primaries` still accepts `host:port` — that port is for
  the *fetch*, and a master on 5353 is fine — but a stub cannot route to a
  nameserver on a non-standard port.
- **A stub's records are read-only through the API** (`409` on every write),
  and the reason is stronger than a secondary's rather than milder. A
  secondary's hand write is served authoritatively until the next transfer
  deletes it; a stub answers from none of its records, but `StubUpstreams`
  reads them back, so a hand-written apex NS record would silently redirect
  the whole claimed suffix until the next fetch undid it.

### A stub does not expire

A secondary past its SOA expire stops answering (RFC 1034 §4.3.5): it holds
its primary's data on loan, past the expire it can no longer confirm what it
holds, and serving it anyway is worse than serving nothing. **A stub does
not expire, and the divergence is deliberate.** `Zone.Serving`'s expiry
check is `secondary`-only, and `StubFetcher.install` never writes an
`expires_at` in the first place.

A stub serves no data at all. Its NS set is *routing information* — where to
ask — so an old-but-working nameserver beats a self-inflicted SERVFAIL, and
if those nameservers really are gone the query SERVFAILs anyway through the
path above. The outcome is the same when it should be, and better when it
should not. "Both pull from a master, so both expire" is a sentence that
writes itself and does not follow; `Zone.Serving` carries the warning at the
line somebody widening it would edit.

### Refresh is `secondary || stub`, never a forwarder

One scheduler drives both types that pull (`zones.Refresher`,
`internal/zones/refresh.go`): a secondary by AXFR, a stub by the two queries
above, each on the `refresh`/`retry` interval its own SOA publishes, floored
at 60 seconds so a peer that publishes a zero cannot turn the schedule into
a busy loop against itself. `refreshed_at`, `last_attempt` and `last_error`
are written the same way for both, which is what makes a stub's state
readable after a restart. Only the third SOA timer, `expire`, is
type-specific — see above.

A forwarder is not in that set and cannot be: it has no master and nothing
to fetch, so refreshing it would be a button that does nothing.
`POST /zones/{id}/refresh` refuses it with `400`. One predicate answers both
questions the API asks — which types may set `primaries`/`tsig_key_id`, and
which may be refreshed — because they are the same question, asked of the
zone's master; it is spelled `pullsFromAMaster` in `internal/api` and again
in `internal/zones`, deliberately not shared, since one answers about a type
an HTTP client just typed and the other about a stored row.

## Package map

| Package | Responsibility |
|---|---|
| `cmd/dnsaur` | Entry point: flag/config parsing, wiring, graceful shutdown |
| `internal/app` | Top-level app object: builds the pipeline, owns settings hot-reload and background jobs |
| `internal/config` | Bootstrap YAML + env config loading and validation |
| `internal/dnssrv` | DNS listeners, the `Handler`/`Middleware` pipeline abstraction, panic recovery, and TSIG (RFC 8945): a `dns.TsigProvider` that verifies signed messages against the stored keys on every message, plus `RequireTSIG` for the paths that must refuse an unsigned one; also the AXFR/IXFR intercept that routes a transfer's raw `ResponseWriter` to a `Transfers` handler ahead of the pipeline, and the NOTIFY opcode intercept that routes to a `Notifies` handler the same way (see Zone transfers and Zone NOTIFY above) |
| `internal/clients` | Client registry: IP/CIDR matching to client + group |
| `internal/filter` | Blocklist/allowlist engine, list parsing, per-client-group rules, background refresh |
| `internal/zones` | Authoritative zones: zone cut and deepest-match lookup, apex-relative names, RR construction from stored presentation-format rdata, and the NODATA/NXDOMAIN/wildcard/CNAME/referral answering rules; also `TransferServer`, which answers AXFR/IXFR requests the `allow_transfer` ACL permits; `NotifyServer`, which answers inbound NOTIFY and probes/transfers on it; and `Notifier`, which drives the outbound NOTIFY queue (see Zone NOTIFY above); and, for the two zone types that route rather than answer, `ParseForwardTo`/`StubUpstreams` (the addresses a claimed suffix goes to) and `StubFetcher` (the SOA/NS fetch and the glue rule) — see Conditional routing above |
| `internal/cache` | In-memory DNS response cache (TTL clamps, negative caching, serve-stale) |
| `internal/upstream` | Upstream forwarders and selection strategy; also the conditional routing table (`SetConditional`) that sends a suffix a `forwarder` or `stub` zone claims to that zone's own upstreams — see Conditional routing above |
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
