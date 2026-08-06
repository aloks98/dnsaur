import { defineConfig, devices } from "@playwright/test";

// End-to-end smoke test against the REAL embedded build: webServer below
// builds the actual Go binary (embedding whatever's currently in
// web/dist/, so `pnpm build` first if you want it to reflect current
// source) and runs it on loopback ports distinct from the documented dev
// workflow (127.0.0.1:5280 Vite + 127.0.0.1:8380 dnsaur, see
// web/README.md) so this can run alongside a `pnpm dev` session without
// port collisions. See e2e/run-server.sh for exactly what it starts.
export default defineConfig({
  testDir: "./e2e",
  fullyParallel: false, // a single spec against a single, stateful server
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 1 : 0,
  workers: 1,
  reporter: "line",
  use: {
    baseURL: "http://127.0.0.1:8381",
    trace: "on-first-retry",
  },
  projects: [{ name: "chromium", use: { ...devices["Desktop Chrome"] } }],
  webServer: {
    command: "bash e2e/run-server.sh",
    url: "http://127.0.0.1:8381/api/v1/health",
    // Every run needs a genuinely fresh instance (no admin account yet) —
    // the smoke test starts from the first-run setup wizard — so an
    // already-running server on this port must never be reused.
    reuseExistingServer: false,
    // go build + a cold sqlite/goose migration can take a while on a
    // loaded CI runner; the health check above still short-circuits as
    // soon as the server is actually up.
    timeout: 120_000,
    stdout: "pipe",
    stderr: "pipe",
  },
});
