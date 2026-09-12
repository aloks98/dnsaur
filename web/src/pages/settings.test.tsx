import { delay, http, HttpResponse } from "msw";
import { toast } from "sonner";
import { expect, test, vi } from "vitest";
import { act, fireEvent, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { Link, Route, Routes } from "react-router";
import { server } from "../test/msw-server";
import { replicaHandlers } from "../test/msw-handlers";
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
    "stats.retention_days": "365",
    "serve.dot.enabled": "false",
    "serve.dot.listen": ":853",
    "serve.doh.enabled": "false",
    "serve.doh.listen": ":443",
    "serve.tls.cert": "",
    "serve.tls.key": "",
    // sync.token is deliberately absent: GET /settings never returns it.
    "sync.peer_url": "",
    "sync.interval_seconds": "30",
    "sync.primary_dns": "",
    "sync.tsig_key_id": "0",
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
  expect(screen.getByText("Protocols")).toBeInTheDocument();

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

// stats.retention_days sits beside the query log's own retention, which is
// a different number for a different table: the log is per-query rows, this
// is the hourly totals the dashboard reads. Two fields with the same label
// would be unusable, so the labels differ and each has to save its own key.
test("statistics retention shows its stored value and PUTs its own key", async () => {
  const user = userEvent.setup();
  let requestBody: unknown;
  mockSettings(fullSettings());
  server.use(
    http.put("/api/v1/settings", async ({ request }) => {
      requestBody = await request.json();
      return new HttpResponse(null, { status: 204 });
    }),
  );

  renderWithProviders(<SettingsPage />);
  await screen.findByText("Upstreams");

  expect(screen.getByLabelText(/^retention \(days\)$/i)).toHaveValue("90");
  const statsInput = screen.getByLabelText(/^statistics retention \(days\)$/i);
  expect(statsInput).toHaveValue("365");
  await user.clear(statsInput);
  await user.type(statsInput, "30");
  await user.click(screen.getAllByRole("button", { name: /^save changes$/i })[0]);

  await waitFor(() => expect(requestBody).toEqual({ key: "stats.retention_days", value: "30" }));
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
  // their entirety. Everything else is re-read on every settings write —
  // Protocols included: reconcileServing (internal/app/serve.go) applies
  // every serve.* write live, on the same write that changed it.
  const restartSections = ["Cache", "Lists"];
  // Sync is instant for the same reason: the pull loop re-reads its peer,
  // interval and primary address on every pass
  // (internal/confsync/replica.go).
  const instantSections = ["Upstreams", "Blocking", "Query log", "Protocols", "Sync"];

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

// --- multi-key saves: the ordering PUT /settings actually requires --------
//
// `PUT /api/v1/settings` is one key per request, and its cross-field
// validation (internal/api/settings_handlers.go's validateCrossField)
// re-reads the store on every one of them. So a save that changes several
// dependent keys is only correct if it sends them in dependency order —
// which is a property of the *interaction* between the form and the API,
// and is exactly what a handler that answers 204 to everything cannot
// test. This handler mirrors validateCrossField instead.

/** An msw PUT handler holding a settings store and applying the same
 * cross-field rules the Go handler does, so a request that would 400 in
 * production 400s here. `certPairs` says which cert/key pairs load; any
 * other complete pair is a mismatch, the way tls.LoadX509KeyPair would
 * report it.
 *
 * The snapshot-then-await-then-write shape is the point, not incidental.
 * handleSettingsPut reads the whole settings map, validates against what it
 * read, and only then writes — so two requests in flight at once both judge
 * the state as it was before either landed. Validating against the live
 * object here would make this handler serialise where the real server does
 * not, and every concurrency bug in the caller would pass. */
function settingsPutMirror(
  stored: Record<string, string>,
  certPairs: [string, string][],
  seen: string[],
) {
  const loads = (cert: string, key: string) => certPairs.some(([c, k]) => c === cert && k === key);
  return http.put("/api/v1/settings", async ({ request }) => {
    const { key, value } = (await request.json()) as { key: string; value: string };
    seen.push(key);
    const current = { ...stored };
    await delay(5);
    const reject = (message: string) =>
      HttpResponse.json({ error: `invalid value for ${key}: ${message}` }, { status: 400 });

    if (key === "serve.dot.enabled" || key === "serve.doh.enabled") {
      if (value === "true") {
        const cert = current["serve.tls.cert"] ?? "";
        const certKey = current["serve.tls.key"] ?? "";
        if (cert === "" || certKey === "") {
          return reject("set serve.tls.cert and serve.tls.key first");
        }
        if (!loads(cert, certKey)) return reject("tls: private key does not match public key");
      }
    }
    if (key === "serve.tls.cert" || key === "serve.tls.key") {
      const cert = key === "serve.tls.cert" ? value : (current["serve.tls.cert"] ?? "");
      const certKey = key === "serve.tls.key" ? value : (current["serve.tls.key"] ?? "");
      if (cert === "" || certKey === "") {
        if (current["serve.dot.enabled"] === "true" || current["serve.doh.enabled"] === "true") {
          return reject("turn DNS-over-TLS and DNS-over-HTTPS off before clearing the certificate");
        }
      } else if (!loads(cert, certKey)) {
        return reject("tls: private key does not match public key");
      }
    }
    stored[key] = value;
    return new HttpResponse(null, { status: 204 });
  });
}

const CERT = "/etc/ssl/dnsaur/fullchain.pem";
const KEY = "/etc/ssl/dnsaur/privkey.pem";
const OTHER_KEY = "/etc/ssl/other/privkey.pem";

// The first thing every operator does on a fresh install: type both
// certificate paths, tick DNS-over-TLS, press Save. All three keys go in
// one submit, and the enable is only valid once both paths are stored.
test("enabling a protocol and setting its certificate saves in one press", async () => {
  const user = userEvent.setup();
  const stored: Record<string, string> = { ...fullSettings() };
  const seen: string[] = [];
  mockSettings(fullSettings());
  server.use(settingsPutMirror(stored, [[CERT, KEY]], seen));
  const successSpy = vi.spyOn(toast, "success");
  const errorSpy = vi.spyOn(toast, "error");
  // Cleared, not merely spied: vi.spyOn hands back the same mock when an
  // earlier test in this file already spied on the same method, history
  // included — so a waitFor on "was it called with X" that an earlier test
  // already satisfied would resolve before this test has done anything.
  successSpy.mockClear();
  errorSpy.mockClear();

  renderWithProviders(<SettingsPage />);
  await screen.findByText("Protocols");

  await user.type(screen.getByLabelText("Certificate"), CERT);
  await user.type(screen.getByLabelText("Private key"), KEY);
  await user.click(screen.getByRole("checkbox", { name: /DNS-over-TLS/ }));
  await user.click(screen.getAllByRole("button", { name: /^save changes$/i })[0]);

  await waitFor(() => expect(successSpy).toHaveBeenCalledWith("3 settings updated"));
  expect(errorSpy).not.toHaveBeenCalled();
  expect(stored["serve.tls.cert"]).toBe(CERT);
  expect(stored["serve.tls.key"]).toBe(KEY);
  expect(stored["serve.dot.enabled"]).toBe("true");
  // The enable must be dispatched after both paths, not merely succeed.
  expect(seen.indexOf("serve.dot.enabled")).toBeGreaterThan(seen.indexOf("serve.tls.cert"));
  expect(seen.indexOf("serve.dot.enabled")).toBeGreaterThan(seen.indexOf("serve.tls.key"));
});

// The same root cause's second face. Both certificate paths in one save,
// and they do not form a loadable pair: the server only runs the pair check
// when a request can see both halves, so writing them concurrently means
// nobody checks and a broken pair stores with a 204.
test("a mismatched certificate pair saved in one press is rejected, not stored", async () => {
  const user = userEvent.setup();
  const stored: Record<string, string> = { ...fullSettings() };
  const seen: string[] = [];
  mockSettings(fullSettings());
  server.use(settingsPutMirror(stored, [[CERT, KEY]], seen));
  const errorSpy = vi.spyOn(toast, "error");
  // Cleared, not merely spied: vi.spyOn hands back the same mock when a
  // previous test in this file already spied on toast.error, history
  // included.
  errorSpy.mockClear();

  renderWithProviders(<SettingsPage />);
  await screen.findByText("Protocols");

  await user.type(screen.getByLabelText("Certificate"), CERT);
  await user.type(screen.getByLabelText("Private key"), OTHER_KEY);
  await user.click(screen.getAllByRole("button", { name: /^save changes$/i })[0]);

  await waitFor(() => expect(errorSpy).toHaveBeenCalled());
  expect(stored["serve.tls.key"]).not.toBe(OTHER_KEY);
  // And the reason reaches the certificate line, where "did I type the
  // right path" is answered.
  expect(await screen.findByText(/private key does not match public key/i)).toBeInTheDocument();
});

// Turning a protocol off and clearing its certificate in the same save.
// The disable has to land first: clearing a path under a live protocol is
// refused (spec §6), so the reverse order fails on a save that is entirely
// coherent as a whole.
test("turning a protocol off and clearing its certificate saves in one press", async () => {
  const user = userEvent.setup();
  const initial = fullSettings({
    "serve.dot.enabled": "true",
    "serve.tls.cert": CERT,
    "serve.tls.key": KEY,
  });
  const stored: Record<string, string> = { ...initial };
  const seen: string[] = [];
  mockSettings(initial);
  server.use(settingsPutMirror(stored, [[CERT, KEY]], seen));
  const successSpy = vi.spyOn(toast, "success");
  const errorSpy = vi.spyOn(toast, "error");
  successSpy.mockClear();
  errorSpy.mockClear();

  renderWithProviders(<SettingsPage />);
  await screen.findByText("Protocols");

  await user.click(screen.getByRole("checkbox", { name: /DNS-over-TLS/ }));
  await user.clear(screen.getByLabelText("Certificate"));
  await user.clear(screen.getByLabelText("Private key"));
  await user.click(screen.getAllByRole("button", { name: /^save changes$/i })[0]);

  await waitFor(() => expect(successSpy).toHaveBeenCalledWith("3 settings updated"));
  expect(errorSpy).not.toHaveBeenCalled();
  expect(stored["serve.dot.enabled"]).toBe("false");
  expect(stored["serve.tls.cert"]).toBe("");
  expect(seen.indexOf("serve.dot.enabled")).toBeLessThan(seen.indexOf("serve.tls.cert"));
});

// --- I2: the status poll has to cover what the reconcile is about to do ---
//
// The reconcile that makes a serve.* write real runs asynchronously off the
// settings watcher (internal/app/serve.go, driven from app.go), so the
// single refetch useUpdateSetting triggers routinely lands before it has
// run. With no interval and refetchOnWindowFocus off, nothing corrected it
// afterwards: the line under a freshly ticked box read "○ off" until the
// operator navigated away and back.
test("saving a serve.* setting keeps the status polling until the reconcile lands", async () => {
  const user = userEvent.setup();
  let statusReads = 0;
  // Reality lags intent, exactly as it does on the server: the reconcile
  // has not run when the first refetch after the save arrives.
  let reconciled = false;
  mockSettings(fullSettings());
  server.use(
    http.get("/api/v1/resolver/status", () => {
      statusReads += 1;
      return HttpResponse.json({
        encryption_downgraded: false,
        reason: "",
        serving: {
          dot: reconciled
            ? { enabled: true, listening: true, addr: ":853" }
            : { enabled: false, listening: false, addr: "" },
          doh: { enabled: false, listening: false, addr: "" },
        },
      });
    }),
    http.put("/api/v1/settings", () => new HttpResponse(null, { status: 204 })),
  );

  renderWithProviders(<SettingsPage />);
  await screen.findByText("Protocols");
  await waitFor(() => expect(statusReads).toBeGreaterThan(0));

  const listen = screen.getByLabelText("DNS-over-TLS listen address");
  await user.clear(listen);
  await user.type(listen, ":8853");
  await user.click(screen.getAllByRole("button", { name: /^save changes$/i })[0]);

  // The save's own invalidation accounts for one extra read. Anything
  // beyond that is the settle-window poll, which is the whole point: at
  // this moment the status says nothing is wrong, and it is about to stop
  // being true.
  const afterSave = statusReads;
  reconciled = true;
  await waitFor(() => expect(statusReads).toBeGreaterThan(afterSave + 1), { timeout: 4000 });
  expect(
    await within(screen.getByRole("group", { name: "DNS-over-TLS" })).findByText(
      "listening on :853",
    ),
  ).toBeInTheDocument();
});

// --- the dirty count, the baseline, and leaving with unsaved work ----------

// The bar counted raw watched values while the submit diffed zod-trimmed
// ones, so a trailing space read as "1 unsaved change" and Save then found
// nothing to send and returned in silence.
test("a whitespace-only edit is not counted as a change", async () => {
  const user = userEvent.setup();
  mockSettings(fullSettings());
  let puts = 0;
  server.use(
    http.put("/api/v1/settings", () => {
      puts += 1;
      return new HttpResponse(null, { status: 204 });
    }),
  );

  renderWithProviders(<SettingsPage />);
  await screen.findByText("Upstreams");

  await user.type(screen.getByLabelText(/^blocked response ttl/i), " ");

  expect(screen.getByText("All changes saved")).toBeInTheDocument();
  expect(screen.getAllByRole("button", { name: /^save changes$/i })[0]).toBeDisabled();
  expect(puts).toBe(0);
});

// The baseline was seeded once and never moved, so a refetch carrying
// another tab's save was read and discarded: this tab kept showing the old
// value, called itself saved, and could not even write the old value back.
test("a refetch with a new value moves a clean field and leaves a dirty one alone", async () => {
  const user = userEvent.setup();
  let current = fullSettings();
  server.use(http.get("/api/v1/settings", () => HttpResponse.json(current)));

  const { queryClient } = renderWithProviders(<SettingsPage />);
  await screen.findByText("Upstreams");

  const ttl = screen.getByLabelText(/^blocked response ttl/i);
  await user.clear(ttl);
  await user.type(ttl, "45");
  expect(screen.getByText("1 unsaved change")).toBeInTheDocument();

  // Another tab saves two keys: one this form has not touched, one it has.
  current = fullSettings({ "qlog.retention_days": "30", "blocking.ttl": "90" });
  await act(async () => {
    await queryClient.invalidateQueries({ queryKey: ["settings"] });
  });

  // The untouched field takes the server's value…
  await waitFor(() => expect(screen.getByLabelText(/^retention \(days\)/i)).toHaveValue("30"));
  // …and the one being edited keeps what was typed, still unsaved.
  expect(ttl).toHaveValue("45");
  expect(screen.getByText("1 unsaved change")).toBeInTheDocument();
});

// A field the server moved underneath a clean form has to become saveable
// again: with the baseline stuck on the old value, editing it back to what
// the server now holds produced an empty diff and a Save that did nothing.
test("a clean field the server moved is saved back from its new value", async () => {
  const user = userEvent.setup();
  let current = fullSettings();
  const puts: { key: string; value: string }[] = [];
  server.use(
    http.get("/api/v1/settings", () => HttpResponse.json(current)),
    http.put("/api/v1/settings", async ({ request }) => {
      puts.push((await request.json()) as { key: string; value: string });
      return new HttpResponse(null, { status: 204 });
    }),
  );

  const { queryClient } = renderWithProviders(<SettingsPage />);
  await screen.findByText("Upstreams");

  current = fullSettings({ "qlog.retention_days": "30" });
  await act(async () => {
    await queryClient.invalidateQueries({ queryKey: ["settings"] });
  });
  const retention = await screen.findByLabelText(/^retention \(days\)/i);
  await waitFor(() => expect(retention).toHaveValue("30"));

  await user.clear(retention);
  await user.type(retention, "60");
  await user.click(screen.getAllByRole("button", { name: /^save changes$/i })[0]);

  await waitFor(() => expect(puts).toEqual([{ key: "qlog.retention_days", value: "60" }]));
});

/** Settings under a route, with somewhere else to navigate to — the blocker
 * only has something to block when a navigation is actually attempted. */
function renderSettingsWithNav() {
  return renderWithProviders(
    <>
      <Link to="/zones">Zones</Link>
      <Routes>
        <Route path="/settings" element={<SettingsPage />} />
        <Route path="/zones" element={<p>the zones page</p>} />
      </Routes>
    </>,
    { route: "/settings" },
  );
}

// Three changed settings and a click on any nav item used to discard all
// three without a word.
test("leaving with unsaved changes asks first, and Stay keeps them", async () => {
  const user = userEvent.setup();
  mockSettings(fullSettings());

  renderSettingsWithNav();
  await screen.findByText("Upstreams");

  const ttl = screen.getByLabelText(/^blocked response ttl/i);
  await user.clear(ttl);
  await user.type(ttl, "45");

  await user.click(screen.getByRole("link", { name: "Zones" }));

  expect(await screen.findByText("Leave without saving?")).toBeInTheDocument();
  await user.click(screen.getByRole("button", { name: /^stay$/i }));

  await waitFor(() => expect(screen.queryByText("Leave without saving?")).not.toBeInTheDocument());
  expect(screen.queryByText("the zones page")).not.toBeInTheDocument();
  expect(screen.getByLabelText(/^blocked response ttl/i)).toHaveValue("45");
});

test("Leave goes on to the page that was asked for", async () => {
  const user = userEvent.setup();
  mockSettings(fullSettings());

  renderSettingsWithNav();
  await screen.findByText("Upstreams");

  const ttl = screen.getByLabelText(/^blocked response ttl/i);
  await user.clear(ttl);
  await user.type(ttl, "45");

  await user.click(screen.getByRole("link", { name: "Zones" }));
  await screen.findByText("Leave without saving?");
  await user.click(screen.getByRole("button", { name: /^leave$/i }));

  expect(await screen.findByText("the zones page")).toBeInTheDocument();
});

test("a form with nothing unsaved navigates without asking", async () => {
  const user = userEvent.setup();
  mockSettings(fullSettings());

  renderSettingsWithNav();
  await screen.findByText("Upstreams");

  await user.click(screen.getByRole("link", { name: "Zones" }));

  expect(await screen.findByText("the zones page")).toBeInTheDocument();
  expect(screen.queryByText("Leave without saving?")).not.toBeInTheDocument();
});

// The two certificate phases are ordered so the second request is the one
// that sees a complete pair — which is also what makes it the one that can
// fail on its own, with the first already written.
test("a cert path that saves and a key path that is rejected keeps only the key dirty", async () => {
  const user = userEvent.setup();
  mockSettings(fullSettings());
  const errorSpy = vi.spyOn(toast, "error");
  server.use(
    http.put("/api/v1/settings", async ({ request }) => {
      const body = (await request.json()) as { key: string };
      return body.key === "serve.tls.key"
        ? HttpResponse.json(
            { error: "tls: private key does not match public key" },
            { status: 400 },
          )
        : new HttpResponse(null, { status: 204 });
    }),
  );

  renderWithProviders(<SettingsPage />);
  await screen.findByText("Protocols");

  await user.type(screen.getByLabelText("Certificate"), "/etc/dnsaur/tls.crt");
  await user.type(screen.getByLabelText("Private key"), "/etc/dnsaur/other.key");
  await user.click(screen.getAllByRole("button", { name: /^save changes$/i })[0]);

  await waitFor(() =>
    expect(errorSpy).toHaveBeenCalledWith(expect.stringMatching(/serve\.tls\.key/)),
  );
  // The rejected half stays dirty and retryable; the half that landed does
  // not, so the bar counts one rather than two.
  expect(await screen.findByText("1 unsaved change")).toBeInTheDocument();
  expect(screen.getByLabelText("Private key")).toHaveValue("/etc/dnsaur/other.key");
  expect(screen.getByText("tls: private key does not match public key")).toBeInTheDocument();
});

// Discard resets to the baseline, and after a partial save the baseline is
// no longer what was loaded: the keys that landed have moved to their new
// values and must not be rolled back with the one that failed.
test("Discard after a partial failure keeps what was saved", async () => {
  const user = userEvent.setup();
  mockSettings(fullSettings());
  server.use(
    http.put("/api/v1/settings", async ({ request }) => {
      const body = (await request.json()) as { key: string };
      return body.key === "qlog.retention_days"
        ? HttpResponse.json({ error: "nope" }, { status: 400 })
        : new HttpResponse(null, { status: 204 });
    }),
  );
  vi.spyOn(toast, "error");

  renderWithProviders(<SettingsPage />);
  await screen.findByText("Upstreams");

  const ttl = screen.getByLabelText(/^blocked response ttl/i);
  await user.clear(ttl);
  await user.type(ttl, "45");
  const retention = screen.getByLabelText(/^retention \(days\)/i);
  await user.clear(retention);
  await user.type(retention, "7");
  await user.click(screen.getAllByRole("button", { name: /^save changes$/i })[0]);

  expect(await screen.findByText("1 unsaved change")).toBeInTheDocument();

  await user.click(screen.getAllByRole("button", { name: /^discard$/i })[0]);

  expect(screen.getByText("All changes saved")).toBeInTheDocument();
  // The blocked TTL landed, so Discard leaves it at 45 rather than putting
  // the loaded 30 back.
  expect(screen.getByLabelText(/^blocked response ttl/i)).toHaveValue("45");
  expect(screen.getByLabelText(/^retention \(days\)/i)).toHaveValue("90");
});

// The backup band is the one thing on this page that acts instead of saving,
// and the path is the whole answer: the file is on the server's disk and
// copying it off is the operator's next move.
test("Back up now writes a backup and says where it went", async () => {
  const user = userEvent.setup();
  mockSettings(fullSettings());
  let posted = 0;
  server.use(
    http.post("/api/v1/backup", () => {
      posted += 1;
      return HttpResponse.json(
        { path: "/var/lib/dnsaur/backups/dnsaur-20260911T140822Z.db", bytes: 2_411_724 },
        { status: 201 },
      );
    }),
  );

  renderWithProviders(<SettingsPage />);
  await screen.findByText("Upstreams");

  await user.click(screen.getByRole("button", { name: /back up now/i }));

  expect(
    await screen.findByText("/var/lib/dnsaur/backups/dnsaur-20260911T140822Z.db"),
  ).toBeInTheDocument();
  expect(screen.getByText(/2\.3 MB/)).toBeInTheDocument();
  expect(posted).toBe(1);
  // It is an action, not a field: nothing about it belongs to the form's
  // unsaved-changes count.
  expect(screen.getByText("All changes saved")).toBeInTheDocument();
});

// The 409 names the tool to run instead, which is the useful half, so it is
// shown as the server wrote it rather than reworded into "couldn't back up".
test("a postgres install is shown the server's own pg_dump answer", async () => {
  const user = userEvent.setup();
  mockSettings(fullSettings());
  server.use(
    http.post("/api/v1/backup", () =>
      HttpResponse.json(
        { error: "backups are a sqlite feature; use pg_dump for postgres" },
        { status: 409 },
      ),
    ),
  );

  renderWithProviders(<SettingsPage />);
  await screen.findByText("Upstreams");

  await user.click(screen.getByRole("button", { name: /back up now/i }));

  expect(
    await screen.findByText("backups are a sqlite feature; use pg_dump for postgres"),
  ).toBeInTheDocument();
});

// --- replica mode ---------------------------------------------------------

// Spec §4.3/§7: the instance keys stay this box's to write, everything else
// belongs to the main and answers 409. The screen has to draw that line
// where the server draws it — a band left live whose every save is refused
// is worse than one that says so up front.
test("a replica keeps the local bands editable and shows the synced ones read-only", async () => {
  mockSettings(fullSettings());
  server.use(...replicaHandlers("https://main.lan"));

  renderWithProviders(<SettingsPage />);

  expect(await screen.findByText("Managed by https://main.lan")).toBeInTheDocument();

  // Synced: the main's to change.
  expect(screen.getByLabelText("Blocked response TTL (seconds)")).toBeDisabled();
  expect(screen.getByLabelText("Retention (days)")).toBeDisabled();

  // Local: still this box's own.
  expect(screen.getByLabelText("DNS-over-TLS listen address")).toBeEnabled();
  expect(screen.getByLabelText("Peer URL")).toBeEnabled();
});

test("a main leaves every band editable and shows no notice", async () => {
  mockSettings(fullSettings());

  renderWithProviders(<SettingsPage />);

  expect(await screen.findByLabelText("Blocked response TTL (seconds)")).toBeEnabled();
  expect(screen.queryByText(/^Managed by/)).not.toBeInTheDocument();
});

// --- the write-only token ---------------------------------------------------

// `sync.token` is the one editable key GET /settings never returns, so the
// box starts empty on every load and "empty" has to keep meaning "leave the
// stored one alone". That only works because the refetch after a save
// notices the server still reports nothing for the key and puts the field —
// and the baseline it is measured against — back to empty together. Get
// that pairing wrong in either direction and the page either offers to
// re-save a credential it no longer holds, or sits on a permanent unsaved
// change nobody can clear.
test("a saved sync token is sent, then leaves the box empty and nothing unsaved", async () => {
  const user = userEvent.setup();
  const sent: unknown[] = [];
  mockSettings(fullSettings());
  server.use(
    http.put("/api/v1/settings", async ({ request }) => {
      sent.push(await request.json());
      return new HttpResponse(null, { status: 204 });
    }),
  );

  renderWithProviders(<SettingsPage />);
  await screen.findByText("Sync");

  await user.type(screen.getByLabelText("Peer URL"), "https://main.lan");
  await user.type(screen.getByLabelText("Token"), "dnsr_abc123");
  await user.click(screen.getAllByRole("button", { name: /^save changes$/i })[0]);

  // The credential goes out with the value that was typed — and before the
  // peer it is judged against (SAVE_PHASES), which is why both are asserted
  // as an ordered pair rather than as a set.
  await waitFor(() => expect(sent).toHaveLength(2));
  expect(sent).toEqual([
    { key: "sync.token", value: "dnsr_abc123" },
    { key: "sync.peer_url", value: "https://main.lan" },
  ]);

  // And then the box is empty again, with nothing left to save: the peer
  // URL came back from the server, the token did not, and the form took
  // both answers.
  await waitFor(() => expect(screen.getByLabelText("Token")).toHaveValue(""));
  expect(screen.getByLabelText("Peer URL")).toHaveValue("https://main.lan");
  expect(screen.getByText(/all changes saved/i)).toBeInTheDocument();
});

// Rotating the token on a box that already follows a main is the same save
// with one field in it, and it is the case where "empty means leave the
// stored one alone" and "the box always starts empty" are the same field:
// nothing but the token may go out, and nothing may be left unsaved
// afterwards for a value the server will never report back.
test("a token typed on its own is the only key saved, and leaves nothing unsaved", async () => {
  const user = userEvent.setup();
  const sent: unknown[] = [];
  mockSettings(fullSettings({ "sync.peer_url": "https://main.lan" }));
  server.use(
    http.put("/api/v1/settings", async ({ request }) => {
      sent.push(await request.json());
      return new HttpResponse(null, { status: 204 });
    }),
  );

  renderWithProviders(<SettingsPage />);
  await screen.findByText("Sync");

  await user.type(screen.getByLabelText("Token"), "dnsr_rotated");
  await user.click(screen.getAllByRole("button", { name: /^save changes$/i })[0]);

  await waitFor(() => expect(sent).toHaveLength(1));
  expect(sent).toEqual([{ key: "sync.token", value: "dnsr_rotated" }]);

  await waitFor(() => expect(screen.getByLabelText("Token")).toHaveValue(""));
  expect(screen.getByLabelText("Peer URL")).toHaveValue("https://main.lan");
  expect(screen.getByText(/all changes saved/i)).toBeInTheDocument();
});

// The Sync band shows GET /sync/status, which is not a setting and so is not
// invalidated by the settings query — but a save is exactly what moves it.
// At its own 30s cadence the band spent up to half a minute contradicting
// the value sitting in the box above it.
test("saving a setting re-reads the sync status the band renders", async () => {
  const user = userEvent.setup();
  let statusReads = 0;
  mockSettings(fullSettings());
  server.use(
    http.get("/api/v1/sync/status", () => {
      statusReads += 1;
      return HttpResponse.json({ role: "main" });
    }),
    http.put("/api/v1/settings", () => new HttpResponse(null, { status: 204 })),
  );

  renderWithProviders(<SettingsPage />);
  await screen.findByText("Sync");
  await waitFor(() => expect(statusReads).toBe(1));

  const ttl = screen.getByLabelText(/^blocked response ttl/i);
  await user.clear(ttl);
  await user.type(ttl, "60");
  await user.click(screen.getAllByRole("button", { name: /^save changes$/i })[0]);

  await waitFor(() => expect(statusReads).toBe(2));
});
