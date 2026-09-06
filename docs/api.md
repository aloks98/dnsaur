# REST API

See also: [`README.md`](../README.md) · [`docs/architecture.md`](architecture.md) · [`docs/configuration.md`](configuration.md)

**Status: shipped.** The `/api/v1` REST layer and its auth system
(`internal/api`, `internal/auth`) are fully implemented — first-run setup,
session/token auth, optional TOTP 2FA, and CRUD/query/stats endpoints for
every resource dnsaur manages. This page is a summary; the
**authoritative reference** is the OpenAPI 3.1 document dnsaur serves
itself, unauthenticated, at `GET /api/v1/openapi.yaml`
(source: `internal/api/openapi.yaml`) — treat that as ground truth over
this page for exact request/response shapes and status codes.

The web dashboard (`web/`, see the status table in the
[`README`](../README.md) and [`docs/architecture.md`](architecture.md)) is
built entirely on this API — it has no privileged access the API doesn't
already expose. Everything the dashboard does is equally scriptable via
curl or any HTTP client.

## Conventions

- Base path: **`/api/v1`**.
- JSON in, JSON out (`application/json`), except `GET /queries/tail`
  (`text/event-stream`) and `GET /openapi.yaml` (`application/yaml`).
- Errors are always a flat envelope: `{"error": "<message>"}`, with a
  matching HTTP status code. One endpoint, zone file import, has more than
  one problem to report at once; it adds an `errors` list alongside the
  same flat `error` — see Zone files below.
- DNS resolution is never affected by API/DB problems — by design
  (`docs/architecture.md`'s "DNS must not die" principle), DB-dependent
  endpoints return `503` on storage errors rather than taking anything else
  down.
- `GET /health` and `GET /openapi.yaml` are unauthenticated; every other
  endpoint requires auth (`401` if missing/invalid).

## Auth model

- **First-run setup:** `GET /setup` reports `{"setup_required": bool}`;
  `POST /setup` (`{username, password}`, password min 8 chars) creates the
  first and only admin account — `409` if setup already completed.
- **Sessions:** `POST /auth/login` (`{username, password[, totp_code]}`)
  authenticates and sets an `HttpOnly`, `SameSite=Strict` `dnsaur_session`
  cookie with a 30-day TTL. Sessions are sliding: a request that
  authenticates with less than half the TTL remaining gets silently
  renewed. If the account has TOTP enabled and `totp_code` is omitted,
  login fails with `428`; bad credentials return `401`.
  `POST /auth/logout` clears the cookie and revokes the session token.
  `GET /auth/me` returns the current user (`id`, `username`,
  `totp_enabled`).
- **API tokens:** scoped, revocable bearer tokens for scripts and other
  clients, created via `POST /tokens` and sent as
  `Authorization: Bearer <token>`. Each token has a `read` or `write`
  scope (`write` is the default if omitted); a `read` token gets `403`
  (`{"error": "read-only token"}`) on anything but `GET`/`HEAD`. The
  plaintext token is only ever shown in the `POST /tokens` response.
- **Passwords** are hashed with argon2id; login timing is equalized for
  unknown usernames to avoid leaking account existence.
- **Optional TOTP 2FA**: `POST /auth/totp/start` begins enrollment
  (returns a base32 `secret` and an `otpauth_url` for a QR code);
  `POST /auth/totp/confirm` (`{secret, code}`) verifies a code and enables
  it; `POST /auth/totp/disable` (`{code}`) turns it off. Once enabled,
  `totp_code` is required on every login.

## Quick example: setup → login → create a zone

```sh
BASE=http://localhost:8080/api/v1

# First run: create the admin account
curl -sX POST "$BASE/setup" \
  -H 'Content-Type: application/json' \
  -d '{"username":"admin","password":"correct-horse-battery"}'

# Log in, keeping the session cookie
curl -sX POST "$BASE/auth/login" \
  -H 'Content-Type: application/json' \
  -d '{"username":"admin","password":"correct-horse-battery"}' \
  -c cookies.txt

# Use the session cookie to create a zone (SOA fields are optional — defaults
# are generated from the name)
curl -sX POST "$BASE/zones" \
  -H 'Content-Type: application/json' \
  -b cookies.txt \
  -d '{"name":"home.lan"}'

# Add a record — name is relative to the zone's apex
curl -sX POST "$BASE/zones/1/records" \
  -H 'Content-Type: application/json' \
  -b cookies.txt \
  -d '{"name":"nas","type":"A","rdata":"192.168.1.10","ttl":300}'
```

## Endpoints

Full parameter/response detail lives in `internal/api/openapi.yaml`
(served at `GET /api/v1/openapi.yaml`). All paths below are relative to
`/api/v1`.

- **Meta** — `GET /health` (liveness + version), `GET /openapi.yaml` (this
  spec). Both unauthenticated.
- **Setup** — `GET /setup`, `POST /setup`. See Auth model above.
- **Auth** — `POST /auth/login`, `POST /auth/logout`, `GET /auth/me`,
  `POST /auth/totp/start`, `POST /auth/totp/confirm`,
  `POST /auth/totp/disable`.
- **Settings** — `GET /settings` (flat key→string map of all editable
  settings; `instance.*`/`stats.*` keys are internal and omitted),
  `PUT /settings` (`{key, value}`, one key per call; editable keys:
  `upstreams`, `upstream.strategy`, `blocking.mode`, `blocking.ttl`,
  `cache.min_ttl`, `cache.max_ttl`, `cache.max_entries`,
  `cache.serve_stale_for`, `lists.refresh_hours`, `qlog.retention_days`,
  `qlog.privacy` — see [`docs/configuration.md`](configuration.md) for
  what each means and which require a restart to take effect). There is
  no separate "upstreams" resource — upstream servers live in the
  `upstreams` setting.
- **Resolver status** — `GET /resolver/status`
  (`{encryption_downgraded, reason}`). Server state rather than a setting,
  which is why it is not in the `GET /settings` map. `encryption_downgraded`
  is true when the stored `upstreams` value named `tls://` or `https://`,
  failed to parse, and the server fell back to its hardcoded **plaintext**
  default resolvers — so queries are travelling in the clear while the
  settings page still shows the encrypted value. `reason` carries the parse
  failure. It clears as soon as a settings apply installs a forwarder built
  from the stored value. The settings screen shows it as a persistent
  warning; see [`docs/configuration.md`](configuration.md#upstreams).
- **Blocking** — `GET /blocking?group_id=` (pause status),
  `POST /blocking/pause` (`{group_id, minutes}`, pauses 1–1440 minutes),
  `DELETE /blocking/pause?group_id=` (resume/cancel a pause).
- **Groups** — `GET /groups`, `POST /groups` (`{name[, enabled][, list_ids]}`
  — `enabled` defaults to true; `list_ids` omitted assigns **every** list,
  while an explicit `[]` assigns none), `PATCH /groups/{id}`
  (rename/enable/disable), `DELETE /groups/{id}`,
  `GET /groups/{id}/lists` and `PUT /groups/{id}/lists` (assign filter
  lists to a group), `GET /groups/{id}/rules` and `POST /groups/{id}/rules`
  (per-group allow/block rules, literal or regex pattern).
- **Clients** — `GET /clients`, `POST /clients`, `PUT /clients/{id}`,
  `DELETE /clients/{id}` — each client is an IP or CIDR `matcher` bound to
  a `group_id`.
- **Filters** — `GET /filters/lists`, `POST /filters/lists` (subscribe a
  block/allow list URL; refreshes synchronously before responding),
  `PATCH /filters/lists/{id}` (enable/disable),
  `DELETE /filters/lists/{id}`, `DELETE /filters/rules/{id}` (delete a
  per-group rule), `POST /filters/refresh` (`202`, kicks off an
  asynchronous refresh of all lists).
- **Zones** — `GET /zones`, `POST /zones`, `GET /zones/{id}`,
  `PATCH /zones/{id}`, `DELETE /zones/{id}` (cascades its records).
  A name inside an enabled zone is answered or routed by that zone, never
  handed to the default upstreams. `name` alone is enough to create a
  `primary` — SOA fields default to generated values, and an apex NS record
  is created alongside it. **That apex NS is seeded for a `primary` only**:
  every other type's contents come from somewhere else, and writing one would
  be dnsaur authoring data in a zone it does not own. `type` may be
  `"primary"`, `"secondary"`, `"forwarder"` or `"stub"`, and the four
  sections below say what each additionally needs — the last two claim a
  suffix and route it instead of answering from records of their own.
  `type: "internal"` is the fifth: the
  built-in zones (`localhost` plus every RFC 6303 §4 reverse zone except
  the private ranges — `BuiltinZones` in `internal/store/builtins.go` has
  the exact list), seeded at migration and never created through this
  API. Every write to an `internal` zone or its records — `PATCH` or
  `DELETE` on the zone, or any write under `/zones/{id}/records` — is
  refused with `409`. A reverse zone for your own network isn't a distinct
  type; it's an ordinary `primary` zone whose `name` ends in
  `in-addr.arpa` or `ip6.arpa` (e.g. `168.192.in-addr.arpa`).
- **Secondary zones** — a `secondary` is a copy of a zone held elsewhere,
  pulled over AXFR and kept fresh on the schedule its own SOA publishes, or
  sooner if one of its `primaries` sends it a DNS NOTIFY — see
  [`docs/architecture.md`](architecture.md) for the accept/refuse rules and
  what each rcode means. Creating one requires `primaries` —
  comma-separated `host[:port]`, port 53 by default, stored as written and
  resolved at transfer time — and takes an optional `tsig_key_id` naming
  the key to sign transfers with.
  Both fields describe a master, so both apply to the two types that have
  one — a `secondary` and a `stub` — and both are refused with `400` on any
  other type. **Its records are read-only**: `POST`/`PUT`/`DELETE` under `/zones/{id}/records`, and
  `POST /zones/{id}/file`, all answer `409`, because the next transfer
  would replace whatever they wrote. It answers `SERVFAIL` for its whole
  suffix before its first transfer lands and again once `expires_at`
  passes — never `NXDOMAIN`, which would be an authoritative claim it is in
  no position to make.
  `POST /zones/{id}/refresh` transfers one now, whatever the schedule says.
  It answers only when the transfer has finished — `200` with the primary
  that answered, the serial and the record count, or `502` carrying the
  transfer's own error. A failed transfer changes nothing: the zone keeps
  the records, serial and `refreshed_at` it already had. The endpoint is
  `secondary`-or-`stub` only, for the same reason `primaries` is: a
  `primary` is authored here and a `forwarder` names its upstreams outright,
  so neither has a master to ask, and both get `400 only secondary and stub
  zones pull from a master`.
  Three read-only fields on the zone describe all of this. `refreshed_at`
  is the last transfer that **succeeded**; `last_attempt` is the last one
  **tried**, successful or not; and `last_error` is why that attempt failed,
  in the transfer's own words, or `""` when it succeeded. The last two are
  written together and survive a restart — the scheduler's own view of a
  failure does not — so they are the honest answer to "is this zone
  working", and `last_error` should always be read beside `last_attempt`.
- **Forwarder zones** — a `forwarder` claims a suffix and sends every query
  beneath it to addresses you name, instead of to the `upstreams` setting.
  It holds no records and answers nothing of its own; see
  [`docs/architecture.md`](architecture.md) for how the routing table is
  built and what happens when the addresses stop answering.
  `forward_to` on `POST /zones` and `PATCH /zones/{id}` is that list: a
  comma-separated list where each entry is `host[:port]`, port defaulting to
  53 — e.g. `"10.0.0.1, 10.0.0.2:5353, ns.corp.example, [fd00::2]:5353"`.
  There is no `key:` suffix, unlike `notify_to`: a forwarder sends ordinary
  queries rather than transfers and signs nothing. **A hostname target is
  never resolved by dnsaur at all**, which is where `forward_to` and
  `primaries` genuinely differ rather than merely differing in timing: a
  primary is resolved to addresses at transfer time, while a forward target
  reaches the forwarder as a dial string and Go's own dialer resolves it on
  every exchange. So a hostname forward target follows DNS per query, and
  cannot go stale between zone reloads. **What's stored is the canonical
  spelling, not what was typed**: the value is re-parsed and re-formatted on write (the port
  always explicit, `", "`-separated) — the same rule `allow_transfer` and
  `notify_to` follow above, for the same reason.
  **Empty is the default, and it is accepted**: a forwarder that names no
  upstreams still claims its suffix and answers `SERVFAIL` for it. That is
  the point of the type, not a gap — see
  [`docs/architecture.md`](architecture.md).
  **A write that changes where a suffix routes also clears the cache
  beneath it** — creating, patching, deleting or disabling a `forwarder` or
  a `stub`. Without that, a name cached from `upstreams` before the zone
  claimed it would go on being answered from that entry, which for a
  split-horizon zone is the public internet answering an internal name. The
  clearing is scoped to the suffix whose routing changed, so an ordinary
  record write costs nothing.
  `forward_to` is refused with `400` on every other type, and a forwarder
  refuses `primaries`, `tsig_key_id`, `allow_transfer` and `notify_to`: it
  has no master, and it serves no zone for anyone to pull or be told about.
  `POST /zones/{id}/refresh` is refused too — there is nothing to fetch.
  Records are the one thing it does *not* refuse: nothing overwrites them,
  so a write is inert rather than lost, which is a different complaint from
  the `409` a `secondary` or `stub` answers. Nothing serves them either.
- **Stub zones** — a `stub` claims a suffix and routes it exactly as a
  forwarder does, but the addresses are fetched rather than typed. It asks
  its master two ordinary questions — `SOA` for the serial and the schedule,
  `NS` for the delegation with glue in the ADDITIONAL section — and routes
  to the nameservers that come back, on the schedule that SOA publishes.
  **Not an AXFR**, which is the point rather than an optimisation: a stub
  needs no `allow_transfer` permission on the far end, so it works against a
  master that will not transfer its zone to anybody. See
  [`docs/architecture.md`](architecture.md) for the fetch, the glue rule,
  and why a stub does not expire.
  Creating one requires `primaries` and takes an optional `tsig_key_id`, in
  the same columns and with the same meaning a secondary gives them — a
  master that requires TSIG on ordinary queries would otherwise refuse the
  fetch. `forward_to`, `allow_transfer` and `notify_to` are all refused with
  `400`: a stub's upstreams are not typed, and it serves no zone.
  **Its records are read-only** — `POST`/`PUT`/`DELETE` under
  `/zones/{id}/records`, and `POST /zones/{id}/file`, all answer `409` — and
  the reason is stronger than a secondary's rather than milder. A stub
  answers from none of its records, but the routing table is rebuilt *from*
  them on every zone reload, so a hand-written apex NS record would redirect
  the whole claimed suffix until the next fetch undid it. Reads are never
  refused: `GET /zones/{id}/records` lists the fetched NS set and its glue,
  and `GET /zones/{id}/file` exports it.
  `POST /zones/{id}/refresh` fetches now, whatever the schedule says,
  answering the same shape a secondary's transfer does — with one difference:
  `expires_at` is always `0`, because a stub is never given one.
  `refreshed_at`, `last_attempt` and `last_error` mean exactly what they mean
  on a secondary.
- **Zone transfers (outbound)** — any zone dnsaur holds, `primary` or
  `secondary`, can be transferred to another nameserver over AXFR (and
  IXFR, answered with a full AXFR — there is no journal yet to compute a
  delta from). `allow_transfer` on `POST /zones` and `PATCH /zones/{id}` is
  the ACL: a comma-separated list where each entry is an IP address
  (`192.168.1.5`), a CIDR prefix (`10.0.0.0/24`), or `key:<tsig-name>` (the
  request must carry a TSIG that verified under that key) — e.g.
  `"10.0.0.0/24, 192.168.1.5, key:secondary-ns2."`. Entries are OR'd; any
  one match allows the transfer. **Empty is the default and means deny
  every transfer** — a zone created without setting it answers every AXFR
  `REFUSED`. Unlike `primaries`/`tsig_key_id`, `allow_transfer` is not
  secondary-only: a secondary re-serves what it pulled, so its own copy can
  be transferred onward too. A `key:` entry must name a TSIG key that
  exists at write time — checked the same way `tsig_key_id` is — and `400`s
  otherwise. **What's stored is the canonical spelling, not what was
  typed**: on write the value is re-parsed and re-formatted (lowercase,
  fully-qualified key names, `", "`-separated), so a later `GET` can read
  back a string that differs from the one sent while still matching the
  same peers.

  Three read-only fields record the most recent transfer *request* —
  distinct from `refreshed_at`/`last_attempt`/`last_error` above, which
  record this zone's own attempts to pull from *its* primary:
  `last_xfr_at` (unix ms of the last time any peer asked, `0` = never),
  `last_xfr_peer` (the address that asked, `""` when none has), and
  `last_xfr_error` (why that request was refused, in the server's own
  words, or `""` when it was served). **Only a request that arrived over
  TCP is recorded.** A zone transfer is a TCP protocol, so a UDP arrival —
  the `NOTIMP` a UDP AXFR gets, the single-SOA reply a UDP IXFR gets, or a
  UDP peer the `allow_transfer` gate refuses — is not a transfer attempt,
  it is a peer using the wrong transport, and none of it touches these
  three columns; it is logged and nothing more. That makes `last_xfr_peer`
  what it looks like: a UDP source address is trivial to spoof, a TCP
  handshake off-path is not, so recording only what arrived over TCP is
  what lets this column be read as proof of who last asked rather than a
  claim that has to be caveated. The column is still throttled, to bound
  how fast a peer that *did* complete a TCP handshake can make the server
  write it. See [`docs/architecture.md`](architecture.md) for what each
  rcode a refused transfer carries actually means.
- **Zone NOTIFY (outbound)** — `notify_to` on `POST /zones` and
  `PATCH /zones/{id}` is who this zone tells when it changes (DNS NOTIFY,
  RFC 1996): a comma-separated list where each entry is `host[:port]` (port
  defaulting to 53), optionally suffixed `key:<tsig-name>` to sign that
  target's NOTIFY — e.g. `"10.0.0.2, 10.0.0.3:5353 key:secondary-ns2."`.
  **Empty is the default and means notify nobody.** Applies to both
  `primary` and `secondary` zones — a secondary re-serving what it pulled
  has its own downstream secondaries to tell — unlike `primaries` and
  `tsig_key_id`, which are secondary-only. A `key:` entry must name a TSIG
  key that exists at write time, `400`s otherwise, checked the same way
  `allow_transfer`'s is. **What's stored is the canonical spelling, not
  what was typed**: the value is re-parsed and re-formatted on write (the
  port always explicit, lowercase fully-qualified key names,
  `", "`-separated) — the same rule `allow_transfer` follows above, for the
  same reason.

  Delivery itself is a background process
  ([`docs/architecture.md`](architecture.md)'s outbound NOTIFY queue) driven
  off the zone's `soa_serial`, not synchronous with the write that set
  `notify_to` — there is no endpoint that sends a NOTIFY on demand.

  `GET /zones/{id}/notifies` reports one row per `host[:port]` in the
  zone's own `notify_to`, whatever the zone's own type, with eight fields:
  `target` (the address, without its key — re-keying a target keeps its
  history, since the key is looked up by name at send time rather than
  stored on the row), `state` (below), `notified_serial` (the last serial
  this target acknowledged; meaningless until `notified_at` is non-zero),
  `notified_at` (unix ms of the last round that landed, `0` = never
  delivered), `attempts` (how many times the *current* round has been
  tried — a target that spent its whole budget against an older serial
  reports `0` here as soon as the zone's serial moves, because that is a new
  round), `max_attempts` (the round budget, currently `5`, sent alongside
  `attempts` so a client never hardcodes the denominator), `last_error`
  (the most recent failure in the sender's own words, `""` when the last
  attempt landed or none has been tried), and `created_at` (unix ms the
  target was first seen — never moves, so it dates a target whose state is
  still `never`). A zone with no `notify_to` returns `[]`, never `null`.

  `state` is derived server-side — never left to a client to infer from the
  other columns, so two clients can never disagree about it — and is one of
  four values:
  - **`never`** — no round has ever been tried (`attempts` is `0` and
    nothing has landed). Nothing for the operator to do; the target simply
    hasn't had its turn yet.
  - **`current`** — the target has acknowledged the zone's present
    `soa_serial` (an RFC 1982 comparison, so a wrapped serial is never
    mistaken for current). Nothing to do.
  - **`retrying`** — behind the current serial, and `attempts` is still
    under `max_attempts`. Mid-round; nothing to do yet.
  - **`gave_up`** — behind the current serial, and the *current round* has
    reached `max_attempts`. **This is not a failure the operator needs to act
    on**: the round simply rests, and it starts over from `attempts: 0` on
    the next serial bump — the zone's own next edit — with no action
    required. The round is what the state is scoped to, so a target that
    exhausted its budget against serial 100 reads `retrying` the moment the
    zone reaches 101, not when the notifier's next pass gets to it: the
    notifier has already decided to send, and the two must not disagree
    about a word.
    (This is also what a target that has *never* been delivered *and* has
    exhausted its attempts reports — `never` means "not yet tried", not
    "tried and failed", so that combination is `gave_up`, not `never`.)

  None of the four states requires operator action for correctness: a
  NOTIFY only ever changes *when* a secondary refreshes, never *whether* it
  does — the target's own SOA `refresh` timer is the backstop regardless of
  how a round ends.
- **Zone records** — `GET /zones/{id}/records`, `POST /zones/{id}/records`,
  `PUT /zones/{id}/records/{rid}`, `DELETE /zones/{id}/records/{rid}` —
  records within a zone, named relative to its apex (`@`, `bifrost`, `*`,
  `*.nexus`; a fully-qualified name has the apex stripped automatically).
  `rdata` is DNS presentation format, validated by handing it to the same
  DNS parser (`miekg/dns`) that builds the record dnsaur serves, so a `400`
  carries that parser's own error text. **What is stored is that parser's
  own spelling of the value, not the text you sent**, so a later `GET` can
  return a string that differs from the one you wrote: `nas.example.com`
  comes back `nas.example.com.`, `hello` comes back `"hello"`,
  `2001:0db8::0001` comes back `2001:db8::1`. The record answers exactly
  the same either way — a name in rdata without a trailing dot is read as
  absolute, not relative to the zone — but only the stored spelling still
  means that when the zone is exported to a file, where a dotless name
  *is* relative. `PTR` is a normal record type here
  like any other — its `rdata` is a domain name, not an address (RFC
  1034); the address is encoded in the record's `name` instead. Three
  write conflicts return `409`: a CNAME beside another record at the same
  name (RFC 1034 §3.6.2), a CNAME at the zone apex (RFC 1912 §2.4), and a
  TTL that disagrees with the rest of an RRSet (RFC 2181 §5.2) — plus a
  fourth, any write at all under a zone whose contents are authored
  elsewhere: `internal`, `secondary` or `stub` (see those sections above).
  A `forwarder` is deliberately not in that list — nothing overwrites its
  records, so a write into one is inert rather than lost.
  Creating, updating or deleting an `A`/`AAAA` record also writes, moves,
  or removes the matching `PTR` in whichever enabled `primary` zone covers
  that address, inside the same request — see
  [`dashboard.md`](dashboard.md#auto-ptr) for the exact rules (no reverse
  zone means no PTR and no error; an address that already has a PTR keeps
  it; a hand-written PTR is never touched). `DELETE /zones/{id}` does the
  same for the whole zone at once: the zone's own records go with it, and
  every PTR they owned is retired from the reverse zone that outlives
  them, under the same check. This can bump the **SOA serial of a
  different zone** than the one in the request path — the reverse zone's,
  not just the forward zone's — since its contents just changed too.
- **Zone files** — `GET /zones/{id}/file` renders the zone as a BIND
  master file (RFC 1035 §5) and returns it as an attachment; disabled
  records are omitted (a master file can't express "present but disabled"),
  and a built-in zone exports like any other. `POST /zones/{id}/file`
  (`{content, dry_run}`) replaces the zone's records and SOA from a master
  file — the file is the zone, and anything the zone has that the file
  doesn't is deleted. `dry_run` is required, not defaulted: a request that
  omits it gets `400`, because Go's zero value for a bool is the committing
  one, and the whole point of the flag is that a caller can't reach the
  destructive branch by accident. `true` returns the diff (`add`, `change`,
  `delete`) and writes nothing; `false` applies it and returns the same
  shape. Records are validated exactly as `POST /zones/{id}/records`
  validates a hand write, against the file's own records rather than the
  zone's current ones; any failure rejects the whole file with `422` and no
  writes. A committed import is applied as one database transaction — every
  delete, change and add plus the new SOA serial land together or not at
  all — so a `503` from a storage failure leaves the zone exactly as it
  was, and the same file can simply be posted again. The `422` body carries `errors` — one message per problem, not
  all in one shape: most name `line N` or the record's name/type/rdata (a
  `$GENERATE` line expands to several with no line of their own), but a
  few — a missing SOA, say — are about the file as a whole and name
  neither. It comes alongside a flat `error` summary, so a client with one
  error handler for the `{"error": "<message>"}` convention above still
  finds what it expects. A file whose SOA carries no TTL is one such
  problem and is always rejected: a zero SOA TTL would make every
  negative answer from the zone uncacheable (RFC 2308 §4). On success
  the SOA's NS, mbox and timers come from the file, but the serial becomes
  `max(file, current) + 1` — never lower than what's already been served,
  so a secondary that has seen the higher value doesn't ignore the zone
  forever after. Import never writes PTR records, the one exception to
  auto-PTR (see [`dashboard.md`](dashboard.md#auto-ptr)): a forward-zone
  import never rewrites a reverse zone it didn't name, so a reverse zone's
  PTRs come from importing that zone's own file. Import is refused with
  `409` into every zone whose contents are authored elsewhere — `internal`,
  `secondary` and `stub` — like any other write to one (see those sections
  above). Export is unaffected: a built-in, a secondary and a stub all
  render. A `forwarder` renders too — a valid master file carrying
  `$ORIGIN`, `$TTL` and the zone's SOA, with no records under it, since it
  holds none. That emptiness is why the dashboard offers Export on a stub
  and not on a forwarder.
- **TSIG keys** — `GET /tsig-keys`, `POST /tsig-keys`, `GET /tsig-keys/{id}`,
  `PUT /tsig-keys/{id}` (full replace — `name`, `algorithm` and `secret` are
  all required, same as create), `DELETE /tsig-keys/{id}`. A TSIG key (RFC
  8945) authenticates a zone transfer between dnsaur and a peer. Changes take
  effect on the next signed message: the DNS server reads keys from the store
  per message, so a create, edit or delete needs no restart. `name` is
  normalised to canonical form (lowercase, trailing dot) on every write, so
  it can't miss the key a signed message names on case or a missing dot.
  `algorithm` must be one of `hmac-sha1.`, `hmac-sha224.`, `hmac-sha256.`,
  `hmac-sha384.`, `hmac-sha512.` — `hmac-md5.sig-alg.reg.int.` is a real RFC
  8945 name but is rejected, because `miekg/dns` removed MD5 support from
  its signer and a key created with it could never sign. `secret` is base64
  and is validated as such at write time, since that's what gets decoded at
  sign time. **Unlike an API token, the secret is returned on every read**
  (list and get) rather than shown once: it has to be pasted, unchanged,
  into the matching key on the peer (BIND's `key{}` clause, Technitium's
  transfer settings), so hiding it after creation would make the feature
  unusable without direct database access. For the same reason `secret` is
  stored in the database as plaintext, not hashed — a signing key has to be
  recoverable to sign with, unlike a password or a token. Nothing else in
  dnsaur is encrypted at rest either, but nothing else has to be recoverable,
  so this table specifically makes the database file credential material.
  Verification has two limits inherited from `miekg/dns`, both fail-closed:
  a truncated MAC is rejected — RFC 8945 §5.2.2.1 permits one, but the
  library's comparison, mirrored here, is against the full MAC — and a
  message must be signed with the algorithm the key was created with; one
  signed with a different algorithm is rejected rather than verified under
  the algorithm it names, so a peer-side algorithm upgrade needs the key
  updated here too. Over UDP a signed reply is fitted to the client's
  advertised size with the signature's own bytes counted in. A client that
  advertises no EDNS size has 512 bytes, of which the signature takes about a
  hundred; per RFC 8945 §5.3, an answer that will not fit in what is left
  comes back with no records, `TC` set and rcode `NOERROR`, which is the
  instruction to ask again over TCP — so a truncated `NXDOMAIN` reports its
  real rcode only on the TCP retry. Transfers are TCP and are never
  truncated.
- **Queries** — `GET /queries` (search the query log; filters: `from`,
  `to`, `client`, `q`, `decision`, `type`, `limit` [default 100, capped
  1000], `offset`), `GET /queries/tail` (live tail as Server-Sent Events,
  `text/event-stream`; `503` if query logging is disabled).
- **Stats** — `GET /stats/overview?hours=` (totals: `total`, `blocked`,
  `cached`, `forwarded`, `clients`), `GET /stats/timeline?hours=`
  (decision counts bucketed over time), `GET /stats/top?metric=&n=&hours=`
  (top-N by `domain`, `blocked_domain`, or `client`).
- **Tokens** — `GET /tokens` (list this user's API tokens; session tokens
  and hashes are never included), `POST /tokens` (`{name, scope}`, returns
  the plaintext token once), `DELETE /tokens/{id}` (revoke).

TOTP enrollment/management endpoints are listed under Auth above, not
Tokens — they manage account 2FA, not API tokens.
