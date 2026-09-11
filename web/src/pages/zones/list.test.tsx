import { http, HttpResponse } from "msw";
import { toast } from "sonner";
import { afterEach, expect, test, vi } from "vitest";
import { act, fireEvent, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useEffect } from "react";
import { useForm } from "react-hook-form";
import { Form } from "@e412/rnui-react";
import { server } from "../../test/msw-server";
import { renderWithProviders } from "../../test/render";
import type { Zone, ZoneRecord } from "../../api/types";
import { CreateRowHint, TSIGKeyField, UpstreamField, ZonesList, type AddZoneValues } from "./list";

function zone(overrides: Partial<Zone> = {}): Zone {
  return {
    id: 1,
    name: "example.com",
    type: "primary",
    enabled: true,
    soa_ns: "ns.example.com",
    soa_mbox: "hostadmin.example.com",
    soa_serial: 3,
    soa_refresh: 900,
    soa_retry: 300,
    soa_expire: 604800,
    soa_minimum: 900,
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

function zoneRecord(zoneId: number, id: number): ZoneRecord {
  return {
    id,
    zone_id: zoneId,
    name: "@",
    type: "A",
    ttl: 300,
    rdata: "10.0.0.1",
    enabled: true,
    comment: "",
  };
}

function mockZones(zones: Zone[]) {
  server.use(http.get("/api/v1/zones", () => HttpResponse.json(zones)));
}

function mockZoneRecords(zoneId: number, records: ZoneRecord[]) {
  server.use(http.get(`/api/v1/zones/${zoneId}/records`, () => HttpResponse.json(records)));
}

function zoneRows(): HTMLElement[] {
  return Array.from(document.querySelectorAll<HTMLElement>('[data-testid="zone-row"]'));
}

/**
 * Hovers a row's status cell and returns the tooltip it opens.
 *
 * The popup is portalled to the end of the body, so it is looked up on
 * `screen` rather than `within(row)` — which is also what makes "the label is
 * nowhere in the row" a real assertion rather than one the tooltip's own copy
 * would satisfy from inside it.
 */
async function statusTip(
  user: ReturnType<typeof userEvent.setup>,
  row: HTMLElement,
): Promise<HTMLElement> {
  await user.hover(within(row).getByTestId("zone-status"));
  return await screen.findByTestId("zone-status-tip");
}

/**
 * Opens a row's actions menu and returns the popup.
 *
 * `fireEvent` rather than `userEvent`, the same way groups-clients.test.tsx
 * drives its dropdown: base-ui's menu opens off a pointerdown sequence that
 * jsdom cannot complete.
 *
 * The popup is portalled to the end of the body, so it is looked up on
 * `screen` and asserted on `within(menu)` rather than inside the row — which
 * is what keeps "the row says nothing about transfers" a real assertion
 * rather than one an open menu would satisfy from inside it.
 */
async function rowMenu(row: HTMLElement, zoneName: string): Promise<HTMLElement> {
  fireEvent.click(within(row).getByRole("button", { name: `Actions for ${zoneName}` }));
  return await screen.findByRole("menu");
}

async function openCreateRow(user: ReturnType<typeof userEvent.setup>) {
  // findAllBy* rather than findBy*: an empty list shows "New zone" twice
  // (header + empty-state body), and this only needs one of them clicked.
  const [button] = await screen.findAllByRole("button", { name: /new zone/i });
  await user.click(button);
}

/**
 * Renders the three type-dependent create-row cells directly, with `type` set
 * by the caller rather than driven through the real select.
 *
 * The select now offers all four creatable types, so most of this is
 * reachable through the real row too — but `internal` is not, and never will
 * be (the API refuses it), and driving four types through a select for a
 * question about one cell is four times the setup for the same assertion.
 */
function TypeAwareFieldsHarness({
  type,
  fieldError,
}: {
  type: Zone["type"];
  /**
   * A validation error to seed onto one of the two upstream fields, so the
   * hint's error branch is reachable without a schema rule to trip. The
   * `forward_to` half has no rule today — a forwarder's upstreams may
   * legitimately be empty and nothing else here parses them — and that is
   * exactly why it is worth pinning: the wiring has to already carry an
   * error the day someone adds one.
   *
   * Seeded onto the *form*, never handed to CreateRowHint as a prop. The cell
   * reads its own errors off the control, so passing one in would have tested
   * the harness rather than the wiring — which is exactly how the first
   * version of this test passed against a cell that could only ever render
   * `primaries`.
   */
  fieldError?: { name: "primaries" | "forward_to"; message: string };
}) {
  const form = useForm<AddZoneValues>({
    defaultValues: { name: "", type: "primary", primaries: "", forward_to: "", tsig_key_id: "" },
  });
  useEffect(() => {
    if (fieldError) form.setError(fieldError.name, { message: fieldError.message });
  }, [fieldError, form]);
  return (
    <Form {...form}>
      <UpstreamField type={type} control={form.control} />
      <TSIGKeyField type={type} control={form.control} />
      <CreateRowHint type={type} control={form.control} />
    </Form>
  );
}

/**
 * Every path MSW served during the test — the only way to prove a *negative*
 * about the network, which the polling tests below are mostly made of.
 * Torn down in the afterEach alongside MSW's own per-test handler reset.
 */
function trackFetchedPaths(): string[] {
  const paths: string[] = [];
  server.events.on("request:start", ({ request }) => {
    paths.push(new URL(request.url).pathname);
  });
  return paths;
}

/** How many times one endpoint was actually read. */
function reads(paths: string[], path: string): number {
  return paths.filter((p) => p === path).length;
}

afterEach(() => {
  vi.restoreAllMocks();
  vi.useRealTimers();
  server.events.removeAllListeners();
});

// The RFC 6303 zones exist to stop junk queries reaching the roots;
// offering a delete that the API refuses is a button that lies. Built-ins
// sit behind the collapsed disclosure now, so it has to be opened first.
test("internal zones cannot be deleted", async () => {
  const user = userEvent.setup();
  mockZones([
    zone({ id: 1, name: "e412.in", type: "primary" }),
    zone({ id: 2, name: "localhost", type: "internal" }),
  ]);
  renderWithProviders(<ZonesList />);
  await user.click(await screen.findByRole("button", { name: /built-in zone/i }));
  const rows = await screen.findAllByTestId("zone-row");

  expect(rows).toHaveLength(2);
  // The user's own zone has an actions menu, and Delete zone inside it.
  const menu = await rowMenu(rows[0], "e412.in");
  expect(within(menu).getByRole("menuitem", { name: "Delete zone" })).toBeInTheDocument();
  // The built-in has no menu for one to be in.
  expect(within(rows[1]).queryByRole("button", { name: /^actions for/i })).not.toBeInTheDocument();
});

// The artboard's stronger claim: an internal zone's Actions cell doesn't
// merely omit buttons, it says BUILT-IN outright, and the name itself is
// plain text with a padlock rather than the primary-coloured link editable
// zones get — three separate "this is not yours to change" signals, all
// pinned together since they're one design decision. Only one built-in
// zone here, so the disclosure's own label is singular too.
test("an internal zone shows Built-in instead of action buttons, and a padlock beside its plain (non-link) name", async () => {
  const user = userEvent.setup();
  mockZones([zone({ id: 2, name: "localhost", type: "internal" })]);
  renderWithProviders(<ZonesList />);
  await user.click(await screen.findByRole("button", { name: /1 built-in zone/i }));
  const rows = await screen.findAllByTestId("zone-row");

  expect(within(rows[0]).getByText(/built-in/i)).toBeInTheDocument();
  expect(within(rows[0]).queryByRole("button")).not.toBeInTheDocument();
  expect(within(rows[0]).queryByRole("link", { name: "localhost" })).not.toBeInTheDocument();
  expect(within(rows[0]).getByText("localhost")).toBeInTheDocument();
});

// The redesign's whole point: RFC 6303 seeds 15 built-in zones (see
// internal/store/builtins.go), which must never make the header count or
// the empty state believe the user has zones of their own. The bug this
// pins: counting `all.length`/`all.length === 0` over the *whole* list
// (built-ins included) would report "15 zones" and could never show "No
// zones yet" on a fresh install, since the built-ins are always present.
test("the header count and the empty state ignore built-in zones — a list of only built-ins shows No zones yet", async () => {
  mockZones(
    Array.from({ length: 15 }, (_, i) =>
      zone({ id: i + 1, name: `builtin${i}.arpa`, type: "internal" }),
    ),
  );
  renderWithProviders(<ZonesList />);

  expect(await screen.findByText("No zones yet")).toBeInTheDocument();
  expect(screen.getByText("Everything is forwarded upstream.")).toBeInTheDocument();
  expect(screen.getByText("0 zones")).toBeInTheDocument();
  expect(screen.queryByText("15 zones")).not.toBeInTheDocument();
  // The 15 built-ins still exist — just behind their own disclosure, not
  // counted as the user's own.
  expect(screen.getByRole("button", { name: /15 built-in zones/i })).toBeInTheDocument();
});

// Default state is collapsed, and the built-in rows genuinely aren't in
// the DOM (not merely hidden) — the whole reason this redesign exists is
// to stop 15 RFC 6303 zones from burying the one or two a user cares about.
test("the built-ins disclosure is collapsed by default and its rows are not rendered", async () => {
  mockZones([
    zone({ id: 1, name: "e412.in" }),
    zone({ id: 2, name: "localhost", type: "internal" }),
  ]);
  renderWithProviders(<ZonesList />);

  const trigger = await screen.findByRole("button", { name: /built-in zone/i });
  expect(trigger).toHaveAttribute("aria-expanded", "false");
  expect(screen.getAllByTestId("zone-row")).toHaveLength(1);
  expect(screen.queryByText("localhost")).not.toBeInTheDocument();
});

test("expanding the disclosure reveals built-in rows with a BUILT-IN cell and no actions menu", async () => {
  const user = userEvent.setup();
  mockZones([
    zone({ id: 1, name: "e412.in" }),
    zone({ id: 2, name: "localhost", type: "internal" }),
  ]);
  renderWithProviders(<ZonesList />);
  const trigger = await screen.findByRole("button", { name: /built-in zone/i });

  await user.click(trigger);

  expect(trigger).toHaveAttribute("aria-expanded", "true");
  const rows = await screen.findAllByTestId("zone-row");
  expect(rows).toHaveLength(2);
  const builtinRow = rows[1];
  expect(within(builtinRow).getByText("localhost")).toBeInTheDocument();
  expect(within(builtinRow).getByText(/built-in/i)).toBeInTheDocument();
  // Asserted against the editable row beside it, so this is a difference
  // between the two rather than a query that would match nothing anywhere.
  expect(within(rows[0]).getByRole("button", { name: "Actions for e412.in" })).toBeInTheDocument();
  expect(
    within(builtinRow).queryByRole("button", { name: /^actions for/i }),
  ).not.toBeInTheDocument();
});

test("the built-ins disclosure label pluralises the count", async () => {
  mockZones([zone({ id: 1, name: "localhost", type: "internal" })]);
  const single = renderWithProviders(<ZonesList />);
  const singleTrigger = await screen.findByRole("button", { name: /built-in zone/i });
  expect(within(singleTrigger).getByText("1 built-in zone")).toBeInTheDocument();
  single.unmount();

  mockZones(
    Array.from({ length: 15 }, (_, i) =>
      zone({ id: i + 1, name: `builtin${i}.arpa`, type: "internal" }),
    ),
  );
  renderWithProviders(<ZonesList />);
  const manyTrigger = await screen.findByRole("button", { name: /built-in zone/i });
  expect(within(manyTrigger).getByText("15 built-in zones")).toBeInTheDocument();
});

// It's a real control (role="button" from a native <button>, keyboard
// focusable) — not a clickable div that only reacts to a mouse.
test("the built-ins disclosure toggles on Enter or Space, not just click", async () => {
  const user = userEvent.setup();
  mockZones([
    zone({ id: 1, name: "e412.in" }),
    zone({ id: 2, name: "localhost", type: "internal" }),
  ]);
  renderWithProviders(<ZonesList />);
  const trigger = await screen.findByRole("button", { name: /built-in zone/i });

  trigger.focus();
  await user.keyboard("{Enter}");
  expect(trigger).toHaveAttribute("aria-expanded", "true");
  expect(await screen.findAllByTestId("zone-row")).toHaveLength(2);

  await user.keyboard(" ");
  expect(trigger).toHaveAttribute("aria-expanded", "false");
  await waitFor(() => expect(screen.getAllByTestId("zone-row")).toHaveLength(1));
});

test("the create row posts the typed name and closes", async () => {
  const user = userEvent.setup();
  let body: unknown;
  mockZones([]);
  server.use(
    http.post("/api/v1/zones", async ({ request }) => {
      body = await request.json();
      return HttpResponse.json({ id: 9 }, { status: 201 });
    }),
  );

  renderWithProviders(<ZonesList />);
  await openCreateRow(user);
  await user.type(screen.getByLabelText(/zone name/i), "  Nexus.Example.com  ");
  await user.click(screen.getByRole("button", { name: /^add$/i }));

  // Trimmed, and nothing but the name — the type select defaults to
  // primary, which the server already treats a missing type as.
  await waitFor(() => expect(body).toEqual({ name: "Nexus.Example.com" }));
  // Closes on success: the create row is gone.
  await waitFor(() => expect(screen.queryByLabelText(/zone name/i)).not.toBeInTheDocument());
});

test("an invalid zone name is rejected client-side and never posted", async () => {
  const user = userEvent.setup();
  let posted = false;
  mockZones([]);
  server.use(
    http.post("/api/v1/zones", () => {
      posted = true;
      return HttpResponse.json({ id: 9 }, { status: 201 });
    }),
  );

  renderWithProviders(<ZonesList />);
  await openCreateRow(user);
  await user.type(screen.getByLabelText(/zone name/i), "has spaces.example.com");
  await user.click(screen.getByRole("button", { name: /^add$/i }));

  expect(await screen.findByText(/enter a domain name/i)).toBeInTheDocument();
  await new Promise((resolve) => setTimeout(resolve, 30));
  expect(posted).toBe(false);
});

// Exact copy from the artboard — "state the fact and stop".
test("shows an empty state with the artboard's exact copy and a New zone action", async () => {
  mockZones([]);
  renderWithProviders(<ZonesList />);

  expect(await screen.findByText("No zones yet")).toBeInTheDocument();
  expect(screen.getByText("Everything is forwarded upstream.")).toBeInTheDocument();
  expect(screen.getAllByRole("button", { name: /new zone/i }).length).toBeGreaterThan(0);
});

test("deleting a zone asks for confirmation, then DELETEs it", async () => {
  const user = userEvent.setup();
  let deleted = false;
  mockZones([zone({ id: 7, name: "old.example.com" })]);
  server.use(
    http.delete("/api/v1/zones/7", () => {
      deleted = true;
      return new HttpResponse(null, { status: 204 });
    }),
  );

  renderWithProviders(<ZonesList />);
  await waitFor(() => expect(zoneRows()).toHaveLength(1));

  const menu = await rowMenu(zoneRows()[0], "old.example.com");
  fireEvent.click(within(menu).getByRole("menuitem", { name: "Delete zone" }));
  const confirm = await screen.findByRole("alertdialog");
  await user.click(within(confirm).getByRole("button", { name: /^delete$/i }));

  await waitFor(() => expect(deleted).toBe(true));
});

test("a failed delete shows a toast and leaves the zone in the list", async () => {
  const user = userEvent.setup();
  mockZones([zone({ id: 7, name: "old.example.com" })]);
  server.use(
    http.delete("/api/v1/zones/7", () => HttpResponse.json({ error: "boom" }, { status: 500 })),
  );
  const errorSpy = vi.spyOn(toast, "error");

  renderWithProviders(<ZonesList />);
  await waitFor(() => expect(zoneRows()).toHaveLength(1));

  const menu = await rowMenu(zoneRows()[0], "old.example.com");
  fireEvent.click(within(menu).getByRole("menuitem", { name: "Delete zone" }));
  const confirm = await screen.findByRole("alertdialog");
  await user.click(within(confirm).getByRole("button", { name: /^delete$/i }));

  await waitFor(() =>
    expect(errorSpy).toHaveBeenCalledWith(
      expect.stringMatching(/couldn't delete old\.example\.com/i),
    ),
  );
  expect(zoneRows()).toHaveLength(1);
});

// soa_serial is how a user sees that a zone changed. The artboard also
// draws a deliberate line between it and the records count beside it: a
// serial is an opaque counter (no thousands grouping), a records count is a
// quantity (grouped) — see the next test for that half.
test("an editable zone's name is a link to its detail page, and its serial renders without thousands grouping", async () => {
  mockZones([
    zone({ id: 1, name: "example.com", type: "primary", soa_serial: 1234567, enabled: true }),
  ]);
  renderWithProviders(<ZonesList />);
  const rows = await screen.findAllByTestId("zone-row");

  const link = within(rows[0]).getByRole("link", { name: "example.com" });
  expect(link).toHaveAttribute("href", "/zones/1");
  expect(within(rows[0]).getByText("primary")).toBeInTheDocument();
  expect(within(rows[0]).getByText("Enabled")).toBeInTheDocument();
  expect(within(rows[0]).getByText("1234567")).toBeInTheDocument();
  expect(within(rows[0]).queryByText("1,234,567")).not.toBeInTheDocument();
});

test("a disabled zone's status reads Disabled", async () => {
  mockZones([zone({ id: 3, name: "off.example.com", enabled: false })]);
  renderWithProviders(<ZonesList />);

  expect(await screen.findByText("Disabled")).toBeInTheDocument();
});

// Narrowing the create select to primary-only must not touch how existing
// rows render — a secondary/stub/forwarder/internal zone can still arrive
// from a direct DB write or a future migration, and a row that can't
// render its own type is worse than one that can.
test("an existing zone row of each non-primary type still renders its badge, and internal also its Built-in cell", async () => {
  const user = userEvent.setup();
  mockZones([
    zone({ id: 11, name: "sec.example.com", type: "secondary" }),
    zone({ id: 12, name: "stub.example.com", type: "stub" }),
    zone({ id: 13, name: "fwd.example.com", type: "forwarder" }),
    zone({ id: 14, name: "localhost", type: "internal" }),
  ]);
  renderWithProviders(<ZonesList />);
  await user.click(await screen.findByRole("button", { name: /built-in zone/i }));
  const rows = await screen.findAllByTestId("zone-row");

  expect(rows).toHaveLength(4);
  expect(within(rows[0]).getByText("secondary")).toBeInTheDocument();
  expect(within(rows[1]).getByText("stub")).toBeInTheDocument();
  expect(within(rows[2]).getByText("forwarder")).toBeInTheDocument();
  expect(within(rows[3]).getByText("internal")).toBeInTheDocument();
  expect(within(rows[3]).getByText(/built-in/i)).toBeInTheDocument();
});

test("the records count groups with thousands separators", async () => {
  mockZones([zone({ id: 5, name: "big.example.com" })]);
  mockZoneRecords(
    5,
    Array.from({ length: 1234 }, (_, i) => zoneRecord(5, i + 1)),
  );

  renderWithProviders(<ZonesList />);
  expect(await screen.findByText("1,234")).toBeInTheDocument();
});

// The type select offers exactly what the API accepts. The artboard draws
// four options — primary, secondary, stub, forwarder — and two of them 400:
// `forwarder` is Milestone D6 and `stub` is in no milestone at all. An option
// that always fails is worse than an absent one, so this pins the list rather
// than leaving it to drift back to the artboard's.
// Exactly the four the API creates or patches into (handleZoneCreate). The
// fifth, `internal`, is the RFC 6303 set seeded at migration and is refused
// with 400 — a select offering it would promise a zone type and hand back a
// server error.
test("the type select offers all four creatable types, and nothing the API would refuse", async () => {
  const user = userEvent.setup();
  mockZones([]);
  renderWithProviders(<ZonesList />);
  await openCreateRow(user);

  const select = screen.getByLabelText("Zone type");
  expect(Array.from(select.querySelectorAll("option")).map((option) => option.value)).toEqual([
    "primary",
    "secondary",
    "stub",
    "forwarder",
  ]);
});

test("creating a primary posts the name alone — no type, no transfer fields", async () => {
  const user = userEvent.setup();
  let body: unknown;
  mockZones([]);
  server.use(
    http.post("*/api/v1/zones", async ({ request }) => {
      body = await request.json();
      return HttpResponse.json({ id: 1 }, { status: 201 });
    }),
  );
  renderWithProviders(<ZonesList />);
  await openCreateRow(user);

  const row = document.querySelector<HTMLElement>('[data-slot="add-zone-row"]')!;
  await user.type(within(row).getByPlaceholderText("home.lan"), "e412.in");
  await user.click(within(row).getByRole("button", { name: "Add" }));

  // `type` is omitted, not sent as "primary": onSubmit drops it when it is
  // the default, and handleZoneCreate reads an absent type as primary.
  // primaries and tsig_key_id are omitted too, and must be — the server 400s
  // either one on a zone that is not a secondary.
  await waitFor(() => expect(body).toEqual({ name: "e412.in" }));
});

// The artboard values this select by key NAME ("xfer.e412.in."). The API
// field is tsig_key_id. Building it as drawn needs a name->id lookup at
// submit time, and the failure mode when that lookup misses is silent: the
// zone is created signing with the wrong key, or none, and nothing says so
// until a transfer is refused. This pins the id on the wire.
test("creating a secondary posts primaries and the TSIG key's id, not its name", async () => {
  const user = userEvent.setup();
  let body: unknown;
  mockZones([]);
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
    http.post("*/api/v1/zones", async ({ request }) => {
      body = await request.json();
      return HttpResponse.json({ id: 1 }, { status: 201 });
    }),
  );
  renderWithProviders(<ZonesList />);
  await openCreateRow(user);

  await user.selectOptions(screen.getByLabelText("Zone type"), "secondary");
  await user.type(screen.getByLabelText(/zone name/i), "e412.in");
  await user.type(screen.getByLabelText(/primary servers/i), "203.0.113.9, ns2.example.net:5353");
  await user.selectOptions(await screen.findByLabelText(/tsig key/i), "4");
  await user.click(screen.getByRole("button", { name: "Add" }));

  await waitFor(() =>
    expect(body).toEqual({
      name: "e412.in",
      type: "secondary",
      primaries: "203.0.113.9, ns2.example.net:5353",
      tsig_key_id: 4,
    }),
  );
});

// An unsigned transfer is a legitimate configuration, and 0 is what the
// server reads an omitted tsig_key_id as — so "No TSIG" puts no field on the
// wire rather than an explicit zero.
test("a secondary with no TSIG key omits tsig_key_id entirely", async () => {
  const user = userEvent.setup();
  let body: unknown;
  mockZones([]);
  server.use(
    http.post("*/api/v1/zones", async ({ request }) => {
      body = await request.json();
      return HttpResponse.json({ id: 1 }, { status: 201 });
    }),
  );
  renderWithProviders(<ZonesList />);
  await openCreateRow(user);

  await user.selectOptions(screen.getByLabelText("Zone type"), "secondary");
  await user.type(screen.getByLabelText(/zone name/i), "e412.in");
  await user.type(screen.getByLabelText(/primary servers/i), "203.0.113.9");
  await user.click(screen.getByRole("button", { name: "Add" }));

  await waitFor(() =>
    expect(body).toEqual({ name: "e412.in", type: "secondary", primaries: "203.0.113.9" }),
  );
});

// The server refuses this and is right to: a secondary with nowhere to pull
// from can never transfer, so it would answer SERVFAIL for its whole suffix
// forever. Caught client-side only to save the round trip — and the message
// has to land under the field it is about, not in column 1 where the name's
// errors go.
test("a secondary with no primaries is rejected client-side and never posted", async () => {
  const user = userEvent.setup();
  let posted = false;
  mockZones([]);
  server.use(
    http.post("*/api/v1/zones", () => {
      posted = true;
      return HttpResponse.json({ id: 1 }, { status: 201 });
    }),
  );
  renderWithProviders(<ZonesList />);
  await openCreateRow(user);

  await user.selectOptions(screen.getByLabelText("Zone type"), "secondary");
  await user.type(screen.getByLabelText(/zone name/i), "e412.in");
  await user.click(screen.getByRole("button", { name: "Add" }));

  expect(await screen.findByText(/where to pull from/i)).toBeInTheDocument();
  await new Promise((resolve) => setTimeout(resolve, 30));
  expect(posted).toBe(false);
});

// One cell, four behaviours. A secondary and a stub both pull from a master,
// so both get "Primary servers" — the API validates `primaries` identically
// for the two (checkZoneTransferConfig's pullsAZone). A forwarder does not
// have primaries at all: it has upstreams it sends queries to, which is a
// different column (`forward_to`) the server 400s on any other type. The
// artboard's own `needsPrimaries` covers all three with one field, and
// building it as drawn would have posted `primaries` for a forwarder and
// been refused.
test("the extra field's label and placeholder follow the type, and only the master-pulling types get TSIG", () => {
  // A fresh render per type rather than rerender(): rerender replaces the
  // whole tree, wrapper included, and TSIGKeyField reads a query client.
  for (const type of ["secondary", "stub"] as const) {
    const view = renderWithProviders(<TypeAwareFieldsHarness type={type} />);
    expect(screen.getByLabelText(/^primary servers$/i)).toHaveAttribute(
      "placeholder",
      "192.168.150.1:53",
    );
    expect(screen.getByLabelText(/tsig key/i)).toBeInTheDocument();
    expect(screen.getByText("Primary servers, comma separated.")).toBeInTheDocument();
    expect(screen.getByText("Optional.")).toBeInTheDocument();
    expect(screen.queryByText("SOA defaults are filled in.")).not.toBeInTheDocument();
    view.unmount();
  }

  const forwarder = renderWithProviders(<TypeAwareFieldsHarness type="forwarder" />);
  expect(screen.getByLabelText(/^forward to$/i)).toHaveAttribute(
    "placeholder",
    "10.0.0.1, 10.0.0.2:5353",
  );
  expect(screen.queryByLabelText(/^primary servers$/i)).not.toBeInTheDocument();
  // A forwarder signs nothing — it sends ordinary queries, not transfers —
  // and tsig_key_id is 400ed on it (checkZoneTransferConfig).
  expect(screen.queryByLabelText(/tsig key/i)).not.toBeInTheDocument();
  expect(screen.getByText("Upstream servers, comma separated.")).toBeInTheDocument();
  // Column 1's hint is a *primary's* alone, and a forwarder is the case that
  // proves it: gating it on "does not pull from a master" would read as right
  // and put "SOA defaults are filled in." under a zone type that answers from
  // no records for an SOA to head. (A secondary and a stub are covered by the
  // loop above; without this line and the internal one below, the gate could
  // be widened back with the suite green.)
  expect(screen.queryByText("SOA defaults are filled in.")).not.toBeInTheDocument();
  forwarder.unmount();

  const internal = renderWithProviders(<TypeAwareFieldsHarness type="internal" />);
  expect(screen.queryByLabelText(/^primary servers$/i)).not.toBeInTheDocument();
  expect(screen.queryByLabelText(/^forward to$/i)).not.toBeInTheDocument();
  expect(screen.queryByLabelText(/tsig key/i)).not.toBeInTheDocument();
  expect(screen.queryByText("SOA defaults are filled in.")).not.toBeInTheDocument();
  internal.unmount();

  renderWithProviders(<TypeAwareFieldsHarness type="primary" />);
  expect(screen.queryByLabelText(/^primary servers$/i)).not.toBeInTheDocument();
  expect(screen.queryByLabelText(/^forward to$/i)).not.toBeInTheDocument();
  expect(screen.queryByLabelText(/tsig key/i)).not.toBeInTheDocument();
  expect(screen.getByText("SOA defaults are filled in.")).toBeInTheDocument();
});

// A stub is configured exactly as a secondary is — same two fields, same
// server-side check — so the only thing that distinguishes the two on the
// wire is `type`.
test("creating a stub posts primaries and the TSIG key's id, like a secondary", async () => {
  const user = userEvent.setup();
  let body: unknown;
  mockZones([]);
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
    http.post("*/api/v1/zones", async ({ request }) => {
      body = await request.json();
      return HttpResponse.json({ id: 1 }, { status: 201 });
    }),
  );
  renderWithProviders(<ZonesList />);
  await openCreateRow(user);

  await user.selectOptions(screen.getByLabelText("Zone type"), "stub");
  await user.type(screen.getByLabelText(/zone name/i), "ad.corp.example");
  await user.type(screen.getByLabelText(/primary servers/i), "10.0.0.9");
  await user.selectOptions(await screen.findByLabelText(/tsig key/i), "4");
  await user.click(screen.getByRole("button", { name: "Add" }));

  await waitFor(() =>
    expect(body).toEqual({
      name: "ad.corp.example",
      type: "stub",
      primaries: "10.0.0.9",
      tsig_key_id: 4,
    }),
  );
});

// The same rule the secondary already has, and for the same reason: a stub
// with nowhere to fetch from claims its suffix and SERVFAILs it forever.
// checkZoneTransferConfig runs ValidatePrimaries for both types identically.
test("a stub with no primaries is rejected client-side and never posted", async () => {
  const user = userEvent.setup();
  let posted = false;
  mockZones([]);
  server.use(
    http.post("*/api/v1/zones", () => {
      posted = true;
      return HttpResponse.json({ id: 1 }, { status: 201 });
    }),
  );
  renderWithProviders(<ZonesList />);
  await openCreateRow(user);

  await user.selectOptions(screen.getByLabelText("Zone type"), "stub");
  await user.type(screen.getByLabelText(/zone name/i), "ad.corp.example");
  await user.click(screen.getByRole("button", { name: "Add" }));

  expect(await screen.findByText(/where to pull from/i)).toBeInTheDocument();
  await new Promise((resolve) => setTimeout(resolve, 30));
  expect(posted).toBe(false);
});

// `forward_to`, not `primaries` — the server 400s "primaries applies to
// secondary and stub zones only" for a forwarder, so posting the field the
// artboard's shared `needsPrimaries` implies would fail every time.
test("creating a forwarder posts forward_to and never primaries", async () => {
  const user = userEvent.setup();
  let body: unknown;
  mockZones([]);
  server.use(
    http.post("*/api/v1/zones", async ({ request }) => {
      body = await request.json();
      return HttpResponse.json({ id: 1 }, { status: 201 });
    }),
  );
  renderWithProviders(<ZonesList />);
  await openCreateRow(user);

  await user.selectOptions(screen.getByLabelText("Zone type"), "forwarder");
  await user.type(screen.getByLabelText(/zone name/i), "corp.example");
  await user.type(screen.getByLabelText(/^forward to$/i), "10.0.0.1, 10.0.0.2:5353");
  await user.click(screen.getByRole("button", { name: "Add" }));

  await waitFor(() =>
    expect(body).toEqual({
      name: "corp.example",
      type: "forwarder",
      forward_to: "10.0.0.1, 10.0.0.2:5353",
    }),
  );
});

// "" is a configuration and not a gap: the zone claims the suffix and
// SERVFAILs everything beneath it rather than falling through. The server
// accepts it, so this row must not invent a rule the server does not have —
// and the field is omitted rather than sent empty, the same way `primaries`
// and `tsig_key_id` already are.
test("a forwarder with no upstreams is accepted and posts no forward_to at all", async () => {
  const user = userEvent.setup();
  let body: unknown;
  mockZones([]);
  server.use(
    http.post("*/api/v1/zones", async ({ request }) => {
      body = await request.json();
      return HttpResponse.json({ id: 1 }, { status: 201 });
    }),
  );
  renderWithProviders(<ZonesList />);
  await openCreateRow(user);

  await user.selectOptions(screen.getByLabelText("Zone type"), "forwarder");
  await user.type(screen.getByLabelText(/zone name/i), "corp.example");
  await user.click(screen.getByRole("button", { name: "Add" }));

  await waitFor(() => expect(body).toEqual({ name: "corp.example", type: "forwarder" }));
});

test("the create row shows an em dash for serial, records and modified", async () => {
  const user = userEvent.setup();
  mockZones([]);
  renderWithProviders(<ZonesList />);
  await openCreateRow(user);

  const addRow = document.querySelector<HTMLElement>('[data-slot="add-zone-row"]')!;
  expect(within(addRow).getAllByText("—")).toHaveLength(3);
});

// The state a plain "disabled" switch can't express: still `enabled`, but
// nothing behind it can answer for it — the artboard makes this its own
// destructive status rather than a shade of disabled.
test("an expired secondary shows the EXPIRED sub-line and a Retry transfer item", async () => {
  const user = userEvent.setup();
  const seventeenDaysAgo = Date.now() - 17 * 86_400_000;
  mockZones([
    zone({
      id: 6,
      name: "branch.example.com",
      type: "secondary",
      enabled: true,
      primaries: "203.0.113.9",
      refreshed_at: seventeenDaysAgo,
      last_attempt: Date.now() - 2 * 3600_000,
      expires_at: Date.now() - 1000,
    }),
  ]);
  renderWithProviders(<ZonesList />);
  const rows = await screen.findAllByTestId("zone-row");

  expect(within(rows[0]).getByText("Not answering")).toBeInTheDocument();
  // When the last attempt was, in the row; what it means, on hover.
  expect(within(rows[0]).getByTestId("zone-pull-attempt").textContent).toBe("2h ago");
  const tip = await statusTip(user, rows[0]);
  expect(tip).toHaveTextContent("Expired");
  // Computed, never the artboard's literal "17d": the age of the copy this
  // zone is no longer allowed to answer from.
  expect(tip).toHaveTextContent(
    "Tried 2h ago. No transfer for 17 days, past the SOA expiry. Answering nothing.",
  );
  // The way to try again, now in the row's own actions menu rather than in a
  // band beneath the row.
  const menu = await rowMenu(rows[0], "branch.example.com");
  const retry = within(menu).getByRole("menuitem", { name: "Retry transfer" });
  expect(retry).toBeInTheDocument();
  // Deliberately nothing that submits, and portalled clear of every form on
  // the page: a submit control here would be one keystroke from posting the
  // create row.
  expect(retry).not.toHaveAttribute("type", "submit");
  expect(retry.closest("form")).toBeNull();
});

test("a healthy secondary shows no sub-line at all, and reads when it last refreshed", async () => {
  mockZones([
    zone({
      id: 8,
      name: "ok-secondary.example.com",
      type: "secondary",
      primaries: "203.0.113.9",
      soa_refresh: 7200,
      refreshed_at: Date.now() - 2 * 3600_000,
      expires_at: Date.now() + 999_999_999,
    }),
  ]);
  renderWithProviders(<ZonesList />);
  const rows = await screen.findAllByTestId("zone-row");

  // Not "Enabled": for a copy, when it was last confirmed current is the
  // thing worth knowing, and a plain "Enabled" hides it.
  expect(within(rows[0]).getByText("Refreshed 2h ago")).toBeInTheDocument();
  expect(within(rows[0]).queryByTestId("zone-pull-warning")).not.toBeInTheDocument();
  expect(screen.queryByText("Expired")).not.toBeInTheDocument();
  // The retry item IS offered, even though nothing is wrong. "Go and get the
  // current version now" is a reasonable thing to ask of a healthy copy, the
  // API allows it on any secondary or stub, and the detail page's "Refresh
  // now" has always offered it — so withholding it here would mean the same
  // zone offering the action on one screen and refusing it on the other.
  //
  // An earlier draft tied this item to the warning, because that was the
  // condition the band it replaced appeared under. That was an artifact of
  // the band, and this assertion is what changed when it went.
  const menu = await rowMenu(rows[0], "ok-secondary.example.com");
  expect(within(menu).getByRole("menuitem", { name: "Disable" })).toBeInTheDocument();
  expect(within(menu).getByRole("menuitem", { name: "Delete zone" })).toBeInTheDocument();
  expect(within(menu).getByRole("menuitem", { name: "Retry transfer" })).toBeInTheDocument();

  // The pulled marker survives being healthy. Elsewhere this is pinned only
  // against being drawn too widely (a forwarder must not get one); without
  // this, narrowing it to "only when something is wrong" passes the whole
  // suite, and a healthy secondary would silently stop being marked as
  // holding someone else's records.
  expect(within(rows[0]).getByLabelText("Pulled from another server")).toBeInTheDocument();
});

// The state a fresh secondary is in for its first minute, and the one an
// "Enabled" badge is most wrong about: the zone is enabled in the database
// and answering SERVFAIL for its whole suffix, because it holds nothing it
// may speak for (Zone.Serving, internal/zones/answer.go).
test("a secondary that has never transferred says so, rather than reading as enabled", async () => {
  const user = userEvent.setup();
  mockZones([
    zone({
      id: 9,
      name: "new-secondary.example.com",
      type: "secondary",
      enabled: true,
      primaries: "203.0.113.9",
    }),
  ]);
  renderWithProviders(<ZonesList />);
  const rows = await screen.findAllByTestId("zone-row");

  // "Not answering" in the status column — the same words an expired zone
  // gets, because it is the same fact — and the tooltip is what says which of
  // the two this is.
  expect(within(rows[0]).getByText("Not answering")).toBeInTheDocument();
  expect(within(rows[0]).queryByText("Enabled")).not.toBeInTheDocument();
  // Nothing has been tried, and the row says that rather than dating a
  // failure that never happened.
  expect(within(rows[0]).getByTestId("zone-pull-attempt").textContent).toBe("never attempted");
  const tip = await statusTip(user, rows[0]);
  expect(tip).toHaveTextContent("Never transferred");
  expect(tip).toHaveTextContent("Answering nothing under new-secondary.example.com.");
});

// The whole point of persisting last_error: this zone is failing for a
// reason recorded in the database, so it reads the same after a restart as
// before one. The row leads with the failure itself and keeps the server's
// whole sentence on the element, because rewriting a resolver error into
// "couldn't transfer" throws away everything actionable about it.
test("a failing secondary leads with the failure and keeps the whole message", async () => {
  const user = userEvent.setup();
  mockZones([
    zone({
      id: 10,
      name: "failing.example.com",
      type: "secondary",
      primaries: "203.0.113.9",
      soa_refresh: 7200,
      refreshed_at: Date.now() - 31 * 3600_000,
      expires_at: Date.now() + 999_999_999,
      last_error: "203.0.113.9:53: dial tcp: connect: connection refused",
      last_attempt: Date.now() - 60_000,
    }),
  ]);
  renderWithProviders(<ZonesList />);
  const rows = await screen.findAllByTestId("zone-row");

  // Not "Serving, transfer failing": the sentence on hover says what is being
  // served and how old it is, which the status column cannot.
  expect(within(rows[0]).getByText("Transfer failing")).toBeInTheDocument();
  expect(within(rows[0]).getByTestId("zone-pull-attempt").textContent).toBe("1m ago");
  const cause = within(rows[0]).getByTestId("zone-pull-error");
  // Just the failure — every word this page wrote itself is somewhere else,
  // not concatenated onto the end of the server's.
  expect(cause.textContent).toBe("connection refused");
  // The whole message moved into the Tooltip when the native title went: two
  // hover surfaces inside one cell fired at different moments in different
  // styles saying different things.
  expect(cause).not.toHaveAttribute("title");
  await user.hover(within(rows[0]).getByTestId("zone-status"));
  expect(
    within(await screen.findByTestId("zone-status-tip")).getByTestId("zone-status-tip-cause"),
  ).toHaveTextContent("203.0.113.9:53: dial tcp: connect: connection refused");
  const tip = await statusTip(user, rows[0]);
  expect(tip).toHaveTextContent("Last transfer");
  expect(tip).toHaveTextContent("Tried 1m ago. Still serving the copy from 1d ago.");
});

// The defect this row had: the transfer's error and this page's own sentence
// about the consequence were one concatenated string, so a long error ran
// into the note with nothing between them and pushed it out of the row.
// Whatever the server said, the note is a separate element and stays whole.
test("a long transfer error does not run into the note or crowd it out", async () => {
  const user = userEvent.setup();
  const longError = `zone "failing.example.com": every primary failed: ${"203.0.113.9:53: dial tcp 203.0.113.9:53: no route to host: ".repeat(8)}connection refused`;
  mockZones([
    zone({
      id: 10,
      name: "failing.example.com",
      type: "secondary",
      primaries: "203.0.113.9",
      soa_refresh: 7200,
      refreshed_at: Date.now() - 31 * 3600_000,
      expires_at: Date.now() + 999_999_999,
      last_error: longError,
      last_attempt: Date.now() - 60_000,
    }),
  ]);
  renderWithProviders(<ZonesList />);
  const rows = await screen.findAllByTestId("zone-row");

  // The two things that stay whole whatever the server said: the date of the
  // attempt, which never shrinks inside the cell, and the way to try again,
  // which is out of the cell entirely and so cannot be crowded at all.
  expect(within(rows[0]).getByTestId("zone-pull-attempt").textContent).toBe("1m ago");

  // And what is drawn of the error is short whatever its length — the part
  // that says what went wrong, not the leading context an ellipsis would
  // have left behind.
  const cause = within(rows[0]).getByTestId("zone-pull-error");
  expect(cause.textContent).toBe("connection refused");
  // Nothing is lost by drawing less of it.
  expect(cause).not.toHaveAttribute("title");
  await user.hover(within(rows[0]).getByTestId("zone-status"));
  expect(
    within(await screen.findByTestId("zone-status-tip")).getByTestId("zone-status-tip-cause"),
  ).toHaveTextContent(longError);
  expect(cause).toHaveClass("truncate");

  const menu = await rowMenu(rows[0], "failing.example.com");
  expect(within(menu).getByRole("menuitem", { name: "Retry transfer" })).toBeInTheDocument();
});

// An error left over from before the last success is not this zone's current
// state, and a success whose bookkeeping write failed is the one way that can
// happen (Refresher.recordAttempt is best-effort). Ordering the two stamps is
// what stops a fixed problem sitting on screen forever.
test("an error older than the last success is not shown", async () => {
  mockZones([
    zone({
      id: 11,
      name: "recovered.example.com",
      type: "secondary",
      primaries: "203.0.113.9",
      soa_refresh: 7200,
      refreshed_at: Date.now() - 60_000,
      expires_at: Date.now() + 999_999_999,
      last_error: "connection refused",
      last_attempt: Date.now() - 9 * 3600_000,
    }),
  ]);
  renderWithProviders(<ZonesList />);
  const rows = await screen.findAllByTestId("zone-row");

  expect(within(rows[0]).queryByText(/connection refused/)).not.toBeInTheDocument();
  expect(within(rows[0]).getByText(/^Refreshed /)).toBeInTheDocument();
});

// The action was inert through Milestone A — there was no transfer endpoint
// to call. It has one now, and it lives in the row's menu.
// One hover surface, not two. The untruncated error used to live in a native
// `title` on the sub-line's cause — inside the very cell the Tooltip wraps —
// so hovering fired both, in two styles, saying two different things. The
// Tooltip is the only thing that speaks on hover now, and it carries the
// message in full while the row keeps the truncated lead.
test("the status tooltip carries the whole error, and no native title competes with it", async () => {
  const user = userEvent.setup();
  const full =
    'zone "corp2.lan": every primary failed: 127.0.0.1:5399: dial tcp 127.0.0.1:5399: connect: connection refused';
  mockZones([
    zone({
      id: 41,
      name: "corp2.lan",
      type: "secondary",
      enabled: true,
      refreshed_at: Date.now() - 7 * 3_600_000,
      expires_at: Date.now() + 86_400_000,
      last_error: full,
      last_attempt: Date.now() - 30_000,
    }),
  ]);
  renderWithProviders(<ZonesList />);
  const rows = await screen.findAllByTestId("zone-row");

  // The row truncates, and says so by not carrying the whole message.
  const inline = within(rows[0]).getByTestId("zone-pull-error");
  expect(inline).not.toHaveAttribute("title");
  expect(inline.textContent).not.toBe(full);

  await user.hover(within(rows[0]).getByTestId("zone-status"));
  const tip = await screen.findByTestId("zone-status-tip");
  expect(within(tip).getByTestId("zone-status-tip-cause")).toHaveTextContent(full);
  // Title-case in the DOM, uppercased by CSS — assert what is actually there,
  // not what the screen shows, or the test passes on a string that never
  // existed.
  expect(within(tip).getByText("Last transfer")).toBeInTheDocument();
});

// The tooltip surface does not follow the theme, and that is the one place in
// this app where that is true. rnui styles the popup `bg-foreground`, which
// flips: near-black on a light page, near-white on a dark one — a pale slab
// over a dark table. bg-tooltip is a token that stays dark in both modes.
//
// Asserting the absence of bg-foreground is the half that matters: the class
// arrives through the component's own cn() merge, so a token rename or a
// changed merge order would leave both classes on the node and the popup
// would go back to flipping with whichever wins.
test("the status tooltip keeps its own surface rather than the theme's", async () => {
  const user = userEvent.setup();
  mockZones([
    zone({
      id: 44,
      name: "surface.example.com",
      type: "stub",
      enabled: true,
      refreshed_at: 0,
      last_error: "i/o timeout",
      last_attempt: Date.now() - 60_000,
    }),
  ]);
  renderWithProviders(<ZonesList />);
  const rows = await screen.findAllByTestId("zone-row");

  const tip = await statusTip(user, rows[0]);
  expect(tip).toHaveClass("bg-tooltip");
  expect(tip).toHaveClass("text-tooltip-foreground");
  expect(tip).not.toHaveClass("bg-foreground");
});

test("Retry transfer asks the server for a transfer now", async () => {
  let refreshed = 0;
  mockZones([
    zone({
      id: 12,
      name: "branch.example.com",
      type: "secondary",
      primaries: "203.0.113.9",
      refreshed_at: Date.now() - 17 * 86_400_000,
      expires_at: Date.now() - 1000,
    }),
  ]);
  server.use(
    http.post("/api/v1/zones/12/refresh", () => {
      refreshed++;
      return HttpResponse.json({
        primary: "203.0.113.9:53",
        serial: 2026080601,
        records: 17,
        refreshed_at: Date.now(),
        expires_at: Date.now() + 999_999,
      });
    }),
  );
  renderWithProviders(<ZonesList />);
  await screen.findAllByTestId("zone-row");

  const menu = await rowMenu(zoneRows()[0], "branch.example.com");
  fireEvent.click(within(menu).getByRole("menuitem", { name: "Retry transfer" }));
  await waitFor(() => expect(refreshed).toBe(1));
});

test("a long .arpa apex does not break the zones-list grid", async () => {
  const user = userEvent.setup();
  mockZones([
    zone({
      id: 1,
      name: "1.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.ip6.arpa",
      type: "internal",
    }),
  ]);
  renderWithProviders(<ZonesList />);
  await user.click(await screen.findByRole("button", { name: /1 built-in zone/i }));
  const cell = await screen.findByTitle(/ip6\.arpa$/);
  expect(cell).toHaveClass("truncate");
});

// The fourth status a secondary can carry, and the one with no analogue on the
// design boards: past its refresh deadline with no failure recorded against
// it. Without this state the row would read "Refreshed 5h ago" in the calm
// green of a zone that is up to date.
test("a secondary past its refresh deadline with nothing recorded reads overdue", async () => {
  const user = userEvent.setup();
  mockZones([
    zone({
      id: 13,
      name: "stale.example.com",
      type: "secondary",
      primaries: "203.0.113.9",
      soa_refresh: 7200,
      refreshed_at: Date.now() - 5 * 3600_000,
      last_attempt: Date.now() - 5 * 3600_000,
      expires_at: Date.now() + 999_999_999,
    }),
  ]);
  renderWithProviders(<ZonesList />);
  const rows = await screen.findAllByTestId("zone-row");

  expect(within(rows[0]).getByText("Serving, transfer overdue")).toBeInTheDocument();
  const tip = await statusTip(user, rows[0]);
  expect(tip).toHaveTextContent("Overdue");
  expect(tip).toHaveTextContent(/nothing has transferred from 203\.0\.113\.9 since 5h ago/i);
});

// A disabled zone is skipped by the scheduler entirely, so its transfer state
// is a consequence of the switch rather than a problem of its own. The list
// already checked `enabled` first; this pins that it keeps doing so.
test("a disabled secondary reads Disabled, with no transfer warning of its own", async () => {
  mockZones([
    zone({
      id: 14,
      name: "off.example.com",
      type: "secondary",
      enabled: false,
      primaries: "203.0.113.9",
      refreshed_at: Date.now() - 5 * 86_400_000,
      expires_at: Date.now() - 1000,
      last_error: "connection refused",
      last_attempt: Date.now() - 4 * 86_400_000,
    }),
  ]);
  renderWithProviders(<ZonesList />);
  const rows = await screen.findAllByTestId("zone-row");

  expect(within(rows[0]).getByText("Disabled")).toBeInTheDocument();
  expect(within(rows[0]).queryByTestId("zone-pull-warning")).not.toBeInTheDocument();
  // Enable is offered rather than Disable — and so is the retry, even though
  // the scheduler is not touching this zone. The tempting rule is "nothing is
  // trying, so do not offer to try", but the detail page's "Refresh now" is
  // gated on type alone and would offer it, and the API accepts it on any
  // secondary or stub. Refusing it here would only move the disagreement
  // between the two screens rather than settle it, and pulling a copy before
  // switching a zone on is a reasonable thing to want.
  const menu = await rowMenu(rows[0], "off.example.com");
  expect(within(menu).getByRole("menuitem", { name: "Enable" })).toBeInTheDocument();
  expect(within(menu).getByRole("menuitem", { name: "Retry transfer" })).toBeInTheDocument();
});

// ── a stub claims a suffix too ──────────────────────────────────────────────
//
// A stub is the other type whose contents arrive from somewhere else, and the
// one this list used to call plain "Enabled" in green whatever state it was
// in. It has no records of its own: it holds an NS set fetched from a master
// and routes the suffix it claims to those nameservers. With no set, the
// claim stands and every name beneath it answers SERVFAIL — a suffix-wide
// outage the row was drawing as health.

test("a stub that has never fetched reads Not answering, never Enabled", async () => {
  mockZones([
    zone({
      id: 20,
      name: "ad.corp.example.net",
      type: "stub",
      enabled: true,
      primaries: "10.0.0.9",
    }),
  ]);
  renderWithProviders(<ZonesList />);
  const rows = await screen.findAllByTestId("zone-row");

  expect(within(rows[0]).getByText("Not answering")).toBeInTheDocument();
  expect(within(rows[0]).queryByText("Enabled")).not.toBeInTheDocument();
  expect(within(rows[0]).getByTestId("zone-pull-attempt").textContent).toBe("never attempted");
});

// Five strings in this codebase have already had to be corrected for saying
// "transfer" about a type that fetches. A stub asks two ordinary questions of
// its master; it does not transfer and it never expires, so no noun on its
// row may say otherwise — including the one on the button.
test("a never-fetched stub's row and tooltip say fetch, and never transfer", async () => {
  const user = userEvent.setup();
  mockZones([
    zone({
      id: 21,
      name: "dr.corp.example.net",
      type: "stub",
      enabled: true,
      primaries: "10.0.0.9",
      last_error: "i/o timeout",
      last_attempt: Date.now() - 12 * 60_000,
    }),
  ]);
  renderWithProviders(<ZonesList />);
  const rows = await screen.findAllByTestId("zone-row");

  expect(within(rows[0]).getByTestId("zone-pull-attempt").textContent).toBe("12m ago");
  expect(within(rows[0]).getByTestId("zone-pull-error").textContent).toBe("i/o timeout");
  const tip = await statusTip(user, rows[0]);
  expect(tip).toHaveTextContent("Never fetched");
  expect(tip).toHaveTextContent("Tried 12m ago. Answering nothing under dr.corp.example.net.");

  expect(rows[0].textContent).not.toMatch(/transfer/i);
  expect(tip.textContent).not.toMatch(/transfer/i);

  // The menu is portalled out of the row, so the sweep above cannot see it
  // and it gets one of its own — this is the fifth string on this screen
  // that has to follow the type, and moving it here must not lose that.
  const menu = await rowMenu(rows[0], "dr.corp.example.net");
  expect(within(menu).getByRole("menuitem", { name: "Fetch now" })).toBeInTheDocument();
  expect(menu.textContent).not.toMatch(/transfer/i);
});

/**
 * The subtlest bug this row can have, and the one nothing else catches.
 *
 * `handleZonePatch` sets the type on a row read from the store and clears no
 * stamp, so a zone retyped secondary → stub keeps the `expires_at` its last
 * transfer wrote. A stub is never given one (§9.11.8), so that number means
 * nothing — but read it and a stub that is fetching perfectly well reads as
 * dead, in destructive red, on a row that claims its whole suffix.
 */
test("a stub carrying a dead expires_at from a retype is not expired", async () => {
  mockZones([
    zone({
      id: 22,
      name: "vpn.example.org",
      type: "stub",
      enabled: true,
      primaries: "10.0.0.9",
      soa_refresh: 7200,
      refreshed_at: Date.now() - 4 * 3600_000,
      last_attempt: Date.now() - 4 * 3600_000,
      // The stamp the transfer left behind, a month past.
      expires_at: Date.now() - 30 * 86_400_000,
    }),
  ]);
  renderWithProviders(<ZonesList />);
  const rows = await screen.findAllByTestId("zone-row");

  // Fetched, not Refreshed, and healthy — no warning row, no tooltip, no
  // destructive treatment of any kind.
  expect(within(rows[0]).getByText("Fetched 4h ago")).toBeInTheDocument();
  expect(within(rows[0]).queryByText("Not answering")).not.toBeInTheDocument();
  expect(within(rows[0]).queryByTestId("zone-pull-warning")).not.toBeInTheDocument();
  expect(within(rows[0]).queryByTestId("zone-status-tip")).not.toBeInTheDocument();
});

// A failed fetch does not take the NS set away: the suffix is still routed to
// the nameservers it already knows, which is the difference between this and
// the state above and the whole reason it is amber rather than red.
test("a stub whose last fetch failed is still routing on the set it has", async () => {
  const user = userEvent.setup();
  mockZones([
    zone({
      id: 23,
      name: "sql.corp.example.net",
      type: "stub",
      enabled: true,
      primaries: "10.0.0.9",
      soa_refresh: 7200,
      refreshed_at: Date.now() - 5 * 86_400_000,
      last_error: "10.0.0.9:53: dial tcp: connect: connection refused",
      last_attempt: Date.now() - 40 * 60_000,
    }),
  ]);
  renderWithProviders(<ZonesList />);
  const rows = await screen.findAllByTestId("zone-row");

  expect(within(rows[0]).getByText("Fetch failing")).toBeInTheDocument();
  expect(within(rows[0]).getByTestId("zone-pull-error").textContent).toBe("connection refused");
  const tip = await statusTip(user, rows[0]);
  expect(tip).toHaveTextContent("Last fetch");
  expect(tip).toHaveTextContent("Tried 40m ago. Still routing on the NS set from 5d ago.");
  expect(rows[0].textContent).not.toMatch(/transfer/i);
});

// The same endpoint a secondary's retry uses — POST /zones/{id}/refresh takes
// either type (400 "only secondary and stub zones pull from a master" is the
// server's own gate) — under the name that describes what it does here.
test("Fetch now asks the server to fetch now", async () => {
  let fetched = 0;
  mockZones([zone({ id: 24, name: "ad.corp.example.net", type: "stub", primaries: "10.0.0.9" })]);
  server.use(
    http.post("/api/v1/zones/24/refresh", () => {
      fetched++;
      return HttpResponse.json({
        primary: "10.0.0.9:53",
        serial: 2026080601,
        records: 2,
        refreshed_at: Date.now(),
        // A stub is given no expiry, and the server sends none.
        expires_at: 0,
      });
    }),
  );
  renderWithProviders(<ZonesList />);
  await screen.findAllByTestId("zone-row");

  const success = vi.spyOn(toast, "success");
  const menu = await rowMenu(zoneRows()[0], "ad.corp.example.net");
  fireEvent.click(within(menu).getByRole("menuitem", { name: "Fetch now" }));
  await waitFor(() => expect(fetched).toBe(1));
  // Fetched, not transferred — the toast is the fifth string on this screen
  // that has to follow the type.
  await waitFor(() => expect(success).toHaveBeenCalledWith("ad.corp.example.net fetched"));
});

// The marker beside the name says the records under this zone were written by
// someone else — true of a stub exactly as it is of a secondary, and the one
// signal that stays visible when the type column is scanned past.
test("a stub is marked as pulled at its name, the same as a secondary", async () => {
  const user = userEvent.setup();
  mockZones([
    zone({ id: 25, name: "vpn.example.org", type: "stub", primaries: "10.0.0.9" }),
    zone({ id: 26, name: "home.lan", type: "primary" }),
  ]);
  renderWithProviders(<ZonesList />);
  const rows = await screen.findAllByTestId("zone-row");

  const marker = within(rows[0]).getByLabelText("Pulled from another server");
  expect(within(rows[1]).queryByLabelText("Pulled from another server")).not.toBeInTheDocument();

  // And it says so on hover as well as to a screen reader — the artboard's
  // own title on this icon, as a real tooltip.
  await user.hover(marker);
  expect(await screen.findByText("Pulled from another server")).toBeInTheDocument();
});

// ── a forwarder has no health to show ───────────────────────────────────────

/**
 * A forwarder pulls nothing. It claims a suffix and sends the queries beneath
 * it upstream, live — so whether those upstreams are answering is a fact
 * about this instant that no column records. The stamps in this fixture are
 * exactly the ones that would make a stub dead; on a forwarder they are
 * leftovers, and a row that read them would be inventing a health state out
 * of a retype.
 */
test("a forwarder gets no pull treatment, whatever stamps its row carries", async () => {
  mockZones([
    zone({
      id: 27,
      name: "corp.example",
      type: "forwarder",
      enabled: true,
      forward_to: "10.0.0.1:53",
      refreshed_at: 0,
      last_error: "i/o timeout",
      last_attempt: Date.now() - 60_000,
      expires_at: Date.now() - 1000,
    }),
  ]);
  renderWithProviders(<ZonesList />);
  const rows = await screen.findAllByTestId("zone-row");

  expect(within(rows[0]).getByText("Enabled")).toBeInTheDocument();
  expect(within(rows[0]).queryByText("Not answering")).not.toBeInTheDocument();
  expect(within(rows[0]).queryByTestId("zone-pull-warning")).not.toBeInTheDocument();
  expect(within(rows[0]).queryByLabelText("Pulled from another server")).not.toBeInTheDocument();
  // And no pull item in its menu — a forwarder has no master to ask, so the
  // endpoint behind that item answers it 400. The two items every zone gets
  // are there, so this is the item missing and not the menu.
  const menu = await rowMenu(rows[0], "corp.example");
  expect(within(menu).getByRole("menuitem", { name: "Disable" })).toBeInTheDocument();
  expect(within(menu).getByRole("menuitem", { name: "Delete zone" })).toBeInTheDocument();
  expect(
    within(menu).queryByRole("menuitem", { name: /fetch now|retry transfer/i }),
  ).not.toBeInTheDocument();
});

// Which leaves one thing to say, and the status cell is where it belongs:
// green here means enabled, and nothing more than that.
test("a forwarder's status says outright that upstream health is not tracked", async () => {
  const user = userEvent.setup();
  mockZones([zone({ id: 28, name: "corp.example", type: "forwarder", forward_to: "10.0.0.1:53" })]);
  renderWithProviders(<ZonesList />);
  const rows = await screen.findAllByTestId("zone-row");

  const tip = await statusTip(user, rows[0]);
  // U+2019, not an ASCII apostrophe: the escape is deliberate, so that an
  // editor "correcting" the production string to ' fails this rather than
  // matching it.
  // Exact, not a substring: toHaveTextContent matches loosely, so the
  // trailing period the artboard added would slip in or out unnoticed.
  // U+2019, not an ASCII apostrophe — the escape is deliberate, so that an
  // editor "correcting" the production string to ' fails this rather than
  // matching it.
  expect(tip.textContent).toBe("Upstream health isn\u2019t tracked.");
});

// ── where the sub-line sits ────────────────────────────────────────────────

/**
 * The sub-line belongs to the STATUS column, not to the row.
 *
 * It shipped as a full-width band beneath the whole row, which put the
 * failure under the zone's *name* and made a warned row two bands tall — so
 * SERIAL, RECORDS and MODIFIED on that row no longer sat on the same line as
 * their neighbours' and the eye lost the column it was scanning. The artboard
 * draws it inside the status cell, directly under the status word, which is
 * the thing it is about.
 *
 * Asserted as containment rather than by class names: any re-styling is free,
 * a move back out to the row is not.
 */
test("the warning sub-line is inside the status cell, not a band under the row", async () => {
  mockZones([
    zone({
      id: 45,
      name: "failing.example.com",
      type: "secondary",
      primaries: "203.0.113.9",
      soa_refresh: 7200,
      refreshed_at: Date.now() - 31 * 3600_000,
      expires_at: Date.now() + 999_999_999,
      last_error: "203.0.113.9:53: dial tcp: connect: connection refused",
      last_attempt: Date.now() - 60_000,
    }),
  ]);
  renderWithProviders(<ZonesList />);
  const rows = await screen.findAllByTestId("zone-row");

  const status = within(rows[0]).getByTestId("zone-status");
  expect(status).toContainElement(within(rows[0]).getByTestId("zone-pull-warning"));
  expect(status).toContainElement(within(rows[0]).getByTestId("zone-pull-attempt"));
  expect(status).toContainElement(within(rows[0]).getByTestId("zone-pull-error"));
  // Which is also what keeps the row one grid: the status word and the
  // sub-line share a cell, so nothing sits outside the seven columns.
  expect(within(rows[0]).getByTestId("zone-pull-warning").closest("[data-slot='zone-row']")).toBe(
    rows[0],
  );
  expect(status).toHaveTextContent("Transfer failing");
});

// ── the row's actions live in one menu ─────────────────────────────────────
//
// The row used to carry a pencil and a trash can, and its retry sat in a band
// across the foot of the row. All three are one kebab now — the artboard's own
// shape, and what lets three actions of very different weight share a 92px
// cell. Rename is drawn on the artboard and deliberately not built: no rename
// exists in this app or its API, and the pencil it would have replaced was a
// link to the detail page, never a rename.

test("the row's actions are one kebab menu, and the pencil and trash are gone", async () => {
  mockZones([zone({ id: 40, name: "home.lan", type: "primary" })]);
  renderWithProviders(<ZonesList />);
  const rows = await screen.findAllByTestId("zone-row");

  // One control in the cell, named for its own zone rather than "Actions" —
  // in a list of forty rows the bare word is forty identical buttons.
  expect(within(rows[0]).queryByRole("button", { name: /^edit /i })).not.toBeInTheDocument();
  expect(within(rows[0]).queryByRole("button", { name: /^delete /i })).not.toBeInTheDocument();

  const menu = await rowMenu(rows[0], "home.lan");
  expect(within(menu).getByRole("menuitem", { name: "Disable" })).toBeInTheDocument();
  expect(within(menu).getByRole("menuitem", { name: "Delete zone" })).toBeInTheDocument();
  // Not invented: the artboard draws it, nothing in the app implements it.
  expect(within(menu).queryByRole("menuitem", { name: /rename/i })).not.toBeInTheDocument();

  // And nothing is lost by dropping the pencil: the name is the same link it
  // pointed at, and it is still the only one on the row.
  expect(within(rows[0]).getAllByRole("link")).toHaveLength(1);
  expect(within(rows[0]).getByRole("link", { name: "home.lan" })).toHaveAttribute(
    "href",
    "/zones/40",
  );
});

// Two rows in one render, so the noun is proved to follow each row's own type
// rather than a single fixture's. Same endpoint behind both — POST
// /zones/{id}/refresh takes either type that pulls — and a different word for
// it, because a stub asks two ordinary questions and does not transfer.
test("the pull item says Fetch now on a stub and Retry transfer on a secondary", async () => {
  mockZones([
    zone({ id: 41, name: "ad.corp.example.net", type: "stub", primaries: "10.0.0.9" }),
    zone({ id: 42, name: "branch.example.com", type: "secondary", primaries: "203.0.113.9" }),
  ]);
  renderWithProviders(<ZonesList />);
  const rows = await screen.findAllByTestId("zone-row");

  const stubMenu = await rowMenu(rows[0], "ad.corp.example.net");
  expect(within(stubMenu).getByRole("menuitem", { name: "Fetch now" })).toBeInTheDocument();
  expect(within(stubMenu).queryByRole("menuitem", { name: /transfer/i })).not.toBeInTheDocument();
  fireEvent.keyDown(stubMenu, { key: "Escape" });

  const secondaryMenu = await rowMenu(rows[1], "branch.example.com");
  expect(
    within(secondaryMenu).getByRole("menuitem", { name: "Retry transfer" }),
  ).toBeInTheDocument();
  expect(within(secondaryMenu).queryByRole("menuitem", { name: /fetch/i })).not.toBeInTheDocument();
});

// Ported from the zone detail page, which is where this lived and nowhere
// else — so the list could show a zone was off but never switch it. Same
// PATCH, same two toasts, and the same absence of a confirmation: disabling
// is one click to undo, and a dialog in front of it would only train people
// to dismiss the one that guards the delete.
test("Disable PATCHes enabled:false and Enable PATCHes enabled:true", async () => {
  let body: unknown;
  server.use(
    http.patch("/api/v1/zones/43", async ({ request }) => {
      body = await request.json();
      return new HttpResponse(null, { status: 204 });
    }),
  );
  const success = vi.spyOn(toast, "success");

  mockZones([zone({ id: 43, name: "home.lan", enabled: true })]);
  const on = renderWithProviders(<ZonesList />);
  await waitFor(() => expect(zoneRows()).toHaveLength(1));

  const offMenu = await rowMenu(zoneRows()[0], "home.lan");
  // One item, not two: an enabled zone is never also offered Enable.
  expect(within(offMenu).queryByRole("menuitem", { name: "Enable" })).not.toBeInTheDocument();
  fireEvent.click(within(offMenu).getByRole("menuitem", { name: "Disable" }));
  await waitFor(() => expect(body).toEqual({ enabled: false }));
  await waitFor(() => expect(success).toHaveBeenCalledWith("Zone disabled"));
  on.unmount();

  // The same zone, off. The item flips its word, and the body it sends with
  // it — a label that flipped alone would disable an already-disabled zone.
  body = undefined;
  mockZones([zone({ id: 43, name: "home.lan", enabled: false })]);
  renderWithProviders(<ZonesList />);
  await waitFor(() => expect(zoneRows()).toHaveLength(1));

  const onMenu = await rowMenu(zoneRows()[0], "home.lan");
  expect(within(onMenu).queryByRole("menuitem", { name: "Disable" })).not.toBeInTheDocument();
  fireEvent.click(within(onMenu).getByRole("menuitem", { name: "Enable" }));
  await waitFor(() => expect(body).toEqual({ enabled: true }));
  await waitFor(() => expect(success).toHaveBeenCalledWith("Zone enabled"));
});

test("a failed disable names the zone and leaves the row enabled", async () => {
  mockZones([zone({ id: 44, name: "home.lan", enabled: true })]);
  server.use(
    http.patch("/api/v1/zones/44", () => HttpResponse.json({ error: "boom" }, { status: 500 })),
  );
  const errorSpy = vi.spyOn(toast, "error");

  renderWithProviders(<ZonesList />);
  await waitFor(() => expect(zoneRows()).toHaveLength(1));

  const menu = await rowMenu(zoneRows()[0], "home.lan");
  fireEvent.click(within(menu).getByRole("menuitem", { name: "Disable" }));

  await waitFor(() => expect(errorSpy).toHaveBeenCalledWith("Couldn't disable home.lan"));
  expect(within(zoneRows()[0]).getByText("Enabled")).toBeInTheDocument();
});

// ── the tooltip is supplementary, never the only copy ───────────────────────

// The row carries the alarm on its own — weight, colour, dot, edge mark, tint
// and the sub-line — and the tooltip carries the words that explain it. What
// must not happen is the label reading twice: it is the tooltip's, and a
// second copy inline is the artboard's own arrangement undone.
test("the warn label and its sentence are in the tooltip and nowhere in the row", async () => {
  const user = userEvent.setup();
  mockZones([
    zone({
      id: 29,
      name: "ad.corp.example.net",
      type: "stub",
      enabled: true,
      primaries: "10.0.0.9",
    }),
  ]);
  renderWithProviders(<ZonesList />);
  const rows = await screen.findAllByTestId("zone-row");

  expect(rows[0].textContent).not.toMatch(/never fetched/i);
  expect(rows[0].textContent).not.toMatch(/answering nothing under/i);

  const tip = await statusTip(user, rows[0]);
  expect(tip).toHaveTextContent("Never fetched");
  expect(tip).toHaveTextContent("Answering nothing under ad.corp.example.net.");
});

// A row with nothing to explain is not a trigger at all: forty rows of
// tooltips that say nothing is how a tooltip stops being read. Asserted on
// the cell rather than by hovering and waiting for nothing to happen — a
// popup that opens 300ms after the assertion would pass that.
test("a healthy primary's status is not a tooltip trigger", async () => {
  mockZones([zone({ id: 30, name: "home.lan", type: "primary" })]);
  renderWithProviders(<ZonesList />);
  const rows = await screen.findAllByTestId("zone-row");

  const cell = within(rows[0]).getByTestId("zone-status");
  expect(cell).toHaveTextContent("Enabled");
  expect(cell).not.toHaveAttribute("data-slot", "tooltip-trigger");
  expect(cell).not.toHaveClass("cursor-help");
});

// ── Keeping up with the scheduler ───────────────────────────────────────────
//
// A secondary's transfer state is the one thing on any of these screens that
// changes with nobody touching it: the scheduler transfers on the SOA's
// refresh, retries a zone whose primary has come back, and lets an
// unreachable one expire. Everything else here changes only when an operator
// changes it, and the mutation that did already invalidated — which is why
// the query client turns polling and focus revalidation off for the whole app
// (lib/query-client.ts) and this is the one place that opts back in.
//
// Fake timers throughout: the point is what happens over minutes, and a test
// that really waits them out is not a test anyone will keep.

/** A healthy secondary two hours into a seven-hour refresh. */
function watchedSecondary(overrides: Partial<Zone> = {}): Zone {
  return zone({
    id: 6,
    name: "branch.example.com",
    type: "secondary",
    primaries: "203.0.113.9",
    soa_refresh: 25_200,
    refreshed_at: Date.now() - 2 * 3600_000,
    expires_at: Date.now() + 999_999_999,
    ...overrides,
  });
}

test("a failing secondary recovers on screen, with nobody touching the page", async () => {
  vi.useFakeTimers({ shouldAdvanceTime: true });
  let failing = true;
  server.use(
    http.get("/api/v1/zones", () =>
      HttpResponse.json([
        watchedSecondary(
          failing
            ? {
                last_error: "203.0.113.9:53: dial tcp: connect: connection refused",
                last_attempt: Date.now(),
              }
            : {},
        ),
      ]),
    ),
  );

  renderWithProviders(<ZonesList />);
  expect(await screen.findByText(/connection refused/)).toBeInTheDocument();

  // The primary came back and the scheduler transferred. Nothing told this
  // page so, and nobody clicked anything.
  failing = false;
  await act(async () => {
    await vi.advanceTimersByTimeAsync(30_000);
  });

  await waitFor(() => expect(screen.queryByText(/connection refused/)).not.toBeInTheDocument());
  expect(screen.getByText("Refreshed 2h ago")).toBeInTheDocument();
});

// The constraint the poll is gated on: a homelab with primaries alone must
// pay nothing for a feature that can only ever report on a secondary.
test("a list of primaries alone is read once and never again", async () => {
  vi.useFakeTimers({ shouldAdvanceTime: true });
  const paths = trackFetchedPaths();
  mockZones([zone({ id: 1, name: "example.com", type: "primary" })]);
  mockZoneRecords(1, [zoneRecord(1, 1)]);

  renderWithProviders(<ZonesList />);
  await screen.findAllByTestId("zone-row");

  await act(async () => {
    await vi.advanceTimersByTimeAsync(5 * 60_000);
  });

  expect(reads(paths, "/api/v1/zones")).toBe(1);
  expect(reads(paths, "/api/v1/zones/1/records")).toBe(1);
});

// The other half of the same bargain: watching is not hammering. Bounds
// rather than an exact count, and measured off the wire rather than read back
// off the query's config, so the assertion survives any reshuffling of how
// the interval is expressed.
test("watching a secondary for five minutes is a handful of reads, and none of its records", async () => {
  vi.useFakeTimers({ shouldAdvanceTime: true });
  const paths = trackFetchedPaths();
  mockZones([watchedSecondary()]);
  mockZoneRecords(6, [zoneRecord(6, 1)]);

  renderWithProviders(<ZonesList />);
  await screen.findAllByTestId("zone-row");

  await act(async () => {
    await vi.advanceTimersByTimeAsync(5 * 60_000);
  });

  const zonesReads = reads(paths, "/api/v1/zones");
  expect(zonesReads).toBeGreaterThan(1);
  // 5 minutes at one read per 30s, plus the first. Anything above this is a
  // page hammering an endpoint whose answer cannot change that fast — the
  // scheduler only decides what is due every 30s.
  expect(zonesReads).toBeLessThanOrEqual(11);
  // And the record list is not on a timer of its own. Nothing transferred, so
  // there was nothing to re-read; polling it would have doubled the traffic
  // to learn what the zone above already reports.
  expect(reads(paths, "/api/v1/zones/6/records")).toBe(1);
});

// A transfer landing is the one thing that changes a secondary's records, and
// it is the zone's own `refreshed_at` moving that says so.
test("a transfer landing re-reads that zone's records, and only that zone's", async () => {
  vi.useFakeTimers({ shouldAdvanceTime: true });
  const paths = trackFetchedPaths();
  const before = Date.now() - 2 * 3600_000;
  let refreshedAt = before;
  server.use(
    http.get("/api/v1/zones", () =>
      HttpResponse.json([
        watchedSecondary({ refreshed_at: refreshedAt }),
        zone({ id: 1, name: "example.com", type: "primary" }),
      ]),
    ),
    http.get("/api/v1/zones/6/records", () =>
      HttpResponse.json(
        refreshedAt === before ? [zoneRecord(6, 1)] : [zoneRecord(6, 1), zoneRecord(6, 2)],
      ),
    ),
  );
  mockZoneRecords(1, [zoneRecord(1, 1)]);

  renderWithProviders(<ZonesList />);
  await waitFor(() => expect(zoneRows()).toHaveLength(2));
  // Waited for, not asserted outright: the rows and their record counts come
  // from two different queries, so a row being on screen does not mean its
  // count is. Asserting the precondition directly failed here roughly one run
  // in three under a full-suite load, on the setup rather than on the
  // behaviour under test.
  await waitFor(() => expect(within(zoneRows()[0]).getByText("1")).toBeInTheDocument());

  refreshedAt = Date.now();
  await act(async () => {
    await vi.advanceTimersByTimeAsync(30_000);
  });

  await waitFor(() => expect(within(zoneRows()[0]).getByText("2")).toBeInTheDocument());
  expect(reads(paths, "/api/v1/zones/6/records")).toBe(2);
  // The primary beside it transferred nothing, because it cannot.
  expect(reads(paths, "/api/v1/zones/1/records")).toBe(1);
});

// A stub's NS set is fetched on the SOA's own schedule by the same scheduler
// that transfers a secondary (internal/zones/refresh.go's pullsFromAMaster
// covers both), so a page showing one is out of date the moment it stops
// asking — exactly the condition the poll exists for. Without a stub in
// `watchesTransfers`, a stub sitting at "no NS set yet" stays there until
// someone reloads, and the first successful fetch never reaches the screen.
test("a stub is watched the same way a secondary is", async () => {
  vi.useFakeTimers({ shouldAdvanceTime: true });
  const paths = trackFetchedPaths();
  // A fixed stamp, not Date.now() per request: refreshed_at moving is the
  // signal useRecordsFollowTransfers watches, so a handler that minted a new
  // one on every poll would re-read the records on every poll and the
  // assertion below would be measuring the fixture rather than the rule.
  const fetchedAt = Date.now();
  let fetched = false;
  server.use(
    http.get("/api/v1/zones", () =>
      HttpResponse.json([
        zone({
          id: 7,
          name: "ad.corp.example",
          type: "stub",
          primaries: "10.0.0.9:53",
          soa_refresh: 25_200,
          refreshed_at: fetched ? fetchedAt : 0,
        }),
      ]),
    ),
  );
  mockZoneRecords(7, []);

  renderWithProviders(<ZonesList />);
  await screen.findAllByTestId("zone-row");
  const before = reads(paths, "/api/v1/zones");

  // The scheduler's first fetch landed. Nothing told this page so.
  fetched = true;
  await act(async () => {
    await vi.advanceTimersByTimeAsync(5 * 60_000);
  });

  expect(reads(paths, "/api/v1/zones")).toBeGreaterThan(before);
  // …and the records that arrived with it were re-read exactly once, off
  // refreshed_at moving — the same rule that follows a transfer.
  await waitFor(() => expect(reads(paths, "/api/v1/zones/7/records")).toBe(2));
});

// The hint cell's error branch, on both fields it can carry one for.
//
// `upstreamError` was renamed from `primariesError` when this cell grew from
// serving one type to serving four, and the rename promised coverage the
// wiring did not have: it was fed `errors.primaries` alone and rendered a
// `<FormField name="primaries">` unconditionally, so a forwarder's error
// would have been dropped on the floor at both ends. There is no `forward_to`
// rule in the schema today — a forwarder's upstreams may legitimately be
// empty — which is precisely what makes this worth holding: the next person
// to add one binds it to a prop whose name says it is already handled.
test("the hint cell shows a validation error for whichever upstream field the type uses", () => {
  const stub = renderWithProviders(
    <TypeAwareFieldsHarness
      type="stub"
      fieldError={{ name: "primaries", message: "Where to pull from, e.g. 192.168.150.1" }}
    />,
  );
  expect(screen.getByText("Where to pull from, e.g. 192.168.150.1")).toBeInTheDocument();
  // The error takes its own column over from the hint rather than appearing
  // beside it.
  expect(screen.queryByText("Primary servers, comma separated.")).not.toBeInTheDocument();
  stub.unmount();

  renderWithProviders(
    <TypeAwareFieldsHarness
      type="forwarder"
      fieldError={{ name: "forward_to", message: "bad forward target" }}
    />,
  );
  expect(screen.getByText("bad forward target")).toBeInTheDocument();
  expect(screen.queryByText("Upstream servers, comma separated.")).not.toBeInTheDocument();
});

// Eighty zones used to mean eighty record requests the moment the page
// loaded, for a number most of those rows are scrolled past without anyone
// reading — and every record write invalidates the whole zoneRecords tree,
// so each write cost eighty more. The count is now asked for per row, when
// the row is actually on screen.
test("a row's record count is not requested until the row is on screen", async () => {
  // Over setup.ts's own stub, which reports everything visible at once: this
  // one hands the callback back so the test decides when a row appears.
  const shown: (() => void)[] = [];
  vi.stubGlobal(
    "IntersectionObserver",
    class {
      constructor(private readonly callback: IntersectionObserverCallback) {}
      observe(target: Element) {
        shown.push(() =>
          this.callback(
            [{ target, isIntersecting: true } as IntersectionObserverEntry],
            this as unknown as IntersectionObserver,
          ),
        );
      }
      unobserve() {}
      disconnect() {}
    },
  );

  const paths = trackFetchedPaths();
  mockZones([zone({ id: 1, name: "a.example" }), zone({ id: 2, name: "b.example" })]);
  mockZoneRecords(1, [zoneRecord(1, 1), zoneRecord(1, 2)]);
  mockZoneRecords(2, [zoneRecord(2, 3)]);

  renderWithProviders(<ZonesList />);
  await screen.findByText("a.example");

  // Both rows are in the document and neither count has been asked for.
  await new Promise((resolve) => setTimeout(resolve, 30));
  expect(reads(paths, "/api/v1/zones/1/records")).toBe(0);
  expect(reads(paths, "/api/v1/zones/2/records")).toBe(0);
  expect(screen.getAllByText("—").length).toBeGreaterThan(0);

  // The first row scrolls into view; only its own count is fetched.
  act(() => shown[0]());
  await waitFor(() => expect(screen.getByText("2")).toBeInTheDocument());
  expect(reads(paths, "/api/v1/zones/1/records")).toBe(1);
  expect(reads(paths, "/api/v1/zones/2/records")).toBe(0);

  act(() => shown[1]());
  await waitFor(() => expect(reads(paths, "/api/v1/zones/2/records")).toBe(1));
});

// ── Clone ─────────────────────────────────────────────────────────────────

test("cloning a zone asks for the new name, then POSTs it", async () => {
  const user = userEvent.setup();
  let body: unknown;
  mockZones([zone({ id: 7, name: "e412.in" })]);
  server.use(
    http.post("/api/v1/zones/7/clone", async ({ request }) => {
      body = await request.json();
      return HttpResponse.json({ id: 8 }, { status: 201 });
    }),
  );

  renderWithProviders(<ZonesList />);
  await waitFor(() => expect(zoneRows()).toHaveLength(1));

  const menu = await rowMenu(zoneRows()[0], "e412.in");
  fireEvent.click(within(menu).getByRole("menuitem", { name: "Clone" }));

  const dialog = await screen.findByRole("dialog");
  // The field opens blank rather than pre-filled with the source's name: the
  // one value that cannot be reused is the one being asked for.
  const field = within(dialog).getByLabelText(/new zone name/i);
  expect(field).toHaveValue("");
  await user.type(field, "e412.dev");
  await user.click(within(dialog).getByRole("button", { name: /^clone$/i }));

  await waitFor(() => expect(body).toEqual({ name: "e412.dev" }));
});

// The server owns the name rules, so its refusal is what the dialog shows —
// not a second copy of them in the browser.
test("a refused clone keeps the dialog open and says why", async () => {
  const user = userEvent.setup();
  mockZones([zone({ id: 7, name: "e412.in" })]);
  server.use(
    http.post("/api/v1/zones/7/clone", () =>
      HttpResponse.json({ error: "a zone with that name already exists" }, { status: 409 }),
    ),
  );
  const errorSpy = vi.spyOn(toast, "error");

  renderWithProviders(<ZonesList />);
  await waitFor(() => expect(zoneRows()).toHaveLength(1));

  const menu = await rowMenu(zoneRows()[0], "e412.in");
  fireEvent.click(within(menu).getByRole("menuitem", { name: "Clone" }));
  const dialog = await screen.findByRole("dialog");
  await user.type(within(dialog).getByLabelText(/new zone name/i), "taken.example");
  await user.click(within(dialog).getByRole("button", { name: /^clone$/i }));

  await waitFor(() =>
    expect(errorSpy).toHaveBeenCalledWith("a zone with that name already exists"),
  );
  expect(screen.getByRole("dialog")).toBeInTheDocument();
});

// A built-in has no menu at all, so there is nothing to hide — but a clone of
// one is refused by the server, and the disclosure's rows must keep offering
// nothing rather than gaining an action that 409s.
test("a built-in zone offers no clone", async () => {
  const user = userEvent.setup();
  mockZones([zone({ id: 2, name: "localhost", type: "internal" })]);

  renderWithProviders(<ZonesList />);
  await user.click(await screen.findByRole("button", { name: /1 built-in zone/i }));

  expect(screen.queryByRole("button", { name: /Actions for localhost/ })).not.toBeInTheDocument();
});
