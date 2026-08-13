import { http, HttpResponse } from "msw";
import { toast } from "sonner";
import { afterEach, expect, test, vi } from "vitest";
import { act, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useForm } from "react-hook-form";
import { Form } from "@e412/rnui-react";
import { server } from "../../test/msw-server";
import { renderWithProviders } from "../../test/render";
import type { Zone, ZoneRecord } from "../../api/types";
import { CreateRowHint, PrimariesField, TSIGKeyField, ZonesList, type AddZoneValues } from "./list";

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

async function openCreateRow(user: ReturnType<typeof userEvent.setup>) {
  // findAllBy* rather than findBy*: an empty list shows "New zone" twice
  // (header + empty-state body), and this only needs one of them clicked.
  const [button] = await screen.findAllByRole("button", { name: /new zone/i });
  await user.click(button);
}

/**
 * Renders the three type-dependent create-row cells directly, with `type` set
 * by the caller rather than driven through the real select. The select can
 * now produce both values a user can pick, but it cannot produce `stub`,
 * `forwarder` or `internal` — and those are exactly the cases worth pinning,
 * since the artboard asks for `stub`/`forwarder` to behave like `secondary`
 * here and they deliberately do not (see PrimariesField in list.tsx).
 */
function TypeAwareFieldsHarness({ type }: { type: Zone["type"] }) {
  const form = useForm<AddZoneValues>({
    defaultValues: { name: "", type: "primary", primaries: "", tsig_key_id: "" },
  });
  return (
    <Form {...form}>
      <PrimariesField type={type} control={form.control} />
      <TSIGKeyField type={type} control={form.control} />
      <CreateRowHint
        type={type}
        control={form.control}
        nameError={undefined}
        primariesError={undefined}
      />
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
  expect(within(rows[0]).getByRole("button", { name: /delete/i })).toBeInTheDocument();
  expect(within(rows[1]).queryByRole("button", { name: /delete/i })).not.toBeInTheDocument();
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

test("expanding the disclosure reveals built-in rows with a BUILT-IN cell and no edit or delete buttons", async () => {
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
  expect(within(builtinRow).queryByRole("button", { name: /edit/i })).not.toBeInTheDocument();
  expect(within(builtinRow).queryByRole("button", { name: /delete/i })).not.toBeInTheDocument();
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

  await user.click(screen.getByRole("button", { name: /delete old.example.com/i }));
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

  await user.click(screen.getByRole("button", { name: /delete old.example.com/i }));
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
test("the type select offers primary and secondary, and nothing the API would refuse", async () => {
  const user = userEvent.setup();
  mockZones([]);
  renderWithProviders(<ZonesList />);
  await openCreateRow(user);

  const select = screen.getByLabelText("Zone type");
  expect(Array.from(select.querySelectorAll("option")).map((option) => option.value)).toEqual([
    "primary",
    "secondary",
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

// The type cell above is static "primary" (previous test), so nothing
// a real user can click ever drives PrimariesField/CreateRowHint's `type`
// prop away from "primary" — this harness (see its own comment) is
// currently the only thing that does, ahead of #72 widening the select
// back out to the four types this still knows how to render.
// The artboard's `needsPrimaries` covers secondary, stub AND forwarder. Only
// secondary is right: neither of the other two is creatable at all (the API
// 400s them), and a forwarder does not have primaries in the first place — it
// has upstreams to ask, which is a different field for a different milestone.
// Building the artboard as drawn would have put a "Primary servers" input in
// front of a zone type that has none.
test("the transfer fields and their hints appear for secondary alone, never for stub or forwarder", () => {
  // A fresh render per type rather than rerender(): rerender replaces the
  // whole tree, wrapper included, and TSIGKeyField reads a query client.
  const secondary = renderWithProviders(<TypeAwareFieldsHarness type="secondary" />);
  expect(screen.getByLabelText(/primary servers/i)).toBeInTheDocument();
  expect(screen.getByLabelText(/tsig key/i)).toBeInTheDocument();
  expect(screen.getByText("Primary servers, comma separated.")).toBeInTheDocument();
  expect(screen.getByText("Optional.")).toBeInTheDocument();
  expect(screen.queryByText("SOA defaults are filled in.")).not.toBeInTheDocument();
  secondary.unmount();

  for (const type of ["primary", "stub", "forwarder", "internal"] as const) {
    const other = renderWithProviders(<TypeAwareFieldsHarness type={type} />);
    expect(screen.queryByLabelText(/primary servers/i)).not.toBeInTheDocument();
    expect(screen.queryByLabelText(/tsig key/i)).not.toBeInTheDocument();
    other.unmount();
  }

  renderWithProviders(<TypeAwareFieldsHarness type="primary" />);
  expect(screen.getByText("SOA defaults are filled in.")).toBeInTheDocument();
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
test("an expired secondary shows the EXPIRED warning row with a Retry transfer button", async () => {
  const seventeenDaysAgo = Date.now() - 17 * 86_400_000;
  mockZones([
    zone({
      id: 6,
      name: "branch.example.com",
      type: "secondary",
      enabled: true,
      primaries: "203.0.113.9",
      refreshed_at: seventeenDaysAgo,
      expires_at: Date.now() - 1000,
    }),
  ]);
  renderWithProviders(<ZonesList />);
  const rows = await screen.findAllByTestId("zone-row");

  expect(within(rows[0]).getByText("Not answering")).toBeInTheDocument();
  expect(within(rows[0]).getByText("Expired")).toBeInTheDocument();
  expect(
    within(rows[0]).getByText(
      "Transfer from 203.0.113.9 last succeeded 17 days ago, past the SOA expiry.",
    ),
  ).toBeInTheDocument();
  const retry = within(rows[0]).getByRole("button", { name: /retry transfer/i });
  expect(retry).toBeInTheDocument();
  // Deliberately inert in this milestone — see the component's own comment.
  expect(retry).not.toHaveAttribute("type", "submit");
});

test("a healthy secondary shows no warning row at all, and reads when it last refreshed", async () => {
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
  expect(screen.queryByText("Expired")).not.toBeInTheDocument();
  expect(screen.queryByRole("button", { name: /retry transfer/i })).not.toBeInTheDocument();
});

// The state a fresh secondary is in for its first minute, and the one an
// "Enabled" badge is most wrong about: the zone is enabled in the database
// and answering SERVFAIL for its whole suffix, because it holds nothing it
// may speak for (Zone.Serving, internal/zones/answer.go).
test("a secondary that has never transferred says so, rather than reading as enabled", async () => {
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
  // gets, because it is the same fact — and the warning row below is what
  // says which of the two this is.
  expect(within(rows[0]).getByText("Not answering")).toBeInTheDocument();
  expect(within(rows[0]).getByText("Never transferred")).toBeInTheDocument();
  expect(within(rows[0]).queryByText("Enabled")).not.toBeInTheDocument();
});

// The whole point of persisting last_error: this zone is failing for a
// reason recorded in the database, so it reads the same after a restart as
// before one. The row leads with the failure itself and keeps the server's
// whole sentence on the element, because rewriting a resolver error into
// "couldn't transfer" throws away everything actionable about it.
test("a failing secondary leads with the failure and keeps the whole message", async () => {
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

  // Not "Serving, transfer failing": the note below says what is being
  // served, and how old it is, which the status column cannot.
  expect(within(rows[0]).getByText("Transfer failing")).toBeInTheDocument();
  const cause = within(rows[0]).getByTestId("zone-transfer-error");
  // Just the failure — this page's own sentence about the consequence is a
  // separate element beside it, not concatenated onto the end of it.
  expect(cause.textContent).toBe("connection refused");
  expect(cause).toHaveAttribute("title", "203.0.113.9:53: dial tcp: connect: connection refused");
  expect(within(rows[0]).getByTestId("zone-transfer-note").textContent).toBe(
    "Still serving the copy from 1d ago.",
  );
});

// The defect this row had: the transfer's error and this page's own sentence
// about the consequence were one concatenated string, so a long error ran
// into the note with nothing between them and pushed it out of the row.
// Whatever the server said, the note is a separate element and stays whole.
test("a long transfer error does not run into the note or crowd it out", async () => {
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

  // The one thing the row has to keep: what the zone is doing about it, in
  // its own element rather than glued to the end of 500 characters of error.
  const note = within(rows[0]).getByTestId("zone-transfer-note");
  expect(note.textContent).toBe("Still serving the copy from 1d ago.");

  // And what is drawn of the error is short whatever its length — the part
  // that says what went wrong, not the leading context an ellipsis would
  // have left behind — with nothing of this page's own sentence run into it.
  const cause = within(rows[0]).getByTestId("zone-transfer-error");
  expect(cause.textContent).toBe("connection refused");
  // Nothing is lost by drawing less of it.
  expect(cause).toHaveAttribute("title", longError);
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

// The button was inert through Milestone A — there was no transfer endpoint
// to call. It has one now.
test("Retry transfer asks the server for a transfer now", async () => {
  const user = userEvent.setup();
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

  await user.click(screen.getByRole("button", { name: /retry transfer/i }));
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
  expect(within(rows[0]).getByText("Overdue")).toBeInTheDocument();
  expect(
    within(rows[0]).getByText(/nothing has transferred from 203\.0\.113\.9 since 5h ago/i),
  ).toBeInTheDocument();
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
  expect(within(rows[0]).queryByText("Expired")).not.toBeInTheDocument();
  expect(
    within(rows[0]).queryByRole("button", { name: /retry transfer/i }),
  ).not.toBeInTheDocument();
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
  expect(within(zoneRows()[0]).getByText("1")).toBeInTheDocument();

  refreshedAt = Date.now();
  await act(async () => {
    await vi.advanceTimersByTimeAsync(30_000);
  });

  await waitFor(() => expect(within(zoneRows()[0]).getByText("2")).toBeInTheDocument());
  expect(reads(paths, "/api/v1/zones/6/records")).toBe(2);
  // The primary beside it transferred nothing, because it cannot.
  expect(reads(paths, "/api/v1/zones/1/records")).toBe(1);
});
