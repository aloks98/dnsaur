import { http, HttpResponse } from "msw";
import { toast } from "sonner";
import { afterEach, expect, test, vi } from "vitest";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { server } from "../../test/msw-server";
import { dhcpHandlers, dhcpLease, replicaHandlers } from "../../test/msw-handlers";
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

// The default group governs every device not pinned somewhere else
// (internal/clients/registry.go's Lookup), so counting only the clients
// pinned *to* it reported "its 0 clients" for the group that was, at that
// moment, filtering nothing for the whole network.
test("a disabled default group counts unpinned devices, not just its own clients", async () => {
  mockAll({ groups: [group({ id: 1, name: "default", enabled: false })] });

  renderWithProviders(<GroupsClientsTab />);
  await waitFor(() => expect(groupRows()).toHaveLength(1));

  expect(screen.getByText(/not filtering/i)).toBeInTheDocument();
  expect(screen.getByText(/not pinned to another group/i)).toBeInTheDocument();
  expect(screen.queryByText(/its 0 clients/i)).not.toBeInTheDocument();
});

// The two grids are ~680px of fixed-pixel columns inside a shell that is
// h-screen/overflow-hidden (components/app-shell.tsx), so on a phone the
// Blocking and Actions columns were clipped and unreachable — no scrollbar,
// no way to pan.
test("the fixed-width grids sit in a horizontal scroll container", async () => {
  mockAll({ groups: [group({ id: 1, name: "default" })] });

  renderWithProviders(<GroupsClientsTab />);
  await waitFor(() => expect(groupRows()).toHaveLength(1));

  const scroller = document.querySelector('[data-slot="h-scroll"]');
  expect(scroller).not.toBeNull();
  expect(scroller!.className).toContain("overflow-x-auto");
  expect(scroller!.contains(groupRows()[0]!)).toBe(true);
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

// The dialog is mounted unconditionally, so `useForm` captured its defaults
// once — at mount, with no target. The field therefore opened empty on the
// first rename and, from then on, held whatever had last been typed: rename
// "Kids" to "Children" and "Office" opened on "Children".
test("rename opens on the target's own name, and never on the previous target's", async () => {
  const user = userEvent.setup();
  mockAll({ groups: [group({ id: 2, name: "Kids" }), group({ id: 3, name: "Office" })] });
  server.use(http.patch("/api/v1/groups/:id", () => new HttpResponse(null, { status: 204 })));

  renderWithProviders(<GroupsClientsTab />);
  await waitFor(() => expect(groupRows()).toHaveLength(2));

  await user.click(screen.getByRole("button", { name: /rename kids/i }));
  let dialog = await screen.findByRole("dialog");
  expect(within(dialog).getByLabelText(/^name$/i)).toHaveValue("Kids");

  await user.clear(within(dialog).getByLabelText(/^name$/i));
  await user.type(within(dialog).getByLabelText(/^name$/i), "Children");
  await user.click(within(dialog).getByRole("button", { name: /^save$/i }));
  await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());

  await user.click(screen.getByRole("button", { name: /rename office/i }));
  dialog = await screen.findByRole("dialog");
  expect(within(dialog).getByLabelText(/^name$/i)).toHaveValue("Office");
});

// The server refuses `DELETE /groups/{id}` with 409 while any client points
// at the group (internal/store/crud.go's DeleteGroup), so a dialog promising
// the clients would fall back to the default group was describing something
// that cannot happen.
test("a group with clients can't be deleted, and says to move them first", async () => {
  mockAll({
    groups: [group({ id: 2, name: "Kids" })],
    clients: [client({ id: 1, group_id: 2 }), client({ id: 2, matcher: "10.0.0.2", group_id: 2 })],
  });

  renderWithProviders(<GroupsClientsTab />);
  await waitFor(() => expect(groupRows()).toHaveLength(1));

  expect(screen.getByRole("button", { name: /delete kids/i })).toBeDisabled();
  expect(screen.getByText(/move its 2 clients first/i)).toBeInTheDocument();
});

test("a group with no clients still deletes, and the dialog says what goes with it", async () => {
  const user = userEvent.setup();
  mockAll({ groups: [group({ id: 2, name: "Kids" })] });
  server.use(http.delete("/api/v1/groups/2", () => new HttpResponse(null, { status: 204 })));

  renderWithProviders(<GroupsClientsTab />);
  await waitFor(() => expect(groupRows()).toHaveLength(1));

  await user.click(screen.getByRole("button", { name: /delete kids/i }));
  const confirm = await screen.findByRole("alertdialog");
  expect(confirm).toHaveTextContent(/rules and list assignments/i);
  expect(confirm).not.toHaveTextContent(/fall back to the default group/i);
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

// The next set is computed from `groupLists.data`, which is stale between a
// successful PUT and the refetch it triggers. Ticking a second list inside
// that window would PUT the *old* set plus the new one and silently drop
// what was just written, so nothing may be clickable until the read lands.
test("no list can be toggled while the post-write refetch is still in flight", async () => {
  const bodies: { list_ids: number[] }[] = [];
  let releaseRefetch: (() => void) | undefined;
  server.use(
    http.get("/api/v1/groups", () => HttpResponse.json([group({ id: 1, name: "default" })])),
    http.get("/api/v1/clients", () => HttpResponse.json([])),
    http.get("/api/v1/filters/lists", () =>
      HttpResponse.json([
        list({ id: 1, name: "A" }),
        list({ id: 2, name: "B" }),
        list({ id: 3, name: "C" }),
      ]),
    ),
    http.get("/api/v1/groups/:id/lists", async () => {
      // Every read after the first write is held open, so the second click
      // lands inside exactly the window the bug lived in.
      if (bodies.length > 0) {
        await new Promise<void>((resolve) => {
          releaseRefetch = resolve;
        });
      }
      return HttpResponse.json([list({ id: 2, name: "B" })]);
    }),
    http.put("/api/v1/groups/1/lists", async ({ request }) => {
      bodies.push((await request.json()) as { list_ids: number[] });
      return new HttpResponse(null, { status: 204 });
    }),
  );

  renderWithProviders(<GroupsClientsTab />);

  // fireEvent, not userEvent: base-ui's Menu re-closes on userEvent's full
  // pointerdown→click sequence under jsdom (no geometry to hit-test).
  fireEvent.click(await screen.findByRole("button", { name: /lists \(1\)/i }));
  fireEvent.click(await screen.findByRole("menuitemcheckbox", { name: "A" }));
  await waitFor(() => expect(bodies).toEqual([{ list_ids: [2, 1] }]));
  // The write has fully settled — so nothing below is merely the write's own
  // pending state holding the control shut — and the refetch it triggered is
  // held open by the handler above.
  // Held shut for the whole read, not merely for the write: the cached set
  // still says [B], so a click here would send [B, C].
  await waitFor(() =>
    expect(screen.getByRole("menuitemcheckbox", { name: "C" })).toHaveAttribute(
      "aria-disabled",
      "true",
    ),
  );
  fireEvent.click(screen.getByRole("menuitemcheckbox", { name: "C" }));
  await new Promise((resolve) => setTimeout(resolve, 30));
  // Never [B, C] — A was written a moment ago and must not vanish again.
  expect(bodies.filter((b) => !b.list_ids.includes(1))).toEqual([]);

  // And the control comes back once the read lands.
  releaseRefetch?.();
  await waitFor(() =>
    expect(screen.getByRole("menuitemcheckbox", { name: "C" })).not.toHaveAttribute(
      "aria-disabled",
      "true",
    ),
  );
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
  // The third matcher kind (spec §8.2): a hardware address, which follows
  // the device's current DHCP lease rather than naming an address at all.
  { name: "a mac matcher", value: "mac:aa:bb:cc:dd:ee:ff" },
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
  // The prefix is recognised, so the message is about the address and not
  // about a CIDR the operator never typed.
  {
    name: "a mac matcher that is not a hardware address",
    value: "mac:not-a-mac",
    error: /hardware address, e\.g\. mac:aa:bb:cc:dd:ee:ff/i,
  },
  {
    name: "an eight-byte mac matcher",
    value: "mac:aa:bb:cc:dd:ee:ff:00:11",
    error: /hardware address/i,
  },
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

/** rnui's DropdownMenu is base-ui Menu-driven; under jsdom userEvent's full
 * pointer sequence trips its outside-click detection and re-closes it. Same
 * fireEvent workaround as components/pause-control.test.tsx. */
function openPauseMenu(row: HTMLElement) {
  fireEvent.click(within(row).getByRole("button", { name: /open blocking controls/i }));
  const menu = document.querySelector('[data-slot="dropdown-menu-content"]');
  if (!menu) throw new Error("pause menu did not open");
  return menu as HTMLElement;
}

test("pausing from a client row pauses that device, not its group", async () => {
  let body: unknown;
  mockAll({ clients: [client({ id: 7, name: "Tablet", group_id: 1 })] });
  server.use(
    http.post("/api/v1/blocking/pause", async ({ request }) => {
      body = await request.json();
      return new HttpResponse(null, { status: 204 });
    }),
  );

  renderWithProviders(<GroupsClientsTab />);
  await waitFor(() => expect(clientRows()).toHaveLength(1));

  const menu = openPauseMenu(clientRows()[0]);
  fireEvent.click(within(menu).getByText(/pause 30 minutes/i));

  await waitFor(() => expect(body).toEqual({ group_id: 0, client_id: 7, minutes: 30 }));
});

test("a client row showing its group's pause says so and can't resume it", async () => {
  let deleted = false;
  mockAll({
    clients: [client({ id: 7, name: "Tablet", group_id: 2 })],
    groups: [group({ id: 2 })],
  });
  server.use(
    // The group's pause, reported on the client as the effective one.
    http.get("/api/v1/blocking", ({ request }) => {
      const q = new URL(request.url).searchParams;
      const paused = q.get("client_id") === "7" || q.get("group_id") === "2";
      return HttpResponse.json(
        paused ? { paused_until: Date.now() + 600_000, scope: "group" } : { paused_until: 0 },
      );
    }),
    http.delete("/api/v1/blocking/pause", () => {
      deleted = true;
      return new HttpResponse(null, { status: 204 });
    }),
  );

  renderWithProviders(<GroupsClientsTab />);
  await waitFor(() => expect(clientRows()).toHaveLength(1));
  await within(clientRows()[0]).findByText(/^paused · \d+:[0-5]\d$/i);

  const menu = openPauseMenu(clientRows()[0]);
  expect(within(menu).getByText(/paused for this group/i)).toBeInTheDocument();

  fireEvent.click(within(menu).getByText(/^resume blocking$/i));
  await new Promise((resolve) => setTimeout(resolve, 30));
  expect(deleted).toBe(false);
});

// --- replica mode ---------------------------------------------------------

// Spec §7: with a peer configured, every write to a synced resource is a 409
// before the store is touched. Groups and clients are synced, so the screen
// states whose configuration this is and stops offering to change it —
// rather than letting an operator fill in a form the server will refuse.
test("a replica says who manages it and won't offer to add a group", async () => {
  mockAll();
  server.use(...replicaHandlers("https://main.lan"));

  renderWithProviders(<GroupsClientsTab />);

  // One muted line in the section header, beside the control it explains —
  // not a page-wide warning strip. Being a replica is a state; the strip is
  // reserved for the two sync facts that are actually wrong.
  const notice = await screen.findByText("Managed by the main");
  const addGroup = screen.getByRole("button", { name: "Add group" });
  expect(addGroup).toBeDisabled();
  expect(notice.parentElement).toContainElement(addGroup);
  expect(notice.className).toContain("text-muted-foreground");
  expect(notice.className).not.toContain("bg-warning");

  expect(screen.getByRole("button", { name: "Add client" })).toBeDisabled();
  expect(screen.getAllByText("Managed by the main")).toHaveLength(1);
});

// --- DHCP ------------------------------------------------------------------

test("a mac matcher is posted in the one spelling the server stores", async () => {
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
  // Typed in hyphen notation and in caps, which the server accepts and then
  // stores as the colon form — so the row would come back spelled
  // differently from what was typed.
  await submitMatcher(user, "mac:AA-BB-CC-DD-EE-FF");

  await waitFor(() =>
    expect(body).toEqual({ name: "", matcher: "mac:aa:bb:cc:dd:ee:ff", group_id: 1 }),
  );
});

// Spec §8.2: the client list shows the lease hostname beside an address
// when the lease table has one. It is the name the *device* gave itself, not
// the one the operator typed — which is how you tell which box 10.0.0.31 is
// without having named it.
test("a client row carries the lease hostname beside its address", async () => {
  mockAll({
    clients: [
      client({ id: 1, name: "", matcher: "192.168.150.31" }),
      client({ id: 2, name: "", matcher: "mac:a4:83:e7:12:9f:c0" }),
      // A CIDR pins a range rather than a device, so no lease can name it.
      client({ id: 3, name: "", matcher: "192.168.150.64/27" }),
    ],
  });
  server.use(
    ...dhcpHandlers({
      leases: [
        dhcpLease({ ip: "192.168.150.31", mac: "dc:a6:32:9a:77:03", hostname: "attic-pi" }),
        dhcpLease({ ip: "192.168.150.104", mac: "a4:83:e7:12:9f:c0", hostname: "alok-mbp" }),
      ],
    }),
  );

  renderWithProviders(<GroupsClientsTab />);
  await waitFor(() => expect(clientRows()).toHaveLength(3));

  expect(await within(clientRows()[0]).findByText("· attic-pi")).toBeInTheDocument();
  expect(within(clientRows()[1]).getByText("· alok-mbp")).toBeInTheDocument();
  expect(within(clientRows()[2]).queryByText(/·/)).not.toBeInTheDocument();
});

// Most instances have no engine, and every DHCP route but the status 404s
// there — so the screen must not ask for a lease table that cannot exist,
// nor for the settings it would only have wanted the poll interval out of.
test("no lease table and no settings are fetched on a box with no engine", async () => {
  const asked: string[] = [];
  mockAll({ clients: [client({ id: 1, matcher: "192.168.150.31" })] });
  server.use(
    http.get("/api/v1/dhcp/leases", () => {
      asked.push("leases");
      return HttpResponse.json([]);
    }),
    http.get("/api/v1/settings", () => {
      asked.push("settings");
      return HttpResponse.json({});
    }),
  );

  const result = renderWithProviders(<GroupsClientsTab />);
  await waitFor(() => expect(clientRows()).toHaveLength(1));
  await waitFor(() => expect(result.queryClient.isFetching()).toBe(0));

  expect(asked).toEqual([]);
});
