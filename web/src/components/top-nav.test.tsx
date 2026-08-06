import { http, HttpResponse } from "msw";
import { toast } from "sonner";
import { afterEach, expect, test, vi } from "vitest";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { useLocation, useNavigationType } from "react-router";
import { server } from "../test/msw-server";
import { renderWithProviders } from "../test/render";
import { TopNav } from "./top-nav";

// rnui's DropdownMenu is base-ui Menu-driven — under jsdom, opening it via
// userEvent's full pointerdown→click sequence trips base-ui's outside-click
// detection and immediately re-closes it (no real geometry to hit-test
// against). A plain fireEvent.click reliably opens and keeps it open; see
// pause-control.test.tsx for the same workaround.
function openGroup(name: RegExp | string): HTMLElement {
  fireEvent.click(screen.getByRole("button", { name }));
  const menu = document.querySelector('[data-slot="dropdown-menu-content"]');
  if (!menu) throw new Error(`the ${String(name)} menu did not open`);
  return menu as HTMLElement;
}

/** Row 2 is a <nav> labelled with the active group — the tabs live in it. */
function tabs(groupLabel: string): HTMLElement {
  return screen.getByRole("navigation", { name: groupLabel });
}

function renderTopNav({ route = "/", onOpen = () => {} } = {}) {
  function Harness() {
    const location = useLocation();
    return (
      <>
        <span data-testid="pathname">{location.pathname}</span>
        <span data-testid="search">{location.search}</span>
        {/* "PUSH" vs "REPLACE" is how the router itself reports whether the
            last navigation added a back-stack entry. */}
        <span data-testid="nav-type">{useNavigationType()}</span>
        <TopNav onOpenCommandPalette={onOpen} />
      </>
    );
  }
  return renderWithProviders(<Harness />, { route });
}

// useLogout finishes with a hard window.location.assign("/") (see
// hooks/use-auth.ts for why). jsdom exposes `assign` as a non-configurable
// own property, so it can't be spied — but `window.location` itself *is*
// configurable, so swap the whole object for the duration of the test.
let restoreLocation: (() => void) | null = null;

function stubNavigation() {
  const original = window.location;
  const assign = vi.fn<(url: string) => void>();
  Object.defineProperty(window, "location", {
    configurable: true,
    value: Object.assign(
      Object.create(null),
      { href: original.href, origin: original.origin },
      { assign },
    ),
  });
  restoreLocation = () =>
    Object.defineProperty(window, "location", { configurable: true, value: original });
  return assign;
}

afterEach(() => {
  restoreLocation?.();
  restoreLocation = null;
  vi.restoreAllMocks();
});

// lib/theme.ts is a module-level store, so whatever a previous test in this
// file toggled to is still in effect here — assert on the flip, not on a
// fixed starting theme.
function clickThemeToggle(menu: HTMLElement) {
  const wasDark = document.documentElement.classList.contains("dark");
  fireEvent.click(
    within(menu).getByRole("menuitem", {
      name: wasDark ? /switch to light theme/i : /switch to dark theme/i,
    }),
  );
  return wasDark;
}

test("row 1 carries the mark, the wordmark and the four nav groups", async () => {
  renderTopNav();

  // The owner's logo component, at the design's 22px tile.
  const mark = screen.getByRole("img", { name: /dnsaur logo/i });
  expect(mark).toHaveAttribute("width", "22");
  expect(screen.getByText("dnsaur")).toBeInTheDocument();

  // Real landmark semantics, carried over from the sidebar this replaced.
  const primary = screen.getByRole("navigation", { name: "Primary" });
  expect(
    within(primary)
      .getAllByRole("button")
      .map((button) => button.textContent),
  ).toEqual(["Monitor", "Filtering", "Network", "System"]);
});

test("the active group is marked and row 2 shows its pages as links", async () => {
  renderTopNav({ route: "/" });

  expect(screen.getByRole("button", { name: "Monitor" })).toHaveAttribute("data-active", "true");
  expect(screen.getByRole("button", { name: "Filtering" })).toHaveAttribute("data-active", "false");

  const monitorTabs = tabs("Monitor");
  expect(within(monitorTabs).getByRole("link", { name: "Dashboard" })).toHaveAttribute(
    "aria-current",
    "page",
  );
  expect(within(monitorTabs).getByRole("link", { name: "Query Log" })).not.toHaveAttribute(
    "aria-current",
  );

  // The marker is a 2px bottom border, and *every* cell carries one — the
  // inactive ones transparent — so lighting a tab never nudges its text.
  const dashboardTab = within(monitorTabs).getByRole("link", { name: "Dashboard" });
  const queryLogTab = within(monitorTabs).getByRole("link", { name: /query log/i });
  expect(dashboardTab.className).toContain("border-b-primary");
  expect(dashboardTab.className).toContain("border-b-2");
  expect(queryLogTab.className).toContain("border-b-transparent");
  expect(queryLogTab.className).toContain("border-b-2");
  // No other group's pages leak into the second row.
  expect(within(monitorTabs).queryByRole("link", { name: "Settings" })).not.toBeInTheDocument();
});

test("a deep Filtering route lights its group and its own tab", async () => {
  renderTopNav({ route: "/filtering/rules" });

  expect(screen.getByRole("button", { name: "Filtering" })).toHaveAttribute("data-active", "true");
  expect(screen.getByRole("button", { name: "Monitor" })).toHaveAttribute("data-active", "false");

  const filteringTabs = tabs("Filtering");
  expect(
    within(filteringTabs)
      .getAllByRole("link")
      .map((link) => link.textContent),
  ).toEqual(["Lists", "Rules", "Groups & Clients"]);
  expect(within(filteringTabs).getByRole("link", { name: "Rules" })).toHaveAttribute(
    "aria-current",
    "page",
  );
});

test("the redirecting /filtering index still lights the Filtering group", async () => {
  // The redirect to /filtering/lists lands a tick later; the chrome must not
  // blink back to Monitor in between.
  renderTopNav({ route: "/filtering" });
  expect(screen.getByRole("button", { name: "Filtering" })).toHaveAttribute("data-active", "true");
});

test("a group with a single page still gets its row-2 tab", async () => {
  renderTopNav({ route: "/dns" });

  expect(screen.getByRole("button", { name: "Network" })).toHaveAttribute("data-active", "true");
  expect(within(tabs("Network")).getByRole("link", { name: "Local DNS" })).toHaveAttribute(
    "aria-current",
    "page",
  );
});

test("a group menu lists its pages as menu items and navigates to them", async () => {
  renderTopNav({ route: "/" });

  const menu = openGroup("Network");
  const item = within(menu).getByRole("menuitem", { name: /local dns/i });
  fireEvent.click(item);

  await waitFor(() => expect(screen.getByTestId("pathname")).toHaveTextContent("/dns"));
});

test("the search cell opens the command palette", async () => {
  const onOpen = vi.fn<() => void>();
  renderTopNav({ onOpen });

  fireEvent.click(screen.getByRole("button", { name: /search/i }));

  expect(onOpen).toHaveBeenCalledTimes(1);
});

test("the System menu carries Settings, Account, the signed-in user, theme and log out", async () => {
  renderTopNav();
  // The identity comes from the useMe layer, not from this component.
  await screen.findByRole("button", { name: "System" });

  const menu = openGroup("System");
  await waitFor(() => expect(within(menu).getByText("admin")).toBeInTheDocument());
  expect(within(menu).getByText("AD")).toBeInTheDocument();

  expect(within(menu).getByRole("menuitem", { name: /settings/i })).toBeInTheDocument();
  expect(within(menu).getByRole("menuitem", { name: /^account/i })).toBeInTheDocument();
  expect(
    within(menu).getByRole("menuitem", { name: /switch to (dark|light) theme/i }),
  ).toBeInTheDocument();
  expect(within(menu).getByRole("menuitem", { name: /log out/i })).toBeInTheDocument();
});

test("the System menu's theme toggle switches themes without closing the menu", async () => {
  renderTopNav();

  const menu = openGroup("System");
  const wasDark = clickThemeToggle(menu);

  expect(document.documentElement.classList.contains("dark")).toBe(!wasDark);
  // Still open, so flipping back is one click and not a second trip.
  expect(
    within(menu).getByRole("menuitem", {
      name: wasDark ? /switch to dark theme/i : /switch to light theme/i,
    }),
  ).toBeInTheDocument();
});

test("logging out posts /auth/logout and leaves for the login screen", async () => {
  const assign = stubNavigation();
  let logoutCalled = false;
  server.use(
    http.post("/api/v1/auth/logout", () => {
      logoutCalled = true;
      return new HttpResponse(null, { status: 204 });
    }),
  );

  renderTopNav();
  fireEvent.click(within(openGroup("System")).getByRole("menuitem", { name: /log out/i }));

  await waitFor(() => expect(logoutCalled).toBe(true));
  await waitFor(() => expect(assign).toHaveBeenCalledWith("/"));
});

test("a 401 from logout is a completed logout, not an error toast", async () => {
  const assign = stubNavigation();
  const errorSpy = vi.spyOn(toast, "error");
  server.use(
    http.post("/api/v1/auth/logout", () =>
      HttpResponse.json({ error: "authentication required" }, { status: 401 }),
    ),
  );

  renderTopNav();
  fireEvent.click(within(openGroup("System")).getByRole("menuitem", { name: /log out/i }));

  await waitFor(() => expect(assign).toHaveBeenCalledWith("/"));
  expect(errorSpy).not.toHaveBeenCalled();
});

test("a real logout failure keeps the user put and says so", async () => {
  const assign = stubNavigation();
  const errorSpy = vi.spyOn(toast, "error");
  server.use(
    http.post("/api/v1/auth/logout", () => HttpResponse.json({ error: "boom" }, { status: 500 })),
  );

  renderTopNav();
  fireEvent.click(within(openGroup("System")).getByRole("menuitem", { name: /log out/i }));

  await waitFor(() => expect(errorSpy).toHaveBeenCalledWith("Couldn't sign out — try again"));
  expect(assign).not.toHaveBeenCalled();
});

test("the resolver readout names its subject and reports GET /health honestly", async () => {
  renderTopNav();
  await waitFor(() => expect(screen.getByRole("status")).toHaveTextContent(/dns ok/i));
  // A bare "Resolving" read as an action in progress, not a state.
  expect(screen.getByRole("status")).not.toHaveTextContent(/resolving/i);
});

test("an unreachable resolver says so rather than staying green", async () => {
  server.use(http.get("/api/v1/health", () => HttpResponse.json({ error: "x" }, { status: 500 })));

  renderTopNav();
  await waitFor(() => expect(screen.getByRole("status")).toHaveTextContent(/dns down/i));
  expect(screen.getByRole("status")).not.toHaveTextContent(/dns ok/i);
});

// Stage 2 parked resolver health in row 2's one filled cell for want of a
// home. Row 2 is the *contextual* row and that cell is the design's only
// call to action, so health moved up beside blocking — the shell's two "is
// this thing working" readouts, in one place and in one visual language.
test("the two health readouts sit together in row 1, neither of them filled", async () => {
  renderTopNav();

  const blocking = screen.getByRole("button", { name: /open blocking controls/i });
  const resolver = screen.getByRole("status");
  await waitFor(() => expect(blocking).toHaveTextContent(/blocking active/i));
  await waitFor(() => expect(resolver).toHaveTextContent(/dns ok/i));

  // Same row (row 1 is the header's first child), and adjacent within it.
  const row1 = screen.getByRole("navigation", { name: "Primary" }).parentElement;
  expect(row1).toContainElement(blocking);
  expect(row1).toContainElement(resolver);
  expect(blocking.compareDocumentPosition(resolver) & Node.DOCUMENT_POSITION_FOLLOWING).toBe(
    Node.DOCUMENT_POSITION_FOLLOWING,
  );

  // Neither shouts over the other: the one solid fill belongs to the CTA.
  expect(resolver.className).not.toContain("bg-primary");
  expect(resolver.className).toContain("text-primary");
  // The strings stay sentence case (accessible names, assertions); the cell
  // is what uppercases them.
  expect(blocking.className).toContain("uppercase");
});

// --- row 2's contextual cells ------------------------------------------------

test("the dashboard's chrome carries the window selector and the filled CTA", async () => {
  renderTopNav({ route: "/" });

  const windows = screen.getByRole("group", { name: "Time window" });
  expect(
    within(windows)
      .getAllByRole("button")
      .map((b) => b.textContent),
  ).toEqual(["1h", "24h", "7d"]);
  // 24h is the default, and it is the one marked as chosen.
  expect(within(windows).getByRole("button", { name: "Last 24 hours" })).toHaveAttribute(
    "aria-pressed",
    "true",
  );
  expect(within(windows).getByRole("button", { name: "Last hour" })).toHaveAttribute(
    "aria-pressed",
    "false",
  );

  const cta = screen.getByRole("link", { name: /view query log/i });
  expect(cta).toHaveAttribute("href", "/queries");
  expect(cta.className).toContain("bg-primary");
});

test("picking a window writes it to the URL, replacing rather than stacking history", async () => {
  renderTopNav({ route: "/" });

  fireEvent.click(screen.getByRole("button", { name: "Last 7 days" }));

  await waitFor(() => expect(screen.getByTestId("search")).toHaveTextContent("?window=7d"));
  expect(screen.getByRole("button", { name: "Last 7 days" })).toHaveAttribute(
    "aria-pressed",
    "true",
  );
  // No back-stack entry: the window is a view setting, not a place you can
  // go "back" from.
  expect(screen.getByTestId("nav-type")).toHaveTextContent("REPLACE");
});

test("a window already in the URL is the one shown as chosen", async () => {
  renderTopNav({ route: "/?window=1h" });

  expect(screen.getByRole("button", { name: "Last hour" })).toHaveAttribute("aria-pressed", "true");
  expect(screen.getByRole("button", { name: "Last 24 hours" })).toHaveAttribute(
    "aria-pressed",
    "false",
  );
});

// Both cells are about the dashboard: a stats window governs nothing on
// Settings, and "view query log" is noise on the query log itself.
test("no other screen gets the window selector or the CTA", async () => {
  renderTopNav({ route: "/settings" });

  expect(screen.queryByRole("group", { name: "Time window" })).not.toBeInTheDocument();
  expect(screen.queryByRole("link", { name: /view query log/i })).not.toBeInTheDocument();
  expect(screen.queryByRole("switch", { name: /live tail/i })).not.toBeInTheDocument();
});

// The query log spends the same filled cell on its own CTA — the one control
// that screen is actually about. The flag it toggles lives in
// lib/live-tail.ts, because the stream it governs is consumed by the routed
// page below rather than by the chrome.
test("the query log's chrome carries the trailing note and the live/pause cell", async () => {
  renderTopNav({ route: "/queries" });

  expect(screen.getByText(/table trails the tail by ~1s/i)).toBeInTheDocument();
  // Not on the dashboard, which has its own pair.
  expect(screen.queryByRole("link", { name: /view query log/i })).not.toBeInTheDocument();

  const toggle = screen.getByRole("switch", { name: /live tail/i });
  expect(toggle.className).toContain("bg-primary");
  // The cell shows both states so it says what it will do; the accessible
  // name says what the pair is *of*.
  // Flex `gap-2` does the spacing, so the text nodes sit flush.
  expect(toggle.textContent).toBe("Live·Pause");
  expect(toggle).toHaveAttribute("aria-checked", "true");

  fireEvent.click(toggle);
  await waitFor(() => expect(toggle).toHaveAttribute("aria-checked", "false"));

  fireEvent.click(toggle);
  await waitFor(() => expect(toggle).toHaveAttribute("aria-checked", "true"));
});
