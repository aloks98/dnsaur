import { act } from "react";
import { http, HttpResponse } from "msw";
import { toast } from "sonner";
import { afterEach, expect, test, vi } from "vitest";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { useLocation, useNavigationType } from "react-router";
import { server } from "../test/msw-server";
import { blockingHandler } from "../test/msw-handlers";
import { renderWithProviders } from "../test/render";
import {
  resetLiveTailReport,
  setLiveTailPaused,
  setLiveTailReport,
  useLiveTailPaused,
} from "../lib/live-tail";
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

/**
 * Stands in for the routed page below the chrome: the tail flag is shared
 * module state (lib/live-tail.ts), and the whole point of it is that the
 * shell's toggle moves what the *page* reads. Observing it through the same
 * hook the page uses is how this file asserts that, rather than reaching
 * into the module.
 */
function TailProbe() {
  const { paused } = useLiveTailPaused();
  return <span data-testid="tail-paused">{String(paused)}</span>;
}

function readLiveTailPaused(): boolean {
  return screen.getByTestId("tail-paused").textContent === "true";
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
        <TailProbe />
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
  // lib/live-tail.ts is a module-level store, so it outlives any one test.
  setLiveTailPaused(false);
  resetLiveTailReport();
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
  ).toEqual(["Monitor", "Filtering", "Zones", "System"]);
});

// Four disclosure chevrons in a row said nothing the marked group and the
// row of pages under it don't already say, and cost a column of noise at
// the top of every screen. The bar keeps exactly one, on the blocking cell,
// where the menu is the only route to the actions.
test("the group cells carry no disclosure chevron", async () => {
  renderTopNav();

  const primary = screen.getByRole("navigation", { name: "Primary" });
  const groups = within(primary).getAllByRole("button");
  // Nothing in a group cell but its name — no icon, and no "▾" typed as
  // text either.
  expect(groups.map((g) => g.textContent)).toEqual(["Monitor", "Filtering", "Zones", "System"]);
  for (const group of groups) {
    expect(group.querySelector("svg")).toBeNull();
    // Still a real menu button — the affordance moved to semantics, not away.
    expect(group).toHaveAttribute("aria-haspopup", "menu");
  }
  // The blocking cell keeps its glyph and its chevron: its menu is the only
  // way to reach pause/resume.
  const blocking = screen.getByRole("button", { name: /open blocking controls/i });
  expect(blocking.querySelectorAll("svg")).toHaveLength(2);
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

// An unknown path renders pages/not-found.tsx rather than redirecting to the
// dashboard. That makes "no group is active" a state the chrome now has to
// hold, instead of a single frame on the way to `/`.
test("an unknown route lights no group and drops the second row entirely", async () => {
  renderTopNav({ route: "/nope/not-a-page" });

  for (const group of ["Monitor", "Filtering", "Zones", "System"]) {
    expect(screen.getByRole("button", { name: group })).toHaveAttribute("data-active", "false");
  }

  // Row 2 is gone, not merely empty: every group's <nav> is absent, while
  // row 1's own navigation is untouched and still reachable.
  for (const group of ["Monitor", "Filtering", "Zones", "System"]) {
    expect(screen.queryByRole("navigation", { name: group })).not.toBeInTheDocument();
  }
  expect(screen.getByRole("navigation", { name: "Primary" })).toBeInTheDocument();
});

test("the redirecting /filtering index still lights the Filtering group", async () => {
  // The redirect to /filtering/lists lands a tick later; the chrome must not
  // blink back to Monitor in between.
  renderTopNav({ route: "/filtering" });
  expect(screen.getByRole("button", { name: "Filtering" })).toHaveAttribute("data-active", "true");
});

// The group is named for the one thing behind it. "Network" promised DHCP,
// interfaces and encrypted DNS, none of which exist.
test("a group with a single page still gets its row-2 tab", async () => {
  renderTopNav({ route: "/zones" });

  expect(screen.getByRole("button", { name: "Zones" })).toHaveAttribute("data-active", "true");
  expect(screen.queryByRole("button", { name: "Network" })).not.toBeInTheDocument();
  expect(within(tabs("Zones")).getByRole("link", { name: "Zones" })).toHaveAttribute(
    "aria-current",
    "page",
  );
});

test("a group menu lists its pages as menu items and navigates to them", async () => {
  renderTopNav({ route: "/" });

  const menu = openGroup("Zones");
  const item = within(menu).getByRole("menuitem", { name: /zones/i });
  fireEvent.click(item);

  await waitFor(() => expect(screen.getByTestId("pathname")).toHaveTextContent("/zones"));
});

test("the search cell opens the command palette", async () => {
  const onOpen = vi.fn<() => void>();
  renderTopNav({ onOpen });

  fireEvent.click(screen.getByRole("button", { name: /search/i }));

  expect(onOpen).toHaveBeenCalledTimes(1);
});

// The shortcut is a key, so it's a <kbd> — rnui's Kbd, not two more
// characters of label text. The word stays in the accessible name even
// where the viewport hides it, so the cell never announces as just "⌘K".
test("the search cell names itself and shows the shortcut as a Kbd", async () => {
  renderTopNav();

  const search = screen.getByRole("button", { name: /search/i });
  expect(search).toHaveAccessibleName("search ⌘K");

  const kbd = search.querySelector("kbd");
  expect(kbd).not.toBeNull();
  expect(kbd).toHaveTextContent("⌘K");
  // The old cell spelled the affordance out in the label instead.
  expect(search.textContent).not.toMatch(/\//);
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

// Stage 2 parked resolver health in row 2's filled cell for want of a
// home. Row 2 is the *contextual* row and that cell is its call to action,
// so health moved up beside blocking — the shell's two "is this thing
// working" readouts, in one place.
test("the two health readouts sit together in row 1, and only blocking is filled", async () => {
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

  // Blocking is row 1's filled cell — it's the control that changes what
  // dnsaur does. Health beside it is a readout and stays flat text, so the
  // bar doesn't shout twice.
  expect(blocking.className).toContain("bg-primary");
  expect(blocking.className).toContain("text-primary-foreground");
  expect(resolver.className).not.toContain("bg-primary");
  expect(resolver.className).toContain("text-primary");
  // The strings stay sentence case (accessible names, assertions); the cell
  // is what uppercases them.
  expect(blocking.className).toContain("uppercase");
});

// The fill is a claim, so only the states we've actually read from
// GET /blocking may make it. A paused instance wears the warning tone; a
// failed read gets no fill at all, which is what stops "I couldn't tell"
// from looking like the confident "BLOCKING ACTIVE".
/** The cell's classes as tokens, so `bg-warning` can't be satisfied by the
 * 10%-opacity wash `bg-warning/10` that this cell used to wear. */
function classes(el: HTMLElement): string[] {
  return el.className.split(/\s+/);
}

test("a paused instance fills warning; an unreadable status fills nothing", async () => {
  server.use(blockingHandler({ 0: Date.now() + 5 * 60_000 }));
  const { unmount } = renderTopNav();

  const paused = screen.getByRole("button", { name: /open blocking controls/i });
  await waitFor(() => expect(paused).toHaveTextContent(/^paused · \d+:[0-5]\d/i));
  // A real fill, not a tint.
  expect(classes(paused)).toContain("bg-warning");
  expect(paused.className).not.toContain("bg-primary");
  // --warning-foreground is rnui's amber *text* tone, not ink for an amber
  // fill: it measures 2.10:1 on --warning in light and 1.41:1 in dark. The
  // app's own on-solid-amber token is what clears AA in both modes.
  expect(paused.className).not.toContain("text-warning-foreground");
  expect(classes(paused)).toContain("text-warning-solid-foreground");
  unmount();

  server.use(
    http.get("/api/v1/blocking", () => HttpResponse.json({ error: "boom" }, { status: 500 })),
  );
  renderTopNav();

  const unknown = screen.getByRole("button", { name: /open blocking controls/i });
  await waitFor(() => expect(unknown).toHaveTextContent(/status unavailable/i), { timeout: 4000 });
  expect(unknown).not.toHaveTextContent(/blocking active/i);
  expect(unknown.className).not.toContain("bg-primary");
  expect(unknown.className).not.toContain("bg-warning");
  expect(classes(unknown)).toContain("text-destructive");
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
});

// --- the query log's contextual cells ----------------------------------------

/** The one cell that reports what the table is doing. */
function modeCell(): HTMLElement {
  return screen.getByRole("status", { name: "Query log" });
}

function tailToggle(): HTMLElement {
  return screen.getByRole("button", { name: /(pause|resume) tail/i });
}

/** The dot's tone — the readout's other half, and the half a screen reader
 * never gets, so it has to agree with the word beside it. */
function modeDot(): HTMLElement {
  return modeCell().querySelector("[aria-hidden]") as HTMLElement;
}

test("the query log's chrome carries one mode readout and the filled tail toggle", async () => {
  renderTopNav({ route: "/queries" });

  // Its own tabs, still marking the page you're on.
  expect(within(tabs("Monitor")).getByRole("link", { name: "Query Log" })).toHaveAttribute(
    "aria-current",
    "page",
  );
  // Not the dashboard's pair — each screen gets its own.
  expect(screen.queryByRole("link", { name: /view query log/i })).not.toBeInTheDocument();
  expect(screen.queryByRole("group", { name: "Time window" })).not.toBeInTheDocument();

  // The design's filled cell on this screen, labelled with what it does
  // rather than with the state it's in — the readout beside it owns that.
  expect(tailToggle()).toHaveTextContent("Pause tail");
  expect(classes(tailToggle())).toContain("bg-primary");

  // An earlier revision carried this note up here; it's gone from the
  // design, and the mode readout says the useful half of it.
  expect(screen.queryByText(/table trails the tail/i)).not.toBeInTheDocument();
});

// One cell, four states. The previous revision had the shell and the page
// each rendering their own version of this, so /queries showed four
// readouts of one piece of state across two rows.
test("the mode readout collapses filter, pause and stream health into one cell", async () => {
  renderTopNav({ route: "/queries" });

  // Default: nothing filtered, not paused, stream still coming up.
  expect(modeCell()).toHaveTextContent(/reconnecting/i);
  expect(classes(modeDot())).toContain("bg-warn");

  act(() => setLiveTailReport({ filtered: false, streamState: "open", reconnect: () => {} }));
  expect(modeCell()).toHaveTextContent(/live tail/i);
  expect(classes(modeDot())).toContain("bg-primary");

  // A pause outranks the stream's own state — a paused tail isn't
  // connecting to anything.
  act(() => setLiveTailPaused(true));
  expect(modeCell()).toHaveTextContent(/^paused$/i);
  expect(classes(modeDot())).toContain("bg-muted-foreground");
  act(() => setLiveTailPaused(false));

  // ...and a filter outranks both: the page has swapped the stream for
  // paged search, so the stream's health is not a thing to report.
  act(() => setLiveTailReport({ filtered: true, streamState: "failed", reconnect: () => {} }));
  expect(modeCell()).toHaveTextContent(/filtered · paged/i);
  expect(classes(modeDot())).toContain("bg-muted-foreground");
  expect(screen.queryByRole("button", { name: "Reconnect" })).not.toBeInTheDocument();
});

// "failed" is not a quieter shade of "reconnecting": api/sse.ts has given up
// after six attempts and nothing further happens without a click.
test("a dead stream says so and offers the click that revives it", async () => {
  const reconnect = vi.fn<() => void>();
  renderTopNav({ route: "/queries" });
  act(() => setLiveTailReport({ filtered: false, streamState: "failed", reconnect }));

  expect(modeCell()).toHaveTextContent(/disconnected/i);
  expect(classes(modeDot())).toContain("bg-destructive");

  fireEvent.click(screen.getByRole("button", { name: "Reconnect" }));
  expect(reconnect).toHaveBeenCalledTimes(1);
});

test("the toggle pauses and resumes the shared tail flag, and wears the warning fill while paused", async () => {
  renderTopNav({ route: "/queries" });
  act(() => setLiveTailReport({ filtered: false, streamState: "open", reconnect: () => {} }));

  fireEvent.click(tailToggle());
  await waitFor(() => expect(tailToggle()).toHaveTextContent("Resume tail"));
  // The flag the routed page below reads (lib/live-tail.ts) really moved.
  expect(readLiveTailPaused()).toBe(true);
  expect(classes(tailToggle())).toContain("bg-warn");
  // White on solid --warning is 4.10:1 in light and 2.18:1 in dark; the
  // app's on-solid-amber token is what clears AA in both.
  expect(classes(tailToggle())).toContain("text-warning-solid-foreground");
  expect(tailToggle().className).not.toContain("text-primary-foreground");

  fireEvent.click(tailToggle());
  await waitFor(() => expect(tailToggle()).toHaveTextContent("Pause tail"));
  expect(readLiveTailPaused()).toBe(false);
});

// Everything above is contextual to /queries and governs nothing elsewhere.
test("no other screen gets the tail cells", async () => {
  renderTopNav({ route: "/settings" });

  expect(screen.queryByRole("button", { name: /(pause|resume) tail/i })).not.toBeInTheDocument();
  expect(screen.queryByRole("status", { name: "Query log" })).not.toBeInTheDocument();
});
