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

There is still no web dashboard (see the status table in the
[`README`](../README.md)) — the API is the configuration surface today,
used via curl or any HTTP client.

## Conventions

- Base path: **`/api/v1`**.
- JSON in, JSON out (`application/json`), except `GET /queries/tail`
  (`text/event-stream`) and `GET /openapi.yaml` (`application/yaml`).
- Errors are always a flat envelope: `{"error": "<message>"}`, with a
  matching HTTP status code.
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

## Quick example: setup → login → create a record

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

# Use the session cookie to create a local DNS record
curl -sX POST "$BASE/records" \
  -H 'Content-Type: application/json' \
  -b cookies.txt \
  -d '{"name":"nas.home.arpa","type":"A","value":"192.168.1.10","ttl":300}'
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
- **Groups** — `GET /groups`, `POST /groups`, `PATCH /groups/{id}`
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
- **Records** — `GET /records`, `POST /records`, `PUT /records/{id}`,
  `DELETE /records/{id}` — local `A`/`AAAA`/`CNAME`/`TXT` records,
  wildcard names (`*.parent`) allowed, TTL 1–86400s.
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
