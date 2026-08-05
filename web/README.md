# dnsaur web

The dnsaur dashboard: a React SPA embedded into the `dnsaur` Go binary and
served by `internal/api` alongside the REST API.

## Stack

- [Vite](https://vite.dev) 8 + React 19 + TypeScript (strict)
- [Tailwind CSS](https://tailwindcss.com) 4 via `@tailwindcss/vite`
- [`@e412/rnui-react`](https://www.npmjs.com/package/@e412/rnui-react) +
  [`@e412/rnui-themes`](https://www.npmjs.com/package/@e412/rnui-themes) for
  components, themed by `src/styles/dnsaur-theme.css` (a custom indigo/cyan
  token set, see that file for the full contract)
- [TanStack Query](https://tanstack.com/query) for server state, React
  Router for client routing
- [oxlint](https://oxc.rs/docs/guide/usage/linter.html) + `oxfmt` for
  lint/format (not eslint/prettier), [Vitest](https://vitest.dev) +
  Testing Library for tests

## Prerequisites

- Node 20+
- pnpm (`corepack enable` or install directly)

## Develop

Two processes, in two terminals, from the repo root:

```sh
# Terminal 1 — the API/DNS server the dashboard talks to.
go build ./cmd/dnsaur
DNSAUR_HTTP_LISTEN=127.0.0.1:8380 ./dnsaur

# Terminal 2 — the dashboard itself, with hot reload.
cd web
pnpm install
pnpm dev       # http://127.0.0.1:5280, proxies /api to http://127.0.0.1:8380
```

Open `http://127.0.0.1:5280` — Vite's dev server proxies every `/api`
request to the `dnsaur` process on `:8380` (see `vite.config.ts`), so the
dashboard has a live backend without needing a production build on every
change. `DNSAUR_DNS_LISTEN` defaults to `:53`, which needs root/
`CAP_NET_BIND_SERVICE` to bind — for local dashboard dev you don't care
about DNS resolution itself, so either run as root, or also set
`DNSAUR_DNS_LISTEN=127.0.0.1:8353` (or similar) to avoid that entirely.

## Scripts

| Command | Purpose |
|---|---|
| `pnpm dev` | Vite dev server (port 5280, proxies `/api`) |
| `pnpm build` | Typecheck (`tsc -b`) + production build to `dist/` |
| `pnpm preview` | Preview the production build locally |
| `pnpm typecheck` | Typecheck only, no emit |
| `pnpm lint` | `oxlint src/` |
| `pnpm format` / `pnpm format:check` | `oxfmt --write` / `--check` on `src/` |
| `pnpm test` / `pnpm test:watch` | Vitest (component tests), once or in watch mode |
| `pnpm test:e2e` | Playwright smoke test against the real embedded build — see below |

Before committing, both gates need to pass:

```sh
cd web && pnpm test && pnpm oxlint src/ && pnpm format:check && pnpm typecheck && pnpm build
go test -race ./... && ~/go/bin/golangci-lint run ./...
```

## Build + embed

`pnpm build` produces `web/dist/`, which `web/embed.go` embeds via
`//go:embed all:dist` and exposes as `web.Dist() fs.FS`. The Go binary
serves it through `internal/api.StaticHandler` (mounted on non-`/api`
paths) with an `index.html` fallback for client-side routes. `go build`
works even without a prior `pnpm build` — `dist/.gitkeep` keeps the
directory present so the embed never fails — but the served app will be
empty (backend-only build) until real assets exist there. CI builds the
real assets before the Go build runs.

See [`../docs/development.md`](../docs/development.md) for the Go side.

## End-to-end smoke test (Playwright)

`e2e/smoke.spec.ts` drives the *real* embedded build — not the Vite dev
server — through the app's critical path: first-run setup (create the
admin account), log out and back in through the real login page, add a
local DNS record and see it listed, then toggle dark mode and confirm it
survives a reload. `playwright.config.ts`'s `webServer` builds the actual
`dnsaur` binary (`e2e/run-server.sh`, `CGO_ENABLED=0 go build ./cmd/dnsaur`)
and runs it against a fresh temp SQLite DB on `127.0.0.1:8381` (DNS on
`127.0.0.1:8354`) — ports distinct from the dev workflow above so this can
run alongside a `pnpm dev` session. Every run starts from a clean, unset-up
instance (`reuseExistingServer: false` and a fresh temp data dir each time),
since the test's first step *is* the first-run setup wizard.

```sh
pnpm build          # make sure dist/ reflects current source first
npx playwright install --with-deps chromium   # once, downloads the browser
pnpm test:e2e
```

This needs a Chromium binary and Go toolchain on `PATH`; it is **not**
part of the `pnpm test`/`pnpm build` gate above and does not run inside the
`web` CI job (see `.forgejo/workflows/ci.yml`'s `e2e` job, which installs
Chromium itself on a `medium` runner) — run it locally before a release, or
rely on CI's `e2e` job, which is best-effort (network access to download
Chromium isn't guaranteed on every runner) rather than a hard merge gate.
