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
  Authoritative DNS zones: a name inside an enabled zone is answered or
  refused, never forwarded upstream. `name` alone is enough to create one —
  SOA fields default to generated values, and an apex NS record is created
  alongside it. `type` may be `"primary"` or `"secondary"`; `stub` and
  `forwarder` exist in the schema but 400 today. `type: "internal"` is the fifth: the
  built-in zones (`localhost` plus every RFC 6303 §4 reverse zone except
  the private ranges — `BuiltinZones` in `internal/store/builtins.go` has
  the exact list), seeded at migration and never created through this
  API. Every write to an `internal` zone or its records — `PATCH` or
  `DELETE` on the zone, or any write under `/zones/{id}/records` — is
  refused with `409`. A reverse zone for your own network isn't a distinct
  type; it's an ordinary `primary` zone whose `name` ends in
  `in-addr.arpa` or `ip6.arpa` (e.g. `168.192.in-addr.arpa`).
- **Secondary zones** — a `secondary` is a copy of a zone held elsewhere,
  pulled over AXFR and kept fresh on the schedule its own SOA publishes.
  Creating one requires `primaries` — comma-separated `host[:port]`, port
  53 by default, stored as written and resolved at transfer time — and
  takes an optional `tsig_key_id` naming the key to sign transfers with.
  Both fields are refused with `400` on any other type. **Its records are
  read-only**: `POST`/`PUT`/`DELETE` under `/zones/{id}/records`, and
  `POST /zones/{id}/file`, all answer `409`, because the next transfer
  would replace whatever they wrote. It answers `SERVFAIL` for its whole
  suffix before its first transfer lands and again once `expires_at`
  passes — never `NXDOMAIN`, which would be an authoritative claim it is in
  no position to make.
  `POST /zones/{id}/refresh` transfers one now, whatever the schedule says.
  It answers only when the transfer has finished — `200` with the primary
  that answered, the serial and the record count, or `502` carrying the
  transfer's own error. A failed transfer changes nothing: the zone keeps
  the records, serial and `refreshed_at` it already had.
  Three read-only fields on the zone describe all of this. `refreshed_at`
  is the last transfer that **succeeded**; `last_attempt` is the last one
  **tried**, successful or not; and `last_error` is why that attempt failed,
  in the transfer's own words, or `""` when it succeeded. The last two are
  written together and survive a restart — the scheduler's own view of a
  failure does not — so they are the honest answer to "is this zone
  working", and `last_error` should always be read beside `last_attempt`.
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
  fourth, any write at all under an `internal` zone (see Zones above).
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
  PTRs come from importing that zone's own file. Import into a
  `type: "internal"` zone is refused with `409`, like any other write to
  one (see Zones above) — export is unaffected.
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
