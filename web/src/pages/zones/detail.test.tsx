import { http, HttpResponse } from "msw";
import { toast } from "sonner";
import { afterEach, expect, test, vi } from "vitest";
import { act, fireEvent, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { Route, Routes } from "react-router";
import { server } from "../../test/msw-server";
import { renderWithProviders } from "../../test/render";
import type { Zone, ZoneRecord } from "../../api/types";
import type { ZoneNotify } from "../../hooks/use-zones";
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
    notify_to: "",
    forward_to: "",
    next_attempt_at: 0,
    failures: 0,
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

function mockNotifies(zoneId: number, rows: ZoneNotify[]) {
  server.use(http.get(`/api/v1/zones/${zoneId}/notifies`, () => HttpResponse.json(rows)));
}

/** A `GET /notifies` row with sensible defaults for the fields a given test
 * doesn't care about — mirrors `zone`/`record` above. */
function notifyRow(overrides: Partial<ZoneNotify> = {}): ZoneNotify {
  return {
    target: "10.0.0.2:53",
    state: "current",
    notified_serial: 3,
    notified_at: Date.now() - 4 * 60_000,
    attempts: 0,
    max_attempts: 5,
    last_error: "",
    created_at: Date.now() - 86_400_000,
    ...overrides,
  };
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
  // Announced, not merely on screen: the message carries the id the Data
  // field's own aria-describedby points at.
  expect(screen.getByLabelText(/^data$/i)).toHaveAccessibleDescription(/dns: bad MX Mx/);

  // …and it renders *under Data*: the message lives inside that column's own
  // grid cell, so nothing can place it under Name.
  const cell = message.parentElement?.parentElement;
  const row = cell?.parentElement;
  expect(row?.children).toHaveLength(5);
  expect(Array.from(row?.children ?? []).indexOf(cell as Element)).toBe(3);
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
    expect(body).toEqual({
      name: "bifrost",
      type: "A",
      ttl: 300,
      rdata: "10.0.0.9",
      enabled: true,
      comment: "",
    }),
  );
  expect(requestUrl).toMatch(/\/api\/v1\/zones\/1\/records\/7$/);
});

// PUT is a full replace and zones.BuildRecord defaults an absent `enabled`
// to true and an absent `comment` to "", so a payload carrying neither
// re-enables a disabled record and wipes its comment — for an edit that only
// touched the TTL.
test("editing a disabled, commented record keeps both in the PUT body", async () => {
  const user = userEvent.setup();
  let body: unknown;
  mockZone(zone({ id: 1 }));
  mockRecords(1, [
    record({
      id: 7,
      name: "bifrost",
      type: "A",
      ttl: 300,
      rdata: "10.0.0.1",
      enabled: false,
      comment: "parked until the move",
    }),
  ]);
  server.use(
    http.put("/api/v1/zones/1/records/7", async ({ request }) => {
      body = await request.json();
      return new HttpResponse(null, { status: 204 });
    }),
  );

  renderDetail();
  await waitFor(() => expect(recordRows()).toHaveLength(1));

  await user.click(screen.getByRole("button", { name: /edit bifrost a 10\.0\.0\.1/i }));
  await waitFor(() => expect(screen.getByLabelText(/zone record name/i)).toHaveValue("bifrost"));

  const ttlField = screen.getByLabelText(/^ttl$/i);
  await user.clear(ttlField);
  await user.type(ttlField, "600");
  await user.click(screen.getByRole("button", { name: /^save$/i }));

  await waitFor(() =>
    expect(body).toEqual({
      name: "bifrost",
      type: "A",
      ttl: 600,
      rdata: "10.0.0.1",
      enabled: false,
      comment: "parked until the move",
    }),
  );
});

// A disabled record is dropped when the served snapshot is built, so a grid
// that draws it like every other row claims it is being answered.
test("a disabled record's row says so", async () => {
  mockZone(zone({ id: 1 }));
  mockRecords(1, [
    record({ id: 7, name: "bifrost", enabled: false }),
    record({ id: 8, name: "nas", enabled: true }),
  ]);

  renderDetail();
  const rows = await waitFor(() => {
    const found = recordRows();
    expect(found).toHaveLength(2);
    return found;
  });

  expect(within(rows[0]).getByText("Disabled")).toBeInTheDocument();
  expect(rows[0].dataset.enabled).toBe("false");
  expect(within(rows[1]).queryByText("Disabled")).not.toBeInTheDocument();
  expect(rows[1].dataset.enabled).toBe("true");
});

// The server takes any type dns.NewRR parses, so a zone imported from
// elsewhere can hold one the select has no option for. Without a passthrough
// option the browser shows the first option instead ("A"), and saving would
// rewrite the record's type to it.
test("a record of an unlisted type can be edited without changing its type", async () => {
  const user = userEvent.setup();
  let body: unknown;
  mockZone(zone({ id: 1 }));
  mockRecords(1, [
    record({ id: 7, name: "_443._tcp", type: "TLSA", ttl: 300, rdata: "3 1 1 abc" }),
  ]);
  server.use(
    http.put("/api/v1/zones/1/records/7", async ({ request }) => {
      body = await request.json();
      return new HttpResponse(null, { status: 204 });
    }),
  );

  renderDetail();
  await waitFor(() => expect(recordRows()).toHaveLength(1));

  await user.click(screen.getByRole("button", { name: /edit _443\._tcp tlsa 3 1 1 abc/i }));
  await waitFor(() => expect(screen.getByLabelText(/^record type$/i)).toHaveValue("TLSA"));

  const ttlField = screen.getByLabelText(/^ttl$/i);
  await user.clear(ttlField);
  await user.type(ttlField, "900");
  await user.click(screen.getByRole("button", { name: /^save$/i }));

  await waitFor(() =>
    expect(body).toEqual({
      name: "_443._tcp",
      type: "TLSA",
      ttl: 900,
      rdata: "3 1 1 abc",
      enabled: true,
      comment: "",
    }),
  );
});

// Adding one record is usually adding several of the same kind, and the two
// fields that repeat are exactly the two that were being cleared.
test("a successful Add clears name and data but keeps type and TTL", async () => {
  const user = userEvent.setup();
  mockZone(zone({ id: 1 }));
  mockRecords(1, []);
  server.use(http.post("/api/v1/zones/1/records", () => HttpResponse.json({ id: 9 })));

  renderDetail();
  await openAddRow(user);

  await user.selectOptions(screen.getByLabelText(/^record type$/i), "AAAA");
  const ttlField = screen.getByLabelText(/^ttl$/i);
  await user.clear(ttlField);
  await user.type(ttlField, "900");
  await user.type(screen.getByLabelText(/zone record name/i), "bifrost");
  await user.type(screen.getByLabelText(/^data$/i), "fd00::28");
  await user.click(screen.getByRole("button", { name: /^add$/i }));

  await waitFor(() => expect(screen.getByLabelText(/zone record name/i)).toHaveValue(""));
  expect(screen.getByLabelText(/^data$/i)).toHaveValue("");
  expect(screen.getByLabelText(/^record type$/i)).toHaveValue("AAAA");
  expect(screen.getByLabelText(/^ttl$/i)).toHaveValue("900");
});

// A 2048-bit DKIM key in a single-line input is one long unreadable ribbon.
test("TXT rdata gets a textarea, other types a single-line input", async () => {
  const user = userEvent.setup();
  mockZone(zone({ id: 1 }));
  mockRecords(1, []);

  renderDetail();
  await openAddRow(user);

  expect(screen.getByLabelText(/^data$/i).tagName).toBe("INPUT");

  await user.selectOptions(screen.getByLabelText(/^record type$/i), "TXT");
  expect(screen.getByLabelText(/^data$/i).tagName).toBe("TEXTAREA");
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
  // notify_to is refused with 400 on the same terms (checkZoneTransferConfig
  // applies to both fields identically) — the NOTIFY OUT row must not be
  // offered here either.
  expect(screen.queryByRole("button", { name: /^edit notify targets$/i })).not.toBeInTheDocument();
  expect(screen.queryByLabelText(/^notify to$/i)).not.toBeInTheDocument();
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

// Cancelling closes the dialog, and the dry run it was waiting on is still in
// flight: without a token to compare against, its onSuccess set phase = diff
// and the dialog reopened by itself seconds after being dismissed.
test("Cancel during the dry run keeps the dialog closed when the result lands", async () => {
  const user = userEvent.setup();
  let release!: () => void;
  const held = new Promise<void>((resolve) => {
    release = resolve;
  });
  server.use(
    http.post("/api/v1/zones/1/file", async () => {
      await held;
      return HttpResponse.json({
        add: [record({ id: 3, name: "nas", rdata: "10.0.0.3" })],
        change: [],
        delete: [],
        errors: [],
      });
    }),
  );

  renderZoneDetail({ zone: zone({ id: 1, name: "e412.in" }) });
  await chooseFile(user);
  expect(await screen.findByText(/checking what this file would change/i)).toBeInTheDocument();

  await user.click(screen.getByRole("button", { name: /^cancel$/i }));
  await waitFor(() =>
    expect(screen.queryByText(/checking what this file would change/i)).not.toBeInTheDocument(),
  );

  release();
  // Long enough for the settled mutation to run its onSuccess.
  await new Promise((resolve) => setTimeout(resolve, 50));
  expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  expect(screen.queryByText(/1 record/i)).not.toBeInTheDocument();
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

test("a secondary with no TSIG key says unsigned rather than showing a zero", async () => {
  renderZoneDetail({ zone: secondary({ tsig_key_id: 0 }) });
  await screen.findByText("e412.in");

  expect(await screen.findByText("unsigned")).toBeInTheDocument();
  expect(screen.queryByText("0")).not.toBeInTheDocument();
});

// The other half of the same mapping: an id the loaded list has no key for
// is a key deleted out from under the zone, and saying so takes the list
// having actually arrived.
test("an id the loaded key list cannot name is marked missing", async () => {
  server.use(http.get("/api/v1/tsig-keys", () => HttpResponse.json([])));
  renderZoneDetail({ zone: secondary({ tsig_key_id: 3 }) });
  await screen.findByText("e412.in");

  expect(await screen.findByText("#3 (missing)")).toBeInTheDocument();
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
  // skipped by Index.Find, so names under it fall through upstream — the zone
  // stops being consulted rather than starting to refuse, and "answers
  // nothing" is the one behaviour it does not have. What that costs a
  // split-horizon zone is docs/dashboard.md's to explain, not this line's.
  expect(screen.queryByText(/answers nothing/i)).not.toBeInTheDocument();
  expect(screen.getByText("Nothing transfers while this zone is disabled.")).toBeInTheDocument();
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
  expect(screen.getByText("Nothing transfers while this zone is disabled.")).toBeInTheDocument();
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

// ── The notify-out row (Task 12) ────────────────────────────────────────
// Read is the default, the pencil opens editing, exactly as allow_transfer
// works above — but the read line also carries a roll-up over GET
// /zones/{id}/notifies (mockNotifies/notifyRow above), since "who" and "how
// each of them is doing" are two different questions this row answers at
// once.

/** Opens the row's editing state the way a user does. */
async function openNotifyEdit(user: ReturnType<typeof userEvent.setup>) {
  await user.click(await screen.findByRole("button", { name: /^edit notify targets$/i }));
  await screen.findByLabelText(/^notify to$/i);
}

test("shows who is notified, and editing seeds the field with it", async () => {
  const user = userEvent.setup();
  renderZoneDetail({ zone: zone({ id: 1, notify_to: "10.0.0.2:53, key:ns2." }) });
  await screen.findByText("example.com");

  expect(screen.getByText("10.0.0.2:53, key:ns2.")).toBeInTheDocument();
  expect(screen.queryByLabelText(/^notify to$/i)).not.toBeInTheDocument();

  await openNotifyEdit(user);
  expect(screen.getByLabelText(/^notify to$/i)).toHaveValue("10.0.0.2:53, key:ns2.");
});

test("says plainly when nobody is notified, with no disclosure to click", async () => {
  renderZoneDetail({ zone: zone({ id: 1, notify_to: "" }) });
  await screen.findByText("example.com");

  expect(screen.getByText("No targets are notified.")).toBeInTheDocument();
  expect(await screen.findByText("no targets")).toBeInTheDocument();
  // Nothing to disclose, so the roll-up is plain text, not a button that
  // would do nothing on click.
  expect(screen.queryByTitle("Show every target")).not.toBeInTheDocument();
});

test("all targets current rolls up to one line, with no rows shown by default", async () => {
  mockNotifies(1, [
    notifyRow({ target: "10.0.0.2:53", state: "current" }),
    notifyRow({ target: "10.0.0.3:53", state: "current" }),
  ]);
  renderZoneDetail({ zone: zone({ id: 1, notify_to: "10.0.0.2, 10.0.0.3" }) });
  await screen.findByText("example.com");

  expect(await screen.findByText("all 2 current")).toBeInTheDocument();
  // Two identical "current" rows would say nothing twice — the roll-up is
  // the whole story until the caret is opened.
  expect(screen.queryByTestId("notify-target-row")).not.toBeInTheDocument();
});

// Ruled 2026-09-02 by the design's author: nothing is behind here (never
// doesn't count), so the artboard's own arithmetic would say "all 4
// current" — a false statement about a target that has been told nothing
// yet. The roll-up names it instead.
test("a never-notified target with nothing behind is named, not folded into current", async () => {
  const user = userEvent.setup();
  mockNotifies(1, [
    notifyRow({ target: "10.0.0.2:53", state: "current" }),
    notifyRow({ target: "10.0.0.3:53", state: "current" }),
    notifyRow({ target: "10.0.0.4:53", state: "current" }),
    notifyRow({ target: "10.0.0.5:53", state: "never" }),
  ]);
  renderZoneDetail({ zone: zone({ id: 1, notify_to: "10.0.0.2, 10.0.0.3, 10.0.0.4, 10.0.0.5" }) });
  await screen.findByText("example.com");

  expect(await screen.findByText("3 of 4 current, 1 never notified")).toBeInTheDocument();
  // Nothing is behind, so nothing gets a row until the caret is opened —
  // same rule as the all-current case above.
  expect(screen.queryByTestId("notify-target-row")).not.toBeInTheDocument();

  await user.click(screen.getByTitle("Show every target"));
  await waitFor(() => expect(screen.getAllByTestId("notify-target-row")).toHaveLength(4));
  expect(screen.getByText("10.0.0.5:53")).toBeInTheDocument();
});

test("only the behind target gets a row by default; the caret expands the rest", async () => {
  const user = userEvent.setup();
  mockNotifies(1, [
    notifyRow({ target: "10.0.0.2:53", state: "current", notified_serial: 7 }),
    notifyRow({
      target: "10.0.0.3:53",
      state: "gave_up",
      notified_serial: 4,
      attempts: 5,
      last_error: "connection refused",
    }),
  ]);
  renderZoneDetail({ zone: zone({ id: 1, notify_to: "10.0.0.2, 10.0.0.3" }) });
  await screen.findByText("example.com");

  expect(await screen.findByText("1 of 2 behind")).toBeInTheDocument();
  await waitFor(() => expect(screen.getAllByTestId("notify-target-row")).toHaveLength(1));
  const behindRow = screen.getByTestId("notify-target-row");
  expect(within(behindRow).getByText("10.0.0.3:53")).toBeInTheDocument();
  // gave_up reads "not acknowledged", never "gave up" — it resolves itself
  // on the next edit and must not read as something to act on.
  expect(within(behindRow).getByText("not acknowledged")).toBeInTheDocument();
  expect(within(behindRow).getByText("connection refused")).toBeInTheDocument();
  expect(screen.getByText("+ 1 current")).toBeInTheDocument();

  await user.click(screen.getByTitle("Show every target"));
  await waitFor(() => expect(screen.getAllByTestId("notify-target-row")).toHaveLength(2));
  expect(screen.getByText("10.0.0.2:53")).toBeInTheDocument();
  expect(screen.queryByText("+ 1 current")).not.toBeInTheDocument();
});

// The "+N" hint under a behind row has the identical false-statement risk
// the roll-up itself does when the collapsed rest is a mix — it must name
// a never-notified target rather than lumping it in with "current" too.
test("the collapsed hint names a never-notified target among the rest, not just current", async () => {
  mockNotifies(1, [
    notifyRow({ target: "10.0.0.2:53", state: "gave_up", attempts: 5, last_error: "REFUSED" }),
    notifyRow({ target: "10.0.0.3:53", state: "current" }),
    notifyRow({ target: "10.0.0.4:53", state: "never" }),
  ]);
  renderZoneDetail({ zone: zone({ id: 1, notify_to: "10.0.0.2, 10.0.0.3, 10.0.0.4" }) });
  await screen.findByText("example.com");

  expect(await screen.findByText("1 of 3 behind")).toBeInTheDocument();
  expect(screen.getByText("+ 1 current, 1 never notified")).toBeInTheDocument();
});

test("retrying shows its attempt count, and a never-notified target shows — and when it was added", async () => {
  const addedAt = Date.now() - 2 * 60_000;
  mockNotifies(1, [
    notifyRow({ target: "10.0.0.2:53", state: "retrying", attempts: 3, max_attempts: 5 }),
    notifyRow({
      target: "10.0.0.3:53",
      state: "never",
      notified_serial: 0,
      notified_at: 0,
      attempts: 0,
      created_at: addedAt,
    }),
  ]);
  renderZoneDetail({ zone: zone({ id: 1, notify_to: "10.0.0.2, 10.0.0.3" }) });
  await screen.findByText("example.com");

  // never doesn't count as behind, so only the retrying target earns a row
  // by default — the roll-up says "1 of 2", not "2 of 2".
  expect(await screen.findByText("1 of 2 behind")).toBeInTheDocument();
  const row = await screen.findByTestId("notify-target-row");
  expect(within(row).getByText("retrying · try 3/5")).toBeInTheDocument();

  await userEvent.setup().click(screen.getByTitle("Show every target"));
  const neverRow = screen.getByText("10.0.0.3:53").closest('[data-testid="notify-target-row"]');
  expect(neverRow).not.toBeNull();
  expect(within(neverRow as HTMLElement).getByText("never notified")).toBeInTheDocument();
  expect(within(neverRow as HTMLElement).getByText("—")).toBeInTheDocument();
  expect(within(neverRow as HTMLElement).getByText("added 2m ago")).toBeInTheDocument();
});

// notified_at != 0, attempts == 0 is also the state of every previously-
// current target in the window between a serial bump and the notify pass's
// first attempt (the mutation's response returns before the async pass
// runs), so "try 0/5" there would read as nonsense on an ordinary record
// edit. Drop the counter at attempts === 0 and leave plain "retrying".
test("retrying with no attempts yet drops the try counter", async () => {
  mockNotifies(1, [
    notifyRow({ target: "10.0.0.2:53", state: "retrying", attempts: 0, max_attempts: 5 }),
  ]);
  renderZoneDetail({ zone: zone({ id: 1, notify_to: "10.0.0.2" }) });
  await screen.findByText("example.com");

  const row = await screen.findByTestId("notify-target-row");
  expect(within(row).getByText("retrying")).toBeInTheDocument();
  expect(within(row).queryByText(/try 0\/5/)).not.toBeInTheDocument();
});

test("Cancel returns to read mode and writes nothing", async () => {
  const user = userEvent.setup();
  let patched = false;
  renderZoneDetail({ zone: zone({ id: 1, notify_to: "10.0.0.2" }) });
  server.use(
    http.patch("/api/v1/zones/1", () => {
      patched = true;
      return new HttpResponse(null, { status: 204 });
    }),
  );
  await screen.findByText("example.com");
  await openNotifyEdit(user);
  await user.type(screen.getByLabelText(/^notify to$/i), ",typed-but-abandoned");

  await user.click(screen.getByRole("button", { name: /^cancel editing notify targets$/i }));

  expect(screen.queryByLabelText(/^notify to$/i)).not.toBeInTheDocument();
  expect(screen.getByText("10.0.0.2")).toBeInTheDocument();
  await new Promise((resolve) => setTimeout(resolve, 30));
  expect(patched).toBe(false);
});

test("rejects a malformed entry before sending it", async () => {
  const user = userEvent.setup();
  let patched = false;
  renderZoneDetail({ zone: zone({ id: 1, notify_to: "" }) });
  server.use(
    http.patch("/api/v1/zones/1", () => {
      patched = true;
      return new HttpResponse(null, { status: 204 });
    }),
  );
  await screen.findByText("example.com");
  await openNotifyEdit(user);

  const field = screen.getByLabelText(/^notify to$/i);
  // A single token, no whitespace: "not a host" would trip the *key*
  // branch instead ("expected `key:<name>` after the host", since a space
  // is what separates a target from its key suffix) — this exercises the
  // host-format check on its own.
  await user.type(field, "not/a/host");
  await user.click(screen.getByRole("button", { name: /^save$/i }));

  expect(
    await screen.findByText(
      'notify target "not/a/host": host must be an IP address or a domain name',
    ),
  ).toBeInTheDocument();
  await new Promise((resolve) => setTimeout(resolve, 30));
  expect(patched).toBe(false);
});

test("saving a valid notify_to PATCHes the zone and returns to read mode", async () => {
  const user = userEvent.setup();
  let body: unknown;
  renderZoneDetail({ zone: zone({ id: 1, notify_to: "" }) });
  server.use(
    http.patch("/api/v1/zones/1", async ({ request }) => {
      body = await request.json();
      return new HttpResponse(null, { status: 204 });
    }),
  );
  await screen.findByText("example.com");
  await openNotifyEdit(user);

  const field = screen.getByLabelText(/^notify to$/i);
  await user.type(field, "10.0.0.2");
  await user.click(screen.getByRole("button", { name: /^save$/i }));

  await waitFor(() => expect(body).toEqual({ notify_to: "10.0.0.2" }));
  await waitFor(() => expect(screen.queryByLabelText(/^notify to$/i)).not.toBeInTheDocument());
});

test("shows the row for a secondary too", async () => {
  mockNotifies(1, [notifyRow({ target: "10.0.0.9:53", state: "current" })]);
  renderZoneDetail({ zone: secondary({ notify_to: "10.0.0.9" }) });
  await screen.findByText("e412.in");

  expect(screen.getByText("10.0.0.9")).toBeInTheDocument();
  expect(await screen.findByText("all 1 current")).toBeInTheDocument();
});

// ── A forwarder zone ──────────────────────────────────────────────────────
// A forwarder answers nothing of its own: it claims a suffix and sends every
// query beneath it to the addresses typed into `forward_to`. So its page is
// short by design — header, one row, zone actions — and every band that
// assumes authored data is gone. The tests below are mostly *absences*,
// because that is where the gating actually lives: a test that only asserted
// the FORWARD TO row is present would pass just as happily with the SOA form
// still under it.

/** A forwarder with two upstreams, in the canonical spelling the server
 * reads `forward_to` back in (port always explicit). */
function forwarder(overrides: Partial<Zone> = {}): Zone {
  return zone({
    id: 1,
    name: "corp.example",
    type: "forwarder",
    forward_to: "10.0.0.1:53, 10.0.0.2:5353",
    ...overrides,
  });
}

test("a forwarder shows its upstreams in the FORWARD TO row, counted", async () => {
  renderZoneDetail({ zone: forwarder() });
  await screen.findByText("Forward to");

  expect(screen.getByText("Forward to")).toBeInTheDocument();
  expect(screen.getByText("10.0.0.1:53, 10.0.0.2:5353")).toBeInTheDocument();
  expect(screen.getByText("2 upstreams")).toBeInTheDocument();
});

// The line that stops a three-element page reading as one that failed to
// load. It is also the single most surprising thing about the type: a
// claimed suffix does *not* fall back to the default resolvers, so a
// forwarder whose upstreams are all down takes its whole suffix down with
// it. §9.11.5 — stated, with the reasoning left to docs/dashboard.md.
test("a forwarder states the SERVFAIL consequence of claiming its suffix", async () => {
  renderZoneDetail({ zone: forwarder() });
  await screen.findByText("Forward to");

  expect(
    screen.getByText(/claimed outright: with no upstream reachable, queries for it get SERVFAIL/i),
  ).toBeInTheDocument();
});

// Every band on this page assumes a zone that holds records. A forwarder
// holds none, so all of them go — including the records grid itself, which
// is the one a stub keeps.
test("a forwarder has no SOA band, no records grid and no record controls", async () => {
  renderZoneDetail({ zone: forwarder(), records: [record({ id: 1, name: "bifrost" })] });
  await screen.findByText("Forward to");

  // The SOA band is the specific regression: the old
  // `isSecondary ? <TransferBand/> : <SoaBand/>` put a forwarder in the else
  // branch, offering an editable SOA form for a zone whose SOA nobody reads.
  expect(screen.queryByRole("button", { name: /^soa$/i })).not.toBeInTheDocument();
  expect(screen.queryByRole("button", { name: /^save soa$/i })).not.toBeInTheDocument();
  // No grid at all — not an empty one. Even a record that somehow exists on
  // the row is not listed, because a forwarder serves nothing from records.
  expect(screen.queryByTestId("zone-record-list")).not.toBeInTheDocument();
  expect(screen.queryByText("bifrost")).not.toBeInTheDocument();
  expect(screen.queryByLabelText(/filter by name/i)).not.toBeInTheDocument();
  expect(screen.queryByRole("button", { name: /add record/i })).not.toBeInTheDocument();
  expect(screen.queryByTestId("record-form-row")).not.toBeInTheDocument();
  // §9.5.3 refuses a transfer of a forwarder with NOTAUTH regardless of its
  // ACL, and checkZoneTransferConfig 400s both fields on one outright.
  expect(screen.queryByRole("button", { name: /^edit allow transfer$/i })).not.toBeInTheDocument();
  expect(screen.queryByRole("button", { name: /^edit notify targets$/i })).not.toBeInTheDocument();
});

// Refresh is `secondary || stub`, never a forwarder: a forwarder has no
// master and nothing to fetch, and POST /zones/{id}/refresh answers it 400
// ("only secondary and stub zones pull from a master"). Export goes for a
// different reason — there is nothing under the apex to render into a file.
test("a forwarder offers neither Refresh now nor Export, but keeps the zone actions", async () => {
  renderZoneDetail({ zone: forwarder() });
  await screen.findByText("Forward to");

  expect(screen.queryByRole("button", { name: /refresh now/i })).not.toBeInTheDocument();
  expect(screen.queryByRole("button", { name: /^export$/i })).not.toBeInTheDocument();
  expect(screen.queryByRole("button", { name: /^import$/i })).not.toBeInTheDocument();
  expect(screen.getByRole("button", { name: /disable zone/i })).toBeInTheDocument();
  expect(screen.getByRole("button", { name: /delete zone/i })).toBeInTheDocument();
});

test("editing the FORWARD TO row PATCHes forward_to and returns to read mode", async () => {
  const user = userEvent.setup();
  let body: unknown;
  renderZoneDetail({ zone: forwarder() });
  server.use(
    http.patch("/api/v1/zones/1", async ({ request }) => {
      body = await request.json();
      return new HttpResponse(null, { status: 204 });
    }),
  );
  await screen.findByText("Forward to");

  await user.click(screen.getByRole("button", { name: /^edit upstreams$/i }));
  const field = screen.getByLabelText(/^forward to$/i);
  // Seeded with the saved value, so an edit is an edit rather than a retype.
  expect(field).toHaveValue("10.0.0.1:53, 10.0.0.2:5353");
  await user.clear(field);
  await user.type(field, "10.0.0.3");
  await user.click(screen.getByRole("button", { name: /^save$/i }));

  await waitFor(() => expect(body).toEqual({ forward_to: "10.0.0.3" }));
  await waitFor(() => expect(screen.queryByLabelText(/^forward to$/i)).not.toBeInTheDocument());
});

// "" is a configuration, not a gap — the zone still claims the suffix and
// SERVFAILs it — so the server accepts it and this row must not invent a
// client-side rule the server does not have.
test("a forwarder with no upstreams says so and can still be saved empty", async () => {
  const user = userEvent.setup();
  let body: unknown;
  renderZoneDetail({ zone: forwarder({ forward_to: "" }) });
  server.use(
    http.patch("/api/v1/zones/1", async ({ request }) => {
      body = await request.json();
      return new HttpResponse(null, { status: 204 });
    }),
  );
  await screen.findByText("Forward to");

  expect(screen.getByText("none")).toBeInTheDocument();
  expect(screen.queryByText(/upstreams$/)).not.toBeInTheDocument();

  await user.click(screen.getByRole("button", { name: /^edit upstreams$/i }));
  await user.click(screen.getByRole("button", { name: /^save$/i }));
  await waitFor(() => expect(body).toEqual({ forward_to: "" }));
});

test("one upstream is counted in the singular", async () => {
  renderZoneDetail({ zone: forwarder({ forward_to: "10.0.0.1:53" }) });
  await screen.findByText("Forward to");

  expect(screen.getByText("1 upstream")).toBeInTheDocument();
});

// ── A stub zone ───────────────────────────────────────────────────────────
// A stub claims a suffix and routes it too, but fetches the addresses rather
// than being told them: an SOA and an NS query with glue against its master,
// not an AXFR. So it keeps the records grid — a fetched NS set is worth
// seeing — read-only, and its primaries row carries the state of the fetch.
// Every one of its states carries its own date: an error with no date is a
// claim about the present made by an unknown past.

/** A stub whose last fetch landed 26 minutes ago. */
function stub(overrides: Partial<Zone> = {}): Zone {
  return zone({
    id: 1,
    name: "ad.corp.example",
    type: "stub",
    primaries: "10.0.0.9:53",
    soa_refresh: 25200,
    soa_retry: 3600,
    refreshed_at: Date.now() - 26 * 60_000,
    ...overrides,
  });
}

const NS_SET = [
  record({ id: 1, name: "@", type: "NS", ttl: 86400, rdata: "dc01.ad.corp.example." }),
  record({ id: 2, name: "@", type: "NS", ttl: 86400, rdata: "dc02.ad.corp.example." }),
];

test("a stub shows its master and dates the NS set it fetched", async () => {
  renderZoneDetail({ zone: stub(), records: NS_SET });
  await screen.findByText("ad.corp.example");

  expect(screen.getByText("Primaries")).toBeInTheDocument();
  expect(screen.getByText("10.0.0.9:53")).toBeInTheDocument();
  expect(screen.getByText("fetched 26m ago")).toBeInTheDocument();
});

// The NS set is the whole point of the type, so it is listed — but it came
// off the wire, and the next fetch replaces it wholesale (DiffRecords in
// StubFetcher.Fetch), so nothing here may be edited.
test("a stub lists its NS set, read-only", async () => {
  renderZoneDetail({ zone: stub(), records: NS_SET });
  await screen.findByText("ad.corp.example");

  const rows = recordRows();
  expect(rows).toHaveLength(2);
  expect(within(rows[0]).getByText("dc01.ad.corp.example.")).toBeInTheDocument();
  expect(within(rows[0]).queryByRole("button", { name: /^edit/i })).not.toBeInTheDocument();
  expect(within(rows[0]).queryByRole("button", { name: /^delete/i })).not.toBeInTheDocument();
  expect(screen.queryByRole("button", { name: /add record/i })).not.toBeInTheDocument();
  expect(screen.queryByTestId("record-form-row")).not.toBeInTheDocument();
  expect(screen.queryByRole("button", { name: /^import$/i })).not.toBeInTheDocument();
  expect(screen.getByText(/fetched · read-only/i)).toBeInTheDocument();
});

test("a stub has no SOA band, no record filter, no allow-transfer row and no notify row", async () => {
  renderZoneDetail({ zone: stub(), records: NS_SET });
  await screen.findByText("ad.corp.example");

  // Same regression as the forwarder's: the else branch would have offered
  // an editable SOA form for an SOA the master owns and the next fetch
  // overwrites (StubFetcher.Fetch writes all seven fields).
  expect(screen.queryByRole("button", { name: /^soa$/i })).not.toBeInTheDocument();
  // The filter row is dropped for *both* routing types, and asserting it on
  // the forwarder alone left half the gate deletable with the suite green —
  // a forwarder has no grid to filter, so `!isForwarder` there would look
  // right and silently give a stub a filter over three rows that are all
  // named "@" and all of type NS.
  expect(screen.queryByLabelText(/filter by name/i)).not.toBeInTheDocument();
  expect(screen.queryByLabelText(/filter by record type/i)).not.toBeInTheDocument();
  expect(screen.queryByRole("button", { name: /^edit allow transfer$/i })).not.toBeInTheDocument();
  expect(screen.queryByRole("button", { name: /^edit notify targets$/i })).not.toBeInTheDocument();
});

// Refresh is `secondary || stub` — a stub has a master and something to
// fetch from it, which is exactly the condition the API's own gate uses.
// Export stays: a fetched delegation is worth reading out.
test("a stub can be refreshed and exported", async () => {
  renderZoneDetail({ zone: stub(), records: NS_SET });
  await screen.findByText("ad.corp.example");

  expect(screen.getByRole("button", { name: /refresh now/i })).toBeInTheDocument();
  expect(screen.getByRole("button", { name: /^export$/i })).toBeInTheDocument();
});

test("Refresh now on a stub fetches, and reports it as a fetch rather than a transfer", async () => {
  const user = userEvent.setup();
  const success = vi.spyOn(toast, "success").mockImplementation(() => "");
  renderZoneDetail({ zone: stub(), records: NS_SET });
  server.use(
    http.post("/api/v1/zones/1/refresh", () =>
      HttpResponse.json({
        primary: "10.0.0.9:53",
        serial: 7,
        records: 5,
        refreshed_at: Date.now(),
        expires_at: 0,
      }),
    ),
  );
  await screen.findByText("ad.corp.example");

  await user.click(screen.getByRole("button", { name: /refresh now/i }));

  await waitFor(() => expect(success).toHaveBeenCalledWith("Fetched 5 records from 10.0.0.9:53"));
});

// State two of three. The first fetch has not landed, so there is no set to
// date — and the grid says which master is being asked, which is the only
// other fact there is.
test("a stub with no NS set yet says so and names the master", async () => {
  renderZoneDetail({ zone: stub({ refreshed_at: 0 }), records: [] });
  await screen.findByText("ad.corp.example");

  expect(screen.getByText("no NS set yet")).toBeInTheDocument();
  expect(screen.getByText("Fetching the NS set from 10.0.0.9:53")).toBeInTheDocument();
});

// State three. The error verbatim, the date of the attempt that produced it,
// and the age of the set still being routed to — three facts, none of which
// means anything without the other two.
test("a failed stub fetch shows the error verbatim, dated, beside what is still being served", async () => {
  renderZoneDetail({
    zone: stub({
      refreshed_at: Date.now() - 3 * 86_400_000,
      last_error: "i/o timeout",
      last_attempt: Date.now() - 12 * 60_000,
    }),
    records: NS_SET,
  });
  await screen.findByText("ad.corp.example");

  expect(screen.getByText("Last fetch · 12m ago")).toBeInTheDocument();
  expect(screen.getByTestId("stub-fetch-error")).toHaveTextContent("i/o timeout");
  expect(screen.getByText("serving what arrived 3d ago")).toBeInTheDocument();
  // The note beside the value goes with it: the set is old, and saying only
  // "fetched 3d ago" would read as a healthy schedule.
  expect(screen.getByText("from 3d ago")).toBeInTheDocument();
});

// The one case where "still serving" would be a lie: nothing has ever been
// fetched, so the zone claims its suffix with no addresses behind it and
// SERVFAILs every name under it (see Refresher.firstAttempt).
test("a stub that has never fetched does not claim to be serving an NS set", async () => {
  renderZoneDetail({
    zone: stub({
      refreshed_at: 0,
      last_error: "dial udp 10.0.0.9:53: connect: connection refused",
      last_attempt: Date.now() - 90_000,
    }),
    records: [],
  });
  await screen.findByText("ad.corp.example");

  expect(screen.getByText("Last fetch · 1m ago")).toBeInTheDocument();
  expect(screen.getByTestId("stub-fetch-error")).toHaveTextContent(
    "dial udp 10.0.0.9:53: connect: connection refused",
  );
  expect(screen.getByText("Answering nothing until the first fetch succeeds.")).toBeInTheDocument();
  expect(screen.queryByText(/serving what arrived/i)).not.toBeInTheDocument();
  // …and the empty grid does not claim to be fetching either. "Fetching the
  // NS set from …" is present tense, and this stub is not fetching — its last
  // attempt failed, which the primaries row above states with its date. The
  // artboard draws the present-tense line unconditionally; this is the one
  // place this page departs from it, and without this assertion the departure
  // could be undone with the whole suite green.
  expect(screen.getByText("No NS set yet.")).toBeInTheDocument();
  expect(screen.queryByText(/fetching the NS set from/i)).not.toBeInTheDocument();
});

// A stub is never *given* an expires_at (§9.11.8) — but a row retyped from
// secondary to stub keeps the column, and internal/zones/refresh.go makes
// this exact carve-out for exactly that reason. Reading it here would put a
// zone that is routing perfectly well on screen as expired and not
// answering, and send an operator hunting an outage that is not happening.
test("a stale expires_at left on a stub is never rendered as an expiry", async () => {
  renderZoneDetail({
    zone: stub({
      expires_at: Date.now() - 17 * 86_400_000,
      refreshed_at: Date.now() - 26 * 60_000,
    }),
    records: NS_SET,
  });
  await screen.findByText("ad.corp.example");

  expect(screen.getByText("Enabled")).toBeInTheDocument();
  expect(screen.queryByText("Not answering")).not.toBeInTheDocument();
  expect(screen.queryByText(/expired/i)).not.toBeInTheDocument();
  expect(screen.getByText("fetched 26m ago")).toBeInTheDocument();
});

test("editing a stub's primaries row PATCHes primaries and returns to read mode", async () => {
  const user = userEvent.setup();
  let body: unknown;
  renderZoneDetail({ zone: stub(), records: NS_SET });
  server.use(
    http.patch("/api/v1/zones/1", async ({ request }) => {
      body = await request.json();
      return new HttpResponse(null, { status: 204 });
    }),
  );
  await screen.findByText("ad.corp.example");

  await user.click(screen.getByRole("button", { name: /^edit primaries$/i }));
  const field = screen.getByLabelText(/^primaries$/i);
  expect(field).toHaveValue("10.0.0.9:53");
  await user.clear(field);
  await user.type(field, "10.0.0.10");
  await user.click(screen.getByRole("button", { name: /^save$/i }));

  // A stub signs its SOA/NS queries with the same key column a secondary
  // signs its transfers with, so the row carries both and sends both.
  await waitFor(() => expect(body).toEqual({ primaries: "10.0.0.10", tsig_key_id: 0 }));
  await waitFor(() => expect(screen.queryByLabelText(/^primaries$/i)).not.toBeInTheDocument());
});

// The server 400s a stub with no primaries — it would never fetch, so it
// would claim its suffix and SERVFAIL it forever. Caught here only to save
// the round trip, which is the same reason the create row catches it.
test("a stub cannot be saved with no primaries, and the empty value is never sent", async () => {
  const user = userEvent.setup();
  let patched = false;
  renderZoneDetail({ zone: stub(), records: NS_SET });
  server.use(
    http.patch("/api/v1/zones/1", () => {
      patched = true;
      return new HttpResponse(null, { status: 204 });
    }),
  );
  await screen.findByText("ad.corp.example");

  await user.click(screen.getByRole("button", { name: /^edit primaries$/i }));
  await user.clear(screen.getByLabelText(/^primaries$/i));
  await user.click(screen.getByRole("button", { name: /^save$/i }));

  expect(await screen.findByText(/where to fetch from/i)).toBeInTheDocument();
  await new Promise((resolve) => setTimeout(resolve, 30));
  expect(patched).toBe(false);
});

// The server's own words, on the field's own row — the same treatment
// allow_transfer and notify_to already get. The client checks nothing about
// the grammar here (zones.ValidateForwardTo is the real check), so the
// server's message is the only account of what was wrong.
test("a server rejection of the upstreams is surfaced verbatim", async () => {
  const user = userEvent.setup();
  const error = vi.spyOn(toast, "error").mockImplementation(() => "");
  renderZoneDetail({ zone: forwarder() });
  server.use(
    http.patch("/api/v1/zones/1", () =>
      HttpResponse.json({ error: 'forward target "10.0.0.1:99999": bad port' }, { status: 400 }),
    ),
  );
  await screen.findByText("Forward to");

  await user.click(screen.getByRole("button", { name: /^edit upstreams$/i }));
  await user.clear(screen.getByLabelText(/^forward to$/i));
  await user.type(screen.getByLabelText(/^forward to$/i), "10.0.0.1:99999");
  await user.click(screen.getByRole("button", { name: /^save$/i }));

  await waitFor(() =>
    expect(error).toHaveBeenCalledWith('forward target "10.0.0.1:99999": bad port'),
  );
});

// The other half of the poll's bargain, on the type that most looks like it
// should be watched and must not be. A forwarder's upstreams are typed in by
// hand and nothing in the background ever writes to its row — no scheduler
// wakes for it (pullsFromAMaster covers `secondary || stub` only) — so a poll
// here would ask a question whose answer cannot change.
test("a forwarder's page is read once and never again", async () => {
  vi.useFakeTimers({ shouldAdvanceTime: true });
  const paths = trackFetchedPaths();
  renderZoneDetail({ zone: forwarder() });
  await screen.findByText("Forward to");

  await act(async () => {
    await vi.advanceTimersByTimeAsync(5 * 60_000);
  });

  expect(paths.filter((p) => p === "/api/v1/zones/1")).toHaveLength(1);
  // Read once and never rendered: the grid is dropped for a forwarder, so
  // this is one request whose answer is always [] and always unused. Kept
  // rather than gated because the gate would have to wait for the zone query
  // to say what type it is, which is a second round trip's worth of
  // sequencing to save one request that costs nothing.
  expect(paths.filter((p) => p === "/api/v1/zones/1/records")).toHaveLength(1);
});

// The stub half of the same rule, and the reason it is not a forwarder's:
// the scheduler fetches a stub's NS set on the SOA's own schedule, so its row
// does change with nobody touching the page.
test("a stub's page keeps asking, because its NS set arrives on a schedule", async () => {
  vi.useFakeTimers({ shouldAdvanceTime: true });
  const paths = trackFetchedPaths();
  renderZoneDetail({ zone: stub({ refreshed_at: 0 }), records: [] });
  await screen.findByText("Primaries");

  await act(async () => {
    await vi.advanceTimersByTimeAsync(5 * 60_000);
  });

  expect(paths.filter((p) => p === "/api/v1/zones/1").length).toBeGreaterThan(1);
});

// ── A secondary's primaries and key are editable ──────────────────────────
// PATCH /zones/{id} takes both on a secondary, but the page only ever
// printed them: a primary that changed address meant deleting the secondary
// and building it again, losing allow_transfer and notify_to with it.

test("a secondary's primaries can be edited, and the PATCH carries them", async () => {
  const user = userEvent.setup();
  let body: unknown;
  renderZoneDetail({ zone: secondary({ primaries: "203.0.113.9" }) });
  server.use(
    http.patch("/api/v1/zones/1", async ({ request }) => {
      body = await request.json();
      return new HttpResponse(null, { status: 204 });
    }),
  );
  await screen.findByText("e412.in");

  await user.click(await screen.findByRole("button", { name: /^edit primaries$/i }));
  const field = await screen.findByLabelText(/^primaries$/i);
  await user.clear(field);
  await user.type(field, "203.0.113.10, ns2.example.net:5353");
  await user.click(screen.getByRole("button", { name: /^save$/i }));

  await waitFor(() =>
    expect(body).toEqual({
      primaries: "203.0.113.10, ns2.example.net:5353",
      tsig_key_id: 0,
    }),
  );
});

test("a secondary's TSIG key is chosen by name and PATCHed by id", async () => {
  const user = userEvent.setup();
  let body: unknown;
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
    http.patch("/api/v1/zones/1", async ({ request }) => {
      body = await request.json();
      return new HttpResponse(null, { status: 204 });
    }),
  );
  renderZoneDetail({ zone: secondary({ primaries: "203.0.113.9", tsig_key_id: 0 }) });
  await screen.findByText("e412.in");

  await user.click(await screen.findByRole("button", { name: /^edit primaries$/i }));
  await user.selectOptions(await screen.findByLabelText(/^tsig key$/i), "4");
  await user.click(screen.getByRole("button", { name: /^save$/i }));

  await waitFor(() => expect(body).toEqual({ primaries: "203.0.113.9", tsig_key_id: 4 }));
});

// An empty primaries is what checkZoneTransferConfig refuses, so the row
// mirrors that refusal rather than spending a round trip on it.
test("a secondary cannot be saved with no primaries", async () => {
  const user = userEvent.setup();
  let patched = false;
  renderZoneDetail({ zone: secondary({ primaries: "203.0.113.9" }) });
  server.use(
    http.patch("/api/v1/zones/1", () => {
      patched = true;
      return new HttpResponse(null, { status: 204 });
    }),
  );
  await screen.findByText("e412.in");

  await user.click(await screen.findByRole("button", { name: /^edit primaries$/i }));
  await user.clear(await screen.findByLabelText(/^primaries$/i));
  await user.click(screen.getByRole("button", { name: /^save$/i }));

  expect(await screen.findByText(/where to fetch from/i)).toBeInTheDocument();
  await new Promise((resolve) => setTimeout(resolve, 30));
  expect(patched).toBe(false);
});

// A failed key fetch is not evidence that the key is gone. Saying "(missing)"
// for one accused the store of something only the request did.
test("a failed TSIG key fetch does not claim the zone's key is missing", async () => {
  server.use(
    http.get("/api/v1/tsig-keys", () => HttpResponse.json({ error: "boom" }, { status: 500 })),
  );
  renderZoneDetail({ zone: secondary({ tsig_key_id: 3 }) });
  await screen.findByText("e412.in");

  expect(await screen.findByText("#3")).toBeInTheDocument();
  expect(screen.queryByText(/missing/i)).not.toBeInTheDocument();
});

// ── What a failed load actually says ──────────────────────────────────────

// A zone deleted from another tab is not a page that failed to load, and
// "try refreshing" is advice that cannot work — the refresh 404s again.
test("a zone that no longer exists says so, with the way back", async () => {
  server.use(
    http.get("/api/v1/zones/1", () => HttpResponse.json({ error: "not found" }, { status: 404 })),
  );

  renderDetail();

  expect(
    await screen.findByText("This zone no longer exists", undefined, { timeout: 3000 }),
  ).toBeInTheDocument();
  expect(screen.getByRole("link", { name: /zones/i })).toHaveAttribute("href", "/zones");
  expect(screen.queryByText(/try refreshing/i)).not.toBeInTheDocument();
});

// /zones/NaN is a URL that never named a zone, so it is the catch-all's
// question rather than a failed fetch — and nothing is requested for it.
test("a non-numeric id is the not-found screen, and asks the API for nothing", async () => {
  const paths = trackFetchedPaths();

  renderDetail("/zones/wat");

  expect(await screen.findByText("NXDOMAIN")).toBeInTheDocument();
  await new Promise((resolve) => setTimeout(resolve, 30));
  expect(paths.filter((p) => p.startsWith("/api/v1/zones"))).toHaveLength(0);
});

// A record deleted from another tab: the DELETE 404s, and with nothing
// invalidated the row it is about stayed on screen looking live.
test("a 404 on delete drops the row rather than leaving it", async () => {
  const user = userEvent.setup();
  let listReads = 0;
  let gone = false;
  server.use(
    http.get("/api/v1/zones/1/records", () => {
      listReads += 1;
      return HttpResponse.json(gone ? [] : [record({ id: 7, name: "bifrost" })]);
    }),
    http.delete("/api/v1/zones/1/records/7", () => {
      gone = true;
      return HttpResponse.json({ error: "record not found" }, { status: 404 });
    }),
  );
  mockZone(zone({ id: 1 }));
  renderDetail();
  await waitFor(() => expect(recordRows()).toHaveLength(1));
  const before = listReads;

  await user.click(screen.getByRole("button", { name: /delete bifrost a 10\.0\.0\.1/i }));
  await user.click(screen.getByRole("button", { name: /^delete$/i }));

  await waitFor(() => expect(recordRows()).toHaveLength(0));
  expect(listReads).toBeGreaterThan(before);
});

// The same record, edited rather than deleted: "record not found" is not a
// complaint about the rdata that was typed, and leaving the form open on it
// invited retyping a record that no longer exists.
test("a 404 on save closes the edit and refetches instead of blaming the data", async () => {
  const user = userEvent.setup();
  let gone = false;
  server.use(
    http.get("/api/v1/zones/1/records", () =>
      HttpResponse.json(gone ? [] : [record({ id: 7, name: "bifrost" })]),
    ),
    http.put("/api/v1/zones/1/records/7", () => {
      gone = true;
      return HttpResponse.json({ error: "record not found" }, { status: 404 });
    }),
  );
  mockZone(zone({ id: 1 }));
  renderDetail();
  await waitFor(() => expect(recordRows()).toHaveLength(1));

  await user.click(screen.getByRole("button", { name: /edit bifrost a 10\.0\.0\.1/i }));
  const data = await screen.findByLabelText(/^data$/i);
  await user.clear(data);
  await user.type(data, "10.0.0.9");
  await user.click(screen.getByRole("button", { name: /^save$/i }));

  await waitFor(() => expect(screen.queryByTestId("record-form-row")).not.toBeInTheDocument());
  expect(screen.queryByText("record not found")).not.toBeInTheDocument();
  await waitFor(() => expect(recordRows()).toHaveLength(0));
});

// ── Errors the screen reader never heard ──────────────────────────────────

test("a rejected SOA field is announced, not only coloured", async () => {
  const user = userEvent.setup();
  renderZoneDetail({ zone: zone({ id: 1, name: "e412.in" }) });
  await screen.findByText("e412.in");

  await user.click(screen.getByRole("button", { name: /^soa$/i }));
  const refresh = await screen.findByLabelText("Refresh");
  await user.clear(refresh);
  await user.type(refresh, "abc");
  await user.click(screen.getByRole("button", { name: /save soa/i }));

  expect(await screen.findByText("Whole number of seconds")).toBeInTheDocument();
  expect(refresh).toHaveAttribute("aria-invalid", "true");
  expect(refresh).toHaveAccessibleDescription(/whole number of seconds/i);
});

test("a rejected allow transfer is announced on the field itself", async () => {
  const user = userEvent.setup();
  renderZoneDetail({ zone: zone({ id: 1, allow_transfer: "" }) });
  await screen.findByText("example.com");

  await user.click(await screen.findByRole("button", { name: /^edit allow transfer$/i }));
  const field = await screen.findByLabelText(/^allow transfer$/i);
  await user.type(field, "not-an-ip");
  await user.click(screen.getByRole("button", { name: /^save$/i }));

  await screen.findByText(/expected an IP address/i);
  expect(field).toHaveAttribute("aria-invalid", "true");
  expect(field).toHaveAccessibleDescription(/expected an IP address/i);
});

// ── The sizes the grid actually meets ─────────────────────────────────────

// A real zone is not five records. Nothing here is virtualised, so this is
// the assertion that the whole set is rendered rather than silently capped.
test("a zone with hundreds of records renders all of them, and the filter narrows them", async () => {
  const many = Array.from({ length: 250 }, (_, i) =>
    record({ id: i + 1, name: `host${i}`, rdata: `10.0.${Math.floor(i / 256)}.${i % 256}` }),
  );
  mockZone(zone({ id: 1 }));
  mockRecords(1, many);

  renderDetail();
  await waitFor(() => expect(recordRows()).toHaveLength(250), { timeout: 10_000 });
  expect(screen.getByText("250 records")).toBeInTheDocument();

  // Set in one go rather than typed: six keystrokes across 400 rows is six
  // full re-renders, and what is under test is the filtering, not the
  // typing.
  fireEvent.change(screen.getByLabelText(/filter by name/i), { target: { value: "host24" } });
  // host24, host240..host249 — eleven names carry that prefix.
  await waitFor(() => expect(recordRows()).toHaveLength(11));
});

// A DKIM key is longer than the column will ever be. The row truncates it
// and keeps the whole value reachable, rather than wrapping the grid.
test("a very long TXT value stays on one row with the full value in reach", async () => {
  const long = `"v=DKIM1; k=rsa; p=${"A".repeat(392)}"`;
  mockZone(zone({ id: 1 }));
  mockRecords(1, [record({ id: 1, name: "mail._domainkey", type: "TXT", rdata: long })]);

  renderDetail();
  const [row] = await waitFor(() => {
    const found = recordRows();
    expect(found).toHaveLength(1);
    return found;
  });

  const cell = within(row).getByTitle(long);
  expect(cell).toHaveTextContent(long);
  expect(cell).toHaveClass("truncate");
});

// Save is disabled for the whole flight of the request, which is the only
// thing standing between an impatient double-click and two writes.
test("a double-clicked Save sends one request", async () => {
  const user = userEvent.setup();
  let puts = 0;
  let release!: () => void;
  const held = new Promise<void>((resolve) => {
    release = resolve;
  });
  mockZone(zone({ id: 1 }));
  mockRecords(1, [record({ id: 7, name: "bifrost" })]);
  server.use(
    http.put("/api/v1/zones/1/records/7", async () => {
      puts += 1;
      await held;
      return new HttpResponse(null, { status: 204 });
    }),
  );

  renderDetail();
  await waitFor(() => expect(recordRows()).toHaveLength(1));
  await user.click(screen.getByRole("button", { name: /edit bifrost a 10\.0\.0\.1/i }));
  const data = await screen.findByLabelText(/^data$/i);
  await user.clear(data);
  await user.type(data, "10.0.0.9");

  const save = screen.getByRole("button", { name: /^save$/i });
  await user.click(save);
  await waitFor(() => expect(save).toBeDisabled());
  fireEvent.click(save);

  release();
  await waitFor(() => expect(screen.queryByTestId("record-form-row")).not.toBeInTheDocument());
  expect(puts).toBe(1);
});

// ── The scheduler's own view, beside the durable one ──────────────────────

/**
 * `next_attempt_at` and `failures` come from the running process rather than
 * a column, so they are the two things the last-error line cannot say: when
 * the retry actually falls, and how many have gone the same way. They sit
 * beside that line rather than replacing it, because a restart forgets them
 * and the line does not.
 */
test("a failing secondary says when the next attempt falls and how many have failed", async () => {
  renderZoneDetail({
    zone: secondary({
      last_error: "dial tcp 203.0.113.9:53: connect: connection refused",
      last_attempt: Date.now() - 60_000,
      // 4m30s, so the flooring in formatDuration reads 4m however long the
      // render takes rather than tipping to 3m on a slow one.
      next_attempt_at: Date.now() + 4 * 60_000 + 30_000,
      failures: 3,
    }),
  });
  await screen.findByText("e412.in");

  expect(screen.getByTestId("scheduler-note").textContent).toBe(
    "next attempt in 4m · 3 failed attempts",
  );
});

test("a healthy secondary has no scheduler note", async () => {
  renderZoneDetail({ zone: secondary() });
  await screen.findByText("e412.in");

  expect(screen.queryByTestId("scheduler-note")).not.toBeInTheDocument();
});

// A stub has no transfer band; its failures are reported on the primaries
// row, and that is where the same two facts belong for one.
test("a failing stub says the same beside its fetch error", async () => {
  renderZoneDetail({
    zone: stub({
      refreshed_at: Date.now() - 3 * 86_400_000,
      last_error: "i/o timeout",
      last_attempt: Date.now() - 12 * 60_000,
      next_attempt_at: Date.now() + 90_000,
      failures: 2,
    }),
    records: NS_SET,
  });
  await screen.findByText("ad.corp.example");

  expect(screen.getByTestId("scheduler-note").textContent).toBe(
    "next attempt in 1m · 2 failed attempts",
  );
});

// ── Bulk TTL over the filtered set ────────────────────────────────────────

/**
 * The action is on the filter bar and only while a filter is on, because the
 * filtered set is the only thing it can mean: with no filter it would read as
 * "retune the whole zone", which is a much bigger claim than the button has
 * any way to confirm.
 */
test("the bulk TTL action appears only while a filter narrows the list", async () => {
  const user = userEvent.setup();
  renderZoneDetail({
    zone: zone(),
    records: [
      record({ id: 1, name: "web", rdata: "10.0.0.1" }),
      record({ id: 2, name: "nas", rdata: "10.0.0.9" }),
    ],
  });
  await screen.findByText("example.com");

  expect(screen.queryByRole("button", { name: /set ttl/i })).not.toBeInTheDocument();

  await user.type(screen.getByLabelText(/filter by name/i), "web");

  expect(
    await screen.findByRole("button", { name: /set ttl for 1 filtered record$/i }),
  ).toBeInTheDocument();
});

test("setting the TTL PATCHes exactly the filtered ids", async () => {
  const user = userEvent.setup();
  let body: unknown;
  server.use(
    http.patch("/api/v1/zones/1/records", async ({ request }) => {
      body = await request.json();
      return new HttpResponse(null, { status: 204 });
    }),
  );
  renderZoneDetail({
    zone: zone(),
    records: [
      record({ id: 1, name: "web", rdata: "10.0.0.1" }),
      record({ id: 2, name: "web", rdata: "10.0.0.2" }),
      record({ id: 3, name: "nas", rdata: "10.0.0.9" }),
    ],
  });
  await screen.findByText("example.com");
  await user.type(screen.getByLabelText(/filter by name/i), "web");

  await user.click(await screen.findByRole("button", { name: /set ttl for 2 filtered records/i }));
  const dialog = await screen.findByRole("dialog");
  await user.type(within(dialog).getByLabelText(/ttl/i), "60");
  await user.click(within(dialog).getByRole("button", { name: /^set ttl$/i }));

  // The ids the grid is showing, not a filter the server would have to
  // re-derive: the row the user can see is the row that changes.
  await waitFor(() => expect(body).toEqual({ ids: [1, 2], ttl: 60 }));
});

// The RRSet rule is the server's, and its refusal is the one worth showing:
// a client-side copy of RFC 2181 §5.2 would be a second rule to drift.
test("a refused bulk TTL keeps the dialog open and shows the server's reason", async () => {
  const user = userEvent.setup();
  server.use(
    http.patch("/api/v1/zones/1/records", () =>
      HttpResponse.json({ error: "records in the same RRSet must share one TTL" }, { status: 409 }),
    ),
  );
  const errorSpy = vi.spyOn(toast, "error");
  renderZoneDetail({
    zone: zone(),
    records: [
      record({ id: 1, name: "web", rdata: "10.0.0.1" }),
      record({ id: 2, name: "web", rdata: "10.0.0.2" }),
    ],
  });
  await screen.findByText("example.com");
  await user.type(screen.getByLabelText(/filter by name/i), "web");
  await user.click(await screen.findByRole("button", { name: /set ttl for 2 filtered records/i }));
  const dialog = await screen.findByRole("dialog");
  await user.type(within(dialog).getByLabelText(/ttl/i), "60");
  await user.click(within(dialog).getByRole("button", { name: /^set ttl$/i }));

  await waitFor(() =>
    expect(errorSpy).toHaveBeenCalledWith("records in the same RRSet must share one TTL"),
  );
  expect(screen.getByRole("dialog")).toBeInTheDocument();
});

// A secondary's records are its primary's, and the server 409s every write
// into one — so the control that would produce that 409 is not drawn, the
// same rule the add and edit controls already follow.
test("a zone whose records are read-only offers no bulk TTL", async () => {
  renderZoneDetail({
    zone: secondary(),
    records: [record({ id: 1, zone_id: 1, name: "web", rdata: "10.0.0.1" })],
  });
  await screen.findByText("e412.in");

  expect(screen.queryByRole("button", { name: /set ttl/i })).not.toBeInTheDocument();
});

// ── Copy buttons ──────────────────────────────────────────────────────────

/**
 * The values on this page that get copied into somebody else's config — a
 * primary's ACL entry, a notify target, the SOA's two names, the serial an
 * operator is comparing against `dig` — get a button rather than a
 * select-and-drag over mono text in a truncating cell.
 */
test("the SOA band's names and serial carry copy buttons", async () => {
  const user = userEvent.setup();
  renderZoneDetail({ zone: zone({ soa_serial: 42 }) });
  await screen.findByText("example.com");

  // The fields live inside the collapsed SOA section.
  await user.click(screen.getByRole("button", { name: /^soa$/i }));

  expect(screen.getByRole("button", { name: /copy primary ns/i })).toBeInTheDocument();
  expect(screen.getByRole("button", { name: /copy responsible/i })).toBeInTheDocument();
  expect(screen.getByRole("button", { name: /copy serial/i })).toBeInTheDocument();
});

test("allow_transfer and notify_to carry copy buttons when they have a value", async () => {
  renderZoneDetail({
    zone: zone({ allow_transfer: "10.0.0.0/24, key:ns2.", notify_to: "10.0.0.2:53" }),
  });
  await screen.findByText("example.com");

  expect(screen.getByRole("button", { name: /copy allow transfer/i })).toBeInTheDocument();
  expect(screen.getByRole("button", { name: /copy notify targets/i })).toBeInTheDocument();
});

// Nothing to copy is not a button that copies nothing — the same rule the
// primaries row already follows for an empty value.
test("an empty allow_transfer and notify_to carry no copy button", async () => {
  renderZoneDetail({ zone: zone({ allow_transfer: "", notify_to: "" }) });
  await screen.findByText("example.com");

  expect(screen.queryByRole("button", { name: /copy allow transfer/i })).not.toBeInTheDocument();
  expect(screen.queryByRole("button", { name: /copy notify targets/i })).not.toBeInTheDocument();
});

// ── Keyboard shortcuts ────────────────────────────────────────────────────

test("/ focuses the record filter", async () => {
  renderZoneDetail({ zone: zone(), records: [record({ id: 1 })] });
  await screen.findByText("example.com");

  fireEvent.keyDown(document.body, { key: "/" });

  expect(screen.getByLabelText(/filter by name/i)).toHaveFocus();
});

test("n opens the add-record row and Esc closes it", async () => {
  renderZoneDetail({ zone: zone(), records: [record({ id: 1 })] });
  await screen.findByText("example.com");
  expect(screen.queryByLabelText(/zone record name/i)).not.toBeInTheDocument();

  fireEvent.keyDown(document.body, { key: "n" });
  const name = await screen.findByLabelText(/zone record name/i);

  // Esc from inside the form, which is where the focus already is: a
  // shortcut that only worked with focus somewhere else could never close
  // the thing it just opened.
  fireEvent.keyDown(name, { key: "Escape" });
  await waitFor(() => expect(screen.queryByLabelText(/zone record name/i)).not.toBeInTheDocument());
});

test("Esc closes an open edit row", async () => {
  const user = userEvent.setup();
  renderZoneDetail({ zone: zone(), records: [record({ id: 1, name: "bifrost" })] });
  await screen.findByText("example.com");
  await user.click(screen.getByRole("button", { name: /edit bifrost A 10.0.0.1/i }));
  await screen.findByLabelText(/zone record name/i);

  fireEvent.keyDown(document.body, { key: "Escape" });

  await waitFor(() => expect(screen.queryByLabelText(/zone record name/i)).not.toBeInTheDocument());
});

// A shortcut that fires while someone is typing is a shortcut that eats their
// text: "n" belongs in a record name, and "/" in a filter.
test("the shortcuts are inert while focus is in a field", async () => {
  const user = userEvent.setup();
  renderZoneDetail({ zone: zone(), records: [record({ id: 1 })] });
  await screen.findByText("example.com");

  const filter = screen.getByLabelText(/filter by name/i);
  await user.type(filter, "n");

  expect(filter).toHaveValue("n");
  expect(screen.queryByLabelText(/zone record name/i)).not.toBeInTheDocument();
});

// A zone whose records are written elsewhere has no add band to open, so the
// shortcut that opens one must not appear to work.
test("n does nothing on a zone whose records are read-only", async () => {
  renderZoneDetail({
    zone: secondary(),
    records: [record({ id: 1, zone_id: 1 })],
  });
  await screen.findByText("e412.in");

  fireEvent.keyDown(document.body, { key: "n" });

  expect(screen.queryByLabelText(/zone record name/i)).not.toBeInTheDocument();
});

test("the filter bar says what the two keys do", async () => {
  renderZoneDetail({ zone: zone(), records: [record({ id: 1 })] });
  await screen.findByText("example.com");

  const hint = screen.getByTestId("record-shortcuts");
  expect(hint).toHaveTextContent("/");
  expect(hint).toHaveTextContent(/filter/i);
  expect(hint).toHaveTextContent("n");
  expect(hint).toHaveTextContent(/add/i);
});
