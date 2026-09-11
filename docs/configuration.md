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
| `sync.peer_url` | *(empty)* | The main instance this one follows, as an absolute `http`/`https` URL. Empty means this instance is a main and accepts writes; non-empty makes it a replica, which pulls the main's configuration and refuses local writes to anything that configuration covers. Clearing it is the promotion. Setting it requires `sync.token` |
| `sync.token` | *(empty)* | The write-scope API token, minted on the main, that this replica pulls with. **Never returned by `GET /api/v1/settings`** — it is a credential, and the settings screen shows only whether one is set |
| `sync.interval_seconds` | `30` | How often a replica probes the main's config version. **Minimum 5** — below that the probe costs the main more than the drift it removes |
| `sync.primary_dns` | *(empty)* | `host:port` where the main answers DNS; the address a replica's derived secondary zones transfer from. The host is required. Empty means the peer URL's host on port 53 |
| `sync.tsig_key_id` | `0` | On a main: the TSIG key replicas transfer under. `0` means none is designated, and creating the key and setting this is the one manual step on the main. Must name a key that exists |

**†** — Restart-required exception. `cache.*` sizing/TTL settings and
`lists.refresh_hours` are read once at startup (the cache and the
background refresh ticker are sized/scheduled then); a running instance
must be restarted to pick up changes to these keys. Everything else in the
table — blocking mode/TTL, upstreams, upstream strategy, the six
`serve.*` encrypted-serving keys, clients, groups, lists, rules, zones and
their records, and query-log privacy — applies live, no restart needed:
settings keys through the change-notification channel, and clients,
filters and zones through a reload the write handler triggers directly.
This is a documented Phase 1 limitation, expected to be revisited in a
later phase.

Two internal key prefixes (`instance.*` and future `stats.*` bookkeeping)
are not meant to be user-edited and are excluded from the settings API
(`GET /api/v1/settings`). `sync.token` is excluded from that read too, and
is the only *editable* key that is: a read must not be a way to copy the
credential out.

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
