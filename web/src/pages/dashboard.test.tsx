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
import { LIVE_TAIL_CAP } from "../hooks/use-queries";
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

/**
 * The three bands, named as the page names them. Sentence case in the DOM,
 * uppercased by CSS in the legend — the same rule the chrome follows, and
 * the reason ECharts' own tooltip (which `text-transform` can't reach) is
 * still readable.
 */
const SERVED = "Forwarded + cached";
const BLOCKED = "Blocked";
const OTHER = "Authoritative + error";

function seriesEl(name: string): HTMLElement {
  const el = chart().querySelector(`[data-series="${name}"]`);
  if (!el) throw new Error(`no "${name}" series on the chart`);
  return el as HTMLElement;
}

function seriesNames(): (string | null)[] {
  return [...chart().querySelectorAll("[data-series]")].map((el) => el.getAttribute("data-series"));
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

/**
 * The StatCard whose title reads `title`, as its outermost Card element.
 * Scoped to the strip's own region: "Blocked" is a stat title *and* a chart
 * legend label, and the two are different things.
 */
function statCard(title: string): HTMLElement {
  const strip = screen.getByRole("region", { name: /^query stats/i });
  const card = within(strip).getByText(title).closest('[data-slot="card"]');
  if (!card) throw new Error(`no stat card titled ${title}`);
  return card as HTMLElement;
}

test("the stat strip shows total, blocked %, cache hit rate and client IPs", async () => {
  renderWithProviders(<Dashboard />);

  expect(await screen.findByText("1,000")).toBeInTheDocument(); // total
  expect(screen.getByText("25%")).toBeInTheDocument(); // blocked: 250/1000
  expect(screen.getByText("40%")).toBeInTheDocument(); // cached: 400/1000
  expect(screen.getByText("12")).toBeInTheDocument(); // clients

  // Each number is inside the card that names it, not merely somewhere on
  // the page — the four are otherwise indistinguishable.
  expect(statCard("Queries")).toHaveTextContent("1,000");
  expect(statCard("Blocked")).toHaveTextContent("25%");
  expect(statCard("Cached")).toHaveTextContent("40%");
  expect(statCard("Client IPs seen")).toHaveTextContent("12");
});

// Every one of these is a question the strip has been asked, and every
// answer is a real property of the API rather than a caption: `total` sums
// *every* decision (so it exceeds blocked + cached + forwarded), `cached`
// is cached + stale in the same handler, and `clients` counts distinct
// client_ip values — an unregistered device still counts.
test("each stat says what it actually counts", async () => {
  renderWithProviders(<Dashboard />);

  await screen.findByText("1,000");
  expect(statCard("Queries")).toHaveTextContent("every decision, incl. authoritative + error");
  expect(statCard("Blocked")).toHaveTextContent("250 blocked");
  expect(statCard("Cached")).toHaveTextContent("400 cached + stale");
  expect(statCard("Client IPs seen")).toHaveTextContent("distinct client_ip, not client rows");
});

// rnui's StatCard is built on Card, and Card brings a card's chrome. In a
// page made of nothing but hairlines that chrome is the one thing that must
// not appear — a ring around every cell turns the strip into four floating
// tiles, and `bg-card` is a visibly different surface from the page.
test("the stat cells keep the grid's hairlines and none of Card's own chrome", async () => {
  renderWithProviders(<Dashboard />);
  await screen.findByText("1,000");

  for (const title of ["Queries", "Blocked", "Cached", "Client IPs seen"]) {
    const cell = statCard(title);
    expect(cell.className).toContain("ring-0");
    expect(cell.className).toContain("bg-transparent");
    // The divider between cells is the thing the strip actually is.
    expect(cell.className).toContain("border-r");
    expect(cell.className).toContain("last:border-r-0");
  }
});

// `total` sums *every* decision the resolver writes — authoritative and
// error included — so blocked + cached + forwarded is legitimately less
// than it, and nothing on this page may derive an "allowed" count by
// subtraction.
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

test("the chart stacks three bands bottom-up, in square bars", async () => {
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
  // Declaration order is stacking order: what dnsaur served, then what it
  // blocked, then what was neither.
  expect(seriesNames()).toEqual([SERVED, BLOCKED, OTHER]);
  // ...and all three share one stack, or they'd be three charts.
  const stacks = new Set(
    [...chart().querySelectorAll("[data-series]")].map((el) => el.getAttribute("data-stack")),
  );
  expect(stacks.size).toBe(1);

  // BarChart's own default is itemStyle.borderRadius [4,4,0,0]; --radius is
  // 0 app-wide and a rounded cap inside a stack notches the band above it.
  for (const name of [SERVED, BLOCKED, OTHER]) {
    expect(seriesEl(name).getAttribute("data-radius")).toBe("0");
  }
});

// The third band is why this changed: `error` used to be buried under the
// same colour as a cache hit, so an upstream outage looked like traffic.
// The three bands still have to partition `total` exactly — the strip above
// sums every decision, and a chart that doesn't add up to it is lying.
test("errors and authoritative answers get their own band, and the three still total the strip", async () => {
  server.use(
    http.get("/api/v1/stats/overview", () =>
      HttpResponse.json({ total: 24, blocked: 5, cached: 4, forwarded: 10, clients: 3 }),
    ),
    http.get("/api/v1/stats/timeline", () =>
      HttpResponse.json([
        {
          bucket: hourStart(0),
          decisions: { forwarded: 10, blocked: 5, error: 3, cached: 2, stale: 2, authoritative: 2 },
        },
      ]),
    ),
  );

  renderWithProviders(<Dashboard />, { route: "/?window=1h" });

  // 10 forwarded + 2 cached + 2 stale = 14 served; 5 blocked; 3 error + 2
  // authoritative = 5 other. 14 + 5 + 5 = 24, the total the strip reports.
  expect(await screen.findByText(`${SERVED}: 14`)).toBeInTheDocument();
  expect(screen.getByText(`${BLOCKED}: 5`)).toBeInTheDocument();
  expect(screen.getByText(`${OTHER}: 5`)).toBeInTheDocument();
  expect(screen.getByRole("figure", { name: /24 queries/ })).toBeInTheDocument();
});

// The remainder band is deliberately "everything the bucket contained that
// the other two didn't claim", not a hardcoded {authoritative, error}: a
// decision the resolver grows later must show up somewhere rather than
// being silently dropped out of a chart that claims to total the strip.
test("a decision the page has never heard of still lands in a band", async () => {
  server.use(
    http.get("/api/v1/stats/timeline", () =>
      HttpResponse.json([
        { bucket: hourStart(0), decisions: { forwarded: 4, blocked: 1, teleported: 7 } },
      ]),
    ),
  );

  renderWithProviders(<Dashboard />, { route: "/?window=1h" });

  expect(await screen.findByText(`${OTHER}: 7`)).toBeInTheDocument();
  expect(screen.getByText(`${SERVED}: 4`)).toBeInTheDocument();
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
  expect(seriesEl(SERVED).textContent).toBe(`${SERVED}: 4,0,0,8`);
  expect(seriesEl(BLOCKED).textContent).toBe(`${BLOCKED}: 1,0,0,2`);
  expect(chart().getAttribute("data-buckets")).toBe("4");
});

/** The category labels the page handed the chart, off the merged option. */
function categories(): string[] {
  return chartOption<{ xAxis: { data: string[] } }>().xAxis.data;
}

// `from` is a raw unix second (`now - hours`) compared against hour-aligned
// bucket starts, so the server drops the partially covered oldest hour whole
// (ui-contract §4). An axis that starts at `floor(now − hours)` therefore
// always opens on an hour the API cannot report — a fabricated zero at the
// left edge of every window.
test("the axis starts at the oldest hour the server can actually report", async () => {
  server.use(
    http.get("/api/v1/stats/timeline", () =>
      HttpResponse.json([{ bucket: hourStart(0), decisions: { forwarded: 9 } }]),
    ),
  );

  renderWithProviders(<Dashboard />, { route: "/?window=24h" });
  await screen.findByTestId("timeline-chart");

  // 24 hours the server can answer for, not 25 with a phantom at the front.
  expect(chart().getAttribute("data-buckets")).toBe("24");
});

// 7d is 168 hourly bars. A date-only label gives all 24 of a day's bars the
// same category, so neither the axis nor ECharts' own tooltip can say which
// hour a bar is.
test("a multi-day window labels each bar with its hour, not just its date", async () => {
  server.use(
    http.get("/api/v1/stats/timeline", () =>
      HttpResponse.json([
        { bucket: hourStart(0), decisions: { forwarded: 9 } },
        { bucket: hourStart(1), decisions: { forwarded: 4 } },
      ]),
    ),
  );

  renderWithProviders(<Dashboard />, { route: "/?window=7d" });
  await screen.findByTestId("timeline-chart");

  const labels = categories();
  expect(labels).toHaveLength(168);
  expect(new Set(labels).size).toBe(168);
});

test("a same-day window keeps the clock-only label", async () => {
  renderWithProviders(<Dashboard />, { route: "/?window=24h" });
  await screen.findByTestId("timeline-chart");

  // No date component — 24 bars never span more than two days, and the
  // window selector above already says which.
  const month = new Date(hourStart(0) * 1000).toLocaleDateString([], { month: "short" });
  for (const label of categories()) expect(label).not.toContain(month);
});

test("the header names the real granularity, admits the lag, and legends all three bands", async () => {
  renderWithProviders(<Dashboard />);
  await screen.findByTestId("timeline-chart");

  // The design says "15-min"; the endpoint only ever produces hour buckets.
  // And these bars are written from the log's batched flush, so the newest
  // minute is on the live feed below before it is up here.
  expect(screen.getByText("hourly buckets · stats lag the log by up to 60s")).toBeInTheDocument();
  expect(screen.queryByText(/15-min/i)).not.toBeInTheDocument();

  const header = screen.getByText("Query volume").closest("div");
  expect(header).not.toBeNull();
  // The legend reads bottom-up, in the order the bands stack.
  expect(
    [...header!.querySelectorAll("span > span:last-child")]
      .map((el) => el.textContent)
      .filter((t) => [SERVED, BLOCKED, OTHER].includes(t ?? "")),
  ).toEqual([SERVED, BLOCKED, OTHER]);
});

test("the chart carries a text alternative naming all three totals", async () => {
  renderWithProviders(<Dashboard />);

  // Default fixture: 5 buckets × {authoritative:120, blocked:30, cached:90,
  // forwarded:40, stale:5} — 135 served, 30 blocked and 120 unrecognised
  // per bucket.
  const figure = await screen.findByRole("figure");
  expect(figure).toHaveAccessibleName(/675 forwarded or cached/);
  expect(figure).toHaveAccessibleName(/150 blocked/);
  expect(figure).toHaveAccessibleName(/600 authoritative or error/);
  expect(figure).toHaveAccessibleName(/1,425 queries/);
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

  const colors = [SERVED, BLOCKED, OTHER].map(
    (name) => seriesEl(name).getAttribute("data-color") ?? "",
  );
  for (const color of colors) {
    expect(color).not.toContain("var(");
    expect(color).not.toBe("");
  }
  // Three bands in one stack are only three bands if they're three colours.
  expect(new Set(colors).size).toBe(3);

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
  const before = [SERVED, BLOCKED, OTHER].map((n) => seriesEl(n).getAttribute("data-color"));

  await user.click(screen.getByRole("button", { name: /go dark/i }));

  await waitFor(() => expect(seriesEl(BLOCKED).getAttribute("data-color")).not.toBe(before[1]));
  // All three tokens are redefined in dark, the newest one included.
  for (const [i, name] of [SERVED, BLOCKED, OTHER].entries()) {
    const color = seriesEl(name).getAttribute("data-color");
    expect(color).not.toBe(before[i]);
    expect(color).not.toContain("var(");
  }
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

test("the panel header says where the rows come from and which end is new", async () => {
  renderWithProviders(<Dashboard />);
  await emitAll(await firstSource(), [entry({ id: 1, q_name: "one.example" })]);

  const header = screen.getByText("Live queries").closest("div")!;
  // rnui's StatusIndicator, live, rather than a second hand-rolled dot.
  const dot = header.querySelector('[data-slot="status-indicator"]');
  expect(dot).not.toBeNull();
  expect(dot).toHaveAttribute("data-state", "active");
  // The buffer size is the hook's, not a number typed twice.
  expect(within(header).getByText(`SSE · ${LIVE_TAIL_CAP}-row buffer`)).toBeInTheDocument();
  expect(within(header).getByText("newest first")).toBeInTheDocument();
});

test("the table carries all eight columns, in the design's order", async () => {
  renderWithProviders(<Dashboard />);
  await emitAll(await firstSource(), [entry({ id: 1, q_name: "one.example" })]);

  expect(screen.getAllByRole("columnheader").map((th) => th.textContent)).toEqual([
    "Time",
    "Domain",
    "Type",
    "Client IP",
    "Hostname",
    "Decision",
    "Upstream",
    "ms",
  ]);
});

test("each row shows time, domain, type, client, decision, upstream and duration", async () => {
  renderWithProviders(<Dashboard />);
  const source = await firstSource();
  await emitAll(source, [
    entry({
      id: 1,
      q_name: "one.example",
      q_type: "AAAA",
      client_ip: "10.0.0.4",
      decision: "cached",
      upstream: "9.9.9.9",
      duration_ms: 3,
    }),
  ]);

  const row = liveRow("one.example");
  expect(within(row).getByText("AAAA")).toBeInTheDocument();
  expect(within(row).getByText("10.0.0.4")).toBeInTheDocument();
  expect(within(row).getByText("cached")).toBeInTheDocument();
  expect(within(row).getByText("9.9.9.9")).toBeInTheDocument();
  // The column is headed MS, so the unit is in the header and not repeated
  // on every one of twelve rows.
  expect(within(row).getByText("3")).toBeInTheDocument();
});

// duration_ms is truncated whole milliseconds, so a cache hit answered in
// 180µs is logged as 0 — and "0" in a column headed MS claims an
// instantaneous resolve. lib/query-rows.ts owns that rule for both screens.
test("a sub-millisecond answer reads <1, never 0", async () => {
  renderWithProviders(<Dashboard />);
  await emitAll(await firstSource(), [
    entry({ id: 1, q_name: "fast.example", decision: "cached", duration_ms: 0 }),
  ]);

  const row = liveRow("fast.example");
  expect(within(row).getByText("<1")).toBeInTheDocument();
  expect(within(row).queryByText("0")).not.toBeInTheDocument();
});

// A query row carries `client_id` — the registry entry whose matcher the
// address hit, or 0 when nothing matched — so a hostname exists only if
// GET /clients can be asked for it. Anything unresolvable renders an em
// dash; reverse-DNS or falling back to the IP would be inventing a name.
test("HOSTNAME resolves through the client registry, and shows an em dash when it can't", async () => {
  server.use(
    http.get("/api/v1/clients", () =>
      HttpResponse.json([{ id: 7, name: "Laptop", matcher: "192.168.1.10", group_id: 1 }]),
    ),
  );

  renderWithProviders(<Dashboard />);
  const source = await firstSource();
  await emitAll(source, [
    entry({ id: 1, q_name: "known.example", client_ip: "192.168.1.10", client_id: 7 }),
    // Nothing in the registry matched this address at query time.
    entry({ id: 2, q_name: "unknown.example", client_ip: "192.168.11.63", client_id: 0 }),
    // Registered once, deleted since — the id no longer resolves.
    entry({ id: 3, q_name: "stale-id.example", client_ip: "192.168.1.99", client_id: 42 }),
  ]);

  expect(within(liveRow("known.example")).getByText("Laptop")).toBeInTheDocument();
  expect(within(liveRow("unknown.example")).getByText("—")).toBeInTheDocument();
  expect(within(liveRow("stale-id.example")).getByText("—")).toBeInTheDocument();
  // Never the address as a stand-in for a name.
  expect(within(liveRow("unknown.example")).getAllByText("192.168.11.63")).toHaveLength(1);
});

// A client's name is not validated server-side and may be empty
// (ui-contract §3.4 — the Add-client form allows it too). An empty string in
// the lookup map is a hit, so `?? EM_DASH` never fires and the cell renders
// blank, which reads as a broken table rather than as "no name".
test("a client with no name still renders an em dash, not an empty cell", async () => {
  server.use(
    http.get("/api/v1/clients", () =>
      HttpResponse.json([{ id: 7, name: "", matcher: "192.168.1.10", group_id: 1 }]),
    ),
  );

  renderWithProviders(<Dashboard />);
  const source = await firstSource();
  await emitAll(source, [
    entry({ id: 0, q_name: "nameless.example", client_ip: "192.168.1.10", client_id: 7 }),
  ]);

  expect(within(liveRow("nameless.example")).getByText("—")).toBeInTheDocument();
});

// The rail labels a top client by its registry name where one exists; an
// empty name is not one, and the address it really was beats a blank row.
test("a top client with no name falls back to its address", async () => {
  server.use(
    http.get("/api/v1/clients", () =>
      HttpResponse.json([{ id: 1, name: "", matcher: "192.168.1.10", group_id: 1 }]),
    ),
  );

  renderWithProviders(<Dashboard />);

  const rail = await screen.findByRole("region", { name: "Top client IPs" });
  expect(await within(rail).findByText("192.168.1.10")).toBeInTheDocument();
});

// The decision vocabulary is the resolver's (internal/dnssrv/pipeline.go).
// There is no `allowed` — no such decision exists — so nothing here may
// invent one.
test("each decision gets its own tone, and blocked/error are the loud ones", async () => {
  renderWithProviders(<Dashboard />);
  const source = await firstSource();
  await emitAll(source, [
    entry({ id: 1, q_name: "blocked.example", decision: "blocked" }),
    entry({ id: 2, q_name: "error.example", decision: "error" }),
    entry({ id: 3, q_name: "stale.example", decision: "stale" }),
    entry({ id: 4, q_name: "cached.example", decision: "cached" }),
    entry({ id: 5, q_name: "forwarded.example", decision: "forwarded" }),
    entry({ id: 6, q_name: "authoritative.example", decision: "authoritative" }),
  ]);

  const toneOf = (decision: string) =>
    screen.getByText(decision, { selector: "td" }).className ?? "";
  expect(toneOf("blocked")).toContain("text-destructive");
  expect(toneOf("error")).toContain("text-destructive");
  expect(toneOf("stale")).toContain("text-warn");
  expect(toneOf("cached")).toContain("text-muted-foreground");
  expect(toneOf("forwarded")).toContain("text-foreground");
  expect(toneOf("authoritative")).toContain("text-primary");
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
    // id 0, the shape every row on the stream actually has.
    for (let i = 1; i <= 20; i += 1) source.emit(entry({ id: 0, q_name: `h${i}.example` }));
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

/** The number the readout is currently claiming, as a number. */
function arrivalRate(): number {
  return Number.parseFloat(screen.getByText(/q\/s/i).textContent ?? "");
}

// Every row on the stream carries `id: 0` — internal/qlog/qlog.go publishes
// the entry to the SSE hub before the batched insert assigns a primary key
// (ui-contract §3.1). Detecting "new" rows by id therefore counted the first
// batch and nothing ever again: `0 <= lastSeenId` for every row after it, so
// the readout sat at 0.0 q/s under real traffic.
test("arrivals are counted by identity, so a second batch of id-0 rows still registers", async () => {
  vi.useFakeTimers({ shouldAdvanceTime: true });

  renderWithProviders(<Dashboard />);
  const source = await firstSource();
  act(() => source.emitOpen());
  act(() => {
    for (let i = 1; i <= 5; i += 1) source.emit(entry({ id: 0, q_name: `first${i}.example` }));
  });
  // Two advances, not one: the first closes the tail's 100ms coalescing
  // window so the batch is actually observed, the second lets the rate
  // timer tick with it recorded — and takes the readout past its warm-up.
  await act(async () => {
    await vi.advanceTimersByTimeAsync(500);
  });
  await act(async () => {
    await vi.advanceTimersByTimeAsync(11_000);
  });
  expect(arrivalRate()).toBeGreaterThan(0);

  // Past the 30s rate window, so the first batch has aged out and the
  // readout is measuring nothing but what comes next.
  await act(async () => {
    await vi.advanceTimersByTimeAsync(40_000);
  });
  expect(arrivalRate()).toBe(0);

  act(() => {
    for (let i = 1; i <= 15; i += 1) source.emit(entry({ id: 0, q_name: `second${i}.example` }));
  });
  await act(async () => {
    await vi.advanceTimersByTimeAsync(500);
  });
  await act(async () => {
    await vi.advanceTimersByTimeAsync(2_000);
  });

  expect(arrivalRate()).toBeGreaterThan(0);
});

// The tail is seeded once from GET /queries so the panel isn't empty on a
// busy instance. Those rows are history that arrived in one lump, not
// traffic measured over the window — counting them would open every
// dashboard on a fabricated burst.
test("the seeded page of history is not counted as arrivals", async () => {
  vi.useFakeTimers({ shouldAdvanceTime: true });
  server.use(
    http.get("/api/v1/queries", () =>
      HttpResponse.json(
        Array.from({ length: 60 }, (_, i) => entry({ id: i + 1, q_name: `seed${i}.example` })),
      ),
    ),
  );

  renderWithProviders(<Dashboard />);
  const source = await firstSource();
  act(() => source.emitOpen());
  await waitFor(() => expect(screen.queryByText("seed0.example")).toBeInTheDocument());

  await act(async () => {
    await vi.advanceTimersByTimeAsync(11_000);
  });

  expect(arrivalRate()).toBe(0);
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

// The two panels look identical and count different things:
// /stats/top?metric=blocked_domain counts blocks only, while metric=client
// counts every decision that client made. Without the notes the obvious
// reading — "these are the clients doing the blocked lookups" — is wrong.
test("each rail panel says which decisions its numbers count", async () => {
  renderWithProviders(<Dashboard />);
  await screen.findByText("ads.tracker.example");

  const blocked = screen.getByRole("region", { name: "Top blocked" });
  expect(within(blocked).getByText("blocked only")).toBeInTheDocument();

  // "Client IPs", not "Clients": the metric groups by address, so one
  // registered device with two addresses is two rows.
  const clients = screen.getByRole("region", { name: "Top client IPs" });
  expect(within(clients).getByText("all decisions")).toBeInTheDocument();
});

// The fill is a ranking you can read without reading a number, so it has to
// sit under the label without swallowing it — `--accent` is the theme's own
// tint surface and half strength keeps the text legible in both modes.
test("the magnitude fills are the accent wash, not a solid bar", async () => {
  renderWithProviders(<Dashboard />);
  const top = (await screen.findByText("ads.tracker.example")).closest("li")!;

  expect(top.querySelector("[aria-hidden]")!.className).toContain("bg-accent/50");
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

// The status map is keyed by domain, so every cell showing that domain
// confirms the same write — but each cell used to label the confirmation
// from *its own* action, and the two disagree. Blocking a forwarded live row
// left the Top-blocked row for the same domain reading "Allowed".
test("the confirmation names the action that was taken, wherever the domain appears", async () => {
  const user = userEvent.setup();
  server.use(
    http.post("/api/v1/groups/1/rules", () => HttpResponse.json({ id: 1 }, { status: 201 })),
  );

  renderWithProviders(<Dashboard />);
  const source = await firstSource();
  // The same domain the Top-blocked rail already lists, arriving forwarded.
  // Emitted by hand rather than through emitAll, which waits on a text
  // match the rail would satisfy on its own.
  act(() => source.emitOpen());
  act(() => source.emit(entry({ id: 0, q_name: "ads.tracker.example" })));

  const live = screen.getByRole("region", { name: "Live queries" });
  const row = (await within(live).findByText("ads.tracker.example")).closest("tr")!;
  await user.click(within(row).getByRole("button", { name: "Block" }));

  expect(await within(row).findByText("Blocked")).toBeInTheDocument();
  const rail = screen.getByRole("region", { name: "Top blocked" });
  expect(within(rail).getByText("Blocked")).toBeInTheDocument();
  expect(within(rail).queryByText("Allowed")).not.toBeInTheDocument();
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

  await waitFor(() => expect(seriesEl(SERVED).textContent).toContain("99"));
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
  expect(plotFrame().className).toContain("h-52");

  // 2. empty — the message is inside the same box.
  await screen.findByText(/no query activity yet/i);
  expect(plotFrame().className).toContain("h-52");
  expect(plotFrame()).toContainElement(screen.getByText(/no query activity yet/i));

  // 3. populated — so is the chart.
  buckets = [{ bucket: hourStart(0), decisions: { forwarded: 9, blocked: 1 } }];
  await act(async () => {
    await client.invalidateQueries({ queryKey: ["stats", "timeline"] });
  });
  await screen.findByTestId("timeline-chart");
  expect(plotFrame().className).toContain("h-52");
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
