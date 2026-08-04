import { act } from "react";
import { http, HttpResponse } from "msw";
import { toast } from "sonner";
import { beforeAll, beforeEach, expect, test, vi } from "vitest";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { server } from "../test/msw-server";
import { renderWithProviders } from "../test/render";
import { FakeEventSource } from "../test/fake-event-source";
import type { QueryEntry } from "../api/types";
import { QueryLog } from "./queries";

// The live-tail grid is virtualized (@tanstack/react-virtual, via rnui's
// DataGridTableVirtual) — it sizes its visible row window from the scroll
// container's offsetWidth/offsetHeight (see @tanstack/virtual-core's
// `getRect`), not from getBoundingClientRect. jsdom has no real layout
// engine and always reports 0 for both, which makes the virtualizer
// compute an empty visible range and render no rows at all. Stub a
// generous fixed viewport (and getBoundingClientRect too, since some
// popup positioning elsewhere in this page also reads it) so every test
// in this file gets a real, non-empty set of rendered rows to assert
// against — scoped to this file only, not the global test setup.
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
});

function entry(overrides: Partial<QueryEntry> = {}): QueryEntry {
  return {
    id: 1,
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

test("live rows render as the mocked stream emits", async () => {
  renderWithProviders(<QueryLog />);

  const source = await firstSource();
  act(() => source.emitOpen());
  act(() => source.emit(entry({ id: 1, q_name: "one.example.com" })));

  expect(await screen.findByText("one.example.com")).toBeInTheDocument();

  act(() => source.emit(entry({ id: 2, q_name: "two.example.com" })));
  expect(await screen.findByText("two.example.com")).toBeInTheDocument();
  expect(screen.getByText("one.example.com")).toBeInTheDocument();
});

test("pausing stops new rows appending but keeps existing rows", async () => {
  const user = userEvent.setup();
  renderWithProviders(<QueryLog />);

  const source = await firstSource();
  act(() => source.emitOpen());
  act(() => source.emit(entry({ id: 1, q_name: "kept.example.com" })));
  expect(await screen.findByText("kept.example.com")).toBeInTheDocument();

  await user.click(screen.getByRole("switch", { name: /live tail/i }));
  expect(await screen.findByText(/paused/i)).toBeInTheDocument();

  // The old stream is torn down on pause; emitting on it must not append.
  act(() => source.emit(entry({ id: 2, q_name: "dropped.example.com" })));

  await new Promise((resolve) => setTimeout(resolve, 50));
  expect(screen.queryByText("dropped.example.com")).not.toBeInTheDocument();
  expect(screen.getByText("kept.example.com")).toBeInTheDocument();
});

test("applying a decision filter calls GET /queries?decision=blocked and renders the results", async () => {
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

  renderWithProviders(<QueryLog />);
  await firstSource();

  // rnui's Filters dropdown is base-ui Menu-driven; under jsdom, opening it
  // and selecting an option via userEvent's full pointerdown→click sequence
  // trips base-ui's outside-click detection and immediately re-closes it
  // (no real geometry to hit-test against). A plain fireEvent.click — a
  // single synthetic click, matching how the menu's own trigger/option
  // handlers are wired — opens and keeps it open reliably; typing the
  // resulting filter value still goes through real userEvent.
  fireEvent.click(screen.getByRole("button", { name: /filter/i }));
  const menu = document.querySelector('[data-slot="dropdown-menu-content"]');
  if (!menu) throw new Error("filter menu did not open");
  fireEvent.click(within(menu as HTMLElement).getByText("Decision"));

  const decisionInput = await screen.findByPlaceholderText(/blocked, allowed, cached/i);
  await user.type(decisionInput, "blocked");

  await waitFor(() => expect(urls.at(-1)).toContain("decision=blocked"));
  expect(await screen.findByText("blocked-domain.example")).toBeInTheDocument();
});

test("block action posts a rule for group 1 and shows a success toast", async () => {
  const user = userEvent.setup();
  let requestBody: unknown;
  server.use(
    http.post("/api/v1/groups/1/rules", async ({ request }) => {
      requestBody = await request.json();
      return HttpResponse.json({ id: 1 }, { status: 201 });
    }),
  );
  const successSpy = vi.spyOn(toast, "success");

  renderWithProviders(<QueryLog />);
  const source = await firstSource();
  act(() => source.emitOpen());
  act(() => source.emit(entry({ id: 1, q_name: "site-one.example.com" })));

  const row = (await screen.findByText("site-one.example.com")).closest("tr");
  if (!row) throw new Error("row not found");
  await user.click(within(row).getByRole("button", { name: /block/i }));

  await waitFor(() =>
    expect(requestBody).toEqual({ action: "block", pattern: "site-one.example.com" }),
  );
  expect(await within(row).findByText("Blocked")).toBeInTheDocument();
  expect(successSpy).toHaveBeenCalledWith("Blocked site-one.example.com");
});

test("allow action posts an allow rule and shows a success toast", async () => {
  const user = userEvent.setup();
  let requestBody: unknown;
  server.use(
    http.post("/api/v1/groups/1/rules", async ({ request }) => {
      requestBody = await request.json();
      return HttpResponse.json({ id: 2 }, { status: 201 });
    }),
  );
  const successSpy = vi.spyOn(toast, "success");

  renderWithProviders(<QueryLog />);
  const source = await firstSource();
  act(() => source.emitOpen());
  act(() => source.emit(entry({ id: 1, q_name: "site-two.example.com" })));

  const row = (await screen.findByText("site-two.example.com")).closest("tr");
  if (!row) throw new Error("row not found");
  await user.click(within(row).getByRole("button", { name: /allow/i }));

  await waitFor(() =>
    expect(requestBody).toEqual({ action: "allow", pattern: "site-two.example.com" }),
  );
  expect(await within(row).findByText("Allowed")).toBeInTheDocument();
  expect(successSpy).toHaveBeenCalledWith("Allowed site-two.example.com");
});

test("a failed block action shows an error toast and doesn't get stuck", async () => {
  const user = userEvent.setup();
  server.use(
    http.post("/api/v1/groups/1/rules", () =>
      HttpResponse.json({ error: "boom" }, { status: 500 }),
    ),
  );
  const errorSpy = vi.spyOn(toast, "error");

  renderWithProviders(<QueryLog />);
  const source = await firstSource();
  act(() => source.emitOpen());
  act(() => source.emit(entry({ id: 1, q_name: "fails.example.com" })));

  const row = (await screen.findByText("fails.example.com")).closest("tr");
  if (!row) throw new Error("row not found");
  await user.click(within(row).getByRole("button", { name: /block/i }));

  await waitFor(() => expect(errorSpy).toHaveBeenCalledWith("Couldn't block fails.example.com"));
  expect(within(row).getByRole("button", { name: /block/i })).toBeInTheDocument();
});

test("the why drawer resolves a matched rule", async () => {
  const user = userEvent.setup();
  server.use(
    http.get("/api/v1/groups/:id/rules", () =>
      HttpResponse.json([
        { id: 5, group_id: 1, action: "block", pattern: "ads.example", is_regex: false },
      ]),
    ),
  );

  renderWithProviders(<QueryLog />);
  const source = await firstSource();
  act(() => source.emitOpen());
  act(() => source.emit(entry({ id: 1, q_name: "ads.example", decision: "blocked", rule_id: 5 })));

  const row = (await screen.findByText("ads.example")).closest("tr");
  if (!row) throw new Error("row not found");
  await user.click(within(row).getByRole("button", { name: /why/i }));

  expect(await screen.findByText("Why this decision?")).toBeInTheDocument();
  await waitFor(() => expect(screen.getByText(/matched a block rule for/i)).toBeInTheDocument());
  expect(screen.getByText("ads.example", { selector: "code" })).toBeInTheDocument();
});

test("the grid is virtualized: rows far past the scroll window aren't mounted until scrolled into view", async () => {
  const many = Array.from({ length: 50 }, (_, i) =>
    entry({ id: i + 1, q_name: `host-${i}.example.com`, decision: "blocked" }),
  );
  server.use(
    http.get("/api/v1/queries", ({ request }) => {
      const url = new URL(request.url);
      return HttpResponse.json(url.searchParams.get("decision") === "blocked" ? many : []);
    }),
  );

  renderWithProviders(<QueryLog />);
  await firstSource();

  fireEvent.click(screen.getByRole("button", { name: /filter/i }));
  const menu = document.querySelector('[data-slot="dropdown-menu-content"]');
  if (!menu) throw new Error("filter menu did not open");
  fireEvent.click(within(menu as HTMLElement).getByText("Decision"));
  const decisionInput = await screen.findByPlaceholderText(/blocked, allowed, cached/i);
  fireEvent.change(decisionInput, { target: { value: "blocked" } });

  expect(await screen.findByText("host-0.example.com")).toBeInTheDocument();
  // With an ~800px stubbed viewport and ~34px estimated rows (plus
  // overscan), only ~35 of the 50 rows should be mounted at scroll-top —
  // the very last row must not be in the DOM yet if virtualization is
  // actually active (a plain, non-virtualized table would mount all 50).
  expect(screen.queryByText("host-49.example.com")).not.toBeInTheDocument();

  const viewport = document.querySelector('[data-slot="scroll-area-viewport"]');
  if (!viewport) throw new Error("scroll viewport not found");
  fireEvent.scroll(viewport, { target: { scrollTop: 5000 } });

  await waitFor(() => expect(screen.getByText("host-49.example.com")).toBeInTheDocument());
});

test("clearing filters returns to live tail: the stream reopens and live rows resume", async () => {
  const user = userEvent.setup();
  server.use(
    http.get("/api/v1/queries", () =>
      HttpResponse.json([
        entry({ id: 99, q_name: "paged-result.example.com", decision: "blocked" }),
      ]),
    ),
  );

  renderWithProviders(<QueryLog />);
  await firstSource();
  expect(FakeEventSource.instances).toHaveLength(1);

  // Apply a filter — switches to paged mode, tearing down the live stream.
  fireEvent.click(screen.getByRole("button", { name: /filter/i }));
  const menu = document.querySelector('[data-slot="dropdown-menu-content"]');
  if (!menu) throw new Error("filter menu did not open");
  fireEvent.click(within(menu as HTMLElement).getByText("Decision"));
  const decisionInput = await screen.findByPlaceholderText(/blocked, allowed, cached/i);
  fireEvent.change(decisionInput, { target: { value: "blocked" } });

  expect(await screen.findByText("paged-result.example.com")).toBeInTheDocument();

  // Clear filters — should return to live tail and open a *new* SSE
  // connection (the old one was torn down when paged mode took over).
  await user.click(screen.getByRole("button", { name: /clear filters/i }));

  await waitFor(() => expect(FakeEventSource.instances).toHaveLength(2));
  expect(screen.queryByText("paged-result.example.com")).not.toBeInTheDocument();

  const newSource = FakeEventSource.instances[1]!;
  act(() => newSource.emitOpen());
  act(() => newSource.emit(entry({ id: 100, q_name: "resumed-live.example.com" })));

  expect(await screen.findByText("resumed-live.example.com")).toBeInTheDocument();
});
