# dnsaur — Design Spec

**Date:** 2026-08-03
**Status:** Approved (Phase 1 + Phase 1.5 detailed; Phases 2–4 sketched)

## What & Why

dnsaur is a self-hosted DNS/DHCP server for homelabs, combining Pi-hole's
strengths (ad/tracker blocking, per-client groups, great dashboard) with
Technitium's (full DNS server, encrypted DNS, DNS↔DHCP integration,
first-class API) — plus built-in high availability, which both of those punt
to third-party tools.

It is both an open-source product (docs, packaging, and polish matter) and
the daily-driver DNS/DHCP for the author's own network (reliability matters
most; proven libraries over from-scratch protocol work).

## Decisions Made

| Decision | Choice |
|---|---|
| Language | Go |
| DNS wire protocol | `miekg/dns` library; server loop and pipeline are our own (AdGuard Home / Blocky pattern). CoreDNS-plugin and fork approaches rejected — dynamic web-UI config fights CoreDNS's static Corefile model; a fork is weak as a portfolio piece. |
| Web UI | React + Vite SPA, Tailwind/shadcn, embedded in the binary via `go:embed` |
| Storage | SQLite default (`modernc.org/sqlite`, no CGO), Postgres optional (`pgx`), behind a storage interface with per-dialect migrations |
| Deployment | Docker-first (distroless, multi-arch amd64+arm64, GHCR) + single static binary (GoReleaser: deb/rpm/tarball, systemd unit, setcap) |
| HA | Instance-to-instance config sync, primary/replica, each instance self-contained on its own SQLite. Shared-Postgres and multi-master CRDT models rejected (DB host becomes a SPOF; conflict-resolution complexity not worth it). |

## Phasing

Architecture is designed for all phases up front; implementation is phased.
Each phase ends with something usable and gets its own spec → plan cycle.

- **Phase 1 — Blocking resolver + dashboard** (this spec, detailed):
  forwarding/caching DNS, blocklist filtering with per-client groups, query
  log, stats, web UI + REST API. A Pi-hole replacement.
- **Phase 1.5 — HA config sync** (this spec, detailed): primary/replica
  sync so two machines serve identical config.
- **Phase 2 — DHCP:** leases, reservations, hostname auto-registration into
  DNS, DHCP failover guidance (split scopes or primary/standby).
- **Phase 3 — Encrypted DNS:** DoH/DoT upstreams, then serving DoH/DoT to
  clients.
- **Phase 4 — Authoritative zones + DNSSEC:** zone/record management,
  transfers, signing/validation.

## Architecture

One Go binary, four long-lived services communicating through shared
internal packages (never via each other's HTTP APIs):

```
                    ┌──────────────────────────────────────────┐
                    │                dnsaur binary             │
 :53 UDP/TCP ──────►│  DNS Engine (middleware pipeline)        │
 (later :853/DoH)   │    client-id → filter → local → cache    │
                    │    → upstream forwarder                  │
 :67 UDP ──────────►│  DHCP Server (Phase 2)                   │
                    │    └─ registers hostnames into DNS store │
 :8080 HTTP ───────►│  API Server (REST /api/v1)               │
                    │    └─ serves embedded React SPA          │
                    │  Background jobs: blocklist refresher,   │
                    │    log pruner, stats aggregator          │
                    │  Storage (SQLite default / Postgres opt) │
                    └──────────────────────────────────────────┘
```

Repo layout:

```
cmd/dnsaur/          — main, wiring, graceful shutdown
internal/dnssrv/     — listeners + middleware pipeline
internal/filter/     — blocklist engine, per-client groups
internal/upstream/   — forwarders, failover (Phase 3: DoH/DoT)
internal/cache/      — DNS cache, TTL + negative caching + serve-stale
internal/records/    — local DNS records (Phase 2: DHCP writes here)
internal/dhcp/       — Phase 2
internal/store/      — storage interfaces + sqlite/postgres drivers
internal/api/        — REST handlers, auth
internal/config/     — bootstrap config load/validation
internal/sync/       — Phase 1.5 primary/replica sync
web/                 — React SPA (go:embed)
```

**Key principle:** the middleware pipeline is the spine. Every future
feature is either a new pipeline stage or a new listener feeding the same
pipeline.

## DNS Engine — Middleware Pipeline

Every query from every listener flows through ordered stages; each stage
answers (short-circuit) or passes on:

1. **Client ID** — map requester IP to a known client (name, group).
   Unknown IPs → default group. Context attached for later stages + log.
2. **Filter** — qname vs. the client's group blocklists/allowlists/regex
   rules. Blocked → `0.0.0.0` by default (configurable: NXDOMAIN or null
   IP), tagged `blocked` in the log.
3. **Local records** — locally-defined records (Phase 2: DHCP hostnames;
   Phase 4: authoritative zones slot in here).
4. **Cache** — in-memory, keyed (qname, qtype). Upstream TTLs respected
   with configurable min/max clamps; negative caching (RFC 2308);
   serve-stale on upstream failure (RFC 8767) with background refresh.
5. **Upstream forwarder** — configured upstreams (UDP/TCP in Phase 1).
   Strategy configurable: parallel race or fastest-weighted rotation.
   Health checks + automatic failover. Per-domain conditional forwarding
   (e.g., `corp.example` → VPN DNS).

Query outcomes (client, qname, qtype, decision, upstream, latency, rcode)
are emitted asynchronously to the query log — logging never blocks
resolution. Stages implement a `Handler`/`Middleware` Go interface;
unit-testable in isolation, new stages additive.

## Filtering Engine

- **Sources:** subscribed list URLs (curated defaults offered at setup,
  e.g., StevenBlack, HaGeZi). Formats: hosts files, plain domain lists,
  AdGuard/ABP syntax (`||ads.example^`). Background refresh (default 24h):
  download → parse → diff → atomic swap; never blocks resolution.
- **Matching:** reversed-label radix trie; subdomain matches in O(labels)
  regardless of list size (~1M entries ≈ tens of MB, µs lookups).
  User-authored regex rules compile once, checked only after trie miss.
- **Precedence (first match wins):** explicit allow > explicit block >
  allowlist entry > blocklist entry.
- **Clients & groups:** clients identified by IP, CIDR, or (Phase 2) MAC.
  Each client in exactly one group; each group has its own subscriptions,
  rules, and enable toggle. Default group covers unknown clients.
- **Pause blocking:** global or per-group "disable for N minutes",
  prominent in UI and API.
- Every decision logs *which rule and which list* matched.

## Storage

- `internal/store` defines interfaces (`ConfigStore`, `QueryLogStore`,
  `ClientStore`, …) with SQLite and Postgres implementations selected at
  runtime by config (`storage.driver` + `storage.dsn`).
- Versioned per-dialect SQL migrations, embedded, applied at startup.
- **Query log:** buffered writes, batch flush (1s or 1000 rows).
  Retention pruner (default 90 days). Privacy modes: full logging,
  anonymized client IPs, or no logging.
- **Stats:** hourly rollup tables maintained by a background job;
  dashboards read rollups, not raw log scans. Raw log remains queryable
  for detailed search.
- **Not in the DB:** DNS cache and compiled filter trie (memory only;
  downloaded blocklist files cached on disk to avoid re-download on
  restart).

## API & Web Dashboard

- **REST `/api/v1`**, JSON, embedded OpenAPI spec. The UI uses only the
  public API — no private endpoints. Resources: `/queries` (search + SSE
  live tail), `/stats/*`, `/filters/lists|rules|groups`, `/clients`,
  `/records`, `/upstreams`, `/settings`, `/blocking` (pause/resume),
  `/sync/*` (Phase 1.5).
- **Auth:** first-run setup wizard creates admin account. Session cookie
  for SPA; scoped revocable API tokens for scripts. argon2id password
  hashing. Optional TOTP 2FA.
- **Dashboard pages:**
  - *Home:* stat tiles (queries, blocked %, clients, cache hit rate), 24h
    allowed-vs-blocked timeline, top domains/blocked/clients.
  - *Query log:* live tail, filtering (client/type/decision/domain),
    one-click block/allow per row, inline "why blocked" (rule + list).
  - *Filtering:* lists, rules, groups, clients; always-visible pause
    button.
  - *Local DNS:* record management.
  - *Settings:* upstreams, cache, logging/privacy, storage.
  - Nav anticipates Phase 2+ (DHCP, Zones sections).

## Configuration & Deployment

- **Bootstrap YAML** only for pre-DB needs: listen addresses, storage
  driver/DSN, data dir. Everything else lives in the DB, managed via
  UI/API with sane first-run defaults. `DNSAUR_*` env overrides for all
  bootstrap settings.
- **Docker:** distroless, multi-arch (amd64/arm64), GHCR; documented
  compose file with `/data` volume. Host-network/`NET_ADMIN` guidance
  documented for Phase 2 DHCP (broadcasts don't traverse bridge networks).
- **Bare metal:** GoReleaser artifacts (deb/rpm/tarball), systemd unit,
  `setcap cap_net_bind_service` for unprivileged port 53.
- **First run:** start → web UI wizard (admin account, upstreams, starter
  blocklists) → point router DNS at it. Under 5 minutes, no file editing.
- **Lifecycle:** config changes hot-reload (no restarts); SIGTERM drains
  in-flight queries; DNS keeps serving even if API/UI errors — resolution
  degrades last.

## High Availability — Config Sync (Phase 1.5)

Primary/replica; every instance fully self-contained on its own SQLite.

- One **primary** = source of truth for configuration. **Replicas** join
  via one-time join token (generated in primary UI, pasted into replica
  setup wizard). Token exchange over HTTPS.
- **Syncs:** settings, upstreams, blocklist subscriptions, rules, groups,
  clients, local DNS records. **Does not sync:** query logs,
  instance-local identity (listen addresses, hostname).
- **Mechanism:** config writes bump a monotonic version; replicas hold a
  long-poll/SSE connection to the primary and pull a config snapshot on
  change, plus periodic full-sync safety net. Applies via the standard
  hot-reload path within seconds.
- **Editing UX:** replica UIs transparently proxy settings writes to the
  primary — either UI works. Primary unreachable → replica settings go
  read-only with a banner.
- **Failure:** primary down → replicas serve DNS indefinitely on
  last-known config; settings frozen until primary returns or a replica
  is promoted (one-click manual promotion; no automatic election, no
  split-brain).
- **Observability:** primary dashboard shows cluster health + federated
  stats via replica APIs. Query logs stay per-instance; merged log view
  is roadmap.
- Client-side failover is native to DNS: DHCP hands out both instances'
  IPs.
- Phase 1 is built sync-aware (config versioning + hot-reload), so 1.5 is
  additive.

## Error Handling

Guiding rule: **DNS must not die.**

- Upstreams failing → failover to healthy ones → serve-stale → SERVFAIL
  only as last resort. Health visible on dashboard.
- Storage failing → resolution continues (trie + cache are in-memory);
  log buffer drops-oldest with surfaced warning; DB-dependent API
  endpoints return 503.
- Blocklist refresh failing → keep previous compiled list, retry with
  backoff, stale-list warning in UI.
- Bad config via API → validated + rejected at write time; running config
  is always last-known-good.
- Pipeline panics → recovered per-query (SERVFAIL + log); one poisoned
  query never takes down the server.

## Testing

- **Unit:** pipeline stages against the `Handler` interface with fakes;
  filter engine against fixture lists (formats, precedence, trie);
  store tests run against both SQLite and Postgres (testcontainer,
  skipped without Docker).
- **Integration:** full server on ephemeral ports, real queries via
  `miekg/dns` client; assert block/allow/cache/failover end-to-end. Mock
  upstream simulates timeouts, truncation, bad responses.
- **API:** handler tests against the OpenAPI contract.
- **CI:** GitHub Actions — golangci-lint, race-detector tests, multi-arch
  build. Green from first commit.

## Out of Scope (for now)

- Multi-master sync / CRDTs (write-proxying gives the same UX).
- Shared/distributed DNS cache across instances.
- Full recursion from root servers (forwarding only until at least
  Phase 4; revisit then).
- Merged cross-instance query-log view (roadmap).
- App/plugin system (Technitium-style) — revisit after Phase 4.
