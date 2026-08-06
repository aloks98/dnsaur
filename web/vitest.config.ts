import { configDefaults, defineConfig } from "vitest/config";
import react from "@vitejs/plugin-react";

export default defineConfig({
  plugins: [react()],
  test: {
    environment: "jsdom",
    globals: true,
    setupFiles: ["./src/test/setup.ts"],
    // e2e/ holds Playwright specs (see playwright.config.ts) — they import
    // `test`/`expect` from @playwright/test, not vitest, and run against a
    // real built binary, not jsdom. Vitest's default include pattern would
    // otherwise pick up *.spec.ts here and fail trying to run them.
    exclude: [...configDefaults.exclude, "e2e/**"],
  },
});
