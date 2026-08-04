import { expect, test } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import { ErrorBoundary } from "../components/error-boundary";
import { renderWithProviders } from "../test/render";
import { Dashboard } from "./dashboard";

// Deliberately does NOT mock AreaChart (unlike dashboard.test.tsx, which
// stubs it out for speed and to assert series data without depending on
// chart-library internals). This is the one test that exercises the real,
// ECharts-backed chart end to end, guarding the jsdom canvas-2D stub in
// test/setup.ts.
//
// Why this matters: without that stub, mounting the real AreaChart under
// jsdom throws inside a layout effect (jsdom has no canvas 2D context).
// React's commit-phase error propagation trips the nearest ErrorBoundary —
// but the sidebar's own nav-link assertion in app.test.tsx lives *outside*
// that boundary, so that test keeps passing even while the dashboard
// silently renders its error fallback instead of the real page. Wrapping
// in the real ErrorBoundary here, and asserting its fallback text is
// absent, means a regression (e.g. a future echarts/zrender upgrade that
// touches a canvas API the stub's permissive proxy doesn't happen to
// cover) fails this test loudly instead of passing green by accident.
test("the real (unmocked) timeline chart mounts without tripping the ErrorBoundary", async () => {
  const { container } = renderWithProviders(
    <ErrorBoundary>
      <Dashboard />
    </ErrorBoundary>,
  );

  // Positive: the real chart actually mounted (rnui's EChart wrapper always
  // renders a `data-slot="echart"` container div once it initializes).
  await waitFor(() => expect(container.querySelector('[data-slot="echart"]')).toBeInTheDocument());
  expect(screen.getByRole("figure", { name: /query volume over time/i })).toBeInTheDocument();

  // Negative: the ErrorBoundary fallback never appeared anywhere on the page.
  expect(screen.queryByText(/something went wrong/i)).not.toBeInTheDocument();
});
