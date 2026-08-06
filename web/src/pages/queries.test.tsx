import { act } from "react";
import { http, HttpResponse } from "msw";
import { toast } from "sonner";
import { afterEach, beforeAll, beforeEach, expect, test, vi } from "vitest";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { server } from "../test/msw-server";
import { renderWithProviders } from "../test/render";
import { FakeEventSource } from "../test/fake-event-source";
import type { QueryEntry } from "../api/types";
import { setLiveTailPaused } from "../lib/live-tail";
import { TopNav } from "../components/top-nav";
import { QueryLog, renderCounts } from "./queries";

// The table is virtualized (@tanstack/react-virtual, via rnui's
// DataGridTableVirtual) — it sizes its visible row window from the scroll
// container's offsetWidth/offsetHeight (see @tanstack/virtual-core's
// `getRect`), not from getBoundingClientRect. jsdom has no layout engine and
// always reports 0 for both, which makes the virtualizer compute an empty
// visible range and render no rows at all. Stub a generous fixed viewport
// (and getBoundingClientRect too, since the filter row's Select popup also
// reads it) so every test in this file gets a real, non-empty set of
// rendered rows to assert against — scoped to this file, not global setup.
beforeAll(() => {
  for (const prop of [
    "offsetHeight",
    "offsetWidth",
    "clientHeight",
    "clientWidth",
    "scrollHeight",
    "scrollWidth",
  ] as const) {
    Object.defineProperty(HTMLElement.prototype, prop, {
      configurable: true,
      get: () => (prop.endsWith("Width") ? 1000 : prop === "scrollHeight" ? 20_000 : 800),
    });
  }
  HTMLElement.prototype.getBoundingClientRect = () =>
    ({
      width: 1000,
      height: 800,
      top: 0,
      left: 0,
      right: 1000,
      bottom: 800,
      x: 0,
      y: 0,
      toJSON() {},
    }) as DOMRect;
});

beforeEach(() => {
  FakeEventSource.instances = [];
  vi.stubGlobal("EventSource", FakeEventSource);
  // The pause flag is module state shared with the chrome (lib/live-tail.ts),
  // so it outlives any one test case and has to be put back by hand.
  setLiveTailPaused(false);
});

// Toast spies are per-test; without this they accumulate calls across the
// whole file and "was never called" assertions can never fail.
afterEach(() => vi.restoreAllMocks());

/**
 * The live/pause toggle is the chrome's cell, not the page's, so the shell
 * row that carries it is rendered alongside the page — which is also the
 * only way the cross-component wiring in lib/live-tail.ts gets exercised at
 * all. Route matters: the chrome only hangs those cells off `/queries`.
 */
function renderQueryLog() {
  return renderWithProviders(
    <>
      <TopNav onOpenCommandPalette={() => {}} />
      <QueryLog />
    </>,
    { route: "/queries" },
  );
}

/**
 * A row as it arrives on the *stream*: `id` 0, because
 * internal/qlog/qlog.go publishes to the SSE hub before the batched insert
 * assigns a primary key. Tests that want a database row pass an explicit id.
 */
function entry(overrides: Partial<QueryEntry> = {}): QueryEntry {
  return {
    id: 0,
    at: Date.now(),
    instance_id: "i1",
    client_ip: "192.168.1.10",
    client_id: 1,
    q_name: "example.com",
    q_type: "A",
    decision: "cached",
    rule_id: 0,
    list_id: 0,
    upstream: "1.1.1.1",
    r_code: "NOERROR",
    duration_ms: 12,
    ...overrides,
  };
}

async function firstSource() {
  await waitFor(() => expect(FakeEventSource.instances.length).toBeGreaterThan(0));
  return FakeEventSource.instances[0]!;
}

/** The rail on the right — the inspector that replaced the "why?" drawer. */
function inspector(): HTMLElement {
  return screen.getByRole("complementary", { name: /why this decision/i });
}

/** Selecting a row is what opens the inspector on it. */
async function selectRow(user: ReturnType<typeof userEvent.setup>, domain: string | RegExp) {
  await user.click(
    await screen.findByRole("button", { name: new RegExp(`why was ${domain}`, "i") }),
  );
}

/** Type a domain substring and let the 300ms debounce land. */
async function searchDomains(text: string) {
  fireEvent.change(screen.getByRole("textbox", { name: /search domains/i }), {
    target: { value: text },
  });
}

/** Put a value in one of the free-text filter cells. */
function setFilterCell(label: RegExp, value: string) {
  fireEvent.change(screen.getByRole("textbox", { name: label }), { target: { value } });
}

// --- the live tail -----------------------------------------------------------

// Every row on the stream carries id 0 (see `entry`). Keying rows on
// `entry.id` collapses the whole live tail onto one identity — react-table's
// row model, React's list keys, the virtualizer's item keys, the "is this
// the selected row" comparison and the per-row action status all see "0" for
// every row. The rows still *render* (React only warns about duplicate
// keys), so the way that shows up is per-row state smearing across the whole
// table: selecting one row marks all of them. Emitting rows the way the
// server really does is what makes this a guard rather than a comment.
test("live rows keep separate identities even though they all arrive with id 0", async () => {
  const user = userEvent.setup();
  renderQueryLog();

  const source = await firstSource();
  act(() => source.emitOpen());
  act(() => source.emit(entry({ q_name: "one.example.com" })));
  expect(await screen.findByText("one.example.com")).toBeInTheDocument();

  act(() => source.emit(entry({ q_name: "two.example.com" })));
  act(() => source.emit(entry({ q_name: "three.example.com" })));

  expect(await screen.findByText("three.example.com")).toBeInTheDocument();
  expect(screen.getByText("two.example.com")).toBeInTheDocument();
  expect(screen.getByText("one.example.com")).toBeInTheDocument();
  // Three distinct rows, not one row rendered three times.
  expect(screen.getAllByText(/^(one|two|three)\.example\.com$/)).toHaveLength(3);

  // And they are three distinct *rows*: selecting one marks exactly one.
  await selectRow(user, "two\\.example\\.com");
  await waitFor(() => expect(document.querySelectorAll("[data-selected]")).toHaveLength(1));
  expect(
    screen
      .getByRole("button", { name: /why was two\.example\.com/i })
      .closest("tr")!
      .querySelector("[data-selected]"),
  ).not.toBeNull();
});

test("pausing from the chrome holds the tail without dropping what's on screen, and resuming doesn't duplicate it", async () => {
  const user = userEvent.setup();
  renderQueryLog();

  const source = await firstSource();
  act(() => source.emitOpen());
  act(() => source.emit(entry({ q_name: "kept.example.com" })));
  expect(await screen.findByText("kept.example.com")).toBeInTheDocument();

  const toggle = screen.getByRole("switch", { name: /live tail/i });
  expect(toggle).toHaveAttribute("aria-checked", "true");
  await user.click(toggle);
  await waitFor(() => expect(toggle).toHaveAttribute("aria-checked", "false"));
  expect(await screen.findByText(/^Paused$/)).toBeInTheDocument();

  // The old stream is torn down on pause; emitting on it must not append.
  act(() => source.emit(entry({ q_name: "dropped.example.com" })));
  await new Promise((resolve) => setTimeout(resolve, 50));
  expect(screen.queryByText("dropped.example.com")).not.toBeInTheDocument();
  expect(screen.getByText("kept.example.com")).toBeInTheDocument();

  // Resuming reopens a *new* stream and prepends only what arrives next —
  // the buffered row is neither lost nor replayed.
  await user.click(toggle);
  await waitFor(() => expect(FakeEventSource.instances).toHaveLength(2));
  const resumed = FakeEventSource.instances[1]!;
  act(() => resumed.emitOpen());
  act(() => resumed.emit(entry({ q_name: "after.example.com" })));

  expect(await screen.findByText("after.example.com")).toBeInTheDocument();
  expect(screen.getAllByText("kept.example.com")).toHaveLength(1);
});

test("a dropped stream is reported as reconnecting", async () => {
  renderQueryLog();

  const source = await firstSource();
  act(() => source.emitOpen());
  expect(await screen.findByText(/streaming/i)).toBeInTheDocument();

  act(() => source.emitError());
  expect(await screen.findByText(/reconnecting/i)).toBeInTheDocument();
});

// api/sse.ts gives up after MAX_CONSECUTIVE_FAILURES (6) errors with no
// successful open in between, because EventSource.onerror carries no status
// code and an expired session is indistinguishable from a network blip.
// Retrying forever would leave a dead tab spinning "Reconnecting…" — so the
// terminal state has to be reachable, visible, and recoverable by hand.
test("a stream that gives up says so and offers a manual reconnect", async () => {
  vi.useFakeTimers();
  try {
    renderQueryLog();
    await vi.waitFor(() => expect(FakeEventSource.instances.length).toBeGreaterThan(0));

    // Errors 1..5 each schedule a retry at a doubling backoff; the sixth is
    // terminal.
    for (const backoff of [1_000, 2_000, 4_000, 8_000, 16_000]) {
      const source = FakeEventSource.instances.at(-1)!;
      act(() => source.emitError());
      act(() => void vi.advanceTimersByTime(backoff));
    }
    act(() => FakeEventSource.instances.at(-1)!.emitError());

    await vi.waitFor(() => expect(screen.getByText(/live tail disconnected/i)).toBeInTheDocument());
    const opened = FakeEventSource.instances.length;

    act(() => void fireEvent.click(screen.getByRole("button", { name: /reconnect/i })));
    expect(FakeEventSource.instances.length).toBe(opened + 1);
  } finally {
    vi.useRealTimers();
  }
});

test("unmounting the page closes the live stream", async () => {
  const { unmount } = renderQueryLog();

  const source = await firstSource();
  act(() => source.emitOpen());
  expect(source.closed).toBe(false);

  unmount();

  expect(source.closed).toBe(true);
});

test("a fresh install is told the tail is listening rather than shown an empty table", async () => {
  renderQueryLog();

  const source = await firstSource();
  act(() => source.emitOpen());

  // DataGridTableVirtual has its own empty branch, separate from the plain
  // grid's — the exact path that regressed twice on this branch.
  expect(await screen.findByText(/listening — queries appear here/i)).toBeInTheDocument();
});

// --- the table ---------------------------------------------------------------

// duration_ms is time.Duration.Milliseconds() — truncated whole
// milliseconds — so a sub-millisecond cache hit is logged as 0. Rendering
// that as "0" claims an instantaneous resolve that nothing measured.
test("a sub-millisecond query renders as <1, not 0", async () => {
  const user = userEvent.setup();
  renderQueryLog();

  const source = await firstSource();
  act(() => source.emitOpen());
  act(() => source.emit(entry({ q_name: "fast.example.com", duration_ms: 0 })));

  const row = (await screen.findByText("fast.example.com")).closest("tr")!;
  expect(within(row).getByText("<1")).toBeInTheDocument();
  expect(within(row).queryByText("0")).not.toBeInTheDocument();

  await selectRow(user, "fast.example.com");
  expect(within(inspector()).getByText(/A · NOERROR · <1 ms/)).toBeInTheDocument();
});

test("the selected row is marked, and clearing the selection unmarks it", async () => {
  const user = userEvent.setup();
  renderQueryLog();

  const source = await firstSource();
  act(() => source.emitOpen());
  act(() => source.emit(entry({ q_name: "picked.example.com" })));

  const row = () =>
    screen.getByRole("button", { name: /why was picked\.example\.com/i }).closest("tr")!;
  await waitFor(() => expect(row()).toBeInTheDocument());
  expect(row().querySelector("[data-selected]")).toBeNull();

  await selectRow(user, "picked\\.example\\.com");
  // The marker is one element doing two jobs: the 2px leading edge, and the
  // hook `has-data-selected:bg-card` on the row tints itself from.
  await waitFor(() => expect(row().querySelector("[data-selected]")).not.toBeNull());
  expect(row().className).toContain("has-data-selected:bg-card");

  await user.click(within(inspector()).getByRole("button", { name: /clear the selected row/i }));
  await waitFor(() => expect(row().querySelector("[data-selected]")).toBeNull());
});

test("the table is virtualized: rows far past the scroll window aren't mounted until scrolled into view", async () => {
  const many = Array.from({ length: 50 }, (_, i) =>
    entry({ id: i + 1, q_name: `host-${i}.example.com`, client_ip: "10.0.0.1" }),
  );
  server.use(
    http.get("/api/v1/queries", ({ request }) => {
      const url = new URL(request.url);
      return HttpResponse.json(url.searchParams.get("client") === "10.0.0.1" ? many : []);
    }),
  );

  renderQueryLog();
  await firstSource();
  setFilterCell(/client/i, "10.0.0.1");

  expect(await screen.findByText("host-0.example.com")).toBeInTheDocument();
  // With an ~800px stubbed viewport and ~29px rows (plus overscan), only
  // some of the 50 rows are mounted at scroll-top — the last one must not
  // be in the DOM yet if virtualization is actually active (a plain,
  // non-virtualized table would mount all 50).
  expect(screen.queryByText("host-49.example.com")).not.toBeInTheDocument();

  const viewport = document.querySelector('[data-slot="scroll-area-viewport"]');
  if (!viewport) throw new Error("scroll viewport not found");
  fireEvent.scroll(viewport, { target: { scrollTop: 5000 } });

  await waitFor(() => expect(screen.getByText("host-49.example.com")).toBeInTheDocument());
});

// --- filters -----------------------------------------------------------------

test("typing a domain substring searches on q and renders the matches", async () => {
  const urls: string[] = [];
  server.use(
    http.get("/api/v1/queries", ({ request }) => {
      urls.push(request.url);
      const url = new URL(request.url);
      if (url.searchParams.get("q") === "ads") {
        return HttpResponse.json([entry({ id: 9, q_name: "ads.example", decision: "blocked" })]);
      }
      return HttpResponse.json([]);
    }),
  );

  renderQueryLog();
  await firstSource();
  await searchDomains("ads");

  await waitFor(() => expect(urls.at(-1)).toContain("q=ads"));
  expect(await screen.findByText("ads.example")).toBeInTheDocument();
});

// The hint is literally true, and has to stay that way: escapeLike in
// internal/store/search.go *strips* % and _ rather than escaping them,
// because the escape syntax isn't portable across the two SQL dialects.
test("the search cell says what the server actually does with % and _", async () => {
  renderQueryLog();
  await firstSource();

  const box = screen.getByRole("textbox", { name: /search domains/i });
  const hint = screen.getByText(/% and _ are ignored/i);
  expect(box).toHaveAttribute("aria-describedby", hint.id);
  expect(hint).toHaveTextContent("substring of q_name — % and _ are ignored");
});

// DecisionAllowed exists in the Go enum (internal/dnssrv/pipeline.go) but is
// never assigned: an allow rule only *skips* blocking, so the row is logged
// with whatever the downstream stage produced. Offering it advertised a
// filter that always returns zero rows.
test("the decision filter offers the six decisions the resolver writes, and never 'allowed'", async () => {
  renderQueryLog();
  await firstSource();

  fireEvent.click(screen.getByRole("combobox", { name: /decision/i }));
  const options = await screen.findAllByRole("option");

  expect(options.map((o) => o.textContent)).toEqual([
    "any",
    "blocked",
    "forwarded",
    "cached",
    "stale",
    "local",
    "error",
  ]);
});

test("picking a decision calls GET /queries?decision=… and renders the results", async () => {
  const user = userEvent.setup();
  const urls: string[] = [];
  server.use(
    http.get("/api/v1/queries", ({ request }) => {
      urls.push(request.url);
      const url = new URL(request.url);
      if (url.searchParams.get("decision") === "blocked") {
        return HttpResponse.json([
          entry({ id: 9, q_name: "blocked-domain.example", decision: "blocked", rule_id: 5 }),
        ]);
      }
      return HttpResponse.json([]);
    }),
  );

  renderQueryLog();
  await firstSource();

  // base-ui's Select commits on the full pointer sequence, not on a bare
  // synthetic click — userEvent replays that sequence, fireEvent doesn't.
  await user.click(screen.getByRole("combobox", { name: /decision/i }));
  await user.click(await screen.findByRole("option", { name: "blocked" }));

  await waitFor(() => expect(urls.at(-1) ?? "").toContain("decision=blocked"));
  expect(await screen.findByText("blocked-domain.example")).toBeInTheDocument();
});

test("clearing filters returns to the live tail: the stream reopens and live rows resume", async () => {
  const user = userEvent.setup();
  server.use(
    http.get("/api/v1/queries", () =>
      HttpResponse.json([entry({ id: 99, q_name: "paged-result.example.com" })]),
    ),
  );

  renderQueryLog();
  await firstSource();
  expect(FakeEventSource.instances).toHaveLength(1);

  // A filter switches to paged mode, tearing down the live stream.
  setFilterCell(/client/i, "10.0.0.1");
  expect(await screen.findByText("paged-result.example.com")).toBeInTheDocument();
  expect(screen.getByText(/filtered · stream paused/i)).toBeInTheDocument();

  await user.click(screen.getByRole("button", { name: /clear filters/i }));

  await waitFor(() => expect(FakeEventSource.instances).toHaveLength(2));
  expect(screen.queryByText("paged-result.example.com")).not.toBeInTheDocument();

  const resumed = FakeEventSource.instances[1]!;
  act(() => resumed.emitOpen());
  act(() => resumed.emit(entry({ q_name: "resumed-live.example.com" })));

  expect(await screen.findByText("resumed-live.example.com")).toBeInTheDocument();
});

// --- paging ------------------------------------------------------------------

function pagedHandler(pageFor: (offset: number) => QueryEntry[], urls: string[]) {
  return http.get("/api/v1/queries", ({ request }) => {
    const url = new URL(request.url);
    urls.push(request.url);
    if (url.searchParams.get("client") !== "10.0.0.1") return HttpResponse.json([]);
    return HttpResponse.json(pageFor(Number(url.searchParams.get("offset") ?? 0)));
  });
}

function page(start: number, count: number): QueryEntry[] {
  return Array.from({ length: count }, (_, i) =>
    entry({ id: start + i, q_name: `hit-${start + i}.example.com`, client_ip: "10.0.0.1" }),
  );
}

test("filtered results page past the first 100 matches instead of stopping there", async () => {
  const urls: string[] = [];
  // A full first page (== limit) means "there may be more"; the short second
  // page is the end of the results.
  server.use(pagedHandler((offset) => (offset === 0 ? page(0, 100) : page(100, 12)), urls));

  renderQueryLog();
  await firstSource();
  setFilterCell(/client/i, "10.0.0.1");

  expect(await screen.findByText("hit-0.example.com")).toBeInTheDocument();
  // A full first page must not be presented as the whole result set.
  expect(screen.queryByText(/all matching queries loaded/i)).not.toBeInTheDocument();
  expect(screen.getByRole("button", { name: /older/i })).toBeEnabled();

  const viewport = document.querySelector('[data-slot="scroll-area-viewport"]');
  if (!viewport) throw new Error("scroll viewport not found");
  fireEvent.scroll(viewport, { target: { scrollTop: 4_000 } });

  await waitFor(() => expect(urls.some((u) => u.includes("offset=100"))).toBe(true));
  expect(await screen.findByText(/all matching queries loaded/i)).toBeInTheDocument();
});

test("the footer's OLDER cell fetches the next page and the footer states the real offset", async () => {
  const urls: string[] = [];
  server.use(pagedHandler((offset) => (offset === 0 ? page(0, 100) : page(100, 12)), urls));

  renderQueryLog();
  await firstSource();
  setFilterCell(/client/i, "10.0.0.1");

  expect(await screen.findByText("hit-0.example.com")).toBeInTheDocument();
  expect(screen.getByText(/100 rows · limit 100 · offset 0/i)).toBeInTheDocument();
  // The endpoint orders by id, and id order is the logger's batched flush
  // order — close enough to look like time, wrong often enough to say so.
  expect(screen.getByText(/ordered by id, not by time/i)).toBeInTheDocument();

  fireEvent.click(screen.getByRole("button", { name: /older/i }));

  await waitFor(() => expect(urls.some((u) => u.includes("offset=100"))).toBe(true));
  await waitFor(() =>
    expect(screen.getByText(/112 rows · limit 100 · offset 100/i)).toBeInTheDocument(),
  );
  expect(screen.getByRole("button", { name: /older/i })).toBeDisabled();
});

test("the footer describes the live tail rather than a page while streaming", async () => {
  renderQueryLog();
  const source = await firstSource();
  act(() => source.emitOpen());
  act(() => source.emit(entry({ q_name: "one.example.com" })));

  await screen.findByText("one.example.com");
  expect(screen.getByText(/1 rows · live tail · 500 row buffer/i)).toBeInTheDocument();
  // Stream order is not id order — live rows have no id at all yet.
  expect(screen.queryByText(/ordered by id/i)).not.toBeInTheDocument();
});

// internal/store/search.go pages with `ORDER BY id DESC LIMIT ? OFFSET ?`,
// and the next offset is the running row count — so any query logged between
// the page-1 and page-2 fetches shifts every row down and page 2 re-returns
// the tail of page 1. Un-deduplicated, those reach react-table and the
// virtualizer as duplicate keys: React logs a duplicate-key warning and the
// same domain renders twice at the boundary.
test("rows re-returned by an offset shift render once, not twice", async () => {
  const urls: string[] = [];
  // Page 1: ids 0-99. Five new queries land before page 2 is asked for, so
  // offset=100 now points five rows earlier in the shifted result set and
  // re-returns ids 95-99 ahead of the genuinely new rows.
  server.use(pagedHandler((offset) => (offset === 0 ? page(0, 100) : page(95, 12)), urls));

  renderQueryLog();
  await firstSource();
  setFilterCell(/client/i, "10.0.0.1");

  expect(await screen.findByText("hit-0.example.com")).toBeInTheDocument();

  const viewport = document.querySelector('[data-slot="scroll-area-viewport"]');
  if (!viewport) throw new Error("scroll viewport not found");
  fireEvent.scroll(viewport, { target: { scrollTop: 4_000 } });
  await waitFor(() => expect(urls.some((u) => u.includes("offset=100"))).toBe(true));

  // Bring the page boundary into the virtualizer's window and count the
  // overlapping ids: each must be mounted exactly once.
  fireEvent.scroll(viewport, { target: { scrollTop: 2_650 } });
  await waitFor(() => expect(screen.getAllByText("hit-99.example.com")).toHaveLength(1));
  expect(screen.getAllByText("hit-95.example.com")).toHaveLength(1);
  // The rows that were genuinely new in page 2 are still there — deduping
  // must not drop them along with the overlap.
  fireEvent.scroll(viewport, { target: { scrollTop: 2_900 } });
  expect(await screen.findByText("hit-106.example.com")).toBeInTheDocument();
});

// query-core flips `status` to "error" on a failed *background* refetch even
// though `data` is intact. This page used to render nothing at all for that
// case: a table that had quietly stopped refreshing looked exactly like one
// that was up to date.
test("a failed next-page fetch keeps the rows already in hand and says they may be stale", async () => {
  const user = userEvent.setup();
  let failNextPage = true;
  server.use(
    http.get("/api/v1/queries", ({ request }) => {
      const url = new URL(request.url);
      if (url.searchParams.get("client") !== "10.0.0.1") return HttpResponse.json([]);
      // A full first page, so there is a second one to ask for — and asking
      // for it is what fails.
      if (Number(url.searchParams.get("offset") ?? 0) === 0) return HttpResponse.json(page(0, 100));
      if (failNextPage) return HttpResponse.json({ error: "boom" }, { status: 500 });
      return HttpResponse.json(page(100, 4));
    }),
  );

  renderQueryLog();
  await firstSource();
  setFilterCell(/client/i, "10.0.0.1");
  expect(await screen.findByText("hit-0.example.com")).toBeInTheDocument();

  fireEvent.click(screen.getByRole("button", { name: /older/i }));

  // `retry: 1` (lib/query-client.ts) means the failure only lands after the
  // retry's own backoff, so this waits past the default 1s.
  expect(
    await screen.findByText(/couldn't refresh the query log/i, undefined, { timeout: 5000 }),
  ).toBeInTheDocument();
  // The whole point of the banner is that the rows underneath it stay.
  expect(screen.getByText("hit-0.example.com")).toBeInTheDocument();

  failNextPage = false;
  await user.click(screen.getByRole("button", { name: /try again/i }));
  await waitFor(() =>
    expect(screen.queryByText(/couldn't refresh the query log/i)).not.toBeInTheDocument(),
  );
});

// --- the inspector -----------------------------------------------------------

test("selecting a row explains the decision and shows the raw record", async () => {
  const user = userEvent.setup();
  server.use(
    http.get("/api/v1/groups/:id/rules", () =>
      HttpResponse.json([
        { id: 5, group_id: 1, action: "block", pattern: "ads.example", is_regex: false },
      ]),
    ),
  );

  renderQueryLog();
  const source = await firstSource();
  act(() => source.emitOpen());
  act(() =>
    source.emit(
      entry({
        q_name: "ads.example",
        decision: "blocked",
        rule_id: 5,
        upstream: "",
        duration_ms: 0,
      }),
    ),
  );

  await screen.findByText("ads.example");
  await selectRow(user, "ads.example");

  const rail = inspector();
  await waitFor(() => expect(within(rail).getByText(/matched a block rule for/i)).toBeVisible());
  expect(within(rail).getByText("ads.example", { selector: "code" })).toBeInTheDocument();

  // The raw row is the contract, annotated where its zero values mean
  // something other than "missing".
  const listId = within(rail).getByText("list_id").nextElementSibling!;
  expect(listId).toHaveTextContent("0 not attributed");
  const upstream = within(rail).getByText("upstream").nextElementSibling!;
  expect(upstream).toHaveTextContent("— never left the box");
  const duration = within(rail).getByText("duration_ms").nextElementSibling!;
  expect(duration).toHaveTextContent("0 under 1 ms, truncated");
  // rule_id 5 matched, so it carries no "nothing matched" annotation.
  expect(within(rail).getByText("rule_id").nextElementSibling).toHaveTextContent(/^5$/);
});

test("the inspector resolves rules from the row's own group, not group 1", async () => {
  const user = userEvent.setup();
  const ruleRequests: string[] = [];
  server.use(
    http.get("/api/v1/clients", () =>
      HttpResponse.json([{ id: 7, name: "Kids tablet", matcher: "192.168.1.40", group_id: 3 }]),
    ),
    http.get("/api/v1/groups/:id/rules", ({ params }) => {
      ruleRequests.push(String(params.id));
      return HttpResponse.json(
        params.id === "3"
          ? [{ id: 5, group_id: 3, action: "block", pattern: "ads.kids.example", is_regex: false }]
          : [],
      );
    }),
  );

  renderQueryLog();
  const source = await firstSource();
  act(() => source.emitOpen());
  act(() =>
    source.emit(
      entry({ client_id: 7, q_name: "ads.kids.example", decision: "blocked", rule_id: 5 }),
    ),
  );

  await screen.findByText("ads.kids.example");
  await selectRow(user, "ads.kids.example");

  // Resolved out of group 3 — group 1's rules would have rendered the
  // "isn't available right now" fallback for every non-default group.
  await waitFor(() =>
    expect(within(inspector()).getByText(/matched a block rule for/i)).toBeInTheDocument(),
  );
  expect(ruleRequests).toContain("3");
});

// The rail is always mounted, so the risk the old drawer had — showing the
// previously clicked row — is now "does picking a second row actually
// replace the first".
test("picking another row replaces what the inspector shows", async () => {
  const user = userEvent.setup();
  renderQueryLog();
  const source = await firstSource();
  act(() => source.emitOpen());
  act(() => source.emit(entry({ q_name: "first.example.com" })));
  act(() => source.emit(entry({ q_name: "second.example.com", decision: "blocked" })));

  await screen.findByText("second.example.com");
  await selectRow(user, "first.example.com");
  await waitFor(() =>
    expect(within(inspector()).getByText("first.example.com")).toBeInTheDocument(),
  );

  await selectRow(user, "second.example.com");
  await waitFor(() =>
    expect(within(inspector()).getByText("second.example.com")).toBeInTheDocument(),
  );
  expect(within(inspector()).queryByText("first.example.com")).not.toBeInTheDocument();
});

test("with nothing selected the rail explains itself instead of rendering an empty shell", async () => {
  renderQueryLog();
  const source = await firstSource();
  act(() => source.emitOpen());

  expect(within(inspector()).getByText(/pick a row to see which rule/i)).toBeInTheDocument();
  expect(
    within(inspector()).queryByRole("button", { name: /copy row json/i }),
  ).not.toBeInTheDocument();
});

// --- quick rules -------------------------------------------------------------

test("the primary cell blocks a resolved row and writes into that row's client group", async () => {
  const user = userEvent.setup();
  const posted: { groupId: string; body: unknown }[] = [];
  server.use(
    http.get("/api/v1/clients", () =>
      HttpResponse.json([{ id: 7, name: "Kids tablet", matcher: "192.168.1.40", group_id: 3 }]),
    ),
    http.post("/api/v1/groups/:id/rules", async ({ params, request }) => {
      posted.push({ groupId: String(params.id), body: await request.json() });
      return HttpResponse.json({ id: 1 }, { status: 201 });
    }),
  );
  const successSpy = vi.spyOn(toast, "success");

  renderQueryLog();
  const source = await firstSource();
  act(() => source.emitOpen());
  act(() => source.emit(entry({ client_id: 7, q_name: "ads.kids.example" })));

  await screen.findByText("ads.kids.example");
  await selectRow(user, "ads.kids.example");

  const block = await within(inspector()).findByRole("button", { name: /block domain/i });
  await waitFor(() => expect(block).toBeEnabled());
  await user.click(block);

  await waitFor(() => expect(posted).toHaveLength(1));
  expect(posted[0]).toEqual({
    groupId: "3",
    body: { action: "block", pattern: "ads.kids.example" },
  });
  expect(successSpy).toHaveBeenCalledWith("Blocked ads.kids.example");
  expect(await within(inspector()).findByText("Blocked")).toBeInTheDocument();
});

// A blocked row needs the opposite rule, so the one primary cell follows the
// row rather than making the user pick between two buttons that are never
// both useful.
test("the primary cell offers an allow for a blocked row", async () => {
  const user = userEvent.setup();
  let body: unknown;
  server.use(
    http.post("/api/v1/groups/1/rules", async ({ request }) => {
      body = await request.json();
      return HttpResponse.json({ id: 2 }, { status: 201 });
    }),
  );
  const successSpy = vi.spyOn(toast, "success");

  renderQueryLog();
  const source = await firstSource();
  act(() => source.emitOpen());
  act(() => source.emit(entry({ q_name: "false-positive.example", decision: "blocked" })));

  await screen.findByText("false-positive.example");
  await selectRow(user, "false-positive.example");

  const allow = await within(inspector()).findByRole("button", { name: /allow domain/i });
  await waitFor(() => expect(allow).toBeEnabled());
  await user.click(allow);

  await waitFor(() => expect(body).toEqual({ action: "allow", pattern: "false-positive.example" }));
  expect(successSpy).toHaveBeenCalledWith("Allowed false-positive.example");
});

test("a failed rule write shows an error toast and leaves the action usable", async () => {
  const user = userEvent.setup();
  server.use(
    http.post("/api/v1/groups/:id/rules", () =>
      HttpResponse.json({ error: "boom" }, { status: 500 }),
    ),
  );
  const errorSpy = vi.spyOn(toast, "error");

  renderQueryLog();
  const source = await firstSource();
  act(() => source.emitOpen());
  act(() => source.emit(entry({ q_name: "fails.example.com" })));

  await screen.findByText("fails.example.com");
  await selectRow(user, "fails.example.com");

  const block = await within(inspector()).findByRole("button", { name: /block domain/i });
  await waitFor(() => expect(block).toBeEnabled());
  await user.click(block);

  await waitFor(() => expect(errorSpy).toHaveBeenCalledWith("Couldn't block fails.example.com"));
  expect(within(inspector()).getByRole("button", { name: /block domain/i })).toBeEnabled();
});

test("a row whose client can't be resolved refuses the action instead of faking success", async () => {
  const user = userEvent.setup();
  const posted: string[] = [];
  server.use(
    http.post("/api/v1/groups/:id/rules", ({ params }) => {
      posted.push(String(params.id));
      return HttpResponse.json({ id: 1 }, { status: 201 });
    }),
  );
  const errorSpy = vi.spyOn(toast, "error");
  const successSpy = vi.spyOn(toast, "success");

  renderQueryLog();
  const source = await firstSource();
  act(() => source.emitOpen());
  // client_id 999 belongs to no client this instance knows about (deleted
  // since the query was logged) — there is no group to write a rule into.
  act(() => source.emit(entry({ client_id: 999, q_name: "orphan.example" })));

  await screen.findByText("orphan.example");
  await selectRow(user, "orphan.example");

  const block = await within(inspector()).findByRole("button", { name: /block domain/i });
  await waitFor(() => expect(block).toBeEnabled());
  await user.click(block);

  await waitFor(() =>
    expect(errorSpy).toHaveBeenCalledWith(
      "Couldn't block orphan.example — this client's group is unknown",
    ),
  );
  expect(posted).toHaveLength(0);
  expect(successSpy).not.toHaveBeenCalled();
});

// groupForEntry answers null both for a client this instance no longer knows
// about *and* for a /clients request that hasn't landed. Leaving the action
// live in the second case fails the click with "this client's group is
// unknown" — a message about a deleted client, which is neither true nor
// actionable when the real cause is a failed sibling request.
test("the quick rule is held closed while the client list is unavailable", async () => {
  const posted: string[] = [];
  server.use(
    http.get("/api/v1/clients", () => HttpResponse.json({ error: "boom" }, { status: 500 })),
    http.post("/api/v1/groups/:id/rules", ({ params }) => {
      posted.push(String(params.id));
      return HttpResponse.json({ id: 1 }, { status: 201 });
    }),
  );
  const errorSpy = vi.spyOn(toast, "error");
  const user = userEvent.setup();

  renderQueryLog();
  const source = await firstSource();
  act(() => source.emitOpen());
  act(() => source.emit(entry({ client_id: 7, q_name: "pending.example" })));

  await screen.findByText("pending.example");
  await selectRow(user, "pending.example");

  const block = await within(inspector()).findByRole("button", { name: /block domain/i });
  expect(block).toBeDisabled();
  // Pending and errored both hold it closed; the reason has to distinguish
  // them, so wait for the request to actually give up before asserting.
  await waitFor(
    () =>
      expect(block).toHaveAttribute(
        "title",
        "Couldn't load clients — reload to write rules from here",
      ),
    { timeout: 5000 },
  );

  fireEvent.click(block);
  expect(posted).toHaveLength(0);
  expect(errorSpy).not.toHaveBeenCalledWith(
    expect.stringMatching(/this client's group is unknown/i),
  );
});

test("copy row json puts the whole record on the clipboard", async () => {
  const user = userEvent.setup();
  const writeText = vi.fn<(text: string) => Promise<void>>(() => Promise.resolve());
  vi.stubGlobal("navigator", { ...navigator, clipboard: { writeText } });
  const successSpy = vi.spyOn(toast, "success");

  renderQueryLog();
  const source = await firstSource();
  act(() => source.emitOpen());
  act(() => source.emit(entry({ q_name: "copy.example.com" })));

  await screen.findByText("copy.example.com");
  await selectRow(user, "copy.example.com");
  await user.click(within(inspector()).getByRole("button", { name: /copy row json/i }));

  await waitFor(() => expect(writeText).toHaveBeenCalledTimes(1));
  expect(JSON.parse(writeText.mock.calls[0]![0]) as QueryEntry).toMatchObject({
    q_name: "copy.example.com",
    decision: "cached",
  });
  expect(successSpy).toHaveBeenCalledWith("Row JSON copied");
});

// --- the memo boundary -------------------------------------------------------

// The tail commits a batch up to ten times a second. `memo` on the filter bar
// and the inspector only holds if every prop they get is referentially stable
// across those commits — one inline arrow in the page, or a `useCallback`
// closing over a react-query result object instead of its stable `mutate`,
// and the boundary silently stops working. Nothing on screen changes when it
// breaks, so the render counters (see pages/queries.tsx) are the only thing
// that can catch it.
test("live tail traffic re-renders neither the filter bar nor the inspector", async () => {
  const user = userEvent.setup();
  renderQueryLog();
  const source = await firstSource();
  act(() => source.emitOpen());
  act(() => source.emit(entry({ q_name: "watched.example.com" })));

  await screen.findByText("watched.example.com");
  // Select a row first, so the inspector is rendering its full contents
  // rather than its one-paragraph empty state.
  await selectRow(user, "watched.example.com");
  await waitFor(() =>
    expect(within(inspector()).getByText("watched.example.com")).toBeInTheDocument(),
  );
  // Let anything else still settling (clients, rules, lists) land first.
  await new Promise((resolve) => setTimeout(resolve, 150));

  const before = { ...renderCounts };
  for (let i = 0; i < 20; i += 1) {
    act(() => source.emit(entry({ q_name: `noise-${i}.example.com` })));
  }
  expect(await screen.findByText("noise-19.example.com")).toBeInTheDocument();

  expect(renderCounts.filterBar).toBe(before.filterBar);
  expect(renderCounts.inspector).toBe(before.inspector);
});

test("changing a filter does re-render the filter bar", async () => {
  renderQueryLog();
  await firstSource();
  await new Promise((resolve) => setTimeout(resolve, 50));

  const before = renderCounts.filterBar;
  setFilterCell(/client/i, "10.0.0.1");

  await waitFor(() => expect(renderCounts.filterBar).toBeGreaterThan(before));
});
