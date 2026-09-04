import { http, HttpResponse } from "msw";
import { toast } from "sonner";
import { afterEach, expect, test, vi } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { server } from "../test/msw-server";
import { renderWithProviders } from "../test/render";
import type { TSIGKey, Zone } from "../api/types";
import { MASK, TSIGKeys } from "./tsig-keys";

// A real 32-byte base64 secret, the shape the generator produces and the
// only shape the API accepts (it base64-decodes at write time).
const SECRET_256 = "xb10GdOUbvmqpzu1X6pbQvdmcK0Kkp2XnMKZJt/k5ZI=";
const SECRET_512 = "KrE5LmHD5Jb7wbs7qHisdwUX466I/RY5Kfj40nppDvk=";
const SECRET_OLD = "MHmDwZnh8rYuvii/VpkjOFUwrQdJgxH3xGO8ofvAItw=";

function key(overrides: Partial<TSIGKey> = {}): TSIGKey {
  return {
    id: 1,
    name: "xfer.e412.in.",
    // The wire value carries the trailing dot — these are miekg's own
    // constants (internal/api/tsigkeys_handlers.go's tsigAlgorithms).
    algorithm: "hmac-sha256.",
    secret: SECRET_256,
    created_at: Date.now() - 86_400_000,
    ...overrides,
  };
}

/** A zone that names a TSIG key — the only fields of it this screen reads
 * are tsig_key_id and allow_transfer. */
function zone(overrides: Partial<Zone> = {}): Zone {
  return {
    id: 1,
    name: "e412.in",
    type: "secondary",
    enabled: true,
    soa_ns: "ns.e412.in",
    soa_mbox: "hostadmin.e412.in",
    soa_serial: 1,
    soa_refresh: 900,
    soa_retry: 300,
    soa_expire: 604800,
    soa_minimum: 900,
    soa_ttl: 900,
    primaries: "203.0.113.9",
    tsig_key_id: 0,
    expires_at: 0,
    refreshed_at: 0,
    last_error: "",
    last_attempt: 0,
    allow_transfer: "",
    last_xfr_at: 0,
    last_xfr_peer: "",
    last_xfr_error: "",
    notify_to: "",
    forward_to: "",
    created_at: Date.now() - 86_400_000,
    modified_at: Date.now() - 60_000,
    ...overrides,
  };
}

function mockZones(zones: Zone[]) {
  server.use(http.get("/api/v1/zones", () => HttpResponse.json(zones)));
}

function mockKeys(keys: TSIGKey[]) {
  server.use(http.get("/api/v1/tsig-keys", () => HttpResponse.json(keys)));
}

function rows(): HTMLElement[] {
  return screen.getAllByTestId("tsig-key-row");
}

async function openCreateRow(user: ReturnType<typeof userEvent.setup>) {
  // findAllBy*: an empty list offers "New key" twice (header + empty state).
  const [button] = await screen.findAllByRole("button", { name: /new key/i });
  await user.click(button);
}

afterEach(() => vi.restoreAllMocks());

// The screen's central display decision: four secrets are not left sitting
// in plain sight, and only one can be open at a time — revealing the second
// row closes the first, so a screen share never accumulates them.
test("secrets are masked at rest and the eye reveals one row at a time", async () => {
  const user = userEvent.setup();
  mockKeys([
    key({ id: 1, name: "xfer.e412.in.", secret: SECRET_256 }),
    key({ id: 2, name: "secondary.e412.in.", algorithm: "hmac-sha512.", secret: SECRET_512 }),
  ]);
  renderWithProviders(<TSIGKeys />);
  await waitFor(() => expect(rows()).toHaveLength(2));

  // Masked to begin with — neither secret is anywhere on the page.
  expect(within(rows()[0]).getByText(MASK)).toBeInTheDocument();
  expect(within(rows()[1]).getByText(MASK)).toBeInTheDocument();
  expect(screen.queryByText(SECRET_256)).not.toBeInTheDocument();

  await user.click(screen.getByRole("button", { name: /show the secret for xfer\.e412\.in\./i }));
  expect(within(rows()[0]).getByText(SECRET_256)).toBeInTheDocument();
  expect(within(rows()[1]).getByText(MASK)).toBeInTheDocument();

  // Opening the second closes the first, rather than adding to it.
  await user.click(
    screen.getByRole("button", { name: /show the secret for secondary\.e412\.in\./i }),
  );
  expect(within(rows()[1]).getByText(SECRET_512)).toBeInTheDocument();
  expect(within(rows()[0]).getByText(MASK)).toBeInTheDocument();
  expect(screen.queryByText(SECRET_256)).not.toBeInTheDocument();

  // And the open one closes itself.
  await user.click(
    screen.getByRole("button", { name: /hide the secret for secondary\.e412\.in\./i }),
  );
  expect(within(rows()[1]).getByText(MASK)).toBeInTheDocument();
});

// Copying is the common task (the secret has to be pasted into the peer's
// config verbatim); reading it aloud is the exception. So Copy is always
// available and does NOT reveal — the row stays masked through the click.
test("Copy puts the real secret on the clipboard while the row stays masked", async () => {
  const user = userEvent.setup();
  const writeText = vi.fn<(text: string) => Promise<void>>(() => Promise.resolve());
  vi.stubGlobal("navigator", { ...navigator, clipboard: { writeText } });
  mockKeys([key({ id: 1, name: "xfer.e412.in.", secret: SECRET_256 })]);

  renderWithProviders(<TSIGKeys />);
  await waitFor(() => expect(rows()).toHaveLength(1));

  await user.click(screen.getByRole("button", { name: /copy the secret for xfer\.e412\.in\./i }));

  await waitFor(() => expect(writeText).toHaveBeenCalledWith(SECRET_256));
  expect(within(rows()[0]).getByText(MASK)).toBeInTheDocument();
  expect(screen.queryByText(SECRET_256)).not.toBeInTheDocument();
});

// Generating is the default path — a hand-typed secret is usually a weak
// one. What matters is that the value the admin was shown is the value that
// reaches the server: the field is the record of what was generated, and
// anything else would put a key on the peer that dnsaur never stored.
test("the create row posts the generated secret exactly as displayed", async () => {
  const user = userEvent.setup();
  let body: unknown;
  mockKeys([]);
  server.use(
    http.post("/api/v1/tsig-keys", async ({ request }) => {
      body = await request.json();
      return HttpResponse.json({ id: 9 }, { status: 201 });
    }),
  );

  renderWithProviders(<TSIGKeys />);
  await openCreateRow(user);

  const secret = screen.getByLabelText(/^secret$/i) as HTMLInputElement;
  const generated = secret.value;
  // 32 bytes, base64 — 44 characters with one pad byte.
  expect(generated).toMatch(/^[A-Za-z0-9+/]{43}=$/);

  await user.type(screen.getByLabelText(/key name/i), "  XFER.e412.IN  ");
  await user.click(screen.getByRole("button", { name: /^add$/i }));

  await waitFor(() =>
    expect(body).toEqual({
      // Trimmed but not canonicalised here — the server lowercases and adds
      // the trailing dot, and the row said so before the click.
      name: "XFER.e412.IN",
      algorithm: "hmac-sha256.",
      secret: generated,
    }),
  );
});

// The mismatch this screen exists on top of: the API's algorithm values are
// miekg's constants and carry a trailing dot; the design shows them without
// one. If the select's values were the *displayed* strings, no fetched key
// would ever match an option — the control would show the first algorithm
// instead, and saving an untouched row would silently rewrite hmac-sha512 to
// hmac-sha1.
test("the algorithm select round-trips a fetched value rather than rewriting it", async () => {
  const user = userEvent.setup();
  let body: unknown;
  mockKeys([key({ id: 4, name: "secondary.e412.in.", algorithm: "hmac-sha512." })]);
  server.use(
    http.put("/api/v1/tsig-keys/4", async ({ request }) => {
      body = await request.json();
      return new HttpResponse(null, { status: 204 });
    }),
  );

  renderWithProviders(<TSIGKeys />);
  await waitFor(() => expect(rows()).toHaveLength(1));
  // Displayed without the dot, at rest.
  expect(within(rows()[0]).getByText("hmac-sha512")).toBeInTheDocument();

  await user.click(screen.getByRole("button", { name: /edit secondary\.e412\.in\./i }));

  const select = screen.getByLabelText(/algorithm/i) as HTMLSelectElement;
  expect(select.value).toBe("hmac-sha512.");
  // The option is *labelled* without the dot and *valued* with it.
  expect(within(select).getByRole("option", { name: "hmac-sha512" })).toHaveProperty(
    "selected",
    true,
  );
  expect(within(select).queryByRole("option", { name: "hmac-sha512." })).not.toBeInTheDocument();

  // Saving an untouched row must put back exactly what came out.
  await user.click(screen.getByRole("button", { name: /^save$/i }));
  await waitFor(() =>
    expect(body).toEqual({
      name: "secondary.e412.in.",
      algorithm: "hmac-sha512.",
      secret: SECRET_256,
    }),
  );
});

test("deleting a key confirms inline, then DELETEs it and drops the row", async () => {
  const user = userEvent.setup();
  let deleted = false;
  const keys = [
    key({ id: 1, name: "xfer.e412.in." }),
    key({ id: 7, name: "legacy-xfer.e412.in.", algorithm: "hmac-sha1.", secret: SECRET_OLD }),
  ];
  server.use(
    http.get("/api/v1/tsig-keys", () =>
      HttpResponse.json(deleted ? keys.filter((k) => k.id !== 7) : keys),
    ),
    http.delete("/api/v1/tsig-keys/7", () => {
      deleted = true;
      return new HttpResponse(null, { status: 204 });
    }),
  );

  renderWithProviders(<TSIGKeys />);
  await waitFor(() => expect(rows()).toHaveLength(2));

  await user.click(screen.getByRole("button", { name: /delete legacy-xfer\.e412\.in\./i }));
  expect(await screen.findByText("Delete legacy-xfer.e412.in.?")).toBeInTheDocument();

  await user.click(screen.getByRole("button", { name: /^delete$/i }));

  await waitFor(() => expect(deleted).toBe(true));
  await waitFor(() => expect(rows()).toHaveLength(1));
  expect(screen.queryByText("legacy-xfer.e412.in.")).not.toBeInTheDocument();
});

test("the delete confirm can be cancelled and nothing is sent", async () => {
  const user = userEvent.setup();
  let deleted = false;
  mockKeys([key({ id: 7, name: "legacy-xfer.e412.in." })]);
  server.use(
    http.delete("/api/v1/tsig-keys/7", () => {
      deleted = true;
      return new HttpResponse(null, { status: 204 });
    }),
  );

  renderWithProviders(<TSIGKeys />);
  await waitFor(() => expect(rows()).toHaveLength(1));

  await user.click(screen.getByRole("button", { name: /delete legacy-xfer\.e412\.in\./i }));
  await user.click(await screen.findByRole("button", { name: /^cancel$/i }));

  await waitFor(() =>
    expect(screen.queryByText("Delete legacy-xfer.e412.in.?")).not.toBeInTheDocument(),
  );
  expect(deleted).toBe(false);
});

// The server canonicalises every name it is given (lowercase, trailing dot
// — normalizeTSIGName in tsigkeys_handlers.go), and the key has to match the
// name the peer signs with. Saying so while it is being typed is cheaper
// than showing a row that doesn't look like what was entered.
test("the create row shows what the typed name will be saved as", async () => {
  const user = userEvent.setup();
  mockKeys([]);
  renderWithProviders(<TSIGKeys />);
  await openCreateRow(user);

  // Nothing typed, nothing claimed.
  expect(screen.queryByText(/^saved as/i)).not.toBeInTheDocument();

  await user.type(screen.getByLabelText(/key name/i), "XFER.e412.IN");
  expect(await screen.findByText("Saved as xfer.e412.in.")).toBeInTheDocument();
});

// The other half of the create row's secret story: generate is the default,
// and "Paste an existing secret" is the deliberate departure from it —
// which has to clear the generated value, or the field silently keeps a
// secret the peer has never heard of.
test("switching to paste clears the generated secret, and generating again refills it", async () => {
  const user = userEvent.setup();
  mockKeys([]);
  renderWithProviders(<TSIGKeys />);
  await openCreateRow(user);

  const secret = screen.getByLabelText(/^secret$/i) as HTMLInputElement;
  expect(secret.value).not.toBe("");
  expect(screen.getByText("base64, 32 bytes")).toBeInTheDocument();

  await user.click(screen.getByRole("button", { name: /paste an existing secret/i }));
  expect(secret.value).toBe("");
  expect(screen.getByPlaceholderText(/base64 secret from the other server/i)).toBeInTheDocument();

  await user.click(screen.getByRole("button", { name: /generate one/i }));
  expect(secret.value).toMatch(/^[A-Za-z0-9+/]{43}=$/);
});

test("regenerate replaces the secret with a different one", async () => {
  const user = userEvent.setup();
  mockKeys([]);
  renderWithProviders(<TSIGKeys />);
  await openCreateRow(user);

  const secret = screen.getByLabelText(/^secret$/i) as HTMLInputElement;
  const first = secret.value;
  await user.click(screen.getByRole("button", { name: /generate a new secret/i }));

  expect(secret.value).not.toBe(first);
  expect(secret.value).toMatch(/^[A-Za-z0-9+/]{43}=$/);
});

test("a name the server would refuse is rejected client-side and never posted", async () => {
  const user = userEvent.setup();
  let posted = false;
  mockKeys([]);
  server.use(
    http.post("/api/v1/tsig-keys", () => {
      posted = true;
      return HttpResponse.json({ id: 9 }, { status: 201 });
    }),
  );

  renderWithProviders(<TSIGKeys />);
  await openCreateRow(user);
  await user.type(screen.getByLabelText(/key name/i), "has spaces.e412.in");
  await user.click(screen.getByRole("button", { name: /^add$/i }));

  expect(await screen.findByText(/enter a domain name/i)).toBeInTheDocument();
  await new Promise((resolve) => setTimeout(resolve, 30));
  expect(posted).toBe(false);
});

test("a secret that isn't base64 is rejected client-side and never posted", async () => {
  const user = userEvent.setup();
  let posted = false;
  mockKeys([]);
  server.use(
    http.post("/api/v1/tsig-keys", () => {
      posted = true;
      return HttpResponse.json({ id: 9 }, { status: 201 });
    }),
  );

  renderWithProviders(<TSIGKeys />);
  await openCreateRow(user);
  await user.click(screen.getByRole("button", { name: /paste an existing secret/i }));
  await user.type(screen.getByLabelText(/key name/i), "xfer.e412.in");
  await user.type(screen.getByLabelText(/^secret$/i), "not base64!!");
  await user.click(screen.getByRole("button", { name: /^add$/i }));

  expect(await screen.findByText(/base64/i)).toBeInTheDocument();
  await new Promise((resolve) => setTimeout(resolve, 30));
  expect(posted).toBe(false);
});

test("shows an empty state with the artboard's copy and a New key action", async () => {
  mockKeys([]);
  renderWithProviders(<TSIGKeys />);

  expect(await screen.findByText("No TSIG keys yet.")).toBeInTheDocument();
  expect(screen.getByText("No keys")).toBeInTheDocument();
  expect(screen.getAllByRole("button", { name: /new key/i }).length).toBeGreaterThan(0);
});

test("the header counts keys and pluralises", async () => {
  mockKeys([key({ id: 1 })]);
  const one = renderWithProviders(<TSIGKeys />);
  expect(await screen.findByText("1 key")).toBeInTheDocument();
  one.unmount();

  mockKeys([key({ id: 1 }), key({ id: 2, name: "b.e412.in." })]);
  renderWithProviders(<TSIGKeys />);
  expect(await screen.findByText("2 keys")).toBeInTheDocument();
});

// Held back through Milestone D1 because nothing could reference a key yet;
// D2 is what gives it something to count. The count is folded from the zones
// list rather than served as a field on the key — every zone already carries
// tsig_key_id, so an endpoint for it would be a second source of the same
// truth.
test("the USED BY column counts the zones that name each key", async () => {
  mockKeys([
    key({ id: 1 }),
    key({ id: 2, name: "solo.e412.in." }),
    key({ id: 3, name: "un.e412.in." }),
  ]);
  mockZones([
    zone({ id: 10, tsig_key_id: 1 }),
    zone({ id: 11, tsig_key_id: 1 }),
    zone({ id: 12, tsig_key_id: 2 }),
    zone({ id: 13, tsig_key_id: 0 }),
  ]);
  renderWithProviders(<TSIGKeys />);
  await waitFor(() => expect(rows()).toHaveLength(3));

  expect(screen.getByText("Used by")).toBeInTheDocument();
  expect(within(rows()[0]).getByText("2 zones")).toBeInTheDocument();
  expect(within(rows()[1]).getByText("1 zone")).toBeInTheDocument();
  // "—", not "0": the column answers "what depends on this", and nothing is
  // not a quantity.
  expect(within(rows()[2]).getByText("—")).toBeInTheDocument();
});

// D3: a key can also be referenced from the other direction — named in a
// zone's allow_transfer rather than held as that zone's own tsig_key_id
// (which is who a *secondary* signs its own pulls with; allow_transfer is
// who may pull *this* zone). Both are real dependents, so both must count.
test("a key named only by a zone's allow_transfer counts as in use too", async () => {
  mockKeys([key({ id: 1, name: "xfer.e412.in." }), key({ id: 2, name: "solo.e412.in." })]);
  mockZones([
    // Names key 1 as its own signing key, and key 2 in its allow_transfer —
    // one zone, two different kinds of reference, two different keys.
    zone({ id: 10, tsig_key_id: 1, allow_transfer: "key:solo.e412.in." }),
  ]);
  renderWithProviders(<TSIGKeys />);
  await waitFor(() => expect(rows()).toHaveLength(2));

  expect(within(rows()[0]).getByText("1 zone")).toBeInTheDocument();
  expect(within(rows()[1]).getByText("1 zone")).toBeInTheDocument();
});

// A key can be referenced both ways by the same zone, or by different
// zones, without being double-counted from one and undercounted from the
// other — each zone contributes at most one to a key's count.
test("a key named by both tsig_key_id and allow_transfer on the same zone counts once", async () => {
  mockKeys([key({ id: 1, name: "xfer.e412.in." })]);
  mockZones([zone({ id: 10, tsig_key_id: 1, allow_transfer: "key:xfer.e412.in." })]);
  renderWithProviders(<TSIGKeys />);
  await waitFor(() => expect(rows()).toHaveLength(1));

  expect(within(rows()[0]).getByText("1 zone")).toBeInTheDocument();
});

// D4 Task 12: a third kind of reference, on the same terms as D3's
// allow_transfer above — notify_to is who this zone signs its own outbound
// NOTIFYs for, a `key:` entry in a different field with its own grammar
// (lib/notify.ts) but the same underlying dependency.
test("a key named only by a zone's notify_to counts as in use too", async () => {
  mockKeys([key({ id: 1, name: "xfer.e412.in." }), key({ id: 2, name: "solo.e412.in." })]);
  mockZones([
    // Names key 1 as its own signing key, and key 2 in its notify_to — one
    // zone, two different kinds of reference, two different keys.
    zone({ id: 10, tsig_key_id: 1, notify_to: "10.0.0.2 key:solo.e412.in." }),
  ]);
  renderWithProviders(<TSIGKeys />);
  await waitFor(() => expect(rows()).toHaveLength(2));

  expect(within(rows()[0]).getByText("1 zone")).toBeInTheDocument();
  expect(within(rows()[1]).getByText("1 zone")).toBeInTheDocument();
});

// A key can be named by allow_transfer and notify_to on the same zone
// (e.g. a secondary that both re-serves and re-notifies under the same
// key) without being double-counted — the union still folds through one
// Set per zone.
test("a key named by both allow_transfer and notify_to on the same zone counts once", async () => {
  mockKeys([key({ id: 1, name: "xfer.e412.in." })]);
  mockZones([
    zone({
      id: 10,
      allow_transfer: "key:xfer.e412.in.",
      notify_to: "10.0.0.2 key:xfer.e412.in.",
    }),
  ]);
  renderWithProviders(<TSIGKeys />);
  await waitFor(() => expect(rows()).toHaveLength(1));

  expect(within(rows()[0]).getByText("1 zone")).toBeInTheDocument();
});

// The store refuses to delete a key an ACL names, on the same terms as one
// named by tsig_key_id — so the screen has to say so before the click, not
// let it 409. Reuses the existing disabled-delete affordance and copy.
test("a key an allow_transfer names cannot be deleted, and the confirm says why", async () => {
  const user = userEvent.setup();
  let deleted = false;
  mockKeys([key({ id: 5, name: "solo.e412.in." })]);
  mockZones([zone({ id: 10, allow_transfer: "key:solo.e412.in." })]);
  server.use(
    http.delete("/api/v1/tsig-keys/5", () => {
      deleted = true;
      return new HttpResponse(null, { status: 204 });
    }),
  );
  renderWithProviders(<TSIGKeys />);
  await waitFor(() => expect(rows()).toHaveLength(1));

  await user.click(screen.getByRole("button", { name: /delete solo\.e412\.in\./i }));
  expect(
    await screen.findByText("In use by 1 zone. Remove it from them first."),
  ).toBeInTheDocument();
  const confirm = screen.getByRole("button", { name: /^delete$/i });
  expect(confirm).toBeDisabled();

  await user.click(confirm);
  await new Promise((resolve) => setTimeout(resolve, 30));
  expect(deleted).toBe(false);
});

// Same guard, the notify_to side of it — the store's own NOT EXISTS clause
// against notifyKeyRef is what actually enforces this; this test is the
// screen not letting the click reach that 409 in the first place.
test("a key a notify_to names cannot be deleted, and the confirm says why", async () => {
  const user = userEvent.setup();
  let deleted = false;
  mockKeys([key({ id: 5, name: "solo.e412.in." })]);
  mockZones([zone({ id: 10, notify_to: "10.0.0.2 key:solo.e412.in." })]);
  server.use(
    http.delete("/api/v1/tsig-keys/5", () => {
      deleted = true;
      return new HttpResponse(null, { status: 204 });
    }),
  );
  renderWithProviders(<TSIGKeys />);
  await waitFor(() => expect(rows()).toHaveLength(1));

  await user.click(screen.getByRole("button", { name: /delete solo\.e412\.in\./i }));
  expect(
    await screen.findByText("In use by 1 zone. Remove it from them first."),
  ).toBeInTheDocument();
  const confirm = screen.getByRole("button", { name: /^delete$/i });
  expect(confirm).toBeDisabled();

  await user.click(confirm);
  await new Promise((resolve) => setTimeout(resolve, 30));
  expect(deleted).toBe(false);
});

// The server refuses this delete with 409 (tsigKeyStore.Delete, one
// statement), because removing the key would leave that secondary unable to
// authenticate its transfers with nothing on the zone to say why. The guard
// is what stops the button producing that 409 on click.
test("a key a zone uses cannot be deleted, and the confirm says why", async () => {
  const user = userEvent.setup();
  let deleted = false;
  mockKeys([key({ id: 5 })]);
  mockZones([zone({ id: 10, tsig_key_id: 5 }), zone({ id: 11, tsig_key_id: 5 })]);
  server.use(
    http.delete("/api/v1/tsig-keys/5", () => {
      deleted = true;
      return new HttpResponse(null, { status: 204 });
    }),
  );
  renderWithProviders(<TSIGKeys />);
  await waitFor(() => expect(rows()).toHaveLength(1));

  await user.click(screen.getByRole("button", { name: /delete xfer\.e412\.in\./i }));
  expect(
    await screen.findByText("In use by 2 zones. Remove it from them first."),
  ).toBeInTheDocument();
  const confirm = screen.getByRole("button", { name: /^delete$/i });
  expect(confirm).toBeDisabled();

  await user.click(confirm);
  await new Promise((resolve) => setTimeout(resolve, 30));
  expect(deleted).toBe(false);
});

// The guard reads a list that can be missing: if /zones failed, every key
// reads "—" and every Delete is enabled. The server still refuses, and the
// 409 has to say something better than its own "resource in use".
test("a 409 on delete explains itself, even when the zones list never loaded", async () => {
  const user = userEvent.setup();
  mockKeys([key({ id: 5 })]);
  server.use(
    http.get("/api/v1/zones", () => HttpResponse.json({ error: "boom" }, { status: 500 })),
    http.delete("/api/v1/tsig-keys/5", () =>
      HttpResponse.json({ error: "resource in use" }, { status: 409 }),
    ),
  );
  const errorSpy = vi.spyOn(toast, "error");
  renderWithProviders(<TSIGKeys />);
  await waitFor(() => expect(rows()).toHaveLength(1));

  await user.click(screen.getByRole("button", { name: /delete xfer\.e412\.in\./i }));
  const confirm = await screen.findByRole("button", { name: /^delete$/i });
  expect(confirm).toBeEnabled();
  await user.click(confirm);

  await waitFor(() =>
    expect(errorSpy).toHaveBeenCalledWith(expect.stringMatching(/is in use by a zone/i)),
  );
});

test("a failed delete shows a toast and leaves the key in the list", async () => {
  const user = userEvent.setup();
  mockKeys([key({ id: 7, name: "legacy-xfer.e412.in." })]);
  server.use(
    http.delete("/api/v1/tsig-keys/7", () => HttpResponse.json({ error: "boom" }, { status: 500 })),
  );
  const errorSpy = vi.spyOn(toast, "error");

  renderWithProviders(<TSIGKeys />);
  await waitFor(() => expect(rows()).toHaveLength(1));

  await user.click(screen.getByRole("button", { name: /delete legacy-xfer\.e412\.in\./i }));
  await user.click(await screen.findByRole("button", { name: /^delete$/i }));

  await waitFor(() =>
    expect(errorSpy).toHaveBeenCalledWith(
      expect.stringMatching(/couldn't delete legacy-xfer\.e412\.in\./i),
    ),
  );
  expect(rows()).toHaveLength(1);
});

test("a rejected create keeps the row open and reports the server's reason", async () => {
  const user = userEvent.setup();
  mockKeys([]);
  server.use(
    http.post("/api/v1/tsig-keys", () =>
      HttpResponse.json({ error: "a TSIG key with that name already exists" }, { status: 409 }),
    ),
  );
  const errorSpy = vi.spyOn(toast, "error");

  renderWithProviders(<TSIGKeys />);
  await openCreateRow(user);
  await user.type(screen.getByLabelText(/key name/i), "xfer.e412.in");
  await user.click(screen.getByRole("button", { name: /^add$/i }));

  await waitFor(() =>
    expect(errorSpy).toHaveBeenCalledWith("a TSIG key with that name already exists"),
  );
  expect(screen.getByLabelText(/key name/i)).toBeInTheDocument();
});

// A first load that failed has nothing to fall back to, so it says so
// instead of rendering an empty list that reads as "you have no keys".
test("a failed load says so rather than showing an empty list", async () => {
  server.use(
    http.get("/api/v1/tsig-keys", () => HttpResponse.json({ error: "boom" }, { status: 500 })),
  );
  renderWithProviders(<TSIGKeys />);

  // `timeout` because the query client retries once with a backoff before
  // the failure is final (see lib/query-client.ts).
  expect(
    await screen.findByText(/couldn't load tsig keys/i, undefined, { timeout: 3000 }),
  ).toBeInTheDocument();
  expect(screen.queryByText("No TSIG keys yet.")).not.toBeInTheDocument();
});
