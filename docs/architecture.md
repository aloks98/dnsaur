# Architecture

See also: [`README.md`](../README.md) ·
[`docs/superpowers/specs/2026-08-03-dnsaur-design.md`](superpowers/specs/2026-08-03-dnsaur-design.md)
for the full design rationale.

dnsaur is a single Go binary. It runs a DNS engine, a storage layer,
background jobs, and a REST API + auth server on top of the same storage
layer (see [`docs/api.md`](api.md)). The web dashboard (`web/`, a React
SPA) is built separately but embedded into that same binary via
`//go:embed` and served by the same HTTP server as the API — there is no
separate frontend process or listener. A second instance following this one
(see Config sync below) is another copy of the same binary, not a component
of this one; the DHCP server planned for a later phase is an additional
listener feeding the same internal packages rather than a separate process.

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
   response instead of crashing the server. It covers the goroutine the
   query arrived on, so a stage that spawns its own — the forwarder's
   `race` — has to contain its panics itself. One bad query never takes down
   the resolver.
3. **client-id** — maps the requester IP to a known client (name, group) via
   the client registry. Unknown IPs fall into the default group.
4. **filter** — checks the qname against the client's group blocklists,
   allowlists, and regex rules. A block returns a configured response
   (null IP or NXDOMAIN) and is tagged `blocked` in the log, carrying the
   rule id, the list id and the entry that actually matched. Rules are
   evaluated before lists and allow before block — see
   [`dashboard.md`](dashboard.md#the-order-that-matters) for the full
   six-stage precedence. Compiled rulesets and the pause state are immutable
   values behind `atomic.Pointer`, so a refresh or a pause never makes a
   query wait on a lock. A pause covers the whole server, one group or one
   client, whichever ends later winning; every change is written to one
   settings row and installed again at the next start, so a restart in the
   middle of a pause does not turn blocking back on.
5. **zones** — answers authoritatively for the suffixes this server holds,
   before the cache or any upstream is consulted, and tags the result
   `authoritative` in the query log. This is a zone cut, not a set of
   overrides: once a zone claims a name, that name is *never* forwarded.
   It either answers, or returns NODATA (name exists, wrong type) or
   NXDOMAIN (name absent), each carrying the zone's SOA so resolvers cache
   the absence. A qname no zone claims passes straight through untouched,
   and so does a question in a class other than `IN`: every record here is
   class IN, so a `CH` or `HS` question is not one a zone can answer, and
   it goes to the next stage rather than being answered from IN records —
   the same fall-through the cache does with a non-IN question.
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

### The query that goes upstream is dnsaur's, not the client's

The forwarder does not relay the message it was handed. It reads the
client's question and builds a new query around it: that one question, RD
set, a message ID minted here, and an OPT record of dnsaur's own —
1232-octet UDP size, carrying the DO bit copied from the client and no
other option. The reply is given the client's ID back before it leaves the
pipeline, and its question keeps the client's exact spelling.

Nothing else the client sent travels. A TSIG-signed ordinary query (a
secondary refreshing an apex this server does not hold) loses its signature
rather than being sent to a resolver that shares no key with the peer;
EDNS Client Subnet, cookies and NSID stay on this network rather than
shaping an answer that the cache — keyed on `(qname, qtype)` and nothing
else — would then serve to every other client.

A message that does not carry **exactly one** question is answered
`FORMERR` and never forwarded. Both transports refuse any other QDCOUNT
before the pipeline is entered — the library's accept function on :53, and
its restatement below on DoH — but a header claiming one question with no
body after it unpacks to a message with none, which neither gate catches.

## Reply shaping

Whatever the pipeline returns is not yet what goes on the wire. Every
transport applies the same rules to it before writing, through one
`shapeReply` (`internal/dnssrv/server.go`) rather than a copy per
transport — the copy is where a rule goes missing, and both of the EDNS
rules below had to be added twice before it existed:

- **EDNS follows the client, not the answer.** A query that carried an OPT
  gets one back, synthesised at a 1232-octet advertised size if the reply
  came without one. A query that carried none gets its reply's OPT
  *removed* — RFC 6891 §7 makes that a MUST NOT, and only removal satisfies
  it, because the cache stores an upstream reply with the OPT it arrived
  with. Without the strip, the first EDNS client to populate an entry
  decided what every later non-EDNS client saw for that name, cookie and
  all. An extended rcode is downgraded to `SERVFAIL` when its OPT goes,
  since its upper bits have nowhere else to live.
- **The DO bit is copied from the query**, RFC 3225 §3. A synthesised OPT
  that always cleared it told a validating client the server was not
  DNSSEC-aware for the answer it had just asked to be able to validate.
- **A handler that failed, returned nothing, or returned no message
  becomes `SERVFAIL`.**
- **RFC 8467 padding, on the encrypted transports only** — and only when
  the query itself was padded. Padding a plaintext reply hides nothing and
  costs bytes.

What is *not* shared stays with the transport that owns it: TSIG signing
and the UDP datagram fit (see Zone transfers below for why the order of
those two is load-bearing) belong to the `:53`/DoT server, HTTP framing to
the DoH one.

## Encrypted serving (DoT and DoH)

DNS-over-TLS (RFC 7858) and DNS-over-HTTPS (RFC 8484) are additional
listeners into the same pipeline, not a second resolver. DoT is
`internal/dnssrv`'s ordinary `Server` bound to a TLS listener — TCP only,
with no UDP sibling — so everything above the socket, the TSIG provider and
the AXFR/NOTIFY intercepts included, is the plaintext path unchanged. DoH
is its own `DoHServer`: an `http.Server` serving `/dns-query`, because an
HTTP request/response pair is not a `dns.ResponseWriter` and there is no
miekg `dns.Server` on that path at all.

That last difference is the one with teeth. On `:53` and on DoT, miekg
applies `DefaultMsgAcceptFunc` to every message before dnsaur ever sees it.
DoH has to apply the same rules itself, and does (`acceptQuery` in
`doh.go`) — a body that merely unpacks is not yet a query:

- a message with the QR bit set is refused with `400`. On `:53` miekg
  answers one by sending nothing at all, so it cannot be used to amplify;
  HTTP has no way to say nothing, and needs none.
- an opcode other than `QUERY` or `NOTIFY` answers `NOTIMP`.
- anything but exactly one question answers `FORMERR`, as does more than
  one answer record, more than one authority record, or more than two
  additional records.

A DoH response also carries `Cache-Control: max-age=<smallest TTL in the
message>` (RFC 8484 §5.1), counting the authority section so a negative
answer gets the SOA's lifetime rather than none, and skipping the OPT,
whose TTL field holds flags rather than a lifetime. A response with no
records — `SERVFAIL`, `REFUSED`, an empty `NOERROR` — gets `max-age=0`.

Both encrypted listeners keep a connection open for **three minutes idle
and any number of queries**. An encrypted connection exists to be reused —
RFC 7858 §3.4 for DoT, HTTP/2 for DoH — and the alternative is a full TLS
handshake per burst for a client like Android's Private DNS, which holds
one connection for the life of the network it is on. miekg's `dns.Server`
defaults (128 queries, eight seconds idle) are the ones being overridden,
and they are not a defence worth keeping here: the listener's network is
the boundary, and the timeout is still finite.

`NOTIFY` passes that gate on every transport, exactly as it does on `:53`,
and a `NOTIFY` or an `AXFR`/`IXFR` arriving over DoH is then answered
`REFUSED` — neither has a meaning on a transport that cannot stream, and
letting either into the pipeline is how a NOTIFY once reached the forwarder
and was sent upstream.

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

**IXFR is answered with the whole zone** — RFC 1995 §4 permits exactly that
("the server may choose to transfer the entire zone just as in a normal full
zone transfer"), and dnsaur has no journal to compute a delta from — **except
for a client that is already current**, which gets the single SOA §2 asks for.
That case is the common one, not a corner: a secondary asks again on every
`refresh` timer it has, and a server that answered each of those with the
entire zone would spend precisely the bandwidth IXFR exists to save on the peer
that needed nothing. The client's serial is read from the SOA its query carries
in the authority section (§3); a query with no SOA of this zone in it has
nothing to compare and gets the whole zone.

A stalled peer does not hold a transfer slot indefinitely. `dns.Server`'s
documented `WriteTimeout` is never applied by the library and
`dns.ResponseWriter` exposes no connection to set a deadline on, so a peer that
stops reading blocks inside `WriteMsg` where the between-envelope deadline
check can never reach it. The transfer's own context therefore closes the
writer when it expires — the same lever the AXFR *client* already uses — which
fails that write and returns the slot.

The gate answers with one of five rcodes, and the first three are what an
operator debugging a secondary that has stopped updating actually needs to
read:

| Rcode | Means |
|---|---|
| `NOTAUTH` | dnsaur doesn't hold that zone — wrong or disabled apex, or a type (`internal`/`stub`/`forwarder`) that holds no data to send — or the request's TSIG didn't verify |
| `REFUSED` | dnsaur holds the zone, but the peer's address or key isn't in its `allow_transfer` — unless the zone is a `primary` and the peer is a registered replica signed with the sync key, which config sync admits beside the ACL rather than by rewriting it (`docs/superpowers/specs/2026-09-11-config-sync-design.md` §6) |
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

The transfer branch checks the opcode as well as the qtype, which is what
keeps the two branches disjoint: `Opcode == NOTIFY, Qtype == AXFR` is a
NOTIFY and goes to the NOTIFY branch below, not to the transfer handler
that happens to be tested first.

## Hostnames in a zone's configuration

Three columns let an operator write a name where an address would do: a
secondary's `primaries`, a zone's `notify_to`, and a stub's out-of-zone
nameserver. All three are stored as written and resolved at use time — that
is what lets a primary move — and all three resolve through **dnsaur's own
cache and forwarder**, never through the host machine's resolver.
`internal/app`'s `zoneLookup` is that resolver, handed to `internal/zones` as
the one-method `Lookup` its four call sites take; `*net.Resolver` satisfies
the same interface, which is what tests use.

Two things follow from it, and both are the reason for it.

**A hostname primary inside the zone it serves can bootstrap.**
`ns1.corp.example` as the primary of `corp.example` used to be unusable:
`net.DefaultResolver` asks whatever the machine resolves through, which on a
machine running dnsaur is usually dnsaur, so the query arrived at this
server's own zones stage — where a secondary that has never transferred
answers `SERVFAIL` for its whole suffix. The transfer that would have fixed
that was the one thing that could not happen, on every attempt, forever.
`zoneLookup` enters the pipeline *below* the zones stage, so the name is
answered from the configured upstreams instead and the first transfer lands.

**Those lookups follow `upstreams`.** A DoT or DoH upstream covers resolving
a primary's name like any other query, rather than leaking it in the clear to
the system resolver; a `forwarder` or `stub` zone claiming the suffix routes
it too, which is what makes a primary named under an internal suffix
resolvable at all. Entering above the cache is deliberate for the same
reason the NOTIFY gate orders its checks the way it does (below): a hostname
primary is resolved to match an arriving NOTIFY's source, so an uncached
lookup there is one outbound query per packet.

Nothing can loop back in. The forwarder is terminal — it puts the question on
a socket and never re-enters the pipeline — so the worst a self-referential
configuration does is fail to resolve.

## Zone NOTIFY (RFC 1996)

Like AXFR/IXFR above, NOTIFY does not go through the middleware pipeline.
`Server.serve` branches on it — `dns.OpcodeNotify` is an *opcode*, not a
qtype, so there is no qtype the transfer branch could route it by and this
is a second, independent branch ahead of the pipeline — and hands the raw
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
load-bearing, not tidiness: `ParsePrimaries` may do a live DNS lookup for a
hostname primary — through dnsaur's own forwarder, per the section above, so
it is a real query leaving this server — and doing that before the local
checks would let one unauthenticated, trivially spoofable UDP packet drive an
outbound lookup for a NOTIFY that was going to be refused anyway. Two more
bounds sit on that lookup for the NOTIFYs that do get past the local checks:
the IP literals in `primaries` are matched first, so the ordinary
`10.0.0.5, ns1.example.com` configuration costs no lookup at all for a NOTIFY
from `10.0.0.5`; and when a hostname really must be resolved, the result — a
failure included — is reused for the length of one throttle window, so a flood
costs one lookup rather than one each. A primary that has just moved is
therefore refused for at most that window, which RFC 1996 §3.6's
retransmission covers.

One unresolvable entry no longer disables the rest: `ParsePrimaries` skips it
and fails only when *nothing* in the list resolved. All-or-nothing resolution
meant a name-server outage took a perfectly dialable IP literal written beside
the name out of service with it — no primary contacted, and every NOTIFY for
the zone refused for want of a source to match against.

The gate, in order:

| Rcode | TSIG error | Means |
|---|---|---|
| `FORMERR` | — | not a single SOA question |
| `NOTAUTH` | — | no zone at that apex, the zone is a type with no master to re-ask (`primary`, `forwarder`, `internal` — a `secondary` and a `stub` both pull from one), or it's disabled |
| `REFUSED` | — | the peer's address isn't one of the zone's configured `primaries`, **and** the message didn't verify under the zone's own `tsig_key_id` |
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
- **A TSIG that verified under the zone's own key stands in for the source
  address.** It is the stronger of the two checks — an address can be spoofed
  and a MAC cannot, and the key on the zone is the one that zone's master
  signs with — so a signed NOTIFY is accepted from wherever it arrives. This
  is what lets a primary behind NAT notify at all: its packets reach us from
  the NAT's address, which is not the one anybody would put in `primaries`.
  Nothing else is widened. From an unlisted source, an unsigned message, one
  signed under a key the zone does not name, and one whose MAC did not verify
  are all still refused, and a server with no key store attached refuses too.
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
2. **Throttle** (5s per zone, trailing-edge): a primary editing ten records
   sends ten NOTIFYs; each gets its own immediate reply, and they collapse to
   one probe. Without this, "NOTIFY is cheap" becomes a probe amplifier
   pointed at dnsaur's own configured primary. A NOTIFY that arrives inside an
   open window is *deferred*, not dropped: the window remembers that something
   arrived and runs one more probe when it closes. Dropping it instead lost
   any serial bump that landed after the window's own probe had already asked
   — the primary editing at t=0 and again at t=2s — until the SOA `refresh`
   fired, with the peer told `NOERROR` so it had stopped retransmitting. The
   bound is unchanged, because what is remembered is "something arrived", not
   how much: ten suppressed NOTIFYs are worth one probe between them.
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
0012 migration) against every enabled zone's `notify_to` — reading the queue
once per pass and writing only to the zones whose rows actually disagree
with their `notify_to`, so an install with no NOTIFY configured (the fifteen
RFC 6303 built-ins and nothing else) costs one query per tick and no
transactions at all. That queue carries
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

One pass's sends run **concurrently, bounded at 8 at a time**. They are
independent — one socket and one queue row each — and the cost of one is a
send timeout against a target that is up and not answering. Sent one at a
time, that silence was paid for by every target behind it in the list, on
every pass, for as long as it lasted: one mothballed secondary delayed every
other zone's notification by seconds it had no reason to spend. Nothing about
what is sent or recorded changes — each target still gets one packet and its
own row's outcome — and the bound is there so a server with hundreds of
targets cannot open a socket per target at once.

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
resolved normally, through the same `Lookup` a hostname in `primaries` is
looked up through — dnsaur's own forwarder, never the host's resolver — and
its addresses are stored beside it exactly as glue is.

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
type-specific — see above. Each attempt re-resolves whatever hostnames the
zone's `primaries` names, through the forwarder rather than the host's
resolver — see Hostnames in a zone's configuration, above.

**A scheduled refresh of a secondary checks the serial before it transfers.**
That is RFC 1034 §4.3.5's refresh timer as the RFC describes it — "check to see
if the zone has been updated", an SOA query first and an AXFR only if the
answer moved. A check that finds nothing new still stamps `refreshed_at` *and*
`expires_at`, because §4.3.5 restarts the expire timer when the primary
answers, not only when it answers with something new; what it skips is the
zone on the wire, the record diff, the snapshot rebuild and the NOTIFY pass. A
probe that *fails* falls through to the transfer rather than failing the
attempt: the probe is one UDP exchange and the transfer is TCP, so a primary
that answers one and not the other is an ordinary misconfiguration, and an
unnecessary AXFR is a cheaper mistake than a zone left to expire with its
primary reachable the whole time. A zone that has never transferred does not
probe at all, for the reason the NOTIFY path gives below: there is no honest
baseline to compare against. `POST /zones/{id}/refresh` is unchanged and still
transfers unconditionally — a button that answered "your serial has not moved"
would be indistinguishable from one that did nothing.

Two more things bound the schedule. Each scheduled attempt gets the same
two-minute deadline dnsaur gives a transfer it is *serving*, because
`transferReadTimeout` bounds each individual read and nothing bounded their
sum — a primary dripping one envelope every twenty seconds could hold a zone's
transfer lock, and the manual refresh and NOTIFY work queued behind it, for as
long as it liked. And a zone whose SOA `expire` is shorter than its `refresh`
is polled at half the expiry instead, with one warning per zone saying so: the
published pair means "stop answering, and do not check", and followed literally
it takes the secondary dark on a schedule (55 minutes of every hour for an
`expire` of 300 against a `refresh` of 3600) with nothing anywhere saying why.

A forwarder is not in that set and cannot be: it has no master and nothing
to fetch, so refreshing it would be a button that does nothing.
`POST /zones/{id}/refresh` refuses it with `400`. One predicate answers both
questions the API asks — which types may set `primaries`/`tsig_key_id`, and
which may be refreshed — because they are the same question, asked of the
zone's master; it is spelled `pullsFromAMaster` in `internal/api` and again
in `internal/zones`, deliberately not shared, since one answers about a type
an HTTP client just typed and the other about a stored row.

## Config sync (main and replica)

Two dnsaur boxes serve one network so that either can answer when the other
is down — DHCP hands out both addresses, and the failover is the client's.
Zone transfers already make authoritative data identical on the pair; config
sync is what makes the rest of it identical, so the two boxes behave the
same.

One instance is the **main**: it takes writes, and it learns nothing it did
not already know except that a replica exists. Any instance with
`sync.peer_url` set is a **replica**: it pulls the main's configuration on a
timer, applies it, and refuses local writes to anything that configuration
covers with `409 managed by <peer>`. Which of the two a box is comes from
that one setting — there is no third mode, no election, and no split-brain
to recover from, because only one box ever accepted writes.
`internal/confsync` holds both halves: `Replica`, the pull loop (it runs on
every box, and a box with no peer never dials), and `Registry`, the main's
record of who is following it. The design rationale is in
[`docs/superpowers/specs/2026-09-11-config-sync-design.md`](superpowers/specs/2026-09-11-config-sync-design.md);
the endpoints are in [`docs/api.md`](api.md), the settings and a two-box
walkthrough in [`docs/configuration.md`](configuration.md#config-sync), and
the screens in [`docs/dashboard.md`](dashboard.md#sync).

### The pull cycle

```
 replica                                   main
   │ every sync.interval_seconds (default 30 s)
   ├─ GET /api/v1/sync/version ───────────►│  {config_version, instance_id}
   │◄──────────────────────────────────────┤
   │ unchanged → register, done            │
   │ changed:                              │
   ├─ GET /api/v1/sync/bundle ────────────►│  the bundle, Bearer sync.token
   │◄──────────────────────────────────────┤
   │ derive zones, import in one tx        │
   │ reload clients, filters, zones, settings
   ├─ POST /api/v1/sync/replicas ─────────►│  {instance_id, dns_addr, version_applied}
   │◄──────────────────────────────────────┤  204; the main records the replica
```

Polling, not push. The version probe is a few hundred bytes, and at 30 s the
worst-case lag is one interval — the same order as the TTLs the network
already lives with. A pull that fails leaves the last applied configuration
in force and records why in `sync.last_error`; DNS is unaffected, which is
the behaviour a store that is down already has everywhere else.

Two rules keep a half-applied configuration off the replica:

- **The version decides.** A bundle is fetched only when the probe's
  `config_version` differs from the one the replica last applied — a box
  that has never applied one, or that last applied one from a *different*
  peer, fetches whatever the main has — and applied
  only when the bundle's own version still matches the probe's. A
  write landing between the two requests is caught by the next cycle rather
  than applied half a version late.
- **One transaction.** `store.ImportBundle` diffs every synced table against
  the bundle by id — insert, update, delete — writes the settings it
  carries, removes the synced settings it does not, and bumps
  `config_version` once. Either the box matches the bundle or it matches
  what it had before.

After the transaction the replica runs what an API write handler runs after
its own write, in that order: `ReloadClients`, `RecompileFilters`,
`ReloadZones`, `ReloadSettings`. Recompile, not refresh — a bundle cannot
change what a list URL serves. A list this box has never downloaded is the
exception: the pull kicks the same background download a list creation does,
because a bundle carries a URL and not its contents, and waiting for
`lists.refresh_hours` would leave a newly following replica serving a LAN
with blocking configured and nothing blocked.

The registration closes every cycle, whether or not anything was applied: it
is also the heartbeat the main measures staleness by, and configuration
changes far less often than three intervals.

### What travels, and what stays on the box

| Data | In the bundle? |
|---|---|
| Groups, clients, lists and their group assignments, rules, TSIG keys | Yes — with the main's ids |
| Zone *definitions* | Yes, rewritten for the replica (below) |
| Zone records, a zone's transfer state, a list's refresh state | No — the replica's own transfers and downloads produce them |
| Every setting outside the local set: `upstreams`, `upstream.strategy`, `blocking.*`, `cache.*`, `lists.refresh_hours`, `qlog.*`, `stats.retention_days` | Yes — including `blocking.pauses`, since a pause is a decision about the network and clients reach either box |
| `instance.id`, every `serve.*` key, every `sync.*` key, `stats.watermark` | No — they describe the box, not the service |
| The bootstrap YAML (`dns_listen`, `http_listen`, `data_dir`, `storage.*`, `log_*`, `trusted_proxies`) | No — it is not in the `settings` table at all |
| Users, sessions, API tokens, the query log and the stats behind it | No — an admin account per box; the replica's token to the main is the only credential that crosses |

`store.LocalSettingKey` is the single answer to "does this key stay here?":
the main asks it to decide what leaves in a bundle, and the replica asks it
both to decide what a bundle may overwrite and to keep its own keys from
being pruned as absent from one.

Ids are the main's, and the import keeps them, so a `group_id`, a
`tsig_key_id` or a query log row's `rule_id` names the same row on both
boxes. A replica never creates a synced row of its own, so its id space is
free for the main's to occupy; on Postgres the import advances each synced
table's sequence past the ids it wrote, so the first row created after a
promotion does not collide with one the main already used.

**The config version** is what the probe compares. Every write to a synced
table advances it, inside that write's own transaction — a group, a client,
a list or an assignment, a rule, a TSIG key, a zone definition, a setting.
Zone records, serial bumps, transfer bookkeeping and a list's refresh state
do not, and neither does a registration arriving from a replica: bumping the
version on each of those would have every replica pull a bundle it already
has, forever.

### Zones on the replica

The bundle carries definitions; the records arrive by AXFR.
`confsync.DeriveZones` rewrites each zone before the import:

| The main's type | The replica gets |
|---|---|
| `primary` | a `secondary` of the same name, transferring from `sync.primary_dns` (empty: the peer URL's host on port 53) under the main's sync key, with `allow_transfer` and `notify_to` copied verbatim so the replica serves its own downstreams the same way the main does |
| `secondary` | the same definition — both boxes become equal secondaries of the same external primary, and the replica does not transfer from the main |
| `stub`, `forwarder` | the same definition |
| `internal` | nothing; each box seeds its own RFC 6303 built-ins, and no bundle carries them |

A derived secondary that already exists keeps its transfer state —
`soa_serial`, `refreshed_at`, `expires_at` and the records themselves. Only
the definition columns are written, so a config pull never makes a serving
secondary forget what it transferred. A newly derived one is due for a
refresh immediately, so its first transfer starts on the scheduler's next
pass.

### The implicit allow and notify

Nothing rewrites an ACL to let the pair transfer. The main designates one
TSIG key as `sync.tsig_key_id` — creating that key and picking it is the one
manual step setting up a pair — and registration adds one clause beside each
existing gate:

- **Transfer.** An AXFR for a `primary` zone that verified under the sync key
  and arrived from a registered replica's `dns_addr` is served even though no
  `allow_transfer` entry matches it (the `REFUSED` row of Zone transfers
  above). The clause is asked only after `allow_transfer` has already
  refused, so it widens who may transfer and never narrows it. A stale
  replica still matches: stale is exactly the state of a box coming back from
  an outage, which is the one that needs the data. A `dns_addr` naming a
  hostname never matches — resolving one would put a DNS lookup inside the
  gate of the server that answers DNS — so such a replica's transfers are
  decided by `allow_transfer` as they were before it registered.
- **NOTIFY.** Every registered replica that is *not* stale is appended to
  each `primary` zone's targets, signed with the sync key, and reconciled
  into `zone_notifies` like any other. Staleness is acted on here and not at
  the transfer gate because the two cost different things: notifying a box
  that has been silent buys a round of retries per zone per serial bump and
  nothing else, since its own refresh timer collects the zone when it
  returns. A replica an operator also wrote into `notify_to` by hand is told
  once, under the entry they wrote. A secondary zone gets no replica targets:
  the replica follows that zone from its own primary.

A replica is **stale** when nothing has been heard from it for three times
`sync.interval_seconds`. It is never deleted automatically — a box down for
an afternoon is not a box whose transfer permission should quietly disappear
— so the operator removes a retired one themselves, with **Forget** on the
Sync band or `DELETE /api/v1/sync/replicas/{instance_id}`.

### Promotion

Clearing `sync.peer_url` and `sync.token` lifts the guard; **Stop following**
sends both in one write. The box keeps whatever configuration it last applied
and takes writes again. Nothing is repointed: the zones it derived are still
secondaries, of a main that may be gone, and turning one into a primary is a
per-zone decision on that zone's own page.

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
| `internal/confsync` | Config sync between two instances (see Config sync above): `Replica`, the pull loop that probes a main's config version, fetches and validates a bundle, derives the replica's own zones from it and applies it; and `Registry`, the main's record of which replicas have registered, which is what the implicit transfer allow and NOTIFY targets are read from |
| `internal/stats` | Hourly stats rollups from the query log |
| `internal/store` | Storage interfaces plus SQLite/Postgres implementations, migrations, settings |
| `internal/api` | HTTP REST API server + handlers (`/api/v1`: setup, settings, blocking, groups, clients, filters, zones, queries, stats, tokens), embedded OpenAPI 3.1 doc; also mounts the web dashboard's static files (`internal/api.StaticHandler`) on every non-`/api` path when `Deps.Static` is set. The static mount — and only the static mount, so SSE on `/api/v1/queries/tail` stays unbuffered — gzips responses and sets `Content-Security-Policy` (`frame-ancestors 'none'`), `X-Content-Type-Options: nosniff`, `Referrer-Policy: same-origin`, and `X-Frame-Options: DENY` |
| `internal/auth` | Auth service: argon2id password hashing (bounded concurrency), session + scoped (read/write) API tokens with a 90-day session ceiling, optional TOTP 2FA with single-use codes |
| `web` | The dashboard's Go-side glue: `//go:embed all:dist` over the React SPA's Vite build output, exposed as `web.Dist() fs.FS` for `internal/app` to hand to `internal/api.Deps.Static`. The actual frontend source (React 19 + TypeScript + Tailwind + TanStack Query, see `web/README.md`) lives under `web/src`, built independently (`pnpm build`) before the Go build embeds its output |

Planned, not yet present: `internal/dhcp` (Phase 2).

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
  originals are the only surviving record of what the user wrote. The
  migration is the only reader, through raw SQL — `Store` exposes no Go API
  over that table, and nothing serves from it.
- Almost all runtime configuration lives in a `settings` key/value table in
  the database, not in the bootstrap YAML. `internal/app` seeds sane
  defaults on first run, and components subscribe to a change notification
  channel (`SettingsStore.Changes()`) so writes hot-reload live components
  without a restart. See [`docs/configuration.md`](configuration.md) for the
  full settings list and the handful of exceptions that still require a
  restart.
- The two tables that grow with traffic are both bounded: `query_log` by
  `qlog.retention_days` and the hourly rollups in `stats_hourly` by
  `stats.retention_days`, pruned together by one daily pass. Both prunes
  delete in bounded chunks rather than one statement, because SQLite runs on
  a single connection and a multi-million-row `DELETE` would hold it long
  enough for the query log's own buffered writes to time out and be
  discarded.
- The DNS cache and the compiled filter sets are memory-only — never
  persisted to the DB. Downloaded blocklist files are cached on disk
  (`<data_dir>/lists/<id>.txt`, with the server's `ETag` beside it) so a
  restart doesn't force a re-download.

## Blocklists: compiling is not downloading

`internal/filter`'s refresher has two entry points, and which one a caller
takes is the difference between a request that answers immediately and one
that waits on the internet.

- **Compile** (`Recompile`) reads the stored rules and the cached list files
  and swaps in a new set of compiled rulesets. No network, no list state
  written — nothing was attempted, so there is nothing to report. Every API
  write to a rule, a list or an assignment takes this path, and so does a
  settings change (`internal/app`'s watcher), which is why a rule save
  answers in milliseconds even with an unreachable list subscribed, and why
  "add a rule, see it blocked" is immediate.
- **Download** (`RefreshAll`) fetches every enabled list into the cache,
  records what each attempt produced (`ok`/`stale`/`failed`/`empty` — see
  [`ui-contract.md`](ui-contract.md#refresh-outcome-last_status)) and then
  compiles. It runs on the `lists.refresh_hours` ticker, on
  `POST /filters/refresh`, when a list is created or re-enabled, once in
  the background at startup, and after a config-sync pull that brought a
  list this box has no copy of.

`App.Start` compiles synchronously **before** it binds any listener. The
first download can take as long as the slowest subscribed URL — with the WAN
down, the full fetch timeout per list — and the alternative is a window
where the resolver answers, unfiltered, with usable copies sitting on disk.

Two bounds apply to a download, both because the body is admin-named but
server-fetched:

- 64 MiB per list, enforced with an `io.LimitReader`; an overflow is a
  failed fetch, not a full data directory.
- The connection is refused unless the address dialled is a public one. The
  check sits in the transport's dialer rather than on the URL, so it sees
  the address after DNS resolution and applies to every redirect hop: a
  subscription pointed (or redirected) at `169.254.169.254`, at loopback, or
  at a neighbour on the LAN cannot make the fetcher a confused deputy.

## "DNS must not die"

The guiding rule for error handling: DNS resolution degrades last, and
everything else is expendable before it.

- Upstream failures fail over to healthy upstreams, then serve-stale cache
  entries, and only return `SERVFAIL` as a last resort.
- Storage failures don't stop resolution — the filter sets and cache are
  in-memory — while DB-dependent API endpoints return `503` and the query
  log buffer drops oldest entries with a surfaced warning rather than
  blocking.
- A failed blocklist refresh keeps serving the previous compiled list and
  retries with backoff instead of going unfiltered or falling over, and a
  restart compiles the cached copies before it binds a listener rather than
  serving unfiltered until the first download finishes.
- Bad configuration is validated and rejected at write time; the running
  config is always the last-known-good one. The bootstrap config is the one
  place that refuses to start instead: no listen address, or a `-config`
  path that isn't there, does not give a degraded resolver but a silent
  one, and a process that comes up answering nothing is worse than a
  process that says why it didn't (see
  [`docs/configuration.md`](configuration.md#what-startup-refuses)).
- Pipeline panics are recovered per-query (`SERVFAIL` + logged) so one
  poisoned query can't take the whole server down.

This shows up concretely in `internal/app`'s settings reload logic: if a new
upstream configuration fails to build, it keeps the previous working
forwarder rather than replacing it with something broken, falling back
through progressively safer defaults only if no forwarder has ever been
installed. The same rule governs the settings a reload applies directly: a
read that *fails* is not a value, so the blocking mode and the query-log
privacy mode already in force are kept, rather than letting a database blip
read as "the operator asked for the default" and quietly start logging whole
client IPs on an install configured to anonymise them.

That last rung has one consequence worth stating out loud: **the hardcoded
defaults are plaintext.** If the stored `upstreams` value asked for
DNS-over-TLS or DNS-over-HTTPS and would not parse, resolving through them
is not a smaller version of what was configured but the opposite of it. The
choice is still to keep resolving — a resolver that stops entirely is worse
than one that resolves unencrypted — but never silently: `App` records the
downgrade, `GET /resolver/status` reports it, and the settings screen shows
a persistent warning until the setting is fixed. The record clears the
moment a later apply installs a forwarder built from the stored value.
