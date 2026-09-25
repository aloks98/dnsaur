import { http, HttpResponse } from "msw";
import { expect, test } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { server } from "../../test/msw-server";
import { dhcpClass, dhcpHandlers, dhcpScope } from "../../test/msw-handlers";
import { renderWithProviders } from "../../test/render";
import { classSummary, DHCPClasses, matcherRowErrors } from "./classes";

function classRows(): HTMLElement[] {
  return Array.from(document.querySelectorAll<HTMLElement>('[data-testid="class-row"]'));
}

async function firstRow(): Promise<HTMLElement> {
  await waitFor(() => expect(classRows().length).toBeGreaterThan(0));
  return classRows()[0];
}

async function renderClasses() {
  const result = renderWithProviders(<DHCPClasses />, { route: "/dhcp/classes" });
  await screen.findByText("Classes");
  return result;
}

/** Two scopes whose pools name class 1 three times: Office twice, IoT once. */
const inUseScopes = [
  dhcpScope({
    name: "Office",
    pools: [
      { id: 1, scope_id: 1, start: "192.168.150.100", end: "192.168.150.149", class_id: 1 },
      { id: 2, scope_id: 1, start: "192.168.150.150", end: "192.168.150.179", class_id: 1 },
      { id: 3, scope_id: 1, start: "192.168.150.180", end: "192.168.150.199", class_id: 0 },
    ],
  }),
  dhcpScope({
    id: 2,
    name: "IoT",
    cidr: "192.168.151.0/24",
    pools: [{ id: 4, scope_id: 2, start: "192.168.151.100", end: "192.168.151.199", class_id: 1 }],
  }),
];

test("the options summary names only set fields, in the board's fixed order", () => {
  expect(classSummary(dhcpClass())).toBe("");
  expect(
    classSummary(
      dhcpClass({
        dns_servers: "192.168.151.2, 192.168.151.3",
        domain: "iot.lan",
        domain_search: "iot.lan",
        ntp_servers: "192.168.151.1",
        static_routes: [{ destination: "10.8.0.0/24", router: "192.168.151.254" }],
        boot_file: "ipxe.efi",
        options: [
          { code: 252, hex: "00" },
          { code: 253, hex: "01" },
        ],
      }),
    ),
  ).toBe("DNS 192.168.151.2 +1 · suffix iot.lan · search · NTP · routes 1 · PXE · opt 2");
  expect(classSummary(dhcpClass({ next_server: "192.168.151.5" }))).toBe("PXE");
});

test("a mac value must be 1–6 whole octets; a vendor value is the server's to judge", () => {
  expect(
    matcherRowErrors([
      { kind: "mac", value: "a4:cf:12" },
      { kind: "mac", value: "A4:CF:12:00:11:22" },
      { kind: "mac", value: "84:f3:e" },
      { kind: "mac", value: "aa:bb:cc:dd:ee:ff:00" },
      { kind: "mac", value: "" },
      { kind: "vendor", value: "PXEClient:Arch:00007" },
    ]),
  ).toEqual([
    undefined,
    undefined,
    "Not a MAC prefix: 84:f3:e",
    "Not a MAC prefix: aa:bb:cc:dd:ee:ff:00",
    undefined,
    undefined,
  ]);
});

test("a class row shows its matchers, what it sets and how many pools name it", async () => {
  server.use(
    ...dhcpHandlers({
      scopes: inUseScopes,
      classes: [
        dhcpClass({
          matchers: ["mac:a4:cf:12", "vendor:espressif", "mac:84:f3:eb", "mac:3c:2a:f4"],
          dns_servers: "192.168.151.2",
          domain: "iot.lan",
        }),
        dhcpClass({ id: 2, name: "pxe-uefi", matchers: ["vendor:PXEClient:Arch:00007"] }),
      ],
    }),
  );
  await renderClasses();

  await waitFor(() => expect(classRows()).toHaveLength(2));
  const [iot, pxe] = classRows();
  expect(within(iot).getByText("iot")).toBeInTheDocument();
  const matchers = within(iot).getByText("mac:a4:cf:12, vendor:espressif");
  expect(within(iot).getByText("+2")).toBeInTheDocument();
  expect(matchers.parentElement).toHaveAttribute(
    "title",
    "mac:a4:cf:12, vendor:espressif, mac:84:f3:eb, mac:3c:2a:f4",
  );
  expect(within(iot).getByText("DNS 192.168.151.2 · suffix iot.lan")).toBeInTheDocument();
  expect(within(iot).getByText("3 pools")).toBeInTheDocument();

  expect(within(pxe).getByText("inherits scope")).toHaveClass("text-muted-foreground");
  expect(within(pxe).getByText("0 pools")).toHaveClass("text-muted-foreground");
});

test("the POOLS column shows a dash, not 0 pools, while the scopes have not loaded", async () => {
  server.use(...dhcpHandlers({ classes: [dhcpClass()] }));
  // A later use() wins over the handlers above.
  server.use(
    http.get("/api/v1/dhcp/scopes", () => HttpResponse.json({ error: "down" }, { status: 503 })),
  );
  await renderClasses();

  const row = await firstRow();
  expect(within(row).getByText("—")).toHaveClass("text-muted-foreground");
  expect(within(row).queryByText(/pools?$/)).not.toBeInTheDocument();
});

test("no classes says so and offers the first one", async () => {
  server.use(...dhcpHandlers({ classes: [] }));
  await renderClasses();

  expect(await screen.findByText("No classes yet")).toBeInTheDocument();
  expect(
    screen.getByText(
      "A class matches clients by vendor or MAC prefix and gives them their own options.",
    ),
  ).toBeInTheDocument();
  expect(screen.getAllByRole("button", { name: /new class/i })).toHaveLength(2);
});

test("a bad mac prefix is refused on its row, and Save posts kind:value strings", async () => {
  let body: unknown;
  server.use(
    ...dhcpHandlers({ classes: [] }),
    http.post("/api/v1/dhcp/classes", async ({ request }) => {
      body = await request.json();
      return HttpResponse.json(dhcpClass(), { status: 201 });
    }),
  );
  await renderClasses();

  await userEvent.click((await screen.findAllByRole("button", { name: /new class/i }))[0]);
  const dialog = await screen.findByRole("dialog");
  expect(within(dialog).getByText("New class")).toBeInTheDocument();
  expect(within(dialog).getByRole("tab", { name: "Matchers" })).toHaveAttribute(
    "aria-selected",
    "true",
  );

  await userEvent.type(within(dialog).getByLabelText("Name"), "iot");
  const value1 = within(dialog).getByLabelText("Matcher 1 value");
  expect(value1).toHaveAttribute("placeholder", "PXEClient:Arch:00007");
  await userEvent.type(value1, "espressif");

  await userEvent.click(within(dialog).getByRole("button", { name: /add matcher/i }));
  await userEvent.selectOptions(within(dialog).getByLabelText("Matcher 2 kind"), "mac");
  const value2 = within(dialog).getByLabelText("Matcher 2 value");
  expect(value2).toHaveAttribute("placeholder", "a4:cf:12");
  await userEvent.type(value2, "84:F3:E");

  const alert = within(dialog).getByRole("alert");
  expect(alert.id).not.toBe("");
  expect(alert).toHaveTextContent("Not a MAC prefix: 84:F3:E");
  expect(value2).toHaveAttribute("aria-invalid", "true");
  expect(value2).toHaveAttribute("aria-describedby", alert.id);
  expect(within(dialog).getByLabelText("Matcher 2 kind")).toHaveAttribute(
    "aria-describedby",
    alert.id,
  );
  expect(value1).not.toHaveAttribute("aria-describedby");
  expect(within(dialog).getByRole("button", { name: "Save" })).toBeDisabled();

  await userEvent.type(value2, "B");
  expect(within(dialog).queryByRole("alert")).not.toBeInTheDocument();

  await userEvent.click(within(dialog).getByRole("tab", { name: "Client options" }));
  const dns = await within(dialog).findByLabelText("DNS servers");
  expect(dns).toHaveAttribute("placeholder", "inherit");
  expect(within(dialog).getByLabelText("NTP servers")).toHaveAttribute("placeholder", "inherit");
  expect(within(dialog).queryByRole("switch")).not.toBeInTheDocument();
  await userEvent.type(dns, "192.168.151.2");

  await userEvent.click(within(dialog).getByRole("button", { name: "Save" }));
  await waitFor(() => expect(body).toBeDefined());
  expect(body).toEqual({
    name: "iot",
    matchers: ["vendor:espressif", "mac:84:f3:eb"],
    dns_servers: "192.168.151.2",
    domain: "",
    domain_search: "",
    ntp_servers: "",
    static_routes: [],
    next_server: "",
    server_hostname: "",
    boot_file: "",
    options: [],
  });
});

test("a class needs a matcher", async () => {
  server.use(...dhcpHandlers({ classes: [] }));
  await renderClasses();

  await userEvent.click((await screen.findAllByRole("button", { name: /new class/i }))[0]);
  const dialog = await screen.findByRole("dialog");
  await userEvent.type(within(dialog).getByLabelText("Name"), "iot");
  await userEvent.click(within(dialog).getByRole("button", { name: "Save" }));
  expect(await within(dialog).findByText("Add a matcher")).toBeInTheDocument();
});

test("Edit prefills the class, splits its matchers, and a refusal lands on its row", async () => {
  let body: unknown;
  server.use(
    ...dhcpHandlers({
      classes: [
        dhcpClass({ matchers: ["vendor:PXEClient:Arch:00007", "mac:a4:cf:12"], domain: "iot.lan" }),
      ],
    }),
    http.patch("/api/v1/dhcp/classes/1", async ({ request }) => {
      body = await request.json();
      return HttpResponse.json(
        { error: 'matchers[0]: vendor prefix "it\'s" has a single quote' },
        { status: 400 },
      );
    }),
  );
  await renderClasses();

  const row = await firstRow();
  await userEvent.click(within(row).getByRole("button", { name: "Edit" }));
  const dialog = await screen.findByRole("dialog");
  expect(within(dialog).getByText("Edit class · iot")).toBeInTheDocument();
  expect(within(dialog).getByLabelText("Name")).toHaveValue("iot");
  expect(within(dialog).getByLabelText("Matcher 1 kind")).toHaveValue("vendor");
  expect(within(dialog).getByLabelText("Matcher 1 value")).toHaveValue("PXEClient:Arch:00007");
  expect(within(dialog).getByLabelText("Matcher 2 kind")).toHaveValue("mac");
  expect(within(dialog).getByLabelText("Matcher 2 value")).toHaveValue("a4:cf:12");

  await userEvent.clear(within(dialog).getByLabelText("Matcher 1 value"));
  await userEvent.type(within(dialog).getByLabelText("Matcher 1 value"), "it's");
  await userEvent.click(within(dialog).getByRole("tab", { name: "Client options" }));
  expect(await within(dialog).findByLabelText("DNS suffix")).toHaveValue("iot.lan");
  await userEvent.click(within(dialog).getByRole("button", { name: "Save" }));

  await waitFor(() => expect(body).toBeDefined());
  expect(body).toMatchObject({ name: "iot", matchers: ["vendor:it's", "mac:a4:cf:12"] });
  // The refusal names matchers[0]: it pulls Matchers into view, on that row.
  expect(await within(dialog).findByRole("alert")).toHaveTextContent(
    'matchers[0]: vendor prefix "it\'s" has a single quote',
  );
  expect(within(dialog).getByRole("tab", { name: "Matchers" })).toHaveAttribute(
    "aria-selected",
    "true",
  );
});

test("Delete of a class pools name says where, and refuses", async () => {
  server.use(...dhcpHandlers({ scopes: inUseScopes }));
  await renderClasses();

  const row = await firstRow();
  await userEvent.click(within(row).getByRole("button", { name: "Delete" }));
  const dialog = await screen.findByRole("alertdialog");
  expect(within(dialog).getByText("Delete class · iot")).toBeInTheDocument();
  expect(
    within(dialog).getByText(
      "In use by 3 pools in Office and IoT. Remove it from those pools first.",
    ),
  ).toBeInTheDocument();
  expect(within(dialog).getByRole("button", { name: "Delete" })).toBeDisabled();
});

test("Delete of an unused class confirms, and the server's 409 is the backstop", async () => {
  let calls = 0;
  server.use(
    ...dhcpHandlers(),
    http.delete("/api/v1/dhcp/classes/1", () => {
      calls++;
      return HttpResponse.json(
        { error: 'class "iot" is in use by 2 pools (Office, IoT)' },
        { status: 409 },
      );
    }),
  );
  await renderClasses();

  const row = await firstRow();
  await userEvent.click(within(row).getByRole("button", { name: "Delete" }));
  const dialog = await screen.findByRole("alertdialog");
  expect(within(dialog).getByText("Delete this class?")).toBeInTheDocument();
  await userEvent.click(within(dialog).getByRole("button", { name: "Delete" }));

  await waitFor(() => expect(calls).toBe(1));
  expect(
    await within(dialog).findByText('class "iot" is in use by 2 pools (Office, IoT)'),
  ).toBeInTheDocument();
  expect(within(dialog).getByRole("button", { name: "Delete" })).toBeDisabled();
});

test("a replica states whose configuration this is and offers no write", async () => {
  server.use(...dhcpHandlers({ sync: { role: "replica", peer_url: "https://main.lan" } }));
  await renderClasses();

  expect(await screen.findByText("Managed by the main")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: /new class/i })).toBeDisabled();
  const row = await firstRow();
  expect(within(row).getByRole("button", { name: "Edit" })).toBeDisabled();
  expect(within(row).getByRole("button", { name: "Delete" })).toBeDisabled();
});
