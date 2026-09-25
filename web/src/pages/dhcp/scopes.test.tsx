import { http, HttpResponse } from "msw";
import { expect, test } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { server } from "../../test/msw-server";
import {
  dhcpClass,
  dhcpHandlers,
  dhcpScope,
  dhcpStatus,
  dhcpReservation,
} from "../../test/msw-handlers";
import { renderWithProviders } from "../../test/render";
import type { DHCPStatus } from "../../api/types";
import { DHCPScopes, engineLine, poolRowErrors } from "./scopes";

function scopeRows(): HTMLElement[] {
  return Array.from(document.querySelectorAll<HTMLElement>('[data-testid="scope-row"]'));
}

async function renderScopes() {
  const result = renderWithProviders(<DHCPScopes />, { route: "/dhcp" });
  await screen.findByText("Scopes");
  return result;
}

/** Name, subnet and one pool: the least a scope can be saved with. */
async function fillBasics(dialog: HTMLElement) {
  await userEvent.type(within(dialog).getByLabelText("Name"), "Lab");
  await userEvent.type(within(dialog).getByLabelText("Subnet"), "192.168.153.0/24");
  await userEvent.type(within(dialog).getByLabelText("Pool 1 start"), "192.168.153.50");
  await userEvent.type(within(dialog).getByLabelText("Pool 1 end"), "192.168.153.99");
}

// --- the status line -------------------------------------------------------
//
// Pure, so every state is asserted as its exact sentence rather than through
// a render that could only show one of them at a time.

test("the engine line states the version and the pair, or says why it can't", () => {
  expect(engineLine(dhcpStatus(), false)?.text).toBe("Engine 2.6.3 · single");

  const paired = dhcpStatus({
    ha: {
      mode: "hot-standby",
      local_state: "hot-standby",
      peer: "backup-box",
      remote_state: "hot-standby",
      communication_interrupted: false,
      unacked_clients: 0,
    },
  });
  expect(engineLine(paired, false)?.text).toBe(
    "Engine 2.6.3 · hot-standby with backup-box · hot-standby",
  );
  // `peer` is omitempty: an engine whose status-get does not carry one drops
  // the clause rather than inventing a name from the config-sync side.
  const nameless = dhcpStatus({
    ha: {
      mode: "hot-standby",
      local_state: "hot-standby",
      remote_state: "hot-standby",
      communication_interrupted: false,
      unacked_clients: 0,
    },
  });
  expect(engineLine(nameless, false)?.text).toBe("Engine 2.6.3 · hot-standby · hot-standby");

  expect(engineLine(dhcpStatus({ engine: "unreachable" }), false)).toEqual({
    text: "Engine unreachable",
    tone: "bad",
    rejected: false,
  });

  const rejected = dhcpStatus({
    engine: "config rejected",
    message: "subnet4[1]: pool 192.168.150.100-192.168.150.199 is not in subnet 192.168.151.0/24",
  });
  expect(engineLine(rejected, false)).toEqual({
    text:
      "Config rejected: subnet4[1]: pool 192.168.150.100-192.168.150.199 " +
      "is not in subnet 192.168.151.0/24",
    tone: "bad",
    rejected: true,
  });

  // A replica with no HA block is not a single box: the main did not choose
  // it as the standby, so it is running plain Kea beside a pair it is not in.
  expect(engineLine(dhcpStatus(), true)?.text).toBe("DHCP: not in the HA pair");
  // And a box with no engine has no line at all.
  expect(engineLine({ enabled: false, table_age_seconds: 0, scopes: [] }, false)).toBeNull();
});

test("a rejected config offers Apply again, and applying re-sends it", async () => {
  const applied: DHCPStatus[] = [];
  server.use(
    ...dhcpHandlers({
      status: dhcpStatus({ engine: "config rejected", message: "pool is not in subnet" }),
    }),
    http.post("/api/v1/dhcp/apply", () => {
      const next = dhcpStatus();
      applied.push(next);
      return HttpResponse.json(next);
    }),
  );
  await renderScopes();

  expect(await screen.findByText("Config rejected: pool is not in subnet")).toBeInTheDocument();
  await userEvent.click(screen.getByRole("button", { name: "Apply again" }));

  await waitFor(() => expect(applied).toHaveLength(1));
  // The answer is the status this render left behind, written straight into
  // the cache — the line moves without waiting out a refetch.
  expect(await screen.findByText("Engine 2.6.3 · single")).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Apply again" })).not.toBeInTheDocument();
});

// --- the table -------------------------------------------------------------

test("a scope row shows its pool and how much of it the engine has handed out", async () => {
  server.use(
    ...dhcpHandlers({
      scopes: [dhcpScope(), dhcpScope({ id: 2, name: "Guest wifi", enabled: false })],
      status: dhcpStatus({
        scopes: [
          { id: 1, pool_size: 100, leased: 61 },
          { id: 2, pool_size: 256, leased: 4 },
        ],
      }),
      reservations: [dhcpReservation(), dhcpReservation({ id: 2, ip: "192.168.150.11" })],
    }),
  );
  await renderScopes();

  await waitFor(() => expect(scopeRows()).toHaveLength(2));
  const main = scopeRows()[0];
  expect(within(main).getByText("192.168.150.0/24")).toBeInTheDocument();
  expect(within(main).getByText("61")).toBeInTheDocument();
  expect(within(main).getByText("/ 100")).toBeInTheDocument();
  // Two reservations in scope 1, none in scope 2 — the link carries the
  // filter the Reservations page reads.
  expect(within(main).getByRole("link", { name: "2 reservations" })).toHaveAttribute(
    "href",
    "/dhcp/reservations?scope=1",
  );
  // rnui's Button over a Link, so base-ui marks it role="button" — it still
  // navigates, and on a replica it can still say it is disabled, which an
  // <a> cannot.
  expect(within(scopeRows()[1]).getByRole("button", { name: /add reservation/i })).toHaveAttribute(
    "href",
    "/dhcp/reservations?scope=2",
  );

  // One scope disabled, said once beside the heading.
  expect(screen.getByText("1 disabled")).toBeInTheDocument();
});

test("the create dialog posts every field the form owns", async () => {
  let body: unknown;
  server.use(
    ...dhcpHandlers({ scopes: [] }),
    http.post("/api/v1/dhcp/scopes", async ({ request }) => {
      body = await request.json();
      return HttpResponse.json(dhcpScope(), { status: 201 });
    }),
  );
  await renderScopes();

  await userEvent.click(screen.getByRole("button", { name: /new scope/i }));
  const dialog = await screen.findByRole("dialog");
  await userEvent.type(within(dialog).getByLabelText("Name"), "Lab");
  await userEvent.type(within(dialog).getByLabelText("Subnet"), "192.168.153.0/24");
  await userEvent.type(within(dialog).getByLabelText("Pool 1 start"), "192.168.153.50");
  await userEvent.type(within(dialog).getByLabelText("Pool 1 end"), "192.168.153.99");
  await userEvent.type(within(dialog).getByLabelText("Gateway"), "192.168.153.1");
  await userEvent.type(within(dialog).getByLabelText("DNS suffix"), "lab.e412.in");
  await userEvent.click(within(dialog).getByRole("button", { name: "Save" }));

  await waitFor(() => expect(body).toBeDefined());
  expect(body).toEqual({
    name: "Lab",
    cidr: "192.168.153.0/24",
    pools: [{ start: "192.168.153.50", end: "192.168.153.99", class_id: 0 }],
    gateway: "192.168.153.1",
    domain: "lab.e412.in",
    // Blank is "use the dhcp.lease_seconds setting", which is 0 on the wire.
    lease_seconds: 0,
    enabled: true,
    dns_servers: "",
    domain_search: "",
    ntp_servers: "",
    static_routes: [],
    next_server: "",
    server_hostname: "",
    boot_file: "",
    options: [],
    // Absent would mean `true` on a create anyway, but this form owns the
    // switch, so it always says which.
    match_client_id: true,
    reservations_only: false,
  });
});

// A scope sorts by id, so a new one lands at the bottom of a long list with
// nothing saying which of them you just wrote.
test("the scope a create just produced is marked in the table", async () => {
  let scopes = [dhcpScope()];
  server.use(
    http.get("/api/v1/dhcp/scopes", () => HttpResponse.json(scopes)),
    http.post("/api/v1/dhcp/scopes", () => {
      const created = dhcpScope({ id: 2, name: "Lab", cidr: "192.168.153.0/24" });
      scopes = [...scopes, created];
      return HttpResponse.json(created, { status: 201 });
    }),
    ...dhcpHandlers(),
  );
  await renderScopes();
  await waitFor(() => expect(scopeRows()).toHaveLength(1));

  await userEvent.click(screen.getByRole("button", { name: /new scope/i }));
  const dialog = await screen.findByRole("dialog");
  await userEvent.type(within(dialog).getByLabelText("Name"), "Lab");
  await userEvent.type(within(dialog).getByLabelText("Subnet"), "192.168.153.0/24");
  await userEvent.type(within(dialog).getByLabelText("Pool 1 start"), "192.168.153.50");
  await userEvent.type(within(dialog).getByLabelText("Pool 1 end"), "192.168.153.99");
  await userEvent.click(within(dialog).getByRole("button", { name: "Save" }));

  await waitFor(() => expect(scopeRows()).toHaveLength(2));
  expect(within(scopeRows()[1]).getByText("new")).toBeInTheDocument();
  // Only the one that was just written.
  expect(within(scopeRows()[0]).queryByText("new")).not.toBeInTheDocument();
});

test("Client options is a tab of its own, and its fields post with the rest", async () => {
  let body: Record<string, unknown> | undefined;
  server.use(
    ...dhcpHandlers({ scopes: [] }),
    http.post("/api/v1/dhcp/scopes", async ({ request }) => {
      body = (await request.json()) as Record<string, unknown>;
      return HttpResponse.json(dhcpScope(), { status: 201 });
    }),
  );
  await renderScopes();

  await userEvent.click(screen.getByRole("button", { name: /new scope/i }));
  const dialog = await screen.findByRole("dialog");
  await fillBasics(dialog);
  // Network is where a new scope starts: the half without which there is no
  // scope at all.
  expect(within(dialog).getByRole("tab", { name: "Network" })).toHaveAttribute(
    "aria-selected",
    "true",
  );
  expect(within(dialog).queryByLabelText("NTP servers")).not.toBeInTheDocument();

  await userEvent.click(within(dialog).getByRole("tab", { name: "Client options" }));
  await userEvent.type(await within(dialog).findByLabelText("NTP servers"), "192.168.153.1");
  await userEvent.click(within(dialog).getByRole("button", { name: /add option/i }));
  await userEvent.type(within(dialog).getByLabelText("Code 1"), "252");
  await userEvent.type(within(dialog).getByLabelText("Hex value 1"), "687474703a2f2f7770");
  // The switch is labelled for what turning it on does, which is the stored
  // column's opposite.
  await userEvent.click(within(dialog).getByRole("switch", { name: "Ignore client identifier" }));

  // The other tab's values survive it being unmounted — base-ui renders one
  // panel at a time, and the form holds the fields either way.
  await userEvent.click(within(dialog).getByRole("button", { name: "Save" }));

  await waitFor(() => expect(body).toBeDefined());
  expect(body?.name).toBe("Lab");
  expect(body?.ntp_servers).toBe("192.168.153.1");
  expect(body?.options).toEqual([{ code: 252, hex: "687474703a2f2f7770" }]);
  expect(body?.match_client_id).toBe(false);
});

// The Options mini-tables are three-column grids, and each row has to put
// exactly three cells in one — a stray fourth wrapped Remove onto a line of
// its own. Removing by position is the other half: the field array is
// addressed by index, so an off-by-one deletes the wrong route.
// An incomplete Options row used to fail the schema with a message nowhere
// on screen: Save did nothing, said nothing, and the field it disliked was
// on the other tab. A row nobody filled in at all is not an error — it is
// the empty row the + button just added — so it is dropped instead.
test("an untouched option row is dropped, and a half-filled one says so", async () => {
  let body: Record<string, unknown> | undefined;
  server.use(
    ...dhcpHandlers({ scopes: [] }),
    http.post("/api/v1/dhcp/scopes", async ({ request }) => {
      body = (await request.json()) as Record<string, unknown>;
      return HttpResponse.json(dhcpScope(), { status: 201 });
    }),
  );
  await renderScopes();

  await userEvent.click(screen.getByRole("button", { name: /new scope/i }));
  const dialog = await screen.findByRole("dialog");
  await fillBasics(dialog);

  await userEvent.click(within(dialog).getByRole("tab", { name: "Client options" }));
  await userEvent.click(await within(dialog).findByRole("button", { name: /add option/i }));
  await userEvent.click(within(dialog).getByRole("button", { name: "Save" }));

  // The blank row went nowhere near the request.
  await waitFor(() => expect(body).toBeDefined());
  expect(body?.options).toEqual([]);

  // Half of one is a different thing: it says what is missing, on the field
  // that is missing it, and no second request goes out.
  body = undefined;
  await userEvent.click(screen.getByRole("button", { name: /new scope/i }));
  const again = await screen.findByRole("dialog");
  await fillBasics(again);
  await userEvent.click(within(again).getByRole("tab", { name: "Client options" }));
  await userEvent.click(await within(again).findByRole("button", { name: /add option/i }));
  await userEvent.type(within(again).getByLabelText("Code 1"), "252");
  await userEvent.click(within(again).getByRole("button", { name: "Save" }));

  expect(await within(again).findByText("Enter the value's bytes")).toBeInTheDocument();
  expect(within(again).getByLabelText("Hex value 1")).toHaveAttribute("aria-invalid", "true");
  await new Promise((resolve) => setTimeout(resolve, 30));
  expect(body).toBeUndefined();
});

// The same rule for the other mini-table, and the tab switch that makes the
// message reachable: a field the form refused is no use behind a tab the
// operator is not looking at.
test("a half-filled route blocks Save and pulls the tab it is on into view", async () => {
  let posted = false;
  server.use(
    ...dhcpHandlers({ scopes: [] }),
    http.post("/api/v1/dhcp/scopes", () => {
      posted = true;
      return HttpResponse.json(dhcpScope(), { status: 201 });
    }),
  );
  await renderScopes();

  await userEvent.click(screen.getByRole("button", { name: /new scope/i }));
  const dialog = await screen.findByRole("dialog");
  await fillBasics(dialog);
  await userEvent.click(within(dialog).getByRole("tab", { name: "Client options" }));
  await userEvent.click(await within(dialog).findByRole("button", { name: /add route/i }));
  await userEvent.type(within(dialog).getByLabelText("Destination 1"), "10.8.0.0/24");

  // Back to Network, so the refusal is behind the tab that is not shown.
  await userEvent.click(within(dialog).getByRole("tab", { name: "Network" }));
  await userEvent.click(within(dialog).getByRole("button", { name: "Save" }));

  expect(await within(dialog).findByText("Enter the router")).toBeInTheDocument();
  expect(within(dialog).getByRole("tab", { name: "Client options" })).toHaveAttribute(
    "aria-selected",
    "true",
  );
  await new Promise((resolve) => setTimeout(resolve, 30));
  expect(posted).toBe(false);
});

test("a static route is three cells wide, and Remove takes out the one it sits on", async () => {
  server.use(...dhcpHandlers({ scopes: [] }));
  await renderScopes();

  await userEvent.click(screen.getByRole("button", { name: /new scope/i }));
  const dialog = await screen.findByRole("dialog");
  await userEvent.click(within(dialog).getByRole("tab", { name: "Client options" }));

  const addRoute = await within(dialog).findByRole("button", { name: /add route/i });
  await userEvent.click(addRoute);
  await userEvent.type(within(dialog).getByLabelText("Destination 1"), "10.8.0.0/24");
  await userEvent.click(addRoute);
  await userEvent.type(within(dialog).getByLabelText("Destination 2"), "10.9.0.0/24");

  // Two inputs and the Remove cell, and nothing else, in the row's grid.
  const row = within(dialog).getByLabelText("Destination 1").closest("div.grid");
  expect(row?.children).toHaveLength(3);

  // Each Remove names the row it sits on, so a panel with four of them does
  // not offer four controls all called "Remove".
  await userEvent.click(within(dialog).getByRole("button", { name: "Remove route 1" }));
  expect(within(dialog).queryByLabelText("Destination 2")).not.toBeInTheDocument();
  expect(within(dialog).getByLabelText("Destination 1")).toHaveValue("10.9.0.0/24");
});

test("the row switch patches enabled alone", async () => {
  let body: unknown;
  server.use(
    ...dhcpHandlers(),
    http.patch("/api/v1/dhcp/scopes/1", async ({ request }) => {
      body = await request.json();
      return new HttpResponse(null, { status: 204 });
    }),
  );
  await renderScopes();

  await userEvent.click(await screen.findByRole("switch", { name: "Main LAN enabled" }));
  await waitFor(() => expect(body).toEqual({ enabled: false }));
});

test("a replica states whose configuration this is and offers no write", async () => {
  server.use(
    ...dhcpHandlers({
      reservations: [],
      sync: { role: "replica", peer_url: "https://main.lan" },
    }),
  );
  await renderScopes();

  expect(await screen.findByText("Managed by the main")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: /new scope/i })).toBeDisabled();
  const row = (await waitFor(() => scopeRows()))[0];
  expect(within(row).getByRole("button", { name: "Edit" })).toBeDisabled();
  expect(within(row).getByRole("button", { name: "Delete" })).toBeDisabled();
  // Add reservation is a link, and an <a> has no `disabled` — so the state
  // has to reach the accessibility tree some other way or a screen reader
  // announces a control that does nothing.
  expect(within(row).getByRole("button", { name: /add reservation/i })).toHaveAttribute(
    "aria-disabled",
    "true",
  );
  // base-ui's Switch reports its disabled state as aria-disabled, the way
  // every other disabled control in this app's tests is asserted.
  expect(within(row).getByRole("switch", { name: "Main LAN enabled" })).toHaveAttribute(
    "aria-disabled",
    "true",
  );
});

test("a replica cannot re-send a refused configuration either", async () => {
  server.use(
    ...dhcpHandlers({
      status: dhcpStatus({ engine: "config rejected", message: "pool is not in subnet" }),
      sync: { role: "replica", peer_url: "https://main.lan" },
    }),
  );
  await renderScopes();

  expect(await screen.findByRole("button", { name: "Apply again" })).toBeDisabled();
});

// --- pools -----------------------------------------------------------------

test("a pool row's four states are the board's exact words", () => {
  const classes = new Set([1]);
  const names = new Map([
    [1, "iot"],
    [7, "printers"],
  ]);
  const row = (start: string, end: string, class_id = "0") => ({ start, end, class_id });
  expect(
    poolRowErrors(
      [
        row("192.168.151.100", "192.168.151.199"),
        row("192.168.152.10", "192.168.152.20"),
        row("192.168.151.240", "192.168.151.230"),
        row("192.168.151.180", "192.168.151.220", "1"),
        row("192.168.151.221", "192.168.151.230", "7"),
        row("192.168.151.231", "192.168.151.235", "9"),
        // Half-typed and blank rows are not wrong yet.
        row("192.168.151.2", ""),
        row("", ""),
      ],
      "192.168.151.0/24",
      classes,
      names,
    ),
  ).toEqual([
    undefined,
    "Not in subnet 192.168.151.0/24",
    "Start is after end",
    "Overlaps 192.168.151.100 – 192.168.151.199",
    "Unknown class printers",
    "Unknown class 9",
    undefined,
    undefined,
  ]);
  // No class is unknown while the list is still loading.
  expect(
    poolRowErrors([row("10.0.0.1", "10.0.0.9", "7")], "10.0.0.0/24", undefined, names),
  ).toEqual([undefined]);
});

test("the dialog edits the scope's pools and patches them whole", async () => {
  let body: Record<string, unknown> | undefined;
  server.use(
    ...dhcpHandlers({
      scopes: [
        dhcpScope({
          pools: [
            { id: 4, scope_id: 1, start: "192.168.150.100", end: "192.168.150.199", class_id: 0 },
            { id: 5, scope_id: 1, start: "192.168.150.200", end: "192.168.150.239", class_id: 2 },
          ],
        }),
      ],
      classes: [dhcpClass(), dhcpClass({ id: 2, name: "printers" })],
    }),
    http.patch("/api/v1/dhcp/scopes/1", async ({ request }) => {
      body = (await request.json()) as Record<string, unknown>;
      return new HttpResponse(null, { status: 204 });
    }),
  );
  await renderScopes();

  await userEvent.click(await screen.findByRole("button", { name: "Edit" }));
  const dialog = await screen.findByRole("dialog");
  expect(within(dialog).getByLabelText("Pool 1 start")).toHaveValue("192.168.150.100");
  expect(within(dialog).getByLabelText("Pool 2 end")).toHaveValue("192.168.150.239");
  const select = await within(dialog).findByLabelText("Pool 2 class");
  await waitFor(() => expect(select).toHaveValue("2"));
  expect(
    within(select)
      .getAllByRole("option")
      .map((o) => o.textContent),
  ).toEqual(["any", "iot", "printers"]);

  await userEvent.click(within(dialog).getByRole("button", { name: /add pool/i }));
  await userEvent.type(within(dialog).getByLabelText("Pool 3 start"), "192.168.150.240");
  await userEvent.type(within(dialog).getByLabelText("Pool 3 end"), "192.168.150.249");
  await userEvent.selectOptions(within(dialog).getByLabelText("Pool 3 class"), "iot");
  await userEvent.click(within(dialog).getByRole("button", { name: "Save" }));

  await waitFor(() => expect(body).toBeDefined());
  expect(body?.pools).toEqual([
    { start: "192.168.150.100", end: "192.168.150.199", class_id: 0 },
    { start: "192.168.150.200", end: "192.168.150.239", class_id: 2 },
    { start: "192.168.150.240", end: "192.168.150.249", class_id: 1 },
  ]);
  expect(body).not.toHaveProperty("pool_start");
  expect(body).not.toHaveProperty("pool_end");
});

test("an overlapping row says which range it overlaps, and Save waits for it", async () => {
  server.use(...dhcpHandlers({ scopes: [] }));
  await renderScopes();

  await userEvent.click(screen.getByRole("button", { name: /new scope/i }));
  const dialog = await screen.findByRole("dialog");
  await fillBasics(dialog);
  await userEvent.click(within(dialog).getByRole("button", { name: /add pool/i }));
  await userEvent.type(within(dialog).getByLabelText("Pool 2 start"), "192.168.153.90");
  await userEvent.type(within(dialog).getByLabelText("Pool 2 end"), "192.168.153.120");

  expect(await within(dialog).findByRole("alert")).toHaveTextContent(
    "Overlaps 192.168.153.50 – 192.168.153.99",
  );
  expect(within(dialog).getByLabelText("Pool 2 start")).toHaveAttribute("aria-invalid", "true");
  expect(within(dialog).getByRole("button", { name: "Save" })).toBeDisabled();

  await userEvent.clear(within(dialog).getByLabelText("Pool 2 start"));
  await userEvent.type(within(dialog).getByLabelText("Pool 2 start"), "192.168.153.100");
  expect(within(dialog).queryByRole("alert")).not.toBeInTheDocument();
  expect(within(dialog).getByRole("button", { name: "Save" })).toBeEnabled();
});

test("a scope needs a pool unless it is reservations only", async () => {
  let body: Record<string, unknown> | undefined;
  server.use(
    ...dhcpHandlers({ scopes: [] }),
    http.post("/api/v1/dhcp/scopes", async ({ request }) => {
      body = (await request.json()) as Record<string, unknown>;
      return HttpResponse.json(dhcpScope(), { status: 201 });
    }),
  );
  await renderScopes();

  await userEvent.click(screen.getByRole("button", { name: /new scope/i }));
  const dialog = await screen.findByRole("dialog");
  await userEvent.type(within(dialog).getByLabelText("Name"), "Lab");
  await userEvent.type(within(dialog).getByLabelText("Subnet"), "192.168.153.0/24");
  await userEvent.click(within(dialog).getByRole("button", { name: "Save" }));
  expect(await within(dialog).findByText("Add a pool")).toBeInTheDocument();
  expect(body).toBeUndefined();

  await userEvent.click(within(dialog).getByRole("tab", { name: "Client options" }));
  await userEvent.click(await within(dialog).findByRole("switch", { name: "Reservations only" }));
  await userEvent.click(within(dialog).getByRole("button", { name: "Save" }));
  await waitFor(() => expect(body).toBeDefined());
  expect(body?.pools).toEqual([]);
});

test("a refusal naming pools[i] lands on that row", async () => {
  server.use(
    ...dhcpHandlers({ scopes: [] }),
    http.post("/api/v1/dhcp/scopes", () =>
      HttpResponse.json({ error: "pools[1]: no class with id 9" }, { status: 422 }),
    ),
  );
  await renderScopes();

  await userEvent.click(screen.getByRole("button", { name: /new scope/i }));
  const dialog = await screen.findByRole("dialog");
  await fillBasics(dialog);
  // A blank row between is dropped on the way out, so the server's
  // pools[1] is the form's third row.
  const add = within(dialog).getByRole("button", { name: /add pool/i });
  await userEvent.click(add);
  await userEvent.click(add);
  await userEvent.type(within(dialog).getByLabelText("Pool 3 start"), "192.168.153.150");
  await userEvent.type(within(dialog).getByLabelText("Pool 3 end"), "192.168.153.159");
  await userEvent.click(within(dialog).getByRole("button", { name: "Save" }));

  expect(await within(dialog).findByRole("alert")).toHaveTextContent(
    "pools[1]: no class with id 9",
  );
  expect(within(dialog).getByLabelText("Pool 3 start")).toHaveAttribute("aria-invalid", "true");
  expect(within(dialog).getByLabelText("Pool 1 start")).toHaveAttribute("aria-invalid", "false");
});

test("the Pool column reads the first range and how many more, Leased the sum", async () => {
  server.use(
    ...dhcpHandlers({
      scopes: [
        dhcpScope({
          pools: [
            { id: 1, scope_id: 1, start: "192.168.150.100", end: "192.168.150.199", class_id: 0 },
            { id: 2, scope_id: 1, start: "192.168.150.200", end: "192.168.150.239", class_id: 1 },
          ],
        }),
      ],
      status: dhcpStatus({ scopes: [{ id: 1, pool_size: 140, leased: 65 }] }),
    }),
  );
  await renderScopes();

  await waitFor(() => expect(scopeRows()).toHaveLength(1));
  const row = scopeRows()[0];
  expect(within(row).getByText("+1 more")).toBeInTheDocument();
  expect(row).toHaveTextContent("192.168.150.100 – 192.168.150.199");
  expect(row).not.toHaveTextContent("192.168.150.239");
  expect(within(row).getByText("65")).toBeInTheDocument();
  expect(within(row).getByText("/ 140")).toBeInTheDocument();
});
