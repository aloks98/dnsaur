import { http, HttpResponse } from "msw";
import { expect, test } from "vitest";
import { Route, Routes } from "react-router";
import { screen } from "@testing-library/react";
import { server } from "../test/msw-server";
import { renderWithProviders } from "../test/render";
import { AppShell } from "./app-shell";
import { SettingsPage } from "../pages/settings";

// The encryption-downgrade warning is a fact about the running server — the
// stored `upstreams` asked for encryption and could not get it, so every
// query is going out in the clear — not a fact about any one screen. It has
// to follow the operator everywhere, the same way an "API unreachable"
// banner would, or someone who lands on Query Log or Zones never learns.
// Mounted once, in the shell, rather than duplicated per page.

function mockDowngraded(reason = 'upstream "tls://1.1.1.1:853": missing "#name"') {
  server.use(
    http.get("/api/v1/resolver/status", () =>
      HttpResponse.json({ encryption_downgraded: true, reason }),
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
  renderWithProviders(
    <Routes>
      <Route element={<AppShell />}>
        <Route index element={<h1>Some other page</h1>} />
      </Route>
    </Routes>,
  );

  expect(screen.getByText("Some other page")).toBeInTheDocument();
  // There's no fetch-backed element in this tree to key a findBy* off (the
  // dummy route renders synchronously), so give the resolver-status query —
  // the only async work here, mocked with no artificial delay — a beat to
  // settle before asserting on its absence.
  await new Promise((resolve) => setTimeout(resolve, 50));
  expect(screen.queryByRole("alert")).not.toBeInTheDocument();
});
