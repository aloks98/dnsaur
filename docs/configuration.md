# Configuration

See also: [`README.md`](../README.md) · [`docs/architecture.md`](architecture.md)

dnsaur has two layers of configuration:

- A small **bootstrap YAML file** (plus environment overrides) for the
  things needed before a database connection exists: listen addresses,
  storage driver/DSN, data directory, log level and format.
- **Everything else** lives in a `settings` table in the database, seeded
  with defaults on first run and managed at runtime via the REST API
  (`GET`/`PUT /api/v1/settings` — see [`docs/api.md`](api.md)). Most of
  these settings hot-reload with no restart.

## Bootstrap config (`dnsaur.yaml`)

Loaded from the path given by `-config` (default `dnsaur.yaml`), merged
over built-in defaults, then overridden by environment variables.

| YAML field | Env var | Default | Meaning |
|---|---|---|---|
| `dns_listen` | `DNSAUR_DNS_LISTEN` (comma-separated) | `[":53"]` | Addresses the DNS engine listens on (UDP+TCP). A list, or a plain string for a single address (`dns_listen: ":53"`); at least one address is required |
| `http_listen` | `DNSAUR_HTTP_LISTEN` | `:8080` | Address the REST API *and* the web dashboard listen on — both are served by the same HTTP server (the dashboard is a static SPA embedded into the binary; the API answers under `/api/v1`, everything else falls through to the dashboard, see [`docs/architecture.md`](architecture.md)) |
| `data_dir` | `DNSAUR_DATA_DIR` | `./data` | Directory for the SQLite DB file and cached blocklist downloads |
| `log_level` | `DNSAUR_LOG_LEVEL` | `info` | slog level: `debug`, `info`, `warn` or `error`. Anything else is refused at startup |
| `log_format` | `DNSAUR_LOG_FORMAT` | `json` | slog handler: `json` (machine-readable, what a log shipper wants) or `text` (`key=value` lines, what a human reading `journalctl` wants). Anything else is refused at startup |
| `storage.driver` | `DNSAUR_STORAGE_DRIVER` | `sqlite` | `sqlite` or `postgres` |
| `storage.dsn` | `DNSAUR_STORAGE_DSN` | `<data_dir>/dnsaur.db` (sqlite) | Data source name; **required** when `storage.driver` is `postgres` |
| `kea_socket` | `DNSAUR_KEA_SOCKET` | *(empty)* | Path of `kea-dhcp4`'s unix control socket. Empty — the default — means DHCP is off: no engine is talked to, no leases are polled, no DNS names come from leases, and every `/api/v1/dhcp/*` route but `GET /dhcp/status` answers `404 {"error": "dhcp is not enabled"}`. Bootstrap rather than a setting because dnsaur cannot move a socket the engine was started with, and because a box with no Kea on it has nothing to point at. dnsaur needs read *and write* on the socket — see DHCP below, where group membership alone turns out not to be enough |
| `trusted_proxies` | `DNSAUR_TRUSTED_PROXIES` (comma-separated) | *(empty)* | Networks a reverse proxy in front of dnsaur may connect from, as CIDRs (a bare address means that one host). A request arriving from one of them has its `X-Forwarded-Proto` and `X-Forwarded-For` believed; every other request does not. See Behind a reverse proxy below |

Example `dnsaur.yaml`:

```yaml
dns_listen: [":53"]
http_listen: ":8080"
data_dir: "/var/lib/dnsaur"
log_level: "info"
log_format: "json"   # or "text"
trusted_proxies: []   # e.g. ["10.0.0.0/8", "192.168.1.5"]
storage:
  driver: sqlite
  dsn: ""   # defaults to <data_dir>/dnsaur.db
```

Env vars always win over the file, and the file always wins over built-in
defaults. All bootstrap fields have a `DNSAUR_*` override; there is no
env-only field that can't also be set in YAML.

`DNSAUR_DNS_LISTEN` is split on commas, with surrounding whitespace trimmed
and empty entries dropped, so `":53, :5353"` and `":53,:5353,"` both mean
the same two addresses. A list written in YAML is normalised identically:
the same addresses written either way produce the same value.

### What startup refuses

Bootstrap config is checked before anything binds, and a value that cannot
be honoured stops the process with a message rather than being replaced by
a default — this is the one layer where "keep going" would mean running a
server that answers nothing (see
[`docs/architecture.md`](architecture.md#dns-must-not-die)):

- **A config file that isn't there**, when `-config` named it. Only the
  default path (`dnsaur.yaml`) is allowed to be absent, since running with
  no config file at all is a supported setup. `-config /etc/dnsaur/dnsuar.yaml`
  is a typo, and starting on defaults hid it behind a server that came up
  answering none of the configured names.
- **A config file that doesn't parse**, or that can't be read.
- **An empty `dns_listen`** — `[]`, `""`, or an env value that trims to
  nothing. There is no DNS server without an address to serve it on.
- **An unknown `log_level`.** `verbose` used to be silently `info`, so an
  operator who asked for debug output got none and nothing said why.
- **An unknown `log_format`**, for the same reason: `cmd/dnsaur` picks a
  handler from the value and has no way to refuse one, so anything but
  `text` or `json` would silently stay `json`.
- **An unknown `storage.driver`**, and a `postgres` driver with no
  `storage.dsn`.
- **A `trusted_proxies` entry that is not an IP address or CIDR.** Dropping
  it silently would leave the operator with a proxy they believe is trusted,
  a session cookie shipping without `Secure`, and nothing anywhere saying
  why.

### What startup logs

Once the config loads, dnsaur logs one `config` line per bootstrap key at
INFO, carrying the value in force and where it came from — `file`, `env` or
`default`:

```
{"level":"INFO","msg":"config","key":"http_listen","value":":8080","source":"default"}
{"level":"INFO","msg":"config","key":"data_dir","value":"/var/lib/dnsaur","source":"file"}
{"level":"INFO","msg":"config","key":"log_level","value":"debug","source":"env"}
```

The source is the half that matters. A setting that "isn't being applied" is
usually a file the process never read — a typo'd `-config`, a container
where the bind mount landed somewhere else, an env var left over from an
earlier run — and a value printed on its own can't distinguish one that was
set from one that defaulted.

The values are the ones actually in force, after normalising (`dns_listen`
trimmed and split) and defaulting (`storage.dsn` filled in from `data_dir`),
so the log shows what the server is running on rather than what was typed.
`storage.dsn` is printed with any password replaced by `xxxxx`; nothing else
in the bootstrap config is a credential.

### In a container

The image (`Dockerfile` at the repo root — see
[`docs/development.md`](development.md#container-image) for how to build it;
nothing is published yet) is configured through the same `DNSAUR_*`
variables and nothing else. Its entrypoint passes **no** `-config`
deliberately: the default path is the one `config.Load` tolerates being
absent, so an env-only container starts, while `WORKDIR /data` means a file
bind-mounted at `/data/dnsaur.yaml` is still picked up with no flag to
change. Naming a path in the entrypoint would make that file mandatory and
break the first case, per *What startup refuses* above.

`DNSAUR_DATA_DIR=/data` is baked in, and `/data` is a volume owned by the
image's non-root user (uid 65532), so the SQLite database and the blocklist
cache land there and survive a container replacement.

That non-root user is also why port 53 is not bound directly: Docker does
not grant `CAP_NET_BIND_SERVICE` to a non-root process, so dnsaur listens
high inside the container and the host publishes 53 onto it.

```sh
docker run -d --name dnsaur \
  -e DNSAUR_DNS_LISTEN=:5353 \
  -e DNSAUR_HTTP_LISTEN=:8080 \
  -e DNSAUR_LOG_LEVEL=info \
  -v dnsaur-data:/data \
  -p 53:5353/udp -p 53:5353/tcp \
  -p 8080:8080 \
  dnsaur:local
```

`--sysctl net.ipv4.ip_unprivileged_port_start=0` with
`DNSAUR_DNS_LISTEN=:53` is the alternative if the port must match inside and
out — needed for `--network host`, where there is no publishing to remap.

`storage.driver: postgres` works the same way:
`-e DNSAUR_STORAGE_DRIVER=postgres -e DNSAUR_STORAGE_DSN=postgres://…`.

`Dockerfile.kea` is the variant with the DHCP engine in the image, and it
has rules of its own — host networking, root, a bundled Kea config. See
*DHCP* below.

## Behind a reverse proxy

dnsaur speaks plain HTTP behind nginx, Caddy or Traefik, which is the usual
deployment. Two things it does depend on facts only the proxy knows — whether
the user's connection was HTTPS, and which client the request came from —
and both arrive as headers that any client could also send. So they are
believed from nothing except the addresses named in `trusted_proxies`:

```yaml
trusted_proxies:
  - 10.0.0.0/8
  - 192.168.1.5   # a bare address means that host alone
```

With that set, a request arriving from one of those networks has:

- **`X-Forwarded-Proto`** decide the session cookie's `Secure` attribute.
  Without it, the 30-day cookie set by `POST /api/v1/auth/login` ships
  without `Secure` on any install where Go did not terminate TLS itself, and
  a single plaintext request to the same host carries the session over the
  wire. Only the first entry is read, since a chain of proxies appends.
- **`X-Forwarded-For`** decide which client the login/setup throttle counts
  against (see [`docs/api.md`](api.md)). Without it every request behind the
  proxy shares one source address and one budget, so one attacker locks
  everybody out. The list is read right to left, skipping hops that are
  themselves trusted proxies.

Leave it empty — the default — when dnsaur is reachable directly. An empty
list trusts nothing, and a header that arrives anyway is ignored.

## Database-managed settings

These are seeded into the `settings` table the first time dnsaur starts
against a fresh database. Changing them via `PUT /api/v1/settings` updates
the DB, bumps a config version, and live components reload automatically —
**except** the entries marked "restart required" below.

The config version counts configuration changes, not settings writes: every
write to a group, client, filter list or its assignments, rule, TSIG key or
zone *definition* advances it too. It is what a replica polls
(`GET /api/v1/sync/version`) to decide whether the main's configuration
moved. Zone records, serial bumps and transfer bookkeeping do not advance
it — none of them travels in a config bundle.

| Key | Default | Meaning |
|---|---|---|
| `instance.id` | random, generated per install | Stable identifier for this instance. It is what `GET /api/v1/sync/version` reports, so a replica pointed at itself — a box whose database was copied from its main — is refused instead of quietly becoming its own replica, and it is the id a replica pairs under and the main's Sync band lists it by |
| `upstreams` | `1.1.1.1:53,1.0.0.1:53,9.9.9.9:53` | Comma-separated upstream resolvers. Each entry is plain (`host:port`; bare IPv6 and missing ports are normalized) or, with a scheme, DNS-over-TLS (`tls://`) or DNS-over-HTTPS (`https://`) — see Encrypted upstreams below for the grammar. These are the **default** route — where a name no zone claims is sent. A suffix claimed by a `forwarder` or `stub` zone goes to that zone's upstreams instead, and never falls back to these; see Conditional forwarding below |
| `upstream.strategy` | `race` | Upstream selection strategy: `race` (query all healthy upstreams in parallel, first good answer wins), `failover` (try them in configured order, fall through on error/SERVFAIL), or `fastest` (try them ordered by measured EWMA latency, fastest first). One setting for the whole server: it applies to a conditional route's upstreams exactly as it applies to the defaults, and there is no per-zone strategy |
| `blocking.mode` | `null-ip` | How blocked queries are answered: `null-ip` (`0.0.0.0`) or `nxdomain` |
| `blocking.ttl` | `30` | TTL (seconds) returned on blocked responses |
| `cache.min_ttl` **†** | `0` | Minimum TTL (seconds) enforced on cached responses |
| `cache.max_ttl` **†** | `86400` | Maximum TTL (seconds) clamp on cached responses |
| `cache.max_entries` **†** | `10000` | Maximum number of entries held in the in-memory cache |
| `cache.serve_stale_for` **†** | `86400` | How long (seconds) a stale cache entry may still be served if upstream is down. It never applies across a routing change: adding, removing or retargeting a `forwarder` or `stub` zone drops what the cache held beneath that suffix, so a claimed suffix cannot be served an answer the default upstreams produced |
| `lists.refresh_hours` **†** | `24` | How often blocklists/allowlists are re-downloaded and recompiled. **Minimum 1** — `0` is an interval no timer can be built from, and `PUT /settings` refuses it. A `0` already in the database (or one written before this check existed) turns the periodic refresh off with a warning in the log instead of taking the process down; lists still recompile on every settings change and on the manual refresh |
| `qlog.retention_days` | `90` | How long query log rows are kept before the pruner deletes them. Deleting happens in bounded chunks, so shortening retention on a large log does not lock the database for the length of one enormous statement |
| `stats.retention_days` | `365` | How long the hourly statistics behind the dashboard are kept. They are aggregates, not per-query rows, so a year of them is cheap and keeps a year-over-year comparison possible long after the query log itself has been pruned. **Minimum 1** — `0` would delete every bucket on the next prune |
| `qlog.privacy` | `full` | Query log privacy mode: `full`, anonymized client IPs, or `none` (no per-query logging) |
| `serve.dot.enabled` | `false` | Serve DNS-over-TLS (RFC 7858) to clients on `serve.dot.listen`. Enabling requires `serve.tls.cert`/`serve.tls.key` to already name a certificate that loads — see Encrypted serving below |
| `serve.dot.listen` | `:853` | Address the DoT listener binds — TCP only, since DoT has no datagram transport. `host:port`, host empty for all interfaces, port 1-65535 |
| `serve.doh.enabled` | `false` | Serve DNS-over-HTTPS (RFC 8484, `/dns-query`) to clients on `serve.doh.listen`. Same certificate requirement as `serve.dot.enabled` |
| `serve.doh.listen` | `:443` | Address the DoH listener binds — same `host:port` grammar as `serve.dot.listen` |
| `serve.tls.cert` | *(empty)* | Absolute path to the PEM certificate DoT and DoH both present. Empty means neither protocol can be enabled yet |
| `serve.tls.key` | *(empty)* | Absolute path to the PEM private key matching `serve.tls.cert` |
| `sync.peer_url` | *(empty)* | The main instance this one follows, as an absolute `http`/`https` URL, scheme and host only. Empty means this instance is a main and accepts writes; non-empty makes it a replica, which pulls the main's configuration and refuses local writes to anything that configuration covers. Pairing writes it; clearing it is the promotion. Writing it by hand requires `sync.token` in the same request or already stored |
| `sync.token` | *(empty)* | The secret this replica pulls with: 32 random bytes the main minted for this box alone when the two paired, good for the two sync reads and nothing else. Pairing writes it — it is never shown and never typed, and the only time an operator writes it by hand is clearing it to stop following. **Never returned by `GET /api/v1/settings`** — it is a credential, and the settings screen shows only whether one is set |
| `sync.interval_seconds` | `30` | How often a replica probes the main's config version. The main reads its own copy too: a replica it has not heard from for three intervals is shown as stale. **Minimum 5** — below that the probe costs the main more than the drift it removes |
| `dhcp.domain` | *(empty)* | The DNS suffix DHCP clients are given (option 15), and the one a lease's hostname is published under. A scope may override it; empty means leases get no names at all. Written without a trailing dot |
| `dhcp.lease_seconds` | `3600` | The lease lifetime scopes that set none inherit. **Minimum 300** — a lease measured in seconds has every client on the segment renewing continuously, and the operator who typed it meant minutes. Renew and rebind timers are derived from it (50% and 87.5%) |
| `dhcp.lease_poll_seconds` | `10` | How often dnsaur reads the engine's lease database into its in-memory table. **Minimum 2** — below that the poll costs the engine more than the freshness it buys. Read on every tick, so a change takes effect at the next poll rather than at the next restart |
| `dhcp.ha_port` | `8000` | The port each box's Kea HA listener answers on. It is not a port dnsaur binds: it is the port in both peers' URLs, and the HA hook opens the listener itself. Both boxes must agree, which they do — the value is synced. Each box's own peer URL is built from the host half of its first `dns_listen`, so **a pair needs a real address there on both boxes** rather than the default wildcard; that is the same rule pairing already depends on (a replica registers its `dns_listen` verbatim). A box whose listener names no host renders `http://:8000/` for itself, which the engine refuses — the message is on the DHCP page |
| `serve.dhcp_interfaces` | *(empty)* | Comma-separated interface names Kea binds (`eth0`, `eth0.10`). Empty means every interface. Local to the box, like the rest of `serve.` |
| `sync.primary_dns` | *(empty)* | An **override** for where the main answers DNS — the address a replica's derived secondary zones transfer from. The host is required when it is set. Empty, which is the ordinary case, means the peer URL's host on the port the main advertises in its version probe (53 when it advertises none) |

**†** — Restart-required exception. `cache.*` sizing/TTL settings and
`lists.refresh_hours` are read once at startup (the cache and the
background refresh ticker are sized/scheduled then); a running instance
must be restarted to pick up changes to these keys. Everything else in the
table — blocking mode/TTL, upstreams, upstream strategy, the six
`serve.*` encrypted-serving keys, the four `dhcp.*` keys and
`serve.dhcp_interfaces` (a change to any of them re-renders the engine's
configuration, and only when it actually changed), clients, groups, lists,
rules, zones and their records, and query-log privacy — applies live, no
restart needed:
settings keys through the change-notification channel, and clients,
filters and zones through a reload the write handler triggers directly.
This is a documented Phase 1 limitation, expected to be revisited in a
later phase.

Bookkeeping rows are not configuration: the `instance.*` prefix, the
rollup watermark `stats.watermark`, the eight sync rows listed under
Config sync below, and the two `dhcp.ha_*` rows beside them. None of them is editable through
`PUT /api/v1/settings`, and `GET /api/v1/settings` omits them all.
`sync.token` is excluded from that read too, and is the only *editable* key
that is: a read must not be a way to copy the credential out.

The pause state `blocking.pauses` is hidden and refused by `PUT` in the same
way, and is **not** one of those rows: a pause is a decision about the
network and clients reach either box, so it is configuration. It advances
`config_version`, a bundle carries it, and a pause set on the main is in
force on every replica within one interval — see
[Blocking pause](ui-contract.md#blocking-pause) for the state itself.

## Config sync

Two instances, one configuration. The **main** is the instance that takes
writes; any instance with `sync.peer_url` set is a **replica**, which pulls
the main's configuration every `sync.interval_seconds`, applies it in one
transaction, and refuses local writes to anything it covers with `409
managed by <peer>`. The four `sync.*` keys in the table above are all there
is to configure, and pairing writes two of them. What travels in a bundle
and what stays on each box is in
[`docs/architecture.md`](architecture.md#config-sync-main-and-replica),
the endpoints are in [`docs/api.md`](api.md), and the screens in
[`docs/dashboard.md`](dashboard.md#sync).

Eight more `sync.*` rows exist in the `settings` table and are **not**
settings: the server writes them about itself, `PUT /settings` refuses them
(`setting not editable`), and `GET /settings` omits them. They are reported
by `GET /api/v1/sync/status` instead, in a shape the dashboard can use —
`sync.pairing` excepted, since a live code is shown once when it is minted
and never read back.

Two `dhcp.*` rows get the same treatment for the same reason, and are in the
table below beside them: the main's DHCP renderer writes the HA pair it
chose from this registry, so the choice is derived from the pairing rather
than typed. Unlike the `sync.*` rows they are **synced** — both boxes have to
render the same pair — and `GET /api/v1/dhcp/status` is where the result is
visible.

| Key | On | Meaning |
|---|---|---|
| `sync.applied_version` | replica | `config_version` of the last bundle it applied. Blanked when the box is promoted: a counter it no longer follows is not a number to compare the next main against |
| `sync.applied_peer` | replica | the peer that version was applied from. Version numbers are each main's own count of its own writes, so a replica re-pointed at a main that happens to sit at the same number still fetches the bundle |
| `sync.applied_at` | replica | when it applied that bundle (unix ms) |
| `sync.last_pull_at` | replica | when the last pull cycle finished, successful or not (unix ms) |
| `sync.last_error` | replica | why the last cycle failed; cleared by the next one that succeeds |
| `sync.replicas` | main | the registered replicas, as JSON keyed by instance id, each with the hash of the secret that box pulls with |
| `sync.pairing` | main | the live pairing code — its hash, when it expires, and how many wrong attempts have been made against it. Never the code itself: 40 bits would not survive being ground offline in the ten minutes it is alive |
| `sync.tsig_key_id` | main | the TSIG key replicas transfer under, created and recorded by the first pairing. Nobody picks it, and `GET /api/v1/sync/status` reports its name |
| `dhcp.ha_primary` | both | the `instance.id` of the box that renders as the HA primary. Written by the main's renderer and carried to the replica in the bundle — both boxes have to render the same two peers under the same names, or Kea's hook has no partner to find |
| `dhcp.ha_standby` | both | the `instance.id` of the replica the main chose as standby: the non-stale registered replica with the lexicographically smallest id. A replica renders the pair when this is its own id, and no HA section otherwise |

### Setting up a pair

Two boxes, `main.example` and `replica.example`, both already set up and
running. The replica has to be a **fresh install**, not a restore of the
main's database: the two would share one `instance.id`, and both ends refuse
that — a main will not pair with its own id, and a replica whose peer answers
the version probe with the replica's own id will not follow it.

The two are introduced by a **pairing code**, and that code is the only thing
that crosses by hand. Three steps, each on one box:

1. **On the main**, press **Add replica** in the Sync band, or
   `POST /api/v1/sync/pairing-code`. It answers a code of eight characters in
   two groups — `KTRW-9PJM` — shown once and good for ten minutes. One code
   is live at a time: minting a second voids the first.

2. **On the replica**, enter the main's URL and that code and press
   **Follow**, or:

   ```
   POST /api/v1/sync/follow
   {"peer_url": "https://main.example", "code": "KTRW-9PJM"}
   ```

   The URL is scheme and host only — no path, query or credentials. The
   replica spends the code on the main, gets back a secret of its own, writes
   it and the peer URL in one settings write, and pulls once immediately
   rather than waiting out an interval. `204` means the pairing landed. The
   code's case and its grouping dash are yours to get wrong; five wrong codes
   void the live one and the main has to mint another.

3. **Wait for the first pull.** Following kicks one off at once; after that
   the replica probes every `sync.interval_seconds`, 30 by default.

Nothing else is configured. The main creates the TSIG key its replicas
transfer under on the first pairing — named `sync-<six characters>.`,
HMAC-SHA256, listed on the **TSIG keys** page like any other key and not
deletable while it is designated — and pairing is what admits that replica's
transfers and adds it as a NOTIFY target. The operator never picks a key and
never handles the secret a replica pulls with.

`sync.interval_seconds` and the `sync.primary_dns` override are the only sync
settings left to touch, both optional, both under **Advanced** in the band.

Within one interval, expect:

- the replica's groups, clients, lists, rules, TSIG keys and settings to
  match the main's, under the main's ids — and the lists it was given to be
  downloaded shortly after the first pull rather than at the next
  `lists.refresh_hours`, since a bundle carries a list's URL and not its
  contents;
- every `primary` zone on the main to exist on the replica as a
  **secondary** transferring from the main's DNS address under the sync key —
  with no `allow_transfer` or `notify_to` edit on either box, since pairing
  is what admits the transfer and adds the NOTIFY target;
- the replica to appear on the main's Sync band with its DNS address,
  applied version and last-seen stamp. It appeared there the moment it
  paired; every version probe carries the version it has applied, and that
  probe is the only heartbeat there is. Missing three of them shows it as
  stale, and nothing removes it but the operator — **Forget** in the band, or
  `DELETE /api/v1/sync/replicas/{instance_id}`, which revokes that box's
  secret with the entry;
- writes to synced configuration on the replica to answer `409 {"error":
  "managed by https://main.example"}`, while its own Protocols, Sync and
  Backup bands, its account, sessions and tokens, and both **Refresh now**
  actions keep working.

Use HTTPS. There is no certificate subsystem here — a reverse proxy in front
of the main, or a LAN you trust, is your call — and over plain `http://` the
pairing code, the replica's secret and the whole bundle, every TSIG secret on
the main included, cross the network in the clear. A replica following an
`http://` peer says so in its Sync band for as long as it is true.

The code is short-lived rather than strong: eight characters from a 32-glyph
alphabet with no ambiguous pair in it is 40 bits, so what protects it is that
it expires after ten minutes, dies on the fifth wrong attempt, and is spent
by the first box that gets it right. Only its hash, its expiry and the count
of wrong attempts are stored. `POST /api/v1/sync/pair` is the one
unauthenticated write in the API — a box that has not paired yet holds no
credential to present — and it is throttled per source address on the login
endpoint's budget. Wrong, expired, spent and voided are one answer,
`403 pairing code refused`, so a guesser cannot learn whether a code is live.
Show a code to exactly one box. Anyone on the LAN who finds the window open
can spoil the live code with five wrong guesses — they learn nothing by it,
but the main has to mint another — and, since that budget is login's, they
lock their own address out of the login form doing it. A replica answers the
two unauthenticated sync routes with `409 managed by <main url>`, so which
box is the main is readable from the LAN; the addresses of both are in the
DHCP leases the pair hands out anyway.

What the code buys is that replica's own secret: 32 random bytes, good for
`GET /sync/version` and `GET /sync/bundle` on this main and nothing else. It
is not an API token, never appears on the tokens page, and the main keeps
only its hash. It is never returned by `GET /settings` on either box, and
leaves the replica only as the `Authorization: Bearer` header on a pull. A
session cookie and an API token are `401` on both of those reads at any
scope. Pairing the same box again replaces its entry and its secret.

To promote the replica, clear both keys in one write —
`PUT /api/v1/settings {"sync.peer_url": "", "sync.token": ""}`, which is what
the Sync band's **Stop following** sends. It keeps the configuration it last
applied and takes writes again; the zones it derived stay secondaries until
you change each one's type on its own page.

## DHCP

dnsaur does not implement DHCP. ISC Kea (`kea-dhcp4`) serves the protocol,
and dnsaur is its control plane: it renders Kea's whole configuration from
the scopes, reservations and settings you edit, sends it over Kea's unix
control socket, and reads the lease table and the engine's status back. You
write one Kea config file, once, and never edit it again.

This section is the install and the wiring. The screens are in
[`docs/dashboard.md`](dashboard.md#dhcp), every field's rules in
[`docs/ui-contract.md`](ui-contract.md#311-dhcp), and the routes in
[`docs/api.md`](api.md); what happens to a lease after it is handed out —
the DNS names, the `mac` client matcher — is in
[`docs/architecture.md`](architecture.md#dhcp).

### Installing the engine

Debian 13 (trixie) ships Kea 2.6.3. The HA and `lease_cmds` hooks are
delivered with the server package rather than inside it — they are in
`kea-common`, which `kea-dhcp4-server` depends on at the same version — so
one install is still all it takes:

```sh
sudo apt install kea-dhcp4-server
```

Alpine edge ships Kea 3.0.3 and packages every hook separately. Both of
these are needed — leases are read over `lease_cmds`, and a pair renders the
HA hook:

```sh
apk add kea-dhcp4 kea-hook-ha kea-hook-lease-cmds
```

2.6 and 3.0 are the tested range (both were driven by hand on 2026-09-13,
and CI runs the Debian package on every push). The renderer emits only keys
both versions understand, with one exception it asks about: the control
socket is `control-socket` below 2.7.2 and `control-sockets` from 2.7.2 on,
so dnsaur reads the version once at start (`version-get`) and spells it
accordingly. The hook directory is the other thing it asks rather than
assumes: it reads the one the engine's own configuration names
(`config-get`), which is why the file below names a hook library, and is how
Debian's `/usr/lib/<triplet>/kea/hooks` and Alpine's `/usr/lib/kea/hooks`
both work with no setting.

### The Kea config file you write once

Kea will not start without a valid configuration, and dnsaur replaces that
configuration the moment it connects. So the file only has to be startable,
and it only has to say four things: bind nothing, listen on the control
socket dnsaur will be given, keep leases in a file, and name one hook
library so dnsaur can see where the hooks live.

`/etc/kea/kea-dhcp4.conf`, replacing what the package shipped:

```json
{
  "Dhcp4": {
    "interfaces-config": { "interfaces": [ ] },
    "control-socket": { "socket-type": "unix", "socket-name": "/run/kea/kea.sock" },
    "lease-database": { "type": "memfile", "persist": true, "name": "/var/lib/kea/kea-leases4.csv" },
    "valid-lifetime": 3600,
    "hooks-libraries": [
      { "library": "/usr/lib/x86_64-linux-gnu/kea/hooks/libdhcp_lease_cmds.so" }
    ],
    "subnet4": [ ]
  }
}
```

**That hook path is Debian's on amd64.** Use your own architecture's
(`dpkg -L kea-common | grep hooks`), or `/usr/lib/kea/hooks` on Alpine. The
line is there for dnsaur rather than for Kea: dnsaur reads the hook
directory out of the engine's running configuration, and naming one library
is what makes that answer right anywhere. A configuration that names none
leaves it guessing — `/usr/lib/x86_64-linux-gnu/kea/hooks` first, then
`/usr/lib/kea/hooks`, whichever exists — which is the wrong guess on, say,
an arm64 Debian box, and the render is then refused with Kea naming the
file it could not open.

On Kea 2.7.2 and later — Alpine's 3.0 — write
`"control-sockets": [ { "socket-type": "unix", "socket-name": "…" } ]`
instead. Not both: 3.0 refuses a configuration carrying the two spellings.

No subnets and no interfaces is deliberate. It means a Kea started before
dnsaur has ever rendered hands out nothing at all, rather than serving
whatever was in the file; the first render is what puts subnets in it.

**The paths are restricted** — on both builds, not just Debian's: control
sockets under `/run/kea`, lease files under `/var/lib/kea`, logs under
`/var/log/kea`. Anything else is refused at start with
`invalid path specified: '<yours>', supported path is '<theirs>'`, which is
at least a message that says what to do. Debian's packaged unit creates all
three — `RuntimeDirectory=kea` at mode 0750, plus `StateDirectory` and
`LogsDirectory` — which matters because `/run` is a tmpfs, so `/run/kea` has
to be made again on every boot; running `kea-dhcp4` by hand outside the unit
means making it yourself.

### Letting dnsaur reach the socket

Point dnsaur at the same path, in `dnsaur.yaml` or as
`DNSAUR_KEA_SOCKET=/run/kea/kea.sock`:

```yaml
kea_socket: /run/kea/kea.sock
```

Empty — the default — means DHCP is off entirely: nothing is rendered, no
leases are polled, no names come from leases, and the dashboard has no DHCP
section. It is bootstrap rather than a setting because Kea keeps the socket
it was started with, so this is a restart either way.

Then make the socket reachable, and check the mode rather than assuming it:
Kea creates the socket owned by the user it runs as, but the two builds do
not agree on the bits. **Debian's 2.6.3 creates it `0750`** — and connecting
to a unix socket needs *write* permission, so a member of the `_kea` group
holding `r-x` gets `permission denied`, which makes group membership alone
useless there. **Alpine's 3.0.3 creates it `0770`**, where a group member
does get in. Both checked; `ls -l` on your own box settles it.

On Debian, then, dnsaur has to *be* `_kea`. The packaged unit runs under
`DynamicUser=`, which has no fixed user to put in a group in the first
place, so override it —
`/etc/systemd/system/dnsaur.service.d/kea.conf`:

```ini
[Service]
DynamicUser=no
User=_kea
Group=_kea
```

`systemctl daemon-reload && systemctl restart dnsaur` after writing it, and
check the database is where you expect on the first start: `DynamicUser=`
keeps the state directory under `/var/lib/private/` with `/var/lib/dnsaur` a
symlink into it, and turning it off moves that back. The
other way round works as well — run `kea-dhcp4` as whatever user dnsaur
already runs as — but then Kea's own state and log directories are the ones
that need re-owning.

A wrong permission is not a silent failure, but the page is terse about it:
the engine line reads `Engine unreachable` and nothing more, and the dial
error itself — `permission denied` on the path — is in
`GET /api/v1/dhcp/status`'s `message` and in dnsaur's log. Fix the
permission and the next poll renders; nothing needs restarting.

### The settings

The four `dhcp.*` keys and `serve.dhcp_interfaces` are in the settings table
under *Database-managed settings* above. In short:

- `dhcp.domain` — the suffix clients are handed and leases are named under.
  Empty means leases get no names at all.
- `dhcp.lease_seconds` — the lease lifetime a scope that sets none inherits.
- `dhcp.lease_poll_seconds` — how often the lease table is re-read.
- `dhcp.ha_port` — the port in both peers' HA URLs (see below).
- `serve.dhcp_interfaces` — comma-separated interface names Kea binds
  (`eth0`, `eth0.10`); empty binds every interface. Local to the box, so
  each half of a pair names its own. Whether the name exists is not checked
  here; Kea names the interface it could not find, and the message lands on
  the DHCP page.

Changing any of them re-renders the engine's configuration. So does every
scope and reservation write, and so does a bundle arriving on a replica.

### A pair

A main and a replica that have paired for config sync render **one Kea
hot-standby pair** between their two engines, from the same synced
configuration. Kea does the rest: lease synchronisation, the standby
answering only once the primary has left clients unacknowledged, and the
reconciliation afterwards.

Four things have to be true for it:

- **Both boxes need a real address in `dns_listen`**, not the default
  wildcard. Each box's peer URL is built from the host half of its first
  `dns_listen` entry, and each box's own address is what its scopes hand out
  as the first DNS server. A wildcard listener names no host, so the render
  is refused with `HA pair needs a host in this box's dns_listen (it is
  0.0.0.0:53); set one such as 192.168.150.40:53` rather than sent as
  `http://:8000/` for Kea to reject. This is the same
  rule pairing already depends on — a replica registers its `dns_listen`
  verbatim — and a main reached by name needs `sync.primary_dns` set to an
  address.
- **`dhcp.ha_port` (8000 by default) has to be reachable between the two
  boxes.** It is not a port dnsaur binds: Kea's HA hook opens its own HTTP
  listener on it. Verified on 2026-09-13 on both 2.6 and 3.0 with nothing
  but a unix control socket configured — which is why dnsaur renders **no**
  HTTP control socket and **no `kea-ctrl-agent` is needed**. An HTTP control
  socket on that port collides with the hook's own listener and fails the
  whole configuration.
- **The choice of standby is the main's.** It picks the non-stale registered
  replica with the lexicographically smallest `instance.id` — deterministic,
  so every render on either box makes the same choice — and records it in
  the synced settings `dhcp.ha_primary` and `dhcp.ha_standby`, which travel
  in the next bundle. The chosen replica renders the identical pair; every
  other replica renders no HA section, runs plain Kea and says
  `DHCP: not in the HA pair`. A replica whose registered address is a
  hostname rather than an address is skipped, with a line in the log saying
  so, and so is one that runs no DHCP engine of its own: every replica says
  whether its `kea_socket` names one on each version probe, and a standby
  with nothing behind it would leave the primary waiting out
  `max-response-delay` on every client before serving it. Because that
  answer arrives with the probe rather than with the pairing, the pair
  appears one `sync.interval_seconds` after a replica pairs, not at the
  moment it does. The two settings are written only once this box's own Kea
  has accepted a configuration carrying the pair — a main that cannot render
  one never tells a replica it is paired.
- **Same Kea version on both boxes.** One main on 2.6 and a standby on 3.0 is
  not something this renders for: each box asks its own engine what it is and
  spells the control socket accordingly, but the HA hook itself is the pair's
  and only a matching pair is the tested configuration.

**Forget the standby last.** Before pressing **Forget** on the replica that
is the standby, either promote it (**Stop following** on its own Sync band)
or stop its `kea-dhcp4` — clearing its `kea_socket` does the same. Forget
revokes that box's pull secret along with its row, so the cleared pair never
reaches it in a bundle: its Kea keeps the pair it was last given, finds the
main no longer talking to it, and goes `partner-down` — which means it starts
answering the whole segment on its own. dnsaur drops the pair from that
replica's *next* render, because a pull the main refused the token on is this
box having been removed rather than a network hiccup; but nothing renders
until something on that box changes, so do it in the order above.

Scopes and reservations are synced configuration, so they are the main's to
edit; releasing a lease stays live on both boxes, because a lease belongs to
the engine and not to the configuration.

The HA channel between the two engines is **plain HTTP on the LAN**. It
carries leases — addresses, hardware addresses, hostnames — and no
credentials. Kea supports TLS and basic auth on it; dnsaur renders neither
in this milestone. Keep `dhcp.ha_port` off any untrusted segment.

Both boxes hand out the same two DNS servers in the same order, main first.
That is what DHCP-level failover needs from DNS: a client keeping its lease
through a takeover keeps its resolvers with it.

### VLANs and relays

One scope is one subnet, and a router relaying DHCP is how a box serves a
segment it has no interface on. Two notes:

- **Relay to both boxes.** In hot-standby both engines have to see the
  traffic — the standby answers only when the primary does not, and it can
  only do that for requests that reach it. Most routers take a list of
  helper addresses per interface.
- **The scope is chosen by `giaddr`**, the address the relay stamps on the
  request, so a relayed request is served from the scope whose subnet
  contains the relay's own address on that segment — not from the scope the
  packet arrived on an interface for.

A box serving a VLAN it *does* hold an interface on wants that interface in
`serve.dhcp_interfaces` (`eth0.10`), or the default of every interface.

Scopes for segments this box has no address inside need their `dns_servers`
filled in: the automatic answer is the box's own address on the scope's
segment, and a render that cannot find one is refused with
`scope <name>: set dns_servers, no local address is inside <cidr>` rather
than handing clients a resolver they cannot reach.

### In a container

`Dockerfile.kea` is the image variant with the engine beside dnsaur — Alpine
edge for Kea 3.0 and its hooks, an entrypoint that starts `kea-dhcp4` with a
bundled minimal configuration and then becomes dnsaur. Build it as described
in [`docs/development.md`](development.md#container-image).

**Host networking, and nothing else will do.** DHCP is broadcast on the
segment: a bridged container never sees a `DHCPDISCOVER`, and published
ports do not help, because the client has no address yet to send a unicast
from.

```sh
docker run -d --name dnsaur --network host \
  -e DNSAUR_DNS_LISTEN=:53 \
  -v dnsaur-data:/data \
  -v dnsaur-kea-leases:/var/lib/kea \
  dnsaur-kea:local
```

Both processes run as root in that image — `kea-dhcp4` opens raw sockets and
dnsaur binds 53 — so the socket permissions above are already satisfied.
`DNSAUR_KEA_SOCKET=/run/kea/kea.sock` is baked in. Mount your own file at
`/etc/kea/kea-dhcp4.conf` to change what the engine starts with, keeping the
control socket where it is.

Nothing supervises the engine: if `kea-dhcp4` exits, the container keeps
serving DNS and the DHCP page reports the engine unreachable. A `docker stop`
does reach it — the entrypoint passes the signal on and waits for the engine
to write out its lease file before the container ends.

**The lease database wants a volume of its own.** Kea's memfile lives in
`/var/lib/kea`, which is declared a volume so `docker run` gives it an
anonymous one; name it — `-v dnsaur-kea-leases:/var/lib/kea` — or every lease
on the segment comes back as a free address the next time the container is
recreated.

The plain `Dockerfile` assumes Kea on the host: bind-mount the socket
directory (`-v /run/kea:/run/kea`) and set `DNSAUR_KEA_SOCKET`. That image
runs as uid 65532, which a 0750 socket does not admit, so run it as the uid
that owns the socket — `--user "$(stat -c %u /run/kea/kea.sock)"`, since the
container has no idea what `_kea` means.

### What this does not do

Everything below is deliberate, not missing by accident:

- **dnsaur does not manage the Kea process.** Starting, stopping and
  upgrading it is your service manager's job, exactly as above.
- **No DHCPv6 and no router advertisements.** Kea has a DHCPv6 server;
  nothing here talks to it.
- **No client classes, vendor-class matching or option 82 policies**, and no
  ping check before an offer — the `ping_check` hook is not in Debian's 2.6
  package.
- **One pool per scope, and no exclusions.** A scope is one range, plus
  reservations, which may sit inside the pool or outside it.
- **A reservation lives inside its scope's subnet.** Moving one between
  scopes is a delete and a re-create.
- **One standby.** Kea's HA supports more in load-balancing mode; a main and
  a backup is what this renders.
- **Generic options cover the rest.** Anything dnsaur has no field for goes
  in by code and hex value — WINS (44), CAPWAP (138), TFTP (150), vendor
  info (43) — and the PXE trio has fields of its own.

### From nothing to a lease

1. Install Kea and write the config file above. `systemctl enable --now
   kea-dhcp4-server`, then check that `/run/kea/kea.sock` exists.
2. Set `kea_socket` in dnsaur's bootstrap config, sort out the socket
   permission, and restart dnsaur. The DHCP section appears in the nav and
   its engine line reads `Engine 2.6.3 · single`.
3. Give leases a DNS suffix — `home.lan`, no trailing dot. The scope form's
   DNS suffix field sets it per scope; a global default for every scope is
   the setting `dhcp.domain`, in **Settings → DHCP** beside the lease time,
   the poll interval, the HA port and this box's DHCP interfaces (the band
   appears only on a box with an engine). **An empty suffix means leases get no
   names at all**: addresses still work, nothing resolves, and step 5 has
   nothing to show.
4. Create a scope on the Scopes page: the subnet in masked form
   (`192.168.1.0/24`), a pool inside it, the gateway. Leave the DNS servers,
   the suffix and the lease time blank unless you mean to override them —
   blank means "use the instance default", which is what step 3 set. Saving
   renders; if Kea refuses the configuration, its own words are on the
   engine line and **Apply again** re-sends once you have fixed it.
5. Point a device at the segment and watch the Leases page. A row appears on
   the next poll, at most `dhcp.lease_poll_seconds` after the engine hands
   the address out, and `<hostname>.<suffix>` starts resolving from the same
   table.

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
tls://[2606:4700:4700::1111]#cloudflare-dns.com  DoT over IPv6; the brackets are required
```

The parser (`internal/upstream/addr.go`, mirrored for the dashboard by
`web/src/lib/upstreams.ts` against the shared fixture
`internal/upstream/testdata/grammar.json`) rejects, each with a reason: a
`tls://` or `https://` host that isn't an IP literal; an IPv6 address
written without brackets, where the reason shows the bracketed form to
write instead; `tls://` or `https://` without `#name`; `#name` on a plain
entry; and a list that mixes schemes.
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
deliberately later release, scheduled together with DNSSEC validation (see
the status table in the README).

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

## Encrypted serving

The six `serve.*` settings above are the other half of encrypted DNS: not
what dnsaur trusts to resolve a name (Encrypted upstreams, above) but
whether a client reaching dnsaur itself gets DNS-over-TLS or
DNS-over-HTTPS instead of plain UDP/TCP port 53. Both apply live — enabling,
disabling, or repointing a listen address or certificate reconciles the
running listeners on the next settings write, no restart. Toggling one
protocol never disturbs the other's live connections; only the protocol
whose configuration actually changed is stopped and restarted.

**Order matters: save the certificate before enabling.** `PUT /settings`
refuses `serve.dot.enabled=true` or `serve.doh.enabled=true` unless
`serve.tls.cert` and `serve.tls.key` are both already set to an absolute
path and load as a matching pair — the same `tls.LoadX509KeyPair` call the
listener itself makes before binding. The rejection says so directly, "set
serve.tls.cert and serve.tls.key first", rather than a bare "invalid
value", because the fields that make the write valid are not the field
being written. Disabling never has this requirement — an operator must
always be able to turn a protocol off, including when the certificate has
gone missing, which is exactly when they most need to.

```
serve.tls.cert  /etc/letsencrypt/live/dns.example.com/fullchain.pem
serve.tls.key   /etc/letsencrypt/live/dns.example.com/privkey.pem
```

The rule runs the other way too: **clearing `serve.tls.cert` or
`serve.tls.key` while either protocol is enabled is refused.** An empty
path is not a configuration the reconciler can act on — it would stop the
running listener and then fail to start it again with `certificate: stat :
no such file or directory`, an error naming nothing. Turn the protocol off
first, then clear the paths.

**Rotating to a *different* path is a four-step change, with downtime.**
A direct swap of one half is rejected as a mismatched pair, since the new
certificate does not match the old key, so moving to a new directory means:
turn both protocols off, clear both paths, set both new paths, turn the
protocols back on. This is deliberate rather than an omission — the case it
would optimise for barely exists, because certbot renews *in place*, over
the same two paths, and that path needs no settings change at all (see
Certificate delivery, below).

### What a DoH client has to send

The endpoint is RFC 8484's, at `/dns-query`, and the path is not
configurable. A `GET` carries the query as base64url in the `dns`
parameter; a `POST` carries it as the request body and **must be typed
`Content-Type: application/dns-message`** — anything else is answered
`415`. Accepting an untyped or wrongly typed body would hide a client's
misconfiguration until it met a resolver that checks. Queries larger than
65535 bytes are rejected rather than truncated, and zone transfers and
NOTIFY are refused on this transport: neither has a meaning inside an HTTP
request/response.

### The bootstrap chain

A DoT client is configured with a **hostname**, not an address — Android's
Private DNS accepts nothing else. Getting from that hostname to an open,
certificate-validated connection goes through plain DNS first:

1. The device resolves `dns.example.com` using the resolver the network
   already handed it, which is dnsaur, in plaintext, on port 53.
2. dnsaur answers with its own address.
3. Only then does the device open DoT (or DoH) to that address and
   validate the certificate against the hostname.

**Plain DNS bootstraps encrypted DNS.** The consequence for deployment:
**the DoT/DoH hostname must resolve to the listener's address for internal
clients**, and dnsaur is the thing that makes that true — typically an
`A`/`AAAA` record in a zone dnsaur itself serves, pointing the hostname at
dnsaur's own address (see [`docs/dashboard.md`](dashboard.md#records) for
adding one). Skip this and the first deployment fails confusingly: the
handshake never completes, which looks like a certificate problem and is
actually a DNS one — the hostname never resolved, or resolved somewhere
else.

### DNS-01 is the only usable ACME challenge

When that hostname resolves to a private address for internal clients,
Let's Encrypt's own servers cannot reach it: HTTP-01 needs to fetch a URL
under the hostname, and TLS-ALPN-01 needs to open a TLS connection to it,
and both require the public internet to reach whatever address the
hostname resolves to. DNS-01 only needs a TXT record published in the
domain's public zone, which has nothing to do with how the hostname
resolves internally — so it is the one challenge type that still works
here.

### The key-permissions trap

certbot writes `privkey.pem` `0600 root:root`. dnsaur binds ports 53 and
853, both privileged, so it runs either as root or with
`CAP_NET_BIND_SERVICE` — and in the second case it cannot read a
root-only key. A certbot deploy hook fixes it on every renewal, not just
the first time:

```sh
#!/bin/sh
# /etc/letsencrypt/renewal-hooks/deploy/dnsaur.sh
chgrp dnsaur "/etc/letsencrypt/live/$RENEWED_LINEAGE/privkey.pem"
chmod 640 "/etc/letsencrypt/live/$RENEWED_LINEAGE/privkey.pem"
```

(`dnsaur` is whichever group the process actually runs as — adjust to
match.) Without the hook, the very next renewal silently reintroduces the
problem a one-off `chmod` just fixed.

This is exactly what save-time validation (above) is for: `PUT /settings`
runs `tls.LoadX509KeyPair` on the two paths before accepting the write, so
a permissions problem is a 400 naming `serve.tls.cert`/`serve.tls.key`
with the OS error that names the unreadable file — at the moment the
certificate is saved, not a failed bind discovered at three in the
morning.

### Certificate delivery is out of scope

dnsaur reads a certificate; it does not obtain one. How the file gets to
`serve.tls.cert`/`serve.tls.key` — certbot running on the dnsaur host, an
`rsync` from a host that does, a manual copy — makes no difference to
dnsaur: the reload watches the *file*, not the process that wrote it
(`internal/dnssrv/certs.go` compares both files' mtimes on every
handshake and reloads the pair together whenever either one has changed).
Whichever delivery mechanism is used, the next handshake after a write
picks up the new keypair, with no restart and no settings change. There
is no certificate-upload or ACME-client feature to look for here, on
purpose — accepting an uploaded key would put private key material in the
settings table, which `GET /settings` returns wholesale.

The safety net for a delivery mechanism that quietly stops — a disabled
timer, a hook that stopped firing, a copy job someone forgot about — is
the **expiry warning**: `GET /resolver/status`'s `certificate` field
carries the loaded certificate's `not_after` and an `expiring_soon` flag
that is true once it is within 14 days of that date. The threshold is
fixed, not a setting, because an operator who could tune it could tune it
to never fire.

The field describes a certificate that is actually in use: it is **absent
entirely** when neither protocol is enabled, when no keypair is configured,
and when none has ever loaded — three different reasons that all amount to
"there is no certificate to warn about". And it follows a renewal without
waiting for a client: reading the status re-checks both files' mtimes, so
a certbot renewal on a quiet resolver clears the warning at the next status
read rather than at the next handshake.

### A bind failure is reported, not just logged

`serve.dot.enabled`/`serve.doh.enabled` are intent; whether a socket is
actually open is a separate fact, and the two are allowed to disagree — a
privileged port already taken by something else, or a certificate that
will not load at the moment a listener starts, leaves the setting `true`
and the listener down. `GET /resolver/status`'s
`serving.dot`/`serving.doh` carry both fields (`enabled`, `listening`)
plus the bind `error` when they disagree, so a protocol that is enabled
but not listening is visible on screen instead of only in the log — the
same "intent is not reality" pattern the encryption-downgrade warning
above already uses for upstreams.

A certificate that stops being readable **under an already-running
listener** is a different case, and does not bring it down: the listener
keeps serving from the keypair it already loaded, and nothing re-probes the
filesystem for a configuration that has not changed. The disagreement only
appears the next time that listener has a reason to start — a restart, an
address change, or a certificate-path change.

**The warning clears on its own.** dnsaur re-attempts the bind every 30
seconds while either protocol is enabled and not listening, so stopping
whatever was holding the port is enough; there is no settings write to make
and nothing to restart. When both protocols are converged, nothing is
retried and nothing is polled.

### What this protects, and what it does not

Once DoT or DoH is enabled, **other devices on the same network stop
seeing which names a client looks up** — a compromised IoT device, a
guest, anything else on the segment can no longer read plaintext DNS off
the wire. **dnsaur itself still sees every query, exactly as before.**
This mirrors [Encrypted upstreams](#encrypted-upstreams)'s own caveat: the
resolver you chose still sees everything you send it. Encrypted serving
protects the local hop the same way encrypted upstreams protects the
outbound one — neither removes dnsaur, or whichever upstream it forwards
to, from the trust picture. Only the network paths in between.

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
