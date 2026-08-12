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
  ).toEqual(["Monitor", "Filtering", "Zones", "System"]);

  // Row 2 is the active group's pages, and "/" makes that Monitor.
  const monitorTabs = screen.getByRole("navigation", { name: "Monitor" });
  expect(within(monitorTabs).getByRole("link", { name: "Dashboard" })).toHaveAttribute(
    "aria-current",
    "page",
  );
  expect(within(monitorTabs).getByRole("link", { name: /query log/i })).toBeInTheDocument();
});

// The dashboard is a full-bleed grid of hairline rules that have to meet
// the viewport edges; the shell's 24px page gutter would leave every one of
// them floating inside a frame, and a non-flex <main> would stop the split
// claiming the leftover height its vertical rule needs.
// Every screen is a full-bleed grid of hairline-separated bands, so <main>
// carries no gutter and does not scroll — each page owns its own scrolling
// pane. This used to branch per route while the screens were rebuilt one at
// a time; Account was the last padded one, and once it went the branch
// always took the same side.
test("the shell gives every route the full viewport, and never scrolls itself", async () => {
  for (const route of [
    "/",
    "/queries",
    "/filtering/lists",
    "/zones",
    "/settings",
    "/tsig-keys",
    "/account",
  ]) {
    const view = renderWithProviders(<App />, { route });
    await screen.findByRole("navigation", { name: "Primary" });

    const main = screen.getByRole("main");
    expect(main.className).not.toContain("p-6");
    expect(main.className).toContain("overflow-hidden");
    // flex-col + min-h-0 is what lets a page claim the leftover height and
    // still shrink; without it a long table pushes <main> past the viewport
    // and the page's own scroller never engages.
    expect(main.className).toContain("flex-col");
    expect(main.className).toContain("min-h-0");
    view.unmount();
  }
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
  expect(await screen.findByRole("button", { name: /refresh all/i })).toBeInTheDocument();

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

// The route loop above asserts only <main>'s classes, which the not-found
// screen satisfies just as well — so it would stay green if /tsig-keys were
// never wired to a page at all. Both halves of the wiring are pinned here:
// the chrome marking System's second tab, and something only the page
// itself renders.
test("/tsig-keys renders the TSIG keys screen under System, not the not-found page", async () => {
  renderWithProviders(<App />, { route: "/tsig-keys" });

  const systemTabs = await screen.findByRole("navigation", { name: "System" });
  expect(within(systemTabs).getByRole("link", { name: "TSIG keys" })).toHaveAttribute(
    "aria-current",
    "page",
  );
  // The page's own header strip: a count of what came back from
  // /api/v1/tsig-keys (one key, per the default handler) and its CTA.
  expect(await screen.findByText("1 key")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: /new key/i })).toBeInTheDocument();
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
  await screen.findByRole("link", { name: "Query Log" });

  // The session is revoked (logged out elsewhere, token revoked, DB reset);
  // the next panel fetch is the first thing to notice. The window selector
  // lives in the chrome now and writes `?window=` — the dashboard refetches
  // off that, so this still drives a real authenticated request.
  signedIn = false;
  await user.click(screen.getByRole("button", { name: "Last hour" }));

  // Two sequential round trips before the form can appear: the 401 from the
  // stats refetch, then the re-run of /auth/me the interceptor triggers —
  // with the gate's spinner in between. One second isn't reliably enough.
  await waitFor(() => expect(screen.getByLabelText(/^password$/i)).toBeInTheDocument(), {
    timeout: 3000,
  });
  expect(screen.queryByRole("link", { name: "Query Log" })).not.toBeInTheDocument();
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
  await user.click(screen.getByRole("link", { name: "Query Log" }));

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
  expect(screen.queryByRole("link", { name: "Query Log" })).not.toBeInTheDocument();
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
