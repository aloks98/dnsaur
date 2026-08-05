# Configuration

See also: [`README.md`](../README.md) · [`docs/architecture.md`](architecture.md)

dnsaur has two layers of configuration:

- A small **bootstrap YAML file** (plus environment overrides) for the
  things needed before a database connection exists: listen addresses,
  storage driver/DSN, data directory, log level.
- **Everything else** lives in a `settings` table in the database, seeded
  with defaults on first run and managed at runtime via the REST API
  (`GET`/`PUT /api/v1/settings` — see [`docs/api.md`](api.md)). Most of
  these settings hot-reload with no restart.

## Bootstrap config (`dnsaur.yaml`)

Loaded from the path given by `-config` (default `dnsaur.yaml`), merged
over built-in defaults, then overridden by environment variables.

| YAML field | Env var | Default | Meaning |
|---|---|---|---|
| `dns_listen` | `DNSAUR_DNS_LISTEN` (comma-separated) | `[":53"]` | Addresses the DNS engine listens on (UDP+TCP) |
| `http_listen` | `DNSAUR_HTTP_LISTEN` | `:8080` | Address the REST API *and* the web dashboard listen on — both are served by the same HTTP server (the dashboard is a static SPA embedded into the binary; the API answers under `/api/v1`, everything else falls through to the dashboard, see [`docs/architecture.md`](architecture.md)) |
| `data_dir` | `DNSAUR_DATA_DIR` | `./data` | Directory for the SQLite DB file and cached blocklist downloads |
| `log_level` | `DNSAUR_LOG_LEVEL` | `info` | slog level (`debug`, `info`, `warn`, `error`) |
| `storage.driver` | `DNSAUR_STORAGE_DRIVER` | `sqlite` | `sqlite` or `postgres` |
| `storage.dsn` | `DNSAUR_STORAGE_DSN` | `<data_dir>/dnsaur.db` (sqlite) | Data source name; **required** when `storage.driver` is `postgres` |

Example `dnsaur.yaml`:

```yaml
dns_listen: [":53"]
http_listen: ":8080"
data_dir: "/var/lib/dnsaur"
log_level: "info"
storage:
  driver: sqlite
  dsn: ""   # defaults to <data_dir>/dnsaur.db
```

Env vars always win over the file, and the file always wins over built-in
defaults. All bootstrap fields have a `DNSAUR_*` override; there is no
env-only field that can't also be set in YAML.

## Database-managed settings

These are seeded into the `settings` table the first time dnsaur starts
against a fresh database. Changing them via `PUT /api/v1/settings` updates
the DB, bumps a config version, and live components reload automatically —
**except** the entries marked "restart required" below.

| Key | Default | Meaning |
|---|---|---|
| `instance.id` | random UUID, generated per install | Stable identifier for this instance (used by future HA sync) |
| `upstreams` | `1.1.1.1:53,1.0.0.1:53,9.9.9.9:53` | Comma-separated upstream resolver addresses (host:port; bare IPv6 and missing ports are normalized) |
| `upstream.strategy` | `race` | Upstream selection strategy: `race` (query all healthy upstreams in parallel, first good answer wins), `failover` (try them in configured order, fall through on error/SERVFAIL), or `fastest` (try them ordered by measured EWMA latency, fastest first) |
| `blocking.mode` | `null-ip` | How blocked queries are answered: `null-ip` (`0.0.0.0`) or `nxdomain` |
| `blocking.ttl` | `30` | TTL (seconds) returned on blocked responses |
| `cache.min_ttl` **†** | `0` | Minimum TTL (seconds) enforced on cached responses |
| `cache.max_ttl` **†** | `86400` | Maximum TTL (seconds) clamp on cached responses |
| `cache.max_entries` **†** | `10000` | Maximum number of entries held in the in-memory cache |
| `cache.serve_stale_for` **†** | `86400` | How long (seconds) a stale cache entry may still be served if upstream is down |
| `lists.refresh_hours` **†** | `24` | How often blocklists/allowlists are re-downloaded and recompiled |
| `qlog.retention_days` | `90` | How long query log rows are kept before the pruner deletes them |
| `qlog.privacy` | `full` | Query log privacy mode: `full`, anonymized client IPs, or `none` (no per-query logging) |

**†** — Restart-required exception. `cache.*` sizing/TTL settings and
`lists.refresh_hours` are read once at startup (the cache and the
background refresh ticker are sized/scheduled then); a running instance
must be restarted to pick up changes to these keys. Everything else in the
table — blocking mode/TTL, upstreams, upstream strategy, clients, groups,
lists, rules, local records, and query-log privacy — applies live via the
settings change-notification channel, no restart needed. This is a
documented Phase 1 limitation, expected to be revisited in a later phase.

Two internal key prefixes (`instance.*` and future `stats.*` bookkeeping)
are not meant to be user-edited and are excluded from the settings API
(`GET /api/v1/settings`).

## Blocklist formats

The filter engine (`internal/filter`) accepts subscribed list URLs in any
mix of these formats, auto-detected per line:

- **Hosts files** — `0.0.0.0 ads.example.com` or `127.0.0.1 ads.example.com`
  (also `::` / `::1`); the domain is extracted, the IP is ignored.
- **Plain domain lists** — one domain per line, e.g. `ads.example.com`.
- **AdGuard/ABP subset** — `||ads.example.com^` (block) and
  `@@||ads.example.com^` (allow). Only plain-domain ABP rules are
  supported; rules containing path or wildcard/regex syntax (`/`, `^` mid-
  pattern, `*`, `$`, `|`) are skipped, not partially matched.

`#` and `!` comment lines (and trailing `#` comments) are ignored. Lines
that don't parse as a valid domain are counted as skipped rather than
rejected outright, so one malformed line doesn't fail an entire list.
