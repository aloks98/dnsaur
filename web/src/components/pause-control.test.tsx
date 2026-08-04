import { http, HttpResponse } from "msw";
import { toast } from "sonner";
import { expect, test, vi } from "vitest";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { server } from "../test/msw-server";
import { renderWithProviders } from "../test/render";
import { PauseControl } from "./pause-control";

// rnui's DropdownMenu is base-ui Menu-driven — under jsdom, opening it via
// userEvent's full pointerdown→click sequence trips base-ui's outside-click
// detection and immediately re-closes it (no real geometry to hit-test
// against). A plain fireEvent.click reliably opens and keeps it open; see
// pages/queries.test.tsx's Filters dropdown for the same workaround. The
// button's own label never changes with state (see pause-control.tsx's
// comment on the "Filter" button convention), so one stable query opens it
// regardless of whether blocking is currently active or paused.
function openMenu() {
  fireEvent.click(screen.getByRole("button", { name: /pause blocking/i }));
  const menu = document.querySelector('[data-slot="dropdown-menu-content"]');
  if (!menu) throw new Error("pause menu did not open");
  return menu as HTMLElement;
}

test("shows 'Blocking active' when there's no pause in effect", async () => {
  renderWithProviders(<PauseControl />);
  expect(await screen.findByText("Blocking active")).toBeInTheDocument();
});

test("Pause 5 minutes posts {group_id:0, minutes:5} and shows a countdown", async () => {
  let requestBody: unknown;
  let pausedUntil = 0;
  server.use(
    http.get("/api/v1/blocking", () => HttpResponse.json({ paused_until: pausedUntil })),
    http.post("/api/v1/blocking/pause", async ({ request }) => {
      requestBody = await request.json();
      const body = requestBody as { group_id: number; minutes: number };
      pausedUntil = Date.now() + body.minutes * 60_000;
      return new HttpResponse(null, { status: 204 });
    }),
  );
  const successSpy = vi.spyOn(toast, "success");

  renderWithProviders(<PauseControl />);
  await screen.findByText("Blocking active");

  const menu = openMenu();
  fireEvent.click(within(menu).getByText(/pause 5 minutes/i));

  await waitFor(() => expect(requestBody).toEqual({ group_id: 0, minutes: 5 }));
  // A live "Paused · m:ss" countdown replaces the active readout.
  expect(await screen.findByText(/^paused · \d+:[0-5]\d$/i)).toBeInTheDocument();
  expect(successSpy).toHaveBeenCalledWith(expect.stringMatching(/paused for 5 minutes/i));
});

test("Resume deletes the pause and returns to active", async () => {
  let deleteUrl: string | undefined;
  let pausedUntil = Date.now() + 5 * 60_000;
  server.use(
    http.get("/api/v1/blocking", () => HttpResponse.json({ paused_until: pausedUntil })),
    http.delete("/api/v1/blocking/pause", ({ request }) => {
      deleteUrl = request.url;
      pausedUntil = 0;
      return new HttpResponse(null, { status: 204 });
    }),
  );
  const successSpy = vi.spyOn(toast, "success");

  renderWithProviders(<PauseControl />);
  await screen.findByText(/^paused · \d+:[0-5]\d$/i);

  const menu = openMenu();
  fireEvent.click(within(menu).getByText(/^resume$/i));

  await waitFor(() => expect(deleteUrl).toContain("group_id=0"));
  expect(await screen.findByText("Blocking active")).toBeInTheDocument();
  expect(successSpy).toHaveBeenCalledWith("Blocking resumed");
});

test("Resume does nothing while blocking is already active", async () => {
  let deleteCalled = false;
  server.use(
    http.delete("/api/v1/blocking/pause", () => {
      deleteCalled = true;
      return new HttpResponse(null, { status: 204 });
    }),
  );

  renderWithProviders(<PauseControl />);
  await screen.findByText("Blocking active");

  const menu = openMenu();
  fireEvent.click(within(menu).getByText(/^resume$/i));

  await new Promise((resolve) => setTimeout(resolve, 30));
  expect(deleteCalled).toBe(false);
});

test("a failed pause shows an error toast and doesn't get stuck", async () => {
  server.use(
    http.post("/api/v1/blocking/pause", () =>
      HttpResponse.json({ error: "boom" }, { status: 500 }),
    ),
  );
  const errorSpy = vi.spyOn(toast, "error");

  renderWithProviders(<PauseControl />);
  await screen.findByText("Blocking active");

  const menu = openMenu();
  fireEvent.click(within(menu).getByText(/pause 30 minutes/i));

  await waitFor(() => expect(errorSpy).toHaveBeenCalledWith("Couldn't pause blocking — try again"));
  expect(screen.getByText("Blocking active")).toBeInTheDocument();
});
