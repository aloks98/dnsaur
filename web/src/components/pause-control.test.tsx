import { http, HttpResponse } from "msw";
import type { QueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { expect, test, vi } from "vitest";
import { act, fireEvent, screen, waitFor, within } from "@testing-library/react";
import { blockingKeys } from "../hooks/use-blocking";
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

/**
 * Make the next poll fail and let the failure land: the query settles into
 * `error` with its previous data intact, and React is given time to commit a
 * render off it. Both matter — without the settle the DOM still shows the
 * pre-refetch render, and the assertion would pass whatever the component
 * does with the error.
 */
async function failPollAndSettle(queryClient: QueryClient) {
  await act(async () => {
    await queryClient.refetchQueries({ queryKey: blockingKeys.status(0) });
  });
  await waitFor(() =>
    expect(queryClient.getQueryState(blockingKeys.status(0))?.status).toBe("error"),
  );
  await act(async () => {
    await new Promise((resolve) => setTimeout(resolve, 100));
  });
}

// GET /blocking polls every 30s. One missed poll used to flip the cell to a
// destructive "Status unavailable" and un-grey Resume, although the pause
// this control had already read was still in effect — the exact thing the
// data layer's rule forbids (web/README.md: a blipped refetch never replaces
// valid data).
test("a failed poll keeps the last known state instead of claiming it's unavailable", async () => {
  let failing = false;
  const pausedUntil = Date.now() + 5 * 60_000;
  server.use(
    http.get("/api/v1/blocking", () =>
      failing
        ? HttpResponse.json({ error: "boom" }, { status: 500 })
        : HttpResponse.json({ paused_until: pausedUntil }),
    ),
  );

  const view = renderWithProviders(<PauseControl />);
  await screen.findByText(/^paused · \d+:[0-5]\d$/i);

  failing = true;
  await failPollAndSettle(view.queryClient);

  expect(screen.getByText(/^paused · \d+:[0-5]\d$/i)).toBeInTheDocument();
  expect(screen.queryByText("Status unavailable")).not.toBeInTheDocument();
});

test("a failed poll leaves Resume greyed out while the state it read says there's nothing to resume", async () => {
  let failing = false;
  server.use(
    http.get("/api/v1/blocking", () =>
      failing
        ? HttpResponse.json({ error: "boom" }, { status: 500 })
        : HttpResponse.json({ paused_until: 0 }),
    ),
  );

  const view = renderWithProviders(<PauseControl />);
  await screen.findByText("Blocking active");

  failing = true;
  await failPollAndSettle(view.queryClient);

  const menu = openMenu();
  const resume = within(menu)
    .getByText(/^resume blocking$/i)
    .closest('[role="menuitem"]');
  expect(resume).toHaveAttribute("aria-disabled", "true");
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

// The countdown is measured from the answer that carried the pause, not from
// a clock this component happened to read at mount. Nothing ticks while
// blocking is active, so by the time a pause arrives — from a poll, or from
// another tab — the component's own clock state can be arbitrarily old, and a
// countdown built on it alone would open minutes too long.
test("a pause that arrives long after mount opens at its real remaining time", async () => {
  vi.useFakeTimers({ shouldAdvanceTime: true });
  try {
    let pausedUntil = 0;
    server.use(
      http.get("/api/v1/blocking", () => HttpResponse.json({ paused_until: pausedUntil })),
    );

    const view = renderWithProviders(<PauseControl />);
    await screen.findByText("Blocking active");

    // Ten minutes of nothing happening, which is exactly what leaves a clock
    // read at mount ten minutes behind.
    await act(async () => {
      await vi.advanceTimersByTimeAsync(10 * 60_000);
    });

    pausedUntil = Date.now() + 5 * 60_000;
    await act(async () => {
      await view.queryClient.refetchQueries({ queryKey: blockingKeys.status(0) });
    });

    const countdown = await screen.findByText(/^paused · \d+:[0-5]\d$/i);
    const minutes = Number(/(\d+):/.exec(countdown.textContent ?? "")?.[1]);
    expect(minutes).toBeLessThanOrEqual(5);
    expect(minutes).toBeGreaterThanOrEqual(4);
  } finally {
    vi.useRealTimers();
  }
});
