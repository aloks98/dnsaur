import { http, HttpResponse } from "msw";
import { expect, test } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { server } from "../../test/msw-server";
import { dhcpHandlers, dhcpReservation, dhcpScope } from "../../test/msw-handlers";
import { renderWithProviders } from "../../test/render";
import { DHCPReservations } from "./reservations";

function reservationRows(): HTMLElement[] {
  return Array.from(document.querySelectorAll<HTMLElement>('[data-testid="reservation-row"]'));
}

function renderReservations(route = "/dhcp/reservations") {
  return renderWithProviders(<DHCPReservations />, { route });
}

const TWO_SCOPES = [dhcpScope(), dhcpScope({ id: 2, name: "Office" })];

test("a row carries the scope, the address, the MAC and an em dash for no comment", async () => {
  server.use(
    ...dhcpHandlers({
      scopes: TWO_SCOPES,
      reservations: [
        dhcpReservation({ comment: "" }),
        dhcpReservation({
          id: 2,
          scope_id: 2,
          ip: "192.168.151.20",
          mac: "14:7d:da:c3:50:2e",
          hostname: "office-printer",
          comment: "Fixed for the AirPlay allow-rule",
        }),
      ],
    }),
  );
  renderReservations();

  await waitFor(() => expect(reservationRows()).toHaveLength(2));
  const first = reservationRows()[0];
  expect(within(first).getByText("Main LAN")).toBeInTheDocument();
  expect(within(first).getByText("192.168.150.10")).toBeInTheDocument();
  expect(within(first).getByText("a4:83:e7:12:9f:c0")).toBeInTheDocument();
  expect(within(first).getByText("alok-mbp")).toBeInTheDocument();
  expect(within(first).getByText("—")).toBeInTheDocument();

  expect(
    within(reservationRows()[1]).getByText("Fixed for the AirPlay allow-rule"),
  ).toBeInTheDocument();
});

test("the create form posts the scope and a canonical MAC", async () => {
  let body: unknown;
  server.use(
    ...dhcpHandlers({ scopes: TWO_SCOPES, reservations: [] }),
    http.post("/api/v1/dhcp/reservations", async ({ request }) => {
      body = await request.json();
      return HttpResponse.json(dhcpReservation(), { status: 201 });
    }),
  );
  renderReservations();

  await userEvent.click(await screen.findByRole("button", { name: /new reservation/i }));
  await userEvent.selectOptions(screen.getByLabelText("Scope"), "2");
  await userEvent.type(screen.getByLabelText("Address"), "192.168.151.20");
  // Typed in hyphen notation and in caps, which the server would accept and
  // then store as the colon form — so the row would come back spelled
  // differently from what was typed.
  await userEvent.type(screen.getByLabelText("MAC"), "14-7D-DA-C3-50-2E");
  await userEvent.type(screen.getByLabelText("Hostname"), "office-printer");
  await userEvent.click(screen.getByRole("button", { name: "Add" }));

  await waitFor(() => expect(body).toBeDefined());
  expect(body).toEqual({
    scope_id: 2,
    ip: "192.168.151.20",
    mac: "14:7d:da:c3:50:2e",
    hostname: "office-printer",
    comment: "",
  });
});

test("a MAC that is not one is refused before the round trip", async () => {
  server.use(...dhcpHandlers({ scopes: TWO_SCOPES, reservations: [] }));
  renderReservations();

  await userEvent.click(await screen.findByRole("button", { name: /new reservation/i }));
  await userEvent.type(screen.getByLabelText("Address"), "192.168.150.50");
  await userEvent.type(screen.getByLabelText("MAC"), "not-a-mac");
  await userEvent.click(screen.getByRole("button", { name: "Add" }));

  expect(
    await screen.findByText("Enter a hardware address, e.g. aa:bb:cc:dd:ee:ff"),
  ).toBeInTheDocument();
});

test("editing patches the row without its scope, which is fixed once created", async () => {
  let body: unknown;
  let patched = "";
  server.use(
    ...dhcpHandlers({ scopes: TWO_SCOPES }),
    http.patch("/api/v1/dhcp/reservations/:id", async ({ request, params }) => {
      patched = String(params.id);
      body = await request.json();
      return new HttpResponse(null, { status: 204 });
    }),
  );
  renderReservations();

  await waitFor(() => expect(reservationRows()).toHaveLength(1));
  await userEvent.click(within(reservationRows()[0]).getByRole("button", { name: "Edit" }));

  // No scope select on an edit: an address validated against one subnet must
  // not be carried into another, and the server refuses a PATCH that moves
  // one.
  expect(screen.queryByLabelText("Scope")).not.toBeInTheDocument();
  await userEvent.type(screen.getByLabelText("Comment"), "dnsaur replica");
  await userEvent.click(screen.getByRole("button", { name: "Save" }));

  await waitFor(() => expect(body).toBeDefined());
  expect(patched).toBe("1");
  expect(body).toEqual({
    ip: "192.168.150.10",
    mac: "a4:83:e7:12:9f:c0",
    hostname: "alok-mbp",
    comment: "dnsaur replica",
  });
});

test("Delete asks first, then deletes", async () => {
  const deleted: string[] = [];
  server.use(
    ...dhcpHandlers({ scopes: TWO_SCOPES }),
    http.delete("/api/v1/dhcp/reservations/:id", ({ params }) => {
      deleted.push(String(params.id));
      return new HttpResponse(null, { status: 204 });
    }),
  );
  renderReservations();

  await waitFor(() => expect(reservationRows()).toHaveLength(1));
  await userEvent.click(within(reservationRows()[0]).getByRole("button", { name: "Delete" }));
  await userEvent.click(
    within(await screen.findByRole("alertdialog")).getByRole("button", { name: "Delete" }),
  );

  await waitFor(() => expect(deleted).toEqual(["1"]));
});

// The Scopes page's reservations column links here with the filter already
// set, which is how most visits to this screen start.
test("?scope= narrows the list, and the select moves it", async () => {
  server.use(
    ...dhcpHandlers({
      scopes: TWO_SCOPES,
      reservations: [
        dhcpReservation(),
        dhcpReservation({ id: 2, scope_id: 2, ip: "192.168.151.20", hostname: "office-printer" }),
      ],
    }),
  );
  renderReservations("/dhcp/reservations?scope=2");

  await waitFor(() => expect(reservationRows()).toHaveLength(1));
  expect(within(reservationRows()[0]).getByText("office-printer")).toBeInTheDocument();
  expect(screen.getByLabelText("Filter by scope")).toHaveValue("2");

  await userEvent.selectOptions(screen.getByLabelText("Filter by scope"), "0");
  await waitFor(() => expect(reservationRows()).toHaveLength(2));
});

// "No reservations yet" under a filter hiding four of them is a lie, and
// the way out is the filter rather than the form.
test("a filter that hides every row says so, and offers the way back", async () => {
  server.use(...dhcpHandlers({ scopes: TWO_SCOPES }));
  renderReservations("/dhcp/reservations?scope=2");

  expect(await screen.findByText("No reservations in this scope")).toBeInTheDocument();
  expect(screen.queryByText("No reservations yet")).not.toBeInTheDocument();
  // The header counts what is on screen and says which set that is; the
  // chrome cell above keeps the total.
  expect(screen.getByText("0 in this scope")).toBeInTheDocument();

  await userEvent.click(screen.getByRole("button", { name: "All scopes" }));
  await waitFor(() => expect(reservationRows()).toHaveLength(1));
  expect(screen.getByText("1 reservation")).toBeInTheDocument();
});

test("an unknown ?scope= shows everything rather than an empty table", async () => {
  server.use(...dhcpHandlers({ scopes: TWO_SCOPES }));
  renderReservations("/dhcp/reservations?scope=99");

  await waitFor(() => expect(reservationRows()).toHaveLength(1));
});

test("a replica states whose configuration this is and offers no write", async () => {
  server.use(
    ...dhcpHandlers({
      scopes: TWO_SCOPES,
      sync: { role: "replica", peer_url: "https://main.lan" },
    }),
  );
  renderReservations();

  expect(await screen.findByText("Managed by the main")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: /new reservation/i })).toBeDisabled();
  await waitFor(() => expect(reservationRows()).toHaveLength(1));
  const row = reservationRows()[0];
  expect(within(row).getByRole("button", { name: "Edit" })).toBeDisabled();
  expect(within(row).getByRole("button", { name: "Delete" })).toBeDisabled();
  // The filter is a read and stays live.
  expect(screen.getByLabelText("Filter by scope")).toBeEnabled();
});
