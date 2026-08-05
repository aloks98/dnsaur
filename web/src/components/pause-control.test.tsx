import { http, HttpResponse } from "msw";
import { toast } from "sonner";
import { expect, test, vi } from "vitest";
import { act, fireEvent, screen, waitFor, within } from "@testing-library/react";
import { server } from "../test/msw-server";
import { renderWithProviders } from "../test/render";
import { PauseControl } from "./pause-control";

// rnui's DropdownMenu is base-ui Menu-driven — under jsdom, opening it via
// userEvent's full pointerdown→click sequence trips base-ui's outside-click
// detection and immediately re-closes it (no real geometry to hit-test
// against). A plain fireEvent.click reliably opens and keeps it open; see
// pages/queries.test.tsx's Filters dropdown for the same workaround.
//
// The trigger's *visible* text is now the blocking state and changes with
// it, so this queries the stable action half of its accessible name — the
// sr-only suffix that keeps the button saying what it does.
function openMenu() {
  fireEvent.click(screen.getByRole("button", { name: /open blocking controls/i }));
  const menu = document.querySelector('[data-slot="dropdown-menu-content"]');
  if (!menu) throw new Error("pause menu did not open");
  return menu as HTMLElement;
}

/** The one button that carries both state and action. */
function trigger() {
  return screen.getByRole("button", { name: /open blocking controls/i });
}

test("the trigger reads 'Blocking active' when there's no pause in effect", async () => {
  renderWithProviders(<PauseControl />);
  await waitFor(() => expect(trigger()).toHaveAccessibleName(/blocking active/i));
  expect(within(trigger()).getByText("Blocking active")).toBeInTheDocument();
});

test("Pause 5 minutes posts {group_id:0, minutes:5} and the trigger becomes a live countdown", async () => {
  vi.useFakeTimers({ shouldAdvanceTime: true });
  try {
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
    // The status readout is the button: "Blocking active" is replaced in
    // place by a "Paused · m:ss" countdown, no separate indicator.
    const countdown = await screen.findByText(/^paused · \d+:[0-5]\d$/i);
    expect(trigger()).toContainElement(countdown);
    expect(screen.queryByText("Blocking active")).not.toBeInTheDocument();
    expect(successSpy).toHaveBeenCalledWith(expect.stringMatching(/paused for 5 minutes/i));

    // ...and it ticks down once a second without another network read.
    const before = countdown.textContent;
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1_100);
    });
    expect(screen.getByText(/^paused · \d+:[0-5]\d$/i).textContent).not.toBe(before);
  } finally {
    vi.useRealTimers();
  }
});

test("Resume deletes the pause and the trigger returns to active", async () => {
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
  fireEvent.click(within(menu).getByText(/^resume blocking$/i));

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
  fireEvent.click(within(menu).getByText(/^resume blocking$/i));

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

test("a failed GET /blocking says so instead of claiming blocking is active", async () => {
  let deleteCalled = false;
  server.use(
    http.get("/api/v1/blocking", () => HttpResponse.json({ error: "boom" }, { status: 500 })),
    http.delete("/api/v1/blocking/pause", () => {
      deleteCalled = true;
      return new HttpResponse(null, { status: 204 });
    }),
  );

  renderWithProviders(<PauseControl />);

  // retry: 1 with backoff (see lib/query-client.ts) — the button reads
  // "Checking…" until the retry is spent, and only then admits it can't tell.
  expect(
    await screen.findByText("Status unavailable", undefined, { timeout: 4000 }),
  ).toBeInTheDocument();
  expect(screen.queryByText("Blocking active")).not.toBeInTheDocument();

  // Still operable: we don't know the state, so resuming stays attemptable
  // rather than greyed out on a guess.
  const menu = openMenu();
  fireEvent.click(within(menu).getByText(/^resume blocking$/i));
  await waitFor(() => expect(deleteCalled).toBe(true));
});
