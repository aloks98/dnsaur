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
  await user.type(screen.getByLabelText(/^username$/i), username);
  await user.type(screen.getByLabelText(/^password$/i), password);
  await user.type(screen.getByLabelText(/confirm password/i), confirm);
  await user.click(screen.getByRole("button", { name: /create account/i }));
}

afterEach(() => vi.restoreAllMocks());

test("short password shows an inline validation error and never calls the API", async () => {
  const user = userEvent.setup();
  renderWithProviders(<Setup />);

  await fillAccountForm(user, { password: "short1", confirm: "short1" });

  expect(await screen.findByText(/password must be at least 8 characters/i)).toBeInTheDocument();
  // Still on step 1 — the account form fields are still present.
  expect(screen.getByLabelText(/^username$/i)).toBeInTheDocument();
});

test("mismatched confirmation shows an inline validation error and never calls the API", async () => {
  const user = userEvent.setup();
  renderWithProviders(<Setup />);

  await fillAccountForm(user, { password: "supersecret1", confirm: "somethingelse1" });

  expect(await screen.findByText(/don't match/i)).toBeInTheDocument();
});

test("happy path: successful setup signs in and advances to the starter-lists step", async () => {
  const user = userEvent.setup();
  server.use(
    http.post("/api/v1/setup", () => HttpResponse.json({ status: "created" }, { status: 201 })),
    http.post("/api/v1/auth/login", () => HttpResponse.json({ status: "ok" })),
  );
  const successSpy = vi.spyOn(toast, "success");

  renderWithProviders(<Setup />);
  await fillAccountForm(user);

  expect(await screen.findByText(/starter blocklists/i)).toBeInTheDocument();
  expect(screen.getByRole("button", { name: /continue/i })).toBeInTheDocument();
  expect(successSpy).toHaveBeenCalledWith("Admin account created");
});

test("409 (already set up) surfaces an error and offers a way to login, staying on step 1", async () => {
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
  // Still on step 1 — never advanced.
  expect(screen.getByLabelText(/^username$/i)).toBeInTheDocument();
  expect(errorSpy).toHaveBeenCalledWith("An admin account already exists");
});

test("if the silent sign-in after setup fails, the wizard skips to step 3 pointing at login", async () => {
  const user = userEvent.setup();
  server.use(
    http.post("/api/v1/setup", () => HttpResponse.json({ status: "created" }, { status: 201 })),
    http.post("/api/v1/auth/login", () =>
      HttpResponse.json({ error: "bad credentials" }, { status: 401 }),
    ),
  );

  renderWithProviders(<Setup />);
  await fillAccountForm(user);

  expect(await screen.findByText(/account created/i)).toBeInTheDocument();
  expect(screen.getByRole("button", { name: /go to login/i })).toBeInTheDocument();
  expect(screen.queryByText(/starter blocklists/i)).not.toBeInTheDocument();
});

test("skipping the starter-lists step advances to done without any starter-setup calls", async () => {
  const user = userEvent.setup();
  server.use(
    http.post("/api/v1/setup", () => HttpResponse.json({ status: "created" }, { status: 201 })),
    http.post("/api/v1/auth/login", () => HttpResponse.json({ status: "ok" })),
  );

  renderWithProviders(<Setup />);
  await fillAccountForm(user);
  await screen.findByText(/starter blocklists/i);

  await user.click(screen.getByRole("button", { name: /do this later/i }));

  expect(await screen.findByText(/you're all set/i)).toBeInTheDocument();
  expect(screen.getByRole("button", { name: /go to dashboard/i })).toBeInTheDocument();
});

test("completing the starter-lists step creates the checked lists, assigns them to group 1, and saves upstreams", async () => {
  const user = userEvent.setup();
  const createdUrls: string[] = [];
  let assignedGroupId: number | undefined;
  let assignedListIds: number[] | undefined;
  let savedUpstreams: string | undefined;

  server.use(
    http.post("/api/v1/setup", () => HttpResponse.json({ status: "created" }, { status: 201 })),
    http.post("/api/v1/auth/login", () => HttpResponse.json({ status: "ok" })),
    http.post("/api/v1/filters/lists", async ({ request }) => {
      const body = (await request.json()) as { url: string; kind: string };
      createdUrls.push(body.url);
      return HttpResponse.json({ id: createdUrls.length }, { status: 201 });
    }),
    http.put("/api/v1/groups/:id/lists", async ({ params, request }) => {
      assignedGroupId = Number(params.id);
      const body = (await request.json()) as { list_ids: number[] };
      assignedListIds = body.list_ids;
      return new HttpResponse(null, { status: 204 });
    }),
    http.put("/api/v1/settings", async ({ request }) => {
      const body = (await request.json()) as { key: string; value: string };
      if (body.key === "upstreams") savedUpstreams = body.value;
      return new HttpResponse(null, { status: 204 });
    }),
  );

  renderWithProviders(<Setup />);
  await fillAccountForm(user);
  await screen.findByText(/starter blocklists/i);

  await user.click(screen.getByRole("button", { name: /continue/i }));

  expect(await screen.findByText(/you're all set/i)).toBeInTheDocument();
  expect(createdUrls).toHaveLength(2);
  expect(assignedGroupId).toBe(1);
  expect(assignedListIds).toEqual([1, 2]);
  expect(savedUpstreams).toBe("1.1.1.1:53,1.0.0.1:53,9.9.9.9:53");
});

test("a failure while saving starter setup still lets the wizard finish, with an error toast", async () => {
  const user = userEvent.setup();
  server.use(
    http.post("/api/v1/setup", () => HttpResponse.json({ status: "created" }, { status: 201 })),
    http.post("/api/v1/auth/login", () => HttpResponse.json({ status: "ok" })),
    http.post("/api/v1/filters/lists", () => HttpResponse.json({ error: "boom" }, { status: 500 })),
    http.put("/api/v1/settings", () => new HttpResponse(null, { status: 204 })),
  );
  const errorSpy = vi.spyOn(toast, "error");

  renderWithProviders(<Setup />);
  await fillAccountForm(user);
  await screen.findByText(/starter blocklists/i);

  await user.click(screen.getByRole("button", { name: /continue/i }));

  await waitFor(() =>
    expect(errorSpy).toHaveBeenCalledWith(
      "Couldn't save starter setup — you can add lists later in Filtering",
    ),
  );
  expect(screen.getByText(/you're all set/i)).toBeInTheDocument();
});

test("a partway failure still attaches the lists that were created, and says so", async () => {
  const user = userEvent.setup();
  let creates = 0;
  let assignedGroupId: number | undefined;
  let assignedListIds: number[] | undefined;

  server.use(
    http.post("/api/v1/setup", () => HttpResponse.json({ status: "created" }, { status: 201 })),
    http.post("/api/v1/auth/login", () => HttpResponse.json({ status: "ok" })),
    // The first starter list is created; the second one fails. The first
    // must still be attached to the group — a list that exists in the
    // catalog but belongs to no group filters precisely nothing.
    http.post("/api/v1/filters/lists", () => {
      creates += 1;
      return creates === 1
        ? HttpResponse.json({ id: 1 }, { status: 201 })
        : HttpResponse.json({ error: "boom" }, { status: 500 });
    }),
    http.put("/api/v1/groups/:id/lists", async ({ params, request }) => {
      assignedGroupId = Number(params.id);
      assignedListIds = ((await request.json()) as { list_ids: number[] }).list_ids;
      return new HttpResponse(null, { status: 204 });
    }),
    http.put("/api/v1/settings", () => new HttpResponse(null, { status: 204 })),
  );
  const errorSpy = vi.spyOn(toast, "error");

  renderWithProviders(<Setup />);
  await fillAccountForm(user);
  await screen.findByText(/starter blocklists/i);

  await user.click(screen.getByRole("button", { name: /continue/i }));

  await waitFor(() => expect(assignedListIds).toEqual([1]));
  expect(assignedGroupId).toBe(1);
  expect(errorSpy).toHaveBeenCalledWith(
    "Only 1 of 2 starter blocklists were saved — you can add the rest later in Filtering",
  );
  expect(screen.getByText(/you're all set/i)).toBeInTheDocument();
});

// Both lists are created, but the group assignment fails — so they sit in the
// catalog filtering nothing. "Couldn't save starter setup — you can add lists
// later" reads as "nothing happened" and sends the admin off to create them a
// second time; the actionable fact is that they exist and need applying.
test("lists created but not attached say so, instead of reporting nothing happened", async () => {
  const user = userEvent.setup();
  let creates = 0;

  server.use(
    http.post("/api/v1/setup", () => HttpResponse.json({ status: "created" }, { status: 201 })),
    http.post("/api/v1/auth/login", () => HttpResponse.json({ status: "ok" })),
    http.post("/api/v1/filters/lists", () => {
      creates += 1;
      return HttpResponse.json({ id: creates }, { status: 201 });
    }),
    http.put("/api/v1/groups/:id/lists", () =>
      HttpResponse.json({ error: "boom" }, { status: 500 }),
    ),
    http.put("/api/v1/settings", () => new HttpResponse(null, { status: 204 })),
  );
  const errorSpy = vi.spyOn(toast, "error");

  renderWithProviders(<Setup />);
  await fillAccountForm(user);
  await screen.findByText(/starter blocklists/i);

  await user.click(screen.getByRole("button", { name: /continue/i }));

  await waitFor(() =>
    expect(errorSpy).toHaveBeenCalledWith(
      "2 blocklists were created but couldn't be applied to the default group — apply them in Filtering",
    ),
  );
  expect(errorSpy).not.toHaveBeenCalledWith(
    "Couldn't save starter setup — you can add lists later in Filtering",
  );
  expect(creates).toBe(2);
  expect(screen.getByText(/you're all set/i)).toBeInTheDocument();
});
