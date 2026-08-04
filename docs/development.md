# Development

See also: [`README.md`](../README.md) · [`docs/architecture.md`](architecture.md)

## Prerequisites

- **Go 1.26** (matches `go.mod` and CI).
- **Docker**, only if you want to run the Postgres-backed store tests —
  they use `testcontainers-go` to spin up a real Postgres and are skipped
  automatically when Docker isn't available.
- **golangci-lint v2.12** for linting (matches the version pinned in CI).

## Build

```sh
go build ./cmd/dnsaur
```

## Test

```sh
go test ./...          # full suite; Postgres store tests skip without Docker
go test -race ./...    # what CI runs
```

## Lint

```sh
golangci-lint run
go vet ./...
```

Linters enabled: `staticcheck`, `errcheck`, `ineffassign`, `unused`,
`misspell` (see `.golangci.yml`).

## CI

CI runs on Forgejo Actions (`.forgejo/workflows/ci.yml`), with GitHub
serving as a passive mirror (no CI runs there — Forgejo is the source of
truth for build status). Two jobs, both triggered on push to `main` and on
pull requests:

- **`checks`** (runner label `tiny-no-docker`): checkout, Go 1.26 setup,
  `golangci-lint` v2.12, `go vet ./...`, and a `CGO_ENABLED=0` build.
- **`test`** (runner label `medium`): checkout, Go 1.26 setup,
  `go test -race ./...`.

## Commit and PR conventions

This repo uses [Conventional Commits](https://www.conventionalcommits.org/)
(`feat:`, `fix:`, `docs:`, `chore:`, etc.) for **both** individual commit
messages and pull request titles.

## Repo layout

See [`docs/architecture.md`](architecture.md) for the full package map and
middleware pipeline. Short version: `cmd/dnsaur` is the entry point,
`internal/*` holds everything else, and `web/` (planned) will hold the
embedded React dashboard.

## Specs and plans

Design specs and implementation plans for each phase of work live under
`docs/superpowers/` (`docs/superpowers/specs/`, `docs/superpowers/plans/`).
That's where the "why" behind a feature and the step-by-step plan that
built it are recorded — check there before re-deriving intent from code.
