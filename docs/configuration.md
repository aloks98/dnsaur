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
| `upstreams` | `1.1.1.1:53,1.0.0.1:53,9.9.9.9:53` | Comma-separated upstream resolvers. Each entry is plain (`host:port`; bare IPv6 and missing ports are normalized) or, with a scheme, DNS-over-TLS (`tls://`) or DNS-over-HTTPS (`https://`) — see Encrypted upstreams below for the grammar. These are the **default** route — where a name no zone claims is sent. A suffix claimed by a `forwarder` or `stub` zone goes to that zone's upstreams instead, and never falls back to these; see Conditional forwarding below |
| `upstream.strategy` | `race` | Upstream selection strategy: `race` (query all healthy upstreams in parallel, first good answer wins), `failover` (try them in configured order, fall through on error/SERVFAIL), or `fastest` (try them ordered by measured EWMA latency, fastest first). One setting for the whole server: it applies to a conditional route's upstreams exactly as it applies to the defaults, and there is no per-zone strategy |
| `blocking.mode` | `null-ip` | How blocked queries are answered: `null-ip` (`0.0.0.0`) or `nxdomain` |
| `blocking.ttl` | `30` | TTL (seconds) returned on blocked responses |
| `cache.min_ttl` **†** | `0` | Minimum TTL (seconds) enforced on cached responses |
| `cache.max_ttl` **†** | `86400` | Maximum TTL (seconds) clamp on cached responses |
| `cache.max_entries` **†** | `10000` | Maximum number of entries held in the in-memory cache |
| `cache.serve_stale_for` **†** | `86400` | How long (seconds) a stale cache entry may still be served if upstream is down. It never applies across a routing change: adding, removing or retargeting a `forwarder` or `stub` zone drops what the cache held beneath that suffix, so a claimed suffix cannot be served an answer the default upstreams produced |
| `lists.refresh_hours` **†** | `24` | How often blocklists/allowlists are re-downloaded and recompiled |
| `qlog.retention_days` | `90` | How long query log rows are kept before the pruner deletes them |
| `qlog.privacy` | `full` | Query log privacy mode: `full`, anonymized client IPs, or `none` (no per-query logging) |

**†** — Restart-required exception. `cache.*` sizing/TTL settings and
`lists.refresh_hours` are read once at startup (the cache and the
background refresh ticker are sized/scheduled then); a running instance
must be restarted to pick up changes to these keys. Everything else in the
table — blocking mode/TTL, upstreams, upstream strategy, clients, groups,
lists, rules, zones and their records, and query-log privacy — applies
live, no restart needed: settings keys through the change-notification
channel, and clients, filters and zones through a reload the write handler
triggers directly. This is a documented Phase 1 limitation, expected to be
revisited in a later phase.

Two internal key prefixes (`instance.*` and future `stats.*` bookkeeping)
are not meant to be user-edited and are excluded from the settings API
(`GET /api/v1/settings`).

## Encrypted upstreams

An `upstreams` entry may add a scheme and, for the encrypted schemes, a
`#name` suffix — the convention Unbound
(`forward-addr: 1.1.1.1@853#cloudflare-dns.com`) and systemd-resolved
(`DNS=1.1.1.1#cloudflare-dns.com`) already use:

```
1.1.1.1:53                                     plain (unchanged)
udp://1.1.1.1:53                               plain, scheme written explicitly
tls://1.1.1.1:853#cloudflare-dns.com           DoT, port defaults to 853
https://1.1.1.1/dns-query#cloudflare-dns.com   DoH, port 443, path defaults to /dns-query
```

The parser (`internal/upstream/addr.go`, mirrored for the dashboard by
`web/src/lib/upstreams.ts` against the shared fixture
`internal/upstream/testdata/grammar.json`) rejects, each with a reason: a
`tls://` or `https://` host that isn't an IP literal; `tls://` or `https://`
without `#name`; `#name` on a plain entry; and a list that mixes schemes.
This rejection now happens at save time — `PUT /api/v1/settings` returns
400 with the reason — where it was previously accepted and silently
dropped the next time dnsaur restarted.

**Why an address, not a hostname.** Reaching `cloudflare-dns.com` requires
a DNS lookup, and dnsaur *is* the DNS — on boot it would need an upstream
to find its upstream. Separately, TLS certificates are issued to names,
not addresses, so dialing `1.1.1.1` and checking the certificate against
`1.1.1.1` fails even when it is genuinely Cloudflare. Hence two values: the
address to dial, and the name the certificate must present. Unbound,
systemd-resolved, Stubby and Knot all ask for the address the same way.
The full analysis (including the alternative of a bootstrap resolver, and
why dnsaur doesn't take it) is in
[the design spec](superpowers/specs/2026-09-05-encrypted-upstreams-design.md).

**What encryption buys, and what it does not.** Once an upstream is
`tls://` or `https://`, the path to it — the ISP first among them — stops
seeing which names this network looks up. **The resolver you chose still
sees every one of them.** Encryption moves trust from "everyone on the
path" to "the operator you picked"; it does not remove it. Removing that
too means dnsaur doing its own recursion instead of forwarding, which is a
deliberately later release (task #68).

**Presets.** The dashboard's upstreams editor offers these providers for
both transports; each round-trips through the same parser as a hand-typed
value:

| Provider | DoT | DoH |
|---|---|---|
| Cloudflare | `tls://1.1.1.1:853#cloudflare-dns.com`, `tls://1.0.0.1:853#cloudflare-dns.com` | `https://1.1.1.1:443/dns-query#cloudflare-dns.com` |
| Quad9 | `tls://9.9.9.9:853#dns.quad9.net`, `tls://149.112.112.112:853#dns.quad9.net` | `https://9.9.9.9:443/dns-query#dns.quad9.net` |
| Google | `tls://8.8.8.8:853#dns.google`, `tls://8.8.4.4:853#dns.google` | `https://8.8.8.8:443/dns-query#dns.google` |

**All entries must share one scheme.** Under the `race` strategy every
upstream is queried simultaneously, so one plaintext entry leaks every
name regardless of what the encrypted entries in the same list are doing.
Under `fastest`, plaintext always wins the race because it skips the TLS
handshake. A mixed list does not degrade to partial privacy — under two of
the three strategies it gives none, while still looking configured.

**If the stored value ever stops parsing, encryption is switched off, and
the dashboard says so.** `PUT /settings` refuses a value the grammar
rejects, so this is not reachable by editing the setting through the
dashboard or the API — it takes a hand-edited database row, a partially
written one, or a future tightening of the grammar across an upgrade. When
it happens, dnsaur falls back through its startup ladder and the last rung
is the hardcoded **plaintext** defaults, so every query goes out in the
clear. It keeps resolving on purpose — a resolver that stops entirely is
worse — but the settings screen carries a persistent warning ("Encryption
is off …") with the parse failure beneath it until the value is fixed, and
`GET /resolver/status` reports the same fact for anything scripting against
the API. The warning clears as soon as a saved value builds a forwarder.

**This is the global setting only.** It governs the default route's
transport. A `forwarder` zone's `forward_to` (see
[Conditional forwarding](#conditional-forwarding) below) stays plaintext
`host:port` — it names internal resolvers on trusted networks, not public
ones reached over the open internet, so there is nothing here for TLS to
protect.

## Conditional forwarding

**There is no conditional-forwarding setting.** No `upstreams.conditional`
key, no per-suffix entry in the table above, nothing to add one to — the
settings API accepts exactly the keys listed there. A suffix is claimed by a
**zone row**, and that is the only place it can be claimed, so there are
never two configurations of the same suffix that can disagree.

To send `corp.example` to a VPN resolver, `POST /api/v1/zones` a zone of
type `forwarder` whose `name` is the suffix and whose `forward_to` names the
resolvers — or a `stub`, which claims the suffix the same way but fetches
its nameservers from a master instead of being told them. See
[`docs/api.md`](api.md) for the fields each type takes,
[`docs/dashboard.md`](dashboard.md#forwarder-zones) for doing it from the UI,
and [`docs/architecture.md`](architecture.md) for how the routing table is
built and swapped.

The one thing to know before creating one: **a claimed suffix does not fall
back to `upstreams`.** If the zone's own upstreams are unreachable, or it
names none, queries beneath it get `SERVFAIL` rather than the public
internet's answer. That is deliberate — for a split-horizon zone the
fall-through *is* the leak — and
[`docs/architecture.md`](architecture.md) carries the reasoning. Disabling
the zone releases the suffix back to `upstreams`.

`upstream.Config.Conditional` in the code is populated from zone rows and
from nothing else. It read for two milestones as a settings key waiting to
be wired up, because §4 of
[the zones design spec](superpowers/specs/2026-08-08-zones-design.md)
described D6 as *migrating* one into a zone row; there was no key to
migrate, and that section now says so.

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
