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

/** The rows are a CSS grid, not a semantic table, so scope by the slot the
 * page marks them with — several of the strings under test ("A", "300")
 * also appear in the filter select and the form row above. */
function rows(): HTMLElement[] {
  return Array.from(document.querySelectorAll<HTMLElement>('[data-slot="record-row"]'));
}

/** The add/edit row is always on screen — there is no dialog to open. */
function nameField() {
  return screen.getByLabelText(/record name/i);
}
function valueField(re: RegExp) {
  return screen.getByLabelText(re);
}
function submit(name: RegExp) {
  return screen.getByRole("button", { name });
}

test("renders a row per record with name, type badge, value, and ttl", async () => {
  mockRecords([record()]);

  renderWithProviders(<LocalDns />);
  await screen.findByText("nas.home.lan");

  const [row] = rows();
  expect(within(row).getByText("nas.home.lan")).toBeInTheDocument();
  expect(within(row).getByText("A")).toBeInTheDocument();
  expect(within(row).getByText("192.168.1.50")).toBeInTheDocument();
  expect(within(row).getByText("300")).toBeInTheDocument();
});

// The `*.` prefix is the difference between one name and every subdomain
// under it, and it is two characters wide in a mono string. It is marked up
// separately so it can be picked out; that must survive.
test("a wildcard name renders its prefix as its own element", async () => {
  mockRecords([record({ name: "*.iot.home.lan" })]);

  renderWithProviders(<LocalDns />);
  await screen.findByText("iot.home.lan");

  const [row] = rows();
  expect(within(row).getByText("*.")).toBeInTheDocument();
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

  // Type defaults to A — leave it as-is and supply an IPv6 value.
  await user.type(nameField(), "printer.home.lan");
  await user.type(valueField(/value.*ipv4/i), "2001:db8::1");
  await user.click(submit(/^add$/i));

  expect(await screen.findByText(/enter a valid ipv4 address/i)).toBeInTheDocument();
  // Give any accidental async POST a chance to land before asserting it didn't.
  await new Promise((resolve) => setTimeout(resolve, 30));
  expect(posted).toBe(false);
});

// The Value rule is chosen by the Type select beside it, so a *shown*
// error has to be re-judged against the newly picked type the moment it
// changes — otherwise the record the admin has now made valid still reads
// as rejected until they submit again. Regression-worthy because the
// re-check hangs off a read of the form's error state, and the obvious
// read (form.formState.errors) is a render-time snapshot that can lag
// behind inside an event handler; see the type select's onChange.
test("switching the type clears a value error the new type accepts", async () => {
  const user = userEvent.setup();
  mockRecords([record()]);

  renderWithProviders(<LocalDns />);
  await screen.findByText("nas.home.lan");

  await user.type(nameField(), "v6.home.lan");
  await user.type(valueField(/value.*ipv4/i), "2001:db8::1");
  await user.click(submit(/^add$/i));
  expect(await screen.findByText(/enter a valid ipv4 address/i)).toBeInTheDocument();

  await user.selectOptions(screen.getByLabelText(/^record type$/i), "AAAA");

  await waitFor(() =>
    expect(screen.queryByText(/enter a valid ipv4 address/i)).not.toBeInTheDocument(),
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

  await user.clear(screen.getByLabelText(/ttl in seconds/i));
  await user.click(submit(/^add$/i));

  expect(await screen.findByText(/enter a domain/i)).toBeInTheDocument();
  expect(screen.getByText(/enter a valid ipv4 address/i)).toBeInTheDocument();
  expect(screen.getByText(/ttl must be a whole number/i)).toBeInTheDocument();
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

  await user.type(nameField(), "*.iot.home.lan");
  await user.type(valueField(/value.*ipv4/i), "192.168.1.99");
  await user.click(submit(/^add$/i));

  await waitFor(() =>
    expect(requestBody).toEqual({
      name: "*.iot.home.lan",
      type: "A",
      value: "192.168.1.99",
      ttl: 300,
    }),
  );
  expect(await screen.findByText("iot.home.lan")).toBeInTheDocument();
});

// Adding one record is very often adding four, so the row has to come back
// empty rather than leaving the previous name to be manually cleared.
test("a successful add empties the form row for the next one", async () => {
  const user = userEvent.setup();
  mockRecords([record()]);
  server.use(http.post("/api/v1/records", () => HttpResponse.json({ id: 2 }, { status: 201 })));

  renderWithProviders(<LocalDns />);
  await screen.findByText("nas.home.lan");

  await user.type(nameField(), "printer.home.lan");
  await user.type(valueField(/value.*ipv4/i), "192.168.1.31");
  await user.click(submit(/^add$/i));

  await waitFor(() => expect(nameField()).toHaveValue(""));
  expect(valueField(/value.*ipv4/i)).toHaveValue("");
  expect(screen.getByLabelText(/ttl in seconds/i)).toHaveValue("300");
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

  await user.type(nameField(), "NAS.Home.LAN.");
  await user.type(valueField(/value.*ipv4/i), "192.168.1.50");
  await user.click(submit(/^add$/i));

  await waitFor(() =>
    expect(requestBody).toEqual({
      name: "NAS.Home.LAN.",
      type: "A",
      value: "192.168.1.50",
      ttl: 300,
    }),
  );
});

// One form row serves four record types, so the type select has to re-label
// the value column, its placeholder and its hint together. If they drift,
// the row silently asks for one thing and validates another.
test("switching the type re-labels the value column, placeholder and hint", async () => {
  const user = userEvent.setup();
  mockRecords([record()]);

  renderWithProviders(<LocalDns />);
  await screen.findByText("nas.home.lan");

  expect(valueField(/value.*ipv4/i)).toBeInTheDocument();
  expect(screen.getByText(/dotted-quad ipv4/i)).toBeInTheDocument();

  await user.selectOptions(screen.getByLabelText(/^record type$/i), "AAAA");

  expect(valueField(/value.*ipv6/i)).toBeInTheDocument();
  expect(screen.getByPlaceholderText("fd00::1")).toBeInTheDocument();
  expect(screen.getByText(/ipv4-mapped form/i)).toBeInTheDocument();
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

  await user.type(nameField(), "nat64.home.lan");
  await user.selectOptions(screen.getByLabelText(/^record type$/i), "AAAA");
  await user.type(valueField(/value.*ipv6/i), "64:ff9b::192.0.2.1");
  await user.click(submit(/^add$/i));

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

  await user.type(nameField(), "mapped.home.lan");
  await user.selectOptions(screen.getByLabelText(/^record type$/i), "AAAA");
  await user.type(valueField(/value.*ipv6/i), "::ffff:192.168.1.1");
  await user.click(submit(/^add$/i));

  await waitFor(() => expect(posted).toBe(true));
  await waitFor(() =>
    expect(errorSpy).toHaveBeenCalledWith(expect.stringMatching(/ipv6 address/i)),
  );
  // The typed value survives a rejected submit, so it can be corrected in
  // place rather than retyped.
  expect(valueField(/value.*ipv6/i)).toHaveValue("::ffff:192.168.1.1");
});

test("editing seeds the form row and PUTs /records/{id} with the updated fields", async () => {
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

  // The row is now bound to that record rather than creating a new one.
  await waitFor(() => expect(nameField()).toHaveValue("printer.home.lan"));
  const valueInput = valueField(/value.*ipv4/i);
  await user.clear(valueInput);
  await user.type(valueInput, "192.168.1.21");
  await user.click(submit(/^save$/i));

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

// The row is the only form on the page, so leaving it bound to a record
// after cancelling would mean the next "add" silently overwrote that record
// instead of creating one.
test("cancelling an edit returns the row to adding", async () => {
  const user = userEvent.setup();
  mockRecords([record({ id: 7, name: "printer.home.lan" })]);

  renderWithProviders(<LocalDns />);
  await screen.findByText("printer.home.lan");

  await user.click(screen.getByRole("button", { name: /^edit printer\.home\.lan$/i }));
  await waitFor(() => expect(submit(/^save$/i)).toBeInTheDocument());

  await user.click(screen.getByRole("button", { name: /cancel editing/i }));

  await waitFor(() => expect(submit(/^add$/i)).toBeInTheDocument());
  expect(nameField()).toHaveValue("");
});

test("the type filter narrows the rows, and can be cleared", async () => {
  const user = userEvent.setup();
  mockRecords([
    record({ id: 1, name: "nas.home.lan", type: "A" }),
    record({ id: 2, name: "home.lan", type: "TXT", value: "v=spf1 -all" }),
  ]);

  renderWithProviders(<LocalDns />);
  await screen.findByText("nas.home.lan");
  expect(rows()).toHaveLength(2);

  await user.selectOptions(screen.getByLabelText(/filter by record type/i), "TXT");
  await waitFor(() => expect(rows()).toHaveLength(1));
  expect(screen.getByText("home.lan")).toBeInTheDocument();

  await user.selectOptions(screen.getByLabelText(/filter by record type/i), "");
  await waitFor(() => expect(rows()).toHaveLength(2));
});

test("a search with no matches says so and offers to clear the filter", async () => {
  const user = userEvent.setup();
  mockRecords([record()]);

  renderWithProviders(<LocalDns />);
  await screen.findByText("nas.home.lan");

  await user.type(screen.getByLabelText(/search records/i), "nothing-matches-this");

  expect(await screen.findByText(/no records match this filter/i)).toBeInTheDocument();
  await user.click(screen.getByRole("button", { name: /clear it/i }));
  await waitFor(() => expect(rows()).toHaveLength(1));
});

// A background refetch failing must not destroy content that's already
// loaded. Every mutation here invalidates the records query, so a blip on
// that refetch used to swap a correct, populated table for "Couldn't load
// local DNS records. Try refreshing the page." — with the rows sitting in
// the cache the whole time.
test("a failing background refetch keeps the rows and offers a retry instead", async () => {
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

test("an empty list explains what records are for, with the form row still there", async () => {
  mockRecords([]);

  renderWithProviders(<LocalDns />);

  expect(await screen.findByText(/no local records yet/i)).toBeInTheDocument();
  // The form is the page's permanent first row, so there is nothing to
  // reveal and no separate "Add record" call to action.
  expect(screen.getByRole("button", { name: /^add$/i })).toBeInTheDocument();
});
