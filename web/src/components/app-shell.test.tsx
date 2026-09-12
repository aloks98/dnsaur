import { http, HttpResponse } from "msw";
import type { QueryClient } from "@tanstack/react-query";
import { expect, test } from "vitest";
import { Route, Routes } from "react-router";
import { screen, waitFor } from "@testing-library/react";
import { server } from "../test/msw-server";
import { renderWithProviders } from "../test/render";
import { settingsKeys } from "../hooks/use-settings";
import type { ResolverStatus } from "../api/types";
import { AppShell } from "./app-shell";
import { SettingsPage } from "../pages/settings";

// The encryption-downgrade warning is a fact about the running server — the
// stored `upstreams` asked for encryption and could not get it, so every
// query is going out in the clear — not a fact about any one screen. It has
// to follow the operator everywhere, the same way an "API unreachable"
// banner would, or someone who lands on Query Log or Zones never learns.
// Mounted once, in the shell, rather than duplicated per page.
//
// ServingBanners (below) is mounted right beside it for the same reason —
// see serving-banners.tsx's own doc comment — so this file covers both.

const OFF_SERVING: ResolverStatus["serving"] = {
  dot: { enabled: false, listening: false, addr: "" },
  doh: { enabled: false, listening: false, addr: "" },
};

function mockDowngraded(reason = 'upstream "tls://1.1.1.1:853": missing "#name"') {
  server.use(
    http.get("/api/v1/resolver/status", () =>
      HttpResponse.json({ encryption_downgraded: true, reason, serving: OFF_SERVING }),
    ),
  );
}

function mockResolverStatus(status: Omit<ResolverStatus, "encryption_downgraded" | "reason">) {
  server.use(
    http.get("/api/v1/resolver/status", () =>
      HttpResponse.json({ encryption_downgraded: false, reason: "", ...status }),
    ),
  );
}

test("the encryption-downgrade banner appears on a route that is not Settings", async () => {
  mockDowngraded();

  renderWithProviders(
    <Routes>
      <Route element={<AppShell />}>
        <Route index element={<h1>Some other page</h1>} />
      </Route>
    </Routes>,
  );

  // The routed page itself still renders — the banner sits above it, it
  // doesn't replace it.
  expect(screen.getByText("Some other page")).toBeInTheDocument();

  // The banner depends on a fetch (useResolverStatus), so it doesn't appear
  // in the same tick as the always-synchronous routed content above.
  const alert = await screen.findByRole("alert");
  expect(alert).toHaveTextContent(/Encryption is off/);
  expect(alert).toHaveTextContent(/falling back to plaintext resolvers/);
  expect(alert).toHaveTextContent('missing "#name"');
});

// Pinned from settings.test.tsx: Settings no longer mounts the banner
// itself (see encryption-downgrade-banner.tsx), but it still has to show
// there, because Settings renders inside the shell exactly like every
// other screen. This is the regression that would slip through if the
// shell mount were ever removed without anyone noticing Settings had lost
// its own copy.
test("the banner still appears on the Settings route, now via the shell rather than the page", async () => {
  mockDowngraded('upstream "tls://1.1.1.1:853": missing "#name"');

  renderWithProviders(
    <Routes>
      <Route element={<AppShell />}>
        <Route path="settings" element={<SettingsPage />} />
      </Route>
    </Routes>,
    { route: "/settings" },
  );

  const alert = await screen.findByRole("alert");
  expect(alert).toHaveTextContent(/Encryption is off/);
  expect(alert).toHaveTextContent('missing "#name"');
});

// A warning that outlives the fix teaches the operator to ignore the one
// banner that means their DNS is unencrypted — and one that shows up on
// every screen unconditionally would be worse, not better. The default msw
// handler already reports encryption_downgraded: false, so this is the
// shell's steady state everywhere else in the suite; pinned explicitly here
// once, on a non-Settings route, since that's the route newly in scope.
test("no banner anywhere in the shell when the server reports nothing wrong", async () => {
  const result = renderWithProviders(
    <Routes>
      <Route element={<AppShell />}>
        <Route index element={<h1>Some other page</h1>} />
      </Route>
    </Routes>,
  );

  expect(screen.getByText("Some other page")).toBeInTheDocument();
  // There's no fetch-backed element in this tree to key a findBy* off (the
  // dummy route renders synchronously), so the wait is on the
  // resolver-status query itself reaching success. A bare setTimeout here
  // would pass on a slow machine because the response had not landed yet —
  // which is the one way a negative assertion can be green and mean
  // nothing.
  await settledStatus(result);
  expect(screen.queryByRole("alert")).not.toBeInTheDocument();
});

/** Waits until GET /resolver/status has actually resolved, so a following
 * "no banner" assertion is about the answer rather than about its absence.
 * `result` is renderWithProviders' return, which carries the QueryClient. */
async function settledStatus(result: { queryClient: QueryClient }): Promise<void> {
  await waitFor(() =>
    expect(result.queryClient.getQueryState(settingsKeys.resolverStatus)?.status).toBe("success"),
  );
}

function renderOnOtherRoute() {
  return renderWithProviders(
    <Routes>
      <Route element={<AppShell />}>
        <Route index element={<h1>Some other page</h1>} />
      </Route>
    </Routes>,
  );
}

// The Protocols group's own shell banners — see serving-banners.tsx. Same
// "follows the operator everywhere" reasoning as the encryption-downgrade
// banner above, pinned the same way: on a route that is not Settings.

test("a DoT listener that's enabled but not listening shows a shell banner on a non-Settings route", async () => {
  mockResolverStatus({
    serving: {
      dot: {
        enabled: true,
        listening: false,
        addr: ":853",
        error: "listen tcp :853: bind: permission denied",
      },
      doh: { enabled: false, listening: false, addr: "" },
    },
  });

  renderOnOtherRoute();

  const alert = await screen.findByRole("alert");
  expect(alert).toHaveTextContent(
    "DNS-over-TLS is enabled but not listening — listen tcp :853: bind: permission denied",
  );
});

test("a DoH listener that's enabled but not listening shows a shell banner", async () => {
  mockResolverStatus({
    serving: {
      dot: { enabled: false, listening: false, addr: "" },
      doh: {
        enabled: true,
        listening: false,
        addr: ":443",
        error: "listen tcp :443: bind: permission denied",
      },
    },
  });

  renderOnOtherRoute();

  const alert = await screen.findByRole("alert");
  expect(alert).toHaveTextContent(
    "DNS-over-HTTPS is enabled but not listening — listen tcp :443: bind: permission denied",
  );
});

test("a certificate expiring within the warning window shows a shell banner", async () => {
  mockResolverStatus({
    serving: OFF_SERVING,
    certificate: { not_after: "2026-09-17T00:00:00Z", expiring_soon: true },
  });

  renderOnOtherRoute();

  const alert = await screen.findByRole("alert");
  expect(alert).toHaveTextContent(/TLS certificate expires in \d+ days? — 17 Sep 2026/);
});

// The artboard's own reason this is a list rather than a single banner: DoT
// and DoH fail independently, and a certificate can be expiring at the same
// time either of those is broken — spec's "shell banners are a list".
test("a failed DoT listener, a failed DoH listener and an expiring certificate all show at once", async () => {
  mockResolverStatus({
    serving: {
      dot: {
        enabled: true,
        listening: false,
        addr: ":853",
        error: "listen tcp :853: bind: permission denied",
      },
      doh: {
        enabled: true,
        listening: false,
        addr: ":443",
        error: "listen tcp :443: bind: permission denied",
      },
    },
    certificate: { not_after: "2026-09-17T00:00:00Z", expiring_soon: true },
  });

  renderOnOtherRoute();

  const alerts = await screen.findAllByRole("alert");
  expect(alerts).toHaveLength(3);
  const text = alerts.map((a) => a.textContent).join("\n");
  expect(text).toContain("DNS-over-TLS is enabled but not listening");
  expect(text).toContain("DNS-over-HTTPS is enabled but not listening");
  expect(text).toContain("TLS certificate expires in");
});

test("a protocol that is enabled and listening gets no banner", async () => {
  mockResolverStatus({
    serving: {
      dot: { enabled: true, listening: true, addr: ":853" },
      doh: { enabled: false, listening: false, addr: "" },
    },
  });

  const result = renderOnOtherRoute();

  expect(screen.getByText("Some other page")).toBeInTheDocument();
  await settledStatus(result);
  expect(screen.queryByRole("alert")).not.toBeInTheDocument();
});

// Sync's own three facts (spec §8). Same shell mount and the same list, for
// the same reason: an operator who never opens Settings still has to learn
// that this box stopped taking its configuration from the main, that the
// bundle — every TSIG secret on it — is travelling in the clear, or that a
// replica of theirs has gone quiet.

test("a failed pull shows the server's own reason in a shell banner", async () => {
  mockResolverStatus({
    serving: OFF_SERVING,
    sync: {
      role: "replica",
      peer_url: "https://main.lan",
      last_error: 'Get "https://main.lan/api/v1/sync/version": dial tcp: connection refused',
    },
  });

  renderOnOtherRoute();

  const alert = await screen.findByRole("alert");
  expect(alert).toHaveTextContent(
    'Last pull failed: Get "https://main.lan/api/v1/sync/version": dial tcp: connection refused',
  );
});

test("a peer reached over plain http shows a shell banner", async () => {
  mockResolverStatus({
    serving: OFF_SERVING,
    sync: { role: "replica", peer_url: "http://main.lan", plain_http: true },
  });

  renderOnOtherRoute();

  expect(await screen.findByRole("alert")).toHaveTextContent("Peer reached over plain HTTP");
});

test("a stale replica shows a shell banner on the main, naming it and how long it has been quiet", async () => {
  mockResolverStatus({
    serving: OFF_SERVING,
    sync: {
      role: "main",
      replicas: [
        {
          instance_id: "eve-2",
          dns_addr: "10.0.0.6:53",
          version_applied: 410,
          last_seen: Date.now() - 95 * 60_000,
          stale: true,
        },
        {
          instance_id: "eve-3",
          dns_addr: "10.0.0.7:53",
          version_applied: 412,
          last_seen: Date.now() - 30_000,
          stale: false,
        },
      ],
    },
  });

  renderOnOtherRoute();

  // Only the stale one, and the duration is how long it has been quiet
  // rather than a wall-clock stamp nobody can subtract from in their head.
  const alerts = await screen.findAllByRole("alert");
  expect(alerts).toHaveLength(1);
  expect(alerts[0]).toHaveTextContent("Replica eve-2 not seen for 1h 35m");
});

test("a replica that is up to date over https gets no banner", async () => {
  mockResolverStatus({
    serving: OFF_SERVING,
    sync: {
      role: "replica",
      peer_url: "https://main.lan",
      peer_version: 412,
      applied_version: 412,
      last_pull_at: Date.now() - 20_000,
    },
  });

  const result = renderOnOtherRoute();

  expect(screen.getByText("Some other page")).toBeInTheDocument();
  await settledStatus(result);
  expect(screen.queryByRole("alert")).not.toBeInTheDocument();
});
