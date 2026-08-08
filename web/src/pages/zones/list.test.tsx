import { http, HttpResponse } from "msw";
import { toast } from "sonner";
import { afterEach, expect, test, vi } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useForm } from "react-hook-form";
import { Form } from "@e412/rnui-react";
import { server } from "../../test/msw-server";
import { renderWithProviders } from "../../test/render";
import type { Zone, ZoneRecord } from "../../api/types";
import { CreateRowHint, PrimariesField, ZonesList, type AddZoneValues } from "./list";

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
 * Renders PrimariesField and CreateRowHint directly, with `type` set by the
 * caller rather than driven through the create row's real select — which,
 * in this milestone, offers only "primary" (see list.tsx's CREATABLE_TYPES)
 * and so can never actually produce "secondary"/"stub"/"forwarder" itself.
 * This harness is currently the *only* way anything exercises those two
 * components with a non-primary type; see their own comments in list.tsx.
 */
function TypeAwareFieldsHarness({ type }: { type: Zone["type"] }) {
  const form = useForm<AddZoneValues>({
    defaultValues: { name: "", type: "primary", primaries: "" },
  });
  return (
    <Form {...form}>
      <PrimariesField type={type} control={form.control} />
      <CreateRowHint type={type} control={form.control} nameError={undefined} />
    </Form>
  );
}

afterEach(() => vi.restoreAllMocks());

// The RFC 6303 zones exist to stop junk queries reaching the roots;
// offering a delete that the API refuses is a button that lies.
test("internal zones cannot be deleted", async () => {
  mockZones([
    zone({ id: 1, name: "e412.in", type: "primary" }),
    zone({ id: 2, name: "localhost", type: "internal" }),
  ]);
  renderWithProviders(<ZonesList />);
  const rows = await screen.findAllByTestId("zone-row");

  expect(within(rows[0]).getByRole("button", { name: /delete/i })).toBeInTheDocument();
  expect(within(rows[1]).queryByRole("button", { name: /delete/i })).not.toBeInTheDocument();
});

// The artboard's stronger claim: an internal zone's Actions cell doesn't
// merely omit buttons, it says BUILT-IN outright, and the name itself is
// plain text with a padlock rather than the primary-coloured link editable
// zones get — three separate "this is not yours to change" signals, all
// pinned together since they're one design decision.
test("an internal zone shows Built-in instead of action buttons, and a padlock beside its plain (non-link) name", async () => {
  mockZones([zone({ id: 2, name: "localhost", type: "internal" })]);
  renderWithProviders(<ZonesList />);
  const rows = await screen.findAllByTestId("zone-row");

  expect(within(rows[0]).getByText(/built-in/i)).toBeInTheDocument();
  expect(within(rows[0]).queryByRole("button")).not.toBeInTheDocument();
  expect(within(rows[0]).queryByRole("link", { name: "localhost" })).not.toBeInTheDocument();
  expect(within(rows[0]).getByText("localhost")).toBeInTheDocument();
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
  mockZones([
    zone({ id: 11, name: "sec.example.com", type: "secondary" }),
    zone({ id: 12, name: "stub.example.com", type: "stub" }),
    zone({ id: 13, name: "fwd.example.com", type: "forwarder" }),
    zone({ id: 14, name: "localhost", type: "internal" }),
  ]);
  renderWithProviders(<ZonesList />);
  const rows = await screen.findAllByTestId("zone-row");

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

// Milestone A ships primary zones only: a secondary/stub/forwarder zone
// with no transfer mechanism behind it (transfers arrive in #72) doesn't
// merely do nothing — answer.go's forwarder/stub fall-through list is
// missing `secondary`, so it gets served like a primary, holding only its
// apex NS and NXDOMAINing every other name under a domain the user owns.
// The API now 400s anything but primary; a single-option select is the
// honest reflection of that on this row, not an arbitrary restriction.
// While `primary` is the only creatable type, the type cell is static text
// rather than a one-option select — a select a user can open and not change
// is a control that does nothing. It still has to POST `primary`, which is
// what the hidden input is for and what this asserts.
test("the type cell states primary rather than offering a dead select", async () => {
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
  expect(within(row).queryByRole("combobox")).not.toBeInTheDocument();
  expect(within(row).getByText("primary")).toBeInTheDocument();

  await user.type(within(row).getByPlaceholderText("home.lan"), "e412.in");
  await user.click(within(row).getByRole("button", { name: "Add" }));
  // `type` is omitted, not sent as "primary": onSubmit drops it when it is
  // the default, and handleZoneCreate reads an absent type as primary. The
  // hidden input exists to keep the form value valid for the zod enum, not
  // to put a field on the wire.
  await waitFor(() => expect(body).toEqual({ name: "e412.in" }));
});

// The type cell above is static "primary" (previous test), so nothing
// a real user can click ever drives PrimariesField/CreateRowHint's `type`
// prop away from "primary" — this harness (see its own comment) is
// currently the only thing that does, ahead of #72 widening the select
// back out to the four types this still knows how to render.
test("the primaries field and its hint appear only for the three non-primary types", () => {
  const { rerender } = render(<TypeAwareFieldsHarness type="primary" />);
  expect(screen.queryByLabelText(/primary servers/i)).not.toBeInTheDocument();
  expect(screen.getByText("SOA defaults are filled in.")).toBeInTheDocument();

  for (const type of ["secondary", "stub", "forwarder"] as const) {
    rerender(<TypeAwareFieldsHarness type={type} />);
    expect(screen.getByLabelText(/primary servers/i)).toBeInTheDocument();
    expect(screen.getByText("Primary servers, comma separated.")).toBeInTheDocument();
    expect(screen.queryByText("SOA defaults are filled in.")).not.toBeInTheDocument();
  }

  rerender(<TypeAwareFieldsHarness type="primary" />);
  expect(screen.queryByLabelText(/primary servers/i)).not.toBeInTheDocument();
  expect(screen.getByText("SOA defaults are filled in.")).toBeInTheDocument();

  // internal is never offered by the select either, but PrimariesField
  // treats it the same as primary — no primaries of its own.
  rerender(<TypeAwareFieldsHarness type="internal" />);
  expect(screen.queryByLabelText(/primary servers/i)).not.toBeInTheDocument();
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

test("a non-expired secondary shows no EXPIRED warning row", async () => {
  mockZones([
    zone({
      id: 8,
      name: "ok-secondary.example.com",
      type: "secondary",
      expires_at: Date.now() + 999_999,
    }),
  ]);
  renderWithProviders(<ZonesList />);
  await screen.findAllByTestId("zone-row");

  expect(screen.queryByText("Expired")).not.toBeInTheDocument();
  expect(screen.queryByRole("button", { name: /retry transfer/i })).not.toBeInTheDocument();
});
