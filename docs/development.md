# Development

See also: [`README.md`](../README.md) · [`docs/architecture.md`](architecture.md)

## Prerequisites

- **Go 1.26** (matches `go.mod` and CI).
- **Node 20+ and pnpm** (`corepack enable`), for the web dashboard —
  see [`web/README.md`](../web/README.md) for its own dev workflow,
  scripts, and Playwright smoke test.
- **Docker**, only if you want to run the Postgres-backed store tests —
  they use `testcontainers-go` to spin up a real Postgres and are skipped
  automatically when Docker isn't available.
- **golangci-lint v2.12** for linting (matches the version pinned in CI).

## Build

```sh
cd web && pnpm install && pnpm build && cd ..   # dashboard first — go:embed needs web/dist
go build ./cmd/dnsaur
```

`go build` also works with no prior `pnpm build` (`web/dist/.gitkeep` keeps
the embed from failing on a fresh checkout), but the binary then serves an
empty dashboard — fine for API-only work, not for anything dashboard-facing.

## Test

```sh
go test ./...          # full suite; Postgres store tests skip without Docker
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

- **`web`** (runner label `tiny-no-docker`): checkout, Node 20 + pnpm
  setup, `oxlint`, `format:check`, `typecheck`, `pnpm test` (Vitest), then
  `pnpm build` — the real `web/dist` is uploaded as an artifact for the
  jobs below.
- **`checks`** (runner label `tiny-no-docker`, needs `web`): checkout, Go
  1.26 setup, downloads the `web-dist` artifact into `web/dist`,
  `golangci-lint` v2.12, `go vet ./...`, and a `CGO_ENABLED=0` build (so
  the binary embeds the real dashboard, not the empty placeholder).
- **`test`** (runner label `medium`, needs `web`): checkout, Go 1.26 setup,
  downloads `web-dist`, `go test -race ./...`.
- **`e2e`** (runner label `medium`, needs `web`, `continue-on-error:
  true`): checkout, Go + Node/pnpm setup, downloads `web-dist`, installs
  Chromium (`npx playwright install --with-deps chromium`), runs the
  Playwright smoke test (`web/e2e/smoke.spec.ts`) against a real built
  `dnsaur` binary. Best-effort, not a hard merge gate — a runner without
  reliable network access to fetch Chromium shouldn't block everything
  else.

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

## Specs and plans

Design specs and implementation plans for each phase of work live under
`docs/superpowers/` (`docs/superpowers/specs/`, `docs/superpowers/plans/`).
That's where the "why" behind a feature and the step-by-step plan that
built it are recorded — check there before re-deriving intent from code.
