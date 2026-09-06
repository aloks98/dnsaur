import { delay, http, HttpResponse } from "msw";
import { toast } from "sonner";
import { expect, test, vi } from "vitest";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
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

  // The upstreams field is the structured editor now (see
  // upstreams-field.test.tsx for its own coverage): Plain is selected and
  // each comma-separated address shows in its own row.
  expect(screen.getByRole("radio", { name: /^plain/i })).toBeChecked();
  expect(screen.getAllByLabelText("Address").map((el) => (el as HTMLInputElement).value)).toEqual([
    "1.1.1.1:53",
    "1.0.0.1:53",
    "9.9.9.9:53",
  ]);
  // inputMode, not type="number": the value is a string on the wire and a
  // spinner buys nothing on a seconds field.
  expect(screen.getByLabelText(/^blocked response ttl/i)).toHaveValue("30");

  // One-of choices are radio lists now, so every option is readable without
  // opening anything.
  expect(screen.getByRole("radio", { name: /null-ip/i })).toBeChecked();
  expect(screen.getByRole("radio", { name: /nxdomain/i })).not.toBeChecked();

  // One save bar, pinned above the scrolling sections — the second copy at
  // the bottom of the page existed because the old layout scrolled the
  // whole page, taking the first one out of reach.
  const save = screen.getByRole("button", { name: /^save changes$/i });
  expect(save).toBeDisabled();
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
// one the admin touched, so editing an unrelated field (retention days) and
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

  const retentionInput = screen.getByLabelText(/^retention \(days\)$/i);
  await user.clear(retentionInput);
  await user.type(retentionInput, "45");
  await user.click(screen.getAllByRole("button", { name: /^save changes$/i })[0]);

  expect(await screen.findByText(/pick one of the options/i)).toBeInTheDocument();
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

// The FormItem wrapping one labelled field — the badge, label, control,
// description and message for that setting and nothing else. Asserting
// inside it is what makes the restart-required test below about the
// *mapping* rather than about a count: an admin told a field hot-reloads
// when it doesn't (or vice versa) is the actual operational bug, and a
// bare `toHaveLength(5)` stays green if the badge moves between fields.
function fieldItem(label: string): HTMLElement {
  const item = screen.getByText(label).closest('[data-slot="form-item"]');
  if (!(item instanceof HTMLElement)) throw new Error(`no FormItem found for "${label}"`);
  return item;
}

// Required test (c): restart-required fields show the label — and only
// those fields do. The labelling is verified against internal/app/app.go:
// applySettings re-reads blocking.*, upstreams, upstream.strategy,
// qlog.privacy, clients and records on every settings write, while cache.*
// is read once when Start() builds the cache and lists.refresh_hours once
// when the refresh ticker is scheduled.
test("every section says whether it applies on save or waits for a restart", async () => {
  mockSettings(fullSettings());

  renderWithProviders(<SettingsPage />);
  await screen.findByText("Upstreams");

  // The badge belongs to the section, not the field: cache.* is read once
  // when Start() builds the cache and lists.refresh_hours once when the
  // refresh ticker is scheduled, so those two groups are restart-only in
  // their entirety. Everything else is re-read on every settings write.
  const restartSections = ["Cache", "Lists"];
  const instantSections = ["Upstreams", "Blocking", "Query log"];

  for (const title of restartSections) {
    const section = screen.getByRole("heading", { name: title }).closest("section")!;
    expect(within(section).getByText(/needs a restart/i)).toBeInTheDocument();
    expect(within(section).queryByText(/applies instantly/i)).not.toBeInTheDocument();
  }
  for (const title of instantSections) {
    const section = screen.getByRole("heading", { name: title }).closest("section")!;
    expect(within(section).getByText(/applies instantly/i)).toBeInTheDocument();
    expect(within(section).queryByText(/needs a restart/i)).not.toBeInTheDocument();
  }

  // Belt and braces: every group is labelled, and none twice.
  expect(screen.getAllByText(/needs a restart/i)).toHaveLength(restartSections.length);
  expect(screen.getAllByText(/applies instantly/i)).toHaveLength(instantSections.length);
});

// The upstream strategy description used to claim "Only Race is
// implemented today". internal/upstream/forwarder.go implements all three
// (race, failover, and fastest, the last sorting by EWMA latency), and
// internal/app/app.go's applySettings rebuilds the forwarder with the new
// strategy on every settings write — so the caveat was doubly wrong. UI
// copy that talks an admin out of a working feature is worth pinning.
test("the upstream strategy field doesn't claim the other strategies are unimplemented", async () => {
  mockSettings(fullSettings());

  renderWithProviders(<SettingsPage />);
  await screen.findByText("Upstreams");

  const item = fieldItem("Upstream strategy");
  expect(within(item).queryByText(/only race/i)).not.toBeInTheDocument();
  expect(within(item).queryByText(/not implemented|unimplemented/i)).not.toBeInTheDocument();
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

  expect(await screen.findByText(/can.t be negative/i)).toBeInTheDocument();
  await new Promise((resolve) => setTimeout(resolve, 30));
  expect(putCalled).toBe(false);
});

// A failed PUT (server 400s the value for a reason the client didn't
// anticipate) surfaces via toast — the same server-rejection-surfaces-as-a-
// toast convention used throughout the app — and keeps the admin's
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
  expect(ttlInput).toHaveValue("999999999999");
});

// The worst case of the "isError swaps out already-loaded content" family,
// and the reason this page's guard is `data === undefined` rather than
// `isError`. Every successful PUT invalidates the settings query, which
// refetches immediately; query-core sets status:"error" if that refetch
// fails even though `data` is still there. Gating the form on isSuccess
// would unmount SettingsForm at exactly that moment — taking
// react-hook-form's state and defaultsRef with it — and a partial save is
// precisely when the admin still has an unsaved value in the box.
test("a failing background refetch after a partial save keeps the form and its dirty value", async () => {
  const user = userEvent.setup();
  let getCount = 0;
  server.use(
    // First load succeeds; every refetch afterwards 500s.
    http.get("/api/v1/settings", () => {
      getCount += 1;
      return getCount === 1
        ? HttpResponse.json(fullSettings())
        : HttpResponse.json({ error: "boom" }, { status: 500 });
    }),
    // blocking.ttl saves; upstreams is rejected, so it stays dirty.
    http.put("/api/v1/settings", async ({ request }) => {
      const body = (await request.json()) as { key: string };
      return body.key === "upstreams"
        ? HttpResponse.json({ error: "nope" }, { status: 400 })
        : new HttpResponse(null, { status: 204 });
    }),
  );
  const errorSpy = vi.spyOn(toast, "error");

  renderWithProviders(<SettingsPage />);
  await screen.findByText("Upstreams");

  // Whittle the three default rows down to one, then retype its address —
  // ending at the same "8.8.8.8:53" the old free-text version of this test
  // typed directly.
  let removeButtons = screen.getAllByRole("button", { name: /remove/i });
  fireEvent.click(removeButtons[2]);
  removeButtons = screen.getAllByRole("button", { name: /remove/i });
  fireEvent.click(removeButtons[1]);
  fireEvent.change(screen.getByLabelText("Address"), { target: { value: "8.8.8.8:53" } });

  const ttlInput = screen.getByLabelText(/^blocked response ttl/i);
  await user.clear(ttlInput);
  await user.type(ttlInput, "45");

  await user.click(screen.getAllByRole("button", { name: /^save changes$/i })[0]);

  await waitFor(() =>
    expect(errorSpy).toHaveBeenCalledWith(expect.stringMatching(/couldn't save upstreams/i)),
  );

  // The refetch the successful PUT triggered has now failed (retry: 1, then
  // error). The form must still be here, with the rejected value intact and
  // still dirty — and the destructive "couldn't load settings" card, which
  // would have replaced it, must not be.
  await screen.findByText(/couldn't refresh settings/i, undefined, { timeout: 3000 });
  expect(screen.queryByText(/couldn't load settings/i)).not.toBeInTheDocument();
  expect(screen.getByLabelText("Address")).toHaveValue("8.8.8.8:53");
  // One save bar, and it still counts the one field that failed.
  expect(screen.getByText(/1 unsaved change/i)).toBeInTheDocument();
  expect(screen.getByRole("button", { name: /^save changes$/i })).toBeEnabled();
  // The value that *did* save moved its baseline, so it's no longer dirty
  // but still shows what the admin typed.
  expect(screen.getByLabelText(/^blocked response ttl/i)).toHaveValue("45");
});

// The encryption-downgrade warning used to be mounted directly on this page;
// it now lives in the app shell (see components/app-shell.test.tsx and
// components/encryption-downgrade-banner.tsx) because it is a fact about the
// running server, not about Settings specifically. SettingsPage rendered in
// isolation — as every other test in this file does — no longer mounts any
// banner at all, so there is nothing left to pin here.
