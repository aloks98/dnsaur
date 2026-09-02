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
| Authoritative DNS zones (primary) | Shipped |
| Secondary zones (AXFR in, TSIG, SOA-scheduled refresh) | Shipped |
| Zone transfers out (AXFR to secondaries, TSIG-authenticated, allow-transfer ACL) | Shipped |
| DNS NOTIFY, both directions (RFC 1996) — dnsaur tells its own secondaries when a zone changes, and transfers promptly when told by its own primary | Shipped |
| Reverse DNS (PTR, RFC 6303 built-in zones, auto-PTR from A/AAAA) | Shipped |
| Caching with TTL clamps, negative caching, serve-stale | Shipped |
| Query log (SQLite/Postgres, buffered writes, retention pruning) | Shipped |
| Stats (hourly rollups) | Shipped |
| Hot-reload of DB-managed settings | Shipped |
| REST API + auth (sessions, scoped tokens, TOTP) | Shipped |
| Web dashboard | Shipped |
| HA config sync (primary/replica) | Planned |
| DHCP | Planned |
| Encrypted DNS (DoH/DoT upstream and serving) | Planned |
| DNSSEC | Planned |

There's no published Docker image yet, but you no longer need to hand-edit
the database or shell out to curl for everyday admin: a React dashboard
(`web/`, embedded into the `dnsaur` binary and served alongside the API —
see [`docs/architecture.md`](docs/architecture.md)) covers first-run setup,
live query monitoring, per-client blocklists/allowlists/rules, authoritative
zones, TSIG keys, and settings — [`docs/dashboard.md`](docs/dashboard.md)
walks through it screen by screen. The REST API under `/api/v1` (see
[`docs/api.md`](docs/api.md)) is still there underneath it — settings,
client groups, filter lists, zones, and more are all scriptable over HTTP
(curl or any HTTP client) too, dashboard or not.

## Quick start

Build from source (Go 1.26+, Node 22+ and pnpm for the dashboard):

```sh
git clone https://github.com/aloks98/dnsaur.git
cd dnsaur
cd web && pnpm install && pnpm build && cd ..   # builds the dashboard into web/dist
go build ./cmd/dnsaur                           # embeds web/dist into the binary
```

(`go build ./cmd/dnsaur` alone also works — the dashboard just embeds
empty and the binary serves API-only, since `web/dist` isn't committed to
the repo. Run `pnpm build` first for a working dashboard.)

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

Open `http://<host>:8080` (or whatever `http_listen` is set to) in a
browser for the dashboard — it walks you through creating an admin account
on first visit.

Docker images are planned (multi-arch, GHCR) but not published yet — for
now, building from source is the only supported install path.

## Documentation

- [`docs/dashboard.md`](docs/dashboard.md) — using the web UI: what each screen decides, and the rules behind it
- [`docs/architecture.md`](docs/architecture.md) — middleware pipeline, package map, storage model
- [`docs/configuration.md`](docs/configuration.md) — bootstrap YAML/env vars and DB-managed settings
- [`docs/api.md`](docs/api.md) — REST API (auth, endpoints, examples)
- [`docs/ui-contract.md`](docs/ui-contract.md) — what the API and dashboard actually do today: every endpoint, field, enum and error string, with what's still unbuilt
- [`docs/development.md`](docs/development.md) — build, test, lint, CI, contributing
- [`web/README.md`](web/README.md) — dashboard dev workflow, scripts, Playwright smoke test

## License

No license has been chosen yet — license TBD. Until a `LICENSE` file is
added, treat this repository as "all rights reserved."

## Contributing

See [`docs/development.md`](docs/development.md) for prerequisites, build/test/lint
commands, and commit conventions.
