#!/usr/bin/env bash
# Builds and runs the real dnsaur binary — the actual embedded SPA, not the
# Vite dev server — on loopback ports that won't collide with a developer's
# own `pnpm dev` + `dnsaur` pair (which use 5280/8380 per web/README.md).
# Used exclusively as Playwright's webServer (see ../playwright.config.ts);
# not meant to be run by hand.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
# A stable path wiped up front, not `mktemp -d`: line 27 `exec`s into the
# server, so no EXIT trap could ever fire to clean up — a fresh temp dir per
# run would leak ~30 MB (built binary + SQLite DB) on every `pnpm test:e2e`.
# Wiping here still guarantees the fresh-instance-per-run the smoke test
# needs (it starts from the first-run setup wizard, so any leftover admin
# account would break it).
# Namespaced by uid: /tmp is world-writable and sticky, so an unqualified
# path already owned by another user would make the rm -rf below fail (and
# abort the script under `set -e`) on a shared host.
work_dir="${TMPDIR:-/tmp}/dnsaur-e2e-$(id -u)"
rm -rf "$work_dir"
mkdir -p "$work_dir"
bin="$work_dir/dnsaur-e2e"

CGO_ENABLED=0 go build -C "$repo_root" -o "$bin" ./cmd/dnsaur

export DNSAUR_HTTP_LISTEN="127.0.0.1:8381"
# >1024 so this doesn't need root/CAP_NET_BIND_SERVICE — the smoke test
# never resolves anything over DNS, it only needs the server to start.
export DNSAUR_DNS_LISTEN="127.0.0.1:8354"
export DNSAUR_DATA_DIR="$work_dir/data"
export DNSAUR_LOG_LEVEL="error"

# -config points at an empty file of our own, so a stray dnsaur.yaml at the
# repo root (if a developer has one for their own local testing) can never
# leak unrelated settings into this run: everything comes from defaults plus
# the env overrides above. The file has to exist — a named path that is
# missing is refused at startup, deliberately, so a typo in -config cannot
# start a server on defaults with nothing saying so.
config="$work_dir/dnsaur-e2e.yaml"
: > "$config"
exec "$bin" -config "$config"
