# dnsaur Web Dashboard — Design Spec

**Date:** 2026-08-04
**Status:** Approved
**Depends on:** Phase 1 blocking resolver (merged), Phase 1 REST API + auth (merged, PR #3)

## What & Why

A web dashboard for dnsaur: a React SPA that turns the existing `/api/v1`
REST API into a usable interface, making dnsaur a true Pi-hole/AdGuard
replacement with a real UI rather than a curl-only control plane. It is the
final piece of the "Phase 1 product" — the resolver, the API, and now the
face.

The dashboard is a **control plane**: it never sits in the DNS query path.
If the dashboard or its API is down, resolution is unaffected.

## Decisions Made

| Decision | Choice |
|---|---|
| Framework | React 19 + Vite 8 + TypeScript (strict) |
| Component library | rnui (`@e412/rnui-react` + `@e412/rnui-themes`) from npm — the author's shadcn-in-library-form; Base UI + Tailwind v4, themeable via `data-theme` |
| Theme | Custom `dnsaur` rnui theme (OKLch token blocks) with light + dark mode and a toggle (system-aware, persisted) — NOT one of the 8 built-in presets |
| Package manager | pnpm (matches rnui) |
| Scope | Full parity — every API surface (setup, login, home, query log, filtering, records, settings, account/security) |
| API types | Hand-maintained TypeScript types mirroring the API + a thin typed `fetch` wrapper. No OpenAPI/Go codegen. |
| Server state | TanStack Query (per-resource hooks, mutation-invalidation, interval refetch for stats) |
| Auth | HttpOnly session cookie (no token in JS); `GET /auth/me` gate |
| Routing | React Router v7, nested layout, auth boundary |
| Query log | Live tail via SSE (`/queries/tail`), toggleable pause/play; filters switch to paged `GET /queries` |
| Lint / format / test | oxlint + oxfmt + Vitest (matches rnui) |
| Design process | Screen design produced via the design skills (`frontend-design`, `arrange`, `typeset`, `colorize`, `animate`, `polish`, `critique`, `audit`) — not hand-styled ad hoc |
| Deployment | Built to `web/dist/`, embedded in the Go binary via `go:embed`; single artifact |

## Architecture & Build Integration

React 19 + Vite 8 + TypeScript SPA in `web/`, built to `web/dist/`, embedded
in the Go binary via `go:embed`. One self-contained artifact — no separate
frontend to deploy.

**Serving (Go side, part of this milestone).** The API server currently 404s
non-`/api/` paths. Add a static handler:
- `/api/*` → existing API handlers (unchanged).
- All other paths → embedded SPA. Hashed asset filenames get long-lived
  cache headers; `index.html` is `no-cache`. Unknown non-asset paths fall
  back to `index.html` so client-side routes (`/settings`, etc.) reload
  correctly.
- If the embedded `dist` is empty (backend-only build), the static handler
  is a no-op and the API still runs.

**Dev workflow (non-default ports).**
- Go API on `127.0.0.1:8380` (`DNSAUR_HTTP_LISTEN=127.0.0.1:8380`).
- Vite dev server on `127.0.0.1:5280`, proxying `/api` → `:8380`.
- Neither is a framework default. Documented in `web/README.md`.
- Production always uses the embedded build served by the API server on the
  operator's configured `HTTPListen`.

**Toolchain.** pnpm, Vite 8, TS strict, `@e412/rnui-react` +
`@e412/rnui-themes` from npm. The Go build depends on `web/dist` existing; CI
builds the SPA first (Node step on the `tiny-no-docker` Forgejo runner) then
the Go binary, so `go:embed` always has real assets.

## Data Layer, Auth & Routing

**Typed API client.** `web/src/api/types.ts` hand-mirrors the API's
request/response shapes (organized per resource). `web/src/api/client.ts` is
a thin typed `fetch` wrapper: base URL, `credentials: 'include'`, JSON
headers, and a typed `ApiError` (status + `{error}` body) thrown on non-2xx.
No codegen. API and frontend live in one repo, so a contract change touches
both together.

**Auth on the HttpOnly session cookie.** The SPA stores no token in JS; the
API's session cookie travels automatically with `credentials: 'include'`.
XSS cannot exfiltrate it.

**Server state via TanStack Query.** Each resource gets a typed hook
(`useSettings`, `useClients`, `useStatsOverview`, `useQueries`, …) over
`useQuery`/`useMutation`. Mutations invalidate the relevant query keys so
writes reflect immediately; stats/overview use interval background refetch.
One shared query-client config centralizes retry, stale time, and a global
error handler.

**Auth flow.** On load, `GET /auth/me`:
- 401 **and** `GET /setup` reports setup-required → **setup wizard**.
- 401 otherwise → **login**.
- 200 → the authenticated app.
A client interceptor watches every response: mid-session `401` → redirect to
login with a "session expired" note; `403` (read-scope token attempting a
write) → toast, no navigation. Login/logout are mutations; logout clears the
query cache.

**Routing.** React Router v7. An auth boundary wraps the app shell (rnui
`Sidebar` + header). Pages are child routes: `/` (home), `/queries`,
`/filtering`, `/dns`, `/settings`, `/account`. Deep-linkable; the
`index.html` fallback makes reloads work.

## Screens

Shared shell: sidebar nav + header (global pause toggle, theme switch,
account menu, ⌘K command palette). Functional design below; visual design
(layout rhythm, dnsaur theme, motion) is produced via the design skills.

**Auth (pre-shell).**
- **Setup wizard** (first-run only): create admin (username + password with
  strength hint) → choose starter blocklists + upstreams (sensible
  defaults) → done. rnui `Stepper` + `Form`.
- **Login**: username/password; if the account has TOTP, a second step for
  the 6-digit code (rnui `InputOTP`). Errors as inline field messages.

**Home / Dashboard (`/`).**
- Stat tiles (rnui `StatCard`): total queries, blocked %, cache hit rate,
  active clients — selectable window (24h default).
- Timeline chart (rnui `charts`): allowed vs blocked over time.
- Three "top" compact tables: top domains, top blocked, top clients; each
  row's domain has a quick block/allow action.
- Health strip: instance up, upstreams healthy, lists last-refreshed.

**Query log (`/queries`)** — the showpiece.
- Live tail via SSE with a pause/play toggle; a virtualized `data-grid`
  streams rows (time, client, domain, type, decision badge, upstream,
  latency).
- Filter bar (rnui `filters`): client, type, decision, domain search —
  applying any filter switches from the stream to paged `GET /queries`.
- Per-row: one-click block/allow; "why?" (matched rule + list) in a
  `Drawer`. Auto-reconnect on stream drop with a subtle indicator; dropped
  streams never lose already-rendered rows.

**Filtering (`/filtering`)** — tabbed.
- **Lists**: subscribed blocklists (URL, kind, enabled, entry count,
  last-refreshed); add/remove/toggle; manual "Refresh now" (202 + progress
  toast).
- **Rules**: manual allow/block + regex per group; add/delete.
- **Groups & Clients**: groups with enable toggles; clients (name, IP/CIDR
  matcher, group); declarative "which lists apply to this group".
- Pause control (global or per-group, N minutes) lives here and in the
  shell header.

**Local DNS (`/dns`).** Records table (name, type, value, TTL); add/edit/
delete via a `Sheet` form; type-aware validation mirroring the API
(A/AAAA/CNAME/TXT).

**Settings (`/settings`).** Grouped forms — upstreams + strategy, cache,
logging/privacy, storage (read-only info). Restart-required settings
(`cache.*`, `lists.refresh_hours`) are labeled as such; other writes
hot-reload live.

**Account (`/account`).** Change password; TOTP enable (QR + confirm) /
disable; API tokens — create (name + read/write scope, plain token shown
once in a copy dialog), list, revoke.

**Cross-cutting.** Toasts (rnui `sonner`) on all mutations; command palette
(rnui `command`, ⌘K) for jump-to-page + quick actions (pause, block a
domain); empty states (rnui `EmptyState`); skeleton loaders; responsive down
to tablet.

## Error Handling

- **Network/API:** typed client throws `ApiError`; TanStack Query surfaces
  it. Queries render an inline error state with retry (rnui `Alert` /
  `EmptyState`); mutations toast `{error}` and leave the form editable. A
  React error boundary per route stops one broken page from blanking the
  app.
- **Auth transitions:** mid-session 401 → login + "session expired"; 403 →
  toast, no navigation.
- **Control-plane framing:** if the API is unreachable the dashboard shows a
  "can't reach dnsaur" banner and keeps retrying; nothing it does affects
  live resolution.
- **SSE:** live tail auto-reconnects with backoff; indicator shows
  streaming / reconnecting / paused; a drop never loses rendered rows.
- **Optimistic UI** only where safe (pause toggle, block/allow from the
  query log) with rollback on failure; everything else confirms after the
  server responds.

## Testing

- **Unit/component:** Vitest + React Testing Library. Hooks and interactions
  tested against **MSW** mocking `/api/v1` (real fetch paths, no real
  backend). Covers: the auth gate (setup/login/authed branches); one
  representative page per pattern (a form page, the query log with a mocked
  SSE stream, a table-CRUD page); client error/401/403 handling.
- **Design/quality gate:** `critique` + `audit` (accessibility/perf) pass on
  the built UI before the milestone closes.
- **E2E smoke (thin):** one Playwright test against the real embedded build
  served by the Go binary — setup → login → add a record → see it listed —
  proving embed + serving + auth cookie work together. Runs in CI where a
  browser is available, else documented as a local gate.
- **CI:** oxlint + oxfmt check + TS typecheck + Vitest on the Node step;
  Playwright smoke where a browser is available. The Go build consumes
  `web/dist`.

## Out of Scope (this milestone)

- Theme picker exposing all 8 rnui presets (custom dnsaur theme only for
  now).
- Multi-instance / HA cluster UI (Phase 1.5 — a health strip stub only).
- DHCP, encrypted-DNS, and zone-management screens (later phases; nav
  anticipates them).
- i18n / localization.
- Mobile-phone-optimized layout (responsive to tablet; phone is best-effort).
