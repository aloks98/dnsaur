import { http, HttpResponse } from "msw";
import { toast } from "sonner";
import { expect, test, vi } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { server } from "../test/msw-server";
import { renderWithProviders } from "../test/render";
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
    AreaChart: ({ series }: { series: { name: string; data: number[] }[] }) => (
      <div data-testid="timeline-chart">
        {series.map((s) => (
          <div key={s.name}>
            {s.name}: {s.data.join(",")}
          </div>
        ))}
      </div>
    ),
  };
});

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
