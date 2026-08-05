import { http, HttpResponse } from "msw";
import { toast } from "sonner";
import { expect, test, vi } from "vitest";
import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router";
import { server } from "../test/msw-server";
import { renderWithProviders } from "../test/render";
import { makeQueryClient } from "../lib/query-client";
import { useTheme } from "../lib/theme";
import { Dashboard } from "./dashboard";

// ECharts (which rnui's AreaChart wraps) needs a real 2D canvas context to
// render, which jsdom does not provide (getContext('2d') returns null,
// crashing zrender's canvas painter). Stub AreaChart with a plain div that
// exposes its series data as text — everything else from the module stays
// real. This is a jsdom/canvas limitation, not a testability smell in the
// component under test.
vi.mock("@e412/rnui-react", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@e412/rnui-react")>();
  return {
    ...actual,
    AreaChart: ({ series }: { series: { name: string; data: number[]; color?: string }[] }) => (
      <div data-testid="timeline-chart">
        {series.map((s) => (
          <div key={s.name} data-series={s.name} data-color={s.color}>
            {s.name}: {s.data.join(",")}
          </div>
        ))}
      </div>
    ),
  };
});

function seriesColor(name: string): string {
  return (
    screen
      .getByTestId("timeline-chart")
      .querySelector(`[data-series="${name}"]`)
      ?.getAttribute("data-color") ?? ""
  );
}

test("renders stat tiles with a computed blocked percentage", async () => {
  renderWithProviders(<Dashboard />);

  expect(await screen.findByText("25%")).toBeInTheDocument(); // blocked: 250/1000
  expect(screen.getByText("40%")).toBeInTheDocument(); // cache hit rate: 400/1000
  expect(screen.getByText("1,000")).toBeInTheDocument(); // total
  expect(screen.getByText("12")).toBeInTheDocument(); // active clients
});

test("changing the window select refetches with the new hours", async () => {
  const user = userEvent.setup();
  const overviewUrls: string[] = [];
  server.use(
    http.get("/api/v1/stats/overview", ({ request }) => {
      overviewUrls.push(request.url);
      return HttpResponse.json({
        total: 1000,
        blocked: 250,
        cached: 400,
        forwarded: 350,
        clients: 12,
      });
    }),
  );

  renderWithProviders(<Dashboard />);
  await screen.findByText("25%");
  expect(overviewUrls.at(-1)).toContain("hours=24");

  await user.click(screen.getByRole("combobox", { name: /time window/i }));
  await user.click(await screen.findByRole("option", { name: /last hour/i }));

  await waitFor(() => expect(overviewUrls.at(-1)).toContain("hours=1"));
});

test("the timeline chart excludes error decisions from the not-blocked series", async () => {
  server.use(
    http.get("/api/v1/stats/timeline", () =>
      HttpResponse.json([
        {
          bucket: Math.floor(Date.now() / 1000),
          // total=20, but 3 of those are `error` (a failed resolve, not a
          // query dnsaur let through) — must not be counted as "not blocked".
          decisions: { allowed: 10, blocked: 5, error: 3, cached: 2 },
        },
      ]),
    ),
  );

  renderWithProviders(<Dashboard />);

  // Not blocked = (10 allowed + 2 cached), excluding both the 5 blocked and
  // the 3 error — i.e. 12, not 15 (the old, buggy total-minus-blocked math).
  expect(await screen.findByText("Not blocked: 12")).toBeInTheDocument();
  expect(screen.getByText("Blocked: 5")).toBeInTheDocument();
});

test("empty stats show EmptyState everywhere and never render NaN", async () => {
  server.use(
    http.get("/api/v1/stats/overview", () =>
      HttpResponse.json({ total: 0, blocked: 0, cached: 0, forwarded: 0, clients: 0 }),
    ),
    http.get("/api/v1/stats/timeline", () => HttpResponse.json([])),
    http.get("/api/v1/stats/top", () => HttpResponse.json([])),
  );

  renderWithProviders(<Dashboard />);

  expect(await screen.findByText("No query activity yet")).toBeInTheDocument();
  expect(screen.getByText("No domains yet")).toBeInTheDocument();
  expect(screen.getByText("No blocked domains yet")).toBeInTheDocument();
  expect(screen.getByText("No clients yet")).toBeInTheDocument();
  // blocked % and cache hit rate both guard the divide-by-zero
  expect(screen.getAllByText("—").length).toBeGreaterThanOrEqual(2);
  expect(document.body.textContent).not.toMatch(/NaN/);
});

test("blocking a top domain posts a block rule for group 1 and shows a success toast", async () => {
  const user = userEvent.setup();
  let requestBody: unknown;
  server.use(
    http.post("/api/v1/groups/1/rules", async ({ request }) => {
      requestBody = await request.json();
      return HttpResponse.json({ id: 1 }, { status: 201 });
    }),
  );
  const successSpy = vi.spyOn(toast, "success");

  renderWithProviders(<Dashboard />);
  await screen.findByText("example.com");

  const row = screen.getByText("example.com").closest("tr");
  if (!row) throw new Error("row not found");
  await user.click(within(row).getByRole("button", { name: /block/i }));

  await waitFor(() => expect(requestBody).toEqual({ action: "block", pattern: "example.com" }));
  expect(await within(row).findByText("Blocked")).toBeInTheDocument();
  expect(successSpy).toHaveBeenCalledWith("Blocked example.com");
});

test("allowing a top blocked domain posts an allow rule for group 1", async () => {
  const user = userEvent.setup();
  let requestBody: unknown;
  server.use(
    http.post("/api/v1/groups/1/rules", async ({ request }) => {
      requestBody = await request.json();
      return HttpResponse.json({ id: 2 }, { status: 201 });
    }),
  );
  const successSpy = vi.spyOn(toast, "success");

  renderWithProviders(<Dashboard />);
  await screen.findByText("ads.tracker.example");

  const row = screen.getByText("ads.tracker.example").closest("tr");
  if (!row) throw new Error("row not found");
  await user.click(within(row).getByRole("button", { name: /allow/i }));

  await waitFor(() =>
    expect(requestBody).toEqual({ action: "allow", pattern: "ads.tracker.example" }),
  );
  expect(await within(row).findByText("Allowed")).toBeInTheDocument();
  expect(successSpy).toHaveBeenCalledWith("Allowed ads.tracker.example");
});

test("a failed quick action shows an error toast and lets the user retry", async () => {
  const user = userEvent.setup();
  server.use(
    http.post("/api/v1/groups/1/rules", () =>
      HttpResponse.json({ error: "boom" }, { status: 500 }),
    ),
  );
  const errorSpy = vi.spyOn(toast, "error");

  renderWithProviders(<Dashboard />);
  await screen.findByText("example.com");

  const row = screen.getByText("example.com").closest("tr");
  if (!row) throw new Error("row not found");
  await user.click(within(row).getByRole("button", { name: /block/i }));

  await waitFor(() => expect(errorSpy).toHaveBeenCalledWith("Couldn't block example.com"));
  // Still offers the action — did not get stuck showing a false "Blocked" state.
  expect(within(row).getByRole("button", { name: /block/i })).toBeInTheDocument();
});

test("errors ride as their own series so the chart's total matches the Total queries tile", async () => {
  server.use(
    http.get("/api/v1/stats/overview", () =>
      // The tile's denominator: the backend sums *every* decision into
      // `total`, errors included (internal/api/queries_handlers.go).
      HttpResponse.json({ total: 20, blocked: 5, cached: 2, forwarded: 0, clients: 3 }),
    ),
    http.get("/api/v1/stats/timeline", () =>
      HttpResponse.json([
        {
          bucket: Math.floor(Date.now() / 1000),
          decisions: { allowed: 10, blocked: 5, error: 3, cached: 2 },
        },
      ]),
    ),
  );

  renderWithProviders(<Dashboard />);

  expect(await screen.findByText("Errors: 3")).toBeInTheDocument();
  // 12 + 5 + 3 = 20, the same number the tile above reports for the window.
  expect(screen.getByRole("figure", { name: /20 queries/ })).toBeInTheDocument();
});

test("a healthy window carries no Errors series at all", async () => {
  server.use(
    http.get("/api/v1/stats/timeline", () =>
      HttpResponse.json([
        {
          bucket: Math.floor(Date.now() / 1000),
          decisions: { allowed: 10, blocked: 5, cached: 2 },
        },
      ]),
    ),
  );

  renderWithProviders(<Dashboard />);

  expect(await screen.findByText("Blocked: 5")).toBeInTheDocument();
  expect(screen.queryByText(/^Errors:/)).not.toBeInTheDocument();
});

test("with several groups the quick rule targets the chosen one, not always group 1", async () => {
  const user = userEvent.setup();
  const posted: { groupId: string; body: unknown }[] = [];
  server.use(
    http.get("/api/v1/groups", () =>
      HttpResponse.json([
        { id: 1, name: "default", enabled: true },
        { id: 2, name: "kids", enabled: true },
      ]),
    ),
    http.post("/api/v1/groups/:id/rules", async ({ params, request }) => {
      posted.push({ groupId: String(params.id), body: await request.json() });
      return HttpResponse.json({ id: 1 }, { status: 201 });
    }),
  );
  const successSpy = vi.spyOn(toast, "success");

  renderWithProviders(<Dashboard />);

  // The picker only appears once there is a choice to make.
  await user.click(await screen.findByRole("combobox", { name: /rule group/i }));
  await user.click(await screen.findByRole("option", { name: "kids" }));

  const row = (await screen.findByText("example.com")).closest("tr");
  if (!row) throw new Error("row not found");
  await user.click(within(row).getByRole("button", { name: /block/i }));

  await waitFor(() => expect(posted).toHaveLength(1));
  expect(posted[0]).toEqual({ groupId: "2", body: { action: "block", pattern: "example.com" } });
  expect(successSpy).toHaveBeenCalledWith("Blocked example.com in kids");
});

test("a single-group instance shows no group picker", async () => {
  renderWithProviders(<Dashboard />);

  await screen.findByText("example.com");
  expect(screen.queryByRole("combobox", { name: /rule group/i })).not.toBeInTheDocument();
});

// --- background-poll failures ---------------------------------------------

// Unlike every other page (which only refetches after a mutation), the
// dashboard polls unprompted forever: use-stats.ts puts a 30s refetchInterval
// on the overview, the timeline, and all three top queries. query-core sets
// status:"error" on a failed *background* refetch even though `data` is still
// in hand, so gating on isError alone made one blipped poll swap the four
// tiles for "Couldn't load stats", the chart for "Couldn't load the timeline"
// and every top table for "Couldn't load this list" — then swap them back 30s
// later, on the app's landing page. Invalidating the cached stats queries
// against a now-failing server drives exactly the path the poll does, without
// waiting 30 seconds for it.
test("a failing background poll keeps the tiles, chart, and top tables", async () => {
  let failing = false;
  const boom = () => HttpResponse.json({ error: "boom" }, { status: 500 });
  server.use(
    http.get("/api/v1/stats/overview", () =>
      failing
        ? boom()
        : HttpResponse.json({
            total: 1000,
            blocked: 250,
            cached: 400,
            forwarded: 350,
            clients: 12,
          }),
    ),
    http.get("/api/v1/stats/timeline", () =>
      failing
        ? boom()
        : HttpResponse.json([
            { bucket: Math.floor(Date.now() / 1000), decisions: { allowed: 10, blocked: 5 } },
          ]),
    ),
    http.get("/api/v1/stats/top", ({ request }) => {
      if (failing) return boom();
      const metric = new URL(request.url).searchParams.get("metric");
      return HttpResponse.json(
        metric === "domain" ? [{ key: "example.com", count: 320 }] : [{ key: "other", count: 1 }],
      );
    }),
  );

  const client = makeQueryClient();
  render(
    <QueryClientProvider client={client}>
      <MemoryRouter>
        <Dashboard />
      </MemoryRouter>
    </QueryClientProvider>,
  );

  expect(await screen.findByText("25%")).toBeInTheDocument();
  expect(await screen.findByText("Blocked: 5")).toBeInTheDocument();
  expect(await screen.findByText("example.com")).toBeInTheDocument();

  failing = true;
  await act(async () => {
    await client.invalidateQueries({ queryKey: ["stats"] });
  });

  // Every section that had data still shows it, under a quiet retry banner.
  await waitFor(() =>
    expect(screen.getAllByText(/couldn't refresh/i).length).toBeGreaterThanOrEqual(3),
  );
  expect(screen.getByText("25%")).toBeInTheDocument(); // tiles
  expect(screen.getByText("1,000")).toBeInTheDocument();
  expect(screen.getByText("Blocked: 5")).toBeInTheDocument(); // chart
  expect(screen.getByText("example.com")).toBeInTheDocument(); // top table

  // None of the destructive "nothing to show" states replaced them.
  expect(screen.queryByText(/couldn't load stats/i)).not.toBeInTheDocument();
  expect(screen.queryByText(/couldn't load the timeline/i)).not.toBeInTheDocument();
  expect(screen.queryByText(/couldn't load this list/i)).not.toBeInTheDocument();
});

// The mirror image: when the *first* load fails there is nothing to keep, so
// the destructive states are still the right answer. `undefined?.length === 0`
// is false, so an errored first load must never be mistaken for "empty".
test("a first load that fails still shows the destructive states, not empty ones", async () => {
  server.use(
    http.get("/api/v1/stats/overview", () => HttpResponse.json({ error: "boom" }, { status: 500 })),
    http.get("/api/v1/stats/timeline", () => HttpResponse.json({ error: "boom" }, { status: 500 })),
    http.get("/api/v1/stats/top", () => HttpResponse.json({ error: "boom" }, { status: 500 })),
  );

  renderWithProviders(<Dashboard />);

  expect(
    await screen.findByText(/couldn't load stats/i, undefined, { timeout: 3000 }),
  ).toBeInTheDocument();
  expect(screen.getByText(/couldn't load the timeline/i)).toBeInTheDocument();
  expect(screen.getAllByText(/couldn't load this list/i)).toHaveLength(3);
  expect(screen.queryByText(/no query activity yet/i)).not.toBeInTheDocument();
  expect(screen.queryByText(/no domains yet/i)).not.toBeInTheDocument();
  expect(screen.queryByText(/couldn't refresh/i)).not.toBeInTheDocument();
});

// --- quick actions vs. an unavailable group list ---------------------------

// availableGroups is `groups.data ?? []`, which hides the group picker while
// /groups is pending or errored — and with no picker, effectiveGroupId falls
// back to DEFAULT_GROUP_ID. A click in that window wrote into group 1 and
// toasted an unqualified "Blocked example.com" on an instance that may have
// several groups: exactly the silent default the picker exists to remove.
test("quick actions are disabled while the group list is unavailable", async () => {
  const user = userEvent.setup();
  const posted: string[] = [];
  server.use(
    http.get("/api/v1/groups", () => HttpResponse.json({ error: "boom" }, { status: 500 })),
    http.post("/api/v1/groups/:id/rules", ({ params }) => {
      posted.push(String(params.id));
      return HttpResponse.json({ id: 1 }, { status: 201 });
    }),
  );
  // spyOn keeps the same mock across tests in this file, so earlier tests'
  // successful quick actions are still recorded on it.
  const successSpy = vi.spyOn(toast, "success").mockClear();

  renderWithProviders(<Dashboard />);
  const row = (await screen.findByText("example.com")).closest("tr");
  if (!row) throw new Error("row not found");

  const block = within(row).getByRole("button", { name: /block/i });
  await waitFor(() => expect(block).toBeDisabled());

  await user.click(block);
  expect(posted).toEqual([]);
  expect(successSpy).not.toHaveBeenCalled();
});

// --- chart colors ----------------------------------------------------------

// Canvas2D cannot resolve CSS custom properties: zrender feeds each series
// color into CanvasGradient.addColorStop for the area fill, where
// "var(--chart-1)" throws a SyntaxError out of a layout effect and the
// ErrorBoundary replaces the whole page with "Something went wrong". The
// dashboard did this on any instance that had served one query (an empty
// timeline draws an EmptyState, so a fresh instance looked fine).
// pages/dashboard-chart.test.tsx guards the crash itself against the real
// chart; this asserts the contract that prevents it.
test("chart series colors are resolved, never raw var() references", async () => {
  renderWithProviders(<Dashboard />);
  await screen.findByTestId("timeline-chart");

  for (const name of ["Not blocked", "Blocked"]) {
    expect(seriesColor(name)).not.toContain("var(");
    expect(seriesColor(name)).not.toBe("");
  }
});

// Light and dark define different values for --chart-1/--destructive/
// --warning, so a single read at mount leaves the chart painted in the old
// theme's colors after a toggle.
test("chart series colors are re-read when the theme changes", async () => {
  function ThemeSwitch() {
    const { setTheme } = useTheme();
    return (
      <button type="button" onClick={() => setTheme("dark")}>
        go dark
      </button>
    );
  }
  const user = userEvent.setup();

  renderWithProviders(
    <>
      <ThemeSwitch />
      <Dashboard />
    </>,
  );
  await screen.findByTestId("timeline-chart");
  const lightBlocked = seriesColor("Blocked");

  await user.click(screen.getByRole("button", { name: /go dark/i }));

  await waitFor(() => expect(seriesColor("Blocked")).not.toBe(lightBlocked));
  expect(seriesColor("Blocked")).not.toContain("var(");
});
