# dnsaur UI contract

What the dashboard and the API actually do **today**, on `main`. Every
endpoint, field, enum value and error string below was read out of the Go
source and then verified against a running instance —
the example responses are real captures from a seeded SQLite instance, not
hand-written samples.

Anything not implemented is marked **TODO**. Nothing here is extrapolated from
the design specs; where a spec promised something the code doesn't do, it is
listed as TODO with the gap named.

Related: [`api.md`](api.md) is the human-facing API guide,
[`dashboard.md`](dashboard.md) is the user guide to the screens themselves,
[`architecture.md`](architecture.md) covers the resolver pipeline. This
document is the contract a UI can be built against.

---

## 1. Ground rules

**Base path** — `/api/v1`. Everything non-`/api` falls through to the embedded
SPA, so `GET /settings` returns `index.html`, not JSON.

**Error envelope** — every error is `{"error": "<string>"}` with
`Content-Type: application/json; charset=utf-8` (`errJSON` over `writeJSON`,
`internal/api/server.go`). The strings quoted in this document are verbatim;
they are what a user will see — **the string only**, with no RFC citation or
other commentary appended, which §2.6's record conflicts are the place that
most often gets written down wrong.

A body that is *not* the envelope never comes from dnsaur — it comes from
whatever sits in front of it, which answers its own 502/413/504 page. The
client falls back to `HTTP <status>` for those (`web/src/api/client.ts`):
`Response.statusText` is empty over HTTP/2, so trusting it left such a failure
with an empty message and the UI showing an empty toast.

*Citations in this section name functions, not line numbers* — four of them
pointed about a hundred lines off by D6, because a line number is a fact
about a file's history rather than about its behaviour. The rest of the
document still carries line-number citations, unverified since they were
written; see §10 item 16.

**Auth** — a `dnsaur_session` cookie or `Authorization: Bearer <token>`. Bearer
wins if both are sent (`Server.requireAuth`, `internal/api/server.go`). The
scheme name is matched case-insensitively (RFC 9110 §11.1), so `bearer` and
`BEARER` work too.

| Cookie attribute | Value |
|---|---|
| Name | `dnsaur_session` |
| `Path` | `/` |
| `Max-Age` | `2592000` (30 days); `-1` on logout |
| `HttpOnly` | yes |
| `SameSite` | `Strict` |
| `Secure` | when Go itself terminated TLS (`r.TLS != nil`), **or** when the request arrived from a network listed in the `trusted_proxies` bootstrap key and its `X-Forwarded-Proto` is `https` (`Server.overHTTPS`, `internal/api/auth_handlers.go`). The header is never believed from any other source. With `trusted_proxies` unset — the default — a TLS-terminating reverse proxy still means no `Secure`. |

Sessions slide: past the halfway mark the expiry is pushed out another 30 days,
but never past **90 days from the login that created the session**
(`auth.SessionMaxLifetime`); at that point the token is deleted and the next
request is a plain **401** (`Service.Authenticate`, `internal/auth/service.go`).
Nothing warns first — treat 401 as "log in again", which the SPA already does.

**Scopes** — a `read` token is rejected on any method other than GET/HEAD with
**403** `read-only token` (`Server.requireAuth`, same function). Enforcement is
method-based with two exceptions on the read side: `GET /tsig-keys` and
`GET /tsig-keys/{id}` blank a `read` token's `secret` and set
`secret_redacted: true` (§2.10), and `GET /sync/bundle` refuses a `read` token
outright with **403** `write scope required` (§2.11) — the bundle *is* every
TSIG secret on the box, so there is nothing left to redact. Session cookies are
always minted `write`, so a browser session is never 403'd for scope — the SPA
has no 403 handling and doesn't need any.

**Managed by a main** — an instance with `sync.peer_url` set is a replica, and
every write to configuration its main owns is refused with **409**
`managed by <peer_url>` before the handler runs (`Server.managed`,
`internal/api/sync_handlers.go`): groups, clients, filter lists, rules, TSIG
keys, zones and zone records, **both `/blocking/pause` writes** (the pause
state lives in the synced `blocking.pauses` setting — a pause is a decision
about the network, and clients reach either box), and `PUT /settings` for any
key outside the instance-local set. The peer URL in the string is verbatim
what `sync.peer_url` holds, so the screen can print it as-is. Two writes stay
available because they are operational rather than configuration —
`POST /filters/lists/{id}/refresh` and `POST /zones/{id}/refresh` — and so do
this box's own account, sessions, tokens and backups. On a main (the default,
no `sync.peer_url`) none of this is reachable. See §2.11.

**CSRF** — a cookie-authenticated non-GET/HEAD request whose `Sec-Fetch-Site`
header is `cross-site` is refused with **403** `cross-site request`
(`Server.requireAuth`), on top of `SameSite=Strict`. Every same-origin
request a browser makes from the SPA sends `same-origin`, and a request with
no `Sec-Fetch-Site` at all (curl, a script) is not treated as cross-site, so
this is invisible to the dashboard. Bearer-authenticated requests are never
checked.

**Throttle** — `POST /api/v1/setup` and `POST /api/v1/auth/login` share one
budget of **10 attempts per minute per source address**. The 11th is **429**
`too many attempts` with a `Retry-After` header (seconds), and the source stays
locked out for a minute. *Every* answer costs an attempt, including 400, 428
and a successful 200, so a UI that retries a login automatically will burn the
budget. Behind a proxy, `trusted_proxies` must be set or every browser shares
one budget.

**Request bodies** — decoded with `DisallowUnknownFields` and a 1 MiB cap
(`decode[T]`, `internal/api/server.go`). An unknown key, malformed JSON, an empty
body and a wrong-typed field are still indistinguishable to the handler, but
they now all produce **`400 invalid json`**, everywhere (`decodeOr400`, same
file). Field validation runs after the decode and says something about the
field, so `POST /tokens {"name": 5}` is `invalid json` rather than the
`name required` it used to claim. Two files are not yet converted and answer
`invalid json` already anyway: `auth_handlers.go` and `settings_handlers.go`.

**Reference failures are not `503`.** A write naming a row that does not
exist — `rules.group_id`, `clients.group_id`, either half of `group_lists` —
used to reach the storage-failure branch and answer `503 storage
unavailable`. It is now `404 not found` when the missing row is the resource
the URL named, and `400` naming the field when it came from the body.

**Status codes** — `201` for creates — **with a `Location` header naming the
new row, and the created object as the body**; a `POST` no longer answers a
bare `{"id": n}` that the client has to build a URL out of. The one `201` with
no `Location` is `POST /setup`, which creates the admin account, and this API
has no URL for a user. `204` for
updates/deletes (empty body — refetch to observe state), `202` for exactly one
endpoint (`POST /filters/refresh`). Four more appear on the zone routes and
nowhere else in this document until now: **`200` on a `POST`** (both
`/zones/{id}/refresh` and `/zones/{id}/file` answer with a body rather than
`201`/`204`, because the caller asked in order to read the result), **`413`**
(`/zones/{id}/file` over the 1 MiB cap), **`422`** (a zone file that parses
but is rejected, carrying an `errors` list beside the flat `error`), and
**`502`** (`/zones/{id}/refresh`, when the master refused or was unreachable —
a failure of a server this one depends on, not of this one).

**`405`** is the one status no endpoint's own table lists, because no handler
produces it: a request naming a real path with a method it does not have is
answered by the router with `405 method not allowed` and an **`Allow`** header
(`DELETE /api/v1/settings` → `Allow: GET, PUT`). It used to be `404 not
found`, which is wrong twice over — the path was found, and the client was
sent looking for a URL it already had.

**No 405.** `/api/` is a registered catch-all, so a method mismatch on a real
path returns **404** `not found`, not 405, and no `Allow` header. `HEAD` works
on every `GET` route.

**Empty lists are `[]`, never `null`** — on every list endpoint, including a
brand-new instance (`internal/store/sql.go`, `search.go:54`, `tokenstore.go:42`).

**Numeric query params never error.** `qInt` discards parse errors
(`internal/api/queries_handlers.go`), so `limit=banana`, `hours=-5` and a
missing param are all `0`, which each endpoint then replaces with its default.
`hours` is also capped at **8784** (24 × 366) on the three stats endpoints: it
had no upper bound, and `time.Duration(hours)*time.Hour` overflows at about
2.5 million hours, so a large enough value wrapped negative and asked for a
window in the future — a 200 reporting that nothing had happened.

---

## 2. API endpoints

### 2.1 Meta

#### `GET /api/v1/health` — public

```json
{"status":"ok","version":"dev"}
```
`version` is the build version (`-X main.Version`); a source build reports `dev`.

#### `GET /api/v1/readyz` — public

```json
{"status":"ok"}
```
200 only when the store answers a ping (within 2s) **and** at least one DNS
socket is bound — plain, DoT or DoH. Otherwise `503` with the ordinary error
envelope and one of two reasons: `storage unavailable`, or `no DNS listener is
bound`.

`/health` above is the liveness half and stays 200 either way. The split is the
point: a process that cannot serve should stop receiving traffic, not be
restarted in a loop.

#### `GET /api/v1/openapi.yaml` — public
Returns the embedded spec as `application/yaml`. Note it disagrees with the code
in places — see §10.

---

### 2.2 Setup and auth

#### `GET /api/v1/setup` — public
```json
{"setup_required":true}
```
True iff the `users` table is empty.

#### `POST /api/v1/setup` — public
Body: `username` (required), `password` (required, **min 8 chars**),
`totp_code` (accepted by the decoder and **ignored**).

**201** → `{"status":"created"}`. **No cookie is set** — the client must then
call `/auth/login`.

| Status | Error string |
|---|---|
| 400 | `invalid json` |
| 400 | `invalid input: username required` |
| 400 | `invalid input: password must be at least 8 characters` |
| 409 | `setup already completed` |
| 429 | `too many attempts` |
| 503 | `storage unavailable` |

#### `POST /api/v1/auth/login` — public
Body: `username`, `password`, `totp_code` (only needed once TOTP is enabled).

**200** → `{"status":"ok"}` plus the session cookie.

| Status | Error string | When |
|---|---|---|
| 400 | `invalid json` | |
| 409 | `setup required` | no admin exists yet |
| 428 | `totp code required` | password correct, TOTP on, code **empty** |
| 401 | `bad credentials` | unknown user, wrong password, wrong TOTP code, **or a TOTP code already used once** |
| 429 | `too many attempts` | 11th attempt in a minute from this source |
| 503 | `storage unavailable` | |

> **The 428/401 split matters for the UI.** A *missing* code is 428; a *wrong*
> code is 401, identical to a wrong password
> (`internal/auth/service.go:96-101`). A client that treats 401 as
> "bad password" will throw the user back to step one on a typo'd 6-digit code.
> Unknown user and wrong password are also deliberately identical, and both burn
> the same argon2 cost, so login timing doesn't leak whether a username exists.

> **A TOTP code works once.** The 30-second time step a code matched is
> recorded, and a later login with a code from that step or an earlier one is
> **401**, indistinguishable from a wrong code (RFC 6238 §5.2). So a user who
> mistypes a password, gets 401, and retries with the *same* still-on-screen
> code gets 401 again — prompt for a fresh code, not just the password.

#### `POST /api/v1/auth/logout`
**204**, always, plus a cookie-clearing header. Behind `requireAuth`, so an
**already-dead session gets 401** — treat that as success, the session is gone
either way. A bearer-authenticated logout revokes nothing but still 204s.

#### `POST /api/v1/auth/password`
Body: `current_password`, `new_password` (**min 8 chars**, the same rule
`/setup` enforces).

**204** on success, and **every other session on the account is revoked** — the
caller's own survives, API tokens are untouched. Any other tab is logged out on
its next request with a plain 401, exactly as the TOTP endpoints do it.

| Status | Error string | When |
|---|---|---|
| 400 | `invalid json` | |
| 400 | `current password is wrong` | **not 401** — the request authenticated fine, and a 401 would send the dashboard back to the login screen over a typo |
| 400 | `invalid input: password must be at least 8 characters` | |
| 429 | `too many attempts` | shares `POST /auth/login`'s per-source budget: same argon2id cost, same yes/no answer about a password |
| 503 | `storage unavailable` | |

#### `DELETE /api/v1/auth/sessions`
**204**, always. Revokes every session on the account except the caller's own.
API tokens are not swept — they are revoked by name (§2.9). Nothing in the body.

#### `GET /api/v1/auth/me`
```json
{"id":1,"totp_enabled":false,"username":"admin"}
```
`totp_enabled` is computed as `TOTPSecret != ""`. The hash and secret are
`json:"-"` and never serialized.

#### TOTP
| Endpoint | Body | Success | Errors |
|---|---|---|---|
| `POST /auth/totp/start` | — | **200** `{"secret","otpauth_url","qr_png"}` | 500 `totp generation failed` |
| `POST /auth/totp/confirm` | `{"secret","code"}` | **204** | 400 `invalid json`, 400 `invalid totp code` |
| `POST /auth/totp/disable` | `{"code"}` | **204** | 400 `invalid json`, 400 `invalid totp code`, 400 `bad credentials` |

`start` is **stateless** — nothing is persisted, the client holds the secret and
passes it back to `confirm`. `qr_png` is `otpauth_url` already drawn as a QR
code, base64-encoded, for `<img src="data:image/png;base64,…">`; the dashboard
renders it as-is rather than shipping a QR encoder. `confirm` and `disable` both **revoke every other
session** on the account; the session making the request survives, and API
tokens are untouched. Any other tab is logged out on its next request with a
plain 401.

---

### 2.3 Settings

#### `GET /api/v1/settings`
A flat object; **every value is a string**, including numbers. Keys prefixed
`instance.` are stripped, as is `stats.watermark` by name — the rollup's own
bookkeeping. `stats.retention_days` is an ordinary setting and is returned.

```json
{
  "blocking.mode": "null-ip",
  "blocking.ttl": "30",
  "cache.max_entries": "10000",
  "cache.max_ttl": "86400",
  "cache.min_ttl": "0",
  "cache.serve_stale_for": "86400",
  "lists.refresh_hours": "24",
  "qlog.privacy": "full",
  "qlog.retention_days": "90",
  "stats.retention_days": "365",
  "upstream.strategy": "race",
  "upstreams": "1.1.1.1:53,1.0.0.1:53,9.9.9.9:53"
}
```

#### `PUT /api/v1/settings`
Two body shapes, told apart by the `key` entry (no editable setting is called
that):

- **One key**: `{"key": "...", "value": "..."}`.
- **Several**: a flat `{"<key>": "<value>", ...}` map.

Either way `value` must be a JSON *string*, even for numeric settings — a
number or a null is `invalid json`. **204** on success.

A map is **all-or-nothing**: every key is validated first, the write lands in
one transaction, and the config version is bumped **once**. So a body with one
bad key writes none of itself, and a six-field save reconfigures the running
server once instead of six times.

The server applies a map in the order the dependencies require — protocols
being turned **off**, then `serve.tls.cert`, then `serve.tls.key`, then
everything else, then protocols being turned **on** — which is the dashboard's
own `SAVE_PHASES` (`web/src/pages/settings.tsx`) moved to the end that can
enforce it. A certificate pair and the `enabled` that depends on it can
therefore travel in one request. The dashboard still dispatches in phases; it
does not have to.

| Status | Error string |
|---|---|
| 400 | `invalid json` (including a non-string value) |
| 400 | `no settings to write` (an empty map) |
| 400 | `setting not editable: <key>` |
| 400 | `invalid value for <key>` |
| 503 | `storage unavailable` |

Full key list, defaults and reload behaviour in §3.9.

#### `POST /api/v1/backup`
Write scope. Takes a copy of the database with SQLite's `VACUUM INTO` and
answers **201** with the file it wrote, plus a `Location` header carrying the
same path:

```json
{"path":"/var/lib/dnsaur/backups/dnsaur-20260911T140822Z.db","bytes":2411724}
```

`path` is on the **server's** filesystem and no endpoint serves it, so the
settings page shows it rather than offering a download. The directory is
created `0700`; the copy is renamed into place, so an interrupted backup
leaves nothing that reads as a complete one. No schedule, no retention.

| Status | Error string |
|---|---|
| 409 | `backups are a sqlite feature; use pg_dump for postgres` |
| 500 | `couldn't write the backup: <os error>` |

The 409 is shown verbatim: the tool to run instead is the useful half of it.

#### `GET /api/v1/resolver/status`
Server state, not a setting — deliberately not folded into the flat map
above.

```json
{
  "encryption_downgraded": false,
  "reason": "",
  "serving": {
    "dot": { "enabled": true, "listening": true, "addr": "[::]:853" },
    "doh": { "enabled": true, "listening": true, "addr": "[::]:443" }
  },
  "certificate": { "not_after": "2026-11-14T00:00:00Z", "expiring_soon": false },
  "sync": { "role": "main" }
}
```

`encryption_downgraded` is true when the stored `upstreams` value named
`tls://` or `https://`, failed to parse, and the server fell back to its
hardcoded **plaintext** default resolvers; `reason` is the parse failure.
It clears server-side as soon as a settings apply installs a forwarder
built from the stored value.

`serving.dot`/`serving.doh` are intent (`enabled`, from settings) beside
reality (`listening`, whether a socket is open), plus `error` — omitted
unless they disagree. A failed bind is retried server-side every 30s.
`certificate` is **omitted entirely** unless a certificate is actually in
use: absent when neither protocol is enabled, when no paths are set, and
when none has ever loaded. That is a different fact from "not expiring
soon", and conflating them would put an expiry warning on a fresh install.

`sync` is **always present** and is the same object `GET /sync/status`
answers on its own (§2.11 and §3.13). It rides here so the warning strip can
show "behind by N", a failed pull, a peer reached over plain HTTP or a stale
replica without a second round trip. An instance with no sync configured
reads as `{"role": "main"}` and nothing else — the `omitempty` on every other
field means a main with no replicas is exactly that one key.

**Polling.** The dashboard polls this while `somethingIsWrong`
(`web/src/lib/serving.ts`) — a downgrade, either protocol enabled and not
listening, or a certificate expiring soon — every 5s, and not at all
otherwise. One predicate, shared with the shell banners, so "we warn about
this" and "we keep asking about this" cannot drift.

It also polls every 1s for 5s after **any `serve.*` write**, whatever the
status currently says. The reconcile that makes such a write real runs
asynchronously off the settings watcher, so the single refetch that follows
a save routinely lands before it: without the window, the reality line
under a freshly ticked checkbox reads `off` until the operator navigates
away and back.

**A status that will not load is its own state.** Both the reality line and
the certificate line render "status unavailable" rather than falling back
to `off` / `No certificate loaded.` — those are positive claims, and a
listener that is serving perfectly well must not be described as off. This
covers the transient case before the first response too.

One exception, on the certificate line only: a save-time rejection still
wins over "Status unavailable." That message came from this form's own
`PUT`, not from the endpoint that is not answering, so it is known to be
true even when the status is unknown.

**Multi-key saves are ordered, not fanned out.** `PUT /settings` is one key
per request and validates each against what is already stored, so the
settings form dispatches in phases: disables, then `serve.tls.cert`, then
`serve.tls.key`, then everything else, then enables. The two certificate
phases hold one key each so the second request is the one that sees a
complete pair — dispatched together, neither sees both values and a
mismatched pair stores with a 204.

| Status | Error string |
|---|---|
| 401 | `authentication required` |

#### Blocking pause
| Endpoint | Params | Success |
|---|---|---|
| `GET /blocking` | `group_id` **or** `client_id` int, optional, **default 0** | **200** `{"paused_until": <unix ms>, "scope": "global"\|"group"\|"client"}`, `paused_until` `0` and no `scope` when not paused |
| `POST /blocking/pause` | body `{["group_id"\|"client_id": int,] "minutes": int}` | **204**; **409** `managed by <peer>` on a replica (§1) |
| `DELETE /blocking/pause` | `group_id` **or** `client_id` query, optional, default 0 | **204**, unconditionally; **409** on a replica |

`minutes` must be **1–1440**, else 400 `minutes must be 1-1440`. Ids are
**not validated** — an id naming nothing is accepted, and a non-numeric one
silently becomes `0`, so a typo pauses or clears *globally*. Sending both
`group_id` and `client_id` is 400 `send group_id or client_id, not both`, on
all three endpoints. `DELETE` 204s even for a scope that was never paused.

Three scopes, one rule: **the later wins**. A group inherits the global
pause, a client inherits its group's and the global one, and none can cut
another short. `scope` on the `GET` says which of the three the reported
`paused_until` came from — a client row needs it, because `DELETE` clears
only the scope it names, so resuming a client whose *group* is paused would
change nothing visible. `GET ?client_id=` reads the client list to find its
group, which is where its **503** comes from; an id naming no client reports
only the global pause.

**Pause state survives a restart.** Every change writes the internal
settings row `blocking.pauses` (unix ms per scope, expired entries pruned),
which `App.Start` installs before the listeners bind. Entries that ran out
while the process was down are dropped rather than reinstated. Like
`stats.watermark`, the row is stripped from `GET /settings` and refused by
`PUT`.

---

### 2.4 Groups and clients

Path ids must parse as int64 **and be > 0**, else 400 `bad id`. So `0`, `-1`,
`abc` and `1.5` all fail.

| Endpoint | Success | Notes |
|---|---|---|
| `GET /groups` | 200 array | ordered by id |
| `POST /groups` | 201 the group + `Location: /api/v1/groups/{id}` | body `{"name"[, "enabled"][, "list_ids"]}`; `name` required non-empty. `enabled` omitted = true. `list_ids` omitted = every existing list; `[]` = none. Every id must exist — one that doesn't is a 400 and **no group is created** |
| `PATCH /groups/{id}` | 204 | body `{"name"?, "enabled"?}` — both optional pointers; `{}` is a legal no-op |
| `DELETE /groups/{id}` | 204 | cascades the group's `group_lists` and `rules` |
| `GET /clients` | 200 array | `?group_id=` narrows to one group; a group with no clients (or none at all) is `[]`, never 404. A `group_id` that isn't a positive id is 400 `group_id must be a positive id` |
| `POST /clients` | 201 the client + `Location: /api/v1/clients/{id}` | the `matcher` in the answer is the canonical spelling, not what was sent |
| `PUT /clients/{id}` | 204 | **full replace** — an omitted `name` becomes `""` |
| `DELETE /clients/{id}` | 204 | |

```json
[{"id":1,"name":"default","enabled":true},
 {"id":2,"name":"kids","enabled":true}]
```
```json
[{"id":1,"name":"kids-ipad","matcher":"192.168.1.42","group_id":2},
 {"id":2,"name":"iot-vlan","matcher":"192.168.30.0/24","group_id":1}]
```

| Status | Error string | When |
|---|---|---|
| 400 | `invalid json` | any body that fails to decode, on every route here |
| 400 | `name required` | groups: `name` missing or empty |
| 400 | `name cannot be empty` | PATCH with `"name": ""` |
| 400 | `list_ids names list <id>, which does not exist` | `POST /groups` with an unknown list id; nothing is created |
| 400 | `matcher must be an IP or CIDR and group_id set` | clients: bad matcher or missing group_id (decode failure is `invalid json`) |
| 400 | `group_id does not name an existing group` | `POST`/`PUT /clients` naming a group that isn't there |
| 409 | `resource in use` | deleting group id 1, or a group with clients attached |
| 409 | `a group with that name already exists` | duplicate name on create **or** rename |
| 409 | `another client already matches <matcher>` | duplicate client matcher on create or update |
| 404 | `not found` | |
| 503 | `storage unavailable` | genuine storage failures only |

---

### 2.5 Filters

| Endpoint | Success | Notes |
|---|---|---|
| `GET /filters/lists` | 200 array | rows carry **`next_refresh_at`** on top of the stored fields (§3.2) |
| `POST /filters/lists` | **201** the list + `Location: /api/v1/filters/lists/{id}` (same row shape `GET /filters/lists` returns, `next_refresh_at` included) | body `{"url","kind"}` + **optional `name`**; kicks off a background refresh of *every* list |
| `PATCH /filters/lists/{id}` | 204 | body `{"enabled": bool}` and/or `{"name": string}` — **at least one required**; `url`/`kind` are rejected |
| `DELETE /filters/lists/{id}` | 204 | |
| `POST /filters/lists/{id}/refresh` | **202** the updated list row | downloads **this list only**, inside the request; no body read. **404** unknown id, **409** a disabled list |
| `GET /groups/{id}/lists` | 200 array | **404** for a group that does not exist (it used to answer `[]` + 200) |
| `PUT /groups/{id}/lists` | 204 | body `{"list_ids":[...]}`; `null`/omitted unassigns everything. One transaction; duplicate ids are a set; unknown id → 400, unknown group → 404 |
| `GET /groups/{id}/rules` | 200 array | **404** for a group that does not exist |
| `POST /groups/{id}/rules` | 201 the rule + `Location: /api/v1/filters/rules/{id}` — the URL that **deletes** it, since a rule has no read of its own | group comes from the **path**, not the body; a group that does not exist → **404** |
| `DELETE /filters/rules/{id}` | 204 | note the asymmetry: created under `/groups/{id}/rules`, deleted under `/filters/rules/{id}` |
| `POST /filters/refresh` | **202** `{"status":"refreshing"}` | no body read |

```json
[{"id":1,
  "url":"https://raw.githubusercontent.com/StevenBlack/hosts/master/hosts",
  "name":"StevenBlack hosts",
  "kind":"block","enabled":true,
  "last_refreshed":1785946876638,"entry_count":99277,
  "last_status":"ok","last_error":"","last_attempt":1785946876638,
  "next_refresh_at":1785990076638},
 {"id":2,
  "url":"https://raw.githubusercontent.com/hagezi/dns-blocklists/main/hosts/pro.txt",
  "name":"hagezi hosts/pro.txt",
  "kind":"block","enabled":true,
  "last_refreshed":0,"entry_count":0,
  "last_status":"failed","last_error":"404 Not Found","last_attempt":1785946876700,
  "next_refresh_at":1785990076638}]
```
```json
[{"id":1,"group_id":1,"action":"block","pattern":"ads.example.com","is_regex":false}]
```

| Status | Error string |
|---|---|
| 409 | `that list URL is already subscribed` |
| 409 | `this list is disabled` (`POST /filters/lists/{id}/refresh`) |
| 400 | `invalid json` — every body that fails to decode, PATCHing `url`/`kind` included (`DisallowUnknownFields` rejects them) |
| 400 | `url must be http(s)` |
| 400 | `kind must be block or allow` |
| 400 | `enabled or name required` (PATCH with neither field) |
| 400 | `name too long (max 120)` (create and PATCH) |
| 400 | `action allow\|block and pattern required` |
| 400 | `list_ids names list <id>, which does not exist` (`PUT /groups/{id}/lists`) |
| 404 | `not found` — a group that does not exist, on any `/groups/{id}/…` route |
| 400 | `regex pattern too long (max 512)` |
| 400 | `invalid regex: <Go's compile error, verbatim>` — e.g. `invalid regex: error parsing regexp: missing closing ]: ` + `` `[unclosed` `` |

**`POST /filters/refresh` is fire-and-forget.** The work runs detached; there is
**no progress endpoint, no job id, and errors never reach the client** — they
only hit the server log. Poll `GET /filters/lists` and read `last_status` /
`last_error`, which is where a refresh's outcome now lands (§3.2). `POST
/filters/lists` does the same background refresh but answers **201**, so a
freshly added list reads `entry_count: 0` with `last_status: "pending"` until
the fetch lands.

**`POST /filters/lists/{id}/refresh` is not.** It shares the 202 so one verb
reads as one status, but the download runs *inside* the request and the body
is the list row as the refresh left it — there is nothing to poll for, and no
other subscription is touched (neither downloaded nor re-stated), so one
unreachable URL elsewhere costs it nothing. A **disabled** list is a `409`:
nothing would compile the copy, so the only effect would be to overwrite the
row's state with the outcome of a download that changes nothing.

**`PUT /groups/{id}/lists` is one transaction** (`FilterStore.ReplaceGroupLists`,
`internal/store/sql.go`). It used to unassign every current list one-by-one and
then assign the requested ids, so a nonexistent list id returned 503 *after* the
unassign loop had committed and left the group with a partial set. Now an
unknown id is a 400 naming it and the group keeps exactly the assignments it
had; a duplicate id in the array is a set, not a failure.

---

### 2.6 Zones and zone records

`/records` is gone. Local overrides were replaced by real authoritative
zones — a name inside an enabled zone is answered or refused and **never
forwarded**, which is the entire point of the change (see
`docs/superpowers/specs/2026-08-08-zones-design.md` §1).

| Endpoint | Success |
|---|---|
| `GET /zones` | 200 array — `?type=` (`primary`/`secondary`/`forwarder`/`stub`/`internal`) and `?enabled=` (exactly `true`/`false`) narrow it; an empty parameter is no filter, an unknown value is 400 |
| `POST /zones` | 201 the zone, generated SOA fields included + `Location: /api/v1/zones/{id}` |
| `GET /zones/{id}` | 200 object |
| `PATCH /zones/{id}` | 204 — conditional on the zone not having changed since it was read (a lost race is retried once, then 409); renaming, disabling or re-enabling a `primary` moves the PTRs its records own |
| `DELETE /zones/{id}` | 204 — cascades every record in the zone, and retires the PTRs those records owned from whatever reverse zone holds them (the cascade cannot reach those: they are rows in a different zone) |
| `POST /zones/{id}/clone` | 201 the new zone + `Location: /api/v1/zones/{new id}` — body `{name}`; copies the configuration and every record, resets `soa_serial` to 1 and carries no transfer history. 409 on a duplicate name or an `internal` source |
| `GET /zones/{id}/records` | 200 array |
| `POST /zones/{id}/records` | 201 the record, `rdata` in the parser's own spelling + `Location: /api/v1/zones/{id}/records/{rid}` |
| `PATCH /zones/{id}/records` | 204 — body `{ids, ttl}`; sets one TTL across the records named, in one transaction with one serial bump. 400 on empty `ids` or a TTL past 2147483647, 404 on an id that is not this zone's, 409 on the RRSet rule or a zone whose records are written elsewhere. auto-PTR does not run |
| `PUT /zones/{id}/records/{rid}` | 204 (full replace) |
| `DELETE /zones/{id}/records/{rid}` | 204 |

> **Known gap — this table and this section are incomplete.** Four zone
> routes that exist and are used by the dashboard are missing from it:
> `GET /zones/{id}/file` (200, a BIND master file as an attachment),
> `POST /zones/{id}/file` (200, the import — `dry_run` required, 413/422 of
> its own), `POST /zones/{id}/refresh` (200, or 502 carrying the master's
> own error), and `GET /zones/{id}/notifies` (200 array). Their full
> contracts — the import's diff shape and `errors` list, the refresh
> result's fields, the notify row's eight fields and four states — are
> **not documented here at all**. [`docs/api.md`](api.md) covers all four in
> prose and `openapi.yaml` is authoritative for the shapes; this section
> will not be trustworthy for a client generator until they are written up
> here, and that is its own piece of work rather than a correction.

Real capture, `POST /zones {"name":"home.lan"}` then `GET /zones`:

```json
[{"id":1,"name":"home.lan","type":"primary","enabled":true,
  "soa_ns":"ns.home.lan","soa_mbox":"hostadmin.home.lan","soa_serial":1,
  "soa_refresh":900,"soa_retry":300,"soa_expire":604800,"soa_minimum":900,
  "soa_ttl":900,"primaries":"","tsig_key_id":0,"expires_at":0,
  "refreshed_at":0,"created_at":1786219213980,"modified_at":1786219213980}]
```

**That capture predates D3, D4 and D6.** The row a current build returns
also carries `last_error`, `last_attempt`, `allow_transfer`, `last_xfr_at`,
`last_xfr_peer`, `last_xfr_error`, `notify_to` and `forward_to` — every one
of them empty or `0` on a fresh `primary`. §3.6 is the field list to read;
this block is kept for the shape, not the column set.

`type` omitted on create defaults to `primary`. **`primary`, `secondary`,
`forwarder` and `stub` are the four types the API will create or patch** —
D2 added the second, D6 the last two. `POST /zones` with `"type":"internal"`
400s `only primary, secondary, forwarder and stub zones are supported`; the
check (`internal/api/zones_handlers.go`) is an equality test against those
four values, not an allowlist that happens to include them, so `internal`
isn't a quiet exception.

Which of the other fields each type may set, all of them 400 elsewhere
(`checkZoneTransferConfig`, one function for create and patch alike):

| Field | Types that may set it |
|---|---|
| `primaries` (**required non-empty**), `tsig_key_id` | `secondary`, `stub` |
| `forward_to` | `forwarder` |
| `allow_transfer`, `notify_to` | `primary`, `secondary` |

`forward_to` is the only one of the four whose empty value is meaningful
rather than merely absent: a forwarder that names no upstreams claims its
suffix and SERVFAILs (§3.8). See [`docs/api.md`](api.md)'s Forwarder zones
entry for the format, which is `notify_to`'s grammar minus the `key:`
suffix.

Creating a **`primary`** also inserts its apex NS record (`name: "@"`,
pointed at `soa_ns`) — RFC 2181 §10.1 requires apex NS on every
authoritative zone. **Only a `primary`** (`zones_handlers.go`'s
`zoneType == zoneTypePrimary` gate): the other three creatable types hold
contents dnsaur does not author — a secondary's and a stub's arrive with the
first pull and would be replaced by it, and a forwarder answers from no
records at all — so seeding one would be this server authoring data in a
zone it does not own. Do not expect a record back from
`GET /zones/{id}/records` on a freshly created `secondary`, `stub` or
`forwarder`; the correct answer there is `[]`. Where it does happen the
insert is best-effort against an already-committed zone: a failure is
logged, not surfaced, so `GET /zones/{id}/records` is how to confirm it
landed. `soa_ttl` is fixed at `900` — there is no request field to set it,
on create or patch.

Zone create/patch errors:

| Status | Error string |
|---|---|
| 400 | `invalid json` |
| 400 | `bad id` |
| 400 | `name must be a valid domain name` |
| 400 | `soa_ns must be a valid domain name` |
| 400 | `soa_mbox must be a valid domain name (a dot in the local part is written \.)` |
| 400 | `only primary, secondary, forwarder and stub zones are supported` |
| 404 | `not found` |
| 409 | `a zone with that name already exists` |
| 409 | `zone changed since it was read` — two writers, twice over |
| 503 | `storage unavailable` |

**What counts as a valid domain name here.** `name`, `soa_ns` and `soa_mbox`
all take labels of letters, digits, hyphen and underscore, nothing else
(`validDomainLabels`, `internal/api/zones_handlers.go`). `dns.IsDomainName`
alone accepts `;`, `"`, `!` and NUL — verified — and none of those survive a
zone-file export or a `Content-Disposition` header, so a zone called
`a;b.lan` could be created and never exported back in. `soa_mbox` is a
mailbox written as a domain name (RFC 1035 §8), so a dot in the local part is
escaped: `first\.last.e412.in` for `first.last@e412.in`. Both SOA names are
stored with any trailing dot removed — `zones.Render` writes `SOA %s. %s.`,
so a stored trailing dot would export as `..` — and both are checked on
`POST` and `PATCH` alike. On `PATCH` they are pointers, so `""` is a value
the caller chose and is rejected; on `POST` they are plain strings, where
`""` still means "use the generated default".

The seeded apex NS goes through `buildZoneRecord`, the same validator a hand
write uses. Before that it went straight to the store, so an unvalidated
`soa_ns` produced an NS row whose rdata `dns.NewRR` rejects: the answer path
dropped it silently and every outbound AXFR errored on it.

Real capture, `GET /zones/1/records` after adding an A record at `bifrost`
(name typed relative) and another at `nas.home.lan` (typed fully-qualified —
the apex is stripped):

```json
[{"id":1,"zone_id":1,"name":"@","type":"NS","ttl":3600,
  "rdata":"ns.home.lan.","enabled":true,"comment":""},
 {"id":2,"zone_id":1,"name":"bifrost","type":"A","ttl":300,
  "rdata":"192.168.150.28","enabled":true,"comment":""},
 {"id":3,"zone_id":1,"name":"nas","type":"A","ttl":300,
  "rdata":"192.168.1.10","enabled":true,"comment":""}]
```

`rdata` is DNS presentation format — the rdata portion only
(`192.168.150.28` for A, `10 mail.example.com.` for MX,
`0 issue "letsencrypt.org"` for CAA). It's validated by handing
`"<name> <ttl> IN <type> <rdata>"` to `dns.NewRR` — the same parser that
builds the RR the resolver actually serves — so an accepted record is by
construction servable, and a `400` carries that parser's own error text.

What comes back from a subsequent `GET` is that parser's spelling of the
value rather than the text that was sent: `nas.example.com` reads back
`nas.example.com.`, `hello` reads back `"hello"`, `2001:0db8::0001` reads
back `2001:db8::1`. A form that echoes the value it just submitted will
show it changed — refetch after a write rather than assuming the field
round-trips.

Real capture, an unparseable A record:

```json
{"error":"dns: bad A A: \"not-an-ip\" at line: 1:32"}
```

`name` is zone-relative: `@` (or blank, or the zone's own name) means the
apex; `bifrost`, `*`, `*.nexus` name anything else. A name carrying the
zone's own apex as a suffix has that suffix stripped rather than doubled
(by whole labels — `foo\.example.com` is one escaped label under `com` and
keeps its full name).

A `@` in `rdata` is the zone apex, resolved the way a zone file resolves it:
`{"type":"MX","rdata":"10 @"}` reads back `10 <zone>.`, and a `@` in a value
that is text rather than a name (`TXT`) reads back as the character it is.

Three write conflicts, checked in this order and each real-captured:

| Order | Status | Error string | RFC |
|---|---|---|---|
| 1 | 409 | `CNAME is not allowed at the zone apex` | 1912 §2.4 |
| 2 | 409 | `CNAME cannot coexist with another record at the same name` | 1034 §3.6.2 |
| 3 | 409 | `records in the same RRSet must share one TTL` | 2181 §5.2 |

**The RFC citation is this table's, not the server's.** `RecordProblem.Error()`
returns `Msg` bare (`internal/zones/record.go`), so an exact-match client
looking for a trailing `(RFC 1912 §2.4)` finds nothing. The RFC column is
where the reference belongs; it used to be in the quoted string too, which
broke §1's promise that these are verbatim.

Rule 2 fires in either write order — a CNAME landing beside an existing
record, or a record landing beside an existing CNAME. Rule 1 only applies
at `@`: the zone's SOA/NS live on the `zones` row rather than as a
`zone_records` row, so the ordinary sibling check (rule 2) would see an
apex with nothing recorded there and miss an apex CNAME on its own — it
needs the dedicated rule.

Other errors:

| Status | Error string | When |
|---|---|---|
| 400 | `bad id` | zone or record id fails to parse as a positive int64 |
| 400 | `invalid json` | |
| 400 | `ttl must not exceed 2147483647` | RFC 2181 §8; the citation is not part of the string |
| 400 | `name "<name>" is not a valid record name` | the name as sent, quoted. A name has to survive being written into the zone's export and read back, so whitespace, `;`, `"`, `(`, `)`, `\`, `/`, an empty label and a leading `$` are all refused. `@`, `*`, `*.nexus` and a leading `_` are not |
| 400 | `the SOA lives on the zone, not in its records` | `type` is `SOA`; the zone's own SOA is edited through `PATCH /zones/{id}` |
| 400 | `unsupported record type "<type>"` | the type as sent, quoted — a type miekg/dns has no name for, including RFC 3597's `TYPE65280` spelling |
| 404 | `not found` | unknown zone id, on `/zones/{id}` **or any `/zones/{id}/records*` route**, or `rid` doesn't belong to the zone named by `id` — real-captured; `openapi.yaml`'s per-route "zone not found" / "rid is not a record of this zone" wording is a description of the situation, not the actual response body, which is always the flat `{"error":"not found"}` |
| 409 | `built-in zones cannot be changed` | any write to an `internal` zone or its records, `POST /zones/{id}/file` included |
| 409 | `a secondary zone's records come from its primary; change them there` | any record write, or a file import, into a `secondary` |
| 409 | `a stub zone's records are the NS set it fetches from its master; change them there` | the same, into a `stub` — "fetches", not "transfers", because it does not transfer |
| 503 | `storage unavailable` | |

The three 409s above are one rule with one implementation
(`recordWriteRefusal`, `internal/api/zonerecords_handlers.go`): a zone whose
contents are authored elsewhere refuses the write that would be destroyed.
**A `forwarder` is deliberately not among them** — nothing overwrites its
records, so a write into one is inert rather than lost, which is a different
complaint and not a 409's to make.

There is **no uniqueness constraint** on records beyond the three conflicts
above — two `TXT` records at the same name, for instance, are both stored
and both answered.

---

### 2.7 Query log

#### `GET /api/v1/queries`

| Param | Type | Default | Notes |
|---|---|---|---|
| `from` | int64 unix **ms** | 0 = unbounded | applied only when `> 0` |
| `to` | int64 unix **ms** | 0 = unbounded | applied only when `> 0` |
| `client` | string | none | **exact** match on `client_ip` |
| `q` | string | none | substring `LIKE`; `%` and `_` are **stripped, not escaped**, so `a_b` silently searches `ab` |
| `decision` | string | none | exact match; an unknown value returns `[]` rather than erroring |
| `type` | string | none | exact, uppercase as stored |
| `limit` | int | **100** | `<= 0` → 100, `> 1000` → clamped to **1000** |
| `offset` | int | **0** | negatives clamped to 0; no maximum |

There is **no `window` param** here — that's `hours`, and only on `/stats/*`.

Ordering is `ORDER BY id DESC` — insertion order, *not* `at`. Since rows are
written in flush batches, `id DESC` is only approximately newest-first.

**No total count, no cursor, no `has_more`.** The client infers "more available"
from `len(result) == limit`.

Real capture (`?limit=2`) — both rows decided by the same zone, one NODATA
(name exists, wrong type) and one NXDOMAIN (name absent); `decision` is
`authoritative` either way, only `r_code` differs:
```json
[{"id":3,"at":1786219236223,
  "instance_id":"1fc5e9d1-9dc8-406f-a86e-555a39f48de2",
  "client_ip":"127.0.0.1","client_id":0,
  "q_name":"doesnotexist.home.lan","q_type":"A","decision":"authoritative",
  "rule_id":0,"list_id":0,"upstream":"","r_code":"NXDOMAIN","duration_ms":0,"matched":""},
 {"id":2,"at":1786219236203,
  "instance_id":"1fc5e9d1-9dc8-406f-a86e-555a39f48de2",
  "client_ip":"127.0.0.1","client_id":0,
  "q_name":"bifrost.home.lan","q_type":"AAAA","decision":"authoritative",
  "rule_id":0,"list_id":0,"upstream":"","r_code":"NOERROR","duration_ms":0,"matched":""}]
```

#### `GET /api/v1/queries/tail` — SSE

`200`, `Content-Type: text/event-stream`, `Cache-Control: no-cache`, headers
flushed immediately. **No params — no filtering is possible on the stream.**

Wire format is one line per event:

```
data: {"id":0,"at":1785946986964,"instance_id":"29cbba52-…","client_ip":"127.0.0.1",
       "client_id":0,"q_name":"sse-probe.example.com","q_type":"A",
       "decision":"forwarded","rule_id":0,"list_id":0,"upstream":"1.1.1.1:53",
       "r_code":"NOERROR","duration_ms":9,"matched":""}
```

Four things a client must know:

- **No `event:` field** — everything is the default `message` type, so use
  `onmessage`, not `addEventListener("...")`.
- **No `id:` field** — no `Last-Event-ID` resumption. A reconnect starts fresh
  and anything in the gap is lost.
- **`id` in the payload is always `0`.** The entry is published from the DNS
  middleware *before* the database assigns a row id
  (`internal/qlog/qlog.go:173`). Streamed rows therefore cannot be correlated
  with stored rows by id, and cannot be deduplicated against `GET /queries`.
- **A heartbeat every 20 seconds.** An idle stream writes `: ping\n\n`, an SSE
  comment frame, so it never looks abandoned to a proxy with an idle-read
  timeout in front of it. An `EventSource` ignores comment lines, so a client
  needs no code for this — but a client parsing the stream by hand must skip
  any line starting with `:`.

Backpressure: the per-subscriber channel is buffered at **64** and publishing is
non-blocking — a slow client **silently misses entries**, with no signal in the
stream (`internal/qlog/qlog.go:107,119-128`).

Errors: 503 `query log disabled` (unreachable in the shipped binary — `Logger`
is always wired), 500 `streaming unsupported` (unreachable — the wrapper
implements `Flush`), 401 `authentication required`.

Because `EventSource` cannot set an `Authorization` header, browser clients must
use the cookie. That works for the bundled SPA (same origin, `SameSite=Strict`).

---

### 2.8 Stats

All three read the pre-aggregated `stats_hourly` table, never the raw log.
Shared param:

| Param | Type | Default | Constraint |
|---|---|---|---|
| `hours` | int | **24** | `<= 0` or unparseable → 24. **No upper bound, no validation** — `hours=banana` and `hours=-5` both silently mean 24. |

#### `GET /stats/overview`
```json
{"blocked":2,"cached":0,"clients":1,"dropped":0,"forwarded":10,"total":14}
```
- `total` = the sum of **every** decision bucket, including `authoritative`
  and `error`.
- `cached` = `cached` + `stale`.
- `clients` = distinct `client_ip` keys in the window — **not** a count of
  configured client rows.
- `dropped` = query log entries discarded since **process start** because the
  write buffer was full (§5). The only field here that ignores `hours`, and
  the only one that is not a count of queries: it says the others are
  undercounts.

**`blocked + cached + forwarded ≤ total`**, and the gap is `authoritative` +
`error`. Any UI computing "allowed = total − blocked" will be wrong.

#### `GET /stats/timeline`
Ascending by bucket; `bucket` is a **unix-seconds hour start**. Only non-zero
decisions appear — missing keys default to 0. **No zero-filling**, so a quiet
hour is simply absent.
```json
[{"bucket":1785945600,"decisions":{"blocked":2,"forwarded":10,"authoritative":2}}]
```

#### `GET /stats/top`

| Param | Required | Default | Values |
|---|---|---|---|
| `metric` | **yes** | — | `domain` \| `blocked_domain` \| `client` |
| `n` | no | **10** | `<= 0` → 10, `> 100` → clamped to 100 |
| `hours` | no | 24 | |

The limit param is **`n`, not `limit`** — sending `limit=2` is silently ignored
and you get 10 rows.

```json
[{"key":"github.com","count":1},{"key":"google.com","count":1}]
```
400 `metric must be domain, blocked_domain or client` — including when `metric`
is missing entirely.

Metric universes differ, so counts across metrics are not comparable:
`domain` counts only `forwarded`/`cached`/`stale`; `blocked_domain` only
`blocked`; `client` counts **every** decision.

---

### 2.9 API tokens

| Endpoint | Success | Notes |
|---|---|---|
| `GET /tokens` | 200 array | only the caller's `kind:"api"` rows; sessions are never listed |
| `POST /tokens` | 201 `{"id","token"}` + `Location: /api/v1/tokens/{id}` | body `{"name"[, "scope"][, "expires_at"]}`; plaintext returned **once** — the only field here that no `GET` ever returns, which is why this one answer is not the stored row |
| `DELETE /tokens/{id}` | 204 | |

```json
[{"id":2,"user_id":1,"kind":"api","name":"grafana-scraper","scope":"read",
  "created_at":1785946863832,"expires_at":0,"last_used":0}]
```
`expires_at: 0` means **never expires**, and is the default: `POST /tokens`
takes an optional `expires_at` (unix ms, and it must be **in the future** — a
past one is a 400 rather than a token that is dead on arrival). Once it passes,
the token answers 401 and is deleted on the attempt.

**An API token's expiry never slides.** A session's is pushed out by use
(§1); a token's is a date its owner chose, and renewing it every time a
script runs would make the field a decoration.

`token_hash` is `json:"-"` and never leaves the server.

Errors: 400 `invalid json` (any body that fails to decode), 400 `name required`,
400 `scope must be read or write`, 400 `expires_at must be a unix-ms time in the
future`, 404 `not found` on delete — which covers a
token that does not exist and another user's token, **and nothing else**: a
storage failure is a 503. Every error used to map to 404, so a database that
was merely unreachable told a script the token was already gone.

The `id` in the create response **is** the insert id
(`auth.Service.CreateAPIToken` returns it). It used to be a guess — the handler
re-listed the user's tokens and picked the highest id with a matching name,
which two tokens of one name made ambiguous and a failed listing made `0`,
silently, since that listing's error was discarded too.

---

### 2.10 TSIG keys

| Endpoint | Success | Notes |
|---|---|---|
| `GET /tsig-keys` | 200 array | `[]` when empty, never `null` |
| `POST /tsig-keys` | 201 the key + `Location: /api/v1/tsig-keys/{id}` | |
| `GET /tsig-keys/{id}` | 200 object | |
| `PUT /tsig-keys/{id}` | 204 | full replace — all three fields required, same as create. **409** when it renames a key a zone names by name — see below |
| `DELETE /tsig-keys/{id}` | 204 | **404** for an id that never existed, **409** when a zone names the key — see below |

```json
[{"id":1,"name":"xfer.e412.in.","algorithm":"hmac-sha256.",
  "secret":"Sh5ZuulpjcmcJuN6VwMQCVEhTJyUmlPTSHexvePtaWo=",
  "secret_redacted":false,
  "created_at":1786000000000}]
```

Three things about this shape are easy to get wrong:

- **`algorithm` carries a trailing dot.** The accepted set is
  `hmac-sha1.`, `hmac-sha224.`, `hmac-sha256.`, `hmac-sha384.`,
  `hmac-sha512.` — `miekg/dns`'s own constants, dot included. The dashboard
  shows them **without** it, so the TSIG keys screen keeps an explicit
  display↔wire mapping (`web/src/lib/tsig.ts`): select options are *valued*
  with the wire string and only *labelled* without the dot. Sending the
  displayed form is a `400`.
- **`name` is canonicalised on every write** (lowercase, fully qualified), so
  what comes back is not what was sent: `XFER.e412.IN` reads back as
  `xfer.e412.in.`.
- **`secret` is returned on every read** — the deliberate opposite of an API
  token. It is base64, stored in plaintext, and has to be pasted unchanged
  into the peer's config, so the screen's masking is a display choice about
  what sits on screen rather than a boundary of any kind. **One caller does
  not get it:** a `read`-scoped API token reads `"secret":""` with
  `"secret_redacted":true` alongside. Every response carries
  `secret_redacted` — `false` for a session or a `write` token — so an
  empty secret you may not see is distinguishable from one that was never
  set. The dashboard runs on a session and always sees the real value.

Errors: 400 `name must be a valid domain name` (the same label rules a zone
name takes — see §2.6), 400 `algorithm must be one of hmac-sha1.,
hmac-sha224., hmac-sha256., hmac-sha384., hmac-sha512.`, 400
`secret must be base64-encoded`, 400 `invalid json` / `bad id`, 404
`not found` (**get, update *and* delete** — both `Update` and `Delete` re-read
on 0 rows precisely so they can tell "never existed" from "spoken for"), 409
`a TSIG key with that name already exists` (create and update), 409
`resource in use` (**update *and* delete**, when a zone names the key — see
below). This document said delete "succeeds even for an id that never
existed" and scoped the 404 to `GET` until D6's docs pass; both were false
and both contradicted the shipped `openapi.yaml`, which documents 404 on all
three.

> **Zones reference keys, and deleting one that is referenced is refused.**
> This paragraph said the opposite until D6's docs pass; it had been false
> since D2/D3, when the things it was waiting for arrived. `tsig_key_id` is
> accepted on `POST`/`PATCH /zones` for a `secondary` or a `stub` (§2.6) and
> validated to name an existing key; `allow_transfer` and `notify_to` both
> take `key:<name>` entries, validated the same way; and transfers, NOTIFYs
> and a stub's SOA/NS fetch all run signed under them.
> `DELETE /tsig-keys/{id}` answers **409 `resource in use`** when any zone
> names the key — by `tsig_key_id`, or by `key:` in either list — enforced
> in the `DELETE` statement itself (`tsigKeyStore.Delete`) because
> `zones.tsig_key_id` carries no foreign key.
>
> **`PUT /tsig-keys/{id}` applies the name half of the same guard.** A write
> that *changes the name* while `allow_transfer` or `notify_to` names the key
> answers the same **409 `resource in use`**: those two columns carry the
> name as text, so a rename leaves the zone naming a key that does not exist
> and every signed transfer or NOTIFY refused with nothing to say why —
> exactly what the delete guard exists to prevent, reached by editing. It
> answered 204 until now. A write that *keeps* the name never consults the
> checks at all, so rotating a secret or correcting an algorithm on a
> referenced key still works; and a key referenced only by `tsig_key_id`
> renames freely, since that reference is by id. Enforced in the `UPDATE`
> statement itself (`tsigKeyStore.Update`), the same way the delete is. The
> dashboard side of this is task #41. The screen has a **USED BY**
> column to match, counting the zones that name the key; it is computed
> client-side from `GET /zones` rather than served as a field, so if that
> request fails every row reads `—` and the 409 is what reports the truth.

---

### 2.11 Sync

Two dnsaur instances serving one network keep the same configuration: the
**main** is the one that accepts writes, a **replica** is any instance with
`sync.peer_url` set, and there is no third mode. A replica pulls, applies,
and refuses local writes to anything the bundle carries (§1, *Managed by a
main*); clearing `sync.peer_url` is the promotion.

| Endpoint | Scope | Success | Notes |
|---|---|---|---|
| `GET /sync/version` | any | 200 `{config_version, instance_id}` | the cheap probe; the bundle is only fetched when the version moved |
| `GET /sync/bundle` | **write** | 200 the bundle | **403** `write scope required` for a `read` token |
| `GET /sync/status` | any | 200 the status object | answers on both roles |
| `POST /sync/replicas` | write | 204 | `{instance_id, dns_addr, version_applied}`; idempotent |
| `DELETE /sync/replicas/{instance_id}` | write | 204 | idempotent — an id that is not registered is already in the state asked for |

`GET /sync/bundle` is the one `GET` in this API that scope refuses rather
than redacts, and the reason is the body: settings (everything except the
instance-local `instance.*`, `serve.*`, `sync.*` and `stats.watermark`),
groups, clients, lists with their group assignments, rules, **TSIG keys with
their secrets**, and zone *definitions*. Zone records are deliberately
absent — a replica gets those by AXFR. Ids in it are the main's and the
replica keeps them, so a `group_id`, or a query log row's `rule_id`, names
the same row on both boxes.

`POST /sync/replicas` is not bookkeeping. Registering an address is what
turns on the implicit transfer allow — an AXFR for a `primary` zone is
accepted when it verified under the main's `sync.tsig_key_id` **and** came
from a registered replica's `dns_addr` — and adds that address as a NOTIFY
target for every primary zone. Neither edits an ACL, so `allow_transfer`
still says exactly what the operator wrote. A `dns_addr` with no host
(`:53` — what a replica listening on every interface has to send) is
completed with the address the request arrived from, which is where this
main can reach that box. Errors: 400 `invalid json`, 400 `instance_id
required`, 400 `dns_addr must be a host:port address: ...` / `dns_addr must
name a host, not just a port` / `dns_addr port must be numeric, 1-65535`,
503 `sync unavailable` (no sync subsystem is running on this instance).

`last_seen` is stamped by the main, never sent by the replica — a clock skew
on the replica must not decide whether it looks stale. A replica not seen for
three intervals is shown as stale and **never removed automatically**; the
operator removes one, because a box that is down for an afternoon is not a
box whose transfer allow should quietly disappear.

---

## 3. Entities

### 3.1 Query log row

| Field | Type | Units / format | Notes |
|---|---|---|---|
| `id` | int64 | | **always `0` on the SSE stream** |
| `at` | int64 | **unix ms** | query *start*, set before resolution |
| `instance_id` | string | UUIDv4 | from the `instance.id` setting |
| `client_ip` | string | | never carries an IPv6 zone; rewritten when `qlog.privacy=anon` |
| `client_id` | int64 | FK → `clients.id` | **`0` = no client row matched** (fell back to the default group) |
| `q_name` | string | lowercased, no trailing dot | `""` if the query had no question |
| `q_type` | string | `A`, `AAAA`, `CNAME`, `TXT`, `PTR`, `SRV`, `HTTPS`, … | `""` for a type miekg/dns doesn't name |
| `decision` | string | enum below | |
| `rule_id` | int64 | FK → `rules.id` | **`0` = not attributed**; non-zero only for a rule-driven block |
| `list_id` | int64 | FK → `lists.id` | **`0` = not attributed**; non-zero only for a list-driven block |
| `upstream` | string | `host:port` | **`""` = never left the box** (blocked/authoritative/cached/stale/error) |
| `r_code` | string | `NOERROR`, `NXDOMAIN`, `SERVFAIL`, `REFUSED`, … | |
| `duration_ms` | int64 | whole ms, truncated | sub-millisecond answers record `0` |
| `matched` | string | rule pattern or list entry | **`""` = nothing matched**, and `""` on every row logged before migration 0015 added the column (not backfilled) |

**`decision` enum** (`internal/dnssrv/pipeline.go:12-20`):

| Value | Meaning |
|---|---|
| `blocked` | a rule or list matched |
| `authoritative` | answered (or authoritatively refused) by a zone this instance holds — was `local` before zones replaced the flat local-records table; see §2.6 |
| `cached` | fresh cache hit |
| `stale` | upstream failed, served an expired entry (TTL rewritten to 30) |
| `forwarded` | answered by an upstream |
| `error` | handler error, or RFC 9520 failure cache → SERVFAIL |

There is no `allowed` value. It used to be declared but never written (dead
since it was added); the zones work deleted the constant outright rather
than leaving it as an unused TODO — see §10 item 2.

> Verified live: an exact-match query, a NODATA (name exists, wrong type)
> and an NXDOMAIN (name absent) against the same zone all logged
> `decision: "authoritative"`, distinguished only by `r_code` — real capture
> in §2.6.

### 3.2 Filter list

| Field | Type | Notes |
|---|---|---|
| `id` | int64 | |
| `url` | string | must be `http`/`https` with a non-empty host; **DB-unique**; **immutable** |
| `name` | string | readable label; **never empty on the way out** — derived from the URL when not supplied. Optional on `POST`, mutable via `PATCH`; max 120 chars |
| `kind` | string | **`block` \| `allow`** — not `hosts`/`abp`; the *format* is auto-detected at parse time. **Immutable** |
| `enabled` | bool | create always forces `true` |
| `last_refreshed` | int64 | **unix ms of the last successful fetch-and-parse**; **`0` = never**. Does **not** move on a failed attempt, so a `stale` list keeps dating the copy it is still serving |
| `entry_count` | int64 | **unique domains currently compiled and enforcing**, after dedup and parse rejects — not lines |
| `last_status` | string | `pending` \| `ok` \| `stale` \| `failed` \| `empty` — see below |
| `last_error` | string | short single-line reason, ≤160 bytes; `""` when `last_status` is `pending` or `ok` |
| `last_attempt` | int64 | **unix ms of the last attempt, successful or not**; `0` = never tried |
| `next_refresh_at` | int64 | **unix ms of the periodic download's next tick**; `0` = no cadence running. **Not stored** — read from the running ticker, so the same on every row (the interval is server-wide, there are no per-list schedules). Present on `GET /filters/lists` and the per-list refresh response; **absent from `GET /groups/{id}/lists`** |

#### Refresh outcome (`last_status`)

Four things used to be indistinguishable on the wire, all of them
`entry_count: 0, last_refreshed: 0`. They are now separate values, and the UI
renders each differently:

| Value | Meaning | Enforcing? |
|---|---|---|
| `pending` | never attempted. **The only thing `0` / "never" is allowed to mean.** | no, not yet |
| `ok` | fetched (or 304'd) and parsed; entries are live | yes |
| `stale` | this attempt failed, but a previously cached copy is still compiled. `last_refreshed` dates that copy; `last_attempt` dates the failure | **yes** |
| `failed` | the attempt failed and there is no usable copy | **no — blocking nothing** |
| `empty` | fetched and parsed fine, and still produced no usable entries (a format the parser rejects, or a file with no domains) | **no — blocking nothing** |

`last_error` carries the actual reason, not a generic string: `404 Not Found`
(the status line's own phrase), `dial tcp 10.0.0.1:443: connect: connection
refused` (the `*url.Error` wrapper stripped, since the URL is already on the
row), `parse failed: …`, or for `empty`, `fetched 4.5 MB, no usable entries —
250,431 lines skipped`.

The cache fallback is unchanged: a failed download still falls back to
`<data_dir>/lists/<id>.txt`, which is exactly why `stale` and `failed` are
different states rather than one.

Parser accepts hosts-style (`0.0.0.0 domain`, `127.0.0.1 domain`, `:: domain`),
ABP-subset (`||domain^`, `@@||domain^`), bare domains, and **`*.domain`
wildcards** (hagezi's `wildcard/*` files); `#`/`!` comments are stripped, and a
leading UTF-8 BOM no longer swallows the first line. ABP rules containing
`/ ^ $ * |` after the prefix are still skipped. Domains are lowercased, ≤253
chars, labels ≤63, charset `a-z 0-9 - _`, with **Unicode converted to punycode**
(`пример.рф` → `xn--e1afmkfd.xn--p1ai`) since that is the form a qname arrives
in. A line over 1 MiB is counted as skipped and parsing continues, rather than
failing the whole list. `kind=allow` uses the file's allow entries **plus** its
block entries, so a plain domain list works as an allowlist.

**A block list's `@@||` exceptions are honoured, scoped to that list.** They
compile into a companion set consulted before the list's block set, so a list
that blocks `||example.com^` and exempts `@@||cdn.example.com^` does not block
`cdn.example.com` (or anything below it) — while another block list naming it
still does, and a block *rule* still wins. They are not promoted into the
allow-list tier.

**Downloading and compiling are separate.** Only `POST /filters/lists`,
`PATCH /filters/lists/{id}` with `enabled: true`, `POST /filters/refresh`,
`POST /filters/lists/{id}/refresh` and the `lists.refresh_hours` ticker
fetch; every other rule/list/assignment write
recompiles from the on-disk copies and returns without touching the network,
and startup compiles from them before the DNS listeners bind. A fetch is
capped at **64 MiB** and refuses any address that is not public (loopback,
link-local, private, unspecified) on the URL *and* on every redirect — both
show up as `last_status: "failed"` with the reason in `last_error`.

**`*.domain` is stored as `domain`.** The matcher (`DomainSet.Match`) walks
whole labels from the TLD inward and hits on any stored ancestor, so
`*.ads.example.com` and `ads.example.com` are the same entry — and it therefore
**also blocks the apex**, which strict AdGuard `*.x` syntax would exclude. That
is deliberate, and matches dnsmasq's `address=/x/`. Only a *leading* `*.` is
accepted.

Skipped-line counts are still not stored as a field, but they are **no longer
discarded**: they are folded into `last_error` for `last_status: "empty"`.

**Name derivation** (`store.DeriveListName`, mirrored for the Add dialog's
placeholder in `web/src/lib/list-name.ts`): GitHub raw/blob URLs
(`/<owner>/<repo>/<ref>/<path…>`) become `<owner> <path-below-ref>` —
`hagezi wildcard/pro.txt`, `StevenBlack hosts`; the branch is dropped.
Everything else becomes `<host-without-www> <last-path-segment>` —
`example.com hosts`. Capped at 60 chars with an ellipsis. A blank `name` on
`PATCH` **resets to this default**, it does not clear the label.

**TODO** — there is still no endpoint to edit a list's `url` or `kind` after
creation; only `name` and `enabled` are mutable.

### 3.3 Rule

| Field | Type | Notes |
|---|---|---|
| `id` | int64 | |
| `group_id` | int64 | from the **path**; a rule always belongs to exactly one group — there are no global rules |
| `action` | string | `allow` \| `block` |
| `pattern` | string | non-empty; a literal pattern is **stored normalised**, not verbatim |
| `is_regex` | bool | default false |

- **Regex rules**: ≤512 **bytes**, must compile with RE2, matched **unanchored**
  against the lowercased qname. Stored exactly as sent.
- **Literal rules**: validated through the same path a list entry takes, plus
  single labels (`localhost`) and minus the "must contain a dot" rule. Sent
  value is lowercased, its trailing dot and leading `*.` removed, and Unicode
  converted to punycode; what `GET` returns is that normalised form. Anything
  that is not a domain — `||x^`, `ads.*.example.com`, `*` — is
  `400 pattern must be a domain like example.com, *.example.com or localhost`
  rather than a stored rule that matches nothing. There is still no length cap
  beyond the 253-byte domain limit. Inserted into a domain set, so a rule
  matches the domain *and every subdomain*, on whole-label boundaries.
- Evaluation order: literal allow → regex allow → literal block → regex block →
  allowlists → blocklists. First match wins.
- The matched pattern is computed per query, reaches the pipeline response and
  the log entry (`dnssrv.Response.Matched`, `store.QueryLogEntry.Matched`), and
  since migration 0015 is stored in `query_log.matched` and served as
  `matched` on both query endpoints (§3.1). Rows logged before that column
  existed carry `""`: the entry is not recoverable from `rule_id`/`list_id`
  after the fact — a list's contents change under it and a rule can be deleted
  — so there is no backfill. The rail still resolves the rule and the list by
  id for their names, which is why a deleted rule reads *"Matched rule #N,
  which isn't available right now"* with the entry beside it.

**TODO** — no update endpoint; rules are create/delete only.

### 3.4 Client

| Field | Type | Notes |
|---|---|---|
| `id` | int64 | accepted in POST bodies but ignored |
| `name` | string | **not validated, may be empty** |
| `matcher` | string | exact IP **or** CIDR; **DB-unique**; **stored canonically**, not as sent |
| `group_id` | int64 | must be `> 0`; **not checked against an existing group** — a bad id fails the FK and returns 503 |

Matching: exact-IP map first, then CIDR list **longest-prefix-first**.
IPv4-mapped IPv6 is unmapped on both sides, so `::ffff:192.0.2.1` matches the
plain IPv4 client. No match → `group_id 1`, hardcoded
(`internal/clients/registry.go`).

**Canonicalisation on write** (`clients.NormalizeMatcher`, applied by both
`POST` and `PUT`, and again when the registry loads older rows): a CIDR is
masked (`10.0.0.1/24` → `10.0.0.0/24`) and an IPv4-mapped form is unmapped
(`::ffff:10.0.0.0/120` → `10.0.0.0/24`, `::ffff:192.0.2.5` → `192.0.2.5`), so
`GET` returns the spelling that will actually be compared. A mapped prefix
shorter than `/96` is not a v4 range and is rejected.

> **IPv6 zone ids are now rejected**, both as an address and as a prefix:
> `fe80::1%eth0` returns `400 matcher must be an IP or CIDR without an
> interface zone, and group_id set`. The request-side address is built with
> `netip.AddrFromSlice`, which never carries a zone (the UDP and TCP arms of
> `Server.serve`'s `RemoteAddr` switch, `internal/dnssrv/server.go`), so such
> a matcher could only ever be dead config. A stored row that predates this
> is skipped at load with a `WARN` naming the client id, rather than silently.

### 3.5 Group

| Field | Type | Notes |
|---|---|---|
| `id` | int64 | |
| `name` | string | required non-empty; **DB-unique** |
| `enabled` | bool | `false` means the group gets **no compiled ruleset at all**, so nothing is blocked for its clients |

The default group is created only when the table is empty, named `default`, and
on a fresh DB gets id **1**. Two places hardcode `1`: deletion refuses it, and
unmatched clients fall back to it.

### 3.6 Zone

| Field | Type | Notes |
|---|---|---|
| `id` | int64 | |
| `name` | string | apex, lowercase, no trailing dot |
| `type` | string | `primary` \| `secondary` \| `stub` \| `forwarder` \| `internal`; all but `internal` are creatable/patchable (§2.6). `forwarder` and `stub` hold no data of their own — they claim the suffix and route it (§3.8) |
| `enabled` | bool | a disabled zone is skipped by lookup entirely — it neither answers nor claims the name, so queries under it fall through to a shallower enabled zone or upstream, exactly as if the zone didn't exist (`internal/zones/zone.go`'s `Index.Find`) |
| `soa_ns`, `soa_mbox` | string | default to `ns.<name>` / `hostadmin.<name>` on create |
| `soa_serial` | uint32 | starts at `1`; bumped by one on every record create/update/delete in the zone — **and on a reverse zone whose PTR auto-PTR just wrote, moved or retired**, so one write addressed to a forward zone can move two zones' serials, and a reverse zone's serial can move with no request ever naming it (`BumpSerial`, best-effort — logged, not surfaced, on failure) |
| `soa_refresh`, `soa_retry`, `soa_expire` | uint32 (seconds) | default 900 / 300 / 604800 |
| `soa_minimum` | uint32 (seconds) | negative-cache TTL advertised for this zone's NXDOMAINs (RFC 2308), not a floor on positive answers; default 900 |
| `soa_ttl` | uint32 (seconds) | the SOA record's own header TTL, independent of `soa_minimum` — RFC 2308 §5 needs both to express `min(minimum, ttl)`. Fixed at `900`; **no request field sets it**, on create or patch |
| `primaries`, `tsig_key_id` | string / int64 | where this zone pulls from and the key it signs with — `secondary` and `stub` only, rejected on every other type. `primaries` is required non-empty on both |
| `refreshed_at`, `last_attempt`, `last_error` | int64 / int64 / string | the last pull that **succeeded**, the last one **tried**, and why that one failed (`""` on success). Written for a `secondary` and a `stub`; `0`/`""` on a type that never pulls. See [`docs/api.md`](api.md)'s Secondary zones entry |
| `expires_at` | int64 (unix ms) | **`secondary` only.** A stub is never given one — its NS set is routing information rather than data it vouches for, so it does not expire (`Zone.Serving`, and [`docs/architecture.md`](architecture.md)). `0` on every other type, **except** a row retyped from `secondary` to `stub`: `handleZonePatch` never clears the column, so that row keeps its old non-zero stamp forever. Nothing reads it — but do not read it either (§9.20) |
| `forward_to` | string | where a `forwarder` sends the queries it claims — `forwarder` only, rejected on every other type. See [`docs/api.md`](api.md)'s Forwarder zones entry for the format, and for why what is stored can differ from what was sent. **Empty is legal and means the zone claims its suffix and SERVFAILs it** (§3.8) |
| `allow_transfer`, `last_xfr_at`, `last_xfr_peer`, `last_xfr_error` | string / int64 | the outbound AXFR ACL and the last inbound transfer *request*'s outcome — `primary` and `secondary` only. See [`docs/api.md`](api.md)'s `allow_transfer` entry for the format and what each read-only field means (and why `last_xfr_peer` is not proof of who asked) |
| `notify_to` | string | who this zone tells when it changes (DNS NOTIFY, RFC 1996) — `primary` and `secondary` only, the same reach as `allow_transfer`. See [`docs/api.md`](api.md)'s `notify_to` entry for the format. Per-target delivery status is a separate read, `GET /zones/{id}/notifies`, not a field on the zone itself — see §9's Notify-out item |
| `next_attempt_at`, `failures` | int64 (unix ms) / int | the scheduler's own view: when the next attempt is allowed (`0` when no back-off is pending) and how many have failed in a row. **Process-local, not columns** (`zones.Refresher.Status`) — a restart reports `0`/`0` for a zone that is still failing, so they are read *beside* `last_error`, never instead of it. `0`/`0` on every type that does not pull |
| `created_at`, `modified_at` | int64 (unix ms) | |

### 3.7 Zone record

| Field | Type | Notes |
|---|---|---|
| `id` | int64 | |
| `zone_id` | int64 | FK → `zones.id` |
| `name` | string | relative to the zone apex: `@`, `bifrost`, `*`, `*.nexus` — never a fully-qualified name in storage, even if one was typed on write (§2.6) |
| `type` | string | any DNS RR type `dns.NewRR` parses — not a closed enum on the server; the dashboard's create/edit form offers `A`, `AAAA`, `CNAME`, `TXT`, `MX`, `SRV`, `NS`, `CAA`, `PTR` (§8), **plus the record's own stored type when it is outside that nine**, so editing an imported `SSHFP`/`HTTPS`/`TLSA` cannot silently rewrite it |
| `ttl` | uint32 (seconds) | `0`–`2147483647` (RFC 2181 §8); every record in the same (`name`,`type`) RRSet must share one value (RFC 2181 §5.2, 409 on mismatch) |
| `rdata` | string | DNS presentation format, rdata portion only — validated by `dns.NewRR`, not a per-type schema |
| `enabled` | bool | a disabled record is dropped when the zone's served snapshot is built (`NewZone`) — indistinguishable from never having been written, so it can't turn an NXDOMAIN into a NODATA or suppress a wildcard that should otherwise match. The records grid draws one muted and marked `DISABLED` |
| `comment` | string | free text, never interpreted |

`PUT /zones/{id}/records/{rid}` is a **full replace**, and `zones.BuildRecord`
defaults an absent `enabled` to `true` and an absent `comment` to `""` —
neither is editable in the dashboard, so the edit row sends both back
unchanged rather than resetting them.

**Answering** (`internal/zones/answer.go`, `Zone.Answer`) — the zone cut is
found first (deepest enabled zone whose apex suffixes the query name wins;
`e412.in` beats `in` if both exist), then, once inside a zone:

1. An `NS` record below the apex is a delegation: referral (`NS` in
   AUTHORITY, in-zone glue in ADDITIONAL, `aa=0`), not an answer from here.
2. An exact name+type match answers, `aa=1`.
3. An exact name match with a `CNAME` present follows it — appending the
   `CNAME` and, if the target is inside this zone, continuing resolution
   there (up to 8 hops) so the final answer lands in one response. A query
   for the `CNAME` itself is not followed.
4. An exact name match with other types but not the one asked for is
   NODATA: `NOERROR`, empty ANSWER, the zone's SOA in AUTHORITY.
5. A name that doesn't exist but has descendants (an empty non-terminal —
   including the apex itself) is also NODATA, not NXDOMAIN — RFC 8020 makes
   NXDOMAIN a claim about the name *and everything below it*.
6. Otherwise, the wildcard at the closest existing encloser is tried
   (`*.<encloser>` only — RFC 4592 §3.3.1, not a walk up through every
   ancestor); a wildcard **never** matches a name that exists with other
   types (RFC 4592 §2.2), which step 4 already excludes by construction.
   `a.*.zone` (asterisk not leftmost) is a literal owner name, not a
   wildcard (RFC 4592 §2.1.1) — legal data, answers only for itself.
7. Nothing matched: NXDOMAIN, SOA in AUTHORITY.

Every negative answer's AUTHORITY-section SOA carries
`ttl = min(soa_minimum, soa_ttl)` (RFC 2308 §5). `forwarder` and `stub`
zones hold no data and never reach this logic: `Zone.Answer` returns
`handled=false` and the query falls through to the next middleware. **That
is not the same as the name being uncovered** — the fall-through lands in
the cache and then in the upstream forwarder, which holds a routing table
keyed on exactly these zones' apexes and sends the query to that zone's own
upstreams, never to the default ones (§3.8).

### 3.8 Upstream

**There is no upstream table and no upstream entity.** Upstreams are a single
comma-separated settings string. Accepted forms: `1.1.1.1`, `1.1.1.1:53`,
`::1`, `[::1]:5353`, hostnames — a bare address gets `:53` appended, IPv6
literals bracketed first. Validation is only "non-empty after trimming"; a
genuinely unusable address is discovered at forwarder-build time.

**Transports:** plain UDP with TCP fallback on truncation, `tls://` (DoT) and
`https://` (DoH) — the address grammar and provider presets are in
[`configuration.md`](configuration.md). No DoQ, deliberately (E1 spec §"Deliberately
not implemented").

`upstream.strategy`:

| Value | Behaviour |
|---|---|
| `race` | fires all healthy upstreams concurrently, takes the first non-error, non-SERVFAIL reply |
| `fastest` | sorts by EWMA latency (`(old*7 + new)/8`), then tries **sequentially** |
| `failover` | same sequential loop, in **configured order** |

Shared: an upstream is marked down for **15s after 3 consecutive failures**; if
all are down, all are tried anyway; failures are negatively cached **30s** per
(qname,qtype) per RFC 9520; per-exchange timeout is **2s and not configurable**;
0x20 case randomisation with strict echo checking is always on.

**Conditional / split-horizon forwarding shipped in Milestone D6.**
`upstream.Config.Conditional` is populated from **zone rows** — every enabled
`forwarder` and `stub` zone's apex, mapped to that zone's upstreams — and
from nothing else. There is still no setting and no separate table, by
design: the zone row is the one place a suffix is claimed.
`Forwarder.SetConditional` swaps the table behind an `atomic.Pointer` on
every zone reload, reusing each address's `*up` so the swap preserves latency
EWMA and health backoff.

A forwarder's addresses come from `forward_to` (§3.6) and are dialled as
written — a hostname there is resolved by Go's dialer per exchange, never by
dnsaur. A stub's are derived from the NS records it fetched: an **in-zone**
nameserver's address must arrive as glue in the master's ADDITIONAL section
and is never looked up (looking it up would route back into this same zone),
an **out-of-zone** one is looked up normally, and either way the address is
stored beside the NS record so `zones.StubUpstreams` can rebuild the table on
reload without querying. Those upstreams are **always port 53**: neither glue
rdata nor an address lookup carries one, so `StubUpstreams` joins 53
unconditionally.

Matching is longest-suffix (one shared `filter.DomainSet`), and **a matched
suffix never falls back to the defaults**: with every upstream in that
suffix's list failing, or the list empty, the query gets SERVFAIL and is
negatively cached 30s like any other upstream failure — *unless the cache
still holds an answer that suffix's own upstreams produced earlier*, in which
case serve-stale answers it NOERROR at TTL 30, exactly as for any other
forwarded name. That is not a fall-through and not an exception to the rule
above: the reply is the zone's own data going stale, never the defaults'.
"Never falls back to the defaults" is about the *route*, and it is absolute;
"SERVFAILs" is about the *outcome*, and it holds when there is nothing
cached beneath the suffix. That is the point of
the feature rather than a gap — see
[`docs/architecture.md`](architecture.md#a-claimed-suffix-servfails-it-never-falls-through).
A disabled zone contributes nothing, so disabling one releases its suffix
back to `upstreams`.

Installing that table also **purges the cache** of every suffix whose route
set changed — added, removed or altered (`Cache.Purge`, from
`App.installConditional`). The cache sits above the forwarder and its entries
record no route, so without it a name cached from `upstreams` before a zone
claimed its suffix would go on being served from that entry, and would be
served *stale* for `cache.serve_stale_for` once the claimed upstreams failed.
Nothing in the UI surfaces the purge; it matters here because it is why a
zone edit takes effect on the next query rather than on the next TTL
expiry.

### 3.9 Settings keys

Full editable allowlist. Values are always strings on the wire.

| Key | Default | Allowed | Reload |
|---|---|---|---|
| `upstreams` | `1.1.1.1:53,1.0.0.1:53,9.9.9.9:53` | non-empty | **hot** |
| `upstream.strategy` | `race` | `failover` \| `fastest` \| `race` | **hot** |
| `blocking.mode` | `null-ip` | `null-ip` \| `nxdomain` | **hot** |
| `blocking.ttl` | `30` | int ≥ 0 | **hot** |
| `cache.min_ttl` | `0` | int ≥ 0 | **restart** |
| `cache.max_ttl` | `86400` | int ≥ 0 | **restart** (stored `0` → 24h) |
| `cache.max_entries` | `10000` | int ≥ 0 | **restart** (stored `0` → 10000) |
| `cache.serve_stale_for` | `86400` | int ≥ 0 | **restart** (stored `0` → 24h) |
| `lists.refresh_hours` | `24` | int ≥ 1 | **restart** for the cadence — a settings change recompiles from the cached copies rather than re-downloading |
| `qlog.retention_days` | `90` | int ≥ 0 | **hot, delayed** — re-read per prune run, so it lands on the next 24h tick |
| `qlog.privacy` | `full` | `full` \| `anon` \| `none` | **hot**, read per query |
| `stats.retention_days` | `365` | int ≥ 1 | **hot, delayed** — same prune run as `qlog.retention_days` |
| `sync.peer_url` | *(empty)* | empty, or an absolute `http`/`https` URL | **hot** — it is what makes this instance a replica, and clearing it is the promotion |
| `sync.token` | *(empty)* | any string — **write-only, never in `GET /settings`** | **hot** |
| `sync.interval_seconds` | `30` | int ≥ 5 | **hot** |
| `sync.primary_dns` | *(empty)* | empty, or `host:port` with the **host present** | **hot** |
| `sync.tsig_key_id` | `0` | `0`, or the id of an existing TSIG key | **hot** |

No upper bound on any integer key. Two have a lower bound above zero, and
both are rejected with `must be a whole number, one or more`:
`lists.refresh_hours`, which becomes a tick interval, and
`stats.retention_days`, where `0` would delete every hourly bucket on the
next prune. `blocking.mode` treats **anything ≠ `nxdomain`** as null-ip.

`sync.peer_url` is a **scheme and a host and nothing else** — the pull loop
joins `/api/v1/sync/...` onto it, so a path, a query, a fragment or
credentials are refused with `invalid value for sync.peer_url: must be a
scheme and host only, with no path, query or credentials`. A lone trailing
slash is accepted and stripped before the value is stored, so
`https://main.lan/` reads back as `https://main.lan`.

The peer and the token are checked **in both directions**. Setting
`sync.peer_url` non-empty requires `sync.token` — already stored, or sent in
the same map (`invalid value for sync.peer_url: set sync.token first`), and
clearing `sync.token` while a peer is configured is refused the same way
(`invalid value for sync.token: clear sync.peer_url first`); either state
would be a replica that pulls a 401 forever. The handler judges `sync.token`
before `sync.peer_url` so the Sync band can save both in one request, and
judges a peer *being cleared* before either, so "stop following" can send
`{"sync.peer_url": "", "sync.token": ""}` in one request too.
`sync.tsig_key_id` is checked against the keys that exist:
`invalid value for sync.tsig_key_id: no TSIG key has that id`.

Non-editable keys that exist but are stripped from `GET /settings`:
`instance.id`, `stats.watermark`, `blocking.pauses` (the stored pause state,
see [Blocking pause](#blocking-pause) — the other `blocking.*` keys above are
ordinary settings and stay visible). `sync.token` is stripped too and is the
only *editable* key that is: it is the credential this instance pulls its
config with, so a read must not be a way to copy it out. The band shows
**set** / **not set** from whether the key is absent, and writes it like any
other setting.

### 3.10 Tokens

`kind` is `session` or `api` — **not** a DB or API-level enum, just the only two
values the code writes. Session tokens always have `scope: "write"`, `name: ""`,
and a 30-day sliding expiry; an API token has `expires_at: 0` (never) unless one
was chosen at creation, and that one never slides. Token material is
32 random bytes, base64url for the plaintext, SHA-256 hex stored.

### 3.11 DHCP lease — **TODO, nothing exists**

There is no lease entity, no table, no route, no UI type, and no `:67` listener.
A case-insensitive search for `dhcp` across all Go, SQL, and TypeScript files
returns **zero implementation hits**. The only occurrences are prose: the README
feature table (`| DHCP | Planned |`), the architecture doc's "Phase 2 … not yet
present", and one line in `openapi.yaml:6` describing the project as a
"DNS/DHCP server" — which is aspirational; no DHCP path is defined in that
document either.

Anything a UI shows for DHCP today would be invented.

### 3.12 Never serialized

| Struct | Field |
|---|---|
| `store.User` | `PasswordHash`, `TOTPSecret`, `TOTPLastStep` (`json:"-"`) |
| `store.AuthToken` | `TokenHash` (`json:"-"`) |

Computed and discarded, so the UI cannot have them: the matched
rule/list pattern, the parser's skipped-line count, and the query-log dropped
count. Also note **there is no join returning a client or group *name* alongside
a query row** — the UI must resolve `client_id` itself against `GET /clients`.

### 3.13 Sync status

One shape for both roles; read `role` first, since every other field is
`omitempty` and a main with no replicas is `{"role": "main"}` and nothing
else. From `GET /sync/status` and from `GET /resolver/status`'s `sync`.

```json
{"role":"replica","peer_url":"https://main.lan","peer_version":412,
 "applied_version":411,"applied_at":1757580000000,"last_pull_at":1757580030000,
 "last_error":"","plain_http":false}
{"role":"main","sync_key":"xfer.e412.in.",
 "replicas":[{"instance_id":"V1StGXR8Z5jdHi6BmyT","dns_addr":"10.0.0.6:53",
              "version_applied":412,"last_seen":1757580030000,"stale":false}]}
```

| Field | Role | Meaning |
|---|---|---|
| `role` | both | `main` or `replica`. Derived from `sync.peer_url`, not stored as a mode |
| `peer_url` | replica | verbatim what `sync.peer_url` holds; the same string the 409 quotes |
| `peer_version` | replica | the main's `config_version` at the last probe — `peer_version - applied_version` is "behind by N" |
| `applied_version`, `applied_at` | replica | the last bundle actually applied, and unix ms of when |
| `last_pull_at` | replica | unix ms of the last attempt, successful or not |
| `last_error` | replica | why the last pull failed, `""` when it did not. A failed pull leaves the previous config in force — DNS is unaffected |
| `plain_http` | replica | the peer is `http://`, so the bundle (TSIG secrets included) crosses in the clear on every pull. Persistent while it is true; there is no certificate subsystem and this warning is the whole mitigation |
| `sync_key` | main | the **name** of the TSIG key replicas transfer under, or `""` when none is designated |
| `replicas[]` | main | `instance_id`, `dns_addr`, `version_applied`, `last_seen` (unix ms, stamped by the main), `stale` |

---

## 4. Stats: computed vs stored

**Everything under `/stats/*` is pre-aggregated. Nothing is computed on read.**
The only stats table is `stats_hourly (bucket, metric, key, value)`. Only
`GET /queries` reads the raw log.

- **Bucket granularity: one hour**, stored as unix *seconds* of the hour start
  (`(at/3600000)*3600`).
- Four metrics per pass: `decision` (all rows), `domain` (only
  `forwarded`/`cached`/`stale`), `blocked_domain` (only `blocked`), `client`
  (all rows).
- Progress is a **`query_log.id` watermark** in `stats.watermark`, not a clock.
- Upserts are additive.

**Rollup cadence: every 60 seconds**, hard-coded, with **no initial tick** — so
the first rollup happens at T+60s after start, and every restart resets that
clock.

> **What a fresh install shows.** For roughly the first minute after start,
> `/stats/overview` returns all zeros, `/stats/timeline` returns `[]` and
> `/stats/top` returns `[]` — while `GET /queries` already lists the queries and
> the SSE tail is streaming them live. Verified during capture: six real `dig`
> queries were visible in the query log immediately but `overview` read
> `total: 0` until the rollup ran, then jumped to `total: 7`. The dashboard
> polls stats every 30s, so a tile can sit at zero for two poll cycles while the
> query table is visibly filling. In steady state `/stats/*` lags reality by
> 0–60s.

**Window mapping is lossy at the edge.** `from` is a raw unix second
(`now - hours`), compared against hour-aligned bucket starts, so the oldest
partially-covered hour is dropped whole. At 14:30 with `hours=24` you get ~23.5
hours; with `hours=1` you get only the current hour's bucket — which just after
the top of an hour is nearly empty.

**Downtime self-heals** because progress is an id watermark: after an outage the
next tick rolls everything up into its correct historical bucket. Real gap
sources: retention pruning racing the rollup (only with a very small
`retention_days`), a corrupt `stats.watermark` (blocks rollups permanently), and
on Postgres a concurrent insert holding a lower id than the `MAX(id)` read at
transaction start.

**`stats_hourly` has its own retention**, `stats.retention_days` (default
**365**), applied by the same daily pass that prunes the query log: buckets
starting before the cutoff are deleted, in chunks. It is deliberately far
longer than `qlog.retention_days` — hourly totals are small, and they are
all that is left of a month once its per-query rows are gone.

---

## 5. Query log: retention, buffering, page size

**Retention** — `qlog.retention_days`, default **90**, unit days. Pruning runs
**once at startup, then every 24 hours**, deleting `WHERE at < cutoff` in
batches of 10000 rows with a short pause between them, so lowering retention
on a large log does not hold the database long enough to time out the log's
own buffered writes. The value is re-read from the DB on every prune, so a
change lands on the next tick without a restart. `0` is a valid value and
means "delete everything". The same pass prunes `stats_hourly` on
`stats.retention_days` (§3.9).

**Write path is buffered**: channel capacity **10000**, batch size **1000**,
flush every **1 second**, 5s DB timeout. **Overflow drops the oldest entry and
never blocks.** The drop counter is reported two ways: a `WARN` from the flush
loop, at most once a minute, and `dropped` on `GET /stats/overview` (§2.8) —
a running total since process start, not since the window the other figures
cover. Flush failures discard the batch with no retry.

**Visible latency:**

| Surface | Delay |
|---|---|
| SSE tail | immediate — published synchronously from the DNS middleware |
| `GET /queries` | up to **~1 second** (the flush tick) |
| `/stats/*` | up to **60 seconds** on top of that |

So the live tail and the paged table are transiently inconsistent by about a
second, by design.

**Page size**: `limit` default **100**, max **1000**. Plain SQL `OFFSET`.
Ordering `id DESC`.

**Disabling** — there is **no setting that disables the query log as a
subsystem**. The functional equivalent is `qlog.privacy = "none"`, which makes
the middleware return before writing or publishing anything: existing rows stay,
`GET /queries` still returns 200 with history, and the SSE stream opens normally
and then stays permanently silent.

**Privacy** changes only what is *stored*; nothing is redacted at read time.
`anon` alters **only `client_ip`** — IPv4 last octet zeroed, IPv6 keeps the
first 48 bits. Everything else is untouched, and pre-existing rows keep their
full IPs.

**Search caveat**: `q` is `LIKE %…%` on a lowercase column. That is ASCII
case-insensitive on **SQLite** but case-sensitive on **Postgres**, so an
uppercase search term matches under SQLite and returns nothing under Postgres.
Verified: `?q=EXAMPLE` returned 9 rows on SQLite.

**Other hard caps a UI can hit**: SSE per-subscriber buffer 64; `/stats/top` `n`
max 100; JSON body 1 MiB; pause 1–1440 minutes; SQLite is pinned to **one
connection**, so API reads serialise behind writes.

---

## 6. Per-row actions that work today

Every action waits for the server. **Nothing anywhere is optimistic** — there is
no `onMutate` and no `setQueryData` in the whole client.

| Screen | Action | Endpoint | Failure string (verbatim) |
|---|---|---|---|
| Dashboard → Live queries | Block (Allow on an already-blocked row) | `POST /groups/{id}/rules` | `Couldn't block ${q_name}` |
| Dashboard → Top blocked | Allow | same | `Couldn't allow ${pattern}` |
| Dashboard → Top clients | — | — | **no row action exists** |
| Query log | Block / Allow | `POST /groups/{id}/rules` | `Couldn't ${verb} ${entry.q_name}` |
| Query log | Block/Allow, client unresolvable | *no request made* | `Couldn't ${verb} ${q_name} — this client's group is unknown` |
| Query log | Why this decision? | local only | — |
| Lists | Toggle enabled | `PATCH /filters/lists/{id}` | `Couldn't ${enabled ? "enable" : "disable"} ${list.name}` |
| Lists | Delete | `DELETE /filters/lists/{id}` | `Couldn't delete ${target.name}` |
| Lists | Add | `POST /filters/lists` | server message, else `Couldn't add the list` |
| Lists | Rename | `PATCH /filters/lists/{id}` | server message, else `Couldn't rename ${target.name}` |
| Lists | Refresh all | `POST /filters/refresh` | `Couldn't start a refresh — try again` |
| Lists | Refresh now (one row) | `POST /filters/lists/{id}/refresh` | `Couldn't refresh ${list.name}` — success is `Refreshed ${list.name}`, past tense, because the request does not return until the download has |
| Rules | Delete | `DELETE /filters/rules/{id}` | `Couldn't delete the rule for ${target.pattern}` |
| Rules | Add | `POST /groups/{id}/rules` | server message, else `Couldn't add the rule` |
| Groups | Toggle enabled | `PATCH /groups/{id}` | `Couldn't ${enabled ? "enable" : "disable"} ${group.name}` |
| Groups | Rename | `PATCH /groups/{id}` | `Couldn't rename ${group.name}` |
| Groups | Delete | `DELETE /groups/{id}` | `Can't delete ${group.name} — it's still in use` (on 409) |
| Groups | Apply/remove a list | `PUT /groups/{id}/lists` | `Couldn't update lists for ${group.name}` |
| Clients | Add | `POST /clients` | server message, else `Couldn't add the client` |
| Clients | Delete | `DELETE /clients/{id}` | `Couldn't delete ${target.matcher}` — the **matcher**, not the name, which may be empty (§3.4) |
| Zones | Add | `POST /zones` | server message, else `Couldn't add the zone` |
| Zones | Delete | `DELETE /zones/{id}` | `Couldn't delete ${target.name}` |
| Zone detail | Enable / disable | `PATCH /zones/{id}` | `Couldn't ${enabled ? "disable" : "enable"} ${target.name}` |
| Zone detail | Save SOA | `PATCH /zones/{id}` | server message, else `Couldn't save the SOA` |
| Zone detail | Edit primaries / upstreams | `PATCH /zones/{id}` (`primaries` + `tsig_key_id`, or `forward_to`) | server message, else `Couldn't save the ${primaries\|upstreams}` |
| Zone detail | Edit / Add record | `PUT\|POST /zones/{id}/records[/{rid}]` | server message, else `Couldn't ${update\|add} the record`; a **404** is `That record no longer exists` and closes the row instead |
| Zone detail | Delete record | `DELETE /zones/{id}/records/{rid}` | `Couldn't delete ${target.name}`; a **404** is `That record no longer exists` |
| Account | Revoke token | `DELETE /tokens/{id}` | `Couldn't revoke ${token.name}` |
| Account | Create token | `POST /tokens` | server message, else `Couldn't create the token` |
| Account | Enable 2FA | `POST /auth/totp/start` | server message, else `Couldn't start setup — try again` |
| Account | Confirm / Disable 2FA | `POST /auth/totp/confirm\|disable` | **inline field error**, not a toast: server message, else `Invalid code — try again` |
| Shell | Pause blocking | `POST /blocking/pause` | `Couldn't pause blocking — try again` |
| Shell | Resume blocking | `DELETE /blocking/pause` | `Couldn't resume blocking — try again` |
| Shell | Log out | `POST /auth/logout` | `Couldn't sign out — try again` — **suppressed on 401**, which counts as logged out |
| Settings | Save changes (bulk) | one `PUT /settings` per changed key | three branches, not one: all saved → **success** toast `${n} setting(s) updated`; some saved → `Saved ${n}, but couldn't save ${keys} — try again`; none saved → the first rejection's server message, else `Couldn't save settings — try again`. Leaving the page with anything unsaved is blocked by a dialog first: `Leave without saving?` / **Stay** / **Leave** |

> **Known gap — this table is missing eleven row actions.** The whole **TSIG
> keys** screen is absent (Add, Save, Delete — the last being
> `Couldn't delete ${target.name}`, and the only Delete here that can come
> back **409 `resource in use`**), and so are Zone detail's **Refresh now**,
> **Export**, **Import**, **Save master/upstreams**, **Save allow_transfer**
> and **Save notify_to**, and the Zones list's inline **create-row** save.
> Absent means undocumented, not absent from the UI. Adding them is its own
> piece of work; until then read this table as covering the screens it
> lists rather than as covering the app.

**Disabled states that actually occur:** dashboard quick actions while
`groups.isPending || groups.isError`; query-log Block/Allow while
`clients.isPending || clients.isError`, and on a row whose `q_name` is `""`
(`This query carried no name to write a rule for`); the query log's **Client**
filter whenever `qlog.privacy` is not `full`, which shows the single option
`Client IPs masked`; Add client when no groups exist (`Create a group first`);
delete on group id 1 (`The default group can't be deleted`); delete on a group
any client still points at (`Move its N clients first`) — the 409 above is the
race guard behind it, not the first line of defence; a list toggle while that
group's assignment read is in flight; a row's **Refresh now** while its own
download is running (the icon spins, and only that row's is disabled); Save
changes when the form isn't dirty.

Settings saves are per-key via `Promise.allSettled`, so a partial failure keeps
the failed fields dirty with the admin's typed value intact.

The `upstreams` editor hands the form whatever the rows currently spell, valid
or not, so a rejected row blocks Save through the field's own schema (the same
`ParseUpstreams` grammar). It used to withhold a rejected value, which left the
form holding the last valid one — an edit that looked applied, was silently
discarded on save, and lost its row error on the resync.

---

## 7. Loading / empty / error states

The client distinguishes two error cases, and the distinction is load-bearing:

- `isError && data === undefined` → a **destructive alert**, content replaced.
- `isError && data !== undefined` → a **stale-data banner above still-valid
  content** (`Couldn't refresh {what}` / "Showing what last loaded
  successfully." / `Try again`). A blipped background poll never wipes good
  data.

The chrome's blocking cell obeys the same rule without a banner: `Status
unavailable` is only for a `GET /blocking` that has never answered. A failed
poll keeps the last state it read — countdown included — and keeps `Resume`
greyed out if that state says there is nothing to resume.

| Screen | Pending | Empty | First load failed | Background refetch failed |
|---|---|---|---|---|
| Dashboard stat strip | 4 skeletons | n/a (`—` for both percentages when `total` is 0) | `Couldn't load stats` | stale banner |
| Dashboard query volume | skeleton | `No query activity yet` | `Couldn't load the timeline` | stale banner |
| Dashboard rail panels | 4 row skeletons | `Nothing blocked in this window yet` / `No clients have queried in this window yet` | plain text `Couldn't load this list.` | stale banner |
| Dashboard live queries | n/a (the stream, not a query) | `Listening — queries appear here as dnsaur answers them` | n/a | n/a |
| Query log | skeleton **only in filtered mode** | `Waiting for traffic` (live) / `No matching queries` (filtered) | `Couldn't load queries` | **nothing — no stale banner exists here** |
| Lists | 4 skeletons | `No filter lists yet` | `Couldn't load filter lists` | stale banner, plus a **per-list** destructive banner (`N filter lists are blocking nothing`) and a warning one (`… serving older copies`) driven by `last_status` |
| Rules | 4 skeletons | `No rules for this group yet` | `Couldn't load rules` | stale banner |
| Groups | 3 skeletons | `No groups yet` | `Couldn't load groups` | stale banner |
| Clients | 4 skeletons | `No clients yet` | `Couldn't load clients` | stale banner |
| Zones | 4 skeletons | `No zones yet` | `Couldn't load zones` | stale banner |
| Zone detail | 4 skeletons (zone), then 4 more (records) | filtered: `No records match this filter.` Unfiltered, a four-way switch on type (`detail.tsx`): **secondary** → `Nothing transferred yet.`; **stub with a recorded failure** → `No NS set yet.`; **stub without one** → `Fetching the NS set from ${primaries}`; **otherwise** → `No records yet.` A **forwarder** has no empty state at all — it renders no records grid | `Couldn't load this zone` (zone) / `Couldn't load records` (records); a **404** on the zone is `This zone no longer exists` with a link back to the list instead, and a non-numeric `:id` renders the not-found screen without requesting anything | stale banner on **records only** — a background zone refetch failing has no banner of its own |
| Settings | layout-shaped skeleton | n/a (fixed 12 fields) | `Couldn't load settings` | stale banner, "Any edits below are untouched." |
| TSIG keys | 3 skeletons | `No TSIG keys yet.` with a **New key** action — suppressed entirely while the create row is open, since the row is already the answer | `Couldn't load TSIG keys` | stale banner |
| Account | 2-card skeleton | `No API tokens yet` | **two**: `Couldn't load your account` (the account card, first load failed) and `Couldn't load API tokens` (the token card, independently) | stale banner |

**Live-tail mode has no loading state at all** — it renders the empty state
until the first SSE row arrives.

**Auth gate:**

| State | Renders |
|---|---|
| `me` pending | full-page spinner |
| `me` failed with **anything but 401** | `Can't reach dnsaur` banner with a manual `Retry` |
| `me` 401 + setup pending | full-page spinner |
| `me` 401 + setup **also failed** | `Can't reach dnsaur` banner with a manual `Retry` |
| `me` 401 + `setup_required: true` | Setup wizard |
| `me` 401 + `setup_required: false` | Login |
| `me` success | app shell |

**Only a 401 means "signed out".** `me` is retried once, after 500ms, on
anything else (5xx, a fetch `TypeError`) and a still-failing non-401 is an
unreachable server, not an absent session — a restart mid-load must not walk an
operator with a valid cookie to a login form. `Retry` on the banner invalidates
`me` as well as refetching `setup`, so recovery does not depend on signing in
again.

A **mid-session 401** from any query revalidates `me` once (re-entrancy
guarded); if it now fails, all non-auth cached data is dropped and the gate
falls back to Login.

**Polling** — two kinds, and the second is conditional, which is what this
paragraph got wrong until D6's docs pass.

*Unconditional, always 30s:* the four dashboard stats queries (overview,
timeline, top blocked, top clients), `GET /health` (the top bar's row-1
`DNS OK` readout, every screen), and `GET /blocking` (pause control, every
screen plus one per group row).

*Conditional, 30s, and only while there is something that can change on its
own* (`web/src/hooks/use-zones.ts`): `useZones` and `useZone` set both
`refetchInterval` **and** `refetchOnWindowFocus` from `watchesTransfers` —
true when a zone in view is a `secondary` or a `stub`, the server's own
`pullsFromAMaster` set. `useZoneNotifies` does the same, gated on the notify
list being non-empty. A zones screen showing only primaries and forwarders
polls at nothing and refetches on focus at nothing, because nothing on it
moves without an operator.

*One-off, not a poll:* `useRefreshFilters` (`web/src/hooks/use-filters.ts`)
re-reads the list table on a **1s / 4s / 12s** ladder after a manual refresh,
because `POST /filters/refresh` is a 202 that resolves long before any row
changes. Its per-row sibling `useRefreshList` has no ladder and needs none:
that 202 arrives after the download, so a plain invalidate already reads the
new state.

Everything else is fetch-on-mount with a 10s stale time and no window-focus
refetch; `me` is a further exception, refetching on focus only once it has
succeeded, and retrying once (500ms) on any failure that is not a 401.

**SSE client behaviour** — backoff doubles 1s → 30s; after **6** consecutive
failures it gives up and shows `Live tail disconnected` with a manual
Reconnect, and invalidates `me` on the way out so an expired session reaches
the auth gate instead of a dead button. A connection counts as working — and
so resets the failure count and the backoff — only once it has **delivered a
message or stayed open 5s**: EventSource reports a 401 as an error *after* the
stream opened, so "it opened" alone would reset the backoff on every attempt
and the retries would never run out. Arrivals batch at 100ms; the ring buffer
holds **500 rows**, newest first, and is never cleared when the stream stops.
Pausing the tail is per-visit: the flag is cleared when the query log unmounts,
because the buffer it governs is per-mount and the seed only runs while
unpaused.

**Keyboard** — one shortcut everywhere: ⌘K / Ctrl+K toggles the command
palette. It is registered on `window` with no target filtering, so it also
fires while typing in an input. The hint in the search cell reads `⌘K` on
Apple platforms and `Ctrl+K` everywhere else; the listener accepts either
modifier on both.

Three more are scoped to the zone detail page and listed on its filter bar:
`/` focuses the record filter, `n` opens the add-record row, `Esc` closes
whichever record form is open. Also on `window`, but **target-filtered**: `/`
and `n` do nothing while focus is in an input, textarea, select or
contenteditable, and none of the three fire with a modifier held. `Esc` is
deliberately *not* target-filtered — the form it closes is where the focus is
— and stands down entirely while a dialog is open, since the dialog closes
itself on that key. `n` is inert, and unlisted, wherever there is no add band to open — every
type but `primary`.

**Theme** — `dnsaur.theme` in `localStorage` (`light`/`dark`) overrides
`prefers-color-scheme`, and with nothing stored the app follows the OS live.
`public/theme-boot.js` applies the same rule from `<head>`, before the module
bundle, so a dark install does not paint light first. It is a file rather than
an inline script because the served CSP is `script-src 'self'` with no
`'unsafe-inline'` (`internal/api/static.go`), which would block one silently.

---

## 8. Screens

### Exists

| Screen | Route |
|---|---|
| Dashboard | `/` |
| Query log | `/queries` |
| Filtering → Lists | `/filtering/lists` (`/filtering` redirects here) |
| Filtering → Rules | `/filtering/rules` |
| Filtering → Groups & Clients | `/filtering/clients` |
| Zones | `/zones` |
| Zone detail | `/zones/{id}` |
| Settings | `/settings` |
| TSIG keys | `/tsig-keys` |
| Account & security | `/account` |
| Login | pre-shell |
| Setup wizard | pre-shell |

### Half-built

| Screen | What's missing |
|---|---|
| **Filtering → Groups & Clients** | CRUD works, but the per-group "Lists (n)" menu has **no error state**: it's disabled only while `isPending`, not on `isError`, and its toggle rebuilds the assignment set from `groupLists.data ?? []`. If that read failed, clicking one list PUTs `[thatOne]` and **silently drops every other assignment**. |
| **Filtering → Lists** | The table leads with the list's `name`; the URL is a muted second line and stays in the row's `title`. Actions (toggle, rename, delete) are labelled by name. The **Status** column replaces the old "Last refreshed" one and carries the badge plus a plain-language line per `last_status`. |
| **Dashboard health** | Reduced to the shell's two row-1 readouts (blocking state, and `DNS OK`/`DNS down` from `GET /health`, whose `version` is the readout's `title`). Filter-list freshness moved off the dashboard with the redesign and now lives only on Filtering → Lists. The spec's "upstreams healthy" signal **has no code at all** — there is no upstream-health endpoint. |
| **Settings** | 12 keys work. The spec's "storage (read-only info)" section is absent, with a code comment noting no endpoint exists to source it. |
| **Account** | TOTP, tokens and password are complete: the Password section holds a current/new/confirm form and a **Log out everywhere** action (`POST /auth/password`, `DELETE /auth/sessions`, §2.2). |
| **Command palette** | Navigates to the 9 leaf pages only, grouped by nav section. The spec's "quick actions (pause, block a domain)" don't exist. |

### Not started

| Screen | Status |
|---|---|
| **404 / unknown route** | renders a dedicated not-found screen inside the shell, no group marked in row 2 (`pages/not-found.tsx`) — this table is stale on this point in older captures; `path="*"` no longer redirects |
| **DHCP** | nothing exists (§3.11) |
| **DNSSEC** | **deferred by decision (2026-09-08), not a gap awaiting work** — no signing, no validation, no UI; validation is scheduled with own-recursion, signing with a hosted zone that needs a DS (README status table, main design spec decisions). Every other zone type on this row has now shipped and left it: `secondary` in D2–D4 (D2 the transfer client, D3 the AXFR server gated by `allow_transfer`, D4 NOTIFY in both directions — §9.18), and `forwarder` and `stub` in D6 (create/patch, the conditional routing table, the stub's SOA/NS fetch, and both page shapes — §3.8, §2.6). **Reverse zones were never on this list either**: `PTR` is a normal record type, a reverse zone is an ordinary `primary` zone ending in `.arpa`, the RFC 6303 §4 built-ins (`internal/store/builtins.go`'s `BuiltinZones`) are seeded as `type: internal` (read-only, `409` on any write), and an A/AAAA write maintains the matching PTR server-side in the same request |
| **HA / cluster UI** | no code; the spec anticipated a health-strip stub, which does not exist |

The nav contains exactly the nine implemented leaf routes, in four groups
(Monitor: Dashboard, Query Log · Filtering: Lists, Rules, Groups & Clients ·
Zones: Zones · System: Settings, TSIG keys, Account) — there are no dead nav
entries pointing at unbuilt screens. The group holding Zones is internally still
named `network` (a stable id for keys/tests), but its label and only child
are both "Zones" — it replaced the flat Local DNS override table, not just
its own nav entry. Theme and log out live under System too; the shell has
no sidebar and no avatar.

### Dashboard layout (`/`)

A full-bleed grid of hairline-separated bands, no cards, everything mono:

1. **Stat strip** — `QUERIES` / `BLOCKED` / `CACHE HIT RATE` / `ACTIVE CLIENTS`
   from `GET /stats/overview`. Both percentages divide by `total`, which sums
   *every* decision including `authoritative` and `error` — so
   `blocked + cached + forwarded ≤ total`, and no "allowed" figure is ever
   derived by subtraction.
2. **Query volume** — rnui `BarChart`, `RESOLVED` stacked under `BLOCKED`
   ("resolved" = every non-`blocked` decision, errors included, so the two
   bands total the strip above). `GET /stats/timeline` returns only hours that
   had traffic, keyed by a unix-**seconds** hour start, and never zero-fills;
   the missing hours are synthesised client-side so the axis stays continuous.
   The header labels the granularity `hourly buckets` — the design says
   "15-min", but the endpoint cannot produce them.
3. **Bottom split** — `LIVE QUERIES` (the shared SSE tail, newest 12 rows,
   time / domain / client / decision / duration) beside a 320px rail holding
   `TOP BLOCKED` (6) and `TOP CLIENTS` (5). Rail rows carry an inline
   proportional bar sized against the largest value *in that panel*.

**Decision colours** (live rows): `blocked`/`error` destructive, `stale` warn,
`cached` muted, `forwarded` foreground, `authoritative` primary. There is no
`allowed` — the value doesn't exist in the Go enum at all (§3.1).

**The rate readout** next to `LIVE QUERIES` has no endpoint: it is measured
from arrivals on the tail over a rolling 30s window, and renders nothing until
it has been listening for 10s.

**Top clients** resolve to a device name only via an exact-IP matcher in
`GET /clients`; a CIDR matcher covers an address without identifying it, so
those rows show the bare IP.

**The window** (1h / 24h / 7d) lives in the URL as `?window=`, written by the
shell's second chrome row and read by the page. Anything unrecognised falls
back to 24h. That row also carries the app's one filled cell,
`VIEW QUERY LOG →`; both are shown on `/` only.

---

## 9. Behaviour that is easy to get wrong

Collected because each one has already caused, or would cause, a wrong UI.

1. **`total` ≠ blocked + cached + forwarded.** `total` sums *every* decision
   including `authoritative` and `error`. Don't compute "allowed" by
   subtraction.
2. **There is no `allowed` decision.** It isn't merely unused — the constant
   was deleted from the Go enum (§3.1), so offering it as a filter is not
   just always-empty, it's offering a value the server has never heard of.
3. **SSE rows have `id: 0`.** Don't key, dedupe or correlate on it.
4. **Stats lag the query log by up to 60s**, and by up to ~120s of wall clock
   before a 30s-polling dashboard reflects them. A fresh install legitimately
   shows zeros next to a filling query table.
5. **`hours=1` is not "the last 60 minutes"** — it's the current hour bucket.
6. **`/stats/top` takes `n`, not `limit`.** `limit` is silently ignored.
7. **`hours` is never validated.** Garbage silently means 24.
8. **A freshly added list reads `entry_count: 0`** until the detached refresh
   finishes. Read `last_status`, not the numbers: `pending` is "still
   loading", `failed`/`empty` is "blocking nothing". `0` + `last_refreshed: 0`
   no longer distinguishes them on its own.
9. **`POST /filters/refresh` never reports failure** to the client — but the
   *outcome* is now persisted per list, so poll `GET /filters/lists` and read
   `last_status`/`last_error` instead of guessing from `entry_count`.
10. **IPv6 zone-id client matchers are accepted but can never match.**
11. **`PUT /groups/{id}/lists` can leave a partial set** on failure.
12. **Query-log domain search is case-sensitive on Postgres**, insensitive on
    SQLite.
13. **The token id in a create response is unreliable** — refetch instead.
14. **A `stale` list is still enforcing.** Don't treat any non-`ok` status as
    "broken": `stale` has real entries and `last_refreshed` points at the copy
    serving them, not at the failed attempt.
15. **A list's `name` is derived unless the admin set one**, so it is not a
    stable identifier — key on `id`, and keep the `url` visible next to it.
16. **A zone record's `name` is relative but its `rdata` is absolute.** `name`
    is resolved against the zone apex (§3.7), while `rdata` is parsed by
    `dns.NewRR` under *no* `$ORIGIN` (`zones.ToRR`), so a dotless CNAME/MX/NS/
    PTR/SRV target is already fully qualified — `nas` is the name `nas.`, not
    `nas.<zone>`. Both spellings are valid records, so this is not something to
    validate; it is something to *say*. Cloudflare and Route 53 resolve the
    dotless form against the zone, so that is the habit users arrive with, and
    a silently-wrong record is the result. The zone detail row (`/zones/{id}`)
    labels both columns for this reason: the apex sits in a chip beside Name,
    and the name-valued types get a `FULL NAME` chip beside Data. **A bare `@`
    is the exception**: `BuildRecord` resolves it to the zone's apex before
    parsing, the same as the import path does, so pointing an MX at the apex
    does not mean spelling the zone name out.
17. **The allow-transfer band shows four states**, not one. Shown on
    `/zones/{id}` for `primary` and `secondary` zones only —
    `internal`/`stub`/`forwarder` refuse every transfer outright and get no
    editable ACL — and the server backs that up rather than leaving it to
    the UI: `allow_transfer` on a `stub` or a `forwarder` is a `400`, and
    any write to an `internal` zone is a `409`. A blank `allow_transfer` renders an explicit **"No peer
    may transfer this zone."** line rather than leaving the field looking
    merely unset; independent of that, the *last inbound transfer
    request* — whatever the ACL says now — is one of **"Never asked
    for."** (`last_xfr_at` is `0`), **"Last served `<time>` to `<peer>`"**
    (`last_xfr_error` is `""`), or **"Refused `<peer>` — `<reason>`"**
    (`last_xfr_error` is set). **`last_xfr_peer` is not proof of who
    asked**: a UDP AXFR is answered `NOTIMP` only after the zone has
    already resolved, so that refusal — spoofable UDP source address
    included — is what gets recorded (see [`docs/api.md`](api.md)'s
    `allow_transfer` field).
18. **The Notify out row shows a roll-up, not one row per target.** Shown on
    `/zones/{id}` for `primary` and `secondary` zones only — the same
    positive set the allow-transfer band above is gated on, and for the same
    reason (`notify_to` 400s on any other type). A blank `notify_to` renders
    an explicit **"No targets are notified."** line. When there are targets,
    the saved value is followed by a roll-up computed from
    `GET /zones/{id}/notifies` (`notifyRollup`, `web/src/lib/notify.ts`):
    **"no targets"**, **"all N current"**, **"X of N current, Y never
    notified"**, or **"M of N behind"** — `behind` (a target whose state is
    `retrying` or `gave_up`) takes precedence over the other wordings
    whenever any target is behind. Decided 2026-09-02 by the design's
    author, overriding the artboard's own arithmetic, which conflated
    "behind" and "never notified". A behind target earns a row of its own
    without opening the disclosure; the rest collapse behind a "+N current"
    / "+N never notified" hint.

    The four states a target's own row can be in, and the exact word shown
    for each (`notifyStateLabel`, `web/src/pages/zones/detail.tsx`) — **none
    of the four requires operator action**, since a NOTIFY only ever changes
    *when* a secondary refreshes, never *whether* it does, and the target's
    own SOA refresh schedule is the correctness backstop regardless of how a
    round ends:
    - `never` → **"never notified"** — no round has ever been tried.
    - `current` → **"current"** — the target has acknowledged the zone's
      present serial.
    - `retrying` → **"retrying · try X/Y"** — behind, and still within the
      round's attempt budget.
    - `gave_up` → **"not acknowledged"**, never rendered *"gave up"* — the
      round rested after exhausting its attempt budget; it resolves itself,
      with no operator action, on the zone's next edit. Drawn amber-on-hollow
      (a ring, no fill) rather than `retrying`'s filled dot, so it reads as
      "resting" rather than as a failure.
19. **A `forwarder` or `stub` zone whose upstreams are unreachable makes its
    whole suffix stop resolving.** It does not fall back to the `upstreams`
    setting: `Forwarder.pick` returns the matched suffix's list and never
    `f.def`, so an empty or entirely-failing list ends in SERVFAIL — with one
    caveat that matters when a screen is deciding what to promise. The purge
    that installing the routing table performs (§3.8) covers entries the
    *defaults* produced, so a claimed suffix can never be answered by the
    public internet. It says nothing about the suffix's own earlier answers:
    an **entirely-failing** list still serves those stale, NOERROR at TTL 30,
    for as long as `cache.serve_stale_for` allows. An **empty** list has
    never produced any, so that one really is SERVFAIL outright. This is
    the single most surprising behaviour of the two types and the reason the
    forwarder page carries a line about it (`ForwarderConsequence`,
    `web/src/pages/zones/detail.tsx`) — the screen states the fact,
    [`docs/architecture.md`](architecture.md) carries the why. The empty
    `forward_to` is not a validation gap: it is accepted deliberately, and
    only *disabling* the zone releases the suffix.
20. **A stub does not expire — but do not detect one by `expires_at === 0`.**
    A stub is never *given* an expiry (`StubFetcher.install` never writes the
    column, and `Zone.Serving`'s check is `secondary`-only), yet a row
    **retyped from `secondary` to `stub` keeps its old non-zero stamp**:
    `handleZonePatch` does not clear the column, so the field is a stale
    leftover on that row rather than an expiry anything honours. Branch on
    `type`, never on the stamp. Do not render a stub's staleness the way a
    secondary's is rendered either: a secondary
    past `expires_at` stops answering, while a stub goes on routing to the
    last NS set it fetched however old that is (`Zone.Serving` is
    `secondary`-only, and `StubFetcher.install` never writes the column).
    `transferState` is likewise not applied to a stub — its row reads
    `refreshed_at` and `last_error` alone (`web/src/lib/zones.ts`). The zone
    list's Status cell says only **Enabled**/**Disabled** for both new types;
    a stub's fetch state lives on the zone page.
21. **A stub's records are read-only for a stronger reason than a
    secondary's.** A hand-written record in a secondary is served
    authoritatively until the next transfer deletes it. A stub answers from
    none of its records — but `StubUpstreams` reads them back to rebuild the
    routing table on every zone reload, so a hand-written apex NS record
    redirects the whole claimed suffix until the next fetch undoes it. Both
    are `409` at the API (see §2.6); the UI hides the controls as well.

---

## 10. Code vs. documentation discrepancies

These are places where the shipped `openapi.yaml`, or the UI, disagrees with the
Go source. **The code is the source of truth.**

1. ~~Unique-constraint violations return 503 `storage unavailable`, not 409.~~
   **Fixed.** `storeErr` mapped only `ErrNotFound` → 404 and `ErrInUse` → 409,
   so every uniqueness violation fell to the default 503 branch — telling a
   user who reused a group name that storage was broken. There is now a
   `store.ErrDuplicate` sentinel, raised from the driver's typed error
   (sqlite result codes `2067`/`1555`, postgres SQLSTATE `23505`) at the two
   chokepoints every write passes through, and mapped to **409** with a
   message naming the collision.
2. ~~The query-log decision filter advertises `allowed`.~~ **Fixed** — the
   placeholder no longer offers a value the resolver never writes, and as of
   the zones work the `allowed` enum member is gone from the Go source too
   (`internal/dnssrv/pipeline.go`), not merely unused; see §3.1.
3. ~~A failed filter-list fetch is invisible: the UI shows `0 entries /
   never refreshed`, exactly what an unrefreshed list looks like.~~
   **Fixed.** `internal/filter/refresh.go` fell back to the on-disk cache on
   every failure and, when no cache existed, logged `"list cache not
   accessible"` and moved on — persisting nothing, so there was nothing to
   render. Lists now carry `last_status`/`last_error`/`last_attempt` (§3.2),
   the refresher records the real reason (HTTP status, transport cause, parse
   failure, or a parsed-but-empty file with its skipped-line count), and the
   Lists tab renders four distinct states plus a page-level banner for the
   ones enforcing nothing.
4. ~~`*.domain` wildcard lists silently parse to zero entries.~~ **Fixed.**
   `validDomain`'s charset rejected the `*` label, so every line of a 4.5 MB
   hagezi `wildcard/*` file was skipped and the list blocked nothing while
   reporting a healthy "refreshed just now". A leading `*.` is now stripped
   and stored as a normal entry (§3.2).
5. ~~`openapi.yaml` does not document `name`, `last_status`, `last_error` or
   `last_attempt` on the filter-list schema, nor the extended `PATCH
   /filters/lists/{id}` body.~~ **Fixed.** The list shape now lives once in
   `components/schemas/List` and is `$ref`'d from both `GET /filters/lists`
   and `GET /groups/{id}/lists`, which previously carried separate inline
   copies that had already drifted apart.
6. **Mostly fixed; the residue is named rather than estimated.** The 503
   `storage unavailable` response is documented on 45 of `openapi.yaml`'s 59
   operations, `GET`/`POST /setup` included — this entry previously said it
   was omitted from `POST /setup` and from "most operations", and both were
   wrong. Eleven operations that touch storage still omit it:
   `POST /auth/logout`, `GET /auth/me`, the three `POST /auth/totp/*`,
   `POST`/`DELETE /blocking/pause`, `POST /filters/refresh`,
   `GET /queries/tail`, `POST /tokens`, `DELETE /tokens/{id}`. (`GET /health`
   and `GET /openapi.yaml` also omit it and correctly: neither reads
   storage.) The **409-on-duplicate** half of this entry is fully resolved —
   every one of the nine `storeErrDup` endpoints documents it — and is
   withdrawn rather than narrowed.
7. ~~`openapi.yaml` marks `group_id` **required** on `DELETE
   /blocking/pause`; the code makes it optional, defaulting to 0 (the global
   pause).~~ **Fixed in the spec**, which is the half that was wrong: the
   parameter is `required: false` with `default: 0` and says why.
8. ~~`openapi.yaml` marks `group_id` required on `POST /blocking/pause`;
   only `minutes` is actually validated.~~ **Fixed in the spec**, the half
   that was wrong: `minutes` is the only required property, and `group_id`
   and `client_id` are optional with `default: 0`.
9. **Partly fixed.** The 1 MiB cap is documented on
   `POST /zones/{id}/file`, where it carries its own **413** — but as that
   endpoint's rule rather than as the global one it is (`decode`'s
   `io.LimitReader(r.Body, 1<<20)` bounds *every* JSON body; only the import
   turns exceeding it into a 413 instead of a decode failure).
   `openapi.yaml` still documents neither `DisallowUnknownFields` — so a
   client sending an unknown key gets `invalid json` with nothing in the
   spec to explain why — nor that a method mismatch yields 404 rather than
   405. The decode failure is at least one string everywhere now (§1)
   rather than a different one per endpoint.
10. `openapi.yaml` describes the SSE stream without noting the absent
    `event:`/`id:` fields, the absent heartbeat, or the 64-entry
    drop-on-slow-consumer behaviour.
11. `openapi.yaml:6` calls the project a "DNS/DHCP server". No DHCP exists.
12. The spec's "session expired" message on a mid-session 401 is **not
    implemented** — no such string exists in the client.
13. The spec's "keeps retrying" behaviour for the API-unreachable banner is
    **not implemented**: recovery requires the manual Retry button.
14. **Was backwards until Milestone D3's review, and widened twice since.**
    `openapi.yaml` is the correct one here: its `POST /zones` and
    `PATCH /zones/{id}` request bodies now give `type` an
    `enum: [primary, secondary, forwarder, stub]` and say `internal` alone
    is rejected with 400, which is what the code does. The five-value list
    it also carries belongs to the *Zone schema*'s `type` — the set a stored
    zone may have, not the set an API caller may send. This document's §2.6
    and §3.6 claimed `primary` alone was creatable, then `primary` and
    `secondary` alone, and quoted error strings (`only primary zones are
    supported`, `only primary and secondary zones are supported`) the code
    stopped emitting in D2 and D6 respectively; all of it is corrected.
15. `openapi.yaml`'s 404 responses on the `/zones/{id}/records*` routes
    describe the situation ("zone not found", "rid is not a record of this
    zone") rather than the response body, which is always the flat
    `{"error":"not found"}` used everywhere else 404 is returned. See §2.6.
16. **This document's own line-number citations rot, and eleven are
    unverified.** §1's and §3.4's pointed about a hundred lines off by D6 and
    are now function names instead; the remaining `file.go:N` citations —
    §1's store/search/token notes, §2.2's TOTP cite, §2.3, §2.5, §2.9,
    §3.1, §3.4's registry note, §2.7's two `qlog.go` cites — were not
    re-checked in that pass and should be converted the same way when each
    section is next touched. A line number is a claim about a file's history; a function name
    is a claim about its behaviour, which is what this document is for.
17. ~~`openapi.yaml`'s `DELETE /tsig-keys/{id}` describes the 409 as "while
    any zone's `tsig_key_id` names this key", narrower than the `DELETE`
    statement, which also refuses on a matching `key:` entry in
    `allow_transfer` or `notify_to`.~~ **Fixed in the spec**, which now
    states the full rule and carries `PUT`'s rename guard beside it.
