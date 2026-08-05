#!/usr/bin/env bash
# Builds and runs the real dnsaur binary — the actual embedded SPA, not the
# Vite dev server — on loopback ports that won't collide with a developer's
# own `pnpm dev` + `dnsaur` pair (which use 5280/8380 per web/README.md).
# Used exclusively as Playwright's webServer (see ../playwright.config.ts);
# not meant to be run by hand.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
work_dir="$(mktemp -d)"
bin="$work_dir/dnsaur-e2e"

CGO_ENABLED=0 go build -C "$repo_root" -o "$bin" ./cmd/dnsaur

export DNSAUR_HTTP_LISTEN="127.0.0.1:8381"
# >1024 so this doesn't need root/CAP_NET_BIND_SERVICE — the smoke test
# never resolves anything over DNS, it only needs the server to start.
export DNSAUR_DNS_LISTEN="127.0.0.1:8354"
export DNSAUR_DATA_DIR="$work_dir/data"
export DNSAUR_LOG_LEVEL="error"

# -config points at a path that's guaranteed not to exist, so a stray
# dnsaur.yaml at the repo root (if a developer has one for their own local
# testing) can never leak unrelated settings into this run — config.Load
# tolerates a missing file and falls back to defaults + the env overrides
# above.
exec "$bin" -config "$work_dir/unused-dnsaur.yaml"
