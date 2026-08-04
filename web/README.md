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

```sh
pnpm install
pnpm dev       # http://127.0.0.1:5280, proxies /api to http://127.0.0.1:8380
```

Run the Go server separately (`go run ./cmd/dnsaur`, listening on
`:8380` per its own config) so the dashboard has a live API to talk to.

## Scripts

| Command | Purpose |
|---|---|
| `pnpm dev` | Vite dev server (port 5280, proxies `/api`) |
| `pnpm build` | Typecheck (`tsc -b`) + production build to `dist/` |
| `pnpm preview` | Preview the production build locally |
| `pnpm typecheck` | Typecheck only, no emit |
| `pnpm lint` | `oxlint src/` |
| `pnpm format` / `pnpm format:check` | `oxfmt --write` / `--check` on `src/` |
| `pnpm test` / `pnpm test:watch` | Vitest, once or in watch mode |

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
