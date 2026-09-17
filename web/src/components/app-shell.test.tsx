import { http, HttpResponse } from "msw";
import type { QueryClient } from "@tanstack/react-query";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { Route, Routes } from "react-router";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { server } from "../test/msw-server";
import { dhcpStatus } from "../test/msw-handlers";
import { renderWithProviders } from "../test/render";
import { settingsKeys } from "../hooks/use-settings";
import type { ResolverStatus } from "../api/types";
import { AppShell } from "./app-shell";
import { SettingsPage } from "../pages/settings";

// Everything wrong with the running server used to stack under the top bar,
// one page-wide `role="alert"` strip per fact, on every screen. Ten facts
// later that was three bars eating the top of every page and an operator who
// had stopped reading them. They are now one cell in the top bar — the
// resolver readout, which becomes a count — and a panel of links under it.
//
// This file covers both halves of the move: what the cell says, and what the
// panel lists. The facts themselves (their text, ids, targets and ordering
// bands) are pinned in lib/serving.test.ts.

const OFF_SERVING: ResolverStatus["serving"] = {
  dot: { enabled: false, listening: false, addr: "" },
  doh: { enabled: false, listening: false, addr: "" },
};

function mockResolverStatus(status: Partial<ResolverStatus>) {
  server.use(
    http.get("/api/v1/resolver/status", () =>
      HttpResponse.json({
        encryption_downgraded: false,
        reason: "",
        serving: OFF_SERVING,
        ...status,
      }),
    ),
  );
}

function renderOnOtherRoute() {
  return renderWithProviders(
    <Routes>
      <Route element={<AppShell />}>
        <Route index element={<h1>Some other page</h1>} />
        {/* Stand-ins for the two bands the panel's rows link to. Without a
            matching route a click on a row leaves the router with nothing to
            render — the shell included — and the assertion after it would be
            about an empty document rather than about the panel. */}
        <Route path="settings" element={<h1>Settings</h1>} />
        <Route path="dhcp" element={<h1>Scopes</h1>} />
      </Route>
    </Routes>,
  );
}

/** Waits until GET /resolver/status has actually resolved, so a following
 * "nothing is wrong" assertion is about the answer rather than about its
 * absence. `result` is renderWithProviders' return, which carries the
 * QueryClient. */
async function settledStatus(result: { queryClient: QueryClient }): Promise<void> {
  await waitFor(() =>
    expect(result.queryClient.getQueryState(settingsKeys.resolverStatus)?.status).toBe("success"),
  );
}

/** The status cell once it has something to report. Named by its count, the
 * way a screen reader reads it. */
function cell(name: string | RegExp): Promise<HTMLElement> {
  return screen.findByRole("button", { name });
}

/**
 * Opens the panel and hands back its rows.
 *
 * `fireEvent.click` rather than userEvent's full pointer sequence, for the
 * reason top-nav.test.tsx gives about the same base-ui machinery: under
 * jsdom there is no geometry to hit-test, so the pointerdown half trips the
 * popover's own outside-click detection and closes it again.
 */
async function openPanel(name: string | RegExp): Promise<HTMLElement> {
  fireEvent.click(await cell(name));
  return screen.findByRole("dialog");
}

function rows(panel: HTMLElement): HTMLElement[] {
  return within(panel).getAllByRole("link");
}

/** Swaps what the server reports and refetches, rather than waiting out the
 * 5s trouble poll: what these tests are about is that the screen follows
 * the status, not how often the status is asked for. */
async function reportInstead(
  result: { queryClient: QueryClient },
  status: Partial<ResolverStatus>,
): Promise<void> {
  mockResolverStatus(status);
  await result.queryClient.invalidateQueries({ queryKey: settingsKeys.resolverStatus });
}

/** The polite live region: the shell's one `aria-live` node, which is
 * always mounted and so cannot be found by role. */
function politeRegion(): HTMLElement {
  const region = document.querySelector('[aria-live="polite"]');
  if (!region) throw new Error("the shell has no polite live region");
  return region as HTMLElement;
}

const EXPIRING = { not_after: "2026-09-17T00:00:00Z", expiring_soon: true } as const;

test("nothing wrong: the cell is the resolver readout and the shell shows no alert", async () => {
  const result = renderOnOtherRoute();

  expect(screen.getByText("Some other page")).toBeInTheDocument();
  // There's no fetch-backed element in this tree to key a findBy* off (the
  // dummy route renders synchronously), so the wait is on the
  // resolver-status query itself reaching success. A bare setTimeout here
  // would pass on a slow machine because the response had not landed yet —
  // which is the one way a negative assertion can be green and mean nothing.
  await settledStatus(result);

  expect(screen.getByRole("status")).toHaveTextContent(/dns ok/i);
  expect(screen.queryByRole("button", { name: /issue/ })).not.toBeInTheDocument();
  expect(screen.queryByRole("alert")).not.toBeInTheDocument();
});

test("one fact: the cell counts it, tinted amber, and the panel is not opened for you", async () => {
  mockResolverStatus({
    certificate: { not_after: "2026-09-17T00:00:00Z", expiring_soon: true },
  });

  renderOnOtherRoute();

  const button = await cell("1 issue");
  expect(button).toHaveTextContent(/1 issue/i);
  expect(button).toHaveClass("text-warning-foreground");
  expect(button).toHaveAttribute("aria-haspopup", "dialog");
  expect(button).toHaveAttribute("aria-expanded", "false");
  // No toast, and nothing opens itself.
  expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  // Amber is polite: it does not interrupt what is being read.
  expect(screen.queryByRole("alert")).not.toBeInTheDocument();
});

// The one red fact: every query is going out in the clear because the stored
// `upstreams` value would not parse. It is the only one that earns an
// interruption, and it takes the whole cell with it.
test("a red fact turns the cell red and is announced assertively", async () => {
  mockResolverStatus({
    encryption_downgraded: true,
    reason: 'upstream "tls://1.1.1.1:853": missing "#name"',
    certificate: { not_after: "2026-09-17T00:00:00Z", expiring_soon: true },
  });

  renderOnOtherRoute();

  const button = await cell("2 issues");
  expect(button).toHaveClass("text-destructive-foreground");
  expect(button).not.toHaveClass("text-warning-foreground");

  const alert = await screen.findByRole("alert");
  expect(alert).toHaveTextContent(/Encryption is off — upstreams could not be parsed/);
  expect(alert).toHaveTextContent('missing "#name"');
  // The amber fact beside it is not in the assertive region.
  expect(alert).not.toHaveTextContent(/TLS certificate/);
});

/** The boards' six-fact panel: one red, three that wait on the operator, two
 * that clear on their own. */
function sixFacts() {
  mockResolverStatus({
    encryption_downgraded: true,
    reason: 'unexpected character ";" at position 14',
    serving: {
      dot: {
        enabled: true,
        listening: false,
        addr: ":853",
        error: "listen tcp :853: bind: permission denied",
      },
      doh: { enabled: false, listening: false, addr: "" },
    },
    certificate: { not_after: "2026-09-17T00:00:00Z", expiring_soon: true },
    sync: {
      role: "main",
      replicas: [
        {
          instance_id: "2IB4ABB4",
          dns_addr: "10.0.0.6:53",
          version_applied: 410,
          last_seen: Date.now() - 3 * 3_600_000,
          stale: true,
          dhcp: true,
        },
      ],
    },
    dhcp: dhcpStatus({
      engine: "config rejected",
      message: "subnet4[1]: pool 192.168.150.100-192.168.150.199 is not in subnet 192.168.151.0/24",
      ha: {
        mode: "hot-standby",
        local_state: "partner-down",
        peer: "backup-box",
        remote_state: "hot-standby",
        communication_interrupted: true,
        unacked_clients: 2,
      },
    }),
  });
}

test("six facts: the count, the order, and a link to the band that fixes each", async () => {
  sixFacts();
  renderOnOtherRoute();

  const panel = await openPanel("6 issues");
  expect(panel).toHaveTextContent("6 open · 1 red");

  // Red first, then the facts that wait on the operator, then the ones that
  // end without anyone doing anything. Every row carries NEW: the panel has
  // not been opened before, so nothing on it has been seen yet.
  expect(rows(panel).map((row) => row.textContent)).toEqual([
    'Encryption is off — upstreams could not be parsed, unexpected character ";" at position 14NEWSettings › Upstreams →',
    "DNS-over-TLS is enabled but not listeningNEWSettings › Protocols →listen tcp :853: bind: permission denied",
    "TLS certificate expires in 1 day — 17 Sep 2026NEWSettings › Protocols →",
    "DHCP config rejected: subnet4[1]: pool 192.168.150.100-192.168.150.199 is not in subnet 192.168.151.0/24NEWDHCP › Scopes →",
    "Replica 2IB4ABB4 not seen for 3hNEWSettings › Sync →",
    "DHCP partner unreachableNEWDHCP › Scopes →",
  ]);

  expect(rows(panel).map((row) => row.getAttribute("href"))).toEqual([
    "/settings#upstreams",
    "/settings#protocols",
    "/settings#protocols",
    "/dhcp",
    "/settings#sync",
    "/dhcp",
  ]);
});

// The bind error is the row's own second line, muted — not appended to the
// sentence, the way the strip used to run the two together.
test("a listener's error is the row's second line, under the fact it belongs to", async () => {
  sixFacts();
  renderOnOtherRoute();

  const panel = await openPanel("6 issues");
  const dot = rows(panel)[1];
  expect(dot).toHaveTextContent("DNS-over-TLS is enabled but not listening");
  expect(within(dot).getByText("listen tcp :853: bind: permission denied")).toHaveClass(
    "text-muted-foreground",
  );
});

// A fact nobody has looked at yet says so. Opening the panel is what makes
// every row on screen no longer new; there is nothing to dismiss.
test("NEW marks the rows the panel has not shown yet, and opening it clears them", async () => {
  sixFacts();
  renderOnOtherRoute();

  const panel = await openPanel("6 issues");
  expect(within(panel).getAllByText("NEW")).toHaveLength(6);

  fireEvent.keyDown(panel, { key: "Escape" });
  await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
  // Focus comes back to the cell it opened from.
  expect(await cell("6 issues")).toHaveFocus();

  const reopened = await openPanel("6 issues");
  expect(within(reopened).queryByText("NEW")).not.toBeInTheDocument();
});

// Facts clear on their own and take their rows with them. At zero the cell
// is the plain resolver readout again.
test("a fact that clears loses its row, and at zero the cell goes back to DNS OK", async () => {
  mockResolverStatus({
    dhcp: dhcpStatus({ engine: "unreachable" }),
    certificate: { not_after: "2026-09-17T00:00:00Z", expiring_soon: true },
  });

  const result = renderOnOtherRoute();
  const panel = await openPanel("2 issues");
  expect(rows(panel)).toHaveLength(2);

  // The engine answered again. Refetching explicitly rather than waiting out
  // the 5s trouble poll: what is being tested is that the row follows the
  // status, not how often the status is asked for.
  mockResolverStatus({
    certificate: { not_after: "2026-09-17T00:00:00Z", expiring_soon: true },
  });
  await result.queryClient.invalidateQueries({ queryKey: settingsKeys.resolverStatus });
  await waitFor(() => expect(rows(screen.getByRole("dialog"))).toHaveLength(1));
  expect(screen.getByRole("dialog")).toHaveTextContent("1 open");
  expect(screen.getByRole("dialog")).not.toHaveTextContent("red");

  mockResolverStatus({});
  await result.queryClient.invalidateQueries({ queryKey: settingsKeys.resolverStatus });
  await waitFor(() => expect(screen.getByRole("status")).toHaveTextContent(/dns ok/i));
  expect(screen.queryByRole("button", { name: /issue/ })).not.toBeInTheDocument();
});

// A resolver that is not answering outranks every fact in the panel: the
// cell says so whatever the count, and the panel still opens from it.
test("DNS down wins the cell, and the facts are still one click away", async () => {
  server.use(http.get("/api/v1/health", () => HttpResponse.json({ error: "x" }, { status: 500 })));
  mockResolverStatus({
    certificate: { not_after: "2026-09-17T00:00:00Z", expiring_soon: true },
  });

  renderOnOtherRoute();

  const button = await cell("DNS down, 1 issue");
  expect(button).toHaveTextContent(/dns down/i);
  expect(button).not.toHaveTextContent(/1 issue/i);
  expect(button).toHaveClass("text-destructive-foreground");

  const panel = await openPanel("DNS down, 1 issue");
  expect(rows(panel)[0]).toHaveTextContent("TLS certificate expires in");
});

// Pinned from settings.test.tsx: Settings does not mount any of this itself,
// but it has to show there, because Settings renders inside the shell
// exactly like every other screen.
test("the cell is the same on the Settings route, via the shell rather than the page", async () => {
  mockResolverStatus({ encryption_downgraded: true, reason: 'missing "#name"' });

  renderWithProviders(
    <Routes>
      <Route element={<AppShell />}>
        <Route path="settings" element={<SettingsPage />} />
      </Route>
    </Routes>,
    { route: "/settings" },
  );

  const panel = await openPanel("1 issue");
  expect(rows(panel)[0]).toHaveTextContent(/Encryption is off/);
});

// The Scopes page carries the engine's own status line with Apply again
// beside it, and the strip used to drop the facts that line repeated. The
// panel does not: nothing stacks above the page any more, so there is no
// second copy to suppress — and the row is what takes you to the line.
test("the Scopes route lists the DHCP facts like every other route", async () => {
  mockResolverStatus({
    dhcp: dhcpStatus({
      engine: "config rejected",
      message: "scope lan: no local address is inside 10.0.0.0/16",
    }),
  });

  renderWithProviders(
    <Routes>
      <Route element={<AppShell />}>
        <Route path="dhcp" element={<h1>Scopes</h1>} />
      </Route>
    </Routes>,
    { route: "/dhcp" },
  );
  await screen.findByRole("heading", { name: "Scopes" });

  const panel = await openPanel("1 issue");
  expect(rows(panel)[0]).toHaveTextContent(
    "DHCP config rejected: scope lan: no local address is inside 10.0.0.0/16",
  );
  expect(rows(panel)[0]).toHaveAttribute("href", "/dhcp");
});

// Most instances have no engine at all, a healthy one is the configuration
// working, and being a replica is a state. None of the three is a fact.
test("a box with no engine, a healthy one, and a plaintext peer get no cell", async () => {
  mockResolverStatus({
    dhcp: { enabled: false, table_age_seconds: 0, scopes: [] },
    sync: { role: "replica", peer_url: "http://main.lan", plain_http: true },
  });

  const result = renderOnOtherRoute();
  expect(await screen.findByRole("link", { name: "Replica · main.lan" })).toBeInTheDocument();
  await settledStatus(result);
  expect(screen.queryByRole("button", { name: /issue/ })).not.toBeInTheDocument();
  result.unmount();

  mockResolverStatus({ dhcp: dhcpStatus() });
  const healthy = renderOnOtherRoute();
  await settledStatus(healthy);
  expect(screen.queryByRole("button", { name: /issue/ })).not.toBeInTheDocument();
});

// --- regressions ----------------------------------------------------------

// The panel unmounts with its last row, and `open` used to outlive it: the
// next fact to arrive mounted the popover *already open*, which is the one
// thing the boards say never happens — and the reader's focus went with the
// unmount rather than back to the cell.
test("the panel does not come back open when a new fact arrives after the last one cleared", async () => {
  mockResolverStatus({ certificate: EXPIRING });

  const result = renderOnOtherRoute();
  await openPanel("1 issue");

  await reportInstead(result, {});
  await waitFor(() => expect(screen.getByRole("status")).toHaveTextContent(/dns ok/i));
  expect(screen.queryByRole("dialog")).not.toBeInTheDocument();

  await reportInstead(result, { dhcp: dhcpStatus({ engine: "unreachable" }) });
  await cell("1 issue");
  expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
});

// A fact that cleared and came back is a new arrival, and `reorder` already
// treated it as one — it took a fresh place at the bottom of its band. The
// tag has to agree, or the row moved with nothing on it saying why.
test("a fact that clears and comes back is NEW again", async () => {
  mockResolverStatus({ certificate: EXPIRING });

  const result = renderOnOtherRoute();
  const panel = await openPanel("1 issue");
  expect(within(panel).getAllByText("NEW")).toHaveLength(1);
  fireEvent.keyDown(panel, { key: "Escape" });
  await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
  expect(within(await openPanel("1 issue")).queryByText("NEW")).not.toBeInTheDocument();
  fireEvent.keyDown(screen.getByRole("dialog"), { key: "Escape" });
  await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());

  // The certificate was renewed, then the new one started expiring too.
  await reportInstead(result, {});
  await waitFor(() => expect(screen.getByRole("status")).toHaveTextContent(/dns ok/i));
  await reportInstead(result, { certificate: EXPIRING });

  expect(within(await openPanel("1 issue")).getAllByText("NEW")).toHaveLength(1);
});

// A live region only announces what actually changes in the DOM. The same
// fault clearing and coming back carries the same words, and re-rendering an
// identical string changes nothing — which left the second arrival silent.
test("the same fact arriving twice is announced twice, and a cleared red fact leaves no alert", async () => {
  mockResolverStatus({ certificate: EXPIRING });

  const result = renderOnOtherRoute();
  await waitFor(() => expect(politeRegion()).toHaveTextContent("TLS certificate expires in"));
  const firstSaid = politeRegion().firstElementChild;

  await reportInstead(result, {});
  await waitFor(() => expect(politeRegion()).toHaveTextContent(""));
  await reportInstead(result, { certificate: EXPIRING });
  await waitFor(() => expect(politeRegion()).toHaveTextContent("TLS certificate expires in"));
  // Same words, a different node: what a screen reader notices is the
  // mutation, not the string.
  expect(politeRegion().firstElementChild).not.toBe(firstSaid);

  // And the assertive region does not keep reporting a fault that ended.
  await reportInstead(result, { encryption_downgraded: true, reason: "boom" });
  expect(await screen.findByRole("alert")).toHaveTextContent("Encryption is off");
  await reportInstead(result, {});
  await waitFor(() => expect(screen.queryByRole("alert")).not.toBeInTheDocument());
});

// Clicking a row is how most readers leave the panel, and it used to be the
// one exit that did not count as having seen the rows.
test("leaving the panel by a row still clears NEW", async () => {
  mockResolverStatus({ certificate: EXPIRING, dhcp: dhcpStatus({ engine: "unreachable" }) });

  renderOnOtherRoute();
  const panel = await openPanel("2 issues");
  expect(within(panel).getAllByText("NEW")).toHaveLength(2);

  fireEvent.click(rows(panel)[0]);
  await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());

  expect(within(await openPanel("2 issues")).queryByText("NEW")).not.toBeInTheDocument();
});

// Below the boards' 640px the panel is a bottom sheet rather than a popover
// anchored to a cell that is most of the bar. Same rows either way, so what
// is pinned here is that the width picks the other container at all.
test("on a phone the same rows arrive in a bottom sheet", async () => {
  vi.spyOn(window, "matchMedia").mockImplementation(
    (query: string) =>
      ({
        matches: query === "(max-width: 640px)",
        media: query,
        onchange: null,
        addListener: () => {},
        removeListener: () => {},
        addEventListener: () => {},
        removeEventListener: () => {},
        dispatchEvent: () => false,
      }) as MediaQueryList,
  );
  mockResolverStatus({ certificate: EXPIRING, dhcp: dhcpStatus({ engine: "unreachable" }) });

  renderOnOtherRoute();
  const panel = await openPanel("2 issues");

  expect(panel).toHaveAttribute("data-slot", "sheet-content");
  expect(panel).toHaveAttribute("data-side", "bottom");
  expect(rows(panel).map((row) => row.getAttribute("href"))).toEqual([
    "/settings#protocols",
    "/dhcp",
  ]);
});

// The certificate fixture expires on 17 Sep 2026, and both the day count and
// the "expired" wording are measured from today, so today is pinned. Only
// Date is faked: testing-library's waitFor keeps its real timers.
beforeEach(() => {
  vi.useFakeTimers({ toFake: ["Date"] });
  vi.setSystemTime(new Date("2026-09-16T00:00:00Z"));
});
afterEach(() => {
  vi.useRealTimers();
  vi.restoreAllMocks();
});
