import { delay, http, HttpResponse } from "msw";
import { toast } from "sonner";
import { expect, test, vi } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { server } from "../test/msw-server";
import { renderWithProviders } from "../test/render";
import type { Settings } from "../api/types";
import { SettingsPage } from "./settings";

// A full, server-valid snapshot — every key in
// internal/api/settings_handlers.go's editableSettings, matching
// docs/configuration.md's defaults. Individual tests override specific
// keys via server.use() to exercise a particular field.
function fullSettings(overrides: Partial<Settings> = {}): Settings {
  return {
    upstreams: "1.1.1.1:53,1.0.0.1:53,9.9.9.9:53",
    "upstream.strategy": "race",
    "blocking.mode": "null-ip",
    "blocking.ttl": "30",
    "cache.min_ttl": "0",
    "cache.max_ttl": "86400",
    "cache.max_entries": "10000",
    "cache.serve_stale_for": "86400",
    "lists.refresh_hours": "24",
    "qlog.retention_days": "90",
    "qlog.privacy": "full",
    ...overrides,
  };
}

function mockSettings(settings: Settings) {
  server.use(http.get("/api/v1/settings", () => HttpResponse.json(settings)));
}

test("renders each grouped Card section with the current values populated", async () => {
  mockSettings(fullSettings());

  renderWithProviders(<SettingsPage />);

  expect(await screen.findByText("Upstreams")).toBeInTheDocument();
  expect(screen.getByText("Blocking")).toBeInTheDocument();
  expect(screen.getByText("Cache")).toBeInTheDocument();
  expect(screen.getByText("Query log")).toBeInTheDocument();
  expect(screen.getByText("Lists")).toBeInTheDocument();

  expect(screen.getByLabelText(/^upstream resolvers$/i)).toHaveValue(
    "1.1.1.1:53,1.0.0.1:53,9.9.9.9:53",
  );
  expect(screen.getByLabelText(/^blocked response ttl/i)).toHaveValue(30);
  expect(screen.getByRole("combobox", { name: /^blocking mode$/i })).toHaveTextContent(/null ip/i);

  // A Save affordance at both the top and bottom of the (long) page, both
  // disabled until something changes.
  const saveButtons = screen.getAllByRole("button", { name: /^save changes$/i });
  expect(saveButtons).toHaveLength(2);
  for (const button of saveButtons) expect(button).toBeDisabled();
});

test("shows a loading skeleton, then the form, once settings arrive", async () => {
  server.use(
    http.get("/api/v1/settings", async () => {
      await delay(30);
      return HttpResponse.json(fullSettings());
    }),
  );

  renderWithProviders(<SettingsPage />);

  expect(screen.queryByText("Upstreams")).not.toBeInTheDocument();
  expect(await screen.findByText("Upstreams")).toBeInTheDocument();
});

test("shows an alert when settings fail to load", async () => {
  server.use(
    http.get("/api/v1/settings", () => HttpResponse.json({ error: "boom" }, { status: 500 })),
  );

  renderWithProviders(<SettingsPage />);

  // The query client retries once by default (see lib/query-client.ts),
  // so the error state lands after that retry's backoff delay — past
  // findBy's default 1000ms timeout.
  expect(
    await screen.findByText(/couldn't load settings/i, undefined, { timeout: 3000 }),
  ).toBeInTheDocument();
});

// Required test (a): an invalid blocking.mode value is blocked
// client-side, no PUT. The Select can't itself produce an invalid value
// (only null-ip/nxdomain are ever offered) — the realistic way an invalid
// value ends up in the form is pre-existing bad data from the server
// (e.g. set by a future/older dnsaur version). react-hook-form's
// handleSubmit validates every registered field on submit, not just the
// one the admin touched, so editing an unrelated field (upstreams) and
// saving must still be blocked by blocking.mode's bad value — and nothing
// PUTs, not even the field that was actually edited.
test("a pre-existing invalid blocking.mode value blocks the whole save, and nothing PUTs", async () => {
  const user = userEvent.setup();
  let putCalled = false;
  mockSettings(fullSettings({ "blocking.mode": "bogus-mode" }));
  server.use(
    http.put("/api/v1/settings", () => {
      putCalled = true;
      return new HttpResponse(null, { status: 204 });
    }),
  );

  renderWithProviders(<SettingsPage />);
  await screen.findByText("Upstreams");

  const upstreamsInput = screen.getByLabelText(/^upstream resolvers$/i);
  await user.type(upstreamsInput, ",8.8.8.8:53");
  await user.click(screen.getAllByRole("button", { name: /^save changes$/i })[0]);

  expect(await screen.findByText(/must be one of: null-ip, nxdomain/i)).toBeInTheDocument();
  // Give any accidental async PUT a chance to land before asserting it didn't.
  await new Promise((resolve) => setTimeout(resolve, 30));
  expect(putCalled).toBe(false);
});

// Required test (b): a valid change PUTs {key,value} and toasts.
test("changing one field PUTs only that key and toasts success", async () => {
  const user = userEvent.setup();
  let requestBody: unknown;
  let putCount = 0;
  mockSettings(fullSettings());
  server.use(
    http.put("/api/v1/settings", async ({ request }) => {
      putCount += 1;
      requestBody = await request.json();
      return new HttpResponse(null, { status: 204 });
    }),
  );
  const successSpy = vi.spyOn(toast, "success");

  renderWithProviders(<SettingsPage />);
  await screen.findByText("Upstreams");

  const ttlInput = screen.getByLabelText(/^blocked response ttl/i);
  await user.clear(ttlInput);
  await user.type(ttlInput, "45");
  await user.click(screen.getAllByRole("button", { name: /^save changes$/i })[0]);

  await waitFor(() => expect(requestBody).toEqual({ key: "blocking.ttl", value: "45" }));
  expect(putCount).toBe(1);
  await waitFor(() => expect(successSpy).toHaveBeenCalledWith("1 setting updated"));
});

// Required test (c): restart-required fields show the label.
test("cache.* and lists.refresh_hours show a Restart required label; nothing else does", async () => {
  mockSettings(fullSettings());

  renderWithProviders(<SettingsPage />);
  await screen.findByText("Upstreams");

  expect(screen.getAllByText(/restart required/i)).toHaveLength(5);
});

// Extra coverage: the same nonNegInt allowlist rule applies to every
// integer setting, not just the one exercised above — a negative cache
// value must be caught client-side too.
test("a negative cache value is blocked client-side, no PUT", async () => {
  const user = userEvent.setup();
  let putCalled = false;
  mockSettings(fullSettings());
  server.use(
    http.put("/api/v1/settings", () => {
      putCalled = true;
      return new HttpResponse(null, { status: 204 });
    }),
  );

  renderWithProviders(<SettingsPage />);
  await screen.findByText("Upstreams");

  const minTtlInput = screen.getByLabelText(/^minimum cache ttl/i);
  await user.clear(minTtlInput);
  await user.type(minTtlInput, "-5");
  await user.click(screen.getAllByRole("button", { name: /^save changes$/i })[0]);

  expect(await screen.findByText(/must be zero or greater/i)).toBeInTheDocument();
  await new Promise((resolve) => setTimeout(resolve, 30));
  expect(putCalled).toBe(false);
});

// A failed PUT (server 400s the value for a reason the client didn't
// anticipate) surfaces via toast, mirroring dns.tsx's own
// server-rejection-surfaces-as-a-toast convention, and keeps the admin's
// attempted value in the field so they can see and correct it.
test("a rejected PUT surfaces the server's error as a toast and keeps the field editable", async () => {
  const user = userEvent.setup();
  mockSettings(fullSettings());
  server.use(
    http.put("/api/v1/settings", () =>
      HttpResponse.json({ error: "invalid value for blocking.ttl" }, { status: 400 }),
    ),
  );
  const errorSpy = vi.spyOn(toast, "error");

  renderWithProviders(<SettingsPage />);
  await screen.findByText("Upstreams");

  const ttlInput = screen.getByLabelText(/^blocked response ttl/i);
  await user.clear(ttlInput);
  await user.type(ttlInput, "999999999999");
  await user.click(screen.getAllByRole("button", { name: /^save changes$/i })[0]);

  await waitFor(() =>
    expect(errorSpy).toHaveBeenCalledWith(
      expect.stringMatching(/invalid value for blocking\.ttl/i),
    ),
  );
  expect(ttlInput).toHaveValue(999999999999);
});
