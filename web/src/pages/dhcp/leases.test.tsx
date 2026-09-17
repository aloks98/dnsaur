import { http, HttpResponse } from "msw";
import { afterEach, expect, test, vi } from "vitest";
import { act, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { server } from "../../test/msw-server";
import { dhcpHandlers, dhcpLease, dhcpScope } from "../../test/msw-handlers";
import { renderWithProviders } from "../../test/render";
import { DHCPLeases, expiresIn } from "./leases";

function leaseRows(): HTMLElement[] {
  return Array.from(document.querySelectorAll<HTMLElement>('[data-testid="lease-row"]'));
}

function renderLeases() {
  return renderWithProviders(<DHCPLeases />, { route: "/dhcp/leases" });
}

afterEach(() => {
  vi.useRealTimers();
});

test("expires in counts down, and a reservation nothing has leased has no countdown", () => {
  const now = Date.now();
  expect(expiresIn(now + 22 * 3_600_000 + 3 * 60_000, now)).toBe("22h 3m");
  expect(expiresIn(now + 48 * 60_000, now)).toBe("48m");
  // expires_at 0 is the one field that tells a pinned address nothing has
  // asked for from a live lease.
  expect(expiresIn(0, now)).toBe("—");
});

test("a lease row carries the address, the device's own name and its scope", async () => {
  server.use(
    ...dhcpHandlers({
      scopes: [dhcpScope()],
      leases: [
        dhcpLease({ ip: "192.168.150.104", hostname: "alok-mbp", reserved: true }),
        dhcpLease({ ip: "192.168.150.117", mac: "b8:27:eb:4e:02:d1", hostname: "" }),
      ],
    }),
  );
  renderLeases();

  await waitFor(() => expect(leaseRows()).toHaveLength(2));
  const first = leaseRows()[0];
  expect(within(first).getByText("192.168.150.104")).toBeInTheDocument();
  expect(within(first).getByText("a4:83:e7:12:9f:c0")).toBeInTheDocument();
  expect(within(first).getByText("alok-mbp")).toBeInTheDocument();
  expect(within(first).getByText("Main LAN")).toBeInTheDocument();
  expect(within(first).getByText("reserved")).toBeInTheDocument();

  // A device that offered no name at all says so rather than leaving the
  // column blank, which would read as a rendering fault.
  expect(within(leaseRows()[1]).getByText("no hostname")).toBeInTheDocument();
  expect(within(leaseRows()[1]).queryByText("reserved")).not.toBeInTheDocument();

  expect(screen.getByText("2 leases · 1 reserved")).toBeInTheDocument();
});

test("an empty table says so", async () => {
  server.use(...dhcpHandlers({ leases: [] }));
  renderLeases();

  expect(await screen.findByText("No leases yet")).toBeInTheDocument();
  expect(screen.getByText("0 leases")).toBeInTheDocument();
});

// The lease table is the engine's, not this page's: rows appear because a
// device asked for an address, and nothing on screen causes that. The
// interval is the `dhcp.lease_poll_seconds` setting, so this asserts both
// that the page polls and that it polls on the operator's number.
test("the table refreshes on the lease poll, at the interval the setting names", async () => {
  vi.useFakeTimers({ shouldAdvanceTime: true });
  let served = [dhcpLease({ ip: "192.168.150.104" })];
  server.use(
    // Ahead of the fixture's own leases handler: server.use keeps the order
    // it is given, so the first match wins.
    http.get("/api/v1/dhcp/leases", () => HttpResponse.json(served)),
    http.get("/api/v1/settings", () => HttpResponse.json({ "dhcp.lease_poll_seconds": "4" })),
    ...dhcpHandlers(),
  );
  renderLeases();

  await waitFor(() => expect(leaseRows()).toHaveLength(1));

  served = [...served, dhcpLease({ ip: "192.168.150.117", mac: "b8:27:eb:4e:02:d1" })];
  // Not yet: a page polling on some other schedule would already have it.
  await act(async () => {
    await vi.advanceTimersByTimeAsync(2_000);
  });
  expect(leaseRows()).toHaveLength(1);

  await act(async () => {
    await vi.advanceTimersByTimeAsync(2_500);
  });
  await waitFor(() => expect(leaseRows()).toHaveLength(2));
});

test("Release hands the address back and the row goes, and Reserve pins it", async () => {
  const released: string[] = [];
  const reserved: string[] = [];
  let served = [dhcpLease({ ip: "192.168.150.104" })];
  server.use(
    http.get("/api/v1/dhcp/leases", () => HttpResponse.json(served)),
    ...dhcpHandlers(),
    http.delete("/api/v1/dhcp/leases/:ip", ({ params }) => {
      // The real handler drops the row from its own table before answering
      // (internal/dhcp's Manager.Release), so the fixture does too — a
      // fixture that kept serving the row would be kinder than the server.
      released.push(String(params.ip));
      served = served.filter((l) => l.ip !== params.ip);
      return new HttpResponse(null, { status: 204 });
    }),
    http.post("/api/v1/dhcp/leases/:ip/reserve", ({ params }) => {
      reserved.push(String(params.ip));
      return HttpResponse.json({ id: 9 }, { status: 201 });
    }),
  );
  renderLeases();

  await waitFor(() => expect(leaseRows()).toHaveLength(1));
  await userEvent.click(within(leaseRows()[0]).getByRole("button", { name: "Reserve" }));
  await waitFor(() => expect(reserved).toEqual(["192.168.150.104"]));

  await userEvent.click(within(leaseRows()[0]).getByRole("button", { name: "Release" }));
  await waitFor(() => expect(released).toEqual(["192.168.150.104"]));
  // The server drops the row from its own table the moment the engine
  // accepts, rather than waiting for the next poll to rebuild one without
  // it — so the re-read this mutation triggers is already short of it, and
  // Release does not read as a button that did nothing.
  await waitFor(() => expect(leaseRows()).toHaveLength(0));
});

test("a replica cannot reserve, and can still release", async () => {
  server.use(
    ...dhcpHandlers({
      leases: [dhcpLease({ ip: "192.168.150.104" })],
      sync: { role: "replica", peer_url: "https://main.lan" },
    }),
  );
  renderLeases();

  await waitFor(() => expect(leaseRows()).toHaveLength(1));
  const row = leaseRows()[0];
  expect(within(row).getByRole("button", { name: "Reserve" })).toBeDisabled();
  // A lease belongs to the engine rather than to the configuration, and
  // Kea's HA propagates the release — so this one stays live.
  expect(within(row).getByRole("button", { name: "Release" })).toBeEnabled();
  expect(screen.getByText("Managed by the main")).toBeInTheDocument();
});

test("a reservation nothing has leased cannot be released", async () => {
  server.use(
    ...dhcpHandlers({
      scopes: [dhcpScope()],
      leases: [dhcpLease({ ip: "192.168.150.50", expires_at: 0, reserved: true })],
    }),
  );
  renderLeases();

  await waitFor(() => expect(leaseRows()).toHaveLength(1));
  const row = leaseRows()[0];
  // There is no lease to hand back — the operator pinned the address and no
  // client has asked for it — so the engine answers lease4-del with "no such
  // lease" and the button is a 404 waiting to happen.
  expect(within(row).getByRole("button", { name: "Release" })).toBeDisabled();
  expect(within(row).getByRole("button", { name: "Reserve" })).toBeDisabled();
});

test("a lease under a scope that has been deleted says so, and cannot be reserved", async () => {
  server.use(
    ...dhcpHandlers({
      scopes: [dhcpScope({ id: 1 })],
      leases: [dhcpLease({ ip: "192.168.150.104", scope_id: 9 })],
    }),
  );
  renderLeases();

  await waitFor(() => expect(leaseRows()).toHaveLength(1));
  const row = leaseRows()[0];
  // A lease the engine still holds for a subnet dnsaur no longer configures.
  // Blank would read as a rendering fault, and Reserve would 404 on the
  // scope the reservation would have to belong to.
  expect(within(row).getByText("deleted scope")).toBeInTheDocument();
  expect(within(row).getByRole("button", { name: "Reserve" })).toBeDisabled();
  expect(within(row).getByRole("button", { name: "Release" })).toBeEnabled();
});
