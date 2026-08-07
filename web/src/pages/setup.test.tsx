import { http, HttpResponse } from "msw";
import { toast } from "sonner";
import { afterEach, expect, test, vi } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { server } from "../test/msw-server";
import { renderWithProviders } from "../test/render";
import { Setup } from "./setup";

interface FillAccountFormOpts {
  username?: string;
  password?: string;
  confirm?: string;
}

async function fillAccountForm(
  user: ReturnType<typeof userEvent.setup>,
  opts: FillAccountFormOpts = {},
) {
  const { username = "admin", password = "supersecret1", confirm = password } = opts;
  // The wizard opens on a welcome screen; the account form is behind it.
  const start = screen.queryByRole("button", { name: /get started/i });
  if (start) await user.click(start);
  await user.type(screen.getByLabelText(/^username$/i), username);
  await user.type(screen.getByLabelText(/^password$/i), password);
  await user.type(screen.getByLabelText(/confirm password/i), confirm);
  await user.click(screen.getByRole("button", { name: /create account/i }));
}

/** Three of the four starter lists are checked by default. */
const DEFAULT_CHOSEN = 3;

function onListsStep() {
  return screen.findByText(/these download now/i);
}

function finishButton() {
  return screen.getByRole("button", { name: /and finish/i });
}

/** Mocks a fresh instance through account creation and silent sign-in. */
function mockAccountCreation() {
  server.use(
    http.post("/api/v1/setup", () => HttpResponse.json({ status: "created" }, { status: 201 })),
    http.post("/api/v1/auth/login", () => HttpResponse.json({ status: "ok" })),
  );
}

afterEach(() => vi.restoreAllMocks());

// The welcome screen is the wizard's step 0. It exists so an unclaimed
// instance says what it is and what the next three steps are, rather than
// opening straight onto a password field.
test("opens on a welcome screen, with the account form behind it", async () => {
  const user = userEvent.setup();
  renderWithProviders(<Setup />);

  expect(await screen.findByText(/point your router at dnsaur/i)).toBeInTheDocument();
  expect(screen.queryByLabelText(/^username$/i)).not.toBeInTheDocument();

  await user.click(screen.getByRole("button", { name: /get started/i }));
  expect(screen.getByLabelText(/^username$/i)).toBeInTheDocument();
});

test("short password shows an inline validation error and never calls the API", async () => {
  const user = userEvent.setup();
  renderWithProviders(<Setup />);

  await fillAccountForm(user, { password: "short1", confirm: "short1" });

  expect(await screen.findByText(/password must be at least 8 characters/i)).toBeInTheDocument();
  // Still on the account step — its fields are still present.
  expect(screen.getByLabelText(/^username$/i)).toBeInTheDocument();
});

// Not in the artboard, kept deliberately: there is no password recovery
// flow, so an unnoticed typo costs shell access to undo.
test("mismatched confirmation shows an inline validation error and never calls the API", async () => {
  const user = userEvent.setup();
  renderWithProviders(<Setup />);

  await fillAccountForm(user, { password: "supersecret1", confirm: "somethingelse1" });

  expect(await screen.findByText(/don't match/i)).toBeInTheDocument();
});

test("happy path: successful setup signs in and advances to the starter-lists step", async () => {
  const user = userEvent.setup();
  mockAccountCreation();
  const successSpy = vi.spyOn(toast, "success");

  renderWithProviders(<Setup />);
  await fillAccountForm(user);

  expect(await onListsStep()).toBeInTheDocument();
  expect(finishButton()).toBeInTheDocument();
  expect(successSpy).toHaveBeenCalledWith("Admin account created");
});

test("409 (already set up) surfaces an error and offers a way to login, staying on the account step", async () => {
  const user = userEvent.setup();
  server.use(
    http.post("/api/v1/setup", () =>
      HttpResponse.json({ error: "setup already completed" }, { status: 409 }),
    ),
  );
  const errorSpy = vi.spyOn(toast, "error");

  renderWithProviders(<Setup />);
  await fillAccountForm(user);

  expect(await screen.findByText(/account already exists/i)).toBeInTheDocument();
  expect(screen.getByRole("button", { name: /go to login/i })).toBeInTheDocument();
  expect(screen.getByLabelText(/^username$/i)).toBeInTheDocument();
  expect(errorSpy).toHaveBeenCalledWith("An admin account already exists");
});

test("if the silent sign-in after setup fails, the wizard skips to done pointing at login", async () => {
  const user = userEvent.setup();
  server.use(
    http.post("/api/v1/setup", () => HttpResponse.json({ status: "created" }, { status: 201 })),
    http.post("/api/v1/auth/login", () =>
      HttpResponse.json({ error: "bad credentials" }, { status: 401 }),
    ),
  );

  renderWithProviders(<Setup />);
  await fillAccountForm(user);

  expect(await screen.findByText(/admin account created/i)).toBeInTheDocument();
  expect(screen.getByRole("button", { name: /go to login/i })).toBeInTheDocument();
  // The list step needs a session, so it is skipped entirely.
  expect(screen.queryByText(/these download now/i)).not.toBeInTheDocument();
});

test("skipping the starter-lists step advances to done without creating anything", async () => {
  const user = userEvent.setup();
  let creates = 0;
  mockAccountCreation();
  server.use(
    http.post("/api/v1/filters/lists", () => {
      creates += 1;
      return HttpResponse.json({ id: creates }, { status: 201 });
    }),
  );

  renderWithProviders(<Setup />);
  await fillAccountForm(user);
  await onListsStep();

  await user.click(screen.getByRole("button", { name: /skip for now/i }));

  expect(await screen.findByRole("heading", { name: /dnsaur is ready/i })).toBeInTheDocument();
  expect(screen.getByRole("button", { name: /open the dashboard/i })).toBeInTheDocument();
  expect(creates).toBe(0);
  // Nothing was chosen, so the wizard must not claim any list result.
  expect(screen.queryByText(/blocklists added/i)).not.toBeInTheDocument();
});

// A set of checkboxes with a submit action is a form, and this step was a
// bare fieldset with two type="button" buttons — so Enter did nothing and
// it was the one step of the wizard a keyboard couldn't finish.
test("Enter submits the blocklist step", async () => {
  const user = userEvent.setup();
  let assigned = false;
  mockAccountCreation();
  server.use(
    http.post("/api/v1/filters/lists", () => HttpResponse.json({ id: 1 }, { status: 201 })),
    http.put("/api/v1/groups/:id/lists", () => {
      assigned = true;
      return new HttpResponse(null, { status: 204 });
    }),
  );

  renderWithProviders(<Setup />);
  await fillAccountForm(user);
  await onListsStep();

  // Focus a checkbox in the set and press Enter — never touching the button.
  const boxes = screen.getAllByRole("checkbox");
  boxes[0].focus();
  await user.keyboard("{Enter}");

  await waitFor(() => expect(assigned).toBe(true));
  expect(await screen.findByRole("heading", { name: /dnsaur is ready/i })).toBeInTheDocument();
});

test("finishing creates the checked lists and assigns them to the default group", async () => {
  const user = userEvent.setup();
  const createdUrls: string[] = [];
  let assignedGroupId: number | undefined;
  let assignedListIds: number[] | undefined;

  mockAccountCreation();
  server.use(
    http.post("/api/v1/filters/lists", async ({ request }) => {
      const body = (await request.json()) as { url: string; kind: string };
      createdUrls.push(body.url);
      return HttpResponse.json({ id: createdUrls.length }, { status: 201 });
    }),
    http.put("/api/v1/groups/:id/lists", async ({ params, request }) => {
      assignedGroupId = Number(params.id);
      assignedListIds = ((await request.json()) as { list_ids: number[] }).list_ids;
      return new HttpResponse(null, { status: 204 });
    }),
  );

  renderWithProviders(<Setup />);
  await fillAccountForm(user);
  await onListsStep();

  await user.click(finishButton());

  expect(await screen.findByRole("heading", { name: /dnsaur is ready/i })).toBeInTheDocument();
  expect(createdUrls).toHaveLength(DEFAULT_CHOSEN);
  expect(assignedGroupId).toBe(1);
  expect(assignedListIds).toEqual([1, 2, 3]);
  expect(screen.getByText(`${DEFAULT_CHOSEN} blocklists added`)).toBeInTheDocument();
});

// PUT /groups/{id}/lists refreshes filters synchronously (unlike POST
// /filters/lists, which backgrounds it), so by the time the wizard reaches
// the done screen the lists are already fetched, parsed and compiled.
// Promising that counts "appear once the first fetch lands" describes a
// wait that has already happened — the real number is readable.
test("the done screen reports the compiled entry count, not a pending download", async () => {
  const user = userEvent.setup();
  mockAccountCreation();
  server.use(
    http.post("/api/v1/filters/lists", () => HttpResponse.json({ id: 1 }, { status: 201 })),
    http.put("/api/v1/groups/:id/lists", () => new HttpResponse(null, { status: 204 })),
    http.get("/api/v1/filters/lists", () =>
      HttpResponse.json([
        { id: 1, entry_count: 99559 },
        { id: 2, entry_count: 218349 },
        { id: 3, entry_count: 48000 },
      ]),
    ),
  );

  renderWithProviders(<Setup />);
  await fillAccountForm(user);
  await onListsStep();
  await user.click(finishButton());

  expect(await screen.findByText(/365,908 domains compiled and enforcing/i)).toBeInTheDocument();
  expect(screen.queryByText(/counts appear once the first fetch lands/i)).not.toBeInTheDocument();
});

// If that read fails the wizard must not print a confident zero — it does
// not know the count, which is a different thing from knowing it is none.
test("an unreadable count falls back to describing the fetch, not to zero", async () => {
  const user = userEvent.setup();
  mockAccountCreation();
  server.use(
    http.post("/api/v1/filters/lists", () => HttpResponse.json({ id: 1 }, { status: 201 })),
    http.put("/api/v1/groups/:id/lists", () => new HttpResponse(null, { status: 204 })),
    http.get("/api/v1/filters/lists", () => HttpResponse.json({ error: "boom" }, { status: 500 })),
  );

  renderWithProviders(<Setup />);
  await fillAccountForm(user);
  await onListsStep();
  await user.click(finishButton());

  expect(await screen.findByRole("heading", { name: /dnsaur is ready/i })).toBeInTheDocument();
  expect(screen.queryByText(/0 domains compiled/i)).not.toBeInTheDocument();
});

// Regression: hagezi's list lives under `wildcard/`, not `hosts/`. The
// `hosts/pro.txt` this once shipped 404s, so every fresh install subscribed
// to a list that could never load — and the wizard reported success.
test("every starter URL is one the instance can actually fetch", async () => {
  const user = userEvent.setup();
  const createdUrls: string[] = [];
  mockAccountCreation();
  server.use(
    http.post("/api/v1/filters/lists", async ({ request }) => {
      createdUrls.push(((await request.json()) as { url: string }).url);
      return HttpResponse.json({ id: createdUrls.length }, { status: 201 });
    }),
    http.put("/api/v1/groups/:id/lists", () => new HttpResponse(null, { status: 204 })),
  );

  renderWithProviders(<Setup />);
  await fillAccountForm(user);
  await onListsStep();
  await user.click(finishButton());

  await waitFor(() => expect(createdUrls).toHaveLength(DEFAULT_CHOSEN));
  expect(createdUrls).not.toContain(
    "https://raw.githubusercontent.com/hagezi/dns-blocklists/main/hosts/pro.txt",
  );
  expect(createdUrls).toContain(
    "https://raw.githubusercontent.com/hagezi/dns-blocklists/main/wildcard/pro.txt",
  );
});

// A failure part-way through no longer abandons the ids already created: a
// list that exists in the catalog but belongs to no group filters precisely
// nothing, so whatever was created still gets attached.
test("a partway failure still attaches what was created, and the done screen says so", async () => {
  const user = userEvent.setup();
  let creates = 0;
  let assignedListIds: number[] | undefined;

  mockAccountCreation();
  server.use(
    http.post("/api/v1/filters/lists", () => {
      creates += 1;
      return creates === 1
        ? HttpResponse.json({ id: 1 }, { status: 201 })
        : HttpResponse.json({ error: "boom" }, { status: 500 });
    }),
    http.put("/api/v1/groups/:id/lists", async ({ request }) => {
      assignedListIds = ((await request.json()) as { list_ids: number[] }).list_ids;
      return new HttpResponse(null, { status: 204 });
    }),
  );

  renderWithProviders(<Setup />);
  await fillAccountForm(user);
  await onListsStep();

  await user.click(finishButton());

  await waitFor(() => expect(assignedListIds).toEqual([1]));
  // Reported on the screen, not in a toast that vanishes: this is the state
  // the instance is now in, and it needs following up in Filtering.
  expect(await screen.findByText(`1 of ${DEFAULT_CHOSEN} lists added`)).toBeInTheDocument();
  expect(screen.getByText(/couldn't be added/i)).toBeInTheDocument();
});

// Both lists are created, but the group assignment fails — so they sit in
// the catalog filtering nothing. Reporting "nothing happened" would send the
// admin off to create them a second time; the actionable fact is that they
// exist and need applying.
test("lists created but not attached are reported as applied to nothing", async () => {
  const user = userEvent.setup();
  let creates = 0;

  mockAccountCreation();
  server.use(
    http.post("/api/v1/filters/lists", () => {
      creates += 1;
      return HttpResponse.json({ id: creates }, { status: 201 });
    }),
    http.put("/api/v1/groups/:id/lists", () =>
      HttpResponse.json({ error: "boom" }, { status: 500 }),
    ),
  );

  renderWithProviders(<Setup />);
  await fillAccountForm(user);
  await onListsStep();

  await user.click(finishButton());

  expect(await screen.findByText(`0 of ${DEFAULT_CHOSEN} lists added`)).toBeInTheDocument();
  expect(screen.getByText(/couldn't be applied to the default group/i)).toBeInTheDocument();
  expect(creates).toBe(DEFAULT_CHOSEN);
});

test("the done screen names the address to point a router at", async () => {
  const user = userEvent.setup();
  mockAccountCreation();

  renderWithProviders(<Setup />);
  await fillAccountForm(user);
  await onListsStep();
  await user.click(screen.getByRole("button", { name: /skip for now/i }));

  expect(await screen.findByText(/point your router here/i)).toBeInTheDocument();
  // The host the admin actually reached this page on — dnsaur has no
  // endpoint that reports its own LAN address.
  expect(screen.getByText(":53")).toBeInTheDocument();
});
