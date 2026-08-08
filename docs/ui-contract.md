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
`Content-Type: application/json; charset=utf-8`
(`internal/api/server.go:126,131-133`). The strings quoted in this document are
verbatim; they are what a user will see.

**Auth** — a `dnsaur_session` cookie or `Authorization: Bearer <token>`. Bearer
wins if both are sent (`internal/api/server.go:168-171`).

| Cookie attribute | Value |
|---|---|
| Name | `dnsaur_session` |
| `Path` | `/` |
| `Max-Age` | `2592000` (30 days); `-1` on logout |
| `HttpOnly` | yes |
| `SameSite` | `Strict` |
| `Secure` | **only when Go itself terminated TLS** (`r.TLS != nil`, `internal/api/auth_handlers.go:105`). Behind a TLS-terminating reverse proxy the cookie ships without `Secure`. |

Sessions slide: past the halfway mark the expiry is pushed out another 30 days
(`internal/auth/service.go:128-130`).

**Scopes** — a `read` token is rejected on any method other than GET/HEAD with
**403** `read-only token` (`internal/api/server.go:182-184`). Enforcement is
purely method-based. Session cookies are always minted `write`, so a browser
session is never 403'd — the SPA has no 403 handling and doesn't need any.

**Request bodies** — decoded with `DisallowUnknownFields` and a 1 MiB cap
(`internal/api/server.go:146-154`). An unknown key, malformed JSON, an empty
body and a wrong-typed field are indistinguishable to the handler, so they all
produce that endpoint's single decode-failure string — which is often *not*
`invalid json`. See each endpoint.

**Status codes** — `201` for creates (no `Location` header, ever), `204` for
updates/deletes (empty body — refetch to observe state), `202` for exactly one
endpoint (`POST /filters/refresh`).

**No 405.** `/api/` is a registered catch-all, so a method mismatch on a real
path returns **404** `not found`, not 405, and no `Allow` header. `HEAD` works
on every `GET` route.

**Empty lists are `[]`, never `null`** — on every list endpoint, including a
brand-new instance (`internal/store/sql.go`, `search.go:54`, `tokenstore.go:42`).

**Numeric query params never error.** `qInt` discards parse errors
(`internal/api/queries_handlers.go:22-25`), so `limit=banana`, `hours=-5` and a
missing param are all `0`, which each endpoint then replaces with its default.

---

## 2. API endpoints

### 2.1 Meta

#### `GET /api/v1/health` — public

```json
{"status":"ok","version":"dev"}
```
`version` is the build version (`-X main.Version`); a source build reports `dev`.

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
| 503 | `storage unavailable` |

#### `POST /api/v1/auth/login` — public
Body: `username`, `password`, `totp_code` (only needed once TOTP is enabled).

**200** → `{"status":"ok"}` plus the session cookie.

| Status | Error string | When |
|---|---|---|
| 400 | `invalid json` | |
| 409 | `setup required` | no admin exists yet |
| 428 | `totp code required` | password correct, TOTP on, code **empty** |
| 401 | `bad credentials` | unknown user, wrong password, **or wrong TOTP code** |
| 503 | `storage unavailable` | |

> **The 428/401 split matters for the UI.** A *missing* code is 428; a *wrong*
> code is 401, identical to a wrong password
> (`internal/auth/service.go:96-101`). A client that treats 401 as
> "bad password" will throw the user back to step one on a typo'd 6-digit code.
> Unknown user and wrong password are also deliberately identical, and both burn
> the same argon2 cost, so login timing doesn't leak whether a username exists.

#### `POST /api/v1/auth/logout`
**204**, always, plus a cookie-clearing header. Behind `requireAuth`, so an
**already-dead session gets 401** — treat that as success, the session is gone
either way. A bearer-authenticated logout revokes nothing but still 204s.

#### `GET /api/v1/auth/me`
```json
{"id":1,"totp_enabled":false,"username":"admin"}
```
`totp_enabled` is computed as `TOTPSecret != ""`. The hash and secret are
`json:"-"` and never serialized.

#### TOTP
| Endpoint | Body | Success | Errors |
|---|---|---|---|
| `POST /auth/totp/start` | — | **200** `{"secret","otpauth_url"}` | 500 `totp generation failed` |
| `POST /auth/totp/confirm` | `{"secret","code"}` | **204** | 400 `invalid json`, 400 `invalid totp code` |
| `POST /auth/totp/disable` | `{"code"}` | **204** | 400 `invalid json`, 400 `invalid totp code`, 400 `bad credentials` |

`start` is **stateless** — nothing is persisted, the client holds the secret and
passes it back to `confirm`. Enabling TOTP does **not** invalidate existing
sessions.

---

### 2.3 Settings

#### `GET /api/v1/settings`
A flat object; **every value is a string**, including numbers. Keys prefixed
`instance.` or `stats.` are stripped, which hides `instance.id` and
`stats.watermark`.

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
  "upstream.strategy": "race",
  "upstreams": "1.1.1.1:53,1.0.0.1:53,9.9.9.9:53"
}
```

#### `PUT /api/v1/settings`
**One key per request**: `{"key": "...", "value": "..."}` — `value` must be a
JSON *string* even for numeric settings. **204** on success.

| Status | Error string |
|---|---|
| 400 | `invalid json` |
| 400 | `setting not editable: <key>` |
| 400 | `invalid value for <key>` |
| 503 | `storage unavailable` |

Full key list, defaults and reload behaviour in §3.8.

#### Blocking pause
| Endpoint | Params | Success |
|---|---|---|
| `GET /blocking` | `group_id` int, optional, **default 0** | **200** `{"paused_until": <unix ms>}`, `0` when not paused |
| `POST /blocking/pause` | body `{"group_id": int, "minutes": int}` | **204** |
| `DELETE /blocking/pause` | `group_id` query, optional, default 0 | **204**, unconditionally |

`minutes` must be **1–1440**, else 400 `minutes must be 1-1440`. `group_id` is
**not validated** — `0` means the global pause across every group. A
non-numeric `group_id` silently becomes `0`, so a typo clears the *global*
pause. `DELETE` 204s even for a group that was never paused or doesn't exist.

**Pause state is in-memory and lost on restart** (`internal/filter/engine.go:24`).

---

### 2.4 Groups and clients

Path ids must parse as int64 **and be > 0**, else 400 `bad id`. So `0`, `-1`,
`abc` and `1.5` all fail.

| Endpoint | Success | Notes |
|---|---|---|
| `GET /groups` | 200 array | ordered by id |
| `POST /groups` | 201 `{"id":2}` | body `{"name"[, "enabled"][, "list_ids"]}`; `name` required non-empty. `enabled` omitted = true. `list_ids` omitted = every existing list; `[]` = none |
| `PATCH /groups/{id}` | 204 | body `{"name"?, "enabled"?}` — both optional pointers; `{}` is a legal no-op |
| `DELETE /groups/{id}` | 204 | cascades the group's `group_lists` and `rules` |
| `GET /clients` | 200 array | |
| `POST /clients` | 201 `{"id":1}` | |
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
| 400 | `name required` | groups: also the decode-failure message |
| 400 | `name cannot be empty` | PATCH with `"name": ""` |
| 400 | `matcher must be an IP or CIDR and group_id set` | clients: **one string covers decode failure, bad matcher and bad group_id** |
| 409 | `resource in use` | deleting group id 1, or a group with clients attached |
| 409 | `a group with that name already exists` | duplicate name on create **or** rename |
| 409 | `another client already matches <matcher>` | duplicate client matcher on create or update |
| 404 | `not found` | |
| 503 | `storage unavailable` | genuine storage failures only |

---

### 2.5 Filters

| Endpoint | Success | Notes |
|---|---|---|
| `GET /filters/lists` | 200 array | |
| `POST /filters/lists` | **201** `{"id":1}` | body `{"url","kind"}` + **optional `name`**; kicks off a background refresh of *every* list |
| `PATCH /filters/lists/{id}` | 204 | body `{"enabled": bool}` and/or `{"name": string}` — **at least one required**; `url`/`kind` are rejected |
| `DELETE /filters/lists/{id}` | 204 | |
| `GET /groups/{id}/lists` | 200 array | a **nonexistent group returns `[]` + 200**, not 404 |
| `PUT /groups/{id}/lists` | 204 | body `{"list_ids":[...]}`; `null`/omitted unassigns everything |
| `GET /groups/{id}/rules` | 200 array | `[]` for a nonexistent group |
| `POST /groups/{id}/rules` | 201 `{"id":1}` | group comes from the **path**, not the body |
| `DELETE /filters/rules/{id}` | 204 | note the asymmetry: created under `/groups/{id}/rules`, deleted under `/filters/rules/{id}` |
| `POST /filters/refresh` | **202** `{"status":"refreshing"}` | no body read |

```json
[{"id":1,
  "url":"https://raw.githubusercontent.com/StevenBlack/hosts/master/hosts",
  "name":"StevenBlack hosts",
  "kind":"block","enabled":true,
  "last_refreshed":1785946876638,"entry_count":99277,
  "last_status":"ok","last_error":"","last_attempt":1785946876638},
 {"id":2,
  "url":"https://raw.githubusercontent.com/hagezi/dns-blocklists/main/hosts/pro.txt",
  "name":"hagezi hosts/pro.txt",
  "kind":"block","enabled":true,
  "last_refreshed":0,"entry_count":0,
  "last_status":"failed","last_error":"404 Not Found","last_attempt":1785946876700}]
```
```json
[{"id":1,"group_id":1,"action":"block","pattern":"ads.example.com","is_regex":false}]
```

| Status | Error string |
|---|---|
| 409 | `that list URL is already subscribed` |
| 400 | `url must be http(s)` |
| 400 | `kind must be block or allow` |
| 400 | `enabled or name required` (also the decode-failure message for PATCH — including an attempt to PATCH `url` or `kind`, which `DisallowUnknownFields` rejects) |
| 400 | `name too long (max 120)` (create and PATCH) |
| 400 | `action allow\|block and pattern required` (also decode failure) |
| 400 | `regex pattern too long (max 512)` |
| 400 | `invalid regex: <Go's compile error, verbatim>` — e.g. `invalid regex: error parsing regexp: missing closing ]: ` + `` `[unclosed` `` |

**`POST /filters/refresh` is fire-and-forget.** The work runs detached; there is
**no progress endpoint, no job id, and errors never reach the client** — they
only hit the server log. Poll `GET /filters/lists` and read `last_status` /
`last_error`, which is where a refresh's outcome now lands (§3.2). `POST
/filters/lists` does the same background refresh but answers **201**, so a
freshly added list reads `entry_count: 0` with `last_status: "pending"` until
the fetch lands.

**`PUT /groups/{id}/lists` is not transactional** — it unassigns every current
list one-by-one, then assigns the requested ids
(`internal/api/filters_handlers.go:148-164`). A failure partway (duplicate id in
the array, or a nonexistent list id) returns 503 *after* the unassign loop has
already committed, leaving the group with a partial set.

---

### 2.6 Local DNS records

| Endpoint | Success |
|---|---|
| `GET /records` | 200 array |
| `POST /records` | 201 `{"id":1}` |
| `PUT /records/{id}` | 204 (full replace) |
| `DELETE /records/{id}` | 204 |

```json
[{"id":1,"name":"nas.home.arpa","type":"A","value":"192.168.1.10","ttl":300}]
```

All eight validation strings, verbatim:

| Error string |
|---|
| `invalid json` |
| `name must be a domain (wildcard *.parent allowed)` |
| `ttl must be 1-86400` |
| `value must be an IPv4 address` |
| `value must be an IPv6 address` |
| `cname target must be a domain` |
| `txt value required` |
| `type must be A, AAAA, CNAME or TXT` |

There is **no uniqueness constraint** on records — duplicates are accepted
silently and all matching rows are answered.

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

Real capture (`?limit=2`):
```json
[{"id":7,"at":1785946876523,
  "instance_id":"29cbba52-a6e1-4088-89a3-3d14e32dcfa5",
  "client_ip":"127.0.0.1","client_id":0,
  "q_name":"ads.example.com","q_type":"A","decision":"blocked",
  "rule_id":1,"list_id":0,"upstream":"","r_code":"NOERROR","duration_ms":0},
 {"id":6,"at":1785946876505,
  "instance_id":"29cbba52-a6e1-4088-89a3-3d14e32dcfa5",
  "client_ip":"127.0.0.1","client_id":0,
  "q_name":"nas.home.arpa","q_type":"A","decision":"local",
  "rule_id":0,"list_id":0,"upstream":"","r_code":"NOERROR","duration_ms":0}]
```

#### `GET /api/v1/queries/tail` — SSE

`200`, `Content-Type: text/event-stream`, `Cache-Control: no-cache`, headers
flushed immediately. **No params — no filtering is possible on the stream.**

Wire format is one line per event:

```
data: {"id":0,"at":1785946986964,"instance_id":"29cbba52-…","client_ip":"127.0.0.1",
       "client_id":0,"q_name":"sse-probe.example.com","q_type":"A",
       "decision":"forwarded","rule_id":0,"list_id":0,"upstream":"1.1.1.1:53",
       "r_code":"NOERROR","duration_ms":9}
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
- **No heartbeat.** There is no keepalive ping, so an idle stream sends zero
  bytes and any proxy with an idle timeout will drop it.

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
{"blocked":2,"cached":0,"clients":1,"forwarded":10,"total":14}
```
- `total` = the sum of **every** decision bucket, including `local` and `error`.
- `cached` = `cached` + `stale`.
- `clients` = distinct `client_ip` keys in the window — **not** a count of
  configured client rows.

**`blocked + cached + forwarded ≤ total`**, and the gap is `local` + `error`.
Any UI computing "allowed = total − blocked" will be wrong.

#### `GET /stats/timeline`
Ascending by bucket; `bucket` is a **unix-seconds hour start**. Only non-zero
decisions appear — missing keys default to 0. **No zero-filling**, so a quiet
hour is simply absent.
```json
[{"bucket":1785945600,"decisions":{"blocked":2,"forwarded":10,"local":2}}]
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
| `POST /tokens` | 201 `{"id","token"}` | plaintext returned **once** |
| `DELETE /tokens/{id}` | 204 | |

```json
[{"id":2,"user_id":1,"kind":"api","name":"grafana-scraper","scope":"read",
  "created_at":1785946863832,"expires_at":0,"last_used":0}]
```
`expires_at: 0` means **never expires** — every API token is non-expiring.
`token_hash` is `json:"-"` and never leaves the server.

Errors: 400 `name required` (also the decode-failure message), 400
`scope must be read or write`, 404 `not found` on delete — which also covers
another user's token *and* any underlying storage failure.

> The `id` in the create response is **not** the insert id. The handler
> re-lists the user's tokens and picks the highest id with a matching name
> (`internal/api/tokens_handlers.go:49-55`). Two tokens with the same name make
> it ambiguous, and a failed listing makes it `0`. Refetch `GET /tokens` rather
> than trusting it. Verified: creating a second `grafana-scraper` returned
> `{"id":5,...}` with no error.

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
| `upstream` | string | `host:port` | **`""` = never left the box** (blocked/local/cached/stale/error) |
| `r_code` | string | `NOERROR`, `NXDOMAIN`, `SERVFAIL`, `REFUSED`, … | |
| `duration_ms` | int64 | whole ms, truncated | sub-millisecond answers record `0` |

**`decision` enum** (`internal/dnssrv/pipeline.go:14-22`):

| Value | Meaning |
|---|---|
| `blocked` | a rule or list matched |
| `local` | answered from a local record |
| `cached` | fresh cache hit |
| `stale` | upstream failed, served an expired entry (TTL rewritten to 30) |
| `forwarded` | answered by an upstream |
| `error` | handler error, or RFC 9520 failure cache → SERVFAIL |
| `allowed` | **TODO — defined but never written.** No code path assigns it. An allow rule only *skips* blocking, so the row is logged with whatever the downstream stage produced. |

> Verified live: filtering by each value returns `blocked` 2, `forwarded` 10,
> `local` 2, and **`allowed` 0**. The query-log filter placeholder used to read
> `"blocked, allowed, cached…"`, advertising a value that can never match; it
> now reads `"blocked, forwarded, cached…"`.

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
wildcards** (hagezi's `wildcard/*` files); `#`/`!` comments are stripped. ABP
rules containing `/ ^ $ * |` after the prefix are still skipped. Domains are
lowercased, ≤253 chars, labels ≤63, charset `a-z 0-9 - _` — **no IDN/punycode
handling**. `kind=allow` uses the file's allow entries **plus** its block
entries, so a plain domain list works as an allowlist.

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
| `pattern` | string | non-empty |
| `is_regex` | bool | default false |

- **Regex rules**: ≤512 **bytes**, must compile with RE2, matched **unanchored**
  against the lowercased qname.
- **Literal rules**: **no length cap and no domain-shape validation at all.**
  Inserted into a label trie, so they match the domain *and every subdomain*, on
  whole-label boundaries.
- Evaluation order: literal allow → regex allow → literal block → regex block →
  allowlists → blocklists. First match wins.
- The matched pattern is computed per query and **discarded** — only `rule_id`
  and `list_id` reach the log, which is why the UI's "why?" drawer has to
  re-resolve them and can only say *"Matched rule #N, which isn't available
  right now"* if it was deleted.

**TODO** — no update endpoint; rules are create/delete only.

### 3.4 Client

| Field | Type | Notes |
|---|---|---|
| `id` | int64 | accepted in POST bodies but ignored |
| `name` | string | **not validated, may be empty** |
| `matcher` | string | exact IP **or** CIDR; **DB-unique** |
| `group_id` | int64 | must be `> 0`; **not checked against an existing group** — a bad id fails the FK and returns 503 |

Matching: exact-IP map first, then CIDR list **longest-prefix-first**.
IPv4-mapped IPv6 is unmapped on both sides, so `::ffff:192.0.2.1` matches the
plain IPv4 client. No match → `group_id 1`, hardcoded
(`internal/clients/registry.go:74`).

> **IPv6 zone ids are accepted but can never match.** `fe80::1%eth0` passes
> validation and stores fine (verified: 201), but the request-side address is
> built with `netip.AddrFromSlice`, which never carries a zone
> (`internal/dnssrv/server.go:91-93`). A zoned matcher is therefore dead
> config. A zoned *CIDR* is correctly rejected at 400.

### 3.5 Group

| Field | Type | Notes |
|---|---|---|
| `id` | int64 | |
| `name` | string | required non-empty; **DB-unique** |
| `enabled` | bool | `false` means the group gets **no compiled ruleset at all**, so nothing is blocked for its clients |

The default group is created only when the table is empty, named `default`, and
on a fresh DB gets id **1**. Two places hardcode `1`: deletion refuses it, and
unmatched clients fall back to it.

### 3.6 Local DNS record

| Field | Type | Notes |
|---|---|---|
| `name` | string | trimmed, trailing dot stripped, **lowercased**; **no trailing dot is added**. Leading `*.` wildcard allowed. Must contain a `.`; no space, `/` or `\` |
| `type` | string | `A` \| `AAAA` \| `CNAME` \| `TXT`, uppercased before storage |
| `value` | string | per-type rules below |
| `ttl` | uint32 | **seconds, 1–86400 inclusive** — `0` fails, so it is effectively required |

- `A` — must parse as IP with `To4() != nil`. Stored verbatim, not canonicalised.
- `AAAA` — must parse as IP with `To4() == nil`, which **rejects IPv4-mapped
  forms** like `::ffff:192.0.2.1`. The web client deliberately does not
  replicate that narrowing and lets the server 400.
- `CNAME` — lowercased and dot-stripped **before storage**; must contain a `.`.
- `TXT` — any non-empty string. No length check and **no chunking at 255
  bytes**.

Resolution: exact name first, then `*.parent` walking up labels; CNAME chains
followed up to 8 hops, and a non-local target is resolved through the rest of
the pipeline and merged. Answers are marked authoritative.

### 3.7 Upstream

**There is no upstream table and no upstream entity.** Upstreams are a single
comma-separated settings string. Accepted forms: `1.1.1.1`, `1.1.1.1:53`,
`::1`, `[::1]:5353`, hostnames — a bare address gets `:53` appended, IPv6
literals bracketed first. Validation is only "non-empty after trimming"; a
genuinely unusable address is discovered at forwarder-build time.

**Only plain UDP with TCP fallback on truncation is implemented.** No DoT, DoH
or DoQ anywhere — **TODO**.

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

**Conditional / split-horizon forwarding is TODO** — `upstream.Config.Conditional`
exists and `New` implements it, but nothing ever populates it: no setting, no
table, no API.

### 3.8 Settings keys

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
| `lists.refresh_hours` | `24` | int ≥ 0 | **restart** for the cadence — but any settings change triggers one immediate refresh |
| `qlog.retention_days` | `90` | int ≥ 0 | **hot, delayed** — re-read per prune run, so it lands on the next 24h tick |
| `qlog.privacy` | `full` | `full` \| `anon` \| `none` | **hot**, read per query |

No upper bound on any integer key. `blocking.mode` treats **anything ≠
`nxdomain`** as null-ip.

Non-editable keys that exist but are stripped from `GET /settings`:
`instance.id`, `stats.watermark`.

### 3.9 Tokens

`kind` is `session` or `api` — **not** a DB or API-level enum, just the only two
values the code writes. Session tokens always have `scope: "write"`, `name: ""`,
and a 30-day expiry; API tokens have `expires_at: 0` (never). Token material is
32 random bytes, base64url for the plaintext, SHA-256 hex stored.

### 3.10 DHCP lease — **TODO, nothing exists**

There is no lease entity, no table, no route, no UI type, and no `:67` listener.
A case-insensitive search for `dhcp` across all Go, SQL, and TypeScript files
returns **zero implementation hits**. The only occurrences are prose: the README
feature table (`| DHCP | Planned |`), the architecture doc's "Phase 2 … not yet
present", and one line in `openapi.yaml:6` describing the project as a
"DNS/DHCP server" — which is aspirational; no DHCP path is defined in that
document either.

Anything a UI shows for DHCP today would be invented.

### 3.11 Never serialized

| Struct | Field |
|---|---|
| `store.User` | `PasswordHash`, `TOTPSecret` (`json:"-"`) |
| `store.AuthToken` | `TokenHash` (`json:"-"`) |

Computed and discarded, so the UI cannot have them: the matched
rule/list pattern, the parser's skipped-line count, and the query-log dropped
count. Also note **there is no join returning a client or group *name* alongside
a query row** — the UI must resolve `client_id` itself against `GET /clients`.

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

**`stats_hourly` is never pruned** — it grows unbounded, independent of
`qlog.retention_days`. **TODO.**

---

## 5. Query log: retention, buffering, page size

**Retention** — `qlog.retention_days`, default **90**, unit days. Pruning runs
**once at startup, then every 24 hours**, deleting `WHERE at < cutoff`. The value
is re-read from the DB on every prune, so a change lands on the next tick
without a restart. `0` is a valid value and means "delete everything".

**Write path is buffered**: channel capacity **10000**, batch size **1000**,
flush every **1 second**, 5s DB timeout. **Overflow drops the oldest entry and
never blocks.** A drop counter exists but **nothing exposes it** — the UI cannot
tell that entries were lost. Flush failures discard the batch with no retry.

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
| Lists | Refresh now | `POST /filters/refresh` | `Couldn't start a refresh — try again` |
| Rules | Delete | `DELETE /filters/rules/{id}` | `Couldn't delete the rule for ${target.pattern}` |
| Rules | Add | `POST /groups/{id}/rules` | server message, else `Couldn't add the rule` |
| Groups | Toggle enabled | `PATCH /groups/{id}` | `Couldn't ${enabled ? "enable" : "disable"} ${group.name}` |
| Groups | Rename | `PATCH /groups/{id}` | `Couldn't rename ${group.name}` |
| Groups | Delete | `DELETE /groups/{id}` | `Can't delete ${group.name} — it's still in use` (on 409) |
| Groups | Apply/remove a list | `PUT /groups/{id}/lists` | `Couldn't update lists for ${group.name}` |
| Clients | Edit / Add | `PUT|POST /clients` | server message, else `Couldn't ${update\|add} the client` |
| Clients | Delete | `DELETE /clients/{id}` | `Couldn't delete ${target.name}` |
| Local DNS | Edit / Add | `PUT|POST /records` | server message, else `Couldn't ${update\|add} the record` |
| Local DNS | Delete | `DELETE /records/{id}` | `Couldn't delete ${target.name}` |
| Account | Revoke token | `DELETE /tokens/{id}` | `Couldn't revoke ${token.name}` |
| Account | Create token | `POST /tokens` | server message, else `Couldn't create the token` |
| Account | Enable 2FA | `POST /auth/totp/start` | server message, else `Couldn't start setup — try again` |
| Account | Confirm / Disable 2FA | `POST /auth/totp/confirm\|disable` | **inline field error**, not a toast: server message, else `Invalid code — try again` |
| Shell | Pause blocking | `POST /blocking/pause` | `Couldn't pause blocking — try again` |
| Shell | Resume blocking | `DELETE /blocking/pause` | `Couldn't resume blocking — try again` |
| Shell | Log out | `POST /auth/logout` | `Couldn't sign out — try again` — **suppressed on 401**, which counts as logged out |
| Settings | Save changes (bulk) | one `PUT /settings` per changed key | partial: `Saved ${n}, but couldn't save ${keys} — try again` |

**Disabled states that actually occur:** dashboard quick actions while
`groups.isPending || groups.isError`; query-log Block/Allow while
`clients.isPending || clients.isError`; Add client when no groups exist
(`Create a group first`); delete on group id 1
(`The default group can't be deleted`); Save changes when the form isn't dirty.

Settings saves are per-key via `Promise.allSettled`, so a partial failure keeps
the failed fields dirty with the admin's typed value intact.

---

## 7. Loading / empty / error states

The client distinguishes two error cases, and the distinction is load-bearing:

- `isError && data === undefined` → a **destructive alert**, content replaced.
- `isError && data !== undefined` → a **stale-data banner above still-valid
  content** (`Couldn't refresh {what}` / "Showing what last loaded
  successfully." / `Try again`). A blipped background poll never wipes good
  data.

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
| Local DNS | 4 skeletons | `No local DNS records yet` | `Couldn't load local DNS records` | stale banner |
| Settings | layout-shaped skeleton | n/a (fixed 11 fields) | `Couldn't load settings` | stale banner, "Any edits below are untouched." |
| Account | 2-card skeleton | `No API tokens yet` | `Couldn't load your account` | stale banner |

**Live-tail mode has no loading state at all** — it renders the empty state
until the first SSE row arrives.

**Auth gate:**

| State | Renders |
|---|---|
| `me` pending | full-page spinner |
| `me` 401 + setup pending | full-page spinner |
| `me` 401 + setup **also failed** | `Can't reach dnsaur` banner with a manual `Retry` |
| `me` 401 + `setup_required: true` | Setup wizard |
| `me` 401 + `setup_required: false` | Login |
| `me` success | app shell |

A **mid-session 401** from any query revalidates `me` once (re-entrancy
guarded); if it now fails, all non-auth cached data is dropped and the gate
falls back to Login.

**Polling** — three things poll, all at 30s: the four dashboard stats queries
(overview, timeline, top blocked, top clients),
`GET /health` (the top bar's row-1 `DNS OK` readout, every screen), and `GET /blocking` (pause
control, every screen plus one per group row). Everything else is
fetch-on-mount with a 10s stale time and no window-focus refetch; `me` is the
exception, refetching on focus only once it has succeeded.

**SSE client behaviour** — backoff doubles 1s → 30s; after **6** consecutive
failures with no successful open it gives up and shows `Live tail disconnected`
with a manual Reconnect. Arrivals batch at 100ms; the ring buffer holds **500
rows**, newest first, and is never cleared when the stream stops.

**Keyboard** — exactly one shortcut: ⌘K / Ctrl+K toggles the command palette.
It is registered on `window` with no target filtering, so it also fires while
typing in an input.

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
| Local DNS | `/dns` |
| Settings | `/settings` |
| Account & security | `/account` |
| Login | pre-shell |
| Setup wizard | pre-shell |

### Half-built

| Screen | What's missing |
|---|---|
| **Filtering → Groups & Clients** | CRUD works, but the per-group "Lists (n)" menu has **no error state**: it's disabled only while `isPending`, not on `isError`, and its toggle rebuilds the assignment set from `groupLists.data ?? []`. If that read failed, clicking one list PUTs `[thatOne]` and **silently drops every other assignment**. |
| **Filtering → Lists** | The table leads with the list's `name`; the URL is a muted second line and stays in the row's `title`. Actions (toggle, rename, delete) are labelled by name. The **Status** column replaces the old "Last refreshed" one and carries the badge plus a plain-language line per `last_status`. |
| **Dashboard health** | Reduced to the shell's two row-1 readouts (blocking state, and `DNS OK`/`DNS down` from `GET /health`). Filter-list freshness moved off the dashboard with the redesign and now lives only on Filtering → Lists. The spec's "upstreams healthy" signal **has no code at all** — there is no upstream-health endpoint. |
| **Settings** | 11 keys work. The spec's "storage (read-only info)" section is absent, with a code comment noting no endpoint exists to source it. |
| **Account** | TOTP and tokens are complete. **Change password is not implemented**; the page says so: *"Password changes aren't available yet — that's planned for a future update."* |
| **Command palette** | Navigates to the 8 leaf pages only, grouped by nav section. The spec's "quick actions (pause, block a domain)" don't exist. |

### Not started

| Screen | Status |
|---|---|
| **404 / unknown route** | `path="*"` silently redirects to Dashboard; there is no not-found screen |
| **DHCP** | nothing exists (§3.10) |
| **Encrypted DNS (DoH/DoT)** | no code |
| **Authoritative zones / DNSSEC** | no code |
| **HA / cluster UI** | no code; the spec anticipated a health-strip stub, which does not exist |

The nav contains exactly the eight implemented routes, in four groups
(Monitor: Dashboard, Query Log · Filtering: Lists, Rules, Groups & Clients ·
Network: Local DNS · System: Settings, Account) — there are no dead nav
entries pointing at unbuilt screens. Theme and log out live under System too;
the shell has no sidebar and no avatar.

### Dashboard layout (`/`)

A full-bleed grid of hairline-separated bands, no cards, everything mono:

1. **Stat strip** — `QUERIES` / `BLOCKED` / `CACHE HIT RATE` / `ACTIVE CLIENTS`
   from `GET /stats/overview`. Both percentages divide by `total`, which sums
   *every* decision including `local` and `error` — so `blocked + cached +
   forwarded ≤ total`, and no "allowed" figure is ever derived by subtraction.
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
`cached` muted, `forwarded` foreground, `local` primary. There is no `allowed`
— the resolver never writes it.

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
   including `local` and `error`. Don't compute "allowed" by subtraction.
2. **`decision: "allowed"` never occurs.** Offering it as a filter yields an
   always-empty result.
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
   placeholder no longer offers a value the resolver never writes. The
   `allowed` enum member itself still exists unused in the Go source; see §3.1.
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
6. `openapi.yaml` omits **503 `storage unavailable`** on most operations that
   can return it, and omits it entirely from `POST /setup`. It still omits
   **409** on the duplicate responses other than `POST /filters/lists`, which
   is now documented.
7. `openapi.yaml` marks `group_id` **required** on `DELETE /blocking/pause`;
   the code makes it optional, defaulting to 0 (the global pause).
8. `openapi.yaml` marks `group_id` required on `POST /blocking/pause`; only
   `minutes` is actually validated.
9. `openapi.yaml` documents neither `DisallowUnknownFields` nor the 1 MiB body
   cap, and does not mention that method mismatches yield 404 rather than 405.
10. `openapi.yaml` describes the SSE stream without noting the absent
    `event:`/`id:` fields, the absent heartbeat, or the 64-entry
    drop-on-slow-consumer behaviour.
11. `openapi.yaml:6` calls the project a "DNS/DHCP server". No DHCP exists.
12. The spec's "session expired" message on a mid-session 401 is **not
    implemented** — no such string exists in the client.
13. The spec's "keeps retrying" behaviour for the API-unreachable banner is
    **not implemented**: recovery requires the manual Retry button.
