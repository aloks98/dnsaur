# Development

See also: [`README.md`](../README.md) · [`docs/architecture.md`](architecture.md)

## Prerequisites

- **Go 1.26** (matches `go.mod` and CI).
- **Node `^20.19.0 || >=22.12.0`** (Vite 8's engines range) **and pnpm**
  (`corepack enable`), for the web dashboard. CI runs Node 24; Node 20 is
  past end-of-life, so prefer 22+ locally. See
  [`web/README.md`](../web/README.md) for the dashboard's own dev
  workflow, scripts, and Playwright smoke test.
- **Docker**, only if you want to run the Postgres-backed tests.
  `internal/store`, `internal/zones` and `internal/app` each use
  `testcontainers-go` to spin up a real Postgres and skip automatically when
  Docker isn't available; set `DNSAUR_TEST_POSTGRES_DSN` to point them at one
  you already run instead. `internal/store` runs its whole suite on both
  drivers; `internal/zones` and `internal/app` run only the cases where
  connection or transaction behaviour could differ — the reload, the transfer
  and stub installs, the concurrent refreshes, and in `internal/app` the
  routing-table windows and the cache invalidation that hangs off them.
- **golangci-lint v2.12** for linting (matches the version pinned in CI).

## Build

```sh
cd web && pnpm install && pnpm build && cd ..   # dashboard first — go:embed needs web/dist
go build ./cmd/dnsaur
```

`go build` also works with no prior `pnpm build` (`web/dist/.gitkeep` keeps
the embed from failing on a fresh checkout), but the binary then serves
API-only — every dashboard route returns 404 until real assets exist in
`web/dist`. Fine for backend work, not for anything dashboard-facing.

## Test

```sh
go test ./...          # full suite; the Postgres halves skip without Docker
go test -race ./...    # what CI runs

cd web && pnpm test    # dashboard component tests (Vitest)
cd web && pnpm test:e2e  # Playwright smoke test against the real embedded build
```

## Lint

```sh
golangci-lint run
go vet ./...

cd web && pnpm oxlint src/ && pnpm format:check && pnpm typecheck
```

Linters enabled: `staticcheck`, `errcheck`, `ineffassign`, `unused`,
`misspell` (see `.golangci.yml`). The dashboard uses `oxlint`/`oxfmt`, not
eslint/prettier.

## CI

CI runs on Forgejo Actions (`.forgejo/workflows/ci.yml`), with GitHub
serving as a passive mirror (no CI runs there — Forgejo is the source of
truth for build status). Jobs, triggered on push to `main` and on pull
requests:

- **`web`** (runner label `tiny-no-docker`): checkout, Node 24 + pnpm
  setup, `pnpm lint`, `format:check`, `typecheck`, `pnpm test` (Vitest),
  then `pnpm build` — the real `web/dist` is uploaded as an artifact for the
  jobs below.
- **`checks`** (runner label `tiny-no-docker`): checkout, Go 1.26 setup,
  `golangci-lint` v2.12, `go vet ./...`, and a `CGO_ENABLED=0` build.
- **`test`** (runner label `medium`): checkout, Go 1.26 setup,
  `go test -race ./...`.
- **`e2e`** (runner label `medium`, needs `web`, `continue-on-error:
  true`): checkout, Go + Node/pnpm setup, downloads `web-dist`, installs
  Chromium (`npx playwright install --with-deps chromium`), runs the
  Playwright smoke test (`web/e2e/smoke.spec.ts`) against a real built
  `dnsaur` binary, and always uploads `web/playwright-report` +
  `web/test-results` (traces) so a failure leaves something to look at.
  Best-effort, not a hard merge gate — a runner without reliable network
  access to fetch Chromium shouldn't block everything else.

`checks` and `test` deliberately **do not** depend on `web`: the Go side
builds and tests fine against the committed `web/dist/.gitkeep` placeholder
(no Go test asserts on real dashboard assets), so a frontend hiccup can
never silently skip backend verification on a PR. Only `e2e` genuinely
consumes the `web-dist` artifact.

## Commit and PR conventions

This repo uses [Conventional Commits](https://www.conventionalcommits.org/)
(`feat:`, `fix:`, `docs:`, `chore:`, etc.) for **both** individual commit
messages and pull request titles.

## Repo layout

See [`docs/architecture.md`](architecture.md) for the full package map and
middleware pipeline. Short version: `cmd/dnsaur` is the entry point,
`internal/*` holds everything else, and `web/` holds the React dashboard —
built independently (`pnpm build`) and embedded into the Go binary via
`//go:embed` (see `web/embed.go`).

## Specs

Design specs for each phase of work live under `docs/superpowers/specs/`.
That's where the "why" behind a feature is recorded — check there before
re-deriving intent from code. The step-by-step implementation plans that
built each milestone were removed once executed; they remain in git
history before 2026-09-09 if one is ever needed.
