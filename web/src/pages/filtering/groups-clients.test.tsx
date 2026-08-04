import { http, HttpResponse } from "msw";
import { toast } from "sonner";
import { expect, test, vi } from "vitest";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { server } from "../../test/msw-server";
import { renderWithProviders } from "../../test/render";
import type { Client, Group, List } from "../../api/types";
import { GroupsClientsTab } from "./groups-clients";

function group(overrides: Partial<Group> = {}): Group {
  return { id: 1, name: "Default", enabled: true, ...overrides };
}

function client(overrides: Partial<Client> = {}): Client {
  return { id: 1, name: "Kid's laptop", matcher: "192.168.1.42", group_id: 1, ...overrides };
}

function list(overrides: Partial<List> = {}): List {
  return {
    id: 1,
    url: "https://example.com/hosts",
    kind: "block",
    enabled: true,
    last_refreshed: Date.now(),
    entry_count: 100,
    ...overrides,
  };
}

function mockGroups(groups: Group[]) {
  server.use(http.get("/api/v1/groups", () => HttpResponse.json(groups)));
}

function mockClients(clients: Client[]) {
  server.use(http.get("/api/v1/clients", () => HttpResponse.json(clients)));
}

// Every group row mounts a PauseControl (GET /blocking — covered by the
// global default handler) and a GroupListsMenu (GET /groups/{id}/lists), so
// most tests need a blanket list-catalog + group-lists fixture even when
// they don't care about list assignment themselves.
function mockNoLists() {
  server.use(
    http.get("/api/v1/filters/lists", () => HttpResponse.json([])),
    http.get("/api/v1/groups/:id/lists", () => HttpResponse.json([])),
  );
}

// rnui's DropdownMenu (the per-group "Lists" menu) is base-ui Menu-driven —
// under jsdom, userEvent's full pointerdown→click sequence trips base-ui's
// outside-click detection and immediately re-closes it. A plain
// fireEvent.click reliably opens and keeps it open; see
// pause-control.test.tsx and pages/queries.test.tsx for the same
// workaround with this exact component.
function openMenu(button: HTMLElement) {
  fireEvent.click(button);
  const menu = document.querySelector('[data-slot="dropdown-menu-content"]');
  if (!menu) throw new Error("menu did not open");
  return menu as HTMLElement;
}

// --- Groups panel ----------------------------------------------------------

test("renders groups with the default group badged", async () => {
  mockGroups([group({ id: 1, name: "Default" }), group({ id: 2, name: "Kids", enabled: false })]);
  mockClients([]);
  mockNoLists();

  renderWithProviders(<GroupsClientsTab />);

  expect(await screen.findByText("Default")).toBeInTheDocument();
  expect(screen.getByText("Kids")).toBeInTheDocument();
  expect(screen.getByText("Enforcing")).toBeInTheDocument();
  expect(screen.getByText("Disabled")).toBeInTheDocument();
});

test("the default group's delete control is disabled and blocks deletion", async () => {
  let deleteCalled = false;
  mockGroups([group({ id: 1, name: "Default" })]);
  mockClients([]);
  mockNoLists();
  server.use(
    http.delete("/api/v1/groups/1", () => {
      deleteCalled = true;
      return new HttpResponse(null, { status: 204 });
    }),
  );

  renderWithProviders(<GroupsClientsTab />);
  await screen.findByText("Default");

  const deleteBtn = screen.getByRole("button", { name: /^delete default$/i });
  expect(deleteBtn).toBeDisabled();

  await userEvent.setup().click(deleteBtn);
  expect(screen.queryByRole("alertdialog")).not.toBeInTheDocument();
  expect(deleteCalled).toBe(false);
});

test("deleting a non-default group asks for confirmation, then DELETEs /groups/{id}", async () => {
  const user = userEvent.setup();
  let deleted = false;
  mockGroups([group({ id: 1, name: "Default" }), group({ id: 2, name: "Kids" })]);
  mockClients([]);
  mockNoLists();
  server.use(
    http.delete("/api/v1/groups/2", () => {
      deleted = true;
      return new HttpResponse(null, { status: 204 });
    }),
  );

  renderWithProviders(<GroupsClientsTab />);
  await screen.findByText("Kids");

  await user.click(screen.getByRole("button", { name: /^delete kids$/i }));
  const confirmDialog = await screen.findByRole("alertdialog");
  await user.click(within(confirmDialog).getByRole("button", { name: /^delete$/i }));

  await waitFor(() => expect(deleted).toBe(true));
});

test("a 409 (group in use) shows a friendly toast instead of a raw error", async () => {
  const user = userEvent.setup();
  mockGroups([group({ id: 1, name: "Default" }), group({ id: 2, name: "Kids" })]);
  mockClients([]);
  mockNoLists();
  server.use(
    http.delete("/api/v1/groups/2", () =>
      HttpResponse.json({ error: "resource in use" }, { status: 409 }),
    ),
  );
  const errorSpy = vi.spyOn(toast, "error");

  renderWithProviders(<GroupsClientsTab />);
  await screen.findByText("Kids");

  await user.click(screen.getByRole("button", { name: /^delete kids$/i }));
  const confirmDialog = await screen.findByRole("alertdialog");
  await user.click(within(confirmDialog).getByRole("button", { name: /^delete$/i }));

  await waitFor(() => expect(errorSpy).toHaveBeenCalledWith(expect.stringMatching(/in use/i)));
});

test("toggling a group's enabled switch PATCHes /groups/{id}", async () => {
  const user = userEvent.setup();
  let requestBody: unknown;
  mockGroups([group({ id: 1, name: "Default", enabled: true })]);
  mockClients([]);
  mockNoLists();
  server.use(
    http.patch("/api/v1/groups/1", async ({ request }) => {
      requestBody = await request.json();
      return new HttpResponse(null, { status: 204 });
    }),
  );

  renderWithProviders(<GroupsClientsTab />);
  await screen.findByText("Default");

  await user.click(screen.getByRole("switch", { name: /^disable default$/i }));

  await waitFor(() => expect(requestBody).toEqual({ enabled: false }));
});

test("renaming a group PATCHes /groups/{id} with the new name", async () => {
  const user = userEvent.setup();
  let requestBody: unknown;
  mockGroups([group({ id: 1, name: "Default" })]);
  mockClients([]);
  mockNoLists();
  server.use(
    http.patch("/api/v1/groups/1", async ({ request }) => {
      requestBody = await request.json();
      return new HttpResponse(null, { status: 204 });
    }),
  );

  renderWithProviders(<GroupsClientsTab />);
  await screen.findByText("Default");

  await user.click(screen.getByRole("button", { name: /^rename default$/i }));
  const dialog = await screen.findByRole("dialog");
  const nameInput = within(dialog).getByLabelText(/^name$/i);
  await user.clear(nameInput);
  await user.type(nameInput, "Household");
  await user.click(within(dialog).getByRole("button", { name: /^save$/i }));

  await waitFor(() => expect(requestBody).toEqual({ name: "Household" }));
});

test("adding a group POSTs /groups with the trimmed name", async () => {
  const user = userEvent.setup();
  let requestBody: unknown;
  mockGroups([group({ id: 1, name: "Default" })]);
  mockClients([]);
  mockNoLists();
  server.use(
    http.post("/api/v1/groups", async ({ request }) => {
      requestBody = await request.json();
      return HttpResponse.json({ id: 2 }, { status: 201 });
    }),
  );

  renderWithProviders(<GroupsClientsTab />);
  await screen.findByText("Default");

  await user.click(screen.getByRole("button", { name: /^new group$/i }));
  const dialog = await screen.findByRole("dialog");
  await user.type(within(dialog).getByLabelText(/^name$/i), "  Guests  ");
  await user.click(within(dialog).getByRole("button", { name: /^add group$/i }));

  await waitFor(() => expect(requestBody).toEqual({ name: "Guests" }));
});

test("toggling a list in a group's Lists menu PUTs the full assigned-list id array", async () => {
  let requestBody: unknown;
  mockGroups([group({ id: 1, name: "Default" })]);
  mockClients([]);
  server.use(
    http.get("/api/v1/filters/lists", () =>
      HttpResponse.json([
        list({ id: 1, url: "https://example.com/hosts" }),
        list({ id: 2, url: "https://example.com/allow" }),
      ]),
    ),
    http.get("/api/v1/groups/1/lists", () =>
      HttpResponse.json([list({ id: 1, url: "https://example.com/hosts" })]),
    ),
    http.put("/api/v1/groups/1/lists", async ({ request }) => {
      requestBody = await request.json();
      return new HttpResponse(null, { status: 204 });
    }),
  );

  renderWithProviders(<GroupsClientsTab />);
  await screen.findByText("Default");

  const listsButton = await screen.findByRole("button", { name: /^lists \(1\)$/i });
  const menu = openMenu(listsButton);
  fireEvent.click(within(menu).getByText("https://example.com/allow"));

  await waitFor(() => expect(requestBody).toEqual({ list_ids: [1, 2] }));
});

test("no groups shows EmptyState with a New group action", async () => {
  mockGroups([]);
  mockClients([]);
  server.use(http.get("/api/v1/filters/lists", () => HttpResponse.json([])));

  renderWithProviders(<GroupsClientsTab />);

  expect(await screen.findByText("No groups yet")).toBeInTheDocument();
});

// --- Clients panel -----------------------------------------------------------

test("renders the clients table with name, matcher, and group", async () => {
  mockGroups([group({ id: 1, name: "Default" })]);
  mockClients([client()]);
  mockNoLists();

  renderWithProviders(<GroupsClientsTab />);

  expect(await screen.findByText("Kid's laptop")).toBeInTheDocument();
  expect(screen.getByText("192.168.1.42")).toBeInTheDocument();
  const table = screen.getByRole("table");
  expect(within(table).getByText("Default")).toBeInTheDocument();
});

test("adding a client posts /clients with the validated payload", async () => {
  const user = userEvent.setup();
  let requestBody: unknown;
  mockGroups([group({ id: 1, name: "Default" }), group({ id: 2, name: "Kids" })]);
  mockClients([]);
  mockNoLists();
  server.use(
    http.post("/api/v1/clients", async ({ request }) => {
      requestBody = await request.json();
      return HttpResponse.json({ id: 2 }, { status: 201 });
    }),
  );

  renderWithProviders(<GroupsClientsTab />);
  await screen.findByText("No clients yet");

  await user.click(screen.getByRole("button", { name: /^add client$/i }));
  const dialog = await screen.findByRole("dialog");
  await user.type(within(dialog).getByLabelText(/^name$/i), "New device");
  await user.type(within(dialog).getByLabelText(/^matcher$/i), "  10.0.0.5  ");
  await user.click(within(dialog).getByRole("combobox", { name: /^group$/i }));
  await user.click(await screen.findByRole("option", { name: /^kids$/i }));
  await user.click(within(dialog).getByRole("button", { name: /^add client$/i }));

  await waitFor(() =>
    expect(requestBody).toEqual({ name: "New device", matcher: "10.0.0.5", group_id: 2 }),
  );
});

test("a client with an invalid matcher shows inline error and never posts", async () => {
  const user = userEvent.setup();
  let posted = false;
  mockGroups([group({ id: 1, name: "Default" })]);
  mockClients([client()]);
  mockNoLists();
  server.use(
    http.post("/api/v1/clients", () => {
      posted = true;
      return HttpResponse.json({ id: 99 }, { status: 201 });
    }),
  );

  renderWithProviders(<GroupsClientsTab />);
  await screen.findByText("Kid's laptop");

  await user.click(screen.getByRole("button", { name: /^add client$/i }));
  const dialog = await screen.findByRole("dialog");
  await user.type(within(dialog).getByLabelText(/^name$/i), "Bad device");
  await user.type(within(dialog).getByLabelText(/^matcher$/i), "not-an-ip");
  await user.click(within(dialog).getByRole("button", { name: /^add client$/i }));

  expect(await within(dialog).findByText(/enter a valid ip address/i)).toBeInTheDocument();
  // Give any accidental async POST a chance to land before asserting it didn't.
  await new Promise((resolve) => setTimeout(resolve, 30));
  expect(posted).toBe(false);
  expect(screen.getByRole("dialog")).toBeInTheDocument();
});

test("an out-of-range CIDR prefix shows inline error and never posts", async () => {
  const user = userEvent.setup();
  let posted = false;
  mockGroups([group({ id: 1, name: "Default" })]);
  mockClients([client()]);
  mockNoLists();
  server.use(
    http.post("/api/v1/clients", () => {
      posted = true;
      return HttpResponse.json({ id: 99 }, { status: 201 });
    }),
  );

  renderWithProviders(<GroupsClientsTab />);
  await screen.findByText("Kid's laptop");

  await user.click(screen.getByRole("button", { name: /^add client$/i }));
  const dialog = await screen.findByRole("dialog");
  await user.type(within(dialog).getByLabelText(/^name$/i), "Bad range");
  await user.type(within(dialog).getByLabelText(/^matcher$/i), "192.168.1.0/99");
  await user.click(within(dialog).getByRole("button", { name: /^add client$/i }));

  expect(await within(dialog).findByText(/cidr prefix must be between/i)).toBeInTheDocument();
  await new Promise((resolve) => setTimeout(resolve, 30));
  expect(posted).toBe(false);
});

test("editing a client PUTs /clients/{id} with the updated fields", async () => {
  const user = userEvent.setup();
  let requestBody: unknown;
  mockGroups([group({ id: 1, name: "Default" }), group({ id: 2, name: "Kids" })]);
  mockClients([client({ id: 1, name: "Kid's laptop", matcher: "192.168.1.42", group_id: 1 })]);
  mockNoLists();
  server.use(
    http.put("/api/v1/clients/1", async ({ request }) => {
      requestBody = await request.json();
      return new HttpResponse(null, { status: 204 });
    }),
  );

  renderWithProviders(<GroupsClientsTab />);
  await screen.findByText("Kid's laptop");

  await user.click(screen.getByRole("button", { name: /^edit kid's laptop$/i }));
  const dialog = await screen.findByRole("dialog");
  const matcherInput = within(dialog).getByLabelText(/^matcher$/i);
  await user.clear(matcherInput);
  await user.type(matcherInput, "192.168.1.99");
  await user.click(within(dialog).getByRole("button", { name: /^save$/i }));

  await waitFor(() =>
    expect(requestBody).toEqual({ name: "Kid's laptop", matcher: "192.168.1.99", group_id: 1 }),
  );
});

test("deleting a client asks for confirmation, then DELETEs /clients/{id}", async () => {
  const user = userEvent.setup();
  let deleted = false;
  mockGroups([group({ id: 1, name: "Default" })]);
  mockClients([client()]);
  mockNoLists();
  server.use(
    http.delete("/api/v1/clients/1", () => {
      deleted = true;
      return new HttpResponse(null, { status: 204 });
    }),
  );

  renderWithProviders(<GroupsClientsTab />);
  await screen.findByText("Kid's laptop");

  await user.click(screen.getByRole("button", { name: /^delete kid's laptop$/i }));
  const confirmDialog = await screen.findByRole("alertdialog");
  await user.click(within(confirmDialog).getByRole("button", { name: /^delete$/i }));

  await waitFor(() => expect(deleted).toBe(true));
});

test("no clients shows EmptyState, and Add client is disabled with no groups", async () => {
  mockGroups([]);
  mockClients([]);
  server.use(http.get("/api/v1/filters/lists", () => HttpResponse.json([])));

  renderWithProviders(<GroupsClientsTab />);

  expect(await screen.findByText("No clients yet")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: /^add client$/i })).toBeDisabled();
});
