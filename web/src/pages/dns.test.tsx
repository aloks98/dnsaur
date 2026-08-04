import { http, HttpResponse } from "msw";
import { expect, test } from "vitest";
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
