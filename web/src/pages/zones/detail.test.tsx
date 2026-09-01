import { http, HttpResponse } from "msw";
import { toast } from "sonner";
import { afterEach, expect, test, vi } from "vitest";
import { act, fireEvent, screen, waitFor, within } from "@testing-library/react";
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
    last_error: "",
    last_attempt: 0,
    allow_transfer: "",
    last_xfr_at: 0,
    last_xfr_peer: "",
    last_xfr_error: "",
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

/**
 * The scrolling list's children in DOM order, each named by the slot it
 * carries: a plain record row, or the form that has taken one row's place.
 *
 * Order is the assertion these enable — an edited record's form has to sit
 * where its own row was, not above the list. The add band is deliberately
 * outside this container, so it never shows up here.
 */
function listSlots(): string[] {
  const list = screen.getByTestId("zone-record-list");
  return Array.from(
    list.querySelectorAll<HTMLElement>(
      '[data-testid="zone-record-row"], [data-testid="record-form-row"]',
    ),
  ).map((el) => el.dataset.testid ?? "");
}

/** Opens the create/edit row in add mode the way a user does — the row is
 * closed until the header's "Add record" button is clicked. */
async function openAddRow(user: ReturnType<typeof userEvent.setup>) {
  await user.click(await screen.findByRole("button", { name: /^add record$/i }));
  await screen.findByLabelText(/zone record name/i);
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

/**
 * Every path MSW served during the test, so a click can be proven to have
 * actually reached the network.
 *
 * Export needs this specifically: its whole effect is a browser download, so
 * there is nothing left in the DOM to assert on afterwards — the anchor is
 * created, clicked and removed. Asserting on that anchor instead would pass
 * just as happily if the request had never fired at all.
 *
 * Listeners are torn down in the afterEach below, alongside MSW's own
 * per-test handler reset in test/setup.ts.
 */
function trackFetchedPaths(): string[] {
  const paths: string[] = [];
  server.events.on("request:start", ({ request }) => {
    paths.push(new URL(request.url).pathname);
  });
  return paths;
}

afterEach(() => {
  vi.restoreAllMocks();
  vi.useRealTimers();
  server.events.removeAllListeners();
});

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

// The row is closed on load — no add/edit band until something opens it —
// and cannot be dismissed once open unless the X actually closes it. These
// three pin the bug directly, each a state the row-is-always-visible defect
// made impossible to reach.
test("the create/edit row is not in the document on load", async () => {
  mockZone(zone({ id: 1 }));
  mockRecords(1, [record({ id: 1, name: "bifrost" })]);

  renderDetail();
  await screen.findByText("bifrost");

  expect(screen.queryByTestId("record-form-row")).not.toBeInTheDocument();
});

test("Add record opens the row, empty and in add mode", async () => {
  const user = userEvent.setup();
  mockZone(zone({ id: 1 }));
  mockRecords(1, []);

  renderDetail();
  expect(screen.queryByTestId("record-form-row")).not.toBeInTheDocument();

  await openAddRow(user);

  expect(screen.getByTestId("record-form-row")).toBeInTheDocument();
  expect(screen.getByLabelText(/zone record name/i)).toHaveValue("");
  expect(screen.getByRole("button", { name: /^add$/i })).toBeInTheDocument();
});

test("the X closes the row", async () => {
  const user = userEvent.setup();
  mockZone(zone({ id: 1 }));
  mockRecords(1, []);

  renderDetail();
  await openAddRow(user);
  expect(screen.getByTestId("record-form-row")).toBeInTheDocument();

  await user.click(screen.getByRole("button", { name: /^close$/i }));

  expect(screen.queryByTestId("record-form-row")).not.toBeInTheDocument();
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
  await openAddRow(user);

  await user.selectOptions(screen.getByLabelText(/^record type$/i), "MX");
  await user.type(screen.getByLabelText(/^data$/i), "Mx");
  await user.click(screen.getByRole("button", { name: /^add$/i }));

  const message = await screen.findByText("dns: bad MX Mx");
  // The parser's own text, rendered mono — not rewritten into friendly prose.
  expect(message).toHaveClass("font-mono");
  expect(screen.getByLabelText(/^data$/i)).toHaveAttribute("aria-invalid", "true");

  // …and it renders *under Data*, which takes holding the three quiet
  // columns open: FormMessage renders null when its field has no error, so
  // without a cell of their own the row would have two children and grid
  // auto-placement would slide this message left under Name.
  const cell = message.parentElement;
  const errorRow = cell?.parentElement;
  expect(errorRow?.children).toHaveLength(5);
  expect(Array.from(errorRow?.children ?? []).indexOf(cell as Element)).toBe(3);
});

// One text input serves nine record types; the DNS parser (not a per-type
// form) is what actually validates the value, so the placeholder is the
// only per-type guidance the row gives.
test("the placeholder follows the selected type", async () => {
  const user = userEvent.setup();
  mockZone(zone({ id: 1 }));
  mockRecords(1, []);

  renderDetail();
  await openAddRow(user);

  expect(screen.getByPlaceholderText("192.168.150.28")).toBeInTheDocument();

  await user.selectOptions(screen.getByLabelText(/^record type$/i), "MX");
  expect(screen.getByPlaceholderText("10 mail.example.com.")).toBeInTheDocument();

  await user.selectOptions(screen.getByLabelText(/^record type$/i), "CAA");
  expect(screen.getByPlaceholderText('0 issue "letsencrypt.org"')).toBeInTheDocument();
});

// ── The Name and Data fields' suffix chips ────────────────────────────────
// Names are relative in the Name column and absolute in Data, and nothing
// on screen said so: a dotless CNAME target (the Cloudflare habit) is a
// valid record pointing somewhere else entirely. These chips are the whole
// fix — labels, not validation, so nothing below asserts on a warning.

test("the name field carries the zone's apex beside what's typed", async () => {
  const user = userEvent.setup();
  renderZoneDetail({ zone: zone({ id: 1, name: "e412.in" }) });
  await openAddRow(user);

  expect(screen.getByTestId("zone-name-suffix").textContent).toBe(".e412.in");
  // Informative, not part of the value — so it lands on the field's
  // description and leaves "Zone record name" as the accessible name.
  const field = screen.getByLabelText(/zone record name/i);
  expect(field).toHaveAccessibleName("Zone record name");
  expect(field).toHaveAccessibleDescription(".e412.in");
});

test("at @ the suffix drops its dot and the typed name goes muted", async () => {
  const user = userEvent.setup();
  renderZoneDetail({ zone: zone({ id: 1, name: "e412.in" }) });
  await openAddRow(user);

  const field = screen.getByLabelText(/zone record name/i);
  await user.type(field, "bifrost");
  expect(screen.getByTestId("zone-name-suffix").textContent).toBe(".e412.in");
  expect(field).not.toHaveClass("text-muted-foreground");

  // "@" is not a subdomain of the zone, it *is* the zone — so the joining
  // dot goes, and the typed text recedes behind the apex.
  await user.clear(field);
  await user.type(field, "@");
  expect(screen.getByTestId("zone-name-suffix").textContent).toBe("e412.in");
  expect(field).toHaveClass("text-muted-foreground");
  expect(field).toHaveAccessibleDescription("e412.in");
});

test("a long apex keeps its head and clips the tail, whole apex still reachable", async () => {
  const user = userEvent.setup();
  renderZoneDetail({ zone: zone({ id: 1, name: "150.168.192.in-addr.arpa" }) });
  await openAddRow(user);

  // Every IPv4 reverse zone ends in in-addr.arpa, so the tail is the shared
  // part and the leading octets are what say *which* zone this is. The clip
  // therefore keeps the head — the reverse of what CSS truncation does.
  const suffix = screen.getByTestId("zone-name-suffix");
  expect(suffix.textContent).toBe(".150.168.192…");
  expect(suffix.textContent).not.toContain("in-addr");
  // The 12-character cut lands exactly on a label boundary here, and a dot
  // sitting immediately before the ellipsis reads as a fourth, empty octet.
  expect(suffix.textContent).not.toContain(".…");
  // Clipped on screen, never lost: pointer gets `title`, everyone else gets
  // the field's description.
  expect(suffix).toHaveAttribute("title", ".150.168.192.in-addr.arpa");
  expect(screen.getByLabelText(/zone record name/i)).toHaveAccessibleDescription(
    ".150.168.192.in-addr.arpa",
  );
});

test("the data field is marked FULL NAME for a name-valued type only", async () => {
  const user = userEvent.setup();
  renderZoneDetail({ zone: zone({ id: 1, name: "e412.in" }) });
  await openAddRow(user);

  // A defaults to A: an address, so no chip.
  expect(screen.queryByTestId("record-data-suffix")).not.toBeInTheDocument();

  await user.selectOptions(screen.getByLabelText(/^record type$/i), "CNAME");
  expect(screen.getByTestId("record-data-suffix")).toHaveTextContent(/full name/i);
  expect(screen.getByLabelText(/^data$/i)).toHaveAccessibleDescription(/full name/i);

  await user.selectOptions(screen.getByLabelText(/^record type$/i), "TXT");
  expect(screen.queryByTestId("record-data-suffix")).not.toBeInTheDocument();
});

// Edit mode is the same band as add mode, so it gets the same chips —
// worth pinning because the row is bound to a record here, not blank.
test("editing a record shows the same suffix chips as adding one", async () => {
  const user = userEvent.setup();
  renderZoneDetail({
    zone: zone({ id: 1, name: "e412.in" }),
    records: [record({ id: 7, name: "git", type: "CNAME", rdata: "nas.e412.in." })],
  });

  await user.click(await screen.findByRole("button", { name: /^edit git cname/i }));
  await waitFor(() => expect(screen.getByLabelText(/zone record name/i)).toHaveValue("git"));

  expect(screen.getByTestId("zone-name-suffix").textContent).toBe(".e412.in");
  expect(screen.getByTestId("record-data-suffix")).toHaveTextContent(/full name/i);
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
  await openAddRow(user);

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
  await openAddRow(user);

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

// ── Editing happens in the record's own row ───────────────────────────────
// The pencil used to open the form as a band above the list, which left the
// record on screen twice — once in the band, once in its own row — and read
// as a duplicate. The row *is* the form now. Adding is unchanged: a new
// record has no row to become, so it stays a band at the top.

test("editing puts the form in the record's own row, and the record is not also listed", async () => {
  const user = userEvent.setup();
  mockZone(zone({ id: 1, name: "e412.in" }));
  mockRecords(1, [
    record({ id: 5, name: "alpha", type: "A", rdata: "10.0.0.1" }),
    record({ id: 7, name: "git", type: "CNAME", rdata: "nas.e412.in." }),
    record({ id: 9, name: "zulu", type: "A", rdata: "10.0.0.3" }),
  ]);

  renderDetail();
  await waitFor(() => expect(recordRows()).toHaveLength(3));

  await user.click(screen.getByRole("button", { name: /^edit git cname/i }));
  await waitFor(() => expect(screen.getByLabelText(/zone record name/i)).toHaveValue("git"));

  // In its own position, between the two rows that were its neighbours —
  // not above them, and not appended.
  expect(listSlots()).toEqual(["zone-record-row", "record-form-row", "zone-record-row"]);
  // …and exactly once. This is the reported bug: the record must not be both
  // the form and a plain row beneath it.
  expect(recordRows()).toHaveLength(2);
  for (const row of recordRows()) {
    expect(within(row).queryByText("git")).not.toBeInTheDocument();
  }
});

test("Save returns the edited row to a plain row", async () => {
  const user = userEvent.setup();
  mockZone(zone({ id: 1, name: "e412.in" }));
  mockRecords(1, [record({ id: 7, name: "git", type: "CNAME", rdata: "nas.e412.in." })]);
  server.use(http.put("/api/v1/zones/1/records/7", () => new HttpResponse(null, { status: 204 })));

  renderDetail();
  await waitFor(() => expect(recordRows()).toHaveLength(1));

  await user.click(screen.getByRole("button", { name: /^edit git cname/i }));
  await waitFor(() => expect(listSlots()).toEqual(["record-form-row"]));

  await user.click(screen.getByRole("button", { name: /^save$/i }));

  await waitFor(() => expect(listSlots()).toEqual(["zone-record-row"]));
  expect(screen.queryByTestId("record-form-row")).not.toBeInTheDocument();
});

test("Cancel returns the edited row to a plain row and writes nothing", async () => {
  const user = userEvent.setup();
  let put = false;
  mockZone(zone({ id: 1, name: "e412.in" }));
  mockRecords(1, [record({ id: 7, name: "git", type: "CNAME", rdata: "nas.e412.in." })]);
  server.use(
    http.put("/api/v1/zones/1/records/7", () => {
      put = true;
      return new HttpResponse(null, { status: 204 });
    }),
  );

  renderDetail();
  await waitFor(() => expect(recordRows()).toHaveLength(1));

  await user.click(screen.getByRole("button", { name: /^edit git cname/i }));
  await waitFor(() => expect(listSlots()).toEqual(["record-form-row"]));
  await user.type(screen.getByLabelText(/^data$/i), "typed-but-abandoned");

  await user.click(screen.getByRole("button", { name: /^cancel editing$/i }));

  expect(listSlots()).toEqual(["zone-record-row"]);
  expect(screen.queryByTestId("record-form-row")).not.toBeInTheDocument();
  await new Promise((resolve) => setTimeout(resolve, 30));
  expect(put).toBe(false);
});

// Two live forms would put two "Zone record name" fields in the document,
// which breaks every selector that reaches for one and is incoherent besides
// — there is one row being written at a time.
test("opening an edit closes the add band", async () => {
  const user = userEvent.setup();
  mockZone(zone({ id: 1, name: "e412.in" }));
  mockRecords(1, [record({ id: 7, name: "git", type: "CNAME", rdata: "nas.e412.in." })]);

  renderDetail();
  await waitFor(() => expect(recordRows()).toHaveLength(1));
  await openAddRow(user);
  await user.type(screen.getByLabelText(/zone record name/i), "draft");

  await user.click(screen.getByRole("button", { name: /^edit git cname/i }));

  expect(screen.getAllByTestId("record-form-row")).toHaveLength(1);
  expect(screen.getAllByLabelText(/zone record name/i)).toHaveLength(1);
  // The one that survives is the row's, seeded from the record.
  expect(listSlots()).toEqual(["record-form-row"]);
  await waitFor(() => expect(screen.getByLabelText(/zone record name/i)).toHaveValue("git"));
});

// Fix round 1, Finding 4: opening the row is the header's "Add record"
// button's obvious job now, but this pins the case that's still easy to get
// wrong — clicking it while a record is being edited must return to add mode
// (blank, focused on Name), not leave the in-progress edit sitting there
// under an "Add record" label. The edit now lives in the record's own row
// rather than a band, so that row also has to go back to being a plain row.
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
  // Still exactly one form, and the record is a plain row again.
  expect(screen.getAllByTestId("record-form-row")).toHaveLength(1);
  expect(listSlots()).toEqual(["zone-record-row"]);
});

// One row at a time. The other rows keep their pencil and trash on screen —
// removing them would make the list jump — but they are genuinely inert, not
// just faded: `disabled` rather than pointer-events alone, so a keyboard
// press cannot reach them either.
test("while a record is being edited the other rows' actions are inert", async () => {
  const user = userEvent.setup();
  mockZone(zone({ id: 1, name: "e412.in" }));
  mockRecords(1, [
    record({ id: 7, name: "git", type: "CNAME", rdata: "nas.e412.in." }),
    record({ id: 9, name: "zulu", type: "A", rdata: "10.0.0.3" }),
  ]);

  renderDetail();
  await waitFor(() => expect(recordRows()).toHaveLength(2));
  expect(screen.getByRole("button", { name: /^edit zulu a/i })).not.toBeDisabled();

  await user.click(screen.getByRole("button", { name: /^edit git cname/i }));
  await waitFor(() => expect(listSlots()).toEqual(["record-form-row", "zone-record-row"]));

  expect(screen.getByRole("button", { name: /^edit zulu a/i })).toBeDisabled();
  expect(screen.getByRole("button", { name: /^delete zulu a/i })).toBeDisabled();
});

// ── Touching a filter closes an in-progress edit ──────────────────────────
// Unconditionally, and whichever filter it is. A rule that closed the edit
// only when the edited row stopped matching would make an admin work out
// which case they were in before knowing whether their typing survived;
// "touch a filter, the edit closes" needs no reasoning. The discard is
// silent — the row was right there, and a confirm on a keystroke would be
// worse than the thing it guards.

test("typing in the name filter closes an in-progress edit, even when the row still matches", async () => {
  const user = userEvent.setup();
  mockZone(zone({ id: 1, name: "e412.in" }));
  mockRecords(1, [
    record({ id: 7, name: "git", type: "CNAME", rdata: "nas.e412.in." }),
    record({ id: 9, name: "zulu", type: "A", rdata: "10.0.0.3" }),
  ]);

  renderDetail();
  await waitFor(() => expect(recordRows()).toHaveLength(2));

  await user.click(screen.getByRole("button", { name: /^edit git cname/i }));
  await waitFor(() => expect(listSlots()).toEqual(["record-form-row", "zone-record-row"]));
  await user.type(screen.getByLabelText(/^data$/i), "typed-but-abandoned");

  // "g" still matches "git" — this is the half that distinguishes the rule
  // from "close it if the row stops matching". The edit closes anyway.
  await user.type(screen.getByPlaceholderText("filter by name…"), "g");

  await waitFor(() => expect(screen.queryByTestId("record-form-row")).not.toBeInTheDocument());
  // The row is still listed, as a plain row: it matched the filter all along.
  expect(listSlots()).toEqual(["zone-record-row"]);
  expect(screen.getByText("1 record")).toBeInTheDocument();
  expect(screen.getByText("git")).toBeInTheDocument();
});

test("changing the type filter closes an in-progress edit", async () => {
  const user = userEvent.setup();
  mockZone(zone({ id: 1, name: "e412.in" }));
  mockRecords(1, [
    record({ id: 7, name: "git", type: "CNAME", rdata: "nas.e412.in." }),
    record({ id: 9, name: "zulu", type: "A", rdata: "10.0.0.3" }),
  ]);

  renderDetail();
  await waitFor(() => expect(recordRows()).toHaveLength(2));

  await user.click(screen.getByRole("button", { name: /^edit git cname/i }));
  await waitFor(() => expect(listSlots()).toEqual(["record-form-row", "zone-record-row"]));

  // git is a CNAME, so its row survives this filter. The edit does not.
  await user.selectOptions(screen.getByLabelText(/filter by record type/i), "CNAME");

  await waitFor(() => expect(screen.queryByTestId("record-form-row")).not.toBeInTheDocument());
  expect(listSlots()).toEqual(["zone-record-row"]);
});

// The other half of the rule: the add band is not a record and has nothing
// to match, so filtering a list of records says nothing about it. It stays,
// and keeps what has been typed into it.
test("a filter change leaves the add band open and untouched", async () => {
  const user = userEvent.setup();
  mockZone(zone({ id: 1, name: "e412.in" }));
  mockRecords(1, [
    record({ id: 7, name: "git", type: "CNAME", rdata: "nas.e412.in." }),
    record({ id: 9, name: "zulu", type: "A", rdata: "10.0.0.3" }),
  ]);

  renderDetail();
  await waitFor(() => expect(recordRows()).toHaveLength(2));
  await openAddRow(user);
  await user.type(screen.getByLabelText(/zone record name/i), "draft");

  await user.type(screen.getByPlaceholderText("filter by name…"), "zulu");
  await waitFor(() => expect(listSlots()).toEqual(["zone-record-row"]));

  expect(screen.getByTestId("record-form-row")).toBeInTheDocument();
  expect(screen.getByLabelText(/zone record name/i)).toHaveValue("draft");
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
  await openAddRow(user);

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
  await openAddRow(user);
  const row = screen.getByTestId("record-form-row");
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
  expect(await screen.findByText(/built-in · read-only/i)).toBeInTheDocument();
  // §9.5.3 refuses a transfer of a built-in zone with NOTAUTH regardless of
  // its ACL, and handleZonePatch 409s every PATCH to one outright — so the
  // allow-transfer row, whose only content is an editable field with a
  // working-looking Save, must not be offered here at all, on the same
  // terms as Add record/Delete zone/Disable zone above.
  expect(screen.queryByRole("button", { name: /^edit allow transfer$/i })).not.toBeInTheDocument();
  expect(screen.queryByLabelText(/^allow transfer$/i)).not.toBeInTheDocument();
});

// ── Zone file export and import ───────────────────────────────────────────

const ZONE_FILE = "$ORIGIN e412.in.\n@ 300 IN A 192.168.150.2\n";

function zoneFile(name = "e412.in.zone"): File {
  return new File([ZONE_FILE], name, { type: "text/dns" });
}

/** Opens the import dialog the way a user does: press Import, then choose a
 * file. The dialog opens on selection, before the dry run has answered, so
 * callers still wait on whichever state they expect it to settle into. */
async function chooseFile(user: ReturnType<typeof userEvent.setup>, file = zoneFile()) {
  await user.click(await screen.findByRole("button", { name: /^import$/i }));
  await user.upload(screen.getByLabelText("Zone file"), file);
}

// Named for what it observes, not for the whole feature: this reaches as far
// as the response body arriving at the download path. What downloadBlob then
// does with it — the anchor, its href and download attributes, and the click
// that is the actual mechanism — is pinned in lib/download.test.ts.
test("Export fetches the zone file and passes the response body to the download", async () => {
  const user = userEvent.setup();
  const fetchedPaths = trackFetchedPaths();
  const objectUrl = vi.spyOn(URL, "createObjectURL");
  server.use(
    http.get(
      "/api/v1/zones/1/file",
      () =>
        new HttpResponse(ZONE_FILE, {
          headers: {
            "Content-Type": "text/dns; charset=utf-8",
            "Content-Disposition": 'attachment; filename="e412.in.zone"',
          },
        }),
    ),
  );

  renderZoneDetail({ zone: zone({ id: 1, name: "e412.in" }) });
  await user.click(await screen.findByRole("button", { name: /^export$/i }));

  // The click must reach the endpoint — see trackFetchedPaths' own comment.
  await waitFor(() => expect(fetchedPaths).toContain("/api/v1/zones/1/file"));
  // …and the body it answered with must reach the download, rather than the
  // request firing and its response being quietly dropped.
  await waitFor(() => expect(objectUrl).toHaveBeenCalledWith(expect.any(Blob)));
});

test("Import shows the diff and writes nothing until Apply", async () => {
  const user = userEvent.setup();
  const posted: { content: string; dry_run: boolean }[] = [];
  server.use(
    http.post("/api/v1/zones/1/file", async ({ request }) => {
      const body = (await request.json()) as { content: string; dry_run: boolean };
      posted.push(body);
      return HttpResponse.json({
        add: [record({ id: 0, name: "grafana", rdata: "192.168.150.44" })],
        change: [
          {
            from: record({ id: 2, name: "nas", ttl: 300 }),
            to: record({ id: 2, name: "nas", ttl: 600 }),
          },
        ],
        delete: [record({ id: 3, name: "printer", rdata: "192.168.150.31" })],
        errors: [],
      });
    }),
  );

  renderZoneDetail({ zone: zone({ id: 1, name: "e412.in" }) });
  await chooseFile(user);

  const dialog = await screen.findByRole("dialog");
  expect(within(dialog).getByText("+1 added")).toBeInTheDocument();
  expect(within(dialog).getByText("~1 changed")).toBeInTheDocument();
  expect(within(dialog).getByText("−1 deleted")).toBeInTheDocument();
  // The destructive half of a whole-zone replace is the point of the dry
  // run, so it is named in the footer rather than left to be inferred.
  expect(within(dialog).getByText(/anything not in the file is deleted/i)).toBeInTheDocument();

  // Nothing is written by looking: every request so far was a dry run.
  expect(posted).toHaveLength(1);
  expect(posted[0]).toEqual({ content: ZONE_FILE, dry_run: true });
});

test("Apply commits the same file with dry_run false", async () => {
  const user = userEvent.setup();
  const posted: { content: string; dry_run: boolean }[] = [];
  server.use(
    http.post("/api/v1/zones/1/file", async ({ request }) => {
      const body = (await request.json()) as { content: string; dry_run: boolean };
      posted.push(body);
      return HttpResponse.json({
        add: [record({ id: 0, name: "grafana", rdata: "192.168.150.44" })],
        change: [],
        delete: [],
        errors: [],
      });
    }),
  );
  const success = vi.spyOn(toast, "success");

  renderZoneDetail({ zone: zone({ id: 1, name: "e412.in" }) });
  await chooseFile(user);
  const dialog = await screen.findByRole("dialog");
  await user.click(within(dialog).getByRole("button", { name: /^apply$/i }));

  await waitFor(() => expect(posted).toHaveLength(2));
  // The same bytes, committed — the server re-reads the file rather than
  // being handed a diff to replay.
  expect(posted[1]).toEqual({ content: ZONE_FILE, dry_run: false });
  await waitFor(() => expect(success).toHaveBeenCalledWith(expect.stringMatching(/1 added/)));
});

test("a rejected import shows every problem the server named, verbatim", async () => {
  const user = userEvent.setup();
  let committed = false;
  server.use(
    http.post("/api/v1/zones/1/file", async ({ request }) => {
      const body = (await request.json()) as { dry_run: boolean };
      if (!body.dry_run) committed = true;
      return HttpResponse.json(
        {
          add: [],
          change: [],
          delete: [],
          errors: [
            "line 5: records in the same RRSet must share one TTL",
            "line 7: CNAME cannot coexist with another record at the same name",
            // A file-wide problem: it names neither a line nor a record, so
            // anything that parsed a "line N:" prefix off these would drop it.
            "the file has no SOA record",
          ],
          error:
            "the zone file was rejected; nothing was written. See errors for every problem found.",
        },
        { status: 422 },
      );
    }),
  );

  renderZoneDetail({ zone: zone({ id: 1, name: "e412.in" }) });
  await chooseFile(user, zoneFile("bad.zone"));

  expect(await screen.findByText(/file rejected — nothing was written/i)).toBeInTheDocument();
  expect(
    screen.getByText("line 5: records in the same RRSet must share one TTL"),
  ).toBeInTheDocument();
  expect(
    screen.getByText("line 7: CNAME cannot coexist with another record at the same name"),
  ).toBeInTheDocument();
  expect(screen.getByText("the file has no SOA record")).toBeInTheDocument();
  expect(committed).toBe(false);
});

// Re-importing a file that was just exported is the ordinary way to reach an
// empty diff, and three empty groups under three zeroes would read as a
// failure to load rather than as "nothing would change".
test("a file identical to the zone says so instead of showing an empty diff", async () => {
  const user = userEvent.setup();
  server.use(
    http.post("/api/v1/zones/1/file", () =>
      HttpResponse.json({ add: [], change: [], delete: [], errors: [] }),
    ),
  );

  renderZoneDetail({ zone: zone({ id: 1, name: "e412.in" }) });
  await chooseFile(user);

  expect(await screen.findByText(/this file matches the zone/i)).toBeInTheDocument();
});

// The dry run is not instant: the server re-parses and re-diffs the whole
// file, which on a large zone is seconds of work (measured on a real build:
// ~1s at 10k records, ~5.5s at 33k). Opening the dialog only once it answers
// left the page looking frozen for that whole time, and the Import button
// live enough to fire a second full-cost request on top of the first.
test("a dry run still in flight shows progress and cannot be fired twice", async () => {
  const user = userEvent.setup();
  const posted: unknown[] = [];
  let release!: () => void;
  const held = new Promise<void>((resolve) => {
    release = resolve;
  });
  server.use(
    http.post("/api/v1/zones/1/file", async ({ request }) => {
      posted.push(await request.json());
      await held;
      return HttpResponse.json({ add: [], change: [], delete: [], errors: [] });
    }),
  );

  renderZoneDetail({ zone: zone({ id: 1, name: "e412.in" }) });
  await chooseFile(user);

  // The wait is on screen from the moment the file is chosen, rather than
  // the page sitting inert until the answer arrives.
  expect(await screen.findByText(/checking what this file would change/i)).toBeInTheDocument();
  // `hidden: true` because the open dialog takes the rest of the page out of
  // the accessibility tree — the button is still there, and still has to be
  // disabled, or a second click buys a second full-cost parse of the file.
  expect(screen.getByRole("button", { name: /^import$/i, hidden: true })).toBeDisabled();

  // The button is only the visible affordance; the input is what actually
  // starts the request, so that is where the guard has to hold. Fired with
  // fireEvent rather than userEvent deliberately: it dispatches the change
  // event straight at the input, ignoring the `disabled` attribute the way a
  // programmatic .click() or a future change to the dialog's focus handling
  // could. Exactly one request must ever leave.
  // Exact, not /zone file/i: the open dialog is itself labelled "Import
  // zone file", so a loose match now finds two elements.
  const input = screen.getByLabelText("Zone file");
  expect(input).toBeDisabled();
  fireEvent.change(input, { target: { files: [zoneFile("second.zone")] } });
  expect(posted).toHaveLength(1);

  release();
  expect(await screen.findByText(/this file matches the zone/i)).toBeInTheDocument();
  expect(posted).toHaveLength(1);
});

// An over-cap body is a 413 carrying only the flat `{"error": …}` envelope —
// no `errors` array to render — so this is the path where zoneFileErrors'
// fallback to the summary is what keeps the dialog from coming up blank.
// The message is the server's own (internal/api/zonefile_handlers.go).
test("an oversize file is reported as too large, in the rejected state", async () => {
  const user = userEvent.setup();
  server.use(
    http.post("/api/v1/zones/1/file", () =>
      HttpResponse.json(
        { error: "request body too large: the limit is 1048576 bytes" },
        { status: 413 },
      ),
    ),
  );

  renderZoneDetail({ zone: zone({ id: 1, name: "e412.in" }) });
  await chooseFile(user);

  expect(await screen.findByText(/file rejected — nothing was written/i)).toBeInTheDocument();
  expect(
    screen.getByText("request body too large: the limit is 1048576 bytes"),
  ).toBeInTheDocument();
});

test("a built-in zone offers no Import, but still exports", async () => {
  renderZoneDetail({ zone: zone({ id: 1, name: "localhost", type: "internal" }) });
  await screen.findByText("localhost");

  expect(screen.queryByRole("button", { name: /^import$/i })).not.toBeInTheDocument();
  // Export stays — the server refuses writes to a built-in zone, not reads.
  expect(screen.getByRole("button", { name: /^export$/i })).toBeInTheDocument();
});

// ── A secondary zone ────────────────────────────────────────────────────────
// A copy of someone else's zone, and the page has to say so in three places:
// the header (what it is and whether it can answer), the transfer band (where
// it comes from and how current it is) and the record list (which is not
// yours to edit).

/** A healthy secondary: transferred recently, well inside its expiry. */
function secondary(overrides: Partial<Zone> = {}): Zone {
  return zone({
    id: 1,
    name: "e412.in",
    type: "secondary",
    primaries: "203.0.113.9, ns2.example.net:5353",
    // 7h refresh against a transfer 2h ago: comfortably inside the schedule,
    // so the state is "fresh" rather than sitting on the boundary.
    soa_refresh: 25200,
    soa_retry: 3600,
    refreshed_at: Date.now() - 2 * 3600_000,
    expires_at: Date.now() + 1209600_000,
    ...overrides,
  });
}

// All four record-write routes answer 409 for a secondary, so every control
// that would produce one is absent rather than left to fail on click. Import
// is absent for the same reason and one more: the next transfer would replace
// whatever it wrote.
test("a secondary's records are read-only — no Add record, no row actions, no Import", async () => {
  renderZoneDetail({ zone: secondary(), records: [record({ id: 1, name: "bifrost" })] });
  await screen.findByText("e412.in");

  expect(screen.queryByRole("button", { name: /add record/i })).not.toBeInTheDocument();
  expect(screen.queryByRole("button", { name: /^import$/i })).not.toBeInTheDocument();
  const rows = recordRows();
  expect(rows).toHaveLength(1);
  expect(within(rows[0]).queryByRole("button", { name: /^edit/i })).not.toBeInTheDocument();
  expect(within(rows[0]).queryByRole("button", { name: /^delete/i })).not.toBeInTheDocument();
  // Export stays: reading a secondary is allowed.
  expect(screen.getByRole("button", { name: /^export$/i })).toBeInTheDocument();
  expect(screen.getByText(/pulled · read-only/i)).toBeInTheDocument();
});

// A secondary's SOA is its primary's, replaced wholesale by every transfer.
// An editable SOA form here would offer to write values the next transfer
// silently discards.
test("a secondary shows the transfer band in place of the SOA form", async () => {
  renderZoneDetail({ zone: secondary() });
  await screen.findByText("e412.in");

  expect(screen.queryByRole("button", { name: /save soa/i })).not.toBeInTheDocument();
  expect(screen.getByText("Primaries")).toBeInTheDocument();
  expect(screen.getByText("203.0.113.9, ns2.example.net:5353")).toBeInTheDocument();
  expect(screen.getByText("Last refresh")).toBeInTheDocument();
  expect(screen.getByText("2h ago")).toBeInTheDocument();
  // A 7h refresh against a transfer 2h ago: the next one is about 5h out.
  // Matched loosely because the fixture's "now" and the render's are a few
  // milliseconds apart, which is enough to move 5h to 4h 59m.
  expect(screen.getByText(/^in [45]h/)).toBeInTheDocument();
});

// The select that attaches a key is valued by id; this is the other end of
// the same mapping — the band has to turn that id back into the name the peer
// knows the key by.
test("the transfer band names the TSIG key, resolving it from its id", async () => {
  server.use(
    http.get("/api/v1/tsig-keys", () =>
      HttpResponse.json([
        {
          id: 4,
          name: "xfer.e412.in.",
          algorithm: "hmac-sha256.",
          secret: "Sh5ZuulpjcmcJuN6VwMQCVEhTJyUmlPTSHexvePtaWo=",
          created_at: Date.now(),
        },
      ]),
    ),
  );
  renderZoneDetail({ zone: secondary({ tsig_key_id: 4 }) });
  await screen.findByText("e412.in");

  expect(await screen.findByText("xfer.e412.in.")).toBeInTheDocument();
});

test("a secondary with no TSIG key says none rather than showing a zero", async () => {
  renderZoneDetail({ zone: secondary({ tsig_key_id: 0 }) });
  await screen.findByText("e412.in");

  expect(screen.getByText("none")).toBeInTheDocument();
});

// The badge an `enabled` flag alone gets most wrong: this zone is enabled in
// the database and answering SERVFAIL for its whole suffix, because past
// expires_at it can no longer vouch for what it holds (Zone.Serving).
test("an expired secondary reads Not answering, not Enabled", async () => {
  renderZoneDetail({
    zone: secondary({
      enabled: true,
      refreshed_at: Date.now() - 17 * 86_400_000,
      expires_at: Date.now() - 1000,
      last_error: "transfer refused by server",
      last_attempt: Date.now() - 5 * 60_000,
    }),
  });
  await screen.findByText("e412.in");

  expect(screen.getByText("Not answering")).toBeInTheDocument();
  expect(screen.queryByText("Enabled")).not.toBeInTheDocument();
  expect(screen.getByText("Answering nothing until a transfer succeeds.")).toBeInTheDocument();
});

// The whole reason last_error is a column. It arrives with the zone, so it
// reads the same after a restart as before one — and it is shown verbatim,
// because the resolver's own words are the only actionable part. The band
// leads with the failure and prints the whole message under it; neither is a
// rewrite of what the server said.
test("the last transfer error is shown verbatim, with when it happened", async () => {
  renderZoneDetail({
    zone: secondary({
      last_error: "203.0.113.9:53: dial tcp: connect: connection refused",
      last_attempt: Date.now() - 4 * 60_000,
    }),
  });
  await screen.findByText("e412.in");

  // Just the failure, with the note beside it as its own element rather than
  // concatenated onto the end of it.
  expect((await screen.findByTestId("transfer-error")).textContent).toBe("connection refused");
  expect(screen.getByTestId("transfer-error-raw")).toHaveTextContent(
    "203.0.113.9:53: dial tcp: connect: connection refused",
  );
  // Never without its date: the same words mean different things four
  // minutes and four days after the fact.
  expect(screen.getByText(/last attempt · 4m ago/i)).toBeInTheDocument();
  expect(screen.getByTestId("transfer-note").textContent).toBe(
    "Still serving the copy from 2h ago.",
  );
});

// A message with no context wrapped around it is not printed twice: the lead
// already is the whole of it.
test("an error with nothing to lead out of is shown once, not on two lines", async () => {
  renderZoneDetail({
    zone: secondary({
      last_error: "transfer refused by server",
      last_attempt: Date.now() - 4 * 60_000,
    }),
  });
  await screen.findByText("e412.in");

  expect(await screen.findByTestId("transfer-error")).toHaveTextContent(
    "transfer refused by server",
  );
  expect(screen.queryByTestId("transfer-error-raw")).not.toBeInTheDocument();
});

// The defect this band had: the error and the neutral note were siblings on
// one flex row with nothing separating them, so a long error ran straight
// into "Still serving the copy from …" and pushed it off the end. They are
// two elements now, and the note is the one that never gives way.
test("a long transfer error does not run into the note or crowd it out", async () => {
  const longError = `zone "e412.in": every primary failed: ${"203.0.113.9:53: dial tcp 203.0.113.9:53: no route to host: ".repeat(8)}connection refused`;
  renderZoneDetail({
    zone: secondary({ last_error: longError, last_attempt: Date.now() - 4 * 60_000 }),
  });
  await screen.findByText("e412.in");

  const note = await screen.findByTestId("transfer-note");
  expect(note.textContent).toBe("Still serving the copy from 2h ago.");

  // The lead is short whatever the message's length, so the actionable part
  // survives however little of the line is drawn — and it is only the error,
  // with none of the note run into the end of it.
  const lead = screen.getByTestId("transfer-error");
  expect(lead.textContent).toBe("connection refused");

  // And the whole message is still on screen, on its own line, in full.
  expect(screen.getByTestId("transfer-error-raw")).toHaveTextContent(longError);
});

test("a healthy secondary shows no error line at all", async () => {
  renderZoneDetail({ zone: secondary() });
  await screen.findByText("e412.in");

  expect(screen.queryByTestId("transfer-error")).not.toBeInTheDocument();
});

test("Refresh now transfers the zone and reports what arrived", async () => {
  const user = userEvent.setup();
  const successSpy = vi.spyOn(toast, "success");
  renderZoneDetail({ zone: secondary() });
  await screen.findByText("e412.in");
  server.use(
    http.post("/api/v1/zones/1/refresh", () =>
      HttpResponse.json({
        primary: "203.0.113.9:53",
        serial: 2026080601,
        records: 17,
        refreshed_at: Date.now(),
        expires_at: Date.now() + 1209600_000,
      }),
    ),
  );

  await user.click(screen.getByRole("button", { name: /refresh now/i }));

  await waitFor(() =>
    expect(successSpy).toHaveBeenCalledWith("Transferred 17 records from 203.0.113.9:53"),
  );
});

// The failure is not kept in component state: the server wrote it to the zone
// row, the mutation refetches either way, and the band renders what came
// back. That is what makes it survive a reload instead of vanishing with the
// component.
test("a failed Refresh now leaves the server's reason in the band, from the refetched zone", async () => {
  const user = userEvent.setup();
  let refreshed = false;
  server.use(
    http.get("/api/v1/zones/1", () =>
      HttpResponse.json(
        refreshed
          ? secondary({
              last_error: "203.0.113.9:53: dial tcp: connect: connection refused",
              last_attempt: Date.now(),
            })
          : secondary(),
      ),
    ),
    http.post("/api/v1/zones/1/refresh", () => {
      refreshed = true;
      return HttpResponse.json(
        { error: "203.0.113.9:53: dial tcp: connect: connection refused" },
        { status: 502 },
      );
    }),
  );
  mockRecords(1, []);
  renderDetail("/zones/1");
  await screen.findByText("e412.in");
  expect(screen.queryByTestId("transfer-error")).not.toBeInTheDocument();

  await user.click(screen.getByRole("button", { name: /refresh now/i }));

  expect(await screen.findByTestId("transfer-error")).toHaveTextContent("connection refused");
  expect(screen.getByTestId("transfer-error-raw")).toHaveTextContent(
    "203.0.113.9:53: dial tcp: connect: connection refused",
  );
});

test("a primary zone shows the SOA form and no transfer band", async () => {
  renderZoneDetail({ zone: zone({ id: 1, name: "e412.in", type: "primary" }) });
  await screen.findByText("e412.in");

  expect(screen.getByRole("button", { name: /add record/i })).toBeInTheDocument();
  expect(screen.queryByText("Primaries")).not.toBeInTheDocument();
  expect(screen.queryByText("Next refresh")).not.toBeInTheDocument();
});

// The state the band had no opinion about, and so contradicted the header
// about. A disabled zone is skipped by the scheduler outright (RefreshDue) and
// skipped again at answer time (Index.Find), so "retrying every 1h" and "still
// serving the copy from …" are both false — while the badge two rows above
// says Disabled.
test("a disabled secondary does not claim to be retrying or serving", async () => {
  renderZoneDetail({
    zone: secondary({
      enabled: false,
      soa_retry: 3600,
      refreshed_at: Date.now() - 9 * 3600_000,
    }),
  });
  await screen.findByText("e412.in");

  expect(screen.getByText("Disabled")).toBeInTheDocument();
  expect(screen.queryByText(/retrying every/i)).not.toBeInTheDocument();
  expect(screen.queryByText(/still serving the copy/i)).not.toBeInTheDocument();
  expect(screen.getByText("not while disabled")).toBeInTheDocument();
  // And it must not claim to answer nothing either. A disabled zone is
  // skipped by Index.Find, so names under it are *forwarded* — the zone stops
  // being consulted rather than starting to refuse. Saying "answers nothing"
  // would describe the one behaviour it does not have, and would hide the
  // thing worth knowing: an internal name is now resolved by a public server.
  expect(screen.queryByText(/answers nothing/i)).not.toBeInTheDocument();
  expect(screen.getByText(/forwarded upstream/i)).toBeInTheDocument();
});

// A disabled zone that had failed before it was switched off keeps that
// failure on screen — it is dated, and it is still the last thing that
// happened — but the consequence beside it is the switch, not the failure.
test("a disabled secondary keeps its recorded failure but not the failure's consequence", async () => {
  renderZoneDetail({
    zone: secondary({
      enabled: false,
      last_error: "203.0.113.9:53: connection refused",
      last_attempt: Date.now() - 4 * 60_000,
    }),
  });
  await screen.findByText("e412.in");

  expect(await screen.findByTestId("transfer-error")).toHaveTextContent("connection refused");
  expect(screen.queryByText(/still serving the copy/i)).not.toBeInTheDocument();
  expect(screen.getByText(/forwarded upstream/i)).toBeInTheDocument();
});

// `overdue` is the state added beyond the design boards: the refresh deadline
// passed with no failure recorded against it, so nothing claims to have tried.
// An enabled zone reaches it when the process has only just started.
test("an enabled secondary past its deadline with nothing recorded reads Overdue", async () => {
  renderZoneDetail({
    zone: secondary({
      soa_refresh: 7200,
      soa_retry: 3600,
      refreshed_at: Date.now() - 5 * 3600_000,
      last_attempt: Date.now() - 5 * 3600_000,
    }),
  });
  await screen.findByText("e412.in");

  expect(screen.getByText("Overdue")).toBeInTheDocument();
  // The exact moment of the next attempt lives in the scheduler's in-memory
  // back-off, so the band names the interval instead of guessing a time.
  expect(screen.getByText("retrying every 1h")).toBeInTheDocument();
  expect(screen.getByText(/still serving the copy from 5h ago/i)).toBeInTheDocument();
  // Nothing recorded a reason, so none is invented.
  expect(screen.queryByTestId("transfer-error")).not.toBeInTheDocument();
});

// ── Keeping up with the scheduler ───────────────────────────────────────────
//
// This page is the one place in the app whose subject changes with nobody
// touching it: the scheduler transfers on the SOA's refresh, retries when a
// primary comes back, and lets an unreachable zone expire. So the zone query
// polls — but only while the zone is a secondary, and the record list never
// does. See use-zones.ts's watchesTransfers and useRecordsFollowTransfers.

test("a failing secondary recovers on screen, with nobody touching the page", async () => {
  vi.useFakeTimers({ shouldAdvanceTime: true });
  // One transfer stamp across both, so this test turns on the poll alone and
  // nothing else follows from the zone changing.
  const refreshedAt = Date.now() - 2 * 3600_000;
  const failingZone = secondary({
    refreshed_at: refreshedAt,
    last_error: "203.0.113.9:53: dial tcp: connect: connection refused",
    last_attempt: Date.now(),
  });
  const healthyZone = secondary({ refreshed_at: refreshedAt });
  let failing = true;
  server.use(
    http.get("/api/v1/zones/1", () => HttpResponse.json(failing ? failingZone : healthyZone)),
  );
  mockRecords(1, [record({ id: 1, name: "bifrost" })]);

  renderDetail("/zones/1");
  expect(await screen.findByTestId("transfer-error")).toHaveTextContent("connection refused");

  // The primary came back and the scheduler's next attempt succeeded. This
  // page was told nothing and nobody clicked anything.
  failing = false;
  await act(async () => {
    await vi.advanceTimersByTimeAsync(30_000);
  });

  await waitFor(() => expect(screen.queryByTestId("transfer-error")).not.toBeInTheDocument());
});

// A primary owns its data outright: nothing about it can change unless
// someone changes it, and whoever does has already invalidated. So the page
// reads once and stops — the whole reason the poll is conditional.
test("a primary's page is read once and never again", async () => {
  vi.useFakeTimers({ shouldAdvanceTime: true });
  const paths = trackFetchedPaths();
  renderZoneDetail({ zone: zone({ id: 1, name: "e412.in" }), records: [record({ id: 1 })] });
  await screen.findByText("e412.in");

  await act(async () => {
    await vi.advanceTimersByTimeAsync(5 * 60_000);
  });

  expect(paths.filter((p) => p === "/api/v1/zones/1")).toHaveLength(1);
  expect(paths.filter((p) => p === "/api/v1/zones/1/records")).toHaveLength(1);
});

// The trap this design exists to avoid: a secondary's records change exactly
// when a transfer replaces them, which the zone row already reports. Polling
// both would double the request rate to learn one fact — so the record list
// is re-read when `refreshed_at` moves, and at no other time.
test("a transfer landing replaces the records, and the record list is never polled for it", async () => {
  vi.useFakeTimers({ shouldAdvanceTime: true });
  const paths = trackFetchedPaths();
  const before = Date.now() - 2 * 3600_000;
  let refreshedAt = before;
  server.use(
    http.get("/api/v1/zones/1", () => HttpResponse.json(secondary({ refreshed_at: refreshedAt }))),
    http.get("/api/v1/zones/1/records", () =>
      HttpResponse.json(
        refreshedAt === before
          ? [record({ id: 1, name: "bifrost" })]
          : [record({ id: 1, name: "bifrost" }), record({ id: 2, name: "heimdall" })],
      ),
    ),
  );

  renderDetail("/zones/1");
  await screen.findByText("bifrost");
  expect(screen.queryByText("heimdall")).not.toBeInTheDocument();

  // Two minutes of watching a zone that transfers once, a minute in.
  await act(async () => {
    await vi.advanceTimersByTimeAsync(60_000);
  });
  expect(paths.filter((p) => p === "/api/v1/zones/1/records")).toHaveLength(1);
  refreshedAt = Date.now();
  await act(async () => {
    await vi.advanceTimersByTimeAsync(60_000);
  });

  expect(await screen.findByText("heimdall")).toBeInTheDocument();
  // Once on load, once because the transfer landed. The zone itself was read
  // many times over the same two minutes.
  expect(paths.filter((p) => p === "/api/v1/zones/1/records")).toHaveLength(2);
  expect(paths.filter((p) => p === "/api/v1/zones/1").length).toBeGreaterThan(2);
});

// Refresh now goes through the same door as the scheduler's own transfer:
// the mutation refetches the zone, and the record list follows the zone. The
// mutation invalidating the records itself would fetch the same list twice
// for one transfer — and would fetch it after a *failed* refresh, which
// replaced no record at all.
test("Refresh now brings the new records in, in one read of them", async () => {
  const user = userEvent.setup();
  const paths = trackFetchedPaths();
  let refreshed = false;
  server.use(
    http.get("/api/v1/zones/1", () =>
      HttpResponse.json(
        secondary(refreshed ? { refreshed_at: Date.now(), soa_serial: 2026080601 } : {}),
      ),
    ),
    http.get("/api/v1/zones/1/records", () =>
      HttpResponse.json(
        refreshed
          ? [record({ id: 1, name: "bifrost" }), record({ id: 2, name: "heimdall" })]
          : [record({ id: 1, name: "bifrost" })],
      ),
    ),
    http.post("/api/v1/zones/1/refresh", () => {
      refreshed = true;
      return HttpResponse.json({
        primary: "203.0.113.9:53",
        serial: 2026080601,
        records: 2,
        refreshed_at: Date.now(),
        expires_at: Date.now() + 1209600_000,
      });
    }),
  );

  renderDetail("/zones/1");
  await screen.findByText("bifrost");

  await user.click(screen.getByRole("button", { name: /refresh now/i }));

  expect(await screen.findByText("heimdall")).toBeInTheDocument();
  expect(paths.filter((p) => p === "/api/v1/zones/1/records")).toHaveLength(2);
});

// ── The allow-transfer row ────────────────────────────────────────────────
// Serving transfers applies to both primary and secondary zones — a
// secondary re-serves what it pulled — so this is neither part of SoaBand
// (primary-only) nor TransferBand (secondary-only, and about the transfers
// this zone *pulls*). It answers a different question from both: who may
// take this zone from us, and who last did. Read is the default; the pencil
// opens editing, and the row returns to read on Save or Cancel.

/** Opens the row's editing state the way a user does — the input is closed
 * until the pencil is clicked. */
async function openAllowTransferEdit(user: ReturnType<typeof userEvent.setup>) {
  await user.click(await screen.findByRole("button", { name: /^edit allow transfer$/i }));
  await screen.findByLabelText(/^allow transfer$/i);
}

test("shows who may transfer the zone, and editing seeds the field with it", async () => {
  const user = userEvent.setup();
  renderZoneDetail({ zone: zone({ id: 1, allow_transfer: "10.0.0.0/24, key:ns2." }) });
  await screen.findByText("example.com");

  expect(screen.getByText("10.0.0.0/24, key:ns2.")).toBeInTheDocument();
  expect(screen.queryByLabelText(/^allow transfer$/i)).not.toBeInTheDocument();

  await openAllowTransferEdit(user);
  expect(screen.getByLabelText(/^allow transfer$/i)).toHaveValue("10.0.0.0/24, key:ns2.");
});

test("says plainly when no peer may", async () => {
  const user = userEvent.setup();
  renderZoneDetail({ zone: zone({ id: 1, allow_transfer: "" }) });
  await screen.findByText("example.com");

  expect(screen.getByText("No peer may transfer this zone.")).toBeInTheDocument();

  await openAllowTransferEdit(user);
  expect(screen.getByLabelText(/^allow transfer$/i)).toHaveValue("");
});

test("Cancel returns to read mode and writes nothing", async () => {
  const user = userEvent.setup();
  let patched = false;
  renderZoneDetail({ zone: zone({ id: 1, allow_transfer: "10.0.0.0/24" }) });
  server.use(
    http.patch("/api/v1/zones/1", () => {
      patched = true;
      return new HttpResponse(null, { status: 204 });
    }),
  );
  await screen.findByText("example.com");
  await openAllowTransferEdit(user);
  await user.type(screen.getByLabelText(/^allow transfer$/i), ",typed-but-abandoned");

  await user.click(screen.getByRole("button", { name: /^cancel editing allow transfer$/i }));

  expect(screen.queryByLabelText(/^allow transfer$/i)).not.toBeInTheDocument();
  expect(screen.getByText("10.0.0.0/24")).toBeInTheDocument();
  await new Promise((resolve) => setTimeout(resolve, 30));
  expect(patched).toBe(false);
});

test("rejects a malformed entry before sending it", async () => {
  const user = userEvent.setup();
  let patched = false;
  renderZoneDetail({ zone: zone({ id: 1, allow_transfer: "" }) });
  server.use(
    http.patch("/api/v1/zones/1", () => {
      patched = true;
      return new HttpResponse(null, { status: 204 });
    }),
  );
  await screen.findByText("example.com");
  await openAllowTransferEdit(user);

  const field = screen.getByLabelText(/^allow transfer$/i);
  await user.type(field, "not-an-ip");
  await user.click(screen.getByRole("button", { name: /^save$/i }));

  expect(
    await screen.findByText(
      'allow_transfer "not-an-ip": expected an IP address, a CIDR prefix, or key:<name>',
    ),
  ).toBeInTheDocument();
  await new Promise((resolve) => setTimeout(resolve, 30));
  expect(patched).toBe(false);
});

test("saving a valid allow transfer PATCHes the zone and returns to read mode", async () => {
  const user = userEvent.setup();
  let body: unknown;
  renderZoneDetail({ zone: zone({ id: 1, allow_transfer: "" }) });
  server.use(
    http.patch("/api/v1/zones/1", async ({ request }) => {
      body = await request.json();
      return new HttpResponse(null, { status: 204 });
    }),
  );
  await screen.findByText("example.com");
  await openAllowTransferEdit(user);

  const field = screen.getByLabelText(/^allow transfer$/i);
  await user.type(field, "10.0.0.0/24");
  await user.click(screen.getByRole("button", { name: /^save$/i }));

  await waitFor(() => expect(body).toEqual({ allow_transfer: "10.0.0.0/24" }));
  await waitFor(() => expect(screen.queryByLabelText(/^allow transfer$/i)).not.toBeInTheDocument());
});

test("shows the last peer served and when", async () => {
  renderZoneDetail({
    zone: zone({
      id: 1,
      allow_transfer: "10.0.0.0/24",
      last_xfr_at: Date.now() - 2 * 60_000,
      last_xfr_peer: "10.0.0.5",
      last_xfr_error: "",
    }),
  });
  await screen.findByText("example.com");

  expect(screen.getByText("Last served 2m ago to 10.0.0.5")).toBeInTheDocument();
});

test("shows a refusal with its reason", async () => {
  renderZoneDetail({
    zone: zone({
      id: 1,
      allow_transfer: "10.0.0.0/24",
      last_xfr_at: Date.now() - 60_000,
      last_xfr_peer: "10.0.0.9",
      last_xfr_error: "not in allow transfer",
    }),
  });
  await screen.findByText("example.com");

  expect(screen.getByText("Refused 10.0.0.9 — not in allow transfer")).toBeInTheDocument();
});

test("says nothing has asked when last_xfr_at is 0", async () => {
  renderZoneDetail({
    zone: zone({ id: 1, last_xfr_at: 0, last_xfr_peer: "", last_xfr_error: "" }),
  });
  await screen.findByText("example.com");

  expect(screen.getByText("Never asked for.")).toBeInTheDocument();
});

test("shows the band for a secondary too", async () => {
  renderZoneDetail({
    zone: secondary({
      allow_transfer: "key:ns2.",
      last_xfr_at: Date.now() - 5 * 60_000,
      last_xfr_peer: "203.0.113.20",
      last_xfr_error: "",
    }),
  });
  await screen.findByText("e412.in");

  // Both bands render for a secondary: TransferBand (about what it pulls)
  // and this row (about who may take it from here), and they must not be
  // confused with each other.
  expect(screen.getByText("Primaries")).toBeInTheDocument();
  expect(screen.getByText("key:ns2.")).toBeInTheDocument();
  expect(screen.getByText("Last served 5m ago to 203.0.113.20")).toBeInTheDocument();
});
