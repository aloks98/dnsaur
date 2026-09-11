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
  down. **`503` means a genuine storage failure and nothing else.** A write
  naming a row that does not exist is user input error, not infrastructure:
  it answers `404` when the missing row is the resource the URL addressed,
  and `400` naming the field when it came from the body.
- A `POST` that creates something answers `201` with a **`Location`**
  header naming the new row (`/api/v1/zones/7`) and **the created object**
  as the body — not the bare `{"id": 7}` it used to, which made a client
  build the URL itself out of a number and then fetch the row to see the
  fields the server had filled in. `POST /tokens` is the one create whose
  body is not the stored row: it also carries the plaintext token, which no
  `GET` ever returns. `POST /setup` is the one `201` with no `Location` —
  it creates the admin account, and there is no URL for a user.
- A body that does not decode — malformed JSON, an unknown key, a
  wrong-typed field, an empty body — is always `400 invalid json`, on every
  endpoint. Field validation runs after that and says something about the
  field. Endpoints used to fold the two together, so `{"name": 5}` came
  back as `name required`, a claim about a field that had in fact been
  sent.
- A request naming a real path with a method it does not have is `405
  method not allowed`, with an **`Allow`** header listing the methods that
  path does serve (`DELETE /settings` → `Allow: GET, PUT`). Only a path no
  endpoint serves at all is `404`.
- `GET /health`, `GET /readyz` and `GET /openapi.yaml` are
  unauthenticated; every other endpoint requires auth (`401` if
  missing/invalid).
- **On a replica, writes to synced configuration are `409
  {"error": "managed by <peer_url>"}`.** An instance with `sync.peer_url`
  set follows another instance's configuration, so groups, clients, filter
  lists, rules, TSIG keys, zones and zone records are read-only on it, as is
  `PUT /settings` for any key outside the instance-local set. A blocking
  pause is refused too: the pause state is persisted to the synced
  `blocking.pauses` setting, because a pause is a decision about the network
  and clients reach either box. The answer comes before the handler runs, so
  nothing was written. The two refresh operations
  (`POST /filters/lists/{id}/refresh`, `POST /zones/{id}/refresh`) are
  operational rather than configuration and stay available, and so do this
  box's own account, sessions, tokens and backups. Clearing `sync.peer_url`
  is the promotion and lifts the refusal — see Sync below.

## Auth model

- **First-run setup:** `GET /setup` reports `{"setup_required": bool}`;
  `POST /setup` (`{username, password}`, password min 8 chars) creates the
  first and only admin account — `409` if setup already completed.
- **Sessions:** `POST /auth/login` (`{username, password[, totp_code]}`)
  authenticates and sets an `HttpOnly`, `SameSite=Strict` `dnsaur_session`
  cookie with a 30-day TTL. Sessions are sliding: a request that
  authenticates with less than half the TTL remaining gets silently
  renewed, up to an absolute ceiling of **90 days from the login that
  created it** — past that the session is deleted and you log in again,
  second factor and all. If the account has TOTP enabled and `totp_code`
  is omitted, login fails with `428`; bad credentials return `401`.
  `POST /auth/logout` clears the cookie and revokes the session token.
  `GET /auth/me` returns the current user (`id`, `username`,
  `totp_enabled`).
  `DELETE /auth/sessions` revokes every session on the account **except the
  caller's own** — "log out everywhere" for a cookie you believe has been
  copied — and answers `204`. API tokens are untouched: they are named
  credentials, revoked by name.
- **Password change:** `POST /auth/password`
  (`{current_password, new_password}`) verifies the current password with
  the same argon2id path login uses, enforces the same 8-character minimum
  `POST /setup` does, and answers `204`. It **revokes every other session**
  for the reason enabling TOTP does — a password change means nothing while
  the sessions minted under the old one still work — keeping the caller's
  own session and every API token. A wrong current password is `400
  current password is wrong`, **not** `401`: the request authenticated
  fine, and a `401` would tell a dashboard its session had expired over a
  typo. There is no user enumeration to guard against here, since the
  account is the one the caller is already signed in to.
- **Throttle:** `POST /setup`, `POST /auth/login` and `POST /auth/password`
  share one budget of
  **10 attempts per minute per source address**; exceeding it answers
  `429` (`{"error": "too many attempts"}`) with a `Retry-After` header, in
  seconds, for a one-minute lockout. Every answer costs an attempt — a
  malformed body, a wrong password, and the `428` that says the password
  was right. `POST /auth/password` is on the same budget although it is
  authenticated: it runs the same argon2id verification and says whether
  the password was right, so a stolen session cookie must not be an
  unmetered oracle for the password it is not enough to change. That last one is what keeps the `428`/`401` split from being
  a free password oracle. Behind a reverse proxy, set `trusted_proxies`
  (see [`docs/configuration.md`](configuration.md)) or every request shares
  one source address and one budget.
- **Cookie `Secure`:** set when Go terminated TLS, or when the request came
  from a network named in `trusted_proxies` and its `X-Forwarded-Proto` is
  `https`. `X-Forwarded-Proto` is never believed from anywhere else.
- **API tokens:** scoped, revocable bearer tokens for scripts and other
  clients, created via `POST /tokens` and sent as
  `Authorization: Bearer <token>`. Each token has a `read` or `write`
  scope (`write` is the default if omitted); a `read` token gets `403`
  (`{"error": "read-only token"}`) on anything but `GET`/`HEAD`. The
  plaintext token is only ever shown in the `POST /tokens` response.
  A `read` token also cannot read TSIG secrets: `GET /tsig-keys` and
  `GET /tsig-keys/{id}` answer it with `secret: ""` and
  `secret_redacted: true`. Sessions and `write` tokens get the real secret
  and `secret_redacted: false`.
  The `Authorization` scheme name is matched case-insensitively
  (RFC 9110 §11.1), so `bearer <token>` works as well as `Bearer <token>`.
- **Passwords** are hashed with argon2id; login timing is equalized for
  unknown usernames to avoid leaking account existence. `POST /setup`
  against an install that already has an admin answers `409` without
  hashing anything, and no more than four argon2id computations run at
  once server-wide.
- **Optional TOTP 2FA**: `POST /auth/totp/start` begins enrollment
  (returns a base32 `secret`, an `otpauth_url`, and `qr_png` — that URL
  already rendered as a base64 PNG, so a client can show the QR with an
  `<img src="data:image/png;base64,…">` and needs no QR encoder of its own);
  `POST /auth/totp/confirm` (`{secret, code}`) verifies a code and enables
  it; `POST /auth/totp/disable` (`{code}`) turns it off. Once enabled,
  `totp_code` is required on every login. **A code logs in once**
  (RFC 6238 §5.2): the 30-second time step it matched is recorded, and any
  later login presenting a code from that step or an earlier one is `401`,
  indistinguishable from a wrong code. Enabling or disabling TOTP **revokes
  every other session** — the one that made the change survives, and API
  tokens are untouched.
- **CSRF:** a cookie-authenticated `POST`/`PUT`/`PATCH`/`DELETE` whose
  `Sec-Fetch-Site` header is `cross-site` is refused with `403`
  (`{"error": "cross-site request"}`), behind `SameSite=Strict` rather than
  instead of it. Bearer requests are never checked: a token is not attached
  by the browser on its own, and scripts legitimately send no fetch
  metadata at all.

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

- **Meta** — `GET /health` (liveness + version), `GET /readyz`
  (readiness), `GET /openapi.yaml` (this spec). All unauthenticated.
  **`/health` and `/readyz` answer different questions.** `/health` is 200
  while the process is up — that is what a container runtime restarts on,
  and a server that cannot serve is still one that should not be killed in
  a loop. `/readyz` is 200 only when the store answers a ping *and* at
  least one socket is bound for DNS (plain, DoT or DoH), and `503` with a
  plain reason otherwise (`storage unavailable`, `no DNS listener is
  bound`). A load balancer wants `/readyz`; pointing it at `/health` keeps
  traffic flowing to a process that is up and resolving nothing.
- **Setup** — `GET /setup`, `POST /setup`. See Auth model above.
- **Auth** — `POST /auth/login`, `POST /auth/logout`, `GET /auth/me`,
  `POST /auth/password`, `DELETE /auth/sessions`,
  `POST /auth/totp/start`, `POST /auth/totp/confirm`,
  `POST /auth/totp/disable`.
- **Settings** — `GET /settings` (flat key→string map of all editable
  settings; `instance.*` keys, the bookkeeping rows — `stats.watermark`,
  `blocking.pauses`, `sync.replicas`, `sync.applied_version`,
  `sync.applied_at`, `sync.last_pull_at`, `sync.last_error` — and the
  write-only `sync.token` are omitted),
  `PUT /settings` (`{key, value}` for one, or a flat
  `{"<key>": "<value>", ...}` map for several at once; editable keys:
  `upstreams`, `upstream.strategy`, `blocking.mode`, `blocking.ttl`,
  `cache.min_ttl`, `cache.max_ttl`, `cache.max_entries`,
  `cache.serve_stale_for`, `lists.refresh_hours`, `qlog.retention_days`,
  `qlog.privacy`, `stats.retention_days`, `serve.dot.enabled`, `serve.dot.listen`,
  `serve.doh.enabled`, `serve.doh.listen`, `serve.tls.cert`,
  `serve.tls.key`, `sync.peer_url`, `sync.token`, `sync.interval_seconds`,
  `sync.primary_dns`, `sync.tsig_key_id` — see [`docs/configuration.md`](configuration.md) for
  what each means and which require a restart to take effect). **A map is
  all-or-nothing**: every key is validated before any of them is written,
  the write lands in one transaction, and there is one config-version bump
  — so a rejected body changes nothing and a six-field save reconfigures
  the running server once instead of six times. The keys are applied in
  the order their dependencies require (protocols off, then
  `serve.tls.cert`, then `serve.tls.key`, then everything else, then
  protocols on), which is what lets a certificate pair and the enable that
  depends on it travel in one request. Every value is a string, numbers
  included; a number or a null is `400 invalid json`. Enabling
  `serve.dot.enabled`/`serve.doh.enabled` requires `serve.tls.cert` and
  `serve.tls.key` to already name a loadable certificate pair, or the
  write is rejected naming which to set first; and clearing either path
  while a protocol is still enabled is rejected for the same reason, from
  the other direction. `serve.dot.listen`/`serve.doh.listen` take
  `host:port` with the port in 1-65535 — port 0 is refused, since it binds
  whatever is free and reports an address no client was ever told. The
  integer keys take zero and up, except `lists.refresh_hours`, which takes
  1 and up: it becomes the refresh timer's interval, and `0` is not a
  slower schedule but one no timer can be built from; and
  `stats.retention_days`, also 1 and up, because `0` would have the next
  prune delete every hourly bucket the dashboard reads. There is no separate
  "upstreams" resource — upstream servers live in the `upstreams` setting.
  The five `sync.*` keys configure this instance's relationship to a main:
  `sync.peer_url` is empty (this is a main) or an absolute `http`/`https`
  URL, and setting it non-empty requires `sync.token` — already stored, or
  sent in the same map, which is why `sync.token` is judged before it.
  `sync.token` is the one editable key `GET /settings` never returns: it is
  the credential this instance pulls with, so the screen shows set/not set
  and a read is not a way to copy it out. `sync.interval_seconds` is 5 or
  more, `sync.primary_dns` is `host:port` with the host required (empty
  means "the peer URL's host on port 53"), and `sync.tsig_key_id` is `0` or
  the id of a TSIG key that exists.
- **Resolver status** — `GET /resolver/status`
  (`{encryption_downgraded, reason, serving, certificate, sync}`). Server state
  rather than a setting, which is why it is not in the `GET /settings`
  map. `encryption_downgraded`/`reason` cover the upstream side exactly as
  before: true when the stored `upstreams` value named `tls://` or
  `https://`, failed to parse, and the server fell back to its hardcoded
  **plaintext** default resolvers — so queries are travelling in the clear
  while the settings page still shows the encrypted value. It clears as
  soon as a settings apply installs a forwarder built from the stored
  value. `serving.dot`/`serving.doh` report the encrypted *serving* side
  instead — each `{enabled, listening, addr, error}`, since a protocol can
  be enabled but not actually bound (a taken port, a certificate that will
  not load). A failed bind is retried every 30 seconds, so this clears on
  its own once the cause is gone. `certificate` is present only while a
  certificate is actually in use — absent when neither protocol is enabled,
  when no paths are configured, and when none has ever loaded — and carries
  `{not_after, expiring_soon}`, the 14-day expiry warning. See
  [`docs/configuration.md`](configuration.md#encrypted-serving) for the
  full story, including why a bind failure surfaces here rather than only
  in the log.
- **Backup** — `POST /backup` (write scope). Takes a copy of the database
  with SQLite's `VACUUM INTO`, into `<data_dir>/backups/dnsaur-<UTC
  timestamp>.db`, and answers `201 {path, bytes}` with `Location` carrying
  that same path. The directory is created `0700`, and the copy is written
  under a temp name and renamed, so an interrupted backup never leaves a
  half file that reads as a complete one. `VACUUM INTO` reads through a
  transaction, so the copy includes everything still in the WAL and nothing
  has to be stopped first — it does hold SQLite's single connection while
  it runs, so other queries wait. The file stays on the server's disk; no
  endpoint serves it, and copying it somewhere else is the operator's job.
  **There is no schedule and no retention** — nothing deletes an old
  backup. On `storage.driver: postgres` the answer is `409` with
  `backups are a sqlite feature; use pg_dump for postgres`.
- **Blocking** — `GET /blocking?group_id=` or `?client_id=` (pause status,
  answering `{paused_until, scope}` where `scope` is `global`, `group` or
  `client`), `POST /blocking/pause` (`{[group_id | client_id,] minutes}`,
  pauses 1–1440 minutes), `DELETE /blocking/pause?group_id=` or
  `?client_id=` (resume/cancel that scope's pause). Send one id or neither,
  never both — both is a 400. Neither means the global pause, which is what
  `group_id=0` has always meant. A group inherits the global pause and a
  client inherits its group's and the global one; whichever ends later
  wins, and `scope` says which that was, so a control knows whether its own
  resume would change anything. Pauses are stored and survive a restart —
  one that ran out while the server was down does not come back with it.
- **Groups** — `GET /groups`, `POST /groups` (`{name[, enabled][, list_ids]}`
  — `enabled` defaults to true; `list_ids` omitted assigns **every** list,
  while an explicit `[]` assigns none), `PATCH /groups/{id}`
  (rename/enable/disable), `DELETE /groups/{id}`,
  `GET /groups/{id}/lists` and `PUT /groups/{id}/lists` (assign filter
  lists to a group), `GET /groups/{id}/rules` and `POST /groups/{id}/rules`
  (per-group allow/block rules, literal or regex pattern). A literal
  pattern must be a domain (`example.com`, `*.example.com`, or a single
  label like `localhost`) and is stored normalised — lowercased, trailing
  dot and leading `*.` removed, Unicode converted to punycode. Anything
  else is `400 pattern must be a domain like example.com, *.example.com or
  localhost`.
  Every `list_ids` entry must name a list that exists — one that doesn't is
  a `400` naming the id, on create and on `PUT` alike, and creates nothing.
  **`PUT /groups/{id}/lists` is one transaction**: a bad id leaves the
  group's current assignments exactly as they were, rather than the
  half-emptied set the old unassign-then-assign loop left behind. Duplicate
  ids in the array are a set, not an error. Every route under
  `/groups/{id}` answers `404` for a group that does not exist, reads
  included — `GET /groups/999/lists` is `404`, not `200 []`.
- **Clients** — `GET /clients` (optionally `?group_id=` for one group's
  clients; a group nobody created is an empty array, since the parameter
  narrows a listing rather than addressing a resource, and a `group_id` that
  is not a positive id is a `400`), `POST /clients`, `PUT /clients/{id}`,
  `DELETE /clients/{id}` — each client is an IP or CIDR `matcher` bound to
  a `group_id`. The matcher is stored canonically: CIDRs are masked,
  IPv4-mapped IPv6 is unmapped. An interface zone (`fe80::1%eth0`) is
  `400`, because the request side never carries one. A `group_id` naming no
  group is `400 group_id does not name an existing group`; the same
  reference from the *path* (`POST /groups/{id}/rules`) is a `404` instead,
  since there the missing row is the resource the URL addressed.
- **Filters** — `GET /filters/lists`, `POST /filters/lists` (subscribe a
  block/allow list URL; downloads in the background after responding),
  `PATCH /filters/lists/{id}` (enable/disable),
  `DELETE /filters/lists/{id}`, `DELETE /filters/rules/{id}` (delete a
  per-group rule), `POST /filters/refresh` (`202`, kicks off an
  asynchronous refresh of all lists), `POST /filters/lists/{id}/refresh`
  (`202`, downloads **that one list** and answers with its updated row —
  the work runs inside the request, so unlike the all-lists refresh there
  is nothing to poll for; a disabled list is `409`).
  Every other rule/list/assignment write **recompiles from the copies
  already on disk and does not download**, so none of them wait on the
  network. A list whose URL resolves to a loopback, link-local or private
  address is refused at fetch time and reads back `last_status: "failed"`;
  the same goes for a body over 64 MiB.
  Rows from `GET /filters/lists` also carry `next_refresh_at`, unix ms of
  the periodic download's next tick (`0` when no cadence is running). It
  comes from the running ticker, not the database, and is the same on every
  row — the interval is server-wide and there are no per-list schedules.
- **Zones** — `GET /zones` (optionally `?type=` — `primary`, `secondary`,
  `forwarder`, `stub` or `internal` — and `?enabled=true|false`; an empty
  parameter is no filter, and a value neither accepts is a `400` rather than
  an empty array, so a mistyped filter cannot read as "you have none"),
  `POST /zones`, `GET /zones/{id}`,
  `PATCH /zones/{id}`, `DELETE /zones/{id}` (cascades its records),
  `POST /zones/{id}/clone` (`{name}` — a second zone holding this one's
  configuration and every one of its records under a new apex; `201` with
  `Location` and the new zone).
  `PATCH` is conditional on the zone not having changed since it was read:
  two patches touching different fields no longer overwrite each other, and
  only a second collision answers `409 zone changed since it was read`.
  Renaming a `primary`, or disabling or re-enabling one, moves the PTR
  records its `A`/`AAAA` records own — retired under the old name and
  re-added under the new, the same rules `DELETE /zones/{id}` follows.
  A name inside an enabled zone is answered or routed by that zone, never
  handed to the default upstreams. `name` alone is enough to create a
  `primary` — SOA fields default to generated values, and an apex NS record
  is created alongside it.
  A zone `name` is restricted to labels of letters, digits, hyphen and
  underscore: `miekg/dns`'s own check is deliberately liberal and accepts
  `;`, `"`, `!` and NUL, none of which survive a zone-file export (`;`
  opens a comment) or a `Content-Disposition` header. `soa_ns` and
  `soa_mbox` take the same labels and are checked on `POST` and `PATCH`
  alike — an unchecked `soa_ns` used to produce an apex NS record whose
  rdata the DNS parser rejects, silently dropped from answers and fatal to
  every outbound AXFR. `soa_mbox` is a mailbox written as a domain name
  (RFC 1035 §8), so a dot in the local part is escaped:
  `first\.last.example.com` for `first.last@example.com`. Both are stored
  with any trailing dot removed. The seeded apex NS goes through the same
  validator a hand-written record does. **That apex NS is seeded for a `primary` only**:
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
- **Cloning a zone** — `POST /zones/{id}/clone` with `{"name": "e412.dev"}`
  creates a second zone from an existing one: a second site, a staging
  domain, a vanity alias. Everything is copied verbatim except the name and
  the serial — `type`, the SOA fields, `allow_transfer`, `notify_to`,
  `primaries`, `tsig_key_id`, `forward_to`, `enabled`, and every record with
  its TTL, rdata, enabled flag and comment. Records need no rewriting,
  because they are stored relative to the apex; `rdata` and the SOA's two
  names are copied as written, since an absolute name in either is a
  deliberate one. `soa_serial` restarts at `1` like any other new zone, and
  the transfer history is not copied at all — the copy has pulled nothing and
  been asked for nothing. The name is normalised and checked exactly as
  `POST /zones` does it: `400` if it is not a domain name, `409` if it is
  already in use. Cloning an `internal` zone is `409`, as creating one is.
- **Secondary zones** — a `secondary` is a copy of a zone held elsewhere,
  pulled over AXFR and kept fresh on the schedule its own SOA publishes — the
  scheduled attempt asks for the primary's serial first and transfers only if
  it has moved — or sooner if one of its `primaries` sends it a DNS NOTIFY. See
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
  is the last attempt that **succeeded** — a transfer that landed, or a
  scheduled check whose SOA probe found the primary already at this zone's
  serial, which RFC 1034 §4.3.5 treats alike, since both are the primary
  confirming what this zone holds; `last_attempt` is the last one **tried**,
  successful or not; and `last_error` is why that attempt failed, in the
  transfer's own words, or `""` when it succeeded. The last two are
  written together and survive a restart — the scheduler's own view of a
  failure does not — so they are the honest answer to "is this zone
  working", and `last_error` should always be read beside `last_attempt`.
  Two more fields carry the part that has no column: `next_attempt_at` (unix
  ms, `0` when no back-off is pending) is when the scheduler will next try,
  and `failures` is how many attempts have failed in a row. Both come from
  the running process, so a restart reports `0` for both while the zone is
  still failing — read them beside `last_error`, never instead of it.
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
  delta from — unless the requesting client's own serial is already this
  server's or newer, which RFC 1995 §2 answers with a single SOA and this
  server does). `allow_transfer` on `POST /zones` and `PATCH /zones/{id}` is
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
  `PATCH /zones/{id}/records` (bulk TTL, below),
  `PUT /zones/{id}/records/{rid}`, `DELETE /zones/{id}/records/{rid}` —
  records within a zone, named relative to its apex (`@`, `bifrost`, `*`,
  `*.nexus`; a fully-qualified name has the apex stripped automatically).
  `name` must be a domain name the zone's export can carry, which rules out
  whitespace, `;`, `"`, `(`, `)`, `\`, `/`, an empty label and a leading
  `$` — every one of them changes how the exported line reads back — and
  `400` says so. `type` must be one the server can serve: an RFC 3597
  `TYPE65280` is refused with the type named, since nothing could ever
  match it, and `SOA` is refused because a zone's SOA lives on the zone
  itself (`PATCH /zones/{id}`), not among its records.
  `rdata` is DNS presentation format, validated by handing it to the same
  DNS parser (`miekg/dns`) that builds the record dnsaur serves, so a `400`
  carries that parser's own error text. A bare `@` in `rdata` means the
  zone apex, the same thing it means in a zone file — `{"type":"MX",
  "rdata":"10 @"}` is stored as `10 <zone>.` — while a `@` in a value that
  is text rather than a name (a `TXT`, say) stays the character it is. **What is stored is that parser's
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
- **Bulk TTL** — `PATCH /zones/{id}/records` with
  `{"ids": [12, 13], "ttl": 60}` sets one TTL across the records named and
  changes nothing else about them. Retuning a zone before a migration is the
  case it exists for: one row at a time is one serial bump, one snapshot
  rebuild and one NOTIFY pass per record. The whole edit is one transaction
  and one serial bump, so it lands entirely or not at all. `204` on success.
  The ids are always explicit — there is no "everything matching a filter"
  form, because the filter belongs to the client and re-deriving it here
  would be a rule the two ends could disagree about on a request that
  rewrites rows. Each changed record is validated exactly as a single write
  is, and against the set **as it will be**: changing half an RRSet is a
  `409` under RFC 2181 §5.2, while changing both halves in one request is
  fine. An empty `ids` or a `ttl` past the RFC 2181 §8 ceiling is `400`; an
  id that is not one of this zone's records is `404` and applies none of the
  edit; the `internal`/`secondary`/`stub` refusal is the same `409` the other
  record routes give. **auto-PTR deliberately does not run** — a PTR's TTL is
  the only thing this could change about the reverse, and it is cosmetic,
  where what makes a PTR correct is the name it points at.
- **Zone files** — `GET /zones/{id}/file` renders the zone as a BIND
  master file (RFC 1035 §5) and returns it as an attachment (the
  `Content-Disposition` filename is built with `mime.FormatMediaType`, so
  it is always a well-formed header); disabled
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
  finds what it expects. A file that expands to more than 100,000 records
  is one of the whole-file problems: `$GENERATE` turns a single line into
  up to 65,536 of them, so the byte limit on the request bounds the file
  and not the zone it describes. A file whose SOA carries no TTL is one such
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
  all required, same as create), `DELETE /tsig-keys/{id}`.
  **A `PUT` that renames a key a zone still names is refused with `409
  resource in use`**, the same guard `DELETE` applies and for the same
  reason: `allow_transfer` and `notify_to` carry the key's *name*, so
  renaming it leaves the zone naming a key that no longer exists and every
  signed transfer refused with nothing to say why. Everything else stays
  editable on a referenced key — rotating the secret is most of what `PUT`
  is for — and a key referenced only by a zone's `tsig_key_id` renames
  freely, since that reference is by id. `name` takes the same labels a
  zone name does. A TSIG key (RFC
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
  `text/event-stream`; `503` if query logging is disabled). An idle stream
  writes an SSE comment frame (`: ping`) every 20 seconds so a reverse
  proxy's idle-read timeout does not close it; an `EventSource` ignores
  comments, a hand-rolled parser has to. A blocked row
  carries `matched`, the rule pattern or list entry that fired — `rule_id`
  and `list_id` name the rule or the list, not the line of it. It is `""`
  for anything that was not blocked, and for rows logged before the column
  existed.
- **Stats** — `GET /stats/overview?hours=` (totals: `total`, `blocked`,
  `cached`, `forwarded`, `clients`, plus `dropped` — query log entries
  discarded since start because the write buffer was full, so a non-zero
  value means the totals beside it are undercounts), `GET /stats/timeline?hours=`
  (decision counts bucketed over time), `GET /stats/top?metric=&n=&hours=`
  (top-N by `domain`, `blocked_domain`, or `client`). `hours` defaults to
  24 and is capped at 8784 (24 × 366): a larger window overflowed the
  duration arithmetic and asked for a span in the future, which answers
  nothing.
- **Sync** — `GET /sync/version`, `GET /sync/bundle`, `GET /sync/status`,
  `POST /sync/replicas`, `DELETE /sync/replicas/{instance_id}`. Two dnsaur
  instances serve one network so either can answer when the other is down;
  these are how the second one keeps the same configuration as the first.
  An instance with `sync.peer_url` set is a **replica** and pulls; the
  instance it points at is the **main** and does not learn anything it did
  not already know, except that a replica exists.
  `GET /sync/version` answers `{config_version, instance_id}` — the cheap
  probe a replica makes every `sync.interval_seconds`, so the bundle is
  only fetched when the version moved. Any scope.
  `GET /sync/bundle` answers the whole synced configuration: settings
  (everything except the instance-local `instance.*`, `serve.*`, `sync.*`
  and `stats.watermark`), groups, clients, lists with their group
  assignments, rules, TSIG keys and zone *definitions*. Zone records are
  deliberately absent — a replica gets those by AXFR, on the schedule the
  SOA gives. Ids are the main's and are kept on the replica, so a
  `group_id` or a query log's `rule_id` means the same row on both boxes.
  **It needs write scope, not read.** Scope is enforced by method
  everywhere else and this is a `GET`, but the body carries every TSIG
  secret on the box — the same credentials a read token stopped being
  handed through `GET /tsig-keys` — so a read-scoped token is `403
  {"error": "write scope required"}`.
  `POST /sync/replicas` (`{instance_id, dns_addr, version_applied}` → 204)
  is what a replica calls after applying a bundle. Registration is what
  turns on the implicit transfer allow — an AXFR for a primary zone is
  accepted when it verified under the main's `sync.tsig_key_id` **and**
  came from a registered replica's `dns_addr` — and adds that address as a
  NOTIFY target for every primary zone, so neither needs an ACL edit and
  `allow_transfer` still says exactly what the operator wrote. `dns_addr`
  must be `host:port` with the host present; `last_seen` is stamped by the
  main rather than sent, so a replica's clock cannot decide whether it
  looks stale. Write scope, for the same reason: an unauthenticated peer
  must not be able to register itself into a transfer allow.
  `DELETE /sync/replicas/{instance_id}` removes one — the operator's
  action, since a replica that stopped pulling is shown as stale and never
  removed automatically. Both are idempotent.
  `GET /sync/status` answers the same object `GET /resolver/status` carries
  as `sync`: `role` (`main` or `replica`), and then either the replica half
  (`peer_url`, `peer_version`, `applied_version`, `applied_at`,
  `last_pull_at`, `last_error`, `plain_http`) or the main half (`sync_key`,
  `replicas[]` with `instance_id`, `dns_addr`, `version_applied`,
  `last_seen`, `stale`). An instance with no sync configured reads as a
  main with no replicas.
- **Tokens** — `GET /tokens` (list this user's API tokens; session tokens
  and hashes are never included), `POST /tokens`
  (`{name, scope[, expires_at]}`, returns the plaintext token once
  alongside the `id` of the row that was actually inserted),
  `DELETE /tokens/{id}` (revoke — `404` means this user owns no token with
  that id, and only that; a storage failure is a `503`).
  `expires_at` is optional, unix ms, and must be in the future — a stamp
  already past would mint a credential that is `401` on its next use, so it
  is `400`. Omitted means never, which is the default and what every token
  created before the field existed carries. Once it passes, the token is
  `401` and is deleted on the attempt. **An API token's expiry does not
  slide**, unlike a session's: it is a date its owner chose, and renewing it
  on every use would mean a token set to die in a month never dies.

TOTP enrollment/management endpoints are listed under Auth above, not
Tokens — they manage account 2FA, not API tokens.
