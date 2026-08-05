import { http, HttpResponse } from "msw";
import { toast } from "sonner";
import { expect, test, vi } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { server } from "../test/msw-server";
import { renderWithProviders } from "../test/render";
import type { LocalRecord } from "../api/types";
import { LocalDns } from "./dns";

function record(overrides: Partial<LocalRecord> = {}): LocalRecord {
  return { id: 1, name: "nas.home.lan", type: "A", value: "192.168.1.50", ttl: 300, ...overrides };
}

function mockRecords(records: LocalRecord[]) {
  server.use(http.get("/api/v1/records", () => HttpResponse.json(records)));
}

test("renders the records table with name, type badge, value, and ttl", async () => {
  mockRecords([record()]);

  renderWithProviders(<LocalDns />);

  const table = await screen.findByRole("table");
  expect(within(table).getByText("nas.home.lan")).toBeInTheDocument();
  expect(within(table).getByText("A")).toBeInTheDocument();
  expect(within(table).getByText("192.168.1.50")).toBeInTheDocument();
  expect(within(table).getByText("300")).toBeInTheDocument();
});

test("an A record with an IPv6 value shows an inline error and never posts", async () => {
  const user = userEvent.setup();
  let posted = false;
  mockRecords([record()]);
  server.use(
    http.post("/api/v1/records", () => {
      posted = true;
      return HttpResponse.json({ id: 99 }, { status: 201 });
    }),
  );

  renderWithProviders(<LocalDns />);
  await screen.findByText("nas.home.lan");

  await user.click(screen.getByRole("button", { name: /^add record$/i }));
  const dialog = await screen.findByRole("dialog");

  // Type defaults to A — leave it as-is and supply an IPv6 value.
  await user.type(within(dialog).getByLabelText(/^name$/i), "printer.home.lan");
  await user.type(within(dialog).getByLabelText(/^ipv4 address$/i), "2001:db8::1");
  await user.click(within(dialog).getByRole("button", { name: /^add record$/i }));

  expect(await within(dialog).findByText(/enter a valid ipv4 address/i)).toBeInTheDocument();
  // Give any accidental async POST a chance to land before asserting it didn't.
  await new Promise((resolve) => setTimeout(resolve, 30));
  expect(posted).toBe(false);
  expect(screen.getByRole("dialog")).toBeInTheDocument();
});

// The Value rule is chosen by the Type select beside it, so a *shown*
// error has to be re-judged against the newly picked type the moment it
// changes — otherwise the record the admin has now made valid still reads
// as rejected until they submit again. Regression-worthy because the
// re-check hangs off a read of the form's error state, and the obvious
// read (form.formState.errors) is a render-time snapshot that can lag
// behind inside an event handler; see the Select's onValueChange.
test("switching the type clears a value error the new type accepts", async () => {
  const user = userEvent.setup();
  mockRecords([record()]);

  renderWithProviders(<LocalDns />);
  await screen.findByText("nas.home.lan");

  await user.click(screen.getByRole("button", { name: /^add record$/i }));
  const dialog = await screen.findByRole("dialog");

  await user.type(within(dialog).getByLabelText(/^name$/i), "v6.home.lan");
  await user.type(within(dialog).getByLabelText(/^ipv4 address$/i), "2001:db8::1");
  await user.click(within(dialog).getByRole("button", { name: /^add record$/i }));
  expect(await within(dialog).findByText(/enter a valid ipv4 address/i)).toBeInTheDocument();

  await user.click(within(dialog).getByRole("combobox", { name: /record type/i }));
  await user.click(await screen.findByRole("option", { name: /^aaaa$/i }));

  await waitFor(() =>
    expect(within(dialog).queryByText(/enter a valid ipv4 address/i)).not.toBeInTheDocument(),
  );
});

// Every field reports on the same submit: the type-dependent value check
// spans two fields, so it can't be skipped just because a *different*
// field (name, ttl) failed in the same pass — an admin fixing one error
// at a time, submit by submit, is the failure mode this guards.
test("an empty form reports the name, value and ttl errors at once", async () => {
  const user = userEvent.setup();
  mockRecords([record()]);

  renderWithProviders(<LocalDns />);
  await screen.findByText("nas.home.lan");

  await user.click(screen.getByRole("button", { name: /^add record$/i }));
  const dialog = await screen.findByRole("dialog");
  await user.clear(within(dialog).getByLabelText(/^ttl/i));
  await user.click(within(dialog).getByRole("button", { name: /^add record$/i }));

  expect(await within(dialog).findByText(/enter a domain/i)).toBeInTheDocument();
  expect(within(dialog).getByText(/enter a valid ipv4 address/i)).toBeInTheDocument();
  expect(within(dialog).getByText(/ttl must be a whole number/i)).toBeInTheDocument();
});

test("a valid add posts /records (wildcard name included) and the new row appears", async () => {
  const user = userEvent.setup();
  let requestBody: unknown;
  let records = [record()];
  server.use(
    http.get("/api/v1/records", () => HttpResponse.json(records)),
    http.post("/api/v1/records", async ({ request }) => {
      requestBody = await request.json();
      const created = record({ id: 2, name: "*.iot.home.lan", value: "192.168.1.99" });
      records = [...records, created];
      return HttpResponse.json({ id: created.id }, { status: 201 });
    }),
  );

  renderWithProviders(<LocalDns />);
  await screen.findByText("nas.home.lan");

  await user.click(screen.getByRole("button", { name: /^add record$/i }));
  const dialog = await screen.findByRole("dialog");

  await user.type(within(dialog).getByLabelText(/^name$/i), "*.iot.home.lan");
  await user.type(within(dialog).getByLabelText(/^ipv4 address$/i), "192.168.1.99");
  await user.click(within(dialog).getByRole("button", { name: /^add record$/i }));

  await waitFor(() =>
    expect(requestBody).toEqual({
      name: "*.iot.home.lan",
      type: "A",
      value: "192.168.1.99",
      ttl: 300,
    }),
  );
  expect(await screen.findByText("*.iot.home.lan")).toBeInTheDocument();
  // Sheet closes on success (its base-ui exit animation lingers in the DOM
  // for a frame or two even under jsdom, so this needs to be a waitFor
  // rather than a synchronous assertion).
  await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
});

// Regression, mirroring pages/filtering/groups-clients.test.tsx's IPv6
// zone-id test: the server itself lowercases the name and trims a trailing
// dot (see internal/api/records_handlers.go's normalizeRecord) before
// checking it, so the client validator must not reject case or a trailing
// dot the server would happily normalize away.
test("a name with mixed case and a trailing dot is accepted and posts /records", async () => {
  const user = userEvent.setup();
  let requestBody: unknown;
  mockRecords([record()]);
  server.use(
    http.post("/api/v1/records", async ({ request }) => {
      requestBody = await request.json();
      return HttpResponse.json({ id: 99 }, { status: 201 });
    }),
  );

  renderWithProviders(<LocalDns />);
  await screen.findByText("nas.home.lan");

  await user.click(screen.getByRole("button", { name: /^add record$/i }));
  const dialog = await screen.findByRole("dialog");

  await user.type(within(dialog).getByLabelText(/^name$/i), "NAS.Home.LAN.");
  await user.type(within(dialog).getByLabelText(/^ipv4 address$/i), "192.168.1.50");
  await user.click(within(dialog).getByRole("button", { name: /^add record$/i }));

  await waitFor(() =>
    expect(requestBody).toEqual({
      name: "NAS.Home.LAN.",
      type: "A",
      value: "192.168.1.50",
      ttl: 300,
    }),
  );
});

test("switching the type select updates the value field's label and placeholder", async () => {
  const user = userEvent.setup();
  mockRecords([record()]);

  renderWithProviders(<LocalDns />);
  await screen.findByText("nas.home.lan");

  await user.click(screen.getByRole("button", { name: /^add record$/i }));
  const dialog = await screen.findByRole("dialog");

  expect(within(dialog).getByLabelText(/^ipv4 address$/i)).toBeInTheDocument();

  await user.click(within(dialog).getByRole("combobox", { name: /record type/i }));
  await user.click(await screen.findByRole("option", { name: /^aaaa$/i }));

  expect(within(dialog).getByLabelText(/^ipv6 address$/i)).toBeInTheDocument();
  expect(within(dialog).getByPlaceholderText(/2001:db8::1/i)).toBeInTheDocument();
});

// Regression: RFC 4291 §2.2 defines a dotted-quad *tail* form for IPv6 —
// e.g. the NAT64 well-known prefix — and Go's net.ParseIP/To4() (the
// server's actual validator) accepts it as a genuine AAAA value (To4() is
// nil for it, so the AAAA check passes). An earlier version of
// isValidIPv6 excluded any literal "." outright, which wrongly blocked
// this from ever reaching the server.
test("an embedded-IPv4 (NAT64) AAAA value is accepted and posts /records", async () => {
  const user = userEvent.setup();
  let requestBody: unknown;
  mockRecords([record()]);
  server.use(
    http.post("/api/v1/records", async ({ request }) => {
      requestBody = await request.json();
      return HttpResponse.json({ id: 99 }, { status: 201 });
    }),
  );

  renderWithProviders(<LocalDns />);
  await screen.findByText("nas.home.lan");

  await user.click(screen.getByRole("button", { name: /^add record$/i }));
  const dialog = await screen.findByRole("dialog");

  await user.type(within(dialog).getByLabelText(/^name$/i), "nat64.home.lan");
  await user.click(within(dialog).getByRole("combobox", { name: /record type/i }));
  await user.click(await screen.findByRole("option", { name: /^aaaa$/i }));
  await user.type(within(dialog).getByLabelText(/^ipv6 address$/i), "64:ff9b::192.0.2.1");
  await user.click(within(dialog).getByRole("button", { name: /^add record$/i }));

  await waitFor(() =>
    expect(requestBody).toEqual({
      name: "nat64.home.lan",
      type: "AAAA",
      value: "64:ff9b::192.0.2.1",
      ttl: 300,
    }),
  );
});

// isValidIPv6 deliberately doesn't special-case the narrower IPv4-mapped
// form (::ffff:a.b.c.d) — which the server's `ip.To4() == nil` check does
// reject as AAAA — since reliably distinguishing "mapped" from "embedded"
// dotted-quad text needs real IPv6 parsing, not a string check. A false
// accept here is expected to surface as the server's own 400 via toast,
// not silently succeed.
test("an IPv4-mapped AAAA value posts, and the server's rejection surfaces as a toast", async () => {
  const user = userEvent.setup();
  let posted = false;
  mockRecords([record()]);
  server.use(
    http.post("/api/v1/records", () => {
      posted = true;
      return HttpResponse.json({ error: "value must be an IPv6 address" }, { status: 400 });
    }),
  );
  const errorSpy = vi.spyOn(toast, "error");

  renderWithProviders(<LocalDns />);
  await screen.findByText("nas.home.lan");

  await user.click(screen.getByRole("button", { name: /^add record$/i }));
  const dialog = await screen.findByRole("dialog");

  await user.type(within(dialog).getByLabelText(/^name$/i), "mapped.home.lan");
  await user.click(within(dialog).getByRole("combobox", { name: /record type/i }));
  await user.click(await screen.findByRole("option", { name: /^aaaa$/i }));
  await user.type(within(dialog).getByLabelText(/^ipv6 address$/i), "::ffff:192.168.1.1");
  await user.click(within(dialog).getByRole("button", { name: /^add record$/i }));

  await waitFor(() => expect(posted).toBe(true));
  await waitFor(() =>
    expect(errorSpy).toHaveBeenCalledWith(expect.stringMatching(/ipv6 address/i)),
  );
  // The sheet stays open on a failed submission (no onSuccess close), so
  // the admin can see the error and correct the value in place.
  expect(screen.getByRole("dialog")).toBeInTheDocument();
});

test("editing a record PUTs /records/{id} with the updated fields", async () => {
  const user = userEvent.setup();
  let requestUrl: string | undefined;
  let requestBody: unknown;
  mockRecords([record({ id: 7, name: "printer.home.lan", value: "192.168.1.20" })]);
  server.use(
    http.put("/api/v1/records/7", async ({ request }) => {
      requestUrl = request.url;
      requestBody = await request.json();
      return new HttpResponse(null, { status: 204 });
    }),
  );

  renderWithProviders(<LocalDns />);
  await screen.findByText("printer.home.lan");

  await user.click(screen.getByRole("button", { name: /^edit printer\.home\.lan$/i }));
  const dialog = await screen.findByRole("dialog");
  const valueInput = within(dialog).getByLabelText(/^ipv4 address$/i);
  await user.clear(valueInput);
  await user.type(valueInput, "192.168.1.21");
  await user.click(within(dialog).getByRole("button", { name: /^save$/i }));

  await waitFor(() =>
    expect(requestBody).toEqual({
      name: "printer.home.lan",
      type: "A",
      value: "192.168.1.21",
      ttl: 300,
    }),
  );
  expect(requestUrl).toMatch(/\/api\/v1\/records\/7$/);
});

// base-ui keeps the panel mounted through its exit transition, so whatever
// the sheet renders while it's on the way out is visible. Deriving `open`
// from `editTarget !== null` meant closing nulled the target *and* left the
// panel on screen, re-rendering it as the add variant: the header flipped
// "Edit record" -> "Add record" and the button "Save" -> "Add record" on
// every close, including right after a successful save.
test("closing the edit sheet keeps its Edit copy through the exit transition", async () => {
  const user = userEvent.setup();
  mockRecords([record({ id: 7, name: "printer.home.lan" })]);

  renderWithProviders(<LocalDns />);
  await screen.findByText("printer.home.lan");

  await user.click(screen.getByRole("button", { name: /^edit printer\.home\.lan$/i }));
  const sheet = await screen.findByRole("dialog");
  expect(within(sheet).getByRole("heading", { name: /^edit record$/i })).toBeInTheDocument();

  await user.click(within(sheet).getByRole("button", { name: /^cancel$/i }));

  const closing = screen.getByRole("dialog");
  expect(within(closing).getByRole("heading", { name: /^edit record$/i })).toBeInTheDocument();
  expect(within(closing).getByRole("button", { name: /^save$/i })).toBeInTheDocument();

  await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
});

// A background refetch failing must not destroy content that's already
// loaded. Every mutation here invalidates the records query, so a blip on
// that refetch used to swap a correct, populated table for "Couldn't load
// local DNS records. Try refreshing the page." — with the rows sitting in
// the cache the whole time.
test("a failing background refetch keeps the table and offers a retry instead", async () => {
  const user = userEvent.setup();
  let getCount = 0;
  server.use(
    http.get("/api/v1/records", () => {
      getCount += 1;
      return getCount === 1
        ? HttpResponse.json([record({ id: 7, name: "printer.home.lan" })])
        : HttpResponse.json({ error: "boom" }, { status: 500 });
    }),
    http.delete("/api/v1/records/7", () => new HttpResponse(null, { status: 204 })),
  );

  renderWithProviders(<LocalDns />);
  await screen.findByText("printer.home.lan");

  await user.click(screen.getByRole("button", { name: /^delete printer\.home\.lan$/i }));
  const confirmDialog = await screen.findByRole("alertdialog");
  await user.click(within(confirmDialog).getByRole("button", { name: /^delete$/i }));

  expect(
    await screen.findByText(/couldn't refresh local dns records/i, undefined, { timeout: 3000 }),
  ).toBeInTheDocument();
  expect(screen.queryByText(/couldn't load local dns records/i)).not.toBeInTheDocument();
  expect(screen.getByRole("table")).toBeInTheDocument();
  expect(screen.getByText("printer.home.lan")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: /try again/i })).toBeInTheDocument();
});

test("deleting a record asks for confirmation, then DELETEs /records/{id}", async () => {
  const user = userEvent.setup();
  let deleted = false;
  let records = [record()];
  server.use(
    http.get("/api/v1/records", () => HttpResponse.json(records)),
    http.delete("/api/v1/records/1", () => {
      deleted = true;
      records = records.filter((r) => r.id !== 1);
      return new HttpResponse(null, { status: 204 });
    }),
  );

  renderWithProviders(<LocalDns />);
  await screen.findByText("nas.home.lan");

  await user.click(screen.getByRole("button", { name: /^delete nas\.home\.lan$/i }));
  const confirmDialog = await screen.findByRole("alertdialog");
  await user.click(within(confirmDialog).getByRole("button", { name: /^delete$/i }));

  await waitFor(() => expect(deleted).toBe(true));
  await waitFor(() => expect(screen.queryByText("nas.home.lan")).not.toBeInTheDocument());
});

test("an empty list shows EmptyState with an Add record action", async () => {
  mockRecords([]);

  renderWithProviders(<LocalDns />);

  expect(await screen.findByText("No local DNS records yet")).toBeInTheDocument();
  // Only one "Add record" affordance when empty — the EmptyState's own
  // action — not a redundant second button in the header above it.
  expect(screen.getAllByRole("button", { name: /^add record$/i })).toHaveLength(1);
});
