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
import { getLiveTailStatus, setLiveTailPaused } from "../lib/live-tail";
import { QueryLog, renderCounts } from "./queries";

// The table is virtualized (@tanstack/react-virtual, via rnui's
// DataGridTableVirtual) — it sizes its visible row window from the scroll
// container's offsetWidth/offsetHeight (see @tanstack/virtual-core's
// `getRect`), not from getBoundingClientRect. jsdom has no layout engine and
// always reports 0 for both, which makes the virtualizer compute an empty
// visible range and render no rows at all. Stub a generous fixed viewport
// (and getBoundingClientRect too, since the filter row's date popover also
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
 * The page alone — deliberately not mounted alongside components/top-nav.tsx.
 *
 * The tail's pause flag is shared module state (lib/live-tail.ts), and the
 * page now carries its own mode readout and PAUSE/RESUME TAIL toggle, so
 * nothing here needs the shell rendered to be exercised. The cross-component
 * half of that contract — "a pause set anywhere is observed here" — is
 * asserted directly against the store instead, which is both stronger and
 * doesn't tie this file to the chrome's markup.
 */
function renderQueryLog() {
  return renderWithProviders(<QueryLog />, { route: "/queries" });
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

/** Pick a value in one of the three filter selects. */
function selectFilter(label: RegExp, value: string) {
  fireEvent.change(screen.getByRole("combobox", { name: label }), { target: { value } });
}

/** The default filter used by tests that only need *some* filter active:
 * the type select, because it needs nothing else to have loaded first. */
function filterByType(type = "AAAA") {
  selectFilter(/type/i, type);
}

/** The row the given domain is rendered in. */
function rowFor(domain: string): HTMLElement {
  return screen.getByText(domain).closest("tr")!;
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

test("pausing holds the stream without dropping what's on screen, and resuming doesn't duplicate it", async () => {
  renderQueryLog();

  const source = await firstSource();
  act(() => source.emitOpen());
  act(() => source.emit(entry({ q_name: "kept.example.com" })));
  expect(await screen.findByText("kept.example.com")).toBeInTheDocument();

  // The design's one filled control on this screen, labelled with what it
  // will do rather than with the state it is in.
  // The toggle itself is chrome (row 2); pausing is the store flip it performs.
  act(() => setLiveTailPaused(true));
  await waitFor(() => expect(getLiveTailStatus().paused).toBe(true));

  // The old stream is torn down on pause; emitting on it must not append.
  act(() => source.emit(entry({ q_name: "dropped.example.com" })));
  await new Promise((resolve) => setTimeout(resolve, 50));
  expect(screen.queryByText("dropped.example.com")).not.toBeInTheDocument();
  expect(screen.getByText("kept.example.com")).toBeInTheDocument();

  // Resuming reopens a *new* stream and prepends only what arrives next —
  // the buffered row is neither lost nor replayed.
  act(() => setLiveTailPaused(false));
  await waitFor(() => expect(FakeEventSource.instances).toHaveLength(2));
  const resumed = FakeEventSource.instances[1]!;
  act(() => resumed.emitOpen());
  act(() => resumed.emit(entry({ q_name: "after.example.com" })));

  expect(await screen.findByText("after.example.com")).toBeInTheDocument();
  expect(screen.getAllByText("kept.example.com")).toHaveLength(1);
});

// The flag lives in a module-level store precisely so the chrome's own cell
// and this page observe one value (lib/live-tail.ts). Setting it from
// outside React is what the chrome's toggle ultimately does, so this covers
// the wiring without depending on the shell's markup.
test("a pause set from outside the page (the chrome's cell) stops the tail here too", async () => {
  renderQueryLog();

  const source = await firstSource();
  act(() => source.emitOpen());
  act(() => source.emit(entry({ q_name: "kept.example.com" })));
  expect(await screen.findByText("kept.example.com")).toBeInTheDocument();

  act(() => setLiveTailPaused(true));

  await waitFor(() => expect(getLiveTailStatus().paused).toBe(true));
  act(() => source.emit(entry({ q_name: "dropped.example.com" })));
  await new Promise((resolve) => setTimeout(resolve, 50));
  expect(screen.queryByText("dropped.example.com")).not.toBeInTheDocument();
});

test("a dropped stream is reported as reconnecting", async () => {
  renderQueryLog();

  const source = await firstSource();
  act(() => source.emitOpen());
  await waitFor(() => expect(getLiveTailStatus().streamState).toBe("open"));

  act(() => source.emitError());
  await waitFor(() => expect(getLiveTailStatus().streamState).toBe("reconnecting"));
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

    // The label and the button live in the chrome now; what this page owes
    // it is the terminal state and a working reconnect callback.
    await vi.waitFor(() => expect(getLiveTailStatus().streamState).toBe("failed"));
    const opened = FakeEventSource.instances.length;

    act(() => getLiveTailStatus().reconnect());
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

// --- the mode readout --------------------------------------------------------

// The two modes are not interchangeable: the live tail is the SSE stream and
// carries rows with no id at all, the filtered view is `GET /queries` paged
// by id. Which one is running changes what the footer means, whether OLDER
// does anything, and how far behind the table can be — so the page says
// which it is rather than leaving it to be inferred from whether rows move.
// The readout itself lives in the chrome (components/top-nav.tsx) — row 2
// owns that cell in the design. What this page is responsible for is
// *publishing* which mode is running, since only it knows whether a filter
// is active. That's what's asserted here.
test("the page publishes which mode is running, and switches when a filter takes over", async () => {
  renderQueryLog();
  await firstSource();

  await waitFor(() => expect(getLiveTailStatus().filtered).toBe(false));

  filterByType();

  await waitFor(() => expect(getLiveTailStatus().filtered).toBe(true));
});

// --- the table ---------------------------------------------------------------

test("the table carries the eight columns the log is read by", async () => {
  renderQueryLog();
  const source = await firstSource();
  act(() => source.emitOpen());
  act(() => source.emit(entry()));

  await screen.findByText("example.com");
  expect(screen.getAllByRole("columnheader").map((h) => h.textContent)).toEqual([
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

// CLIENT IP and HOSTNAME are separate columns because they answer different
// questions and come from different places: the IP is on the row, the
// hostname is `client_id` looked up in GET /clients. A row whose client was
// deleted since (or which matched no client at all, `client_id` 0) has no
// hostname to show — and inventing one, or reusing the IP, would claim the
// log knows something it doesn't.
test("hostname resolves through client_id, and says nothing rather than guessing when it can't", async () => {
  server.use(
    http.get("/api/v1/clients", () =>
      HttpResponse.json([{ id: 7, name: "kids-ipad", matcher: "192.168.1.42", group_id: 1 }]),
    ),
  );

  renderQueryLog();
  const source = await firstSource();
  act(() => source.emitOpen());
  act(() =>
    source.emit(entry({ q_name: "known.example.com", client_id: 7, client_ip: "192.168.1.42" })),
  );
  // client_id 99 belongs to no client this instance knows about; client_id 0
  // means the query matched no client entry at all.
  act(() =>
    source.emit(entry({ q_name: "gone.example.com", client_id: 99, client_ip: "192.168.11.63" })),
  );
  act(() =>
    source.emit(entry({ q_name: "anon.example.com", client_id: 0, client_ip: "192.168.11.64" })),
  );

  await screen.findByText("anon.example.com");
  await waitFor(() =>
    expect(within(rowFor("known.example.com")).getByText("kids-ipad")).toBeInTheDocument(),
  );
  // The row still shows its IP — only the *name* is unknown. (upstream is
  // non-empty on these fixtures, so the em dash can only be the hostname.)
  expect(within(rowFor("gone.example.com")).getByText("192.168.11.63")).toBeInTheDocument();
  expect(within(rowFor("gone.example.com")).getByText("—")).toBeInTheDocument();
  expect(within(rowFor("anon.example.com")).getByText("—")).toBeInTheDocument();
});

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
    entry({ id: i + 1, q_name: `host-${i}.example.com`, q_type: "AAAA" }),
  );
  server.use(
    http.get("/api/v1/queries", ({ request }) => {
      const url = new URL(request.url);
      return HttpResponse.json(url.searchParams.get("type") === "AAAA" ? many : []);
    }),
  );

  renderQueryLog();
  await firstSource();
  filterByType();

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
test("the search box says what the server actually does with % and _", async () => {
  renderQueryLog();
  await firstSource();

  const box = screen.getByRole("textbox", { name: /search domains/i });
  expect(box).toHaveAttribute("placeholder", "search q_name…");
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

  const select = screen.getByRole("combobox", { name: /decision/i });
  expect(
    within(select)
      .getAllByRole("option")
      .map((o) => o.textContent),
  ).toEqual(["All decisions", "blocked", "forwarded", "cached", "stale", "local", "error"]);
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

  await user.selectOptions(screen.getByRole("combobox", { name: /decision/i }), "blocked");

  await waitFor(() => expect(urls.at(-1) ?? "").toContain("decision=blocked"));
  expect(await screen.findByText("blocked-domain.example")).toBeInTheDocument();
});

// `type` is an exact, uppercase match server-side (docs/ui-contract.md §2.7),
// so a free-text box here was a licence to type "a" and be told, truthfully
// but uselessly, that there are no matches.
test("the type filter offers the record types the resolver answers in bulk", async () => {
  const urls: string[] = [];
  server.use(
    http.get("/api/v1/queries", ({ request }) => {
      urls.push(request.url);
      const url = new URL(request.url);
      return HttpResponse.json(
        url.searchParams.get("type") === "HTTPS"
          ? [entry({ id: 3, q_name: "svc.example", q_type: "HTTPS" })]
          : [],
      );
    }),
  );

  renderQueryLog();
  await firstSource();

  const select = screen.getByRole("combobox", { name: /type/i });
  expect(
    within(select)
      .getAllByRole("option")
      .map((o) => o.textContent),
  ).toEqual(["All types", "A", "AAAA", "HTTPS", "PTR", "TXT"]);

  selectFilter(/type/i, "HTTPS");
  await waitFor(() => expect(urls.at(-1) ?? "").toContain("type=HTTPS"));
  expect(await screen.findByText("svc.example")).toBeInTheDocument();
});

// `client` is an *exact* match on client_ip, so the options can only be IPs
// — and a client whose matcher is a CIDR (which the API accepts) can never
// equal a single logged address. Offering it would be the `allowed` mistake
// again: a filter that looks like it narrows the search and returns nothing,
// forever. Those clients still name their rows, because HOSTNAME resolves
// through client_id rather than through the matcher.
test("the client filter lists exact-IP clients as '<ip> <name>' and filters on the IP", async () => {
  const urls: string[] = [];
  server.use(
    http.get("/api/v1/clients", () =>
      HttpResponse.json([
        { id: 7, name: "kids-ipad", matcher: "192.168.1.42", group_id: 1 },
        { id: 8, name: "iot-vlan", matcher: "192.168.30.0/24", group_id: 1 },
      ]),
    ),
    http.get("/api/v1/queries", ({ request }) => {
      urls.push(request.url);
      const url = new URL(request.url);
      return HttpResponse.json(
        url.searchParams.get("client") === "192.168.1.42"
          ? [entry({ id: 4, q_name: "tablet.example", client_id: 7 })]
          : [],
      );
    }),
  );

  renderQueryLog();
  await firstSource();

  const select = screen.getByRole("combobox", { name: /client/i });
  await waitFor(() =>
    expect(
      within(select)
        .getAllByRole("option")
        .map((o) => o.textContent),
    ).toEqual(["Any client IP", "192.168.1.42 kids-ipad"]),
  );

  selectFilter(/client/i, "192.168.1.42");
  await waitFor(() => expect(urls.at(-1) ?? "").toContain("client=192.168.1.42"));
  expect(await screen.findByText("tablet.example")).toBeInTheDocument();
});

// --- the time range ----------------------------------------------------------

/** Local-midnight bounds of a day, the way the page computes them. */
function dayRange(year: number, monthIndex: number, day: number): [number, number] {
  return [
    new Date(year, monthIndex, day).getTime(),
    new Date(year, monthIndex, day + 1).getTime() - 1,
  ];
}

/** Open the range popover and hand back its date box — typeable, and ISO,
 * because `03/04` names two different days depending on where you are. */
async function openTimeRange(user: ReturnType<typeof userEvent.setup>) {
  await user.click(screen.getByRole("button", { name: /time range/i }));
  const input = await screen.findByPlaceholderText(/select date/i);
  expect(input).not.toHaveAttribute("readonly");
  return input;
}

// `from`/`to` are inclusive unix-millisecond bounds applied only when > 0
// (internal/store/search.go). A picked *day* is a whole day, not an instant:
// sending the day's midnight as both bounds would return only queries logged
// in the first millisecond of it.
test("picking a day filters on that whole day and switches the page to paged mode", async () => {
  const user = userEvent.setup();
  const urls: string[] = [];
  server.use(
    http.get("/api/v1/queries", ({ request }) => {
      urls.push(request.url);
      return HttpResponse.json([entry({ id: 5, q_name: "dated.example" })]);
    }),
  );

  renderQueryLog();
  await firstSource();

  const input = await openTimeRange(user);
  fireEvent.change(input, { target: { value: "2026-03-15" } });

  const [start, end] = dayRange(2026, 2, 15);
  await waitFor(() => expect(urls.at(-1) ?? "").toContain(`from=${start}`));
  expect(urls.at(-1)).toContain(`to=${end}`);
  await waitFor(() => expect(getLiveTailStatus().filtered).toBe(true));
});

// "after the 15th" is an open-ended bound. Sending a `to` as well would turn
// it into "on the 15th" — the operator would be decoration.
test("the 'after' operator sends only a lower bound", async () => {
  const user = userEvent.setup();
  const urls: string[] = [];
  server.use(
    http.get("/api/v1/queries", ({ request }) => {
      urls.push(request.url);
      return HttpResponse.json([]);
    }),
  );

  renderQueryLog();
  await firstSource();

  const input = await openTimeRange(user);
  await user.click(screen.getByRole("tab", { name: "after" }));
  fireEvent.change(input, { target: { value: "2026-03-15" } });

  const [start] = dayRange(2026, 2, 15);
  await waitFor(() => expect(urls.at(-1) ?? "").toContain(`from=${start}`));
  expect(urls.at(-1)).not.toContain("to=");
});

// `qlog.retention_days` defaults to 90 (docs/ui-contract.md §5), so a period
// longer than a month selects a window the pruner has already emptied, and a
// year picker offering 2015 offers a decade of guaranteed-empty results.
test("the period and year choices stay inside what retention can actually hold", async () => {
  const user = userEvent.setup();
  renderQueryLog();
  await firstSource();

  await openTimeRange(user);
  const periods = screen
    .getAllByRole("tab")
    .map((t) => t.textContent)
    .filter((t) => t !== null);
  expect(periods).toContain("Day");
  expect(periods).toContain("Month");
  expect(periods).not.toContain("Quarter");
  expect(periods).not.toContain("Half-year");
  expect(periods).not.toContain("Year");
});

test("clearing filters returns to the live tail and forgets the picked range", async () => {
  const user = userEvent.setup();
  server.use(
    http.get("/api/v1/queries", () =>
      HttpResponse.json([entry({ id: 99, q_name: "paged-result.example.com" })]),
    ),
  );

  renderQueryLog();
  await firstSource();
  expect(FakeEventSource.instances).toHaveLength(1);

  const input = await openTimeRange(user);
  fireEvent.change(input, { target: { value: "2026-03-15" } });
  expect(await screen.findByText("paged-result.example.com")).toBeInTheDocument();
  await waitFor(() => expect(getLiveTailStatus().filtered).toBe(true));
  // The trigger says what it's filtering on, operator included.
  expect(screen.getByRole("button", { name: /is 2026-03-15/i })).toBeInTheDocument();

  await user.click(screen.getByRole("button", { name: /clear filters/i }));

  // A cleared range must not be left showing on the trigger — the picker
  // keeps its own selection, so it is remounted rather than trusted.
  await waitFor(() =>
    expect(screen.queryByRole("button", { name: /is 2026-03-15/i })).not.toBeInTheDocument(),
  );
  expect(screen.getByRole("button", { name: /time range/i })).toBeInTheDocument();

  await waitFor(() => expect(FakeEventSource.instances).toHaveLength(2));
  expect(screen.queryByText("paged-result.example.com")).not.toBeInTheDocument();
  await waitFor(() => expect(getLiveTailStatus().filtered).toBe(false));

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
    if (url.searchParams.get("type") !== "AAAA") return HttpResponse.json([]);
    return HttpResponse.json(pageFor(Number(url.searchParams.get("offset") ?? 0)));
  });
}

function page(start: number, count: number): QueryEntry[] {
  return Array.from({ length: count }, (_, i) =>
    entry({ id: start + i, q_name: `hit-${start + i}.example.com`, q_type: "AAAA" }),
  );
}

test("filtered results page past the first 100 matches instead of stopping there", async () => {
  const urls: string[] = [];
  // A full first page (== limit) means "there may be more"; the short second
  // page is the end of the results.
  server.use(pagedHandler((offset) => (offset === 0 ? page(0, 100) : page(100, 12)), urls));

  renderQueryLog();
  await firstSource();
  filterByType();

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

test("the footer's OLDER button fetches the next page and the footer states the real offset", async () => {
  const urls: string[] = [];
  server.use(pagedHandler((offset) => (offset === 0 ? page(0, 100) : page(100, 12)), urls));

  renderQueryLog();
  await firstSource();
  filterByType();

  expect(await screen.findByText("hit-0.example.com")).toBeInTheDocument();
  expect(screen.getByText(/100 rows · limit 100 · offset 0/i)).toBeInTheDocument();
  // The endpoint orders by id, and id order is the logger's batched flush
  // order — close enough to look like `at`, wrong often enough to say so.
  expect(screen.getByText(/ordered by id, not by at/i)).toBeInTheDocument();

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
  filterByType();

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
      if (url.searchParams.get("type") !== "AAAA") return HttpResponse.json([]);
      // A full first page, so there is a second one to ask for — and asking
      // for it is what fails.
      if (Number(url.searchParams.get("offset") ?? 0) === 0) return HttpResponse.json(page(0, 100));
      if (failNextPage) return HttpResponse.json({ error: "boom" }, { status: 500 });
      return HttpResponse.json(page(100, 4));
    }),
  );

  renderQueryLog();
  await firstSource();
  filterByType();
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
  // The decision is a chip beside the one-line summary, not a word in a tint
  // — the rail shows one row at a time, so it can afford the weight.
  const chip = rail.querySelector('[data-slot="badge"]');
  expect(chip).toHaveTextContent("blocked");
  expect(within(rail).getByText(/A · NOERROR · <1 ms/)).toBeInTheDocument();

  // The alert names *which* rule, in which group — "rule #5" alone says
  // nothing, because rules are per-group (see groupForEntry).
  const alert = within(rail).getByRole("alert");
  expect(alert).toHaveTextContent("Matched rule #5 in group default");
  await waitFor(() => expect(within(alert).getByText(/matched a block rule for/i)).toBeVisible());
  expect(within(alert).getByText("ads.example", { selector: "code" })).toBeInTheDocument();

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

// A rule can be deleted between the query being logged and the row being
// inspected, and the rail must say so rather than rendering a blank
// explanation under a title that promises one.
test("a rule that no longer exists is reported as such, not silently omitted", async () => {
  const user = userEvent.setup();
  server.use(http.get("/api/v1/groups/:id/rules", () => HttpResponse.json([])));

  renderQueryLog();
  const source = await firstSource();
  act(() => source.emitOpen());
  act(() => source.emit(entry({ q_name: "orphaned.example", decision: "blocked", rule_id: 42 })));

  await screen.findByText("orphaned.example");
  await selectRow(user, "orphaned.example");

  const alert = within(inspector()).getByRole("alert");
  await waitFor(() =>
    expect(alert).toHaveTextContent(
      /Matched rule #42, which isn't available right now \(it may have been deleted\)\./i,
    ),
  );
});

test("the inspector resolves rules from the row's own group, not group 1", async () => {
  const user = userEvent.setup();
  const ruleRequests: string[] = [];
  server.use(
    http.get("/api/v1/groups", () =>
      HttpResponse.json([
        { id: 1, name: "default", enabled: true },
        { id: 3, name: "kids", enabled: true },
      ]),
    ),
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
  const alert = within(inspector()).getByRole("alert");
  await waitFor(() =>
    expect(within(alert).getByText(/matched a block rule for/i)).toBeInTheDocument(),
  );
  expect(alert).toHaveTextContent("Matched rule #5 in group kids");
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

test("the primary action blocks a resolved row and writes into that row's client group", async () => {
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

// A blocked row needs the opposite rule, so the one primary action follows
// the row rather than making the user pick between two buttons that are
// never both useful.
test("the primary action offers an allow for a blocked row", async () => {
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
// across those commits — one inline arrow in the page, a `useCallback`
// closing over a react-query result object instead of its stable `mutate`, or
// a freshly-built list of client options per render, and the boundary
// silently stops working. Nothing on screen changes when it breaks, so the
// render counters (see pages/queries.tsx) are the only thing that can catch
// it.
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
  // Let anything else still settling (clients, groups, rules, lists) land
  // first — including the date picker's own on-mount value emission.
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
  await new Promise((resolve) => setTimeout(resolve, 150));

  const before = renderCounts.filterBar;
  filterByType();

  await waitFor(() => expect(renderCounts.filterBar).toBeGreaterThan(before));
});

// The rail is a panel, not a detail view bolted to the selection: closing it
// should give the table the full width even while a row stays highlighted,
// and picking a row should bring it back without hunting for a re-open
// control. Those are two pieces of state, and conflating them was the bug —
// "Close" only cleared the selection and left a 384px empty panel behind.
test("the inspector rail closes, and picking a row brings it back", async () => {
  renderQueryLog();
  const source = await firstSource();
  act(() => source.emitOpen());
  act(() => source.emit(entry({ q_name: "closable.example.com" })));

  expect(await screen.findByRole("heading", { name: /why this decision/i })).toBeInTheDocument();

  await userEvent.setup().click(screen.getByRole("button", { name: /close the inspector/i }));
  await waitFor(() =>
    expect(screen.queryByRole("heading", { name: /why this decision/i })).not.toBeInTheDocument(),
  );

  await userEvent.setup().click(await screen.findByText("closable.example.com"));
  expect(await screen.findByRole("heading", { name: /why this decision/i })).toBeInTheDocument();
});
