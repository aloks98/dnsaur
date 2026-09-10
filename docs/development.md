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
  routing-table windows and the cache invalidation that hangs off them. The
  container, the migrated template database and the per-test copy taken from
  it all live in `internal/storetest`, so a package that needs Postgres calls
  `storetest.Start` from its `TestMain` and `storetest.Database` from its
  fixture rather than growing a fourth copy of the scaffolding.
  `DNSAUR_TEST_REQUIRE_POSTGRES=1` turns "no Postgres, its halves will skip"
  from a line on stderr into a failed run — set it where Postgres is not
  optional, which is what CI does.
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

### Container image

`Dockerfile` at the repo root builds both halves itself — Node 24 for
`web/dist`, then Go 1.26 with `CGO_ENABLED=0` — so it needs neither a local
toolchain nor a prior `pnpm build`:

```sh
docker build --build-arg VERSION="$(git describe --tags --always)" -t dnsaur:local .
```

Both architectures at once, the way a release builds it. The Go stage stays
on the native toolchain and cross-compiles (`--platform=$BUILDPLATFORM` plus
`GOARCH`), so arm64 costs a compile rather than an emulated one:

```sh
docker buildx build --platform linux/amd64,linux/arm64 -t dnsaur:local .
```

The final stage is `gcr.io/distroless/static-debian12:nonroot`: no shell and
no package manager, so `docker exec … sh` is not available for poking
around. [`docs/configuration.md`](configuration.md) covers running it —
what the entrypoint deliberately does *not* pass, and why port 53 is
published rather than bound.

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

`oxlint` 1.82 added the React Compiler rules — `react(purity)`,
`react(refs)`, `react(set-state-in-effect)`, `react(immutability)`,
`react(incompatible-library)` — to the `correctness` category
`.oxlintrc.json` treats as errors. The dashboard satisfies all of them
except one, which is not about our code: `react(incompatible-library)` is a
statement about TanStack Table, whose `useReactTable()` returns functions
React Compiler cannot memoize safely. The rule has no options, and the
query log's grid is rnui's `DataGrid`, whose props *are* a
`Table<TData>` — so there is nothing to change short of replacing the
design system's grid. It also costs nothing today: this project does not
run the React Compiler, so the memoization the rule is warning about is not
happening either way.

There is one `// oxlint-disable-next-line react/incompatible-library`
comment above that `useReactTable(` call in `src/pages/queries.tsx`, and
nothing in the config: a line directive names the one call it excuses and
disappears with it, where a config override would go on silently covering
whatever that file grew next. Deleting the comment puts the error straight
back, which is the point — it is load-bearing, not decoration.

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
  `golangci-lint` v2.12, `go vet ./...`, and a `CGO_ENABLED=0` build. The Go
  build and module caches are cached explicitly, keyed on the `go.sum` hash
  with a `restore-keys` fallback, rather than through `setup-go`'s built-in
  cache — that one has no fallback, so a dependency bump misses it outright
  and leaves `golangci-lint` compiling the whole module from nothing on the
  smallest runner.
- **`test`** (runner label `medium`): checkout, Go 1.26 setup,
  `go test -race -cover -count=1 ./...` with
  `DNSAUR_TEST_REQUIRE_POSTGRES=1`, so a run where the Postgres halves
  skipped fails instead of going green on a stderr line nobody reads.
  `-count=1` because a race suite served from the build cache detects
  nothing — `go test` replays the previous PASS without running the binary,
  and an intermittent race never gets the chance to show. The per-package
  coverage lines are written to the job summary; nothing gates on the
  number.
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

## Release

`.goreleaser.yaml` describes the bare-metal artifacts: `linux/amd64` and
`linux/arm64` builds of `./cmd/dnsaur`, a `.tar.gz` each, and a `.deb` and
`.rpm` each. The packages install the binary to `/usr/bin/dnsaur`,
`contrib/dnsaur.service` to `/usr/lib/systemd/system/`, and
`contrib/dnsaur.example.yaml` to `/etc/dnsaur/dnsaur.yaml` as a conffile, so
an upgrade never overwrites an edited config. The unit runs under
`DynamicUser=` with `StateDirectory=dnsaur` and gets port 53 from
`AmbientCapabilities=CAP_NET_BIND_SERVICE` — no account to create, no
`setcap` on the binary. Only the tarball path needs
`setcap cap_net_bind_service=+ep ./dnsaur`, since it comes with no unit.

Check and exercise it locally. `pnpm build` first, or the packaged binaries
embed an empty `web/dist` and serve API-only — the release workflow builds
the dashboard for the same reason:

```sh
go run github.com/goreleaser/goreleaser/v2@v2.18.1 check
cd web && pnpm install && pnpm build && cd ..
go run github.com/goreleaser/goreleaser/v2@v2.18.1 release --snapshot --clean --skip=publish
```

**Nothing is published yet, on purpose.** Pushing a `v*` tag runs
`.forgejo/workflows/release.yml`, which does exactly that snapshot build
plus a `docker buildx build --platform linux/amd64,linux/arm64`, uploads the
artifacts to the run and stops there. That keeps both configs honest — a
broken one is red on the tag rather than on the first real release — while
leaving the decision to publish, and where to, open. Turning it on means
dropping `--snapshot --skip=publish`, giving the job a token, and adding a
registry login and `--push` to the image step.

The image is built by `docker buildx`, not by GoReleaser, and
`.goreleaser.yaml` has no `dockers`/`dockers_v2` section. GoReleaser builds
images in a temporary context holding only the release artifacts, so it can
drive a Dockerfile that copies a prebuilt binary and nothing else — while
`Dockerfile` builds the dashboard and the binary from source, which is what
`docker build .` and CI use. One Dockerfile both paths share beats a
GoReleaser stanza plus a second one that quietly diverges from it.

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
