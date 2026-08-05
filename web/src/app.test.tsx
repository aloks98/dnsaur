import { delay, http, HttpResponse } from "msw";
import { afterEach, expect, test, vi } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { Route, Routes } from "react-router";
import { focusManager } from "@tanstack/react-query";
import { server } from "./test/msw-server";
import { renderWithProviders } from "./test/render";
import { AppShell } from "./components/app-shell";
import { App } from "./app";

afterEach(() => vi.restoreAllMocks());

function unauthorized() {
  return HttpResponse.json({ error: "authentication required" }, { status: 401 });
}

function Boom(): never {
  throw new Error("this page is broken");
}

test("authenticated user gets the two-row shell: nav groups above, the active group's pages below", async () => {
  renderWithProviders(<App />);

  const primary = await screen.findByRole("navigation", { name: "Primary" });
  expect(
    within(primary)
      .getAllByRole("button")
      .map((button) => button.textContent),
  ).toEqual(["Monitor", "Filtering", "Network", "System"]);

  // Row 2 is the active group's pages, and "/" makes that Monitor.
  const monitorTabs = screen.getByRole("navigation", { name: "Monitor" });
  expect(within(monitorTabs).getByRole("link", { name: "Dashboard" })).toHaveAttribute(
    "aria-current",
    "page",
  );
  expect(within(monitorTabs).getByRole("link", { name: /query log/i })).toBeInTheDocument();
});

test("/filtering forwards to Lists, and each panel is a deep-linkable route", async () => {
  const user = userEvent.setup();
  renderWithProviders(<App />, { route: "/filtering" });

  // The index redirects rather than rendering a fourth, empty thing.
  const filteringTabs = await screen.findByRole("navigation", { name: "Filtering" });
  await waitFor(() =>
    expect(within(filteringTabs).getByRole("link", { name: "Lists" })).toHaveAttribute(
      "aria-current",
      "page",
    ),
  );
  // "Refresh now" belongs to the Lists panel and to no other.
  expect(await screen.findByRole("button", { name: /refresh now/i })).toBeInTheDocument();

  // ...and the tabs are navigation, so the URL follows the panel.
  await user.click(within(filteringTabs).getByRole("link", { name: "Groups & Clients" }));
  await waitFor(() =>
    expect(within(filteringTabs).getByRole("link", { name: "Groups & Clients" })).toHaveAttribute(
      "aria-current",
      "page",
    ),
  );
  expect(within(filteringTabs).getByRole("link", { name: "Lists" })).not.toHaveAttribute(
    "aria-current",
  );
});

test("a deep-linked panel opens directly, instead of resetting to the first tab", async () => {
  renderWithProviders(<App />, { route: "/filtering/rules" });

  const filteringTabs = await screen.findByRole("navigation", { name: "Filtering" });
  expect(within(filteringTabs).getByRole("link", { name: "Rules" })).toHaveAttribute(
    "aria-current",
    "page",
  );
});

test("unauthenticated + setup-required shows setup", async () => {
  server.use(
    http.get("/api/v1/auth/me", () => HttpResponse.json({ error: "x" }, { status: 401 })),
    http.get("/api/v1/setup", () => HttpResponse.json({ setup_required: true })),
  );
  renderWithProviders(<App />);
  await waitFor(() => expect(screen.getByText(/set up/i)).toBeInTheDocument());
});

test("unauthenticated + setup-done shows login", async () => {
  server.use(
    http.get("/api/v1/auth/me", () => HttpResponse.json({ error: "x" }, { status: 401 })),
    http.get("/api/v1/setup", () => HttpResponse.json({ setup_required: false })),
  );
  renderWithProviders(<App />);
  await waitFor(() => expect(screen.getByLabelText(/password/i)).toBeInTheDocument());
});

test("a session that dies mid-session returns to login instead of stranding a shell of 401s", async () => {
  const user = userEvent.setup();
  let signedIn = true;
  server.use(
    http.get("/api/v1/auth/me", () =>
      signedIn
        ? HttpResponse.json({ id: 1, username: "admin", totp_enabled: false })
        : unauthorized(),
    ),
    http.get("/api/v1/setup", () => HttpResponse.json({ setup_required: false })),
    http.get("/api/v1/stats/overview", () =>
      signedIn
        ? HttpResponse.json({ total: 10, blocked: 1, cached: 2, forwarded: 3, clients: 1 })
        : unauthorized(),
    ),
  );

  renderWithProviders(<App />);
  await screen.findByRole("link", { name: /query log/i });

  // The session is revoked (logged out elsewhere, token revoked, DB reset);
  // the next panel fetch is the first thing to notice.
  signedIn = false;
  await user.click(screen.getByRole("combobox", { name: /time window/i }));
  await user.click(await screen.findByRole("option", { name: /last hour/i }));

  await waitFor(() => expect(screen.getByLabelText(/^password$/i)).toBeInTheDocument());
  expect(screen.queryByRole("link", { name: /query log/i })).not.toBeInTheDocument();
});

test("a page that throws is contained by the shell's boundary, and navigating away clears it", async () => {
  const user = userEvent.setup();
  vi.spyOn(console, "error").mockImplementation(() => {});

  renderWithProviders(
    <Routes>
      <Route element={<AppShell />}>
        <Route index element={<Boom />} />
        <Route path="queries" element={<h1>Recovered page</h1>} />
      </Route>
    </Routes>,
  );

  expect(await screen.findByText(/something went wrong/i)).toBeInTheDocument();
  // The chrome outside the boundary is still mounted and still usable...
  await user.click(screen.getByRole("link", { name: /query log/i }));

  // ...and the boundary doesn't pin its fallback over the next route.
  expect(await screen.findByText("Recovered page")).toBeInTheDocument();
  expect(screen.queryByText(/something went wrong/i)).not.toBeInTheDocument();
});

test("a crash on an unauthenticated screen is caught, not left as a blank page", async () => {
  vi.spyOn(console, "error").mockImplementation(() => {});
  server.use(
    http.get("/api/v1/auth/me", () => unauthorized()),
    // A malformed payload the gate reads straight through: without an
    // app-level boundary React 19 answers this by unmounting the whole root.
    http.get("/api/v1/setup", () => HttpResponse.json(null)),
  );

  renderWithProviders(<App />);

  expect(await screen.findByText(/something went wrong/i)).toBeInTheDocument();
});

test("api unreachable (both auth/me and setup fail) shows the unreachable banner, not the shell or login", async () => {
  server.use(
    http.get("/api/v1/auth/me", () => HttpResponse.json({ error: "unreachable" }, { status: 500 })),
    http.get("/api/v1/setup", () => HttpResponse.json({ error: "unreachable" }, { status: 500 })),
  );
  renderWithProviders(<App />);
  await waitFor(() => expect(screen.getByText(/can't reach/i)).toBeInTheDocument());
  expect(screen.queryByRole("link", { name: /query log/i })).not.toBeInTheDocument();
  expect(screen.queryByLabelText(/password/i)).not.toBeInTheDocument();
});

test("refocusing the tab on the login screen keeps the form (no gate flicker)", async () => {
  // jsdom never fires visibilitychange, so drive query-core's focusManager
  // directly — the same entry point the browser listener calls.
  //
  // The second /auth/me is deliberately slow: query-core flips an errored,
  // data-less query back to `pending` while it refetches, but if the retry
  // rejects instantly React batches pending+error into one commit and the
  // spinner never renders, which would make this test pass either way.
  let meCalls = 0;
  server.use(
    http.get("/api/v1/auth/me", async () => {
      meCalls += 1;
      if (meCalls > 1) await delay(40);
      return unauthorized();
    }),
    http.get("/api/v1/setup", () => HttpResponse.json({ setup_required: false })),
  );
  renderWithProviders(<App />);

  const password = await screen.findByLabelText(/password/i);
  await userEvent.type(password, "hunter2");

  focusManager.setFocused(false);
  focusManager.setFocused(true);

  // `me` has never succeeded, so refetching it on focus would flip the gate
  // to its full-page spinner, unmount Login, and discard everything typed.
  await new Promise((resolve) => setTimeout(resolve, 120));
  expect(meCalls).toBe(1);
  expect(screen.getByLabelText(/password/i)).toHaveValue("hunter2");
});
