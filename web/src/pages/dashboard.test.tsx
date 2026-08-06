import { act } from "react";
import { http, HttpResponse } from "msw";
import { toast } from "sonner";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router";
import { server } from "../test/msw-server";
import { hourStart } from "../test/msw-handlers";
import { renderWithProviders } from "../test/render";
import { FakeEventSource } from "../test/fake-event-source";
import { makeQueryClient } from "../lib/query-client";
import { useTheme } from "../lib/theme";
import type { QueryEntry, TimelineBucket } from "../api/types";
import { Dashboard } from "./dashboard";

// ECharts (which rnui's BarChart wraps) needs a real 2D canvas context,
// which jsdom does not provide. Stub BarChart with a plain div that exposes
// the props the design actually depends on — the categories, each series'
// data/stack/colour/corner radius, and the merged ECharts `option` — so this
// file can assert the *contract* with the chart library without depending on
// its internals. pages/dashboard-chart.test.tsx mounts the real one.
interface MockSeries {
  name: string;
  data: number[];
  stack?: string;
  itemStyle?: { color?: string; borderRadius?: number };
}

/** Counts how often the chart is actually handed a new option to paint. */
const chartRenders = vi.hoisted(() => ({ count: 0 }));

vi.mock("@e412/rnui-react", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@e412/rnui-react")>();
  return {
    ...actual,
    BarChart: ({
      categories,
      series,
      stacked,
      option,
    }: {
      categories?: string[];
      series?: unknown;
      stacked?: boolean;
      option?: unknown;
    }) => {
      chartRenders.count += 1;
      return (
        <div
          data-testid="timeline-chart"
          data-stacked={String(stacked)}
          data-buckets={String(categories?.length ?? 0)}
          data-option={JSON.stringify(option)}
        >
          {(series as MockSeries[]).map((s) => (
            <div
              key={s.name}
              data-series={s.name}
              data-color={s.itemStyle?.color}
              data-radius={String(s.itemStyle?.borderRadius)}
              data-stack={s.stack}
            >
              {s.name}: {s.data.join(",")}
            </div>
          ))}
        </div>
      );
    },
  };
});

beforeEach(() => {
  chartRenders.count = 0;
  FakeEventSource.instances = [];
  vi.stubGlobal("EventSource", FakeEventSource);
});

afterEach(() => {
  vi.useRealTimers();
  vi.restoreAllMocks();
});

// --- helpers -----------------------------------------------------------------

function chart(): HTMLElement {
  return screen.getByTestId("timeline-chart");
}

function seriesEl(name: string): HTMLElement {
  const el = chart().querySelector(`[data-series="${name}"]`);
  if (!el) throw new Error(`no "${name}" series on the chart`);
  return el as HTMLElement;
}

/** The ECharts `option` the page merged into BarChart, as JSON. */
function chartOption<T>(): T {
  return JSON.parse(chart().getAttribute("data-option") ?? "{}") as T;
}

async function firstSource(): Promise<FakeEventSource> {
  await waitFor(() => expect(FakeEventSource.instances.length).toBeGreaterThan(0));
  return FakeEventSource.instances[0]!;
}

function entry(overrides: Partial<QueryEntry> = {}): QueryEntry {
  return {
    id: 1,
    at: Date.now(),
    instance_id: "i1",
    client_ip: "192.168.1.10",
    client_id: 1,
    q_name: "example.com",
    q_type: "A",
    decision: "forwarded",
    rule_id: 0,
    list_id: 0,
    upstream: "1.1.1.1",
    r_code: "NOERROR",
    duration_ms: 7,
    ...overrides,
  };
}

/** Push entries through the mocked stream and let useLiveTail's 100ms
 * coalescing window close. Waits on the *last* entry emitted: it's the
 * newest, so it's the one guaranteed to be on screen however many were
 * sent. */
async function emitAll(source: FakeEventSource, entries: QueryEntry[]) {
  act(() => source.emitOpen());
  act(() => {
    for (const e of entries) source.emit(e);
  });
  await waitFor(() => expect(screen.queryByText(entries.at(-1)!.q_name)).toBeInTheDocument());
}

/** The live-queries row whose domain cell reads `domain`. */
function liveRow(domain: string): HTMLElement {
  const row = screen.getByText(domain).closest("tr");
  if (!row) throw new Error(`no live row for ${domain}`);
  return row;
}

// --- 1. stat strip -----------------------------------------------------------

test("the stat strip shows total, blocked %, cache hit rate and clients", async () => {
  renderWithProviders(<Dashboard />);

  expect(await screen.findByText("1,000")).toBeInTheDocument(); // total
  expect(screen.getByText("25%")).toBeInTheDocument(); // blocked: 250/1000
  expect(screen.getByText("40%")).toBeInTheDocument(); // cached: 400/1000
  expect(screen.getByText("12")).toBeInTheDocument(); // clients

  // Only the blocked numeral is tinted — the design's one coloured stat.
  expect(screen.getByText("25%").className).toContain("text-chart-blocked");
  expect(screen.getByText("1,000").className).not.toContain("text-chart-blocked");
});

// `total` sums *every* decision the resolver writes — local and error
// included — so blocked + cached + forwarded is legitimately less than it,
// and nothing on this page may derive an "allowed" count by subtraction.
test("percentages divide by the reported total, never by a derived remainder", async () => {
  server.use(
    http.get("/api/v1/stats/overview", () =>
      HttpResponse.json({ total: 400, blocked: 100, cached: 40, forwarded: 60, clients: 2 }),
    ),
  );
  renderWithProviders(<Dashboard />);

  expect(await screen.findByText("25%")).toBeInTheDocument(); // 100/400, not 100/200
  expect(screen.getByText("10%")).toBeInTheDocument(); // 40/400
});

test("an all-zero window renders em dashes, never NaN", async () => {
  server.use(
    http.get("/api/v1/stats/overview", () =>
      HttpResponse.json({ total: 0, blocked: 0, cached: 0, forwarded: 0, clients: 0 }),
    ),
    http.get("/api/v1/stats/timeline", () => HttpResponse.json([])),
    http.get("/api/v1/stats/top", () => HttpResponse.json([])),
  );

  renderWithProviders(<Dashboard />);

  await screen.findByText(/no query activity yet/i);
  expect(screen.getAllByText("—")).toHaveLength(2); // blocked % and cache hit rate
  expect(document.body.textContent).not.toMatch(/NaN/);
  expect(screen.getByText(/nothing blocked in this window yet/i)).toBeInTheDocument();
  expect(screen.getByText(/no clients have queried in this window yet/i)).toBeInTheDocument();
});

// --- 2. the window, which now lives in the URL -------------------------------

test("the window comes from the URL, so every stats call asks for its hours", async () => {
  const asked: string[] = [];
  server.use(
    http.get("/api/v1/stats/overview", ({ request }) => {
      asked.push(request.url);
      return HttpResponse.json({ total: 8, blocked: 1, cached: 2, forwarded: 5, clients: 1 });
    }),
  );

  renderWithProviders(<Dashboard />, { route: "/?window=7d" });

  await screen.findByText("8");
  expect(asked.at(-1)).toContain("hours=168");
});

test("an unrecognised window falls back to 24h rather than putting it on the wire", async () => {
  const asked: string[] = [];
  server.use(
    http.get("/api/v1/stats/overview", ({ request }) => {
      asked.push(request.url);
      return HttpResponse.json({ total: 8, blocked: 1, cached: 2, forwarded: 5, clients: 1 });
    }),
  );

  renderWithProviders(<Dashboard />, { route: "/?window=all-time" });

  await screen.findByText("8");
  expect(asked.at(-1)).toContain("hours=24");
});

// --- 3. query volume ---------------------------------------------------------

test("the chart stacks resolved under blocked, in square bars, in resolved colours", async () => {
  server.use(
    http.get("/api/v1/stats/timeline", () =>
      HttpResponse.json([
        { bucket: hourStart(1), decisions: { forwarded: 10, blocked: 5, error: 3, cached: 2 } },
      ]),
    ),
  );

  renderWithProviders(<Dashboard />, { route: "/?window=1h" });
  await screen.findByTestId("timeline-chart");

  expect(chart().getAttribute("data-stacked")).toBe("true");
  // Both series share one stack, and "Resolved" is declared first so it
  // sits underneath.
  const names = [...chart().querySelectorAll("[data-series]")].map((el) =>
    el.getAttribute("data-series"),
  );
  expect(names).toEqual(["Resolved", "Blocked"]);
  expect(seriesEl("Resolved").getAttribute("data-stack")).toBe(
    seriesEl("Blocked").getAttribute("data-stack"),
  );

  // BarChart's own default is itemStyle.borderRadius [4,4,0,0]; --radius is
  // 0 app-wide and a rounded cap inside a stack notches the band above it.
  for (const name of ["Resolved", "Blocked"]) {
    expect(seriesEl(name).getAttribute("data-radius")).toBe("0");
  }
});

// "Resolved" is every non-blocked decision, errors included: a failed
// resolve isn't a query dnsaur let through, but it *is* one it didn't
// block, and the two bands have to add up to the same total the strip
// above reports.
test("resolved counts every non-blocked decision, so the bands total the strip", async () => {
  server.use(
    http.get("/api/v1/stats/overview", () =>
      HttpResponse.json({ total: 20, blocked: 5, cached: 2, forwarded: 10, clients: 3 }),
    ),
    http.get("/api/v1/stats/timeline", () =>
      HttpResponse.json([
        { bucket: hourStart(0), decisions: { forwarded: 10, blocked: 5, error: 3, cached: 2 } },
      ]),
    ),
  );

  renderWithProviders(<Dashboard />, { route: "/?window=1h" });

  // 10 forwarded + 3 error + 2 cached = 15 resolved, 5 blocked, 20 total.
  expect(await screen.findByText("Resolved: 0,15")).toBeInTheDocument();
  expect(screen.getByText("Blocked: 0,5")).toBeInTheDocument();
  expect(screen.getByRole("figure", { name: /20 queries/ })).toBeInTheDocument();
});

// /stats/timeline emits a row only for hours that had traffic and never
// zero-fills, so plotting rows in arrival order drew 23:00 immediately
// beside 07:00 at equal width — a quiet night reading as continuous
// traffic. The missing hours are synthesised, and the section says so.
test("hours with no traffic are synthesised as zeroes so the axis stays continuous", async () => {
  server.use(
    http.get("/api/v1/stats/timeline", () =>
      HttpResponse.json([
        { bucket: hourStart(3), decisions: { forwarded: 4, blocked: 1 } },
        // hourStart(2) and hourStart(1) never happened.
        { bucket: hourStart(0), decisions: { forwarded: 8, blocked: 2 } },
      ]),
    ),
  );

  renderWithProviders(<Dashboard />, { route: "/?window=1h" });
  await screen.findByTestId("timeline-chart");

  // The window is 1h, but the data reaches 3h back — the axis grows to
  // cover it rather than dropping buckets it was handed.
  expect(seriesEl("Resolved").textContent).toBe("Resolved: 4,0,0,8");
  expect(seriesEl("Blocked").textContent).toBe("Blocked: 1,0,0,2");
  expect(chart().getAttribute("data-buckets")).toBe("4");
});

test("the header names the real granularity and legends both bands", async () => {
  renderWithProviders(<Dashboard />);
  await screen.findByTestId("timeline-chart");

  // The design says "15-min"; the endpoint only ever produces hour buckets.
  expect(screen.getByText("hourly buckets")).toBeInTheDocument();
  expect(screen.queryByText(/15-min/i)).not.toBeInTheDocument();

  const header = screen.getByText("Query volume").closest("div");
  expect(header).not.toBeNull();
  expect(within(header!).getByText("Blocked")).toBeInTheDocument();
  expect(within(header!).getByText("Resolved")).toBeInTheDocument();
});

test("the chart carries a text alternative naming both totals", async () => {
  renderWithProviders(<Dashboard />);

  // Default fixture: 5 buckets × (120+90+40+5 resolved, 30 blocked).
  const figure = await screen.findByRole("figure");
  expect(figure).toHaveAccessibleName(/1,275 resolved/);
  expect(figure).toHaveAccessibleName(/150 blocked/);
  expect(figure).toHaveAccessibleName(/hourly buckets/);
});

test("an empty window draws no chart at all, rather than a flat fabricated axis", async () => {
  server.use(http.get("/api/v1/stats/timeline", () => HttpResponse.json([])));

  renderWithProviders(<Dashboard />);

  expect(await screen.findByText(/no query activity yet/i)).toBeInTheDocument();
  expect(screen.queryByTestId("timeline-chart")).not.toBeInTheDocument();
});

// --- chart colours -----------------------------------------------------------

// Canvas2D cannot resolve CSS custom properties: zrender hands series colours
// straight to the canvas painter, where "var(--chart-blocked)" throws a
// SyntaxError out of a layout effect and the ErrorBoundary replaces the whole
// page with "Something went wrong". test/setup.ts rejects the same values a
// real browser does; dashboard-chart.test.tsx guards the crash end to end,
// and this pins the contract that prevents it.
test("every colour the chart is given is resolved, never a raw var() reference", async () => {
  renderWithProviders(<Dashboard />);
  await screen.findByTestId("timeline-chart");

  for (const name of ["Resolved", "Blocked"]) {
    const color = seriesEl(name).getAttribute("data-color") ?? "";
    expect(color).not.toContain("var(");
    expect(color).not.toBe("");
  }

  // The axis/gridline colours ride in `option` and reach the same painter.
  expect(chart().getAttribute("data-option")).not.toContain("var(");
});

test("gridlines and axis labels use the design's muted-border and muted-foreground tones", async () => {
  renderWithProviders(<Dashboard />);
  await screen.findByTestId("timeline-chart");

  const option = chartOption<{
    yAxis: { splitLine: { lineStyle: { color: string } }; axisLabel: { color: string } };
    xAxis: { axisTick: { show: boolean } };
  }>();
  // Light mode's --border-muted / --muted-foreground, resolved.
  expect(option.yAxis.splitLine.lineStyle.color).toBe("oklch(0.9507 0.0108 158.84)");
  expect(option.yAxis.axisLabel.color).toBe("oklch(0.511 0.0259 155.36)");
  expect(option.xAxis.axisTick.show).toBe(false);
});

// Light and dark define different values for every chart token, so a single
// read at mount leaves the chart painted in the old theme after a toggle.
test("chart colours are re-read when the theme changes", async () => {
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
  const lightBlocked = seriesEl("Blocked").getAttribute("data-color");

  await user.click(screen.getByRole("button", { name: /go dark/i }));

  await waitFor(() =>
    expect(seriesEl("Blocked").getAttribute("data-color")).not.toBe(lightBlocked),
  );
  expect(seriesEl("Blocked").getAttribute("data-color")).not.toContain("var(");
});

// --- 4. live queries ---------------------------------------------------------

test("live rows render from the shared tail, newest first, capped at twelve", async () => {
  renderWithProviders(<Dashboard />);
  const source = await firstSource();

  await emitAll(
    source,
    Array.from({ length: 20 }, (_, i) => entry({ id: i + 1, q_name: `host${i + 1}.example` })),
  );

  const rows = screen.getAllByRole("row").filter((r) => r.className.includes("group/row"));
  expect(rows).toHaveLength(12);
  // Newest first: host20 is on screen, host1 (13 arrivals older) is not.
  expect(screen.getByText("host20.example")).toBeInTheDocument();
  expect(screen.queryByText("host1.example")).not.toBeInTheDocument();

  // One SSE client for the whole app — never a second stream of its own.
  expect(FakeEventSource.instances).toHaveLength(1);
  expect(FakeEventSource.instances[0]!.url).toBe("/api/v1/queries/tail");
});

test("each row shows time, domain, client, decision and duration", async () => {
  renderWithProviders(<Dashboard />);
  const source = await firstSource();
  await emitAll(source, [
    entry({
      id: 1,
      q_name: "one.example",
      client_ip: "10.0.0.4",
      decision: "cached",
      duration_ms: 3,
    }),
  ]);

  const row = liveRow("one.example");
  expect(within(row).getByText("10.0.0.4")).toBeInTheDocument();
  expect(within(row).getByText("cached")).toBeInTheDocument();
  expect(within(row).getByText("3ms")).toBeInTheDocument();
});

// The decision vocabulary is the resolver's (internal/dnssrv/pipeline.go).
// There is no `allowed` — the pipeline never writes it — so nothing here
// may invent one.
test("each decision gets its own tone, and blocked/error are the loud ones", async () => {
  renderWithProviders(<Dashboard />);
  const source = await firstSource();
  await emitAll(source, [
    entry({ id: 1, q_name: "blocked.example", decision: "blocked" }),
    entry({ id: 2, q_name: "error.example", decision: "error" }),
    entry({ id: 3, q_name: "stale.example", decision: "stale" }),
    entry({ id: 4, q_name: "cached.example", decision: "cached" }),
    entry({ id: 5, q_name: "forwarded.example", decision: "forwarded" }),
    entry({ id: 6, q_name: "local.example", decision: "local" }),
  ]);

  const toneOf = (decision: string) =>
    screen.getByText(decision, { selector: "td" }).className ?? "";
  expect(toneOf("blocked")).toContain("text-destructive");
  expect(toneOf("error")).toContain("text-destructive");
  expect(toneOf("stale")).toContain("text-warn");
  expect(toneOf("cached")).toContain("text-muted-foreground");
  expect(toneOf("forwarded")).toContain("text-foreground");
  expect(toneOf("local")).toContain("text-primary");
});

// There is no rate endpoint; the number is measured from arrivals on the
// stream. Showing one computed over the first second of uptime would be a
// number, not a measurement.
test("the rate readout stays absent until it has watched long enough to mean it", async () => {
  vi.useFakeTimers({ shouldAdvanceTime: true });

  renderWithProviders(<Dashboard />);
  const source = await firstSource();
  act(() => source.emitOpen());
  act(() => {
    for (let i = 1; i <= 20; i += 1) source.emit(entry({ id: i, q_name: `h${i}.example` }));
  });
  // Past two ticks of the rate timer, so "nothing yet" is the warm-up
  // holding it back and not merely a timer that hasn't fired.
  await act(async () => {
    await vi.advanceTimersByTimeAsync(2_500);
  });

  expect(screen.queryByText(/q\/s/i)).not.toBeInTheDocument();

  await act(async () => {
    await vi.advanceTimersByTimeAsync(9_000);
  });

  // 20 arrivals over the ~11.5s observed so far.
  expect(screen.getByText(/q\/s/i)).toBeInTheDocument();
});

// --- 5. the right rail -------------------------------------------------------

test("the rail asks for six blocked domains and five clients, and bars them proportionally", async () => {
  const asked: string[] = [];
  server.use(
    http.get("/api/v1/stats/top", ({ request }) => {
      asked.push(request.url);
      const metric = new URL(request.url).searchParams.get("metric");
      return HttpResponse.json(
        metric === "blocked_domain"
          ? [
              { key: "ads.example", count: 200 },
              { key: "telemetry.example", count: 50 },
            ]
          : [{ key: "192.168.1.24", count: 10 }],
      );
    }),
  );

  renderWithProviders(<Dashboard />);
  await screen.findByText("ads.example");

  expect(asked.some((u) => u.includes("metric=blocked_domain") && u.includes("n=6"))).toBe(true);
  expect(asked.some((u) => u.includes("metric=client") && u.includes("n=5"))).toBe(true);

  // The bar is a share of the panel's own largest value, not of any total.
  const top = screen.getByText("ads.example").closest("li")!;
  const second = screen.getByText("telemetry.example").closest("li")!;
  expect(top.querySelector("[aria-hidden]")).toHaveStyle({ width: "100%" });
  expect(second.querySelector("[aria-hidden]")).toHaveStyle({ width: "25%" });

  // Blocked counts carry the design's blocked tint; client counts don't.
  expect(within(top).getByText("200").className).toContain("text-chart-blocked");
  expect(
    within(screen.getByText("192.168.1.24").closest("li")!).getByText("10").className,
  ).not.toContain("text-chart-blocked");
});

// GET /stats/top?metric=client answers with an IP and nothing else, so a
// name can only come from the client registry — and only where the registry
// can actually answer for that address.
test("a top client resolves to its name only on an exact-IP matcher", async () => {
  server.use(
    http.get("/api/v1/clients", () =>
      HttpResponse.json([
        { id: 1, name: "Laptop", matcher: "192.168.1.10", group_id: 1 },
        // A subnet covers the address but does not identify the device.
        { id: 2, name: "LAN", matcher: "192.168.1.0/24", group_id: 1 },
      ]),
    ),
  );

  renderWithProviders(<Dashboard />);

  expect(await screen.findByText("Laptop")).toBeInTheDocument(); // 192.168.1.10
  expect(screen.getByText("192.168.1.24")).toBeInTheDocument(); // only the CIDR covers it
  expect(screen.queryByText("LAN")).not.toBeInTheDocument();
});

test("each rail panel offers a way through to the full list", async () => {
  renderWithProviders(<Dashboard />);
  await screen.findByText("ads.tracker.example");

  expect(
    screen.getByRole("link", { name: /all blocked domains in the query log/i }),
  ).toHaveAttribute("href", "/queries");
  expect(screen.getByRole("link", { name: /all clients in groups & clients/i })).toHaveAttribute(
    "href",
    "/filtering/clients",
  );
});

// --- 6. quick block / allow --------------------------------------------------

test("a live row reveals Block on hover and posts the rule", async () => {
  const user = userEvent.setup();
  let body: unknown;
  server.use(
    http.post("/api/v1/groups/1/rules", async ({ request }) => {
      body = await request.json();
      return HttpResponse.json({ id: 1 }, { status: 201 });
    }),
  );
  const successSpy = vi.spyOn(toast, "success");

  renderWithProviders(<Dashboard />);
  const source = await firstSource();
  await emitAll(source, [entry({ id: 1, q_name: "tracker.example" })]);

  const row = liveRow("tracker.example");
  await user.click(within(row).getByRole("button", { name: "Block" }));

  await waitFor(() => expect(body).toEqual({ action: "block", pattern: "tracker.example" }));
  expect(await within(row).findByText("Blocked")).toBeInTheDocument();
  expect(successSpy).toHaveBeenCalledWith("Blocked tracker.example");
});

// An already-blocked row offers the opposite action; blocking it again
// would be a no-op the UI reported as a success.
test("an already-blocked live row offers Allow instead", async () => {
  renderWithProviders(<Dashboard />);
  const source = await firstSource();
  await emitAll(source, [entry({ id: 1, q_name: "ads.example", decision: "blocked" })]);

  const row = liveRow("ads.example");
  expect(within(row).getByRole("button", { name: "Allow" })).toBeInTheDocument();
  expect(within(row).queryByRole("button", { name: "Block" })).not.toBeInTheDocument();
});

test("a top-blocked row reveals Allow and posts the rule", async () => {
  const user = userEvent.setup();
  let body: unknown;
  server.use(
    http.post("/api/v1/groups/1/rules", async ({ request }) => {
      body = await request.json();
      return HttpResponse.json({ id: 2 }, { status: 201 });
    }),
  );
  const successSpy = vi.spyOn(toast, "success");

  renderWithProviders(<Dashboard />);
  const row = (await screen.findByText("ads.tracker.example")).closest("li")!;
  await user.click(within(row).getByRole("button", { name: "Allow" }));

  await waitFor(() => expect(body).toEqual({ action: "allow", pattern: "ads.tracker.example" }));
  expect(await within(row).findByText("Allowed")).toBeInTheDocument();
  expect(successSpy).toHaveBeenCalledWith("Allowed ads.tracker.example");
});

test("a failed quick action says so and leaves the action available", async () => {
  const user = userEvent.setup();
  server.use(
    http.post("/api/v1/groups/1/rules", () =>
      HttpResponse.json({ error: "boom" }, { status: 500 }),
    ),
  );
  const errorSpy = vi.spyOn(toast, "error");

  renderWithProviders(<Dashboard />);
  const row = (await screen.findByText("ads.tracker.example")).closest("li")!;
  await user.click(within(row).getByRole("button", { name: "Allow" }));

  await waitFor(() => expect(errorSpy).toHaveBeenCalledWith("Couldn't allow ads.tracker.example"));
  // Not stuck showing a false "Allowed".
  expect(within(row).getByRole("button", { name: "Allow" })).toBeInTheDocument();
});

// The rail aggregates across every client, so there is nothing to resolve a
// group from. Writing silently into group 1 meant a rule that reported
// success and did nothing for anyone outside the default.
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

  await user.click(await screen.findByRole("combobox", { name: /rule group/i }));
  await user.click(await screen.findByRole("option", { name: "kids" }));

  const row = (await screen.findByText("ads.tracker.example")).closest("li")!;
  await user.click(within(row).getByRole("button", { name: "Allow" }));

  await waitFor(() => expect(posted).toHaveLength(1));
  expect(posted[0]).toEqual({
    groupId: "2",
    body: { action: "allow", pattern: "ads.tracker.example" },
  });
  expect(successSpy).toHaveBeenCalledWith("Allowed ads.tracker.example in kids");
});

test("a single-group instance shows no group picker", async () => {
  renderWithProviders(<Dashboard />);

  await screen.findByText("ads.tracker.example");
  expect(screen.queryByRole("combobox", { name: /rule group/i })).not.toBeInTheDocument();
});

// Without the group list there is no picker and the target collapses to
// group 1 — exactly the silent default the picker exists to remove.
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
  const successSpy = vi.spyOn(toast, "success");

  renderWithProviders(<Dashboard />);
  const row = (await screen.findByText("ads.tracker.example")).closest("li")!;
  const allow = within(row).getByRole("button", { name: "Allow" });

  await waitFor(() => expect(allow).toBeDisabled());
  expect(allow).toHaveAttribute("title", expect.stringContaining("groups"));

  await user.click(allow);
  expect(posted).toEqual([]);
  expect(successSpy).not.toHaveBeenCalled();
});

// --- 7. polling failures -----------------------------------------------------

// The dashboard polls unprompted forever (use-stats.ts puts a 30s
// refetchInterval on the overview, the timeline and both top queries).
// query-core sets status:"error" on a failed *background* refetch even with
// `data` still in hand, so gating on isError alone made one blipped poll
// wipe the strip, the chart and the rail — then restore them 30s later, on
// the app's landing page. Invalidating drives exactly the poll's path.
test("a failing background poll keeps the strip, the chart and the rail", async () => {
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
            forwarded: 250,
            clients: 12,
          }),
    ),
    http.get("/api/v1/stats/timeline", () =>
      failing
        ? boom()
        : HttpResponse.json([{ bucket: hourStart(0), decisions: { forwarded: 10, blocked: 5 } }]),
    ),
    http.get("/api/v1/stats/top", ({ request }) => {
      if (failing) return boom();
      const metric = new URL(request.url).searchParams.get("metric");
      return HttpResponse.json(
        metric === "blocked_domain"
          ? [{ key: "ads.example", count: 140 }]
          : [{ key: "192.168.1.24", count: 9 }],
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
  expect(await screen.findByTestId("timeline-chart")).toBeInTheDocument();
  expect(await screen.findByText("ads.example")).toBeInTheDocument();

  failing = true;
  await act(async () => {
    await client.invalidateQueries({ queryKey: ["stats"] });
  });

  // All four polling queries kept their content under a quiet retry banner.
  await waitFor(() => expect(screen.getAllByText(/couldn't refresh/i)).toHaveLength(4));
  expect(screen.getByText("25%")).toBeInTheDocument();
  expect(screen.getByText("1,000")).toBeInTheDocument();
  expect(screen.getByTestId("timeline-chart")).toBeInTheDocument();
  expect(screen.getByText("ads.example")).toBeInTheDocument();
  expect(screen.getByText("192.168.1.24")).toBeInTheDocument();

  expect(screen.queryByText(/couldn't load stats/i)).not.toBeInTheDocument();
  expect(screen.queryByText(/couldn't load the timeline/i)).not.toBeInTheDocument();
  expect(screen.queryByText(/couldn't load this list/i)).not.toBeInTheDocument();
});

// The mirror image: with nothing to keep, the destructive states are still
// the right answer. `undefined?.length === 0` is false, so an errored first
// load must never be mistaken for an empty one.
test("a first load that fails shows the destructive states, not the empty ones", async () => {
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
  expect(screen.getAllByText(/couldn't load this list/i)).toHaveLength(2);
  expect(screen.queryByText(/no query activity yet/i)).not.toBeInTheDocument();
  expect(screen.queryByText(/nothing blocked in this window yet/i)).not.toBeInTheDocument();
  expect(screen.queryByText(/couldn't refresh/i)).not.toBeInTheDocument();
});

// --- 8. the chart's own render budget ----------------------------------------

// The live tail commits a batch up to ten times a second. While it was
// subscribed in the page component, every batch re-rendered the chart with
// freshly-built `option`/`series` objects; rnui's EChart re-applies an option
// whose identity changed, and re-applying tears down ECharts' hover state.
// The visible symptom was the tooltip being eaten on first hover and only
// working if you hovered again between two commits.
test("live rows arriving never re-render the chart", async () => {
  server.use(
    http.post("/api/v1/groups/1/rules", () => HttpResponse.json({ id: 1 }, { status: 201 })),
  );
  renderWithProviders(<Dashboard />);
  await screen.findByTestId("timeline-chart");
  await waitFor(() => expect(screen.getByText("ads.tracker.example")).toBeInTheDocument());

  const before = chartRenders.count;
  const source = await firstSource();

  // Three separate flush batches — the coalescing window is 100ms, so this
  // is three commits of the live panel, not one.
  for (let batch = 0; batch < 3; batch += 1) {
    await emitAll(
      source,
      Array.from({ length: 5 }, (_, i) =>
        entry({ id: batch * 5 + i + 1, q_name: `b${batch}h${i}.example` }),
      ),
    );
  }
  expect(screen.getByText("b2h4.example")).toBeInTheDocument(); // the stream really ran

  // ...and neither does a page-level state change with nothing to do with
  // the chart. The tail owning its own state is one layer; `memo` plus
  // stable `series`/`option` identities is the other, and this is the half
  // that covers everything else that re-renders the page.
  const user = userEvent.setup();
  const railRow = screen.getByText("ads.tracker.example").closest("li")!;
  await user.click(within(railRow).getByRole("button", { name: "Allow" }));
  expect(await within(railRow).findByText("Allowed")).toBeInTheDocument();

  expect(chartRenders.count).toBe(before);
});

// The other half of the same guarantee: a *real* change still repaints.
test("a new timeline still re-renders the chart", async () => {
  const client = makeQueryClient();
  render(
    <QueryClientProvider client={client}>
      <MemoryRouter>
        <Dashboard />
      </MemoryRouter>
    </QueryClientProvider>,
  );
  await screen.findByTestId("timeline-chart");
  const before = chartRenders.count;

  server.use(
    http.get("/api/v1/stats/timeline", () =>
      HttpResponse.json([{ bucket: hourStart(0), decisions: { forwarded: 99, blocked: 1 } }]),
    ),
  );
  await act(async () => {
    await client.invalidateQueries({ queryKey: ["stats", "timeline"] });
  });

  await waitFor(() => expect(seriesEl("Resolved").textContent).toContain("99"));
  expect(chartRenders.count).toBeGreaterThan(before);
});

// --- reserved height ---------------------------------------------------------

// A fresh instance sits with no traffic in the window for its first hour, and
// the hourly stats tables lag the query log by up to a minute — so the
// loading → empty → populated sequence is *guaranteed* to be watched. Letting
// the empty message collapse to one line of text shunted LIVE QUERIES, the
// whole right rail and the bottom rule ~200px up the page and dropped them
// back the moment the first bucket landed. jsdom has no layout to measure, so
// what's asserted is the mechanism: every state renders into the same box.
function plotFrame(): HTMLElement {
  const box = document.querySelector('[data-slot="query-volume-plot"]');
  if (!box) throw new Error("the query volume section rendered no plot frame");
  return box as HTMLElement;
}

test("the query volume section reserves its height while loading, when empty, and when drawn", async () => {
  let buckets: TimelineBucket[] = [];
  server.use(http.get("/api/v1/stats/timeline", () => HttpResponse.json(buckets)));

  const client = makeQueryClient();
  render(
    <QueryClientProvider client={client}>
      <MemoryRouter>
        <Dashboard />
      </MemoryRouter>
    </QueryClientProvider>,
  );

  // 1. loading — the skeleton is inside the box, not instead of it.
  expect(plotFrame().className).toContain("h-64");

  // 2. empty — the message is inside the same box.
  await screen.findByText(/no query activity yet/i);
  expect(plotFrame().className).toContain("h-64");
  expect(plotFrame()).toContainElement(screen.getByText(/no query activity yet/i));

  // 3. populated — so is the chart.
  buckets = [{ bucket: hourStart(0), decisions: { forwarded: 9, blocked: 1 } }];
  await act(async () => {
    await client.invalidateQueries({ queryKey: ["stats", "timeline"] });
  });
  await screen.findByTestId("timeline-chart");
  expect(plotFrame().className).toContain("h-64");
  expect(plotFrame()).toContainElement(screen.getByTestId("timeline-chart"));
});

test("an empty rail panel keeps the height its rows would have taken", async () => {
  server.use(http.get("/api/v1/stats/top", () => HttpResponse.json([])));

  renderWithProviders(<Dashboard />);

  await screen.findByText(/nothing blocked in this window yet/i);
  const bodies = [...document.querySelectorAll('[data-slot="rail-body"]')];
  // TOP BLOCKED holds 6 rows, TOP CLIENTS 5 — 32px each, so 48 and 40
  // spacing units. Empty must not collapse either of them.
  expect(bodies.map((b) => b.className)).toEqual(["min-h-48", "min-h-40"]);
});
