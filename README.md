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
| Secondary zones (AXFR in, TSIG, SOA-scheduled refresh that checks the serial before transferring) | Shipped |
| Zone transfers out (AXFR to secondaries, TSIG-authenticated, allow-transfer ACL) | Shipped |
| DNS NOTIFY, both directions (RFC 1996) — dnsaur tells its own secondaries when a zone changes, and transfers promptly when told by its own primary | Shipped |
| Conditional forwarding zones (a suffix routed to upstreams you name, never to the default resolvers) | Shipped |
| Stub zones (a suffix routed to nameservers fetched from a master — no AXFR permission needed on it) | Shipped |
| Reverse DNS (PTR, RFC 6303 built-in zones, auto-PTR from A/AAAA) | Shipped |
| Caching with TTL clamps, negative caching, serve-stale | Shipped |
| Query log (SQLite/Postgres, buffered writes, retention pruning) | Shipped |
| Stats (hourly rollups) | Shipped |
| Hot-reload of DB-managed settings | Shipped |
| REST API + auth (sessions, scoped tokens, TOTP) | Shipped |
| Web dashboard | Shipped |
| Encrypted upstreams (DNS-over-TLS, DNS-over-HTTPS) | Shipped |
| HA config sync (a replica pulls the main's configuration as one bundle and its zone data by AXFR; the dashboard says who manages what) | Shipped |
| DHCP | Planned |
| Encrypted DNS serving (DoH/DoT for clients of dnsaur) | Shipped |
| DNSSEC (validation and signing) | Deferred — see below |

**DNSSEC is deferred on purpose** (decided 2026-09-08), not merely unbuilt.
dnsaur forwards to upstreams it reaches over DoT/DoH, and the default ones
(Cloudflare, Quad9) already validate and refuse bogus answers inside that
authenticated channel; validating again locally would mostly re-check their
work while adding the classic way DNS breaks — expired signatures, and
clock skew on a box with no RTC that needs DNS to reach NTP. Validation is
scheduled together with own-recursion, where there is no validating upstream
to lean on. Signing waits for a hosted zone that needs a DS at its
registrar. Until then a client that sets DO gets whatever signatures the
upstream returned, subject to the cache: an answer cached for a DO=0 client
is served without them.

Nothing is published to a registry or a package repo yet — the Dockerfile
and the deb/rpm build are in the repo and you run them yourself (see Quick
start). What you no longer need is to hand-edit the database or shell out to
curl for everyday admin: a React dashboard
(`web/`, embedded into the `dnsaur` binary and served alongside the API —
see [`docs/architecture.md`](docs/architecture.md)) covers first-run setup,
live query monitoring, per-client blocklists/allowlists/rules, authoritative
zones, TSIG keys, and settings — [`docs/dashboard.md`](docs/dashboard.md)
walks through it screen by screen. The REST API under `/api/v1` (see
[`docs/api.md`](docs/api.md)) is still there underneath it — settings,
client groups, filter lists, zones, and more are all scriptable over HTTP
(curl or any HTTP client) too, dashboard or not.

## Quick start

There are no published images and no packages to download yet, so all three
paths build from this repo. The release config is in place and exercised on
every tag ([`docs/development.md`](docs/development.md#release)), so
publishing is a decision rather than a project.

```sh
git clone https://github.com/aloks98/dnsaur.git
cd dnsaur
```

### Docker

```sh
docker build -t dnsaur:local .
docker run -d --name dnsaur \
  -e DNSAUR_DNS_LISTEN=:5353 \
  -v dnsaur-data:/data \
  -p 53:5353/udp -p 53:5353/tcp -p 8080:8080 \
  dnsaur:local
```

The image builds the dashboard and the binary itself, so nothing needs a Go
or Node toolchain. It is configured through `DNSAUR_*` variables, or a
`dnsaur.yaml` mounted at `/data/dnsaur.yaml`; see
[`docs/configuration.md`](docs/configuration.md#in-a-container), which also
covers why 53 is published onto a high port rather than bound directly.

### A deb or an rpm

```sh
cd web && pnpm install && pnpm build && cd ..   # or the packages serve API-only
go run github.com/goreleaser/goreleaser/v2@v2.18.1 release --snapshot --clean --skip=publish
sudo apt install ./dist/dnsaur_*_linux_amd64.deb   # or: sudo rpm -i dist/dnsaur_*_linux_amd64.rpm
sudo systemctl enable --now dnsaur
```

That installs `/usr/bin/dnsaur`, `/etc/dnsaur/dnsaur.yaml` to edit, and a
systemd unit that reaches port 53 without root and keeps its database in
`/var/lib/dnsaur`.

### From source

Go 1.26+, Node 22+ and pnpm:

```sh
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

### First start

dnsaur creates its data directory, opens (and migrates) its SQLite
database, seeds default settings (Cloudflare/Quad9 upstreams, a `default`
client group), and starts serving DNS. Point a client or your router's DNS
setting at the host running dnsaur to try it.

Open `http://<host>:8080` (or whatever `http_listen` is set to) in a
browser for the dashboard — it walks you through creating an admin account
on first visit.

## Documentation

- [`docs/dashboard.md`](docs/dashboard.md) — using the web UI: what each screen decides, and the rules behind it
- [`docs/architecture.md`](docs/architecture.md) — middleware pipeline, package map, storage model
- [`docs/configuration.md`](docs/configuration.md) — bootstrap YAML/env vars and DB-managed settings
- [`docs/api.md`](docs/api.md) — REST API (auth, endpoints, examples)
- [`docs/ui-contract.md`](docs/ui-contract.md) — what the API and dashboard actually do today: every endpoint, field, enum and error string, with what's still unbuilt
- [`docs/development.md`](docs/development.md) — build, test, lint, CI, contributing
- [`web/README.md`](web/README.md) — dashboard dev workflow, scripts, Playwright smoke test

## License

AGPL-3.0-only — see [`LICENSE`](LICENSE).

## Contributing

See [`docs/development.md`](docs/development.md) for prerequisites, build/test/lint
commands, and commit conventions.
