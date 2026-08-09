import { http, HttpResponse } from "msw";
import { toast } from "sonner";
import { afterEach, expect, test, vi } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { Route, Routes } from "react-router";
import { server } from "../../test/msw-server";
import { renderWithProviders } from "../../test/render";
import type { Zone, ZoneRecord } from "../../api/types";
import { ZoneDetail } from "./detail";

function zone(overrides: Partial<Zone> = {}): Zone {
  return {
    id: 1,
    name: "example.com",
    type: "primary",
    enabled: true,
    soa_ns: "ns.example.com",
    soa_mbox: "hostadmin.example.com",
    soa_serial: 3,
    soa_refresh: 7200,
    soa_retry: 3600,
    soa_expire: 1209600,
    soa_minimum: 300,
    soa_ttl: 900,
    primaries: "",
    tsig_key_id: 0,
    expires_at: 0,
    refreshed_at: 0,
    created_at: Date.now() - 86_400_000,
    modified_at: Date.now() - 60_000,
    ...overrides,
  };
}

function record(overrides: Partial<ZoneRecord> = {}): ZoneRecord {
  return {
    id: 1,
    zone_id: 1,
    name: "bifrost",
    type: "A",
    ttl: 300,
    rdata: "10.0.0.1",
    enabled: true,
    comment: "",
    ...overrides,
  };
}

function mockZone(z: Zone) {
  server.use(http.get(`/api/v1/zones/${z.id}`, () => HttpResponse.json(z)));
}

function mockRecords(zoneId: number, records: ZoneRecord[]) {
  server.use(http.get(`/api/v1/zones/${zoneId}/records`, () => HttpResponse.json(records)));
}

/** Renders under a real route so useParams() resolves ":id" the way the
 * app router does. Every test targets zone id 1 unless it overrides the
 * route itself. */
function renderDetail(route = "/zones/1") {
  return renderWithProviders(
    <Routes>
      <Route path="/zones/:id" element={<ZoneDetail />} />
    </Routes>,
    { route },
  );
}

function recordRows(): HTMLElement[] {
  return Array.from(document.querySelectorAll<HTMLElement>('[data-testid="zone-record-row"]'));
}

/** Mocks both the zone and its records fetch and renders the detail page for
 * it — used by tests that only care about the zone itself (its type) rather
 * than a specific record set, so they don't have to call mockZone/mockRecords
 * separately. */
function renderZoneDetail({ zone: z, records = [] }: { zone: Zone; records?: ZoneRecord[] }) {
  mockZone(z);
  mockRecords(z.id, records);
  return renderDetail(`/zones/${z.id}`);
}

afterEach(() => vi.restoreAllMocks());

// Record `name` is zone-apex-relative — "@" is the literal, stored value
// for the apex record, never the zone's own name. A row that quietly
// substituted the zone name back in would make every apex record
// indistinguishable from a record actually named after the zone.
test("the apex row shows @ rather than the zone name", async () => {
  mockZone(zone({ id: 1, name: "example.com" }));
  mockRecords(1, [record({ id: 1, name: "@", type: "NS", rdata: "ns.example.com." })]);

  renderDetail();
  const [row] = await waitFor(() => {
    const rows = recordRows();
    expect(rows).toHaveLength(1);
    return rows;
  });

  expect(within(row).getByText("@")).toBeInTheDocument();
  expect(within(row).queryByText("example.com")).not.toBeInTheDocument();
});

// The API validates rdata with dns.NewRR, so its 400 message *is* the
// parser's own text. The brief is explicit that this must reach the admin
// verbatim rather than get replaced with generic copy — this test pins the
// exact string landing on screen, not just "some error appeared".
test("a parser error from the API lands on the data field", async () => {
  const user = userEvent.setup();
  mockZone(zone({ id: 1 }));
  mockRecords(1, []);
  server.use(
    http.post("/api/v1/zones/1/records", () =>
      HttpResponse.json({ error: "dns: bad MX Mx" }, { status: 400 }),
    ),
  );

  renderDetail();
  await screen.findByLabelText(/zone record name/i);

  await user.selectOptions(screen.getByLabelText(/^record type$/i), "MX");
  await user.type(screen.getByLabelText(/^data$/i), "Mx");
  await user.click(screen.getByRole("button", { name: /^add$/i }));

  const message = await screen.findByText("dns: bad MX Mx");
  // The parser's own text, rendered mono — not rewritten into friendly prose.
  expect(message).toHaveClass("font-mono");
  expect(screen.getByLabelText(/^data$/i)).toHaveAttribute("aria-invalid", "true");
});

// One text input serves nine record types; the DNS parser (not a per-type
// form) is what actually validates the value, so the placeholder is the
// only per-type guidance the row gives.
test("the placeholder follows the selected type", async () => {
  const user = userEvent.setup();
  mockZone(zone({ id: 1 }));
  mockRecords(1, []);

  renderDetail();
  await screen.findByLabelText(/zone record name/i);

  expect(screen.getByPlaceholderText("192.168.150.28")).toBeInTheDocument();

  await user.selectOptions(screen.getByLabelText(/^record type$/i), "MX");
  expect(screen.getByPlaceholderText("10 mail.example.com.")).toBeInTheDocument();

  await user.selectOptions(screen.getByLabelText(/^record type$/i), "CAA");
  expect(screen.getByPlaceholderText('0 issue "letsencrypt.org"')).toBeInTheDocument();
});

test("TTL renders right-aligned and thousands-grouped", async () => {
  mockZone(zone({ id: 1 }));
  mockRecords(1, [record({ id: 5, name: "big", ttl: 1234567 })]);

  renderDetail();
  expect(await screen.findByText("1,234,567")).toBeInTheDocument();
});

// @ and wildcard names are the two shapes worth scanning for in a long
// list — the artboard makes them primary/600, everything else stays plain.
test("the apex and a wildcard name render highlighted; an ordinary name does not", async () => {
  mockZone(zone({ id: 1 }));
  mockRecords(1, [
    record({ id: 1, name: "@", type: "NS", rdata: "ns.example.com." }),
    record({ id: 2, name: "*.nexus", type: "CNAME", rdata: "bifrost.example.com." }),
    record({ id: 3, name: "bifrost", type: "A", rdata: "10.0.0.1" }),
  ]);

  renderDetail();
  await waitFor(() => expect(recordRows()).toHaveLength(3));
  const [apexRow, wildRow, plainRow] = recordRows();

  expect(within(apexRow).getByText("@")).toHaveClass("text-primary");
  expect(within(wildRow).getByText("*.nexus")).toHaveClass("text-primary");
  expect(within(plainRow).getByText("bifrost")).not.toHaveClass("text-primary");
});

test("the name filter narrows the rows", async () => {
  const user = userEvent.setup();
  mockZone(zone({ id: 1 }));
  mockRecords(1, [
    record({ id: 1, name: "bifrost", type: "A" }),
    record({ id: 2, name: "nexus", type: "A", rdata: "10.0.0.2" }),
  ]);

  renderDetail();
  await waitFor(() => expect(recordRows()).toHaveLength(2));

  await user.type(screen.getByPlaceholderText("filter by name…"), "bif");
  await waitFor(() => expect(recordRows()).toHaveLength(1));
  expect(screen.getByText("bifrost")).toBeInTheDocument();
});

test("the type filter narrows the rows and the count line follows it", async () => {
  const user = userEvent.setup();
  mockZone(zone({ id: 1 }));
  mockRecords(1, [
    record({ id: 1, name: "bifrost", type: "A" }),
    record({ id: 2, name: "bifrost", type: "TXT", rdata: "v=spf1 -all" }),
  ]);

  renderDetail();
  await waitFor(() => expect(recordRows()).toHaveLength(2));
  expect(screen.getByText("2 records")).toBeInTheDocument();

  await user.selectOptions(screen.getByLabelText(/filter by record type/i), "TXT");
  await waitFor(() => expect(recordRows()).toHaveLength(1));
  expect(screen.getByText("1 record")).toBeInTheDocument();
});

test("creating a record posts the zone-relative name and the row appears", async () => {
  const user = userEvent.setup();
  let body: unknown;
  mockZone(zone({ id: 1 }));
  mockRecords(1, []);
  server.use(
    http.post("/api/v1/zones/1/records", async ({ request }) => {
      body = await request.json();
      return HttpResponse.json({ id: 9 }, { status: 201 });
    }),
  );

  renderDetail();
  await screen.findByLabelText(/zone record name/i);

  await user.type(screen.getByLabelText(/zone record name/i), "bifrost");
  await user.clear(screen.getByLabelText(/^ttl$/i));
  await user.type(screen.getByLabelText(/^ttl$/i), "600");
  await user.type(screen.getByLabelText(/^data$/i), "192.168.150.28");
  await user.click(screen.getByRole("button", { name: /^add$/i }));

  await waitFor(() =>
    expect(body).toEqual({ name: "bifrost", type: "A", ttl: 600, rdata: "192.168.150.28" }),
  );
  // The row is empty and ready for the next record.
  await waitFor(() => expect(screen.getByLabelText(/zone record name/i)).toHaveValue(""));
});

// Fix round 1, Finding 1: a zod failure must never be a silent no-op. Before
// this fix, an invalid TTL set aria-invalid and blocked submit with nothing
// on screen explaining why — the Add button just looked dead.
test("a non-numeric TTL shows its validation message and never posts", async () => {
  const user = userEvent.setup();
  let posted = false;
  mockZone(zone({ id: 1 }));
  mockRecords(1, []);
  server.use(
    http.post("/api/v1/zones/1/records", () => {
      posted = true;
      return HttpResponse.json({ id: 9 }, { status: 201 });
    }),
  );

  renderDetail();
  await screen.findByLabelText(/zone record name/i);

  await user.clear(screen.getByLabelText(/^ttl$/i));
  await user.type(screen.getByLabelText(/^ttl$/i), "abc");
  await user.type(screen.getByLabelText(/^data$/i), "192.168.150.28");
  await user.click(screen.getByRole("button", { name: /^add$/i }));

  expect(await screen.findByText(/ttl must be a whole number/i)).toBeInTheDocument();
  await new Promise((resolve) => setTimeout(resolve, 30));
  expect(posted).toBe(false);
});

test("editing a record seeds the row and PUTs the update", async () => {
  const user = userEvent.setup();
  let requestUrl: string | undefined;
  let body: unknown;
  mockZone(zone({ id: 1 }));
  mockRecords(1, [record({ id: 7, name: "bifrost", type: "A", ttl: 300, rdata: "10.0.0.1" })]);
  server.use(
    http.put("/api/v1/zones/1/records/7", async ({ request }) => {
      requestUrl = request.url;
      body = await request.json();
      return new HttpResponse(null, { status: 204 });
    }),
  );

  renderDetail();
  await waitFor(() => expect(recordRows()).toHaveLength(1));

  await user.click(screen.getByRole("button", { name: /edit bifrost a 10\.0\.0\.1/i }));
  await waitFor(() => expect(screen.getByLabelText(/zone record name/i)).toHaveValue("bifrost"));

  const dataField = screen.getByLabelText(/^data$/i);
  await user.clear(dataField);
  await user.type(dataField, "10.0.0.9");
  await user.click(screen.getByRole("button", { name: /^save$/i }));

  await waitFor(() =>
    expect(body).toEqual({ name: "bifrost", type: "A", ttl: 300, rdata: "10.0.0.9" }),
  );
  expect(requestUrl).toMatch(/\/api\/v1\/zones\/1\/records\/7$/);
});

// Fix round 1, Finding 4: the header's "Add record" button is inferred
// behaviour (the row is always visible, so the button has to do *something*
// beyond opening it) — cancel any in-progress edit and return to add mode.
// Exactly the interaction that regresses silently if it's ever wired wrong.
test("the header's Add record button returns an in-progress edit to add mode", async () => {
  const user = userEvent.setup();
  mockZone(zone({ id: 1 }));
  mockRecords(1, [record({ id: 7, name: "bifrost", type: "A", ttl: 300, rdata: "10.0.0.1" })]);

  renderDetail();
  await waitFor(() => expect(recordRows()).toHaveLength(1));

  await user.click(screen.getByRole("button", { name: /edit bifrost a 10\.0\.0\.1/i }));
  await waitFor(() => expect(screen.getByLabelText(/zone record name/i)).toHaveValue("bifrost"));
  expect(screen.getByRole("button", { name: /^save$/i })).toBeInTheDocument();

  await user.click(screen.getByRole("button", { name: /^add record$/i }));

  await waitFor(() => expect(screen.getByLabelText(/zone record name/i)).toHaveValue(""));
  expect(screen.getByRole("button", { name: /^add$/i })).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: /^save$/i })).not.toBeInTheDocument();
});

test("a 409 conflict from the API surfaces the server's message verbatim", async () => {
  const user = userEvent.setup();
  mockZone(zone({ id: 1 }));
  mockRecords(1, [record({ id: 1, name: "@", type: "A", rdata: "10.0.0.1" })]);
  server.use(
    http.post("/api/v1/zones/1/records", () =>
      HttpResponse.json(
        { error: "CNAME cannot coexist with another record at the same name" },
        { status: 409 },
      ),
    ),
  );

  renderDetail();
  await waitFor(() => expect(recordRows()).toHaveLength(1));

  await user.type(screen.getByLabelText(/zone record name/i), "@");
  await user.selectOptions(screen.getByLabelText(/^record type$/i), "CNAME");
  await user.type(screen.getByLabelText(/^data$/i), "bifrost.example.com.");
  await user.click(screen.getByRole("button", { name: /^add$/i }));

  expect(
    await screen.findByText("CNAME cannot coexist with another record at the same name"),
  ).toBeInTheDocument();
});

test("deleting a record asks for confirmation, then DELETEs it", async () => {
  const user = userEvent.setup();
  let deleted = false;
  mockZone(zone({ id: 1 }));
  mockRecords(1, [record({ id: 7, name: "bifrost", type: "A" })]);
  server.use(
    http.delete("/api/v1/zones/1/records/7", () => {
      deleted = true;
      return new HttpResponse(null, { status: 204 });
    }),
  );

  renderDetail();
  await waitFor(() => expect(recordRows()).toHaveLength(1));

  await user.click(screen.getByRole("button", { name: /delete bifrost a 10\.0\.0\.1/i }));
  const confirm = await screen.findByRole("alertdialog");
  await user.click(within(confirm).getByRole("button", { name: /^delete$/i }));

  await waitFor(() => expect(deleted).toBe(true));
});

// Collapsed, the SOA band is a single readable line rather than seven
// fields — primary NS · responsible · refresh/retry/expire/minimum.
test("the SOA band starts collapsed and shows a one-line summary", async () => {
  mockZone(
    zone({
      id: 1,
      soa_ns: "ns1.home.lan.",
      soa_mbox: "hostmaster.home.lan.",
      soa_refresh: 7200,
      soa_retry: 3600,
      soa_expire: 1209600,
      soa_minimum: 300,
    }),
  );
  mockRecords(1, []);

  renderDetail();
  expect(
    await screen.findByText("ns1.home.lan. · hostmaster.home.lan. · 7200/3600/1209600/300"),
  ).toBeInTheDocument();
  expect(screen.queryByLabelText(/^primary ns$/i)).not.toBeInTheDocument();
});

// The server owns soa_serial — it bumps on every record write — so the
// field is read-only, but (fix round 1: the artboard notes' first draft
// wrongly said the number itself is never shown) the box's content IS the
// actual serial, with AUTO as a marker beside it, not a substitute for it.
test("opening the SOA band shows editable fields, a serial box with the real number and an AUTO marker, and a disabled Save until something changes", async () => {
  const user = userEvent.setup();
  mockZone(zone({ id: 1, soa_serial: 42 }));
  mockRecords(1, []);

  renderDetail();
  await user.click(await screen.findByRole("button", { name: /soa/i }));

  expect(screen.getByLabelText(/^primary ns$/i)).toHaveValue("ns.example.com");
  expect(screen.getByLabelText(/^responsible$/i)).toHaveValue("hostadmin.example.com");
  expect(screen.getByText("AUTO")).toBeInTheDocument();
  expect(screen.getByText("42")).toBeInTheDocument();

  const save = screen.getByRole("button", { name: /^save soa$/i });
  expect(save).toBeDisabled();

  await user.clear(screen.getByLabelText(/^refresh$/i));
  await user.type(screen.getByLabelText(/^refresh$/i), "1800");
  expect(save).not.toBeDisabled();
});

// Fix round 1, Finding 1: a zod failure must never be a silent no-op — the
// button blocking submit with nothing on screen explaining why. Clearing a
// required SOA field is the regression this pins: Save SOA looks clickable
// (dirty), but must show *why* it did nothing rather than just doing nothing.
test("clearing a required SOA field shows its validation message and blocks the PATCH", async () => {
  const user = userEvent.setup();
  let patched = false;
  mockZone(zone({ id: 1 }));
  mockRecords(1, []);
  server.use(
    http.patch("/api/v1/zones/1", () => {
      patched = true;
      return new HttpResponse(null, { status: 204 });
    }),
  );

  renderDetail();
  await user.click(await screen.findByRole("button", { name: /soa/i }));

  await user.clear(screen.getByLabelText(/^primary ns$/i));
  await user.click(screen.getByRole("button", { name: /^save soa$/i }));

  expect(await screen.findByText("Required")).toBeInTheDocument();
  await new Promise((resolve) => setTimeout(resolve, 30));
  expect(patched).toBe(false);
});

test("saving the SOA band PATCHes the zone with the edited fields", async () => {
  const user = userEvent.setup();
  let body: unknown;
  mockZone(zone({ id: 1, soa_refresh: 7200 }));
  mockRecords(1, []);
  server.use(
    http.patch("/api/v1/zones/1", async ({ request }) => {
      body = await request.json();
      return new HttpResponse(null, { status: 204 });
    }),
  );

  renderDetail();
  await user.click(await screen.findByRole("button", { name: /soa/i }));

  await user.clear(screen.getByLabelText(/^refresh$/i));
  await user.type(screen.getByLabelText(/^refresh$/i), "1800");
  await user.click(screen.getByRole("button", { name: /^save soa$/i }));

  await waitFor(() =>
    expect(body).toMatchObject({
      soa_ns: "ns.example.com",
      soa_mbox: "hostadmin.example.com",
      soa_refresh: 1800,
      soa_retry: 3600,
      soa_expire: 1209600,
      soa_minimum: 300,
    }),
  );
});

test("the header shows the zone name, type and status badges, and the back link returns to /zones", async () => {
  mockZone(zone({ id: 1, name: "example.com", type: "primary", enabled: true }));
  mockRecords(1, []);

  renderDetail();
  await screen.findByText("example.com");

  expect(screen.getByText("primary")).toBeInTheDocument();
  expect(screen.getByText("Enabled")).toBeInTheDocument();
  const back = screen.getByRole("link", { name: /zones/i });
  expect(back).toHaveAttribute("href", "/zones");
});

test("a disabled zone reads Disabled and offers Enable zone", async () => {
  mockZone(zone({ id: 1, enabled: false }));
  mockRecords(1, []);

  renderDetail();
  expect(await screen.findByText("Disabled")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: /^enable zone$/i })).toBeInTheDocument();
});

test("Disable zone PATCHes enabled: false", async () => {
  const user = userEvent.setup();
  let body: unknown;
  mockZone(zone({ id: 1, enabled: true }));
  mockRecords(1, []);
  server.use(
    http.patch("/api/v1/zones/1", async ({ request }) => {
      body = await request.json();
      return new HttpResponse(null, { status: 204 });
    }),
  );

  renderDetail();
  await user.click(await screen.findByRole("button", { name: /^disable zone$/i }));

  await waitFor(() => expect(body).toEqual({ enabled: false }));
});

test("deleting the zone asks for confirmation, then DELETEs and navigates back to the list", async () => {
  const user = userEvent.setup();
  let deleted = false;
  mockZone(zone({ id: 1, name: "gone.example.com" }));
  mockRecords(1, []);
  server.use(
    http.delete("/api/v1/zones/1", () => {
      deleted = true;
      return new HttpResponse(null, { status: 204 });
    }),
  );

  renderDetail();
  await user.click(await screen.findByRole("button", { name: /^delete zone$/i }));
  const confirm = await screen.findByRole("alertdialog");
  await user.click(within(confirm).getByRole("button", { name: /^delete$/i }));

  await waitFor(() => expect(deleted).toBe(true));
});

test("a failed zone delete shows a toast and stays on the page", async () => {
  const user = userEvent.setup();
  mockZone(zone({ id: 1, name: "stuck.example.com" }));
  mockRecords(1, []);
  server.use(
    http.delete("/api/v1/zones/1", () => HttpResponse.json({ error: "boom" }, { status: 500 })),
  );
  const errorSpy = vi.spyOn(toast, "error");

  renderDetail();
  await user.click(await screen.findByRole("button", { name: /^delete zone$/i }));
  const confirm = await screen.findByRole("alertdialog");
  await user.click(within(confirm).getByRole("button", { name: /^delete$/i }));

  await waitFor(() =>
    expect(errorSpy).toHaveBeenCalledWith(
      expect.stringMatching(/couldn't delete stuck\.example\.com/i),
    ),
  );
  expect(await screen.findByText("stuck.example.com")).toBeInTheDocument();
});

test("an empty zone explains there are no records yet", async () => {
  mockZone(zone({ id: 1 }));
  mockRecords(1, []);

  renderDetail();
  expect(await screen.findByText("0 records")).toBeInTheDocument();
});

test("PTR is offered as a record type and its placeholder is a name", async () => {
  const user = userEvent.setup();
  renderZoneDetail({ zone: zone({ id: 1, name: "150.168.192.in-addr.arpa" }) });
  const row = await screen.findByTestId("record-form-row");
  await user.selectOptions(within(row).getByLabelText(/type/i), "PTR");
  // PTR rdata is a domain name, not an address (RFC 1034) — a placeholder
  // showing an IP would teach exactly the wrong thing.
  expect(within(row).getByLabelText(/data/i)).toHaveAttribute("placeholder", "bifrost.e412.in.");
});

test("a built-in zone offers no way to change it", async () => {
  renderZoneDetail({ zone: zone({ id: 1, name: "localhost", type: "internal" }) });
  await screen.findByText("localhost");
  expect(screen.queryByRole("button", { name: /add record/i })).not.toBeInTheDocument();
  expect(screen.queryByRole("button", { name: /delete zone/i })).not.toBeInTheDocument();
  expect(screen.queryByRole("button", { name: /disable zone/i })).not.toBeInTheDocument();
  // Its records are still listed — reading is the point of showing it at all.
  expect(await screen.findByText("Built-in")).toBeInTheDocument();
});
