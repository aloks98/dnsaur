# dnsaur

A self-hosted DNS server for homelabs — Pi-hole's blocking with Technitium's
server depth, built-in HA that neither of them ships out of the box.

dnsaur is one Go binary: a forwarding/caching DNS resolver with per-client
blocklist filtering, a query log, and stats, backed by SQLite (or Postgres)
with hot-reloadable settings. It's built to be both a solid open-source
project and the author's own daily-driver network DNS.

See `docs/superpowers/specs/2026-08-03-dnsaur-design.md` for the full design
spec (architecture, phasing, RFC targets, decisions).

## Status

dnsaur is early and under active development. The table below is the source
of truth — if it's not listed as shipped, it doesn't work yet.

| Feature | Status |
|---|---|
| Blocking resolver (forward + cache, hosts/plain/ABP-subset blocklists) | Shipped |
| Per-client groups (IP/CIDR matchers, per-group lists & rules) | Shipped |
| Local DNS records | Shipped |
| Caching with TTL clamps, negative caching, serve-stale | Shipped |
| Query log (SQLite/Postgres, buffered writes, retention pruning) | Shipped |
| Stats (hourly rollups) | Shipped |
| Hot-reload of DB-managed settings | Shipped |
| REST API + auth (sessions, scoped tokens, TOTP) | Shipped |
| Web dashboard | Planned |
| HA config sync (primary/replica) | Planned |
| DHCP | Planned |
| Encrypted DNS (DoH/DoT upstream and serving) | Planned |
| Authoritative zones + DNSSEC | Planned |

There is no web dashboard or published Docker image yet, but you no longer
need to hand-edit the database: the REST API under `/api/v1` (see
[`docs/api.md`](docs/api.md)) is now the configuration surface — settings,
client groups, filter lists, local records, and more are all managed over
HTTP (curl or any HTTP client) for now, ahead of a proper dashboard.

## Quick start

Build from source (Go 1.26+):

```sh
git clone https://github.com/aloks98/dnsaur.git
cd dnsaur
go build ./cmd/dnsaur
```

Write a minimal `dnsaur.yaml` next to the binary:

```yaml
dns_listen: [":53"]
http_listen: ":8080"
data_dir: "./data"
log_level: "info"
storage:
  driver: sqlite
```

Run it:

```sh
./dnsaur -config dnsaur.yaml
```

On first start dnsaur creates `./data`, opens (and migrates) its SQLite
database, seeds default settings (Cloudflare/Quad9 upstreams, a `default`
client group), and starts serving DNS on `:53`. Point a client or your
router's DNS setting at the host running dnsaur to try it.

Docker images are planned (multi-arch, GHCR) but not published yet — for
now, building from source is the only supported install path.

## Documentation

- [`docs/architecture.md`](docs/architecture.md) — middleware pipeline, package map, storage model
- [`docs/configuration.md`](docs/configuration.md) — bootstrap YAML/env vars and DB-managed settings
- [`docs/api.md`](docs/api.md) — REST API (auth, endpoints, examples)
- [`docs/development.md`](docs/development.md) — build, test, lint, CI, contributing

## License

No license has been chosen yet — license TBD. Until a `LICENSE` file is
added, treat this repository as "all rights reserved."

## Contributing

See [`docs/development.md`](docs/development.md) for prerequisites, build/test/lint
commands, and commit conventions.
