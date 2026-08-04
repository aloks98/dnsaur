import { act } from "react";
import { http, HttpResponse } from "msw";
import { toast } from "sonner";
import { beforeEach, expect, test, vi } from "vitest";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { server } from "../test/msw-server";
import { renderWithProviders } from "../test/render";
import { FakeEventSource } from "../test/fake-event-source";
import type { QueryEntry } from "../api/types";
import { QueryLog } from "./queries";

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
