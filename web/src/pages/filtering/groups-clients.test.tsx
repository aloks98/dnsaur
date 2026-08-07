import { http, HttpResponse } from "msw";
import { toast } from "sonner";
import { afterEach, expect, test, vi } from "vitest";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { server } from "../../test/msw-server";
import { renderWithProviders } from "../../test/render";
import type { Client, Group, List } from "../../api/types";
import { GroupsClientsTab } from "./groups-clients";

function group(overrides: Partial<Group> = {}): Group {
  return { id: 1, name: "default", enabled: true, ...overrides };
}

function client(overrides: Partial<Client> = {}): Client {
  return { id: 1, name: "Laptop", matcher: "192.168.1.10", group_id: 1, ...overrides };
}

function list(overrides: Partial<List> = {}): List {
  return {
    id: 1,
    url: "https://example.com/hosts",
    name: "example hosts",
    kind: "block",
    enabled: true,
    last_refreshed: 0,
    entry_count: 0,
    last_status: "ok",
    last_error: "",
    last_attempt: 0,
    ...overrides,
  };
}

interface MockOpts {
  groups?: Group[];
  clients?: Client[];
  lists?: List[];
  /** Makes GET /groups/{id}/lists fail — the case that used to wipe assignments. */
  groupListsFail?: boolean;
  assigned?: List[];
}

function mockAll({
  groups = [group()],
  clients = [],
  lists = [],
  groupListsFail = false,
  assigned = [],
}: MockOpts = {}) {
  server.use(
    http.get("/api/v1/groups", () => HttpResponse.json(groups)),
    http.get("/api/v1/clients", () => HttpResponse.json(clients)),
    http.get("/api/v1/filters/lists", () => HttpResponse.json(lists)),
    http.get("/api/v1/groups/:id/lists", () =>
      groupListsFail
        ? HttpResponse.json({ error: "boom" }, { status: 500 })
        : HttpResponse.json(assigned),
    ),
  );
}

function groupRows(): HTMLElement[] {
  return Array.from(document.querySelectorAll<HTMLElement>('[data-slot="group-row"]'));
}
function clientRows(): HTMLElement[] {
  return Array.from(document.querySelectorAll<HTMLElement>('[data-slot="client-row"]'));
}

afterEach(() => vi.restoreAllMocks());

// --- groups ---------------------------------------------------------------

test("renders a row per group with its client count", async () => {
  mockAll({
    groups: [group({ id: 1, name: "default" }), group({ id: 2, name: "Kids" })],
    clients: [client({ id: 1, group_id: 1 }), client({ id: 2, matcher: "10.0.0.2", group_id: 2 })],
  });

  renderWithProviders(<GroupsClientsTab />);
  await waitFor(() => expect(groupRows()).toHaveLength(2));

  const [first, second] = groupRows();
  expect(within(first).getByText("default")).toBeInTheDocument();
  expect(within(first).getByText("1")).toBeInTheDocument();
  expect(within(second).getByText("Kids")).toBeInTheDocument();
});

// The seeded group is structural — the server refuses to delete it, so the
// row says so rather than letting the click round trip to a 409.
test("the default group cannot be deleted, and says why", async () => {
  mockAll({ groups: [group({ id: 1, name: "default" })] });

  renderWithProviders(<GroupsClientsTab />);
  await waitFor(() => expect(groupRows()).toHaveLength(1));

  expect(screen.getByRole("button", { name: /delete default/i })).toBeDisabled();
  expect(screen.getByText(/fallback group can't be deleted/i)).toBeInTheDocument();
});

// A disabled group compiles no ruleset at all, so nothing is blocked for
// any of its clients. That is a state to notice, not a dimmed row.
test("a disabled group says nothing is being filtered for its clients", async () => {
  mockAll({
    groups: [group({ id: 2, name: "IoT", enabled: false })],
    clients: [client({ id: 1, group_id: 2 }), client({ id: 2, matcher: "10.0.0.9", group_id: 2 })],
  });

  renderWithProviders(<GroupsClientsTab />);
  await waitFor(() => expect(groupRows()).toHaveLength(1));

  expect(screen.getByText(/not filtering/i)).toBeInTheDocument();
  expect(screen.getByText(/nothing is blocked for its 2 clients/i)).toBeInTheDocument();
});

test("toggling a group's switch PATCHes /groups/{id}", async () => {
  const user = userEvent.setup();
  let body: unknown;
  mockAll({ groups: [group({ id: 2, name: "Kids" })] });
  server.use(
    http.patch("/api/v1/groups/2", async ({ request }) => {
      body = await request.json();
      return new HttpResponse(null, { status: 204 });
    }),
  );

  renderWithProviders(<GroupsClientsTab />);
  await waitFor(() => expect(groupRows()).toHaveLength(1));

  await user.click(screen.getByRole("switch", { name: /disable kids/i }));
  await waitFor(() => expect(body).toEqual({ enabled: false }));
});

test("adding a group POSTs /groups with the trimmed name", async () => {
  const user = userEvent.setup();
  let body: unknown;
  mockAll();
  server.use(
    http.post("/api/v1/groups", async ({ request }) => {
      body = await request.json();
      return HttpResponse.json({ id: 9 }, { status: 201 });
    }),
  );

  renderWithProviders(<GroupsClientsTab />);
  await waitFor(() => expect(groupRows()).toHaveLength(1));

  await user.click(screen.getByRole("button", { name: /add group/i }));
  await user.type(await screen.findByLabelText(/group name/i), "  Office  ");
  // Scoped: the client row below has its own Add.
  const addGroupRow = document.querySelector<HTMLElement>('[data-slot="add-group-row"]')!;
  await user.click(within(addGroupRow).getByRole("button", { name: /^add$/i }));

  // No list_ids: untouched means "inherit every list", and that is the
  // server's job to resolve — sending a snapshot from this page would go
  // stale if a list were added in between.
  await waitFor(() => expect(body).toEqual({ name: "Office", enabled: true }));
});

// The row's switch and list picker are real now: POST /groups takes both.
test("the add-group row can create a disabled group with chosen lists", async () => {
  const user = userEvent.setup();
  let body: unknown;
  mockAll({ lists: [list({ id: 1, name: "A" }), list({ id: 2, name: "B" })] });
  server.use(
    http.post("/api/v1/groups", async ({ request }) => {
      body = await request.json();
      return HttpResponse.json({ id: 9 }, { status: 201 });
    }),
  );

  renderWithProviders(<GroupsClientsTab />);
  await waitFor(() => expect(groupRows()).toHaveLength(1));

  await user.click(screen.getByRole("button", { name: /add group/i }));
  const row = document.querySelector<HTMLElement>('[data-slot="add-group-row"]')!;
  await user.type(await screen.findByLabelText(/group name/i), "Office");
  await user.click(within(row).getByRole("switch", { name: /enabled/i }));

  // Starts holding every list, so unticking one leaves the other.
  fireEvent.click(within(row).getByRole("button", { name: /lists \(2\)/i }));
  fireEvent.click(await screen.findByRole("menuitemcheckbox", { name: "A" }));

  await user.click(within(row).getByRole("button", { name: /^add$/i }));

  await waitFor(() => expect(body).toEqual({ name: "Office", enabled: false, list_ids: [2] }));
});

test("renaming a group PATCHes /groups/{id}", async () => {
  const user = userEvent.setup();
  let body: unknown;
  mockAll({ groups: [group({ id: 2, name: "Kids" })] });
  server.use(
    http.patch("/api/v1/groups/2", async ({ request }) => {
      body = await request.json();
      return new HttpResponse(null, { status: 204 });
    }),
  );

  renderWithProviders(<GroupsClientsTab />);
  await waitFor(() => expect(groupRows()).toHaveLength(1));

  await user.click(screen.getByRole("button", { name: /rename kids/i }));
  const dialog = await screen.findByRole("dialog");
  const field = within(dialog).getByLabelText(/^name$/i);
  await user.clear(field);
  await user.type(field, "Children");
  await user.click(within(dialog).getByRole("button", { name: /^save$/i }));

  await waitFor(() => expect(body).toEqual({ name: "Children" }));
});

test("deleting a non-default group asks for confirmation, then DELETEs it", async () => {
  const user = userEvent.setup();
  let deleted = false;
  mockAll({ groups: [group({ id: 2, name: "Kids" })] });
  server.use(
    http.delete("/api/v1/groups/2", () => {
      deleted = true;
      return new HttpResponse(null, { status: 204 });
    }),
  );

  renderWithProviders(<GroupsClientsTab />);
  await waitFor(() => expect(groupRows()).toHaveLength(1));

  await user.click(screen.getByRole("button", { name: /delete kids/i }));
  const confirm = await screen.findByRole("alertdialog");
  await user.click(within(confirm).getByRole("button", { name: /^delete$/i }));

  await waitFor(() => expect(deleted).toBe(true));
});

test("a 409 on group delete shows a friendly toast, not the raw error", async () => {
  const user = userEvent.setup();
  mockAll({ groups: [group({ id: 2, name: "Kids" })] });
  server.use(
    http.delete("/api/v1/groups/2", () =>
      HttpResponse.json({ error: "resource in use" }, { status: 409 }),
    ),
  );
  const errorSpy = vi.spyOn(toast, "error");

  renderWithProviders(<GroupsClientsTab />);
  await waitFor(() => expect(groupRows()).toHaveLength(1));

  await user.click(screen.getByRole("button", { name: /delete kids/i }));
  const confirm = await screen.findByRole("alertdialog");
  await user.click(within(confirm).getByRole("button", { name: /^delete$/i }));

  await waitFor(() =>
    expect(errorSpy).toHaveBeenCalledWith(expect.stringMatching(/still in use/i)),
  );
});

// --- the assignment-wipe bug ---------------------------------------------

test("toggling a list PUTs the whole assigned set, not just the one clicked", async () => {
  let body: unknown;
  mockAll({
    groups: [group({ id: 1, name: "default" })],
    lists: [list({ id: 1, name: "A" }), list({ id: 2, name: "B" })],
    assigned: [list({ id: 1, name: "A" })],
  });
  server.use(
    http.put("/api/v1/groups/1/lists", async ({ request }) => {
      body = await request.json();
      return new HttpResponse(null, { status: 204 });
    }),
  );

  renderWithProviders(<GroupsClientsTab />);
  await waitFor(() => expect(groupRows()).toHaveLength(1));

  // fireEvent, not userEvent: base-ui's Menu re-closes on userEvent's full
  // pointerdown→click sequence under jsdom (no geometry to hit-test).
  fireEvent.click(await screen.findByRole("button", { name: /lists \(1\)/i }));
  fireEvent.click(await screen.findByRole("menuitemcheckbox", { name: "B" }));

  // Both ids, because A was already assigned and must survive adding B.
  await waitFor(() => expect(body).toEqual({ list_ids: [1, 2] }));
});

// The bug this screen shipped with. The toggle computes the next set from
// this query's data; when the query has *failed* that set is empty, so one
// click PUT `[thatOne]` and dropped every other assignment. The control was
// disabled while pending but not while errored, so the click was reachable.
test("a failed lists read offers a retry and makes no write reachable", async () => {
  const user = userEvent.setup();
  let put = false;
  mockAll({
    groups: [group({ id: 1, name: "default" })],
    lists: [list({ id: 1, name: "A" }), list({ id: 2, name: "B" })],
    groupListsFail: true,
  });
  server.use(
    http.put("/api/v1/groups/1/lists", () => {
      put = true;
      return new HttpResponse(null, { status: 204 });
    }),
  );

  renderWithProviders(<GroupsClientsTab />);

  // The assignment control is replaced, so there is nothing that could
  // write a truncated set.
  // query-client.ts sets retry: 1, so the failure is only final after a
  // retry and its backoff.
  const retry = await screen.findByRole("button", { name: /can't load/i }, { timeout: 5000 });
  expect(screen.queryByRole("button", { name: /^lists/i })).not.toBeInTheDocument();

  await user.click(retry);
  await new Promise((resolve) => setTimeout(resolve, 50));
  expect(put).toBe(false);
});

// --- clients --------------------------------------------------------------

test("renders clients with name, matcher and group, and marks unnamed ones", async () => {
  mockAll({
    groups: [group({ id: 1, name: "default" })],
    clients: [
      client({ id: 1, name: "Laptop", matcher: "192.168.1.10" }),
      client({ id: 2, name: "", matcher: "192.168.1.11" }),
    ],
  });

  renderWithProviders(<GroupsClientsTab />);
  await waitFor(() => expect(clientRows()).toHaveLength(2));

  const [first, second] = clientRows();
  expect(within(first).getByText("Laptop")).toBeInTheDocument();
  expect(within(first).getByText("192.168.1.10")).toBeInTheDocument();
  // The server allows a blank name, so the row has to render something.
  expect(within(second).getByText(/unnamed device/i)).toBeInTheDocument();
});

test("selecting a group filters the client list, and Show all clears it", async () => {
  const user = userEvent.setup();
  mockAll({
    groups: [group({ id: 1, name: "default" }), group({ id: 2, name: "Kids" })],
    clients: [
      client({ id: 1, name: "Laptop", group_id: 1 }),
      client({ id: 2, name: "Tablet", matcher: "10.0.0.2", group_id: 2 }),
    ],
  });

  renderWithProviders(<GroupsClientsTab />);
  await waitFor(() => expect(clientRows()).toHaveLength(2));

  await user.click(screen.getByRole("button", { name: /^kids$/i }));
  await waitFor(() => expect(clientRows()).toHaveLength(1));
  expect(screen.getByText("Tablet")).toBeInTheDocument();

  await user.click(screen.getByRole("button", { name: /show all/i }));
  await waitFor(() => expect(clientRows()).toHaveLength(2));
});

test("adding a client posts /clients with the validated payload", async () => {
  const user = userEvent.setup();
  let body: unknown;
  mockAll({ groups: [group({ id: 1, name: "default" })] });
  server.use(
    http.post("/api/v1/clients", async ({ request }) => {
      body = await request.json();
      return HttpResponse.json({ id: 5 }, { status: 201 });
    }),
  );

  renderWithProviders(<GroupsClientsTab />);
  await waitFor(() => expect(groupRows()).toHaveLength(1));

  await openAddClient(user);
  await user.type(screen.getByLabelText(/client name/i), "Laptop");
  await user.type(screen.getByLabelText(/^matcher$/i), "192.168.1.10");
  const row = document.querySelector<HTMLElement>('[data-slot="add-client-row"]')!;
  await user.click(within(row).getByRole("button", { name: /^add$/i }));

  await waitFor(() =>
    expect(body).toEqual({ name: "Laptop", matcher: "192.168.1.10", group_id: 1 }),
  );
});

// The server does not require a client name (internal/store's Client.Name
// is unvalidated), so the form must not invent that constraint.
test("a client can be added with no name", async () => {
  const user = userEvent.setup();
  let body: unknown;
  mockAll({ groups: [group({ id: 1, name: "default" })] });
  server.use(
    http.post("/api/v1/clients", async ({ request }) => {
      body = await request.json();
      return HttpResponse.json({ id: 5 }, { status: 201 });
    }),
  );

  renderWithProviders(<GroupsClientsTab />);
  await waitFor(() => expect(groupRows()).toHaveLength(1));

  await submitMatcher(user, "192.168.1.31");

  await waitFor(() => expect(body).toEqual({ name: "", matcher: "192.168.1.31", group_id: 1 }));
});

/** Matcher cases worth keeping from the original suite: the client
 * validator mirrors Go's netip.ParsePrefix, and each of these was a real
 * disagreement between the two at some point. */
const ACCEPTED_MATCHERS = [
  { name: "a plain IPv6 address", value: "2001:db8::1" },
  { name: "an IPv6 address with a zone id", value: "fe80::1%eth0" },
  { name: "a bare /0 prefix", value: "0.0.0.0/0" },
];

const REJECTED_MATCHERS = [
  { name: "nonsense", value: "not-an-ip", error: /valid ip address or cidr/i },
  { name: "an out-of-range prefix", value: "192.168.1.0/33", error: /between 0 and 32/i },
  {
    name: "a zone id combined with a CIDR range",
    value: "fe80::1%eth0/64",
    error: /zone ids can't be combined/i,
  },
  { name: "a zero-padded prefix", value: "192.168.1.0/024", error: /leading zero/i },
];

/** The client row is behind its own button, like the group one. */
async function openAddClient(user: ReturnType<typeof userEvent.setup>) {
  await user.click(screen.getByRole("button", { name: /add client/i }));
  return screen.findByLabelText(/^matcher$/i);
}

/** Opens the row, types the matcher and submits. */
async function submitMatcher(user: ReturnType<typeof userEvent.setup>, value: string) {
  const matcher = await openAddClient(user);
  await user.type(matcher, value);
  const row = document.querySelector<HTMLElement>('[data-slot="add-client-row"]')!;
  await user.click(within(row).getByRole("button", { name: /^add$/i }));
}

for (const c of ACCEPTED_MATCHERS) {
  test(`${c.name} is accepted as a matcher`, async () => {
    const user = userEvent.setup();
    let posted = false;
    mockAll({ groups: [group({ id: 1, name: "default" })] });
    server.use(
      http.post("/api/v1/clients", () => {
        posted = true;
        return HttpResponse.json({ id: 5 }, { status: 201 });
      }),
    );

    renderWithProviders(<GroupsClientsTab />);
    await waitFor(() => expect(groupRows()).toHaveLength(1));
    await submitMatcher(user, c.value);

    await waitFor(() => expect(posted).toBe(true));
  });
}

for (const c of REJECTED_MATCHERS) {
  test(`${c.name} is rejected as a matcher, and never posts`, async () => {
    const user = userEvent.setup();
    let posted = false;
    mockAll({ groups: [group({ id: 1, name: "default" })] });
    server.use(
      http.post("/api/v1/clients", () => {
        posted = true;
        return HttpResponse.json({ id: 5 }, { status: 201 });
      }),
    );

    renderWithProviders(<GroupsClientsTab />);
    await waitFor(() => expect(groupRows()).toHaveLength(1));
    await submitMatcher(user, c.value);

    expect(await screen.findByText(c.error)).toBeInTheDocument();
    // Give any accidental async POST a chance to land before asserting it didn't.
    await new Promise((resolve) => setTimeout(resolve, 30));
    expect(posted).toBe(false);
  });
}

test("deleting a client asks for confirmation, then DELETEs /clients/{id}", async () => {
  const user = userEvent.setup();
  let deleted = false;
  mockAll({
    groups: [group({ id: 1, name: "default" })],
    clients: [client({ id: 3, name: "Laptop" })],
  });
  server.use(
    http.delete("/api/v1/clients/3", () => {
      deleted = true;
      return new HttpResponse(null, { status: 204 });
    }),
  );

  renderWithProviders(<GroupsClientsTab />);
  await waitFor(() => expect(clientRows()).toHaveLength(1));

  await user.click(screen.getByRole("button", { name: /delete laptop/i }));
  const confirm = await screen.findByRole("alertdialog");
  await user.click(within(confirm).getByRole("button", { name: /^delete$/i }));

  await waitFor(() => expect(deleted).toBe(true));
});

test("no clients says so, and names where unpinned devices land", async () => {
  mockAll({ groups: [group({ id: 1, name: "default" })], clients: [] });

  renderWithProviders(<GroupsClientsTab />);

  expect(await screen.findByText(/no clients yet/i)).toBeInTheDocument();
  expect(screen.getByText(/fall back to default/i)).toBeInTheDocument();
});

test("the client-matching popover states the order", async () => {
  const user = userEvent.setup();
  mockAll();

  renderWithProviders(<GroupsClientsTab />);
  await waitFor(() => expect(groupRows()).toHaveLength(1));

  expect(screen.queryByText(/first match wins/i)).not.toBeInTheDocument();
  await user.click(screen.getByRole("button", { name: /client matching/i }));

  expect(await screen.findByText(/first match wins/i)).toBeInTheDocument();
  const listed = screen.getAllByRole("listitem").map((li) => li.textContent);
  expect(listed).toEqual(["1.exact IP", "2.CIDR, longest prefix first", "3.default"]);
});
